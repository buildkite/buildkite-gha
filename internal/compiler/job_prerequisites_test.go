package compiler

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// TestJobPrerequisitesMergeEveryProvenance derives the expected list by hand
// from four differently sourced bindings that overlap and arrive unsorted.
func TestJobPrerequisitesMergeEveryProvenance(t *testing.T) {
	sourced := sourcedJob{
		Job:          workflow.Job{ID: "leaf", Needs: []string{"b", "a"}},
		needBindings: map[string]needBinding{"upstream": {members: []string{"b", "a"}}},
		inputs: reusableInputs{deferred: map[string]deferredInput{
			"subject": {needs: map[string]needBinding{"producer": {members: []string{"c", "a"}}}},
		}},
		callGuards: []sourcedCallGuard{
			{needBindings: map[string]needBinding{"gate": {members: []string{"d"}}}},
			{inputs: reusableInputs{deferred: map[string]deferredInput{
				"forwarded": {needs: map[string]needBinding{"outer": {members: []string{"e", "b"}}}},
			}}},
		},
	}
	if got, want := jobPrerequisites(sourced), []string{"a", "b", "c", "d", "e"}; !slices.Equal(got, want) {
		t.Fatalf("prerequisites = %q, want %q", got, want)
	}
	if got := jobPrerequisites(sourcedJob{Job: workflow.Job{ID: "root"}}); len(got) != 0 {
		t.Fatalf("root prerequisites = %q, want none", got)
	}
}

