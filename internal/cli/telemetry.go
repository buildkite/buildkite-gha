package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	maxCommandTelemetryDiagnostics = 20
	maxCommandErrorCaptureBytes    = 64 << 10
)

func emitCommandTelemetry(ctx context.Context, command telemetry.Command, outcome telemetry.Outcome, version string, duration time.Duration, details telemetry.Details) {
	client, err := telemetry.New(telemetry.Config{
		Endpoint:      os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
		JobID:         os.Getenv("BUILDKITE_JOB_ID"),
		JobToken:      os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
		ClientVersion: version,
		Disabled:      os.Getenv("BUILDKITE_GHA_TELEMETRY_DISABLED") == "true",
	})
	if err != nil || client == nil {
		return
	}
	_ = client.EmitContext(context.WithoutCancel(ctx), command, outcome, duration, details)
}

func telemetryOutcome(code int, conclusion string, contextErr error) telemetry.Outcome {
	if code == 2 {
		return telemetry.OutcomeUsageError
	}
	if code == buildkitepipeline.ContinueOnErrorExitStatus {
		return telemetry.OutcomeToleratedFailure
	}
	if conclusion == "cancelled" {
		return telemetry.OutcomeCancelled
	}
	if errors.Is(contextErr, context.Canceled) || errors.Is(contextErr, context.DeadlineExceeded) {
		return telemetry.OutcomeCancelled
	}
	if code != 0 || conclusion == "failure" {
		return telemetry.OutcomeFailure
	}
	if conclusion == "skipped" {
		return telemetry.OutcomeSkipped
	}
	return telemetry.OutcomeSuccess
}

type commandTelemetryDetails struct {
	failurePhase  telemetry.FailurePhase
	failureCode   telemetry.FailureCode
	blocker       string
	blockerDetail string
	diagnostics   []telemetry.Diagnostic
	seen          map[telemetryDiagnosticKey]int
	errorOutput   boundedTailBuffer
}

type telemetryDiagnosticKey struct {
	code, blocker, blockerDetail string
}

func (d *commandTelemetryDetails) captureErrors(writer io.Writer) io.Writer {
	return &errorCaptureWriter{writer: writer, capture: &d.errorOutput}
}

func captureCommandRunnerErrors(runner transport.Runner, writer io.Writer) transport.Runner {
	commandRunner, ok := runner.(transport.CommandRunner)
	if !ok {
		return runner
	}
	commandRunner.Stderr = writer
	return commandRunner
}

func (d *commandTelemetryDetails) setFailurePhase(phase telemetry.FailurePhase) {
	if d.failurePhase == "" {
		d.failurePhase = phase
	}
}

func (d *commandTelemetryDetails) setFailureCode(code telemetry.FailureCode) {
	if d.failureCode == "" && code != "" {
		d.failureCode = code
	}
}

// addReportDiagnostics records a report's diagnostics. Errors a command
// handles, such as workflows emitted as failing pipeline steps, belong here so
// they never attribute an unrelated later failure.
func (d *commandTelemetryDetails) addReportDiagnostics(report compatibility.ProcessingReport) {
	for _, diagnostic := range report.Diagnostics {
		severity, ok := telemetrySeverity(diagnostic.Level)
		if !ok || !allowlistedTelemetryDiagnosticCode(diagnostic.Code) {
			continue
		}
		d.addDiagnostic(diagnostic.Code, severity, diagnostic.Blocker, diagnostic.BlockerDetail)
		if diagnostic.Level == "error" {
			d.setBlocker(diagnostic.Blocker, diagnostic.BlockerDetail)
		}
	}
}

// observe records a report that ends the command, so its first error also
// attributes the failure.
func (d *commandTelemetryDetails) observe(report compatibility.ProcessingReport) {
	d.addReportDiagnostics(report)
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Level == "error" && d.failureCode == "" && allowlistedTelemetryDiagnosticCode(diagnostic.Code) {
			d.failurePhase = telemetryPhase(diagnostic.Stage)
			d.failureCode = telemetry.FailureCode(diagnostic.Code)
		}
	}
}

func (d *commandTelemetryDetails) addWarnings(warnings []compiler.Warning) {
	for _, warning := range warnings {
		if allowlistedTelemetryDiagnosticCode(warning.Code) {
			d.addDiagnostic(warning.Code, telemetry.SeverityWarning, warning.Blocker, warning.BlockerDetail)
		}
	}
}

// addActionRuntimeUnknown records admitted actions whose runtime behavior was
// never proven. Upload keeps this in telemetry rather than the processing
// report, where it would annotate every import that uses actions.
func (d *commandTelemetryDetails) addActionRuntimeUnknown() {
	d.addDiagnostic("W_ACTION_RUNTIME_UNKNOWN", telemetry.SeverityWarning, "", "")
}

