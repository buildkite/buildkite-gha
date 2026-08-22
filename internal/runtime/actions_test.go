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
	"slices"
	"strings"
	"sync"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
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

// normalizeActionJob lowers the fixture manifests into the same immutable
// action program consumed in production. Roots may be workspace paths or fake
// materializers whose already-materialized repository contains remote actions.
func normalizeActionJob(t *testing.T, job *plan.Job, roots ...any) {
	t.Helper()
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		switch root := root.(type) {
		case string:
			paths = append(paths, root)
		case *fakeActionMaterializer:
			paths = append(paths, root.result.RepositoryRoot)
		}
	}
	if len(job.Actions) == 0 && len(paths) != 0 {
		var addWorkspaceAction func(string) (string, error)
		addWorkspaceAction = func(path string) (string, error) {
			path = strings.TrimPrefix(path, "./")
			for _, lock := range job.Actions {
				if lock.Source == "workspace" && lock.Path == path {
					return lock.ID, nil
				}
			}
			m, err := metadata.Load(paths[0], path)
			if err != nil {
				return "", err
			}
			id := fmt.Sprintf("a-%016x", len(job.Actions)+1)
			job.Actions = append(job.Actions, plan.ActionLock{ID: id, Source: "workspace", Path: path, SourceDigest: digestTree(t, filepath.Join(paths[0], filepath.FromSlash(path)))})
			index := len(job.Actions) - 1
			if actionRuntime, runtimeErr := m.Runtime(); runtimeErr == nil && actionRuntime == metadata.RuntimeComposite {
				for _, step := range m.Runs.Steps {
					if !strings.HasPrefix(step.Uses, "./") {
						continue
					}
					child, childErr := addWorkspaceAction(step.Uses)
					if childErr != nil {
						return "", childErr
					}
					if job.Actions[index].Children == nil {
						job.Actions[index].Children = map[string]plan.ActionSelector{}
					}
					job.Actions[index].Children[step.Uses] = plan.ActionSelector{Lock: child}
				}
			}
			return id, nil
		}
		for i := range job.Steps {
			step := &job.Steps[i]
			if step.Kind != "uses" || !strings.HasPrefix(step.Uses, "./") {
				continue
			}
			id, err := addWorkspaceAction(step.Uses)
			if err != nil {
				continue
			}
			if step.Action == nil {
				step.Action = &plan.ActionSelector{Lock: id}
			}
		}
	}
	job.ActionPrograms = make(map[string]program.Action, len(job.Actions))
	for _, lock := range job.Actions {
		if actionintegration.UsesNativeAdapter(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path}) {
			job.ActionPrograms[lock.ID] = program.Action{Source: lock.Source, Runtime: program.ActionRuntimeNative}
			continue
		}
		var m metadata.Metadata
		var err = fmt.Errorf("no fixture root matches source digest %q", lock.SourceDigest)
		for _, root := range paths {
			if root == "" {
				continue
			}
			for _, candidate := range []struct{ digestPath, metadataPath string }{
				{digestPath: lock.Path, metadataPath: lock.Path},
				{digestPath: "", metadataPath: lock.Path},
				{digestPath: "", metadataPath: ""},
			} {
				tree := filepath.Join(root, filepath.FromSlash(candidate.digestPath))
				digest, digestErr := source.DigestTree(tree)
				if digestErr != nil || digest != lock.SourceDigest {
					continue
				}
				m, err = metadata.Load(root, candidate.metadataPath)
				if err == nil {
					break
				}
			}
			if err == nil {
				break
			}
		}
		if err != nil {
			if len(paths) == 1 {
				m, err = metadata.Load(paths[0], lock.Path)
			}
		}
		if err != nil {
			t.Fatalf("load action program %q: %v", lock.ID, err)
		}
		job.ActionPrograms[lock.ID] = lowerTestAction(m, lock)
	}
	orderedSteps := make([]program.Step, len(job.Steps))
	for i, step := range job.Steps {
		orderedSteps[i] = program.Step{ID: step.ID, Kind: step.Kind}
		for _, normalized := range job.Program.Job.Steps {
			if normalized.ID == step.ID {
				orderedSteps[i] = normalized
				break
			}
		}
	}
	job.Program.Job.Steps = orderedSteps
	for i := range job.Steps {
		step := &job.Steps[i]
		if step.Action == nil {
			for _, lock := range job.Actions {
				local := lock.Source == "workspace" && strings.TrimPrefix(step.Uses, "./") == strings.TrimPrefix(lock.Path, "./")
				remote := lock.Source == "github" && (step.Uses == lock.Repository+"@"+lock.RequestedRef || strings.HasPrefix(step.Uses, lock.Repository+"/"))
				if local || remote {
					step.Action = &plan.ActionSelector{Lock: lock.ID}
					break
				}
			}
		}
		if step.Action == nil {
			continue
		}
		invocation := &program.Invocation{
			Uses: testActionSite(step.Uses, program.SurfaceRuntimeTemplate, program.ProvenanceWorkflow, program.PurposeExpression, program.Location{}, ""),
			With: testActionBindings(step.With, program.SurfaceStepTemplate, program.ProvenanceWorkflow, program.PurposeActionInput, program.Location{}, ""),
			Lock: step.Action.Lock,
		}
		normalized := &job.Program.Job.Steps[i]
		normalized.Condition = testActionSite(step.Condition, program.SurfaceStepCondition, program.ProvenanceWorkflow, program.PurposeExpression, program.Location{}, "")
		normalized.Env = testActionBindings(step.Env, program.SurfaceStepTemplate, program.ProvenanceWorkflow, program.PurposeExpression, program.Location{}, "")
		normalized.Invocation = invocation
	}
}

