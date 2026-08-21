// Static reference introspection over templates, complete expressions, and
// conditions. Each predicate deliberately answers one narrow question with
// an error for unsupported input.
package expression

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rhysd/actionlint"
)

// NeedOutputReference is one statically identified direct job output.
type NeedOutputReference struct {
	Job    string
	Output string
}

// ReferencePath extracts one complete static variable reference. Dot and
// literal string index access are accepted; functions, operators, literals,
// compound templates, and dynamic indexes return an error.
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

// RuntimeMatrixOutput accepts only the complete runtime matrix expression
// fromJSON(needs.<job>.outputs.<name>). Dot access is required so the source
// cannot hide dynamic indexes or compose the producer output with other data.
func RuntimeMatrixOutput(expr Expression) (NeedOutputReference, error) {
	body, err := expressionBody(expr.Text)
	if err != nil {
		return NeedOutputReference{}, err
	}
	if strings.ContainsAny(body, "[]") {
		return NeedOutputReference{}, fmt.Errorf("runtime matrix expression requires dot access")
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return NeedOutputReference{}, fmt.Errorf("invalid expression: %w", parseErr)
	}
	call, ok := node.(*actionlint.FuncCallNode)
	if !ok || !strings.EqualFold(call.Callee, "fromJSON") || len(call.Args) != 1 {
		return NeedOutputReference{}, fmt.Errorf("runtime matrix expression must be exactly fromJSON(needs.<job>.outputs.<name>)")
	}
	root, path, err := referencePath(call.Args[0])
	if err != nil || !strings.EqualFold(root, "needs") || len(path) != 3 || !strings.EqualFold(path[1], "outputs") {
		return NeedOutputReference{}, fmt.Errorf("runtime matrix expression must be exactly fromJSON(needs.<job>.outputs.<name>)")
	}
	if !runtimeMatrixIdentifier(path[0]) || !runtimeMatrixIdentifier(path[2]) {
		return NeedOutputReference{}, fmt.Errorf("runtime matrix expression has an invalid job or output name")
	}
	return NeedOutputReference{Job: path[0], Output: path[2]}, nil
}

func runtimeMatrixIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 255 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// SecretReferences returns the statically named secrets referenced by a
// template. Dynamic indexes return an error because the runtime cannot determine
// which values to resolve and register with the log redactor before execution.
func SecretReferences(template string) ([]string, error) {
	found := map[string]struct{}{}
	err := visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		return collectSecretReferences(expression, found)
	})
	if err != nil {
		return nil, err
	}
	return sortedReferenceNames(found), nil
}

// ConditionSecretReferences returns statically named secrets from one
// condition without interpreting string literal contents as templates.
func ConditionSecretReferences(source string) ([]string, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return nil, err
	}
	found := map[string]struct{}{}
	if err := collectSecretReferences(node, found); err != nil {
		return nil, err
	}
	return sortedReferenceNames(found), nil
}

func collectSecretReferences(expression actionlint.ExprNode, found map[string]struct{}) error {
	var referenceErr error
	actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
		if !entering || referenceErr != nil {
			return
		}
		switch node.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.ArrayDerefNode, *actionlint.IndexAccessNode:
		default:
			return
		}
		if _, ok := node.(*actionlint.ArrayDerefNode); ok && strings.EqualFold(referenceRoot(node), "secrets") {
			referenceErr = fmt.Errorf("secret reference must name exactly one secret")
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
}

// ValidateServiceRuntimeTemplate permits only needs.<job>.outputs.<name>
// references after compile-time service evaluation. Other documented service
// contexts must already have been resolved by the compiler.
func ValidateServiceRuntimeTemplate(template string) error {
	return visitTemplateExpressions(template, func(node actionlint.ExprNode) error {
		root, path, err := referencePath(node)
		if err != nil || !strings.EqualFold(root, "needs") || len(path) != 3 || !strings.EqualFold(path[1], "outputs") {
			return fmt.Errorf("service runtime expression must directly reference needs.<job>.outputs.<name>")
		}
		return nil
	})
}

