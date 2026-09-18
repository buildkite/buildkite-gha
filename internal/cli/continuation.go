package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/git"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// continuationSchema identifies the continuation artifact format. The
// continuation step rejects any other schema or compiler version so that a
// deferred compile can never run with inputs a different compiler produced.
const continuationSchema = "buildkite-gha/continuation/v1"

// maxContinuationArtifactBytes bounds the artifact a continuation reads back.
// The artifact records the job graph, runner mappings, and variables but not
// the event, which every continuation of an upload shares by digest, so the
// bound is independent of the event size. The importer enforces the same
// bound when it writes the artifact, so an upload never records a
// continuation its own step would refuse to read.
const maxContinuationArtifactBytes = 4 * 1024 * 1024

// maxContinuationWorkflowBytes bounds the workflow file a continuation reads
// from the checkout before comparing its digest with the recorded one.
const maxContinuationWorkflowBytes = 4 * 1024 * 1024

var continuationDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// continuationArtifact carries everything one deferred upload needs to compile
// the jobs behind a needs-derived matrix exactly as the importer would have.
// The importer writes it during the initial upload; the continuation step reads
// it back by digest, so the producer job output is the only input that the
// initial upload did not fix.
type continuationArtifact struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	// Distribution is the compiler distribution digest recorded in job plans.
	Distribution string `json:"distribution_digest"`
	// Runtimes maps each platform to the runtime distribution digest the
	// importer uploaded. A deferred job on a platform without one fails.
	Runtimes map[string]string `json:"runtime_digests"`
	// Importer is the job whose artifacts hold the distributions and this file.
	Importer string               `json:"importer"`
	Workflow continuationWorkflow `json:"workflow"`
	Event    continuationEvent    `json:"event"`
	// Runners are the explicit runs-on mappings the importer was configured
	// with. The continuation applies the same policy, so a producer cannot
	// route a job to a queue the importer would not have used.
	Runners    []continuationRunner    `json:"runners,omitempty"`
	Vars       continuationVars        `json:"vars"`
	OIDC       *plan.OIDCConfiguration `json:"oidc,omitempty"`
	RunnerUser bool                    `json:"experimental_runner_user"`
	// PrivateReusableWorkflows records the plugin setting that lets the
	// importer read remote reusable workflows through the agent's Git
	// credentials. The continuation reads them the same way.
	PrivateReusableWorkflows bool `json:"private_reusable_workflows,omitempty"`
	// Continuation names the consumer, its deferred dependents, and the step
	// that performs the upload.
	Continuation compiler.RuntimeMatrixContinuation `json:"continuation"`
	// Others are the workflow's remaining continuations, each expanded by its
	// own deferred step. A recompilation must leave exactly these deferred.
	Others []compiler.RuntimeMatrixContinuation `json:"other_continuations,omitempty"`
	// Producer is the generated job whose verified result supplies the matrix.
	Producer continuationProducer `json:"producer"`
	// JoinedProducers binds each additional root to its parent-owned plan.
	JoinedProducers map[string]continuationProducer `json:"joined_producers,omitempty"`
	// Graph records every job the initial upload created so the continuation
	// can prove that its recompilation reproduced them before uploading.
	Graph         []compiledJob `json:"graph"`
	ApprovalGates []string      `json:"approval_gates,omitempty"`
}

type continuationWorkflow struct {
	// Path is the repository-relative workflow path the continuation reads
	// from its checkout; Digest is the SHA-256 of the bytes the importer used.
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Namespace  string `json:"step_key_namespace"`
	GroupLabel string `json:"group_label"`
	CheckName  string `json:"check_name"`
}

type continuationEvent struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Digest names the event source artifact the importer uploaded once for
	// every continuation; see buildkitepipeline.ContinuationEventPath.
	Digest string `json:"source_digest"`
	// File records whether generated jobs expose the payload through
	// GITHUB_EVENT_PATH, which is part of every plan digest.
	File bool `json:"file"`
}

type continuationRunner struct {
	Label    string                         `json:"label"`
	Queue    string                         `json:"queue,omitempty"`
	Platform string                         `json:"platform"`
	Image    string                         `json:"image,omitempty"`
	Cache    *buildkitepipeline.CacheVolume `json:"cache,omitempty"`
}

