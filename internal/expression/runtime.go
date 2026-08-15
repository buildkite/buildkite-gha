// Direct runtime template evaluation and runtime reference resolution.
package expression

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rhysd/actionlint"
)

// Context contains the compile-time values available while evaluating a template.
type Context struct {
	Inputs           map[string]string
	Matrix           map[string]any
	Steps            map[string]map[string]string
	StepStatuses     map[string]StepStatus
	Needs            map[string]map[string]string
	NeedResults      map[string]string
	Secrets          map[string]string
	Vars             map[string]string
	Env              map[string]string
	GitHub           map[string]any
	Runner           map[string]string
	Services         map[string]ServiceContext
	JobStatus        string
	HashFiles        func([]string) (string, error)
	HashFilesContext func(context.Context, []string) (string, error)
}

type ServiceContext struct {
	ID      string
	Network string
	Ports   map[string]string
}

func ValidateRuntimeTemplate(template string) error {
	return validateRuntimeTemplate(template)
}

type runtimeReferenceKind uint8

const (
	runtimeReferenceUnsupported runtimeReferenceKind = iota
	runtimeReferenceServicePort
	runtimeReferenceServiceValue
	runtimeReferenceGitHub
	runtimeReferenceInput
	runtimeReferenceMatrix
	runtimeReferenceSecret
	runtimeReferenceVar
	runtimeReferenceEnv
	runtimeReferenceStepOutput
	runtimeReferenceStepStatus
	runtimeReferenceNeedOutput
	runtimeReferenceNeedResult
	runtimeReferenceRunner
)

// validateRuntimeTemplate verifies that every expression in a runtime template
// is one direct reference supported by Evaluate. Runtime values are
// deliberately not resolved because many contexts do not exist until a job or
// step runs.
func validateRuntimeTemplate(template string) error {
	return visitTemplateExpressions(template, func(node actionlint.ExprNode) error {
		root, path, err := referencePath(node)
		if err != nil {
			return fmt.Errorf("runtime interpolation requires a direct context reference: %w", err)
		}
		if classifyRuntimeReference(root, path) == runtimeReferenceUnsupported {
			return fmt.Errorf("unsupported runtime expression %q", referenceName(root, path))
		}
		return nil
	})
}

// Evaluate substitutes direct runtime references in a template once.
func Evaluate(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateDirectRuntimeNode)
}

func EvaluateValue(source string, context Context) (any, error) {
	body, err := expressionBody(source)
	if err != nil {
		return nil, err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return nil, fmt.Errorf("invalid expression: %w", parseErr)
	}
	if call, ok := node.(*actionlint.FuncCallNode); ok && strings.EqualFold(call.Callee, "fromJSON") && len(call.Args) == 1 {
		value, err := evaluateDirectRuntimeNode(call.Args[0], context)
		if err != nil {
			return nil, err
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("fromJSON argument resolved to %T, want string", value)
		}
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("fromJSON: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("fromJSON: unexpected trailing content")
		}
		return decoded, nil
	}
	return evaluateDirectRuntimeNode(node, context)
}

// EvaluateStep substitutes direct runtime references and hashFiles calls in a
// workflow step template. Other runtime surfaces remain direct-reference only.
func EvaluateStep(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateStepRuntimeNode)
}

func evaluateRuntimeTemplate(template string, context Context, evaluate func(actionlint.ExprNode, Context) (any, error)) (string, error) {
	const open = "${{"
	var evaluated strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			evaluated.WriteString(remaining)
			return evaluated.String(), nil
		}
		evaluated.WriteString(remaining[:start])
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			if !strings.Contains(source, "}}") {
				return "", fmt.Errorf("unterminated expression in %q", template)
			}
			return "", fmt.Errorf("invalid expression: %w", lexErr)
		}
		node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(source[:consumed]))
		if parseErr != nil {
			return "", fmt.Errorf("invalid expression: %w", parseErr)
		}
		value, err := evaluate(node, context)
		if err != nil {
			return "", err
		}
		switch value := value.(type) {
		case nil:
		case string:
			evaluated.WriteString(value)
		default:
			_, _ = fmt.Fprint(&evaluated, value)
		}
		remaining = source[consumed:]
	}
}

func evaluateDirectRuntimeNode(node actionlint.ExprNode, context Context) (any, error) {
	root, path, err := referencePath(node)
	if err != nil {
		return nil, err
	}
	return resolveRuntimeReference(root, path, context)
}

func evaluateStepRuntimeNode(node actionlint.ExprNode, context Context) (any, error) {
	call, ok := node.(*actionlint.FuncCallNode)
	if !ok || !strings.EqualFold(call.Callee, "hashFiles") {
		return evaluateDirectRuntimeNode(node, context)
	}
	if context.HashFiles == nil {
		return nil, fmt.Errorf("runtime function %q is unavailable", call.Callee)
	}
	patterns, err := evaluateHashFilesArguments(call.Args, func(argument actionlint.ExprNode) (any, error) {
		return evaluateRuntimeHashFilesArgument(argument, context)
	})
	if err != nil {
		return nil, err
	}
	return context.HashFiles(patterns)
}

func evaluateHashFilesArguments(nodes []actionlint.ExprNode, evaluate func(actionlint.ExprNode) (any, error)) ([]string, error) {
	if len(nodes) == 0 || len(nodes) > 255 {
		return nil, fmt.Errorf("function %q requires 1 to 255 arguments", "hashFiles")
	}
	patterns := make([]string, len(nodes))
	for i, node := range nodes {
		value, err := evaluate(node)
		if err != nil {
			return nil, fmt.Errorf("hashFiles argument %d: %w", i+1, err)
		}
		patterns[i] = runtimeString(value)
	}
	return patterns, nil
}

