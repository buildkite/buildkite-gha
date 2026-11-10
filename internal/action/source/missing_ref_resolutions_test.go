package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMissingRefResolutionsKeepSourceContextsSeparate(t *testing.T) {
	var requests atomic.Int64
	var available atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if available.Load() {
			_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
			return
		}
		http.NotFound(w, r)
	})
	resolver := newMissingRefTestResolver(t, handler)
	ctx := WithMissingRefResolutions(t.Context())
	base, _ := Parse("Public/Action/one@Next")
	_, err := resolver.Resolve(ctx, base)
	requireMissingRefError(t, err)
	if got := requests.Load(); got != 3 {
		t.Fatalf("first lookup requests = %d, want tag, branch, and commit probes", got)
	}
	for _, test := range []struct {
		name             string
		raw              string
		repositoryRoot   bool
		authorizationRef string
		wantRequests     int64
	}{
		{name: "canonical repository", raw: "public/action/one@Next"},
		{name: "different path", raw: "public/action/two@Next", wantRequests: 3},
		{name: "case-sensitive ref", raw: "public/action/one@next", wantRequests: 3},
		{name: "repository source", raw: "public/action/one@Next", repositoryRoot: true, wantRequests: 3},
		{name: "original authority", raw: "public/action/one@Next", repositoryRoot: true, authorizationRef: "allowed", wantRequests: 3},
		{name: "different repository", raw: "public/another/one@Next", wantRequests: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			ref, _ := Parse(test.raw)
			ref.RepositoryRoot, ref.authorizationRef = test.repositoryRoot, test.authorizationRef
			before := requests.Load()
			_, err := resolver.Resolve(ctx, ref)
			requireMissingRefError(t, err)
			if got := requests.Load() - before; got != test.wantRequests {
				t.Errorf("API requests = %d, want %d", got, test.wantRequests)
			}
		})
	}

	available.Store(true)
	before := requests.Load()
	other := newMissingRefTestResolver(t, handler)
	for _, test := range []struct {
		name     string
		resolver *Resolver
		ctx      context.Context
	}{
		{name: "different resolver", resolver: other, ctx: ctx},
		{name: "fresh child scope", resolver: resolver, ctx: WithMissingRefResolutions(ctx)},
		{name: "unscoped resolution", resolver: resolver, ctx: t.Context()},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := test.resolver.Resolve(test.ctx, base)
			if err != nil || resolved.Commit != testSHA {
				t.Fatalf("Resolve() = %#v, %v, want the new ref", resolved, err)
			}
		})
	}
	if got := requests.Load() - before; got != 3 {
		t.Errorf("fresh context requests = %d, want 3", got)
	}
	_, err = resolver.Resolve(ctx, base)
	requireMissingRefError(t, err)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := resolver.Resolve(canceled, base); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled cache hit = %v, want context cancellation", err)
	}
	if got := requests.Load() - before; got != 3 {
		t.Errorf("cached or canceled resolution made an API request")
	}
}

