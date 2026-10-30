// A compile-time template is a string or value containing literal text and
// `${{ ... }}` expressions. This file finds those expression regions,
// evaluates or preserves them according to graph-time availability, prevents
// replacements from introducing new expression syntax, and substitutes static
// inputs. compile.go owns expression parsing, validation, and tree evaluation.

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

// evaluateCompileStringTemplate evaluates container image templates. A null
// complete expression renders as an empty image, disabling the container.
// Other complete expressions must produce strings; interpolated scalar values
// retain the normal template rendering rules.
func evaluateCompileStringTemplate(template string, context CompileContext) (string, error) {
	if expr, err := parseExpression(template, 1, 1); err == nil {
		value, err := evaluateCompile(expr, context)
		if err != nil {
			return "", err
		}
		if value == nil {
			return "", nil
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("expression resolved to %T, want string", value)
		}
		if strings.Contains(text, "${{") {
			return "", authoredErrorf("compile-time expression result contains expression syntax")
		}
		return text, nil
	}
	evaluated, err := evaluateCompileTemplate(template, context)
	if err != nil {
		return "", err
	}
	if strings.Contains(evaluated, "${{") {
		return "", authoredErrorf("compile-time expression result contains expression syntax")
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
				return "", authoredErrorf("compile-time expression result contains expression syntax")
			}
			evaluated.WriteString(replacement)
		}
		remaining = source[consumed:]
	}
}

// reduceAvailableCompileTemplate replaces statically available scalar
// subtrees in each template expression while preserving runtime-dependent
// subtrees. Values rendered outside an expression retain the same expression
// injection protection as EvaluateAvailableCompileTemplate. Besides event
// subtrees, it reduces expressions that read foldContext when one is named:
// matrix for service credentials, inputs for partial service templates.
func reduceAvailableCompileTemplate(template string, context CompileContext, foldContext string) (string, error) {
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
		if !nodeReferencesGitHubEventPayload(node) && !nodeReferencesContext(node, "event") && (foldContext == "" || !nodeReferencesContext(node, foldContext)) {
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
				return "", authoredErrorf("compile-time expression result contains expression syntax")
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

// SubstituteSecretSelectorInputs resolves secrets[<index>] selectors whose
// index reads workflow inputs, such as secrets[inputs.name], to literal secret
// names. Secret inventory requires static names, so this is the only runtime
// position where inputs are substituted. Other input reads stay in the source.
func SubstituteSecretSelectorInputs(template string, inputs map[string]any, inputsComplete bool) (string, error) {
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
		var rewritten strings.Builder
		copied := 0
		for i := 0; i+1 < len(tokens); i++ {
			if tokens[i].Kind != actionlint.TokenKindIdent || !strings.EqualFold(tokens[i].Value, "secrets") ||
				i > 0 && tokens[i-1].Kind == actionlint.TokenKindDot || tokens[i+1].Kind != actionlint.TokenKindLeftBracket {
				continue
			}
			end, depth := i+2, 1
			for ; end < len(tokens); end++ {
				switch tokens[end].Kind {
				case actionlint.TokenKindLeftBracket:
					depth++
				case actionlint.TokenKindRightBracket:
					depth--
				}
				if depth == 0 {
					break
				}
			}
			if end == len(tokens) {
				break
			}
			index := expressionSource[tokens[i+2].Offset:tokens[end].Offset]
			node, err := parseCompleteExpression(open + index + "}}")
			if err != nil || !nodeReferencesContext(node, "inputs") {
				i = end
				continue
			}
			value, available, err := evaluateCompileAvailable(Expression{Text: open + index + "}}"}, CompileContext{Inputs: inputs, InputsComplete: inputsComplete})
			name, isString := value.(string)
			if err != nil || !available || !isString {
				return "", fmt.Errorf("secret name must resolve from compile-known workflow inputs")
			}
			literal, _ := compileInputLiteral(name)
			rewritten.WriteString(expressionSource[copied:tokens[i+2].Offset])
			rewritten.WriteString(literal)
			copied = tokens[end].Offset
			i = end
		}
		rewritten.WriteString(expressionSource[copied:])
		substituted.WriteString(rewritten.String())
		remaining = source[consumed:]
	}
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
