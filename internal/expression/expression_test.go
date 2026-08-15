package expression

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseUsesExpressionSyntaxFrontend(t *testing.T) {
	expr, err := Parse("${{ fromJSON(needs.prepare.outputs.matrix) }}", 7, 15)
	if err != nil {
		t.Fatal(err)
	}
	if expr.Span.Start.Line != 7 || expr.Span.Start.Column != 15 || expr.Span.End.Column <= expr.Span.Start.Column {
		t.Fatalf("Parse() span = %#v, want owned source span", expr.Span)
	}

	_, err = Parse("${{ true || }}", 1, 1)
	if err == nil || !strings.Contains(err.Error(), "invalid expression") {
		t.Fatalf("Parse() error = %v, want syntax error", err)
	}

	_, err = Parse("${{ vars.RUNNER }} }}", 1, 1)
	if err == nil || !strings.Contains(err.Error(), "embedded closing delimiter") {
		t.Fatalf("Parse() error = %v, want embedded delimiter rejection", err)
	}
}

func TestReferencePathOwnsOnlyOneStaticReference(t *testing.T) {
	for _, source := range []string{
		"${{ jobs.Build.outputs.Release }}",
		"${{ jobs['Build'].outputs['Release'] }}",
	} {
		root, path, err := ReferencePath(source)
		if err != nil {
			t.Fatalf("ReferencePath(%q) error = %v", source, err)
		}
		if !strings.EqualFold(root, "jobs") || len(path) != 3 || !strings.EqualFold(path[0], "Build") || !strings.EqualFold(path[1], "outputs") || !strings.EqualFold(path[2], "Release") {
			t.Fatalf("ReferencePath(%q) = %q / %#v", source, root, path)
		}
	}
	for _, source := range []string{
		"literal",
		"${{ jobs.build.outputs.release }} suffix",
		"${{ format('{0}', jobs.build.outputs.release) }}",
		"${{ jobs[inputs.name].outputs.release }}",
	} {
		if _, _, err := ReferencePath(source); err == nil {
			t.Fatalf("ReferencePath(%q) succeeded, want static-reference rejection", source)
		}
	}
}

