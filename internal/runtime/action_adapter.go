package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
)

// actionAdapterState journals exact leaf results that remain invisible to the
// action machine and must be committed once by the job scheduler.
type actionAdapterState struct {
	Results []Result
}

// actionAdapter executes normalized leaves against one prepared job runtime.
type actionAdapter struct {
	run         *jobRun
	eval        expression.Context
	preparation *remotePreparationTimeout
}

type actionSession struct {
	machine *program.ActionMachine[actionAdapterState]
	adapter *actionAdapter
	cursor  int
}

type actionPostRegistration struct {
	session   *actionSession
	stepIndex int
}

type actionPostSequence struct {
	mu            sync.Mutex
	registrations []actionPostRegistration
}

func (s *actionPostSequence) register(registration actionPostRegistration) {
	s.mu.Lock()
	s.registrations = append(s.registrations, registration)
	s.mu.Unlock()
}

func (s *actionPostSequence) snapshot() []actionPostRegistration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]actionPostRegistration(nil), s.registrations...)
}

func newActionSession(run *jobRun, stepIndex int) *actionSession {
	adapter, state := newActionAdapter(run)
	session := &actionSession{machine: program.NewActionMachine(run.job.ActionPrograms, adapter, state), adapter: adapter}
	session.machine.SetPostRegistrationCallback(func() {
		run.actionPosts.register(actionPostRegistration{session: session, stepIndex: stepIndex})
	})
	return session
}

func (s *actionSession) drain() Result {
	results := s.machine.State().Results
	if s.cursor >= len(results) {
		return newResult()
	}
	result := foldActionResults(results[s.cursor:])
	s.cursor = len(results)
	return result
}

func foldActionResults(results []Result) Result {
	total := newResult()
	for _, result := range results {
		for name, value := range result.Outputs {
			total.Outputs[name] = value
		}
		for name, value := range result.Env {
			total.Env[name] = value
		}
		for name, value := range result.State {
			total.State[name] = value
		}
		if result.pathBaseSet {
			total.pathBase = result.pathBase
			total.pathBaseSet = true
			total.Paths = total.Paths[:0]
		}
		total.Paths = append(total.Paths, result.Paths...)
		total.Artifacts = append(total.Artifacts, result.Artifacts...)
		appendJobSummary(&total.Summary, &total.summaryTruncated, result.Summary, result.summaryTruncated)
	}
	return total
}

func actionFrame(eval expression.Context, environment, stableEnvironment map[string]string, explicitPATH bool) program.Frame {
	frame := program.Frame{
		WorkflowInputs:    anyValueObject(eval.WorkflowInputs),
		ActionInputs:      program.ValueObject{Fields: map[string]expression.AbstractValue{}},
		Environment:       stringValueObject(environment),
		StableEnvironment: stringValueObject(stableEnvironment),
		GitHub:            anyValueObject(eval.GitHub),
		Matrix:            anyValueObject(eval.Matrix),
		Steps:             make(map[string]program.StepFrame, len(eval.Steps)),
		ExplicitPATH:      explicitPATH,
	}
	for name, step := range eval.Steps {
		frame.Steps[name] = program.StepFrame{
			Outcome: outcomeSet(step.Outcome), Conclusion: outcomeSet(step.Conclusion), Outputs: stringValueObject(step.Outputs),
		}
	}
	frame.JobStatus = outcomeSet(eval.JobStatus)
	return frame
}

func anyValueObject(values map[string]any) program.ValueObject {
	result := program.ValueObject{Fields: make(map[string]expression.AbstractValue, len(values))}
	for name, value := range values {
		result.Fields[name] = expression.AbstractValue{Known: true, Value: value}
	}
	return result
}

func outcomeSet(value string) program.OutcomeSet {
	switch value {
	case "failure":
		return program.OutcomeFailure
	case "cancelled":
		return program.OutcomeCancelled
	default:
		return program.OutcomeSuccess
	}
}

func actionExecutionError(execution program.Execution) error {
	if execution.Outcomes&(program.OutcomeFailure|program.OutcomeCancelled) == 0 {
		return nil
	}
	if execution.Failure == nil || execution.Failure.Cause == nil {
		return markHardJobFailure(fmt.Errorf("failed action execution has no cause"))
	}
	if execution.Failure.Hard {
		return markHardJobFailure(execution.Failure.Cause)
	}
	return execution.Failure.Cause
}

