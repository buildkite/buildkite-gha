package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	executionprogram "github.com/buildkite/buildkite-gha/internal/program"
)

type testSecretResolver map[string]string

func (s testSecretResolver) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := s[name]
	if !ok {
		return "", fmt.Errorf("denied")
	}
	return value, nil
}

type testRedactor struct{ values []string }

func (r *testRedactor) AddRedaction(_ context.Context, value string) error {
	r.values = append(r.values, value)
	return nil
}

type testWorkflowTokenProvider struct {
	token       string
	repository  string
	workflow    string
	permissions map[string]string
	calls       int
}

func (p *testWorkflowTokenProvider) WorkflowToken(_ context.Context, repository, workflow string, permissions map[string]string) (string, error) {
	p.calls++
	p.repository = repository
	p.workflow = workflow
	p.permissions = maps.Clone(permissions)
	return p.token, nil
}

type failingTokenRedactor struct{ token string }

func (r failingTokenRedactor) AddRedaction(context.Context, string) error {
	return fmt.Errorf("failed to redact %s", r.token)
}

func (fake fakeDocker) calls(t *testing.T) []fakeDockerCall {
	t.Helper()
	data, err := os.ReadFile(fake.transcript)
	if err != nil {
		t.Fatal(err)
	}
	var calls []fakeDockerCall
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Split(line, "|")
		metadata := fields[0]
		config, ok := strings.CutPrefix(strings.SplitN(metadata, ";", 2)[0], "config=")
		if !ok {
			t.Fatalf("malformed fake Docker transcript line %q", line)
		}
		calls = append(calls, fakeDockerCall{config: config, metadata: metadata, args: fields[1:]})
	}
	return calls
}

func TestRunJobEvaluatesIndexedWorkflowInputs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/inputs.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: inputs\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID: "check", Kind: "run", Command: `test "$VALUE" = dispatched`,
		Env: map[string]string{"VALUE": "${{ inputs[env.KEY] }}"},
	}})
	job.Inputs = map[string]any{"label": "dispatched"}
	job.Env = map[string]string{"KEY": "label"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestRunJobResolvesWorkspaceAndRunIdentity(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/identity.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: identity\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID: "check", Kind: "run",
		Command: `test "$FAKE_HOME" = "$GITHUB_WORKSPACE/fake-home" &&
test "$RUN_KEY" = key-b5e828e8-7457-013c-9a17-2f01b563f36a-512-2 &&
test "$GITHUB_RUN_ID" = b5e828e8-7457-013c-9a17-2f01b563f36a &&
test "$GITHUB_RUN_NUMBER" = 512 &&
test "$GITHUB_RUN_ATTEMPT" = 2`,
	}})
	job.Env = map[string]string{
		"FAKE_HOME": "${{ github.workspace }}/fake-home",
		"RUN_KEY":   "key-${{ github.run_id }}-${{ github.run_number }}-${{ github.run_attempt }}",
	}
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs, RunIdentity: RunIdentity{BuildID: "b5e828e8-7457-013c-9a17-2f01b563f36a", BuildNumber: "512", RetryCount: "1"}}
	result, err := runner.runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestRunJobRejectsRunIdentityExpressionsWithoutIdentity(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/identity.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: identity\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "check", Kind: "run", Command: "true"}})
	job.Env = map[string]string{"RUN_KEY": "key-${{ github.run_id }}"}
	var logs bytes.Buffer
	_, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `unavailable github value "run_id"`) {
		t.Fatalf("RunJob() error = %v, want unavailable run_id", err)
	}
}

func TestRunJobDoesNotTolerateDockerActionCleanupFailure(t *testing.T) {
	requireLinuxAMD64(t)
	fake := newFakeDocker(t, "leftover")
	workspace := fixturePath(t)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []runtimeTestStep{{
		ID: "docker", Kind: "uses", Uses: "./actions/docker", ContinueOnError: true,
	}})
	job.RequiredCapabilities = []string{"docker", "network"}
	job.ContinueOnError = true

	result, err := (Runner{Docker: fake.path}).runTestJob(t.Context(), job, workspace)
	if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "owned Docker resources remain after cleanup") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestSequentialRunControlsAndEnvironment(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	if err := os.Mkdir(filepath.Join(workspace, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	redactor := &testRedactor{}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "first", Kind: "run", WorkingDirectory: "subdir", Env: map[string]string{"LEVEL": "step"}, Command: `test "$LEVEL" = step
test "$TOKEN" = mask-me
test "$GITHUB_SHA" = 0123456789abcdef
test "${{ github.repository }}" = buildkite/example
echo "$GITHUB_WORKSPACE/bin" >> "$GITHUB_PATH"
echo "LEVEL=file" >> "$GITHUB_ENV"
echo "secret=${{ secrets.CANARY }}"`},
		{ID: "soft", Kind: "run", Command: "exit 7", ContinueOnError: true},
		{ID: "after-soft", Kind: "run", Condition: "steps.soft.outcome == 'failure' && steps.soft.conclusion == 'success'", Env: map[string]string{"SOFT_OUTCOME": "${{ steps.soft.outcome }}"}, Command: `test "$LEVEL" = file
test "$SOFT_OUTCOME:${{ steps.soft.conclusion }}" = failure:success
case "$PATH" in "$GITHUB_WORKSPACE/bin"*) ;; *) exit 9 ;; esac
echo after-soft`},
	})
	job.Env = map[string]string{"LEVEL": "job", "TOKEN": "${{ secrets.CANARY }}"}
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"CANARY"}
	job.Event.Repository = "buildkite/example"
	job.Event.Ref = "refs/heads/main"
	job.Event.SHA = "0123456789abcdef"
	job.Event.Actor = "octocat"
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: testSecretResolver{"CANARY": "mask-me"}, Redactor: redactor}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v, logs = %q", err, logs.String())
	}
	if result.Conclusion != "success" || result.Env["LEVEL"] != "file" || result.Env["TOKEN"] != "***" || len(redactor.values) != 1 {
		t.Fatalf("RunJob() result = %#v, redactions = %#v", result, redactor.values)
	}
	if strings.Contains(logs.String(), "mask-me") || !strings.Contains(logs.String(), "secret=***") || !strings.Contains(logs.String(), "after-soft") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestCompiledRunDefaultsEvaluateAtRuntime(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/defaults.yml"
	source := `on: push
jobs:
  matrix-defaults:
    strategy:
      matrix:
        shell: [bash]
        dir: [matrix]
    runs-on: ubuntu-latest
    defaults:
      run:
        shell: ${{ matrix.shell }}
        working-directory: ${{ matrix.dir }}
    steps:
      - run: test "$(basename "$PWD")" = matrix
  env-defaults:
    runs-on: ubuntu-latest
    env:
      DEFAULT_SHELL: bash
      DEFAULT_DIR: env
    defaults:
      run:
        shell: ${{ env.DEFAULT_SHELL }}
        working-directory: ${{ env.DEFAULT_DIR }}
    steps:
      - run: test "$(basename "$PWD")" = env
  vars-defaults:
    runs-on: ubuntu-latest
    defaults:
      run:
        shell: ${{ vars.DEFAULT_SHELL }}
        working-directory: ${{ vars.DEFAULT_DIR }}
    steps:
      - run: test "$(basename "$PWD")" = vars
  explicit-precedence:
    runs-on: ubuntu-latest
    env:
      DEFAULT_SHELL: unsupported-default
    defaults:
      run:
        shell: ${{ env.DEFAULT_SHELL }}
        working-directory: missing-default
    steps:
      - shell: sh
        working-directory: ${{ vars.OVERRIDE_DIR }}
        run: test "$(basename "$PWD")" = override
`
	writeFixtureFile(t, workspace, workflowPath, source)
	for _, directory := range []string{"matrix", "env", "vars", "override"} {
		if err := os.Mkdir(filepath.Join(workspace, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(t.Context(),
		filepath.Join(workspace, workflowPath),
		[]byte(source),
		event,
		"0.0.0-test",
		"sha256:"+strings.Repeat("2", 64),
		compiler.Options{
			EventTrust: compiler.EventUntrusted,
			Vars: compiler.VariableSources{Bridge: map[string]string{
				"DEFAULT_SHELL": "bash",
				"DEFAULT_DIR":   "vars",
				"OVERRIDE_DIR":  "override",
			}},
			Runners: compiler.RunnerPolicy{
				Labels:          map[string]string{"ubuntu-latest": "gha-untrusted"},
				UntrustedQueues: []string{"gha-untrusted"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 4 {
		t.Fatalf("plans = %d, want four defaults cases", len(plans))
	}
	for _, job := range plans {
		result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
		if err != nil || result.Conclusion != "success" {
			t.Fatalf("RunJob(%s) result = %#v, error = %v", job.Workflow.LogicalJobID, result, err)
		}
	}
}

func TestCompiledDynamicUnsupportedShellFailsAtRuntime(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/dynamic-shell.yml"
	source := `on: push
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      DEFAULT_SHELL: pwsh
    steps:
      - shell: ${{ env.DEFAULT_SHELL }}
        run: Write-Output test
`
	writeFixtureFile(t, workspace, workflowPath, source)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(
		filepath.Join(workspace, workflowPath),
		[]byte(source),
		event,
		"0.0.0-test",
		"sha256:"+strings.Repeat("2", 64),
		"gha-untrusted",
	)
	if err != nil {
		t.Fatalf("compile runtime-dependent shell: %v", err)
	}
	if len(plans) != 1 || plans[0].ExecutionJob().Steps[0].Run.Shell.Source != "${{ env.DEFAULT_SHELL }}" {
		t.Fatalf("compiled plans = %#v", plans)
	}
	_, err = (Runner{}).runTestJob(t.Context(), plans[0], workspace)
	if err == nil {
		t.Fatal("RunJob() succeeded")
	}
	if got := ClassifyFailure(err); got != FailureClassUnsupportedFeature {
		t.Fatalf("ClassifyFailure() = %q, want %q", got, FailureClassUnsupportedFeature)
	}
	for _, want := range []string{
		`shell "pwsh" is unsupported`,
		"Use bash, sh, python, or a valid custom shell template whose command is available on PATH",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("RunJob() error = %v, want %q", err, want)
		}
	}
}

func TestFailureConditionsAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "fail", Kind: "run", Command: "exit 1"},
		{ID: "default", Kind: "run", Command: "echo must-not-run"},
		{ID: "recover", Kind: "run", Condition: "failure()", Command: "echo recovered"},
	})
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || strings.Contains(logs.String(), "must-not-run") || !strings.Contains(logs.String(), "recovered") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}

	job = runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "timeout", Kind: "run", Command: "sleep 30", TimeoutMinutes: 0.0005}})
	started := time.Now()
	result, err = (Runner{}).runTestJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "failure" || time.Since(started) > 3*time.Second {
		t.Fatalf("timed RunJob() result = %#v, error = %v, elapsed = %s", result, err, time.Since(started))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	job = runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "cancel", Kind: "run", Condition: "always()", Command: "sleep 30"}})
	result, err = (Runner{}).runTestJob(ctx, job, workspace)
	if !errors.Is(err, context.Canceled) || result.Conclusion != "cancelled" {
		t.Fatalf("cancelled RunJob() result = %#v, error = %v", result, err)
	}
}

