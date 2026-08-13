// Action input default validation and evaluation with GitHub's loose
// coercion semantics. Deliberately separate from the strict condition
// family in condition.go.
package expression

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"

	"github.com/rhysd/actionlint"
)

// ValidateActionInputDefault verifies the restricted compound expression
// surface supported only while evaluating action metadata input defaults.
func ValidateActionInputDefault(template string) error {
	referencesJobStatus, err := ReferencesJobStatus(template)
	if err != nil {
		return err
	}
	if referencesJobStatus {
		root, path, err := ReferencePath(template)
		if err != nil || !isJobStatusReference(root, path) {
			return fmt.Errorf("action input default job.status must be one direct expression")
		}
	}
	return visitTemplateExpressions(template, validateActionInputDefaultNode)
}

func validateActionInputDefaultNode(node actionlint.ExprNode) error {
	switch node := node.(type) {
	case *actionlint.NullNode, *actionlint.BoolNode, *actionlint.IntNode, *actionlint.FloatNode, *actionlint.StringNode:
		return nil
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return fmt.Errorf("runtime interpolation requires a direct context reference: %w", err)
		}
		if isJobStatusReference(root, path) {
			return nil
		}
		kind := classifyRuntimeReference(root, path)
		if kind == runtimeReferenceSecret {
			return fmt.Errorf("action input defaults cannot grant secret authority")
		}
		if kind == runtimeReferenceUnsupported {
			return fmt.Errorf("unsupported runtime expression %q", referenceName(root, path))
		}
		return nil
	case *actionlint.LogicalOpNode:
		if err := validateActionInputDefaultNode(node.Left); err != nil {
			return err
		}
		return validateActionInputDefaultNode(node.Right)
	case *actionlint.CompareOpNode:
		if !node.Kind.IsEqualityOp() {
			return fmt.Errorf("action input default comparison %s is unsupported", node.Kind)
		}
		if err := validateActionInputDefaultNode(node.Left); err != nil {
			return err
		}
		return validateActionInputDefaultNode(node.Right)
	case *actionlint.FuncCallNode:
		if !strings.EqualFold(node.Callee, "toJSON") || len(node.Args) != 1 {
			return fmt.Errorf("action input default function %q is unsupported", node.Callee)
		}
		root, path, err := referencePath(node.Args[0])
		if err != nil || !strings.EqualFold(root, "matrix") || len(path) != 0 {
			return fmt.Errorf("action input default toJSON requires the complete matrix context")
		}
		return nil
	default:
		return fmt.Errorf("action input default expression is unsupported")
	}
}

// EvaluateActionInputDefault substitutes the restricted compound expressions
// supported only in action metadata input defaults.
func EvaluateActionInputDefault(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateActionInputDefaultNode)
}

// ActionInputDefaultRequiresGitHubToken reports whether a metadata default can
// reach github.token for the event provider. Defaults involving any other
// runtime value fail closed because those values are not known during
// compilation.
func ActionInputDefaultRequiresGitHubToken(template, serverURL string) (bool, error) {
	referencesToken, err := ReferencesGitHubToken(template)
	if err != nil || !referencesToken {
		return referencesToken, err
	}
	onlyKnownReferences := true
	err = visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
			if !entering || !onlyKnownReferences {
				return
			}
			switch node.(type) {
			case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
			default:
				return
			}
			root, path, referenceErr := referencePath(node)
			if referenceErr != nil {
				onlyKnownReferences = false
				return
			}
			if len(path) == 0 {
				switch parent := parent.(type) {
				case *actionlint.ObjectDerefNode:
					if parent.Receiver == node {
						return
					}
				case *actionlint.IndexAccessNode:
					if parent.Operand == node {
						return
					}
				}
			}
			onlyKnownReferences = strings.EqualFold(root, "github") && len(path) == 1 &&
				(strings.EqualFold(path[0], "server_url") || strings.EqualFold(path[0], "token"))
		})
		return nil
	})
	if err != nil {
		return false, err
	}
	if !onlyKnownReferences {
		return true, nil
	}
	_, err = EvaluateActionInputDefault(template, Context{GitHub: map[string]any{"server_url": serverURL}})
	return err != nil, nil
}

