package program

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

type traceState struct{ events []string }

type traceAdapter struct {
	executions       map[string]Execution
	evaluationErrors map[string]error
	executionErrors  map[string]error
}

func (a *traceAdapter) Evaluate(_ context.Context, state traceState, site Site, frame FrameView) (traceState, expression.Analysis, error) {
	state.events = append(state.events, "eval:"+site.Source)
	if err := a.evaluationErrors[site.Source]; err != nil {
		return state, expression.Analysis{}, err
	}
	var value expression.AbstractValue
	switch site.Source {
	case "true":
		value = known(true)
	case "false":
		value = known(false)
	case "unknown":
		value = expression.AbstractValue{}
	default:
		switch {
		case strings.HasPrefix(site.Source, "step-success:"):
			step := frame.Steps()[strings.TrimPrefix(site.Source, "step-success:")]
			value = known(step.Conclusion == OutcomeSuccess)
		case strings.HasPrefix(site.Source, "env:"):
			value = frame.Environment().Fields[site.Source[4:]]
		default:
			value = known(site.Source)
		}
	}
	return state, expression.Analysis{Value: value}, nil
}

func (a *traceAdapter) Execute(_ context.Context, state traceState, operation LeafOperation, _ FrameView) (traceState, Execution, error) {
	state.events = append(state.events, fmt.Sprintf("exec:%s:%s env=%s/%s input=%s", operation.InvocationID, operation.Entrypoint,
		valueString(operation.Environment.Fields["value"]), valueString(operation.Environment.Fields["live"]), valueString(operation.Inputs.Fields["value"])))
	if err := a.executionErrors[operation.Entrypoint]; err != nil {
		return state, Execution{}, err
	}
	if execution, ok := a.executions[operation.Entrypoint]; ok {
		return state, execution, nil
	}
	return state, successfulExecution(), nil
}

func (*traceAdapter) Fork(state traceState) (traceState, traceState) {
	return cloneTrace(state), cloneTrace(state)
}

func (*traceAdapter) Join(left, right traceState) traceState {
	result := cloneTrace(right)
	for _, event := range left.events {
		if !contains(result.events, event) {
			result.events = append(result.events, event)
		}
	}
	return result
}

func cloneTrace(state traceState) traceState {
	return traceState{events: append([]string(nil), state.events...)}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func known(value any) expression.AbstractValue {
	return expression.AbstractValue{Known: true, Value: value}
}

func valueString(value expression.AbstractValue) string {
	if !value.Known {
		return "?"
	}
	return fmt.Sprint(value.Value)
}

func site(source string) Site { return Site{Source: source} }

func binding(name, source string) Binding { return Binding{Name: name, Value: site(source)} }

func js(pre, main, post string) Action {
	return Action{Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{Pre: pre, Main: main, Post: post}}
}

func invocation(id, lock string) ActionInvocation { return ActionInvocation{ID: id, Lock: lock} }

func requireTrace(t *testing.T, machine *ActionMachine[traceState], want ...string) {
	t.Helper()
	if got := machine.State().events; !reflect.DeepEqual(got, want) {
		t.Fatalf("trace mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestActionMachinePreparesNestedJavaScriptDepthFirstInSourceOrder(t *testing.T) {
	actions := map[string]Action{
		"root": {Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{Condition: site("false"), Invocation: &Invocation{Lock: "nested"}},
			{Invocation: &Invocation{Lock: "last"}},
		}}},
		"nested": {Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{Invocation: &Invocation{Lock: "first"}}, {Invocation: &Invocation{Lock: "second"}},
		}}},
		"first":  js("pre-first", "main", ""),
		"second": js("pre-second", "main", ""),
		"last":   js("pre-last", "main", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{}, traceState{})
	if _, _, err := machine.Prepare(t.Context(), invocation("root", "root"), Frame{}); err != nil {
		t.Fatal(err)
	}
	requireTrace(t, machine,
		"exec:root/0/0:pre-first env=?/? input=?",
		"exec:root/0/1:pre-second env=?/? input=?",
		"exec:root/1:pre-last env=?/? input=?",
	)
}

