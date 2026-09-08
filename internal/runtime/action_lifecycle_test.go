package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	executionprogram "github.com/buildkite/buildkite-gha/internal/program"
)

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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "background", Kind: "uses", Uses: "./.github/actions/background", Background: true}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "cwd", Kind: "uses", Uses: "./.github/actions/cwd", Env: map[string]string{"CWD_LOG": cwdLog}}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "cwd", Kind: "uses", Uses: "./.github/actions/cwd", Env: map[string]string{"CWD_LOG": cwdLog}}})
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

func TestJavaScriptPreMainPostFilesAndMasking(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []runtimeTestStep{{ID: "javascript", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "hello"}}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "summary", Kind: "uses", Uses: "./.github/actions/summary"}})
	job.Env = map[string]string{"MAIN_SUMMARY_BYTES": strconv.Itoa(maxJobSummaryBytes)}

	result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if len(result.Summary) > maxJobSummaryBytes || strings.Contains(result.Summary, "post-summary-must-be-truncated") || !strings.HasSuffix(result.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("RunJob() summary bytes = %d, suffix present = %v", len(result.Summary), strings.HasSuffix(result.Summary, jobSummaryTruncationNotice))
	}
}

func TestPostActionsRunLIFOAfterMainFailure(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []runtimeTestStep{
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
		Program: &executionprogram.Program{Version: executionprogram.Version, Job: executionprogram.Job{Steps: []executionprogram.Step{{ID: "inspect", Kind: "run", Condition: invalidStepCondition}}}},
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
	result, runErr = (Runner{Node24: fakeNode}).runTestJob(t.Context(), timedOut, workspace)
	if !errors.Is(runErr, context.DeadlineExceeded) || IsToleratedJobFailure(runErr) || result.Conclusion != "cancelled" {
		t.Fatalf("timed out RunJob() result = %#v, error = %v", result, runErr)
	}
	if _, err := os.Stat(post); !os.IsNotExist(err) {
		t.Fatalf("failure-only post ran after job cancellation: %v", err)
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
			job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "conditional", Kind: "uses", Uses: "./.github/actions/conditional"}})
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
			steps := []runtimeTestStep{{ID: "cache", Kind: "uses", Uses: "./.github/actions/rust-cache"}}
			if test.finalCacheValue != "" {
				steps = append(steps, runtimeTestStep{ID: "override", Kind: "run", Command: fmt.Sprintf("printf 'CACHE_ON_FAILURE=%s\\n' >> \"$GITHUB_ENV\"", test.finalCacheValue)})
			}
			if test.failJob {
				steps = append(steps, runtimeTestStep{ID: "fail", Kind: "run", Command: "exit 7"})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "cache", Kind: "uses", Uses: "./.github/actions/cache"}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "cancel", Kind: "uses", Uses: "./.github/actions/cancel"}})
	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	result, err := (Runner{Node24: fakeNode, Stdout: &logs, Stderr: &logs}).runTestJob(ctx, job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" || !strings.Contains(logs.String(), "post-after-cancel") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
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
	steps := []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job.ExecutionJob().Steps[0].Invocation.Lock = topID
	job.ExecutionJob().Steps[1].Invocation.Lock = compositeID
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
