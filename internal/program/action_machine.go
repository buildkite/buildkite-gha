package program

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

type OutcomeSet uint8

const (
	OutcomeSuccess OutcomeSet = 1 << iota
	OutcomeFailure
	OutcomeCancelled
)

type ValueObject struct {
	Fields map[string]expression.AbstractValue
	Open   bool
}

type EnvironmentMutation struct {
	Sets         map[string]expression.AbstractValue
	PrependsPATH []expression.AbstractValue
	Unknown      bool
}

type ExecutionFailure struct {
	Cause error
	Hard  bool
}

type Execution struct {
	Outcomes    OutcomeSet
	Outputs     ValueObject
	Environment EnvironmentMutation
	State       ValueObject
	Failure     *ExecutionFailure
}

type StepFrame struct {
	Outcome    OutcomeSet
	Conclusion OutcomeSet
	Outputs    ValueObject
}

type Frame struct {
	WorkflowInputs    ValueObject
	ActionInputs      ValueObject
	Environment       ValueObject
	StableEnvironment ValueObject
	Steps             map[string]StepFrame
	State             ValueObject
	GitHub            ValueObject
	Matrix            ValueObject
	JobStatus         OutcomeSet
	CurrentLock       string
	ExplicitPATH      bool
	Preparation       bool
}

// FrameView prevents adapters from mutating lifecycle state owned by the
// machine. Object maps are cloned when the view is constructed.
type FrameView struct{ frame Frame }

func (v FrameView) WorkflowInputs() ValueObject { return cloneValueObject(v.frame.WorkflowInputs) }
func (v FrameView) ActionInputs() ValueObject   { return cloneValueObject(v.frame.ActionInputs) }
func (v FrameView) Environment() ValueObject    { return cloneValueObject(v.frame.Environment) }
func (v FrameView) StableEnvironment() ValueObject {
	return cloneValueObject(v.frame.StableEnvironment)
}
func (v FrameView) Steps() map[string]StepFrame { return cloneStepFrames(v.frame.Steps) }
func (v FrameView) State() ValueObject          { return cloneValueObject(v.frame.State) }
func (v FrameView) GitHub() ValueObject         { return cloneValueObject(v.frame.GitHub) }
func (v FrameView) Matrix() ValueObject         { return cloneValueObject(v.frame.Matrix) }
func (v FrameView) JobStatus() OutcomeSet       { return v.frame.JobStatus }
func (v FrameView) CurrentLock() string         { return v.frame.CurrentLock }
func (v FrameView) Preparation() bool           { return v.frame.Preparation }

type LeafKind string

type LeafPhase string

const (
	LeafJavaScript LeafKind  = "javascript"
	LeafDocker     LeafKind  = "docker"
	LeafShell      LeafKind  = "shell"
	LeafNative     LeafKind  = "native"
	PhasePre       LeafPhase = "pre"
	PhaseMain      LeafPhase = "main"
	PhasePost      LeafPhase = "post"
)

type LeafOperation struct {
	Kind              LeafKind
	Phase             LeafPhase
	Lock              string
	InvocationID      string
	Entrypoint        string
	NodeMajor         int
	Command           expression.AbstractValue
	Shell             expression.AbstractValue
	WorkingDirectory  expression.AbstractValue
	Arguments         []expression.AbstractValue
	Inputs            ValueObject
	InputBindings     []Binding
	Environment       ValueObject
	ActionEnvironment ValueObject
	ExplicitPATH      bool
}

type ActionAdapter[S any] interface {
	Evaluate(context.Context, S, Site, FrameView) (S, expression.Analysis, error)
	Execute(context.Context, S, LeafOperation, FrameView) (S, Execution, error)
	Fork(S) (S, S)
	Join(S, S) S
}

type GuardDecision uint8

const (
	GuardFalse GuardDecision = iota
	GuardTrue
	GuardUnknown
)

type preparedInvocation struct {
	Overlay        ValueObject
	Inputs         ValueObject
	State          ValueObject
	Failure        *Execution
	PostRegistered bool
	Possible       bool
}

type registeredPost struct {
	InvocationID string
	Lock         string
	Condition    Site
	Frame        Frame
	Overlay      ValueObject
	Supplied     map[string]bool
	Possible     bool
}

type ActionMachine[S any] struct {
	actions    map[string]Action
	adapter    ActionAdapter[S]
	state      S
	prepared   map[string]preparedInvocation
	posts      []registeredPost
	postAdded  func()
	active     map[string]bool
	stepScopes map[string]map[string]StepFrame
}

func NewActionMachine[S any](actions map[string]Action, adapter ActionAdapter[S], state S) *ActionMachine[S] {
	return &ActionMachine[S]{
		actions: actions, adapter: adapter, state: state,
		prepared: map[string]preparedInvocation{}, active: map[string]bool{}, stepScopes: map[string]map[string]StepFrame{},
	}
}

func (m *ActionMachine[S]) State() S { return m.state }

// SetPostRegistrationCallback observes concrete post registrations without
// taking ownership of their lifecycle state. Speculative clones do not inherit
// the callback.
func (m *ActionMachine[S]) SetPostRegistrationCallback(callback func()) {
	m.postAdded = callback
}

func (m *ActionMachine[S]) Prepare(ctx context.Context, invocation ActionInvocation, frame Frame) (Frame, Execution, error) {
	if invocation.ID == "" {
		return Frame{}, Execution{}, fmt.Errorf("prepare action invocation ID is missing")
	}
	action, err := m.action(invocation.Lock)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	if action.Runtime == ActionRuntimeNative {
		return frame, successfulExecution(), nil
	}
	if action.Source == "workspace" {
		return frame, successfulExecution(), nil
	}
	frame = cloneFrame(frame)
	frame.Preparation = true
	return m.prepare(ctx, invocation, action, frame)
}

func (m *ActionMachine[S]) Invoke(ctx context.Context, invocation ActionInvocation, frame Frame) (Frame, Execution, error) {
	if invocation.ID == "" {
		return Frame{}, Execution{}, fmt.Errorf("invoke action invocation ID is missing")
	}
	action, err := m.action(invocation.Lock)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	frame = cloneFrame(frame)
	frame.Preparation = false
	return m.invokeGuarded(ctx, invocation, action, frame)
}

