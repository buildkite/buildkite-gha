package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// continueRetryGuidance tells the reader how to recover from a continuation
// failure. Retrying only the continuation step is safe when nothing about the
// producer changed; anything that changed the producer needs a new build.
const continueRetryGuidance = "Retry the whole build to expand this matrix again. If the matrix producer job was retried, only a new build can expand it."

type continueOptions struct {
	digest   string
	producer string
}

func continueArgs(args []string) (continueOptions, error) {
	var options continueOptions
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch option := args[i]; option {
		case "--continuation-digest", "--continuation-producer":
			if seen[option] {
				return continueOptions{}, fmt.Errorf("%s may only be specified once", option)
			}
			seen[option] = true
			i++
			if i == len(args) {
				return continueOptions{}, fmt.Errorf("%s requires a value", option)
			}
			if option == "--continuation-digest" {
				options.digest = args[i]
			} else {
				options.producer = args[i]
			}
		default:
			return continueOptions{}, fmt.Errorf("unknown option %q", option)
		}
	}
	if !continuationDigestPattern.MatchString(options.digest) {
		return continueOptions{}, errors.New("--continuation-digest requires a sha256 digest")
	}
	if options.producer == "" {
		return continueOptions{}, errors.New("--continuation-producer requires the importer job")
	}
	return options, nil
}

// continueUpload implements `buildkite-gha continue`: the deferred pipeline
// upload that expands one needs-derived matrix after its producer job ran.
func continueUpload(args []string, stdout, stderr io.Writer, version, clientVersion string, agent transport.Agent) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return continueUploadContext(ctx, args, stdout, stderr, version, clientVersion, agent)
}

