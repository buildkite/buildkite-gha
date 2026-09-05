package runtime

import (
	"github.com/buildkite/buildkite-gha/internal/expression"
)

// evaluateLegacyTemplate evaluates one template through the production
// expression engine for tests that assert on context visibility directly.
func evaluateLegacyTemplate(source string, profile expression.ProfileID, context expression.Context) (string, error) {
	value, err := expression.NewEngine().Evaluate(expression.Site{Source: source, Profile: profile, Result: expression.ResultString, Purpose: expression.PurposeExpression}, expression.Values{Runtime: context})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}
