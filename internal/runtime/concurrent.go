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

type backgroundTaskState uint8

const (
	backgroundTaskQueued backgroundTaskState = iota
	backgroundTaskRunning
	backgroundTaskFinished
	backgroundTaskCommitted
)

type backgroundTask struct {
	id     string
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	run    func(context.Context) stepExecution
	// cancelled may run while the supervisor mutex is held and must not block.
	cancelled func(context.Context) stepExecution
	execution stepExecution
	// state is protected by the supervisor mutex.
	state backgroundTaskState
}

func (task *backgroundTask) transitionLocked(next backgroundTaskState) {
	valid := task.state == backgroundTaskQueued && (next == backgroundTaskRunning || next == backgroundTaskFinished) ||
		task.state == backgroundTaskRunning && next == backgroundTaskFinished ||
		task.state == backgroundTaskFinished && next == backgroundTaskCommitted
	if !valid {
		panic(fmt.Sprintf("invalid background task transition %d -> %d", task.state, next))
	}
	task.state = next
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
		task.transitionLocked(backgroundTaskRunning)
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
	task.transitionLocked(backgroundTaskFinished)
	task.cancel(nil)
	close(task.done)
}

func (s *backgroundSupervisor) cancel(target string) []stepExecution {
	s.mu.Lock()
	task := s.tasks[strings.ToLower(target)]
	if task == nil || task.state == backgroundTaskCommitted {
		s.mu.Unlock()
		return nil
	}
	task.cancel(errExplicitBackgroundCancel)
	if task.state == backgroundTaskQueued {
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
		if task != nil && task.state != backgroundTaskCommitted {
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
		if task.state != backgroundTaskCommitted {
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
		if task.state == backgroundTaskCommitted {
			continue
		}
		task.transitionLocked(backgroundTaskCommitted)
		executions = append(executions, task.execution)
	}
	return executions
}

func stepContext(parent context.Context, timeoutMinutes float64) (context.Context, context.CancelFunc) {
	if timeoutMinutes > 0 {
		return context.WithTimeout(parent, durationMinutes(timeoutMinutes))
	}
	return context.WithCancel(parent)
}

func bindHashFilesContext(ctx context.Context, eval *expression.Context) {
	if eval.HashFilesContext != nil {
		eval.HashFiles = func(patterns []string) (string, error) {
			return eval.HashFilesContext(ctx, patterns)
		}
	}
}

func (r *jobRun) executePlanStep(jobCtx, runCtx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv, stepEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations) stepExecution {
	result, err := r.runJobStep(runCtx, processor, workspace, job, step, invocationID, jobEnv, stepEnv, eval, posts, actions, prepared)
	return classifyStepExecutionWithControls(jobCtx, runCtx, step, result, err, eval)
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
	if step.ContinueOnError && execution.outcome == "failure" && !isHardJobFailure(err) {
		execution.conclusion = "success"
	}
	return execution
}

func classifyStepExecutionWithControls(jobCtx, runCtx context.Context, step plan.Step, result Result, err error, eval expression.Context) stepExecution {
	execution := classifyStepExecution(jobCtx, runCtx, step, result, err)
	if execution.outcome == "failure" && !isHardJobFailure(err) && step.ContinueOnErrorExpression != "" {
		resolved, controlErr := evaluateStepContinueOnError(step, eval)
		if controlErr != nil {
			return classifyStepExecution(jobCtx, runCtx, step, result, errors.Join(err, fmt.Errorf("controls: %w", controlErr)))
		}
		return classifyStepExecution(jobCtx, runCtx, resolved, result, err)
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

func commitStepExecution(execution stepExecution, jobResult *JobResult, eval *expression.Context) error {
	id := strings.ToLower(execution.step.ID)
	eval.Steps[id] = expression.StepStatus{Outcome: execution.outcome, Conclusion: execution.conclusion, Outputs: execution.result.Outputs}
	commitResultEnvironment(jobResult.Env, execution.result)
	eval.Env = jobResult.Env
	mergeInto(jobResult.State, execution.result.State)
	appendJobSummary(&jobResult.Summary, &jobResult.summaryTruncated, execution.result.Summary, execution.result.summaryTruncated)
	jobResult.Artifacts = append(jobResult.Artifacts, execution.result.Artifacts...)
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
	var inputs map[string]string
	if in.Inputs != nil {
		inputs = cloneStrings(in.Inputs)
	}
	return expression.Context{
		Inputs:           inputs,
		WorkflowInputs:   cloneAnyMap(in.WorkflowInputs),
		Matrix:           cloneAnyMap(in.Matrix),
		Steps:            cloneStepStatuses(in.Steps),
		Needs:            cloneNeedStatuses(in.Needs),
		Secrets:          cloneStrings(in.Secrets),
		Vars:             cloneStrings(in.Vars),
		Env:              cloneStrings(in.Env),
		GitHub:           cloneAnyMap(in.GitHub),
		Runner:           cloneStrings(in.Runner),
		Services:         cloneServiceContexts(in.Services),
		JobStatus:        in.JobStatus,
		HashFiles:        in.HashFiles,
		HashFilesContext: in.HashFilesContext,
	}
}

func cloneServiceContexts(in map[string]expression.ServiceContext) map[string]expression.ServiceContext {
	if in == nil {
		return nil
	}
	out := make(map[string]expression.ServiceContext, len(in))
	for name, service := range in {
		service.Ports = cloneStrings(service.Ports)
		out[name] = service
	}
	return out
}

func cloneStepStatuses(in map[string]expression.StepStatus) map[string]expression.StepStatus {
	out := make(map[string]expression.StepStatus, len(in))
	for name, status := range in {
		status.Outputs = cloneStrings(status.Outputs)
		out[name] = status
	}
	return out
}

func cloneNeedStatuses(in map[string]expression.NeedStatus) map[string]expression.NeedStatus {
	out := make(map[string]expression.NeedStatus, len(in))
	for name, status := range in {
		status.Outputs = cloneStrings(status.Outputs)
		out[name] = status
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
