package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"go.yaml.in/yaml/v4"
)

const (
	cliTestBuildID       = "11111111-1111-4111-8111-111111111111"
	cliTestJobID         = "22222222-2222-4222-8222-222222222222"
	cliTestProducerJobID = "33333333-3333-4333-8333-333333333333"
)

func TestRunHelpAndVersion(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{name: "help flag", args: []string{"--help"}, wantOutput: "validate"},
		{name: "help command", args: []string{"help"}, wantOutput: "run-job"},
		{name: "command help", args: []string{"help", "compile"}, wantOutput: "buildkite-gha compile --event-path"},
		{name: "upload help", args: []string{"help", "upload"}, wantOutput: "--runtime-queue hosted"},
		{name: "command help flag", args: []string{"run-job", "--help"}, wantOutput: "--plan <path>"},
		{name: "version flag", args: []string{"--version"}, wantOutput: "buildkite-gha test-version\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr, "test-version"); code != 0 {
				t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.wantOutput) {
				t.Errorf("Run() stdout = %q, want it to contain %q", stdout.String(), test.wantOutput)
			}
			if stderr.Len() != 0 {
				t.Errorf("Run() stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunValidateAndCompile(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")

	t.Run("validate", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		if want := "2 logical jobs and 3 static instances"; !strings.Contains(stdout.String(), want) {
			t.Fatalf("Run() stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("validate json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.Report
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "compilable" || report.Instances != 3 {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("validate json blocker", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "dynamic.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}\n    steps:\n      - run: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflow}, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.Report
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "E_COMPILE" {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("compile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"compile", workflowPath, "--event-path", eventPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var pipeline struct {
			Steps []struct {
				Key string `yaml:"key"`
			} `yaml:"steps"`
		}
		if err := yaml.Unmarshal(stdout.Bytes(), &pipeline); err != nil {
			t.Fatalf("compile output is not pipeline YAML: %v", err)
		}
		if len(pipeline.Steps) != 3 {
			t.Fatalf("compiled steps = %d, want 3", len(pipeline.Steps))
		}
	})

	t.Run("compile ir json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"compile", "--format", "ir-json", workflowPath, "--event-path", eventPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var ir compiler.IR
		if err := json.Unmarshal(stdout.Bytes(), &ir); err != nil || len(ir.Jobs) != 3 {
			t.Fatalf("compile IR = %#v, error = %v", ir, err)
		}
	})
}

func TestRunUploadCompilesArtifactsAndUploadsSelfContainedPipeline(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "phase-2-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 3 jobs") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if len(runner.commands) != 5 {
		t.Fatalf("commands = %#v, want distribution, three plans, and pipeline", runner.commands)
	}
	root := runner.commands[0].dir
	for i, command := range runner.commands[:4] {
		if command.dir != root || command.name != "buildkite-agent" || len(command.args) != 3 || command.args[0] != "artifact" || command.args[1] != "upload" {
			t.Fatalf("artifact command %d = %#v", i, command)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temporary artifact root still exists: %v", err)
	}
	pipelineCommand := runner.commands[4]
	wantPipelineArgs := []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets"}
	if strings.Join(pipelineCommand.args, " ") != strings.Join(wantPipelineArgs, " ") {
		t.Fatalf("pipeline args = %#v, want %#v", pipelineCommand.args, wantPipelineArgs)
	}
	var pipeline struct {
		Steps []struct {
			Key     string `yaml:"key"`
			Command string `yaml:"command"`
			Agents  struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
			Checkout struct {
				Skip bool `yaml:"skip"`
			} `yaml:"checkout"`
			DependsOn []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 3 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps {
		if !step.Checkout.Skip || step.Agents.Queue != "hosted" || len(step.DependsOn) == 0 || step.DependsOn[0].Step != "phase-2-importer" || step.DependsOn[0].AllowFailure {
			t.Fatalf("step %q lacks isolated checkout or exact importer dependency: %#v", step.Key, step)
		}
		if !strings.Contains(step.Command, `bootstrap_dir="$(mktemp -d `) ||
			!strings.Contains(step.Command, `--step 'phase-2-importer'`) ||
			!strings.Contains(step.Command, `sha256sum "$distribution"`) ||
			!strings.Contains(step.Command, `"$distribution" run-job --plan "$plan"`) {
			t.Fatalf("step %q command is not self-contained:\n%s", step.Key, step.Command)
		}
	}
}

func TestRunUploadFailsClosedBeforePipeline(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "phase-2-importer")
	runner := &cliCaptureRunner{failAt: 2}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", workflowPath, "--event-path", eventPath, "--runtime-queue", "hosted"}, &stdout, &stderr, "dev", runner); code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if len(runner.commands) != 2 || !strings.Contains(stderr.String(), "upload artifact") {
		t.Fatalf("commands = %#v, stderr = %q", runner.commands, stderr.String())
	}
}

func TestUnprivilegedUploadRejectsCapabilities(t *testing.T) {
	for _, capability := range []string{"secrets", "provider-token-read", "provider-token-write", "privileged-container", "docker", "network"} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow:             plan.Workflow{LogicalJobID: "protected"},
			RequiredCapabilities: []string{capability},
		}}}}
		if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), capability) {
			t.Fatalf("validateUnprivilegedBundle(%q) error = %v, want capability rejection", capability, err)
		}
	}
}

