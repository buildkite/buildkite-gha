package cli

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"go.yaml.in/yaml/v4"
)

const continueRunsOnWorkflow = `on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      runner: ${{ steps.select.outputs.runner }}
    steps:
      - id: select
        run: echo 'runner=["ubuntu-22.04"]' >> "$GITHUB_OUTPUT"
  build:
    needs: plan
    runs-on: ${{ fromJSON(needs.plan.outputs.runner) }}
    steps: [{run: echo ready}]
  publish:
    needs: build
    runs-on: ubuntu-latest
    steps: [{run: true}]
  lint:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`

func TestContinueRunsOnUsesMappingAndPreservesReplayChecks(t *testing.T) {
	initial := runContinueInitialUploads(t, continueRunsOnWorkflow, "--runner-queue", "ubuntu-22.04=selected-queue")[0]
	continuation := initial.artifact.Continuation
	if !slices.Equal(continuation.Jobs, []string{"build", "publish"}) || !strings.HasSuffix(continuation.StepKey, "-build-runs-on") || !strings.Contains(initial.pipeline, ":github: runs-on · build") || !strings.Contains(initial.pipeline, "build (runs-on)") {
		t.Fatalf("continuation=%+v pipeline=%s", continuation, initial.pipeline)
	}
	first := initial.continueRunner(initial.producerManifest(t, "success", `["ubuntu-22.04"]`))
	if code, stdout, stderr := runContinue(t, first, initial.digest); code != 0 {
		t.Fatalf("continue=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	plans := uploadedPlans(t, first)
	if len(plans) != 2 || len(plans["build"]) != 1 || len(plans["publish"]) != 1 || plans["build"][0].Target.Queue != "selected-queue" || plans["build"][0].GitHubToken != nil {
		t.Fatalf("plans=%+v", plans)
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, first))
	if len(steps) != 2 || steps[0].Agents.Queue != "selected-queue" || steps[0].DependsOn[0].Step != initial.artifact.rootProducer().Key || steps[1].DependsOn[0].Step != steps[0].Key {
		t.Fatalf("steps=%+v", steps)
	}
	for path := range first.uploaded {
		if _, static := initial.plans[path]; static {
			t.Fatalf("re-uploaded initial plan %s", path)
		}
	}
	existing := map[string]map[string]string{}
	for _, step := range steps {
		existing[step.Key] = map[string]string{"command": step.Command}
	}
	for _, test := range []struct {
		name, output string
		want         int
	}{
		{"same output", `["ubuntu-22.04"]`, 0},
		{"changed output", `["ubuntu-24.04"]`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := initial.continueRunner(initial.producerManifest(t, "success", test.output))
			replay.pipelineUploadErr = errors.New("duplicate step key")
			replay.stepAttributes = existing
			code, stdout, stderr := runContinue(t, replay, initial.digest)
			if code != test.want || code == 0 && !strings.Contains(stdout, "already uploaded") || code == 1 && !strings.Contains(stderr, "Retry the whole build") {
				t.Fatalf("replay=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
		})
	}
	t.Run("producer retried", func(t *testing.T) {
		runner := initial.continueRunner(initial.producerManifestFrom(t, continueContinuationJobID, "success", `["ubuntu-22.04"]`))
		code, _, stderr := runContinue(t, runner, initial.digest)
		if code != 1 || pipelineUploads(runner) != 0 || !strings.Contains(stderr, "Retry the whole build") {
			t.Fatalf("producer retry=%d stderr=%s", code, stderr)
		}
	})
	for _, result := range []string{"failure", "skipped", "cancelled"} {
		t.Run(result, func(t *testing.T) {
			runner := initial.continueRunner(initial.producerManifest(t, result, ""))
			if code, stdout, stderr := runContinue(t, runner, initial.digest); code != 0 {
				t.Fatalf("skip=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			_, _, skipped := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
			if len(skipped) != 2 || skipped[0].Key != steps[0].Key || skipped[1].Key != steps[1].Key || skipped[0].Skip == "" || skipped[1].Skip == "" || len(uploadedPlans(t, runner)) != 0 {
				t.Fatalf("skipped=%+v", skipped)
			}
		})
	}
}

func TestContinueRunsOnRejectsInvalidValuesAndTargets(t *testing.T) {
	initial := runContinueInitialUploads(t, continueRunsOnWorkflow)[0]
	for _, test := range []struct{ output, want string }{
		{`["privileged-runner"]`, "compile deferred jobs"},
		{`["macos-latest"]`, "no runtime distribution configured for darwin/arm64"},
		{`{"queue":"privileged"}`, "want string or array"},
		{`["ubuntu-latest", 12]`, "want string"},
		{`["ubuntu-latest", "ubuntu-latest"]`, "duplicate"},
		{`[]`, "no runner labels"},
		{`["${{ github.token }}"]`, "compile deferred jobs"},
		{"", `did not publish output "runner"`},
	} {
		runner := initial.continueRunner(initial.producerManifest(t, "success", test.output))
		code, _, stderr := runContinue(t, runner, initial.digest)
		if code != 1 || pipelineUploads(runner) != 0 || !strings.Contains(stderr, test.want) {
			t.Errorf("output %q: code=%d stderr=%s", test.output, code, stderr)
		}
	}
}

func TestContinueRunsOnUsesLiveRunnerResolution(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected queue", true: "server rejects preset"}[reject], func(t *testing.T) {
			const warning = "Selected labels use a heuristic Ubuntu fallback."
			verdict := map[string]any{
				"target":   map[string]string{"queue": "live-selected", "platform": "linux/amd64", "image": defaultNobleRunnerImage},
				"warnings": []map[string]string{{"code": "runner_label_fallback", "message": warning}},
			}
			if reject {
				verdict = map[string]any{"error": map[string]string{"code": "missing_queue", "message": "Selected queue was removed."}}
			}
			server, requests := runnerResolutionServer(t, http.StatusOK, map[string]map[string]any{
				"ubuntu-latest": {"target": map[string]string{"queue": "linux-medium", "platform": "linux/amd64", "image": defaultNobleRunnerImage}},
				"ubuntu-22.04":  verdict,
			})
			t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
			t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
			initial := runContinueInitialUploads(t, continueRunsOnWorkflow)[0]
			initialRequests := *requests
			runner := initial.continueRunner(initial.producerManifest(t, "success", `["ubuntu-22.04"]`))
			code, _, stderr := runContinue(t, runner, initial.digest)
			if *requests != initialRequests+1 {
				t.Fatalf("runner resolution requests=%d stderr=%s", *requests, stderr)
			}
			if reject {
				if code != 1 || pipelineUploads(runner) != 0 || !strings.Contains(stderr, "Selected queue was removed.") {
					t.Fatalf("rejected target: code=%d stderr=%s", code, stderr)
				}
			} else if plans := uploadedPlans(t, runner); code != 0 || len(plans["build"]) != 1 || plans["build"][0].Target.Queue != "live-selected" {
				t.Fatalf("live target: code=%d plans=%+v stderr=%s", code, plans, stderr)
			}
			var warnings int
			for _, command := range runner.commands {
				if slices.Equal(command.args, []string{"annotate", "--scope", "job", "--job", continueContinuationJobID, "--context", runnerResolutionContext, "--style", "warning"}) && strings.Contains(string(command.stdin), "runner_label_fallback") && strings.Contains(string(command.stdin), warning) {
					warnings++
				}
			}
			if !reject && warnings != 1 || reject && warnings != 0 {
				t.Fatalf("runner fallback annotations=%d reject=%v", warnings, reject)
			}
		})
	}
}

func TestContinueRunsOnPreservesNativeAgentEnvironments(t *testing.T) {
	server, _ := runnerResolutionServer(t, http.StatusOK, map[string]map[string]any{
		"ubuntu-latest": {"target": map[string]any{"queue": "native", "platform": "linux/amd64", "agents": map[string]string{"nsc-gha-image": "ubuntu-24.04"}, "tool_cache": false}},
		"ubuntu-22.04":  {"target": map[string]any{"queue": "native", "platform": "linux/amd64", "agents": map[string]string{"nsc-gha-image": "ubuntu-22.04"}, "tool_cache": false}},
	})
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	initial := runContinueInitialUploads(t, continueRunsOnWorkflow)[0]
	var document struct {
		Steps []struct {
			Steps []struct {
				Key     string            `yaml:"key"`
				Image   string            `yaml:"image"`
				Agents  map[string]string `yaml:"agents"`
				Command string            `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal([]byte(initial.pipeline), &document); err != nil {
		t.Fatal(err)
	}
	stageFound := false
	for _, step := range document.Steps[0].Steps {
		if step.Key == initial.artifact.Continuation.StepKey {
			stageFound = true
			if step.Image != "" || step.Agents["nsc-gha-image"] != "ubuntu-24.04" || step.Agents["queue"] != "native" {
				t.Fatalf("continuation host = %#v", step)
			}
		}
	}
	if !stageFound {
		t.Fatal("missing stage upload step")
	}
	runner := initial.continueRunner(initial.producerManifest(t, "success", `["ubuntu-22.04"]`))
	code, _, stderr := runContinue(t, runner, initial.digest)
	if code != 0 {
		t.Fatalf("continue: %d %s", code, stderr)
	}
	if err := yaml.Unmarshal(lastPipelineUpload(t, runner), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 || len(document.Steps[0].Steps) != 2 {
		t.Fatalf("expanded steps = %#v", document.Steps)
	}
	for _, step := range document.Steps[0].Steps {
		want := "ubuntu-24.04"
		if strings.HasSuffix(step.Key, "-build") {
			want = "ubuntu-22.04"
		}
		if step.Image != "" || step.Agents["nsc-gha-image"] != want || step.Agents["queue"] != "native" || strings.Contains(step.Command, "--hosted-tool-cache") {
			t.Fatalf("expanded host = %#v, want %s", step, want)
		}
	}
}

func TestContinueRunnerAndMatrixRemainIndependent(t *testing.T) {
	source := strings.Replace(continueTwoMatricesWorkflow, `runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan-b.outputs.matrix) }}`, `runs-on: ${{ needs.plan-b.outputs.matrix }}`, 1)
	for _, test := range []struct {
		name, source string
		scheduling   bool
	}{
		{"matrix", source, false},
		{"matrix concurrency", strings.Replace(source, "    needs: plan-a\n", "    needs: plan-a\n    concurrency: ${{ needs.plan-a.outputs.matrix && 'deploy' }}\n", 1), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			initial := runContinueInitialUploads(t, test.source)
			if len(initial) != 2 || initial[1].artifact.Continuation.Descriptor.Shape != compiler.RuntimeRunsOnShape || initial[0].artifact.Continuation.Scheduling != test.scheduling {
				t.Fatalf("continuations=%+v", initial)
			}
			for i, output := range []string{`[{"target":"x"},{"target":"y"}]`, "ubuntu-22.04"} {
				runner := initial[i].continueRunner(initial[i].producerManifest(t, "success", output))
				code, stdout, stderr := runContinue(t, runner, initial[i].digest)
				if code != 0 || pipelineUploads(runner) != 1 {
					t.Fatalf("continue %d=%d stdout=%s stderr=%s", i, code, stdout, stderr)
				}
				plans := uploadedPlans(t, runner)
				if i == 0 && (len(plans) != 1 || len(plans["build-a"]) != 2) || i == 1 && (len(plans) != 2 || len(plans["build-b"]) != 1 || len(plans["publish-b"]) != 1) {
					t.Fatalf("wrong continuation owns plans: %+v", plans)
				}
				_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
				for _, step := range steps {
					if test.scheduling && i == 0 {
						if step.Concurrency != 1 || step.ConcurrencyGroup == "" {
							t.Fatalf("missing matrix concurrency: %+v", step)
						}
					} else if step.Concurrency != 0 || step.ConcurrencyGroup != "" {
						t.Fatalf("matrix concurrency leaked to another component: %+v", step)
					}
				}
			}
		})
	}
}
