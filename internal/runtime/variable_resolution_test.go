package runtime

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestAgentVariableResolverResolvesBothScopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs/"+testCacheJobID+"/github-actions/variables" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "buildkite-gha/1.2.3" {
			t.Errorf("request = %s headers %#v", r.Method, r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"repo_url":"https://github.com/buildkite/buildkite-gha"}` {
			t.Errorf("request body = %s", body)
		}
		// Scopes arrive separately and sorted by name; the same name may appear
		// in both, and case differs between scopes. Nothing merges here.
		_, _ = io.WriteString(w, `{"repository_variables":[{"name":"AWS_REGION","value":"eu-west-1"},{"name":"EMPTY","value":""},{"name":"registry","value":"ghcr.io/team"}],`+
			`"organization_variables":[{"name":"AWS_REGION","value":"us-east-1"},{"name":"REGISTRY","value":"ghcr.io/acme"},{"name":"cert_pem","value":"-----BEGIN-----\nline\n"}]}`)
	}))
	defer server.Close()
	resolver, err := NewAgentVariableResolver(AgentVariableResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := resolver.ResolveVariables(t.Context(), "buildkite/buildkite-gha")
	if err != nil {
		t.Fatal(err)
	}
	want := VariablesSnapshot{
		Repository:   map[string]string{"AWS_REGION": "eu-west-1", "EMPTY": "", "registry": "ghcr.io/team"},
		Organization: map[string]string{"AWS_REGION": "us-east-1", "REGISTRY": "ghcr.io/acme", "cert_pem": "-----BEGIN-----\nline\n"},
	}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("ResolveVariables() = %#v, want %#v", snapshot, want)
	}
}

func TestAgentVariableResolverDecodesEmptyScopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"repository_variables":[],"organization_variables":[]}`)
	}))
	defer server.Close()
	resolver, err := NewAgentVariableResolver(AgentVariableResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := resolver.ResolveVariables(t.Context(), "buildkite/buildkite-gha")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Repository) != 0 || len(snapshot.Organization) != 0 {
		t.Fatalf("ResolveVariables() = %#v, want empty scopes", snapshot)
	}
}

func TestAgentVariableResolverRejectsInvalidInputBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	resolver, err := NewAgentVariableResolver(AgentVariableResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range []string{"", "other", "../other/repo", "owner/..", "owner/repo/extra", "owner/repo?permission=write"} {
		if _, err := resolver.ResolveVariables(t.Context(), repository); err == nil {
			t.Errorf("ResolveVariables(%q) succeeded", repository)
		}
	}
	var unconfigured *AgentVariableResolver
	if _, err := unconfigured.ResolveVariables(t.Context(), "buildkite/buildkite-gha"); err == nil {
		t.Error("ResolveVariables() on a nil resolver succeeded")
	}
	if requests != 0 {
		t.Fatalf("network requests = %d, want 0", requests)
	}
}

func TestAgentVariableResolverRejectsUnsafeConfiguration(t *testing.T) {
	valid := AgentVariableResolverConfig{Endpoint: "https://agent.example/v3", JobID: testCacheJobID, JobToken: "job-token"}
	if _, err := NewAgentVariableResolver(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*AgentVariableResolverConfig){
		"missing endpoint":            func(c *AgentVariableResolverConfig) { c.Endpoint = "" },
		"endpoint credentials":        func(c *AgentVariableResolverConfig) { c.Endpoint = "https://user@agent.example/v3" },
		"endpoint plaintext hostname": func(c *AgentVariableResolverConfig) { c.Endpoint = "http://localhost/v3" },
		"invalid job ID":              func(c *AgentVariableResolverConfig) { c.JobID = "../other" },
		"missing job token":           func(c *AgentVariableResolverConfig) { c.JobToken = "" },
		"job token header split":      func(c *AgentVariableResolverConfig) { c.JobToken = "secret\r\nOther: value" },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewAgentVariableResolver(config); err == nil {
				t.Fatalf("NewAgentVariableResolver(%#v) succeeded", config)
			}
		})
	}
}