func TestUnprivilegedUploadRejectsActionSteps(t *testing.T) {
	for _, step := range []plan.Step{
		{ID: "remote", Kind: "uses", Uses: "actions/checkout@v4"},
		{ID: "malformed", Kind: "run", Uses: "./.github/actions/local"},
	} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow: plan.Workflow{LogicalJobID: "action-job"},
			Steps:    []plan.Step{step},
		}}}}
		if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), "action step") {
			t.Fatalf("validateUnprivilegedBundle(%#v) error = %v, want action-step rejection", step, err)
		}
	}
}

func TestUnprivilegedUploadRejectsConcurrentStepsUntilSupervisorIsActive(t *testing.T) {
	for _, step := range []plan.Step{
		{ID: "background", Kind: "run", Command: "true", Background: true},
		{ID: "wait", Kind: "wait", Targets: []string{"background"}},
		{ID: "wait-all", Kind: "wait-all"},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow: plan.Workflow{LogicalJobID: "concurrent-job"},
			Steps:    []plan.Step{step},
		}}}}
		if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), "concurrent step") {
			t.Fatalf("validateUnprivilegedBundle(%#v) error = %v, want concurrent-step rejection", step, err)
		}
	}
}

type cliCommand struct {
	dir   string
	name  string
	args  []string
	stdin []byte
}

type cliCaptureRunner struct {
	commands      []cliCommand
	failAt        int
	failMetadata  bool
	jobByStep     map[string]string
	dataByPath    map[string][]byte
	uploaded      map[string][]byte
	contextErrors []error
}

func (r *cliCaptureRunner) Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, cliCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: bytes.Clone(stdin)})
	r.contextErrors = append(r.contextErrors, ctx.Err())
	if r.failAt != 0 && len(r.commands) == r.failAt {
		return nil, errors.New("injected failure")
	}
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "search" {
		return []byte(r.jobByStep[args[4]] + "\n"), nil
	}
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "download" {
		contents, ok := r.dataByPath[args[2]]
		if !ok {
			return nil, errors.New("missing fixture artifact")
		}
		path := filepath.Join(args[3], filepath.FromSlash(args[2]))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, contents, 0o600)
	}
	if len(args) >= 3 && args[0] == "artifact" && args[1] == "upload" {
		contents, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(args[2])))
		if err != nil {
			return nil, err
		}
		if r.uploaded == nil {
			r.uploaded = map[string][]byte{}
		}
		r.uploaded[args[2]] = contents
	}
	if r.failMetadata && len(args) >= 2 && args[0] == "meta-data" && args[1] == "set" {
		return nil, errors.New("metadata unavailable")
	}
	return nil, nil
}

var _ transport.Runner = (*cliCaptureRunner)(nil)

