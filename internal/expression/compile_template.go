// Compile-time template interpolation, reduction, and input substitution.

package expression

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rhysd/actionlint"
)

// evaluateCompileTemplate substitutes supported graph-time expressions once.
func evaluateCompileTemplate(template string, context CompileContext) (string, error) {
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
		text := open + source[:consumed]
		value, err := evaluateCompile(Expression{Text: text}, context)
		if err != nil {
			return "", err
		}
		replacement, scalar := expressionString(value)
		if !scalar {
			return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
		}
		evaluated.WriteString(replacement)
		remaining = source[consumed:]
	}
}

// evaluateCompileStringTemplate evaluates a compile-time template whose
// complete-expression form must produce a string. Interpolated scalar values
// retain the normal template rendering rules.
func evaluateCompileStringTemplate(template string, context CompileContext) (string, error) {
	if expr, err := parseExpression(template, 1, 1); err == nil {
		value, err := evaluateCompile(expr, context)
		if err != nil {
			return "", err
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("expression resolved to %T, want string", value)
		}
		if strings.Contains(text, "${{") {
			return "", fmt.Errorf("compile-time expression result contains expression syntax")
		}
		return text, nil
	}
	evaluated, err := evaluateCompileTemplate(template, context)
	if err != nil {
		return "", err
	}
	if strings.Contains(evaluated, "${{") {
		return "", fmt.Errorf("compile-time expression result contains expression syntax")
	}
	return evaluated, nil
}

// evaluateAvailableCompileTemplate folds each graph-time expression that can be
// resolved independently and preserves expressions that need runtime context.
func evaluateAvailableCompileTemplate(template string, context CompileContext) (string, error) {
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
		value, err := evaluateCompile(Expression{Text: complete}, context)
		if err != nil {
			evaluated.WriteString(complete)
		} else {
			replacement, scalar := expressionString(value)
			if !scalar {
				return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
			}
			if introducesExpressionSyntax(evaluated.String(), replacement, source[consumed:]) {
				return "", fmt.Errorf("compile-time expression result contains expression syntax")
			}
			evaluated.WriteString(replacement)
		}
		remaining = source[consumed:]
	}
}

// reduceAvailableCompileTemplate replaces statically available scalar
// subtrees in each template expression while preserving runtime-dependent
// subtrees. Values rendered outside an expression retain the same expression
// injection protection as EvaluateAvailableCompileTemplate.
func reduceAvailableCompileTemplate(template string, context CompileContext) (string, error) {
	const open = "${{"
	var reduced strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			reduced.WriteString(remaining)
			return reduced.String(), nil
		}
		reduced.WriteString(remaining[:start])
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			return "", fmt.Errorf("invalid expression: %w", lexErr)
		}
		complete := open + source[:consumed]
		expr, err := parseExpression(complete, 1, 1)
		if err != nil {
			return "", err
		}
		node, err := parseCompileExpression(expr)
		if err != nil {
			return "", err
		}
		if !nodeReferencesGitHubEventPayload(node) && !nodeReferencesContext(node, "event") {
			reduced.WriteString(complete)
			remaining = source[consumed:]
			continue
		}
		if referencesWholeEvent(node) {
			reduced.WriteString(complete)
			remaining = source[consumed:]
			continue
		}
		value, available, err := evaluateCompileNodeAvailable(node, context)
		if err != nil {
			return "", err
		}
		if available {
			replacement, scalar := expressionString(value)
			if !scalar {
				return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
			}
			if introducesExpressionSyntax(reduced.String(), replacement, source[consumed:]) {
				return "", fmt.Errorf("compile-time expression result contains expression syntax")
			}
			reduced.WriteString(replacement)
		} else {
			reduced.WriteString(open)
			reduced.WriteString(" ")
			reduced.WriteString(reduceCompileNode(node, context))
			reduced.WriteString(" }}")
		}
		remaining = source[consumed:]
	}
}