func TestActionMachineFalsePreGuardRemainsSkippedDuringInvocation(t *testing.T) {
	action := js("pre", "main", "post")
	action.JavaScript.PreCondition = site("env:run")
	machine := NewActionMachine(map[string]Action{"action": action}, &traceAdapter{}, traceState{})
	inv := invocation("action", "action")
	frame, _, err := machine.Prepare(t.Context(), inv, Frame{Environment: ValueObject{Fields: map[string]expression.AbstractValue{"run": known(false)}}})
	if err != nil {
		t.Fatal(err)
	}
	frame.Environment.Fields["run"] = known(true)
	frame, _, err = machine.Invoke(t.Context(), inv, frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = machine.Finish(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	requireTrace(t, machine, "eval:env:run", "exec:action:main env=?/? input=?", "exec:action:post env=?/? input=?")
}

func ptrSite(value Site) *Site { return &value }

func TestActionMachineRegistersNoPrePostBeforeMainAndFinishesLIFO(t *testing.T) {
	actions := map[string]Action{"first": js("", "main-first", "post-first"), "second": js("", "main-second", "post-second")}
	machine := NewActionMachine(actions, &traceAdapter{}, traceState{})
	frame := Frame{}
	for _, inv := range []ActionInvocation{invocation("first", "first"), invocation("second", "second")} {
		var err error
		frame, _, err = machine.Invoke(t.Context(), inv, frame)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := machine.Finish(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	requireTrace(t, machine,
		"exec:first:main-first env=?/? input=?", "exec:second:main-second env=?/? input=?",
		"exec:second:post-second env=?/? input=?", "exec:first:post-first env=?/? input=?",
	)
}

func TestActionMachineEnvironmentAndBindingsFollowLifecycleTime(t *testing.T) {
	action := js("pre", "main", "post")
	action.Inputs = []ActionInput{{Name: "value", Default: ptrSite(site("env:value"))}}
	adapter := &traceAdapter{executions: map[string]Execution{
		"pre":  {Outcomes: OutcomeSuccess, Environment: EnvironmentMutation{Sets: map[string]expression.AbstractValue{"live": known("from-pre")}}},
		"main": {Outcomes: OutcomeSuccess, Environment: EnvironmentMutation{Sets: map[string]expression.AbstractValue{"live": known("from-main")}}},
	}}
	machine := NewActionMachine(map[string]Action{"action": action}, adapter, traceState{})
	inv := invocation("action", "action")
	inv.Environment = []Binding{binding("value", "overlay")}
	frame, _, err := machine.Prepare(t.Context(), inv, Frame{Environment: ValueObject{Fields: map[string]expression.AbstractValue{"value": known("live")}}})
	if err != nil {
		t.Fatal(err)
	}
	frame, _, err = machine.Invoke(t.Context(), inv, frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = machine.Finish(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	requireTrace(t, machine,
		"eval:overlay", "eval:env:value", "exec:action:pre env=overlay/? input=live",
		"eval:env:value", "eval:overlay", "exec:action:main env=overlay/from-pre input=live",
		"exec:action:post env=overlay/from-main input=live",
	)
	if got := valueString(frame.Environment.Fields["value"]); got != "live" {
		t.Fatalf("invocation overlay leaked: env value = %q", got)
	}
}

func TestActionMachineFailedPreSuppressesMainAndRetainsPost(t *testing.T) {
	adapter := &traceAdapter{executions: map[string]Execution{"pre": {
		Outcomes: OutcomeFailure,
		Outputs:  ValueObject{Fields: map[string]expression.AbstractValue{"partial": known("value")}, Open: true},
	}}}
	machine := NewActionMachine(map[string]Action{"action": js("pre", "main", "post")}, adapter, traceState{})
	frame, _, err := machine.Prepare(t.Context(), invocation("action", "action"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	frame, execution, err := machine.Invoke(t.Context(), invocation("action", "action"), frame)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcomes != OutcomeFailure {
		t.Fatalf("invoke outcome = %v, want failure", execution.Outcomes)
	}
	if !execution.Outputs.Open || len(execution.Outputs.Fields) != 0 {
		t.Fatalf("invoke outputs = %#v, want only preserved uncertainty", execution.Outputs)
	}
	if _, _, err = machine.Finish(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	requireTrace(t, machine, "exec:action:pre env=?/? input=?", "exec:action:post env=?/? input=?")
}

func TestActionMachineRejectsMissingRequiredInput(t *testing.T) {
	action := js("", "main", "")
	action.Inputs = []ActionInput{{Name: "token", Required: true}}
	machine := NewActionMachine(map[string]Action{"action": action}, &traceAdapter{}, traceState{})
	_, execution, err := machine.Invoke(t.Context(), invocation("action", "action"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Failure == nil || !execution.Failure.Hard || !strings.Contains(execution.Failure.Cause.Error(), `required action input "token" is missing`) {
		t.Fatalf("Invoke() execution = %#v, want hard missing required input failure", execution)
	}
}

func TestActionMachineCompositeImplicitSuccessSkipsAfterFailure(t *testing.T) {
	failure := Execution{Outcomes: OutcomeFailure, Failure: &ExecutionFailure{Cause: fmt.Errorf("failed")}}
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{Run: &Run{Command: site("fail")}},
			{Invocation: &Invocation{Lock: "skipped"}},
			{Condition: site("true"), Invocation: &Invocation{Lock: "always"}},
		}}},
		"skipped": js("", "skipped", ""),
		"always":  js("", "always", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{executions: map[string]Execution{"": failure}}, traceState{})
	_, execution, err := machine.Invoke(t.Context(), invocation("root", "root"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcomes != OutcomeFailure {
		t.Fatalf("composite outcome = %v, want preserved failure", execution.Outcomes)
	}
	for _, event := range machine.State().events {
		if strings.Contains(event, ":skipped ") {
			t.Fatalf("implicit success step ran after failure: %q", machine.State().events)
		}
	}
	if !slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":always ") }) {
		t.Fatalf("explicitly guarded step did not run: %q", machine.State().events)
	}
}

func TestActionMachineCompositeContinueOnErrorAllowsLaterSteps(t *testing.T) {
	failure := Execution{Outcomes: OutcomeFailure, Failure: &ExecutionFailure{Cause: fmt.Errorf("failed")}}
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{ContinueOnError: true, Run: &Run{Command: site("fail")}},
			{Invocation: &Invocation{Lock: "after"}},
		}}},
		"after": js("", "after", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{executions: map[string]Execution{"": failure}}, traceState{})
	_, execution, err := machine.Invoke(t.Context(), invocation("root", "root"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcomes != OutcomeSuccess {
		t.Fatalf("composite outcome = %v, want success", execution.Outcomes)
	}
	if execution.Failure != nil {
		t.Fatalf("composite retained tolerated failure: %v", execution.Failure)
	}
	if !slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":after ") }) {
		t.Fatalf("step after continued failure did not run: %q", machine.State().events)
	}
}

func TestActionMachineCompositeContinueOnErrorDoesNotTolerateHardFailure(t *testing.T) {
	failure := Execution{Outcomes: OutcomeFailure, Failure: &ExecutionFailure{Cause: fmt.Errorf("failed"), Hard: true}}
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{ContinueOnError: true, Run: &Run{Command: site("fail")}},
			{Invocation: &Invocation{Lock: "after"}},
		}}},
		"after": js("", "after", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{executions: map[string]Execution{"": failure}}, traceState{})
	_, execution, err := machine.Invoke(t.Context(), invocation("root", "root"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcomes != OutcomeFailure || execution.Failure == nil || !execution.Failure.Hard {
		t.Fatalf("composite execution = %#v, want hard failure", execution)
	}
	if slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":after ") }) {
		t.Fatalf("step after hard failure ran: %q", machine.State().events)
	}
}

