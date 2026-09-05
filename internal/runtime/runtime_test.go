package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "check", Kind: "run", Command: "true"}})
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
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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
	if len(plans) != 1 || plans[0].Steps[0].Shell != "${{ env.DEFAULT_SHELL }}" {
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fail", Kind: "run", Command: "exit 1"},
		{ID: "default", Kind: "run", Command: "echo must-not-run"},
		{ID: "recover", Kind: "run", Condition: "failure()", Command: "echo recovered"},
	})
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || strings.Contains(logs.String(), "must-not-run") || !strings.Contains(logs.String(), "recovered") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}

	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "timeout", Kind: "run", Command: "sleep 30", TimeoutMinutes: 0.0005}})
	started := time.Now()
	result, err = (Runner{}).runTestJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "failure" || time.Since(started) > 3*time.Second {
		t.Fatalf("timed RunJob() result = %#v, error = %v, elapsed = %s", result, err, time.Since(started))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cancel", Kind: "run", Condition: "always()", Command: "sleep 30"}})
	result, err = (Runner{}).runTestJob(ctx, job, workspace)
	if !errors.Is(err, context.Canceled) || result.Conclusion != "cancelled" {
		t.Fatalf("cancelled RunJob() result = %#v, error = %v", result, err)
	}
}

func TestExplicitCancelCommitsEffectsWithoutFailingJob(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	ready := filepath.Join(workspace, "background.ready")
	terminated := filepath.Join(workspace, "background.terminated")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `
echo "CANCEL_EFFECT=visible" >> "$GITHUB_ENV"
trap 'touch "$TERMINATED"; exit 0' INT
touch "$READY"
while :; do sleep 1; done`},
		{ID: "await-start", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
		{ID: "cancel-again", Kind: "cancel", Targets: []string{"background"}},
		{ID: "after-cancel", Kind: "run", Condition: "steps.background.outcome == 'cancelled' && steps.cancel.conclusion == 'success' && steps.cancel-again.conclusion == 'success'", Command: `test "$CANCEL_EFFECT" = visible; test -f "$TERMINATED"; echo after-cancel`},
	})
	job.Env = map[string]string{"READY": ready, "TERMINATED": terminated}
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Env["CANCEL_EFFECT"] != "visible" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(logs.String(), "after-cancel") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestCancelQueuedBackgroundNeverStartsIt(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	release := filepath.Join(workspace, "release")
	queuedMarker := filepath.Join(workspace, "queued-started")
	steps := make([]plan.Step, 0, maxActiveBackgroundSteps+4)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, plan.Step{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		plan.Step{ID: "queued", Kind: "run", Background: true, Command: `touch "$QUEUED_MARKER"`},
		plan.Step{ID: "cancel-queued", Kind: "cancel", Targets: []string{"queued"}},
		plan.Step{ID: "release", Kind: "run", Command: `test ! -e "$QUEUED_MARKER"; touch "$RELEASE"`},
		plan.Step{ID: "wait", Kind: "wait-all"},
	)
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{"RELEASE": release, "QUEUED_MARKER": queuedMarker}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, statErr := os.Stat(queuedMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queued canceled step ran: %v", statErr)
	}
}

func TestQueuedBackgroundTimeoutStartsAtDispatch(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: background timeout dispatch\n")
	release := filepath.Join(workspace, "release")
	queuedMarker := filepath.Join(workspace, "queued-started")
	steps := make([]plan.Step, 0, maxActiveBackgroundSteps+3)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, plan.Step{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		plan.Step{ID: "queued", Kind: "run", Background: true, TimeoutMinutes: 0.001, Command: `touch "$QUEUED_MARKER"`},
		plan.Step{ID: "release", Kind: "run", Command: `sleep 0.2; touch "$RELEASE"`},
		plan.Step{ID: "wait", Kind: "wait-all"},
	)
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{"RELEASE": release, "QUEUED_MARKER": queuedMarker}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(queuedMarker); err != nil {
		t.Fatalf("queued timed step did not run: %v", err)
	}
}

func TestCancelQueuedBackgroundNeverRegistersPostAction(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/queued/action.yml", "name: Queued lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/queued/main.js", "console.log('queued-main-must-not-run')\n")
	writeFixtureFile(t, workspace, ".github/actions/queued/post.js", "console.log('queued-post-must-not-run')\n")
	release := filepath.Join(workspace, "release")
	steps := make([]plan.Step, 0, maxActiveBackgroundSteps+4)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, plan.Step{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		plan.Step{ID: "queued", Kind: "uses", Uses: "./.github/actions/queued", Background: true},
		plan.Step{ID: "cancel-queued", Kind: "cancel", Targets: []string{"queued"}},
		plan.Step{ID: "release", Kind: "run", Command: `touch "$RELEASE"`},
		plan.Step{ID: "wait", Kind: "wait-all"},
	)
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{"RELEASE": release}
	var logs bytes.Buffer

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(logs.String(), "queued-main-must-not-run") || strings.Contains(logs.String(), "queued-post-must-not-run") {
		t.Fatalf("queued cancelled action ran lifecycle phase: %q", logs.String())
	}
}

func TestFailureBeforeCancelStillFailsJob(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	failedPID := filepath.Join(workspace, "failed.pid")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `echo $$ > "$FAILED_PID"; exit 7`},
		{ID: "await-failure", Kind: "run", Command: `while [ ! -f "$FAILED_PID" ]; do sleep 0.01; done; while kill -0 "$(cat "$FAILED_PID")" 2>/dev/null; do sleep 0.01; done; sleep 0.05`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
		{ID: "default-after-cancel", Kind: "run", Command: "echo must-not-run"},
		{ID: "recover", Kind: "run", Condition: "failure()", Command: "echo recovered"},
	})
	job.Env = map[string]string{"FAILED_PID": failedPID}

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(logs.String(), "recovered") || strings.Contains(logs.String(), "must-not-run") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestBackgroundEffectsCommitAtCoveringBarriers(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	oneDone := filepath.Join(workspace, "one.done")
	twoDone := filepath.Join(workspace, "two.done")
	pathEntry := filepath.Join(workspace, "from-background")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "one", Kind: "run", Background: true, Command: `
echo "ONE=committed-one" >> "$GITHUB_ENV"
echo "value=output-one" >> "$GITHUB_OUTPUT"
echo "$PATH_ENTRY" >> "$GITHUB_PATH"
echo one-summary >> "$GITHUB_STEP_SUMMARY"
touch "$ONE_DONE"`},
		{ID: "two", Kind: "run", Background: true, Command: `
echo "TWO=committed-two" >> "$GITHUB_ENV"
echo "value=output-two" >> "$GITHUB_OUTPUT"
touch "$TWO_DONE"`},
		{ID: "before-wait", Kind: "run", Command: `
while [ ! -f "$ONE_DONE" ] || [ ! -f "$TWO_DONE" ]; do sleep 0.01; done
test -z "$ONE"
test -z "$TWO"
case "$PATH" in "$PATH_ENTRY"*) exit 1 ;; esac`},
		{ID: "wait-one", Kind: "wait", Targets: []string{"one"}},
		{ID: "after-targeted-wait", Kind: "run", Command: `
test "$ONE" = committed-one
test -z "$TWO"
test "${{ steps.one.outputs.value }}" = output-one
case "$PATH" in "$PATH_ENTRY"*) ;; *) exit 1 ;; esac`},
		{ID: "wait-all", Kind: "wait-all"},
		{ID: "after-wait-all", Kind: "run", Command: `
test "$ONE" = committed-one
test "$TWO" = committed-two
test "${{ steps.two.outputs.value }}" = output-two`},
	})
	job.Env = map[string]string{"ONE_DONE": oneDone, "TWO_DONE": twoDone, "PATH_ENTRY": pathEntry}
	job.Outputs = map[string]string{"one": "${{ steps.one.outputs.value }}", "two": "${{ steps.two.outputs.value }}"}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" || result.Outputs["one"] != "output-one" || result.Outputs["two"] != "output-two" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if result.Summary != "one-summary\n" {
		t.Fatalf("RunJob() summary = %q", result.Summary)
	}
}

func TestConcurrentPathEffectsComposeWithLiveBarrierState(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	oneDone := filepath.Join(workspace, "one.done")
	twoDone := filepath.Join(workspace, "two.done")
	onePath := filepath.Join(workspace, "background-one")
	twoPath := filepath.Join(workspace, "background-two")
	foregroundPath := filepath.Join(workspace, "foreground")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "one", Kind: "run", Background: true, Command: `echo "$ONE_PATH" >> "$GITHUB_PATH"; touch "$ONE_DONE"`},
		{ID: "two", Kind: "run", Background: true, Command: `echo "$TWO_PATH" >> "$GITHUB_PATH"; touch "$TWO_DONE"`},
		{ID: "foreground", Kind: "run", Command: `
while [ ! -f "$ONE_DONE" ] || [ ! -f "$TWO_DONE" ]; do sleep 0.01; done
echo "$FOREGROUND_PATH" >> "$GITHUB_PATH"`},
		{ID: "wait-all", Kind: "wait-all"},
		{ID: "verify", Kind: "run", Command: `
want="$TWO_PATH:$ONE_PATH:$FOREGROUND_PATH:"
case "$PATH" in "$want"*) ;; *) exit 1 ;; esac
test "$(printf '%s' "$PATH" | tr ':' '\n' | grep -Fxc "$ONE_PATH")" -eq 1
test "$(printf '%s' "$PATH" | tr ':' '\n' | grep -Fxc "$TWO_PATH")" -eq 1
test "$(printf '%s' "$PATH" | tr ':' '\n' | grep -Fxc "$FOREGROUND_PATH")" -eq 1`},
	})
	job.Env = map[string]string{
		"ONE_DONE": oneDone, "TWO_DONE": twoDone,
		"ONE_PATH": onePath, "TWO_PATH": twoPath, "FOREGROUND_PATH": foregroundPath,
	}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
}

func TestBackgroundCompositePathEffectsComposeAtBarrier(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/path/action.yml", `name: Path writer
runs:
  using: composite
  steps:
    - shell: bash
      run: echo "$COMPOSITE_PATH" >> "$GITHUB_PATH"
`)
	compositePath := filepath.Join(workspace, "composite")
	foregroundPath := filepath.Join(workspace, "foreground")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/path", Background: true},
		{ID: "foreground", Kind: "run", Command: `echo "$FOREGROUND_PATH" >> "$GITHUB_PATH"`},
		{ID: "wait", Kind: "wait", Targets: []string{"composite"}},
		{ID: "verify", Kind: "run", Command: `case "$PATH" in "$COMPOSITE_PATH:$FOREGROUND_PATH:"*) ;; *) exit 1 ;; esac`},
	})
	job.Env = map[string]string{"COMPOSITE_PATH": compositePath, "FOREGROUND_PATH": foregroundPath}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
}

func TestNode20DeclarationUsesNode24ForJavaScriptLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/path/action.yml", `name: JavaScript path writer
runs:
  using: node20
  pre: pre.js
  main: main.js
  post: post.js
`)
	writeFixtureFile(t, workspace, ".github/actions/path/pre.js", "")
	writeFixtureFile(t, workspace, ".github/actions/path/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/path/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v24.0.0
  exit 0
fi
case "${1##*/}" in
  pre.js) printf '%s\n' "$PATH_ENTRY" >> "$GITHUB_PATH" ;;
  main.js) case ":$PATH:" in *":$PATH_ENTRY:"*) ;; *) exit 9 ;; esac ;;
  post.js) printf 'NODE24_POST=true\n' >> "$GITHUB_ENV" ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	pathEntry := filepath.Join(workspace, "from-pre")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./.github/actions/path"}})
	job.Env = map[string]string{"PATH_ENTRY": pathEntry}

	result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if result.Env["NODE24_POST"] != "true" {
		t.Fatalf("RunJob() environment = %#v, want Node 24 post lifecycle effect", result.Env)
	}
	if result.WarningAnnotations != "" {
		t.Fatalf("RunJob() warnings = %q, want no Node 20 deprecation warning", result.WarningAnnotations)
	}
}

func TestNode16DeclarationUsesExactLifecycleAndWarns(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/action.yml", `name: Node 16 lifecycle
runs:
  using: node16
  pre: pre.js
  main: main.js
  post: post.js
