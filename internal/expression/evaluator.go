package expression

import (
	"fmt"
	"math"
	"strings"

	"github.com/rhysd/actionlint"
)

// semanticEvaluator owns expression-tree traversal while each fixed surface
// supplies its existing reference, comparison, function, and error behavior.
// Validation and authority analysis remain separate and run at their existing
// call sites.
type semanticEvaluator struct {
	policy          evaluationPolicy
	resolve         func(string, []string) (any, error)
	resolveRoot     func(string) (any, error)
	truthy          func(any) bool
	validateCompare func(actionlint.CompareOpNodeKind) error
	compare         func(actionlint.CompareOpNodeKind, any, any) (any, error)
	call            func(*semanticEvaluator, *actionlint.FuncCallNode) (any, error)
	unsupported     func(actionlint.ExprNode) error
	logicalError    func(actionlint.LogicalOpNodeKind) error
}

type evaluationSurface uint8

const (
	compileTimeSurface evaluationSurface = iota
	conditionSurface
	runtimeReferenceSurface
	actionInputDefaultSurface
	stepRuntimeSurface
)

type evaluationPolicy struct {
	allowLiterals bool
	allowNot      bool
	allowLogical  bool
	allowCompare  bool
	allowFunction bool
	logicalBool   bool
}

func newSemanticEvaluator(surface evaluationSurface) semanticEvaluator {
	var policy evaluationPolicy
	switch surface {
	case compileTimeSurface:
		policy = evaluationPolicy{allowLiterals: true, allowNot: true, allowLogical: true, allowCompare: true, allowFunction: true}
	case conditionSurface:
		policy = evaluationPolicy{allowLiterals: true, allowNot: true, allowLogical: true, allowCompare: true, allowFunction: true}
	case actionInputDefaultSurface:
		policy = evaluationPolicy{allowLiterals: true, allowLogical: true, allowCompare: true, allowFunction: true}
	case stepRuntimeSurface:
		policy = evaluationPolicy{allowLiterals: true, allowNot: true, allowLogical: true, allowCompare: true, allowFunction: true}
	case runtimeReferenceSurface:
	}
	return semanticEvaluator{policy: policy}
}

type semanticValidator struct {
	policy            evaluationPolicy
	validateReference func(actionlint.ExprNode, string, []string) error
	validateAccess    func(actionlint.ExprNode) error
	referenceError    func(error) error
	validateCompare   func(actionlint.CompareOpNodeKind) error
	afterCompare      func(*actionlint.CompareOpNode) error
	validateCall      func(*semanticValidator, *actionlint.FuncCallNode) error
	unsupported       func(actionlint.ExprNode) error
}

func newSemanticValidator(surface evaluationSurface) semanticValidator {
	return semanticValidator{
		policy:         newSemanticEvaluator(surface).policy,
		referenceError: func(err error) error { return err },
	}
}

func (v *semanticValidator) validate(node actionlint.ExprNode) error {
	switch node := node.(type) {
	case *actionlint.NullNode, *actionlint.BoolNode, *actionlint.IntNode, *actionlint.FloatNode, *actionlint.StringNode:
		if v.policy.allowLiterals {
			return nil
		}
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode:
		root, path, err := referencePath(node)
		if err == nil {
			if _, whole := node.(*actionlint.VariableNode); whole && v.validateAccess != nil {
				return v.validateAccess(node)
			}
			return v.validateReference(node, root, path)
		}
		if v.validateAccess != nil {
			return v.validateAccess(node)
		}
		return v.referenceError(err)
	case *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		staticAuthority := strings.EqualFold(root, "github") || strings.EqualFold(root, "secrets")
		if err == nil && (v.validateAccess == nil || staticAuthority || strings.EqualFold(root, "job") && len(path) == 4 && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports")) {
			return v.validateReference(node, root, path)
		}
		if v.validateAccess != nil {
			return v.validateAccess(node)
		}
		return v.referenceError(err)
	case *actionlint.ArrayDerefNode:
		if v.validateAccess != nil {
			return v.validateAccess(node)
		}
		return v.unsupported(node)
	case *actionlint.NotOpNode:
		if v.policy.allowNot {
			return v.validate(node.Operand)
		}
	case *actionlint.LogicalOpNode:
		if v.policy.allowLogical {
			if err := v.validate(node.Left); err != nil {
				return err
			}
			return v.validate(node.Right)
		}
	case *actionlint.CompareOpNode:
		if v.policy.allowCompare {
			if err := v.validateCompare(node.Kind); err != nil {
				return err
			}
			if err := v.validate(node.Left); err != nil {
				return err
			}
			if err := v.validate(node.Right); err != nil {
				return err
			}
			return v.afterCompare(node)
		}
	case *actionlint.FuncCallNode:
		if v.policy.allowFunction {
			return v.validateCall(v, node)
		}
	}
	return v.unsupported(node)
}

