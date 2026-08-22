package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// planBuilder lowers expanded job instances into validated execution-plan
// contracts and derives the authorization metadata emitted beside each plan.
type planBuilder struct {
	ctx                        context.Context
	ir                         IR
	compilerVersion            string
	compilerDistributionDigest string
	options                    Options
	actionSource               ActionSource
	workflowName               string
	eventDigest                [sha256.Size]byte
	planDigests                map[string]string
}

type builtPlanActions struct {
	locks               []plan.ActionLock
	capabilities        []string
	authorization       PlanAuthorization
	requiredSecrets     []string
	inputsInspected     bool
	requiresGitHubToken bool
	requiresMise        bool
	resolved            bool
}

var (
	errRetainedGitHubEvent  = errors.New("github.event cannot be retained in a job plan")
	errEventActionReference = errors.New("github.event cannot select an action reference")
)

func compilePlansWithAuthorization(ctx context.Context, ir IR, compilerVersion, compilerDistributionDigest string, options Options) ([]plan.Job, []PlanAuthorization, []JobEvaluation, error) {
	payload, err := json.Marshal(ir.Event.Payload)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode event payload: %w", err)
	}
	workflowName := ir.Workflow.Name
	if workflowName == "" {
		workflowName = canonicalWorkflowName(ir.Workflow.Path)
	}
	builder := planBuilder{
		ctx: ctx, ir: ir, compilerVersion: compilerVersion, compilerDistributionDigest: compilerDistributionDigest,
		options: options, actionSource: newMemoizedActionSource(options.ActionSource), workflowName: workflowName,
		eventDigest: sha256.Sum256(payload), planDigests: make(map[string]string, len(ir.Jobs)),
	}
	plans := make([]plan.Job, 0, len(ir.Jobs))
	authorizations := make([]PlanAuthorization, 0, len(ir.Jobs))
	evaluations := make([]JobEvaluation, 0, len(ir.Jobs))
	failedInstances := make(map[string]bool, len(ir.Jobs))
	var diagnostics []error
instances:
	for _, instance := range ir.Jobs {
		for _, dependency := range instance.Needs {
			if failedInstances[dependency] {
				failedInstances[instance.Key] = true
				evaluations = append(evaluations, JobEvaluation{Instance: instance.Key, Job: instance.LogicalJobID})
				continue instances
			}
		}
		runtimeDistributionDigest := options.RuntimeDistributions[instance.Platform]
		if runtimeDistributionDigest == "" && instance.Platform == PlatformLinuxAMD64 {
			runtimeDistributionDigest = compilerDistributionDigest
		}
		job, authorization, encoded, err := builder.buildPlan(instance, runtimeDistributionDigest)
		if err != nil {
			failedInstances[instance.Key] = true
			evaluations = append(evaluations, JobEvaluation{Instance: instance.Key, Job: instance.LogicalJobID, Evaluated: true})
			diagnostics = append(diagnostics, planConstructionFinding(instance, err))
			continue
		}
		digest := sha256.Sum256(encoded)
		builder.planDigests[instance.Key] = "sha256:" + hex.EncodeToString(digest[:])
		plans = append(plans, job)
		authorizations = append(authorizations, authorization)
		evaluations = append(evaluations, JobEvaluation{Instance: instance.Key, Job: instance.LogicalJobID, Evaluated: true, Passed: true})
	}
	return plans, authorizations, evaluations, errors.Join(diagnostics...)
}

// reducePlanEventExpressions is the phase boundary for compile-time-only event
// data. It runs before action inspection, authority inventory, and plan
// serialization, so every downstream consumer sees the same reduced values.
func reducePlanEventExpressions(ir IR) (IR, error) {
	workflowName := ir.Workflow.Name
	if workflowName == "" {
		workflowName = canonicalWorkflowName(ir.Workflow.Path)
	}
	builder := planBuilder{ir: ir, workflowName: workflowName}
	ir.Jobs = append([]JobInstance(nil), ir.Jobs...)
	var diagnostics []error
	for i, instance := range ir.Jobs {
		reduced, err := builder.reducePlanInstanceEventExpressions(instance)
		if err != nil {
			diagnostics = append(diagnostics, planConstructionFinding(instance, err))
			continue
		}
		ir.Jobs[i] = reduced
	}
	return ir, errors.Join(diagnostics...)
}

