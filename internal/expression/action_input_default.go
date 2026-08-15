// Action input default validation and evaluation with GitHub's loose
// coercion semantics. Deliberately separate from the strict condition
// family in condition.go.
package expression

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

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
	validator := newSemanticValidator(actionInputDefaultSurface)
	validator.validateReference = func(_ actionlint.ExprNode, root string, path []string) error {
		if isJobStatusReference(root, path) {
			return nil
		}
		kind := classifyRuntimeReference(root, path)
		if kind == runtimeReferenceSecret {
			return fmt.Errorf("action input defaults cannot grant secret authority")
		}
		if kind == runtimeReferenceUnsupported {
			return fmt.Errorf("unsupported runtime expression %q", referenceName(root, path))
		}
		return nil
	}
	validator.referenceError = func(err error) error {
		return fmt.Errorf("runtime interpolation requires a direct context reference: %w", err)
	}
	validator.validateCompare = func(kind actionlint.CompareOpNodeKind) error {
		return nil
	}
	validator.afterCompare = func(*actionlint.CompareOpNode) error { return nil }
	validator.validateCall = func(_ *semanticValidator, node *actionlint.FuncCallNode) error {
		if !strings.EqualFold(node.Callee, "toJSON") || len(node.Args) != 1 {
			return fmt.Errorf("action input default function %q is unsupported", node.Callee)
		}
		root, path, err := referencePath(node.Args[0])
		if err != nil || !strings.EqualFold(root, "matrix") || len(path) != 0 {
			return fmt.Errorf("action input default toJSON requires the complete matrix context")
		}
		return nil
	}
	validator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("action input default expression is unsupported") }
	return validator.validate(node)
}

// EvaluateActionInputDefault substitutes the restricted compound expressions
// supported only in action metadata input defaults.
func EvaluateActionInputDefault(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateActionInputDefaultNode)
}

// ActionInputDefaultRequiresGitHubToken reports whether a metadata default can
// reach github.token for the event provider. Defaults involving any other
// runtime value fail closed because those values are not known during
// compilation.
func ActionInputDefaultRequiresGitHubToken(template, serverURL string) (bool, error) {
	referencesToken, err := ReferencesGitHubToken(template)
	if err != nil || !referencesToken {
		return referencesToken, err
	}
	onlyKnownReferences := true
	err = visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		actionlint.VisitExprNode(expression, func(node, parent actionlint.ExprNode, entering bool) {
			if !entering || !onlyKnownReferences {
				return
			}
			switch node.(type) {
			case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
			default:
				return
			}
			root, path, referenceErr := referencePath(node)
			if referenceErr != nil {
				onlyKnownReferences = false
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
			}
			onlyKnownReferences = strings.EqualFold(root, "github") && len(path) == 1 &&
				(strings.EqualFold(path[0], "server_url") || strings.EqualFold(path[0], "token"))
		})
		return nil
	})
	if err != nil {
		return false, err
	}
	if !onlyKnownReferences {
		return true, nil
	}
	_, err = EvaluateActionInputDefault(template, Context{GitHub: map[string]any{"server_url": serverURL}})
	return err != nil, nil
}

func evaluateActionInputDefaultNode(node actionlint.ExprNode, context Context) (any, error) {
	evaluator := newSemanticEvaluator(actionInputDefaultSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		if isJobStatusReference(root, path) {
			if context.JobStatus == "" {
				return nil, fmt.Errorf("expression references unavailable job.status")
			}
			return context.JobStatus, nil
		}
		return resolveRuntimeReferenceWithMissingMembers(root, path, context)
	}
	evaluator.truthy = githubTruthy
	evaluator.compare = func(kind actionlint.CompareOpNodeKind, left, right any) (any, error) {
		return githubCompare(kind, left, right)
	}
	evaluator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("action input default expression is unsupported") }
	evaluator.logicalError = func(kind actionlint.LogicalOpNodeKind) error {
		return fmt.Errorf("action input default logical operator %s is unsupported", kind)
	}
	evaluator.call = func(_ *semanticEvaluator, node *actionlint.FuncCallNode) (any, error) {
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
	}
	return evaluator.evaluate(node)
}
