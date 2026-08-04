package expression

import (
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

func TestEvaluateIsSinglePass(t *testing.T) {
	literal := "literal ${{ matrix.secret }} and ${{"
	context := Context{
		Inputs:       map[string]string{"value": literal},
		Matrix:       map[string]any{"value": literal, "secret": "reevaluated"},
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

func TestEvaluateConditionFailsClosed(t *testing.T) {
	if _, err := EvaluateCondition("1 < 2", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() accepted unsupported ordered comparison")
	}
	if _, err := EvaluateCondition("true == 'true'", ConditionContext{}); err == nil {
		t.Fatal("EvaluateCondition() silently coerced mixed equality operands")
	}
	if got, err := EvaluateCondition("", ConditionContext{Unsuccessful: true}); err != nil || got {
		t.Fatalf("default condition after skipped prerequisite = %v, %v, want false", got, err)
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
	}
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

func TestEvaluateCompileFailsClosed(t *testing.T) {
	tests := []struct {
		expression string
		want       string
	}{
		{expression: "${{ secrets.TOKEN }}", want: `unsupported compile-time context "secrets"`},
		{expression: "${{ vars.MISSING }}", want: `unavailable value "vars.missing"`},
		{expression: "${{ hashFiles('go.sum') }}", want: `unsupported compile-time function "hashFiles"`},
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
