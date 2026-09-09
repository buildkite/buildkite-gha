package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// PlanArtifact is one immutable encoded job plan and its content-addressed path.
type PlanArtifact struct {
	Job           plan.Job
	Digest        string
	Path          string
	Contents      []byte
	Authorization PlanAuthorization
}

// PlanAuthorization is same-process compiler evidence for upload admission.
// It is deliberately not part of the encoded plan: runtimes independently
// enforce capabilities, while upload policy relies only on fresh compilation.
type PlanAuthorization struct {
	DockerCapabilitySources             []string
	ProviderTokenReadCapabilitySources  []string
	ProviderTokenWriteCapabilitySources []string
	WorkflowTokenPolicyFilename         string
	GitHubTokenActions                  []string
	GitHubTokenSecretReference          bool
}

// Bundle is the complete deterministic output of static compilation.
type Bundle struct {
	IR                IR
	Plans             []PlanArtifact
	JobOutcomes       map[string]JobOutcome
	EventArtifact     *transport.Artifact
	Pipeline          []byte
	GeneratedWorkflow buildkitepipeline.Workflow
	Processing        ProcessingEvidence
}

// JobOutcome is the compiler-owned preparation result for one expanded job.
type JobOutcome string

const (
	JobPlanned JobOutcome = "planned"
	JobFailed  JobOutcome = "failed"
	JobBlocked JobOutcome = "blocked"
)

// CompileBundle compiles an unattested event snapshot with the default runner
// policy, which rejects unsupported runner labels.
func CompileBundle(path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, compilerStep string) (Bundle, error) {
	return CompileBundleWithOptions(path, source, eventSource, compilerVersion, compilerDistributionDigest, compilerStep, defaultOptions())
}

// CompileBundleWithOptions produces versioned plans and the Buildkite pipeline
// that schedules their exact static dependency graph.
func CompileBundleWithOptions(path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, compilerStep string, options Options) (Bundle, error) {
	return CompileBundleContext(context.Background(), path, source, eventSource, compilerVersion, compilerDistributionDigest, compilerStep, options)
}

