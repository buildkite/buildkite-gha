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
	GitHubToken  GitHubTokenEffect
	EventPayload bool
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
		effects.EventPayload = effects.EventPayload || value.Effects.EventPayload
	}
	return Analysis{Effects: effects}
}

func derivedAnalysis(value any, inputs ...Analysis) Analysis {
	analysis := knownAnalysis(value)
	for _, input := range inputs {
		analysis.Effects.GitHubToken |= input.Effects.GitHubToken
		analysis.Effects.EventPayload = analysis.Effects.EventPayload || input.Effects.EventPayload
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
	evaluator.policy.resolveStaticIndexedReference = true
	evaluator.resolve = func(root string, path []string) (Analysis, error) {
		analysis := Analysis{Effects: abstractReferenceEffects(root, path)}
		if strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "token") {
			analysis.Effects.GitHubToken = GitHubTokenDirect
			if value, ok := knownReferences["github.token"]; ok {
				analysis.Value = AbstractValue{Known: true, Value: value}
			}
			return analysis, nil
		}
		if value, ok := knownReferences[strings.ToLower(referenceName(root, path))]; ok {
			analysis.Value = AbstractValue{Known: true, Value: value}
		}
		return analysis, nil
	}
	// Dynamic access to a runtime root is valid but unavailable to planning.
	// Static references still resolve through resolve above. Root effects remain
	// visible so dynamic github.event access retains payload authority.
	evaluator.resolveRoot = func(root string) (Analysis, error) {
		analysis := Analysis{Effects: abstractReferenceEffects(root, nil)}
		if value, ok := knownReferences[strings.ToLower(root)]; ok {
			analysis.Value = AbstractValue{Known: true, Value: value}
		}
		return analysis, nil
	}
	evaluator.call = func(evaluator *expressionEvaluator[Analysis], node *actionlint.FuncCallNode) (Analysis, error) {
		if strings.EqualFold(node.Callee, "toJSON") && len(node.Args) == 1 && strings.EqualFold(referenceRoot(node.Args[0]), "matrix") {
			return Analysis{}, nil
		}
		if value, recognized, err := evaluatePureFunction(evaluator, node); recognized {
			return value, err
		}
		return Analysis{}, fmt.Errorf("action input default function %q is unsupported", node.Callee)
	}
	return evaluator.evaluate(node)
}

// analyzeRuntimeNode uses the same evaluator traversal as concrete step
// evaluation while resolving only planning-known references. Unknown values
// retain effects from every reachable branch; known lazy guards exclude
// effects from branches concrete evaluation cannot select.
func analyzeRuntimeNode(node actionlint.ExprNode, knownReferences map[string]any, purpose Purpose) (Analysis, error) {
	evaluator := newAbstractEvaluator(stepRuntimeSurface)
	evaluator.policy.resolveStaticIndexedReference = true
	evaluator.resolve = func(root string, path []string) (Analysis, error) {
		name := strings.ToLower(referenceName(root, path))
		analysis := Analysis{Effects: abstractReferenceEffects(root, path)}
		if strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "token") {
			analysis.Effects.GitHubToken = GitHubTokenDirect
		}
		if value, ok := knownReferences[name]; ok {
			analysis.Value = AbstractValue{Known: true, Value: value}
		}
		return analysis, nil
	}
	evaluator.resolveRoot = func(root string) (Analysis, error) {
		analysis := Analysis{Effects: abstractReferenceEffects(root, nil)}
		if strings.EqualFold(root, "github") {
			if purpose == PurposeCompositeActionInput {
				analysis.Effects.GitHubToken = GitHubTokenCompositeContext
			} else {
				analysis.Effects.GitHubToken = GitHubTokenWorkflowContext
			}
		}
		if value, ok := knownReferences[strings.ToLower(root)]; ok {
			analysis.Value = AbstractValue{Known: true, Value: value}
		}
		return analysis, nil
	}
	evaluator.call = func(evaluator *expressionEvaluator[Analysis], node *actionlint.FuncCallNode) (Analysis, error) {
		if value, recognized, err := evaluatePureFunction(evaluator, node); recognized {
			return value, err
		}
		if strings.EqualFold(node.Callee, "hashFiles") {
			return Analysis{}, nil
		}
		return Analysis{}, fmt.Errorf("runtime function %q is unsupported", node.Callee)
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported runtime expression") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("unsupported runtime logical operator %s", kind)
	}
	return evaluator.evaluate(node)
}

