package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	processingAnnotationContext   = "buildkite-gha-processing"
	skippedWorkflowsContext       = "buildkite-gha-skipped-workflows"
	runnerResolutionContext       = "buildkite-gha-runner-resolution"
	processingAnnotationBodyLimit = 1024 * 1024
	processingAnnotationNotice    = "\n_Additional diagnostics omitted at the Buildkite annotation size limit._\n"
	processingAnnotationTimeout   = 5 * time.Second
)

// processingOutput owns where one command writes processing reports and
// command diagnostics.
type processingOutput struct {
	context       context.Context
	command       string
	format        string
	reports       io.Writer
	stderr        io.Writer
	plugin        bool
	observe       func(compatibility.ProcessingReport)
	annotationJob string
	buildURL      string
	agent         transport.Agent
	sourceLinks   sourceLinkContext
}

type sourceLinkContext struct {
	serverURL          string
	repository         string
	sha                string
	workflowSourceRoot string
}

func sourceLinksForEvent(event compiler.Event) sourceLinkContext {
	return sourceLinkContext{
		serverURL:  plan.EventServerURL(event.Provider),
		repository: event.Repository.Owner + "/" + event.Repository.Name,
		sha:        event.SHA,
	}
}

func (c sourceLinkContext) link(path string, line int) string {
	path = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "./")
	if c.serverURL == "" || c.repository == "" || c.sha == "" || path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return ""
	}
	segments := strings.Split(c.repository+"/blob/"+c.sha+"/"+path, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	link := strings.TrimSuffix(c.serverURL, "/") + "/" + strings.Join(segments, "/")
	if line > 0 {
		link += fmt.Sprintf("#L%d", line)
	}
	return link
}

func newProcessingOutput(ctx context.Context, command, format string, reports, stderr io.Writer, agent transport.Agent) processingOutput {
	out := processingOutput{context: ctx, command: command, format: format, reports: reports, stderr: stderr}
	if os.Getenv("BUILDKITE") == "true" && os.Getenv("BUILDKITE_JOB_ID") != "" {
		out.annotationJob = os.Getenv("BUILDKITE_JOB_ID")
		out.buildURL = os.Getenv("BUILDKITE_BUILD_URL")
		out.agent = agent
	}
	return out
}

// write emits the report, reporting write failures on stderr.
func (o processingOutput) write(ctx context.Context, report compatibility.ProcessingReport) error {
	if o.observe != nil {
		o.observe(report)
	}
	var err error
	if o.plugin {
		err = writePluginProcessing(o.reports, report)
	} else {
		err = compatibility.WriteProcessing(o.reports, o.format, report)
	}
	if err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: write report: %v\n", o.command, err)
		return err
	}
	o.annotate(ctx, report)
	return nil
}

func (o processingOutput) writeV3(ctx context.Context, report compatibility.ProcessingReportV3) error {
	if err := compatibility.WriteProcessingV3(o.reports, o.format, report); err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: write report: %v\n", o.command, err)
		return err
	}
	combined := report.Validation
	combined.Profile = report.Profile
	combined.Diagnostics = append([]compatibility.Diagnostic(nil), report.Validation.Diagnostics...)
	for _, evaluation := range report.Evaluations {
		for _, diagnostic := range evaluation.Report.Diagnostics {
			diagnostic.Message = fmt.Sprintf("Generated %s event: %s", evaluation.Event, diagnostic.Message)
			combined.Diagnostics = append(combined.Diagnostics, diagnostic)
		}
	}
	o.annotate(ctx, combined)
	return nil
}

