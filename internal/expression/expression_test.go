package expression

import (
	"encoding/json"
	"math"
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
			if err := ValidateRuntimeTemplate(template); err != nil {
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
			err := ValidateRuntimeTemplate(test.template)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateRuntimeTemplate(%q) error = %v, want %q", test.template, err, test.want)
			}
		})
	}
}

func TestRunnerDirectReferencesWorkAcrossRuntimeEvaluationSurfaces(t *testing.T) {
	runner := map[string]string{"os": "macOS", "arch": "ARM64", "temp": "/runner/temp"}
	if got, err := Evaluate("${{ runner.os }}/${{ RUNNER.ARCH }}", Context{Runner: runner}); err != nil || got != "macOS/ARM64" {
		t.Fatalf("Evaluate() = %q, %v", got, err)
	}
	if got, err := EvaluateStep("${{ runner.temp }}", Context{Runner: runner}); err != nil || got != "/runner/temp" {
		t.Fatalf("EvaluateStep() runner.temp = %q, %v", got, err)
	}
	if got, err := EvaluateCondition("runner.os == 'macOS' && runner.arch == 'ARM64'", ConditionContext{Runner: runner}); err != nil || !got {
		t.Fatalf("EvaluateCondition() = %v, %v", got, err)
	}
	if err := ValidateCondition("runner.temp != ''", StepCondition); err != nil {
		t.Fatalf("ValidateCondition() step runner.temp = %v", err)
	}
	if err := ValidateCondition("runner.temp != ''", JobCondition); err == nil {
		t.Fatal("ValidateCondition() accepted runner.temp before job setup")
	}
	if got, err := EvaluateActionInputDefault("${{ runner.os == 'macOS' && runner.arch || 'X64' }}", Context{Runner: runner}); err != nil || got != "ARM64" {
		t.Fatalf("EvaluateActionInputDefault() = %q, %v", got, err)
	}
	for _, reference := range []string{"runner.name", "runner.os.extra", "runner"} {
		if err := ValidateRuntimeTemplate("${{ " + reference + " }}"); err == nil {
			t.Errorf("validateRuntimeTemplate(%q) unexpectedly succeeded", reference)
		}
	}
}

func TestValidateActionInputDefaultSupportsRestrictedCompoundExpressions(t *testing.T) {
	for _, template := range []string{
		"${{ github.server_url == 'https://github.com' && github.token || '' }}",
		"${{ runner.debug }}",
		"${{ runner.debug == '1' }}",
		"${{ job.status }}",
		"${{ toJSON(matrix) }}",
		"${{ true && 'quoted }} braces' || '' }}",
		"${{ 1 > 0 }}",
	} {
		if err := ValidateActionInputDefault(template); err != nil {
			t.Errorf("ValidateActionInputDefault(%q) error = %v", template, err)
		}
	}
	for _, template := range []string{"${{ secrets.TOKEN }}", "${{ hashFiles('go.sum') }}", "${{ toJSON(secrets) }}", "${{ github[env.NAME] }}", "${{ runner['debug'] }}", "${{ runner[env.NAME] }}", "${{ runner }}", "${{ runner.debug.extra }}", "${{ runner.name }}", "${{ runner.temp }}", "${{ job.status == 'success' }}", "status-${{ job.status }}"} {
		if err := ValidateActionInputDefault(template); err == nil {
			t.Errorf("ValidateActionInputDefault(%q) unexpectedly succeeded", template)
		}
	}
	if _, err := EvaluateActionInputDefault("${{ runner.temp || '' }}", Context{Runner: map[string]string{"temp": "/runner/temp"}}); err == nil {
		t.Fatal("EvaluateActionInputDefault() accepted runner.temp")
	}
}

func TestEvaluateActionInputDefaultTreatsRunnerDebugAsFalse(t *testing.T) {
	for _, test := range []struct {
		template string
		want     string
	}{
		{template: "${{ runner.debug }}", want: "false"},
		{template: "${{ runner.debug == '1' }}", want: "false"},
	} {
		got, err := EvaluateActionInputDefault(test.template, Context{})
		if err != nil || got != test.want {
			t.Errorf("EvaluateActionInputDefault(%q) = %q, %v; want %q", test.template, got, err, test.want)
		}
	}
}

