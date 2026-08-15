package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

type fakeActionMaterializer struct {
	mu          sync.Mutex
	calls       int
	resolved    source.Resolved
	result      source.Materialized
	err         error
	materialize func(context.Context, source.Resolved) (source.Materialized, error)
}

func (f *fakeActionMaterializer) Materialize(ctx context.Context, r source.Resolved) (source.Materialized, error) {
	f.mu.Lock()
	f.calls++
	f.resolved = r
	f.mu.Unlock()
	if f.materialize != nil {
		return f.materialize(ctx, r)
	}
	return f.result, f.err
}

func writeAction(t *testing.T, root, path string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action.yml"), []byte("name: test\nruns:\n  using: node20\n  main: index.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func digestTree(t *testing.T, root string) string {
	t.Helper()
	d, err := source.DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func workflowJob(t *testing.T, workspace string) plan.Job {
	t.Helper()
	p := filepath.Join(workspace, ".github", "workflows", "test.yml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	b := []byte("jobs: {}\n")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return plan.Job{Workflow: plan.Workflow{Path: ".github/workflows/test.yml", Digest: "sha256:" + hex.EncodeToString(h[:])}}
}

func TestActionLockResolverGitHubExactSourceSingleFlightAndTampering(t *testing.T) {
	repo := t.TempDir()
	writeAction(t, repo, "nested")
	digest := digestTree(t, repo)
	commit := strings.Repeat("a", 40)
	job := plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{{ID: "lock", Source: "github", Repository: "owner/repo", RequestedRef: "do-not-use", Commit: commit, Path: "nested", SourceDigest: digest}}}
	fake := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: repo, ActionRoot: filepath.Join(repo, "wrong"), SourceDigest: digest}}
	r := newActionLockResolver(job, "", fake)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := r.resolve(context.Background(), plan.ActionSelector{Lock: "lock"}); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}()
	}
	wg.Wait()
	fake.mu.Lock()
	if fake.calls != 1 || fake.resolved.Commit != commit || fake.resolved.SourceDigest != digest || fake.resolved.Reference.Raw != "owner/repo/nested@"+commit || fake.resolved.Reference.Ref != commit {
		t.Errorf("materializer calls/resolved = %d / %#v", fake.calls, fake.resolved)
	}
	fake.mu.Unlock()
	if err := os.WriteFile(filepath.Join(repo, "nested", "index.js"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.resolve(context.Background(), plan.ActionSelector{Lock: "lock"}); err == nil {
		t.Fatal("resolve after repository tampering succeeded")
	}
}

func TestActionLockResolverHardensOnlyNonContextMaterializationFailures(t *testing.T) {
	commit := strings.Repeat("a", 40)
	lock := plan.ActionLock{ID: "lock", Source: "github", Repository: "owner/repo", Commit: commit, SourceDigest: "sha256:" + strings.Repeat("0", 64)}
	for _, tc := range []struct {
		name     string
		err      error
		wantHard bool
	}{
		{name: "step deadline", err: context.DeadlineExceeded},
		{name: "integrity error racing deadline", err: errors.New("cache digest mismatch"), wantHard: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 0)
			defer cancel()
			materializer := &fakeActionMaterializer{err: tc.err}

			_, _, err := newActionLockResolver(plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock}}, "", materializer).resolve(ctx, plan.ActionSelector{Lock: lock.ID})
			if !errors.Is(err, tc.err) || isHardJobFailure(err) != tc.wantHard {
				t.Fatalf("resolve() error = %v, want hard = %t", err, tc.wantHard)
			}
		})
	}
}

func TestActionLockResolverAllowsOnlyAuditedCacheCommitsAndEntryPoints(t *testing.T) {
	repo := t.TempDir()
	for _, path := range []string{"", "restore", "save"} {
		writeAction(t, repo, path)
	}
	digest := digestTree(t, repo)
	for version, commit := range map[string]string{
		"v4.3.0": actionintegration.CacheV4Commit,
		"v5.0.3": actionintegration.CacheV503Commit,
		"v5.1.0": actionintegration.CacheV5Commit,
		"v6.1.0": actionintegration.CacheCommit,
	} {
		for _, path := range []string{"", "restore", "save"} {
			name := version + "/root"
			if path != "" {
				name = version + "/" + path
			}
			t.Run(name, func(t *testing.T) {
				lock := plan.ActionLock{ID: "cache", Source: "github", Repository: "actions/cache", Commit: commit, Path: path, SourceDigest: digest}
				job := plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock}}
				materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: repo, SourceDigest: digest}}
				if _, resolved, err := newActionLockResolver(job, "", materializer).resolve(context.Background(), plan.ActionSelector{Lock: lock.ID}); err != nil || !reflect.DeepEqual(resolved, lock) {
					t.Fatalf("resolve() = %#v, %v", resolved, err)
				}
				if materializer.calls != 1 || materializer.resolved.Commit != commit || materializer.resolved.Reference.Path != path || materializer.resolved.SourceDigest != digest {
					t.Fatalf("materializer calls/resolved = %d / %#v", materializer.calls, materializer.resolved)
				}
			})
		}
	}

	lock := plan.ActionLock{ID: "cache", Source: "github", Repository: "actions/cache", Commit: strings.Repeat("0", 40), SourceDigest: digest}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: repo, SourceDigest: digest}}
	if _, _, err := newActionLockResolver(plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock}}, "", materializer).resolve(context.Background(), plan.ActionSelector{Lock: lock.ID}); err == nil {
		t.Fatal("unproved cache commit resolved")
	}
	if materializer.calls != 0 {
		t.Fatalf("unproved cache commit reached materializer %d times", materializer.calls)
	}
}

