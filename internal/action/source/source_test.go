package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestParse(t *testing.T) {
	for _, good := range []string{"owner/repo@v1", "owner/repo/sub/action@feature/slash", "o/r@deadbeef"} {
		r, err := Parse(good)
		if err != nil || r.Raw != good {
			t.Errorf("Parse(%q) = %#v, %v", good, r, err)
		}
	}
	for _, bad := range []string{"", "./local@v1", "owner@v1", "owner/repo", "owner/repo@", "owner//repo@v1", "owner/repo/../x@v1", `owner/repo\x@v1`, "owner/repo@a?b", "owner/repo@${{ x }}", "-owner/repo@v1", "owner/repo.git@v1", "owner/repo@a//b"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestParseExplainsUnsupportedContainerActions(t *testing.T) {
	for _, reference := range []string{"docker://alpine:3.20", "docker://image@sha256:abc", "DOCKER://alpine:latest"} {
		if _, err := Parse(reference); err == nil || err.Error() != UnsupportedContainerActionReason {
			t.Errorf("Parse(%q) error = %v, want %q", reference, err, UnsupportedContainerActionReason)
		}
	}
}

func TestResolverTagPeelingAndHeaders(t *testing.T) {
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials sent")
		}
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Accept") == "" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("required headers missing")
		}
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/tags/"):
			_, _ = fmt.Fprintf(w, `{"object":{"type":"tag","sha":"%s"}}`, testSHA)
		case strings.Contains(r.URL.Path, "/git/tags/"):
			_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":"%s"}}`, testSHA)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	r, err := NewResolver(&http.Client{}, WithTestEndpoints(ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("owner/repo@v1")
	got, err := r.Resolve(t.Context(), ref)
	if err != nil || got.Commit != testSHA || len(calls) != 2 {
		t.Fatalf("Resolve = %#v, %v; calls %v", got, err, calls)
	}
}

func TestResolverOptionalAuthenticationAndVisibility(t *testing.T) {
	for _, tt := range []struct {
		name            string
		token           string
		tokenRepository string
		private         bool
		visibility      string
		wantAuth        string
		wantNotPublic   bool
		localToken      bool
	}{
		{name: "anonymous", visibility: "public"},
		{name: "authenticated public", token: "test-token", tokenRepository: "pipeline/repo", visibility: "public", wantAuth: "Bearer test-token"},
		{name: "local authenticated public", token: "test-token", visibility: "public", wantAuth: "Bearer test-token", localToken: true},
		{name: "authenticated private", token: "test-token", tokenRepository: "pipeline/repo", private: true, visibility: "private", wantAuth: "Bearer test-token", wantNotPublic: true},
		{name: "authenticated internal", token: "test-token", tokenRepository: "pipeline/repo", visibility: "internal", wantAuth: "Bearer test-token", wantNotPublic: true},
		{name: "credential-scoping repository remains anonymous", token: "test-token", tokenRepository: "o/r", visibility: "public"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var refRequests int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != tt.wantAuth {
					t.Errorf("Authorization = %q, want %q", got, tt.wantAuth)
				}
				switch r.URL.Path {
				case "/repos/o/r":
					_, _ = fmt.Fprintf(w, `{"private":%t,"visibility":%q}`, tt.private, tt.visibility)
				case "/repos/o/r/git/ref/tags/v1":
					refRequests++
					_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":"%s"}}`, testSHA)
				default:
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()
			opts := []Option{WithTestEndpoints(ts.URL)}
			if tt.token != "" {
				provider := func(context.Context) (string, error) { return tt.token, nil }
				if tt.localToken {
					opts = append(opts, WithGitHubAPITokenProvider(provider))
				} else {
					opts = append(opts, WithGitHubActionSourceTokenProvider(tt.tokenRepository, provider))
				}
			}
			resolver, err := NewResolver(ts.Client(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			ref, _ := Parse("o/r@v1")
			_, err = resolver.Resolve(t.Context(), ref)
			if tt.wantNotPublic {
				var notPublic *NotPublicError
				if !errors.As(err, &notPublic) || refRequests != 0 {
					t.Fatalf("Resolve() error = %v, ref requests = %d", err, refRequests)
				}
			} else if err != nil || refRequests != 1 {
				t.Fatalf("Resolve() error = %v, ref requests = %d", err, refRequests)
			}
		})
	}
}

func TestResolverFullSHADirectAndRefEncoding(t *testing.T) {
	tests := []struct {
		ref           string
		authenticated bool
		calls         []string
	}{
		{ref: testSHA, authenticated: true},
		{ref: "feature/a+b", calls: []string{"/repos/o/r/git/ref/tags/feature%2Fa+b", "/repos/o/r/git/ref/heads/feature%2Fa+b", "/repos/o/r/commits/feature%2Fa+b"}},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			var calls []string
			var provisions int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.EscapedPath())
				if strings.Contains(r.URL.Path, "/commits/") {
					_, _ = fmt.Fprintf(w, `{"sha":"%s"}`, testSHA)
					return
				}
				http.NotFound(w, r)
			}))
			defer ts.Close()
			opts := []Option{WithTestEndpoints(ts.URL)}
			if tt.authenticated {
				opts = append(opts, WithGitHubActionSourceTokenProvider("pipeline/repo", func(context.Context) (string, error) {
					provisions++
					return "test-token", nil
				}))
			}
			resolver, _ := NewResolver(ts.Client(), opts...)
			ref, _ := Parse("o/r@" + tt.ref)
			if _, err := resolver.Resolve(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(calls) != fmt.Sprint(tt.calls) || provisions != 0 {
				t.Fatalf("calls/provisions = %q / %d, want %q / 0", calls, provisions, tt.calls)
			}
		})
	}
}

