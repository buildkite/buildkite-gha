package program

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

type ActionAuthorityContext struct {
	ServerURL                     string
	WorkflowInputs                map[string]any
	UnknownWorkflowInputs         map[string]bool
	Matrix                        map[string]any
	Environment                   map[string]string
	EnvironmentLayers             [][]Binding
	MainEnvironmentMutable        bool
	PreparationEnvironmentMutable bool
}

type ActionInvocation struct {
	Lock      string
	Inputs    []Binding
	Condition Site
}

// InventoryActionAuthority abstractly executes normalized action lifecycle
// structure and returns the workflow authority that reachable phases require.
func InventoryActionAuthority(actions map[string]Action, invocation ActionInvocation, context ActionAuthorityContext) (Authority, error) {
	planner := actionAuthorityPlanner{actions: actions, secrets: map[string]struct{}{}, active: map[string]bool{}}
	known, err := workflowKnownReferences(context)
	if err != nil {
		return Authority{}, err
	}
	mainKnown, err := actionPhaseKnownReferences(context, context.MainEnvironmentMutable)
	if err != nil {
		return Authority{}, err
	}
	preparationKnown, err := actionPhaseKnownReferences(context, context.PreparationEnvironmentMutable)
	if err != nil {
		return Authority{}, err
	}
	stableKnown, err := actionPhaseKnownReferences(context, true)
	if err != nil {
		return Authority{}, err
	}
	planner.workflowInputScope = known
	mainReachable := true
	if invocation.Condition.Source != "" {
		analysis, err := expression.AnalyzeCondition(invocation.Condition.Source, mainKnown, expression.GitHubTokenWorkflowContext)
		if err != nil {
			if validationErr := expression.ValidateCondition(invocation.Condition.Source, expression.StepCondition); validationErr != nil {
				return Authority{}, err
			}
			requiresToken, fallbackErr := expression.ConditionReferencesGitHubToken(invocation.Condition.Source)
			if fallbackErr != nil {
				return Authority{}, err
			}
			planner.githubToken = planner.githubToken || requiresToken
		} else if analysis.Value.Known {
			mainReachable, _ = analysis.Value.Value.(bool)
		}
	}
	if err := planner.inspect(invocation.Lock, invocation.Inputs, invocation.Inputs, mainKnown, preparationKnown, stableKnown, mainReachable, true, false, true); err != nil {
		return Authority{}, err
	}
	authority := Authority{
		GitHubToken: planner.githubToken, PreparationEnvironmentMutable: planner.preparationEnvironmentMutable,
		Secrets: make([]string, 0, len(planner.secrets)),
	}
	for name := range planner.secrets {
		authority.Secrets = append(authority.Secrets, name)
	}
	sort.Strings(authority.Secrets)
	return authority, nil
}

type actionAuthorityPlanner struct {
	actions                       map[string]Action
	secrets                       map[string]struct{}
	githubToken                   bool
	active                        map[string]bool
	workflowInputScope            map[string]any
	preparationEnvironmentMutable bool
}

type effectiveActionInputs struct {
	values      map[string]any
	unknown     map[string]bool
	omitted     map[string]bool
	githubToken bool
}

