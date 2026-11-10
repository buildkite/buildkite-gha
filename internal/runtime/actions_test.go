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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	executionprogram "github.com/buildkite/buildkite-gha/internal/program"
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
	r := testActionLockResolver(t, job, "", fake)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err != nil {
				t.Errorf("resolve: %v", err)
			}
		})
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
	if _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err == nil {
		t.Fatal("resolve after repository tampering succeeded")
	}
}

func TestActionLockResolverProvenanceDoesNotHideUnixModeTampering(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix executable bits")
	}
	for _, kind := range []string{"workspace", "github"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeAction(t, root, "nested")
			entry := filepath.Join(root, "nested", "index.js")
			if err := os.Chmod(entry, 0o755); err != nil {
				t.Fatal(err)
			}
			job := workflowJob(t, root)
			lock := plan.ActionLock{ID: "lock", Source: kind, Path: "nested"}
			if kind == "workspace" {
				lock.SourceDigest = digestTree(t, filepath.Join(root, "nested"))
				lock.ExecutablePaths = []string{"index.js"}
			} else {
				lock.Repository, lock.Commit = "owner/repo", strings.Repeat("a", 40)
				lock.SourceDigest = digestTree(t, root)
				lock.ExecutablePaths = []string{"nested/index.js"}
				job.RequiredCapabilities = []string{"network"}
			}
			job.Actions = []plan.ActionLock{lock}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: root, SourceDigest: lock.SourceDigest}}
			resolver := testActionLockResolver(t, job, root, materializer)
			if _, err := resolver.resolve(t.Context(), plan.ActionSelector{Lock: lock.ID}); err != nil {
				t.Fatal(err)
			}
			if materializer.resolved.ExecutablePaths != nil {
				t.Fatal("Unix runtime supplied mode overrides to materializer")
			}
			if err := os.Chmod(entry, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.resolve(t.Context(), plan.ActionSelector{Lock: lock.ID}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("mode tamper error = %v", err)
			}
		})
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
			ctx, cancel := context.WithTimeout(t.Context(), 0)
			defer cancel()
			materializer := &fakeActionMaterializer{err: tc.err}

			_, err := testActionLockResolver(t, plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock}}, "", materializer).resolve(ctx, plan.ActionSelector{Lock: lock.ID})
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
	for _, commit := range actionintegration.CacheCommits() {
		for _, path := range []string{"", "restore", "save"} {
			name := commit[:12] + "/root"
			if path != "" {
				name = commit[:12] + "/" + path
			}
			t.Run(name, func(t *testing.T) {
				lock := plan.ActionLock{ID: "cache", Source: "github", Repository: "actions/cache", Commit: commit, Path: path, SourceDigest: digest}
				job := plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock}}
				materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: repo, SourceDigest: digest}}
				if resolved, err := testActionLockResolver(t, job, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: lock.ID}); err != nil || !reflect.DeepEqual(resolved.lock, lock) {
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
	if _, err := testActionLockResolver(t, plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock}}, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: lock.ID}); err == nil {
		t.Fatal("unproved cache commit resolved")
	}
	if materializer.calls != 0 {
		t.Fatalf("unproved cache commit reached materializer %d times", materializer.calls)
	}
}

