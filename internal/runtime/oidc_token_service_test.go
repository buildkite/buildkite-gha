package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestAgentOIDCTokensMintsRequestedAudienceAndConfiguredClaims(t *testing.T) {
	const token = "header.payload.signature"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/jobs/"+testCacheJobID+"/oidc/tokens" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Token job-secret" || request.Header.Get("Accept") != "application/json" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("User-Agent") != "buildkite-gha/1.2.3" {
			t.Errorf("request = %s headers %#v", request.Method, request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"audience":"sts.amazonaws.com","claims":["organization_id"],"aws_session_tags":["organization_slug","pipeline_id"],"subject_claim":"pipeline_id"}` {
			t.Errorf("body = %s", body)
		}
		_, _ = io.WriteString(w, `{"token":"`+token+`"}`)
	}))
	defer server.Close()
	provider, err := NewAgentOIDCTokens(AgentOIDCTokenConfig{
		Endpoint:       server.URL,
		JobID:          testCacheJobID,
		JobToken:       "job-secret",
		Claims:         []string{"organization_id"},
		AWSSessionTags: []string{"organization_slug", "pipeline_id"},
		SubjectClaim:   "pipeline_id",
		ClientVersion:  "1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider.OIDCToken(t.Context(), "sts.amazonaws.com")
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
			_, err = provider.OIDCToken(t.Context(), "audience")
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("OIDCToken() error = %v, want %q without response body", err, test.want)
			}
		})
	}
}

type testOIDCTokenProvider struct {
	token              string
	requireLiveContext bool
	audiences          []string
	err                error
}

func (p *testOIDCTokenProvider) OIDCToken(ctx context.Context, audience string) (string, error) {
	if p.requireLiveContext && ctx.Err() != nil {
		return "", ctx.Err()
	}
	p.audiences = append(p.audiences, audience)
	if p.err != nil {
		return "", p.err
	}
	return p.token, nil
}

func TestIDTokenServiceWireContract(t *testing.T) {
	provider := &testOIDCTokenProvider{token: "header.payload.signature", requireLiveContext: true}
	redactor := &testRedactor{}
	stderr := &bytes.Buffer{}
	processor := newCommandProcessor(&bytes.Buffer{}, stderr)
	service, err := startIDTokenService(t.Context(), provider, redactor, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close(t.Context()) }()
	env, revoke, err := service.actionEnvironment(t.Context(), nil)
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
	warnings, _, _, _ := processor.workflowCommandAnnotations()
	if warnings != "" || stderr.Len() != 0 {
		t.Fatalf("unauthorized request emitted guidance: annotation = %q, stderr = %q", warnings, stderr)
	}
	authorizedRequest := func(audience string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, env["ACTIONS_ID_TOKEN_REQUEST_URL"]+"&audience="+audience, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"])
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := authorizedRequest("sts.amazonaws.com")
	defer func() { _ = response.Body.Close() }()
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || body.Value != provider.token {
		t.Fatalf("response/provider = %d %#v / %#v", response.StatusCode, body, provider.audiences)
	}
	second := authorizedRequest("second-audience")
	_ = second.Body.Close()
	if second.StatusCode != http.StatusOK || len(provider.audiences) != 2 || provider.audiences[0] != "sts.amazonaws.com" || provider.audiences[1] != "second-audience" {
		t.Fatalf("second response/provider = %d / %#v", second.StatusCode, provider.audiences)
	}
	if len(redactor.values) != 3 || redactor.values[0] != env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] || redactor.values[1] != provider.token || redactor.values[2] != provider.token {
		t.Fatalf("redactions = %#v", redactor.values)
	}
	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated || strings.Count(warnings, "Buildkite issued this job an OIDC token") != 1 || !strings.Contains(warnings, "https://agent.buildkite.com") || !strings.Contains(warnings, "https://buildkite.com/docs/pipelines/security/oidc") {
		t.Fatalf("OIDC guidance annotation = %q, truncated = %v", warnings, truncated)
	}
	if strings.Count(stderr.String(), oidcIssuerMigrationWarn) != 1 {
		t.Fatalf("OIDC guidance log = %q", stderr)
	}
	for _, secret := range []string{env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"], provider.token} {
		if strings.Contains(warnings, secret) || strings.Contains(stderr.String(), secret) {
			t.Fatalf("OIDC guidance leaked secret %q", secret)
		}
	}
}

func TestIDTokenServicePreservesPermanentMintFailureStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			provider := &testOIDCTokenProvider{err: oidcTokenStatusError(status)}
			processor := newCommandProcessor(&bytes.Buffer{}, &bytes.Buffer{})
			service, err := startIDTokenService(t.Context(), provider, &testRedactor{}, processor)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = service.Close(t.Context()) }()
			env, revoke, err := service.actionEnvironment(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer revoke()
			request, err := http.NewRequest(http.MethodGet, env["ACTIONS_ID_TOKEN_REQUEST_URL"], nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"])
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != status || len(provider.audiences) != 1 {
				t.Fatalf("request = HTTP %d with %d mint calls, want HTTP %d with one call", response.StatusCode, len(provider.audiences), status)
			}
			warnings, _, _, _ := processor.workflowCommandAnnotations()
			if warnings != "" {
				t.Fatalf("failed mint emitted issued-token guidance: %q", warnings)
			}
		})
	}
}

func writeOIDCUtilsContractShim(t *testing.T, workspace, actionPath string) {
	t.Helper()
	writeFixtureFile(t, workspace, actionPath+"/node_modules/@actions/core/index.js", `
const http = require("node:http");

function requiredEnvironment(name) {
  const value = process.env[name];
  if (!value) throw new Error("Unable to get " + name + " env variable");
  return value;
}

// Contract-conformant test shim mirroring actions/toolkit's oidc-utils.ts.
exports.getIDToken = async function getIDToken(audience) {
  let endpoint = requiredEnvironment("ACTIONS_ID_TOKEN_REQUEST_URL");
  if (audience) endpoint += "&audience=" + encodeURIComponent(audience);
  const bearer = requiredEnvironment("ACTIONS_ID_TOKEN_REQUEST_TOKEN");
  return await new Promise((resolve, reject) => {
    const request = http.get(endpoint, {headers: {Accept: "application/json", Authorization: "Bearer " + bearer}}, response => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", chunk => body += chunk);
      response.on("end", () => {
        if (response.statusCode < 200 || response.statusCode > 299) return reject(new Error("HTTP " + response.statusCode));
        let parsed;
        try {
          parsed = JSON.parse(body);
        } catch (error) {
          return reject(error);
        }
        if (!parsed || typeof parsed.value !== "string" || parsed.value.length === 0) {
          return reject(new Error("Response json body do not have ID Token field"));
        }
        resolve(parsed.value);
      });
    });
    request.on("error", reject);
  });
};
`)
}

func TestNodeActionUsesOIDCUtilsContractShimThroughLoopbackService(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/oidc.yml"
	actionPath := ".github/actions/oidc"
	writeFixtureFile(t, workspace, workflowPath, "name: OIDC contract\n")
	writeFixtureFile(t, workspace, actionPath+"/action.yml", "name: OIDC\nruns:\n  using: node24\n  main: main.js\n")
	writeOIDCUtilsContractShim(t, workspace, actionPath)
	writeFixtureFile(t, workspace, actionPath+"/main.js", `
const fs = require("node:fs");
const core = require("@actions/core");
const endpoint = new URL(process.env.ACTIONS_ID_TOKEN_REQUEST_URL);
if (!endpoint.search) throw new Error("ACTIONS_ID_TOKEN_REQUEST_URL must already contain a query string");
if (process.env.NO_PROXY !== "upper.example,127.0.0.1") throw new Error("NO_PROXY does not preserve the proxy bypass list");
if (process.env.no_proxy !== "lower.example,127.0.0.1") throw new Error("no_proxy does not preserve the proxy bypass list");
(async () => {
  const first = await core.getIDToken("sts.amazonaws.com");
  const second = await core.getIDToken("second-audience");
  fs.writeFileSync(process.env.MARKER, first + "\n" + second);
})().catch(error => { console.error(error); process.exitCode = 1; });
`)
	marker := filepath.Join(workspace, "token")
	lockID := "a-0123456789abcdef"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "oidc", Kind: "uses", Uses: "./" + actionPath,
		Env: map[string]string{"MARKER": marker, "NO_PROXY": "upper.example", "no_proxy": "lower.example"}, Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.IDTokenPermission = "write"
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: actionPath, SourceDigest: digestTree(t, filepath.Join(workspace, actionPath))}}
	provider := &testOIDCTokenProvider{token: "header.payload.signature"}
	redactor := &testRedactor{}
	result, err := (Runner{Node24: node, OIDCToken: provider, Redactor: redactor}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() = %#v, %v", result, err)
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != provider.token+"\n"+provider.token || len(provider.audiences) != 2 || provider.audiences[0] != "sts.amazonaws.com" || provider.audiences[1] != "second-audience" {
		t.Fatalf("token/audiences = %q / %#v", contents, provider.audiences)
	}
	if strings.Count(result.WarningAnnotations, "Buildkite issued this job an OIDC token") != 1 || !strings.Contains(result.WarningAnnotations, "https://agent.buildkite.com") || !strings.Contains(result.WarningAnnotations, "https://buildkite.com/docs/pipelines/security/oidc") {
		t.Fatalf("RunJob() OIDC guidance annotation = %q", result.WarningAnnotations)
	}
	if strings.Contains(result.WarningAnnotations, provider.token) {
		t.Fatalf("RunJob() OIDC guidance annotation leaked token: %q", result.WarningAnnotations)
	}
}

func TestNodePostActionUsesIDTokenServiceAfterJobCancellation(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/oidc-cancel.yml"
	actionPath := ".github/actions/oidc-cancel"
	writeFixtureFile(t, workspace, workflowPath, "name: OIDC cancellation\n")
	writeFixtureFile(t, workspace, actionPath+"/action.yml", "name: OIDC cancellation\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeOIDCUtilsContractShim(t, workspace, actionPath)
	writeFixtureFile(t, workspace, actionPath+"/main.js", "setTimeout(() => {}, 30000)\n")
	writeFixtureFile(t, workspace, actionPath+"/post.js", `
const fs = require("node:fs");
const core = require("@actions/core");
(async () => fs.writeFileSync(process.env.MARKER, await core.getIDToken("post-cleanup")))().catch(error => { console.error(error); process.exitCode = 1; });
`)
	marker := filepath.Join(workspace, "post-token")
	lockID := "a-0123456789abcdef"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "oidc-cancel", Kind: "uses", Uses: "./" + actionPath,
		Env: map[string]string{"MARKER": marker}, Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.IDTokenPermission = "write"
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: actionPath, SourceDigest: digestTree(t, filepath.Join(workspace, actionPath))}}
	provider := &testOIDCTokenProvider{token: "header.payload.signature", requireLiveContext: true}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := (Runner{Node24: node, OIDCToken: provider, Redactor: &testRedactor{}}).runTestJob(ctx, job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() = %#v, %v", result, err)
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("post action did not mint an ID token after cancellation: %v", err)
	}
	if string(contents) != provider.token || len(provider.audiences) != 1 || provider.audiences[0] != "post-cleanup" {
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
	writeOIDCUtilsContractShim(t, workspace, actionPath)
	writeFixtureFile(t, workspace, actionPath+"/main.js", `
const fs = require("node:fs");
const core = require("@actions/core");
(async () => {
  try {
    await core.getIDToken("sts.amazonaws.com");
    throw new Error("getIDToken unexpectedly succeeded");
  } catch (error) {
    if (error.message !== "Unable to get ACTIONS_ID_TOKEN_REQUEST_URL env variable") throw error;
    fs.writeFileSync(process.env.MARKER, error.message);
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
`)
	marker := filepath.Join(workspace, "missing-environment")
	lockID := "a-0123456789abcdef"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "no-oidc", Kind: "uses", Uses: "./" + actionPath,
		Env:    map[string]string{"ACTIONS_ID_TOKEN_REQUEST_URL": "https://attacker.invalid/?x=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "attacker", "MARKER": marker},
		Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: actionPath, SourceDigest: digestTree(t, filepath.Join(workspace, actionPath))}}
	result, err := (Runner{Node24: node}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() = %#v, %v", result, err)
	}
	message, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != "Unable to get ACTIONS_ID_TOKEN_REQUEST_URL env variable" {
		t.Fatalf("getIDToken error = %q", message)
	}
}