func TestActionLockResolverAllowsSymlinkedRepositoryAncestor(t *testing.T) {
	base := canonicalTempDir(t)
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalRoot := filepath.Join(base, "logical")
	if err := os.Symlink(realRoot, logicalRoot); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	repository := filepath.Join(logicalRoot, "repository")
	writeAction(t, repository, "nested")
	digest := digestTree(t, repository)
	commit := strings.Repeat("a", 40)
	job := plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{{
		ID: "lock", Source: "github", Repository: "owner/repo", Commit: commit, Path: "nested", SourceDigest: digest,
	}}}
	materializer := &fakeActionMaterializer{result: source.Materialized{
		RepositoryRoot: repository,
		ActionRoot:     filepath.Join(repository, "nested"),
		SourceDigest:   digest,
	}}
	resolved, _, err := newActionLockResolver(job, "", materializer).resolve(context.Background(), plan.ActionSelector{Lock: "lock"})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SourceRoot != canonicalRepository {
		t.Fatalf("resolved source root = %q, want %q", resolved.SourceRoot, canonicalRepository)
	}
	actionRuntime, err := resolved.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.ValidateEntrypoints(actionRuntime); err != nil {
		t.Fatalf("validate entry points through aliased ancestor: %v", err)
	}
	linkedRepository := filepath.Join(base, "linked-repository")
	if err := os.Symlink(repository, linkedRepository); err != nil {
		t.Fatal(err)
	}
	materializer.result.RepositoryRoot = linkedRepository
	if _, _, err := newActionLockResolver(job, "", materializer).resolve(context.Background(), plan.ActionSelector{Lock: "lock"}); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
		t.Fatalf("resolver accepted symlink repository root: %v", err)
	}
}

