// Package compatibility renders stable human and machine-readable validation reports.
package compatibility

import (
	"encoding/json"
	"fmt"
	"io"
)

const Schema = "buildkite-gha/compatibility-report/v1"

// Diagnostic is one actionable compatibility finding.
type Diagnostic struct {
	Level   string `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Report describes whether a workflow can enter the static compiler.
type Report struct {
	Schema      string       `json:"schema"`
	Workflow    string       `json:"workflow"`
	Result      string       `json:"result"`
	LogicalJobs int          `json:"logical_jobs"`
	Instances   int          `json:"instances"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Compilable constructs a successful compile-level report.
func Compilable(workflow string, logicalJobs, instances int) Report {
	return Report{
		Schema: Schema, Workflow: workflow, Result: "compilable",
		LogicalJobs: logicalJobs, Instances: instances, Diagnostics: []Diagnostic{},
	}
}

// Blocked constructs a failed compile-level report without exposing input data beyond the diagnostic.
func Blocked(workflow string, err error) Report {
	return Report{
		Schema: Schema, Workflow: workflow, Result: "incompatible", Diagnostics: []Diagnostic{{
			Level: "error", Code: "E_COMPILE", Message: err.Error(),
		}},
	}
}

// Write renders a report as text or deterministic indented JSON.
func Write(w io.Writer, format string, report Report) error {
	switch format {
	case "text":
		if _, err := fmt.Fprintf(w, "Workflow: %s\nResult: %s\n", report.Workflow, report.Result); err != nil {
			return err
		}
		if report.Result == "compilable" {
			_, err := fmt.Fprintf(w, "✓ %d logical jobs and %d static instances compile\n", report.LogicalJobs, report.Instances)
			return err
		}
		for _, diagnostic := range report.Diagnostics {
			if _, err := fmt.Fprintf(w, "✗ [%s] %s\n", diagnostic.Code, diagnostic.Message); err != nil {
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
		return fmt.Errorf("unsupported compatibility report format %q", format)
	}
}
