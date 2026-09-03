package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestAgentEnvironmentResolverResolvesEnvironmentBatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs/"+testCacheJobID+"/github-actions/environments" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "buildkite-gha/1.2.3" {
			t.Errorf("request = %s headers %#v", r.Method, r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"repo_url":"https://github.com/buildkite/buildkite-gha","environment_names":["production","staging"],"include_variables":true}` {
			t.Errorf("request body = %s", body)
		}
		_, _ = io.WriteString(w, `{"environments":[`+
			`{"name":"production","required_reviewers":true,"prevent_self_review":true,"wait_timer_minutes":15,"branch_policy":false,"unsupported_rules":["custom deployment protection rule \"datadog\""],"secret_names":["DEPLOY_KEY","API_KEY"],"variables":[{"name":"AWS_REGION","value":"eu-west-1"},{"name":"cert_pem","value":"-----BEGIN-----\nline\n"},{"name":"EMPTY","value":""}]},`+
			`{"name":"staging","required_reviewers":false,"prevent_self_review":false,"wait_timer_minutes":0,"branch_policy":false,"unsupported_rules":[],"secret_names":[],"variables":[]}]}`)
	}))
	defer server.Close()
	resolver, err := NewAgentEnvironmentResolver(AgentEnvironmentResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := resolver.ResolveEnvironments(t.Context(), "buildkite/buildkite-gha", []string{"production", "staging"})
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvironmentSnapshot{
		{
			RequiredReviewers: true,
			PreventSelfReview: true,
			WaitTimerMinutes:  15,
			BranchPolicy:      false,
			UnsupportedRules:  []string{`custom deployment protection rule "datadog"`},
			SecretNames:       []string{"API_KEY", "DEPLOY_KEY"},
			Variables:         map[string]string{"AWS_REGION": "eu-west-1", "cert_pem": "-----BEGIN-----\nline\n", "EMPTY": ""},
		},
		{Variables: map[string]string{}},
	}
	if !reflect.DeepEqual(snapshots, want) {
		t.Fatalf("ResolveEnvironments() = %#v, want %#v", snapshots, want)
	}
}

func TestAgentEnvironmentResolverRejectsInvalidInputBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	resolver, err := NewAgentEnvironmentResolver(AgentEnvironmentResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range []string{"", "other", "../other/repo", "owner/..", "owner/repo/extra", "owner/repo?permission=write"} {
		if _, err := resolver.ResolveEnvironments(t.Context(), repository, []string{"production"}); err == nil {
			t.Errorf("ResolveEnvironments(%q) succeeded", repository)
		}
	}
	for name, names := range map[string][]string{
		"no names":                {},
		"empty name":              {""},
		"duplicate names":         {"production", "production"},
		"case-insensitive repeat": {"Production", "production"},
	} {
		if _, err := resolver.ResolveEnvironments(t.Context(), "buildkite/buildkite-gha", names); err == nil {
			t.Errorf("ResolveEnvironments with %s succeeded", name)
		}
	}
	if requests != 0 {
		t.Fatalf("network requests = %d, want 0", requests)
	}
}

func TestAgentEnvironmentResolverRejectsUnsafeConfiguration(t *testing.T) {
	valid := AgentEnvironmentResolverConfig{Endpoint: "https://agent.example/v3", JobID: testCacheJobID, JobToken: "job-token"}
	if _, err := NewAgentEnvironmentResolver(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*AgentEnvironmentResolverConfig){
		"missing endpoint":              func(c *AgentEnvironmentResolverConfig) { c.Endpoint = "" },
		"endpoint credentials":          func(c *AgentEnvironmentResolverConfig) { c.Endpoint = "https://user@agent.example/v3" },
		"endpoint plaintext hostname":   func(c *AgentEnvironmentResolverConfig) { c.Endpoint = "http://localhost/v3" },
		"endpoint plaintext private IP": func(c *AgentEnvironmentResolverConfig) { c.Endpoint = "http://10.0.0.1/v3" },
		"invalid job ID":                func(c *AgentEnvironmentResolverConfig) { c.JobID = "../other" },
		"missing job token":             func(c *AgentEnvironmentResolverConfig) { c.JobToken = "" },
		"job token header split":        func(c *AgentEnvironmentResolverConfig) { c.JobToken = "secret\r\nOther: value" },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewAgentEnvironmentResolver(config); err == nil {
				t.Fatalf("NewAgentEnvironmentResolver(%#v) succeeded", config)
			}
		})
	}
}

func TestAgentEnvironmentResolverStatusErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		retryAfter string
		body       string
		want       string
	}{
		{"rejected", http.StatusBadRequest, "", "", "rejected"},
		{"rejected with backend policy message", http.StatusBadRequest, "", `{"message":"environment_names cannot contain more than 20 names; split the upload or remove environments"}`, "rejected: environment_names cannot contain more than 20 names; split the upload or remove environments"},
		{"rejected with malformed body", http.StatusBadRequest, "", `not json`, "rejected; confirm the environment exists"},
		{"rejected with unsafe message", http.StatusBadRequest, "", "{\"message\":\"bad\\nrequest\"}", "rejected: bad request"},
		{"denied", http.StatusForbidden, "", "", "denied"},
		{"unavailable endpoint", http.StatusNotFound, "", "", "the Agent API does not offer GitHub environment resolution"},
		{"rate limited with delay", http.StatusTooManyRequests, "3600", "", "rate limited; retry after 3600 seconds"},
		{"rate limited unsafe header", http.StatusTooManyRequests, "soon", "", "environment resolution requests are rate limited"},
		{"unavailable with delay", http.StatusServiceUnavailable, "60", "", "temporarily unavailable; retry after 60 seconds"},
		{"unavailable", http.StatusServiceUnavailable, "", "", "temporarily unavailable"},
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
			resolver, err := NewAgentEnvironmentResolver(AgentEnvironmentResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolver.ResolveEnvironments(t.Context(), "buildkite/buildkite-gha", []string{"production"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveEnvironments() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentEnvironmentResolverRejectsUntrustedResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"malformed JSON", `{"environments":`, "decode"},
		{"trailing JSON", `{"environments":[` + completeEnvironment("production") + `]}{}`, "trailing data"},
		{"missing environment", `{"environments":[]}`, "has 0 environments, want 1"},
		{"extra environment", `{"environments":[` + completeEnvironment("production") + `,` + completeEnvironment("staging") + `]}`, "has 2 environments, want 1"},
		{"wrong name", `{"environments":[` + completeEnvironment("staging") + `]}`, `names "staging" where "production" was requested`},
		{"negative wait timer", `{"environments":[` + environmentWith("production", `"wait_timer_minutes":-1`) + `]}`, "invalid wait timer"},
		{"blank protection rule", `{"environments":[` + environmentWith("production", `"unsupported_rules":[" "]`) + `]}`, "invalid protection rule"},
		{"invalid secret name", `{"environments":[` + environmentWith("production", `"secret_names":["not a name"]`) + `]}`, "invalid secret name"},
		{"oversized", `{"environments":[` + completeEnvironment("production") + `]}` + strings.Repeat(" ", environmentResolutionResponseLimit), "exceeds"},
		// Absent or null fields must not decode to fail-open zero values: a
		// missing required_reviewers would drop the approval gate and missing
		// secret_names would leave secrets unscoped.
		{"only name", `{"environments":[{"name":"production"}]}`, "omits"},
		{"missing name", `{"environments":[` + strings.Replace(completeEnvironment("production"), `"name":"production",`, "", 1) + `]}`, "omits the name"},
		{"missing required_reviewers", `{"environments":[` + environmentWithout("production", "required_reviewers") + `]}`, `omits required_reviewers`},
		{"null required_reviewers", `{"environments":[` + environmentWith("production", `"required_reviewers":null`) + `]}`, `omits required_reviewers`},
		{"missing prevent_self_review", `{"environments":[` + environmentWithout("production", "prevent_self_review") + `]}`, `omits prevent_self_review`},
		{"missing wait_timer_minutes", `{"environments":[` + environmentWithout("production", "wait_timer_minutes") + `]}`, `omits wait_timer_minutes`},
		{"missing branch_policy", `{"environments":[` + environmentWithout("production", "branch_policy") + `]}`, `omits branch_policy`},
		{"missing unsupported_rules", `{"environments":[` + environmentWithout("production", "unsupported_rules") + `]}`, `omits unsupported_rules`},
		{"missing secret_names", `{"environments":[` + environmentWithout("production", "secret_names") + `]}`, `omits secret_names`},
		{"null secret_names", `{"environments":[` + environmentWith("production", `"secret_names":null`) + `]}`, `omits secret_names`},
		// Variables were requested, so an absent list must not decode as an
		// environment without variables: vars.NAME would fail closed with a
		// misleading "not defined" diagnostic instead of a contract error.
		{"missing variables", `{"environments":[` + environmentWithout("production", "variables") + `]}`, `omits variables`},
		{"null variables", `{"environments":[` + environmentWith("production", `"variables":null`) + `]}`, `omits variables`},
		{"variable without value", `{"environments":[` + environmentWith("production", `"variables":[{"name":"AWS_REGION"}]`) + `]}`, `omits a variable name or value`},
		{"variable without name", `{"environments":[` + environmentWith("production", `"variables":[{"value":"eu-west-1"}]`) + `]}`, `omits a variable name or value`},
		{"invalid variable name", `{"environments":[` + environmentWith("production", `"variables":[{"name":"not a name","value":"x"}]`) + `]}`, `invalid variable name`},
		{"case-colliding variable names", `{"environments":[` + environmentWith("production", `"variables":[{"name":"Region","value":"a"},{"name":"REGION","value":"b"}]`) + `]}`, `repeats variable "Region" as "REGION"`},
		{"too many variables", `{"environments":[` + environmentWith("production", `"variables":[`+repeatedVariables(environmentVariableCountLimit+1, 1)+`]`) + `]}`, `contains 101 variables`},
		{"oversized variable value", `{"environments":[` + environmentWith("production", `"variables":[{"name":"BIG","value":"`+strings.Repeat("x", environmentVariableValueLimit+1)+`"}]`) + `]}`, `variable "BIG" exceeds`},
		{"oversized variables", `{"environments":[` + environmentWith("production", `"variables":[`+repeatedVariables(6, environmentVariableValueLimit)+`]`) + `]}`, `variables exceed 262144 bytes`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			resolver, err := NewAgentEnvironmentResolver(AgentEnvironmentResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolver.ResolveEnvironments(t.Context(), "buildkite/buildkite-gha", []string{"production"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveEnvironments() error = %v, want %q", err, test.want)
			}
		})
	}

	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			_, _ = io.WriteString(w, `{"environments":[`+completeEnvironment("production")+`]}`)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	resolver, err := NewAgentEnvironmentResolver(AgentEnvironmentResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveEnvironments(t.Context(), "buildkite/buildkite-gha", []string{"production"}); err == nil || !strings.Contains(err.Error(), "HTTP 307") || redirected {
		t.Fatalf("redirect ResolveEnvironments() error/redirected = %v / %v", err, redirected)
	}
}

// TestAgentEnvironmentResolverScalesResponseLimitWithBatchSize proves the
// response budget grows with the request so a full batch of environments,
// each carrying many secret names, still decodes.
func TestAgentEnvironmentResolverScalesResponseLimitWithBatchSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"environments":[`+completeEnvironment("production")+`,`+completeEnvironment("staging")+`]}`+strings.Repeat(" ", environmentResolutionResponseLimit))
	}))
	defer server.Close()
	resolver, err := NewAgentEnvironmentResolver(AgentEnvironmentResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveEnvironments(t.Context(), "buildkite/buildkite-gha", []string{"production", "staging"}); err != nil {
		t.Fatalf("ResolveEnvironments() = %v", err)
	}
}