func TestResolverOnlyTreatsLowercaseFullSHAAsResolved(t *testing.T) {
	upperSHA := strings.ToUpper(testSHA)
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if strings.Contains(r.URL.Path, "/commits/") {
			_, _ = fmt.Fprintf(w, `{"sha":"%s"}`, testSHA)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	resolver, err := NewResolver(ts.Client(), WithTestEndpoints(ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := Parse("o/r@" + upperSHA)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(t.Context(), ref)
	if err != nil || resolved.Commit != testSHA || !slices.Equal(calls, []string{"/repos/o/r/git/ref/tags/" + upperSHA, "/repos/o/r/git/ref/heads/" + upperSHA, "/repos/o/r/commits/" + upperSHA}) {
		t.Fatalf("Resolve() = %#v, %v; calls = %v", resolved, err, calls)
	}
}

func TestResolverCachesMutableRefsWithBoundedFreshness(t *testing.T) {
	cache := t.TempDir()
	var calls atomic.Int32
	commit := testSHA
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, commit)
	}))
	defer server.Close()
	ref, _ := Parse("owner/repo@v1")

	resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	resolver.cfg.mutableRefs, err = newMutableRefCache(cache, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	commit = strings.Repeat("a", 40)
	resolver, err = NewResolver(server.Client(), WithTestEndpoints(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	resolver.cfg.mutableRefs, err = newMutableRefCache(cache, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(t.Context(), ref)
	if err != nil || resolved.Commit != testSHA || calls.Load() != 1 {
		t.Fatalf("cached Resolve() = %#v, %v; calls = %d", resolved, err, calls.Load())
	}
	time.Sleep(60 * time.Millisecond)
	resolved, err = resolver.Resolve(t.Context(), ref)
	if err != nil || resolved.Commit != commit || calls.Load() != 2 {
		t.Fatalf("revalidated Resolve() = %#v, %v; calls = %d", resolved, err, calls.Load())
	}
}

func TestResolverMutableRefCacheCoalescesConcurrentProcesses(t *testing.T) {
	cache := t.TempDir()
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
	}))
	defer server.Close()
	ref, _ := Parse("owner/repo@v1")
	const workers = 12
	errors := make(chan error, workers)
	for range workers {
		go func() {
			resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
			if err == nil {
				resolver.cfg.mutableRefs, err = newMutableRefCache(cache, time.Hour)
			}
			if err == nil {
				_, err = resolver.Resolve(t.Context(), ref)
			}
			errors <- err
		}()
	}
	<-started
	close(release)
	for range workers {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("GitHub requests = %d, want 1", calls.Load())
	}
}

func TestResolverUsesPlatformMutableRefCache(t *testing.T) {
	root, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "buildkite-gha", "action-ref-resolutions", "v1")
	if resolver.cfg.mutableRefs == nil || resolver.cfg.mutableRefs.root != want || resolver.cfg.mutableRefs.freshness != time.Hour {
		t.Fatalf("mutable ref cache = %#v, want root %q with one-hour freshness", resolver.cfg.mutableRefs, want)
	}
}

func TestResolverFallbackErrorsAndCancellation(t *testing.T) {
	t.Run("rate limit", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		}))
		defer ts.Close()
		resolver, _ := NewResolver(ts.Client(), WithTestEndpoints(ts.URL))
		ref, _ := Parse("o/r@v1")
		var rate *RateLimitError
		if _, err := resolver.Resolve(t.Context(), ref); !errors.As(err, &rate) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ioString(w, "{") }))
		defer ts.Close()
		resolver, _ := NewResolver(ts.Client(), WithTestEndpoints(ts.URL))
		ref, _ := Parse("o/r@v1")
		if _, err := resolver.Resolve(t.Context(), ref); err == nil {
			t.Fatal("expected malformed response error")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		resolver, _ := NewResolver(nil)
		ref, _ := Parse("o/r@v1")
		if _, err := resolver.Resolve(ctx, ref); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
}

func ioString(w http.ResponseWriter, s string) { _, _ = w.Write([]byte(s)) }

