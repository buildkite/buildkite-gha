package expression

// These aliases keep the pre-engine conformance corpus as a test-only oracle.
// Production callers must select a site profile through Engine.
func Parse(text string, line, column int) (Expression, error) {
	return parseExpression(text, line, column)
}

func EvaluateCompile(expr Expression, context CompileContext) (any, error) {
	return evaluateCompile(expr, context)
}

func EvaluateCompileAvailable(expr Expression, context CompileContext) (any, bool, error) {
	return evaluateCompileAvailable(expr, context)
}

func EvaluateCompileCondition(source string, context CompileContext) (bool, error) {
	return evaluateCompileCondition(source, context)
}

func ReduceCompileCondition(source string, context CompileContext) (string, error) {
	return reduceCompileCondition(source, context)
}

func EvaluateCompileTemplate(source string, context CompileContext) (string, error) {
	return evaluateCompileTemplate(source, context)
}

func EvaluateCompileStringTemplate(source string, context CompileContext) (string, error) {
	return evaluateCompileStringTemplate(source, context)
}

func EvaluateAvailableCompileTemplate(source string, context CompileContext) (string, error) {
	return evaluateAvailableCompileTemplate(source, context)
}

func ReduceAvailableCompileTemplate(source string, context CompileContext) (string, error) {
	return reduceAvailableCompileTemplate(source, context)
}

func ReferencePath(text string) (string, []string, error) { return staticReferencePath(text) }

func SecretReferences(source string) ([]string, error) { return secretReferences(source) }

func ConditionSecretReferences(source string) ([]string, error) {
	return conditionSecretReferences(source)
}

func ReferencesStatusFunction(source string) (bool, error) { return referencesStatusFunction(source) }

func ValidateReusableInputDefault(source string) error { return validateReusableInputDefault(source) }

func EvaluateReusableInputDefault(source string, context CompileContext) (any, error) {
	return evaluateReusableInputDefault(source, context)
}

func ValidateRunName(source string) error { return validateRunName(source) }

func EvaluateRunName(source string, context CompileContext) (string, error) {
	return evaluateRunName(source, context)
}

func ValidateActionInputDefault(source string) error { return validateActionInputDefault(source) }

func EvaluateActionInputDefault(source string, context Context) (string, error) {
	return evaluateActionInputDefault(source, context)
}

func ValidateDockerActionArg(source string) error { return validateDockerActionArg(source) }

func EvaluateDockerActionArg(source string, inputs map[string]string) (string, error) {
	return evaluateDockerActionArg(source, inputs)
}

func ValidateRuntimeTemplate(source string) error { return validateRuntimeTemplate(source) }

func Evaluate(source string, context Context) (string, error) {
	return evaluateDirectTemplate(source, context)
}

func EvaluateValue(source string, context Context) (any, error) {
	return evaluateRuntimeValue(source, context)
}

func EvaluateObject(source string, context Context) ([]ObjectEntry, error) {
	return evaluateObject(source, context)
}

func EvaluateStep(source string, context Context) (string, error) {
	return evaluateStep(source, context)
}

func EvaluateStepControl(source string, context Context) (any, error) {
	return evaluateStepControl(source, context)
}

func ValidateStepControl(source string) error { return validateStepControl(source) }

func EvaluateJobEnvironment(source string, context Context) (string, error) {
	return evaluateJobEnvironment(source, context)
}

func EvaluateJobDefault(source string, context Context) (string, error) {
	return evaluateJobDefault(source, context)
}

func EvaluateJobOutput(source string, context Context) (string, error) {
	return evaluateJobOutput(source, context)
}

func ValidateCondition(source string, scope ConditionScope) error {
	return validateConditionLegacy(source, scope)
}

func ValidateConditionWithMatrix(source string, scope ConditionScope, matrix map[string]any) error {
	return validateConditionWithMatrix(source, scope, matrix)
}

func ValidateCallCondition(source string) error { return validateCallCondition(source) }

func ValidateCompileCallCondition(source string, context CompileContext) error {
	return validateCompileCallCondition(source, context)
}

func ValidateActionLifecycleCondition(source string) error {
	return validateActionLifecycleCondition(source)
}

func ValidateCompileConditionWithMatrix(source string, scope ConditionScope, context CompileContext, matrix map[string]any) error {
	return validateCompileConditionWithMatrix(source, scope, context, matrix)
}

func EvaluateActionLifecycleCondition(source string, context ConditionContext) (bool, error) {
	return evaluateActionLifecycleCondition(source, context)
}

func EvaluateCondition(source string, context ConditionContext) (bool, error) {
	return evaluateConditionLegacy(source, context)
}

func ValidateServiceRuntimeTemplate(source string) error {
	return validateServiceRuntimeTemplate(source)
}

func ValidateServiceCredentialTemplate(source string) error {
	return validateServiceCredentialTemplate(source)
}

func ValidateServiceMapRuntimeExpression(source string) error {
	return validateServiceMapRuntimeExpression(source)
}

func ReferencesGitHubToken(source string) (bool, error) {
	return templateReferencesGitHubToken(source)
}

func ReferencesStepGitHubToken(source string) (bool, error) {
	return stepReferencesGitHubToken(source)
}

func ReferencesCompositeStepGitHubToken(source string) (bool, error) {
	return compositeStepReferencesGitHubToken(source)
}

func ConditionReferencesGitHubToken(source string) (bool, error) {
	return conditionReferencesGitHubToken(source)
}

func ReferencesJobStatus(source string) (bool, error) { return templateReferencesJobStatus(source) }

func ReferencesGitHubEvent(source string) (bool, error) {
	return conditionReferencesCompileEvent(source)
}

func ConditionReferencesGitHubEventPayload(source string) (bool, error) {
	return conditionReferencesEventPayload(source)
}

func TemplateReferencesGitHubEvent(source string) (bool, error) {
	return templateReferencesEventPayload(source)
}

func ConditionUsesContext(source, contextName string) (bool, error) {
	return conditionUsesContext(source, contextName)
}

func TemplateUsesContext(source, contextName string) (bool, error) {
	return templateUsesContext(source, contextName)
}

func ConditionUsesStaticContextReference(source, contextName string) (bool, error) {
	return conditionUsesStaticContextReference(source, contextName)
}

func TemplateUsesStaticContextReference(source, contextName string) (bool, error) {
	return templateUsesStaticContextReference(source, contextName)
}

func ActionInputDefaultRequiresGitHubToken(source, serverURL string) (bool, error) {
	return actionInputDefaultRequiresGitHubToken(source, serverURL)
}
