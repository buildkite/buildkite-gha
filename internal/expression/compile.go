// Compile-time expression evaluation: graph-construction substitution and
// evaluation against the snapshotted CompileContext.

package expression

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

// CompileContext contains the non-secret values available while constructing
// a workflow graph. Values are snapshots supplied by the compiler; evaluation
// never reads the process environment or a secret provider.
type CompileContext struct {
	GitHub   map[string]any
	Event    map[string]any
	Vars     map[string]string
	Inputs   map[string]any
	Matrix   map[string]any
	Strategy map[string]any
}

// evaluateCompile evaluates one complete graph-time expression. The supported
// surface is intentionally limited to literals, github/event/vars/matrix
// references, boolean/equality operators, and selected pure functions.
func evaluateCompile(expr Expression, context CompileContext) (any, error) {
	node, err := parseCompileExpression(expr)
	if err != nil {
		return nil, err
	}
	if err := validateCompileExpressionNode(node); err != nil {
		return nil, err
	}
	return evaluateCompileNode(node, context)
}

// evaluateCompileAvailable evaluates an expression until it either produces a
// value, needs a runtime-only value, or encounters a deterministic error.
func evaluateCompileAvailable(expr Expression, context CompileContext) (any, bool, error) {
	node, err := parseCompileExpression(expr)
	if err != nil {
		return nil, false, err
	}
	value, err := evaluateCompileNode(node, context)
	var dependency compileRuntimeDependencyError
	if errors.As(err, &dependency) {
		return nil, false, nil
	}
	return value, err == nil, err
}

func parseCompileExpression(expr Expression) (actionlint.ExprNode, error) {
	body, err := expressionBody(expr.Text)
	if err != nil {
		return nil, err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return nil, fmt.Errorf("invalid expression: %w", parseErr)
	}
	return node, nil
}

// validateReusableInputDefault validates an expression-valued workflow_call
// input default without resolving its values.
func validateReusableInputDefault(template string) error {
	return visitTemplateExpressions(template, validateReusableInputDefaultNode)
}

func validateReusableInputDefaultNode(node actionlint.ExprNode) error {
	validator := newSemanticValidator(compileTimeSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, _ []string) error {
		if !strings.EqualFold(root, "github") && !strings.EqualFold(root, "vars") {
			return fmt.Errorf("reusable-workflow input default context %q is unavailable", root)
		}
		return nil
	}
	validator.validateAccess = func(node actionlint.ExprNode) error {
		root := referenceRoot(node)
		if !strings.EqualFold(root, "github") && !strings.EqualFold(root, "vars") {
			return fmt.Errorf("reusable-workflow input default context %q is unavailable", root)
		}
		switch node := node.(type) {
		case *actionlint.ObjectDerefNode:
			return validator.validate(node.Receiver)
		case *actionlint.IndexAccessNode:
			if err := validator.validate(node.Operand); err != nil {
				return err
			}
			return validator.validate(node.Index)
		case *actionlint.ArrayDerefNode:
			return validator.validate(node.Receiver)
		default:
			return nil
		}
	}
	validator.validateCompare = func(actionlint.CompareOpNodeKind) error { return nil }
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(validator *semanticValidator, node *actionlint.FuncCallNode) error {
		if recognized, err := validatePureFunction(validator, node); recognized {
			return err
		}
		return fmt.Errorf("unsupported reusable-workflow input default function %q", node.Callee)
	}
	validator.unsupported = func(actionlint.ExprNode) error {
		return fmt.Errorf("unsupported reusable-workflow input default expression")
	}
	if err := validator.validate(node); err != nil {
		return err
	}
	return validateCompileExpressionNode(node)
}

