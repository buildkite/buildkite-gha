// Condition validation and strict runtime condition evaluation. The
// condition* value helpers fail closed on mixed-type comparisons.
package expression

import (
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

// ConditionContext contains the runtime values available while evaluating a
// job or step condition.
type ConditionContext struct {
	Inputs       map[string]string
	Needs        map[string]map[string]string
	NeedResults  map[string]string
	Steps        map[string]StepStatus
	Env          map[string]string
	Vars         map[string]string
	Matrix       map[string]any
	GitHub       map[string]any
	Runner       map[string]string
	Services     map[string]map[string]string
	Failure      bool
	Cancelled    bool
	Unsuccessful bool
	HashFiles    func([]string) (string, error)
}

// ConditionScope identifies the runtime phase in which a workflow condition
// is evaluated.
type ConditionScope uint8

const (
	// JobCondition is evaluated before a job starts.
	JobCondition ConditionScope = iota
	// StepCondition is evaluated while a job is running.
	StepCondition
)

// ValidateCondition verifies that a job or step condition uses only expression
// syntax, functions, and contexts implemented by the corresponding runtime
// phase. Runtime-dependent values are not evaluated.
func ValidateCondition(source string, scope ConditionScope) error {
	return validateCondition(source, scope, nil, false)
}

// ValidateConditionWithMatrix additionally verifies references and operand
// types against one concrete, statically expanded matrix instance.
func ValidateConditionWithMatrix(source string, scope ConditionScope, matrix map[string]any) error {
	return validateCondition(source, scope, matrix, true)
}

// ValidateCompileConditionWithMatrix verifies every branch of an event-backed
// condition before compile-time evaluation can short-circuit it. It admits the
// union of compile-time and runtime condition references, while retaining the
// concrete matrix type checks used by runtime validation.
func ValidateCompileConditionWithMatrix(source string, scope ConditionScope, context CompileContext, matrix map[string]any) error {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return err
	}
	context.Matrix = matrix
	return validateCompileConditionNode(node, scope, context, matrix)
}

func validateCondition(source string, scope ConditionScope, matrix map[string]any, matrixKnown bool) error {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return err
	}
	return validateConditionNode(node, scope, matrix, matrixKnown)
}

func validateConditionNode(node actionlint.ExprNode, scope ConditionScope, matrix map[string]any, matrixKnown bool) error {
	validator := newSemanticValidator(conditionSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, path []string) error {
		if matrixKnown && strings.EqualFold(root, "matrix") && len(path) == 1 {
			if _, ok := conditionMatrixValue(matrix, path[0]); !ok {
				return fmt.Errorf("condition reference %q is unavailable in this matrix instance", root+"."+path[0])
			}
		}
		return validateConditionReference(root, path, scope)
	}
	validator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		if !kind.IsEqualityOp() {
			return fmt.Errorf("condition comparison %s is unsupported", kind)
		}
		return nil
	}
	validator.afterCompare = func(node *actionlint.CompareOpNode) error {
		left, right := conditionOperandCategory(node.Left, matrix), conditionOperandCategory(node.Right, matrix)
		if left == "unsupported" || right == "unsupported" {
			return fmt.Errorf("condition equality uses an unsupported matrix value type")
		}
		if left != "unknown" && right != "unknown" && left != right {
			return fmt.Errorf("condition equality compares incompatible %s and %s operands", left, right)
		}
		return nil
	}
	validator.validateCall = func(_ *semanticValidator, node *actionlint.FuncCallNode) error {
		switch strings.ToLower(node.Callee) {
		case "always", "success", "failure", "cancelled":
			if len(node.Args) != 0 {
				return fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
			}
			return nil
		case "hashfiles":
			if scope != StepCondition {
				return fmt.Errorf("condition function %q is unavailable in job conditions", node.Callee)
			}
			if len(node.Args) == 0 || len(node.Args) > 255 {
				return fmt.Errorf("condition function %q requires 1 to 255 arguments", node.Callee)
			}
			for _, argument := range node.Args {
				if err := validateHashFilesArgument(argument, scope, matrix, matrixKnown); err != nil {
					return err
				}
			}
			return nil
		default:
			return fmt.Errorf("condition function %q is unsupported", node.Callee)
		}
	}
	validator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported condition expression") }
	return validator.validate(node)
}

