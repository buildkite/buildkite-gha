package source

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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

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
	for _, bad := range []string{"", "./local@v1", "docker://image@v1", "owner@v1", "owner/repo", "owner/repo@", "owner//repo@v1", "owner/repo/../x@v1", `owner/repo\x@v1`, "owner/repo@a?b", "owner/repo@${{ x }}", "-owner/repo@v1", "owner/repo.git@v1", "owner/repo@a//b"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
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
	got, err := r.Resolve(context.Background(), ref)
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
	}{
		{name: "anonymous", visibility: "public"},
		{name: "authenticated public", token: "test-token", tokenRepository: "pipeline/repo", visibility: "public", wantAuth: "Bearer test-token"},
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
				opts = append(opts, WithScopedGitHubToken(tt.token, tt.tokenRepository))
			}
			resolver, err := NewResolver(ts.Client(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			ref, _ := Parse("o/r@v1")
			_, err = resolver.Resolve(context.Background(), ref)
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
				opts = append(opts, WithScopedGitHubToken("test-token", "pipeline/repo"))
			}
			resolver, _ := NewResolver(ts.Client(), opts...)
			ref, _ := Parse("o/r@" + tt.ref)
			if _, err := resolver.Resolve(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(calls) != fmt.Sprint(tt.calls) {
				t.Fatalf("calls = %q, want %q", calls, tt.calls)
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
	resolved, err := resolver.Resolve(context.Background(), ref)
	if err != nil || resolved.Commit != testSHA || !slices.Equal(calls, []string{"/repos/o/r/git/ref/tags/" + upperSHA, "/repos/o/r/git/ref/heads/" + upperSHA, "/repos/o/r/commits/" + upperSHA}) {
		t.Fatalf("Resolve() = %#v, %v; calls = %v", resolved, err, calls)
	}
}

func TestResolverFallbackErrorsAndCancellation(t *testing.T) {
	t.Run("rate limit", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(403)
		}))
		defer ts.Close()
		resolver, _ := NewResolver(ts.Client(), WithTestEndpoints(ts.URL))
		ref, _ := Parse("o/r@v1")
		var rate *RateLimitError
		if _, err := resolver.Resolve(context.Background(), ref); !errors.As(err, &rate) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ioString(w, "{") }))
		defer ts.Close()
		resolver, _ := NewResolver(ts.Client(), WithTestEndpoints(ts.URL))
		ref, _ := Parse("o/r@v1")
		if _, err := resolver.Resolve(context.Background(), ref); err == nil {
			t.Fatal("expected malformed response error")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
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
	c.maxFile, c.maxExpanded, c.maxEntries = 3, 6, 3
	tests := []struct {
		name    string
		entries []tar.Header
	}{
		{"traversal", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/../x", Typeflag: tar.TypeReg}}},
		{"link", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeSymlink, Linkname: "y"}}},
		{"hardlink", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeLink, Linkname: "root/y"}}},
		{"fifo", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeFifo}}},
		{"device", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeChar}}},
		{"duplicate", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeReg}, {Name: "root/X", Typeflag: tar.TypeReg}}},
		{"mixed roots", []tar.Header{{Name: "one/", Typeflag: tar.TypeDir}, {Name: "two/", Typeflag: tar.TypeDir}}},
		{"file size", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/x", Typeflag: tar.TypeReg, Size: 4}}},
		{"count", []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/a", Typeflag: tar.TypeDir}, {Name: "root/b", Typeflag: tar.TypeDir}, {Name: "root/c", Typeflag: tar.TypeDir}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := extractTar(bytes.NewReader(tarBytes(t, tt.entries)), filepath.Join(t.TempDir(), "out"), c); err == nil {
				t.Fatal("expected rejection")
			}
		})
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
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	digests := make([]string, 0, 2)
	for _, mask := range []int{0o022, 0o077} {
		syscall.Umask(mask)
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
	first, err := store.Materialize(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(first.ActionRoot, "action.yml")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.SourceDigest, "sha256:") || first.RepositoryRoot == first.ActionRoot {
		t.Fatalf("materialized identity = %#v", first)
	}
	second, err := store.Materialize(context.Background(), resolved)
	if err != nil || second != first || requests.Load() != 1 {
		t.Fatalf("second = %q, %v; requests=%d", second, err, requests.Load())
	}
}

func TestStoreExpectedDigestAndManifestTamperFailClosed(t *testing.T) {
	archive := tgz(t, []tar.Header{{Name: "root/", Typeflag: tar.TypeDir}, {Name: "root/action.yml", Typeflag: tar.TypeReg, Size: 1}})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	defer ts.Close()
	root := t.TempDir()
	store, _ := NewStore(root, ts.Client(), WithTestEndpoints(ts.URL, ts.URL))
	ref, _ := Parse("Owner/Repo@v1")
	r := Resolved{Reference: ref, Commit: testSHA}
	got, err := store.Materialize(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.SourceDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err = store.Materialize(context.Background(), r); err == nil || !strings.Contains(err.Error(), "source digest mismatch") {
		t.Fatalf("digest mismatch error = %v", err)
	}
	manifestPath := filepath.Join(filepath.Dir(got.RepositoryRoot), manifestName)
	if err = os.WriteFile(manifestPath, []byte(`{"schema":"forged"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.SourceDigest = got.SourceDigest
	if _, err = store.Materialize(context.Background(), r); err == nil || !strings.Contains(err.Error(), "verify action source cache") {
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
			_, err := store.Materialize(context.Background(), Resolved{Reference: ref, Commit: testSHA})
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
	store, err := NewStore(t.TempDir(), apiServer.Client(), WithTestEndpoints(apiServer.URL, archiveServer.URL), WithScopedGitHubToken(token, "pipeline/repo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	if apiRequests != 0 || archiveRequests != 1 {
		t.Fatalf("API/archive requests = %d / %d, want 0 / 1", apiRequests, archiveRequests)
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
		store, err := NewStore(t.TempDir(), server.Client(), WithTestEndpoints(server.URL, server.URL), WithScopedGitHubToken("test-token", "pipeline/repo"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Materialize(context.Background(), resolved); err != nil {
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
		if _, err := store.Materialize(context.Background(), resolved); err == nil || !strings.Contains(err.Error(), "archive redirect denied") {
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
	release := make(chan struct{})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 2 {
			close(release)
		}
		<-release
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
			got, err := store.Materialize(context.Background(), resolved)
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
	}
}