func (b planBuilder) buildPlan(instance JobInstance, runtimeDistributionDigest string) (plan.Job, PlanAuthorization, []byte, error) {
	if runtimeDistributionDigest == "" {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("build plan for job %q: no runtime distribution configured for %s", instance.LogicalJobID, instance.Platform)
	}
	steps, actionIndexes, actionRefs, actionInputs := lowerPlanSteps(instance.Steps)
	actions, err := b.buildActions(instance, steps, actionIndexes, actionRefs, actionInputs)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	if err := addContainerCapabilities(instance, actionRefs, &actions); err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	needSources, err := buildPlanNeedSources(instance, b.planDigests)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	deferredInputs, err := buildPlanDeferredInputs(instance.DeferredInputs, b.planDigests)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	callGuards, err := buildPlanCallGuards(instance, b.planDigests)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	secrets, githubToken, err := b.authorizePlanSecrets(instance, &actions)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	if len(secrets) != 0 {
		actions.capabilities = append(actions.capabilities, "secrets")
	}
	sort.Strings(actions.capabilities)
	actions.capabilities = slices.Compact(actions.capabilities)
	if instance.Platform == PlatformDarwinARM64 && slices.Contains(actions.capabilities, "docker") {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("%s:%d:%d: job %q requires Docker, which is unavailable on darwin/arm64", instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column, instance.LogicalJobID)
	}
	job := b.lowerPlanJob(instance, runtimeDistributionDigest, steps, actions, needSources, buildPlanNeedOutputs(instance), deferredInputs, callGuards, secrets, githubToken)
	if err := job.Validate(); err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	encoded, err := plan.Encode(job)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("encode plan for job %q: %w", instance.LogicalJobID, err)
	}
	return job, actions.authorization, encoded, nil
}

