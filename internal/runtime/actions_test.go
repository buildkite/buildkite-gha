package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

type fakeActionMaterializer struct {
	mu       sync.Mutex
	calls    int
	resolved source.Resolved
	result   source.Materialized
}

func (f *fakeActionMaterializer) Materialize(_ context.Context, r source.Resolved) (source.Materialized, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.resolved = r
	return f.result, nil
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
	job := plan.Job{Actions: []plan.ActionLock{{ID: "lock", Source: "github", Repository: "owner/repo", RequestedRef: "do-not-use", Commit: commit, Path: "nested", SourceDigest: digest}}}
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
	job := plan.Job{
		Schema: plan.SchemaV3,
		Compiler: plan.Compiler{
			Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Workflow: plan.Workflow{
			Path: ".github/workflows/test.yml", Digest: "sha256:" + hex.EncodeToString(workflowDigest[:]), LogicalJobID: "v3",
		},
		Event: plan.Event{
			Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64), Repository: "owner/project", SHA: strings.Repeat("b", 40),
		},
		Target:               plan.Target{StepKey: "gha-v3", Queue: "trusted"},
		RequiredCapabilities: []string{},
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
