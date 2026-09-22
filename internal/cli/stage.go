package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/git"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// stageSchema identifies the stage record format. A stage step rejects any
// other schema or compiler version so that a deferred compile can never run
// with inputs a different compiler produced.
const stageSchema = "buildkite-gha/stage/v1"

// maxStageRecordBytes bounds the record a stage step reads back. The record
// carries the job graph, runner mappings, and variables but not the event,
// which every stage of an upload shares by digest, so the bound is
// independent of the event size. The writer enforces the same bound, so an
// upload never records a stage its own step would refuse to read.
const maxStageRecordBytes = 4 * 1024 * 1024

// maxStageWorkflowBytes bounds the workflow file a stage step reads from the
// checkout before comparing its digest with the recorded one.
const maxStageWorkflowBytes = 4 * 1024 * 1024

var stageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// stageRecord is the state of one workflow's compilation inside a build.
//
// upload compiles a workflow in stages. The importer runs the first stage
// from live inputs; its record has no graph, no resolved rows, and no
// continuation. Each stage compiles the whole workflow through the shared
// hosted compile request, uploads the jobs whose matrix rows are known, and
// for every matrix the compiler leaves deferred writes the next stage's
// record: the same recorded inputs, the rows resolved so far, the graph as
// the build now holds it, and the continuation the next stage expands. The
// stage step that reads a record compiles that stage the same way, so the
// producer job output is the only input the importer did not fix.
type stageRecord struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	// Distribution is the compiler distribution digest recorded in job plans.
	Distribution string `json:"distribution_digest"`
	// Runtimes maps each platform to the runtime distribution digest the
	// importer uploaded. A deferred job on a platform without one fails.
	Runtimes map[string]string `json:"runtime_digests"`
	// Importer is the job whose artifacts hold the distributions and the
	// event source.
	Importer string        `json:"importer"`
	Workflow stageWorkflow `json:"workflow"`
	Event    stageEvent    `json:"event"`
	// Runners are the explicit runs-on mappings the importer was configured
	// with. Every stage applies the same policy, so a producer cannot route a
	// job to a queue the importer would not have used.
	Runners    []stageRunner           `json:"runners,omitempty"`
	Vars       stageVars               `json:"vars"`
	OIDC       *plan.OIDCConfiguration `json:"oidc,omitempty"`
	RunnerUser bool                    `json:"experimental_runner_user"`
	// PrivateReusableWorkflows records the plugin setting that lets the
	// importer read remote reusable workflows through the agent's Git
	// credentials. Every stage reads them the same way.
	PrivateReusableWorkflows bool `json:"private_reusable_workflows,omitempty"`
	// Continuation is the deferred remainder this stage expands: its roots,
	// their dependents, and the step that performs the upload. Its action
	// locks are the component's from the importer's compilation, so every
	// stage pins the same revisions. The importer's own record has none.
	Continuation compiler.RuntimeContinuation `json:"continuation"`
	// Others are the workflow's remaining continuations, each expanded by its
	// own stage step. A recompilation must leave exactly these deferred.
	Others []compiler.RuntimeContinuation `json:"other_continuations,omitempty"`
	// Resolved records, for every root an earlier stage expanded, the rows its
	// verified producer published or that the producer did not succeed. The
	// recompilation supplies them again so the jobs the earlier stages
	// uploaded come back unchanged.
	Resolved []resolvedMatrix `json:"resolved_matrices,omitempty"`
	// Graph records every job the earlier stages uploaded, so a stage can
	// prove that its recompilation reproduced them before uploading. Each
	// root's producer is the graph entry under its producer step key.
	Graph []compiledJob `json:"graph"`
	// ApprovalGates are the gates the earlier stages created, which a later
	// stage references instead of creating again.
	ApprovalGates []string `json:"approval_gates,omitempty"`
}

// resolvedMatrix is one root an earlier stage expanded: the rows its producer
// published, or Skipped when the producer did not succeed and the stage
// uploaded the root's closure as skipped. The producer's plan is in Graph;
// ResultDigest binds its verified result, including the job attempt, so later
// stages cannot combine recorded rows with a changed producer.
type resolvedMatrix struct {
	Job             string           `json:"job"`
	Rows            []map[string]any `json:"rows,omitempty"`
	Skipped         bool             `json:"skipped,omitempty"`
	ProducerStepKey string           `json:"producer_step_key"`
	ResultDigest    string           `json:"result_digest"`
}

type stageWorkflow struct {
	// Path is the repository-relative workflow path a stage step reads from
	// its checkout; Digest is the SHA-256 of the bytes the importer used.
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Namespace  string `json:"step_key_namespace"`
	GroupLabel string `json:"group_label"`
	CheckName  string `json:"check_name"`
	Ungrouped  bool   `json:"ungrouped,omitempty"`
}

type stageEvent struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Digest names the event source artifact the importer uploaded once for
	// every stage; see buildkitepipeline.StageEventPath.
	Digest string `json:"source_digest"`
	// File records whether generated jobs expose the payload through
	// GITHUB_EVENT_PATH, which is part of every plan digest.
	File bool `json:"file"`
}

