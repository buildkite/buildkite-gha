// Package compatibility renders stable human and machine-readable validation reports.
package compatibility

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ProcessingSchema is the versioned, stage-oriented report shared by all
// workflow-processing commands.
const ProcessingSchema = "buildkite-gha/processing-report/v1"

const (
	Passed       = "passed"
	Failed       = "failed"
	NotEvaluated = "not-evaluated"
)

// SourceLocation identifies input without retaining any source or event data.
type SourceLocation struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// Diagnostic is one actionable compatibility finding.
type Diagnostic struct {
	Level    string          `json:"level"`
	Code     string          `json:"code"`
	Category string          `json:"category,omitempty"`
	Stage    string          `json:"stage,omitempty"`
	Message  string          `json:"message"`
	Location *SourceLocation `json:"location,omitempty"`
	Job      string          `json:"job,omitempty"`
	Instance string          `json:"instance,omitempty"`
	Action   string          `json:"action,omitempty"`
	Step     int             `json:"step,omitempty"`
}

// ProcessingStage is one required workflow-processing boundary.
type ProcessingStage struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
}

// JobResult retains successfully discovered logical jobs and matrix instances.
type JobResult struct {
	ID       string          `json:"id"`
	Instance string          `json:"instance,omitempty"`
	Result   string          `json:"result"`
	Location *SourceLocation `json:"location,omitempty"`
}

// ActionResult retains action invocations without downloaded action contents.
type ActionResult struct {
	Reference string          `json:"reference"`
	Job       string          `json:"job"`
	Step      int             `json:"step"`
	Result    string          `json:"result"`
	Location  *SourceLocation `json:"location,omitempty"`
}

// ProcessingReport records all safely discoverable results. Artifacts are
// deliberately excluded so reports cannot expose event or downloaded content.
type ProcessingReport struct {
	Schema      string            `json:"schema"`
	Workflow    string            `json:"workflow"`
	Profile     string            `json:"profile,omitempty"`
	Result      string            `json:"result"`
	Status      string            `json:"status"`
	LogicalJobs int               `json:"logical_jobs"`
	Instances   int               `json:"instances"`
	Compile     Stage             `json:"compile,omitempty"`
	Admission   Stage             `json:"admission,omitempty"`
	Stages      []ProcessingStage `json:"stages"`
	Jobs        []JobResult       `json:"jobs"`
	Actions     []ActionResult    `json:"actions"`
	Diagnostics []Diagnostic      `json:"diagnostics"`
}

var processingStages = []ProcessingStage{
	{ID: "workflow-parsing", Name: "Workflow parsing"},
	{ID: "event-validation", Name: "Event validation"},
	{ID: "static-graph-construction", Name: "Static graph construction"},
	{ID: "matrix-expansion", Name: "Matrix expansion"},
	{ID: "expression-validation", Name: "Expression validation"},
	{ID: "action-discovery", Name: "Local and public action discovery"},
	{ID: "action-resolution", Name: "Immutable action resolution"},
	{ID: "job-plan-construction", Name: "Job-plan construction"},
	{ID: "hosted-profile-admission", Name: "Hosted-profile admission"},
	{ID: "pipeline-generation", Name: "Pipeline generation"},
}

// NewProcessingReport returns a deterministic report with every stage present.
func NewProcessingReport(workflow, profile string) ProcessingReport {
	stages := append([]ProcessingStage(nil), processingStages...)
	for i := range stages {
		stages[i].Result = NotEvaluated
	}
	return ProcessingReport{
		Schema: ProcessingSchema, Workflow: workflow, Profile: profile,
		Result: NotEvaluated, Status: NotEvaluated, Stages: stages, Jobs: []JobResult{},
		Actions: []ActionResult{}, Diagnostics: []Diagnostic{},
		Compile: Stage{Result: NotEvaluated}, Admission: Stage{Result: NotEvaluated},
	}
}

// SetStage records a stage outcome by stable ID.
func (r *ProcessingReport) SetStage(id, result string) {
	for i := range r.Stages {
		if r.Stages[i].ID == id {
			r.Stages[i].Result = result
			return
		}
	}
}