func (b planBuilder) reducePlanInstanceEventExpressions(instance JobInstance) (JobInstance, error) {
	context := compileContext(b.ir.Event, b.ir.Vars, instance.SourcePath, b.workflowName)
	context.Inputs = instance.Inputs
	context.Matrix = instance.Matrix

	reduceTemplateValue := func(value string) (string, error) {
		if value == "" {
			return value, nil
		}
		reduced, err := expression.ReduceAvailableCompileTemplate(value, context)
		if err != nil {
			return "", err
		}
		referencesEvent, err := expression.TemplateReferencesGitHubEvent(reduced)
		if err != nil {
			return "", err
		}
		referencesEventAlias, err := expression.TemplateUsesContext(reduced, "event")
		if err != nil {
			return "", err
		}
		if referencesEvent || referencesEventAlias {
			return "", errRetainedGitHubEvent
		}
		return reduced, nil
	}
	reduceConditionValue := func(value string) (string, error) {
		if value == "" {
			return value, nil
		}
		referencesEvent, err := expression.ReferencesGitHubEvent(value)
		if err != nil {
			return "", err
		}
		if !referencesEvent {
			return value, nil
		}
		reduced, err := expression.ReduceCompileCondition(value, context)
		if err != nil {
			return "", err
		}
		referencesEvent, err = expression.ReferencesGitHubEvent(reduced)
		if err != nil {
			return "", err
		}
		if referencesEvent {
			return "", errRetainedGitHubEvent
		}
		return reduced, nil
	}
	reduceTemplate := func(value, field string, position workflow.Position, step int) (string, error) {
		reduced, err := reduceTemplateValue(value)
		if err != nil {
			return "", planExpressionFinding(instance, instance.SourcePath, field, value, position, step, err)
		}
		return reduced, nil
	}
	reduceCondition := func(value, field string, position workflow.Position, step int) (string, error) {
		reduced, err := reduceConditionValue(value)
		if err != nil {
			return "", planExpressionFinding(instance, instance.SourcePath, field, value, position, step, err)
		}
		return reduced, nil
	}
	reduceTypedExpression := func(value, field string, position workflow.Position, step int) (string, error) {
		referencesEvent, err := expression.ReferencesGitHubEvent(value)
		if err != nil {
			return "", planExpressionFinding(instance, instance.SourcePath, field, value, position, step, err)
		}
		if !referencesEvent {
			return value, nil
		}
		reduced, err := reduceCondition(value, field, position, step)
		if err != nil {
			return "", err
		}
		return "${{ " + reduced + " }}", nil
	}
	reduceMap := func(values map[string]string, field, positionPrefix string, positions map[string]workflow.Position, fallback workflow.Position, step int) (map[string]string, error) {
		if values == nil {
			return nil, nil
		}
		result := cloneMap(values)
		for _, name := range sortedValueKeys(result) {
			value, err := reduceTemplate(result[name], fmt.Sprintf("%s.%s", field, name), sourcePosition(positions, positionPrefix+"."+name, fallback), step)
			if err != nil {
				return nil, err
			}
			result[name] = value
		}
		return result, nil
	}
	reduceSlice := func(values []string, field, positionPrefix string, positions map[string]workflow.Position, fallback workflow.Position) ([]string, error) {
		if values == nil {
			return nil, nil
		}
		result := append([]string(nil), values...)
		for i := range result {
			value, err := reduceTemplate(result[i], fmt.Sprintf("%s[%d]", field, i), sourcePosition(positions, fmt.Sprintf("%s[%d]", positionPrefix, i), fallback), 0)
			if err != nil {
				return nil, err
			}
			result[i] = value
		}
		return result, nil
	}

	var err error
	if instance.If, err = reduceCondition(instance.If, "job.if", sourcePosition(instance.Positions, "if", instance.Source.Start), 0); err != nil {
		return JobInstance{}, err
	}
	jobFields := []struct {
		name     string
		position string
		value    *string
	}{
		{"job.defaults.run.shell", "defaults.run.shell", &instance.DefaultShell},
		{"job.defaults.run.working-directory", "defaults.run.working-directory", &instance.DefaultWorkingDirectory},
		{"job.services", "services", &instance.ServicesExpression},
	}
	for _, field := range jobFields {
		if *field.value, err = reduceTemplate(*field.value, field.name, sourcePosition(instance.Positions, field.position, instance.Source.Start), 0); err != nil {
			return JobInstance{}, err
		}
	}
	if instance.Env, err = reduceMap(instance.Env, "job.env", "env", instance.Positions, instance.Source.Start, 0); err != nil {
		return JobInstance{}, err
	}
	if instance.Outputs, err = reduceMap(instance.Outputs, "job.outputs", "outputs", instance.Positions, instance.Source.Start, 0); err != nil {
		return JobInstance{}, err
	}

	instance.CallGuards = append([]CallGuard(nil), instance.CallGuards...)
	for i := range instance.CallGuards {
		guard := &instance.CallGuards[i]
		reduced, guardErr := reduceConditionValue(guard.Condition)
		if guardErr != nil {
			path := guard.SourcePath
			if path == "" {
				path = instance.SourcePath
			}
			position := guard.Position
			if position.Line == 0 || position.Column == 0 {
				position = instance.Source.Start
			}
			return JobInstance{}, planExpressionFinding(instance, path, fmt.Sprintf("job.call-guards[%d]", i), guard.Condition, position, 0, guardErr)
		}
		guard.Condition = reduced
	}

	instance.Steps = append([]workflow.Step(nil), instance.Steps...)
	for i := range instance.Steps {
		step := &instance.Steps[i]
		field := fmt.Sprintf("steps[%d]", i+1)
		if step.If, err = reduceCondition(step.If, field+".if", sourcePosition(step.Positions, "if", step.IfSpan.Start), i+1); err != nil {
			return JobInstance{}, err
		}
		stepTemplates := []struct {
			name  string
			value *string
		}{{"name", &step.Name}, {"run", &step.Run}, {"shell", &step.Shell}, {"working-directory", &step.WorkingDirectory}}
		for _, template := range stepTemplates {
			if *template.value, err = reduceTemplate(*template.value, field+"."+template.name, sourcePosition(step.Positions, template.name, step.Span.Start), i+1); err != nil {
				return JobInstance{}, err
			}
		}
		typedExpressions := []struct {
			name  string
			value *string
		}{{"continue-on-error", &step.ContinueOnErrorExpression}, {"timeout-minutes", &step.TimeoutMinutesExpression}}
		for _, typed := range typedExpressions {
			if *typed.value, err = reduceTypedExpression(*typed.value, field+"."+typed.name, sourcePosition(step.Positions, typed.name, step.Span.Start), i+1); err != nil {
				return JobInstance{}, err
			}
		}
		if step.Env, err = reduceMap(step.Env, field+".env", "env", step.Positions, step.Span.Start, i+1); err != nil {
			return JobInstance{}, err
		}
		if step.With, err = reduceMap(step.With, field+".with", "with", step.Positions, step.Span.Start, i+1); err != nil {
			return JobInstance{}, err
		}
		referencesEvent, referencesErr := expression.TemplateReferencesGitHubEvent(step.Uses)
		if referencesErr == nil && !referencesEvent {
			referencesEvent, referencesErr = expression.TemplateUsesContext(step.Uses, "event")
		}
		if referencesErr != nil {
			return JobInstance{}, planExpressionFinding(instance, instance.SourcePath, field+".uses", step.Uses, sourcePosition(step.Positions, "uses", step.Span.Start), i+1, referencesErr)
		}
		if referencesEvent {
			return JobInstance{}, planExpressionFinding(instance, instance.SourcePath, field+".uses", step.Uses, sourcePosition(step.Positions, "uses", step.Span.Start), i+1, errEventActionReference)
		}
	}

	if instance.Container != nil {
		container := *instance.Container
		if container.Image, err = reduceTemplate(container.Image, "job.container.image", sourcePosition(container.Positions, "image", container.Span.Start), 0); err != nil {
			return JobInstance{}, err
		}
		if container.Env, err = reduceMap(container.Env, "job.container.env", "env", container.Positions, container.Span.Start, 0); err != nil {
			return JobInstance{}, err
		}
		if container.Ports, err = reduceSlice(container.Ports, "job.container.ports", "ports", container.Positions, container.Span.Start); err != nil {
			return JobInstance{}, err
		}
		instance.Container = &container
	}

	instance.Services = append([]workflow.Service(nil), instance.Services...)
	for i := range instance.Services {
		container := instance.Services[i].Container
		field := fmt.Sprintf("job.services.%s", instance.Services[i].Name)
		serviceFields := []struct {
			name  string
			value *string
		}{{"image", &container.Image}, {"options", &container.Options}, {"command", &container.Command}, {"entrypoint", &container.Entrypoint}}
		for _, serviceField := range serviceFields {
			if *serviceField.value, err = reduceTemplate(*serviceField.value, field+"."+serviceField.name, sourcePosition(container.Positions, serviceField.name, container.Span.Start), 0); err != nil {
				return JobInstance{}, err
			}
		}
		if container.Env, err = reduceMap(container.Env, field+".env", "env", container.Positions, container.Span.Start, 0); err != nil {
			return JobInstance{}, err
		}
		if container.Ports, err = reduceSlice(container.Ports, field+".ports", "ports", container.Positions, container.Span.Start); err != nil {
			return JobInstance{}, err
		}
		if container.Volumes, err = reduceSlice(container.Volumes, field+".volumes", "volumes", container.Positions, container.Span.Start); err != nil {
			return JobInstance{}, err
		}
		if container.Credentials != nil {
			credentials := *container.Credentials
			if credentials.Username, err = reduceTemplate(credentials.Username, field+".credentials.username", sourcePosition(container.Positions, "credentials.username", container.Span.Start), 0); err != nil {
				return JobInstance{}, err
			}
			if credentials.Password, err = reduceTemplate(credentials.Password, field+".credentials.password", sourcePosition(container.Positions, "credentials.password", container.Span.Start), 0); err != nil {
				return JobInstance{}, err
			}
			container.Credentials = &credentials
		}
		instance.Services[i].Container = container
	}
	return instance, nil
}