func TestExpressionValuedStepControls(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: expression controls\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "soft", Kind: "run", Command: "exit 7", ContinueOnErrorExpression: "${{ matrix.experimental }}", TimeoutMinutesExpression: "${{ matrix.timeout }}"},
		{ID: "verify", Kind: "run", Condition: "steps.soft.outcome == 'failure' && steps.soft.conclusion == 'success'", Command: "true"},
	})
	job.Matrix = map[string]any{"experimental": true, "timeout": 1.0}
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	job, err = plan.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestSkippedStepDoesNotEvaluateTypedControls(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: skipped controls\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID: "skipped", Kind: "run", Condition: "false", Command: "true", TimeoutMinutesExpression: "${{ fromJSON('invalid') }}",
	}})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestStepTimeoutExpressionUsesSameStepEnvironment(t *testing.T) {
	step := normalizedTestStep(runtimeTestStep{Env: map[string]string{"MINUTES": "5"}, TimeoutMinutesExpression: "${{ fromJSON(env.MINUTES) }}"})
	context := expression.Context{}
	env, err := executionprogram.EvaluateBindings(step.Env, executionprogram.EvaluationContext{Expression: context})
	if err != nil {
		t.Fatal(err)
	}
	context.Env = env
	timeoutMinutes, err := evaluateStepTimeout(step, context)
	if err != nil || timeoutMinutes != 5 {
		t.Fatalf("evaluateStepTimeout() = %v, %v", timeoutMinutes, err)
	}
}

func TestExpressionValuedStepControlsRequireTypedBoundedResults(t *testing.T) {
	for _, test := range []struct {
		name string
		step runtimeTestStep
		want string
	}{
		{name: "boolean", step: runtimeTestStep{ContinueOnErrorExpression: "${{ 'true' }}"}, want: "want boolean"},
		{name: "number", step: runtimeTestStep{TimeoutMinutesExpression: "${{ '1' }}"}, want: "want number"},
		{name: "range", step: runtimeTestStep{TimeoutMinutesExpression: "${{ 361 }}"}, want: "at most 360"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := evaluateStepControls(normalizedTestStep(test.step), expression.Context{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("evaluateStepControls() error = %v, want %q", err, test.want)
			}
		})
	}
}

func normalizedTestStep(step runtimeTestStep) executionprogram.Step {
	return normalizeRuntimeTestStep(step)
}

func TestExpressionContinueOnErrorAppliesToPreparedActionFailure(t *testing.T) {
	step := normalizedTestStep(runtimeTestStep{ID: "action", ContinueOnErrorExpression: "${{ true }}"})
	execution := classifyStepExecutionWithControls(t.Context(), t.Context(), step, newResult(), errors.New("pre failed"), expression.Context{})
	if execution.outcome != "failure" || execution.conclusion != "success" {
		t.Fatalf("prepared action execution = %#v", execution)
	}
}

func TestExpressionContinueOnErrorIsNotEvaluatedForCancellation(t *testing.T) {
	jobCtx, cancel := context.WithCancel(t.Context())
	cancel()
	step := normalizedTestStep(runtimeTestStep{ID: "cancelled", ContinueOnErrorExpression: "${{ fromJSON('invalid') }}"})
	execution := classifyStepExecutionWithControls(jobCtx, jobCtx, step, newResult(), context.Canceled, expression.Context{})
	if execution.outcome != "cancelled" || execution.err != context.Canceled {
		t.Fatalf("cancelled execution = %#v", execution)
	}
}

func TestStepNameFailsClosedOnUnavailableBackgroundOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "background", Kind: "run", Background: true, Command: `echo "value=private" >> "$GITHUB_OUTPUT"`},
		{ID: "premature-reader", Name: `${{ steps.background.outputs.value }}`, Kind: "run", Command: `touch should-not-run`},
	})

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "name: expression references unavailable step") {
		t.Fatalf("RunJob() result = %#v, error = %v, want unavailable background output in name", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "should-not-run")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("premature reader command ran: %v", statErr)
	}
}

func TestJobConditionConsumesNeedResultAndOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
	job.Needs = map[string]plan.Need{"producer": {Result: "failure", Outputs: map[string]string{"gate": "yes"}}}
	job.Condition = "always() && needs.producer.result == 'failure' && needs.producer.outputs.gate"
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	job.Condition = ""
	job.Needs["producer"] = plan.Need{Result: "skipped"}
	result, err = (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "skipped" {
		t.Fatalf("RunJob() skipped prerequisite result = %#v, error = %v", result, err)
	}
}

func TestReusableWorkflowCallGuardsRunBeforeCapabilitiesAndJobConditions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "ran")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "run", Kind: "run", Condition: "always()", Command: "touch " + marker}})
	job.CallGuards = []plan.CallGuard{{Condition: "false"}, {Condition: "always()"}}
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"TOKEN"}
	job.Condition = "always()"

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "skipped" || len(result.Outputs) != 0 {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outer false guard allowed descendant always() step to run: %v", err)
	}
}

func TestReusableWorkflowCallGuardUsesOnlyCallerNeeds(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
	job.CallGuards = []plan.CallGuard{{
		Condition: "success() && needs.caller.result == 'success' && needs.caller.outputs.ready",
		Needs:     map[string]plan.Need{"caller": {Result: "success", Outputs: map[string]string{"ready": "true"}}},
	}}
	job.Needs = map[string]plan.Need{"internal": {Result: "failure"}}
	job.Condition = "always()"

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}

	job.CallGuards[0].Condition = ""
	job.CallGuards[0].Needs["caller"] = plan.Need{Result: "failure"}
	if err := job.Validate(); err == nil {
		t.Fatal("Validate() accepted an empty call guard condition")
	}
	job.CallGuards[0].Condition = "needs.caller.outputs.ready"
	result, err = (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "skipped" {
		t.Fatalf("implicit success guard result = %#v, error = %v", result, err)
	}
	job.CallGuards[0].Condition = "failure()"
	result, err = (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("failure() guard result = %#v, error = %v", result, err)
	}
}

func TestJobRuntimeFieldsEvaluateCompoundExpressions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:      "run",
		Kind:    "run",
		Env:     map[string]string{"SCOPE": "step"},
		Command: `test "$VALUE" = "release-linux-v1" && printf 'value=done\n' >> "$GITHUB_OUTPUT"`,
	}})
	job.Matrix = map[string]any{"os": "linux", "directory": "src"}
	job.Vars = map[string]string{"PREFIX": "release"}
	job.Needs = map[string]plan.Need{"producer": {Result: "success", Outputs: map[string]string{"tag": "v1"}}}
	job.Env = map[string]string{
		"ROOT":  workspace,
		"SCOPE": "job",
		"VALUE": "${{ format('{0}-{1}-{2}', vars.PREFIX, matrix.os, needs.producer.outputs.tag) }}",
	}
	job.DefaultShell = "${{ format('{0}', 'sh') }}"
	job.DefaultWorkingDirectory = "${{ format('{0}/{1}', env.ROOT, matrix.directory) }}"
	job.Outputs = map[string]string{
		"environment": "${{ format('{0}', env.SCOPE) }}",
		"result":      "${{ format('{0}-{1}', steps.run.outputs.value, needs.producer.result) }}",
	}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["result"] != "done-success" || result.Outputs["environment"] != "job" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestDecodedPlanMatrixNumbersDriveRuntimeConditions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	nonzeroMarker := filepath.Join(workspace, "nonzero")
	zeroMarker := filepath.Join(workspace, "zero")
	maxUintMarker := filepath.Join(workspace, "max-uint")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "nonzero", Kind: "run", Condition: "matrix.nonzero", Command: `touch "$NONZERO"`},
		{ID: "zero", Kind: "run", Condition: "matrix.zero", Command: `touch "$ZERO"`},
		{ID: "max-uint", Kind: "run", Condition: "matrix.max_uint != 0", Command: `touch "$MAX_UINT"`},
	})
	job.Condition = "matrix.count == 1"
	job.Matrix = map[string]any{"count": 1, "nonzero": 2, "zero": 0, "max_uint": ^uint64(0)}
	job.Env = map[string]string{"NONZERO": nonzeroMarker, "ZERO": zeroMarker, "MAX_UINT": maxUintMarker}
	attachTestProgram(&job)

	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := plan.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{}).runTestJob(t.Context(), decoded, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(nonzeroMarker); err != nil {
		t.Fatalf("nonzero matrix condition did not run: %v", err)
	}
	if _, err := os.Stat(zeroMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zero matrix condition unexpectedly ran: %v", err)
	}
	if _, err := os.Stat(maxUintMarker); err != nil {
		t.Fatalf("max uint64 matrix condition did not run: %v", err)
	}
}

func TestRunJobRejectsRegisteredSecretInOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"CANARY"}
	job.Outputs = map[string]string{"leak": "${{ secrets.CANARY }}"}
	_, err := (Runner{Secrets: testSecretResolver{"CANARY": "do-not-publish"}, Redactor: &testRedactor{}}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "contains a registered secret") || strings.Contains(err.Error(), "do-not-publish") {
		t.Fatalf("RunJob() error = %v, want non-disclosing secret-output rejection", err)
	}
}

func TestRunJobScrubsSecretFromRedactorFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"CANARY"}
	_, err := (Runner{Secrets: testSecretResolver{"CANARY": "do-not-leak"}, Redactor: failingTokenRedactor{token: "do-not-leak"}}).runTestJob(t.Context(), job, workspace)
	if err == nil || strings.Contains(err.Error(), "do-not-leak") || !strings.Contains(err.Error(), "***") {
		t.Fatalf("RunJob() error = %v, want scrubbed redactor failure", err)
	}
}

func TestRunJobMintsAndRedactsScopedGitHubWorkflowToken(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow token\n")
	const token = "ghs_scoped_workflow_token"
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID: "use-token", Kind: "run", Shell: "sh", Command: `test "$GH_TOKEN" = "ghs_scoped_workflow_token"
test "$ALIAS_TOKEN" = "$GH_TOKEN"
printf '%s %s\n' "$GH_TOKEN" "$ALIAS_TOKEN"`,
	}})
	job.Schema = plan.Schema
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "caller.yml", Permissions: map[string]string{"contents": "read", "pull_requests": "write"}, Aliases: []string{"TOKEN_ALIAS"}}
	job.Env = map[string]string{"GH_TOKEN": "${{ secrets.GITHUB_TOKEN }}", "ALIAS_TOKEN": "${{ secrets.TOKEN_ALIAS }}"}
	provider := &testWorkflowTokenProvider{token: token}
	redactor := &testRedactor{}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, WorkflowToken: provider, Redactor: redactor}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if provider.calls != 1 || provider.repository != job.Event.Repository || provider.workflow != "caller.yml" || !reflect.DeepEqual(provider.permissions, job.GitHubToken.Permissions) {
		t.Fatalf("token request = calls %d, repository %q, workflow %q, permissions %#v", provider.calls, provider.repository, provider.workflow, provider.permissions)
	}
	if !reflect.DeepEqual(redactor.values, []string{token}) {
		t.Fatalf("redacted values = %#v", redactor.values)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(fmt.Sprintf("%#v", result), token) || result.Env["GH_TOKEN"] != "***" || !strings.Contains(logs.String(), "***") {
		t.Fatalf("workflow token leaked: result = %#v, logs = %q", result, logs.String())
	}
}

func TestRunJobScrubsTokenSerializedByToJSONGitHub(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: serialized context\n")
	writeFixtureFile(t, workspace, ".github/actions/serialized/action.yml", `name: serialized context
runs:
  using: composite
  steps:
    - shell: sh
      env:
        GITHUB_CONTEXT: ${{ ToJson(GitHub) }}
      run: |
        compact=$(printf '%s' "$GITHUB_CONTEXT" | tr -d '\n')
        printf 'composite context: %s\n' "$compact"
        printf '::warning::composite context: %s\n' "$compact"
`)
	const token = "ghs_serialized_context_token"
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{
			ID: "serialize", Kind: "run", Shell: "sh",
			Env: map[string]string{"GITHUB_CONTEXT": "${{ toJSON(github) }}"},
			Command: `
compact=$(printf '%s' "$GITHUB_CONTEXT" | tr -d '\n')
printf 'context: %s\n' "$compact"
printf 'context error: %s\n' "$compact" >&2
printf '::warning::context: %s\n' "$compact"
printf '::error::context: %s\n' "$compact"
printf '%s\n' "$GITHUB_CONTEXT" >> "$GITHUB_STEP_SUMMARY"
printf 'SERIALIZED_CONTEXT<<EOF\n%s\nEOF\n' "$GITHUB_CONTEXT" >> "$GITHUB_ENV"
printf 'context<<EOF\n%s\nEOF\n' "$GITHUB_CONTEXT" >> "$GITHUB_OUTPUT"
`,
		},
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/serialized"},
	})
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"contents": "read"}}
	job.Outputs = map[string]string{"serialized": "${{ steps.serialize.outputs.context }}"}
	provider := &testWorkflowTokenProvider{token: token}
	redactor := &testRedactor{}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, WorkflowToken: provider, Redactor: redactor}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `job output "serialized" contains a registered secret`) {
		t.Fatalf("RunJob() error = %v, want serialized token output rejection", err)
	}
	if provider.calls != 1 || !reflect.DeepEqual(redactor.values, []string{token}) {
		t.Fatalf("token handling = provider calls %d, redactions %#v", provider.calls, redactor.values)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked serialized token: %v", err)
	}
	for name, value := range map[string]string{
		"logs":        logs.String(),
		"result":      fmt.Sprintf("%#v", result),
		"environment": result.Env["SERIALIZED_CONTEXT"],
		"summary":     result.Summary,
		"warnings":    result.WarningAnnotations,
		"annotations": result.ErrorAnnotations,
	} {
		if strings.Contains(value, token) || !strings.Contains(value, "***") {
			t.Errorf("%s was not scrubbed: %q", name, value)
		}
	}
}

func TestRunJobScrubsTokenSerializedIntoRuntimeError(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: serialized context error\n")
	const token = "ghs_serialized_error_token"
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID: "serialize", Kind: "run", Shell: "${{ toJSON(github) }}", Command: "true",
	}})
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"contents": "read"}}
	job.ContinueOnError = true
	provider := &testWorkflowTokenProvider{token: token}
	redactor := &testRedactor{}
	result, err := (Runner{WorkflowToken: provider, Redactor: redactor}).runTestJob(t.Context(), job, workspace)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") || !strings.Contains(err.Error(), "is unsupported") {
		t.Fatalf("RunJob() error = %v, want scrubbed unsupported-shell error", err)
	}
	if result.Conclusion != "success" || !IsToleratedJobFailure(err) {
		t.Fatalf("RunJob() result/error = %#v / %v, want preserved tolerated-failure classification", result, err)
	}
	if provider.calls != 1 || !reflect.DeepEqual(redactor.values, []string{token}) {
		t.Fatalf("token handling = provider calls %d, redactions %#v", provider.calls, redactor.values)
	}
}

func TestRunJobRejectsInvalidWorkflowTokenPolicyBeforeMinting(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/nested/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow token\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
	job.Schema = plan.Schema
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "nested/test.yml", Permissions: map[string]string{"contents": "read"}}
	provider := &testWorkflowTokenProvider{token: "must-not-be-minted"}
	_, err := (Runner{WorkflowToken: provider, Redactor: &testRedactor{}}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "simple .yml or .yaml filename") || provider.calls != 0 {
		t.Fatalf("RunJob() error/calls = %v / %d", err, provider.calls)
	}
}

func TestResolveActionInputsExposesScopedTokenToMetadataDefaults(t *testing.T) {
	tokenDefault := "${{ github.token }}"
	conditionalTokenDefault := "${{ github.server_url == 'https://github.com' && github.token || '' }}"
	actorDefault := "${{ github.actor }}"
	action := metadata.Metadata{Inputs: map[string]metadata.Input{
		"github_token":      {Default: &tokenDefault},
		"conditional_token": {Default: &conditionalTokenDefault},
		"actor":             {Default: &actorDefault},
	}}
	eval := expression.Context{
		GitHub:  map[string]any{"actor": "octocat", "server_url": "https://github.com"},
		Secrets: map[string]string{"GITHUB_TOKEN": "ghs_scoped_action_default"},
	}

	inputs, err := resolveActionInputs(action, nil, eval)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inputs, map[string]string{"github_token": "ghs_scoped_action_default", "conditional_token": "ghs_scoped_action_default", "actor": "octocat"}) {
		t.Fatalf("resolved action inputs = %#v", inputs)
	}
	if _, leaked := eval.GitHub["token"]; leaked {
		t.Fatalf("metadata default evaluation mutated the shared GitHub context: %#v", eval.GitHub)
	}
	if _, err := evaluateLegacyTemplate("${{ github.token }}", expression.ProfileRuntimeTemplate, eval); err == nil || !strings.Contains(err.Error(), `unavailable github value "token"`) {
		t.Fatalf("workflow github.token evaluation error = %v, want unavailable value", err)
	}

	inputs, err = resolveActionInputs(action, map[string]string{"GITHUB_TOKEN": ""}, eval)
	if err != nil {
		t.Fatal(err)
	}
	if inputs["github_token"] != "" {
		t.Fatalf("explicit empty input did not suppress metadata default: %#v", inputs)
	}

	withoutToken := eval
	withoutToken.Secrets = nil
	if _, err := resolveActionInputs(action, nil, withoutToken); err == nil || !strings.Contains(err.Error(), `unavailable github value "token"`) {
		t.Fatalf("unplanned metadata github.token evaluation error = %v, want unavailable value", err)
	}
	ghesAction := metadata.Metadata{Inputs: map[string]metadata.Input{"token": {Default: &conditionalTokenDefault}}}
	ghes := eval
	ghes.GitHub = map[string]any{"server_url": "https://github.example.com"}
	ghes.Secrets = nil
	inputs, err = resolveActionInputs(ghesAction, nil, ghes)
	if err != nil || inputs["token"] != "" {
		t.Fatalf("GHES conditional token input = %#v, %v, want empty token", inputs, err)
	}
}

func TestResolveActionInputsUsesContextDefaultsUnlessExplicitlySupplied(t *testing.T) {
	for _, test := range []struct {
		name, input, expression, supplied, defaultValue string
	}{
		{name: "runner debug", input: "debug", expression: "${{ runner.debug }}", supplied: "true", defaultValue: "false"},
		{name: "empty check run ID", input: "check-run-id", expression: "${{ job.check_run_id }}", supplied: "1234"},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := metadata.Metadata{Inputs: map[string]metadata.Input{test.input: {Default: &test.expression}}}
			inputs, err := resolveActionInputs(action, nil, expression.Context{})
			if err != nil || inputs[test.input] != test.defaultValue {
				t.Fatalf("context default = %#v, %v; want %q", inputs, err, test.defaultValue)
			}

			inputs, err = resolveActionInputs(action, map[string]string{strings.ToUpper(test.input): test.supplied}, expression.Context{})
			if err != nil || inputs[test.input] != test.supplied {
				t.Fatalf("explicit input = %#v, %v; want %q", inputs, err, test.supplied)
			}
		})
	}
}