// evaluateReusableInputDefault evaluates a statically available workflow_call
// input default. A complete expression preserves its type; a template renders
// to a string.
func evaluateReusableInputDefault(template string, context CompileContext) (any, error) {
	if err := validateReusableInputDefault(template); err != nil {
		return nil, err
	}
	if expression, err := parseExpression(template, 1, 1); err == nil {
		return evaluateCompile(expression, context)
	}
	return evaluateCompileTemplate(template, context)
}

// validateRunName validates the supported expression surface available while
// creating a workflow run.
func validateRunName(template string) error {
	return visitTemplateExpressions(template, validateRunNameNode)
}

func validateRunNameNode(node actionlint.ExprNode) error {
	validator := newSemanticValidator(compileTimeSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, _ []string) error {
		if !strings.EqualFold(root, "github") && !strings.EqualFold(root, "inputs") {
			return fmt.Errorf("run-name context %q is unavailable", root)
		}
		return nil
	}
	validator.validateAccess = func(node actionlint.ExprNode) error {
		root := referenceRoot(node)
		if root != "" && !strings.EqualFold(root, "github") && !strings.EqualFold(root, "inputs") {
			return fmt.Errorf("run-name context %q is unavailable", root)
		}
		switch node := node.(type) {
		case *actionlint.ObjectDerefNode:
			return validator.validate(node.Receiver)
		case *actionlint.IndexAccessNode:
			if err := validator.validate(node.Operand); err != nil {
				return err
			}
			return validator.validate(node.Index)
		case *actionlint.ArrayDerefNode:
			return validator.validate(node.Receiver)
		default:
			return nil
		}
	}
	validator.validateCompare = func(actionlint.CompareOpNodeKind) error { return nil }
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(validator *semanticValidator, node *actionlint.FuncCallNode) error {
		if recognized, err := validatePureFunction(validator, node); recognized {
			return err
		}
		return fmt.Errorf("unsupported run-name function %q", node.Callee)
	}
	validator.unsupported = func(actionlint.ExprNode) error {
		return fmt.Errorf("unsupported run-name expression")
	}
	if err := validator.validate(node); err != nil {
		return err
	}
	return validateCompileExpressionNode(node)
}

// evaluateRunName evaluates a validated workflow run-name.
func evaluateRunName(template string, context CompileContext) (string, error) {
	if err := validateRunName(template); err != nil {
		return "", err
	}
	return evaluateCompileTemplate(template, context)
}

func validateCompileExpressionNode(node actionlint.ExprNode) error {
	validator := newSemanticValidator(compileTimeSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, path []string) error {
		reference := referenceName(root, path)
		switch {
		case strings.EqualFold(root, "vars") && len(path) == 1,
			strings.EqualFold(root, "inputs") && len(path) == 1,
			// Matrix values may be objects or arrays (matrix include entries
			// and object lists), so nested references such as
			// matrix.config.os are valid.
			strings.EqualFold(root, "matrix") && len(path) >= 1,
			strings.EqualFold(root, "strategy") && len(path) == 1,
			strings.EqualFold(root, "event") && len(path) >= 1:
			return nil
		case strings.EqualFold(root, "github") && len(path) == 1:
			switch strings.ToLower(path[0]) {
			case "actor", "base_ref", "event_name", "head_ref", "ref", "ref_name", "ref_type", "repository", "repository_owner", "sha", "workflow":
				return nil
			}
		case strings.EqualFold(root, "github") && len(path) >= 2 && strings.EqualFold(path[0], "event"):
			return nil
		}
		if strings.EqualFold(root, "github") {
			return fmt.Errorf("compile-time expression references unavailable value %q", reference)
		}
		if !strings.EqualFold(root, "vars") && !strings.EqualFold(root, "inputs") && !strings.EqualFold(root, "matrix") && !strings.EqualFold(root, "strategy") && !strings.EqualFold(root, "event") {
			return fmt.Errorf("unsupported compile-time context %q", root)
		}
		return fmt.Errorf("unsupported compile-time reference %q", reference)
	}
	validator.validateAccess = func(node actionlint.ExprNode) error {
		return validateCompileAccessNode(&validator, node)
	}
	validator.validateCompare = func(actionlint.CompareOpNodeKind) error { return nil }
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(validator *semanticValidator, node *actionlint.FuncCallNode) error {
		if recognized, err := validatePureFunction(validator, node); recognized {
			return err
		}
		return fmt.Errorf("unsupported compile-time function %q", node.Callee)
	}
	validator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported compile-time expression") }
	return validator.validate(node)
}