type stageRunner struct {
	Label    string                         `json:"label"`
	Queue    string                         `json:"queue,omitempty"`
	Platform string                         `json:"platform"`
	Image    string                         `json:"image,omitempty"`
	Cache    *buildkitepipeline.CacheVolume `json:"cache,omitempty"`
}

type stageVars struct {
	Organization map[string]string `json:"organization,omitempty"`
	Repository   map[string]string `json:"repository,omitempty"`
	Resolved     bool              `json:"resolved"`
}

// stageResult is what one stage adds to the build.
type stageResult struct {
	// jobs are the generated jobs this stage uploads, in bundle order,
	// followed by the placeholders of the jobs it skips.
	jobs  []buildkitepipeline.Job
	plans []compiler.PlanArtifact
	// next are the continuations the compilation left deferred for later
	// stages, with the steps that expand them and the records those steps
	// read.
	next    []compiler.RuntimeContinuation
	steps   []buildkitepipeline.Job
	records []transport.Artifact
}

// checkoutWorkflowPath reports whether a recorded workflow path is a cleaned,
// slash-separated path inside the build checkout, which is where a stage step
// reads the workflow again. A filename may contain "..", but no segment may
// be "..", and the path may not be absolute.
func checkoutWorkflowPath(workflowPath string) bool {
	return workflowPath != "" && path.Clean(workflowPath) == workflowPath && filepath.IsLocal(filepath.FromSlash(workflowPath))
}

// requireCheckoutWorkflow proves a stage step can reopen the workflow from a
// fresh checkout of the build commit: the recorded path lies inside the
// repository, git tracks the file at that path, and the file committed at the
// build commit is byte-for-byte the content the importer compiled. A custom
// importer may pass one workflow from outside the repository, an untracked
// file, or a file with edits in the working tree or the index; each would fail
// only after the producer has run, so the initial upload fails now. The build
// commit is BUILDKITE_COMMIT when the agent resolved it, otherwise HEAD.
func requireCheckoutWorkflow(input workflowInput, buildCommit string) error {
	const because = "so its matrices cannot be expanded from a job output: the deferred upload reads the workflow from the checkout"
	if !checkoutWorkflowPath(input.CanonicalPath) {
		return fmt.Errorf("workflow %q is outside the repository checkout, %s", input.CanonicalPath, because)
	}
	rootBytes, err := exec.Command("git", "-C", filepath.Dir(input.Path), "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("workflow %q is not inside a git repository, %s", input.CanonicalPath, because)
	}
	root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
	if relative, err := filepath.Rel(root, input.Path); err != nil || filepath.ToSlash(relative) != input.CanonicalPath {
		return fmt.Errorf("workflow %q resolves to %q in its git repository, %s", input.CanonicalPath, filepath.ToSlash(relative), because)
	}
	listed, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", ":(top,literal)"+input.CanonicalPath).Output()
	if err != nil {
		return fmt.Errorf("inspect workflow path %q in git index: %w", input.CanonicalPath, err)
	}
	if !bytes.Equal(listed, append([]byte(input.CanonicalPath), 0)) {
		return fmt.Errorf("workflow %q is not tracked by git, %s", input.CanonicalPath, because)
	}
	if !git.ValidObjectID(buildCommit) {
		buildCommit = "HEAD"
	}
	committed, err := exec.Command("git", "-C", root, "show", buildCommit+":"+input.CanonicalPath).Output()
	if err != nil {
		return fmt.Errorf("workflow %q is not committed at %s, %s", input.CanonicalPath, buildCommit, because)
	}
	if !bytes.Equal(committed, input.Source) {
		return fmt.Errorf("workflow %q differs from the file committed at %s, %s", input.CanonicalPath, buildCommit, because)
	}
	return nil
}