func lowerPlanSteps(source []workflow.Step) ([]plan.Step, []int, []string, []map[string]string) {
	steps := make([]plan.Step, len(source))
	var actionIndexes []int
	var actionRefs []string
	var actionInputs []map[string]string
	usedIDs := make(map[string]struct{}, len(source))
	for _, step := range source {
		if step.ID != "" {
			usedIDs[strings.ToLower(step.ID)] = struct{}{}
		}
	}
	for i, step := range source {
		id := step.ID
		if id == "" {
			id = fmt.Sprintf("step-%d", i+1)
			for suffix := 2; ; suffix++ {
				if _, exists := usedIDs[strings.ToLower(id)]; !exists {
					break
				}
				id = fmt.Sprintf("step-%d-%d", i+1, suffix)
			}
			usedIDs[strings.ToLower(id)] = struct{}{}
		}
		span := planSpan(step.Span)
		steps[i] = plan.Step{
			ID: id, Name: step.Name, Kind: step.Kind, Background: step.Background, Targets: append([]string(nil), step.Targets...), Command: step.Run, Uses: step.Uses,
			Shell: step.Shell, WorkingDirectory: step.WorkingDirectory,
			Env: cloneMap(step.Env), With: cloneMap(step.With), Condition: step.If,
			ContinueOnError: step.ContinueOnError, ContinueOnErrorExpression: step.ContinueOnErrorExpression,
			TimeoutMinutes: step.TimeoutMinutes, TimeoutMinutesExpression: step.TimeoutMinutesExpression, Source: &span,
		}
		if step.Kind == "uses" {
			actionIndexes = append(actionIndexes, i)
			actionRefs = append(actionRefs, step.Uses)
			actionInputs = append(actionInputs, step.With)
		}
	}
	return steps, actionIndexes, actionRefs, actionInputs
}

