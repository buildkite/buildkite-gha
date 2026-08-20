package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

type fakeReusableRepositorySource struct {
	mu              sync.Mutex
	roots           map[string]string
	commits         map[string]string
	digests         map[string]string
	driftPathDigest string
	calls           []actionsource.Reference
	err             error
}

func newFakeReusableRepositorySource(t *testing.T, roots map[string]string) *fakeReusableRepositorySource {
	t.Helper()
	source := &fakeReusableRepositorySource{
		roots: roots, commits: make(map[string]string, len(roots)), digests: make(map[string]string, len(roots)),
	}
	index := 0
	for repository, root := range roots {
		digest, err := actionsource.DigestTree(root)
		if err != nil {
			t.Fatal(err)
		}
		source.digests[repository] = digest
		source.commits[repository] = strings.Repeat(string(rune('a'+index)), 40)
		index++
	}
	return source
}

func (s *fakeReusableRepositorySource) Fetch(ctx context.Context, ref actionsource.Reference) (actionsource.Resolved, actionsource.Materialized, error) {
	if err := ctx.Err(); err != nil {
		return actionsource.Resolved{}, actionsource.Materialized{}, err
	}
	s.mu.Lock()
	s.calls = append(s.calls, ref)
	s.mu.Unlock()
	if s.err != nil {
		return actionsource.Resolved{}, actionsource.Materialized{}, s.err
	}
	repository := strings.ToLower(ref.Owner + "/" + ref.Repository)
	root := s.roots[repository]
	commit := s.commits[repository]
	digest := s.digests[repository]
	if len(ref.Ref) == 40 && strings.Trim(ref.Ref, "0123456789abcdef") == "" {
		commit = ref.Ref
	}
	if ref.Path != "" && s.driftPathDigest != "" {
		digest = s.driftPathDigest
	}
	return actionsource.Resolved{Reference: ref, Commit: commit, SourceDigest: digest}, actionsource.Materialized{
		RepositoryRoot: root, ActionRoot: filepath.Join(root, filepath.FromSlash(ref.Path)), SourceDigest: digest,
	}, nil
}

func (s *fakeReusableRepositorySource) references() []actionsource.Reference {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]actionsource.Reference(nil), s.calls...)
}

