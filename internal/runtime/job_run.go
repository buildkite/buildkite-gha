package runtime

import (
	"context"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// jobRun owns all mutable state and resources for one job execution. Runner is
// embedded as a private configuration copy so per-job state cannot leak when
// callers reuse the public facade.
type jobRun struct {
	Runner

	job             plan.Job
	workspace       string
	callerWorkspace bool
	processor       *commandProcessor
	eval            expression.Context
	result          JobResult
	runtimeEnv      map[string]string
	actions         *actionLockResolver
	posts           *postRegistry
	supervisor      *backgroundSupervisor
	prepared        remotePreparations
	preFailures     map[int]stepExecution
	runErr          error
	hardFailure     bool

	runnerTemp       string
	implicitJobPATH  string
	explicitJobPATH  bool
	jobContainer     *jobContainerBackend
	jobDocker        *jobContainerBackend
	prebuiltDocker   *prebuiltDockerBackend
	nodeVerification *managedNodeVerification
	artifactRegistry *artifactRegistry
	node16Warnings   *node16DeprecationWarnings
	idTokenService   *idTokenService
}

// RunJob executes the plan's ordered steps and always drains registered post actions.
func (r Runner) RunJob(ctx context.Context, job plan.Job, workspace string) (JobResult, error) {
	run := newJobRun(r)
	run.job = job
	run.workspace = workspace
	run.callerWorkspace = workspace != ""
	return run.run(ctx)
}

func newJobRun(r Runner) *jobRun {
	return &jobRun{
		Runner:           r,
		nodeVerification: &managedNodeVerification{paths: make(map[int]string, 2)},
		artifactRegistry: &artifactRegistry{names: make(map[string]bool)},
		node16Warnings:   &node16DeprecationWarnings{},
	}
}

func (r *jobRun) run(ctx context.Context) (final JobResult, runJobErr error) {
	defer func() {
		if final.Conclusion == "success" && runJobErr != nil && !IsToleratedJobFailure(runJobErr) {
			final.Conclusion = "failure"
		}
	}()
	// prepare chains the remaining phases before it returns so its resource
	// cleanup stays deferred until post actions and finalization complete.
	return r.prepare(ctx)
}
