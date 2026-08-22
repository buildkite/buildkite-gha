package program

import (
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

// EvaluationContext supplies both template and condition contexts because
// conditions carry failure and cancellation state that templates do not.
type EvaluationContext struct {
	Expression expression.Context
	Condition  expression.ConditionContext
}

// EvaluateSite applies the concrete runtime semantics selected by a site.
// The normalized interpreter and the legacy runtime can therefore be tested
// against the same expression inputs before the plan-schema cutover.
func EvaluateSite(site Site, context EvaluationContext) (any, error) {
	switch site.Surface {
	case SurfaceJobCondition, SurfaceStepCondition:
		return expression.EvaluateCondition(site.Source, context.Condition)
	case SurfaceJobEnvironment:
		return expression.EvaluateJobEnvironment(site.Source, context.Expression)
	case SurfaceJobDefault:
		return expression.EvaluateJobDefault(site.Source, context.Expression)
	case SurfaceJobOutput:
		return expression.EvaluateJobOutput(site.Source, context.Expression)
	case SurfaceStepTemplate:
		return expression.EvaluateStep(site.Source, context.Expression)
	case SurfaceStepControl:
		return expression.EvaluateStepControl(site.Source, context.Expression)
	case SurfaceRuntimeTemplate, SurfaceServiceCredential:
		return expression.Evaluate(site.Source, context.Expression)
	case SurfaceServiceMap:
		return expression.EvaluateObject(site.Source, context.Expression)
	default:
		return nil, fmt.Errorf("unsupported expression surface %q", site.Surface)
	}
}

// EvaluateBindings evaluates ordered bindings and retains deterministic error
// ownership. Later bindings do not run after an earlier failure.
func EvaluateBindings(bindings []Binding, context EvaluationContext) (map[string]string, error) {
	values := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		value, err := EvaluateSite(binding.Value, context)
		if err != nil {
			return nil, fmt.Errorf("evaluate %q: %w", binding.Name, err)
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("evaluate %q produced %T, want string", binding.Name, value)
		}
		values[binding.Name] = text
	}
	return values, nil
}