func (m *ActionMachine[S]) Finish(ctx context.Context, frame Frame) (Frame, Execution, error) {
	result := successfulExecution()
	var finishErr error
	for len(m.posts) != 0 {
		var execution Execution
		var err error
		frame, execution, err = m.FinishOne(ctx, frame)
		finishErr = errors.Join(finishErr, err)
		if execution.Outcomes != 0 {
			result = accumulateExecutions(result, execution)
		}
	}
	return frame, result, finishErr
}

// FinishOne runs the most recently registered post in this machine. The post
// is removed before evaluation so failures cannot make it runnable twice.
func (m *ActionMachine[S]) FinishOne(ctx context.Context, frame Frame) (Frame, Execution, error) {
	if len(m.posts) == 0 {
		return frame, Execution{}, fmt.Errorf("finish action post: registration stack is empty")
	}
	post := m.posts[len(m.posts)-1]
	m.posts = m.posts[:len(m.posts)-1]
	action, err := m.action(post.Lock)
	if err != nil {
		return frame, Execution{}, err
	}
	if action.JavaScript == nil || action.JavaScript.Post == "" {
		return frame, successfulExecution(), nil
	}
	postFrame := cloneFrame(post.Frame)
	postFrame.Environment = overlayEnvironment(frame.Environment, post.Overlay)
	if separator := strings.LastIndex(post.InvocationID, "/"); separator >= 0 {
		if steps, ok := m.stepScopes[post.InvocationID[:separator]]; ok {
			postFrame.Steps = cloneStepFrames(steps)
		}
	} else {
		postFrame.Steps = cloneStepFrames(frame.Steps)
	}
	postFrame.JobStatus = frame.JobStatus
	for _, input := range action.Inputs {
		if input.Default == nil || post.Supplied[strings.ToLower(input.Name)] {
			continue
		}
		referencesStatus, err := expression.ReferencesJobStatus(input.Default.Source)
		if err != nil {
			execution := failureExecution(fmt.Errorf("action input %q default: %w", input.Name, err))
			return applyFailedPost(frame, execution), execution, nil
		}
		if !referencesStatus {
			continue
		}
		analysis, err := m.evaluate(ctx, *input.Default, postFrame)
		if err != nil {
			execution := failureExecution(fmt.Errorf("action input %q post default: %w", input.Name, err))
			return applyFailedPost(frame, execution), execution, nil
		}
		if postFrame.ActionInputs.Fields == nil {
			postFrame.ActionInputs.Fields = map[string]expression.AbstractValue{}
		}
		postFrame.ActionInputs.Fields[strings.ToLower(input.Name)] = analysis.Value
	}
	condition, err := m.evaluateCondition(ctx, lifecycleConditionSite(action.JavaScript.PostCondition), postFrame)
	if err != nil {
		execution := failureExecution(fmt.Errorf("action post-if: %w", err))
		return applyFailedPost(frame, execution), execution, nil
	}
	if condition == GuardFalse {
		return frame, successfulExecution(), nil
	}
	operation := LeafOperation{
		Kind: LeafJavaScript, Phase: PhasePost, Lock: post.Lock, InvocationID: post.InvocationID,
		Entrypoint: action.JavaScript.Post, NodeMajor: action.JavaScript.NodeMajor,
		Inputs: postFrame.ActionInputs, Environment: postFrame.Environment,
	}
	execution, err := m.execute(ctx, operation, postFrame)
	if err != nil {
		return frame, Execution{}, err
	}
	frame = applyExecution(frame, execution)
	frame.JobStatus = advanceJobStatus(frame.JobStatus, execution.Outcomes)
	return frame, execution, nil
}

func applyFailedPost(frame Frame, execution Execution) Frame {
	frame.JobStatus = advanceJobStatus(frame.JobStatus, execution.Outcomes)
	return frame
}

func (m *ActionMachine[S]) action(lock string) (Action, error) {
	action, ok := m.actions[lock]
	if !ok {
		return Action{}, fmt.Errorf("action program %q is missing", lock)
	}
	return action, nil
}