func (b planBuilder) buildActions(instance JobInstance, steps []plan.Step, actionIndexes []int, actionRefs []string, actionInputs []map[string]string) (builtPlanActions, error) {
	built := builtPlanActions{requiresMise: len(actionRefs) != 0, resolved: b.options.ResolveActions}
	if len(actionRefs) != 0 && !built.resolved {
		built.resolved = true
		for _, ref := range actionRefs {
			if !strings.HasPrefix(ref, "./") {
				built.resolved = false
				break
			}
		}
	}
	if !built.resolved || len(actionRefs) == 0 {
		capabilities, err := requiredCapabilities(instance.RepositoryRoot, instance.SourcePath, instance.Steps)
		if err != nil {
			return built, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
		}
		built.capabilities = capabilities
		return built, nil
	}
	compiled, err := compileActionInvocations(b.ctx, instance.RepositoryRoot, b.actionSource, plan.EventServerURL(b.ir.Event.Provider), actionRefs, actionInputs)
	if err != nil {
		return built, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	built.requiresMise = compiled.requiresMise
	built.requiresGitHubToken = compiled.requiresGitHubToken
	built.requiredSecrets = compiled.requiredSecrets
	built.authorization.GitHubTokenActions = append([]string(nil), compiled.githubTokenActions...)
	built.inputsInspected = true
	locksByID := make(map[string]plan.ActionLock, len(compiled.locks))
	for _, lock := range compiled.locks {
		locksByID[lock.ID] = lock
	}
	for i, selector := range compiled.selectors {
		stepIndex := actionIndexes[i]
		steps[stepIndex].Action = &plan.ActionSelector{Lock: selector.Lock}
		lock, ok := locksByID[selector.Lock]
		if !ok {
			return built, fmt.Errorf("build plan for job %q: action lock %q is missing", instance.LogicalJobID, selector.Lock)
		}
		if err := b.validateActionAdapter(instance, stepIndex, lock, &built); err != nil {
			return built, err
		}
	}
	built.locks = compiled.locks
	built.capabilities = append(append([]string{}, built.capabilities...), compiled.capabilities...)
	sort.Strings(built.capabilities)
	built.capabilities = slices.Compact(built.capabilities)
	sort.Strings(built.authorization.ProviderTokenReadCapabilitySources)
	built.authorization.ProviderTokenReadCapabilitySources = slices.Compact(built.authorization.ProviderTokenReadCapabilitySources)
	if slices.Contains(compiled.capabilities, "docker") {
		built.authorization.DockerCapabilitySources = append(built.authorization.DockerCapabilitySources, "dockerfile-actions")
	}
	return built, nil
}

func (b planBuilder) validateActionAdapter(instance JobInstance, stepIndex int, lock plan.ActionLock, built *builtPlanActions) error {
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	switch descriptor.Adapter {
	case actionintegration.AdapterCheckoutExactEventSHA:
		checkoutInputs := cloneMap(instance.Steps[stepIndex].With)
		for name, value := range checkoutInputs {
			if !strings.EqualFold(name, "ref") {
				continue
			}
			root, path, err := expression.ReferencePath(value)
			if err == nil && (strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "sha") ||
				strings.EqualFold(root, "needs") && len(path) == 3 && strings.EqualFold(path[1], "outputs")) {
				checkoutInputs[name] = b.ir.Event.SHA
			}
		}
		if err := actionintegration.ValidateCheckoutInputs(lock.Commit, checkoutInputs, b.ir.Event.Repository.Owner+"/"+b.ir.Event.Repository.Name, b.ir.Event.SHA); err != nil {
			span := instance.Steps[stepIndex].Span.Start
			return fmt.Errorf("%s:%d:%d: checkout adapter: %w", instance.SourcePath, span.Line, span.Column, err)
		}
		built.capabilities = append(built.capabilities, "provider-token-read")
		built.authorization.ProviderTokenReadCapabilitySources = append(built.authorization.ProviderTokenReadCapabilitySources, "checkout-adapter")
	case actionintegration.AdapterUploadArtifactBuildkite:
		if err := actionintegration.ValidateUploadArtifactInputs(lock.Commit, instance.Steps[stepIndex].With); err != nil {
			span := instance.Steps[stepIndex].Span.Start
			return fmt.Errorf("%s:%d:%d: bounded upload-artifact adapter: %w", instance.SourcePath, span.Line, span.Column, err)
		}
	case actionintegration.AdapterDownloadArtifactBuildkite:
		if err := actionintegration.ValidateDownloadArtifactInputs(lock.Commit, instance.Steps[stepIndex].With); err != nil {
			span := instance.Steps[stepIndex].Span.Start
			return fmt.Errorf("%s:%d:%d: bounded download-artifact adapter: %w", instance.SourcePath, span.Line, span.Column, err)
		}
		if len(instance.NeedGroups) == 0 {
			span := instance.Steps[stepIndex].Span.Start
			return fmt.Errorf("%s:%d:%d: bounded download-artifact adapter requires at least one direct needs producer", instance.SourcePath, span.Line, span.Column)
		}
	}
	return nil
}

func addContainerCapabilities(instance JobInstance, actionRefs []string, built *builtPlanActions) error {
	if instance.Container == nil && len(instance.Services) == 0 && instance.ServicesExpression == "" {
		return nil
	}
	if len(actionRefs) != 0 && !built.resolved {
		return fmt.Errorf("build plan for job %q: containers with remote actions require action resolution through upload or profile validation", instance.LogicalJobID)
	}
	built.capabilities = append(built.capabilities, "docker", "network")
	sort.Strings(built.capabilities)
	built.capabilities = slices.Compact(built.capabilities)
	if instance.Container != nil {
		built.authorization.DockerCapabilitySources = append(built.authorization.DockerCapabilitySources, "job-containers")
	}
	if len(instance.Services) != 0 || instance.ServicesExpression != "" {
		built.authorization.DockerCapabilitySources = append(built.authorization.DockerCapabilitySources, "service-containers")
	}
	sort.Strings(built.authorization.DockerCapabilitySources)
	return nil
}

func buildPlanNeedSources(instance JobInstance, planDigests map[string]string) (map[string][]plan.NeedSource, error) {
	needSources := make(map[string][]plan.NeedSource, len(instance.NeedGroups))
	for _, logicalNeed := range sortedKeys(instance.NeedGroups) {
		dependencies := instance.NeedGroups[logicalNeed]
		if len(dependencies) > plan.MaxNeedProducers {
			return nil, fmt.Errorf("build plan for job %q: prerequisite %q has %d producers, maximum is %d", instance.LogicalJobID, logicalNeed, len(dependencies), plan.MaxNeedProducers)
		}
		for _, dependency := range dependencies {
			digest, ok := planDigests[dependency]
			if !ok {
				return nil, fmt.Errorf("build plan for job %q: prerequisite %q has no earlier plan digest", instance.LogicalJobID, dependency)
			}
			needSources[logicalNeed] = append(needSources[logicalNeed], plan.NeedSource{StepKey: dependency, PlanDigest: digest})
		}
	}
	return needSources, nil
}

func buildPlanNeedOutputs(instance JobInstance) map[string][]plan.NeedOutput {
	if len(instance.NeedOutputs) == 0 {
		return nil
	}
	needOutputs := make(map[string][]plan.NeedOutput, len(instance.NeedOutputs))
	for _, logicalNeed := range sortedKeys(instance.NeedOutputs) {
		outputs := instance.NeedOutputs[logicalNeed]
		needOutputs[logicalNeed] = make([]plan.NeedOutput, len(outputs))
		for i, output := range outputs {
			needOutputs[logicalNeed][i] = plan.NeedOutput{Name: output.Name, StepKey: output.StepKey, Output: output.Output}
		}
	}
	return needOutputs
}