func lowerTestAction(m metadata.Metadata, lock plan.ActionLock) program.Action {
	location := program.Location{File: m.SourcePath, Field: "action"}
	action := program.Action{Name: m.Name, Source: lock.Source, Location: location}
	for _, name := range sortedTestKeys(m.Inputs) {
		definition := m.Inputs[name]
		input := program.ActionInput{Name: name, Required: definition.Required}
		if definition.Default != nil {
			site := testActionSite(*definition.Default, program.SurfaceActionInputDefault, program.ProvenanceAction, program.PurposeExpression, location, "action.inputs."+name+".default")
			input.Default = &site
		}
		action.Inputs = append(action.Inputs, input)
	}
	runtime, err := m.Runtime()
	if err != nil {
		return action
	}
	switch runtime {
	case metadata.RuntimeNode16, metadata.RuntimeNode24:
		action.Runtime = program.ActionRuntimeJavaScript
		major := 24
		if m.Runs.Using == string(metadata.RuntimeNode16) {
			major = 16
		}
		action.JavaScript = &program.JavaScriptAction{NodeMajor: major, Pre: m.Runs.Pre, Main: m.Runs.Main, Post: m.Runs.Post,
			PreCondition:  testActionSite(m.Runs.PreIf, program.SurfaceActionLifecycle, program.ProvenanceAction, program.PurposeExpression, location, "action.runs.pre-if"),
			PostCondition: testActionSite(m.Runs.PostIf, program.SurfaceActionLifecycle, program.ProvenanceAction, program.PurposeExpression, location, "action.runs.post-if")}
	case metadata.RuntimeComposite:
		action.Runtime = program.ActionRuntimeComposite
		action.Composite = &program.CompositeAction{}
		for i, step := range m.Runs.Steps {
			field := fmt.Sprintf("action.runs.steps[%d]", i)
			lowered := program.CompositeStep{ID: step.ID, ContinueOnError: step.ContinueOnError,
				Name:      testActionSite(step.Name, program.SurfaceCompositeTemplate, program.ProvenanceAction, program.PurposeExpression, location, field+".name"),
				Condition: testActionSite(step.If, program.SurfaceStepCondition, program.ProvenanceAction, program.PurposeExpression, location, field+".if"),
				Env:       testActionBindings(step.Env, program.SurfaceCompositeTemplate, program.ProvenanceAction, program.PurposeExpression, location, field+".env")}
			if step.Run != "" {
				lowered.Run = &program.Run{Command: testActionSite(step.Run, program.SurfaceCompositeTemplate, program.ProvenanceAction, program.PurposeExpression, location, field+".run"), Shell: testActionSite(step.Shell, program.SurfaceCompositeTemplate, program.ProvenanceAction, program.PurposeExpression, location, field+".shell"), WorkingDirectory: testActionSite(step.WorkingDirectory, program.SurfaceCompositeTemplate, program.ProvenanceAction, program.PurposeExpression, location, field+".working-directory")}
			}
			if step.Uses != "" {
				lowered.Invocation = &program.Invocation{Uses: testActionSite(step.Uses, program.SurfaceRuntimeTemplate, program.ProvenanceAction, program.PurposeExpression, location, field+".uses"), With: testActionBindings(step.With, program.SurfaceCompositeTemplate, program.ProvenanceAction, program.PurposeExpression, location, field+".with")}
				if child, ok := lock.Children[step.Uses]; ok {
					lowered.Invocation.Lock = child.Lock
				}
			}
			action.Composite.Steps = append(action.Composite.Steps, lowered)
		}
		for _, name := range sortedTestKeys(m.Outputs) {
			action.Outputs = append(action.Outputs, program.Binding{Name: name, Value: testActionSite(m.Outputs[name].Value, program.SurfaceCompositeTemplate, program.ProvenanceAction, program.PurposeExpression, location, "action.outputs."+name)})
		}
	case metadata.RuntimeDocker:
		action.Runtime = program.ActionRuntimeDocker
		action.Docker = &program.DockerAction{Env: testActionBindings(m.Runs.Env, program.SurfaceRuntimeTemplate, program.ProvenanceAction, program.PurposeExpression, location, "action.runs.env")}
		for i, arg := range m.Runs.Args {
			action.Docker.Arguments = append(action.Docker.Arguments, testActionSite(arg, program.SurfaceDockerArgument, program.ProvenanceAction, program.PurposeExpression, location, fmt.Sprintf("action.runs.args[%d]", i)))
		}
	}
	return action
}