// Finalize sorts entity results and derives the overall result.
func (r *ProcessingReport) Finalize() {
	r.Status = NotEvaluated
	for _, stage := range r.Stages {
		if stage.Result == Failed {
			r.Status = Failed
			break
		}
		if stage.Result == Passed {
			r.Status = Passed
		}
	}
	for _, diagnostic := range r.Diagnostics {
		if diagnostic.Level == "error" {
			r.Status = Failed
			break
		}
	}
	if r.Result == NotEvaluated {
		r.Result = r.Status
	}
	sort.SliceStable(r.Jobs, func(i, j int) bool {
		if r.Jobs[i].ID != r.Jobs[j].ID {
			return r.Jobs[i].ID < r.Jobs[j].ID
		}
		return r.Jobs[i].Instance < r.Jobs[j].Instance
	})
	sort.SliceStable(r.Actions, func(i, j int) bool {
		if r.Actions[i].Job != r.Actions[j].Job {
			return r.Actions[i].Job < r.Actions[j].Job
		}
		if r.Actions[i].Step != r.Actions[j].Step {
			return r.Actions[i].Step < r.Actions[j].Step
		}
		return r.Actions[i].Reference < r.Actions[j].Reference
	})
	stageOrder := make(map[string]int, len(processingStages))
	for i, stage := range processingStages {
		stageOrder[stage.ID] = i
	}
	sort.SliceStable(r.Diagnostics, func(i, j int) bool {
		left, right := r.Diagnostics[i], r.Diagnostics[j]
		if stageOrder[left.Stage] != stageOrder[right.Stage] {
			return stageOrder[left.Stage] < stageOrder[right.Stage]
		}
		leftPath, rightPath, leftLine, rightLine, leftColumn, rightColumn := "", "", 0, 0, 0, 0
		if left.Location != nil {
			leftPath, leftLine, leftColumn = left.Location.Path, left.Location.Line, left.Location.Column
		}
		if right.Location != nil {
			rightPath, rightLine, rightColumn = right.Location.Path, right.Location.Line, right.Location.Column
		}
		if leftPath != rightPath {
			return leftPath < rightPath
		}
		if leftLine != rightLine {
			return leftLine < rightLine
		}
		if leftColumn != rightColumn {
			return leftColumn < rightColumn
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Message < right.Message
	})
	r.Diagnostics = compactDiagnostics(r.Diagnostics)
}

func compactDiagnostics(diagnostics []Diagnostic) []Diagnostic {
	if len(diagnostics) < 2 {
		return diagnostics
	}
	out := diagnostics[:1]
	for _, diagnostic := range diagnostics[1:] {
		previous := out[len(out)-1]
		if previous.Level == diagnostic.Level && previous.Code == diagnostic.Code && previous.Category == diagnostic.Category && previous.Stage == diagnostic.Stage && previous.Message == diagnostic.Message && previous.Job == diagnostic.Job && previous.Instance == diagnostic.Instance && previous.Action == diagnostic.Action && previous.Step == diagnostic.Step && sameLocation(previous.Location, diagnostic.Location) {
			continue
		}
		out = append(out, diagnostic)
	}
	return out
}

func sameLocation(left, right *SourceLocation) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// WriteProcessing renders the same fields in text or deterministic JSON.
func WriteProcessing(w io.Writer, format string, report ProcessingReport) error {
	report.Finalize()
	switch format {
	case "text":
		if _, err := fmt.Fprintf(w, "Schema: %s\nWorkflow: %s\nResult: %s\nStatus: %s\nLogical jobs: %d\nInstances: %d\nCompile: %s\nAdmission: %s\n", report.Schema, report.Workflow, report.Result, report.Status, report.LogicalJobs, report.Instances, report.Compile.Result, report.Admission.Result); err != nil {
			return err
		}
		if report.Profile != "" {
			if _, err := fmt.Fprintf(w, "Profile: %s\n", report.Profile); err != nil {
				return err
			}
		}
		if report.Compile.Result == "compilable" {
			if _, err := fmt.Fprintf(w, "✓ %d logical jobs and %d static instances compile\n", report.LogicalJobs, report.Instances); err != nil {
				return err
			}
		}
		for _, stage := range report.Stages {
			if _, err := fmt.Fprintf(w, "- %s: %s\n", stage.Name, stage.Result); err != nil {
				return err
			}
		}
		for _, job := range report.Jobs {
			name := job.ID
			if job.Instance != "" {
				name += "/" + job.Instance
			}
			if _, err := fmt.Fprintf(w, "  job %s: %s%s\n", name, job.Result, textLocation(job.Location)); err != nil {
				return err
			}
		}
		for _, action := range report.Actions {
			if _, err := fmt.Fprintf(w, "  action %s (job %s, step %d): %s%s\n", action.Reference, action.Job, action.Step, action.Result, textLocation(action.Location)); err != nil {
				return err
			}
		}
		for _, diagnostic := range report.Diagnostics {
			marker := "!"
			if diagnostic.Level == "error" {
				marker = "x"
			}
			if _, err := fmt.Fprintf(w, "%s [%s] %s%s%s\n", marker, diagnostic.Code, diagnostic.Message, textLocation(diagnostic.Location), textDiagnosticMetadata(diagnostic)); err != nil {
				return err
			}
		}
		return nil
	case "json":
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	default:
		return fmt.Errorf("unsupported processing report format %q", format)
	}
}

func textDiagnosticMetadata(diagnostic Diagnostic) string {
	var fields []string
	if diagnostic.Category != "" {
		fields = append(fields, "category="+diagnostic.Category)
	}
	if diagnostic.Stage != "" {
		fields = append(fields, "stage="+diagnostic.Stage)
	}
	if diagnostic.Job != "" {
		fields = append(fields, "job="+diagnostic.Job)
	}
	if diagnostic.Instance != "" {
		fields = append(fields, "instance="+diagnostic.Instance)
	}
	if diagnostic.Action != "" {
		fields = append(fields, "action="+diagnostic.Action)
	}
	if diagnostic.Step != 0 {
		fields = append(fields, fmt.Sprintf("step=%d", diagnostic.Step))
	}
	if len(fields) == 0 {
		return ""
	}
	return " {" + strings.Join(fields, ", ") + "}"
}

func textLocation(location *SourceLocation) string {
	if location == nil {
		return ""
	}
	return fmt.Sprintf(" (%s:%d:%d)", location.Path, location.Line, location.Column)
}

// Stage is one independently reported compatibility boundary.
type Stage struct {
	Result      string `json:"result"`
	LogicalJobs int    `json:"logical_jobs,omitempty"`
	Instances   int    `json:"instances,omitempty"`
}