// TestAgentEnvironmentResolverDecodesMaximalEscapedVariables proves the
// response budget holds the backend's largest permitted variable payload even
// when JSON escaping expands every value byte to six.
func TestAgentEnvironmentResolverDecodesMaximalEscapedVariables(t *testing.T) {
	variables := make([]string, 0, environmentVariableCountLimit)
	for i := range environmentVariableCountLimit {
		variables = append(variables, `{"name":"V`+strconv.Itoa(i)+`","value":"`+strings.Repeat(`\u0001`, 2560)+`"}`)
	}
	body := `{"environments":[` + environmentWith("production", `"variables":[`+strings.Join(variables, ",")+`]`) + `]}`
	if len(body) <= 6*256000 {
		t.Fatalf("test body is %d bytes; want every value byte escaped to six", len(body))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	resolver, err := NewAgentEnvironmentResolver(AgentEnvironmentResolverConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := resolver.ResolveEnvironments(t.Context(), "buildkite/buildkite-gha", []string{"production"})
	if err != nil {
		t.Fatalf("ResolveEnvironments() = %v", err)
	}
	if len(snapshots[0].Variables) != environmentVariableCountLimit || snapshots[0].Variables["V0"] != strings.Repeat("\x01", 2560) {
		t.Fatalf("variables = %d entries", len(snapshots[0].Variables))
	}
}

// completeEnvironmentFields are the snapshot fields the backend contract
// requires on every environment object, with their default JSON values.
var completeEnvironmentFields = []string{
	`"required_reviewers":false`,
	`"prevent_self_review":false`,
	`"wait_timer_minutes":0`,
	`"branch_policy":false`,
	`"unsupported_rules":[]`,
	`"secret_names":[]`,
	`"variables":[]`,
}

// repeatedVariables returns count distinct variable objects whose values are
// size bytes each.
func repeatedVariables(count, size int) string {
	variables := make([]string, 0, count)
	for i := range count {
		variables = append(variables, `{"name":"V`+strconv.Itoa(i)+`","value":"`+strings.Repeat("x", size)+`"}`)
	}
	return strings.Join(variables, ",")
}

// completeEnvironment returns one contract-complete environment object.
func completeEnvironment(name string) string {
	return environmentWith(name)
}

// environmentWith returns a complete environment object with the given
// `"field":value` overrides replacing their defaults.
func environmentWith(name string, overrides ...string) string {
	fields := []string{`"name":"` + name + `"`}
	for _, field := range completeEnvironmentFields {
		key, _, _ := strings.Cut(field, ":")
		replaced := false
		for _, override := range overrides {
			if strings.HasPrefix(override, key+":") {
				fields = append(fields, override)
				replaced = true
			}
		}
		if !replaced {
			fields = append(fields, field)
		}
	}
	return "{" + strings.Join(fields, ",") + "}"
}

// environmentWithout returns a complete environment object missing one field.
func environmentWithout(name, omitted string) string {
	fields := []string{`"name":"` + name + `"`}
	for _, field := range completeEnvironmentFields {
		if !strings.HasPrefix(field, `"`+omitted+`":`) {
			fields = append(fields, field)
		}
	}
	return "{" + strings.Join(fields, ",") + "}"
}
