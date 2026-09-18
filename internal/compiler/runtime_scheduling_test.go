package compiler

import (
	"strings"
	"testing"
)

func schedulingWorkflow() string {
	source := strings.Replace(runtimeMatrixWorkflow, "      matrix: ${{ steps.plan.outputs.matrix }}", "      matrix: ${{ steps.plan.outputs.matrix }}\n      group: ${{ steps.plan.outputs.group }}\n      parallel: ${{ steps.plan.outputs.parallel }}", 1)
	source = strings.Replace(source, "    needs: plan\n", "    needs: plan\n    concurrency: ${{ needs.plan.outputs.group }}\n", 1)
	return strings.Replace(source, "    strategy:\n", "    strategy:\n      max-parallel: ${{ fromJSON(needs.plan.outputs.parallel) }}\n", 1)
}

func TestRuntimeSchedulingKeepsContinuationOwnershipAndOutputData(t *testing.T) {
	source := schedulingWorkflow()
	initial, err := compileRuntimeMatrix(t, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Jobs) != 2 || len(initial.Continuations) != 1 || !initial.Continuations[0].Scheduling || strings.Join(initial.Continuations[0].Jobs, ",") != "build,publish" {
		t.Fatalf("initial graph = %#v", initial)
	}
	options := defaultOptions()
	options.RuntimeMatrixRows = map[string][]map[string]any{"build": {{"runner": "ubuntu-latest", "target": "one"}, {"runner": "ubuntu-latest", "target": "two"}, {"runner": "ubuntu-latest", "target": "three"}}}
	// Output data must not be parsed as another expression or alter authority.
	const group = "Deploy-${{ github.token }}"
	options.RuntimeSchedulingOutputs = map[string]map[string]string{"build": {"group": group, "parallel": "2"}}
	ir, err := CompileIRWithOptionsContext(t.Context(), "runtime.yml", []byte(source), readFile(t, smokePath("events", "push.json")), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Continuations) != 0 || len(ir.Jobs) != 6 {
		t.Fatalf("jobs = %d, continuations = %d", len(ir.Jobs), len(ir.Continuations))
	}
	for _, job := range ir.Jobs {
		if job.LogicalJobID == "build" && (job.ConcurrencyGroup != group || job.MaxParallel == nil || *job.MaxParallel != 2) {
			t.Fatalf("consumer scheduling = %#v", job)
		}
		if job.LogicalJobID == "publish" && len(job.Needs) != 4 {
			t.Fatalf("publish needs = %v, want all three consumers and lint", job.Needs)
		}
	}
}