func (m *ActionMachine[S]) prepare(ctx context.Context, invocation ActionInvocation, action Action, frame Frame) (Frame, Execution, error) {
	if action.Source == "workspace" {
		return frame, successfulExecution(), nil
	}
	if m.active[invocation.Lock] {
		return Frame{}, Execution{}, fmt.Errorf("action program graph contains a cycle at %q", invocation.Lock)
	}
	m.active[invocation.Lock] = true
	defer delete(m.active, invocation.Lock)

	switch action.Runtime {
	case ActionRuntimeJavaScript:
		if action.JavaScript == nil {
			return Frame{}, Execution{}, fmt.Errorf("action program %q has no JavaScript lifecycle", invocation.Lock)
		}
		if action.JavaScript.Pre == "" {
			return frame, successfulExecution(), nil
		}
		overlay, invocationFrame, err := m.enterEnvironment(ctx, invocation.Environment, frame)
		if err != nil {
			execution := failureExecution(err)
			return applyExecution(frame, execution), execution, nil
		}
		invocationFrame.CurrentLock = invocation.Lock
		condition, err := m.evaluateCondition(ctx, lifecycleConditionSite(action.JavaScript.PreCondition), invocationFrame)
		if err != nil {
			execution := failureExecution(fmt.Errorf("action pre-if: %w", err))
			return applyExecution(frame, execution), execution, nil
		}
		if condition == GuardFalse {
			m.prepared[invocation.ID] = preparedInvocation{}
			return frame, successfulExecution(), nil
		}
		if condition == GuardUnknown {
			return m.prepareUnknown(ctx, invocation, action, frame)
		}
		return m.prepareJavaScript(ctx, invocation, action, frame, overlay, invocationFrame, false)
	case ActionRuntimeComposite:
		if action.Composite == nil {
			return Frame{}, Execution{}, fmt.Errorf("action program %q has no composite execution", invocation.Lock)
		}
		inputs, err := m.resolveInputs(ctx, invocation.Inputs, action, frame)
		if err != nil {
			execution := failureExecution(err)
			return applyExecution(frame, execution), execution, nil
		}
		_, invocationFrame, err := m.enterEnvironment(ctx, invocation.Environment, frame)
		if err != nil {
			execution := failureExecution(err)
			return applyExecution(frame, execution), execution, nil
		}
		invocationFrame.ActionInputs = inputs
		invocationFrame.CurrentLock = invocation.Lock
		invocationFrame.Steps = map[string]StepFrame{}
		result := successfulExecution()
		for i, step := range action.Composite.Steps {
			if step.Invocation == nil {
				continue
			}
			childAction, err := m.action(step.Invocation.Lock)
			if err != nil {
				return Frame{}, Execution{}, fmt.Errorf("composite action step %d: %w", i+1, err)
			}
			child := ActionInvocation{
				ID: fmt.Sprintf("%s/%d", invocation.ID, i), Lock: step.Invocation.Lock,
				Inputs: step.Invocation.With, Environment: step.Env,
			}
			var childExecution Execution
			childFrame, childExecution, err := m.prepare(ctx, child, childAction, invocationFrame)
			if err != nil {
				failure := failureExecution(fmt.Errorf("composite action step %d child %q: %w", i+1, step.Invocation.Uses.Source, err))
				if !step.ContinueOnError {
					return Frame{}, failure, nil
				}
				childExecution = failure
			} else {
				invocationFrame = childFrame
			}
			if childExecution.Outcomes&(OutcomeFailure|OutcomeCancelled) != 0 {
				prepared := m.prepared[child.ID]
				failure := childExecution
				prepared.Failure = &failure
				m.prepared[child.ID] = prepared
			}
			effective := childExecution
			if step.ContinueOnError && effective.Outcomes&OutcomeFailure != 0 && (effective.Failure == nil || !effective.Failure.Hard) {
				effective.Outcomes = (effective.Outcomes &^ OutcomeFailure) | OutcomeSuccess
				effective.Failure = nil
			}
			result = accumulateExecutions(result, effective)
			invocationFrame.JobStatus = advanceJobStatus(invocationFrame.JobStatus, effective.Outcomes)
		}
		return frameWithEnvironment(frame, invocationFrame.Environment), result, nil
	case ActionRuntimeDocker, ActionRuntimeNative:
		return frame, successfulExecution(), nil
	default:
		return Frame{}, Execution{}, fmt.Errorf("action program %q has unsupported runtime %q", invocation.Lock, action.Runtime)
	}
}

func (m *ActionMachine[S]) prepareJavaScript(ctx context.Context, invocation ActionInvocation, action Action, parent Frame, overlay ValueObject, invocationFrame Frame, possible bool) (Frame, Execution, error) {
	inputs, err := m.resolveInputs(ctx, invocation.Inputs, action, parent)
	if err != nil {
		execution := failureExecution(err)
		return applyExecution(parent, execution), execution, nil
	}
	invocationFrame.ActionInputs = inputs
	prepared := preparedInvocation{Overlay: overlay, Inputs: inputs, Possible: possible}
	m.prepared[invocation.ID] = prepared
	operation := LeafOperation{
		Kind: LeafJavaScript, Phase: PhasePre, Lock: invocation.Lock, InvocationID: invocation.ID,
		Entrypoint: action.JavaScript.Pre, NodeMajor: action.JavaScript.NodeMajor,
		Inputs: invocationFrame.ActionInputs, Environment: invocationFrame.Environment,
	}
	execution, err := m.execute(ctx, operation, invocationFrame)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	invocationFrame = applyExecution(invocationFrame, execution)
	if action.JavaScript.Post != "" {
		m.registerPost(invocation.ID, invocation.Lock, action.JavaScript.PostCondition, invocationFrame, overlay, invocation.Inputs, possible)
		prepared.PostRegistered = true
	}
	prepared.State = cloneValueObject(execution.State)
	invocationFrame.State = cloneValueObject(prepared.State)
	if execution.Outcomes&(OutcomeFailure|OutcomeCancelled) != 0 {
		failure := execution
		prepared.Failure = &failure
	}
	m.prepared[invocation.ID] = prepared
	if prepared.PostRegistered {
		m.updatePostFrame(invocation.ID, invocationFrame)
	}
	return applyExecution(parent, execution), execution, nil
}

func (m *ActionMachine[S]) prepareUnknown(ctx context.Context, invocation ActionInvocation, action Action, frame Frame) (Frame, Execution, error) {
	trueState, falseState := m.adapter.Fork(m.state)
	trueMachine := m.clone(trueState)
	overlay, invocationFrame, err := trueMachine.enterEnvironment(ctx, invocation.Environment, frame)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	trueFrame, execution, err := trueMachine.prepareJavaScript(ctx, invocation, action, frame, overlay, invocationFrame, true)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	m.state = m.adapter.Join(trueMachine.state, falseState)
	m.prepared = joinPrepared(trueMachine.prepared, m.prepared)
	m.posts = joinPosts(trueMachine.posts, m.posts)
	m.stepScopes = joinStepScopes(trueMachine.stepScopes, m.stepScopes)
	return joinFrames(trueFrame, frame), joinExecutions(execution, successfulExecution()), nil
}

func (m *ActionMachine[S]) invokeGuarded(ctx context.Context, invocation ActionInvocation, action Action, frame Frame) (Frame, Execution, error) {
	condition, err := m.evaluateCondition(ctx, invocation.Condition, frame)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	if condition == GuardFalse {
		return frame, successfulExecution(), nil
	}
	if condition == GuardUnknown {
		return m.invokeUnknown(ctx, invocation, action, frame)
	}
	return m.invoke(ctx, invocation, action, frame)
}

func (m *ActionMachine[S]) invokeUnknown(ctx context.Context, invocation ActionInvocation, action Action, frame Frame) (Frame, Execution, error) {
	trueState, falseState := m.adapter.Fork(m.state)
	trueMachine := m.clone(trueState)
	falseMachine := m.clone(falseState)
	trueFrame, trueExecution, err := trueMachine.invoke(ctx, invocation, action, cloneFrame(frame))
	if err != nil {
		return Frame{}, Execution{}, err
	}
	m.state = m.adapter.Join(trueMachine.state, falseMachine.state)
	m.prepared = joinPrepared(trueMachine.prepared, falseMachine.prepared)
	m.posts = joinPosts(trueMachine.posts, falseMachine.posts)
	m.stepScopes = joinStepScopes(trueMachine.stepScopes, falseMachine.stepScopes)
	return joinFrames(trueFrame, frame), joinExecutions(trueExecution, successfulExecution()), nil
}

