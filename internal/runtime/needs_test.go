package runtime

import (
	"context"
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