// ValidateServiceCredentialTemplate matches GitHub's narrower service
// credential context: github, vars, secrets, and env direct references.
func ValidateServiceCredentialTemplate(template string) error {
	return visitTemplateExpressions(template, func(node actionlint.ExprNode) error {
		root, path, err := referencePath(node)
		if err != nil {
			return fmt.Errorf("service credential expression requires a direct context reference: %w", err)
		}
		switch classifyRuntimeReference(root, path) {
		case runtimeReferenceGitHub, runtimeReferenceVar, runtimeReferenceSecret, runtimeReferenceEnv:
			return nil
		default:
			return fmt.Errorf("service credential expression context %q is unsupported", root)
		}
	})
}

func ValidateServiceMapRuntimeExpression(source string) error {
	body, err := expressionBody(source)
	if err != nil {
		return err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return fmt.Errorf("invalid service-map expression: %w", parseErr)
	}
	call, ok := node.(*actionlint.FuncCallNode)
	if !ok || !strings.EqualFold(call.Callee, "fromJSON") || len(call.Args) != 1 {
		return fmt.Errorf("service-map expression must call fromJSON with one needs output")
	}
	root, path, err := referencePath(call.Args[0])
	if err != nil || !strings.EqualFold(root, "needs") || len(path) != 3 || !strings.EqualFold(path[1], "outputs") {
		return fmt.Errorf("service-map expression must call fromJSON with one needs output")
	}
	return nil
}

func sortedReferenceNames(found map[string]struct{}) []string {
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ReferencesGitHubToken reports whether a template statically references
// github.token. Dynamic GitHub indexes return an error so compiler-owned token
// authority cannot depend on a runtime-selected property.
func ReferencesGitHubToken(template string) (bool, error) {
	return referencesGitHubToken(template, false, false)
}

// ReferencesStepGitHubToken reports whether a step runtime template statically
// references github.token, including the exact toJSON(github) shape.
func ReferencesStepGitHubToken(template string) (bool, error) {
	return referencesGitHubToken(template, true, true)
}

// ReferencesCompositeStepGitHubToken validates the same runtime surface for a
// composite-authored step input, but does not let toJSON(github) grant token
// authority. A direct github.token reference still reports true.
func ReferencesCompositeStepGitHubToken(template string) (bool, error) {
	return referencesGitHubToken(template, true, false)
}

func referencesGitHubToken(template string, allowContextSerialization, contextSerializationReferencesToken bool) (bool, error) {
	found := false
	err := visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		referencesToken, err := nodeReferencesGitHubToken(expression, allowContextSerialization, contextSerializationReferencesToken)
		found = found || referencesToken
		return err
	})
	return found, err
}

// ConditionReferencesGitHubToken reports direct token references in one
// condition without interpreting string literal contents as templates.
func ConditionReferencesGitHubToken(source string) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return false, err
	}
	return nodeReferencesGitHubToken(node, false, false)
}

func nodeReferencesGitHubToken(expression actionlint.ExprNode, allowContextSerialization, contextSerializationReferencesToken bool) (bool, error) {
	found := false
	var referenceErr error
	actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
		if !entering || referenceErr != nil {
			return
		}
		switch node.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.ArrayDerefNode, *actionlint.IndexAccessNode:
		default:
			return
		}
		if !strings.EqualFold(referenceRoot(node), "github") {
			return
		}
		if referenceHasArrayDeref(node) {
			referenceErr = fmt.Errorf("github reference must name one static property")
			return
		}
		_, path, err := referencePath(node)
		if err != nil {
			referenceErr = fmt.Errorf("github reference: %w", err)
			return
		}
		if len(path) == 0 {
			if referenceReceiver(node, parent) {
				return
			}
			if call, ok := parent.(*actionlint.FuncCallNode); allowContextSerialization && ok && isToJSONGitHubCall(call) {
				found = found || contextSerializationReferencesToken
				return
			}
			referenceErr = fmt.Errorf("github reference must name one static property")
			return
		}
		if !strings.EqualFold(path[0], "token") {
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
	return found, referenceErr
}

