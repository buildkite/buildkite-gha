//go:build linux && amd64

package runtime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/agentapi"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestRuntimePublicAction(t *testing.T) {
	fixtureBytes, err := os.ReadFile("testdata/github_action_source_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request  json.RawMessage
		Response json.RawMessage
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	var request actionSourceRequest
	var expectedRequest map[string]any
	if err := json.Unmarshal(fixture.Request, &request); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.Request, &expectedRequest); err != nil {
		t.Fatal(err)
	}
	selector := request.Selectors[0]
	for _, tc := range []struct {
		name, metadata, script, reject, retryAfter, errorBody string
		status, calls, downloads                              int
		changeResponse                                        func(string) string
	}{
		{name: "success", calls: 1, downloads: 2}, // Preparation and existing runtime verification.
		{name: "additive fields", calls: 1, downloads: 2, changeResponse: func(body string) string {
			for _, field := range []string{`"version":`, `"selector":`, `"ref":`, `"format":`} {
				body = strings.Replace(body, field, `"future":{"enabled":true},`+field, 1)
			}
			return body
		}},
		{name: "action failure skips Bash", script: "console.log('action:vivid-73'); process.exit(7)", reject: "exit status 7", calls: 1, downloads: 2},
		{name: "upgrade required", status: 400, retryAfter: "0", errorBody: `{"message":"This text may change","code":"client_upgrade_required"}`,
			reject: "action source resolver returned HTTP 400: upgrade buildkite-gha to a supported version and retry the job", calls: 1},
		{name: "upgrade code on wrong status", status: 404, errorBody: `{"code":"client_upgrade_required"}`,
			reject: "action source resolver returned HTTP 404", calls: 1},
		{name: "unknown error code", status: 400, errorBody: `{"code":"invalid_selector"}`,
			reject: "action source resolver returned HTTP 400", calls: 1},
		{name: "oversized upgrade error", status: 400, errorBody: `{"code":"client_upgrade_required"}` + strings.Repeat(" ", 16<<10),
			reject: "action source resolver returned HTTP 400", calls: 1},
		{name: "no grant", status: 404, reject: "HTTP 404", calls: 1},
		{name: "policy denied", status: 403, reject: "HTTP 403", calls: 1},
		{name: "rate limit within budget", status: 429, retryAfter: "0", calls: 2, downloads: 2},
		{name: "retry delay exceeds budget", status: 503, retryAfter: "120", reject: "HTTP 503", calls: 1},
		{name: "malformed known field", reject: "invalid version 1 action source response", calls: 1,
			changeResponse: func(body string) string { return strings.Replace(body, `"path": ""`, `"path": []`, 1) }},
		{name: "response version omitted", reject: "invalid version 1 action source response", calls: 1,
			changeResponse: func(body string) string { return strings.Replace(body, `"version": 1,`, "", 1) }},
		{name: "trailing response document", reject: "invalid version 1 action source response", calls: 1,
			changeResponse: func(body string) string { return body + `{}` }},
		{name: "response loses exact selector", reject: "does not match the requested selector", calls: 1,
			changeResponse: func(body string) string { return strings.Replace(body, "Actions/", "actions/", 1) }},
		{name: "archive descriptor changes host", reject: "exact GitHub codeload", calls: 1,
			changeResponse: func(body string) string {
				return strings.Replace(body, "codeload.github.com", "archive.example.com", 1)
			}},
		{name: "metadata asks for token", metadata: "name: test\ninputs:\n  auth:\n    default: ${{ github.token }}\nruns:\n  using: node24\n  main: main.js\n", reject: "outside the admitted job envelope", calls: 1, downloads: 1},
		{name: "metadata has a post hook", metadata: "name: test\nruns:\n  using: node24\n  main: main.js\n  post: main.js\n", reject: "Node 24 action with main only", calls: 1, downloads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, "workflow.yml", "name: public action fixture\n")
			job := runtimePlan(t, workspace, "workflow.yml", []runtimeTestStep{
				{ID: "first", Kind: "run", Shell: "bash", Command: "echo before > order"},
				{ID: "greet", Kind: "uses", Uses: selector.Repository + "@" + selector.Ref, With: map[string]string{"message": "vivid-73"}},
				{ID: "last", Kind: "run", Shell: "bash", Command: "echo after >> order"},
			})
			job.ActionResolution = plan.RuntimePublicActionsV1
			job.Event.PayloadSource, job.Event.Repository, job.Event.SHA, job.Event.Ref = "buildkite-webhook", "example/workflows", strings.Repeat("b", 40), "refs/heads/main"
			job.RequiredCapabilities = []string{"network"}
			encoded, err := plan.Encode(job)
			if err != nil {
				t.Fatal(err)
			}
			job, err = plan.Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				defer func() { _ = req.Body.Close() }()
				var got map[string]any
				if ua := req.UserAgent(); ua != "buildkite-gha/1.2.3-test" {
					t.Errorf("User-Agent = %q, want executable version", ua)
				}
				if req.Method != http.MethodPost || req.URL.Path != "/v3/jobs/"+testCacheJobID+"/github-actions/action-sources" || req.Header.Get("Authorization") != "Token job-token" ||
					json.NewDecoder(req.Body).Decode(&got) != nil || !reflect.DeepEqual(got, expectedRequest) {
					t.Error("request does not match the shared version 1 fixture and job boundary")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if requests.Add(1) == 1 && tc.status != 0 {
					w.Header().Set("Retry-After", tc.retryAfter)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.errorBody))
					return
				}
				body := string(fixture.Response)
				if tc.changeResponse != nil {
					body = tc.changeResponse(body)
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			resolver, err := NewAgentActionSources(agentapi.Config{Endpoint: server.URL + "/v3", JobID: testCacheJobID, JobToken: "job-token", ClientVersion: "1.2.3-test"}, request.PlanDigest)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			meta := tc.metadata
			if meta == "" {
				meta = "name: test\ninputs:\n  message:\n    description: message\nruns:\n  using: node24\n  main: main.js\n"
			}
			writeFixtureFile(t, root, "action.yml", meta)
			script := tc.script
			if script == "" {
				script = `const fs = require('node:fs');
if (fs.readFileSync('order', 'utf8') !== 'before\n' || process.env.BUILDKITE_AGENT_ACCESS_TOKEN) process.exit(8);
fs.appendFileSync('order', 'action:' + process.env.INPUT_MESSAGE + '\n');`
			}
			writeFixtureFile(t, root, "main.js", script)
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: root, ActionRoot: root, SourceDigest: digestTree(t, root)}}
			var logs bytes.Buffer
			runner := Runner{Actions: materializer, ActionSources: resolver, Stdout: &logs, Stderr: &logs}
			if tc.downloads == 2 {
				runner.Node24 = requireNode24(t)
			}
			result, err := runner.RunJob(t.Context(), job, workspace)
			if int(requests.Load()) != tc.calls || materializer.calls != tc.downloads {
				t.Fatalf("resolver calls=%d, materializations=%d, err=%v", requests.Load(), materializer.calls, err)
			}
			if tc.reject != "" {
				if err == nil || !strings.Contains(err.Error(), tc.reject) || result.Conclusion != "failure" {
					t.Fatalf("want failure %q: %#v, %v\n%s", tc.reject, result, err, &logs)
				}
				if tc.status != 0 && !strings.HasSuffix(err.Error(), tc.reject) {
					t.Fatalf("want only the expected status guidance, got %v", err)
				}
				order, readErr := os.ReadFile(workspace + "/order")
				if tc.downloads < 2 && !os.IsNotExist(readErr) || tc.downloads == 2 && string(order) != "before\n" {
					t.Fatalf("unexpected steps ran: %q, %v", order, readErr)
				}
				return
			}
			order, readErr := os.ReadFile(workspace + "/order")
			if err != nil || result.Conclusion != "success" || readErr != nil || string(order) != "before\naction:vivid-73\nafter\n" {
				t.Fatalf("want ordered execution, got %q, %v; result=%s, %v\n%s", order, readErr, result.Conclusion, err, &logs)
			}
		})
	}
}