func TestActionMachineCompositeContinueOnErrorHandlesEvaluationFailure(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{ContinueOnError: true, Env: []Binding{binding("value", "invalid")}, Run: &Run{Command: site("never")}},
			{Invocation: &Invocation{Lock: "after"}},
		}}},
		"after": js("", "after", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{evaluationErrors: map[string]error{"invalid": fmt.Errorf("invalid expression value")}}, traceState{})
	_, execution, err := machine.Invoke(t.Context(), invocation("root", "root"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcomes != OutcomeSuccess || execution.Failure != nil {
		t.Fatalf("composite execution = %#v, want tolerated failure", execution)
	}
	if !slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":after ") }) {
		t.Fatalf("step after tolerated evaluation failure did not run: %q", machine.State().events)
	}
}

func TestActionMachineCompositeContinueOnErrorDoesNotTolerateAdapterFailure(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{ContinueOnError: true, Run: &Run{Command: site("fail")}},
			{Invocation: &Invocation{Lock: "after"}},
		}}},
		"after": js("", "after", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{executionErrors: map[string]error{"": fmt.Errorf("adapter setup failed")}}, traceState{})
	_, execution, err := machine.Invoke(t.Context(), invocation("root", "root"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Failure == nil || !execution.Failure.Hard {
		t.Fatalf("composite execution = %#v, want hard adapter failure", execution)
	}
	if slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":after ") }) {
		t.Fatalf("step after adapter failure ran: %q", machine.State().events)
	}
}