func validateCompileAccessNode(validator *semanticValidator, node actionlint.ExprNode) error {
	switch node := node.(type) {
	case *actionlint.VariableNode:
		switch strings.ToLower(node.Name) {
		case "vars", "inputs", "matrix", "strategy":
			return nil
		case "event":
			return fmt.Errorf("whole event access is unsupported")
		case "github":
			return fmt.Errorf("whole github access is unsupported")
		default:
			return fmt.Errorf("unsupported compile-time context %q", node.Name)
		}
	case *actionlint.ObjectDerefNode:
		if referenceRoot(node) == "" {
			return validator.validate(node.Receiver)
		}
		if root, path, err := referencePath(node); err == nil && strings.EqualFold(root, "github") && len(path) >= 2 && strings.EqualFold(path[0], "event") {
			return nil
		}
		if variable, ok := node.Receiver.(*actionlint.VariableNode); ok && strings.EqualFold(variable.Name, "event") {
			return nil
		}
		if variable, ok := node.Receiver.(*actionlint.VariableNode); ok && strings.EqualFold(variable.Name, "github") {
			if strings.EqualFold(node.Property, "event") {
				return nil
			}
			return fmt.Errorf("compile-time expression references unavailable value %q", "github."+node.Property)
		}
		return validateCompileAccessNode(validator, node.Receiver)
	case *actionlint.ArrayDerefNode:
		if referenceRoot(node) == "" {
			return validator.validate(node.Receiver)
		}
		if root, path, err := referencePath(node.Receiver); err == nil && strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "event") {
			return fmt.Errorf("whole event projection is unsupported")
		}
		return validateCompileAccessNode(validator, node.Receiver)
	case *actionlint.IndexAccessNode:
		if variable, ok := node.Operand.(*actionlint.VariableNode); ok {
			if strings.EqualFold(variable.Name, "github") {
				return fmt.Errorf("dynamic github access is unsupported")
			}
			if strings.EqualFold(variable.Name, "event") {
				return validator.validate(node.Index)
			}
		}
		var err error
		switch node.Operand.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.ArrayDerefNode, *actionlint.IndexAccessNode:
			err = validateCompileAccessNode(validator, node.Operand)
		default:
			err = validator.validate(node.Operand)
		}
		if err != nil {
			return err
		}
		return validator.validate(node.Index)
	default:
		return fmt.Errorf("unsupported compile-time access expression")
	}
}

// evaluateCompileCondition evaluates a condition whose entire value is known
// while constructing the graph. Callers may fall back to runtime condition
// handling when this returns an unavailable-context or unsupported error.
func evaluateCompileCondition(source string, context CompileContext) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil {
		return false, err
	}
	if empty {
		return true, nil
	}
	value, err := evaluateCompileNode(node, context)
	if err != nil {
		return false, err
	}
	return githubTruthy(value), nil
}

// reduceCompileCondition replaces every compile-time scalar subtree in a
// condition while preserving runtime-dependent subtrees for later evaluation.
func reduceCompileCondition(source string, context CompileContext) (string, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return source, err
	}
	if _, _, err := evaluateCompileNodeAvailable(node, context); err != nil {
		return "", err
	}
	return reduceCompileNode(node, context), nil
}

