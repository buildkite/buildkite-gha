package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// stageRetryGuidance tells the reader how to recover from a failed stage.
// Retrying only the stage step is safe when nothing about the producer
// changed; anything that changed the producer needs a new build.
const stageRetryGuidance = "Retry the whole build to expand this matrix again. If the matrix producer job was retried, only a new build can expand it."

// stageOptions is the `upload --stage-digest <digest> --stage-producer <job>`
// form: the record to compile from and the job whose artifacts hold it.
type stageOptions struct {
	digest   string
	producer string
}

// isStageUpload reports whether upload was invoked in its stage form.
func isStageUpload(args []string) bool {
	return slices.Contains(args, "--stage-digest") || slices.Contains(args, "--stage-producer")
}

func stageArgs(args []string) (stageOptions, error) {
	var options stageOptions
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch option := args[i]; option {
		case "--stage-digest", "--stage-producer":
			if seen[option] {
				return stageOptions{}, fmt.Errorf("%s may only be specified once", option)
			}
			seen[option] = true
			i++
			if i == len(args) {
				return stageOptions{}, fmt.Errorf("%s requires a value", option)
			}
			if option == "--stage-digest" {
				options.digest = args[i]
			} else {
				options.producer = args[i]
			}
		default:
			return stageOptions{}, fmt.Errorf("%q cannot be combined with --stage-digest", option)
		}
	}
	if !stageDigestPattern.MatchString(options.digest) {
		return stageOptions{}, errors.New("--stage-digest requires a sha256 digest")
	}
	if options.producer == "" {
		return stageOptions{}, errors.New("--stage-producer requires the job that uploaded the stage record")
	}
	return options, nil
}

// uploadStage runs a later stage of a workflow's upload: it reads the stage
// record an earlier stage uploaded, expands one needs-derived matrix from its
// producers' results, and uploads the jobs that compile from it.
func uploadStage(options stageOptions, stdout, stderr io.Writer, version, clientVersion string, agent transport.Agent) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return uploadStageContext(ctx, options, stdout, stderr, version, clientVersion, agent)
}

