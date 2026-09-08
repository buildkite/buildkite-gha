package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestBackgroundSummariesAreBoundedInCommitOrder(t *testing.T) {
	supervisor := newBackgroundSupervisor(2)
	releaseFirst := make(chan struct{})
	completionOrder := make(chan string, 2)
	first := normalizeRuntimeTestStep(runtimeTestStep{ID: "first"})
	second := normalizeRuntimeTestStep(runtimeTestStep{ID: "second"})
	summaryBytes := maxJobSummaryBytes * 3 / 4

	supervisor.start(t.Context(), first.ID,
		func(ctx context.Context) stepExecution {
			<-releaseFirst
			completionOrder <- first.ID
			result := newResult()
			result.Summary = strings.Repeat("a", summaryBytes)
			return classifyStepExecution(ctx, ctx, first.ID, false, result, nil)
		},
		func(ctx context.Context) stepExecution {
			return cancelledStepExecution(t.Context(), ctx, first)
		},
	)
	supervisor.start(t.Context(), second.ID,
		func(ctx context.Context) stepExecution {
			completionOrder <- second.ID
			close(releaseFirst)
			result := newResult()
			result.Summary = strings.Repeat("b", summaryBytes)
			return classifyStepExecution(ctx, ctx, second.ID, false, result, nil)
		},
		func(ctx context.Context) stepExecution {
			return cancelledStepExecution(t.Context(), ctx, second)
		},
	)

	jobResult := JobResult{Env: map[string]string{}, State: map[string]string{}}
	eval := expression.Context{Steps: map[string]expression.StepStatus{}}
	for _, execution := range supervisor.waitAll() {
		if err := commitStepExecution(execution, &jobResult, &eval); err != nil {
			t.Fatal(err)
		}
	}
	jobResult.Summary = finalizeJobSummary(jobResult.Summary, jobResult.summaryTruncated)

	if got := []string{<-completionOrder, <-completionOrder}; !slices.Equal(got, []string{second.ID, first.ID}) {
		t.Fatalf("completion order = %v, want second then first", got)
	}
	if len(jobResult.Summary) > maxJobSummaryBytes || !strings.HasSuffix(jobResult.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("summary bytes = %d, suffix present = %v", len(jobResult.Summary), strings.HasSuffix(jobResult.Summary, jobSummaryTruncationNotice))
	}
	prefix := strings.TrimSuffix(jobResult.Summary, jobSummaryTruncationNotice)
	wantPrefix := strings.Repeat("a", summaryBytes) + strings.Repeat("b", len(prefix)-summaryBytes)
	if prefix != wantPrefix {
		t.Fatalf("summary did not preserve commit order")
	}
}

func TestCloneExpressionContextDeepCopiesStepOutputs(t *testing.T) {
	source := expression.Context{Steps: map[string]expression.StepStatus{
		"build": {Outcome: "success", Conclusion: "success", Outputs: map[string]string{"result": "source"}},
	}}

	cloned := cloneExpressionContext(source)
	status := cloned.Steps["build"]
	status.Outcome = "failure"
	status.Outputs["result"] = "clone"
	cloned.Steps["build"] = status
	cloned.Steps["added"] = expression.StepStatus{}

	if got := source.Steps["build"]; got.Outcome != "success" || got.Outputs["result"] != "source" {
		t.Fatalf("source step changed through clone: %#v", got)
	}
	if _, ok := source.Steps["added"]; ok {
		t.Fatal("source steps map changed through clone")
	}
}

func TestCloneExpressionContextDeepCopiesNeedOutputs(t *testing.T) {
	source := expression.Context{Needs: map[string]expression.NeedStatus{
		"build": {Outputs: map[string]string{"result": "source"}, Result: "success"},
	}}

	cloned := cloneExpressionContext(source)
	status := cloned.Needs["build"]
	status.Result = "failure"
	status.Outputs["result"] = "clone"
	cloned.Needs["build"] = status
	cloned.Needs["added"] = expression.NeedStatus{}

	if got := source.Needs["build"]; got.Result != "success" || got.Outputs["result"] != "source" {
		t.Fatalf("source need changed through clone: %#v", got)
	}
	if _, ok := source.Needs["added"]; ok {
		t.Fatal("source needs map changed through clone")
	}
}

