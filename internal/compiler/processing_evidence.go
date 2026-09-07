package compiler

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