func TestRuntimeMatrixOutputAcceptsOnlyExactDirectNeedOutput(t *testing.T) {
	for _, source := range []string{
		"${{ fromJSON(needs.build_django_matrix.outputs.include) }}",
		"${{ fromJson( needs.Build-Django.outputs.Matrix_1 ) }}",
	} {
		expr, err := Parse(source, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RuntimeMatrixOutput(expr)
		if err != nil {
			t.Fatalf("RuntimeMatrixOutput(%q) error = %v", source, err)
		}
		if got.Job == "" || got.Output == "" {
			t.Fatalf("RuntimeMatrixOutput(%q) = %#v", source, got)
		}
	}

	for _, source := range []string{
		"${{ needs.build.outputs.matrix }}",
		"${{ fromJSON(vars.matrix) }}",
		"${{ fromJSON(needs.build.outputs.matrix || '[]') }}",
		"${{ fromJSON(needs.build.outputs.matrix).include }}",
		"${{ fromJSON(needs['build'].outputs.matrix) }}",
		"${{ toJSON(needs.build.outputs.matrix) }}",
		"${{ fromJSON(needs.build.result) }}",
	} {
		expr, err := Parse(source, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RuntimeMatrixOutput(expr); err == nil {
			t.Fatalf("RuntimeMatrixOutput(%q) unexpectedly succeeded", source)
		}
	}
}

func TestValidateRuntimeTemplateMatchesEvaluateReferenceGrammar(t *testing.T) {
	for _, template := range []string{
		"literal",
		"${{ github.actor }}",
		"${{ inputs.target }}",
		"${{ matrix.version }}",
		"${{ secrets.TOKEN }}",
		"${{ vars.RELEASE }}",
		"${{ env.PATH }}",
		"${{ runner.os }}",
		"${{ runner['arch'] }}",
		"${{ steps.build.outputs.release }}",
		"${{ steps.build.outcome }}",
		"${{ steps.build.conclusion }}",
		"${{ needs.build.outputs.release }}",
		"${{ needs.build.result }}",
		"${{ job.services.redis.ports[6379] }}",
		"prefix-${{ github.actor }}-${{ matrix.version }}",
	} {
		t.Run(template, func(t *testing.T) {
			if err := validateRuntimeTemplate(template); err != nil {
				t.Fatalf("validateRuntimeTemplate(%q) error = %v", template, err)
			}
		})
	}

	for _, test := range []struct {
		template string
		want     string
	}{
		{template: "${{ github.server_url == 'https://github.com' && github.token || '' }}", want: "requires a direct context reference"},
		{template: "${{ hashFiles('go.sum') }}", want: "requires a direct context reference"},
		{template: "${{ github }}", want: `unsupported runtime expression "github"`},
		{template: "${{ steps.build.status }}", want: `unsupported runtime expression "steps.build.status"`},
		{template: "${{ github[env.NAME] }}", want: "index must be a string literal"},
		{template: "${{ true || }}", want: "invalid expression"},
	} {
		t.Run(test.template, func(t *testing.T) {
			err := validateRuntimeTemplate(test.template)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateRuntimeTemplate(%q) error = %v, want %q", test.template, err, test.want)
			}
		})
	}
}

func TestRunnerDirectReferencesWorkAcrossRuntimeEvaluationSurfaces(t *testing.T) {
	runner := map[string]string{"os": "macOS", "arch": "ARM64"}
	if got, err := Evaluate("${{ runner.os }}/${{ RUNNER.ARCH }}", Context{Runner: runner}); err != nil || got != "macOS/ARM64" {
		t.Fatalf("Evaluate() = %q, %v", got, err)
	}
	if got, err := EvaluateCondition("runner.os == 'macOS' && runner.arch == 'ARM64'", ConditionContext{Runner: runner}); err != nil || !got {
		t.Fatalf("EvaluateCondition() = %v, %v", got, err)
	}
	if got, err := EvaluateActionInputDefault("${{ runner.os == 'macOS' && runner.arch || 'X64' }}", Context{Runner: runner}); err != nil || got != "ARM64" {
		t.Fatalf("EvaluateActionInputDefault() = %q, %v", got, err)
	}
	for _, reference := range []string{"runner.name", "runner.os.extra", "runner"} {
		if err := validateRuntimeTemplate("${{ " + reference + " }}"); err == nil {
			t.Errorf("validateRuntimeTemplate(%q) unexpectedly succeeded", reference)
		}
	}
}

func TestValidateActionInputDefaultSupportsRestrictedCompoundExpressions(t *testing.T) {
	for _, template := range []string{
		"${{ github.server_url == 'https://github.com' && github.token || '' }}",
		"${{ job.status }}",
		"${{ toJSON(matrix) }}",
		"${{ true && 'quoted }} braces' || '' }}",
	} {
		if err := ValidateActionInputDefault(template); err != nil {
			t.Errorf("ValidateActionInputDefault(%q) error = %v", template, err)
		}
	}
	for _, template := range []string{"${{ secrets.TOKEN }}", "${{ hashFiles('go.sum') }}", "${{ toJSON(secrets) }}", "${{ toJSON(matrix.value) }}", "${{ 1 > 0 }}", "${{ github[env.NAME] }}", "${{ job.status == 'success' }}", "status-${{ job.status }}"} {
		if err := ValidateActionInputDefault(template); err == nil {
			t.Errorf("ValidateActionInputDefault(%q) unexpectedly succeeded", template)
		}
	}
}

func TestEvaluateActionInputDefaultSupportsConditionalValueExpressions(t *testing.T) {
	template := "${{ github.server_url == 'https://github.com' && github.token || '' }}"
	for _, test := range []struct {
		name    string
		github  map[string]any
		want    string
		wantErr string
	}{
		{name: "GitHub.com token", github: map[string]any{"server_url": "https://github.com", "token": "ghs_scoped"}, want: "ghs_scoped"},
		{name: "case insensitive URL", github: map[string]any{"server_url": "HTTPS://GITHUB.COM", "token": "ghs_scoped"}, want: "ghs_scoped"},
		{name: "GHES empty token", github: map[string]any{"server_url": "https://github.example.com"}},
		{name: "GitHub.com missing token", github: map[string]any{"server_url": "https://github.com"}, wantErr: `unavailable github value "token"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateActionInputDefault(template, Context{GitHub: test.github})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Evaluate() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("Evaluate() = %q, %v, want %q", got, err, test.want)
			}
		})
	}
	if _, err := Evaluate(template, Context{}); err == nil {
		t.Fatal("Evaluate() accepted action-default compound expression outside action metadata")
	}
}

func TestActionInputDefaultRequiresGitHubTokenUsesProviderServerURL(t *testing.T) {
	for _, test := range []struct {
		name      string
		template  string
		serverURL string
		want      bool
	}{
		{name: "direct GitHub.com token", template: "${{ github.token }}", serverURL: "https://github.com", want: true},
		{name: "direct Origin token", template: "${{ github.token }}", serverURL: "https://origin.cursor.com", want: true},
		{name: "GitHub.com guarded token", template: "${{ github.server_url == 'https://github.com' && github.token || '' }}", serverURL: "https://github.com", want: true},
		{name: "Origin skips GitHub.com token", template: "${{ github.server_url == 'https://github.com' && github.token || '' }}", serverURL: "https://origin.cursor.com"},
		{name: "Origin reaches reverse guard", template: "${{ github.server_url != 'https://github.com' && github.token || '' }}", serverURL: "https://origin.cursor.com", want: true},
		{name: "literal false skips token", template: "${{ false && github.token || '' }}", serverURL: "https://origin.cursor.com"},
		{name: "unknown guard fails closed", template: "${{ inputs.use_token && github.token || '' }}", serverURL: "https://origin.cursor.com", want: true},
		{name: "no token reference", template: "${{ github.server_url }}", serverURL: "https://origin.cursor.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ActionInputDefaultRequiresGitHubToken(test.template, test.serverURL)
			if err != nil || got != test.want {
				t.Fatalf("ActionInputDefaultRequiresGitHubToken() = %v, %v, want %v", got, err, test.want)
			}
		})
	}
	if _, err := ActionInputDefaultRequiresGitHubToken("${{ github[env.NAME] }}", "https://origin.cursor.com"); err == nil || !strings.Contains(err.Error(), "index must be a string literal") {
		t.Fatalf("ActionInputDefaultRequiresGitHubToken() error = %v, want dynamic index rejection", err)
	}
}

func TestEvaluateActionInputDefaultSupportsJobStatus(t *testing.T) {
	references, err := ReferencesJobStatus("${{ job.status }}")
	if err != nil || !references {
		t.Fatalf("ReferencesJobStatus() = %v, %v", references, err)
	}
	for _, status := range []string{"success", "failure", "cancelled"} {
		got, err := EvaluateActionInputDefault("${{ job.status }}", Context{JobStatus: status})
		if err != nil || got != status {
			t.Errorf("EvaluateActionInputDefault(job.status = %q) = %q, %v", status, got, err)
		}
	}
	if _, err := EvaluateActionInputDefault("${{ job.status }}", Context{}); err == nil {
		t.Fatal("EvaluateActionInputDefault() accepted unavailable job.status")
	}
	if _, err := Evaluate("${{ job.status }}", Context{JobStatus: "success"}); err == nil {
		t.Fatal("Evaluate() accepted action-default-only job.status")
	}
}

func TestEvaluateActionInputDefaultSupportsMatrixJSON(t *testing.T) {
	got, err := EvaluateActionInputDefault("${{ toJSON(matrix) }}", Context{Matrix: map[string]any{"scala": "2.13", "jdk": 17}})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"jdk\": 17,\n  \"scala\": \"2.13\"\n}"
	if got != want {
		t.Fatalf("EvaluateActionInputDefault(toJSON(matrix)) = %q, want %q", got, want)
	}
}

func TestEvaluateActionInputDefaultMatchesGitHubEqualityAndTemplateBoundaries(t *testing.T) {
	for _, test := range []struct {
		name     string
		template string
		context  Context
		want     string
	}{
		{name: "quoted closing delimiter", template: "${{ true && 'quoted }} braces' || '' }}", want: "quoted }} braces"},
		{name: "numeric string", template: "${{ matrix.version == '20' && 'yes' || 'no' }}", context: Context{Matrix: map[string]any{"version": 20}}, want: "yes"},
		{name: "null and zero", template: "${{ null == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "false and zero", template: "${{ false == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "empty string and zero", template: "${{ '' == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "not equal", template: "${{ 'not-a-number' != 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "falsy zero", template: "${{ 0 && 'yes' || 'no' }}", want: "no"},
		{name: "truthy short circuit", template: "${{ 'fallback' || github.missing }}", want: "fallback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateActionInputDefault(test.template, test.context)
			if err != nil || got != test.want {
				t.Fatalf("EvaluateActionInputDefault() = %q, %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestEvaluateIsSinglePass(t *testing.T) {
	literal := "literal ${{ matrix.secret }} and ${{"
	context := Context{
		Inputs:       map[string]string{"value": literal},
		Matrix:       map[string]any{"value": literal, "secret": "reevaluated", "number": json.Number("1e3")},
		Steps:        map[string]map[string]string{"producer": {"value": literal}},
		StepStatuses: map[string]StepStatus{"producer": {Outcome: "failure", Conclusion: "success"}},
		Needs:        map[string]map[string]string{"producer": {"value": literal}},
		NeedResults:  map[string]string{"producer": "success"},
	}
	tests := map[string]string{
		"${{ inputs.value }}":                 literal,
		"${{ matrix.value }}":                 literal,
		"${{ steps.Producer.outputs.value }}": literal,
		"${{ steps.Producer.outcome }}":       "failure",
		"${{ steps.Producer.conclusion }}":    "success",
		"${{ needs.Producer.outputs.value }}": literal,
		"${{ needs.Producer.result }}":        "success",
		"${{ matrix.number }}":                "1e3",
		"before ${{ inputs.value }} after":    "before " + literal + " after",
	}
	for template, want := range tests {
		got, err := Evaluate(template, context)
		if err != nil {
			t.Fatalf("Evaluate(%q) error = %v", template, err)
		}
		if got != want {
			t.Errorf("Evaluate(%q) = %q, want %q", template, got, want)
		}
	}

	got, err := Evaluate("${{ inputs.value }}:${{ matrix.secret }}", context)
	if err != nil || got != literal+":reevaluated" {
		t.Fatalf("Evaluate() = %q, %v, want both original expressions evaluated once", got, err)
	}
}

func TestEvaluateSupportsStaticIndexReferences(t *testing.T) {
	context := Context{
		Matrix:  map[string]any{"shell": "bash"},
		Secrets: map[string]string{"TOKEN": "secret-value"},
	}
	got, err := Evaluate("${{ matrix['shell'] }}:${{ secrets['TOKEN'] }}", context)
	if err != nil {
		t.Fatal(err)
	}
	if got != "bash:secret-value" {
		t.Fatalf("Evaluate() = %q, want static index references", got)
	}
	if _, err := Evaluate("${{ secrets[env.NAME] }}", context); err == nil || !strings.Contains(err.Error(), "index must be a string literal") {
		t.Fatalf("Evaluate() dynamic index error = %v", err)
	}
}

func TestServicePortIsTheOnlyRuntimeJobExpression(t *testing.T) {
	services := map[string]map[string]string{"redis": {"6379": "49152"}}
	context := Context{Services: services, Env: map[string]string{"PORT": "6379"}}
	for _, reference := range []string{"job.services.redis.ports[6379]", "JOB.Services.REDIS.Ports[6379]", "job.services.REDIS.ports['6379']"} {
		got, err := Evaluate("${{ "+reference+" }}", context)
		if err != nil || got != "49152" {
			t.Fatalf("Evaluate(%q) = %q, %v", reference, got, err)
		}
	}
	if got, err := EvaluateCondition("job.services.Redis.ports[6379] == '49152'", ConditionContext{Services: services}); err != nil || !got {
		t.Fatalf("service condition = %v, %v", got, err)
	}
	for _, reference := range []string{"job.services.missing.ports[6379]", "job.services.redis.ports[1234]", "job.services.redis.ports[env.PORT]", "job.status"} {
		if _, err := Evaluate("${{ "+reference+" }}", context); err == nil {
			t.Errorf("Evaluate(%q) unexpectedly succeeded", reference)
		}
	}
	for _, reference := range []string{"matrix[0]", "env[0]", "github.event.items[0]"} {
		if _, err := Evaluate("${{ "+reference+" }}", context); err == nil || !strings.Contains(err.Error(), "does not support numeric indices") {
			t.Errorf("Evaluate(%q) numeric index error = %v", reference, err)
		}
	}
	expr, err := Parse("${{ job.services.redis.ports[6379] }}", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateCompile(expr, CompileContext{}); err == nil {
		t.Fatal("compile-time job context unexpectedly succeeded")
	}
}

func TestSecretReferencesUsesExpressionAST(t *testing.T) {
	template := "${{ secrets.dot_token }}:${{ secrets['BRACKET_TOKEN'] }}:${{ secrets.DOT_TOKEN }}:${{ matrix.value }}:${{ 'not }} a delimiter' }}"
	got, err := SecretReferences(template)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"BRACKET_TOKEN", "DOT_TOKEN"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretReferences() = %#v, want %#v", got, want)
	}
	if _, err := SecretReferences("${{ secrets[env.NAME] }}"); err == nil || !strings.Contains(err.Error(), "index must be a string literal") {
		t.Fatalf("SecretReferences() dynamic index error = %v", err)
	}
	if _, err := SecretReferences("${{ toJSON(secrets) }}"); err == nil || !strings.Contains(err.Error(), "must name exactly one secret") {
		t.Fatalf("SecretReferences() whole-context error = %v", err)
	}
}

func TestReferencesGitHubTokenUsesExpressionAST(t *testing.T) {
	for _, test := range []struct {
		name     string
		template string
		want     bool
	}{
		{name: "dot", template: "${{ github.token }}", want: true},
		{name: "bracket", template: "prefix-${{ github['TOKEN'] }}", want: true},
		{name: "compound", template: "${{ github.token || '' }}", want: true},
		{name: "other GitHub value", template: "${{ github.actor }}"},
		{name: "plain", template: "github.token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ReferencesGitHubToken(test.template)
			if err != nil || got != test.want {
				t.Fatalf("ReferencesGitHubToken(%q) = %v, %v, want %v", test.template, got, err, test.want)
			}
		})
	}
	if _, err := ReferencesGitHubToken("${{ github[env.NAME] }}"); err == nil || !strings.Contains(err.Error(), "index must be a string literal") {
		t.Fatalf("ReferencesGitHubToken() dynamic index error = %v", err)
	}
	if _, err := ReferencesGitHubToken("${{ github.token.extra }}"); err == nil || !strings.Contains(err.Error(), "must name exactly github.token") {
		t.Fatalf("ReferencesGitHubToken() token dereference error = %v", err)
	}
}

func TestConditionUsesContextSupportsOptionalDelimiters(t *testing.T) {
	for _, condition := range []string{"inputs.enabled", "${{ inputs.enabled }}", "inputs.enabled && github.ref"} {
		usesInputs, err := ConditionUsesContext(condition, "inputs")
		if err != nil || !usesInputs {
			t.Fatalf("ConditionUsesContext(%q) = %v, %v, want true", condition, usesInputs, err)
		}
	}
	usesInputs, err := ConditionUsesContext("github.ref", "inputs")
	if err != nil || usesInputs {
		t.Fatalf("ConditionUsesContext(github.ref) = %v, %v, want false", usesInputs, err)
	}
}

func TestValidateConditionAllowsSupportedRuntimeExpressions(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		scope  ConditionScope
	}{
		{
			name:   "job dependencies and identity",
			source: "always() && needs.build.result == 'success' && needs.build.outputs.ready && github.ref == 'refs/heads/main' && vars.ENABLED && matrix.os",
			scope:  JobCondition,
		},
		{
			name:   "step status environment and services",
			source: "${{ failure() && steps.test.outcome == 'failure' && steps.test.conclusion == 'success' && steps.test.outputs.ready && env.LEVEL && job.services.redis.ports[6379] }}",
			scope:  StepCondition,
		},
		{name: "github head ref in job", source: "github.head_ref != ''", scope: JobCondition},
		{name: "github head ref in step", source: "github.head_ref != ''", scope: StepCondition},
		{name: "compatible booleans", source: "success() == true", scope: JobCondition},
		{name: "compatible strings", source: "vars.ENABLED == 'true'", scope: JobCondition},
		{name: "compatible integer and float", source: "1 == 1.0", scope: JobCondition},
		{name: "runtime-dependent matrix value", source: "matrix.enabled == true", scope: JobCondition},
		{name: "runner identity", source: "runner.os == 'Linux' && runner.arch == 'X64'", scope: JobCondition},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateCondition(test.source, test.scope); err != nil {
				t.Fatalf("ValidateCondition() error = %v", err)
			}
		})
	}
}

func TestValidateConditionRejectsUnsupportedRuntimeExpressions(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		scope  ConditionScope
		want   string
	}{
		{name: "hashFiles needs a pattern", source: "hashFiles()", scope: StepCondition, want: `condition function "hashFiles" requires 1 to 255 arguments`},
		{name: "hashFiles unavailable in jobs", source: "hashFiles('go.sum')", scope: JobCondition, want: `condition function "hashFiles" is unavailable in job conditions`},
		{name: "unsupported function", source: "contains('a', 'b')", scope: StepCondition, want: `condition function "contains" is unsupported`},
		{name: "function arguments", source: "always(true)", scope: StepCondition, want: `condition function "always" arguments are unsupported`},
		{name: "ordered comparison", source: "matrix.count > 1", scope: JobCondition, want: "condition comparison > is unsupported"},
		{name: "runtime event payload", source: "github.event.pull_request.draft", scope: JobCondition, want: `condition reference "github.event.pull_request.draft" is unavailable at runtime`},
		{name: "unsupported github property", source: "github.run_id", scope: StepCondition, want: `condition reference "github.run_id" is unavailable at runtime`},
		{name: "step context in job", source: "steps.build.outcome", scope: JobCondition, want: `condition context "steps" is unavailable in job conditions`},
		{name: "environment in job", source: "env.ENABLED", scope: JobCondition, want: `condition context "env" is unavailable in job conditions`},
		{name: "unsupported context", source: "secrets.TOKEN", scope: StepCondition, want: `condition context "secrets" is unsupported`},
		{name: "unsupported need shape", source: "needs.build.status", scope: JobCondition, want: `expected needs.<job>.result`},
		{name: "dynamic index", source: "steps[env.STEP].outcome", scope: StepCondition, want: "expression index must be a string literal"},
		{name: "string and boolean equality", source: "vars.ENABLED == true", scope: JobCondition, want: "condition equality compares incompatible string and boolean operands"},
		{name: "boolean and number equality", source: "success() != 1", scope: JobCondition, want: "condition equality compares incompatible boolean and number operands"},
		{name: "malformed", source: "${{ github.ref == }}", scope: JobCondition, want: "parse condition"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCondition(test.source, test.scope)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateCondition() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHashFilesIsLimitedToStepRuntimeExpressions(t *testing.T) {
	hash := func(patterns []string) (string, error) {
		if !reflect.DeepEqual(patterns, []string{"*.go", "!generated/**"}) {
			t.Fatalf("hashFiles patterns = %#v", patterns)
		}
		return "digest", nil
	}
	context := Context{HashFiles: hash}
	if got, err := EvaluateStep("key-${{ hashFiles('*.go', '!generated/**') }}", context); err != nil || got != "key-digest" {
		t.Fatalf("EvaluateStep() = %q, %v", got, err)
	}
	if _, err := Evaluate("${{ hashFiles('*.go') }}", context); err == nil || !strings.Contains(err.Error(), "unsupported expression reference") {
		t.Fatalf("Evaluate() hashFiles error = %v", err)
	}
	if _, err := EvaluateStep("${{ contains('a', 'b') }}", context); err == nil || !strings.Contains(err.Error(), "unsupported expression reference") {
		t.Fatalf("EvaluateStep() contains error = %v", err)
	}

	condition := ConditionContext{HashFiles: hash}
	if got, err := EvaluateCondition("hashFiles('*.go', '!generated/**') != ''", condition); err != nil || !got {
		t.Fatalf("EvaluateCondition() = %v, %v", got, err)
	}
	if err := ValidateCondition("hashFiles('*.go') != ''", StepCondition); err != nil {
		t.Fatalf("ValidateCondition() = %v", err)
	}
}

func TestCompileConditionValidationAdmitsRuntimeHashFilesWithoutFilesystemAccess(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"event": map[string]any{"pull_request": map[string]any{"draft": false}}}}
	source := "github.event.pull_request.draft && hashFiles('go.sum') != ''"
	if err := ValidateCompileConditionWithMatrix(source, StepCondition, context, nil); err != nil {
		t.Fatalf("ValidateCompileConditionWithMatrix() = %v", err)
	}
	resolved, err := EvaluateCompileCondition(source, context)
	if err != nil || resolved {
		t.Fatalf("EvaluateCompileCondition() = %v, %v", resolved, err)
	}
	if _, err := EvaluateCompile(Expression{Text: "${{ hashFiles('go.sum') }}"}, context); err == nil || !strings.Contains(err.Error(), `unsupported compile-time function "hashFiles"`) {
		t.Fatalf("EvaluateCompile() hashFiles error = %v", err)
	}
}

func TestValidateConditionUsesConcreteMatrixTypes(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		matrix map[string]any
		want   string
	}{
		{name: "numeric value", source: "matrix.version == 12", matrix: map[string]any{"version": 14.0}},
		{name: "json numeric value", source: "matrix.version == 12", matrix: map[string]any{"version": json.Number("14")}},
		{name: "boolean value", source: "matrix.experimental == true", matrix: map[string]any{"experimental": false}},
		{name: "string and number", source: "matrix.version == 12", matrix: map[string]any{"version": "14"}, want: "condition equality compares incompatible string and number operands"},
		{name: "null and number", source: "matrix.version == 12", matrix: map[string]any{"version": nil}, want: "condition equality compares incompatible null and number operands"},
		{name: "missing value", source: "matrix.version == 12", matrix: map[string]any{}, want: `condition reference "matrix.version" is unavailable in this matrix instance`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateConditionWithMatrix(test.source, JobCondition, test.matrix)
			if test.want == "" && err != nil {
				t.Fatalf("ValidateCondition() error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("ValidateCondition() error = %v, want %q", err, test.want)
			}
		})
	}
	if err := ValidateCondition("matrix.version == 12", JobCondition); err != nil {
		t.Fatalf("ValidateCondition() rejected unknown matrix type: %v", err)
	}
}

func TestValidateConditionAcceptsCompilerNumericKinds(t *testing.T) {
	values := []any{
		int(-1), int8(-1), int16(-1), int32(-1), int64(-1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1), ^uint64(0),
		float32(1.5), float64(1.5), json.Number("1e3"),
	}
	for _, value := range values {
		t.Run(reflect.TypeOf(value).String(), func(t *testing.T) {
			if err := ValidateConditionWithMatrix("matrix.value != 0", JobCondition, map[string]any{"value": value}); err != nil {
				t.Fatalf("ValidateConditionWithMatrix(%T) error = %v", value, err)
			}
		})
	}
}

func TestEvaluateFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{name: "unavailable github value", template: "${{ github.sha }}", want: `unavailable github value "sha"`},
		{name: "unavailable matrix value", template: "${{ matrix.missing }}", want: `unavailable matrix value "missing"`},
		{name: "unavailable step", template: "${{ steps.missing.outputs.value }}", want: `unavailable step "missing"`},
		{name: "unavailable step outcome", template: "${{ steps.missing.outcome }}", want: `unavailable step "missing"`},
		{name: "unavailable need", template: "${{ needs.missing.outputs.value }}", want: `unavailable need "missing"`},
		{name: "unavailable need result", template: "${{ needs.missing.result }}", want: `unavailable need "missing"`},
		{name: "unterminated", template: "${{ inputs.value", want: "unterminated expression"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Evaluate(test.template, Context{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Evaluate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEvaluateActionLifecycleCondition(t *testing.T) {
	tests := []struct {
		name         string
		condition    string
		unsuccessful bool
		cancelled    bool
		want         bool
		wantErr      bool
	}{
		{name: "empty means unconditional", condition: "", want: true},
		{name: "empty stays true after failure", condition: "", unsuccessful: true, want: true},
		{name: "empty stays true after cancellation", condition: "", cancelled: true, want: true},
		{name: "whitespace only is empty", condition: "   ", unsuccessful: true, want: true},
		{name: "always", condition: "always()", unsuccessful: true, cancelled: true, want: true},
		{name: "success on success", condition: "success()", want: true},
		{name: "success after failure", condition: "success()", unsuccessful: true, want: false},
		{name: "success after cancellation", condition: "success()", cancelled: true, want: false},
		{name: "failure after failure", condition: "failure()", unsuccessful: true, want: true},
		{name: "cancellation dominates failure", condition: "failure()", unsuccessful: true, cancelled: true, want: false},
		{name: "failure on success", condition: "failure()", want: false},
		{name: "cancelled when cancelled", condition: "cancelled()", cancelled: true, want: true},
		{name: "cancelled on success", condition: "cancelled()", want: false},
		{name: "delimiters unwrap", condition: "${{ failure() }}", unsuccessful: true, want: true},
		{name: "delimiters without spaces", condition: "${{always()}}", cancelled: true, want: true},
		{name: "case is insensitive", condition: "ALWAYS()", want: true},
		{name: "surrounding whitespace trims", condition: "  success()  ", want: true},
		{name: "literals fail closed", condition: "true", wantErr: true},
		{name: "references fail closed", condition: "github.event_name == 'push'", wantErr: true},
		{name: "compound expressions fail closed", condition: "success() || failure()", wantErr: true},
		{name: "arguments fail closed", condition: "success('build')", wantErr: true},
		{name: "unknown functions fail closed", condition: "finished()", wantErr: true},
		{name: "unopened delimiter fails closed", condition: "failure() }}", wantErr: true},
		{name: "unclosed delimiter fails closed", condition: "${{ failure()", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateActionLifecycleCondition(test.condition, test.unsuccessful, test.cancelled)
			if test.wantErr {
				if err == nil || got {
					t.Fatalf("EvaluateActionLifecycleCondition(%q) = %v, %v, want false with error", test.condition, got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("EvaluateActionLifecycleCondition(%q, unsuccessful=%v, cancelled=%v) = %v, %v, want %v", test.condition, test.unsuccessful, test.cancelled, got, err, test.want)
			}
		})
	}
}

func TestEvaluateConditionStatusOutputsAndTruthiness(t *testing.T) {
	context := ConditionContext{
		Inputs:      map[string]string{"enabled": "true"},
		Needs:       map[string]map[string]string{"build": {"gate": "yes"}},
		NeedResults: map[string]string{"build": "failure"},
		Steps:       map[string]StepStatus{"soft": {Outcome: "failure", Conclusion: "success", Outputs: map[string]string{"ready": "true"}}},
		Failure:     true,
	}
	for _, condition := range []string{
		"inputs.enabled == 'true'",
		"always() && needs.build.result == 'failure' && needs.build.outputs.gate == 'yes'",
		"failure() && steps.soft.outcome == 'failure' && steps.soft.conclusion == 'success'",
		"always() && steps.soft.outputs.ready",
	} {
		got, err := EvaluateCondition(condition, context)
		if err != nil || !got {
			t.Fatalf("EvaluateCondition(%q) = %v, %v", condition, got, err)
		}
	}
	for _, condition := range []string{"''", "0", "false", "null"} {
		got, err := EvaluateCondition(condition, ConditionContext{})
		if err != nil || got {
			t.Fatalf("EvaluateCondition(%q) = %v, %v, want false", condition, got, err)
		}
	}
}

func TestEvaluateConditionInputsMatchNormalExpressionSemantics(t *testing.T) {
	context := ConditionContext{Inputs: map[string]string{"enabled": "true"}}
	for condition, want := range map[string]bool{
		"inputs.enabled == 'true'": true,
		"INPUTS.ENABLED == 'true'": true,
		"inputs.missing":           false,
	} {
		got, err := EvaluateCondition(condition, context)
		if err != nil || got != want {
			t.Errorf("EvaluateCondition(%q) = %v, %v, want %v", condition, got, err, want)
		}
	}
}

func TestEvaluateConditionSupportsJSONNumbers(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		matrix    map[string]any
		want      bool
	}{
		{name: "json number and int", condition: "matrix.value == 1", matrix: map[string]any{"value": json.Number("1")}, want: true},
		{name: "json number and float", condition: "matrix.value == 1.5", matrix: map[string]any{"value": json.Number("1.5")}, want: true},
		{name: "json numbers", condition: "matrix.left == matrix.right", matrix: map[string]any{"left": json.Number("1e3"), "right": json.Number("1000")}, want: true},
		{name: "nonzero truthiness", condition: "matrix.value", matrix: map[string]any{"value": json.Number("0.25")}, want: true},
		{name: "zero truthiness", condition: "matrix.value", matrix: map[string]any{"value": json.Number("0")}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateCondition(test.condition, ConditionContext{Matrix: test.matrix})
			if err != nil || got != test.want {
				t.Fatalf("EvaluateCondition(%q) = %v, %v, want %v", test.condition, got, err, test.want)
			}
		})
	}
}

func TestEvaluateConditionFailsClosed(t *testing.T) {
	if _, err := EvaluateCondition("1 < 2", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() accepted unsupported ordered comparison")
	}
	if _, err := EvaluateCondition("true == 'true'", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() silently coerced mixed equality operands")
	}
	if _, err := EvaluateCondition("null == true", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() accepted mixed null equality operands")
	}
	if got, err := EvaluateCondition("", ConditionContext{Unsuccessful: true}); err != nil || got {
		t.Fatalf("default condition after skipped prerequisite = %v, %v, want false", got, err)
	}
	hashCalled := false
	if got, err := EvaluateCondition("hashFiles('**') != ''", ConditionContext{Unsuccessful: true, HashFiles: func([]string) (string, error) {
		hashCalled = true
		return "digest", nil
	}}); err != nil || got || hashCalled {
		t.Fatalf("implicit success guard = %v, %v, hash called %v; want false, nil, false", got, err, hashCalled)
	}
	if _, err := EvaluateCondition("needs.missing.result", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() accepted an unavailable need result")
	}
	if _, err := EvaluateCondition("inputs.enabled", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() accepted inputs outside an action context")
	}
}

func TestEvaluateCompileSupportsGraphContextsAndFromJSON(t *testing.T) {
	context := CompileContext{
		GitHub: map[string]any{"event_name": "push", "event": map[string]any{"action": "opened"}},
		Event:  map[string]any{"action": "opened"},
		Vars:   map[string]string{"RUNNERS": `["ubuntu-24.04","ubuntu-22.04"]`},
		Matrix: map[string]any{"os": "ubuntu-24.04"},
	}
	tests := []struct {
		expression string
		want       any
	}{
		{expression: "${{ github.event_name }}", want: "push"},
		{expression: "${{ github.event.action }}", want: "opened"},
		{expression: "${{ event.action }}", want: "opened"},
		{expression: "${{ matrix.os }}", want: "ubuntu-24.04"},
		{expression: "${{ github.event.number || github.ref }}", want: "refs/pull/42/merge"},
		{expression: "${{ github.ref == 'refs/pull/42/merge' }}", want: true},
		{expression: "${{ startsWith(github.ref, 'REFS/PULL/') }}", want: true},
		{expression: "${{ contains(github.ref, 'PULL/42') }}", want: true},
		{expression: "${{ contains(github.ref, 'ISSUES') }}", want: false},
		{expression: "${{ endsWith(github.ref, '/MERGE') }}", want: true},
		{expression: "${{ endsWith(github.ref, '/HEAD') }}", want: false},
	}
	context.GitHub["ref"] = "refs/pull/42/merge"
	context.GitHub["event"] = map[string]any{"action": "opened", "number": json.Number("0")}
	for _, test := range tests {
		expr, err := Parse(test.expression, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := EvaluateCompile(expr, context)
		if err != nil {
			t.Fatalf("EvaluateCompile(%q) error = %v", test.expression, err)
		}
		if got != test.want {
			t.Fatalf("EvaluateCompile(%q) = %#v, want %#v", test.expression, got, test.want)
		}
	}

	expr, err := Parse("${{ fromJSON(vars.RUNNERS) }}", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateCompile(expr, context)
	if err != nil {
		t.Fatal(err)
	}
	runners, ok := got.([]any)
	if !ok || len(runners) != 2 || runners[0] != "ubuntu-24.04" {
		t.Fatalf("fromJSON runners = %#v", got)
	}
}

func TestEvaluateCompileConditionUsesEventSnapshot(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{
		"event_name": "pull_request",
		"event":      map[string]any{"pull_request": map[string]any{"draft": false}},
	}}
	got, err := EvaluateCompileCondition("github.event_name == 'pull_request' && github.event.pull_request.draft", context)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("draft condition evaluated true for a non-draft event")
	}
	if _, err := EvaluateCompileCondition("needs.build.result == 'success'", context); err == nil || !strings.Contains(err.Error(), `unsupported compile-time context "needs"`) {
		t.Fatalf("runtime condition error = %v", err)
	}
	usesEvent, err := ReferencesGitHubEvent("github.event_name == 'pull_request' && github.event.pull_request.draft")
	if err != nil || !usesEvent {
		t.Fatalf("ReferencesGitHubEvent() = %v, %v", usesEvent, err)
	}
	if usesEvent, err := ReferencesGitHubEvent("github.event_name == 'push'"); err != nil || usesEvent {
		t.Fatalf("ReferencesGitHubEvent(event_name) = %v, %v", usesEvent, err)
	}
}

func TestCompileConditionValidationSupportsStringPredicates(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{
		"event": map[string]any{"pull_request": map[string]any{"title": "Release is READY"}},
	}}
	source := "contains(github.event.pull_request.title, 'release') && endsWith(github.event.pull_request.title, 'ready')"
	if err := ValidateCompileConditionWithMatrix(source, JobCondition, context, nil); err != nil {
		t.Fatalf("ValidateCompileConditionWithMatrix() = %v", err)
	}
	if resolved, err := EvaluateCompileCondition(source, context); err != nil || !resolved {
		t.Fatalf("EvaluateCompileCondition() = %v, %v, want true", resolved, err)
	}
}

func TestCompileInputLiteralRepresentations(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr string
	}{
		{name: "nil is the null literal", value: nil, want: "null"},
		{name: "true", value: true, want: "true"},
		{name: "false", value: false, want: "false"},
		{name: "plain string is quoted", value: "ready", want: "'ready'"},
		{name: "apostrophes escape by doubling", value: "it's ready", want: "'it''s ready'"},
		{name: "empty string stays a literal", value: "", want: "''"},
		{name: "json number preserves source text", value: json.Number("0.30"), want: "0.30"},
		{name: "int", value: 42, want: "42"},
		{name: "float64 uses shortest form", value: 2.5, want: "2.5"},
		{name: "aggregate values cannot be literals", value: []any{"x"}, wantErr: "cannot be represented"},
		{name: "maps cannot be literals", value: map[string]any{"x": "y"}, wantErr: "cannot be represented"},
		{name: "typed numerics outside the YAML model fail closed", value: int32(7), wantErr: "cannot be represented"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := compileInputLiteral(test.value)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("compileInputLiteral(%#v) error = %v, want %q", test.value, err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("compileInputLiteral(%#v) = %q, %v, want %q", test.value, got, err, test.want)
			}
		})
	}
}

func TestSubstituteCompileInputsPreservesExpressionSyntax(t *testing.T) {
	template := "${{ !inputs.enabled && matrix.run-new && 'inputs.enabled' || inputs.label }}"
	got, err := SubstituteCompileInputs(template, map[string]any{"enabled": false, "label": "it''s ready"})
	if err != nil {
		t.Fatal(err)
	}
	want := "${{ !false && matrix.run-new && 'inputs.enabled' || 'it''''s ready' }}"
	if got != want {
		t.Fatalf("SubstituteCompileInputs() = %q, want %q", got, want)
	}
}

func TestEvaluateAvailableCompileTemplatePreservesRuntimeExpressions(t *testing.T) {
	got, err := EvaluateAvailableCompileTemplate("echo ${{ 'target' }} ${{ github.ref }}", CompileContext{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "echo target ${{ github.ref }}"; got != want {
		t.Fatalf("EvaluateAvailableCompileTemplate() = %q, want %q", got, want)
	}
}

func TestEvaluateCompileFailsClosed(t *testing.T) {
	tests := []struct {
		expression string
		want       string
	}{
		{expression: "${{ secrets.TOKEN }}", want: `unsupported compile-time context "secrets"`},
		{expression: "${{ vars.MISSING }}", want: `unavailable value "vars.missing"`},
		{expression: "${{ hashFiles('go.sum') }}", want: `unsupported compile-time function "hashFiles"`},
		{expression: "${{ startsWith(github.ref) }}", want: `unsupported compile-time function "startsWith"`},
		{expression: "${{ contains(github.ref) }}", want: `unsupported compile-time function "contains"`},
		{expression: "${{ endsWith('ref', true) }}", want: "endsWith arguments resolved to string and bool, want strings"},
		{expression: "${{ contains(fromJSON('[\"push\"]'), 'push') }}", want: "contains arguments resolved to []interface {} and string, want strings"},
		{expression: "${{ fromJSON(vars.BAD) }}", want: "invalid JSON"},
		{expression: "${{ event.Ref }}", want: "ambiguous properties"},
	}
	for _, test := range tests {
		t.Run(test.expression, func(t *testing.T) {
			expr, err := Parse(test.expression, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			_, err = EvaluateCompile(expr, CompileContext{Vars: map[string]string{"BAD": "["}, Event: map[string]any{"ref": "one", "REF": "two"}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("EvaluateCompile() error = %v, want %q", err, test.want)
			}
		})
	}
}
