package program

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

type authorityMachineState struct {
	githubToken                   bool
	preparationEnvironmentMutable bool
}

type authorityAdapter struct{}

func validateActionAuthorityProgram(actions map[string]Action, lock string, active map[string]bool) error {
	action, ok := actions[lock]
	if !ok {
		return fmt.Errorf("action program %q is missing", lock)
	}
	if active[lock] {
		return fmt.Errorf("action program graph contains a cycle at %q", lock)
	}
	active[lock] = true
	defer delete(active, lock)
	if action.Runtime == ActionRuntimeNative {
		return nil
	}
	for _, input := range action.Inputs {
		if input.Default != nil {
			if err := validateActionAuthoritySite(*input.Default); err != nil {
				return fmt.Errorf("action input %q default: %w", input.Name, err)
			}
		}
	}
	if action.JavaScript != nil {
		if err := validateActionAuthoritySite(lifecycleConditionSite(action.JavaScript.PreCondition)); err != nil {
			return fmt.Errorf("action pre-if: %w", err)
		}
		if err := validateActionAuthoritySite(lifecycleConditionSite(action.JavaScript.PostCondition)); err != nil {
			return fmt.Errorf("action post-if: %w", err)
		}
	}
	if action.Docker != nil {
		for i, argument := range action.Docker.Arguments {
			if err := validateActionAuthoritySite(argument); err != nil {
				return fmt.Errorf("docker action argument %d: %w", i+1, err)
			}
		}
		for _, binding := range action.Docker.Env {
			if err := validateActionAuthoritySite(binding.Value); err != nil {
				return fmt.Errorf("docker environment %q: %w", binding.Name, err)
			}
		}
	}
	if action.Composite != nil {
		for i, step := range action.Composite.Steps {
			if err := validateActionAuthoritySite(step.Name); err != nil {
				return fmt.Errorf("composite action step %d name: %w", i+1, err)
			}
			if err := validateActionAuthoritySite(step.Condition); err != nil {
				return fmt.Errorf("composite action step %d condition: %w", i+1, err)
			}
			for _, bindings := range [][]Binding{step.Env, invocationBindings(step.Invocation)} {
				for _, binding := range bindings {
					if err := validateActionAuthoritySite(binding.Value); err != nil {
						return fmt.Errorf("composite action step %d %q: %w", i+1, binding.Name, err)
					}
				}
			}
			if step.Run != nil {
				for _, site := range []Site{step.Run.Command, step.Run.Shell, step.Run.WorkingDirectory} {
					if err := validateActionAuthoritySite(site); err != nil {
						return fmt.Errorf("composite action step %d: %w", i+1, err)
					}
				}
			}
			if step.Invocation != nil {
				if err := validateActionAuthoritySite(step.Invocation.Uses); err != nil {
					return fmt.Errorf("composite action step %d uses: %w", i+1, err)
				}
				if err := validateActionAuthorityProgram(actions, step.Invocation.Lock, active); err != nil {
					return err
				}
			}
		}
	}
	for _, output := range action.Outputs {
		if err := validateActionAuthoritySite(output.Value); err != nil {
			return fmt.Errorf("action output %q: %w", output.Name, err)
		}
	}
	return nil
}

func invocationBindings(invocation *Invocation) []Binding {
	if invocation == nil {
		return nil
	}
	return invocation.With
}

func validateActionAuthoritySite(site Site) error {
	if site.Provenance == ProvenanceAction {
		var secrets []string
		var err error
		switch site.Surface {
		case SurfaceActionLifecycle, SurfaceStepCondition:
			secrets, err = expression.ConditionSecretReferences(site.Source)
		default:
			secrets, err = expression.SecretReferences(site.Source)
		}
		if err != nil {
			return err
		}
		if len(secrets) != 0 {
			if site.Surface == SurfaceActionInputDefault {
				return fmt.Errorf("action input defaults cannot grant secret authority")
			}
			return fmt.Errorf("composite action metadata cannot grant secret authority")
		}
	}
	switch site.Surface {
	case SurfaceActionInputDefault:
		return expression.ValidateActionInputDefault(site.Source)
	case SurfaceActionLifecycle:
		return expression.ValidateActionLifecycleCondition(site.Source)
	case SurfaceStepCondition:
		return expression.ValidateCompositeActionCondition(site.Source)
	case SurfaceDockerArgument:
		return expression.ValidateDockerActionArg(site.Source)
	default:
		return expression.ValidateStepTemplate(site.Source)
	}
}

func (authorityAdapter) Evaluate(_ context.Context, state authorityMachineState, site Site, frame FrameView) (authorityMachineState, expression.Analysis, error) {
	analysis, err := analyzeAuthoritySite(site, frame)
	if err != nil {
		return state, expression.Analysis{}, err
	}
	state.githubToken = state.githubToken || authorityEffectsRequireToken(analysis.Effects)
	return state, analysis, nil
}

