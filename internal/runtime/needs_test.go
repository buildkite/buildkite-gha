package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

type deferredInputRunner struct {
	jobID string
	path  string
	data  []byte
}

func (r deferredInputRunner) Run(ctx context.Context, root string, command string, args []string, stdin []byte) ([]byte, error) {
	return resultManifestRunner{r.path: {jobID: r.jobID, data: r.data}}.Run(ctx, root, command, args, stdin)
}

// resultManifestRunner serves one uploaded result manifest per artifact path.
type resultManifestRunner map[string]struct {
	jobID string
	data  []byte
}

func (r resultManifestRunner) Run(_ context.Context, _ string, _ string, args []string, _ []byte) ([]byte, error) {
	if len(args) >= 4 && args[0] == "artifact" {
		if manifest, ok := r[args[2]]; ok {
			switch args[1] {
			case "search":
				return []byte(manifest.jobID + "\n"), nil
			case "download":
				path := filepath.Join(args[3], filepath.FromSlash(args[2]))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					return nil, err
				}
				return nil, os.WriteFile(path, manifest.data, 0o600)
			}
		}
	}
	return nil, fmt.Errorf("unexpected agent command: %v", args)
}

func TestResolveDeferredInputsRendersTemplateFromVerifiedOutputs(t *testing.T) {
	const buildID = "11111111-1111-4111-8111-111111111111"
	runner := resultManifestRunner{}
	sources := map[string][]plan.NeedSource{}
	outputs := map[string][]plan.NeedOutput{}
	for i, producer := range []struct {
		need, stepKey string
		outputs       []transport.Output
	}{
		{need: "meta", stepKey: "gha-meta", outputs: []transport.Output{{Name: "tag", Value: "v4.3.0"}, {Name: "flavor", Value: "latest"}}},
		{need: "produce", stepKey: "gha-produce-outputter", outputs: []transport.Output{{Name: "subject", Value: "sha256:abc"}}},
	} {
		planDigest := transport.Digest([]byte(producer.stepKey + " plan"))
		jobID := fmt.Sprintf("2222222%d-2222-4222-8222-222222222222", i)
		manifest, err := transport.MarshalResultManifest(transport.ResultManifest{
			PlanDigest: planDigest,
			Producer:   transport.Producer{BuildID: buildID, JobID: jobID, StepKey: producer.stepKey},
			Result:     "success",
			Outputs:    producer.outputs,
		})
		if err != nil {
			t.Fatal(err)
		}
		runner[transport.ResultPath(producer.stepKey, planDigest)] = struct {
			jobID string
			data  []byte
		}{jobID: jobID, data: manifest}
		sources[producer.need] = []plan.NeedSource{{StepKey: producer.stepKey, PlanDigest: planDigest}}
		for _, output := range producer.outputs {
			outputs[producer.need] = append(outputs[producer.need], plan.NeedOutput{Name: output.Name, StepKey: producer.stepKey, Output: output.Name})
		}
	}
	deferred := map[string]plan.DeferredInput{
		"tags": {
			Template:    "type=raw,value=${{ needs.Meta.outputs.tag }}\ntype=raw,value=main-${{ needs.produce.outputs.subject }}\n",
			NeedSources: sources,
			NeedOutputs: outputs,
		},
		"flavor": {
			Template:    "${{ format('latest={0},missing={1}', needs.meta.outputs.flavor, needs.meta.outputs.absent) }}",
			NeedSources: map[string][]plan.NeedSource{"meta": sources["meta"]},
			NeedOutputs: map[string][]plan.NeedOutput{"meta": outputs["meta"]},
		},
	}

	inputs, err := ResolveDeferredInputs(t.Context(), transport.Agent{Runner: runner}, t.TempDir(), buildID, deferred)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"tags":   "type=raw,value=v4.3.0\ntype=raw,value=main-sha256:abc\n",
		"flavor": "latest=latest,missing=",
	}
	if !reflect.DeepEqual(inputs, want) {
		t.Fatalf("ResolveDeferredInputs() = %#v, want %#v", inputs, want)
	}

	deferred["tags"] = plan.DeferredInput{Template: "${{ needs.other.outputs.tag }}", NeedSources: sources, NeedOutputs: outputs}
	if _, err := ResolveDeferredInputs(t.Context(), transport.Agent{Runner: runner}, t.TempDir(), buildID, deferred); err == nil || !strings.Contains(err.Error(), `input "tags"`) || !strings.Contains(err.Error(), `unavailable need "other"`) {
		t.Fatalf("ResolveDeferredInputs() unbound need error = %v", err)
	}
}

