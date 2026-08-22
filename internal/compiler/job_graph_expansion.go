package compiler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

type jobGraphExpansionResult struct {
	instances             []JobInstance
	candidates            []JobInstance
	runtimeMatrixBoundary bool
	runtimeMatrices       []RuntimeMatrixDescriptor
	jobs                  []ParsedJob
	notEvaluatedJobs      map[string]bool
	notEvaluatedInstances map[string]bool
}

// jobGraphExpansion carries state while a flattened logical job graph is
// ordered, expanded into matrix instances, and bound to instance dependencies.
type jobGraphExpansion struct {
	path           string
	context        expression.CompileContext
	options        Options
	result         jobGraphExpansionResult
	accepted       []sourcedJob
	acceptedIndex  map[string]int
	topologyJobs   map[string]workflow.Job
	order          []string
	matricesByJob  map[string][]map[string]any
	failedMatrices map[string]bool
	failedJobs     map[string]bool
	byLogicalID    map[string][]JobInstance
	instanceKeys   map[string]string
	diagnostics    []error
}

func parsedJobs(path string, parsed *workflow.Workflow) []ParsedJob {
	jobs := make([]ParsedJob, len(parsed.Jobs))
	for i, job := range parsed.Jobs {
		jobs[i] = ParsedJob{ID: job.ID, Path: path, Source: job.Span}
	}
	return jobs
}

func processingJobs(path string, parsed *workflow.Workflow, resolved []sourcedJob) []ParsedJob {
	jobs := parsedJobs(path, parsed)
	seen := make(map[string]bool, len(jobs)+len(resolved))
	for _, job := range jobs {
		seen[job.ID] = true
	}
	for _, sourced := range resolved {
		if seen[sourced.ID] {
			continue
		}
		seen[sourced.ID] = true
		jobs = append(jobs, ParsedJob{ID: sourced.ID, Path: sourced.path, Source: sourced.Span})
	}
	return jobs
}

func jobGraphExpansionReport(expanded jobGraphExpansionResult, warnings []Warning) Report {
	return Report{
		LogicalJobs: len(expanded.jobs), Instances: len(expanded.candidates),
		Jobs: expanded.candidates, RuntimeMatrixBoundary: expanded.runtimeMatrixBoundary,
		RuntimeMatrices: expanded.runtimeMatrices, ParsedJobs: expanded.jobs, Warnings: warnings,
		NotEvaluatedJobs: expanded.notEvaluatedJobs, NotEvaluatedInstances: expanded.notEvaluatedInstances,
	}
}

// expandJobGraph resolves reusable workflow calls, then turns the parsed
// logical job graph into deterministic JobInstance values and report data.
func expandJobGraph(ctx context.Context, path string, source []byte, parsed *workflow.Workflow, context expression.CompileContext, options Options) (jobGraphExpansionResult, error) {
	resolved, runtimeMatrixBoundary, err := resolveReusableWorkflows(ctx, path, source, parsed, context, options.RepositorySource)
	if err != nil {
		notEvaluatedJobs := make(map[string]bool, len(parsed.Jobs))
		for _, job := range parsed.Jobs {
			notEvaluatedJobs[job.ID] = true
		}
		return jobGraphExpansionResult{jobs: parsedJobs(path, parsed), notEvaluatedJobs: notEvaluatedJobs, runtimeMatrixBoundary: runtimeMatrixBoundary}, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", err)
	}
	expansion := jobGraphExpansion{
		path: path, context: context, options: options,
		result: jobGraphExpansionResult{
			jobs: processingJobs(path, parsed, resolved), runtimeMatrixBoundary: runtimeMatrixBoundary,
			notEvaluatedJobs: make(map[string]bool), notEvaluatedInstances: make(map[string]bool),
		},
		acceptedIndex:  make(map[string]int, len(resolved)),
		failedJobs:     make(map[string]bool, len(resolved)),
		failedMatrices: make(map[string]bool),
		instanceKeys:   make(map[string]string),
	}
	expansion.acceptJobs(resolved)
	expansion.orderJobs()
	expansion.expandMatrices()
	expansion.expandInstances()
	return expansion.result, errors.Join(expansion.diagnostics...)
}

