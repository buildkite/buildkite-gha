// Package expression adapts actionlint's expression parser into owned values.
package expression

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rhysd/actionlint"
)

// Position is a one-based source position.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Span locates an expression in its source workflow.
type Span struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Expression retains expression text independently of actionlint's AST.
type Expression struct {
	Text string `json:"text"`
	Span Span   `json:"span"`
}

// Context contains the Phase 0 values available while evaluating a template.
type Context struct {
	Inputs       map[string]string
	Matrix       map[string]any
	Steps        map[string]map[string]string
	StepStatuses map[string]StepStatus
	Needs        map[string]map[string]string
	NeedResults  map[string]string
	Secrets      map[string]string
	Vars         map[string]string
	Env          map[string]string
	GitHub       map[string]any
	Services     map[string]map[string]string
}

// StepStatus contains the values exposed for one completed step while
// evaluating a condition.
type StepStatus struct {
	Outcome    string
	Conclusion string
	Outputs    map[string]string
}

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
	Services     map[string]map[string]string
	Failure      bool
	Cancelled    bool
	Unsuccessful bool
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

// CompileContext contains the non-secret values available while constructing
// a workflow graph. Values are snapshots supplied by the compiler; evaluation
// never reads the process environment or a secret provider.
type CompileContext struct {
	GitHub map[string]any
	Event  map[string]any
	Vars   map[string]string
	Matrix map[string]any
}

// Parse validates a complete ${{ ... }} expression using actionlint and returns
// an owned representation.
func Parse(text string, line, column int) (Expression, error) {
	body, err := expressionBody(text)
	if err != nil {
		return Expression{}, err
	}
	parser := actionlint.NewExprParser()
	if _, err := parser.Parse(actionlint.NewExprLexer(body + "}}")); err != nil {
		return Expression{}, fmt.Errorf("invalid expression: %w", err)
	}

	return Expression{
		Text: text,
		Span: Span{
			Start: Position{Line: line, Column: column},
			End:   endPosition(line, column, text),
		},
	}, nil
}

// ReferencePath extracts one complete static variable reference. Dot and
// literal string index access are accepted; functions, operators, literals,
// compound templates, and dynamic indexes fail closed.
func ReferencePath(text string) (string, []string, error) {
	body, err := expressionBody(text)
	if err != nil {
		return "", nil, err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return "", nil, fmt.Errorf("invalid expression: %w", parseErr)
	}
	return referencePath(node)
}

// EvaluateCompile evaluates one complete graph-time expression. The supported
// surface is intentionally limited to literals, github/event/vars/matrix
// references, and fromJSON applied to one of those values.
func EvaluateCompile(expr Expression, context CompileContext) (any, error) {
	body, err := expressionBody(expr.Text)
	if err != nil {
		return nil, err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return nil, fmt.Errorf("invalid expression: %w", parseErr)
	}
	return evaluateCompileNode(node, context)
}

// EvaluateCompileTemplate substitutes supported graph-time expressions once.
func EvaluateCompileTemplate(template string, context CompileContext) (string, error) {
	const open, close = "${{", "}}"
	var evaluated strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			evaluated.WriteString(remaining)
			return evaluated.String(), nil
		}
		evaluated.WriteString(remaining[:start])
		end := strings.Index(remaining[start+len(open):], close)
		if end < 0 {
			return "", fmt.Errorf("unterminated expression")
		}
		end += start + len(open)
		text := remaining[start : end+len(close)]
		value, err := EvaluateCompile(Expression{Text: text}, context)
		if err != nil {
			return "", err
		}
		switch value := value.(type) {
		case nil:
		case string:
			evaluated.WriteString(value)
		case bool, json.Number, float64, int:
			_, _ = fmt.Fprint(&evaluated, value)
		default:
			return "", fmt.Errorf("expression in runner label resolved to %T, want a scalar", value)
		}
		remaining = remaining[end+len(close):]
	}
}

