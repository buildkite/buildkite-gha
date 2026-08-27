package compiler

import (
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

var compilerExpressionEngine = expression.NewEngine()

func compileExpressionSite(source string, profile expression.ProfileID, result expression.ResultType) expression.Site {
	return expression.Site{Source: source, Profile: profile, Result: result, Purpose: expression.PurposeExpression}
}

func evaluateCompileSite(source string, profile expression.ProfileID, result expression.ResultType, context expression.CompileContext) (any, error) {
	return compilerExpressionEngine.Evaluate(compileExpressionSite(source, profile, result), expression.Values{Compile: context})
}

func reduceCompileSite(source string, profile expression.ProfileID, result expression.ResultType, context expression.CompileContext) (expression.Reduced, error) {
	return compilerExpressionEngine.Reduce(compileExpressionSite(source, profile, result), expression.Values{Compile: context})
}

func validateCompileSite(source string, profile expression.ProfileID, result expression.ResultType) error {
	_, err := compilerExpressionEngine.Validate(compileExpressionSite(source, profile, result))
	return err
}

func staticReference(source string) (string, []string, error) {
	return compilerExpressionEngine.StaticReference(compileExpressionSite(source, expression.ProfilePartialTemplate, expression.ResultString))
}

func referencesStatus(source string, profile expression.ProfileID) (bool, error) {
	return compilerExpressionEngine.ReferencesStatus(compileExpressionSite(source, profile, expression.ResultBoolean))
}

func referencesContext(source string, profile expression.ProfileID, contextName string, static bool) (bool, error) {
	return compilerExpressionEngine.ReferencesContext(compileExpressionSite(source, profile, expression.ResultString), contextName, static)
}

func siteReferencesEvent(source string, profile expression.ProfileID, result expression.ResultType, includeDerived bool) (bool, error) {
	return compilerExpressionEngine.ReferencesEvent(compileExpressionSite(source, profile, result), includeDerived)
}

func reduceCompileString(source string, context expression.CompileContext) (string, error) {
	return reduceTemplateString(source, expression.ProfileCompileTemplate, context)
}

func reduceTemplateString(source string, profile expression.ProfileID, context expression.CompileContext) (string, error) {
	reduced, err := reduceCompileSite(source, profile, expression.ResultString, context)
	if err != nil {
		return "", err
	}
	if reduced.Known {
		return reduced.Value.(string), nil
	}
	return reduced.Source, nil
}

func reducePartialTemplateString(source string, residualProfile expression.ProfileID, context expression.CompileContext) (string, error) {
	value, err := reduceTemplateString(source, expression.ProfilePartialTemplate, context)
	if err != nil {
		return "", err
	}
	if err := validateCompileSite(value, residualProfile, expression.ResultString); err != nil {
		return "", err
	}
	return value, nil
}

func compileConditionProfile(scope expression.ConditionScope) (expression.ProfileID, error) {
	switch scope {
	case expression.JobCondition:
		return expression.ProfileCompileJobCondition, nil
	case expression.StepCondition:
		return expression.ProfileCompileStepCondition, nil
	case expression.CallCondition:
		return expression.ProfileCompileCallCondition, nil
	default:
		return "", fmt.Errorf("unsupported condition scope %d", scope)
	}
}

func runtimeConditionProfile(scope expression.ConditionScope) (expression.ProfileID, error) {
	switch scope {
	case expression.JobCondition:
		return expression.ProfileJobCondition, nil
	case expression.StepCondition:
		return expression.ProfileStepCondition, nil
	case expression.CallCondition:
		return expression.ProfileCallCondition, nil
	default:
		return "", fmt.Errorf("unsupported condition scope %d", scope)
	}
}