func (e *jobGraphExpansion) acceptJobs(resolved []sourcedJob) {
	e.accepted = make([]sourcedJob, 0, len(resolved))
	for _, sourced := range resolved {
		job := sourced.Job
		if _, exists := e.acceptedIndex[job.ID]; exists {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", sourced.path, 0, 0, job.ID, "", "", 0, jobError(sourced.path, job, fmt.Sprintf("flattened job id %q collides with another job", job.ID))))
			continue
		}
		e.acceptedIndex[job.ID] = len(e.accepted)
		e.accepted = append(e.accepted, sourced)
		if err := supported(sourced.path, job); err != nil {
			e.diagnostics = append(e.diagnostics, err)
			e.failedJobs[job.ID] = true
		}
	}
}

func (e *jobGraphExpansion) orderJobs() {
	e.topologyJobs = make(map[string]workflow.Job, len(e.accepted))
	for _, sourced := range e.accepted {
		job := sourced.Job
		for _, guard := range sourced.callGuards {
			job.Needs = append(job.Needs, bindingMembers(guard.needBindings)...)
			job.Needs = append(job.Needs, bindingMembers(guard.inputs.deferred)...)
		}
		job.Needs = append(job.Needs, bindingMembers(sourced.inputs.deferred)...)
		sort.Strings(job.Needs)
		job.Needs = slices.Compact(job.Needs)
		e.topologyJobs[sourced.ID] = job
	}
	order, err := topologicalOrder(e.path, e.topologyJobs)
	if err != nil {
		e.diagnostics = append(e.diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", err))
		order = sortedKeys(e.topologyJobs)
	}
	e.order = order
}

func (e *jobGraphExpansion) expandMatrices() {
	e.matricesByJob = make(map[string][]map[string]any, len(e.accepted))
	for _, id := range e.order {
		sourced := e.accepted[e.acceptedIndex[id]]
		job := sourced.Job
		descriptor, deferred, err := describeRuntimeMatrix(job, sourced.path, sourced.digest, sourced.needBindings, e.topologyJobs, e.matricesByJob)
		var matrices []map[string]any
		if deferred {
			e.result.runtimeMatrixBoundary = true
			line, column := matrixErrorPosition(job)
			if err == nil {
				e.result.runtimeMatrices = append(e.result.runtimeMatrices, descriptor)
				err = errors.New("runtime matrix source is valid, but continuation upload is disabled because Buildkite transport has no authoritative current-attempt fence and durable idempotency boundary")
			}
			err = locatedJobError(sourced.path, job, line, column, err.Error())
		} else {
			matrices, err = expandMatrix(sourced.path, job, e.context)
		}
		if err != nil {
			line, column := matrixErrorPosition(job)
			e.diagnostics = append(e.diagnostics, &ProcessingFinding{
				Stage: StageMatrix, Code: CodeMatrixInvalid, Category: "compatibility",
				Path: sourced.path, Line: line, Column: column, Job: job.ID,
				Message: "matrix could not be expanded or validated", Err: err,
			})
			e.failedMatrices[id] = true
			e.failedJobs[id] = true
			continue
		}
		e.matricesByJob[id] = matrices
	}
}

func matrixErrorPosition(job workflow.Job) (int, int) {
	line, column := job.Span.Start.Line, job.Span.Start.Column
	if job.Matrix == nil {
		return line, column
	}
	line, column = job.Matrix.Span.Start.Line, job.Matrix.Span.Start.Column
	if job.Matrix.Expression != nil {
		return job.Matrix.Expression.Span.Start.Line, job.Matrix.Expression.Span.Start.Column
	}
	if job.Matrix.IncludeExpression != nil {
		return job.Matrix.IncludeExpression.Span.Start.Line, job.Matrix.IncludeExpression.Span.Start.Column
	}
	if job.Matrix.ExcludeExpression != nil {
		return job.Matrix.ExcludeExpression.Span.Start.Line, job.Matrix.ExcludeExpression.Span.Start.Column
	}
	return line, column
}

func (e *jobGraphExpansion) expandInstances() {
	e.byLogicalID = make(map[string][]JobInstance, len(e.accepted))
	for _, id := range e.order {
		if e.failedMatrices[id] {
			continue
		}
		e.expandJobInstances(id)
	}
	for _, id := range e.order {
		e.result.instances = append(e.result.instances, e.byLogicalID[id]...)
	}
}

func (e *jobGraphExpansion) expandJobInstances(id string) {
	sourced := e.accepted[e.acceptedIndex[id]]
	job := sourced.Job
	jobPath := sourced.path
	jobBlocked := e.jobBlocked(sourced)
	jobFailed := e.failedJobs[id]
	matrices := e.matricesByJob[id]
	concurrencyGroups := make(map[string]struct{}, len(matrices))
	jobContext := e.context
	jobContext.Inputs = sourced.inputs.values
	for matrixIndex, matrix := range matrices {
		strategy := matrixStrategy(job, matrixIndex, len(matrices))
		instanceContext := jobContext
		instanceContext.Matrix = matrix
		instanceContext.Strategy = strategy
		compileConditionErr := supportedCompileTimeConditions(jobPath, job, jobContext, matrix)
		instanceJob := resolveCompileTimeConditions(job, jobContext, matrix)
		conditionValidationJob := instanceJob
		conditionContext := jobContext
		conditionContext.Matrix = matrix
		if resolved, err := expression.EvaluateCompileCondition(instanceJob.If, conditionContext); err == nil && !resolved {
			instanceJob.If = "false"
			instanceJob.Steps = []workflow.Step{{Name: "Statically disabled job", Kind: "run", Run: ":", If: "false", Span: job.Span}}
		}
		key, err := namespacedInstanceKey(e.options.StepKeyNamespace, job.ID, matrix)
		if err != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", jobPath, 0, 0, job.ID, "", "", 0, jobError(jobPath, job, fmt.Sprintf("create deterministic instance key: %v", err))))
			jobFailed = true
			continue
		}
		if existingJob, exists := e.instanceKeys[key]; exists {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", jobPath, 0, 0, job.ID, "", "", 0, jobError(jobPath, job, fmt.Sprintf("deterministic instance key %q collides with another instance from job %q", key, existingJob))))
			jobFailed = true
			continue
		}
		e.instanceKeys[key] = job.ID
		resolvedContainer, containerErr := resolveCompileContainer(instanceJob.Container, instanceContext)
		instanceJob.Container = resolvedContainer
		resolvedServices, serviceErr := resolveCompileServices(instanceJob.Services, instanceContext)
		candidate := newJobCandidate(sourced, instanceJob, matrix, key, resolvedServices)

		valid := true
		if containerErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, job.Span.Start.Line, job.Span.Start.Column, job.ID, key, "", 0, jobError(jobPath, job, fmt.Sprintf("resolve job container: %v", containerErr))))
			valid = false
		} else if serviceErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, job.Span.Start.Line, job.Span.Start.Column, job.ID, key, "", 0, jobError(jobPath, job, fmt.Sprintf("resolve service containers: %v", serviceErr))))
			valid = false
		} else if compileConditionErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, compileConditionErr))
			valid = false
		} else if err := supportedConditions(jobPath, conditionValidationJob, matrix, true); err != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, err))
			valid = false
		}
		labels, runsOnErr := resolveRunsOn(job, jobContext, matrix)
		if runsOnErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, runsOnPosition(job).Line, runsOnPosition(job).Column, job.ID, key, "", 0, locatedJobError(jobPath, job, runsOnPosition(job).Line, runsOnPosition(job).Column, runsOnErr.Error())))
			valid = false
		}
		candidate.RunsOn = labels
		e.result.candidates = append(e.result.candidates, candidate)
		var target RunnerTarget
		if runsOnErr == nil {
			target, err = e.options.Runners.resolve(labels, e.options.EventTrust)
			if err != nil {
				message, detail := runnerRejectionDiagnostic(err, reportableRunnerLabels(job, labels), e.options.Runners.supportedLabels(), e.options.Runners.UntrustedQueues)
				e.diagnostics = append(e.diagnostics, &ProcessingFinding{
					Stage: StageExpressions, Code: CodeExpressionInvalid, Category: "compatibility",
					Path: jobPath, Line: runsOnPosition(job).Line, Column: runsOnPosition(job).Column,
					Job: job.ID, Instance: key, Message: message, Detail: detail,
					Err: locatedJobError(jobPath, job, runsOnPosition(job).Line, runsOnPosition(job).Column, err.Error()),
				})
				valid = false
			}
		}
		concurrencyGroup, concurrencyErr := resolveConcurrency(jobPath, job.ID, job.Concurrency, jobContext, matrix)
		if concurrencyErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, concurrencyErr))
			valid = false
		}
		if cancellationErr := rejectJobCancellation(jobPath, job); cancellationErr != nil {
			position := job.Concurrency.CancelInProgressPosition
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, position.Line, position.Column, job.ID, key, "", 0, cancellationErr))
			valid = false
		}
		if concurrencyGroup != "" {
			concurrencyGroups[canonicalConcurrencyGroup(concurrencyGroup)] = struct{}{}
		}
		if !valid {
			jobFailed = true
			continue
		}
		if jobBlocked {
			e.result.notEvaluatedJobs[id] = true
			e.result.notEvaluatedInstances[key] = true
			continue
		}
		instance := candidate
		instance.Label = instanceLabel(job, matrix, e.context)
		instance.Queue = target.Queue
		instance.Platform = target.Platform
		instance.RuntimeImage = target.Image
		instance.ConcurrencyGroup = concurrencyGroup
		if e.bindInstanceDependencies(sourced, job, key, &instance) {
			jobFailed = true
			continue
		}
		e.byLogicalID[id] = append(e.byLogicalID[id], instance)
	}
	if job.MaxParallel != nil && len(concurrencyGroups) > 1 {
		position := job.Concurrency.Span.Start
		e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, position.Line, position.Column, job.ID, "", "", 0, locatedJobError(jobPath, job, position.Line, position.Column, "concurrency groups that vary by matrix cannot be combined with strategy.max-parallel")))
		jobFailed = true
	}
	e.failedJobs[id] = jobFailed || jobBlocked
}

