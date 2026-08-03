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
		"missing results URL":    func(c *AgentCacheConfig) { c.ResultsURL = "" },
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
const required = ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"];
for (const name of required) if (!process.env[name]) throw new Error("missing " + name);
if (process.env.BUILDKITE_AGENT_ACCESS_TOKEN) throw new Error("job credential leaked");
if (process.env.ACTIONS_CACHE_URL) throw new Error("legacy cache URL leaked");
fs.appendFileSync(process.env.LIFECYCLE_LOG, "%s|" + process.env.ACTIONS_RUNTIME_TOKEN + "|" + process.env.ACTIONS_RESULTS_URL + "|" + process.env.ACTIONS_CACHE_SERVICE_V2 + "\n");
console.log("credential=" + process.env.ACTIONS_RUNTIME_TOKEN);
`, phase)
		writeFixtureFile(t, remote, phase+".js", program)
	}
	writeFixtureFile(t, remote, "ordinary/action.yml", "name: ordinary\nruns:\n  using: node24\n  main: index.js\n")
	writeFixtureFile(t, remote, "ordinary/index.js", `import fs from "node:fs";
for (const name of ["ACTIONS_CACHE_SERVICE_V2", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN", "BUILDKITE_AGENT_ACCESS_TOKEN"])
  if (process.env[name]) throw new Error(name + " leaked");
fs.appendFileSync(process.env.LIFECYCLE_LOG, "ordinary\n");
`)
	digest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	cacheID, ordinaryID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "ordinary", Kind: "uses", Uses: "owner/repo/ordinary@v1", Action: &plan.ActionSelector{Lock: ordinaryID}},
		{ID: "cache", Kind: "uses", Uses: "actions/cache@" + actionintegration.CacheCommit, Action: &plan.ActionSelector{Lock: cacheID}},
	})
	job.Schema = plan.SchemaV3
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
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
		context.Background(), processor, "node", JavaScriptAction{Name: "cache", Path: actionRoot, Main: "main.js", Cache: true}, "main.js", nil, nil, &result,
	)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("runJavaScriptPhase() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("action marker exists or stat failed unexpectedly: %v", err)
	}
}
