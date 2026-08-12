// Static reference introspection over templates, complete expressions, and
// conditions. Each predicate deliberately answers one narrow question with
// its own fail-closed behavior.
package expression

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rhysd/actionlint"
)

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