func concreteExecutionOutputs(outputs program.ValueObject) (map[string]string, error) {
	return concreteStrings(outputs, "action outputs")
}

func normalizedActionInvocation(job plan.Job, stepIndex int, invocationID string) (program.ActionInvocation, error) {
	if stepIndex < 0 || stepIndex >= len(job.Program.Job.Steps) {
		return program.ActionInvocation{}, fmt.Errorf("normalized action step %d is missing", stepIndex+1)
	}
	step := job.Program.Job.Steps[stepIndex]
	if step.Invocation == nil || step.Invocation.Lock == "" {
		return program.ActionInvocation{}, fmt.Errorf("normalized action step %q has no invocation", step.ID)
	}
	// The job scheduler has already admitted this top-level step. always()
	// prevents the machine from adding a second implicit success() guard.
	return program.ActionInvocation{
		ID: invocationID, Lock: step.Invocation.Lock, Inputs: step.Invocation.With,
		Condition: program.Site{Source: "always()", Surface: program.SurfaceStepCondition, Result: program.ResultBoolean, Provenance: program.ProvenanceWorkflow},
	}, nil
}

func newActionAdapter(run *jobRun) (*actionAdapter, actionAdapterState) {
	return &actionAdapter{run: run}, actionAdapterState{}
}

var _ program.ActionAdapter[actionAdapterState] = (*actionAdapter)(nil)

func (a *actionAdapter) Evaluate(ctx context.Context, state actionAdapterState, site program.Site, view program.FrameView) (actionAdapterState, expression.Analysis, error) {
	eval, condition, err := concreteEvaluationContexts(a.eval, view, a.run.job.Program.Job.Steps)
	if err != nil {
		return state, expression.Analysis{}, err
	}
	if lock := view.CurrentLock(); lock != "" && site.Provenance == program.ProvenanceAction {
		facts, sourceErr := a.run.actions.sourceFacts(ctx, plan.ActionSelector{Lock: lock})
		if sourceErr != nil {
			return state, expression.Analysis{}, sourceErr
		}
		actionPath := facts.ActionPath
		if a.run.jobContainer != nil {
			actionPath = a.run.jobContainer.containerPath(actionPath)
		}
		eval.GitHub = cloneAnyMap(eval.GitHub)
		eval.GitHub["action_path"] = actionPath
		eval.GitHub["action_repository"] = ""
		eval.GitHub["action_ref"] = ""
		if facts.Lock.Source == "github" {
			eval.GitHub["action_repository"] = facts.Lock.Repository
			eval.GitHub["action_ref"] = facts.Lock.RequestedRef
		}
		condition.GitHub = eval.GitHub
	}
	if site.Provenance == program.ProvenanceAction && site.Surface != program.SurfaceActionLifecycle {
		eval.HashFiles = nil
		condition.HashFiles = nil
		condition.Inputs = make(map[string]any, len(eval.Inputs))
		for name, value := range eval.Inputs {
			condition.Inputs[name] = value
		}
	}
	value, err := program.EvaluateSite(site, program.EvaluationContext{Expression: eval, Condition: condition})
	if err != nil {
		return state, expression.Analysis{}, err
	}
	return state, expression.Analysis{Value: expression.AbstractValue{Known: true, Value: value}}, nil
}

