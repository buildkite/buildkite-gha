package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/transport"
)

func schedulingSource(group bool) string {
	source := strings.Replace(continueDeferredMatrixWorkflow, "      matrix: ${{ steps.matrix.outputs.matrix }}", "      matrix: ${{ steps.matrix.outputs.matrix }}\n      group: ${{ steps.matrix.outputs.group }}\n      parallel: ${{ steps.matrix.outputs.parallel }}", 1)
	if group {
		return strings.Replace(source, "    needs: plan\n", "    needs: plan\n    concurrency: ${{ needs.plan.outputs.group }}\n", 1)
	}
	return strings.Replace(source, "    strategy:\n", "    strategy:\n      max-parallel: ${{ fromJSON(needs.plan.outputs.parallel) }}\n", 1)
}

func schedulingManifest(t *testing.T, initial continueInitialUpload, group, parallel string) []byte {
	t.Helper()
	var manifest transport.ResultManifest
	if err := json.Unmarshal(initial.producerManifest(t, "success", `[{"target":"one","runner":"ubuntu-latest"},{"target":"two","runner":"ubuntu-latest"},{"target":"three","runner":"ubuntu-latest"}]`), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Outputs = append(manifest.Outputs, transport.Output{Name: "group", Value: group}, transport.Output{Name: "parallel", Value: parallel})
	data, err := transport.MarshalResultManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestContinueSchedulingWaitsForStaticGraphAndChecksReplayAttributes(t *testing.T) {
	for _, group := range []bool{true, false} {
		t.Run(fmt.Sprint(group), func(t *testing.T) {
			initial := runContinueInitialUploads(t, schedulingSource(group))[0]
			if !initial.artifact.Continuation.Scheduling {
				t.Fatal("scheduling stage was not recorded")
			}
			_, _, initialSteps := decodeContinuePipeline(t, []byte(initial.pipeline))
			for _, step := range initialSteps {
				if step.Key != initial.artifact.Continuation.StepKey {
					continue
				}
				if len(step.DependsOn) != len(initial.artifact.Graph)+1 || step.DependsOn[0].Step != "continue-importer" || step.DependsOn[0].AllowFailure || step.Concurrency != 0 || step.ConcurrencyGroup != "" {
					t.Fatalf("coordinator must wait without holding a slot: %#v", step)
				}
				for _, job := range initial.artifact.Graph {
					found := false
					for _, dependency := range step.DependsOn {
						found = found || dependency.Step == job.Key
					}
					if !found {
						t.Fatalf("coordinator does not wait for static job %s", job.Key)
					}
				}
			}
			manifest := schedulingManifest(t, initial, "Deploy", "2")
			first := initial.continueRunner(manifest)
			if code, stdout, stderr := runContinue(t, first, initial.digest); code != 0 {
				t.Fatalf("continue = %d, %s, %s", code, stdout, stderr)
			}
			_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, first))
			if len(steps) != 4 {
				t.Fatalf("uploaded %d jobs, want three consumers and publish", len(steps))
			}
			existing := make(map[string]map[string]string)
			var consumer string
			for _, step := range steps {
				existing[step.Key] = map[string]string{"command": step.Command, "concurrency_limit": fmt.Sprint(step.Concurrency), "concurrency_key": step.ConcurrencyGroup}
				if step.Concurrency == 0 {
					continue
				}
				consumer = step.Key
				want := 2
				if group {
					want = 1
				}
				if step.Concurrency != want || step.ConcurrencyGroup == "" {
					t.Fatalf("scheduling = %#v", step)
				}
			}
			if consumer == "" {
				t.Fatal("no scheduled consumer was uploaded")
			}
			for _, changed := range []string{"", "concurrency_limit", "concurrency_key"} {
				replay := initial.continueRunner(manifest)
				replay.pipelineUploadErr = errors.New("duplicate step key")
				replay.stepAttributes = existing
				old := existing[consumer][changed]
				if changed != "" {
					existing[consumer][changed] = "different"
				}
				code, stdout, stderr := runContinue(t, replay, initial.digest)
				existing[consumer][changed] = old
				if changed == "" && (code != 0 || !strings.Contains(stdout, "already uploaded")) || changed != "" && (code != 1 || !strings.Contains(stderr, stageRetryGuidance)) {
					t.Fatalf("replay with changed %q = %d, %s, %s", changed, code, stdout, stderr)
				}
			}
			// Failure still skips the entire owned graph without evaluating outputs.
			skipped := initial.continueRunner(initial.producerManifest(t, "failure", ""))
			if code, _, stderr := runContinue(t, skipped, initial.digest); code != 0 {
				t.Fatalf("failed producer: %d, %s", code, stderr)
			}
			_, _, placeholders := decodeContinuePipeline(t, lastPipelineUpload(t, skipped))
			if len(placeholders) != 2 || placeholders[0].Skip == "" || placeholders[1].Skip == "" {
				t.Fatalf("failure placeholders = %#v", placeholders)
			}
		})
	}
}
