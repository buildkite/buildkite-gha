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