func buildPlanDeferredInputs(inputs map[string]DeferredInput, planDigests map[string]string) (map[string]plan.DeferredInput, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	resolved := make(map[string]plan.DeferredInput, len(inputs))
	for _, name := range sortedKeys(inputs) {
		input := inputs[name]
		deferred := plan.DeferredInput{Outputs: make([]plan.NeedOutput, len(input.Outputs))}
		for _, source := range input.Sources {
			digest, ok := planDigests[source]
			if !ok {
				return nil, fmt.Errorf("deferred input %q source %q has no earlier plan digest", name, source)
			}
			deferred.Sources = append(deferred.Sources, plan.NeedSource{StepKey: source, PlanDigest: digest})
		}
		for i, output := range input.Outputs {
			deferred.Outputs[i] = plan.NeedOutput{Name: output.Name, StepKey: output.StepKey, Output: output.Output}
		}
		resolved[name] = deferred
	}
	return resolved, nil
}

func buildPlanCallGuards(instance JobInstance, planDigests map[string]string) ([]plan.CallGuard, error) {
	callGuards := make([]plan.CallGuard, len(instance.CallGuards))
	for guardIndex, guard := range instance.CallGuards {
		deferredInputs, err := buildPlanDeferredInputs(guard.DeferredInputs, planDigests)
		if err != nil {
			return nil, fmt.Errorf("build plan for job %q call guard %d: %w", instance.LogicalJobID, guardIndex+1, err)
		}
		planGuard := plan.CallGuard{Condition: guard.Condition, Inputs: cloneAnyMap(guard.Inputs), DeferredInputs: deferredInputs}
		if len(guard.NeedGroups) != 0 {
			planGuard.NeedSources = make(map[string][]plan.NeedSource, len(guard.NeedGroups))
			for _, logicalNeed := range sortedKeys(guard.NeedGroups) {
				for _, dependency := range guard.NeedGroups[logicalNeed] {
					digest, ok := planDigests[dependency]
					if !ok {
						return nil, fmt.Errorf("build plan for job %q: call guard prerequisite %q has no earlier plan digest", instance.LogicalJobID, dependency)
					}
					planGuard.NeedSources[logicalNeed] = append(planGuard.NeedSources[logicalNeed], plan.NeedSource{StepKey: dependency, PlanDigest: digest})
				}
			}
		}
		if len(guard.NeedOutputs) != 0 {
			planGuard.NeedOutputs = make(map[string][]plan.NeedOutput, len(guard.NeedOutputs))
			for _, logicalNeed := range sortedKeys(guard.NeedOutputs) {
				for _, output := range guard.NeedOutputs[logicalNeed] {
					planGuard.NeedOutputs[logicalNeed] = append(planGuard.NeedOutputs[logicalNeed], plan.NeedOutput{Name: output.Name, StepKey: output.StepKey, Output: output.Output})
				}
			}
		}
		callGuards[guardIndex] = planGuard
	}
	return callGuards, nil
}

