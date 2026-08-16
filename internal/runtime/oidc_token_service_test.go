package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestAgentOIDCTokensMintsRequestedAudience(t *testing.T) {
	const token = "header.payload.signature"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/jobs/"+testCacheJobID+"/oidc/tokens" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Token job-secret" || request.Header.Get("Accept") != "application/json" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s headers %#v", request.Method, request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"audience":"sts.amazonaws.com"}` {
			t.Errorf("body = %s", body)
		}
		_, _ = io.WriteString(w, `{"token":"`+token+`"}`)
	}))
	defer server.Close()
	provider, err := NewAgentOIDCTokens(AgentOIDCTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider.OIDCToken(context.Background(), "sts.amazonaws.com")
	if err != nil || got != token {
		t.Fatalf("OIDCToken() = %q, %v", got, err)
	}
}

func TestAgentOIDCTokensRejectsUnsafeConfiguration(t *testing.T) {
	valid := AgentOIDCTokenConfig{Endpoint: "https://agent.example/v3", JobID: testCacheJobID, JobToken: "job-token"}
	for name, mutate := range map[string]func(*AgentOIDCTokenConfig){
		"missing endpoint":     func(c *AgentOIDCTokenConfig) { c.Endpoint = "" },
		"endpoint credentials": func(c *AgentOIDCTokenConfig) { c.Endpoint = "https://user@agent.example/v3" },
		"endpoint query":       func(c *AgentOIDCTokenConfig) { c.Endpoint += "?redirect=other" },
		"plaintext hostname":   func(c *AgentOIDCTokenConfig) { c.Endpoint = "http://localhost/v3" },
		"invalid job ID":       func(c *AgentOIDCTokenConfig) { c.JobID = "../other" },
		"missing job token":    func(c *AgentOIDCTokenConfig) { c.JobToken = "" },
		"job token split":      func(c *AgentOIDCTokenConfig) { c.JobToken = "secret\r\nOther: value" },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewAgentOIDCTokens(config); err == nil {
				t.Fatalf("NewAgentOIDCTokens(%#v) succeeded", config)
			}
		})
	}
	for _, endpoint := range []string{"https://agent.example/v3", "http://127.0.0.1:1234/v3", "http://[::1]:1234/v3"} {
		if _, err := NewAgentOIDCTokens(AgentOIDCTokenConfig{Endpoint: endpoint, JobID: testCacheJobID, JobToken: "job-token"}); err != nil {
			t.Errorf("NewAgentOIDCTokens(%q): %v", endpoint, err)
		}
	}
}

func TestAgentOIDCTokensRejectsAuthFailuresAndMalformedResponses(t *testing.T) {
	const secret = "must.not.leak"
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, secret, "denied"},
		{"forbidden", http.StatusForbidden, secret, "denied"},
		{"malformed", http.StatusOK, `{"token":`, "decode"},
		{"unknown field", http.StatusOK, `{"token":"header.payload.signature","other":true}`, "unknown field"},
		{"trailing data", http.StatusOK, `{"token":"header.payload.signature"}{}`, "trailing data"},
		{"invalid JWT", http.StatusOK, `{"token":"not-a-jwt"}`, "invalid token"},
		{"oversized", http.StatusOK, `{"token":"header.payload.signature"}` + strings.Repeat(" ", oidcTokenResponseLimit), "exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			provider, err := NewAgentOIDCTokens(AgentOIDCTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.OIDCToken(context.Background(), "audience")
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("OIDCToken() error = %v, want %q without response body", err, test.want)
			}
		})
	}
}

type testOIDCTokenProvider struct {
	token     string
	audiences []string
}

func (p *testOIDCTokenProvider) OIDCToken(_ context.Context, audience string) (string, error) {
	p.audiences = append(p.audiences, audience)
	return p.token, nil
}