`)
	for _, entry := range []string{"pre.js", "main.js", "post.js"} {
		writeFixtureFile(t, workspace, ".github/actions/node16/"+entry, "")
	}
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v16.20.2
  exit 0
fi
printf 'NODE16_%s=true\n' "$(basename "$1" .js | tr '[:lower:]' '[:upper:]')" >> "$GITHUB_ENV"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./.github/actions/node16"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	for _, phase := range []string{"PRE", "MAIN", "POST"} {
		if result.Env["NODE16_"+phase] != "true" {
			t.Fatalf("RunJob() environment = %#v, want Node 16 %s lifecycle effect", result.Env, phase)
		}
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/node16")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings = %q, want one Node 16 lifecycle warning %q", logs.String(), result.WarningAnnotations, warning)
	}
}

func TestNode16WarningAggregatesInvokedActions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	for _, name := range []string{"alpha", "beta", "skipped"} {
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/action.yml", "name: "+name+"\nruns:\n  using: node16\n  main: main.js\n")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/main.js", "")
	}
	writeFixtureFile(t, workspace, ".github/actions/modern/action.yml", "name: modern\nruns:\n  using: node20\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/modern/main.js", "")
	node16 := filepath.Join(workspace, "node16")
	node24 := filepath.Join(workspace, "node24")
	writeNodeExecutable(t, node16, 16)
	writeNodeExecutable(t, node24, 24)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "beta", Kind: "uses", Uses: "./.github/actions/beta"},
		{ID: "alpha", Kind: "uses", Uses: "./.github/actions/alpha"},
		{ID: "alpha-again", Kind: "uses", Uses: "./.github/actions/alpha"},
		{ID: "skipped", Kind: "uses", Uses: "./.github/actions/skipped", Condition: "false"},
		{ID: "modern", Kind: "uses", Uses: "./.github/actions/modern"},
	})
	var logs bytes.Buffer
	result, err := (Runner{Node16: node16, Node24: node24, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/alpha, ./.github/actions/beta")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings = %q, want one aggregate warning %q", logs.String(), result.WarningAnnotations, warning)
	}
	for _, absent := range []string{"./.github/actions/skipped", "./.github/actions/modern"} {
		if strings.Contains(result.WarningAnnotations, absent) {
			t.Fatalf("RunJob() warnings = %q, unexpectedly named %q", result.WarningAnnotations, absent)
		}
	}
}

func TestNode16WarningSurvivesSuppressionAndMasksReferences(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/sensitive/action.yml", "name: sensitive\nruns:\n  using: node16\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/sensitive/main.js", "")
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v16.20.2; exit 0; fi
printf '%s\n' '::add-mask::sensitive'
head -c 1048577 /dev/zero | tr '\000' x
printf '\n'
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "node16", Kind: "uses", Uses: "./.github/actions/sensitive"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "line exceeds 1048576-byte limit") || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v, want oversized-line failure", result, err)
	}
	warningLog := logs.String()
	if warningIndex := strings.LastIndex(warningLog, "warning: Node.js 16 actions are deprecated"); warningIndex >= 0 {
		warningLog = warningLog[warningIndex:]
	}
	for _, output := range []string{warningLog, result.WarningAnnotations} {
		if !strings.Contains(output, "Node.js 16 actions are deprecated") || !strings.Contains(output, "./.github/actions/***") || strings.Contains(output, "sensitive") {
			t.Fatalf("RunJob() warning = %q, want masked Node 16 warning after stream suppression", output)
		}
	}
}

func TestNode16WarningHasPriorityOverUntrustedAnnotationLimit(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/action.yml", "name: node16\nruns:\n  using: node16\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/main.js", "")
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v16.20.2; exit 0; fi
i=0
while [ "$i" -lt 17 ]; do
  printf '%s' '::warning::'
  head -c 65536 /dev/zero | tr '\000' x
  printf '\n'
  i=$((i + 1))
done
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "node16", Kind: "uses", Uses: "./.github/actions/node16"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: io.Discard, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/node16")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings contain Node 16 message %d times, want one priority warning", logs.String(), strings.Count(result.WarningAnnotations, warning))
	}
	if len(result.WarningAnnotations) > maxJobAnnotationBytes || !utf8.ValidString(result.WarningAnnotations) || !strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice) {
		t.Fatalf("RunJob() warning annotation bytes = %d, valid UTF-8 = %v, suffix present = %v", len(result.WarningAnnotations), utf8.ValidString(result.WarningAnnotations), strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice))
	}
}

func TestBackgroundFailureSurfacesAtWait(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "failed.done")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "failure", Kind: "run", Background: true, Command: `echo "FAILED_EFFECT=visible" >> "$GITHUB_ENV"; touch "$FAILURE_DONE"; exit 9`},
		{ID: "before-wait", Kind: "run", Command: `while [ ! -f "$FAILURE_DONE" ]; do sleep 0.01; done; test -z "$FAILED_EFFECT"; echo before-barrier`},
		{ID: "wait", Kind: "wait", Targets: []string{"failure"}},
		{ID: "default-after-wait", Kind: "run", Command: "echo must-not-run"},
		{ID: "recover", Kind: "run", Condition: "failure() && steps.failure.outcome == 'failure'", Command: `test "$FAILED_EFFECT" = visible; echo recovered`},
	})
	job.Env = map[string]string{"FAILURE_DONE": marker}

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(logs.String(), "before-barrier") || !strings.Contains(logs.String(), "recovered") || strings.Contains(logs.String(), "must-not-run") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestBackgroundContinueOnError(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "soft-background", Kind: "run", Background: true, ContinueOnError: true, Command: "exit 7"},
		{ID: "wait-soft", Kind: "wait", Targets: []string{"soft-background"}},
		{ID: "after-soft", Kind: "run", Condition: "steps.soft-background.outcome == 'failure' && steps.soft-background.conclusion == 'success'", Command: "echo after-soft"},
	})

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(logs.String(), "after-soft") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestSkippedBackgroundAndRepeatedWaitAreCommittedAtMostOnce(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "skipped", Kind: "run", Background: true, Condition: "false", Command: "exit 1"},
		{ID: "cancel-skipped", Kind: "cancel", Targets: []string{"skipped"}},
		{ID: "wait-skipped", Kind: "wait", Targets: []string{"skipped"}},
		{ID: "completed", Kind: "run", Background: true, Command: `echo once >> "$GITHUB_STEP_SUMMARY"`},
		{ID: "first-wait", Kind: "wait", Targets: []string{"completed"}},
		{ID: "cancel-completed", Kind: "cancel", Targets: []string{"completed"}},
		{ID: "second-wait", Kind: "wait", Targets: []string{"completed"}},
		{ID: "verify", Kind: "run", Condition: "steps.skipped.conclusion == 'skipped' && steps.skipped.outputs.missing == '' && steps.cancel-skipped.conclusion == 'success' && steps.cancel-skipped.outputs.missing == '' && steps.wait-skipped.conclusion == 'success' && steps.wait-skipped.outputs.missing == '' && steps.first-wait.outputs.missing == '' && steps.cancel-completed.conclusion == 'success' && steps.cancel-completed.outputs.missing == '' && steps.second-wait.outputs.missing == ''", Command: "true"},
	})

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Summary != "once\n" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestBackgroundOutputsReturnErrorBeforeBarrier(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `echo "value=private" >> "$GITHUB_OUTPUT"`},
		{ID: "premature-reader", Kind: "run", Command: `echo "${{ steps.background.outputs.value }}"`},
	})

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "unavailable step") {
		t.Fatalf("RunJob() result = %#v, error = %v, want unavailable background output", result, err)
	}
}

func TestExpressionValuedStepControls(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: expression controls\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "skipped", Kind: "run", Condition: "false", Command: "true", TimeoutMinutesExpression: "${{ fromJSON('invalid') }}",
	}})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestExpressionValuedStepControlsRequireTypedBoundedResults(t *testing.T) {
	for _, test := range []struct {
		name string
		step plan.Step
		want string
	}{
		{name: "boolean", step: plan.Step{ContinueOnErrorExpression: "${{ 'true' }}"}, want: "want boolean"},
		{name: "number", step: plan.Step{TimeoutMinutesExpression: "${{ '1' }}"}, want: "want number"},
		{name: "range", step: plan.Step{TimeoutMinutesExpression: "${{ 361 }}"}, want: "at most 360"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := evaluateStepControls(normalizedTestStep(test.step), expression.Context{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("evaluateStepControls() error = %v, want %q", err, test.want)
			}
		})
	}
}

func normalizedTestStep(step plan.Step) plan.Step {
	job := plan.Job{Steps: []plan.Step{step}}
	attachTestProgram(&job)
	return job.Steps[0]
}

func TestExpressionContinueOnErrorAppliesToPreparedActionFailure(t *testing.T) {
	step := plan.Step{ID: "action", ContinueOnErrorExpression: "${{ true }}"}
	step = normalizedTestStep(step)
	execution := classifyStepExecutionWithControls(t.Context(), t.Context(), step, newResult(), errors.New("pre failed"), expression.Context{})
	if execution.outcome != "failure" || execution.conclusion != "success" {
		t.Fatalf("prepared action execution = %#v", execution)
	}
}

func TestExpressionContinueOnErrorIsNotEvaluatedForCancellation(t *testing.T) {
	jobCtx, cancel := context.WithCancel(t.Context())
	cancel()
	step := plan.Step{ID: "cancelled", ContinueOnErrorExpression: "${{ fromJSON('invalid') }}"}
	step = normalizedTestStep(step)
	execution := classifyStepExecutionWithControls(jobCtx, jobCtx, step, newResult(), context.Canceled, expression.Context{})
	if execution.outcome != "cancelled" || execution.err != context.Canceled {
		t.Fatalf("cancelled execution = %#v", execution)
	}
}

func TestStepNameFailsClosedOnUnavailableBackgroundOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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

func TestImplicitWaitAllPrecedesPostCleanup(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/background/action.yml", "name: Background lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/background/main.js", `
const fs = require('fs')
setTimeout(() => {
  fs.appendFileSync(process.env.GITHUB_ENV, 'BACKGROUND_READY=true\n')
  fs.appendFileSync(process.env.GITHUB_OUTPUT, 'value=implicit\n')
}, 50)
`)
	writeFixtureFile(t, workspace, ".github/actions/background/post.js", `
