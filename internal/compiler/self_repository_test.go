package compiler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// Exercise the unchanged pytest consumer and downloaded action trees. This
// follows the existing opt-in public action test convention, not a hosted run.
func TestLivePytestSelfRepositoryAction(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_ACTIONS") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_ACTIONS=1 to download pinned pytest and setup-uv source")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	const repository = "pytest-dev/pytest"
	const commit = "a315b97ce61f158bef9957152c8415fe743aa553"
	const workflowPath = ".github/workflows/test.yml"
	resolver, err := actionsource.NewResolver(nil, actionsource.WithGitHubActionSourceTokenProvider("event/repo", func(context.Context) (string, error) {
		t.Fatal("pinned self and setup-uv references requested a source token")
		return "", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	store, err := actionsource.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	source := MemoizeRepositorySource(PublicActionSource{Resolver: resolver, Store: store})
	ref, err := actionsource.Parse(repository + "/" + workflowPath + "@" + commit)
	if err != nil {
		t.Fatal(err)
	}
	ref.RepositoryRoot = true
	_, materialized, err := source.Fetch(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer materialized.Release()
	data := readFile(t, filepath.Join(materialized.RepositoryRoot, filepath.FromSlash(workflowPath)))
	parsed, err := workflow.Parse(workflowPath, data)
	if err != nil {
		t.Fatal(err)
	}
	var selfStep workflow.Step
	for _, job := range parsed.Jobs {
		if job.ID != "build" {
			continue
		}
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "$/") {
				selfStep = step
				if step.With["python-version"] != "${{ matrix.python }}" {
					t.Fatalf("pytest setup-tox input changed: %#v", step.With)
				}
			}
		}
	}
	if selfStep.Uses != "$/.github/actions/setup-tox" || parsed.Permissions == nil || len(parsed.Permissions.Scopes) != 0 {
		t.Fatalf("pytest self action or top-level permissions changed: %#v", parsed)
	}
	options := defaultOptions()
	options.WorkflowSource = &WorkflowSourceReference{Repository: repository, Commit: commit}
	options.RepositorySource = source
	options.ResolveActions = true
	instance := JobInstance{
		LogicalJobID: "build", SourcePath: "./" + workflowPath, SourceDigest: "sha256:" + sha256Sum(data),
		RepositoryRoot: t.TempDir(), Steps: []workflow.Step{selfStep},
		Matrix: map[string]any{"name": "windows-py311", "python": "3.11", "tox_env": "py311"},
	}
	event, err := ParseEvent(pushEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	builder := planBuilder{ctx: ctx, options: options, actionSource: source, ir: IR{Event: event, Workflow: WorkflowSource{WorkflowTokenPermissions: parsed.Permissions.Scopes}}}
	workflowProgram := lowerWorkflowProgram(instance)
	compiled, err := builder.buildActions(instance, &workflowProgram)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.locks) != 2 || !compiled.requiresMise || !slices.Contains(compiled.capabilities, "network") || !compiled.requiresGitHubToken || len(compiled.requiredSecrets) != 0 {
		t.Fatalf("pytest action graph = %#v", compiled)
	}
	// setup-uv defaults github-token to github.token. Source resolution must
	// not erase that authority or grant it under pytest's empty root policy.
	workflowProgram.Actions = compiled.programs
	if _, _, _, err := builder.authorizePlanSecrets(instance, workflowProgram, &compiled); err == nil || !strings.Contains(err.Error(), "action input default that references github.token but has no effective permissions") {
		t.Fatalf("pytest empty-permissions boundary = %v", err)
	} else {
		t.Log(err)
	}
	want := map[string]string{repository: commit, "astral-sh/setup-uv": "11f9893b081a58869d3b5fccaea48c9e9e46f990"}
	for _, lock := range compiled.locks {
		if lock.Commit != want[lock.Repository] || lock.SourceDigest == "" {
			t.Fatalf("pytest source lock = %#v", lock)
		}
		t.Logf("%s/%s@%s: %s", lock.Repository, lock.Path, lock.Commit, lock.SourceDigest)
	}
}

func TestSelfRepositoryRootVerifiesWorkflowBytesWithoutCheckout(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	text := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/\n"
	caller := writeWorkflow(t, workspace, "ci.yml", text)
	writeWorkflow(t, remote, "ci.yml", text)
	writeSelfAction(t, remote, "", "runs:\n  using: composite\n  steps:\n    - run: echo remote\n      shell: bash\n")
	fake := newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote})
	options := defaultOptions()
	options.WorkflowSource = &WorkflowSourceReference{Repository: "source/repo", Commit: strings.Repeat("e", 40)}
	options.RepositorySource = MemoizeRepositorySource(fake)
	options.ActionSource, options.ResolveActions = options.RepositorySource, true
	plans, err := compilePlansForTest(t.Context(), caller, []byte(text), pushEvent(t), "test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	job := plans[0]
	if job.Workflow.SelfRepository == nil || job.Workflow.SelfRepository.Repository != "source/repo" || job.Actions[0].Commit != strings.Repeat("e", 40) || job.Actions[0].Path != "" || job.Event.Repository == "source/repo" || job.Workflow.Remote != nil {
		t.Fatalf("root self identity = %#v", job)
	}
	if job.Program.Job.Steps[0].Invocation.Uses.Source != "$/" {
		t.Fatal("authored self reference was rewritten")
	}
	validateCompiledPlansAgainstSchema(t, plans)
	for _, mutate := range []func(*plan.Job){
		func(j *plan.Job) { j.Workflow.SelfRepository = nil },
		func(j *plan.Job) { j.Actions[0].Repository = "event/repo" },
		func(j *plan.Job) { j.Actions[0].Commit = strings.Repeat("f", 40) },
		func(j *plan.Job) { j.Actions[0].SourceDigest = "sha256:" + strings.Repeat("f", 64) },
	} {
		changed := job
		changed.Actions = slices.Clone(job.Actions)
		mutate(&changed)
		if err := changed.Validate(); err == nil || !strings.Contains(err.Error(), "self-repository") {
			t.Fatalf("plan accepted unrelated self source: %v", err)
		}
	}
	// The disk file still matches the remote. Verification must bind supplied
	// compiler bytes, not reread the disk file and mistakenly approve them.
	if _, err := compilePlansForTest(t.Context(), caller, []byte(text+"# edited\n"), pushEvent(t), "test", testDistributionDigest, options); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatching supplied workflow accepted: %v", err)
	}
	options.WorkflowSource = nil
	if _, err := compilePlansForTest(t.Context(), caller, []byte(text), pushEvent(t), "test", testDistributionDigest, options); err == nil || !strings.Contains(err.Error(), "containing workflow repository") {
		t.Fatalf("event identity substituted for missing workflow identity: %v", err)
	}
}

