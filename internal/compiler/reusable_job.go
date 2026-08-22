package compiler

import (
	"fmt"
	"slices"
	"sort"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// sourcedJob owns reusable-workflow state from recursive expansion until it is
// projected onto a concrete JobInstance. Graph expansion only orchestrates the
// ordered calls below; it does not interpret authority, provenance, deferred
// inputs, prerequisite projections, or call guards independently.
type sourcedJob struct {
	workflow.Job
	reusable reusableJobState
}

type reusableExpansion struct {
	source            reusableWorkflowSource
	digest            string
	namespace         string
	labelPrefix       string
	inputs            reusableInputs
	externalNeeds     map[string]needBinding
	permissionCeiling *workflow.Permissions
	authority         reusableJobAuthority
	callGuards        []sourcedCallGuard
	depth             int
}

type reusableJobState struct {
	provenance   reusableJobProvenance
	inputs       reusableInputs
	needBindings map[string]needBinding
	authority    reusableJobAuthority
	callGuards   []sourcedCallGuard
}

type reusableJobProvenance struct {
	path   string
	digest string
	root   string
	remote *RemoteWorkflowSource
}

type reusableJobAuthority struct {
	secrets               secretAuthority
	tokenPolicyNarrowed   bool
	jobPermissionsIgnored bool
	reusableCall          workflow.Position
}

type secretAuthority struct {
	unrestricted bool
	bindings     map[string]secretBinding
}

type secretBinding struct {
	source string
	token  bool
}

type sourcedCallGuard struct {
	condition    string
	inputs       reusableInputs
	needBindings map[string]needBinding
}

type reusableInputs struct {
	values   map[string]any
	deferred map[string]needBinding
}

type needBinding struct {
	// members are flattened logical job IDs owned by one source-level need.
	// projectOutputs distinguishes reusable-call/status-only boundaries from
	// ordinary job needs, whose outputs pass through unchanged.
	members        []string
	projectOutputs bool
	outputs        []needOutputBinding
}

type needOutputBinding struct {
	name   string
	member string
	output string
	path   string
	span   workflow.Span
}

func (expansion reusableExpansion) flattenedJob(job workflow.Job, needs map[string]needBinding, authority reusableJobAuthority, workspaceRoot string) sourcedJob {
	authority.secrets = cloneSecretAuthority(authority.secrets)
	return sourcedJob{Job: job, reusable: reusableJobState{
		provenance: reusableJobProvenance{
			path: expansion.source.displayPath, digest: expansion.digest, root: workspaceRoot,
			remote: cloneRemoteWorkflowSource(expansion.source.remote),
		},
		inputs: cloneReusableInputs(expansion.inputs), needBindings: cloneNeedBindings(needs),
		authority: authority, callGuards: cloneSourcedCallGuards(expansion.callGuards),
	}}
}

func (sourced sourcedJob) sourcePath() string {
	return sourced.reusable.provenance.path
}

func (sourced sourcedJob) describeRuntimeMatrix(jobs map[string]workflow.Job, matricesByJob map[string][]map[string]any) (RuntimeMatrixDescriptor, bool, error) {
	return describeRuntimeMatrix(sourced.Job, sourced.reusable.provenance.path, sourced.reusable.provenance.digest, sourced.reusable.needBindings, jobs, matricesByJob)
}

func (sourced sourcedJob) topologyJob() workflow.Job {
	job := sourced.Job
	for _, guard := range sourced.reusable.callGuards {
		job.Needs = append(job.Needs, bindingMembers(guard.needBindings)...)
		job.Needs = append(job.Needs, bindingMembers(guard.inputs.deferred)...)
	}
	job.Needs = append(job.Needs, bindingMembers(sourced.reusable.inputs.deferred)...)
	sort.Strings(job.Needs)
	job.Needs = slices.Compact(job.Needs)
	return job
}

func (sourced sourcedJob) compileContext(context expression.CompileContext) expression.CompileContext {
	context.Inputs = sourced.reusable.inputs.values
	return context
}

func (sourced sourcedJob) newCandidate(job workflow.Job, matrix map[string]any, key string, services []workflow.Service) JobInstance {
	candidate := JobInstance{
		Key: key, LogicalJobID: job.ID, Matrix: matrix, Inputs: cloneAnyMap(sourced.reusable.inputs.values),
		FailFast: job.FailFast, MaxParallel: job.MaxParallel, Steps: append([]workflow.Step(nil), job.Steps...),
		Env: cloneMap(job.Env), Permissions: permissionScopes(job.Permissions), If: job.If,
		ContinueOnError: job.ContinueOnError, TimeoutMinutes: job.TimeoutMinutes,
		DefaultShell: job.DefaultShell, DefaultWorkingDirectory: job.DefaultWorkingDirectory,
		Outputs: cloneMap(job.Outputs), Container: job.Container, Services: services,
		ServicesExpression: job.ServicesExpression, SourcePath: sourced.reusable.provenance.path, SourceDigest: sourced.reusable.provenance.digest,
		RemoteWorkflow: cloneRemoteWorkflowSource(sourced.reusable.provenance.remote), RepositoryRoot: sourced.reusable.provenance.root, Source: job.Span,
		reusable: sourced.reusable.authority,
	}
	for _, guard := range sourced.reusable.callGuards {
		candidate.CallGuards = append(candidate.CallGuards, CallGuard{Condition: guard.condition, Inputs: cloneAnyMap(guard.inputs.values)})
	}
	return candidate
}

func (sourced sourcedJob) blockedBy(failedJobs map[string]bool) bool {
	if bindingsFailed(sourced.reusable.needBindings, failedJobs) || bindingsFailed(sourced.reusable.inputs.deferred, failedJobs) {
		return true
	}
	for _, guard := range sourced.reusable.callGuards {
		if bindingsFailed(guard.needBindings, failedJobs) || bindingsFailed(guard.inputs.deferred, failedJobs) {
			return true
		}
	}
	return false
}

func bindingsFailed(bindings map[string]needBinding, failedJobs map[string]bool) bool {
	for _, binding := range bindings {
		for _, member := range binding.members {
			if failedJobs[member] {
				return true
			}
		}
	}
	return false
}

func (sourced sourcedJob) bindInstanceDependencies(key string, instance *JobInstance, byLogicalID map[string][]JobInstance) ([]error, bool) {
	job := sourced.Job
	var diagnostics []error
	for _, need := range sortedKeys(sourced.reusable.needBindings) {
		binding := sourced.reusable.needBindings[need]
		var members []string
		for _, member := range binding.members {
			for _, prerequisite := range byLogicalID[member] {
				members = append(members, prerequisite.Key)
			}
		}
		sort.Strings(members)
		if len(members) == 0 {
			diagnostics = append(diagnostics, sourced.graphFinding(key, jobError(sourced.reusable.provenance.path, job, fmt.Sprintf("prerequisite %q has no expanded instances", need))))
			return diagnostics, true
		}
		if instance.NeedGroups == nil {
			instance.NeedGroups = make(map[string][]string, len(sourced.reusable.needBindings))
		}
		instance.NeedGroups[need] = members
		instance.Needs = append(instance.Needs, members...)
		if binding.projectOutputs {
			diagnostics = append(diagnostics, sourced.projectNeedOutputs(need, binding, instance, byLogicalID)...)
		}
	}
	deferredInputs, dependencies, err := resolveDeferredInputBindings(sourced.reusable.inputs.deferred, byLogicalID)
	if err != nil {
		diagnostics = append(diagnostics, sourced.graphFinding(key, jobError(sourced.reusable.provenance.path, job, fmt.Sprintf("resolve deferred reusable-workflow inputs: %v", err))))
		return diagnostics, true
	}
	instance.DeferredInputs = deferredInputs
	instance.Needs = append(instance.Needs, dependencies...)
	for guardIndex, guard := range sourced.reusable.callGuards {
		groups, outputs, dependencies, err := resolveCallGuardBindings(guard.needBindings, byLogicalID)
		if err != nil {
			diagnostics = append(diagnostics, sourced.graphFinding(key, jobError(sourced.reusable.provenance.path, job, fmt.Sprintf("resolve reusable-workflow call guard: %v", err))))
			return diagnostics, true
		}
		instance.CallGuards[guardIndex].NeedGroups = groups
		instance.CallGuards[guardIndex].NeedOutputs = outputs
		instance.Needs = append(instance.Needs, dependencies...)
		instance.CallGuards[guardIndex].DeferredInputs, dependencies, err = resolveDeferredInputBindings(guard.inputs.deferred, byLogicalID)
		if err != nil {
			diagnostics = append(diagnostics, sourced.graphFinding(key, jobError(sourced.reusable.provenance.path, job, fmt.Sprintf("resolve reusable-workflow call guard inputs: %v", err))))
			return diagnostics, true
		}
		instance.Needs = append(instance.Needs, dependencies...)
	}
	sort.Strings(instance.Needs)
	instance.Needs = slices.Compact(instance.Needs)
	return diagnostics, false
}

func (sourced sourcedJob) graphFinding(key string, err error) error {
	return attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", sourced.reusable.provenance.path, 0, 0, sourced.ID, key, "", 0, err)
}

func (sourced sourcedJob) projectNeedOutputs(need string, binding needBinding, instance *JobInstance, byLogicalID map[string][]JobInstance) []error {
	if instance.NeedOutputs == nil {
		instance.NeedOutputs = make(map[string][]NeedOutput)
	}
	var diagnostics []error
	projected := []NeedOutput{}
	for _, output := range binding.outputs {
		producers := byLogicalID[output.member]
		if len(producers) == 0 {
			diagnostics = append(diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", fmt.Errorf("%s:%d:%d: workflow_call output %q selects unexpanded job %q", output.path, output.span.Start.Line, output.span.Start.Column, output.name, output.member)))
			continue
		}
		if len(projected)+len(producers) > plan.MaxNeedOutputs {
			diagnostics = append(diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", fmt.Errorf("%s:%d:%d: workflow_call output %q expands call projections beyond the maximum of %d", output.path, output.span.Start.Line, output.span.Start.Column, output.name, plan.MaxNeedOutputs)))
			continue
		}
		for _, producer := range producers {
			projected = append(projected, NeedOutput{Name: output.name, StepKey: producer.Key, Output: output.output})
		}
	}
	sortNeedOutputs(projected)
	instance.NeedOutputs[need] = projected
	return diagnostics
}

func (instance JobInstance) lowerReusablePlan(job *plan.Job, planDigests map[string]string, workflowName string) error {
	needSources, err := buildPlanNeedSources(instance, planDigests)
	if err != nil {
		return err
	}
	deferredInputs, err := buildPlanDeferredInputs(instance.DeferredInputs, planDigests)
	if err != nil {
		return fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
	}
	callGuards, err := buildPlanCallGuards(instance, planDigests)
	if err != nil {
		return err
	}
	job.Workflow = plan.Workflow{
		Path: instance.SourcePath, Name: workflowName, Digest: instance.SourceDigest,
		LogicalJobID: instance.LogicalJobID, Remote: planRemoteWorkflowSource(instance.RemoteWorkflow),
	}
	job.Inputs = cloneAnyMap(instance.Inputs)
	job.DeferredInputs = deferredInputs
	job.Dependencies = append([]string(nil), instance.Needs...)
	job.NeedSources = needSources
	job.NeedOutputs = buildPlanNeedOutputs(instance)
	job.CallGuards = callGuards
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

func resolveDeferredInputBindings(bindings map[string]needBinding, byLogicalID map[string][]JobInstance) (map[string]DeferredInput, []string, error) {
	if len(bindings) == 0 {
		return nil, nil, nil
	}
	resolved := make(map[string]DeferredInput, len(bindings))
	var dependencies []string
	for _, name := range sortedKeys(bindings) {
		input, err := resolveDeferredInputBinding(bindings[name], byLogicalID)
		if err != nil {
			return nil, nil, fmt.Errorf("input %q: %w", name, err)
		}
		resolved[name] = input
		dependencies = append(dependencies, input.Sources...)
	}
	return resolved, dependencies, nil
}

func resolveDeferredInputBinding(binding needBinding, byLogicalID map[string][]JobInstance) (DeferredInput, error) {
	var deferred DeferredInput
	for _, member := range binding.members {
		producers := byLogicalID[member]
		if len(producers) == 0 {
			return DeferredInput{}, fmt.Errorf("source job %q has no expanded instances", member)
		}
		for _, producer := range producers {
			deferred.Sources = append(deferred.Sources, producer.Key)
		}
	}
	sort.Strings(deferred.Sources)
	deferred.Sources = slices.Compact(deferred.Sources)
	if len(deferred.Sources) > plan.MaxNeedProducers {
		return DeferredInput{}, fmt.Errorf("has %d producers, maximum is %d", len(deferred.Sources), plan.MaxNeedProducers)
	}
	for _, output := range binding.outputs {
		producers := byLogicalID[output.member]
		if len(deferred.Outputs)+len(producers) > plan.MaxNeedOutputs {
			return DeferredInput{}, fmt.Errorf("output %q expands beyond the maximum of %d projections", output.output, plan.MaxNeedOutputs)
		}
		for _, producer := range producers {
			deferred.Outputs = append(deferred.Outputs, NeedOutput{Name: output.name, StepKey: producer.Key, Output: output.output})
		}
	}
	sortNeedOutputs(deferred.Outputs)
	return deferred, nil
}

func resolveCallGuardBindings(bindings map[string]needBinding, byLogicalID map[string][]JobInstance) (map[string][]string, map[string][]NeedOutput, []string, error) {
	if len(bindings) == 0 {
		return nil, nil, nil, nil
	}
	groups := make(map[string][]string, len(bindings))
	var projected map[string][]NeedOutput
	var dependencies []string
	for _, name := range sortedKeys(bindings) {
		binding := bindings[name]
		var members []string
		for _, member := range binding.members {
			for _, producer := range byLogicalID[member] {
				members = append(members, producer.Key)
			}
		}
		sort.Strings(members)
		if len(members) == 0 {
			return nil, nil, nil, fmt.Errorf("prerequisite %q has no expanded instances", name)
		}
		groups[name] = members
		dependencies = append(dependencies, members...)
		if !binding.projectOutputs {
			continue
		}
		if projected == nil {
			projected = make(map[string][]NeedOutput)
		}
		outputs := []NeedOutput{}
		for _, output := range binding.outputs {
			producers := byLogicalID[output.member]
			if len(producers) == 0 {
				return nil, nil, nil, fmt.Errorf("output %q selects unexpanded job %q", output.name, output.member)
			}
			if len(outputs)+len(producers) > plan.MaxNeedOutputs {
				return nil, nil, nil, fmt.Errorf("output %q expands projections beyond the maximum of %d", output.name, plan.MaxNeedOutputs)
			}
			for _, producer := range producers {
				outputs = append(outputs, NeedOutput{Name: output.name, StepKey: producer.Key, Output: output.output})
			}
		}
		sortNeedOutputs(outputs)
		projected[name] = outputs
	}
	return groups, projected, dependencies, nil
}

func sortNeedOutputs(outputs []NeedOutput) {
	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].Name != outputs[j].Name {
			return outputs[i].Name < outputs[j].Name
		}
		if outputs[i].StepKey != outputs[j].StepKey {
			return outputs[i].StepKey < outputs[j].StepKey
		}
		return outputs[i].Output < outputs[j].Output
	})
}
