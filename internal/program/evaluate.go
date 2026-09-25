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

// ValidateSite validates a normalized site and returns its exhaustive secret
// inventory through the same profile used by evaluation and authority analysis.
func ValidateSite(site Site) ([]string, error) {
	validation, err := expression.NewEngine().Validate(site.expressionSite())
	return validation.Secrets, err
}

// EvaluateSite applies the concrete runtime semantics selected by a site.
func EvaluateSite(site Site, context EvaluationContext) (any, error) {
	if site.Provenance == ProvenanceAction && site.Surface == SurfaceStepTemplate {
		context.Expression.HashFiles = nil
		context.Expression.HashFilesContext = nil
	}
	return expression.NewEngine().Evaluate(site.expressionSite(), expression.Values{
		Runtime: context.Expression, Condition: context.Condition,
	})
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

// StaticReference returns one complete static reference from a normalized
// site. Compound expressions and dynamic indexes return an error.
func StaticReference(site Site) (string, []string, error) {
	return expression.NewEngine().StaticReference(site.expressionSite())
}
