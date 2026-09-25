package source

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubAPIStatsObserveActualAuthenticationAndEndpoints(t *testing.T) {
	const token = "secret-api-token"
	var requests atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		wantAuth := "Bearer " + token
		if strings.HasPrefix(r.URL.Path, "/repos/pipeline/private-name") {
			wantAuth = ""
		}
		if r.Header.Get("Authorization") != wantAuth {
			t.Error("unexpected authentication on source request")
		}
		switch {
		case r.URL.Path == "/repos/pipeline/private-name", r.URL.Path == "/repos/public/action-name":
			_, _ = io.WriteString(w, `{"private":false,"visibility":"public"}`)
		case strings.HasSuffix(r.URL.Path, "/tags/release-name"):
			_, _ = fmt.Fprintf(w, `{"object":{"type":"tag","sha":%q}}`, testSHA)
		case strings.Contains(r.URL.Path, "/git/tags/"), strings.HasSuffix(r.URL.Path, "/heads/branch-name"):
			_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = fmt.Fprintf(w, `{"sha":%q}`, testSHA)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL), WithGitHubActionSourceTokenProvider("pipeline/private-name", func(context.Context) (string, error) {
		return token, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stats := WithGitHubAPIStats(t.Context())
	ctx = WithPublicRepositoryChecks(ctx)
	for _, raw := range []string{"pipeline/private-name@branch-name", "public/action-name@release-name", "public/action-name@commit-name", "public/action-name@" + testSHA} {
		ref, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if resolved, err := resolver.Resolve(ctx, ref); err != nil || resolved.Commit != testSHA {
			t.Fatalf("Resolve() = %#v, %v", resolved, err)
		}
	}
	got := stats.Snapshot()
	want := []GitHubAPIRequestCount{
		{Authentication: "anonymous", Endpoint: "branch_ref", Status: 200, Outcome: "response", Count: 1},
		{Authentication: "anonymous", Endpoint: "repository", Status: 200, Outcome: "response", Count: 1},
		{Authentication: "anonymous", Endpoint: "tag_ref", Status: 404, Outcome: "response", Count: 1},
		{Authentication: "authenticated", Endpoint: "annotated_tag", Status: 200, Outcome: "response", Count: 1},
		{Authentication: "authenticated", Endpoint: "branch_ref", Status: 404, Outcome: "response", Count: 1},
		{Authentication: "authenticated", Endpoint: "commit", Status: 200, Outcome: "response", Count: 1},
		{Authentication: "authenticated", Endpoint: "repository", Status: 200, Outcome: "response", Count: 1},
		{Authentication: "authenticated", Endpoint: "tag_ref", Status: 200, Outcome: "response", Count: 1},
		{Authentication: "authenticated", Endpoint: "tag_ref", Status: 404, Outcome: "response", Count: 1},
	}
	if got.Scope != "github_source_rest" || got.HTTPAttempts != requests.Load() || got.HTTPAttempts != 9 || got.HTTPResponses != 9 || !slices.Equal(got.Requests, want) {
		t.Fatalf("stats = %#v, wire requests = %d; want %#v", got, requests.Load(), want)
	}
	if !slices.Equal(got.CacheHits, []SourceCacheHitCount{{Cache: "public_repository_check", Count: 1}}) || len(got.Suppressed) != 0 {
		t.Fatalf("cache/suppression counts = %#v / %#v", got.CacheHits, got.Suppressed)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, "pipeline", "private-name", "action-name", "branch-name", "release-name", "commit-name", testSHA, server.URL} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("summary contains request identity %q", secret)
		}
	}
}