func TestActionLockResolverBindsProgramAndIntegration(t *testing.T) {
	repo := canonicalTempDir(t)
	writeAction(t, repo, "")
	writeAction(t, repo, "nested")
	digest := digestTree(t, repo)
	program := executionprogram.Action{Name: "planned, not source metadata", Runtime: "node24", Main: "index.js"}
	for _, test := range []struct {
		name, repository, path, commit string
		serverURL                      string
		omitProgram                    bool
		want                           actionintegration.Descriptor
		wantError                      bool
	}{
		{name: "ordinary nested action", repository: "owner/action", path: "nested"},
		{name: "cache service", repository: "actions/cache", commit: actionintegration.CacheCommit, want: actionintegration.Descriptor{Service: actionintegration.ServiceCache}},
		{name: "cache client", repository: "actions/setup-node", want: actionintegration.Descriptor{CacheClientCompatibility: true}},
		{name: "unrecognized subpath", repository: "actions/setup-node", path: "nested"},
		{name: "native without program", repository: "actions/checkout", commit: actionintegration.CheckoutV4Commit, omitProgram: true, want: actionintegration.Descriptor{Adapter: actionintegration.AdapterCheckoutExactEventSHA}},
		{name: "explicit github.com native", serverURL: "https://github.com", repository: "actions/checkout", commit: actionintegration.CheckoutV4Commit, omitProgram: true, want: actionintegration.Descriptor{Adapter: actionintegration.AdapterCheckoutExactEventSHA}},
		{name: "other host checkout executes program", serverURL: "https://ghe.example", repository: "actions/checkout", commit: actionintegration.CheckoutV4Commit},
		{name: "other host checkout requires program", serverURL: "https://ghe.example", repository: "actions/checkout", commit: actionintegration.CheckoutV4Commit, omitProgram: true, wantError: true},
		{name: "other host cache client", serverURL: "https://ghe.example", repository: "actions/setup-node"},
		{name: "ordinary missing program", repository: "owner/action", omitProgram: true, wantError: true},
		{name: "known non-native missing program", repository: "actions/setup-node", omitProgram: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			commit := test.commit
			if commit == "" {
				commit = strings.Repeat("a", 40)
			}
			lock := plan.ActionLock{ID: "selected", Source: "github", ServerURL: test.serverURL, Repository: test.repository, Path: test.path, Commit: commit, SourceDigest: digest}
			job := plan.Job{
				RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock},
				Program: &executionprogram.Program{Actions: map[string]executionprogram.Action{
					"other": {Name: "wrong program", Runtime: "composite"},
				}},
			}
			if !test.omitProgram {
				job.Program.Actions[lock.ID] = program
			}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: repo, SourceDigest: digest}}
			resolved, err := newActionLockResolver(job, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: lock.ID})
			if materializer.calls != 1 || materializer.resolved.Reference.ServerURL != test.serverURL {
				t.Fatalf("source host not propagated: calls=%d, reference=%#v", materializer.calls, materializer.resolved.Reference)
			}
			if test.wantError {
				if err == nil || !isHardJobFailure(err) || !strings.Contains(err.Error(), "action program is missing") {
					t.Fatalf("resolve() error = %v, want hard missing-program failure", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resolved.lock, lock) || resolved.integration != test.want {
				t.Fatalf("resolved lock/integration = %#v / %#v, want %#v / %#v", resolved.lock, resolved.integration, lock, test.want)
			}
			if resolved.metadata.Path != filepath.Join(repo, test.path) || resolved.metadata.SourceRoot != repo {
				t.Fatalf("resolved source paths = %q / %q", resolved.metadata.Path, resolved.metadata.SourceRoot)
			}
			if test.omitProgram {
				if resolved.program != nil {
					t.Fatalf("native action unexpectedly acquired a program: %#v", resolved.program)
				}
			} else if !reflect.DeepEqual(resolved.program, &program) || resolved.metadata.Name != program.Name {
				t.Fatalf("resolved program/metadata = %#v / %#v", resolved.program, resolved.metadata)
			}
		})
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
	resolved, err := testActionLockResolver(t, job, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: "lock"})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.metadata.SourceRoot != canonicalRepository {
		t.Fatalf("resolved source root = %q, want %q", resolved.metadata.SourceRoot, canonicalRepository)
	}
	actionRuntime, err := resolved.metadata.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.metadata.ValidateEntrypoints(actionRuntime); err != nil {
		t.Fatalf("validate entry points through aliased ancestor: %v", err)
	}
	linkedRepository := filepath.Join(base, "linked-repository")
	if err := os.Symlink(repository, linkedRepository); err != nil {
		t.Fatal(err)
	}
	materializer.result.RepositoryRoot = linkedRepository
	if _, err := testActionLockResolver(t, job, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
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
	resolver := testActionLockResolver(t, job, "", store)
	if _, err := resolver.resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err != nil {
		t.Fatal(err)
	}
	if apiRequests != 0 || archiveRequests != 1 || tokenProvisions != 0 {
		t.Fatalf("API/archive/token-provision requests = %d / %d / %d, want 0 / 1 / 0", apiRequests, archiveRequests, tokenProvisions)
	}
}