func TestRuntimeSchedulingParallelGroupBoundsAndIsolation(t *testing.T) {
	jobID := strings.Repeat("b", 85)
	source := strings.ReplaceAll(schedulingWorkflow(), "build", jobID)
	source = strings.Replace(source, "    concurrency: ${{ needs.plan.outputs.group }}\n", "", 1)
	if _, err := compileRuntimeMatrix(t, source, nil); err != nil {
		t.Fatalf("initial compile: %v", err)
	}
	compileGroup := func(namespace, buildID string) string {
		t.Helper()
		options := defaultOptions()
		options.StepKeyNamespace = namespace
		options.RuntimeSchedulingBuildID = buildID
		options.RuntimeMatrixRows = map[string][]map[string]any{jobID: {{"runner": "ubuntu-latest", "target": "one"}, {"runner": "ubuntu-latest", "target": "two"}}}
		options.RuntimeSchedulingOutputs = map[string]map[string]string{jobID: {"parallel": "2"}}
		bundle, err := CompileBundleWithOptions("runtime.yml", []byte(source), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", options)
		if err != nil {
			t.Fatalf("deferred compile: %v", err)
		}
		group, consumers := "", 0
		for _, job := range bundle.GeneratedWorkflow.Jobs {
			if job.Concurrency == 0 {
				continue
			}
			if job.Concurrency != 2 || job.ConcurrencyGroup == "" || group != "" && group != job.ConcurrencyGroup {
				t.Fatalf("matrix consumers must share one limit: %#v", job)
			}
			group = job.ConcurrencyGroup
			consumers++
		}
		if consumers != 2 {
			t.Fatalf("scheduled consumers = %d, want 2", consumers)
		}
		return group
	}
	const buildID = "11111111-1111-4111-8111-111111111111"
	first := compileGroup("", buildID)
	if compileGroup("", buildID) != first {
		t.Fatal("replay changed the concurrency group")
	}
	namespaced := compileGroup("0123456789abcdef", buildID)
	otherBuild := compileGroup("", "22222222-2222-4222-8222-222222222222")
	if first == namespaced || first == otherBuild || namespaced == otherBuild {
		t.Fatal("different workflows or builds share a parallelism limit")
	}
}

func TestRuntimeSchedulingRejectsUnavailableAuthorityAndStages(t *testing.T) {
	for _, test := range []struct{ name, source, want string }{
		{"no matrix", strings.Replace(schedulingWorkflow(), "include: ${{ fromJSON(needs.plan.outputs.matrix) }}", "include: [{runner: ubuntu-latest, target: one}]", 1), "requires a needs-derived matrix"},
		{"other producer", strings.Replace(schedulingWorkflow(), "needs.plan.outputs.group", "needs.lint.outputs.group", 1), "direct prerequisite"},
		{"undeclared output", strings.Replace(schedulingWorkflow(), "needs.plan.outputs.group", "needs.plan.outputs.missing", 1), "not declared"},
		{"token in dead branch", strings.Replace(schedulingWorkflow(), "needs.plan.outputs.group", "needs.plan.outputs.group || (false && github.token)", 1), "github"},
		{"secret in dead branch", strings.Replace(schedulingWorkflow(), "needs.plan.outputs.group", "needs.plan.outputs.group || (false && secrets.DEPLOY)", 1), "secrets"},
		{"root gate", "concurrency: root\n" + schedulingWorkflow(), "workflow concurrency cannot be combined"},
		{"job cancellation", strings.Replace(schedulingWorkflow(), "concurrency: ${{ needs.plan.outputs.group }}", "concurrency:\n      group: ${{ needs.plan.outputs.group }}\n      cancel-in-progress: true", 1), "cancel-in-progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := compileRuntimeMatrix(t, test.source, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeSchedulingKeepsJoinsAndChainsOutsideItsComponent(t *testing.T) {
	independent := schedulingWorkflow() + `  test:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps: [{run: true}]
`
	joined := strings.Replace(independent, "needs: [build, lint]", "needs: [build, test, lint]", 1)
	firstStage := strings.Replace(runtimeMatrixStagesWorkflow, runtimeMatrixWorkflow, schedulingWorkflow(), 1)
	laterStage := strings.Replace(runtimeMatrixStagesWorkflow, "    needs: package\n", "    needs: package\n    concurrency: ${{ needs.package.outputs.matrix }}\n", 1)
	for _, test := range []struct {
		name, source string
		allowed      bool
	}{
		{"independent", independent, true},
		{"joined", joined, false},
		{"first stage", firstStage, false},
		{"later stage", laterStage, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, resolved := range []bool{false, true} {
				options := defaultOptions()
				if resolved {
					options.RuntimeMatrixRows = map[string][]map[string]any{
						"build": {{"runner": "ubuntu-latest", "target": "one"}},
						"test":  {{"target": "unit"}}, "deploy": {{"region": "eu"}}, "release": {{"channel": "stable"}},
					}
					options.RuntimeSchedulingOutputs = map[string]map[string]string{
						"build": {"group": "deploy", "parallel": "2"}, "deploy": {"matrix": "deploy"},
					}
				}
				_, err := CompileIRWithOptionsContext(t.Context(), "runtime.yml", []byte(test.source), pushEvent(t), options)
				if test.allowed && err != nil || !test.allowed && (err == nil || !strings.Contains(err.Error(), "joined or chained matrices")) {
					t.Fatalf("resolved=%v: error = %v, allowed = %v", resolved, err, test.allowed)
				}
			}
		})
	}
}

func TestRuntimeSchedulingValidatesOutputLimits(t *testing.T) {
	for _, parallel := range []string{"0", "-1", "1.5", "257", "true", `"2"`, "null", "1e999", "not-json", ""} {
		t.Run(parallel, func(t *testing.T) {
			options := defaultOptions()
			options.RuntimeMatrixRows = map[string][]map[string]any{"build": {{"runner": "ubuntu-latest", "target": "one"}}}
			options.RuntimeSchedulingOutputs = map[string]map[string]string{"build": {"group": "deployment", "parallel": parallel}}
			_, err := CompileIRWithOptionsContext(t.Context(), "runtime.yml", []byte(schedulingWorkflow()), readFile(t, smokePath("events", "push.json")), options)
			if err == nil {
				t.Fatalf("max-parallel output %q was accepted", parallel)
			}
		})
	}
}

func TestRuntimeSchedulingRejectsSiblingWorkflowGates(t *testing.T) {
	repository := t.TempDir()
	writeWorkflow(t, repository, "gated.yml", `on: workflow_call
concurrency: sibling
jobs:
  release:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	path := writeWorkflow(t, repository, "caller.yml", schedulingWorkflow()+`  release:
    needs: plan
    uses: ./.github/workflows/gated.yml
`)
	_, err := CompileBundle(path, readFile(t, path), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with reusable-workflow concurrency gates") {
		t.Fatalf("sibling gate error = %v", err)
	}
}
