// Package expression adapts actionlint's expression parser into owned values.
package expression

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"sort"
	"strconv"
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
	JobStatus    string
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

// ValidateRuntimeTemplate verifies that every expression in a runtime template
// is one direct reference supported by Evaluate. Runtime values are
// deliberately not resolved because many contexts do not exist until a job or
// step runs.
func ValidateRuntimeTemplate(template string) error {
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

// ValidateActionInputDefault verifies the restricted compound expression
// surface supported only while evaluating action metadata input defaults.
func ValidateActionInputDefault(template string) error {
	referencesJobStatus, err := ReferencesJobStatus(template)
	if err != nil {
		return err
	}
	if referencesJobStatus {
		root, path, err := ReferencePath(template)
		if err != nil || !isJobStatusReference(root, path) {
			return fmt.Errorf("action input default job.status must be one direct expression")
		}
	}
	return visitTemplateExpressions(template, validateActionInputDefaultNode)
}

func validateActionInputDefaultNode(node actionlint.ExprNode) error {
	switch node := node.(type) {
	case *actionlint.NullNode, *actionlint.BoolNode, *actionlint.IntNode, *actionlint.FloatNode, *actionlint.StringNode:
		return nil
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return fmt.Errorf("runtime interpolation requires a direct context reference: %w", err)
		}
		if isJobStatusReference(root, path) {
			return nil
		}
		if classifyRuntimeReference(root, path) == runtimeReferenceUnsupported {
			return fmt.Errorf("unsupported runtime expression %q", referenceName(root, path))
		}
		return nil
	case *actionlint.LogicalOpNode:
		if err := validateActionInputDefaultNode(node.Left); err != nil {
			return err
		}
		return validateActionInputDefaultNode(node.Right)
	case *actionlint.CompareOpNode:
		if !node.Kind.IsEqualityOp() {
			return fmt.Errorf("action input default comparison %s is unsupported", node.Kind)
		}
		if err := validateActionInputDefaultNode(node.Left); err != nil {
			return err
		}
		return validateActionInputDefaultNode(node.Right)
	case *actionlint.FuncCallNode:
		if !strings.EqualFold(node.Callee, "toJSON") || len(node.Args) != 1 {
			return fmt.Errorf("action input default function %q is unsupported", node.Callee)
		}
		root, path, err := referencePath(node.Args[0])
		if err != nil || !strings.EqualFold(root, "matrix") || len(path) != 0 {
			return fmt.Errorf("action input default toJSON requires the complete matrix context")
		}
		return nil
	default:
		return fmt.Errorf("action input default expression is unsupported")
	}
}

// EvaluateCompile evaluates one complete graph-time expression. The supported
// surface is intentionally limited to literals, github/event/vars/matrix
// references, boolean/equality operators, and selected pure functions.
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
	return actionInputDefaultTruthy(value), nil
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
			return "", fmt.Errorf("template expression resolved to %T, want a scalar", value)
		}
		remaining = remaining[end+len(close):]
	}
}

// SubstituteCompileInputs replaces static inputs.<name> references inside
// expression regions with equivalent GitHub expression literals. Text and
// string literals outside those references are preserved byte-for-byte.
func SubstituteCompileInputs(template string, inputs map[string]any) (string, error) {
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
		for i := 0; i+2 < len(tokens); i++ {
			if tokens[i].Kind != actionlint.TokenKindIdent || !strings.EqualFold(tokens[i].Value, "inputs") || tokens[i+1].Kind != actionlint.TokenKindDot || tokens[i+2].Kind != actionlint.TokenKindIdent {
				continue
			}
			value, ok := findCompileInput(inputs, tokens[i+2].Value)
			if !ok {
				continue
			}
			literal, err := compileInputLiteral(value)
			if err != nil {
				return "", err
			}
			replacements = append(replacements, replacement{
				start: tokens[i].Offset,
				end:   tokens[i+2].Offset + len(tokens[i+2].Value),
				value: literal,
			})
			i += 2
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
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("compile-time input %T cannot be represented as an expression literal", value)
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

// ReferencesGitHubToken reports whether a template statically references
// github.token. Dynamic GitHub indexes fail closed so compiler-owned token
// authority cannot depend on a runtime-selected property.
func ReferencesGitHubToken(template string) (bool, error) {
	found := false
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
			if !strings.EqualFold(root, "github") {
				return
			}
			if err != nil {
				referenceErr = fmt.Errorf("github reference: %w", err)
				return
			}
			if len(path) == 0 || !strings.EqualFold(path[0], "token") {
				return
			}
			if len(path) != 1 {
				referenceErr = fmt.Errorf("github.token reference must name exactly github.token")
				return
			}
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
			found = true
		})
		return referenceErr
	})
	return found, err
}