if (process.env.BACKGROUND_READY !== 'true') process.exit(9)
console.log('post-after-implicit-wait')
`)
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "background", Kind: "uses", Uses: "./.github/actions/background", Background: true}})
	job.Outputs = map[string]string{"value": "${{ steps.background.outputs.value }}"}

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["value"] != "implicit" || result.Env["BACKGROUND_READY"] != "true" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	if !strings.Contains(logs.String(), "post-after-implicit-wait") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestJavaScriptActionLifecycleRunsInWorkspace(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cwd/action.yml", "name: CWD\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, workspace, ".github/actions/cwd/"+phase+".js", fmt.Sprintf(`
require('node:fs').appendFileSync(process.env.CWD_LOG, %q + process.cwd() + '\t' + process.env.GITHUB_WORKSPACE + '\n')
`, phase+":"))
	}
	cwdLog := filepath.Join(t.TempDir(), "cwd.log")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cwd", Kind: "uses", Uses: "./.github/actions/cwd", Env: map[string]string{"CWD_LOG": cwdLog}}})
	result, err := (Runner{Node24: node}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	data, err := os.ReadFile(cwdLog)
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("CWD log = %q, want pre/main/post entries", data)
	}
	for _, line := range lines {
		phase, paths, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("CWD log line %q is malformed", line)
		}
		cwd, workspaceEnv, ok := strings.Cut(paths, "\t")
		if !ok || cwd != resolvedWorkspace || workspaceEnv != resolvedWorkspace {
			t.Fatalf("%s phase CWD/workspace = %q / %q, want %q", phase, cwd, workspaceEnv, resolvedWorkspace)
		}
	}
}

func TestJavaScriptActionCanonicalizesWorkspaceAndRunnerTemp(t *testing.T) {
	node := requireNode24(t)
	base := canonicalTempDir(t)
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	workspace := filepath.Join(linkParent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", linkParent)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cwd/action.yml", "name: CWD\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cwd/main.js", `
require('node:fs').writeFileSync(process.env.CWD_LOG, process.cwd() + '\t' + process.env.GITHUB_WORKSPACE + '\t' + process.env.RUNNER_TEMP)
`)
	cwdLog := filepath.Join(base, "cwd.log")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cwd", Kind: "uses", Uses: "./.github/actions/cwd", Env: map[string]string{"CWD_LOG": cwdLog}}})
	result, err := (Runner{Node24: node}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	data, err := os.ReadFile(cwdLog)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(data), "\t")
	if len(parts) != 3 {
		t.Fatalf("CWD log = %q", data)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if parts[0] != resolvedWorkspace || parts[1] != resolvedWorkspace {
		t.Fatalf("CWD/workspace = %q / %q, want %q", parts[0], parts[1], resolvedWorkspace)
	}
	if filepath.Dir(parts[2]) != realParent {
		t.Fatalf("RUNNER_TEMP = %q, want canonical parent %q", parts[2], realParent)
	}
}

func TestConcurrentStreamsShareMaskRegistration(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "mask.ready")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "masker", Kind: "run", Background: true, Command: `echo '::add-mask::cross-stream-secret'; sleep 0.05; touch "$MASK_READY"`},
		{ID: "other-stream", Kind: "run", Command: `while [ ! -f "$MASK_READY" ]; do sleep 0.01; done; echo 'probe cross-stream-secret'`},
		{ID: "wait", Kind: "wait-all"},
	})
	job.Env = map[string]string{"MASK_READY": marker}
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(logs.String(), "cross-stream-secret") || !strings.Contains(logs.String(), "probe ***") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestConcurrentSmokeWorkflowEndToEnd(t *testing.T) {
	workspace := fixturePath(t, "smoke")
	workflowPath := filepath.Join(workspace, ".github", "workflows", "concurrent.yml")
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(filepath.Join(workspace, "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, source, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %#v, want concurrent and observer", plans)
	}
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs}
	concurrent, err := runner.runTestJob(t.Context(), plans[0], workspace)
	if err != nil || concurrent.Conclusion != "success" {
		t.Fatalf("concurrent result = %#v, error = %v, logs = %q", concurrent, err, logs.String())
	}
	plans[1].Needs = map[string]plan.Need{"concurrent": {Result: concurrent.Conclusion, Outputs: concurrent.Outputs}}
	observer, err := runner.runTestJob(t.Context(), plans[1], workspace)
	if err != nil || observer.Conclusion != "success" {
		t.Fatalf("observer result = %#v, error = %v, logs = %q", observer, err, logs.String())
	}
	if strings.Contains(logs.String(), "concurrent-cross-stream-secret") || !strings.Contains(logs.String(), "CONCURRENT_MASK_PROBE=***") {
		t.Fatalf("concurrent masking logs = %q", logs.String())
	}
	want := `CONCURRENT_OBSERVATION={"cancel":"graceful","failure":"failure-at-wait","implicit":"implicit-wait-all","parallel":"parallel","queue_max":10,"targeted":"targeted-and-full"}`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("concurrent observation missing from logs = %q", logs.String())
	}
}

func TestJobConditionConsumesNeedResultAndOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Condition: "always()", Command: "touch " + marker}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
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

	inputs, err := resolveProgramActionInputs(executionprogram.ActionFromMetadata(action, "node24", nil), nil, eval)
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

	inputs, err = resolveProgramActionInputs(executionprogram.ActionFromMetadata(action, "node24", nil), map[string]string{"GITHUB_TOKEN": ""}, eval)
	if err != nil {
		t.Fatal(err)
	}
	if inputs["github_token"] != "" {
		t.Fatalf("explicit empty input did not suppress metadata default: %#v", inputs)
	}

	withoutToken := eval
	withoutToken.Secrets = nil
	if _, err := resolveProgramActionInputs(executionprogram.ActionFromMetadata(action, "node24", nil), nil, withoutToken); err == nil || !strings.Contains(err.Error(), `unavailable github value "token"`) {
		t.Fatalf("unplanned metadata github.token evaluation error = %v, want unavailable value", err)
	}
	ghesAction := metadata.Metadata{Inputs: map[string]metadata.Input{"token": {Default: &conditionalTokenDefault}}}
	ghes := eval
	ghes.GitHub = map[string]any{"server_url": "https://github.example.com"}
	ghes.Secrets = nil
	inputs, err = resolveProgramActionInputs(executionprogram.ActionFromMetadata(ghesAction, "node24", nil), nil, ghes)
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
			inputs, err := resolveProgramActionInputs(executionprogram.ActionFromMetadata(action, "node24", nil), nil, expression.Context{})
			if err != nil || inputs[test.input] != test.defaultValue {
				t.Fatalf("context default = %#v, %v; want %q", inputs, err, test.defaultValue)
			}

			inputs, err = resolveProgramActionInputs(executionprogram.ActionFromMetadata(action, "node24", nil), map[string]string{strings.ToUpper(test.input): test.supplied}, expression.Context{})
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
	inputs, err := resolveProgramActionInputs(executionprogram.ActionFromMetadata(action, "node24", nil), nil, expression.Context{GitHub: github})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "never", Kind: "run", Command: "false"}})
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
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

func TestJavaScriptPreMainPostFilesAndMasking(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "hello"}}})
	job.Outputs = map[string]string{"result": "${{ steps.javascript.outputs.result }}"}
	result, err := runner.runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if got := result.Outputs["result"]; got != "hello-javascript" {
		t.Errorf("output = %q, want hello-javascript", got)
	}
	if got := result.Env["RUNTIME_SEEN"]; got != "true" {
		t.Errorf("environment = %q, want true", got)
	}
	if got := result.State["phase"]; got != "main" {
		t.Errorf("state = %q, want main", got)
	}
	if got := result.State["pre"]; got != "ready" {
		t.Errorf("pre state = %q, want ready", got)
	}
	if result.Summary != "runtime main summary\nruntime post single\n" {
		t.Errorf("summary = %q", result.Summary)
	}
	if strings.Contains(logs.String(), "runtime-secret-value") {
		t.Fatalf("raw forwarded logs contain literal secret: %q", logs.String())
	}
	for _, event := range []string{"lifecycle:pre", "lifecycle:main", "masked probe: ***", "lifecycle:post:single"} {
		if !strings.Contains(logs.String(), event) {
			t.Errorf("logs = %q, want event %q", logs.String(), event)
		}
	}
	pre := strings.Index(logs.String(), "lifecycle:pre")
	main := strings.Index(logs.String(), "lifecycle:main")
	post := strings.Index(logs.String(), "lifecycle:post:single")
	if pre > main || main > post {
		t.Errorf("lifecycle logs are out of order: %q", logs.String())
	}
}

func TestPostActionSummaryOverflowIsTruncatedWithoutFailingJob(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/summary/action.yml", `name: Summary writer
runs:
  using: node24
  main: main.js
  post: post.js
`)
	writeFixtureFile(t, workspace, ".github/actions/summary/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/summary/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v24.0.0
  exit 0
fi
case "${1##*/}" in
  main.js) head -c "$MAIN_SUMMARY_BYTES" /dev/zero | tr '\000' m >> "$GITHUB_STEP_SUMMARY" ;;
  post.js) printf 'post-summary-must-be-truncated\n' >> "$GITHUB_STEP_SUMMARY" ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "summary", Kind: "uses", Uses: "./.github/actions/summary"}})
	job.Env = map[string]string{"MAIN_SUMMARY_BYTES": strconv.Itoa(maxJobSummaryBytes)}

	result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if len(result.Summary) > maxJobSummaryBytes || strings.Contains(result.Summary, "post-summary-must-be-truncated") || !strings.HasSuffix(result.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("RunJob() summary bytes = %d, suffix present = %v", len(result.Summary), strings.HasSuffix(result.Summary, jobSummaryTruncationNotice))
	}
}

func TestOversizedStepSummaryIsNonFatalAndDiscarded(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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

func TestPostActionsRunLIFOAfterMainFailure(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{
		{ID: "one", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "one", "order": "one"}},
		{ID: "two", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "two", "order": "two", "fail": "true"}},
	})
	_, err := runner.runTestJob(t.Context(), job, workspace)
	if err == nil {
		t.Fatal("RunJob() error = nil, want main failure")
	}
	if !strings.Contains(logs.String(), "requested main failure") {
		t.Fatalf("forwarded logs = %q, want requested main failure", logs.String())
	}
	one := strings.Index(logs.String(), "lifecycle:post:one")
	two := strings.Index(logs.String(), "lifecycle:post:two")
	if two < 0 || one < 0 || two > one {
		t.Errorf("post logs are not LIFO: %q", logs.String())
	}
}

func TestConditionInspectionFailureAfterMainFailureStillRunsPostPhase(t *testing.T) {
	invalidStepCondition := executionprogram.Site{
		Source: "(", Surface: executionprogram.SurfaceStepCondition, Result: executionprogram.ResultBoolean,
		Provenance: executionprogram.ProvenanceWorkflow, Purpose: executionprogram.PurposeExpression,
	}
	invalidPostCondition := executionprogram.Site{
		Source: "(", Surface: executionprogram.SurfaceActionLifecycle, Result: executionprogram.ResultBoolean,
		Provenance: executionprogram.ProvenanceAction, Purpose: executionprogram.PurposeExpression,
	}
	run := newJobRun(Runner{})
	run.job = plan.Job{
		Steps:   []plan.Step{{ID: "inspect", Kind: "run", Execution: &executionprogram.Step{Condition: invalidStepCondition}}},
		Program: &executionprogram.Program{Version: executionprogram.Version},
	}
	run.processor = newCommandOutputProcessor(io.Discard, io.Discard)
	run.eval = expression.Context{Env: map[string]string{}, Steps: map[string]expression.StepStatus{}}
	run.result = JobResult{Conclusion: "failure", Outputs: map[string]string{}, Env: map[string]string{}, State: map[string]string{}}
	run.runtimeEnv = map[string]string{}
	run.posts = &postRegistry{}
	run.posts.register(&registeredPost{
		conditionSite: &invalidPostCondition,
		invocation:    &preparedInvocation{action: javaScriptAction{Name: "cleanup"}, eval: expression.Context{}},
	})
	run.supervisor = newBackgroundSupervisor(1)
	run.preFailures = map[int]stepExecution{}
	run.runErr = errors.New("main failed")

	_, err := run.runSteps(t.Context(), t.Context())
	if err == nil || !strings.Contains(err.Error(), `step "inspect"`) || !strings.Contains(err.Error(), `post action "cleanup" condition`) {
		t.Fatalf("runSteps() error = %v, want step and post-phase diagnostics", err)
	}
	if got := run.eval.Steps["inspect"].Conclusion; got != "failure" {
		t.Fatalf("condition inspection conclusion = %q, want failure", got)
	}
}

func TestJobContinueOnErrorPreservesFailureLifecycleAndOutputs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "test.yml")
	workflow := []byte(`on: push
jobs:
  report:
    runs-on: ubuntu-latest
    continue-on-error: true
    outputs:
      diagnostic: ${{ steps.fail.outputs.diagnostic }}
    steps:
      - uses: ./.github/actions/lifecycle
      - id: fail
        run: echo "diagnostic=failed" >> "$GITHUB_OUTPUT"; exit 7
      - run: touch "$ORDINARY_MARKER"
      - if: failure()
        run: touch "$RECOVERY_MARKER"
