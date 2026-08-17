package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"go.yaml.in/yaml/v4"
)

const (
	cliTestBuildID       = "11111111-1111-4111-8111-111111111111"
	cliTestJobID         = "22222222-2222-4222-8222-222222222222"
	cliTestProducerJobID = "33333333-3333-4333-8333-333333333333"
)

func TestMain(m *testing.M) {
	_ = os.Unsetenv("BUILDKITE")
	_ = os.Unsetenv("BUILDKITE_JOB_ID")
	os.Exit(m.Run())
}

// Stable stage IDs asserted throughout the report expectations below.
const (
	stageWorkflowParsing = string(compiler.StageWorkflowParsing)
	stageEventValidation = string(compiler.StageEventValidation)
	stageGraph           = string(compiler.StageGraph)
	stageMatrix          = string(compiler.StageMatrix)
	stageExpressions     = string(compiler.StageExpressions)
	stageResolution      = string(compiler.StageResolution)
)

func requireImporterHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the importer requires linux/amd64")
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunHelpAndVersion(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{name: "help flag", args: []string{"--help"}, wantOutput: "validate"},
		{name: "help command", args: []string{"help"}, wantOutput: "run-job"},
		{name: "command help", args: []string{"help", "compile"}, wantOutput: "buildkite-gha compile --event-path"},
		{name: "upload help", args: []string{"help", "upload"}, wantOutput: "--runner-queue"},
		{name: "upload help flag", args: []string{"upload", "--help"}, wantOutput: "--runner-queue"},
		{name: "command help flag", args: []string{"run-job", "--help"}, wantOutput: "--plan <path>"},
		{name: "version flag", args: []string{"--version"}, wantOutput: "buildkite-gha test-version\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr, "test-version"); code != 0 {
				t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.wantOutput) {
				t.Errorf("Run() stdout = %q, want it to contain %q", stdout.String(), test.wantOutput)
			}
			if stderr.Len() != 0 {
				t.Errorf("Run() stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestUploadHelpFormsMatch(t *testing.T) {
	outputs := make([]string, 0, 2)
	for _, args := range [][]string{{"help", "upload"}, {"upload", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test-version"); code != 0 {
			t.Fatalf("Run(%q) code = %d, stderr = %q", args, code, stderr.String())
		}
		outputs = append(outputs, stdout.String())
	}
	if outputs[0] != outputs[1] || !strings.Contains(outputs[0], "explicit .yml or .yaml path") || !strings.Contains(outputs[0], "failed or skipped workflows become top-level replacement steps") || !strings.Contains(outputs[0], "--runner-image") || !strings.Contains(outputs[0], "every scheduled workflow group is eligible") {
		t.Fatalf("upload help outputs differ or omit runner profile or aggregate workflow options:\nhelp command: %q\nhelp flag: %q", outputs[0], outputs[1])
	}
}

func TestPluginIsHiddenAndZeroArgument(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test-version"); code != 0 {
			t.Fatalf("Run(%q) code = %d, stderr = %q", args, code, stderr.String())
		}
		if strings.Contains(stdout.String(), "plugin") {
			t.Fatalf("Run(%q) exposed plugin command: %q", args, stdout.String())
		}
	}
	for _, args := range [][]string{{"help", "plugin"}, {"plugin", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test-version"); code != 2 {
			t.Fatalf("Run(%q) code = %d, want 2", args, code)
		}
		if stdout.Len() != 0 || strings.Contains(stderr.String(), "buildkite-gha plugin") {
			t.Fatalf("Run(%q) exposed plugin help: stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

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
		if received.Event != "pipelines:buildkite_gha:command_completed" || received.Properties.Command != test.wantCommand || received.Properties.Outcome != "usage_error" || received.Properties.ClientVersion != "test-version" || received.Properties.DurationMS < 0 || received.Properties.FailurePhase != "configuration" || received.Properties.FailureCode != "unknown" {
			t.Fatalf("run(%q) event = %#v", test.args, received)
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

func TestRevisionedDevelopmentVersionKeepsPlanCompatibilityVersion(t *testing.T) {
	if got := commandVersion("dev+0123456789ab"); got != "dev" {
		t.Fatalf("commandVersion() = %q, want dev", got)
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

func TestPluginTelemetryReportsUnprovenActionRuntime(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"action.yml": "on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n",
	})
	actionPath := filepath.Join(repository, ".github", "actions", "local")
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("runs:\n  using: node24\n  main: main.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "main.js"), []byte("console.log('local action')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE_GHA_NODE20", writeFakeNode(t, repository, 20))
	t.Setenv("BUILDKITE_GHA_NODE24", writeFakeNode(t, repository, 24))

	events := make(chan telemetry.Properties, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received struct {
			Properties telemetry.Properties `json:"properties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode telemetry event: %v", err)
		}
		events <- received.Properties
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")

	configuration, err := json.Marshal(map[string]any{"workflow": filepath.Join(".github", "workflows", "action.yml")})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "telemetry-action-importer")

	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	properties := <-events
	if properties.Command != telemetry.CommandPluginImport || properties.Outcome != telemetry.OutcomeSuccess {
		t.Fatalf("event = %#v", properties)
	}
	want := []telemetry.Diagnostic{{Code: "W_ACTION_RUNTIME_UNKNOWN", Severity: telemetry.SeverityWarning}}
	if !reflect.DeepEqual(properties.Diagnostics, want) {
		t.Fatalf("diagnostics = %#v, want %#v", properties.Diagnostics, want)
	}
	for _, command := range runner.commands {
		if len(command.args) != 0 && command.args[0] == "annotate" {
			t.Fatalf("unproven action runtime annotated the build: %#v", command.args)
		}
	}
}

func TestPluginRequiresConfigurationWithoutSideEffects(t *testing.T) {
	requireImporterHost(t)
	t.Setenv(pluginConfigurationEnvironment, "")
	t.Setenv("BUILDKITE_PLUGIN_GITHUB_ACTIONS_WORKFLOW", "legacy.yml")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 2 {
		t.Fatalf("run() code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), pluginConfigurationEnvironment+" is required") || len(runner.commands) != 0 {
		t.Fatalf("stderr = %q, commands = %#v", stderr.String(), runner.commands)
	}
}

func TestPluginRejectsUnknownAndNonBooleanConfigurationWithoutSideEffects(t *testing.T) {
	requireImporterHost(t)
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "unknown field", source: `{"workflow":"ci.yml","unknown":true}`, want: "unknown field"},
		{name: "non-boolean experiment", source: `{"workflow":"ci.yml","experimental-runner-user":"true"}`, want: "must be a boolean"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(pluginConfigurationEnvironment, test.source)
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 2 {
				t.Fatalf("run() code = %d, want 2; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.want) || stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("invalid configuration reached upload: stdout = %q, stderr = %q, commands = %#v, uploads = %#v", stdout.String(), stderr.String(), runner.commands, runner.uploaded)
			}
		})
	}
}

func TestParsePluginConfiguration(t *testing.T) {
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	configuration, err := parsePluginConfiguration(`{
  "workflows": [".github/workflows/ci.yml", ".github/workflows/release.yml"],
  "version": "0.8.0",
  "source-ref": "0123456789abcdef0123456789abcdef01234567",
  "minimum-release-age": "24h",
  "experimental-runner-user": true,
  "runners": [
    {"runs-on":"ubuntu-latest","queue":"hosted","image":"` + image + `"},
    {"runs-on":"macos-14","queue":"macos-sonoma-arm64"}
  ]
}`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(configuration.Workflows, []string{".github/workflows/ci.yml", ".github/workflows/release.yml"}) || !configuration.ExperimentalRunnerUser || len(configuration.runnerTargets) != 2 {
		t.Fatalf("configuration = %#v", configuration)
	}
	if got := configuration.runnerTargets["ubuntu-latest"]; got != (compiler.RunnerTarget{Queue: "hosted", Platform: compiler.PlatformLinuxAMD64, Image: image}) {
		t.Fatalf("Linux target = %#v", got)
	}
	if got := configuration.runnerTargets["macos-14"]; got != (compiler.RunnerTarget{Queue: "macos-sonoma-arm64", Platform: compiler.PlatformDarwinARM64}) {
		t.Fatalf("Darwin target = %#v", got)
	}
	minimal, err := parsePluginConfiguration(`{"workflow":"workflow.yml"}`)
	if err != nil || !slices.Equal(minimal.Workflows, []string{"workflow.yml"}) || !minimal.ExperimentalRunnerUser || len(minimal.runnerTargets) != 0 {
		t.Fatalf("minimal configuration = %#v, %v", minimal, err)
	}
	disabled, err := parsePluginConfiguration(`{"workflow":"workflow.yml","experimental-runner-user":false}`)
	if err != nil || disabled.ExperimentalRunnerUser {
		t.Fatalf("disabled runner user configuration = %#v, %v", disabled, err)
	}

	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "malformed", source: `{`, want: "decode"},
		{name: "missing workflow selection", source: `{}`, want: "workflow or workflows is required"},
		{name: "duplicate workflow", source: `{"workflow":"one.yml","workflow":"two.yml"}`, want: "duplicate object key"},
		{name: "both workflow fields", source: `{"workflow":"one.yml","workflows":"two.yml"}`, want: "mutually exclusive"},
		{name: "empty workflow", source: `{"workflow":""}`, want: "workflow must be a non-empty string"},
		{name: "workflows string", source: `{"workflows":"one.yml"}`, want: "workflows must be a non-empty array"},
		{name: "empty workflows array", source: `{"workflows":[]}`, want: "workflows must be a non-empty array"},
		{name: "non-string workflow entry", source: `{"workflows":["one.yml",null]}`, want: "workflows entry 1 must be a non-empty string"},
		{name: "empty workflow entry", source: `{"workflows":["one.yml",""]}`, want: "workflows entry 1 must be a non-empty string"},
		{name: "unknown top-level field", source: `{"workflow":"ci.yml","runnerss":[]}`, want: "unknown field"},
		{name: "retired source acquisition field", source: `{"workflow":"ci.yml","buildkite-gha-source-ref":"latest"}`, want: "unknown field"},
		{name: "string experimental runner user", source: `{"workflow":"ci.yml","experimental-runner-user":"true"}`, want: "must be a boolean"},
		{name: "numeric experimental runner user", source: `{"workflow":"ci.yml","experimental-runner-user":1}`, want: "must be a boolean"},
		{name: "null experimental runner user", source: `{"workflow":"ci.yml","experimental-runner-user":null}`, want: "must be a boolean"},
		{name: "null runners", source: `{"workflow":"ci.yml","runners":null}`, want: "non-empty array"},
		{name: "empty runners", source: `{"workflow":"ci.yml","runners":[]}`, want: "non-empty array"},
		{name: "unknown runner field", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","extra":true}]}`, want: "unknown field"},
		{name: "case alias runner field", source: `{"workflow":"ci.yml","runners":[{"Runs-On":"ubuntu-latest","queue":"hosted"}]}`, want: "unknown field"},
		{name: "duplicate runner field", source: `{"workflow":"ci.yml","runners":[{"runs-on":"windows-latest","runs-on":"ubuntu-latest","queue":"hosted"}]}`, want: "duplicate object key"},
		{name: "duplicate runner", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"one"},{"runs-on":"UBUNTU-LATEST","queue":"two"}]}`, want: "only be configured once"},
		{name: "missing queue", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest"}]}`, want: "queue must be a string"},
		{name: "empty image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","image":""}]}`, want: "immutable registry"},
		{name: "null image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","image":null}]}`, want: "immutable registry"},
		{name: "mutable image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","image":"ubuntu:latest"}]}`, want: "immutable registry"},
		{name: "Darwin image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"macos-14","queue":"macos","image":"` + image + `"}]}`, want: "unsupported on darwin/arm64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parsePluginConfiguration(test.source); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parsePluginConfiguration() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfiguredLinuxRunnerTargetsDefaultHostedToolchainImages(t *testing.T) {
	for _, test := range []struct {
		label string
		image string
	}{
		{label: "Ubuntu-Latest", image: defaultNobleRunnerImage},
		{label: "ubuntu-24.04", image: defaultNobleRunnerImage},
		{label: "ubuntu-22.04", image: defaultJammyRunnerImage},
	} {
		t.Run(test.label, func(t *testing.T) {
			canonical, target, err := configuredRunnerTarget(test.label, "hosted", "")
			if err != nil {
				t.Fatal(err)
			}
			if canonical != strings.ToLower(test.label) || target != (compiler.RunnerTarget{Queue: "hosted", Platform: compiler.PlatformLinuxAMD64, Image: test.image}) {
				t.Fatalf("configuredRunnerTarget() = %q, %#v", canonical, target)
			}
		})
	}

	override := "registry.example.com/custom@sha256:" + strings.Repeat("0", 64)
	_, target, err := configuredRunnerTarget("ubuntu-latest", "hosted", override)
	if err != nil {
		t.Fatal(err)
	}
	if target.Image != override {
		t.Fatalf("explicit image = %q, want %q", target.Image, override)
	}
}

func TestHostedRunnerTargetsContainOnlyHostedGuarantees(t *testing.T) {
	targets := hostedRunnerTargets()
	labels := make([]string, 0, len(targets))
	for label := range targets {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	want := []string{"macos-latest", "ubuntu-22.04", "ubuntu-24.04", "ubuntu-latest"}
	if !slices.Equal(labels, want) {
		t.Fatalf("hosted runner labels = %q, want %q", labels, want)
	}
	if got := targets["macos-latest"]; got != (compiler.RunnerTarget{Queue: defaultMacOSRunnerQueue, Platform: compiler.PlatformDarwinARM64}) {
		t.Fatalf("macos-latest target = %#v", got)
	}
	for _, label := range []string{"macos-14", "macos-15", "ubuntu-24.04-arm", "windows-latest"} {
		if _, ok := targets[label]; ok {
			t.Errorf("hosted runner preset unexpectedly contains %q", label)
		}
	}

	canonical, target, err := configuredRunnerTarget("macOS-15", "organization-macos", "")
	if err != nil || canonical != "macos-15" || target != (compiler.RunnerTarget{Queue: "organization-macos", Platform: compiler.PlatformDarwinARM64}) {
		t.Fatalf("organization macOS target = %q, %#v, %v", canonical, target, err)
	}
}

func TestImporterPlatform(t *testing.T) {
	for _, test := range []struct {
		goos, goarch string
		want         compiler.Platform
	}{
		{goos: "linux", goarch: "amd64", want: compiler.PlatformLinuxAMD64},
		{goos: "darwin", goarch: "arm64", want: compiler.PlatformDarwinARM64},
	} {
		got, err := importerPlatform(test.goos, test.goarch)
		if err != nil || got != test.want {
			t.Fatalf("importerPlatform(%q, %q) = %s, %v", test.goos, test.goarch, got, err)
		}
	}
	if _, err := importerPlatform("linux", "arm64"); err == nil || !strings.Contains(err.Error(), "linux/amd64 or darwin/arm64") {
		t.Fatalf("unsupported importer error = %v", err)
	}
}

func TestUploadRejectsUnsupportedImporterBeforeProcessing(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "linux.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  linux:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo linux\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "darwin-importer")
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "Linux graph", args: []string{workflowPath}},
		{name: "explicit runtimes", args: []string{
			"--runtime-distribution", "linux/amd64=/tmp/buildkite-gha-linux",
			"--runtime-distribution", "darwin/arm64=/tmp/buildkite-gha-darwin",
			workflowPath,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := uploadFromPlatform("linux", "arm64", test.args, &stdout, &stderr, "dev", transport.Agent{Runner: runner}); code != 1 {
				t.Fatalf("uploadFromPlatform() code = %d, want 1", code)
			}
			if got := stderr.String(); got != "buildkite-gha: upload: importer requires linux/amd64 or darwin/arm64, running on linux/arm64\n" {
				t.Fatalf("stderr = %q", got)
			}
			if stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("unsupported importer performed work: stdout = %q, commands = %d, uploads = %d", stdout.String(), len(runner.commands), len(runner.uploaded))
			}
		})
	}
}

