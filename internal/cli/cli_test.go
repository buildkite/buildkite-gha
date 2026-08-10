package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"go.yaml.in/yaml/v4"
)

const (
	cliTestBuildID       = "11111111-1111-4111-8111-111111111111"
	cliTestJobID         = "22222222-2222-4222-8222-222222222222"
	cliTestProducerJobID = "33333333-3333-4333-8333-333333333333"
)

func TestRunHelpAndVersion(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{name: "help flag", args: []string{"--help"}, wantOutput: "validate"},
		{name: "help command", args: []string{"help"}, wantOutput: "run-job"},
		{name: "command help", args: []string{"help", "compile"}, wantOutput: "buildkite-gha compile --event-path"},
		{name: "upload help", args: []string{"help", "upload"}, wantOutput: "default agent targeting"},
		{name: "upload help flag", args: []string{"upload", "--help"}, wantOutput: "default agent targeting"},
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
	if outputs[0] != outputs[1] || !strings.Contains(outputs[0], targetQueueEnvironment) {
		t.Fatalf("upload help outputs differ or omit target queue environment:\nhelp command: %q\nhelp flag: %q", outputs[0], outputs[1])
	}
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

	t.Run("validate json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.Report
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "compilable" || report.Instances != 3 {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("validate json blocker", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "dynamic.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}\n    steps:\n      - run: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflow}, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var report compatibility.Report
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "E_COMPILE" {
			t.Fatalf("report = %#v", report)
		}

		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("profile Run() code = %d, want 1; stderr = %q", code, stderr.String())
		}
		var profileReport compatibility.ProfileReport
		if err := json.Unmarshal(stdout.Bytes(), &profileReport); err != nil {
			t.Fatal(err)
		}
		if profileReport.Result != "incompatible" || profileReport.Compile.Result != "incompatible" || profileReport.Admission.Result != "not-evaluated" || len(profileReport.Diagnostics) != 1 || profileReport.Diagnostics[0].Code != "E_COMPILE" {
			t.Fatalf("profile report = %#v", profileReport)
		}
	})

	t.Run("validate hosted tokenless profile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}
		runner := &cliCaptureRunner{}
		if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("profile validation made Buildkite calls: %#v", runner.commands)
		}
		var report compatibility.ProfileReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "admitted" || report.Profile != "hosted-tokenless" || report.Compile.Instances != 3 || report.Admission.Result != "admitted" || len(report.Diagnostics) != 0 {
			t.Fatalf("profile report = %#v", report)
		}
	})

	t.Run("validate profile requires event", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--profile", "hosted-tokenless", workflowPath}, &stdout, &stderr, "dev"); code != 2 || !strings.Contains(stderr.String(), "--event-path is required with --profile") {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("validate profile importer cannot collide with generated key", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "profile.yml")
		if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  profile:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProfileReport
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
		var report compatibility.Report
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
		var report compatibility.ProfileReport
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

func TestUnsupportedConditionPreflightAppliesToEveryCompilerEntryPoint(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "conditions.yml")
	if err := os.WriteFile(workflowPath, []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: [12, "14"]
    if: matrix.version == 12
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	want := `job condition: condition equality compares incompatible string and number operands`

	t.Run("validate json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", "--format", "json", workflowPath}, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.Report
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "E_COMPILE" || !strings.Contains(report.Diagnostics[0].Message, want) {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("profile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var report compatibility.ProfileReport
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

	t.Run("upload before Buildkite calls", func(t *testing.T) {
		t.Setenv("BUILDKITE", "true")
		t.Setenv("BUILDKITE_STEP_KEY", "condition-importer")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		args := []string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}
		if code := run(args, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), want) {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("unsupported condition made Buildkite calls: %#v", runner.commands)
		}
	})
}

func TestUploadRejectsUnsupportedEffectiveActionInputDefaultBeforeBuildkiteCalls(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "action.yml")
	actionRoot := filepath.Join(root, ".github", "actions", "complex-default")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/complex-default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte(`name: complex default
inputs:
  token:
    default: ${{ github.server_url == 'https://github.com' && github.token || '' }}
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
	if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code != 1 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{`compile action "./.github/actions/complex-default"`, `action input "token" default`, "requires a direct context reference"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	if len(runner.commands) != 0 {
		t.Fatalf("unsupported action default made Buildkite calls: %#v", runner.commands)
	}
}

func TestValidateHostedTokenlessProfileResolvesActionsWithoutClaimingRuntime(t *testing.T) {
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
	var report compatibility.ProfileReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "W_ACTION_RUNTIME_UNKNOWN" {
		t.Fatalf("profile report = %#v", report)
	}

	t.Setenv("BUILDKITE_GHA_NODE20", filepath.Join(root, "missing-node20"))
	t.Setenv(targetQueueEnvironment, "not a queue")
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

func TestValidateHostedTokenlessProfileRejectsProtectedCapabilityAfterCompile(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "secret.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  secret:\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets.TOKEN }}\n    steps:\n      - run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProfileReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "not-admitted" || report.Compile.Result != "compilable" || report.Admission.Result != "not-admitted" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "E_PROFILE" || !strings.Contains(report.Diagnostics[0].Message, `capability "secrets"`) {
		t.Fatalf("profile report = %#v", report)
	}
}

func TestRunUploadCompilesArtifactsAndUploadsSelfContainedPipeline(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "phase-2-importer")
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
	}
	if err := yaml.Unmarshal(pipelineCommand.stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 3 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps {
		if step.Cache != nil {
			t.Fatalf("shell-only step %q unexpectedly configures a runtime cache: %#v", step.Key, step.Cache)
		}
		if !step.Checkout.Skip || step.Agents != nil || len(step.DependsOn) == 0 || step.DependsOn[0].Step != "phase-2-importer" || step.DependsOn[0].AllowFailure {
			t.Fatalf("step %q lacks isolated checkout or exact importer dependency: %#v", step.Key, step)
		}
		if !strings.Contains(step.Command, `bootstrap_dir="$(mktemp -d `) ||
			!strings.Contains(step.Command, `--step 'phase-2-importer'`) ||
			!strings.Contains(step.Command, `sha256sum "$distribution"`) ||
			!strings.Contains(step.Command, `"$distribution" run-job --plan "$plan"`) {
			t.Fatalf("step %q command is not self-contained:\n%s", step.Key, step.Command)
		}
	}
}