// importerStage is the importer's stage of one workflow: the request it
// admitted, with the variables its final compile used, recorded so that the
// later stages rebuild the same request. It returns the record and the event
// source artifact every stage of the workflow reads. The workflow must be
// reopenable from the checkout, because that is where the later stages read
// it.
func importerStage(request hostedCompileRequest, importer, buildCommit string, input workflowInput, groupLabel, checkName string, event effectiveEventSelection, runnerUser, privateReusable bool) (stageRecord, transport.Artifact, error) {
	if err := requireCheckoutWorkflow(input, buildCommit); err != nil {
		return stageRecord{}, transport.Artifact{}, err
	}
	if len(event.Source) > plan.MaxEventPayloadBytes {
		return stageRecord{}, transport.Artifact{}, fmt.Errorf("event source is %d bytes, maximum is %d", len(event.Source), plan.MaxEventPayloadBytes)
	}
	eventDigest := sha256Digest(event.Source)
	eventPath, err := buildkitepipeline.StageEventPath(eventDigest)
	if err != nil {
		return stageRecord{}, transport.Artifact{}, err
	}
	runtimes := make(map[string]string, len(request.RuntimeDistributions))
	for platform, digest := range request.RuntimeDistributions {
		runtimes[platform.String()] = digest
	}
	runners := make([]stageRunner, 0, len(request.RunnerTargets))
	for _, label := range slices.Sorted(maps.Keys(request.RunnerTargets)) {
		target := request.RunnerTargets[label]
		runners = append(runners, stageRunner{Label: label, Queue: target.Queue, Platform: target.Platform.String(), Image: target.Image, Cache: target.Cache})
	}
	record := stageRecord{
		Schema:       stageSchema,
		Version:      request.Version,
		Distribution: request.DistributionDigest,
		Runtimes:     runtimes,
		Importer:     importer,
		Workflow: stageWorkflow{
			Path:       input.CanonicalPath,
			Digest:     sha256Digest(request.WorkflowSource),
			Namespace:  request.StepKeyNamespace,
			GroupLabel: groupLabel,
			CheckName:  checkName,
		},
		Event:                    stageEvent{Name: event.Event.Event, Provider: event.Event.Provider, Digest: eventDigest, File: request.EventFile},
		Runners:                  runners,
		Vars:                     stageVars{Organization: request.Vars.Organization, Repository: request.Vars.Repository, Resolved: request.Vars.Resolved},
		OIDC:                     request.OIDC,
		RunnerUser:               runnerUser,
		PrivateReusableWorkflows: privateReusable,
	}
	return record, transport.Artifact{Path: eventPath, Digest: eventDigest, Contents: event.Source}, nil
}

// importer reports whether the record is the importer's stage, which has no
// continuation to expand: every job of the bundle is its own, and every
// continuation the compiler leaves deferred starts a component.
func (s stageRecord) importer() bool {
	return s.Continuation.StepKey == ""
}

// producer returns the graph entry of the job whose verified result supplies
// the rows of the root job.
func (s stageRecord) producer(job string) compiledJob {
	for _, root := range s.Continuation.Roots() {
		if root.Descriptor.Job != job {
			continue
		}
		for _, entry := range s.Graph {
			if entry.Key == root.ProducerStepKey {
				return entry
			}
		}
	}
	return compiledJob{}
}