func (p *actionAuthorityPlanner) inspect(lock string, mainSupplied, preparationSupplied []Binding, mainScope, preparationScope, stableScope map[string]any, mainReachable, preparationReachable, preparationEnvironmentToken, workflowAuthored bool) error {
	action, ok := p.actions[lock]
	if !ok {
		return fmt.Errorf("action program %q is missing", lock)
	}
	if p.active[lock] {
		return fmt.Errorf("action program graph contains a cycle at %q", lock)
	}
	p.active[lock] = true
	defer delete(p.active, lock)
	if action.Runtime != ActionRuntimeNative {
		for _, input := range action.Inputs {
			if input.Default == nil {
				continue
			}
			if err := expression.ValidateActionInputDefault(input.Default.Source); err != nil {
				return fmt.Errorf("action input %q default: %w", input.Name, err)
			}
		}
	}

	resolveDefaults := action.Runtime != ActionRuntimeNative
	mainInputs, err := p.resolveInputs(action, mainSupplied, mainScope, workflowAuthored, resolveDefaults)
	if err != nil {
		return err
	}
	preparationInputs, err := p.resolveInputs(action, preparationSupplied, preparationScope, workflowAuthored, resolveDefaults)
	if err != nil {
		return err
	}
	if mainReachable && mainInputs.githubToken {
		p.githubToken = true
	}
	prepareAction := preparationReachable && action.Source == "github"

	switch action.Runtime {
	case ActionRuntimeNative:
		return nil
	case ActionRuntimeJavaScript:
		if action.JavaScript == nil {
			return fmt.Errorf("action program %q has no JavaScript lifecycle", lock)
		}
		preReachable := false
		if action.JavaScript.Pre != "" && (prepareAction || mainReachable) {
			known := workflowInputReferences(mainScope, p.workflowInputScope)
			inputs := mainInputs
			if prepareAction {
				known = workflowInputReferences(preparationScope, p.workflowInputScope)
				inputs = preparationInputs
				p.githubToken = p.githubToken || preparationEnvironmentToken
			}
			// The current runtime evaluates lifecycle conditions with reusable-
			// workflow inputs, not the action's resolved inputs.
			analysis, err := expression.AnalyzeActionLifecycleCondition(action.JavaScript.PreCondition.Source, known)
			if err != nil {
				if validationErr := expression.ValidateActionLifecycleCondition(action.JavaScript.PreCondition.Source); validationErr != nil {
					return fmt.Errorf("action pre-if: %w", err)
				}
				requiresToken, fallbackErr := expression.ConditionReferencesGitHubToken(action.JavaScript.PreCondition.Source)
				if fallbackErr != nil {
					return fmt.Errorf("action pre-if: %w", err)
				}
				preReachable = true
				p.githubToken = p.githubToken || requiresToken
			} else {
				preReachable = !analysis.Value.Known || analysis.Value.Value == true
				p.githubToken = p.githubToken || authorityEffectsRequireToken(analysis.Effects)
			}
			p.githubToken = p.githubToken || preReachable && inputs.githubToken
			p.preparationEnvironmentMutable = p.preparationEnvironmentMutable || prepareAction && preReachable
		}
		if action.JavaScript.Post != "" && (mainReachable || preReachable) {
			known := workflowInputReferences(retainStableEnvironment(mainScope, stableScope), p.workflowInputScope)
			analysis, err := expression.AnalyzeActionLifecycleCondition(action.JavaScript.PostCondition.Source, known)
			if err != nil {
				if validationErr := expression.ValidateActionLifecycleCondition(action.JavaScript.PostCondition.Source); validationErr != nil {
					return fmt.Errorf("action post-if: %w", err)
				}
				requiresToken, fallbackErr := expression.ConditionReferencesGitHubToken(action.JavaScript.PostCondition.Source)
				if fallbackErr != nil {
					return fmt.Errorf("action post-if: %w", err)
				}
				p.githubToken = p.githubToken || requiresToken
			} else {
				p.githubToken = p.githubToken || authorityEffectsRequireToken(analysis.Effects)
			}
		}
		return nil
	case ActionRuntimeDocker:
		if action.Docker == nil {
			return fmt.Errorf("action program %q has no Docker execution", lock)
		}
		known := actionInputReferences(mainScope, mainInputs.values, mainInputs.unknown, mainInputs.omitted)
		for _, binding := range action.Docker.Env {
			if err := p.inspectMetadataTemplate(binding.Value, known, mainReachable); err != nil {
				return err
			}
		}
		for i, argument := range action.Docker.Arguments {
			if err := expression.ValidateDockerActionArg(argument.Source); err != nil {
				return fmt.Errorf("docker action argument %d: %w", i+1, err)
			}
		}
		return nil
	case ActionRuntimeComposite:
		if action.Composite == nil {
			return fmt.Errorf("action program %q has no composite execution", lock)
		}
		p.githubToken = p.githubToken || prepareAction && (preparationEnvironmentToken || preparationInputs.githubToken)
		return p.inspectComposite(action, mainInputs, preparationInputs, mainScope, preparationScope, stableScope, mainReachable, prepareAction)
	default:
		return fmt.Errorf("action program %q has unsupported runtime %q", lock, action.Runtime)
	}
}

