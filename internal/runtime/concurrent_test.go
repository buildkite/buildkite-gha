package runtime

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/expression"
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
