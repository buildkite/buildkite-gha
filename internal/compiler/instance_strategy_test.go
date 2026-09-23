package compiler

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompileUsesInstanceStrategyForSchedulingAndNames(t *testing.T) {
	const source = `on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      rows: ${{ steps.plan.outputs.rows }}
    steps: [{id: plan, run: true}]
  build:
    needs: plan
    name: Build ${{ matrix.target }} ${{ strategy.job-index }}/${{ strategy.job-total }}
    runs-on: ${{ format('runner-{0}-{1}', strategy.job-index, strategy.job-total) }}
    concurrency: build-${{ matrix.target }}-${{ strategy.job-index }}-${{ strategy.job-total }}
    strategy:
      matrix:
        target: [zeta, skipped, alpha]
        exclude: [{target: skipped}]
    steps: [{run: true}]
`
	for _, deferred := range []bool{false, true} {
		for _, selector := range []string{
			"${{ format('runner-{0}-{1}', strategy.job-index, strategy.job-total) }}",
			"['runner-${{ strategy.job-index }}-${{ strategy.job-total }}']",
		} {
			t.Run(fmt.Sprintf("%s/deferred=%t", selector, deferred), func(t *testing.T) {
				workflow := strings.Replace(source, "${{ format('runner-{0}-{1}', strategy.job-index, strategy.job-total) }}", selector, 1)
				options := defaultOptions()
				options.EventTrust = EventTrusted
				options.Runners.Labels["runner-0-2"] = "first"
				options.Runners.Labels["runner-1-2"] = "second"
				if deferred {
					workflow = strings.Replace(workflow, "target: [zeta, skipped, alpha]\n        exclude: [{target: skipped}]", "include: ${{ fromJSON(needs.plan.outputs.rows) }}", 1)
					options.RuntimeMatrixRows = map[string][]map[string]any{"build": {{"target": "zeta"}, {"target": "alpha"}}}
				}
				ir, err := CompileIRWithOptionsContext(t.Context(), "strategy.yml", []byte(workflow), pushEvent(t), options)
				if err != nil {
					t.Fatal(err)
				}
				if len(ir.Jobs) != 3 {
					t.Fatalf("jobs = %d, want producer and two matrix rows", len(ir.Jobs))
				}
				for i, want := range []struct{ target, label, runner, queue, group string }{
					{"zeta", "Build zeta 0/2", "runner-0-2", "first", "build-zeta-0-2"},
					{"alpha", "Build alpha 1/2", "runner-1-2", "second", "build-alpha-1-2"},
				} {
					got := ir.Jobs[i+1]
					if got.Matrix["target"] != want.target || got.Label != want.label || strings.Join(got.RunsOn, ",") != want.runner || got.Queue != want.queue || got.ConcurrencyGroup != want.group {
						t.Fatalf("instance %d = %+v, want %+v", i, got, want)
					}
				}
			})
		}
	}
}

func TestCompileInstanceStrategyPreservesNameSuffixAndFallback(t *testing.T) {
	for _, test := range []struct{ name, first, second string }{
		{"Build ${{ strategy.job-index }}/${{ strategy.job-total }}", "Build 0/2 (target=zeta)", "Build 1/2 (target=alpha)"},
		{"Build ${{ strategy.missing }}", "Build ${{ strategy.missing }} (target=zeta)", "Build ${{ strategy.missing }} (target=alpha)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := `on: push
jobs:
  build:
    name: ` + test.name + `
    runs-on: ubuntu-latest
    strategy:
      matrix:
        target: [zeta, alpha]
    steps: [{run: true}]
`
			ir, err := CompileIRWithOptionsContext(t.Context(), "strategy.yml", []byte(source), pushEvent(t), defaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if len(ir.Jobs) != 2 || ir.Jobs[0].Label != test.first || ir.Jobs[1].Label != test.second {
				t.Fatalf("names = %+v, want %q and %q", ir.Jobs, test.first, test.second)
			}
		})
	}
}

func TestCompileInstanceStrategyRetainsSchedulingRestrictions(t *testing.T) {
	const source = `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    concurrency: build-${{ strategy.job-index }}-${{ strategy.job-total }}
    strategy:
      max-parallel: 2
      matrix:
        target: [zeta, alpha]
    steps: [{run: true}]
`
	for _, test := range []struct{ name, source, want string }{
		{"varying concurrency", source, "concurrency groups that vary by matrix cannot be combined with strategy.max-parallel"},
		{"unmapped runner", strings.Replace(source, "runs-on: ubuntu-latest", "runs-on: runner-${{ strategy.job-index }}-${{ strategy.job-total }}", 1), `runner label "runner-0-2" is not mapped by policy`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileIRWithOptionsContext(t.Context(), "strategy.yml", []byte(test.source), pushEvent(t), defaultOptions())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeRunsOnReceivesInstanceStrategy(t *testing.T) {
	source := strings.Replace(runtimeRunsOnWorkflow, "${{ needs.plan.outputs.runner }}", "${{ strategy.job-index == 0 && strategy.job-total == 1 && needs.plan.outputs.runner || 'unmapped' }}", 1)
	options := defaultOptions()
	options.EventTrust = EventTrusted
	options.Runners.Labels["ubuntu-22.04"] = "linux"
	initial, err := CompileIRWithOptionsContext(t.Context(), "strategy.yml", []byte(source), pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Continuations) != 1 {
		t.Fatalf("continuations = %+v, want deferred runner selection", initial.Continuations)
	}
	options.RuntimeRunsOnOutputs = map[string]string{"build": "ubuntu-22.04"}
	expanded, err := CompileIRWithOptionsContext(t.Context(), "strategy.yml", []byte(source), pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	build := jobKeys(expanded)["gha-build"]
	if len(expanded.Continuations) != 0 || strings.Join(build.RunsOn, ",") != "ubuntu-22.04" || build.Queue != "linux" {
		t.Fatalf("expanded runner selection = %+v, continuations = %+v", build, expanded.Continuations)
	}
}