// CompileBundleContext produces a complete bundle and permits cancellation
// while compilation resolves immutable public action source.
func CompileBundleContext(ctx context.Context, path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, compilerStep string, options Options) (Bundle, error) {
	bundle, err := CompileBundlePlansContext(ctx, path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
	if err != nil {
		return bundle, err
	}
	return GenerateBundlePipeline(bundle, compilerDistributionDigest, compilerStep, options)
}

// CompileBundlePlansContext constructs every immutable plan but deliberately
// stops before pipeline generation so callers can apply admission policy.
func CompileBundlePlansContext(ctx context.Context, path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest string, options Options) (Bundle, error) {
	if compilerVersion == "" {
		return Bundle{}, fmt.Errorf("compiler version is required")
	}
	if !strings.HasPrefix(compilerDistributionDigest, "sha256:") {
		return Bundle{}, fmt.Errorf("compiler distribution digest is required")
	}
	ir, compileErr := compile(ctx, path, source, eventSource, options)
	if compileErr != nil && !ir.JobGraphComplete {
		return Bundle{IR: ir}, compileErr
	}
	ir, reductionErr := reducePlanEventExpressions(ir)
	options.ActionSource = newMemoizedActionSource(options.ActionSource)
	directFailures := failedInstancesFromError(compileErr)
	for key := range failedInstancesFromError(reductionErr) {
		directFailures[key] = true
	}
	if ErrorHasUnscopedFailure(compileErr) || ErrorHasUnscopedFailure(reductionErr) {
		for _, instance := range ir.Jobs {
			directFailures[instance.Key] = true
		}
	}
	failed := failedDependencyClosure(ir, maps.Clone(directFailures))
	planningIR := irWithoutJobs(ir, failed)
	evidence, actionErr := validateActionResolutions(ctx, planningIR, options)
	bundle := Bundle{IR: ir, Processing: evidence}
	if actionErr != nil {
		actionFailureFound := false
		for _, action := range evidence.Actions {
			if !action.Passed {
				directFailures[action.Instance] = true
				actionFailureFound = true
			}
		}
		if !actionFailureFound {
			for _, instance := range planningIR.Jobs {
				directFailures[instance.Key] = true
			}
		}
		failed = failedDependencyClosure(ir, maps.Clone(directFailures))
		planningIR = irWithoutJobs(ir, failed)
	}
	plans, authorizations, planEvaluations, planErr := compilePlansWithAuthorization(ctx, planningIR, compilerVersion, compilerDistributionDigest, options)
	bundle.Processing.Plans = planEvaluations
	bundle.JobOutcomes = jobOutcomes(ir, directFailures, failed, planEvaluations)
	if len(authorizations) != len(plans) {
		return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("compiler produced %d plans and %d authorizations", len(plans), len(authorizations)))
	}
	instances := make(map[string]JobInstance, len(ir.Jobs))
	for _, instance := range ir.Jobs {
		instances[instance.Key] = instance
	}
	warnedLegacyCheckout := map[string]bool{}
	warnedUnknownCheckout := map[string]bool{}
	warnedUnknownUploadArtifact := map[string]bool{}
	warnedLegacyUploadArtifact := map[string]bool{}
	warnedUnknownDownloadArtifact := map[string]bool{}
	for _, job := range plans {
		instance, exists := instances[job.Target.StepKey]
		if !exists {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("plan target %q has no expanded job instance", job.Target.StepKey))
		}
		locks := make(map[string]plan.ActionLock, len(job.Actions))
		for _, lock := range job.Actions {
			locks[lock.ID] = lock
		}
		for stepIndex, step := range job.ExecutionJob().Steps {
			if step.Invocation == nil || step.Invocation.Lock == "" || stepIndex >= len(instance.Steps) {
				continue
			}
			for _, lock := range reachableActionLocks(locks, step.Invocation.Lock) {
				descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
				switch descriptor.Adapter {
				case actionintegration.AdapterCheckoutExactEventSHA:
					if actionintegration.CheckoutUsesFallbackContract(lock.Commit) {
						if warnedUnknownCheckout[lock.Commit] {
							continue
						}
						warnedUnknownCheckout[lock.Commit] = true
						warning := unknownCheckoutCommitWarning(instance.Steps[stepIndex].Span.Start, lock.Commit)
						warning.Path = instance.SourcePath
						warning.Job = instance.LogicalJobID
						warning.Step = stepIndex + 1
						bundle.IR.Warnings = append(bundle.IR.Warnings, warning)
						continue
					}
					release, legacy := actionintegration.LegacyCheckoutRelease(lock.Commit)
					if !legacy || warnedLegacyCheckout[release] {
						continue
					}
					warnedLegacyCheckout[release] = true
					warning := legacyCheckoutWarning(instance.Steps[stepIndex].Span.Start, release, actionintegration.CheckoutDefaultsToFullHistory(lock.Commit))
					warning.Path = instance.SourcePath
					warning.Job = instance.LogicalJobID
					warning.Step = stepIndex + 1
					bundle.IR.Warnings = append(bundle.IR.Warnings, warning)
				case actionintegration.AdapterUploadArtifactBuildkite:
					if actionintegration.UploadArtifactUsesFallbackContract(lock.Commit) {
						if warnedUnknownUploadArtifact[lock.Commit] {
							continue
						}
						warnedUnknownUploadArtifact[lock.Commit] = true
						warning := unknownUploadArtifactCommitWarning(instance.Steps[stepIndex].Span.Start, lock.Commit)
						warning.Path = instance.SourcePath
						warning.Job = instance.LogicalJobID
						warning.Step = stepIndex + 1
						bundle.IR.Warnings = append(bundle.IR.Warnings, warning)
						continue
					}
					release, legacy := actionintegration.LegacyUploadArtifactRelease(lock.Commit)
					if !legacy || warnedLegacyUploadArtifact[release] {
						continue
					}
					warnedLegacyUploadArtifact[release] = true
					bundle.IR.Warnings = append(bundle.IR.Warnings, legacyUploadArtifactWarning(instance.Steps[stepIndex].Span.Start, release))
				case actionintegration.AdapterDownloadArtifactBuildkite:
					if !actionintegration.DownloadArtifactUsesFallbackContract(lock.Commit) || warnedUnknownDownloadArtifact[lock.Commit] {
						continue
					}
					warnedUnknownDownloadArtifact[lock.Commit] = true
					warning := unknownDownloadArtifactCommitWarning(instance.Steps[stepIndex].Span.Start, lock.Commit)
					warning.Path = instance.SourcePath
					warning.Job = instance.LogicalJobID
					warning.Step = stepIndex + 1
					bundle.IR.Warnings = append(bundle.IR.Warnings, warning)
				}
			}
		}
	}
	planned := make(map[string]bool, len(plans))
	for _, job := range plans {
		planned[job.Target.StepKey] = true
	}
	warnedCacheSubstitution := map[string]bool{}
	for _, evaluation := range bundle.Processing.Actions {
		instance, exists := instances[evaluation.Instance]
		if !exists || !planned[evaluation.Instance] || evaluation.Step > len(instance.Steps) {
			continue
		}
		for _, substitution := range evaluation.CacheSubstitutions {
			if warnedCacheSubstitution[substitution.ResolvedCommit] {
				continue
			}
			warnedCacheSubstitution[substitution.ResolvedCommit] = true
			warning := substitutedCacheCommitWarning(instance.Steps[evaluation.Step-1].Span.Start, substitution)
			warning.Path = instance.SourcePath
			warning.Job = instance.LogicalJobID
			warning.Step = evaluation.Step
			bundle.IR.Warnings = append(bundle.IR.Warnings, warning)
		}
	}
	warnedReusablePermissions := false
	warnedJobPermissions := false
	for _, job := range plans {
		if job.GitHubToken == nil {
			continue
		}
		instance := instances[job.Target.StepKey]
		if instance.tokenPolicyNarrowed && !warnedReusablePermissions {
			bundle.IR.Warnings = append(bundle.IR.Warnings, reusableWorkflowTokenWarning(instance.reusableCall))
			warnedReusablePermissions = true
		}
		if (instance.jobPermissionsIgnored || jobPermissionsIgnored(job.GitHubToken.Permissions, instance.Permissions)) && !warnedJobPermissions {
			position := instance.Source.Start
			path := instance.SourcePath
			if instance.jobPermissionsIgnored && instance.reusableCall.Line != 0 {
				position = instance.reusableCall
				path = ir.Workflow.Path
			}
			warning := jobWorkflowTokenWarning(position, job.GitHubToken.Permissions)
			warning.Path = path
			warning.Job = instance.LogicalJobID
			bundle.IR.Warnings = append(bundle.IR.Warnings, warning)
			warnedJobPermissions = true
		}
	}

	artifacts := make([]PlanArtifact, len(plans))
	for i, job := range plans {
		instance := instances[job.Target.StepKey]
		if job.Target.Queue != instance.Queue {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("plan %d target %q/%q does not match job instance queue %q", i, job.Target.StepKey, job.Target.Queue, instance.Queue))
		}
		expectedRuntimeDigest := options.RuntimeDistributions[instance.Platform]
		if expectedRuntimeDigest == "" && instance.Platform == PlatformLinuxAMD64 {
			expectedRuntimeDigest = compilerDistributionDigest
		}
		if job.Runtime == nil || job.Runtime.DistributionDigest != expectedRuntimeDigest {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("plan %d runtime distribution does not match job platform %s", i, instance.Platform))
		}
		contents, err := plan.Encode(job)
		if err != nil {
			return bundle, &ProcessingFinding{Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility", Job: job.Workflow.LogicalJobID, Instance: instance.Key, Err: fmt.Errorf("encode plan for job %q: %w", job.Workflow.LogicalJobID, err)}
		}
		digest := transport.Digest(contents)
		planPath, err := buildkitepipeline.PlanPath(digest)
		if err != nil {
			return bundle, &ProcessingFinding{Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility", Job: job.Workflow.LogicalJobID, Instance: instance.Key, Err: fmt.Errorf("locate plan for job %q: %w", job.Workflow.LogicalJobID, err)}
		}
		artifacts[i] = PlanArtifact{Job: job, Digest: digest, Path: planPath, Contents: contents, Authorization: authorizations[i]}
	}
	bundle.Plans = artifacts
	for _, job := range plans {
		if !job.Event.PayloadArtifact {
			continue
		}
		contents, err := json.Marshal(ir.Event.Payload)
		if err != nil {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("encode event payload artifact: %w", err))
		}
		if len(contents) > plan.MaxEventPayloadBytes {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("event payload artifact exceeds the %d-byte limit", plan.MaxEventPayloadBytes))
		}
		digest := transport.Digest(contents)
		if digest != job.Event.PayloadDigest {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("event payload artifact does not match the plan digest"))
		}
		path, err := buildkitepipeline.EventPath(digest)
		if err != nil {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", err)
		}
		bundle.EventArtifact = &transport.Artifact{Path: path, Digest: digest, Contents: contents}
		break
	}
	if actionErr == nil && planErr == nil && len(bundle.Plans) == len(ir.Jobs) {
		bundle.Processing.PlansConstructed = true
	}
	return bundle, errors.Join(compileErr, reductionErr, actionErr, planErr)
}

