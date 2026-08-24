package expression

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/rhysd/actionlint"
)

// expressionEvaluator owns expression-tree traversal while each fixed surface
// and value domain supplies its reference, comparison, function, and error
// behavior. The concrete semanticEvaluator alias preserves the existing
// runtime evaluators; abstract evaluation uses the same traversal with a value
// that can represent unknown runtime data and authority effects.
type expressionEvaluator[T any] struct {
	policy          evaluationPolicy
	resolve         func(string, []string) (T, error)
	resolveRoot     func(string) (T, error)
	domain          expressionDomain[T]
	truthy          func(any) bool
	validateCompare func(actionlint.CompareOpNodeKind) error
	compare         func(actionlint.CompareOpNodeKind, any, any) (any, error)
	call            func(*expressionEvaluator[T], *actionlint.FuncCallNode) (T, error)
	unsupported     func(actionlint.ExprNode) error
	logicalError    func(actionlint.LogicalOpNodeKind) error
}

type semanticEvaluator = expressionEvaluator[any]

type expressionDomain[T any] interface {
	known(any) T
	derive(any, ...T) T
	value(T) (any, bool)
	truthiness(T, func(any) bool) (bool, bool)
	unknown(...T) T
	unknownWithTruthiness(bool, ...T) T
	join(func(any) bool, ...T) T
}

type concreteExpressionDomain struct{}

func (concreteExpressionDomain) known(value any) any            { return value }
func (concreteExpressionDomain) derive(value any, _ ...any) any { return value }
func (concreteExpressionDomain) value(value any) (any, bool)    { return value, true }
func (concreteExpressionDomain) truthiness(value any, truthy func(any) bool) (bool, bool) {
	return truthy(value), true
}
func (concreteExpressionDomain) unknown(...any) any {
	panic("concrete expression evaluation produced an unknown value")
}
func (concreteExpressionDomain) unknownWithTruthiness(bool, ...any) any {
	panic("concrete expression evaluation produced an unknown value")
}
func (concreteExpressionDomain) join(func(any) bool, ...any) any {
	panic("concrete expression evaluation joined multiple paths")
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
	allowLiterals                 bool
	allowNot                      bool
	allowLogical                  bool
	allowCompare                  bool
	allowFunction                 bool
	logicalBool                   bool
	resolveStaticIndexedReference bool
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
		policy = evaluationPolicy{allowLiterals: true, allowNot: true, allowLogical: true, allowCompare: true, allowFunction: true, resolveStaticIndexedReference: true}
	case runtimeReferenceSurface:
	}
	return semanticEvaluator{
		policy: policy,
		domain: concreteExpressionDomain{},
	}
}