func (a *actionAdapter) Execute(ctx context.Context, state actionAdapterState, operation program.LeafOperation, view program.FrameView) (actionAdapterState, program.Execution, error) {
	if a.run == nil || a.run.actions == nil {
		return state, program.Execution{}, fmt.Errorf("action adapter runtime is not configured")
	}
	facts, err := a.run.actions.sourceFacts(ctx, plan.ActionSelector{Lock: operation.Lock})
	if err != nil {
		return state, program.Execution{}, err
	}
	inputs, err := concreteStrings(operation.Inputs, "action inputs")
	if err != nil {
		return state, program.Execution{}, err
	}
	environment, err := concreteStrings(operation.Environment, "action environment")
	if err != nil {
		return state, program.Execution{}, err
	}
	if operation.Phase == program.PhasePre && a.preparation != nil {
		ctx, err = a.preparation.context(ctx)
		if err != nil {
			return state, program.Execution{}, err
		}
	}
	environment = mergeStepEnvironment(a.run.runtimeEnv, environment)
	processor := a.run.processor
	if processor == nil {
		processor = newCommandProcessor(a.run.stdout(), a.run.stderr())
	}
	result := newResult()
	var processErr error
	switch operation.Kind {
	case program.LeafJavaScript:
		entrypoint, entryErr := verifiedActionEntrypoint(facts, operation.Entrypoint)
		if entryErr != nil {
			return state, program.Execution{}, entryErr
		}
		node, nodeErr := a.run.discoverNode(ctx, operation.NodeMajor, a.run.explicitNode(operation.NodeMajor))
		if nodeErr != nil {
			return state, program.Execution{}, nodeErr
		}
		var stateEnv map[string]string
		if operation.Phase == program.PhasePost {
			stateEnv, err = concreteStrings(view.State(), "action state")
			if err != nil {
				return state, program.Execution{}, err
			}
		}
		stateOut := map[string]string{}
		action := javaScriptAction{Name: facts.Action.Name, Path: facts.ActionPath, Inputs: inputs, Env: environment,
			Cache: usesCacheService(facts.Lock), CacheClientCompatibility: usesCacheClientCompatibility(facts.Lock), nodeMajor: operation.NodeMajor,
			reference: normalizedActionReference(facts.Lock)}
		_ = entrypoint // verified above; retain the normalized relative entry for execution.
		processErr = a.run.runJavaScriptPhase(ctx, processor, a.run.workspace, node, action, operation.Entrypoint, stateEnv, stateOut, &result)
	case program.LeafShell:
		command, shell, working, valueErr := concreteShellOperation(operation)
		if valueErr != nil {
			return state, program.Execution{}, valueErr
		}
		dir, dirErr := stepWorkingDirectory(a.run.workspace, working)
		if dirErr != nil {
			return state, program.Execution{}, dirErr
		}
		if shell == "" {
			shell = "bash"
		}
		environment = cloneStrings(environment)
		actionPath := facts.ActionPath
		if a.run.jobContainer != nil {
			actionPath = a.run.jobContainer.containerPath(actionPath)
		}
		environment["GITHUB_ACTION_PATH"] = actionPath
		processErr = a.run.runShellProcess(ctx, processor, dir, environment, &result, shell, command)
	case program.LeafDocker:
		if facts.Action.Docker.Image == "" {
			if facts.Action.Docker.Entrypoint != "" {
				return state, program.Execution{}, fmt.Errorf("normalized Dockerfile action cannot override its entrypoint")
			}
			if _, entryErr := verifiedActionEntrypoint(facts, "Dockerfile"); entryErr != nil {
				return state, program.Execution{}, entryErr
			}
		} else if facts.Action.Docker.Image != facts.Lock.DockerImage {
			return state, program.Execution{}, fmt.Errorf("normalized action image %q does not match planned image %q", facts.Action.Docker.Image, facts.Lock.DockerImage)
		}
		arguments, valueErr := concreteValues(operation.Arguments, "Docker arguments")
		if valueErr != nil {
			return state, program.Execution{}, valueErr
		}
		actionEnvironment, valueErr := concreteStrings(operation.ActionEnvironment, "Docker action environment")
		if valueErr != nil {
			return state, program.Execution{}, valueErr
		}
		environment = mergeStepEnvironment(environment, actionInputEnv(inputs))
		for name, value := range actionEnvironment {
			if _, exists := environment[name]; !exists || name == "PATH" && !operation.ExplicitPATH {
				environment[name] = value
			}
		}
		_, actionPATH := actionEnvironment["PATH"]
		result, processErr = a.run.runDocker(ctx, processor, dockerAction{Name: facts.Action.Name, Path: facts.ActionPath,
			SourceRoot: facts.RepositoryRoot, SourceDigest: facts.Lock.SourceDigest, Args: arguments,
			Image: facts.Action.Docker.Image, Entrypoint: facts.Action.Docker.Entrypoint,
			Workspace: a.run.workspace, Env: environment, explicitPATH: operation.ExplicitPATH || actionPATH})
	case program.LeafNative:
		descriptor, admitted, admitErr := actionintegration.Admit(actionintegration.Identity{Source: facts.Lock.Source, Repository: facts.Lock.Repository, Path: facts.Lock.Path}, facts.Lock.Commit)
		if admitErr != nil || !admitted || descriptor.Adapter == "" {
			return state, program.Execution{}, fmt.Errorf("action lock %q has no admitted native adapter", operation.Lock)
		}
		switch descriptor.Adapter {
		case actionintegration.AdapterCheckoutExactEventSHA:
			sourceInputs := make(map[string]string, len(operation.InputBindings))
			for _, binding := range operation.InputBindings {
				sourceInputs[binding.Name] = binding.Value.Source
			}
			if provenanceErr := validateCheckoutRefProvenance(sourceInputs, inputs, a.run.job.Event.SHA); provenanceErr != nil {
				processErr = provenanceErr
				break
			}
			result, processErr = a.run.runCheckout(ctx, processor, a.run.workspace, a.run.job, facts.Lock.Commit, inputs)
		case actionintegration.AdapterUploadArtifactBuildkite:
			result, processErr = a.run.runUploadArtifactCommit(ctx, processor, a.run.workspace, facts.Lock.Commit, inputs)
		case actionintegration.AdapterDownloadArtifactBuildkite:
			result, processErr = a.run.runDownloadArtifact(ctx, processor, a.run.workspace, a.run.job.Needs, facts.Lock.Commit, inputs)
		default:
			return state, program.Execution{}, fmt.Errorf("action lock %q uses unsupported native adapter %q", operation.Lock, descriptor.Adapter)
		}
	default:
		return state, program.Execution{}, fmt.Errorf("unsupported normalized leaf kind %q", operation.Kind)
	}
	state.Results = append(state.Results, result)
	return state, executionFromResult(ctx, result, processErr, environment), nil
}

