package expression

import (
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

// ValidateDockerActionArg permits literals and direct inputs.<name>
// interpolation in one Docker action argument.
func ValidateDockerActionArg(template string) error {
	return visitTemplateExpressions(template, validateDockerActionArgNode)
}

// EvaluateDockerActionArg revalidates and evaluates one Docker action argument
// without exposing any context other than resolved action inputs.
func EvaluateDockerActionArg(template string, inputs map[string]string) (string, error) {
	if err := ValidateDockerActionArg(template); err != nil {
		return "", err
	}
	return evaluateRuntimeTemplate(template, Context{Inputs: inputs}, func(node actionlint.ExprNode, context Context) (any, error) {
		root, path, _ := referencePath(node)
		return resolveRuntimeReference(root, path, context)
	})
}

func validateDockerActionArgNode(node actionlint.ExprNode) error {
	root, path, err := referencePath(node)
	if err != nil {
		return fmt.Errorf("docker action argument requires a direct inputs reference: %w", err)
	}
	if !strings.EqualFold(root, "inputs") || len(path) != 1 {
		return fmt.Errorf("docker action argument may reference only inputs.<name>")
	}
	return nil
}