// ReferencesJobStatus reports whether a template statically references
// job.status.
func ReferencesJobStatus(template string) (bool, error) {
	found := false
	err := visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		actionlint.VisitExprNode(expression, func(node, _ actionlint.ExprNode, entering bool) {
			if !entering || found {
				return
			}
			switch node.(type) {
			case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
			default:
				return
			}
			root, path, err := referencePath(node)
			if err == nil && isJobStatusReference(root, path) {
				found = true
			}
		})
		return nil
	})
	return found, err
}

// ReferencesGitHubEvent reports whether a condition reads the event payload
// through github.event. The compiler can fold those conditions before the
// immutable runtime boundary, which deliberately omits the payload body.
func ReferencesGitHubEvent(source string) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return false, err
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
		root, path, pathErr := referencePath(node)
		found = pathErr == nil && strings.EqualFold(root, "github") && len(path) != 0 && strings.EqualFold(path[0], "event")
	})
	return found, nil
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
	return validateCondition(source, scope, nil, false)
}

// ValidateConditionWithMatrix additionally verifies references and operand
// types against one concrete, statically expanded matrix instance.
func ValidateConditionWithMatrix(source string, scope ConditionScope, matrix map[string]any) error {
	return validateCondition(source, scope, matrix, true)
}

func validateCondition(source string, scope ConditionScope, matrix map[string]any, matrixKnown bool) error {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return err
	}
	return validateConditionNode(node, scope, matrix, matrixKnown)
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

