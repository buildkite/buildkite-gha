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
	stageWorkflowParsing = string(compiler.StageWorkflowParsing)
	stageEventValidation = string(compiler.StageEventValidation)
	stageGraph           = string(compiler.StageGraph)
	stageMatrix          = string(compiler.StageMatrix)
	stageExpressions     = string(compiler.StageExpressions)
	stageDiscovery       = string(compiler.StageDiscovery)
	stageResolution      = string(compiler.StageResolution)
	stagePlans           = string(compiler.StagePlans)
	stageAdmission       = string(compiler.StageAdmission)
	stagePipeline        = string(compiler.StagePipeline)
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
			stage, code, category := processingErrorDetails(err, stageGraph, compiler.CodeGraphInvalid, "compatibility")
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
	workflowFailed := failed[stageWorkflowParsing]
	if workflowFailed {
		report.SetStage(stageWorkflowParsing, compatibility.Failed)
	} else {
		report.SetStage(stageWorkflowParsing, compatibility.Passed)
	}
	eventFailed := false
	if eventEvaluated {
		eventFailed = failed[stageEventValidation]
		if eventFailed {
			report.SetStage(stageEventValidation, compatibility.Failed)
		} else {
			report.SetStage(stageEventValidation, compatibility.Passed)
		}
	}
	blocked := workflowFailed || eventFailed
	for _, stage := range []string{stageGraph, stageMatrix, stageExpressions, stageDiscovery} {
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
	for _, item := range flattenErrors(err) {
		itemStage, itemCode, itemCategory := processingErrorDetails(item, stage, code, category)
		report.SetStage(itemStage, compatibility.Failed)
		diagnostic := diagnosticFromError(path, itemStage, itemCode, itemCategory, item)
		report.Diagnostics = append(report.Diagnostics, diagnostic)
	}
	markFailedJobs(report)
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
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.Level == "error" && diagnostic.Stage == stagePlans && diagnostic.Instance == report.Jobs[i].Instance && diagnostic.Instance != "" {
				report.Jobs[i].Result = compatibility.Failed
			}
		}
	}
}

func applyProcessingEvidence(report *compatibility.ProcessingReport, evidence compiler.ProcessingEvidence) {
	resolutionFailed := false
	for _, evaluation := range evidence.Actions {
		if !evaluation.Passed {
			resolutionFailed = true
		}
		for i := range report.Actions {
			if report.Actions[i].Job != evaluation.Instance || report.Actions[i].Step != evaluation.Step || report.Actions[i].Reference != evaluation.Reference {
				continue
			}
			if evaluation.Passed {
				report.Actions[i].Result = compatibility.Passed
			} else {
				report.Actions[i].Result = compatibility.Failed
			}
		}
	}
	if resolutionFailed {
		report.SetStage(stageResolution, compatibility.Failed)
	} else if evidence.ActionResolutionComplete {
		report.SetStage(stageResolution, compatibility.Passed)
	}
	if evidence.PlansConstructed {
		report.SetStage(stagePlans, compatibility.Passed)
	}
	if evidence.PipelineGenerated {
		report.SetStage(stagePipeline, compatibility.Passed)
	}
}

func flattenErrors(err error) []error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*compiler.ProcessingFinding); ok {
		return []error{err}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, child := range joined.Unwrap() {
			out = append(out, flattenErrors(child)...)
		}
		return out
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		nested := flattenErrors(wrapped.Unwrap())
		for _, item := range nested {
			if _, structured := item.(*compiler.ProcessingFinding); structured {
				return nested
			}
		}
	}
	return []error{err}
}

func processingErrorDetails(err error, fallbackStage, fallbackCode, fallbackCategory string) (stage, code, category string) {
	if finding, ok := err.(*compiler.ProcessingFinding); ok {
		return string(finding.Stage), finding.Code, finding.Category
	}
	return fallbackStage, fallbackCode, fallbackCategory
}

func diagnosticFromError(defaultPath, stage, code, category string, err error) compatibility.Diagnostic {
	message := err.Error()
	location := (*compatibility.SourceLocation)(nil)
	var finding *compiler.ProcessingFinding
	if structured, ok := err.(*compiler.ProcessingFinding); ok {
		finding = structured
		if finding.Path != "" {
			location = sourceLocation(finding.Path, finding.Line, finding.Column)
		}
	}
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
	if finding != nil {
		diagnostic.Job = finding.Job
		diagnostic.Instance = finding.Instance
		diagnostic.Action = finding.Action
		diagnostic.Step = finding.Step
	}
	if diagnostic.Location == nil && defaultPath != "" && stage != stageEventValidation {
		diagnostic.Location = sourceLocation(defaultPath, 1, 1)
	}
	if diagnostic.Job == "" {
		if start := strings.Index(message, `job "`); start >= 0 {
			rest := message[start+len(`job "`):]
			if end := strings.Index(rest, `"`); end >= 0 {
				diagnostic.Job = rest[:end]
			}
		}
	}
	if diagnostic.Action == "" {
		if start := strings.Index(message, `action "`); start >= 0 {
			rest := message[start+len(`action "`):]
			if end := strings.Index(rest, `"`); end >= 0 {
				diagnostic.Action = rest[:end]
			}
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