func writePluginProcessing(w io.Writer, report compatibility.ProcessingReport) error {
	report.Finalize()
	workflow, _ := processingAnnotationWorkflowPath(report.Workflow, "")
	failed := processingReportHasErrors(report)
	if failed {
		if _, err := fmt.Fprintln(w, "^^^ +++"); err != nil {
			return err
		}
	}
	for _, level := range []string{"error", "warning"} {
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.Level != level {
				continue
			}
			marker := "!"
			if level == "error" {
				marker = "x"
			}
			metadata := []string{"workflow=" + workflow}
			if diagnostic.Stage != "" {
				metadata = append(metadata, "stage="+diagnostic.Stage)
			}
			if diagnostic.Job != "" {
				metadata = append(metadata, "job="+diagnostic.Job)
			}
			if diagnostic.Action != "" {
				metadata = append(metadata, "action="+diagnostic.Action)
			}
			if diagnostic.Step != 0 {
				metadata = append(metadata, fmt.Sprintf("step=%d", diagnostic.Step))
			}
			location := ""
			if diagnostic.Location != nil {
				location = fmt.Sprintf(" (%s:%d:%d)", diagnostic.Location.Path, diagnostic.Location.Line, diagnostic.Location.Column)
			}
			if _, err := fmt.Fprintf(w, "%s [%s] %s%s {%s}\n", marker, diagnostic.Code, diagnostic.Message, location, strings.Join(metadata, ", ")); err != nil {
				return err
			}
		}
	}
	if failed {
		_, err := fmt.Fprintf(w, "Compilation: %s. Admission: %s.\n", report.Compile.Result, report.Admission.Result)
		return err
	}
	return nil
}

func (o processingOutput) annotate(parent context.Context, report compatibility.ProcessingReport) {
	if o.annotationJob == "" {
		return
	}
	style, body := processingAnnotation(report, o.sourceLinks)
	if body == "" {
		return
	}
	digest := sha256.Sum256([]byte(report.Workflow))
	annotationContext := fmt.Sprintf("%s-%x", processingAnnotationContext, digest[:6])
	ctx, cancel := context.WithTimeout(parent, processingAnnotationTimeout)
	defer cancel()
	if err := o.agent.AnnotateJob(ctx, o.annotationJob, annotationContext, style, body); err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: warning: processing annotation: %v\n", o.command, err)
	}
}

type skippedWorkflow struct {
	label  string
	key    string
	reason string
}

func (o processingOutput) annotateSkippedWorkflows(parent context.Context, event string, workflows []skippedWorkflow) {
	body := skippedWorkflowsAnnotation(event, workflows, o.buildURL)
	if o.annotationJob == "" || body == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, processingAnnotationTimeout)
	defer cancel()
	if err := o.agent.AnnotateJob(ctx, o.annotationJob, skippedWorkflowsContext, "info", body); err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: warning: skipped workflows annotation: %v\n", o.command, err)
	}
}

func (o processingOutput) annotateRunnerResolutionWarnings(parent context.Context, warnings []runtime.RunnerWarning) {
	if o.annotationJob == "" || len(warnings) == 0 {
		return
	}
	var body strings.Builder
	body.WriteString("#### Unsupported runner labels were mapped to Ubuntu\n\nBuildkite used heuristic runner mappings:\n\n")
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(&body, "* %s — %s\n", annotationCode(warning.Code), annotationHTML(warning.Message))
	}
	ctx, cancel := context.WithTimeout(parent, processingAnnotationTimeout)
	defer cancel()
	if err := o.agent.AnnotateJob(ctx, o.annotationJob, runnerResolutionContext, "warning", body.String()); err != nil {
		_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: warning: runner resolution annotation: %v\n", o.command, err)
	}
}

func skippedWorkflowsAnnotation(event string, workflows []skippedWorkflow, buildURL string) string {
	if len(workflows) == 0 || buildURL == "" {
		return ""
	}
	var out strings.Builder
	if len(workflows) == 1 {
		out.WriteString("#### 1 workflow was skipped\n\n")
	} else {
		_, _ = fmt.Fprintf(&out, "#### %d workflows were skipped\n\n", len(workflows))
	}
	_, _ = fmt.Fprintf(&out, "The current <code>%s</code> event does not match these workflows:\n\n", annotationHTML(event))
	for _, workflow := range workflows {
		annotationURL := fmt.Sprintf("%s/canvas?key=%s&open=false", strings.TrimRight(buildURL, "/"), url.QueryEscape(workflow.key))
		label := annotationHTML(strings.Join(strings.Fields(workflow.label), " "))
		label = strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(label)
		_, _ = fmt.Fprintf(&out, "* [:github: %s](%s) — %s\n", label, annotationURL, annotationHTML(workflow.reason))
	}
	return out.String()
}

