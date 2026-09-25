package compiler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

type jobGraphExpansionResult struct {
	instances             []JobInstance
	candidates            []JobInstance
	sources               map[string]WorkflowSourceReference
	runtimeMatrixBoundary bool
	referencesVars        bool
	runtimeMatrices       []RuntimeOutputDescriptor
	continuations         []RuntimeContinuation
	skippedJobs           map[string]bool
	deferredActions       map[string][]deferredAction
	jobs                  []ParsedJob
	notEvaluatedJobs      map[string]bool
	notEvaluatedInstances map[string]bool
	warnings              []Warning
}

// jobGraphExpansion carries state while a flattened logical job graph is
// ordered, expanded into matrix instances, and bound to instance dependencies.
type jobGraphExpansion struct {
	path    string
	context expression.CompileContext
	options Options
	// workflowConcurrency records that the root workflow declares
	// `concurrency`. Its finishing marker can wait only for the steps of the
	// initial upload, so it would release the group before jobs a deferred
	// upload adds have finished; such a workflow cannot hold a deferred upload.
	workflowConcurrency bool
	result              jobGraphExpansionResult
	accepted            []sourcedJob
	acceptedIndex       map[string]int
	// prerequisites holds jobPrerequisites for every accepted job by flattened
	// logical ID. Ordering, blocked-job classification, and any later split of
	// the graph into compilation stages read this one map.
	prerequisites map[string][]string
	// topologyJobs is the accepted graph with Needs replaced by prerequisites.
	// It is the ordering view of the graph and the producer lookup for
	// runtime-matrix descriptions; it is not any job's needs scope.
	topologyJobs   map[string]workflow.Job
	order          []string
	matricesByJob  map[string][]map[string]any
	failedMatrices map[string]bool
	failedJobs     map[string]bool
	// deferred maps each job compiled by a continuation upload, rather than
	// by this compilation, to its index in result.continuations.
	deferred map[string]int
	// owners maps every job downstream of a needs-derived matrix to the
	// representative of its component: the roots whose forward closures
	// intersect, with everything they reach. mergeContinuations computes it
	// from the whole graph, so a continuation upload that compiles part of a
	// component from supplied rows sees the same components as the initial
	// compilation and reproduces its budgets.
	owners         map[string]string
	runsOnContexts map[string]expression.CompileContext
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
		Sources:     expanded.sources,
		LogicalJobs: len(expanded.jobs), Instances: len(expanded.candidates),
		Jobs: expanded.candidates, RuntimeMatrixBoundary: expanded.runtimeMatrixBoundary, ReferencesVars: expanded.referencesVars,
		RuntimeMatrices: expanded.runtimeMatrices, Continuations: expanded.continuations, ParsedJobs: expanded.jobs, Warnings: append(warnings, expanded.warnings...),
		NotEvaluatedJobs: expanded.notEvaluatedJobs, NotEvaluatedInstances: expanded.notEvaluatedInstances,
	}
}

// expandJobGraph resolves reusable workflow calls, then turns the parsed
// logical job graph into deterministic JobInstance values and report data.
func expandJobGraph(ctx context.Context, path string, source []byte, parsed *workflow.Workflow, context expression.CompileContext, options Options) (jobGraphExpansionResult, error) {
	resolved, warnings, scan, err := resolveReusableWorkflows(ctx, path, source, parsed, context, options.RepositorySource, options.WorkflowSource)
	if err != nil {
		notEvaluatedJobs := make(map[string]bool, len(parsed.Jobs))
		for _, job := range parsed.Jobs {
			notEvaluatedJobs[job.ID] = true
		}
		return jobGraphExpansionResult{jobs: parsedJobs(path, parsed), notEvaluatedJobs: notEvaluatedJobs, runtimeMatrixBoundary: scan.runtimeMatrixBoundary, referencesVars: scan.referencesVars, sources: scan.sources, warnings: warnings}, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", err)
	}
	expansion := jobGraphExpansion{
		path: path, context: context, options: options, workflowConcurrency: parsed.Concurrency != nil,
		result: jobGraphExpansionResult{
			sources: scan.sources,
			jobs:    processingJobs(path, parsed, resolved), runtimeMatrixBoundary: scan.runtimeMatrixBoundary, referencesVars: scan.referencesVars, warnings: warnings,
			notEvaluatedJobs: make(map[string]bool), notEvaluatedInstances: make(map[string]bool),
		},
		acceptedIndex:  make(map[string]int, len(resolved)),
		failedJobs:     make(map[string]bool, len(resolved)),
		failedMatrices: make(map[string]bool),
		deferred:       make(map[string]int),
		runsOnContexts: make(map[string]expression.CompileContext),
		instanceKeys:   make(map[string]string),
	}
	expansion.acceptJobs(resolved)
	expansion.orderJobs()
	expansion.expandMatrices()
	expansion.mergeContinuations()
	expansion.expandInstances()
	expansion.assignContinuationKeys()
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
			if sourced.blockerDetailUnsafe {
				err = suppressBlockerDetail(err)
			}
			e.diagnostics = append(e.diagnostics, err)
			e.failedJobs[job.ID] = true
		}
	}
}