// advance checks a compilation of the stage's workflow against what the
// earlier stages uploaded and returns what this stage adds to the build.
//
// The jobs the earlier stages created must come back with the same keys,
// dependencies, and plan digests. Every other job must belong to this stage,
// come from the workflow source the importer resolved, and use only the
// action revisions the importer resolved; those are the jobs this stage
// uploads. The continuations the earlier stages recorded must still be
// deferred exactly as recorded, because their own steps expand them. Any
// continuation left over is a later stage of this one: for the importer,
// every continuation of the bundle; for a later stage, the one continuation
// rooted at the later matrices its parent promised, which gets the budget
// this stage leaves. Anything else means the inputs moved between compiles,
// and a pipeline with mismatched plans would be wrong to upload.
//
// rows and skipped are the matrices this stage resolved, recorded with their
// producer result digests for the next stage; digest addresses this record in
// skipped placeholders.
func (s stageRecord) advance(bundle compiler.Bundle, rows map[string][]map[string]any, skipped map[string]bool, results map[string]string, digest string) (stageResult, error) {
	recorded := make(map[string]compiledJob, len(s.Graph))
	for _, job := range s.Graph {
		recorded[job.Key] = job
	}
	owned := make(map[string]bool, len(s.Continuation.Jobs))
	for _, job := range s.Continuation.Jobs {
		owned[job] = true
	}
	recordedLocks := make(map[string]plan.ActionLock, len(s.Continuation.ActionLocks))
	for _, lock := range s.Continuation.ActionLocks {
		recordedLocks[lock.ID] = lock
	}
	generated := make(map[string]buildkitepipeline.Job, len(bundle.GeneratedWorkflow.Jobs))
	for _, job := range bundle.GeneratedWorkflow.Jobs {
		generated[job.Key] = job
	}
	plans := make(map[string]compiler.PlanArtifact, len(bundle.Plans))
	for _, jobPlan := range bundle.Plans {
		plans[jobPlan.Job.Target.StepKey] = jobPlan
	}
	var result stageResult
	compiled := make(map[string]bool, len(s.Continuation.Jobs))
	reproduced := 0
	// compiledJobs lists the instances in bundle.IR.Jobs order.
	for i, current := range compiledJobs(bundle) {
		if current.PlanDigest == "" {
			return stageResult{}, fmt.Errorf("job %q has no plan after recompilation", current.Key)
		}
		before, exists := recorded[current.Key]
		if exists && !owned[current.LogicalJob] {
			if !before.equal(current) {
				return stageResult{}, fmt.Errorf("job %q compiled differently from the initial upload; the workflow inputs changed", current.Key)
			}
			reproduced++
			continue
		}
		instance, jobPlan := bundle.IR.Jobs[i], plans[current.Key]
		if !s.importer() {
			if !owned[current.LogicalJob] {
				return stageResult{}, fmt.Errorf("job %q did not exist in the initial upload; the workflow inputs changed", current.Key)
			}
			if exists {
				return stageResult{}, fmt.Errorf("deferred job %q collides with a job the initial upload created", current.Key)
			}
			if _, uploaded := s.Runtimes[instance.Platform.String()]; !uploaded {
				return stageResult{}, fmt.Errorf("deferred job %q runs on %s, but the importer uploaded no runtime for that platform; configure --runtime-distribution for it", current.Key, instance.Platform)
			}
			// A deferred job has no earlier plan to compare, so its workflow
			// source must be the one the importer compiled: the same file
			// bytes and, for a remote reusable workflow, the same commit.
			if source, exists := s.Continuation.Sources[current.LogicalJob]; !exists || !source.Matches(instance) {
				return stageResult{}, fmt.Errorf("deferred job %q comes from a different workflow source than the initial upload compiled; the workflow inputs changed", current.Key)
			}
			// Its actions must be the revisions the importer resolved: the
			// same commit and source tree for a remote action, the same tree
			// for one in the checkout.
			for _, lock := range jobPlan.Job.Actions {
				if before, exists := recordedLocks[lock.ID]; !exists || !reflect.DeepEqual(before, lock) {
					return stageResult{}, fmt.Errorf("deferred job %q uses action %s, which the initial upload did not resolve to that revision; the workflow inputs changed", current.Key, describeActionLock(lock))
				}
			}
		}
		job, exists := generated[current.Key]
		if !exists {
			return stageResult{}, fmt.Errorf("job %q has no generated step", current.Key)
		}
		compiled[current.LogicalJob] = true
		result.jobs = append(result.jobs, job)
		result.plans = append(result.plans, jobPlan)
	}
	if reproduced != len(recorded) {
		return stageResult{}, fmt.Errorf("recompilation reproduced %d of %d jobs from the initial upload; the workflow inputs changed", reproduced, len(recorded))
	}
	others := make(map[string]compiler.RuntimeContinuation, len(s.Others))
	for _, continuation := range s.Others {
		others[continuation.StepKey] = continuation
	}
	for _, continuation := range bundle.IR.Continuations {
		if before, exists := others[continuation.StepKey]; exists {
			if !reflect.DeepEqual(before, continuation) {
				return stageResult{}, fmt.Errorf("deferred step %q compiled differently from the initial upload; the workflow inputs changed", continuation.StepKey)
			}
			delete(others, continuation.StepKey)
			continue
		}
		if !s.importer() {
			if !slices.Contains(s.Continuation.LaterMatrices, continuation.Descriptor.Job) {
				return stageResult{}, fmt.Errorf("recompilation defers job %q through step %q, which the initial upload did not create; the workflow differs from the initial upload", continuation.Descriptor.Job, continuation.StepKey)
			}
			if len(result.next) != 0 {
				return stageResult{}, fmt.Errorf("recompilation defers the later matrices of step %q through two steps %q and %q; the workflow inputs changed", s.Continuation.StepKey, result.next[0].StepKey, continuation.StepKey)
			}
			if err := checkNextStage(s.Continuation, continuation); err != nil {
				return stageResult{}, err
			}
			continuation.ActionLocks = s.Continuation.ActionLocks
		}
		result.next = append(result.next, continuation)
	}
	if len(others) != 0 {
		missing := slices.Sorted(maps.Keys(others))
		return stageResult{}, fmt.Errorf("recompilation no longer defers job %q through step %q; the workflow differs from the initial upload", others[missing[0]].Descriptor.Job, missing[0])
	}
	if !s.importer() {
		// Every job this stage owns is now compiled, skipped because a
		// producer did not succeed, or handed to the next stage.
		for _, job := range s.Continuation.Jobs {
			if compiled[job] || bundle.IR.RuntimeMatrixSkippedJobs[job] || len(result.next) != 0 && slices.Contains(result.next[0].Jobs, job) {
				continue
			}
			return stageResult{}, fmt.Errorf("recompilation neither compiled nor deferred job %q; the workflow inputs changed", job)
		}
		result.jobs = append(result.jobs, s.skippedJobs(bundle.IR.RuntimeMatrixSkippedJobs, digest)...)
		if len(result.jobs) > s.Continuation.JobBudget {
			return stageResult{}, fmt.Errorf("step %q compiled %d deferred jobs, but may upload at most %d: the workflow's deferred uploads share the %d-job graph bound", s.Continuation.StepKey, len(result.jobs), s.Continuation.JobBudget, compiler.MaxRuntimeMatrixGraphJobs)
		}
	}
	// The next stage references the gates that exist once this upload is in
	// place: the earlier stages', and every gate the importer emits or a
	// later stage's jobs use.
	gates := slices.Clone(s.ApprovalGates)
	for _, gate := range bundle.GeneratedWorkflow.ApprovalGates {
		if s.importer() && !slices.Contains(gates, gate.Key) {
			gates = append(gates, gate.Key)
		}
	}
	for _, job := range result.jobs {
		if job.ApprovalGate != "" && !slices.Contains(gates, job.ApprovalGate) {
			gates = append(gates, job.ApprovalGate)
		}
	}
	resolved := slices.Clone(s.Resolved)
	if !s.importer() {
		for _, root := range s.Continuation.Roots() {
			job := root.Descriptor.Job
			resolved = append(resolved, resolvedMatrix{
				Job: job, Rows: rows[job], Skipped: skipped[job],
				ProducerStepKey: root.ProducerStepKey, ResultDigest: results[root.ProducerStepKey],
			})
		}
	}
	for _, next := range result.next {
		child := s
		child.Continuation = next
		if !s.importer() {
			// The row check before compiling counted the next stage's roots
			// and dependents, so what this stage leaves covers them.
			child.Continuation.JobBudget = s.Continuation.JobBudget - len(result.jobs)
		}
		child.Others = slices.DeleteFunc(slices.Clone(bundle.IR.Continuations), func(other compiler.RuntimeContinuation) bool {
			return other.StepKey == next.StepKey
		})
		child.Resolved, child.ApprovalGates = resolved, gates
		record, step, err := child.write(bundle)
		if err != nil {
			return stageResult{}, err
		}
		result.records = append(result.records, record)
		result.steps = append(result.steps, step)
	}
	return result, nil
}