`)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", string(workflow))
	writeFixtureFile(t, workspace, ".github/actions/lifecycle/action.yml", "name: lifecycle\ninputs:\n  job_status:\n    default: ${{ job.status }}\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n  post-if: failure()\n")
	writeFixtureFile(t, workspace, ".github/actions/lifecycle/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/lifecycle/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "${1##*/}" = post.js ]; then printenv INPUT_JOB_STATUS > "$POST_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.Schema || !plans[0].ContinueOnError {
		t.Fatalf("compiled plans = %#v, want one tolerated plan", plans)
	}
	ordinary := filepath.Join(workspace, "ordinary")
	recovery := filepath.Join(workspace, "recovery")
	post := filepath.Join(workspace, "post")
	plans[0].Env = map[string]string{"ORDINARY_MARKER": ordinary, "RECOVERY_MARKER": recovery, "POST_MARKER": post}
	plans[0].Program.Job.Env = []executionprogram.Binding{
		{Name: "ORDINARY_MARKER", Value: executionprogram.Site{Source: ordinary, Surface: executionprogram.SurfaceJobEnvironment, Result: executionprogram.ResultString, Provenance: executionprogram.ProvenanceWorkflow, Purpose: executionprogram.PurposeExpression}},
		{Name: "POST_MARKER", Value: executionprogram.Site{Source: post, Surface: executionprogram.SurfaceJobEnvironment, Result: executionprogram.ResultString, Provenance: executionprogram.ProvenanceWorkflow, Purpose: executionprogram.PurposeExpression}},
		{Name: "RECOVERY_MARKER", Value: executionprogram.Site{Source: recovery, Surface: executionprogram.SurfaceJobEnvironment, Result: executionprogram.ResultString, Provenance: executionprogram.ProvenanceWorkflow, Purpose: executionprogram.PurposeExpression}},
	}

	result, runErr := (Runner{Node24: fakeNode}).runTestJob(t.Context(), plans[0], workspace)
	if runErr == nil || !IsToleratedJobFailure(runErr) || result.Conclusion != "success" || result.Outputs["diagnostic"] != "failed" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, runErr)
	}
	if _, err := os.Stat(ordinary); !os.IsNotExist(err) {
		t.Fatalf("ordinary success-gated step ran after failure: %v", err)
	}
	if _, err := os.Stat(recovery); err != nil {
		t.Fatalf("failure-gated step did not run: %v", err)
	}
	status, err := os.ReadFile(post)
	if err != nil || strings.TrimSpace(string(status)) != "failure" {
		t.Fatalf("post job.status = %q, %v, want failure", status, err)
	}

	if err := os.Remove(post); err != nil {
		t.Fatal(err)
	}
	timedOut := plans[0]
	timedOut.TimeoutMinutes = 0.001
	programCopy := *timedOut.Program
	programCopy.Job = timedOut.Program.Job
	programCopy.Job.TimeoutMinutes = 0.001
	programCopy.Job.Steps = slices.Clone(timedOut.Program.Job.Steps)
	runCopy := *programCopy.Job.Steps[1].Run
	runCopy.Command.Source = "sleep 1"
	programCopy.Job.Steps[1].Run = &runCopy
	timedOut.Program = &programCopy
	timedOut.Steps = slices.Clone(timedOut.Steps)
	timedOut.Steps[1].Command = "sleep 1"
	timedOut.Steps[1].Execution = &timedOut.Program.Job.Steps[1]
	result, runErr = (Runner{Node24: fakeNode}).runTestJob(t.Context(), timedOut, workspace)
	if !errors.Is(runErr, context.DeadlineExceeded) || IsToleratedJobFailure(runErr) || result.Conclusion != "cancelled" {
		t.Fatalf("timed out RunJob() result = %#v, error = %v", result, runErr)
	}
	if _, err := os.Stat(post); !os.IsNotExist(err) {
		t.Fatalf("failure-only post ran after job cancellation: %v", err)
	}
}

func TestJobTimeoutDuringSetupIsCancelled(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: setup timeout\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
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

func TestJavaScriptPostConditionsUseFinalJobStatus(t *testing.T) {
	tests := []struct {
		name        string
		condition   string
		failMain    bool
		wantPost    bool
		wantStatus  string
		wantFailure bool
	}{
		{name: "success after success", condition: "success()", wantPost: true, wantStatus: "success"},
		{name: "success after failure", condition: "${{ success() }}", failMain: true, wantFailure: true},
		{name: "failure after failure", condition: "failure()", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
		{name: "final step state after failure", condition: "failure() && steps.conditional.outcome == 'failure'", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
		{name: "always after failure", condition: "always()", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
		{name: "not cancelled after success", condition: "!cancelled()", wantPost: true, wantStatus: "success"},
		{name: "not cancelled after failure", condition: "!cancelled()", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			writeFixtureFile(t, workspace, ".github/actions/conditional/action.yml", "name: Conditional post\ninputs:\n  job_status:\n    default: ${{ job.status }}\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n  post-if: "+test.condition+"\n")
			writeFixtureFile(t, workspace, ".github/actions/conditional/main.js", "")
			writeFixtureFile(t, workspace, ".github/actions/conditional/post.js", "")
			fakeNode := filepath.Join(workspace, "node24")
			writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "${1##*/}" = post.js ]; then printenv INPUT_JOB_STATUS > "$POST_MARKER"; fi
if [ "${1##*/}" = main.js ] && [ "${FAIL_MAIN:-false}" = true ]; then exit 9; fi
`)
			if err := os.Chmod(fakeNode, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(workspace, "post-ran")
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "conditional", Kind: "uses", Uses: "./.github/actions/conditional"}})
			job.Env = map[string]string{"POST_MARKER": marker, "FAIL_MAIN": strconv.FormatBool(test.failMain)}

			result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
			if (err != nil) != test.wantFailure || (result.Conclusion == "failure") != test.wantFailure {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			_, statErr := os.Stat(marker)
			if gotPost := statErr == nil; gotPost != test.wantPost {
				t.Fatalf("post ran = %v, want %v (stat error %v)", gotPost, test.wantPost, statErr)
			}
			if test.wantPost {
				status, readErr := os.ReadFile(marker)
				if readErr != nil || strings.TrimSpace(string(status)) != test.wantStatus {
					t.Fatalf("post job.status = %q, %v, want %q", status, readErr, test.wantStatus)
				}
			}
		})
	}
}

func TestRustCachePostConditionUsesFinalStatusAndMainEnvironment(t *testing.T) {
	tests := []struct {
		name            string
		failJob         bool
		cacheValue      string
		finalCacheValue string
		wantPost        bool
	}{
		{name: "success", wantPost: true},
		{name: "failure default", failJob: true},
		{name: "failure disabled", failJob: true, cacheValue: "false"},
		{name: "failure enabled", failJob: true, cacheValue: "true", wantPost: true},
		{name: "later environment wins", failJob: true, cacheValue: "true", finalCacheValue: "false"},
		{name: "post process sees later environment", cacheValue: "true", finalCacheValue: "false", wantPost: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
			if err != nil {
				t.Fatal(err)
			}
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: rust-cache lifecycle\n")
			writeFixtureFile(t, workspace, ".github/actions/rust-cache/action.yml", `name: rust-cache
runs:
  using: node24
  main: main.js
  post: post.js
  post-if: success() || env.CACHE_ON_FAILURE == 'true'
`)
			writeFixtureFile(t, workspace, ".github/actions/rust-cache/main.js", "")
			writeFixtureFile(t, workspace, ".github/actions/rust-cache/post.js", "")
			fakeNode := filepath.Join(workspace, "node24")
			writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
case "$(basename "$1")" in
  main.js)
    if [ -n "${CACHE_SETTING:-}" ]; then printf 'CACHE_ON_FAILURE=%s\n' "$CACHE_SETTING" >> "$GITHUB_ENV"; fi
    printf '%s\n' 'GITHUB_WORKSPACE=/tmp/spoofed' >> "$GITHUB_ENV"
    ;;
  post.js) printf '%s|%s' "${CACHE_ON_FAILURE:-}" "$GITHUB_WORKSPACE" > "$POST_MARKER" ;;
esac
`)
			if err := os.Chmod(fakeNode, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(workspace, "post")
			steps := []plan.Step{{ID: "cache", Kind: "uses", Uses: "./.github/actions/rust-cache"}}
			if test.finalCacheValue != "" {
				steps = append(steps, plan.Step{ID: "override", Kind: "run", Command: fmt.Sprintf("printf 'CACHE_ON_FAILURE=%s\\n' >> \"$GITHUB_ENV\"", test.finalCacheValue)})
			}
			if test.failJob {
				steps = append(steps, plan.Step{ID: "fail", Kind: "run", Command: "exit 7"})
			}
			job := runtimePlan(t, workspace, workflowPath, steps)
			job.Env = map[string]string{"CACHE_SETTING": test.cacheValue, "POST_MARKER": marker}
			result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
			if (err != nil) != test.failJob || (result.Conclusion == "failure") != test.failJob {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			_, statErr := os.Stat(marker)
			if got := statErr == nil; got != test.wantPost {
				t.Fatalf("post ran = %v, want %v (stat error %v)", got, test.wantPost, statErr)
			}
			if test.wantPost && test.finalCacheValue != "" {
				value, err := os.ReadFile(marker)
				want := test.finalCacheValue + "|" + resolvedWorkspace
				if err != nil || string(value) != want {
					t.Fatalf("post environment = %q, %v, want %q", value, err, want)
				}
			}
		})
	}
}

func TestNestedSetupRustToolchainPostUsesMainEnvironmentAtJobTeardown(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested rust-cache lifecycle\n")
	writeFixtureFile(t, workspace, ".github/actions/setup-rust-toolchain/action.yml", `name: setup-rust-toolchain
runs:
  using: composite
  steps:
    - uses: ./.github/actions/rust-cache
    - id: finalize
      shell: sh
      run: "true"
`)
	writeFixtureFile(t, workspace, ".github/actions/rust-cache/action.yml", `name: rust-cache
runs:
  using: node24
  main: main.js
  post: post.js
  post-if: (success() || env.CACHE_ON_FAILURE == 'true') && env.PARENT_FLAG == 'true' && steps.finalize.conclusion == 'success'
`)
	writeFixtureFile(t, workspace, ".github/actions/rust-cache/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/rust-cache/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
case "$(basename "$1")" in
  main.js) printf '%s\n' 'CACHE_ON_FAILURE=true' >> "$GITHUB_ENV" ;;
  post.js) touch "$POST_MARKER" ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "post")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "setup", Kind: "uses", Uses: "./.github/actions/setup-rust-toolchain", Env: map[string]string{"PARENT_FLAG": "true"}},
		{ID: "not-yet", Kind: "run", Command: `test ! -e "$POST_MARKER"`},
		{ID: "finalize", Kind: "run", Command: "exit 7"},
	})
	job.Env = map[string]string{"POST_MARKER": marker}
	result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("nested rust-cache post did not run during teardown: %v", err)
	}
}

func TestJavaScriptPostConditionUsesWorkflowInputsAndPostHashFiles(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: lifecycle hashFiles\n")
	writeFixtureFile(t, workspace, "Cargo.lock", "locked")
	writeFixtureFile(t, workspace, ".github/actions/cache/action.yml", `name: cache
runs:
  using: node24
  main: main.js
  post: post.js
  post-if: inputs.cache == true && hashFiles('Cargo.lock') != ''
`)
	writeFixtureFile(t, workspace, ".github/actions/cache/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/cache/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "$(basename "$1")" = post.js ]; then touch "$POST_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "post")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cache", Kind: "uses", Uses: "./.github/actions/cache"}})
	job.Inputs = map[string]any{"cache": true}
	job.Env = map[string]string{"POST_MARKER": marker}
	result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post did not run: %v", err)
	}
}

func TestRunnerToolCacheIsPerJobAndReserved(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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

func TestConcurrentPostActionsRunLIFOByRegistration(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/background/action.yml", "name: Background lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/background/main.js", "require('fs').writeFileSync(process.env.READY, 'ready')\nsetTimeout(() => {}, 100)\n")
	writeFixtureFile(t, workspace, ".github/actions/background/post.js", "console.log('post:background')\n")
	writeFixtureFile(t, workspace, ".github/actions/foreground/action.yml", "name: Foreground lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/foreground/main.js", "console.log('main:foreground')\n")
	writeFixtureFile(t, workspace, ".github/actions/foreground/post.js", "console.log('post:foreground')\n")
	ready := filepath.Join(workspace, "background.ready")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "uses", Uses: "./.github/actions/background", Background: true},
		{ID: "await-background", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "foreground", Kind: "uses", Uses: "./.github/actions/foreground"},
		{ID: "wait-background", Kind: "wait", Targets: []string{"background"}},
	})
	job.Env = map[string]string{"READY": ready}
	var logs bytes.Buffer

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	background := strings.Index(logs.String(), "post:background")
	foreground := strings.Index(logs.String(), "post:foreground")
	if foreground < 0 || background < 0 || foreground > background {
		t.Fatalf("concurrent post logs are not registration-order LIFO: %q", logs.String())
	}
}

func TestPostActionsUseBoundedCleanupContext(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/action.yml", "name: Slow post\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/main.js", "console.log('main completed')\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/post.js", "setTimeout(() => console.log('slow post completed'), 30000)\n")
	runner := Runner{Node24: node, CleanupTimeout: 200 * time.Millisecond}
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
	started := time.Now()
	_, err := runner.runTestJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("bounded cleanup took %s, want under 3s", elapsed)
	}
}

func TestJobTimeoutLimitsPostActionsToCleanupGrace(t *testing.T) {
	t.Parallel()

	workspace := canonicalTempDir(t)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/action.yml", "name: Job timeout post\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/slow/post.js", "")
	postStarted := filepath.Join(workspace, "post-started")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "$(basename "$1")" = post.js ]; then : > "$POST_STARTED"; fi
sleep 30
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
	// Leave enough of the job budget for process discovery on slower Darwin
	// hosts; the post still has only the separate 250 ms cleanup grace.
	job.TimeoutMinutes = 0.01
	job.Env = map[string]string{"POST_STARTED": postStarted}
	attachTestProgram(&job)
	runner := Runner{
		Node24: fakeNode, CleanupTimeout: 250 * time.Millisecond, PostActionTimeout: 3 * time.Second,
		InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond,
	}
	started := time.Now()
	result, err := runner.runTestJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(postStarted); err != nil {
		t.Fatalf("post action did not start during cleanup grace: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("job-timeout cleanup took %s, want cleanup grace rather than 3s post budget", elapsed)
	}
}

func TestCancellationStillRunsRegisteredPostAction(t *testing.T) {
	t.Parallel()

	workspace := canonicalTempDir(t)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/action.yml", "name: Cancellation cleanup\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/cancel/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "$(basename "$1")" = post.js ]; then echo post-after-cancel; exit 0; fi
sleep 30
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "cancel", Kind: "uses", Uses: "./.github/actions/cancel"}})
	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	result, err := (Runner{Node24: fakeNode, Stdout: &logs, Stderr: &logs}).runTestJob(ctx, job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" || !strings.Contains(logs.String(), "post-after-cancel") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestPriorFailureSkipsLaterStepEnvironmentWithoutStatusCondition(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{
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

func TestExplicitBackgroundCancelStillRunsRegisteredPostAction(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/action.yml", "name: Explicit cancellation cleanup\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/main.js", "require('fs').writeFileSync(process.env.READY, 'ready')\nsetInterval(() => {}, 30000)\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/post.js", "console.log('post-after-explicit-cancel')\n")
	ready := filepath.Join(workspace, "action.ready")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{
		{ID: "background", Kind: "uses", Uses: "./.github/actions/cancel", Background: true},
		{ID: "await-start", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	})
	job.Env = map[string]string{"READY": ready}

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || !strings.Contains(logs.String(), "post-after-explicit-cancel") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
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

func TestRunDockerRejectsInvalidEnvironmentNamesBeforeDocker(t *testing.T) {
	for _, name := range []string{"", "GITHUB_SHA=ALIAS", "NUL\x00NAME"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "docker-ran")
			docker := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(docker, []byte("#!/bin/sh\ntouch "+shellTestQuote(marker)+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			action := fakeDockerAction(t)
			action.runnerTemp = t.TempDir()
			action.Env = map[string]string{name: "value"}
			_, err := newJobRun(Runner{Docker: docker}).runDocker(t.Context(), newCommandOutputProcessor(io.Discard, io.Discard), action)
			if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
				t.Fatalf("runDocker() error = %v, want invalid environment name", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Docker was invoked with invalid environment name: %v", statErr)
			}
		})
	}
}

func TestRunJobLogsSynchronousStepSectionsAndExpandsFailures(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: step sections\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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

func TestMiseNodeSelectionIsExactAndConfigFree(t *testing.T) {
	root := canonicalTempDir(t)
	log := filepath.Join(root, "args")
	dataDir := filepath.Join(root, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node20Version)
	node := filepath.Join(installation, "bin", "node")
	nodeBytes := []byte("#!/bin/sh\nprintf 'v20.20.2\\n'\n")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nprintf 'mise progress\\n' >&2\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", log, installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nodeBytes)
	r := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, nodeDigests: map[int]string{20: hex.EncodeToString(digest[:])}})
	got, err := r.discoverNode(t.Context(), 20, "")
	if err != nil || got != node {
		t.Fatalf("discoverNode() = %q, %v", got, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--no-config install core:node@20.20.2\n--no-config where core:node@20.20.2\n" {
		t.Fatalf("mise arguments = %q", data)
	}
}

func TestMiseNode16SelectionIsExactAndConfigFree(t *testing.T) {
	root := canonicalTempDir(t)
	log := filepath.Join(root, "args")
	dataDir := filepath.Join(root, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node16Version)
	node := filepath.Join(installation, "bin", "node")
	nodeBytes := []byte("#!/bin/sh\nprintf 'v16.20.2\\n'\n")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", log, installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nodeBytes)
	got, err := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, nodeDigests: map[int]string{16: hex.EncodeToString(digest[:])}}).discoverNode(t.Context(), 16, "")
	if err != nil || got != node {
		t.Fatalf("discoverNode() = %q, %v", got, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--no-config install core:node@16.20.2\n--no-config where core:node@16.20.2\n" {
		t.Fatalf("mise arguments = %q", data)
	}
}

func TestMiseNodeInstallationAllowsSymlinkedDataDirAncestor(t *testing.T) {
	base := canonicalTempDir(t)
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	dataDir := filepath.Join(logicalParent, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	writeNodeExecutable(t, node, 24)
	mise := filepath.Join(base, "mise")
	writeFixtureFile(t, base, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	gotInstallation, gotNode, err := (Runner{MiseDataDir: dataDir}).miseNodeInstallation(t.Context(), 24, mise)
	if err != nil {
		t.Fatal(err)
	}
	wantInstallation := filepath.Join(realParent, "data", "installs", "node", Node24Version)
	if gotInstallation != wantInstallation || gotNode != filepath.Join(wantInstallation, "bin", "node") {
		t.Fatalf("miseNodeInstallation() = %q, %q; want canonical paths under %q", gotInstallation, gotNode, wantInstallation)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeNodeExecutable(t, filepath.Join(outside, "node"), 24)
	if err := os.RemoveAll(filepath.Join(wantInstallation, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wantInstallation, "bin")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Runner{MiseDataDir: dataDir}).miseNodeInstallation(t.Context(), 24, mise); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("miseNodeInstallation() accepted symlinked bin directory: %v", err)
	}
}

func TestMiseNodePathIgnoresProgressOnStderr(t *testing.T) {
	root := canonicalTempDir(t)
	nodeRoot := filepath.Join(root, "node")
	if err := os.MkdirAll(filepath.Join(nodeRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(nodeRoot, "bin", "node")
	writeNodeExecutable(t, node, 24)
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf 'mise progress\\n' >&2\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", nodeRoot))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongBytes, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := sha256.Sum256(wrongBytes)
	if _, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(wrongDigest[:])}}).resolveMiseNodePath(t.Context(), 24); err == nil || !strings.Contains(err.Error(), `reported "v24.99.0", want "v24.18.0"`) {
		t.Fatalf("resolveMiseNodePath() error = %v, want exact executable version rejection", err)
	}
	correctBytes := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n")
	if err := os.WriteFile(node, correctBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	correctDigest := sha256.Sum256(correctBytes)
	got, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(correctDigest[:])}}).resolveMiseNodePath(t.Context(), 24)
	if err != nil || got != filepath.Join(nodeRoot, "bin", "node") {
		t.Fatalf("resolveMiseNodePath() = %q, %v", got, err)
	}
}

func TestManagedMiseCacheReplacesNodeWithWrongDigest(t *testing.T) {
	root := canonicalTempDir(t)
	dataDir := filepath.Join(root, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	poisoned := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n# poisoned\n")
	if err := os.WriteFile(node, poisoned, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n# replacement\n")
	digest := sha256.Sum256(replacement)
	mise := filepath.Join(root, "mise")
	script := fmt.Sprintf(`#!/bin/sh
node=%q
installation=%q
case "$2" in
  install)
    if [ ! -x "$node" ]; then
      mkdir -p "$(dirname "$node")"
      cat > "$node" <<'NODE'
