package compiler

import "errors"

// ProcessingStage identifies one stable workflow-processing boundary without
// coupling the compiler to the compatibility report wire format.
type ProcessingStage string

const (
	StageWorkflowParsing ProcessingStage = "workflow-parsing"
	StageEventValidation ProcessingStage = "event-validation"
	StageGraph           ProcessingStage = "static-graph-construction"
	StageMatrix          ProcessingStage = "matrix-expansion"
	StageExpressions     ProcessingStage = "expression-validation"
	StageDiscovery       ProcessingStage = "action-discovery"
	StageResolution      ProcessingStage = "action-resolution"
	StagePlans           ProcessingStage = "job-plan-construction"
	StageAdmission       ProcessingStage = "hosted-profile-admission"
	StagePipeline        ProcessingStage = "pipeline-generation"
)

const (
	CodeWorkflowSyntax     = "E_WORKFLOW_SYNTAX"
	CodeEventInvalid       = "E_EVENT_INVALID"
	CodeGraphInvalid       = "E_GRAPH_INVALID"
	CodeMatrixInvalid      = "E_MATRIX_INVALID"
	CodeExpressionInvalid  = "E_EXPRESSION_INVALID"
	CodeActionDiscovery    = "E_ACTION_DISCOVERY"
	CodeActionResolution   = "E_ACTION_RESOLUTION"
	CodePlanConstruction   = "E_PLAN_CONSTRUCTION"
	CodePipelineGeneration = "E_PIPELINE_GENERATION"
	CodeEnvironment        = "E_ENVIRONMENT"
)

// ProcessingFinding carries stable attribution independently of its rendered
// error text. Err remains wrapped so errors.Is and errors.As keep working.
type ProcessingFinding struct {
	Stage    ProcessingStage
	Code     string
	Category string
	Path     string
	Line     int
	Column   int
	Job      string
	Instance string
	Action   string
	Step     int
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