// resolveSourcedJobsForTest flattens a workflow the way compilation does before
// the job graph is ordered.
func resolveSourcedJobsForTest(t *testing.T, path string) ([]sourcedJob, *workflow.Workflow, expression.CompileContext) {
	t.Helper()
	source := readFile(t, path)
	parsed, err := parseReusableWorkflow(path, source)
	if err != nil {
		t.Fatal(err)
	}
	event, err := ParseEvent(readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	context := compileContext(event, nil, path, parsed.Name)
	context.Inputs = workflowDispatchInputs(parsed, event)
	resolved, _, _, err := resolveReusableWorkflows(t.Context(), path, source, parsed, context, defaultOptions().RepositorySource, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved, parsed, context
}

func sourcedJobByID(t *testing.T, resolved []sourcedJob, id string) sourcedJob {
	t.Helper()
	for _, sourced := range resolved {
		if sourced.ID == id {
			return sourced
		}
	}
	t.Fatalf("flattened job %q missing from %q", id, flattenedJobIDs(resolved))
	return sourcedJob{}
}

func flattenedJobIDs(resolved []sourcedJob) []string {
	ids := make([]string, len(resolved))
	for i, sourced := range resolved {
		ids[i] = sourced.ID
	}
	return ids
}

// TestJobPrerequisitesCarryGuardAndDeferredInputProvenanceWithoutWideningNeeds
// builds a nested call whose leaf job reaches the caller's jobs only through
// a call guard and a forwarded deferred input. Those jobs must be
// prerequisites of the leaf, but its needs scope must stay empty because
// the leaf declares no needs of its own.
func TestJobPrerequisitesCarryGuardAndDeferredInputProvenanceWithoutWideningNeeds(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      ready: ${{ steps.emit.outputs.ready }}
    steps:
      - id: emit
        run: echo ready=true >> "$GITHUB_OUTPUT"
  hash:
    runs-on: ubuntu-latest
    outputs:
      hashes: ${{ steps.hash.outputs.hashes }}
    steps:
      - id: hash
        run: echo hashes=value >> "$GITHUB_OUTPUT"
  delegated:
    needs: [prepare, hash]
    if: needs.prepare.outputs.ready
    uses: ./.github/workflows/middle.yml
    with:
      subjects: ${{ needs.hash.outputs.hashes }}
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    inputs:
      subjects: {type: string, required: true}
jobs:
  setup:
    runs-on: ubuntu-latest
    steps:
      - run: echo setup
  inner:
    needs: setup
    uses: ./.github/workflows/leaf.yml
    with:
      subjects: ${{ inputs.subjects }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      subjects: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.subjects }}
`)

	resolved, _, _ := resolveSourcedJobsForTest(t, callerPath)
	want := map[string]struct{ prerequisites, needs []string }{
		"prepare":              {prerequisites: []string{}, needs: nil},
		"hash":                 {prerequisites: []string{}, needs: nil},
		"delegated.setup":      {prerequisites: []string{"hash", "prepare"}, needs: nil},
		"delegated.inner.test": {prerequisites: []string{"delegated.setup", "hash", "prepare"}, needs: nil},
	}
	if got := flattenedJobIDs(resolved); len(got) != len(want) {
		t.Fatalf("flattened jobs = %q", got)
	}
	for id, expected := range want {
		sourced := sourcedJobByID(t, resolved, id)
		if got := jobPrerequisites(sourced); !slices.Equal(got, expected.prerequisites) {
			t.Errorf("%s prerequisites = %q, want %q", id, got, expected.prerequisites)
		}
		if got := sourced.Needs; !slices.Equal(got, expected.needs) {
			t.Errorf("%s needs = %q, want %q", id, got, expected.needs)
		}
		if got := bindingMembers(sourced.needBindings); !slices.Equal(got, expected.needs) {
			t.Errorf("%s need bindings = %q, want the same members as needs %q", id, got, expected.needs)
		}
	}
	// Caller guards and a forwarded input reach prepare and hash;
	// neither is a need binding the leaf can read.
	leaf := sourcedJobByID(t, resolved, "delegated.inner.test")
	for _, caller := range []string{"prepare", "hash"} {
		for need, binding := range leaf.needBindings {
			if slices.Contains(binding.members, caller) {
				t.Fatalf("leaf need %q binds caller job %q", need, caller)
			}
		}
	}
	if len(leaf.callGuards) != 2 || !slices.Equal(bindingMembers(leaf.callGuards[0].needBindings), []string{"hash", "prepare"}) || !slices.Equal(bindingMembers(leaf.callGuards[1].needBindings), []string{"delegated.setup"}) {
		t.Fatalf("leaf call guards = %#v", leaf.callGuards)
	}
	if deferred := leaf.inputs.deferred["subjects"]; !slices.Equal(bindingMembers(deferred.needs), []string{"hash"}) {
		t.Fatalf("leaf deferred input = %#v", deferred)
	}
}

// TestJobPrerequisitesDeduplicateGuardAndDeferredInput lists a caller job
// reached through both a call guard and a deferred input exactly once.
func TestJobPrerequisitesDeduplicateGuardAndDeferredInput(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  hash:
    runs-on: ubuntu-latest
    outputs:
      hashes: ${{ steps.hash.outputs.hashes }}
    steps:
      - id: hash
        run: echo hashes=value >> "$GITHUB_OUTPUT"
  consume:
    needs: hash
    if: needs.hash.outputs.hashes != ''
    uses: ./.github/workflows/leaf.yml
    with:
      subjects: ${{ needs.hash.outputs.hashes }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      subjects: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.subjects }}
`)

	resolved, _, _ := resolveSourcedJobsForTest(t, callerPath)
	leaf := sourcedJobByID(t, resolved, "consume.test")
	if len(leaf.callGuards) != 1 || len(leaf.callGuards[0].needBindings) != 1 || len(leaf.inputs.deferred) != 1 || len(leaf.needBindings) != 0 {
		t.Fatalf("leaf provenance = %#v", leaf)
	}
	if got := jobPrerequisites(leaf); !slices.Equal(got, []string{"hash"}) {
		t.Fatalf("leaf prerequisites = %q, want [hash] once", got)
	}
}

// expandJobGraphForTest runs the graph expansion the way compilation does and
// returns its result and joined diagnostics.
func expandJobGraphForTest(t *testing.T, path string) (jobGraphExpansionResult, error) {
	t.Helper()
	source := readFile(t, path)
	parsed, err := parseReusableWorkflow(path, source)
	if err != nil {
		t.Fatal(err)
	}
	event, err := ParseEvent(readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	options := defaultOptions()
	event.Trust = options.EventTrust
	context := compileContext(event, nil, path, parsed.Name)
	context.Inputs = workflowDispatchInputs(parsed, event)
	return expandJobGraph(t.Context(), path, source, parsed, context, options)
}

func candidateLogicalJobs(result jobGraphExpansionResult) []string {
	ids := make([]string, 0, len(result.candidates))
	for _, candidate := range result.candidates {
		if len(ids) == 0 || ids[len(ids)-1] != candidate.LogicalJobID {
			ids = append(ids, candidate.LogicalJobID)
		}
	}
	return ids
}

// TestExpandJobGraphOrdersJobsByPrerequisites expects the flattened callee
// jobs after every caller job they reach through needs, the guard, or the
// forwarded input. Among the jobs whose prerequisites are all placed, the
// smallest ID goes first, so the callee jobs precede the unrelated root zeta
// once alpha is placed.
func TestExpandJobGraphOrdersJobsByPrerequisites(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  zeta:
    runs-on: ubuntu-latest
    steps:
      - run: echo zeta
  alpha:
    runs-on: ubuntu-latest
    outputs:
      value: ${{ steps.emit.outputs.value }}
    steps:
      - id: emit
        run: echo value=one >> "$GITHUB_OUTPUT"
  call:
    needs: alpha
    if: needs.alpha.outputs.value != ''
    uses: ./.github/workflows/leaf.yml
    with:
      subject: ${{ needs.alpha.outputs.value }}
  omega:
    needs: [zeta, call]
    runs-on: ubuntu-latest
    steps:
      - run: echo omega
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      subject: {type: string, required: true}
jobs:
  second:
    needs: first
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.subject }}
  first:
    runs-on: ubuntu-latest
    steps:
      - run: echo first