func TestRunUploadUsesExplicitTargetQueue(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "explicit-queue-importer")
	t.Setenv(targetQueueEnvironment, "hosted")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, "--runtime-queue", "hosted", workflowPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}

	var pipeline struct {
		Steps []struct {
			Key    string            `yaml:"key"`
			Agents map[string]string `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 3 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps {
		if step.Agents["queue"] != "hosted" {
			t.Fatalf("step %q agents = %#v, want hosted queue", step.Key, step.Agents)
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
		if job.Schema == plan.SchemaV7 || job.Target.Queue != "hosted" {
			t.Fatalf("explicitly targeted plan = schema %q, target %#v", job.Schema, job.Target)
		}
	}
	if planCount != 3 {
		t.Fatalf("uploaded plan count = %d, want 3", planCount)
	}
}

func TestRunUploadRejectsInvalidTargetQueueEnvironment(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	for _, test := range []struct {
		name  string
		queue string
	}{
		{name: "empty", queue: ""},
		{name: "malformed", queue: "not a queue"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "invalid-queue-importer")
			t.Setenv(targetQueueEnvironment, test.queue)
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"upload", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", runner); code == 0 {
				t.Fatalf("run() code = 0, want invalid target queue failure")
			}
			if len(runner.commands) != 0 || (!strings.Contains(stderr.String(), targetQueueEnvironment) && !strings.Contains(stderr.String(), "runner policy queue")) {
				t.Fatalf("commands = %#v, stderr = %q", runner.commands, stderr.String())
			}
		})
	}
}

func TestRunUploadMergesGeneratedJobsIntoContainingGroup(t *testing.T) {
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
			Group string `yaml:"group"`
			Key   string `yaml:"key"`
			Steps []struct {
				Key string `yaml:"key"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != ":github: Run workflow" || pipeline.Steps[0].Key != "" || len(pipeline.Steps[0].Steps) != 3 {
		t.Fatalf("grouped upload = %#v", pipeline.Steps)
	}
}

func TestRunUploadDerivesUnattestedBuildkiteEvent(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	sha := "0123456789abcdef0123456789abcdef01234567"
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "derived-event-importer")
	t.Setenv("BUILDKITE_REPO", "git@github.com:buildkite/buildkite-gha.git")
	t.Setenv("BUILDKITE_COMMIT", sha)
	t.Setenv("BUILDKITE_BRANCH", "main")
	t.Setenv("BUILDKITE_TAG", "")
	t.Setenv("BUILDKITE_PULL_REQUEST", "false")
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

func TestRunUploadUsesWebhookPayloadWithoutRetainingIt(t *testing.T) {
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
	t.Setenv("BUILDKITE_PULL_REQUEST", "false")
	t.Setenv("BUILDKITE_BUILD_AUTHOR", "Build Author")
	t.Setenv("BUILDKITE_GITHUB_EVENT", "pull_request")
	rawSecret := "raw-webhook-value-must-not-be-retained"
	runner := &cliCaptureRunner{webhook: []byte(fmt.Sprintf("{\"marker\":\"latest\",\"private\":\"%s\",\"ref\":\"refs/heads/trigger\",\"after\":\"%s\",\"repository\":{\"full_name\":\"other/trigger\"},\"sender\":{\"login\":\"octocat\"}}", rawSecret, strings.Repeat("b", 40)))}
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
			if len(runner.commands) != 1 || !slices.Equal(runner.commands[0].args, []string{"meta-data", "get", "buildkite:webhook"}) {
				t.Fatalf("commands = %#v, want only one metadata read", runner.commands)
			}
		})
	}
}

