package compiler

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"go.yaml.in/yaml/v4"
)

type fakeActionSource struct {
	root  string
	calls map[string]int
}

type contextActionSource struct{}

func (contextActionSource) Fetch(ctx context.Context, _ source.Reference) (source.Resolved, source.Materialized, error) {
	<-ctx.Done()
	return source.Resolved{}, source.Materialized{}, ctx.Err()
}

func (f *fakeActionSource) Fetch(_ context.Context, r source.Reference) (source.Resolved, source.Materialized, error) {
	f.calls[r.Raw]++
	d, err := source.DigestTree(filepath.Join(f.root, r.Path))
	commit := strings.Repeat("a", 40)
	if len(r.Ref) == 40 && strings.Trim(strings.ToLower(r.Ref), "0123456789abcdef") == "" {
		commit = strings.ToLower(r.Ref)
	}
	return source.Resolved{Reference: r, Commit: commit}, source.Materialized{RepositoryRoot: f.root, ActionRoot: filepath.Join(f.root, r.Path), SourceDigest: d}, err
}

func writeAction(t *testing.T, root, name, body string) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "action.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "using: docker") {
		if err := os.WriteFile(filepath.Join(d, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []string{"index.js"} {
		if strings.Contains(body, entry) {
			if err := os.WriteFile(filepath.Join(d, entry), []byte("// fixture\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCompileActionLocksLocalAndDedup(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "js", "name: js\nruns:\n  using: node20\n  main: index.js\n")
	selectors, locks, caps, err := compileActionLocks(context.Background(), w, nil, []string{"./js", "./js"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || len(selectors) != 2 || selectors[0] != selectors[1] || locks[0].Path != "js" || !strings.HasPrefix(locks[0].SourceDigest, "sha256:") || len(caps) != 0 {
		t.Fatalf("unexpected result: %#v %#v %#v", selectors, locks, caps)
	}
}

func TestCompileActionLocksRemoteCompositeUsesWorkspaceRoot(t *testing.T) {
	w, remote := t.TempDir(), t.TempDir()
	writeAction(t, w, "child", "name: child\nruns:\n  using: docker\n  image: Dockerfile\n")
	writeAction(t, remote, "", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	f := &fakeActionSource{root: remote, calls: map[string]int{}}
	_, locks, caps, err := compileActionLocks(context.Background(), w, f, []string{"Owner/Repo@v1", "Owner/Repo@v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 || f.calls["Owner/Repo@v1"] != 1 || !reflect.DeepEqual(caps, []string{"docker", "network"}) {
		t.Fatalf("unexpected result: %#v calls=%v caps=%v", locks, f.calls, caps)
	}
	var found bool
	for _, l := range locks {
		if l.Source == "workspace" && l.Path == "child" {
			found = true
		}
	}
	if !found {
		t.Fatal("local child was not locked from workspace")
	}
}

func TestCompileActionLocksRecursion(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "loop", "name: loop\nruns:\n  using: composite\n  steps:\n    - uses: ./loop\n")
	_, _, _, err := compileActionLocks(context.Background(), w, nil, []string{"./loop"})
	if err == nil || !strings.Contains(err.Error(), "recursion") {
		t.Fatalf("got %v", err)
	}
}

func TestCompileActionLocksRequiresRepositoryRoot(t *testing.T) {
	_, _, _, err := compileActionLocks(context.Background(), "", nil, []string{"./local"})
	if err == nil || !strings.Contains(err.Error(), "workflow path must identify a repository root") {
		t.Fatalf("compileActionLocks() error = %v, want repository-root rejection", err)
	}
}

func TestCompileActionLocksRejectsExcessiveDepthBeforeResolvingLeaf(t *testing.T) {
	w := t.TempDir()
	for i := 0; i <= metadata.MaxNestedActionDepth; i++ {
		steps := ""
		if i < metadata.MaxNestedActionDepth {
			steps = "  steps:\n    - uses: ./depth-" + strconv.Itoa(i+1) + "\n"
		}
		writeAction(t, w, "depth-"+strconv.Itoa(i), "name: depth\nruns:\n  using: composite\n"+steps)
	}
	_, _, _, err := compileActionLocks(context.Background(), w, nil, []string{"./depth-0"})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum depth") {
		t.Fatalf("compileActionLocks() error = %v, want depth rejection", err)
	}
}

func TestCompileActionLocksRejectsEscapedJavaScriptEntrypoint(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "js", "name: js\nruns:\n  using: node20\n  main: ../outside.js\n")
	if err := os.WriteFile(filepath.Join(w, "outside.js"), []byte("// outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := compileActionLocks(context.Background(), w, nil, []string{"./js"})
	if err == nil || !strings.Contains(err.Error(), "escapes action source") {
		t.Fatalf("compileActionLocks() error = %v, want entry-point confinement rejection", err)
	}
}

func TestCompileActionLocksExplicitRemoteAndDistinctRefs(t *testing.T) {
	w, remote := t.TempDir(), t.TempDir()
	writeAction(t, remote, "", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: Other/Child/sub@v2\n")
	writeAction(t, remote, "sub", "name: child\nruns:\n  using: node24\n  main: index.js\n")
	f := &fakeActionSource{root: remote, calls: map[string]int{}}
	selectors, locks, _, err := compileActionLocks(context.Background(), w, f, []string{"Owner/Repo@v1", "Owner/Repo@main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 3 || selectors[0] == selectors[1] || f.calls["Other/Child/sub@v2"] != 1 {
		t.Fatalf("unexpected result: selectors=%v locks=%#v calls=%v", selectors, locks, f.calls)
	}
	for _, lock := range locks {
		if lock.Source == "github" && lock.Repository != strings.ToLower(lock.Repository) {
			t.Fatalf("repository is not canonical: %q", lock.Repository)
		}
	}
}

func TestCompileActionLocksDeterministic(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "parent", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	writeAction(t, w, "child", "name: child\nruns:\n  using: node20\n  main: index.js\n")
	aSelectors, aLocks, aCaps, err := compileActionLocks(context.Background(), w, nil, []string{"./parent"})
	if err != nil {
		t.Fatal(err)
	}
	bSelectors, bLocks, bCaps, err := compileActionLocks(context.Background(), w, nil, []string{"./parent"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]any{aSelectors, aLocks, aCaps}, []any{bSelectors, bLocks, bCaps}) {
		t.Fatalf("non-deterministic output:\n%#v\n%#v", aLocks, bLocks)
	}
}

func TestPublicActionSourceNil(t *testing.T) {
	_, _, err := (PublicActionSource{}).Fetch(context.Background(), source.Reference{})
	if err == nil {
		t.Fatal("nil dependencies accepted")
	}
}

func TestCompilePlansTokenlessActionsEmitV3Locks(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, workspace, "local", "name: local\nruns:\n  using: node20\n  main: index.js\n")
	writeAction(t, workspace, "child", "name: child\nruns:\n  using: node24\n  main: index.js\n")
	writeAction(t, remote, "", "name: remote\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	workflow := []byte(`on: push
jobs:
  shell:
    runs-on: ubuntu-latest
    steps:
      - run: echo shell
  actions:
    runs-on: ubuntu-latest
    steps:
      - uses: ./local
      - uses: Owner/Repo@v1
      - uses: Owner/Repo@v1
  other-actions:
    runs-on: ubuntu-latest
    steps:
      - uses: owner/repo@v1
`)
	fake := &fakeActionSource{root: remote, calls: map[string]int{}}
	options := Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   fake,
	}
	first, err := CompilePlansWithOptions(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompilePlansWithOptions(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("tokenless action plans are not deterministic")
	}
	if len(first) != 3 || first[0].Schema != plan.SchemaV3 || first[1].Schema != plan.SchemaV3 || first[2].Schema != plan.SchemaV2 {
		t.Fatalf("plan schemas = %#v, want two action v3 plans and one shell v2 plan", []string{first[0].Schema, first[1].Schema, first[2].Schema})
	}
	actionJob := first[0]
	if len(actionJob.Actions) != 3 || actionJob.Steps[0].Action == nil || actionJob.Steps[1].Action == nil || actionJob.Steps[2].Action == nil || *actionJob.Steps[1].Action != *actionJob.Steps[2].Action {
		t.Fatalf("action locks/selectors = %#v / %#v", actionJob.Actions, actionJob.Steps)
	}
	if fake.calls["Owner/Repo@v1"] != 2 {
		t.Fatalf("remote calls = %d, want one per independent compilation", fake.calls["Owner/Repo@v1"])
	}
	if err := actionJob.Validate(); err != nil {
		t.Fatalf("compiled v3 plan: %v", err)
	}
	var remoteLock *plan.ActionLock
	for i := range actionJob.Actions {
		if actionJob.Actions[i].Source == "github" {
			remoteLock = &actionJob.Actions[i]
			break
		}
	}
	if remoteLock == nil || remoteLock.Repository != "owner/repo" || remoteLock.Commit != strings.Repeat("a", 40) {
		t.Fatalf("remote lock = %#v", remoteLock)
	}
	child := remoteLock.Children["./child"]
	var childLock *plan.ActionLock
	for i := range actionJob.Actions {
		if actionJob.Actions[i].ID == child.Lock {
			childLock = &actionJob.Actions[i]
		}
	}
	if childLock == nil || childLock.Source != "workspace" || childLock.Path != "child" {
		t.Fatalf("remote composite child lock = %#v", childLock)
	}
}

func TestCompilePlansLocksLocalActionsWithoutRemoteResolution(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, workspace, "local", "name: local\nruns:\n  using: node24\n  main: index.js\n")
	workflow := []byte(`on: push
jobs:
  actions:
    runs-on: ubuntu-latest
    container: node:24
    services:
      redis:
        image: redis:7
    steps:
      - uses: ./local
`)
	first, err := CompilePlans(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompilePlans(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("workspace action plans are not deterministic")
	}
	if len(first) != 1 || first[0].Schema != plan.SchemaV4 || len(first[0].Actions) != 1 || first[0].Actions[0].Source != "workspace" || first[0].Steps[0].Action == nil {
		t.Fatalf("workspace action plan = %#v", first)
	}
}

func TestCompilePlansContainersWithRemoteActionsRequireResolution(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  actions:\n    runs-on: ubuntu-latest\n    container: node:24\n    steps:\n      - uses: owner/action@v1\n")
	_, err := CompilePlans(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err == nil || !strings.Contains(err.Error(), "containers with remote actions require action resolution through upload or profile validation") {
		t.Fatalf("CompilePlans() error = %v", err)
	}
}

func TestTokenlessCheckoutAdapterInputBoundary(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "checkout.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, remote, "", "name: checkout\nruns:\n  using: node24\n  main: index.js\n")
	compile := func(with string) ([]plan.Job, error) {
		workflow := []byte("on: push\njobs:\n  checkout:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1\n" + with)
		if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
			t.Fatal(err)
		}
		options := Options{
			EventTrust: EventUntrusted,
			Runners: RunnerPolicy{
				Labels:          map[string]string{"ubuntu-latest": "hosted"},
				UntrustedQueues: []string{"hosted"},
			},
			ResolveActions: true,
			ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
		}
		return CompilePlansWithOptions(workflowPath, workflow, pushEvent(t), "phase4-test", testDistributionDigest, options)
	}

	accepted := []string{
		"",
		"        with:\n          repository: buildkite/buildkite-gha\n          ref: 1111111111111111111111111111111111111111\n          fetch-depth: '1'\n          persist-credentials: false\n          clean: true\n          set-safe-directory: true\n",
	}
	for _, with := range accepted {
		plans, err := compile(with)
		if err != nil {
			t.Fatalf("tokenless checkout with %q: %v", with, err)
		}
		if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"network"}) {
			t.Fatalf("tokenless checkout plans = %#v", plans)
		}
	}

	rejected := map[string]string{
		"token":       "          token: ''\n",
		"repository":  "          repository: other/repository\n",
		"ref":         "          ref: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		"ssh-key":     "          ssh-key: key\n",
		"submodules":  "          submodules: true\n",
		"path":        "          path: nested\n",
		"fetch-depth": "          fetch-depth: '0'\n",
		"credentials": "          persist-credentials: true\n",
	}
	for name, input := range rejected {
		t.Run(name, func(t *testing.T) {
			_, err := compile("        with:\n" + input)
			if err == nil || !strings.Contains(err.Error(), "checkout.yml:") || !strings.Contains(err.Error(), "tokenless checkout adapter") || !strings.Contains(err.Error(), "Phase 6") {
				t.Fatalf("CompilePlansWithOptions() error = %v", err)
			}
		})
	}
}

func TestPhase4ContinuationDependsOnCompiledPublicActionsTerminal(t *testing.T) {
	remote := t.TempDir()
	writeAction(t, remote, "", "name: remote\nruns:\n  using: node24\n  main: index.js\n")
	workflowPath := filepath.Join("..", "..", "testdata", "phase4", ".github", "workflows", "public-actions.yml")
	eventPath := filepath.Join("..", "..", "testdata", "phase4", "events", "public-checkout.json")
	plans, err := CompilePlansWithOptions(workflowPath, readFile(t, workflowPath), readFile(t, eventPath), "phase4-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dependedOn := make(map[string]bool)
	for _, job := range plans {
		for _, dependency := range job.Dependencies {
			dependedOn[dependency] = true
		}
	}
	var terminals []string
	for _, job := range plans {
		if !dependedOn[job.Target.StepKey] {
			terminals = append(terminals, job.Target.StepKey)
		}
	}
	continuationSource := readFile(t, filepath.Join("..", "..", ".buildkite", "phase-4-upload-continuation.yml"))
	var continuation struct {
		Steps []struct {
			DependsOn []struct {
				Step string `yaml:"step"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(continuationSource, &continuation); err != nil {
		t.Fatal(err)
	}
	if len(terminals) != 1 || len(continuation.Steps) != 1 || len(continuation.Steps[0].DependsOn) != 1 || continuation.Steps[0].DependsOn[0].Step != terminals[0] {
		t.Fatalf("Phase 4 continuation dependencies = %#v, compiled public actions terminals = %#v", continuation.Steps, terminals)
	}
}

func TestPhase5ContinuationDependsOnCompiledDockerfileActionTerminal(t *testing.T) {
	remote := t.TempDir()
	writeAction(t, remote, "", "name: remote Docker\nruns:\n  using: docker\n  image: Dockerfile\n")
	workflowPath := filepath.Join("..", "..", "testdata", "phase5", ".github", "workflows", "docker-action.yml")
	templatePath := workflowPath + ".tmpl"
	plans, err := CompilePlansWithOptions(workflowPath, readFile(t, templatePath), pushEvent(t), "phase5-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"docker", "network"}) || len(plans[0].Actions) != 1 || plans[0].Actions[0].Source != "github" {
		t.Fatalf("Phase 5 Dockerfile action plans = %#v", plans)
	}
	continuationSource := readFile(t, filepath.Join("..", "..", ".buildkite", "phase-5-docker-action-continuation.yml"))
	var continuation struct {
		Steps []struct {
			DependsOn []struct {
				Step string `yaml:"step"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(continuationSource, &continuation); err != nil {
		t.Fatal(err)
	}
	if len(continuation.Steps) != 1 || len(continuation.Steps[0].DependsOn) != 1 || continuation.Steps[0].DependsOn[0].Step != plans[0].Target.StepKey {
		t.Fatalf("Phase 5 continuation dependencies = %#v, compiled terminal = %q", continuation.Steps, plans[0].Target.StepKey)
	}
}

func TestCompilePlansRemoteActionRequiresSource(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "remote.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/repo@v1\n")
	_, err := CompilePlansWithOptions(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
	})
	if err == nil || !strings.Contains(err.Error(), "remote action source is not configured") {
		t.Fatalf("CompilePlansWithOptions() error = %v, want source configuration rejection", err)
	}
}

func TestCompilePlansContextCancelsRemoteResolution(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "remote.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/repo@v1\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CompilePlansContext(ctx, workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   contextActionSource{},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompilePlansContext() error = %v, want context cancellation", err)
	}
}

func TestLivePublicActionCompatibility(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_ACTIONS") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_ACTIONS=1 to query and download public GitHub actions anonymously")
	}
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "public-actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  actions:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - uses: actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38
        with:
          node-version: "24"
          package-manager-cache: "false"
      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16
        with:
          go-version: "1.26.5"
          cache: "false"
`)
	resolver, err := source.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	actionCache := filepath.Join(t.TempDir(), "actions")
	store, err := source.NewStore(actionCache, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	plans, err := CompilePlansContext(ctx, workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   PublicActionSource{Resolver: resolver, Store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.SchemaV3 || len(plans[0].Actions) != 3 {
		t.Fatalf("public action plan = %#v", plans)
	}
	wantCommits := map[string]string{
		"actions/checkout":   "3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/setup-node": "249970729cb0ef3589644e2896645e5dc5ba9c38",
		"actions/setup-go":   "924ae3a1cded613372ab5595356fb5720e22ba16",
	}
	for _, lock := range plans[0].Actions {
		if want := wantCommits[lock.Repository]; lock.Commit != want || lock.SourceDigest == "" {
			t.Fatalf("public action lock = %#v, want commit %q", lock, want)
		}
	}
	checkoutRoot := filepath.Join(actionCache, "actions", "checkout", wantCommits["actions/checkout"], "tree")
	checkout, err := metadata.Load(checkoutRoot, ".")
	if err != nil {
		t.Fatal(err)
	}
	if token := checkout.Inputs["token"].Default; token == nil || *token != "${{ github.token }}" {
		t.Fatalf("actions/checkout token default = %#v, want github.token", token)
	}
	distribution, err := os.ReadFile(filepath.Join(checkoutRoot, "dist", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(distribution, []byte("getInput('token', { required: true })")) || !bytes.Contains(distribution, []byte("Input required and not supplied:")) {
		t.Fatal("pinned actions/checkout no longer requires a token before Git; revisit the built-in tokenless adapter")
	}
}
