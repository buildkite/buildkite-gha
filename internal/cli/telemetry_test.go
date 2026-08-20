package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestCommandCompletionTelemetryPlacementAndExitSemantics(t *testing.T) {
	type event struct {
		Event      string `json:"event"`
		Properties struct {
			Command       string `json:"command"`
			Outcome       string `json:"outcome"`
			ClientVersion string `json:"client_version"`
			DurationMS    int64  `json:"duration_ms"`
			FailurePhase  string `json:"failure_phase"`
			FailureCode   string `json:"failure_code"`
			ErrorMessage  string `json:"error_message"`
		} `json:"properties"`
	}
	events := make(chan event, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token telemetry-token" {
			t.Errorf("Authorization = %q", got)
		}
		var received event
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode telemetry event: %v", err)
		}
		events <- received
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")

	for _, test := range []struct {
		args        []string
		wantCode    int
		wantCommand string
	}{
		{args: []string{"plugin", "unexpected"}, wantCode: 2, wantCommand: "plugin_import"},
		{args: []string{"run-job", "--unknown"}, wantCode: 2, wantCommand: "run_job"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(test.args, &stdout, &stderr, "test-version", &cliCaptureRunner{}); code != test.wantCode {
			t.Fatalf("run(%q) code = %d, stderr = %q; want %d", test.args, code, stderr.String(), test.wantCode)
		}
		received := <-events
		if received.Event != "pipelines:buildkite_gha:command_completed" || received.Properties.Command != test.wantCommand || received.Properties.Outcome != "usage_error" || received.Properties.ClientVersion != "test-version" || received.Properties.DurationMS < 0 || received.Properties.FailurePhase != "configuration" || received.Properties.FailureCode != "unknown" || !strings.Contains(received.Properties.ErrorMessage, "unknown") && !strings.Contains(received.Properties.ErrorMessage, "does not accept arguments") {
			t.Fatalf("run(%q) event = %#v", test.args, received)
		}
		if strings.ContainsAny(received.Properties.ErrorMessage, "\r\n\t") {
			t.Fatalf("run(%q) error_message is not normalized: %q", test.args, received.Properties.ErrorMessage)
		}
	}
}

func TestCommandCompletionTelemetryOptOut(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "true")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--unknown"}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != 2 {
		t.Fatalf("run() code = %d, stderr = %q; want 2", code, stderr.String())
	}
	if requests != 0 {
		t.Fatalf("telemetry requests = %d, want 0", requests)
	}
}

func TestCommandCompletionTelemetryFailureDoesNotChangeExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private response", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--unknown"}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != 2 {
		t.Fatalf("run() code = %d, stderr = %q; want 2", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "private response") || strings.Contains(stderr.String(), "telemetry") {
		t.Fatalf("telemetry failure leaked to stderr: %q", stderr.String())
	}
}

func TestCommandTelemetryDetailsCollectTypedDiagnostics(t *testing.T) {
	details := &commandTelemetryDetails{}
	details.observe(compatibility.ProcessingReport{Diagnostics: []compatibility.Diagnostic{
		{Level: "error", Code: compiler.CodeWorkflowSyntax, Stage: string(compiler.StageWorkflowParsing), Message: "must not be uploaded"},
		{Level: "error", Code: compiler.CodeWorkflowSyntax, Stage: string(compiler.StageWorkflowParsing), Message: "different sensitive text"},
		{Level: "warning", Code: "W_ACTION_RUNTIME_UNKNOWN", Stage: string(compiler.StageAdmission), Message: "must not be uploaded"},
		{Level: "error", Code: "E_FUTURE_UNALLOWLISTED", Stage: string(compiler.StageGraph), Message: "must not be uploaded"},
	}})
	got := details.telemetryDetails()
	if got.FailurePhase != "parsing" || got.FailureCode != "E_WORKFLOW_SYNTAX" {
		t.Fatalf("failure details = %#v", got)
	}
	want := []telemetry.Diagnostic{
		{Code: compiler.CodeWorkflowSyntax, Severity: telemetry.SeverityError},
		{Code: "W_ACTION_RUNTIME_UNKNOWN", Severity: telemetry.SeverityWarning},
	}
	if !reflect.DeepEqual(got.Diagnostics, want) {
		t.Fatalf("diagnostics = %#v, want %#v", got.Diagnostics, want)
	}
}

