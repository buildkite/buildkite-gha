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
	// FailureClassWorkflowValidation means all errors reject workflow-authored
	// values or file commands, rather than indicating a product defect.
	FailureClassWorkflowValidation FailureClass = "workflow_validation"
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
// chain. Unsupported features and integrity failures outrank setup failures,
// which in turn outrank ordinary step process exits.
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
	if isHardJobFailure(err) {
		return FailureClassIntegrity
	}
	var setup *jobSetupFailure
	if errors.As(err, &setup) {
		return setup.class
	}
	var exit *stepProcessExitError
	if errors.As(err, &exit) {
		return FailureClassStepProcessExit
	}
	if isWorkflowValidationFailure(err) {
		return FailureClassWorkflowValidation
	}
	return FailureClassUnknown
}

// UnexpectedFailure removes expected workflow failures from an error tree.
// Unlike completion classification, an unsupported or invalid workflow branch
// must not hide an independent cleanup, credential, or internal failure.
func UnexpectedFailure(err error) error {
	return unexpectedFailure(err, false, false)
}

func unexpectedFailure(err error, workflowTimeout, stepExit bool) error {
	switch err := err.(type) {
	case nil:
		return nil
	case *hardJobFailure, *jobSetupFailure, *containerTerminationError:
		return err
	case *unsupportedFeatureError, *metadata.UnsupportedRuntimeError, *workflowValidationError:
		return nil
	case *toleratedJobFailure:
		return unexpectedFailure(err.err, workflowTimeout, stepExit)
	case *stepProcessExitError:
		return unexpectedFailure(err.err, workflowTimeout, true)
	case *workflowTimeoutError:
		if err.processExit {
			return nil
		}
		return unexpectedFailure(err.err, true, stepExit)
	case *exec.ExitError:
		if stepExit {
			return nil
		}
		return err
	case interface {
		error
		Unwrap() []error
	}:
		var unexpected []error
		changed := false
		for _, child := range err.Unwrap() {
			selected := unexpectedFailure(child, workflowTimeout, stepExit)
			changed = changed || selected != child
			if selected != nil {
				unexpected = append(unexpected, selected)
			}
		}
		if !changed {
			return err
		}
		return errors.Join(unexpected...)
	case interface {
		error
		Unwrap() error
	}:
		child := err.Unwrap()
		selected := unexpectedFailure(child, workflowTimeout, stepExit)
		if selected == child {
			return err
		}
		return selected
	default:
		if err == context.Canceled || (workflowTimeout && err == context.DeadlineExceeded) {
			return nil
		}
		return err
	}
}

var errWorkflowDeadline = fmt.Errorf("workflow timeout: %w", context.DeadlineExceeded)

type workflowTimeoutError struct {
	err         error
	processExit bool
}

func (e *workflowTimeoutError) Error() string { return e.err.Error() }
func (e *workflowTimeoutError) Unwrap() error { return e.err }

// Mark only deadlines owned by workflow timeout-minutes. HTTP client and
// detached cleanup deadlines remain unexpected, even when joined here.
func markWorkflowTimeout(ctx context.Context, err error) error {
	if err != nil && ctx.Err() == context.DeadlineExceeded && context.Cause(ctx) == errWorkflowDeadline {
		return &workflowTimeoutError{err: err}
	}
	return err
}

// MarkWorkflowProcessTimeout attributes a CommandContext result to an owned
// workflow deadline. Call it on the subprocess result before wrapping or
// joining cleanup errors; it preserves the original error and classification.
func MarkWorkflowProcessTimeout(ctx context.Context, err error) error {
	marked := markWorkflowTimeout(ctx, err)
	if timeout, ok := marked.(*workflowTimeoutError); ok {
		var exit *exec.ExitError
		timeout.processExit = errors.As(err, &exit)
	}
	return marked
}

type containerTerminationError struct{ err error }

func (e *containerTerminationError) Error() string { return e.err.Error() }
func (e *containerTerminationError) Unwrap() error { return e.err }

type workflowValidationError struct {
	err error
}

func (e *workflowValidationError) Error() string { return e.err.Error() }
func (e *workflowValidationError) Unwrap() error { return e.err }

func errWorkflowValidationf(format string, args ...any) error {
	return &workflowValidationError{err: fmt.Errorf(format, args...)}
}

func isWorkflowValidationFailure(err error) bool {
	switch err := err.(type) {
	case *workflowValidationError:
		return true
	case interface{ Unwrap() []error }:
		children := err.Unwrap()
		for _, child := range children {
			if !isWorkflowValidationFailure(child) {
				return false
			}
		}
		return len(children) != 0
	case interface{ Unwrap() error }:
		return isWorkflowValidationFailure(err.Unwrap())
	default:
		return false
	}
}

// credentialRequestError reports a credential request that the Agent API
// boundary sent and could not complete. status is the Agent API response
// status, or zero when the request failed without a non-success response.
type credentialRequestError struct {
	status int
	err    error
}

func (e *credentialRequestError) Error() string { return e.err.Error() }
func (e *credentialRequestError) Unwrap() error { return e.err }

func credentialStatusError(status int, format string, args ...any) error {
	return &credentialRequestError{status: status, err: fmt.Errorf(format, args...)}
}

// credentialRequestFailure marks err as a failed credential request unless the
// caller cancelled it. HTTP client timeouts also match DeadlineExceeded, but
// are request failures unless ctx itself has expired.
func credentialRequestFailure(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
		return err
	}
	var requestErr *credentialRequestError
	if errors.As(err, &requestErr) {
		return err
	}
	return &credentialRequestError{err: err}
}

// jobSetupFailure attributes a failed credential request to the job setup
// step that needed it.
type jobSetupFailure struct {
	class FailureClass
	err   error
}

func (e *jobSetupFailure) Error() string { return e.err.Error() }
func (e *jobSetupFailure) Unwrap() error { return e.err }

// markJobSetupFailure classifies failed credential requests. Local validation
// errors and cancellations pass through unclassified.
func markJobSetupFailure(class FailureClass, err error) error {
	var requestErr *credentialRequestError
	if !errors.As(err, &requestErr) {
		return err
	}
	var marked *jobSetupFailure
	if errors.As(err, &marked) {
		return err
	}
	return &jobSetupFailure{class: class, err: err}
}

// AgentAPIHTTPStatus returns the Agent API status of a failed credential
// request.
func AgentAPIHTTPStatus(err error) (int, bool) {
	var requestErr *credentialRequestError
	if !errors.As(err, &requestErr) || requestErr.status == 0 {
		return 0, false
	}
	return requestErr.status, true
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