func TestActionMachineNestedPreparationPreservesHardAdapterFailure(t *testing.T) {
	actions := map[string]Action{
		"root": {Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{ContinueOnError: true, Invocation: &Invocation{Lock: "nested"}},
		}}},
		"nested": {Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{Invocation: &Invocation{Lock: "child"}},
		}}},
		"child": js("pre", "main", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{executionErrors: map[string]error{"pre": fmt.Errorf("adapter setup failed")}}, traceState{})
	_, execution, err := machine.Prepare(t.Context(), invocation("root", "root"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Failure == nil || !execution.Failure.Hard {
		t.Fatalf("Prepare() execution = %#v, want hard nested adapter failure", execution)
	}
}

func TestActionMachineInvocationExpressionFailuresAreSoft(t *testing.T) {
	tests := map[string]Action{
		"javascript pre-if": {
			Runtime:    ActionRuntimeJavaScript,
			JavaScript: &JavaScriptAction{Pre: "pre", PreCondition: site("invalid"), Main: "main"},
		},
		"docker environment": {
			Runtime: ActionRuntimeDocker,
			Docker:  &DockerAction{Env: []Binding{binding("value", "invalid")}},
		},
		"docker argument": {
			Runtime: ActionRuntimeDocker,
			Docker:  &DockerAction{Arguments: []Site{site("invalid")}},
		},
		"composite output": {
			Runtime:   ActionRuntimeComposite,
			Composite: &CompositeAction{},
			Outputs:   []Binding{binding("value", "invalid")},
		},
	}
	for name, action := range tests {
		t.Run(name, func(t *testing.T) {
			machine := NewActionMachine(map[string]Action{"action": action}, &traceAdapter{evaluationErrors: map[string]error{"invalid": fmt.Errorf("invalid expression")}}, traceState{})
			_, execution, err := machine.Invoke(t.Context(), invocation("action", "action"), Frame{})
			if err != nil {
				t.Fatalf("Invoke() error = %v, want failed execution", err)
			}
			if execution.Failure == nil || execution.Failure.Hard {
				t.Fatalf("Invoke() execution = %#v, want soft expression failure", execution)
			}
		})
	}
}

func TestActionMachinePostConditionFailureDoesNotSkipOlderPost(t *testing.T) {
	newer := js("", "newer-main", "newer-post")
	newer.JavaScript.PostCondition = site("invalid")
	actions := map[string]Action{"older": js("", "older-main", "older-post"), "newer": newer}
	machine := NewActionMachine(actions, &traceAdapter{evaluationErrors: map[string]error{"invalid": fmt.Errorf("invalid post condition")}}, traceState{})
	frame := Frame{}
	for _, inv := range []ActionInvocation{invocation("older", "older"), invocation("newer", "newer")} {
		var err error
		frame, _, err = machine.Invoke(t.Context(), inv, frame)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, execution, err := machine.Finish(t.Context(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcomes&OutcomeFailure == 0 {
		t.Fatalf("finish outcome = %v, want failed post", execution.Outcomes)
	}
	if !slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":older-post ") }) {
		t.Fatalf("older post did not run after newer condition failed: %q", machine.State().events)
	}
}

func TestActionMachineRegistersPostOnlyAfterAdapterStartsPhase(t *testing.T) {
	t.Run("adapter setup failure", func(t *testing.T) {
		action := js("pre", "main", "post")
		machine := NewActionMachine(map[string]Action{"action": action}, &traceAdapter{executionErrors: map[string]error{"pre": fmt.Errorf("adapter setup failed")}}, traceState{})
		registrations := 0
		machine.SetPostRegistrationCallback(func() { registrations++ })
		if _, _, err := machine.Prepare(t.Context(), invocation("action", "action"), Frame{}); err == nil {
			t.Fatal("Prepare() succeeded")
		}
		if registrations != 0 {
			t.Fatalf("post registrations = %d, want 0", registrations)
		}
	})
	t.Run("pre and main share one registration", func(t *testing.T) {
		action := js("pre", "main", "post")
		machine := NewActionMachine(map[string]Action{"action": action}, &traceAdapter{}, traceState{})
		registrations := 0
		machine.SetPostRegistrationCallback(func() { registrations++ })
		frame, _, err := machine.Prepare(t.Context(), invocation("action", "action"), Frame{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = machine.Invoke(t.Context(), invocation("action", "action"), frame); err != nil {
			t.Fatal(err)
		}
		if registrations != 1 {
			t.Fatalf("post registrations = %d, want 1", registrations)
		}
	})
}

func TestActionMachineNestedPostSeesParentCompositeStepScope(t *testing.T) {
	child := js("", "main", "post")
	child.JavaScript.PostCondition = site("step-success:child")
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{
			{ID: "child", Invocation: &Invocation{Lock: "child"}},
		}}},
		"child": child,
	}
	machine := NewActionMachine(actions, &traceAdapter{}, traceState{})
	frame, _, err := machine.Invoke(t.Context(), invocation("root", "root"), Frame{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = machine.Finish(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":post ") }) {
		t.Fatalf("nested post did not observe its parent step scope: %q", machine.State().events)
	}
}

