// Deferred reusable-workflow inputs: caller-authored string inputs that embed
// needs.<job>.outputs.<name> references inside a larger template or
// expression. Graph construction folds every graph-time subtree and keeps the
// needs references; Buildkite evaluates the residual template against verified
// producer outputs before the called job runs.

package expression

import (
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

// ReduceDeferredInput folds the graph-time parts of a reusable-workflow input
// template and returns the residual template with the needs outputs it still
// references, in first-use order without repeats. Static parent inputs must
// already be substituted. The residual validates under ProfileDeferredInput.
// An input without needs references reduces to a plain string.
func ReduceDeferredInput(template string, context CompileContext) (string, []NeedOutputReference, error) {
	const open = "${{"
	var (
		reduced    strings.Builder
		references []NeedOutputReference
	)
	remaining := template
	for {
		start := strings.Index(remaining, open)
		if start < 0 {
			reduced.WriteString(remaining)
			return reduced.String(), references, nil
		}
		reduced.WriteString(remaining[:start])
		source := remaining[start+len(open):]
		_, consumed, lexErr := actionlint.LexExpression(source)
		if lexErr != nil {
			return "", nil, fmt.Errorf("invalid expression: %w", lexErr)
		}
		node, err := parseCompileExpression(Expression{Text: open + source[:consumed]})
		if err != nil {
			return "", nil, err
		}
		if err := validateDeferredInputCompileNode(node); err != nil {
			return "", nil, err
		}
		if err := validateDeferredInputGraphReferences(node, context); err != nil {
			return "", nil, err
		}
		value, available, err := evaluateCompileNodeAvailable(node, context)
		if err != nil {
			return "", nil, err
		}
		if available {
			replacement, scalar := expressionString(value)
			if !scalar {
				return "", nil, fmt.Errorf("template expression resolved to %T, want a scalar", value)
			}
			if introducesExpressionSyntax(reduced.String(), replacement, source[consumed:]) {
				return "", nil, fmt.Errorf("compile-time expression result contains expression syntax")
			}
			reduced.WriteString(replacement)
		} else {
			references = appendNeedOutputReferences(references, node)
			reduced.WriteString(open)
			reduced.WriteString(" ")
			reduced.WriteString(reduceCompileNode(node, context))
			reduced.WriteString(" }}")
		}
		remaining = source[consumed:]
	}
}

// DeferredInputReferences lists the needs outputs a reduced deferred-input
// template reads, in first-use order without repeats.
func DeferredInputReferences(template string) ([]NeedOutputReference, error) {
	var references []NeedOutputReference
	err := visitTemplateExpressions(template, func(node actionlint.ExprNode) error {
		if err := validateDeferredInputNode(node); err != nil {
			return err
		}
		references = appendNeedOutputReferences(references, node)
		return nil
	})
	return references, err
}

// validateDeferredInputCompileNode accepts the graph-time expression grammar
// plus direct needs.<job>.outputs.<name> references.
func validateDeferredInputCompileNode(node actionlint.ExprNode) error {
	validator := newSemanticValidator(compileTimeSurface)
	validator.validateReference = func(node actionlint.ExprNode, root string, path []string) error {
		if strings.EqualFold(root, "needs") {
			return validateDeferredNeedReference(node, root, path)
		}
		return validateCompileExpressionNode(node)
	}
	validator.validateAccess = func(node actionlint.ExprNode) error {
		if strings.EqualFold(referenceRoot(node), "needs") {
			return deferredNeedReferenceError(node)
		}
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

// validateDeferredInputGraphReferences requires every reference outside needs
// to resolve while constructing the graph, so the residual expression depends
// on needs alone.
func validateDeferredInputGraphReferences(node actionlint.ExprNode, context CompileContext) error {
	var validationErr error
	actionlint.VisitExprNode(node, func(current, parent actionlint.ExprNode, entering bool) {
		if !entering || validationErr != nil || referenceReceiver(current, parent) {
			return
		}
		switch current.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode, *actionlint.ArrayDerefNode:
		default:
			return
		}
		root, path, err := referencePath(current)
		if err != nil || strings.EqualFold(root, "needs") {
			return
		}
		_, available, err := evaluateCompileNodeAvailable(current, context)
		if err != nil {
			validationErr = err
			return
		}
		if !available {
			validationErr = fmt.Errorf("compile-time expression references unavailable value %q", referenceName(root, path))
		}
	})
	return validationErr
}

// validateDeferredInputNode validates one residual expression: needs-only
// runtime grammar with direct output references.
func validateDeferredInputNode(node actionlint.ExprNode) error {
	if err := validateStepRuntimeExpression(node, false, false, map[string]bool{"needs": true}); err != nil {
		return err
	}
	var validationErr error
	actionlint.VisitExprNode(node, func(current, parent actionlint.ExprNode, entering bool) {
		if !entering || validationErr != nil || referenceReceiver(current, parent) {
			return
		}
		switch current.(type) {
		case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode, *actionlint.ArrayDerefNode:
		default:
			return
		}
		root, path, err := referencePath(current)
		if err != nil || !strings.EqualFold(root, "needs") {
			return
		}
		validationErr = validateDeferredNeedReference(current, root, path)
	})
	return validationErr
}

func validateDeferredNeedReference(node actionlint.ExprNode, root string, path []string) error {
	if expressionReferenceUsesIndex(node) || len(path) != 3 || !strings.EqualFold(path[1], "outputs") || !runtimeMatrixIdentifier(path[0]) || !runtimeMatrixIdentifier(path[2]) {
		return deferredNeedReferenceError(node)
	}
	return nil
}

func deferredNeedReferenceError(node actionlint.ExprNode) error {
	root, path, err := referencePath(node)
	if err != nil {
		return fmt.Errorf("reusable-workflow input needs reference must be needs.<job>.outputs.<name>")
	}
	return fmt.Errorf("reusable-workflow input needs reference %q must be needs.<job>.outputs.<name>", referenceName(root, path))
}

func appendNeedOutputReferences(references []NeedOutputReference, node actionlint.ExprNode) []NeedOutputReference {
	actionlint.VisitExprNode(node, func(current, parent actionlint.ExprNode, entering bool) {
		if !entering || referenceReceiver(current, parent) {
			return
		}
		if _, ok := current.(*actionlint.ObjectDerefNode); !ok {
			return
		}
		root, path, err := referencePath(current)
		if err != nil || !strings.EqualFold(root, "needs") || len(path) != 3 {
			return
		}
		reference := NeedOutputReference{Job: path[0], Output: path[2]}
		for _, existing := range references {
			if strings.EqualFold(existing.Job, reference.Job) && strings.EqualFold(existing.Output, reference.Output) {
				return
			}
		}
		references = append(references, reference)
	})
	return references
}
