package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentGitHubTokensMintsFixedReadOnlyRepositoryCredential(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v3/jobs/"+testCacheJobID+"/github_scoped_access_token" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request headers = %#v", r.Header)
		}
		if string(body) != `{"repo_url":"https://github.com/buildkite/buildkite-gha","permissions":{"contents":"read"}}` {
			t.Errorf("request body = %s", body)
		}
		_, _ = io.WriteString(w, `{"token":"ghs_scoped_token"}`)
	}))
	defer server.Close()

	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{
		Endpoint: server.URL + "/v3/", JobID: testCacheJobID, JobToken: "job-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.Token(context.Background(), "buildkite/buildkite-gha")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || token != "ghs_scoped_token" {
		t.Fatalf("calls/token = %d / %q", calls, token)
	}
}

func TestAgentGitHubTokensRejectsUnsafeConfigurationAndRepository(t *testing.T) {
	valid := AgentGitHubTokenConfig{Endpoint: "https://agent.example/v3", JobID: testCacheJobID, JobToken: "job-token"}
	for name, mutate := range map[string]func(*AgentGitHubTokenConfig){
		"missing endpoint":       func(c *AgentGitHubTokenConfig) { c.Endpoint = "" },
		"endpoint credentials":   func(c *AgentGitHubTokenConfig) { c.Endpoint = "https://user@agent.example/v3" },
		"endpoint query":         func(c *AgentGitHubTokenConfig) { c.Endpoint += "?redirect=other" },
		"invalid job ID":         func(c *AgentGitHubTokenConfig) { c.JobID = "../other" },
		"missing job token":      func(c *AgentGitHubTokenConfig) { c.JobToken = "" },
		"job token header split": func(c *AgentGitHubTokenConfig) { c.JobToken = "secret\r\nOther: value" },
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
		if _, err := provider.Token(context.Background(), repository); err == nil {
			t.Fatalf("Token(%q) succeeded", repository)
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
			_, err = provider.Token(context.Background(), "buildkite/buildkite-gha")
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("Token() error = %v, want %q without response data", err, test.want)
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
	if _, err := provider.Token(context.Background(), "buildkite/buildkite-gha"); err == nil || !strings.Contains(err.Error(), "HTTP 307") || redirected {
		t.Fatalf("redirect Token() error/redirected = %v / %v", err, redirected)
	}
}

func TestAgentGitHubTokensHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: "https://agent.invalid/v3", JobID: testCacheJobID, JobToken: "job-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Token(ctx, "buildkite/buildkite-gha"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Token() error = %v, want cancellation", err)
	}
}
