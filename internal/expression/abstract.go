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

// AnalyzeActionInputDefault evaluates an action input default with retained
// planning values and records authority reachable through unknown branches.
func AnalyzeActionInputDefault(template string, knownReferences map[string]any) (Analysis, error) {
	if err := ValidateActionInputDefault(template); err != nil {
		return Analysis{}, err
	}
	return analyzeRuntimeTemplate(template, func(node actionlint.ExprNode) (Analysis, error) {
		return analyzeActionInputDefault(node, normalizeKnownReferences(knownReferences))
	})
}

// AnalyzeStepTemplate evaluates a workflow or composite step template with
// retained planning values. wholeGitHub records the provenance of toJSON(github).
func AnalyzeStepTemplate(template string, knownReferences map[string]any, wholeGitHub GitHubTokenEffect) (Analysis, error) {
	if err := ValidateStepTemplate(template); err != nil {
		return Analysis{}, err
	}
	knownReferences = normalizeKnownReferences(knownReferences)
	return analyzeRuntimeTemplate(template, func(node actionlint.ExprNode) (Analysis, error) {
		return analyzeRuntimeNode(node, stepRuntimeSurface, knownReferences, wholeGitHub)
	})
}

// ValidateStepTemplate verifies every branch of a workflow or composite step
// template without resolving runtime-dependent values.
func ValidateStepTemplate(template string) error {
	return visitTemplateExpressions(template, func(node actionlint.ExprNode) error {
		return validateStepRuntimeExpression(node, true, true, nil)
	})
}

// AnalyzeCondition evaluates a validated condition with retained planning
// values. Runtime-dependent values remain unknown.
func AnalyzeCondition(source string, knownReferences map[string]any, wholeGitHub GitHubTokenEffect) (Analysis, error) {
	if err := ValidateCondition(source, StepCondition); err != nil {
		return Analysis{}, err
	}
	return analyzeCondition(source, knownReferences, wholeGitHub)
}

// AnalyzeActionLifecycleCondition evaluates pre-if or post-if with retained
// planning values while preserving unknown runtime state.
func AnalyzeActionLifecycleCondition(source string, knownReferences map[string]any) (Analysis, error) {
	if err := ValidateActionLifecycleCondition(source); err != nil {
		return Analysis{}, err
	}
	return analyzeCondition(source, knownReferences, GitHubTokenCompositeContext)
}

func analyzeCondition(source string, knownReferences map[string]any, wholeGitHub GitHubTokenEffect) (Analysis, error) {
	node, empty, err := parseCondition(source)
	if err != nil {
		return Analysis{}, err
	}
	if empty {
		return knownAnalysis(true), nil
	}
	analysis, err := analyzeRuntimeNode(node, conditionSurface, normalizeKnownReferences(knownReferences), wholeGitHub)
	if err != nil || !analysis.Value.Known {
		return analysis, err
	}
	return derivedAnalysis(githubTruthy(analysis.Value.Value), analysis), nil
}

func analyzeRuntimeNode(node actionlint.ExprNode, surface evaluationSurface, knownReferences map[string]any, wholeGitHub GitHubTokenEffect) (Analysis, error) {
	evaluator := newAbstractEvaluator(surface)
	evaluator.resolve = func(root string, path []string) (Analysis, error) {
		name := strings.ToLower(referenceName(root, path))
		if name == "github.token" {
			return Analysis{Effects: Effects{GitHubToken: GitHubTokenDirect}}, nil
		}
		if value, ok := knownReferences[name]; ok {
			return knownAnalysis(value), nil
		}
		return Analysis{}, nil
	}
	evaluator.resolveRoot = func(root string) (Analysis, error) {
		name := strings.ToLower(root)
		if value, ok := knownReferences[name]; ok {
			return knownAnalysis(value), nil
		}
		if name == "github" {
			return Analysis{Effects: Effects{GitHubToken: wholeGitHub}}, nil
		}
		return Analysis{}, nil
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported runtime expression") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("unsupported runtime logical operator %s", kind)
	}
	evaluator.call = func(evaluator *expressionEvaluator[Analysis], call *actionlint.FuncCallNode) (Analysis, error) {
		if value, recognized, err := evaluatePureFunction(evaluator, call); recognized {
			return value, err
		}
		switch strings.ToLower(call.Callee) {
		case "hashfiles", "success", "failure", "cancelled", "always":
			return Analysis{}, nil
		default:
			return Analysis{}, fmt.Errorf("unsupported runtime function %q", call.Callee)
		}
	}
	return evaluator.evaluate(node)
}

func analyzeRuntimeTemplate(template string, analyze func(actionlint.ExprNode) (Analysis, error)) (Analysis, error) {
	const open = "${{"
	remaining := template
	var result strings.Builder
	analysis := knownAnalysis("")
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			if analysis.Value.Known {
				result.WriteString(remaining)
				analysis.Value.Value = result.String()
			}
			return analysis, nil
		}
		if analysis.Value.Known {
			result.WriteString(remaining[:start])
		}
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
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
		analysis.Effects.GitHubToken |= value.Effects.GitHubToken
		if analysis.Value.Known && value.Value.Known {
			if value.Value.Value != nil {
				text, ok := expressionString(value.Value.Value)
				if !ok {
					return Analysis{}, fmt.Errorf("template expression resolved to %T, want a scalar", value.Value.Value)
				}
				result.WriteString(text)
			}
		} else {
			analysis.Value = AbstractValue{}
		}
		remaining = source[consumed:]
	}
}

func normalizeKnownReferences(references map[string]any) map[string]any {
	known := make(map[string]any, len(references))
	for name, value := range references {
		known[strings.ToLower(name)] = value
	}
	return known
}