func TestStepExpressionContextExposesScopedTokenWithoutMutatingJobContext(t *testing.T) {
	eval := expression.Context{
		GitHub:  map[string]any{"actor": "octocat"},
		Secrets: map[string]string{"GITHUB_TOKEN": "ghs_scoped_step"},
	}
	stepEval := stepExpressionContext(eval)
	value, err := evaluateLegacyTemplate("${{ github.token }}", expression.ProfileRuntimeTemplate, stepEval)
	if err != nil || value != "ghs_scoped_step" {
		t.Fatalf("step github.token = %q, %v", value, err)
	}
	if _, leaked := eval.GitHub["token"]; leaked {
		t.Fatalf("step evaluation mutated job context: %#v", eval.GitHub)
	}
}

func TestOriginUsesProviderServerURLWithoutGitHubToken(t *testing.T) {
	job := plan.Job{Event: plan.Event{Provider: "cursor-origin"}}
	github := githubContext(job)
	env := standardEnvironment(job, "/workspace", "/tmp", "/tool-cache", RunIdentity{})
	if github["server_url"] != "https://origin.cursor.com" || env["GITHUB_SERVER_URL"] != "https://origin.cursor.com" {
		t.Fatalf("Origin server URLs = context %#v, environment %q", github["server_url"], env["GITHUB_SERVER_URL"])
	}
	conditionalTokenDefault := "${{ github.server_url == 'https://github.com' && github.token || '' }}"
	action := metadata.Metadata{Inputs: map[string]metadata.Input{"token": {Default: &conditionalTokenDefault}}}
	inputs, err := resolveActionInputs(action, nil, expression.Context{GitHub: github})
	if err != nil || inputs["token"] != "" {
		t.Fatalf("Origin conditional token input = %#v, %v, want empty token", inputs, err)
	}
}

func TestGitHubContextExposesRuntimeEventIdentity(t *testing.T) {
	tests := []struct {
		name        string
		event       plan.Event
		wantOwner   string
		wantRefName string
		wantRefType string
		wantBaseRef string
	}{
		{name: "branch", event: plan.Event{Repository: "acme/widgets", Ref: "refs/heads/feature/runtime"}, wantOwner: "acme", wantRefName: "feature/runtime", wantRefType: "branch"},
		{name: "tag", event: plan.Event{Repository: "acme/widgets", Ref: "refs/tags/v1.2.3"}, wantOwner: "acme", wantRefName: "v1.2.3", wantRefType: "tag"},
		{name: "release", event: plan.Event{Name: "release", Repository: "acme/widgets", Ref: "refs/tags/v1.2.3", SHA: strings.Repeat("a", 40)}, wantOwner: "acme", wantRefName: "v1.2.3", wantRefType: "tag"},
		{name: "pull request merge", event: plan.Event{Repository: "acme/widgets", Ref: "refs/pull/42/merge", HeadRef: "feature/runtime", BaseRef: "main"}, wantOwner: "acme", wantRefName: "42/merge", wantRefType: "branch", wantBaseRef: "main"},
		{name: "pull request head", event: plan.Event{Repository: "acme/widgets", Ref: "refs/pull/42/head", HeadRef: "feature/runtime", BaseRef: "main"}, wantOwner: "acme", wantRefName: "42/head", wantRefType: "branch", wantBaseRef: "main"},
		{name: "unavailable", event: plan.Event{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			github := githubContext(plan.Job{Event: test.event})
			if github["repository_owner"] != test.wantOwner || github["ref_name"] != test.wantRefName || github["ref_type"] != test.wantRefType || github["base_ref"] != test.wantBaseRef || github["action_path"] != "" || github["action_ref"] != "" || github["action_repository"] != "" {
				t.Fatalf("GitHub context = %#v", github)
			}
			stepContext := stepExpressionContext(expression.Context{GitHub: github, Secrets: map[string]string{"GITHUB_TOKEN": "ghs_test"}})
			serialized, err := evaluateLegacyTemplate("${{ toJSON(github) }}", expression.ProfileStepTemplate, stepContext)
			if err != nil || !strings.Contains(serialized, `"action_path": ""`) || !strings.Contains(serialized, `"action_ref": ""`) || !strings.Contains(serialized, `"action_repository": ""`) {
				t.Fatalf("serialized top-level GitHub context = %q, %v", serialized, err)
			}
			condition := "github.repository_owner == '" + test.wantOwner + "' && github.ref_name == '" + test.wantRefName + "' && github.ref_type == '" + test.wantRefType + "' && github.base_ref == '" + test.wantBaseRef + "'"
			value, err := expression.NewEngine().Evaluate(expression.Site{Source: condition, Profile: expression.ProfileStepCondition, Result: expression.ResultBoolean, Purpose: expression.PurposeExpression}, expression.Values{Condition: expression.ConditionContext{GitHub: github}})
			got, _ := value.(bool)
			if err != nil || !got {
				t.Fatalf("condition %q = %v, %v", condition, got, err)
			}
			if test.name == "release" {
				env := standardEnvironment(plan.Job{Event: test.event}, "/workspace", "/tmp", "/tool-cache", RunIdentity{})
				if env["GITHUB_EVENT_NAME"] != "release" || env["GITHUB_REF"] != "refs/tags/v1.2.3" || env["GITHUB_SHA"] != strings.Repeat("a", 40) {
					t.Fatalf("release environment = %#v", env)
				}
			}
		})
	}
}

func TestStandardEnvironmentSuppliesProtectedGitHubWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		workflow plan.Workflow
		want     string
	}{
		{name: "declared name", workflow: plan.Workflow{Path: ".github/workflows/ci.yml", Name: "CI"}, want: "CI"},
		{name: "path fallback", workflow: plan.Workflow{Path: ".github/workflows/ci.yml"}, want: ".github/workflows/ci.yml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := standardEnvironment(plan.Job{Workflow: test.workflow}, "/workspace", "/tmp", "/tool-cache", RunIdentity{})
			if got := env["GITHUB_WORKFLOW"]; got != test.want {
				t.Fatalf("GITHUB_WORKFLOW = %q, want %q", got, test.want)
			}
			merged := mergeStepEnvironment(env, map[string]string{"GITHUB_WORKFLOW": "spoofed"})
			if got := merged["GITHUB_WORKFLOW"]; got != test.want {
				t.Fatalf("overlaid GITHUB_WORKFLOW = %q, want protected value %q", got, test.want)
			}
		})
	}
}

func TestRuntimeWorkflowIdentityUsesImmutableCallerPlanData(t *testing.T) {
	event := plan.Event{
		Repository: "acme/widgets",
		Ref:        "refs/heads/main",
		SHA:        strings.Repeat("a", 40),
	}
	tests := []struct {
		name     string
		workflow plan.Workflow
	}{
		{
			name:     "direct workflow",
			workflow: plan.Workflow{Path: "./.github/workflows/caller.yml"},
		},
		{
			name:     "local reusable workflow",
			workflow: plan.Workflow{Path: "./.github/workflows/reusable.yml", RunPath: "./.github/workflows/caller.yml"},
		},
		{
			name: "remote reusable workflow",
			workflow: plan.Workflow{
				Path:    "shared/workflows/.github/workflows/reusable.yml@v2",
				RunPath: "./.github/workflows/caller.yml",
				Remote: &plan.RemoteWorkflowSource{
					Repository: "shared/workflows", RequestedRef: "v2", Commit: strings.Repeat("b", 40),
				},
			},
		},
	}
	wantRef := "acme/widgets/.github/workflows/caller.yml@refs/heads/main"
	wantSHA := strings.Repeat("a", 40)
	engine := expression.NewEngine()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := plan.Job{Workflow: test.workflow, Event: event}
			github := githubContext(job)
			if github["workflow_ref"] != wantRef || github["workflow_sha"] != wantSHA {
				t.Fatalf("GitHub workflow identity = %#v / %#v, want %q / %q", github["workflow_ref"], github["workflow_sha"], wantRef, wantSHA)
			}
			for expressionSource, want := range map[string]string{
				"${{ github.workflow_ref }}": wantRef,
				"${{ github.workflow_sha }}": wantSHA,
			} {
				value, err := engine.Evaluate(
					expression.Site{Source: expressionSource, Profile: expression.ProfileStepTemplate, Result: expression.ResultString, Purpose: expression.PurposeExpression},
					expression.Values{Runtime: stepExpressionContext(expression.Context{GitHub: github})},
				)
				got, _ := value.(string)
				if err != nil || got != want {
					t.Fatalf("EvaluateStep(%q) = %q, %v, want %q", expressionSource, got, err, want)
				}
			}
			condition := "github.workflow_ref == '" + wantRef + "' && github.workflow_sha == '" + wantSHA + "'"
			value, err := engine.Evaluate(
				expression.Site{Source: condition, Profile: expression.ProfileStepCondition, Result: expression.ResultBoolean, Purpose: expression.PurposeExpression},
				expression.Values{Condition: expression.ConditionContext{GitHub: github}},
			)
			got, _ := value.(bool)
			if err != nil || !got {
				t.Fatalf("EvaluateCondition(%q) = %v, %v", condition, got, err)
			}

			env := standardEnvironment(job, "/workspace", "/tmp", "/tool-cache", RunIdentity{})
			if env["GITHUB_WORKFLOW_REF"] != wantRef || env["GITHUB_WORKFLOW_SHA"] != wantSHA {
				t.Fatalf("workflow environment = %q / %q, want %q / %q", env["GITHUB_WORKFLOW_REF"], env["GITHUB_WORKFLOW_SHA"], wantRef, wantSHA)
			}
			merged := mergeStepEnvironment(env, map[string]string{
				"GITHUB_WORKFLOW_REF": "spoofed-ref",
				"GITHUB_WORKFLOW_SHA": "spoofed-sha",
			})
			if merged["GITHUB_WORKFLOW_REF"] != wantRef || merged["GITHUB_WORKFLOW_SHA"] != wantSHA {
				t.Fatalf("protected workflow environment = %q / %q, want %q / %q", merged["GITHUB_WORKFLOW_REF"], merged["GITHUB_WORKFLOW_SHA"], wantRef, wantSHA)
			}
		})
	}
}