func (m *ActionMachine[S]) invoke(ctx context.Context, invocation ActionInvocation, action Action, frame Frame) (Frame, Execution, error) {
	if m.active[invocation.Lock] {
		return Frame{}, Execution{}, fmt.Errorf("action program graph contains a cycle at %q", invocation.Lock)
	}
	m.active[invocation.Lock] = true
	defer delete(m.active, invocation.Lock)
	prefix := successfulExecution()
	if prepared, ok := m.prepared[invocation.ID]; ok && prepared.Failure != nil {
		delete(m.prepared, invocation.ID)
		return frame, failureOnly(*prepared.Failure), nil
	}
	if action.Runtime == ActionRuntimeJavaScript && action.JavaScript != nil && action.JavaScript.Pre != "" {
		if _, prepared := m.prepared[invocation.ID]; !prepared {
			overlay, preFrame, err := m.enterEnvironment(ctx, invocation.Environment, frame)
			if err != nil {
				execution := failureExecution(err)
				return applyExecution(frame, execution), execution, nil
			}
			preFrame.CurrentLock = invocation.Lock
			condition, err := m.evaluateCondition(ctx, lifecycleConditionSite(action.JavaScript.PreCondition), preFrame)
			if err != nil {
				execution := failureExecution(fmt.Errorf("action pre-if: %w", err))
				return applyExecution(frame, execution), execution, nil
			}
			switch condition {
			case GuardUnknown:
				frame, prefix, err = m.prepareUnknown(ctx, invocation, action, frame)
			case GuardTrue:
				frame, prefix, err = m.prepareJavaScript(ctx, invocation, action, frame, overlay, preFrame, false)
			}
			if err != nil {
				return Frame{}, Execution{}, err
			}
			if prefix.Outcomes&(OutcomeFailure|OutcomeCancelled) != 0 {
				delete(m.prepared, invocation.ID)
				return frame, prefix, nil
			}
		}
	}

	var inputs ValueObject
	var err error
	if action.Runtime == ActionRuntimeNative {
		inputs, err = m.evaluateBindings(ctx, invocation.Inputs, frame)
	} else {
		inputs, err = m.resolveInputs(ctx, invocation.Inputs, action, frame)
	}
	if err != nil {
		execution := failureExecution(err)
		return applyExecution(frame, execution), execution, nil
	}
	overlay, invocationFrame, err := m.enterEnvironment(ctx, invocation.Environment, frame)
	if err != nil {
		execution := failureExecution(err)
		return applyExecution(frame, execution), execution, nil
	}
	invocationFrame.ActionInputs = inputs
	invocationFrame.CurrentLock = invocation.Lock

	var resultFrame Frame
	var execution Execution
	switch action.Runtime {
	case ActionRuntimeNative:
		execution, err = m.execute(ctx, LeafOperation{
			Kind: LeafNative, Phase: PhaseMain, Lock: invocation.Lock, InvocationID: invocation.ID,
			Inputs: inputs, InputBindings: invocation.Inputs, Environment: invocationFrame.Environment,
		}, invocationFrame)
		if err != nil {
			return Frame{}, Execution{}, err
		}
		resultFrame = applyExecution(frame, execution)
	case ActionRuntimeJavaScript:
		resultFrame, execution, err = m.invokeJavaScript(ctx, invocation, action, frame, invocationFrame, overlay)
	case ActionRuntimeComposite:
		resultFrame, execution, err = m.invokeComposite(ctx, invocation, action, frame, invocationFrame)
	case ActionRuntimeDocker:
		resultFrame, execution, err = m.invokeDocker(ctx, invocation, action, frame, invocationFrame)
	default:
		return Frame{}, Execution{}, fmt.Errorf("action program %q has unsupported runtime %q", invocation.Lock, action.Runtime)
	}
	if err != nil {
		return Frame{}, Execution{}, err
	}
	return resultFrame, sequenceExecutions(prefix, execution), nil
}

func (m *ActionMachine[S]) evaluate(ctx context.Context, site Site, frame Frame) (expression.Analysis, error) {
	var analysis expression.Analysis
	var err error
	m.state, analysis, err = m.adapter.Evaluate(ctx, m.state, site, frame.view())
	return analysis, err
}

func (m *ActionMachine[S]) execute(ctx context.Context, operation LeafOperation, frame Frame) (Execution, error) {
	var execution Execution
	var err error
	m.state, execution, err = m.adapter.Execute(ctx, m.state, operation, frame.view())
	if err != nil {
		return Execution{}, &adapterExecutionError{err: err}
	}
	if execution.Outcomes == 0 {
		return Execution{}, fmt.Errorf("%s leaf returned no outcome", operation.Kind)
	}
	return execution, nil
}

type adapterExecutionError struct{ err error }

func (e *adapterExecutionError) Error() string     { return e.err.Error() }
func (e *adapterExecutionError) Unwrap() error     { return e.err }
func (e *adapterExecutionError) HardFailure() bool { return true }

type hardProgramError struct{ err error }

func (e *hardProgramError) Error() string     { return e.err.Error() }
func (e *hardProgramError) Unwrap() error     { return e.err }
func (e *hardProgramError) HardFailure() bool { return true }

type hardFailureMarker interface {
	HardFailure() bool
}

func failureExecution(err error) Execution {
	hard := false
	var marker hardFailureMarker
	if errors.As(err, &marker) {
		hard = hard || marker.HardFailure()
	}
	return Execution{Outcomes: OutcomeFailure, Failure: &ExecutionFailure{Cause: err, Hard: hard}}
}

func guardDecision(analysis expression.Analysis) (GuardDecision, error) {
	if !analysis.Value.Known {
		return GuardUnknown, nil
	}
	value, ok := analysis.Value.Value.(bool)
	if !ok {
		return GuardFalse, fmt.Errorf("condition produced %T, want boolean", analysis.Value.Value)
	}
	if value {
		return GuardTrue, nil
	}
	return GuardFalse, nil
}

func (f Frame) view() FrameView { return FrameView{frame: cloneFrame(f)} }

