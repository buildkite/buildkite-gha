package cli

import (
	"slices"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
)

const maxCommandTelemetryDiagnostics = 20

type commandTelemetryDetails struct {
	failurePhase telemetry.FailurePhase
	failureCode  telemetry.FailureCode
	diagnostics  []telemetry.Diagnostic
	seen         map[string]int
}

func (d *commandTelemetryDetails) observe(report compatibility.ProcessingReport) {
	for _, diagnostic := range report.Diagnostics {
		severity, ok := telemetrySeverity(diagnostic.Level)
		if !ok || !allowlistedTelemetryDiagnosticCode(diagnostic.Code) {
			continue
		}
		d.addDiagnostic(diagnostic.Code, severity)
		if diagnostic.Level == "error" && d.failureCode == "" {
			d.failurePhase = telemetryPhase(diagnostic.Stage)
			d.failureCode = telemetry.FailureCode(diagnostic.Code)
		}
	}
}

func (d *commandTelemetryDetails) addWarnings(warnings []compiler.Warning) {
	for _, warning := range warnings {
		if allowlistedTelemetryDiagnosticCode(warning.Code) {
			d.addDiagnostic(warning.Code, telemetry.SeverityWarning)
		}
	}
}

func (d *commandTelemetryDetails) addDiagnostic(code string, severity telemetry.Severity) {
	if d.seen == nil {
		d.seen = make(map[string]int)
	}
	if index, exists := d.seen[code]; exists {
		if severity == telemetry.SeverityError {
			d.diagnostics[index].Severity = severity
		}
		return
	}
	if len(d.diagnostics) == maxCommandTelemetryDiagnostics {
		return
	}
	d.seen[code] = len(d.diagnostics)
	d.diagnostics = append(d.diagnostics, telemetry.Diagnostic{Code: code, Severity: severity})
}

func (d *commandTelemetryDetails) forOutcome(command telemetry.Command, outcome telemetry.Outcome) telemetry.Details {
	if outcome == telemetry.OutcomeSuccess || outcome == telemetry.OutcomeSkipped {
		return telemetry.Details{Diagnostics: slices.Clone(d.diagnostics)}
	}
	phase, code := d.failurePhase, d.failureCode
	if phase == "" {
		switch {
		case outcome == telemetry.OutcomeUsageError:
			phase = telemetry.FailurePhaseConfiguration
		case command == telemetry.CommandRunJob:
			phase = telemetry.FailurePhaseExecution
		default:
			phase = telemetry.FailurePhaseUnknown
		}
	}
	if code == "" {
		code = telemetry.FailureCodeUnknown
	}
	return telemetry.Details{FailurePhase: phase, FailureCode: code, Diagnostics: slices.Clone(d.diagnostics)}
}

func (d *commandTelemetryDetails) telemetryDetails() telemetry.Details {
	return telemetry.Details{FailurePhase: d.failurePhase, FailureCode: d.failureCode, Diagnostics: slices.Clone(d.diagnostics)}
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
		"W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED":
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