func (e *semanticEvaluator) evaluate(node actionlint.ExprNode) (any, error) {
	switch node := node.(type) {
	case *actionlint.NullNode:
		if e.policy.allowLiterals {
			return nil, nil
		}
	case *actionlint.BoolNode:
		if e.policy.allowLiterals {
			return node.Value, nil
		}
	case *actionlint.IntNode:
		if e.policy.allowLiterals {
			return node.Value, nil
		}
	case *actionlint.FloatNode:
		if e.policy.allowLiterals {
			return node.Value, nil
		}
	case *actionlint.StringNode:
		if e.policy.allowLiterals {
			return node.Value, nil
		}
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode:
		root, path, err := referencePath(node)
		if err == nil {
			if _, object := node.(*actionlint.ObjectDerefNode); object && e.resolveRoot != nil && expressionReferenceUsesIndex(node) {
				return e.evaluateAccess(node)
			}
			if _, whole := node.(*actionlint.VariableNode); whole && e.resolveRoot != nil {
				return e.resolveRoot(root)
			}
			return e.resolve(root, path)
		}
		if e.resolveRoot != nil {
			return e.evaluateAccess(node)
		}
		return nil, err
	case *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err == nil && strings.EqualFold(root, "job") && len(path) == 4 && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports") {
			return e.resolve(root, path)
		}
		if e.resolveRoot != nil {
			return e.evaluateAccess(node)
		}
		if err != nil {
			return nil, err
		}
		return e.resolve(root, path)
	case *actionlint.ArrayDerefNode:
		if e.resolveRoot != nil {
			return e.evaluateAccess(node)
		}
		return nil, e.unsupported(node)
	case *actionlint.NotOpNode:
		if e.policy.allowNot {
			value, err := e.evaluate(node.Operand)
			if err != nil {
				return nil, err
			}
			return !e.truthy(value), nil
		}
	case *actionlint.LogicalOpNode:
		if e.policy.allowLogical {
			left, err := e.evaluate(node.Left)
			if err != nil {
				return nil, err
			}
			leftTruthy := e.truthy(left)
			switch node.Kind {
			case actionlint.LogicalOpNodeKindAnd:
				if !leftTruthy {
					if e.policy.logicalBool {
						return false, nil
					}
					return left, nil
				}
			case actionlint.LogicalOpNodeKindOr:
				if leftTruthy {
					if e.policy.logicalBool {
						return true, nil
					}
					return left, nil
				}
			default:
				return nil, e.logicalError(node.Kind)
			}
			right, err := e.evaluate(node.Right)
			if err != nil {
				return nil, err
			}
			if e.policy.logicalBool {
				return e.truthy(right), nil
			}
			return right, nil
		}
	case *actionlint.CompareOpNode:
		if e.policy.allowCompare {
			if e.validateCompare != nil {
				if err := e.validateCompare(node.Kind); err != nil {
					return nil, err
				}
			}
			left, err := e.evaluate(node.Left)
			if err != nil {
				return nil, err
			}
			right, err := e.evaluate(node.Right)
			if err != nil {
				return nil, err
			}
			return e.compare(node.Kind, left, right)
		}
	case *actionlint.FuncCallNode:
		if e.policy.allowFunction {
			return e.call(e, node)
		}
	}
	return nil, e.unsupported(node)
}