func cloneFrame(frame Frame) Frame {
	frame.WorkflowInputs = cloneValueObject(frame.WorkflowInputs)
	frame.ActionInputs = cloneValueObject(frame.ActionInputs)
	frame.Environment = cloneValueObject(frame.Environment)
	frame.StableEnvironment = cloneValueObject(frame.StableEnvironment)
	frame.Steps = cloneStepFrames(frame.Steps)
	frame.State = cloneValueObject(frame.State)
	frame.GitHub = cloneValueObject(frame.GitHub)
	frame.Matrix = cloneValueObject(frame.Matrix)
	return frame
}

func cloneValueObject(object ValueObject) ValueObject {
	return ValueObject{Fields: maps.Clone(object.Fields), Open: object.Open}
}

func cloneStepFrames(steps map[string]StepFrame) map[string]StepFrame {
	if steps == nil {
		return nil
	}
	cloned := make(map[string]StepFrame, len(steps))
	for name, step := range steps {
		step.Outputs = cloneValueObject(step.Outputs)
		cloned[name] = step
	}
	return cloned
}

func normalizeValueObject(object ValueObject) ValueObject {
	if object.Fields == nil {
		object.Fields = map[string]expression.AbstractValue{}
	}
	values := make(map[string]expression.AbstractValue, len(object.Fields))
	for name, value := range object.Fields {
		values[strings.ToLower(name)] = value
	}
	object.Fields = values
	return object
}

func successfulExecution() Execution { return Execution{Outcomes: OutcomeSuccess} }

func frameWithEnvironment(frame Frame, environment ValueObject) Frame {
	frame.Environment = cloneValueObject(environment)
	return frame
}

func overlayEnvironment(live, overlay ValueObject) ValueObject {
	result := cloneValueObject(live)
	if result.Fields == nil {
		result.Fields = map[string]expression.AbstractValue{}
	}
	for name, value := range overlay.Fields {
		result.Fields[name] = value
	}
	result.Open = result.Open || overlay.Open
	return result
}

func overlayValueObject(base, overlay ValueObject) ValueObject {
	result := cloneValueObject(base)
	if result.Fields == nil {
		result.Fields = map[string]expression.AbstractValue{}
	}
	for name, value := range overlay.Fields {
		result.Fields[name] = value
	}
	result.Open = result.Open || overlay.Open
	return result
}

func applyExecution(frame Frame, execution Execution) Frame {
	frame = cloneFrame(frame)
	if execution.Environment.Unknown {
		frame.Environment.Open = true
		for name := range frame.Environment.Fields {
			frame.Environment.Fields[name] = expression.AbstractValue{}
		}
	}
	if frame.Environment.Fields == nil {
		frame.Environment.Fields = map[string]expression.AbstractValue{}
	}
	for name, value := range execution.Environment.Sets {
		frame.Environment.Fields[name] = value
	}
	if len(execution.Environment.PrependsPATH) != 0 {
		frame.Environment.Fields["path"] = expression.AbstractValue{}
	}
	return frame
}

func joinExecutions(left, right Execution) Execution {
	result := Execution{
		Outcomes: left.Outcomes | right.Outcomes,
		Outputs:  joinValueObjects(left.Outputs, right.Outputs),
		State:    joinValueObjects(left.State, right.State),
		Environment: EnvironmentMutation{
			Sets:         joinMutationSets(left.Environment.Sets, right.Environment.Sets),
			PrependsPATH: append(append([]expression.AbstractValue(nil), left.Environment.PrependsPATH...), right.Environment.PrependsPATH...),
			Unknown:      left.Environment.Unknown || right.Environment.Unknown,
		},
	}
	if left.Failure != nil || right.Failure != nil {
		result.Failure = &ExecutionFailure{}
		var causes []error
		for _, failure := range []*ExecutionFailure{left.Failure, right.Failure} {
			if failure == nil {
				continue
			}
			result.Failure.Hard = result.Failure.Hard || failure.Hard
			if failure.Cause != nil {
				causes = append(causes, failure.Cause)
			}
		}
		result.Failure.Cause = errors.Join(causes...)
	}
	return result
}

func failureOnly(execution Execution) Execution {
	return Execution{
		Outcomes: execution.Outcomes,
		Outputs:  ValueObject{Open: execution.Outputs.Open},
		Failure:  execution.Failure,
	}
}

func sequenceExecutions(first, second Execution) Execution {
	result := joinExecutions(first, second)
	result.Outcomes = second.Outcomes
	result.Outputs = second.Outputs
	result.State = overlayValueObject(first.State, second.State)
	result.Environment.Sets = maps.Clone(first.Environment.Sets)
	if result.Environment.Sets == nil {
		result.Environment.Sets = map[string]expression.AbstractValue{}
	}
	for name, value := range second.Environment.Sets {
		result.Environment.Sets[name] = value
	}
	return result
}

func accumulateExecutions(first, second Execution) Execution {
	result := sequenceExecutions(first, second)
	if first.Outcomes&(OutcomeFailure|OutcomeCancelled) != 0 {
		result.Outcomes = first.Outcomes | second.Outcomes&(OutcomeFailure|OutcomeCancelled)
	}
	return result
}

func joinFrames(left, right Frame) Frame {
	return Frame{
		WorkflowInputs:    joinValueObjects(left.WorkflowInputs, right.WorkflowInputs),
		ActionInputs:      joinValueObjects(left.ActionInputs, right.ActionInputs),
		Environment:       joinValueObjects(left.Environment, right.Environment),
		StableEnvironment: joinValueObjects(left.StableEnvironment, right.StableEnvironment),
		Steps:             joinStepFrames(left.Steps, right.Steps),
		State:             joinValueObjects(left.State, right.State),
		GitHub:            joinValueObjects(left.GitHub, right.GitHub),
		Matrix:            joinValueObjects(left.Matrix, right.Matrix),
		JobStatus:         left.JobStatus | right.JobStatus,
		CurrentLock:       left.CurrentLock,
		ExplicitPATH:      left.ExplicitPATH || right.ExplicitPATH,
		Preparation:       left.Preparation || right.Preparation,
	}
}

func joinMutationSets(left, right map[string]expression.AbstractValue) map[string]expression.AbstractValue {
	return joinValueObjects(ValueObject{Fields: left}, ValueObject{Fields: right}).Fields
}

