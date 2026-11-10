package compiler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/source"
)

func TestCompileMissingRefsResolveOnceAndRefreshForNextCompilation(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		name := "anonymous"
		if authenticated {
			name = "app-token"
		}
		t.Run(name, func(t *testing.T) {
			var available atomic.Bool
			var requests atomic.Int64
			commit := strings.Repeat("a", 40)
			archive := actionRequestBudgetArchive(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/repos/") {
					if !strings.Contains(r.URL.Path, "/tar.gz/") {
						t.Errorf("unexpected archive request %q", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					if r.Header.Get("Authorization") != "" {
						t.Error("archive request included credentials")
					}
					_, _ = w.Write(archive)
					return
				}
				requests.Add(1)
				if got := r.Header.Get("Authorization") != ""; got != authenticated {
					t.Errorf("request authenticated = %v, want %v", got, authenticated)
				}
				switch r.URL.Path {
				case "/repos/public/action":
					_, _ = fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
					return
				case "/repos/public/action/git/ref/tags/upcoming":
					if available.Load() {
						_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, commit)
						return
					}
				case "/repos/public/action/git/ref/heads/upcoming", "/repos/public/action/commits/upcoming":
				default:
					t.Errorf("unexpected API request %q", r.URL.Path)
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(server.Close)
			endpoint := source.WithTestEndpoints(server.URL, server.URL)
			options := []source.Option{endpoint}
			if authenticated {
				options = append(options, source.WithGitHubActionSourceTokenProvider("pipeline/repository", func(context.Context) (string, error) {
					return "dummy-app-token", nil
				}))
			}
			resolver, err := source.NewResolver(server.Client(), options...)
			if err != nil {
				t.Fatal(err)
			}
			store, err := source.NewStore(t.TempDir(), server.Client(), endpoint)
			if err != nil {
				t.Fatal(err)
			}
			actions := MemoizeActionSource(PublicActionSource{Resolver: resolver, Store: store})
			const rows = 50
			shards := make([]string, rows)
			for i := range shards {
				shards[i] = strconv.Itoa(i)
			}
			workflow := fmt.Sprintf(`on: push
jobs:
  affected:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        shard: [%s]
    steps:
      - uses: public/action@upcoming
  independent:
    runs-on: ubuntu-latest
    steps:
      - uses: public/action@%s
`, strings.Join(shards, ", "), commit)
			ctx := source.WithMissingRefResolutions(source.WithPublicRepositoryChecks(t.Context()))
			bundle, err := compileActionRequestBudgetWorkflow(t, ctx, workflow, actions)
			var missing *source.NotPublicError
			if !errors.As(err, &missing) {
				t.Fatalf("compile error = %v, want missing action", err)
			}
			failed, passed := 0, 0
			for _, action := range bundle.Processing.Actions {
				if action.Passed {
					passed++
				} else {
					failed++
				}
			}
			if failed != rows || passed != 1 {
				t.Errorf("action evaluations = %d failed, %d passed; want %d failed, 1 passed", failed, passed, rows)
			}
			if len(bundle.Plans) != 1 || bundle.Plans[0].Job.Workflow.LogicalJobID != "independent" {
				t.Errorf("plans = %d, want the independent pinned job", len(bundle.Plans))
			}
			wantRequests := int64(3)
			if authenticated {
				wantRequests++
			}
			t.Logf("%d matrix rows, %d failed action evaluations: %d GitHub API requests", rows, failed, requests.Load())
			if got := requests.Load(); got != wantRequests {
				t.Errorf("GitHub API requests = %d, want %d for one complete resolution", got, wantRequests)
			}

			available.Store(true)
			before := requests.Load()
			bundle, err = compileActionRequestBudgetWorkflow(t, ctx, workflow, actions)
			if err != nil {
				t.Fatalf("next compilation: %v", err)
			}
			if len(bundle.Plans) != rows+1 {
				t.Errorf("next compilation plans = %d, want %d", len(bundle.Plans), rows+1)
			}
			if got := requests.Load() - before; got != wantRequests-2 {
				t.Errorf("next compilation requests = %d, want %d for the now-existing tag", got, wantRequests-2)
			}
		})
	}
}