// SecretReferences returns the statically named secrets referenced by a
// template. Dynamic indexes fail closed because the runtime cannot determine
// which values to resolve and register with the log redactor before execution.
func SecretReferences(template string) ([]string, error) {
	found := map[string]struct{}{}
	err := visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		var referenceErr error
		actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
			if !entering || referenceErr != nil {
				return
			}
			switch node.(type) {
			case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
			default:
				return
			}
			root, path, err := referencePath(node)
			if !strings.EqualFold(root, "secrets") {
				return
			}
			if err != nil {
				referenceErr = fmt.Errorf("secret reference: %w", err)
				return
			}
			if len(path) == 0 {
				switch parent := parent.(type) {
				case *actionlint.ObjectDerefNode:
					if parent.Receiver == node {
						return
					}
				case *actionlint.IndexAccessNode:
					if parent.Operand == node {
						return
					}
				}
				referenceErr = fmt.Errorf("secret reference must name exactly one secret")
				return
			}
			if len(path) != 1 {
				referenceErr = fmt.Errorf("secret reference must name exactly one secret")
				return
			}
			found[strings.ToUpper(path[0])] = struct{}{}
		})
		return referenceErr
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ConditionUsesContext reports whether a condition references a named context.
// Conditions may omit the normal ${{ ... }} delimiters.
func ConditionUsesContext(source, contextName string) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil {
		return false, err
	}
	if empty {
		return false, nil
	}
	found := false
	actionlint.VisitExprNode(node, func(node, _ actionlint.ExprNode, entering bool) {
		if !entering || found {
			return
		}
		switch node.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		default:
			return
		}
		root, _, _ := referencePath(node)
		found = strings.EqualFold(root, contextName)
	})
	return found, nil
}

// ValidateCondition verifies that a job or step condition uses only expression
// syntax, functions, and contexts implemented by the corresponding runtime
// phase. Runtime-dependent values are not evaluated.
func ValidateCondition(source string, scope ConditionScope) error {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return err
	}
	return validateConditionNode(node, scope)
}

func parseCondition(source string) (actionlint.ExprNode, bool, error) {
	condition := strings.TrimSpace(source)
	if condition == "" {
		return nil, true, nil
	}
	if strings.HasPrefix(condition, "${{") && strings.HasSuffix(condition, "}}") {
		condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	}
	node, err := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(condition + "}}"))
	if err != nil {
		return nil, false, fmt.Errorf("parse condition: %w", err)
	}
	return node, false, nil
}

func validateConditionNode(node actionlint.ExprNode, scope ConditionScope) error {
	switch node := node.(type) {
	case *actionlint.NullNode, *actionlint.BoolNode, *actionlint.IntNode, *actionlint.FloatNode, *actionlint.StringNode:
		return nil
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return err
		}
		return validateConditionReference(root, path, scope)
	case *actionlint.NotOpNode:
		return validateConditionNode(node.Operand, scope)
	case *actionlint.LogicalOpNode:
		if err := validateConditionNode(node.Left, scope); err != nil {
			return err
		}
		return validateConditionNode(node.Right, scope)
	case *actionlint.CompareOpNode:
		if !node.Kind.IsEqualityOp() {
			return fmt.Errorf("condition comparison %s is unsupported", node.Kind)
		}
		if err := validateConditionNode(node.Left, scope); err != nil {
			return err
		}
		return validateConditionNode(node.Right, scope)
	case *actionlint.FuncCallNode:
		switch strings.ToLower(node.Callee) {
		case "always", "success", "failure", "cancelled":
			if len(node.Args) != 0 {
				return fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
			}
			return nil
		default:
			return fmt.Errorf("condition function %q is unsupported", node.Callee)
		}
	default:
		return fmt.Errorf("unsupported condition expression")
	}
}