func TestGHESPublicReusableAndNestedActionsCompileAndRun(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	files := map[string]string{
		".github/workflows/reuse.yml": "on: workflow_call\njobs:\n  call:\n    uses: ./.github/workflows/nested.yml\n",
		".github/workflows/nested.yml": `on: workflow_call
jobs:
  execute:
    runs-on: ubuntu-latest
    outputs:
      marker: ${{ steps.parent.outputs.marker }}
    steps:
      - id: parent
        uses: o/r/parent@v1
`,
		"parent/action.yml": `name: Parent
outputs:
  marker:
    value: ${{ steps.child.outputs.marker }}
runs:
  using: composite
  steps:
    - id: child
      uses: o/r/child@` + commit + "\n",
		"child/action.yml": `name: Child
outputs:
  marker:
    value: ${{ steps.write.outputs.marker }}
runs:
  using: composite
  steps:
    - id: write
      shell: bash
      run: echo 'marker=enterprise-source' >> "$GITHUB_OUTPUT"
`,
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "root/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var archives int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("GHES received credentials")
		}
		switch r.URL.Path {
		case "/api/v3/repos/o/r/git/ref/tags/v1":
			_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, commit)
		case "/api/v3/repos/o/r/tarball/" + commit:
			archives++
			_, _ = w.Write(archive.Bytes())
		default:
			t.Errorf("unexpected GHES request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	resolver, err := source.NewResolver(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	store, err := source.NewStore(t.TempDir(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	repositorySource := compiler.MemoizeRepositorySource(compiler.PublicActionSource{Resolver: resolver, Store: store})
	workspace := t.TempDir()
	workflowPath := ".github/workflows/caller.yml"
	workflow := "on: push\njobs:\n  reuse:\n    uses: o/r/.github/workflows/reuse.yml@v1\n"
	writeFixtureFile(t, workspace, workflowPath, workflow)
	event := []byte(fmt.Sprintf(`{"provider":"github","server_url":%q,"event":"push","repository":{"owner":"caller","name":"project","default_branch":"main"},"ref":"refs/heads/main","sha":%q,"actor":"test"}`, server.URL, commit))
	bundle, err := compiler.CompileBundlePlansContext(t.Context(), filepath.Join(workspace, workflowPath), []byte(workflow), event, "test", "sha256:"+strings.Repeat("1", 64), compiler.Options{
		EventTrust:     compiler.EventUntrusted,
		Runners:        compiler.RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "hosted"}, UntrustedQueues: []string{"hosted"}},
		ResolveActions: true, ActionSource: repositorySource, RepositorySource: repositorySource,
	})
	if err != nil || len(bundle.Plans) != 1 {
		t.Fatalf("compile: %v; plans=%d", err, len(bundle.Plans))
	}
	job := bundle.Plans[0].Job
	if len(job.Actions) != 2 {
		t.Fatalf("action locks = %#v", job.Actions)
	}
	for _, lock := range job.Actions {
		if lock.ServerURL != server.URL || lock.Commit != commit || lock.SourceDigest == "" {
			t.Fatalf("lost immutable source identity: %#v", lock)
		}
	}
	// Runtime starts with a cold cache, as it does on another hosted agent.
	runtimeStore, err := source.NewStore(t.TempDir(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	result, err := (Runner{Actions: runtimeStore, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["marker"] != "enterprise-source" {
		t.Fatalf("run: %v; result=%#v\n%s", err, result, logs.String())
	}
	if archives != 2 {
		t.Fatalf("archive downloads = %d, want compile and cold runtime", archives)
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
	r := testActionLockResolver(t, job, workspace, nil)
	if _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "local"}); err == nil {
		t.Fatal("missing workspace action succeeded")
	}
	writeAction(t, workspace, "actions/local")
	if _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "local"}); err != nil {
		t.Fatalf("populated workspace action: %v", err)
	}
	if err := os.WriteFile(filepath.Join(action, "index.js"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "local"}); err == nil {
		t.Fatal("tampered workspace action succeeded")
	}
}

func TestActionLockResolverFailsClosed(t *testing.T) {
	r := testActionLockResolver(t, plan.Job{Actions: []plan.ActionLock{{ID: "x", Source: "github", Repository: "owner/other", Commit: strings.Repeat("b", 40), SourceDigest: "sha256:" + strings.Repeat("0", 64)}}}, "", nil)
	for _, selector := range []plan.ActionSelector{{}, {Lock: "missing"}, {Lock: "x"}} {
		if _, err := r.resolve(t.Context(), selector); err == nil {
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
		Program: runtimeTestProgram([]runtimeTestStep{
			{ID: "checkout", Kind: "uses", Uses: "owner/repo@v1", Action: &plan.ActionSelector{Lock: remoteID}},
			{ID: "verify", Kind: "run", Shell: "sh", Command: `test "$V3_LOCAL_CHILD" = seen`},
		}),
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
	attachTestProgram(&job)
	if err := attachTestActionProgramFromRoot(&job, remoteID, remote, "."); err != nil {
		t.Fatal(err)
	}
	if err := attachTestActionProgramFromRoot(&job, localID, localFixture, "."); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Actions: materializer}).runTestJob(t.Context(), job, "")
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
