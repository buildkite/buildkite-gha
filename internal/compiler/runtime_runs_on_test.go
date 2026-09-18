package compiler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const runtimeRunsOnWorkflow = `on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      runner: ${{ steps.select.outputs.runner }}
    steps:
      - id: select
        run: echo 'runner=ubuntu-22.04' >> "$GITHUB_OUTPUT"
  build:
    needs: plan
    runs-on: ${{ needs.plan.outputs.runner }}
    steps:
      - run: echo ready
  lint:
    runs-on: ubuntu-latest
    steps: [{run: true}]
  publish:
    needs: [build, lint]
    runs-on: ubuntu-latest
    steps: [{run: true}]
`

func TestRuntimeRunsOnOwnsDownstreamAndMatchesStaticSelection(t *testing.T) {
	for _, test := range []struct {
		name, selector, output, literal string
	}{
		{"scalar", "${{ needs.plan.outputs.runner }}", "ubuntu-22.04", "ubuntu-22.04"},
		{"array", "${{ fromJSON(needs.plan.outputs.runner) }}", `["ubuntu-22.04", "ubuntu-24.04"]`, "[ubuntu-22.04, ubuntu-24.04]"},
		{"template", "ubuntu-${{ needs.plan.outputs.runner }}", "24.04", "ubuntu-24.04"},
		{"list", "[ubuntu-latest, '${{ needs.plan.outputs.runner }}']", "ubuntu-22.04", "[ubuntu-latest, ubuntu-22.04]"},
		{"conditional", "${{ needs.plan.outputs.runner == 'older' && 'ubuntu-22.04' || 'ubuntu-24.04' }}", "older", "ubuntu-22.04"},
		{"case aliases", "${{ needs.PLAN.outputs.RUNNER == 'older' && needs.plan.outputs.runner == 'older' && 'ubuntu-22.04' || 'ubuntu-24.04' }}", "older", "ubuntu-22.04"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(runtimeRunsOnWorkflow, "${{ needs.plan.outputs.runner }}", test.selector, 1)
			compile := func(source string, outputs map[string]string) IR {
				t.Helper()
				options := defaultOptions()
				options.RuntimeRunsOnOutputs = outputs
				ir, err := CompileIRWithOptionsContext(t.Context(), "runner.yml", []byte(source), pushEvent(t), options)
				if err != nil {
					t.Fatal(err)
				}
				return ir
			}
			initial := compile(source, nil)
			if len(initial.Jobs) != 2 || len(initial.Continuations) != 1 {
				t.Fatalf("initial jobs=%v continuations=%v", jobKeys(initial), initial.Continuations)
			}
			continuation := initial.Continuations[0]
			if continuation.StepKey != "gha-build-runs-on" || !slices.Equal(continuation.Jobs, []string{"build", "publish"}) || continuation.Descriptor.ProducerOutput != "runner" || continuation.Descriptor.Schema != RuntimeRunsOnSchemaV1 || continuation.DependentInstances() != 1 {
				t.Fatalf("continuation=%+v", continuation)
			}
			dynamic := compile(source, map[string]string{"build": test.output})
			static := compile(strings.Replace(source, test.selector, test.literal, 1), nil)
			if len(dynamic.Continuations) != 0 || len(dynamic.Jobs) != 4 {
				t.Fatalf("expanded jobs=%v continuations=%v", jobKeys(dynamic), dynamic.Continuations)
			}
			for key, want := range jobKeys(static) {
				got := jobKeys(dynamic)[key]
				if got.Label != want.Label || got.Queue != want.Queue || !slices.Equal(got.RunsOn, want.RunsOn) || !slices.Equal(got.Needs, want.Needs) {
					t.Fatalf("job %q: got %+v, want %+v", key, got, want)
				}
			}
		})
	}
}

