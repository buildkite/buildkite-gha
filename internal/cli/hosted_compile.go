package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// hostedCompileRequest is every input to one hosted compilation of one
// workflow against one event. Validation, the runtime-platform preflight, and
// compilation derive their compiler options from the same request, so the
// admitted workflow, the runtimes the upload acquires, and the compiled
// pipeline cannot disagree about policy.
//
// A request always compiles the whole workflow. A later compilation stage
// that continues an earlier upload builds the same request from the inputs
// the importer recorded and uploads only the jobs it is allowed to add;
// compiledJobs gives it the identities the earlier upload must reproduce.
type hostedCompileRequest struct {
	WorkflowPath   string
	WorkflowSource []byte
	EventSource    []byte
	// EventFile records that the event came from a file instead of the
	// build, so compiled jobs read the payload from an uploaded artifact.
	EventFile bool

	// Version is the compiler version stamped into plans; DistributionDigest
	// is the importer's own runtime distribution digest.
	Version            string
	DistributionDigest string
	// ImporterStep is the pipeline step that compiled the workflow, or empty
	// when the generated pipeline is not uploaded by itself.
	ImporterStep string
	GroupLabel   string
	// StepKeyNamespace keeps step keys of workflows uploaded together apart.
	StepKeyNamespace string

	// RunnerTargets are the organization's configured runner labels;
	// RunnerResolution is the agent's live verdict for the labels the
	// workflow uses, or empty when resolution was unavailable.
	RunnerTargets    map[string]compiler.RunnerTarget
	RunnerResolution agentRunnerResolution
	// RuntimeDistributions maps each platform to the digest of the runtime
	// distribution its jobs bootstrap.
	RuntimeDistributions map[compiler.Platform]string
	OIDC                 *plan.OIDCConfiguration
	EnvironmentSource    compiler.EnvironmentSource
	Vars                 compiler.VariableSources

	// RepositorySource reads remote reusable workflows and actions. When nil,
	// compileHostedRequest builds a source authenticated for the workflow's
	// own repository through ActionAuthentication and rooted at
	// ActionCacheDir, or a temporary directory when that is empty.
	RepositorySource     compiler.RepositorySource
	ActionCacheDir       string
	ActionAuthentication *actionSourceAuthentication

	// RuntimeMatrixRows supplies, per consumer job, the verified rows of a
	// matrix whose values come from a job output. An initial compilation
	// leaves it empty and defers those jobs to a continuation; the
	// continuation compiles the same request with the rows the producer
	// published. RuntimeMatrixActionLocks pins the actions of the deferred
	// jobs to the revisions the initial compilation resolved.
	RuntimeMatrixRows        map[string][]map[string]any
	RuntimeMatrixSkipped     map[string]bool
	RuntimeMatrixActionLocks []plan.ActionLock
}

// validationOptions returns the options for validating the workflow against
// the event before compilation: the hosted runner policy, runner resolution,
// namespace, variables, and repository source, without the compile-only
// inputs that never affect admission.
func (r hostedCompileRequest) validationOptions() compiler.Options {
	options := hostedOptions("", r.RunnerTargets, nil)
	options.WorkflowSource = candidateWorkflowSource(r.EventSource)
	applyRunnerResolution(&options, r.RunnerResolution)
	options.StepKeyNamespace = r.StepKeyNamespace
	options.RepositorySource = r.RepositorySource
	options.Vars = r.Vars
	options.RuntimeMatrixRows = r.RuntimeMatrixRows
	options.RuntimeMatrixSkipped = r.RuntimeMatrixSkipped
	return options
}

// options returns the options for compiling the workflow, before the
// repository source is attached.
func (r hostedCompileRequest) options() compiler.Options {
	options := hostedOptions(r.GroupLabel, r.RunnerTargets, r.RuntimeDistributions)
	options.WorkflowSource = candidateWorkflowSource(r.EventSource)
	options.EventFile = r.EventFile
	applyRunnerResolution(&options, r.RunnerResolution)
	options.StepKeyNamespace = r.StepKeyNamespace
	options.OIDC = r.OIDC
	options.EnvironmentSource = r.EnvironmentSource
	options.Vars = r.Vars
	options.RuntimeMatrixRows = r.RuntimeMatrixRows
	options.RuntimeMatrixSkipped = r.RuntimeMatrixSkipped
	options.RuntimeMatrixActionLocks = r.RuntimeMatrixActionLocks
	return options
}

// validateHostedRequest validates the request's workflow against its event.
func validateHostedRequest(ctx context.Context, request hostedCompileRequest) (compiler.Report, error) {
	return compiler.ValidateEventWithOptionsContext(ctx, request.WorkflowPath, request.WorkflowSource, request.EventSource, request.validationOptions())
}