func TestSelfRepositoryInsideLocalReusableWorkflow(t *testing.T) {
	for _, uses := range []string{"$/action", "$/.github/workflows/leaf.yml"} {
		t.Run(uses, func(t *testing.T) {
			workspace, remote := t.TempDir(), t.TempDir()
			caller := writeWorkflow(t, workspace, "ci.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/local.yml\n")
			local := "on: workflow_call\njobs:\n  child:\n"
			if strings.HasSuffix(uses, ".yml") {
				local += "    uses: " + uses + "\n"
			} else {
				local += "    runs-on: ubuntu-latest\n    steps:\n      - uses: " + uses + "\n"
			}
			writeWorkflow(t, workspace, "local.yml", local)
			writeWorkflow(t, remote, "local.yml", local)
			writeWorkflow(t, remote, "leaf.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/action\n")
			writeSelfAction(t, remote, "action", "runs:\n  using: composite\n  steps:\n    - run: echo pinned\n      shell: bash\n")
			fake := newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote})
			options := defaultOptions()
			options.WorkflowSource = &WorkflowSourceReference{Repository: "source/repo", Commit: strings.Repeat("e", 40)}
			options.RepositorySource = MemoizeRepositorySource(fake)
			options.ActionSource, options.ResolveActions = options.RepositorySource, true
			plans, err := compilePlansForTest(t.Context(), caller, readFile(t, caller), pushEvent(t), "test", testDistributionDigest, options)
			if err != nil || len(plans) != 1 || plans[0].Actions[0].Repository != "source/repo" {
				t.Fatalf("local reusable self resolution: plans=%#v err=%v", plans, err)
			}
			validateCompiledPlansAgainstSchema(t, plans)
			writeWorkflow(t, workspace, "local.yml", local+"# modified locally\n")
			if _, err := compilePlansForTest(t.Context(), caller, readFile(t, caller), pushEvent(t), "test", testDistributionDigest, options); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("local reusable bytes were not verified: %v", err)
			}
		})
	}
}

