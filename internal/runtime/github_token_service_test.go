package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAgentGitHubTokensMintsExactWorkflowPermissions(t *testing.T) {
	const statelessToken = "ghs_header.payload-with-hyphen.signature"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs/"+testCacheJobID+"/github_workflow_access_token" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s headers %#v", r.Method, r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if string(body) != `{"repo_url":"https://github.com/buildkite/buildkite-gha","workflow":"ci.yml","permissions":{"contents":"read","pull_requests":"write"}}` {
			t.Errorf("request body = %s", body)
		}
		_, _ = io.WriteString(w, `{"token":"`+statelessToken+`"}`)
	}))
	defer server.Close()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"pull_requests": "write", "contents": "read"})
	if err != nil || token != statelessToken {
		t.Fatalf("WorkflowToken() = %q, %v", token, err)
	}
	for _, permissions := range []map[string]string{nil, {}, {"contents": "admin"}, {"administration": "write"}, {"models": "write"}} {
		if _, err := provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", "ci.yml", permissions); err == nil {
			t.Fatalf("WorkflowToken(%#v) succeeded", permissions)
		}
	}
	for _, permissions := range []map[string]string{{"models": "read"}, {"repository_projects": "read"}} {
		if _, err := provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", "ci.yml", permissions); err == nil {
			t.Fatalf("WorkflowToken(%#v) succeeded", permissions)
		}
	}
}

