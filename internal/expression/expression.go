// Package expression adapts actionlint's expression parser into owned values.
package expression

import (
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

// Position is a one-based source position.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Span locates an expression in its source workflow.
type Span struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Expression retains expression text independently of actionlint's AST.
type Expression struct {
	Text string `json:"text"`
	Span Span   `json:"span"`
}

// Context contains the Phase 0 values available while evaluating a template.
type Context struct {
	Inputs map[string]string
	Matrix map[string]any
	Steps  map[string]map[string]string
	Needs  map[string]map[string]string
}

// Parse validates a complete ${{ ... }} expression using actionlint and returns
// an owned representation.
func Parse(text string, line, column int) (Expression, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "${{") || !strings.HasSuffix(trimmed, "}}") {
		return Expression{}, fmt.Errorf("expected a complete ${{ ... }} expression")
	}

	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "${{"), "}}"))
	parser := actionlint.NewExprParser()
	if _, err := parser.Parse(actionlint.NewExprLexer(body + "}}")); err != nil {
		return Expression{}, fmt.Errorf("invalid expression: %w", err)
	}

	return Expression{
		Text: text,
		Span: Span{
			Start: Position{Line: line, Column: column},
			End:   endPosition(line, column, text),
		},
	}, nil
}

// Evaluate substitutes the supported Phase 0 expressions in a template once.
func Evaluate(template string, context Context) (string, error) {
	const open, close = "${{", "}}"
	var evaluated strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			evaluated.WriteString(remaining)
			return evaluated.String(), nil
		}
		evaluated.WriteString(remaining[:start])
		end := strings.Index(remaining[start+len(open):], close)
		if end < 0 {
			return "", fmt.Errorf("unterminated expression in %q", template)
		}
		end += start + len(open)
		replacement, err := evaluateReference(strings.TrimSpace(remaining[start+len(open):end]), context)
		if err != nil {
			return "", err
		}
		evaluated.WriteString(replacement)
		remaining = remaining[end+len(close):]
	}
}

func evaluateReference(reference string, context Context) (string, error) {
	parts := strings.Split(reference, ".")
	switch {
	case len(parts) == 2 && parts[0] == "inputs":
		return context.Inputs[parts[1]], nil
	case len(parts) == 2 && parts[0] == "matrix":
		value, ok := context.Matrix[parts[1]]
		if !ok {
			return "", fmt.Errorf("expression references unavailable matrix value %q", parts[1])
		}
		return fmt.Sprint(value), nil
	case len(parts) == 4 && parts[0] == "steps" && parts[2] == "outputs":
		outputs, ok := findOutputs(context.Steps, parts[1])
		if !ok {
			return "", fmt.Errorf("expression references unavailable step %q", parts[1])
		}
		return outputs[parts[3]], nil
	case len(parts) == 4 && parts[0] == "needs" && parts[2] == "outputs":
		outputs, ok := findOutputs(context.Needs, parts[1])
		if !ok {
			return "", fmt.Errorf("expression references unavailable need %q", parts[1])
		}
		return outputs[parts[3]], nil
	default:
		return "", fmt.Errorf("unsupported expression %q", reference)
	}
}

func findOutputs(values map[string]map[string]string, name string) (map[string]string, bool) {
	for candidate, outputs := range values {
		if strings.EqualFold(candidate, name) {
			return outputs, true
		}
	}
	return nil, false
}

func endPosition(line, column int, text string) Position {
	end := Position{Line: line, Column: column}
	for _, r := range text {
		if r == '\n' {
			end.Line++
			end.Column = 1
		} else {
			end.Column++
		}
	}
	return end
}
