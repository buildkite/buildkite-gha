package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
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
		{ID: "after-soft", Kind: "run", Condition: "steps.soft.outcome == 'failure' && steps.soft.conclusion == 'success'", Command: `test "$LEVEL" = file
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
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: testSecretResolver{"CANARY": "mask-me"}, Redactor: redactor}).RunJob(context.Background(), job, workspace)
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
    defaults:
      run:
        shell: unsupported-default
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
	plans, err := compiler.CompilePlansWithOptions(
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
		result, err := (Runner{}).RunJob(context.Background(), job, workspace)
		if err != nil || result.Conclusion != "success" {
			t.Fatalf("RunJob(%s) result = %#v, error = %v", job.Workflow.LogicalJobID, result, err)
		}
	}
}

func TestCompiledBracketSecretResolvesAndMasks(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/secrets.yml"
	source := "on: push\njobs:\n  secrets:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo \"secret=${{ secrets['TOKEN'] }}\"\n"
	writeFixtureFile(t, workspace, workflowPath, source)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compiler.CompilePlans(filepath.Join(workspace, workflowPath), []byte(source), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !slices.Equal(plans[0].RequiredSecrets, []string{"TOKEN"}) || !plans[0].HasCapability("secrets") {
		t.Fatalf("compiled secret boundary = %#v", plans)
	}
	var logs bytes.Buffer
	redactor := &testRedactor{}
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: testSecretResolver{"TOKEN": "secret-value"}, Redactor: redactor}).RunJob(context.Background(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(logs.String(), "secret-value") || !strings.Contains(logs.String(), "secret=***") || !slices.Equal(redactor.values, []string{"secret-value"}) {
		t.Fatalf("logs = %q, redactions = %#v", logs.String(), redactor.values)
	}
}

func TestEnvironmentSecretsCannotReadAmbientAgentVariables(t *testing.T) {
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "ambient-token")
	t.Setenv("BUILDKITE_GHA_SECRET_BUILDKITE_AGENT_ACCESS_TOKEN", "")
	if _, err := (EnvironmentSecrets{}).ResolveSecret(context.Background(), "BUILDKITE_AGENT_ACCESS_TOKEN"); err != nil {
		// The explicit namespace exists but is empty; this still proves the ambient
		// variable was not selected.
		return
	}
	value, _ := (EnvironmentSecrets{}).ResolveSecret(context.Background(), "BUILDKITE_AGENT_ACCESS_TOKEN")
	if value == "ambient-token" {
		t.Fatal("EnvironmentSecrets exposed the ambient Buildkite Agent token")
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
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
	if err == nil || result.Conclusion != "failure" || strings.Contains(logs.String(), "must-not-run") || !strings.Contains(logs.String(), "recovered") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}

	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "timeout", Kind: "run", Command: "sleep 30", TimeoutMinutes: 0.0005}})
	started := time.Now()
	result, err = (Runner{}).RunJob(context.Background(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "failure" || time.Since(started) > 3*time.Second {
		t.Fatalf("timed RunJob() result = %#v, error = %v, elapsed = %s", result, err, time.Since(started))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cancel", Kind: "run", Condition: "always()", Command: "sleep 30"}})
	result, err = (Runner{}).RunJob(ctx, job, workspace)
	if !errors.Is(err, context.Canceled) || result.Conclusion != "cancelled" {
		t.Fatalf("cancelled RunJob() result = %#v, error = %v", result, err)
	}
}

func TestExplicitCancelCommitsEffectsWithoutFailingJob(t *testing.T) {
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
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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
	for i := 0; i < maxActiveBackgroundSteps; i++ {
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

	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, statErr := os.Stat(queuedMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queued canceled step ran: %v", statErr)
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
	for i := 0; i < maxActiveBackgroundSteps; i++ {
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

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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

	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
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

	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
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

	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
}

func TestJavaScriptMainSeesPrePathEffects(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/path/action.yml", `name: JavaScript path writer
runs:
  using: node24
  pre: pre.js
  main: main.js
`)
	writeFixtureFile(t, workspace, ".github/actions/path/pre.js", "")
	writeFixtureFile(t, workspace, ".github/actions/path/main.js", "")
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
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	pathEntry := filepath.Join(workspace, "from-pre")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./.github/actions/path"}})
	job.Env = map[string]string{"PATH_ENTRY": pathEntry}

	result, err := (Runner{Node24: fakeNode}).RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
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

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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
		{ID: "verify", Kind: "run", Condition: "steps.skipped.conclusion == 'skipped' && steps.cancel-skipped.conclusion == 'success' && steps.cancel-completed.conclusion == 'success'", Command: "true"},
	})

	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Summary != "once\n" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestBackgroundOutputsFailClosedBeforeBarrier(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `echo "value=private" >> "$GITHUB_OUTPUT"`},
		{ID: "premature-reader", Kind: "run", Command: `echo "${{ steps.background.outputs.value }}"`},
	})

	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "unavailable step") {
		t.Fatalf("RunJob() result = %#v, error = %v, want unavailable background output", result, err)
	}
}

func TestBackgroundSupervisorBoundsActiveWorkAndQueuesFIFO(t *testing.T) {
	supervisor := newBackgroundSupervisor(maxActiveBackgroundSteps)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	var started atomic.Int32
	for i := 0; i < maxActiveBackgroundSteps+2; i++ {
		supervisor.start(context.Background(), strconv.Itoa(i), func(context.Context) stepExecution {
			current := active.Add(1)
			for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
			}
			started.Add(1)
			<-release
			active.Add(-1)
			return stepExecution{}
		}, func(context.Context) stepExecution { return stepExecution{} })
	}
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() != maxActiveBackgroundSteps && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != maxActiveBackgroundSteps {
		t.Fatalf("started = %d, want %d before release", got, maxActiveBackgroundSteps)
	}
	close(release)
	if got := len(supervisor.waitAll()); got != maxActiveBackgroundSteps+2 {
		t.Fatalf("completed = %d, want %d", got, maxActiveBackgroundSteps+2)
	}
	if got := maximum.Load(); got != maxActiveBackgroundSteps {
		t.Fatalf("maximum active = %d, want %d", got, maxActiveBackgroundSteps)
	}

	fifo := newBackgroundSupervisor(1)
	firstRelease := make(chan struct{})
	var mu sync.Mutex
	var order []string
	start := func(id string, wait <-chan struct{}) {
		fifo.start(context.Background(), id, func(context.Context) stepExecution {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			if wait != nil {
				<-wait
			}
			return stepExecution{}
		}, func(context.Context) stepExecution { return stepExecution{} })
	}
	start("first", firstRelease)
	start("second", nil)
	start("third", nil)
	close(firstRelease)
	fifo.waitAll()
	mu.Lock()
	gotOrder := strings.Join(order, ",")
	mu.Unlock()
	if gotOrder != "first,second,third" {
		t.Fatalf("start order = %q, want FIFO", gotOrder)
	}
}

func TestImplicitWaitAllPrecedesPostCleanup(t *testing.T) {
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

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["value"] != "implicit" || result.Env["BACKGROUND_READY"] != "true" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	if !strings.Contains(logs.String(), "post-after-implicit-wait") {
		t.Fatalf("RunJob() logs = %q", logs.String())
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
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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
	plans, err := compiler.CompilePlans(workflowPath, source, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %#v, want concurrent and observer", plans)
	}
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs}
	concurrent, err := runner.RunJob(context.Background(), plans[0], workspace)
	if err != nil || concurrent.Conclusion != "success" {
		t.Fatalf("concurrent result = %#v, error = %v, logs = %q", concurrent, err, logs.String())
	}
	plans[1].Needs = map[string]plan.Need{"concurrent": {Result: concurrent.Conclusion, Outputs: concurrent.Outputs}}
	observer, err := runner.RunJob(context.Background(), plans[1], workspace)
	if err != nil || observer.Conclusion != "success" {
		t.Fatalf("observer result = %#v, error = %v, logs = %q", observer, err, logs.String())
	}
	if strings.Contains(logs.String(), "phase3-cross-stream-secret") || !strings.Contains(logs.String(), "PHASE3_MASK_PROBE=***") {
		t.Fatalf("concurrent masking logs = %q", logs.String())
	}
	want := `PHASE3_OBSERVATION={"cancel":"graceful","failure":"failure-at-wait","implicit":"implicit-wait-all","parallel":"parallel","queue_max":10,"targeted":"targeted-and-full"}`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("concurrent observation missing from logs = %q", logs.String())
	}
}

func TestCancellationTerminatesChildProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := (Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}).runStreaming(ctx, newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"PID_FILE": pidFile}, "sh", "-c", `(trap '' INT TERM; sleep 30) & echo $! > "$PID_FILE"; wait`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStreaming() error = %v, want deadline", err)
	}
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived cancellation", pid)
}

func TestCancellationEscalatesFromInterruptToTermination(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	signals := filepath.Join(dir, "signals")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	go func() {
		defer close(cancelled)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runner := Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 500 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"READY": ready, "SIGNALS": signals}, "bash", "-c", `
trap 'printf "INT\n" >> "$SIGNALS"' INT
trap 'printf "TERM\n" >> "$SIGNALS"; exit 0' TERM
touch "$READY"
while :; do sleep 1; done`)
	<-cancelled
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	contents, readErr := os.ReadFile(signals)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := string(contents); got != "INT\nTERM\n" {
		t.Fatalf("signal order = %q, want SIGINT then SIGTERM", got)
	}
}

func TestCancellationWaitsForProcessGroupCleanupAfterOutputCloses(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	started := time.Now()
	runner := Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"PID_FILE": pidFile, "READY": ready}, "bash", "-c", `
(trap '' INT TERM; exec >/dev/null 2>&1; while :; do sleep 1; done) &
echo $! > "$PID_FILE"
trap 'exit 0' INT
touch "$READY"
while :; do sleep 1; done`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed < 90*time.Millisecond {
		t.Fatalf("runStreaming() returned before process-group escalation completed: %s", elapsed)
	}
	contents, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived completed cancellation", pid)
}

func TestExplicitCancelTerminatesBackgroundProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	pidFile := filepath.Join(workspace, "child.pid")
	ready := filepath.Join(workspace, "background.ready")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `(trap '' INT TERM; sleep 30) & echo $! > "$PID_FILE"; touch "$READY"; wait`},
		{ID: "await-start", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	})
	job.Env = map[string]string{"PID_FILE": pidFile, "READY": ready}

	result, err := (Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("explicitly canceled child process %d survived", pid)
}

func TestJobConditionConsumesNeedResultAndOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.Needs = map[string]plan.Need{"producer": {Result: "failure", Outputs: map[string]string{"gate": "yes"}}}
	job.Condition = "always() && needs.producer.result == 'failure' && needs.producer.outputs.gate"
	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	job.Condition = ""
	job.Needs["producer"] = plan.Need{Result: "skipped"}
	result, err = (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "skipped" {
		t.Fatalf("RunJob() skipped prerequisite result = %#v, error = %v", result, err)
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
	_, err := (Runner{Secrets: testSecretResolver{"CANARY": "do-not-publish"}, Redactor: &testRedactor{}}).RunJob(context.Background(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "contains a registered secret") || strings.Contains(err.Error(), "do-not-publish") {
		t.Fatalf("RunJob() error = %v, want non-disclosing secret-output rejection", err)
	}
}

func TestRunJobRequiresHydratedStaticDependencyResults(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: dependency boundary\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.Dependencies = []string{"gha-producer"}
	job.NeedSources = map[string][]plan.NeedSource{"producer": {{StepKey: "gha-producer", PlanDigest: "sha256:" + strings.Repeat("1", 64)}}}
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "no hydrated prerequisite results") {
		t.Fatalf("RunJob() error = %v, want fail-closed hydration boundary", err)
	}
	job.Needs = map[string]plan.Need{"producer": {Result: "success"}}
	if result, err := (Runner{}).RunJob(context.Background(), job, workspace); err != nil || result.Conclusion != "success" {
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
	result, err := runner.RunJob(context.Background(), job, workspace)
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

func TestPostActionsRunLIFOAfterMainFailure(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{
		{ID: "one", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "one", "order": "one"}},
		{ID: "two", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "two", "order": "two", "fail": "true"}},
	})
	_, err := runner.RunJob(context.Background(), job, workspace)
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

func TestConcurrentPostActionsRunLIFOByRegistration(t *testing.T) {
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

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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
	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/action.yml", "name: Slow post\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/main.js", "console.log('main completed')\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/post.js", "setTimeout(() => console.log('slow post completed'), 30000)\n")
	runner := Runner{Node24: node, CleanupTimeout: 200 * time.Millisecond}
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
	started := time.Now()
	_, err := runner.RunJob(context.Background(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("bounded cleanup took %s, want under 3s", elapsed)
	}
}

func TestCancellationStillRunsRegisteredPostAction(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/action.yml", "name: Cancellation cleanup\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/main.js", "setTimeout(() => {}, 30000)\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/post.js", "console.log('post-after-cancel')\n")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "cancel", Kind: "uses", Uses: "./.github/actions/cancel"}})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(ctx, job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" || !strings.Contains(logs.String(), "post-after-cancel") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestExplicitBackgroundCancelStillRunsRegisteredPostAction(t *testing.T) {
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

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || !strings.Contains(logs.String(), "post-after-explicit-cancel") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestFileCommandParsing(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     map[string]string
		wantErr  string
	}{
		{name: "LF", contents: "single=value\nmulti<<END\nfirst\nsecond\nEND\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "CRLF", contents: "single=value\r\nmulti<<END\r\nfirst\r\nsecond\r\nEND\r\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "equals before heredoc", contents: "single=value<<literal\n", want: map[string]string{"single": "value<<literal"}},
		{name: "heredoc before equals", contents: "multi<<END=value\npayload\nEND=value\n", want: map[string]string{"multi": "payload"}},
		{name: "missing name", contents: "=value\n", wantErr: "invalid file command"},
		{name: "missing delimiter", contents: "multi<<\n", wantErr: "invalid multiline file command"},
		{name: "unterminated LF", contents: "multi<<END\nunterminated\n", wantErr: `missing delimiter "END"`},
		{name: "unterminated CRLF", contents: "multi<<END\r\nunterminated\r\n", wantErr: `missing delimiter "END"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "commands")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := parseCommandFile(path)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseCommandFile() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCommandFile() error = %v", err)
			}
			if !maps.Equal(got, test.want) {
				t.Fatalf("parseCommandFile() = %#v, want %#v", got, test.want)
			}
		})
	}

	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(files.dir) }()
	if err := os.WriteFile(files.env, []byte("NODE_OPTIONS=--require bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "NODE_OPTIONS") {
		t.Fatalf("commandFiles.apply() error = %v, want NODE_OPTIONS rejection", err)
	}
}

func TestFileCommandLineLimitIsExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(path, []byte("value="+strings.Repeat("x", 70*1024)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if values, err := parseCommandFile(path); err != nil || len(values["value"]) != 70*1024 {
		t.Fatalf("parseCommandFile() value length = %d, error = %v", len(values["value"]), err)
	}
	if err := os.WriteFile(path, []byte("value="+strings.Repeat("x", maxStreamLineBytes)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCommandFile(path); err == nil || !strings.Contains(err.Error(), "parse file command output") {
		t.Fatalf("parseCommandFile() error = %v, want attributed size failure", err)
	}
}

func TestFileCommandAggregateLimits(t *testing.T) {
	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(files.dir) }()

	many := strings.Repeat("value=x\n", maxCommandEntries+1)
	if err := os.WriteFile(files.output, []byte(many), 0o600); err != nil {
		t.Fatal(err)
	}
	result := newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("apply() error = %v, want entry limit", err)
	}

	if err := os.WriteFile(files.output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.summary, bytes.Repeat([]byte("x"), maxCommandFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "summary exceeds") {
		t.Fatalf("apply() error = %v, want summary size limit", err)
	}

	for _, path := range []string{files.output, files.env, files.state} {
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 700*1024), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(files.summary, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("apply() error = %v, want aggregate size limit", err)
	}
}

func TestExpressionEvaluationIsSinglePass(t *testing.T) {
	literal := "literal ${{ matrix.secret }} and ${{"
	values, err := evaluateMap(map[string]string{
		"value": "before ${{ matrix.value }} after",
	}, expression.Context{Matrix: map[string]any{
		"value":  literal,
		"secret": "reevaluated",
	}})
	if err != nil || values["value"] != "before "+literal+" after" {
		t.Fatalf("evaluateMap() = %#v, %v, want single-pass substitution", values, err)
	}
}

func TestRunStreamingDrainsOversizedLineAndPreservesMasking(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = (Runner{}).runStreaming(ctx, processor, "", map[string]string{"GO_WANT_RUNTIME_LONG_LINE": "1"}, executable, "-test.run=^TestLongLineChildProcess$")
	if err == nil || !strings.Contains(err.Error(), "stdout stream: line exceeds 1048576-byte limit and was discarded") {
		t.Fatalf("runStreaming() error = %v, want oversized-line diagnostic", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStreaming() deadlocked: %v", err)
	}
	if strings.Contains(logs.String(), "runtime-stream-secret") {
		t.Fatalf("runStreaming() leaked masked content: %q", logs.String())
	}
	if strings.Contains(logs.String(), "after long line") {
		t.Fatalf("runStreaming() forwarded output after masking became uncertain: %q", logs.String())
	}
}

func TestStreamLineLimitIncludesContentNotNewline(t *testing.T) {
	want := strings.Repeat("x", maxStreamLineBytes)
	var lines []string
	suppressed := false
	err := streamLines(strings.NewReader(want+"\nnext\n"), func(line string) {
		lines = append(lines, line)
	}, func() {
		suppressed = true
	})
	if err != nil || suppressed {
		t.Fatalf("streamLines() = %v, suppressed = %v", err, suppressed)
	}
	if len(lines) != 2 || lines[0] != want || lines[1] != "next" {
		t.Fatalf("streamLines() returned %d lines with unexpected content", len(lines))
	}
}

func TestLongLineChildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIME_LONG_LINE") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "::add-mask::runtime-stream-secret")
	_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", maxStreamLineBytes+1)+"runtime-stream-secret")
	_, _ = fmt.Fprintln(os.Stdout, "after long line: runtime-stream-secret")
}

func TestProcessEnvironmentIsExplicitAndUsable(t *testing.T) {
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "must-not-leak")
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	command := `test -n "$PATH" && test -n "$HOME" && test -n "$TMPDIR" && test "$DECLARED" = visible && test -z "${BUILDKITE_AGENT_ACCESS_TOKEN:-}" && printf '%s\n' environment-ok`
	if err := (Runner{}).runStreaming(context.Background(), processor, "", map[string]string{"DECLARED": "visible"}, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}
	if logs.String() != "environment-ok\n" {
		t.Fatalf("runStreaming() logs = %q", logs.String())
	}
	for _, entry := range processEnv(nil) {
		if strings.HasPrefix(entry, "BUILDKITE_") {
			t.Fatalf("processEnv() inherited agent variable %q", entry)
		}
	}
}

func TestWorkflowCommandParsingIsCaseInsensitiveAndExact(t *testing.T) {
	if got, ok := workflowCommand("::ADD-MASK::secret%250Avalue", "add-mask"); !ok || got != "secret%0Avalue" {
		t.Fatalf("workflowCommand() = %q, %v", got, ok)
	}
	if _, ok := workflowCommand("::add-mask-extra::secret", "add-mask"); ok {
		t.Fatal("workflowCommand() accepted a different command name")
	}
}

func TestActionMetadataRejectsCaseInsensitiveOutputCollisions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/conflict/action.yml", `name: Conflicting outputs
outputs:
  Result:
    value: first
  result:
    value: second
runs:
  using: composite
  steps: []
`)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "conflict", Kind: "uses", Uses: "./.github/actions/conflict"}})
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), `duplicate case-insensitive name "result"`) {
		t.Fatalf("RunJob() error = %v, want duplicate output rejection", err)
	}
}

func TestDiscoverNode24ManagedAndWrongExplicitVersion(t *testing.T) {
	managed := t.TempDir()
	node := filepath.Join(managed, "node24", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverNode24("", managed)
	if err != nil || got != node {
		t.Fatalf("DiscoverNode24() = %q, %v, want %q, nil", got, err, node)
	}

	wrong := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(wrong, []byte("#!/bin/sh\necho v23.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverNode24(wrong, ""); err == nil || !strings.Contains(err.Error(), `reported "v23.1.0"`) {
		t.Fatalf("DiscoverNode24() error = %v, want wrong-version detail", err)
	}
}

func TestDockerAction(t *testing.T) {
	docker := requireDocker(t)
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs, Docker: docker}
	result, err := runner.RunDocker(context.Background(), DockerAction{
		Name: "local Docker", Path: fixturePath(t, "actions", "docker"), Workspace: fixturePath(t),
	})
	if err != nil {
		t.Fatalf("RunDocker() error = %v", err)
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
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Node24: node}).RunJob(context.Background(), job, workspace)
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
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
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
	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
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

func TestRuntimeMapDiagnosticsAreSorted(t *testing.T) {
	_, err := evaluateMap(map[string]string{
		"z-last":  "${{ unsupported.z }}",
		"a-first": "${{ unsupported.a }}",
	}, expression.Context{})
	if err == nil || !strings.Contains(err.Error(), `evaluate "a-first"`) {
		t.Fatalf("evaluateMap() error = %v, want alphabetically first key", err)
	}

	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "shell", Kind: "run", Shell: "sh", Command: "true"}})
	job.Needs = map[string]plan.Need{"z-last": {}, "a-first": {}}
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), `prerequisite result "a-first"`) {
		t.Fatalf("RunJob() prerequisite error = %v, want alphabetically first key", err)
	}

	job.Needs = nil
	job.Outputs = map[string]string{"z-valid": "partial", "a-invalid": "${{ unsupported.a }}"}
	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `job output "a-invalid"`) {
		t.Fatalf("RunJob() output error = %v, want alphabetically first key", err)
	}
	if len(result.Outputs) != 0 {
		t.Fatalf("RunJob() partial outputs = %#v, want none before first sorted error", result.Outputs)
	}
}

func TestRunJobDockerUsesSharedMasking(t *testing.T) {
	docker := requireDocker(t)
	workspace := fixturePath(t)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "docker", Kind: "uses", Uses: "./actions/docker"}})
	job.RequiredCapabilities = []string{"docker"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Docker: docker}).RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Env["DOCKER_RUNTIME_SEEN"] != "true" || strings.Contains(logs.String(), "docker-secret-value") || !strings.Contains(logs.String(), "masked docker probe: ***") {
		t.Fatalf("RunJob() result = %#v, logs = %q", result, logs.String())
	}
}

func TestRunJobRejectsWorkflowMismatchAndUnsupportedAction(t *testing.T) {
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "local", Kind: "uses", Uses: "./actions/javascript"}})
	job.Workflow.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "workflow digest mismatch") {
		t.Fatalf("RunJob() error = %v, want workflow digest mismatch", err)
	}
	job = runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "remote", Kind: "uses", Uses: "actions/checkout@v4"}})
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "remote action") {
		t.Fatalf("RunJob() error = %v, want explicit remote action error", err)
	}

	for _, using := range []string{"node20", "future"} {
		t.Run(using, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			writeFixtureFile(t, workspace, ".github/actions/unsupported/action.yml", "runs:\n  using: "+using+"\n")
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "unsupported", Kind: "uses", Uses: "./.github/actions/unsupported"}})
			if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), `unsupported runtime "`+using+`"`) {
				t.Fatalf("RunJob() error = %v, want %s fail-closed boundary", err, using)
			}
		})
	}
}

func runtimePlan(t *testing.T, workspace, workflowPath string, steps []plan.Step) plan.Job {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(workspace, workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(source)
	return plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow: plan.Workflow{Path: workflowPath, Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "fixture"},
		Event:    plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:   plan.Target{StepKey: "gha-fixture", Queue: "ubuntu-latest"}, Steps: steps,
	}
}

func writeFixtureFile(t *testing.T, root, path, contents string) {
	t.Helper()
	path = filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixturePath(t *testing.T, parts ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	pathParts := append([]string{filepath.Dir(file), "..", "..", "testdata"}, parts...)
	path, err := filepath.Abs(filepath.Join(pathParts...))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func requireNode24(t *testing.T) string {
	t.Helper()
	if node := os.Getenv("BUILDKITE_GHA_TEST_NODE24"); node != "" {
		if _, err := DiscoverNode24(node, ""); err != nil {
			t.Fatalf("BUILDKITE_GHA_TEST_NODE24 is not Node 24: %v", err)
		}
		return node
	}
	if mise, err := exec.LookPath("mise"); err == nil {
		output, err := exec.Command(mise, "where", "node@24").CombinedOutput()
		if err == nil {
			node := filepath.Join(strings.TrimSpace(string(output)), "bin", "node")
			if _, err := DiscoverNode24(node, ""); err == nil {
				return node
			}
		}
	}
	t.Skip("Node 24 unavailable: set BUILDKITE_GHA_TEST_NODE24 or install managed Node 24 with `mise install node@24`")
	return ""
}

func requireDocker(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker unavailable: docker executable not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, docker, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("Docker unavailable: daemon probe failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return docker
}
