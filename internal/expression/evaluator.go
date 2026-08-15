package expression

import (
	"fmt"

	"github.com/rhysd/actionlint"
)

// semanticEvaluator owns expression-tree traversal while each fixed surface
// supplies its existing reference, comparison, function, and error behavior.
// Validation and authority analysis remain separate and run at their existing
// call sites.
type semanticEvaluator struct {
	policy          evaluationPolicy
	resolve         func(string, []string) (any, error)
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
		policy = evaluationPolicy{allowLiterals: true, allowNot: true, allowLogical: true, allowCompare: true, allowFunction: true, logicalBool: true}
	case actionInputDefaultSurface:
		policy = evaluationPolicy{allowLiterals: true, allowLogical: true, allowCompare: true, allowFunction: true}
	case runtimeReferenceSurface:
	}
	return semanticEvaluator{policy: policy}
}

type semanticValidator struct {
	policy            evaluationPolicy
	validateReference func(actionlint.ExprNode, string, []string) error
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
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return v.referenceError(err)
		}
		return v.validateReference(node, root, path)
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
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return nil, err
		}
		return e.resolve(root, path)
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

func unsupportedReference(node actionlint.ExprNode) error {
	_, _, err := referencePath(node)
	if err == nil {
		return fmt.Errorf("unsupported expression reference")
	}
	return err
}
