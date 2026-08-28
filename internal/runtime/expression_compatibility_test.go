package runtime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
)

// These helpers preserve field-oriented test setup while exercising the same
// expression engine as normalized programs. They are compatibility helpers,
// not an independent differential oracle. Production execution accepts
// normalized programs only.
func evaluateMap(values map[string]string, context expression.Context) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, name := range sortedKeys(values) {
		resolved, err := evaluateLegacyTemplate(values[name], expression.ProfileRuntimeTemplate, context)
		if err != nil {
			return nil, fmt.Errorf("evaluate %q: %w", name, err)
		}
		out[name] = resolved
	}
	return out, nil
}

func evaluateStepMap(values map[string]string, context expression.Context) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, name := range sortedKeys(values) {
		resolved, err := evaluateLegacyTemplate(values[name], expression.ProfileStepTemplate, context)
		if err != nil {
			return nil, fmt.Errorf("evaluate %q: %w", name, err)
		}
		out[name] = resolved
	}
	return out, nil
}

func resolveActionInputs(action metadata.Metadata, supplied map[string]string, context expression.Context) (map[string]string, error) {
	inputs := make(map[string]string, len(supplied))
	for _, name := range sortedKeys(supplied) {
		lower := strings.ToLower(name)
		if _, exists := inputs[lower]; exists {
			return nil, fmt.Errorf("action inputs contain duplicate case-insensitive name %q", lower)
		}
		inputs[lower] = supplied[name]
	}
	names := make([]string, 0, len(action.Inputs))
	for name := range action.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	defaultContext := context
	if token, ok := context.Secrets["GITHUB_TOKEN"]; ok {
		defaultContext.GitHub = cloneAnyMap(context.GitHub)
		defaultContext.GitHub["token"] = token
	}
	for _, name := range names {
		definition := action.Inputs[name]
		if _, ok := inputs[name]; ok {
			continue
		}
		if definition.Default != nil {
			defaultContext.Inputs = inputs
			value, err := evaluateLegacyTemplate(*definition.Default, expression.ProfileActionInputDefault, defaultContext)
			if err != nil {
				return nil, fmt.Errorf("action input %q default: %w", name, err)
			}
			inputs[name] = value
			continue
		}
		if definition.Required {
			return nil, fmt.Errorf("required action input %q is missing", name)
		}
	}
	return inputs, nil
}

func evaluateLegacyTemplate(source string, profile expression.ProfileID, context expression.Context) (string, error) {
	value, err := expression.NewEngine().Evaluate(expression.Site{Source: source, Profile: profile, Result: expression.ResultString, Purpose: expression.PurposeExpression}, expression.Values{Runtime: context})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}