func (p *actionAuthorityPlanner) resolveInputs(action Action, supplied []Binding, scope map[string]any, workflowAuthored, resolveDefaults bool) (effectiveActionInputs, error) {
	resolved := effectiveActionInputs{values: map[string]any{}, unknown: map[string]bool{}, omitted: map[string]bool{}}
	definitions := make(map[string]ActionInput, len(action.Inputs))
	for _, input := range action.Inputs {
		definitions[input.Name] = input
	}
	known := scope
	for _, binding := range supplied {
		name := strings.ToLower(binding.Name)
		if _, exists := resolved.values[name]; exists || resolved.unknown[name] {
			return effectiveActionInputs{}, fmt.Errorf("action inputs contain duplicate case-insensitive name %q", name)
		}
		if err := p.inventorySupplied(binding, definitions[name], workflowAuthored); err != nil {
			return effectiveActionInputs{}, err
		}
		wholeGitHub := expression.GitHubTokenCompositeContext
		if workflowAuthored {
			wholeGitHub = expression.GitHubTokenWorkflowContext
		}
		analysis, err := expression.AnalyzeStepTemplate(binding.Value.Source, known, wholeGitHub)
		if err != nil {
			if validationErr := expression.ValidateStepTemplate(binding.Value.Source); validationErr != nil {
				return effectiveActionInputs{}, fmt.Errorf("action input %q: %w", binding.Name, err)
			}
			requiresToken, fallbackErr := stepTemplateReferencesGitHubToken(binding.Value.Source, workflowAuthored)
			if fallbackErr != nil {
				return effectiveActionInputs{}, fmt.Errorf("action input %q: %w", binding.Name, err)
			}
			resolved.githubToken = resolved.githubToken || requiresToken
			resolved.unknown[name] = true
			continue
		}
		resolved.githubToken = resolved.githubToken || authorityEffectsRequireToken(analysis.Effects)
		if analysis.Value.Known {
			resolved.values[name] = analysis.Value.Value
		} else {
			resolved.unknown[name] = true
		}
	}
	if !resolveDefaults {
		return resolved, nil
	}
	for _, input := range action.Inputs {
		if _, exists := resolved.values[input.Name]; exists || resolved.unknown[input.Name] {
			continue
		}
		if input.Default == nil {
			if !input.Required {
				resolved.values[input.Name] = ""
				resolved.omitted[input.Name] = true
			}
			continue
		}
		known = actionInputReferences(scope, resolved.values, resolved.unknown, resolved.omitted)
		for _, later := range action.Inputs {
			if _, exists := resolved.values[later.Name]; !exists && !resolved.unknown[later.Name] {
				known["inputs."+later.Name] = ""
			}
		}
		analysis, err := expression.AnalyzeActionInputDefault(input.Default.Source, known)
		if err != nil {
			serverURL, _ := known["github.server_url"].(string)
			requiresToken, fallbackErr := expression.ActionInputDefaultRequiresGitHubToken(input.Default.Source, serverURL)
			if fallbackErr != nil {
				return effectiveActionInputs{}, fmt.Errorf("action input %q default: %w", input.Name, err)
			}
			resolved.githubToken = resolved.githubToken || requiresToken
			resolved.unknown[input.Name] = true
			continue
		}
		resolved.githubToken = resolved.githubToken || authorityEffectsRequireToken(analysis.Effects)
		if analysis.Value.Known {
			resolved.values[input.Name] = analysis.Value.Value
		} else {
			resolved.unknown[input.Name] = true
		}
	}
	return resolved, nil
}

func (p *actionAuthorityPlanner) inventorySupplied(binding Binding, definition ActionInput, workflowAuthored bool) error {
	referencesEvent, err := expression.TemplateReferencesGitHubEvent(binding.Value.Source)
	if err != nil {
		return err
	}
	if referencesEvent {
		return fmt.Errorf("action input %q: github.event cannot be retained in a job plan", binding.Name)
	}
	names, err := expression.SecretReferences(binding.Value.Source)
	if err != nil {
		return err
	}
	if !workflowAuthored && len(names) != 0 {
		return fmt.Errorf("action input %q: composite action metadata cannot grant secret authority", binding.Name)
	}
	for _, name := range names {
		if definition.Name != "" && !definition.Required && name != "GITHUB_TOKEN" {
			continue
		}
		p.secrets[name] = struct{}{}
	}
	return nil
}

