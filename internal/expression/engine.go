package expression

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rhysd/actionlint"
)

// ProfileID selects one closed expression surface. Callers identify where an
// expression runs; they do not assemble context or function policy.
type ProfileID string

const (
	ProfileCompile               ProfileID = "compile"
	ProfileCompileTemplate       ProfileID = "compile-template"
	ProfileCompileContainerImage ProfileID = "compile-container-image"
	ProfilePartialTemplate       ProfileID = "partial-template"
	ProfileCompileJobCondition   ProfileID = "compile-job-condition"
	ProfileCompileStepCondition  ProfileID = "compile-step-condition"
	ProfileCompileCallCondition  ProfileID = "compile-call-condition"
	ProfileReusableInput         ProfileID = "reusable-input"
	ProfileRunName               ProfileID = "run-name"
	ProfileJobCondition          ProfileID = "job-condition"
	ProfileStepCondition         ProfileID = "step-condition"
	ProfileCallCondition         ProfileID = "call-condition"
	ProfileActionLifecycle       ProfileID = "action-lifecycle"
	ProfileJobEnvironment        ProfileID = "job-environment"
	ProfileJobDefault            ProfileID = "job-default"
	ProfileJobOutput             ProfileID = "job-output"
	ProfileStepTemplate          ProfileID = "step-template"
	ProfileStepControl           ProfileID = "step-control"
	ProfileRuntimeTemplate       ProfileID = "runtime-template"
	ProfileServiceTemplate       ProfileID = "service-template"
	ProfileServiceCredential     ProfileID = "service-credential"
	ProfileServiceMap            ProfileID = "service-map"
	ProfileActionInputDefault    ProfileID = "action-input-default"
	ProfileDockerActionArg       ProfileID = "docker-action-arg"
)

// Form identifies whether a site is one expression or an interpolated
// template. Scope is the phase in which its values become available.
type Form string
type Scope string
type MissingPolicy string
type TokenPolicy string

const (
	FormExpression Form = "expression"
	FormTemplate   Form = "template"

	ScopeCompile Scope = "compile"
	ScopeJob     Scope = "job"
	ScopeStep    Scope = "step"
	ScopeAction  Scope = "action"
	ScopeCall    Scope = "call"

	MissingError MissingPolicy = "error"
	MissingEmpty MissingPolicy = "empty"
	MissingNull  MissingPolicy = "null"

	TokenDenied          TokenPolicy = "denied"
	TokenDirect          TokenPolicy = "direct"
	TokenWorkflowContext TokenPolicy = "workflow-context"
)

type ContextSet []string
type FunctionSet []string

// Profile is the immutable policy declaration for an expression surface.
type Profile struct {
	Form      Form
	Scope     Scope
	Contexts  ContextSet
	Functions FunctionSet
	Missing   MissingPolicy
	Token     TokenPolicy
	semantics profileSemantics
	condition ConditionScope
}

type profileSemantics uint8

const (
	semanticsCompile profileSemantics = iota
	semanticsCompileTemplate
	semanticsCompileStringTemplate
	semanticsPartialTemplate
	semanticsCompileCondition
	semanticsReusableInput
	semanticsRunName
	semanticsCondition
	semanticsActionLifecycle
	semanticsJobEnvironment
	semanticsJobDefault
	semanticsJobOutput
	semanticsStepTemplate
	semanticsStepControl
	semanticsRuntimeTemplate
	semanticsServiceTemplate
	semanticsServiceCredential
	semanticsServiceMap
	semanticsActionInputDefault
	semanticsDockerActionArg
)

var commonPureFunctions = FunctionSet{"case", "contains", "endsWith", "format", "fromJSON", "join", "startsWith", "toJSON"}

func profileFunctions(additional ...string) FunctionSet {
	return append(append(FunctionSet(nil), commonPureFunctions...), additional...)
}