func continueUploadContext(ctx context.Context, args []string, stdout, stderr io.Writer, version, clientVersion string, agent transport.Agent) int {
	options, err := continueArgs(args)
	if err != nil {
		return usageError(stderr, "continue: %v", err)
	}
	if os.Getenv("BUILDKITE") != "true" || os.Getenv("BUILDKITE_BUILD_ID") == "" || os.Getenv("BUILDKITE_JOB_ID") == "" {
		return usageError(stderr, "continue: BUILDKITE=true, BUILDKITE_BUILD_ID, and BUILDKITE_JOB_ID are required")
	}
	fail := func(format string, args ...any) int {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: continue: "+format+"\n", args...)
		_, _ = fmt.Fprintln(stderr, continueRetryGuidance)
		return 1
	}
	root, err := os.MkdirTemp("", "buildkite-gha-continue-")
	if err != nil {
		return fail("create working directory: %v", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	_, _ = fmt.Fprintln(stdout, "~~~ :github: Read continuation")
	artifactPath, err := buildkitepipeline.ContinuationPath(options.digest)
	if err != nil {
		return fail("%v", err)
	}
	downloadDir := filepath.Join(root, "continuation")
	if err := agent.DownloadArtifact(ctx, artifactPath, downloadDir, options.producer); err != nil {
		return fail("download continuation from importer %q: %v", options.producer, err)
	}
	data, err := readBoundedFile(filepath.Join(downloadDir, filepath.FromSlash(artifactPath)), maxContinuationArtifactBytes)
	if err != nil {
		return fail("read continuation: %v", err)
	}
	if actual := sha256Digest(data); actual != options.digest {
		return fail("continuation digest %s does not match expected %s", actual, options.digest)
	}
	artifact, err := decodeContinuationArtifact(data, version)
	if err != nil {
		return fail("%v", err)
	}
	if artifact.Importer != options.producer {
		return fail("continuation was written by importer %q, not %q", artifact.Importer, options.producer)
	}
	eventPath, err := buildkitepipeline.ContinuationEventPath(artifact.Event.Digest)
	if err != nil {
		return fail("%v", err)
	}
	if err := agent.DownloadArtifact(ctx, eventPath, downloadDir, options.producer); err != nil {
		return fail("download event from importer %q: %v", options.producer, err)
	}
	eventSource, err := readBoundedFile(filepath.Join(downloadDir, filepath.FromSlash(eventPath)), plan.MaxEventPayloadBytes)
	if err != nil {
		return fail("read event: %v", err)
	}
	if actual := sha256Digest(eventSource); actual != artifact.Event.Digest {
		return fail("event digest %s does not match the continuation's %s", actual, artifact.Event.Digest)
	}
	checkout := os.Getenv("BUILDKITE_BUILD_CHECKOUT_PATH")
	if checkout == "" {
		checkout, err = os.Getwd()
		if err != nil {
			return fail("resolve checkout: %v", err)
		}
	}
	workflowPath := filepath.Join(checkout, filepath.FromSlash(artifact.Workflow.Path))
	workflowSource, err := readBoundedFile(workflowPath, maxContinuationWorkflowBytes)
	if err != nil {
		return fail("read workflow %s: %v", artifact.Workflow.Path, err)
	}
	if actual := sha256Digest(workflowSource); actual != artifact.Workflow.Digest {
		return fail("workflow %s in the checkout differs from the workflow the importer compiled", artifact.Workflow.Path)
	}

	run := continuationRun{
		ctx: ctx, stdout: stdout, stderr: stderr, version: version, clientVersion: clientVersion,
		agent: agent, root: root, artifact: artifact, eventSource: eventSource, workflowPath: workflowPath, workflowSource: workflowSource,
		buildID: os.Getenv("BUILDKITE_BUILD_ID"), jobID: os.Getenv("BUILDKITE_JOB_ID"),
		out: newProcessingOutput(ctx, "continue", "text", stderr, stderr, agent),
	}
	return run.execute(fail)
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("%s is %d bytes, maximum is %d", path, info.Size(), limit)
	}
	return os.ReadFile(path)
}

type continuationRun struct {
	ctx                    context.Context
	stdout, stderr         io.Writer
	version, clientVersion string
	agent                  transport.Agent
	root                   string
	artifact               continuationArtifact
	eventSource            []byte
	workflowPath           string
	workflowSource         []byte
	buildID, jobID         string
	out                    processingOutput
}

// execute reads the producer result, expands the matrix, recompiles the
// workflow, and uploads only the deferred jobs.
func (r continuationRun) execute(fail func(string, ...any) int) int {
	artifact := r.artifact
	descriptor := artifact.Continuation.Descriptor
	_, _ = fmt.Fprintf(r.stdout, "~~~ :github: Read matrix from job %q output %q\n", descriptor.ProducerJob, descriptor.ProducerOutput)
	manifest, err := transport.DownloadResult(r.ctx, r.agent, r.root, r.buildID, transport.ResultSource{StepKey: artifact.Producer.StepKey, PlanDigest: artifact.Producer.PlanDigest})
	if err != nil {
		return fail("matrix producer %q result is unavailable: %v", descriptor.ProducerJob, err)
	}
	graphKeys := make([]string, 0, len(artifact.Graph))
	for _, job := range artifact.Graph {
		graphKeys = append(graphKeys, job.Key)
	}
	if manifest.Result != "success" {
		_, _ = fmt.Fprintf(r.stdout, "Matrix producer %q finished with result %q; the deferred jobs are skipped.\n", descriptor.ProducerJob, manifest.Result)
		return r.uploadSkipped(fail, manifest.Result, graphKeys)
	}
	var output *string
	for _, candidate := range manifest.Outputs {
		if strings.EqualFold(candidate.Name, descriptor.ProducerOutput) {
			value := candidate.Value
			output = &value
			break
		}
	}
	if output == nil {
		return fail("matrix producer %q did not publish output %q", descriptor.ProducerJob, descriptor.ProducerOutput)
	}
	rows, err := compiler.ExpandRuntimeMatrixOutput(descriptor, []byte(*output), graphKeys)
	if err != nil {
		return fail("matrix from job %q output %q is invalid: %v", descriptor.ProducerJob, descriptor.ProducerOutput, err)
	}
	_, _ = fmt.Fprintf(r.stdout, "Expanding job %q into %d matrix instances.\n", descriptor.Job, len(rows))
	continuation := artifact.Continuation
	if wanted := len(rows) + continuation.DependentInstances(); wanted > continuation.JobBudget {
		return fail("job %q produced %d matrix rows, but step %q may upload at most %d jobs (%d rows plus their dependents): the workflow's deferred uploads share the %d-job graph bound. Reduce the matrix or the number of deferred matrices in the workflow.",
			descriptor.ProducerJob, len(rows), continuation.StepKey, continuation.JobBudget, continuation.JobBudget-continuation.DependentInstances(), compiler.MaxRuntimeMatrixGraphJobs)
	}

	_, _ = fmt.Fprintln(r.stdout, "~~~ :github: Compile deferred jobs")
	preflight, report, err := r.compile(rows)
	if err != nil {
		_ = r.out.write(r.ctx, report)
		return fail("compile deferred jobs: %v", err)
	}
	bundle := preflight.Bundle
	if err := r.checkDrift(bundle); err != nil {
		return fail("%v", err)
	}
	pipeline, plans, createdGates, err := r.deferredPipeline(bundle, nil)
	if err != nil {
		return fail("%v", err)
	}
	if len(plans) > continuation.JobBudget {
		return fail("step %q compiled %d deferred jobs, but may upload at most %d: the workflow's deferred uploads share the %d-job graph bound", continuation.StepKey, len(plans), continuation.JobBudget, compiler.MaxRuntimeMatrixGraphJobs)
	}
	writeCompilerWarnings(r.stderr, "continue", artifact.Workflow.Path, bundle.IR.Warnings)
	_ = compatibility.WriteProcessing(r.stderr, "text", report)

	_, _ = fmt.Fprintf(r.stdout, "~~~ :github: Upload %d deferred jobs\n", len(plans))
	artifacts := make([]transport.Artifact, 0, len(plans)+1)
	if bundle.EventArtifact != nil {
		artifacts = append(artifacts, *bundle.EventArtifact)
	}
	expected := make(map[string]string, len(plans))
	for _, jobPlan := range plans {
		artifacts = append(artifacts, transport.Artifact{Path: jobPlan.Path, Digest: jobPlan.Digest, Contents: jobPlan.Contents})
		// Every bootstrap variant the emitter writes single-quotes the plan
		// digest, and a digest never contains quotes.
		expected[jobPlan.Job.Target.StepKey] = "'" + jobPlan.Digest + "'"
	}
	if err := transport.UploadArtifacts(r.ctx, r.agent, r.root, artifacts, pipeline); err != nil {
		if r.ctx.Err() != nil || !errors.Is(err, transport.ErrPipelineUpload) {
			return fail("%v", err)
		}
		if r.alreadyApplied(expected, "command") {
			_, _ = fmt.Fprintf(r.stdout, "The %d deferred jobs were already uploaded by an earlier run of this step; nothing to do.\n", len(plans))
			return 0
		}
		raced := r.existingSteps(createdGates)
		if len(raced) == 0 {
			return fail("%v", err)
		}
		if err := r.uploadAfterGateRace(bundle, raced); err != nil {
			return fail("%v", err)
		}
	}
	_, _ = fmt.Fprintf(r.stdout, "Uploaded %d jobs for %q from job %q output %q.\n", len(plans), descriptor.Job, descriptor.ProducerJob, descriptor.ProducerOutput)
	return 0
}

// compile recompiles the workflow with the expanded rows using the same hosted
// options as the importer: the recorded runner mapping, the same runner
// resolution path, the recorded variables, OIDC, the runtime digests, and the
// same repository source for remote reusable workflows and actions.
func (r continuationRun) compile(rows []map[string]any) (hostedCompilation, compatibility.ProcessingReport, error) {
	artifact := r.artifact
	consumer := artifact.Continuation.Descriptor.Job
	targets := artifact.runnerTargets()
	vars := artifact.variableSources()
	repositorySource, cleanupSource, err := r.repositorySource()
	if err != nil {
		report := compatibility.EnvironmentProcessingReport(artifact.Workflow.Path, hostedProfile, "repository source could not be configured")
		return hostedCompilation{}, report, hostedError(hostedEnvironmentFailure, err)
	}
	defer cleanupSource()
	validationOptions := hostedOptions("", targets, nil)
	validationOptions.RepositorySource = repositorySource
	validationOptions.StepKeyNamespace = artifact.Workflow.Namespace
	validationOptions.Vars = vars
	validationOptions.EventFile = artifact.Event.File
	validationOptions.RuntimeMatrixRows = map[string][]map[string]any{consumer: rows}
	validation, validationErr := compiler.ValidateEventWithOptionsContext(r.ctx, r.workflowPath, r.workflowSource, r.eventSource, validationOptions)
	resolution, err := suggestedRunnerTargets(r.ctx, []compiler.Report{validation}, targets, r.clientVersion)
	if err != nil {
		if r.ctx.Err() != nil {
			return hostedCompilation{}, compatibility.ProcessingReport{}, r.ctx.Err()
		}
		_, _ = fmt.Fprintf(r.stderr, "buildkite-gha: continue: warning: runner resolution unavailable (%v); using built-in runner presets\n", err)
	}
	if !resolution.empty() {
		applyRunnerResolution(&validationOptions, resolution)
		validation, validationErr = compiler.ValidateEventWithOptionsContext(r.ctx, r.workflowPath, r.workflowSource, r.eventSource, validationOptions)
	}
	report := compatibility.InitialProcessingReport(artifact.Workflow.Path, hostedProfile, true, validation, validationErr)
	if validationErr != nil {
		report.Result = "incompatible"
	}
	options := hostedOptions("", targets, artifact.runtimeDigests())
	applyRunnerResolution(&options, resolution)
	options.StepKeyNamespace = artifact.Workflow.Namespace
	options.OIDC = artifact.OIDC
	options.EventFile = artifact.Event.File
	options.EnvironmentSource = environmentSourceFromAgent(r.clientVersion)
	options.Vars = vars
	options.RuntimeMatrixRows = map[string][]map[string]any{consumer: rows}
	options.RuntimeMatrixActionLocks = artifact.pinnedActionLocks()
	preflight, err := compileHostedBundle(r.ctx, r.workflowPath, r.workflowSource, r.eventSource, r.version, artifact.Distribution, "pipeline-trigger-importer", options, "", repositorySource, nil)
	applyHostedPreflight(&report, preflight)
	if err != nil {
		report.Result = classifyHostedFailure(&report, artifact.Workflow.Path, err)
		return preflight, report, err
	}
	report.Result = "admitted"
	return preflight, report, nil
}

// repositorySource builds the source the importer used for remote reusable
// workflows and actions: the job-scoped GitHub token for the workflow's own
// repository and, when the importer was configured to read private reusable
// workflows, the agent's Git credentials. Validation and compilation share
// the returned memoized source, as they do in upload.
func (r continuationRun) repositorySource() (compiler.RepositorySource, func(), error) {
	privateOptions, err := privateRepositorySourceOptions(r.artifact.PrivateReusableWorkflows)
	if err != nil {
		return nil, nil, err
	}
	sourceOptions := append([]actionsource.Option(nil), privateOptions...)
	if r.artifact.Event.Provider == "github" {
		if event, eventErr := compiler.ParseEvent(r.eventSource); eventErr == nil {
			authentication := importerJobActionSourceAuthentication(r.stderr, r.clientVersion)
			if option := authentication.option(event.Repository.Owner + "/" + event.Repository.Name); option != nil {
				sourceOptions = append(sourceOptions, option)
			}
		}
	}
	return newHostedActionSource(r.ctx, "", r.clientVersion, sourceOptions, privateOptions)
}

// checkDrift proves that the recompilation reproduced the initial upload: the
// jobs the importer created must come back with the same keys, dependencies,
// and plan digests, every job the continuation adds must belong to it, come
// from the workflow source the importer resolved, and use only the action
// revisions the importer resolved, and the workflow's other continuations must
// still be deferred exactly as recorded, because their own steps expand them.
// Anything else means the inputs moved between the two compiles, and a
// pipeline with mismatched plans would be wrong to upload.
func (r continuationRun) checkDrift(bundle compiler.Bundle) error {
	artifact := r.artifact
	if err := checkContinuationsUnchanged(artifact.Others, bundle.IR.Continuations); err != nil {
		return err
	}
	recorded := make(map[string]continuationGraphJob, len(artifact.Graph))
	for _, job := range artifact.Graph {
		recorded[job.Key] = job
	}
	deferred := make(map[string]bool, len(artifact.Continuation.Jobs))
	for _, job := range artifact.Continuation.Jobs {
		deferred[job] = true
	}
	recordedLocks := make(map[string]plan.ActionLock, len(artifact.Continuation.ActionLocks))
	for _, lock := range artifact.Continuation.ActionLocks {
		recordedLocks[lock.ID] = lock
	}
	plans := make(map[string]compiler.PlanArtifact, len(bundle.Plans))
	for _, jobPlan := range bundle.Plans {
		plans[jobPlan.Job.Target.StepKey] = jobPlan
	}
	reproduced := 0
	for _, instance := range bundle.IR.Jobs {
		jobPlan, planned := plans[instance.Key]
		if !planned {
			return fmt.Errorf("job %q has no plan after recompilation", instance.Key)
		}
		digest := jobPlan.Digest
		if deferred[instance.LogicalJobID] {
			if _, exists := recorded[instance.Key]; exists {
				return fmt.Errorf("deferred job %q collides with a job the initial upload created", instance.Key)
			}
			if _, uploaded := artifact.Runtimes[instance.Platform.String()]; !uploaded {
				return fmt.Errorf("deferred job %q runs on %s, but the importer uploaded no runtime for that platform; configure --runtime-distribution for it", instance.Key, instance.Platform)
			}
			// A deferred job has no earlier plan to compare, so its workflow
			// source must be the one the importer compiled: the same file
			// bytes and, for a remote reusable workflow, the same commit.
			if source, exists := artifact.Continuation.Sources[instance.LogicalJobID]; !exists || !source.Matches(instance) {
				return fmt.Errorf("deferred job %q comes from a different workflow source than the initial upload compiled; the workflow inputs changed", instance.Key)
			}
			// Its actions must be the revisions the importer resolved: the
			// same commit and source tree for a remote action, the same tree
			// for one in the checkout.
			for _, lock := range jobPlan.Job.Actions {
				if before, exists := recordedLocks[lock.ID]; !exists || !reflect.DeepEqual(before, lock) {
					return fmt.Errorf("deferred job %q uses action %s, which the initial upload did not resolve to that revision; the workflow inputs changed", instance.Key, describeActionLock(lock))
				}
			}
			continue
		}
		before, exists := recorded[instance.Key]
		if !exists {
			return fmt.Errorf("job %q did not exist in the initial upload; the workflow inputs changed", instance.Key)
		}
		needs := append([]string(nil), instance.Needs...)
		sort.Strings(needs)
		beforeNeeds := append([]string(nil), before.Needs...)
		sort.Strings(beforeNeeds)
		if before.Job != instance.LogicalJobID || before.PlanDigest != digest || strings.Join(needs, "\x00") != strings.Join(beforeNeeds, "\x00") {
			return fmt.Errorf("job %q compiled differently from the initial upload; the workflow inputs changed", instance.Key)
		}
		reproduced++
	}
	if reproduced != len(recorded) {
		return fmt.Errorf("recompilation reproduced %d of %d jobs from the initial upload; the workflow inputs changed", reproduced, len(recorded))
	}
	return nil
}

// describeActionLock names an action lock as the workflow references it.
func describeActionLock(lock plan.ActionLock) string {
	if lock.Source == "workspace" {
		return "./" + lock.Path
	}
	name := lock.Repository
	if lock.Path != "" {
		name += "/" + lock.Path
	}
	return name + "@" + lock.RequestedRef + " (commit " + lock.Commit + ")"
}

// checkContinuationsUnchanged requires the continuations a recompilation still
// defers to be exactly the other continuations the importer recorded, by step
// key, producer instance, deferred jobs, and descriptor.
func checkContinuationsUnchanged(recorded, remaining []compiler.RuntimeMatrixContinuation) error {
	byKey := make(map[string]compiler.RuntimeMatrixContinuation, len(recorded))
	for _, continuation := range recorded {
		byKey[continuation.StepKey] = continuation
	}
	for _, continuation := range remaining {
		before, exists := byKey[continuation.StepKey]
		if !exists {
			return fmt.Errorf("recompilation defers job %q through step %q, which the initial upload did not create; the workflow differs from the initial upload", continuation.Descriptor.Job, continuation.StepKey)
		}
		if !reflect.DeepEqual(before, continuation) {
			return fmt.Errorf("deferred step %q compiled differently from the initial upload; the workflow inputs changed", continuation.StepKey)
		}
		delete(byKey, continuation.StepKey)
	}
	missing := make([]string, 0, len(byKey))
	for key := range byKey {
		missing = append(missing, key)
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		return fmt.Errorf("recompilation no longer defers job %q through step %q; the workflow differs from the initial upload", byKey[missing[0]].Descriptor.Job, missing[0])
	}
	return nil
}

// deferredPipeline emits the pipeline for the deferred jobs only. Jobs the
// initial upload created and approval gates that already exist in the build,
// or that the caller knows another continuation created (sharedGates), are
// referenced as existing steps instead of being uploaded again. It returns
// the keys of the gates the pipeline creates.
func (r continuationRun) deferredPipeline(bundle compiler.Bundle, sharedGates map[string]bool) ([]byte, []compiler.PlanArtifact, []string, error) {
	artifact := r.artifact
	deferred := make(map[string]bool, len(artifact.Continuation.Jobs))
	for _, job := range artifact.Continuation.Jobs {
		deferred[job] = true
	}
	logical := make(map[string]string, len(bundle.IR.Jobs))
	for _, instance := range bundle.IR.Jobs {
		logical[instance.Key] = instance.LogicalJobID
	}
	existing := make([]string, 0, len(artifact.Graph)+len(artifact.ApprovalGates))
	for _, job := range artifact.Graph {
		existing = append(existing, job.Key)
	}
	jobs := make([]buildkitepipeline.Job, 0)
	usedGates := make(map[string]bool)
	for _, job := range bundle.GeneratedWorkflow.Jobs {
		if !deferred[logical[job.Key]] {
			continue
		}
		if job.ApprovalGate != "" {
			usedGates[job.ApprovalGate] = true
		}
		jobs = append(jobs, job)
	}
	if len(jobs) == 0 {
		return nil, nil, nil, errors.New("recompilation produced no deferred jobs")
	}
	plans := make([]compiler.PlanArtifact, 0, len(jobs))
	for _, jobPlan := range bundle.Plans {
		if deferred[logical[jobPlan.Job.Target.StepKey]] {
			plans = append(plans, jobPlan)
		}
	}
	recordedGates := make(map[string]bool, len(artifact.ApprovalGates))
	for _, key := range artifact.ApprovalGates {
		recordedGates[key] = true
	}
	var gates []buildkitepipeline.ApprovalGate
	var created []string
	for _, gate := range bundle.GeneratedWorkflow.ApprovalGates {
		if !usedGates[gate.Key] {
			continue
		}
		// A gate the initial upload created, or that another continuation of
		// this build already uploaded, must be referenced rather than repeated.
		if recordedGates[gate.Key] || sharedGates[gate.Key] || r.stepExists(gate.Key) {
			existing = append(existing, gate.Key)
			continue
		}
		gates = append(gates, gate)
		created = append(created, gate.Key)
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		Continuation:         true,
		ArtifactProducer:     r.jobID,
		DistributionProducer: artifact.Importer,
		ExistingSteps:        existing,
		EventProvider:        artifact.Event.Provider,
		DisableRunnerUser:    !artifact.RunnerUser,
		Workflows: []buildkitepipeline.Workflow{{
			GroupLabel:    artifact.Workflow.GroupLabel,
			CheckName:     artifact.Workflow.CheckName,
			Event:         artifact.Event.Name,
			ApprovalGates: gates,
			Jobs:          jobs,
		}},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("emit deferred jobs: %w", err)
	}
	return pipeline, plans, created, nil
}

// uploadAfterGateRace handles two continuations that share an approval gate
// neither found in the build: both include it, and Buildkite rejects the
// second upload for the duplicate key. The caller passes the gates this run
// tried to create that now exist, so the pipeline is emitted again referencing
// them, still creating any gate only this continuation uses, and uploaded once
// more. The artifacts of the first attempt are already in place, so only the
// pipeline is repeated.
func (r continuationRun) uploadAfterGateRace(bundle compiler.Bundle, raced []string) error {
	shared := make(map[string]bool, len(raced))
	for _, key := range raced {
		shared[key] = true
	}
	_, _ = fmt.Fprintf(r.stdout, "Another deferred upload created approval gates %s first; uploading again with them as existing steps.\n", quotedKeys(raced))
	pipeline, _, _, err := r.deferredPipeline(bundle, shared)
	if err != nil {
		return err
	}
	if err := r.agent.UploadPipeline(r.ctx, pipeline); err != nil {
		return fmt.Errorf("%w: %w", transport.ErrPipelineUpload, err)
	}
	return nil
}

// existingSteps returns the keys that already name a step in the build.
func (r continuationRun) existingSteps(keys []string) []string {
	var existing []string
	for _, key := range keys {
		if r.stepExists(key) {
			existing = append(existing, key)
		}
	}
	return existing
}

func quotedKeys(keys []string) string {
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = fmt.Sprintf("%q", key)
	}
	return strings.Join(quoted, ", ")
}