func isToJSONGitHubCall(call *actionlint.FuncCallNode) bool {
	if !strings.EqualFold(call.Callee, "toJSON") || len(call.Args) != 1 {
		return false
	}
	root, ok := call.Args[0].(*actionlint.VariableNode)
	return ok && strings.EqualFold(root.Name, "github")
}

func referenceRoot(node actionlint.ExprNode) string {
	switch node := node.(type) {
	case *actionlint.VariableNode:
		return node.Name
	case *actionlint.ObjectDerefNode:
		return referenceRoot(node.Receiver)
	case *actionlint.ArrayDerefNode:
		return referenceRoot(node.Receiver)
	case *actionlint.IndexAccessNode:
		return referenceRoot(node.Operand)
	default:
		return ""
	}
}

func referenceHasArrayDeref(node actionlint.ExprNode) bool {
	switch node := node.(type) {
	case *actionlint.ObjectDerefNode:
		return referenceHasArrayDeref(node.Receiver)
	case *actionlint.ArrayDerefNode:
		return true
	case *actionlint.IndexAccessNode:
		return referenceHasArrayDeref(node.Operand)
	default:
		return false
	}
}

func referenceReceiver(node, parent actionlint.ExprNode) bool {
	switch parent := parent.(type) {
	case *actionlint.ObjectDerefNode:
		return parent.Receiver == node
	case *actionlint.ArrayDerefNode:
		return parent.Receiver == node
	case *actionlint.IndexAccessNode:
		return parent.Operand == node
	default:
		return false
	}
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

// ReferencesGitHubEvent reports whether a condition reads the event payload or
// an event-derived GitHub ref scalar that the compiler must fold.
func ReferencesGitHubEvent(source string) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return false, err
	}
	return nodeReferencesCompileGitHubEvent(node), nil
}

// TemplateReferencesGitHubEvent reports whether an interpolated template
// retains the compile-time-only event payload.
func TemplateReferencesGitHubEvent(template string) (bool, error) {
	found := false
	err := visitTemplateExpressions(template, func(node actionlint.ExprNode) error {
		found = found || nodeReferencesGitHubEventPayload(node)
		return nil
	})
	return found, err
}

func nodeReferencesCompileGitHubEvent(node actionlint.ExprNode) bool {
	return nodeReferencesGitHub(node, func(path []string) bool {
		if len(path) == 0 {
			return false
		}
		switch strings.ToLower(path[0]) {
		case "base_ref", "ref_name", "ref_type":
			return len(path) == 1
		case "event":
			return true
		default:
			return false
		}
	})
}

func nodeReferencesGitHubEventPayload(node actionlint.ExprNode) bool {
	return nodeReferencesGitHub(node, func(path []string) bool {
		return len(path) != 0 && strings.EqualFold(path[0], "event")
	})
}

func nodeReferencesGitHub(node actionlint.ExprNode, matches func([]string) bool) bool {
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
		found = pathErr == nil && strings.EqualFold(root, "github") && matches(path)
	})
	return found
}

// ReferencesStatusFunction reports whether a condition explicitly names one of
// the status functions that suppress the implicit success guard.
func ReferencesStatusFunction(source string) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return false, err
	}
	return containsStatusFunction(node), nil
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

// TemplateUsesContext reports whether any expression in a template references
// a named context.
func TemplateUsesContext(source, contextName string) (bool, error) {
	found := false
	err := visitTemplateExpressions(source, func(expression actionlint.ExprNode) error {
		found = found || nodeUsesContext(expression, contextName)
		return nil
	})
	return found, err
}

