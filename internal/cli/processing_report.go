package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
)

const (
	stageWorkflowParsing = "workflow-parsing"
	stageEventValidation = "event-validation"
	stageGraph           = "static-graph-construction"
	stageMatrix          = "matrix-expansion"
	stageExpressions     = "expression-validation"
	stageDiscovery       = "action-discovery"
	stageResolution      = "action-resolution"
	stagePlans           = "job-plan-construction"
	stageAdmission       = "hosted-profile-admission"
	stagePipeline        = "pipeline-generation"
)

var locatedDiagnosticPattern = regexp.MustCompile(`^(.+):(\d+):(\d+): (.*)$`)

func initialProcessingReport(path, profile string, eventEvaluated bool, report compiler.Report, processingErr error) compatibility.ProcessingReport {
	out := compatibility.NewProcessingReport(path, profile)
	out.LogicalJobs = report.LogicalJobs
	out.Instances = report.Instances
	out.Compile = compatibility.Stage{Result: "compilable", LogicalJobs: report.LogicalJobs, Instances: report.Instances}
	out.Admission = compatibility.Stage{Result: compatibility.NotEvaluated}
	for _, job := range report.ParsedJobs {
		out.Jobs = append(out.Jobs, compatibility.JobResult{
			ID: job.ID, Result: compatibility.Passed,
			Location: sourceLocation(job.Path, job.Source.Start.Line, job.Source.Start.Column),
		})
	}
	for _, instance := range report.Jobs {
		out.Jobs = append(out.Jobs, compatibility.JobResult{
			ID: instance.LogicalJobID, Instance: instance.Key, Result: compatibility.Passed,
			Location: sourceLocation(instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column),
		})
		for i, step := range instance.Steps {
			if step.Uses == "" {
				continue
			}
			out.Actions = append(out.Actions, compatibility.ActionResult{
				Reference: step.Uses, Job: instance.Key, Step: i + 1,
				Result:   compatibility.NotEvaluated,
				Location: sourceLocation(instance.SourcePath, step.Span.Start.Line, step.Span.Start.Column),
			})
		}
	}

	if processingErr == nil {
		out.SetStage(stageWorkflowParsing, compatibility.Passed)
		if eventEvaluated {
			out.SetStage(stageEventValidation, compatibility.Passed)
		}
		out.SetStage(stageGraph, compatibility.Passed)
		out.SetStage(stageMatrix, compatibility.Passed)
		out.SetStage(stageExpressions, compatibility.Passed)
		out.SetStage(stageDiscovery, compatibility.Passed)
		if len(out.Actions) == 0 {
			out.SetStage(stageResolution, compatibility.Passed)
		}
	} else {
		out.Compile.Result = "incompatible"
		failed := map[string]bool{}
		for _, err := range flattenErrors(processingErr) {
			stage, code, category := classifyProcessingError(err)
			if len(report.ParsedJobs) == 0 && stage != stageEventValidation {
				stage, category = stageWorkflowParsing, "syntax"
			}
			failed[stage] = true
			out.Diagnostics = append(out.Diagnostics, diagnosticFromError(path, stage, code, category, err))
		}
		markFailedJobs(&out)
		setInitialStageResults(&out, eventEvaluated, failed)
	}
	for _, warning := range report.Warnings {
		out.Diagnostics = append(out.Diagnostics, compatibility.Diagnostic{
			Level: "warning", Code: warning.Code, Category: "compatibility", Stage: stageExpressions,
			Message: fmt.Sprintf("%s:%d:%d: %s", path, warning.Line, warning.Column, warning.Message), Location: sourceLocation(path, warning.Line, warning.Column),
		})
	}
	return out
}

func setInitialStageResults(report *compatibility.ProcessingReport, eventEvaluated bool, failed map[string]bool) {
	order := []string{stageWorkflowParsing, stageEventValidation, stageGraph, stageMatrix, stageExpressions, stageDiscovery}
	blocked := false
	for _, stage := range order {
		if stage == stageEventValidation && !eventEvaluated {
			continue
		}
		if failed[stage] {
			report.SetStage(stage, compatibility.Failed)
			blocked = true
			continue
		}
		if !blocked {
			report.SetStage(stage, compatibility.Passed)
		}
	}
}

func addProcessingFailure(report *compatibility.ProcessingReport, path, stage, code, category string, err error) {
	report.SetStage(stage, compatibility.Failed)
	if stage == stageResolution && category == "action-resolution" {
		markActions(report, compatibility.Passed)
	}
	for _, item := range flattenErrors(err) {
		diagnostic := diagnosticFromError(path, stage, code, category, item)
		report.Diagnostics = append(report.Diagnostics, diagnostic)
	}
	markFailedJobs(report)
	for i := range report.Actions {
		if stage != stageResolution {
			continue
		}
		for _, diagnostic := range report.Diagnostics {
			jobMatches := diagnostic.Job == "" || actionJobMatches(*report, report.Actions[i].Job, diagnostic.Job)
			if jobMatches && ((diagnostic.Action != "" && diagnostic.Action == report.Actions[i].Reference) || (diagnostic.Action == "" && strings.Contains(diagnostic.Message, report.Actions[i].Reference))) {
				report.Actions[i].Result = compatibility.Failed
			}
		}
	}
}