func TestSelfRepositoryReusableRootBytesAndCycle(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	text := "on: [push, workflow_call]\njobs:\n  call:\n    uses: $/.github/workflows/leaf.yml\n"
	caller := writeWorkflow(t, workspace, "ci.yml", text)
	writeWorkflow(t, remote, "ci.yml", text)
	writeWorkflow(t, remote, "leaf.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo leaf\n")
	options := defaultOptions()
	options.WorkflowSource = &WorkflowSourceReference{Repository: "source/repo", Commit: strings.Repeat("e", 40)}
	options.RepositorySource = MemoizeRepositorySource(newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote}))
	if _, err := compilePlansForTest(t.Context(), caller, []byte(text), pushEvent(t), "test", testDistributionDigest, options); err != nil {
		t.Fatal(err)
	}
	if _, err := compilePlansForTest(t.Context(), caller, []byte(text+"# changed input\n"), pushEvent(t), "test", testDistributionDigest, options); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("root reusable call did not verify supplied bytes: %v", err)
	}
	writeWorkflow(t, remote, "leaf.yml", "on: workflow_call\njobs:\n  call:\n    uses: $/.github/workflows/ci.yml\n")
	options.RepositorySource = MemoizeRepositorySource(newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote}))
	if _, err := compilePlansForTest(t.Context(), caller, []byte(text), pushEvent(t), "test", testDistributionDigest, options); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("self-repository reusable cycle error = %v", err)
	}
	if err := os.Remove(filepath.Join(remote, ".github", "workflows", "leaf.yml")); err != nil {
		t.Fatal(err)
	}
	options.RepositorySource = MemoizeRepositorySource(newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote}))
	if _, err := compilePlansForTest(t.Context(), caller, []byte(text), pushEvent(t), "test", testDistributionDigest, options); err == nil || !strings.Contains(err.Error(), `"$/.github/workflows/leaf.yml"`) {
		t.Fatalf("missing workflow diagnostic lost authored self reference: %v", err)
	}
}