func joinValueObjects(left, right ValueObject) ValueObject {
	result := ValueObject{Fields: map[string]expression.AbstractValue{}, Open: left.Open || right.Open}
	names := map[string]struct{}{}
	for name := range left.Fields {
		names[name] = struct{}{}
	}
	for name := range right.Fields {
		names[name] = struct{}{}
	}
	for name := range names {
		leftValue, leftOK := left.Fields[name]
		rightValue, rightOK := right.Fields[name]
		if leftOK && rightOK && reflect.DeepEqual(leftValue, rightValue) {
			result.Fields[name] = leftValue
		} else {
			result.Fields[name] = expression.AbstractValue{}
		}
	}
	return result
}

func joinStepFrames(left, right map[string]StepFrame) map[string]StepFrame {
	result := map[string]StepFrame{}
	for name, step := range left {
		result[name] = step
	}
	for name, rightStep := range right {
		leftStep, ok := result[name]
		if !ok {
			result[name] = StepFrame{Outputs: joinValueObjects(ValueObject{}, rightStep.Outputs), Outcome: rightStep.Outcome, Conclusion: rightStep.Conclusion}
			continue
		}
		result[name] = StepFrame{Outputs: joinValueObjects(leftStep.Outputs, rightStep.Outputs), Outcome: leftStep.Outcome | rightStep.Outcome, Conclusion: leftStep.Conclusion | rightStep.Conclusion}
	}
	return result
}

func joinStepScopes(left, right map[string]map[string]StepFrame) map[string]map[string]StepFrame {
	result := make(map[string]map[string]StepFrame, len(left)+len(right))
	for id, steps := range left {
		result[id] = cloneStepFrames(steps)
	}
	for id, steps := range right {
		result[id] = joinStepFrames(result[id], steps)
	}
	return result
}

func (m *ActionMachine[S]) clone(state S) *ActionMachine[S] {
	prepared := make(map[string]preparedInvocation, len(m.prepared))
	for id, invocation := range m.prepared {
		invocation.Overlay = cloneValueObject(invocation.Overlay)
		invocation.Inputs = cloneValueObject(invocation.Inputs)
		invocation.State = cloneValueObject(invocation.State)
		prepared[id] = invocation
	}
	posts := make([]registeredPost, len(m.posts))
	for i, post := range m.posts {
		post.Frame = cloneFrame(post.Frame)
		post.Supplied = maps.Clone(post.Supplied)
		posts[i] = post
	}
	stepScopes := make(map[string]map[string]StepFrame, len(m.stepScopes))
	for id, steps := range m.stepScopes {
		stepScopes[id] = cloneStepFrames(steps)
	}
	return &ActionMachine[S]{actions: m.actions, adapter: m.adapter, state: state, prepared: prepared, posts: posts, active: maps.Clone(m.active), stepScopes: stepScopes}
}

func joinPrepared(left, right map[string]preparedInvocation) map[string]preparedInvocation {
	result := make(map[string]preparedInvocation, len(left)+len(right))
	for id, invocation := range left {
		result[id] = invocation
	}
	for id, invocation := range right {
		if previous, ok := result[id]; ok {
			invocation.Overlay = joinValueObjects(previous.Overlay, invocation.Overlay)
			invocation.Inputs = joinValueObjects(previous.Inputs, invocation.Inputs)
			invocation.State = joinValueObjects(previous.State, invocation.State)
			invocation.PostRegistered = previous.PostRegistered || invocation.PostRegistered
			invocation.Possible = previous.Possible || invocation.Possible
		} else {
			invocation.Possible = true
		}
		result[id] = invocation
	}
	return result
}

func joinPosts(left, right []registeredPost) []registeredPost {
	result := append([]registeredPost(nil), left...)
	seen := map[string]int{}
	for i, post := range result {
		seen[post.InvocationID] = i
	}
	for _, post := range right {
		if i, ok := seen[post.InvocationID]; ok {
			result[i].Frame = joinFrames(result[i].Frame, post.Frame)
			result[i].Possible = result[i].Possible || post.Possible
			if result[i].Supplied == nil {
				result[i].Supplied = map[string]bool{}
			}
			for name, supplied := range post.Supplied {
				result[i].Supplied[name] = result[i].Supplied[name] || supplied
			}
			continue
		}
		post.Possible = true
		seen[post.InvocationID] = len(result)
		result = append(result, post)
	}
	return result
}

func (m *ActionMachine[S]) registerPost(id, lock string, condition Site, frame Frame, overlay ValueObject, supplied []Binding, possible bool) {
	suppliedNames := make(map[string]bool, len(supplied))
	for _, binding := range supplied {
		suppliedNames[strings.ToLower(binding.Name)] = true
	}
	for i := range m.posts {
		if m.posts[i].InvocationID == id {
			m.posts[i].Frame = cloneFrame(frame)
			m.posts[i].Overlay = cloneValueObject(overlay)
			m.posts[i].Supplied = suppliedNames
			m.posts[i].Possible = m.posts[i].Possible || possible
			return
		}
	}
	m.posts = append(m.posts, registeredPost{InvocationID: id, Lock: lock, Condition: condition, Frame: cloneFrame(frame), Overlay: cloneValueObject(overlay), Supplied: suppliedNames, Possible: possible})
	if m.postAdded != nil {
		m.postAdded()
	}
}

func (m *ActionMachine[S]) updatePostFrame(id string, frame Frame) {
	for i := range m.posts {
		if m.posts[i].InvocationID == id {
			m.posts[i].Frame = cloneFrame(frame)
			return
		}
	}
}

func (m *ActionMachine[S]) updateNestedPostSteps(id string, steps map[string]StepFrame) {
	prefix := id + "/"
	for i := range m.posts {
		remainder := strings.TrimPrefix(m.posts[i].InvocationID, prefix)
		if remainder != m.posts[i].InvocationID && !strings.Contains(remainder, "/") {
			m.posts[i].Frame.Steps = cloneStepFrames(steps)
		}
	}
}