func (e *expressionEvaluator[T]) result(value T, inputs ...T) T {
	concrete, known := e.domain.value(value)
	inputs = append(inputs, value)
	if !known {
		if truthy, truthKnown := e.domain.truthiness(value, e.truthy); truthKnown {
			return e.domain.unknownWithTruthiness(truthy, inputs...)
		}
		return e.domain.unknown(inputs...)
	}
	return e.domain.derive(concrete, inputs...)
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

func (e *expressionEvaluator[T]) evaluate(node actionlint.ExprNode) (T, error) {
	var zero T
	switch node := node.(type) {
	case *actionlint.NullNode:
		if e.policy.allowLiterals {
			return e.domain.known(nil), nil
		}
	case *actionlint.BoolNode:
		if e.policy.allowLiterals {
			return e.domain.known(node.Value), nil
		}
	case *actionlint.IntNode:
		if e.policy.allowLiterals {
			return e.domain.known(node.Value), nil
		}
	case *actionlint.FloatNode:
		if e.policy.allowLiterals {
			return e.domain.known(node.Value), nil
		}
	case *actionlint.StringNode:
		if e.policy.allowLiterals {
			return e.domain.known(node.Value), nil
		}
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode:
		root, path, err := referencePath(node)
		if err == nil {
			if _, object := node.(*actionlint.ObjectDerefNode); object && e.resolveRoot != nil && expressionReferenceUsesIndex(node) && !e.policy.resolveStaticIndexedReference {
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
		return zero, err
	case *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		// Mirror the validator: static bracket references to authority
		// contexts resolve as ordinary references so evaluation never
		// exposes the whole context root.
		staticAuthority := strings.EqualFold(root, "github") || strings.EqualFold(root, "secrets")
		if err == nil && (staticAuthority || strings.EqualFold(root, "job") && len(path) == 4 && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports")) {
			return e.resolve(root, path)
		}
		if e.resolveRoot != nil {
			return e.evaluateAccess(node)
		}
		if err != nil {
			return zero, err
		}
		return e.resolve(root, path)
	case *actionlint.ArrayDerefNode:
		if e.resolveRoot != nil {
			return e.evaluateAccess(node)
		}
		return zero, e.unsupported(node)
	case *actionlint.NotOpNode:
		if e.policy.allowNot {
			value, err := e.evaluate(node.Operand)
			if err != nil {
				return zero, err
			}
			truthy, known := e.domain.truthiness(value, e.truthy)
			if !known {
				return e.domain.unknown(value), nil
			}
			return e.domain.derive(!truthy, value), nil
		}
	case *actionlint.LogicalOpNode:
		if e.policy.allowLogical {
			left, err := e.evaluate(node.Left)
			if err != nil {
				return zero, err
			}
			leftTruthy, leftKnown := e.domain.truthiness(left, e.truthy)
			if !leftKnown {
				right, err := e.evaluate(node.Right)
				if err != nil {
					return zero, err
				}
				rightTruthy, rightKnown := e.domain.truthiness(right, e.truthy)
				if rightKnown {
					switch node.Kind {
					case actionlint.LogicalOpNodeKindAnd:
						if !rightTruthy {
							return e.domain.unknownWithTruthiness(false, left, right), nil
						}
					case actionlint.LogicalOpNodeKindOr:
						if rightTruthy {
							return e.domain.unknownWithTruthiness(true, left, right), nil
						}
					default:
						return zero, e.logicalError(node.Kind)
					}
				}
				return e.domain.unknown(left, right), nil
			}
			switch node.Kind {
			case actionlint.LogicalOpNodeKindAnd:
				if !leftTruthy {
					if e.policy.logicalBool {
						return e.domain.derive(false, left), nil
					}
					return left, nil
				}
			case actionlint.LogicalOpNodeKindOr:
				if leftTruthy {
					if e.policy.logicalBool {
						return e.domain.derive(true, left), nil
					}
					return left, nil
				}
			default:
				return zero, e.logicalError(node.Kind)
			}
			right, err := e.evaluate(node.Right)
			if err != nil {
				return zero, err
			}
			if e.policy.logicalBool {
				rightTruthy, rightKnown := e.domain.truthiness(right, e.truthy)
				if !rightKnown {
					return e.domain.unknown(left, right), nil
				}
				return e.domain.derive(rightTruthy, left, right), nil
			}
			return e.result(right, left), nil
		}
	case *actionlint.CompareOpNode:
		if e.policy.allowCompare {
			if e.validateCompare != nil {
				if err := e.validateCompare(node.Kind); err != nil {
					return zero, err
				}
			}
			left, err := e.evaluate(node.Left)
			if err != nil {
				return zero, err
			}
			right, err := e.evaluate(node.Right)
			if err != nil {
				return zero, err
			}
			leftValue, leftKnown := e.domain.value(left)
			rightValue, rightKnown := e.domain.value(right)
			if !leftKnown || !rightKnown {
				return e.domain.unknown(left, right), nil
			}
			compared, err := e.compare(node.Kind, leftValue, rightValue)
			if err != nil {
				return zero, err
			}
			return e.domain.derive(compared, left, right), nil
		}
	case *actionlint.FuncCallNode:
		if e.policy.allowFunction {
			return e.call(e, node)
		}
	}
	return zero, e.unsupported(node)
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

func newExpressionProjection(capacity int) expressionProjection {
	if capacity == 0 {
		capacity = 1
	}
	return make(expressionProjection, 0, capacity)
}

func (e *expressionEvaluator[T]) evaluateAccess(node actionlint.ExprNode) (T, error) {
	var zero T
	switch node := node.(type) {
	case *actionlint.VariableNode:
		return e.resolveRoot(node.Name)
	case *actionlint.ObjectDerefNode:
		receiver, err := e.evaluateAccess(node.Receiver)
		if err != nil {
			return zero, err
		}
		receiverValue, known := e.domain.value(receiver)
		if !known {
			return e.domain.unknown(receiver), nil
		}
		if projection, ok := receiverValue.(expressionProjection); ok {
			result := newExpressionProjection(len(projection))
			for _, item := range projection {
				value, found, err := objectValue(item, node.Property)
				if err != nil {
					return zero, err
				}
				if found {
					result = append(result, value)
				}
			}
			return e.domain.derive(result, receiver), nil
		}
		value, found, err := objectValue(receiverValue, node.Property)
		if err != nil || found {
			return e.domain.derive(value, receiver), err
		}
		return e.domain.derive(nil, receiver), nil
	case *actionlint.IndexAccessNode:
		operand, err := e.evaluateAccess(node.Operand)
		if err != nil {
			return zero, err
		}
		index, err := e.evaluate(node.Index)
		if err != nil {
			return zero, err
		}
		operandValue, operandKnown := e.domain.value(operand)
		indexValue, indexKnown := e.domain.value(index)
		if !operandKnown || !indexKnown {
			return e.domain.unknown(operand, index), nil
		}
		value, err := expressionIndex(operandValue, indexValue)
		if err != nil {
			return zero, err
		}
		return e.domain.derive(value, operand, index), nil
	case *actionlint.ArrayDerefNode:
		receiver, err := e.evaluateAccess(node.Receiver)
		if err != nil {
			return zero, err
		}
		receiverValue, known := e.domain.value(receiver)
		if !known {
			return e.domain.unknown(receiver), nil
		}
		if projection, ok := receiverValue.(expressionProjection); ok {
			result := newExpressionProjection(0)
			for _, item := range projection {
				if values, ok := expressionChildren(item); ok {
					result = append(result, values...)
				}
			}
			return e.domain.derive(result, receiver), nil
		}
		values, ok := expressionChildren(receiverValue)
		if !ok {
			return e.domain.derive(newExpressionProjection(0), receiver), nil
		}
		return e.domain.derive(append(newExpressionProjection(len(values)), values...), receiver), nil
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
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			values = append(values, value[key])
		}
		return values, true
	case map[string]string:
		values := make([]any, 0, len(value))
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			values = append(values, value[key])
		}
		return values, true
	default:
		return nil, false
	}
}

func expressionIndex(value, index any) (any, error) {
	if projection, ok := value.(expressionProjection); ok {
		result := newExpressionProjection(0)
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
