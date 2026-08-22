package expression

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/rhysd/actionlint"
)

// AbstractValue is either one concrete expression value or an unknown runtime
// value. Unknown is distinct from a known null value.
type AbstractValue struct {
	Known bool
	Value any
}

// GitHubTokenEffect records why evaluation can require github.token.
type GitHubTokenEffect uint8

const (
	GitHubTokenDirect GitHubTokenEffect = 1 << iota
	GitHubTokenWorkflowContext
	GitHubTokenCompositeContext
)

// Effects are the authority effects reachable while evaluating an expression.
type Effects struct {
	GitHubToken GitHubTokenEffect
}

// Analysis contains an abstract expression result and its reachable authority
// effects.
type Analysis struct {
	Value   AbstractValue
	Effects Effects
}

func knownAnalysis(value any) Analysis {
	return Analysis{Value: AbstractValue{Known: true, Value: value}}
}

func unknownAnalysis(values ...Analysis) Analysis {
	var effects Effects
	for _, value := range values {
		effects.GitHubToken |= value.Effects.GitHubToken
	}
	return Analysis{Effects: effects}
}

func derivedAnalysis(value any, inputs ...Analysis) Analysis {
	analysis := knownAnalysis(value)
	for _, input := range inputs {
		analysis.Effects.GitHubToken |= input.Effects.GitHubToken
	}
	return analysis
}

type abstractExpressionDomain struct{}

func (abstractExpressionDomain) known(value any) Analysis { return knownAnalysis(value) }
func (abstractExpressionDomain) derive(value any, inputs ...Analysis) Analysis {
	return derivedAnalysis(value, inputs...)
}
func (abstractExpressionDomain) value(analysis Analysis) (any, bool) {
	return analysis.Value.Value, analysis.Value.Known
}
func (abstractExpressionDomain) unknown(values ...Analysis) Analysis {
	return unknownAnalysis(values...)
}
func (abstractExpressionDomain) join(values ...Analysis) Analysis {
	if len(values) == 0 {
		return Analysis{}
	}
	result := unknownAnalysis(values...)
	first := values[0].Value
	if !first.Known {
		return result
	}
	for _, value := range values[1:] {
		if !value.Value.Known || !abstractValuesIdentical(value.Value.Value, first.Value) {
			return result
		}
	}
	result.Value = first
	return result
}

func abstractValuesIdentical(left, right any) bool {
	if left == nil || right == nil || reflect.TypeOf(left) != reflect.TypeOf(right) {
		return left == nil && right == nil
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	switch leftValue.Kind() {
	case reflect.Map:
		return leftValue.UnsafePointer() == rightValue.UnsafePointer()
	case reflect.Pointer, reflect.Slice:
		return leftValue.Pointer() == rightValue.Pointer()
	default:
		return reflect.DeepEqual(left, right)
	}
}

func newAbstractEvaluator(surface evaluationSurface) expressionEvaluator[Analysis] {
	return expressionEvaluator[Analysis]{
		policy: newSemanticEvaluator(surface).policy,
		domain: abstractExpressionDomain{},
		truthy: githubTruthy,
		compare: func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
			return githubCompare(kind, left, right)
		},
		unsupported: func(actionlint.ExprNode) error {
			return fmt.Errorf("action input default expression is unsupported")
		},
		logicalError: func(kind actionlint.LogicalOpNodeKind) error {
			return fmt.Errorf("action input default logical operator %s is unsupported", kind)
		},
	}
}

func analyzeActionInputDefault(node actionlint.ExprNode, knownReferences map[string]any) (Analysis, error) {
	evaluator := newAbstractEvaluator(actionInputDefaultSurface)
	evaluator.resolve = func(root string, path []string) (Analysis, error) {
		if strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "token") {
			analysis := Analysis{Effects: Effects{GitHubToken: GitHubTokenDirect}}
			if value, ok := knownReferences["github.token"]; ok {
				analysis.Value = AbstractValue{Known: true, Value: value}
			}
			return analysis, nil
		}
		if value, ok := knownReferences[strings.ToLower(referenceName(root, path))]; ok {
			return knownAnalysis(value), nil
		}
		return Analysis{}, nil
	}
	evaluator.call = func(evaluator *expressionEvaluator[Analysis], node *actionlint.FuncCallNode) (Analysis, error) {
		if value, recognized, err := evaluatePureFunction(evaluator, node); recognized {
			return value, err
		}
		return Analysis{}, fmt.Errorf("action input default function %q is unsupported", node.Callee)
	}
	return evaluator.evaluate(node)
}