func TestNeedStatusesCopiesOnlyExpressionVisibleState(t *testing.T) {
	outputs := map[string]string{"release": "v1"}
	needs := map[string]plan.Need{
		"producer": {
			Result:    "success",
			Outputs:   outputs,
			Artifacts: []plan.NeedArtifact{{Name: "private-runtime-authority"}},
		},
	}

	got := needStatuses(needs)
	want := map[string]expression.NeedStatus{
		"producer": {Outputs: map[string]string{"release": "v1"}, Result: "success"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("needStatuses() = %#v, want %#v", got, want)
	}
	outputs["release"] = "plan"
	if got["producer"].Outputs["release"] != "v1" {
		t.Fatalf("expression output changed through plan: %#v", got)
	}
	got["producer"].Outputs["release"] = "expression"
	if outputs["release"] != "plan" {
		t.Fatalf("plan output changed through expression state: %#v", outputs)
	}
}

func TestNeedsJSONCompilesAndRunsWithOnlyDirectDependencies(t *testing.T) {
	workspace := t.TempDir()
	const workflowPath = ".github/workflows/check.yml"
	const source = `on: push
jobs:
  ancestor:
    runs-on: ubuntu-latest
    steps: [{run: echo ancestor}]
  build:
    needs: ancestor
    runs-on: ubuntu-latest
    steps: [{run: echo build}]
  failed:
    runs-on: ubuntu-latest
    steps: [{run: exit 1}]
  skipped:
    runs-on: ubuntu-latest
    steps: [{run: echo skipped}]
  cancelled:
    runs-on: ubuntu-latest
    steps: [{run: echo cancelled}]
  check:
    needs: [build, failed, skipped, cancelled]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - env:
          NEEDS: ${{ toJSON(needs) }}
        run: printf '%s' "$NEEDS" > needs.json
`
	writeFixtureFile(t, workspace, workflowPath, source)
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := compileUntrustedPlans(filepath.Join(workspace, workflowPath), []byte(source), event, "test", "sha256:"+strings.Repeat("2", 64), "test")
	if err != nil {
		t.Fatal(err)
	}
	var job plan.Job
	for _, candidate := range jobs {
		if candidate.Workflow.LogicalJobID == "check" {
			job = candidate
		}
	}
	if len(job.NeedSources) != 4 || len(job.NeedSources["ancestor"]) != 0 {
		t.Fatalf("direct sources = %#v", job.NeedSources)
	}
	const buildID = "11111111-1111-4111-8111-111111111111"
	const jobID = "22222222-2222-4222-8222-222222222222"
	runner := resultManifestRunner{}
	want := map[string]map[string]any{
		"build":     {"result": "success", "outputs": map[string]any{"tag": "v1\n\"<&>"}},
		"failed":    {"result": "failure", "outputs": map[string]any{}},
		"skipped":   {"result": "skipped", "outputs": map[string]any{}},
		"cancelled": {"result": "cancelled", "outputs": map[string]any{}},
	}
	for name, sources := range job.NeedSources {
		producer := sources[0]
		var outputs []transport.Output
		if name == "build" {
			outputs = []transport.Output{{Name: "tag", Value: "v1\n\"<&>"}}
		}
		manifest, err := transport.MarshalResultManifest(transport.ResultManifest{
			PlanDigest: producer.PlanDigest,
			Producer:   transport.Producer{BuildID: buildID, JobID: jobID, StepKey: producer.StepKey},
			Result:     want[name]["result"].(string), Outputs: outputs,
		})
		if err != nil {
			t.Fatal(err)
		}
		runner[transport.ResultPath(producer.StepKey, producer.PlanDigest)] = struct {
			jobID string
			data  []byte
		}{jobID: jobID, data: manifest}
	}
	job.Needs, err = ResolveNeeds(t.Context(), transport.Agent{Runner: runner}, t.TempDir(), buildID, job.NeedSources, job.NeedOutputs)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() = %#v, %v", result, err)
	}
	encoded, err := os.ReadFile(filepath.Join(workspace, "needs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("needs JSON = %s, %v; want %#v", encoded, err, want)
	}
}

func TestDeferredTypedInputsCompileAndRunNestedWorkflow(t *testing.T) {
	for _, enabled := range []string{"true", "false", ""} {
		t.Run("deps="+enabled, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/ci.yaml"
			caller := `on: push
jobs:
  detect:
    runs-on: ubuntu-latest
    outputs:
      deps: ${{ steps.classify.outputs.deps }}
      count: ${{ steps.classify.outputs.count }}
    steps:
      - id: classify
        run: echo unused
  supply-chain:
    needs: detect
    uses: ./.github/workflows/middle.yml
    with:
      deps: ${{ needs.detect.outputs.deps == 'true' }}
      count: ${{ fromJSON(needs.detect.outputs.count) }}
`
			writeFixtureFile(t, workspace, workflowPath, caller)
			writeFixtureFile(t, workspace, ".github/workflows/middle.yml", `on:
  workflow_call:
    inputs:
      deps: {type: boolean}
      count: {type: number}
jobs:
  call:
    if: inputs.deps
    uses: ./.github/workflows/leaf.yml
    with:
      deps: ${{ inputs.deps }}
      count: ${{ inputs.count }}
`)
			writeFixtureFile(t, workspace, ".github/workflows/leaf.yml", `on:
  workflow_call:
    inputs:
      deps: {type: boolean}
      count: {type: number}
jobs:
  scan:
    if: inputs.deps
    runs-on: ubuntu-latest
    outputs:
      ran: ${{ steps.scan.outputs.ran }}
    steps:
      - id: scan
        if: inputs.deps && inputs.count == 7
        run: echo ran=yes >> "$GITHUB_OUTPUT"
`)
			event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
			if err != nil {
				t.Fatal(err)
			}
			jobs, err := compileUntrustedPlans(filepath.Join(workspace, workflowPath), []byte(caller), event, "test", "sha256:"+strings.Repeat("2", 64), "test")
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 2 {
				t.Fatalf("jobs = %d, want producer and callee", len(jobs))
			}
			encoded, err := plan.Encode(jobs[1])
			if err != nil {
				t.Fatal(err)
			}
			job, err := plan.Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			const buildID = "11111111-1111-4111-8111-111111111111"
			const jobID = "22222222-2222-4222-8222-222222222222"
			producer := job.DeferredInputs["deps"].NeedSources["detect"][0]
			outputs := []transport.Output{{Name: "count", Value: "7"}}
			if enabled != "" {
				outputs = append(outputs, transport.Output{Name: "deps", Value: enabled})
			}
			manifest, err := transport.MarshalResultManifest(transport.ResultManifest{
				PlanDigest: producer.PlanDigest,
				Producer:   transport.Producer{BuildID: buildID, JobID: jobID, StepKey: producer.StepKey},
				Result:     "success",
				Outputs:    outputs,
			})
			if err != nil {
				t.Fatal(err)
			}
			agent := transport.Agent{Runner: deferredInputRunner{jobID: jobID, path: transport.ResultPath(producer.StepKey, producer.PlanDigest), data: manifest}}
			job.DeferredInputValues, err = ResolveDeferredInputs(t.Context(), agent, t.TempDir(), buildID, job.DeferredInputs)
			if err != nil {
				t.Fatal(err)
			}
			if job.DeferredInputValues["deps"] != (enabled == "true") {
				t.Fatalf("deps = %#v, want boolean", job.DeferredInputValues["deps"])
			}
			if job.DeferredInputValues["count"] != float64(7) {
				t.Fatalf("count = %#v, want number 7", job.DeferredInputValues["count"])
			}
			job.CallGuards, err = ResolveCallGuards(t.Context(), agent, t.TempDir(), buildID, job.CallGuards)
			if err != nil {
				t.Fatal(err)
			}
			job.Needs, err = ResolveNeeds(t.Context(), agent, t.TempDir(), buildID, job.NeedSources, job.NeedOutputs)
			if err != nil {
				t.Fatal(err)
			}
			result, err := (Runner{}).RunJob(t.Context(), job, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if enabled == "true" {
				if result.Conclusion != "success" || result.Outputs["ran"] != "yes" {
					t.Fatalf("enabled result = %#v", result)
				}
			} else if result.Conclusion != "skipped" || result.Outputs["ran"] != "" {
				t.Fatalf("disabled result = %#v", result)
			}
		})
	}
}

func TestDeferredReusableWorkflowInputFlowsFromVerifiedOutputToCalleeStep(t *testing.T) {
	const (
		buildID = "11111111-1111-4111-8111-111111111111"
		jobID   = "22222222-2222-4222-8222-222222222222"
		value   = "c2hhMjU2ICBzdWJqZWN0Cg=="
	)
	planDigest := transport.Digest([]byte("producer plan"))
	stepKey := "gha-hash"
	manifest, err := transport.MarshalResultManifest(transport.ResultManifest{
		PlanDigest: planDigest,
		Producer:   transport.Producer{BuildID: buildID, JobID: jobID, StepKey: stepKey},
		Result:     "success",
		Outputs:    []transport.Output{{Name: "hashes", Value: value}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := transport.ResultPath(stepKey, planDigest)
	inputs, err := ResolveDeferredInputs(t.Context(), transport.Agent{Runner: deferredInputRunner{jobID: jobID, path: artifactPath, data: manifest}}, t.TempDir(), buildID, map[string]plan.DeferredInput{
		"base64-subjects": {
			Template:    "${{ needs.hash.outputs.hashes }}",
			NeedSources: map[string][]plan.NeedSource{"hash": {{StepKey: stepKey, PlanDigest: planDigest}}},
			NeedOutputs: map[string][]plan.NeedOutput{"hash": {{Name: "hashes", StepKey: stepKey, Output: "hashes"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inputs["base64-subjects"] != value {
		t.Fatalf("resolved inputs = %#v", inputs)
	}

	workspace := t.TempDir()
	workflowPath := ".github/workflows/generator_generic_slsa3.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:        "create-file",
		Kind:      "run",
		Env:       map[string]string{"UNTRUSTED_SUBJECTS": "${{ inputs.base64-subjects }}"},
		Condition: "inputs.base64-subjects != ''",
		Command:   `test "$UNTRUSTED_SUBJECTS" = "` + value + `" && echo ran=yes >> "$GITHUB_OUTPUT"`,
	}})
	job.Outputs = map[string]string{"ran": "${{ steps.create-file.outputs.ran }}"}
	job.Dependencies = []string{stepKey}
	job.DeferredInputs = map[string]plan.DeferredInput{
		"base64-subjects": {
			Template:    "${{ needs.hash.outputs.hashes }}",
			NeedSources: map[string][]plan.NeedSource{"hash": {{StepKey: stepKey, PlanDigest: planDigest}}},
			NeedOutputs: map[string][]plan.NeedOutput{"hash": {{Name: "hashes", StepKey: stepKey, Output: "hashes"}}},
		},
	}
	job.CallGuards = []plan.CallGuard{{
		Condition:      "inputs.base64-subjects != ''",
		DeferredInputs: job.DeferredInputs,
	}}
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "deferred reusable-workflow inputs were not hydrated") {
		t.Fatalf("RunJob() unhydrated error = %v", err)
	}
	job.DeferredInputValues = inputs
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "call guard 1: deferred inputs were not hydrated") {
		t.Fatalf("RunJob() unhydrated guard error = %v", err)
	}
	job.CallGuards[0].DeferredInputValues = inputs
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["ran"] != "yes" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}