func (d *commandTelemetryDetails) addDiagnostic(code string, severity telemetry.Severity, blocker, blockerDetail string) {
	if d.seen == nil {
		d.seen = make(map[telemetryDiagnosticKey]int)
	}
	key := telemetryDiagnosticKey{code: code, blocker: blocker, blockerDetail: blockerDetail}
	if index, exists := d.seen[key]; exists {
		if severity == telemetry.SeverityError {
			d.diagnostics[index].Severity = severity
		}
		return
	}
	if len(d.diagnostics) == maxCommandTelemetryDiagnostics {
		return
	}
	d.seen[key] = len(d.diagnostics)
	d.diagnostics = append(d.diagnostics, telemetry.Diagnostic{Code: code, Severity: severity, Blocker: blocker, BlockerDetail: blockerDetail})
}

func (d *commandTelemetryDetails) setBlocker(blocker, detail string) {
	if d.blocker == "" && blocker != "" {
		d.blocker, d.blockerDetail = blocker, detail
	}
}

func (d *commandTelemetryDetails) forOutcome(outcome telemetry.Outcome) telemetry.Details {
	if outcome == telemetry.OutcomeSuccess || outcome == telemetry.OutcomeSkipped {
		return telemetry.Details{Diagnostics: slices.Clone(d.diagnostics), Blocker: d.blocker, BlockerDetail: d.blockerDetail}
	}
	phase, code := d.failurePhase, d.failureCode
	if phase == "" {
		if outcome == telemetry.OutcomeUsageError {
			phase = telemetry.FailurePhaseConfiguration
		} else {
			phase = telemetry.FailurePhaseUnknown
		}
	}
	if code == "" {
		code = telemetry.FailureCodeUnknown
	}
	return telemetry.Details{
		FailurePhase: phase, FailureCode: code, Diagnostics: slices.Clone(d.diagnostics),
		Blocker: d.blocker, BlockerDetail: d.blockerDetail,
		ErrorMessage: string(d.errorOutput.bytes), ErrorMessageTruncated: d.errorOutput.truncated,
	}
}

func (d *commandTelemetryDetails) telemetryDetails() telemetry.Details {
	return telemetry.Details{FailurePhase: d.failurePhase, FailureCode: d.failureCode, Diagnostics: slices.Clone(d.diagnostics), Blocker: d.blocker, BlockerDetail: d.blockerDetail}
}

func telemetrySeverity(level string) (telemetry.Severity, bool) {
	switch level {
	case "error":
		return telemetry.SeverityError, true
	case "warning":
		return telemetry.SeverityWarning, true
	default:
		return "", false
	}
}

func allowlistedTelemetryDiagnosticCode(code string) bool {
	switch code {
	case compiler.CodeWorkflowSyntax, compiler.CodeEventInvalid, compiler.CodeGraphInvalid,
		compiler.CodeMatrixInvalid, compiler.CodeExpressionInvalid, compiler.CodeActionDiscovery,
		compiler.CodeActionResolution, compiler.CodePlanConstruction, compiler.CodePipelineGeneration,
		compiler.CodeEnvironment, "E_PROFILE", "W_ACTION_RUNTIME_UNKNOWN",
		"W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED", "W_TRIGGER_EVENT_UNSUPPORTED":
		return true
	default:
		return false
	}
}

func telemetryPhase(stage string) telemetry.FailurePhase {
	switch compiler.ProcessingStage(stage) {
	case compiler.StageWorkflowParsing:
		return telemetry.FailurePhaseParsing
	case compiler.StageEventValidation, compiler.StageGraph, compiler.StageMatrix, compiler.StageExpressions:
		return telemetry.FailurePhaseEvaluation
	case compiler.StageDiscovery, compiler.StageResolution:
		return telemetry.FailurePhaseSourceResolution
	case compiler.StageAdmission:
		return telemetry.FailurePhaseAdmission
	case compiler.StagePlans, compiler.StagePipeline:
		return telemetry.FailurePhaseCompilation
	default:
		return telemetry.FailurePhaseUnknown
	}
}

type errorCaptureWriter struct {
	mu      sync.Mutex
	writer  io.Writer
	capture *boundedTailBuffer
}

func (w *errorCaptureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writer.Write(p)
	w.capture.Write(p[:n])
	return n, err
}

type boundedTailBuffer struct {
	bytes     []byte
	truncated bool
}

func (b *boundedTailBuffer) Write(p []byte) {
	if len(p) >= maxCommandErrorCaptureBytes {
		b.truncated = b.truncated || len(b.bytes) > 0 || len(p) > maxCommandErrorCaptureBytes
		b.bytes = append(b.bytes[:0], p[len(p)-maxCommandErrorCaptureBytes:]...)
		return
	}
	if overflow := len(b.bytes) + len(p) - maxCommandErrorCaptureBytes; overflow > 0 {
		copy(b.bytes, b.bytes[overflow:])
		b.bytes = b.bytes[:len(b.bytes)-overflow]
		b.truncated = true
	}
	b.bytes = append(b.bytes, p...)
}