func TestRunJobSuppliesScopedGitHubTokenToEffectiveActionDefault(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "test.yml")
	workflow := []byte(`on: push
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/token

      - if: env.GITHUB_SHA == 'action-sha' && env.RUNNER_TEMP == '/action-temp'
        env:
          DIRECT_GITHUB_TOKEN: ${{ github.token }}
          ENV_GITHUB_SHA: ${{ env.GITHUB_SHA }}
          ENV_RUNNER_TEMP: ${{ env.RUNNER_TEMP }}
        run: |
          test "$GITHUB_TOKEN" = "ghs_scoped_action_default"
          test "$DIRECT_GITHUB_TOKEN" = "ghs_scoped_action_default"
          test "$GITHUB_SHA" = "1111111111111111111111111111111111111111"
          test "$RUNNER_TEMP" = "$EXPECTED_RUNNER_TEMP"
          test "$ENV_GITHUB_SHA" = "action-sha"
          test "$ENV_RUNNER_TEMP" = "/action-temp"
          printf 'exported token: %s\n' "$GITHUB_TOKEN"

      - uses: ./.github/actions/observer

      - env:
          GITHUB_ACTION_PATH: step-action-path
          GITHUB_TOKEN: step-token
          GITHUB_SHA: step-sha
          RUNNER_TEMP: /step-temp
          ENV_GITHUB_ACTION_PATH: ${{ env.GITHUB_ACTION_PATH }}
          ENV_GITHUB_SHA: ${{ env.GITHUB_SHA }}
          ENV_RUNNER_TEMP: ${{ env.RUNNER_TEMP }}
        run: |
          test "$GITHUB_ACTION_PATH" = "step-action-path"
          test "$GITHUB_TOKEN" = "step-token"
          test "$GITHUB_SHA" = "1111111111111111111111111111111111111111"
          test "$RUNNER_TEMP" = "$EXPECTED_RUNNER_TEMP"
          test "$ENV_GITHUB_ACTION_PATH" = "file-action-path"
          test "$ENV_GITHUB_SHA" = "action-sha"
          test "$ENV_RUNNER_TEMP" = "/action-temp"
`)
	writeFixtureFile(t, workspace, ".github/actions/token/action.yml", `name: token default
inputs:
  github_token:
    default: ${{ github.server_url == 'https://github.com' && github.token || '' }}
runs:
  using: node24
  main: dist/index.js
`)
	writeFixtureFile(t, workspace, ".github/actions/token/dist/index.js", `const fs = require("node:fs");
if (process.env.GITHUB_TOKEN !== undefined) throw new Error("GITHUB_TOKEN was injected before the action exported it");
if (process.env.INPUT_GITHUB_TOKEN !== "ghs_scoped_action_default") throw new Error("scoped token input was not provided");
fs.appendFileSync(process.env.GITHUB_ENV,
  "GITHUB_TOKEN=" + process.env.INPUT_GITHUB_TOKEN + "\n" +
  "GITHUB_ACTION_PATH=file-action-path\n" +
  "GITHUB_SHA=action-sha\n" +
  "RUNNER_TEMP=/action-temp\n" +
  "EXPECTED_RUNNER_TEMP=" + process.env.RUNNER_TEMP + "\n");
`)
	writeFixtureFile(t, workspace, ".github/actions/observer/action.yml", `name: context observer
runs:
  using: composite
  steps:
    - shell: sh
      env:
        ENV_GITHUB_ACTION_PATH: ${{ env.GITHUB_ACTION_PATH }}
        ENV_GITHUB_SHA: ${{ env.GITHUB_SHA }}
        ENV_RUNNER_TEMP: ${{ env.RUNNER_TEMP }}
      run: |
        test "$GITHUB_TOKEN" = "ghs_scoped_action_default"
        test "$GITHUB_ACTION_PATH" != "file-action-path"
        test -f "$GITHUB_ACTION_PATH/action.yml"
        test "$GITHUB_SHA" = "1111111111111111111111111111111111111111"
        test "$RUNNER_TEMP" = "$EXPECTED_RUNNER_TEMP"
        test "$ENV_GITHUB_ACTION_PATH" = "file-action-path"
        test "$ENV_GITHUB_SHA" = "action-sha"
        test "$ENV_RUNNER_TEMP" = "/action-temp"
`)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", string(workflow))
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(t.Context(), workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels:                     map[string]string{"ubuntu-latest": ""},
			AllowUntrustedDefaultQueue: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil {
		t.Fatalf("compiled scoped token plan = %#v", plans)
	}
	provider := &testWorkflowTokenProvider{token: "ghs_scoped_action_default"}
	redactor := &testRedactor{}
	var logs bytes.Buffer
	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs, WorkflowToken: provider, Redactor: redactor}).runTestJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if provider.calls != 1 || !reflect.DeepEqual(provider.permissions, map[string]string{"contents": "read"}) || !reflect.DeepEqual(redactor.values, []string{"ghs_scoped_action_default"}) {
		t.Fatalf("token handling = provider calls %d, permissions %#v, redactions %#v", provider.calls, provider.permissions, redactor.values)
	}
	if result.Env["GITHUB_TOKEN"] != "***" || result.Env["GITHUB_SHA"] != "action-sha" || result.Env["RUNNER_TEMP"] != "/action-temp" || strings.Contains(logs.String(), "ghs_scoped_action_default") || !strings.Contains(logs.String(), "exported token: ***") {
		t.Fatalf("exported workflow token leaked: result = %#v, logs = %q", result, logs.String())
	}
}

func TestCompileAndRunJobDiscardsTopLevelActionEnv(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "test.yml")
	workflow := []byte(`on: push
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/release
        env:
          GITHUB_TOKEN: workflow-step-token
          PRESERVED: workflow-step-env
`)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", string(workflow))
	writeFixtureFile(t, workspace, ".github/actions/release/action.yml", `name: GH Release
env:
  GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
  METADATA_ONLY: ${{ secrets.DEPLOY_TOKEN }}
runs:
  using: composite
  steps:
    - shell: sh
      run: |
        test "$GITHUB_TOKEN" = workflow-step-token
        test "$PRESERVED" = workflow-step-env
        test -z "${METADATA_ONLY:-}"
`)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken != nil || len(plans[0].RequiredSecrets) != 0 || plans[0].HasCapability("provider-token-write") || plans[0].HasCapability("secrets") {
		t.Fatalf("top-level action env added authority to plan: %#v", plans)
	}
	encoded, err := json.Marshal(plans[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "DEPLOY_TOKEN") {
		t.Fatalf("top-level action env authority leaked into plan: %s", encoded)
	}
	for _, action := range plans[0].Program.Actions {
		if _, leaked := action.Metadata("", "").Runs.Env["METADATA_ONLY"]; leaked {
			t.Fatal("top-level action env leaked into normalized action environment")
		}
	}

	provider := &testWorkflowTokenProvider{token: "must-not-be-minted"}
	result, err := (Runner{WorkflowToken: provider, Secrets: testSecretResolver{}}).runTestJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if provider.calls != 0 {
		t.Fatalf("top-level action env requested %d workflow tokens", provider.calls)
	}
}

func TestRunJobAbortsAndScrubsWorkflowTokenWhenRedactionFails(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow token\n")
	const token = "ghs_redaction_failure_token"
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "never", Kind: "run", Command: "false"}})
	job.Schema = plan.Schema
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"pull_requests": "write"}}
	_, err := (Runner{WorkflowToken: &testWorkflowTokenProvider{token: token}, Redactor: failingTokenRedactor{token: token}}).runTestJob(t.Context(), job, workspace)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("RunJob() error = %v, want redaction failure without token disclosure", err)
	}
}

func TestRunJobRequiresHydratedStaticDependencyResults(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: dependency boundary\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
	job.Dependencies = []string{"gha-producer"}
	job.NeedSources = map[string][]plan.NeedSource{"producer": {{StepKey: "gha-producer", PlanDigest: "sha256:" + strings.Repeat("1", 64)}}}
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "no hydrated prerequisite results") {
		t.Fatalf("RunJob() error = %v, want missing hydration rejection", err)
	}
	job.Needs = map[string]plan.Need{"producer": {Result: "success"}}
	if result, err := (Runner{}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() hydrated result = %#v, error = %v", result, err)
	}
}

func TestOversizedStepSummaryIsNonFatalAndDiscarded(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "oversized", Kind: "run", Shell: "sh", Command: `
printf 'SUMMARY_EFFECT=preserved\n' >> "$GITHUB_ENV"
head -c "$SUMMARY_BYTES" /dev/zero | tr '\000' x >> "$GITHUB_STEP_SUMMARY"`},
		{ID: "after", Kind: "run", Shell: "sh", Command: `
test "$SUMMARY_EFFECT" = preserved
printf 'retained summary\n' >> "$GITHUB_STEP_SUMMARY"`},
	})
	job.Env = map[string]string{"SUMMARY_BYTES": strconv.Itoa(maxCommandFileBytes + 1)}
	var logs bytes.Buffer

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Env["SUMMARY_EFFECT"] != "preserved" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if result.Summary != "retained summary\n" || !strings.Contains(logs.String(), "GITHUB_STEP_SUMMARY upload skipped") {
		t.Fatalf("RunJob() summary = %q, logs = %q", result.Summary, logs.String())
	}
}

