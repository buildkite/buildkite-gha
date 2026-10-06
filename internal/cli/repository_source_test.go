package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
)

func TestRepositorySourceSetupSafeCauses(t *testing.T) {
	const sensitive = "/private/customer/token-secret"
	for _, test := range []struct {
		name, operation, cause, advice string
		err                            error
	}{
		{"missing Git", "resolve Git executable", "Git was not found on PATH", "install Git", &exec.Error{Name: sensitive, Err: exec.ErrNotFound}},
		{"missing temporary directory", "create action source store", "temporary directory does not exist", "create the directory", &os.PathError{Op: "stat", Path: sensitive, Err: os.ErrNotExist}},
		{"temporary path is a file", "create action source store", "not a directory", "to a directory", &os.PathError{Op: "mkdir", Path: sensitive, Err: syscall.ENOTDIR}},
		{"permission denied", "create action source store", "access to temporary storage was denied", "grant the importer user access", &os.PathError{Op: "mkdir", Path: sensitive, Err: os.ErrPermission}},
		{"storage full", "create action source store", "storage is full", "free temporary-storage capacity", &os.PathError{Op: "mkdir", Path: sensitive, Err: syscall.ENOSPC}},
		{"quota exhausted", "create action source store", "quota is exhausted", "available capacity", &os.PathError{Op: "mkdir", Path: sensitive, Err: syscall.EDQUOT}},
		{"unknown resolver failure", "configure public action resolver", "cause is unrecognized", "Contact Buildkite support", errors.New(sensitive + " backend response")},
		{"unknown store failure", "configure public action source store", "cause is unrecognized", "Contact Buildkite support", errors.New(sensitive + " credentials invalid")},
		{"unrecognized Git failure", "resolve Git executable", "cause is unrecognized", "Contact Buildkite support", errors.New("not found: " + sensitive)},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupErr := &repositorySourceSetupError{operation: test.operation, err: test.err}
			if !errors.Is(setupErr, test.err) || !strings.Contains(setupErr.Error(), sensitive) {
				t.Fatal("setup error lost its original cause")
			}
			report := repositorySourceSetupReport("ci.yml", "repository source could not be configured", fmt.Errorf("outer wrapper: %w", setupErr))
			if report.Result != "indeterminate" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "E_ENVIRONMENT" {
				t.Fatalf("report = %#v", report)
			}
			detail := report.Diagnostics[0].Detail
			for _, want := range []string{test.cause, test.advice} {
				if !strings.Contains(detail, want) {
					t.Fatalf("detail = %q, want %q", detail, want)
				}
			}
			if test.cause == "cause is unrecognized" && !strings.Contains(detail, test.operation) {
				t.Fatalf("unknown cause lost operation: %q", detail)
			}
			if test.cause != "cause is unrecognized" && (!strings.Contains(detail, "If you manage the environment running this command, ") || !strings.Contains(detail, "On Buildkite-hosted agents")) {
				t.Fatalf("guidance assumes agent ownership: %q", detail)
			}
			details := &commandTelemetryDetails{}
			details.observe(report)
			payload, err := json.Marshal(details.forOutcome(telemetry.OutcomeFailure))
			if err != nil {
				t.Fatal(err)
			}
			_, annotation := processingAnnotation(t.Context(), report, sourceLinkContext{})
			for _, output := range []string{detail, annotation, string(payload)} {
				if strings.Contains(output, sensitive) || strings.Contains(output, "backend response") || strings.Contains(output, "credentials invalid") {
					t.Fatalf("unsafe cause escaped: %q", output)
				}
			}
			if len(report.Diagnostics[0].Message)+1+len(detail) > 1024 {
				t.Fatal("cause and advice exceed the telemetry bound")
			}
		})
	}
}

