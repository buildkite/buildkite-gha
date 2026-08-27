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
	"github.com/buildkite/buildkite-gha/internal/program"
	shellcompat "github.com/buildkite/buildkite-gha/internal/shell"
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
	locks                []plan.ActionLock
	capabilities         []string
	authorization        PlanAuthorization
	requiredSecrets      []string
	inputsInspected      bool
	requiresGitHubToken  bool
	requiresEventPayload bool
	requiresMise         bool
	resolved             bool
}

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
	workflowProgram := lowerWorkflowProgram(instance)
	if err := b.validateShellCompatibility(instance, workflowProgram); err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	steps := projectPlanSteps(workflowProgram.Job.Steps)
	actionIndexes, actionRefs, actionInputs := programActionInvocations(workflowProgram.Job.Steps)
	actions, err := b.buildActions(instance, steps, actionIndexes, actionRefs, actionInputs)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	bindProgramActionSelectors(&workflowProgram, steps)
	steps = projectPlanSteps(workflowProgram.Job.Steps)
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
	secrets, secretMappings, githubToken, err := b.authorizePlanSecrets(instance, workflowProgram, &actions)
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
	job := b.lowerPlanJob(instance, workflowProgram, runtimeDistributionDigest, steps, actions, needSources, buildPlanNeedOutputs(instance), deferredInputs, callGuards, secrets, secretMappings, githubToken)
	if err := job.Validate(); err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	encoded, err := plan.Encode(job)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("encode plan for job %q: %w", instance.LogicalJobID, err)
	}
	return job, actions.authorization, encoded, nil
}

func (b planBuilder) validateShellCompatibility(instance JobInstance, workflowProgram program.Program) error {
	context := compileContext(b.ir.Event, b.ir.Vars, instance.SourcePath, b.workflowName)
	context.Inputs = instance.Inputs
	context.Matrix = instance.Matrix
	var diagnostics []error
	validate := func(site program.Site, step int) {
		if site.Source == "" {
			return
		}
		resolved, err := expression.EvaluateAvailableCompileTemplate(site.Source, context)
		if err != nil || strings.Contains(resolved, "${{") {
			return
		}
		if err := shellcompat.ValidateCompatibility(resolved); err != nil {
			owner := fmt.Sprintf("job %q", instance.LogicalJobID)
			if step != 0 {
				owner += fmt.Sprintf(" step %d", step)
			}
			diagnostics = append(diagnostics, &ProcessingFinding{
				Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility",
				Path: site.Location.File, Line: site.Location.Start.Line, Column: site.Location.Start.Column,
				Job: instance.LogicalJobID, Instance: instance.Key, Step: step, Message: err.Error(),
				Err: fmt.Errorf("%s:%d:%d: %s: %w", site.Location.File, site.Location.Start.Line, site.Location.Start.Column, owner, err),
			})
		}
	}
	validate(workflowProgram.Job.Defaults.Shell, 0)
	for i, step := range workflowProgram.Job.Steps {
		if step.Run != nil {
			validate(step.Run.Shell, i+1)
		}
	}
	return errors.Join(diagnostics...)
}