func TestJobTimeoutDuringSetupIsCancelled(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: setup timeout\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
	job.ContinueOnError = true
	job.TimeoutMinutes = 0.001
	attachTestProgram(&job)
	runner := Runner{ResolveMise: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}

	result, err := runner.runTestJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || IsToleratedJobFailure(err) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunnerToolCacheIsPerJobAndReserved(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:      "tool-cache",
		Kind:    "run",
		Command: `test -d "$RUNNER_TOOL_CACHE"; case "$RUNNER_TOOL_CACHE" in "$RUNNER_TEMP"/*) ;; *) exit 9 ;; esac`,
	}})
	job.Env = map[string]string{"RUNNER_TOOL_CACHE": filepath.Join(workspace, "untrusted")}
	if result, err := (Runner{}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunnerUsesConfiguredToolCache(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	toolCache, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:      "tool-cache",
		Kind:    "run",
		Command: `test "$RUNNER_TOOL_CACHE" = "$EXPECTED_TOOL_CACHE"`,
	}})
	job.Env = map[string]string{"EXPECTED_TOOL_CACHE": toolCache}
	if result, err := (Runner{ToolCache: toolCache}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestUbuntuImageOS(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "Ubuntu 24", source: "ID=ubuntu\nVERSION_ID=\"24.04\"\n", want: "ubuntu24"},
		{name: "Ubuntu 22", source: "VERSION_ID='22.04'\nID=ubuntu\n", want: "ubuntu22"},
		{name: "other distribution", source: "ID=debian\nVERSION_ID=12\n"},
		{name: "missing version", source: "ID=ubuntu\n"},
		{name: "non-LTS version", source: "ID=ubuntu\nVERSION_ID=24.10\n"},
		{name: "invalid version", source: "ID=ubuntu\nVERSION_ID=current\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ubuntuImageOS([]byte(test.source)); got != test.want {
				t.Fatalf("ubuntuImageOS() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestImageOSUsesNormalEnvironmentPrecedence(t *testing.T) {
	if got := mergeStepEnvironment(map[string]string{"ImageOS": "ubuntu24"}, map[string]string{"ImageOS": "workflow-controlled"}); got["ImageOS"] != "workflow-controlled" {
		t.Fatalf("ImageOS did not use normal workflow environment precedence: %#v", got)
	}
}

func TestCanonicalRunnerContext(t *testing.T) {
	for _, test := range []struct {
		goos, goarch, os, arch string
	}{
		{goos: "linux", goarch: "amd64", os: "Linux", arch: "X64"},
		{goos: "darwin", goarch: "arm64", os: "macOS", arch: "ARM64"},
	} {
		got, err := canonicalRunnerContext(test.goos, test.goarch)
		if err != nil || got["os"] != test.os || got["arch"] != test.arch {
			t.Errorf("canonicalRunnerContext(%s, %s) = %#v, %v", test.goos, test.goarch, got, err)
		}
	}
	if _, err := canonicalRunnerContext("linux", "arm64"); err == nil {
		t.Fatal("canonicalRunnerContext() accepted unsupported pair")
	}
}

func TestValidateHostRejectsDockerOnDarwin(t *testing.T) {
	job := plan.Job{RequiredCapabilities: []string{"docker", "network"}}
	if err := ValidateHost(job, "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "unsupported on macOS") {
		t.Fatalf("ValidateHost() Darwin Docker error = %v", err)
	}
	if err := ValidateHost(job, "linux", "amd64"); err != nil {
		t.Fatalf("ValidateHost() Linux Docker error = %v", err)
	}
	if err := ValidateHost(job, "darwin", "amd64"); err == nil || !strings.Contains(err.Error(), "unsupported runner platform") {
		t.Fatalf("ValidateHost() unsupported platform error = %v", err)
	}
}

func TestRunnerEnvironmentIsProtected(t *testing.T) {
	base := map[string]string{"RUNNER_OS": "Linux", "RUNNER_ARCH": "X64"}
	got := mergeStepEnvironment(base, map[string]string{"RUNNER_OS": "overridden", "RUNNER_ARCH": "overridden"})
	if got["RUNNER_OS"] != "Linux" || got["RUNNER_ARCH"] != "X64" {
		t.Fatalf("runner environment was overridden: %#v", got)
	}
}

func TestJavaScriptInputEnvironmentMatchesToolkitNames(t *testing.T) {
	env := actionInputEnv(map[string]string{"node-version": "24", "two words": "value"})
	if env["INPUT_NODE-VERSION"] != "24" || env["INPUT_TWO_WORDS"] != "value" {
		t.Fatalf("actionInputEnv() = %#v", env)
	}
	if _, ok := env["INPUT_NODE_VERSION"]; ok {
		t.Fatalf("actionInputEnv() rewrote a hyphen: %#v", env)
	}
}

func TestPriorFailureSkipsLaterStepEnvironmentWithoutStatusCondition(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
		{ID: "fail", Kind: "run", Command: "exit 1"},
		{ID: "skipped", Kind: "run", Command: "exit 1", Env: map[string]string{"INVALID": "${{ fromJSON('invalid') }}"}},
	})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(err.Error(), "environment") {
		t.Fatalf("failed job evaluated a skipped step environment: %v", err)
	}
}

func TestCancellationSkipsLaterStepEnvironmentWithoutStatusCondition(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
		{ID: "cancel", Kind: "run", Command: "sleep 30"},
		{ID: "skipped", Kind: "run", Command: "exit 1", Env: map[string]string{"INVALID": "${{ fromJSON('invalid') }}"}},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := (Runner{}).runTestJob(ctx, job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(err.Error(), "environment") {
		t.Fatalf("cancelled job evaluated a skipped step environment: %v", err)
	}
}

func TestServiceContainerExpressionErrorsUseFieldOrder(t *testing.T) {
	invalid := testProgramSite("${{", executionprogram.SurfaceServiceTemplate, executionprogram.ResultString)
	container := executionprogram.ServiceContainer{
		Image:      testProgramSite("postgres:16", executionprogram.SurfaceServiceTemplate, executionprogram.ResultString),
		Options:    invalid,
		Command:    invalid,
		Entrypoint: invalid,
	}
	for range 20 {
		_, err := evaluateProgramServiceContainer(container, expression.Context{})
		if err == nil || !strings.HasPrefix(err.Error(), "options:") {
			t.Fatalf("evaluateProgramServiceContainer() error = %v, want options first", err)
		}
	}
}

func TestJobSummaryTruncationPreservesUTF8(t *testing.T) {
	var summary string
	var truncated bool
	prefixBytes := maxJobSummaryBytes - 1
	appendJobSummary(&summary, &truncated, strings.Repeat("a", prefixBytes), false)
	appendJobSummary(&summary, &truncated, "€ and omitted text", false)
	summary = finalizeJobSummary(summary, truncated)

	if !truncated || len(summary) > maxJobSummaryBytes || !utf8.ValidString(summary) {
		t.Fatalf("summary truncated = %v, bytes = %d, valid UTF-8 = %v", truncated, len(summary), utf8.ValidString(summary))
	}
	if strings.Contains(summary, "€") || !strings.HasSuffix(summary, jobSummaryTruncationNotice) {
		t.Fatalf("summary split a UTF-8 rune or omitted its truncation notice")
	}
}

func TestJobSummaryRemainsBoundedAfterSecretScrubbing(t *testing.T) {
	secret := "xx"
	result := JobResult{Summary: strings.Repeat(secret, maxJobSummaryBytes/len(secret))}
	result = scrubJobResult(result, []string{secret})

	if len(result.Summary) > maxJobSummaryBytes || !utf8.ValidString(result.Summary) {
		t.Fatalf("scrubbed summary bytes = %d, valid UTF-8 = %v", len(result.Summary), utf8.ValidString(result.Summary))
	}
	if strings.Contains(result.Summary, secret) || !strings.HasSuffix(result.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("scrubbed summary leaked the secret or omitted its truncation notice")
	}
}

func TestTruncatedJobSummaryDoesNotExposePartialSecret(t *testing.T) {
	secret := "partial-secret-token"
	prefixBytes := maxJobSummaryBytes - len(jobSummaryTruncationNotice)
	visibleSecretBytes := len(secret) / 2
	contents := strings.Repeat("a", prefixBytes-visibleSecretBytes) + secret + strings.Repeat("z", len(jobSummaryTruncationNotice))
	result := JobResult{}
	appendJobSummary(&result.Summary, &result.summaryTruncated, contents, false)

	result = scrubJobResult(result, []string{secret})
	if strings.Contains(result.Summary, secret[:visibleSecretBytes]) || !strings.HasSuffix(result.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("truncated summary retained a partial secret suffix")
	}
}

func TestJobSummarySecretScrubbingIsOrderIndependent(t *testing.T) {
	first := scrubJobResult(JobResult{Summary: "overlapping-secret"}, []string{"overlapping", "overlapping-secret"})
	second := scrubJobResult(JobResult{Summary: "overlapping-secret"}, []string{"overlapping-secret", "overlapping"})
	if first.Summary != "***" || second.Summary != first.Summary {
		t.Fatalf("scrubbed summaries = %q and %q, want deterministic longest match", first.Summary, second.Summary)
	}
}

func TestRunJobLogsSynchronousStepSectionsAndExpandsFailures(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: step sections\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "success", Name: "Build (${{ matrix.version }})\n+++ injected", Kind: "run", Shell: "sh", Command: "echo built"},
		{ID: "skipped", Name: "Must not appear", Kind: "run", Shell: "sh", Condition: "false", Command: "echo skipped"},
		{ID: "failure", Kind: "run", Shell: "sh", Command: "echo broken; exit 1"},
	})
	job.Matrix = map[string]any{"version": "1.26"}
	var logs bytes.Buffer

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	got := logs.String()
	for _, fragment := range []string{"--- Build (1.26) +++ injected\n", "built\n", "--- failure\n", "broken\n", "^^^ +++\n"} {
		if !strings.Contains(got, fragment) {
			t.Errorf("logs lack %q: %q", fragment, got)
		}
	}
	if strings.Contains(got, "Must not appear") || strings.Index(got, "^^^ +++") < strings.Index(got, "broken") {
		t.Fatalf("logs contain a skipped heading or expand before failure output: %q", got)
	}
}

