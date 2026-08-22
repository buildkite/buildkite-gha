package program

import (
	contextpkg "context"
	"fmt"
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
	ID          string
	Lock        string
	Inputs      []Binding
	Environment []Binding
	Condition   Site
}

// InventoryActionAuthority abstractly executes normalized action lifecycle
// structure and returns the workflow authority that reachable phases require.
func InventoryActionAuthority(actions map[string]Action, invocation ActionInvocation, context ActionAuthorityContext) (Authority, error) {
	if err := validateActionAuthorityProgram(actions, invocation.Lock, map[string]bool{}); err != nil {
		return Authority{}, err
	}
	secrets, err := inventoryInvocationSecrets(actions, invocation)
	if err != nil {
		return Authority{}, err
	}
	if invocation.ID == "" {
		invocation.ID = "root"
	}
	machine := NewActionMachine(actions, authorityAdapter{}, authorityMachineState{})
	preparationFrame := authorityFrame(context, context.PreparationEnvironmentMutable)
	preparationFrame, preparation, err := machine.Prepare(contextpkg.Background(), invocation, preparationFrame)
	if err != nil {
		return Authority{}, err
	}
	if preparation.Failure != nil && preparation.Failure.Cause != nil {
		return Authority{}, preparation.Failure.Cause
	}
	mainFrame := authorityFrame(context, context.MainEnvironmentMutable)
	mainFrame.Environment = joinValueObjects(mainFrame.Environment, preparationFrame.Environment)
	mainFrame, execution, err := machine.Invoke(contextpkg.Background(), invocation, mainFrame)
	if err != nil {
		return Authority{}, err
	}
	if execution.Failure != nil && execution.Failure.Cause != nil {
		return Authority{}, execution.Failure.Cause
	}
	_, posts, err := machine.Finish(contextpkg.Background(), mainFrame)
	if err != nil {
		return Authority{}, err
	}
	if posts.Failure != nil && posts.Failure.Cause != nil {
		return Authority{}, posts.Failure.Cause
	}
	state := machine.State()
	return Authority{
		GitHubToken:                   state.githubToken,
		PreparationEnvironmentMutable: state.preparationEnvironmentMutable,
		Secrets:                       secrets,
	}, nil
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

// WorkflowEnvironmentMutableBefore reports whether an earlier workflow step
// can execute and append to GITHUB_ENV before the selected step.
func WorkflowEnvironmentMutableBefore(steps []Step, before int, context ActionAuthorityContext, immutableInvocations map[int]bool) (bool, error) {
	background := map[string]bool{}
	for i, step := range steps[:before] {
		stepContext := context
		stepContext.EnvironmentLayers = append(append([][]Binding(nil), context.EnvironmentLayers...), step.Env)
		known, err := workflowKnownReferences(stepContext)
		if err != nil {
			return false, err
		}
		analysis, err := expression.AnalyzeCondition(step.Condition.Source, known, expression.GitHubTokenWorkflowContext)
		if err != nil {
			if validationErr := expression.ValidateCondition(step.Condition.Source, expression.StepCondition); validationErr != nil {
				return false, err
			}
			return true, nil
		}
		if analysis.Value.Known && analysis.Value.Value != true {
			continue
		}
		if step.Kind == "wait-all" && len(background) != 0 {
			return true, nil
		}
		if step.Kind == "wait" || step.Kind == "cancel" {
			for _, target := range step.Targets {
				if background[strings.ToLower(target)] {
					return true, nil
				}
			}
			continue
		}
		if step.Run == nil && step.Invocation == nil {
			continue
		}
		if step.Invocation != nil && immutableInvocations[i] {
			continue
		}
		if step.Background {
			background[strings.ToLower(step.ID)] = true
			continue
		}
		return true, nil
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

func authorityEffectsRequireToken(effects expression.Effects) bool {
	return effects.GitHubToken&(expression.GitHubTokenDirect|expression.GitHubTokenWorkflowContext) != 0
}

func stepTemplateReferencesGitHubToken(source string, workflowAuthored bool) (bool, error) {
	if workflowAuthored {
		return expression.ReferencesStepGitHubToken(source)
	}
	return expression.ReferencesCompositeStepGitHubToken(source)
}
