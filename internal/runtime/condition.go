package runtime

import (
	"fmt"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/rhysd/actionlint"
)

type stepStatus struct {
	Outcome    string
	Conclusion string
	Outputs    map[string]string
}

type conditionContext struct {
	needs        map[string]plan.Need
	steps        map[string]stepStatus
	env          map[string]string
	vars         map[string]string
	matrix       map[string]any
	github       map[string]any
	failure      bool
	cancelled    bool
	unsuccessful bool
}

func evaluateCondition(source string, context conditionContext) (bool, error) {
	condition := strings.TrimSpace(source)
	if condition == "" {
		return !context.unsuccessful && !context.cancelled, nil
	}
	if strings.HasPrefix(condition, "${{") && strings.HasSuffix(condition, "}}") {
		condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(condition + "}}"))
	if parseErr != nil {
		return false, fmt.Errorf("parse condition: %w", parseErr)
	}
	value, err := evaluateConditionNode(node, context)
	if err != nil {
		return false, err
	}
	result := conditionTruthy(value)
	if !containsStatusFunction(node) && (context.unsuccessful || context.cancelled) {
		return false, nil
	}
	return result, nil
}

func evaluateConditionNode(node actionlint.ExprNode, context conditionContext) (any, error) {
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
		root, path, err := conditionReference(node)
		if err != nil {
			return nil, err
		}
		return resolveConditionReference(root, path, context)
	case *actionlint.NotOpNode:
		value, err := evaluateConditionNode(node.Operand, context)
		if err != nil {
			return nil, err
		}
		return !conditionTruthy(value), nil
	case *actionlint.LogicalOpNode:
		left, err := evaluateConditionNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		leftBool := conditionTruthy(left)
		if node.Kind == actionlint.LogicalOpNodeKindAnd && !leftBool {
			return false, nil
		}
		if node.Kind == actionlint.LogicalOpNodeKindOr && leftBool {
			return true, nil
		}
		right, err := evaluateConditionNode(node.Right, context)
		if err != nil {
			return nil, err
		}
		return conditionTruthy(right), nil
	case *actionlint.CompareOpNode:
		left, err := evaluateConditionNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		right, err := evaluateConditionNode(node.Right, context)
		if err != nil {
			return nil, err
		}
		equal, err := conditionEqual(left, right)
		if err != nil {
			return nil, err
		}
		switch node.Kind {
		case actionlint.CompareOpNodeKindEq:
			return equal, nil
		case actionlint.CompareOpNodeKindNotEq:
			return !equal, nil
		default:
			return nil, fmt.Errorf("condition comparison %s is unsupported", node.Kind)
		}
	case *actionlint.FuncCallNode:
		if len(node.Args) != 0 {
			return nil, fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
		}
		switch strings.ToLower(node.Callee) {
		case "always":
			return true, nil
		case "success":
			return !context.unsuccessful && !context.cancelled, nil
		case "failure":
			return context.failure, nil
		case "cancelled":
			return context.cancelled, nil
		default:
			return nil, fmt.Errorf("condition function %q is unsupported", node.Callee)
		}
	default:
		return nil, fmt.Errorf("unsupported condition expression")
	}
}

func conditionTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case int:
		return value != 0
	case float64:
		return value != 0
	case string:
		return value != ""
	default:
		return false
	}
}

func conditionEqual(left, right any) (bool, error) {
	switch left := left.(type) {
	case nil:
		return right == nil, nil
	case string:
		right, ok := right.(string)
		if !ok {
			return false, fmt.Errorf("mixed-type condition equality is unsupported")
		}
		return strings.EqualFold(left, right), nil
	case bool:
		right, ok := right.(bool)
		if !ok {
			return false, fmt.Errorf("mixed-type condition equality is unsupported")
		}
		return left == right, nil
	case int:
		switch right := right.(type) {
		case int:
			return left == right, nil
		case float64:
			return float64(left) == right, nil
		}
	case float64:
		switch right := right.(type) {
		case int:
			return left == float64(right), nil
		case float64:
			return left == right, nil
		}
	}
	return false, fmt.Errorf("mixed-type condition equality is unsupported")
}

func conditionReference(node actionlint.ExprNode) (string, []string, error) {
	switch node := node.(type) {
	case *actionlint.VariableNode:
		return node.Name, nil, nil
	case *actionlint.ObjectDerefNode:
		root, path, err := conditionReference(node.Receiver)
		return root, append(path, node.Property), err
	case *actionlint.IndexAccessNode:
		root, path, err := conditionReference(node.Operand)
		if err != nil {
			return "", nil, err
		}
		index, ok := node.Index.(*actionlint.StringNode)
		if !ok {
			return "", nil, fmt.Errorf("condition index must be a string literal")
		}
		return root, append(path, index.Value), nil
	default:
		return "", nil, fmt.Errorf("unsupported condition reference")
	}
}

func resolveConditionReference(root string, path []string, context conditionContext) (any, error) {
	if strings.EqualFold(root, "github") {
		if value, ok := lookupConditionValue(context.github, path); ok {
			return value, nil
		}
	}
	if len(path) == 1 && strings.EqualFold(root, "env") {
		return conditionString(context.env, path[0]), nil
	}
	if len(path) == 1 && strings.EqualFold(root, "vars") {
		return conditionString(context.vars, path[0]), nil
	}
	if len(path) == 1 && strings.EqualFold(root, "matrix") {
		for name, value := range context.matrix {
			if strings.EqualFold(name, path[0]) {
				return value, nil
			}
		}
	}
	if strings.EqualFold(root, "needs") && len(path) == 2 && strings.EqualFold(path[1], "result") {
		for name, need := range context.needs {
			if strings.EqualFold(name, path[0]) {
				return need.Result, nil
			}
		}
	}
	if strings.EqualFold(root, "needs") && len(path) == 3 && strings.EqualFold(path[1], "outputs") {
		for name, need := range context.needs {
			if strings.EqualFold(name, path[0]) {
				return conditionString(need.Outputs, path[2]), nil
			}
		}
	}
	if strings.EqualFold(root, "steps") && len(path) == 2 {
		for name, step := range context.steps {
			if !strings.EqualFold(name, path[0]) {
				continue
			}
			switch {
			case strings.EqualFold(path[1], "outcome"):
				return step.Outcome, nil
			case strings.EqualFold(path[1], "conclusion"):
				return step.Conclusion, nil
			}
		}
	}
	if strings.EqualFold(root, "steps") && len(path) == 3 && strings.EqualFold(path[1], "outputs") {
		for name, step := range context.steps {
			if strings.EqualFold(name, path[0]) {
				return conditionString(step.Outputs, path[2]), nil
			}
		}
	}
	return nil, fmt.Errorf("condition references unavailable value %s.%s", root, strings.Join(path, "."))
}

func lookupConditionValue(value any, path []string) (any, bool) {
	current := value
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		matched := false
		for name, item := range object {
			if strings.EqualFold(name, part) {
				current, matched = item, true
				break
			}
		}
		if !matched {
			return nil, false
		}
	}
	return current, true
}

func conditionString(values map[string]string, name string) string {
	for candidate, value := range values {
		if strings.EqualFold(candidate, name) {
			return value
		}
	}
	return ""
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