func validateCompileConditionNode(node actionlint.ExprNode, scope ConditionScope, context CompileContext, matrix map[string]any) error {
	validator := newSemanticValidator(conditionSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, path []string) error {
		if strings.EqualFold(root, "github") && len(path) != 0 {
			if strings.EqualFold(path[0], "event") {
				return nil
			}
			switch strings.ToLower(path[0]) {
			case "actor", "event_name", "ref", "repository", "repository_owner", "sha", "workflow":
				if len(path) == 1 {
					return nil
				}
			}
		}
		if strings.EqualFold(root, "matrix") && len(path) == 1 {
			if _, ok := conditionMatrixValue(matrix, path[0]); !ok {
				return fmt.Errorf("condition reference %q is unavailable in this matrix instance", root+"."+path[0])
			}
		}
		return validateConditionReference(root, path, scope)
	}
	validator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		if !kind.IsEqualityOp() {
			return fmt.Errorf("condition comparison %s is unsupported", kind)
		}
		return nil
	}
	validator.afterCompare = func(node *actionlint.CompareOpNode) error {
		left := compileConditionOperandCategory(node.Left, context, matrix)
		right := compileConditionOperandCategory(node.Right, context, matrix)
		if left == "unsupported" || right == "unsupported" {
			return fmt.Errorf("condition equality uses an unsupported matrix value type")
		}
		if left != "unknown" && right != "unknown" && left != right {
			return fmt.Errorf("condition equality compares incompatible %s and %s operands", left, right)
		}
		return nil
	}
	validator.validateCall = func(validator *semanticValidator, node *actionlint.FuncCallNode) error {
		switch {
		case (strings.EqualFold(node.Callee, "always") || strings.EqualFold(node.Callee, "success") || strings.EqualFold(node.Callee, "failure") || strings.EqualFold(node.Callee, "cancelled")) && len(node.Args) == 0:
			return nil
		case strings.EqualFold(node.Callee, "fromJSON") && len(node.Args) == 1,
			(strings.EqualFold(node.Callee, "startsWith") || strings.EqualFold(node.Callee, "contains") || strings.EqualFold(node.Callee, "endsWith")) && len(node.Args) == 2:
			for _, argument := range node.Args {
				if err := validator.validate(argument); err != nil {
					return err
				}
			}
			_, err := evaluateCompileNode(node, context)
			return err
		case strings.EqualFold(node.Callee, "hashFiles") && scope == StepCondition:
			if len(node.Args) == 0 || len(node.Args) > 255 {
				return fmt.Errorf("condition function %q requires 1 to 255 arguments", node.Callee)
			}
			for _, argument := range node.Args {
				if err := validateHashFilesArgument(argument, scope, matrix, true); err != nil {
					return err
				}
			}
			return nil
		default:
			return fmt.Errorf("condition function %q is unsupported", node.Callee)
		}
	}
	validator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported condition expression") }
	return validator.validate(node)
}

func compileConditionOperandCategory(node actionlint.ExprNode, context CompileContext, matrix map[string]any) string {
	if value, err := evaluateCompileNode(node, context); err == nil {
		return conditionValueCategory(value)
	}
	root, _, err := referencePath(node)
	if err == nil && strings.EqualFold(root, "github") {
		return "unknown"
	}
	return conditionOperandCategory(node, matrix)
}

func conditionOperandCategory(node actionlint.ExprNode, matrix map[string]any) string {
	switch node := node.(type) {
	case *actionlint.NullNode:
		return "null"
	case *actionlint.BoolNode, *actionlint.NotOpNode, *actionlint.LogicalOpNode, *actionlint.CompareOpNode:
		return "boolean"
	case *actionlint.FuncCallNode:
		if strings.EqualFold(node.Callee, "hashFiles") {
			return "string"
		}
		return "boolean"
	case *actionlint.IntNode, *actionlint.FloatNode:
		return "number"
	case *actionlint.StringNode:
		return "string"
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return "unknown"
		}
		if !strings.EqualFold(root, "matrix") {
			return "string"
		}
		if len(path) == 1 {
			if value, ok := conditionMatrixValue(matrix, path[0]); ok {
				return conditionValueCategory(value)
			}
		}
	}
	return "unknown"
}

func conditionMatrixValue(matrix map[string]any, target string) (any, bool) {
	for name, value := range matrix {
		if strings.EqualFold(name, target) {
			return value, true
		}
	}
	return nil, false
}

func conditionValueCategory(value any) string {
	if _, ok := conditionNumber(value); ok {
		return "number"
	}
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	default:
		return "unsupported"
	}
}