func (p *actionAuthorityPlanner) inspectComposite(action Action, mainInputs, preparationInputs effectiveActionInputs, mainScope, preparationScope, stableScope map[string]any, mainReachable, preparationReachable bool) error {
	mainKnown := actionInputReferences(mainScope, mainInputs.values, mainInputs.unknown, mainInputs.omitted)
	preparationKnown := actionInputReferences(preparationScope, preparationInputs.values, preparationInputs.unknown, preparationInputs.omitted)
	for i, step := range action.Composite.Steps {
		condition, err := expression.AnalyzeCompositeActionCondition(step.Condition.Source, mainKnown)
		if err != nil {
			if validationErr := expression.ValidateCompositeActionCondition(step.Condition.Source); validationErr != nil {
				return fmt.Errorf("composite action step %d condition: %w", i+1, err)
			}
			requiresToken, fallbackErr := expression.ConditionReferencesGitHubToken(step.Condition.Source)
			if fallbackErr != nil {
				return fmt.Errorf("composite action step %d condition: %w", i+1, err)
			}
			p.githubToken = p.githubToken || mainReachable && requiresToken
			condition = expression.Analysis{}
		}
		stepReachable := mainReachable && (!condition.Value.Known || condition.Value.Value == true)
		p.githubToken = p.githubToken || mainReachable && authorityEffectsRequireToken(condition.Effects)
		for _, binding := range step.Env {
			if err := p.inspectMetadataTemplate(binding.Value, mainKnown, stepReachable); err != nil {
				return fmt.Errorf("composite action step %d environment %q: %w", i+1, binding.Name, err)
			}
		}
		if step.Run != nil {
			for _, site := range []Site{step.Run.Command, step.Run.Shell, step.Run.WorkingDirectory} {
				if err := p.inspectMetadataTemplate(site, mainKnown, stepReachable); err != nil {
					return fmt.Errorf("composite action step %d: %w", i+1, err)
				}
			}
		}
		if step.Invocation == nil {
			if stepReachable && step.Run != nil {
				mainKnown = retainStableEnvironment(mainKnown, stableScope)
			}
			continue
		}
		for _, binding := range step.Invocation.With {
			if err := p.inspectMetadataTemplate(binding.Value, mainKnown, stepReachable); err != nil {
				return fmt.Errorf("composite action step %d input %q: %w", i+1, binding.Name, err)
			}
		}
		childMainScope, _, err := overlayActionEnvironment(mainKnown, step.Env)
		if err != nil {
			return fmt.Errorf("composite action step %d environment: %w", i+1, err)
		}
		childPreparationScope, preparationEnvironmentToken, err := overlayActionEnvironment(preparationKnown, step.Env)
		if err != nil {
			return fmt.Errorf("composite action step %d preparation environment: %w", i+1, err)
		}
		stableKnown := actionInputReferences(stableScope, mainInputs.values, mainInputs.unknown, mainInputs.omitted)
		childStableScope, _, err := overlayActionEnvironment(stableKnown, step.Env)
		if err != nil {
			return fmt.Errorf("composite action step %d stable environment: %w", i+1, err)
		}
		preparationWasMutable := p.preparationEnvironmentMutable
		if err := p.inspect(step.Invocation.Lock, step.Invocation.With, step.Invocation.With, childMainScope, childPreparationScope, childStableScope, stepReachable, preparationReachable, preparationEnvironmentToken, false); err != nil {
			return fmt.Errorf("composite action step %d child %q: %w", i+1, step.Invocation.Uses.Source, err)
		}
		if stepReachable {
			mainKnown = retainStableEnvironment(mainKnown, stableScope)
		}
		if preparationReachable && p.preparationEnvironmentMutable != preparationWasMutable {
			preparationKnown = retainStableEnvironment(preparationKnown, stableScope)
		}
	}
	for _, output := range action.Outputs {
		if err := p.inspectMetadataTemplate(output.Value, mainKnown, mainReachable); err != nil {
			return fmt.Errorf("composite output %q: %w", output.Name, err)
		}
	}
	return nil
}