// TestAgentVariableResolverStatusErrors proves each backend status maps to the
// agreed client behavior: 404 is the ErrVariablesUnavailable sentinel so
// callers keep empty scopes, 400 carries the backend's message, and 429 and
// 503 carry a numeric Retry-After delay.
func TestAgentVariableResolverStatusErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		retryAfter string
		body       string
		want       string
	}{
		{"rejected", http.StatusBadRequest, "", "", "the variable resolution request was rejected; confirm that Buildkite's GitHub App can read the repository's variables"},
		{"rejected with backend policy message", http.StatusBadRequest, "", `{"message":"GitHub repository and organization variables exceed 262144 bytes; remove or shrink repository or organization variables"}`, "rejected: GitHub repository and organization variables exceed 262144 bytes"},
		{"rejected with permission message", http.StatusBadRequest, "", `{"message":"GitHub App cannot read repository variables; ensure Variables: read is approved"}`, "rejected: GitHub App cannot read repository variables; ensure Variables: read is approved"},
		{"rejected with malformed body", http.StatusBadRequest, "", `not json`, "rejected; confirm"},
		{"rejected with unsafe message", http.StatusBadRequest, "", "{\"message\":\"bad\\nrequest\"}", "rejected: bad request"},
		{"denied", http.StatusForbidden, "", "", "denied"},
		{"unavailable endpoint", http.StatusNotFound, "", "", "the Agent API does not offer GitHub variable resolution"},
		{"rate limited with delay", http.StatusTooManyRequests, "3600", "", "variable resolution requests are rate limited; retry after 3600 seconds"},
		{"rate limited unsafe header", http.StatusTooManyRequests, "soon", "", "variable resolution requests are rate limited"},
		{"unavailable with delay", http.StatusServiceUnavailable, "60", "", "the variable resolution service is temporarily unavailable; retry after 60 seconds"},
		{"unavailable", http.StatusServiceUnavailable, "", "", "the variable resolution service is temporarily unavailable"},
		{"unexpected status", http.StatusBadGateway, "", "", "HTTP 502"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			resolver, err := NewAgentVariableResolver(AgentVariableResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolver.ResolveVariables(t.Context(), "buildkite/buildkite-gha")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveVariables() error = %v, want %q", err, test.want)
			}
			if test.status == http.StatusTooManyRequests && strings.Contains(test.want, "retry after") && !strings.Contains(err.Error(), test.retryAfter) {
				t.Fatalf("ResolveVariables() error = %v, want Retry-After %s", err, test.retryAfter)
			}
			if unavailable := errors.Is(err, ErrVariablesUnavailable); unavailable != (test.status == http.StatusNotFound) {
				t.Fatalf("errors.Is(err, ErrVariablesUnavailable) = %v for HTTP %d", unavailable, test.status)
			}
		})
	}
}