func TestRepositorySourceConfigurationErrorsKeepOperationAndCleanup(t *testing.T) {
	for _, operation := range []string{"configure public action resolver", "configure public action source store"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			invalid := []actionsource.Option{actionsource.WithTestEndpoints("invalid")}
			var resolverOptions, storeOptions []actionsource.Option
			if operation == "configure public action resolver" {
				resolverOptions = invalid
			} else {
				storeOptions = invalid
			}
			_, cleanup, err := newHostedActionSource(t.Context(), "", "dev", resolverOptions, storeOptions)
			cleanup()
			var setupErr *repositorySourceSetupError
			if !errors.As(err, &setupErr) || setupErr.operation != operation {
				t.Fatalf("configuration error = %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary source store leaked: %v, %v", entries, err)
			}
		})
	}
}

func TestPluginRepositorySourceFailureReachesAnnotationsAndTelemetry(t *testing.T) {
	requireImporterHost(t)
	workflow, err := os.ReadFile("../../testdata/smoke/.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, temporaryPathIsFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("temporaryPathIsFile=%t", temporaryPathIsFile), func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{"ci.yml": string(workflow)})
			t.Chdir(repository)
			setCLIPluginBuildkiteEnvironment(t, "importer")
			configuration, err := json.Marshal(map[string]any{"workflow": ".github/workflows/ci.yml"})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(pluginConfigurationEnvironment, string(configuration))
			root := t.TempDir()
			const sensitive = "private-token-secret"
			cause, advice := "temporary directory does not exist", "create the directory"
			if temporaryPathIsFile {
				if err := os.WriteFile(filepath.Join(root, sensitive), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				cause, advice = "not a directory", "to a directory"
			}
			t.Setenv("TMPDIR", filepath.Join(root, sensitive))
			received := make(chan telemetry.Properties, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var event struct {
					Properties telemetry.Properties `json:"properties"`
				}
				if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
					t.Error(err)
				}
				received <- event.Properties
				w.WriteHeader(http.StatusAccepted)
			}))
			defer server.Close()
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "importer")
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
			t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", sensitive)
			t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 1 {
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			}
			if pipelineUploads(runner) != 0 {
				t.Fatal("setup failure uploaded a pipeline")
			}
			if len(received) != 1 {
				t.Fatalf("telemetry events = %d", len(received))
			}
			properties := <-received
			if properties.Outcome != telemetry.OutcomeFailure || properties.FailureCode != "E_ENVIRONMENT" || len(properties.Diagnostics) != 1 || properties.Diagnostics[0].Code != "E_ENVIRONMENT" {
				t.Fatalf("telemetry = %#v", properties)
			}
			if properties.ErrorMessage != properties.Diagnostics[0].Message {
				t.Fatalf("command and diagnostic causes diverged: %#v", properties)
			}
			var annotation string
			for _, command := range runner.commands {
				if len(command.args) > 0 && command.args[0] == "annotate" {
					annotation += string(command.stdin)
				}
			}
			for _, output := range []string{stderr.String(), annotation, properties.ErrorMessage} {
				if !strings.Contains(output, cause) || !strings.Contains(output, advice) || !strings.Contains(output, "On Buildkite-hosted agents") {
					t.Fatalf("missing safe cause/advice: %q", output)
				}
			}
			payload, err := json.Marshal(properties)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(payload), sensitive) || strings.Contains(annotation, sensitive) {
				t.Fatalf("sensitive cause escaped: telemetry=%s annotation=%s", payload, annotation)
			}
		})
	}
}

func TestValidateAllEventsRepositorySourceFailureHasSafeDetail(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(root, "private-token-secret"))
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--profile", "hosted", "--all-events", "--format", "json", "../../testdata/smoke/.github/workflows/ci.yml"}, &stdout, &stderr, "dev", runner); code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReportV3
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Validation.Diagnostics) != 1 || report.Validation.Diagnostics[0].Code != "E_ENVIRONMENT" {
		t.Fatalf("report = %#v", report)
	}
	var annotation string
	for _, command := range runner.commands {
		if len(command.args) > 0 && command.args[0] == "annotate" {
			annotation += string(command.stdin)
		}
	}
	for _, output := range []string{stdout.String(), annotation} {
		if !strings.Contains(output, "temporary directory does not exist") || !strings.Contains(output, "create the directory") || strings.Contains(output, "private-token-secret") {
			t.Fatalf("missing safe diagnostic: %q", output)
		}
	}
}
