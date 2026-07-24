package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

const maxActiveBackgroundSteps = 10

var errExplicitBackgroundCancel = errors.New("background step explicitly cancelled")

type stepExecution struct {
	step       plan.Step
	result     Result
	err        error
	outcome    string
	conclusion string
}

type backgroundTask struct {
	id     string
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	run    func(context.Context) stepExecution
	// cancelled may run while the supervisor mutex is held and must not block.
	cancelled func(context.Context) stepExecution
	execution stepExecution
	started   bool
	finished  bool
	committed bool
}

// backgroundSupervisor owns private task admission and completion state. It
// never mutates workflow-visible job state; the RunJob goroutine does that only
// after a covering barrier returns completed executions.
type backgroundSupervisor struct {
	mu     sync.Mutex
	limit  int
	active int
	tasks  map[string]*backgroundTask
	order  []*backgroundTask
	queue  []*backgroundTask
}

func newBackgroundSupervisor(limit int) *backgroundSupervisor {
	return &backgroundSupervisor{limit: limit, tasks: make(map[string]*backgroundTask)}
}

func (s *backgroundSupervisor) start(parent context.Context, id string, run, cancelled func(context.Context) stepExecution) {
	ctx, cancel := context.WithCancelCause(parent)
	task := &backgroundTask{id: strings.ToLower(id), ctx: ctx, cancel: cancel, done: make(chan struct{}), run: run, cancelled: cancelled}
	s.mu.Lock()
	s.tasks[task.id] = task
	s.order = append(s.order, task)
	s.queue = append(s.queue, task)
	s.dispatchLocked()
	s.mu.Unlock()
}

func (s *backgroundSupervisor) dispatchLocked() {
	for s.active < s.limit && len(s.queue) != 0 {
		task := s.queue[0]
		s.queue = s.queue[1:]
		if task.ctx.Err() != nil {
			s.finishLocked(task, task.cancelled(task.ctx))
			continue
		}
		task.started = true
		s.active++
		go func() {
			execution := task.run(task.ctx)
			s.mu.Lock()
			s.active--
			s.finishLocked(task, execution)
			s.dispatchLocked()
			s.mu.Unlock()
		}()
	}
}

func (s *backgroundSupervisor) finishLocked(task *backgroundTask, execution stepExecution) {
	task.execution = execution
	task.finished = true
	task.cancel(nil)
	close(task.done)
}

func (s *backgroundSupervisor) cancel(target string) []stepExecution {
	s.mu.Lock()
	task := s.tasks[strings.ToLower(target)]
	if task == nil || task.committed {
		s.mu.Unlock()
		return nil
	}
	task.cancel(errExplicitBackgroundCancel)
	if !task.started && !task.finished {
		for i, queued := range s.queue {
			if queued == task {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				break
			}
		}
		s.finishLocked(task, task.cancelled(task.ctx))
	}
	s.mu.Unlock()
	return s.commitCompleted([]*backgroundTask{task})
}

func (s *backgroundSupervisor) wait(targets []string) []stepExecution {
	s.mu.Lock()
	tasks := make([]*backgroundTask, 0, len(targets))
	for _, target := range targets {
		task := s.tasks[strings.ToLower(target)]
		if task != nil && !task.committed {
			tasks = append(tasks, task)
		}
	}
	s.mu.Unlock()
	return s.commitCompleted(tasks)
}

func (s *backgroundSupervisor) waitAll() []stepExecution {
	s.mu.Lock()
	tasks := make([]*backgroundTask, 0, len(s.order))
	for _, task := range s.order {
		if !task.committed {
			tasks = append(tasks, task)
		}
	}
	s.mu.Unlock()
	return s.commitCompleted(tasks)
}

func (s *backgroundSupervisor) commitCompleted(tasks []*backgroundTask) []stepExecution {
	for _, task := range tasks {
		<-task.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	executions := make([]stepExecution, 0, len(tasks))
	for _, task := range tasks {
		if task.committed {
			continue
		}
		task.committed = true
		executions = append(executions, task.execution)
	}
	return executions
}

func (r Runner) executePlanStep(jobCtx, runCtx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations) stepExecution {
	stepCtx := runCtx
	cancelStep := func() {}
	if step.TimeoutMinutes > 0 {
		stepCtx, cancelStep = context.WithTimeout(runCtx, durationMinutes(step.TimeoutMinutes))
	}
	result, err := r.runJobStep(stepCtx, processor, workspace, job, step, invocationID, jobEnv, eval, posts, actions, prepared)
	cancelStep()
	return classifyStepExecution(jobCtx, runCtx, step, result, err)
}

func classifyStepExecution(jobCtx, runCtx context.Context, step plan.Step, result Result, err error) stepExecution {
	execution := stepExecution{step: step, result: result, err: err, outcome: "success", conclusion: "success"}
	if err == nil {
		return execution
	}
	execution.outcome = "failure"
	if jobCtx.Err() != nil || errors.Is(context.Cause(runCtx), errExplicitBackgroundCancel) {
		execution.outcome = "cancelled"
	}
	execution.conclusion = execution.outcome
	if step.ContinueOnError && execution.outcome == "failure" {
		execution.conclusion = "success"
	}
	return execution
}

func cancelledStepExecution(jobCtx, runCtx context.Context, step plan.Step) stepExecution {
	err := context.Cause(runCtx)
	if err == nil {
		err = context.Canceled
	}
	return classifyStepExecution(jobCtx, runCtx, step, newResult(), err)
}

func commitStepExecution(execution stepExecution, jobResult *JobResult, eval *expression.Context, statuses map[string]expression.StepStatus) error {
	id := strings.ToLower(execution.step.ID)
	eval.Steps[id] = execution.result.Outputs
	commitResultEnvironment(jobResult.Env, execution.result)
	eval.Env = jobResult.Env
	mergeInto(jobResult.State, execution.result.State)
	jobResult.Summary += execution.result.Summary
	statuses[id] = expression.StepStatus{Outcome: execution.outcome, Conclusion: execution.conclusion, Outputs: execution.result.Outputs}
	if execution.conclusion != "success" {
		return fmt.Errorf("step %q: %w", execution.step.ID, execution.err)
	}
	return nil
}

func commitResultEnvironment(env map[string]string, result Result) {
	effects := result.Env
	if len(result.Paths) > 0 {
		effects = cloneStrings(effects)
		if result.pathBaseSet {
			effects["PATH"] = result.pathBase
		} else {
			delete(effects, "PATH")
		}
	}
	mergeInto(env, effects)
	applyPaths(env, result.Paths)
}

func cloneExpressionContext(in expression.Context) expression.Context {
	return expression.Context{
		Inputs:      cloneStrings(in.Inputs),
		Matrix:      cloneAnyMap(in.Matrix),
		Steps:       cloneNestedStrings(in.Steps),
		Needs:       cloneNestedStrings(in.Needs),
		NeedResults: cloneStrings(in.NeedResults),
		Secrets:     cloneStrings(in.Secrets),
		Vars:        cloneStrings(in.Vars),
		Env:         cloneStrings(in.Env),
		GitHub:      cloneAnyMap(in.GitHub),
	}
}

func cloneNestedStrings(in map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(in))
	for name, values := range in {
		out[name] = cloneStrings(values)
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for name, value := range in {
		out[name] = cloneAny(value)
	}
	return out
}

func cloneAny(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneAnyMap(value)
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = cloneAny(value[i])
		}
		return out
	default:
		return value
	}
}
