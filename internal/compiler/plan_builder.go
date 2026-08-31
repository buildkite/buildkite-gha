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
	reachableSecrets     []string
	requiresMise         bool
	resolved             bool
	hasInvocations       bool
	programs             map[string]program.Action
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
	actions, err := b.buildActions(instance, &workflowProgram)
	if err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, err
	}
	workflowProgram.Actions = actions.programs
	if err := addContainerCapabilities(instance, &actions); err != nil {
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
	job := b.lowerPlanJob(instance, workflowProgram, runtimeDistributionDigest, actions, needSources, buildPlanNeedOutputs(instance), deferredInputs, callGuards, secrets, secretMappings, githubToken)
	if err := job.ProjectProgram(); err != nil {
		return plan.Job{}, PlanAuthorization{}, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
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
		resolved, err := reduceCompileString(site.Source, context)
		if err != nil || strings.Contains(resolved, "${{") {
			return
		}
		if err := shellcompat.ValidateCompatibility(resolved); err != nil {
			command, _ := shellcompat.UnsupportedCommand(err)
			owner := fmt.Sprintf("job %q", instance.LogicalJobID)
			if step != 0 {
				owner += fmt.Sprintf(" step %d", step)
			}
			diagnostics = append(diagnostics, &ProcessingFinding{
				Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility",
				Blocker: "shell", BlockerDetail: command,
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
	reduceTemplate := func(value string, profile expression.ProfileID) (string, error) {
		if value == "" {
			return value, nil
		}
		reduced, err := reduceTemplateString(value, profile, context)
		if err != nil {
			return "", err
		}
		referencesEventAlias, err := referencesContext(reduced, profile, "event", false)
		if err != nil {
			return "", err
		}
		if referencesEventAlias {
			return "", fmt.Errorf("event context access must resolve during compilation; use github.event for whole or runtime-selected event access")
		}
		return reduced, nil
	}
	reduceCondition := func(value string, profile expression.ProfileID) (string, error) {
		if value == "" {
			return value, nil
		}
		referencesEvent, err := siteReferencesEvent(value, profile, expression.ResultBoolean, true)
		if err != nil {
			return "", err
		}
		if !referencesEvent {
			return value, nil
		}
		reduction, err := reduceCompileSite(value, profile, expression.ResultBoolean, context)
		if err != nil {
			return "", err
		}
		reduced := reduction.Source
		if reduction.Known {
			reduced = fmt.Sprint(reduction.Value)
		}
		return reduced, nil
	}
	reduceTypedExpression := func(site program.Site, profile expression.ProfileID) (string, error) {
		value := site.Source
		result := expression.ResultType(site.Result)
		referencesEvent, err := siteReferencesEvent(value, profile, result, true)
		if err != nil || !referencesEvent {
			return value, err
		}
		reduced, err := reduceCompileSiteAt(value, profile, result, context, expression.Location{
			File: site.Location.File, Field: site.Location.Field,
			Span: expression.Span{
				Start: expression.Position{Line: site.Location.Start.Line, Column: site.Location.Start.Column},
				End:   expression.Position{Line: site.Location.End.Line, Column: site.Location.End.Column},
			},
		})
		if err != nil {
			return "", err
		}
		if reduced.Known {
			literal, err := expression.NewEngine().Literal(reduced.Value)
			if err != nil {
				return "", err
			}
			return "${{ " + literal + " }}", nil
		}
		return reduced.Source, nil
	}
	workflowProgram := lowerWorkflowProgram(instance)
	reducedProgram, err := workflowProgram.TransformSites(func(site program.Site) (program.Site, error) {
		profile, reduceErr := compileReductionProfile(site.Surface)
		if reduceErr != nil {
			return site, reduceErr
		}
		switch site.Surface {
		case program.SurfaceJobCondition:
			site.Source, reduceErr = reduceCondition(site.Source, profile)
		case program.SurfaceCallCondition:
			site.Source, reduceErr = reduceCondition(site.Source, profile)
		case program.SurfaceStepCondition:
			site.Source, reduceErr = reduceCondition(site.Source, profile)
		case program.SurfaceStepControl:
			site.Source, reduceErr = reduceTypedExpression(site, profile)
		case program.SurfaceJobEnvironment:
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		case program.SurfaceJobDefault:
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		case program.SurfaceJobOutput:
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		case program.SurfaceStepTemplate:
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		case program.SurfaceRuntimeTemplate:
			if strings.HasSuffix(site.Location.Field, ".uses") {
				var referencesEvent bool
				referencesEvent, reduceErr = siteReferencesEvent(site.Source, profile, expression.ResultString, false)
				if reduceErr == nil && referencesEvent {
					reduceErr = fmt.Errorf("github.event cannot select an action reference")
				}
				break
			}
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		case program.SurfaceServiceTemplate:
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		case program.SurfaceServiceCredential:
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		case program.SurfaceServiceMap:
			site.Source, reduceErr = reduceTemplate(site.Source, profile)
		}
		return site, reduceErr
	})
	if err != nil {
		return JobInstance{}, err
	}
	projectReducedProgram(&instance, reducedProgram)
	return instance, nil
}

// compileReductionProfile is the single admission policy for event reduction.
// Program.Validate applies the destination runtime profile to every residual.
var compileReductionProfiles = map[program.Surface]expression.ProfileID{
	program.SurfaceJobCondition:      expression.ProfileCompileJobCondition,
	program.SurfaceCallCondition:     expression.ProfileCompileCallCondition,
	program.SurfaceStepCondition:     expression.ProfileCompileStepCondition,
	program.SurfaceStepControl:       expression.ProfileStepControl,
	program.SurfaceJobEnvironment:    expression.ProfileJobEnvironment,
	program.SurfaceJobDefault:        expression.ProfileJobDefault,
	program.SurfaceJobOutput:         expression.ProfileJobOutput,
	program.SurfaceStepTemplate:      expression.ProfileStepTemplate,
	program.SurfaceRuntimeTemplate:   expression.ProfileRuntimeTemplate,
	program.SurfaceServiceTemplate:   expression.ProfileRuntimeTemplate,
	program.SurfaceServiceCredential: expression.ProfileServiceCredential,
	program.SurfaceServiceMap:        expression.ProfileStepTemplate,
}

func compileReductionProfile(surface program.Surface) (expression.ProfileID, error) {
	profile, ok := compileReductionProfiles[surface]
	if !ok {
		return "", fmt.Errorf("expression surface %q has no compile reduction profile", surface)
	}
	return profile, nil
}

// projectReducedProgram updates the source-oriented IR after the immutable
// whole-program pass. Expression traversal remains owned by Program.TransformSites.
func projectReducedProgram(instance *JobInstance, reduced program.Program) {
	instance.If = reduced.Job.Condition.Source
	instance.Env = programBindingMap(reduced.Job.Env)
	instance.DefaultShell = reduced.Job.Defaults.Shell.Source
	instance.DefaultWorkingDirectory = reduced.Job.Defaults.WorkingDirectory.Source
	instance.Outputs = programBindingMap(reduced.Job.Outputs)
	for i := range instance.CallGuards {
		instance.CallGuards[i].Condition = reduced.Job.Guards[i].Condition.Source
	}
	for i := range instance.Steps {
		source := reduced.Job.Steps[i]
		target := &instance.Steps[i]
		target.Name = source.Name.Source
		target.If = source.Condition.Source
		target.Env = programBindingMap(source.Env)
		if source.ContinueOnError.Expression != nil {
			target.ContinueOnErrorExpression = source.ContinueOnError.Expression.Source
		}
		if source.TimeoutMinutes.Expression != nil {
			target.TimeoutMinutesExpression = source.TimeoutMinutes.Expression.Source
		}
		if source.Run != nil {
			target.Run = source.Run.Command.Source
			target.Shell = source.Run.Shell.Source
			target.WorkingDirectory = source.Run.WorkingDirectory.Source
		}
		if source.Invocation != nil {
			target.Uses = source.Invocation.Uses.Source
			target.With = programBindingMap(source.Invocation.With)
		}
	}
	if instance.Container != nil && reduced.Job.Container != nil {
		container := *instance.Container
		container.Image = reduced.Job.Container.Image.Source
		container.Env = programBindingMap(reduced.Job.Container.Env)
		container.Ports = programSiteSources(reduced.Job.Container.Ports)
		instance.Container = &container
	}
	for i := range instance.Services {
		source := reduced.Job.Services.Static[i].Container
		target := instance.Services[i].Container
		target.Image = source.Image.Source
		target.Env = programBindingMap(source.Env)
		target.Ports = programSiteSources(source.Ports)
		target.Volumes = programSiteSources(source.Volumes)
		target.Options = source.Options.Source
		target.Command = source.Command.Source
		target.Entrypoint = source.Entrypoint.Source
		if target.Credentials != nil && source.Credentials != nil {
			credentials := *target.Credentials
			credentials.Username = source.Credentials.Username.Source
			credentials.Password = source.Credentials.Password.Source
			target.Credentials = &credentials
		}
		instance.Services[i].Container = target
	}
	if reduced.Job.Services.Dynamic != nil {
		instance.ServicesExpression = reduced.Job.Services.Dynamic.Source
	}
}

func (b planBuilder) buildActions(instance JobInstance, workflowProgram *program.Program) (builtPlanActions, error) {
	steps := workflowProgram.Job.Steps
	var actionIndexes []int
	var actionRefs []string
	var actionInputs []map[string]string
	for i, step := range steps {
		if step.Invocation == nil {
			continue
		}
		actionIndexes = append(actionIndexes, i)
		actionRefs = append(actionRefs, step.Invocation.Uses.Source)
		actionInputs = append(actionInputs, programBindingMap(step.Invocation.With))
	}
	built := builtPlanActions{requiresMise: len(actionRefs) != 0, resolved: b.options.ResolveActions, hasInvocations: len(actionRefs) != 0}
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
	built.requiredSecrets = compiled.requiredSecrets
	built.programs = compiled.programs
	built.inputsInspected = true
	reachability, err := program.WorkflowReachability(*workflowProgram, expression.AbstractValues{References: b.authorityReferences(instance)})
	if err != nil {
		return built, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	reachableSecrets := map[string]bool{}
	for i, authority := range compiled.rootAuthorities {
		if !reachability.Steps[actionIndexes[i]] {
			continue
		}
		built.requiresGitHubToken = built.requiresGitHubToken || authority.GitHubToken
		built.requiresEventPayload = built.requiresEventPayload || authority.EventPayload
		if authority.GitHubToken {
			built.authorization.GitHubTokenActions = append(built.authorization.GitHubTokenActions, actionRefs[i])
		}
		for _, name := range authority.Secrets {
			reachableSecrets[name] = true
		}
		for _, binding := range steps[actionIndexes[i]].Invocation.With {
			names, err := program.ValidateSite(binding.Value)
			if err != nil {
				return built, fmt.Errorf("build plan for job %q: action input %q: %w", instance.LogicalJobID, binding.Name, err)
			}
			for _, name := range names {
				reachableSecrets[name] = true
			}
		}
	}
	built.reachableSecrets = sortedKeys(reachableSecrets)
	locksByID := make(map[string]plan.ActionLock, len(compiled.locks))
	for _, lock := range compiled.locks {
		locksByID[lock.ID] = lock
	}
	for i, selector := range compiled.selectors {
		stepIndex := actionIndexes[i]
		steps[stepIndex].Invocation.Lock = selector.Lock
		lock, ok := locksByID[selector.Lock]
		if !ok {
			return built, fmt.Errorf("build plan for job %q: action lock %q is missing", instance.LogicalJobID, selector.Lock)
		}
		if err := b.validateActionAdapter(instance, stepIndex, lock, reachability.Steps[stepIndex], &built); err != nil {
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

func (b planBuilder) authorityReferences(instance JobInstance) map[string]any {
	return map[string]any{
		"github.server_url": plan.EventServerURL(b.ir.Event.Provider),
		"inputs":            instance.Inputs,
		"matrix":            instance.Matrix,
		"vars":              b.ir.Vars,
	}
}

func (b planBuilder) validateActionAdapter(instance JobInstance, stepIndex int, lock plan.ActionLock, reachable bool, built *builtPlanActions) error {
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	switch descriptor.Adapter {
	case actionintegration.AdapterCheckoutExactEventSHA:
		checkoutInputs := cloneMap(instance.Steps[stepIndex].With)
		for name, value := range checkoutInputs {
			if !strings.EqualFold(name, "ref") {
				continue
			}
			root, path, err := staticReference(value)
			if err == nil && (strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "sha") ||
				strings.EqualFold(root, "needs") && len(path) == 3 && strings.EqualFold(path[1], "outputs")) {
				checkoutInputs[name] = b.ir.Event.SHA
			}
		}
		if err := actionintegration.ValidateCheckoutInputs(lock.Commit, checkoutInputs, b.ir.Event.Repository.Owner+"/"+b.ir.Event.Repository.Name, b.ir.Event.SHA); err != nil {
			span := instance.Steps[stepIndex].Span.Start
			return fmt.Errorf("%s:%d:%d: checkout adapter: %w", instance.SourcePath, span.Line, span.Column, err)
		}
		if reachable {
			built.capabilities = append(built.capabilities, "provider-token-read")
			built.authorization.ProviderTokenReadCapabilitySources = append(built.authorization.ProviderTokenReadCapabilitySources, "checkout-adapter")
		}
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

func addContainerCapabilities(instance JobInstance, built *builtPlanActions) error {
	if instance.Container == nil && len(instance.Services) == 0 && instance.ServicesExpression == "" {
		return nil
	}
	if built.hasInvocations && !built.resolved {
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
	serverURL := plan.EventServerURL(b.ir.Event.Provider)
	secrets, mappings, tokenAliases, referencesGitHubToken, referencesEventPayload, err := requiredSecrets(workflowProgram, instance.secretAuthority, actions.requiredSecrets, actions.reachableSecrets, actions.inputsInspected, serverURL, b.authorityReferences(instance))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	actions.requiresEventPayload = actions.requiresEventPayload || referencesEventPayload
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
		reference := "an action input default that references github.token"
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

func (b planBuilder) lowerPlanJob(instance JobInstance, workflowProgram program.Program, runtimeDistributionDigest string, actions builtPlanActions, needSources map[string][]plan.NeedSource, needOutputs map[string][]plan.NeedOutput, deferredInputs map[string]plan.DeferredInput, callGuards []plan.CallGuard, secrets []string, secretMappings map[string]string, githubToken *plan.GitHubToken) plan.Job {
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
		Target:               plan.Target{StepKey: instance.Key, Queue: instance.Queue},
		RequiredCapabilities: actions.capabilities,
		RequiredSecrets:      secrets,
		SecretMappings:       secretMappings,
		GitHubToken:          githubToken,
		IDTokenPermission:    instance.Permissions["id-token"],
		OIDC:                 cloneOIDCConfiguration(b.options.OIDC),
		Matrix:               instance.Matrix,
		Inputs:               cloneAnyMap(instance.Inputs),
		DeferredInputs:       deferredInputs,
		Vars:                 cloneMap(b.ir.Vars),
		Dependencies:         append([]string(nil), instance.Needs...),
		NeedSources:          needSources,
		NeedOutputs:          needOutputs,
		CallGuards:           callGuards,
		Program:              &workflowProgram,
		Actions:              actions.locks,
	}
	job.Event.PayloadArtifact = actions.requiresEventPayload
	job.RequiresMise = &actions.requiresMise
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

func requiredSecrets(workflowProgram program.Program, bindings secretAuthority, actionRequired, reachableActionRequired []string, actionInputsInspected bool, serverURL string, references map[string]any) ([]string, map[string]string, []string, bool, bool, error) {
	if references == nil {
		references = map[string]any{"github.server_url": serverURL}
	}
	authority, err := program.InventoryAuthority(workflowProgram, program.AuthorityOptions{
		ActionInputsInspected: actionInputsInspected,
		Values:                expression.AbstractValues{References: references},
	})
	if err != nil {
		return nil, nil, nil, false, false, err
	}
	found := make(map[string]string, len(authority.Secrets)+len(actionRequired))
	workflowFound := make(map[string]bool, len(authority.ReachableSecrets))
	for _, name := range authority.Secrets {
		found[name] = name
	}
	for _, name := range authority.ReachableSecrets {
		workflowFound[name] = true
	}
	if actionInputsInspected {
		err = workflowProgram.VisitSites(func(site program.Site) error {
			if site.Purpose != program.PurposeActionInput {
				return nil
			}
			names, err := program.ValidateSite(site)
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
			return nil, nil, nil, false, false, err
		}
	}
	for _, name := range actionRequired {
		found[name] = name
	}
	reachableActionFound := make(map[string]bool, len(reachableActionRequired))
	for _, name := range reachableActionRequired {
		reachableActionFound[name] = true
	}
	sources := make(map[string]struct{}, len(found))
	mappings := map[string]string{}
	var tokenAliases []string
	for alias := range found {
		if alias == "GITHUB_TOKEN" {
			if workflowFound[alias] || reachableActionFound[alias] {
				tokenAliases = append(tokenAliases, alias)
			}
			continue
		}
		binding, ok := bindings.resolve(alias)
		if !ok {
			continue
		}
		if binding.token {
			if workflowFound[alias] || reachableActionFound[alias] {
				tokenAliases = append(tokenAliases, alias)
			}
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
	return names, mappings, tokenAliases, authority.GitHubToken, authority.EventPayload, nil
}

func planRemoteWorkflowSource(source *RemoteWorkflowSource) *plan.RemoteWorkflowSource {
	if source == nil {
		return nil
	}
	return &plan.RemoteWorkflowSource{
		Repository: source.Repository, RequestedRef: source.RequestedRef, Commit: source.Commit, SourceDigest: source.SourceDigest,
	}
}