func validateConditionReference(root string, path []string, scope ConditionScope) error {
	reference := root
	if len(path) != 0 {
		reference += "." + strings.Join(path, ".")
	}
	switch strings.ToLower(root) {
	case "github":
		if len(path) == 1 {
			switch strings.ToLower(path[0]) {
			case "actor", "event_name", "ref", "repository", "sha":
				return nil
			}
		}
		return fmt.Errorf("condition reference %q is unavailable at runtime; supported github properties are actor, event_name, ref, repository, and sha", reference)
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

func visitTemplateExpressions(template string, visit func(actionlint.ExprNode) error) error {
	const open = "${{"
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			return nil
		}
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			return fmt.Errorf("invalid expression: %w", lexErr)
		}
		node, err := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(source[:consumed]))
		if err != nil {
			return fmt.Errorf("invalid expression: %w", err)
		}
		if err := visit(node); err != nil {
			return err
		}
		remaining = source[consumed:]
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
	value, err := evaluateConditionNode(node, context)
	if err != nil {
		return false, err
	}
	result := conditionTruthy(value)
	if !containsStatusFunction(node) && (context.Unsuccessful || context.Cancelled) {
		return false, nil
	}
	return result, nil
}

func evaluateConditionNode(node actionlint.ExprNode, context ConditionContext) (any, error) {
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
	case *actionlint.NotOpNode:
		value, err := evaluateConditionNode(node.Operand, context)
		if err != nil {
			return nil, err
		}
		return !conditionTruthy(value), nil
	case *actionlint.LogicalOpNode:
		left, err := evaluateConditionNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		leftBool := conditionTruthy(left)
		if node.Kind == actionlint.LogicalOpNodeKindAnd && !leftBool {
			return false, nil
		}
		if node.Kind == actionlint.LogicalOpNodeKindOr && leftBool {
			return true, nil
		}
		right, err := evaluateConditionNode(node.Right, context)
		if err != nil {
			return nil, err
		}
		return conditionTruthy(right), nil
	case *actionlint.CompareOpNode:
		left, err := evaluateConditionNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		right, err := evaluateConditionNode(node.Right, context)
		if err != nil {
			return nil, err
		}
		equal, err := conditionEqual(left, right)
		if err != nil {
			return nil, err
		}
		switch node.Kind {
		case actionlint.CompareOpNodeKindEq:
			return equal, nil
		case actionlint.CompareOpNodeKindNotEq:
			return !equal, nil
		default:
			return nil, fmt.Errorf("condition comparison %s is unsupported", node.Kind)
		}
	case *actionlint.FuncCallNode:
		if len(node.Args) != 0 {
			return nil, fmt.Errorf("condition function %q arguments are unsupported", node.Callee)
		}
		switch strings.ToLower(node.Callee) {
		case "always":
			return true, nil
		case "success":
			return !context.Unsuccessful && !context.Cancelled, nil
		case "failure":
			return context.Failure, nil
		case "cancelled":
			return context.Cancelled, nil
		default:
			return nil, fmt.Errorf("condition function %q is unsupported", node.Callee)
		}
	default:
		return nil, fmt.Errorf("unsupported condition expression")
	}
}

func resolveConditionReference(root string, path []string, context ConditionContext) (any, error) {
	switch {
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

func conditionTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case int:
		return value != 0
	case float64:
		return value != 0
	case string:
		return value != ""
	default:
		return false
	}
}

func conditionEqual(left, right any) (bool, error) {
	switch left := left.(type) {
	case nil:
		return right == nil, nil
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
	case int:
		switch right := right.(type) {
		case int:
			return left == right, nil
		case float64:
			return float64(left) == right, nil
		}
	case float64:
		switch right := right.(type) {
		case int:
			return left == float64(right), nil
		case float64:
			return left == right, nil
		}
	}
	return false, fmt.Errorf("mixed-type condition equality is unsupported")
}

func containsStatusFunction(node actionlint.ExprNode) bool {
	found := false
	actionlint.VisitExprNode(node, func(node, _ actionlint.ExprNode, entering bool) {
		if !entering {
			return
		}
		call, ok := node.(*actionlint.FuncCallNode)
		if ok {
			switch strings.ToLower(call.Callee) {
			case "always", "success", "failure", "cancelled":
				found = true
			}
		}
	})
	return found
}