func processingAnnotation(report compatibility.ProcessingReport, sourceLinks sourceLinkContext) (style, body string) {
	return processingAnnotationWithin(report, sourceLinks, processingAnnotationBodyLimit, processingAnnotationNotice, true)
}

func processingAnnotationWithin(report compatibility.ProcessingReport, sourceLinks sourceLinkContext, bodyLimit int, truncationNotice string, includeHeading bool) (style, body string) {
	report.Finalize()
	style = "warning"
	diagnostics := make([]compatibility.Diagnostic, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == "W_ACTION_RUNTIME_UNKNOWN" {
			continue
		}
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

	heading := "GitHub Actions workflow diagnostics"
	if style == "error" {
		heading = "Workflow could not be run"
	}
	var out strings.Builder
	if includeHeading {
		out.WriteString("<h2 class=\"h4 mb2\">")
		out.WriteString(heading)
		out.WriteString("</h2>\n")
	}
	workflowPath, workflowLinkable := processingAnnotationWorkflowPath(report.Workflow, "")
	if workflowLinkable {
		sourceLinks.workflowSourceRoot = processingWorkflowSourceRoot(report.Workflow)
	}
	out.WriteString("<p>")
	out.WriteString(annotationSourcePath(workflowPath, 0, 0, workflowLinkable, sourceLinks))
	out.WriteString("</p>\n")
	rows := make([]string, len(diagnostics))
	bodyBytes := out.Len()
	for i, diagnostic := range diagnostics {
		rows[i] = renderProcessingDiagnostic(diagnostic, sourceLinks)
		bodyBytes += len(rows[i])
	}
	if bodyBytes <= bodyLimit {
		for _, row := range rows {
			out.WriteString(row)
		}
		return style, out.String()
	}

	remaining := bodyLimit - out.Len() - len(truncationNotice)
	for i, row := range rows {
		if len(row) > remaining {
			if row = renderProcessingDiagnosticWithin(diagnostics[i], remaining, sourceLinks); row != "" {
				out.WriteString(row)
			}
			out.WriteString(truncationNotice)
			return style, out.String()
		}
		out.WriteString(row)
		remaining -= len(row)
	}
	panic("processing annotation exceeded its precomputed size")
}

func processingWorkflowSourceRoot(path string) string {
	resolved := path
	if !filepath.IsAbs(resolved) {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return ""
		}
		resolved = filepath.Join(workingDirectory, resolved)
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(resolved)))
}

func processingAnnotationWorkflowPath(path, workflowSourceRoot string) (display string, linkable bool) {
	root := os.Getenv("BUILDKITE_BUILD_CHECKOUT_PATH")
	if root == "" {
		root, _ = os.Getwd()
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		slashPath := filepath.ToSlash(path)
		if workflowSourceRoot != "" && strings.HasPrefix(slashPath, "./.github/workflows/") {
			resolved = filepath.Join(workflowSourceRoot, filepath.FromSlash(strings.TrimPrefix(slashPath, "./")))
		} else if workingDirectory, err := os.Getwd(); err == nil {
			resolved = filepath.Join(workingDirectory, resolved)
		}
	}
	display = filepath.ToSlash(filepath.Clean(path))
	if relative, err := filepath.Rel(root, resolved); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		display = filepath.ToSlash(relative)
	}
	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return display, false
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return display, false
	}
	relative, err := filepath.Rel(canonicalRoot, canonical)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(relative), true
	}
	return display, false
}