// jobPrerequisites returns the flattened logical jobs that must be compiled
// before this job, sorted and without duplicates:
//
//   - the members of every source-visible need bound to the job;
//   - the caller needs whose outputs feed a deferred reusable-workflow input;
//   - for each enclosing call guard, the caller needs the guard condition
//     reads and the caller needs feeding that call's deferred inputs.
//
// The list decides ordering and whether a failed earlier job blocks this one.
// It is not the job's needs scope: bindInstanceDependencies derives that from
// needBindings alone, so caller needs reached only through a guard or a
// deferred input never become `needs.<id>` inside the callee.
func jobPrerequisites(sourced sourcedJob) []string {
	seen := make(map[string]struct{})
	addBindings := func(bindings map[string]needBinding) {
		for _, binding := range bindings {
			for _, member := range binding.members {
				seen[member] = struct{}{}
			}
		}
	}
	addDeferred := func(inputs map[string]deferredInput) {
		for _, input := range inputs {
			addBindings(input.needs)
		}
	}
	addBindings(sourced.needBindings)
	addDeferred(sourced.inputs.deferred)
	for _, guard := range sourced.callGuards {
		addBindings(guard.needBindings)
		addDeferred(guard.inputs.deferred)
	}
	return sortedKeys(seen)
}

func (e *jobGraphExpansion) orderJobs() {
	e.prerequisites = make(map[string][]string, len(e.accepted))
	e.topologyJobs = make(map[string]workflow.Job, len(e.accepted))
	for _, sourced := range e.accepted {
		prerequisites := jobPrerequisites(sourced)
		e.prerequisites[sourced.ID] = prerequisites
		job := sourced.Job
		job.Needs = prerequisites
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
		continuation := e.deferredContinuation(sourced)
		descriptor, deferred, err := describeRuntimeMatrix(job, sourced.path, sourced.digest, sourced.needBindings, e.topologyJobs, e.matricesByJob)
		scheduling, schedulingErr := e.validateRuntimeScheduling(sourced, descriptor, deferred)
		if schedulingErr != nil && err == nil {
			e.rejectRuntimeMatrix(sourced, schedulingErr)
			continue
		}
		if hasRuntimeRunsOn(sourced) {
			var output *string
			if value, provided := e.options.RuntimeRunsOnOutputs[id]; provided {
				output = &value
			}
			descriptor, runnerContext, err := describeRuntimeRunsOn(sourced, e.context, e.topologyJobs, e.matricesByJob, output)
			if err == nil && continuation >= 0 {
				err = errors.New("job-output-derived runs-on cannot depend on a deferred upload")
			}
			if err == nil && (e.workflowConcurrency || len(sourced.concurrencyGates) != 0) {
				err = errors.New("workflow concurrency cannot be combined with job-output-derived runs-on")
			}
			if err != nil {
				e.rejectRuntimeRunsOn(sourced, err)
				continue
			}
			e.result.runtimeMatrices = append(e.result.runtimeMatrices, descriptor)
			if output == nil {
				producerKey, err := namespacedInstanceKey(e.options.StepKeyNamespace, descriptor.ProducerJob, e.matricesByJob[descriptor.ProducerJob][0])
				if err != nil {
					e.rejectRuntimeRunsOn(sourced, err)
					continue
				}
				e.result.continuations = append(e.result.continuations, RuntimeContinuation{
					Descriptor: descriptor, ProducerStepKey: producerKey, Labels: make(map[string]string), Sources: make(map[string]RuntimeMatrixJobSource),
				})
				e.deferJob(sourced, len(e.result.continuations)-1, nil)
				continue
			}
			e.runsOnContexts[id] = runnerContext
		}
		var matrices []map[string]any
		if deferred {
			e.result.runtimeMatrixBoundary = true
			switch {
			case err != nil:
			case len(sourced.concurrencyGates) != 0:
				err = errors.New("reusable-workflow concurrency cannot be combined with a needs-derived matrix")
			case e.workflowConcurrency:
				err = errors.New("workflow concurrency cannot be combined with a needs-derived matrix")
			}
			if err != nil {
				e.rejectRuntimeMatrix(sourced, err)
				continue
			}
			// Every root is recorded, including the ones this compilation
			// skips or leaves to a later stage, so each compilation of the
			// workflow sees the same components and shares the budget the
			// same way.
			e.result.runtimeMatrices = append(e.result.runtimeMatrices, descriptor)
			rows, provided := e.options.RuntimeMatrixRows[id]
			producerStage, producerDeferred := e.deferred[descriptor.ProducerJob]
			switch {
			case e.skippedPrerequisite(id):
				// A job this matrix needs is skipped, so the matrix is
				// skipped like any other dependent, whatever its own
				// producer published.
				e.skip(id)
			case producerDeferred:
				// The producer is itself deferred, so the rows exist only
				// after the continuation that compiles it has uploaded: that
				// upload writes the child continuation whose step expands
				// this matrix. Rows supplied for it without its producer's
				// rows leave the component partly unresolved, which
				// mergeContinuations rejects.
				owner := &e.result.continuations[producerStage]
				owner.LaterMatrices = append(owner.LaterMatrices, id)
				e.deferJob(sourced, producerStage, nil)
				continue
			case !provided:
				// describeRuntimeMatrix guarantees exactly one producer instance.
				producerKey, keyErr := namespacedInstanceKey(e.options.StepKeyNamespace, descriptor.ProducerJob, e.matricesByJob[descriptor.ProducerJob][0])
				if keyErr != nil {
					e.rejectRuntimeMatrix(sourced, fmt.Errorf("runtime matrix producer %q: %w", descriptor.ProducerJob, keyErr))
					continue
				}
				e.result.continuations = append(e.result.continuations, RuntimeContinuation{
					Descriptor: descriptor, ProducerStepKey: producerKey, Labels: make(map[string]string), Sources: make(map[string]RuntimeMatrixJobSource),
					Scheduling: scheduling,
				})
				e.deferJob(sourced, len(e.result.continuations)-1, nil)
				continue
			case e.options.RuntimeMatrixSkipped[id] && len(rows) == 0:
				e.skip(id)
			case len(rows) == 0 || e.options.RuntimeMatrixSkipped[id]:
				e.rejectRuntimeMatrix(sourced, fmt.Errorf("output %q of job %q expanded to no matrix instances", descriptor.ProducerOutput, descriptor.ProducerJob))
				continue
			default:
				matrices = make([]map[string]any, len(rows))
				for i, row := range rows {
					matrices[i] = cloneAnyMap(row)
				}
			}
		} else {
			if continuation >= 0 && len(sourced.concurrencyGates) != 0 {
				e.rejectRuntimeMatrix(sourced, errors.New("reusable-workflow concurrency cannot depend on a needs-derived matrix"))
				continue
			}
			if e.skippedPrerequisite(id) {
				e.skip(id)
			}
			matrixContext := e.context
			matrixContext.Inputs = sourced.inputs.values
			matrixContext.InputsComplete = len(sourced.inputs.deferred) == 0
			matrixContext.Matrix = nil
			matrixContext.Strategy = nil
			matrices, err = expandMatrix(sourced.path, job, matrixContext)
			if err == nil && continuation >= 0 {
				e.deferJob(sourced, continuation, matrices)
			}
		}
		if err != nil {
			line, column := matrixErrorPosition(job, err)
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

// skippedPrerequisite reports whether a job needs a job this compilation
// skips because a matrix producer did not succeed.
func (e *jobGraphExpansion) skippedPrerequisite(id string) bool {
	for _, member := range e.prerequisites[id] {
		if e.result.skippedJobs[member] {
			return true
		}
	}
	return false
}

// skip marks a job the continuation upload records as skipped instead of
// compiling, because its matrix producer, or a job it needs, did not succeed.
func (e *jobGraphExpansion) skip(id string) {
	if e.result.skippedJobs == nil {
		e.result.skippedJobs = make(map[string]bool)
	}
	e.result.skippedJobs[id] = true
}

// deferJob hands a job to the continuation at index, which compiles it once
// the rows of its roots are known. matrices are the instances the job's
// static matrix expands to; a root whose rows the producer supplies has none.
func (e *jobGraphExpansion) deferJob(sourced sourcedJob, index int, matrices []map[string]any) {
	id, job := sourced.ID, sourced.Job
	e.deferred[id] = index
	deferred := &e.result.continuations[index]
	deferred.Jobs = append(deferred.Jobs, id)
	deferred.Labels[id] = instanceLabel(job, nil, e.context)
	deferred.Sources[id] = deferredJobSource(sourced)
	e.recordDeferredActions(deferred.Descriptor.Job, sourced)
	for _, matrix := range matrices {
		// reserveDeferredKeys reports keys that cannot be derived.
		key, err := namespacedInstanceKey(e.options.StepKeyNamespace, id, matrix)
		if err != nil {
			continue
		}
		if deferred.Instances == nil {
			deferred.Instances = make(map[string][]RuntimeMatrixInstance)
		}
		deferred.Instances[id] = append(deferred.Instances[id], RuntimeMatrixInstance{
			Key: key, Label: instanceLabel(job, matrix, e.context), CheckLabel: instanceCheckLabel(JobInstance{LogicalJobID: id, Matrix: matrix}),
		})
	}
}

// recordDeferredActions keeps the `uses` steps of a deferred job under its
// continuation's consumer so the bundle can resolve and record their locks.
func (e *jobGraphExpansion) recordDeferredActions(consumer string, sourced sourcedJob) {
	actions := deferredJobActions(sourced)
	if len(actions) == 0 {
		return
	}
	if e.result.deferredActions == nil {
		e.result.deferredActions = make(map[string][]deferredAction)
	}
	e.result.deferredActions[consumer] = append(e.result.deferredActions[consumer], actions...)
}

// deferredJobSource records the workflow file a deferred job came from, with
// the same fields newJobCandidate gives an expanded instance so the deferred
// upload can compare them.
func deferredJobSource(sourced sourcedJob) RuntimeMatrixJobSource {
	return RuntimeMatrixJobSource{Path: sourced.path, Digest: sourced.digest, Remote: cloneRemoteWorkflowSource(sourced.remote)}
}

// deferredContinuation assigns a provisional owner. mergeContinuations merges
// intersecting forward closures after every matrix boundary is known.
func (e *jobGraphExpansion) deferredContinuation(sourced sourcedJob) int {
	continuation := -1
	for _, member := range e.prerequisites[sourced.ID] {
		index, deferred := e.deferred[member]
		if !deferred {
			continue
		}
		continuation = index
	}
	return continuation
}

// mergeContinuations computes ownership from forward reachability, including
// supplied roots so a continuation reproduces the original component budgets.
// Outside prerequisites never acquire an owner merely by feeding a component.
func (e *jobGraphExpansion) mergeContinuations() {
	owners := make(map[string]string)
	for _, descriptor := range e.result.runtimeMatrices {
		owners[descriptor.Job] = descriptor.Job
	}
	for _, id := range e.order {
		for _, need := range e.prerequisites[id] {
			owner := owners[need]
			if owner == "" {
				continue
			}
			if prior := owners[id]; prior != "" && prior != owner {
				// Pick a stable representative and relabel the entire component,
				// including branches seen before this transitive intersection.
				keep, drop := min(prior, owner), max(prior, owner)
				for job, component := range owners {
					if component == drop {
						owners[job] = keep
					}
				}
				owner = keep
			}
			owners[id] = owner
		}
	}
	e.owners = owners
	// Output-derived runner selection and matrix scheduling remain single-root,
	// single-stage subsets. Ordinary matrix components retain joins and chains.
	// Check the complete component even when its rows were supplied by an earlier
	// stage, so joins and chains cannot silently lose scheduling inputs on replay.
	rootsByOwner := make(map[string]int)
	for _, descriptor := range e.result.runtimeMatrices {
		rootsByOwner[owners[descriptor.Job]]++
	}
	for _, descriptor := range e.result.runtimeMatrices {
		sourced := e.accepted[e.acceptedIndex[descriptor.Job]]
		if sites, _ := runtimeSchedulingSites(sourced.Job); len(sites) != 0 && rootsByOwner[owners[descriptor.Job]] > 1 {
			e.rejectRuntimeMatrix(sourced, errors.New("needs-derived scheduling cannot be combined with joined or chained matrices in the same deferred component"))
		}
		if descriptor.Shape == RuntimeRunsOnShape && rootsByOwner[owners[descriptor.Job]] != 1 {
			e.rejectRuntimeRunsOn(sourced, errors.New("job-output-derived runs-on cannot join or feed another deferred stage"))
		}
	}
	// A continuation upload supplies rows for every root of its stage at
	// once. A root of the same component without rows must belong to a later
	// stage: one whose producer the component compiles, so its rows cannot
	// exist yet.
	provided := make(map[string]bool)
	unresolved := make(map[string]bool)
	for _, descriptor := range e.result.runtimeMatrices {
		owner := owners[descriptor.Job]
		_, matrixProvided := e.options.RuntimeMatrixRows[descriptor.Job]
		_, runnerProvided := e.options.RuntimeRunsOnOutputs[descriptor.Job]
		if matrixProvided || runnerProvided {
			provided[owner] = true
		} else if owners[descriptor.ProducerJob] == "" {
			unresolved[owner] = true
		}
	}
	for owner := range provided {
		if unresolved[owner] {
			e.rejectRuntimeMatrix(e.accepted[e.acceptedIndex[owner]], errors.New("all intersecting deferred matrices must be expanded by the same continuation"))
		}
	}
	var merged []RuntimeContinuation
	indices := make(map[string]int)
	for _, original := range e.result.continuations {
		owner := owners[original.Descriptor.Job]
		index, exists := indices[owner]
		if !exists {
			index = len(merged)
			indices[owner] = index
			merged = append(merged, original)
			continue
		}
		target := &merged[index]
		target.Joined = append(target.Joined, RuntimeMatrixRoot{Descriptor: original.Descriptor, ProducerStepKey: original.ProducerStepKey})
		for job, label := range original.Labels {
			target.Labels[job] = label
			target.Sources[job] = original.Sources[job]
		}
		if target.Instances == nil {
			target.Instances = make(map[string][]RuntimeMatrixInstance)
		}
		for job, instances := range original.Instances {
			target.Instances[job] = instances
		}
		target.LaterMatrices = append(target.LaterMatrices, original.LaterMatrices...)
		if actions := e.result.deferredActions[original.Descriptor.Job]; len(actions) != 0 {
			e.result.deferredActions[target.Descriptor.Job] = append(e.result.deferredActions[target.Descriptor.Job], actions...)
		}
	}
	for i := range merged {
		merged[i].Jobs = nil
	}
	for _, id := range e.order {
		if _, deferred := e.deferred[id]; deferred {
			index := indices[owners[id]]
			e.deferred[id] = index
			merged[index].Jobs = append(merged[index].Jobs, id)
		}
	}
	e.result.continuations = merged
}

// rejectRuntimeMatrix records why a needs-derived matrix, or a job that
// depends on one, cannot be compiled. The reason quotes only job and output
// identifiers from the workflow, never event data, so it can travel in Detail.
func (e *jobGraphExpansion) rejectRuntimeMatrix(sourced sourcedJob, err error) {
	job := sourced.Job
	line, column := matrixErrorPosition(job, err)
	e.diagnostics = append(e.diagnostics, &ProcessingFinding{
		Stage: StageMatrix, Code: CodeMatrixInvalid, Category: "compatibility",
		Blocker: "expression",
		Path:    sourced.path, Line: line, Column: column, Job: job.ID,
		Message: runtimeMatrixDeferredMessage, Detail: err.Error(),
		Err: locatedJobError(sourced.path, job, line, column, err.Error()),
	})
	e.failedMatrices[job.ID] = true
	e.failedJobs[job.ID] = true
}

// reserveDeferredKeys registers the step keys deferred jobs take when their
// continuation uploads them: the logical key the consumer uses as a skipped
// placeholder (its expanded keys are unknown until the producer runs), one
// key per known static instance of every dependent (an empty row takes the
// logical key, so duplicate empty rows collide here), and the approval gate
// key of every environment a deferred job declares. A collision with a key
// the initial upload creates is reported now, because the later upload would
// otherwise be rejected for a duplicate key with no way to recover, and a
// static job that owned a deferred gate's key would let the continuation take
// that job for the approval block and run protected jobs unapproved.
func (e *jobGraphExpansion) reserveDeferredKeys() {
	// Gates the initial upload may create for static jobs take their keys
	// first, so deferred keys cannot land on them. Static jobs that collide
	// with their own gates already fail the initial upload loudly.
	gates := make(map[string]bool)
	for _, id := range e.order {
		if _, deferred := e.deferred[id]; deferred {
			continue
		}
		if environment := e.accepted[e.acceptedIndex[id]].Environment; environment != "" {
			key := environmentGateKey(e.options.StepKeyNamespace, environment)
			gates[key] = true
			if _, exists := e.instanceKeys[key]; !exists {
				e.instanceKeys[key] = id
			}
		}
	}
	for _, id := range e.order {
		if _, deferred := e.deferred[id]; !deferred {
			continue
		}
		sourced := e.accepted[e.acceptedIndex[id]]
		var keys []string
		continuation := e.result.continuations[e.deferred[id]]
		for _, root := range continuation.Roots() {
			if root.Descriptor.Job == id {
				keys = []string{LogicalJobStepKey(e.options.StepKeyNamespace, id)}
			}
		}
		if slices.Contains(continuation.LaterMatrices, id) {
			// A later matrix takes its placeholder key and the key of the
			// child step that expands it.
			keys = []string{LogicalJobStepKey(e.options.StepKeyNamespace, id), continuationStepKey(e.options.StepKeyNamespace, id)}
		}
		for _, matrix := range e.matricesByJob[id] {
			key, err := namespacedInstanceKey(e.options.StepKeyNamespace, id, matrix)
			if err != nil {
				e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", sourced.path, 0, 0, id, "", "", 0, jobError(sourced.path, sourced.Job, fmt.Sprintf("create deterministic instance key: %v", err))))
				continue
			}
			keys = append(keys, key)
		}
		for _, key := range keys {
			if owner, exists := e.instanceKeys[key]; exists {
				e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", sourced.path, 0, 0, id, "", "", 0, jobError(sourced.path, sourced.Job, fmt.Sprintf("deferred instance key %q collides with a step from job %q", key, owner))))
				continue
			}
			e.instanceKeys[key] = id
		}
		if sourced.Environment == "" {
			continue
		}
		key := environmentGateKey(e.options.StepKeyNamespace, sourced.Environment)
		// Deferred jobs that share an environment share its gate.
		if gates[key] {
			continue
		}
		if owner, exists := e.instanceKeys[key]; exists {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", sourced.path, 0, 0, id, "", "", 0, jobError(sourced.path, sourced.Job, fmt.Sprintf("approval gate key %q for environment %q collides with a step from job %q", key, sourced.Environment, owner))))
			continue
		}
		gates[key] = true
		e.instanceKeys[key] = id
	}
}

// assignContinuationKeys gives each continuation its deferred upload step key
// once every static and deferred instance key is known, so collisions are
// reported.
func (e *jobGraphExpansion) assignContinuationKeys() {
	e.reserveDeferredKeys()
	// The components share the jobs the graph bound leaves after the static
	// graph equally, so together they cannot exceed it whatever the producers
	// publish. Each share must at least hold the jobs the continuation already
	// promised: one row per root and every dependent instance.
	// A continuation upload recompiles with rows supplied for the roots of
	// its stage. The instances that compiles belong to the component, not to
	// the static graph, so every compilation of the workflow derives the same
	// shares. A stage that leaves later matrices to a child continuation
	// passes on the share it did not use itself.
	staticJobs := 0
	for _, candidate := range e.result.candidates {
		if e.owners[candidate.LogicalJobID] == "" {
			staticJobs++
		}
	}
	components := make(map[string]bool)
	for _, descriptor := range e.result.runtimeMatrices {
		components[e.owners[descriptor.Job]] = true
	}
	uploads := len(components)
	budget := 0
	if uploads != 0 {
		budget = (MaxRuntimeMatrixGraphJobs - staticJobs) / uploads
	}
	for i := range e.result.continuations {
		continuation := &e.result.continuations[i]
		consumer := continuation.Descriptor.Job
		sourced := e.accepted[e.acceptedIndex[consumer]]
		if promised := len(continuation.Roots()) + continuation.DependentInstances(); budget < promised {
			e.rejectRuntimeMatrix(sourced, fmt.Errorf("the graph bound of %d jobs leaves %d for each of the workflow's %d deferred uploads after its %d static jobs, but jobs %s already need %d", MaxRuntimeMatrixGraphJobs, max(budget, 0), uploads, staticJobs, quotedList(continuation.Jobs), promised))
			continue
		}
		continuation.JobBudget = budget
		key := LogicalJobStepKey(e.options.StepKeyNamespace, consumer) + "-" + continuation.Descriptor.Kind()
		if owner, exists := e.instanceKeys[key]; exists {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", sourced.path, 0, 0, consumer, "", "", 0, jobError(sourced.path, sourced.Job, fmt.Sprintf("deferred upload step key %q collides with a step from job %q", key, owner))))
			continue
		}
		e.instanceKeys[key] = consumer
		continuation.StepKey = key
		line, column := matrixErrorPosition(sourced.Job, nil)
		code, subject := "W_MATRIX_DEFERRED", "matrix values"
		if continuation.Descriptor.Shape == RuntimeRunsOnShape {
			code, subject = "W_RUNS_ON_DEFERRED", "runs-on values"
			position := runsOnPosition(sourced.Job)
			line, column = position.Line, position.Column
		}
		message := fmt.Sprintf("%s come from output %q of job %q; jobs %s are compiled and uploaded by step %q after that job finishes, so this report leaves them not-evaluated", subject, continuation.Descriptor.ProducerOutput, continuation.Descriptor.ProducerJob, quotedList(continuation.Jobs), key)
		if len(continuation.LaterMatrices) != 0 {
			message += fmt.Sprintf("; the matrices of jobs %s come from jobs that upload compiles, so later steps expand them in turn", quotedList(continuation.LaterMatrices))
		}
		e.result.warnings = append(e.result.warnings, Warning{Code: code, Path: sourced.path, Line: line, Column: column, Job: consumer, Message: message})
	}
}

func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return strings.Join(quoted, ", ")
}

func matrixErrorPosition(job workflow.Job, err error) (int, int) {
	line, column := job.Span.Start.Line, job.Span.Start.Column
	if job.Matrix == nil {
		return line, column
	}
	var positioned matrixPositionError
	if errors.As(err, &positioned) {
		return positioned.line, positioned.column
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
		if e.failedMatrices[id] || e.result.skippedJobs[id] {
			continue
		}
		if _, deferred := e.deferred[id]; deferred {
			e.result.notEvaluatedJobs[id] = true
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
	job, schedulingGroup, schedulingErr := resolveRuntimeScheduling(sourced.Job, e.options.RuntimeSchedulingOutputs[id])
	if schedulingErr != nil {
		e.rejectRuntimeMatrix(sourced, schedulingErr)
		return
	}
	jobPath := sourced.path
	jobBlocked := e.jobBlocked(id)
	jobFailed := e.failedJobs[id]
	matrices := e.matricesByJob[id]
	concurrencyGroups := make(map[string]struct{}, len(matrices))
	jobContext := e.context
	jobContext.Inputs = sourced.inputs.values
	jobContext.InputsComplete = len(sourced.inputs.deferred) == 0
	for matrixIndex, matrix := range matrices {
		strategy := matrixStrategy(job, matrixIndex, len(matrices))
		instanceContext := jobContext
		instanceContext.Matrix = matrix
		instanceContext.Strategy = strategy
		// Conditions keep vars residual; see resolveCompileTimeConditions.
		conditionContext := jobContext
		conditionContext.Vars = nil
		compileConditionErr := supportedCompileTimeConditions(jobPath, job, conditionContext)
		if sourced.blockerDetailUnsafe {
			compileConditionErr = suppressBlockerDetail(compileConditionErr)
		}
		instanceJob := resolveCompileTimeConditions(job, conditionContext, matrix)
		conditionValidationJob := instanceJob
		conditionContext.Matrix = matrix
		if value, err := evaluateCompileSite(instanceJob.If, expression.ProfileCompileJobCondition, expression.ResultBoolean, conditionContext); err == nil && !value.(bool) {
			instanceJob.If = "false"
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
		controlContext := instanceContext
		controlContext.Vars = nil
		resolvedContinueOnError, continueOnErrorErr := resolveJobContinueOnError(instanceJob, controlContext)
		instanceJob = resolvedContinueOnError
		candidate := newJobCandidate(sourced, instanceJob, matrix, key, resolvedServices)

		valid := true
		if containerErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, job.Span.Start.Line, job.Span.Start.Column, job.ID, key, "", 0, jobError(jobPath, job, fmt.Sprintf("resolve job container: %v", containerErr))))
			valid = false
		} else if serviceErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, job.Span.Start.Line, job.Span.Start.Column, job.ID, key, "", 0, jobError(jobPath, job, fmt.Sprintf("resolve service containers: %v", serviceErr))))
			valid = false
		} else if continueOnErrorErr != nil {
			position := job.ContinueOnErrorSpan.Start
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, position.Line, position.Column, job.ID, key, "", 0, locatedJobError(jobPath, job, position.Line, position.Column, fmt.Sprintf("resolve job continue-on-error: %v", continueOnErrorErr))))
			valid = false
		} else if compileConditionErr != nil {
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, compileConditionErr))
			valid = false
		} else if err := supportedConditions(jobPath, conditionValidationJob); err != nil {
			if sourced.blockerDetailUnsafe {
				err = suppressBlockerDetail(err)
			}
			e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, err))
			valid = false
		}
		runnerContext := instanceContext
		if supplied, ok := e.runsOnContexts[id]; ok {
			runnerContext = supplied
			runnerContext.Strategy = strategy
		}
		labels, runsOnErr := resolveRunsOn(job, runnerContext, matrix)
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
				reportableLabels := reportableRunnerLabels(job, labels)
				if sourced.blockerDetailUnsafe {
					reportableLabels = nil
				}
				message, detail := runnerRejectionDiagnostic(err, reportableLabels, e.options.Runners.supportedLabels(), e.options.Runners.UntrustedQueues)
				e.diagnostics = append(e.diagnostics, &ProcessingFinding{
					Stage: StageExpressions, Code: CodeExpressionInvalid, Category: "compatibility",
					Blocker: "runner_label", BlockerDetail: runnerRejectionBlockerDetail(err, reportableLabels),
					Path: jobPath, Line: runsOnPosition(job).Line, Column: runsOnPosition(job).Column,
					Job: job.ID, Instance: key, Message: message, Detail: detail,
					Err: locatedJobError(jobPath, job, runsOnPosition(job).Line, runsOnPosition(job).Column, err.Error()),
				})
				valid = false
			} else if target.Cache != nil && instanceJob.Container != nil {
				e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, job.Span.Start.Line, job.Span.Start.Column, job.ID, key, "", 0, jobError(jobPath, job, "runner cache volumes are unsupported for jobs with a container")))
				valid = false
			}
		}
		var concurrencyGroup string
		var concurrencyErr error
		if schedulingGroup != nil {
			concurrencyGroup = *schedulingGroup
		} else {
			concurrencyGroup, concurrencyErr = resolveConcurrency(jobPath, job.ID, job.Concurrency, instanceContext, matrix)
		}
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
		instance.Label = instanceLabel(job, matrix, instanceContext)
		instance.Queue = target.Queue
		instance.Platform = target.Platform
		instance.RuntimeImage = target.Image
		instance.Agents = target.Agents
		instance.ToolCache = target.ToolCache
		instance.Cache = target.Cache
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

