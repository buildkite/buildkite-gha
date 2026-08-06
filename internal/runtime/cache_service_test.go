package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

const testCacheJobID = "11111111-1111-4111-8111-111111111111"

type cacheCredentialProviderFunc func(context.Context) (CacheCredentials, error)

func (f cacheCredentialProviderFunc) Credentials(ctx context.Context) (CacheCredentials, error) {
	return f(ctx)
}

func TestAgentCacheCredentialsMintsBoundedJobCredential(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v3/jobs/"+testCacheJobID+"/ghac_tokens" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Accept") != "application/json" || len(body) != 0 {
			t.Errorf("request headers/body = %#v / %q", r.Header, body)
		}
		_, _ = io.WriteString(w, `{"token":"header.payload.signature"}`)
	}))
	defer server.Close()

	provider, err := NewAgentCacheCredentials(AgentCacheConfig{
		Endpoint: server.URL + "/v3/",
		JobID:    testCacheJobID, JobToken: "job-secret",
		ResultsURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provider.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || credentials.Token != "header.payload.signature" || credentials.ResultsURL != server.URL+"/" {
		t.Fatalf("calls/credentials = %d / %#v", calls, credentials)
	}
}

func TestAgentCacheCredentialsDefaultsOfficialResultsService(t *testing.T) {
	provider, err := NewAgentCacheCredentials(AgentCacheConfig{
		Endpoint: "https://agent.example/v3", JobID: testCacheJobID, JobToken: "job-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.resultsURL != defaultCacheResultsURL {
		t.Fatalf("default Results URL = %q, want %q", provider.resultsURL, defaultCacheResultsURL)
	}
}

func TestAgentCacheCredentialsRejectsUnsafeConfiguration(t *testing.T) {
	valid := AgentCacheConfig{
		Endpoint: "https://agent.example/v3", JobID: testCacheJobID,
		JobToken: "job-token", ResultsURL: "https://cache.example",
	}
	for name, mutate := range map[string]func(*AgentCacheConfig){
		"missing endpoint":       func(c *AgentCacheConfig) { c.Endpoint = "" },
		"endpoint credentials":   func(c *AgentCacheConfig) { c.Endpoint = "https://user@agent.example/v3" },
		"endpoint query":         func(c *AgentCacheConfig) { c.Endpoint += "?redirect=other" },
		"invalid job ID":         func(c *AgentCacheConfig) { c.JobID = "../other" },
		"missing job token":      func(c *AgentCacheConfig) { c.JobToken = "" },
		"job token header split": func(c *AgentCacheConfig) { c.JobToken = "secret\r\nOther: value" },
		"results credentials":    func(c *AgentCacheConfig) { c.ResultsURL = "https://user@cache.example/v2" },
		"results path":           func(c *AgentCacheConfig) { c.ResultsURL += "/v2" },
		"results query":          func(c *AgentCacheConfig) { c.ResultsURL += "?token=value" },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewAgentCacheCredentials(config); err == nil {
				t.Fatalf("NewAgentCacheCredentials(%#v) succeeded", config)
			}
		})
	}
}

func TestAgentCacheCredentialsRejectsRedirectsAndUntrustedResponses(t *testing.T) {
	secret := "must.not.leak"
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"denied", http.StatusForbidden, secret, "denied"},
		{"disabled", http.StatusNotFound, secret, "not enabled"},
		{"invalid provenance", http.StatusUnprocessableEntity, secret, "provenance"},
		{"rate limited", http.StatusTooManyRequests, secret, "rate limited"},
		{"unexpected status", http.StatusBadGateway, secret, "HTTP 502"},
		{"malformed JSON", http.StatusOK, `{"token":`, "decode"},
		{"unknown field", http.StatusOK, `{"token":"a.b.c","other":true}`, "unknown field"},
		{"trailing JSON", http.StatusOK, `{"token":"a.b.c"}{}`, "trailing data"},
		{"invalid token", http.StatusOK, `{"token":"not-a-jwt"}`, "invalid token"},
		{"oversized", http.StatusOK, `{"token":"a.b.c"}` + strings.Repeat(" ", cacheCredentialResponseLimit), "exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			provider, err := NewAgentCacheCredentials(AgentCacheConfig{
				Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token", ResultsURL: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Credentials(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("Credentials() error = %v, want %q without response body", err, test.want)
			}
		})
	}

	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			_, _ = io.WriteString(w, `{"token":"a.b.c"}`)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	provider, err := NewAgentCacheCredentials(AgentCacheConfig{
		Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token", ResultsURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Credentials(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 307") || redirected {
		t.Fatalf("redirect Credentials() error/redirected = %v / %v", err, redirected)
	}
}

func TestAgentCacheCredentialsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider, err := NewAgentCacheCredentials(AgentCacheConfig{
		Endpoint: "https://agent.invalid/v3", JobID: testCacheJobID,
		JobToken: "job-token", ResultsURL: "https://cache.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Credentials(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Credentials() error = %v, want cancellation", err)
	}
}

type sequenceCacheCredentials struct {
	mu     sync.Mutex
	tokens []string
	calls  int
}

func (p *sequenceCacheCredentials) Credentials(context.Context) (CacheCredentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls >= len(p.tokens) {
		return CacheCredentials{}, fmt.Errorf("unexpected cache credential request")
	}
	token := p.tokens[p.calls]
	p.calls++
	return CacheCredentials{ResultsURL: "https://cache.example", Token: token}, nil
}

func TestCacheV6LifecycleUsesFreshIsolatedCredentials(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/cache.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: cache lifecycle\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "package.json", `{"type":"module"}`)
	writeFixtureFile(t, remote, "action.yml", "name: cache v6\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		program := fmt.Sprintf(`import fs from "node:fs";
import {spawnSync} from "node:child_process";
const required = ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"];
for (const name of required) if (!process.env[name]) throw new Error("missing " + name);
for (const name of [
  "ACTIONS_CACHE_URL", "ACTIONS_RUNTIME_URL",
  "NODE_OPTIONS", "NODE_PATH", "NODE_EXTRA_CA_CERTS", "NODE_TLS_REJECT_UNAUTHORIZED", "SSLKEYLOGFILE", "LD_AUDIT", "LD_PRELOAD", "LD_LIBRARY_PATH",
  "OPENSSL_CONF", "OPENSSL_CONF_INCLUDE", "OPENSSL_ENGINES", "OPENSSL_MODULES",
  "TAR_OPTIONS",
  "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy",
  "BUILDKITE_AGENT_ACCESS_TOKEN", "BUILDKITE_JOB_ID",
]) if (process.env[name]) throw new Error(name + " leaked");
const tar = spawnSync("tar", ["--version"], {encoding: "utf8"});
if (tar.status !== 0) throw new Error("trusted tar failed: " + tar.stderr);
if (process.env.PATH !== %q) throw new Error("unsafe PATH: " + process.env.PATH);
fs.appendFileSync(process.env.LIFECYCLE_LOG, "%s|" + process.env.ACTIONS_RUNTIME_TOKEN + "|" + process.env.ACTIONS_RESULTS_URL + "|" + process.env.ACTIONS_CACHE_SERVICE_V2 + "\n");
console.log("credential=" + process.env.ACTIONS_RUNTIME_TOKEN);
`, cacheActionToolPath, phase)
		writeFixtureFile(t, remote, phase+".js", program)
	}
	writeFixtureFile(t, remote, "ordinary/action.yml", "name: ordinary\nruns:\n  using: node24\n  main: index.js\n")
	writeFixtureFile(t, remote, "ordinary/index.js", `import fs from "node:fs";
for (const name of ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN", "BUILDKITE_AGENT_ACCESS_TOKEN"])
  if (process.env[name]) throw new Error(name + " leaked");
if (!process.env.PATH.startsWith(process.env.ATTACKER_BIN + ":")) throw new Error("ordinary action PATH was isolated");
fs.appendFileSync(process.env.LIFECYCLE_LOG, "ordinary\n");
`)
	digest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	cacheID, ordinaryID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	attackerBin := filepath.Join(workspace, "attacker-bin")
	if err := os.Mkdir(attackerBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTarMarker := filepath.Join(workspace, "fake-tar-ran")
	writeFixtureFile(t, attackerBin, "tar", "#!/bin/sh\nprintf '%s' \"$ACTIONS_RUNTIME_TOKEN\" > \"$FAKE_TAR_MARKER\"\n")
	if err := os.Chmod(filepath.Join(attackerBin, "tar"), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheEnv := map[string]string{
		"ACTIONS_RESULTS_URL": "https://attacker.invalid", "ACTIONS_RUNTIME_TOKEN": "workflow-token", "ACTIONS_CACHE_SERVICE_V2": "false",
		"ACTIONS_CACHE_URL": "https://legacy.invalid", "ACTIONS_RUNTIME_URL": "https://legacy.invalid",
		"NODE_OPTIONS": "--require attacker", "NODE_PATH": "/attacker", "NODE_EXTRA_CA_CERTS": "/attacker.pem", "NODE_TLS_REJECT_UNAUTHORIZED": "0",
		"SSLKEYLOGFILE": "/attacker/keys", "LD_AUDIT": "/attacker-audit.so", "LD_PRELOAD": "/attacker.so", "LD_LIBRARY_PATH": "/attacker/lib",
		"OPENSSL_CONF": "/attacker/openssl.cnf", "OPENSSL_CONF_INCLUDE": "/attacker/includes", "OPENSSL_ENGINES": "/attacker/engines", "OPENSSL_MODULES": "/attacker/modules",
		"TAR_OPTIONS": "--checkpoint=1 --checkpoint-action=exec=/attacker-command",
		"HTTP_PROXY":  "http://attacker", "HTTPS_PROXY": "http://attacker", "ALL_PROXY": "http://attacker", "NO_PROXY": "cache.example",
		"http_proxy": "http://attacker", "https_proxy": "http://attacker", "all_proxy": "http://attacker", "no_proxy": "cache.example",
		"BUILDKITE_AGENT_ACCESS_TOKEN": "workflow-agent-token", "BUILDKITE_JOB_ID": testCacheJobID, "FAKE_TAR_MARKER": fakeTarMarker,
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "poison-path", Kind: "run", Command: `printf '%s\n' "$ATTACKER_BIN" >> "$GITHUB_PATH"`},
		{ID: "ordinary", Kind: "uses", Uses: "owner/repo/ordinary@v1", Action: &plan.ActionSelector{Lock: ordinaryID}},
		{ID: "cache", Kind: "uses", Uses: "actions/cache@" + actionintegration.CacheCommit, Env: cacheEnv, Action: &plan.ActionSelector{Lock: cacheID}},
	})
	job.Schema = plan.SchemaV3
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle, "ATTACKER_BIN": attackerBin}
	job.Actions = []plan.ActionLock{
		{ID: cacheID, Source: "github", Repository: "actions/cache", RequestedRef: actionintegration.CacheCommit, Commit: actionintegration.CacheCommit, SourceDigest: digest},
		{ID: ordinaryID, Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: strings.Repeat("a", 40), Path: "ordinary", SourceDigest: digest},
	}
	provider := &sequenceCacheCredentials{tokens: []string{"header.pre.signature", "header.main.signature", "header.post.signature"}}
	redactor := &testRedactor{}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	var logs bytes.Buffer
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token-must-not-leak")
	result, err := (Runner{
		Node24: node, Actions: materializer, Cache: provider, Redactor: redactor,
		Stdout: &logs, Stderr: &logs,
	}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	contents, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	want := "pre|header.pre.signature|https://cache.example/|true\nordinary\nmain|header.main.signature|https://cache.example/|true\npost|header.post.signature|https://cache.example/|true\n"
	if string(contents) != want {
		t.Fatalf("lifecycle = %q, want %q", contents, want)
	}
	if provider.calls != 3 || fmt.Sprint(redactor.values) != fmt.Sprint(provider.tokens) {
		t.Fatalf("credential calls/redactions = %d / %#v", provider.calls, redactor.values)
	}
	for _, token := range provider.tokens {
		if strings.Contains(logs.String(), token) {
			t.Fatalf("logs contain cache credential %q: %q", token, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "credential=***") {
		t.Fatalf("logs do not contain masked credential: %q", logs.String())
	}
	if _, err := os.Stat(fakeTarMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workflow-controlled tar executed or marker stat failed: %v", err)
	}
	for _, name := range []string{"ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN", "BUILDKITE_AGENT_ACCESS_TOKEN"} {
		if result.Env[name] != "" {
			t.Fatalf("result environment persisted %s", name)
		}
	}

	job.Actions[0].Commit = strings.Repeat("b", 40)
	if _, err := (Runner{Node24: node, Actions: materializer, Cache: provider, Redactor: redactor}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), actionintegration.CacheCommit) {
		t.Fatalf("unsupported runtime cache commit error = %v", err)
	}
}

type failingCacheRedactor struct{ token string }

func (r failingCacheRedactor) AddRedaction(context.Context, string) error {
	return fmt.Errorf("redactor rejected %s", r.token)
}

func TestCacheV6RedactorFailureAbortsBeforeExecutionAndScrubsToken(t *testing.T) {
	token := "header.secret.signature"
	actionRoot := t.TempDir()
	marker := filepath.Join(actionRoot, "executed")
	writeFixtureFile(t, actionRoot, "main.js", `require("node:fs").writeFileSync(process.env.MARKER, "executed")`)
	provider := cacheCredentialProviderFunc(func(context.Context) (CacheCredentials, error) {
		return CacheCredentials{ResultsURL: "https://cache.example", Token: token}, nil
	})
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	result := newResult()
	result.Env["MARKER"] = marker
	err := (Runner{Cache: provider, Redactor: failingCacheRedactor{token: token}}).runJavaScriptPhase(
		context.Background(), processor, actionRoot, "node", JavaScriptAction{Name: "cache", Path: actionRoot, Main: "main.js", Cache: true}, "main.js", nil, nil, &result,
	)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("runJavaScriptPhase() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("action marker exists or stat failed unexpectedly: %v", err)
	}
}

func TestCacheV6TokenCommandFileEffectsAreDiscarded(t *testing.T) {
	node := requireNode24(t)
	token := "header.secret.signature"
	for name, command := range map[string]string{
		"output":  `fs.appendFileSync(process.env.GITHUB_OUTPUT, "leak=" + token + "\n");`,
		"env":     `fs.appendFileSync(process.env.GITHUB_ENV, "LEAK=" + token + "\n");`,
		"state":   `fs.appendFileSync(process.env.GITHUB_STATE, "leak=" + token + "\n");`,
		"path":    `fs.appendFileSync(process.env.GITHUB_PATH, token + "\n");`,
		"summary": `fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, token + "\n");`,
	} {
		t.Run(name, func(t *testing.T) {
			actionRoot := t.TempDir()
			writeFixtureFile(t, actionRoot, "main.js", `const fs = require("node:fs"); const token = process.env.ACTIONS_RUNTIME_TOKEN; `+command)
			provider := cacheCredentialProviderFunc(func(context.Context) (CacheCredentials, error) {
				return CacheCredentials{ResultsURL: "https://cache.example", Token: token}, nil
			})
			result := newResult()
			result.Outputs["kept"] = "output"
			result.Env["KEPT"] = "environment"
			result.State["kept"] = "result state"
			result.Summary = "existing summary\n"
			result.Paths = []string{"/existing/path"}
			state := map[string]string{"kept": "action state"}
			err := (Runner{Cache: provider, Redactor: &testRedactor{}}).runJavaScriptPhase(
				context.Background(), newCommandProcessor(io.Discard, io.Discard), actionRoot, node,
				JavaScriptAction{Name: "cache", Path: actionRoot, Main: "main.js", Cache: true}, "main.js", nil, state, &result,
			)
			if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "phase effects were discarded") {
				t.Fatalf("runJavaScriptPhase() error = %v", err)
			}
			if fmt.Sprint(result.Outputs) != "map[kept:output]" || fmt.Sprint(result.Env) != "map[KEPT:environment]" || fmt.Sprint(result.State) != "map[kept:result state]" {
				t.Fatalf("cache effects survived in result: %#v", result)
			}
			if result.Summary != "existing summary\n" || fmt.Sprint(result.Paths) != "[/existing/path]" || fmt.Sprint(state) != "map[kept:action state]" {
				t.Fatalf("cache summary/path/state effects survived: %#v / %#v", result, state)
			}
		})
	}
}