func renderProcessingDiagnosticWithin(diagnostic compatibility.Diagnostic, limit int, sourceLinks sourceLinkContext) string {
	detail := diagnostic.Detail
	message := diagnostic.Message
	row := renderProcessingDiagnostic(diagnostic, sourceLinks)
	for len(row) > limit && detail != "" {
		end := max(0, len(detail)-(len(row)-limit))
		for end > 0 && !utf8.ValidString(detail[:end]) {
			end--
		}
		if end == 0 {
			diagnostic.Detail = ""
		} else {
			diagnostic.Detail = detail[:end] + "…"
		}
		detail = detail[:end]
		row = renderProcessingDiagnostic(diagnostic, sourceLinks)
	}
	for len(row) > limit && message != "" {
		end := max(0, len(message)-(len(row)-limit))
		for end > 0 && !utf8.ValidString(message[:end]) {
			end--
		}
		diagnostic.Message = message[:end] + "…"
		message = message[:end]
		row = renderProcessingDiagnostic(diagnostic, sourceLinks)
	}
	if len(row) > limit {
		return ""
	}
	return row
}

func renderProcessingDiagnostic(diagnostic compatibility.Diagnostic, sourceLinks sourceLinkContext) string {
	heading, details := annotationDiagnosticPresentation(diagnostic)
	var out strings.Builder
	out.WriteString("<p><strong>")
	out.WriteString(annotationHTML(heading))
	out.WriteString("</strong></p>\n")
	context := make([]string, 0, 4)
	if diagnostic.Action != "" {
		context = append(context, "Action "+annotationCode(diagnostic.Action))
	}
	if diagnostic.Location != nil {
		path, linkable := processingAnnotationWorkflowPath(diagnostic.Location.Path, sourceLinks.workflowSourceRoot)
		context = append(context, annotationSourcePath(path, diagnostic.Location.Line, diagnostic.Location.Column, linkable, sourceLinks))
	}
	if diagnostic.Job != "" {
		context = append(context, "Job "+annotationCode(diagnostic.Job))
	}
	if diagnostic.Step != 0 {
		context = append(context, fmt.Sprintf("Step %d", diagnostic.Step))
	}
	if len(context) != 0 {
		out.WriteString("<p>")
		out.WriteString(strings.Join(context, " · "))
		out.WriteString("</p>\n")
	}
	if len(details) != 0 {
		for _, sentence := range details {
			out.WriteString("<p>")
			detail := annotationHTML(sentence)
			detail = strings.ReplaceAll(detail, "&#34;windows-latest&#34;", annotationCode("windows-latest"))
			detail = strings.ReplaceAll(detail, "&#34;ubuntu-latest&#34;", annotationCode("ubuntu-latest"))
			const issueURL = "https://github.com/buildkite/buildkite-gha"
			detail = strings.ReplaceAll(detail, issueURL+" ", `<a href="`+issueURL+`" target="_blank">buildkite/buildkite-gha</a> `)
			out.WriteString(detail)
			out.WriteString("</p>\n")
		}
	}
	if diagnostic.Detail != "" {
		out.WriteString("<details><summary>Diagnostic detail</summary><p>")
		out.WriteString(annotationHTML(diagnostic.Detail))
		out.WriteString("</p></details>\n")
	}
	return out.String()
}

func annotationSourcePath(path string, line, column int, linkable bool, sourceLinks sourceLinkContext) string {
	display := path
	if line > 0 {
		display += fmt.Sprintf(":%d", line)
		if column > 0 {
			display += fmt.Sprintf(":%d", column)
		}
	}
	code := annotationCode(display)
	if link := sourceLinks.link(path, line); linkable && link != "" {
		return `<a href="` + html.EscapeString(link) + `">` + code + `</a>`
	}
	return code
}

func annotationDiagnosticMessage(diagnostic compatibility.Diagnostic) string {
	if diagnostic.Location == nil {
		return diagnostic.Message
	}
	prefix := fmt.Sprintf("%s:%d:%d: ", diagnostic.Location.Path, diagnostic.Location.Line, diagnostic.Location.Column)
	return strings.TrimPrefix(diagnostic.Message, prefix)
}

