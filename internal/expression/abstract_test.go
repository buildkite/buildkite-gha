package expression

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rhysd/actionlint"
)

func TestAbstractActionInputDefaultMatchesConcreteWhenKnown(t *testing.T) {
	for _, source := range []string{
		"false && 'unreachable' || 'fallback'",
		"github.server_url == 'https://github.com' && 'github' || 'other'",
		"case(false, 'unused', true, format('{0}-{1}', 'selected', 2), 'fallback')",
		"contains(fromJSON('[1, 2]'), 2)",
		"join(fromJSON('[\"one\", \"two\"]'), '-')",
	} {
		t.Run(source, func(t *testing.T) {
			node := parseAbstractTestExpression(t, source)
			context := Context{GitHub: map[string]any{"server_url": "https://github.com"}}
			concrete, err := evaluateActionInputDefaultNode(node, context)
			if err != nil {
				t.Fatal(err)
			}
			abstract, err := analyzeActionInputDefault(node, map[string]any{"github.server_url": "https://github.com"})
			if err != nil {
				t.Fatal(err)
			}
			if !abstract.Value.Known || !reflect.DeepEqual(abstract.Value.Value, concrete) {
				t.Fatalf("abstract value = %#v, concrete value = %#v", abstract.Value, concrete)
			}
			if abstract.Effects != (Effects{}) {
				t.Fatalf("abstract effects = %#v, want none", abstract.Effects)
			}
		})
	}
}

func TestAbstractActionInputDefaultTokenEffectsNarrowWithKnownValues(t *testing.T) {
	node := parseAbstractTestExpression(t, "github.server_url == 'https://github.com' && github.token || ''")
	for _, test := range []struct {
		name  string
		known map[string]any
		want  GitHubTokenEffect
	}{
		{name: "unknown provider", want: GitHubTokenDirect},
		{name: "GitHub provider", known: map[string]any{"github.server_url": "https://github.com"}, want: GitHubTokenDirect},
		{name: "Origin provider", known: map[string]any{"github.server_url": "https://origin.cursor.com"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			analysis, err := analyzeActionInputDefault(node, test.known)
			if err != nil {
				t.Fatal(err)
			}
			if analysis.Effects.GitHubToken != test.want {
				t.Fatalf("github.token effects = %v, want %v", analysis.Effects.GitHubToken, test.want)
			}
		})
	}
}

func TestAbstractActionInputDefaultPreservesLazyAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   bool
	}{
		{name: "known false logical branch", source: "false && github.token || ''"},
		{name: "selected case branch", source: "case(false, github.token, true, 'selected', github.token)"},
		{name: "unused format argument", source: "format('constant', github.token)"},
		{name: "empty contains collection", source: "contains(fromJSON('[]'), github.token)"},
		{name: "single join item", source: "join(fromJSON('[\"one\"]'), github.token)"},
		{name: "unknown logical branch", source: "inputs.enabled && github.token || ''", want: true},
		{name: "unknown case branch", source: "case(inputs.enabled, '', true, github.token, '')", want: true},
		{name: "unknown case before false branch", source: "case(inputs.enabled, '', false, github.token, '')"},
		{name: "equal unknown case results", source: "case(inputs.enabled, false, false) && github.token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			analysis, err := analyzeActionInputDefault(parseAbstractTestExpression(t, test.source), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := analysis.Effects.GitHubToken != 0; got != test.want {
				t.Fatalf("requires github.token = %v, want %v", got, test.want)
			}
		})
	}
}

func TestActionInputDefaultAuthorityPreservesRuntimeErrorFallback(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   bool
	}{
		{name: "known false skips error and token", source: "${{ false && fromJSON('bad') && github.token || '' }}"},
		{name: "unknown guard conservatively falls back", source: "${{ inputs.enabled && fromJSON('bad') && github.token || '' }}", want: true},
		{name: "reachable error conservatively falls back", source: "${{ fromJSON('bad') && github.token || '' }}", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ActionInputDefaultRequiresGitHubToken(test.source, "https://origin.cursor.com")
			if err != nil || got != test.want {
				t.Fatalf("ActionInputDefaultRequiresGitHubToken() = %v, %v, want %v", got, err, test.want)
			}
		})
	}
}

