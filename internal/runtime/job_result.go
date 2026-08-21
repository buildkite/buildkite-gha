package runtime

import (
	"context"
	"errors"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// JobResult is the bounded logical result returned to the transport layer.
type JobResult struct {
	Conclusion         string                     `json:"conclusion"`
	Outputs            map[string]string          `json:"outputs,omitempty"`
	Env                map[string]string          `json:"env,omitempty"`
	State              map[string]string          `json:"state,omitempty"`
	Summary            string                     `json:"summary,omitempty"`
	WarningAnnotations string                     `json:"warning_annotations,omitempty"`
	ErrorAnnotations   string                     `json:"error_annotations,omitempty"`
	Artifacts          []transport.ResultArtifact `json:"artifacts,omitempty"`

	summaryTruncated  bool
	warningsTruncated bool
	errorsTruncated   bool
	failureVisible    bool
}

// FailureVisible reports whether the runtime expanded a section containing the failure.
func (r JobResult) FailureVisible() bool { return r.failureVisible }

const maxJobOutputBytes = 1024

type toleratedJobFailure struct {
	err error
}

func (e *toleratedJobFailure) Error() string { return e.err.Error() }
func (e *toleratedJobFailure) Unwrap() error { return e.err }

type hardJobFailure struct {
	err error
}

func (e *hardJobFailure) Error() string { return e.err.Error() }
func (e *hardJobFailure) Unwrap() error { return e.err }

type workflowJobFailure struct {
	err error
}

func (e *workflowJobFailure) Error() string { return e.err.Error() }
func (e *workflowJobFailure) Unwrap() error { return e.err }

func markHardJobFailure(err error) error {
	if err == nil || isHardJobFailure(err) {
		return err
	}
	return &hardJobFailure{err: err}
}

func isHardJobFailure(err error) bool {
	var target *hardJobFailure
	return errors.As(err, &target)
}

func markWorkflowJobFailure(err error) error {
	if err == nil {
		return nil
	}
	return &workflowJobFailure{err: err}
}

func isWorkflowJobFailure(err error) bool {
	var target *workflowJobFailure
	return errors.As(err, &target)
}

// IsToleratedJobFailure reports whether err contains only a workflow failure
// admitted by the job's continue-on-error setting. Joined cleanup, integrity,
// transport, and publication errors deliberately return false.
func IsToleratedJobFailure(err error) bool {
	_, ok := err.(*toleratedJobFailure)
	return ok
}

func tolerateJobSetupFailure(runCtx context.Context, job plan.Job, result JobResult, err error) (JobResult, error) {
	if runCtx.Err() != nil {
		result.Conclusion = "cancelled"
		return result, errors.Join(err, runCtx.Err())
	}
	if job.ContinueOnError && isWorkflowJobFailure(err) && !isHardJobFailure(err) {
		result.Conclusion = "success"
		return result, &toleratedJobFailure{err: err}
	}
	return result, err
}
