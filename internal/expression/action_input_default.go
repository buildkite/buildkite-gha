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
	validator := newSemanticValidator(actionInputDefaultSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, path []string) error {
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
	}
	validator.referenceError = func(err error) error {
		return fmt.Errorf("runtime interpolation requires a direct context reference: %w", err)
	}
	validator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		if !kind.IsEqualityOp() {
			return fmt.Errorf("action input default comparison %s is unsupported", kind)
		}
		return nil
	}
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(_ *semanticValidator, node *actionlint.FuncCallNode) error {
		if !strings.EqualFold(node.Callee, "toJSON") || len(node.Args) != 1 {
			return fmt.Errorf("action input default function %q is unsupported", node.Callee)
		}
		root, path, err := referencePath(node.Args[0])
		if err != nil || !strings.EqualFold(root, "matrix") || len(path) != 0 {
			return fmt.Errorf("action input default toJSON requires the complete matrix context")
		}
		return nil
	}
	validator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("action input default expression is unsupported") }
	return validator.validate(node)
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
	evaluator := newSemanticEvaluator(actionInputDefaultSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		if isJobStatusReference(root, path) {
			if context.JobStatus == "" {
				return nil, fmt.Errorf("expression references unavailable job.status")
			}
			return context.JobStatus, nil
		}
		return resolveRuntimeReference(root, path, context)
	}
	evaluator.truthy = actionInputDefaultTruthy
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		equal := actionInputDefaultEqual(left, right)
		switch kind {
		case actionlint.CompareOpNodeKindEq:
			return equal, nil
		case actionlint.CompareOpNodeKindNotEq:
			return !equal, nil
		default:
			return nil, fmt.Errorf("action input default comparison %s is unsupported", kind)
		}
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("action input default expression is unsupported") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("action input default logical operator %s is unsupported", kind)
	}
	evaluator.call = func(_ *semanticEvaluator, node *actionlint.FuncCallNode) (any, error) {
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
	}
	return evaluator.evaluate(node)
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