func nodeUsesContext(expression actionlint.ExprNode, contextName string) bool {
	found := false
	actionlint.VisitExprNode(expression, func(node, _ actionlint.ExprNode, entering bool) {
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
	return found
}

// GitHubTokenInputReferences returns inputs referenced in the same template
// expressions as github.token and reports whether those expressions also use
// computed input access.
func GitHubTokenInputReferences(source string) ([]string, bool, error) {
	found := map[string]struct{}{}
	dynamic := false
	err := visitGitHubTokenExpressions(source, func(expression actionlint.ExprNode) error {
		inputNames, dynamicInputs := nodeInputReferences(expression)
		for _, name := range inputNames {
			found[name] = struct{}{}
		}
		dynamic = dynamic || dynamicInputs
		return nil
	})
	return sortedReferenceNames(found), dynamic, err
}

// GitHubTokenReferencesRuntimeValue reports whether an interpolation that
// references github.token also depends on a runtime context other than inputs
// or the retained server URL.
func GitHubTokenReferencesRuntimeValue(source string) (bool, error) {
	found := false
	err := visitGitHubTokenExpressions(source, func(expression actionlint.ExprNode) error {
		if nodeUsesGitHubPropertyOutside(expression, "server_url", "token") {
			found = true
			return nil
		}
		for _, contextName := range []string{"env", "job", "matrix", "needs", "runner", "services", "steps", "vars"} {
			if nodeUsesContext(expression, contextName) {
				found = true
				break
			}
		}
		return nil
	})
	return found, err
}

// GitHubTokenRequiresEvaluation reports whether evaluating a template can
// reach github.token after reducing known inputs and the retained server URL.
func GitHubTokenRequiresEvaluation(source string, context Context, unknownInputs map[string]bool) (bool, error) {
	required := false
	err := visitGitHubTokenExpressions(source, func(expression actionlint.ExprNode) error {
		required = required || githubTokenReachable(expression, context, unknownInputs)
		return nil
	})
	return required, err
}

func githubTokenReachable(node actionlint.ExprNode, context Context, unknownInputs map[string]bool) bool {
	logical, ok := node.(*actionlint.LogicalOpNode)
	if ok {
		if githubTokenReachable(logical.Left, context, unknownInputs) {
			return true
		}
		left, known := evaluateKnownStepNode(logical.Left, context, unknownInputs)
		if known && (logical.Kind == actionlint.LogicalOpNodeKindAnd && !left || logical.Kind == actionlint.LogicalOpNodeKindOr && left) {
			return false
		}
		return githubTokenReachable(logical.Right, context, unknownInputs)
	}
	if directGitHubTokenReference(node) {
		return true
	}
	switch node := node.(type) {
	case *actionlint.ObjectDerefNode:
		return githubTokenReachable(node.Receiver, context, unknownInputs)
	case *actionlint.ArrayDerefNode:
		return githubTokenReachable(node.Receiver, context, unknownInputs)
	case *actionlint.IndexAccessNode:
		return githubTokenReachable(node.Operand, context, unknownInputs) || githubTokenReachable(node.Index, context, unknownInputs)
	case *actionlint.NotOpNode:
		return githubTokenReachable(node.Operand, context, unknownInputs)
	case *actionlint.CompareOpNode:
		return githubTokenReachable(node.Left, context, unknownInputs) || githubTokenReachable(node.Right, context, unknownInputs)
	case *actionlint.FuncCallNode:
		if strings.EqualFold(node.Callee, "case") && len(node.Args) >= 3 && len(node.Args)%2 == 1 {
			return githubTokenReachableCase(node.Args, context, unknownInputs)
		}
		if reachable, handled := githubTokenReachablePureCall(node, context, unknownInputs); handled {
			return reachable
		}
		for _, argument := range node.Args {
			if githubTokenReachable(argument, context, unknownInputs) {
				return true
			}
		}
	}
	return false
}

func githubTokenReachableCase(arguments []actionlint.ExprNode, context Context, unknownInputs map[string]bool) bool {
	if len(arguments) == 1 {
		return githubTokenReachable(arguments[0], context, unknownInputs)
	}
	if githubTokenReachable(arguments[0], context, unknownInputs) {
		return true
	}
	selected, known := evaluateKnownStepNode(arguments[0], context, unknownInputs)
	if known && selected {
		return githubTokenReachable(arguments[1], context, unknownInputs)
	}
	if !known && githubTokenReachable(arguments[1], context, unknownInputs) {
		return true
	}
	return githubTokenReachableCase(arguments[2:], context, unknownInputs)
}

func githubTokenReachablePureCall(node *actionlint.FuncCallNode, context Context, unknownInputs map[string]bool) (bool, bool) {
	if len(node.Args) == 0 {
		return false, false
	}
	firstReachable := githubTokenReachable(node.Args[0], context, unknownInputs)
	first, firstKnown := evaluateKnownStepValue(node.Args[0], context, unknownInputs)
	switch strings.ToLower(node.Callee) {
	case "format":
		if firstReachable {
			return true, true
		}
		format, convertible := expressionAggregateString(first)
		if !firstKnown || !convertible {
			return !firstKnown && anyGitHubTokenReachable(node.Args[1:], context, unknownInputs), true
		}
		reachable := false
		_, _ = expressionFormat(format, len(node.Args)-1, func(index int) (any, error) {
			reachable = reachable || githubTokenReachable(node.Args[index+1], context, unknownInputs)
			return "", nil
		})
		return reachable, true
	case "contains":
		if firstReachable {
			return true, true
		}
		if !firstKnown {
			return anyGitHubTokenReachable(node.Args[1:], context, unknownInputs), true
		}
		if items, ok := expressionCollection(first); ok && len(items) == 0 {
			return false, true
		}
		if _, ok := expressionString(first); !ok {
			if _, collection := expressionCollection(first); !collection {
				return false, true
			}
		}
		return anyGitHubTokenReachable(node.Args[1:], context, unknownInputs), true
	case "startswith", "endswith":
		if firstReachable {
			return true, true
		}
		if firstKnown {
			if _, convertible := expressionString(first); !convertible {
				return false, true
			}
		}
		return anyGitHubTokenReachable(node.Args[1:], context, unknownInputs), true
	case "join":
		if firstReachable {
			return true, true
		}
		if !firstKnown {
			return anyGitHubTokenReachable(node.Args[1:], context, unknownInputs), true
		}
		items, collection := expressionCollection(first)
		if !collection || len(items) <= 1 {
			return false, true
		}
		return anyGitHubTokenReachable(node.Args[1:], context, unknownInputs), true
	case "tojson", "fromjson":
		return firstReachable, true
	default:
		return false, false
	}
}

func anyGitHubTokenReachable(nodes []actionlint.ExprNode, context Context, unknownInputs map[string]bool) bool {
	for _, node := range nodes {
		if githubTokenReachable(node, context, unknownInputs) {
			return true
		}
	}
	return false
}

func directGitHubTokenReference(node actionlint.ExprNode) bool {
	switch node.(type) {
	case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
	default:
		return false
	}
	root, path, err := referencePath(node)
	return err == nil && strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "token")
}

func evaluateKnownStepNode(node actionlint.ExprNode, context Context, unknownInputs map[string]bool) (bool, bool) {
	if logical, ok := node.(*actionlint.LogicalOpNode); ok {
		left, known := evaluateKnownStepNode(logical.Left, context, unknownInputs)
		if known {
			if logical.Kind == actionlint.LogicalOpNodeKindAnd && !left || logical.Kind == actionlint.LogicalOpNodeKindOr && left {
				return left, true
			}
			return evaluateKnownStepNode(logical.Right, context, unknownInputs)
		}
		right, rightKnown := evaluateKnownStepNode(logical.Right, context, unknownInputs)
		if rightKnown && (logical.Kind == actionlint.LogicalOpNodeKindAnd && !right || logical.Kind == actionlint.LogicalOpNodeKindOr && right) {
			return right, true
		}
		return false, false
	}
	if call, ok := node.(*actionlint.FuncCallNode); ok && strings.EqualFold(call.Callee, "case") && len(call.Args) >= 3 && len(call.Args)%2 == 1 {
		for i := 0; i < len(call.Args)-1; i += 2 {
			selected, known := evaluateKnownStepNode(call.Args[i], context, unknownInputs)
			if !known {
				return false, false
			}
			if selected {
				return evaluateKnownStepNode(call.Args[i+1], context, unknownInputs)
			}
		}
		return evaluateKnownStepNode(call.Args[len(call.Args)-1], context, unknownInputs)
	}
	value, known := evaluateKnownStepValue(node, context, unknownInputs)
	return githubTruthy(value), known
}

func evaluateKnownStepValue(node actionlint.ExprNode, context Context, unknownInputs map[string]bool) (any, bool) {
	value, err := evaluateKnownStepExpression(node, context, unknownInputs)
	return value, err == nil
}

var errKnownStepValueUnavailable = errors.New("step runtime value is unavailable during planning")

// EvaluateKnownStep evaluates a step template using only retained planning
// values. Runtime-dependent templates are reported as unknown.
func EvaluateKnownStep(template string, context Context, unknownInputs map[string]bool) (string, bool, error) {
	value, err := evaluateRuntimeTemplate(template, context, func(node actionlint.ExprNode, context Context) (any, error) {
		return evaluateKnownStepExpression(node, context, unknownInputs)
	})
	if errors.Is(err, errKnownStepValueUnavailable) {
		return "", false, nil
	}
	return value, err == nil, err
}

func evaluateKnownStepExpression(node actionlint.ExprNode, context Context, unknownInputs map[string]bool) (any, error) {
	evaluator := newSemanticEvaluator(stepRuntimeSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		switch {
		case strings.EqualFold(root, "inputs") && len(path) == 1:
			if (context.Inputs == nil && context.WorkflowInputs == nil) || unknownInputs[strings.ToLower(path[0])] {
				return nil, errKnownStepValueUnavailable
			}
			return resolveRuntimeReferenceWithMissingMembers(root, path, context)
		case strings.EqualFold(root, "env") && len(path) == 1:
			if _, ok := findStringValue(context.Env, path[0]); !ok {
				return nil, errKnownStepValueUnavailable
			}
			return resolveRuntimeReferenceWithMissingMembers(root, path, context)
		case strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "server_url"):
			return resolveRuntimeReferenceWithMissingMembers(root, path, context)
		default:
			return nil, errKnownStepValueUnavailable
		}
	}
	evaluator.resolveRoot = func(root string) (any, error) {
		if !strings.EqualFold(root, "inputs") || (context.Inputs == nil && context.WorkflowInputs == nil) || len(unknownInputs) != 0 {
			return nil, errKnownStepValueUnavailable
		}
		if context.Inputs != nil {
			return context.Inputs, nil
		}
		return context.WorkflowInputs, nil
	}
	evaluator.truthy = githubTruthy
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		return githubCompare(kind, left, right)
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return errKnownStepValueUnavailable }
	evaluator.logicalError = func(actionlint.LogicalOpNodeKind) error { return errKnownStepValueUnavailable }
	evaluator.call = func(evaluator *semanticEvaluator, call *actionlint.FuncCallNode) (any, error) {
		if value, recognized, err := evaluatePureFunction(evaluator, call); recognized {
			return value, err
		}
		return nil, errKnownStepValueUnavailable
	}
	return evaluator.evaluate(node)
}

