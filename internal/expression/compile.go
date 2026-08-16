// Compile-time expression evaluation: graph-construction substitution and
// evaluation against the snapshotted CompileContext.
package expression

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

// EvaluateCompile evaluates one complete graph-time expression. The supported
// surface is intentionally limited to literals, github/event/vars/matrix
// references, boolean/equality operators, and selected pure functions.
func EvaluateCompile(expr Expression, context CompileContext) (any, error) {
	node, err := parseCompileExpression(expr)
	if err != nil {
		return nil, err
	}
	if err := validateCompileExpressionNode(node); err != nil {
		return nil, err
	}
	return evaluateCompileNode(node, context)
}

// EvaluateCompileAvailable evaluates an expression until it either produces a
// value, needs a runtime-only value, or encounters a deterministic error.
func EvaluateCompileAvailable(expr Expression, context CompileContext) (any, bool, error) {
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

// ValidateReusableInputDefault validates an expression-valued workflow_call
// input default without resolving its values.
func ValidateReusableInputDefault(template string) error {
	return visitTemplateExpressions(template, func(node actionlint.ExprNode) error {
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
	})
}

// EvaluateReusableInputDefault evaluates a statically available workflow_call
// input default. A complete expression preserves its type; a template renders
// to a string.
func EvaluateReusableInputDefault(template string, context CompileContext) (any, error) {
	if err := ValidateReusableInputDefault(template); err != nil {
		return nil, err
	}
	if expression, err := Parse(template, 1, 1); err == nil {
		return EvaluateCompile(expression, context)
	}
	return EvaluateCompileTemplate(template, context)
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
			case "actor", "event_name", "head_ref", "ref", "repository", "repository_owner", "sha", "workflow":
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

// EvaluateCompileCondition evaluates a condition whose entire value is known
// while constructing the graph. Callers may fall back to runtime condition
// handling when this returns an unavailable-context or unsupported error.
func EvaluateCompileCondition(source string, context CompileContext) (bool, error) {
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

// ReduceCompileCondition replaces every compile-time scalar subtree in a
// condition while preserving runtime-dependent subtrees for later evaluation.
func ReduceCompileCondition(source string, context CompileContext) (string, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return source, err
	}
	return reduceCompileNode(node, context), nil
}

func reduceCompileNode(node actionlint.ExprNode, context CompileContext) string {
	if value, err := evaluateCompileNode(node, context); err == nil {
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

func compileScalarLiteral(value any) (string, bool) {
	literal, err := compileInputLiteral(value)
	return literal, err == nil
}

// EvaluateCompileTemplate substitutes supported graph-time expressions once.
func EvaluateCompileTemplate(template string, context CompileContext) (string, error) {
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
			return "", fmt.Errorf("invalid expression: %w", lexErr)
		}
		text := open + source[:consumed]
		value, err := EvaluateCompile(Expression{Text: text}, context)
		if err != nil {
			return "", err
		}
		replacement, scalar := expressionString(value)
		if !scalar {
			return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
		}
		evaluated.WriteString(replacement)
		remaining = source[consumed:]
	}
}

// EvaluateAvailableCompileTemplate folds each graph-time expression that can be
// resolved independently and preserves expressions that need runtime context.
func EvaluateAvailableCompileTemplate(template string, context CompileContext) (string, error) {
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
			return "", fmt.Errorf("invalid expression: %w", lexErr)
		}
		complete := open + source[:consumed]
		value, err := EvaluateCompile(Expression{Text: complete}, context)
		if err != nil {
			evaluated.WriteString(complete)
		} else {
			replacement, scalar := expressionString(value)
			if !scalar {
				return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
			}
			if introducesExpressionSyntax(evaluated.String(), replacement, source[consumed:]) {
				return "", fmt.Errorf("compile-time expression result contains expression syntax")
			}
			evaluated.WriteString(replacement)
		}
		remaining = source[consumed:]
	}
}