func TestUnprovenActionRuntimeIgnoresNativeAdapters(t *testing.T) {
	bundleWith := func(locks ...plan.ActionLock) compiler.Bundle {
		return compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Actions: locks,
			Steps:   []plan.Step{{Kind: "uses", Uses: "actions/checkout@v4"}},
		}}}}
	}
	checkout := plan.ActionLock{Source: "github", Repository: "actions/checkout"}
	upload := plan.ActionLock{Source: "github", Repository: "actions/upload-artifact"}
	download := plan.ActionLock{Source: "github", Repository: "actions/download-artifact"}
	setupGo := plan.ActionLock{Source: "github", Repository: "actions/setup-go"}
	local := plan.ActionLock{Source: "workspace", Path: "actions/build"}

	for _, test := range []struct {
		name  string
		locks []plan.ActionLock
		want  bool
	}{
		{name: "no actions"},
		{name: "native adapters only", locks: []plan.ActionLock{checkout, upload, download}},
		{name: "public action", locks: []plan.ActionLock{setupGo}, want: true},
		{name: "local action", locks: []plan.ActionLock{local}, want: true},
		{name: "native adapter alongside public action", locks: []plan.ActionLock{checkout, setupGo}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := bundleRunsUnprovenActions(bundleWith(test.locks...)); got != test.want {
				t.Fatalf("bundleRunsUnprovenActions() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestHandledReportErrorsDoNotAttributeCommandFailure(t *testing.T) {
	details := &commandTelemetryDetails{}
	details.addReportDiagnostics(compatibility.ProcessingReport{Diagnostics: []compatibility.Diagnostic{
		{Level: "error", Code: compiler.CodeExpressionInvalid, Stage: string(compiler.StageExpressions), Message: "emitted as a failing step"},
	}})
	got := details.forOutcome(telemetry.OutcomeFailure)
	if got.FailurePhase != telemetry.FailurePhaseUnknown || got.FailureCode != telemetry.FailureCodeUnknown {
		t.Fatalf("handled error attributed a later failure: %#v", got)
	}
	want := []telemetry.Diagnostic{{Code: compiler.CodeExpressionInvalid, Severity: telemetry.SeverityError}}
	if !reflect.DeepEqual(got.Diagnostics, want) {
		t.Fatalf("diagnostics = %#v, want %#v", got.Diagnostics, want)
	}
}

func TestCommandTelemetryDetailsPreservesFirstFailurePhase(t *testing.T) {
	details := &commandTelemetryDetails{}
	details.setFailurePhase(telemetry.FailurePhaseExecution)
	details.setFailurePhase(telemetry.FailurePhaseResultPublication)
	got := details.forOutcome(telemetry.OutcomeFailure)
	if got.FailurePhase != telemetry.FailurePhaseExecution || got.FailureCode != telemetry.FailureCodeUnknown {
		t.Fatalf("failure details = %#v", got)
	}
}

func TestTelemetryOutcomePreservesCommandSemantics(t *testing.T) {
	for _, test := range []struct {
		name       string
		code       int
		conclusion string
		contextErr error
		want       telemetry.Outcome
	}{
		{name: "success", conclusion: "success", want: telemetry.OutcomeSuccess},
		{name: "failure", code: 1, conclusion: "failure", want: telemetry.OutcomeFailure},
		{name: "result publication failure", code: 1, conclusion: "success", want: telemetry.OutcomeFailure},
		{name: "cancelled result", code: 1, conclusion: "cancelled", want: telemetry.OutcomeCancelled},
		{name: "cancelled context", code: 1, contextErr: context.Canceled, want: telemetry.OutcomeCancelled},
		{name: "skipped", conclusion: "skipped", want: telemetry.OutcomeSkipped},
		{name: "skipped result publication failure", code: 1, conclusion: "skipped", want: telemetry.OutcomeFailure},
		{name: "tolerated", code: buildkitepipeline.ContinueOnErrorExitStatus, conclusion: "success", want: telemetry.OutcomeToleratedFailure},
		{name: "usage", code: 2, want: telemetry.OutcomeUsageError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := telemetryOutcome(test.code, test.conclusion, test.contextErr); got != test.want {
				t.Fatalf("telemetryOutcome() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunJobUntypedFailurePhaseIsUnknown(t *testing.T) {
	details := (&commandTelemetryDetails{}).forOutcome(telemetry.OutcomeFailure)
	if details.FailurePhase != telemetry.FailurePhaseUnknown || details.FailureCode != telemetry.FailureCodeUnknown {
		t.Fatalf("run-job fallback details = %#v", details)
	}
}

func TestCommandTelemetryCapturesBoundedErrorTailWithoutChangingOutput(t *testing.T) {
	details := &commandTelemetryDetails{}
	var stderr bytes.Buffer
	writer := details.captureErrors(&stderr)
	message := "omitted" + strings.Repeat("x", maxCommandErrorCaptureBytes) + " final failure"
	if _, err := writer.Write([]byte(message)); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != message {
		t.Fatal("error capture changed stderr output")
	}
	got := details.forOutcome(telemetry.OutcomeFailure)
	if !got.ErrorMessageTruncated || len(got.ErrorMessage) != maxCommandErrorCaptureBytes || !strings.HasSuffix(got.ErrorMessage, " final failure") || strings.Contains(got.ErrorMessage, "omitted") {
		t.Fatalf("captured error = %q (%d bytes), truncated = %t", got.ErrorMessage, len(got.ErrorMessage), got.ErrorMessageTruncated)
	}
	if success := details.forOutcome(telemetry.OutcomeSuccess); success.ErrorMessage != "" || success.ErrorMessageTruncated {
		t.Fatalf("successful telemetry included error output: %#v", success)
	}
}

func TestCommandTelemetryCapturesCommandRunnerErrors(t *testing.T) {
	details := &commandTelemetryDetails{}
	var stderr bytes.Buffer
	writer := details.captureErrors(&stderr)
	runner := captureCommandRunnerErrors(transport.CommandRunner{}, writer)

	commandRunner, ok := runner.(transport.CommandRunner)
	if !ok {
		t.Fatalf("runner = %T", runner)
	}
	if _, err := commandRunner.Stderr.Write([]byte("agent-diagnostic")); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "agent-diagnostic" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if got := details.forOutcome(telemetry.OutcomeFailure); got.ErrorMessage != "agent-diagnostic" {
		t.Fatalf("error message = %q", got.ErrorMessage)
	}
}
