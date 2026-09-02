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

func (r deferredInputRunner) Run(_ context.Context, _ string, _ string, args []string, _ []byte) ([]byte, error) {
	if len(args) >= 5 && args[0] == "artifact" && args[1] == "search" && args[2] == r.path {
		return []byte(r.jobID + "\n"), nil
	}
	if len(args) >= 4 && args[0] == "artifact" && args[1] == "download" && args[2] == r.path {
		path := filepath.Join(args[3], filepath.FromSlash(r.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, r.data, 0o600)
	}
	return nil, fmt.Errorf("unexpected agent command: %v", args)
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
			Sources: []plan.NeedSource{{StepKey: stepKey, PlanDigest: planDigest}},
			Outputs: []plan.NeedOutput{{Name: "value", StepKey: stepKey, Output: "hashes"}},
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
			Sources: []plan.NeedSource{{StepKey: stepKey, PlanDigest: planDigest}},
			Outputs: []plan.NeedOutput{{Name: "value", StepKey: stepKey, Output: "hashes"}},
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
