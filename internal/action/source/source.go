// Package source fetches immutable GitHub repository source archives.
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
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	defaultAPI      = "https://api.github.com"
	defaultCodeload = "https://codeload.github.com"
	manifestName    = "manifest-v1.json"
	mutableRefTTL   = time.Hour
)

var (
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,98}[A-Za-z0-9])?$`)
	shaRE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Reference is a parsed remote repository reference. RepositoryRoot asks a
// RepositorySource to materialize the complete tree while retaining Path as
// the exact requested resource for authorization and cache isolation.
type Reference struct {
	Owner, Repository, Path, Ref, Raw string
	RepositoryRoot                    bool
	authorizationRef                  string
}

// Resolved pins a requested reference to an immutable commit.
type Resolved struct {
	Reference    Reference
	Commit       string
	SourceDigest string
}

// Materialized identifies the immutable repository tree and selected source.
type Materialized struct {
	RepositoryRoot string
	ActionRoot     string
	SourceDigest   string
	lease          *materializedLease
}

// Release marks the materialized tree as no longer in use. Callers must
// release persistent-cache entries after reading them.
func (m Materialized) Release() {
	if m.lease != nil {
		m.lease.release()
	}
}

// Retain acquires another lease for a memoized materialization.
func (m Materialized) Retain(ctx context.Context) (Materialized, error) {
	if m.lease == nil {
		return m, nil
	}
	return m.lease.retain(ctx)
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
	if len(parts) < 2 || len(parts[0])+1+len(parts[1]) > 140 || !ownerRE.MatchString(parts[0]) || !repoRE.MatchString(parts[1]) || strings.HasSuffix(parts[1], ".git") {
		return Reference{}, fmt.Errorf("invalid GitHub owner/repository")
	}
	for _, s := range append(parts[2:], strings.Split(ref, "/")...) {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return Reference{}, fmt.Errorf("invalid action reference segment")
		}
	}
	return Reference{Owner: parts[0], Repository: parts[1], Path: strings.Join(parts[2:], "/"), Ref: ref, Raw: raw}, nil
}

// PinReference replaces a mutable ref with an immutable commit while retaining
// the originally requested ref for repository-source authorization.
func PinReference(ref Reference, commit string) (Reference, error) {
	if !shaRE.MatchString(commit) {
		return Reference{}, fmt.Errorf("commit must be lower-case full SHA")
	}
	raw := ref.Owner + "/" + ref.Repository
	if ref.Path != "" {
		raw += "/" + ref.Path
	}
	pinned, err := Parse(raw + "@" + commit)
	if err != nil {
		return Reference{}, err
	}
	pinned.RepositoryRoot = ref.RepositoryRoot
	pinned.authorizationRef = ref.Ref
	if ref.authorizationRef != "" {
		pinned.authorizationRef = ref.authorizationRef
	}
	return pinned, nil
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
	codeload                            *url.URL
	finalHosts                          map[string]bool
	credential                          *actionSourceCredential
	git                                 string
	mutableRefs                         *mutableRefCache
	resolutionSnapshot                  *actionResolutionSnapshot
	cacheMaxBytes                       int64
	maxCompressed, maxExpanded, maxFile int64
	maxEntries                          int
	maxPath, maxSegment                 int
}

func defaults() config {
	api, _ := url.Parse(defaultAPI)
	codeload, _ := url.Parse(defaultCodeload)
	return config{api: api, codeload: codeload, finalHosts: map[string]bool{"codeload.github.com": true}, maxCompressed: 100 << 20, maxExpanded: 512 << 20, maxFile: 100 << 20, maxEntries: 50000, maxPath: 4096, maxSegment: 255}
}

// Option configures trusted process-level limits or test endpoints. Options must
// never be populated from workflow input.
type Option func(*config) error

// WithGitHubActionSourceTokenProvider authenticates mutable-ref API requests using a
// credential provisioned at the first such request and cached for this client.
// Returning an empty token selects anonymous resolution. Full lowercase SHAs
// never invoke the provider.
func WithGitHubActionSourceTokenProvider(repository string, provider func(context.Context) (string, error)) Option {
	return func(c *config) error {
		if provider == nil || !validRepository(repository) {
			return fmt.Errorf("invalid GitHub action source credential provider")
		}
		c.credential = &actionSourceCredential{repository: strings.ToLower(repository), provider: provider}
		return nil
	}
}

// WithGitHubAPITokenProvider authenticates public action resolution without a
// repository-scoped token exception. It is intended for explicit local tools.
func WithGitHubAPITokenProvider(provider func(context.Context) (string, error)) Option {
	return func(c *config) error {
		if provider == nil {
			return fmt.Errorf("invalid GitHub API credential provider")
		}
		c.credential = &actionSourceCredential{provider: provider}
		return nil
	}
}

// WithGitRepositorySource enables Git fallback for references explicitly
// marked RepositoryRoot. Git inherits the process's trusted credential
// helpers and environment. Other references remain on the public source path,
// so this option cannot grant private action access.
func WithGitRepositorySource(executable string) Option {
	return func(c *config) error {
		if executable == "" || !filepath.IsAbs(executable) {
			return fmt.Errorf("git repository source requires an absolute Git executable")
		}
		info, err := os.Stat(executable)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("git repository source executable is not an executable regular file")
		}
		c.git = executable
		return nil
	}
}

func validRepository(repository string) bool {
	parts := strings.Split(repository, "/")
	return len(parts) == 2 && ownerRE.MatchString(parts[0]) && repoRE.MatchString(parts[1]) && !strings.HasSuffix(parts[1], ".git")
}

type actionSourceCredential struct {
	repository string
	provider   func(context.Context) (string, error)
	once       sync.Once
	token      string
	err        error
}

func (c *actionSourceCredential) provision(ctx context.Context) error {
	if c == nil || c.provider == nil {
		return nil
	}
	c.once.Do(func() { c.token, c.err = c.provider(ctx) })
	return c.err
}

// WithTestEndpoints replaces the API base and, when supplied, the direct
// codeload base. It exists for hermetic tests; production callers should use
// defaults.
func WithTestEndpoints(apiBase string, codeloadBase ...string) Option {
	return func(c *config) error {
		u, err := url.Parse(apiBase)
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
			return fmt.Errorf("invalid API base")
		}
		c.api = u
		if len(codeloadBase) > 1 {
			return fmt.Errorf("only one codeload base may be specified")
		}
		if len(codeloadBase) == 1 {
			archive, err := url.Parse(codeloadBase[0])
			if err != nil || archive.Scheme != "https" || archive.Host == "" || archive.User != nil || archive.RawQuery != "" || archive.Fragment != "" {
				return fmt.Errorf("invalid codeload base")
			}
			c.codeload = archive
			c.finalHosts = map[string]bool{strings.ToLower(archive.Host): true}
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

// WithCacheMaxBytes bounds the immutable action-source cache. A materialized
// entry remains protected from eviction until its lease is released.
func WithCacheMaxBytes(maxBytes int64) Option {
	return func(c *config) error {
		if maxBytes <= 0 {
			return fmt.Errorf("cache maximum bytes must be positive")
		}
		c.cacheMaxBytes = maxBytes
		return nil
	}
}

// Resolver resolves GitHub references, optionally using credentials only for
// GitHub API requests. Requests discard the client's cookie jar.
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
	if c.mutableRefs == nil && c.resolutionSnapshot == nil && c.api.String() == defaultAPI {
		if root, cacheErr := os.UserCacheDir(); cacheErr == nil {
			c.mutableRefs, _ = newMutableRefCache(filepath.Join(root, "buildkite-gha", "action-ref-resolutions", "v1"), mutableRefTTL)
		}
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
	parsed.RepositoryRoot = ref.RepositoryRoot
	parsed.authorizationRef = ref.authorizationRef
	ref = parsed
	if shaRE.MatchString(ref.Ref) {
		return Resolved{Reference: ref, Commit: ref.Ref}, nil
	}
	if r.cfg.resolutionSnapshot != nil {
		return r.cfg.resolutionSnapshot.resolve(ctx, ref, r.resolveMutable)
	}
	if r.cfg.mutableRefs != nil {
		return r.cfg.mutableRefs.resolve(ctx, ref, r.resolveMutable)
	}
	return r.resolveMutable(ctx, ref)
}

// ResolutionSnapshotID identifies the immutable mutable-ref generation.
func (r *Resolver) ResolutionSnapshotID() string {
	if r == nil || r.cfg.resolutionSnapshot == nil {
		return ""
	}
	return r.cfg.resolutionSnapshot.generation
}

func (r *Resolver) resolveMutable(ctx context.Context, ref Reference) (Resolved, error) {
	if err := r.cfg.credential.provision(ctx); err != nil {
		return Resolved{}, err
	}
	if r.cfg.credential != nil && r.cfg.credential.token != "" {
		if err := ensurePublic(ctx, r.client, r.cfg, ref); err != nil {
			var notPublic *NotPublicError
			if !errors.As(err, &notPublic) || !ref.RepositoryRoot || r.cfg.git == "" {
				return Resolved{}, err
			}
			return resolveWithGit(ctx, r.cfg, ref)
		}
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
	resolved, err := r.resolveCommit(ctx, ref)
	var notPublic *NotPublicError
	if errors.As(err, &notPublic) && ref.RepositoryRoot && r.cfg.git != "" {
		return resolveWithGit(ctx, r.cfg, ref)
	}
	return resolved, err
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
	for range 5 {
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
	return githubAPIGet(ctx, r.client, r.cfg, parts, out)
}
func githubAPIGet(ctx context.Context, client *http.Client, cfg config, parts []string, out any) error {
	u := apiURL(cfg.api, parts...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	setAPIHeaders(req, actionSourceToken(cfg, parts))
	c := *client
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	lr := io.LimitReader(resp.Body, 1<<20+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return fmt.Errorf("GitHub API response too large")
	}
	if resp.StatusCode == http.StatusNotFound {
		return &NotPublicError{}
	}
	if rate := rateLimitError(resp, body); rate != nil {
		return rate
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &NotPublicError{}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("malformed GitHub API response: %w", err)
	}
	return nil
}
func ensurePublic(ctx context.Context, client *http.Client, cfg config, ref Reference) error {
	var repository struct {
		Private    *bool   `json:"private"`
		Visibility *string `json:"visibility"`
	}
	if err := githubAPIGet(ctx, client, cfg, repoParts(ref), &repository); err != nil {
		return err
	}
	if repository.Private == nil || repository.Visibility == nil {
		return fmt.Errorf("malformed GitHub API response: repository visibility is missing")
	}
	if *repository.Private || *repository.Visibility != "public" {
		return &NotPublicError{}
	}
	return nil
}
func actionSourceToken(cfg config, parts []string) string {
	if cfg.credential == nil {
		return ""
	}
	if len(parts) >= 3 && parts[0] == "repos" && strings.EqualFold(parts[1]+"/"+parts[2], cfg.credential.repository) {
		return ""
	}
	return cfg.credential.token
}

func resolveWithGit(ctx context.Context, cfg config, ref Reference) (Resolved, error) {
	repository, cleanup, err := fetchWithGit(ctx, cfg, ref, ref.Ref)
	if err != nil {
		return Resolved{}, err
	}
	defer cleanup()
	commit, err := gitCommit(ctx, cfg.git, repository)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Reference: ref, Commit: commit}, nil
}

func fetchWithGit(ctx context.Context, cfg config, ref Reference, requestedRef string) (string, func(), error) {
	cleanup := func() {}
	if !ref.RepositoryRoot || cfg.git == "" {
		return "", cleanup, &NotPublicError{}
	}
	root, err := os.MkdirTemp("", "buildkite-gha-repository-source-")
	if err != nil {
		return "", cleanup, fmt.Errorf("create Git repository source: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(root) }
	repository := filepath.Join(root, "repository.git")
	if err := runGit(ctx, cfg.git, "", io.Discard, "init", "--bare", "--quiet", repository); err != nil {
		cleanup()
		if ctx.Err() != nil {
			return "", func() {}, ctx.Err()
		}
		return "", func() {}, fmt.Errorf("initialize Git repository source")
	}
	remote := "https://github.com/" + ref.Owner + "/" + ref.Repository + ".git"
	refs := []string{"refs/tags/" + requestedRef, "refs/heads/" + requestedRef, requestedRef}
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	fetched := false
	for _, candidate := range refs {
		err = runGit(fetchCtx, cfg.git, repository, io.Discard,
			"fetch", "--quiet", "--force", "--no-tags", "--depth=1", "--no-recurse-submodules", "--", remote, candidate)
		if err == nil {
			fetched = true
			break
		}
		if fetchCtx.Err() != nil {
			cleanup()
			if ctx.Err() != nil {
				return "", func() {}, ctx.Err()
			}
			return "", func() {}, fmt.Errorf("fetch Git repository source: %w", fetchCtx.Err())
		}
	}
	if !fetched {
		cleanup()
		return "", func() {}, &NotPublicError{}
	}
	if err := gitObjectLimit(repository, cfg.maxCompressed); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return repository, cleanup, nil
}

func gitCommit(ctx context.Context, executable, repository string) (string, error) {
	var output bytes.Buffer
	if err := runGit(ctx, executable, repository, &output, "rev-parse", "--verify", "FETCH_HEAD^{commit}"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &NotPublicError{}
	}
	commit := strings.TrimSpace(output.String())
	if !shaRE.MatchString(commit) {
		return "", fmt.Errorf("git returned malformed commit SHA")
	}
	return commit, nil
}

func gitObjectLimit(repository string, limit int64) error {
	var total int64
	err := filepath.WalkDir(filepath.Join(repository, "objects"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > limit {
			return fmt.Errorf("git repository source exceeds compressed size limit")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func runGit(ctx context.Context, executable, repository string, stdout io.Writer, args ...string) error {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "credential.interactive=false"}
	if repository != "" {
		base = append(base, "-C", repository)
	}
	cmd := exec.CommandContext(ctx, executable, append(base, args...)...)
	cmd.Env = gitEnvironment()
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func gitEnvironment() []string {
	environment := os.Environ()
	filtered := environment[:0]
	for _, value := range environment {
		if strings.HasPrefix(value, "GIT_TERMINAL_PROMPT=") ||
			strings.HasPrefix(value, "GCM_INTERACTIVE=") ||
			strings.HasPrefix(value, "GIT_TRACE=") ||
			strings.HasPrefix(value, "GIT_TRACE_") ||
			strings.HasPrefix(value, "GIT_CURL_VERBOSE=") ||
			strings.HasPrefix(value, "GCM_TRACE=") ||
			strings.HasPrefix(value, "GCM_TRACE_") {
			continue
		}
		filtered = append(filtered, value)
	}
	return append(filtered, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
}
func rateLimitError(resp *http.Response, body []byte) error {
	if resp.StatusCode != http.StatusTooManyRequests && (resp.StatusCode != http.StatusForbidden || (resp.Header.Get("X-RateLimit-Remaining") != "0" && !strings.Contains(strings.ToLower(string(body)), "rate limit"))) {
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
func setAPIHeaders(r *http.Request, token string) {
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("User-Agent", "buildkite-gha-action-source/1")
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// Store downloads exact public commits directly from codeload and atomically
// caches them in an existing real directory. A Store must not be shared by
// mutually untrusted processes writing the same cache root.
type Store struct {
	root     string
	client   *http.Client
	cfg      config
	pressure atomic.Bool
}

func NewStore(root string, client *http.Client, opts ...Option) (*Store, error) {
	return NewStoreContext(context.Background(), root, client, opts...)
}

func NewStoreContext(ctx context.Context, root string, client *http.Client, opts ...Option) (*Store, error) {
	c, e := makeConfig(opts)
	if e != nil {
		return nil, e
	}
	if root == "" {
		return nil, fmt.Errorf("cache root is required")
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return nil, fmt.Errorf("resolve cache root: %w", e)
	}
	info, e := os.Lstat(root)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("cache root is not a non-symlink directory")
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return nil, fmt.Errorf("canonicalize cache root: %w", e)
	}
	canonicalInfo, e := os.Stat(root)
	if e != nil || !os.SameFile(info, canonicalInfo) {
		return nil, fmt.Errorf("cache root changed while canonicalizing")
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	store := &Store{root: root, client: client, cfg: c}
	if c.cacheMaxBytes > 0 {
		if err := store.maintain(ctx); err != nil {
			return nil, fmt.Errorf("maintain action source cache: %w", err)
		}
	}
	return store, nil
}

// Materialize returns the verified repository and selected source identity.
func (s *Store) Materialize(ctx context.Context, resolved Resolved) (Materialized, error) {
	if !shaRE.MatchString(resolved.Commit) {
		return Materialized{}, fmt.Errorf("commit must be lower-case full SHA")
	}
	parsed, err := Parse(resolved.Reference.Raw)
	if err != nil {
		return Materialized{}, err
	}
	parsed.RepositoryRoot = resolved.Reference.RepositoryRoot
	parsed.authorizationRef = resolved.Reference.authorizationRef
	resolved.Reference = parsed
	selectedPath := parsed.Path
	if parsed.RepositoryRoot {
		selectedPath = ""
	}
	base := filepath.Join(s.root, strings.ToLower(parsed.Owner), strings.ToLower(parsed.Repository), resolved.Commit)
	tree := filepath.Join(base, "tree")
	parent := filepath.Dir(base)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Materialized{}, err
	}
	entryLock, err := lockActionCache(ctx, base+".lock", actionCacheLockShared, false)
	if err != nil {
		return Materialized{}, err
	}
	m, verifyErr := s.verify(base, resolved)
	if verifyErr == nil {
		if err := s.authorizeCachedRepository(ctx, resolved.Reference, m); err != nil {
			entryLock.unlock()
			return Materialized{}, err
		}
		s.touch(base)
		return s.materializedLease(ctx, entryLock, resolved, tree, selectedPath, m.Digest)
	}
	if _, statErr := os.Stat(base); statErr == nil {
		// The initial verification may have raced a publisher between its
		// manifest and tree operations. Once base exists, verify the complete
		// publication again before treating it as corrupt.
		if m, retryErr := s.verify(base, resolved); retryErr == nil {
			if err := s.authorizeCachedRepository(ctx, resolved.Reference, m); err != nil {
				entryLock.unlock()
				return Materialized{}, err
			}
			s.touch(base)
			return s.materializedLease(ctx, entryLock, resolved, tree, selectedPath, m.Digest)
		}
		entryLock.unlock()
		return Materialized{}, fmt.Errorf("verify action source cache: %w", verifyErr)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		entryLock.unlock()
		return Materialized{}, fmt.Errorf("stat action source cache: %w", statErr)
	}
	entryLock.unlock()
	if err := ctx.Err(); err != nil {
		return Materialized{}, err
	}
	entryLock, cached, err := s.lockMissingEntry(ctx, base, tree, selectedPath, resolved)
	if err != nil {
		return Materialized{}, err
	}
	if cached != nil {
		return *cached, nil
	}
	defer func() {
		if entryLock != nil {
			entryLock.unlock()
		}
	}()
	if m, verifyErr = s.verify(base, resolved); verifyErr == nil {
		if err := s.authorizeCachedRepository(ctx, resolved.Reference, m); err != nil {
			return Materialized{}, err
		}
		exclusive := entryLock
		entryLock = nil
		return s.reacquireMaterializedLease(ctx, exclusive, resolved, base, tree, selectedPath)
	}
	tmp, partialLock, err := s.createPartial(ctx, parent)
	if err != nil {
		return Materialized{}, err
	}
	defer partialLock.unlock()
	defer func() { _ = os.RemoveAll(tmp) }()
	authenticated, err := s.downloadExtract(ctx, resolved, filepath.Join(tmp, "tree"))
	if err != nil {
		return Materialized{}, err
	}
	if _, err = selected(filepath.Join(tmp, "tree"), selectedPath); err != nil {
		return Materialized{}, err
	}
	m, err = buildManifest(filepath.Join(tmp, "tree"), s.cfg, parsed, resolved.Commit)
	if err != nil {
		return Materialized{}, err
	}
	m.Authenticated = authenticated
	if resolved.SourceDigest != "" && resolved.SourceDigest != m.Digest {
		return Materialized{}, fmt.Errorf("source digest mismatch: expected %s, got %s", resolved.SourceDigest, m.Digest)
	}
	data, _ := json.Marshal(m)
	if err = os.WriteFile(filepath.Join(tmp, manifestName), data, 0o644); err != nil {
		return Materialized{}, err
	}
	if err = s.publishCacheEntry(ctx, tmp, base); err != nil {
		m, verifyErr = s.verify(base, resolved)
		if verifyErr != nil {
			return Materialized{}, fmt.Errorf("publish cache: %w (existing cache invalid: %v)", err, verifyErr)
		}
	}
	exclusive := entryLock
	entryLock = nil
	return s.reacquireMaterializedLease(ctx, exclusive, resolved, base, tree, selectedPath)
}

func (s *Store) lockMissingEntry(ctx context.Context, base, tree, actionPath string, resolved Resolved) (*actionCacheLock, *Materialized, error) {
	for {
		exclusive, err := lockActionCache(ctx, base+".lock", actionCacheLockExclusive, true)
		if err == nil {
			return exclusive, nil, nil
		}
		if !errors.Is(err, errActionCacheLockUnavailable) {
			return nil, nil, err
		}
		shared, sharedErr := lockActionCache(ctx, base+".lock", actionCacheLockShared, true)
		if sharedErr == nil {
			m, verifyErr := s.verify(base, resolved)
			if verifyErr == nil {
				if authErr := s.authorizeCachedRepository(ctx, resolved.Reference, m); authErr != nil {
					shared.unlock()
					return nil, nil, authErr
				}
				s.touch(base)
				materialized, leaseErr := s.materializedLease(ctx, shared, resolved, tree, actionPath, m.Digest)
				return nil, &materialized, leaseErr
			}
			shared.unlock()
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *Store) reacquireMaterializedLease(ctx context.Context, exclusive *actionCacheLock, resolved Resolved, base, tree, actionPath string) (Materialized, error) {
	exclusive.unlock()
	shared, err := lockActionCache(ctx, base+".lock", actionCacheLockShared, false)
	if err != nil {
		return Materialized{}, err
	}
	m, err := s.verify(base, resolved)
	if err != nil {
		shared.unlock()
		return s.Materialize(ctx, resolved)
	}
	s.touch(base)
	return s.materializedLease(ctx, shared, resolved, tree, actionPath, m.Digest)
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

func (s *Store) authorizeCachedRepository(ctx context.Context, ref Reference, m manifest) error {
	if !m.Authenticated {
		return nil
	}
	if !ref.RepositoryRoot || s.cfg.git == "" {
		return &NotPublicError{}
	}
	requestedRef := ref.Ref
	if ref.authorizationRef != "" {
		requestedRef = ref.authorizationRef
	}
	repository, cleanup, err := fetchWithGit(ctx, s.cfg, ref, requestedRef)
	if err != nil {
		return err
	}
	defer cleanup()
	commit, err := gitCommit(ctx, s.cfg.git, repository)
	if err != nil {
		return err
	}
	if commit != m.Commit {
		return fmt.Errorf("repository source changed while resolving immutable commit")
	}
	return nil
}

func (s *Store) downloadExtract(ctx context.Context, r Resolved, dst string) (bool, error) {
	u := apiURL(s.cfg.codeload, r.Reference.Owner, r.Reference.Repository, "tar.gz", r.Commit)
	if !validArchiveURL(u, s.cfg.finalHosts) {
		return false, fmt.Errorf("archive URL denied")
	}
	err := s.downloadArchive(ctx, u, dst)
	var notPublic *NotPublicError
	if !errors.As(err, &notPublic) || !r.Reference.RepositoryRoot || s.cfg.git == "" {
		return false, err
	}
	if err := s.extractGitRepository(ctx, r, dst); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) extractGitRepository(ctx context.Context, resolved Resolved, dst string) error {
	requestedRef := resolved.Reference.Ref
	if resolved.Reference.authorizationRef != "" {
		requestedRef = resolved.Reference.authorizationRef
	}
	repository, cleanup, err := fetchWithGit(ctx, s.cfg, resolved.Reference, requestedRef)
	if err != nil {
		return err
	}
	defer cleanup()
	commit, err := gitCommit(ctx, s.cfg.git, repository)
	if err != nil {
		return err
	}
	if commit != resolved.Commit {
		return fmt.Errorf("repository source changed while resolving immutable commit")
	}
	args := []string{"-c", "core.hooksPath=/dev/null", "-C", repository, "archive", "--format=tar", "--prefix=repository/", commit}
	cmd := exec.CommandContext(ctx, s.cfg.git, args...)
	cmd.Env = gitEnvironment()
	cmd.Stderr = io.Discard
	archive, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Git repository archive")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Git repository archive")
	}
	extractErr := extractTar(archive, dst, s.cfg)
	_ = archive.Close()
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("archive Git repository source")
	}
	return nil
}

func (s *Store) downloadArchive(ctx context.Context, u *url.URL, dst string) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return e
	}
	req.Header.Del("Authorization")
	req.Header.Del("Cookie")
	req.Header.Set("User-Agent", "buildkite-gha-action-source/1")
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
	defer func() { _ = resp.Body.Close() }()
	if !validArchiveURL(resp.Request.URL, s.cfg.finalHosts) {
		return fmt.Errorf("archive final URL denied")
	}
	if resp.StatusCode == http.StatusNotFound {
		return &NotPublicError{}
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return fmt.Errorf("read archive error response: %w", readErr)
		}
		if rate := rateLimitError(resp, body); rate != nil {
			return rate
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &NotPublicError{}
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
	Schema        string         `json:"schema"`
	Owner         string         `json:"owner"`
	Repository    string         `json:"repository"`
	Commit        string         `json:"commit"`
	Files         []manifestFile `json:"files"`
	Digest        string         `json:"digest"`
	Authenticated bool           `json:"authenticated,omitempty"`
}
type manifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

// DigestTree returns the canonical source digest for a local action directory.
// It uses the same bounded manifest and file-mode model as immutable remote
// action source. It rejects symlinks and other special files.
func DigestTree(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("stat action source tree: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("action source tree is not a directory")
	}
	m, err := buildManifestFiles(root, defaults(), Reference{}, "", true)
	if err != nil {
		return "", fmt.Errorf("digest action source tree: %w", err)
	}
	return m.Digest, nil
}

func buildManifest(root string, c config, ref Reference, commit string) (manifest, error) {
	return buildManifestFiles(root, c, ref, commit, false)
}

func buildManifestFiles(root string, c config, ref Reference, commit string, excludeGitMetadata bool) (manifest, error) {
	m := manifest{Schema: "buildkite-gha-action-source/v1", Owner: strings.ToLower(ref.Owner), Repository: strings.ToLower(ref.Repository), Commit: commit}
	var total int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(root, p)
		if excludeGitMetadata && rel == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeType != 0 {
			return fmt.Errorf("cache contains special file")
		}
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
		_ = f.Close()
		if e != nil {
			return e
		}
		mode := uint32(0o644)
		if st.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		m.Files = append(m.Files, manifestFile{Path: rel, Size: st.Size(), SHA256: hex.EncodeToString(h.Sum(nil)), Mode: mode})
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
	got.Authenticated = want.Authenticated
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
	type omittedSymlink struct {
		path, target string
	}
	var symlinks []omittedSymlink
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
		if !utf8.ValidString(h.Name) || !utf8.ValidString(h.Linkname) {
			return fmt.Errorf("archive contains invalid UTF-8 metadata")
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			// archive/tar still exposes legacy xattrs separately from PAX records.
			if h.Xattrs != nil || h.Linkname != "" || !benignPAX(h.PAXRecords, false) { //nolint:staticcheck
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
		// Check the deprecated field because callers can construct Header values
		// with legacy xattrs that are not represented in PAXRecords.
		if h.Xattrs != nil || (h.Linkname != "" && h.Typeflag != tar.TypeSymlink) || !benignPAX(h.PAXRecords, h.Typeflag == tar.TypeSymlink) { //nolint:staticcheck
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
		for _, link := range symlinks {
			if strings.HasPrefix(fold, strings.ToLower(link.path)+"/") {
				return fmt.Errorf("archive entry descends from an omitted symlink")
			}
		}
		if h.Typeflag == tar.TypeSymlink {
			for oldFold := range seen {
				if strings.HasPrefix(oldFold, fold+"/") {
					return fmt.Errorf("omitted symlink is an ancestor of an archive entry")
				}
			}
		}
		seen[fold] = rel
		out := filepath.Join(dst, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			e = os.MkdirAll(out, os.FileMode(h.Mode)&0o777|0o700)
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA remains valid input for legacy tar archives.
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
					switch {
					case ce != nil || n != h.Size:
						e = fmt.Errorf("short archive file")
					case cl != nil:
						e = cl
					default:
						mode := os.FileMode(0o644)
						if h.Mode&0o100 != 0 {
							mode = 0o755
						}
						e = os.Chmod(out, mode)
					}
				}
			}
		case tar.TypeSymlink:
			target, targetErr := omittedSymlinkTarget(rel, h.Linkname, h.Size, c)
			if targetErr != nil {
				return targetErr
			}
			symlinks = append(symlinks, omittedSymlink{path: rel, target: target})
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
	for _, link := range symlinks {
		alias := filepath.Join(dst, filepath.FromSlash(link.path))
		if _, err := os.Lstat(alias); !os.IsNotExist(err) {
			if err != nil {
				return err
			}
			return fmt.Errorf("omitted symlink path was materialized")
		}
		target, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(link.target)))
		if err != nil || !target.Mode().IsRegular() {
			return fmt.Errorf("omitted symlink target is not an extracted regular file")
		}
	}
	return nil
}

func omittedSymlinkTarget(name, linkname string, size int64, c config) (string, error) {
	if linkname == "" || size != 0 || len(linkname) > c.maxPath || strings.Contains(linkname, "\\") || hasControl(linkname) || path.IsAbs(linkname) {
		return "", fmt.Errorf("unsafe archive symlink target")
	}
	target := path.Clean(path.Join(path.Dir(name), linkname))
	if target == "." || target == ".." || strings.HasPrefix(target, "../") {
		return "", fmt.Errorf("archive symlink target escapes root")
	}
	for _, part := range strings.Split(target, "/") {
		if part == "" || part == "." || part == ".." || len(part) > c.maxSegment {
			return "", fmt.Errorf("unsafe archive symlink target")
		}
	}
	return target, nil
}

func benignPAX(records map[string]string, allowLinkpath bool) bool {
	for key := range records {
		switch key {
		case "path", "size", "mtime", "comment":
			// archive/tar has already applied path and size to Header fields;
			// those resolved fields undergo the normal validation above.
		case "linkpath":
			if !allowLinkpath {
				return false
			}
		default:
			// This rejects linkpath outside a symlink, GNU sparse metadata,
			// SCHILY xattrs, and all unknown extensions rather than guessing at
			// their semantics.
			return false
		}
	}
	return true
}