func matrixStrategy(job workflow.Job, index, total int) map[string]any {
	strategy := map[string]any{"job-index": index, "job-total": total, "fail-fast": true, "max-parallel": total}
	if job.FailFast != nil {
		strategy["fail-fast"] = *job.FailFast
	}
	if job.MaxParallel != nil {
		strategy["max-parallel"] = *job.MaxParallel
	}
	return strategy
}

func newJobCandidate(sourced sourcedJob, job workflow.Job, matrix map[string]any, key string, services []workflow.Service) JobInstance {
	candidate := JobInstance{
		Key: key, LogicalJobID: job.ID, Matrix: matrix, Inputs: cloneAnyMap(sourced.inputs.values),
		FailFast: job.FailFast, MaxParallel: job.MaxParallel, Steps: append([]workflow.Step(nil), job.Steps...),
		Env: cloneMap(job.Env), Permissions: permissionScopes(job.Permissions), If: job.If,
		ContinueOnError: job.ContinueOnError, TimeoutMinutes: job.TimeoutMinutes,
		DefaultShell: job.DefaultShell, DefaultWorkingDirectory: job.DefaultWorkingDirectory,
		Outputs: cloneMap(job.Outputs), Container: job.Container, Services: services,
		ServicesExpression: job.ServicesExpression, SourcePath: sourced.path, SourceDigest: sourced.digest,
		RemoteWorkflow: cloneRemoteWorkflowSource(sourced.remote), RepositoryRoot: sourced.root, Source: job.Span,
		secretAuthority: sourced.secretAuthority, tokenPolicyNarrowed: sourced.tokenPolicyNarrowed,
		jobPermissionsIgnored: sourced.jobPermissionsIgnored, reusableCall: sourced.reusableCall,
	}
	for _, guard := range sourced.callGuards {
		candidate.CallGuards = append(candidate.CallGuards, CallGuard{Condition: guard.condition, Inputs: cloneAnyMap(guard.inputs.values)})
	}
	return candidate
}