func TestExtractRejectsUnsafeArchives(t *testing.T) {
	c := defaults()
	c.maxFile, c.maxExpanded, c.maxEntries = 3, 6, 4
	tests := []struct {
		name    string
		entries []tar.Header
	}{
		{"traversal", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/../x", Typeflag: tar.TypeReg}}},
		{"link", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeSymlink, Linkname: "y"}}},
		{"absolute link", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/y", Typeflag: tar.TypeReg}, {Name: "root/x", Typeflag: tar.TypeSymlink, Linkname: "/root/y"}}},
		{"escaping link", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/y", Typeflag: tar.TypeReg}, {Name: "root/x", Typeflag: tar.TypeSymlink, Linkname: "../../y"}}},
		{"directory link", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/y/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeSymlink, Linkname: "y"}}},
		{"link ancestor first", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/y", Typeflag: tar.TypeReg}, {Name: "root/x", Typeflag: tar.TypeSymlink, Linkname: "y"}, {Name: "root/x/child", Typeflag: tar.TypeReg}}},
		{"link ancestor last", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/y", Typeflag: tar.TypeReg}, {Name: "root/x/child", Typeflag: tar.TypeReg}, {Name: "root/x", Typeflag: tar.TypeSymlink, Linkname: "y"}}},
		{"hardlink", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeLink, Linkname: "root/y"}}},
		{"fifo", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeFifo}}},
		{"device", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeChar}}},
		{"duplicate", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeReg}, {Name: "root/X", Typeflag: tar.TypeReg}}},
		{"mixed roots", []tar.Header{{Name: "one/", Typeflag: tar.TypeDir}, {Name: "two/", Typeflag: tar.TypeDir}}},
		{"file size", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeReg, Size: 4}}},
		{"count", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/a", Typeflag: tar.TypeDir}, {Name: "root/b", Typeflag: tar.TypeDir}, {Name: "root/c", Typeflag: tar.TypeDir}, {Name: "root/d", Typeflag: tar.TypeDir}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := extractTar(bytes.NewReader(tarBytes(t, tt.entries)), filepath.Join(t.TempDir(), "out"), c); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestExtractOmitsSafeRepositorySymlinks(t *testing.T) {
	orders := [][]tar.Header{
		{
			{Name: "root/", Typeflag: tar.TypeDir},
			{Name: "root/__tests__/data/poetry.lock", Typeflag: tar.TypeReg, Size: 1},
			{Name: "root/__tests__/data/inner/", Typeflag: tar.TypeDir},
			{Name: "root/__tests__/data/inner/poetry.lock", Typeflag: tar.TypeSymlink, Linkname: "../poetry.lock"},
		},
		{
			{Name: "root/", Typeflag: tar.TypeDir},
			{Name: "root/__tests__/data/inner/poetry.lock", Typeflag: tar.TypeSymlink, Linkname: "../poetry.lock"},
			{Name: "root/__tests__/data/inner/", Typeflag: tar.TypeDir},
			{Name: "root/__tests__/data/poetry.lock", Typeflag: tar.TypeReg, Size: 1},
		},
	}
	var digests []string
	for _, entries := range orders {
		out := filepath.Join(t.TempDir(), "out")
		if err := extractTar(bytes.NewReader(tarBytes(t, entries)), out, defaults()); err != nil {
			t.Fatalf("extract symlink archive: %v", err)
		}
		if _, err := os.Stat(filepath.Join(out, "__tests__", "data", "poetry.lock")); err != nil {
			t.Fatalf("regular target: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(out, "__tests__", "data", "inner", "poetry.lock")); !os.IsNotExist(err) {
			t.Fatalf("omitted alias exists: %v", err)
		}
		manifest, err := buildManifest(out, defaults(), Reference{Owner: "actions", Repository: "setup-python"}, testSHA)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, manifest.Digest)
	}
	if digests[0] != digests[1] {
		t.Fatalf("omitted symlink changed digest by archive order: %q != %q", digests[0], digests[1])
	}
}

func TestExtractPAXCompatibilityAndRejection(t *testing.T) {
	longName := "root/" + strings.Repeat("long-segment-", 10) + "action.yml"
	good := []tar.Header{
		{Name: "global", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "github codeload"}, Format: tar.FormatPAX},
		{Name: "root/", Typeflag: tar.TypeDir},
		{Name: longName, Typeflag: tar.TypeReg, Size: 1, Mode: 0o711, Format: tar.FormatPAX, PAXRecords: map[string]string{"path": longName, "mtime": "1.0"}},
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := extractTar(bytes.NewReader(tarBytes(t, good)), out, defaults()); err != nil {
		t.Fatalf("extract PAX archive: %v", err)
	}
	if st, err := os.Stat(filepath.Join(out, strings.TrimPrefix(longName, "root/"))); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("normalized long-path file = %v, %v", st, err)
	}

	// archive/tar consumes some standardized keys (notably linkpath and GNU
	// sparse records) before exposing Header.PAXRecords. Exercise records it
	// preserves so the extractor's deny-by-default policy is observable.
	for _, key := range []string{"SCHILY.xattr.user.evil", "security.capability"} {
		t.Run(key, func(t *testing.T) {
			entries := []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeReg, Format: tar.FormatPAX, PAXRecords: map[string]string{key: "evil"}}}
			if err := extractTar(bytes.NewReader(tarBytes(t, entries)), filepath.Join(t.TempDir(), "out"), defaults()); err == nil {
				t.Fatal("expected PAX record rejection")
			}
		})
	}
}

func TestExtractDigestIndependentOfUmask(t *testing.T) {
	archive := tarBytes(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/plain", Typeflag: tar.TypeReg, Size: 1, Mode: 0o666}, {Name: "root/executable", Typeflag: tar.TypeReg, Size: 1, Mode: 0o744}})
	old, err := testUmask(0o022)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("umask unsupported")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = testUmask(old) }()
	digests := make([]string, 0, 2)
	for _, mask := range []int{0o022, 0o077} {
		if _, err := testUmask(mask); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "tree")
		if err := extractTar(bytes.NewReader(archive), out, defaults()); err != nil {
			t.Fatal(err)
		}
		m, err := buildManifest(out, defaults(), Reference{Owner: "o", Repository: "r"}, testSHA)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, m.Digest)
	}
	if digests[0] != digests[1] {
		t.Fatalf("digest changed with umask: %q != %q", digests[0], digests[1])
	}
}

func TestDigestTreeIsCanonicalAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "action.yml"), []byte("runs:\n  using: node24\n  main: dist/index.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "dist", "index.js")
	if err := os.WriteFile(entry, []byte("console.log('one')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digests = %q and %q, want stable sha256 digest", first, second)
	}
	if err := os.Chmod(entry, 0o664); err != nil {
		t.Fatal(err)
	}
	nonExecutableMode, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if nonExecutableMode != first {
		t.Fatalf("digest changed across non-executable Git modes: %q != %q", nonExecutableMode, first)
	}
	if err := os.Chmod(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	executableMode, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if executableMode == first {
		t.Fatal("digest did not change when Git executable bit changed")
	}
	if err := os.Chmod(entry, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("console.log('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("digest did not change with action source")
	}
	if err := os.Symlink("index.js", filepath.Join(root, "dist", "linked.js")); err != nil {
		t.Fatal(err)
	}
	if _, err := DigestTree(root); err == nil || !strings.Contains(err.Error(), "special file") {
		t.Fatalf("DigestTree() symlink error = %v, want special-file rejection", err)
	}
	if _, err := DigestTree(entry); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("DigestTree() file error = %v, want directory rejection", err)
	}
}

func TestDigestTreeExcludesGitMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "action.yml"), []byte("runs:\n  using: node24\n  main: .git/index.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(root, ".git", "index.js")
	if err := os.WriteFile(entrypoint, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest includes Git metadata: %q != %q", first, second)
	}
	action, err := metadata.Load(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := action.ValidateEntrypoints(metadata.RuntimeNode24); err == nil || !strings.Contains(err.Error(), "excluded from verified action source") {
		t.Fatalf("ValidateEntrypoints() error = %v, want verified-source exclusion", err)
	}
}

func tarBytes(t *testing.T, entries []tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := range entries {
		if err := tw.WriteHeader(&entries[i]); err != nil {
			t.Fatal(err)
		}
		if entries[i].Size > 0 {
			_, _ = tw.Write(make([]byte, entries[i].Size))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func tgz(t *testing.T, entries []tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	_, _ = gz.Write(tarBytes(t, entries))
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestStoreExactCommitAtomicHitAndSubpath(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "repo-root/", Typeflag: tar.TypeDir}, {Name: "repo-root/action/", Typeflag: tar.TypeDir}, {Name: "repo-root/action/action.yml", Typeflag: tar.TypeReg, Size: 2, Mode: 0o755}})
	var requests atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/Owner/Repo/tar.gz/"+testSHA {
			t.Errorf("URL = %s", r.URL.Path)
		}
		_, _ = w.Write(archive)
	}))
	defer ts.Close()
	store, err := NewStore(t.TempDir(), ts.Client(), WithTestEndpoints(ts.URL, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("Owner/Repo/action@v1")
	resolved := Resolved{Reference: ref, Commit: testSHA}
	first, err := store.Materialize(t.Context(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(first.ActionRoot, "action.yml")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.SourceDigest, "sha256:") || first.RepositoryRoot == first.ActionRoot {
		t.Fatalf("materialized identity = %#v", first)
	}
	second, err := store.Materialize(t.Context(), resolved)
	if err != nil || second.RepositoryRoot != first.RepositoryRoot || second.ActionRoot != first.ActionRoot || second.SourceDigest != first.SourceDigest || requests.Load() != 1 {
		t.Fatalf("second = %v, %v; requests=%d", second, err, requests.Load())
	}
	first.Release()
	second.Release()
}

func TestStoreRejectsInvalidUTF8ArchiveMetadataOnEveryMaterialization(t *testing.T) {
	tests := []struct {
		name    string
		entries []tar.Header
	}{
		{
			name: "name",
			entries: []tar.Header{
				{Name: "root/", Typeflag: tar.TypeDir},
				{Name: "root/\xff", Typeflag: tar.TypeReg},
			},
		},
		{
			name: "symlink target",
			entries: []tar.Header{
				{Name: "root/", Typeflag: tar.TypeDir},
				{Name: "root/target", Typeflag: tar.TypeReg},
				{Name: "root/link", Typeflag: tar.TypeSymlink, Linkname: "target/\xff/.."},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := tgz(t, tt.entries)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(archive)
			}))
			defer server.Close()

			store, err := NewStore(t.TempDir(), server.Client(), WithTestEndpoints(server.URL, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			ref, _ := Parse("Owner/Repo@v1")
			resolved := Resolved{Reference: ref, Commit: testSHA}
			for attempt := 1; attempt <= 2; attempt++ {
				materialized, err := store.Materialize(t.Context(), resolved)
				if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
					materialized.Release()
					t.Fatalf("materialization %d error = %v, want invalid UTF-8 rejection", attempt, err)
				}
			}
		})
	}
}

func TestStoreCanonicalizesAliasedAncestorAndRejectsSymlinkRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	logicalRoot := filepath.Join(logicalParent, "actions")
	if err := os.Mkdir(logicalRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(logicalRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if store.root != filepath.Join(realParent, "actions") {
		t.Fatalf("store root = %q, want canonical root", store.root)
	}
	linkedRoot := filepath.Join(base, "linked-root")
	if err := os.Symlink(store.root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(linkedRoot, nil); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
		t.Fatalf("NewStore() accepted symlink root: %v", err)
	}
	missingRoot := filepath.Join(base, "missing-root")
	if _, err := NewStore(missingRoot, nil); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
		t.Fatalf("NewStore() accepted missing root: %v", err)
	}
	if _, err := os.Lstat(missingRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewStore() created missing root: %v", err)
	}
}