func expressionReferenceUsesIndex(node actionlint.ExprNode) bool {
	switch node := node.(type) {
	case *actionlint.ObjectDerefNode:
		return expressionReferenceUsesIndex(node.Receiver)
	case *actionlint.IndexAccessNode, *actionlint.ArrayDerefNode:
		return true
	default:
		return false
	}
}

type expressionProjection []any

func (e *semanticEvaluator) evaluateAccess(node actionlint.ExprNode) (any, error) {
	switch node := node.(type) {
	case *actionlint.VariableNode:
		return e.resolveRoot(node.Name)
	case *actionlint.ObjectDerefNode:
		receiver, err := e.evaluateAccess(node.Receiver)
		if err != nil {
			return nil, err
		}
		if projection, ok := receiver.(expressionProjection); ok {
			result := make(expressionProjection, 0, len(projection))
			for _, item := range projection {
				value, found, err := objectValue(item, node.Property)
				if err != nil {
					return nil, err
				}
				if found {
					result = append(result, value)
				}
			}
			return result, nil
		}
		value, found, err := objectValue(receiver, node.Property)
		if err != nil || found {
			return value, err
		}
		return nil, nil
	case *actionlint.IndexAccessNode:
		operand, err := e.evaluateAccess(node.Operand)
		if err != nil {
			return nil, err
		}
		index, err := e.evaluate(node.Index)
		if err != nil {
			return nil, err
		}
		return expressionIndex(operand, index)
	case *actionlint.ArrayDerefNode:
		receiver, err := e.evaluateAccess(node.Receiver)
		if err != nil {
			return nil, err
		}
		if projection, ok := receiver.(expressionProjection); ok {
			result := expressionProjection{}
			for _, item := range projection {
				if values, ok := expressionChildren(item); ok {
					result = append(result, values...)
				}
			}
			return result, nil
		}
		values, ok := expressionChildren(receiver)
		if !ok {
			return expressionProjection{}, nil
		}
		return expressionProjection(values), nil
	default:
		return e.evaluate(node)
	}
}

func expressionChildren(value any) ([]any, bool) {
	if values, ok := expressionCollection(value); ok {
		return values, true
	}
	switch value := value.(type) {
	case map[string]any:
		values := make([]any, 0, len(value))
		for _, item := range value {
			values = append(values, item)
		}
		return values, true
	case map[string]string:
		values := make([]any, 0, len(value))
		for _, item := range value {
			values = append(values, item)
		}
		return values, true
	default:
		return nil, false
	}
}

func expressionIndex(value, index any) (any, error) {
	if projection, ok := value.(expressionProjection); ok {
		result := expressionProjection{}
		for _, child := range projection {
			item, found, err := expressionIndexValue(child, index)
			if err != nil {
				return nil, err
			}
			if found {
				result = append(result, item)
			}
		}
		return result, nil
	}
	item, _, err := expressionIndexValue(value, index)
	return item, err
}

func expressionIndexValue(value, index any) (any, bool, error) {
	if items, ok := expressionCollection(value); ok {
		number, numeric := githubNumber(index)
		if !numeric || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > math.MaxInt32 {
			return nil, false, nil
		}
		position := int(math.Floor(number))
		if position < 0 || position >= len(items) {
			return nil, false, nil
		}
		return items[position], true, nil
	}
	key, ok := expressionString(index)
	if !ok {
		return nil, false, nil
	}
	item, found, err := objectValue(value, key)
	return item, found, err
}

func unsupportedReference(node actionlint.ExprNode) error {
	_, _, err := referencePath(node)
	if err == nil {
		return fmt.Errorf("unsupported expression reference")
	}
	return err
}