func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing command", want: "Usage:"},
		{name: "unknown command", args: []string{"nope"}, want: `unknown command "nope"`},
		{name: "unknown help command", args: []string{"help", "nope"}, want: `unknown command "nope"`},
		{name: "version arguments", args: []string{"--version", "extra"}, want: "does not accept arguments"},
		{name: "upload missing event", args: []string{"upload", "--runtime-queue", "hosted", "workflow.yml"}, want: "--event-path is required"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr, "dev"); code != 2 {
				t.Fatalf("Run() code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("Run() stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Errorf("Run() stderr = %q, want it to contain %q", stderr.String(), test.want)
			}
		})
	}
}

func TestRunJobExecutesBoundPlanAndWritesResult(t *testing.T) {
	workspace := t.TempDir()
	workflowSource := []byte("name: cli fixture\n")
	workflowPath := filepath.Join(workspace, "workflow.yml")
	if err := os.WriteFile(workflowPath, workflowSource, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(workflowSource)
	job := plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "dev", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow: plan.Workflow{Path: "workflow.yml", Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "cli"},
		Event:    plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:   plan.Target{StepKey: "gha-cli", Queue: "ubuntu-latest"},
		Outputs:  map[string]string{"result": "${{ steps.produce.outputs.result }}"},
		Steps:    []plan.Step{{ID: "produce", Kind: "run", Command: `echo "result=cli-ok" >> "$GITHUB_OUTPUT"`}},
	}
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(workspace, "plan.json")
	resultPath := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest := sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", job.Target.StepKey)
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", job.Target.Queue)
	oldDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDirectory) })

	var stdout, stderr bytes.Buffer
	runner := &cliCaptureRunner{}
	if code := run([]string{"run-job", "--plan", planPath, "--result", resultPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"result": "cli-ok"`) {
		t.Fatalf("result = %s", result)
	}
	stdout.Reset()
	stderr.Reset()
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+strings.Repeat("0", 64))
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not match expected digest") {
		t.Fatalf("Run() code = %d, stderr = %q, want digest mismatch", code, stderr.String())
	}
	job.Compiler.Version = "0.0.0-other"
	encoded, err = plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest = sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not match runtime version") {
		t.Fatalf("Run() code = %d, stderr = %q, want version mismatch", code, stderr.String())
	}
}

func TestRunJobHydratesNeedsAndPublishesAuthoritativeResult(t *testing.T) {
	producerStep := "gha-producer"
	producerPlanDigest := transport.Digest([]byte("producer-plan"))
	job := cliRunJobPlan()
	job.Dependencies = []string{producerStep}
	job.NeedSources = map[string][]plan.NeedSource{
		"producer": {{StepKey: producerStep, PlanDigest: producerPlanDigest}},
	}
	job.Env = map[string]string{"RESULT": "${{ needs.producer.outputs.result }}"}
	job.Steps = []plan.Step{{ID: "consume", Kind: "run", Command: `test "$RESULT" = "hydrated"`}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)

	producerManifest, err := transport.MarshalResultManifest(transport.ResultManifest{
		PlanDigest: producerPlanDigest,
		Producer:   transport.Producer{BuildID: cliTestBuildID, JobID: cliTestProducerJobID, StepKey: producerStep},
		Result:     "success",
		Outputs:    []transport.Output{{Name: "result", Value: "hydrated"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	producerPath := transport.ResultPath(producerStep, producerPlanDigest)
	runner := &cliCaptureRunner{
		failMetadata: true,
		jobByStep:    map[string]string{producerStep: cliTestProducerJobID},
		dataByPath:   map[string][]byte{producerPath: producerManifest},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: result metadata mirror") {
		t.Fatalf("stderr = %q, want non-fatal metadata warning", stderr.String())
	}
	if len(runner.commands) < 3 || strings.Join(runner.commands[0].args, " ") != strings.Join([]string{"artifact", "search", producerPath, "--step", producerStep, "--format", "%j"}, " ") {
		t.Fatalf("commands = %#v, want exact producer search first", runner.commands)
	}
	if got := runner.commands[1].args[len(runner.commands[1].args)-1]; got != cliTestProducerJobID {
		t.Fatalf("download producer = %q, want exact job UUID", got)
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if manifest.Result != "success" {
		t.Fatalf("published result = %q, want success", manifest.Result)
	}
	for _, command := range runner.commands {
		if command.dir != "" {
			if _, err := os.Stat(command.dir); !os.IsNotExist(err) {
				t.Fatalf("temporary artifact root %q still exists: %v", command.dir, err)
			}
		}
	}
}

func TestRunJobPublishesEveryTerminalResultAfterCancellation(t *testing.T) {
	tests := []struct {
		name       string
		condition  string
		command    string
		cancel     bool
		wantResult string
		wantCode   int
	}{
		{name: "failure", command: "exit 7", wantResult: "failure", wantCode: 1},
		{name: "skipped", condition: "${{ false }}", command: "true", wantResult: "skipped", wantCode: 0},
		{name: "cancelled", command: "sleep 1", cancel: true, wantResult: "cancelled", wantCode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Condition = test.condition
			job.Steps[0].Command = test.command
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			runner := &cliCaptureRunner{}
			ctx := context.Background()
			if test.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			var stdout, stderr bytes.Buffer
			if code := runJobContext(ctx, []string{"--plan", planPath}, &stdout, &stderr, "dev", transport.Agent{Runner: runner}); code != test.wantCode {
				t.Fatalf("runJobContext() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			manifest := publishedCLIManifest(t, runner, job, planDigest)
			if manifest.Result != test.wantResult {
				t.Fatalf("published result = %q, want %q", manifest.Result, test.wantResult)
			}
			for i, contextErr := range runner.contextErrors {
				if contextErr != nil {
					t.Fatalf("publication command %d inherited cancelled context: %v", i, contextErr)
				}
			}
		})
	}
}

func TestRunJobPublishesHydrationFailureAndRejectsMissingIdentity(t *testing.T) {
	producerStep := "gha-producer"
	producerPlanDigest := transport.Digest([]byte("producer-plan"))
	job := cliRunJobPlan()
	job.Dependencies = []string{producerStep}
	job.NeedSources = map[string][]plan.NeedSource{
		"producer": {{StepKey: producerStep, PlanDigest: producerPlanDigest}},
	}
	planPath, planDigest := writeCLIJobPlan(t, job)

	t.Run("needs require Buildkite", func(t *testing.T) {
		clearCLIJobIdentity(t)
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "prerequisites require Buildkite result identity") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want no unauthenticated agent calls", runner.commands)
		}
	})

	t.Run("hydration failure is published", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		runner := &cliCaptureRunner{jobByStep: map[string]string{producerStep: cliTestProducerJobID}}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "hydrate prerequisite results") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		manifest := publishedCLIManifest(t, runner, job, planDigest)
		if manifest.Result != "failure" {
			t.Fatalf("published result = %q, want failure", manifest.Result)
		}
	})

	t.Run("partial identity fails before execution", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		t.Setenv("BUILDKITE_JOB_ID", "")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "requires valid BUILDKITE_BUILD_ID") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want identity failure before side effects", runner.commands)
		}
	})

	t.Run("missing plan digest fails before execution", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "requires BUILDKITE_GHA_PLAN_DIGEST") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want digest failure before side effects", runner.commands)
		}
	})
}

func TestRunJobFailsWhenAuthoritativePublicationFails(t *testing.T) {
	job := cliRunJobPlan()
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	runner := &cliCaptureRunner{failAt: 1}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "publish terminal result") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunJobPublishesBoundedFailureForUnrepresentableOutputs(t *testing.T) {
	job := cliRunJobPlan()
	job.Outputs = make(map[string]string, transport.MaxResultOutputs+1)
	for i := 0; i <= transport.MaxResultOutputs; i++ {
		job.Outputs[fmt.Sprintf("output_%02d", i)] = "value"
	}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "validate terminal result") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if manifest.Result != "failure" || len(manifest.Outputs) != 0 {
		t.Fatalf("published result = %#v, want bounded failure without outputs", manifest)
	}
}

func TestVerifyBuildkiteTargetFailsClosed(t *testing.T) {
	job := plan.Job{Target: plan.Target{StepKey: "gha-expected", Queue: "gha-runtime"}}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "gha-other")
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "gha-runtime")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "executing step") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want step mismatch", err)
	}
	t.Setenv("BUILDKITE_STEP_KEY", "")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "requires BUILDKITE_STEP_KEY") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want missing binding", err)
	}
}

func TestArgumentParsersRejectRepeatedOptions(t *testing.T) {
	if _, _, err := runJobArgs([]string{"--plan", "one", "--plan", "two"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("runJobArgs() error = %v, want duplicate option error", err)
	}
	if _, _, err := workflowArgs([]string{"--event-path", "one", "--event-path", "two", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("workflowArgs() error = %v, want duplicate option error", err)
	}
	if _, _, _, err := validateArgs([]string{"--format", "json", "--format", "text", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("validateArgs() error = %v, want duplicate format error", err)
	}
	if _, _, _, err := compileArgs([]string{"--format", "pipeline", "--format", "ir-json", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("compileArgs() error = %v, want duplicate format error", err)
	}
	if _, _, _, err := uploadArgs([]string{"--runtime-queue", "one", "--runtime-queue", "two", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("uploadArgs() error = %v, want duplicate runtime queue error", err)
	}
	if _, _, _, err := uploadArgs([]string{"workflow.yml"}); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("uploadArgs() error = %v, want required runtime queue error", err)
	}
	if _, _, _, err := uploadArgs([]string{"--runtime-queue", "elastic-runners", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), `must be "hosted"`) {
		t.Fatalf("uploadArgs() error = %v, want fixed runtime queue error", err)
	}
}

func TestRunJobExecutesPureRunPlanWithoutCheckout(t *testing.T) {
	clearCLIJobIdentity(t)
	workspace := t.TempDir()
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Workflow: plan.Workflow{
			Path:         "missing-workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "missing",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-missing", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
	}
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(workspace, "plan.json")
	resultPath := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	oldDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDirectory) })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run-job", "--plan", planPath, "--result", resultPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q, want success", code, stderr.String())
	}
	result, err := os.ReadFile(resultPath)
	if err != nil || !bytes.Contains(result, []byte(`"conclusion": "success"`)) {
		t.Fatalf("result = %q, error = %v", result, err)
	}
}

func cliRunJobPlan() plan.Job {
	return plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Workflow: plan.Workflow{
			Path:         "workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "cli",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-cli", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
	}
}

func writeCLIJobPlan(t *testing.T, job plan.Job) (string, string) {
	t.Helper()
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, transport.Digest(encoded)
}

func setCLIJobIdentity(t *testing.T, job plan.Job, planDigest string) {
	t.Helper()
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", job.Target.StepKey)
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", job.Target.Queue)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", planDigest)
}

func clearCLIJobIdentity(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BUILDKITE",
		"BUILDKITE_BUILD_ID",
		"BUILDKITE_JOB_ID",
		"BUILDKITE_STEP_KEY",
		"BUILDKITE_AGENT_META_DATA_QUEUE",
		"BUILDKITE_GHA_PLAN_DIGEST",
	} {
		t.Setenv(name, "")
	}
}

func publishedCLIManifest(t *testing.T, runner *cliCaptureRunner, job plan.Job, planDigest string) transport.ResultManifest {
	t.Helper()
	path := transport.ResultPath(job.Target.StepKey, planDigest)
	data, ok := runner.uploaded[path]
	if !ok {
		t.Fatalf("authoritative result %q was not uploaded; uploads = %#v", path, runner.uploaded)
	}
	producer := transport.Producer{BuildID: cliTestBuildID, JobID: cliTestJobID, StepKey: job.Target.StepKey}
	manifest, err := transport.VerifyResultManifest(data, planDigest, producer)
	if err != nil {
		t.Fatalf("verify published result: %v\n%s", err, data)
	}
	return manifest
}