func TestGitHubAPIStatsSeparateSuppressedAndCanceledLookups(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprintf("authenticated=%t", authenticated), func(t *testing.T) {
			var requests atomic.Uint64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(server.Close)
			options := []Option{WithTestEndpoints(server.URL)}
			authentication, endpoint := "anonymous", "tag_ref"
			if authenticated {
				authentication, endpoint = "authenticated", "repository"
				options = append(options, WithGitHubAPITokenProvider(func(context.Context) (string, error) { return "token", nil }))
			}
			resolver, err := NewResolver(server.Client(), options...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, stats := WithGitHubAPIStats(t.Context())
			ref, _ := Parse("owner/action@v1")
			for range 2 {
				_, err := resolver.Resolve(ctx, ref)
				var rate *RateLimitError
				if !errors.As(err, &rate) {
					t.Fatalf("Resolve() error = %v, want rate limit", err)
				}
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := resolver.Resolve(canceled, ref); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled Resolve() error = %v", err)
			}
			pinned, _ := Parse("owner/action@" + testSHA)
			if _, err := resolver.Resolve(ctx, pinned); err != nil {
				t.Fatal(err)
			}
			got := stats.Snapshot()
			if requests.Load() != 1 || got.HTTPAttempts != 1 || got.HTTPResponses != 1 || !slices.Equal(got.Requests, []GitHubAPIRequestCount{{Authentication: authentication, Endpoint: endpoint, Status: 429, Outcome: "response", Count: 1}}) {
				t.Fatalf("stats = %#v; wire requests = %d", got, requests.Load())
			}
			if !slices.Equal(got.Suppressed, []GitHubAPISuppressedCount{{Authentication: authentication, Endpoint: endpoint, Reason: "rate_limit", Count: 1}}) || len(got.CacheHits) != 0 {
				t.Fatalf("suppression/cache counts = %#v / %#v", got.Suppressed, got.CacheHits)
			}
		})
	}
}

type statsRoundTripper func(*http.Request) (*http.Response, error)

func (f statsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type statsBrokenBody struct{}

func (statsBrokenBody) Read([]byte) (int, error) { return 0, errors.New("secret body failure") }

func TestGitHubAPIStatsCountFailedHTTPAttempts(t *testing.T) {
	for _, test := range []struct {
		name, outcome string
		status        int
		body          io.Reader
		err           error
	}{
		{name: "transport", outcome: "transport_error", err: errors.New("secret network failure")},
		{name: "canceled", outcome: "canceled", err: context.Canceled},
		{name: "read", status: 200, outcome: "read_error", body: statsBrokenBody{}},
		{name: "oversized", status: 200, outcome: "read_error", body: strings.NewReader(strings.Repeat("x", 1<<20+1))},
		{name: "decode", status: 200, outcome: "decode_error", body: strings.NewReader("not JSON: secret response")},
		{name: "server error", status: 503, outcome: "response", body: strings.NewReader("secret server response")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts int
			client := &http.Client{Transport: statsRoundTripper(func(r *http.Request) (*http.Response, error) {
				attempts++
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(test.body), Header: make(http.Header), Request: r}, nil
			})}
			resolver, err := NewResolver(client, WithTestEndpoints("https://source-api.invalid"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, stats := WithGitHubAPIStats(t.Context())
			ref, _ := Parse("secret-owner/secret-repository@secret-ref")
			if _, err := resolver.Resolve(ctx, ref); err == nil {
				t.Fatal("Resolve() succeeded")
			}
			got := stats.Snapshot()
			var responses uint64
			if test.status != 0 {
				responses = 1
			}
			if attempts != 1 || got.HTTPAttempts != 1 || got.HTTPResponses != responses || !slices.Equal(got.Requests, []GitHubAPIRequestCount{{Authentication: "anonymous", Endpoint: "tag_ref", Status: test.status, Outcome: test.outcome, Count: 1}}) {
				t.Fatalf("stats = %#v; attempts = %d", got, attempts)
			}
			data, err := json.Marshal(got)
			if err != nil || strings.Contains(string(data), "secret") || strings.Contains(string(data), "source-api.invalid") {
				t.Fatalf("unsafe summary: %s; %v", data, err)
			}
		})
	}
}

func TestGitHubAPIStatsNameCacheLayersAndKeepCommandsIndependent(t *testing.T) {
	for _, test := range []struct {
		name, cache string
		missing     bool
		snapshot    bool
	}{
		{name: "mutable reference", cache: "mutable_ref_disk"},
		{name: "positive snapshot", cache: "snapshot_resolved", snapshot: true},
		{name: "negative snapshot", cache: "snapshot_missing", snapshot: true, missing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Uint64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if test.missing {
					http.NotFound(w, r)
					return
				}
				_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
			}))
			t.Cleanup(server.Close)
			cacheRoot := t.TempDir()
			option := Option(func(c *config) error {
				c.mutableRefs = &mutableRefCache{root: cacheRoot, freshness: time.Hour}
				return nil
			})
			if test.snapshot {
				option = WithActionResolutionSnapshot(cacheRoot, false)
			}
			resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL), option)
			if err != nil {
				t.Fatal(err)
			}
			ctx, stats := WithGitHubAPIStats(t.Context())
			ref, _ := Parse("owner/action@v1")
			resolve := func(ctx context.Context) {
				t.Helper()
				_, err := resolver.Resolve(ctx, ref)
				var missing *NotPublicError
				if (test.missing && !errors.As(err, &missing)) || (!test.missing && err != nil) {
					t.Fatalf("Resolve() error = %v", err)
				}
			}
			resolve(ctx)
			resolve(ctx)
			wantRequests := uint64(1)
			if test.missing {
				wantRequests = 3
			}
			wantHits := []SourceCacheHitCount{{Cache: test.cache, Count: 1}}
			before := stats.Snapshot()
			if requests.Load() != wantRequests || before.HTTPAttempts != wantRequests || before.HTTPResponses != wantRequests || !slices.Equal(before.CacheHits, wantHits) {
				t.Fatalf("initial stats = %#v, wire requests = %d", before, requests.Load())
			}
			nextContext, nextStats := WithGitHubAPIStats(ctx)
			resolve(nextContext)
			next := nextStats.Snapshot()
			if requests.Load() != wantRequests || next.HTTPAttempts != 0 || next.HTTPResponses != 0 || len(next.Requests) != 0 || !slices.Equal(next.CacheHits, wantHits) || !reflect.DeepEqual(before, stats.Snapshot()) {
				t.Fatalf("next stats = %#v, parent stats = %#v", next, stats.Snapshot())
			}
		})
	}
}