func TestRuntimeRunsOnRejectsUnsupportedScheduling(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{strings.Replace(runtimeRunsOnWorkflow, "needs: plan", "needs: lint", 1), "direct prerequisite"},
		{strings.Replace(runtimeRunsOnWorkflow, "needs.plan.outputs.runner", "needs.plan.outputs.missing", 1), "not declared"},
		{strings.Replace(runtimeRunsOnWorkflow, "needs.plan.outputs.runner", "needs.plan.result", 1), "must be needs.<job>.outputs.<name>"},
		{strings.Replace(runtimeRunsOnWorkflow, "needs.plan.outputs.runner", "needs.plan.outputs.runner || secrets.TOKEN", 1), "secrets"},
		{strings.Replace(runtimeRunsOnWorkflow, "needs.plan.outputs.runner", "needs.plan.outputs.runner || github.token", 1), "github.token"},
		{strings.Replace(runtimeRunsOnWorkflow, "needs.plan.outputs.runner", "needs.plan.outputs.runner || runner.os", 1), "runner"},
		{strings.Replace(runtimeRunsOnWorkflow, "needs: plan", "needs: plan\n    strategy:\n      matrix:\n        n: [1]", 1), "matrix on the same job"},
		{"concurrency: deploy\n" + runtimeRunsOnWorkflow, "workflow concurrency"},
		{strings.Replace(runtimeRunsOnWorkflow, "  plan:\n", "  plan:\n    strategy:\n      matrix:\n        n: [1, 2]\n", 1), "exactly one statically expanded instance"},
		{strings.Replace(runtimeRunsOnWorkflow, "runner: ${{ steps.select.outputs.runner }}", "runner: ${{ steps.select.outputs.runner }}\n      other: x", 1) + "  second:\n    needs: [plan, build]\n    runs-on: ${{ needs.plan.outputs.other }}\n    steps: [{run: true}]\n", "cannot depend on a deferred upload"},
	} {
		_, err := CompileIRWithOptionsContext(t.Context(), "runner.yml", []byte(test.source), pushEvent(t), defaultOptions())
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("want %q, got %v", test.want, err)
		}
	}
}