func TestRunUploadCompilesConcurrentSmokePipeline(t *testing.T) {
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "concurrent.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "phase-3-upload-importer")
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
			Key       string `yaml:"key"`
			Agents    any    `yaml:"agents"`
			DependsOn []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[3].stdin, &pipeline); err != nil {
		t.Fatalf("uploaded pipeline YAML: %v", err)
	}
	if len(pipeline.Steps) != 2 {
		t.Fatalf("uploaded steps = %#v", pipeline.Steps)
	}
	if pipeline.Steps[0].Key != "gha-concurrent" || pipeline.Steps[0].Agents != nil || len(pipeline.Steps[0].DependsOn) != 1 || pipeline.Steps[0].DependsOn[0].Step != "phase-3-upload-importer" || pipeline.Steps[0].DependsOn[0].AllowFailure {
		t.Fatalf("concurrent step = %#v", pipeline.Steps[0])
	}
	observer := pipeline.Steps[1]
	if observer.Key != "gha-observe" || observer.Agents != nil || len(observer.DependsOn) != 2 || observer.DependsOn[0].Step != "phase-3-upload-importer" || observer.DependsOn[0].AllowFailure || observer.DependsOn[1].Step != "gha-concurrent" || !observer.DependsOn[1].AllowFailure {
		t.Fatalf("observer step = %#v", observer)
	}
}