// analyzeTemplate renders a known template value and combines effects through
// the expression domain. It deliberately parses in evaluation order so a
// later ordered action default can consume a fully known earlier default.
func analyzeTemplate(template string, analyze func(actionlint.ExprNode) (Analysis, error)) (Analysis, error) {
	const open = "${{"
	remaining := template
	var rendered strings.Builder
	result := knownAnalysis("")
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			rendered.WriteString(remaining)
			if result.Value.Known {
				result.Value.Value = rendered.String()
			}
			return result, nil
		}
		rendered.WriteString(remaining[:start])
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			if !strings.Contains(source, "}}") {
				return Analysis{}, fmt.Errorf("unterminated expression in %q", template)
			}
			return Analysis{}, fmt.Errorf("invalid expression: %w", lexErr)
		}
		node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(source[:consumed]))
		if parseErr != nil {
			return Analysis{}, fmt.Errorf("invalid expression: %w", parseErr)
		}
		value, err := analyze(node)
		if err != nil {
			return Analysis{}, err
		}
		result.Effects.GitHubToken |= value.Effects.GitHubToken
		result.Effects.EventPayload = result.Effects.EventPayload || value.Effects.EventPayload
		if result.Value.Known && value.Value.Known {
			switch concrete := value.Value.Value.(type) {
			case nil:
			case string:
				rendered.WriteString(concrete)
			default:
				text, ok := expressionString(concrete)
				if !ok {
					return Analysis{}, fmt.Errorf("template expression resolved to %T, want a scalar", concrete)
				}
				rendered.WriteString(text)
			}
		} else {
			result.Value = AbstractValue{}
		}
		remaining = source[consumed:]
	}
}

func analyzeConditionNode(node actionlint.ExprNode, known map[string]any) (Analysis, error) {
	evaluator := newAbstractEvaluator(conditionSurface)
	evaluator.resolve = func(root string, path []string) (Analysis, error) {
		analysis := Analysis{Effects: abstractReferenceEffects(root, path)}
		if strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "token") {
			analysis.Effects.GitHubToken = GitHubTokenDirect
		}
		name := strings.ToLower(root + "." + strings.Join(path, "."))
		if value, ok := known[name]; ok {
			analysis.Value = AbstractValue{Known: true, Value: value}
			return analysis, nil
		}
		if value, ok := known[strings.ToLower(root)]; ok {
			if resolved, found := lookupRuntimeValue(value, path); found {
				analysis.Value = AbstractValue{Known: true, Value: resolved}
			}
		}
		return analysis, nil
	}
	evaluator.resolveRoot = func(root string) (Analysis, error) {
		analysis := Analysis{Effects: abstractReferenceEffects(root, nil)}
		if value, ok := known[strings.ToLower(root)]; ok {
			analysis.Value = AbstractValue{Known: true, Value: value}
		}
		return analysis, nil
	}
	evaluator.call = func(evaluator *expressionEvaluator[Analysis], node *actionlint.FuncCallNode) (Analysis, error) {
		if value, recognized, err := evaluatePureFunction(evaluator, node); recognized {
			return value, err
		}
		switch strings.ToLower(node.Callee) {
		case "always":
			return knownAnalysis(true), nil
		case "success":
			if value, ok := known["success"]; ok {
				return knownAnalysis(value), nil
			}
			return Analysis{}, nil
		case "failure":
			if value, ok := known["failure"]; ok {
				return knownAnalysis(value), nil
			}
			return Analysis{}, nil
		case "cancelled":
			if value, ok := known["cancelled"]; ok {
				return knownAnalysis(value), nil
			}
			return Analysis{}, nil
		case "hashfiles":
			return Analysis{}, nil
		default:
			return Analysis{}, fmt.Errorf("condition function %q is unsupported", node.Callee)
		}
	}
	return evaluator.evaluate(node)
}

func abstractReferenceEffects(root string, path []string) Effects {
	return Effects{EventPayload: strings.EqualFold(root, "event") ||
		strings.EqualFold(root, "github") && (len(path) == 0 || strings.EqualFold(path[0], "event"))}
}