func TestSelfRepositoryActionsFollowContainingSources(t *testing.T) {
	workspace, workflowRoot, compositeRoot := t.TempDir(), t.TempDir(), t.TempDir()
	caller := writeWorkflow(t, workspace, "caller.yml", "on: push\njobs:\n  call:\n    uses: workflows/repo/.github/workflows/ci.yml@release\n")
	writeWorkflow(t, workflowRoot, "ci.yml", `on: workflow_call
jobs:
  nested:
    uses: $/.github/workflows/nested.yml
`)
	writeWorkflow(t, workflowRoot, "nested.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/repo/parent@moving
      - uses: $/first
      - uses: ./child
`)
	writeSelfAction(t, workflowRoot, "first", "runs:\n  using: composite\n  steps:\n    - uses: $/leaf\n")
	writeSelfAction(t, workflowRoot, "leaf", "runs:\n  using: composite\n  steps:\n    - run: echo workflow\n      shell: bash\n")
	writeSelfAction(t, workflowRoot, "workspace-leaf", "runs:\n  using: composite\n  steps:\n    - run: echo workflow fallback\n      shell: bash\n")
	writeSelfAction(t, compositeRoot, "parent", "runs:\n  using: composite\n  steps:\n    - uses: $/child\n    - uses: ./child\n")
	writeSelfAction(t, compositeRoot, "child", "runs:\n  using: composite\n  steps:\n    - uses: $/leaf\n")
	writeSelfAction(t, compositeRoot, "leaf", "runs:\n  using: composite\n  steps:\n    - run: echo remote\n      shell: bash\n")
	writeSelfAction(t, workspace, "child", "runs:\n  using: composite\n  steps:\n    - uses: $/workspace-leaf\n")
	fake := newFakeReusableRepositorySource(t, map[string]string{"workflows/repo": workflowRoot, "actions/repo": compositeRoot})
	fake.commits["workflows/repo"] = strings.Repeat("b", 40)
	fake.commits["actions/repo"] = strings.Repeat("c", 40)
	options := defaultOptions()
	options.WorkflowSource = &WorkflowSourceReference{Repository: "event/repo", Commit: strings.Repeat("a", 40)}
	options.RepositorySource = MemoizeRepositorySource(fake)
	options.ResolveActions, options.ActionSource = true, options.RepositorySource
	plans, err := compilePlansForTest(t.Context(), caller, readFile(t, caller), pushEvent(t), "test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d", len(plans))
	}
	job := plans[0]
	if job.Event.Repository == "workflows/repo" || job.Event.Repository == "actions/repo" || job.Event.SHA == fake.commits["workflows/repo"] {
		t.Fatalf("test must distinguish event and containing identities: %#v", job.Event)
	}
	if len(job.Actions) != 7 || job.GitHubToken != nil || !slices.Contains(job.RequiredCapabilities, "network") {
		t.Fatalf("action graph or authority = %#v", job)
	}
	byID := make(map[string]plan.ActionLock)
	for _, lock := range job.Actions {
		byID[lock.ID] = lock
		if lock.Source == "github" && (lock.Commit != fake.commits[lock.Repository] || lock.SourceDigest != fake.digests[lock.Repository]) {
			t.Fatalf("lock lost containing source: %#v", lock)
		}
	}
	for _, lock := range job.Actions {
		for uses, selector := range lock.Children {
			child := byID[selector.Lock]
			if strings.HasPrefix(uses, "$/") {
				repository, commit := lock.Repository, lock.Commit
				if lock.Source == "workspace" {
					repository, commit = "workflows/repo", strings.Repeat("b", 40)
				}
				if child.Repository != repository || child.Commit != commit {
					t.Fatalf("%s child of %#v resolved to %#v", uses, lock, child)
				}
			}
			if uses == "./child" && child.Source != "workspace" {
				t.Fatalf("checkout-relative child changed: %#v", child)
			}
		}
	}
	workflowFetches := 0
	for _, ref := range fake.references() {
		if ref.RepositoryRoot {
			workflowFetches++
			if ref.Raw != "workflows/repo/.github/workflows/ci.yml@release" {
				t.Fatalf("unexpected repository authorization: %#v", ref)
			}
		} else if ref.Path != "parent" && ref.Ref != fake.commits[ref.Owner+"/"+ref.Repository] {
			t.Fatalf("self action re-resolved mutable ref: %#v", ref)
		}
	}
	if workflowFetches != 1 {
		t.Fatalf("workflow fetches = %d, want one authorized containing tree", workflowFetches)
	}
	validateCompiledPlansAgainstSchema(t, plans)
}

type privateSelfRepositorySource struct{ *fakeReusableRepositorySource }

func (s privateSelfRepositorySource) Fetch(ctx context.Context, ref actionsource.Reference) (actionsource.Resolved, actionsource.Materialized, error) {
	if !ref.RepositoryRoot {
		return actionsource.Resolved{}, actionsource.Materialized{}, &actionsource.NotPublicError{}
	}
	return s.fakeReusableRepositorySource.Fetch(ctx, ref)
}

func TestSelfRepositoryDoesNotGrantPrivateActionAccess(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	caller := writeWorkflow(t, workspace, "ci.yml", "on: push\njobs:\n  call:\n    uses: private/repo/.github/workflows/ci.yml@v1\n")
	writeWorkflow(t, remote, "ci.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/action\n")
	writeSelfAction(t, remote, "action", "runs:\n  using: composite\n  steps:\n    - run: echo private\n      shell: bash\n")
	options := defaultOptions()
	options.RepositorySource = MemoizeRepositorySource(privateSelfRepositorySource{newFakeReusableRepositorySource(t, map[string]string{"private/repo": remote})})
	options.ActionSource, options.ResolveActions = options.RepositorySource, true
	var denied *actionsource.NotPublicError
	if _, err := compilePlansForTest(t.Context(), caller, readFile(t, caller), pushEvent(t), "test", testDistributionDigest, options); !errors.As(err, &denied) {
		t.Fatalf("private workflow cache granted action access: %v", err)
	}
}

func TestSelfRepositoryActionCyclesAndDepth(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(fmt.Sprint(cycle), func(t *testing.T) {
			root := t.TempDir()
			for i := range metadata.MaxNestedActionDepth + 1 {
				next := i + 1
				if cycle {
					next = 0
				}
				writeSelfAction(t, root, fmt.Sprint(i), fmt.Sprintf("runs:\n  using: composite\n  steps:\n    - uses: $/%d\n", next))
			}
			fake := newFakeReusableRepositorySource(t, map[string]string{"source/repo": root})
			_, _, _, _, err := compileActionLocks(t.Context(), t.TempDir(), fake, []string{"source/repo/0@v1"})
			want := "maximum depth"
			if cycle {
				want = "recursion detected"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("self graph bound error = %v, want %s", err, want)
			}
		})
	}
}

func writeSelfAction(t *testing.T, root, path, metadata string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action.yml"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSelfRepositoryPathsAndMissingIdentity(t *testing.T) {
	identity := &RemoteWorkflowSource{Repository: "owner/repo", Commit: strings.Repeat("a", 40)}
	for _, uses := range []string{"$/../escape", "$//absolute", "$/a/../b", "$/a\\b", "$/a@main", "$/./child", "$/a?query", "$/a\n"} {
		t.Run(uses, func(t *testing.T) {
			if _, err := selfRepositoryReference(uses, identity); err == nil {
				t.Fatalf("accepted unsafe path %q", uses)
			}
		})
	}
	if _, err := selfRepositoryReference("$/child", nil); err == nil || !strings.Contains(err.Error(), "containing workflow repository") {
		t.Fatalf("missing identity error = %v", err)
	}
}

func TestSelfRepositorySecretForwardingRetainsRootScope(t *testing.T) {
	for _, paths := range []struct{ first, second string }{{"$/", "$/"}, {"$/", "./"}, {"./", "$/"}} {
		for _, mode := range []string{"inherit", "map"} {
			for _, omitted := range []string{"none", "first", "second"} {
				t.Run(paths.first+paths.second+"/"+mode+"/omit-"+omitted, func(t *testing.T) {
					workspace, remote := t.TempDir(), t.TempDir()
					first, second := "    secrets: inherit\n", "    secrets: inherit\n"
					ordinary, token := "ORIGINAL", "optional_token"
					if mode == "map" {
						first = "    secrets:\n      middle_alias: ${{ secrets.ORIGINAL }}\n      middle_token: ${{ secrets.GITHUB_TOKEN }}\n"
						second = "    secrets:\n      leaf_alias: ${{ secrets.middle_alias }}\n      leaf_token: ${{ secrets.middle_token }}\n"
						ordinary, token = "leaf_alias", "leaf_token"
					}
					switch omitted {
					case "first":
						first = ""
					case "second":
						second = ""
					}
					callerText := "on: push\npermissions: {contents: read}\njobs:\n  call:\n    uses: " + paths.first + ".github/workflows/middle.yml\n" + first
					caller := writeWorkflow(t, workspace, "caller.yml", callerText)
					writeWorkflow(t, remote, "caller.yml", callerText)
					middle := "on:\n  workflow_call:\n    secrets:\n      middle_alias:\n      middle_token:\njobs:\n  nested:\n    uses: " + paths.second + ".github/workflows/leaf.yml\n" + second
					writeWorkflow(t, workspace, "middle.yml", middle)
					writeWorkflow(t, remote, "middle.yml", middle)
					// Only fetched source contains the leaf; this must not become
					// a workspace call merely to permit forwarding.
					leaf := fmt.Sprintf("on:\n  workflow_call:\n    secrets:\n      leaf_alias:\n      leaf_token:\n      optional_token:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      VALUE: ${{ secrets.%s }}\n      TOKEN: ${{ secrets.%s }}\n    steps:\n      - run: true\n", ordinary, token)
					writeWorkflow(t, remote, "leaf.yml", leaf)
					fake := newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote})
					options := defaultOptions()
					options.WorkflowSource = &WorkflowSourceReference{Repository: "source/repo", Commit: strings.Repeat("e", 40)}
					options.RepositorySource = MemoizeRepositorySource(fake)
					plans, err := compilePlansForTest(t.Context(), caller, []byte(callerText), pushEvent(t), "test", testDistributionDigest, options)
					if err != nil || len(plans) != 1 {
						t.Fatalf("self forwarding: plans=%#v err=%v", plans, err)
					}
					job := plans[0]
					var wantSecrets []string
					var wantMappings map[string]string
					if omitted == "none" {
						wantSecrets = []string{"ORIGINAL"}
						if mode == "map" {
							wantMappings = map[string]string{"LEAF_ALIAS": "ORIGINAL"}
						} else {
							wantSecrets = []string{"OPTIONAL_TOKEN", "ORIGINAL"}
						}
					}
					if !slices.Equal(job.RequiredSecrets, wantSecrets) || !maps.Equal(job.SecretMappings, wantMappings) || job.HasCapability("secrets") != (len(wantSecrets) > 0) {
						t.Fatalf("forwarded secret scope = %#v / %#v / %#v", job.RequiredSecrets, job.SecretMappings, job.RequiredCapabilities)
					}
					wantToken := mode == "map" && omitted == "none"
					if (job.GitHubToken != nil) != wantToken || job.HasCapability("provider-token-write") != wantToken {
						t.Fatalf("token scope = %#v / %#v", job.GitHubToken, job.RequiredCapabilities)
					}
					if wantToken && (!slices.Equal(job.GitHubToken.Aliases, []string{"LEAF_TOKEN"}) || !maps.Equal(job.GitHubToken.Permissions, map[string]string{"contents": "read"})) {
						t.Fatalf("token alias or permissions changed: %#v", job.GitHubToken)
					}
					if job.Workflow.Remote == nil || job.Workflow.Remote.Repository != "source/repo" || job.Workflow.Remote.Commit != strings.Repeat("e", 40) || job.Workflow.Remote.SourceDigest != fake.digests["source/repo"] || job.Workflow.Digest != "sha256:"+sha256Sum([]byte(leaf)) {
						t.Fatalf("forwarding changed immutable provenance: %#v", job.Workflow)
					}
					validateCompiledPlansAgainstSchema(t, plans)
				})
			}
		}
	}
}

func TestSelfRepositorySecretForwardingCannotReenterRootScope(t *testing.T) {
	for _, test := range []struct{ name, first, middle, last string }{
		{"self then remote", "$/.github/workflows/middle.yml", "other/repo/.github/workflows/leaf.yml@v1", ""},
		{"remote then self", "other/repo/.github/workflows/middle.yml@v1", "$/.github/workflows/leaf.yml", ""},
		{"remote then local then self", "other/repo/.github/workflows/middle.yml@v1", "./.github/workflows/leaf.yml", "$/.github/workflows/end.yml"},
		{"same repository remote then self then local", "source/repo/.github/workflows/middle.yml@" + strings.Repeat("e", 40), "$/.github/workflows/leaf.yml", "./.github/workflows/end.yml"},
	} {
		for _, forwarding := range []string{"    secrets: inherit\n", "    secrets:\n      token: ${{ secrets.GITHUB_TOKEN }}\n"} {
			t.Run(test.name+"/"+strings.TrimSpace(forwarding), func(t *testing.T) {
				workspace, remote, other := t.TempDir(), t.TempDir(), t.TempDir()
				callerText := "on: push\npermissions: {contents: read}\njobs:\n  call:\n    uses: " + test.first + "\n"
				caller := writeWorkflow(t, workspace, "caller.yml", callerText)
				writeWorkflow(t, remote, "caller.yml", callerText)
				middle := "on: workflow_call\njobs:\n  nested:\n    uses: " + test.middle + "\n"
				leaf := "on:\n  workflow_call:\n    secrets:\n      token:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets.token }}\n    steps:\n      - run: true\n"
				if test.last == "" {
					middle += forwarding
				} else {
					leaf = "on: workflow_call\njobs:\n  last:\n    uses: " + test.last + "\n" + forwarding
				}
				for _, root := range []string{remote, other} {
					writeWorkflow(t, root, "middle.yml", middle)
					writeWorkflow(t, root, "leaf.yml", leaf)
					writeWorkflow(t, root, "end.yml", "on:\n  workflow_call:\n    secrets:\n      token:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
				}
				options := defaultOptions()
				options.WorkflowSource = &WorkflowSourceReference{Repository: "source/repo", Commit: strings.Repeat("e", 40)}
				options.RepositorySource = MemoizeRepositorySource(newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote, "other/repo": other}))
				_, err := compilePlansForTest(t.Context(), caller, []byte(callerText), pushEvent(t), "test", testDistributionDigest, options)
				if err == nil || !strings.Contains(err.Error(), "cannot forward secrets to a workflow in another repository") {
					t.Fatalf("remote chain regained forwarding eligibility: %v", err)
				}
			})
		}
	}
}