func normalizedActionReference(lock plan.ActionLock) string {
	if lock.Source == "workspace" {
		return "./" + strings.TrimPrefix(lock.Path, "./")
	}
	reference := lock.Repository
	if lock.Path != "" {
		reference += "/" + lock.Path
	}
	return reference + "@" + lock.RequestedRef
}

func (*actionAdapter) Fork(state actionAdapterState) (actionAdapterState, actionAdapterState) {
	return cloneAdapterState(state), cloneAdapterState(state)
}

func (*actionAdapter) Join(left, right actionAdapterState) actionAdapterState { return left }

func concreteEvaluationContexts(base expression.Context, view program.FrameView, planned []program.Step) (expression.Context, expression.ConditionContext, error) {
	workflow, err := concreteAny(view.WorkflowInputs(), "workflow inputs")
	if err != nil {
		return expression.Context{}, expression.ConditionContext{}, err
	}
	inputs, err := concreteStrings(view.ActionInputs(), "action inputs")
	if err != nil {
		return expression.Context{}, expression.ConditionContext{}, err
	}
	env, err := concreteStrings(view.Environment(), "environment")
	if err != nil {
		return expression.Context{}, expression.ConditionContext{}, err
	}
	github, err := concreteAny(view.GitHub(), "github")
	if err != nil {
		return expression.Context{}, expression.ConditionContext{}, err
	}
	matrix, err := concreteAny(view.Matrix(), "matrix")
	if err != nil {
		return expression.Context{}, expression.ConditionContext{}, err
	}
	steps := map[string]expression.StepStatus{}
	for name, step := range view.Steps() {
		outputs, outputErr := concreteStrings(step.Outputs, "step outputs")
		if outputErr != nil {
			return expression.Context{}, expression.ConditionContext{}, outputErr
		}
		steps[name] = expression.StepStatus{Outcome: outcomeName(step.Outcome), Conclusion: outcomeName(step.Conclusion), Outputs: outputs}
	}
	if view.Preparation() {
		for _, step := range planned {
			if _, exists := steps[step.ID]; !exists {
				steps[step.ID] = expression.StepStatus{Outputs: map[string]string{}}
			}
		}
	}
	status := outcomeName(view.JobStatus())
	eval := cloneExpressionContext(base)
	eval.Inputs, eval.WorkflowInputs, eval.Env, eval.GitHub, eval.Matrix, eval.Steps, eval.JobStatus = inputs, workflow, env, github, matrix, steps, status
	condition := expression.ConditionContext{Inputs: workflow, Needs: eval.Needs, Env: env, Vars: eval.Vars, GitHub: github, Matrix: matrix, Steps: steps, Runner: eval.Runner, Services: eval.Services, HashFiles: eval.HashFiles,
		Failure: status == "failure", Cancelled: status == "cancelled", Unsuccessful: status != "success"}
	return eval, condition, nil
}