// compileHostedRequest compiles the request's workflow, admits the result
// for hosted execution, and generates its pipeline. The returned error is a
// hostedFailure whose kind says whether the environment, evaluation, or
// admission failed; the compilation carries whatever partial result the
// caller may still report or upload.
func compileHostedRequest(ctx context.Context, request hostedCompileRequest) (hostedCompilation, error) {
	repositorySource := request.RepositorySource
	cleanup := func() {}
	if repositorySource == nil {
		var sourceOptions []actionsource.Option
		if event, eventErr := compiler.ParseEvent(request.EventSource); eventErr == nil && event.Provider == "github" {
			authenticationOption := request.ActionAuthentication.option(event.Repository.Owner + "/" + event.Repository.Name)
			if authenticationOption != nil {
				sourceOptions = append(sourceOptions, authenticationOption)
			}
		}
		var err error
		repositorySource, cleanup, err = newHostedActionSource(ctx, request.ActionCacheDir, request.Version, sourceOptions, nil)
		if err != nil {
			return hostedCompilation{}, hostedError(hostedEnvironmentFailure, err)
		}
	}
	defer cleanup()
	options := request.options()
	options.RepositorySource = repositorySource
	options.ResolveActions = true
	options.ActionSource = repositorySource
	bundle, err := compiler.CompileBundlePlansContext(ctx, request.WorkflowPath, request.WorkflowSource, request.EventSource, request.Version, request.DistributionDigest, options)
	hasActions := irUsesActions(bundle.IR)
	compiled := hostedCompilation{Bundle: bundle, JobGraphComplete: bundle.IR.JobGraphComplete, HasActions: hasActions}
	if err != nil {
		if compiler.ErrorHasUnscopedFailure(err) {
			compiled.Bundle = failedPartialBundle(bundle)
			return compiled, hostedError(hostedEvaluationFailure, err)
		}
		if !bundle.IR.JobGraphComplete || len(bundle.Plans) == 0 {
			return compiled, hostedError(hostedEvaluationFailure, err)
		}
		if !hasActions && bundleUsesActions(bundle) {
			compiled.Bundle = failedPartialBundle(bundle)
			return compiled, hostedError(hostedEvaluationFailure, errors.Join(err, errors.New("compilation introduced actions absent from the expanded workflow")))
		}
		if admissionErr := validateUnprivilegedBundle(bundle); admissionErr != nil {
			compiled.Bundle = failedPartialBundle(bundle)
			return compiled, hostedError(hostedEvaluationFailure, errors.Join(err, admissionErr))
		}
		generated, generationErr := compiler.GeneratePlannedWorkflow(bundle, options)
		if generationErr != nil {
			compiled.Bundle = failedPartialBundle(bundle)
			return compiled, hostedError(hostedEvaluationFailure, errors.Join(err, generationErr))
		}
		bundle.GeneratedWorkflow = generated
		compiled.Bundle = bundle
		compiled.Admitted = true
		return compiled, hostedError(hostedEvaluationFailure, err)
	}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		return compiled, hostedError(hostedAdmissionFailure, err)
	}
	bundle, err = compiler.GenerateBundlePipeline(bundle, request.DistributionDigest, request.ImporterStep, options)
	if err != nil {
		compiled.Bundle = bundle
		compiled.Admitted = true
		return compiled, hostedError(hostedEvaluationFailure, err)
	}
	compiled.Bundle = bundle
	compiled.Admitted = true
	return compiled, nil
}

// compiledJob identifies one compiled job instance: the step key the
// pipeline uses, the logical job it expands, the step keys it depends on,
// and the digest of its plan. Two compilations of the same request agree on
// every field; a compilation with different variables agrees on everything
// but the plan digest. The continuation artifact records the initial
// upload's instances in this form so the deferred upload can prove that its
// recompilation reproduced them.
type compiledJob struct {
	Key        string   `json:"key"`
	LogicalJob string   `json:"job"`
	Needs      []string `json:"needs,omitempty"`
	PlanDigest string   `json:"plan_digest"`
}

// equal reports whether two compilations produced the same instance: the
// same key, logical job, dependencies, and plan.
func (j compiledJob) equal(other compiledJob) bool {
	return j.Key == other.Key && j.LogicalJob == other.LogicalJob && j.PlanDigest == other.PlanDigest && slices.Equal(slices.Sorted(slices.Values(j.Needs)), slices.Sorted(slices.Values(other.Needs)))
}

// compiledJobs lists the bundle's job instances in expansion order. An
// instance without a plan, such as one whose job failed evaluation, has an
// empty PlanDigest.
func compiledJobs(bundle compiler.Bundle) []compiledJob {
	digests := make(map[string]string, len(bundle.Plans))
	for _, jobPlan := range bundle.Plans {
		digests[jobPlan.Job.Target.StepKey] = jobPlan.Digest
	}
	jobs := make([]compiledJob, 0, len(bundle.IR.Jobs))
	for _, instance := range bundle.IR.Jobs {
		jobs = append(jobs, compiledJob{
			Key:        instance.Key,
			LogicalJob: instance.LogicalJobID,
			Needs:      append([]string(nil), instance.Needs...),
			PlanDigest: digests[instance.Key],
		})
	}
	return jobs
}

// privateRepositorySourceOptions returns the source options that read remote
// reusable workflows through the agent's Git credentials when the plugin's
// private-reusable-workflows setting is on.
func privateRepositorySourceOptions(privateReusableWorkflows bool) ([]actionsource.Option, error) {
	if !privateReusableWorkflows {
		return nil, nil
	}
	git, err := exec.LookPath("git")
	if err == nil {
		git, err = filepath.Abs(git)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve Git executable: %w", err)
	}
	return []actionsource.Option{actionsource.WithGitRepositorySource(git)}, nil
}