func (m *ActionMachine[S]) evaluateCondition(ctx context.Context, site Site, frame Frame) (GuardDecision, error) {
	if site.Surface == "" {
		site.Surface = SurfaceStepCondition
	}
	if site.Source == "" {
		if site.Surface == SurfaceActionLifecycle {
			return GuardTrue, nil
		}
		success := frame.JobStatus == 0 || frame.JobStatus&OutcomeSuccess != 0
		unsuccessful := frame.JobStatus&(OutcomeFailure|OutcomeCancelled) != 0
		switch {
		case success && unsuccessful:
			return GuardUnknown, nil
		case success:
			return GuardTrue, nil
		default:
			return GuardFalse, nil
		}
	}
	site.Result = ResultBoolean
	analysis, err := m.evaluate(ctx, site, frame)
	if err != nil {
		return GuardFalse, err
	}
	return guardDecision(analysis)
}

func lifecycleConditionSite(site Site) Site {
	if site.Surface == "" {
		site.Surface = SurfaceActionLifecycle
	}
	site.Result = ResultBoolean
	return site
}

func (m *ActionMachine[S]) evaluateBindings(ctx context.Context, bindings []Binding, frame Frame) (ValueObject, error) {
	result := ValueObject{Fields: map[string]expression.AbstractValue{}}
	for _, binding := range bindings {
		analysis, err := m.evaluate(ctx, binding.Value, frame)
		if err != nil {
			return ValueObject{}, fmt.Errorf("evaluate %q: %w", binding.Name, err)
		}
		result.Fields[binding.Name] = analysis.Value
	}
	return result, nil
}

func (m *ActionMachine[S]) enterEnvironment(ctx context.Context, bindings []Binding, parent Frame) (ValueObject, Frame, error) {
	frame := cloneFrame(parent)
	environment, err := m.evaluateBindings(ctx, bindings, frame)
	if err != nil {
		return ValueObject{}, Frame{}, fmt.Errorf("action environment: %w", err)
	}
	overlay := overlayEnvironment(parent.StableEnvironment, environment)
	frame.StableEnvironment = overlay
	frame.Environment = overlayEnvironment(parent.Environment, environment)
	for _, binding := range bindings {
		if strings.EqualFold(binding.Name, "PATH") {
			frame.ExplicitPATH = true
			break
		}
	}
	return overlay, frame, nil
}

func (m *ActionMachine[S]) resolveInputs(ctx context.Context, bindings []Binding, action Action, frame Frame) (ValueObject, error) {
	inputs, err := m.evaluateBindings(ctx, bindings, frame)
	if err != nil {
		return ValueObject{}, fmt.Errorf("action inputs: %w", err)
	}
	inputs = normalizeValueObject(inputs)
	for _, definition := range action.Inputs {
		name := strings.ToLower(definition.Name)
		if _, exists := inputs.Fields[name]; exists {
			continue
		}
		if definition.Default == nil {
			continue
		}
		frame.ActionInputs = cloneValueObject(inputs)
		analysis, err := m.evaluate(ctx, *definition.Default, frame)
		if err != nil {
			return ValueObject{}, fmt.Errorf("action input %q default: %w", definition.Name, err)
		}
		inputs.Fields[name] = analysis.Value
	}
	for _, definition := range action.Inputs {
		name := strings.ToLower(definition.Name)
		if _, exists := inputs.Fields[name]; definition.Required && !exists {
			return ValueObject{}, &hardProgramError{err: fmt.Errorf("required action input %q is missing", definition.Name)}
		}
	}
	return inputs, nil
}

func (m *ActionMachine[S]) invokeJavaScript(ctx context.Context, invocation ActionInvocation, action Action, parent, frame Frame, overlay ValueObject) (Frame, Execution, error) {
	if action.JavaScript == nil {
		return Frame{}, Execution{}, fmt.Errorf("action program %q has no JavaScript lifecycle", invocation.Lock)
	}
	if prepared, ok := m.prepared[invocation.ID]; ok {
		frame.State = cloneValueObject(prepared.State)
	}
	execution, err := m.execute(ctx, LeafOperation{
		Kind: LeafJavaScript, Phase: PhaseMain, Lock: invocation.Lock, InvocationID: invocation.ID,
		Entrypoint: action.JavaScript.Main, NodeMajor: action.JavaScript.NodeMajor,
		Inputs: frame.ActionInputs, Environment: frame.Environment,
	}, frame)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	if action.JavaScript.Post != "" {
		m.registerPost(invocation.ID, invocation.Lock, action.JavaScript.PostCondition, frame, overlay, invocation.Inputs, false)
	}
	frame = applyExecution(frame, execution)
	frame.State = overlayValueObject(frame.State, execution.State)
	m.updatePostFrame(invocation.ID, frame)
	if action.JavaScript.Post != "" {
		m.registerPost(invocation.ID, invocation.Lock, action.JavaScript.PostCondition, frame, overlay, invocation.Inputs, false)
	}
	delete(m.prepared, invocation.ID)
	return applyExecution(parent, execution), execution, nil
}

func (m *ActionMachine[S]) invokeDocker(ctx context.Context, invocation ActionInvocation, action Action, parent, frame Frame) (Frame, Execution, error) {
	if action.Docker == nil {
		return Frame{}, Execution{}, fmt.Errorf("action program %q has no Docker execution", invocation.Lock)
	}
	environment, err := m.evaluateBindings(ctx, action.Docker.Env, frame)
	if err != nil {
		execution := failureExecution(fmt.Errorf("docker environment: %w", err))
		return applyExecution(parent, execution), execution, nil
	}
	arguments := make([]expression.AbstractValue, 0, len(action.Docker.Arguments))
	for i, site := range action.Docker.Arguments {
		analysis, err := m.evaluate(ctx, site, frame)
		if err != nil {
			execution := failureExecution(fmt.Errorf("docker argument %d: %w", i+1, err))
			return applyExecution(parent, execution), execution, nil
		}
		arguments = append(arguments, analysis.Value)
	}
	execution, err := m.execute(ctx, LeafOperation{
		Kind: LeafDocker, Phase: PhaseMain, Lock: invocation.Lock, InvocationID: invocation.ID,
		Arguments: arguments, Inputs: frame.ActionInputs, Environment: frame.Environment,
		ActionEnvironment: environment, ExplicitPATH: frame.ExplicitPATH,
	}, frame)
	if err != nil {
		return Frame{}, Execution{}, err
	}
	return applyExecution(parent, execution), execution, nil
}