func TestInvalidWorkflowCommandStopTokenFailsTheStep(t *testing.T) {
	for _, token := range []string{"warning", "add-matcher", "remove-matcher"} {
		t.Run(token, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: invalid workflow command stop token\n")
			job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
				ID: "invalid-stop", Kind: "run", Shell: "sh",
				Command: fmt.Sprintf("printf '%%s\\n' '::stop-commands::%s'\nprintf '%%s\\n' '::warning::commands remain active'", token),
			}})
			var logs bytes.Buffer

			result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
			if err == nil || !strings.Contains(err.Error(), "invalid ::stop-commands workflow command") || result.Conclusion != "failure" {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			if !strings.Contains(result.ErrorAnnotations, "invalid ::stop-commands token") || !strings.Contains(result.WarningAnnotations, "commands remain active") {
				t.Fatalf("RunJob() warnings = %q, errors = %q", result.WarningAnnotations, result.ErrorAnnotations)
			}
			if !strings.Contains(logs.String(), "error: invalid ::stop-commands token") {
				t.Fatalf("RunJob() logs = %q, want invalid-token diagnostic", logs.String())
			}
		})
	}
}

func TestRunJobCollectsWarningAndErrorCommandsWithoutChangingConclusion(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow command annotations\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID: "diagnostics", Kind: "run", Shell: "sh",
		Command: "printf '%s\\n' '::warning title=Compiler::warning from stdout'\nprintf '%s\\n' '::error file=main.go,line=7::error from stderr' >&2",
	}})
	var stdout, stderr bytes.Buffer

	result, err := (Runner{Stdout: &stdout, Stderr: &stderr}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(result.WarningAnnotations, "warning from stdout") || !strings.Contains(result.ErrorAnnotations, "error from stderr") {
		t.Fatalf("RunJob() warnings = %q, errors = %q", result.WarningAnnotations, result.ErrorAnnotations)
	}
	if strings.Contains(stdout.String(), "::warning") || strings.Contains(stderr.String(), "::error") {
		t.Fatalf("RunJob() echoed raw workflow command: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRunJobShellJavaScriptCompositeAndPost(t *testing.T) {
	node := requireNode24(t)
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []runtimeTestStep{
		{ID: "shell", Kind: "run", Shell: "bash", Command: `echo "result=smoke" >> "$GITHUB_OUTPUT"`},
		{ID: "javascript", Name: "JavaScript", Kind: "uses", Uses: "./.github/actions/javascript", With: map[string]string{"message": "${{ steps.shell.outputs.result }}"}},
		{ID: "composite", Name: "Composite", Kind: "uses", Uses: "./.github/actions/composite", With: map[string]string{"message": "${{ steps.javascript.outputs.result }}"}},
		{ID: "verify", Kind: "run", Shell: "bash", Command: `test "$SMOKE_COMPOSITE_SEEN" = true`},
	})
	job.Outputs = map[string]string{"result": "${{ steps.composite.outputs.result }}"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Node24: node}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v\nlogs:\n%s", err, logs.String())
	}
	if result.Outputs["result"] != "smoke-javascript-composite" || result.Env["SMOKE_COMPOSITE_SEEN"] != "true" || result.State["phase"] != "main" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if strings.Contains(logs.String(), "smoke-mask-value") || !strings.Contains(logs.String(), "masked probe: ***") {
		t.Fatalf("RunJob() logs were not masked: %q", logs.String())
	}
	if post, verify := strings.Index(logs.String(), "JavaScript post phase completed"), strings.Index(logs.String(), "masked probe: ***"); post < verify {
		t.Fatalf("post action did not run after main steps: %q", logs.String())
	}
	if !strings.Contains(result.Summary, "main phase") || !strings.Contains(result.Summary, "post phase") {
		t.Fatalf("RunJob() summary = %q", result.Summary)
	}
}

func TestRunJobRejectsDynamicallyMaskedJobOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:    "derive",
		Kind:  "run",
		Shell: "sh",
		Command: `printf '%s\n' '::add-mask::derived-mask-value'
printf '%s\n' 'secret=derived-mask-value' >> "$GITHUB_OUTPUT"
printf '%s\n' 'DYNAMIC_VALUE=derived-mask-value' >> "$GITHUB_ENV"
printf '%s\n' 'derived-mask-value' >> "$GITHUB_STEP_SUMMARY"`,
	}})
	job.Outputs = map[string]string{"secret": "${{ steps.derive.outputs.secret }}"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `job output "secret" contains a registered secret`) {
		t.Fatalf("RunJob() error = %v, want dynamically masked output rejection", err)
	}
	if strings.Contains(logs.String(), "derived-mask-value") || strings.Contains(fmt.Sprintf("%#v", result), "derived-mask-value") {
		t.Fatalf("RunJob() leaked dynamically masked value: result = %#v, logs = %q", result, logs.String())
	}
	if result.Env["DYNAMIC_VALUE"] != "***" || result.Summary != "***\n" {
		t.Fatalf("RunJob() did not scrub dynamic mask from bounded result: %#v", result)
	}
}

func TestCompositeExposesOnlyDeclaredOutputs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	actionPath := ".github/actions/composite/action.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, actionPath, `name: Output-scoped composite
outputs:
  public:
    value: ${{ steps.inner.outputs.public }}
runs:
  using: composite
  steps:
    - id: inner
      shell: sh
      run: |
        printf '%s\n' 'public=visible' >> "$GITHUB_OUTPUT"
        printf '%s\n' 'private=hidden' >> "$GITHUB_OUTPUT"
        printf '%s\n' 'COMPOSITE_ENV=propagated' >> "$GITHUB_ENV"
        printf '%s\n' 'composite summary' >> "$GITHUB_STEP_SUMMARY"
`)
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	job.Outputs = map[string]string{
		"private": "${{ steps.composite.outputs.private }}",
		"public":  "${{ steps.composite.outputs.public }}",
	}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Outputs["public"] != "visible" || result.Outputs["private"] != "" {
		t.Fatalf("RunJob() outputs = %#v, want only declared composite output", result.Outputs)
	}
	if result.Env["COMPOSITE_ENV"] != "propagated" || result.Summary != "composite summary\n" {
		t.Fatalf("RunJob() effects = %#v, summary = %q", result.Env, result.Summary)
	}
}

func TestNestedCompositeEvaluatesEnvironmentOnceAndIsolatesStepScopes(t *testing.T) {
	workspace := canonicalTempDir(t)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/js/action.yml", `name: Nested JavaScript
inputs:
  message:
    required: true
runs:
  using: node20
  main: main.js
`)
	writeFixtureFile(t, workspace, ".github/actions/js/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/inner/action.yml", `name: Inner
outputs:
  result:
    value: ${{ steps.same.outputs.result }}
runs:
  using: composite
  steps:
    - id: verify-path
      shell: sh
      run: test "$GITHUB_ACTION_PATH" = "$EXPECTED_INNER_PATH"
    - id: same
      uses: ./.github/actions/js
      with:
        message: nested-input
      env:
        VALUE: ${{ env.TEMPLATE }}
        DYNAMIC_COPY: ${{ env.DYNAMIC }}
        CHILD_ONLY: private
`)
	writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", `name: Outer
inputs:
  parent-only:
    required: true
outputs:
  result:
    value: ${{ steps.nested.outputs.result }}
runs:
  using: composite
  steps:
    - id: same
      shell: sh
      run: |
        printf '%s\n' 'DYNAMIC=from-env-file' >> "$GITHUB_ENV"
        printf 'TEMPLATE=$%s\n' '{{ inputs.not_evaluated }}' >> "$GITHUB_ENV"
    - id: nested
      uses: ./.github/actions/inner
    - id: verify
      shell: sh
      run: test -z "${CHILD_ONLY:-}"
`)
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
printf 'input=%s value=%s dynamic=%s path=%s expected=%s\n' "$INPUT_MESSAGE" "$VALUE" "$DYNAMIC_COPY" "$GITHUB_ACTION_PATH" "$EXPECTED_ACTION_PATH" >&2
test "$INPUT_MESSAGE" = nested-input
test -z "${INPUT_PARENT_ONLY:-}"
test "$VALUE" = '${{ inputs.not_evaluated }}'
test "$DYNAMIC_COPY" = from-env-file
test "$GITHUB_ACTION_PATH" = "$EXPECTED_ACTION_PATH"
printf '%s\n' 'result=nested-ok' >> "$GITHUB_OUTPUT"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "outer", Kind: "uses", Uses: "./.github/actions/outer", With: map[string]string{"parent-only": "private-to-parent"}}})
	job.Env = map[string]string{
		"EXPECTED_ACTION_PATH": filepath.Join(workspace, ".github", "actions", "js"),
		"EXPECTED_INNER_PATH":  filepath.Join(workspace, ".github", "actions", "inner"),
	}
	job.Outputs = map[string]string{"result": "${{ steps.outer.outputs.result }}"}
	var logs bytes.Buffer
	result, err := (Runner{Node24: fakeNode, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v\nlogs: %s", err, logs.String())
	}
	if result.Outputs["result"] != "nested-ok" {
		t.Fatalf("result = %#v, want nested composite output chain", result.Outputs)
	}
}

func TestJobContinueOnErrorDoesNotTolerateLazyWorkspaceActionIntegrityFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: lazy workspace integrity\n")
	writeFixtureFile(t, workspace, ".github/actions/local/action.yml", "name: local\nruns:\n  using: node24\n  main: index.js\n")
	writeFixtureFile(t, workspace, ".github/actions/local/index.js", "console.log('original')\n")
	lockID := "a-0000000000000001"
	lock := plan.ActionLock{ID: lockID, Source: "workspace", Path: ".github/actions/local", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/local"))}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "tamper", Kind: "run", Shell: "sh", Command: "printf tampered > .github/actions/local/index.js"},
		{ID: "local", Kind: "uses", Uses: "./.github/actions/local", Action: &plan.ActionSelector{Lock: lockID}, ContinueOnError: true},
	})
	job.Actions = []plan.ActionLock{lock}
	job.ContinueOnError = true

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "workspace action digest mismatch") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestCompositeConditionsRunAfterFailureAndPreserveFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/conditions/action.yml", `name: Conditional composite
runs:
  using: composite
  steps:
    - id: failed
      shell: sh
      run: exit 7
    - id: skipped
      shell: sh
      run: touch "$SHOULD_NOT_RUN"
    - id: observed
      if: failure() && steps.failed.outcome == 'failure' && steps.skipped.outcome == 'skipped' && steps.skipped.outputs.missing == ''
      shell: sh
      run: touch "$STATUS_RAN"
    - id: cleanup
      if: always()
      shell: sh
      run: touch "$ALWAYS_RAN"