func reduceCompileNode(node actionlint.ExprNode, context CompileContext) string {
	if value, available, _ := evaluateCompileNodeAvailable(node, context); available {
		if literal, ok := compileScalarLiteral(value); ok {
			return literal
		}
	}
	switch node := node.(type) {
	case *actionlint.VariableNode:
		return node.Name
	case *actionlint.ObjectDerefNode:
		return reduceCompileNode(node.Receiver, context) + "." + node.Property
	case *actionlint.ArrayDerefNode:
		return reduceCompileNode(node.Receiver, context) + ".*"
	case *actionlint.IndexAccessNode:
		return reduceCompileNode(node.Operand, context) + "[" + reduceCompileNode(node.Index, context) + "]"
	case *actionlint.NotOpNode:
		return "!(" + reduceCompileNode(node.Operand, context) + ")"
	case *actionlint.CompareOpNode:
		return "(" + reduceCompileNode(node.Left, context) + " " + node.Kind.String() + " " + reduceCompileNode(node.Right, context) + ")"
	case *actionlint.LogicalOpNode:
		return "(" + reduceCompileNode(node.Left, context) + " " + node.Kind.String() + " " + reduceCompileNode(node.Right, context) + ")"
	case *actionlint.FuncCallNode:
		arguments := make([]string, len(node.Args))
		for i, argument := range node.Args {
			arguments[i] = reduceCompileNode(argument, context)
		}
		return node.Callee + "(" + strings.Join(arguments, ", ") + ")"
	default:
		return ""
	}
}

// evaluateCompileNodeAvailable checks every branch before evaluation. This
// prevents short-circuit evaluation from hiding runtime dependencies or
// deterministic errors in an unselected branch.
func evaluateCompileNodeAvailable(node actionlint.ExprNode, context CompileContext) (any, bool, error) {
	children := func(nodes ...actionlint.ExprNode) (bool, error) {
		available := true
		for _, child := range nodes {
			_, childAvailable, err := evaluateCompileNodeAvailable(child, context)
			if err != nil {
				return false, err
			}
			available = available && childAvailable
		}
		return available, nil
	}
	switch node := node.(type) {
	case *actionlint.ObjectDerefNode:
		if available, err := children(node.Receiver); err != nil || !available {
			return nil, available, err
		}
	case *actionlint.ArrayDerefNode:
		if available, err := children(node.Receiver); err != nil || !available {
			return nil, available, err
		}
	case *actionlint.NotOpNode:
		if available, err := children(node.Operand); err != nil || !available {
			return nil, available, err
		}
	case *actionlint.CompareOpNode:
		if available, err := children(node.Left, node.Right); err != nil || !available {
			return nil, available, err
		}
	case *actionlint.LogicalOpNode:
		if available, err := children(node.Left, node.Right); err != nil || !available {
			return nil, available, err
		}
	case *actionlint.FuncCallNode:
		if available, err := children(node.Args...); err != nil || !available {
			return nil, available, err
		}
	case *actionlint.IndexAccessNode:
		if available, err := children(node.Operand, node.Index); err != nil || !available {
			return nil, available, err
		}
	}
	value, err := evaluateCompileNode(node, context)
	var dependency compileRuntimeDependencyError
	if errors.As(err, &dependency) {
		return nil, false, nil
	}
	return value, err == nil, err
}

func compileScalarLiteral(value any) (string, bool) {
	literal, err := compileInputLiteral(value)
	return literal, err == nil
}

