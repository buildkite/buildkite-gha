package expression

import (
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

func TestEvaluateIsSinglePass(t *testing.T) {
	literal := "literal ${{ matrix.secret }} and ${{"
	context := Context{
		Inputs: map[string]string{"value": literal},
		Matrix: map[string]any{"value": literal, "secret": "reevaluated"},
		Steps:  map[string]map[string]string{"producer": {"value": literal}},
		Needs:  map[string]map[string]string{"producer": {"value": literal}},
	}
	tests := map[string]string{
		"${{ inputs.value }}":                 literal,
		"${{ matrix.value }}":                 literal,
		"${{ steps.Producer.outputs.value }}": literal,
		"${{ needs.Producer.outputs.value }}": literal,
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

func TestEvaluateFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{name: "unsupported", template: "${{ github.sha }}", want: `unsupported expression "github.sha"`},
		{name: "unavailable matrix value", template: "${{ matrix.missing }}", want: `unavailable matrix value "missing"`},
		{name: "unavailable step", template: "${{ steps.missing.outputs.value }}", want: `unavailable step "missing"`},
		{name: "unavailable need", template: "${{ needs.missing.outputs.value }}", want: `unavailable need "missing"`},
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