func markFailedJobs(report *compatibility.ProcessingReport) {
	failed := map[string]bool{}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Level == "error" && diagnostic.Job != "" {
			failed[diagnostic.Job] = true
		}
	}
	for i := range report.Jobs {
		if report.Jobs[i].Instance == "" && failed[report.Jobs[i].ID] {
			report.Jobs[i].Result = compatibility.Failed
		}
	}
}

func actionJobMatches(report compatibility.ProcessingReport, instance, logical string) bool {
	for _, job := range report.Jobs {
		if job.Instance == instance {
			return job.ID == logical
		}
	}
	return false
}

func markActions(report *compatibility.ProcessingReport, result string) {
	for i := range report.Actions {
		report.Actions[i].Result = result
	}
}

func allLocalActions(actions []compatibility.ActionResult) bool {
	for _, action := range actions {
		if !strings.HasPrefix(action.Reference, "./") {
			return false
		}
	}
	return true
}

func markLaterPrerequisites(report *compatibility.ProcessingReport, failedStage string, hosted bool) {
	if failedStage == stagePlans || failedStage == stageAdmission || failedStage == stagePipeline {
		report.SetStage(stageResolution, compatibility.Passed)
		markActions(report, compatibility.Passed)
	}
	if failedStage == stageAdmission || failedStage == stagePipeline {
		report.SetStage(stagePlans, compatibility.Passed)
	}
	if hosted && failedStage == stagePipeline {
		report.SetStage(stageAdmission, compatibility.Passed)
		report.Admission.Result = "admitted"
	}
}

func laterProcessingFailure(err error) (stage, code, category string) {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "runner label"), strings.Contains(message, "untrusted event cannot"), strings.Contains(message, "runs-on"), strings.Contains(message, "condition"), strings.Contains(message, "expression"):
		return stageExpressions, "E_COMPILE", "compatibility"
	case strings.Contains(message, "emit buildkite pipeline"), strings.Contains(message, "pipeline requires"), strings.Contains(message, "invalid compiler step"):
		return stagePipeline, "E_PIPELINE_GENERATION", "compatibility"
	case strings.Contains(message, "action"), strings.Contains(message, "source store"), strings.Contains(message, "workflow path must identify a repository root"):
		return stageResolution, "E_ACTION_RESOLUTION", "action-resolution"
	default:
		return stagePlans, "E_PLAN_CONSTRUCTION", "compatibility"
	}
}

func flattenErrors(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, child := range joined.Unwrap() {
			out = append(out, flattenErrors(child)...)
		}
		return out
	}
	return []error{err}
}

func classifyProcessingError(err error) (stage, code, category string) {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "parse workflow yaml"), strings.Contains(message, "actionlint"), strings.Contains(message, "could not parse as yaml"), strings.Contains(message, "yaml syntax"):
		return stageWorkflowParsing, "E_COMPILE", "syntax"
	case strings.Contains(message, "event snapshot"):
		return stageEventValidation, "E_COMPILE", "environment"
	case strings.Contains(message, "matrix"):
		return stageMatrix, "E_COMPILE", "compatibility"
	case strings.Contains(message, "condition"), strings.Contains(message, "expression"), strings.Contains(message, "concurrency group"), strings.Contains(message, "runs-on"):
		return stageExpressions, "E_COMPILE", "compatibility"
	case strings.Contains(message, "needs unknown"), strings.Contains(message, "job graph"), strings.Contains(message, "reusable workflow"), strings.Contains(message, "flattened job"):
		return stageGraph, "E_COMPILE", "compatibility"
	default:
		return stageGraph, "E_COMPILE", "compatibility"
	}
}

func diagnosticFromError(defaultPath, stage, code, category string, err error) compatibility.Diagnostic {
	message := err.Error()
	location := (*compatibility.SourceLocation)(nil)
	if match := locatedDiagnosticPattern.FindStringSubmatch(message); match != nil {
		line, lineErr := strconv.Atoi(match[2])
		column, columnErr := strconv.Atoi(match[3])
		if lineErr == nil && columnErr == nil {
			location = sourceLocation(match[1], line, column)
			message = match[4]
		}
	}
	diagnostic := compatibility.Diagnostic{
		Level: "error", Code: code, Category: category, Stage: stage,
		Message: message, Location: location,
	}
	if diagnostic.Location == nil && defaultPath != "" && stage != stageEventValidation {
		diagnostic.Location = sourceLocation(defaultPath, 1, 1)
	}
	if start := strings.Index(message, `job "`); start >= 0 {
		rest := message[start+len(`job "`):]
		if end := strings.Index(rest, `"`); end >= 0 {
			diagnostic.Job = rest[:end]
		}
	}
	if start := strings.Index(message, `action "`); start >= 0 {
		rest := message[start+len(`action "`):]
		if end := strings.Index(rest, `"`); end >= 0 {
			diagnostic.Action = rest[:end]
		}
	}
	return diagnostic
}

func sourceLocation(path string, line, column int) *compatibility.SourceLocation {
	if line <= 0 {
		line = 1
	}
	if column <= 0 {
		column = 1
	}
	return &compatibility.SourceLocation{Path: path, Line: line, Column: column}
}