func visitGitHubTokenExpressions(source string, visit func(actionlint.ExprNode) error) error {
	return visitTemplateExpressions(source, func(expression actionlint.ExprNode) error {
		referencesToken, err := nodeReferencesGitHubToken(expression, true, false)
		if err != nil || !referencesToken {
			return err
		}
		return visit(expression)
	})
}

// ConditionInputReferences returns statically named inputs from a condition
// and reports whether it also reads the whole context or uses computed access.
func ConditionInputReferences(source string) ([]string, bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return nil, false, err
	}
	names, dynamic := nodeInputReferences(node)
	return names, dynamic, nil
}

// TemplateInputReferences returns statically named inputs from a template and
// reports whether it also reads the whole context or uses computed access.
func TemplateInputReferences(source string) ([]string, bool, error) {
	found := map[string]struct{}{}
	dynamic := false
	err := visitTemplateExpressions(source, func(expression actionlint.ExprNode) error {
		names, expressionDynamic := nodeInputReferences(expression)
		for _, name := range names {
			found[name] = struct{}{}
		}
		dynamic = dynamic || expressionDynamic
		return nil
	})
	return sortedReferenceNames(found), dynamic, err
}

func nodeInputReferences(expression actionlint.ExprNode) ([]string, bool) {
	found := map[string]struct{}{}
	dynamic := false
	actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
		if !entering || referenceReceiver(node, parent) {
			return
		}
		switch node.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.ArrayDerefNode, *actionlint.IndexAccessNode:
		default:
			return
		}
		if !strings.EqualFold(referenceRoot(node), "inputs") {
			return
		}
		root, path, err := referencePath(node)
		if err != nil || len(path) == 0 {
			dynamic = true
			return
		}
		if strings.EqualFold(root, "inputs") {
			found[strings.ToLower(path[0])] = struct{}{}
		}
	})
	return sortedReferenceNames(found), dynamic
}

