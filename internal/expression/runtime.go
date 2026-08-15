// Direct runtime template evaluation and runtime reference resolution.
package expression

import (
	"context"
	"fmt"
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
	Services         map[string]map[string]string
	JobStatus        string
	HashFiles        func([]string) (string, error)
	HashFilesContext func(context.Context, []string) (string, error)
}

type runtimeReferenceKind uint8

const (
	runtimeReferenceUnsupported runtimeReferenceKind = iota
	runtimeReferenceServicePort
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
		validator := newSemanticValidator(runtimeReferenceSurface)
		validator.validateReference = func(_ actionlint.ExprNode, root string, path []string) error {
			if classifyRuntimeReference(root, path) == runtimeReferenceUnsupported {
				return fmt.Errorf("unsupported runtime expression %q", referenceName(root, path))
			}
			return nil
		}
		validator.referenceError = func(err error) error {
			return fmt.Errorf("runtime interpolation requires a direct context reference: %w", err)
		}
		validator.unsupported = func(node actionlint.ExprNode) error {
			_, _, err := referencePath(node)
			return fmt.Errorf("runtime interpolation requires a direct context reference: %w", err)
		}
		return validator.validate(node)
	})
}

// Evaluate substitutes direct runtime references in a template once.
func Evaluate(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateDirectRuntimeNode)
}

// EvaluateStep evaluates the expression surface available to workflow step
// fields. Other runtime surfaces remain direct-reference only.
func EvaluateStep(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateStepRuntimeNode)
}

// EvaluateStepControl evaluates one complete expression for a typed workflow
// step control.
func EvaluateStepControl(expression string, context Context) (any, error) {
	body, err := expressionBody(expression)
	if err != nil {
		return nil, err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return nil, fmt.Errorf("invalid expression: %w", parseErr)
	}
	return evaluateStepRuntimeExpression(node, context, true)
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
	evaluator := newSemanticEvaluator(runtimeReferenceSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		return resolveRuntimeReference(root, path, context)
	}
	evaluator.unsupported = unsupportedReference
	return evaluator.evaluate(node)
}

func evaluateStepRuntimeNode(node actionlint.ExprNode, context Context) (any, error) {
	return evaluateStepRuntimeExpression(node, context, true)
}

func evaluateStepRuntimeExpression(node actionlint.ExprNode, context Context, allowHashFiles bool) (any, error) {
	validator := newSemanticValidator(stepRuntimeSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, path []string) error {
		if strings.EqualFold(root, "github") {
			if len(path) != 1 {
				return fmt.Errorf("unsupported runtime github reference %q", referenceName(root, path))
			}
			switch strings.ToLower(path[0]) {
			case "actor", "event_name", "head_ref", "ref", "repository", "server_url", "sha", "token":
				return nil
			default:
				return fmt.Errorf("unsupported runtime github reference %q", referenceName(root, path))
			}
		}
		if classifyRuntimeReference(root, path) == runtimeReferenceUnsupported {
			return fmt.Errorf("unsupported runtime expression %q", referenceName(root, path))
		}
		return nil
	}
	validator.validateAccess = func(access actionlint.ExprNode) error {
		root := strings.ToLower(referenceRoot(access))
		switch root {
		case "github", "secrets":
			return fmt.Errorf("dynamic or whole %s access is unsupported", root)
		case "steps", "needs":
			return fmt.Errorf("computed or aggregate %s access is unsupported", root)
		case "matrix", "vars", "inputs", "env", "runner":
		default:
			return fmt.Errorf("unsupported runtime context %q", root)
		}
		switch access := access.(type) {
		case *actionlint.ObjectDerefNode:
			return validator.validate(access.Receiver)
		case *actionlint.IndexAccessNode:
			if err := validator.validate(access.Operand); err != nil {
				return err
			}
			return validator.validate(access.Index)
		case *actionlint.ArrayDerefNode:
			return validator.validate(access.Receiver)
		default:
			return nil
		}
	}
	validator.validateCompare = func(actionlint.CompareOpNodeKind) error { return nil }
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(validator *semanticValidator, call *actionlint.FuncCallNode) error {
		if recognized, err := validatePureFunction(validator, call); recognized {
			return err
		}
		if !allowHashFiles || !strings.EqualFold(call.Callee, "hashFiles") || len(call.Args) == 0 || len(call.Args) > 255 {
			return fmt.Errorf("unsupported runtime function %q", call.Callee)
		}
		for _, argument := range call.Args {
			if err := validator.validate(argument); err != nil {
				return err
			}
		}
		return nil
	}
	validator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported runtime expression") }
	if err := validator.validate(node); err != nil {
		return nil, err
	}

	evaluator := newSemanticEvaluator(stepRuntimeSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		return resolveRuntimeReferenceWithMissingMembers(root, path, context)
	}
	evaluator.resolveRoot = func(root string) (any, error) { return resolveStepRuntimeRoot(root, context) }
	evaluator.truthy = githubTruthy
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		return githubCompare(kind, left, right)
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported runtime expression") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("unsupported runtime logical operator %s", kind)
	}
	evaluator.call = func(evaluator *semanticEvaluator, call *actionlint.FuncCallNode) (any, error) {
		if value, recognized, err := evaluatePureFunction(evaluator, call); recognized {
			return value, err
		}
		if !allowHashFiles || !strings.EqualFold(call.Callee, "hashFiles") {
			return nil, fmt.Errorf("unsupported runtime function %q", call.Callee)
		}
		if context.HashFiles == nil {
			return nil, fmt.Errorf("runtime function %q is unavailable", call.Callee)
		}
		patterns, err := evaluateHashFilesArguments(call.Args, evaluator.evaluate)
		if err != nil {
			return nil, err
		}
		return context.HashFiles(patterns)
	}
	return evaluator.evaluate(node)
}