func concreteAny(object program.ValueObject, label string) (map[string]any, error) {
	if object.Open {
		return nil, fmt.Errorf("%s contain unknown values", label)
	}
	values := make(map[string]any, len(object.Fields))
	for name, value := range object.Fields {
		if !value.Known {
			return nil, fmt.Errorf("%s value %q is unknown", label, name)
		}
		values[name] = value.Value
	}
	return values, nil
}

func concreteStrings(object program.ValueObject, label string) (map[string]string, error) {
	values, err := concreteAny(object, label)
	if err != nil {
		return nil, err
	}
	strings := make(map[string]string, len(values))
	for name, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s value %q is %T, want string", label, name, value)
		}
		strings[name] = text
	}
	return strings, nil
}

func concreteValues(values []expression.AbstractValue, label string) ([]string, error) {
	result := make([]string, len(values))
	for i, value := range values {
		if !value.Known {
			return nil, fmt.Errorf("%s value %d is unknown", label, i+1)
		}
		text, ok := value.Value.(string)
		if !ok {
			return nil, fmt.Errorf("%s value %d is %T, want string", label, i+1, value.Value)
		}
		result[i] = text
	}
	return result, nil
}

func concreteShellOperation(operation program.LeafOperation) (string, string, string, error) {
	values, err := concreteValues([]expression.AbstractValue{operation.Command, operation.Shell, operation.WorkingDirectory}, "shell operation")
	if err != nil {
		return "", "", "", err
	}
	return values[0], values[1], values[2], nil
}

func verifiedActionEntrypoint(facts actionSourceFacts, entry string) (string, error) {
	if entry == "" || filepath.IsAbs(entry) {
		return "", fmt.Errorf("normalized action entrypoint is missing or absolute")
	}
	candidate := filepath.Join(facts.ActionPath, filepath.FromSlash(entry))
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve normalized action entrypoint: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(facts.RepositoryRoot)
	if err != nil {
		return "", fmt.Errorf("resolve verified source root: %w", err)
	}
	rel, relErr := filepath.Rel(resolvedRoot, resolved)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("normalized action entrypoint escapes verified source root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("normalized action entrypoint is not a regular file")
	}
	return resolved, nil
}

func executionFromResult(ctx context.Context, result Result, err error, currentEnvironment map[string]string) program.Execution {
	outcome := program.OutcomeSuccess
	if err != nil {
		outcome = program.OutcomeFailure
		if ctx.Err() != nil {
			outcome = program.OutcomeCancelled
		}
	}
	environment := cloneStrings(result.Env)
	if len(result.Paths) != 0 {
		pathEnvironment := cloneStrings(currentEnvironment)
		commitResultEnvironment(pathEnvironment, result)
		environment["PATH"] = pathEnvironment["PATH"]
	}
	execution := program.Execution{Outcomes: outcome, Outputs: stringValueObject(result.Outputs), State: stringValueObject(result.State),
		Environment: program.EnvironmentMutation{Sets: abstractStrings(environment)}}
	if err != nil {
		execution.Failure = &program.ExecutionFailure{Cause: err, Hard: isHardJobFailure(err)}
	}
	return execution
}

func stringValueObject(values map[string]string) program.ValueObject {
	return program.ValueObject{Fields: abstractStrings(values)}
}
func abstractStrings(values map[string]string) map[string]expression.AbstractValue {
	result := make(map[string]expression.AbstractValue, len(values))
	for k, v := range values {
		result[k] = expression.AbstractValue{Known: true, Value: v}
	}
	return result
}
func outcomeName(outcome program.OutcomeSet) string {
	if outcome == 0 {
		return "skipped"
	}
	if outcome&program.OutcomeCancelled != 0 {
		return "cancelled"
	}
	if outcome&program.OutcomeFailure != 0 {
		return "failure"
	}
	return "success"
}
func cloneAdapterState(state actionAdapterState) actionAdapterState {
	clone := actionAdapterState{Results: make([]Result, len(state.Results))}
	for i, result := range state.Results {
		clone.Results[i] = cloneResult(result)
	}
	return clone
}