// uploadSkipped records every deferred job as skipped when the producer did
// not succeed, so the workflow's checks and dependents resolve the same way a
// skipped static job would.
func (r continuationRun) uploadSkipped(fail func(string, ...any) int, result string, graphKeys []string) int {
	artifact := r.artifact
	producer := artifact.Continuation.Descriptor.ProducerJob
	reason := fmt.Sprintf("matrix producer job %q finished with result %s", producer, result)
	jobs := make([]buildkitepipeline.Job, 0, len(artifact.Continuation.Jobs))
	expected := make(map[string]string, len(artifact.Continuation.Jobs))
	skipped := func(key, label, checkLabel string) {
		jobs = append(jobs, buildkitepipeline.Job{
			Key: key, Label: label, CheckLabel: checkLabel, SkipReason: reason,
			Dependencies: []string{artifact.Producer.StepKey},
		})
		expected[key] = ":github: job · " + label
	}
	// The consumer's instances are unknown, so it gets one placeholder under
	// its logical key; each dependent promised one check per static instance.
	consumer := artifact.Continuation.Descriptor.Job
	skipped(compiler.LogicalJobStepKey(artifact.Workflow.Namespace, consumer), artifact.Continuation.JobLabel(consumer), consumer)
	for _, job := range artifact.Continuation.Jobs[1:] {
		for _, instance := range artifact.Continuation.Instances[job] {
			skipped(instance.Key, instance.Label, instance.CheckLabel)
		}
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		Continuation:     true,
		ArtifactProducer: r.jobID,
		ExistingSteps:    graphKeys,
		EventProvider:    artifact.Event.Provider,
		Workflows: []buildkitepipeline.Workflow{{
			GroupLabel: artifact.Workflow.GroupLabel,
			CheckName:  artifact.Workflow.CheckName,
			Event:      artifact.Event.Name,
			Jobs:       jobs,
		}},
	})
	if err != nil {
		return fail("emit skipped jobs: %v", err)
	}
	if err := r.agent.UploadPipeline(r.ctx, pipeline); err != nil {
		if r.ctx.Err() == nil && r.alreadyApplied(expected, "label") {
			_, _ = fmt.Fprintln(r.stdout, "The skipped jobs were already uploaded by an earlier run of this step; nothing to do.")
			return 0
		}
		return fail("upload skipped jobs: %v", err)
	}
	_, _ = fmt.Fprintf(r.stdout, "Uploaded %d skipped jobs for %q.\n", len(jobs), artifact.Continuation.Descriptor.Job)
	return 0
}

// alreadyApplied decides whether a rejected upload was a replay of this
// continuation's own earlier upload. Buildkite rejects a whole upload when any
// step key already exists in the build, and the deferred keys are only ever
// uploaded by this continuation, so if every expected step exists with the
// expected attribute the earlier upload succeeded and this run has nothing
// left to do. Any missing step or differing attribute means the rejection had
// another cause, and the run fails so the build can be retried.
func (r continuationRun) alreadyApplied(expected map[string]string, attribute string) bool {
	if len(expected) == 0 {
		return false
	}
	for key, want := range expected {
		value, err := r.agent.GetStepAttribute(r.ctx, key, attribute)
		if err != nil || !strings.Contains(string(value), want) {
			return false
		}
	}
	return true
}

// stepExists reports whether a step with the key is already in the build.
func (r continuationRun) stepExists(key string) bool {
	value, err := r.agent.GetStepAttribute(r.ctx, key, "key")
	return err == nil && strings.TrimSpace(string(value)) != ""
}