func TestActionLockResolverDownloadsExactCommitDirectlyFromCodeload(t *testing.T) {
	const token = "must-not-be-sent"
	commit := strings.Repeat("a", 40)
	fixture := t.TempDir()
	writeAction(t, fixture, "")
	digest := digestTree(t, fixture)
	archive := githubActionArchive(t)
	var apiRequests, archiveRequests, tokenProvisions int
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiRequests++
		http.Error(w, "GitHub REST must not be used by runtime exact-commit materialization", http.StatusInternalServerError)
	}))
	defer apiServer.Close()
	codeloadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveRequests++
		if r.URL.Path != "/owner/repo/tar.gz/"+commit {
			t.Errorf("codeload path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("codeload credentials = Authorization %q, Cookie %q", r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		_, _ = w.Write(archive)
	}))
	defer codeloadServer.Close()
	store, err := source.NewStore(t.TempDir(), codeloadServer.Client(), source.WithTestEndpoints(apiServer.URL, codeloadServer.URL), source.WithGitHubActionSourceTokenProvider("pipeline/repo", func(context.Context) (string, error) {
		tokenProvisions++
		return token, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Job{
		RequiredCapabilities: []string{"network"},
		Actions: []plan.ActionLock{{
			ID: "lock", Source: "github", Repository: "owner/repo",
			RequestedRef: "v1", Commit: commit, SourceDigest: digest,
		}},
	}
	resolver := newActionLockResolver(job, "", store)
	if _, _, err := resolver.resolve(context.Background(), plan.ActionSelector{Lock: "lock"}); err != nil {
		t.Fatal(err)
	}
	if apiRequests != 0 || archiveRequests != 1 || tokenProvisions != 0 {
		t.Fatalf("API/archive/token-provision requests = %d / %d / %d, want 0 / 1 / 0", apiRequests, archiveRequests, tokenProvisions)
	}
}

func githubActionArchive(t *testing.T) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		body string
	}{
		{name: "root/action.yml", body: "name: test\nruns:\n  using: node20\n  main: index.js\n"},
		{name: "root/index.js", body: "ok\n"},
	} {
		body := []byte(entry.body)
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func TestActionLockResolverWorkspaceLazyAndReverified(t *testing.T) {
	workspace := t.TempDir()
	job := workflowJob(t, workspace)
	action := filepath.Join(workspace, "actions", "local")
	// Compute the future lock digest outside the initially empty workspace.
	fixture := t.TempDir()
	writeAction(t, fixture, "")
	job.Actions = []plan.ActionLock{{ID: "local", Source: "workspace", Path: "actions/local", SourceDigest: digestTree(t, fixture)}}
	r := newActionLockResolver(job, workspace, nil)
	if _, _, err := r.resolve(context.Background(), plan.ActionSelector{Lock: "local"}); err == nil {
		t.Fatal("missing workspace action succeeded")
	}
	writeAction(t, workspace, "actions/local")
	if _, _, err := r.resolve(context.Background(), plan.ActionSelector{Lock: "local"}); err != nil {
		t.Fatalf("populated workspace action: %v", err)
	}
	if err := os.WriteFile(filepath.Join(action, "index.js"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.resolve(context.Background(), plan.ActionSelector{Lock: "local"}); err == nil {
		t.Fatal("tampered workspace action succeeded")
	}
}

func TestActionLockResolverFailsClosed(t *testing.T) {
	r := newActionLockResolver(plan.Job{Actions: []plan.ActionLock{{ID: "x", Source: "github", Repository: "owner/other", Commit: strings.Repeat("b", 40), SourceDigest: "sha256:" + strings.Repeat("0", 64)}}}, "", nil)
	for _, selector := range []plan.ActionSelector{{}, {Lock: "missing"}, {Lock: "x"}} {
		if _, _, err := r.resolve(context.Background(), selector); err == nil {
			t.Fatalf("selector %#v unexpectedly succeeded", selector)
		}
	}
}

func TestRunJobV3RemoteActionPopulatesEmptyWorkspaceBeforeLocalChild(t *testing.T) {
	workflowSource := "name: checked out\n"
	localSource := "name: local\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: printf '%s\\n' 'V3_LOCAL_CHILD=seen' >> \"$GITHUB_ENV\"\n"
	workflowEncoded := base64.StdEncoding.EncodeToString([]byte(workflowSource))
	localEncoded := base64.StdEncoding.EncodeToString([]byte(localSource))
	localFixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(localFixture, "action.yml"), []byte(localSource), 0o644); err != nil {
		t.Fatal(err)
	}
	localDigest := digestTree(t, localFixture)

	remote := t.TempDir()
	remoteMetadata := `name: checkout-like
runs:
  using: composite
  steps:
    - shell: sh
      run: |
        mkdir -p "$GITHUB_WORKSPACE/.github/workflows" "$GITHUB_WORKSPACE/actions/local"
        printf '%s' '` + workflowEncoded + `' | base64 -d > "$GITHUB_WORKSPACE/.github/workflows/test.yml"
        printf '%s' '` + localEncoded + `' | base64 -d > "$GITHUB_WORKSPACE/actions/local/action.yml"
        chmod 0644 "$GITHUB_WORKSPACE/.github/workflows/test.yml" "$GITHUB_WORKSPACE/actions/local/action.yml"
    - uses: ./actions/local
`
	if err := os.WriteFile(filepath.Join(remote, "action.yml"), []byte(remoteMetadata), 0o644); err != nil {
		t.Fatal(err)
	}
	remoteDigest := digestTree(t, remote)
	workflowDigest := sha256.Sum256([]byte(workflowSource))
	commit := strings.Repeat("a", 40)
	remoteID, localID := "a-0000000000000001", "a-0000000000000002"
	requiresMise := false
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Runtime: &plan.Runtime{DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow: plan.Workflow{
			Path: ".github/workflows/test.yml", Digest: "sha256:" + hex.EncodeToString(workflowDigest[:]), LogicalJobID: "v3",
		},
		Event: plan.Event{
			Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64), Repository: "owner/project", SHA: strings.Repeat("b", 40),
		},
		Target:               plan.Target{StepKey: "gha-v3", Queue: "trusted"},
		RequiredCapabilities: []string{"network"},
		Steps: []plan.Step{
			{ID: "checkout", Kind: "uses", Uses: "owner/repo@v1", Action: &plan.ActionSelector{Lock: remoteID}},
			{ID: "verify", Kind: "run", Shell: "sh", Command: `test "$V3_LOCAL_CHILD" = seen`},
		},
		Actions: []plan.ActionLock{
			{
				ID: remoteID, Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: commit, SourceDigest: remoteDigest,
				Children: map[string]plan.ActionSelector{"./actions/local": {Lock: localID}},
			},
			{ID: localID, Source: "workspace", Path: "actions/local", SourceDigest: localDigest},
		},
		RequiresMise: &requiresMise,
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: remoteDigest}}
	result, err := (Runner{Actions: materializer}).RunJob(context.Background(), job, "")
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" || result.Env["V3_LOCAL_CHILD"] != "seen" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	materializer.mu.Lock()
	defer materializer.mu.Unlock()
	if materializer.calls != 1 || materializer.resolved.Commit != commit || materializer.resolved.SourceDigest != remoteDigest {
		t.Fatalf("materializer calls/resolved = %d / %#v", materializer.calls, materializer.resolved)
	}
}