func referencesWholeEvent(expression actionlint.ExprNode) bool {
	found := false
	actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
		if !entering || found {
			return
		}
		if projection, ok := node.(*actionlint.ArrayDerefNode); ok {
			root, path, err := referencePath(projection.Receiver)
			found = err == nil && (strings.EqualFold(root, "event") && len(path) == 0 ||
				strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "event"))
			if found {
				return
			}
		}
		if referenceReceiver(node, parent) {
			return
		}
		switch node.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.ArrayDerefNode, *actionlint.IndexAccessNode:
		default:
			return
		}
		root, path, err := referencePath(node)
		if err != nil {
			return
		}
		found = strings.EqualFold(root, "event") && len(path) == 0 ||
			strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "event")
	})
	return found
}

func nodeReferencesContext(expression actionlint.ExprNode, contextName string) bool {
	found := false
	actionlint.VisitExprNode(expression, func(node, _ actionlint.ExprNode, entering bool) {
		variable, ok := node.(*actionlint.VariableNode)
		found = found || entering && ok && strings.EqualFold(variable.Name, contextName)
	})
	return found
}

func introducesExpressionSyntax(before, replacement, after string) bool {
	if len(before) > 2 {
		before = before[len(before)-2:]
	}
	if len(after) > 2 {
		after = after[:2]
	}
	boundary, replacementEnd := len(before), len(before)+len(replacement)
	combined := before + replacement + after
	for offset := 0; offset < len(combined); {
		relative := strings.Index(combined[offset:], "${{")
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len("${{")
		if len(replacement) > 0 && start < replacementEnd && end > boundary || len(replacement) == 0 && start < boundary && end > boundary {
			return true
		}
		offset = start + 1
	}
	return false
}

// SubstituteCompileInputs replaces static inputs.<name> and inputs['name']
// references inside expression regions with equivalent GitHub expression
// literals. Text and string literals outside those references are preserved
// byte-for-byte.
func SubstituteCompileInputs(template string, inputs map[string]any) (string, error) {
	resolved := template
	for {
		next, err := substituteCompileInputsOnce(resolved, inputs)
		if err != nil {
			return "", err
		}
		if next == resolved {
			return next, nil
		}
		resolved = next
	}
}

func substituteCompileInputsOnce(template string, inputs map[string]any) (string, error) {
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
		for i := 0; i < len(tokens); i++ {
			if tokens[i].Kind != actionlint.TokenKindIdent || !strings.EqualFold(tokens[i].Value, "inputs") {
				continue
			}
			// A preceding '.' means this is a property named "inputs" on
			// another receiver, such as github.event.inputs.<name>, not the
			// workflow-call inputs context.
			if i > 0 && tokens[i-1].Kind == actionlint.TokenKindDot {
				continue
			}
			var name string
			var end, consumedTokens int
			switch {
			case i+2 < len(tokens) && tokens[i+1].Kind == actionlint.TokenKindDot && tokens[i+2].Kind == actionlint.TokenKindIdent:
				name = tokens[i+2].Value
				end = tokens[i+2].Offset + len(tokens[i+2].Value)
				consumedTokens = 2
			case i+3 < len(tokens) && tokens[i+1].Kind == actionlint.TokenKindLeftBracket && tokens[i+2].Kind == actionlint.TokenKindString && tokens[i+3].Kind == actionlint.TokenKindRightBracket:
				name = strings.ReplaceAll(strings.Trim(tokens[i+2].Value, "'"), "''", "'")
				end = tokens[i+3].Offset + 1
				consumedTokens = 3
			default:
				continue
			}
			value, ok := findCompileInput(inputs, name)
			if !ok {
				continue
			}
			literal, err := compileInputLiteral(value)
			if err != nil {
				return "", err
			}
			replacements = append(replacements, replacement{
				start: tokens[i].Offset,
				end:   end,
				value: literal,
			})
			i += consumedTokens
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
		return expressionNumberString(value), nil
	default:
		return "", fmt.Errorf("compile-time input %T cannot be represented as an expression literal", value)
	}
}