%sNODE
      chmod 0755 "$node"
    fi
    ;;
  where) printf '%%s\n' "$installation" ;;
  *) exit 9 ;;
esac
`, node, installation, replacement)
	if err := os.WriteFile(mise, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	verification := &managedNodeVerification{paths: make(map[int]string)}
	runner := newJobRun(Runner{
		Mise:        mise,
		MiseDataDir: dataDir,
		nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])},
	})
	runner.nodeVerification = verification
	if got, err := runner.discoverNode(t.Context(), 24, ""); err != nil || got != node {
		t.Fatalf("discoverNode() = %q, %v", got, err)
	}
	got, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) || verification.paths[24] != node {
		t.Fatalf("replacement node = %q, verified = %#v", got, verification.paths)
	}
}

func TestManagedMiseCacheRefusesSymlinkedRemoval(t *testing.T) {
	dataDir := canonicalTempDir(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dataDir, "installs")); err != nil {
		t.Fatal(err)
	}
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	if err := removeManagedNodeInstallation(dataDir, installation); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("removeManagedNodeInstallation() error = %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory was affected: %v", err)
	}
}

func TestJavaScriptPhaseUsesVerifiedMiseNodeWithoutWorkflowRedirection(t *testing.T) {
	root := canonicalTempDir(t)
	log := filepath.Join(root, "node-args")
	dataDir := filepath.Join(root, "mise-data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeBytes := fmt.Appendf(nil, "#!/bin/sh\nif [ \"${1:-}\" = --version ]; then printf 'v24.18.0\\n'; else printf '%%s|MISE_DATA_DIR=%%s\\n' \"$*\" \"${MISE_DATA_DIR-unset}\" >> %q; fi\n", log)
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, root, "main.js", "")
	node20 := filepath.Join(root, "node20")
	writeNodeExecutable(t, node20, 20)
	digest := sha256.Sum256(nodeBytes)
	runner := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, Node20: node20, nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])}})
	resolvedNode, err := runner.discoverNode(t.Context(), 24, "")
	if err != nil {
		t.Fatal(err)
	}
	result := newResult()
	action := javaScriptAction{Name: "mise", Path: root, Main: "main.js", Env: map[string]string{"MISE_DATA_DIR": "/workflow-controlled"}, nodeMajor: 24}
	if err := runner.runJavaScriptPhase(t.Context(), newCommandOutputProcessor(io.Discard, io.Discard), root, resolvedNode, action, action.Main, nil, nil, &result); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "main.js") + "|MISE_DATA_DIR=/workflow-controlled\n"
	if string(data) != want {
		t.Fatalf("Node invocations = %q, want %q", data, want)
	}
}

func TestMiseMissingIsClear(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := newJobRun(Runner{}).discoverNode(t.Context(), 24, "")
	if err == nil || !strings.Contains(err.Error(), "mise is required") {
		t.Fatalf("discoverNode() error = %v", err)
	}
}

func TestMiseNodeSelectionRejectsWrongExactVersion(t *testing.T) {
	root := canonicalTempDir(t)
	installation := filepath.Join(root, "node")
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeBytes := []byte("#!/bin/sh\nprintf 'v24.18.1\\n'\n")
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nodeBytes)
	_, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])}}).discoverNode(t.Context(), 24, "")
	if err == nil || !strings.Contains(err.Error(), `reported "v24.18.1", want "v24.18.0"`) {
		t.Fatalf("discoverNode() error = %v", err)
	}
}

func TestDockerAction(t *testing.T) {
	docker := requireDocker(t)
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs, Docker: docker}
	result, err := runner.runDockerAction(t.Context(), dockerAction{
		Name: "local Docker", Path: fixturePath(t, "actions", "docker"), Workspace: fixturePath(t),
		Env: map[string]string{"INPUT_EXPECTED_FILE": "smoke/.github/workflows/ci.yml"},
	})

	if err != nil {
		t.Fatalf("runDockerAction() error = %v", err)
	}
	if result.Outputs["container"] != "ran" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" {
		t.Errorf("Docker result = %#v", result)
	}
	if result.Summary != "docker action summary\n" {
		t.Errorf("Docker summary = %q", result.Summary)
	}
	if strings.Contains(logs.String(), "docker-secret-value") {
		t.Fatalf("raw forwarded Docker logs contain literal secret: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "masked docker probe: ***") {
		t.Errorf("Docker logs = %q, want masked probe", logs.String())
	}
}

func TestPrebuiltDockerAction(t *testing.T) {
	docker := requireDocker(t)
	var logs bytes.Buffer
	result, err := (Runner{Docker: docker, Stdout: &logs, Stderr: &logs}).runDockerAction(t.Context(), dockerAction{
		Name:       "prebuilt Docker",
		Image:      "busybox@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028",
		Entrypoint: "/bin/sh",
		Args: []string{"-c", `printf 'prebuilt=ran\n' >> "$GITHUB_OUTPUT"
printf '%s\n' '::add-mask::prebuilt-secret'
printf '%s\n' 'masked prebuilt probe: prebuilt-secret'`},
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runDockerAction() error = %v", err)
	}
	if result.Outputs["prebuilt"] != "ran" {
		t.Fatalf("prebuilt Docker result = %#v", result)
	}
	if strings.Contains(logs.String(), "prebuilt-secret") || !strings.Contains(logs.String(), "masked prebuilt probe: ***") {
		t.Fatalf("prebuilt Docker logs were not masked: %q", logs.String())
	}
}

func TestDockerActionArgsPreserveEntrypointAndControlCMD(t *testing.T) {
	docker := requireDocker(t)
	action := t.TempDir()
	writeFixtureFile(t, action, "main.go", `package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	encoded, err := json.Marshal(os.Args[1:])
	if err != nil {
		panic(err)
	}
	output, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		panic(err)
	}
	defer output.Close()
	if _, err := fmt.Fprintf(output, "args=%s\n", encoded); err != nil {
		panic(err)
	}
}
`)
	binary := filepath.Join(action, "entrypoint")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, filepath.Join(action, "main.go"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Docker args entrypoint: %v: %s", err, output)
	}
	writeFixtureFile(t, action, "Dockerfile", "FROM scratch\nCOPY entrypoint /entrypoint\nENTRYPOINT [\"/entrypoint\"]\nCMD [\"image-default\"]\n")

	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "omitted preserves CMD", want: []string{"image-default"}},
		{name: "nonempty replaces CMD", args: []string{"", "  ", "--privileged", `$(echo no-shell)`}, want: []string{"", "  ", "--privileged", `$(echo no-shell)`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := (Runner{Docker: docker}).runDockerAction(t.Context(), dockerAction{Name: "Docker args", Path: action, Workspace: t.TempDir(), Args: test.args})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			if err := json.Unmarshal([]byte(result.Outputs["args"]), &got); err != nil {
				t.Fatalf("decode observed argv %q: %v", result.Outputs["args"], err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("entrypoint argv = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestDockerActionSupportsImageDefaultNonRootUser(t *testing.T) {
	docker := requireDocker(t)
	action := t.TempDir()
	writeFixtureFile(t, action, "main.go", `package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	workspace := os.Getenv("GITHUB_WORKSPACE")
	if workspace != "/github/workspace" || os.Getenv("RUNNER_TEMP") != "/github/runner_temp" {
		panic("container paths were not translated")
	}
	if err := os.WriteFile(filepath.Join(workspace, "nonroot-workspace"), []byte("written"), 0o600); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("RUNNER_TEMP"), "nonroot-temp"), []byte("written"), 0o600); err != nil {
		panic(err)
	}
	output, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		panic(err)
	}
	defer output.Close()
	if _, err := fmt.Fprintln(output, "nonroot=written"); err != nil {
		panic(err)
	}
}
`)
	binary := filepath.Join(action, "entrypoint")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, filepath.Join(action, "main.go"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build non-root action entrypoint: %v: %s", err, output)
	}
	writeFixtureFile(t, action, "Dockerfile", "FROM scratch\nCOPY entrypoint /entrypoint\nUSER 65534:65534\nENTRYPOINT [\"/entrypoint\"]\n")
	workspace := t.TempDir()
	before, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Docker: docker}).runDockerAction(t.Context(), dockerAction{Name: "non-root Docker", Path: action, Workspace: workspace})
	if err != nil {
		t.Fatalf("runDockerAction() error = %v", err)
	}
	if result.Outputs["nonroot"] != "written" {
		t.Fatalf("Docker result = %#v", result)
	}
	if info, err := os.Stat(filepath.Join(workspace, "nonroot-workspace")); err != nil || info.Size() != int64(len("written")) {
		t.Fatalf("non-root workspace output = %v, %v", info, err)
	}
	after, err := os.Stat(workspace)
	if err != nil || after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("workspace mode after Docker action = %v, %v; want %v", after, err, before.Mode().Perm())
	}
}

func TestRunJobShellJavaScriptCompositeAndPost(t *testing.T) {
	node := requireNode24(t)
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
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

func TestRunJobPythonShellUsesTemporaryScript(t *testing.T) {
	installPythonShellTestCommand(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:    "python",
		Kind:  "run",
		Shell: "python",
		Command: `import os