func annotationDiagnosticPresentation(diagnostic compatibility.Diagnostic) (heading string, details []string) {
	message := annotationDiagnosticMessage(diagnostic)
	if diagnostic.Action != "" {
		for _, prefix := range []string{
			fmt.Sprintf("Action %q is unsupported: ", diagnostic.Action),
			fmt.Sprintf("Action %q could not be resolved: ", diagnostic.Action),
		} {
			if after, ok := strings.CutPrefix(message, prefix); ok {
				message = upperFirst(after)
				break
			}
		}
	}
	sentences := annotationSentences(message)
	if len(sentences) == 0 {
		return "Compatibility diagnostic", nil
	}
	return sentences[0], sentences[1:]
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

func annotationCode(value string) string {
	return "<code>" + annotationHTML(value) + "</code>"
}

func annotationHTML(value string) string {
	return html.EscapeString(strings.Join(strings.Fields(value), " "))
}

// fail emits the report and the failure that ended the command.
func (o processingOutput) fail(ctx context.Context, report compatibility.ProcessingReport, err error) int {
	_ = o.write(ctx, report)
	_, _ = fmt.Fprintf(o.stderr, "buildkite-gha: %s: %v\n", o.command, err)
	return 1
}

// loadProcessingInputs reads the workflow source and acquires the optional
// event snapshot, emitting the standard failure report when either input is
// unavailable. A nil loadEvent means the command runs without an event.
func loadProcessingInputs(ctx context.Context, out processingOutput, workflowPath, profile, eventFailureMessage string, loadEvent func() ([]byte, error)) (source, event []byte, ok bool) {
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		out.fail(ctx, compatibility.EnvironmentProcessingReport(workflowPath, profile, "workflow input could not be read"), err)
		return nil, nil, false
	}
	return loadProcessingInputsSource(ctx, out, workflowPath, profile, source, eventFailureMessage, loadEvent)
}

func loadProcessingInputsSource(ctx context.Context, out processingOutput, workflowPath, profile string, source []byte, eventFailureMessage string, loadEvent func() ([]byte, error)) ([]byte, []byte, bool) {
	if loadEvent == nil {
		return source, nil, true
	}
	event, err := loadEvent()
	if err != nil {
		out.fail(ctx, compatibility.EventInputProcessingReport(workflowPath, profile, source, eventFailureMessage), err)
		return nil, nil, false
	}
	return source, event, true
}

// validatedProcessingReport validates the workflow, against the event when
// one was evaluated, and starts the processing report. It emits the report
// when validation rejects the workflow.
func validatedProcessingReport(ctx context.Context, out processingOutput, workflowPath, profile string, source, event []byte, eventEvaluated bool) (compatibility.ProcessingReport, bool) {
	return validatedProcessingReportWithOptions(ctx, out, workflowPath, profile, source, event, eventEvaluated, nil)
}

func validatedProcessingReportWithOptions(ctx context.Context, out processingOutput, workflowPath, profile string, source, event []byte, eventEvaluated bool, options *compiler.Options) (compatibility.ProcessingReport, bool) {
	var validation compiler.Report
	var err error
	switch {
	case eventEvaluated && options != nil:
		validation, err = compiler.ValidateEventWithOptionsContext(ctx, workflowPath, source, event, *options)
	case eventEvaluated:
		validation, err = compiler.ValidateEventWithOptionsContext(ctx, workflowPath, source, event, compiler.DefaultOptions())
	case options != nil:
		validation, err = compiler.ValidateWithOptionsContext(ctx, workflowPath, source, *options)
	default:
		validation, err = compiler.ValidateWithOptionsContext(ctx, workflowPath, source, compiler.DefaultOptions())
	}
	report := compatibility.InitialProcessingReport(workflowPath, profile, eventEvaluated, validation, err)
	if err != nil {
		report.Result = "incompatible"
		_ = out.write(ctx, report)
		return report, false
	}
	return report, true
}

// applyHostedPreflight folds hosted preflight evidence and any admission
// grant into the report.
func applyHostedPreflight(report *compatibility.ProcessingReport, preflight hostedCompilation) {
	report.ApplyEvidence(preflight.Bundle.Processing)
	report.ApplyWarnings(report.Workflow, preflight.Bundle.IR.Warnings)
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