func evaluateActionInputDefaultNode(node actionlint.ExprNode, context Context) (any, error) {
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
		if isJobStatusReference(root, path) {
			if context.JobStatus == "" {
				return nil, fmt.Errorf("expression references unavailable job.status")
			}
			return context.JobStatus, nil
		}
		return resolveRuntimeReference(root, path, context)
	case *actionlint.LogicalOpNode:
		left, err := evaluateActionInputDefaultNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		switch node.Kind {
		case actionlint.LogicalOpNodeKindAnd:
			if !actionInputDefaultTruthy(left) {
				return left, nil
			}
			return evaluateActionInputDefaultNode(node.Right, context)
		case actionlint.LogicalOpNodeKindOr:
			if actionInputDefaultTruthy(left) {
				return left, nil
			}
			return evaluateActionInputDefaultNode(node.Right, context)
		default:
			return nil, fmt.Errorf("action input default logical operator %s is unsupported", node.Kind)
		}
	case *actionlint.CompareOpNode:
		left, err := evaluateActionInputDefaultNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		right, err := evaluateActionInputDefaultNode(node.Right, context)
		if err != nil {
			return nil, err
		}
		equal := actionInputDefaultEqual(left, right)
		switch node.Kind {
		case actionlint.CompareOpNodeKindEq:
			return equal, nil
		case actionlint.CompareOpNodeKindNotEq:
			return !equal, nil
		default:
			return nil, fmt.Errorf("action input default comparison %s is unsupported", node.Kind)
		}
	case *actionlint.FuncCallNode:
		if !strings.EqualFold(node.Callee, "toJSON") || len(node.Args) != 1 {
			return nil, fmt.Errorf("action input default function %q is unsupported", node.Callee)
		}
		root, path, err := referencePath(node.Args[0])
		if err != nil || !strings.EqualFold(root, "matrix") || len(path) != 0 {
			return nil, fmt.Errorf("action input default toJSON requires the complete matrix context")
		}
		value, err := json.MarshalIndent(context.Matrix, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode action input default matrix as JSON: %w", err)
		}
		return string(value), nil
	default:
		return nil, fmt.Errorf("action input default expression is unsupported")
	}
}

// The actionInputDefault* helpers are the loose coercion family that mirrors
// GitHub's expression semantics for action input defaults: nil and empty
// strings coerce to zero, booleans coerce to numbers, and same-typed
// aggregates compare by identity. Runtime conditions deliberately use the
// strict condition* family instead; keep the two separate.
func actionInputDefaultTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	}
	if number, ok := conditionNumber(value); ok {
		return number.Sign() != 0
	}
	return true
}

func actionInputDefaultEqual(left, right any) bool {
	if left == nil && right == nil {
		return true
	}
	if leftNumber, leftOK := conditionNumber(left); leftOK {
		if rightNumber, rightOK := conditionNumber(right); rightOK {
			return leftNumber.Cmp(rightNumber) == 0
		}
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
		case reflect.Map, reflect.Pointer, reflect.Slice:
			return leftValue.Pointer() == rightValue.Pointer()
		}
	}
	leftNumber, leftOK := actionInputDefaultNumber(left)
	rightNumber, rightOK := actionInputDefaultNumber(right)
	return leftOK && rightOK && leftNumber.Cmp(rightNumber) == 0
}

func actionInputDefaultNumber(value any) (*big.Rat, bool) {
	switch value := value.(type) {
	case nil:
		return new(big.Rat), true
	case bool:
		if value {
			return big.NewRat(1, 1), true
		}
		return new(big.Rat), true
	case string:
		if strings.TrimSpace(value) == "" {
			return new(big.Rat), true
		}
		decoded, err := decodeJSONValue(value)
		if err != nil {
			return nil, false
		}
		number, ok := decoded.(json.Number)
		if !ok {
			return nil, false
		}
		return conditionNumber(number)
	default:
		return conditionNumber(value)
	}
}