from pathlib import Path

assert Path.cwd() == Path(os.environ["GITHUB_WORKSPACE"])
assert Path(__file__).suffix == ".py"
with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
    output.write(f"script={__file__}\n")
`,
	}})
	job.Outputs = map[string]string{"script": "${{ steps.python.outputs.script }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	script := result.Outputs["script"]
	if !strings.HasSuffix(script, ".py") {
		t.Fatalf("Python script path = %q, want .py suffix", script)
	}
	if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary Python script remains at %q: %v", script, err)
	}
}

func TestRunJobCustomShellTemplatesUseTemporaryScripts(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  string
		shell    string
		wantArgs []string
	}{
		{name: "R", command: "Rscript", shell: `Rscript --vanilla "two words" {0} --after='quoted value'`, wantArgs: []string{"--vanilla", "two words"}},
		{name: "Julia", command: "julia", shell: `julia --color=yes {0} --project "two words"`, wantArgs: []string{"--color=yes"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := installCustomShellTestCommand(t, test.command)
			workspace := t.TempDir()
			arguments := filepath.Join(workspace, "arguments")
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{
				{ID: "path", Kind: "run", Shell: "sh", Command: `printf '%s\n' "$CUSTOM_SHELL_BIN" >> "$GITHUB_PATH"`, Env: map[string]string{"CUSTOM_SHELL_BIN": bin}},
				{
					ID:      "custom",
					Kind:    "run",
					Shell:   test.shell,
					Env:     map[string]string{"CUSTOM_SHELL_ARGS": arguments},
					Command: `printf 'script=%s\n' "$0" >> "$GITHUB_OUTPUT"`,
				},
			})
			job.Outputs = map[string]string{"script": "${{ steps.custom.outputs.script }}"}
			result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
			if err != nil {
				t.Fatalf("RunJob() error = %v", err)
			}
			script := result.Outputs["script"]
			if base := filepath.Base(script); !strings.HasPrefix(base, "buildkite-gha-shell-") || filepath.Ext(base) != "" {
				t.Fatalf("custom shell script path = %q, want extensionless temporary script", script)
			}
			if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary custom shell script remains at %q: %v", script, err)
			}
			data, err := os.ReadFile(arguments)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(got) < len(test.wantArgs)+1 || !slices.Equal(got[:len(test.wantArgs)], test.wantArgs) || got[len(test.wantArgs)] != script {
				t.Fatalf("custom shell arguments = %#v, want prefix %#v followed by %q", got, test.wantArgs, script)
			}
		})
	}
}

func TestRunJobLoginBashTemplate(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "login", Kind: "run", Shell: "bash -l {0}", Command: `printf 'value=login-bash\n' >> "$GITHUB_OUTPUT"`}})
	job.Outputs = map[string]string{"value": "${{ steps.login.outputs.value }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Outputs["value"] != "login-bash" {
		t.Fatalf("RunJob() output = %q, error = %v, want login-bash", result.Outputs["value"], err)
	}
}

func TestCompositePythonShell(t *testing.T) {
	installPythonShellTestCommand(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Python composite
outputs:
  value:
    value: ${{ steps.python.outputs.value }}
runs:
  using: composite
  steps:
    - id: python
      shell: python
      run: |
        import os
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            output.write("value=python-composite\n")
`)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	job.Outputs = map[string]string{"value": "${{ steps.composite.outputs.value }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Outputs["value"] != "python-composite" {
		t.Fatalf("RunJob() output = %q, want python-composite", result.Outputs["value"])
	}
}