func TestStoreExpectedDigestAndManifestRejectTampering(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	defer ts.Close()
	root := t.TempDir()
	store, _ := NewStore(root, ts.Client(), WithTestEndpoints(ts.URL, ts.URL))
	ref, _ := Parse("Owner/Repo@v1")
	r := Resolved{Reference: ref, Commit: testSHA}
	got, err := store.Materialize(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.SourceDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err = store.Materialize(t.Context(), r); err == nil || !strings.Contains(err.Error(), "source digest mismatch") {
		t.Fatalf("digest mismatch error = %v", err)
	}
	manifestPath := filepath.Join(filepath.Dir(got.RepositoryRoot), manifestName)
	if err = os.WriteFile(manifestPath, []byte(`{"schema":"forged"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.SourceDigest = got.SourceDigest
	if _, err = store.Materialize(t.Context(), r); err == nil || !strings.Contains(err.Error(), "verify action source cache") {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestStoreArchiveHTTPClassification(t *testing.T) {
	for _, tt := range []struct {
		name string
		code int
		rate bool
	}{
		{"rate limited", http.StatusTooManyRequests, true},
		{"not public", http.StatusNotFound, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(tt.code)
			}))
			defer ts.Close()
			store, _ := NewStore(t.TempDir(), ts.Client(), WithTestEndpoints(ts.URL, ts.URL))
			ref, _ := Parse("o/r@v1")
			_, err := store.Materialize(t.Context(), Resolved{Reference: ref, Commit: testSHA})
			if tt.rate {
				var rate *RateLimitError
				if !errors.As(err, &rate) || rate.Reset.IsZero() {
					t.Fatalf("error = %v", err)
				}
			} else {
				var notPublic *NotPublicError
				if !errors.As(err, &notPublic) {
					t.Fatalf("error = %v", err)
				}
			}
		})
	}
}

func TestStoreExactCommitDownloadsDirectlyFromCodeloadWithoutCredentials(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	const token = "test-token"
	var apiRequests int
	var archiveRequests int
	var tokenProvisions int
	archiveServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveRequests++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("codeload credentials = Authorization %q, Cookie %q", r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		if r.URL.Path != "/o/r/tar.gz/"+testSHA {
			t.Errorf("codeload path = %q", r.URL.Path)
		}
		_, _ = w.Write(archive)
	}))
	defer archiveServer.Close()
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiRequests++
		http.Error(w, "GitHub REST must not be used for exact-commit materialization", http.StatusInternalServerError)
	}))
	defer apiServer.Close()
	ref, _ := Parse("o/r@v1")
	resolved := Resolved{Reference: ref, Commit: testSHA}
	store, err := NewStore(t.TempDir(), apiServer.Client(), WithTestEndpoints(apiServer.URL, archiveServer.URL), WithGitHubActionSourceTokenProvider("pipeline/repo", func(context.Context) (string, error) {
		tokenProvisions++
		return token, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize(t.Context(), resolved); err != nil {
		t.Fatal(err)
	}
	if apiRequests != 0 || archiveRequests != 1 || tokenProvisions != 0 {
		t.Fatalf("API/archive/token-provision requests = %d / %d / %d, want 0 / 1 / 0", apiRequests, archiveRequests, tokenProvisions)
	}
}

func TestStoreCodeloadRedirectPolicy(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	ref, _ := Parse("o/r@v1")
	resolved := Resolved{Reference: ref, Commit: testSHA}
	t.Run("same host remains anonymous", func(t *testing.T) {
		var requests int
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Errorf("redirect request credentials = Authorization %q, Cookie %q", r.Header.Get("Authorization"), r.Header.Get("Cookie"))
			}
			if r.URL.Path != "/archive" {
				http.Redirect(w, r, "/archive", http.StatusFound)
				return
			}
			_, _ = w.Write(archive)
		}))
		defer server.Close()
		store, err := NewStore(t.TempDir(), server.Client(), WithTestEndpoints(server.URL, server.URL), WithGitHubActionSourceTokenProvider("pipeline/repo", func(context.Context) (string, error) {
			return "test-token", nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Materialize(t.Context(), resolved); err != nil {
			t.Fatal(err)
		}
		if requests != 2 {
			t.Fatalf("requests = %d, want 2", requests)
		}
	})
	t.Run("cross host denied before request", func(t *testing.T) {
		var deniedRequests int
		denied := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			deniedRequests++
		}))
		defer denied.Close()
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, denied.URL+"/archive", http.StatusFound)
		}))
		defer server.Close()
		store, err := NewStore(t.TempDir(), server.Client(), WithTestEndpoints(server.URL, server.URL))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Materialize(t.Context(), resolved); err == nil || !strings.Contains(err.Error(), "archive redirect denied") {
			t.Fatalf("Materialize() error = %v, want denied redirect", err)
		}
		if deniedRequests != 0 {
			t.Fatalf("denied host requests = %d, want 0", deniedRequests)
		}
	})
}

func TestStoreConcurrentMaterialize(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	var requests atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(archive)
	}))
	defer ts.Close()
	store, _ := NewStore(t.TempDir(), ts.Client(), WithTestEndpoints(ts.URL, ts.URL))
	ref, _ := Parse("o/r@v1")
	resolved := Resolved{Reference: ref, Commit: testSHA}
	var wg sync.WaitGroup
	results := make(chan Materialized, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := store.Materialize(t.Context(), resolved)
			results <- got
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Materialize: %v", err)
		}
	}
	var digest string
	for got := range results {
		if digest != "" && digest != got.SourceDigest {
			t.Fatalf("digests differ: %q and %q", digest, got.SourceDigest)
		}
		digest = got.SourceDigest
		got.Release()
	}
	if requests.Load() != 1 {
		t.Fatalf("archive requests = %d, want 1", requests.Load())
	}
}

func TestStoreEvictionSkipsLeasedEntryAndRemovesItAfterRelease(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	defer ts.Close()
	root := t.TempDir()
	store, err := NewStore(root, ts.Client(), WithTestEndpoints(ts.URL, ts.URL), WithCacheMaxBytes(1))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("o/r@v1")
	materialized, err := store.Materialize(t.Context(), Resolved{Reference: ref, Commit: testSHA})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(materialized.RepositoryRoot)
	if err := store.maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("leased entry was evicted: %v", err)
	}
	retained, err := materialized.Retain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	materialized.Release()
	if err := store.maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("retained entry was evicted: %v", err)
	}
	retained.Release()
	if err := store.maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released entry still exists: %v", err)
	}
}

func TestStoreMaintenanceUsesLRUAndCleansOnlyUnlockedPartials(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "owner", "repo")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	commits := []string{strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)}
	var entrySize int64
	for i, commit := range commits {
		base := filepath.Join(repository, commit)
		if err := os.MkdirAll(filepath.Join(base, "tree"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "tree", "payload"), bytes.Repeat([]byte{byte('a' + i)}, 32), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest := filepath.Join(base, manifestName)
		if err := os.WriteFile(manifest, []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(manifest, when, when); err != nil {
			t.Fatal(err)
		}
		entrySize, _ = cacheEntrySize(base)
	}
	activePartial := filepath.Join(repository, ".partial-active")
	abandonedPartial := filepath.Join(repository, ".partial-abandoned")
	for _, partial := range []string{activePartial, abandonedPartial} {
		if err := os.Mkdir(partial, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	activeLock, err := lockActionCache(t.Context(), filepath.Join(activePartial, ".lock"), actionCacheLockExclusive, false)
	if err != nil {
		t.Fatal(err)
	}
	defer activeLock.unlock()
	if _, err := NewStore(root, nil, WithCacheMaxBytes(2*entrySize)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, commits[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest entry still exists: %v", err)
	}
	for _, commit := range commits[1:] {
		if _, err := os.Stat(filepath.Join(repository, commit)); err != nil {
			t.Fatalf("newer entry %s was evicted: %v", commit, err)
		}
	}
	if _, err := os.Stat(activePartial); err != nil {
		t.Fatalf("active partial was removed: %v", err)
	}
	if _, err := os.Stat(abandonedPartial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned partial still exists: %v", err)
	}
}

func TestStoreConcurrentBoundedEviction(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	defer ts.Close()
	root := t.TempDir()
	stores := make([]*Store, 2)
	for i := range stores {
		store, err := NewStore(root, ts.Client(), WithTestEndpoints(ts.URL, ts.URL), WithCacheMaxBytes(1))
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = store
	}
	ref, _ := Parse("o/r@v1")
	errs := make(chan error, 8)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			commit := fmt.Sprintf("%040x", i+1)
			materialized, err := stores[i%len(stores)].Materialize(t.Context(), Resolved{Reference: ref, Commit: commit})
			if err == nil {
				_, err = os.Stat(filepath.Join(materialized.ActionRoot, "action.yml"))
				materialized.Release()
			}
			errs <- err
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent bounded materialization: %v", err)
		}
	}
	if err := stores[0].maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, _, err := stores[0].cacheEntries()
	if err != nil || len(entries) != 0 {
		t.Fatalf("bounded entries = %d, error %v", len(entries), err)
	}
}

func TestStoreBoundedEvictionProtectsUnboundedReader(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	defer ts.Close()
	root := t.TempDir()
	unbounded, err := NewStore(root, ts.Client(), WithTestEndpoints(ts.URL, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("o/r@v1")
	active, err := unbounded.Materialize(t.Context(), Resolved{Reference: ref, Commit: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	defer active.Release()
	bounded, err := NewStore(root, ts.Client(), WithTestEndpoints(ts.URL, ts.URL), WithCacheMaxBytes(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active.ActionRoot); err != nil {
		t.Fatalf("bounded maintenance evicted an unbounded reader: %v", err)
	}
	active.Release()
	if err := bounded.maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active.ActionRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released entry still exists: %v", err)
	}
}

func TestActionResolutionSnapshotPinsAndRefreshesMutableRefs(t *testing.T) {
	const nextSHA = "1123456789abcdef0123456789abcdef01234567"
	var requests atomic.Int32
	var refreshed atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.Contains(r.URL.Path, "/git/ref/tags/") {
			http.NotFound(w, r)
			return
		}
		commit := testSHA
		if refreshed.Load() {
			commit = nextSHA
		}
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":"%s"}}`, commit)
	}))
	defer ts.Close()
	root := t.TempDir()
	newResolver := func(refresh bool) *Resolver {
		resolver, err := NewResolver(ts.Client(), WithTestEndpoints(ts.URL), WithActionResolutionSnapshot(root, refresh))
		if err != nil {
			t.Fatal(err)
		}
		return resolver
	}
	ref, _ := Parse("owner/repo@v1")
	first := newResolver(false)
	resolved, err := first.Resolve(t.Context(), ref)
	if err != nil || resolved.Commit != testSHA || requests.Load() != 1 {
		t.Fatalf("first resolution = %#v, %v; requests %d", resolved, err, requests.Load())
	}
	firstGeneration := first.ResolutionSnapshotID()
	refreshed.Store(true)
	second := newResolver(false)
	resolved, err = second.Resolve(t.Context(), ref)
	if err != nil || resolved.Commit != testSHA || requests.Load() != 1 || second.ResolutionSnapshotID() != firstGeneration {
		t.Fatalf("reused resolution = %#v, %v; requests %d; generation %q", resolved, err, requests.Load(), second.ResolutionSnapshotID())
	}
	if err := os.Remove(second.cfg.resolutionSnapshot.entryPath(ref)); err != nil {
		t.Fatal(err)
	}
	if _, err := newResolver(false).Resolve(t.Context(), ref); err == nil || !strings.Contains(err.Error(), "snapshot entry is missing") {
		t.Fatalf("missing snapshot entry error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("missing snapshot entry caused %d requests, want 1", requests.Load())
	}
	third := newResolver(true)
	resolved, err = third.Resolve(t.Context(), ref)
	if err != nil || resolved.Commit != nextSHA || requests.Load() != 2 || third.ResolutionSnapshotID() == firstGeneration {
		t.Fatalf("refreshed resolution = %#v, %v; requests %d; generation %q", resolved, err, requests.Load(), third.ResolutionSnapshotID())
	}
}

