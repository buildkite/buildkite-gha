package compatibility

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

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
	stagePipeline        = string(compiler.StagePipeline)
)

var locatedDiagnosticPattern = regexp.MustCompile(`^(.+):(\d+):(\d+): (.*)$`)

// InitialProcessingReport starts a processing report from a compiler
// validation outcome, recording stage results, discovered jobs and actions,
// diagnostics, and warnings.
func InitialProcessingReport(path, profile string, eventEvaluated bool, report compiler.Report, processingErr error) ProcessingReport {
	out := NewProcessingReport(path, profile)
	out.LogicalJobs = report.LogicalJobs
	out.Instances = report.Instances
	out.Compile = Stage{Result: "compilable", LogicalJobs: report.LogicalJobs, Instances: report.Instances}
	out.Admission = Stage{Result: NotEvaluated}
	for _, job := range report.ParsedJobs {
		result := Passed
		if report.NotEvaluatedJobs[job.ID] {
			result = NotEvaluated
		}
		out.Jobs = append(out.Jobs, JobResult{
			ID: job.ID, Result: result,
			Location: sourceLocation(job.Path, job.Source.Start.Line, job.Source.Start.Column),
		})
	}
	for _, instance := range report.Jobs {
		result := Passed
		if report.NotEvaluatedInstances[instance.Key] {
			result = NotEvaluated
		}
		out.Jobs = append(out.Jobs, JobResult{
			ID: instance.LogicalJobID, Instance: instance.Key, Result: result,
			Location: sourceLocation(instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column),
		})
		for i, step := range instance.Steps {
			if step.Uses == "" {
				continue
			}
			out.Actions = append(out.Actions, ActionResult{
				Reference: step.Uses, Job: instance.Key, Step: i + 1,
				Result:   NotEvaluated,
				Location: sourceLocation(instance.SourcePath, step.Span.Start.Line, step.Span.Start.Column),
			})
		}
	}

	if processingErr == nil {
		out.SetStage(stageWorkflowParsing, Passed)
		if eventEvaluated {
			out.SetStage(stageEventValidation, Passed)
		}
		out.SetStage(stageGraph, Passed)
		out.SetStage(stageMatrix, Passed)
		out.SetStage(stageExpressions, Passed)
		out.SetStage(stageDiscovery, Passed)
		if len(out.Actions) == 0 {
			out.SetStage(stageResolution, Passed)
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
		out.Diagnostics = append(out.Diagnostics, Diagnostic{
			Level: "warning", Code: warning.Code, Category: "compatibility", Stage: stageExpressions,
			Message: fmt.Sprintf("%s:%d:%d: %s", path, warning.Line, warning.Column, warning.Message), Location: sourceLocation(path, warning.Line, warning.Column),
		})
	}
	return out
}

func setInitialStageResults(report *ProcessingReport, eventEvaluated bool, failed map[string]bool) {
	workflowFailed := failed[stageWorkflowParsing]
	if workflowFailed {
		report.SetStage(stageWorkflowParsing, Failed)
	} else {
		report.SetStage(stageWorkflowParsing, Passed)
	}
	eventFailed := false
	if eventEvaluated {
		eventFailed = failed[stageEventValidation]
		if eventFailed {
			report.SetStage(stageEventValidation, Failed)
		} else {
			report.SetStage(stageEventValidation, Passed)
		}
	}
	blocked := workflowFailed || eventFailed || failed[""]
	for _, stage := range []string{stageGraph, stageMatrix, stageExpressions, stageDiscovery} {
		if failed[stage] {
			report.SetStage(stage, Failed)
			blocked = true
			continue
		}
		if !blocked {
			report.SetStage(stage, Passed)
		}
	}
}

// AddFailure records every finding in err as a failed-stage diagnostic and
// marks the jobs those findings implicate.
func (r *ProcessingReport) AddFailure(path, stage, code, category string, err error) {
	for _, item := range flattenErrors(err) {
		itemStage, itemCode, itemCategory := processingErrorDetails(item, stage, code, category)
		r.SetStage(itemStage, Failed)
		diagnostic := diagnosticFromError(path, itemStage, itemCode, itemCategory, item)
		r.Diagnostics = append(r.Diagnostics, diagnostic)
	}
	markFailedJobs(r)
}

// AddEnvironmentFailure records a failure of the processing environment
// rather than of the workflow under evaluation.
func (r *ProcessingReport) AddEnvironmentFailure(message string) {
	r.Diagnostics = append(r.Diagnostics, Diagnostic{
		Level: "error", Code: "E_ENVIRONMENT", Category: "environment", Message: message,
	})
}

// EnvironmentProcessingReport reports that processing could not start because
// the environment failed before the workflow was read.
func EnvironmentProcessingReport(path, profile, message string) ProcessingReport {
	report := NewProcessingReport(path, profile)
	report.Result = "indeterminate"
	report.AddEnvironmentFailure(message)
	return report
}

// EventInputProcessingReport reports that the event input could not be
// acquired, retaining whatever the workflow source alone can prove.
func EventInputProcessingReport(path, profile string, source []byte, message string) ProcessingReport {
	parsed, parseErr := compiler.ParseWorkflow(path, source)
	report := NewProcessingReport(path, profile)
	report.LogicalJobs = parsed.LogicalJobs
	for _, job := range parsed.ParsedJobs {
		report.Jobs = append(report.Jobs, JobResult{
			ID: job.ID, Result: NotEvaluated,
			Location: sourceLocation(job.Path, job.Source.Start.Line, job.Source.Start.Column),
		})
	}
	if parseErr != nil {
		report.AddFailure(path, stageWorkflowParsing, compiler.CodeWorkflowSyntax, "syntax", parseErr)
	} else {
		report.SetStage(stageWorkflowParsing, Passed)
	}
	report.Result = "indeterminate"
	report.AddEnvironmentFailure(message)
	return report
}

func markFailedJobs(report *ProcessingReport) {
	failed := map[string]bool{}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Level == "error" && diagnostic.Job != "" {
			failed[diagnostic.Job] = true
		}
	}
	for i := range report.Jobs {
		if report.Jobs[i].Instance == "" && failed[report.Jobs[i].ID] {
			report.Jobs[i].Result = Failed
		}
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.Level != "error" || diagnostic.Stage == stageResolution || report.Jobs[i].Instance == "" {
				continue
			}
			if diagnostic.Instance == report.Jobs[i].Instance || (diagnostic.Instance == "" && diagnostic.Job == report.Jobs[i].ID) {
				report.Jobs[i].Result = Failed
			}
		}
	}
}

