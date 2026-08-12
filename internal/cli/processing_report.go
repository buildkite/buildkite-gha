package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	processingAnnotationContext   = "buildkite-gha-processing"
	processingAnnotationBodyLimit = 1024 * 1024
)

// processingOutput owns where one command writes processing reports and
// command diagnostics.
type processingOutput struct {
	command       string
	format        string
	reports       io.Writer
	stderr        io.Writer
	annotationJob string
	agent         transport.Agent
}

func newProcessingOutput(command, format string, reports, stderr io.Writer, agent transport.Agent) processingOutput {
	out := processingOutput{command: command, format: format, reports: reports, stderr: stderr}
	if os.Getenv("BUILDKITE") == "true" && os.Getenv("BUILDKITE_JOB_ID") != "" {
		out.annotationJob = os.Getenv("BUILDKITE_JOB_ID")
		out.agent = agent
	}
	return out
}

// write emits the report, reporting write failures on stderr.
func (o processingOutput) write(report compatibility.ProcessingReport) error {
	if err := compatibility.WriteProcessing(o.reports, o.format, report); err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: write report: %v\n", o.command, err)
		return err
	}
	o.annotate(report)
	return nil
}

func (o processingOutput) annotate(report compatibility.ProcessingReport) {
	if o.annotationJob == "" {
		return
	}
	style, body := processingAnnotation(report)
	if body == "" {
		return
	}
	digest := sha256.Sum256([]byte(report.Workflow))
	annotationContext := fmt.Sprintf("%s-%x", processingAnnotationContext, digest[:6])
	if err := o.agent.AnnotateJob(context.Background(), o.annotationJob, annotationContext, style, body); err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: warning: processing annotation: %v\n", o.command, err)
	}
}

func processingAnnotation(report compatibility.ProcessingReport) (style, body string) {
	report.Finalize()
	style = "warning"
	diagnostics := make([]compatibility.Diagnostic, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Level != "warning" && diagnostic.Level != "error" {
			continue
		}
		if diagnostic.Level == "error" {
			style = "error"
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	if len(diagnostics) == 0 {
		return "", ""
	}

	var out strings.Builder
	out.WriteString("### GitHub Actions workflow diagnostics\n\n")
	out.WriteString("**Workflow:** ")
	out.WriteString(markdownText(report.Workflow))
	out.WriteString("\n")
	for _, diagnostic := range diagnostics {
		heading, message := annotationDiagnosticPresentation(diagnostic)
		out.WriteString("\n#### ")
		out.WriteString(heading)
		out.WriteString("\n\n")
		out.WriteString(markdownCode(diagnostic.Code))
		if diagnostic.Location != nil {
			out.WriteString(" · ")
			out.WriteString(markdownCode(fmt.Sprintf("%s:%d:%d", diagnostic.Location.Path, diagnostic.Location.Line, diagnostic.Location.Column)))
		}
		if diagnostic.Job != "" {
			out.WriteString(" · Job ")
			out.WriteString(markdownCode(diagnostic.Job))
		}
		if diagnostic.Step != 0 {
			_, _ = fmt.Fprintf(&out, " · Step %d", diagnostic.Step)
		}
		out.WriteString("\n\n")
		for i, sentence := range annotationSentences(message) {
			if i != 0 {
				out.WriteString("  \n")
			}
			out.WriteString(markdownText(sentence))
		}
		out.WriteString("\n")
	}
	return style, truncateProcessingAnnotation(out.String())
}

func truncateProcessingAnnotation(body string) string {
	if len(body) <= processingAnnotationBodyLimit {
		return body
	}
	const notice = "\n\n_Additional diagnostics omitted at the Buildkite annotation size limit._\n"
	end := processingAnnotationBodyLimit - len(notice)
	for !utf8.ValidString(body[:end]) {
		end--
	}
	return body[:end] + notice
}

func annotationDiagnosticMessage(diagnostic compatibility.Diagnostic) string {
	if diagnostic.Location == nil {
		return diagnostic.Message
	}
	prefix := fmt.Sprintf("%s:%d:%d: ", diagnostic.Location.Path, diagnostic.Location.Line, diagnostic.Location.Column)
	return strings.TrimPrefix(diagnostic.Message, prefix)
}

func annotationDiagnosticPresentation(diagnostic compatibility.Diagnostic) (heading, message string) {
	message = annotationDiagnosticMessage(diagnostic)
	if diagnostic.Action != "" {
		for _, presentation := range []struct {
			prefix  string
			heading string
		}{
			{fmt.Sprintf("Action %q is unsupported: ", diagnostic.Action), "Unsupported action"},
			{fmt.Sprintf("Action %q could not be resolved: ", diagnostic.Action), "Action could not be resolved"},
		} {
			if strings.HasPrefix(message, presentation.prefix) {
				return presentation.heading + ": **" + markdownText(diagnostic.Action) + "**", upperFirst(strings.TrimPrefix(message, presentation.prefix))
			}
		}
		return "Action compatibility: **" + markdownText(diagnostic.Action) + "**", message
	}
	switch {
	case strings.HasPrefix(message, "Runner label"):
		heading = "Unsupported runner"
	case strings.Contains(message, "job or service containers"):
		heading = "Unsupported hosted container"
	case strings.Contains(message, "needs GITHUB_TOKEN"):
		heading = "Unsupported GITHUB_TOKEN configuration"
	case diagnostic.Code == "W_ACTION_RUNTIME_UNKNOWN":
		heading = "Third-party action runtime compatibility"
	case diagnostic.Stage == string(compiler.StageAdmission):
		heading = "Hosted profile incompatibility"
	case diagnostic.Level == "warning":
		heading = "Compatibility warning"
	default:
		heading = "Workflow incompatibility"
	}
	return heading, message
}

func annotationSentences(message string) []string {
	parts := strings.Split(strings.Join(strings.Fields(message), " "), ". ")
	for i := range parts[:len(parts)-1] {
		parts[i] += "."
	}
	return parts
}

func upperFirst(value string) string {
	if value == "" {
		return value
	}
	first, size := utf8.DecodeRuneInString(value)
	return string(unicode.ToUpper(first)) + value[size:]
}

func markdownCode(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "\\`").Replace(value)
	return "`" + value + "`"
}

func markdownText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
	replacer := strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]",
		"<", "\\<", ">", "\\>", "#", "\\#", "|", "\\|",
	)
	return replacer.Replace(value)
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
		out.fail(compatibility.EnvironmentProcessingReport(workflowPath, profile, "workflow input could not be read"), err)
		return nil, nil, false
	}
	if loadEvent == nil {
		return source, nil, true
	}
	event, err = loadEvent()
	if err != nil {
		out.fail(compatibility.EventInputProcessingReport(workflowPath, profile, source, eventFailureMessage), err)
		return nil, nil, false
	}
	return source, event, true
}