func validateConditionNode(node actionlint.ExprNode, scope ConditionScope, matrix map[string]any, matrixKnown bool) error {
	switch node := node.(type) {
	case *actionlint.NullNode, *actionlint.BoolNode, *actionlint.IntNode, *actionlint.FloatNode, *actionlint.StringNode:
		return nil
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		root, path, err := referencePath(node)
		if err != nil {
			return err
		}
		if matrixKnown && strings.EqualFold(root, "matrix") && len(path) == 1 {
			if _, ok := conditionMatrixValue(matrix, path[0]); !ok {
				return fmt.Errorf("condition reference %q is unavailable in this matrix instance", root+"."+path[0])
			}
		}
		return validateConditionReference(root, path, scope)
	case *actionlint.NotOpNode:
		return validateConditionNode(node.Operand, scope, matrix, matrixKnown)
	case *actionlint.LogicalOpNode:
		if err := validateConditionNode(node.Left, scope, matrix, matrixKnown); err != nil {
			return err
		}
		return validateConditionNode(node.Right, scope, matrix, matrixKnown)
	case *actionlint.CompareOpNode:
		if !node.Kind.IsEqualityOp() {
			return fmt.Errorf("condition comparison %s is unsupported", node.Kind)
		}
		if err := validateConditionNode(node.Left, scope, matrix, matrixKnown); err != nil {
			return err
		}
		if err := validateConditionNode(node.Right, scope, matrix, matrixKnown); err != nil {
			return err
		}
		left, right := conditionOperandCategory(node.Left, matrix), conditionOperandCategory(node.Right, matrix)
		if left == "unsupported" || right == "unsupported" {
			return fmt.Errorf("condition equality uses an unsupported matrix value type")
		}
		if left != "unknown" && right != "unknown" && left != right {
			return fmt.Errorf("condition equality compares incompatible %s and %s operands", left, right)
		}
		return nil
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

func conditionOperandCategory(node actionlint.ExprNode, matrix map[string]any) string {
	switch node := node.(type) {
	case *actionlint.NullNode:
		return "null"
	case *actionlint.BoolNode, *actionlint.NotOpNode, *actionlint.LogicalOpNode, *actionlint.CompareOpNode, *actionlint.FuncCallNode:
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

func conditionNumber(value any) (*big.Rat, bool) {
	var source string
	switch value := value.(type) {
	case json.Number:
		source = value.String()
	default:
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			source = strconv.FormatInt(reflected.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			source = strconv.FormatUint(reflected.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			source = strconv.FormatFloat(reflected.Float(), 'g', -1, reflected.Type().Bits())
		default:
			return nil, false
		}
	}
	number, ok := new(big.Rat).SetString(source)
	return number, ok
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
	case *actionlint.NotOpNode:
		value, err := evaluateCompileNode(node.Operand, context)
		if err != nil {
			return nil, err
		}
		return !actionInputDefaultTruthy(value), nil
	case *actionlint.LogicalOpNode:
		left, err := evaluateCompileNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		switch node.Kind {
		case actionlint.LogicalOpNodeKindAnd:
			if !actionInputDefaultTruthy(left) {
				return left, nil
			}
			return evaluateCompileNode(node.Right, context)
		case actionlint.LogicalOpNodeKindOr:
			if actionInputDefaultTruthy(left) {
				return left, nil
			}
			return evaluateCompileNode(node.Right, context)
		default:
			return nil, fmt.Errorf("unsupported compile-time logical operator %s", node.Kind)
		}
	case *actionlint.CompareOpNode:
		if !node.Kind.IsEqualityOp() {
			return nil, fmt.Errorf("unsupported compile-time comparison %s", node.Kind)
		}
		left, err := evaluateCompileNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		right, err := evaluateCompileNode(node.Right, context)
		if err != nil {
			return nil, err
		}
		equal := actionInputDefaultEqual(left, right)
		if node.Kind == actionlint.CompareOpNodeKindNotEq {
			return !equal, nil
		}
		return equal, nil
	case *actionlint.FuncCallNode:
		switch {
		case strings.EqualFold(node.Callee, "fromJSON") && len(node.Args) == 1:
			value, err := evaluateCompileNode(node.Args[0], context)
			if err != nil {
				return nil, err
			}
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("fromJSON argument resolved to %T, want string", value)
			}
			return decodeJSONValue(text)
		case strings.EqualFold(node.Callee, "startsWith") && len(node.Args) == 2:
			value, err := evaluateCompileNode(node.Args[0], context)
			if err != nil {
				return nil, err
			}
			prefix, err := evaluateCompileNode(node.Args[1], context)
			if err != nil {
				return nil, err
			}
			valueText, valueOK := value.(string)
			prefixText, prefixOK := prefix.(string)
			if !valueOK || !prefixOK {
				return nil, fmt.Errorf("startsWith arguments resolved to %T and %T, want strings", value, prefix)
			}
			return strings.HasPrefix(strings.ToLower(valueText), strings.ToLower(prefixText)), nil
		default:
			return nil, fmt.Errorf("unsupported compile-time function %q", node.Callee)
		}
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

// Evaluate substitutes direct runtime references in a template once.
func Evaluate(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateDirectRuntimeNode)
}

// EvaluateActionInputDefault substitutes the restricted compound expressions
// supported only in action metadata input defaults.
func EvaluateActionInputDefault(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateActionInputDefaultNode)
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

func evaluateActionInputDefaultNode(node actionlint.ExprNode, context Context) (any, error) {
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
		if isJobStatusReference(root, path) {
			if context.JobStatus == "" {
				return nil, fmt.Errorf("expression references unavailable job.status")
			}
			return context.JobStatus, nil
		}
		return resolveRuntimeReference(root, path, context)
	case *actionlint.LogicalOpNode:
		left, err := evaluateActionInputDefaultNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		switch node.Kind {
		case actionlint.LogicalOpNodeKindAnd:
			if !actionInputDefaultTruthy(left) {
				return left, nil
			}
			return evaluateActionInputDefaultNode(node.Right, context)
		case actionlint.LogicalOpNodeKindOr:
			if actionInputDefaultTruthy(left) {
				return left, nil
			}
			return evaluateActionInputDefaultNode(node.Right, context)
		default:
			return nil, fmt.Errorf("action input default logical operator %s is unsupported", node.Kind)
		}
	case *actionlint.CompareOpNode:
		left, err := evaluateActionInputDefaultNode(node.Left, context)
		if err != nil {
			return nil, err
		}
		right, err := evaluateActionInputDefaultNode(node.Right, context)
		if err != nil {
			return nil, err
		}
		equal := actionInputDefaultEqual(left, right)
		switch node.Kind {
		case actionlint.CompareOpNodeKindEq:
			return equal, nil
		case actionlint.CompareOpNodeKindNotEq:
			return !equal, nil
		default:
			return nil, fmt.Errorf("action input default comparison %s is unsupported", node.Kind)
		}
	case *actionlint.FuncCallNode:
		if !strings.EqualFold(node.Callee, "toJSON") || len(node.Args) != 1 {
			return nil, fmt.Errorf("action input default function %q is unsupported", node.Callee)
		}
		root, path, err := referencePath(node.Args[0])
		if err != nil || !strings.EqualFold(root, "matrix") || len(path) != 0 {
			return nil, fmt.Errorf("action input default toJSON requires the complete matrix context")
		}
		value, err := json.MarshalIndent(context.Matrix, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode action input default matrix as JSON: %w", err)
		}
		return string(value), nil
	default:
		return nil, fmt.Errorf("action input default expression is unsupported")
	}
}

func actionInputDefaultTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	}
	if number, ok := conditionNumber(value); ok {
		return number.Sign() != 0
	}
	return true
}

func actionInputDefaultEqual(left, right any) bool {
	if left == nil && right == nil {
		return true
	}
	if leftNumber, leftOK := conditionNumber(left); leftOK {
		if rightNumber, rightOK := conditionNumber(right); rightOK {
			return leftNumber.Cmp(rightNumber) == 0
		}
	}
	switch left := left.(type) {
	case string:
		if right, ok := right.(string); ok {
			return strings.EqualFold(left, right)
		}
	case bool:
		if right, ok := right.(bool); ok {
			return left == right
		}
	}
	if left != nil && right != nil && reflect.TypeOf(left) == reflect.TypeOf(right) {
		leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
		switch leftValue.Kind() {
		case reflect.Map, reflect.Pointer, reflect.Slice:
			return leftValue.Pointer() == rightValue.Pointer()
		}
	}
	leftNumber, leftOK := actionInputDefaultNumber(left)
	rightNumber, rightOK := actionInputDefaultNumber(right)
	return leftOK && rightOK && leftNumber.Cmp(rightNumber) == 0
}

func actionInputDefaultNumber(value any) (*big.Rat, bool) {
	switch value := value.(type) {
	case nil:
		return new(big.Rat), true
	case bool:
		if value {
			return big.NewRat(1, 1), true
		}
		return new(big.Rat), true
	case string:
		if strings.TrimSpace(value) == "" {
			return new(big.Rat), true
		}
		decoded, err := decodeJSONValue(value)
		if err != nil {
			return nil, false
		}
		number, ok := decoded.(json.Number)
		if !ok {
			return nil, false
		}
		return conditionNumber(number)
	default:
		return conditionNumber(value)
	}
}

func resolveRuntimeReference(root string, path []string, context Context) (any, error) {
	switch classifyRuntimeReference(root, path) {
	case runtimeReferenceServicePort:
		return resolveServicePort(context.Services, path[1], path[3], "expression")
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

func isJobStatusReference(root string, path []string) bool {
	return len(path) == 1 && strings.EqualFold(root, "job") && strings.EqualFold(path[0], "status")
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