func TestBackgroundTaskLifecycleTransitions(t *testing.T) {
	states := []struct {
		name  string
		state backgroundTaskState
	}{
		{name: "queued", state: backgroundTaskQueued},
		{name: "running", state: backgroundTaskRunning},
		{name: "finished", state: backgroundTaskFinished},
		{name: "committed", state: backgroundTaskCommitted},
	}
	valid := map[[2]backgroundTaskState]bool{
		{backgroundTaskQueued, backgroundTaskRunning}:     true,
		{backgroundTaskQueued, backgroundTaskFinished}:    true,
		{backgroundTaskRunning, backgroundTaskFinished}:   true,
		{backgroundTaskFinished, backgroundTaskCommitted}: true,
	}

	for _, from := range states {
		for _, to := range states {
			t.Run(from.name+"-to-"+to.name, func(t *testing.T) {
				task := backgroundTask{state: from.state}
				var panicked bool
				func() {
					defer func() { panicked = recover() != nil }()
					task.transitionLocked(to.state)
				}()

				if valid[[2]backgroundTaskState{from.state, to.state}] {
					if panicked || task.state != to.state {
						t.Fatalf("transition %d -> %d: state = %d, panicked = %v", from.state, to.state, task.state, panicked)
					}
				} else if !panicked || task.state != from.state {
					t.Fatalf("invalid transition %d -> %d: state = %d, panicked = %v", from.state, to.state, task.state, panicked)
				}
			})
		}
	}
}

func TestBackgroundSupervisorCommitsCompletedTaskExactlyOnceUnderContention(t *testing.T) {
	supervisor := newBackgroundSupervisor(1)
	task := &backgroundTask{
		done:      make(chan struct{}),
		execution: stepExecution{id: "only"},
		state:     backgroundTaskFinished,
	}
	close(task.done)

	const callers = 32
	start := make(chan struct{})
	results := make(chan []stepExecution, callers)
	var group sync.WaitGroup
	for range callers {
		group.Go(func() {
			<-start
			results <- supervisor.commitCompleted([]*backgroundTask{task})
		})
	}
	close(start)
	group.Wait()
	close(results)

	commits := 0
	for executions := range results {
		commits += len(executions)
		if len(executions) == 1 && executions[0].id != "only" {
			t.Fatalf("committed execution ID = %q, want only", executions[0].id)
		}
	}
	if commits != 1 {
		t.Fatalf("commits = %d, want 1", commits)
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if task.state != backgroundTaskCommitted {
		t.Fatalf("state = %d, want committed", task.state)
	}
}

func TestBackgroundSupervisorBoundsActiveWorkAndQueuesFIFO(t *testing.T) {
	supervisor := newBackgroundSupervisor(maxActiveBackgroundSteps)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	var started atomic.Int32
	for i := range maxActiveBackgroundSteps + 2 {
		supervisor.start(t.Context(), strconv.Itoa(i), func(context.Context) stepExecution {
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
		fifo.start(t.Context(), id, func(context.Context) stepExecution {
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

func TestExplicitCancelCommitsEffectsWithoutFailingJob(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	ready := filepath.Join(workspace, "background.ready")
	terminated := filepath.Join(workspace, "background.terminated")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	steps := make([]runtimeTestStep, 0, maxActiveBackgroundSteps+4)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, runtimeTestStep{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		runtimeTestStep{ID: "queued", Kind: "run", Background: true, Command: `touch "$QUEUED_MARKER"`},
		runtimeTestStep{ID: "cancel-queued", Kind: "cancel", Targets: []string{"queued"}},
		runtimeTestStep{ID: "release", Kind: "run", Command: `test ! -e "$QUEUED_MARKER"; touch "$RELEASE"`},
		runtimeTestStep{ID: "wait", Kind: "wait-all"},
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
	steps := make([]runtimeTestStep, 0, maxActiveBackgroundSteps+3)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, runtimeTestStep{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		runtimeTestStep{ID: "queued", Kind: "run", Background: true, TimeoutMinutes: 0.001, Command: `touch "$QUEUED_MARKER"`},
		runtimeTestStep{ID: "release", Kind: "run", Command: `sleep 0.2; touch "$RELEASE"`},
		runtimeTestStep{ID: "wait", Kind: "wait-all"},
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
	steps := make([]runtimeTestStep, 0, maxActiveBackgroundSteps+4)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, runtimeTestStep{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		runtimeTestStep{ID: "queued", Kind: "uses", Uses: "./.github/actions/queued", Background: true},
		runtimeTestStep{ID: "cancel-queued", Kind: "cancel", Targets: []string{"queued"}},
		runtimeTestStep{ID: "release", Kind: "run", Command: `touch "$RELEASE"`},
		runtimeTestStep{ID: "wait", Kind: "wait-all"},
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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

func TestBackgroundFailureSurfacesAtWait(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "failed.done")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "background", Kind: "run", Background: true, Command: `echo "value=private" >> "$GITHUB_OUTPUT"`},
		{ID: "premature-reader", Kind: "run", Command: `echo "${{ steps.background.outputs.value }}"`},
	})

	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "unavailable step") {
		t.Fatalf("RunJob() result = %#v, error = %v, want unavailable background output", result, err)
	}
}

func TestConcurrentStreamsShareMaskRegistration(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "mask.ready")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
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