func (authorityAdapter) Execute(_ context.Context, state authorityMachineState, operation LeafOperation, _ FrameView) (authorityMachineState, Execution, error) {
	if operation.Phase == PhasePre {
		state.preparationEnvironmentMutable = true
	}
	execution := Execution{
		Outcomes: OutcomeSuccess | OutcomeFailure | OutcomeCancelled,
		Outputs:  ValueObject{Fields: map[string]expression.AbstractValue{}, Open: true},
	}
	if operation.Kind != LeafNative {
		execution.Environment.Unknown = true
	}
	return state, execution, nil
}

func (authorityAdapter) Fork(state authorityMachineState) (authorityMachineState, authorityMachineState) {
	return state, state
}

func (authorityAdapter) Join(left, right authorityMachineState) authorityMachineState {
	return authorityMachineState{
		githubToken:                   left.githubToken || right.githubToken,
		preparationEnvironmentMutable: left.preparationEnvironmentMutable || right.preparationEnvironmentMutable,
	}
}

func analyzeAuthoritySite(site Site, view FrameView) (expression.Analysis, error) {
	if site.Source == "" {
		value := any("")
		if site.Result == ResultBoolean {
			value = true
		}
		return expression.Analysis{Value: expression.AbstractValue{Known: true, Value: value}}, nil
	}
	known := authorityReferences(site, view)
	var analysis expression.Analysis
	var err error
	switch site.Surface {
	case SurfaceActionInputDefault:
		analysis, err = expression.AnalyzeActionInputDefault(site.Source, known)
	case SurfaceActionLifecycle:
		analysis, err = expression.AnalyzeActionLifecycleCondition(site.Source, known)
	case SurfaceStepCondition:
		if site.Provenance == ProvenanceAction {
			analysis, err = expression.AnalyzeCompositeActionCondition(site.Source, known)
		} else {
			analysis, err = expression.AnalyzeCondition(site.Source, known, expression.GitHubTokenWorkflowContext)
		}
	case SurfaceDockerArgument:
		err = expression.ValidateDockerActionArg(site.Source)
		analysis.Value = expression.AbstractValue{Known: true, Value: site.Source}
	default:
		wholeGitHub := expression.GitHubTokenWorkflowContext
		if site.Provenance == ProvenanceAction {
			wholeGitHub = expression.GitHubTokenCompositeContext
		}
		analysis, err = expression.AnalyzeStepTemplate(site.Source, known, wholeGitHub)
	}
	if err != nil {
		if site.Surface == SurfaceDockerArgument {
			return expression.Analysis{}, err
		}
		fallback, fallbackErr := conservativeAuthorityAnalysis(site, known)
		if fallbackErr == nil && !strings.Contains(strings.ToLower(err.Error()), "unsupported") {
			return fallback, nil
		}
		if site.Location.Field == "" {
			return expression.Analysis{}, err
		}
		return expression.Analysis{}, fmt.Errorf("%s: %w", site.Location.Field, err)
	}
	return analysis, nil
}

func conservativeAuthorityAnalysis(site Site, known map[string]any) (expression.Analysis, error) {
	var requiresToken bool
	var err error
	switch site.Surface {
	case SurfaceActionInputDefault:
		if err = expression.ValidateActionInputDefault(site.Source); err == nil {
			serverURL, _ := known["github.server_url"].(string)
			requiresToken, err = expression.ActionInputDefaultRequiresGitHubToken(site.Source, serverURL)
		}
	case SurfaceActionLifecycle, SurfaceStepCondition:
		switch {
		case site.Surface == SurfaceActionLifecycle:
			err = expression.ValidateActionLifecycleCondition(site.Source)
		case site.Provenance == ProvenanceAction:
			err = expression.ValidateCompositeActionCondition(site.Source)
		default:
			err = expression.ValidateCondition(site.Source, expression.StepCondition)
		}
		if err == nil {
			requiresToken, err = expression.ConditionReferencesGitHubToken(site.Source)
		}
	default:
		err = expression.ValidateStepTemplate(site.Source)
		if err == nil {
			requiresToken, err = stepTemplateReferencesGitHubToken(site.Source, site.Provenance == ProvenanceWorkflow)
		}
	}
	if err != nil {
		return expression.Analysis{}, err
	}
	analysis := expression.Analysis{}
	if requiresToken {
		analysis.Effects.GitHubToken = expression.GitHubTokenDirect
	}
	return analysis, nil
}

func authorityReferences(site Site, view FrameView) map[string]any {
	known := map[string]any{}
	inputs := view.WorkflowInputs()
	if site.Provenance == ProvenanceAction && site.Surface != SurfaceActionLifecycle {
		inputs = view.ActionInputs()
	}
	retainObjectReferences(known, "inputs", inputs)
	retainObjectReferences(known, "env", view.Environment())
	retainObjectReferences(known, "github", view.GitHub())
	retainObjectReferences(known, "matrix", view.Matrix())
	steps := view.Steps()
	if steps != nil {
		stepValues := make(map[string]any, len(steps))
		closed := true
		for name, step := range steps {
			value := map[string]any{}
			if outputs, ok := concreteObject(step.Outputs); ok {
				value["outputs"] = outputs
				retainKnownReferences(known, "steps."+strings.ToLower(name)+".outputs", outputs)
			} else {
				closed = false
			}
			if outcome, ok := abstractOutcomeName(step.Outcome); ok {
				value["outcome"] = outcome
				known["steps."+strings.ToLower(name)+".outcome"] = outcome
			} else {
				closed = false
			}
			if conclusion, ok := abstractOutcomeName(step.Conclusion); ok {
				value["conclusion"] = conclusion
				known["steps."+strings.ToLower(name)+".conclusion"] = conclusion
			} else {
				closed = false
			}
			stepValues[strings.ToLower(name)] = value
		}
		if closed {
			known["steps"] = stepValues
		}
	}
	return known
}

