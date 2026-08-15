// Compile-time expression evaluation: graph-construction substitution and
// evaluation against the snapshotted CompileContext.
package expression

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rhysd/actionlint"
)

// CompileContext contains the non-secret values available while constructing
// a workflow graph. Values are snapshots supplied by the compiler; evaluation
// never reads the process environment or a secret provider.
type CompileContext struct {
	GitHub map[string]any
	Event  map[string]any
	Vars   map[string]string
	Matrix map[string]any
}

// EvaluateCompile evaluates one complete graph-time expression. The supported
// surface is intentionally limited to literals, github/event/vars/matrix
// references, boolean/equality operators, and selected pure functions.
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

// EvaluateCompileCondition evaluates a condition whose entire value is known
// while constructing the graph. Callers may fall back to runtime condition
// handling when this returns an unavailable-context or unsupported error.
func EvaluateCompileCondition(source string, context CompileContext) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil {
		return false, err
	}
	if empty {
		return true, nil
	}
	value, err := evaluateCompileNode(node, context)
	if err != nil {
		return false, err
	}
	return actionInputDefaultTruthy(value), nil
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
			return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
		}
		remaining = remaining[end+len(close):]
	}
}

// EvaluateAvailableCompileTemplate folds each graph-time expression that can be
// resolved independently and preserves expressions that need runtime context.
func EvaluateAvailableCompileTemplate(template string, context CompileContext) (string, error) {
	const open = "${{"
	var evaluated strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			evaluated.WriteString(remaining)
			return evaluated.String(), nil
		}
		evaluated.WriteString(remaining[:start])
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			return "", fmt.Errorf("invalid expression: %w", lexErr)
		}
		complete := open + source[:consumed]
		value, err := EvaluateCompile(Expression{Text: complete}, context)
		if err != nil {
			evaluated.WriteString(complete)
		} else {
			switch value := value.(type) {
			case nil:
			case string:
				evaluated.WriteString(value)
			case bool, json.Number, float64, int:
				_, _ = fmt.Fprint(&evaluated, value)
			default:
				return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
			}
		}
		remaining = source[consumed:]
	}
}

// SubstituteCompileInputs replaces static inputs.<name> references inside
// expression regions with equivalent GitHub expression literals. Text and
// string literals outside those references are preserved byte-for-byte.
func SubstituteCompileInputs(template string, inputs map[string]any) (string, error) {
	const open = "${{"
	var substituted strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			substituted.WriteString(remaining)
			return substituted.String(), nil
		}
		substituted.WriteString(remaining[:start+len(open)])
		source := remaining[start+len(open):]
		tokens, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			return "", fmt.Errorf("invalid expression: %w", lexErr)
		}
		expressionSource := source[:consumed]
		type replacement struct {
			start, end int
			value      string
		}
		var replacements []replacement
		for i := 0; i+2 < len(tokens); i++ {
			if tokens[i].Kind != actionlint.TokenKindIdent || !strings.EqualFold(tokens[i].Value, "inputs") || tokens[i+1].Kind != actionlint.TokenKindDot || tokens[i+2].Kind != actionlint.TokenKindIdent {
				continue
			}
			value, ok := findCompileInput(inputs, tokens[i+2].Value)
			if !ok {
				continue
			}
			literal, err := compileInputLiteral(value)
			if err != nil {
				return "", err
			}
			replacements = append(replacements, replacement{
				start: tokens[i].Offset,
				end:   tokens[i+2].Offset + len(tokens[i+2].Value),
				value: literal,
			})
			i += 2
		}
		for i := len(replacements) - 1; i >= 0; i-- {
			replacement := replacements[i]
			expressionSource = expressionSource[:replacement.start] + replacement.value + expressionSource[replacement.end:]
		}
		substituted.WriteString(expressionSource)
		remaining = source[consumed:]
	}
}

func findCompileInput(inputs map[string]any, target string) (any, bool) {
	for name, value := range inputs {
		if strings.EqualFold(name, target) {
			return value, true
		}
	}
	return nil, false
}

func compileInputLiteral(value any) (string, error) {
	switch value := value.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(value), nil
	case string:
		return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
	case json.Number:
		return value.String(), nil
	case int:
		return strconv.Itoa(value), nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("compile-time input %T cannot be represented as an expression literal", value)
	}
}

func evaluateCompileNode(node actionlint.ExprNode, context CompileContext) (any, error) {
	evaluator := newSemanticEvaluator(compileTimeSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		return resolveCompileReference(root, path, context)
	}
	evaluator.truthy = actionInputDefaultTruthy
	evaluator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		if !kind.IsEqualityOp() {
			return fmt.Errorf("unsupported compile-time comparison %s", kind)
		}
		return nil
	}
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		equal := actionInputDefaultEqual(left, right)
		if kind == actionlint.CompareOpNodeKindNotEq {
			return !equal, nil
		}
		return equal, nil
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported compile-time expression") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("unsupported compile-time logical operator %s", kind)
	}
	evaluator.call = func(evaluator *semanticEvaluator, node *actionlint.FuncCallNode) (any, error) {
		switch {
		case strings.EqualFold(node.Callee, "fromJSON") && len(node.Args) == 1:
			value, err := evaluator.evaluate(node.Args[0])
			if err != nil {
				return nil, err
			}
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("fromJSON argument resolved to %T, want string", value)
			}
			return decodeJSONValue(text)
		case (strings.EqualFold(node.Callee, "startsWith") || strings.EqualFold(node.Callee, "contains") || strings.EqualFold(node.Callee, "endsWith")) && len(node.Args) == 2:
			value, err := evaluator.evaluate(node.Args[0])
			if err != nil {
				return nil, err
			}
			search, err := evaluator.evaluate(node.Args[1])
			if err != nil {
				return nil, err
			}
			valueText, valueOK := value.(string)
			searchText, searchOK := search.(string)
			if !valueOK || !searchOK {
				return nil, fmt.Errorf("%s arguments resolved to %T and %T, want strings", node.Callee, value, search)
			}
			valueText, searchText = strings.ToLower(valueText), strings.ToLower(searchText)
			switch {
			case strings.EqualFold(node.Callee, "startsWith"):
				return strings.HasPrefix(valueText, searchText), nil
			case strings.EqualFold(node.Callee, "contains"):
				return strings.Contains(valueText, searchText), nil
			default:
				return strings.HasSuffix(valueText, searchText), nil
			}
		default:
			return nil, fmt.Errorf("unsupported compile-time function %q", node.Callee)
		}
	}
	return evaluator.evaluate(node)
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

func resolveServicePort(services map[string]map[string]string, service, port, kind string) (string, error) {
	for id, ports := range services {
		if strings.EqualFold(id, service) {
			if value, ok := ports[port]; ok {
				return value, nil
			}
			return "", fmt.Errorf("%s references unavailable service port %s.%s", kind, service, port)
		}
	}
	return "", fmt.Errorf("%s references unavailable service %q", kind, service)
}
