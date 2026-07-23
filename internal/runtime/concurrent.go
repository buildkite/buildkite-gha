package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

const maxActiveBackgroundSteps = 10

type stepExecution struct {
	step       plan.Step
	result     Result
	post       *registeredPost
	err        error
	outcome    string
	conclusion string
}

type backgroundTask struct {
	id        string
	done      chan struct{}
	run       func() stepExecution
	execution stepExecution
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

func (s *backgroundSupervisor) start(id string, run func() stepExecution) {
	task := &backgroundTask{id: strings.ToLower(id), done: make(chan struct{}), run: run}
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
		s.active++
		go func() {
			execution := task.run()
			s.mu.Lock()
			task.execution = execution
			s.active--
			close(task.done)
			s.dispatchLocked()
			s.mu.Unlock()
		}()
	}
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

func (r Runner) executePlanStep(jobCtx, runCtx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, jobEnv map[string]string, eval expression.Context) stepExecution {
	stepCtx := runCtx
	cancelStep := func() {}
	if step.TimeoutMinutes > 0 {
		stepCtx, cancelStep = context.WithTimeout(runCtx, durationMinutes(step.TimeoutMinutes))
	}
	result, post, err := r.runJobStep(stepCtx, processor, workspace, job, step, jobEnv, eval)
	cancelStep()

	execution := stepExecution{step: step, result: result, post: post, err: err, outcome: "success", conclusion: "success"}
	if err == nil {
		return execution
	}
	execution.outcome = "failure"
	if jobCtx.Err() != nil {
		execution.outcome = "cancelled"
	}
	execution.conclusion = execution.outcome
	if step.ContinueOnError && execution.outcome == "failure" {
		execution.conclusion = "success"
	}
	return execution
}

func commitStepExecution(execution stepExecution, jobResult *JobResult, eval *expression.Context, statuses map[string]expression.StepStatus, posts *[]registeredPost) error {
	id := strings.ToLower(execution.step.ID)
	eval.Steps[id] = execution.result.Outputs
	mergeInto(jobResult.Env, execution.result.Env)
	applyPaths(jobResult.Env, execution.result.Paths)
	eval.Env = jobResult.Env
	mergeInto(jobResult.State, execution.result.State)
	jobResult.Summary += execution.result.Summary
	if execution.post != nil {
		*posts = append(*posts, *execution.post)
	}
	statuses[id] = expression.StepStatus{Outcome: execution.outcome, Conclusion: execution.conclusion, Outputs: execution.result.Outputs}
	if execution.conclusion != "success" {
		return fmt.Errorf("step %q: %w", execution.step.ID, execution.err)
	}
	return nil
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