func evaluateRuntimeHashFilesArgument(node actionlint.ExprNode, context Context) (any, error) {
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
		return evaluateDirectRuntimeNode(node, context)
	default:
		return nil, fmt.Errorf("arguments must be literals or direct context references")
	}
}

func runtimeString(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(value)
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}

func resolveRuntimeReference(root string, path []string, context Context) (any, error) {
	switch classifyRuntimeReference(root, path) {
	case runtimeReferenceRunner:
		if value, ok := findStringValue(context.Runner, path[0]); ok {
			return value, nil
		}
		return "", fmt.Errorf("expression references unavailable runner value %q", path[0])
	case runtimeReferenceServicePort:
		return resolveServicePort(context.Services, path[1], path[3], "expression")
	case runtimeReferenceServiceValue:
		return resolveServiceValue(context.Services, path[1], path[2], "expression")
	case runtimeReferenceGitHub:
		value, ok := lookupRuntimeValue(context.GitHub, path)
		if !ok {
			return "", fmt.Errorf("expression references unavailable github value %q", strings.Join(path, "."))
		}
		return value, nil
	case runtimeReferenceInput:
		return findString(context.Inputs, path[0]), nil
	case runtimeReferenceMatrix:
		for name, value := range context.Matrix {
			if strings.EqualFold(name, path[0]) {
				return value, nil
			}
		}
		return "", fmt.Errorf("expression references unavailable matrix value %q", path[0])
	case runtimeReferenceSecret:
		return findString(context.Secrets, path[0]), nil
	case runtimeReferenceVar:
		return findString(context.Vars, path[0]), nil
	case runtimeReferenceEnv:
		return findString(context.Env, path[0]), nil
	case runtimeReferenceStepOutput:
		outputs, ok := findOutputs(context.Steps, path[0])
		if !ok {
			return "", fmt.Errorf("expression references unavailable step %q", path[0])
		}
		return findString(outputs, path[2]), nil
	case runtimeReferenceStepStatus:
		for candidate, status := range context.StepStatuses {
			if !strings.EqualFold(candidate, path[0]) {
				continue
			}
			switch {
			case strings.EqualFold(path[1], "outcome"):
				return status.Outcome, nil
			case strings.EqualFold(path[1], "conclusion"):
				return status.Conclusion, nil
			}
		}
		return "", fmt.Errorf("expression references unavailable step %q", path[0])
	case runtimeReferenceNeedOutput:
		outputs, ok := findOutputs(context.Needs, path[0])
		if !ok {
			return "", fmt.Errorf("expression references unavailable need %q", path[0])
		}
		return findString(outputs, path[2]), nil
	case runtimeReferenceNeedResult:
		for candidate, result := range context.NeedResults {
			if strings.EqualFold(candidate, path[0]) {
				return result, nil
			}
		}
		return "", fmt.Errorf("expression references unavailable need %q", path[0])
	default:
		return nil, fmt.Errorf("unsupported expression %q", referenceName(root, path))
	}
}

func classifyRuntimeReference(root string, path []string) runtimeReferenceKind {
	switch {
	case len(path) == 1 && strings.EqualFold(root, "runner") && (strings.EqualFold(path[0], "os") || strings.EqualFold(path[0], "arch")):
		return runtimeReferenceRunner
	case len(path) == 4 && strings.EqualFold(root, "job") && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports"):
		return runtimeReferenceServicePort
	case len(path) == 3 && strings.EqualFold(root, "job") && strings.EqualFold(path[0], "services") && (strings.EqualFold(path[2], "id") || strings.EqualFold(path[2], "network")):
		return runtimeReferenceServiceValue
	case len(path) >= 1 && strings.EqualFold(root, "github"):
		return runtimeReferenceGitHub
	case len(path) == 1 && strings.EqualFold(root, "inputs"):
		return runtimeReferenceInput
	case len(path) == 1 && strings.EqualFold(root, "matrix"):
		return runtimeReferenceMatrix
	case len(path) == 1 && strings.EqualFold(root, "secrets"):
		return runtimeReferenceSecret
	case len(path) == 1 && strings.EqualFold(root, "vars"):
		return runtimeReferenceVar
	case len(path) == 1 && strings.EqualFold(root, "env"):
		return runtimeReferenceEnv
	case len(path) == 3 && strings.EqualFold(root, "steps") && strings.EqualFold(path[1], "outputs"):
		return runtimeReferenceStepOutput
	case len(path) == 2 && strings.EqualFold(root, "steps") && (strings.EqualFold(path[1], "outcome") || strings.EqualFold(path[1], "conclusion")):
		return runtimeReferenceStepStatus
	case len(path) == 3 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "outputs"):
		return runtimeReferenceNeedOutput
	case len(path) == 2 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "result"):
		return runtimeReferenceNeedResult
	default:
		return runtimeReferenceUnsupported
	}
}

func referenceName(root string, path []string) string {
	if len(path) == 0 {
		return root
	}
	return root + "." + strings.Join(path, ".")
}

func lookupRuntimeValue(value any, path []string) (any, bool) {
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

func findString(values map[string]string, name string) string {
	value, _ := findStringValue(values, name)
	return value
}

func findStringValue(values map[string]string, name string) (string, bool) {
	for candidate, value := range values {
		if strings.EqualFold(candidate, name) {
			return value, true
		}
	}
	return "", false
}

func findOutputs(values map[string]map[string]string, name string) (map[string]string, bool) {
	for candidate, outputs := range values {
		if strings.EqualFold(candidate, name) {
			return outputs, true
		}
	}
	return nil, false
}