func introducesExpressionSyntax(before, replacement, after string) bool {
	if len(before) > 2 {
		before = before[len(before)-2:]
	}
	if len(after) > 2 {
		after = after[:2]
	}
	boundary, replacementEnd := len(before), len(before)+len(replacement)
	combined := before + replacement + after
	for offset := 0; offset < len(combined); {
		relative := strings.Index(combined[offset:], "${{")
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len("${{")
		if len(replacement) > 0 && start < replacementEnd && end > boundary || len(replacement) == 0 && start < boundary && end > boundary {
			return true
		}
		offset = start + 1
	}
	return false
}

// SubstituteCompileInputs replaces static inputs.<name> and inputs['name']
// references inside expression regions with equivalent GitHub expression
// literals. Text and string literals outside those references are preserved
// byte-for-byte.
func SubstituteCompileInputs(template string, inputs map[string]any) (string, error) {
	resolved := template
	for {
		next, err := substituteCompileInputsOnce(resolved, inputs)
		if err != nil {
			return "", err
		}
		if next == resolved {
			return next, nil
		}
		resolved = next
	}
}

func substituteCompileInputsOnce(template string, inputs map[string]any) (string, error) {
	const open = "${{"
	var substituted strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			substituted.WriteString(remaining)
			return substituted.String(), nil
		}
		substituted.WriteString(remaining[:start+len(open)])
		source := remaining[start+len(open):]
		tokens, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			return "", fmt.Errorf("invalid expression: %w", lexErr)
		}
		expressionSource := source[:consumed]
		type replacement struct {
			start, end int
			value      string
		}
		var replacements []replacement
		for i := 0; i < len(tokens); i++ {
			if tokens[i].Kind != actionlint.TokenKindIdent || !strings.EqualFold(tokens[i].Value, "inputs") {
				continue
			}
			// A preceding '.' means this is a property named "inputs" on
			// another receiver, such as github.event.inputs.<name>, not the
			// workflow-call inputs context.
			if i > 0 && tokens[i-1].Kind == actionlint.TokenKindDot {
				continue
			}
			var name string
			var end, consumedTokens int
			switch {
			case i+2 < len(tokens) && tokens[i+1].Kind == actionlint.TokenKindDot && tokens[i+2].Kind == actionlint.TokenKindIdent:
				name = tokens[i+2].Value
				end = tokens[i+2].Offset + len(tokens[i+2].Value)
				consumedTokens = 2
			case i+3 < len(tokens) && tokens[i+1].Kind == actionlint.TokenKindLeftBracket && tokens[i+2].Kind == actionlint.TokenKindString && tokens[i+3].Kind == actionlint.TokenKindRightBracket:
				name = strings.ReplaceAll(strings.Trim(tokens[i+2].Value, "'"), "''", "'")
				end = tokens[i+3].Offset + 1
				consumedTokens = 3
			default:
				continue
			}
			value, ok := findCompileInput(inputs, name)
			if !ok {
				continue
			}
			literal, err := compileInputLiteral(value)
			if err != nil {
				return "", err
			}
			replacements = append(replacements, replacement{
				start: tokens[i].Offset,
				end:   end,
				value: literal,
			})
			i += consumedTokens
		}
		for i := len(replacements) - 1; i >= 0; i-- {
			replacement := replacements[i]
			expressionSource = expressionSource[:replacement.start] + replacement.value + expressionSource[replacement.end:]
		}
		substituted.WriteString(expressionSource)
		remaining = source[consumed:]
	}
}

func findCompileInput(inputs map[string]any, target string) (any, bool) {
	for name, value := range inputs {
		if strings.EqualFold(name, target) {
			return value, true
		}
	}
	return nil, false
}

func compileInputLiteral(value any) (string, error) {
	switch value := value.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(value), nil
	case string:
		return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
	case json.Number:
		return value.String(), nil
	case int:
		return strconv.Itoa(value), nil
	case float64:
		return expressionNumberString(value), nil
	default:
		return "", fmt.Errorf("compile-time input %T cannot be represented as an expression literal", value)
	}
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
		if strings.EqualFold(node.Callee, "hashFiles") {
			return nil, compileRuntimeDependencyError{fmt.Errorf("unsupported compile-time function %q", node.Callee)}
		}
		return nil, fmt.Errorf("unsupported compile-time function %q", node.Callee)
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
