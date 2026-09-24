package compiler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/source"
)

func TestCompileActionRequestBudgetAfterRateLimit(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		name := "anonymous"
		if authenticated {
			name = "app-token"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newActionRequestBudgetFixture(t, authenticated)
			cached, err := source.Parse("public/cached@v1")
			if err != nil {
				t.Fatal(err)
			}
			_, materialized, err := fixture.actions.Fetch(t.Context(), cached)
			if err != nil {
				t.Fatalf("warm action cache: %v", err)
			}
			materialized.Release()
			fixture.rateLimited.Store(true)
			before := fixture.apiRequests.Load()

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
      - uses: public/limited@v1
      - uses: public/another@v2
  independent:
    runs-on: ubuntu-latest
    steps:
      - uses: public/cached@v1
      - uses: public/pinned@%s
`, strings.Join(shards, ", "), fixture.commit)
			bundle, err := compileActionRequestBudgetWorkflow(t, t.Context(), workflow, fixture.actions)
			var rateLimit *source.RateLimitError
			if !errors.As(err, &rateLimit) {
				t.Fatalf("compile error = %v, want rate limit", err)
			}
			failed, passed := 0, 0
			for _, action := range bundle.Processing.Actions {
				if action.Passed {
					passed++
				} else {
					failed++
				}
			}
			if failed != 2*rows || passed != 2 {
				t.Errorf("action evaluations = %d failed, %d passed; want %d failed, 2 passed", failed, passed, 2*rows)
			}
			if len(bundle.Plans) != 1 || bundle.Plans[0].Job.Workflow.LogicalJobID != "independent" {
				t.Errorf("plans = %d, want the independent cached/pinned job", len(bundle.Plans))
			}
			requests := fixture.apiRequests.Load() - before
			t.Logf("%d matrix rows, %d failed action evaluations: %d GitHub API requests after rate limiting", rows, failed, requests)
			if requests != 1 {
				t.Errorf("GitHub API requests after rate limiting = %d, want 1", requests)
			}
		})
	}
}

func TestCompileActionRequestBudgetSharesPublicVisibility(t *testing.T) {
	fixture := newActionRequestBudgetFixture(t, true)
	const aliases = 8
	var workflow strings.Builder
	workflow.WriteString(`on: push
jobs:
  actions:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        shard: [0, 1, 2, 3, 4]
    steps:
`)
	for i := range aliases {
		fmt.Fprintf(&workflow, "      - uses: public/action@v%d\n", i+1)
	}
	bundle, err := compileActionRequestBudgetWorkflow(t, t.Context(), workflow.String(), fixture.actions)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 5 {
		t.Fatalf("plans = %d, want 5", len(bundle.Plans))
	}
	requests := fixture.apiRequests.Load()
	t.Logf("%d mutable refs across 5 matrix rows: %d GitHub API requests", aliases, requests)
	if requests != aliases+1 {
		t.Errorf("GitHub API requests = %d, want %d refs plus one repository visibility check", requests, aliases)
	}
}

func TestCompileActionRequestBudgetRechecksVisibilityForNextCompilation(t *testing.T) {
	fixture := newActionRequestBudgetFixture(t, true)
	ctx := source.WithPublicRepositoryChecks(t.Context())
	workflow := `on: push
jobs:
  action:
    runs-on: ubuntu-latest
    steps:
      - uses: public/action@v1
`
	if _, err := compileActionRequestBudgetWorkflow(t, ctx, workflow, fixture.actions); err != nil {
		t.Fatal(err)
	}
	fixture.privateRepository.Store(true)
	before := fixture.apiRequests.Load()
	bundle, err := compileActionRequestBudgetWorkflow(t, ctx, strings.ReplaceAll(workflow, "@v1", "@v2"), fixture.actions)
	var notPublic *source.NotPublicError
	if !errors.As(err, &notPublic) {
		t.Fatalf("second compilation error = %v, want repository visibility refusal", err)
	}
	if len(bundle.Plans) != 0 {
		t.Errorf("plans = %d, want no plans using the now-private repository", len(bundle.Plans))
	}
	if requests := fixture.apiRequests.Load() - before; requests != 1 {
		t.Errorf("second compilation API requests = %d, want one visibility check and no ref lookup", requests)
	}
}

type actionRequestBudgetFixture struct {
	actions           ActionSource
	commit            string
	apiRequests       atomic.Int64
	rateLimited       atomic.Bool
	privateRepository atomic.Bool
}

func newActionRequestBudgetFixture(t *testing.T, authenticated bool) *actionRequestBudgetFixture {
	t.Helper()
	fixture := &actionRequestBudgetFixture{commit: strings.Repeat("a", 40)}
	archive := actionRequestBudgetArchive(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/") {
			if !strings.Contains(r.URL.Path, "/tar.gz/") {
				t.Errorf("unexpected request path %q", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Authorization") != "" {
				t.Error("action archive request included credentials")
			}
			_, _ = w.Write(archive)
			return
		}
		fixture.apiRequests.Add(1)
		if got := r.Header.Get("Authorization") != ""; got != authenticated {
			t.Errorf("request authenticated = %v, want %v", got, authenticated)
		}
		if fixture.rateLimited.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		if strings.Contains(r.URL.Path, "/git/ref/tags/") {
			_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, fixture.commit)
			return
		}
		if len(strings.Split(strings.Trim(r.URL.Path, "/"), "/")) == 3 {
			if fixture.privateRepository.Load() {
				_, _ = fmt.Fprint(w, `{"private":true,"visibility":"private"}`)
			} else {
				_, _ = fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
			}
			return
		}
		t.Errorf("unexpected API request path %q", r.URL.Path)
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
	fixture.actions = MemoizeActionSource(PublicActionSource{Resolver: resolver, Store: store})
	return fixture
}

func actionRequestBudgetArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	contents := "name: public fixture\nruns:\n  using: composite\n  steps:\n    - run: echo fixture\n      shell: bash\n"
	if err := writer.WriteHeader(&tar.Header{Name: "repository/action.yml", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func compileActionRequestBudgetWorkflow(t *testing.T, ctx context.Context, workflow string, actions ActionSource) (Bundle, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".github", "workflows", "request-budget.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return CompileBundlePlansContext(ctx, path, []byte(workflow), pushEvent(t), "0.0.0-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   actions,
	})
}