func (m *ActionMachine[S]) invokeComposite(ctx context.Context, invocation ActionInvocation, action Action, parent, frame Frame) (Frame, Execution, error) {
	if action.Composite == nil {
		return Frame{}, Execution{}, fmt.Errorf("action program %q has no composite execution", invocation.Lock)
	}
	frame.Steps = map[string]StepFrame{}
	result := successfulExecution()
	for i, step := range action.Composite.Steps {
		stepFrame := cloneFrame(frame)
		condition, err := m.evaluateCondition(ctx, step.Condition, stepFrame)
		var execution Execution
		if err != nil {
			execution = Execution{Outcomes: OutcomeFailure, Failure: &ExecutionFailure{Cause: fmt.Errorf("composite action step %d condition: %w", i+1, err)}}
		}
		if condition == GuardFalse {
			if err != nil {
				// An invalid condition is a failed composite step, not a skipped one.
				goto complete
			}
			if step.ID != "" {
				if frame.Steps == nil {
					frame.Steps = map[string]StepFrame{}
				}
				status := StepFrame{}
				if step.Invocation != nil {
					childID := fmt.Sprintf("%s/%d", invocation.ID, i)
					if prepared, ok := m.prepared[childID]; ok && prepared.Failure != nil {
						status.Outcome = prepared.Failure.Outcomes
						status.Conclusion = status.Outcome
						if step.ContinueOnError && status.Conclusion&OutcomeFailure != 0 {
							status.Conclusion = (status.Conclusion &^ OutcomeFailure) | OutcomeSuccess
						}
					}
				}
				frame.Steps[strings.ToLower(step.ID)] = status
			}
			continue
		}

		switch {
		case step.Run != nil:
			_, stepFrame, err = m.enterEnvironment(ctx, step.Env, stepFrame)
			if err != nil {
				execution = failureExecution(fmt.Errorf("composite action step %d environment: %w", i+1, err))
				goto complete
			}
			operation, operationErr := m.runOperation(ctx, invocation, i, *step.Run, stepFrame)
			if operationErr != nil {
				execution = failureExecution(operationErr)
				goto complete
			}
			execution, err = m.executePossibly(ctx, operation, stepFrame, condition)
		case step.Invocation != nil:
			child := ActionInvocation{
				ID: fmt.Sprintf("%s/%d", invocation.ID, i), Lock: step.Invocation.Lock,
				Inputs: step.Invocation.With, Environment: step.Env,
			}
			childAction, actionErr := m.action(child.Lock)
			if actionErr != nil {
				execution = failureExecution(fmt.Errorf("composite action step %d: %w", i+1, actionErr))
				execution.Failure.Hard = true
				goto complete
			}
			if condition == GuardUnknown {
				_, execution, err = m.invokeUnknown(ctx, child, childAction, stepFrame)
			} else {
				_, execution, err = m.invoke(ctx, child, childAction, stepFrame)
			}
		default:
			execution = successfulExecution()
		}
		if err != nil {
			execution = failureExecution(fmt.Errorf("composite action step %d: %w", i+1, err))
		}
	complete:
		frame = applyExecution(frame, execution)
		outcome := execution.Outcomes
		conclusion := outcome
		if step.ContinueOnError && conclusion&OutcomeFailure != 0 && (execution.Failure == nil || !execution.Failure.Hard) {
			conclusion = (conclusion &^ OutcomeFailure) | OutcomeSuccess
			execution.Failure = nil
		}
		if step.ID != "" {
			if frame.Steps == nil {
				frame.Steps = map[string]StepFrame{}
			}
			frame.Steps[strings.ToLower(step.ID)] = StepFrame{Outputs: execution.Outputs, Outcome: outcome, Conclusion: conclusion}
		}
		execution.Outcomes = conclusion
		frame.JobStatus = advanceJobStatus(frame.JobStatus, conclusion)
		result = accumulateExecutions(result, execution)
	}
	outputs, err := m.evaluateBindings(ctx, action.Outputs, frame)
	if err != nil {
		result = accumulateExecutions(result, failureExecution(fmt.Errorf("composite action outputs: %w", err)))
	} else {
		result.Outputs = outputs
	}
	m.stepScopes[invocation.ID] = cloneStepFrames(frame.Steps)
	m.updateNestedPostSteps(invocation.ID, frame.Steps)
	return applyExecution(parent, result), result, nil
}

func advanceJobStatus(current, conclusion OutcomeSet) OutcomeSet {
	status := current & (OutcomeFailure | OutcomeCancelled)
	if current == 0 || current&OutcomeSuccess != 0 {
		status |= conclusion
	}
	return status
}

func (m *ActionMachine[S]) runOperation(ctx context.Context, invocation ActionInvocation, index int, run Run, frame Frame) (LeafOperation, error) {
	values := make([]expression.AbstractValue, 3)
	for i, site := range []Site{run.Command, run.Shell, run.WorkingDirectory} {
		if site.Source == "" {
			values[i] = expression.AbstractValue{Known: true, Value: ""}
			continue
		}
		analysis, err := m.evaluate(ctx, site, frame)
		if err != nil {
			return LeafOperation{}, fmt.Errorf("composite action step %d: %w", index+1, err)
		}
		values[i] = analysis.Value
	}
	return LeafOperation{
		Kind: LeafShell, Phase: PhaseMain, Lock: invocation.Lock, InvocationID: fmt.Sprintf("%s/%d", invocation.ID, index),
		Command: values[0], Shell: values[1], WorkingDirectory: values[2],
		Inputs: frame.ActionInputs, Environment: frame.Environment,
	}, nil
}

func (m *ActionMachine[S]) executePossibly(ctx context.Context, operation LeafOperation, frame Frame, decision GuardDecision) (Execution, error) {
	if decision != GuardUnknown {
		return m.execute(ctx, operation, frame)
	}
	trueState, falseState := m.adapter.Fork(m.state)
	trueMachine := m.clone(trueState)
	execution, err := trueMachine.execute(ctx, operation, frame)
	if err != nil {
		return Execution{}, err
	}
	m.state = m.adapter.Join(trueMachine.state, falseState)
	return joinExecutions(execution, successfulExecution()), nil
}
