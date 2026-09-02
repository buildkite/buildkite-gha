package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
)

// FailureClass attributes a RunJob error for telemetry so ordinary workflow
// failures are not counted as compatibility gaps.
type FailureClass string

const (
	// FailureClassUnknown covers errors the runtime cannot attribute.
	FailureClassUnknown FailureClass = "unknown"
	// FailureClassStepProcessExit means a workflow-authored process ran and
	// exited nonzero: the job failed, not the compatibility runtime.
	FailureClassStepProcessExit FailureClass = "step_process_exit"
	// FailureClassUnsupportedFeature means the supported runtime subset
	// rejected a feature the workflow asked for.
	FailureClassUnsupportedFeature FailureClass = "unsupported_feature"
	// FailureClassIntegrity means a runtime integrity or cleanup verification
	// failed.
	FailureClassIntegrity FailureClass = "integrity"
	// FailureClassWorkflowToken means the runtime could not acquire the job's
	// GitHub workflow token.
	FailureClassWorkflowToken FailureClass = "workflow_token"
	// FailureClassOIDCToken means an action could not acquire an OIDC token.
	FailureClassOIDCToken FailureClass = "oidc_token"
	// FailureClassCacheCredential means the runtime could not acquire an
	// action's cache credential.
	FailureClassCacheCredential FailureClass = "cache_credential"
)

// ClassifyFailure reports the most specific class found in a RunJob error
// chain. Unsupported features and setup failures outrank integrity failures
// and step process exits, so a specific runtime signal is never hidden by an
// ordinary workflow failure joined into the same error.
func ClassifyFailure(err error) FailureClass {
	var unsupported *unsupportedFeatureError
	if errors.As(err, &unsupported) {
		return FailureClassUnsupportedFeature
	}
	// metadata.Runtime() rejects unsupported runs.using values with its own
	// typed error; recognize it so those rejections classify without wrapping
	// at every call site.
	var unsupportedRuntime *metadata.UnsupportedRuntimeError
	if errors.As(err, &unsupportedRuntime) {
		return FailureClassUnsupportedFeature
	}
	var setup *jobSetupFailure
	if errors.As(err, &setup) {
		return setup.class
	}
	if isHardJobFailure(err) {
		return FailureClassIntegrity
	}
	var exit *stepProcessExitError
	if errors.As(err, &exit) {
		return FailureClassStepProcessExit
	}
	return FailureClassUnknown
}

type jobSetupFailure struct {
	class      FailureClass
	httpStatus int
	err        error
}

func (e *jobSetupFailure) Error() string { return e.err.Error() }
func (e *jobSetupFailure) Unwrap() error { return e.err }

func markJobSetupFailure(class FailureClass, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var marked *jobSetupFailure
	if errors.As(err, &marked) {
		return err
	}
	return &jobSetupFailure{class: class, err: err}
}

func newJobSetupHTTPFailure(class FailureClass, status int, message string) error {
	return &jobSetupFailure{class: class, httpStatus: status, err: errors.New(message)}
}

// AgentAPIHTTPStatus returns the upstream Agent API status retained by a
// classified runtime setup failure.
func AgentAPIHTTPStatus(err error) (int, bool) {
	var setup *jobSetupFailure
	if !errors.As(err, &setup) || setup.httpStatus == 0 {
		return 0, false
	}
	return setup.httpStatus, true
}

type stepProcessExitError struct {
	err error
}

func (e *stepProcessExitError) Error() string { return e.err.Error() }
func (e *stepProcessExitError) Unwrap() error { return e.err }

// markStepProcessExit marks an error whose chain shows a step payload process
// exiting nonzero. Errors without an exec.ExitError, such as cancellations and
// launch failures, pass through unchanged.
func markStepProcessExit(err error) error {
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	var marked *stepProcessExitError
	if errors.As(err, &marked) {
		return err
	}
	return &stepProcessExitError{err: err}
}

type unsupportedFeatureError struct {
	blocker string
	detail  string
	err     error
}

func (e *unsupportedFeatureError) Error() string { return e.err.Error() }
func (e *unsupportedFeatureError) Unwrap() error { return e.err }

// errUnsupportedf builds a rejection error for a feature outside the supported
// runtime subset.
func errUnsupportedf(format string, args ...any) error {
	return &unsupportedFeatureError{err: fmt.Errorf(format, args...)}
}

func errUnsupportedFeature(blocker, detail, format string, args ...any) error {
	return &unsupportedFeatureError{blocker: blocker, detail: detail, err: fmt.Errorf(format, args...)}
}

// UnsupportedFeature reports the structured feature rejected by a runtime
// error, when the rejection site can identify one safely.
func UnsupportedFeature(err error) (string, string, bool) {
	var unsupported *unsupportedFeatureError
	if errors.As(err, &unsupported) && unsupported.blocker != "" {
		return unsupported.blocker, unsupported.detail, true
	}
	return "", "", false
}
