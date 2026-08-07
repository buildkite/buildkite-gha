// Package compatibility renders stable human and machine-readable validation reports.
package compatibility

import (
	"encoding/json"
	"fmt"
	"io"
)

const Schema = "buildkite-gha/compatibility-report/v1"

// ProfileSchema distinguishes static compilation from profile admission.
const ProfileSchema = "buildkite-gha/compatibility-report/v2"

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

// Stage is one independently reported compatibility boundary.
type Stage struct {
	Result      string `json:"result"`
	LogicalJobs int    `json:"logical_jobs,omitempty"`
	Instances   int    `json:"instances,omitempty"`
}

// ProfileReport describes static compilation and admission under a named
// execution profile. Admission proves plan construction and policy only; it is
// not a claim that arbitrary action code will execute successfully.
type ProfileReport struct {
	Schema      string       `json:"schema"`
	Workflow    string       `json:"workflow"`
	Profile     string       `json:"profile"`
	Result      string       `json:"result"`
	Compile     Stage        `json:"compile"`
	Admission   Stage        `json:"admission"`
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

// Admitted constructs a report for plans accepted by the named profile.
func Admitted(workflow, profile string, logicalJobs, instances int, actionRuntimeUnknown bool) ProfileReport {
	diagnostics := []Diagnostic{}
	if actionRuntimeUnknown {
		diagnostics = append(diagnostics, Diagnostic{
			Level: "warning",
			Code:  "W_ACTION_RUNTIME_UNKNOWN",
			Message: "resolved action metadata cannot prove that arbitrary action code " +
				"is independent of GitHub-only runtime services",
		})
	}
	return ProfileReport{
		Schema: ProfileSchema, Workflow: workflow, Profile: profile, Result: "admitted",
		Compile:   Stage{Result: "compilable", LogicalJobs: logicalJobs, Instances: instances},
		Admission: Stage{Result: "admitted"}, Diagnostics: diagnostics,
	}
}

// ProfileBlocked constructs a report for a workflow that compiles but whose
// plans cannot be admitted by the named profile.
func ProfileBlocked(workflow, profile string, logicalJobs, instances int, err error) ProfileReport {
	return ProfileReport{
		Schema: ProfileSchema, Workflow: workflow, Profile: profile, Result: "not-admitted",
		Compile:   Stage{Result: "compilable", LogicalJobs: logicalJobs, Instances: instances},
		Admission: Stage{Result: "not-admitted"}, Diagnostics: []Diagnostic{{
			Level: "error", Code: "E_PROFILE", Message: err.Error(),
		}},
	}
}

// ProfileNotEvaluated constructs a report when the workflow compiles but the
// local environment cannot complete profile evaluation.
func ProfileNotEvaluated(workflow, profile string, logicalJobs, instances int, code string, err error) ProfileReport {
	return ProfileReport{
		Schema: ProfileSchema, Workflow: workflow, Profile: profile, Result: "indeterminate",
		Compile:   Stage{Result: "compilable", LogicalJobs: logicalJobs, Instances: instances},
		Admission: Stage{Result: "not-evaluated"}, Diagnostics: []Diagnostic{{
			Level: "error", Code: code, Message: err.Error(),
		}},
	}
}

// ProfileCompileBlocked constructs a report when static compilation prevents
// the named profile from being evaluated.
func ProfileCompileBlocked(workflow, profile string, err error) ProfileReport {
	return ProfileReport{
		Schema: ProfileSchema, Workflow: workflow, Profile: profile, Result: "incompatible",
		Compile: Stage{Result: "incompatible"}, Admission: Stage{Result: "not-evaluated"},
		Diagnostics: []Diagnostic{{Level: "error", Code: "E_COMPILE", Message: err.Error()}},
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
			if _, err := fmt.Fprintf(w, "✓ %d logical jobs and %d static instances compile\n", report.LogicalJobs, report.Instances); err != nil {
				return err
			}
		}
		for _, diagnostic := range report.Diagnostics {
			marker := "✗"
			if diagnostic.Level == "warning" {
				marker = "!"
			}
			if _, err := fmt.Fprintf(w, "%s [%s] %s\n", marker, diagnostic.Code, diagnostic.Message); err != nil {
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

// WriteProfile renders a staged profile report as text or deterministic JSON.
func WriteProfile(w io.Writer, format string, report ProfileReport) error {
	switch format {
	case "text":
		if _, err := fmt.Fprintf(w, "Workflow: %s\nProfile: %s\nResult: %s\nCompile: %s\n", report.Workflow, report.Profile, report.Result, report.Compile.Result); err != nil {
			return err
		}
		if report.Compile.Result == "compilable" {
			if _, err := fmt.Fprintf(w, "✓ %d logical jobs and %d static instances compile\n", report.Compile.LogicalJobs, report.Compile.Instances); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "Admission: %s\n", report.Admission.Result); err != nil {
			return err
		}
		if report.Admission.Result == "admitted" {
			if _, err := fmt.Fprintln(w, "✓ plans resolve and satisfy the profile's upload policy"); err != nil {
				return err
			}
		}
		for _, diagnostic := range report.Diagnostics {
			marker := "✗"
			if diagnostic.Level == "warning" {
				marker = "!"
			}
			if _, err := fmt.Fprintf(w, "%s [%s] %s\n", marker, diagnostic.Code, diagnostic.Message); err != nil {
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
