package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"go.yaml.in/yaml/v4"
)

// Stable stage IDs asserted throughout the report expectations below.
const (
	stageWorkflowParsing = compiler.StageWorkflowParsing
	stageEventValidation = compiler.StageEventValidation
	stageGraph           = compiler.StageGraph
	stageMatrix          = compiler.StageMatrix
	stageExpressions     = compiler.StageExpressions
	stageResolution      = compiler.StageResolution
)

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
			{name: "unsupported event", trigger: "issue_comment", want: `unsupported GitHub trigger event "issue_comment"`},
			{name: "malformed path filter", trigger: "push:\n    paths: ['!src/**']", want: "must follow a positive pattern"},
			{name: "mixed branch filters", trigger: "push:\n    branches: [main]\n    branches-ignore: [release]", want: "include and ignore filters cannot be combined"},
			{name: "pull request tag filter", trigger: "pull_request:\n    tags: [v1]", want: "pull_request tag filters are unsupported"},
			{name: "pull request activity", trigger: "pull_request:\n    types: [auto_merge_enabled, submitted]", want: `activity type "submitted" cannot be mapped exactly`},
			{name: "bare release", trigger: "release", want: "on: release needs a types list"},
			{name: "unsupported release activity", trigger: "release:\n    types: [edited]", want: `release activity type "edited" cannot be mapped exactly`},
			{name: "release branch filter", trigger: "release:\n    types: [published]\n    branches: [main]", want: "release has unsupported filters"},
			{name: "unknown issues activity", trigger: "issues:\n    types: [not-real]", want: `issues activity type "not-real" cannot be mapped exactly`},
			{name: "issues branch filter", trigger: "issues:\n    branches: [main]", want: "issues has unsupported filters"},
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
					pipelineFailed = pipelineFailed || stage.ID == compiler.StagePipeline && stage.Result == compatibility.Failed
				}
				var failures []compatibility.Diagnostic
				for _, diagnostic := range report.Diagnostics {
					if diagnostic.Level == "error" {
						failures = append(failures, diagnostic)
					}
				}
				if report.Result != "incompatible" || !pipelineFailed || len(failures) != 1 || failures[0].Stage != compiler.StagePipeline || !strings.Contains(failures[0].Message+failures[0].Detail, test.want) {
					t.Fatalf("report = %#v, want trigger failure containing %q", report, test.want)
				}
			})
		}
	})

	t.Run("validate rejects empty issues activities", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "issues.yml")
		if err := os.WriteFile(workflow, []byte("on:\n  issues:\n    types: []\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
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
		if report.Result != "incompatible" || len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, `"types" section should not be empty`) {
			t.Fatalf("report = %#v", report)
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
		if err := os.WriteFile(workflow, []byte("on:\n  push:\n  pull_request:\n  merge_group:\n  release:\n    types: [published, created, released]\n  issues:\n    types: [opened, field_added, field_removed, typed]\n  workflow_dispatch:\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, event := range []string{"push", "pull_request", "merge_group", "release", "issues", "workflow_dispatch", "schedule"} {
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
		if err := os.WriteFile(workflow, []byte("on:\n  push:\n  pull_request:\n  merge_group:\n  release:\n    types: [published, created, released]\n  issues:\n  workflow_dispatch:\n  schedule:\n    - cron: '0 0 * * *'\n  workflow_call:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
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
		if report.Schema != compatibility.ProcessingSchemaV3 || report.Result != "admitted" || report.Status != compatibility.Passed || report.Validation.Result != "compilable" || len(report.Evaluations) != 7 {
			t.Fatalf("aggregate report = %#v", report)
		}
		for i, event := range []string{"push", "pull_request", "merge_group", "release", "issues", "workflow_dispatch", "schedule"} {
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

	t.Run("validate hosted profile skips a workflow without the selected event", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "issues.yml")
		if err := os.WriteFile(workflow, []byte("on: issues\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
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
		if report.Result != "not-applicable" || report.Compile.Result != compatibility.NotEvaluated || report.Admission.Result != compatibility.NotEvaluated {
			t.Fatalf("profile trigger report = %#v", report)
		}
	})

	t.Run("validate hosted profile ignores unsupported trigger events beside a supported one", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "mixed.yml")
		if err := os.WriteFile(workflow, []byte("on: [push, issue_comment, pull_request_target]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
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
		if report.Result != "admitted" {
			t.Fatalf("mixed trigger report = %#v", report)
		}
		messages := map[string]string{}
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.Level == "error" {
				t.Fatalf("unexpected error diagnostic %#v", diagnostic)
			}
			if diagnostic.Code == "W_TRIGGER_EVENT_UNSUPPORTED" {
				message := annotationDiagnosticMessage(diagnostic)
				event := strings.TrimPrefix(strings.Fields(message)[0], "on.")
				messages[event] = message
			}
		}
		for _, event := range []string{"issue_comment", "pull_request_target"} {
			want := "on." + event + " is ignored, so nothing in this workflow runs from it. The supported triggers declared in this workflow still run: push. Move the jobs this trigger guards to one of those triggers if you need them. If you need " + event + ", log an issue on https://github.com/buildkite/buildkite-gha so we can prioritise it."
			if messages[event] != want {
				t.Fatalf("unsupported-trigger message for %s = %q, want %q; report = %#v", event, messages[event], want, report)
			}
		}
	})

	t.Run("validate hosted profile keeps unsupported-trigger warnings in a not-applicable report", func(t *testing.T) {
		workflow := filepath.Join(t.TempDir(), "cross-event.yml")
		if err := os.WriteFile(workflow, []byte("on: [pull_request, issue_comment]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
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
		if report.Result != "not-applicable" {
			t.Fatalf("cross-event report = %#v", report)
		}
		warned := false
		for _, diagnostic := range report.Diagnostics {
			warned = warned || diagnostic.Code == "W_TRIGGER_EVENT_UNSUPPORTED"
		}
		if !warned {
			t.Fatalf("not-applicable report missing unsupported-trigger warning: %#v", report)
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
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), fmt.Appendf(nil, `on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/%s
`, next), 0o600); err != nil {
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
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), fmt.Appendf(nil, `on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/%s
`, next), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("depth-limited reusable discovery stops before event metadata", func(t *testing.T) {
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
	t.Run("incomplete reusable discovery stops before event metadata", func(t *testing.T) {
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
	stageResults := map[compiler.ProcessingStage]string{}
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
			results := map[compiler.ProcessingStage]string{}
			for _, stage := range report.Stages {
				results[stage.ID] = stage.Result
			}
			if results[stageMatrix] != compatibility.Passed || results[stageExpressions] != compatibility.Failed || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != compiler.CodeExpressionInvalid {
				t.Fatalf("report = %#v", report)
			}
		})
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
		stages := map[compiler.ProcessingStage]string{}
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
	stages := map[compiler.ProcessingStage]string{}
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
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Job != "delegated" || report.Diagnostics[0].Stage != stageGraph {
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
	wantMessage := "cancel-in-progress is ignored, so superseded builds keep running. Buildkite handles this as a pipeline setting rather than in the workflow file. Turn on Cancel Intermediate Builds under Settings > Builds. It cancels earlier running builds on the same branch, rather than per concurrency group."
	assertWarning := func(t *testing.T, command, output string) {
		t.Helper()
		for _, want := range []string{
			"buildkite-gha: " + command + ": warning:",
			workflowPath + ":4:23:",
			"[W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED]",
			wantMessage,
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
			wantMessage,
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
		if len(report.Diagnostics) != 1 || report.Diagnostics[0].Level != "warning" || report.Diagnostics[0].Code != "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED" || report.Diagnostics[0].Message != workflowPath+":4:23: "+wantMessage {
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
		if len(report.Diagnostics) != 1 || report.Diagnostics[0].Level != "warning" || report.Diagnostics[0].Code != "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED" || report.Diagnostics[0].Message != workflowPath+":4:23: "+wantMessage {
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
		publish func(context.Context, processingOutput)
	}{
		{
			name: "processing",
			publish: func(ctx context.Context, out processingOutput) {
				report := compatibility.NewProcessingReport("ci.yml", "")
				report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{Level: "warning", Message: "test warning"})
				out.annotate(ctx, report)
			},
		},
		{
			name: "skipped workflow",
			publish: func(ctx context.Context, out processingOutput) {
				out.annotateSkippedWorkflows(ctx, "push", "", true, []skippedWorkflow{{label: "CI", key: "ci", reason: "not applicable"}})
			},
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			active, cancel := context.WithCancel(t.Context())
			cancel()
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_BUILD_URL", "https://buildkite.com/acme/widgets/builds/42")
			runner := &annotationContextRunner{}
			out := newProcessingOutput(active, "test", "text", io.Discard, io.Discard, transport.Agent{Runner: runner})
			path.publish(active, out)
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

func TestProcessingAnnotationPreservesLongerRepositoryURLs(t *testing.T) {
	const docsURL = "https://github.com/buildkite/buildkite-gha/blob/main/docs/compatibility.md#cache-action"
	diagnostic := compatibility.Diagnostic{
		Level: "error", Code: "E_TEST", Message: "Unsupported action. Use a supported version from " + docsURL + ".",
	}

	body := renderProcessingDiagnostic(diagnostic, sourceLinkContext{})
	if !strings.Contains(body, docsURL) || strings.Contains(body, `href="https://github.com/buildkite/buildkite-gha"`) {
		t.Fatalf("annotation = %q, want intact documentation URL", body)
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

func TestProcessingAnnotationRendersJobPermissionWarningGuidance(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	report := compatibility.NewProcessingReport(workflowPath, "hosted")
	report.ApplyWarnings(report.Workflow, []compiler.Warning{{
		Code: "W_JOB_GITHUB_TOKEN_USES_WORKFLOW_PERMISSIONS", Path: report.Workflow, Line: 5, Column: 3, Job: "build",
		Message: "Job-level permissions are ignored for GITHUB_TOKEN. The top-level workflow permissions apply instead. This job's token has contents: read. Move this job's permissions block to the workflow top level. If you need per-job permissions, log an issue on https://github.com/buildkite/buildkite-gha so we can prioritise it.",
	}})
	_, body := processingAnnotation(report, sourceLinkContext{})
	want := `<h2 class="h4 mb2">GitHub Actions workflow diagnostics</h2>
<p><code>.github/workflows/ci.yml</code></p>
<p><strong>Job-level permissions are ignored for GITHUB_TOKEN.</strong></p>
<p><code>.github/workflows/ci.yml:5:3</code> · Job <code>build</code></p>
<p>The top-level workflow permissions apply instead.</p>
<p>This job&#39;s token has contents: read.</p>
<p>Move this job&#39;s permissions block to the workflow top level.</p>
<p>If you need per-job permissions, log an issue on <a href="https://github.com/buildkite/buildkite-gha" target="_blank">buildkite/buildkite-gha</a> so we can prioritise it.</p>
`
	if body != want {
		t.Fatalf("annotation = %q, want %q", body, want)
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
		Level: "error", Code: "E_ACTION_UNSUPPORTED", Stage: compiler.StageResolution,
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
				Message: `Job "test" needs GITHUB_TOKEN, but its workflow path is unsupported.`},
			want: `Job "test" needs GITHUB_TOKEN, but its workflow path is unsupported.`,
		},
		{
			name: "Docker provenance",
			diagnostic: compatibility.Diagnostic{Code: "E_PROFILE",
				Message: `Job "test" requires Docker without matching compiler provenance. Hosted runs support only verified Docker actions and bounded job or service containers.`},
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
	const message = `Job "test" needs GITHUB_TOKEN, but its workflow path is unsupported. Move the workflow under .github/workflows.`
	const detail = `Effective permissions: contents: read.`
	report := compatibility.NewProcessingReport("ci.yml", "hosted")
	for _, instance := range []string{"gha-test-a", "gha-test-b"} {
		report.Diagnostics = append(report.Diagnostics, compatibility.Diagnostic{
			Level: "error", Code: "E_PROFILE", Stage: compiler.StageAdmission,
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
		strings.Count(annotation, `Job &#34;test&#34; needs GITHUB_TOKEN, but its workflow path is unsupported.`) != 1 ||
		strings.Count(annotation, `Move the workflow under .github/workflows.`) != 1 ||
		strings.Count(annotation, `<summary>Diagnostic detail</summary>`) != 1 || strings.Count(annotation, detail) != 1 {
		t.Fatalf("text = %q; annotation = %q", textOutput.String(), annotation)
	}
	if strings.Contains(annotation, "gha-test-") {
		t.Fatalf("annotation exposes matrix instance IDs: %q", annotation)
	}
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
		wantLabel := ":github: workflow · " + filepath.ToSlash(filepath.Clean(workflowPath))
		message := failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "messages")
		if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != "" || len(pipeline.Steps[0].Steps) != 0 || pipeline.Steps[0].Label != wantLabel || !isGeneratedFailureCommand(pipeline.Steps[0].Command) || !strings.Contains(string(message), want) {
			t.Fatalf("unsupported condition pipeline = %#v", pipeline.Steps)
		}
	})
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
	if _, _, _, _, _, _, err := validateArgs([]string{"--profile", "hosted", "--event", "issue_comment", "workflow.yml"}); err == nil || !strings.Contains(err.Error(), "supported events") {
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