func (b planBuilder) authorizePlanSecrets(instance JobInstance, actions *builtPlanActions) ([]string, *plan.GitHubToken, error) {
	secrets, referencesGitHubToken, err := requiredSecrets(instance, actions.requiredSecrets, actions.inputsInspected)
	if err != nil {
		return nil, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	referencesGitHubTokenSecret := slices.Contains(secrets, "GITHUB_TOKEN")
	if referencesGitHubTokenSecret {
		secrets = slices.DeleteFunc(secrets, func(name string) bool { return name == "GITHUB_TOKEN" })
	}
	if !referencesGitHubTokenSecret && !referencesGitHubToken && !actions.requiresGitHubToken {
		return secrets, nil, nil
	}
	policyWorkflow := b.ir.Workflow.WorkflowTokenPolicyFilename
	if policyWorkflow == "" {
		policyWorkflow = filepath.Base(instance.SourcePath)
	}
	if len(b.ir.Workflow.WorkflowTokenPermissions) == 0 {
		reference := "an action input default that references github.token"
		if referencesGitHubTokenSecret {
			reference = "secrets.GITHUB_TOKEN"
		} else if referencesGitHubToken {
			reference = "github.token"
		}
		return nil, nil, fmt.Errorf("%s:%d:%d: job %q references %s but has no effective permissions", instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column, instance.LogicalJobID, reference)
	}
	actions.capabilities = append(actions.capabilities, "provider-token-write")
	actions.authorization.ProviderTokenWriteCapabilitySources = []string{"effective-permissions"}
	actions.authorization.WorkflowTokenPolicyFilename = b.ir.Workflow.WorkflowTokenPolicyFilename
	actions.authorization.GitHubTokenSecretReference = referencesGitHubTokenSecret
	return secrets, &plan.GitHubToken{Workflow: policyWorkflow, Permissions: cloneMap(b.ir.Workflow.WorkflowTokenPermissions)}, nil
}

func (b planBuilder) lowerPlanJob(instance JobInstance, runtimeDistributionDigest string, steps []plan.Step, actions builtPlanActions, needSources map[string][]plan.NeedSource, needOutputs map[string][]plan.NeedOutput, deferredInputs map[string]plan.DeferredInput, callGuards []plan.CallGuard, secrets []string, githubToken *plan.GitHubToken) plan.Job {
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version: b.compilerVersion, DistributionDigest: b.compilerDistributionDigest,
		},
		Runtime: &plan.Runtime{DistributionDigest: runtimeDistributionDigest},
		Workflow: plan.Workflow{
			Path:         instance.SourcePath,
			Name:         b.workflowName,
			Digest:       instance.SourceDigest,
			LogicalJobID: instance.LogicalJobID,
			Remote:       planRemoteWorkflowSource(instance.RemoteWorkflow),
		},
		Event: plan.Event{
			Provider: b.ir.Event.Provider, Name: b.ir.Event.Event, PayloadDigest: "sha256:" + hex.EncodeToString(b.eventDigest[:]),
			Repository: b.ir.Event.Repository.Owner + "/" + b.ir.Event.Repository.Name,
			Ref:        b.ir.Event.Ref, HeadRef: eventHeadRef(b.ir.Event), BaseRef: eventBaseRef(b.ir.Event), SHA: b.ir.Event.SHA, Actor: b.ir.Event.Actor,
		},
		Target:                  plan.Target{StepKey: instance.Key, Queue: instance.Queue},
		RequiredCapabilities:    actions.capabilities,
		RequiredSecrets:         secrets,
		GitHubToken:             githubToken,
		IDTokenPermission:       instance.Permissions["id-token"],
		OIDC:                    cloneOIDCConfiguration(b.options.OIDC),
		Matrix:                  instance.Matrix,
		Inputs:                  cloneAnyMap(instance.Inputs),
		DeferredInputs:          deferredInputs,
		Vars:                    cloneMap(b.ir.Vars),
		Dependencies:            append([]string(nil), instance.Needs...),
		NeedSources:             needSources,
		NeedOutputs:             needOutputs,
		CallGuards:              callGuards,
		Env:                     instance.Env,
		Condition:               instance.If,
		ContinueOnError:         instance.ContinueOnError,
		TimeoutMinutes:          instance.TimeoutMinutes,
		DefaultShell:            instance.DefaultShell,
		DefaultWorkingDirectory: instance.DefaultWorkingDirectory,
		Outputs:                 instance.Outputs,
		Steps:                   steps,
		Actions:                 actions.locks,
		ServicesExpression:      instance.ServicesExpression,
	}
	job.RequiresMise = &actions.requiresMise
	if instance.Container != nil {
		job.Container = &plan.Container{Image: instance.Container.Image, Env: cloneMap(instance.Container.Env), Ports: append([]string(nil), instance.Container.Ports...)}
	}
	if len(instance.Services) != 0 {
		job.Services = make(map[string]plan.ServiceContainer, len(instance.Services))
		job.ServiceOrder = make([]string, 0, len(instance.Services))
	}
	for _, service := range instance.Services {
		container := plan.ServiceContainer{
			Image: service.Container.Image, Env: cloneMap(service.Container.Env), Ports: append([]string(nil), service.Container.Ports...),
			Volumes: append([]string(nil), service.Container.Volumes...), Options: service.Container.Options,
			Command: service.Container.Command, Entrypoint: service.Container.Entrypoint,
		}
		if service.Container.Credentials != nil {
			container.Credentials = &plan.ContainerCredentials{Username: service.Container.Credentials.Username, Password: service.Container.Credentials.Password}
		}
		job.Services[service.Name] = container
		job.ServiceOrder = append(job.ServiceOrder, service.Name)
	}
	return job
}

func cloneOIDCConfiguration(configuration *plan.OIDCConfiguration) *plan.OIDCConfiguration {
	if configuration == nil {
		return nil
	}
	return &plan.OIDCConfiguration{
		Claims:         append([]string(nil), configuration.Claims...),
		AWSSessionTags: append([]string(nil), configuration.AWSSessionTags...),
		SubjectClaim:   configuration.SubjectClaim,
	}
}

func sourcePosition(positions map[string]workflow.Position, field string, fallback workflow.Position) workflow.Position {
	if position := positions[field]; position.Line > 0 && position.Column > 0 {
		return position
	}
	return fallback
}

func planExpressionFinding(instance JobInstance, sourcePath, field, source string, position workflow.Position, step int, err error) error {
	finding := &ProcessingFinding{
		Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility",
		Path: sourcePath, Line: position.Line, Column: position.Column,
		Job: instance.LogicalJobID, Instance: instance.Key, Step: step,
		Err: fmt.Errorf("%s:%d:%d: job %q %s expression %q: %w", sourcePath, position.Line, position.Column, instance.LogicalJobID, field, source, err),
	}
	if errors.Is(err, errRetainedGitHubEvent) || strings.Contains(err.Error(), "whole github.event access is unsupported") {
		finding.Message = "This expression uses the whole GitHub event or accesses it dynamically. Reference a specific scalar property, such as github.event.pull_request.head.sha. The full event payload is unavailable at runtime."
		finding.Detail = fmt.Sprintf("Expression in %s: %q", field, source)
	} else if errors.Is(err, errEventActionReference) {
		finding.Message = "An action reference must be a static value and cannot use github.event. Replace the expression with a literal owner/repository/path@ref or local action path."
		finding.Detail = fmt.Sprintf("Expression in %s: %q", field, source)
	}
	return finding
}