func (p *actionAuthorityPlanner) inspectMetadataTemplate(site Site, known map[string]any, reachable bool) error {
	if site.Source == "" {
		return nil
	}
	referencesEvent, err := expression.TemplateReferencesGitHubEvent(site.Source)
	if err != nil {
		return err
	}
	if referencesEvent {
		return fmt.Errorf("github.event cannot be retained in a job plan")
	}
	names, err := expression.SecretReferences(site.Source)
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return fmt.Errorf("composite action metadata cannot grant secret authority")
	}
	analysis, err := expression.AnalyzeStepTemplate(site.Source, known, expression.GitHubTokenCompositeContext)
	if err != nil {
		if validationErr := expression.ValidateStepTemplate(site.Source); validationErr != nil {
			return err
		}
		requiresToken, fallbackErr := expression.ReferencesCompositeStepGitHubToken(site.Source)
		if fallbackErr != nil {
			return err
		}
		if reachable && requiresToken {
			p.githubToken = true
		}
		return nil
	}
	if reachable && authorityEffectsRequireToken(analysis.Effects) {
		p.githubToken = true
	}
	return nil
}

func workflowKnownReferences(context ActionAuthorityContext) (map[string]any, error) {
	known := map[string]any{"github.server_url": context.ServerURL}
	workflowInputs := make(map[string]any, len(context.WorkflowInputs))
	for name, value := range context.WorkflowInputs {
		name = strings.ToLower(name)
		if !context.UnknownWorkflowInputs[name] {
			known["inputs."+name] = value
			workflowInputs[name] = value
		}
	}
	if len(context.UnknownWorkflowInputs) == 0 && context.WorkflowInputs != nil {
		known["inputs"] = workflowInputs
	}
	for name, value := range context.Environment {
		known["env."+strings.ToLower(name)] = value
	}
	if context.Matrix != nil {
		matrix := make(map[string]any, len(context.Matrix))
		for name, value := range context.Matrix {
			name = strings.ToLower(name)
			value = normalizeKnownValue(value)
			matrix[name] = value
			retainKnownReferences(known, "matrix."+name, value)
		}
		known["matrix"] = matrix
	}
	for _, layer := range context.EnvironmentLayers {
		values := make(map[string]expression.AbstractValue, len(layer))
		for _, binding := range layer {
			analysis, err := expression.AnalyzeStepTemplate(binding.Value.Source, known, expression.GitHubTokenWorkflowContext)
			if err != nil {
				if validationErr := expression.ValidateStepTemplate(binding.Value.Source); validationErr != nil {
					return nil, fmt.Errorf("environment %q: %w", binding.Name, err)
				}
				if _, fallbackErr := expression.ReferencesStepGitHubToken(binding.Value.Source); fallbackErr != nil {
					return nil, fmt.Errorf("environment %q: %w", binding.Name, err)
				}
				values[strings.ToLower(binding.Name)] = expression.AbstractValue{}
				continue
			}
			values[strings.ToLower(binding.Name)] = analysis.Value
		}
		for name, value := range values {
			name = "env." + name
			if value.Known {
				known[name] = value.Value
			} else {
				delete(known, name)
			}
		}
	}
	return known, nil
}

func actionPhaseKnownReferences(context ActionAuthorityContext, mutable bool) (map[string]any, error) {
	if !mutable {
		return workflowKnownReferences(context)
	}
	context.Environment = nil
	if len(context.EnvironmentLayers) != 0 {
		context.EnvironmentLayers = context.EnvironmentLayers[len(context.EnvironmentLayers)-1:]
	}
	return workflowKnownReferences(context)
}

// WorkflowEnvironmentMutableBefore reports whether an earlier workflow step
// can execute and append to GITHUB_ENV before the selected step.
func WorkflowEnvironmentMutableBefore(steps []Step, before int, context ActionAuthorityContext) (bool, error) {
	known, err := workflowKnownReferences(context)
	if err != nil {
		return false, err
	}
	for _, step := range steps[:before] {
		if step.Run == nil && step.Invocation == nil {
			continue
		}
		analysis, err := expression.AnalyzeCondition(step.Condition.Source, known, expression.GitHubTokenWorkflowContext)
		if err != nil {
			if validationErr := expression.ValidateCondition(step.Condition.Source, expression.StepCondition); validationErr != nil {
				return false, err
			}
			return true, nil
		}
		if !analysis.Value.Known || analysis.Value.Value == true {
			return true, nil
		}
	}
	return false, nil
}

func normalizeKnownValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(value))
		for name, child := range value {
			normalized[strings.ToLower(name)] = normalizeKnownValue(child)
		}
		return normalized
	case []any:
		normalized := make([]any, len(value))
		for i, child := range value {
			normalized[i] = normalizeKnownValue(child)
		}
		return normalized
	default:
		return value
	}
}

func retainKnownReferences(known map[string]any, prefix string, value any) {
	known[prefix] = value
	if object, ok := value.(map[string]any); ok {
		for name, child := range object {
			retainKnownReferences(known, prefix+"."+name, child)
		}
	}
}

func actionInputReferences(scope, inputs map[string]any, unknown, omitted map[string]bool) map[string]any {
	known := make(map[string]any, len(scope)+len(inputs)+1)
	for name, value := range scope {
		if name != "inputs" && !strings.HasPrefix(name, "inputs.") {
			known[name] = value
		}
	}
	for name, value := range inputs {
		known["inputs."+strings.ToLower(name)] = value
	}
	if len(unknown) == 0 && inputs != nil {
		aggregate := make(map[string]any, len(inputs)-len(omitted))
		for name, value := range inputs {
			if !omitted[name] {
				aggregate[name] = value
			}
		}
		known["inputs"] = aggregate
	}
	return known
}

func workflowInputReferences(scope, workflowScope map[string]any) map[string]any {
	known := make(map[string]any, len(scope))
	for name, value := range scope {
		if name != "inputs" && !strings.HasPrefix(name, "inputs.") {
			known[name] = value
		}
	}
	for name, value := range workflowScope {
		if name == "inputs" || strings.HasPrefix(name, "inputs.") {
			known[name] = value
		}
	}
	return known
}

func overlayActionEnvironment(scope map[string]any, bindings []Binding) (map[string]any, bool, error) {
	result := make(map[string]any, len(scope)+len(bindings))
	githubToken := false
	for name, value := range scope {
		result[name] = value
	}
	for _, binding := range bindings {
		analysis, err := expression.AnalyzeStepTemplate(binding.Value.Source, scope, expression.GitHubTokenCompositeContext)
		if err != nil {
			if validationErr := expression.ValidateStepTemplate(binding.Value.Source); validationErr != nil {
				return nil, false, err
			}
			requiresToken, fallbackErr := expression.ReferencesCompositeStepGitHubToken(binding.Value.Source)
			if fallbackErr != nil {
				return nil, false, err
			}
			githubToken = githubToken || requiresToken
			delete(result, "env."+strings.ToLower(binding.Name))
			continue
		}
		githubToken = githubToken || authorityEffectsRequireToken(analysis.Effects)
		name := "env." + strings.ToLower(binding.Name)
		if analysis.Value.Known {
			result[name] = analysis.Value.Value
		} else {
			delete(result, name)
		}
	}
	return result, githubToken, nil
}

func withoutKnownEnvironment(known map[string]any) map[string]any {
	result := make(map[string]any, len(known))
	for name, value := range known {
		if !strings.HasPrefix(name, "env.") {
			result[name] = value
		}
	}
	return result
}

func retainStableEnvironment(known, stable map[string]any) map[string]any {
	result := withoutKnownEnvironment(known)
	for name, value := range stable {
		if strings.HasPrefix(name, "env.") {
			result[name] = value
		}
	}
	return result
}

func authorityEffectsRequireToken(effects expression.Effects) bool {
	return effects.GitHubToken&(expression.GitHubTokenDirect|expression.GitHubTokenWorkflowContext) != 0
}

func stepTemplateReferencesGitHubToken(source string, workflowAuthored bool) (bool, error) {
	if workflowAuthored {
		return expression.ReferencesStepGitHubToken(source)
	}
	return expression.ReferencesCompositeStepGitHubToken(source)
}
