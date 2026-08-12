package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
)

// processingOutput owns where one command writes processing reports and
// command diagnostics.
type processingOutput struct {
	command string
	format  string
	reports io.Writer
	stderr  io.Writer
}

// write emits the report, reporting write failures on stderr.
func (o processingOutput) write(report compatibility.ProcessingReport) error {
	if err := compatibility.WriteProcessing(o.reports, o.format, report); err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: write report: %v\n", o.command, err)
		return err
	}
	return nil
}

// fail emits the report and the failure that ended the command.
func (o processingOutput) fail(report compatibility.ProcessingReport, err error) int {
	_ = o.write(report)
	_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: %v\n", o.command, err)
	return 1
}

// loadProcessingInputs reads the workflow source and acquires the optional
// event snapshot, emitting the standard failure report when either input is
// unavailable. A nil loadEvent means the command runs without an event.
func loadProcessingInputs(out processingOutput, workflowPath, profile, eventFailureMessage string, loadEvent func() ([]byte, error)) (source, event []byte, ok bool) {
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		out.fail(environmentProcessingReport(workflowPath, profile, "workflow input could not be read"), err)
		return nil, nil, false
	}
	if loadEvent == nil {
		return source, nil, true
	}
	event, err = loadEvent()
	if err != nil {
		out.fail(eventInputProcessingReport(workflowPath, profile, source, eventFailureMessage), err)
		return nil, nil, false
	}
	return source, event, true
}

// validatedProcessingReport validates the workflow, against the event when
// one was evaluated, and starts the processing report. It emits the report
// when validation rejects the workflow.
func validatedProcessingReport(out processingOutput, workflowPath, profile string, source, event []byte, eventEvaluated bool) (compatibility.ProcessingReport, bool) {
	var validation compiler.Report
	var err error
	if eventEvaluated {
		validation, err = compiler.ValidateEvent(workflowPath, source, event)
	} else {
		validation, err = compiler.Validate(workflowPath, source)
	}
	report := initialProcessingReport(workflowPath, profile, eventEvaluated, validation, err)
	if err != nil {
		report.Result = "incompatible"
		_ = out.write(report)
		return report, false
	}
	return report, true
}

// applyHostedPreflight folds hosted preflight evidence and any admission
// grant into the report.
func applyHostedPreflight(report *compatibility.ProcessingReport, preflight hostedTokenlessCompilation) {
	applyProcessingEvidence(report, preflight.Bundle.Processing)
	if preflight.Admitted {
		report.SetStage(stageAdmission, compatibility.Passed)
		report.Admission.Result = "admitted"
	}
}

// classifyHostedTokenlessFailure records a failed hosted preflight in the
// report and returns the report result it implies.
func classifyHostedTokenlessFailure(report *compatibility.ProcessingReport, workflowPath string, err error) string {
	var failure *hostedTokenlessFailure
	if errors.As(err, &failure) && failure.Kind == hostedTokenlessAdmissionFailure {
		addProcessingFailure(report, workflowPath, stageAdmission, "E_PROFILE", "admission", err)
		report.Admission.Result = "not-admitted"
		return "not-admitted"
	}
	if errors.As(err, &failure) && failure.Kind == hostedTokenlessEnvironmentFailure {
		addEnvironmentFailure(report, "hosted workflow-processing environment could not be initialized")
	} else {
		addProcessingFailure(report, workflowPath, stageResolution, compiler.CodeActionResolution, "action-resolution", err)
	}
	return "indeterminate"
}

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
		result := compatibility.Passed
		if report.NotEvaluatedJobs[job.ID] {
			result = compatibility.NotEvaluated
		}
		out.Jobs = append(out.Jobs, compatibility.JobResult{
			ID: job.ID, Result: result,
			Location: sourceLocation(job.Path, job.Source.Start.Line, job.Source.Start.Column),
		})
	}
	for _, instance := range report.Jobs {
		result := compatibility.Passed
		if report.NotEvaluatedInstances[instance.Key] {
			result = compatibility.NotEvaluated
		}
		out.Jobs = append(out.Jobs, compatibility.JobResult{
			ID: instance.LogicalJobID, Instance: instance.Key, Result: result,
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
	blocked := workflowFailed || eventFailed || failed[""]
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

func addEnvironmentFailure(report *compatibility.ProcessingReport, message string) {
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Code: "E_ENVIRONMENT", Category: "environment", Message: message,
	})
}

func environmentProcessingReport(path, profile, message string) compatibility.ProcessingReport {
	report := compatibility.NewProcessingReport(path, profile)
	report.Result = "indeterminate"
	addEnvironmentFailure(&report, message)
	return report
}

func eventInputProcessingReport(path, profile string, source []byte, message string) compatibility.ProcessingReport {
	parsed, parseErr := compiler.ParseWorkflow(path, source)
	report := compatibility.NewProcessingReport(path, profile)
	report.LogicalJobs = parsed.LogicalJobs
	for _, job := range parsed.ParsedJobs {
		report.Jobs = append(report.Jobs, compatibility.JobResult{
			ID: job.ID, Result: compatibility.NotEvaluated,
			Location: sourceLocation(job.Path, job.Source.Start.Line, job.Source.Start.Column),
		})
	}
	if parseErr != nil {
		addProcessingFailure(&report, path, stageWorkflowParsing, compiler.CodeWorkflowSyntax, "syntax", parseErr)
	} else {
		report.SetStage(stageWorkflowParsing, compatibility.Passed)
	}
	report.Result = "indeterminate"
	addEnvironmentFailure(&report, message)
	return report
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
			if diagnostic.Level != "error" || diagnostic.Stage == stageResolution || report.Jobs[i].Instance == "" {
				continue
			}
			if diagnostic.Instance == report.Jobs[i].Instance || (diagnostic.Instance == "" && diagnostic.Job == report.Jobs[i].ID) {
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
	for _, evaluation := range evidence.Plans {
		for i := range report.Jobs {
			if report.Jobs[i].Instance != evaluation.Instance || evaluation.Instance == "" {
				continue
			}
			switch {
			case !evaluation.Evaluated:
				report.Jobs[i].Result = compatibility.NotEvaluated
			case evaluation.Passed:
				report.Jobs[i].Result = compatibility.Passed
			default:
				report.Jobs[i].Result = compatibility.Failed
			}
		}
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

func sourceLocation(path string, line, column int) *compatibility.SourceLocation {
	if line <= 0 {
		line = 1
	}
	if column <= 0 {
		column = 1
	}
	return &compatibility.SourceLocation{Path: path, Line: line, Column: column}
}