func planConstructionFinding(instance JobInstance, err error) error {
	return attributedProcessingFinding(
		StagePlans, CodePlanConstruction, "compatibility",
		instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column,
		instance.LogicalJobID, instance.Key, "", 0, err,
	)
}

func requiredSecrets(instance JobInstance, actionRequired []string, actionInputsInspected bool) ([]string, bool, error) {
	found := map[string]string{}
	referencesGitHubToken := false
	collect := func(value string, stepRuntime bool) error {
		referencesEvent, err := expression.TemplateReferencesGitHubEvent(value)
		if err != nil {
			return err
		}
		if referencesEvent {
			return errRetainedGitHubEvent
		}
		names, err := expression.SecretReferences(value)
		if err != nil {
			return err
		}
		for _, name := range names {
			found[name] = name
		}
		var referencesToken bool
		if stepRuntime {
			referencesToken, err = expression.ReferencesStepGitHubToken(value)
		} else {
			referencesToken, err = expression.ReferencesGitHubToken(value)
		}
		if err != nil {
			return err
		}
		if referencesToken {
			referencesGitHubToken = true
		}
		return nil
	}
	checkCondition := func(value string) error {
		referencesEvent, err := expression.ReferencesGitHubEvent(value)
		if err != nil {
			return err
		}
		if referencesEvent {
			return errRetainedGitHubEvent
		}
		names, err := expression.ConditionSecretReferences(value)
		if err != nil {
			return err
		}
		for _, name := range names {
			found[name] = name
		}
		referencesToken, err := expression.ConditionReferencesGitHubToken(value)
		if err != nil {
			return err
		}
		if referencesToken {
			referencesGitHubToken = true
		}
		return nil
	}
	if err := checkCondition(instance.If); err != nil {
		return nil, false, err
	}
	for _, value := range []string{instance.DefaultShell, instance.DefaultWorkingDirectory} {
		if err := collect(value, false); err != nil {
			return nil, false, err
		}
	}
	for _, values := range []map[string]string{instance.Env, instance.Outputs} {
		for _, name := range sortedValueKeys(values) {
			if err := collect(values[name], false); err != nil {
				return nil, false, err
			}
		}
	}
	for _, step := range instance.Steps {
		if err := checkCondition(step.If); err != nil {
			return nil, false, err
		}
		for _, value := range []string{step.Name, step.Run, step.Shell, step.WorkingDirectory, step.ContinueOnErrorExpression, step.TimeoutMinutesExpression} {
			if err := collect(value, true); err != nil {
				return nil, false, err
			}
		}
		if err := collect(step.Uses, false); err != nil {
			return nil, false, err
		}
		valuesToInspect := []map[string]string{step.Env}
		if step.Kind != "uses" || !actionInputsInspected {
			valuesToInspect = append(valuesToInspect, step.With)
		}
		for _, values := range valuesToInspect {
			for _, name := range sortedValueKeys(values) {
				if err := collect(values[name], true); err != nil {
					return nil, false, err
				}
			}
		}
	}
	if instance.Container != nil {
		for _, value := range append([]string{instance.Container.Image}, instance.Container.Ports...) {
			if err := collect(value, false); err != nil {
				return nil, false, err
			}
		}
		for _, name := range sortedValueKeys(instance.Container.Env) {
			if err := collect(instance.Container.Env[name], false); err != nil {
				return nil, false, err
			}
		}
	}
	for _, service := range instance.Services {
		for _, value := range append([]string{service.Container.Image}, service.Container.Ports...) {
			if err := collect(value, false); err != nil {
				return nil, false, err
			}
		}
		for _, name := range sortedValueKeys(service.Container.Env) {
			if err := collect(service.Container.Env[name], false); err != nil {
				return nil, false, err
			}
		}
		if service.Container.Credentials != nil {
			if err := collect(service.Container.Credentials.Username, false); err != nil {
				return nil, false, err
			}
			if err := collect(service.Container.Credentials.Password, false); err != nil {
				return nil, false, err
			}
		}
	}
	for _, name := range actionRequired {
		found[name] = name
	}
	if !instance.secretAuthority {
		for name := range found {
			if name != "GITHUB_TOKEN" {
				delete(found, name)
			}
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, referencesGitHubToken, nil
}

func planSpan(span workflow.Span) plan.Span {
	return plan.Span{
		Start: plan.Position{Line: span.Start.Line, Column: span.Start.Column},
		End:   plan.Position{Line: span.End.Line, Column: span.End.Column},
	}
}

func planRemoteWorkflowSource(source *RemoteWorkflowSource) *plan.RemoteWorkflowSource {
	if source == nil {
		return nil
	}
	return &plan.RemoteWorkflowSource{
		Repository: source.Repository, RequestedRef: source.RequestedRef, Commit: source.Commit, SourceDigest: source.SourceDigest,
	}
}