func (b planBuilder) reducePlanInstanceEventExpressions(instance JobInstance) (JobInstance, error) {
	context := compileContext(b.ir.Event, b.ir.Vars, instance.SourcePath, b.workflowName)
	context.Inputs = instance.Inputs
	context.Matrix = instance.Matrix
	retainsEventPayload := false

	reduceTemplate := func(value string) (string, error) {
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
		if referencesEventAlias {
			return "", fmt.Errorf("event context access must resolve during compilation; use github.event for whole or runtime-selected event access")
		}
		retainsEventPayload = retainsEventPayload || referencesEvent
		return reduced, nil
	}
	reduceCondition := func(value string) (string, error) {
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
			retainsEventPayload = true
		}
		return reduced, nil
	}
	reduceTypedExpression := func(value string) (string, error) {
		referencesEvent, err := expression.ReferencesGitHubEvent(value)
		if err != nil || !referencesEvent {
			return value, err
		}
		reduced, err := reduceCondition(value)
		if err != nil {
			return "", err
		}
		return "${{ " + reduced + " }}", nil
	}
	reduceMap := func(values map[string]string) (map[string]string, error) {
		if values == nil {
			return nil, nil
		}
		result := cloneMap(values)
		for _, name := range sortedValueKeys(result) {
			value, err := reduceTemplate(result[name])
			if err != nil {
				return nil, err
			}
			result[name] = value
		}
		return result, nil
	}
	reduceSlice := func(values []string) ([]string, error) {
		if values == nil {
			return nil, nil
		}
		result := append([]string(nil), values...)
		for i := range result {
			value, err := reduceTemplate(result[i])
			if err != nil {
				return nil, err
			}
			result[i] = value
		}
		return result, nil
	}

	var err error
	if instance.If, err = reduceCondition(instance.If); err != nil {
		return JobInstance{}, err
	}
	for _, field := range []*string{&instance.DefaultShell, &instance.DefaultWorkingDirectory, &instance.ServicesExpression} {
		if *field, err = reduceTemplate(*field); err != nil {
			return JobInstance{}, err
		}
	}
	if instance.Env, err = reduceMap(instance.Env); err != nil {
		return JobInstance{}, err
	}
	if instance.Outputs, err = reduceMap(instance.Outputs); err != nil {
		return JobInstance{}, err
	}

	instance.CallGuards = append([]CallGuard(nil), instance.CallGuards...)
	for i := range instance.CallGuards {
		if instance.CallGuards[i].Condition, err = reduceCondition(instance.CallGuards[i].Condition); err != nil {
			return JobInstance{}, err
		}
	}

	instance.Steps = append([]workflow.Step(nil), instance.Steps...)
	for i := range instance.Steps {
		step := &instance.Steps[i]
		referencesEvent, referencesErr := expression.TemplateReferencesGitHubEvent(step.Uses)
		if referencesErr != nil {
			return JobInstance{}, referencesErr
		}
		if referencesEvent {
			return JobInstance{}, fmt.Errorf("github.event cannot select an action reference")
		}
		if step.If, err = reduceCondition(step.If); err != nil {
			return JobInstance{}, err
		}
		for _, field := range []*string{&step.Name, &step.Run, &step.Shell, &step.WorkingDirectory} {
			if *field, err = reduceTemplate(*field); err != nil {
				return JobInstance{}, err
			}
		}
		for _, field := range []*string{&step.ContinueOnErrorExpression, &step.TimeoutMinutesExpression} {
			if *field, err = reduceTypedExpression(*field); err != nil {
				return JobInstance{}, err
			}
		}
		if step.Env, err = reduceMap(step.Env); err != nil {
			return JobInstance{}, err
		}
		if step.With, err = reduceMap(step.With); err != nil {
			return JobInstance{}, err
		}
	}

	if instance.Container != nil {
		container := *instance.Container
		if container.Image, err = reduceTemplate(container.Image); err != nil {
			return JobInstance{}, err
		}
		if container.Env, err = reduceMap(container.Env); err != nil {
			return JobInstance{}, err
		}
		if container.Ports, err = reduceSlice(container.Ports); err != nil {
			return JobInstance{}, err
		}
		instance.Container = &container
	}

	instance.Services = append([]workflow.Service(nil), instance.Services...)
	for i := range instance.Services {
		container := instance.Services[i].Container
		for _, field := range []*string{&container.Image, &container.Options, &container.Command, &container.Entrypoint} {
			if *field, err = reduceTemplate(*field); err != nil {
				return JobInstance{}, err
			}
		}
		if container.Env, err = reduceMap(container.Env); err != nil {
			return JobInstance{}, err
		}
		if container.Ports, err = reduceSlice(container.Ports); err != nil {
			return JobInstance{}, err
		}
		if container.Volumes, err = reduceSlice(container.Volumes); err != nil {
			return JobInstance{}, err
		}
		if container.Credentials != nil {
			credentials := *container.Credentials
			if credentials.Username, err = reduceTemplate(credentials.Username); err != nil {
				return JobInstance{}, err
			}
			if credentials.Password, err = reduceTemplate(credentials.Password); err != nil {
				return JobInstance{}, err
			}
			container.Credentials = &credentials
		}
		instance.Services[i].Container = container
	}
	instance.RetainEventPayload = retainsEventPayload
	return instance, nil
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
	reachable := make([]bool, len(actionIndexes))
	knownReferences := make(map[string]any, len(instance.Matrix))
	for name, value := range instance.Matrix {
		knownReferences["matrix."+strings.ToLower(name)] = value
	}
	for i, stepIndex := range actionIndexes {
		reachable[i] = compositeStepMayRun(steps[stepIndex].Condition, knownReferences)
	}
	compiled, err := compileReachableActionInvocations(b.ctx, instance.RepositoryRoot, b.actionSource, plan.EventServerURL(b.ir.Event.Provider), actionRefs, actionInputs, reachable)
	if err != nil {
		return built, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	built.requiresMise = compiled.requiresMise
	built.requiresGitHubToken = compiled.requiresGitHubToken
	built.requiresEventPayload = compiled.requiresEventPayload
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
		// Keep the established authorization label for source-verified Docker
		// action metadata, including prebuilt-image declarations.
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

func (b planBuilder) authorizePlanSecrets(instance JobInstance, workflowProgram program.Program, actions *builtPlanActions) ([]string, map[string]string, *plan.GitHubToken, error) {
	secrets, mappings, tokenAliases, referencesGitHubToken, err := requiredSecrets(workflowProgram, instance.secretAuthority, actions.requiredSecrets, actions.inputsInspected)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	referencesGitHubTokenSecret := len(tokenAliases) != 0
	tokenAliases = slices.DeleteFunc(tokenAliases, func(name string) bool { return name == "GITHUB_TOKEN" })
	if !referencesGitHubTokenSecret && !referencesGitHubToken && !actions.requiresGitHubToken {
		return secrets, mappings, nil, nil
	}
	policyWorkflow := b.ir.Workflow.WorkflowTokenPolicyFilename
	if policyWorkflow == "" {
		policyWorkflow = filepath.Base(instance.SourcePath)
	}
	if len(b.ir.Workflow.WorkflowTokenPermissions) == 0 {
		reference := "resolved action metadata that references github.token"
		if referencesGitHubTokenSecret {
			reference = "secrets.GITHUB_TOKEN"
		} else if referencesGitHubToken {
			reference = "github.token"
		}
		return nil, nil, nil, fmt.Errorf("%s:%d:%d: job %q references %s but has no effective permissions", instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column, instance.LogicalJobID, reference)
	}
	actions.capabilities = append(actions.capabilities, "provider-token-write")
	actions.authorization.ProviderTokenWriteCapabilitySources = []string{"effective-permissions"}
	actions.authorization.WorkflowTokenPolicyFilename = b.ir.Workflow.WorkflowTokenPolicyFilename
	actions.authorization.GitHubTokenSecretReference = referencesGitHubTokenSecret
	return secrets, mappings, &plan.GitHubToken{Workflow: policyWorkflow, Permissions: cloneMap(b.ir.Workflow.WorkflowTokenPermissions), Aliases: tokenAliases}, nil
}

func (b planBuilder) lowerPlanJob(instance JobInstance, workflowProgram program.Program, runtimeDistributionDigest string, steps []plan.Step, actions builtPlanActions, needSources map[string][]plan.NeedSource, needOutputs map[string][]plan.NeedOutput, deferredInputs map[string]plan.DeferredInput, callGuards []plan.CallGuard, secrets []string, secretMappings map[string]string, githubToken *plan.GitHubToken) plan.Job {
	programJob := workflowProgram.Job
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
		SecretMappings:          secretMappings,
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
		Env:                     programBindingMap(programJob.Env),
		Condition:               programJob.Condition.Source,
		ContinueOnError:         programJob.ContinueOnError,
		TimeoutMinutes:          programJob.TimeoutMinutes,
		DefaultShell:            programJob.Defaults.Shell.Source,
		DefaultWorkingDirectory: programJob.Defaults.WorkingDirectory.Source,
		Outputs:                 programBindingMap(programJob.Outputs),
		Steps:                   steps,
		Actions:                 actions.locks,
	}
	job.Event.PayloadArtifact = instance.RetainEventPayload || actions.requiresEventPayload
	job.RequiresMise = &actions.requiresMise
	if programJob.Container != nil {
		job.Container = &plan.Container{
			Image: programJob.Container.Image.Source,
			Env:   programBindingMap(programJob.Container.Env),
			Ports: programSiteSources(programJob.Container.Ports),
		}
	}
	if programJob.Services.Dynamic != nil {
		job.ServicesExpression = programJob.Services.Dynamic.Source
	}
	if len(programJob.Services.Static) != 0 {
		job.Services = make(map[string]plan.ServiceContainer, len(programJob.Services.Static))
		job.ServiceOrder = make([]string, 0, len(programJob.Services.Static))
	}
	for _, service := range programJob.Services.Static {
		container := plan.ServiceContainer{
			Image: service.Container.Image.Source, Env: programBindingMap(service.Container.Env), Ports: programSiteSources(service.Container.Ports),
			Volumes: programSiteSources(service.Container.Volumes), Options: service.Container.Options.Source,
			Command: service.Container.Command.Source, Entrypoint: service.Container.Entrypoint.Source,
		}
		if service.Container.Credentials != nil {
			container.Credentials = &plan.ContainerCredentials{
				Username: service.Container.Credentials.Username.Source,
				Password: service.Container.Credentials.Password.Source,
			}
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

func planConstructionFinding(instance JobInstance, err error) error {
	return attributedProcessingFinding(
		StagePlans, CodePlanConstruction, "compatibility",
		instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column,
		instance.LogicalJobID, instance.Key, "", 0, err,
	)
}

func requiredSecrets(workflowProgram program.Program, bindings secretAuthority, actionRequired []string, actionInputsInspected bool) ([]string, map[string]string, []string, bool, error) {
	authority, err := program.InventoryAuthority(workflowProgram, program.AuthorityOptions{ActionInputsInspected: actionInputsInspected})
	if err != nil {
		return nil, nil, nil, false, err
	}
	found := make(map[string]string, len(authority.Secrets)+len(actionRequired))
	for _, name := range authority.Secrets {
		found[name] = name
	}
	if actionInputsInspected {
		err = workflowProgram.VisitSites(func(site program.Site) error {
			if site.Purpose != program.PurposeActionInput {
				return nil
			}
			names, err := expression.SecretReferences(site.Source)
			if err != nil {
				return err
			}
			for _, name := range names {
				binding, ok := bindings.resolve(name)
				if strings.EqualFold(name, "GITHUB_TOKEN") || ok && binding.token {
					found[name] = name
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, nil, false, err
		}
	}
	for _, name := range actionRequired {
		found[name] = name
	}
	sources := make(map[string]struct{}, len(found))
	mappings := map[string]string{}
	var tokenAliases []string
	for alias := range found {
		if alias == "GITHUB_TOKEN" {
			tokenAliases = append(tokenAliases, alias)
			continue
		}
		binding, ok := bindings.resolve(alias)
		if !ok {
			continue
		}
		if binding.token {
			tokenAliases = append(tokenAliases, alias)
			continue
		}
		sources[binding.source] = struct{}{}
		if !bindings.unrestricted {
			mappings[alias] = binding.source
		}
	}
	names := sortedKeys(sources)
	sort.Strings(tokenAliases)
	if len(mappings) == 0 {
		mappings = nil
	}
	return names, mappings, tokenAliases, authority.GitHubToken, nil
}

func planRemoteWorkflowSource(source *RemoteWorkflowSource) *plan.RemoteWorkflowSource {
	if source == nil {
		return nil
	}
	return &plan.RemoteWorkflowSource{
		Repository: source.Repository, RequestedRef: source.RequestedRef, Commit: source.Commit, SourceDigest: source.SourceDigest,
	}
}