func TestMissingRefResolutionsRetryIncompleteLookups(t *testing.T) {
	for _, test := range []struct {
		name, failurePath string
		status            int
		annotated         bool
		wantFirstRequests int64
	}{
		{name: "tag unauthorized", failurePath: "/git/ref/tags/next", status: 401, wantFirstRequests: 3},
		{name: "tag forbidden", failurePath: "/git/ref/tags/next", status: 403, wantFirstRequests: 3},
		{name: "branch unauthorized", failurePath: "/git/ref/heads/next", status: 401, wantFirstRequests: 3},
		{name: "branch forbidden", failurePath: "/git/ref/heads/next", status: 403, wantFirstRequests: 3},
		{name: "commit unauthorized", failurePath: "/commits/next", status: 401, wantFirstRequests: 3},
		{name: "commit forbidden", failurePath: "/commits/next", status: 403, wantFirstRequests: 3},
		{name: "server failure", failurePath: "/git/ref/tags/next", status: 500, wantFirstRequests: 1},
		{name: "rate limit", failurePath: "/git/ref/tags/next", status: 429, wantFirstRequests: 1},
		{name: "malformed response", failurePath: "/git/ref/tags/next", status: 200, wantFirstRequests: 1},
		{name: "missing annotated object", failurePath: "/git/tags/" + testSHA, status: 404, annotated: true, wantFirstRequests: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int64
			var available atomic.Bool
			resolver := newMissingRefTestResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if test.annotated && strings.Contains(r.URL.Path, "/git/ref/tags/") {
					_, _ = fmt.Fprintf(w, `{"object":{"type":"tag","sha":%q}}`, testSHA)
					return
				}
				if available.Load() {
					_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
					return
				}
				if strings.HasSuffix(r.URL.Path, test.failurePath) {
					if test.status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "1")
					}
					w.WriteHeader(test.status)
					_, _ = fmt.Fprint(w, "unavailable")
					return
				}
				http.NotFound(w, r)
			}))
			now := time.Unix(1_000, 0)
			resolver.cfg.now = func() time.Time { return now }
			ctx := WithMissingRefResolutions(t.Context())
			ref, _ := Parse("public/action@next")
			if _, err := resolver.Resolve(ctx, ref); err == nil {
				t.Fatal("first lookup succeeded, want a retryable failure")
			}
			if got := requests.Load(); got != test.wantFirstRequests {
				t.Fatalf("first lookup requests = %d, want %d", got, test.wantFirstRequests)
			}
			available.Store(true)
			now = now.Add(time.Second)
			resolved, err := resolver.Resolve(ctx, ref)
			if err != nil || resolved.Commit != testSHA {
				t.Fatalf("retry = %#v, %v, want the available ref", resolved, err)
			}
		})
	}
}

func TestMissingRefResolutionsRetryVisibilityDenials(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int64
			var public atomic.Bool
			resolver := newMissingRefTestResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("API request omitted the configured token")
				}
				if r.URL.Path == "/repos/public/action" {
					if !public.Load() {
						w.WriteHeader(status)
						_, _ = fmt.Fprint(w, `{"private":true,"visibility":"private"}`)
						return
					}
					_, _ = fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
			}), WithGitHubAPITokenProvider(func(context.Context) (string, error) { return "fixture-token", nil }))
			ctx := WithMissingRefResolutions(WithPublicRepositoryChecks(t.Context()))
			ref, _ := Parse("public/action@next")
			_, err := resolver.Resolve(ctx, ref)
			requireMissingRefError(t, err)
			public.Store(true)
			if status == http.StatusNotFound {
				_, err = resolver.Resolve(ctx, ref)
				requireMissingRefError(t, err)
				if got := requests.Load(); got != 1 {
					t.Errorf("cached repository absence requests = %d, want 1", got)
				}
				ctx = WithMissingRefResolutions(WithPublicRepositoryChecks(ctx))
			}
			resolved, err := resolver.Resolve(ctx, ref)
			if err != nil || resolved.Commit != testSHA {
				t.Fatalf("public repository retry = %#v, %v", resolved, err)
			}
			if got := requests.Load(); got != 3 {
				t.Errorf("API requests = %d, want the failed check, successful check, and tag lookup", got)
			}
		})
	}
}

func TestMissingRefResolutionsRetryNetworkFailures(t *testing.T) {
	resolver := newMissingRefTestResolver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
	}))
	var failed atomic.Bool
	failure := errors.New("temporary connection failure")
	resolver.client.Transport = missingRefRoundTripper(func(request *http.Request) (*http.Response, error) {
		if !failed.Swap(true) {
			return nil, failure
		}
		return http.DefaultTransport.RoundTrip(request)
	})
	ctx := WithMissingRefResolutions(t.Context())
	ref, _ := Parse("public/action@next")
	if _, err := resolver.Resolve(ctx, ref); !errors.Is(err, failure) {
		t.Fatalf("first resolution error = %v, want transport failure", err)
	}
	resolved, err := resolver.Resolve(ctx, ref)
	if err != nil || resolved.Commit != testSHA {
		t.Fatalf("network retry = %#v, %v", resolved, err)
	}
}

