// Static reference introspection over templates, complete expressions, and
// conditions. Each predicate deliberately answers one narrow question with
// an error for unsupported input.
package expression

import (
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
	found := false
	err := visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		referencesToken, err := nodeReferencesGitHubToken(expression)
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
	return nodeReferencesGitHubToken(node)
}

func nodeReferencesGitHubToken(expression actionlint.ExprNode) (bool, error) {
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
			if call, ok := parent.(*actionlint.FuncCallNode); ok && isToJSONGitHubCall(call) {
				found = true
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
		return nil
	})
	return found, err
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