// checkNextStage requires the continuation a recompilation left deferred to
// be the next stage the parent promised: rooted at the parent's later
// matrices and made of the parent's jobs, from the workflow sources the
// importer resolved. The next stage compares the jobs it compiles against the
// sources and action locks it inherits from the parent, so a source that
// moved between stages is caught here and an action that moved is caught
// there.
func checkNextStage(parent, next compiler.RuntimeContinuation) error {
	if err := validateContinuationShape(next); err != nil {
		return fmt.Errorf("deferred step %q for the next stage: %w", next.StepKey, err)
	}
	for _, root := range next.Roots() {
		if !slices.Contains(parent.LaterMatrices, root.Descriptor.Job) {
			return fmt.Errorf("deferred step %q expands job %q, which the initial upload did not defer to a later stage; the workflow inputs changed", next.StepKey, root.Descriptor.Job)
		}
	}
	for _, job := range next.Jobs {
		if !slices.Contains(parent.Jobs, job) {
			return fmt.Errorf("deferred step %q defers job %q, which step %q does not own; the workflow inputs changed", next.StepKey, job, parent.StepKey)
		}
		if !reflect.DeepEqual(parent.Sources[job], next.Sources[job]) || !reflect.DeepEqual(parent.Instances[job], next.Instances[job]) {
			return fmt.Errorf("deferred job %q compiled differently from the initial upload; the workflow inputs changed", job)
		}
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

// placeholders returns the steps a deferred job gets when it is skipped: one
// per static instance of a dependent, or one under the logical key of a root
// or a later matrix, whose instances are unknown.
func (s stageRecord) placeholders(job string) []compiler.RuntimeMatrixInstance {
	if instances := s.Continuation.Instances[job]; len(instances) != 0 {
		return instances
	}
	return []compiler.RuntimeMatrixInstance{{Key: compiler.LogicalJobStepKey(s.Workflow.Namespace, job), Label: s.Continuation.JobLabel(job), CheckLabel: job}}
}

// skippedJobs renders only the failed roots' closures, preserving the other
// branches in the component and emitting a shared join once.
func (s stageRecord) skippedJobs(skipped map[string]bool, digest string) []buildkitepipeline.Job {
	var jobs []buildkitepipeline.Job
	for _, job := range s.Continuation.Jobs {
		if !skipped[job] {
			continue
		}
		for _, instance := range s.placeholders(job) {
			jobs = append(jobs, buildkitepipeline.Job{Key: instance.Key, Label: instance.Label, CheckLabel: instance.CheckLabel,
				SkipReason: "matrix producer did not succeed", SkipDigest: digest})
		}
	}
	return jobs
}

// write records the bundle's jobs as the graph the next stage must reproduce
// and returns the content-addressed record with the stage step that reads
// it. The step runs where the first root's producer ran and depends on every
// root's producer, so the results it reads are the ones this build's
// producers publish.
func (s stageRecord) write(bundle compiler.Bundle) (transport.Artifact, buildkitepipeline.Job, error) {
	continuation := s.Continuation
	s.Graph = compiledJobs(bundle)
	plans := make(map[string]string, len(s.Graph))
	for _, job := range s.Graph {
		if job.PlanDigest == "" {
			return transport.Artifact{}, buildkitepipeline.Job{}, fmt.Errorf("job %q has no plan to record for the next stage", job.Key)
		}
		plans[job.Key] = job.PlanDigest
	}
	var dependencies []string
	for _, root := range continuation.Roots() {
		// A one-row static matrix gives the producer a digest-suffixed key, so
		// the compiler records the producer's actual instance key.
		if _, ok := plans[root.ProducerStepKey]; !ok {
			return transport.Artifact{}, buildkitepipeline.Job{}, fmt.Errorf("matrix producer %q of job %q has no plan", root.Descriptor.ProducerJob, root.Descriptor.Job)
		}
		if !slices.Contains(dependencies, root.ProducerStepKey) {
			dependencies = append(dependencies, root.ProducerStepKey)
		}
	}
	if continuation.Scheduling {
		// Ordered queues can add edges to jobs uploaded earlier. Finish all
		// static jobs, including external needs of downstream owned jobs,
		// before admitting output-derived groups. This step holds no slot.
		dependencies = make([]string, 0, len(s.Graph))
		for _, job := range s.Graph {
			dependencies = append(dependencies, job.Key)
		}
	}
	var producer *buildkitepipeline.Job
	for i, job := range bundle.GeneratedWorkflow.Jobs {
		if job.Key == continuation.ProducerStepKey {
			producer = &bundle.GeneratedWorkflow.Jobs[i]
		}
	}
	if producer == nil {
		return transport.Artifact{}, buildkitepipeline.Job{}, fmt.Errorf("matrix producer %q of job %q has no generated step", continuation.Descriptor.ProducerJob, continuation.Descriptor.Job)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return transport.Artifact{}, buildkitepipeline.Job{}, fmt.Errorf("encode stage for job %q: %w", continuation.Descriptor.Job, err)
	}
	if len(encoded) > maxStageRecordBytes {
		return transport.Artifact{}, buildkitepipeline.Job{}, fmt.Errorf("stage record for job %q is %d bytes, maximum is %d", continuation.Descriptor.Job, len(encoded), maxStageRecordBytes)
	}
	digest := sha256Digest(encoded)
	path, err := buildkitepipeline.StagePath(digest)
	if err != nil {
		return transport.Artifact{}, buildkitepipeline.Job{}, err
	}
	step := buildkitepipeline.Job{
		Key:                continuation.StepKey,
		Label:              continuation.JobLabel(continuation.Descriptor.Job),
		CheckLabel:         continuation.Descriptor.Job + " (" + continuation.Descriptor.Kind() + ")",
		Queue:              producer.Queue,
		Platform:           producer.Platform,
		DistributionDigest: producer.DistributionDigest,
		RuntimeImage:       producer.RuntimeImage,
		Dependencies:       dependencies,
		Stage:              &buildkitepipeline.StageStep{ArtifactDigest: digest, Kind: continuation.Descriptor.Kind()},
	}
	return transport.Artifact{Path: path, Digest: digest, Contents: encoded}, step, nil
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decodeStageRecord strictly decodes a record an earlier stage wrote and
// checks that it was produced for this compiler version.
func decodeStageRecord(data []byte, version string) (stageRecord, error) {
	if len(data) > maxStageRecordBytes {
		return stageRecord{}, fmt.Errorf("stage record is %d bytes, maximum is %d", len(data), maxStageRecordBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	// Resolved rows keep their numbers as json.Number, as the producer
	// output was decoded, so the recompilation derives the same instance keys
	// and plan digests as the stage that expanded them.
	decoder.UseNumber()
	var record stageRecord
	if err := decoder.Decode(&record); err != nil {
		return stageRecord{}, fmt.Errorf("decode stage record: %w", err)
	}
	if decoder.More() {
		return stageRecord{}, errors.New("decode stage record: trailing data")
	}
	if record.Schema != stageSchema {
		return stageRecord{}, fmt.Errorf("stage record schema %q is not %q", record.Schema, stageSchema)
	}
	if record.Version != version {
		return stageRecord{}, fmt.Errorf("stage record was written by buildkite-gha %s, but %s is running", record.Version, version)
	}
	if !stageDigestPattern.MatchString(record.Distribution) || !stageDigestPattern.MatchString(record.Workflow.Digest) || !stageDigestPattern.MatchString(record.Event.Digest) {
		return stageRecord{}, errors.New("stage record has an invalid digest")
	}
	if !checkoutWorkflowPath(record.Workflow.Path) {
		return stageRecord{}, fmt.Errorf("stage record has an invalid workflow path %q", record.Workflow.Path)
	}
	if err := validateContinuationShape(record.Continuation); err != nil {
		return stageRecord{}, fmt.Errorf("stage record: %w", err)
	}
	for _, other := range record.Others {
		if err := validateContinuationShape(other); err != nil {
			return stageRecord{}, fmt.Errorf("stage record other continuation: %w", err)
		}
		if other.StepKey == record.Continuation.StepKey || other.Descriptor.Job == record.Continuation.Descriptor.Job {
			return stageRecord{}, fmt.Errorf("stage record lists its own step %q among the other continuations", other.StepKey)
		}
	}
	if len(record.Graph) == 0 {
		return stageRecord{}, errors.New("stage record does not record the initial job graph")
	}
	graph := make(map[string]compiledJob, len(record.Graph))
	for _, job := range record.Graph {
		if _, exists := graph[job.Key]; exists {
			return stageRecord{}, fmt.Errorf("stage record repeats graph key %q", job.Key)
		}
		graph[job.Key] = job
	}
	// Each root's producer is an uploaded instance of its producer job, so the
	// result the stage reads is one this build's graph published.
	for _, root := range record.Continuation.Roots() {
		job, exists := graph[root.ProducerStepKey]
		if !exists || job.LogicalJob != root.Descriptor.ProducerJob || !stageDigestPattern.MatchString(job.PlanDigest) {
			return stageRecord{}, fmt.Errorf("stage record producer of %q is not in the initial graph", root.Descriptor.Job)
		}
	}
	owners := make(map[string]string)
	for _, component := range append([]compiler.RuntimeContinuation{record.Continuation}, record.Others...) {
		for _, job := range component.Jobs {
			if owner, exists := owners[job]; exists {
				return stageRecord{}, fmt.Errorf("job %q has duplicate continuation owners %q and %q", job, owner, component.StepKey)
			}
			owners[job] = component.StepKey
		}
	}
	for _, resolved := range record.Resolved {
		if _, deferred := owners[resolved.Job]; deferred || resolved.Job == "" {
			return stageRecord{}, fmt.Errorf("stage record resolves job %q, which is still deferred or already resolved", resolved.Job)
		}
		owners[resolved.Job] = "resolved"
		if err := validateResolvedMatrix(resolved); err != nil {
			return stageRecord{}, fmt.Errorf("stage record resolved matrix of job %q: %w", resolved.Job, err)
		}
		producer, exists := graph[resolved.ProducerStepKey]
		if !exists || !stageDigestPattern.MatchString(producer.PlanDigest) || !stageDigestPattern.MatchString(resolved.ResultDigest) {
			return stageRecord{}, fmt.Errorf("stage record resolved matrix of job %q has an invalid producer result binding", resolved.Job)
		}
	}
	for _, runner := range record.Runners {
		if _, err := compiler.ParsePlatform(runner.Platform); err != nil {
			return stageRecord{}, fmt.Errorf("stage record runner %q: %w", runner.Label, err)
		}
	}
	for platform := range record.Runtimes {
		if _, err := compiler.ParsePlatform(platform); err != nil {
			return stageRecord{}, fmt.Errorf("stage record runtime: %w", err)
		}
	}
	return record, nil
}

// validateResolvedMatrix checks that a root an earlier stage expanded records
// either its skip or the scalar rows the producer published. The rows' effect
// is verified later: the recompilation must reproduce the jobs the earlier
// stage uploaded from them.
func validateResolvedMatrix(resolved resolvedMatrix) error {
	if resolved.Skipped {
		if len(resolved.Rows) != 0 {
			return errors.New("records rows for a skipped matrix")
		}
		return nil
	}
	if len(resolved.Rows) == 0 || len(resolved.Rows) > compiler.MaxRuntimeMatrixInstances {
		return fmt.Errorf("must record between 1 and %d rows", compiler.MaxRuntimeMatrixInstances)
	}
	for _, row := range resolved.Rows {
		if len(row) == 0 || len(row) > compiler.MaxRuntimeMatrixProperties {
			return fmt.Errorf("rows must have between 1 and %d properties", compiler.MaxRuntimeMatrixProperties)
		}
		for key, value := range row {
			switch value.(type) {
			case string, bool, json.Number:
			default:
				return fmt.Errorf("row property %q is not a string, boolean, or number", key)
			}
		}
	}
	return nil
}

// validateContinuationShape checks that a recorded continuation names its
// step, its producer instance, and its deferred jobs, consumer first.
func validateContinuationShape(continuation compiler.RuntimeContinuation) error {
	if err := continuation.Descriptor.Validate(); err != nil {
		return fmt.Errorf("descriptor: %w", err)
	}
	if continuation.Scheduling && (len(continuation.Joined) != 0 || len(continuation.LaterMatrices) != 0) {
		return errors.New("needs-derived scheduling requires a single-root, single-stage component")
	}
	if continuation.StepKey == "" || continuation.ProducerStepKey == "" || len(continuation.Jobs) == 0 || continuation.Jobs[0] != continuation.Descriptor.Job {
		return errors.New("does not name its deferred jobs")
	}
	roots := make(map[string]bool)
	for _, root := range continuation.Roots() {
		if err := root.Descriptor.Validate(); err != nil {
			return fmt.Errorf("descriptor: %w", err)
		}
		if root.Descriptor.Shape == compiler.RuntimeRunsOnShape && (len(continuation.Joined) != 0 || len(continuation.LaterMatrices) != 0) {
			return errors.New("runner selection cannot join or feed another deferred stage")
		}
		job := root.Descriptor.Job
		if roots[job] || root.ProducerStepKey == "" || !slices.Contains(continuation.Jobs, job) {
			return fmt.Errorf("invalid or duplicate deferred root %q", job)
		}
		if _, exists := continuation.Instances[job]; exists {
			return errors.New("must record the instances of every deferred dependent and none for the consumer")
		}
		roots[job] = true
	}
	// A later matrix reads its rows from a job this continuation compiles, so
	// it has neither known instances nor a producer result yet.
	for _, job := range continuation.LaterMatrices {
		if _, exists := continuation.Instances[job]; roots[job] || exists || !slices.Contains(continuation.Jobs, job) {
			return fmt.Errorf("records an invalid later matrix %q", job)
		}
	}
	if len(continuation.Instances)+len(continuation.LaterMatrices) != len(continuation.Jobs)-len(roots) {
		return errors.New("must record the instances of every deferred dependent and none for the consumer")
	}
	for _, job := range continuation.Jobs {
		if roots[job] || slices.Contains(continuation.LaterMatrices, job) {
			continue
		}
		instances := continuation.Instances[job]
		if len(instances) == 0 {
			return fmt.Errorf("records no instances for deferred dependent %q", job)
		}
		for _, instance := range instances {
			if instance.Key == "" || instance.Label == "" || instance.CheckLabel == "" {
				return fmt.Errorf("records an incomplete instance for %q", job)
			}
		}
	}
	if len(continuation.Sources) != len(continuation.Jobs) {
		return errors.New("must record the source of every deferred job")
	}
	for _, job := range continuation.Jobs {
		source, exists := continuation.Sources[job]
		if !exists || source.Path == "" || !stageDigestPattern.MatchString(source.Digest) {
			return fmt.Errorf("records no source for deferred job %q", job)
		}
		if remote := source.Remote; remote != nil && (remote.Repository == "" || remote.RequestedRef == "" || remote.Commit == "" || !stageDigestPattern.MatchString(remote.SourceDigest)) {
			return fmt.Errorf("records an incomplete remote source for deferred job %q", job)
		}
	}
	if _, err := plan.ValidateActionLockList(continuation.ActionLocks); err != nil {
		return fmt.Errorf("action locks: %w", err)
	}
	if continuation.JobBudget < len(roots)+continuation.DependentInstances() || continuation.JobBudget > compiler.MaxRuntimeMatrixGraphJobs {
		return fmt.Errorf("records an invalid job budget %d", continuation.JobBudget)
	}
	return nil
}

// compileRequest rebuilds the importer's compilation from the recorded
// inputs: the same runner mapping, runtime digests, variables, OIDC, event
// exposure, and namespace, with the verified matrix rows supplied for the
// roots of this stage and of the earlier stages, and the deferred jobs'
// actions pinned to the revisions the importer resolved. The workflow and
// event bytes come from the caller, which has verified their digests; the
// repository source reads remote reusable workflows and actions the way the
// importer did. Runner resolution and the environment source are live policy
// the caller attaches before compiling.
func (s stageRecord) compileRequest(workflowPath string, workflowSource, eventSource []byte, rows map[string][]map[string]any, skipped map[string]bool, runnerOutputs map[string]string, repositorySource compiler.RepositorySource) hostedCompileRequest {
	allRows := make(map[string][]map[string]any, len(rows)+len(s.Resolved))
	allSkipped := make(map[string]bool, len(skipped))
	for _, resolved := range s.Resolved {
		allRows[resolved.Job] = resolved.Rows
		if resolved.Skipped {
			allSkipped[resolved.Job] = true
		}
	}
	maps.Copy(allRows, rows)
	maps.Copy(allSkipped, skipped)
	targets := make(map[string]compiler.RunnerTarget, len(s.Runners))
	for _, runner := range s.Runners {
		platform, _ := compiler.ParsePlatform(runner.Platform)
		targets[runner.Label] = compiler.RunnerTarget{Queue: runner.Queue, Platform: platform, Image: runner.Image, Cache: runner.Cache}
	}
	runtimes := make(map[compiler.Platform]string, len(s.Runtimes))
	for name, digest := range s.Runtimes {
		platform, _ := compiler.ParsePlatform(name)
		runtimes[platform] = digest
	}
	// One recompilation resolves the actions of this stage's jobs and of the
	// workflow's other continuations, so every recorded lock applies.
	locks := append([]plan.ActionLock(nil), s.Continuation.ActionLocks...)
	for _, other := range s.Others {
		locks = append(locks, other.ActionLocks...)
	}
	return hostedCompileRequest{
		WorkflowPath:             workflowPath,
		WorkflowSource:           workflowSource,
		EventSource:              eventSource,
		EventFile:                s.Event.File,
		Version:                  s.Version,
		DistributionDigest:       s.Distribution,
		ImporterStep:             "pipeline-trigger-importer",
		StepKeyNamespace:         s.Workflow.Namespace,
		RunnerTargets:            targets,
		RuntimeDistributions:     runtimes,
		OIDC:                     s.OIDC,
		Vars:                     compiler.VariableSources{Organization: s.Vars.Organization, Repository: s.Vars.Repository, Resolved: s.Vars.Resolved},
		RepositorySource:         repositorySource,
		RuntimeMatrixRows:        allRows,
		RuntimeMatrixSkipped:     allSkipped,
		RuntimeMatrixActionLocks: locks,
		RuntimeRunsOnOutputs:     runnerOutputs,
	}
}