func TestRunUploadJavaScriptActionRequiresRuntimeMiseWithoutTransport(t *testing.T) {
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
			Command string `yaml:"command"`
			Cache   struct {
				Paths []string `yaml:"paths"`
				Name  string   `yaml:"name"`
			} `yaml:"cache"`
			Env map[string]string `yaml:"env"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[2].stdin, &pipeline); err != nil {
		t.Fatalf("parse uploaded pipeline: %v", err)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("uploaded pipeline steps = %#v", pipeline.Steps)
	}
	command := pipeline.Steps[0].Command
	if strings.Contains(command, ".buildkite-gha/tools/mise/") || strings.Contains(command, `export PATH="$bootstrap_dir:$PATH"`) || strings.Contains(command, "BUILDKITE_GHA_NODE") || strings.Contains(command, ".buildkite-gha/runtimes") {
		t.Fatalf("generated pipeline still transports runtime tools:\n%s", command)
	}
	step := pipeline.Steps[0]
	if step.Cache.Name != "buildkite-gha" || len(step.Cache.Paths) != 1 || step.Cache.Paths[0] != "/cache/bkcache/buildkite-gha" {
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
	if got := prepareMiseDataDir(filepath.Join(link, "mise"), &stderr); got != "" || !strings.Contains(stderr.String(), "not a real directory") {
		t.Fatalf("prepareMiseDataDir(symlink) = %q, stderr = %q", got, stderr.String())
	}
}

func TestRunUploadAllowsCompilerVerifiedLocalDockerfileAction(t *testing.T) {
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
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
	want := filepath.Join(root, "linux-x64", "mise")
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
	root := t.TempDir()
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
	privateRuntime := t.TempDir()
	got, err := pinRuntimeMise(context.Background(), cached, privateRuntime, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(privateRuntime, "mise") {
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
	if _, err := validateRuntimeMise(context.Background(), got, runtimeMiseBinaryDigest); err != nil {
		t.Fatalf("validate installed mise release: %v", err)
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
	job.Schema = plan.SchemaV7
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
		{name: "legacy or unknown action plan resolves mise", job: actionJob(nil), want: true},
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
	job.Schema = plan.SchemaV3
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
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "phase-2-importer")
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
	for _, capability := range []string{"secrets", "provider-token-write", "privileged-container", "future-capability"} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow:             plan.Workflow{LogicalJobID: "protected"},
			RequiredCapabilities: []string{capability},
		}}}}
		if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), capability) {
			t.Fatalf("validateUnprivilegedBundle(%q) error = %v, want capability rejection", capability, err)
		}
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
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "checkout provenance")) {
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
		Workflow:             plan.Workflow{LogicalJobID: "comment"},
		RequiredCapabilities: []string{"provider-token-write"},
		GitHubToken:          &plan.GitHubToken{Permissions: map[string]string{"pull_requests": "write"}},
	}
	for _, test := range []struct {
		name          string
		authorization compiler.PlanAuthorization
		wantError     bool
	}{
		{name: "verified permissions", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"workflow-permissions"}}},
		{name: "missing provenance", wantError: true},
		{name: "broadened provenance", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"workflow-permissions", "step-input"}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: job, Authorization: test.authorization}}}
			err := validateUnprivilegedBundle(bundle)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "workflow permission provenance")) {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
		})
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
	if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), "without compiler-verified Dockerfile action provenance") {
		t.Fatalf("validateUnprivilegedBundle() error = %v, want Docker provenance rejection", err)
	}
}

func TestUnprivilegedUploadRejectsContainerProvenance(t *testing.T) {
	for _, sources := range [][]string{
		{"job-containers"},
		{"service-containers"},
		{"dockerfile-actions", "job-containers", "service-containers"},
	} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow: plan.Workflow{LogicalJobID: "container-job"}, RequiredCapabilities: []string{"docker", "network"},
		}, Authorization: compiler.PlanAuthorization{DockerCapabilitySources: sources}}}}
		if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), "hosted-tokenless upload does not admit") {
			t.Fatalf("validateUnprivilegedBundle(%v) error = %v", sources, err)
		}
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
			if err == nil || !strings.Contains(err.Error(), "GitHub Actions "+test.service+" service") || !strings.Contains(err.Error(), "Phase 6") {
				t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", test.action, err)
			}
		})
	}
}

func TestUnprivilegedUploadAllowsOnlyAuditedCacheV6Commit(t *testing.T) {
	for _, path := range []string{"", "restore", "save"} {
		action := plan.ActionLock{Source: "github", Repository: "actions/cache", Path: path, Commit: actionintegration.CacheCommit}
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow: plan.Workflow{LogicalJobID: "cache-v6"},
			Actions:  []plan.ActionLock{action},
		}}}}
		if err := validateUnprivilegedBundle(bundle); err != nil {
			t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
		}
	}

	action := plan.ActionLock{Source: "github", Repository: "actions/cache", Commit: strings.Repeat("0", 40)}
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow: plan.Workflow{LogicalJobID: "cache-old"},
		Actions:  []plan.ActionLock{action},
	}}}}
	if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), "v6.1.0") {
		t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
	}
}

func TestCacheServiceRequiredUsesOnlyAuditedCacheV6Locks(t *testing.T) {
	required, err := cacheServiceRequired([]plan.ActionLock{{Source: "github", Repository: "owner/action", Commit: strings.Repeat("a", 40)}})
	if err != nil || required {
		t.Fatalf("ordinary action cache requirement = %v, %v", required, err)
	}

	locks := []plan.ActionLock{
		{Source: "github", Repository: "owner/action", Commit: strings.Repeat("a", 40)},
		{Source: "github", Repository: "actions/cache", Path: "restore", Commit: actionintegration.CacheCommit},
	}
	required, err = cacheServiceRequired(locks)
	if err != nil || !required {
		t.Fatalf("nested cache v6 requirement = %v, %v", required, err)
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
	webhook        []byte
	webhookErr     error
	jobByStep      map[string]string
	dataByPath     map[string][]byte
	uploaded       map[string][]byte
	contextErrors  []error
}

func (r *cliCaptureRunner) Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, cliCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: bytes.Clone(stdin)})
	r.contextErrors = append(r.contextErrors, ctx.Err())
	if r.failAt != 0 && len(r.commands) == r.failAt {
		return nil, errors.New("injected failure")
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
	job := plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "dev", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow: plan.Workflow{Path: "workflow.yml", Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "cli"},
		Event:    plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:   plan.Target{StepKey: "gha-cli", Queue: "ubuntu-latest"},
		Outputs:  map[string]string{"result": "${{ steps.produce.outputs.result }}"},
		Steps:    []plan.Step{{ID: "produce", Kind: "run", Command: `echo "result=cli-ok" >> "$GITHUB_OUTPUT"`}},
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
	stdout.Reset()
	stderr.Reset()
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+strings.Repeat("0", 64))
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not match expected digest") {
		t.Fatalf("Run() code = %d, stderr = %q, want digest mismatch", code, stderr.String())
	}
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
			if len(annotations) != 1 || !slices.Equal(annotations[0].args, wantArgs) || string(annotations[0].stdin) != "### Job summary\n\nPassed.\n" {
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
		wantResult string
		wantCode   int
	}{
		{name: "failure", command: "exit 7", wantResult: "failure", wantCode: 1},
		{name: "skipped", condition: "${{ false }}", command: "true", wantResult: "skipped", wantCode: 0},
		{name: "cancelled", command: "sleep 1", cancel: true, wantResult: "cancelled", wantCode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Condition = test.condition
			job.Steps[0].Command = test.command
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
			if code := runJobContext(ctx, []string{"--plan", planPath}, &stdout, &stderr, "dev", transport.Agent{Runner: runner}); code != test.wantCode {
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
	wantBodies := []string{"summary before cancellation\n", "warning before cancellation\n", "error before cancellation\n"}
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
	runner := &cliCaptureRunner{failAt: 1}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "publish terminal result") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
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
	if len(last.args) == 0 || last.args[0] != "annotate" || string(last.stdin) != "summary from malformed result\n" {
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
	if _, _, err := runJobArgs([]string{"--plan", "one", "--plan", "two"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("runJobArgs() error = %v, want duplicate option error", err)
	}
	if _, _, err := workflowArgs([]string{"--event-path", "one", "--event-path", "two", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("workflowArgs() error = %v, want duplicate option error", err)
	}
	if _, _, _, _, err := validateArgs([]string{"--format", "json", "--format", "text", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("validateArgs() error = %v, want duplicate format error", err)
	}
	if _, _, _, _, err := validateArgs([]string{"--profile", "hosted-tokenless", "--profile", "hosted-tokenless", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("validateArgs() error = %v, want duplicate profile error", err)
	}
	if _, _, _, _, err := validateArgs([]string{"--profile", "unknown", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), `must be "hosted-tokenless"`) {
		t.Fatalf("validateArgs() error = %v, want unknown profile error", err)
	}
	if _, _, _, err := compileArgs([]string{"--format", "pipeline", "--format", "ir-json", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("compileArgs() error = %v, want duplicate format error", err)
	}
	if _, _, err := uploadArgs([]string{"--runtime-queue", "one", "--runtime-queue", "two", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("uploadArgs() error = %v, want duplicate runtime queue error", err)
	}
	workflow, event, err := uploadArgs([]string{"workflow.yml"})
	if err != nil || workflow != "workflow.yml" || event != "" {
		t.Fatalf("uploadArgs() default = %q, %q, %v", workflow, event, err)
	}
	if _, _, err := uploadArgs([]string{"--runtime-queue", "custom-runners", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), `must be "hosted"`) {
		t.Fatalf("uploadArgs() error = %v, want legacy runtime queue error", err)
	}
	if _, _, err := uploadArgs([]string{"--private-checkout", "--private-checkout", "--runtime-queue", "hosted", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "only be specified once") {
		t.Fatalf("uploadArgs() error = %v, want duplicate private checkout error", err)
	}
	workflow, event, err = uploadArgs([]string{"--private-checkout", "--runtime-queue", "hosted", "--event-path", "event.json", "workflow.yml"})
	if err != nil || workflow != "workflow.yml" || event != "event.json" {
		t.Fatalf("uploadArgs() deprecated private checkout = %q, %q, %v", workflow, event, err)
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
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Workflow: plan.Workflow{
			Path:         "missing-workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "missing",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-missing", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
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
	return plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Workflow: plan.Workflow{
			Path:         "workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "cli",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-cli", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
	}
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