func TestActionResolutionSnapshotRetriesStorageFailures(t *testing.T) {
	original := actionResolutionSnapshotStorage
	t.Cleanup(func() { actionResolutionSnapshotStorage = original })
	failure := errors.New("injected storage failure")

	t.Run("generation publication preserves current", func(t *testing.T) {
		for _, operation := range []string{"create", "write", "short write", "close", "rename"} {
			t.Run(operation, func(t *testing.T) {
				actionResolutionSnapshotStorage = original
				root := t.TempDir()
				prior, err := newActionResolutionSnapshot(root, false)
				if err != nil {
					t.Fatal(err)
				}
				injectActionResolutionSnapshotStorageFailure(t, operation, failure)
				wantErr := failure
				if operation == "short write" {
					wantErr = io.ErrShortWrite
				}
				if _, err := newActionResolutionSnapshot(root, true); !errors.Is(err, wantErr) {
					t.Fatalf("refresh error = %v, want %v", err, wantErr)
				}
				actionResolutionSnapshotStorage = original
				retried, err := newActionResolutionSnapshot(root, false)
				if err != nil || retried.generation != prior.generation {
					t.Fatalf("retry = %#v, %v; want generation %q", retried, err, prior.generation)
				}
			})
		}
	})

	t.Run("generation initialization can retry", func(t *testing.T) {
		for _, operation := range []string{"create", "write", "short write", "close", "rename"} {
			t.Run(operation, func(t *testing.T) {
				actionResolutionSnapshotStorage = original
				root := t.TempDir()
				injectActionResolutionSnapshotStorageFailure(t, operation, failure)
				wantErr := failure
				if operation == "short write" {
					wantErr = io.ErrShortWrite
				}
				if _, err := newActionResolutionSnapshot(root, false); !errors.Is(err, wantErr) {
					t.Fatalf("initialization error = %v, want %v", err, wantErr)
				}
				actionResolutionSnapshotStorage = original
				if _, err := newActionResolutionSnapshot(root, false); err != nil {
					t.Fatalf("retry: %v", err)
				}
			})
		}
	})

	t.Run("entry publication can retry", func(t *testing.T) {
		for _, operation := range []string{"claim create", "claim close", "entry create", "entry write", "entry short write", "entry close", "entry rename"} {
			t.Run(operation, func(t *testing.T) {
				actionResolutionSnapshotStorage = original
				snapshot, err := newActionResolutionSnapshot(t.TempDir(), false)
				if err != nil {
					t.Fatal(err)
				}
				ref, _ := Parse("owner/repo@v1")
				injectActionResolutionSnapshotStorageFailure(t, operation, failure)
				resolve := func(context.Context, Reference) (Resolved, error) {
					return Resolved{Reference: ref, Commit: testSHA}, nil
				}
				wantErr := failure
				if operation == "entry short write" {
					wantErr = io.ErrShortWrite
				}
				if _, err := snapshot.resolve(t.Context(), ref, resolve); !errors.Is(err, wantErr) {
					t.Fatalf("first resolution error = %v, want %v", err, wantErr)
				}
				actionResolutionSnapshotStorage = original
				resolved, err := snapshot.resolve(t.Context(), ref, resolve)
				if err != nil || resolved.Commit != testSHA {
					t.Fatalf("retry = %#v, %v", resolved, err)
				}
			})
		}
	})
}