func testActionBindings(values map[string]string, surface program.Surface, provenance program.Provenance, purpose program.Purpose, location program.Location, field string) []program.Binding {
	bindings := make([]program.Binding, 0, len(values))
	for _, name := range sortedTestKeys(values) {
		bindings = append(bindings, program.Binding{Name: name, Value: testActionSite(values[name], surface, provenance, purpose, location, field+"."+name)})
	}
	return bindings
}

func testActionSite(source string, surface program.Surface, provenance program.Provenance, purpose program.Purpose, location program.Location, field string) program.Site {
	location.Field = field
	result := program.ResultString
	if surface == program.SurfaceStepCondition || surface == program.SurfaceActionLifecycle {
		result = program.ResultBoolean
	}
	return program.Site{Source: source, Surface: surface, Result: result, Provenance: provenance, Purpose: purpose, Location: location}
}

func sortedTestKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
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

func TestActionLockResolverUsesNormalizedProgramWithoutParsingMetadata(t *testing.T) {
	workspace := t.TempDir()
	job := workflowJob(t, workspace)
	actionPath := filepath.Join(workspace, "action")
	if err := os.Mkdir(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "index.js"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const lockID = "a-0000000000000001"
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: "action", SourceDigest: digestTree(t, actionPath)}}
	job.ActionPrograms = map[string]program.Action{lockID: {
		Name: "compiled action", Source: "workspace", Runtime: program.ActionRuntimeJavaScript,
		JavaScript: &program.JavaScriptAction{NodeMajor: 24, Main: "index.js"},
	}}

	action, _, err := newActionLockResolver(job, workspace, nil).resolve(t.Context(), plan.ActionSelector{Lock: lockID})
	if err != nil {
		t.Fatal(err)
	}
	if action.Name != "compiled action" || action.Runs.Main != "index.js" {
		t.Fatalf("resolved normalized action = %#v", action)
	}
}

func TestActionMetadataRejectsMalformedNormalizedCompositeStep(t *testing.T) {
	_, err := actionMetadata(program.Action{
		Runtime:   program.ActionRuntimeComposite,
		Composite: &program.CompositeAction{Steps: []program.CompositeStep{{}}},
	}, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "must have exactly one execution") {
		t.Fatalf("malformed composite step error = %v", err)
	}
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
			if _, _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err != nil {
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
	if _, _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err == nil {
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
			ctx, cancel := context.WithTimeout(t.Context(), 0)
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
				if _, resolved, err := newActionLockResolver(job, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: lock.ID}); err != nil || !reflect.DeepEqual(resolved, lock) {
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
	if _, _, err := newActionLockResolver(plan.Job{RequiredCapabilities: []string{"network"}, Actions: []plan.ActionLock{lock}}, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: lock.ID}); err == nil {
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
	resolved, _, err := newActionLockResolver(job, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: "lock"})
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
	if _, _, err := newActionLockResolver(job, "", materializer).resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
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
	if _, _, err := resolver.resolve(t.Context(), plan.ActionSelector{Lock: "lock"}); err != nil {
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
	if _, _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "local"}); err == nil {
		t.Fatal("missing workspace action succeeded")
	}
	writeAction(t, workspace, "actions/local")
	if _, _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "local"}); err != nil {
		t.Fatalf("populated workspace action: %v", err)
	}
	if err := os.WriteFile(filepath.Join(action, "index.js"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.resolve(t.Context(), plan.ActionSelector{Lock: "local"}); err == nil {
		t.Fatal("tampered workspace action succeeded")
	}
}

func TestActionLockResolverFailsClosed(t *testing.T) {
	r := newActionLockResolver(plan.Job{Actions: []plan.ActionLock{{ID: "x", Source: "github", Repository: "owner/other", Commit: strings.Repeat("b", 40), SourceDigest: "sha256:" + strings.Repeat("0", 64)}}}, "", nil)
	for _, selector := range []plan.ActionSelector{{}, {Lock: "missing"}, {Lock: "x"}} {
		if _, _, err := r.resolve(t.Context(), selector); err == nil {
			t.Fatalf("selector %#v unexpectedly succeeded", selector)
		}
	}
}

func TestVerifiedActionEntrypointStaysWithinRepository(t *testing.T) {
	parent := t.TempDir()
	repository := filepath.Join(parent, "repository")
	action := filepath.Join(repository, "action")
	dist := filepath.Join(repository, "dist")
	if err := os.MkdirAll(action, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(dist, "index.js")
	if err := os.WriteFile(entrypoint, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := verifiedActionEntrypoint(actionSourceFacts{ActionPath: action, RepositoryRoot: repository}, "../dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if got != entrypoint {
		t.Fatalf("verified entrypoint = %q, want %q", got, entrypoint)
	}

	outside := filepath.Join(parent, "outside.js")
	if err := os.WriteFile(outside, []byte("no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedActionEntrypoint(actionSourceFacts{ActionPath: action, RepositoryRoot: repository}, "../../outside.js"); err == nil {
		t.Fatal("entrypoint outside repository succeeded")
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
	normalizeActionJob(t, &job, remote, localFixture)
	result, err := (Runner{Actions: materializer}).RunJob(t.Context(), job, "")
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