func TestRunnerDebugRemainsUnavailableOutsideActionInputDefaults(t *testing.T) {
	template := "${{ runner.debug }}"
	if err := ValidateRuntimeTemplate(template); err == nil {
		t.Fatal("ValidateRuntimeTemplate() accepted runner.debug")
	}
	if _, err := Evaluate(template, Context{}); err == nil {
		t.Fatal("Evaluate() accepted runner.debug")
	}
	if _, err := EvaluateStep(template, Context{}); err == nil {
		t.Fatal("EvaluateStep() accepted runner.debug")
	}
	if _, err := EvaluateCondition("runner.debug", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() accepted runner.debug")
	}
	if _, err := EvaluateActionLifecycleCondition("runner.debug", ConditionContext{}); err == nil {
		t.Fatal("EvaluateActionLifecycleCondition() accepted runner.debug")
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
		{name: "unknown guard requires token", template: "${{ inputs.use_token && github.token || '' }}", serverURL: "https://origin.cursor.com", want: true},
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
		{name: "matrix numeric string", template: "${{ matrix.version == '20' && 'yes' || 'no' }}", context: Context{Matrix: map[string]any{"version": 20}}, want: "yes"},
		{name: "null and zero", template: "${{ null == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "false and zero", template: "${{ false == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "empty string and zero", template: "${{ '' == 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "numeric string", template: "${{ '12' == 12 && 'yes' || 'no' }}", want: "yes"},
		{name: "same-type strings are not numerically coerced", template: "${{ '01' == '1' && 'yes' || 'no' }}", want: "no"},
		{name: "case-insensitive string equality", template: "${{ 'Release' == 'release' && 'yes' || 'no' }}", want: "yes"},
		{name: "not equal", template: "${{ 'not-a-number' != 0 && 'yes' || 'no' }}", want: "yes"},
		{name: "ordered numeric strings", template: "${{ '12' > 2 && 'yes' || 'no' }}", want: "yes"},
		{name: "case-insensitive string ordering", template: "${{ 'Beta' > 'alpha' && 'yes' || 'no' }}", want: "yes"},
		{name: "NaN ordering is false", template: "${{ 'not-a-number' > 0 && 'yes' || 'no' }}", want: "no"},
		{name: "falsy zero", template: "${{ 0 && 'yes' || 'no' }}", want: "no"},
		{name: "and returns selected operand", template: "${{ 'left' && 'right' }}", want: "right"},
		{name: "or returns selected operand", template: "${{ '' || 'fallback' }}", want: "fallback"},
		{name: "truthy short circuit", template: "${{ 'fallback' || github.missing }}", want: "fallback"},
		{name: "falsy short circuit", template: "${{ false && github.missing || 'fallback' }}", want: "fallback"},
		{name: "missing member is null", template: "${{ matrix.missing == null && 'yes' || 'no' }}", context: Context{Matrix: map[string]any{}}, want: "yes"},
		{name: "primitive string conversion", template: "${{ startsWith(123, '12') }}", want: "true"},
		{name: "format", template: "${{ format('{0}-{1}', 'release', 2) }}", want: "release-2"},
		{name: "JSON number formatting", template: "${{ format('{0}', fromJSON('1e2')) }}", want: "100"},
		{name: "JSON exponent formatting", template: "${{ format('{0}', fromJSON('1e20')) }}", want: "1E+20"},
		{name: "JSON negative zero formatting", template: "${{ format('{0}', fromJSON('-0')) }}", want: "0"},
		{name: "array membership", template: "${{ contains(fromJSON('[\"push\",2]'), 2) }}", want: "true"},
		{name: "join", template: "${{ join(fromJSON('[\"one\",2]'), '-') }}", want: "one-2"},
		{name: "lazy empty join separator", template: "${{ join(fromJSON('[]'), fromJSON('bad')) }}", want: ""},
		{name: "lazy single join separator", template: "${{ join(fromJSON('[\"one\"]'), fromJSON('bad')) }}", want: "one"},
		{name: "lazy case", template: "${{ case(true, 'selected', github.missing) }}", want: "selected"},
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
		Inputs: map[string]string{"value": literal},
		Matrix: map[string]any{"value": literal, "secret": "reevaluated", "number": json.Number("1e3")},
		Steps:  map[string]StepStatus{"producer": {Outcome: "failure", Conclusion: "success", Outputs: map[string]string{"value": literal}}},
		Needs:  map[string]NeedStatus{"producer": {Outputs: map[string]string{"value": literal}, Result: "success"}},
	}
	tests := map[string]string{
		"${{ inputs.value }}":                 literal,
		"${{ matrix.value }}":                 literal,
		"${{ steps.Producer.outputs.value }}": literal,
		"${{ steps.Producer.outcome }}":       "failure",
		"${{ steps.Producer.conclusion }}":    "success",
		"${{ needs.Producer.outputs.value }}": literal,
		"${{ needs.Producer.result }}":        "success",
		"${{ matrix.number }}":                "1000",
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

func TestServiceRuntimeContext(t *testing.T) {
	services := map[string]ServiceContext{"redis": {ID: "container-id", Network: "job-network", Ports: map[string]string{"6379": "49152"}}}
	context := Context{Services: services, Env: map[string]string{"PORT": "6379"}}
	for _, reference := range []string{"job.services.redis.ports[6379]", "JOB.Services.REDIS.Ports[6379]", "job.services.REDIS.ports['6379']"} {
		got, err := Evaluate("${{ "+reference+" }}", context)
		if err != nil || got != "49152" {
			t.Fatalf("Evaluate(%q) = %q, %v", reference, got, err)
		}
	}
	for reference, want := range map[string]string{"job.services.redis.id": "container-id", "job.services.redis.network": "job-network"} {
		got, err := Evaluate("${{ "+reference+" }}", context)
		if err != nil || got != want {
			t.Fatalf("Evaluate(%q) = %q, %v; want %q", reference, got, err, want)
		}
	}
	for template, want := range map[string]string{
		"${{ job.services['redis'].id }}":                      "container-id",
		"${{ format('{0}', job.services.redis.ports[6379]) }}": "49152",
	} {
		if got, err := EvaluateStep(template, context); err != nil || got != want {
			t.Fatalf("EvaluateStep(%q) = %q, %v; want %q", template, got, err, want)
		}
	}
	if _, err := EvaluateStep("${{ job.services[env.NAME].id }}", context); err == nil {
		t.Fatal("EvaluateStep() accepted dynamic service access")
	}
	if got, err := EvaluateCondition("job.services.Redis.ports[6379] == '49152'", ConditionContext{Services: services}); err != nil || !got {
		t.Fatalf("service condition = %v, %v", got, err)
	}
	if got, err := EvaluateCondition("job.services.redis.id == 'container-id' && job.services.redis.network == 'job-network'", ConditionContext{Services: services}); err != nil || !got {
		t.Fatalf("service identity condition = %v, %v", got, err)
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
	for _, template := range []string{"${{ github }}", "${{ github.* }}", "${{ github.event.*.token }}"} {
		if _, err := ReferencesGitHubToken(template); err == nil || !strings.Contains(err.Error(), "must name one static property") {
			t.Fatalf("ReferencesGitHubToken(%q) error = %v, want static-property rejection", template, err)
		}
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

func TestTemplateUsesContextSupportsIndexedAccess(t *testing.T) {
	for _, template := range []string{"${{ inputs.enabled }}", "prefix-${{ inputs['enabled'] }}", "${{ inputs[env.KEY] }}"} {
		usesInputs, err := TemplateUsesContext(template, "inputs")
		if err != nil || !usesInputs {
			t.Fatalf("TemplateUsesContext(%q) = %v, %v, want true", template, usesInputs, err)
		}
	}
	usesInputs, err := TemplateUsesContext("${{ github.ref }}", "inputs")
	if err != nil || usesInputs {
		t.Fatalf("TemplateUsesContext(github.ref) = %v, %v, want false", usesInputs, err)
	}
}

func TestStaticContextReferencesExcludeRuntimeComputedAccess(t *testing.T) {
	for _, source := range []string{"${{ inputs.enabled }}", "${{ inputs['enabled'] }}", "inputs.enabled"} {
		var usesInputs bool
		var err error
		if strings.HasPrefix(source, "inputs") {
			usesInputs, err = ConditionUsesStaticContextReference(source, "inputs")
		} else {
			usesInputs, err = TemplateUsesStaticContextReference(source, "inputs")
		}
		if err != nil || !usesInputs {
			t.Fatalf("static context reference %q = %v, %v, want true", source, usesInputs, err)
		}
	}
	for _, source := range []string{"${{ inputs[env.KEY] }}", "${{ inputs.* }}"} {
		usesInputs, err := TemplateUsesStaticContextReference(source, "inputs")
		if err != nil || usesInputs {
			t.Fatalf("runtime context reference %q = %v, %v, want false", source, usesInputs, err)
		}
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
		{name: "GitHub runtime event identity in job", source: "github.repository_owner && github.ref_name && github.ref_type && github.base_ref", scope: JobCondition},
		{name: "GitHub runtime event identity in step", source: "github.repository_owner && github.ref_name && github.ref_type && github.base_ref", scope: StepCondition},
		{name: "compatible booleans", source: "success() == true", scope: JobCondition},
		{name: "compatible strings", source: "vars.ENABLED == 'true'", scope: JobCondition},
		{name: "compatible integer and float", source: "1 == 1.0", scope: JobCondition},
		{name: "runtime-dependent matrix value", source: "matrix.enabled == true", scope: JobCondition},
		{name: "runner identity", source: "runner.os == 'Linux' && runner.arch == 'X64'", scope: JobCondition},
		{name: "ordered comparison", source: "matrix.count > 1", scope: JobCondition},
		{name: "string and boolean equality", source: "vars.ENABLED == true", scope: JobCondition},
		{name: "boolean and number equality", source: "success() != 1", scope: JobCondition},
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
		{name: "function arguments", source: "always(true)", scope: StepCondition, want: `condition function "always" arguments are unsupported`},
		{name: "runtime event payload", source: "github.event.pull_request.draft", scope: JobCondition, want: `condition reference "github.event.pull_request.draft" is unavailable at runtime`},
		{name: "unsupported github property", source: "github.run_id", scope: StepCondition, want: `condition reference "github.run_id" is unavailable at runtime`},
		{name: "step context in job", source: "steps.build.outcome", scope: JobCondition, want: `condition context "steps" is unavailable in job conditions`},
		{name: "environment in job", source: "env.ENABLED", scope: JobCondition, want: `condition context "env" is unavailable in job conditions`},
		{name: "unsupported context", source: "secrets.TOKEN", scope: StepCondition, want: `condition context "secrets" is unsupported`},
		{name: "unsupported need shape", source: "needs.build.status", scope: JobCondition, want: `expected needs.<job>.result`},
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

func TestValidateCallConditionUsesCallerOnlySurface(t *testing.T) {
	if err := ValidateCallCondition("always() && github.ref && vars.FLAG && inputs.enabled && needs.prepare.outputs.ready"); err != nil {
		t.Fatalf("ValidateCallCondition() error = %v", err)
	}
	for _, source := range []string{"matrix.os", "strategy.job-index", "secrets.TOKEN", "env.FLAG", "runner.os", "steps.test.outcome", "job.services.redis.id", "hashFiles('**')"} {
		if err := ValidateCallCondition(source); err == nil {
			t.Errorf("ValidateCallCondition(%q) accepted unavailable surface", source)
		}
	}
	context := CompileContext{GitHub: map[string]any{"ref": "refs/heads/main"}, Vars: map[string]string{"FLAG": "true"}, Inputs: map[string]any{"enabled": true}}
	if err := ValidateCompileCallCondition("github.ref == 'refs/heads/main' && vars.FLAG && inputs.enabled && needs.prepare.result == 'success'", context); err != nil {
		t.Fatalf("ValidateCompileCallCondition() error = %v", err)
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
	if got, err := EvaluateStep("${{ contains('abc', 'B') }}", context); err != nil || got != "true" {
		t.Fatalf("EvaluateStep() contains = %q, %v", got, err)
	}

	condition := ConditionContext{HashFiles: hash}
	if got, err := EvaluateCondition("hashFiles('*.go', '!generated/**') != ''", condition); err != nil || !got {
		t.Fatalf("EvaluateCondition() = %v, %v", got, err)
	}
	if err := ValidateCondition("hashFiles('*.go') != ''", StepCondition); err != nil {
		t.Fatalf("ValidateCondition() = %v", err)
	}
}

func TestEvaluateStepSupportsCompoundRuntimeExpressions(t *testing.T) {
	context := Context{
		Matrix: map[string]any{"os": "linux", "versions": []any{1, 2}},
		Vars:   map[string]string{"PREFIX": "release"},
		Env:    map[string]string{"KEY": "os"},
		Steps:  map[string]StepStatus{"build": {Outcome: "success", Conclusion: "success", Outputs: map[string]string{"image": "app:v1"}}},
	}
	template := "${{ format('{0}-{1}-{2}', vars.PREFIX, matrix[env.KEY], join(matrix.versions, '.')) }}:${{ matrix.missing || steps.build.outputs.image }}"
	got, err := EvaluateStep(template, context)
	if err != nil || got != "release-linux-1.2:app:v1" {
		t.Fatalf("EvaluateStep() = %q, %v", got, err)
	}
	for _, template := range []string{
		"${{ secrets[env.KEY] }}",
		"${{ github[env.KEY] }}",
		"${{ false && secrets[env.KEY] || '' }}",
		"${{ steps[env.KEY].outputs.image }}",
		"${{ toJSON(needs) }}",
		"${{ matrix[steps[env.KEY].outputs.image] || 'fallback' }}",
		"${{ matrix[toJSON(needs)] }}",
	} {
		if _, err := EvaluateStep(template, context); err == nil {
			t.Errorf("EvaluateStep(%q) allowed prohibited access", template)
		}
	}
	if _, err := EvaluateStep("${{ github.token || '' }}", context); err == nil || !strings.Contains(err.Error(), `unavailable github value "token"`) {
		t.Fatalf("EvaluateStep() github.token error = %v", err)
	}
	if _, err := EvaluateStep("${{ steps['missing'].outputs.value }}", context); err == nil || !strings.Contains(err.Error(), `unavailable step "missing"`) {
		t.Fatalf("EvaluateStep() indexed missing step error = %v", err)
	}
	if _, err := Evaluate("${{ contains('abc', 'a') }}", context); err == nil {
		t.Fatal("Evaluate() broadened general runtime interpolation")
	}
	if got, err := EvaluateStep(`${{ join(fromJSON('[{"name":"bug"},{"name":"help"}]').*.name, ',') }}`, context); err != nil || got != "bug,help" {
		t.Fatalf("EvaluateStep() function projection = %q, %v", got, err)
	}
	for _, template := range []string{`${{ fromJSON('["a","b"]') }}`, `${{ fromJSON('{"a":1}') }}`} {
		if _, err := EvaluateStep(template, context); err == nil || !strings.Contains(err.Error(), "want a scalar") {
			t.Errorf("EvaluateStep(%q) error = %v, want scalar rejection", template, err)
		}
	}
}

func TestEvaluateStepSupportsRetainedGitHubMembers(t *testing.T) {
	context := Context{GitHub: map[string]any{
		"action_path":      "/workspace/actions/composite",
		"base_ref":         "main",
		"job":              "build",
		"ref_name":         "feature",
		"ref_type":         "branch",
		"repository_owner": "buildkite",
		"workflow":         "CI",
	}}
	for template, want := range map[string]string{
		"${{ github.action_path }}/script.sh": "/workspace/actions/composite/script.sh",
		"${{ github.base_ref }}":              "main",
		"${{ github.job }}":                   "build",
		"${{ github.ref_name }}":              "feature",
		"${{ github.ref_type }}":              "branch",
		"${{ github.repository_owner }}":      "buildkite",
		"${{ github.workflow }}":              "CI",
	} {
		if got, err := EvaluateStep(template, context); err != nil || got != want {
			t.Errorf("EvaluateStep(%q) = %q, %v; want %q", template, got, err, want)
		}
	}
	if got, err := EvaluateStep("${{ github.action_path }}", Context{GitHub: map[string]any{}}); err != nil || got != "" {
		t.Fatalf("EvaluateStep() action_path outside composite scope = %q, %v; want empty", got, err)
	}
	if _, err := EvaluateStep("${{ github.run_id }}", context); err == nil {
		t.Fatal("EvaluateStep() accepted github.run_id")
	}
}

func TestExpressionMapProjectionIsDeterministic(t *testing.T) {
	context := Context{Matrix: map[string]any{"zed": "last", "alpha": "first", "middle": "second"}}
	for range 100 {
		got, err := EvaluateStep("${{ join(matrix.*, '-') }}", context)
		if err != nil || got != "first-second-last" {
			t.Fatalf("EvaluateStep() map projection = %q, %v", got, err)
		}
	}
}

func TestEvaluateJobSurfacesSupportAuthorizedCompoundExpressions(t *testing.T) {
	context := Context{
		GitHub:  map[string]any{"ref": "refs/heads/main"},
		Inputs:  map[string]string{"suffix": "prod"},
		Matrix:  map[string]any{"os": "linux"},
		Needs:   map[string]NeedStatus{"build": {Outputs: map[string]string{"tag": "v1"}, Result: "success"}},
		Secrets: map[string]string{"TOKEN": "secret"},
		Steps:   map[string]StepStatus{"build": {Outputs: map[string]string{"image": "app:v1"}}},
		Vars:    map[string]string{"PREFIX": "release"},
		Env:     map[string]string{"ROOT": "src"},
	}

	jobEnv := "${{ format('{0}-{1}-{2}', vars.PREFIX, matrix.os, inputs.suffix) }}:${{ needs.build.outputs.tag }}"
	if got, err := EvaluateJobEnvironment(jobEnv, context); err != nil || got != "release-linux-prod:v1" {
		t.Fatalf("EvaluateJobEnvironment() = %q, %v", got, err)
	}
	jobDefault := "${{ format('{0}/{1}', env.ROOT, matrix.os) }}-${{ github.ref }}"
	if got, err := EvaluateJobDefault(jobDefault, context); err != nil || got != "src/linux-refs/heads/main" {
		t.Fatalf("EvaluateJobDefault() = %q, %v", got, err)
	}
	jobOutput := "${{ steps.build.outputs.image }}-${{ needs.build.result }}-${{ matrix.os }}"
	if got, err := EvaluateJobOutput(jobOutput, context); err != nil || got != "app:v1-success-linux" {
		t.Fatalf("EvaluateJobOutput() = %q, %v", got, err)
	}
	if got, err := EvaluateJobEnvironment("${{ secrets.TOKEN }}", context); err != nil || got != "secret" {
		t.Fatalf("EvaluateJobEnvironment() secret = %q, %v", got, err)
	}
}

func TestEvaluateJobSurfacesErrors(t *testing.T) {
	context := Context{
		GitHub: map[string]any{"token": "secret"},
		Env:    map[string]string{"KEY": "TOKEN"},
		Steps:  map[string]StepStatus{"build": {Outputs: map[string]string{"value": "ok"}}},
	}
	tests := []struct {
		name     string
		evaluate func(string, Context) (string, error)
		template string
	}{
		{name: "job env excludes env", evaluate: EvaluateJobEnvironment, template: "${{ false && env.KEY || 'ok' }}"},
		{name: "job env excludes steps", evaluate: EvaluateJobEnvironment, template: "${{ false && steps.build.outputs.value || 'ok' }}"},
		{name: "job default excludes steps", evaluate: EvaluateJobDefault, template: "${{ false && steps.build.outputs.value || 'ok' }}"},
		{name: "dynamic secret", evaluate: EvaluateJobEnvironment, template: "${{ false && secrets[env.KEY] || 'ok' }}"},
		{name: "aggregate needs", evaluate: EvaluateJobDefault, template: "${{ false && toJSON(needs) || 'ok' }}"},
		{name: "aggregate steps", evaluate: EvaluateJobOutput, template: "${{ false && toJSON(steps) || 'ok' }}"},
		{name: "hash files", evaluate: EvaluateJobDefault, template: "${{ false && hashFiles('go.sum') || 'ok' }}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.evaluate(test.template, context); err == nil {
				t.Fatalf("evaluation accepted %q", test.template)
			}
		})
	}
	if _, err := Evaluate("${{ contains('abc', 'a') }}", context); err == nil {
		t.Fatal("Evaluate() broadened general runtime interpolation")
	}
}

func TestEvaluateJobSurfacesRejectGitHubToken(t *testing.T) {
	context := Context{GitHub: map[string]any{"token": "secret"}}
	for _, evaluate := range []func(string, Context) (string, error){EvaluateJobEnvironment, EvaluateJobDefault, EvaluateJobOutput} {
		for _, template := range []string{"${{ github.token }}", "${{ false && github.token || 'ok' }}"} {
			if _, err := evaluate(template, context); err == nil || !strings.Contains(err.Error(), "github.token is unavailable in this field") {
				t.Errorf("job evaluation of %q error = %v", template, err)
			}
		}
	}
	if got, err := EvaluateStep("${{ github.token }}", context); err != nil || got != "secret" {
		t.Fatalf("EvaluateStep() token = %q, %v", got, err)
	}
}

func TestEvaluateStepControlReturnsTypedValuesWithoutHashFiles(t *testing.T) {
	context := Context{Matrix: map[string]any{"experimental": true, "timeout": 1.5}}
	for _, test := range []struct {
		expression string
		want       any
	}{
		{expression: "${{ matrix.experimental && true }}", want: true},
		{expression: "${{ matrix.timeout }}", want: 1.5},
	} {
		got, err := EvaluateStepControl(test.expression, context)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("EvaluateStepControl(%q) = %#v, %v; want %#v", test.expression, got, err, test.want)
		}
	}
	context.HashFiles = func(patterns []string) (string, error) { return strings.Join(patterns, ","), nil }
	if got, err := EvaluateStepControl("${{ hashFiles('go.sum') }}", context); err != nil || got != "go.sum" {
		t.Fatalf("EvaluateStepControl() hashFiles = %#v, %v", got, err)
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

func TestEvaluateReusableInputDefaultUsesOnlyGraphTimeValues(t *testing.T) {
	context := CompileContext{
		GitHub: map[string]any{"event_name": "push", "ref": "refs/heads/main"},
		Vars:   map[string]string{"COUNT": "3", "SUFFIX": "release"},
	}
	for _, test := range []struct {
		template string
		want     any
	}{
		{template: "${{ format('{0}-{1}', github.event_name, vars.SUFFIX) }}", want: "push-release"},
		{template: "${{ github.ref == 'refs/heads/main' }}", want: true},
		{template: "${{ fromJSON(vars.COUNT) }}", want: float64(3)},
		{template: "deploy-${{ vars.SUFFIX }}", want: "deploy-release"},
		{template: "pre-${{ format('{{{0}}}', vars.SUFFIX) }}", want: "pre-{release}"},
	} {
		got, err := EvaluateReusableInputDefault(test.template, context)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("EvaluateReusableInputDefault(%q) = %#v, %v; want %#v", test.template, got, err, test.want)
		}
	}
}

func TestValidateReusableInputDefaultRejectsUnavailableContexts(t *testing.T) {
	for _, template := range []string{
		"${{ inputs.other }}",
		"${{ needs.build.result }}",
		"${{ matrix.os }}",
		"${{ secrets.TOKEN }}",
		"${{ false && github.token || 'safe' }}",
		"${{ github[vars.KEY] }}",
	} {
		if err := ValidateReusableInputDefault(template); err == nil {
			t.Errorf("ValidateReusableInputDefault(%q) unexpectedly succeeded", template)
		}
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
		{name: "string and number", source: "matrix.version == 12", matrix: map[string]any{"version": "14"}},
		{name: "null and number", source: "matrix.version == 12", matrix: map[string]any{"version": nil}},
		{name: "missing value", source: "matrix.version == 12", matrix: map[string]any{}},
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

func TestEvaluateNeedLookupDistinguishesMissingNeedAndOutput(t *testing.T) {
	needs := map[string]NeedStatus{
		"build": {Outputs: map[string]string{"release": "v1"}, Result: "success"},
	}
	context := Context{Needs: needs}
	if got, err := Evaluate("${{ needs.BUILD.outputs.RELEASE }}", context); err != nil || got != "v1" {
		t.Fatalf("case-insensitive need output = %q, %v, want v1", got, err)
	}
	if got, err := Evaluate("${{ needs.BUILD.outputs.missing }}", context); err != nil || got != "" {
		t.Fatalf("missing output = %q, %v, want empty value", got, err)
	}
	if _, err := Evaluate("${{ needs.missing.outputs.release }}", context); err == nil {
		t.Fatal("missing need output did not fail")
	}
	condition := ConditionContext{Needs: needs}
	if got, err := EvaluateCondition("needs.BUILD.result == 'success' && needs.BUILD.outputs.MISSING == ''", condition); err != nil || !got {
		t.Fatalf("condition need lookup = %v, %v, want true", got, err)
	}
	if _, err := EvaluateCondition("needs.missing.result", condition); err == nil {
		t.Fatal("missing condition need did not fail")
	}
}

func TestEvaluateActionLifecycleCondition(t *testing.T) {
	tests := []struct {
		name         string
		condition    string
		unsuccessful bool
		cancelled    bool
		env          map[string]string
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
		{name: "not cancelled on success", condition: "!cancelled()", want: true},
		{name: "not cancelled after failure", condition: "!cancelled()", unsuccessful: true, want: true},
		{name: "not cancelled after cancellation", condition: "!cancelled()", cancelled: true, want: false},
		{name: "delimiters unwrap", condition: "${{ failure() }}", unsuccessful: true, want: true},
		{name: "delimiters without spaces", condition: "${{always()}}", cancelled: true, want: true},
		{name: "case is insensitive", condition: "ALWAYS()", want: true},
		{name: "surrounding whitespace trims", condition: "  success()  ", want: true},
		{name: "literal", condition: "true", want: true},
		{name: "rust-cache succeeds normally", condition: "success() || env.CACHE_ON_FAILURE == 'true'", want: true},
		{name: "rust-cache skips after failure by default", condition: "success() || env.CACHE_ON_FAILURE == 'true'", unsuccessful: true},
		{name: "rust-cache skips after failure when disabled", condition: "success() || env.CACHE_ON_FAILURE == 'true'", unsuccessful: true, env: map[string]string{"CACHE_ON_FAILURE": "false"}},
		{name: "rust-cache runs after opted-in failure", condition: "success() || env.CACHE_ON_FAILURE == 'true'", unsuccessful: true, env: map[string]string{"CACHE_ON_FAILURE": "true"}, want: true},
		{name: "rust-cache skips after cancellation", condition: "success() || env.CACHE_ON_FAILURE == 'true'", cancelled: true},
		{name: "unavailable references return errors", condition: "github.event_name == 'push'", wantErr: true},
		{name: "compound status expression", condition: "success() || failure()", unsuccessful: true, want: true},
		{name: "arguments return errors", condition: "success('build')", wantErr: true},
		{name: "unknown functions return errors", condition: "finished()", wantErr: true},
		{name: "unsupported lazy branch returns error", condition: "success() || secrets.TOKEN != ''", wantErr: true},
		{name: "unopened delimiter returns error", condition: "failure() }}", wantErr: true},
		{name: "unclosed delimiter returns error", condition: "${{ failure()", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context := ConditionContext{
				Env:          test.env,
				Failure:      test.unsuccessful && !test.cancelled,
				Unsuccessful: test.unsuccessful,
				Cancelled:    test.cancelled,
			}
			got, err := EvaluateActionLifecycleCondition(test.condition, context)
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

func TestValidateActionLifecycleCondition(t *testing.T) {
	for _, condition := range []string{
		"success() || env.CACHE_ON_FAILURE == 'true'",
		"${{ failure() && matrix.allow_failure }}",
		"steps.build.conclusion == 'success' && runner.os == 'Linux'",
		"inputs.cache == true && hashFiles('Cargo.lock') != ''",
		"inputs['cache'] == true",
	} {
		if err := ValidateActionLifecycleCondition(condition); err != nil {
			t.Errorf("ValidateActionLifecycleCondition(%q) error = %v", condition, err)
		}
	}
	for _, condition := range []string{
		"success() || secrets.TOKEN != ''",
		"success() || needs.build.result == 'success'",
		"needs[env.JOB].result == 'success'",
		"vars[env.FLAG] == 'true'",
		"steps[env.STEP].conclusion == 'success'",
		"inputs[env.INPUT] == true",
		"unknown()",
	} {
		if err := ValidateActionLifecycleCondition(condition); err == nil {
			t.Errorf("ValidateActionLifecycleCondition(%q) accepted unsupported expression", condition)
		}
	}
}

func TestEvaluateActionLifecycleConditionUsesWorkflowInputsAndHashFiles(t *testing.T) {
	var patterns []string
	context := ConditionContext{
		Inputs: map[string]any{"cache": true},
		HashFiles: func(got []string) (string, error) {
			patterns = append([]string(nil), got...)
			return "digest", nil
		},
	}
	got, err := EvaluateActionLifecycleCondition("inputs.cache && hashFiles('Cargo.lock') != ''", context)
	if err != nil || !got {
		t.Fatalf("EvaluateActionLifecycleCondition() = %v, %v, want true", got, err)
	}
	if !reflect.DeepEqual(patterns, []string{"Cargo.lock"}) {
		t.Fatalf("hashFiles patterns = %#v, want [Cargo.lock]", patterns)
	}
}

func TestEvaluateConditionStatusOutputsAndTruthiness(t *testing.T) {
	context := ConditionContext{
		Inputs:  map[string]any{"enabled": "true"},
		Needs:   map[string]NeedStatus{"build": {Outputs: map[string]string{"gate": "yes"}, Result: "failure"}},
		Steps:   map[string]StepStatus{"soft": {Outcome: "failure", Conclusion: "success", Outputs: map[string]string{"ready": "true"}}},
		Failure: true,
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
	context := ConditionContext{Inputs: map[string]any{"enabled": "true", "deploy": true, "retries": json.Number("2")}}
	for condition, want := range map[string]bool{
		"inputs.enabled == 'true'": true,
		"INPUTS.ENABLED == 'true'": true,
		"inputs.deploy":            true,
		"inputs.retries == 2":      true,
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

func TestEvaluateConditionMatchesGitHubCoercionAndOrdering(t *testing.T) {
	tests := []struct {
		condition string
		context   ConditionContext
		want      bool
	}{
		{condition: "null == 0", want: true},
		{condition: "false == 0", want: true},
		{condition: "'' == 0", want: true},
		{condition: "'12' == 12", want: true},
		{condition: "'01' == '1'", want: false},
		{condition: "'Release' == 'release'", want: true},
		{condition: "'12' > 2", want: true},
		{condition: "'Beta' > 'alpha'", want: true},
		{condition: "'not-a-number' > 0", want: false},
		{condition: "matrix.left == matrix.right", context: ConditionContext{Matrix: map[string]any{"left": json.Number("9007199254740992"), "right": json.Number("9007199254740993")}}, want: true},
		{condition: "'1e-400' == 0", want: true},
		{condition: "'1e309' == matrix.value", context: ConditionContext{Matrix: map[string]any{"value": math.Inf(1)}}, want: true},
		{condition: "matrix.value", context: ConditionContext{Matrix: map[string]any{"value": math.NaN()}}, want: false},
		{condition: "matrix.value", context: ConditionContext{Matrix: map[string]any{"value": json.Number("1e-400")}}, want: false},
		{condition: "matrix.missing == null", context: ConditionContext{Matrix: map[string]any{}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.condition, func(t *testing.T) {
			got, err := EvaluateCondition(test.condition, test.context)
			if err != nil || got != test.want {
				t.Fatalf("EvaluateCondition(%q) = %v, %v, want %v", test.condition, got, err, test.want)
			}
		})
	}
}

func TestEvaluateConditionSupportsPureFunctions(t *testing.T) {
	for _, condition := range []string{
		"startsWith(123, '12')",
		"endsWith(true, 'UE')",
		"contains(fromJSON('[1,\"Deploy\"]'), 'deploy')",
		"format('{0}-{1}', 'release', 2) == 'release-2'",
		"format('{0}-{1}', fromJSON('{}'), fromJSON('[]')) == 'Object-Array'",
		"format(fromJSON('{}')) == 'Object' && format(fromJSON('[]')) == 'Array'",
		"join(fromJSON('[\"one\",2]'), '-') == 'one-2'",
		"fromJSON(toJSON(true))",
		"fromJSON(true)",
		"join('abc', '-') == 'abc'",
		"format(123) == '123'",
		"format('ok', fromJSON('bad')) == 'ok'",
		"contains(fromJSON('[]'), fromJSON('bad')) == false",
		"startsWith(fromJSON('[]'), 'x') == false",
		"case(false, matrix.unavailable, true, 'selected', matrix.unavailable) == 'selected'",
	} {
		got, err := EvaluateCondition(condition, ConditionContext{})
		if err != nil || !got {
			t.Errorf("EvaluateCondition(%q) = %v, %v", condition, got, err)
		}
	}
	if err := ValidateCondition("case(true, 'selected', false, secrets.TOKEN, '')", StepCondition); err == nil {
		t.Fatal("ValidateCondition() allowed an unsupported context in a lazy branch")
	}
	if _, err := EvaluateCondition("case('true', 'selected', 'fallback')", ConditionContext{}); err == nil || !strings.Contains(err.Error(), "want boolean") {
		t.Fatalf("EvaluateCondition() case predicate error = %v", err)
	}
}

func TestNestedMatrixReferencesAreSupported(t *testing.T) {
	matrix := map[string]any{"config": map[string]any{"os": "ubuntu-24.04", "name": "linux"}, "os": "ubuntu-24.04"}

	// Compile-time templates such as runs-on.
	got, err := EvaluateCompileTemplate("${{ matrix.config.os }}", CompileContext{Matrix: matrix})
	if err != nil || got != "ubuntu-24.04" {
		t.Fatalf("EvaluateCompileTemplate() = %q, %v", got, err)
	}
	// Missing or scalar intermediate segments yield null, matching GitHub.
	for _, template := range []string{"${{ matrix.config.missing }}", "${{ matrix.os.name }}"} {
		if got, err := EvaluateCompileTemplate(template, CompileContext{Matrix: matrix}); err != nil || got != "" {
			t.Errorf("EvaluateCompileTemplate(%q) = %q, %v, want empty", template, got, err)
		}
	}

	// Runtime templates such as step run and env.
	if err := ValidateRuntimeTemplate("${{ matrix.config.name }}"); err != nil {
		t.Fatalf("ValidateRuntimeTemplate() error = %v", err)
	}
	if got, err := EvaluateStep("${{ matrix.config.name }}", Context{Matrix: matrix}); err != nil || got != "linux" {
		t.Fatalf("EvaluateStep() = %q, %v", got, err)
	}
	if got, err := Evaluate("${{ matrix.config.missing }}", Context{Matrix: matrix}); err != nil || got != "" {
		t.Fatalf("Evaluate() = %q, %v, want empty", got, err)
	}

	// Conditions.
	for condition, want := range map[string]bool{
		"matrix.config.name == 'linux'": true,
		"matrix.config.name == 'mac'":   false,
		"matrix.config.missing == null": true,
		"matrix.os.name == null":        true,
	} {
		if err := ValidateCondition(condition, StepCondition); err != nil {
			t.Errorf("ValidateCondition(%q) error = %v", condition, err)
			continue
		}
		got, err := EvaluateCondition(condition, ConditionContext{Matrix: matrix})
		if err != nil || got != want {
			t.Errorf("EvaluateCondition(%q) = %v, %v, want %v", condition, got, err, want)
		}
	}
}

func TestEvaluateConditionSupportsBracketFormGitHubReferences(t *testing.T) {
	for condition, want := range map[string]bool{
		"github['event_name'] == 'push'":       true,
		"github['EVENT_NAME'] == 'push'":       true,
		"github['event_name'] == 'workflow'":   false,
		"github['head_ref'] == null":           true,
		"github['ref'] == 'refs/heads/main'":   true,
		"github['repository'] == 'acme/thing'": true,
	} {
		if err := ValidateCondition(condition, StepCondition); err != nil {
			t.Errorf("ValidateCondition(%q) error = %v", condition, err)
			continue
		}
		got, err := EvaluateCondition(condition, ConditionContext{GitHub: map[string]any{
			"event_name": "push",
			"ref":        "refs/heads/main",
			"repository": "acme/thing",
		}})
		if err != nil || got != want {
			t.Errorf("EvaluateCondition(%q) = %v, %v, want %v", condition, got, err, want)
		}
	}
	if _, err := EvaluateCondition("github['event_name'] == 'push'", ConditionContext{}); err == nil || !strings.Contains(err.Error(), "condition references unavailable value github.event_name") {
		t.Fatalf("EvaluateCondition() without github context error = %v", err)
	}
	// Whole and dynamic github access returns an error at evaluation even
	// without prior validation, such as composite-action if conditions.
	for _, condition := range []string{"github", "github[vars.KEY]", "github.*"} {
		if _, err := EvaluateCondition(condition, ConditionContext{GitHub: map[string]any{"event_name": "push"}, Vars: map[string]string{"KEY": "event_name"}}); err == nil {
			t.Errorf("EvaluateCondition(%q) error = nil, want unsupported access error", condition)
		}
	}
}

func TestEvaluateConditionSupportsIndexesFiltersAndWholeContexts(t *testing.T) {
	context := ConditionContext{
		Vars:   map[string]string{"KEY": "target"},
		Inputs: map[string]any{"target": "selected"},
		Matrix: map[string]any{
			"target": "selected",
			"array":  []any{"zero", "one", "two"},
			"object": map[string]any{"true": "boolean", "2": "number"},
			"items":  []any{[]any{"first"}, []any{}, []any{nil}},
		},
		Needs: map[string]NeedStatus{
			"build": {Outputs: map[string]string{}, Result: "success"},
			"lint":  {Outputs: map[string]string{}, Result: "failure"},
		},
		Env: map[string]string{"STEP": "build"},
		Steps: map[string]StepStatus{
			"build": {Outcome: "success", Conclusion: "success"},
			"lint":  {Outcome: "failure", Conclusion: "success"},
		},
	}
	for _, condition := range []string{
		"matrix[vars.KEY] == 'selected'",
		"inputs[vars.KEY] == 'selected'",
		"fromJSON('[\"zero\",\"one\"]')[1] == 'one'",
		"fromJSON('[1]')[4] == null",
		"matrix.array['1'] == 'one'",
		"matrix.array[1.9] == 'one'",
		"matrix.array['1e309'] == null",
		"matrix.array['2147483648'] == null",
		"matrix.object[true] == 'boolean'",
		"matrix.object[2] == 'number'",
		"join(matrix.items.*[0], ',') == 'first,'",
		"contains(fromJSON('[{\"name\":\"bug\"}]').*.name, 'bug')",
		"contains(needs.*.result, 'FAILURE')",
		"steps[env.STEP].outcome == 'success'",
		"steps['missing'].outcome == null",
		"contains(steps.*.outcome, 'success') && contains(steps.*.outcome, 'failure')",
		"contains(toJSON(matrix), '\"target\"')",
		"needs == needs",
		"steps == steps",
		"matrix <= matrix && matrix >= matrix",
		"fromJSON('[]') != fromJSON('[]')",
		"fromJSON('[]').* != fromJSON('[]').*",
	} {
		if err := ValidateCondition(condition, StepCondition); err != nil {
			t.Errorf("ValidateCondition(%q) error = %v", condition, err)
			continue
		}
		got, err := EvaluateCondition(condition, context)
		if err != nil || !got {
			t.Errorf("EvaluateCondition(%q) = %v, %v", condition, got, err)
		}
	}
	if err := ValidateCondition("github[vars.KEY]", StepCondition); err == nil {
		t.Fatal("ValidateCondition() allowed dynamic github access")
	}
	if err := ValidateCondition("steps.*.outcome", JobCondition); err == nil {
		t.Fatal("ValidateCondition() allowed step projection in a job condition")
	}
	for _, condition := range []string{"toJSON(vars)", "toJSON(env)"} {
		if err := ValidateCondition(condition, StepCondition); err == nil {
			t.Errorf("ValidateCondition(%q) allowed an unavailable whole or computed context", condition)
		}
	}
}

func TestEvaluateConditionFailsClosed(t *testing.T) {
	if got, err := EvaluateCondition("1 < 2", ConditionContext{}); err != nil || !got {
		t.Fatalf("EvaluateCondition() ordered comparison = %v, %v", got, err)
	}
	if got, err := EvaluateCondition("true == 'true'", ConditionContext{}); err != nil || got {
		t.Fatalf("EvaluateCondition() NaN equality = %v, %v", got, err)
	}
	if got, err := EvaluateCondition("null == false", ConditionContext{}); err != nil || !got {
		t.Fatalf("EvaluateCondition() null coercion = %v, %v", got, err)
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
		GitHub: map[string]any{
			"base_ref":   "main",
			"event_name": "push",
			"event":      map[string]any{"action": "opened"},
			"ref_name":   "42/merge",
			"ref_type":   "branch",
		},
		Event:  map[string]any{"action": "opened"},
		Vars:   map[string]string{"RUNNERS": `["ubuntu-24.04","ubuntu-22.04"]`},
		Matrix: map[string]any{"os": "ubuntu-24.04"},
	}
	tests := []struct {
		expression string
		want       any
	}{
		{expression: "${{ github.event_name }}", want: "push"},
		{expression: "${{ github.ref_name }}", want: "42/merge"},
		{expression: "${{ github.ref_type }}", want: "branch"},
		{expression: "${{ github.base_ref }}", want: "main"},
		{expression: "${{ github.event.action }}", want: "opened"},
		{expression: "${{ event.action }}", want: "opened"},
		{expression: "${{ matrix.os }}", want: "ubuntu-24.04"},
		{expression: "${{ vars.MISSING }}", want: nil},
		{expression: "${{ github.event.number || github.ref }}", want: "refs/pull/42/merge"},
		{expression: "${{ github.ref == 'refs/pull/42/merge' }}", want: true},
		{expression: "${{ startsWith(github.ref, 'REFS/PULL/') }}", want: true},
		{expression: "${{ contains(github.ref, 'PULL/42') }}", want: true},
		{expression: "${{ contains(github.ref, 'ISSUES') }}", want: false},
		{expression: "${{ endsWith(github.ref, '/MERGE') }}", want: true},
		{expression: "${{ endsWith(github.ref, '/HEAD') }}", want: false},
		{expression: "${{ endsWith('ref', true) }}", want: false},
		{expression: "${{ contains(fromJSON('[\"push\",\"pull_request\"]'), 'PUSH') }}", want: true},
		{expression: "${{ format('{0}-{1}', github.event_name, 2) }}", want: "push-2"},
		{expression: "${{ format('{0}', fromJSON('1e2')) }}", want: "100"},
		{expression: "${{ join(fromJSON('[\"one\",2,true,null]'), '-') }}", want: "one-2-true-"},
		{expression: "${{ join(fromJSON('[1e2]')) }}", want: "100"},
		{expression: "${{ join(fromJSON('{}')) }}", want: ""},
		{expression: "${{ join(fromJSON('[\"one\",\"two\"]'), fromJSON('{}')) }}", want: "one,two"},
		{expression: "${{ join(fromJSON('[{},[]]')) }}", want: "Object,Array"},
		{expression: "${{ toJSON(fromJSON('1e2')) }}", want: "100"},
		{expression: "${{ toJSON(fromJSON('1e20')) }}", want: "1E+20"},
		{expression: "${{ toJSON(github.event_name) }}", want: `"push"`},
		{expression: "${{ toJSON('<&>') }}", want: `"<&>"`},
		{expression: "${{ case(false, vars.missing, true, 'selected', vars.missing) }}", want: "selected"},
		{expression: "${{ '0xff' == 255 && '0o10' == 8 && 'Infinity' > 1e308 }}", want: true},
		{expression: "${{ '0xffffffff' == -1 }}", want: true},
		{expression: "${{ '0o37777777777' == -1 }}", want: true},
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

func TestEvaluateCompileTemplateSupportsGitHubRefScalars(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{
		"base_ref": "main",
		"ref_name": "42/merge",
		"ref_type": "branch",
	}}
	got, err := EvaluateCompileTemplate("${{ github.ref_type }}-${{ github.ref_name }}-${{ github.base_ref }}", context)
	if err != nil {
		t.Fatal(err)
	}
	if got != "branch-42/merge-main" {
		t.Fatalf("EvaluateCompileTemplate() = %q, want branch-42/merge-main", got)
	}

	got, err = EvaluateCompileTemplate("base-${{ github.base_ref }}", CompileContext{GitHub: map[string]any{"base_ref": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "base-" {
		t.Fatalf("EvaluateCompileTemplate() empty base_ref = %q, want base-", got)
	}

	if _, err := EvaluateCompileTemplate("${{ github.run_id }}", CompileContext{GitHub: map[string]any{"run_id": "123"}}); err == nil {
		t.Fatal("EvaluateCompileTemplate() admitted unsupported github.run_id")
	}
}

func TestEncodeExpressionJSONSupportsNonFiniteNumbers(t *testing.T) {
	got, err := encodeExpressionJSON(map[string]any{
		"values": []any{math.Inf(1), math.Inf(-1), math.NaN()},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"values\": [\n    Infinity,\n    -Infinity,\n    NaN\n  ]\n}"
	if got != want {
		t.Fatalf("encodeExpressionJSON() = %q, want %q", got, want)
	}
}

func TestEvaluateCompileSupportsIndexesAndFilters(t *testing.T) {
	context := CompileContext{
		GitHub: map[string]any{"event": map[string]any{
			"items": []any{
				map[string]any{"name": "one", "groups": []any{map[string]any{"id": 1}, map[string]any{"id": 2}}},
				map[string]any{"groups": []any{map[string]any{"id": 3}}},
				map[string]any{"name": "three"},
			},
		}},
		Vars:   map[string]string{"KEY": "target"},
		Matrix: map[string]any{"target": "selected"},
	}
	for _, test := range []struct {
		expression string
		want       any
	}{
		{expression: "${{ matrix[vars.KEY] }}", want: "selected"},
		{expression: "${{ fromJSON('[\"zero\",\"one\"]')[1] }}", want: "one"},
		{expression: "${{ fromJSON('[1]')[4] }}", want: nil},
		{expression: "${{ join(github.event.items.*.name, ',') }}", want: "one,three"},
		{expression: "${{ join(github['event'].items.*.name, ',') }}", want: "one,three"},
		{expression: "${{ join(github.event.items.*.groups.*.id, ',') }}", want: "1,2,3"},
	} {
		expr, err := Parse(test.expression, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := EvaluateCompile(expr, context)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("EvaluateCompile(%q) = %#v, %v, want %#v", test.expression, got, err, test.want)
		}
	}
}

func TestEvaluateStepSupportsIndexedWorkflowInputs(t *testing.T) {
	context := Context{
		WorkflowInputs: map[string]any{"label": "dispatched", "enabled": true},
		Env:            map[string]string{"KEY": "label"},
	}
	for _, test := range []struct {
		template string
		want     string
	}{
		{template: "${{ inputs['label'] }}", want: "dispatched"},
		{template: "${{ inputs[env.KEY] }}", want: "dispatched"},
		{template: "${{ inputs.enabled }}", want: "true"},
	} {
		got, err := EvaluateStep(test.template, context)
		if err != nil || got != test.want {
			t.Errorf("EvaluateStep(%q) = %q, %v; want %q", test.template, got, err, test.want)
		}
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
	for _, source := range []string{"github.event_name == 'push'", "github.ref == 'refs/heads/main'"} {
		if usesEvent, err := ReferencesGitHubEvent(source); err != nil || usesEvent {
			t.Fatalf("ReferencesGitHubEvent(%q) = %v, %v", source, usesEvent, err)
		}
	}
	if usesEvent, err := ReferencesGitHubEvent("github.ref_type == 'branch' && github.ref_name && github.base_ref == ''"); err != nil || !usesEvent {
		t.Fatalf("ReferencesGitHubEvent(ref scalars) = %v, %v", usesEvent, err)
	}
}

func TestReduceCompileConditionPreservesRuntimeSubtrees(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{
		"event": map[string]any{"pull_request": map[string]any{"draft": true, "title": "It's ready"}},
	}}
	got, err := ReduceCompileCondition("github.event.pull_request.draft && (failure() || github.event.pull_request.title == needs.build.outputs.title)", context)
	if err != nil {
		t.Fatal(err)
	}
	want := "(true && (failure() || ('It''s ready' == needs.build.outputs.title)))"
	if got != want {
		t.Fatalf("ReduceCompileCondition() = %q, want %q", got, want)
	}
	if usesEvent, err := ReferencesGitHubEvent(got); err != nil || usesEvent {
		t.Fatalf("reduced condition retains github.event: %q, %v", got, err)
	}
}

func TestReduceCompileConditionConvertsMissingEventMembersToNull(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"event": map[string]any{}}}
	got, err := ReduceCompileCondition("github.event.pull_request.draft || needs.build.result == 'success'", context)
	if err != nil {
		t.Fatal(err)
	}
	if want := "(null || (needs.build.result == 'success'))"; got != want {
		t.Fatalf("ReduceCompileCondition() = %q, want %q", got, want)
	}
}

func TestConditionAuthorityScanningIgnoresExpressionTextInLiterals(t *testing.T) {
	source := "'${{ github.token }} ${{ secrets.DEPLOY }} ${{ github.event.action }}' == runner.os"
	if names, err := ConditionSecretReferences(source); err != nil || len(names) != 0 {
		t.Fatalf("ConditionSecretReferences() = %#v, %v", names, err)
	}
	if token, err := ConditionReferencesGitHubToken(source); err != nil || token {
		t.Fatalf("ConditionReferencesGitHubToken() = %v, %v", token, err)
	}
	if event, err := ReferencesGitHubEvent(source); err != nil || event {
		t.Fatalf("ReferencesGitHubEvent() = %v, %v", event, err)
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
	if err := ValidateCompileConditionWithMatrix("contains(toJSON(github.event), needs.build.outputs.marker)", JobCondition, context, nil); err == nil || !strings.Contains(err.Error(), "whole github.event access is unsupported") {
		t.Fatalf("ValidateCompileConditionWithMatrix() whole event error = %v", err)
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
		{name: "typed numerics outside the YAML model are rejected", value: int32(7), wantErr: "cannot be represented"},
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
	template := "${{ !inputs.enabled && matrix.run-new && 'inputs.enabled' || inputs['label'] }}"
	got, err := SubstituteCompileInputs(template, map[string]any{"enabled": false, "label": "it''s ready"})
	if err != nil {
		t.Fatal(err)
	}
	want := "${{ !false && matrix.run-new && 'inputs.enabled' || 'it''''s ready' }}"
	if got != want {
		t.Fatalf("SubstituteCompileInputs() = %q, want %q", got, want)
	}
}

func TestSubstituteCompileInputsResolvesNestedComputedInputIndex(t *testing.T) {
	got, err := SubstituteCompileInputs("${{ inputs[inputs.key] }}", map[string]any{"key": "target", "target": "release"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "${{ 'release' }}"; got != want {
		t.Fatalf("SubstituteCompileInputs() = %q, want %q", got, want)
	}
}

func TestSubstituteCompileInputsIgnoresNestedInputsProperties(t *testing.T) {
	template := "${{ inputs.debug }} ${{ github.event.inputs.debug }} ${{ steps.inputs.name }} ${{ fromJSON(vars.CFG).inputs.name }}"
	got, err := SubstituteCompileInputs(template, map[string]any{"debug": true, "name": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	want := "${{ true }} ${{ github.event.inputs.debug }} ${{ steps.inputs.name }} ${{ fromJSON(vars.CFG).inputs.name }}"
	if got != want {
		t.Fatalf("SubstituteCompileInputs() = %q, want %q", got, want)
	}
}

func TestEvaluateAvailableCompileTemplatePreservesRuntimeExpressions(t *testing.T) {
	got, err := EvaluateAvailableCompileTemplate("echo ${{ 'target' }} ${{ fromJSON('1e20') }} ${{ github.ref }}", CompileContext{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "echo target 1E+20 ${{ github.ref }}"; got != want {
		t.Fatalf("EvaluateAvailableCompileTemplate() = %q, want %q", got, want)
	}
}

func TestReduceAvailableCompileTemplateReducesEventSubtrees(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"event": map[string]any{
		"action":       "opened",
		"pull_request": map[string]any{"head": map[string]any{"sha": "abc123"}},
	}}, Event: map[string]any{"action": "opened"}}
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{name: "direct", template: "${{ github.event.pull_request.head.sha }}", want: "abc123"},
		{name: "missing member", template: "before-${{ github.event.push.missing }}-after", want: "before--after"},
		{name: "mixed runtime", template: "${{ github.event.pull_request.head.sha == steps.checkout.outputs.sha }}", want: "${{ ('abc123' == steps.checkout.outputs.sha) }}"},
		{name: "multiple", template: "${{ github.event.pull_request.head.sha }}-${{ event.action }}-${{ needs.build.outputs.suffix }}", want: "abc123-opened-${{ needs.build.outputs.suffix }}"},
		{name: "runtime short circuit branch", template: "${{ github.event.action == 'opened' || needs.build.outputs.ready }}", want: "${{ (true || needs.build.outputs.ready) }}"},
		{name: "unrelated compile value", template: "${{ github.sha }}", want: "${{ github.sha }}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ReduceAvailableCompileTemplate(test.template, context)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("ReduceAvailableCompileTemplate() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReduceAvailableCompileTemplateRejectsIntroducedExpressionSyntax(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"event": map[string]any{"value": "${{ secrets.ADMIN }}"}}}
	_, err := ReduceAvailableCompileTemplate("${{ github.event.value }}", context)
	if err == nil || !strings.Contains(err.Error(), "result contains expression syntax") {
		t.Fatalf("ReduceAvailableCompileTemplate() error = %v", err)
	}
}

func TestReduceAvailableCompileTemplateRejectsWholeEventAccess(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"event": map[string]any{"action": "opened"}}}
	context.Event = context.GitHub["event"].(map[string]any)
	for _, template := range []string{
		"${{ toJSON(github.event) }}",
		"${{ toJSON(github.event.*) }}",
		"${{ toJSON(event.*) }}",
	} {
		_, err := ReduceAvailableCompileTemplate(template, context)
		if err == nil || !strings.Contains(err.Error(), "whole github.event access is unsupported") {
			t.Errorf("ReduceAvailableCompileTemplate(%q) error = %v", template, err)
		}
	}
}

func TestReduceAvailableCompileTemplateRejectsDeterministicEventErrors(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"event": map[string]any{"value": "["}}}
	for _, template := range []string{
		"${{ fromJSON(github.event.value) }}",
		"${{ needs.build.outputs.ready || fromJSON(github.event.value) }}",
		"${{ (fromJSON(needs.build.outputs.config) || fromJSON(github.event.value)).foo }}",
		"${{ (fromJSON(needs.build.outputs.config) || fromJSON(github.event.value)).*.foo }}",
	} {
		_, err := ReduceAvailableCompileTemplate(template, context)
		if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Errorf("ReduceAvailableCompileTemplate(%q) error = %v", template, err)
		}
	}
}

func TestReduceCompileConditionRejectsDeterministicEventErrors(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"event": map[string]any{"value": "["}}}
	_, err := ReduceCompileCondition("needs.build.outputs.ready || fromJSON(github.event.value)", context)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("ReduceCompileCondition() error = %v", err)
	}
}

func TestEvaluateCompileTemplateUsesGitHubNumberRendering(t *testing.T) {
	got, err := EvaluateCompileTemplate("prefix-${{ fromJSON('1e20') }}", CompileContext{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "prefix-1E+20"; got != want {
		t.Fatalf("EvaluateCompileTemplate() = %q, want %q", got, want)
	}
}

func TestValidateServiceCredentialTemplateContexts(t *testing.T) {
	for _, template := range []string{"${{ github.actor }}", "${{ vars.USER }}", "${{ secrets.PASSWORD }}", "${{ env.USER }}"} {
		if err := ValidateServiceCredentialTemplate(template); err != nil {
			t.Errorf("ValidateServiceCredentialTemplate(%q) = %v", template, err)
		}
	}
	for _, template := range []string{"${{ inputs.user }}", "${{ matrix.user }}", "${{ strategy.job-index }}", "${{ needs.build.outputs.user }}", "${{ env.USER.extra }}", "${{ secrets }}"} {
		if err := ValidateServiceCredentialTemplate(template); err == nil {
			t.Errorf("ValidateServiceCredentialTemplate(%q) succeeded", template)
		}
	}
}

func TestEvaluateAvailableCompileTemplateRejectsIntroducedExpressionSyntax(t *testing.T) {
	for _, test := range []struct {
		template string
		value    string
	}{
		{template: "${{ inputs.value }}", value: "${{ secrets.ADMIN }}"},
		{template: "$${{ inputs.value }}", value: "{{ secrets.ADMIN }}"},
		{template: "$${{ inputs.value }}{{ secrets.ADMIN }}", value: ""},
	} {
		_, err := EvaluateAvailableCompileTemplate(test.template, CompileContext{Inputs: map[string]any{"value": test.value}})
		if err == nil || !strings.Contains(err.Error(), "result contains expression syntax") {
			t.Errorf("EvaluateAvailableCompileTemplate(%q) error = %v", test.template, err)
		}
	}
}

func TestEvaluateCompileFailsClosed(t *testing.T) {
	tests := []struct {
		expression string
		want       string
	}{
		{expression: "${{ secrets.TOKEN }}", want: `unsupported compile-time context "secrets"`},
		{expression: "${{ github.token }}", want: `unavailable value "github.token"`},
		{expression: "${{ case(true, 'safe', github.token) }}", want: `unavailable value "github.token"`},
		{expression: "${{ case(true, 'safe', secrets.TOKEN) }}", want: `unsupported compile-time context "secrets"`},
		{expression: "${{ toJSON(github.event) }}", want: `unavailable value "github.event"`},
		{expression: "${{ toJSON(github.event.*) }}", want: `whole event projection is unsupported`},
		{expression: "${{ toJSON(event) }}", want: `whole event access is unsupported`},
		{expression: "${{ hashFiles('go.sum') }}", want: `unsupported compile-time function "hashFiles"`},
		{expression: "${{ startsWith(github.ref) }}", want: `function "startsWith" received an unsupported number of arguments`},
		{expression: "${{ contains(github.ref) }}", want: `function "contains" received an unsupported number of arguments`},
		{expression: "${{ fromJSON(vars.BAD) }}", want: "invalid JSON"},
		{expression: "${{ event.Ref }}", want: "ambiguous properties"},
	}
	for _, test := range tests {
		t.Run(test.expression, func(t *testing.T) {
			expr, err := Parse(test.expression, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			_, err = EvaluateCompile(expr, CompileContext{GitHub: map[string]any{"ref": "refs/heads/main"}, Vars: map[string]string{"BAD": "["}, Event: map[string]any{"ref": "one", "REF": "two"}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("EvaluateCompile() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEvaluateCompileRejectsNonDigitFormatPlaceholders(t *testing.T) {
	for _, template := range []string{
		"${{ format('{+0}', 'x') }}",
		"${{ format('{-0}', 'x') }}",
		"${{ format('{ 0 }', 'x') }}",
		"${{ format('{0x1}', 'x') }}",
	} {
		if _, err := EvaluateCompileTemplate(template, CompileContext{}); err == nil || !strings.Contains(err.Error(), "format placeholder") {
			t.Errorf("EvaluateCompileTemplate(%q) error = %v, want format placeholder error", template, err)
		}
	}
	got, err := EvaluateCompileTemplate("${{ format('{0} {01}', 'a', 'b') }}", CompileContext{})
	if err != nil || got != "a b" {
		t.Fatalf("EvaluateCompileTemplate() = %q, %v", got, err)
	}
}

func TestFromJSONCollapsesCaseInsensitiveDuplicateKeys(t *testing.T) {
	for template, want := range map[string]string{
		`${{ fromJSON('{"a":1,"A":2}').a }}`:       "2",
		`${{ fromJSON('{"a":1,"A":2}').A }}`:       "2",
		`${{ fromJSON('{"a":1,"A":2}')['A'] }}`:    "2",
		`${{ fromJSON('{"A":2,"a":1}').a }}`:       "1",
		`${{ fromJSON('{"Σ":1,"ς":2}')['Σ'] }}`:    "2",
		`${{ fromJSON('{"Σ":1,"ς":2}')['σ'] }}`:    "2",
		`${{ toJSON(fromJSON('{"a":1,"A":2}')) }}`: "{\n  \"a\": 2\n}",
	} {
		got, err := EvaluateCompileTemplate(template, CompileContext{})
		if err != nil || got != want {
			t.Errorf("EvaluateCompileTemplate(%q) = %q, %v, want %q", template, got, err, want)
		}
	}
}

func TestEvaluateCompileSupportsFunctionResultProjection(t *testing.T) {
	expr, err := Parse("${{ join(fromJSON('[{\"name\":\"bug\"}]').*.name, ',') }}", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateCompile(expr, CompileContext{})
	if err != nil || got != "bug" {
		t.Fatalf("EvaluateCompile() function projection = %#v, %v", got, err)
	}
}