func TestCompilePublicReusableWorkflowWithNestedPinnedLocalCall(t *testing.T) {
	callerRoot := t.TempDir()
	callerPath := writeWorkflow(t, callerRoot, "caller.yml", "on: push\njobs:\n  delegated:\n    uses: Octo/Workflows/.github/workflows/ci.yml@v1\n")
	remoteRoot := t.TempDir()
	writeWorkflow(t, remoteRoot, "ci.yml", `on: workflow_call
jobs:
  prepare:
    runs-on: ubuntu-latest
    steps:
      - run: prepare
  nested:
    needs: prepare
    uses: ./.github/workflows/nested.yml
`)
	nestedPath := writeWorkflow(t, remoteRoot, "nested.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: nested
`)
	fake := newFakeReusableRepositorySource(t, map[string]string{"octo/workflows": remoteRoot})
	shared := MemoizeRepositorySource(fake)
	options := defaultOptions()
	options.RepositorySource = shared

	compiled, err := CompileWithOptionsContext(t.Context(), callerPath, readFile(t, callerPath), pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := unmarshalJSON(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(ir.Jobs))
	}
	byID := make(map[string]JobInstance, len(ir.Jobs))
	for _, job := range ir.Jobs {
		byID[job.LogicalJobID] = job
	}
	direct := byID["delegated.prepare"]
	nested := byID["delegated.nested.test"]
	if direct.SourcePath != "octo/workflows/.github/workflows/ci.yml@v1" || nested.SourcePath != "octo/workflows/.github/workflows/nested.yml@v1" {
		t.Fatalf("remote source paths = %q / %q", direct.SourcePath, nested.SourcePath)
	}
	if nested.RemoteWorkflow == nil || nested.RemoteWorkflow.Repository != "octo/workflows" || nested.RemoteWorkflow.RequestedRef != "v1" || nested.RemoteWorkflow.Commit != fake.commits["octo/workflows"] || nested.RemoteWorkflow.SourceDigest != fake.digests["octo/workflows"] {
		t.Fatalf("remote workflow provenance = %#v", nested.RemoteWorkflow)
	}
	wantNestedDigest := "sha256:" + sha256Sum(readFile(t, nestedPath))
	if nested.SourceDigest != wantNestedDigest {
		t.Fatalf("nested file binding = %q, want %q", nested.SourceDigest, wantNestedDigest)
	}

	plans, err := compilePlansForTest(t.Context(), callerPath, readFile(t, callerPath), pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[1].Workflow.Remote == nil || plans[1].Workflow.Path != nested.SourcePath || plans[1].Workflow.Digest != wantNestedDigest || plans[1].Workflow.Remote.SourceDigest != fake.digests["octo/workflows"] {
		t.Fatalf("remote plan provenance = %#v", plans)
	}
	if calls := fake.references(); len(calls) != 1 || calls[0].Raw != "Octo/Workflows@v1" || calls[0].Path != "" {
		t.Fatalf("repository source calls = %#v, want one repository-root resolution", calls)
	}
}

func TestCompileRejectsSecretInheritanceIntoRemoteReusableWorkflows(t *testing.T) {
	for _, test := range []struct {
		name   string
		caller string
		remote string
	}{
		{
			name:   "direct remote call",
			caller: "on: push\njobs:\n  call:\n    uses: owner/workflows/.github/workflows/ci.yml@v1\n    secrets: inherit\n",
			remote: "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
		{
			name:   "local path within remote repository",
			caller: "on: push\njobs:\n  call:\n    uses: owner/workflows/.github/workflows/ci.yml@v1\n",
			remote: "on: workflow_call\njobs:\n  nested:\n    uses: ./.github/workflows/nested.yml\n    secrets: inherit\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			callerRoot := t.TempDir()
			callerPath := writeWorkflow(t, callerRoot, "caller.yml", test.caller)
			remoteRoot := t.TempDir()
			writeWorkflow(t, remoteRoot, "ci.yml", test.remote)
			writeWorkflow(t, remoteRoot, "nested.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
			options := defaultOptions()
			options.RepositorySource = MemoizeRepositorySource(newFakeReusableRepositorySource(t, map[string]string{"owner/workflows": remoteRoot}))
			_, err := CompileWithOptions(callerPath, readFile(t, callerPath), pushEvent(t), options)
			if err == nil || !strings.Contains(err.Error(), "secrets: inherit is supported only for repository-local reusable workflows") {
				t.Fatalf("CompileWithOptions() error = %v, want remote inheritance rejection", err)
			}
		})
	}
}

func TestCompilePublicReusableWorkflowDefersCallerNeedOutputInput(t *testing.T) {
	callerRoot := t.TempDir()
	callerPath := writeWorkflow(t, callerRoot, "caller.yml", `on: push
jobs:
  hash:
    runs-on: ubuntu-latest
    outputs:
      hashes: ${{ steps.hash.outputs.hashes }}
    steps:
      - id: hash
        run: echo hashes=c2hhMjU2ICBzdWJqZWN0Cg== >> "$GITHUB_OUTPUT"
  call-remote:
    needs: hash
    uses: slsa-framework/slsa-github-generator/.github/workflows/generator_generic_slsa3.yml@v2.1.0
    with:
      base64-subjects: ${{ needs.hash.outputs.hashes }}
`)
	remoteRoot := t.TempDir()
	writeWorkflow(t, remoteRoot, "generator_generic_slsa3.yml", `on:
  workflow_call:
    inputs:
      base64-subjects:
        required: false
        type: string
jobs:
  generator:
    runs-on: ubuntu-latest
    steps:
      - name: Create subject file
        env:
          UNTRUSTED_SUBJECTS: ${{ inputs.base64-subjects }}
        run: test -n "$UNTRUSTED_SUBJECTS"
`)
	fake := newFakeReusableRepositorySource(t, map[string]string{"slsa-framework/slsa-github-generator": remoteRoot})
	options := defaultOptions()
	options.RepositorySource = MemoizeRepositorySource(fake)

	plans, err := compilePlansForTest(t.Context(), callerPath, readFile(t, callerPath), pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want producer and flattened remote job", len(plans))
	}
	producer, callee := plans[0], plans[1]
	producerPlan, err := plan.Encode(producer)
	if err != nil {
		t.Fatal(err)
	}
	deferred, ok := callee.DeferredInputs["base64-subjects"]
	if !ok || len(deferred.Sources) != 1 || deferred.Sources[0] != (plan.NeedSource{StepKey: producer.Target.StepKey, PlanDigest: transport.Digest(producerPlan)}) {
		t.Fatalf("deferred input sources = %#v", callee.DeferredInputs)
	}
	if len(deferred.Outputs) != 1 || deferred.Outputs[0] != (plan.NeedOutput{Name: "value", StepKey: producer.Target.StepKey, Output: "hashes"}) {
		t.Fatalf("deferred input outputs = %#v", deferred.Outputs)
	}
	if len(callee.Dependencies) != 1 || callee.Dependencies[0] != producer.Target.StepKey || len(callee.NeedSources["hash"]) != 1 || len(callee.NeedOutputs["hash"]) != 0 {
		t.Fatalf("callee dependencies = %#v, needs = %#v / %#v", callee.Dependencies, callee.NeedSources, callee.NeedOutputs)
	}
	if _, exists := callee.Inputs["base64-subjects"]; exists || callee.Steps[0].Env["UNTRUSTED_SUBJECTS"] != "${{ inputs.base64-subjects }}" {
		t.Fatalf("callee deferred input boundary = inputs %#v, step %#v", callee.Inputs, callee.Steps[0])
	}
	if callee.Workflow.Remote == nil || callee.Workflow.Remote.Repository != "slsa-framework/slsa-github-generator" || callee.Workflow.Remote.RequestedRef != "v2.1.0" {
		t.Fatalf("remote provenance = %#v", callee.Workflow.Remote)
	}
}

func TestCompilePinsRemoteWorkflowAndActionToOneCommit(t *testing.T) {
	callerRoot := t.TempDir()
	callerPath := writeWorkflow(t, callerRoot, "caller.yml", "on: push\njobs:\n  delegated:\n    uses: owner/repository/.github/workflows/ci.yml@v1\n")
	remoteRoot := t.TempDir()
	writeWorkflow(t, remoteRoot, "ci.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: owner/repository/action@v1
`)
	actionRoot := filepath.Join(remoteRoot, "action")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte("runs:\n  using: node24\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "index.js"), []byte("console.log('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeReusableRepositorySource(t, map[string]string{"owner/repository": remoteRoot})
	shared := MemoizeRepositorySource(fake)
	options := defaultOptions()
	options.RepositorySource = shared
	options.ResolveActions = true
	options.ActionSource = shared
	plans, err := compilePlansForTest(t.Context(), callerPath, readFile(t, callerPath), pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	commit := fake.commits["owner/repository"]
	if len(plans) != 1 || plans[0].Workflow.Remote == nil || plans[0].Workflow.Remote.Commit != commit || len(plans[0].Actions) != 1 || plans[0].Actions[0].Commit != commit || plans[0].Actions[0].RequestedRef != "v1" {
		t.Fatalf("workflow/action pins = %#v", plans)
	}
	calls := fake.references()
	if len(calls) != 2 || calls[0].Raw != "owner/repository@v1" || calls[1].Raw != "owner/repository/action@"+commit {
		t.Fatalf("repository source calls = %#v, want mutable ref once then exact commit", calls)
	}
}

func TestMemoizeRepositorySourceRejectsTreeDriftAfterPin(t *testing.T) {
	root := t.TempDir()
	fake := newFakeReusableRepositorySource(t, map[string]string{"owner/repository": root})
	fake.driftPathDigest = "sha256:" + strings.Repeat("f", 64)
	shared := MemoizeRepositorySource(fake)
	repositoryRef, err := actionsource.Parse("owner/repository@v1")
	if err != nil {
		t.Fatal(err)
	}
	_, materialized, err := shared.Fetch(t.Context(), repositoryRef)
	if err != nil {
		t.Fatal(err)
	}
	materialized.Release()
	actionRef, err := actionsource.Parse("owner/repository/action@v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := shared.Fetch(t.Context(), actionRef); err == nil || !strings.Contains(err.Error(), "repository source changed after immutable pin") {
		t.Fatalf("Fetch() drift error = %v", err)
	}
}

func TestCompileRemoteReusableWorkflowLocalActionUsesCallerWorkspace(t *testing.T) {
	callerRoot := t.TempDir()
	callerPath := writeWorkflow(t, callerRoot, "caller.yml", "on: push\njobs:\n  delegated:\n    uses: owner/repository/.github/workflows/ci.yml@v1\n")
	actionRoot := filepath.Join(callerRoot, ".github", "actions", "local")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte("runs:\n  using: node24\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "index.js"), []byte("console.log('caller')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteRoot := t.TempDir()
	writeWorkflow(t, remoteRoot, "ci.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n")
	fake := newFakeReusableRepositorySource(t, map[string]string{"owner/repository": remoteRoot})
	options := defaultOptions()
	options.RepositorySource = MemoizeRepositorySource(fake)
	plans, err := compilePlansForTest(t.Context(), callerPath, readFile(t, callerPath), pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Actions) != 1 || plans[0].Actions[0].Source != "workspace" || plans[0].Actions[0].Path != ".github/actions/local" {
		t.Fatalf("remote workflow caller-local action plan = %#v", plans)
	}
	if calls := fake.references(); len(calls) != 1 || calls[0].Path != "" {
		t.Fatalf("repository source calls = %#v, want only the remote workflow repository", calls)
	}
}

func TestCompileRemoteReusableWorkflowCyclesUseSourceIdentity(t *testing.T) {
	t.Run("cross repository cycle", func(t *testing.T) {
		callerRoot := t.TempDir()
		callerPath := writeWorkflow(t, callerRoot, "caller.yml", "on: push\njobs:\n  call:\n    uses: owner/a/.github/workflows/ci.yml@v1\n")
		aRoot := t.TempDir()
		bRoot := t.TempDir()
		writeWorkflow(t, aRoot, "ci.yml", "on: workflow_call\njobs:\n  call:\n    uses: owner/b/.github/workflows/ci.yml@v1\n")
		writeWorkflow(t, bRoot, "ci.yml", "on: workflow_call\njobs:\n  call:\n    uses: owner/a/.github/workflows/ci.yml@v1\n")
		fake := newFakeReusableRepositorySource(t, map[string]string{"owner/a": aRoot, "owner/b": bRoot})
		options := defaultOptions()
		options.RepositorySource = MemoizeRepositorySource(fake)
		_, err := CompileWithOptions(callerPath, readFile(t, callerPath), pushEvent(t), options)
		if err == nil || !strings.Contains(err.Error(), "reusable-workflow cycle detected") || !strings.Contains(err.Error(), "owner/a/.github/workflows/ci.yml@"+fake.commits["owner/a"]) || !strings.Contains(err.Error(), "owner/b/.github/workflows/ci.yml@"+fake.commits["owner/b"]) {
			t.Fatalf("CompileWithOptions() error = %v, want source-aware cross-repository cycle", err)
		}
	})

	t.Run("same path in different repositories", func(t *testing.T) {
		callerRoot := t.TempDir()
		callerPath := writeWorkflow(t, callerRoot, "caller.yml", "on: push\njobs:\n  call:\n    uses: owner/a/.github/workflows/ci.yml@v1\n")
		aRoot := t.TempDir()
		bRoot := t.TempDir()
		writeWorkflow(t, aRoot, "ci.yml", "on: workflow_call\njobs:\n  call:\n    uses: owner/b/.github/workflows/ci.yml@v1\n")
		writeWorkflow(t, bRoot, "ci.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		fake := newFakeReusableRepositorySource(t, map[string]string{"owner/a": aRoot, "owner/b": bRoot})
		options := defaultOptions()
		options.RepositorySource = MemoizeRepositorySource(fake)
		compiled, err := CompileWithOptions(callerPath, readFile(t, callerPath), pushEvent(t), options)
		if err != nil {
			t.Fatal(err)
		}
		var ir IR
		if err := unmarshalJSON(compiled, &ir); err != nil || len(ir.Jobs) != 1 || ir.Jobs[0].RemoteWorkflow.Repository != "owner/b" {
			t.Fatalf("compiled remote chain = %#v, %v", ir.Jobs, err)
		}
	})
}

func TestCompileRemoteReusableWorkflowLimitsAndDiagnostics(t *testing.T) {
	callerRoot := t.TempDir()
	event := pushEvent(t)

	t.Run("top-level workflow file size", func(t *testing.T) {
		callerPath := filepath.Join(callerRoot, ".github", "workflows", "oversized-top-level.yml")
		source := []byte("on: push\njobs: {}\n#" + strings.Repeat("x", MaxReusableWorkflowBytes))
		_, err := Compile(callerPath, source, event)
		if err == nil || !strings.Contains(err.Error(), "workflow exceeds 1048576-byte limit") {
			t.Fatalf("Compile() error = %v, want bounded top-level workflow load", err)
		}
	})

	t.Run("workflow file size", func(t *testing.T) {
		callerPath := writeWorkflow(t, callerRoot, "oversized-caller.yml", "on: push\njobs:\n  call:\n    uses: owner/large/.github/workflows/ci.yml@v1\n")
		remoteRoot := t.TempDir()
		oversized := []byte("on: workflow_call\njobs: {}\n#" + strings.Repeat("x", MaxReusableWorkflowBytes))
		writeWorkflow(t, remoteRoot, "ci.yml", string(oversized))
		fake := newFakeReusableRepositorySource(t, map[string]string{"owner/large": remoteRoot})
		options := defaultOptions()
		options.RepositorySource = MemoizeRepositorySource(fake)
		_, err := CompileWithOptions(callerPath, readFile(t, callerPath), event, options)
		if err == nil || !strings.Contains(err.Error(), "workflow exceeds 1048576-byte limit") {
			t.Fatalf("CompileWithOptions() error = %v, want bounded workflow load", err)
		}
	})

	for _, test := range []struct {
		name string
		uses string
		want string
	}{
		{name: "dynamic", uses: "${{ inputs.workflow }}", want: "runtime-dependent"},
		{name: "nested path", uses: "owner/repo/.github/workflows/nested/ci.yml@v1", want: "directly under .github/workflows"},
		{name: "wrong directory", uses: "owner/repo/workflows/ci.yml@v1", want: "directly under .github/workflows"},
		{name: "non YAML", uses: "owner/repo/.github/workflows/ci.json@v1", want: "must end in .yml or .yaml"},
		{name: "traversal", uses: "owner/repo/.github/workflows/../ci.yml@v1", want: "invalid action reference segment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			callerPath := writeWorkflow(t, callerRoot, strings.ReplaceAll(test.name, " ", "-")+".yml", "on: push\njobs:\n  call:\n    uses: "+test.uses+"\n")
			fake := newFakeReusableRepositorySource(t, map[string]string{})
			options := defaultOptions()
			options.RepositorySource = MemoizeRepositorySource(fake)
			_, err := CompileWithOptions(callerPath, readFile(t, callerPath), event, options)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(fake.references()) != 0 {
				t.Fatalf("CompileWithOptions() error/calls = %v / %#v, want %q before source access", err, fake.references(), test.want)
			}
		})
	}

	t.Run("missing or private", func(t *testing.T) {
		callerPath := writeWorkflow(t, callerRoot, "private.yml", "on: push\njobs:\n  call:\n    uses: owner/private/.github/workflows/ci.yml@v1\n")
		fake := newFakeReusableRepositorySource(t, map[string]string{})
		fake.err = &actionsource.NotPublicError{}
		options := defaultOptions()
		options.RepositorySource = MemoizeRepositorySource(fake)
		_, err := CompileWithOptions(callerPath, readFile(t, callerPath), event, options)
		if err == nil || !strings.Contains(err.Error(), `public reusable workflow "owner/private/.github/workflows/ci.yml@v1" was not found or is not public`) {
			t.Fatalf("CompileWithOptions() error = %v, want non-enumerating source error", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		callerPath := writeWorkflow(t, callerRoot, "cancel.yml", "on: push\njobs:\n  call:\n    uses: owner/repo/.github/workflows/ci.yml@v1\n")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		options := defaultOptions()
		options.RepositorySource = contextActionSource{}
		_, err := CompileWithOptionsContext(ctx, callerPath, readFile(t, callerPath), event, options)
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("CompileWithOptionsContext() error = %v, want cancellation", err)
		}
	})
}

func unmarshalJSON(source []byte, value any) error {
	return json.Unmarshal(source, value)
}
