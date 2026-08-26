package runtime

import (
	"errors"
	"fmt"
	"os/exec"
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
)

// ClassifyFailure reports the most specific class found in a RunJob error
// chain. Unsupported features outrank integrity failures, which outrank step
// process exits, so a compatibility signal is never hidden by an ordinary
// workflow failure joined into the same error.
func ClassifyFailure(err error) FailureClass {
	var unsupported *unsupportedFeatureError
	if errors.As(err, &unsupported) {
		return FailureClassUnsupportedFeature
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
	err error
}

func (e *unsupportedFeatureError) Error() string { return e.err.Error() }
func (e *unsupportedFeatureError) Unwrap() error { return e.err }

// errUnsupportedf builds a rejection error for a feature outside the supported
// runtime subset.
func errUnsupportedf(format string, args ...any) error {
	return &unsupportedFeatureError{err: fmt.Errorf(format, args...)}
}