func installCustomShellTestCommand(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	wrapper := `#!/bin/sh
: > "$CUSTOM_SHELL_ARGS"
script=
for arg do
  printf '%s\n' "$arg" >> "$CUSTOM_SHELL_ARGS"
  case "$arg" in
    */buildkite-gha-shell-*) script=$arg ;;
  esac
done
test -n "$script"
exec /bin/sh "$script"
`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func installPythonShellTestCommand(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	python := filepath.Join(dir, "python")
	wrapper := "#!/bin/sh\nBUILDKITE_GHA_TEST_PYTHON_HELPER=1 exec " + shellTestQuote(executable) + " -test.run '^TestPythonShellCommandHelper$' -- \"$@\"\n"
	if err := os.WriteFile(python, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPythonShellCommandHelper(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_TEST_PYTHON_HELPER") == "" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatalf("Python helper arguments = %#v", os.Args)
	}
	script := os.Args[separator+1]
	source, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(script) != ".py" {
		t.Fatalf("Python script = %q, want .py suffix", script)
	}
	if cwd, err := os.Getwd(); err != nil || cwd != os.Getenv("GITHUB_WORKSPACE") {
		t.Fatalf("working directory = %q, %v; workspace = %q", cwd, err, os.Getenv("GITHUB_WORKSPACE"))
	}
	var output string
	switch {
	case bytes.Contains(source, []byte(`script={__file__}`)):
		output = "script=" + script + "\n"
	case bytes.Contains(source, []byte(`value=python-composite`)):
		output = "value=python-composite\n"
	default:
		t.Fatalf("unexpected Python source: %s", source)
	}
	file, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(output); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompositeStepWorkingDirectoryEvaluatesExpressions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Working-directory composite
inputs:
  dir:
    required: false
runs:
  using: composite
  steps:
    - shell: bash
      working-directory: ${{ inputs.dir || '.' }}
      run: test "$(basename "$PWD")" = sub
    - shell: bash
      working-directory: literal-dir
      run: test "$(basename "$PWD")" = literal-dir
`)
	for _, directory := range []string{"sub", "literal-dir"} {
		if err := os.Mkdir(filepath.Join(workspace, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite", With: map[string]string{"dir": "sub"}}})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestShellStepsMissingWorkingDirectoryFailClearly(t *testing.T) {
	for _, test := range []struct {
		name  string
		steps []plan.Step
		setup func(*testing.T, string)
	}{
		{
			name:  "workflow",
			steps: []plan.Step{{ID: "run", Kind: "run", Shell: "sh", Command: "true", WorkingDirectory: "missing"}},
		},
		{
			name:  "composite",
			steps: []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}},
			setup: func(t *testing.T, workspace string) {
				t.Helper()
				writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "name: composite\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      working-directory: missing\n      run: true\n")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			if test.setup != nil {
				test.setup(t, workspace)
			}
			job := runtimePlan(t, workspace, workflowPath, test.steps)
			if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `working directory "missing" does not exist`) {
				t.Fatalf("RunJob() error = %v, want missing working directory diagnostic", err)
			}
		})
	}
}

func TestCompositeShellWorkingDirectoryFailurePrecedesShellEvaluation(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "name: composite\nruns:\n  using: composite\n  steps:\n    - shell: ${{ fromJSON('invalid') }}\n      working-directory: missing\n      run: true\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	_, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `working directory "missing" does not exist`) || strings.Contains(err.Error(), "fromJSON") {
		t.Fatalf("RunJob() error = %v, want only the prior working-directory failure", err)
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "outer", Kind: "uses", Uses: "./.github/actions/outer", With: map[string]string{"parent-only": "private-to-parent"}}})
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

func TestCompositeGitHubActionPathExpressionIsInvocationScoped(t *testing.T) {
	workspace := canonicalTempDir(t)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/inner/action.yml", `name: Inner
inputs:
  caller-path:
    required: true
runs:
  using: composite
  steps:
    - shell: sh
      env:
        ENV_ACTION_PATH: ${{ github.action_path }}
        WITH_CALLER_PATH: ${{ inputs.caller-path }}
      run: |
        test "${{ github.action_path }}" = "$EXPECTED_INNER_PATH"
        test "$ENV_ACTION_PATH" = "$EXPECTED_INNER_PATH"
        test "$WITH_CALLER_PATH" = "$EXPECTED_OUTER_PATH"
`)
	writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", `name: Outer
runs:
  using: composite
  steps:
    - shell: sh
      run: |
        test "${{ github.action_path }}" = "$EXPECTED_OUTER_PATH"
        test "${{ github.action_path }}" = "$GITHUB_ACTION_PATH"
        test -f "${{ github.action_path }}/action.yml"
        test "${{ github.workflow }}" = "$EXPECTED_WORKFLOW"
        test "${{ github.job }}" = fixture
    - uses: ./.github/actions/inner
      with:
        caller-path: ${{ github.action_path }}
    - shell: sh
      run: test "${{ github.action_path }}" = "$EXPECTED_OUTER_PATH"
`)
	steps := []plan.Step{
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/outer"},
		{ID: "top", Kind: "run", Command: `test -z "${{ github.action_path }}"`},
	}
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{
		"EXPECTED_OUTER_PATH": filepath.Join(workspace, ".github", "actions", "outer"),
		"EXPECTED_INNER_PATH": filepath.Join(workspace, ".github", "actions", "inner"),
		"EXPECTED_WORKFLOW":   workflowPath,
	}
	var logs bytes.Buffer
	if _, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace); err != nil {
		t.Fatalf("RunJob() error = %v\nlogs: %s", err, logs.String())
	}
}

func TestRemoteCompositeGitHubActionIdentityExpressionsAreInvocationScoped(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: action identity\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "composite/action.yml", `name: Identity
runs:
  using: composite
  steps:
    - shell: sh
      env:
        ACTION_USER_AGENT: ${{ github.action_repository }}@${{ github.action_ref }}
      run: test "$ACTION_USER_AGENT" = owner/repo@v1
`)
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "identity", Kind: "uses", Uses: remoteLifecycleUses("composite"), Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "composite", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	if result, err := (Runner{Actions: materializer}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestNestedRemoteCompositeInvocationFieldsRetainCallerIdentityForPreAndMain(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested action identity\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", `name: Root
runs:
  using: composite
  steps:
    - uses: child-owner/child-action/child@child-ref
      env:
        CALLER_IDENTITY: ${{ github.action_repository }}@${{ github.action_ref }}
`)
	writeFixtureFile(t, remote, "child/action.yml", `name: Child
runs:
  using: composite
  steps:
    - uses: js-owner/js-action/js@js-ref
`)
	writeFixtureFile(t, remote, "js/action.yml", "name: JavaScript\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "js/pre.js", "")
	writeFixtureFile(t, remote, "js/main.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
test "$CALLER_IDENTITY" = root-owner/root-action@root-ref
printf '%s\n' "$(basename "$1" .js)" >> "$PHASES"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID, jsID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3)
	rootLock := remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{"child-owner/child-action/child@child-ref": {Lock: childID}})
	rootLock.Repository, rootLock.RequestedRef = "root-owner/root-action", "root-ref"
	childLock := remoteLifecycleLock(childID, "child", digest, map[string]plan.ActionSelector{"js-owner/js-action/js@js-ref": {Lock: jsID}})
	childLock.Repository, childLock.RequestedRef = "child-owner/child-action", "child-ref"
	jsLock := remoteLifecycleLock(jsID, "js", digest, nil)
	jsLock.Repository, jsLock.RequestedRef = "js-owner/js-action", "js-ref"
	phases := filepath.Join(workspace, "phases")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "identity", Kind: "uses", Uses: "root-owner/root-action/root@root-ref", Action: &plan.ActionSelector{Lock: rootID},
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"PHASES": phases}
	job.Actions = []plan.ActionLock{rootLock, childLock, jsLock}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	if result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	got, err := os.ReadFile(phases)
	if err != nil || string(got) != "pre\nmain\n" {
		t.Fatalf("phases = %q, %v; want pre and main", got, err)
	}
}

func TestExpressionTimeoutBoundsNestedCompositePre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested pre timeout\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", "name: root\nruns:\n  using: composite\n  steps:\n    - uses: "+remoteLifecycleUses("child")+"\n      continue-on-error: true\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "$(basename "$(dirname "$1")")/$(basename "$1")" = child/pre.js ]; then sleep 5; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}, TimeoutMinutesExpression: "${{ matrix.timeout }}",
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Matrix = map[string]any{"timeout": 0.001}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	started := time.Now()
	_, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() nested pre timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("RunJob() nested pre took %s after expression timeout", elapsed)
	}
}

func TestNestedRemoteCompositePreSupportsCompoundStepFields(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested pre expressions\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", `name: root
inputs:
  targets:
    required: false
  target:
    required: false
runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      with:
        message: ${{ inputs.targets || inputs.target || '' }}
      env:
        TARGETS: ${{ inputs.targets || inputs.target || '' }}
`)
	writeFixtureFile(t, remote, "child/action.yml", "name: child\ninputs:\n  message:\n    required: false\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
test -z "$INPUT_MESSAGE"
test -z "$TARGETS"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRemoteCompositePreExposesActionPathToChild(t *testing.T) {
	workspace := canonicalTempDir(t)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: pre action path\n")
	remote := canonicalTempDir(t)
	writeFixtureFile(t, remote, "root/action.yml", `name: root
runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      with:
        caller_path: ${{ github.action_path }}
      env:
        CALLER_PATH_ENV: ${{ github.action_path }}
`)
	writeFixtureFile(t, remote, "child/action.yml", "name: child\ninputs:\n  caller_path:\n    required: false\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
test "$INPUT_CALLER_PATH" = "$EXPECTED_ROOT_PATH"
test "$CALLER_PATH_ENV" = "$EXPECTED_ROOT_PATH"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"EXPECTED_ROOT_PATH": filepath.Join(remote, "root")}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	var logs bytes.Buffer
	result, err := (Runner{Node24: fakeNode, Actions: materializer, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v\nlogs: %s", result, err, logs.String())
	}
}

func TestSkippedRemoteCompositeWithoutPreDoesNotEvaluateTimeout(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: skipped composite timeout\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", "name: root\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n")
	digest := digestTree(t, remote)
	rootID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}, Condition: "false", TimeoutMinutesExpression: "${{ fromJSON('invalid') }}",
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(rootID, "root", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	if result, err := (Runner{Actions: materializer}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() skipped composite result = %#v, error = %v", result, err)
	}
}

func TestCompileKnownFalseRootActionDoesNotPrepareRemoteLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: skipped remote lifecycle\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", "name: root\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "root/pre.js", "")
	writeFixtureFile(t, remote, "root/main.js", "")
	digest := digestTree(t, remote)
	rootID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}, Condition: "false",
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(rootID, "root", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() skipped root result = %#v, error = %v", result, err)
	}
	if materializer.calls != 0 {
		t.Fatalf("known-false root materializations = %d, want 0", materializer.calls)
	}
}

func TestNestedCompositePreservesInheritedJobStatus(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/status/action.yml", `name: Observe status
inputs:
  job-status:
    default: ${{ job.status }}
runs:
  using: composite
  steps:
    - if: always()
      shell: sh
      run: printf '%s' '${{ inputs.job-status }}' > "$STATUS_MARKER"
`)
	writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", `name: Outer
runs:
  using: composite
  steps:
    - if: always()
      uses: ./.github/actions/status
`)
	marker := filepath.Join(workspace, "status")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fail", Kind: "run", Command: "exit 7"},
		{ID: "outer", Kind: "uses", Uses: "./.github/actions/outer", Condition: "always()"},
	})
	job.Env = map[string]string{"STATUS_MARKER": marker}

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v; want original step failure", result, err)
	}
	status, readErr := os.ReadFile(marker)
	if readErr != nil || string(status) != "failure" {
		t.Fatalf("nested action job.status = %q, %v; want failure", status, readErr)
	}
}

func TestNestedJavaScriptPostSharesJobLIFORegistry(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	for _, name := range []string{"top", "nested"} {
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/main.js", "")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/post.js", "")
	}
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Nested lifecycle
runs:
  using: composite
  steps:
    - id: child
      uses: ./.github/actions/nested
`)
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "top", Kind: "uses", Uses: "./.github/actions/top"},
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"},
	})
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	if result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(events), "top:main\nnested:main\nnested:post\ntop:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}

	if err := os.Remove(lifecycle); err != nil {
		t.Fatal(err)
	}
	topID, compositeID, nestedID := "a-0000000000000001", "a-0000000000000002", "a-0000000000000003"
	job.Schema = plan.Schema
	job.Steps[0].Action = &plan.ActionSelector{Lock: topID}
	job.Steps[1].Action = &plan.ActionSelector{Lock: compositeID}
	job.Actions = []plan.ActionLock{
		{ID: topID, Source: "workspace", Path: ".github/actions/top", SourceDigest: digestTree(t, filepath.Join(workspace, ".github", "actions", "top"))},
		{
			ID: compositeID, Source: "workspace", Path: ".github/actions/composite", SourceDigest: digestTree(t, filepath.Join(workspace, ".github", "actions", "composite")),
			Children: map[string]plan.ActionSelector{"./.github/actions/nested": {Lock: nestedID}},
		},
		{ID: nestedID, Source: "workspace", Path: ".github/actions/nested", SourceDigest: digestTree(t, filepath.Join(workspace, ".github", "actions", "nested"))},
	}
	if result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("v3 RunJob() result = %#v, error = %v", result, err)
	}
	events, err = os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(events), "top:main\nnested:main\nnested:post\ntop:post\n"; got != want {
		t.Fatalf("v3 lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionPreHooksRunBeforeJobMainInDepthFirstOrder(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: remote lifecycle ordering\n")
	remote := t.TempDir()
	for i, name := range []string{"first", "skipped", "second"} {
		using := "node24"
		if i == 0 {
			using = "node20"
		}
		postIf := ""
		if name == "skipped" {
			postIf = "  post-if: steps.failed.outcome == 'failure' && steps.skipped.outcome == 'skipped' && steps.second.outcome == 'success'\n"
		}
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: "+using+"\n  pre: pre.js\n  main: main.js\n  post: post.js\n"+postIf)
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, name+"/"+phase+".js", "")
		}
	}
	writeFixtureFile(t, remote, "root/action.yml", `name: root
runs:
  using: composite
  steps:
    - uses: ./local
    - uses: owner/repo/first@v1
    - uses: owner/repo/nested@v1
`)
	writeFixtureFile(t, workspace, "local/action.yml", `name: local
runs:
  using: composite
  steps:
    - shell: sh
      run: printf '%s\n' 'local:main' >> "$LIFECYCLE_LOG"
`)
	writeFixtureFile(t, remote, "nested/action.yml", `name: nested
runs:
  using: composite
  steps:
    - id: failed
      shell: sh
      run: exit 7
    - id: skipped
      uses: owner/repo/skipped@v1
    - id: second
      if: always()
      uses: owner/repo/second@v1
`)
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	preBin := filepath.Join(workspace, "pre-bin")
	if err := os.Mkdir(preBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, preBin, "pre-tool", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(preBin, "pre-tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
case "$phase" in
  pre)
    printf 'owner=%s\n' "$action" >> "$GITHUB_STATE"
    if [ "$action" = first ]; then
      printf '%s\n' 'PRE_ENV=visible' >> "$GITHUB_ENV"
      printf '%s\n' "$PRE_BIN" >> "$GITHUB_PATH"
      printf '%s\n' '::add-mask::remote-pre-secret'
    fi
    if [ "$action" = second ]; then printf '%s\n' 'remote-pre-secret'; fi
    ;;
  main)
    test "$PRE_ENV" = visible
    test "$(command -v pre-tool)" = "$PRE_BIN/pre-tool"
    printf 'main=%s\n' "$action" >> "$GITHUB_STATE"
    ;;
  post)
    test "$STATE_owner" = "$action"
    if [ "$action" != skipped ]; then test "$STATE_main" = "$action"; fi
    ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}

	digest := digestTree(t, remote)
	rootID, firstID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	nestedID, skippedID, secondID := remoteLifecycleLockID(3), remoteLifecycleLockID(4), remoteLifecycleLockID(5)
	localID := remoteLifecycleLockID(6)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "ordinary", Kind: "run", Command: `test "$PRE_ENV" = visible
test "$(command -v pre-tool)" = "$PRE_BIN/pre-tool"
printf '%s\n' 'job:main' >> "$LIFECYCLE_LOG"`},
		{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle, "PRE_BIN": preBin}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{
			"./local":                     {Lock: localID},
			remoteLifecycleUses("first"):  {Lock: firstID},
			remoteLifecycleUses("nested"): {Lock: nestedID},
		}),
		remoteLifecycleLock(firstID, "first", digest, nil),
		remoteLifecycleLock(nestedID, "nested", digest, map[string]plan.ActionSelector{
			remoteLifecycleUses("skipped"): {Lock: skippedID},
			remoteLifecycleUses("second"):  {Lock: secondID},
		}),
		remoteLifecycleLock(skippedID, "skipped", digest, nil),
		remoteLifecycleLock(secondID, "second", digest, nil),
		{ID: localID, Source: "workspace", Path: "local", SourceDigest: digestTree(t, filepath.Join(workspace, "local"))},
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	var logs bytes.Buffer
	result, err := (Runner{Node24: fakeNode, Actions: materializer, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	want := "first:pre\nskipped:pre\nsecond:pre\njob:main\nlocal:main\nfirst:main\nsecond:main\nsecond:post\nskipped:post\nfirst:post\n"
	if got := string(events); got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
	if strings.Contains(logs.String(), "remote-pre-secret") || !strings.Contains(logs.String(), "***") {
		t.Fatalf("pre masking logs = %q", logs.String())
	}
	pathCount := 0
	for _, entry := range filepath.SplitList(result.Env["PATH"]) {
		if entry == preBin {
			pathCount++
		}
	}
	if result.Env["PRE_ENV"] != "visible" || pathCount != 1 {
		t.Fatalf("pre environment = %#v, pre path count = %d", result.Env, pathCount)
	}
}

func TestRemoteActionPostRegistrationFollowsStartedLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: skipped remote lifecycle\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "with-pre/action.yml", "name: with pre\ninputs:\n  token:\n    required: true\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, remote, "with-pre/"+phase+".js", "")
	}
	writeFixtureFile(t, remote, "without-pre/action.yml", "name: without pre\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, remote, "without-pre/main.js", "")
	writeFixtureFile(t, remote, "without-pre/post.js", "")
	writeFixtureFile(t, remote, "pre-if-false/action.yml", "name: pre condition false\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: success() && env.RUN_PRE == 'true'\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, remote, "pre-if-false/"+phase+".js", "")
	}
	writeFixtureFile(t, remote, "main-fails/action.yml", "name: main fails\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, remote, "main-fails/main.js", "")
	writeFixtureFile(t, remote, "main-fails/post.js", "")
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
if [ "$action:$phase" = with-pre:pre ]; then test "$INPUT_TOKEN" = ghs_scoped_pre; fi
if [ "$action:$phase" = main-fails:main ]; then exit 7; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	withPreID, withoutPreID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	falsePreID, mainFailsID := remoteLifecycleLockID(3), remoteLifecycleLockID(4)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "with-pre", Kind: "uses", Uses: remoteLifecycleUses("with-pre"), Action: &plan.ActionSelector{Lock: withPreID}, With: map[string]string{"token": "${{ github.token }}"}, Env: map[string]string{"MINUTES": "5"}, TimeoutMinutesExpression: "${{ fromJSON(env.MINUTES) }}"},
		{ID: "without-pre", Kind: "uses", Uses: remoteLifecycleUses("without-pre"), Action: &plan.ActionSelector{Lock: withoutPreID}, Condition: "failure()"},
		{ID: "pre-if-false", Kind: "uses", Uses: remoteLifecycleUses("pre-if-false"), Action: &plan.ActionSelector{Lock: falsePreID}, Condition: "failure()", TimeoutMinutesExpression: "${{ fromJSON('invalid') }}"},
		{ID: "main-fails", Kind: "uses", Uses: remoteLifecycleUses("main-fails"), Action: &plan.ActionSelector{Lock: mainFailsID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network", "provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"contents": "read"}}
	job.Event.Repository = "owner/repo"
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(withPreID, "with-pre", digest, nil),
		remoteLifecycleLock(withoutPreID, "without-pre", digest, nil),
		remoteLifecycleLock(falsePreID, "pre-if-false", digest, nil),
		remoteLifecycleLock(mainFailsID, "main-fails", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	provider := &testWorkflowTokenProvider{token: "ghs_scoped_pre"}
	result, err := (Runner{Node24: fakeNode, Actions: materializer, WorkflowToken: provider, Redactor: &testRedactor{}}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, readErr := os.ReadFile(lifecycle)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(events), "with-pre:pre\nwith-pre:main\nmain-fails:main\nmain-fails:post\nwith-pre:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionMainAndPostEvaluateInputsAfterPriorSteps(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: deferred remote inputs\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", `name: root
inputs:
  message:
    required: true
runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      with:
        message: ${{ inputs.message }}
`)
	writeFixtureFile(t, remote, "child/action.yml", `name: child
inputs:
  message:
    required: true
runs:
  using: node24
  main: main.js
  post: post.js
`)
	writeFixtureFile(t, remote, "child/main.js", "")
	writeFixtureFile(t, remote, "child/post.js", "")
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
test "$INPUT_MESSAGE" = from-producer
printf 'child:%s:%s\n' "$(basename "$1" .js)" "$INPUT_MESSAGE" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "producer", Kind: "run", Command: `printf '%s\n' 'value=from-producer' >> "$GITHUB_OUTPUT"`},
		{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), With: map[string]string{"message": "${{ steps.producer.outputs.value }}"}, Action: &plan.ActionSelector{Lock: rootID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(events), "child:main:from-producer\nchild:post:from-producer\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionPreFailureContinuesPreparationAndFailsJob(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: failed remote pre\n")
	remote := t.TempDir()
	for _, action := range []struct {
		name  string
		preIf string
	}{
		{name: "fails"},
		{name: "after"},
		{name: "on-failure", preIf: "failure()"},
		{name: "on-success", preIf: "success()"},
	} {
		condition := ""
		if action.preIf != "" {
			condition = "  pre-if: " + action.preIf + "\n"
		}
		writeFixtureFile(t, remote, action.name+"/action.yml", "name: "+action.name+"\nruns:\n  using: node24\n  pre: pre.js\n"+condition+"  main: main.js\n  post: post.js\n")
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, action.name+"/"+phase+".js", "")
		}
	}
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
if [ "$action:$phase" = fails:pre ]; then exit 7; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	failsID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	onFailureID, onSuccessID := remoteLifecycleLockID(3), remoteLifecycleLockID(4)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fails", Kind: "uses", Uses: remoteLifecycleUses("fails"), Action: &plan.ActionSelector{Lock: failsID}},
		{ID: "after", Kind: "uses", Uses: remoteLifecycleUses("after"), Action: &plan.ActionSelector{Lock: afterID}},
		{ID: "on-failure", Kind: "uses", Uses: remoteLifecycleUses("on-failure"), Action: &plan.ActionSelector{Lock: onFailureID}},
		{ID: "on-success", Kind: "uses", Uses: remoteLifecycleUses("on-success"), Action: &plan.ActionSelector{Lock: onSuccessID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(failsID, "fails", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
		remoteLifecycleLock(onFailureID, "on-failure", digest, nil),
		remoteLifecycleLock(onSuccessID, "on-success", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), `action "owner/repo/fails@v1" pre`) {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, readErr := os.ReadFile(lifecycle)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(events), "fails:pre\nafter:pre\non-failure:pre\non-failure:post\nafter:post\nfails:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionPreFailurePropagatesEnvironmentToLaterPre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: failed remote pre effects\n")
	remote := t.TempDir()
	for _, name := range []string{"fails", "after"} {
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
		writeFixtureFile(t, remote, name+"/pre.js", "")
		writeFixtureFile(t, remote, name+"/main.js", "")
	}
	marker := filepath.Join(workspace, "observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = fails:pre ]; then
  printf '%s\n' 'PRE_EFFECT=available' >> "$GITHUB_ENV"
  exit 7
fi
if [ "$action:$phase" = after:pre ]; then
  test "$PRE_EFFECT" = available
  touch "$MARKER"
fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	failsID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fails", Kind: "uses", Uses: remoteLifecycleUses("fails"), Action: &plan.ActionSelector{Lock: failsID}, ContinueOnError: true},
		{ID: "after", Kind: "uses", Uses: remoteLifecycleUses("after"), Action: &plan.ActionSelector{Lock: afterID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"MARKER": marker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(failsID, "fails", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later pre did not observe failed pre environment: %v", err)
	}
}

func TestRemoteCompositeSoftPreFailurePreservesSuccessForLaterPre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened composite pre failure\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\noutputs:\n  status:\n    value: ${{ steps.child.outcome }}-${{ steps.child.conclusion }}\nruns:\n  using: composite\n  steps:\n    - id: child\n      if: false\n      uses: owner/repo/child@v1\n      continue-on-error: true\n    - id: finalize\n      shell: sh\n      run: touch \"$COMPOSITE_MARKER\"\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n  post-if: steps.finalize.conclusion == 'success'\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	writeFixtureFile(t, remote, "child/post.js", "")
	writeFixtureFile(t, remote, "after/action.yml", "name: after\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: success()\n  main: main.js\n")
	writeFixtureFile(t, remote, "after/pre.js", "")
	writeFixtureFile(t, remote, "after/main.js", "")
	marker := filepath.Join(workspace, "observed")
	compositeMarker := filepath.Join(workspace, "composite-observed")
	childMainMarker := filepath.Join(workspace, "child-main-observed")
	childPostMarker := filepath.Join(workspace, "child-post-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:pre ]; then exit 7; fi
if [ "$action:$phase" = child:main ]; then touch "$CHILD_MAIN_MARKER"; fi
if [ "$action:$phase" = child:post ]; then touch "$CHILD_POST_MARKER"; fi
if [ "$action:$phase" = after:pre ]; then touch "$MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, childID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}, ContinueOnError: true},
		{ID: "after", Kind: "uses", Uses: remoteLifecycleUses("after"), Action: &plan.ActionSelector{Lock: afterID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"MARKER": marker, "COMPOSITE_MARKER": compositeMarker, "CHILD_MAIN_MARKER": childMainMarker, "CHILD_POST_MARKER": childPostMarker}
	job.Outputs = map[string]string{"status": "${{ steps.parent.outputs.status }}"}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later success pre did not run: %v", err)
	}
	if result.Outputs["status"] != "failure-success" {
		t.Fatalf("RunJob() outputs = %#v, want retained child pre-failure status", result.Outputs)
	}
	if _, err := os.Stat(compositeMarker); err != nil {
		t.Fatalf("later composite step did not run: %v", err)
	}
	if _, err := os.Stat(childMainMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child main ran after its pre failed: %v", err)
	}
	if _, err := os.Stat(childPostMarker); err != nil {
		t.Fatalf("child post did not see the final composite step status: %v", err)
	}
}

func TestRemoteCompositeSoftPreIfFailureSkipsMain(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened composite pre-if failure\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/child@v1\n      continue-on-error: true\n    - shell: sh\n      run: touch \"$LATER_STEP_MARKER\"\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: fromJSON('invalid')\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	laterStepMarker := filepath.Join(workspace, "later-step-observed")
	childMainMarker := filepath.Join(workspace, "child-main-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:main ]; then touch "$CHILD_MAIN_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LATER_STEP_MARKER": laterStepMarker, "CHILD_MAIN_MARKER": childMainMarker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(laterStepMarker); err != nil {
		t.Fatalf("later composite step did not run: %v", err)
	}
	if _, err := os.Stat(childMainMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child main ran after its pre-if failed: %v", err)
	}
}

func TestRemoteCompositeSoftConditionFailureBindsPostToFinalStepState(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened composite condition failure post scope\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\nruns:\n  using: composite\n  steps:\n    - id: child\n      if: fromJSON('invalid')\n      uses: owner/repo/child@v1\n      continue-on-error: true\n    - id: finalize\n      shell: sh\n      run: touch \"$FINALIZE_MARKER\"\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n  post-if: steps.finalize.conclusion == 'success'\n")
	for _, path := range []string{"child/pre.js", "child/main.js", "child/post.js"} {
		writeFixtureFile(t, remote, path, "")
	}
	finalizeMarker := filepath.Join(workspace, "finalize-observed")
	postMarker := filepath.Join(workspace, "post-observed")
	mainMarker := filepath.Join(workspace, "main-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:main ]; then touch "$MAIN_MARKER"; fi
if [ "$action:$phase" = child:post ]; then touch "$POST_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"FINALIZE_MARKER": finalizeMarker, "MAIN_MARKER": mainMarker, "POST_MARKER": postMarker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	for _, path := range []string{finalizeMarker, postMarker} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected marker %q: %v", path, err)
		}
	}
	if _, err := os.Stat(mainMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("condition-failed child main ran: %v", err)
	}
}

func TestRemoteCompositeSoftNestedPreparationFailurePreservesSuccessForLaterPre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened nested composite preparation failure\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/nested@v1\n      continue-on-error: true\n    - uses: owner/repo/after@v1\n")
	writeFixtureFile(t, remote, "nested/action.yml", "name: nested\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/child@v1\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "after/action.yml", "name: after\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: success()\n  main: main.js\n")
	for _, path := range []string{"child/pre.js", "child/main.js", "after/pre.js", "after/main.js"} {
		writeFixtureFile(t, remote, path, "")
	}
	marker := filepath.Join(workspace, "later-pre-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:pre ]; then exit 7; fi
if [ "$action:$phase" = after:pre ]; then touch "$MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, nestedID, childID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3), remoteLifecycleLockID(4)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"MARKER": marker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("nested"): {Lock: nestedID}, remoteLifecycleUses("after"): {Lock: afterID}}),
		remoteLifecycleLock(nestedID, "nested", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later pre-if: success() did not run: %v", err)
	}
}

func TestRemotePreparationErrorStillDrainsRegisteredPosts(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: remote preparation cleanup\n")
	remote := t.TempDir()
	for _, name := range []string{"first", "broken"} {
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, name+"/"+phase+".js", "")
		}
	}
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
printf '%s:%s\n' "$(basename "$(dirname "$1")")" "$(basename "$1" .js)" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	firstID, brokenID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "first", Kind: "uses", Uses: remoteLifecycleUses("first"), Action: &plan.ActionSelector{Lock: firstID}},
		{ID: "broken", Kind: "uses", Uses: remoteLifecycleUses("broken"), Action: &plan.ActionSelector{Lock: brokenID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(firstID, "first", digest, nil),
		remoteLifecycleLock(brokenID, "broken", "sha256:"+strings.Repeat("f", 64), nil),
	}
	job.ContinueOnError = true
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "materialized source digest mismatch") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, readErr := os.ReadFile(lifecycle)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(events), "first:pre\nfirst:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
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

