package compiler

import (
	"errors"

	"github.com/buildkite/buildkite-gha/internal/processing"
)

// ProcessingStage identifies one stable workflow-processing boundary.
type ProcessingStage = processing.Stage

const (
	StageWorkflowParsing = processing.StageWorkflowParsing
	StageEventValidation = processing.StageEventValidation
	StageGraph           = processing.StageGraph
	StageMatrix          = processing.StageMatrix
	StageExpressions     = processing.StageExpressions
	StageDiscovery       = processing.StageDiscovery
	StageResolution      = processing.StageResolution
	StagePlans           = processing.StagePlans
	StageAdmission       = processing.StageAdmission
	StagePipeline        = processing.StagePipeline
)

const (
	CodeWorkflowSyntax     = processing.CodeWorkflowSyntax
	CodeEventInvalid       = processing.CodeEventInvalid
	CodeGraphInvalid       = processing.CodeGraphInvalid
	CodeMatrixInvalid      = processing.CodeMatrixInvalid
	CodeExpressionInvalid  = processing.CodeExpressionInvalid
	CodeActionDiscovery    = processing.CodeActionDiscovery
	CodeActionResolution   = processing.CodeActionResolution
	CodePlanConstruction   = processing.CodePlanConstruction
	CodePipelineGeneration = processing.CodePipelineGeneration
	CodeContextRequired    = processing.CodeContextRequired
	CodeEnvironment        = processing.CodeEnvironment
)

// ProcessingFinding carries stable attribution independently of its rendered
// error text. Err remains wrapped so errors.Is and errors.As keep working.
type ProcessingFinding struct {
	Stage    ProcessingStage
	Code     string
	Category string
	Blocker  string
	// BlockerDetail is opt-in telemetry attribution. Set it only from original
	// workflow syntax or values proven to depend solely on workflow literals.
	BlockerDetail string
	Path          string
	Line          int
	Column        int
	Job           string
	Instance      string
	Action        string
	Step          int
	// Message replaces Err's text in the rendered report. Set it whenever Err
	// can quote event-derived data, so that data cannot reach the report.
	Message string
	// Detail adds lower-level diagnostic information after Message. It must not
	// contain event-derived data.
	Detail string
	Err    error
}

func (e *ProcessingFinding) Error() string { return e.Err.Error() }
func (e *ProcessingFinding) Unwrap() error { return e.Err }

type blockerDetailSuppressedError struct {
	blocker string
	err     error
}

func (e *blockerDetailSuppressedError) Error() string { return e.err.Error() }
func (e *blockerDetailSuppressedError) Unwrap() error { return e.err }
func (e *blockerDetailSuppressedError) CompatibilityBlocker() (string, string) {
	return e.blocker, ""
}

func suppressBlockerDetail(err error) error {
	if err == nil {
		return nil
	}
	if finding, ok := err.(*ProcessingFinding); ok {
		copy := *finding
		copy.BlockerDetail = ""
		if copy.Blocker == "" {
			var blocker interface {
				CompatibilityBlocker() (string, string)
			}
			if errors.As(copy.Err, &blocker) {
				copy.Blocker, _ = blocker.CompatibilityBlocker()
			}
		}
		return &copy
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		wrapped := make([]error, 0, len(children))
		for _, child := range children {
			wrapped = append(wrapped, suppressBlockerDetail(child))
		}
		return errors.Join(wrapped...)
	}
	var blocker interface {
		CompatibilityBlocker() (string, string)
	}
	if !errors.As(err, &blocker) {
		return err
	}
	kind, _ := blocker.CompatibilityBlocker()
	return &blockerDetailSuppressedError{blocker: kind, err: err}
}

func processingFinding(stage ProcessingStage, code, category string, err error) error {
	return attributedProcessingFinding(stage, code, category, "", 0, 0, "", "", "", 0, err)
}

func attributedProcessingFinding(stage ProcessingStage, code, category, path string, line, column int, job, instance, action string, step int, err error) error {
	if err == nil {
		return nil
	}
	if finding, ok := err.(*ProcessingFinding); ok {
		if finding.Path == "" {
			finding.Path, finding.Line, finding.Column = path, line, column
		}
		if finding.Job == "" {
			finding.Job = job
		}
		if finding.Instance == "" {
			finding.Instance = instance
		}
		if finding.Action == "" {
			finding.Action = action
		}
		if finding.Step == 0 {
			finding.Step = step
		}
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		wrapped := make([]error, 0, len(children))
		for _, child := range children {
			wrapped = append(wrapped, attributedProcessingFinding(stage, code, category, path, line, column, job, instance, action, step, child))
		}
		return errors.Join(wrapped...)
	}
	return &ProcessingFinding{
		Stage: stage, Code: code, Category: category,
		Path: path, Line: line, Column: column, Job: job, Instance: instance, Action: action, Step: step,
		Err: err,
	}
}

// ActionEvaluation records an attempted immutable resolution by invocation.
// Skipped invocations are absent and remain not-evaluated in the report.
type ActionEvaluation struct {
	Instance  string
	Job       string
	Reference string
	Step      int
	Passed    bool
}

// JobEvaluation records whether plan construction ran for one instance.
type JobEvaluation struct {
	Instance  string
	Job       string
	Evaluated bool
	Passed    bool
}

// ProcessingEvidence records facts learned before bundle construction stops.
// It is returned on both success and failure.
type ProcessingEvidence struct {
	ActionResolutionComplete bool
	Actions                  []ActionEvaluation
	Plans                    []JobEvaluation
	PlansConstructed         bool
	PipelineGenerated        bool
}