var profiles = map[ProfileID]Profile{
	ProfileCompile:               {Form: FormExpression, Scope: ScopeCompile, Contexts: ContextSet{"event", "github", "inputs", "matrix", "strategy", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCompile},
	ProfileCompileTemplate:       {Form: FormTemplate, Scope: ScopeCompile, Contexts: ContextSet{"event", "github", "inputs", "matrix", "strategy", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCompileTemplate},
	ProfileCompileContainerImage: {Form: FormTemplate, Scope: ScopeCompile, Contexts: ContextSet{"event", "github", "inputs", "matrix", "strategy", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCompileStringTemplate},
	ProfilePartialTemplate:       {Form: FormTemplate, Scope: ScopeCompile, Contexts: ContextSet{"env", "event", "github", "inputs", "job", "jobs", "matrix", "needs", "runner", "secrets", "steps", "strategy", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsPartialTemplate},
	ProfileCompileJobCondition:   {Form: FormExpression, Scope: ScopeCompile, Contexts: ContextSet{"event", "github", "inputs", "matrix", "needs", "runner", "strategy", "vars"}, Functions: profileFunctions("always", "cancelled", "failure", "success"), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCompileCondition, condition: JobCondition},
	ProfileCompileStepCondition:  {Form: FormExpression, Scope: ScopeCompile, Contexts: ContextSet{"env", "event", "github", "inputs", "job", "matrix", "needs", "runner", "steps", "strategy", "vars"}, Functions: profileFunctions("always", "cancelled", "failure", "hashFiles", "success"), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCompileCondition, condition: StepCondition},
	ProfileCompileCallCondition:  {Form: FormExpression, Scope: ScopeCompile, Contexts: ContextSet{"event", "github", "inputs", "needs", "vars"}, Functions: profileFunctions("always", "cancelled", "failure", "success"), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCompileCondition, condition: CallCondition},
	ProfileReusableInput:         {Form: FormTemplate, Scope: ScopeCompile, Contexts: ContextSet{"github", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsReusableInput},
	ProfileRunName:               {Form: FormTemplate, Scope: ScopeCompile, Contexts: ContextSet{"github", "inputs"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsRunName},
	ProfileJobCondition:          {Form: FormExpression, Scope: ScopeJob, Contexts: ContextSet{"github", "inputs", "matrix", "needs", "runner", "vars"}, Functions: profileFunctions("always", "cancelled", "failure", "success"), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCondition, condition: JobCondition},
	ProfileStepCondition:         {Form: FormExpression, Scope: ScopeStep, Contexts: ContextSet{"env", "github", "inputs", "job", "matrix", "needs", "runner", "steps", "vars"}, Functions: profileFunctions("always", "cancelled", "failure", "hashFiles", "success"), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCondition, condition: StepCondition},
	ProfileCallCondition:         {Form: FormExpression, Scope: ScopeCall, Contexts: ContextSet{"github", "inputs", "needs", "vars"}, Functions: profileFunctions("always", "cancelled", "failure", "success"), Missing: MissingNull, Token: TokenDenied, semantics: semanticsCondition, condition: CallCondition},
	ProfileActionLifecycle:       {Form: FormExpression, Scope: ScopeAction, Contexts: ContextSet{"env", "github", "inputs", "job", "matrix", "runner", "steps"}, Functions: profileFunctions("always", "cancelled", "failure", "hashFiles", "success"), Missing: MissingNull, Token: TokenDenied, semantics: semanticsActionLifecycle, condition: actionLifecycleCondition},
	ProfileJobEnvironment:        {Form: FormTemplate, Scope: ScopeJob, Contexts: ContextSet{"github", "inputs", "matrix", "needs", "secrets", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsJobEnvironment},
	ProfileJobDefault:            {Form: FormTemplate, Scope: ScopeJob, Contexts: ContextSet{"env", "github", "inputs", "matrix", "needs", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsJobDefault},
	ProfileJobOutput:             {Form: FormTemplate, Scope: ScopeJob, Contexts: ContextSet{"env", "github", "inputs", "matrix", "needs", "runner", "secrets", "steps", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDenied, semantics: semanticsJobOutput},
	ProfileStepTemplate:          {Form: FormTemplate, Scope: ScopeStep, Contexts: ContextSet{"env", "github", "inputs", "job", "matrix", "needs", "runner", "secrets", "steps", "vars"}, Functions: profileFunctions("hashFiles"), Missing: MissingNull, Token: TokenWorkflowContext, semantics: semanticsStepTemplate},
	ProfileStepControl:           {Form: FormExpression, Scope: ScopeStep, Contexts: ContextSet{"env", "github", "inputs", "job", "matrix", "needs", "runner", "secrets", "steps", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenWorkflowContext, semantics: semanticsStepControl},
	ProfileRuntimeTemplate:       {Form: FormTemplate, Scope: ScopeStep, Contexts: ContextSet{"env", "github", "inputs", "job", "matrix", "needs", "runner", "secrets", "steps", "vars"}, Missing: MissingEmpty, Token: TokenDirect, semantics: semanticsRuntimeTemplate},
	ProfileServiceTemplate:       {Form: FormTemplate, Scope: ScopeJob, Contexts: ContextSet{"needs"}, Missing: MissingEmpty, Token: TokenDenied, semantics: semanticsServiceTemplate},
	ProfileServiceCredential:     {Form: FormTemplate, Scope: ScopeJob, Contexts: ContextSet{"env", "github", "secrets", "vars"}, Missing: MissingEmpty, Token: TokenDirect, semantics: semanticsServiceCredential},
	ProfileServiceMap:            {Form: FormExpression, Scope: ScopeJob, Contexts: ContextSet{"needs"}, Functions: FunctionSet{"fromJSON"}, Missing: MissingError, Token: TokenDenied, semantics: semanticsServiceMap},
	ProfileActionInputDefault:    {Form: FormTemplate, Scope: ScopeAction, Contexts: ContextSet{"env", "github", "inputs", "job", "matrix", "needs", "runner", "steps", "vars"}, Functions: profileFunctions(), Missing: MissingNull, Token: TokenDirect, semantics: semanticsActionInputDefault},
	ProfileDockerActionArg:       {Form: FormTemplate, Scope: ScopeAction, Contexts: ContextSet{"inputs"}, Missing: MissingEmpty, Token: TokenDenied, semantics: semanticsDockerActionArg},
}

// Profiles returns a copy of the closed profile table for exhaustive tests.
func Profiles() map[ProfileID]Profile {
	result := make(map[ProfileID]Profile, len(profiles))
	for id, profile := range profiles {
		profile.Contexts = append(ContextSet(nil), profile.Contexts...)
		profile.Functions = append(FunctionSet(nil), profile.Functions...)
		result[id] = profile
	}
	return result
}

type ResultType string

const (
	ResultAny     ResultType = "any"
	ResultString  ResultType = "string"
	ResultBoolean ResultType = "boolean"
	ResultNumber  ResultType = "number"
	ResultObject  ResultType = "object"
)

type Purpose string

const (
	PurposeExpression           Purpose = "expression"
	PurposeWorkflowActionInput  Purpose = "workflow-action-input"
	PurposeCompositeActionInput Purpose = "composite-action-input"
)

type Location struct {
	File  string
	Field string
	Span  Span
}

type Site struct {
	Source   string
	Profile  ProfileID
	Result   ResultType
	Location Location
	Purpose  Purpose
}

// Values carries concrete values without changing the policy selected by the
// site's profile.
type Values struct {
	Runtime   Context
	Condition ConditionContext
	Compile   CompileContext
}

// AbstractValues carries the compile-known subset used by authority planning.
// Reference keys are canonical names such as github.server_url.
type AbstractValues struct {
	References map[string]any
}

type Reduced struct {
	Known  bool
	Value  any
	Source string
}

type Validation struct {
	Secrets []string
}

// Engine is the only site-oriented expression entry point.
type Engine struct{}

func NewEngine() Engine { return Engine{} }

func (Engine) Parse(site Site, line, column int) (Expression, error) {
	if _, ok := profiles[site.Profile]; !ok {
		return Expression{}, siteError(site, fmt.Errorf("unknown expression profile %q", site.Profile))
	}
	parsed, err := parseExpression(site.Source, line, column)
	if err != nil {
		return Expression{}, siteError(site, err)
	}
	return parsed, nil
}

func (engine Engine) StaticReference(site Site) (string, []string, error) {
	if _, err := engine.Validate(site); err != nil {
		return "", nil, err
	}
	root, path, err := staticReferencePath(site.Source)
	if err != nil {
		return "", nil, siteError(site, err)
	}
	return root, path, nil
}

func (engine Engine) ReferencesStatus(site Site) (bool, error) {
	if _, err := engine.Validate(site); err != nil {
		return false, err
	}
	references, err := referencesStatusFunction(site.Source)
	if err != nil {
		return false, siteError(site, err)
	}
	return references, nil
}

// ReferencesContext reports any named context reference, or only static
// direct/literal-index references when static is true.
func (engine Engine) ReferencesContext(site Site, contextName string, static bool) (bool, error) {
	profile, ok := profiles[site.Profile]
	if !ok {
		return false, siteError(site, fmt.Errorf("unknown expression profile %q", site.Profile))
	}
	var found bool
	inspect := func(node actionlint.ExprNode) error {
		if static {
			found = found || usesStaticContextReference(node, contextName)
			return nil
		}
		actionlint.VisitExprNode(node, func(candidate, _ actionlint.ExprNode, entering bool) {
			if !entering || found {
				return
			}
			switch candidate.(type) {
			case *actionlint.VariableNode, *actionlint.ObjectDerefNode, *actionlint.IndexAccessNode:
			default:
				return
			}
			root, _, _ := referencePath(candidate)
			found = strings.EqualFold(root, contextName)
		})
		return nil
	}
	if profile.Form == FormExpression {
		node, empty, err := parseCondition(site.Source)
		if err != nil || empty {
			return false, siteError(site, err)
		}
		_ = inspect(node)
		return found, nil
	}
	if err := visitTemplateExpressions(site.Source, inspect); err != nil {
		return false, siteError(site, err)
	}
	return found, nil
}

// ReferencesEvent reports event-payload references. Expression profiles also
// include compiler-folded GitHub ref identity fields when includeDerived is
// true.
func (engine Engine) ReferencesEvent(site Site, includeDerived bool) (bool, error) {
	profile, ok := profiles[site.Profile]
	if !ok {
		return false, siteError(site, fmt.Errorf("unknown expression profile %q", site.Profile))
	}
	if profile.Form == FormExpression {
		node, empty, err := parseCondition(site.Source)
		if err != nil || empty {
			return false, siteError(site, err)
		}
		if includeDerived {
			return nodeReferencesCompileGitHubEvent(node), nil
		}
		return nodeReferencesGitHubEventPayload(node), nil
	}
	found := false
	err := visitTemplateExpressions(site.Source, func(node actionlint.ExprNode) error {
		actionlint.VisitExprNode(node, func(candidate, _ actionlint.ExprNode, entering bool) {
			if entering {
				call, ok := candidate.(*actionlint.FuncCallNode)
				found = found || ok && isToJSONGitHubCall(call)
			}
		})
		found = found || nodeReferencesGitHubEventPayload(node)
		return nil
	})
	return found, siteError(site, err)
}

func (Engine) Validate(site Site) (Validation, error) {
	profile, ok := profiles[site.Profile]
	if !ok {
		return Validation{}, siteError(site, fmt.Errorf("unknown expression profile %q", site.Profile))
	}
	if site.Source == "" {
		return Validation{}, nil
	}
	var secrets []string
	var err error
	if profile.Form == FormExpression {
		secrets, err = conditionSecretReferences(site.Source)
	} else {
		secrets, err = secretReferences(site.Source)
	}
	if err != nil {
		return Validation{}, siteError(site, err)
	}
	switch profile.semantics {
	case semanticsCompile:
		var node actionlint.ExprNode
		var expression Expression
		expression, err = parseExpression(site.Source, 1, 1)
		if err == nil {
			node, err = parseCompileExpression(expression)
		}
		if err == nil {
			err = validateCompileExpressionNode(node)
		}
	case semanticsCompileTemplate, semanticsCompileStringTemplate:
		err = visitTemplateExpressions(site.Source, validateCompileExpressionNode)
	case semanticsPartialTemplate:
		err = visitTemplateExpressions(site.Source, func(actionlint.ExprNode) error { return nil })
	case semanticsCompileCondition:
		var node actionlint.ExprNode
		var empty bool
		node, empty, err = parseCondition(site.Source)
		if err == nil && !empty {
			err = validateCompileConditionNode(node, profile.condition, CompileContext{}, nil)
		}
	case semanticsReusableInput:
		err = visitTemplateExpressions(site.Source, validateReusableInputDefaultNode)
	case semanticsRunName:
		err = visitTemplateExpressions(site.Source, validateRunNameNode)
	case semanticsCondition:
		err = validateCondition(site.Source, profile.condition)
	case semanticsActionLifecycle:
		err = validateLifecycleDelimiters(site.Source)
		if err == nil {
			err = validateCondition(site.Source, profile.condition)
		}
	case semanticsJobEnvironment:
		_, err = templateReferencesGitHubToken(site.Source)
		if err == nil {
			err = validateStepProfile(site.Source, profile)
		}
	case semanticsJobDefault:
		_, err = templateReferencesGitHubToken(site.Source)
		if err == nil {
			err = validateStepProfile(site.Source, profile)
		}
	case semanticsJobOutput:
		_, err = templateReferencesGitHubToken(site.Source)
		if err == nil {
			err = validateStepProfile(site.Source, profile)
		}
	case semanticsStepTemplate:
		err = validateStepProfile(site.Source, profile)
	case semanticsStepControl:
		var node actionlint.ExprNode
		node, err = parseCompleteExpression(site.Source)
		if err == nil {
			err = validateStepRuntimeExpression(node, profileAllowsFunction(profile, "hashFiles"), profile.Token != TokenDenied, stepProfileContextMap(profile))
		}
	case semanticsRuntimeTemplate:
		err = visitTemplateExpressions(site.Source, validateRuntimeReferenceNode)
	case semanticsServiceTemplate:
		err = visitTemplateExpressions(site.Source, validateServiceRuntimeNode)
	case semanticsServiceCredential:
		err = visitTemplateExpressions(site.Source, validateServiceCredentialNode)
	case semanticsServiceMap:
		err = validateServiceMapExpression(site.Source)
	case semanticsActionInputDefault:
		err = validateActionInputDefaultTemplate(site.Source)
	case semanticsDockerActionArg:
		err = visitTemplateExpressions(site.Source, validateDockerActionArgNode)
	}
	if err == nil {
		err = validateDeclaredProfile(site.Source, profile)
	}
	if err != nil {
		return Validation{}, siteError(site, err)
	}
	return Validation{Secrets: secrets}, nil
}

func validateStepProfile(source string, profile Profile) error {
	return visitTemplateExpressions(source, func(node actionlint.ExprNode) error {
		return validateStepRuntimeExpression(node, profileAllowsFunction(profile, "hashFiles"), profile.Token != TokenDenied, stepProfileContextMap(profile))
	})
}

func stepProfileContextMap(profile Profile) map[string]bool {
	if profile.semantics == semanticsStepTemplate || profile.semantics == semanticsStepControl {
		return nil
	}
	return profileContextMap(profile)
}

func parseCompleteExpression(source string) (actionlint.ExprNode, error) {
	body, err := expressionBody(source)
	if err != nil {
		return nil, err
	}
	node, parseErr := actionlint.NewExprParser().Parse(actionlint.NewExprLexer(body + "}}"))
	if parseErr != nil {
		return nil, fmt.Errorf("invalid expression: %w", parseErr)
	}
	return node, nil
}

func profileContextMap(profile Profile) map[string]bool {
	contexts := make(map[string]bool, len(profile.Contexts))
	for _, context := range profile.Contexts {
		contexts[strings.ToLower(context)] = true
	}
	return contexts
}

func profileAllowsFunction(profile Profile, name string) bool {
	for _, function := range profile.Functions {
		if strings.EqualFold(function, name) {
			return true
		}
	}
	return false
}

// validateDeclaredProfile makes the closed table the outer policy boundary.
// Surface-specific validators may further restrict member shapes, but cannot
// admit a context or function omitted by the profile declaration.
func validateDeclaredProfile(source string, profile Profile) error {
	contexts := profileContextMap(profile)
	validate := func(node actionlint.ExprNode) error {
		var validationErr error
		actionlint.VisitExprNode(node, func(current, _ actionlint.ExprNode, entering bool) {
			if !entering || validationErr != nil {
				return
			}
			if call, ok := current.(*actionlint.FuncCallNode); ok {
				if !profileAllowsFunction(profile, call.Callee) {
					validationErr = fmt.Errorf("expression function %q is unavailable in this profile", call.Callee)
					return
				}
				if isToJSONGitHubCall(call) && (profile.Token == TokenDenied || profile.Token == TokenDirect) {
					validationErr = fmt.Errorf("whole github context is unavailable in this profile")
					return
				}
			}
			root, path, referenceErr := referencePath(current)
			if referenceErr == nil && strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "token") && profile.Token == TokenDenied {
				validationErr = fmt.Errorf("github.token is unavailable in this profile")
				return
			}
			variable, ok := current.(*actionlint.VariableNode)
			if ok && !contexts[strings.ToLower(variable.Name)] {
				validationErr = fmt.Errorf("expression context %q is unavailable in this profile", variable.Name)
			}
		})
		return validationErr
	}
	if profile.Form == FormExpression {
		node, empty, err := parseCondition(source)
		if err != nil || empty {
			return err
		}
		return validate(node)
	}
	return visitTemplateExpressions(source, validate)
}

func (engine Engine) Evaluate(site Site, values Values) (any, error) {
	if _, err := engine.Validate(site); err != nil {
		return nil, err
	}
	if site.Source == "" {
		profile := profiles[site.Profile]
		if profile.semantics == semanticsCondition && values.Condition.Unsuccessful {
			return false, nil
		}
		if profile.Form == FormExpression && site.Result == ResultBoolean {
			return true, nil
		}
		return zeroResult(site.Result), nil
	}
	var value any
	var err error
	profile := profiles[site.Profile]
	switch profile.semantics {
	case semanticsCompile:
		var expression Expression
		expression, err = parseExpression(site.Source, 1, 1)
		if err == nil {
			value, err = evaluateCompile(expression, values.Compile)
		}
	case semanticsCompileTemplate:
		value, err = evaluateCompileTemplate(site.Source, values.Compile)
	case semanticsCompileStringTemplate:
		value, err = evaluateCompileStringTemplate(site.Source, values.Compile)
	case semanticsPartialTemplate:
		value, err = evaluateAvailableCompileTemplate(site.Source, values.Compile)
	case semanticsCompileCondition:
		value, err = evaluateCompileCondition(site.Source, values.Compile)
	case semanticsReusableInput:
		if expression, parseErr := parseExpression(site.Source, 1, 1); parseErr == nil {
			value, err = evaluateCompile(expression, values.Compile)
		} else {
			value, err = evaluateCompileTemplate(site.Source, values.Compile)
		}
	case semanticsRunName:
		value, err = evaluateCompileTemplate(site.Source, values.Compile)
	case semanticsCondition:
		value, err = evaluateProfileCondition(site.Source, values.Condition, true)
	case semanticsActionLifecycle:
		value, err = evaluateProfileCondition(site.Source, values.Condition, false)
	case semanticsJobEnvironment:
		value, err = evaluateStepProfile(site.Source, values.Runtime, profile)
	case semanticsJobDefault:
		value, err = evaluateStepProfile(site.Source, values.Runtime, profile)
	case semanticsJobOutput:
		value, err = evaluateStepProfile(site.Source, values.Runtime, profile)
	case semanticsStepTemplate:
		value, err = evaluateStepProfile(site.Source, values.Runtime, profile)
	case semanticsStepControl:
		var node actionlint.ExprNode
		node, err = parseCompleteExpression(site.Source)
		if err == nil {
			value, err = evaluateStepRuntimeExpression(node, values.Runtime, profileAllowsFunction(profile, "hashFiles"), profile.Token != TokenDenied, stepProfileContextMap(profile))
		}
	case semanticsRuntimeTemplate, semanticsServiceTemplate, semanticsServiceCredential:
		value, err = evaluateRuntimeTemplate(site.Source, values.Runtime, evaluateDirectRuntimeNode)
	case semanticsServiceMap:
		value, err = evaluateRuntimeObject(site.Source, values.Runtime)
	case semanticsActionInputDefault:
		value, err = evaluateRuntimeTemplate(site.Source, values.Runtime, evaluateActionInputDefaultNode)
	case semanticsDockerActionArg:
		value, err = evaluateRuntimeTemplate(site.Source, Context{Inputs: values.Runtime.Inputs}, func(node actionlint.ExprNode, context Context) (any, error) {
			root, path, _ := referencePath(node)
			return resolveRuntimeReference(root, path, context)
		})
	default:
		err = fmt.Errorf("unknown expression profile %q", site.Profile)
	}
	if err != nil {
		return nil, siteError(site, err)
	}
	if !matchesEngineResult(value, site.Result) {
		return nil, siteError(site, fmt.Errorf("expression produced %T, want %s", value, site.Result))
	}
	return value, nil
}

func evaluateStepProfile(source string, context Context, profile Profile) (string, error) {
	return evaluateRuntimeTemplate(source, context, func(node actionlint.ExprNode, context Context) (any, error) {
		return evaluateStepRuntimeExpression(node, context, profileAllowsFunction(profile, "hashFiles"), profile.Token != TokenDenied, stepProfileContextMap(profile))
	})
}

func evaluateProfileCondition(source string, context ConditionContext, implicitSuccess bool) (bool, error) {
	node, empty, err := parseCondition(source)
	if err != nil {
		return false, err
	}
	if empty {
		if implicitSuccess {
			return !context.Unsuccessful && !context.Cancelled, nil
		}
		return true, nil
	}
	if implicitSuccess && !containsStatusFunction(node) && (context.Unsuccessful || context.Cancelled) {
		return false, nil
	}
	value, err := evaluateConditionNode(node, context)
	if err != nil {
		return false, err
	}
	return githubTruthy(value), nil
}

func (engine Engine) Reduce(site Site, values Values) (Reduced, error) {
	profile, ok := profiles[site.Profile]
	if !ok {
		return Reduced{}, siteError(site, fmt.Errorf("unknown expression profile %q", site.Profile))
	}
	if site.Source == "" {
		value, err := engine.Evaluate(site, values)
		return Reduced{Known: true, Value: value}, err
	}
	if profile.semantics == semanticsCompile {
		if _, err := engine.Validate(site); err != nil {
			return Reduced{}, err
		}
		expression, err := parseExpression(site.Source, 1, 1)
		if err != nil {
			return Reduced{}, siteError(site, err)
		}
		value, available, err := evaluateCompileAvailable(expression, values.Compile)
		if err != nil {
			return Reduced{}, siteError(site, err)
		}
		if available {
			if !matchesEngineResult(value, site.Result) {
				return Reduced{}, siteError(site, fmt.Errorf("expression produced %T, want %s", value, site.Result))
			}
			return Reduced{Known: true, Value: value}, nil
		}
		return Reduced{Source: site.Source}, nil
	}
	if profile.semantics == semanticsCompileCondition {
		if _, err := engine.Validate(site); err != nil {
			return Reduced{}, err
		}
		reduced, err := reduceCompileCondition(site.Source, values.Compile)
		if err != nil {
			return Reduced{}, siteError(site, err)
		}
		if value, err := evaluateCompileCondition(reduced, values.Compile); err == nil {
			return Reduced{Known: true, Value: value}, nil
		}
		return Reduced{Source: reduced}, nil
	}
	if profile.semantics == semanticsCompileStringTemplate {
		value, err := engine.Evaluate(site, values)
		return Reduced{Known: err == nil, Value: value}, err
	}
	if profile.semantics == semanticsCompileTemplate || profile.semantics == semanticsPartialTemplate {
		if _, err := engine.Validate(site); err != nil {
			return Reduced{}, err
		}
		reduced, err := reduceAvailableCompileTemplate(site.Source, values.Compile)
		if err != nil {
			return Reduced{}, siteError(site, err)
		}
		reduced, err = evaluateAvailableCompileTemplate(reduced, values.Compile)
		if err != nil {
			return Reduced{}, siteError(site, err)
		}
		if !strings.Contains(reduced, "${{") {
			return Reduced{Known: true, Value: reduced}, nil
		}
		return Reduced{Source: reduced}, nil
	}
	if profile.Scope == ScopeCompile {
		value, err := engine.Evaluate(site, values)
		return Reduced{Known: err == nil, Value: value}, err
	}
	if profile.Form == FormExpression {
		reduced, err := reduceCompileCondition(site.Source, values.Compile)
		if err != nil {
			return Reduced{}, siteError(site, err)
		}
		if profile.semantics == semanticsStepControl {
			node, empty, parseErr := parseCondition(reduced)
			if parseErr != nil {
				return Reduced{}, siteError(site, parseErr)
			}
			if !empty {
				if value, available, evaluateErr := evaluateCompileNodeAvailable(node, values.Compile); evaluateErr == nil && available {
					if !matchesEngineResult(value, site.Result) {
						return Reduced{}, siteError(site, fmt.Errorf("expression produced %T, want %s", value, site.Result))
					}
					return Reduced{Known: true, Value: value}, nil
				}
			}
		} else if value, evaluateErr := evaluateCompileCondition(reduced, values.Compile); evaluateErr == nil {
			return Reduced{Known: true, Value: value}, nil
		}
		residual := site
		residual.Source = reduced
		if profile.semantics == semanticsStepControl || profile.semantics == semanticsServiceMap {
			residual.Source = "${{ " + reduced + " }}"
		}
		if _, err := engine.Validate(residual); err != nil {
			return Reduced{}, err
		}
		return Reduced{Source: residual.Source}, nil
	}
	reduced, err := reduceAvailableCompileTemplate(site.Source, values.Compile)
	if err != nil {
		return Reduced{}, siteError(site, err)
	}
	residual := site
	residual.Source = reduced
	if _, err := engine.Validate(residual); err != nil {
		return Reduced{}, err
	}
	if !strings.Contains(reduced, "${{") {
		return Reduced{Known: true, Value: reduced}, nil
	}
	return Reduced{Source: reduced}, nil
}

func (engine Engine) Analyze(site Site, values AbstractValues) (Analysis, error) {
	profile, ok := profiles[site.Profile]
	if !ok {
		return Analysis{}, siteError(site, fmt.Errorf("unknown expression profile %q", site.Profile))
	}
	if _, err := engine.Validate(site); err != nil {
		return Analysis{}, err
	}
	if site.Source == "" {
		if profiles[site.Profile].Form == FormExpression && site.Result == ResultBoolean {
			return knownAnalysis(true), nil
		}
		return knownAnalysis(zeroResult(site.Result)), nil
	}
	// Apply the profile's static authority grammar before reachability. This
	// keeps prohibited dynamic and whole-context access exhaustive even when a
	// lazy branch would not execute.
	var staticErr error
	switch {
	case profile.Token == TokenWorkflowContext:
		_, staticErr = stepReferencesGitHubToken(site.Source)
	case profile.Form == FormExpression:
		_, staticErr = conditionReferencesGitHubToken(site.Source)
	default:
		_, staticErr = templateReferencesGitHubToken(site.Source)
	}
	if staticErr != nil {
		return Analysis{}, siteError(site, staticErr)
	}

	knownReferences := canonicalAbstractReferences(values.References)
	analyzeNode := func(node actionlint.ExprNode) (Analysis, error) {
		if profile.semantics == semanticsActionInputDefault {
			return analyzeActionInputDefault(node, knownReferences)
		}
		return analyzeRuntimeNode(node, knownReferences, site.Purpose)
	}
	analysis := Analysis{}
	var err error
	if profile.Form == FormExpression {
		var node actionlint.ExprNode
		var empty bool
		node, empty, err = parseCondition(site.Source)
		if err == nil && !empty {
			switch profile.semantics {
			case semanticsCondition, semanticsActionLifecycle, semanticsCompileCondition:
				analysis, err = analyzeConditionNode(node, knownReferences)
			default:
				analysis, err = analyzeNode(node)
			}
		}
	} else {
		analysis, err = analyzeTemplate(site.Source, analyzeNode)
	}
	if err != nil {
		return Analysis{}, siteError(site, err)
	}
	return analysis, nil
}

func (Engine) Literal(value any) (string, error) {
	return compileInputLiteral(value)
}

func (Engine) Truthy(value any) bool {
	return githubTruthy(value)
}

func canonicalAbstractReferences(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for name, value := range source {
		result[strings.ToLower(name)] = value
	}
	return result
}

func matchesEngineResult(value any, result ResultType) bool {
	if result == "" || result == ResultAny {
		return true
	}
	switch result {
	case ResultString:
		_, ok := value.(string)
		return ok
	case ResultBoolean:
		_, ok := value.(bool)
		return ok
	case ResultNumber:
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			return true
		}
	case ResultObject:
		_, ok := value.([]ObjectEntry)
		return ok
	}
	return false
}

func zeroResult(result ResultType) any {
	switch result {
	case ResultString:
		return ""
	case ResultBoolean:
		return false
	case ResultNumber:
		return float64(0)
	case ResultObject:
		return []ObjectEntry(nil)
	default:
		return nil
	}
}

func siteError(site Site, err error) error {
	if err == nil || site.Location.File == "" {
		return err
	}
	position := site.Location.Span.Start
	if position.Line == 0 {
		return fmt.Errorf("%s: %w", site.Location.File, err)
	}
	return fmt.Errorf("%s:%d:%d: %s: %w", site.Location.File, position.Line, position.Column, site.Location.Field, err)
}