`)
	statusRan := filepath.Join(workspace, "status-ran")
	alwaysRan := filepath.Join(workspace, "always-ran")
	shouldNotRun := filepath.Join(workspace, "should-not-run")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/conditions"}})
	job.Env = map[string]string{"STATUS_RAN": statusRan, "ALWAYS_RAN": alwaysRan, "SHOULD_NOT_RUN": shouldNotRun}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v, want preserved composite failure", result, err)
	}
	for _, path := range []string{statusRan, alwaysRan} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("expected conditional child %q to run: %v", path, statErr)
		}
	}
	if _, statErr := os.Stat(shouldNotRun); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("default child ran after failure: %v", statErr)
	}
}

func TestCompositeContinueOnErrorPreservesOutcomeAndRunsLaterSteps(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/soft-failure/action.yml", `name: Soft failure composite
outputs:
  status:
    value: ${{ steps.failed.outcome }}-${{ steps.failed.conclusion }}
runs:
  using: composite
  steps:
    - id: failed
      shell: sh
      run: exit 7
      continue-on-error: true
    - shell: sh
      run: touch "$LATER_STEP_RAN"
`)
	laterStep := filepath.Join(workspace, "later-step-ran")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/soft-failure"}})
	job.Env = map[string]string{"LATER_STEP_RAN": laterStep}
	job.Outputs = map[string]string{"status": "${{ steps.composite.outputs.status }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, want soft composite failure", result, err)
	}
	if result.Outputs["status"] != "failure-success" {
		t.Fatalf("RunJob() outputs = %#v, want retained soft-failure status", result.Outputs)
	}
	if _, statErr := os.Stat(laterStep); statErr != nil {
		t.Fatalf("later composite step did not run: %v", statErr)
	}
}

func TestCompositeContinueOnErrorToleratesConditionFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/soft-condition/action.yml", `name: Soft condition failure composite
outputs:
  status:
    value: ${{ steps.failed.outcome }}-${{ steps.failed.conclusion }}
runs:
  using: composite
  steps:
    - id: failed
      if: fromJSON('invalid')
      shell: sh
      run: touch "$SHOULD_NOT_RUN"
      continue-on-error: true
    - shell: sh
      run: touch "$LATER_STEP_RAN"
`)
	laterStep := filepath.Join(workspace, "later-step-ran")
	shouldNotRun := filepath.Join(workspace, "should-not-run")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/soft-condition"}})
	job.Env = map[string]string{"LATER_STEP_RAN": laterStep, "SHOULD_NOT_RUN": shouldNotRun}
	job.Outputs = map[string]string{"status": "${{ steps.composite.outputs.status }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, want soft condition failure", result, err)
	}
	if result.Outputs["status"] != "failure-success" {
		t.Fatalf("RunJob() outputs = %#v, want retained soft-failure status", result.Outputs)
	}
	if _, statErr := os.Stat(laterStep); statErr != nil {
		t.Fatalf("later composite step did not run: %v", statErr)
	}
	if _, statErr := os.Stat(shouldNotRun); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("condition-failed child ran: %v", statErr)
	}
}

func TestCompositeChildConditionUsesActionInputs(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		t.Run(enabled, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			writeFixtureFile(t, workspace, ".github/actions/conditional/action.yml", `name: Input conditional composite
inputs:
  enabled:
    required: true
runs:
  using: composite
  steps:
    - if: inputs.enabled == 'true'
      shell: sh
      run: touch "$CONDITIONAL_RAN"
`)
			marker := filepath.Join(workspace, "conditional-ran")
			job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/conditional", With: map[string]string{"enabled": enabled}}})
			job.Env = map[string]string{"CONDITIONAL_RAN": marker}
			result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			_, statErr := os.Stat(marker)
			if enabled == "true" && statErr != nil {
				t.Fatalf("enabled child did not run: %v", statErr)
			}
			if enabled == "false" && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("disabled child ran: %v", statErr)
			}
		})
	}
}

func TestCompositeStepSupportsCompoundInputExpression(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/rust/action.yml", `name: Rust setup expression
inputs:
  toolchain:
    required: false
  components:
    required: false
  targets:
    required: false
  target:
    required: false
runs:
  using: composite
  steps:
    - shell: sh
      env:
        targets: ${{ inputs.targets || inputs.target || '' }}
        owner: ${{ github.repository_owner }}
      run: |
        test "${{ runner.temp }}" = "$RUNNER_TEMP"
        echo "downgrade=${{contains(inputs.toolchain, 'nightly') && inputs.components && ' --allow-downgrade' || ''}}" > "$RESULT"
        echo "targets=$targets" >> "$RESULT"
        echo "owner=$owner" >> "$RESULT"
`)
	output := filepath.Join(workspace, "result")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:   "rust",
		Kind: "uses",
		Uses: "./.github/actions/rust",
		With: map[string]string{"toolchain": "nightly", "components": "rustfmt"},
	}})
	job.Env = map[string]string{"RESULT": output}
	job.Event.Repository = "buildkite/buildkite-gha"

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "downgrade= --allow-downgrade\ntargets=\nowner=buildkite\n"; got != want {
		t.Fatalf("compound composite step expressions = %q, want %q", got, want)
	}
}

func TestRuntimeRejectsRecursiveAndOverDepthCompositeActions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/recursive/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/recursive\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "recursive", Kind: "uses", Uses: "./.github/actions/recursive"}})
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "contains a cycle") {
		t.Fatalf("RunJob() recursion error = %v", err)
	}

	for i := 0; i <= metadata.MaxNestedActionDepth; i++ {
		next := "  steps:\n    - shell: sh\n      run: \"true\"\n"
		if i < metadata.MaxNestedActionDepth {
			next = fmt.Sprintf("  steps:\n    - uses: ./.github/actions/depth-%d\n", i+1)
		}
		writeFixtureFile(t, workspace, fmt.Sprintf(".github/actions/depth-%d/action.yml", i), "runs:\n  using: composite\n"+next)
	}
	job = runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "deep", Kind: "uses", Uses: "./.github/actions/depth-0"}})
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds maximum depth %d", metadata.MaxNestedActionDepth)) {
		t.Fatalf("RunJob() depth error = %v", err)
	}
}

func TestRuntimeMapDiagnosticsAreSorted(t *testing.T) {
	_, err := evaluateMap(map[string]string{
		"z-last":  "${{ unsupported.z }}",
		"a-first": "${{ unsupported.a }}",
	}, expression.Context{})
	if err == nil || !strings.Contains(err.Error(), `evaluate "a-first"`) {
		t.Fatalf("evaluateMap() error = %v, want alphabetically first key", err)
	}

	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []runtimeTestStep{{ID: "shell", Kind: "run", Shell: "sh", Command: "true"}})
	job.Needs = map[string]plan.Need{"z-last": {}, "a-first": {}}
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `prerequisite result "a-first"`) {
		t.Fatalf("RunJob() prerequisite error = %v, want alphabetically first key", err)
	}

	job.Needs = nil
	job.Outputs = map[string]string{"z-valid": "partial", "a-invalid": "${{ fromJSON('invalid') }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `job output "a-invalid"`) {
		t.Fatalf("RunJob() output error = %v, want alphabetically first key", err)
	}
	if len(result.Outputs) != 0 {
		t.Fatalf("RunJob() partial outputs = %#v, want none before first sorted error", result.Outputs)
	}
}

func TestLivePortableSetupActions(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_ACTIONS") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_ACTIONS=1 to execute public setup actions with anonymous downloads")
	}
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "portable-setup.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  setup:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38
        with:
          node-version: "24"
          package-manager-cache: "false"
          token: ""
      - run: node --version | grep '^v24\.'
      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16
        with:
          go-version: "1.26.5"
          cache: "false"
          token: ""
      - run: go version | grep 'go1\.26\.5 '
`)
	if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := source.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	actionCache := filepath.Join(t.TempDir(), "actions")
	if err := os.Mkdir(actionCache, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := source.NewStore(actionCache, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(ctx, workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource: compiler.PublicActionSource{
			Resolver: resolver,
			Store:    store,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.Schema || len(plans[0].Actions) != 2 || plans[0].RequiresMise == nil || !*plans[0].RequiresMise {
		t.Fatalf("portable setup plans = %#v", plans)
	}
	if got := plans[0].ExecutionJob().Steps[0].Invocation.With[0].Value.Source; got != "24" {
		t.Fatalf("setup-node plan input = %q, want 24", got)
	}
	var logs bytes.Buffer
	result, err := (Runner{Node24: node, Actions: store, Stdout: &logs, Stderr: &logs}).runTestJob(ctx, plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v\nlogs:\n%s", result, err, logs.String())
	}
}

func TestCompiledWorkflowUsesHashFilesAfterWorkspacePreparation(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "hash-files.yml")
	workflow := []byte(`on: push
jobs:
  hash:
    runs-on: ubuntu-latest
    steps:
      - run: printf 'runtime contents' > payload
      - if: hashFiles('payload') != ''
        env:
          PAYLOAD_HASH: ${{ hashFiles('payload') }}
        run: test "$PAYLOAD_HASH" = "93f7a1af9e76c89675b5bc8c5f5c6aa62f1c78bc0c95693f0296b25274843527"
`)
	writeFixtureFile(t, workspace, ".github/workflows/hash-files.yml", string(workflow))
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "hosted")
	if err != nil || len(plans) != 1 {
		t.Fatalf("compile hashFiles workflow = %#v, %v", plans, err)
	}
	result, err := (Runner{}).runTestJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunJobDockerUsesSharedMasking(t *testing.T) {
	docker := requireDocker(t)
	workspace := fixturePath(t)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []runtimeTestStep{{ID: "docker", Kind: "uses", Uses: "./actions/docker"}})
	job.RequiredCapabilities = []string{"docker", "network"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Docker: docker}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Env["DOCKER_RUNTIME_SEEN"] != "true" || strings.Contains(logs.String(), "docker-secret-value") || !strings.Contains(logs.String(), "masked docker probe: ***") {
		t.Fatalf("RunJob() result = %#v, logs = %q", result, logs.String())
	}
}