// TestAgentVariableResolverRejectsUntrustedResponses proves the client
// enforces the backend's bounds itself and never trusts a malformed snapshot.
// No error message carries a variable value.
func TestAgentVariableResolverRejectsUntrustedResponses(t *testing.T) {
	const value = "value-that-must-not-leak"
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"malformed JSON", `{"repository_variables":`, "decode"},
		{"trailing JSON", `{"repository_variables":[],"organization_variables":[]}{}`, "trailing data"},
		{"missing repository scope", `{"organization_variables":[]}`, "omits repository_variables or organization_variables"},
		{"null repository scope", `{"repository_variables":null,"organization_variables":[]}`, "omits repository_variables or organization_variables"},
		{"missing organization scope", `{"repository_variables":[]}`, "omits repository_variables or organization_variables"},
		{"oversized", `{"repository_variables":[],"organization_variables":[]}` + strings.Repeat(" ", variableResolutionResponseLimit), "exceeds"},
		{"variable without value", `{"repository_variables":[{"name":"AWS_REGION"}],"organization_variables":[]}`, "invalid variable resolution response: a variable in the repository scope has no name or value"},
		{"variable without name", `{"repository_variables":[],"organization_variables":[{"value":"` + value + `"}]}`, "invalid variable resolution response: a variable in the organization scope has no name or value"},
		{"invalid repository variable name", `{"repository_variables":[{"name":"not a name","value":"` + value + `"}],"organization_variables":[]}`, "invalid variable resolution response: invalid repository variable name"},
		{"invalid organization variable name", `{"repository_variables":[],"organization_variables":[{"name":"1BAD","value":"` + value + `"}]}`, "invalid variable resolution response: invalid organization variable name"},
		{"case-colliding repository names", `{"repository_variables":[{"name":"Region","value":"` + value + `"},{"name":"REGION","value":"b"}],"organization_variables":[]}`, `invalid variable resolution response: repository variable "REGION" repeats "Region"; GitHub variable names are case-insensitive`},
		{"too many repository variables", `{"repository_variables":[` + repeatedVariables(repositoryVariableCountLimit+1, 1) + `],"organization_variables":[]}`, "invalid variable resolution response: 501 repository variables exceed GitHub's limit of 500"},
		{"too many organization variables", `{"repository_variables":[],"organization_variables":[` + repeatedVariables(organizationVariableCountLimit+1, 1) + `]}`, "invalid variable resolution response: 1001 organization variables exceed GitHub's limit of 1000"},
		{"oversized variable value", `{"repository_variables":[{"name":"BIG","value":"` + strings.Repeat("x", environmentVariableValueLimit+1) + `"}],"organization_variables":[]}`, `invalid variable resolution response: repository variable "BIG" exceeds GitHub's 49152-byte value limit`},
		// The 256 KiB budget spans both scopes: three maximal values per scope
		// pass alone but exceed it together.
		{"oversized combined scopes", `{"repository_variables":[` + repeatedVariables(3, environmentVariableValueLimit) + `],"organization_variables":[` + repeatedVariables(3, environmentVariableValueLimit) + `]}`, "repository and organization variables exceed 262144 bytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			resolver, err := NewAgentVariableResolver(AgentVariableResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolver.ResolveVariables(t.Context(), "buildkite/buildkite-gha")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveVariables() error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), value) {
				t.Fatalf("ResolveVariables() error = %v carries a variable value", err)
			}
		})
	}

	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			_, _ = io.WriteString(w, `{"repository_variables":[],"organization_variables":[]}`)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	resolver, err := NewAgentVariableResolver(AgentVariableResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveVariables(t.Context(), "buildkite/buildkite-gha"); err == nil || !strings.Contains(err.Error(), "HTTP 307") || redirected {
		t.Fatalf("redirect ResolveVariables() error/redirected = %v / %v", err, redirected)
	}
}

// TestAgentVariableResolverDecodesMaximalEscapedVariables proves the response
// budget holds the backend's largest permitted payload — 256 KiB of names and
// values across both scopes — even when JSON escaping expands every value
// byte to six.
func TestAgentVariableResolverDecodesMaximalEscapedVariables(t *testing.T) {
	// 500 repository and 12 organization variables of 512 bytes each is just
	// under the 256 KiB combined budget once names are counted.
	const size = 500
	variables := func(count int, prefix string) string {
		entries := make([]string, 0, count)
		for i := range count {
			entries = append(entries, `{"name":"`+prefix+strconv.Itoa(i)+`","value":"`+strings.Repeat(`\u0001`, size)+`"}`)
		}
		return strings.Join(entries, ",")
	}
	body := `{"repository_variables":[` + variables(repositoryVariableCountLimit, "R") + `],"organization_variables":[` + variables(12, "O") + `]}`
	if len(body) <= 6*size*(repositoryVariableCountLimit+12) {
		t.Fatalf("test body is %d bytes; want every value byte escaped to six", len(body))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	resolver, err := NewAgentVariableResolver(AgentVariableResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := resolver.ResolveVariables(t.Context(), "buildkite/buildkite-gha")
	if err != nil {
		t.Fatalf("ResolveVariables() = %v", err)
	}
	if len(snapshot.Repository) != repositoryVariableCountLimit || len(snapshot.Organization) != 12 || snapshot.Repository["R0"] != strings.Repeat("\x01", size) {
		t.Fatalf("variables = %d repository, %d organization entries", len(snapshot.Repository), len(snapshot.Organization))
	}
}
