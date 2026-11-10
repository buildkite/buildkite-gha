package compiler

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompileBundleResolvesJobControlExpressions(t *testing.T) {
	for _, test := range []struct {
		name, timeout, parallel string
		minutes                 []float64
		limit                   int
	}{
		{"constants", "${{ 5 }}", "${{ 2 }}", []float64{5, 5}, 2},
		{"fractional timeout", "${{ 0.5 }}", "${{ 300 }}", []float64{0.5, 0.5}, 300},
		{"timeout maximum", "${{ 360 }}", "${{ 1 }}", []float64{360, 360}, 1},
		{"matrix and strategy", "${{ matrix.minutes || strategy.max-parallel }}", "${{ 3 }}", []float64{7, 3}, 3},
		{"event and vars", "${{ github.event.ref == 'refs/heads/main' && fromJSON(vars.TIMEOUT) || 9 }}", "${{ github.ref_name == 'main' && fromJSON(vars.PARALLEL) || 1 }}", []float64{11, 11}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := fmt.Sprintf(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    timeout-minutes: %s
    strategy:
      max-parallel: %s
      matrix:
        minutes: [7, 0]
    steps: [{run: true}]
`, test.timeout, test.parallel)
			options := defaultOptions()
			options.Vars.Repository = map[string]string{"TIMEOUT": "11", "PARALLEL": "2"}
			bundle, err := CompileBundleWithOptions("controls.yml", []byte(source), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", options)
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Plans) != 2 || len(bundle.GeneratedWorkflow.Jobs) != 2 {
				t.Fatalf("plans/pipeline jobs = %d/%d, want 2/2", len(bundle.Plans), len(bundle.GeneratedWorkflow.Jobs))
			}
			for i, artifact := range bundle.Plans {
				if got := artifact.Job.ExecutionJob().TimeoutMinutes; got != test.minutes[i] {
					t.Errorf("job %d timeout = %v, want %v", i, got, test.minutes[i])
				}
				job := bundle.GeneratedWorkflow.Jobs[i]
				if job.Concurrency != test.limit || job.ConcurrencyGroup == "" || job.ConcurrencyGroup != bundle.GeneratedWorkflow.Jobs[0].ConcurrencyGroup {
					t.Errorf("job %d scheduling = %#v, want shared limit %d", i, job, test.limit)
				}
			}
			if !strings.Contains(string(bundle.Pipeline), fmt.Sprintf("concurrency: %d", test.limit)) {
				t.Fatalf("pipeline does not emit limit %d:\n%s", test.limit, bundle.Pipeline)
			}
		})
	}
}

func TestCompileBundleResolvesDispatchJobControls(t *testing.T) {
	source := []byte(`on:
  workflow_dispatch:
    inputs:
      minutes: {type: number, default: 17}
      parallel: {type: string, default: '3'}
jobs:
  test:
    runs-on: ubuntu-latest
    timeout-minutes: ${{ inputs.minutes }}
    strategy:
      max-parallel: ${{ fromJSON(inputs.parallel) }}
      matrix: {shard: [one, two]}
    steps: [{run: true}]
`)
	for _, test := range []struct {
		inputs   string
		minutes  float64
		parallel int
	}{{`{}`, 17, 3}, {`{"minutes":"23", "parallel":"1"}`, 23, 1}} {
		event := strings.Replace(string(readFile(t, smokePath("events", "workflow_dispatch.json"))), `"inputs": {}`, `"inputs": `+test.inputs, 1)
		bundle, err := CompileBundleWithOptions("controls.yml", source, []byte(event), "0.0.0-test", testDistributionDigest, "gha-importer", defaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		for i, artifact := range bundle.Plans {
			if artifact.Job.ExecutionJob().TimeoutMinutes != test.minutes || bundle.GeneratedWorkflow.Jobs[i].Concurrency != test.parallel {
				t.Fatalf("dispatch controls did not resolve to %v/%d", test.minutes, test.parallel)
			}
		}
	}
}

func TestCompileBundleScopesReusableJobControls(t *testing.T) {
	repository := t.TempDir()
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  first:
    uses: ./.github/workflows/wrapper.yml
    with: {minutes: 7, parallel: 2}
  second:
    uses: ./.github/workflows/wrapper.yml
    with: {minutes: 13, parallel: 1}
`)
	writeWorkflow(t, repository, "wrapper.yml", `on:
  workflow_call:
    inputs:
      minutes: {type: number, required: true}
      parallel: {type: number, required: true}
jobs:
  nested:
    uses: ./.github/workflows/callee.yml
    with:
      minutes: ${{ inputs.minutes }}
      parallel: ${{ inputs.parallel }}
`)
	writeWorkflow(t, repository, "callee.yml", `on:
  workflow_call:
    inputs:
      minutes: {type: number, required: true}
      parallel: {type: number, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    timeout-minutes: ${{ matrix.fast && 0.5 || inputs['minutes'] }}
    strategy:
      max-parallel: ${{ inputs.parallel }}
      matrix:
        fast: [false, true]
    steps: [{run: true}]
`)
	bundle, err := CompileBundleWithOptions(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 4 || len(bundle.GeneratedWorkflow.Jobs) != 4 {
		t.Fatalf("plans/pipeline jobs = %d/%d, want 4/4", len(bundle.Plans), len(bundle.GeneratedWorkflow.Jobs))
	}
	for i, artifact := range bundle.Plans {
		if got := artifact.Job.ExecutionJob().TimeoutMinutes; got != []float64{7, 0.5, 13, 0.5}[i] {
			t.Errorf("job %d timeout = %v", i, got)
		}
		if got := bundle.GeneratedWorkflow.Jobs[i].Concurrency; got != []int{2, 2, 1, 1}[i] {
			t.Errorf("job %d max-parallel = %d", i, got)
		}
	}
	jobs := bundle.GeneratedWorkflow.Jobs
	if jobs[0].ConcurrencyGroup != jobs[1].ConcurrencyGroup || jobs[2].ConcurrencyGroup != jobs[3].ConcurrencyGroup || jobs[0].ConcurrencyGroup == jobs[2].ConcurrencyGroup {
		t.Fatal("reusable invocations did not retain separate parallelism groups")
	}
}

func TestCompileRejectsInvalidJobControlExpressions(t *testing.T) {
	for _, field := range []string{"timeout-minutes", "max-parallel"} {
		for _, value := range []string{"0", "-1", "true", "'5'", "null", "fromJSON('[2]')", "fromJSON('{}')", "fromJSON('not-json')", "1e309", "inputs.missing", "github.run_attempt", "fromJSON(needs.plan.outputs.limit)"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				control := "    timeout-minutes: ${{ " + value + " }}\n"
				if field == "max-parallel" {
					control = "    strategy:\n      max-parallel: ${{ " + value + " }}\n      matrix: {shard: [one, two]}\n"
				}
				source := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n" + control + "    steps: [{run: true}]\n"
				_, err := CompileBundleWithOptions("controls.yml", []byte(source), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", defaultOptions())
				if err == nil {
					t.Fatal("invalid or unresolved control compiled")
				}
			})
		}
	}
	for _, control := range []string{"timeout-minutes: ${{ 360.1 }}", "strategy: {max-parallel: '${{ 1.5 }}'}", "strategy: {max-parallel: '${{ 1e30 }}'}"} {
		source := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    " + control + "\n    steps: [{run: true}]\n"
		if _, err := CompileIRWithOptionsContext(t.Context(), "controls.yml", []byte(source), pushEvent(t), defaultOptions()); err == nil {
			t.Errorf("compiled invalid control %s", control)
		}
	}
}

func TestCompileRejectsUnresolvedReusableJobControls(t *testing.T) {
	for _, control := range []string{"timeout-minutes: ${{ inputs.limit }}", "strategy: {max-parallel: '${{ inputs.limit }}'}"} {
		for _, value := range []string{"${{ needs.plan.outputs.limit }}", "${{ '$' }}{{ 5 }}", "'private-invalid-number'"} {
			t.Run(control+"/"+value, func(t *testing.T) {
				repository := t.TempDir()
				caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs: {limit: '${{ steps.plan.outputs.limit }}'}
    steps: [{id: plan, run: true}]
  call:
    needs: plan
    uses: ./.github/workflows/callee.yml
    with:
      limit: `+value+"\n")
				writeWorkflow(t, repository, "callee.yml", `on:
  workflow_call:
    inputs:
      limit: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    `+control+`
    steps: [{run: true}]
`)
				_, err := CompileBundleWithOptions(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", defaultOptions())
				if err == nil || (!strings.Contains(err.Error(), "known before jobs run") && !strings.Contains(err.Error(), "finite number")) || strings.Contains(err.Error(), "private-invalid-number") {
					t.Fatalf("error = %v, want rejection without input data", err)
				}
			})
		}
	}
}