func expressionBody(text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "${{") || !strings.HasSuffix(trimmed, "}}") {
		return "", fmt.Errorf("expected a complete ${{ ... }} expression")
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "${{"), "}}"))
	if strings.Contains(body, "}}") {
		return "", fmt.Errorf("expression contains an embedded closing delimiter")
	}
	return body, nil
}

func evaluateCompileNode(node actionlint.ExprNode, context CompileContext) (any, error) {
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
		return resolveCompileReference(root, path, context)
	case *actionlint.FuncCallNode:
		if !strings.EqualFold(node.Callee, "fromJSON") || len(node.Args) != 1 {
			return nil, fmt.Errorf("unsupported compile-time function %q", node.Callee)
		}
		value, err := evaluateCompileNode(node.Args[0], context)
		if err != nil {
			return nil, err
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("fromJSON argument resolved to %T, want string", value)
		}
		return decodeJSONValue(text)
	default:
		return nil, fmt.Errorf("unsupported compile-time expression")
	}
}

func referencePath(node actionlint.ExprNode) (string, []string, error) {
	switch node := node.(type) {
	case *actionlint.VariableNode:
		return node.Name, nil, nil
	case *actionlint.ObjectDerefNode:
		root, path, err := referencePath(node.Receiver)
		return root, append(path, node.Property), err
	case *actionlint.IndexAccessNode:
		root, path, err := referencePath(node.Operand)
		if err != nil {
			return "", nil, err
		}
		switch index := node.Index.(type) {
		case *actionlint.StringNode:
			return root, append(path, index.Value), nil
		case *actionlint.IntNode:
			if !strings.EqualFold(root, "job") || len(path) != 3 || !strings.EqualFold(path[0], "services") || !strings.EqualFold(path[2], "ports") {
				return root, nil, fmt.Errorf("expression variable %q does not support numeric indices", root)
			}
			return root, append(path, fmt.Sprint(index.Value)), nil
		default:
			return root, nil, fmt.Errorf("expression index must be a string literal or integer literal")
		}
	default:
		return "", nil, fmt.Errorf("unsupported expression reference")
	}
}

func resolveCompileReference(root string, path []string, context CompileContext) (any, error) {
	var current any
	switch {
	case strings.EqualFold(root, "github"):
		current = context.GitHub
	case strings.EqualFold(root, "event"):
		current = context.Event
	case strings.EqualFold(root, "vars"):
		current = context.Vars
	case strings.EqualFold(root, "matrix"):
		current = context.Matrix
	default:
		return nil, fmt.Errorf("unsupported compile-time context %q", root)
	}
	for _, part := range path {
		var (
			ok  bool
			err error
		)
		current, ok, err = objectValue(current, part)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("compile-time expression references unavailable value %q", root+"."+strings.Join(path, "."))
		}
	}
	return current, nil
}

func resolveServicePort(services map[string]map[string]string, service, port, kind string) (string, error) {
	for id, ports := range services {
		if strings.EqualFold(id, service) {
			if value, ok := ports[port]; ok {
				return value, nil
			}
			return "", fmt.Errorf("%s references unavailable service port %s.%s", kind, service, port)
		}
	}
	return "", fmt.Errorf("%s references unavailable service %q", kind, service)
}

func objectValue(value any, name string) (any, bool, error) {
	switch value := value.(type) {
	case map[string]any:
		var (
			found      any
			matchedKey string
		)
		for candidate, item := range value {
			if strings.EqualFold(candidate, name) {
				if matchedKey != "" {
					return nil, false, fmt.Errorf("compile-time object contains ambiguous properties %q and %q", matchedKey, candidate)
				}
				found, matchedKey = item, candidate
			}
		}
		if matchedKey != "" {
			return found, true, nil
		}
	case map[string]string:
		var (
			found      string
			matchedKey string
		)
		for candidate, item := range value {
			if strings.EqualFold(candidate, name) {
				if matchedKey != "" {
					return nil, false, fmt.Errorf("compile-time object contains ambiguous properties %q and %q", matchedKey, candidate)
				}
				found, matchedKey = item, candidate
			}
		}
		if matchedKey != "" {
			return found, true, nil
		}
	}
	return nil, false, nil
}