type continuationVars struct {
	Organization map[string]string `json:"organization,omitempty"`
	Repository   map[string]string `json:"repository,omitempty"`
	Resolved     bool              `json:"resolved"`
}

type continuationProducer struct {
	StepKey    string `json:"step_key"`
	PlanDigest string `json:"plan_digest"`
}

// continuationInputs are the importer-side values one artifact is built from:
// the request the importer compiled, its result, and the upload-wide
// settings the request does not carry.
type continuationInputs struct {
	// request is the compilation the importer admitted, with the variables
	// its final compile used; the artifact records the request's inputs so
	// the deferred upload rebuilds the same request.
	request hostedCompileRequest
	bundle  compiler.Bundle
	// importer is the job whose artifacts hold the distributions and the
	// continuation records.
	importer string
	// buildCommit is BUILDKITE_COMMIT as the agent set it; empty or symbolic
	// values fall back to the checked-out HEAD.
	buildCommit           string
	workflow              workflowInput
	groupLabel, checkName string
	event                 effectiveEventSelection
	runnerUser            bool
	privateReusable       bool
}

// checkoutWorkflowPath reports whether a recorded workflow path is a cleaned,
// slash-separated path inside the build checkout, which is where the deferred
// upload reads the workflow again. A filename may contain "..", but no segment
// may be "..", and the path may not be absolute.
func checkoutWorkflowPath(workflowPath string) bool {
	return workflowPath != "" && path.Clean(workflowPath) == workflowPath && filepath.IsLocal(filepath.FromSlash(workflowPath))
}

// requireCheckoutWorkflow proves the deferred upload can reopen the workflow
// from a fresh checkout of the build commit: the recorded path lies inside the
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