func TestStepTemplateTokenEffectsNarrowWithKnownInputs(t *testing.T) {
	template := "${{ inputs.fallback == 'cargo-binstall' && github.server_url == 'https://github.com' && github.token || '' }}"
	for _, test := range []struct {
		name  string
		known map[string]any
		want  bool
	}{
		{name: "unknown input", known: map[string]any{"github.server_url": "https://github.com"}, want: true},
		{name: "matching input", known: map[string]any{"inputs.fallback": "cargo-binstall", "github.server_url": "https://github.com"}, want: true},
		{name: "disabled input", known: map[string]any{"inputs.fallback": "none", "github.server_url": "https://github.com"}},
		{name: "Origin provider", known: map[string]any{"inputs.fallback": "cargo-binstall", "github.server_url": "https://origin.cursor.com"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := StepTemplateRequiresGitHubToken(template, test.known)
			if err != nil || got != test.want {
				t.Fatalf("StepTemplateRequiresGitHubToken() = %v, %v, want %v", got, err, test.want)
			}
		})
	}
}

func TestStepTemplateWholeGitHubContextDoesNotGrantTokenAuthority(t *testing.T) {
	got, err := StepTemplateRequiresGitHubToken("${{ toJSON(github) }}", nil)
	if err != nil || got {
		t.Fatalf("StepTemplateRequiresGitHubToken() = %v, %v, want false", got, err)
	}
}

func TestStepTemplateFunctionsPreserveTokenReachability(t *testing.T) {
	for _, test := range []struct {
		template string
		want     bool
	}{
		{template: "${{ hashFiles(github.token) }}", want: true},
		{template: "${{ format('constant', github.token) }}"},
	} {
		got, err := StepTemplateRequiresGitHubToken(test.template, nil)
		if err != nil || got != test.want {
			t.Fatalf("StepTemplateRequiresGitHubToken(%q) = %v, %v, want %v", test.template, got, err, test.want)
		}
	}
}

func TestStepTemplateKnownRightGuardPrunesUnknownLeft(t *testing.T) {
	for _, template := range []string{
		"${{ env.RUNTIME == 'yes' && inputs.enabled == 'true' && github.token || '' }}",
		"${{ true && (env.RUNTIME == 'yes' && inputs.enabled == 'true') && github.token || '' }}",
		"${{ case(env.SELECT == 'yes', env.RUNTIME == 'yes' && false, false) && github.token || '' }}",
		"${{ case(env.RUNTIME == 'yes' && inputs.enabled == 'true', github.token, '') }}",
	} {
		got, err := StepTemplateRequiresGitHubToken(template, map[string]any{"inputs.enabled": "false"})
		if err != nil || got {
			t.Errorf("StepTemplateRequiresGitHubToken(%q) = %v, %v, want false", template, got, err)
		}
	}
}

func TestConditionMayBeTrueUsesKnownReferencesAfterUnknownValues(t *testing.T) {
	known := map[string]any{
		"inputs.enabled":    "false",
		"github.server_url": "https://origin.cursor.com",
	}
	for _, condition := range []string{
		"env.RUNTIME == 'yes' && inputs.enabled == 'true'",
		"env.RUNTIME == 'yes' && github.server_url == 'https://github.com'",
		"success() && inputs.enabled == 'true'",
	} {
		mayRun, err := ConditionMayBeTrue(condition, known)
		if err != nil || mayRun {
			t.Errorf("ConditionMayBeTrue(%q) = %v, %v, want false", condition, mayRun, err)
		}
	}
}

func TestEvaluateKnownActionInputDefaultUsesProviderValues(t *testing.T) {
	value, known, err := EvaluateKnownActionInputDefault(
		"${{ github.server_url == 'https://github.com' && 'true' || 'false' }}",
		map[string]any{"github.server_url": "https://origin.cursor.com"},
	)
	if err != nil || !known || value != "false" {
		t.Fatalf("EvaluateKnownActionInputDefault() = %q, %v, %v, want false, true", value, known, err)
	}
	_, known, err = EvaluateKnownActionInputDefault("${{ matrix.enabled }}", map[string]any{"github.server_url": "https://github.com"})
	if err != nil || known {
		t.Fatalf("runtime-dependent EvaluateKnownActionInputDefault() known = %v, error = %v", known, err)
	}
}

