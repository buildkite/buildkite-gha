package compiler

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

const runtimeMatrixWorkflow = `on: push
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: true
  plan:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.plan.outputs.matrix }}
    steps:
      - id: plan
        run: echo 'matrix=[]' >> "$GITHUB_OUTPUT"
  build:
    needs: plan
    runs-on: ${{ matrix.runner }}
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: echo ${{ matrix.target }}
  publish:
    needs: [build, lint]
    runs-on: ubuntu-latest
    steps:
      - run: true
`

func compileRuntimeMatrix(t *testing.T, source string, rows map[string][]map[string]any) (IR, error) {
	t.Helper()
	options := defaultOptions()
	options.RuntimeMatrixRows = rows
	return CompileIRWithOptionsContext(t.Context(), "runtime.yml", []byte(source), readFile(t, smokePath("events", "push.json")), options)
}

func jobKeys(ir IR) map[string]JobInstance {
	jobs := make(map[string]JobInstance, len(ir.Jobs))
	for _, job := range ir.Jobs {
		jobs[job.Key] = job
	}
	return jobs
}

func TestRuntimeMatrixDefersConsumerAndDependents(t *testing.T) {
	ir, err := compileRuntimeMatrix(t, runtimeMatrixWorkflow, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := jobKeys(ir)
	if len(jobs) != 2 || jobs["gha-lint"].Key == "" || jobs["gha-plan"].Key == "" {
		t.Fatalf("initial jobs = %v, want only lint and plan", jobs)
	}
	if len(ir.Continuations) != 1 {
		t.Fatalf("continuations = %#v, want one", ir.Continuations)
	}
	continuation := ir.Continuations[0]
	if continuation.StepKey != "gha-build-matrix" || continuation.Descriptor.Job != "build" || continuation.Descriptor.ProducerJob != "plan" || continuation.Descriptor.ProducerOutput != "matrix" {
		t.Fatalf("continuation = %#v", continuation)
	}
	if strings.Join(continuation.Jobs, ",") != "build,publish" {
		t.Fatalf("deferred jobs = %v, want consumer first then its dependents", continuation.Jobs)
	}
	if continuation.JobLabel("build") != "build" || continuation.JobLabel("publish") != "publish" || continuation.JobLabel("missing") != "missing" {
		t.Fatalf("labels = %#v", continuation.Labels)
	}
	report, err := ValidateEventWithOptions("runtime.yml", []byte(runtimeMatrixWorkflow), readFile(t, smokePath("events", "push.json")), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !report.NotEvaluatedJobs["build"] || !report.NotEvaluatedJobs["publish"] || report.NotEvaluatedJobs["lint"] || len(report.Continuations) != 1 {
		t.Fatalf("not evaluated jobs = %v continuations = %#v", report.NotEvaluatedJobs, report.Continuations)
	}
	var deferred int
	for _, warning := range ir.Warnings {
		if warning.Code == "W_MATRIX_DEFERRED" && warning.Job == "build" {
			deferred++
		}
	}
	if deferred != 1 {
		t.Fatalf("warnings = %#v, want one W_MATRIX_DEFERRED for build", ir.Warnings)
	}
	if LogicalJobStepKey("", "publish") != "gha-publish" || LogicalJobStepKey("release", "publish") != "gha-release-publish" {
		t.Fatalf("logical step key = %q", LogicalJobStepKey("", "publish"))
	}
}

// The continuation must name the producer's generated instance key, which
// carries the workflow namespace and, for a one-row static matrix, the matrix
// digest, so the deferred step depends on and reads from the step that exists.
func TestRuntimeMatrixContinuationNamesProducerInstance(t *testing.T) {
	source := strings.Replace(runtimeMatrixWorkflow, "  plan:\n    runs-on: ubuntu-latest\n", "  plan:\n    runs-on: ${{ matrix.os }}\n    strategy:\n      matrix:\n        os: [ubuntu-latest]\n", 1)
	if source == runtimeMatrixWorkflow {
		t.Fatal("workflow fixture did not change")
	}
	for _, namespace := range []string{"", "0123456789abcdef"} {
		options := defaultOptions()
		options.StepKeyNamespace = namespace
		ir, err := CompileIRWithOptionsContext(t.Context(), "runtime.yml", []byte(source), readFile(t, smokePath("events", "push.json")), options)
		if err != nil {
			t.Fatal(err)
		}
		if len(ir.Continuations) != 1 {
			t.Fatalf("continuations = %#v, want one", ir.Continuations)
		}
		producer := ir.Continuations[0].ProducerStepKey
		logical := LogicalJobStepKey(namespace, "plan")
		if _, exists := jobKeys(ir)[producer]; !exists || producer == logical || !strings.HasPrefix(producer, logical+"-") {
			t.Fatalf("namespace %q: producer step key %q is not the compiled plan instance among %v", namespace, producer, jobKeys(ir))
		}
	}
}

// Rows supplied at continuation time must produce exactly the instances,
// keys, labels, and dependencies that the same rows written literally in the
// workflow produce, so downstream steps and check names never depend on when
// the matrix was expanded.
func TestRuntimeMatrixRowsMatchStaticInclude(t *testing.T) {
	rows := []map[string]any{
		{"target": "amd64", "runner": "ubuntu-latest"},
		{"target": "arm64", "runner": "ubuntu-latest"},
	}
	dynamic, err := compileRuntimeMatrix(t, runtimeMatrixWorkflow, map[string][]map[string]any{"build": rows})
	if err != nil {
		t.Fatal(err)
	}
	if len(dynamic.Continuations) != 0 {
		t.Fatalf("rows should expand the deferred subgraph, got continuations %#v", dynamic.Continuations)
	}
	static := strings.Replace(runtimeMatrixWorkflow, "include: ${{ fromJSON(needs.plan.outputs.matrix) }}", "include:\n          - {target: amd64, runner: ubuntu-latest}\n          - {target: arm64, runner: ubuntu-latest}", 1)
	want, err := compileRuntimeMatrix(t, static, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantJobs, gotJobs := jobKeys(want), jobKeys(dynamic)
	if len(gotJobs) != 5 || len(wantJobs) != len(gotJobs) {
		t.Fatalf("got %d jobs, static %d jobs, want 5", len(gotJobs), len(wantJobs))
	}
	for key, wantJob := range wantJobs {
		got, ok := gotJobs[key]
		if !ok {
			t.Fatalf("dynamic expansion lacks static key %q; keys %v", key, dynamic.Jobs)
		}
		if got.Label != wantJob.Label || got.Queue != wantJob.Queue || strings.Join(got.Needs, ",") != strings.Join(wantJob.Needs, ",") || len(got.Matrix) != len(wantJob.Matrix) {
			t.Fatalf("job %q differs: dynamic %+v static %+v", key, got, wantJob)
		}
	}
	publish := gotJobs["gha-publish"]
	instances := 0
	for _, need := range publish.Needs {
		if strings.HasPrefix(need, "gha-build-") {
			instances++
		}
	}
	if instances != 2 || len(publish.Needs) != 3 {
		t.Fatalf("publish needs = %v, want both build instances and lint", publish.Needs)
	}
}

// TestRuntimeMatrixRecordsStaticInstancesOfDependents proves the continuation
// records every statically known instance of each deferred dependent with the
// key, label, and check label the expanded graph gives it, so a producer
// failure can skip each promised check. A matrix-free dependent and an empty
// include row record the logical key; the consumer records nothing.
func TestRuntimeMatrixRecordsStaticInstancesOfDependents(t *testing.T) {
	for name, test := range map[string]struct {
		matrix string
		want   int
	}{
		"matrix-free": {matrix: "", want: 1},
		"two rows":    {matrix: "    strategy:\n      matrix:\n        os: [linux, macos]\n", want: 2},
		// A literal empty include row is rejected, but a compile-time
		// fromJSON include can still expand to one.
		"empty row included": {matrix: "    strategy:\n      matrix:\n        include: " + `"${{ fromJSON('[{}, {\"os\": \"linux\"}]') }}"` + "\n", want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			source := strings.Replace(runtimeMatrixWorkflow, "    needs: [build, lint]\n", "    needs: [build, lint]\n"+test.matrix, 1)
			deferred, err := compileRuntimeMatrix(t, source, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(deferred.Continuations) != 1 {
				t.Fatalf("continuations = %#v", deferred.Continuations)
			}
			continuation := deferred.Continuations[0]
			instances := continuation.Instances["publish"]
			if len(continuation.Instances) != 1 || len(instances) != test.want {
				t.Fatalf("instances = %#v, want %d for publish only", continuation.Instances, test.want)
			}
			expanded, err := compileRuntimeMatrix(t, source, map[string][]map[string]any{"build": {{"target": "x", "runner": "ubuntu-latest"}}})
			if err != nil {
				t.Fatal(err)
			}
			keys := make(map[string]bool, len(instances))
			for _, instance := range instances {
				job, ok := jobKeys(expanded)[instance.Key]
				if !ok || job.LogicalJobID != "publish" || job.Label != instance.Label || instanceCheckLabel(job) != instance.CheckLabel {
					t.Fatalf("recorded instance %#v does not match expanded job %#v", instance, job)
				}
				keys[instance.Key] = true
			}
			if len(keys) != test.want {
				t.Fatalf("instances share keys: %#v", instances)
			}
			if test.matrix == "" || strings.Contains(test.matrix, "{}") {
				if !keys["gha-publish"] {
					t.Fatalf("instances %#v do not include the logical key", instances)
				}
			}
		})
	}
}

func TestRuntimeMatrixRejectsEmptyRows(t *testing.T) {
	_, err := compileRuntimeMatrix(t, runtimeMatrixWorkflow, map[string][]map[string]any{"build": {}})
	if err == nil || !strings.Contains(err.Error(), `output "matrix" of job "plan" expanded to no matrix instances`) {
		t.Fatalf("err = %v", err)
	}
}

// productionGateJobID is a job id whose step key equals the approval gate key
// of the "production" environment.
var productionGateJobID = strings.TrimPrefix(environmentGateKey("", "production"), "gha-")

func TestRuntimeMatrixClosureOwnership(t *testing.T) {
	source := runtimeMatrixWorkflow
	for _, job := range []string{"test", "package", "independent"} {
		source += fmt.Sprintf(`  %s:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: true
`, job)
	}
	for _, test := range []struct {
		name       string
		joins      string
		components int
		owned      []string
	}{
		{name: "shared producer does not merge", components: 4, owned: []string{"build", "publish"}},
		{name: "join merges full branches", components: 3, joins: "  join:\n    needs: [build, test, lint]\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n", owned: []string{"build", "join", "publish", "test"}},
		{name: "overlap merges transitively", components: 2, joins: "  join:\n    needs: [build, test, lint]\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n  second:\n    needs: [test, package]\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n  tail:\n    needs: second\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n", owned: []string{"build", "join", "package", "publish", "second", "tail", "test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ir, err := compileRuntimeMatrix(t, source+test.joins, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(ir.Jobs) != 2 || len(ir.Continuations) != test.components {
				t.Fatalf("initial jobs = %v, continuations = %#v", jobKeys(ir), ir.Continuations)
			}
			var owner RuntimeMatrixContinuation
			seen := make(map[string]bool)
			for _, component := range ir.Continuations {
				for _, job := range component.Jobs {
					if seen[job] || job == "plan" || job == "lint" {
						t.Fatalf("invalid ownership of %s", job)
					}
					seen[job] = true
				}
				if slices.Contains(component.Jobs, "build") {
					owner = component
				}
			}
			owned := slices.Clone(owner.Jobs)
			slices.Sort(owned)
			if !reflect.DeepEqual(owned, test.owned) {
				t.Fatalf("owned = %v, want %v", owned, test.owned)
			}
			if owner.JobBudget != (MaxRuntimeMatrixGraphJobs-2)/test.components {
				t.Fatalf("budget = %d", owner.JobBudget)
			}
			rows := make(map[string][]map[string]any)
			for _, root := range owner.Roots() {
				rows[root.Descriptor.Job] = []map[string]any{{"target": root.Descriptor.Job, "runner": "ubuntu-latest"}}
			}
			expanded, err := compileRuntimeMatrix(t, source+test.joins, rows)
			if err != nil {
				t.Fatal(err)
			}
			if len(expanded.Jobs) != 2+len(test.owned) {
				t.Fatalf("expanded jobs = %v", jobKeys(expanded))
			}
			for _, remaining := range expanded.Continuations {
				index := slices.IndexFunc(ir.Continuations, func(c RuntimeMatrixContinuation) bool { return c.StepKey == remaining.StepKey })
				if index < 0 || !reflect.DeepEqual(remaining, ir.Continuations[index]) {
					t.Fatalf("remaining owner drifted: %#v", remaining)
				}
			}
			if len(owner.Joined) != 0 {
				delete(rows, owner.Joined[0].Descriptor.Job)
				if _, err := compileRuntimeMatrix(t, source+test.joins, rows); err == nil || !strings.Contains(err.Error(), "same continuation") {
					t.Fatalf("partial component error = %v", err)
				}
			}
		})
	}
}

// runtimeMatrixStagesWorkflow chains three needs-derived matrices: build's
// rows come from the static job plan, deploy's from package, which needs
// build, and release's from verify, which needs deploy. Each matrix can be
// expanded only after the jobs of the earlier stage have run.
const runtimeMatrixStagesWorkflow = runtimeMatrixWorkflow + `  package:
    needs: build
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.plan.outputs.matrix }}
    steps:
      - id: plan
        run: echo 'matrix=[]' >> "$GITHUB_OUTPUT"
  deploy:
    needs: package
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.package.outputs.matrix) }}
    steps:
      - run: echo ${{ matrix.region }}
  verify:
    needs: deploy
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.plan.outputs.matrix }}
    steps:
      - id: plan
        run: echo 'matrix=[]' >> "$GITHUB_OUTPUT"
  release:
    needs: [verify, lint]
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.verify.outputs.matrix) }}
    steps:
      - run: echo ${{ matrix.channel }}
`

func logicalJobs(ir IR) []string {
	jobs := make([]string, 0, len(ir.Jobs))
	for _, job := range ir.Jobs {
		jobs = append(jobs, job.LogicalJobID)
	}
	return jobs
}

func continuationFor(t *testing.T, ir IR, consumer string) RuntimeMatrixContinuation {
	t.Helper()
	for _, continuation := range ir.Continuations {
		if continuation.Descriptor.Job == consumer {
			return continuation
		}
	}
	t.Fatalf("no continuation expands %q: %#v", consumer, ir.Continuations)
	return RuntimeMatrixContinuation{}
}

func instanceStepKey(t *testing.T, job string, matrix map[string]any) string {
	t.Helper()
	key, err := instanceKey(job, matrix)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestRuntimeMatrixChainsStagesThroughLaterMatrices(t *testing.T) {
	buildRows := []map[string]any{{"target": "x", "runner": "ubuntu-latest"}, {"target": "y", "runner": "ubuntu-latest"}}
	deployRows := []map[string]any{{"region": "eu"}, {"region": "us"}, {"region": "ap"}}
	releaseRows := []map[string]any{{"channel": "stable"}}

	// The initial compilation defers the whole component to build's step;
	// deploy and release are later matrices with no instances of their own.
	initial, err := compileRuntimeMatrix(t, runtimeMatrixStagesWorkflow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := logicalJobs(initial); !slices.Equal(got, []string{"lint", "plan"}) || len(initial.Continuations) != 1 {
		t.Fatalf("initial jobs = %v, continuations = %#v", got, initial.Continuations)
	}
	first := initial.Continuations[0]
	if first.Descriptor.Job != "build" || len(first.Joined) != 0 || !slices.Equal(first.Jobs, []string{"build", "package", "deploy", "publish", "verify", "release"}) || !slices.Equal(first.LaterMatrices, []string{"deploy", "release"}) {
		t.Fatalf("first stage = %#v", first)
	}
	if got := slices.Sorted(maps.Keys(first.Instances)); !slices.Equal(got, []string{"package", "publish", "verify"}) || first.DependentInstances() != 5 || first.JobBudget != MaxRuntimeMatrixGraphJobs-2 {
		t.Fatalf("first stage instances = %v, budget = %d", got, first.JobBudget)
	}

	// Rows for build compile everything up to deploy's producer and leave a
	// continuation rooted at deploy, with release still a later matrix.
	second, err := compileRuntimeMatrix(t, runtimeMatrixStagesWorkflow, map[string][]map[string]any{"build": buildRows})
	if err != nil {
		t.Fatal(err)
	}
	if got := logicalJobs(second); !slices.Equal(got, []string{"lint", "plan", "build", "build", "package", "publish"}) || len(second.Continuations) != 1 {
		t.Fatalf("second stage jobs = %v, continuations = %#v", got, second.Continuations)
	}
	next := second.Continuations[0]
	if next.Descriptor.Job != "deploy" || next.StepKey != "gha-deploy-matrix" || next.ProducerStepKey != "gha-package" || !slices.Equal(next.Jobs, []string{"deploy", "verify", "release"}) || !slices.Equal(next.LaterMatrices, []string{"release"}) {
		t.Fatalf("second stage continuation = %#v", next)
	}
	if !reflect.DeepEqual(next.Instances, map[string][]RuntimeMatrixInstance{"verify": first.Instances["verify"]}) || next.Sources["release"] != first.Sources["release"] {
		t.Fatalf("second stage continuation = %#v", next)
	}

	third, err := compileRuntimeMatrix(t, runtimeMatrixStagesWorkflow, map[string][]map[string]any{"build": buildRows, "deploy": deployRows})
	if err != nil {
		t.Fatal(err)
	}
	if got := logicalJobs(third); len(got) != 10 || len(third.Continuations) != 1 || third.Continuations[0].Descriptor.Job != "release" || !slices.Equal(third.Continuations[0].Jobs, []string{"release"}) {
		t.Fatalf("third stage jobs = %v, continuations = %#v", got, third.Continuations)
	}

	final, err := compileRuntimeMatrix(t, runtimeMatrixStagesWorkflow, map[string][]map[string]any{"build": buildRows, "deploy": deployRows, "release": releaseRows})
	if err != nil {
		t.Fatal(err)
	}
	finalKeys := jobKeys(final)
	if len(final.Continuations) != 0 || len(finalKeys) != 11 {
		t.Fatalf("final stage jobs = %v, continuations = %#v", logicalJobs(final), final.Continuations)
	}
	if release := finalKeys[instanceStepKey(t, "release", releaseRows[0])]; !slices.Equal(release.Needs, []string{"gha-lint", "gha-verify"}) {
		t.Fatalf("release needs = %v", release.Needs)
	}

	t.Run("rows for a later stage need the earlier rows", func(t *testing.T) {
		_, err := compileRuntimeMatrix(t, runtimeMatrixStagesWorkflow, map[string][]map[string]any{"deploy": deployRows})
		if err == nil || !strings.Contains(err.Error(), "same continuation") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("skipped stage skips every later stage", func(t *testing.T) {
		options := defaultOptions()
		options.RuntimeMatrixRows = map[string][]map[string]any{"build": buildRows, "deploy": nil}
		options.RuntimeMatrixSkipped = map[string]bool{"deploy": true}
		ir, err := CompileIRWithOptionsContext(t.Context(), "runtime.yml", []byte(runtimeMatrixStagesWorkflow), readFile(t, smokePath("events", "push.json")), options)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]bool{"deploy": true, "verify": true, "release": true}; len(ir.Continuations) != 0 || !reflect.DeepEqual(ir.RuntimeMatrixSkippedJobs, want) {
			t.Fatalf("skipped = %v, continuations = %#v", ir.RuntimeMatrixSkippedJobs, ir.Continuations)
		}
	})

	t.Run("root with a static producer joins the stage of a deferred prerequisite", func(t *testing.T) {
		// macos reads its rows from plan, which the initial upload runs, but
		// needs build, so it is expanded in build's stage with rows for both
		// roots rather than in a stage of its own.
		source := strings.Replace(runtimeMatrixWorkflow, "      matrix: ${{ steps.plan.outputs.matrix }}\n", "      matrix: ${{ steps.plan.outputs.matrix }}\n      macos: ${{ steps.plan.outputs.macos }}\n", 1) + `  macos:
    needs: [plan, build]
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.macos) }}
    steps:
      - run: true
`
		initial, err := compileRuntimeMatrix(t, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		first := continuationFor(t, initial, "build")
		if len(initial.Continuations) != 1 || len(first.Joined) != 1 || first.Joined[0].Descriptor.Job != "macos" || len(first.LaterMatrices) != 0 {
			t.Fatalf("continuations = %#v", initial.Continuations)
		}
		if _, err := compileRuntimeMatrix(t, source, map[string][]map[string]any{"build": buildRows}); err == nil || !strings.Contains(err.Error(), "same continuation") {
			t.Fatalf("rows for build only: err = %v", err)
		}
		// build's producer failing skips macos although its own producer
		// published rows.
		options := defaultOptions()
		options.RuntimeMatrixRows = map[string][]map[string]any{"build": nil, "macos": {{"arch": "arm64"}}}
		options.RuntimeMatrixSkipped = map[string]bool{"build": true}
		skipped, err := CompileIRWithOptionsContext(t.Context(), "runtime.yml", []byte(source), readFile(t, smokePath("events", "push.json")), options)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(skipped.RuntimeMatrixSkippedJobs, map[string]bool{"build": true, "macos": true, "publish": true}) {
			t.Fatalf("skipped = %v", skipped.RuntimeMatrixSkippedJobs)
		}
	})

	t.Run("later stage joins another root", func(t *testing.T) {
		// test's rows come from plan like build's, and deploy needs test as
		// well as package, so build, test, and everything after deploy form
		// one component with deploy as a later stage.
		source := strings.Replace(runtimeMatrixStagesWorkflow, "  deploy:\n    needs: package\n", "  deploy:\n    needs: [package, test]\n", 1) + `  test:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: true
`
		initial, err := compileRuntimeMatrix(t, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		first := continuationFor(t, initial, "build")
		if len(initial.Continuations) != 1 || len(first.Joined) != 1 || first.Joined[0].Descriptor.Job != "test" || !slices.Equal(first.LaterMatrices, []string{"deploy", "release"}) {
			t.Fatalf("continuations = %#v", initial.Continuations)
		}
		second, err := compileRuntimeMatrix(t, source, map[string][]map[string]any{"build": buildRows, "test": {{"target": "t"}}})
		if err != nil {
			t.Fatal(err)
		}
		if next := continuationFor(t, second, "deploy"); len(second.Continuations) != 1 || len(next.Joined) != 0 || !slices.Equal(next.Jobs, []string{"deploy", "verify", "release"}) {
			t.Fatalf("second stage = %#v", second.Continuations)
		}
	})

	t.Run("other components keep their share across stages", func(t *testing.T) {
		source := runtimeMatrixStagesWorkflow + `  other:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: true
`
		initial, err := compileRuntimeMatrix(t, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := (MaxRuntimeMatrixGraphJobs - 2) / 2
		if len(initial.Continuations) != 2 || continuationFor(t, initial, "build").JobBudget != want || continuationFor(t, initial, "other").JobBudget != want {
			t.Fatalf("continuations = %#v", initial.Continuations)
		}
		second, err := compileRuntimeMatrix(t, source, map[string][]map[string]any{"build": buildRows})
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Continuations) != 2 || !reflect.DeepEqual(continuationFor(t, second, "other"), continuationFor(t, initial, "other")) {
			t.Fatalf("other component drifted: %#v", second.Continuations)
		}
	})

	t.Run("later stage upload step key is reserved", func(t *testing.T) {
		source := strings.Replace(runtimeMatrixStagesWorkflow, "  lint:\n", "  deploy-matrix:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n  lint:\n", 1)
		_, err := compileRuntimeMatrix(t, source, nil)
		if err == nil || !strings.Contains(err.Error(), `"gha-deploy-matrix" collides with a step from job "deploy-matrix"`) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRuntimeMatrixRejectsUnsupportedGraphs(t *testing.T) {
	tests := []struct {
		name, source, want string
	}{
		{
			// The consumer's own instances are unknown until its producer
			// runs, so a matrix cannot read its output.
			name:   "matrix read from a deferred consumer's output",
			source: strings.Replace(strings.Replace(runtimeMatrixWorkflow, "    runs-on: ${{ matrix.runner }}\n", "    runs-on: ${{ matrix.runner }}\n    outputs:\n      matrix: ${{ matrix.target }}\n", 1), "  publish:\n    needs: [build, lint]\n    runs-on: ubuntu-latest\n", "  publish:\n    needs: [build, lint]\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n        include: ${{ fromJSON(needs.build.outputs.matrix) }}\n", 1),
			want:   `producer "build" must have exactly one statically expanded instance`,
		},
		{
			name: "static job owning the deferred step key",
			source: runtimeMatrixWorkflow + `  build-matrix:
    runs-on: ubuntu-latest
    steps:
      - run: true
`,
			want: `deferred upload step key "gha-build-matrix" collides with a step from job "build-matrix"`,
		},
		{
			// The deferred job's own key is only created by the continuation
			// upload, so the collision must be caught before the initial
			// upload creates the deferred step.
			name: "deferred job owning the deferred step key",
			source: runtimeMatrixWorkflow + `  build-matrix:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`,
			want: `deferred upload step key "gha-build-matrix" collides with a step from job "build-matrix"`,
		},
		{
			// The continuation would only discover the duplicate when it
			// expands the deferred job, after the build has started.
			name:   "deferred job with duplicate matrix rows",
			source: strings.Replace(runtimeMatrixWorkflow, "    needs: [build, lint]\n", "    needs: [build, lint]\n    strategy:\n      matrix:\n        variant: [same, same]\n", 1),
			want:   `collides with a step from job "publish"`,
		},
		{
			// Every empty row takes the logical key, so two of them collide.
			name:   "deferred job with duplicate empty rows",
			source: strings.Replace(runtimeMatrixWorkflow, "    needs: [build, lint]\n", "    needs: [build, lint]\n    strategy:\n      matrix:\n        include: "+`"${{ fromJSON('[{}, {}]') }}"`+"\n", 1),
			want:   `deferred instance key "gha-publish" collides with a step from job "publish"`,
		},
		{
			// The continuation decides that an existing step with the gate's
			// key is the approval block, so no job may own that key.
			name: "static job owning a deferred approval gate key",
			source: strings.Replace(runtimeMatrixWorkflow, "    needs: plan\n    runs-on: ${{ matrix.runner }}\n", "    needs: plan\n    environment: production\n    runs-on: ${{ matrix.runner }}\n", 1) + "  " + productionGateJobID + `:
    runs-on: ubuntu-latest
    steps:
      - run: true
`,
			want: `approval gate key "` + environmentGateKey("", "production") + `" for environment "production" collides with a step from job "` + productionGateJobID + `"`,
		},
		{
			name: "deferred job owning a deferred approval gate key",
			source: strings.Replace(runtimeMatrixWorkflow, "    needs: plan\n    runs-on: ${{ matrix.runner }}\n", "    needs: plan\n    environment: production\n    runs-on: ${{ matrix.runner }}\n", 1) + "  " + productionGateJobID + `:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`,
			want: `deferred instance key "` + environmentGateKey("", "production") + `" collides with a step from job "build"`,
		},
		{
			// The initial upload may create this gate for the static job, so
			// the deferred job's key would be rejected as a duplicate.
			name: "deferred job owning a static approval gate key",
			source: strings.Replace(runtimeMatrixWorkflow, "  lint:\n    runs-on: ubuntu-latest\n", "  lint:\n    environment: production\n    runs-on: ubuntu-latest\n", 1) + "  " + productionGateJobID + `:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`,
			want: `deferred instance key "` + environmentGateKey("", "production") + `" collides with a step from job "lint"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := compileRuntimeMatrix(t, test.source, nil)
			var finding *ProcessingFinding
			if !errors.As(err, &finding) || finding.Code != CodeMatrixInvalid || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %s containing %q", err, CodeMatrixInvalid, test.want)
			}
		})
	}
}

// TestRuntimeMatrixRejectsConcurrencyGatedCallsNeedingDeferredJobs proves that
// a reusable workflow with its own concurrency group may need a static job in
// a workflow that also defers a matrix, but not the deferred jobs: its gates
// would have to be uploaded by the continuation, outside the concurrency
// analysis the initial upload performs.
func TestRuntimeMatrixRejectsConcurrencyGatedCallsNeedingDeferredJobs(t *testing.T) {
	repository := t.TempDir()
	writeWorkflow(t, repository, "release.yml", `on: workflow_call
concurrency:
  group: release
jobs:
  release:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	call := func(needs string) string {
		return writeWorkflow(t, repository, "caller.yml", runtimeMatrixWorkflow+`  release:
    needs: `+needs+`
    uses: ./.github/workflows/release.yml
`)
	}
	static := call("plan")
	bundle, err := CompileBundle(static, readFile(t, static), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatalf("gated call needing a static job: %v", err)
	}
	if len(bundle.IR.Continuations) != 1 || !slices.Equal(bundle.IR.Continuations[0].Jobs, []string{"build", "publish"}) {
		t.Fatalf("continuations = %#v, want the deferred matrix untouched by the gated call", bundle.IR.Continuations)
	}
	if called := jobKeys(bundle.IR)["gha-release-release"]; len(called.ConcurrencyGates) != 1 || called.ConcurrencyGates[0].Group != "release" {
		t.Fatalf("called job = %#v, want the release gate", called)
	}

	deferred := call("build")
	_, err = CompileBundle(deferred, readFile(t, deferred), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer")
	var finding *ProcessingFinding
	if !errors.As(err, &finding) || finding.Code != CodeMatrixInvalid || !strings.Contains(err.Error(), "reusable-workflow concurrency cannot depend on a needs-derived matrix") {
		t.Fatalf("gated call needing a deferred job: err = %v, want %s", err, CodeMatrixInvalid)
	}
}

// TestRuntimeMatrixSharesDeferredGates proves that deferred jobs declaring
// the same environment share one gate key rather than colliding with each
// other.
func TestRuntimeMatrixSharesDeferredGates(t *testing.T) {
	deferredProduction := strings.Replace(runtimeMatrixWorkflow, "    needs: plan\n    runs-on: ${{ matrix.runner }}\n", "    needs: plan\n    environment: production\n    runs-on: ${{ matrix.runner }}\n", 1)
	for name, source := range map[string]string{
		"two deferred jobs":                strings.Replace(deferredProduction, "    needs: [build, lint]\n", "    needs: [build, lint]\n    environment: production\n", 1),
		"case variants of one environment": strings.Replace(deferredProduction, "    needs: [build, lint]\n", "    needs: [build, lint]\n    environment: Production\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			ir, err := compileRuntimeMatrix(t, source, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(ir.Continuations) != 1 {
				t.Fatalf("continuations = %#v", ir.Continuations)
			}
		})
	}
}

// TestRuntimeMatrixReservesOnlyKnownDependentInstances proves that a
// deferred dependent reserves the keys its static instances will really take:
// flattened reusable job call.publish with a one-row matrix uploads
// gha-call-publish-<digest>, so a static call-publish job keeps gha-call-publish,
// while a matrix-free call.publish would take that key and must be rejected.
func TestRuntimeMatrixReservesOnlyKnownDependentInstances(t *testing.T) {
	caller := runtimeMatrixWorkflow + `  call:
    needs: build
    uses: ./.github/workflows/publish.yml
  call-publish:
    runs-on: ubuntu-latest
    steps:
      - run: true
`
	for name, test := range map[string]struct {
		matrix  string
		wantKey string
		wantErr string
	}{
		"one-row matrix": {matrix: "    strategy:\n      matrix:\n        os: [linux]\n", wantKey: "gha-call-publish-"},
		"matrix-free":    {wantErr: `deferred instance key "gha-call-publish" collides with a step from job "call-publish"`},
	} {
		t.Run(name, func(t *testing.T) {
			repository := t.TempDir()
			callerPath := writeWorkflow(t, repository, "caller.yml", caller)
			writeWorkflow(t, repository, "publish.yml", `on:
  workflow_call:
jobs:
  publish:
    runs-on: ubuntu-latest
`+test.matrix+`    steps:
      - run: true
`)
			ir, err := CompileIRWithOptionsContext(t.Context(), callerPath, []byte(caller), readFile(t, smokePath("events", "push.json")), defaultOptions())
			if test.wantErr != "" {
				var finding *ProcessingFinding
				if !errors.As(err, &finding) || finding.Code != CodeMatrixInvalid || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err = %v, want %s containing %q", err, CodeMatrixInvalid, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(ir.Continuations) != 1 || jobKeys(ir)["gha-call-publish"].LogicalJobID != "call-publish" {
				t.Fatalf("continuations = %#v, jobs = %#v", ir.Continuations, jobKeys(ir))
			}
			instances := ir.Continuations[0].Instances["call.publish"]
			if len(instances) != 1 || !strings.HasPrefix(instances[0].Key, test.wantKey) || instances[0].Key == "gha-call-publish" {
				t.Fatalf("call.publish instances = %#v, want one digest-suffixed key", instances)
			}
		})
	}
}

// TestRuntimeMatrixRecordsDeferredJobSources proves the continuation records,
// for every deferred job, the workflow source the expanded jobs will carry,
// including the commit a remote reusable workflow was pinned to, and that the
// record stops matching once that reference moves.
func TestRuntimeMatrixRecordsDeferredJobSources(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", runtimeMatrixWorkflow+`  call:
    needs: build
    uses: owner/workflows/.github/workflows/publish.yml@v1
`)
	remoteRoot := t.TempDir()
	writeWorkflow(t, remoteRoot, "publish.yml", "on: workflow_call\njobs:\n  publish:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	fake := newFakeReusableRepositorySource(t, map[string]string{"owner/workflows": remoteRoot})
	compile := func(rows map[string][]map[string]any) IR {
		t.Helper()
		options := defaultOptions()
		options.RepositorySource = MemoizeRepositorySource(fake)
		options.RuntimeMatrixRows = rows
		ir, err := CompileIRWithOptionsContext(t.Context(), callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), options)
		if err != nil {
			t.Fatal(err)
		}
		return ir
	}

	deferred := compile(nil)
	if len(deferred.Continuations) != 1 {
		t.Fatalf("continuations = %#v", deferred.Continuations)
	}
	sources := deferred.Continuations[0].Sources
	if len(sources) != 3 || sources["build"].Remote != nil || sources["build"].Path != "./.github/workflows/caller.yml" || sources["publish"].Digest != sources["build"].Digest {
		t.Fatalf("sources = %#v", sources)
	}
	remote := sources["call.publish"]
	if remote.Path != "owner/workflows/.github/workflows/publish.yml@v1" || remote.Remote == nil || remote.Remote.Commit != strings.Repeat("a", 40) || remote.Remote.RequestedRef != "v1" {
		t.Fatalf("remote source = %#v", remote)
	}

	rows := map[string][]map[string]any{"build": {{"target": "x", "runner": "ubuntu-latest"}}}
	expanded := compile(rows)
	matched := 0
	for _, job := range expanded.Jobs {
		source, deferredJob := sources[job.LogicalJobID]
		if !deferredJob {
			continue
		}
		if !source.Matches(job) {
			t.Fatalf("expanded job %#v does not match its recorded source %#v", job, source)
		}
		matched++
	}
	if matched != 3 {
		t.Fatalf("matched %d expanded deferred jobs, want build, publish, and call.publish", matched)
	}

	// The remote tag moves to another commit before the deferred upload.
	fake.commits["owner/workflows"] = strings.Repeat("b", 40)
	for _, job := range compile(rows).Jobs {
		if job.LogicalJobID == "call.publish" && sources["call.publish"].Matches(job) {
			t.Fatalf("moved remote workflow still matches the recorded source %#v: %#v", sources["call.publish"], job.RemoteWorkflow)
		}
	}
}

// TestRuntimeMatrixRejectsWorkflowConcurrency proves a root workflow with
// `concurrency` cannot hold a needs-derived matrix. The initial upload's
// closing marker waits only for the steps it uploads, so the group would be
// released while the jobs of the deferred upload still run.
func TestRuntimeMatrixRejectsWorkflowConcurrency(t *testing.T) {
	source := strings.Replace(runtimeMatrixWorkflow, "on: push\n", "on: push\nconcurrency:\n  group: release\n  cancel-in-progress: false\n", 1)
	ir, err := compileRuntimeMatrix(t, source, nil)
	var finding *ProcessingFinding
	if !errors.As(err, &finding) || finding.Code != CodeMatrixInvalid || finding.Job != "build" || !strings.Contains(err.Error(), "workflow concurrency cannot be combined with a needs-derived matrix") {
		t.Fatalf("err = %v, want %s for job build", err, CodeMatrixInvalid)
	}
	if len(ir.Continuations) != 0 {
		t.Fatalf("continuations = %#v, want none", ir.Continuations)
	}
}

// runtimeMatrixConsumers appends count consumers of plan's matrix output, and
// the matching static jobs, to a workflow whose first jobs are plan.
func runtimeMatrixConsumers(count int, matrix string) string {
	var source strings.Builder
	source.WriteString(`on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.plan.outputs.matrix }}
    steps:
      - id: plan
        run: echo 'matrix=[]' >> "$GITHUB_OUTPUT"
`)
	for i := range count {
		fmt.Fprintf(&source, `  build-%d:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
%s    steps:
      - run: true
`, i, matrix)
	}
	return source.String()
}

// TestRuntimeMatrixSharesJobBudgetBetweenDeferredUploads proves the initial
// compilation gives every deferred upload an equal share of the jobs the
// graph bound leaves after the static graph, and that a continuation's
// recompilation, which supplies rows for one consumer, reproduces the other
// continuations' shares exactly, so the artifact comparison accepts it.
func TestRuntimeMatrixSharesJobBudgetBetweenDeferredUploads(t *testing.T) {
	t.Run("one deferred upload", func(t *testing.T) {
		ir, err := compileRuntimeMatrix(t, runtimeMatrixWorkflow, nil)
		if err != nil {
			t.Fatal(err)
		}
		// lint and plan stay static; build and publish are deferred.
		if len(ir.Continuations) != 1 || ir.Continuations[0].JobBudget != MaxRuntimeMatrixGraphJobs-2 {
			t.Fatalf("continuations = %#v, want a job budget of %d", ir.Continuations, MaxRuntimeMatrixGraphJobs-2)
		}
	})

	t.Run("three deferred uploads", func(t *testing.T) {
		deferred := "        include: ${{ fromJSON(needs.plan.outputs.matrix) }}\n"
		source := runtimeMatrixConsumers(3, deferred) + `  publish:
    needs: build-2
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [linux, macos]
    steps:
      - run: true
`
		initial, err := compileRuntimeMatrix(t, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		// plan is the only static job; the rest is shared three ways.
		want := (MaxRuntimeMatrixGraphJobs - 1) / 3
		if len(initial.Continuations) != 3 {
			t.Fatalf("continuations = %#v", initial.Continuations)
		}
		for _, continuation := range initial.Continuations {
			if continuation.JobBudget != want {
				t.Fatalf("continuation %s job budget = %d, want %d", continuation.StepKey, continuation.JobBudget, want)
			}
		}
		if got := initial.Continuations[2]; got.Descriptor.Job != "build-2" || got.DependentInstances() != 2 {
			t.Fatalf("build-2 continuation = %#v, want two publish instances", got)
		}
		rows := []map[string]any{{"target": "x"}, {"target": "y"}, {"target": "z"}}
		for _, supplied := range []string{"build-0", "build-2"} {
			expanded, err := compileRuntimeMatrix(t, source, map[string][]map[string]any{supplied: rows})
			if err != nil {
				t.Fatal(err)
			}
			var others []RuntimeMatrixContinuation
			for _, continuation := range initial.Continuations {
				if continuation.Descriptor.Job != supplied {
					others = append(others, continuation)
				}
			}
			if !reflect.DeepEqual(expanded.Continuations, others) {
				t.Fatalf("recompiling with rows for %s changed the other continuations:\n%#v\nwant\n%#v", supplied, expanded.Continuations, others)
			}
		}
	})

	t.Run("static graph leaves no share", func(t *testing.T) {
		// Four static jobs with 256 instances each fill the graph bound before
		// plan, build, and publish are counted.
		var values []string
		for i := range maxMatrixInstances {
			values = append(values, strconv.Itoa(i))
		}
		full := "        n: [" + strings.Join(values, ", ") + "]\n"
		source := runtimeMatrixConsumers(4, full) + `  build:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: true
  publish:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`
		_, err := compileRuntimeMatrix(t, source, nil)
		var finding *ProcessingFinding
		want := fmt.Sprintf("the graph bound of %d jobs leaves 0 for each of the workflow's 1 deferred uploads after its %d static jobs, but jobs \"build\", \"publish\" already need 2", MaxRuntimeMatrixGraphJobs, 4*maxMatrixInstances+1)
		if !errors.As(err, &finding) || finding.Code != CodeMatrixInvalid || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %s containing %q", err, CodeMatrixInvalid, want)
		}
	})
}

func TestRuntimeMatrixPinsDeferredActions(t *testing.T) {
	repository, remote := t.TempDir(), t.TempDir()
	writeAction(t, remote, "", "name: x\nruns:\n  using: node24\n  main: index.js\n")
	workflowPath := writeWorkflow(t, repository, "actions.yml", strings.Replace(runtimeMatrixWorkflow, "      - run: echo ${{ matrix.target }}\n", "      - uses: owner/action@v1\n", 1))
	fake := &fakeActionSource{root: remote, calls: map[string]int{}}
	compile := func(rows map[string][]map[string]any, locks []plan.ActionLock) (Bundle, error) {
		t.Helper()
		return CompileBundleWithOptions(workflowPath, readFile(t, workflowPath), pushEvent(t), "0.0.0-test", testDistributionDigest, "importer", Options{
			EventTrust:               EventUntrusted,
			Runners:                  RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "hosted"}, UntrustedQueues: []string{"hosted"}},
			ResolveActions:           true,
			ActionSource:             fake,
			RuntimeMatrixRows:        rows,
			RuntimeMatrixActionLocks: locks,
		})
	}

	deferred, err := compile(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deferred.IR.Continuations) != 1 {
		t.Fatalf("continuations = %#v", deferred.IR.Continuations)
	}
	recorded := deferred.IR.Continuations[0].ActionLocks
	if len(recorded) != 1 || recorded[0].Repository != "owner/action" || recorded[0].RequestedRef != "v1" || recorded[0].Commit != strings.Repeat("a", 40) {
		t.Fatalf("recorded action locks = %#v", recorded)
	}

	// The tag moves before the deferred upload. The recorded lock keeps the
	// deferred job on the commit the initial upload resolved.
	fake.commit = strings.Repeat("b", 40)
	rows := map[string][]map[string]any{"build": {{"target": "x", "runner": "ubuntu-latest"}}}
	deferredLocks := func(bundle Bundle) []plan.ActionLock {
		t.Helper()
		for _, artifact := range bundle.Plans {
			if artifact.Job.Workflow.LogicalJobID == "build" {
				return artifact.Job.Actions
			}
		}
		t.Fatalf("no build plan in %#v", bundle.Plans)
		return nil
	}
	pinned, err := compile(rows, recorded)
	if err != nil {
		t.Fatal(err)
	}
	if locks := deferredLocks(pinned); len(locks) != 1 || locks[0].Commit != strings.Repeat("a", 40) || locks[0].RequestedRef != "v1" {
		t.Fatalf("pinned deferred locks = %#v", locks)
	}
	unpinned, err := compile(rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if locks := deferredLocks(unpinned); len(locks) != 1 || locks[0].Commit != strings.Repeat("b", 40) {
		t.Fatalf("unpinned deferred locks = %#v", locks)
	}
}

func TestRuntimeMatrixResolvesDeferredActionsBeforeUpload(t *testing.T) {
	repository := t.TempDir()
	workflowPath := writeWorkflow(t, repository, "missing-action.yml", strings.Replace(runtimeMatrixWorkflow, "      - run: echo ${{ matrix.target }}\n", "      - uses: ./missing\n", 1))
	_, err := CompileBundleWithOptions(workflowPath, readFile(t, workflowPath), pushEvent(t), "0.0.0-test", testDistributionDigest, "importer", Options{
		EventTrust:     EventUntrusted,
		Runners:        RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "hosted"}, UntrustedQueues: []string{"hosted"}},
		ResolveActions: true,
		ActionSource:   &fakeActionSource{root: t.TempDir(), calls: map[string]int{}},
	})
	var finding *ProcessingFinding
	if !errors.As(err, &finding) || finding.Code != CodeActionResolution || finding.Job != "build" || !strings.Contains(err.Error(), `deferred job "build"`) {
		t.Fatalf("missing deferred action error = %v", err)
	}
}

// TestRuntimeMatrixReportsVarsReferencedOnlyByDeferredActions proves a vars
// reference that lives only in an action of a deferred job is reported by
// ActionsReferenceVars, although the deferred jobs have no plan in the bundle.
// The importer resolves the scopes on that signal and records them for the
// deferred upload, which never fetches variables itself.
func TestRuntimeMatrixReportsVarsReferencedOnlyByDeferredActions(t *testing.T) {
	const varsAction = "name: region\ninputs:\n  region:\n    default: ${{ vars.AWS_REGION }}\nruns:\n  using: node24\n  main: index.js\n"
	const plainAction = "name: plain\nruns:\n  using: node24\n  main: index.js\n"
	for _, tc := range []struct {
		name   string
		job    string
		action string
		want   bool
	}{
		{"deferred job action reads vars", "build", varsAction, true},
		{"static job action reads vars", "lint", varsAction, true},
		{"deferred job action without vars", "build", plainAction, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := t.TempDir()
			writeAction(t, repository, ".github/actions/region", tc.action)
			step := "      - run: echo ${{ matrix.target }}\n"
			if tc.job == "lint" {
				step = "      - run: true\n  plan:"
			}
			replacement := "      - uses: ./.github/actions/region\n"
			if tc.job == "lint" {
				replacement += "  plan:"
			}
			source := strings.Replace(runtimeMatrixWorkflow, step, replacement, 1)
			if source == runtimeMatrixWorkflow {
				t.Fatal("workflow fixture did not change")
			}
			workflowPath := writeWorkflow(t, repository, "vars.yml", source)
			bundle, err := CompileBundleWithOptions(workflowPath, readFile(t, workflowPath), pushEvent(t), "0.0.0-test", testDistributionDigest, "importer", Options{
				EventTrust:     EventUntrusted,
				Runners:        RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "hosted"}, UntrustedQueues: []string{"hosted"}},
				ResolveActions: true,
				ActionSource:   &fakeActionSource{root: t.TempDir(), calls: map[string]int{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.IR.Continuations) != 1 {
				t.Fatalf("continuations = %#v", bundle.IR.Continuations)
			}
			if got := ActionsReferenceVars(bundle); got != tc.want {
				t.Fatalf("ActionsReferenceVars() = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestRuntimeMatrixPinsDeferredSelfRepositoryActions proves the initial upload
// resolves a deferred job's `$/` action against the containing workflow
// repository and commit, as it does for a job it expands statically, and that
// the continuation reuses the recorded lock.
func TestRuntimeMatrixPinsDeferredSelfRepositoryActions(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	source := strings.Replace(runtimeMatrixWorkflow, "      - run: echo ${{ matrix.target }}\n", "      - uses: $/.github/actions/setup\n", 1)
	workflowPath := writeWorkflow(t, workspace, "self.yml", source)
	writeWorkflow(t, remote, "self.yml", source)
	writeSelfAction(t, remote, ".github/actions/setup", "runs:\n  using: composite\n  steps:\n    - run: echo setup\n      shell: bash\n")
	fake := newFakeReusableRepositorySource(t, map[string]string{"source/repo": remote})
	commit := strings.Repeat("e", 40)
	compile := func(workflowSource *WorkflowSourceReference, rows map[string][]map[string]any, locks []plan.ActionLock) (Bundle, error) {
		t.Helper()
		options := defaultOptions()
		options.WorkflowSource = workflowSource
		options.RepositorySource = MemoizeRepositorySource(fake)
		options.ActionSource, options.ResolveActions = options.RepositorySource, true
		options.RuntimeMatrixRows, options.RuntimeMatrixActionLocks = rows, locks
		return CompileBundleWithOptions(workflowPath, []byte(source), pushEvent(t), "0.0.0-test", testDistributionDigest, "importer", options)
	}
	identity := &WorkflowSourceReference{Repository: "source/repo", Commit: commit}

	deferred, err := compile(identity, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deferred.IR.Continuations) != 1 {
		t.Fatalf("continuations = %#v", deferred.IR.Continuations)
	}
	recorded := deferred.IR.Continuations[0].ActionLocks
	if len(recorded) != 1 || recorded[0].Repository != "source/repo" || recorded[0].Path != ".github/actions/setup" || recorded[0].Commit != commit || recorded[0].SourceDigest == "" {
		t.Fatalf("recorded self-repository lock = %#v", recorded)
	}

	expanded, err := compile(identity, map[string][]map[string]any{"build": {{"target": "x", "runner": "ubuntu-latest"}}}, recorded)
	if err != nil {
		t.Fatal(err)
	}
	var build *plan.Job
	for i := range expanded.Plans {
		if expanded.Plans[i].Job.Workflow.LogicalJobID == "build" {
			build = &expanded.Plans[i].Job
		}
	}
	if build == nil {
		t.Fatalf("no build plan in %#v", expanded.Plans)
	}
	if !reflect.DeepEqual(build.Actions, recorded) || build.Workflow.SelfRepository == nil || build.Workflow.SelfRepository.Repository != "source/repo" {
		t.Fatalf("expanded self-repository plan = %#v, want locks %#v", build, recorded)
	}

	// The event repository must not stand in for a missing workflow identity
	// for a deferred job any more than for a static one.
	if _, err := compile(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "containing workflow repository") {
		t.Fatalf("event identity substituted for missing workflow identity: %v", err)
	}
}