func injectActionResolutionSnapshotStorageFailure(t *testing.T, operation string, failure error) {
	t.Helper()
	original := actionResolutionSnapshotStorage
	failed := false
	shouldFail := func(want string) bool {
		if !failed && operation == want {
			failed = true
			return true
		}
		return false
	}
	actionResolutionSnapshotStorage.createTemp = func(dir, pattern string) (*os.File, error) {
		if shouldFail("create") || shouldFail("entry create") {
			return nil, failure
		}
		return original.createTemp(dir, pattern)
	}
	actionResolutionSnapshotStorage.openFile = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		if shouldFail("claim create") {
			return nil, failure
		}
		return original.openFile(path, flag, perm)
	}
	actionResolutionSnapshotStorage.write = func(file *os.File, data []byte) (int, error) {
		if shouldFail("write") || shouldFail("entry write") {
			return 0, failure
		}
		if shouldFail("short write") || shouldFail("entry short write") {
			return len(data) - 1, nil
		}
		return original.write(file, data)
	}
	actionResolutionSnapshotStorage.close = func(file *os.File) error {
		if shouldFail("close") || shouldFail("claim close") || shouldFail("entry close") {
			_ = file.Close()
			return failure
		}
		return original.close(file)
	}
	actionResolutionSnapshotStorage.rename = func(oldPath, newPath string) error {
		if shouldFail("rename") || shouldFail("entry rename") {
			return failure
		}
		return original.rename(oldPath, newPath)
	}
}