func TestRuntimeRunsOnRejectsMixedDeferredComponents(t *testing.T) {
	for _, test := range []struct{ name, source string }{
		{"join", runtimeRunsOnWorkflow + `  matrix:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.plan.outputs.runner) }}
    steps: [{run: true}]
  join:
    needs: [matrix, build]
    runs-on: ubuntu-latest
    steps: [{run: true}]
`},
		{"later matrix", runtimeRunsOnWorkflow + `  next:
    needs: build
    runs-on: ubuntu-latest
    outputs:
      rows: ${{ steps.select.outputs.rows }}
    steps:
      - id: select
        run: echo 'rows={"n":[1,2]}' >> "$GITHUB_OUTPUT"
  matrix:
    needs: next
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.next.outputs.rows) }}
    steps: [{run: true}]
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, supplied := range []bool{false, true} {
				options := defaultOptions()
				if supplied {
					options.RuntimeRunsOnOutputs = map[string]string{"build": "ubuntu-22.04"}
					options.RuntimeMatrixRows = map[string][]map[string]any{"matrix": {{"n": 1}, {"n": 2}}}
				}
				_, err := CompileIRWithOptionsContext(t.Context(), "runner.yml", []byte(test.source), pushEvent(t), options)
				if err == nil || !strings.Contains(err.Error(), "runs-on cannot join or feed another deferred stage") {
					t.Errorf("supplied=%v: expected mixed component rejection, got %v", supplied, err)
				}
			}
		})
	}
}

func TestRuntimeRunsOnForwardedInputKeepsCallerProvenance(t *testing.T) {
	root := t.TempDir()
	caller := writeWorkflow(t, root, "caller.yml", strings.Replace(runtimeRunsOnWorkflow, `    runs-on: ${{ needs.plan.outputs.runner }}
    steps:
      - run: echo ready`, `    uses: ./.github/workflows/forward.yml
    with:
      runner: ${{ needs.plan.outputs.runner }}`, 1))
	writeWorkflow(t, root, "forward.yml", `on:
  workflow_call:
    inputs:
      runner: {type: string, required: true}
jobs:
  call:
    uses: ./.github/workflows/callee.yml
    with:
      target: ${{ inputs.runner }}
`)
	writeWorkflow(t, root, "callee.yml", `on:
  workflow_call:
    inputs:
      target: {type: string, required: true}
jobs:
  local:
    runs-on: ubuntu-latest
    steps: [{run: true}]
  run:
    needs: local
    runs-on: ${{ fromJSON(inputs.target) }}
    steps:
      - run: echo '${{ inputs.target }}'
      - if: inputs.target == 'never'
        run: echo '${{ github.token }}'
`)
	options := defaultOptions()
	initial, err := CompileIRWithOptionsContext(t.Context(), caller, readFile(t, caller), pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Continuations) != 1 {
		t.Fatalf("continuations=%+v", initial.Continuations)
	}
	continuation := initial.Continuations[0]
	if continuation.Descriptor.Job != "build.call.run" || continuation.Descriptor.ProducerJob != "plan" || !slices.Equal(continuation.Jobs, []string{"build.call.run", "publish"}) {
		t.Fatalf("continuation=%+v", continuation)
	}
	options.RuntimeRunsOnOutputs = map[string]string{"build.call.run": `["ubuntu-22.04"]`}
	expanded, err := CompileIRWithOptionsContext(t.Context(), caller, readFile(t, caller), pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	run := jobKeys(expanded)["gha-build-call-run"]
	if !slices.Equal(run.RunsOn, []string{"ubuntu-22.04"}) || len(run.NeedGroups) != 1 || len(run.NeedGroups["local"]) != 1 || run.NeedGroups["plan"] != nil || run.Inputs["target"] != nil {
		t.Fatalf("scheduling leaked caller scope or resolved runtime input: %+v", run)
	}
	if len(run.DeferredInputs) != 1 || !reflect.DeepEqual(run.DeferredInputs["target"].NeedGroups["plan"], []string{"gha-plan"}) || !slices.Equal(run.Needs, []string{"gha-build-call-local", "gha-plan"}) {
		t.Fatalf("runtime input lost its producer dependency: %+v", run)
	}
	bundle, err := CompileBundleWithOptions(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", options)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, artifact := range bundle.Plans {
		if artifact.Job.Workflow.LogicalJobID == "build.call.run" {
			found = true
			if artifact.Job.GitHubToken == nil {
				t.Fatal("scheduling output incorrectly pruned runtime-dependent token authority")
			}
		}
	}
	if !found {
		t.Fatal("missing reusable runner job plan")
	}
}

func TestRuntimeRunsOnDescriptorAndOutputBounds(t *testing.T) {
	// The label stays short so only the producer-output bound is exercised.
	source := strings.Replace(runtimeRunsOnWorkflow, "${{ needs.plan.outputs.runner }}", "${{ needs.plan.outputs.runner && 'ubuntu-22.04' }}", 1)
	options := defaultOptions()
	ir, err := CompileIRWithOptionsContext(t.Context(), "runner.yml", []byte(source), pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := ir.Continuations[0].Descriptor
	encoded, err := EncodeRuntimeMatrixDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRuntimeMatrixDescriptor(encoded)
	if err != nil || !reflect.DeepEqual(decoded, descriptor) {
		t.Fatalf("descriptor roundtrip=%+v error=%v", decoded, err)
	}
	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "runtime-runs-on-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument, document any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(RuntimeRunsOnSchemaV1, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(RuntimeRunsOnSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(document); err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandRuntimeMatrixOutput(descriptor, []byte(`[{"os":"linux"}]`), nil); err == nil {
		t.Fatal("runner descriptor accepted by matrix expansion")
	}
	for _, output := range []string{strings.Repeat("x", 1024), strings.Repeat("x", 1025), "\xff"} {
		options.RuntimeRunsOnOutputs = map[string]string{"build": output}
		_, err := CompileIRWithOptionsContext(t.Context(), "runner.yml", []byte(source), pushEvent(t), options)
		if len(output) == 1024 {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "valid UTF-8 and at most 1024 bytes") {
			t.Fatalf("output length=%d error=%v", len(output), err)
		}
	}
}