func TestRemotePreparedInvocationIDsCannotAliasStepIDs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: remote invocation identity\n")
	remote := t.TempDir()
	for _, name := range []string{"child", "direct"} {
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, name+"/"+phase+".js", "")
		}
	}
	writeFixtureFile(t, remote, "root/action.yml", "name: root\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/child@v1\n")
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
printf '%s:%s\n' "$(basename "$(dirname "$1")")" "$(basename "$1" .js)" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID, directID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}},
		{ID: "root/0", Kind: "uses", Uses: remoteLifecycleUses("direct"), Action: &plan.ActionSelector{Lock: directID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
		remoteLifecycleLock(directID, "direct", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	want := "child:pre\ndirect:pre\nchild:main\ndirect:main\ndirect:post\nchild:post\n"
	if got := string(events); got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/conditions"}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/soft-failure"}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/soft-condition"}})
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
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/conditional", With: map[string]string{"enabled": enabled}}})
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
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
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "recursive", Kind: "uses", Uses: "./.github/actions/recursive"}})
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
	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "deep", Kind: "uses", Uses: "./.github/actions/depth-0"}})
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds maximum depth %d", metadata.MaxNestedActionDepth)) {
		t.Fatalf("RunJob() depth error = %v", err)
	}
}

func TestRuntimeMapDiagnosticsAreSorted(t *testing.T) {
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "shell", Kind: "run", Shell: "sh", Command: "true"}})
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
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "docker", Kind: "uses", Uses: "./actions/docker"}})
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

func remoteLifecycleLockID(index int) string {
	return fmt.Sprintf("a-%016x", index)
}

func remoteLifecycleUses(path string) string {
	return "owner/repo/" + path + "@v1"
}

func remoteLifecycleLock(id, path, digest string, children map[string]plan.ActionSelector) plan.ActionLock {
	return plan.ActionLock{
		ID:           id,
		Source:       "github",
		Repository:   "owner/repo",
		RequestedRef: "v1",
		Commit:       strings.Repeat("a", 40),
		Path:         path,
		SourceDigest: digest,
		Children:     children,
	}
}
