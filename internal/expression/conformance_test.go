package expression

import (
	"strings"
	"testing"
)

// These cases are the GitHub-compatible semantic baseline already implemented
// by action input defaults. Later expression surfaces should reuse the same
// expected results instead of defining their own coercion rules.
func TestGitHubExpressionCoreConformanceBaseline(t *testing.T) {
	for _, test := range []struct {
		name       string
		expression string
		want       string
	}{
		{name: "null equals zero", expression: "${{ null == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "false equals zero", expression: "${{ false == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "empty string equals zero", expression: "${{ '' == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "numeric string equals number", expression: "${{ '2.5' == 2.5 && 'yes' || 'no' }}", want: "yes"},
		{name: "strings compare without case", expression: "${{ 'Release' == 'release' && 'yes' || 'no' }}", want: "yes"},
		{name: "nonnumeric string differs from zero", expression: "${{ 'not-a-number' != 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "zero is falsy", expression: "${{ 0 && 'yes' || 'no' }}", want: "no"},
		{name: "empty string is falsy", expression: "${{ '' || 'fallback' }}", want: "fallback"},
		{name: "and returns its selected operand", expression: "${{ true && 'selected' }}", want: "selected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateActionInputDefault(test.expression, Context{})
			if err != nil || got != test.want {
				t.Fatalf("EvaluateActionInputDefault(%q) = %q, %v, want %q", test.expression, got, err, test.want)
			}
		})
	}
}

func TestExpressionParserParityPrerequisites(t *testing.T) {
	for _, source := range []string{
		"1 < 2 && 3 >= 2",
		"github.event.issue.labels.*.name",
		"case(true, 'selected', 'default')",
	} {
		t.Run(source, func(t *testing.T) {
			if _, _, err := parseCondition(source); err != nil {
				t.Fatalf("parseCondition(%q) = %v", source, err)
			}
		})
	}

	if _, _, err := parseCondition("github.event.issue.labels[*].name"); err == nil {
		t.Fatal("pinned expression parser accepted bracket wildcard unexpectedly")
	}
}

func TestExpressionAuthorityBaselineRejectsFilteredSecrets(t *testing.T) {
	if _, err := SecretReferences("${{ secrets.* }}"); err == nil || !strings.Contains(err.Error(), "must name exactly one secret") {
		t.Fatalf("SecretReferences(filtered secrets) = %v, want static-name rejection", err)
	}
	if _, err := SecretReferences("${{ secrets[inputs.name] }}"); err == nil || !strings.Contains(err.Error(), "index must be a string literal") {
		t.Fatalf("SecretReferences(dynamic secret) = %v, want dynamic-index rejection", err)
	}
	if _, err := ReferencesGitHubToken("${{ github[inputs.name] }}"); err == nil || !strings.Contains(err.Error(), "index must be a string literal") {
		t.Fatalf("ReferencesGitHubToken(dynamic github property) = %v, want dynamic-index rejection", err)
	}
}
