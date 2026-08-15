package compatibility

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ProcessingSchemaV3 is the aggregate report emitted by multi-event
// validation. Each evaluation retains one complete v2 processing report.
const ProcessingSchemaV3 = "buildkite-gha/processing-report/v3"

// EventEvaluation records hosted validation for one generated event snapshot.
type EventEvaluation struct {
	Event  string           `json:"event"`
	Source string           `json:"event_source"`
	Report ProcessingReport `json:"report"`
}

// ProcessingReportV3 preserves event-independent validation separately from
// each event-specific hosted admission evaluation.
type ProcessingReportV3 struct {
	Schema      string            `json:"schema"`
	Workflow    string            `json:"workflow"`
	Profile     string            `json:"profile"`
	Result      string            `json:"result"`
	Status      string            `json:"status"`
	Validation  ProcessingReport  `json:"validation"`
	Evaluations []EventEvaluation `json:"evaluations"`
}

// NewProcessingReportV3 returns an aggregate report with no event evaluations.
func NewProcessingReportV3(workflow, profile string, validation ProcessingReport) ProcessingReportV3 {
	return ProcessingReportV3{
		Schema: ProcessingSchemaV3, Workflow: workflow, Profile: profile,
		Validation: validation, Evaluations: []EventEvaluation{},
	}
}

// Finalize derives the aggregate outcome without merging event-specific
// evidence. Admission means every generated event evaluation was admitted.
func (r *ProcessingReportV3) Finalize() {
	r.Validation.Finalize()
	r.Status = r.Validation.Status
	if r.Validation.Result != "compilable" {
		r.Result = r.Validation.Result
		return
	}
	if len(r.Evaluations) == 0 {
		r.Result = "not-applicable"
		return
	}
	r.Result = "admitted"
	for i := range r.Evaluations {
		r.Evaluations[i].Report.Finalize()
		if r.Evaluations[i].Report.Status == Failed {
			r.Status = Failed
		}
		switch r.Evaluations[i].Report.Result {
		case "indeterminate":
			r.Result = "indeterminate"
		case "incompatible":
			if r.Result != "indeterminate" {
				r.Result = "incompatible"
			}
		case "not-admitted":
			if r.Result == "admitted" {
				r.Result = "not-admitted"
			}
		case "admitted":
		default:
			if r.Result == "admitted" {
				r.Result = r.Evaluations[i].Report.Result
			}
		}
	}
}

// WriteProcessingV3 renders an aggregate report in text or deterministic JSON.
func WriteProcessingV3(w io.Writer, format string, report ProcessingReportV3) error {
	report.Finalize()
	switch format {
	case "text":
		if _, err := fmt.Fprintf(w, "Schema: %s\nWorkflow: %s\nProfile: %s\nResult: %s\nStatus: %s\n\nEvent-independent validation:\n", report.Schema, report.Workflow, report.Profile, report.Result, report.Status); err != nil {
			return err
		}
		if err := writeIndentedProcessing(w, report.Validation); err != nil {
			return err
		}
		for _, evaluation := range report.Evaluations {
			if _, err := fmt.Fprintf(w, "\nGenerated event %s:\n", evaluation.Event); err != nil {
				return err
			}
			if err := writeIndentedProcessing(w, evaluation.Report); err != nil {
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

func writeIndentedProcessing(w io.Writer, report ProcessingReport) error {
	var rendered bytes.Buffer
	if err := WriteProcessing(&rendered, "text", report); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSuffix(rendered.String(), "\n"), "\n") {
		if _, err := fmt.Fprintf(w, "  %s\n", line); err != nil {
			return err
		}
	}
	return nil
}
