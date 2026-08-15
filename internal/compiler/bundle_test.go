package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"go.yaml.in/yaml/v4"
)

const testDistributionDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestCompileBundleGoldenAndDeterministic(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	source := readFile(t, path)
	event := readFile(t, smokePath("events", "push.json"))
	first, err := CompileBundle(path, source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileBundle(path, source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated bundle compilation was not byte-identical")
	}
	if len(first.Plans) != 3 || len(first.IR.Jobs) != 3 {
		t.Fatalf("bundle jobs/plans = %d/%d, want 3/3", len(first.IR.Jobs), len(first.Plans))
	}
	producer := first.IR.Jobs[0].Key
	for i, artifact := range first.Plans {
		if artifact.Path != ".buildkite-gha/plans/"+strings.TrimPrefix(artifact.Digest, "sha256:")+".json" {
			t.Fatalf("plan %d path = %q", i, artifact.Path)
		}
		if i == 0 && len(artifact.Job.Dependencies) != 0 {
			t.Fatalf("producer dependencies = %#v", artifact.Job.Dependencies)
		}
		if i > 0 && !reflect.DeepEqual(artifact.Job.Dependencies, []string{producer}) {
			t.Fatalf("consumer dependencies = %#v, want %q", artifact.Job.Dependencies, producer)
		}
	}
	wantPipeline := readFile(t, filepath.Join("testdata", "shell.pipeline.golden.yml"))
	if !bytes.Equal(first.Pipeline, wantPipeline) {
		t.Fatalf("pipeline changed\nwant:\n%s\ngot:\n%s", wantPipeline, first.Pipeline)
	}
	wantPlans := readFile(t, filepath.Join("testdata", "shell.plans.golden.json"))
	if !bytes.Equal(encodeGoldenPlans(t, first.Plans), wantPlans) {
		t.Fatalf("plans changed; update shell.plans.golden.json intentionally")
	}
}

func TestCompileBundleDoesNotActivateValidatedRuntimeMatrixWithoutFencing(t *testing.T) {
	source := []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.matrix.outputs.include }}
    steps:
      - id: matrix
        run: echo 'include=[]' >> "$GITHUB_OUTPUT"
  generated:
    needs: producer
    runs-on: ${{ matrix.runs-on }}
    permissions:
      contents: write
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    steps:
      - run: echo "${{ matrix.steps }}"