func validateConditionReference(root string, path []string, scope ConditionScope) error {
	reference := root
	if len(path) != 0 {
		reference += "." + strings.Join(path, ".")
	}
	switch strings.ToLower(root) {
	case "runner":
		if len(path) == 1 && (strings.EqualFold(path[0], "os") || strings.EqualFold(path[0], "arch")) {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected runner.os or runner.arch", reference)
	case "github":
		if len(path) == 1 {
			switch strings.ToLower(path[0]) {
			case "actor", "event_name", "head_ref", "ref", "repository", "sha":
				return nil
			}
		}
		return fmt.Errorf("condition reference %q is unavailable at runtime; supported github properties are actor, event_name, head_ref, ref, repository, and sha", reference)
	case "needs":
		if len(path) == 2 && strings.EqualFold(path[1], "result") || len(path) == 3 && strings.EqualFold(path[1], "outputs") {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected needs.<job>.result or needs.<job>.outputs.<name>", reference)
	case "vars", "matrix":
		if len(path) == 1 {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected %s.<name>", reference, strings.ToLower(root))
	case "steps":
		if scope == JobCondition {
			return fmt.Errorf("condition context %q is unavailable in job conditions", root)
		}
		if len(path) == 2 && (strings.EqualFold(path[1], "outcome") || strings.EqualFold(path[1], "conclusion")) || len(path) == 3 && strings.EqualFold(path[1], "outputs") {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected steps.<step>.outcome, steps.<step>.conclusion, or steps.<step>.outputs.<name>", reference)
	case "env":
		if scope == JobCondition {
			return fmt.Errorf("condition context %q is unavailable in job conditions", root)
		}
		if len(path) == 1 {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected env.<name>", reference)
	case "job":
		if scope == JobCondition {
			return fmt.Errorf("condition context %q is unavailable in job conditions", root)
		}
		if len(path) == 4 && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports") {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected job.services.<service>.ports[<port>]", reference)
	default:
		return fmt.Errorf("condition context %q is unsupported", root)
	}
}

// EvaluateActionLifecycleCondition evaluates an action pre-if or post-if
// condition against the supplied lifecycle state. The accepted grammar is
// deliberately narrower than general conditions: exactly one bare status
// function with optional ${{ }} delimiters. An empty condition is
// unconditionally true (unlike general conditions, which apply the implicit
// success guard), failure() means unsuccessful and not cancelled, and any
// other expression fails closed with an error.
func EvaluateActionLifecycleCondition(value string, unsuccessful, cancelled bool) (bool, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "${{") && strings.HasSuffix(value, "}}") {
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "${{"), "}}"))
	}
	switch strings.ToLower(value) {
	case "", "always()":
		return true, nil
	case "success()":
		return !unsuccessful && !cancelled, nil
	case "failure()":
		return unsuccessful && !cancelled, nil
	case "cancelled()":
		return cancelled, nil
	default:
		return false, fmt.Errorf("condition %q is unsupported", value)
	}
}

// EvaluateCondition evaluates a job or step condition. Unsupported syntax and
// unavailable values fail closed with an error.
func EvaluateCondition(source string, context ConditionContext) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil {
		return false, err
	}
	if empty {
		return !context.Unsuccessful && !context.Cancelled, nil
	}
	if !containsStatusFunction(node) && (context.Unsuccessful || context.Cancelled) {
		return false, nil
	}
	value, err := evaluateConditionNode(node, context)
	if err != nil {
		return false, err
	}
	result := conditionTruthy(value)
	return result, nil
}

func evaluateConditionNode(node actionlint.ExprNode, context ConditionContext) (any, error) {
	evaluator := newSemanticEvaluator(conditionSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		return resolveConditionReference(root, path, context)
	}
	evaluator.truthy = conditionTruthy
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		equal, err := conditionEqual(left, right)
		if err != nil {
			return nil, err
		}
		switch kind {
		case actionlint.CompareOpNodeKindEq:
			return equal, nil
		case actionlint.CompareOpNodeKindNotEq:
			return !equal, nil
		default:
			return nil, fmt.Errorf("condition comparison %s is unsupported", kind)
		}
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported condition expression") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("condition logical operator %s is unsupported", kind)
	}
	evaluator.call = func(_ *semanticEvaluator, node *actionlint.FuncCallNode) (any, error) {
		switch strings.ToLower(node.Callee) {
		case "always":
			if len(node.Args) != 0 {
				return nil, fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
			}
			return true, nil
		case "success":
			if len(node.Args) != 0 {
				return nil, fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
			}
			return !context.Unsuccessful && !context.Cancelled, nil
		case "failure":
			if len(node.Args) != 0 {
				return nil, fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
			}
			return context.Failure, nil
		case "cancelled":
			if len(node.Args) != 0 {
				return nil, fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
			}
			return context.Cancelled, nil
		case "hashfiles":
			if context.HashFiles == nil {
				return nil, fmt.Errorf("condition function %q is unavailable", node.Callee)
			}
			patterns, err := evaluateHashFilesArguments(node.Args, func(argument actionlint.ExprNode) (any, error) {
				return evaluateConditionHashFilesArgument(argument, context)
			})
			if err != nil {
				return nil, err
			}
			return context.HashFiles(patterns)
		default:
			return nil, fmt.Errorf("condition function %q is unsupported", node.Callee)
		}
	}
	return evaluator.evaluate(node)
}