func TestActionResolutionSnapshotRejectsMissingCurrentGeneration(t *testing.T) {
	root := t.TempDir()
	resolver, err := NewResolver(nil, WithActionResolutionSnapshot(root, false))
	if err != nil {
		t.Fatal(err)
	}
	firstGeneration := resolver.ResolutionSnapshotID()
	if err := os.Remove(filepath.Join(root, "current.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewResolver(nil, WithActionResolutionSnapshot(root, false)); err == nil || !strings.Contains(err.Error(), "current generation is missing") {
		t.Fatalf("missing current generation error = %v", err)
	}
	refreshed, err := NewResolver(nil, WithActionResolutionSnapshot(root, true))
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ResolutionSnapshotID() == firstGeneration {
		t.Fatal("refresh reused generation after the current pointer was removed")
	}
	if err := os.RemoveAll(filepath.Join(root, "generations", refreshed.ResolutionSnapshotID())); err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("owner/repo@v1")
	called := false
	_, err = refreshed.cfg.resolutionSnapshot.resolve(t.Context(), ref, func(context.Context, Reference) (Resolved, error) {
		called = true
		return Resolved{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "current generation is missing") || called {
		t.Fatalf("running resolver after missing generation = %v; called resolver = %t", err, called)
	}
	if _, err := NewResolver(nil, WithActionResolutionSnapshot(root, false)); err == nil || !strings.Contains(err.Error(), "current generation is missing") {
		t.Fatalf("missing generation directory error = %v", err)
	}
	if _, err := NewResolver(nil, WithActionResolutionSnapshot(root, true)); err != nil {
		t.Fatalf("refresh after missing generation directory: %v", err)
	}
}

func TestActionResolutionSnapshotPersistsOnlyDefinitiveMissingRefs(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		var requests atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.NotFound(w, r)
		}))
		defer ts.Close()
		resolver, err := NewResolver(ts.Client(), WithTestEndpoints(ts.URL), WithActionResolutionSnapshot(t.TempDir(), false))
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := Parse("owner/repo@missing")
		for range 2 {
			_, err := resolver.Resolve(t.Context(), ref)
			var notPublic *NotPublicError
			if !errors.As(err, &notPublic) {
				t.Fatalf("resolution error = %v", err)
			}
		}
		if requests.Load() != 3 {
			t.Fatalf("missing ref requests = %d, want 3", requests.Load())
		}
	})

	t.Run("server failure", func(t *testing.T) {
		var requests atomic.Int32
		var failed atomic.Bool
		failed.Store(true)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if failed.Load() {
				http.Error(w, "temporary", http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":"%s"}}`, testSHA)
		}))
		defer ts.Close()
		resolver, err := NewResolver(ts.Client(), WithTestEndpoints(ts.URL), WithActionResolutionSnapshot(t.TempDir(), false))
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := Parse("owner/repo@v1")
		if _, err := resolver.Resolve(t.Context(), ref); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("transient resolution error = %v", err)
		}
		failed.Store(false)
		resolved, err := resolver.Resolve(t.Context(), ref)
		if err != nil || resolved.Commit != testSHA || requests.Load() != 2 {
			t.Fatalf("retried resolution = %#v, %v; requests %d", resolved, err, requests.Load())
		}
	})
}

func TestActionResolutionSnapshotCoalescesConcurrentResolution(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(20 * time.Millisecond)
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":"%s"}}`, testSHA)
	}))
	defer ts.Close()
	resolver, err := NewResolver(ts.Client(), WithTestEndpoints(ts.URL), WithActionResolutionSnapshot(t.TempDir(), false))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("owner/repo@v1")
	errs := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			resolved, err := resolver.Resolve(t.Context(), ref)
			if err == nil && resolved.Commit != testSHA {
				err = fmt.Errorf("commit = %s", resolved.Commit)
			}
			errs <- err
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("concurrent resolution requests = %d, want 1", requests.Load())
	}
}

func TestActionResolutionSnapshotRejectsCorruptEntry(t *testing.T) {
	root := t.TempDir()
	resolver, err := NewResolver(nil, WithTestEndpoints("https://example.com"), WithActionResolutionSnapshot(root, false))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("owner/repo@v1")
	path := resolver.cfg.resolutionSnapshot.entryPath(ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), ref); err == nil || !strings.Contains(err.Error(), "snapshot entry is invalid") {
		t.Fatalf("corrupt snapshot error = %v", err)
	}
}