func decodeJSONValue(source string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("fromJSON argument is invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("fromJSON argument contains multiple JSON values")
	}
	return value, nil
}

// Evaluate substitutes the supported Phase 0 expressions in a template once.
func Evaluate(template string, context Context) (string, error) {
	const open, close = "${{", "}}"
	var evaluated strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			evaluated.WriteString(remaining)
			return evaluated.String(), nil
		}
		evaluated.WriteString(remaining[:start])
		end := strings.Index(remaining[start+len(open):], close)
		if end < 0 {
			return "", fmt.Errorf("unterminated expression in %q", template)
		}
		end += start + len(open)
		replacement, err := evaluateReference(strings.TrimSpace(remaining[start+len(open):end]), context)
		if err != nil {
			return "", err
		}
		evaluated.WriteString(replacement)
		remaining = remaining[end+len(close):]
	}
}

func evaluateReference(reference string, context Context) (string, error) {
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(reference + "}}"))
	if parseErr != nil {
		return "", fmt.Errorf("invalid expression: %w", parseErr)
	}
	root, path, err := referencePath(node)
	if err != nil {
		return "", err
	}
	switch {
	case len(path) == 4 && strings.EqualFold(root, "job") && strings.EqualFold(path[0], "services") && strings.EqualFold(path[2], "ports"):
		return resolveServicePort(context.Services, path[1], path[3], "expression")
	case len(path) >= 1 && strings.EqualFold(root, "github"):
		value, ok := lookupRuntimeValue(context.GitHub, path)
		if !ok {
			return "", fmt.Errorf("expression references unavailable github value %q", strings.Join(path, "."))
		}
		return fmt.Sprint(value), nil
	case len(path) == 1 && strings.EqualFold(root, "inputs"):
		return findString(context.Inputs, path[0]), nil
	case len(path) == 1 && strings.EqualFold(root, "matrix"):
		for name, value := range context.Matrix {
			if strings.EqualFold(name, path[0]) {
				return fmt.Sprint(value), nil
			}
		}
		return "", fmt.Errorf("expression references unavailable matrix value %q", path[0])
	case len(path) == 1 && strings.EqualFold(root, "secrets"):
		return findString(context.Secrets, path[0]), nil
	case len(path) == 1 && strings.EqualFold(root, "vars"):
		return findString(context.Vars, path[0]), nil
	case len(path) == 1 && strings.EqualFold(root, "env"):
		return findString(context.Env, path[0]), nil
	case len(path) == 3 && strings.EqualFold(root, "steps") && strings.EqualFold(path[1], "outputs"):
		outputs, ok := findOutputs(context.Steps, path[0])
		if !ok {
			return "", fmt.Errorf("expression references unavailable step %q", path[0])
		}
		return findString(outputs, path[2]), nil
	case len(path) == 2 && strings.EqualFold(root, "steps"):
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
	case len(path) == 3 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "outputs"):
		outputs, ok := findOutputs(context.Needs, path[0])
		if !ok {
			return "", fmt.Errorf("expression references unavailable need %q", path[0])
		}
		return findString(outputs, path[2]), nil
	case len(path) == 2 && strings.EqualFold(root, "needs") && strings.EqualFold(path[1], "result"):
		for candidate, result := range context.NeedResults {
			if strings.EqualFold(candidate, path[0]) {
				return result, nil
			}
		}
		return "", fmt.Errorf("expression references unavailable need %q", path[0])
	default:
		return "", fmt.Errorf("unsupported expression %q", reference)
	}
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
	for candidate, value := range values {
		if strings.EqualFold(candidate, name) {
			return value
		}
	}
	return ""
}

func findOutputs(values map[string]map[string]string, name string) (map[string]string, bool) {
	for candidate, outputs := range values {
		if strings.EqualFold(candidate, name) {
			return outputs, true
		}
	}
	return nil, false
}

func endPosition(line, column int, text string) Position {
	end := Position{Line: line, Column: column}
	for _, r := range text {
		if r == '\n' {
			end.Line++
			end.Column = 1
		} else {
			end.Column++
		}
	}
	return end
}