func uploadStageContext(ctx context.Context, options stageOptions, stdout, stderr io.Writer, version, clientVersion string, agent transport.Agent) int {
	if os.Getenv("BUILDKITE") != "true" || os.Getenv("BUILDKITE_BUILD_ID") == "" || os.Getenv("BUILDKITE_JOB_ID") == "" {
		return usageError(stderr, "upload: BUILDKITE=true, BUILDKITE_BUILD_ID, and BUILDKITE_JOB_ID are required")
	}
	retryGuidance := stageRetryGuidance
	fail := func(format string, args ...any) int {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: "+format+"\n", args...)
		_, _ = fmt.Fprintln(stderr, retryGuidance)
		return 1
	}
	root, err := os.MkdirTemp("", "buildkite-gha-stage-")
	if err != nil {
		return fail("create working directory: %v", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	_, _ = fmt.Fprintln(stdout, "~~~ :github: Read stage record")
	recordPath, err := buildkitepipeline.StagePath(options.digest)
	if err != nil {
		return fail("%v", err)
	}
	downloadDir := filepath.Join(root, "stage")
	if err := os.Mkdir(downloadDir, 0o700); err != nil {
		return fail("create stage download directory: %v", err)
	}
	if err := agent.DownloadArtifact(ctx, recordPath, downloadDir, options.producer); err != nil {
		return fail("download stage record from job %q: %v", options.producer, err)
	}
	data, err := readBoundedFile(filepath.Join(downloadDir, filepath.FromSlash(recordPath)), maxStageRecordBytes)
	if err != nil {
		return fail("read stage record: %v", err)
	}
	if actual := sha256Digest(data); actual != options.digest {
		return fail("stage record digest %s does not match expected %s", actual, options.digest)
	}
	record, err := decodeStageRecord(data, version)
	if err != nil {
		return fail("%v", err)
	}
	if record.Continuation.Descriptor.Shape == compiler.RuntimeRunsOnShape {
		retryGuidance = "Retry the whole build to select this runner again. If the producer job was retried, only a new build can select it."
	}
	// The importer writes the first record of a component and each stage
	// writes the next one, so the record comes from the job the step names.
	// The event source and the runtimes come from the importer it records.
	eventPath, err := buildkitepipeline.StageEventPath(record.Event.Digest)
	if err != nil {
		return fail("%v", err)
	}
	if err := agent.DownloadArtifact(ctx, eventPath, downloadDir, record.Importer); err != nil {
		return fail("download event from importer %q: %v", record.Importer, err)
	}
	eventSource, err := readBoundedFile(filepath.Join(downloadDir, filepath.FromSlash(eventPath)), plan.MaxEventPayloadBytes)
	if err != nil {
		return fail("read event: %v", err)
	}
	if actual := sha256Digest(eventSource); actual != record.Event.Digest {
		return fail("event digest %s does not match the stage record's %s", actual, record.Event.Digest)
	}
	checkout := os.Getenv("BUILDKITE_BUILD_CHECKOUT_PATH")
	if checkout == "" {
		checkout, err = os.Getwd()
		if err != nil {
			return fail("resolve checkout: %v", err)
		}
	}
	workflowPath := filepath.Join(checkout, filepath.FromSlash(record.Workflow.Path))
	workflowSource, err := readBoundedFile(workflowPath, maxStageWorkflowBytes)
	if err != nil {
		return fail("read workflow %s: %v", record.Workflow.Path, err)
	}
	if actual := sha256Digest(workflowSource); actual != record.Workflow.Digest {
		return fail("workflow %s in the checkout differs from the workflow the importer compiled", record.Workflow.Path)
	}

	run := stageRun{
		ctx: ctx, stdout: stdout, stderr: stderr, version: version, clientVersion: clientVersion,
		agent: agent, root: root, record: record, digest: options.digest, eventSource: eventSource, workflowPath: workflowPath, workflowSource: workflowSource,
		buildID: os.Getenv("BUILDKITE_BUILD_ID"), jobID: os.Getenv("BUILDKITE_JOB_ID"),
		out: newProcessingOutput(ctx, "upload", "text", stderr, stderr, agent),
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

type stageRun struct {
	ctx                    context.Context
	stdout, stderr         io.Writer
	version, clientVersion string
	agent                  transport.Agent
	root                   string
	record                 stageRecord
	digest                 string
	eventSource            []byte
	workflowPath           string
	workflowSource         []byte
	buildID, jobID         string
	producerOutputs        map[string]string
	out                    processingOutput
}

// execute reads the producer results, expands the matrix, recompiles the
// workflow, and uploads only the jobs this stage adds.
func (r stageRun) execute(fail func(string, ...any) int) int {
	record := r.record
	continuation := record.Continuation
	descriptor := continuation.Descriptor
	graphKeys := make([]string, 0, len(record.Graph))
	graph := make(map[string]compiledJob, len(record.Graph))
	for _, job := range record.Graph {
		graphKeys = append(graphKeys, job.Key)
		graph[job.Key] = job
	}
	rows := make(map[string][]map[string]any)
	skipped := make(map[string]bool)
	runnerOutputs := make(map[string]string)
	// Read every producer before uploading anything. A missing manifest is
	// not a terminal skip, even if another branch has already failed.
	manifests := make(map[string]transport.ResultManifest)
	results := make(map[string]string)
	readProducer := func(key string) (transport.ResultManifest, error) {
		if manifest, exists := manifests[key]; exists {
			return manifest, nil
		}
		manifest, err := transport.DownloadResult(r.ctx, r.agent, r.root, r.buildID, transport.ResultSource{StepKey: key, PlanDigest: graph[key].PlanDigest})
		if err != nil {
			return transport.ResultManifest{}, err
		}
		data, err := transport.MarshalResultManifest(manifest)
		if err != nil {
			return transport.ResultManifest{}, err
		}
		manifests[key], results[key] = manifest, sha256Digest(data)
		return manifest, nil
	}
	for _, resolved := range record.Resolved {
		if _, err := readProducer(resolved.ProducerStepKey); err != nil {
			return fail("earlier matrix %q producer result is unavailable: %v", resolved.Job, err)
		}
		if results[resolved.ProducerStepKey] != resolved.ResultDigest {
			return fail("earlier matrix %q producer result changed after expansion", resolved.Job)
		}
	}
	for _, root := range continuation.Roots() {
		descriptor := root.Descriptor
		_, _ = fmt.Fprintf(r.stdout, "~~~ :github: Read %s from job %q output %q\n", descriptor.Kind(), descriptor.ProducerJob, descriptor.ProducerOutput)
		manifest, err := readProducer(root.ProducerStepKey)
		if err != nil {
			return fail("%s producer %q result is unavailable: %v", descriptor.Kind(), descriptor.ProducerJob, err)
		}
		if manifest.Result != "success" {
			subject := "Matrix"
			if descriptor.Shape == compiler.RuntimeRunsOnShape {
				subject = "Runner selection"
			}
			_, _ = fmt.Fprintf(r.stdout, "%s producer %q finished with result %q; the deferred jobs are skipped for this root.\n", subject, descriptor.ProducerJob, manifest.Result)
			if len(continuation.Joined) == 0 {
				return r.uploadSkipped(fail, manifest.Result, graphKeys)
			}
			skipped[descriptor.Job] = true
			rows[descriptor.Job] = nil
			continue
		}
		if continuation.Scheduling {
			r.producerOutputs = make(map[string]string, len(manifest.Outputs))
			for _, output := range manifest.Outputs {
				r.producerOutputs[output.Name] = output.Value
			}
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
			return fail("%s producer %q did not publish output %q", descriptor.Kind(), descriptor.ProducerJob, descriptor.ProducerOutput)
		}
		if descriptor.Shape == compiler.RuntimeRunsOnShape {
			runnerOutputs[descriptor.Job] = *output
			continue
		}
		expanded, err := compiler.ExpandRuntimeMatrixOutput(descriptor, []byte(*output), graphKeys)
		if err != nil {
			return fail("matrix from job %q output %q is invalid: %v", descriptor.ProducerJob, descriptor.ProducerOutput, err)
		}
		rows[descriptor.Job] = expanded
		_, _ = fmt.Fprintf(r.stdout, "Expanding job %q into %d matrix instances.\n", descriptor.Job, len(expanded))
	}
	wanted := continuation.DependentInstances() + len(runnerOutputs)
	for job, expanded := range rows {
		wanted += len(expanded)
		if skipped[job] {
			wanted++
		}
	}
	if wanted > continuation.JobBudget {
		return fail("job %q produced %d matrix rows, but step %q may upload at most %d jobs (%d rows plus their dependents): the workflow's deferred uploads share the %d-job graph bound. Reduce the matrix or the number of deferred matrices in the workflow.",
			descriptor.ProducerJob, wanted-continuation.DependentInstances(), continuation.StepKey, continuation.JobBudget, continuation.JobBudget-continuation.DependentInstances(), compiler.MaxRuntimeMatrixGraphJobs)
	}

	_, _ = fmt.Fprintln(r.stdout, "~~~ :github: Compile deferred jobs")
	preflight, report, err := r.compile(rows, skipped, runnerOutputs)
	if err != nil {
		_ = r.out.write(r.ctx, report)
		return fail("compile deferred jobs: %v", err)
	}
	bundle := preflight.Bundle
	// advance proves the recompilation reproduced the earlier stages and
	// returns the jobs this stage uploads, with the step and record of the
	// next stage when some of them feed a later matrix.
	stage, err := record.advance(bundle, rows, skipped, results, r.digest)
	if err != nil {
		return fail("%v", err)
	}
	if len(stage.jobs) == 0 {
		return fail("recompilation produced no deferred jobs")
	}
	pipeline, createdGates, err := r.deferredPipeline(bundle, stage, nil)
	if err != nil {
		return fail("%v", err)
	}
	writeCompilerWarnings(r.stderr, "upload", record.Workflow.Path, bundle.IR.Warnings)
	_ = compatibility.WriteProcessing(r.stderr, "text", report)

	_, _ = fmt.Fprintf(r.stdout, "~~~ :github: Upload %d deferred jobs\n", len(stage.jobs))
	artifacts := make([]transport.Artifact, 0, len(stage.plans)+len(stage.records)+1)
	if bundle.EventArtifact != nil {
		artifacts = append(artifacts, *bundle.EventArtifact)
	}
	artifacts = append(artifacts, stage.records...)
	expected := make(map[string]string, len(stage.jobs)+len(stage.steps))
	for _, jobPlan := range stage.plans {
		artifacts = append(artifacts, transport.Artifact{Path: jobPlan.Path, Digest: jobPlan.Digest, Contents: jobPlan.Contents})
		expected[jobPlan.Job.Target.StepKey] = jobPlan.Digest
	}
	for _, job := range stage.jobs {
		if job.SkipDigest != "" {
			expected[job.Key] = job.SkipDigest
		}
	}
	for _, step := range stage.steps {
		expected[step.Key] = step.Stage.ArtifactDigest
	}
	if err := transport.UploadArtifacts(r.ctx, r.agent, r.root, artifacts, pipeline); err != nil {
		if r.ctx.Err() != nil || !errors.Is(err, transport.ErrPipelineUpload) {
			return fail("%v", err)
		}
		if r.alreadyApplied(expected, "command") && r.schedulingAlreadyApplied(stage.jobs) {
			_, _ = fmt.Fprintf(r.stdout, "The %d deferred jobs were already uploaded by an earlier run of this step; nothing to do.\n", len(stage.jobs))
			return 0
		}
		raced := r.existingSteps(createdGates)
		if len(raced) == 0 {
			return fail("%v", err)
		}
		if err := r.uploadAfterGateRace(bundle, stage, raced); err != nil {
			return fail("%v", err)
		}
	}
	_, _ = fmt.Fprintf(r.stdout, "Uploaded %d jobs for %q from job %q output %q.\n", len(stage.jobs), descriptor.Job, descriptor.ProducerJob, descriptor.ProducerOutput)
	for i, step := range stage.steps {
		_, _ = fmt.Fprintf(r.stdout, "Step %q expands %s once these jobs have run.\n", step.Key, quotedKeys(stage.next[i].Jobs))
	}
	return 0
}

// compile recompiles the workflow with the supplied scheduling values through the
// importer's compile path: the recorded inputs become one hosted compile
// request, which is validated with the live runner resolution and compiled
// with the environment source this job observes.
func (r stageRun) compile(rows map[string][]map[string]any, skipped map[string]bool, runnerOutputs map[string]string) (hostedCompilation, compatibility.ProcessingReport, error) {
	record := r.record
	repositorySource, cleanupSource, err := hostedRepositorySource(r.ctx, r.clientVersion, r.eventSource, importerJobActionSourceAuthentication(r.stderr, r.clientVersion), record.PrivateReusableWorkflows)
	if err != nil {
		return hostedCompilation{}, compatibility.EnvironmentProcessingReport(record.Workflow.Path, hostedProfile, "repository source could not be configured"), err
	}
	defer cleanupSource()
	request := record.compileRequest(r.workflowPath, r.workflowSource, r.eventSource, rows, skipped, runnerOutputs, repositorySource)
	if record.Continuation.Scheduling {
		request.RuntimeSchedulingOutputs = map[string]map[string]string{record.Continuation.Descriptor.Job: r.producerOutputs}
		request.RuntimeSchedulingBuildID = r.buildID
	}
	request.EnvironmentSource = environmentSourceFromAgent(r.clientVersion)
	_, reports, err := validateHostedRequests(r.ctx, r.out, []*hostedCompileRequest{&request}, r.clientVersion)
	if err != nil {
		return hostedCompilation{}, compatibility.ProcessingReport{}, err
	}
	compiled, err := compileHostedRequest(r.ctx, request)
	applyHostedCompilation(&reports[0], record.Workflow.Path, compiled, err)
	return compiled, reports[0], err
}

// deferredPipeline emits the pipeline for the jobs this stage adds, with the
// steps that expand the next stages. Jobs the earlier stages created and
// approval gates that already exist in the build, or that the caller knows
// another stage created (sharedGates), are referenced as existing steps
// instead of being uploaded again. It returns the keys of the gates the
// pipeline creates.
func (r stageRun) deferredPipeline(bundle compiler.Bundle, stage stageResult, sharedGates map[string]bool) ([]byte, []string, error) {
	record := r.record
	existing := make([]string, 0, len(record.Graph)+len(record.ApprovalGates))
	for _, job := range record.Graph {
		existing = append(existing, job.Key)
	}
	usedGates := make(map[string]bool)
	for _, job := range stage.jobs {
		if job.ApprovalGate != "" {
			usedGates[job.ApprovalGate] = true
		}
	}
	var gates []buildkitepipeline.ApprovalGate
	var created []string
	for _, gate := range bundle.GeneratedWorkflow.ApprovalGates {
		if !usedGates[gate.Key] {
			continue
		}
		// A gate an earlier stage created, or that another stage of this
		// build already uploaded, must be referenced rather than repeated.
		if slices.Contains(record.ApprovalGates, gate.Key) || sharedGates[gate.Key] || r.stepExists(gate.Key) {
			existing = append(existing, gate.Key)
			continue
		}
		gates = append(gates, gate)
		created = append(created, gate.Key)
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		Deferred:             true,
		ArtifactProducer:     r.jobID,
		DistributionProducer: record.Importer,
		ExistingSteps:        existing,
		EventProvider:        record.Event.Provider,
		DisableRunnerUser:    !record.RunnerUser,
		Workflows: []buildkitepipeline.Workflow{{
			GroupLabel:    record.Workflow.GroupLabel,
			Ungrouped:     record.Workflow.Ungrouped,
			CheckName:     record.Workflow.CheckName,
			Event:         record.Event.Name,
			ApprovalGates: gates,
			Jobs:          append(slices.Clone(stage.jobs), stage.steps...),
		}},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("emit deferred jobs: %w", err)
	}
	return pipeline, created, nil
}

// uploadAfterGateRace handles two stages that share an approval gate neither
// found in the build: both include it, and Buildkite rejects the second upload
// for the duplicate key. The caller passes the gates this run tried to create
// that now exist, so the pipeline is emitted again referencing them, still
// creating any gate only this stage uses, and uploaded once more. The
// artifacts of the first attempt are already in place, so only the pipeline
// is repeated.
func (r stageRun) uploadAfterGateRace(bundle compiler.Bundle, stage stageResult, raced []string) error {
	shared := make(map[string]bool, len(raced))
	for _, key := range raced {
		shared[key] = true
	}
	_, _ = fmt.Fprintf(r.stdout, "Another deferred upload created approval gates %s first; uploading again with them as existing steps.\n", quotedKeys(raced))
	pipeline, _, err := r.deferredPipeline(bundle, stage, shared)
	if err != nil {
		return err
	}
	if err := r.agent.UploadPipeline(r.ctx, pipeline); err != nil {
		return fmt.Errorf("%w: %w", transport.ErrPipelineUpload, err)
	}
	return nil
}

// existingSteps returns the keys that already name a step in the build.
func (r stageRun) existingSteps(keys []string) []string {
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
func (r stageRun) uploadSkipped(fail func(string, ...any) int, result string, graphKeys []string) int {
	record := r.record
	producer := record.Continuation.Descriptor.ProducerJob
	reason := fmt.Sprintf("%s producer job %q finished with result %s", record.Continuation.Descriptor.Kind(), producer, result)
	jobs := make([]buildkitepipeline.Job, 0, len(record.Continuation.Jobs))
	expected := make(map[string]string, len(record.Continuation.Jobs))
	// The consumer's instances are unknown, so it gets one placeholder under
	// its logical key, as does every later matrix; each dependent promised one
	// check per static instance.
	for _, job := range record.Continuation.Jobs {
		for _, instance := range record.placeholders(job) {
			jobs = append(jobs, buildkitepipeline.Job{
				Key: instance.Key, Label: instance.Label, CheckLabel: instance.CheckLabel, SkipReason: reason,
				Dependencies: []string{record.Continuation.ProducerStepKey},
			})
			expected[instance.Key] = ":github: job · " + instance.Label
		}
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		Deferred:         true,
		ArtifactProducer: r.jobID,
		ExistingSteps:    graphKeys,
		EventProvider:    record.Event.Provider,
		Workflows: []buildkitepipeline.Workflow{{
			GroupLabel: record.Workflow.GroupLabel,
			Ungrouped:  record.Workflow.Ungrouped,
			CheckName:  record.Workflow.CheckName,
			Event:      record.Event.Name,
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
	_, _ = fmt.Fprintf(r.stdout, "Uploaded %d skipped jobs for %q.\n", len(jobs), record.Continuation.Descriptor.Job)
	return 0
}

// schedulingAlreadyApplied checks scheduler attributes independently of the
// command's plan digest: concurrency groups are pipeline fields, not plan data.
func (r stageRun) schedulingAlreadyApplied(jobs []buildkitepipeline.Job) bool {
	if !r.record.Continuation.Scheduling {
		return true
	}
	for _, job := range jobs {
		if job.Concurrency == 0 {
			continue
		}
		// The step API names these differently from pipeline YAML.
		for attribute, want := range map[string]string{"concurrency_key": job.ConcurrencyGroup, "concurrency_limit": fmt.Sprint(job.Concurrency)} {
			value, err := r.agent.GetStepAttribute(r.ctx, job.Key, attribute)
			if err != nil || strings.TrimSpace(string(value)) != want {
				return false
			}
		}
	}
	return true
}

// alreadyApplied decides whether a rejected upload was a replay of this
// stage's own earlier upload. Buildkite rejects a whole upload when any step
// key already exists in the build, and the deferred keys are only ever
// uploaded by this stage, so if every expected step exists with the expected
// attribute the earlier upload succeeded and this run has nothing left to do.
// Any missing step or differing attribute means the rejection had another
// cause, and the run fails so the build can be retried.
func (r stageRun) alreadyApplied(expected map[string]string, attribute string) bool {
	if len(expected) == 0 {
		return false
	}
	for key, want := range expected {
		value, err := r.agent.GetStepAttribute(r.ctx, key, attribute)
		if err != nil {
			return false
		}
		if attribute == "command" {
			if !buildkitepipeline.BootstrapHasDigest(string(value), want) {
				return false
			}
		} else if !strings.Contains(string(value), want) {
			return false
		}
	}
	return true
}

// stepExists reports whether a step with the key is already in the build.
func (r stageRun) stepExists(key string) bool {
	value, err := r.agent.GetStepAttribute(r.ctx, key, "key")
	return err == nil && strings.TrimSpace(string(value)) != ""
}
