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
	credentials, err := provider.Credentials(t.Context())
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

func TestAgentCacheCredentialsAcceptsHTTPSAndLoopbackHTTP(t *testing.T) {
	for _, endpoint := range []string{
		"https://service.example",
		"http://127.0.0.1:1234",
		"http://[::1]:1234",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := NewAgentCacheCredentials(AgentCacheConfig{
				Endpoint: endpoint + "/v3", JobID: testCacheJobID, JobToken: "job-token", ResultsURL: endpoint,
			}); err != nil {
				t.Fatalf("NewAgentCacheCredentials(%q): %v", endpoint, err)
			}
		})
	}
}

func TestAgentCacheCredentialsRejectsUnsafeConfiguration(t *testing.T) {
	valid := AgentCacheConfig{
		Endpoint: "https://agent.example/v3", JobID: testCacheJobID,
		JobToken: "job-token", ResultsURL: "https://cache.example",
	}
	for name, mutate := range map[string]func(*AgentCacheConfig){
		"missing endpoint":              func(c *AgentCacheConfig) { c.Endpoint = "" },
		"endpoint credentials":          func(c *AgentCacheConfig) { c.Endpoint = "https://user@agent.example/v3" },
		"endpoint query":                func(c *AgentCacheConfig) { c.Endpoint += "?redirect=other" },
		"endpoint plaintext hostname":   func(c *AgentCacheConfig) { c.Endpoint = "http://localhost/v3" },
		"endpoint plaintext private IP": func(c *AgentCacheConfig) { c.Endpoint = "http://10.0.0.1/v3" },
		"endpoint plaintext public IP":  func(c *AgentCacheConfig) { c.Endpoint = "http://203.0.113.1/v3" },
		"invalid job ID":                func(c *AgentCacheConfig) { c.JobID = "../other" },
		"missing job token":             func(c *AgentCacheConfig) { c.JobToken = "" },
		"job token header split":        func(c *AgentCacheConfig) { c.JobToken = "secret\r\nOther: value" },
		"results credentials":           func(c *AgentCacheConfig) { c.ResultsURL = "https://user@cache.example/v2" },
		"results path":                  func(c *AgentCacheConfig) { c.ResultsURL += "/v2" },
		"results query":                 func(c *AgentCacheConfig) { c.ResultsURL += "?token=value" },
		"results plaintext hostname":    func(c *AgentCacheConfig) { c.ResultsURL = "http://localhost" },
		"results plaintext private IP":  func(c *AgentCacheConfig) { c.ResultsURL = "http://10.0.0.1" },
		"results plaintext public IP":   func(c *AgentCacheConfig) { c.ResultsURL = "http://203.0.113.1" },
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
			_, err = provider.Credentials(t.Context())
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
	if _, err := provider.Credentials(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP 307") || redirected {
		t.Fatalf("redirect Credentials() error/redirected = %v / %v", err, redirected)
	}
}

func TestAgentCacheCredentialsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
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

func TestCacheServiceLifecycleUsesFreshIsolatedCredentials(t *testing.T) {
	for _, test := range []struct {
		name   string
		using  string
		commit string
	}{
		{name: "v4.3.0", using: "node20", commit: actionintegration.CacheV4Commit},
		{name: "v6.1.0", using: "node24", commit: actionintegration.CacheCommit},
	} {
		t.Run(test.name, func(t *testing.T) {
			testCacheServiceLifecycleUsesFreshIsolatedCredentials(t, test.using, test.commit)
		})
	}
}

func testCacheServiceLifecycleUsesFreshIsolatedCredentials(t *testing.T, using, commit string) {
	t.Helper()
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/cache.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: cache lifecycle\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "package.json", `{"type":"module"}`)
	writeFixtureFile(t, remote, "action.yml", "name: cache service fixture\nruns:\n  using: "+using+"\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		program := fmt.Sprintf(`import fs from "node:fs";
import {spawnSync} from "node:child_process";
if (process.versions.node.split(".")[0] !== "24") throw new Error("actions/cache did not use managed Node 24");
const required = ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_CACHE_URL"];
for (const name of required) if (!process.env[name]) throw new Error("missing " + name);
if (process.env.ACTIONS_CACHE_URL !== %q) throw new Error("unexpected ACTIONS_CACHE_URL: " + process.env.ACTIONS_CACHE_URL);
if (process.env.GITHUB_SERVER_URL !== %q) throw new Error("unexpected GITHUB_SERVER_URL: " + process.env.GITHUB_SERVER_URL);
for (const name of [
  "ACTIONS_RUNTIME_URL",
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
`, cacheURLCompatibility, githubServerURLOverride, cacheActionToolPath, phase)
		writeFixtureFile(t, remote, phase+".js", program)
	}
	writeFixtureFile(t, remote, "ordinary/action.yml", "name: ordinary\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, remote, "ordinary/"+phase+".js", fmt.Sprintf(`import fs from "node:fs";
for (const name of ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"])
  if (!process.env[name]) throw new Error("missing " + name);
for (const name of ["ACTIONS_CACHE_URL", "ACTIONS_RUNTIME_URL", "BUILDKITE_AGENT_ACCESS_TOKEN"])
  if (process.env[name]) throw new Error(name + " leaked");
if (process.env.GITHUB_SERVER_URL !== "https://origin.cursor.com") throw new Error("ordinary action server URL changed");
if (process.env.PATH === %q) throw new Error("ordinary action PATH was isolated");
fs.appendFileSync(process.env.LIFECYCLE_LOG, "ordinary-%s|" + process.env.ACTIONS_RUNTIME_TOKEN + "|" + process.env.ACTIONS_RESULTS_URL + "|" + process.env.ACTIONS_CACHE_SERVICE_V2 + "\n");
console.log("ordinary-credential=" + process.env.ACTIONS_RUNTIME_TOKEN);
`, cacheActionToolPath, phase))
	}
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
		{ID: "shell-before", Kind: "run", Command: `test -z "${ACTIONS_RUNTIME_TOKEN:-}" && test -z "${ACTIONS_RESULTS_URL:-}" && test -z "${ACTIONS_CACHE_SERVICE_V2:-}"`},
		{ID: "poison-path", Kind: "run", Command: `printf '%s\n' "$ATTACKER_BIN" >> "$GITHUB_PATH"`},
		{ID: "ordinary", Kind: "uses", Uses: "owner/repo/ordinary@v1", Env: map[string]string{
			"ACTIONS_RESULTS_URL": "https://attacker.invalid", "ACTIONS_RUNTIME_TOKEN": "workflow-token", "ACTIONS_CACHE_SERVICE_V2": "false",
			"ACTIONS_CACHE_URL": "https://legacy.invalid", "ACTIONS_RUNTIME_URL": "https://legacy.invalid",
		}, Action: &plan.ActionSelector{Lock: ordinaryID}},
		{ID: "cache", Kind: "uses", Uses: "actions/cache@" + commit, Env: cacheEnv, Action: &plan.ActionSelector{Lock: cacheID}},
		{ID: "shell-after", Kind: "run", Command: `test -z "${ACTIONS_RUNTIME_TOKEN:-}" && test -z "${ACTIONS_RESULTS_URL:-}" && test -z "${ACTIONS_CACHE_SERVICE_V2:-}"`},
	})
	job.Schema = plan.Schema
	job.Event.Provider = "cursor-origin"
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle, "ATTACKER_BIN": attackerBin}
	job.Actions = []plan.ActionLock{
		{ID: cacheID, Source: "github", Repository: "actions/cache", RequestedRef: commit, Commit: commit, SourceDigest: digest},
		{ID: ordinaryID, Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: strings.Repeat("a", 40), Path: "ordinary", SourceDigest: digest},
	}
	provider := &sequenceCacheCredentials{tokens: []string{
		"header.first.signature", "header.second.signature", "header.third.signature",
		"header.fourth.signature", "header.fifth.signature", "header.sixth.signature",
	}}
	redactor := &testRedactor{}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	var logs bytes.Buffer
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token-must-not-leak")
	result, err := (Runner{
		Node24: node, Actions: materializer, Cache: provider, Redactor: redactor,
		Stdout: &logs, Stderr: &logs,
	}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	contents, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	wantPhases := map[string]bool{
		"pre": false, "main": false, "post": false,
		"ordinary-pre": false, "ordinary-main": false, "ordinary-post": false,
	}
	seenTokens := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 4 || fields[2] != "https://cache.example/" || fields[3] != "true" {
			t.Fatalf("invalid lifecycle record %q in %q", line, contents)
		}
		if _, ok := wantPhases[fields[0]]; !ok || wantPhases[fields[0]] {
			t.Fatalf("unexpected or duplicate lifecycle phase %q in %q", fields[0], contents)
		}
		wantPhases[fields[0]] = true
		if seenTokens[fields[1]] {
			t.Fatalf("cache credential %q was reused in %q", fields[1], contents)
		}
		seenTokens[fields[1]] = true
	}
	for phase, seen := range wantPhases {
		if !seen {
			t.Fatalf("lifecycle phase %q missing from %q", phase, contents)
		}
	}
	for _, token := range provider.tokens {
		if !seenTokens[token] {
			t.Fatalf("cache credential %q missing from %q", token, contents)
		}
	}
	if provider.calls != 6 || fmt.Sprint(redactor.values) != fmt.Sprint(provider.tokens) {
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
	if _, err := (Runner{Node24: node, Actions: materializer, Cache: provider, Redactor: redactor}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), actionintegration.CacheCommit) {
		t.Fatalf("unsupported runtime cache commit error = %v", err)
	}
}

func TestShouldOverrideGitHubServerURL(t *testing.T) {
	cases := []struct {
		name      string
		serverURL string
		want      bool
	}{
		{"github.com", "https://github.com", false},
		{"ghe.com cloud", "https://x.ghe.com", false},
		{"dot-localhost", "https://buildkite-gha.localhost", false},
		{"cursor origin", "https://origin.cursor.com", true},
		{"real GHES", "https://ghe.corp", true},
		{"empty", "", false},
		{"malformed", "://bad", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldOverrideGitHubServerURL(c.serverURL); got != c.want {
				t.Fatalf("shouldOverrideGitHubServerURL(%q) = %v, want %v", c.serverURL, got, c.want)
			}
		})
	}
}

func TestIsolateCacheActionEnvironmentLeavesGitHubServerURLForSharedOverride(t *testing.T) {
	env := map[string]string{
		"GITHUB_SERVER_URL":            "https://origin.cursor.com",
		"ACTIONS_CACHE_SERVICE_V2":     "true",
		"ACTIONS_RESULTS_URL":          "https://ghacs.buildkite.com/",
		"ACTIONS_RUNTIME_TOKEN":        "a.b.c",
		"BUILDKITE_AGENT_ACCESS_TOKEN": "secret",
		"BUILDKITE_JOB_ID":             "job-id",
	}
	isolated := isolateCacheActionEnvironment(env)
	if got := isolated["GITHUB_SERVER_URL"]; got != "https://origin.cursor.com" {
		t.Fatalf("GITHUB_SERVER_URL = %q, want unchanged https://origin.cursor.com", got)
	}
	if isolated["PATH"] != cacheActionToolPath {
		t.Fatalf("PATH = %q, want %q", isolated["PATH"], cacheActionToolPath)
	}
	for _, name := range []string{"BUILDKITE_AGENT_ACCESS_TOKEN", "BUILDKITE_JOB_ID"} {
		if _, ok := isolated[name]; ok {
			t.Fatalf("%s should have been stripped from the isolated environment", name)
		}
	}
}

func TestIsolateCacheActionEnvironmentLeavesRealGitHubServerURLUnchanged(t *testing.T) {
	env := map[string]string{
		"GITHUB_SERVER_URL": "https://github.com",
	}
	isolated := isolateCacheActionEnvironment(env)
	if got := isolated["GITHUB_SERVER_URL"]; got != "https://github.com" {
		t.Fatalf("GITHUB_SERVER_URL = %q, want unchanged https://github.com", got)
	}
}

func TestSetupActionsUseCacheClientCompatibilityWithoutCacheActionIsolation(t *testing.T) {
	node := requireNode24(t)
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: setup fixture\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n  post-if: success()\n")
	for _, phase := range []string{"main", "post"} {
		writeFixtureFile(t, remote, phase+".js", fmt.Sprintf(`const fs = require("node:fs");
if (process.env.GITHUB_SERVER_URL !== %q) throw new Error("unexpected GITHUB_SERVER_URL: " + process.env.GITHUB_SERVER_URL);
if (process.env.HTTP_PROXY !== "http://proxy.example") throw new Error("setup action environment was isolated");
for (const name of ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"])
  if (!process.env[name]) throw new Error("missing " + name);
if ((process.env.ACTIONS_CACHE_URL || "") !== process.env.EXPECTED_CACHE_URL)
  throw new Error("unexpected ACTIONS_CACHE_URL: " + process.env.ACTIONS_CACHE_URL);
fs.appendFileSync(process.env.LIFECYCLE_LOG, %q + "\n");
`, githubServerURLOverride, phase))
	}
	digest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}

	for _, setup := range []struct {
		repository string
		ref        string
	}{
		{repository: "actions/setup-node", ref: "v4"},
		{repository: "actions/setup-java", ref: "v4"},
		{repository: "actions/setup-python", ref: "v5"},
		{repository: "actions/setup-go", ref: "v5"},
		{repository: "actions/setup-dotnet", ref: "v4"},
	} {
		t.Run(strings.TrimPrefix(setup.repository, "actions/"), func(t *testing.T) {
			provider := &sequenceCacheCredentials{tokens: []string{"header.main.signature", "header.post.signature"}}
			workspace := t.TempDir()
			workflowPath := ".github/workflows/setup.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: setup action override\n")
			lifecycle := filepath.Join(workspace, "lifecycle.log")
			lockID := remoteLifecycleLockID(1)
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
				ID: "setup", Kind: "uses", Uses: setup.repository + "@" + setup.ref, Action: &plan.ActionSelector{Lock: lockID},
			}})
			job.Schema = plan.Schema
			job.Event.Provider = "cursor-origin"
			job.RequiredCapabilities = []string{"network"}
			job.Env = map[string]string{
				"EXPECTED_CACHE_URL": cacheURLCompatibility,
				"HTTP_PROXY":         "http://proxy.example",
				"LIFECYCLE_LOG":      lifecycle,
			}
			job.Actions = []plan.ActionLock{{
				ID: lockID, Source: "github", Repository: setup.repository, RequestedRef: setup.ref,
				Commit: strings.Repeat("a", 40), SourceDigest: digest,
			}}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
			result, err := (Runner{Node24: node, Actions: materializer, Cache: provider, Redactor: &testRedactor{}}).RunJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			contents, err := os.ReadFile(lifecycle)
			if err != nil || string(contents) != "main\npost\n" {
				t.Fatalf("setup lifecycle = %q, %v", contents, err)
			}
			if provider.calls != 2 {
				t.Fatalf("cache credential calls = %d, want 2", provider.calls)
			}
		})
	}
}

func TestActionCacheRedactorIsPinnedBeforeWorkflowExecution(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/cache-redactor.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: cache redactor\n")
	actionPath := ".github/actions/generic"
	writeFixtureFile(t, workspace, actionPath+"/action.yml", "name: generic\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, workspace, actionPath+"/main.js", "")

	token := "header.pinned.signature"
	trustedDir := canonicalTempDir(t)
	trustedMarker := filepath.Join(t.TempDir(), "trusted-agent-ran")
	trustedAgent := filepath.Join(trustedDir, "buildkite-agent")
	writeFixtureFile(t, trustedDir, "buildkite-agent", "#!/bin/sh\nIFS= read -r value\ntest \"$value\" = "+shellTestQuote(token)+"\n: > "+shellTestQuote(trustedMarker)+"\n")
	if err := os.Chmod(trustedAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	lookupDir := t.TempDir()
	lookupAgent := filepath.Join(lookupDir, "buildkite-agent")
	if err := os.Symlink(trustedAgent, lookupAgent); err != nil {
		t.Fatal(err)
	}
	poisonDir := canonicalTempDir(t)
	poisonMarker := filepath.Join(t.TempDir(), "poison-agent-ran")
	poisonAgent := filepath.Join(poisonDir, "buildkite-agent")
	writeFixtureFile(t, poisonDir, "buildkite-agent", "#!/bin/sh\n: > "+shellTestQuote(poisonMarker)+"\nexit 99\n")
	if err := os.Chmod(poisonAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", lookupDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "poison", Kind: "run", Command: `rm -f "$LOOKUP_AGENT" && ln -s "$POISON_AGENT" "$LOOKUP_AGENT"`},
		{ID: "generic", Kind: "uses", Uses: "./" + actionPath, Action: &plan.ActionSelector{Lock: lockID}},
	})
	job.Schema = plan.Schema
	job.Env = map[string]string{"LOOKUP_AGENT": lookupAgent, "POISON_AGENT": poisonAgent}
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "workspace", Path: actionPath,
		SourceDigest: digestTree(t, filepath.Join(workspace, filepath.FromSlash(actionPath))),
	}}
	provider := &sequenceCacheCredentials{tokens: []string{token}}
	result, err := (Runner{Node24: node, Cache: provider, Redactor: AgentRedactor{}}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if target, err := filepath.EvalSymlinks(lookupAgent); err != nil || target != poisonAgent {
		t.Fatalf("workflow Agent replacement = %q, %v; want %q", target, err, poisonAgent)
	}
	if _, err := os.Stat(trustedMarker); err != nil {
		t.Fatalf("trusted Agent redactor did not run: %v", err)
	}
	if _, err := os.Stat(poisonMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workflow-selected Agent received cache credential or marker stat failed: %v", err)
	}
}

func TestGenericActionCacheDisablesWhenRedactorCannotBePinned(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/cache-fallback.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: cache fallback\n")
	actionPath := ".github/actions/generic"
	writeFixtureFile(t, workspace, actionPath+"/action.yml", "name: generic\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, workspace, actionPath+"/main.js", `for (const name of ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"]) if (process.env[name]) throw new Error(name + " leaked");`)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "generic", Kind: "uses", Uses: "./" + actionPath, Action: &plan.ActionSelector{Lock: lockID}}})
	job.Schema = plan.Schema
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "workspace", Path: actionPath,
		SourceDigest: digestTree(t, filepath.Join(workspace, filepath.FromSlash(actionPath))),
	}}
	provider := &sequenceCacheCredentials{tokens: []string{"header.unused.signature"}}
	result, err := (Runner{
		Node24: node, Cache: provider,
		Redactor: AgentRedactor{Executable: filepath.Join(t.TempDir(), "missing-agent")},
	}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if provider.calls != 0 {
		t.Fatalf("cache credential provider called %d times after redactor fallback", provider.calls)
	}
}

func TestExplicitCacheRequiresPinnedRedactorBeforeWorkflowExecution(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/cache-strict.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: strict cache\n")
	marker := filepath.Join(workspace, "workflow-ran")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: cache\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, remote, "main.js", "")
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "run", Kind: "run", Command: `: > "$MARKER"`},
		{ID: "cache", Kind: "uses", Uses: "actions/cache@" + actionintegration.CacheCommit, Action: &plan.ActionSelector{Lock: lockID}},
	})
	job.Schema = plan.Schema
	job.Env = map[string]string{"MARKER": marker}
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "github", Repository: "actions/cache",
		RequestedRef: actionintegration.CacheCommit, Commit: actionintegration.CacheCommit,
		SourceDigest: digestTree(t, remote),
	}}
	provider := &sequenceCacheCredentials{tokens: []string{"header.unused.signature"}}
	_, err := (Runner{
		Cache:    provider,
		Redactor: AgentRedactor{Executable: filepath.Join(t.TempDir(), "missing-agent")},
	}).RunJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "resolve Buildkite Agent redactor before workflow execution") {
		t.Fatalf("RunJob() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workflow executed before strict cache redactor failure: %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("cache credential provider called %d times before redactor failure", provider.calls)
	}
}