// buildContinuationArtifacts returns the upload artifact and the deferred step
// for every continuation in the bundle, plus the one event source artifact
// they all reference. The producer's plan digest comes from the initial
// bundle, so the continuation reads exactly the result the producer this build
// ran will publish.
func buildContinuationArtifacts(inputs continuationInputs) (transport.Artifact, []transport.Artifact, []buildkitepipeline.Job, error) {
	bundle, request := inputs.bundle, inputs.request
	if err := requireCheckoutWorkflow(inputs.workflow, inputs.buildCommit); err != nil {
		return transport.Artifact{}, nil, nil, err
	}
	graph := compiledJobs(bundle)
	plans := make(map[string]string, len(graph))
	for _, job := range graph {
		if job.PlanDigest == "" {
			return transport.Artifact{}, nil, nil, fmt.Errorf("job %q has no plan to record for the continuation", job.Key)
		}
		plans[job.Key] = job.PlanDigest
	}
	generated := make(map[string]buildkitepipeline.Job, len(bundle.GeneratedWorkflow.Jobs))
	for _, job := range bundle.GeneratedWorkflow.Jobs {
		generated[job.Key] = job
	}
	gates := make([]string, 0, len(bundle.GeneratedWorkflow.ApprovalGates))
	for _, gate := range bundle.GeneratedWorkflow.ApprovalGates {
		gates = append(gates, gate.Key)
	}
	runtimes := make(map[string]string, len(request.RuntimeDistributions))
	for platform, digest := range request.RuntimeDistributions {
		runtimes[platform.String()] = digest
	}
	runnerLabels := make([]string, 0, len(request.RunnerTargets))
	for label := range request.RunnerTargets {
		runnerLabels = append(runnerLabels, label)
	}
	sort.Strings(runnerLabels)
	runners := make([]continuationRunner, 0, len(runnerLabels))
	for _, label := range runnerLabels {
		target := request.RunnerTargets[label]
		runners = append(runners, continuationRunner{Label: label, Queue: target.Queue, Platform: target.Platform.String(), Image: target.Image, Cache: target.Cache})
	}
	if len(inputs.event.Source) > plan.MaxEventPayloadBytes {
		return transport.Artifact{}, nil, nil, fmt.Errorf("event source is %d bytes, maximum is %d", len(inputs.event.Source), plan.MaxEventPayloadBytes)
	}
	eventDigest := sha256Digest(inputs.event.Source)
	eventPath, err := buildkitepipeline.ContinuationEventPath(eventDigest)
	if err != nil {
		return transport.Artifact{}, nil, nil, err
	}
	event := transport.Artifact{Path: eventPath, Digest: eventDigest, Contents: inputs.event.Source}
	artifacts := make([]transport.Artifact, 0, len(bundle.IR.Continuations))
	steps := make([]buildkitepipeline.Job, 0, len(bundle.IR.Continuations))
	for i, continuation := range bundle.IR.Continuations {
		// A one-row static matrix gives the producer a digest-suffixed key, so
		// the compiler records the producer's actual instance key.
		producerKey := continuation.ProducerStepKey
		producerDigest, ok := plans[producerKey]
		if !ok {
			return transport.Artifact{}, nil, nil, fmt.Errorf("matrix producer %q of job %q has no plan", continuation.Descriptor.ProducerJob, continuation.Descriptor.Job)
		}
		producer, ok := generated[producerKey]
		if !ok {
			return transport.Artifact{}, nil, nil, fmt.Errorf("matrix producer %q of job %q has no generated step", continuation.Descriptor.ProducerJob, continuation.Descriptor.Job)
		}
		var joined map[string]continuationProducer
		dependencies := []string{producerKey}
		for _, root := range continuation.Joined {
			digest, ok := plans[root.ProducerStepKey]
			if !ok {
				return transport.Artifact{}, nil, nil, fmt.Errorf("matrix producer %q of job %q has no plan", root.Descriptor.ProducerJob, root.Descriptor.Job)
			}
			if joined == nil {
				joined = make(map[string]continuationProducer)
			}
			joined[root.Descriptor.Job] = continuationProducer{StepKey: root.ProducerStepKey, PlanDigest: digest}
			if !slices.Contains(dependencies, root.ProducerStepKey) {
				dependencies = append(dependencies, root.ProducerStepKey)
			}
		}
		artifact := continuationArtifact{
			Schema:       continuationSchema,
			Version:      request.Version,
			Distribution: request.DistributionDigest,
			Runtimes:     runtimes,
			Importer:     inputs.importer,
			Workflow: continuationWorkflow{
				Path:       inputs.workflow.CanonicalPath,
				Digest:     sha256Digest(request.WorkflowSource),
				Namespace:  request.StepKeyNamespace,
				GroupLabel: inputs.groupLabel,
				CheckName:  inputs.checkName,
			},
			Event:                    continuationEvent{Name: inputs.event.Event.Event, Provider: inputs.event.Event.Provider, Digest: eventDigest, File: request.EventFile},
			Runners:                  runners,
			Vars:                     continuationVars{Organization: request.Vars.Organization, Repository: request.Vars.Repository, Resolved: request.Vars.Resolved},
			OIDC:                     request.OIDC,
			RunnerUser:               inputs.runnerUser,
			PrivateReusableWorkflows: inputs.privateReusable,
			Continuation:             continuation,
			Others:                   append(append([]compiler.RuntimeMatrixContinuation(nil), bundle.IR.Continuations[:i]...), bundle.IR.Continuations[i+1:]...),
			Producer:                 continuationProducer{StepKey: producerKey, PlanDigest: producerDigest},
			JoinedProducers:          joined,
			Graph:                    graph,
			ApprovalGates:            gates,
		}
		encoded, err := json.Marshal(artifact)
		if err != nil {
			return transport.Artifact{}, nil, nil, fmt.Errorf("encode continuation for job %q: %w", continuation.Descriptor.Job, err)
		}
		if len(encoded) > maxContinuationArtifactBytes {
			return transport.Artifact{}, nil, nil, fmt.Errorf("continuation for job %q is %d bytes, maximum is %d", continuation.Descriptor.Job, len(encoded), maxContinuationArtifactBytes)
		}
		digest := sha256Digest(encoded)
		path, err := buildkitepipeline.ContinuationPath(digest)
		if err != nil {
			return transport.Artifact{}, nil, nil, err
		}
		artifacts = append(artifacts, transport.Artifact{Path: path, Digest: digest, Contents: encoded})
		steps = append(steps, buildkitepipeline.Job{
			Key:                continuation.StepKey,
			Label:              continuation.JobLabel(continuation.Descriptor.Job),
			CheckLabel:         continuationCheckLabel(continuation.Descriptor.Job),
			Queue:              producer.Queue,
			Platform:           producer.Platform,
			DistributionDigest: producer.DistributionDigest,
			RuntimeImage:       producer.RuntimeImage,
			Dependencies:       dependencies,
			Continuation:       &buildkitepipeline.ContinuationStep{ArtifactDigest: digest},
		})
	}
	return event, artifacts, steps, nil
}