func (e *jobGraphExpansion) jobBlocked(sourced sourcedJob) bool {
	if bindingsFailed(sourced.needBindings, e.failedJobs) || bindingsFailed(sourced.inputs.deferred, e.failedJobs) {
		return true
	}
	for _, guard := range sourced.callGuards {
		if bindingsFailed(guard.needBindings, e.failedJobs) || bindingsFailed(guard.inputs.deferred, e.failedJobs) {
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

func (e *jobGraphExpansion) bindInstanceDependencies(sourced sourcedJob, job workflow.Job, key string, instance *JobInstance) bool {
	for _, need := range sortedKeys(sourced.needBindings) {
		binding := sourced.needBindings[need]
		var members []string
		for _, member := range binding.members {
			for _, prerequisite := range e.byLogicalID[member] {
				members = append(members, prerequisite.Key)
			}
		}
		sort.Strings(members)
		if len(members) == 0 {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", sourced.path, 0, 0, job.ID, key, "", 0, jobError(sourced.path, job, fmt.Sprintf("prerequisite %q has no expanded instances", need))))
			return true
		}
		if instance.NeedGroups == nil {
			instance.NeedGroups = make(map[string][]string, len(sourced.needBindings))
		}
		instance.NeedGroups[need] = members
		instance.Needs = append(instance.Needs, members...)
		if binding.projectOutputs {
			e.projectNeedOutputs(sourced, need, binding, instance)
		}
	}
	deferredInputs, dependencies, err := resolveDeferredInputBindings(sourced.inputs.deferred, e.byLogicalID)
	if err != nil {
		e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", sourced.path, 0, 0, job.ID, key, "", 0, jobError(sourced.path, job, fmt.Sprintf("resolve deferred reusable-workflow inputs: %v", err))))
		return true
	}
	instance.DeferredInputs = deferredInputs
	instance.Needs = append(instance.Needs, dependencies...)
	for guardIndex, guard := range sourced.callGuards {
		groups, outputs, dependencies, err := resolveCallGuardBindings(guard.needBindings, e.byLogicalID)
		if err != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", sourced.path, 0, 0, job.ID, key, "", 0, jobError(sourced.path, job, fmt.Sprintf("resolve reusable-workflow call guard: %v", err))))
			return true
		}
		instance.CallGuards[guardIndex].NeedGroups = groups
		instance.CallGuards[guardIndex].NeedOutputs = outputs
		instance.Needs = append(instance.Needs, dependencies...)
		instance.CallGuards[guardIndex].DeferredInputs, dependencies, err = resolveDeferredInputBindings(guard.inputs.deferred, e.byLogicalID)
		if err != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", sourced.path, 0, 0, job.ID, key, "", 0, jobError(sourced.path, job, fmt.Sprintf("resolve reusable-workflow call guard inputs: %v", err))))
			return true
		}
		instance.Needs = append(instance.Needs, dependencies...)
	}
	sort.Strings(instance.Needs)
	instance.Needs = slices.Compact(instance.Needs)
	return false
}

func (e *jobGraphExpansion) projectNeedOutputs(sourced sourcedJob, need string, binding needBinding, instance *JobInstance) {
	if instance.NeedOutputs == nil {
		instance.NeedOutputs = make(map[string][]NeedOutput)
	}
	projected := []NeedOutput{}
	for _, output := range binding.outputs {
		producers := e.byLogicalID[output.member]
		if len(producers) == 0 {
			e.diagnostics = append(e.diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", fmt.Errorf("%s:%d:%d: workflow_call output %q selects unexpanded job %q", output.path, output.span.Start.Line, output.span.Start.Column, output.name, output.member)))
			continue
		}
		if len(projected)+len(producers) > plan.MaxNeedOutputs {
			e.diagnostics = append(e.diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", fmt.Errorf("%s:%d:%d: workflow_call output %q expands call projections beyond the maximum of %d", output.path, output.span.Start.Line, output.span.Start.Column, output.name, plan.MaxNeedOutputs)))
			continue
		}
		for _, producer := range producers {
			projected = append(projected, NeedOutput{Name: output.name, StepKey: producer.Key, Output: output.output})
		}
	}
	sortNeedOutputs(projected)
	instance.NeedOutputs[need] = projected
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

func topologicalOrder(path string, jobs map[string]workflow.Job) ([]string, error) {
	indegree := make(map[string]int, len(jobs))
	dependents := make(map[string][]string, len(jobs))
	for id, job := range jobs {
		indegree[id] = len(job.Needs)
		for _, need := range job.Needs {
			if _, ok := jobs[need]; !ok {
				return nil, jobError(path, job, fmt.Sprintf("needs unknown job %q", need))
			}
			dependents[need] = append(dependents[need], id)
		}
	}
	var ready []string
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) != 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(jobs) {
		var cyclic []string
		for id, degree := range indegree {
			if degree != 0 {
				cyclic = append(cyclic, id)
			}
		}
		sort.Strings(cyclic)
		return nil, jobError(path, jobs[cyclic[0]], "workflow job graph contains a cycle")
	}
	return order, nil
}