`)

	result, err := expandJobGraphForTest(t, callerPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "call.first", "call.second", "zeta", "omega"}
	if got := candidateLogicalJobs(result); !slices.Equal(got, want) {
		t.Fatalf("expansion order = %q, want %q", got, want)
	}
	if len(result.notEvaluatedJobs) != 0 {
		t.Fatalf("not evaluated jobs = %v", result.notEvaluatedJobs)
	}
}

// TestExpandJobGraphBlocksEveryDependentOfAFailedPrerequisite fails the job a
// guarded call reads and expects the callee jobs, the job that needs them, and
// nothing else to be recorded as not evaluated.
func TestExpandJobGraphBlocksEveryDependentOfAFailedPrerequisite(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  broken:
    runs-on: ubuntu-latest
    outputs:
      value: ${{ steps.emit.outputs.value }}
    steps:
      - id: emit
        run: echo value=one >> "$GITHUB_OUTPUT"
      - id: emit
        run: echo twice
  unrelated:
    runs-on: ubuntu-latest
    steps:
      - run: echo unrelated
  call:
    needs: broken
    if: needs.broken.outputs.value != ''
    uses: ./.github/workflows/leaf.yml
  after:
    needs: call
    runs-on: ubuntu-latest
    steps:
      - run: echo after
`)
	writeWorkflow(t, repository, "leaf.yml", `on: workflow_call
jobs:
  first:
    runs-on: ubuntu-latest
    steps:
      - run: echo first
  second:
    needs: first
    runs-on: ubuntu-latest
    steps:
      - run: echo second
`)

	result, err := expandJobGraphForTest(t, callerPath)
	if err == nil || !strings.Contains(err.Error(), `duplicate step id "emit"`) {
		t.Fatalf("expansion error = %v, want the duplicate step diagnostic only", err)
	}
	if got, want := result.notEvaluatedJobs, map[string]bool{"call.first": true, "call.second": true, "after": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("not evaluated jobs = %v, want %v", got, want)
	}
	// Blocked jobs keep their candidates for the report but produce no
	// instances. The failed job itself is not blocked: its instance is kept
	// so its diagnostic can be attributed, and the unrelated job is unaffected.
	if got, want := candidateLogicalJobs(result), []string{"broken", "call.first", "call.second", "after", "unrelated"}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
	instances := make([]string, 0, len(result.instances))
	for _, instance := range result.instances {
		instances = append(instances, instance.LogicalJobID)
	}
	if want := []string{"broken", "unrelated"}; !slices.Equal(instances, want) {
		t.Fatalf("evaluated instances = %q, want %q", instances, want)
	}
}

// TestExpandJobGraphReportsCycleAndFallsBackToSortedOrder keeps the cycle
// diagnostic attributed to the alphabetically first cyclic job and still
// records every job so the report can list them.
func TestExpandJobGraphReportsCycleAndFallsBackToSortedOrder(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  loop-b:
    needs: loop-a
    runs-on: ubuntu-latest
    steps:
      - run: echo b
  loop-a:
    needs: loop-b
    runs-on: ubuntu-latest
    steps:
      - run: echo a
  free:
    runs-on: ubuntu-latest
    steps:
      - run: echo free
`)

	result, err := expandJobGraphForTest(t, callerPath)
	if err == nil || !strings.Contains(err.Error(), `job "loop-a"`) || !strings.Contains(err.Error(), "workflow job graph contains a cycle") {
		t.Fatalf("expansion error = %v", err)
	}
	if got, want := candidateLogicalJobs(result), []string{"free", "loop-a", "loop-b"}; !slices.Equal(got, want) {
		t.Fatalf("fallback order = %q, want %q", got, want)
	}
}