func TestAbstractActionInputDefaultIsSoundAsValuesBecomeKnown(t *testing.T) {
	for _, source := range []string{
		"matrix.enabled && github.token || ''",
		"case(matrix.enabled, '', true, github.token, '')",
		"format(case(matrix.enabled, '{0}', 'constant'), github.token)",
		"contains(case(matrix.enabled, fromJSON('[]'), fromJSON('[1]')), github.token)",
		"join(case(matrix.enabled, fromJSON('[1]'), fromJSON('[1, 2]')), github.token)",
	} {
		t.Run(source, func(t *testing.T) {
			node := parseAbstractTestExpression(t, source)
			lessKnown, err := analyzeActionInputDefault(node, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{false, true} {
				known := map[string]any{"matrix.enabled": enabled, "github.token": "ghs_scoped"}
				refined, err := analyzeActionInputDefault(node, known)
				if err != nil {
					t.Fatal(err)
				}
				if refined.Effects.GitHubToken&^lessKnown.Effects.GitHubToken != 0 {
					t.Fatalf("refined effects %v are not contained by less-known effects %v", refined.Effects.GitHubToken, lessKnown.Effects.GitHubToken)
				}
				concrete, err := evaluateActionInputDefaultNode(node, Context{Matrix: map[string]any{"enabled": enabled}, GitHub: map[string]any{"token": "ghs_scoped"}})
				if err != nil {
					t.Fatal(err)
				}
				if !refined.Value.Known || !reflect.DeepEqual(refined.Value.Value, concrete) {
					t.Fatalf("abstract value = %#v, concrete value = %#v", refined.Value, concrete)
				}
			}
		})
	}
}

func TestAbstractCaseDoesNotMergeDistinctObjectIdentities(t *testing.T) {
	node := parseAbstractTestExpression(t, "case(matrix.enabled, matrix.left, matrix.right) != matrix.left && github.token || ''")
	left := map[string]any{"value": true}
	right := map[string]any{"value": true}
	analysis, err := analyzeActionInputDefault(node, map[string]any{
		"matrix.left":  left,
		"matrix.right": right,
	})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Effects.GitHubToken != GitHubTokenDirect {
		t.Fatalf("github.token effects = %v, want %v", analysis.Effects.GitHubToken, GitHubTokenDirect)
	}
}

func FuzzAbstractActionInputDefaultMatchesConcrete(f *testing.F) {
	for _, serverURL := range []string{"https://github.com", "HTTPS://GITHUB.COM", "https://origin.cursor.com", ""} {
		f.Add(serverURL, "ghs_scoped")
	}
	node, err := actionlint.NewExprParser().Parse(actionlint.NewExprLexer("github.server_url == 'https://github.com' && github.token || ''}}"))
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, serverURL, token string) {
		concrete, err := evaluateActionInputDefaultNode(node, Context{GitHub: map[string]any{"server_url": serverURL, "token": token}})
		if err != nil {
			t.Fatal(err)
		}
		abstract, err := analyzeActionInputDefault(node, map[string]any{"github.server_url": serverURL, "github.token": token})
		if err != nil {
			t.Fatal(err)
		}
		if !abstract.Value.Known || !reflect.DeepEqual(abstract.Value.Value, concrete) {
			t.Fatalf("abstract value = %#v, concrete value = %#v", abstract.Value, concrete)
		}
		wantEffect := GitHubTokenEffect(0)
		if strings.EqualFold(serverURL, "https://github.com") {
			wantEffect = GitHubTokenDirect
		}
		if abstract.Effects.GitHubToken != wantEffect {
			t.Fatalf("github.token effects = %v, want %v", abstract.Effects.GitHubToken, wantEffect)
		}
	})
}

func parseAbstractTestExpression(t *testing.T, source string) actionlint.ExprNode {
	t.Helper()
	node, err := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(source + "}}"))
	if err != nil {
		t.Fatalf("parse expression: %v", err)
	}
	return node
}
