package program

import (
	"encoding/json"
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
	var value any
	var err error
	switch site.Surface {
	case SurfaceJobCondition, SurfaceStepCondition:
		value, err = expression.EvaluateCondition(site.Source, context.Condition)
	case SurfaceJobEnvironment:
		value, err = expression.EvaluateJobEnvironment(site.Source, context.Expression)
	case SurfaceJobDefault:
		value, err = expression.EvaluateJobDefault(site.Source, context.Expression)
	case SurfaceJobOutput:
		value, err = expression.EvaluateJobOutput(site.Source, context.Expression)
	case SurfaceStepTemplate:
		value, err = expression.EvaluateStep(site.Source, context.Expression)
	case SurfaceStepControl:
		value, err = expression.EvaluateStepControl(site.Source, context.Expression)
	case SurfaceRuntimeTemplate, SurfaceServiceCredential:
		value, err = expression.Evaluate(site.Source, context.Expression)
	case SurfaceActionInputDefault:
		value, err = expression.EvaluateActionInputDefault(site.Source, context.Expression)
	case SurfaceActionLifecycle:
		value, err = expression.EvaluateActionLifecycleCondition(site.Source, context.Condition)
	case SurfaceCompositeTemplate:
		value, err = expression.EvaluateStep(site.Source, context.Expression)
	case SurfaceDockerArgument:
		value, err = expression.EvaluateDockerActionArg(site.Source, context.Expression.Inputs)
	case SurfaceServiceMap:
		value, err = expression.EvaluateObject(site.Source, context.Expression)
	default:
		return nil, fmt.Errorf("unsupported expression surface %q", site.Surface)
	}
	if err != nil {
		return nil, err
	}
	if !matchesResultType(value, site.Result) {
		return nil, fmt.Errorf("expression produced %T, want %s", value, site.Result)
	}
	return value, nil
}

func matchesResultType(value any, result ResultType) bool {
	switch result {
	case ResultString:
		_, ok := value.(string)
		return ok
	case ResultBoolean:
		_, ok := value.(bool)
		return ok
	case ResultNumber:
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			return true
		default:
			return false
		}
	case ResultObject:
		_, ok := value.([]expression.ObjectEntry)
		return ok
	default:
		return false
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