// TemplateUsesGitHubPropertyOutside reports whether a template references a
// github property other than the allowed static properties.
func TemplateUsesGitHubPropertyOutside(source string, allowed ...string) (bool, error) {
	found := false
	err := visitTemplateExpressions(source, func(expression actionlint.ExprNode) error {
		found = found || nodeUsesGitHubPropertyOutside(expression, allowed...)
		return nil
	})
	return found, err
}

func nodeUsesGitHubPropertyOutside(expression actionlint.ExprNode, allowed ...string) bool {
	allowedProperties := make(map[string]bool, len(allowed))
	for _, property := range allowed {
		allowedProperties[strings.ToLower(property)] = true
	}
	found := false
	actionlint.VisitExprNode(expression, func(node, _ actionlint.ExprNode, entering bool) {
		if !entering || found {
			return
		}
		switch node.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
		default:
			return
		}
		root, path, _ := referencePath(node)
		found = strings.EqualFold(root, "github") && len(path) != 0 && !allowedProperties[strings.ToLower(path[0])]
	})
	return found
}

// ConditionUsesStaticContextReference reports whether a condition contains a
// direct or literal-index reference to a context. Computed indexes and
// projections remain runtime expressions.
func ConditionUsesStaticContextReference(source, contextName string) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil || empty {
		return false, err
	}
	return usesStaticContextReference(node, contextName), nil
}

// TemplateUsesStaticContextReference reports whether a template contains a
// direct or literal-index reference to a context. Computed indexes and
// projections remain runtime expressions.
func TemplateUsesStaticContextReference(source, contextName string) (bool, error) {
	found := false
	err := visitTemplateExpressions(source, func(expression actionlint.ExprNode) error {
		found = found || usesStaticContextReference(expression, contextName)
		return nil
	})
	return found, err
}

func usesStaticContextReference(expression actionlint.ExprNode, contextName string) bool {
	found := false
	actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
		if !entering || found || referenceReceiver(node, parent) {
			return
		}
		switch node.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.ArrayDerefNode, *actionlint.IndexAccessNode:
		default:
			return
		}
		root, _, err := referencePath(node)
		found = err == nil && strings.EqualFold(root, contextName)
	})
	return found
}