func evaluateCompileNode(node actionlint.ExprNode, context CompileContext) (any, error) {
	evaluator := newSemanticEvaluator(compileTimeSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		return resolveCompileReference(root, path, context)
	}
	evaluator.resolveRoot = func(root string) (any, error) {
		switch strings.ToLower(root) {
		case "github":
			if context.GitHub == nil {
				return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time context %q is unavailable", root)}
			}
			return context.GitHub, nil
		case "event":
			if context.Event == nil {
				return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time context %q is unavailable", root)}
			}
			return context.Event, nil
		case "vars":
			if context.Vars == nil {
				return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time context %q is unavailable", root)}
			}
			return context.Vars, nil
		case "matrix":
			if context.Matrix == nil {
				return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time context %q is unavailable", root)}
			}
			return context.Matrix, nil
		case "inputs":
			if context.Inputs == nil {
				return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time context %q is unavailable", root)}
			}
			return context.Inputs, nil
		case "strategy":
			if context.Strategy == nil {
				return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time context %q is unavailable", root)}
			}
			return context.Strategy, nil
		default:
			return nil, compileRuntimeDependencyError{fmt.Errorf("unsupported compile-time context %q", root)}
		}
	}
	evaluator.truthy = githubTruthy
	evaluator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		return nil
	}
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		return githubCompare(kind, left, right)
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("unsupported compile-time expression") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("unsupported compile-time logical operator %s", kind)
	}
	evaluator.call = func(evaluator *semanticEvaluator, node *actionlint.FuncCallNode) (any, error) {
		value, recognized, err := evaluatePureFunction(evaluator, node)
		if recognized {
			return value, err
		}
		return nil, compileRuntimeDependencyError{fmt.Errorf("unsupported compile-time function %q", node.Callee)}
	}
	return evaluator.evaluate(node)
}

type compileRuntimeDependencyError struct {
	err error
}

func (e compileRuntimeDependencyError) Error() string { return e.err.Error() }

func resolveCompileReference(root string, path []string, context CompileContext) (any, error) {
	if strings.EqualFold(root, "github") && len(path) != 0 && strings.EqualFold(path[0], "token") {
		return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time expression references unavailable value %q", root+"."+strings.Join(path, "."))}
	}
	var (
		current   any
		available bool
	)
	switch {
	case strings.EqualFold(root, "github"):
		current, available = context.GitHub, context.GitHub != nil
	case strings.EqualFold(root, "event"):
		current, available = context.Event, context.Event != nil
	case strings.EqualFold(root, "vars"):
		current, available = context.Vars, context.Vars != nil
	case strings.EqualFold(root, "inputs"):
		current, available = context.Inputs, context.Inputs != nil
	case strings.EqualFold(root, "matrix"):
		current, available = context.Matrix, context.Matrix != nil
	case strings.EqualFold(root, "strategy"):
		current, available = context.Strategy, context.Strategy != nil
	default:
		return nil, compileRuntimeDependencyError{fmt.Errorf("unsupported compile-time context %q", root)}
	}
	legalMissing := strings.EqualFold(root, "event") || strings.EqualFold(root, "vars") || strings.EqualFold(root, "matrix") ||
		strings.EqualFold(root, "github") && len(path) != 0 && strings.EqualFold(path[0], "event")
	missing := false
	for _, part := range path {
		if missing {
			continue
		}
		var (
			ok  bool
			err error
		)
		current, ok, err = objectValue(current, part)
		if err != nil {
			return nil, err
		}
		if !ok {
			if available && legalMissing {
				current = nil
				missing = true
				continue
			}
			return nil, compileRuntimeDependencyError{fmt.Errorf("compile-time expression references unavailable value %q", root+"."+strings.Join(path, "."))}
		}
	}
	return current, nil
}

func resolveServicePort(services map[string]ServiceContext, service, port, kind string) (string, error) {
	for id, context := range services {
		if strings.EqualFold(id, service) {
			if value, ok := context.Ports[port]; ok {
				return value, nil
			}
			return "", fmt.Errorf("%s references unavailable service port %s.%s", kind, service, port)
		}
	}
	return "", fmt.Errorf("%s references unavailable service %q", kind, service)
}

func resolveServiceValue(services map[string]ServiceContext, service, field, kind string) (string, error) {
	for id, context := range services {
		if strings.EqualFold(id, service) {
			if strings.EqualFold(field, "id") {
				return context.ID, nil
			}
			return context.Network, nil
		}
	}
	return "", fmt.Errorf("%s references unavailable service %q", kind, service)
}
