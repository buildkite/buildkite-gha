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