func TestGenericJavaScriptCacheCredentialFailureFallsBackUncached(t *testing.T) {
	node := requireNode24(t)
	actionRoot := t.TempDir()
	marker := filepath.Join(actionRoot, "executed")
	writeFixtureFile(t, actionRoot, "main.js", `const fs = require("node:fs");
for (const name of ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"])
  if (process.env[name]) throw new Error(name + " leaked");
fs.writeFileSync(process.env.MARKER, "executed");
`)
	provider := cacheCredentialProviderFunc(func(context.Context) (CacheCredentials, error) {
		return CacheCredentials{}, fmt.Errorf("cache unavailable")
	})
	result := newResult()
	result.Env["MARKER"] = marker
	result.Env["ACTIONS_RUNTIME_TOKEN"] = "workflow-token"
	processor := newCommandProcessor(io.Discard, io.Discard)
	runner := Runner{Cache: provider, Redactor: &testRedactor{}}
	if err := runner.runJavaScriptPhase(
		t.Context(), processor, actionRoot, node,
		javaScriptAction{Name: "ordinary", Path: actionRoot, Main: "main.js"}, "main.js", nil, nil, &result,
	); err != nil {
		t.Fatalf("generic action cache fallback error = %v", err)
	}
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "executed" {
		t.Fatalf("generic action marker = %q, %v", contents, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := runner.runJavaScriptPhase(
		t.Context(), processor, actionRoot, node,
		javaScriptAction{Name: "cache", Path: actionRoot, Main: "main.js", Cache: true}, "main.js", nil, nil, &result,
	); err == nil || !strings.Contains(err.Error(), "configure actions/cache service: cache unavailable") {
		t.Fatalf("explicit cache action error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("strict cache action executed or marker stat failed: %v", err)
	}
}

func TestDockerActionReceivesCacheCredentialsWithoutTokenInArguments(t *testing.T) {
	token := "header.docker.signature"
	provider := &sequenceCacheCredentials{tokens: []string{token}}
	redactor := &testRedactor{}
	fake := newFakeDocker(t, "success")
	action := fakeDockerAction(t)
	action.Env["ACTIONS_RUNTIME_TOKEN"] = "workflow-token"
	action.Env["ACTIONS_RESULTS_URL"] = "https://attacker.invalid"
	action.Env["ACTIONS_CACHE_SERVICE_V2"] = "false"
	if _, err := (Runner{Docker: fake.path, Cache: provider, Redactor: redactor}).runDockerAction(t.Context(), action); err != nil {
		t.Fatal(err)
	}
	cacheRuntime, err := os.ReadFile(filepath.Join(fake.root, "cache-runtime"))
	if err != nil || string(cacheRuntime) != token+"|https://cache.example/|true" {
		t.Fatalf("Docker cache environment = %q, %v", cacheRuntime, err)
	}
	calls := fake.calls(t)
	runIndex := callIndex(calls, "run")
	if runIndex < 0 {
		t.Fatalf("Docker run absent: %#v", calls)
	}
	arguments := strings.Join(calls[runIndex].args, "\x00")
	if strings.Contains(arguments, token) {
		t.Fatalf("Docker arguments contain cache credential: %#v", calls[runIndex].args)
	}
	for _, name := range []string{"ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"} {
		if !strings.Contains(arguments, "\x00--env\x00"+name+"\x00") {
			t.Fatalf("Docker arguments omit inherited %s: %#v", name, calls[runIndex].args)
		}
	}
	if provider.calls != 1 || fmt.Sprint(redactor.values) != fmt.Sprint([]string{token}) {
		t.Fatalf("credential calls/redactions = %d / %#v", provider.calls, redactor.values)
	}
}

type failingCacheRedactor struct{ token string }

func (r failingCacheRedactor) AddRedaction(context.Context, string) error {
	return fmt.Errorf("redactor rejected %s", r.token)
}

func TestCacheRedactorFailureAbortsBeforeExecutionAndScrubsToken(t *testing.T) {
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
		t.Context(), processor, actionRoot, "node", javaScriptAction{Name: "cache", Path: actionRoot, Main: "main.js", Cache: true}, "main.js", nil, nil, &result,
	)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("runJavaScriptPhase() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("action marker exists or stat failed unexpectedly: %v", err)
	}
}

func TestActionRuntimeCacheTokenCommandFileEffectsAreDiscarded(t *testing.T) {
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
				t.Context(), newCommandProcessor(io.Discard, io.Discard), actionRoot, node,
				javaScriptAction{Name: "ordinary", Path: actionRoot, Main: "main.js"}, "main.js", nil, state, &result,
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