// ApplyEvidence folds compiler processing evidence into action, job, and
// stage results.
func (r *ProcessingReport) ApplyEvidence(evidence compiler.ProcessingEvidence) {
	resolutionFailed := false
	for _, evaluation := range evidence.Actions {
		if !evaluation.Passed {
			resolutionFailed = true
		}
		for i := range r.Actions {
			if r.Actions[i].Job != evaluation.Instance || r.Actions[i].Step != evaluation.Step || r.Actions[i].Reference != evaluation.Reference {
				continue
			}
			if evaluation.Passed {
				r.Actions[i].Result = Passed
			} else {
				r.Actions[i].Result = Failed
			}
		}
	}
	if resolutionFailed {
		r.SetStage(stageResolution, Failed)
	} else if evidence.ActionResolutionComplete {
		r.SetStage(stageResolution, Passed)
	}
	for _, evaluation := range evidence.Plans {
		for i := range r.Jobs {
			if r.Jobs[i].Instance != evaluation.Instance || evaluation.Instance == "" {
				continue
			}
			switch {
			case !evaluation.Evaluated:
				r.Jobs[i].Result = NotEvaluated
			case evaluation.Passed:
				r.Jobs[i].Result = Passed
			default:
				r.Jobs[i].Result = Failed
			}
		}
	}
	if evidence.PlansConstructed {
		r.SetStage(stagePlans, Passed)
	}
	if evidence.PipelineGenerated {
		r.SetStage(stagePipeline, Passed)
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

func diagnosticFromError(defaultPath, stage, code, category string, err error) Diagnostic {
	message := err.Error()
	location := (*SourceLocation)(nil)
	var finding *compiler.ProcessingFinding
	if structured, ok := err.(*compiler.ProcessingFinding); ok {
		finding = structured
		if finding.Message != "" {
			message = finding.Message
		}
		if finding.Path != "" {
			location = sourceLocation(finding.Path, finding.Line, finding.Column)
		}
	}
	if finding == nil || finding.Message == "" {
		if match := locatedDiagnosticPattern.FindStringSubmatch(message); match != nil {
			line, lineErr := strconv.Atoi(match[2])
			column, columnErr := strconv.Atoi(match[3])
			if lineErr == nil && columnErr == nil {
				location = sourceLocation(match[1], line, column)
				message = match[4]
			}
		}
	}
	diagnostic := Diagnostic{
		Level: "error", Code: code, Category: category, Stage: stage,
		Message: message, Location: location,
	}
	if finding != nil {
		diagnostic.Job = finding.Job
		diagnostic.Instance = finding.Instance
		diagnostic.Action = finding.Action
		diagnostic.Step = finding.Step
	}
	if diagnostic.Location == nil && defaultPath != "" && stage != "" && stage != stageEventValidation {
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

func sourceLocation(path string, line, column int) *SourceLocation {
	if line <= 0 {
		line = 1
	}
	if column <= 0 {
		column = 1
	}
	return &SourceLocation{Path: path, Line: line, Column: column}
}