func TestDarwinUploadRequiresLinuxDistributionForLinuxWorkflow(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "linux.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  linux:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo linux\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "darwin-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	code := uploadFromPlatform("darwin", "arm64", []string{"--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", transport.Agent{Runner: runner})
	if code != 1 || !strings.Contains(stderr.String(), "runtime distribution for linux/amd64 is required by the selected workflows") {
		t.Fatalf("uploadFromPlatform() = %d, stderr = %q", code, stderr.String())
	}
	if len(runner.uploaded) != 0 {
		t.Fatalf("missing runtime reached artifact upload: %#v", runner.uploaded)
	}
}

func TestPluginUsesJSONConfigurationAndOnlyRequiredRuntime(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(map[string]any{
		"workflow":                 workflowPath,
		"version":                  "0.8.0",
		"experimental-runner-user": true,
		"runners": []map[string]string{
			{"runs-on": "ubuntu-latest", "queue": "hosted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	t.Setenv("BUILDKITE_PLUGIN_GITHUB_ACTIONS_WORKFLOW", "ignored.yml")
	_ = executable // The plugin uses the already-opened running executable, not an environment path.
	setCLIPluginBuildkiteEnvironment(t, "plugin-linux")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 3 jobs") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Count(strings.TrimSpace(stdout.String()), "\n") != 0 {
		t.Fatalf("plugin success output is not a one-line summary: %q", stdout.String())
	}
	for _, verbose := range []string{"Schema:", "- Workflow parsing:", "  job ", "  action "} {
		if strings.Contains(stdout.String(), verbose) {
			t.Fatalf("plugin success output contains verbose inventory %q: %q", verbose, stdout.String())
		}
	}
	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Image   string            `yaml:"image"`
				Agents  map[string]string `yaml:"agents"`
				Command string            `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("workflow groups = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps[0].Steps {
		if step.Agents["queue"] != "hosted" || step.Image != defaultNobleRunnerImage || !strings.Contains(step.Command, "--hosted-tool-cache") || !strings.Contains(step.Command, "useradd --create-home") || !strings.Contains(step.Command, "sudo -n --preserve-env --user runner") {
			t.Fatalf("plugin profile was not applied: %#v", step)
		}
	}
}

func TestPluginAdmissionFailurePrintsDiagnosticsBeforeSummaryAndAnnotatesDetails(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"secret.yml": "name: Secret\non: push\njobs:\n  secret:\n    permissions:\n      contents: read\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets.GITHUB_TOKEN }}\n    steps: [{run: true}]\n",
	})
	t.Chdir(repository)
	configuration, err := json.Marshal(map[string]any{"workflow": ".github/workflows/secret.yml"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-admission-failure")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 1 {
		t.Fatalf("run() code = %d, want 1; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	output := stderr.String()
	diagnostic := strings.Index(output, "x [E_PROFILE]")
	summary := strings.Index(output, "Compilation: compilable. Admission: not-admitted.")
	if !strings.HasPrefix(output, "^^^ +++\n") || diagnostic == -1 || summary <= diagnostic ||
		!strings.Contains(output, "workflow=.github/workflows/secret.yml") ||
		!strings.Contains(output, "stage=hosted-profile-admission") ||
		!strings.Contains(output, "job=secret") {
		t.Fatalf("plugin failure output = %q", output)
	}
	for _, verbose := range []string{"Schema:", "- Workflow parsing:", "  job secret:"} {
		if strings.Contains(output, verbose) {
			t.Fatalf("plugin failure output contains verbose inventory %q: %q", verbose, output)
		}
	}
	var annotation *cliCommand
	for i := range runner.commands {
		if len(runner.commands[i].args) != 0 && runner.commands[i].args[0] == "annotate" {
			annotation = &runner.commands[i]
		}
	}
	if annotation == nil || !strings.Contains(string(annotation.stdin), "job-level permissions are unsupported") ||
		!strings.Contains(string(annotation.stdin), `href="https://github.com/buildkite/buildkite-gha/blob/0123456789abcdef0123456789abcdef01234567/.github/workflows/secret.yml#L`) {
		t.Fatalf("admission failure annotation = %#v", annotation)
	}
}

func TestPluginUploadsPluralWorkflowList(t *testing.T) {
	requireImporterHost(t)
	configuration, err := json.Marshal(map[string]any{
		"workflows": []string{
			filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml"),
			filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "concurrent.yml"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-workflows")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 5 jobs from 2 workflows") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	var pipeline struct {
		Steps []struct {
			Group string `yaml:"group"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 2 {
		t.Fatalf("workflow groups = %#v", pipeline.Steps)
	}
}

func TestPluginRejectsNonExplicitWorkflowSelectors(t *testing.T) {
	requireImporterHost(t)
	workflowDirectory := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows")
	for _, test := range []struct {
		name      string
		field     string
		workflows any
		want      string
	}{
		{name: "all shorthand", field: "workflow", workflows: "*", want: "explicit paths"},
		{name: "string glob", field: "workflow", workflows: filepath.Join(workflowDirectory, "*.yml"), want: "explicit paths"},
		{name: "array glob", field: "workflows", workflows: []string{filepath.Join(workflowDirectory, "*.yml")}, want: "explicit paths"},
		{name: "directory", field: "workflow", workflows: workflowDirectory, want: "does not name a regular tracked file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration, err := json.Marshal(map[string]any{test.field: test.workflows})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(pluginConfigurationEnvironment, string(configuration))
			setCLIPluginBuildkiteEnvironment(t, "plugin-workflows-explicit")
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
			}
			if stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("invalid workflow selection reached Buildkite: stdout %q, commands %#v, uploads %#v", stdout.String(), runner.commands, runner.uploaded)
			}
		})
	}
}

func TestPluginPublishesMixedRuntimeDistributions(t *testing.T) {
	requireImporterHost(t)
	const fullCommit = "0123456789abcdef0123456789abcdef01234567"
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"mixed.yml": "on: push\njobs:\n  linux:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo linux\n  macos:\n    needs: linux\n    runs-on: macos-15\n    steps:\n      - run: echo macos\n",
	})
	t.Chdir(repository)
	workflowPath := filepath.Join(".github", "workflows", "mixed.yml")
	linuxPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	darwinContents := []byte{
		0xcf, 0xfa, 0xed, 0xfe,
		0x0c, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	darwinPath := filepath.Join(t.TempDir(), "buildkite-gha-darwin")
	if err := os.WriteFile(darwinPath, darwinContents, 0o700); err != nil {
		t.Fatal(err)
	}
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	configuration, err := json.Marshal(map[string]any{
		"workflow": workflowPath,
		"runners": []map[string]any{
			{"runs-on": "ubuntu-latest", "queue": "linux", "image": image},
			{"runs-on": "macos-15", "queue": "macos"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	_ = linuxPath // The plugin's Linux runtime is always its running executable.
	t.Setenv(pluginDevDarwinRuntimeEnvironment, darwinPath)
	setCLIPluginBuildkiteEnvironment(t, "plugin-mixed")
	t.Setenv("BUILDKITE_COMMIT", "HEAD")
	runner := &cliCaptureRunner{gitOutput: []byte(fullCommit + "\n")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	darwinDigest := transport.Digest(darwinContents)
	planRuntimes := map[string]string{}
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		if job.Event.SHA != fullCommit {
			t.Fatalf("plan event SHA = %q, want normalized commit %q", job.Event.SHA, fullCommit)
		}
		planRuntimes[job.Workflow.LogicalJobID] = job.RuntimeDistributionDigest()
	}
	if planRuntimes["linux"] != cliTestRuntimeDigest() || planRuntimes["macos"] != darwinDigest {
		t.Fatalf("plan runtime digests = %#v", planRuntimes)
	}
	distributionPath, err := buildkitepipeline.DistributionPath(darwinDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runner.uploaded[distributionPath], darwinContents) {
		t.Fatal("Darwin runtime distribution was not published")
	}
	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Key     string            `yaml:"key"`
				Image   string            `yaml:"image"`
				Agents  map[string]string `yaml:"agents"`
				Command string            `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("workflow groups = %#v", pipeline.Steps)
	}
	steps := make(map[string]struct {
		Image   string
		Queue   string
		Command string
	}, len(pipeline.Steps[0].Steps))
	for _, step := range pipeline.Steps[0].Steps {
		steps[step.Key] = struct {
			Image   string
			Queue   string
			Command string
		}{Image: step.Image, Queue: step.Agents["queue"], Command: step.Command}
	}
	var linux, macos struct {
		Image   string
		Queue   string
		Command string
	}
	for key, step := range steps {
		switch {
		case strings.HasSuffix(key, "-linux"):
			linux = step
		case strings.HasSuffix(key, "-macos"):
			macos = step
		}
	}
	if linux.Queue != "linux" || linux.Image != image || !strings.Contains(linux.Command, "--hosted-tool-cache") || !strings.Contains(linux.Command, strings.TrimPrefix(cliTestRuntimeDigest(), "sha256:")) {
		t.Fatalf("Linux pipeline step = %#v", linux)
	}
	if macos.Queue != "macos" || macos.Image != "" || strings.Contains(macos.Command, "--hosted-tool-cache") || !strings.Contains(macos.Command, strings.TrimPrefix(darwinDigest, "sha256:")) {
		t.Fatalf("Darwin pipeline step = %#v", macos)
	}
}

func TestPluginRunsNativelyOnDarwinARM64(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native Darwin/arm64 importer test")
	}
	const fullCommit = "0123456789abcdef0123456789abcdef01234567"
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"macos.yml": "on: push\njobs:\n  macos:\n    runs-on: macos-15\n    steps:\n      - run: echo macos\n",
	})
	t.Chdir(repository)
	configuration, err := json.Marshal(map[string]any{
		"workflow": filepath.Join(".github", "workflows", "macos.yml"),
		"runners":  []map[string]string{{"runs-on": "macos-15", "queue": "macos"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-darwin-importer")
	t.Setenv("BUILDKITE_COMMIT", "HEAD")
	runner := &cliCaptureRunner{gitOutput: []byte(fullCommit + "\n")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 1 jobs") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		if job.RuntimeDistributionDigest() != cliTestRuntimeDigest() {
			t.Fatalf("Darwin job runtime = %q, want importer %q", job.RuntimeDistributionDigest(), cliTestRuntimeDigest())
		}
		return
	}
	t.Fatal("Darwin importer did not upload a job plan")
}

func TestPluginDevCounterpartInjectionIsRequiredAndReleaseRejectsIt(t *testing.T) {
	linux := runtimeDistribution{contents: []byte("linux"), digest: "sha256:linux"}
	required := map[compiler.Platform]bool{compiler.PlatformDarwinARM64: true}
	t.Setenv(pluginDevDarwinRuntimeEnvironment, "")
	if _, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(context.Background(), required, compiler.PlatformLinuxAMD64, linux); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("dev acquisition error = %v", err)
	}
	darwin := runtimeDistribution{contents: []byte("darwin"), digest: "sha256:darwin"}
	if _, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(context.Background(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformDarwinARM64, darwin); err == nil || !strings.Contains(err.Error(), pluginDevLinuxRuntimeEnvironment) {
		t.Fatalf("Darwin-hosted dev acquisition error = %v", err)
	}
	t.Setenv(pluginDevDarwinRuntimeEnvironment, filepath.Join(t.TempDir(), "injected"))
	if _, err := (&pluginRuntimeAcquisition{version: "1.2.3"}).acquire(context.Background(), map[compiler.Platform]bool{}, compiler.PlatformLinuxAMD64, linux); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("release injection error = %v", err)
	}
}

func TestPluginAcquiresVerifiedLinuxRuntimeForDarwinHost(t *testing.T) {
	linux := pluginTestLinuxExecutable()
	archive := pluginTestArchive(t, linux, false)
	archiveDigest := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch filepath.Base(request.URL.Path) {
		case "checksums.txt":
			_, _ = fmt.Fprintf(response, "%x  %s\n", archiveDigest, pluginLinuxAsset)
		case pluginLinuxAsset:
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	distribution, err := acquirePluginRuntime(context.Background(), "1.2.3", compiler.PlatformLinuxAMD64, server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if distribution.digest != transport.Digest(linux) || !bytes.Equal(distribution.contents, linux) {
		t.Fatalf("Linux distribution = %q, %d bytes", distribution.digest, len(distribution.contents))
	}
}

func pluginTestLinuxExecutable() []byte {
	contents := make([]byte, 64)
	copy(contents, []byte("\x7fELF"))
	contents[4] = 2                                    // ELFCLASS64
	contents[5] = 1                                    // ELFDATA2LSB
	contents[6] = 1                                    // EV_CURRENT
	binary.LittleEndian.PutUint16(contents[16:18], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(contents[18:20], 62) // EM_X86_64
	binary.LittleEndian.PutUint32(contents[20:24], 1)  // EV_CURRENT
	binary.LittleEndian.PutUint16(contents[52:54], 64) // ELF header size
	return contents
}

func TestPluginAcquisitionIsLazyAndBindsVerifiedDarwinContents(t *testing.T) {
	linux := runtimeDistribution{contents: []byte("running executable"), digest: "sha256:running"}
	t.Setenv(pluginDevDarwinRuntimeEnvironment, "")
	got, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(context.Background(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformLinuxAMD64, linux)
	if err != nil || got[compiler.PlatformLinuxAMD64].digest != linux.digest {
		t.Fatalf("Linux-only acquisition = %#v, %v", got, err)
	}

	darwin := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	archive := pluginTestArchive(t, darwin, false)
	archiveDigest := sha256.Sum256(archive)
	requests := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests[request.URL.Path]++
		switch filepath.Base(request.URL.Path) {
		case "checksums.txt":
			_, _ = fmt.Fprintf(response, "%x  %s\n", archiveDigest, pluginDarwinAsset)
		case pluginDarwinAsset:
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	cache := filepath.Join(canonicalTempDir(t), pluginDarwinAsset)
	distribution, err := acquirePluginRuntime(context.Background(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, cache)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := transport.Digest(darwin)
	if distribution.digest != wantDigest || !bytes.Equal(distribution.contents, darwin) {
		t.Fatalf("Darwin distribution = %q, %x", distribution.digest, distribution.contents)
	}
	if _, err := acquirePluginRuntime(context.Background(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, cache); err != nil {
		t.Fatal(err)
	}
	if requests["/v1.2.3/checksums.txt"] != 2 || requests["/v1.2.3/"+pluginDarwinAsset] != 1 {
		t.Fatalf("requests = %#v; verified cache did not avoid an archive download", requests)
	}
	if err := os.WriteFile(cache, []byte("untrusted cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePluginRuntime(context.Background(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, cache); err != nil {
		t.Fatal(err)
	}
	if requests["/v1.2.3/checksums.txt"] != 3 || requests["/v1.2.3/"+pluginDarwinAsset] != 2 {
		t.Fatalf("requests = %#v; cache was trusted without live checksum or invalid cache was used", requests)
	}
}

func TestPluginDarwinRejectsChecksumArchiveAndBinaryFailures(t *testing.T) {
	validBinary := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	archive := pluginTestArchive(t, validBinary, false)
	digest := sha256.Sum256(archive)
	if _, err := pluginAssetChecksum([]byte(fmt.Sprintf("%x  %s\n%x  %s\n", digest, pluginDarwinAsset, digest, pluginDarwinAsset)), pluginDarwinAsset); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("duplicate checksum error = %v", err)
	}
	if _, err := extractPluginRuntime(pluginTestArchive(t, validBinary, true), compiler.PlatformDarwinARM64); err == nil || !strings.Contains(err.Error(), "unexpected member") {
		t.Fatalf("unsafe archive error = %v", err)
	}
	wrong := pluginTestArchive(t, []byte("not Mach-O"), false)
	wrongDigest := sha256.Sum256(wrong)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if filepath.Base(request.URL.Path) == "checksums.txt" {
			_, _ = fmt.Fprintf(response, "%x  %s\n", wrongDigest, pluginDarwinAsset)
			return
		}
		_, _ = response.Write(wrong)
	}))
	defer server.Close()
	if _, err := acquirePluginRuntime(context.Background(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, ""); err == nil || !strings.Contains(err.Error(), "Mach-O") {
		t.Fatalf("wrong binary error = %v", err)
	}
}

func pluginTestArchive(t *testing.T, executable []byte, extra bool) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range []struct {
		name string
		mode int64
		body []byte
	}{{"buildkite-gha", 0o755, executable}, {"LICENSE", 0o644, []byte("license")}} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: member.mode, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if extra {
		body := []byte("extra")
		if err := tarWriter.WriteHeader(&tar.Header{Name: "../extra", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tarWriter.Write(body)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestNormalizePluginCommit(t *testing.T) {
	const fullCommit = "0123456789abcdef0123456789abcdef01234567"
	t.Run("preserves valid full commit", func(t *testing.T) {
		runner := &cliCaptureRunner{}
		setCalls := 0
		err := normalizePluginCommit(context.Background(), func(string) string { return fullCommit }, func(string, string) error {
			setCalls++
			return nil
		}, runner)
		if err != nil || setCalls != 0 || len(runner.commands) != 0 {
			t.Fatalf("normalizePluginCommit() error = %v, set calls = %d, commands = %#v", err, setCalls, runner.commands)
		}
	})
	t.Run("resolves symbolic commit from HEAD", func(t *testing.T) {
		runner := &cliCaptureRunner{gitOutput: []byte(fullCommit + "\n")}
		name, value := "", ""
		err := normalizePluginCommit(context.Background(), func(string) string { return "HEAD" }, func(gotName, gotValue string) error {
			name, value = gotName, gotValue
			return nil
		}, runner)
		if err != nil || name != "BUILDKITE_COMMIT" || value != fullCommit {
			t.Fatalf("normalizePluginCommit() = %q, %q, %v", name, value, err)
		}
		if len(runner.commands) != 1 || runner.commands[0].name != "git" || !slices.Equal(runner.commands[0].args, []string{"rev-parse", "HEAD"}) {
			t.Fatalf("commands = %#v, want exact git rev-parse HEAD invocation", runner.commands)
		}
	})
	t.Run("propagates resolution failure", func(t *testing.T) {
		runner := &cliCaptureRunner{gitErr: errors.New("no checkout")}
		err := normalizePluginCommit(context.Background(), func(string) string { return "HEAD" }, os.Setenv, runner)
		if err == nil || !strings.Contains(err.Error(), "resolve BUILDKITE_COMMIT from checked-out HEAD: no checkout") {
			t.Fatalf("normalizePluginCommit() error = %v", err)
		}
	})
}

func setCLIPluginBuildkiteEnvironment(t *testing.T, stepKey string) {
	t.Helper()
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", stepKey)
	t.Setenv("BUILDKITE_REPO", "https://github.com/buildkite/buildkite-gha")
	t.Setenv("BUILDKITE_COMMIT", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("BUILDKITE_BRANCH", "main")
	t.Setenv("BUILDKITE_TAG", "")
	t.Setenv("BUILDKITE_PULL_REQUEST", "false")
	t.Setenv("BUILDKITE_SOURCE", "webhook")
}

func TestRunValidateAndCompile(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")

	t.Run("validate", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		if want := "2 logical jobs and 3 static instances"; !strings.Contains(stdout.String(), want) {
			t.Fatalf("Run() stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("validate checks every trigger without an event", func(t *testing.T) {
		tests := []struct {
			name, trigger, want string
		}{
			{name: "unsupported event", trigger: "issues", want: `unsupported GitHub trigger event "issues"`},
			{name: "malformed path filter", trigger: "push:\n    paths: ['!src/**']", want: "must follow a positive pattern"},
			{name: "mixed branch filters", trigger: "push:\n    branches: [main]\n    branches-ignore: [release]", want: "include and ignore filters cannot be combined"},
			{name: "pull request tag filter", trigger: "pull_request:\n    tags: [v1]", want: "pull_request tag filters are unsupported"},
			{name: "pull request activity", trigger: "pull_request:\n    types: [auto_merge_enabled, submitted]", want: `activity type "submitted" cannot be mapped exactly`},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				workflow := filepath.Join(t.TempDir(), "trigger.yml")
				source := "on:\n  " + test.trigger + "\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
				if err := os.WriteFile(workflow, []byte(source), 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				if code := Run([]string{"validate", "--format", "json", workflow}, &stdout, &stderr, "dev"); code != 1 {
					t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
				}
				var report compatibility.ProcessingReport
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				pipelineFailed := false
				for _, stage := range report.Stages {
					pipelineFailed = pipelineFailed || stage.ID == string(compiler.StagePipeline) && stage.Result == compatibility.Failed
				}
				if report.Result != "incompatible" || !pipelineFailed || len(report.Diagnostics) != 1 || report.Diagnostics[0].Stage != string(compiler.StagePipeline) || !strings.Contains(report.Diagnostics[0].Message+report.Diagnostics[0].Detail, test.want) {
					t.Fatalf("report = %#v, want trigger failure containing %q", report, test.want)
				}
			})
		}
	})

	t.Run("bare validation accepts supported path filters", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "trigger.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  push:\n    paths: [docs/**]\n  pull_request:\n    paths: [src/**]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflow}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "compilable" || len(report.Diagnostics) != 0 {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("validate json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "compilable" || report.Instances != 3 {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("validate json blocker", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "dynamic.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  prepare:\n    runs-on: ubuntu-latest\n    outputs:\n      matrix: ${{ steps.matrix.outputs.value }}\n    steps:\n      - id: matrix\n        run: true\n  build:\n    needs: prepare\n    runs-on: ubuntu-latest\n    strategy:\n      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}\n    steps:\n      - run: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflow}, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodeMatrixInvalid {
			t.Fatalf("report = %#v", report)
		}

		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("profile Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var profileReport compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &profileReport); err != nil {
			t.Fatal(err)
		}
		if profileReport.Result != "incompatible" || profileReport.Compile.Result != "incompatible" || profileReport.Admission.Result != "not-evaluated" || len(profileReport.Diagnostics) != 1 || profileReport.Diagnostics[0].Code != compiler.CodeMatrixInvalid {
			t.Fatalf("profile report = %#v", profileReport)
		}
	})

	t.Run("validate hosted profile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflowPath}
		runner := &cliCaptureRunner{}
		if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("profile validation made Buildkite calls: %#v", runner.commands)
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "admitted" || report.Profile != "hosted" || report.Compile.Instances != 3 || report.Admission.Result != "admitted" || len(report.Diagnostics) != 0 {
			t.Fatalf("profile report = %#v", report)
		}
	})

	t.Run("validate hosted profile with generated events", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  push:\n  pull_request:\n  merge_group:\n  workflow_dispatch:\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, event := range []string{"push", "pull_request", "merge_group", "workflow_dispatch", "schedule"} {
			t.Run(event, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				args := []string{"validate", "--profile", "hosted", "--event", event, "--format", "json", workflow}
				if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
					t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
				}
				var report compatibility.ProcessingReport
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				if report.Result != "admitted" || report.Profile != "hosted" || report.Admission.Result != "admitted" {
					t.Fatalf("profile report = %#v", report)
				}
			})
		}
	})

	t.Run("validate hosted profile with all generated events", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  push:\n  pull_request:\n  merge_group:\n  workflow_dispatch:\n  schedule:\n    - cron: '0 0 * * *'\n  workflow_call:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--all-events", "--format", "json", workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReportV3
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Schema != compatibility.ProcessingSchemaV3 || report.Result != "admitted" || report.Status != compatibility.Passed || report.Validation.Result != "compilable" || len(report.Evaluations) != 5 {
			t.Fatalf("aggregate report = %#v", report)
		}
		for i, event := range []string{"push", "pull_request", "merge_group", "workflow_dispatch", "schedule"} {
			if report.Evaluations[i].Event != event || report.Evaluations[i].Source != "generated" || report.Evaluations[i].Report.Result != "admitted" {
				t.Fatalf("evaluation %d = %#v", i, report.Evaluations[i])
			}
		}
	})

	t.Run("validate all events applies hosted runner policy", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  test:\n    runs-on: macOS-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--all-events", "--format", "json", workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReportV3
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "admitted" || report.Validation.Result != "compilable" || len(report.Validation.Diagnostics) != 0 || len(report.Evaluations) != 1 || report.Evaluations[0].Report.Result != "admitted" {
			t.Fatalf("aggregate report = %#v", report)
		}
	})

	t.Run("validate all events stops before admission on a malformed trigger", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  push:\n    paths: ['!src/**']\n  pull_request:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--all-events", "--format", "json", workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReportV3
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Evaluations) != 0 || len(report.Validation.Diagnostics) != 1 || !strings.Contains(report.Validation.Diagnostics[0].Message, "must follow a positive pattern") {
			t.Fatalf("aggregate report = %#v", report)
		}
	})

	t.Run("validate all events leaves supported paths unmeasured", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  push:\n    paths: [docs/**]\n  pull_request:\n    paths: [src/**]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--all-events", "--format", "json", workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReportV3
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "context-required" || report.Validation.Result != "compilable" || len(report.Validation.Diagnostics) != 0 || len(report.Evaluations) != 2 {
			t.Fatalf("aggregate report = %#v", report)
		}
		if report.Evaluations[0].Event != "push" || report.Evaluations[0].Report.Result != "context-required" || report.Evaluations[1].Event != "pull_request" || report.Evaluations[1].Report.Result != "context-required" {
			t.Fatalf("event evaluations = %#v", report.Evaluations)
		}
		for _, evaluation := range report.Evaluations {
			if evaluation.Report.Compile.Result != "compilable" || evaluation.Report.Admission.Result != compatibility.NotEvaluated || len(evaluation.Report.Diagnostics) != 1 || evaluation.Report.Diagnostics[0].Code != compiler.CodeContextRequired || !strings.Contains(evaluation.Report.Diagnostics[0].Message, "verified local git diff") {
				t.Fatalf("%s evaluation = %#v", evaluation.Event, evaluation.Report)
			}
		}
	})

	t.Run("validate generated pull request leaves path filters unmeasured", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  pull_request:\n    paths: [src/**]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--event", "pull_request", "--format", "json", workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "context-required" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodeContextRequired {
			t.Fatalf("processing report = %#v", report)
		}
		if report.Compile.Result != "compilable" || report.Admission.Result != compatibility.NotEvaluated {
			t.Fatalf("processing stages = compile %#v, admission %#v", report.Compile, report.Admission)
		}
	})

	t.Run("validate generated pull request still finds malformed inactive triggers", func(t *testing.T) {
		for _, triggers := range []string{
			"  pull_request:\n    paths: [src/**]\n  push:\n    paths: ['!docs/**']\n",
			"  push:\n    paths: ['!docs/**']\n  pull_request:\n    paths: [src/**]\n",
		} {
			workflow := filepath.Join(t.TempDir(), "events.yml")
			if err := os.WriteFile(workflow, []byte("on:\n"+triggers+"jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			args := []string{"validate", "--profile", "hosted", "--event", "pull_request", "--format", "json", workflow}
			if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
				t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
			}
			var report compatibility.ProcessingReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodePipelineGeneration || !strings.Contains(report.Diagnostics[0].Message+report.Diagnostics[0].Detail, "must follow a positive pattern") {
				t.Fatalf("processing report = %#v", report)
			}
		}
	})

	t.Run("validate generated pull request still finds body incompatibility", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  pull_request:\n    paths: [src/**]\njobs:\n  test:\n    runs-on: windows-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--event", "pull_request", "--format", "json", workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code == compiler.CodeContextRequired || !strings.Contains(report.Diagnostics[0].Message, "Windows") {
			t.Fatalf("processing report = %#v", report)
		}
	})

	t.Run("validate all events keeps mixed push and pull request paths context required", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "events.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  pull_request:\n    paths: [src/**]\n  push:\n    paths: [docs/**]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--all-events", "--format", "json", workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReportV3
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "context-required" || len(report.Evaluations) != 2 || len(report.Validation.Diagnostics) != 0 {
			t.Fatalf("aggregate report = %#v", report)
		}
		for _, evaluation := range report.Evaluations {
			if evaluation.Report.Result != "context-required" {
				t.Fatalf("evaluation = %#v", evaluation)
			}
		}
	})

	t.Run("validate hosted profile applies upload trigger policy", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "issues.yml")
		if err := os.WriteFile(workflow, []byte("on: issues\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodePipelineGeneration || !strings.Contains(report.Diagnostics[0].Message, `unsupported GitHub trigger event "issues"`) {
			t.Fatalf("profile trigger report = %#v", report)
		}
	})

	t.Run("validate hosted profile does not compile a cross-event workflow", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "pull-request.yml")
		if err := os.WriteFile(workflow, []byte("on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "not-applicable" || report.Compile.Result != compatibility.NotEvaluated || report.Admission.Result != compatibility.NotEvaluated || len(report.Diagnostics) != 0 {
			t.Fatalf("cross-event profile report = %#v", report)
		}
	})

	t.Run("validate hosted profile admits macOS-latest alias", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "macos-latest.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  macos:\n    runs-on: macOS-latest\n    steps:\n      - run: echo macos\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "admitted" || report.Admission.Result != "admitted" || len(report.Diagnostics) != 0 {
			t.Fatalf("macos-latest profile report = %#v", report)
		}
	})

	t.Run("validate hosted macOS profile requires upload mapping", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "macos.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  macos:\n    runs-on: macos-15\n    steps:\n      - run: echo macos\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflow}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || report.Compile.Result != "incompatible" || len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, `Runner label "macos-15" has no runner-target mapping`) || !strings.Contains(report.Diagnostics[0].Detail, "Supported runner labels:") {
			t.Fatalf("macOS profile report = %#v", report)
		}
	})

	t.Run("validate profile requires event", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--profile", "hosted", workflowPath}, &stdout, &stderr, "dev"); code != 2 || !strings.Contains(stderr.String(), "use bare validate <workflow> for event-independent syntax and trigger compatibility validation") {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("validate profile importer cannot collide with generated key", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "profile.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  profile:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Result != "admitted" {
			t.Fatalf("profile report = %#v, error = %v", report, err)
		}
	})

	t.Run("compile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"compile", workflowPath, "--event-path", eventPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var pipeline struct {
			Steps []struct {
				Key string `yaml:"key"`
			} `yaml:"steps"`
		}
		if err := yaml.Unmarshal(stdout.Bytes(), &pipeline); err != nil {
			t.Fatalf("compile output is not pipeline YAML: %v", err)
		}
		if len(pipeline.Steps) != 3 {
			t.Fatalf("compiled steps = %d, want 3", len(pipeline.Steps))
		}
	})

	t.Run("compile ir json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"compile", "--format", "ir-json", workflowPath, "--event-path", eventPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var ir compiler.IR
		if err := json.Unmarshal(stdout.Bytes(), &ir); err != nil || len(ir.Jobs) != 3 {
			t.Fatalf("compile IR = %#v, error = %v", ir, err)
		}
	})
}

func TestCommandsKeepValidatedRuntimeMatrixIncompatibleWithoutUpload(t *testing.T) {
	workflow := filepath.Join(t.TempDir(), "dynamic.yml")
	if err := os.WriteFile(workflow, []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.matrix.outputs.include }}
    steps:
      - id: matrix
        run: true
  generated:
    needs: producer
    runs-on: ${{ matrix.runs-on }}
    strategy:
      matrix:
        include: ${{ fromJson(needs.producer.outputs.include) }}
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "gha-importer")
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "validate", args: []string{"validate", "--format", "json", workflow}},
		{name: "compile pipeline", args: []string{"compile", "--event-path", eventPath, workflow}},
		{name: "compile IR", args: []string{"compile", "--format", "ir-json", "--event-path", eventPath, workflow}},
		{name: "upload", args: []string{"upload", "--event-path", eventPath, workflow}},
		{name: "upload before event metadata", args: []string{"upload", workflow}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.args[0] == "upload" {
				requireImporterHost(t)
			}
			var stdout, stderr bytes.Buffer
			runner := &cliCaptureRunner{}
			if code := run(test.args, &stdout, &stderr, "dev", runner); code != 1 {
				t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			if len(runner.commands) != 0 {
				t.Fatalf("runtime matrix command made Buildkite calls: %#v", runner.commands)
			}
			output := stdout.String() + stderr.String()
			if !strings.Contains(output, "incompatible") || strings.Contains(output, "runtime_matrices") || strings.Contains(output, compiler.RuntimeMatrixSchemaV1) {
				t.Fatalf("command output = %q", output)
			}
			if test.name != "validate" && stdout.Len() != 0 {
				t.Fatalf("unsafe command wrote stdout = %q", stdout.String())
			}
		})
	}

	invalidWorkflow := filepath.Join(t.TempDir(), "invalid-dynamic.yml")
	if err := os.WriteFile(invalidWorkflow, []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    steps:
      - run: true
  generated:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.missing) }}
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("invalid boundary before event metadata", func(t *testing.T) {
		requireImporterHost(t)
		var stdout, stderr bytes.Buffer
		runner := &cliCaptureRunner{}
		if code := run([]string{"upload", invalidWorkflow}, &stdout, &stderr, "dev", runner); code != 1 {
			t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("invalid runtime matrix boundary made Buildkite calls: %#v", runner.commands)
		}
		if !strings.Contains(stderr.String(), "Result: incompatible") || !strings.Contains(stderr.String(), "[E_MATRIX_INVALID]") {
			t.Fatalf("upload stderr = %q", stderr.String())
		}
	})

	repository := t.TempDir()
	workflowDirectory := filepath.Join(repository, ".github", "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	graphFailureWorkflow := filepath.Join(workflowDirectory, "dynamic-with-graph-failure.yml")
	if err := os.WriteFile(graphFailureWorkflow, []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.matrix.outputs.include }}
    steps:
      - id: matrix
        run: true
  generated:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    steps:
      - run: true
  missing-reusable:
    uses: ./.github/workflows/missing.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("exact boundary survives an earlier graph failure", func(t *testing.T) {
		requireImporterHost(t)
		var stdout, stderr bytes.Buffer
		runner := &cliCaptureRunner{}
		if code := run([]string{"upload", graphFailureWorkflow}, &stdout, &stderr, "dev", runner); code != 1 {
			t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("runtime matrix boundary with graph failure made Buildkite calls: %#v", runner.commands)
		}
		if !strings.Contains(stderr.String(), "Result: incompatible") || !strings.Contains(stderr.String(), "[E_GRAPH_INVALID]") {
			t.Fatalf("upload stderr = %q", stderr.String())
		}
	})

	reusableBoundaryWorkflow := filepath.Join(workflowDirectory, "reusable-boundary-with-graph-failure.yml")
	if err := os.WriteFile(reusableBoundaryWorkflow, []byte(`on: push
jobs:
  a-invalid-reusable:
    uses: ./.github/workflows/not-callable-order.yml
  z-runtime-matrix:
    uses: ./.github/workflows/runtime-matrix.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDirectory, "not-callable-order.yml"), []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDirectory, "runtime-matrix.yml"), []byte(`on: workflow_call
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.matrix.outputs.include }}
    steps:
      - id: matrix
        run: true
  generated:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("reusable boundary discovery does not depend on fail-fast order", func(t *testing.T) {
		requireImporterHost(t)
		var stdout, stderr bytes.Buffer
		runner := &cliCaptureRunner{}
		if code := run([]string{"upload", reusableBoundaryWorkflow}, &stdout, &stderr, "dev", runner); code != 1 {
			t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("reusable runtime matrix boundary with graph failure made Buildkite calls: %#v", runner.commands)
		}
		if !strings.Contains(stderr.String(), "Result: incompatible") || !strings.Contains(stderr.String(), "[E_GRAPH_INVALID]") {
			t.Fatalf("upload stderr = %q", stderr.String())
		}
	})

	sharedBoundaryWorkflow := filepath.Join(workflowDirectory, "shared-boundary-with-depth-failure.yml")
	if err := os.WriteFile(sharedBoundaryWorkflow, []byte(`on: push
jobs:
  a-deep:
    uses: ./.github/workflows/deep-1.yml
  z-shared:
    uses: ./.github/workflows/shared.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, next := range []string{"deep-2.yml", "deep-3.yml", "shared.yml"} {
		name := fmt.Sprintf("deep-%d.yml", i+1)
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), []byte(fmt.Sprintf(`on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/%s
`, next)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workflowDirectory, "shared.yml"), []byte(`on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/runtime-matrix.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("shared boundary is rescanned when reached at a shallower depth", func(t *testing.T) {
		requireImporterHost(t)
		var stdout, stderr bytes.Buffer
		runner := &cliCaptureRunner{}
		if code := run([]string{"upload", sharedBoundaryWorkflow}, &stdout, &stderr, "dev", runner); code != 1 {
			t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("shared runtime matrix boundary with graph failure made Buildkite calls: %#v", runner.commands)
		}
		if !strings.Contains(stderr.String(), "Result: incompatible") || !strings.Contains(stderr.String(), "[E_GRAPH_INVALID]") || !strings.Contains(stderr.String(), "job a-deep: failed") {
			t.Fatalf("upload stderr = %q", stderr.String())
		}
	})

	depthBoundaryWorkflow := filepath.Join(workflowDirectory, "depth-boundary-with-graph-failure.yml")
	if err := os.WriteFile(depthBoundaryWorkflow, []byte(`on: push
jobs:
  a-invalid-reusable:
    uses: ./.github/workflows/not-callable-order.yml
  z-deep:
    uses: ./.github/workflows/depth-1.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, next := range []string{"depth-2.yml", "depth-3.yml", "depth-4.yml", "runtime-matrix.yml"} {
		name := fmt.Sprintf("depth-%d.yml", i+1)
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), []byte(fmt.Sprintf(`on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/%s
`, next)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("depth-limited reusable discovery fails closed before event metadata", func(t *testing.T) {
		requireImporterHost(t)
		var stdout, stderr bytes.Buffer
		runner := &cliCaptureRunner{}
		if code := run([]string{"upload", depthBoundaryWorkflow}, &stdout, &stderr, "dev", runner); code != 1 {
			t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("depth-limited runtime matrix discovery made Buildkite calls: %#v", runner.commands)
		}
		if !strings.Contains(stderr.String(), "Result: incompatible") || !strings.Contains(stderr.String(), "[E_GRAPH_INVALID]") || !strings.Contains(stderr.String(), "job a-invalid-reusable: failed") {
			t.Fatalf("upload stderr = %q", stderr.String())
		}
	})

	malformedBoundaryWorkflow := filepath.Join(workflowDirectory, "malformed-boundary-with-graph-failure.yml")
	if err := os.WriteFile(malformedBoundaryWorkflow, []byte(`on: push
jobs:
  a-not-callable:
    uses: ./.github/workflows/not-callable.yml
  z-malformed:
    uses: ./.github/workflows/malformed-runtime-matrix.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDirectory, "not-callable.yml"), []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDirectory, "malformed-runtime-matrix.yml"), []byte(`on: workflow_call
jobs:
  generated:
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    invalid: [
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("incomplete reusable discovery fails closed before event metadata", func(t *testing.T) {
		requireImporterHost(t)
		var stdout, stderr bytes.Buffer
		runner := &cliCaptureRunner{}
		if code := run([]string{"upload", malformedBoundaryWorkflow}, &stdout, &stderr, "dev", runner); code != 1 {
			t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("incomplete runtime matrix discovery made Buildkite calls: %#v", runner.commands)
		}
		if !strings.Contains(stderr.String(), "Result: incompatible") || !strings.Contains(stderr.String(), "[E_GRAPH_INVALID]") || !strings.Contains(stderr.String(), "job a-not-callable: failed") {
			t.Fatalf("upload stderr = %q", stderr.String())
		}
	})
}

func TestProcessingReportAggregatesIndependentErrorsAndRetainsPartialSuccess(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "incompatible.yml")
	workflow := []byte(`on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.value }}
    steps:
      - id: matrix
        run: true
  good:
    runs-on: ubuntu-latest
    steps:
      - uses: ./missing-action
  bad-matrix:
    needs: prepare
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}
    steps:
      - run: true
  bad-condition:
    runs-on: ubuntu-latest
    steps:
      - if: ${{ unsupported('go.sum') }}
        run: true
  bad-runner:
    runs-on: windows-latest
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != compatibility.Failed || len(report.Stages) != 10 || len(report.Diagnostics) < 3 {
		t.Fatalf("processing report = %#v", report)
	}
	stageResults := map[string]string{}
	for _, stage := range report.Stages {
		stageResults[stage.ID] = stage.Result
	}
	if stageResults[stageMatrix] != compatibility.Failed || stageResults[stageExpressions] != compatibility.Failed || stageResults[stageResolution] != compatibility.NotEvaluated {
		t.Fatalf("stage results = %#v", stageResults)
	}
	foundGoodInstance := false
	for _, job := range report.Jobs {
		if job.ID == "good" && job.Instance != "" && job.Result == compatibility.Passed {
			foundGoodInstance = true
		}
	}
	if !foundGoodInstance || len(report.Actions) != 1 || report.Actions[0].Reference != "./missing-action" {
		t.Fatalf("partial jobs/actions = %#v / %#v", report.Jobs, report.Actions)
	}
}

func TestCompileReportsActionResolutionFailureWithoutPipeline(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "action.yml")
	workflow := []byte("on: push\njobs:\n  first:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./missing-one\n  second:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./missing-two\n")
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"compile", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("compile emitted partial pipeline: %q", stdout.String())
	}
	for _, want := range []string{
		"Immutable action resolution: failed",
		"Job-plan construction: not-evaluated",
		"Pipeline generation: not-evaluated",
		"[E_ACTION_RESOLUTION]",
		"job first/gha-first: passed",
		"job second/gha-second: passed",
		"action ./missing-one (job gha-first, step 1): failed",
		"action ./missing-two (job gha-second, step 1): failed",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestProcessingReportStageAttributionDoesNotDependOnWorkflowPath(t *testing.T) {
	for _, name := range []string{"matrix.yml", "workflow.yml"} {
		t.Run(name, func(t *testing.T) {
			workflowPath := filepath.Join(t.TempDir(), name)
			workflow := []byte("on: push\njobs:\n  test:\n    runs-on: windows-latest\n    steps:\n      - run: true\n")
			if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
				t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
			}
			var report compatibility.ProcessingReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			results := map[string]string{}
			for _, stage := range report.Stages {
				results[stage.ID] = stage.Result
			}
			if results[stageMatrix] != compatibility.Passed || results[stageExpressions] != compatibility.Failed || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodeExpressionInvalid {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestCompileLeavesSkippedRemoteActionResolutionNotEvaluated(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "remote.yml")
	workflow := []byte(`on: push
permissions: {}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: owner/action@v1
      - run: echo '${{ secrets.GITHUB_TOKEN }}'
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"compile", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("compile emitted partial pipeline: %q", stdout.String())
	}
	for _, want := range []string{
		"Immutable action resolution: not-evaluated",
		"Job-plan construction: failed",
		"action owner/action@v1 (job gha-test, step 1): not-evaluated",
		"[E_PLAN_CONSTRUCTION]",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestHostedReportRetainsIndependentActionInvocationResultsThroughWrapper(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "actions.yml")
	actionPath := filepath.Join(root, ".github", "actions", "dynamic")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  duplicate:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/dynamic
        with:
          value: supplied
      - uses: ./.github/actions/dynamic
  missing:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/missing-one
      - uses: ./.github/actions/missing-two
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	action := []byte(`name: dynamic
inputs:
  value:
    default: ${{ github[env.NAME] }}
runs:
  using: node24
  main: main.js
`)
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), action, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "main.js"), []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	args := []string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}
	if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 3 {
		t.Fatalf("diagnostics = %#v, want three independent action failures", report.Diagnostics)
	}
	wantResults := map[int]string{1: compatibility.Passed, 2: compatibility.Failed}
	missingFailures := 0
	for _, result := range report.Actions {
		switch result.Job {
		case "gha-duplicate":
			if result.Result != wantResults[result.Step] {
				t.Fatalf("duplicate action result = %#v", result)
			}
		case "gha-missing":
			if result.Result == compatibility.Failed {
				missingFailures++
			}
		}
	}
	if missingFailures != 2 {
		t.Fatalf("actions = %#v, want both missing invocations failed", report.Actions)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Instance == "" || diagnostic.Step == 0 || diagnostic.Code != compiler.CodeActionResolution || !strings.Contains(diagnostic.Message, `Action "`) || !strings.Contains(diagnostic.Message, "unsupported") {
			t.Fatalf("diagnostic lacks invocation identity: %#v", diagnostic)
		}
	}
}

func TestValidateReportsIndependentWorkflowAndEventSyntaxFailures(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, "invalid.yml")
	eventPath := filepath.Join(root, "event.json")
	if err := os.WriteFile(workflowPath, []byte("jobs: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventPath, []byte("{\"provider\":"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for _, stage := range report.Stages {
		results[stage.ID] = stage.Result
	}
	if results[stageWorkflowParsing] != compatibility.Failed || results[stageEventValidation] != compatibility.Failed || results[stageGraph] != compatibility.NotEvaluated {
		t.Fatalf("stage results = %#v", results)
	}
	if len(report.Diagnostics) != 2 || report.Diagnostics[0].Category != "syntax" || report.Diagnostics[1].Category != "environment" {
		t.Fatalf("diagnostics = %#v", report.Diagnostics)
	}

	stdout.Reset()
	stderr.Reset()
	validEventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	if code := Run([]string{"validate", "--format", "json", "--event-path", validEventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() with valid event code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	results = map[string]string{}
	for _, stage := range report.Stages {
		results[stage.ID] = stage.Result
	}
	if results[stageWorkflowParsing] != compatibility.Failed || results[stageEventValidation] != compatibility.Passed || results[stageGraph] != compatibility.NotEvaluated {
		t.Fatalf("stage results with valid event = %#v", results)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Category != "syntax" {
		t.Fatalf("diagnostics with valid event = %#v", report.Diagnostics)
	}
}

func TestCommandsEmitVersionedReportWhenEventInputCannotBeRead(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, "workflow.yml")
	missingEventPath := filepath.Join(root, "missing-event.json")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  test:\n    runs-on: ${{ github.event.runner }}\n    steps:\n      - run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("validate JSON", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--format", "json", "--event-path", missingEventPath, workflowPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		stages := map[string]string{}
		for _, stage := range report.Stages {
			stages[stage.ID] = stage.Result
		}
		if report.Schema != compatibility.ProcessingSchema || report.Result != "indeterminate" || report.Status != compatibility.Failed || stages[stageWorkflowParsing] != compatibility.Passed || stages[stageEventValidation] != compatibility.NotEvaluated || stages[stageExpressions] != compatibility.NotEvaluated || report.Instances != 0 || len(report.Jobs) != 1 || report.Jobs[0].Result != compatibility.NotEvaluated || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "E_ENVIRONMENT" || report.Diagnostics[0].Stage != "" {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("compile text", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"compile", "--event-path", missingEventPath, workflowPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		for _, want := range []string{"Schema: " + compatibility.ProcessingSchema, "Event validation: not-evaluated", "[E_ENVIRONMENT] event input could not be read"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
		}
	})
}

func TestProcessingReportLeavesDependentsOfFailedMatrixNotEvaluated(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "failed-prerequisite.yml")
	workflow := []byte(`on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.value }}
    steps:
      - id: matrix
        run: true
  upstream:
    needs: prepare
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}
    steps:
      - run: true
  downstream:
    needs: upstream
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	stages := map[string]string{}
	for _, stage := range report.Stages {
		stages[stage.ID] = stage.Result
	}
	if stages[stageGraph] != compatibility.Passed || stages[stageMatrix] != compatibility.Failed {
		t.Fatalf("stages = %#v", stages)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == compiler.CodeGraphInvalid || diagnostic.Job == "downstream" {
			t.Fatalf("cascading graph diagnostic = %#v", diagnostic)
		}
	}
	logical, instance := "", ""
	for _, job := range report.Jobs {
		if job.ID != "downstream" {
			continue
		}
		if job.Instance == "" {
			logical = job.Result
		} else {
			instance = job.Result
		}
	}
	if logical != compatibility.NotEvaluated || instance != compatibility.NotEvaluated {
		t.Fatalf("downstream results = logical %q, instance %q; jobs = %#v", logical, instance, report.Jobs)
	}
}

func TestProcessingReportRetainsEveryExpandedMatrixCandidate(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "matrix.yml")
	workflow := []byte(`on: push
jobs:
  test:
    strategy:
      matrix:
        os: [ubuntu-latest, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	instances := map[string]string{}
	for _, job := range report.Jobs {
		if job.Instance != "" {
			instances[job.Instance] = job.Result
		}
	}
	if report.Instances != 2 || len(instances) != 2 {
		t.Fatalf("instances = %d / %#v, want both matrix candidates", report.Instances, instances)
	}
	failedInstance := ""
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == compiler.CodeExpressionInvalid {
			failedInstance = diagnostic.Instance
		}
	}
	if failedInstance == "" || instances[failedInstance] != compatibility.Failed {
		t.Fatalf("diagnostics/jobs = %#v / %#v", report.Diagnostics, report.Jobs)
	}
	passed := 0
	for _, result := range instances {
		if result == compatibility.Passed {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("instances = %#v, want one passed and one failed", instances)
	}
}

func TestProcessingReportFailsAllInstancesForJobScopedExpressionFailure(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "matrix-concurrency.yml")
	workflow := []byte(`on: push
jobs:
  test:
    strategy:
      max-parallel: 2
      matrix:
        target: [one, two]
    runs-on: ubuntu-latest
    concurrency: deploy-${{ matrix.target }}
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	instances := 0
	for _, job := range report.Jobs {
		if job.ID == "test" && job.Instance != "" {
			instances++
			if job.Result != compatibility.Failed {
				t.Fatalf("matrix instance = %#v, want failed", job)
			}
		}
	}
	if instances != 2 {
		t.Fatalf("matrix instances = %d, want 2; jobs = %#v", instances, report.Jobs)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Job != "test" || report.Diagnostics[0].Instance != "" {
		t.Fatalf("diagnostics = %#v, want one job-scoped finding", report.Diagnostics)
	}
}

func TestProcessingReportRedactsEventDerivedRunnerValues(t *testing.T) {
	const sentinel = "raw-event-secret-8675309"
	root := t.TempDir()
	workflowPath := filepath.Join(root, "event-runner.yml")
	eventPath := filepath.Join(root, "event.json")
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ${{ github.event.runner }}\n    steps:\n      - run: true\n")
	event := []byte(`{"provider":"github","event":"push","repository":{"owner":"owner","name":"repo"},"ref":"refs/heads/main","sha":"1111111111111111111111111111111111111111","actor":"actor","payload":{"runner":"` + sentinel + `"}}`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventPath, event, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), sentinel) || strings.Contains(stderr.String(), sentinel) {
		t.Fatalf("report leaked event payload value: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 1 || strings.Contains(report.Diagnostics[0].Message, sentinel) || strings.Contains(report.Diagnostics[0].Detail, sentinel) || report.Diagnostics[0].Message != "Runner label has no runner-target mapping. Configure a mapping for this label or use a mapped runner label." || report.Diagnostics[0].Detail != "Supported runner labels: ubuntu-22.04, ubuntu-24.04, ubuntu-latest." {
		t.Fatalf("diagnostics = %#v", report.Diagnostics)
	}
}

func TestProcessingReportRedactsEventDerivedMatrixKeys(t *testing.T) {
	const sentinel = "EVENT_SECRET_KEY_8675309"
	root := t.TempDir()
	workflowPath := filepath.Join(root, "event-matrix.yml")
	eventPath := filepath.Join(root, "event.json")
	workflow := []byte("on: push\njobs:\n  test:\n    strategy:\n      matrix: ${{ github.event.matrix }}\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	event := []byte(`{"provider":"github","event":"push","repository":{"owner":"owner","name":"repo"},"ref":"refs/heads/main","sha":"1111111111111111111111111111111111111111","actor":"actor","payload":{"matrix":{"` + sentinel + `":"invalid"}}}`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventPath, event, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), sentinel) || strings.Contains(stderr.String(), sentinel) {
		t.Fatalf("report leaked event payload key: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodeMatrixInvalid || report.Diagnostics[0].Message != "matrix could not be expanded or validated" {
		t.Fatalf("diagnostics = %#v", report.Diagnostics)
	}
}

func TestCompileAggregatesIndependentPlanFailuresWithoutPipeline(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "plans.yml")
	workflow := []byte(`on: push
permissions: {}
jobs:
  first:
    runs-on: ubuntu-latest
    steps:
      - run: echo '${{ secrets.GITHUB_TOKEN }}'
  second:
    runs-on: ubuntu-latest
    steps:
      - run: echo '${{ secrets.GITHUB_TOKEN }}'
  dependent:
    needs: first
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"compile", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("compile emitted partial pipeline: %q", stdout.String())
	}
	if got := strings.Count(stderr.String(), "[E_PLAN_CONSTRUCTION]"); got != 2 {
		t.Fatalf("plan diagnostics = %d, want 2; report = %q", got, stderr.String())
	}
	for _, want := range []string{"job first/gha-first: failed", "job second/gha-second: failed", "job dependent/gha-dependent: not-evaluated", "Pipeline generation: not-evaluated"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("report = %q, want %q", stderr.String(), want)
		}
	}
}

func TestHostedReportAdmitsIndependentContainerJobs(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "containers.yml")
	workflow := []byte(`on: push
jobs:
  first:
    runs-on: ubuntu-latest
    container: alpine:3.20
    steps:
      - run: true
  second:
    runs-on: ubuntu-latest
    container: alpine:3.20
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	args := []string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}
	if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	instances := map[string]string{}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == "E_PROFILE" {
			t.Fatalf("unexpected profile diagnostic: %#v", diagnostic)
		}
	}
	for _, job := range report.Jobs {
		if job.Instance != "" {
			instances[job.Instance] = job.Result
		}
	}
	if report.Result != "admitted" || report.Admission.Result != "admitted" || instances["gha-first"] != compatibility.Passed || instances["gha-second"] != compatibility.Passed {
		t.Fatalf("report = %#v", report)
	}
}

func TestProcessingReportRetainsReusableCalleeJobsAfterFailure(t *testing.T) {
	root := t.TempDir()
	workflowRoot := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflowRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(workflowRoot, "caller.yml")
	calleePath := filepath.Join(workflowRoot, "reusable.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callee := []byte(`on: workflow_call
jobs:
  good:
    runs-on: ubuntu-latest
    steps:
      - run: true
  bad:
    runs-on: windows-latest
    steps:
      - run: true
`)
	if err := os.WriteFile(calleePath, callee, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	logicalBad, instanceBad := false, false
	for _, job := range report.Jobs {
		if job.ID != "call.bad" {
			continue
		}
		if job.Instance == "" && job.Result == compatibility.Failed {
			logicalBad = true
		}
		if job.Instance != "" && job.Result == compatibility.Failed {
			instanceBad = true
		}
	}
	if !logicalBad || !instanceBad {
		t.Fatalf("callee job ledger = %#v", report.Jobs)
	}
}

func TestProcessingReportFailsReusableCallerWhenResolutionFails(t *testing.T) {
	root := t.TempDir()
	workflowRoot := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflowRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(workflowRoot, "caller.yml")
	calleePath := filepath.Join(workflowRoot, "reusable.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  delegated:\n    uses: ./.github/workflows/reusable.yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callee := []byte(`on:
  workflow_call:
    outputs:
      published:
        value: literal
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	if err := os.WriteFile(calleePath, callee, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Jobs) != 1 || report.Jobs[0].ID != "delegated" || report.Jobs[0].Result != compatibility.Failed {
		t.Fatalf("caller job ledger = %#v", report.Jobs)
	}
	if len(report.Diagnostics) != 2 || report.Diagnostics[0].Job != "delegated" || report.Diagnostics[0].Stage != stageGraph || report.Diagnostics[1].Code != "W_REUSABLE_WORKFLOW_TOKEN_USES_ROOT_PERMISSIONS" {
		t.Fatalf("diagnostics = %#v", report.Diagnostics)
	}
}

func TestProcessingReportAggregatesIndependentExpressionChecksPerInstance(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "expressions.yml")
	workflow := []byte(`on: push
jobs:
  test:
    if: ${{ hashFiles('condition') }}
    runs-on: windows-latest
    concurrency: ${{ hashFiles('concurrency') }}
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 3 {
		t.Fatalf("diagnostics = %#v, want condition, runner, and concurrency failures", report.Diagnostics)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code != compiler.CodeExpressionInvalid || diagnostic.Instance != "gha-test" {
			t.Fatalf("diagnostic = %#v", diagnostic)
		}
	}
}

func TestWorkflowConcurrencyCancellationWarnsAndRetainsGate(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "concurrency.yml")
	workflow := []byte(`on: push
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	assertWarning := func(t *testing.T, command, output string) {
		t.Helper()
		for _, want := range []string{
			"buildkite-gha: " + command + ": warning:",
			workflowPath + ":4:23:",
			"[W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED]",
			"cancel-in-progress is not enforced",
			"Buildkite pipeline settings",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("stderr = %q, want %q", output, want)
			}
		}
	}

	t.Run("validate", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		for _, want := range []string{
			"! [W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED]",
			workflowPath + ":4:23:",
			"cancel-in-progress is not enforced",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("stdout = %q, want %q", stdout.String(), want)
			}
		}
	})

	t.Run("validate JSON", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Diagnostics) != 1 || report.Diagnostics[0].Level != "warning" || report.Diagnostics[0].Code != "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED" || !strings.Contains(report.Diagnostics[0].Message, workflowPath+":4:23:") {
			t.Fatalf("report diagnostics = %#v", report.Diagnostics)
		}
	})

	t.Run("validate profile JSON", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Diagnostics) != 1 || report.Diagnostics[0].Level != "warning" || report.Diagnostics[0].Code != "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED" || !strings.Contains(report.Diagnostics[0].Message, workflowPath+":4:23:") {
			t.Fatalf("profile report diagnostics = %#v", report.Diagnostics)
		}
	})

	t.Run("compile pipeline", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"compile", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		assertWarning(t, "compile", stderr.String())
		if count := strings.Count(stdout.String(), "concurrency_group:"); count != 2 {
			t.Fatalf("pipeline concurrency gates = %d, want 2\n%s", count, stdout.String())
		}
	})

	t.Run("compile IR", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"compile", "--format", "ir-json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		assertWarning(t, "compile", stderr.String())
		var ir compiler.IR
		if err := json.Unmarshal(stdout.Bytes(), &ir); err != nil {
			t.Fatal(err)
		}
		if len(ir.Warnings) != 1 || ir.Workflow.ConcurrencyGroup != "ci-refs/heads/main" {
			t.Fatalf("IR = %#v", ir)
		}
	})

	t.Run("upload", func(t *testing.T) {
		requireImporterHost(t)
		t.Setenv("BUILDKITE", "true")
		t.Setenv("BUILDKITE_STEP_KEY", "concurrency-importer")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		assertWarning(t, "upload", stderr.String())
		if len(runner.commands) == 0 {
			t.Fatalf("uploaded commands = %#v", runner.commands)
		}
		pipeline := string(runner.commands[len(runner.commands)-1].stdin)
		if strings.Count(pipeline, "concurrency_group:") != 2 || strings.Contains(pipeline, "agents:") {
			t.Fatalf("uploaded commands = %#v", runner.commands)
		}
	})
}

func TestValidatePublishesProcessingDiagnosticsInBuildkite(t *testing.T) {
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)

	t.Run("error", func(t *testing.T) {
		workflowPath := filepath.Join(t.TempDir(), "unsupported.yml")
		if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  test:\n    runs-on: windows-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev", runner); code != 1 {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 1 {
			t.Fatalf("commands = %#v, want one annotation", runner.commands)
		}
		annotation := runner.commands[0]
		if len(annotation.args) != 9 || annotation.args[0] != "annotate" || annotation.args[2] != "job" || annotation.args[4] != cliTestJobID || !strings.HasPrefix(annotation.args[6], processingAnnotationContext+"-") || annotation.args[8] != "error" {
			t.Fatalf("annotation args = %#v", annotation.args)
		}
		for _, want := range []string{
			`<h2 class="h4 mb2">Workflow could not be run</h2>`,
			`<p><strong>Runner label &#34;windows-latest&#34; requires Windows, which is unsupported.`,
			`<summary>Diagnostic detail</summary>`,
			`Supported runner labels: ubuntu-22.04, ubuntu-24.04, ubuntu-latest.`,
			"Job <code>test</code>",
		} {
			if !strings.Contains(string(annotation.stdin), want) {
				t.Fatalf("annotation = %q, want %q", annotation.stdin, want)
			}
		}
		for _, unwanted := range []string{"E_EXPRESSION_INVALID", "stage:", "instance:", "gha-test"} {
			if strings.Contains(string(annotation.stdin), unwanted) {
				t.Fatalf("annotation = %q, does not want %q", annotation.stdin, unwanted)
			}
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Result != "incompatible" {
			t.Fatalf("report = %#v, error = %v", report, err)
		}
	})

	t.Run("warning publication failure is non-fatal", func(t *testing.T) {
		workflowPath := filepath.Join(t.TempDir(), "warning.yml")
		workflow := []byte("on: push\nconcurrency:\n  group: ci\n  cancel-in-progress: true\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
		if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
			t.Fatal(err)
		}
		runner := &cliCaptureRunner{failAnnotation: true}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"validate", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 1 || runner.commands[0].args[8] != "warning" {
			t.Fatalf("commands = %#v, want one warning annotation", runner.commands)
		}
		if body := string(runner.commands[0].stdin); !strings.Contains(body, `<h2 class="h4 mb2">GitHub Actions workflow diagnostics</h2>`) || strings.Contains(body, "Workflow could not be run") {
			t.Fatalf("warning annotation = %q", body)
		}
		if !strings.Contains(stderr.String(), "warning: processing annotation: annotation unavailable") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})
}

type annotationContextRunner struct {
	contexts []context.Context
}

func (r *annotationContextRunner) Run(ctx context.Context, _ string, _ string, _ []string, _ []byte) ([]byte, error) {
	r.contexts = append(r.contexts, ctx)
	return nil, nil
}

func TestProcessingAnnotationsUseActiveBoundedContext(t *testing.T) {
	for _, path := range []struct {
		name    string
		publish func(processingOutput)
	}{
		{
			name: "processing",
			publish: func(out processingOutput) {
				report := compatibility.NewProcessingReport("ci.yml", "")
				report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{Level: "warning", Message: "test warning"})
				out.annotate(report)
			},
		},
		{
			name: "skipped workflow",
			publish: func(out processingOutput) {
				out.annotateSkippedWorkflows("push", []skippedWorkflow{{label: "CI", key: "ci", reason: "not applicable"}})
			},
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			active, cancel := context.WithCancel(context.Background())
			cancel()
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_BUILD_URL", "https://buildkite.com/acme/widgets/builds/42")
			runner := &annotationContextRunner{}
			out := newProcessingOutput(active, "test", "text", io.Discard, io.Discard, transport.Agent{Runner: runner})
			path.publish(out)
			if len(runner.contexts) != 1 {
				t.Fatalf("annotation calls = %d, want 1", len(runner.contexts))
			}
			if err := runner.contexts[0].Err(); !errors.Is(err, context.Canceled) {
				t.Errorf("annotation context error = %v, want context canceled", err)
			}
			if _, ok := runner.contexts[0].Deadline(); !ok {
				t.Error("annotation context has no deadline")
			}
		})
	}
}

func TestEventBackedCommandsLinkEarlyWorkflowDiagnostics(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "broken.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := writeUploadEvent(t, repository, "push", "refs/heads/main", map[string]any{})
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", "importer")
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)

	for _, command := range []string{"validate", "upload"} {
		t.Run(command, func(t *testing.T) {
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{command, "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 1 {
				t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
			}
			if len(runner.commands) != 1 {
				t.Fatalf("commands = %#v, want one annotation", runner.commands)
			}
			want := `href="https://github.com/buildkite/buildkite-gha/blob/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/.github/workflows/broken.yml`
			if !strings.Contains(string(runner.commands[0].stdin), want) {
				t.Fatalf("annotation = %q, want linked workflow path %q", runner.commands[0].stdin, want)
			}
		})
	}
}

func TestProcessingAnnotationIsBoundedAndEscapesHTML(t *testing.T) {
	report := compatibility.NewProcessingReport("<workflow>|name", "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Code: "E_TEST", Message: `line one
<script>*unsafe*</script> "quoted" ` + strings.Repeat("界", processingAnnotationBodyLimit),
	})
	style, body := processingAnnotation(report, sourceLinkContext{})
	if style != "error" || len(body) > processingAnnotationBodyLimit || !utf8.ValidString(body) {
		t.Fatalf("style = %q, bytes = %d, valid UTF-8 = %v", style, len(body), utf8.ValidString(body))
	}
	for _, want := range []string{"&lt;workflow&gt;|name", "&lt;script&gt;*unsafe*&lt;/script&gt; &#34;quoted&#34;", "Additional diagnostics omitted"} {
		if !strings.Contains(body, want) {
			t.Fatalf("annotation lacks %q", want)
		}
	}
	if strings.Count(body, "<div") != strings.Count(body, "</div>") {
		t.Fatalf("annotation contains unbalanced styling markup")
	}
}

func TestProcessingAnnotationOmitsUnknownActionRuntime(t *testing.T) {
	report := compatibility.NewProcessingReport("ci.yml", "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "warning", Code: "W_ACTION_RUNTIME_UNKNOWN", Message: "Action runtime behavior was not evaluated.",
	})
	style, body := processingAnnotation(report, sourceLinkContext{})
	if style != "" || body != "" {
		t.Fatalf("processingAnnotation() = %q, %q, want no Buildkite annotation", style, body)
	}
}

func TestProcessingAnnotationReservesSpaceForTruncationNotice(t *testing.T) {
	report := compatibility.NewProcessingReport("ci.yml", "")
	probe := compatibility.Diagnostic{Level: "warning", Code: "W_LARGE", Message: "a"}
	probeRow := renderProcessingDiagnostic(probe, sourceLinkContext{})
	prefixBytes := len("<h2 class=\"h4 mb2\">GitHub Actions workflow diagnostics</h2>\n") +
		len("<p>") + len(annotationCode(report.Workflow)) + len("</p>\n")
	messageBytes := processingAnnotationBodyLimit - prefixBytes - len(processingAnnotationNotice)/2 - (len(probeRow) - len(probe.Message))
	report.Diagnostics = append(report.Diagnostics,
		compatibility.Diagnostic{Level: "warning", Code: "W_LARGE", Message: strings.Repeat("a", messageBytes)},
		compatibility.Diagnostic{Level: "warning", Code: "W_OMITTED", Message: "omitted diagnostic"},
	)

	_, body := processingAnnotation(report, sourceLinkContext{})
	if len(body) > processingAnnotationBodyLimit || !strings.Contains(body, "Additional diagnostics omitted") {
		t.Fatalf("annotation bytes = %d, notice present = %v", len(body), strings.Contains(body, "Additional diagnostics omitted"))
	}
}

func TestProcessingAnnotationDropsDetailBeforeTruncatingMessage(t *testing.T) {
	diagnostic := compatibility.Diagnostic{
		Level: "error", Message: "Keep this actionable guidance intact.", Detail: strings.Repeat("diagnostic context ", 20),
	}
	withoutDetail := diagnostic
	withoutDetail.Detail = ""
	want := renderProcessingDiagnostic(withoutDetail, sourceLinkContext{})

	got := renderProcessingDiagnosticWithin(diagnostic, len(want), sourceLinkContext{})
	if got != want || strings.Contains(got, "<details") {
		t.Fatalf("bounded diagnostic = %q, want primary message without detail %q", got, want)
	}
}

func TestProcessingAnnotationUsesRepositoryRelativeWorkflowPath(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "test-image-build.yml")
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	report := compatibility.NewProcessingReport(workflowPath, "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid workflow",
		Location: &compatibility.SourceLocation{Path: workflowPath, Line: 4, Column: 2},
	})

	_, body := processingAnnotation(report, sourceLinkContext{})
	wantWorkflow := "<p><code>.github/workflows/test-image-build.yml</code></p>"
	wantLocation := "<code>.github/workflows/test-image-build.yml:4:2</code>"
	if !strings.Contains(body, wantWorkflow) || !strings.Contains(body, wantLocation) || strings.Contains(body, repository) {
		t.Fatalf("annotation = %q, want %q and %q without checkout path", body, wantWorkflow, wantLocation)
	}
}

func TestProcessingAnnotationResolvesPathsFromBelowCheckoutRoot(t *testing.T) {
	repository := t.TempDir()
	workingDirectory := filepath.Join(repository, ".github")
	workflowPath := filepath.Join(workingDirectory, "workflows", "hello.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workingDirectory)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	report := compatibility.NewProcessingReport("workflows/hello.yml", "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid workflow",
		Location: &compatibility.SourceLocation{Path: "workflows/hello.yml", Line: 100, Column: 3},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}

	_, body := processingAnnotation(report, sourceLinks)
	want := `href="https://github.com/owner/repo/blob/abc123/.github/workflows/hello.yml#L100"`
	if !strings.Contains(body, want) {
		t.Fatalf("annotation = %q, want %q", body, want)
	}
}

func TestProcessingAnnotationResolvesCompilerLocationsFromCheckoutRoot(t *testing.T) {
	repository := t.TempDir()
	workingDirectory := filepath.Join(repository, ".github")
	workflowDirectory := filepath.Join(workingDirectory, "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"caller.yml", "build-security.yml"} {
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), []byte("on: workflow_call\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workingDirectory)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	report := compatibility.NewProcessingReport("workflows/caller.yml", "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid reusable workflow",
		Location: &compatibility.SourceLocation{Path: "./.github/workflows/build-security.yml", Line: 35, Column: 13},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}

	_, body := processingAnnotation(report, sourceLinks)
	want := `<a href="https://github.com/owner/repo/blob/abc123/.github/workflows/build-security.yml#L35"><code>.github/workflows/build-security.yml:35:13</code></a>`
	if !strings.Contains(body, want) {
		t.Fatalf("annotation = %q, want %q", body, want)
	}
}

func TestProcessingDiagnosticsRetainNestedWorkflowSourceRoot(t *testing.T) {
	repository := t.TempDir()
	workingDirectory := filepath.Join(repository, "nested")
	if err := os.MkdirAll(filepath.Join(workingDirectory, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"caller.yml", "build-security.yml"} {
		if err := os.WriteFile(filepath.Join(workingDirectory, ".github", "workflows", name), []byte("on: workflow_call\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workingDirectory)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	report := compatibility.NewProcessingReport("./.github/workflows/caller.yml", "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid reusable workflow",
		Location: &compatibility.SourceLocation{Path: "./.github/workflows/build-security.yml", Line: 35, Column: 13},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}
	wantLink := "https://github.com/owner/repo/blob/abc123/nested/.github/workflows/build-security.yml#L35"

	_, annotation := processingAnnotation(report, sourceLinks)
	_, summary := processingAnnotationWithin(report, sourceLinks, workflowCheckSummaryLimit, workflowCheckSummaryNotice, false)
	if !strings.Contains(annotation, `href="`+wantLink+`"`) || !strings.Contains(summary, `href="`+wantLink+`"`) {
		t.Fatalf("nested workflow location was not retained: annotation=%q summary=%q", annotation, summary)
	}
}

func TestProcessingAnnotationLinksWorkflowLocationsToSource(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "hello world.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	report := compatibility.NewProcessingReport(".github/workflows/hello world.yml", "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid workflow",
		Location: &compatibility.SourceLocation{Path: ".github/workflows/hello world.yml", Line: 100, Column: 3},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.example.com", repository: "owner/repo", sha: "abc123"}

	_, body := processingAnnotation(report, sourceLinks)
	for _, want := range []string{
		`<a href="https://github.example.com/owner/repo/blob/abc123/.github/workflows/hello%20world.yml"><code>.github/workflows/hello world.yml</code></a>`,
		`<a href="https://github.example.com/owner/repo/blob/abc123/.github/workflows/hello%20world.yml#L100"><code>.github/workflows/hello world.yml:100:3</code></a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("annotation = %q, want %q", body, want)
		}
	}
}

func TestSourceLinkRequiresRepositoryContextAndRelativePath(t *testing.T) {
	configured := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}
	for _, test := range []struct {
		name    string
		context sourceLinkContext
		path    string
	}{
		{name: "missing context", path: ".github/workflows/ci.yml"},
		{name: "absolute path", context: configured, path: "/tmp/ci.yml"},
		{name: "path traversal", context: configured, path: "../ci.yml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.context.link(test.path, 1); got != "" {
				t.Fatalf("link() = %q, want empty", got)
			}
		})
	}
}

func TestProcessingDiagnosticsDoNotLinkPathsOutsideCheckout(t *testing.T) {
	checkout := t.TempDir()
	outside := t.TempDir()
	t.Chdir(outside)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", checkout)
	report := compatibility.NewProcessingReport("ci.yml", "hosted")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid workflow",
		Location: &compatibility.SourceLocation{Path: "ci.yml", Line: 4, Column: 2},
	}, compatibility.Diagnostic{
		Level: "error", Message: "invalid reusable workflow",
		Location: &compatibility.SourceLocation{Path: "./.github/workflows/ci.yml", Line: 5, Column: 3},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}

	_, annotation := processingAnnotation(report, sourceLinks)
	_, summary := processingAnnotationWithin(report, sourceLinks, workflowCheckSummaryLimit, workflowCheckSummaryNotice, false)
	if strings.Contains(annotation, "href=") || strings.Contains(summary, "href=") {
		t.Fatalf("outside path was linked: annotation=%q summary=%q", annotation, summary)
	}
}

func TestProcessingDiagnosticsDoNotLinkSymlinksOutsideCheckout(t *testing.T) {
	checkout := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "ci.yml")
	if err := os.WriteFile(target, []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(checkout, "ci.yml")
	if err := os.Symlink(target, workflowPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", checkout)
	report := compatibility.NewProcessingReport(workflowPath, "hosted")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid workflow",
		Location: &compatibility.SourceLocation{Path: workflowPath, Line: 1, Column: 1},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}

	_, annotation := processingAnnotation(report, sourceLinks)
	_, summary := processingAnnotationWithin(report, sourceLinks, workflowCheckSummaryLimit, workflowCheckSummaryNotice, false)
	if strings.Contains(annotation, "href=") || strings.Contains(summary, "href=") {
		t.Fatalf("outside symlink was linked: annotation=%q summary=%q", annotation, summary)
	}
}

func TestProcessingDiagnosticsLinkThroughCheckoutRootSymlink(t *testing.T) {
	realCheckout := t.TempDir()
	workflowPath := filepath.Join(realCheckout, "ci.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(realCheckout, checkout); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", checkout)
	report := compatibility.NewProcessingReport(filepath.Join(checkout, "ci.yml"), "hosted")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "invalid workflow",
		Location: &compatibility.SourceLocation{Path: filepath.Join(checkout, "ci.yml"), Line: 1, Column: 1},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}

	_, annotation := processingAnnotation(report, sourceLinks)
	if want := `href="https://github.com/owner/repo/blob/abc123/ci.yml#L1"`; !strings.Contains(annotation, want) {
		t.Fatalf("checkout symlink location was not linked: annotation=%q want=%q", annotation, want)
	}
}

func TestProcessingAnnotationDoesNotRepeatDiagnosticLocation(t *testing.T) {
	report := compatibility.NewProcessingReport("ci.yml", "")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "warning", Code: "W_TEST", Message: "ci.yml:4:23: warning message",
		Location: &compatibility.SourceLocation{Path: "ci.yml", Line: 4, Column: 23},
	})
	_, body := processingAnnotation(report, sourceLinkContext{})
	if count := strings.Count(body, "ci.yml:4:23"); count != 1 {
		t.Fatalf("annotation location count = %d, want 1: %q", count, body)
	}
	if !strings.Contains(body, "warning message") {
		t.Fatalf("annotation = %q", body)
	}
}

func TestAnnotationCodeEscapesHTML(t *testing.T) {
	if got, want := annotationCode("action` **not bold** & <tag> ``tail"), "<code>action` **not bold** &amp; &lt;tag&gt; ``tail</code>"; got != want {
		t.Fatalf("annotationCode() = %q, want %q", got, want)
	}
}

func TestProcessingAnnotationPresentsActionFailureAsAConciseCard(t *testing.T) {
	report := compatibility.NewProcessingReport("ci.yml", "hosted")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Code: "E_ACTION_UNSUPPORTED", Stage: string(compiler.StageResolution),
		Message: `Action "actions/setup-java@v4" is unsupported: action metadata uses unsupported field "deprecationMessage"`,
		Job:     "test", Instance: "gha-test-a1b2", Action: "actions/setup-java@v4", Step: 2,
	})

	_, body := processingAnnotation(report, sourceLinkContext{})
	for _, want := range []string{
		`<p><strong>Action metadata uses unsupported field &#34;deprecationMessage&#34;</strong></p>`,
		"<p>Action <code>actions/setup-java@v4</code> · Job <code>test</code> · Step 2</p>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("annotation = %q, want %q", body, want)
		}
	}
	if count := strings.Count(body, "actions/setup-java@v4"); count != 1 {
		t.Fatalf("action count = %d, want 1: %q", count, body)
	}
	for _, unwanted := range []string{"E_ACTION_UNSUPPORTED", "stage:", "instance:", "gha-test-a1b2"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("annotation = %q, does not want %q", body, unwanted)
		}
	}
}

func TestProcessingAnnotationLeadsWithTheActionableDiagnostic(t *testing.T) {
	tests := []struct {
		name       string
		diagnostic compatibility.Diagnostic
		want       string
	}{
		{
			name: "action resolution",
			diagnostic: compatibility.Diagnostic{Action: "owner/action@v1",
				Message: `Action "owner/action@v1" could not be resolved: tag v1 was not found`},
			want: "Tag v1 was not found",
		},
		{
			name: "token configuration",
			diagnostic: compatibility.Diagnostic{Code: "E_PROFILE",
				Message: `Job "test" needs GITHUB_TOKEN, but job-level permissions are unsupported.`},
			want: `Job "test" needs GITHUB_TOKEN, but job-level permissions are unsupported.`,
		},
		{
			name: "Docker provenance",
			diagnostic: compatibility.Diagnostic{Code: "E_PROFILE",
				Message: `Job "test" requires Docker without matching compiler provenance. Hosted runs support only verified Dockerfile actions and bounded job or service containers.`},
			want: `Job "test" requires Docker without matching compiler provenance.`,
		},
		{
			name: "action runtime warning",
			diagnostic: compatibility.Diagnostic{Level: "warning", Code: "W_ACTION_RUNTIME_UNKNOWN",
				Message: "Action runtime behavior was not evaluated."},
			want: "Action runtime behavior was not evaluated.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			heading, _ := annotationDiagnosticPresentation(test.diagnostic)
			if heading != test.want {
				t.Fatalf("heading = %q, want %q", heading, test.want)
			}
		})
	}
}

func TestProcessingDiagnosticRenderingsUseTheSameMessageAndAggregation(t *testing.T) {
	const message = `Job "test" needs GITHUB_TOKEN, but job-level permissions are unsupported. Move permissions to the workflow level.`
	const detail = `Effective permissions: contents: read.`
	report := compatibility.NewProcessingReport("ci.yml", "hosted")
	for _, instance := range []string{"gha-test-a", "gha-test-b"} {
		report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
			Level: "error", Code: "E_PROFILE", Stage: string(compiler.StageAdmission),
			Message: message, Detail: detail, Job: "test", Instance: instance,
		})
	}

	var jsonOutput bytes.Buffer
	if err := compatibility.WriteProcessing(&jsonOutput, "json", report); err != nil {
		t.Fatal(err)
	}
	var decoded compatibility.ProcessingReport
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Diagnostics) != 1 || decoded.Diagnostics[0].Message != message || decoded.Diagnostics[0].Detail != detail || decoded.Diagnostics[0].Instance != "" {
		t.Fatalf("JSON diagnostics = %#v", decoded.Diagnostics)
	}

	var textOutput bytes.Buffer
	if err := compatibility.WriteProcessing(&textOutput, "text", report); err != nil {
		t.Fatal(err)
	}
	_, annotation := processingAnnotation(report, sourceLinkContext{})
	if strings.Count(textOutput.String(), message) != 1 || strings.Count(textOutput.String(), "detail: "+detail) != 1 ||
		strings.Count(annotation, `Job &#34;test&#34; needs GITHUB_TOKEN, but job-level permissions are unsupported.`) != 1 ||
		strings.Count(annotation, `Move permissions to the workflow level.`) != 1 ||
		strings.Count(annotation, `<summary>Diagnostic detail</summary>`) != 1 || strings.Count(annotation, detail) != 1 {
		t.Fatalf("text = %q; annotation = %q", textOutput.String(), annotation)
	}
	if strings.Contains(annotation, "gha-test-") {
		t.Fatalf("annotation exposes matrix instance IDs: %q", annotation)
	}
}

type failureArtifactDownload struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type failureArtifactPlugin struct {
	Step     string                    `yaml:"step"`
	Download []failureArtifactDownload `yaml:"download"`
}

type failureStepPlugins []map[string]failureArtifactPlugin

func isGeneratedFailureCommand(command string) bool {
	return command == `cat .buildkite-gha-failure-message.txt
buildkite-agent annotate --scope=job --style=error < .buildkite-gha-failure-annotation.html
exit 1`
}

func failureArtifactForStep(plugins failureStepPlugins, uploaded map[string][]byte, kind string) []byte {
	if len(plugins) != 1 {
		return nil
	}
	plugin, ok := plugins[0]["artifacts#v1.9.4"]
	if !ok {
		return nil
	}
	prefix := ".buildkite-gha/failures/" + kind + "/"
	for _, download := range plugin.Download {
		if strings.HasPrefix(download.From, prefix) {
			return uploaded[download.From]
		}
	}
	return nil
}

func TestUnsupportedConditionPreflightAppliesToEveryCompilerEntryPoint(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "conditions.yml")
	if err := os.WriteFile(workflowPath, []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: [12, "14"]
    if: unsupported(matrix.version)
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	want := `job condition: condition function "unsupported" is unsupported`

	t.Run("validate json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodeExpressionInvalid || !strings.Contains(report.Diagnostics[0].Message, want) {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("profile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProcessingReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Compile.Result != "incompatible" || report.Admission.Result != "not-evaluated" || len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, want) {
			t.Fatalf("profile report = %#v", report)
		}
	})

	t.Run("compile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"compile", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 || !strings.Contains(stderr.String(), want) {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("upload generates an annotating failing step", func(t *testing.T) {
		requireImporterHost(t)
		t.Setenv("BUILDKITE", "true")
		t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
		t.Setenv("BUILDKITE_STEP_KEY", "condition-importer")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		args := []string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}
		if code := run(args, &stdout, &stderr, "dev", runner); code != 0 || stderr.Len() != 0 {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		for _, command := range runner.commands {
			if len(command.args) != 0 && command.args[0] == "annotate" {
				t.Fatalf("unsupported condition annotated the importer: %#v", runner.commands)
			}
		}
		var pipeline struct {
			Steps []struct {
				Group   string             `yaml:"group"`
				Label   string             `yaml:"label"`
				Command string             `yaml:"command"`
				Plugins failureStepPlugins `yaml:"plugins"`
				Steps   []any              `yaml:"steps"`
			} `yaml:"steps"`
		}
		if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
			t.Fatal(err)
		}
		wantLabel := ":github: " + filepath.ToSlash(filepath.Clean(workflowPath))
		message := failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "messages")
		if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != "" || len(pipeline.Steps[0].Steps) != 0 || pipeline.Steps[0].Label != wantLabel || !isGeneratedFailureCommand(pipeline.Steps[0].Command) || !strings.Contains(string(message), want) {
			t.Fatalf("unsupported condition pipeline = %#v", pipeline.Steps)
		}
	})
}

func TestUploadAcceptsConditionalActionInputDefault(t *testing.T) {
	requireImporterHost(t)
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "action.yml")
	actionRoot := filepath.Join(root, ".github", "actions", "complex-default")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\npermissions:\n  contents: read\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/complex-default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte(`name: complex default
inputs:
  token:
    default: ${{ github.server_url == 'https://github.com' && github.token || '' }}
  job-status:
    default: ${{ job.status }}
runs:
  using: node24
  main: main.js
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "main.js"), []byte("console.log('must not run')\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "action-default-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if len(runner.commands) == 0 {
		t.Fatal("conditional action default did not upload a pipeline")
	}
}

func TestValidateHostedProfileResolvesActionsWithoutClaimingRuntime(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "action.yml")
	actionPath := filepath.Join(root, ".github", "actions", "local")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("runs:\n  using: node24\n  main: main.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "main.js"), []byte("console.log('local action')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_GHA_NODE20", writeFakeNode(t, root, 20))
	t.Setenv("BUILDKITE_GHA_NODE24", writeFakeNode(t, root, 24))
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "W_ACTION_RUNTIME_UNKNOWN" {
		t.Fatalf("profile report = %#v", report)
	}

	t.Setenv("BUILDKITE_GHA_NODE20", filepath.Join(root, "missing-node20"))
	t.Setenv("BUILDKITE_GHA_TARGET_QUEUE", "not a queue")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("environment override affected production profile: code = %d; stderr = %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" {
		t.Fatalf("environment profile report = %#v", report)
	}
}

func TestValidateHostedProfileAdmitsOrdinarySecretsAfterCompile(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "secret.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  secret:\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets.TOKEN }}\n    steps:\n      - run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" || report.Compile.Result != "compilable" || report.Admission.Result != "admitted" || len(report.Diagnostics) != 0 {
		t.Fatalf("profile report = %#v", report)
	}
}

func TestValidateHostedProfileAdmitsImplicitReadOnlyWorkflowToken(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "token.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  token:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ secrets.GITHUB_TOKEN }}'\n")
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" || report.Compile.Result != "compilable" || report.Admission.Result != "admitted" || len(report.Diagnostics) != 0 {
		t.Fatalf("profile report = %#v", report)
	}
}

func TestRunUploadCompilesArtifactsAndUploadsSelfContainedPipeline(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "shell-upload-importer")
	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 3 jobs") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if len(runner.commands) != 5 {
		t.Fatalf("commands = %#v, want distribution, three plans, and pipeline", runner.commands)
	}
	root := runner.commands[0].dir
	for i, command := range runner.commands[:4] {
		if command.dir != root || command.name != "buildkite-agent" || len(command.args) != 3 || command.args[0] != "artifact" || command.args[1] != "upload" {
			t.Fatalf("artifact command %d = %#v", i, command)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temporary artifact root still exists: %v", err)
	}
	pipelineCommand := runner.commands[4]
	wantPipelineArgs := []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets"}
	if strings.Join(pipelineCommand.args, " ") != strings.Join(wantPipelineArgs, " ") {
		t.Fatalf("pipeline args = %#v, want %#v", pipelineCommand.args, wantPipelineArgs)
	}
	var pipeline struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Key       string `yaml:"key"`
			Condition string `yaml:"if"`
			DependsOn string `yaml:"depends_on"`
			Steps     []struct {
				Key      string `yaml:"key"`
				Command  string `yaml:"command"`
				Cache    any    `yaml:"cache"`
				Agents   any    `yaml:"agents"`
				Checkout struct {
					Skip bool `yaml:"skip"`
				} `yaml:"checkout"`
				DependsOn []struct {
					Step         string `yaml:"step"`
					AllowFailure bool   `yaml:"allow_failure"`
				} `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != ":github: buildkite-gha shell smoke" || pipeline.Steps[0].Key == "" || pipeline.Steps[0].Condition != `(true)` || pipeline.Steps[0].DependsOn != "shell-upload-importer" || len(pipeline.Steps[0].Steps) != 3 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	wantLegacyKeys := map[string]bool{"gha-producer": true, "gha-consumer-5ebbc197d87b": true, "gha-consumer-91934b28b00f": true}
	for _, step := range pipeline.Steps[0].Steps {
		if !wantLegacyKeys[step.Key] {
			t.Fatalf("literal single-workflow upload changed generated key %q", step.Key)
		}
		if step.Cache != nil {
			t.Fatalf("shell-only step %q unexpectedly configures a runtime cache: %#v", step.Key, step.Cache)
		}
		if !step.Checkout.Skip || step.Agents != nil {
			t.Fatalf("step %q lacks isolated checkout or default agent targeting: %#v", step.Key, step)
		}
		for _, dependency := range step.DependsOn {
			if dependency.Step == "shell-upload-importer" {
				t.Fatalf("step %q retains importer dependency: %#v", step.Key, step.DependsOn)
			}
		}
		if !strings.HasPrefix(step.Command, "set -euo pipefail\n") ||
			!strings.Contains(step.Command, `bootstrap_dir="$(mktemp -d `) ||
			!strings.Contains(step.Command, `--step 'shell-upload-importer'`) ||
			!strings.Contains(step.Command, `sha256sum "$distribution"`) ||
			!strings.Contains(step.Command, `sha256sum "$plan"`) ||
			!strings.Contains(step.Command, `sudo -n --preserve-env --user runner`) ||
			!strings.Contains(step.Command, `run-job --plan "$plan"`) {
			t.Fatalf("step %q command is not self-contained:\n%s", step.Key, step.Command)
		}
	}
}

func TestRunUploadPublishesMixedRuntimeDistributions(t *testing.T) {
	requireImporterHost(t)
	root := t.TempDir()
	workflowPath := filepath.Join(root, "mixed.yml")
	workflow := []byte("on: push\njobs:\n  linux:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo linux\n  macos:\n    needs: linux\n    runs-on: macos-15\n    steps:\n      - run: echo macos\n")
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	_, importerContents, _, err := executable()
	if err != nil {
		t.Fatal(err)
	}
	linuxContents := append(bytes.Clone(importerContents), 0)
	linuxPath := filepath.Join(root, "buildkite-gha-linux")
	if err := os.WriteFile(linuxPath, linuxContents, 0o700); err != nil {
		t.Fatal(err)
	}
	darwinContents := []byte{
		0xcf, 0xfa, 0xed, 0xfe, // 64-bit little-endian Mach-O
		0x0c, 0x00, 0x00, 0x01, // CPU_TYPE_ARM64
		0x00, 0x00, 0x00, 0x00, // CPU subtype
		0x02, 0x00, 0x00, 0x00, // MH_EXECUTE
		0x00, 0x00, 0x00, 0x00, // load command count
		0x00, 0x00, 0x00, 0x00, // load command size
		0x00, 0x00, 0x00, 0x00, // flags
		0x00, 0x00, 0x00, 0x00, // reserved
	}
	darwinPath := filepath.Join(root, "buildkite-gha-darwin")
	if err := os.WriteFile(darwinPath, darwinContents, 0o700); err != nil {
		t.Fatal(err)
	}
	linuxDigest := transport.Digest(linuxContents)
	darwinDigest := transport.Digest(darwinContents)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "mixed-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	args := []string{
		"upload", "--event-path", eventPath,
		"--runner-queue", "ubuntu-latest=linux",
		"--runner-queue", "macos-15=macos",
		"--runtime-distribution", "linux/amd64=" + linuxPath,
		"--runtime-distribution", "darwin/arm64=" + darwinPath,
		workflowPath,
	}
	if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}

	wantDistributionDigests := []string{linuxDigest, darwinDigest}
	for _, digest := range wantDistributionDigests {
		path, err := buildkitepipeline.DistributionPath(digest)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := runner.uploaded[path]; !ok {
			t.Errorf("runtime distribution %s was not uploaded", digest)
		}
	}
	artifactUploads := map[string]int{}
	for _, command := range runner.commands {
		if len(command.args) == 3 && slices.Equal(command.args[:2], []string{"artifact", "upload"}) {
			artifactUploads[command.args[2]]++
		}
	}
	if len(runner.uploaded) != 4 {
		t.Fatalf("uploaded artifact count = %d, want two runtimes and two plans", len(runner.uploaded))
	}
	for path, count := range artifactUploads {
		if count != 1 {
			t.Errorf("artifact %q uploaded %d times", path, count)
		}
	}

	planRuntime := map[string]string{}
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatalf("decode plan %q: %v", path, err)
		}
		planRuntime[job.Workflow.LogicalJobID] = job.RuntimeDistributionDigest()
	}
	if planRuntime["linux"] != linuxDigest || planRuntime["macos"] != darwinDigest {
		t.Fatalf("plan runtime digests = %#v", planRuntime)
	}
	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Key     string `yaml:"key"`
				Command string `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("workflow groups = %#v", pipeline.Steps)
	}
	commands := map[string]string{}
	for _, step := range pipeline.Steps[0].Steps {
		commands[step.Key] = step.Command
	}
	for jobID, digest := range planRuntime {
		if !strings.Contains(commands["gha-"+jobID], strings.TrimPrefix(digest, "sha256:")) {
			t.Errorf("job %q does not download its plan-bound runtime:\n%s", jobID, commands["gha-"+jobID])
		}
	}
}

func TestRunUploadRejectsExplicitTrackedSymlinks(t *testing.T) {
	requireImporterHost(t)
	workflowSource := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	for _, test := range []struct {
		name   string
		target func(repository string) string
	}{
		{
			name: "outside repository",
			target: func(string) string {
				path := filepath.Join(t.TempDir(), "outside.yml")
				if err := os.WriteFile(path, []byte(workflowSource), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "inside repository",
			target: func(repository string) string {
				return filepath.Join(repository, ".github", "workflows", "regular.yml")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{"regular.yml": workflowSource})
			link := filepath.Join(repository, ".github", "workflows", "linked.yml")
			if err := os.Symlink(test.target(repository), link); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("git", "-C", repository, "add", ".github/workflows/linked.yml").CombinedOutput(); err != nil {
				t.Fatalf("git add symlink: %v: %s", err, output)
			}
			eventPath := writeUploadEvent(t, repository, "push", "refs/heads/main", map[string]any{})
			t.Chdir(repository)
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "symlink-importer")
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"upload", "--event-path", eventPath, ".github/workflows/linked.yml"}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not name a regular tracked file") {
				t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
			}
			if stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("tracked symlink reached Buildkite: stdout %q, commands %#v, uploads %#v", stdout.String(), runner.commands, runner.uploaded)
			}
		})
	}
}

func TestExpandExplicitWorkflowPathsCanonicalizesTrackedPaths(t *testing.T) {
	workflowSource := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"a.yml":           workflowSource,
		"b.yaml":          workflowSource,
		"workflow[1].yml": workflowSource,
	})
	workflowDirectory := filepath.Join(repository, ".github", "workflows")
	notePath := filepath.Join(workflowDirectory, "note.txt")
	if err := os.WriteFile(notePath, []byte("not a workflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(workflowDirectory, "linked.yml")
	if err := os.Symlink("a.yml", symlinkPath); err != nil {
		t.Fatal(err)
	}
	leadingDashPath := filepath.Join(repository, "-leading.yml")
	if err := os.WriteFile(leadingDashPath, []byte(workflowSource), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repository, "add", ".github/workflows/note.txt", ".github/workflows/linked.yml").CombinedOutput(); err != nil {
		t.Fatalf("git add fixtures: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", repository, "add", "--", "-leading.yml").CombinedOutput(); err != nil {
		t.Fatalf("git add leading-dash fixture: %v: %s", err, output)
	}
	untrackedPath := filepath.Join(workflowDirectory, "untracked.yml")
	if err := os.WriteFile(untrackedPath, []byte(workflowSource), 0o600); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside.yml")
	if err := os.WriteFile(outsidePath, []byte(workflowSource), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)

	aPath := filepath.Join(".github", "workflows", "a.yml")
	bPath := filepath.Join(".github", "workflows", "b.yaml")
	first, err := expandExplicitWorkflowPaths([]string{filepath.Join(repository, bPath), "./" + aPath, aPath})
	if err != nil {
		t.Fatal(err)
	}
	second, err := expandExplicitWorkflowPaths([]string{aPath, filepath.Join(repository, bPath)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || len(first) != 2 || first[0].CanonicalPath != ".github/workflows/a.yml" || first[1].CanonicalPath != ".github/workflows/b.yaml" {
		t.Fatalf("canonical explicit inputs = %#v and %#v", first, second)
	}
	for _, input := range first {
		if input.Identity == "" || input.StepKeyNamespace != input.Identity || !filepath.IsAbs(input.Path) {
			t.Fatalf("explicit workflow identity = %#v", input)
		}
	}
	metacharacter, err := expandExplicitWorkflowPaths([]string{filepath.Join(".github", "workflows", "workflow[1].yml"), aPath})
	if err != nil || len(metacharacter) != 2 || metacharacter[1].CanonicalPath != ".github/workflows/workflow[1].yml" {
		t.Fatalf("literal metacharacter list = %#v, %v", metacharacter, err)
	}
	operands, _, err := uploadArgs([]string{"--", "-leading.yml", aPath})
	if err != nil {
		t.Fatal(err)
	}
	leadingDash, err := expandExplicitWorkflowPaths(operands)
	if err != nil || len(leadingDash) != 2 || leadingDash[0].CanonicalPath != "-leading.yml" {
		t.Fatalf("leading-dash explicit path = %#v, %v", leadingDash, err)
	}

	for _, test := range []struct {
		name     string
		operands []string
		want     string
	}{
		{name: "mixed glob and literal", operands: []string{filepath.Join(".github", "workflows", "*.yml"), aPath}, want: "glob pattern"},
		{name: "multiple globs", operands: []string{filepath.Join(".github", "workflows", "*.yml"), filepath.Join(".github", "workflows", "*.yaml")}, want: "glob pattern"},
		{name: "missing", operands: []string{filepath.Join(".github", "workflows", "missing.yml"), aPath}, want: "regular tracked file"},
		{name: "untracked", operands: []string{untrackedPath, aPath}, want: "not tracked by git"},
		{name: "directory", operands: []string{workflowDirectory, aPath}, want: "regular tracked file"},
		{name: "outside repository", operands: []string{outsidePath, aPath}, want: "outside the checked-out git repository"},
		{name: "non-workflow extension", operands: []string{notePath, aPath}, want: "must end in .yml or .yaml"},
		{name: "symlink", operands: []string{symlinkPath, aPath}, want: "regular tracked file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := expandExplicitWorkflowPaths(test.operands); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expandExplicitWorkflowPaths(%q) error = %v, want %q", test.operands, err, test.want)
			}
		})
	}
}

func TestRunUploadExplicitPathsAreAtomicAndOrderIndependent(t *testing.T) {
	requireImporterHost(t)
	workflow := func(name string) string {
		return "name: " + name + "\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	}
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"a.yml": workflow("A"),
		"b.yml": workflow("B"),
	})
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "explicit-list-importer")
	aPath := filepath.Join(".github", "workflows", "a.yml")
	bPath := filepath.Join(".github", "workflows", "b.yml")

	runUpload := func(operands []string) (string, []byte, map[string][]byte) {
		t.Helper()
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		args := append([]string{"upload", "--event-path", eventPath}, operands...)
		if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
			t.Fatalf("run(%q) code = %d, stderr = %q", operands, code, stderr.String())
		}
		if stderr.Len() != 0 || len(runner.commands) == 0 {
			t.Fatalf("run(%q) stderr/commands = %q / %#v", operands, stderr.String(), runner.commands)
		}
		return stdout.String(), runner.commands[len(runner.commands)-1].stdin, runner.uploaded
	}
	firstOutput, firstPipeline, firstArtifacts := runUpload([]string{bPath, aPath, "./" + aPath})
	secondOutput, secondPipeline, secondArtifacts := runUpload([]string{"./" + aPath, aPath, bPath})
	if firstOutput != secondOutput || !bytes.Equal(firstPipeline, secondPipeline) || !reflect.DeepEqual(firstArtifacts, secondArtifacts) {
		t.Fatalf("reversed explicit paths changed output:\nfirst: %q\nsecond: %q\nfirst pipeline:\n%s\nsecond pipeline:\n%s", firstOutput, secondOutput, firstPipeline, secondPipeline)
	}
	var pipeline struct {
		Steps []struct {
			Group string `yaml:"group"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(firstPipeline, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 2 || pipeline.Steps[0].Group != ":github: A" || pipeline.Steps[1].Group != ":github: B" {
		t.Fatalf("explicit path groups = %#v", pipeline.Steps)
	}
}

func TestRunUploadRejectsWorkflowSelectorsBeforeBuildkite(t *testing.T) {
	requireImporterHost(t)
	workflowSource := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	repository := writeUploadWorkflowRepository(t, map[string]string{"a.yml": workflowSource})
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "invalid-list-importer")
	for _, operands := range [][]string{
		{"*"},
		{filepath.Join(".github", "workflows", "*.yml")},
		{filepath.Join(".github", "workflows", "*.yml"), filepath.Join(".github", "workflows", "a.yml")},
	} {
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		args := append([]string{"upload"}, operands...)
		if code := run(args, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "glob pattern") {
			t.Fatalf("run(%q) code/stderr = %d / %q", operands, code, stderr.String())
		}
		if stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
			t.Fatalf("invalid workflow selector reached Buildkite: stdout %q, commands %#v, uploads %#v", stdout.String(), runner.commands, runner.uploaded)
		}
	}
}

func TestRunUploadAggregatesExplicitPathsAtomicallyWithNamespacedJobs(t *testing.T) {
	requireImporterHost(t)
	workflowDirectory := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows")
	workflowPaths := []string{
		filepath.Join(workflowDirectory, "artifact-multi-prefix.yml"),
		filepath.Join(workflowDirectory, "cache-v2-compatibility.yml"),
		filepath.Join(workflowDirectory, "cache-v5.yml"),
		filepath.Join(workflowDirectory, "cache-v6.yml"),
		filepath.Join(workflowDirectory, "concurrent.yml"),
		filepath.Join(workflowDirectory, "shell.yml"),
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	inputs, err := expandExplicitWorkflowPaths(workflowPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "aggregate-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	args := append([]string{"upload", "--event-path", eventPath}, workflowPaths...)
	if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 14 jobs from 6 workflows") || stderr.Len() != 0 || len(runner.commands) != 16 {
		t.Fatalf("stdout/stderr/commands = %q / %q / %d", stdout.String(), stderr.String(), len(runner.commands))
	}
	pipelineCommand := runner.commands[len(runner.commands)-1]
	if !slices.Equal(pipelineCommand.args, []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets"}) {
		t.Fatalf("aggregate pipeline command = %#v", pipelineCommand)
	}
	var pipeline struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Key       string `yaml:"key"`
			Condition string `yaml:"if"`
			DependsOn string `yaml:"depends_on"`
			Notify    any    `yaml:"notify"`
			Steps     []struct {
				Key    string `yaml:"key"`
				Label  string `yaml:"label"`
				Notify []struct {
					GitHubCheck struct {
						Name string `yaml:"name"`
					} `yaml:"github_check"`
				} `yaml:"notify"`
				DependsOn []struct {
					Step string `yaml:"step"`
				} `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	wantLabels := []string{"bounded multi-prefix artifact selection", "actions/cache v2 compatibility proof", "actions/cache v5 proof", "actions/cache v6 proof", "buildkite-gha concurrent smoke", "buildkite-gha shell smoke"}
	seenKeys := make(map[string]bool)
	seenCheckNames := make(map[string]bool)
	for i, group := range pipeline.Steps {
		if i >= len(inputs) {
			t.Fatalf("unexpected aggregate group %d = %#v", i, group)
		}
		if group.Group != ":github: "+wantLabels[i] || group.Key != "gha-workflow-"+inputs[i].Identity || group.Condition != `(true)` || group.DependsOn != "aggregate-importer" || group.Notify != nil {
			t.Fatalf("aggregate group %d = %#v", i, group)
		}
		prefix := "gha-" + inputs[i].Identity + "-"
		for _, step := range group.Steps {
			if len(step.Notify) != 1 {
				t.Fatalf("aggregate step %q notifications = %#v", step.Key, step.Notify)
			}
			checkName := step.Notify[0].GitHubCheck.Name
			if !strings.HasPrefix(step.Key, prefix) || seenKeys[step.Key] || seenCheckNames[checkName] || !strings.HasPrefix(checkName, wantLabels[i]+" / ") || !strings.HasSuffix(checkName, " (push)") {
				t.Fatalf("aggregate step key %q, prefix %q, seen %t", step.Key, prefix, seenKeys[step.Key])
			}
			seenKeys[step.Key] = true
			seenCheckNames[checkName] = true
			for _, dependency := range step.DependsOn {
				if dependency.Step == "aggregate-importer" || !strings.HasPrefix(dependency.Step, prefix) {
					t.Fatalf("step %q has cross-workflow or unnamespaced dependency %q", step.Key, dependency.Step)
				}
			}
		}
	}
	if len(pipeline.Steps) != 6 || len(seenKeys) != 14 {
		t.Fatalf("aggregate groups/keys = %d/%d", len(pipeline.Steps), len(seenKeys))
	}
	planCount := 0
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		planCount++
		if !seenKeys[job.Target.StepKey] {
			t.Fatalf("plan targets unknown aggregate key %q", job.Target.StepKey)
		}
	}
	if planCount != 14 {
		t.Fatalf("aggregate uploaded plans = %d", planCount)
	}
}

func TestRunUploadNamesAggregateGitHubChecksFromWorkflowLabels(t *testing.T) {
	requireImporterHost(t)
	repository := t.TempDir()
	workflowDirectory := filepath.Join(repository, ".github", "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	runnable := func(name, trigger string) string {
		return name + "on: " + trigger + "\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n"
	}
	sources := map[string]string{
		"a.yml":        runnable("name: 'Shared \"checks\"'\n", "push"),
		"b.yml":        runnable("name: 'Shared \"checks\"'\n", "pull_request"),
		"unnamed.yml":  runnable("", "\n  push:\n    branches-ignore: [main]"),
		"reusable.yml": "name: Shared\non: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n",
	}
	for name, source := range sources {
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", repository}, {"-C", repository, "add", ".github/workflows"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "checks-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	workflowPaths := []string{
		".github/workflows/a.yml",
		".github/workflows/b.yml",
		".github/workflows/unnamed.yml",
		".github/workflows/reusable.yml",
	}
	args := append([]string{"upload", "--event-path", eventPath}, workflowPaths...)
	if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Label     string `yaml:"label"`
			Type      string `yaml:"type"`
			Condition string `yaml:"if"`
			Skip      string `yaml:"skip"`
			Command   string `yaml:"command"`
			DependsOn string `yaml:"depends_on"`
			Notify    []struct {
				GitHubCheck struct {
					Name string `yaml:"name"`
				} `yaml:"github_check"`
			} `yaml:"notify"`
			Steps []struct {
				Label  string `yaml:"label"`
				Notify []struct {
					GitHubCheck struct {
						Name string `yaml:"name"`
					} `yaml:"github_check"`
				} `yaml:"notify"`
				DependsOn *yaml.Node `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		group, checkName, condition, skip string
	}{
		{group: `:github: Shared "checks"`, checkName: `Shared "checks" / test (push)`, condition: `(true)`},
		{group: `:github: Shared "checks"`, checkName: `Shared "checks" (push)`, skip: "This workflow is not triggered by a `push` event"},
		{group: ":github: .github/workflows/unnamed.yml", checkName: ".github/workflows/unnamed.yml / test (push)", condition: `!("main" =~ /^main$/)`},
	}
	if len(pipeline.Steps) != len(want) {
		t.Fatalf("aggregate groups = %#v, want %d directly runnable workflows", pipeline.Steps, len(want))
	}
	for i, group := range pipeline.Steps {
		if !strings.Contains(group.Condition, want[i].condition) || group.Skip != want[i].skip || strings.Contains(group.Condition, "build.") || group.DependsOn != "checks-importer" {
			t.Fatalf("aggregate group %d = %#v, want %#v", i, group, want[i])
		}
		if group.Skip != "" {
			if group.Group != "" || group.Label != want[i].group || group.Type != "command" || group.Command != "" || len(group.Notify) != 1 || group.Notify[0].GitHubCheck.Name != want[i].checkName || len(group.Steps) != 0 {
				t.Fatalf("aggregate skipped step %d = %#v", i, group)
			}
			continue
		}
		if group.Group != want[i].group || group.Label != "" || group.Notify != nil || len(group.Steps) != 1 {
			t.Fatalf("aggregate group %d = %#v, want %#v", i, group, want[i])
		}
		for _, step := range group.Steps {
			if len(step.Notify) != 1 || step.Notify[0].GitHubCheck.Name != want[i].checkName || step.DependsOn != nil {
				t.Fatalf("aggregate group %d child emitted notification or dependency: %#v / %#v", i, step.Notify, step.DependsOn)
			}
		}
	}
	planCount := 0
	for path := range runner.uploaded {
		if strings.HasSuffix(path, ".json") {
			planCount++
		}
	}
	if planCount != 2 {
		t.Fatalf("uploaded plans = %d, want plans for workflows with matching event triggers", planCount)
	}
}

func TestNewEffectiveEventSeparatesExpressionsAndSnapshot(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := newEffectiveEvent(source, effectiveEventFromPath, func(string) string { return "webhook" })
	if err != nil {
		t.Fatal(err)
	}
	wantExpressions := buildkitepipeline.TriggerConditionExpressions{
		EventPredicate:        "true",
		Branch:                `"main"`,
		Tag:                   "null",
		PullRequestBaseBranch: "null",
		PullRequestAction:     "null",
		MergeGroupBaseBranch:  "null",
		MergeGroupAction:      "null",
	}
	branch := "main"
	wantSnapshot := buildkitepipeline.TriggerEventSnapshot{Branch: &branch}
	if !reflect.DeepEqual(explicit.TriggerExpressions, wantExpressions) || !reflect.DeepEqual(explicit.TriggerSnapshot, wantSnapshot) {
		t.Fatalf("explicit effective event = expressions %#v, snapshot %#v", explicit.TriggerExpressions, explicit.TriggerSnapshot)
	}

	webhook, err := newEffectiveEvent(source, effectiveEventFromWebhook, func(key string) string {
		if key == "BUILDKITE_SOURCE" {
			return "webhook"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	wantExpressions.EventPredicate = `build.source == "webhook"`
	if !reflect.DeepEqual(webhook.TriggerExpressions, wantExpressions) || !reflect.DeepEqual(webhook.TriggerSnapshot, wantSnapshot) {
		t.Fatalf("webhook effective event = expressions %#v, snapshot %#v", webhook.TriggerExpressions, webhook.TriggerSnapshot)
	}
}

func TestRunUploadNamesGitHubCheckForActiveEvent(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join(t.TempDir(), "multi-trigger.yml")
	workflowSource := "name: Active event\non:\n  push:\n  pull_request:\n  merge_group:\n  workflow_dispatch:\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n"
	if err := os.WriteFile(workflowPath, []byte(workflowSource), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, source, githubEvent, wantEvent, wantCondition string
		eventPath                                           string
		webhook                                             []byte
	}{
		{name: "push fallback", source: "webhook", wantEvent: "push", wantCondition: `build.source == "webhook"`},
		{name: "pull request webhook metadata", source: "webhook", githubEvent: "pull_request", webhook: []byte(`{"action":"opened","pull_request":{"base":{"ref":"main"}}}`), wantEvent: "pull_request", wantCondition: `build.source == "webhook"`},
		{name: "merge group webhook metadata", source: "webhook", githubEvent: "merge_group", webhook: []byte(`{"action":"checks_requested","merge_group":{"head_ref":"refs/heads/gh-readonly-queue/main/pr-1-deadbeef","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_ref":"refs/heads/main","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`), wantEvent: "merge_group", wantCondition: `build.source == "webhook"`},
		{name: "UI fallback", source: "ui", wantEvent: "workflow_dispatch", wantCondition: `build.source == "ui"`},
		{name: "API fallback", source: "api", wantEvent: "workflow_dispatch", wantCondition: `build.source == "api"`},
		{name: "schedule fallback", source: "schedule", wantEvent: "schedule", wantCondition: `build.source == "schedule"`},
		{name: "explicit event path precedence", source: "schedule", githubEvent: "pull_request", eventPath: eventPath, wantEvent: "push", wantCondition: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "event-name-importer")
			t.Setenv("BUILDKITE_REPO", "https://github.com/buildkite/buildkite-gha")
			t.Setenv("BUILDKITE_COMMIT", strings.Repeat("a", 40))
			t.Setenv("BUILDKITE_BRANCH", "main")
			t.Setenv("BUILDKITE_TAG", "")
			t.Setenv("BUILDKITE_PULL_REQUEST", "false")
			t.Setenv("BUILDKITE_SOURCE", test.source)
			t.Setenv("BUILDKITE_GITHUB_EVENT", test.githubEvent)
			if test.githubEvent == "merge_group" {
				t.Setenv("BUILDKITE_BRANCH", "gh-readonly-queue/main/pr-1-deadbeef")
				t.Setenv("BUILDKITE_MERGE_QUEUE_BASE_BRANCH", "main")
				t.Setenv("BUILDKITE_MERGE_QUEUE_BASE_COMMIT", strings.Repeat("b", 40))
			}
			runner := &cliCaptureRunner{webhook: test.webhook}
			args := []string{"upload"}
			if test.eventPath != "" {
				args = append(args, "--event-path", test.eventPath)
				runner.webhookErr = errors.New("metadata must not be read with --event-path")
			}
			args = append(args, workflowPath)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
			}
			var pipeline struct {
				Steps []struct {
					Group     string `yaml:"group"`
					Condition string `yaml:"if"`
					Notify    any    `yaml:"notify"`
					Steps     []struct {
						Notify []struct {
							GitHubCheck struct {
								Name string `yaml:"name"`
							} `yaml:"github_check"`
						} `yaml:"notify"`
					} `yaml:"steps"`
				} `yaml:"steps"`
			}
			if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
				t.Fatal(err)
			}
			wantCheckName := "Active event / test (" + test.wantEvent + ")"
			wantGroup := ":github: Active event"
			if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != wantGroup || !strings.Contains(pipeline.Steps[0].Condition, test.wantCondition) || pipeline.Steps[0].Notify != nil || len(pipeline.Steps[0].Steps) != 1 || len(pipeline.Steps[0].Steps[0].Notify) != 1 || pipeline.Steps[0].Steps[0].Notify[0].GitHubCheck.Name != wantCheckName {
				t.Fatalf("aggregate event group = %#v, want group %q and check %q", pipeline.Steps, wantGroup, wantCheckName)
			}
		})
	}
}

func TestRunUploadUsesOriginCheckForOriginEvent(t *testing.T) {
	requireImporterHost(t)
	directory := t.TempDir()
	workflowPath := filepath.Join(directory, "origin.yml")
	workflowSource := "name: Origin CI\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n"
	if err := os.WriteFile(workflowPath, []byte(workflowSource), 0o600); err != nil {
		t.Fatal(err)
	}
	eventSource, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	eventSource = bytes.Replace(eventSource, []byte(`"provider": "github"`), []byte(`"provider": "cursor-origin"`), 1)
	eventPath := filepath.Join(directory, "push.json")
	if err := os.WriteFile(eventPath, eventSource, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "origin-check-importer")
	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Key    string `yaml:"key"`
			Notify any    `yaml:"notify"`
			Steps  []struct {
				Key    string `yaml:"key"`
				Notify []struct {
					GitHubCheck any `yaml:"github_check"`
					OriginCheck *struct {
						Key  string `yaml:"key"`
						Name string `yaml:"name"`
					} `yaml:"origin_check"`
				} `yaml:"notify"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Notify != nil || len(pipeline.Steps[0].Steps) != 1 || len(pipeline.Steps[0].Steps[0].Notify) != 1 || pipeline.Steps[0].Steps[0].Notify[0].GitHubCheck != nil || pipeline.Steps[0].Steps[0].Notify[0].OriginCheck == nil {
		t.Fatalf("Origin workflow pipeline = %#v", pipeline.Steps)
	}
	check := pipeline.Steps[0].Steps[0].Notify[0].OriginCheck
	if check.Key != pipeline.Steps[0].Steps[0].Key || check.Name != "Origin CI / test (push)" {
		t.Fatalf("Origin workflow check = %#v, step key = %q", check, pipeline.Steps[0].Steps[0].Key)
	}
}

func TestRunUploadIsolatesExplicitEffectiveEventsBeforeCompilation(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"dispatch.yml":     "name: Dispatch\non: workflow_dispatch\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		"pull-request.yml": "name: Pull request\non: pull_request\njobs:\n  test:\n    runs-on: ${{ github.event.pull_request.runner }}\n    steps: [{run: true}]\n",
		"push.yml":         "name: Push\non: push\njobs:\n  test:\n    runs-on: ${{ github.event.push_runner }}\n    steps: [{run: true}]\n",
		"schedule.yml":     "name: Schedule\non:\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	events := map[string]string{
		"push": writeUploadEvent(t, repository, "push", "refs/heads/main", map[string]any{"push_runner": "ubuntu-latest"}),
		"pull_request": writeUploadEvent(t, repository, "pull_request", "refs/pull/42/head", map[string]any{
			"action":       "opened",
			"pull_request": map[string]any{"runner": "ubuntu-latest", "base": map[string]any{"ref": "main"}},
		}),
		"workflow_dispatch": writeUploadEvent(t, repository, "workflow_dispatch", "refs/heads/main", map[string]any{}),
		"schedule":          writeUploadEvent(t, repository, "schedule", "refs/heads/main", map[string]any{}),
	}
	workflowPaths := []string{
		".github/workflows/dispatch.yml",
		".github/workflows/pull-request.yml",
		".github/workflows/push.yml",
		".github/workflows/schedule.yml",
	}
	for _, test := range []struct {
		name, event, source, workflow string
	}{
		{name: "API build with explicit push", event: "push", source: "api", workflow: "Push"},
		{name: "explicit pull request excludes push data", event: "pull_request", source: "schedule", workflow: "Pull request"},
		{name: "explicit dispatch excludes push and PR", event: "workflow_dispatch", source: "webhook", workflow: "Dispatch"},
		{name: "explicit schedule excludes dispatch", event: "schedule", source: "api", workflow: "Schedule"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(repository)
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "explicit-event-importer")
			t.Setenv("BUILDKITE_SOURCE", test.source)
			runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
			var stdout, stderr bytes.Buffer
			args := append([]string{"upload", "--event-path", events[test.event]}, workflowPaths...)
			if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
			}
			var pipeline struct {
				Steps []struct {
					Group     string `yaml:"group"`
					Condition string `yaml:"if"`
					Skip      string `yaml:"skip"`
					Notify    any    `yaml:"notify"`
					Steps     []struct {
						Notify []struct {
							GitHubCheck struct {
								Name string `yaml:"name"`
							} `yaml:"github_check"`
						} `yaml:"notify"`
					} `yaml:"steps"`
				} `yaml:"steps"`
			}
			if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
				t.Fatal(err)
			}
			wantGroup := ":github: " + test.workflow
			wantCheck := test.workflow + " / test (" + test.event + ")"
			if len(pipeline.Steps) != 4 {
				t.Fatalf("effective-event pipeline = %#v, want all workflow groups", pipeline.Steps)
			}
			activeFound := false
			for _, group := range pipeline.Steps {
				if group.Group == wantGroup {
					activeFound = true
					if !strings.Contains(group.Condition, "true") || strings.Contains(group.Condition, "build.") || group.Skip != "" || group.Notify != nil || len(group.Steps) != 1 || len(group.Steps[0].Notify) != 1 || group.Steps[0].Notify[0].GitHubCheck.Name != wantCheck {
						t.Fatalf("active event group = %#v, want group %q and check %q", group, wantGroup, wantCheck)
					}
					continue
				}
				if group.Condition != "" || group.Skip != "This workflow is not triggered by a `"+test.event+"` event" {
					t.Fatalf("inactive event group = %#v", group)
				}
			}
			if !activeFound {
				t.Fatalf("effective-event pipeline = %#v, want group %q", pipeline.Steps, wantGroup)
			}
			if !strings.Contains(stdout.String(), "from 4 workflows") {
				t.Fatalf("stdout = %q", stdout.String())
			}
			for path, contents := range runner.uploaded {
				if !strings.HasSuffix(path, ".json") {
					continue
				}
				job, err := plan.Decode(contents)
				if err != nil {
					t.Fatal(err)
				}
				if job.Event.Name != test.event {
					t.Fatalf("plan event = %q, want %q", job.Event.Name, test.event)
				}
			}
		})
	}
}

func TestRunUploadAlignsBuildkiteFallbackWithEffectiveEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"dispatch.yml":     "name: Dispatch\non: workflow_dispatch\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		"pull-request.yml": "name: Pull request\non:\n  pull_request:\n    branches: [main]\n    types: [synchronize]\njobs:\n  test:\n    runs-on: ${{ github.event.pull_request.base.ref == 'main' && 'ubuntu-latest' || 'ubuntu-22.04' }}\n    steps: [{run: true}]\n",
		"push.yml":         "name: Push\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		"schedule.yml":     "name: Schedule\non:\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	workflowPaths := []string{
		".github/workflows/dispatch.yml",
		".github/workflows/pull-request.yml",
		".github/workflows/push.yml",
		".github/workflows/schedule.yml",
	}
	for _, test := range []struct {
		name, source, event, workflow string
		pullRequest                   bool
	}{
		{name: "trigger job push", source: "trigger_job", event: "push", workflow: "Push"},
		{name: "trigger job pull request", source: "trigger_job", event: "pull_request", workflow: "Pull request", pullRequest: true},
		{name: "UI dispatch", source: "ui", event: "workflow_dispatch", workflow: "Dispatch"},
		{name: "API dispatch", source: "api", event: "workflow_dispatch", workflow: "Dispatch"},
		{name: "schedule", source: "schedule", event: "schedule", workflow: "Schedule"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(repository)
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "fallback-event-importer")
			t.Setenv("BUILDKITE_REPO", "https://github.com/buildkite/buildkite-gha")
			t.Setenv("BUILDKITE_COMMIT", strings.Repeat("a", 40))
			t.Setenv("BUILDKITE_BRANCH", "main")
			t.Setenv("BUILDKITE_TAG", "")
			if test.pullRequest {
				t.Setenv("BUILDKITE_PULL_REQUEST", "42")
				t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "main")
			} else {
				t.Setenv("BUILDKITE_PULL_REQUEST", "false")
			}
			t.Setenv("BUILDKITE_SOURCE", test.source)
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			args := append([]string{"upload"}, workflowPaths...)
			if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
			}
			var pipeline struct {
				Steps []struct {
					Group     string `yaml:"group"`
					Condition string `yaml:"if"`
					Skip      string `yaml:"skip"`
				} `yaml:"steps"`
			}
			if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
				t.Fatal(err)
			}
			wantCondition := `build.source == "` + test.source + `"`
			if len(pipeline.Steps) != 4 {
				t.Fatalf("fallback pipeline = %#v, want all workflow groups", pipeline.Steps)
			}
			activeFound := false
			for _, group := range pipeline.Steps {
				if group.Group == ":github: "+test.workflow {
					activeFound = true
					if !strings.Contains(group.Condition, wantCondition) || strings.Contains(group.Condition, "source_event") || group.Skip != "" {
						t.Fatalf("active fallback group = %#v, want event %q and condition %q", group, test.event, wantCondition)
					}
					if test.pullRequest && (!strings.Contains(group.Condition, `"main" =~ /^main$/`) || !strings.Contains(group.Condition, `"synchronize" == "synchronize"`) || strings.Contains(group.Condition, "build.pull_request") || strings.Contains(group.Condition, "build.source_action")) {
						t.Fatalf("fallback pull-request filters do not use the effective snapshot: %q", group.Condition)
					}
					continue
				}
				if group.Condition != "" || group.Skip != "This workflow is not triggered by a `"+test.event+"` event" {
					t.Fatalf("inactive fallback group = %#v", group)
				}
			}
			if !activeFound {
				t.Fatalf("fallback pipeline = %#v, want workflow %q", pipeline.Steps, test.workflow)
			}
		})
	}
}

func TestRunUploadEmitsApplicableCompilationFailuresAsFailingSteps(t *testing.T) {
	requireImporterHost(t)
	directory := t.TempDir()
	workflowPath := filepath.Join(directory, "invalid-push.yml")
	workflow := "name: Invalid push\non:\n  push:\n    branches: [main]\njobs:\n  alpha:\n    runs-on: ${{ github.event.runner }}\n    steps: [{run: true}]\n  beta:\n    runs-on: ${{ github.event.runners.event_secret_key }}\n    steps: [{run: true}]\n  gamma:\n    runs-on: ${{ fromJSON(github.event.runner_json) }}\n    steps: [{run: true}]\n"
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	const valueSentinel = "EVENT-DERIVED-RUNNER-SENTINEL"
	const keySentinel = "EVENT_SECRET_KEY"
	const jsonSentinel = "@"
	eventPath := writeUploadEvent(t, directory, "push", "refs/heads/chore_updates", map[string]any{
		"runner":      valueSentinel,
		"runner_json": jsonSentinel,
		"runners": map[string]any{
			keySentinel:                  "one",
			strings.ToLower(keySentinel): "two",
		},
	})
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "failing-applicable-importer")
	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), valueSentinel) || strings.Contains(stderr.String(), valueSentinel) || strings.Contains(stdout.String(), keySentinel) || strings.Contains(stderr.String(), keySentinel) || strings.Contains(stdout.String(), strings.ToLower(keySentinel)) || strings.Contains(stderr.String(), strings.ToLower(keySentinel)) || strings.Contains(stdout.String(), jsonSentinel) || strings.Contains(stderr.String(), jsonSentinel) {
		t.Fatalf("event-derived runner leaked to output: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Group     string             `yaml:"group"`
			Label     string             `yaml:"label"`
			Condition string             `yaml:"if"`
			Command   string             `yaml:"command"`
			Plugins   failureStepPlugins `yaml:"plugins"`
			Notify    []struct {
				GitHubCheck struct {
					Output struct {
						Title   string `yaml:"title"`
						Summary string `yaml:"summary"`
					} `yaml:"output"`
				} `yaml:"github_check"`
			} `yaml:"notify"`
			Checkout struct {
				Skip bool `yaml:"skip"`
			} `yaml:"checkout"`
			Steps []any `yaml:"steps"`
		} `yaml:"steps"`
	}
	pipelineCommand := runner.commands[len(runner.commands)-1]
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != "" || len(pipeline.Steps[0].Steps) != 0 {
		t.Fatalf("compiler failure pipeline = %#v\n%s", pipeline.Steps, pipelineCommand.stdin)
	}
	step := pipeline.Steps[0]
	message := failureArtifactForStep(step.Plugins, runner.uploaded, "messages")
	annotation := failureArtifactForStep(step.Plugins, runner.uploaded, "annotations")
	if step.Label != ":github: Invalid push" || step.Condition != "" || !isGeneratedFailureCommand(step.Command) || strings.Contains(step.Command, "Runner label has no") || !strings.Contains(string(message), "Runner label has no") || !strings.Contains(string(message), "detail: Supported runner labels:") || !strings.Contains(string(annotation), `<h2 class="h4 mb2">Workflow could not be run</h2>`) || !strings.Contains(string(annotation), "Job <code>alpha</code>") || !strings.Contains(string(annotation), "Job <code>beta</code>") || !strings.Contains(string(annotation), "Job <code>gamma</code>") || len(step.Notify) != 1 || step.Notify[0].GitHubCheck.Output.Title != "Workflow could not be run" || strings.Contains(step.Notify[0].GitHubCheck.Output.Summary, "<h2") || !strings.Contains(step.Notify[0].GitHubCheck.Output.Summary, "<p>") || !step.Checkout.Skip {
		t.Fatalf("compiler failure step = %#v", step)
	}
	if strings.Contains(step.Notify[0].GitHubCheck.Output.Summary, "E_EXPRESSION_INVALID") {
		t.Fatalf("compiler failure check summary contains diagnostic code: %q", step.Notify[0].GitHubCheck.Output.Summary)
	}
	if len(step.Plugins) != 1 {
		t.Fatalf("compiler failure plugins = %#v", step.Plugins)
	}
	plugin, ok := step.Plugins[0]["artifacts#v1.9.4"]
	if !ok {
		t.Fatalf("compiler failure plugins = %#v", step.Plugins)
	}
	commandDirectory := t.TempDir()
	for _, download := range plugin.Download {
		if err := os.WriteFile(filepath.Join(commandDirectory, download.To), runner.uploaded[download.From], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agentDirectory := t.TempDir()
	agentPath := filepath.Join(agentDirectory, "buildkite-agent")
	agentScript := `#!/bin/sh
if [ "$1" = annotate ]; then
  cat
else
  exit 2
fi
`
	if err := os.WriteFile(agentPath, []byte(agentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", step.Command)
	command.Dir = commandDirectory
	command.Env = append(os.Environ(), "PATH="+agentDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	plainIndex := strings.Index(string(output), "Runner label has no")
	annotationIndex := strings.Index(string(output), `<h2 class="h4 mb2">Workflow could not be run</h2>`)
	logPrefix := "\x1b[31m"
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 || !strings.HasPrefix(string(output), logPrefix) || plainIndex == -1 || annotationIndex <= plainIndex {
		t.Fatalf("compiler failure command output/error = %q / %v", output, err)
	}
	if strings.Contains(string(pipelineCommand.stdin), valueSentinel) || strings.Contains(string(pipelineCommand.stdin), keySentinel) || strings.Contains(string(pipelineCommand.stdin), strings.ToLower(keySentinel)) || strings.Contains(string(pipelineCommand.stdin), jsonSentinel) {
		t.Fatalf("event-derived runner leaked to aggregate pipeline: %s", pipelineCommand.stdin)
	}
}

func TestFailedGeneratedWorkflowIncludesWarnings(t *testing.T) {
	report := compatibility.NewProcessingReport("ci.yml", "hosted")
	report.Diagnostics = append(report.Diagnostics,
		compatibility.Diagnostic{Level: "warning", Code: "W_CONCURRENCY", Message: "cancel-in-progress is ignored"},
		compatibility.Diagnostic{Level: "error", Code: "E_RUNNER", Message: "runner is unsupported", Job: "test"},
	)

	workflow, artifacts := failedGeneratedWorkflow(workflowInput{Name: "CI", CanonicalPath: ".github/workflows/ci.yml", Identity: "ci", TriggerCondition: "false"}, "push", report, sourceLinkContext{})
	if workflow.Condition != "" || workflow.Failure == nil || len(artifacts) != 2 || workflow.Failure.MessagePath != artifacts[0].Path || workflow.Failure.AnnotationPath != artifacts[1].Path || !bytes.HasPrefix(artifacts[0].Contents, []byte("\x1b[31m")) || !bytes.HasSuffix(artifacts[0].Contents, []byte("\x1b[0m\n")) || !strings.Contains(string(artifacts[1].Contents), `<h2 class="h4 mb2">Workflow could not be run</h2>`) || !strings.Contains(string(artifacts[1].Contents), "<strong>runner is unsupported</strong>") || !strings.Contains(string(artifacts[1].Contents), "<strong>cancel-in-progress is ignored</strong>") || !strings.Contains(string(artifacts[1].Contents), "<p>") || strings.Contains(workflow.Failure.Summary, "<h2") || !strings.Contains(workflow.Failure.Summary, "<p>") {
		t.Fatalf("failure = %#v", workflow.Failure)
	}
}

func TestFailedGeneratedWorkflowKeepsLargeDiagnosticsOutOfCommand(t *testing.T) {
	message := strings.Repeat("large diagnostic ", 16*1024)
	report := compatibility.NewProcessingReport("ci.yml", "hosted")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{Level: "error", Message: message})
	workflow, artifacts := failedGeneratedWorkflow(workflowInput{Name: "CI", CanonicalPath: "ci.yml", Identity: "ci", TriggerCondition: "true"}, "push", report, sourceLinkContext{})

	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{CompilerStep: "importer", EventProvider: "github", Workflows: []buildkitepipeline.Workflow{workflow}})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Command string             `yaml:"command"`
			Plugins failureStepPlugins `yaml:"plugins"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(pipeline, &document); err != nil {
		t.Fatal(err)
	}
	uploaded := make(map[string][]byte, len(artifacts))
	for _, artifact := range artifacts {
		uploaded[artifact.Path] = artifact.Contents
	}
	step := document.Steps[0]
	command := step.Command
	plain := failureArtifactForStep(step.Plugins, uploaded, "messages")
	annotation := failureArtifactForStep(step.Plugins, uploaded, "annotations")
	if !isGeneratedFailureCommand(command) || strings.Contains(command, message[:1024]) || len(plain) < 128*1024 || len(annotation) < 128*1024 {
		t.Fatalf("command bytes = %d, message artifact bytes = %d, annotation artifact bytes = %d", len(command), len(plain), len(annotation))
	}
}

func TestFailureCheckSummaryFitsProviderLimit(t *testing.T) {
	report := compatibility.NewProcessingReport("ci.yml", "hosted")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: strings.Repeat("x", workflowCheckSummaryLimit) + "🙂", Job: "test",
	})

	_, summary := processingAnnotationWithin(report, sourceLinkContext{}, workflowCheckSummaryLimit, workflowCheckSummaryNotice, false)
	if len(summary) > workflowCheckSummaryLimit || !utf8.ValidString(summary) || !strings.HasSuffix(summary, workflowCheckSummaryNotice) {
		t.Fatalf("truncated provider check summary is invalid: bytes=%d, valid UTF-8=%t, suffix=%q", len(summary), utf8.ValidString(summary), summary[len(summary)-100:])
	}
}

func TestFailureCheckSummaryUsesAnnotationMarkupAndLinks(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "hello.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	report := compatibility.NewProcessingReport(".github/workflows/hello.yml", "hosted")
	report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
		Level: "error", Message: "runner is unsupported", Job: "test",
		Location: &compatibility.SourceLocation{Path: ".github/workflows/hello.yml", Line: 100, Column: 3},
	})
	sourceLinks := sourceLinkContext{serverURL: "https://github.com", repository: "owner/repo", sha: "abc123"}
	want := `<a href="https://github.com/owner/repo/blob/abc123/.github/workflows/hello.yml#L100"><code>.github/workflows/hello.yml:100:3</code></a>`

	workflow, artifacts := failedGeneratedWorkflow(workflowInput{Name: "CI", CanonicalPath: ".github/workflows/hello.yml", Identity: "ci"}, "push", report, sourceLinks)
	annotation := string(artifacts[1].Contents)
	if !strings.Contains(annotation, `<h2 class="h4 mb2">Workflow could not be run</h2>`) || strings.Contains(workflow.Failure.Summary, "<h2") || !strings.Contains(workflow.Failure.Summary, "<p>") || !strings.Contains(workflow.Failure.Summary, want) {
		t.Fatalf("check summary = %q, annotation = %q, want %q", workflow.Failure.Summary, annotation, want)
	}
}

func TestRunUploadEmitsUnsupportedJobCancellationAsFailingStep(t *testing.T) {
	requireImporterHost(t)
	directory := t.TempDir()
	workflowPath := filepath.Join(directory, "rebase-needed.yml")
	workflow := `name: Rebase needed
on: push
jobs:
  label-rebase-needed:
    runs-on: ubuntu-latest
    concurrency:
      group: rebase-needed
      cancel-in-progress: true
    steps: [{run: true}]
`
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "job-cancellation-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Group    string             `yaml:"group"`
			Label    string             `yaml:"label"`
			Command  string             `yaml:"command"`
			Plugins  failureStepPlugins `yaml:"plugins"`
			Checkout struct {
				Skip bool `yaml:"skip"`
			} `yaml:"checkout"`
		} `yaml:"steps"`
	}
	pipelineCommand := runner.commands[len(runner.commands)-1]
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("job cancellation pipeline = %#v\n%s", pipeline.Steps, pipelineCommand.stdin)
	}
	step := pipeline.Steps[0]
	message := failureArtifactForStep(step.Plugins, runner.uploaded, "messages")
	if step.Group != "" || step.Label != ":github: Rebase needed" || !isGeneratedFailureCommand(step.Command) || !strings.Contains(string(message), `job "label-rebase-needed": concurrency cancel-in-progress is unsupported`) || !step.Checkout.Skip {
		t.Fatalf("job cancellation failure step = %#v", step)
	}
}

func TestRunUploadContinuesAfterWorkflowCompilationFailures(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"a-invalid.yml": "name: Invalid\non: push\njobs:\n  first:\n    runs-on: windows-latest\n    steps: [{run: true}]\n  second:\n    runs-on: macos-15\n    steps: [{run: true}]\n",
		"b-action.yml":  "name: Missing action\non: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./missing-action\n",
		"c-success.yml": "name: Success\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", "mixed-failure-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"upload", "--event-path", eventPath,
		".github/workflows/a-invalid.yml",
		".github/workflows/b-action.yml",
		".github/workflows/c-success.yml",
	}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var annotationBodies []string
	for _, command := range runner.commands {
		if len(command.args) != 0 && command.args[0] == "annotate" {
			annotationBodies = append(annotationBodies, string(command.stdin))
		}
	}
	if len(annotationBodies) != 0 {
		t.Fatalf("mixed failures annotated the importer = %#v", annotationBodies)
	}
	var pipeline struct {
		Steps []struct {
			Group   string             `yaml:"group"`
			Label   string             `yaml:"label"`
			Command string             `yaml:"command"`
			Plugins failureStepPlugins `yaml:"plugins"`
			Steps   []struct {
				Label   string `yaml:"label"`
				Key     string `yaml:"key"`
				Command string `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 3 {
		t.Fatalf("aggregate pipeline groups = %#v", pipeline.Steps)
	}
	wantFailureLabels := []string{":github: Invalid", ":github: Missing action"}
	for i, step := range pipeline.Steps[:2] {
		if step.Group != "" || len(step.Steps) != 0 || step.Label != wantFailureLabels[i] || !isGeneratedFailureCommand(step.Command) {
			t.Fatalf("failed workflow step %d = %#v", i, step)
		}
	}
	firstFailureMessage := string(failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "messages"))
	if !strings.Contains(firstFailureMessage, `Runner label "windows-latest" requires Windows, which is unsupported. Use a Linux or macOS runner label.`) ||
		!strings.Contains(firstFailureMessage, `Runner label "macos-15" has no runner-target mapping. Configure a mapping for this label or use a mapped runner label.`) ||
		strings.Count(firstFailureMessage, "detail: Supported runner labels: macos-latest, ubuntu-22.04, ubuntu-24.04, ubuntu-latest.") != 2 {
		t.Fatalf("multi-diagnostic failure message = %q", firstFailureMessage)
	}
	actionFailureAnnotation := string(failureArtifactForStep(pipeline.Steps[1].Plugins, runner.uploaded, "annotations"))
	if !strings.Contains(actionFailureAnnotation, `Resolve local action &#34;missing-action&#34;`) || !strings.Contains(actionFailureAnnotation, "no such file or directory") {
		t.Fatalf("action failure annotation = %q", actionFailureAnnotation)
	}
	if pipeline.Steps[2].Group != ":github: Success" || len(pipeline.Steps[2].Steps) != 1 || pipeline.Steps[2].Steps[0].Key == "" || !strings.Contains(pipeline.Steps[2].Steps[0].Command, `run-job --plan "$plan"`) || !strings.Contains(pipeline.Steps[2].Steps[0].Command, "--user runner") {
		t.Fatalf("successful workflow group = %#v", pipeline.Steps[2])
	}
}

func TestRunUploadExplainsWhenNoWorkflowsApply(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join(t.TempDir(), "pull-request.yml")
	if err := os.WriteFile(workflowPath, []byte("on: pull_request\njobs:\n  test:\n    runs-on: ${{ github.event.pull_request.runner }}\n    steps: [{run: true}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_URL", "https://buildkite.com/acme/widgets/builds/42")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_STEP_KEY", "no-applicable-importer")
	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 0 jobs from 1 workflows") || stderr.Len() != 0 || len(runner.commands) == 0 {
		t.Fatalf("stdout/stderr/commands/uploads = %q / %q / %#v / %#v", stdout.String(), stderr.String(), runner.commands, runner.uploaded)
	}
	var pipeline struct {
		Steps []struct {
			Group   string `yaml:"group"`
			Label   string `yaml:"label"`
			Key     string `yaml:"key"`
			Type    string `yaml:"type"`
			Skip    string `yaml:"skip"`
			Command string `yaml:"command"`
			Steps   []any  `yaml:"steps"`
		} `yaml:"steps"`
	}
	var pipelineSource []byte
	var annotation *cliCommand
	for i := range runner.commands {
		command := &runner.commands[i]
		if slices.Equal(command.args, []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets"}) {
			pipelineSource = command.stdin
		}
		if len(command.args) > 0 && command.args[0] == "annotate" {
			annotation = command
		}
	}
	if err := yaml.Unmarshal(pipelineSource, &pipeline); err != nil {
		t.Fatal(err)
	}
	wantLabel := filepath.ToSlash(filepath.Clean(workflowPath))
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != "" || pipeline.Steps[0].Label != ":github: "+wantLabel || pipeline.Steps[0].Key == "" || pipeline.Steps[0].Type != "command" || pipeline.Steps[0].Skip != "This workflow is not triggered by a `push` event" || pipeline.Steps[0].Command != "" || len(pipeline.Steps[0].Steps) != 0 {
		t.Fatalf("ignored-only pipeline = %#v", pipeline.Steps)
	}
	wantAnnotationArgs := []string{"annotate", "--scope", "job", "--job", cliTestJobID, "--context", skippedWorkflowsContext, "--style", "info"}
	wantWorkflows := []skippedWorkflow{{label: wantLabel, key: pipeline.Steps[0].Key, reason: "This workflow is not triggered by a `push` event"}}
	if annotation == nil || !slices.Equal(annotation.args, wantAnnotationArgs) || string(annotation.stdin) != skippedWorkflowsAnnotation("push", wantWorkflows, "https://buildkite.com/acme/widgets/builds/42") {
		t.Fatalf("skipped workflow annotation = %#v", annotation)
	}
	for path := range runner.uploaded {
		if strings.HasSuffix(path, ".json") {
			t.Fatalf("ignored workflow compiled plan %q", path)
		}
	}
}

func TestRunUploadExplainsWhenWorkflowTriggerFiltersDoNotMatch(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"ci.yml": "name: CI\non:\n  push:\n    branches: [main, development]\n  workflow_dispatch:\n  pull_request:\njobs:\n  lint:\n    runs-on: ubuntu-latest\n    steps: [{run: yarn lint}]\n",
	})
	eventPath := writeUploadEvent(t, repository, "push", "refs/heads/feature", map[string]any{})
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_URL", "https://buildkite.com/acme/widgets/builds/42")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_STEP_KEY", "filter-mismatch-importer")
	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, ".github/workflows/ci.yml"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	for _, command := range runner.commands {
		if slices.Equal(command.args, []string{"annotate", "--scope", "job", "--job", cliTestJobID, "--context", skippedWorkflowsContext, "--style", "info"}) {
			body := string(command.stdin)
			if !strings.Contains(body, "#### 1 workflow was skipped") || !strings.Contains(body, ":github: CI") || !strings.Contains(body, "Only runs on `main` or `development`.") {
				t.Fatalf("skipped workflow annotation = %q", body)
			}
			return
		}
	}
	t.Fatal("skipped workflow annotation was not published")
}

func TestSkippedWorkflowsAnnotation(t *testing.T) {
	for _, test := range []struct {
		name      string
		workflows []skippedWorkflow
		want      string
	}{
		{
			name:      "singular",
			workflows: []skippedWorkflow{{label: "CI", key: "gha-workflow-ci", reason: "This workflow is not triggered by a `push` event"}},
			want: "#### 1 workflow was skipped\n\n" +
				"The current <code>push</code> event does not match these workflows:\n\n" +
				"* [:github: CI](https://buildkite.com/acme/widgets/builds/42/canvas?key=gha-workflow-ci&open=false) — This workflow is not triggered by a `push` event\n",
		},
		{
			name: "plural",
			workflows: []skippedWorkflow{
				{label: "CI", key: "gha-workflow-ci", reason: "Only runs on `main` or `development`."},
				{label: "Release [production]", key: "gha-workflow-release?production", reason: "This workflow is not triggered by a `push` event"},
			},
			want: "#### 2 workflows were skipped\n\n" +
				"The current <code>push</code> event does not match these workflows:\n\n" +
				"* [:github: CI](https://buildkite.com/acme/widgets/builds/42/canvas?key=gha-workflow-ci&open=false) — Only runs on `main` or `development`.\n" +
				"* [:github: Release \\[production\\]](https://buildkite.com/acme/widgets/builds/42/canvas?key=gha-workflow-release%3Fproduction&open=false) — This workflow is not triggered by a `push` event\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := skippedWorkflowsAnnotation(
				"push",
				test.workflows,
				"https://buildkite.com/acme/widgets/builds/42",
			)
			if body != test.want {
				t.Fatalf("skipped workflow annotation = %q, want %q", body, test.want)
			}
		})
	}
}

func writeUploadWorkflowRepository(t *testing.T, sources map[string]string) string {
	t.Helper()
	repository := canonicalTempDir(t)
	workflowDirectory := filepath.Join(repository, ".github", "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, source := range sources {
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", repository}, {"-C", repository, "add", ".github/workflows"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return repository
}

func writeUploadEvent(t *testing.T, directory, event, ref string, payload map[string]any) string {
	t.Helper()
	source, err := json.Marshal(map[string]any{
		"provider": "github",
		"event":    event,
		"repository": map[string]any{
			"owner": "buildkite",
			"name":  "buildkite-gha",
		},
		"ref":     ref,
		"sha":     strings.Repeat("a", 40),
		"actor":   "octocat",
		"payload": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, event+".json")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunUploadEmitsTriggerFailuresAsFailingSteps(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"crowdin-upload.yml": "name: Crowdin upload\non:\n  push:\n    paths: [\"crowdin/**\"]\njobs:\n  upload:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		"success.yml":        "name: Success\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", "trigger-failure-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"upload", "--event-path", eventPath,
		".github/workflows/crowdin-upload.yml",
		".github/workflows/success.yml",
	}, &stdout, &stderr, "dev", runner); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
	}
	var annotationBodies []string
	for _, command := range runner.commands {
		if len(command.args) != 0 && command.args[0] == "annotate" {
			annotationBodies = append(annotationBodies, string(command.stdin))
		}
	}
	if len(annotationBodies) != 0 {
		t.Fatalf("trigger failure annotated the importer = %#v", annotationBodies)
	}
	var pipeline struct {
		Steps []struct {
			Group     string             `yaml:"group"`
			Label     string             `yaml:"label"`
			Condition string             `yaml:"if"`
			Command   string             `yaml:"command"`
			Plugins   failureStepPlugins `yaml:"plugins"`
			Checkout  struct {
				Skip bool `yaml:"skip"`
			} `yaml:"checkout"`
			Steps []any `yaml:"steps"`
		} `yaml:"steps"`
	}
	pipelineCommand := runner.commands[len(runner.commands)-1]
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 2 {
		t.Fatalf("trigger failure pipeline = %#v\n%s", pipeline.Steps, pipelineCommand.stdin)
	}
	failure := pipeline.Steps[0]
	message := failureArtifactForStep(failure.Plugins, runner.uploaded, "messages")
	annotation := failureArtifactForStep(failure.Plugins, runner.uploaded, "annotations")
	primary := "Push trigger path filters could not be evaluated safely. Ensure the linked webhook and local checkout contain matching push history, or remove the path filters."
	detail := "push path filters are unsupported: push path filters require linked Buildkite webhook data"
	if failure.Group != "" || failure.Label != ":github: Crowdin upload" || failure.Condition != "" || !isGeneratedFailureCommand(failure.Command) || !strings.Contains(string(message), primary) || !strings.Contains(string(message), "detail: "+detail) || !strings.Contains(string(annotation), "<strong>Push trigger path filters could not be evaluated safely.</strong>") || !strings.Contains(string(annotation), "matching push history") || !strings.Contains(string(annotation), detail) || strings.Contains(string(message), "translate workflow triggers") || strings.Contains(string(message), ".github/workflows/crowdin-upload.yml") || !failure.Checkout.Skip || len(failure.Steps) != 0 {
		t.Fatalf("trigger failure step = %#v, message = %q, annotation = %q", failure, message, annotation)
	}
	if success := pipeline.Steps[1]; success.Group != ":github: Success" || len(success.Steps) != 1 {
		t.Fatalf("successful workflow after trigger failure = %#v", success)
	}
	if !strings.Contains(stdout.String(), "Pipeline generation: failed") || !strings.Contains(stdout.String(), compiler.CodePipelineGeneration) || !strings.Contains(stdout.String(), primary) || !strings.Contains(stdout.String(), "detail: "+detail) {
		t.Fatalf("trigger failure omitted processing report: %q", stdout.String())
	}
}

func TestRunUploadAppliesPullRequestPathFiltersFromGitDiff(t *testing.T) {
	requireImporterHost(t)
	workflowJobs := "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	workflowSource := "on:\n  pull_request:\n    paths: [\"src/**\"]\n" + workflowJobs
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"ci.yml":       "name: CI\n" + workflowSource,
		"mismatch.yml": "name: Original\n" + workflowSource,
	})
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "-qm", "base")
	base := runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", base)
	if err := os.Mkdir(filepath.Join(repository, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "src", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "src/main.go")
	runGit("commit", "-qm", "head")
	head := runGit("rev-parse", "HEAD")
	merge := runGit("commit-tree", head+"^{tree}", "-p", base, "-p", head, "-m", "merge")
	if err := os.WriteFile(filepath.Join(repository, ".github", "workflows", "mismatch.yml"), []byte("name: Local mismatch\non: pull_request\n"+workflowJobs), 0o600); err != nil {
		t.Fatal(err)
	}
	webhook, err := json.Marshal(map[string]any{
		"action": "opened", "number": 42,
		"pull_request": map[string]any{
			"base":             map[string]any{"ref": "main", "sha": base, "repo": map[string]any{"full_name": "buildkite/buildkite-gha"}},
			"head":             map[string]any{"ref": "feature", "sha": head, "repo": map[string]any{"full_name": "contributor/buildkite-gha"}},
			"mergeable":        true,
			"merge_commit_sha": merge,
		},
		"sender": map[string]any{"login": "octocat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", "path-filter-importer")
	t.Setenv("BUILDKITE_REPO", "https://github.com/buildkite/buildkite-gha")
	t.Setenv("BUILDKITE_COMMIT", head)
	t.Setenv("BUILDKITE_BRANCH", "contributor:feature")
	t.Setenv("BUILDKITE_TAG", "")
	t.Setenv("BUILDKITE_PULL_REQUEST", "42")
	t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "main")
	t.Setenv("BUILDKITE_PULL_REQUEST_REPO", "https://github.com/contributor/buildkite-gha")
	t.Setenv("BUILDKITE_SOURCE", "webhook")
	t.Setenv("BUILDKITE_GITHUB_EVENT", "pull_request")
	runner := &cliCaptureRunner{webhook: webhook}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", ".github/workflows/ci.yml", ".github/workflows/mismatch.yml"}, &stdout, &stderr, "dev", runner); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Label     string `yaml:"label"`
			Condition string `yaml:"if"`
			Command   string `yaml:"command"`
			Steps     []any  `yaml:"steps"`
		} `yaml:"steps"`
	}
	pipelineCommand := runner.commands[len(runner.commands)-1]
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 2 || pipeline.Steps[0].Group != ":github: CI" || strings.Contains(pipeline.Steps[0].Condition, "false") || len(pipeline.Steps[0].Steps) != 1 {
		t.Fatalf("path-filter pipeline = %#v\n%s", pipeline.Steps, pipelineCommand.stdin)
	}
	if failure := pipeline.Steps[1]; failure.Label != ":github: Local mismatch" || !isGeneratedFailureCommand(failure.Command) || failure.Group != "" || len(failure.Steps) != 0 {
		t.Fatalf("workflow mismatch failure = %#v\n%s", failure, pipelineCommand.stdin)
	}
}

func TestRunUploadEmitsReusableInputFailuresAsActionableFailingSteps(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"caller.yml":   "name: Caller\non: push\njobs:\n  middle:\n    uses: ./.github/workflows/middle.yml\n",
		"middle.yml":   "on: workflow_call\njobs:\n  prepare:\n    runs-on: ubuntu-latest\n    outputs:\n      target: ${{ steps.value.outputs.target }}\n    steps:\n      - id: value\n        run: echo target=test >> $GITHUB_OUTPUT\n  call:\n    needs: prepare\n    uses: ./.github/workflows/reusable.yml\n    with:\n      target: ${{ needs.prepare.outputs.target }}\n",
		"reusable.yml": "on:\n  workflow_call:\n    inputs:\n      target:\n        type: string\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "reusable-input-failure-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, ".github/workflows/caller.yml"}, &stdout, &stderr, "dev", runner); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Command string             `yaml:"command"`
			Plugins failureStepPlugins `yaml:"plugins"`
			Notify  []struct {
				GitHubCheck struct {
					Output struct {
						Summary string `yaml:"summary"`
					} `yaml:"output"`
				} `yaml:"github_check"`
			} `yaml:"notify"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 || !isGeneratedFailureCommand(pipeline.Steps[0].Command) {
		t.Fatalf("reusable input failure pipeline = %#v", pipeline.Steps)
	}
	primary := `Reusable workflow input "target" uses the needs context, which is unavailable before jobs run. Replace it with a literal or an expression that does not depend on job results.`
	detail := `Reusable-workflow input "target" is not statically resolvable: unsupported compile-time context "needs"`
	message := string(failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "messages"))
	annotation := string(failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "annotations"))
	if !strings.Contains(message, primary) || !strings.Contains(message, "detail: "+detail) || !strings.Contains(annotation, "<strong>Reusable workflow input &#34;target&#34; uses the needs context, which is unavailable before jobs run.</strong>") || !strings.Contains(annotation, "Replace it with a literal") || !strings.Contains(annotation, strings.ReplaceAll(detail, `"`, "&#34;")) || len(pipeline.Steps[0].Notify) != 1 || strings.Contains(pipeline.Steps[0].Notify[0].GitHubCheck.Output.Summary, "<h2") || !strings.Contains(pipeline.Steps[0].Notify[0].GitHubCheck.Output.Summary, "Reusable workflow input &#34;target&#34;") {
		t.Fatalf("reusable input failure output = message %q, annotation %q, pipeline %#v", message, annotation, pipeline.Steps[0])
	}
}

func TestRunUploadEmitsIncompletePullRequestSnapshotsAsFailingSteps(t *testing.T) {
	requireImporterHost(t)
	for _, test := range []struct {
		name, workflow, want string
		payload              map[string]any
	}{
		{
			name:     "missing action",
			workflow: "on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			payload:  map[string]any{"pull_request": map[string]any{"base": map[string]any{"ref": "main"}}},
			want:     "payload.action",
		},
		{
			name:     "missing filtered base branch",
			workflow: "on:\n  pull_request:\n    branches: [main]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			payload:  map[string]any{"action": "opened", "pull_request": map[string]any{}},
			want:     "payload.pull_request.base.ref",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{"pull-request.yml": test.workflow})
			eventPath := writeUploadEvent(t, repository, "pull_request", "refs/pull/42/head", test.payload)
			t.Chdir(repository)
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "incomplete-pull-request-importer")
			runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"upload", "--event-path", eventPath, ".github/workflows/pull-request.yml"}, &stdout, &stderr, "dev", runner); code != 0 || stderr.Len() != 0 {
				t.Fatalf("run() code/stderr = %d / %q, want %q", code, stderr.String(), test.want)
			}
			var pipeline struct {
				Steps []struct {
					Group   string             `yaml:"group"`
					Label   string             `yaml:"label"`
					Command string             `yaml:"command"`
					Plugins failureStepPlugins `yaml:"plugins"`
					Steps   []any              `yaml:"steps"`
				} `yaml:"steps"`
			}
			pipelineCommand := runner.commands[len(runner.commands)-1]
			if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
				t.Fatal(err)
			}
			command := pipeline.Steps[0].Command
			message := strings.ReplaceAll(string(failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "messages")), `\_`, "_")
			if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != "" || pipeline.Steps[0].Label != ":github: .github/workflows/pull-request.yml" || !isGeneratedFailureCommand(command) || !strings.Contains(message, test.want) || len(pipeline.Steps[0].Steps) != 0 {
				t.Fatalf("incomplete pull request failure step = %#v\n%s", pipeline.Steps, pipelineCommand.stdin)
			}
		})
	}
}

func TestRunEmitsUnclassifiablePushSnapshotAsFailingStep(t *testing.T) {
	requireImporterHost(t)
	workflow := "on:\n  push:\n    branches: [main]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	repository := writeUploadWorkflowRepository(t, map[string]string{"push.yml": workflow})
	eventPath := writeUploadEvent(t, repository, "push", "refs/pull/42/head", map[string]any{})
	workflowPath := filepath.Join(repository, ".github", "workflows", "push.yml")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("validate code/stderr = %d / %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "incompatible" || report.Admission.Result == "admitted" || len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, "refs/heads/") {
		t.Fatalf("malformed push profile report = %#v", report)
	}

	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "malformed-push-importer")
	runner := &cliCaptureRunner{}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"upload", "--event-path", eventPath, ".github/workflows/push.yml"}, &stdout, &stderr, "dev", runner); code != 0 || stderr.Len() != 0 {
		t.Fatalf("upload code/stderr = %d / %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Group     string             `yaml:"group"`
			Condition string             `yaml:"if"`
			Command   string             `yaml:"command"`
			Plugins   failureStepPlugins `yaml:"plugins"`
		} `yaml:"steps"`
	}
	pipelineCommand := runner.commands[len(runner.commands)-1]
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	message := failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "messages")
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != "" || pipeline.Steps[0].Condition != "" || !isGeneratedFailureCommand(pipeline.Steps[0].Command) || !strings.Contains(string(message), "refs/heads/") || strings.Contains(string(message), "null == null") {
		t.Fatalf("unclassifiable push failure step = %#v\n%s", pipeline.Steps, pipelineCommand.stdin)
	}
}

func TestRunUploadSkipsReusableOnlyMatchButCompilesItThroughCaller(t *testing.T) {
	requireImporterHost(t)
	repository := t.TempDir()
	workflowDirectory := filepath.Join(repository, ".github", "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	caller := "name: Caller\non: push\njobs:\n  imported:\n    uses: ./.github/workflows/reusable.yml\n"
	inactive := "name: Pull request only\non: pull_request\njobs:\n  test:\n    runs-on: ${{ github.event.pull_request.runner }}\n    steps: [{run: true}]\n"
	reusable := "name: Reusable\non: workflow_call\njobs:\n  shared:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	for name, source := range map[string]string{"caller.yml": caller, "pull-request.yml": inactive, "reusable.yml": reusable} {
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", repository}, {"-C", repository, "add", ".github/workflows"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "reusable-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"upload", "--event-path", eventPath,
		".github/workflows/reusable.yml",
		".github/workflows/pull-request.yml",
		".github/workflows/caller.yml",
	}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 1 jobs from 2 workflows") || len(runner.commands) != 3 {
		t.Fatalf("stdout/commands = %q / %d", stdout.String(), len(runner.commands))
	}
	var pipeline struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Label     string `yaml:"label"`
			Type      string `yaml:"type"`
			Skip      string `yaml:"skip"`
			Command   string `yaml:"command"`
			DependsOn string `yaml:"depends_on"`
			Notify    []struct {
				GitHubCheck struct {
					Name string `yaml:"name"`
				} `yaml:"github_check"`
			} `yaml:"notify"`
			Steps []struct {
				Key    string `yaml:"key"`
				Label  string `yaml:"label"`
				Notify []struct {
					GitHubCheck struct {
						Name string `yaml:"name"`
					} `yaml:"github_check"`
				} `yaml:"notify"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 2 {
		t.Fatalf("aggregate reusable pipeline = %#v", pipeline)
	}
	for _, workflow := range pipeline.Steps {
		switch workflow.Group {
		case ":github: Caller":
			if workflow.Skip != "" || workflow.DependsOn != "reusable-importer" || workflow.Notify != nil || len(workflow.Steps) != 1 || !strings.HasPrefix(workflow.Steps[0].Key, "gha-") || len(workflow.Steps[0].Notify) != 1 || workflow.Steps[0].Notify[0].GitHubCheck.Name != "Caller / imported.shared (push)" {
				t.Fatalf("caller group = %#v", workflow)
			}
		case "":
			if workflow.Label != ":github: Pull request only" || workflow.Type != "command" || workflow.Skip != "This workflow is not triggered by a `push` event" || workflow.Command != "" || len(workflow.Steps) != 0 {
				t.Fatalf("inactive workflow step = %#v", workflow)
			}
		default:
			t.Fatalf("reusable-only workflow became an aggregate group: %#v", workflow)
		}
	}
	compiledReusable := false
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		compiledReusable = job.Workflow.Path == "./.github/workflows/reusable.yml"
	}
	if !compiledReusable {
		t.Fatal("caller did not compile the reusable-only workflow into its plan")
	}
}

func TestRunUploadRejectsAllReusableOnlyMatches(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join(t.TempDir(), "reusable.yml")
	source := "on: workflow_call\njobs:\n  shared:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	if err := os.WriteFile(workflowPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "reusable-only-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", workflowPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "matched only reusable workflow_call workflows") {
		t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
	}
	if len(runner.commands) != 0 || len(runner.uploaded) != 0 {
		t.Fatalf("reusable-only upload reached Buildkite: commands %#v, uploads %#v", runner.commands, runner.uploaded)
	}
}

func TestRunUploadRejectsAllReusableExplicitPaths(t *testing.T) {
	requireImporterHost(t)
	reusable := "on: workflow_call\njobs:\n  shared:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"first.yml":  reusable,
		"second.yml": reusable,
	})
	t.Chdir(repository)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "reusable-list-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", ".github/workflows/second.yml", ".github/workflows/first.yml"}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "workflow paths matched only reusable workflow_call workflows") {
		t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
	}
	if stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
		t.Fatalf("reusable-only list reached Buildkite: stdout %q, commands %#v, uploads %#v", stdout.String(), runner.commands, runner.uploaded)
	}
}

func TestActionSourceAuthenticationUsesDedicatedTokenAndFallsBackAnonymously(t *testing.T) {
	const token = "ghs_action_source"
	provider := &cliActionSourceTokenProvider{token: token}
	redactor := &cliRedactor{}
	authentication := &actionSourceAuthentication{provider: provider, redactor: redactor}
	option := authentication.option("buildkite/buildkite-gha")
	if option == nil || provider.calls != 0 || len(redactor.values) != 0 {
		t.Fatalf("authentication = option %v, provider %#v, redactions %#v", option != nil, provider, redactor.values)
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/repos/o/r" {
			_, _ = io.WriteString(w, `{"private":false,"visibility":"public"}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}}`)
	}))
	defer server.Close()
	resolver, err := actionsource.NewResolver(server.Client(), option, actionsource.WithTestEndpoints(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	exact, err := actionsource.Parse("o/r@0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), exact); err != nil || requests != 0 || provider.calls != 0 || len(redactor.values) != 0 {
		t.Fatalf("exact Resolve() error/requests/provider/redactions = %v / %d / %d / %#v", err, requests, provider.calls, redactor.values)
	}
	ref, err := actionsource.Parse("o/r@v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), ref); err != nil || requests != 2 || provider.calls != 1 || provider.repository != "buildkite/buildkite-gha" || !slices.Equal(redactor.values, []string{token}) {
		t.Fatalf("Resolve() error/requests = %v / %d", err, requests)
	}
	second, _ := actionsource.Parse("o/r@v2")
	if _, err := resolver.Resolve(context.Background(), second); err != nil || provider.calls != 1 || !slices.Equal(redactor.values, []string{token}) {
		t.Fatalf("second Resolve() error/provider/redactions = %v / %d / %#v", err, provider.calls, redactor.values)
	}

	for _, test := range []struct {
		name          string
		provider      *cliActionSourceTokenProvider
		redactor      *cliRedactor
		cancelContext bool
		wantContext   bool
	}{
		{name: "unavailable", redactor: &cliRedactor{}},
		{name: "mint failure", provider: &cliActionSourceTokenProvider{err: errors.New("secret backend details")}, redactor: &cliRedactor{}},
		{name: "redaction failure", provider: &cliActionSourceTokenProvider{token: token}, redactor: &cliRedactor{err: errors.New("secret backend details")}},
		{name: "mint client timeout", provider: &cliActionSourceTokenProvider{err: context.DeadlineExceeded}, redactor: &cliRedactor{}},
		{name: "redaction client timeout", provider: &cliActionSourceTokenProvider{token: token}, redactor: &cliRedactor{err: context.DeadlineExceeded}},
		{name: "pre-cancelled while unavailable", redactor: &cliRedactor{}, cancelContext: true, wantContext: true},
		{name: "mint cancellation", provider: &cliActionSourceTokenProvider{err: context.Canceled}, redactor: &cliRedactor{}, wantContext: true},
		{name: "redaction cancellation", provider: &cliActionSourceTokenProvider{token: token}, redactor: &cliRedactor{err: context.Canceled}, wantContext: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var warnings bytes.Buffer
			ctx := context.Background()
			if test.cancelContext {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			authentication := &actionSourceAuthentication{redactor: test.redactor, warnings: &warnings}
			if test.provider != nil {
				authentication.provider = test.provider
			}
			gotToken, err := authentication.token(ctx, "buildkite/buildkite-gha")
			if test.wantContext {
				if !errors.Is(err, context.Canceled) || gotToken != "" || warnings.Len() != 0 {
					t.Fatalf("token/error/warnings = %q / %v / %q", gotToken, err, warnings.String())
				}
			} else if err != nil || gotToken != "" {
				t.Fatalf("token/error = %q / %v, want anonymous fallback", gotToken, err)
			} else if !strings.Contains(warnings.String(), "resolving mutable public action references anonymously") || strings.Contains(warnings.String(), token) || strings.Contains(warnings.String(), "backend details") {
				t.Fatalf("fallback warning = %q, want sanitized observable warning", warnings.String())
			}
		})
	}
}

func TestActionSourceAuthenticationReusesOneTokenAcrossConcurrentWorkflowResolvers(t *testing.T) {
	const token = "ghs_action_source"
	provider := &cliActionSourceTokenProvider{token: token}
	redactor := &cliRedactor{}
	authentication := &actionSourceAuthentication{provider: provider, redactor: redactor}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r" {
			_, _ = io.WriteString(w, `{"private":false,"visibility":"public"}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}}`)
	}))
	defer server.Close()

	const resolverCount = 11
	resolvers := make([]*actionsource.Resolver, resolverCount)
	for i := range resolvers {
		resolver, err := actionsource.NewResolver(server.Client(), authentication.option("buildkite/buildkite-gha"), actionsource.WithTestEndpoints(server.URL))
		if err != nil {
			t.Fatal(err)
		}
		resolvers[i] = resolver
	}
	ref, err := actionsource.Parse("o/r@v1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, resolverCount)
	for _, resolver := range resolvers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := resolver.Resolve(context.Background(), ref)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls != 1 || !slices.Equal(redactor.values, []string{token}) {
		t.Fatalf("provider calls/redactions = %d / %#v, want one importer-job mint and redaction", provider.calls, redactor.values)
	}
}

func TestHostedLocalActionDoesNotProvisionSourceToken(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "local.yml")
	actionPath := filepath.Join(root, ".github", "actions", "local")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	workflowSource := []byte("on: push\njobs:\n  local:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n")
	if err := os.WriteFile(workflowPath, workflowSource, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("runs:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventSource, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	originEventSource := bytes.Replace(eventSource, []byte(`"provider": "github"`), []byte(`"provider": "cursor-origin"`), 1)
	originEventSource = bytes.Replace(originEventSource, []byte(`"owner": "buildkite"`), []byte(`"owner": "acme_team"`), 1)
	originEventSource = bytes.Replace(originEventSource, []byte(`"name": "buildkite-gha"`), []byte(`"name": "widgets"`), 1)
	originEventSource = bytes.Replace(originEventSource, []byte(`"clone_url": "https://github.com/buildkite/buildkite-gha.git"`), []byte(`"clone_url": "https://origin.cursor.com/git/acme_team/widgets.git"`), 1)
	for _, test := range []struct {
		name  string
		event []byte
	}{
		{name: "GitHub", event: eventSource},
		{name: "Origin repository with GitHub-incompatible namespace", event: originEventSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &cliActionSourceTokenProvider{token: "must-not-be-minted"}
			redactor := &cliRedactor{}
			var warnings bytes.Buffer
			authentication := &actionSourceAuthentication{provider: provider, redactor: redactor, warnings: &warnings}
			if _, err := compileHosted(context.Background(), workflowPath, workflowSource, test.event, "dev", "sha256:"+strings.Repeat("0", 64), "importer", "", nil, nil, authentication); err != nil {
				t.Fatal(err)
			}
			if provider.calls != 0 || len(redactor.values) != 0 || warnings.Len() != 0 {
				t.Fatalf("local action provisioned source credential: calls %d, redactions %#v, warnings %q", provider.calls, redactor.values, warnings.String())
			}
		})
	}
}

func TestCompileHostedRequiresExplicitMacOSQueueAndRuntime(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  macos:\n    runs-on: macos-15\n    steps:\n      - run: echo macos\n")
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	compilerDigest := "sha256:" + strings.Repeat("0", 64)
	darwinDigest := "sha256:" + strings.Repeat("1", 64)
	_, err = compileHosted(context.Background(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `runner label "macos-15" is not mapped by policy`) {
		t.Fatalf("missing macOS queue error = %v", err)
	}
	macOSTarget := map[string]compiler.RunnerTarget{"macos-15": {Queue: "macos", Platform: compiler.PlatformDarwinARM64}}
	_, err = compileHosted(context.Background(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", macOSTarget, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no runtime distribution configured for darwin/arm64") {
		t.Fatalf("missing macOS runtime error = %v", err)
	}
	compiled, err := compileHosted(context.Background(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", macOSTarget, map[compiler.Platform]string{compiler.PlatformDarwinARM64: darwinDigest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Bundle.Plans) != 1 || compiled.Bundle.Plans[0].Job.Target.Queue != "macos" || compiled.Bundle.Plans[0].Job.RuntimeDistributionDigest() != darwinDigest || !bytes.Contains(compiled.Bundle.Pipeline, []byte("queue: \"macos\"")) {
		t.Fatalf("macOS compilation = %#v\n%s", compiled.Bundle.Plans, compiled.Bundle.Pipeline)
	}
}

func TestCompileHostedDefaultsMacOSLatestAliasToHostedQueue(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  macos:\n    runs-on: macOS-latest\n    steps:\n      - run: echo macos\n")
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	compilerDigest := "sha256:" + strings.Repeat("0", 64)
	darwinDigest := "sha256:" + strings.Repeat("1", 64)
	_, err = compileHosted(context.Background(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no runtime distribution configured for darwin/arm64") {
		t.Fatalf("unmapped macos-latest without Darwin runtime error = %v", err)
	}
	darwinRuntime := map[compiler.Platform]string{compiler.PlatformDarwinARM64: darwinDigest}
	compiled, err := compileHosted(context.Background(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", nil, darwinRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Bundle.Plans) != 1 || compiled.Bundle.Plans[0].Job.Target.Queue != defaultMacOSRunnerQueue || compiled.Bundle.Plans[0].Job.RuntimeDistributionDigest() != darwinDigest || !bytes.Contains(compiled.Bundle.Pipeline, []byte("queue: \"macos-medium\"")) {
		t.Fatalf("default macos-latest compilation = %#v\n%s", compiled.Bundle.Plans, compiled.Bundle.Pipeline)
	}
	override := map[string]compiler.RunnerTarget{"macos-latest": {Queue: "custom-macos", Platform: compiler.PlatformDarwinARM64}}
	compiled, err = compileHosted(context.Background(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", override, darwinRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Bundle.Plans) != 1 || compiled.Bundle.Plans[0].Job.Target.Queue != "custom-macos" || !bytes.Contains(compiled.Bundle.Pipeline, []byte("queue: \"custom-macos\"")) {
		t.Fatalf("overridden macos-latest compilation = %#v\n%s", compiled.Bundle.Plans, compiled.Bundle.Pipeline)
	}
}

func TestJobScopedActionSourceAuthenticationIgnoresAmbientGitHubTokens(t *testing.T) {
	t.Setenv("GH_TOKEN", "ignored")
	t.Setenv("GITHUB_TOKEN", "ignored")
	t.Setenv("BUILDKITE_GHA_GITHUB_TOKEN", "ignored")
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", "")
	t.Setenv("BUILDKITE_JOB_ID", "")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "")
	var warnings bytes.Buffer
	authentication := importerJobActionSourceAuthentication(&warnings)
	token, err := authentication.token(context.Background(), "buildkite/buildkite-gha")
	if err != nil || token != "" || authentication.provider != nil || !strings.Contains(warnings.String(), "authentication is unavailable") {
		t.Fatalf("ambient GitHub token authentication = provider %v, token %q, error %v, warnings %q", authentication.provider != nil, token, err, warnings.String())
	}

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	if authentication := importerJobActionSourceAuthentication(io.Discard); authentication.provider == nil {
		t.Fatal("job-scoped Agent configuration did not configure action-source authentication")
	}
}

func TestRunUploadUsesExplicitTargetQueueAndRunnerUserDefault(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "explicit-queue-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, "--runner-queue", "ubuntu-latest=hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}

	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Key     string            `yaml:"key"`
				Image   string            `yaml:"image"`
				Agents  map[string]string `yaml:"agents"`
				Command string            `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 1 || len(pipeline.Steps[0].Steps) != 3 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps[0].Steps {
		if step.Agents["queue"] != "hosted" {
			t.Fatalf("step %q agents = %#v, want hosted queue", step.Key, step.Agents)
		}
		if step.Image != defaultNobleRunnerImage || !strings.Contains(step.Command, "--hosted-tool-cache") || !strings.Contains(step.Command, "useradd --create-home") || !strings.Contains(step.Command, "sudo -n --preserve-env --user runner") {
			t.Fatalf("step %q image = %q, command = %q", step.Key, step.Image, step.Command)
		}
	}

	planCount := 0
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatalf("decode plan %q: %v", path, err)
		}
		planCount++
		if job.Schema != plan.Schema || job.Target.Queue != "hosted" {
			t.Fatalf("explicitly targeted plan = schema %q, target %#v", job.Schema, job.Target)
		}
	}
	if planCount != 3 {
		t.Fatalf("uploaded plan count = %d, want 3", planCount)
	}

	disabledRunner := &cliCaptureRunner{}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"upload", "--event-path", eventPath, "--runner-queue", "ubuntu-latest=hosted", "--experimental-runner-user=false", workflowPath}, &stdout, &stderr, "dev", disabledRunner); code != 0 {
		t.Fatalf("opt-out run() code = %d, stderr = %q", code, stderr.String())
	}
	var disabledPipeline struct {
		Steps []struct {
			Steps []struct {
				Command string `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(disabledRunner.commands[len(disabledRunner.commands)-1].stdin, &disabledPipeline); err != nil {
		t.Fatalf("opt-out pipeline YAML: %v", err)
	}
	for _, step := range disabledPipeline.Steps[0].Steps {
		if strings.Contains(step.Command, "useradd") || strings.Contains(step.Command, "--user runner") || !strings.Contains(step.Command, "run-job --plan-digest") {
			t.Fatalf("opt-out step still uses runner user: %q", step.Command)
		}
	}
}

func TestRunUploadDefaultsHostedToolchainImageWithoutRunnerQueue(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "default-image-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}

	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Key     string            `yaml:"key"`
				Image   string            `yaml:"image"`
				Agents  map[string]string `yaml:"agents"`
				Command string            `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 1 || len(pipeline.Steps[0].Steps) != 3 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps[0].Steps {
		if len(step.Agents) != 0 {
			t.Fatalf("step %q agents = %#v, want default targeting", step.Key, step.Agents)
		}
		if step.Image != defaultNobleRunnerImage || !strings.Contains(step.Command, "--hosted-tool-cache") {
			t.Fatalf("step %q image = %q, command = %q", step.Key, step.Image, step.Command)
		}
	}
}

func TestRunUploadUsesExplicitRuntimeImage(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "runtime-image-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, "--runner-queue", "ubuntu-latest=hosted", "--runner-image", "ubuntu-latest=" + image, workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Image   string `yaml:"image"`
				Command string `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 1 || len(pipeline.Steps[0].Steps) != 3 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps[0].Steps {
		if step.Image != image || !strings.Contains(step.Command, "--hosted-tool-cache") {
			t.Fatalf("runtime image step = %#v", step)
		}
	}
}

func TestRunnerProfileDoesNotAffectOtherLinuxLabels(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  selected:
    runs-on: ubuntu-latest
    steps:
      - run: echo selected
  default:
    runs-on: ubuntu-22.04
    steps:
      - run: echo default
`)
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	targets := map[string]compiler.RunnerTarget{
		"ubuntu-latest": {Queue: "hosted", Platform: compiler.PlatformLinuxAMD64, Image: image},
	}
	compiled, err := compileHosted(context.Background(), "profiles.yml", workflow, event, "dev", "sha256:"+strings.Repeat("1", 64), "importer", "", targets, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pipeline struct {
		Steps []struct {
			Key     string            `yaml:"key"`
			Image   string            `yaml:"image"`
			Agents  map[string]string `yaml:"agents"`
			Command string            `yaml:"command"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(compiled.Bundle.Pipeline, &pipeline); err != nil {
		t.Fatal(err)
	}
	steps := make(map[string]struct {
		Image   string
		Agents  map[string]string
		Command string
	}, len(pipeline.Steps))
	for _, step := range pipeline.Steps {
		steps[step.Key] = struct {
			Image   string
			Agents  map[string]string
			Command string
		}{Image: step.Image, Agents: step.Agents, Command: step.Command}
	}
	selected, defaulted := steps["gha-selected"], steps["gha-default"]
	if selected.Image != image || selected.Agents["queue"] != "hosted" || !strings.Contains(selected.Command, "--hosted-tool-cache") {
		t.Fatalf("selected profile step = %#v", selected)
	}
	if defaulted.Image != defaultJammyRunnerImage || len(defaulted.Agents) != 0 || !strings.Contains(defaulted.Command, "--hosted-tool-cache") {
		t.Fatalf("unmapped Linux step = %#v, want default targeting with the Jammy hosted-toolchains image", defaulted)
	}
}

func TestRunUploadRejectsLegacyTargetingEnvironment(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	for _, environment := range []string{legacyTargetQueueEnvironment, legacyRuntimeImageEnvironment} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "legacy-targeting-importer")
			t.Setenv(environment, "stale-policy")
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 2 {
				t.Fatalf("run() code = %d, want 2; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), environment+" is no longer supported") || len(runner.commands) != 0 {
				t.Fatalf("stderr = %q, commands = %#v", stderr.String(), runner.commands)
			}
		})
	}
}

func TestRunUploadUsesWorkflowGroupInsteadOfContainingGroup(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "grouped-importer")
	t.Setenv("BUILDKITE_GROUP_LABEL", ":github: Run workflow")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var pipeline struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Key       string `yaml:"key"`
			Condition string `yaml:"if"`
			DependsOn string `yaml:"depends_on"`
			Steps     []struct {
				Key string `yaml:"key"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != ":github: buildkite-gha shell smoke" || pipeline.Steps[0].Key == "" || pipeline.Steps[0].Condition == "" || pipeline.Steps[0].DependsOn != "grouped-importer" || len(pipeline.Steps[0].Steps) != 3 {
		t.Fatalf("grouped upload = %#v", pipeline.Steps)
	}
}

func TestRunUploadDerivesUnattestedBuildkiteEvent(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	sha := "0123456789abcdef0123456789abcdef01234567"
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "derived-event-importer")
	t.Setenv("BUILDKITE_REPO", "git@github.com:buildkite/buildkite-gha.git")
	t.Setenv("BUILDKITE_COMMIT", sha)
	t.Setenv("BUILDKITE_BRANCH", "main")
	t.Setenv("BUILDKITE_TAG", "")
	t.Setenv("BUILDKITE_PULL_REQUEST", "false")
	t.Setenv("BUILDKITE_SOURCE", "webhook")
	t.Setenv("BUILDKITE_BUILD_AUTHOR", "Unverified Author")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	planCount := 0
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatalf("decode derived-event plan %q: %v", path, err)
		}
		planCount++
		if job.Event.Provider != "github" || job.Event.Name != "push" || job.Event.Repository != "buildkite/buildkite-gha" || job.Event.Ref != "refs/heads/main" || job.Event.SHA != sha || job.Event.Actor != "Unverified Author" {
			t.Fatalf("derived plan event = %#v", job.Event)
		}
	}
	if planCount != 3 {
		t.Fatalf("derived-event plan count = %d, want 3", planCount)
	}
	if len(runner.commands) == 0 || !slices.Equal(runner.commands[0].args, []string{"meta-data", "get", "buildkite:webhook"}) {
		t.Fatalf("first command = %#v, want one webhook metadata read", runner.commands)
	}
	for _, command := range runner.commands[1:] {
		if slices.Equal(command.args, []string{"meta-data", "get", "buildkite:webhook"}) {
			t.Fatalf("webhook metadata read more than once: %#v", runner.commands)
		}
	}
}

func TestRunUploadDerivesOriginEvent(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	sha := "0123456789abcdef0123456789abcdef01234567"
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "origin-event-importer")
	t.Setenv("BUILDKITE_REPO", "https://origin.cursor.com/git/acme/widgets.git")
	t.Setenv("BUILDKITE_COMMIT", sha)
	t.Setenv("BUILDKITE_BRANCH", "main")
	t.Setenv("BUILDKITE_TAG", "")
	t.Setenv("BUILDKITE_PULL_REQUEST", "false")
	t.Setenv("BUILDKITE_BUILD_AUTHOR", "Origin Author")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	planCount := 0
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatalf("decode Origin plan %q: %v", path, err)
		}
		planCount++
		if job.Event.Provider != "cursor-origin" || job.Event.Repository != "acme/widgets" || job.Event.Ref != "refs/heads/main" || job.Event.SHA != sha {
			t.Fatalf("Origin plan event = %#v", job.Event)
		}
	}
	if planCount != 3 {
		t.Fatalf("Origin plan count = %d, want 3", planCount)
	}
}

func TestRunUploadUsesWebhookPayloadWithoutRetainingIt(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join(t.TempDir(), "webhook.yml")
	if err := os.WriteFile(workflowPath, []byte("on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-${{ github.event.marker }}\n    steps:\n      - run: echo selected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "webhook-importer")
	t.Setenv("BUILDKITE_REPO", "https://github.com/buildkite/buildkite-gha")
	t.Setenv("BUILDKITE_COMMIT", sha)
	t.Setenv("BUILDKITE_BRANCH", "executed")
	t.Setenv("BUILDKITE_TAG", "")
	t.Setenv("BUILDKITE_PULL_REQUEST", "false")
	t.Setenv("BUILDKITE_BUILD_AUTHOR", "Build Author")
	t.Setenv("BUILDKITE_GITHUB_EVENT", "pull_request")
	rawSecret := "raw-webhook-value-must-not-be-retained"
	runner := &cliCaptureRunner{webhook: []byte(fmt.Sprintf("{\"action\":\"opened\",\"marker\":\"latest\",\"private\":\"%s\",\"pull_request\":{\"base\":{\"ref\":\"main\"}},\"ref\":\"refs/heads/trigger\",\"after\":\"%s\",\"repository\":{\"full_name\":\"other/trigger\"},\"sender\":{\"login\":\"octocat\"}}", rawSecret, strings.Repeat("b", 40)))}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if len(runner.commands) == 0 || !slices.Equal(runner.commands[0].args, []string{"meta-data", "get", "buildkite:webhook"}) {
		t.Fatalf("commands = %#v, want metadata read first", runner.commands)
	}
	metadataReads, planCount := 0, 0
	for _, command := range runner.commands {
		if slices.Equal(command.args, []string{"meta-data", "get", "buildkite:webhook"}) {
			metadataReads++
		}
		if bytes.Contains(command.stdin, []byte(rawSecret)) {
			t.Fatalf("command retained raw webhook payload: %#v", command.args)
		}
	}
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		if bytes.Contains(contents, []byte(rawSecret)) {
			t.Fatalf("plan %q retained raw webhook payload", path)
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		planCount++
		if job.Event.Name != "pull_request" || job.Event.Repository != "buildkite/buildkite-gha" || job.Event.Ref != "refs/heads/executed" || job.Event.SHA != sha || job.Event.Actor != "octocat" {
			t.Fatalf("webhook plan = %#v", job)
		}
	}
	if metadataReads != 1 || planCount != 1 {
		t.Fatalf("metadata reads = %d, plans = %d", metadataReads, planCount)
	}
}

func TestRunUploadRejectsInvalidWebhookMetadata(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "webhook-importer")
	t.Setenv("BUILDKITE_REPO", "https://github.com/buildkite/buildkite-gha")
	t.Setenv("BUILDKITE_COMMIT", strings.Repeat("a", 40))
	t.Setenv("BUILDKITE_BRANCH", "main")
	for _, test := range []struct {
		name    string
		webhook []byte
		err     error
		want    string
	}{
		{name: "malformed", webhook: []byte("{\"incomplete\":"), want: "parse buildkite:webhook"},
		{name: "non-object", webhook: []byte(`[1]`), want: "must be a JSON object"},
		{name: "oversized", webhook: bytes.Repeat([]byte("x"), maxWebhookMetadataBytes+1), want: "exceeds"},
		{name: "operational failure", err: errors.New("agent authorization failed"), want: "agent authorization failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &cliCaptureRunner{webhook: test.webhook, webhookErr: test.err}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"upload", workflowPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("run() code = %d, stderr = %q, want %q", code, stderr.String(), test.want)
			}
			if !strings.Contains(stderr.String(), "Schema: "+compatibility.ProcessingSchema) || !strings.Contains(stderr.String(), "[E_ENVIRONMENT] event input could not be acquired") {
				t.Fatalf("stderr = %q, want versioned environment report", stderr.String())
			}
			if len(runner.commands) != 1 || !slices.Equal(runner.commands[0].args, []string{"meta-data", "get", "buildkite:webhook"}) {
				t.Fatalf("commands = %#v, want only one metadata read", runner.commands)
			}
		})
	}
}

func TestRunUploadCompilesConcurrentSmokePipeline(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "concurrent.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "concurrent-steps-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 2 jobs") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if len(runner.commands) != 4 {
		t.Fatalf("commands = %#v, want distribution, two plans, and pipeline", runner.commands)
	}

	var pipeline struct {
		Steps []struct {
			DependsOn string `yaml:"depends_on"`
			Steps     []struct {
				Key       string `yaml:"key"`
				Agents    any    `yaml:"agents"`
				DependsOn []struct {
					Step         string `yaml:"step"`
					AllowFailure bool   `yaml:"allow_failure"`
				} `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[3].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].DependsOn != "concurrent-steps-importer" || len(pipeline.Steps[0].Steps) != 2 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	steps := pipeline.Steps[0].Steps
	if steps[0].Key != "gha-concurrent" || steps[0].Agents != nil || len(steps[0].DependsOn) != 0 {
		t.Fatalf("concurrent step = %#v", steps[0])
	}
	observer := steps[1]
	if observer.Key != "gha-observe" || observer.Agents != nil || len(observer.DependsOn) != 1 || observer.DependsOn[0].Step != "gha-concurrent" || !observer.DependsOn[0].AllowFailure {
		t.Fatalf("observer step = %#v", observer)
	}
}

func TestRunUploadJavaScriptActionRequiresRuntimeMiseWithoutTransport(t *testing.T) {
	requireImporterHost(t)
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "action.yml")
	actionRoot := filepath.Join(root, ".github", "actions", "local")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte("runs:\n  using: node24\n  main: main.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "main.js"), []byte("console.log('local')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "action-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	if code := run([]string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if len(runner.commands) != 3 {
		t.Fatalf("commands = %d, want distribution, plan, and pipeline", len(runner.commands))
	}
	for path := range runner.uploaded {
		if strings.Contains(path, "/runtimes/") || strings.Contains(path, "/tools/mise/") {
			t.Fatalf("upload contains a runtime tool artifact %q", path)
		}
	}
	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Command string `yaml:"command"`
				Cache   struct {
					Paths []string `yaml:"paths"`
					Name  string   `yaml:"name"`
				} `yaml:"cache"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[2].stdin, &pipeline); err != nil {
		t.Fatalf("parse uploaded pipeline: %v", err)
	}
	if len(pipeline.Steps) != 1 || len(pipeline.Steps[0].Steps) != 1 {
		t.Fatalf("uploaded pipeline steps = %#v", pipeline.Steps)
	}
	command := pipeline.Steps[0].Steps[0].Command
	if strings.Contains(command, ".buildkite-gha/tools/mise/") || strings.Contains(command, `export PATH="$bootstrap_dir:$PATH"`) || strings.Contains(command, "BUILDKITE_GHA_NODE") || strings.Contains(command, ".buildkite-gha/runtimes") {
		t.Fatalf("generated pipeline still transports runtime tools:\n%s", command)
	}
	step := pipeline.Steps[0].Steps[0]
	if step.Cache.Name != "buildkite-gha-linux-amd64" || len(step.Cache.Paths) != 1 || step.Cache.Paths[0] != "/cache/bkcache/buildkite-gha/mise/linux-amd64" {
		t.Fatalf("generated action cache = %#v", step.Cache)
	}
	if step.Env["BUILDKITE_GHA_MISE_DATA_DIR"] != buildkitepipeline.MiseDataDir() {
		t.Fatalf("generated mise data directory = %q", step.Env["BUILDKITE_GHA_MISE_DATA_DIR"])
	}
}

func TestPrepareMiseDataDirFallsBackWhenCacheIsUnavailable(t *testing.T) {
	var stderr bytes.Buffer
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "mise")
	if got := prepareMiseDataDir(dir, &stderr); got != dir || stderr.Len() != 0 {
		t.Fatalf("prepareMiseDataDir() = %q, stderr = %q", got, stderr.String())
	}

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if got := prepareMiseDataDir(file, &stderr); got != "" || !strings.Contains(stderr.String(), "using the ephemeral agent cache") {
		t.Fatalf("prepareMiseDataDir(file) = %q, stderr = %q", got, stderr.String())
	}

	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linked-cache")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	want := filepath.Join(target, "mise")
	if got := prepareMiseDataDir(filepath.Join(link, "mise"), &stderr); got != want || stderr.Len() != 0 {
		t.Fatalf("prepareMiseDataDir(aliased ancestor) = %q, stderr = %q, want %q", got, stderr.String(), want)
	}

	stderr.Reset()
	if got := prepareMiseDataDir(link, &stderr); got != "" || !strings.Contains(stderr.String(), "not a real directory") {
		t.Fatalf("prepareMiseDataDir(symlink root) = %q, stderr = %q", got, stderr.String())
	}
}

func TestRunUploadAllowsCompilerVerifiedLocalDockerfileAction(t *testing.T) {
	requireImporterHost(t)
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "docker.yml")
	actionRoot := filepath.Join(root, ".github", "actions", "docker")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := "on: push\njobs:\n  docker:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/docker\n"
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte("runs:\n  using: docker\n  image: Dockerfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "docker-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	if code := run([]string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var job plan.Job
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		decoded, err := plan.Decode(contents)
		if err != nil {
			t.Fatalf("decode uploaded Docker plan: %v", err)
		}
		job = decoded
	}
	if !slices.Equal(job.RequiredCapabilities, []string{"docker", "network"}) || len(job.Actions) != 1 || job.Actions[0].Source != "workspace" {
		t.Fatalf("uploaded Docker action plan = %#v", job)
	}
}

func writeFakeNode(t *testing.T, root string, major int) string {
	t.Helper()
	path := filepath.Join(root, fmt.Sprintf("node-%d-%d", major, len(root)))
	contents := fmt.Sprintf("#!/bin/sh\nprintf 'v%d.0.0\\n'\n", major)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func setFakeMise(t *testing.T, version string) string {
	t.Helper()
	root := canonicalTempDir(t)
	mise := filepath.Join(root, "mise")
	script := "#!/bin/sh\nif [ -n \"${MISE_TEST_POISON:-}\" ]; then printf 'poisoned\\n'; else printf '" + version + " linux-x64 (test)\\n'; fi\n"
	if err := os.WriteFile(mise, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	return mise
}

func TestResolveRuntimeMisePinsRequiredExecutable(t *testing.T) {
	realMise := setFakeMise(t, buildkitepipeline.MinimumMiseVersion)
	linkRoot := t.TempDir()
	link := filepath.Join(linkRoot, "mise")
	if err := os.Symlink(realMise, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", linkRoot)
	t.Setenv("MISE_TEST_POISON", "must-not-reach-version-check")
	got, err := resolveRuntimeMise(context.Background(), "", t.TempDir(), t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realMise)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("resolveRuntimeMise() = %q, want pinned %q", got, want)
	}
}

func TestResolveRuntimeMiseAcceptsNewerVersion(t *testing.T) {
	realMise := setFakeMise(t, "2026.8.1")
	got, err := resolveRuntimeMiseWithInstaller(context.Background(), "", t.TempDir(), t.TempDir(), io.Discard, func(context.Context, string, string, io.Writer) (string, error) {
		return "", errors.New("unexpected managed mise install")
	})
	if err != nil || got != realMise {
		t.Fatalf("resolveRuntimeMiseWithInstaller() = %q, %v; want %q", got, err, realMise)
	}
}

func TestResolveRuntimeMiseAcceptsPrefixedVersionOutput(t *testing.T) {
	root := canonicalTempDir(t)
	mise := filepath.Join(root, "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\nprintf 'mise v2026.8.1 linux-x64 (test)\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRuntimeMise(context.Background(), mise, t.TempDir(), t.TempDir(), io.Discard)
	if err != nil || got != mise {
		t.Fatalf("resolveRuntimeMise() = %q, %v; want %q", got, err, mise)
	}
}

func TestMiseVersionAtLeast(t *testing.T) {
	for _, test := range []struct {
		actual string
		want   bool
	}{
		{actual: "2026.5.12", want: true},
		{actual: "v2026.5.12", want: true},
		{actual: "2026.5.13", want: true},
		{actual: "2026.8.1", want: true},
		{actual: "2027.1.1", want: true},
		{actual: "2026.5.11"},
		{actual: "2025.12.31"},
		{actual: "2026.5"},
		{actual: "latest"},
	} {
		if got := miseVersionAtLeast(test.actual, buildkitepipeline.MinimumMiseVersion); got != test.want {
			t.Errorf("miseVersionAtLeast(%q, %q) = %t, want %t", test.actual, buildkitepipeline.MinimumMiseVersion, got, test.want)
		}
	}
}

func TestResolveRuntimeMiseInstallsManagedCopyWhenNeeded(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
	}{
		{name: "missing"},
		{name: "old PATH version", version: "2026.5.11"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pathRoot := t.TempDir()
			if test.version != "" {
				mise := filepath.Join(pathRoot, "mise")
				if err := os.WriteFile(mise, []byte("#!/bin/sh\nprintf '"+test.version+" linux-x64 (test)\\n'\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", pathRoot)
			dataDir := t.TempDir()
			managed := filepath.Join(t.TempDir(), "mise")
			called := 0
			privateRuntime := t.TempDir()
			installer := func(_ context.Context, gotDataDir, gotPrivateRuntime string, _ io.Writer) (string, error) {
				called++
				if gotDataDir != dataDir {
					t.Fatalf("installer data dir = %q, want %q", gotDataDir, dataDir)
				}
				if gotPrivateRuntime != privateRuntime {
					t.Fatalf("installer private runtime = %q, want %q", gotPrivateRuntime, privateRuntime)
				}
				return managed, nil
			}
			got, err := resolveRuntimeMiseWithInstaller(context.Background(), "", dataDir, privateRuntime, io.Discard, installer)
			if err != nil || got != managed || called != 1 {
				t.Fatalf("resolveRuntimeMiseWithInstaller() = %q, %v; calls = %d", got, err, called)
			}
		})
	}
}

func TestResolveRuntimeMiseRejectsInvalidExplicitOverride(t *testing.T) {
	t.Run("configured path must be absolute", func(t *testing.T) {
		if _, err := resolveRuntimeMise(context.Background(), "mise", t.TempDir(), t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Fatalf("resolveRuntimeMise() error = %v", err)
		}
	})
	t.Run("old version", func(t *testing.T) {
		mise := setFakeMise(t, "2026.5.11")
		if _, err := resolveRuntimeMise(context.Background(), mise, t.TempDir(), t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), `reported version "2026.5.11", want "2026.5.12" or newer`) {
			t.Fatalf("resolveRuntimeMise() error = %v", err)
		}
	})
	t.Run("not executable", func(t *testing.T) {
		mise := filepath.Join(t.TempDir(), "mise")
		if err := os.WriteFile(mise, []byte("mise"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveRuntimeMise(context.Background(), mise, t.TempDir(), t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "not an executable regular file") {
			t.Fatalf("resolveRuntimeMise() error = %v", err)
		}
	})
}

func TestInstallRuntimeMiseDownloadsVerifiesAndReusesCache(t *testing.T) {
	binary := []byte("#!/bin/sh\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	archive := runtimeMiseTestArchive(t, binary)
	archiveHash := sha256.Sum256(archive)
	binaryHash := sha256.Sum256(binary)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	root := filepath.Join(logicalParent, "cache")
	want := filepath.Join(realParent, "cache", "linux-x64", "mise")
	for i := 0; i < 2; i++ {
		got, err := installRuntimeMiseFrom(context.Background(), root, server.Client(), server.URL, hex.EncodeToString(archiveHash[:]), hex.EncodeToString(binaryHash[:]))
		if err != nil || got != want {
			t.Fatalf("installRuntimeMiseFrom() = %q, %v", got, err)
		}
	}
	if requests != 1 {
		t.Fatalf("mise archive requests = %d, want one cache miss", requests)
	}
	if got, err := os.ReadFile(want); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("installed mise = %q, %v", got, err)
	}
}

func TestInstallRuntimeMiseRejectsInvalidArchive(t *testing.T) {
	binary := []byte("#!/bin/sh\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	archive := runtimeMiseTestArchive(t, binary)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	binaryHash := sha256.Sum256(binary)
	if _, err := installRuntimeMiseFrom(context.Background(), t.TempDir(), server.Client(), server.URL, strings.Repeat("0", 64), hex.EncodeToString(binaryHash[:])); err == nil || !strings.Contains(err.Error(), "archive checksum") {
		t.Fatalf("installRuntimeMiseFrom() error = %v", err)
	}
}

func TestValidateRuntimeMiseRejectsOversizedCacheEntry(t *testing.T) {
	cached := filepath.Join(t.TempDir(), "mise")
	file, err := os.OpenFile(cached, os.O_CREATE|os.O_WRONLY, 0o500)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(runtimeMiseBinaryLimit + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRuntimeMiseFile(context.Background(), cached, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("validateRuntimeMiseFile() error = %v, want size rejection", err)
	}
}

func TestManagedMiseCacheIsNotExecutedBeforePrivateCopy(t *testing.T) {
	root := canonicalTempDir(t)
	marker := filepath.Join(t.TempDir(), "executed")
	binary := []byte("#!/bin/sh\nprintf ran > '" + marker + "'\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	digest := sha256.Sum256(binary)
	cached := filepath.Join(root, "linux-x64", "mise")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, binary, 0o500); err != nil {
		t.Fatal(err)
	}
	got, err := installRuntimeMiseFrom(context.Background(), root, nil, "", "", hex.EncodeToString(digest[:]))
	if err != nil || got != cached {
		t.Fatalf("installRuntimeMiseFrom() = %q, %v; want cache hit %q", got, err, cached)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared cache executable ran during validation: %v", err)
	}
	if _, err := pinRuntimeMise(context.Background(), cached, t.TempDir(), hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("private executable did not run during version validation: %v", err)
	}
}

func TestManagedMiseColdCacheIsNotExecutedBeforePrivateCopy(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	binary := []byte("#!/bin/sh\nprintf ran > '" + marker + "'\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	archive := runtimeMiseTestArchive(t, binary)
	archiveDigest := sha256.Sum256(archive)
	binaryDigest := sha256.Sum256(binary)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	cached, err := installRuntimeMiseFrom(context.Background(), t.TempDir(), server.Client(), server.URL, hex.EncodeToString(archiveDigest[:]), hex.EncodeToString(binaryDigest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared staging executable ran during validation: %v", err)
	}
	if _, err := pinRuntimeMise(context.Background(), cached, t.TempDir(), hex.EncodeToString(binaryDigest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("private executable did not run during version validation: %v", err)
	}
}

func TestPinRuntimeMiseCopiesVerifiedBytesPrivately(t *testing.T) {
	binary := []byte("#!/bin/sh\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	digest := sha256.Sum256(binary)
	cached := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(cached, binary, 0o500); err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	privateRuntime := filepath.Join(logicalParent, "runtime")
	if err := os.Mkdir(privateRuntime, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := pinRuntimeMise(context.Background(), cached, privateRuntime, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(realParent, "runtime", "mise") {
		t.Fatalf("pinRuntimeMise() = %q, want private executable", got)
	}
	if err := os.Chmod(cached, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if copied, err := os.ReadFile(got); err != nil || !bytes.Equal(copied, binary) {
		t.Fatalf("private mise changed with cache: %q, %v", copied, err)
	}
	if _, err := pinRuntimeMise(context.Background(), cached, t.TempDir(), hex.EncodeToString(digest[:])); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("pinRuntimeMise() accepted tampered cache: %v", err)
	}
	linkedRuntime := filepath.Join(base, "linked-runtime")
	if err := os.Symlink(filepath.Join(realParent, "runtime"), linkedRuntime); err != nil {
		t.Fatal(err)
	}
	if _, err := pinRuntimeMise(context.Background(), got, linkedRuntime, hex.EncodeToString(digest[:])); err == nil || !strings.Contains(err.Error(), "contains a symlink") {
		t.Fatalf("pinRuntimeMise() accepted symlink root: %v", err)
	}
}

func TestInstallRuntimeMiseLiveRelease(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_REQUIRED") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_REQUIRED=1 to verify the pinned mise release")
	}
	privateRuntime := t.TempDir()
	got, err := installRuntimeMise(context.Background(), t.TempDir(), privateRuntime, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != privateRuntime {
		t.Fatalf("installed mise path = %q, want job-private root %q", got, privateRuntime)
	}
	selected, err := selectRuntimeMiseRelease(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateRuntimeMise(context.Background(), got, selected.binaryDigest); err != nil {
		t.Fatalf("validate installed mise release: %v", err)
	}
}

func TestSelectRuntimeMiseRelease(t *testing.T) {
	for _, test := range []struct {
		goos, goarch, asset, cacheKey, archiveDigest, binaryDigest string
	}{
		{"linux", "amd64", "linux-x64", "linux-amd64", runtimeMiseArchiveDigest, runtimeMiseBinaryDigest},
		{"darwin", "arm64", "macos-arm64", "darwin-arm64", runtimeMiseDarwinARM64ArchiveDigest, runtimeMiseDarwinARM64BinaryDigest},
	} {
		selected, err := selectRuntimeMiseRelease(test.goos, test.goarch)
		if err != nil {
			t.Fatal(err)
		}
		if selected.asset != test.asset || selected.cacheKey != test.cacheKey || selected.archiveDigest != test.archiveDigest || selected.binaryDigest != test.binaryDigest {
			t.Fatalf("selectRuntimeMiseRelease(%s/%s) = %#v", test.goos, test.goarch, selected)
		}
	}
	if _, err := selectRuntimeMiseRelease("linux", "arm64"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("selectRuntimeMiseRelease() unsupported platform error = %v", err)
	}
}

func runtimeMiseTestArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gzipWriter := gzip.NewWriter(&out)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "mise/bin/mise", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRunJobPublishesFailureWhenExplicitRuntimeMiseIsInvalid(t *testing.T) {
	job := cliRunJobPlan()
	job.Schema = plan.Schema
	requiresMise := true
	job.RequiresMise = &requiresMise
	job.Actions = []plan.ActionLock{{
		ID: "a-0000000000000001", Source: "workspace", Path: "actions/build",
		SourceDigest: "sha256:" + strings.Repeat("a", 64),
	}}
	job.Steps = []plan.Step{{
		ID: "local", Kind: "uses", Uses: "./actions/build",
		Action: &plan.ActionSelector{Lock: "a-0000000000000001"},
	}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	t.Setenv("BUILDKITE_GHA_MISE", filepath.Join(t.TempDir(), "missing-mise"))
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "prepare action runtime: resolve runtime mise executable") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "+++ :warning: Prepare GitHub Actions job failed\n~~~ :package: Publish GitHub Actions result\n") {
		t.Fatalf("stdout = %q, want visible runner setup failure before collapsed publication", stdout.String())
	}
	if manifest := publishedCLIManifest(t, runner, job, planDigest); manifest.Result != "failure" {
		t.Fatalf("published result = %q, want failure", manifest.Result)
	}
}

func TestRunJobMiseRequirementUsesCompilerDecisionAndFailsClosed(t *testing.T) {
	no := false
	yes := true
	actionJob := func(requiresMise *bool) plan.Job {
		return plan.Job{
			Actions:      []plan.ActionLock{{ID: "a-0000000000000001"}},
			Steps:        []plan.Step{{Kind: "uses", Action: &plan.ActionSelector{Lock: "a-0000000000000001"}}},
			RequiresMise: requiresMise,
		}
	}
	cacheJob := actionJob(&yes)
	cacheJob.Actions[0].Source = "github"
	cacheJob.Actions[0].Repository = "actions/cache"
	for _, test := range []struct {
		name string
		job  plan.Job
		want bool
	}{
		{name: "shell plan does not resolve mise", job: plan.Job{Steps: []plan.Step{{Kind: "run"}}}},
		{name: "native plan does not resolve mise", job: actionJob(&no)},
		{name: "JavaScript plan resolves mise", job: actionJob(&yes), want: true},
		{name: "cache client plan resolves mise", job: cacheJob, want: true},
		{name: "missing action decision resolves mise", job: actionJob(nil), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.job.NeedsMise(); got != test.want {
				t.Fatalf("NeedsMise() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRunJobSkipsActionJobBeforePreparingRuntimeMise(t *testing.T) {
	job := cliRunJobPlan()
	job.Schema = plan.Schema
	job.Condition = "${{ false }}"
	job.Actions = []plan.ActionLock{{
		ID: "a-0000000000000001", Source: "workspace", Path: "actions/build",
		SourceDigest: "sha256:" + strings.Repeat("a", 64),
	}}
	job.Steps = []plan.Step{{
		ID: "local", Kind: "uses", Uses: "./actions/build",
		Action: &plan.ActionSelector{Lock: "a-0000000000000001"},
	}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	t.Setenv("BUILDKITE_GHA_MISE", filepath.Join(t.TempDir(), "missing-mise"))
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "prepare action runtime") {
		t.Fatalf("skipped job prepared action runtime: %q", stderr.String())
	}
	if manifest := publishedCLIManifest(t, runner, job, planDigest); manifest.Result != "skipped" {
		t.Fatalf("published result = %q, want skipped", manifest.Result)
	}
}

func TestRunUploadFailsClosedBeforePipeline(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "shell-upload-importer")
	runner := &cliCaptureRunner{failAt: 2}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", workflowPath, "--event-path", eventPath, "--runtime-queue", "hosted"}, &stdout, &stderr, "dev", runner); code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if len(runner.commands) != 2 || !strings.Contains(stderr.String(), "upload artifact") {
		t.Fatalf("commands = %#v, stderr = %q", runner.commands, stderr.String())
	}
}

func TestUnprivilegedUploadRejectsCapabilities(t *testing.T) {
	tests := []struct {
		capability string
		want       string
	}{
		{capability: "privileged-container", want: `Job "protected" requires unsupported hosted runtime capability "privileged-container". Remove the requirement or use a runtime profile that supports it.`},
		{capability: "future-capability", want: `Job "protected" requires unsupported hosted runtime capability "future-capability". Remove the requirement or use a runtime profile that supports it.`},
	}
	for _, test := range tests {
		capability := test.capability
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow:             plan.Workflow{LogicalJobID: "protected"},
			RequiredCapabilities: []string{capability},
		}}}}
		err := validateUnprivilegedBundle(bundle)
		var finding *compiler.ProcessingFinding
		if err == nil || !errors.As(err, &finding) || finding.Message != test.want || finding.Detail != "" {
			t.Fatalf("validateUnprivilegedBundle(%q) error = %v, want capability rejection", capability, err)
		}
	}
}

func TestUnprivilegedUploadAdmitsSecretsCapability(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow:             plan.Workflow{LogicalJobID: "release"},
		RequiredCapabilities: []string{"secrets"},
		RequiredSecrets:      []string{"HOMEBREW_TAP_GITHUB_TOKEN"},
	}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatalf("validateUnprivilegedBundle() error = %v", err)
	}
}

func TestUnprivilegedUploadAdmitsOnlyCompilerVerifiedCheckoutCredentials(t *testing.T) {
	job := plan.Job{
		Workflow:             plan.Workflow{LogicalJobID: "checkout"},
		RequiredCapabilities: []string{"network", "provider-token-read"},
	}
	for _, test := range []struct {
		name          string
		authorization compiler.PlanAuthorization
		wantError     bool
	}{
		{name: "verified adapter", authorization: compiler.PlanAuthorization{ProviderTokenReadCapabilitySources: []string{"checkout-adapter"}}},
		{name: "missing provenance", wantError: true},
		{name: "broadened provenance", authorization: compiler.PlanAuthorization{ProviderTokenReadCapabilitySources: []string{"checkout-adapter", "javascript-action"}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: job, Authorization: test.authorization}}}
			err := validateUnprivilegedBundle(bundle)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "unsupported GitHub checkout credentials")) {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
		})
	}
}

func TestUnprivilegedUploadAdmitsOnlyCompilerVerifiedWorkflowToken(t *testing.T) {
	job := plan.Job{
		Workflow:             plan.Workflow{Path: ".github/workflows/comment.yml", LogicalJobID: "comment"},
		RequiredCapabilities: []string{"provider-token-write"},
		GitHubToken:          &plan.GitHubToken{Workflow: "comment.yml", Permissions: map[string]string{"pull_requests": "write"}},
	}
	for _, test := range []struct {
		name          string
		authorization compiler.PlanAuthorization
		wantError     bool
	}{
		{name: "verified policy", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions"}, WorkflowTokenPolicyFilename: "comment.yml"}},
		{name: "missing provenance", wantError: true},
		{name: "missing policy", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions"}}, wantError: true},
		{name: "mismatched policy", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions"}, WorkflowTokenPolicyFilename: "other.yml"}, wantError: true},
		{name: "broadened provenance", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions", "step-input"}, WorkflowTokenPolicyFilename: "comment.yml"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: job, Authorization: test.authorization}}}
			err := validateUnprivilegedBundle(bundle)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN")) {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
		})
	}
}

func TestGitHubTokenAdmissionDiagnosticSeparatesGuidanceFromDetail(t *testing.T) {
	artifact := compiler.PlanArtifact{
		Job: plan.Job{
			Workflow:    plan.Workflow{LogicalJobID: "build"},
			GitHubToken: &plan.GitHubToken{Workflow: "build.yml", Permissions: map[string]string{"contents": "read", "pull_requests": "write"}},
		},
		Authorization: compiler.PlanAuthorization{GitHubTokenActions: []string{"owner/action@v1"}},
	}
	wantMessage := `Job "build" needs GITHUB_TOKEN, but job-level permissions are unsupported for hosted GITHUB_TOKEN issuance. Move the permissions map to the workflow top level.`
	wantDetail := `Cause: action "owner/action@v1" defaults an input to github.token. Effective permissions: contents: read, pull-requests: write.`
	message, detail := githubTokenAdmissionDiagnostic(artifact, "GitHub workflow access tokens do not support job-level permissions")
	if message != wantMessage || detail != wantDetail {
		t.Fatalf("githubTokenAdmissionDiagnostic() = %q, %q", message, detail)
	}
}

func TestUnprivilegedUploadAllowsPublicAndDockerfileActionCapabilities(t *testing.T) {
	for _, capabilities := range [][]string{nil, {"network"}, {"docker"}, {"docker", "network"}} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow:             plan.Workflow{LogicalJobID: "action-job"},
			RequiredCapabilities: capabilities,
			Steps:                []plan.Step{{ID: "action", Kind: "uses", Uses: "owner/example@commit"}},
		}, Authorization: compiler.PlanAuthorization{DockerCapabilitySources: []string{"dockerfile-actions"}}}}}
		if err := validateUnprivilegedBundle(bundle); err != nil {
			t.Fatalf("validateUnprivilegedBundle(%v) error = %v", capabilities, err)
		}
	}
}

func TestUnprivilegedUploadRejectsDockerWithoutCompilerProvenance(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow:             plan.Workflow{LogicalJobID: "unproven-docker"},
		RequiredCapabilities: []string{"docker"},
	}}}}
	err := validateUnprivilegedBundle(bundle)
	var finding *compiler.ProcessingFinding
	want := `Job "unproven-docker" requires Docker without matching compiler provenance. Hosted runs support only verified Dockerfile actions and bounded job or service containers.`
	if err == nil || !errors.As(err, &finding) || finding.Message != want || finding.Detail != "" {
		t.Fatalf("validateUnprivilegedBundle() error = %v, want Docker provenance rejection", err)
	}
}

func TestUnprivilegedUploadAdmitsCompilerVerifiedContainerProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		job     plan.Job
		sources []string
	}{
		{name: "job", job: plan.Job{Container: &plan.Container{Image: "node:24"}}, sources: []string{"job-containers"}},
		{name: "service", job: plan.Job{Services: map[string]plan.Container{"redis": {Image: "redis:7"}}}, sources: []string{"service-containers"}},
		{name: "dynamic services", job: plan.Job{ServicesExpression: "${{ fromJSON(needs.build.outputs.services) }}"}, sources: []string{"service-containers"}},
		{name: "all", job: plan.Job{Container: &plan.Container{Image: "node:24"}, Services: map[string]plan.Container{"redis": {Image: "redis:7"}}}, sources: []string{"dockerfile-actions", "job-containers", "service-containers"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "container-job"}, RequiredCapabilities: []string{"docker", "network"}, Container: test.job.Container, Services: test.job.Services, ServicesExpression: test.job.ServicesExpression,
			}, Authorization: compiler.PlanAuthorization{DockerCapabilitySources: test.sources}}}}
			if err := validateUnprivilegedBundle(bundle); err != nil {
				t.Fatalf("validateUnprivilegedBundle(%v) error = %v", test.sources, err)
			}
		})
	}
}

func TestUnprivilegedUploadRejectsMismatchedContainerProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		job     plan.Job
		sources []string
	}{
		{name: "claim without container", sources: []string{"job-containers"}},
		{name: "container without claim", job: plan.Job{Container: &plan.Container{Image: "node:24"}}},
		{name: "unknown source", sources: []string{"docker-plugin"}},
		{name: "unsorted source", job: plan.Job{Container: &plan.Container{Image: "node:24"}}, sources: []string{"job-containers", "dockerfile-actions"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "container-job"}, RequiredCapabilities: []string{"docker", "network"}, Container: test.job.Container,
			}, Authorization: compiler.PlanAuthorization{DockerCapabilitySources: test.sources}}}}
			if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), "unsupported Docker access") {
				t.Fatalf("validateUnprivilegedBundle(%v) error = %v", test.sources, err)
			}
		})
	}
}

func TestUnprivilegedUploadRejectsKnownGitHubServiceActions(t *testing.T) {
	tests := []struct {
		action  plan.ActionLock
		service string
	}{
		{plan.ActionLock{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}, "artifact"},
	}
	for _, test := range tests {
		name := test.action.Repository
		if test.action.Path != "" {
			name += "/" + test.action.Path
		}
		t.Run(name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "service-action"},
				Actions:  []plan.ActionLock{test.action},
			}}}}
			err := validateUnprivilegedBundle(bundle)
			var finding *compiler.ProcessingFinding
			want := `Action "actions/upload-artifact/merge" requires the GitHub Actions artifact service, which hosted runs do not provide. Replace the action or use a runtime profile that provides this service.`
			if err == nil || !errors.As(err, &finding) || finding.Message != want || finding.Detail != "" {
				t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", test.action, err)
			}
		})
	}
}

func TestUnprivilegedUploadAllowsOnlyAuditedCacheCommits(t *testing.T) {
	for _, commit := range actionintegration.CacheCommits() {
		for _, path := range []string{"", "restore", "save"} {
			action := plan.ActionLock{Source: "github", Repository: "actions/cache", Path: path, Commit: commit}
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "cache"},
				Actions:  []plan.ActionLock{action},
			}}}}
			if err := validateUnprivilegedBundle(bundle); err != nil {
				t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
			}
		}
	}

	action := plan.ActionLock{Source: "github", Repository: "actions/cache", RequestedRef: "v6.0.2", Commit: strings.Repeat("0", 40)}
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow: plan.Workflow{LogicalJobID: "cache-old"},
		Actions:  []plan.ActionLock{action},
	}}}}
	err := validateUnprivilegedBundle(bundle)
	if err == nil || !strings.Contains(err.Error(), "v6.1.0") {
		t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
	}
	var finding *compiler.ProcessingFinding
	if !errors.As(err, &finding) || finding.Message != "actions/cache v6.0.2 is unsupported. Use a supported version from https://github.com/buildkite/buildkite-gha/blob/main/docs/compatibility.md#cache-action." || !strings.Contains(finding.Detail, "Buildkite cache-v2 service") || !strings.Contains(finding.Detail, action.Commit) || strings.Contains(finding.Message, action.Commit) {
		t.Fatalf("hosted cache diagnostic = %#v", finding)
	}
}

func TestCacheServiceRequiredUsesOnlyAuditedCacheLocks(t *testing.T) {
	required, err := cacheServiceRequired([]plan.ActionLock{{Source: "github", Repository: "owner/action", Commit: strings.Repeat("a", 40)}})
	if err != nil || required {
		t.Fatalf("ordinary action cache requirement = %v, %v", required, err)
	}

	locks := []plan.ActionLock{{Source: "github", Repository: "owner/action", Commit: strings.Repeat("a", 40)}}
	for _, commit := range actionintegration.CacheCommits() {
		locks = append(locks[:1], plan.ActionLock{Source: "github", Repository: "actions/cache", Path: "restore", Commit: commit})
		required, err = cacheServiceRequired(locks)
		if err != nil || !required {
			t.Fatalf("cache requirement for %s = %v, %v", commit, required, err)
		}
	}

	locks[1].Commit = strings.Repeat("b", 40)
	if _, err := cacheServiceRequired(locks); err == nil || !strings.Contains(err.Error(), actionintegration.CacheCommit) {
		t.Fatalf("unsupported cache lock error = %v", err)
	}
}

func TestUnprivilegedUploadAllowsNativeDownloadArtifactAdapter(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{Workflow: plan.Workflow{LogicalJobID: "consumer"}, Actions: []plan.ActionLock{{Source: "github", Repository: "actions/download-artifact", Commit: actionintegration.DownloadArtifactCommit}}}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatal(err)
	}
}

func TestUnprivilegedUploadAllowsNativeUploadArtifactAdapter(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow: plan.Workflow{LogicalJobID: "artifact-producer"},
		Actions: []plan.ActionLock{{
			Source: "github", Repository: "actions/upload-artifact",
			Commit: actionintegration.UploadArtifactCommit,
		}},
	}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatalf("validateUnprivilegedBundle() error = %v", err)
	}
}

func TestUnprivilegedUploadDoesNotBroadenKnownServiceActionIdentity(t *testing.T) {
	for _, action := range []plan.ActionLock{
		{Source: "workspace", Repository: "actions/cache"},
		{Source: "github", Repository: "actions/cache", Path: "nested"},
		{Source: "github", Repository: "actions/upload-artifact", Path: "nested"},
		{Source: "github", Repository: "actions/download-artifact", Path: "nested"},
		{Source: "github", Repository: "owner/action"},
	} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow: plan.Workflow{LogicalJobID: "ordinary-action"},
			Actions:  []plan.ActionLock{action},
		}}}}
		if err := validateUnprivilegedBundle(bundle); err != nil {
			t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
		}
	}
}

func TestBundleUsesActionsDetectsStepsAndLocks(t *testing.T) {
	for _, job := range []plan.Job{
		{Steps: []plan.Step{{Kind: "uses"}}},
		{Actions: []plan.ActionLock{{ID: "a-deadbeefdeadbeef"}}},
	} {
		if !bundleUsesActions(compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: job}}}) {
			t.Fatalf("bundleUsesActions() = false for %#v", job)
		}
	}
	if bundleUsesActions(compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{Steps: []plan.Step{{Kind: "run"}}}}}}) {
		t.Fatal("bundleUsesActions() = true for shell-only plan")
	}
}

func TestUnprivilegedUploadAllowsCapabilityFreeConcurrentShellSteps(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow: plan.Workflow{LogicalJobID: "concurrent-job"},
		Steps: []plan.Step{
			{ID: "background", Kind: "run", Command: "true", Background: true},
			{ID: "wait", Kind: "wait", Targets: []string{"background"}},
			{ID: "wait-all", Kind: "wait-all"},
			{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
		},
	}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatalf("validateUnprivilegedBundle() error = %v", err)
	}
}

type cliCommand struct {
	dir   string
	name  string
	args  []string
	stdin []byte
}

type cliCaptureRunner struct {
	commands       []cliCommand
	failAt         int
	failMetadata   bool
	failAnnotation bool
	gitOutput      []byte
	gitErr         error
	webhook        []byte
	webhookErr     error
	jobByStep      map[string]string
	dataByPath     map[string][]byte
	uploaded       map[string][]byte
	contextErrors  []error
}

type cliActionSourceTokenProvider struct {
	mu         sync.Mutex
	token      string
	err        error
	repository string
	calls      int
}

func (p *cliActionSourceTokenProvider) ActionSourceToken(_ context.Context, repository string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.repository = repository
	return p.token, p.err
}

type cliRedactor struct {
	mu     sync.Mutex
	values []string
	err    error
}

func (r *cliRedactor) AddRedaction(_ context.Context, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, value)
	return r.err
}

func (r *cliCaptureRunner) Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, cliCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: bytes.Clone(stdin)})
	r.contextErrors = append(r.contextErrors, ctx.Err())
	if r.failAt != 0 && len(r.commands) == r.failAt {
		return nil, errors.New("injected failure")
	}
	if name == "git" && slices.Equal(args, []string{"rev-parse", "HEAD"}) {
		return bytes.Clone(r.gitOutput), r.gitErr
	}
	if slices.Equal(args, []string{"meta-data", "get", "buildkite:webhook"}) {
		if r.webhookErr != nil {
			return nil, r.webhookErr
		}
		if r.webhook == nil {
			return nil, transport.ErrMetadataUnavailable
		}
		return bytes.Clone(r.webhook), nil
	}
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "search" {
		return []byte(r.jobByStep[args[4]] + "\n"), nil
	}
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "download" {
		contents, ok := r.dataByPath[args[2]]
		if !ok {
			return nil, errors.New("missing fixture artifact")
		}
		path := filepath.Join(args[3], filepath.FromSlash(args[2]))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, contents, 0o600)
	}
	if len(args) >= 3 && args[0] == "artifact" && args[1] == "upload" {
		contents, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(args[2])))
		if err != nil {
			return nil, err
		}
		if r.uploaded == nil {
			r.uploaded = map[string][]byte{}
		}
		r.uploaded[args[2]] = contents
	}
	if r.failMetadata && len(args) >= 2 && args[0] == "meta-data" && args[1] == "set" {
		return nil, errors.New("metadata unavailable")
	}
	if r.failAnnotation && len(args) > 0 && args[0] == "annotate" {
		return nil, errors.New("annotation unavailable")
	}
	return nil, nil
}

var _ transport.Runner = (*cliCaptureRunner)(nil)

func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing command", want: "Usage:"},
		{name: "unknown command", args: []string{"nope"}, want: `unknown command "nope"`},
		{name: "unknown help command", args: []string{"help", "nope"}, want: `unknown command "nope"`},
		{name: "version arguments", args: []string{"--version", "extra"}, want: "does not accept arguments"},
		{name: "upload outside Buildkite", args: []string{"upload", "--runtime-queue", "hosted", "workflow.yml"}, want: "BUILDKITE=true"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.args) != 0 && test.args[0] == "upload" {
				requireImporterHost(t)
			}
			t.Setenv("BUILDKITE", "")
			t.Setenv("BUILDKITE_STEP_KEY", "")
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr, "dev"); code != 2 {
				t.Fatalf("Run() code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("Run() stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Errorf("Run() stderr = %q, want it to contain %q", stderr.String(), test.want)
			}
		})
	}
}

func TestRunJobExecutesBoundPlanAndWritesResult(t *testing.T) {
	workspace := t.TempDir()
	workflowSource := []byte("name: cli fixture\n")
	workflowPath := filepath.Join(workspace, "workflow.yml")
	if err := os.WriteFile(workflowPath, workflowSource, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(workflowSource)
	requiresMise := false
	job := plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "dev", DistributionDigest: "sha256:" + strings.Repeat("9", 64)},
		Runtime:      &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()},
		Workflow:     plan.Workflow{Path: "workflow.yml", Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "cli"},
		Event:        plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:       plan.Target{StepKey: "gha-cli", Queue: "ubuntu-latest"},
		Outputs:      map[string]string{"result": "${{ steps.produce.outputs.result }}"},
		Steps:        []plan.Step{{ID: "produce", Kind: "run", Command: `echo "result=cli-ok" >> "$GITHUB_OUTPUT"`}},
		RequiresMise: &requiresMise,
	}
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(workspace, "plan.json")
	resultPath := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest := sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", job.Target.StepKey)
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", job.Target.Queue)
	oldDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDirectory) })

	var stdout, stderr bytes.Buffer
	runner := &cliCaptureRunner{}
	if code := run([]string{"run-job", "--plan", planPath, "--result", resultPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"result": "cli-ok"`) {
		t.Fatalf("result = %s", result)
	}
	artifactDigest := transport.Digest(encoded)
	artifactPath, err := buildkitepipeline.PlanPath(artifactDigest)
	if err != nil {
		t.Fatal(err)
	}
	artifactRunner := &cliCaptureRunner{dataByPath: map[string][]byte{artifactPath: encoded}}
	artifactResultPath := filepath.Join(workspace, "artifact-result.json")
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+strings.Repeat("0", 64))
	stdout.Reset()
	stderr.Reset()
	failedArtifactRunner := &cliCaptureRunner{failAt: 1}
	if code := run([]string{"run-job", "--plan-digest", artifactDigest, "--plan-producer", "gha-importer", "--result", artifactResultPath}, &stdout, &stderr, "dev", failedArtifactRunner); code != 1 || !strings.Contains(stderr.String(), "download plan") {
		t.Fatalf("failed artifact Run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "~~~ :package: Prepare GitHub Actions job\n^^^ +++\n") {
		t.Fatalf("failed artifact stdout = %q, want expanded preparation group", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan-digest", artifactDigest, "--plan-producer", "gha-importer", "--result", artifactResultPath}, &stdout, &stderr, "dev", artifactRunner); code != 0 {
		t.Fatalf("artifact Run() code = %d, stderr = %q", code, stderr.String())
	}
	if len(artifactRunner.commands) == 0 || !slices.Equal(artifactRunner.commands[0].args[:3], []string{"artifact", "download", artifactPath}) || artifactRunner.commands[0].args[4] != "--step" || artifactRunner.commands[0].args[5] != "gha-importer" {
		t.Fatalf("plan artifact download = %#v", artifactRunner.commands)
	}
	if destination := artifactRunner.commands[0].args[3]; destination == workspace {
		t.Fatalf("plan downloaded into workspace %q", destination)
	} else if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("private plan directory still exists: %v", err)
	}
	tamperedRunner := &cliCaptureRunner{dataByPath: map[string][]byte{artifactPath: []byte("tampered")}}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan-digest", artifactDigest, "--plan-producer", "gha-importer"}, &stdout, &stderr, "dev", tamperedRunner); code != 1 || !strings.Contains(stderr.String(), "does not match expected digest") {
		t.Fatalf("tampered artifact Run() code = %d, stderr = %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+strings.Repeat("0", 64))
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not match expected digest") {
		t.Fatalf("Run() code = %d, stderr = %q, want digest mismatch", code, stderr.String())
	}
	job.Runtime.DistributionDigest = "sha256:" + strings.Repeat("0", 64)
	encoded, err = plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest = sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "runtime distribution digest") {
		t.Fatalf("Run() code = %d, stderr = %q, want runtime digest mismatch", code, stderr.String())
	}
	job.Runtime.DistributionDigest = cliTestRuntimeDigest()
	job.Compiler.Version = "0.0.0-other"
	encoded, err = plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest = sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not match runtime version") {
		t.Fatalf("Run() code = %d, stderr = %q, want version mismatch", code, stderr.String())
	}
}

func TestRunJobHydratesNeedsAndPublishesAuthoritativeResult(t *testing.T) {
	producerStep := "gha-producer"
	producerPlanDigest := transport.Digest([]byte("producer-plan"))
	job := cliRunJobPlan()
	job.Dependencies = []string{producerStep}
	job.NeedSources = map[string][]plan.NeedSource{
		"producer": {{StepKey: producerStep, PlanDigest: producerPlanDigest}},
	}
	job.Env = map[string]string{"RESULT": "${{ needs.producer.outputs.result }}"}
	job.Steps = []plan.Step{{ID: "consume", Kind: "run", Command: `test "$RESULT" = "hydrated"`}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)

	producerManifest, err := transport.MarshalResultManifest(transport.ResultManifest{
		PlanDigest: producerPlanDigest,
		Producer:   transport.Producer{BuildID: cliTestBuildID, JobID: cliTestProducerJobID, StepKey: producerStep},
		Result:     "success",
		Outputs:    []transport.Output{{Name: "result", Value: "hydrated"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	producerPath := transport.ResultPath(producerStep, producerPlanDigest)
	runner := &cliCaptureRunner{
		failMetadata: true,
		jobByStep:    map[string]string{producerStep: cliTestProducerJobID},
		dataByPath:   map[string][]byte{producerPath: producerManifest},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: result metadata mirror") {
		t.Fatalf("stderr = %q, want non-fatal metadata warning", stderr.String())
	}
	if !strings.Contains(stdout.String(), "~~~ :package: Publish GitHub Actions result\n") || !strings.Contains(stdout.String(), "^^^ +++\n") {
		t.Fatalf("stdout = %q, want expanded result publication group", stdout.String())
	}
	if len(runner.commands) < 3 || strings.Join(runner.commands[0].args, " ") != strings.Join([]string{"artifact", "search", producerPath, "--step", producerStep, "--format", "%j"}, " ") {
		t.Fatalf("commands = %#v, want exact producer search first", runner.commands)
	}
	if got := runner.commands[1].args[len(runner.commands[1].args)-1]; got != cliTestProducerJobID {
		t.Fatalf("download producer = %q, want exact job UUID", got)
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if manifest.Result != "success" {
		t.Fatalf("published result = %q, want success", manifest.Result)
	}
	for _, command := range runner.commands {
		if command.dir != "" {
			if _, err := os.Stat(command.dir); !os.IsNotExist(err) {
				t.Fatalf("temporary artifact root %q still exists: %v", command.dir, err)
			}
		}
	}
}

func TestRunJobPublishesSummaryAsAdvisoryJobAnnotation(t *testing.T) {
	tests := []struct {
		name           string
		command        string
		failAnnotation bool
		wantAnnotation bool
		wantWarning    bool
		wantResult     string
		wantCode       int
	}{
		{name: "published", command: `printf '### Job summary\n\nPassed.\n' >> "$GITHUB_STEP_SUMMARY"`, wantAnnotation: true, wantResult: "success"},
		{name: "published after job failure", command: `printf '### Job summary\n\nPassed.\n' >> "$GITHUB_STEP_SUMMARY"; exit 7`, wantAnnotation: true, wantResult: "failure", wantCode: 1},
		{name: "failure is advisory", command: `printf '### Job summary\n\nPassed.\n' >> "$GITHUB_STEP_SUMMARY"`, failAnnotation: true, wantAnnotation: true, wantWarning: true, wantResult: "success"},
		{name: "empty summary is a no-op", command: "true", wantResult: "success"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Steps[0].Command = test.command
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			runner := &cliCaptureRunner{failAnnotation: test.failAnnotation}
			var stdout, stderr bytes.Buffer

			if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != test.wantCode {
				t.Fatalf("run() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			if !strings.Contains(stdout.String(), "~~~ :package: Publish GitHub Actions result\n") {
				t.Fatalf("stdout = %q, want result publication group", stdout.String())
			}
			if test.wantWarning && !strings.Contains(stdout.String(), "^^^ +++\n") {
				t.Fatalf("stdout = %q, want publication warning to expand its group", stdout.String())
			}
			if !test.wantWarning && test.wantCode == 0 && strings.Contains(stdout.String(), "^^^ +++\n") {
				t.Fatalf("stdout = %q, want successful publication group collapsed", stdout.String())
			}
			if test.wantCode != 0 && strings.Contains(stdout.String(), "Prepare GitHub Actions job failed") {
				t.Fatalf("stdout = %q, want action failure owned by its existing section", stdout.String())
			}
			if result := publishedCLIManifest(t, runner, job, planDigest); result.Result != test.wantResult {
				t.Fatalf("published result = %q, want %q", result.Result, test.wantResult)
			}
			var annotations []cliCommand
			for _, command := range runner.commands {
				if len(command.args) > 0 && command.args[0] == "annotate" {
					annotations = append(annotations, command)
				}
			}
			if !test.wantAnnotation {
				if len(annotations) != 0 {
					t.Fatalf("annotations = %#v, want none", annotations)
				}
				return
			}
			wantArgs := []string{"annotate", "--scope", "job", "--job", cliTestJobID, "--context", "buildkite-gha-job-summary", "--style", "info"}
			wantBody := "<h2 class=\"h4 mb2\">GitHub Actions job summary</h2>\n<div class=\"border-top border-gray pt2\"></div>\n\n### Job summary\n\nPassed.\n"
			if len(annotations) != 1 || !slices.Equal(annotations[0].args, wantArgs) || string(annotations[0].stdin) != wantBody {
				t.Fatalf("annotations = %#v", annotations)
			}
			if last := runner.commands[len(runner.commands)-1]; len(last.args) == 0 || last.args[0] != "annotate" {
				t.Fatalf("last command = %#v, want annotation after authoritative publication", last)
			}
			if gotWarning := strings.Contains(stderr.String(), "warning: job summary annotation"); gotWarning != test.wantWarning {
				t.Fatalf("stderr = %q, warning present = %v, want %v", stderr.String(), gotWarning, test.wantWarning)
			}
			if runner.contextErrors[len(runner.contextErrors)-1] != nil {
				t.Fatalf("annotation inherited cancelled context: %v", runner.contextErrors[len(runner.contextErrors)-1])
			}
		})
	}
}

func TestRunJobDisabledWorkflowTokenLinksPipelineSettings(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	job := cliRunJobPlan()
	job.Workflow.Path = ".github/workflows/ci.yml"
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "ci.yml", Permissions: map[string]string{"contents": "read"}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	t.Setenv("BUILDKITE_GHA_AGENT", truePath)
	t.Setenv("BUILDKITE_ORGANIZATION_SLUG", "acme-inc")
	t.Setenv("BUILDKITE_PIPELINE_SLUG", "my-pipeline")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != 1 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	want := `enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings: https://buildkite.com/acme-inc/my-pipeline/settings/repository`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRunJobPublishesWorkflowCommandsAsAdvisoryJobAnnotations(t *testing.T) {
	diagnostics := "printf '%s\\n' '::warning title=Lint::warning body'; printf '%s\\n' '::error file=main.go,line=7::error body' >&2"
	tests := []struct {
		name           string
		command        string
		failAnnotation bool
		wantAnnotation bool
		wantWarning    bool
		wantResult     string
		wantCode       int
	}{
		{name: "published without changing success", command: diagnostics, wantAnnotation: true, wantResult: "success"},
		{name: "published after job failure", command: diagnostics + "; exit 7", wantAnnotation: true, wantResult: "failure", wantCode: 1},
		{name: "publication failure is advisory", command: diagnostics, failAnnotation: true, wantAnnotation: true, wantWarning: true, wantResult: "success"},
		{name: "empty diagnostics are a no-op", command: "true", wantResult: "success"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Steps[0].Command = test.command
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			runner := &cliCaptureRunner{failAnnotation: test.failAnnotation}
			var stdout, stderr bytes.Buffer

			if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != test.wantCode {
				t.Fatalf("run() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			if result := publishedCLIManifest(t, runner, job, planDigest); result.Result != test.wantResult {
				t.Fatalf("published result = %q, want %q", result.Result, test.wantResult)
			}
			var annotations []cliCommand
			for _, command := range runner.commands {
				if len(command.args) > 0 && command.args[0] == "annotate" {
					annotations = append(annotations, command)
				}
			}
			if !test.wantAnnotation {
				if len(annotations) != 0 {
					t.Fatalf("annotations = %#v, want none", annotations)
				}
				return
			}
			want := []struct {
				context, style, body string
			}{
				{context: "buildkite-gha-workflow-warnings", style: "warning", body: "warning body"},
				{context: "buildkite-gha-workflow-errors", style: "error", body: "error body"},
			}
			if len(annotations) != len(want) {
				t.Fatalf("annotations = %#v, want warning and error", annotations)
			}
			for i, expected := range want {
				wantArgs := []string{"annotate", "--scope", "job", "--job", cliTestJobID, "--context", expected.context, "--style", expected.style}
				if !slices.Equal(annotations[i].args, wantArgs) || !strings.Contains(string(annotations[i].stdin), expected.body) {
					t.Errorf("annotation %d = %#v, want args %#v and body containing %q", i, annotations[i], wantArgs, expected.body)
				}
			}
			for _, command := range runner.commands[len(runner.commands)-len(want):] {
				if len(command.args) == 0 || command.args[0] != "annotate" {
					t.Fatalf("trailing command = %#v, want annotations after authoritative publication", command)
				}
			}
			gotWarning := strings.Contains(stderr.String(), "warning: workflow warning annotation") && strings.Contains(stderr.String(), "warning: workflow error annotation")
			if gotWarning != test.wantWarning {
				t.Fatalf("stderr = %q, advisory warnings present = %v, want %v", stderr.String(), gotWarning, test.wantWarning)
			}
			for _, contextErr := range runner.contextErrors[len(runner.contextErrors)-len(want):] {
				if contextErr != nil {
					t.Fatalf("annotation inherited cancelled context: %v", contextErr)
				}
			}
		})
	}
}

func TestRunJobPublishesEveryTerminalResultAfterCancellation(t *testing.T) {
	tests := []struct {
		name       string
		condition  string
		command    string
		cancel     bool
		tolerate   bool
		wantResult string
		wantCode   int
	}{
		{name: "failure", command: "exit 7", wantResult: "failure", wantCode: 1},
		{name: "tolerated failure", command: "exit 7", tolerate: true, wantResult: "success", wantCode: buildkitepipeline.ContinueOnErrorExitStatus},
		{name: "skipped", condition: "${{ false }}", command: "true", wantResult: "skipped", wantCode: 0},
		{name: "cancelled", command: "sleep 1", cancel: true, wantResult: "cancelled", wantCode: 1},
		{name: "tolerated cancellation", command: "sleep 1", cancel: true, tolerate: true, wantResult: "cancelled", wantCode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Condition = test.condition
			job.Steps[0].Command = test.command
			if test.tolerate {
				job.Schema = plan.Schema
				job.ContinueOnError = true
				job.Runtime = &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()}
				requiresMise := false
				job.RequiresMise = &requiresMise
			}
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			runner := &cliCaptureRunner{}
			ctx := context.Background()
			if test.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			var stdout, stderr bytes.Buffer
			if code := runJobContext(ctx, []string{"--plan", planPath}, &stdout, &stderr, "dev", "dev", transport.Agent{Runner: runner}); code != test.wantCode {
				t.Fatalf("runJobContext() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			manifest := publishedCLIManifest(t, runner, job, planDigest)
			if manifest.Result != test.wantResult {
				t.Fatalf("published result = %q, want %q", manifest.Result, test.wantResult)
			}
			for i, contextErr := range runner.contextErrors {
				if contextErr != nil {
					t.Fatalf("publication command %d inherited cancelled context: %v", i, contextErr)
				}
			}
		})
	}
}

func TestPublishTerminalResultAnnotatesCancelledJobWithFreshContext(t *testing.T) {
	job := cliRunJobPlan()
	planDigest := transport.Digest([]byte("cancelled-plan"))
	producer := transport.Producer{BuildID: cliTestBuildID, JobID: cliTestJobID, StepKey: job.Target.StepKey}
	runner := &cliCaptureRunner{}
	artifactDigest := strings.Repeat("a", 64)
	artifact := transport.ResultArtifact{
		Name: "payload", ID: "123", Path: "buildkite-gha/v1/artifacts/" + artifactDigest + ".zip",
		Digest: "sha256:" + artifactDigest, Size: 42, FileCount: 1,
	}

	publication, err := publishTerminalResult(
		transport.Agent{Runner: runner},
		t.TempDir(),
		job,
		planDigest,
		producer,
		gharuntime.JobResult{
			Conclusion:         "cancelled",
			Summary:            "summary before cancellation\n",
			WarningAnnotations: "warning before cancellation\n",
			ErrorAnnotations:   "error before cancellation\n",
			Artifacts:          []transport.ResultArtifact{artifact},
		},
	)
	if err != nil || publication.SummaryAnnotationError != nil || publication.WarningAnnotationError != nil || publication.ErrorAnnotationError != nil {
		t.Fatalf("publishTerminalResult() publication = %#v, error = %v", publication, err)
	}
	wantBodies := []string{
		"<h2 class=\"h4 mb2\">GitHub Actions job summary</h2>\n<div class=\"border-top border-gray pt2\"></div>\n\nsummary before cancellation\n",
		"warning before cancellation\n",
		"error before cancellation\n",
	}
	commands := runner.commands[len(runner.commands)-len(wantBodies):]
	for i, command := range commands {
		if len(command.args) == 0 || command.args[0] != "annotate" || string(command.stdin) != wantBodies[i] {
			t.Errorf("annotation %d = %#v, want cancelled job annotation body %q", i, command, wantBodies[i])
		}
	}
	for _, contextErr := range runner.contextErrors[len(runner.contextErrors)-len(wantBodies):] {
		if contextErr != nil {
			t.Fatalf("annotation inherited cancelled context: %v", contextErr)
		}
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if !reflect.DeepEqual(manifest.Artifacts, []transport.ResultArtifact{artifact}) {
		t.Fatalf("published artifacts = %#v, want %#v", manifest.Artifacts, artifact)
	}
}

func TestRunJobPublishesHydrationFailureAndRejectsMissingIdentity(t *testing.T) {
	producerStep := "gha-producer"
	producerPlanDigest := transport.Digest([]byte("producer-plan"))
	job := cliRunJobPlan()
	job.Dependencies = []string{producerStep}
	job.NeedSources = map[string][]plan.NeedSource{
		"producer": {{StepKey: producerStep, PlanDigest: producerPlanDigest}},
	}
	planPath, planDigest := writeCLIJobPlan(t, job)

	t.Run("needs require Buildkite", func(t *testing.T) {
		clearCLIJobIdentity(t)
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "prerequisites require Buildkite result identity") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want no unauthenticated agent calls", runner.commands)
		}
	})

	t.Run("hydration failure is published", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		runner := &cliCaptureRunner{jobByStep: map[string]string{producerStep: cliTestProducerJobID}}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "hydrate prerequisite results") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "+++ :warning: Prepare GitHub Actions job failed\n~~~ :package: Publish GitHub Actions result\n") {
			t.Fatalf("stdout = %q, want visible prerequisite failure before collapsed publication", stdout.String())
		}
		manifest := publishedCLIManifest(t, runner, job, planDigest)
		if manifest.Result != "failure" {
			t.Fatalf("published result = %q, want failure", manifest.Result)
		}
	})

	t.Run("partial identity fails before execution", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		t.Setenv("BUILDKITE_JOB_ID", "")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "requires valid BUILDKITE_BUILD_ID") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want identity failure before side effects", runner.commands)
		}
	})

	t.Run("missing plan digest fails before execution", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "requires BUILDKITE_GHA_PLAN_DIGEST") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want digest failure before side effects", runner.commands)
		}
	})
}

func TestRunJobFailsWhenAuthoritativePublicationFails(t *testing.T) {
	job := cliRunJobPlan()
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	events := captureCommandTelemetry(t)
	runner := &cliCaptureRunner{failAt: 1}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "publish terminal result") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "~~~ :package: Publish GitHub Actions result\n") || !strings.Contains(stdout.String(), "^^^ +++\n") {
		t.Fatalf("stdout = %q, want failed publication group expanded", stdout.String())
	}
	if event := <-events; event.FailurePhase != telemetry.FailurePhaseResultPublication || event.FailureCode != telemetry.FailureCodeUnknown {
		t.Fatalf("telemetry = %#v", event)
	}
}

func TestRunJobTelemetryClassifiesExecutionFailure(t *testing.T) {
	job := cliRunJobPlan()
	job.Steps[0].Command = "exit 7"
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	events := captureCommandTelemetry(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != 1 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if event := <-events; event.FailurePhase != telemetry.FailurePhaseExecution || event.FailureCode != telemetry.FailureCodeUnknown {
		t.Fatalf("telemetry = %#v", event)
	}
}

func TestRunJobPublishesBoundedFailureForUnrepresentableOutputs(t *testing.T) {
	job := cliRunJobPlan()
	job.Steps[0].Command = `printf 'summary from malformed result\n' >> "$GITHUB_STEP_SUMMARY"`
	job.Outputs = make(map[string]string, transport.MaxResultOutputs+1)
	for i := 0; i <= transport.MaxResultOutputs; i++ {
		job.Outputs[fmt.Sprintf("output_%02d", i)] = "value"
	}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "validate terminal result") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if manifest.Result != "failure" || len(manifest.Outputs) != 0 {
		t.Fatalf("published result = %#v, want bounded failure without outputs", manifest)
	}
	last := runner.commands[len(runner.commands)-1]
	wantSummary := "<h2 class=\"h4 mb2\">GitHub Actions job summary</h2>\n<div class=\"border-top border-gray pt2\"></div>\n\nsummary from malformed result\n"
	if len(last.args) == 0 || last.args[0] != "annotate" || string(last.stdin) != wantSummary {
		t.Fatalf("last command = %#v, want summary annotation after bounded failure", last)
	}
}

func TestVerifyBuildkiteTargetFailsClosed(t *testing.T) {
	job := plan.Job{Target: plan.Target{StepKey: "gha-expected", Queue: "gha-runtime"}}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "gha-other")
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "gha-runtime")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "executing step") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want step mismatch", err)
	}
	t.Setenv("BUILDKITE_STEP_KEY", "")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "requires BUILDKITE_STEP_KEY") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want missing binding", err)
	}
	t.Setenv("BUILDKITE_STEP_KEY", "gha-expected")
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "gha-other")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "executing queue") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want explicit queue mismatch", err)
	}
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "requires BUILDKITE_AGENT_META_DATA_QUEUE") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want missing explicit queue binding", err)
	}
	job.Target.Queue = ""
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "customer-default")
	if err := verifyBuildkiteTarget(job); err != nil {
		t.Fatalf("verifyBuildkiteTarget() default targeting error = %v", err)
	}
}

func TestArgumentParsersRejectRepeatedOptions(t *testing.T) {
	if _, _, err := validateActionCacheArgs([]string{"--action-cache-dir", "one", "--action-cache-dir", "two", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("validateActionCacheArgs() error = %v, want duplicate cache directory error", err)
	}
	if _, _, err := validateActionCacheArgs([]string{"--action-cache-dir", ""}); err == nil || !strings.Contains(err.Error(), "requires a path") {
		t.Fatalf("validateActionCacheArgs() error = %v, want empty cache directory error", err)
	}
	if args, cacheDir, err := validateActionCacheArgs([]string{"--profile", "hosted", "--action-cache-dir", "cache", "--all-events", "workflow.yml"}); err != nil || cacheDir != "cache" || !slices.Equal(args, []string{"--profile", "hosted", "--all-events", "workflow.yml"}) {
		t.Fatalf("validateActionCacheArgs() = %q, %q, %v", args, cacheDir, err)
	}
	if _, err := runJobArgs([]string{"--plan", "one", "--plan", "two"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("runJobArgs() error = %v, want duplicate option error", err)
	}
	if _, err := runJobArgs([]string{"--plan", "one", "--hosted-tool-cache", "--hosted-tool-cache"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("runJobArgs() error = %v, want duplicate hosted tool cache error", err)
	}
	if options, err := runJobArgs([]string{"--hosted-tool-cache", "--result", "result.json", "--plan", "plan.json"}); err != nil || options.planPath != "plan.json" || options.resultPath != "result.json" || !options.hostedToolCache {
		t.Fatalf("runJobArgs() = %#v, %v", options, err)
	}
	if _, err := runJobArgs([]string{"--plan", "plan.json", "--plan-digest", "sha256:" + strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("runJobArgs() error = %v, want conflicting plan source error", err)
	}
	if _, err := runJobArgs([]string{"--plan", "", "--plan-digest", "sha256:" + strings.Repeat("0", 64), "--plan-producer", "importer"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("runJobArgs() error = %v, want conflicting empty plan source error", err)
	}
	if _, err := runJobArgs([]string{"--plan", ""}); err == nil || !strings.Contains(err.Error(), "requires a path") {
		t.Fatalf("runJobArgs() error = %v, want empty plan path error", err)
	}
	if _, err := runJobArgs([]string{"--plan", "plan.json", "--plan-digest", ""}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("runJobArgs() error = %v, want conflicting empty digest source error", err)
	}
	if _, err := runJobArgs([]string{"--plan-digest", "sha256:" + strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "both --plan-digest and --plan-producer") {
		t.Fatalf("runJobArgs() error = %v, want incomplete artifact source error", err)
	}
	if _, _, err := workflowArgs([]string{"--event-path", "one", "--event-path", "two", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("workflowArgs() error = %v, want duplicate option error", err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--format", "json", "--format", "text", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("validateArgs() error = %v, want duplicate format error", err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--profile", "hosted", "--profile", "hosted", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("validateArgs() error = %v, want duplicate profile error", err)
	}
	if _, _, _, _, profile, _, err := validateArgs([]string{"--profile", "hosted-tokenless", "workflow.yml"}); err != nil || profile != "hosted" {
		t.Fatalf("validateArgs() legacy profile = %q, %v", profile, err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--profile", "unknown", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), `must be "hosted"`) {
		t.Fatalf("validateArgs() error = %v, want unknown profile error", err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--profile", "hosted", "--event", "push", "--event-path", "event.json", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("validateArgs() error = %v, want mutually exclusive event inputs", err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--event", "push", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "requires --profile hosted") {
		t.Fatalf("validateArgs() error = %v, want profile requirement", err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--profile", "hosted", "--event", "issues", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "supported events") {
		t.Fatalf("validateArgs() error = %v, want supported event list", err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--profile", "hosted", "--all-events", "--event", "push", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("validateArgs() error = %v, want mutually exclusive all-events input", err)
	}
	if _, _, _, _, _, _, err := validateArgs([]string{"--all-events", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "requires --profile hosted") {
		t.Fatalf("validateArgs() error = %v, want all-events profile requirement", err)
	}
	if _, _, _, err := compileArgs([]string{"--format", "pipeline", "--format", "ir-json", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("compileArgs() error = %v, want duplicate format error", err)
	}
	if _, _, err := uploadArgs([]string{"--runtime-queue", "one", "--runtime-queue", "two", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("uploadArgs() error = %v, want duplicate runtime queue error", err)
	}
	if _, err := parseUploadArgs([]string{"--experimental-runner-user", "--experimental-runner-user", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("parseUploadArgs() error = %v, want duplicate experimental runner user error", err)
	}
	if parsed, err := parseUploadArgs([]string{"--experimental-runner-user=false", "workflow.yml"}); err != nil || parsed.experimentalRunnerUser {
		t.Fatalf("parseUploadArgs() opt-out = %#v, %v", parsed, err)
	}
	if _, err := parseUploadArgs([]string{"--experimental-runner-user=maybe", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("parseUploadArgs() error = %v, want boolean runner user error", err)
	}
	workflows, event, err := uploadArgs([]string{"workflow.yml"})
	if err != nil || !slices.Equal(workflows, []string{"workflow.yml"}) || event != "" {
		t.Fatalf("uploadArgs() default = %q, %q, %v", workflows, event, err)
	}
	if _, _, err := uploadArgs([]string{"--runtime-queue", "custom-runners", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), `must be "hosted"`) {
		t.Fatalf("uploadArgs() error = %v, want legacy runtime queue error", err)
	}
	if _, _, err := uploadArgs([]string{"--private-checkout", "--private-checkout", "--runtime-queue", "hosted", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("uploadArgs() error = %v, want duplicate private checkout error", err)
	}
	workflows, event, err = uploadArgs([]string{"--private-checkout", "--runtime-queue", "hosted", "--event-path", "event.json", "workflow.yml"})
	if err != nil || !slices.Equal(workflows, []string{"workflow.yml"}) || event != "event.json" {
		t.Fatalf("uploadArgs() deprecated private checkout = %q, %q, %v", workflows, event, err)
	}
}

func TestUploadArgsAcceptsExplicitPathsAndEndOfOptions(t *testing.T) {
	workflows, event, err := uploadArgs([]string{
		"first.yml", "--event-path", "event.json", "second.yaml",
	})
	if err != nil || !slices.Equal(workflows, []string{"first.yml", "second.yaml"}) || event != "event.json" {
		t.Fatalf("uploadArgs() list = %q, %q, %v", workflows, event, err)
	}

	workflows, event, err = uploadArgs([]string{
		"--event-path", "event.json", "--", "-first.yml", "--event-path",
	})
	if err != nil || !slices.Equal(workflows, []string{"-first.yml", "--event-path"}) || event != "event.json" {
		t.Fatalf("uploadArgs() after -- = %q, %q, %v", workflows, event, err)
	}
	if _, _, err := uploadArgs([]string{"-first.yml", "second.yml"}); err == nil || !strings.Contains(err.Error(), `unknown option "-first.yml"`) {
		t.Fatalf("uploadArgs() leading-dash error = %v", err)
	}
	if _, _, err := uploadArgs(nil); err == nil || !strings.Contains(err.Error(), "workflow path is required") {
		t.Fatalf("uploadArgs() empty error = %v", err)
	}
	if _, _, err := workflowArgs([]string{"first.yml", "second.yml"}); err == nil || !strings.Contains(err.Error(), "expected one workflow path") {
		t.Fatalf("workflowArgs() accepted a list: %v", err)
	}
}

func TestUploadArgsParsesPlatformRuntimeDistributions(t *testing.T) {
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	parsed, err := parseUploadArgs([]string{
		"--experimental-runner-user",
		"--runner-queue", "ubuntu-latest=hosted",
		"--runner-image", "ubuntu-latest=" + image,
		"--runner-queue", "macos-14=macos-sonoma-arm64",
		"--runtime-distribution", "linux/amd64=/tmp/buildkite-gha-linux",
		"--event-path", "event.json",
		"--runtime-distribution", "darwin/arm64=/tmp/buildkite-gha-darwin",
		"workflow.yml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(parsed.workflowOperands, []string{"workflow.yml"}) || parsed.eventPath != "event.json" || parsed.runtimeDistributionPaths[compiler.PlatformLinuxAMD64] != "/tmp/buildkite-gha-linux" || parsed.runtimeDistributionPaths[compiler.PlatformDarwinARM64] != "/tmp/buildkite-gha-darwin" || !parsed.experimentalRunnerUser {
		t.Fatalf("parseUploadArgs() = %#v", parsed)
	}
	if got := parsed.runnerTargets["ubuntu-latest"]; got != (compiler.RunnerTarget{Queue: "hosted", Platform: compiler.PlatformLinuxAMD64, Image: image}) {
		t.Fatalf("Linux runner target = %#v", got)
	}
	if got := parsed.runnerTargets["macos-14"]; got != (compiler.RunnerTarget{Queue: "macos-sonoma-arm64", Platform: compiler.PlatformDarwinARM64}) {
		t.Fatalf("macOS runner target = %#v", got)
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"--runtime-distribution", "darwin/amd64=/tmp/runtime", "workflow.yml"}, want: "unsupported runtime platform"},
		{args: []string{"--runtime-distribution", "darwin/arm64=relative", "workflow.yml"}, want: "must use an absolute path"},
		{args: []string{"--runtime-distribution", "darwin/arm64=/tmp/one", "--runtime-distribution", "darwin/arm64=/tmp/two", "workflow.yml"}, want: "may only be specified once"},
		{args: []string{"--runtime-distribution", "darwin/arm64", "workflow.yml"}, want: "requires platform=absolute-path"},
		{args: []string{"--runner-queue", "ubuntu-latest=one", "--runner-queue", "UBUNTU-LATEST=two", "workflow.yml"}, want: "may only be specified once"},
		{args: []string{"--runner-image", "ubuntu-latest=" + image, "workflow.yml"}, want: "requires --runner-queue"},
		{args: []string{"--runner-queue", "macos-14=macos", "--runner-image", "macos-14=" + image, "workflow.yml"}, want: "unsupported on darwin/arm64"},
		{args: []string{"--runner-queue", "windows-latest=windows", "workflow.yml"}, want: "unsupported runner label"},
		{args: []string{"--runner-queue", "ubuntu-20.04=hosted", "workflow.yml"}, want: "unsupported runner label"},
		{args: []string{"--runner-queue", "ubuntu-latest=not a queue", "workflow.yml"}, want: "runner queue"},
		{args: []string{"--runner-queue", "ubuntu-latest=hosted", "--runner-image", "ubuntu-latest=ubuntu:latest", "workflow.yml"}, want: "immutable registry sha256 reference"},
	} {
		if _, err := parseUploadArgs(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("parseUploadArgs(%#v) error = %v, want %q", test.args, err, test.want)
		}
	}
}

func TestValidateActionCacheRequiresHostedProfile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--action-cache-dir", t.TempDir(), "workflow.yml"}, &stdout, &stderr, "dev"); code != 2 || !strings.Contains(stderr.String(), "requires --profile hosted") {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	stderr.Reset()
	missing := filepath.Join(t.TempDir(), "missing")
	if code := Run([]string{"validate", "--profile", "hosted", "--event", "push", "--action-cache-dir", missing, "workflow.yml"}, &stdout, &stderr, "dev"); code != 2 || !strings.Contains(stderr.String(), "cache root is not a non-symlink directory") {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
}

func TestLoadRuntimeDistributionsValidatesPlatformBinaryAndSymlink(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	platform, err := compiler.ParsePlatform(runtime.GOOS + "/" + runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	distributions, err := loadRuntimeDistributions(map[compiler.Platform]string{platform: executable})
	if err != nil {
		t.Fatal(err)
	}
	if got := distributions[platform].digest; got != cliTestRuntimeDigest() {
		t.Fatalf("runtime digest = %q, want %q", got, cliTestRuntimeDigest())
	}
	other := compiler.PlatformDarwinARM64
	wantFormat := "Mach-O"
	if platform == compiler.PlatformDarwinARM64 {
		other = compiler.PlatformLinuxAMD64
		wantFormat = "ELF"
	}
	if _, err := loadRuntimeDistributions(map[compiler.Platform]string{other: executable}); err == nil || !strings.Contains(err.Error(), wantFormat) {
		t.Fatalf("%s runtime accepted %s executable: %v", other, platform, err)
	}
	symlink := filepath.Join(t.TempDir(), "runtime")
	if err := os.Symlink(executable, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimeDistributions(map[compiler.Platform]string{platform: symlink}); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("runtime symlink error = %v", err)
	}
}

func TestRepositoryProviderGitCredentialsUseServerEnvironment(t *testing.T) {
	values := map[string]string{}
	getenv := func(name string) string { return values[name] }
	if repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("repository provider credentials enabled without a server setting")
	}
	values[repositoryProviderGitCredentialsEnvironment] = "true"
	if !repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("repository provider credentials setting was ignored")
	}
	delete(values, repositoryProviderGitCredentialsEnvironment)
	values[legacyGitHubAppGitCredentialsEnvironment] = "true"
	if !repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("legacy GitHub App credentials setting was ignored")
	}
	values[legacyGitHubAppGitCredentialsEnvironment] = "TRUE"
	if repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("non-canonical server setting enabled repository credentials")
	}
}

func TestRunJobExecutesPureRunPlanWithoutCheckout(t *testing.T) {
	clearCLIJobIdentity(t)
	workspace := t.TempDir()
	requiresMise := false
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: cliTestRuntimeDigest(),
		},
		Runtime: &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()},
		Workflow: plan.Workflow{
			Path:         "missing-workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "missing",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-missing", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
		RequiresMise:         &requiresMise,
	}
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(workspace, "plan.json")
	resultPath := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	oldDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDirectory) })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run-job", "--plan", planPath, "--result", resultPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q, want success", code, stderr.String())
	}
	result, err := os.ReadFile(resultPath)
	if err != nil || !bytes.Contains(result, []byte(`"conclusion": "success"`)) {
		t.Fatalf("result = %q, error = %v", result, err)
	}
}

func cliRunJobPlan() plan.Job {
	requiresMise := false
	return plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: cliTestRuntimeDigest(),
		},
		Runtime: &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()},
		Workflow: plan.Workflow{
			Path:         "workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "cli",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-cli", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
		RequiresMise:         &requiresMise,
	}
}

func cliTestRuntimeDigest() string {
	digest, err := executableDigest()
	if err != nil {
		panic(err)
	}
	return digest
}

func writeCLIJobPlan(t *testing.T, job plan.Job) (string, string) {
	t.Helper()
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, transport.Digest(encoded)
}

func setCLIJobIdentity(t *testing.T, job plan.Job, planDigest string) {
	t.Helper()
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", job.Target.StepKey)
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", job.Target.Queue)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", planDigest)
}

func captureCommandTelemetry(t *testing.T) <-chan telemetry.Properties {
	t.Helper()
	events := make(chan telemetry.Properties, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received struct {
			Properties telemetry.Properties `json:"properties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode telemetry event: %v", err)
		}
		events <- received.Properties
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")
	return events
}

func clearCLIJobIdentity(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BUILDKITE",
		"BUILDKITE_BUILD_ID",
		"BUILDKITE_JOB_ID",
		"BUILDKITE_STEP_KEY",
		"BUILDKITE_AGENT_META_DATA_QUEUE",
		"BUILDKITE_GHA_PLAN_DIGEST",
	} {
		t.Setenv(name, "")
	}
}

func publishedCLIManifest(t *testing.T, runner *cliCaptureRunner, job plan.Job, planDigest string) transport.ResultManifest {
	t.Helper()
	path := transport.ResultPath(job.Target.StepKey, planDigest)
	data, ok := runner.uploaded[path]
	if !ok {
		t.Fatalf("authoritative result %q was not uploaded; uploads = %#v", path, runner.uploaded)
	}
	producer := transport.Producer{BuildID: cliTestBuildID, JobID: cliTestJobID, StepKey: job.Target.StepKey}
	manifest, err := transport.VerifyResultManifest(data, planDigest, producer)
	if err != nil {
		t.Fatalf("verify published result: %v\n%s", err, data)
	}
	return manifest
}