func abstractOutcomeName(outcome OutcomeSet) (string, bool) {
	switch outcome {
	case OutcomeSuccess:
		return "success", true
	case OutcomeFailure:
		return "failure", true
	case OutcomeCancelled:
		return "cancelled", true
	default:
		return "", false
	}
}

func retainObjectReferences(known map[string]any, root string, object ValueObject) {
	if value, ok := concreteObject(object); ok {
		known[root] = value
	}
	for name, value := range object.Fields {
		if value.Known {
			known[root+"."+strings.ToLower(name)] = value.Value
		}
	}
}

func concreteObject(object ValueObject) (map[string]any, bool) {
	if object.Open {
		return nil, false
	}
	result := make(map[string]any, len(object.Fields))
	for name, value := range object.Fields {
		if !value.Known {
			return nil, false
		}
		result[strings.ToLower(name)] = value.Value
	}
	return result, true
}

func authorityFrame(context ActionAuthorityContext, mutable bool) Frame {
	frame := Frame{
		WorkflowInputs: authorityObject(context.WorkflowInputs, context.UnknownWorkflowInputs),
		Environment:    ValueObject{Fields: map[string]expression.AbstractValue{}, Open: mutable},
		GitHub:         ValueObject{Fields: map[string]expression.AbstractValue{"server_url": {Known: true, Value: context.ServerURL}}},
		Matrix:         authorityObject(context.Matrix, nil),
	}
	if !mutable {
		for name, value := range context.Environment {
			frame.Environment.Fields[strings.ToLower(name)] = expression.AbstractValue{Known: true, Value: value}
		}
	}
	known := authorityReferences(Site{Provenance: ProvenanceWorkflow}, frame.view())
	layers := context.EnvironmentLayers
	if mutable && len(layers) != 0 {
		layers = layers[len(layers)-1:]
	}
	for _, layer := range layers {
		for _, binding := range layer {
			analysis, err := expression.AnalyzeStepTemplate(binding.Value.Source, known, expression.GitHubTokenWorkflowContext)
			if err != nil {
				frame.Environment.Fields[strings.ToLower(binding.Name)] = expression.AbstractValue{}
				continue
			}
			frame.Environment.Fields[strings.ToLower(binding.Name)] = analysis.Value
		}
		known = authorityReferences(Site{Provenance: ProvenanceWorkflow}, frame.view())
	}
	if len(layers) != 0 {
		frame.StableEnvironment = ValueObject{Fields: map[string]expression.AbstractValue{}}
		for _, binding := range layers[len(layers)-1] {
			frame.StableEnvironment.Fields[strings.ToLower(binding.Name)] = frame.Environment.Fields[strings.ToLower(binding.Name)]
		}
	}
	return frame
}

func authorityObject(values map[string]any, unknown map[string]bool) ValueObject {
	object := ValueObject{Fields: map[string]expression.AbstractValue{}, Open: values == nil || len(unknown) != 0}
	for name, value := range values {
		if unknown[strings.ToLower(name)] {
			object.Fields[strings.ToLower(name)] = expression.AbstractValue{}
			continue
		}
		object.Fields[strings.ToLower(name)] = expression.AbstractValue{Known: true, Value: normalizeKnownValue(value)}
	}
	return object
}

func inventoryInvocationSecrets(actions map[string]Action, invocation ActionInvocation) ([]string, error) {
	action, ok := actions[invocation.Lock]
	if !ok {
		return nil, fmt.Errorf("action program %q is missing", invocation.Lock)
	}
	definitions := make(map[string]ActionInput, len(action.Inputs))
	for _, input := range action.Inputs {
		definitions[strings.ToLower(input.Name)] = input
	}
	found := map[string]struct{}{}
	for _, binding := range invocation.Inputs {
		if referencesEvent, err := expression.TemplateReferencesGitHubEvent(binding.Value.Source); err != nil {
			return nil, err
		} else if referencesEvent {
			return nil, fmt.Errorf("action input %q: github.event cannot be retained in a job plan", binding.Name)
		}
		names, err := expression.SecretReferences(binding.Value.Source)
		if err != nil {
			return nil, err
		}
		definition := definitions[strings.ToLower(binding.Name)]
		for _, name := range names {
			if definition.Name == "" || definition.Required || name == "GITHUB_TOKEN" {
				found[name] = struct{}{}
			}
		}
	}
	secrets := make([]string, 0, len(found))
	for name := range found {
		secrets = append(secrets, name)
	}
	sort.Strings(secrets)
	return secrets, nil
}
