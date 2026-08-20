// Condition validation and strict runtime condition evaluation. The
// condition* value helpers reject mixed-type comparisons.
package expression

import (
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

// ConditionContext contains the runtime values available while evaluating a
// job or step condition.
type ConditionContext struct {
	Inputs       map[string]any
	Needs        map[string]NeedStatus
	Steps        map[string]StepStatus
	Env          map[string]string
	Vars         map[string]string
	Matrix       map[string]any
	GitHub       map[string]any
	Runner       map[string]string
	Services     map[string]ServiceContext
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
	// CallCondition is evaluated in the caller scope before a local reusable
	// workflow's flattened jobs are allowed to start.
	CallCondition
	// actionLifecycleCondition is evaluated for action pre-if and post-if
	// metadata. It has its own context policy and no implicit success guard.
	actionLifecycleCondition
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

// ValidateCallCondition verifies the caller-only runtime surface of a local
// reusable-workflow call condition.
func ValidateCallCondition(source string) error {
	return validateCondition(source, CallCondition, nil, false)
}

// ValidateCompileCallCondition verifies every branch of a call condition
// before event-backed values are reduced by the compiler.
func ValidateCompileCallCondition(source string, context CompileContext) error {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return err
	}
	return validateCompileConditionNode(node, CallCondition, context, nil)
}

// ValidateActionLifecycleCondition verifies an action pre-if or post-if
// expression without resolving runtime-dependent values.
func ValidateActionLifecycleCondition(source string) error {
	if err := validateLifecycleDelimiters(source); err != nil {
		return err
	}
	return validateCondition(source, actionLifecycleCondition, nil, false)
}

func validateLifecycleDelimiters(source string) error {
	condition := strings.TrimSpace(source)
	if strings.HasPrefix(condition, "${{") != strings.HasSuffix(condition, "}}") {
		return fmt.Errorf("parse condition: mismatched expression delimiters")
	}
	return nil
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
		return validateConditionReference(root, path, scope)
	}
	validator.validateAccess = func(node actionlint.ExprNode) error {
		return validateConditionAccessNode(&validator, node, scope)
	}
	validator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		return nil
	}
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(validator *semanticValidator, node *actionlint.FuncCallNode) error {
		if recognized, err := validatePureFunction(validator, node); recognized {
			return err
		}
		switch strings.ToLower(node.Callee) {
		case "always", "success", "failure", "cancelled":
			if len(node.Args) != 0 {
				return fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
			}
			return nil
		case "hashfiles":
			if scope != StepCondition && scope != actionLifecycleCondition {
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
				if len(path) == 1 {
					return fmt.Errorf("whole github.event access is unsupported")
				}
				return nil
			}
			switch strings.ToLower(path[0]) {
			case "actor", "base_ref", "event_name", "ref", "ref_name", "ref_type", "repository", "repository_owner", "sha", "workflow":
				if len(path) == 1 {
					return nil
				}
			}
		}
		return validateConditionReference(root, path, scope)
	}
	validator.validateAccess = func(node actionlint.ExprNode) error {
		root := referenceRoot(node)
		if strings.EqualFold(root, "github") || strings.EqualFold(root, "event") || strings.EqualFold(root, "vars") || strings.EqualFold(root, "matrix") {
			return validateCompileAccessNode(&validator, node)
		}
		return validateConditionAccessNode(&validator, node, scope)
	}
	validator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		return nil
	}
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(validator *semanticValidator, node *actionlint.FuncCallNode) error {
		if recognized, err := validatePureFunction(validator, node); recognized {
			return err
		}
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

func validateConditionReference(root string, path []string, scope ConditionScope) error {
	reference := root
	if len(path) != 0 {
		reference += "." + strings.Join(path, ".")
	}
	if scope == actionLifecycleCondition {
		switch strings.ToLower(root) {
		case "env", "github", "inputs", "job", "matrix", "runner", "steps":
		default:
			return fmt.Errorf("lifecycle condition context %q is unsupported", root)
		}
	}
	if scope == CallCondition {
		switch strings.ToLower(root) {
		case "github", "vars", "inputs", "needs":
		default:
			return fmt.Errorf("reusable-workflow call condition context %q is unsupported", root)
		}
	}
	switch strings.ToLower(root) {
	case "runner":
		if len(path) == 1 {
			if strings.EqualFold(path[0], "os") || strings.EqualFold(path[0], "arch") {
				return nil
			}
			if strings.EqualFold(path[0], "temp") && scope != JobCondition && scope != CallCondition {
				return nil
			}
		}
		if scope == JobCondition {
			return fmt.Errorf("condition reference %q is unsupported; expected runner.os or runner.arch", reference)
		}
		return fmt.Errorf("condition reference %q is unsupported; expected runner.os, runner.arch, or runner.temp", reference)
	case "github":
		if len(path) == 1 {
			switch strings.ToLower(path[0]) {
			case "actor", "base_ref", "event_name", "head_ref", "ref", "ref_name", "ref_type", "repository", "repository_owner", "sha":
				return nil
			}
		}
		return fmt.Errorf("condition reference %q is unavailable at runtime; supported github properties are actor, base_ref, event_name, head_ref, ref, ref_name, ref_type, repository, repository_owner, and sha", reference)
	case "needs":
		if len(path) == 2 && strings.EqualFold(path[1], "result") || len(path) == 3 && strings.EqualFold(path[1], "outputs") {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected needs.<job>.result or needs.<job>.outputs.<name>", reference)
	case "vars":
		if len(path) == 1 {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected vars.<name>", reference)
	case "inputs":
		if len(path) == 1 {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected inputs.<name>", reference)
	case "matrix":
		// Matrix values may be objects or arrays, so nested references such
		// as matrix.config.os are valid.
		if len(path) >= 1 {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected matrix.<name>", reference)
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
		if len(path) == 4 && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports") || len(path) == 3 && strings.EqualFold(path[0], "services") && (strings.EqualFold(path[2], "id") || strings.EqualFold(path[2], "network")) {
			return nil
		}
		return fmt.Errorf("condition reference %q is unsupported; expected job.services.<service>.id, job.services.<service>.network, or job.services.<service>.ports[<port>]", reference)
	default:
		return fmt.Errorf("condition context %q is unsupported", root)
	}
}

func validateConditionAccessNode(validator *semanticValidator, node actionlint.ExprNode, scope ConditionScope) error {
	root := strings.ToLower(referenceRoot(node))
	if scope == actionLifecycleCondition {
		switch root {
		case "env", "github", "inputs", "job", "matrix", "runner", "steps":
		default:
			return fmt.Errorf("lifecycle condition context %q is unsupported", root)
		}
		if _, whole := node.(*actionlint.VariableNode); whole {
			return fmt.Errorf("whole lifecycle condition context %q is unsupported", root)
		}
		staticRoot, path, err := referencePath(node)
		if err != nil {
			return fmt.Errorf("dynamic lifecycle condition access is unsupported")
		}
		return validateConditionReference(staticRoot, path, scope)
	}
	switch root {
	case "":
		switch node := node.(type) {
		case *actionlint.ObjectDerefNode:
			return validator.validate(node.Receiver)
		case *actionlint.ArrayDerefNode:
			return validator.validate(node.Receiver)
		case *actionlint.IndexAccessNode:
			if err := validator.validate(node.Operand); err != nil {
				return err
			}
			return validator.validate(node.Index)
		}
		return fmt.Errorf("unsupported condition access expression")
	case "matrix", "needs":
	case "vars":
		if _, whole := node.(*actionlint.VariableNode); whole {
			return fmt.Errorf("whole condition context %q is unsupported", root)
		}
	case "inputs":
		if _, whole := node.(*actionlint.VariableNode); whole {
			return fmt.Errorf("whole condition context %q is unsupported", root)
		}
	case "steps":
		if scope == JobCondition {
			return fmt.Errorf("condition context %q is unavailable in job conditions", root)
		}
	case "env":
		if scope == JobCondition {
			return fmt.Errorf("condition context %q is unavailable in job conditions", root)
		}
		if _, whole := node.(*actionlint.VariableNode); whole {
			return fmt.Errorf("whole condition context %q is unsupported", root)
		}
	case "github":
		return fmt.Errorf("dynamic or whole github access is unsupported")
	default:
		return fmt.Errorf("condition context %q is unsupported", root)
	}
	var validationErr error
	actionlint.VisitExprNode(node, func(candidate, _ actionlint.ExprNode, entering bool) {
		if !entering || validationErr != nil {
			return
		}
		if index, ok := candidate.(*actionlint.IndexAccessNode); ok {
			validationErr = validator.validate(index.Index)
		}
	})
	return validationErr
}

// EvaluateActionLifecycleCondition evaluates action pre-if or post-if
// metadata. Empty conditions are unconditionally true and, unlike workflow
// step conditions, lifecycle conditions have no implicit success guard.
func EvaluateActionLifecycleCondition(source string, context ConditionContext) (bool, error) {
	if err := validateLifecycleDelimiters(source); err != nil {
		return false, err
	}
	node, empty, err := parseCondition(source)
	if err != nil {
		return false, err
	}
	if empty {
		return true, nil
	}
	// Validate before evaluation so short-circuiting cannot hide an
	// unsupported context or function in an unselected branch.
	if err := validateConditionNode(node, actionLifecycleCondition, nil, false); err != nil {
		return false, err
	}
	value, err := evaluateConditionNode(node, context)
	if err != nil {
		return false, err
	}
	return githubTruthy(value), nil
}

// EvaluateCondition evaluates a job or step condition. Unsupported syntax and
// unavailable values return an error.
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
	result := githubTruthy(value)
	return result, nil
}

// EvaluateKnownInputCondition evaluates a condition when its result is fixed
// by known action inputs and short-circuiting. Runtime-dependent results are
// reported as unknown.
func EvaluateKnownInputCondition(source string, inputs map[string]any, unknownInputs map[string]bool) (bool, bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil {
		return false, false, err
	}
	if empty {
		return true, true, nil
	}
	return evaluateKnownInputConditionNode(node, ConditionContext{Inputs: inputs}, unknownInputs)
}

func evaluateKnownInputConditionNode(node actionlint.ExprNode, context ConditionContext, unknownInputs map[string]bool) (bool, bool, error) {
	if logical, ok := node.(*actionlint.LogicalOpNode); ok {
		left, known, err := evaluateKnownInputConditionNode(logical.Left, context, unknownInputs)
		if err != nil {
			return false, false, err
		}
		if !known {
			right, rightKnown, rightErr := evaluateKnownInputConditionNode(logical.Right, context, unknownInputs)
			if rightErr != nil {
				return false, false, rightErr
			}
			if rightKnown && (logical.Kind == actionlint.LogicalOpNodeKindAnd && !right || logical.Kind == actionlint.LogicalOpNodeKindOr && right) {
				return right, true, nil
			}
			return false, false, nil
		}
		switch logical.Kind {
		case actionlint.LogicalOpNodeKindAnd:
			if !left {
				return false, true, nil
			}
		case actionlint.LogicalOpNodeKindOr:
			if left {
				return true, true, nil
			}
		}
		return evaluateKnownInputConditionNode(logical.Right, context, unknownInputs)
	}

	names, dynamic := nodeInputReferences(node)
	if dynamic && len(unknownInputs) != 0 {
		return false, false, nil
	}
	for _, name := range names {
		if unknownInputs[name] {
			return false, false, nil
		}
	}
	if containsStatusFunction(node) {
		return false, false, nil
	}
	for _, contextName := range []string{"env", "github", "job", "matrix", "needs", "runner", "steps", "vars"} {
		if nodeUsesContext(node, contextName) {
			return false, false, nil
		}
	}
	value, err := evaluateConditionNode(node, context)
	if err != nil {
		return false, false, nil
	}
	return githubTruthy(value), true, nil
}

func evaluateConditionNode(node actionlint.ExprNode, context ConditionContext) (any, error) {
	evaluator := newSemanticEvaluator(conditionSurface)
	rootValues := make(map[string]any)
	evaluator.resolve = func(root string, path []string) (any, error) {
		return resolveConditionReference(root, path, context)
	}
	evaluator.resolveRoot = func(root string) (any, error) {
		key := strings.ToLower(root)
		if value, ok := rootValues[key]; ok {
			return value, nil
		}
		value, err := resolveConditionRoot(root, context)
		if err == nil {
			rootValues[key] = value
		}
		return value, err
	}
	evaluator.truthy = githubTruthy
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		return githubCompare(kind, left, right)
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported condition expression") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("condition logical operator %s is unsupported", kind)
	}
	evaluator.call = func(_ *semanticEvaluator, node *actionlint.FuncCallNode) (any, error) {
		if value, recognized, err := evaluatePureFunction(&evaluator, node); recognized {
			return value, err
		}
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

func resolveConditionRoot(root string, context ConditionContext) (any, error) {
	switch strings.ToLower(root) {
	case "matrix":
		if context.Matrix == nil {
			return nil, fmt.Errorf("condition context %q is unavailable", root)
		}
		return context.Matrix, nil
	case "vars":
		return context.Vars, nil
	case "inputs":
		if context.Inputs == nil {
			return nil, fmt.Errorf("condition context %q is unavailable", root)
		}
		return context.Inputs, nil
	case "env":
		return context.Env, nil
	case "needs":
		needs := make(map[string]any)
		for name, status := range context.Needs {
			needs[name] = map[string]any{"outputs": status.Outputs, "result": status.Result}
		}
		return needs, nil
	case "steps":
		steps := make(map[string]any)
		for name, step := range context.Steps {
			steps[name] = map[string]any{"outputs": step.Outputs, "outcome": step.Outcome, "conclusion": step.Conclusion}
		}
		return steps, nil
	default:
		return nil, fmt.Errorf("condition context %q is unsupported", root)
	}
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
	case len(path) == 3 && strings.EqualFold(root, "job") && strings.EqualFold(path[0], "services") && (strings.EqualFold(path[2], "id") || strings.EqualFold(path[2], "network")):
		return resolveServiceValue(context.Services, path[1], path[2], "condition")
	case strings.EqualFold(root, "github"):
		if value, ok := lookupRuntimeValue(context.GitHub, path); ok {
			return value, nil
		}
		if context.GitHub == nil {
			break
		}
		return nil, nil
	case len(path) == 1 && strings.EqualFold(root, "inputs") && context.Inputs != nil:
		value, found, err := objectValue(context.Inputs, path[0])
		if err != nil || found {
			return value, err
		}
		return nil, nil
	case len(path) == 1 && strings.EqualFold(root, "env"):
		return findString(context.Env, path[0]), nil
	case len(path) == 1 && strings.EqualFold(root, "vars"):
		return findString(context.Vars, path[0]), nil
	case len(path) >= 1 && strings.EqualFold(root, "matrix"):
		for name, value := range context.Matrix {
			if strings.EqualFold(name, path[0]) {
				// Nested references such as matrix.config.os walk into
				// object-valued matrix entries; a missing or non-object
				// segment yields null, matching GitHub.
				for _, part := range path[1:] {
					var (
						ok  bool
						err error
					)
					value, ok, err = objectValue(value, part)
					if err != nil {
						return nil, err
					}
					if !ok {
						return nil, nil
					}
				}
				return value, nil
			}
		}
		if context.Matrix == nil {
			break
		}
		return nil, nil
	case len(path) == 2 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "result"):
		if status, ok := findNeedStatus(context.Needs, path[0]); ok {
			return status.Result, nil
		}
	case len(path) == 3 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "outputs"):
		if status, ok := findNeedStatus(context.Needs, path[0]); ok {
			return findString(status.Outputs, path[2]), nil
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