func resolveStepRuntimeRoot(root string, context Context) (any, error) {
	switch strings.ToLower(root) {
	case "github":
		return context.GitHub, nil
	case "secrets":
		return context.Secrets, nil
	case "matrix":
		return context.Matrix, nil
	case "vars":
		return context.Vars, nil
	case "inputs":
		return context.Inputs, nil
	case "env":
		return context.Env, nil
	case "runner":
		return context.Runner, nil
	case "steps":
		steps := make(map[string]any)
		for name, outputs := range context.Steps {
			steps[name] = map[string]any{"outputs": outputs}
		}
		for name, status := range context.StepStatuses {
			step, _ := steps[name].(map[string]any)
			if step == nil {
				step = map[string]any{"outputs": map[string]string{}}
				steps[name] = step
			}
			step["outcome"], step["conclusion"] = status.Outcome, status.Conclusion
		}
		return steps, nil
	case "needs":
		needs := make(map[string]any)
		for name, outputs := range context.Needs {
			needs[name] = map[string]any{"outputs": outputs, "result": context.NeedResults[name]}
		}
		for name, result := range context.NeedResults {
			if _, ok := needs[name]; !ok {
				needs[name] = map[string]any{"outputs": map[string]string{}, "result": result}
			}
		}
		return needs, nil
	default:
		return nil, fmt.Errorf("unsupported runtime context %q", root)
	}
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
	return resolveRuntimeReferenceValue(root, path, context, false)
}

func resolveRuntimeReferenceWithMissingMembers(root string, path []string, context Context) (any, error) {
	return resolveRuntimeReferenceValue(root, path, context, true)
}

func resolveRuntimeReferenceValue(root string, path []string, context Context, allowMissing bool) (any, error) {
	switch classifyRuntimeReference(root, path) {
	case runtimeReferenceRunner:
		if value, ok := findStringValue(context.Runner, path[0]); ok {
			return value, nil
		}
		return "", fmt.Errorf("expression references unavailable runner value %q", path[0])
	case runtimeReferenceServicePort:
		return resolveServicePort(context.Services, path[1], path[3], "expression")
	case runtimeReferenceGitHub:
		value, ok := lookupRuntimeValue(context.GitHub, path)
		if !ok {
			if !allowMissing || context.GitHub == nil || strings.EqualFold(path[0], "token") {
				return "", fmt.Errorf("expression references unavailable github value %q", strings.Join(path, "."))
			}
			return nil, nil
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
		if !allowMissing || context.Matrix == nil {
			return "", fmt.Errorf("expression references unavailable matrix value %q", path[0])
		}
		return nil, nil
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