func TestActionMachineCompositeDoesNotInheritWorkflowStepScope(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{{
			Condition: site("step-success:workflow"), Invocation: &Invocation{Lock: "child"},
		}}}},
		"child": js("", "child", ""),
	}
	machine := NewActionMachine(actions, &traceAdapter{}, traceState{})
	frame := Frame{Steps: map[string]StepFrame{"workflow": {Outcome: OutcomeSuccess, Conclusion: OutcomeSuccess}}}
	if _, _, err := machine.Invoke(t.Context(), invocation("root", "root"), frame); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(machine.State().events, func(event string) bool { return strings.Contains(event, ":child ") }) {
		t.Fatalf("workflow step scope leaked into composite: %q", machine.State().events)
	}
}

func TestActionMachineUnknownGuardsJoinPossibleEffects(t *testing.T) {
	tests := []struct {
		name      string
		prepare   bool
		condition func(*Action, *ActionInvocation)
		want      []string
	}{
		{name: "pre", prepare: true, condition: func(action *Action, _ *ActionInvocation) { action.JavaScript.PreCondition = site("unknown") }, want: []string{"eval:unknown", "exec:action:pre env=?/? input=?", "exec:action:post env=?/? input=?"}},
		{name: "invocation", condition: func(_ *Action, inv *ActionInvocation) { inv.Condition = site("unknown") }, want: []string{"eval:unknown", "exec:action:main env=?/? input=?", "exec:action:post env=?/? input=?"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action := js("", "main", "post")
			if test.prepare {
				action.JavaScript.Pre = "pre"
			}
			inv := invocation("action", "action")
			test.condition(&action, &inv)
			machine := NewActionMachine(map[string]Action{"action": action}, &traceAdapter{}, traceState{})
			frame := Frame{}
			var err error
			if test.prepare {
				frame, _, err = machine.Prepare(t.Context(), inv, frame)
			} else {
				frame, _, err = machine.Invoke(t.Context(), inv, frame)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = machine.Finish(t.Context(), frame); err != nil {
				t.Fatal(err)
			}
			requireTrace(t, machine, test.want...)
		})
	}
}