func jobOutcomes(ir IR, directFailures, excluded map[string]bool, evaluations []JobEvaluation) map[string]JobOutcome {
	outcomes := make(map[string]JobOutcome, len(ir.Jobs))
	for key := range excluded {
		outcomes[key] = JobBlocked
	}
	for key := range directFailures {
		outcomes[key] = JobFailed
	}
	for _, evaluation := range evaluations {
		switch {
		case !evaluation.Evaluated:
			outcomes[evaluation.Instance] = JobBlocked
		case evaluation.Passed:
			outcomes[evaluation.Instance] = JobPlanned
		default:
			outcomes[evaluation.Instance] = JobFailed
		}
	}
	for _, instance := range ir.Jobs {
		if _, exists := outcomes[instance.Key]; !exists {
			outcomes[instance.Key] = JobBlocked
		}
	}
	return outcomes
}

func failedInstancesFromError(err error) map[string]bool {
	failed := make(map[string]bool)
	var visit func(error)
	visit = func(err error) {
		if err == nil {
			return
		}
		if finding, ok := err.(*ProcessingFinding); ok {
			if finding.Instance != "" {
				failed[finding.Instance] = true
			}
			return
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				visit(child)
			}
			return
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	return failed
}

// ErrorHasUnscopedFailure reports whether err contains a compiler failure that
// cannot be assigned to one expanded job instance.
func ErrorHasUnscopedFailure(err error) bool {
	if err == nil {
		return false
	}
	if finding, ok := err.(*ProcessingFinding); ok {
		return finding.Instance == ""
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if ErrorHasUnscopedFailure(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return ErrorHasUnscopedFailure(wrapped.Unwrap())
	}
	return true
}

// JobsOutsideFailureClosure returns jobs that neither failed with an
// instance-scoped compiler error nor depend on one. An unscoped error leaves
// no safe candidates.
func JobsOutsideFailureClosure(ir IR, err error) []JobInstance {
	failed := failedDependencyClosure(ir, failedInstancesFromError(err))
	if ErrorHasUnscopedFailure(err) {
		failed = make(map[string]bool, len(ir.Jobs))
		for _, instance := range ir.Jobs {
			failed[instance.Key] = true
		}
	}
	return irWithoutJobs(ir, failed).Jobs
}

func failedDependencyClosure(ir IR, failed map[string]bool) map[string]bool {
	if len(failed) == 0 {
		return failed
	}
	for changed := true; changed; {
		changed = false
		for _, instance := range ir.Jobs {
			if failed[instance.Key] {
				continue
			}
			for _, dependency := range instance.Needs {
				if failed[dependency] {
					failed[instance.Key] = true
					changed = true
					break
				}
			}
		}
	}
	return failed
}

func irWithoutJobs(ir IR, removed map[string]bool) IR {
	if len(removed) == 0 {
		return ir
	}
	jobs := ir.Jobs
	ir.Jobs = make([]JobInstance, 0, len(jobs)-len(removed))
	for _, instance := range jobs {
		if !removed[instance.Key] {
			ir.Jobs = append(ir.Jobs, instance)
		}
	}
	return ir
}

func reachableActionLocks(locks map[string]plan.ActionLock, root string) []plan.ActionLock {
	var reachable []plan.ActionLock
	visited := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		lock, ok := locks[id]
		if !ok {
			return
		}
		reachable = append(reachable, lock)
		for _, uses := range sortedKeys(lock.Children) {
			visit(lock.Children[uses].Lock)
		}
	}
	visit(root)
	return reachable
}

func jobPermissionsIgnored(workflowPermissions, effectivePermissions map[string]string) bool {
	normalized := make(map[string]string, len(effectivePermissions))
	for name, access := range effectivePermissions {
		if name != "id-token" {
			normalized[strings.ReplaceAll(name, "-", "_")] = access
		}
	}
	return !maps.Equal(workflowPermissions, normalized)
}

// GenerateBundlePipeline emits pipeline bytes only after plan construction and
// any caller-owned admission stage have succeeded.
func GenerateBundlePipeline(bundle Bundle, compilerDistributionDigest, compilerStep string, options Options) (Bundle, error) {
	ir, artifacts := bundle.IR, bundle.Plans
	if len(artifacts) != len(ir.Jobs) {
		return bundle, processingFinding(StagePipeline, CodePipelineGeneration, "compatibility", fmt.Errorf("compiler has %d plans for %d job instances", len(artifacts), len(ir.Jobs)))
	}
	generatedWorkflow, err := GeneratePlannedWorkflow(bundle, options)
	if err != nil {
		return bundle, processingFinding(StagePipeline, CodePipelineGeneration, "compatibility", err)
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		CompilerStep:    compilerStep,
		GroupLabel:      options.GroupLabel,
		ConcurrencyGate: generatedWorkflow.ConcurrencyGate,
		ApprovalGates:   generatedWorkflow.ApprovalGates,
		Jobs:            generatedWorkflow.Jobs,
	})
	if err != nil {
		return bundle, processingFinding(StagePipeline, CodePipelineGeneration, "compatibility", fmt.Errorf("emit Buildkite pipeline: %w", err))
	}
	bundle.Pipeline = pipeline
	bundle.GeneratedWorkflow = generatedWorkflow
	bundle.Processing.PipelineGenerated = true
	return bundle, nil
}