// validatedProcessingReport validates the workflow, against the event when
// one was evaluated, and starts the processing report. It emits the report
// when validation rejects the workflow.
func validatedProcessingReport(out processingOutput, workflowPath, profile string, source, event []byte, eventEvaluated bool) (compatibility.ProcessingReport, bool) {
	return validatedProcessingReportWithOptions(out, workflowPath, profile, source, event, eventEvaluated, nil)
}

func validatedProcessingReportWithOptions(out processingOutput, workflowPath, profile string, source, event []byte, eventEvaluated bool, options *compiler.Options) (compatibility.ProcessingReport, bool) {
	var validation compiler.Report
	var err error
	if eventEvaluated && options != nil {
		validation, err = compiler.ValidateEventWithOptions(workflowPath, source, event, *options)
	} else if eventEvaluated {
		validation, err = compiler.ValidateEvent(workflowPath, source, event)
	} else {
		validation, err = compiler.Validate(workflowPath, source)
	}
	report := compatibility.InitialProcessingReport(workflowPath, profile, eventEvaluated, validation, err)
	if err != nil {
		report.Result = "incompatible"
		_ = out.write(report)
		return report, false
	}
	return report, true
}

// applyHostedPreflight folds hosted preflight evidence and any admission
// grant into the report.
func applyHostedPreflight(report *compatibility.ProcessingReport, preflight hostedCompilation) {
	report.ApplyEvidence(preflight.Bundle.Processing)
	if preflight.Admitted {
		report.SetStage(string(compiler.StageAdmission), compatibility.Passed)
		report.Admission.Result = "admitted"
	}
}

// classifyHostedFailure records a failed hosted preflight in the
// report and returns the report result it implies.
func classifyHostedFailure(report *compatibility.ProcessingReport, workflowPath string, err error) string {
	var failure *hostedFailure
	if errors.As(err, &failure) && failure.Kind == hostedAdmissionFailure {
		report.AddFailure(workflowPath, string(compiler.StageAdmission), "E_PROFILE", "admission", err)
		report.Admission.Result = "not-admitted"
		return "not-admitted"
	}
	if errors.As(err, &failure) && failure.Kind == hostedEnvironmentFailure {
		report.AddEnvironmentFailure("hosted workflow-processing environment could not be initialized")
	} else {
		report.AddFailure(workflowPath, string(compiler.StageResolution), compiler.CodeActionResolution, "action-resolution", err)
	}
	return "indeterminate"
}
