package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
)

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