func TestGitHubAPIStatsConcurrentRequestsAndSnapshots(t *testing.T) {
	const workers = 40
	var requests atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/repos/owner/action" {
			_, _ = io.WriteString(w, `{"private":false,"visibility":"public"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
	}))
	t.Cleanup(server.Close)
	resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL), WithGitHubAPITokenProvider(func(context.Context) (string, error) { return "token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stats := WithGitHubAPIStats(t.Context())
	ctx = WithPublicRepositoryChecks(ctx)
	var group sync.WaitGroup
	errors := make(chan error, workers)
	for range workers {
		group.Go(func() {
			ref, _ := Parse("owner/action@v1")
			_, err := resolver.Resolve(ctx, ref)
			errors <- err
			_ = stats.Snapshot()
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := stats.Snapshot()
	if requests.Load() != workers+1 || got.HTTPAttempts != workers+1 || got.HTTPResponses != workers+1 || !slices.Equal(got.CacheHits, []SourceCacheHitCount{{Cache: "public_repository_check", Count: workers - 1}}) {
		t.Fatalf("stats = %#v; wire requests = %d", got, requests.Load())
	}
	want := []GitHubAPIRequestCount{
		{Authentication: "authenticated", Endpoint: "repository", Status: 200, Outcome: "response", Count: 1},
		{Authentication: "authenticated", Endpoint: "tag_ref", Status: 200, Outcome: "response", Count: workers},
	}
	if !slices.Equal(got.Requests, want) {
		t.Fatalf("request counts = %#v, want %#v", got.Requests, want)
	}
	got.Requests[0].Count = 999
	got.CacheHits[0].Count = 999
	if !slices.Equal(stats.Snapshot().Requests, want) || stats.Snapshot().CacheHits[0].Count != workers-1 {
		t.Fatal("mutating a snapshot changed the shared counters")
	}
}

func TestGitHubAPIStatsExcludeSourceArchivesAndTreeCache(t *testing.T) {
	archive := tgz(t, []tar.Header{
		{Name: "root/", Typeflag: tar.TypeDir},
		{Name: "root/action.yml", Typeflag: tar.TypeReg, Mode: 0o644},
	})
	var downloads atomic.Uint64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	store, err := NewStore(t.TempDir(), server.Client(), WithTestEndpoints(server.URL, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stats := WithGitHubAPIStats(t.Context())
	ref, _ := Parse("owner/action@" + testSHA)
	for range 2 {
		materialized, err := store.Materialize(ctx, Resolved{Reference: ref, Commit: testSHA})
		if err != nil {
			t.Fatal(err)
		}
		materialized.Release()
	}
	got := stats.Snapshot()
	if downloads.Load() != 1 || got.HTTPAttempts != 0 || got.HTTPResponses != 0 || len(got.Requests) != 0 || len(got.CacheHits) != 0 || len(got.Suppressed) != 0 {
		t.Fatalf("archive downloads = %d; REST summary = %#v", downloads.Load(), got)
	}
}
