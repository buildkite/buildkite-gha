package compiler

// ActionEvaluation records an attempted immutable resolution by invocation.
// Skipped invocations are absent and remain not-evaluated in the report.
type ActionEvaluation struct {
	Instance  string
	Job       string
	Reference string
	Step      int
	Passed    bool
	// CacheSubstitutions lists actions/cache references reachable from this
	// step whose resolved commit was replaced by an audited release.
	CacheSubstitutions []CacheSubstitution
}

// CacheSubstitution records one actions/cache reference that resolved to a
// commit outside the frozen cache-v2 snapshot and runs an audited release
// instead.
type CacheSubstitution struct {
	// Reference is the requested action, such as actions/cache@v6.
	Reference string
	// ResolvedCommit is the commit the requested ref resolved to.
	ResolvedCommit string
	// Commit and Release identify the audited substitute that runs.
	Commit  string
	Release string
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
