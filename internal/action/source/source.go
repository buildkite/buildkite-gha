// Package source fetches immutable, public GitHub Action source archives.
package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultAPI   = "https://api.github.com"
	manifestName = "manifest-v1.json"
)

var (
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,98}[A-Za-z0-9])?$`)
	shaRE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Reference is a parsed remote action reference.
type Reference struct{ Owner, Repository, Path, Ref, Raw string }

// Resolved pins a requested reference to an immutable commit.
type Resolved struct {
	Reference    Reference
	Commit       string
	SourceDigest string
}

// Materialized identifies the immutable repository tree and selected action.
type Materialized struct {
	RepositoryRoot string
	ActionRoot     string
	SourceDigest   string
}

// Parse parses owner/repository[/path]@ref.
func Parse(raw string) (Reference, error) {
	if len(raw) == 0 || len(raw) > 2048 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\?#") || strings.Contains(raw, "${{") || hasControl(raw) {
		return Reference{}, fmt.Errorf("invalid action reference")
	}
	at := strings.LastIndexByte(raw, '@')
	if at <= 0 || at == len(raw)-1 {
		return Reference{}, fmt.Errorf("action reference must contain owner/repository@ref")
	}
	left, ref := raw[:at], raw[at+1:]
	if len(ref) > 1024 || strings.HasPrefix(left, "./") || strings.HasPrefix(left, "/") || strings.HasPrefix(strings.ToLower(left), "docker://") {
		return Reference{}, fmt.Errorf("invalid action reference")
	}
	parts := strings.Split(left, "/")
	if len(parts) < 2 || !ownerRE.MatchString(parts[0]) || !repoRE.MatchString(parts[1]) || strings.HasSuffix(parts[1], ".git") {
		return Reference{}, fmt.Errorf("invalid GitHub owner/repository")
	}
	for _, s := range append(parts[2:], strings.Split(ref, "/")...) {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return Reference{}, fmt.Errorf("invalid action reference segment")
		}
	}
	return Reference{Owner: parts[0], Repository: parts[1], Path: strings.Join(parts[2:], "/"), Ref: ref, Raw: raw}, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// RateLimitError reports anonymous GitHub API exhaustion.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return "GitHub API rate limit exceeded"
	}
	return "GitHub API rate limit exceeded until " + e.Reset.UTC().Format(time.RFC3339)
}

// NotPublicError deliberately does not distinguish missing and private repositories.
type NotPublicError struct{}

func (*NotPublicError) Error() string { return "GitHub action was not found or is not public" }

type config struct {
	api                                 *url.URL
	finalHosts                          map[string]bool
	maxCompressed, maxExpanded, maxFile int64
	maxEntries                          int
	maxPath, maxSegment                 int
}

func defaults() config {
	u, _ := url.Parse(defaultAPI)
	return config{u, map[string]bool{"codeload.github.com": true}, 100 << 20, 512 << 20, 100 << 20, 50000, 4096, 255}
}

// Option configures trusted process-level limits or test endpoints. Options must
// never be populated from workflow input.
type Option func(*config) error

// WithTestEndpoints replaces the API base and permitted archive final hosts.
// It exists for hermetic tests; production callers should use defaults.
func WithTestEndpoints(apiBase string, archiveFinalHosts ...string) Option {
	return func(c *config) error {
		u, err := url.Parse(apiBase)
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
			return fmt.Errorf("invalid API base")
		}
		c.api = u
		if len(archiveFinalHosts) > 0 {
			c.finalHosts = map[string]bool{}
			for _, h := range archiveFinalHosts {
				c.finalHosts[strings.ToLower(h)] = true
			}
		}
		return nil
	}
}

// WithLimits lowers or raises trusted archive limits.
func WithLimits(compressed, expanded, perFile int64, entries int) Option {
	return func(c *config) error {
		if compressed <= 0 || expanded <= 0 || perFile <= 0 || entries <= 0 {
			return fmt.Errorf("limits must be positive")
		}
		c.maxCompressed, c.maxExpanded, c.maxFile, c.maxEntries = compressed, expanded, perFile, entries
		return nil
	}
}

// Resolver resolves public GitHub references without credentials. An injected
// client must be credential-free; production callers should pass nil. Requests
// also discard the client's cookie jar and strip credential headers.
type Resolver struct {
	client *http.Client
	cfg    config
}

func NewResolver(client *http.Client, opts ...Option) (*Resolver, error) {
	c, err := makeConfig(opts)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Resolver{client, c}, nil
}
func makeConfig(opts []Option) (config, error) {
	c := defaults()
	for _, o := range opts {
		if err := o(&c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// Resolve pins a ref to a commit. Full lowercase SHA pins bypass mutable refs.
func (r *Resolver) Resolve(ctx context.Context, ref Reference) (Resolved, error) {
	parsed, err := Parse(ref.Raw)
	if err != nil {
		return Resolved{}, err
	}
	ref = parsed
	if shaRE.MatchString(ref.Ref) {
		return r.resolveCommit(ctx, ref)
	}
	for _, kind := range []string{"tags", "heads"} {
		var v struct {
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		err := r.get(ctx, repoParts(ref, "git", "ref", kind, ref.Ref), &v)
		if err == nil {
			sha, err := r.peel(ctx, ref, v.Object.Type, v.Object.SHA)
			if err != nil {
				return Resolved{}, err
			}
			return Resolved{Reference: ref, Commit: sha}, nil
		}
		var nf *NotPublicError
		if !errors.As(err, &nf) {
			return Resolved{}, err
		}
	}
	return r.resolveCommit(ctx, ref)
}
func (r *Resolver) resolveCommit(ctx context.Context, ref Reference) (Resolved, error) {
	var v struct {
		SHA string `json:"sha"`
	}
	if err := r.get(ctx, repoParts(ref, "commits", ref.Ref), &v); err != nil {
		return Resolved{}, err
	}
	if !shaRE.MatchString(v.SHA) {
		return Resolved{}, fmt.Errorf("GitHub returned malformed commit SHA")
	}
	return Resolved{Reference: ref, Commit: v.SHA}, nil
}
func (r *Resolver) peel(ctx context.Context, ref Reference, typ, sha string) (string, error) {
	for i := 0; i < 5; i++ {
		if !shaRE.MatchString(sha) {
			return "", fmt.Errorf("GitHub returned malformed object SHA")
		}
		if typ == "commit" {
			return sha, nil
		}
		if typ != "tag" {
			return "", fmt.Errorf("GitHub ref resolved to unsupported object type")
		}
		var v struct {
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		if err := r.get(ctx, repoParts(ref, "git", "tags", sha), &v); err != nil {
			return "", err
		}
		typ, sha = v.Object.Type, v.Object.SHA
	}
	return "", fmt.Errorf("annotated tag nesting exceeds limit")
}
func repoParts(r Reference, suffix ...string) []string {
	return append([]string{"repos", r.Owner, r.Repository}, suffix...)
}
func apiURL(base *url.URL, parts ...string) *url.URL {
	u := *base
	decoded := strings.TrimSuffix(base.Path, "/")
	escaped := strings.TrimSuffix(base.EscapedPath(), "/")
	for _, part := range parts {
		decoded += "/" + part
		escaped += "/" + url.PathEscape(part)
	}
	u.Path, u.RawPath = decoded, escaped
	return &u
}
func (r *Resolver) get(ctx context.Context, parts []string, out any) error {
	u := apiURL(r.cfg.api, parts...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	setHeaders(req)
	c := *r.client
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API request: %w", err)
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, 1<<20+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return fmt.Errorf("GitHub API response too large")
	}
	if resp.StatusCode == 404 {
		return &NotPublicError{}
	}
	if rate := rateLimitError(resp, body); rate != nil {
		return rate
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("malformed GitHub API response: %w", err)
	}
	return nil
}
func rateLimitError(resp *http.Response, body []byte) error {
	if resp.StatusCode != 429 && (resp.StatusCode != 403 || (resp.Header.Get("X-RateLimit-Remaining") != "0" && !strings.Contains(strings.ToLower(string(body)), "rate limit"))) {
		return nil
	}
	var reset time.Time
	if n, e := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil {
		reset = time.Unix(n, 0)
	} else if n, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); e == nil {
		reset = time.Now().Add(time.Duration(n) * time.Second)
	} else if t, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
		reset = t
	}
	return &RateLimitError{reset}
}
func setHeaders(r *http.Request) {
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	r.Header.Set("User-Agent", "buildkite-gha-action-source/1")
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// Store downloads and atomically caches repositories. A Store must not be
// shared by mutually untrusted processes writing the same cache root. An
// injected client must be credential-free; production callers should pass nil.
type Store struct {
	root   string
	client *http.Client
	cfg    config
}

func NewStore(root string, client *http.Client, opts ...Option) (*Store, error) {
	c, e := makeConfig(opts)
	if e != nil {
		return nil, e
	}
	if root == "" {
		return nil, fmt.Errorf("cache root is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Store{root, client, c}, nil
}

// Materialize returns the verified repository and selected action identity.
func (s *Store) Materialize(ctx context.Context, resolved Resolved) (Materialized, error) {
	if !shaRE.MatchString(resolved.Commit) {
		return Materialized{}, fmt.Errorf("commit must be lower-case full SHA")
	}
	parsed, err := Parse(resolved.Reference.Raw)
	if err != nil {
		return Materialized{}, err
	}
	resolved.Reference = parsed
	base := filepath.Join(s.root, strings.ToLower(parsed.Owner), strings.ToLower(parsed.Repository), resolved.Commit)
	tree := filepath.Join(base, "tree")
	m, verifyErr := s.verify(base, resolved)
	if verifyErr == nil {
		return materialized(tree, parsed.Path, m.Digest)
	}
	if _, statErr := os.Stat(base); statErr == nil {
		// The initial verification may have raced a publisher between its
		// manifest and tree operations. Once base exists, verify the complete
		// publication again before treating it as corrupt.
		if m, retryErr := s.verify(base, resolved); retryErr == nil {
			return materialized(tree, parsed.Path, m.Digest)
		}
		return Materialized{}, fmt.Errorf("verify action source cache: %w", verifyErr)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Materialized{}, fmt.Errorf("stat action source cache: %w", statErr)
	}
	if err := ctx.Err(); err != nil {
		return Materialized{}, err
	}
	parent := filepath.Dir(base)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Materialized{}, err
	}
	tmp, err := os.MkdirTemp(parent, ".partial-")
	if err != nil {
		return Materialized{}, err
	}
	defer os.RemoveAll(tmp)
	if err = s.downloadExtract(ctx, resolved, filepath.Join(tmp, "tree")); err != nil {
		return Materialized{}, err
	}
	if _, err = selected(filepath.Join(tmp, "tree"), parsed.Path); err != nil {
		return Materialized{}, err
	}
	m, err = buildManifest(filepath.Join(tmp, "tree"), s.cfg, parsed, resolved.Commit)
	if err != nil {
		return Materialized{}, err
	}
	if resolved.SourceDigest != "" && resolved.SourceDigest != m.Digest {
		return Materialized{}, fmt.Errorf("source digest mismatch: expected %s, got %s", resolved.SourceDigest, m.Digest)
	}
	data, _ := json.Marshal(m)
	if err = os.WriteFile(filepath.Join(tmp, manifestName), data, 0o644); err != nil {
		return Materialized{}, err
	}
	if err = os.Rename(tmp, base); err != nil {
		m, verifyErr = s.verify(base, resolved)
		if verifyErr != nil {
			return Materialized{}, fmt.Errorf("publish cache: %w (existing cache invalid: %v)", err, verifyErr)
		}
	}
	return materialized(tree, parsed.Path, m.Digest)
}
func materialized(tree, p, digest string) (Materialized, error) {
	action, err := selected(tree, p)
	if err != nil {
		return Materialized{}, err
	}
	return Materialized{RepositoryRoot: tree, ActionRoot: action, SourceDigest: digest}, nil
}
func selected(tree, p string) (string, error) {
	dst := tree
	if p != "" {
		dst = filepath.Join(tree, filepath.FromSlash(p))
	}
	rel, e := filepath.Rel(tree, dst)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("action path escapes repository")
	}
	st, e := os.Stat(dst)
	if e != nil || !st.IsDir() {
		return "", fmt.Errorf("selected action path does not exist or is not a directory")
	}
	return dst, nil
}

func (s *Store) downloadExtract(ctx context.Context, r Resolved, dst string) error {
	u := apiURL(s.cfg.api, repoParts(r.Reference, "tarball", r.Commit)...)
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return e
	}
	setHeaders(req)
	c := *s.client
	c.Jar = nil
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return fmt.Errorf("too many redirects")
		}
		if !validArchiveURL(req.URL, s.cfg.finalHosts) {
			return fmt.Errorf("archive redirect denied")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		return nil
	}
	resp, e := c.Do(req)
	if e != nil {
		return fmt.Errorf("download archive: %w", e)
	}
	defer resp.Body.Close()
	if !validArchiveURL(resp.Request.URL, s.cfg.finalHosts) {
		return fmt.Errorf("archive final URL denied")
	}
	if resp.StatusCode == 404 {
		return &NotPublicError{}
	}
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return fmt.Errorf("read archive error response: %w", readErr)
		}
		if rate := rateLimitError(resp, body); rate != nil {
			return rate
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("archive returned HTTP %d", resp.StatusCode)
	}
	limited := &io.LimitedReader{R: resp.Body, N: s.cfg.maxCompressed + 1}
	gz, e := gzip.NewReader(limited)
	if e != nil {
		return fmt.Errorf("open archive: %w", e)
	}
	e = extractTar(gz, dst, s.cfg)
	closeErr := gz.Close()
	if limited.N <= 0 {
		return fmt.Errorf("compressed archive exceeds limit")
	}
	if e != nil {
		return e
	}
	return closeErr
}

func validArchiveURL(u *url.URL, allowed map[string]bool) bool {
	if u.Scheme != "https" || u.User != nil {
		return false
	}
	// An authority (including a port) can only appear when explicitly injected
	// as a trusted test endpoint. Production's hostname-only allowlist rejects
	// custom ports and IP literals.
	if allowed[strings.ToLower(u.Host)] {
		return true
	}
	return u.Port() == "" && net.ParseIP(u.Hostname()) == nil && allowed[strings.ToLower(u.Hostname())]
}

type manifest struct {
	Schema     string         `json:"schema"`
	Owner      string         `json:"owner"`
	Repository string         `json:"repository"`
	Commit     string         `json:"commit"`
	Files      []manifestFile `json:"files"`
	Digest     string         `json:"digest"`
}
type manifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

func buildManifest(root string, c config, ref Reference, commit string) (manifest, error) {
	m := manifest{Schema: "buildkite-gha-action-source/v1", Owner: strings.ToLower(ref.Owner), Repository: strings.ToLower(ref.Repository), Commit: commit}
	var total int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeType != 0 {
			return fmt.Errorf("cache contains special file")
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		st, e := d.Info()
		if e != nil {
			return e
		}
		total += st.Size()
		if st.Size() > c.maxFile || total > c.maxExpanded || len(m.Files) >= c.maxEntries {
			return fmt.Errorf("cache exceeds limits")
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return e
		}
		m.Files = append(m.Files, manifestFile{Path: rel, Size: st.Size(), SHA256: hex.EncodeToString(h.Sum(nil)), Mode: uint32(st.Mode().Perm() & 0o777)})
		return nil
	})
	if err != nil {
		return m, err
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	b, _ := json.Marshal(m.Files)
	sum := sha256.Sum256(b)
	m.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return m, nil
}
func (s *Store) verify(base string, resolved Resolved) (manifest, error) {
	data, e := os.ReadFile(filepath.Join(base, manifestName))
	if e != nil || len(data) > 16<<20 {
		return manifest{}, fmt.Errorf("invalid manifest")
	}
	var want manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&want) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return manifest{}, fmt.Errorf("invalid manifest")
	}
	got, e := buildManifest(filepath.Join(base, "tree"), s.cfg, resolved.Reference, resolved.Commit)
	if e != nil || !reflect.DeepEqual(got, want) {
		return manifest{}, fmt.Errorf("cache verification failed")
	}
	if resolved.SourceDigest != "" && got.Digest != resolved.SourceDigest {
		return manifest{}, fmt.Errorf("source digest mismatch: expected %s, got %s", resolved.SourceDigest, got.Digest)
	}
	return got, nil
}

func extractTar(r io.Reader, dst string, c config) error {
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	seen := map[string]string{}
	root := ""
	var total int64
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return fmt.Errorf("read archive: %w", e)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			if h.Xattrs != nil || !benignPAX(h.PAXRecords) {
				return fmt.Errorf("archive extended metadata is not allowed")
			}
			continue
		}
		count++
		if count > c.maxEntries {
			return fmt.Errorf("archive entry count exceeds limit")
		}
		// A PAX linkpath is resolved by archive/tar into Linkname and may be
		// removed from PAXRecords, so reject it independently for every type.
		if h.Xattrs != nil || h.Linkname != "" || !benignPAX(h.PAXRecords) {
			return fmt.Errorf("archive extended metadata is not allowed")
		}
		name := h.Name
		if len(name) > c.maxPath || strings.Contains(name, "\\") || hasControl(name) || path.IsAbs(name) {
			return fmt.Errorf("unsafe archive path")
		}
		parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
		if len(parts) < 1 || parts[0] == "" {
			return fmt.Errorf("archive has no root")
		}
		if root == "" {
			root = parts[0]
		} else if root != parts[0] {
			return fmt.Errorf("archive has mixed roots")
		}
		for _, p := range parts {
			if p == "" || p == "." || p == ".." || len(p) > c.maxSegment {
				return fmt.Errorf("unsafe archive path")
			}
		}
		if len(parts) == 1 {
			if h.Typeflag != tar.TypeDir {
				return fmt.Errorf("archive root is not a directory")
			}
			continue
		}
		rel := strings.Join(parts[1:], "/")
		fold := strings.ToLower(rel)
		if old, ok := seen[fold]; ok {
			return fmt.Errorf("duplicate or case-colliding archive path %q and %q", old, rel)
		}
		seen[fold] = rel
		out := filepath.Join(dst, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			e = os.MkdirAll(out, os.FileMode(h.Mode)&0o777|0o700)
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > c.maxFile || total+h.Size > c.maxExpanded {
				return fmt.Errorf("archive size exceeds limit")
			}
			total += h.Size
			if e = os.MkdirAll(filepath.Dir(out), 0o755); e == nil {
				var f *os.File
				f, e = os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if e == nil {
					n, ce := io.CopyN(f, tr, h.Size)
					cl := f.Close()
					if ce != nil || n != h.Size {
						e = fmt.Errorf("short archive file")
					} else if cl != nil {
						e = cl
					} else {
						mode := os.FileMode(0o644)
						if h.Mode&0o100 != 0 {
							mode = 0o755
						}
						e = os.Chmod(out, mode)
					}
				}
			}
		default:
			return fmt.Errorf("archive entry type is not allowed")
		}
		if e != nil {
			return e
		}
	}
	if root == "" {
		return fmt.Errorf("empty archive")
	}
	return nil
}

func benignPAX(records map[string]string) bool {
	for key := range records {
		switch key {
		case "path", "size", "mtime", "comment":
			// archive/tar has already applied path and size to Header fields;
			// those resolved fields undergo the normal validation above.
		default:
			// This rejects linkpath, GNU sparse metadata, SCHILY xattrs, and
			// all unknown extensions rather than guessing at their semantics.
			return false
		}
	}
	return true
}
