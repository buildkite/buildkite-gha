// Package expression adapts actionlint's expression parser into owned values.
package expression

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
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

// StepStatus contains the values exposed for one completed step while
// evaluating a condition.
type StepStatus struct {
	Outcome    string
	Conclusion string
	Outputs    map[string]string
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

func parseCondition(source string) (actionlint.ExprNode, bool, error) {
	condition := strings.TrimSpace(source)
	if condition == "" {
		return nil, true, nil
	}
	if strings.HasPrefix(condition, "${{") && strings.HasSuffix(condition, "}}") {
		condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	}
	node, err := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(condition + "}}"))
	if err != nil {
		return nil, false, fmt.Errorf("parse condition: %w", err)
	}
	return node, false, nil
}

func visitTemplateExpressions(template string, visit func(actionlint.ExprNode) error) error {
	const open = "${{"
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			return nil
		}
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			return fmt.Errorf("invalid expression: %w", lexErr)
		}
		node, err := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(source[:consumed]))
		if err != nil {
			return fmt.Errorf("invalid expression: %w", err)
		}
		if err := visit(node); err != nil {
			return err
		}
		remaining = source[consumed:]
	}
}

func githubTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case float32:
		return !math.IsNaN(float64(value)) && value != 0
	case float64:
		return !math.IsNaN(value) && value != 0
	}
	if number, ok := githubNumber(value); ok {
		return !math.IsNaN(number) && number != 0
	}
	return true
}

func githubEqual(left, right any) bool {
	if left == nil && right == nil {
		return true
	}
	switch left := left.(type) {
	case string:
		if right, ok := right.(string); ok {
			return strings.EqualFold(left, right)
		}
	case bool:
		if right, ok := right.(bool); ok {
			return left == right
		}
	}
	if left != nil && right != nil && reflect.TypeOf(left) == reflect.TypeOf(right) {
		leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
		switch leftValue.Kind() {
		case reflect.Map:
			return leftValue.UnsafePointer() == rightValue.UnsafePointer()
		case reflect.Pointer, reflect.Slice:
			return leftValue.Pointer() == rightValue.Pointer()
		}
	}
	leftNumber, leftOK := githubNumber(left)
	rightNumber, rightOK := githubNumber(right)
	return leftOK && rightOK && !math.IsNaN(leftNumber) && !math.IsNaN(rightNumber) && leftNumber == rightNumber
}

func githubOrderedCompare(left, right any) (int, bool) {
	leftString, leftIsString := left.(string)
	rightString, rightIsString := right.(string)
	if leftIsString && rightIsString {
		return strings.Compare(strings.ToLower(leftString), strings.ToLower(rightString)), true
	}
	leftNumber, leftOK := githubNumber(left)
	rightNumber, rightOK := githubNumber(right)
	if !leftOK || !rightOK {
		return 0, false
	}
	switch {
	case math.IsNaN(leftNumber) || math.IsNaN(rightNumber):
		return 0, false
	case leftNumber < rightNumber:
		return -1, true
	case leftNumber > rightNumber:
		return 1, true
	default:
		return 0, true
	}
}

func githubCompare(kind actionlint.CompareOpNodeKind, left, right any) (bool, error) {
	switch kind {
	case actionlint.CompareOpNodeKindEq:
		return githubEqual(left, right), nil
	case actionlint.CompareOpNodeKindNotEq:
		return !githubEqual(left, right), nil
	}
	comparison, ok := githubOrderedCompare(left, right)
	if !ok {
		return false, nil
	}
	switch kind {
	case actionlint.CompareOpNodeKindLess:
		return comparison < 0, nil
	case actionlint.CompareOpNodeKindLessEq:
		return comparison <= 0, nil
	case actionlint.CompareOpNodeKindGreater:
		return comparison > 0, nil
	case actionlint.CompareOpNodeKindGreaterEq:
		return comparison >= 0, nil
	default:
		return false, fmt.Errorf("unsupported comparison %s", kind)
	}
}

func githubNumber(value any) (float64, bool) {
	switch value := value.(type) {
	case nil:
		return 0, true
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	case json.Number:
		parsed, err := strconv.ParseFloat(value.String(), 64)
		return parsed, err == nil || math.IsInf(parsed, 0)
	case string:
		if strings.TrimSpace(value) == "" {
			return 0, true
		}
		decoded, err := decodeJSONValue(value)
		if err != nil {
			return math.NaN(), true
		}
		number, ok := decoded.(json.Number)
		if !ok {
			return math.NaN(), true
		}
		parsed, err := strconv.ParseFloat(number.String(), 64)
		return parsed, err == nil || math.IsInf(parsed, 0)
	default:
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return float64(reflected.Int()), true
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return float64(reflected.Uint()), true
		case reflect.Float32, reflect.Float64:
			return reflected.Float(), true
		default:
			return math.NaN(), false
		}
	}
}

func containsStatusFunction(node actionlint.ExprNode) bool {
	found := false
	actionlint.VisitExprNode(node, func(node, _ actionlint.ExprNode, entering bool) {
		if !entering {
			return
		}
		call, ok := node.(*actionlint.FuncCallNode)
		if ok {
			switch strings.ToLower(call.Callee) {
			case "always", "success", "failure", "cancelled":
				found = true
			}
		}
	})
	return found
}

func expressionBody(text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "${{") {
		return "", fmt.Errorf("expected a complete ${{ ... }} expression")
	}
	source := strings.TrimPrefix(trimmed, "${{")
	_, consumed, err := actionlint.LexExpression(source)
	if err != nil {
		return "", fmt.Errorf("invalid expression: %w", err)
	}
	if consumed != len(source) {
		return "", fmt.Errorf("expression contains an embedded closing delimiter")
	}
	return strings.TrimSpace(strings.TrimSuffix(source[:consumed], "}}")), nil
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
		switch index := node.Index.(type) {
		case *actionlint.StringNode:
			return root, append(path, index.Value), nil
		case *actionlint.IntNode:
			if !strings.EqualFold(root, "job") || len(path) != 3 || !strings.EqualFold(path[0], "services") || !strings.EqualFold(path[2], "ports") {
				return root, nil, fmt.Errorf("expression variable %q does not support numeric indices", root)
			}
			return root, append(path, fmt.Sprint(index.Value)), nil
		default:
			return root, nil, fmt.Errorf("expression index must be a string literal or integer literal")
		}
	default:
		return "", nil, fmt.Errorf("unsupported expression reference")
	}
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
					return nil, false, fmt.Errorf("compile-time object contains ambiguous properties")
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
					return nil, false, fmt.Errorf("compile-time object contains ambiguous properties")
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
		return nil, fmt.Errorf("fromJSON argument is invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("fromJSON argument contains multiple JSON values")
	}
	return value, nil
}

func isJobStatusReference(root string, path []string) bool {
	return len(path) == 1 && strings.EqualFold(root, "job") && strings.EqualFold(path[0], "status")
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
