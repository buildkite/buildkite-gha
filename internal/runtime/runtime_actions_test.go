//go:build linux && amd64

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/integration"
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

func TestRuntimePublicActionBatch(t *testing.T) {
	const first = "Example/alpha@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const second = "Example/bravo@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, tc := range []struct{ name, reject string }{
		{name: "reordered batch with repeated invocation"},
		{name: "duplicate response selector", reject: "identity does not match"},
		{name: "missing response selector", reject: "invalid version 2 action source response"},
		{name: "unsupported second source", reject: `require JavaScript metadata, got "composite"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			const checkoutID = "a-0000000000000002"
			const workflow = "name: batch fixture\n"
			orderFile := t.TempDir() + "/order"
			writeFixtureFile(t, workspace, "workflow.yml", workflow)
			job := runtimePlan(t, workspace, "workflow.yml", []runtimeTestStep{
				{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &plan.ActionSelector{Lock: checkoutID}},
				{ID: "shell", Kind: "run", Shell: "bash", Command: "echo shell >> " + shellTestQuote(orderFile)},
				{ID: "a1", Kind: "uses", Uses: first, With: map[string]string{"label": "one"}},
				{ID: "b", Kind: "uses", Uses: second},
				{ID: "a2", Kind: "uses", Uses: first, With: map[string]string{"label": "two"}},
			})
			if err := os.Remove(workspace + "/workflow.yml"); err != nil {
				t.Fatal(err)
			}
			job.ActionResolution = plan.RuntimePublicActionsV2
			job.Event.PayloadSource, job.Event.Repository, job.Event.SHA, job.Event.Ref = "buildkite-webhook", "example/workflows", strings.Repeat("c", 40), "refs/heads/main"
			job.RequiredCapabilities = []string{"network", "provider-token-read"}
			checkout := t.TempDir()
			writeFixtureFile(t, checkout, "action.yml", "name: checkout\nruns:\n  using: node24\n  main: main.js\n")
			writeFixtureFile(t, checkout, "main.js", "throw new Error('the native adapter must not run JavaScript')\n")
			checkoutDigest := digestTree(t, checkout)
			job.Actions = []plan.ActionLock{{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: integration.CheckoutV7Commit, SourceDigest: checkoutDigest}}
			job.Dependencies = []string{"gha-producer"}
			job.NeedSources = map[string][]plan.NeedSource{"producer": {{StepKey: "gha-producer", PlanDigest: "sha256:" + strings.Repeat("2", 64)}}}
			encoded, err := plan.Encode(job)
			if err != nil {
				t.Fatal(err)
			}
			job, err = plan.Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			job.Needs = map[string]plan.Need{"producer": {Result: "success"}}
			planDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				var body actionSourceRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Version != 2 || body.PlanDigest != planDigest || len(body.Selectors) != 2 {
					t.Errorf("unexpected batch: %#v, %v", body, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for i, uses := range []string{first, second} {
					sel := body.Selectors[i]
					if sel.Path == nil || *sel.Path != "" || sel.Repository+"@"+sel.Ref != uses {
						t.Errorf("unexpected selector: %#v", sel)
					}
				}
				slices.Reverse(body.Selectors)
				if tc.name == "duplicate response selector" {
					body.Selectors[1] = body.Selectors[0]
				}
				if tc.name == "missing response selector" {
					body.Selectors = body.Selectors[:1]
				}
				var rows []string
				for _, sel := range body.Selectors {
					repo := strings.ToLower(sel.Repository)
					rows = append(rows, fmt.Sprintf(`{"selector":{"repository":%q,"ref":%q,"path":""},"repository":%q,"repository_id":2,"owner_id":1,"commit":%q,"download":{"format":"tar.gz","url":%q}}`, sel.Repository, sel.Ref, repo, sel.Ref, "https://codeload.github.com/"+repo+"/tar.gz/"+sel.Ref))
				}
				_, _ = fmt.Fprintf(w, `{"version":2,"sources":[%s]}`, strings.Join(rows, ","))
			}))
			defer server.Close()
			resolver, err := NewAgentActionSources(agentapi.Config{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "job-token"}, planDigest)
			if err != nil {
				t.Fatal(err)
			}
			byRepository := map[string]source.Materialized{"checkout": {RepositoryRoot: checkout, ActionRoot: checkout, SourceDigest: checkoutDigest}}
			materializer := &fakeActionMaterializer{materialize: func(_ context.Context, resolved source.Resolved) (source.Materialized, error) {
				material, ok := byRepository[resolved.Reference.Repository]
				if !ok {
					return source.Materialized{}, fmt.Errorf("unexpected archive: %v", resolved)
				}
				return material, nil
			}}
			for _, repo := range []string{"alpha", "bravo"} {
				root := t.TempDir()
				meta := "name: fixture\nruns:\n  using: node24\n  main: main.js\n"
				if repo == "alpha" {
					meta = "name: alpha\ninputs:\n  label:\n    description: label\nruns:\n  using: node20\n  pre: pre.js\n  main: main.js\n  post: post.js\n  post-if: success()\n"
				} else if tc.name == "unsupported second source" {
					meta = "name: child\nruns:\n  using: composite\n  steps:\n    - uses: example/other@v1\n"
				}
				writeFixtureFile(t, root, "action.yml", meta)
				for _, phase := range []string{"pre", "main", "post"} {
					writeFixtureFile(t, root, phase+".js", fmt.Sprintf(`require('node:fs').appendFileSync(%q, %q + (process.env.INPUT_LABEL || '') + '\n')`, orderFile, repo+":"+phase+":"))
				}
				byRepository[repo] = source.Materialized{RepositoryRoot: root, ActionRoot: root, SourceDigest: digestTree(t, root)}
			}
			git := t.TempDir() + "/git"
			if err := os.WriteFile(git, []byte(`#!/bin/sh
set -eu
for arg in "$@"; do
  case "$arg" in
    init) mkdir -p .git; exit 0 ;;
    checkout)
      printf '%s\n' '`+job.Event.SHA+`' > .git/HEAD
      printf '%s' `+shellTestQuote(workflow)+` > workflow.yml
      echo checkout >> `+shellTestQuote(orderFile)+`
      exit 0 ;;
  esac
done
`), 0o700); err != nil {
				t.Fatal(err)
			}
			runner := Runner{Actions: materializer, ActionSources: resolver, Git: git}
			if tc.reject == "" {
				runner.Node24 = requireNode24(t)
			}
			result, err := runner.RunJob(t.Context(), job, workspace)
			if requests.Load() != 1 {
				t.Fatalf("requests=%d, err=%v", requests.Load(), err)
			}
			order, readErr := os.ReadFile(orderFile)
			if tc.reject != "" {
				if err == nil || !strings.Contains(err.Error(), tc.reject) || result.Conclusion != "failure" || !os.IsNotExist(readErr) {
					t.Fatalf("want pre-execution failure %q, got %v (%s), order=%q (%v)", tc.reject, err, result.Conclusion, order, readErr)
				}
				return
			}
			want := "alpha:pre:one\nalpha:pre:two\ncheckout\nshell\nalpha:main:one\nbravo:main:\nalpha:main:two\nalpha:post:two\nalpha:post:one\n"
			if err != nil || result.Conclusion != "success" || readErr != nil || string(order) != want {
				t.Fatalf("want ordered lifecycle %q, got %q (%v); result=%s, %v", want, order, readErr, result.Conclusion, err)
			}
		})
	}
}