func TestIDTokenServiceActionsCoreContract(t *testing.T) {
	provider := &testOIDCTokenProvider{token: "header.payload.signature"}
	redactor := &testRedactor{}
	processor := newCommandProcessor(&bytes.Buffer{}, &bytes.Buffer{})
	service, err := startIDTokenService(context.Background(), provider, redactor, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	env, revoke, err := service.actionEnvironment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer revoke()
	unauthorized, err := http.Get(env["ACTIONS_ID_TOKEN_REQUEST_URL"] + "&audience=denied")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized || len(provider.audiences) != 0 {
		t.Fatalf("unauthorized request = %d, provider calls %#v", unauthorized.StatusCode, provider.audiences)
	}
	request, err := http.NewRequest(http.MethodGet, env["ACTIONS_ID_TOKEN_REQUEST_URL"]+"&audience=sts.amazonaws.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"])
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || body.Value != provider.token || len(provider.audiences) != 1 || provider.audiences[0] != "sts.amazonaws.com" {
		t.Fatalf("response/provider = %d %#v / %#v", response.StatusCode, body, provider.audiences)
	}
	if len(redactor.values) != 2 || redactor.values[0] != env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] || redactor.values[1] != provider.token {
		t.Fatalf("redactions = %#v", redactor.values)
	}
}

func TestNodeActionCallsActionsCoreGetIDTokenThroughLoopbackService(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/oidc.yml"
	actionPath := ".github/actions/oidc"
	writeFixtureFile(t, workspace, workflowPath, "name: OIDC contract\n")
	writeFixtureFile(t, workspace, actionPath+"/action.yml", "name: OIDC\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, workspace, actionPath+"/node_modules/@actions/core/index.js", `
const http = require("node:http");
exports.getIDToken = async function getIDToken(audience) {
  const endpoint = process.env.ACTIONS_ID_TOKEN_REQUEST_URL;
  const bearer = process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN;
  if (!endpoint || !bearer) throw new Error("ID token environment is unavailable");
  return await new Promise((resolve, reject) => {
    const request = http.get(endpoint + "&audience=" + encodeURIComponent(audience), {headers: {Authorization: "Bearer " + bearer}}, response => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", chunk => body += chunk);
      response.on("end", () => response.statusCode === 200 ? resolve(JSON.parse(body).value) : reject(new Error("HTTP " + response.statusCode)));
    });
    request.on("error", reject);
  });
};
`)
	writeFixtureFile(t, workspace, actionPath+"/main.js", `
const fs = require("node:fs");
const core = require("@actions/core");
(async () => fs.writeFileSync(process.env.MARKER, await core.getIDToken("sts.amazonaws.com")))().catch(error => { console.error(error); process.exitCode = 1; });
`)
	marker := filepath.Join(workspace, "token")
	lockID := "a-0123456789abcdef"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "oidc", Kind: "uses", Uses: "./" + actionPath,
		Env: map[string]string{"MARKER": marker}, Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.IDTokenPermission = "write"
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: actionPath, SourceDigest: digestTree(t, filepath.Join(workspace, actionPath))}}
	provider := &testOIDCTokenProvider{token: "header.payload.signature"}
	redactor := &testRedactor{}
	result, err := (Runner{Node24: node, OIDCToken: provider, Redactor: redactor}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() = %#v, %v", result, err)
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != provider.token || len(provider.audiences) != 1 || provider.audiences[0] != "sts.amazonaws.com" {
		t.Fatalf("token/audiences = %q / %#v", contents, provider.audiences)
	}
}

func TestNodeActionIDTokenEnvironmentRequiresWritePermission(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/no-oidc.yml"
	actionPath := ".github/actions/no-oidc"
	writeFixtureFile(t, workspace, workflowPath, "name: no OIDC\n")
	writeFixtureFile(t, workspace, actionPath+"/action.yml", "name: no OIDC\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, workspace, actionPath+"/main.js", `
for (const name of ["ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"])
  if (process.env[name]) throw new Error(name + " leaked without id-token: write");
`)
	lockID := "a-0123456789abcdef"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "no-oidc", Kind: "uses", Uses: "./" + actionPath,
		Env:    map[string]string{"ACTIONS_ID_TOKEN_REQUEST_URL": "https://attacker.invalid/?x=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "attacker"},
		Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: actionPath, SourceDigest: digestTree(t, filepath.Join(workspace, actionPath))}}
	result, err := (Runner{Node24: node}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() = %#v, %v", result, err)
	}
}
