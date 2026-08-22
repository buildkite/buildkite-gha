// Action input default validation and evaluation with GitHub's loose
// coercion semantics. Deliberately separate from the strict condition
// family in condition.go.
package expression

import (
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
	validator.validateReference = func(node actionlint.ExprNode, root string, path []string) error {
		if isJobStatusReference(root, path) {
			return nil
		}
		if isJobCheckRunIDReference(root, path) {
			return nil
		}
		if isRunnerTempReference(root, path) {
			return fmt.Errorf("action input defaults cannot reference runner.temp")
		}
		if isDirectRunnerDebug(node, root, path) {
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
	validator.validateCall = func(validator *semanticValidator, node *actionlint.FuncCallNode) error {
		if strings.EqualFold(node.Callee, "toJSON") && len(node.Args) == 1 {
			root, path, err := referencePath(node.Args[0])
			if err == nil && strings.EqualFold(root, "matrix") && len(path) == 0 {
				return nil
			}
		}
		if recognized, err := validatePureFunction(validator, node); recognized {
			return err
		}
		return fmt.Errorf("action input default function %q is unsupported", node.Callee)
	}
	validator.unsupported = func(actionlint.ExprNode) error { return fmt.Errorf("action input default expression is unsupported") }
	return validator.validate(node)
}

// EvaluateActionInputDefault substitutes the restricted compound expressions
// supported only in action metadata input defaults.
func EvaluateActionInputDefault(template string, context Context) (string, error) {
	return evaluateRuntimeTemplate(template, context, evaluateActionInputDefaultNode)
}

func isDirectRunnerDebug(node actionlint.ExprNode, root string, path []string) bool {
	_, direct := node.(*actionlint.ObjectDerefNode)
	return direct && strings.EqualFold(root, "runner") && len(path) == 1 && strings.EqualFold(path[0], "debug")
}

func isJobCheckRunIDReference(root string, path []string) bool {
	return strings.EqualFold(root, "job") && len(path) == 1 && strings.EqualFold(path[0], "check_run_id")
}

func isRunnerTempReference(root string, path []string) bool {
	return strings.EqualFold(root, "runner") && len(path) == 1 && strings.EqualFold(path[0], "temp")
}

// ActionInputDefaultRequiresGitHubToken reports whether a metadata default can
// reach github.token for the event provider. A token branch guarded by an
// unknown runtime value requires the token because that value is not known
// during compilation.
func ActionInputDefaultRequiresGitHubToken(template, serverURL string) (bool, error) {
	referencesToken, err := ReferencesGitHubToken(template)
	if err != nil || !referencesToken {
		return referencesToken, err
	}
	requiresToken := false
	err = visitTemplateExpressions(template, func(expression actionlint.ExprNode) error {
		analysis, analysisErr := analyzeActionInputDefault(expression, map[string]any{
			"github.server_url": serverURL,
			"job.check_run_id":  "",
		})
		if analysisErr != nil {
			// Runtime-dependent evaluation failures previously fell back to
			// conservative authority. Preserve that behavior while the
			// exhaustive reference pass above continues to own validation.
			requiresToken = true
			return nil
		}
		requiresToken = requiresToken || analysis.Effects.GitHubToken != 0
		return nil
	})
	return requiresToken, err
}

func evaluateActionInputDefaultNode(node actionlint.ExprNode, context Context) (any, error) {
	root, path, err := referencePath(node)
	if err == nil && strings.EqualFold(root, "runner") && len(path) == 1 && strings.EqualFold(path[0], "debug") && !isDirectRunnerDebug(node, root, path) {
		return nil, fmt.Errorf("unsupported runtime expression %q", referenceName(root, path))
	}
	evaluator := newSemanticEvaluator(actionInputDefaultSurface)
	evaluator.resolve = func(root string, path []string) (any, error) {
		if isJobStatusReference(root, path) {
			if context.JobStatus == "" {
				return nil, fmt.Errorf("expression references unavailable job.status")
			}
			return context.JobStatus, nil
		}
		if isJobCheckRunIDReference(root, path) {
			// Buildkite creates no GitHub check run. GitHub documents this
			// property as unavailable on GitHub Enterprise Server; unavailable
			// properties interpolate as an empty string.
			return "", nil
		}
		if isRunnerTempReference(root, path) {
			return nil, fmt.Errorf("action input defaults cannot reference runner.temp")
		}
		if strings.EqualFold(root, "runner") && len(path) == 1 && strings.EqualFold(path[0], "debug") {
			return "false", nil
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
	evaluator.call = func(evaluator *semanticEvaluator, node *actionlint.FuncCallNode) (any, error) {
		if strings.EqualFold(node.Callee, "toJSON") && len(node.Args) == 1 {
			root, path, err := referencePath(node.Args[0])
			if err == nil && strings.EqualFold(root, "matrix") && len(path) == 0 {
				value, err := encodeExpressionJSON(context.Matrix)
				if err != nil {
					return nil, fmt.Errorf("encode action input default matrix as JSON: %w", err)
				}
				return value, nil
			}
		}
		if value, recognized, err := evaluatePureFunction(evaluator, node); recognized {
			return value, err
		}
		return nil, fmt.Errorf("action input default function %q is unsupported", node.Callee)
	}
	return evaluator.evaluate(node)
}