// continuationCheckLabel names the GitHub check of the deferred upload step.
// Expanded instances are labelled "job (key=value)", so the suffix cannot
// collide with them.
func continuationCheckLabel(job string) string {
	return job + " (matrix)"
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decodeContinuationArtifact strictly decodes an artifact the importer wrote
// and checks that it was produced for this compiler version.
func decodeContinuationArtifact(data []byte, version string) (continuationArtifact, error) {
	if len(data) > maxContinuationArtifactBytes {
		return continuationArtifact{}, fmt.Errorf("continuation artifact is %d bytes, maximum is %d", len(data), maxContinuationArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact continuationArtifact
	if err := decoder.Decode(&artifact); err != nil {
		return continuationArtifact{}, fmt.Errorf("decode continuation artifact: %w", err)
	}
	if decoder.More() {
		return continuationArtifact{}, errors.New("decode continuation artifact: trailing data")
	}
	if artifact.Schema != continuationSchema {
		return continuationArtifact{}, fmt.Errorf("continuation artifact schema %q is not %q", artifact.Schema, continuationSchema)
	}
	if artifact.Version != version {
		return continuationArtifact{}, fmt.Errorf("continuation artifact was written by buildkite-gha %s, but %s is running", artifact.Version, version)
	}
	if !continuationDigestPattern.MatchString(artifact.Distribution) || !continuationDigestPattern.MatchString(artifact.Workflow.Digest) || !continuationDigestPattern.MatchString(artifact.Producer.PlanDigest) || !continuationDigestPattern.MatchString(artifact.Event.Digest) {
		return continuationArtifact{}, errors.New("continuation artifact has an invalid digest")
	}
	if !checkoutWorkflowPath(artifact.Workflow.Path) {
		return continuationArtifact{}, fmt.Errorf("continuation artifact has an invalid workflow path %q", artifact.Workflow.Path)
	}
	if err := validateContinuationShape(artifact.Continuation); err != nil {
		return continuationArtifact{}, fmt.Errorf("continuation artifact: %w", err)
	}
	for _, other := range artifact.Others {
		if err := validateContinuationShape(other); err != nil {
			return continuationArtifact{}, fmt.Errorf("continuation artifact other continuation: %w", err)
		}
		if other.StepKey == artifact.Continuation.StepKey || other.Descriptor.Job == artifact.Continuation.Descriptor.Job {
			return continuationArtifact{}, fmt.Errorf("continuation artifact lists its own step %q among the other continuations", other.StepKey)
		}
	}
	if artifact.Producer.StepKey == "" || len(artifact.Graph) == 0 {
		return continuationArtifact{}, errors.New("continuation artifact does not record the initial job graph")
	}
	if len(artifact.JoinedProducers) != len(artifact.Continuation.Joined) {
		return continuationArtifact{}, errors.New("continuation artifact must bind every joined producer")
	}
	graph := make(map[string]compiledJob, len(artifact.Graph))
	for _, job := range artifact.Graph {
		if _, exists := graph[job.Key]; exists {
			return continuationArtifact{}, fmt.Errorf("continuation artifact repeats graph key %q", job.Key)
		}
		graph[job.Key] = job
	}
	for _, root := range artifact.Continuation.Roots() {
		producer := artifact.producer(root.Descriptor.Job)
		job, exists := graph[producer.StepKey]
		if !exists || producer.StepKey != root.ProducerStepKey || job.LogicalJob != root.Descriptor.ProducerJob || job.PlanDigest != producer.PlanDigest || !continuationDigestPattern.MatchString(producer.PlanDigest) {
			return continuationArtifact{}, fmt.Errorf("continuation artifact producer binding for %q does not match the initial graph", root.Descriptor.Job)
		}
	}
	owners := make(map[string]string)
	for _, component := range append([]compiler.RuntimeMatrixContinuation{artifact.Continuation}, artifact.Others...) {
		for _, job := range component.Jobs {
			if owner, exists := owners[job]; exists {
				return continuationArtifact{}, fmt.Errorf("job %q has duplicate continuation owners %q and %q", job, owner, component.StepKey)
			}
			owners[job] = component.StepKey
		}
	}
	for _, runner := range artifact.Runners {
		if _, err := compiler.ParsePlatform(runner.Platform); err != nil {
			return continuationArtifact{}, fmt.Errorf("continuation artifact runner %q: %w", runner.Label, err)
		}
	}
	for platform := range artifact.Runtimes {
		if _, err := compiler.ParsePlatform(platform); err != nil {
			return continuationArtifact{}, fmt.Errorf("continuation artifact runtime: %w", err)
		}
	}
	return artifact, nil
}

func (a continuationArtifact) producer(job string) continuationProducer {
	if job == a.Continuation.Descriptor.Job {
		return a.Producer
	}
	return a.JoinedProducers[job]
}

// validateContinuationShape checks that a recorded continuation names its
// step, its producer instance, and its deferred jobs, consumer first.
func validateContinuationShape(continuation compiler.RuntimeMatrixContinuation) error {
	if err := continuation.Descriptor.Validate(); err != nil {
		return fmt.Errorf("descriptor: %w", err)
	}
	if continuation.StepKey == "" || continuation.ProducerStepKey == "" || len(continuation.Jobs) == 0 || continuation.Jobs[0] != continuation.Descriptor.Job {
		return errors.New("does not name its deferred jobs")
	}
	roots := make(map[string]bool)
	for _, root := range continuation.Roots() {
		if err := root.Descriptor.Validate(); err != nil {
			return fmt.Errorf("descriptor: %w", err)
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
	if len(continuation.Instances) != len(continuation.Jobs)-len(roots) {
		return errors.New("must record the instances of every deferred dependent and none for the consumer")
	}
	for _, job := range continuation.Jobs {
		if roots[job] {
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
		if !exists || source.Path == "" || !continuationDigestPattern.MatchString(source.Digest) {
			return fmt.Errorf("records no source for deferred job %q", job)
		}
		if remote := source.Remote; remote != nil && (remote.Repository == "" || remote.RequestedRef == "" || remote.Commit == "" || !continuationDigestPattern.MatchString(remote.SourceDigest)) {
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
// consumer and the deferred jobs' actions pinned to the revisions the
// importer resolved. The workflow and event bytes come from the caller, which
// has verified their digests; the repository source reads remote reusable
// workflows and actions the way the importer did. Runner resolution and the
// environment source are live policy the caller attaches before compiling.
func (a continuationArtifact) compileRequest(workflowPath string, workflowSource, eventSource []byte, rows map[string][]map[string]any, repositorySource compiler.RepositorySource) hostedCompileRequest {
	targets := make(map[string]compiler.RunnerTarget, len(a.Runners))
	for _, runner := range a.Runners {
		platform, _ := compiler.ParsePlatform(runner.Platform)
		targets[runner.Label] = compiler.RunnerTarget{Queue: runner.Queue, Platform: platform, Image: runner.Image, Cache: runner.Cache}
	}
	runtimes := make(map[compiler.Platform]string, len(a.Runtimes))
	for name, digest := range a.Runtimes {
		platform, _ := compiler.ParsePlatform(name)
		runtimes[platform] = digest
	}
	// One recompilation resolves the actions of this continuation's jobs and
	// of the workflow's other continuations, so every recorded lock applies.
	locks := append([]plan.ActionLock(nil), a.Continuation.ActionLocks...)
	for _, other := range a.Others {
		locks = append(locks, other.ActionLocks...)
	}
	return hostedCompileRequest{
		WorkflowPath:             workflowPath,
		WorkflowSource:           workflowSource,
		EventSource:              eventSource,
		EventFile:                a.Event.File,
		Version:                  a.Version,
		DistributionDigest:       a.Distribution,
		ImporterStep:             "pipeline-trigger-importer",
		StepKeyNamespace:         a.Workflow.Namespace,
		RunnerTargets:            targets,
		RuntimeDistributions:     runtimes,
		OIDC:                     a.OIDC,
		Vars:                     compiler.VariableSources{Organization: a.Vars.Organization, Repository: a.Vars.Repository, Resolved: a.Vars.Resolved},
		RepositorySource:         repositorySource,
		RuntimeMatrixRows:        rows,
		RuntimeMatrixActionLocks: locks,
	}
}