func TestMissingRefResolutionsWaiters(t *testing.T) {
	for _, cancelLeader := range []bool{false, true} {
		name := "missing leader"
		if cancelLeader {
			name = "canceled leader"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int64
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			resolver := newMissingRefTestResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				http.NotFound(w, r)
			}))
			ctx := WithMissingRefResolutions(t.Context())
			ref, _ := Parse("public/action@next")
			leaderCtx, stopLeader := context.WithCancel(ctx)
			defer stopLeader()
			leader := make(chan error, 1)
			go func() { _, err := resolver.Resolve(leaderCtx, ref); leader <- err }()
			<-started
			const waiters = 4
			live := make(chan error, waiters)
			for range waiters {
				waitCtx := newPublicCheckWaitContext(ctx)
				go func() { _, err := resolver.Resolve(waitCtx, ref); live <- err }()
				<-waitCtx.waiting
			}
			cancelCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			waitCtx := newPublicCheckWaitContext(cancelCtx)
			canceled := make(chan error, 1)
			go func() { _, err := resolver.Resolve(waitCtx, ref); canceled <- err }()
			<-waitCtx.waiting
			cancel()
			if err := <-canceled; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled waiter error = %v", err)
			}
			wantRequests := int64(3)
			if cancelLeader {
				stopLeader()
				wantRequests++
				if err := <-leader; !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled leader error = %v", err)
				}
			} else {
				unblock()
				requireMissingRefError(t, <-leader)
			}
			for range waiters {
				requireMissingRefError(t, <-live)
			}
			_, err := resolver.Resolve(ctx, ref)
			requireMissingRefError(t, err)
			if got := requests.Load(); got != wantRequests {
				t.Errorf("concurrent API requests = %d, want %d", got, wantRequests)
			}
		})
	}
}

func TestMissingRefResolutionsPreserveGitFallbackAndRetries(t *testing.T) {
	git, work, remote, commit := configureGitRepositorySource(t, map[string]string{
		".github/workflows/ci.yml": "on: workflow_call\njobs: {}\n",
	})
	var requests atomic.Int64
	resolver := newMissingRefTestResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}), withGitFixtureSource(git, remote))
	ctx := WithMissingRefResolutions(t.Context())
	ref, _ := Parse("o/r/.github/workflows/ci.yml@main")
	_, err := resolver.Resolve(ctx, ref)
	requireMissingRefError(t, err)
	ref.RepositoryRoot = true
	resolved, err := resolver.Resolve(ctx, ref)
	if err != nil || resolved.Commit != commit {
		t.Fatalf("repository fallback after action miss = %#v, %v", resolved, err)
	}
	ref, _ = Parse("o/r/.github/workflows/ci.yml@upcoming")
	ref.RepositoryRoot = true
	_, err = resolver.Resolve(ctx, ref)
	requireMissingRefError(t, err)
	runSourceGit(t, git, work, "push", "--quiet", "origin", "HEAD:refs/heads/upcoming")
	resolved, err = resolver.Resolve(ctx, ref)
	if err != nil || resolved.Commit != commit {
		t.Fatalf("Git fallback retry = %#v, %v", resolved, err)
	}
	if got := requests.Load(); got != 12 {
		t.Errorf("API requests = %d, want four complete lookups before permitted Git fallback", got)
	}
}

func newMissingRefTestResolver(t *testing.T, handler http.Handler, options ...Option) *Resolver {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	resolver, err := NewResolver(server.Client(), append(options, WithTestEndpoints(server.URL))...)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func requireMissingRefError(t *testing.T, err error) {
	t.Helper()
	var notPublic *NotPublicError
	if !errors.As(err, &notPublic) || err.Error() != (&NotPublicError{}).Error() {
		t.Fatalf("resolution error = %v, want the unchanged missing-or-private diagnostic", err)
	}
}

type missingRefRoundTripper func(*http.Request) (*http.Response, error)

func (f missingRefRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