`)
	bundle, err := CompileBundle("runtime-matrix.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), "continuation upload is disabled") {
		t.Fatalf("CompileBundle() error = %v", err)
	}
	if len(bundle.Plans) != 0 || len(bundle.Pipeline) != 0 {
		t.Fatalf("unsafe runtime matrix produced %d plans and %d pipeline bytes", len(bundle.Plans), len(bundle.Pipeline))
	}
	if len(bundle.IR.Jobs) != 1 || bundle.IR.Jobs[0].LogicalJobID != "producer" {
		t.Fatalf("partial safe IR jobs = %#v", bundle.IR.Jobs)
	}
}

func TestBundlePlansPermitAdmissionBeforePipelineGeneration(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	options := defaultOptions()
	bundle, err := CompileBundlePlansContext(context.Background(), path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 3 || len(bundle.Pipeline) != 0 {
		t.Fatalf("plan-stage bundle has %d plans and %d pipeline bytes", len(bundle.Plans), len(bundle.Pipeline))
	}
	options.RuntimeImage = "mutable:latest"
	failed, err := GenerateBundlePipeline(bundle, testDistributionDigest, "gha-importer", options)
	if err == nil || len(failed.Pipeline) != 0 || len(failed.Plans) != 3 {
		t.Fatalf("pipeline generation = %d bytes, %d plans, %v", len(failed.Pipeline), len(failed.Plans), err)
	}
}

func TestCompileBundlePreservesExplicitQueuePolicy(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	bundle, err := CompileBundleWithOptions("workflow.yml", workflow, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer", Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "customer-untrusted"},
			UntrustedQueues: []string{"customer-untrusted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 1 || bundle.Plans[0].Job.Target.Queue != "customer-untrusted" || bundle.Plans[0].Job.Schema != plan.Schema {
		t.Fatalf("explicitly targeted plan = %#v", bundle.Plans)
	}
	if !bytes.Contains(bundle.Pipeline, []byte("agents:\n      queue: \"customer-untrusted\"")) {
		t.Fatalf("explicit queue absent from pipeline:\n%s", bundle.Pipeline)
	}
}

func TestCompileBundleCarriesLiteralJobContinueOnError(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  report:
    runs-on: ubuntu-latest
    continue-on-error: true
    outputs:
      diagnostic: ${{ steps.report.outputs.diagnostic }}
    steps:
      - id: report
        run: echo "diagnostic=failed" >> "$GITHUB_OUTPUT"; exit 1
  consume:
    needs: report
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ needs.report.result }}:${{ needs.report.outputs.diagnostic }}" = success:failed
`)
	bundle, err := CompileBundle("workflow.yml", workflow, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 2 || bundle.Plans[0].Job.Schema != plan.Schema || !bundle.Plans[0].Job.ContinueOnError {
		t.Fatalf("compiled plans = %#v, want tolerated producer", bundle.Plans)
	}
	var pipeline struct {
		Steps []struct {
			Key      string `yaml:"key"`
			SoftFail []struct {
				ExitStatus int `yaml:"exit_status"`
			} `yaml:"soft_fail"`
			DependsOn []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(bundle.Pipeline, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 2 || pipeline.Steps[0].Key != "gha-report" || len(pipeline.Steps[0].SoftFail) != 1 || pipeline.Steps[0].SoftFail[0].ExitStatus != buildkitepipeline.ContinueOnErrorExitStatus {
		t.Fatalf("pipeline = %s, want soft-failing report", bundle.Pipeline)
	}
	dependencies := pipeline.Steps[1].DependsOn
	if len(dependencies) != 2 || dependencies[1].Step != "gha-report" || !dependencies[1].AllowFailure {
		t.Fatalf("consumer dependencies = %#v, want failure-tolerant report dependency", dependencies)
	}
}

func TestCompileBundlePreservesRuntimeImagePolicy(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	options := defaultOptions()
	options.RuntimeImage = image
	bundle, err := CompileBundleWithOptions("workflow.yml", workflow, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer", options)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Image   string `yaml:"image"`
			Command string `yaml:"command"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(bundle.Pipeline, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 || document.Steps[0].Image != image || !strings.Contains(document.Steps[0].Command, "--hosted-tool-cache") {
		t.Fatalf("runtime image pipeline = %#v", document.Steps)
	}
}

func TestCompileBundleEmitsMixedLinuxAndDarwinRuntimes(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  linux:
    runs-on: ubuntu-24.04
    steps:
      - run: echo linux
  macos:
    needs: linux
    runs-on: macos-15
    steps:
      - run: echo macos
`)
	linuxDigest := "sha256:" + strings.Repeat("a", 64)
	darwinDigest := "sha256:" + strings.Repeat("b", 64)
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("c", 64)
	bundle, err := CompileBundleWithOptions("mixed.yml", workflow, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer", Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Targets: map[string]RunnerTarget{
				"ubuntu-24.04": {Queue: "linux", Platform: PlatformLinuxAMD64, Image: image},
				"macos-15":     {Queue: "macos", Platform: PlatformDarwinARM64},
			},
			UntrustedQueues: []string{"linux", "macos"},
		},
		RuntimeDistributions: map[Platform]string{
			PlatformLinuxAMD64:  linuxDigest,
			PlatformDarwinARM64: darwinDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(bundle.Plans))
	}
	plans := map[string]PlanArtifact{}
	for _, artifact := range bundle.Plans {
		plans[artifact.Job.Workflow.LogicalJobID] = artifact
		if artifact.Job.Schema != plan.Schema || artifact.Job.Compiler.DistributionDigest != testDistributionDigest {
			t.Fatalf("plan identity = schema %q compiler %q", artifact.Job.Schema, artifact.Job.Compiler.DistributionDigest)
		}
		if bytes.Contains(artifact.Contents, []byte(`"platform"`)) || bytes.Contains(artifact.Contents, []byte("darwin/arm64")) {
			t.Fatalf("plan exposed internal platform selection:\n%s", artifact.Contents)
		}
	}
	if got := plans["linux"].Job.RuntimeDistributionDigest(); got != linuxDigest {
		t.Fatalf("Linux runtime digest = %q, want %q", got, linuxDigest)
	}
	if got := plans["macos"].Job.RuntimeDistributionDigest(); got != darwinDigest {
		t.Fatalf("Darwin runtime digest = %q, want %q", got, darwinDigest)
	}

	var document struct {
		Steps []struct {
			Key     string            `yaml:"key"`
			Image   string            `yaml:"image"`
			Command string            `yaml:"command"`
			Agents  map[string]string `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(bundle.Pipeline, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 2 {
		t.Fatalf("pipeline steps = %#v", document.Steps)
	}
	steps := map[string]struct {
		Key     string
		Image   string
		Command string
		Agents  map[string]string
	}{}
	for _, step := range document.Steps {
		steps[step.Key] = struct {
			Key     string
			Image   string
			Command string
			Agents  map[string]string
		}{step.Key, step.Image, step.Command, step.Agents}
	}
	linux := steps["gha-linux"]
	macos := steps["gha-macos"]
	if linux.Agents["queue"] != "linux" || linux.Image != image || !strings.Contains(linux.Command, strings.TrimPrefix(linuxDigest, "sha256:")) || !strings.Contains(linux.Command, "--hosted-tool-cache") {
		t.Fatalf("Linux step = %#v", linux)
	}
	if macos.Agents["queue"] != "macos" || macos.Image != "" || !strings.Contains(macos.Command, strings.TrimPrefix(darwinDigest, "sha256:")) || strings.Contains(macos.Command, "--hosted-tool-cache") || strings.Contains(macos.Command, "/opt/hostedtoolcache") {
		t.Fatalf("macOS step = %#v", macos)
	}
}

func TestCompileBundleRejectsDockerOnDarwin(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "docker.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	actionRoot := filepath.Join(workspace, ".github", "actions")
	writeAction(t, actionRoot, "docker", "runs:\n  using: docker\n  image: Dockerfile\n")
	writeAction(t, actionRoot, "composite", "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/docker\n")
	options := Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Targets: map[string]RunnerTarget{
				"macos-15":      {Queue: "macos", Platform: PlatformDarwinARM64},
				"ubuntu-latest": {Queue: "linux", Platform: PlatformLinuxAMD64},
			},
			UntrustedQueues: []string{"linux", "macos"},
		},
		RuntimeDistributions: map[Platform]string{
			PlatformLinuxAMD64:  "sha256:" + strings.Repeat("a", 64),
			PlatformDarwinARM64: "sha256:" + strings.Repeat("b", 64),
		},
	}
	tests := []struct {
		name     string
		runsOn   string
		workload string
		reject   bool
	}{
		{name: "job container", runsOn: "macos-15", workload: "    container: node:24\n    steps:\n      - run: true\n", reject: true},
		{name: "service container", runsOn: "macos-15", workload: "    services:\n      redis:\n        image: redis:7\n    steps:\n      - run: true\n", reject: true},
		{name: "Dockerfile action", runsOn: "macos-15", workload: "    steps:\n      - uses: ./.github/actions/docker\n", reject: true},
		{name: "transitive Dockerfile action", runsOn: "macos-15", workload: "    steps:\n      - uses: ./.github/actions/composite\n", reject: true},
		{name: "Linux Dockerfile action", runsOn: "ubuntu-latest", workload: "    steps:\n      - uses: ./.github/actions/docker\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workflow := []byte("on: push\njobs:\n  test:\n    runs-on: " + test.runsOn + "\n" + test.workload)
			bundle, err := CompileBundleWithOptions(workflowPath, workflow, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer", options)
			if test.reject {
				if err == nil || !strings.Contains(err.Error(), `requires Docker, which is unavailable on darwin/arm64`) {
					t.Fatalf("CompileBundleWithOptions() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Plans) != 1 || !slices.Contains(bundle.Plans[0].Job.RequiredCapabilities, "docker") {
				t.Fatalf("Linux Docker plan = %#v", bundle.Plans)
			}
		})
	}
}

func TestCompileBundleNamespacesTargetsAndDependencies(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	options := defaultOptions()
	options.StepKeyNamespace = "0123456789abcdef"
	bundle, err := CompileBundleWithOptions(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer", options)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.IR.Jobs) != 3 || len(bundle.Plans) != 3 || len(bundle.GeneratedWorkflow.Jobs) != 3 {
		t.Fatalf("namespaced bundle size = IR %d, plans %d, jobs %d", len(bundle.IR.Jobs), len(bundle.Plans), len(bundle.GeneratedWorkflow.Jobs))
	}
	producer := "gha-0123456789abcdef-producer"
	if bundle.IR.Jobs[0].Key != producer || bundle.Plans[0].Job.Target.StepKey != producer || bundle.GeneratedWorkflow.Jobs[0].Key != producer {
		t.Fatalf("namespaced producer = IR %q, plan %q, pipeline %q", bundle.IR.Jobs[0].Key, bundle.Plans[0].Job.Target.StepKey, bundle.GeneratedWorkflow.Jobs[0].Key)
	}
	for i := 1; i < len(bundle.Plans); i++ {
		if !strings.HasPrefix(bundle.Plans[i].Job.Target.StepKey, "gha-0123456789abcdef-consumer-") || !reflect.DeepEqual(bundle.Plans[i].Job.Dependencies, []string{producer}) || !reflect.DeepEqual(bundle.GeneratedWorkflow.Jobs[i].Dependencies, []string{producer}) {
			t.Fatalf("namespaced consumer %d = target %q, plan needs %#v, pipeline needs %#v", i, bundle.Plans[i].Job.Target.StepKey, bundle.Plans[i].Job.Dependencies, bundle.GeneratedWorkflow.Jobs[i].Dependencies)
		}
	}
}

func TestCompileBundleNamespacesMaxParallelConcurrency(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    strategy:\n      max-parallel: 1\n      matrix:\n        target: [one, two]\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
	options := defaultOptions()
	options.StepKeyNamespace = "0123456789abcdef"
	bundle, err := CompileBundleWithOptions("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer", options)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range bundle.GeneratedWorkflow.Jobs {
		if !strings.Contains(job.ConcurrencyGroup, "/0123456789abcdef/test") {
			t.Fatalf("matrix concurrency group = %q, want workflow namespace", job.ConcurrencyGroup)
		}
	}
}

func TestCompileBundleUsesJobIDsForUniqueCheckLabels(t *testing.T) {
	source := []byte("on: push\njobs:\n  alpha:\n    name: Test\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n  beta:\n    name: Test\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n  matrix:\n    name: Test\n    strategy:\n      matrix:\n        shard: [one, two, 1, '1']\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
	bundle, err := CompileBundleWithOptions("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer", defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(bundle.GeneratedWorkflow.Jobs))
	for i, job := range bundle.GeneratedWorkflow.Jobs {
		got[i] = job.CheckLabel
	}
	want := []string{"alpha", "beta", `matrix (shard="one")`, `matrix (shard="two")`, "matrix (shard=1)", `matrix (shard="1")`}
	if !slices.Equal(got, want) {
		t.Fatalf("provider check labels = %#v, want %#v", got, want)
	}
}

func TestCompileBundleCompilesSmokeCorpus(t *testing.T) {
	workflows, err := filepath.Glob(smokePath(".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) != 10 {
		t.Fatalf("smoke workflows = %d, want 10", len(workflows))
	}
	event := readFile(t, smokePath("events", "push.json"))
	for _, path := range workflows {
		t.Run(filepath.Base(path), func(t *testing.T) {
			bundle, err := CompileBundle(path, readFile(t, path), event, "0.0.0-test", testDistributionDigest, "gha-importer")
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Plans) == 0 || len(bundle.Pipeline) == 0 {
				t.Fatalf("empty bundle: %#v", bundle)
			}
			var document map[string]any
			if err := yaml.Unmarshal(bundle.Pipeline, &document); err != nil {
				t.Fatalf("pipeline YAML: %v", err)
			}
		})
	}
}

func TestCompileBundleCarriesResolvedMiseRequirementIntoPipeline(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, workspace, "docker", "runs:\n  using: docker\n  image: Dockerfile\n")
	writeAction(t, workspace, "javascript", "runs:\n  using: node24\n  main: index.js\n")
	workflow := []byte("on: push\njobs:\n  native:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./docker\n  javascript:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./javascript\n")
	bundle, err := CompileBundle(workflowPath, workflow, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "importer")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(bundle.Plans))
	}
	decisions := map[string]bool{}
	for _, artifact := range bundle.Plans {
		if artifact.Job.RequiresMise == nil {
			t.Fatalf("job %q lacks requires_mise", artifact.Job.Target.StepKey)
		}
		decisions[artifact.Job.Workflow.LogicalJobID] = *artifact.Job.RequiresMise
	}
	if decisions["native"] || !decisions["javascript"] {
		t.Fatalf("requires_mise decisions = %#v", decisions)
	}
	var document struct {
		Steps []struct {
			Key   string            `yaml:"key"`
			Cache any               `yaml:"cache"`
			Env   map[string]string `yaml:"env"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(bundle.Pipeline, &document); err != nil {
		t.Fatal(err)
	}
	for _, step := range document.Steps {
		wantMise := strings.Contains(step.Key, "javascript")
		if hasCache := step.Cache != nil; hasCache != wantMise {
			t.Fatalf("step %q managed cache = %t, want %t", step.Key, hasCache, wantMise)
		}
		if hasEnv := step.Env["BUILDKITE_GHA_MISE_DATA_DIR"] != ""; hasEnv != wantMise {
			t.Fatalf("step %q managed mise env = %t, want %t", step.Key, hasEnv, wantMise)
		}
	}
}

func TestCompileBundleDoesNotExposeEventValues(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	var event map[string]any
	if err := json.Unmarshal(readFile(t, smokePath("events", "push.json")), &event); err != nil {
		t.Fatal(err)
	}
	event["payload"].(map[string]any)["private_fixture_value"] = "super-secret-value"
	eventSource, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := CompileBundle(path, readFile(t, path), eventSource, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bundle.Pipeline, []byte("super-secret-value")) {
		t.Fatal("pipeline contains an event payload value")
	}
	for _, artifact := range bundle.Plans {
		if bytes.Contains(artifact.Contents, []byte("super-secret-value")) {
			t.Fatalf("plan %q contains an event payload value", artifact.Job.Target.StepKey)
		}
	}
}

func TestCompileBundleRetainsGitHubHeadRefWithoutPayload(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	var event map[string]any
	if err := json.Unmarshal(readFile(t, smokePath("events", "push.json")), &event); err != nil {
		t.Fatal(err)
	}
	event["event"] = "pull_request"
	event["payload"] = map[string]any{
		"private_fixture_value": "super-secret-value",
		"pull_request":          map[string]any{"head": map[string]any{"ref": "feature/plan"}},
	}
	eventSource, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := CompileBundle(path, readFile(t, path), eventSource, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range bundle.Plans {
		if artifact.Job.Event.HeadRef != "feature/plan" {
			t.Fatalf("plan %q head ref = %q", artifact.Job.Target.StepKey, artifact.Job.Event.HeadRef)
		}
		if bytes.Contains(artifact.Contents, []byte("super-secret-value")) {
			t.Fatalf("plan %q contains an unrelated event payload value", artifact.Job.Target.StepKey)
		}
	}
}

func TestCompileBundleTranslatesWorkflowAndJobConcurrency(t *testing.T) {
	source := []byte(`name: Deployment
on: push
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
jobs:
  static:
    strategy:
      max-parallel: 2
      matrix:
        target: [one, two]
    runs-on: ubuntu-latest
    concurrency: Deploy
    steps: [{run: true}]
  dynamic:
    strategy:
      matrix:
        target: [alpha, beta]
    runs-on: ubuntu-latest
    concurrency: deploy-${{ matrix.target }}
    steps: [{run: true}]
`)
	bundle, err := CompileBundle("concurrency.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.IR.Workflow.ConcurrencyGroup != "Deployment-refs/heads/main" || len(bundle.IR.Jobs) != 4 {
		t.Fatalf("concurrency IR = workflow %q jobs %#v", bundle.IR.Workflow.ConcurrencyGroup, bundle.IR.Jobs)
	}
	if len(bundle.IR.Warnings) != 1 || bundle.IR.Warnings[0].Code != "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED" || bundle.IR.Warnings[0].Line != 5 || bundle.IR.Warnings[0].Column != 23 {
		t.Fatalf("concurrency warnings = %#v", bundle.IR.Warnings)
	}
	repository := bundle.IR.Event.Repository
	wantJobGroups := make(map[string]string, len(bundle.IR.Jobs))
	for _, job := range bundle.IR.Jobs {
		want := "Deploy"
		if job.LogicalJobID == "dynamic" {
			want = "deploy-" + job.Matrix["target"].(string)
		}
		if job.ConcurrencyGroup != want {
			t.Fatalf("job %q concurrency group = %q, want %q", job.Key, job.ConcurrencyGroup, want)
		}
		wantJobGroups[job.Key] = buildkiteConcurrencyGroup(repository, want)
	}
	if buildkiteConcurrencyGroup(repository, "Deploy") != buildkiteConcurrencyGroup(repository, "deploy") {
		t.Fatal("Buildkite concurrency group did not preserve GitHub's case-insensitive grouping")
	}

	var document struct {
		Steps []struct {
			Key              string `yaml:"key"`
			Concurrency      int    `yaml:"concurrency"`
			ConcurrencyGroup string `yaml:"concurrency_group"`
			DependsOn        []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(bundle.Pipeline, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 6 {
		t.Fatalf("pipeline steps = %#v\n%s", document.Steps, bundle.Pipeline)
	}
	wantWorkflowGroup := buildkiteConcurrencyGroup(repository, bundle.IR.Workflow.ConcurrencyGroup)
	if document.Steps[0].Concurrency != 1 || document.Steps[0].ConcurrencyGroup != wantWorkflowGroup || document.Steps[5].Concurrency != 1 || document.Steps[5].ConcurrencyGroup != wantWorkflowGroup {
		t.Fatalf("workflow gate groups = %#v / %#v, want %q", document.Steps[0], document.Steps[5], wantWorkflowGroup)
	}
	openKey := document.Steps[0].Key
	for _, step := range document.Steps[1:5] {
		if step.Concurrency != 1 || step.ConcurrencyGroup != wantJobGroups[step.Key] {
			t.Fatalf("generated job concurrency = %#v, want %q", step, wantJobGroups[step.Key])
		}
		if len(step.DependsOn) < 2 || step.DependsOn[1].Step != openKey || step.DependsOn[1].AllowFailure {
			t.Fatalf("generated job gate dependency = %#v", step.DependsOn)
		}
	}
	if len(document.Steps[5].DependsOn) != 5 {
		t.Fatalf("closing gate dependencies = %#v", document.Steps[5].DependsOn)
	}
	for _, dependency := range document.Steps[5].DependsOn[1:] {
		if !dependency.AllowFailure {
			t.Fatalf("closing gate dependency is strict: %#v", document.Steps[5].DependsOn)
		}
	}
}

func TestCompileBundleResolvesKafkaWorkflowConcurrency(t *testing.T) {
	source := []byte(`name: CI
on: pull_request
concurrency:
  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.ref }}
  cancel-in-progress: ${{ startsWith(github.ref, 'refs/pull/') }}
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	event := []byte(`{
  "provider": "github",
  "event": "pull_request",
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": "refs/pull/42/merge",
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": {"pull_request": {"number": 42}}
}`)
	bundle, err := CompileBundle(".github/workflows/ci.yml", source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.IR.Workflow.ConcurrencyGroup != "CI-42" {
		t.Fatalf("workflow concurrency group = %q, want CI-42", bundle.IR.Workflow.ConcurrencyGroup)
	}
	if len(bundle.IR.Warnings) != 1 || bundle.IR.Warnings[0].Code != "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED" || bundle.IR.Warnings[0].Line != 5 {
		t.Fatalf("workflow concurrency warnings = %#v", bundle.IR.Warnings)
	}
}

func TestCompileRejectsMatrixVaryingConcurrencyWithMaxParallel(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    strategy:
      max-parallel: 2
      matrix:
        target: [one, two]
    runs-on: ubuntu-latest
    concurrency: deploy-${{ matrix.target }}
    steps: [{run: true}]
`)
	_, err := CompileBundle("concurrency.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), "concurrency groups that vary by matrix cannot be combined with strategy.max-parallel") {
		t.Fatalf("CompileBundle() error = %v, want matrix concurrency conflict", err)
	}
}

func TestCompileAllowsCaseEquivalentMatrixConcurrencyWithMaxParallel(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    strategy:
      max-parallel: 2
      matrix:
        target: [Prod, prod]
    runs-on: ubuntu-latest
    concurrency: deploy-${{ matrix.target }}
    steps: [{run: true}]
`)
	bundle, err := CompileBundle("concurrency.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.IR.Jobs) != 2 || bundle.IR.Jobs[0].ConcurrencyGroup == bundle.IR.Jobs[1].ConcurrencyGroup {
		t.Fatalf("resolved concurrency groups = %#v", bundle.IR.Jobs)
	}
	for _, group := range []string{bundle.IR.Jobs[0].ConcurrencyGroup, bundle.IR.Jobs[1].ConcurrencyGroup} {
		if buildkiteConcurrencyGroup(bundle.IR.Event.Repository, group) != buildkiteConcurrencyGroup(bundle.IR.Event.Repository, "deploy-prod") {
			t.Fatalf("group %q did not canonicalize case-insensitively", group)
		}
	}
}

func TestValidateDefersRequiredGitHubConcurrencyValues(t *testing.T) {
	source := []byte(`on: push
concurrency: validate-${{ github.ref }}
jobs:
  test:
    runs-on: ubuntu-latest
    concurrency: test-${{ github.sha }}
    steps: [{run: true}]
  head-ref:
    runs-on: ubuntu-latest
    concurrency: ${{ github.head_ref }}
    steps: [{run: true}]
`)
	if _, err := Validate("concurrency.yml", source); err != nil {
		t.Fatalf("Validate() rejected event-backed concurrency: %v", err)
	}
}

func TestValidateClassifiesJobCancellationAsCompatibilityFailure(t *testing.T) {
	source := []byte(`on: push
jobs:
  label-rebase-needed:
    runs-on: ubuntu-latest
    concurrency:
      group: rebase-needed
      cancel-in-progress: true
    steps: [{run: true}]
`)
	report, err := Validate("rebase-needed.yml", source)
	if err == nil || !strings.Contains(err.Error(), `job "label-rebase-needed": concurrency cancel-in-progress is unsupported`) {
		t.Fatalf("Validate() error = %v, want unsupported job cancellation", err)
	}
	var finding *ProcessingFinding
	if !errors.As(err, &finding) || finding.Stage != StageExpressions || finding.Code != CodeExpressionInvalid || finding.Job != "label-rebase-needed" {
		t.Fatalf("Validate() finding = %#v", finding)
	}
	if report.LogicalJobs != 1 || report.Instances != 1 || len(report.Jobs) != 1 {
		t.Fatalf("Validate() report = %#v", report)
	}
}

func TestCanonicalWorkflowNameIsRepositoryRelative(t *testing.T) {
	relative := smokePath(".github", "workflows", "shell.yml")
	absolute, err := filepath.Abs(relative)
	if err != nil {
		t.Fatal(err)
	}
	want := ".github/workflows/shell.yml"
	if got := canonicalWorkflowName(relative); got != want {
		t.Fatalf("canonicalWorkflowName(relative) = %q, want %q", got, want)
	}
	if got := canonicalWorkflowName(absolute); got != want {
		t.Fatalf("canonicalWorkflowName(absolute) = %q, want %q", got, want)
	}
}

func TestCompileResolvesReusableJobConcurrencyInputs(t *testing.T) {
	repository := t.TempDir()
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  deploy:
    runs-on: ubuntu-latest
    concurrency: deploy-${{ inputs.target }}
    steps: [{run: true}]
`)
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      target: production
`)
	bundle, err := CompileBundle(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.IR.Jobs) != 1 || bundle.IR.Jobs[0].ConcurrencyGroup != "deploy-production" {
		t.Fatalf("reusable job concurrency = %#v", bundle.IR.Jobs)
	}
}

func TestCompileRejectsCalledWorkflowConcurrency(t *testing.T) {
	repository := t.TempDir()
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
concurrency: reusable
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
`)
	_, err := CompileBundle(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), "workflow concurrency in a called reusable workflow is unsupported") {
		t.Fatalf("CompileBundle() error = %v, want called workflow concurrency rejection", err)
	}
}

func TestCompileBundleDeclaresSecretCapabilityAndNames(t *testing.T) {
	source := []byte("name: secrets\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets['deploy_token'] }}\n    steps:\n      - run: echo \\\"${{ secrets.CANARY }}\\\"\n")
	event := readFile(t, smokePath("events", "push.json"))
	bundle, err := CompileBundle("workflow.yml", source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	job := bundle.Plans[0].Job
	if !reflect.DeepEqual(job.RequiredSecrets, []string{"CANARY", "DEPLOY_TOKEN"}) || !reflect.DeepEqual(job.RequiredCapabilities, []string{"secrets"}) {
		t.Fatalf("secret boundary = names %#v capabilities %#v", job.RequiredSecrets, job.RequiredCapabilities)
	}
}

func TestCompileBundleDeclaresScopedGitHubWorkflowToken(t *testing.T) {
	source := []byte(`name: token
on: push
permissions:
  contents: read
  pull-requests: write
jobs:
  token:
    runs-on: ubuntu-latest
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      OTHER: ${{ secrets.OTHER }}
    steps:
      - run: test -n "$GH_TOKEN"
  tokenless:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	bundle, err := CompileBundle(".github/workflows/token.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	jobs := map[string]PlanArtifact{}
	for _, artifact := range bundle.Plans {
		jobs[artifact.Job.Workflow.LogicalJobID] = artifact
	}
	token := jobs["token"]
	if token.Job.Schema != plan.Schema || token.Job.GitHubToken == nil || !reflect.DeepEqual(token.Job.GitHubToken.Permissions, map[string]string{"contents": "read", "pull_requests": "write"}) {
		t.Fatalf("GitHub workflow token plan = %#v", token.Job)
	}
	if !reflect.DeepEqual(token.Job.RequiredSecrets, []string{"OTHER"}) || !reflect.DeepEqual(token.Job.RequiredCapabilities, []string{"provider-token-write", "secrets"}) {
		t.Fatalf("token secret boundary = names %#v capabilities %#v", token.Job.RequiredSecrets, token.Job.RequiredCapabilities)
	}
	if !reflect.DeepEqual(token.Authorization.ProviderTokenWriteCapabilitySources, []string{"effective-permissions"}) || token.Authorization.WorkflowTokenPolicyFilename != "token.yml" {
		t.Fatalf("token authorization = %#v", token.Authorization)
	}
	if jobs["tokenless"].Job.GitHubToken != nil || jobs["tokenless"].Job.HasCapability("provider-token-write") {
		t.Fatalf("unreferenced permissions minted a token: %#v", jobs["tokenless"].Job)
	}
}

func TestCompileBundleKeepsReleaseTokensOnSeparateBoundaries(t *testing.T) {
	source := []byte(`name: Release
on: push
permissions:
  contents: write
jobs:
  goreleaser:
    runs-on: ubuntu-latest
    env:
      GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      HOMEBREW_TAP_GITHUB_TOKEN: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
    steps:
      - run: goreleaser release
`)
	bundle, err := CompileBundle(".github/workflows/release.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	job := bundle.Plans[0].Job
	if job.GitHubToken == nil || !reflect.DeepEqual(job.GitHubToken.Permissions, map[string]string{"contents": "write"}) {
		t.Fatalf("GITHUB_TOKEN boundary = %#v", job.GitHubToken)
	}
	if !reflect.DeepEqual(job.RequiredSecrets, []string{"HOMEBREW_TAP_GITHUB_TOKEN"}) || !reflect.DeepEqual(job.RequiredCapabilities, []string{"provider-token-write", "secrets"}) {
		t.Fatalf("ordinary secret boundary = names %#v capabilities %#v", job.RequiredSecrets, job.RequiredCapabilities)
	}
	if bytes.Contains(bundle.Pipeline, []byte("GITHUB_TOKEN")) || bytes.Contains(bundle.Pipeline, []byte("HOMEBREW_TAP_GITHUB_TOKEN")) {
		t.Fatalf("generated pipeline contains secret names:\n%s", bundle.Pipeline)
	}
}

func TestCompileBundleGitHubTokenUsesRestrictedDefaultPermissions(t *testing.T) {
	source := []byte("on: push\njobs:\n  token:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ secrets.GITHUB_TOKEN }}'\n")
	bundle, err := CompileBundle(".github/workflows/workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	job := bundle.Plans[0]
	if job.Job.GitHubToken == nil || !reflect.DeepEqual(job.Job.GitHubToken.Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("default GitHub workflow token plan = %#v", job.Job)
	}
	if !reflect.DeepEqual(job.Authorization.ProviderTokenWriteCapabilitySources, []string{"effective-permissions"}) || job.Authorization.WorkflowTokenPolicyFilename != "workflow.yml" {
		t.Fatalf("default token authorization = %#v", job.Authorization)
	}
}

func TestCompileBundleGitHubTokenRejectsExplicitEmptyPermissions(t *testing.T) {
	for _, permissions := range []string{"permissions: {}\n", "permissions:\n  contents: none\n"} {
		source := []byte("on: push\n" + permissions + "jobs:\n  token:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ secrets.GITHUB_TOKEN }}'\n")
		_, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
		if err == nil || !strings.Contains(err.Error(), "references secrets.GITHUB_TOKEN but has no effective permissions") {
			t.Fatalf("CompileBundle() error = %v, want empty permission rejection", err)
		}
	}
}

func TestCompileBundleJobPermissionsReplaceWorkflowPermissions(t *testing.T) {
	source := []byte(`on: push
permissions:
  contents: read
jobs:
  token:
    runs-on: ubuntu-latest
    permissions:
      pull-requests: write
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    steps: [{run: true}]
`)
	bundle, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if got := bundle.Plans[0].Job.GitHubToken.Permissions; !reflect.DeepEqual(got, map[string]string{"pull_requests": "write"}) {
		t.Fatalf("effective job permissions = %#v", got)
	}
}

func TestCompileBundleRejectsPermissionNormalizationCollision(t *testing.T) {
	source := []byte(`on: push
permissions:
  pull-requests: read
  pull_requests: write
jobs:
  token:
    runs-on: ubuntu-latest
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    steps: [{run: true}]
`)
	_, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), `unsupported permission "pull_requests"`) {
		t.Fatalf("CompileBundle() error = %v, want non-canonical permission rejection", err)
	}
}

func TestCompileBundleRejectsDynamicSecretIndex(t *testing.T) {
	source := []byte("name: secrets\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      SECRET_NAME: TOKEN\n      TOKEN: ${{ secrets[env.SECRET_NAME] }}\n    steps:\n      - run: true\n")
	_, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), "expression index must be a string literal") {
		t.Fatalf("CompileBundle() error = %v, want dynamic secret index rejection", err)
	}
}

func TestCompileBundleScansRetainedExpressionsForAuthority(t *testing.T) {
	source := []byte(`on: push
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: ${{ secrets.NAME_SECRET }}
        run: echo '${{ github.token }}'
`)
	bundle, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	job := bundle.Plans[0].Job
	if !reflect.DeepEqual(job.RequiredSecrets, []string{"NAME_SECRET"}) {
		t.Fatalf("retained field secrets = %#v", job.RequiredSecrets)
	}
	if job.GitHubToken == nil || !job.HasCapability("provider-token-write") {
		t.Fatalf("github.token authority = %#v, capabilities %#v", job.GitHubToken, job.RequiredCapabilities)
	}
}

func TestCompileBundleRejectsRetainedGitHubEventPayload(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo '${{ github.event.action }}'
`)
	_, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), "github.event cannot be retained in a job plan") {
		t.Fatalf("CompileBundle() error = %v, want retained event rejection", err)
	}
}

func TestRequiredSecretsDoesNotInterpretConditionLiteralsAsTemplates(t *testing.T) {
	instance := JobInstance{If: "'${{ github.token }} ${{ secrets.DEPLOY }} ${{ github.event.action }}' == runner.os"}
	secrets, err := requiredSecrets(instance, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 0 {
		t.Fatalf("condition literal granted secret authority: %#v", secrets)
	}
}

func encodeGoldenPlans(t *testing.T, artifacts []PlanArtifact) []byte {
	t.Helper()
	plans := make([]json.RawMessage, len(artifacts))
	for i, artifact := range artifacts {
		plans[i] = json.RawMessage(bytes.TrimSpace(artifact.Contents))
	}
	encoded, err := json.MarshalIndent(plans, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}
