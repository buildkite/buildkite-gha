package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const continueEnvironmentWorkflow = `on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      environment: ${{ steps.choose.outputs.environment }}
    steps:
      - id: choose
        run: echo 'environment=staging-eu' >> "$GITHUB_OUTPUT"
  deploy:
    needs: plan
    runs-on: ubuntu-latest
    environment:
      name: ${{ needs.plan.outputs.environment }}
      url: ${{ steps.deploy.outputs.url }}
    steps:
      - id: deploy
        run: echo "$KEY" "$REGION"
        env:
          KEY: ${{ secrets.DEPLOY_KEY }}
          REGION: ${{ vars.REGION }}
  finish:
    needs: deploy
    runs-on: ubuntu-latest
    steps: [{run: true}]
`

// environmentContinuationAgent exposes only snapshots, never GitHub tokens.
// Returning 404 for runner resolution keeps the explicit test runner mapping.
func environmentContinuationAgent(t *testing.T, status int, change func(map[string]any)) *atomic.Int32 {
	t.Helper()
	requests := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/github-actions/environments") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		if r.Header.Get("Authorization") != "Token job-secret" {
			t.Error("environment request lacks job-scoped authentication")
		}
		if !strings.HasPrefix(r.URL.Path, "/jobs/"+continueContinuationJobID+"/") && !strings.HasPrefix(r.URL.Path, "/jobs/"+continueImporterJobID+"/") {
			t.Errorf("unexpected environment caller %s", r.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		var request struct {
			Repository string   `json:"repo_url"`
			Names      []string `json:"environment_names"`
			Variables  bool     `json:"include_variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Repository != "https://github.com/buildkite/buildkite-gha" || !request.Variables || len(request.Names) == 0 {
			t.Errorf("snapshot request = %#v", request)
		}
		var snapshots []map[string]any
		for _, name := range request.Names {
			snapshot := map[string]any{
				"name": name, "required_reviewers": false, "prevent_self_review": false,
				"wait_timer_minutes": 0, "branch_policy": false, "unsupported_rules": []string{},
				"secret_names": []string{"DEPLOY_KEY"},
				"variables":    []map[string]string{{"name": "REGION", "value": "eu-west-1-private-configuration"}},
			}
			if change != nil {
				change(snapshot)
			}
			snapshots = append(snapshots, snapshot)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"environments": snapshots})
	}))
	t.Cleanup(server.Close)
	setAgentResolutionEnvironment(t, server.URL)
	return requests
}

func TestContinueSelectsUnprotectedEnvironment(t *testing.T) {
	requests := environmentContinuationAgent(t, http.StatusOK, nil)
	initial := runContinueInitialUploads(t, continueEnvironmentWorkflow, "--runner-queue", "ubuntu-latest=custom-linux")[0]
	prefix := strings.TrimSuffix(initial.artifact.rootProducer().Key, "plan")
	if requests.Load() != 0 || strings.Contains(initial.pipeline, `key: "`+prefix+`deploy"`) || !strings.Contains(initial.pipeline, ":github: environment · deploy") || !strings.Contains(initial.pipeline, "deploy (environment)") {
		t.Fatalf("premature resolution or incorrect initial scheduling: requests=%d\n%s", requests.Load(), initial.pipeline)
	}
	runner := initial.continueRunner(initial.producerManifest(t, "success", "staging-eu"))
	code, stdout, stderr := runContinue(t, runner, initial.digest)
	if code != 0 || requests.Load() != 1 || pipelineUploads(runner) != 1 {
		t.Fatalf("continue=%d requests=%d stdout=%s stderr=%s", code, requests.Load(), stdout, stderr)
	}
	plans := uploadedPlans(t, runner)
	if len(plans) != 2 || len(plans["deploy"]) != 1 || len(plans["finish"]) != 1 {
		t.Fatalf("wrong deferred plans: %v", maps.Keys(plans))
	}
	deploy := plans["deploy"][0]
	if !maps.Equal(deploy.SecretMappings, map[string]string{"DEPLOY_KEY": "STAGING_EU_DEPLOY_KEY"}) || !maps.Equal(deploy.EnvironmentVars, map[string]string{"REGION": "eu-west-1-private-configuration"}) || deploy.Target.Queue != "custom-linux" {
		t.Fatalf("wrong scope or queue: %#v", deploy)
	}
	pipeline := lastPipelineUpload(t, runner)
	if strings.Contains(string(pipeline)+stdout+stderr, "eu-west-1-private-configuration") || strings.Contains(string(pipeline), "block:") {
		t.Fatal("configuration leaked or an unprotected environment added a block")
	}
	_, _, steps := decodeContinuePipeline(t, pipeline)
	if len(steps) != 2 || steps[0].Key != prefix+"deploy" || len(steps[0].DependsOn) != 1 || steps[0].DependsOn[0].Step != prefix+"plan" || steps[1].DependsOn[0].Step != prefix+"deploy" {
		t.Fatalf("wrong deferred dependencies: %#v", steps)
	}
}

func TestDynamicEnvironmentAlongsideChainedMatrixStages(t *testing.T) {
	requests := environmentContinuationAgent(t, http.StatusOK, nil)
	workflow := continueChainedWorkflow + `  plan_env:
    runs-on: ubuntu-latest
    outputs:
      environment: ${{ steps.choose.outputs.environment }}
    steps:
      - id: choose
        run: echo 'environment=staging-eu' >> "$GITHUB_OUTPUT"
  env_deploy:
    needs: plan_env
    runs-on: ubuntu-latest
    environment: ${{ needs.plan_env.outputs.environment }}
    steps:
      - run: test -n "$KEY"
        env:
          KEY: ${{ secrets.DEPLOY_KEY }}
`
	initials := runContinueInitialUploads(t, workflow, "--runner-queue", "ubuntu-latest=custom-linux")
	if len(initials) != 2 || initials[0].artifact.Continuation.Descriptor.Job != "build" || initials[1].artifact.Continuation.Descriptor.Job != "env_deploy" {
		t.Fatalf("expected independent matrix and environment stages, got %d", len(initials))
	}
	first := firstStage(initials[0])
	runner, code, _, stderr := first.run(t, "success", `[{"target":"linux","runner":"ubuntu-latest"}]`, continueProducerJobID, nil)
	if code != 0 {
		t.Fatal(stderr)
	}
	second := first.nextStage(t, runner)
	if second.initial.artifact.Continuation.JobBudget+3 != first.initial.artifact.Continuation.JobBudget {
		t.Fatal("matrix stage did not preserve the component budget")
	}
	runner, code, _, stderr = second.run(t, "success", `[{"runner":"ubuntu-latest"},{"runner":"ubuntu-latest","target":"canary"}]`, continuePackageJobID, nil)
	if code != 0 {
		t.Fatal(stderr)
	}
	plans := uploadedPlans(t, runner)
	if len(plans) != 2 || len(plans["deploy"]) != 2 || len(plans["release"]) != 1 || requests.Load() != 0 {
		t.Fatalf("matrix chain/join changed or resolved the independent environment: plans=%v requests=%d", maps.Keys(plans), requests.Load())
	}
	environment := initials[1]
	runner = environment.continueRunner(environment.producerManifest(t, "success", "staging-eu"))
	if code, _, stderr := runContinue(t, runner, environment.digest); code != 0 {
		t.Fatal(stderr)
	}
	plans = uploadedPlans(t, runner)
	if len(plans) != 1 || len(plans["env_deploy"]) != 1 || plans["env_deploy"][0].SecretMappings["DEPLOY_KEY"] != "STAGING_EU_DEPLOY_KEY" || requests.Load() != 1 {
		t.Fatalf("environment stage changed matrix ownership or lost its scope: plans=%v requests=%d", maps.Keys(plans), requests.Load())
	}
}

func TestContinueEnvironmentFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		change func(map[string]any)
		want   string
	}{
		{"reviewers", 200, func(s map[string]any) { s["required_reviewers"] = true }, "protected dynamic environment"},
		{"self review", 200, func(s map[string]any) { s["prevent_self_review"] = true }, "protected dynamic environment"},
		{"timer", 200, func(s map[string]any) { s["wait_timer_minutes"] = 1 }, "protected dynamic environment"},
		{"branch", 200, func(s map[string]any) { s["branch_policy"] = true }, "protected dynamic environment"},
		{"custom", 200, func(s map[string]any) { s["unsupported_rules"] = []string{"custom"} }, "protected dynamic environment"},
		{"missing reviewers", 200, func(s map[string]any) { delete(s, "required_reviewers") }, "omits required_reviewers"},
		{"missing variables", 200, func(s map[string]any) { delete(s, "variables") }, "omits variables"},
		{"missing secrets", 200, func(s map[string]any) { delete(s, "secret_names") }, "omits secret_names"},
		{"unavailable", 404, nil, "does not offer GitHub environment resolution"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environmentContinuationAgent(t, test.status, test.change)
			initial := runContinueInitialUploads(t, continueEnvironmentWorkflow, "--runner-queue", "ubuntu-latest=custom-linux")[0]
			runner := initial.continueRunner(initial.producerManifest(t, "success", "production"))
			code, _, stderr := runContinue(t, runner, initial.digest)
			if code != 1 || !strings.Contains(stderr, test.want) || pipelineUploads(runner) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("continue=%d stderr=%s uploads=%d artifacts=%d", code, stderr, pipelineUploads(runner), len(runner.uploaded))
			}
		})
	}
}

func TestContinueEnvironmentProducerAndCrossWorkflowBoundaries(t *testing.T) {
	eventPath := pushEventPath(t)
	requests := environmentContinuationAgent(t, http.StatusOK, nil)
	workflows := writeCommittedUploadWorkflows(t, map[string]string{
		"deploy.yml": continueEnvironmentWorkflow,
		"plain.yml":  "on: push\njobs:\n  other:\n    runs-on: ubuntu-latest\n    environment: staging-eu\n    steps: [{run: true}]\n",
	})
	initial := runContinueInitialUploadsInCheckout(t, workflows, eventPath, "--runner-queue", "ubuntu-latest=custom-linux")[0]
	if len(initial.artifact.KnownEnvironmentNames) != 1 || initial.artifact.KnownEnvironmentNames[0] != "staging-eu" {
		t.Fatalf("other workflow's name not recorded: %v", initial.artifact.KnownEnvironmentNames)
	}
	for _, test := range []struct{ name, output, result, want string }{
		{"collision", "staging/eu", "success", "secret prefix"},
		{"missing", "", "success", "did not publish output"},
		{"empty name", " ", "success", "dynamic environment name must be"},
		{"control", "prod\n", "success", "dynamic environment name must be"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := initial.continueRunner(initial.producerManifest(t, test.result, test.output))
			code, _, stderr := runContinue(t, runner, initial.digest)
			if code != 1 || !strings.Contains(stderr, test.want) || pipelineUploads(runner) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("continue=%d stderr=%s", code, stderr)
			}
		})
	}
	before := requests.Load()
	runner := initial.continueRunner(initial.producerManifest(t, "failure", "production"))
	code, _, stderr := runContinue(t, runner, initial.digest)
	if code != 0 || requests.Load() != before || len(uploadedPlans(t, runner)) != 0 {
		t.Fatalf("failed producer resolved an environment or emitted plans: code=%d stderr=%s", code, stderr)
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
	if len(steps) != 2 || steps[0].Skip == "" || steps[1].Skip == "" {
		t.Fatalf("failed producer did not skip all deferred jobs: %#v", steps)
	}
}

func TestUploadRejectsTwoDynamicEnvironments(t *testing.T) {
	eventPath := pushEventPath(t)
	workflows := writeCommittedUploadWorkflows(t, map[string]string{"build.yml": continueEnvironmentWorkflow, "deploy.yml": continueEnvironmentWorkflow})
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_JOB_ID", continueImporterJobID)
	t.Setenv("BUILDKITE_STEP_KEY", "continue-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	args := append([]string{"upload", "--event-path", eventPath, "--runner-queue", "ubuntu-latest=custom-linux"}, workflows...)
	code := run(args, &stdout, &stderr, "dev", runner)
	if code == 0 || !strings.Contains(stderr.String(), "only one dynamic environment name") || pipelineUploads(runner) != 0 {
		t.Fatalf("upload=%d stderr=%s", code, stderr.String())
	}
}

func TestContinueEnvironmentReplayAndRetriedProducer(t *testing.T) {
	requests := environmentContinuationAgent(t, http.StatusOK, nil)
	initial := runContinueInitialUploads(t, continueEnvironmentWorkflow, "--runner-queue", "ubuntu-latest=custom-linux")[0]
	first := initial.continueRunner(initial.producerManifest(t, "success", "staging-eu"))
	if code, _, stderr := runContinue(t, first, initial.digest); code != 0 {
		t.Fatal(stderr)
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, first))
	existing := map[string]map[string]string{}
	for _, step := range steps {
		existing[step.Key] = map[string]string{"command": step.Command}
	}
	for _, test := range []struct {
		name, environment string
		want              int
	}{
		{"same plan", "staging-eu", 0},
		{"changed secret scope", "staging-us", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := initial.continueRunner(initial.producerManifest(t, "success", test.environment))
			replay.pipelineUploadErr = errors.New("duplicate step key")
			replay.stepAttributes = existing
			if code, stdout, stderr := runContinue(t, replay, initial.digest); code != test.want {
				t.Fatalf("replay=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
		})
	}
	before := requests.Load()
	retried := initial.continueRunner(initial.producerManifestFrom(t, "0192f7d0-0000-4000-8000-00000000bbb2", "success", "staging-eu"))
	if code, _, stderr := runContinue(t, retried, initial.digest); code != 1 || requests.Load() != before || pipelineUploads(retried) != 0 {
		t.Fatalf("retried producer escaped verification: code=%d stderr=%s", code, stderr)
	}
}