// GeneratePlannedWorkflow projects every successfully compiled plan into its
// runnable Buildkite job. It rejects a plan whose dependency has no plan, so a
// partial workflow cannot consume outputs or artifacts from a failed path.
func GeneratePlannedWorkflow(bundle Bundle, options Options) (buildkitepipeline.Workflow, error) {
	ir, artifacts := bundle.IR, bundle.Plans
	instances := make(map[string]JobInstance, len(ir.Jobs))
	for _, instance := range ir.Jobs {
		instances[instance.Key] = instance
	}
	planned := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		key := artifact.Job.Target.StepKey
		if planned[key] {
			return buildkitepipeline.Workflow{}, fmt.Errorf("compiler has duplicate plan for job instance %q", key)
		}
		if _, exists := instances[key]; !exists {
			return buildkitepipeline.Workflow{}, fmt.Errorf("plan target %q has no expanded job instance", key)
		}
		planned[key] = true
	}
	jobs := make([]buildkitepipeline.Job, len(artifacts))
	for i, artifact := range artifacts {
		instance := instances[artifact.Job.Target.StepKey]
		for _, dependency := range instance.Needs {
			if !planned[dependency] {
				return buildkitepipeline.Workflow{}, fmt.Errorf("planned job %q depends on job %q without a plan", instance.Key, dependency)
			}
		}
		job := artifact.Job
		jobs[i] = buildkitepipeline.Job{
			Key:                instance.Key,
			Label:              instance.Label,
			CheckLabel:         instanceCheckLabel(instance),
			Queue:              instance.Queue,
			Platform:           instance.Platform.String(),
			DistributionDigest: job.RuntimeDistributionDigest(),
			PlanDigest:         artifact.Digest,
			EventPayload:       job.Event.PayloadArtifact,
			Dependencies:       append([]string(nil), instance.Needs...),
			RequiresMise:       job.NeedsMise(),
			Cache:              instance.Cache,
			SoftFail:           job.ContinueOnError,
		}
		for _, gate := range instance.ConcurrencyGates {
			jobs[i].ConcurrencyGates = append(jobs[i].ConcurrencyGates, buildkitepipeline.ConcurrencyGate{
				ID: gate.ID, Group: buildkiteConcurrencyGroup(ir.Event.Repository, gate.Group),
			})
		}
		if instance.Platform == PlatformLinuxAMD64 {
			jobs[i].RuntimeImage = instance.RuntimeImage
			if jobs[i].RuntimeImage == "" {
				jobs[i].RuntimeImage = options.RuntimeImage
			}
		}
		if instance.ConcurrencyGroup != "" {
			jobs[i].Concurrency = 1
			jobs[i].ConcurrencyGroup = buildkiteConcurrencyGroup(ir.Event.Repository, instance.ConcurrencyGroup)
		} else if instance.MaxParallel != nil {
			jobs[i].Concurrency = *instance.MaxParallel
			workflowScope := strings.TrimPrefix(ir.Workflow.Digest, "sha256:")
			if options.StepKeyNamespace != "" {
				workflowScope += "/" + options.StepKeyNamespace
			}
			jobs[i].ConcurrencyGroup = "buildkite-gha/" + workflowScope + "/" + instance.LogicalJobID
		}
	}
	var approvalGates []buildkitepipeline.ApprovalGate
	gateKeys := make(map[string]string)
	for i := range jobs {
		instance := instances[jobs[i].Key]
		if !instance.EnvironmentApproval {
			continue
		}
		environment := instance.Environment
		// GitHub environment names are case-insensitive, so case variants
		// share one gate; the label keeps the first authored spelling.
		identity := strings.ToLower(environment)
		key, exists := gateKeys[identity]
		if !exists {
			key = environmentGateKey(options.StepKeyNamespace, environment)
			gateKeys[identity] = key
			approvalGates = append(approvalGates, buildkitepipeline.ApprovalGate{Key: key, Environment: environment})
		}
		jobs[i].ApprovalGate = key
	}
	var concurrencyGate *buildkitepipeline.ConcurrencyGate
	if ir.Workflow.ConcurrencyGroup != "" && len(jobs) != 0 {
		concurrencyGate = &buildkitepipeline.ConcurrencyGate{
			Group: buildkiteConcurrencyGroup(ir.Event.Repository, ir.Workflow.ConcurrencyGroup),
			Queue: jobs[0].Queue,
		}
	}
	return buildkitepipeline.Workflow{
		ConcurrencyGate: concurrencyGate,
		ApprovalGates:   approvalGates,
		Jobs:            jobs,
	}, nil
}

// ExpandedWorkflowJobs projects stable presentation identity and dependencies
// from an expanded IR without adding executable plan configuration. Upload uses
// these skeletons to represent per-job preparation failures without running a
// partial workflow.
func ExpandedWorkflowJobs(ir IR) []buildkitepipeline.Job {
	jobs := make([]buildkitepipeline.Job, len(ir.Jobs))
	for i, instance := range ir.Jobs {
		jobs[i] = buildkitepipeline.Job{
			Key:          instance.Key,
			Label:        instance.Label,
			CheckLabel:   instanceCheckLabel(instance),
			Dependencies: append([]string(nil), instance.Needs...),
		}
	}
	return jobs
}

func buildkiteConcurrencyGroup(repository Repository, group string) string {
	scope := strings.ToLower(repository.Owner+"/"+repository.Name) + "\x00" + canonicalConcurrencyGroup(group)
	digest := sha256.Sum256([]byte(scope))
	return "buildkite-gha/concurrency/" + hex.EncodeToString(digest[:])
}