func resolveJobContinueOnError(job workflow.Job, context expression.CompileContext) (workflow.Job, error) {
	if job.ContinueOnErrorExpression == "" {
		return job, nil
	}
	reduced, err := reduceCompileSite(job.ContinueOnErrorExpression, expression.ProfileJobControl, expression.ResultBoolean, context)
	if err != nil {
		return job, err
	}
	if reduced.Known {
		job.ContinueOnError = reduced.Value.(bool)
		job.ContinueOnErrorExpression = ""
	} else {
		job.ContinueOnErrorExpression = reduced.Source
	}
	return job, nil
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
		CacheMode: job.CacheMode,
		FailFast:  job.FailFast, MaxParallel: job.MaxParallel, Steps: append([]workflow.Step(nil), job.Steps...),
		Env: cloneMap(job.Env), Permissions: permissionScopes(job.Permissions), If: job.If, Environment: job.Environment,
		ContinueOnError: job.ContinueOnError, ContinueOnErrorExpression: job.ContinueOnErrorExpression, ContinueOnErrorSpan: job.ContinueOnErrorSpan, TimeoutMinutes: job.TimeoutMinutes,
		DefaultShell: job.DefaultShell, DefaultWorkingDirectory: job.DefaultWorkingDirectory,
		Outputs: cloneMap(job.Outputs), Container: job.Container, Services: services,
		ConcurrencyGates:   append([]WorkflowConcurrencyGate(nil), sourced.concurrencyGates...),
		ServicesExpression: job.ServicesExpression, SourcePath: sourced.path, SourceDigest: sourced.digest,
		RemoteWorkflow: cloneRemoteWorkflowSource(sourced.remote), BlockerDetailUnsafe: sourced.blockerDetailUnsafe,
		RepositoryRoot: sourced.root, Source: job.Span,
		secretAuthority: sourced.secretAuthority, tokenPolicyNarrowed: sourced.tokenPolicyNarrowed,
		jobPermissionsIgnored: sourced.jobPermissionsIgnored, reusableCall: sourced.reusableCall,
	}
	for _, guard := range sourced.callGuards {
		candidate.CallGuards = append(candidate.CallGuards, CallGuard{Condition: guard.condition, Inputs: cloneAnyMap(guard.inputs.values)})
	}
	return candidate
}

// jobBlocked reports whether any prerequisite of the job failed, in which
// case the job's instances are recorded but not evaluated.
func (e *jobGraphExpansion) jobBlocked(id string) bool {
	for _, prerequisite := range e.prerequisites[id] {
		if e.failedJobs[prerequisite] {
			return true
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

func resolveDeferredInputBindings(inputs map[string]deferredInput, byLogicalID map[string][]JobInstance) (map[string]DeferredInput, []string, error) {
	if len(inputs) == 0 {
		return nil, nil, nil
	}
	resolved := make(map[string]DeferredInput, len(inputs))
	var dependencies []string
	for _, name := range sortedKeys(inputs) {
		input := inputs[name]
		groups, outputs, inputDependencies, err := resolveCallGuardBindings(input.needs, byLogicalID)
		if err != nil {
			return nil, nil, fmt.Errorf("input %q: %w", name, err)
		}
		resolved[name] = DeferredInput{Template: input.template, Type: input.inputType, NeedGroups: groups, NeedOutputs: outputs}
		dependencies = append(dependencies, inputDependencies...)
	}
	return resolved, dependencies, nil
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