func TestAgentGitHubTokensMintsActionSourceTokenWithRepositoryOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs/"+testCacheJobID+"/github_action_source_access_token" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s headers %#v", r.Method, r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"repo_url":"https://github.com/actions/checkout"}` {
			t.Errorf("request body = %s", body)
		}
		_, _ = io.WriteString(w, `{"token":"ghs_action_source"}`)
	}))
	defer server.Close()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if token, err := provider.ActionSourceToken(context.Background(), "actions/checkout"); err != nil || token != "ghs_action_source" {
		t.Fatalf("ActionSourceToken() = %q, %v", token, err)
	}
}

func TestAgentGitHubTokensRejectsInvalidWorkflowBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, workflow := range []string{"", "../ci.yml", "nested/ci.yml", ".ci.yml", "ci.json"} {
		if _, err := provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", workflow, map[string]string{"contents": "read"}); err == nil {
			t.Errorf("WorkflowToken(%q) succeeded", workflow)
		}
	}
	if requests != 0 {
		t.Fatalf("network requests = %d, want 0", requests)
	}
}

func TestAgentGitHubTokensAcceptsHTTPSAndLoopbackHTTP(t *testing.T) {
	for _, endpoint := range []string{
		"https://agent.example/v3",
		"http://127.0.0.1:1234/v3",
		"http://[::1]:1234/v3",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{
				Endpoint: endpoint, JobID: testCacheJobID, JobToken: "job-token",
			}); err != nil {
				t.Fatalf("NewAgentGitHubTokens(%q): %v", endpoint, err)
			}
		})
	}
}

func TestAgentGitHubTokensRejectsUnsafeConfigurationAndRepository(t *testing.T) {
	valid := AgentGitHubTokenConfig{Endpoint: "https://agent.example/v3", JobID: testCacheJobID, JobToken: "job-token"}
	for name, mutate := range map[string]func(*AgentGitHubTokenConfig){
		"missing endpoint":              func(c *AgentGitHubTokenConfig) { c.Endpoint = "" },
		"endpoint credentials":          func(c *AgentGitHubTokenConfig) { c.Endpoint = "https://user@agent.example/v3" },
		"endpoint query":                func(c *AgentGitHubTokenConfig) { c.Endpoint += "?redirect=other" },
		"endpoint plaintext hostname":   func(c *AgentGitHubTokenConfig) { c.Endpoint = "http://localhost/v3" },
		"endpoint plaintext private IP": func(c *AgentGitHubTokenConfig) { c.Endpoint = "http://10.0.0.1/v3" },
		"endpoint plaintext public IP":  func(c *AgentGitHubTokenConfig) { c.Endpoint = "http://203.0.113.1/v3" },
		"invalid job ID":                func(c *AgentGitHubTokenConfig) { c.JobID = "../other" },
		"missing job token":             func(c *AgentGitHubTokenConfig) { c.JobToken = "" },
		"job token header split":        func(c *AgentGitHubTokenConfig) { c.JobToken = "secret\r\nOther: value" },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewAgentGitHubTokens(config); err == nil {
				t.Fatalf("NewAgentGitHubTokens(%#v) succeeded", config)
			}
		})
	}
	provider, err := NewAgentGitHubTokens(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range []string{"", "other", "../other/repo", "owner/..", "owner/repo/extra", "owner/repo?permission=write"} {
		if _, err := provider.WorkflowToken(context.Background(), repository, "ci.yml", map[string]string{"contents": "read"}); err == nil {
			t.Fatalf("WorkflowToken(%q) succeeded", repository)
		}
		if _, err := provider.ActionSourceToken(context.Background(), repository); err == nil {
			t.Fatalf("ActionSourceToken(%q) succeeded", repository)
		}
	}
}

func TestAgentGitHubTokensRejectsRedirectsAndUntrustedResponses(t *testing.T) {
	// Every response carries its own distinct secret so a leak names the branch that
	// leaked it. Only the 400 branch may echo its own body: that body is the Agent
	// API's rejection reason, which operators need to see.
	const leakPrefix = "ghs_must_not_leak"
	leaked := func(name string) string { return leakPrefix + "_" + name }
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       string
	}{
		{"rejected", http.StatusBadRequest, leaked("rejected"), "", "rejected"},
		{"unauthorized", http.StatusUnauthorized, leaked("unauthorized"), "", "denied"},
		{"denied", http.StatusForbidden, leaked("denied"), "", "denied"},
		{"disabled", http.StatusNotFound, leaked("disabled"), "", "not enabled"},
		{"rate limited", http.StatusServiceUnavailable, leaked("rate_limited"), "60", "retry after 60 seconds"},
		{"unsafe retry header", http.StatusServiceUnavailable, leaked("unsafe_body"), leaked("unsafe_header"), "temporarily unavailable"},
		{"unexpected status", http.StatusBadGateway, leaked("unexpected_status"), "", "HTTP 502"},
		{"malformed JSON", http.StatusOK, `{"token":`, "", "decode"},
		{"unknown field", http.StatusOK, `{"token":"ghs_valid","other":true}`, "", "unknown field"},
		{"trailing JSON", http.StatusOK, `{"token":"ghs_valid"}{}`, "", "trailing data"},
		{"empty token", http.StatusOK, `{"token":""}`, "", "invalid token"},
		{"invalid token", http.StatusOK, `{"token":"secret with spaces"}`, "", "invalid token"},
		{"oversized", http.StatusOK, `{"token":"ghs_valid"}` + strings.Repeat(" ", githubTokenResponseLimit), "", "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("WorkflowToken() error = %v, want %q", err, test.want)
			}
			if test.status == http.StatusBadRequest && !strings.Contains(err.Error(), test.body) {
				t.Fatalf("WorkflowToken() error = %v, want the rejection reason from the response body", err)
			}
			for _, other := range tests {
				echoesOwnBody := other.name == test.name && test.status == http.StatusBadRequest
				if strings.HasPrefix(other.body, leakPrefix) && !echoesOwnBody && strings.Contains(err.Error(), other.body) {
					t.Fatalf("WorkflowToken() error = %v, leaked the %q response body", err, other.name)
				}
				if strings.HasPrefix(other.retryAfter, leakPrefix) && strings.Contains(err.Error(), other.retryAfter) {
					t.Fatalf("WorkflowToken() error = %v, leaked the %q retry header", err, other.name)
				}
			}
		})
	}

	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			_, _ = io.WriteString(w, `{"token":"ghs_valid"}`)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"}); err == nil || !strings.Contains(err.Error(), "HTTP 307") || redirected {
		t.Fatalf("redirect WorkflowToken() error/redirected = %v / %v", err, redirected)
	}
}

func TestAgentGitHubTokensAppendsSanitizedRejectionReason(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       string
	}{
		{
			name:   "JSON message",
			status: http.StatusBadRequest,
			body:   `{"message":"Repository provider refused to issue token"}`,
			want:   "GitHub workflow token request was rejected: Repository provider refused to issue token",
		},
		{
			name:   "plain text body",
			status: http.StatusBadRequest,
			body:   "Repository provider refused to issue token",
			want:   "GitHub workflow token request was rejected: Repository provider refused to issue token",
		},
		{
			name:   "empty body",
			status: http.StatusBadRequest,
			body:   "",
			want:   "GitHub workflow token request was rejected",
		},
		{
			name:       "rate limited unaffected by body",
			status:     http.StatusServiceUnavailable,
			body:       `{"message":"Repository provider refused to issue token"}`,
			retryAfter: "30",
			want:       "GitHub workflow token service is temporarily unavailable; retry after 30 seconds",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
			if err == nil || err.Error() != test.want {
				t.Fatalf("WorkflowToken() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentGitHubTokensCollapsesAndTruncatesRejectionReason(t *testing.T) {
	body := "line one\r\n\nline two\t\t" + strings.Repeat("z", 300)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.WorkflowToken(context.Background(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
	if err == nil {
		t.Fatal("WorkflowToken() succeeded, want error")
	}
	const prefix = "GitHub workflow token request was rejected: "
	message := err.Error()
	if !strings.HasPrefix(message, prefix) {
		t.Fatalf("WorkflowToken() error = %q, want prefix %q", message, prefix)
	}
	reason := strings.TrimPrefix(message, prefix)
	if strings.ContainsAny(reason, "\r\n\t") {
		t.Fatalf("reason retains control characters: %q", reason)
	}
	if !strings.Contains(reason, "line one line two") {
		t.Fatalf("reason lost its collapsed text: %q", reason)
	}
	if got := len([]rune(reason)); got != githubTokenRejectionReasonRuneLimit {
		t.Fatalf("reason length = %d, want %d", got, githubTokenRejectionReasonRuneLimit)
	}
}

func TestAgentGitHubTokensHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: "https://agent.invalid/v3", JobID: testCacheJobID, JobToken: "job-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.WorkflowToken(ctx, "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("WorkflowToken() error = %v, want cancellation", err)
	}
}

func TestAgentGitHubTokensAuthenticateUnrelatedPublicRepository(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_REQUIRED") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_REQUIRED=1 to verify job-scoped public GitHub access")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{
		Endpoint: os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
		JobID:    os.Getenv("BUILDKITE_JOB_ID"),
		JobToken: os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.ActionSourceToken(ctx, "buildkite/buildkite-gha")
	if err != nil {
		if strings.Contains(err.Error(), "not enabled") || strings.Contains(err.Error(), "temporarily unavailable") {
			t.Skipf("GitHub action source token rollout is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := (AgentRedactor{Executable: os.Getenv("BUILDKITE_GHA_AGENT")}).AddRedaction(ctx, token); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	const commit = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	for _, test := range []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "metadata", path: "/repos/actions/checkout", wantStatus: http.StatusOK},
		{name: "tag", path: "/repos/actions/checkout/git/ref/tags/v4", wantStatus: http.StatusOK},
		{name: "commit", path: "/repos/actions/checkout/commits/" + commit, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Accept", "application/vnd.github+json")
			request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("GitHub public %s request returned HTTP %d", test.name, response.StatusCode)
			}
			if test.name == "metadata" {
				limit, err := strconv.Atoi(response.Header.Get("X-RateLimit-Limit"))
				if err != nil || limit <= 60 {
					t.Fatalf("GitHub public metadata request rate limit = %q, want authenticated budget", response.Header.Get("X-RateLimit-Limit"))
				}
				var repository struct {
					Visibility string `json:"visibility"`
				}
				if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&repository); err != nil || repository.Visibility != "public" {
					t.Fatalf("GitHub public repository metadata visibility = %q, error = %v", repository.Visibility, err)
				}
				return
			}
		})
	}
}