func validateHashFilesArgument(node actionlint.ExprNode, scope ConditionScope, matrix map[string]any, matrixKnown bool) error {
	switch node.(type) {
	case *actionlint.NullNode, *actionlint.BoolNode, *actionlint.IntNode, *actionlint.FloatNode, *actionlint.StringNode:
		return nil
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		return validateConditionNode(node, scope, matrix, matrixKnown)
	default:
		return fmt.Errorf("condition function %q arguments must be literals or direct context references", "hashFiles")
	}
}

func evaluateConditionHashFilesArgument(node actionlint.ExprNode, context ConditionContext) (any, error) {
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
		return resolveConditionReference(root, path, context)
	default:
		return nil, fmt.Errorf("arguments must be literals or direct context references")
	}
}

func resolveConditionReference(root string, path []string, context ConditionContext) (any, error) {
	switch {
	case len(path) == 1 && strings.EqualFold(root, "runner"):
		if value, ok := findStringValue(context.Runner, path[0]); ok {
			return value, nil
		}
	case len(path) == 4 && strings.EqualFold(root, "job") && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports"):
		return resolveServicePort(context.Services, path[1], path[3], "condition")
	case strings.EqualFold(root, "github"):
		if value, ok := lookupRuntimeValue(context.GitHub, path); ok {
			return value, nil
		}
	case len(path) == 1 && strings.EqualFold(root, "inputs") && context.Inputs != nil:
		return findString(context.Inputs, path[0]), nil
	case len(path) == 1 && strings.EqualFold(root, "env"):
		return findString(context.Env, path[0]), nil
	case len(path) == 1 && strings.EqualFold(root, "vars"):
		return findString(context.Vars, path[0]), nil
	case len(path) == 1 && strings.EqualFold(root, "matrix"):
		for name, value := range context.Matrix {
			if strings.EqualFold(name, path[0]) {
				return value, nil
			}
		}
	case len(path) == 2 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "result"):
		for name, result := range context.NeedResults {
			if strings.EqualFold(name, path[0]) {
				return result, nil
			}
		}
	case len(path) == 3 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "outputs"):
		if outputs, ok := findOutputs(context.Needs, path[0]); ok {
			return findString(outputs, path[2]), nil
		}
	case len(path) == 2 && strings.EqualFold(root, "steps"):
		for name, step := range context.Steps {
			if !strings.EqualFold(name, path[0]) {
				continue
			}
			switch {
			case strings.EqualFold(path[1], "outcome"):
				return step.Outcome, nil
			case strings.EqualFold(path[1], "conclusion"):
				return step.Conclusion, nil
			}
		}
	case len(path) == 3 && strings.EqualFold(root, "steps") && strings.EqualFold(path[1], "outputs"):
		for name, step := range context.Steps {
			if strings.EqualFold(name, path[0]) {
				return findString(step.Outputs, path[2]), nil
			}
		}
	}
	return nil, fmt.Errorf("condition references unavailable value %s.%s", root, strings.Join(path, "."))
}

// The condition* helpers are the strict evaluation family used by runtime
// conditions: mixed-type equality is an error rather than a coercion, so
// unsupported comparisons fail closed instead of silently converting. The
// actionInputDefault* family implements GitHub's loose coercion for action
// input defaults; the two families are deliberately separate and must not
// be unified.
func conditionTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	}
	number, ok := conditionNumber(value)
	return ok && number.Sign() != 0
}

func conditionEqual(left, right any) (bool, error) {
	if leftNumber, ok := conditionNumber(left); ok {
		rightNumber, ok := conditionNumber(right)
		if !ok {
			return false, fmt.Errorf("mixed-type condition equality is unsupported")
		}
		return leftNumber.Cmp(rightNumber) == 0, nil
	}
	switch left := left.(type) {
	case nil:
		if right != nil {
			return false, fmt.Errorf("mixed-type condition equality is unsupported")
		}
		return true, nil
	case string:
		right, ok := right.(string)
		if !ok {
			return false, fmt.Errorf("mixed-type condition equality is unsupported")
		}
		return strings.EqualFold(left, right), nil
	case bool:
		right, ok := right.(bool)
		if !ok {
			return false, fmt.Errorf("mixed-type condition equality is unsupported")
		}
		return left == right, nil
	}
	return false, fmt.Errorf("mixed-type condition equality is unsupported")
}
