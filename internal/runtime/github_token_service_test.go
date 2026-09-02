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
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "buildkite-gha/1.2.3" {
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
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"pull_requests": "write", "contents": "read"})
	if err != nil || token != statelessToken {
		t.Fatalf("WorkflowToken() = %q, %v", token, err)
	}
	for _, permissions := range []map[string]string{nil, {}, {"contents": "admin"}, {"administration": "write"}, {"models": "write"}} {
		if _, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", permissions); err == nil {
			t.Fatalf("WorkflowToken(%#v) succeeded", permissions)
		}
	}
	for _, permissions := range []map[string]string{{"models": "read"}, {"repository_projects": "read"}} {
		if _, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", permissions); err == nil {
			t.Fatalf("WorkflowToken(%#v) succeeded", permissions)
		}
	}
}

func TestAgentGitHubTokensMintsExactAllWorkflowPermissions(t *testing.T) {
	for _, access := range []string{"read", "write"} {
		t.Run(access, func(t *testing.T) {
			wantBody := `{"repo_url":"https://github.com/buildkite/buildkite-gha","workflow":"ci.yml","permissions":{"actions":"` + access + `","artifact_metadata":"` + access + `","attestations":"` + access + `","checks":"` + access + `","contents":"` + access + `","deployments":"` + access + `","discussions":"` + access + `","issues":"` + access + `","packages":"` + access + `","pages":"` + access + `","pull_requests":"` + access + `","security_events":"` + access + `","statuses":"` + access + `"}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != wantBody {
					t.Errorf("request body = %s, want %s", body, wantBody)
				}
				_, _ = io.WriteString(w, `{"token":"ghs_all"}`)
			}))
			defer server.Close()
			provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
			if err != nil {
				t.Fatal(err)
			}
			permissions := map[string]string{
				"actions": access, "artifact_metadata": access, "attestations": access, "checks": access, "contents": access,
				"deployments": access, "discussions": access, "issues": access, "packages": access, "pages": access,
				"pull_requests": access, "security_events": access, "statuses": access,
			}
			if token, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", permissions); err != nil || token != "ghs_all" {
				t.Fatalf("WorkflowToken(%s-all) = %q, %v", access, token, err)
			}
		})
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
	if token, err := provider.ActionSourceToken(t.Context(), "actions/checkout"); err != nil || token != "ghs_action_source" {
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
		if _, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", workflow, map[string]string{"contents": "read"}); err == nil {
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
		if _, err := provider.WorkflowToken(t.Context(), repository, "ci.yml", map[string]string{"contents": "read"}); err == nil {
			t.Fatalf("WorkflowToken(%q) succeeded", repository)
		}
		if _, err := provider.ActionSourceToken(t.Context(), repository); err == nil {
			t.Fatalf("ActionSourceToken(%q) succeeded", repository)
		}
	}
}

func TestAgentGitHubTokensRejectsRedirectsAndUntrustedResponses(t *testing.T) {
	secret := "ghs_must_not_leak"
	for _, test := range []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       string
	}{
		{"rejected", http.StatusBadRequest, secret, "", "rejected"},
		{"unauthorized", http.StatusUnauthorized, secret, "", "denied"},
		{"denied", http.StatusForbidden, secret, "", "denied"},
		{"disabled", http.StatusNotFound, secret, "", "not enabled"},
		{"rate limited", http.StatusServiceUnavailable, secret, "60", "retry after 60 seconds"},
		{"unsafe retry header", http.StatusServiceUnavailable, secret, secret, "temporarily unavailable"},
		{"unexpected status", http.StatusBadGateway, secret, "", "HTTP 502"},
		{"malformed JSON", http.StatusOK, `{"token":`, "", "decode"},
		{"unknown field", http.StatusOK, `{"token":"ghs_valid","other":true}`, "", "unknown field"},
		{"trailing JSON", http.StatusOK, `{"token":"ghs_valid"}{}`, "", "trailing data"},
		{"empty token", http.StatusOK, `{"token":""}`, "", "invalid token"},
		{"invalid token", http.StatusOK, `{"token":"secret with spaces"}`, "", "invalid token"},
		{"oversized", http.StatusOK, `{"token":"ghs_valid"}` + strings.Repeat(" ", githubTokenResponseLimit), "", "exceeds"},
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
			_, err = provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("WorkflowToken() error = %v, want %q without response data", err, test.want)
			}
			if ClassifyFailure(err) != FailureClassWorkflowToken {
				t.Fatalf("ClassifyFailure() = %q, want %q", ClassifyFailure(err), FailureClassWorkflowToken)
			}
			status, ok := AgentAPIHTTPStatus(err)
			if test.status >= 200 && test.status < 300 {
				if ok || status != 0 {
					t.Fatalf("AgentAPIHTTPStatus() = %d, %t for HTTP %d", status, ok, test.status)
				}
			} else if !ok || status != test.status {
				t.Fatalf("AgentAPIHTTPStatus() = %d, %t, want %d, true", status, ok, test.status)
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
	if _, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"}); err == nil || !strings.Contains(err.Error(), "HTTP 307") || redirected {
		t.Fatalf("redirect WorkflowToken() error/redirected = %v / %v", err, redirected)
	}
}

func TestAgentGitHubTokensRejectedWorkflowTokenGuidance(t *testing.T) {
	const responseData = "provider response must not escape"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, responseData)
	}))
	defer server.Close()

	for _, test := range []struct {
		name     string
		buildURL string
		wantURL  bool
	}{
		{name: "safe build URL", buildURL: "https://buildkite.com/acme-inc/my-pipeline/builds/42", wantURL: true},
		{name: "missing build URL"},
		{name: "unsafe build URL", buildURL: "https://attacker.invalid/builds/job-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{
				Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token", BuildURL: test.buildURL,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
			if err == nil {
				t.Fatal("WorkflowToken() succeeded")
			}
			message := err.Error()
			for _, want := range []string{"top-level permissions", "Buildkite GitHub App", "event repository", "contact Buildkite support", "this build's URL"} {
				if !strings.Contains(message, want) {
					t.Errorf("WorkflowToken() error = %q, want %q", message, want)
				}
			}
			if strings.Contains(message, "enable") || strings.Contains(message, "pipeline's repository settings") || strings.Contains(message, responseData) {
				t.Errorf("WorkflowToken() error contains unsafe or incorrect guidance: %q", message)
			}
			if got := strings.Contains(message, test.buildURL) && test.buildURL != ""; got != test.wantURL {
				t.Errorf("WorkflowToken() error contains build URL = %t, want %t: %q", got, test.wantURL, message)
			}
		})
	}
}

func TestAgentGitHubTokensKeepsActionSourceFailureUnclassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.ActionSourceToken(t.Context(), "actions/checkout")
	if err == nil || err.Error() != "GitHub action source token request was rejected" {
		t.Fatalf("ActionSourceToken() error = %v", err)
	}
	if ClassifyFailure(err) != FailureClassUnknown {
		t.Fatalf("ClassifyFailure() = %q, want %q", ClassifyFailure(err), FailureClassUnknown)
	}
	if status, ok := AgentAPIHTTPStatus(err); ok || status != 0 {
		t.Fatalf("AgentAPIHTTPStatus() = %d, %t, want 0, false", status, ok)
	}
}

func TestAgentGitHubTokensDisabledWorkflowTokenGuidance(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	for _, test := range []struct {
		name         string
		organization string
		pipeline     string
		want         string
	}{
		{
			name:         "affected pipeline link",
			organization: "acme-inc",
			pipeline:     "my-pipeline",
			want:         `GitHub workflow access tokens are not enabled for this organization or pipeline; enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings: https://buildkite.com/acme-inc/my-pipeline/settings/repository`,
		},
		{
			name:         "unsafe organization",
			organization: "acme/other",
			pipeline:     "my-pipeline",
			want:         `GitHub workflow access tokens are not enabled for this organization or pipeline; enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings`,
		},
		{
			name:         "unsafe pipeline",
			organization: "acme-inc",
			pipeline:     "my-pipeline?token=secret",
			want:         `GitHub workflow access tokens are not enabled for this organization or pipeline; enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings`,
		},
		{
			name: "missing identity",
			want: `GitHub workflow access tokens are not enabled for this organization or pipeline; enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{
				Endpoint:         server.URL,
				JobID:            testCacheJobID,
				JobToken:         "job-token",
				OrganizationSlug: test.organization,
				PipelineSlug:     test.pipeline,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
			if err == nil || err.Error() != test.want {
				t.Fatalf("WorkflowToken() error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestAgentGitHubTokensHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: "https://agent.invalid/v3", JobID: testCacheJobID, JobToken: "job-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.WorkflowToken(ctx, "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("WorkflowToken() error = %v, want cancellation", err)
	} else if ClassifyFailure(err) != FailureClassUnknown {
		t.Fatalf("ClassifyFailure() = %q, want %q", ClassifyFailure(err), FailureClassUnknown)
	} else if status, ok := AgentAPIHTTPStatus(err); ok || status != 0 {
		t.Fatalf("AgentAPIHTTPStatus() = %d, %t, want 0, false", status, ok)
	}
}

func TestAgentGitHubTokensAuthenticateUnrelatedPublicRepository(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_REQUIRED") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_REQUIRED=1 to verify job-scoped public GitHub access")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
