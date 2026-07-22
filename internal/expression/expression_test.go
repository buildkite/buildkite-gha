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
