// Package expression adapts actionlint's expression parser into owned values.
package expression

import (
	"encoding/json"
	"fmt"
	"io"
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
	Inputs      map[string]string
	Matrix      map[string]any
	Steps       map[string]map[string]string
	Needs       map[string]map[string]string
	NeedResults map[string]string
}

// CompileContext contains the non-secret values available while constructing
// a workflow graph. Values are snapshots supplied by the compiler; evaluation
// never reads the process environment or a secret provider.
type CompileContext struct {
	GitHub map[string]any
	Event  map[string]any
	Vars   map[string]string
	Matrix map[string]any
}

// Parse validates a complete ${{ ... }} expression using actionlint and returns
// an owned representation.
func Parse(text string, line, column int) (Expression, error) {
	body, err := expressionBody(text)
	if err != nil {
		return Expression{}, err
	}
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

// EvaluateCompile evaluates one complete graph-time expression. The supported
// surface is intentionally limited to literals, github/event/vars/matrix
// references, and fromJSON applied to one of those values.
func EvaluateCompile(expr Expression, context CompileContext) (any, error) {
	body, err := expressionBody(expr.Text)
	if err != nil {
		return nil, err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return nil, fmt.Errorf("invalid expression: %w", parseErr)
	}
	return evaluateCompileNode(node, context)
}

// EvaluateCompileTemplate substitutes supported graph-time expressions once.
func EvaluateCompileTemplate(template string, context CompileContext) (string, error) {
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
			return "", fmt.Errorf("unterminated expression")
		}
		end += start + len(open)
		text := remaining[start : end+len(close)]
		value, err := EvaluateCompile(Expression{Text: text}, context)
		if err != nil {
			return "", err
		}
		switch value := value.(type) {
		case nil:
		case string:
			evaluated.WriteString(value)
		case bool, json.Number, float64, int:
			_, _ = fmt.Fprint(&evaluated, value)
		default:
			return "", fmt.Errorf("expression in runner label resolved to %T, want a scalar", value)
		}
		remaining = remaining[end+len(close):]
	}
}

func expressionBody(text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "${{") || !strings.HasSuffix(trimmed, "}}") {
		return "", fmt.Errorf("expected a complete ${{ ... }} expression")
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "${{"), "}}"))
	if strings.Contains(body, "}}") {
		return "", fmt.Errorf("expression contains an embedded closing delimiter")
	}
	return body, nil
}

func evaluateCompileNode(node actionlint.ExprNode, context CompileContext) (any, error) {
	switch node := node.(type) {
	case *actionlint.NullNode:
		return nil, nil
	case *actionlint.BoolNode:
		return node.Value, nil
	case *actionlint.IntNode:
		return node.Value, nil
	case *actionlint.FloatNode:
		return node.Value, nil
	case *actionlint.StringNode:
		return node.Value, nil
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return nil, err
		}
		return resolveCompileReference(root, path, context)
	case *actionlint.FuncCallNode:
		if !strings.EqualFold(node.Callee, "fromJSON") || len(node.Args) != 1 {
			return nil, fmt.Errorf("unsupported compile-time function %q", node.Callee)
		}
		value, err := evaluateCompileNode(node.Args[0], context)
		if err != nil {
			return nil, err
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("fromJSON argument resolved to %T, want string", value)
		}
		return decodeJSONValue(text)
	default:
		return nil, fmt.Errorf("unsupported compile-time expression")
	}
}

func referencePath(node actionlint.ExprNode) (string, []string, error) {
	switch node := node.(type) {
	case *actionlint.VariableNode:
		return node.Name, nil, nil
	case *actionlint.ObjectDerefNode:
		root, path, err := referencePath(node.Receiver)
		return root, append(path, node.Property), err
	case *actionlint.IndexAccessNode:
		root, path, err := referencePath(node.Operand)
		if err != nil {
			return "", nil, err
		}
		index, ok := node.Index.(*actionlint.StringNode)
		if !ok {
			return "", nil, fmt.Errorf("compile-time index must be a string literal")
		}
		return root, append(path, index.Value), nil
	default:
		return "", nil, fmt.Errorf("unsupported compile-time reference")
	}
}

func resolveCompileReference(root string, path []string, context CompileContext) (any, error) {
	var current any
	switch {
	case strings.EqualFold(root, "github"):
		current = context.GitHub
	case strings.EqualFold(root, "event"):
		current = context.Event
	case strings.EqualFold(root, "vars"):
		current = context.Vars
	case strings.EqualFold(root, "matrix"):
		current = context.Matrix
	default:
		return nil, fmt.Errorf("unsupported compile-time context %q", root)
	}
	for _, part := range path {
		var (
			ok  bool
			err error
		)
		current, ok, err = objectValue(current, part)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("compile-time expression references unavailable value %q", root+"."+strings.Join(path, "."))
		}
	}
	return current, nil
}

func objectValue(value any, name string) (any, bool, error) {
	switch value := value.(type) {
	case map[string]any:
		var (
			found      any
			matchedKey string
		)
		for candidate, item := range value {
			if strings.EqualFold(candidate, name) {
				if matchedKey != "" {
					return nil, false, fmt.Errorf("compile-time object contains ambiguous properties %q and %q", matchedKey, candidate)
				}
				found, matchedKey = item, candidate
			}
		}
		if matchedKey != "" {
			return found, true, nil
		}
	case map[string]string:
		var (
			found      string
			matchedKey string
		)
		for candidate, item := range value {
			if strings.EqualFold(candidate, name) {
				if matchedKey != "" {
					return nil, false, fmt.Errorf("compile-time object contains ambiguous properties %q and %q", matchedKey, candidate)
				}
				found, matchedKey = item, candidate
			}
		}
		if matchedKey != "" {
			return found, true, nil
		}
	}
	return nil, false, nil
}

func decodeJSONValue(source string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("fromJSON argument is invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("fromJSON argument contains multiple JSON values")
	}
	return value, nil
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
	case len(parts) == 3 && parts[0] == "needs" && parts[2] == "result":
		for candidate, result := range context.NeedResults {
			if strings.EqualFold(candidate, parts[1]) {
				return result, nil
			}
		}
		return "", fmt.Errorf("expression references unavailable need %q", parts[1])
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
