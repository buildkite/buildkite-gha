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

func TestValidateReportsActionableWorkflowSyntaxDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, source, headline, message, job string
		column                               int
	}{
		{
			name:     "GitHub environment",
			source:   "on: push\njobs:\n  deploy:\n    environment: production\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			headline: "GitHub environments and environment secrets are unsupported.",
			message:  `GitHub environments and environment secrets are unsupported. Remove the environment key from job "deploy". Approvals, deployment records, and protection rules are unavailable. Move environment secrets into Buildkite secrets and reference them by name. If you need GitHub environments, open an issue in https://github.com/buildkite/buildkite-gha so we can prioritize support`,
			job:      "deploy",
			column:   5,
		},
		{
			name:     "job-level write-all",
			source:   "on: push\njobs:\n  publish:\n    permissions: write-all\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			headline: "permissions: write-all is unsupported as job-level shorthand.",
			message:  `permissions: write-all is unsupported as job-level shorthand. In job "publish", you cannot set separate repository permissions. At the workflow top level, declare each needed repository permission, such as contents: write and pull-requests: write. These permissions apply to every job that receives GITHUB_TOKEN. Use permissions: write-all at the workflow top level only when every supported repository permission should have write access. If you need different repository permissions for individual jobs, open an issue in https://github.com/buildkite/buildkite-gha so we can prioritize support`,
			job:      "publish",
			column:   18,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflowPath := filepath.Join(t.TempDir(), "workflow.yml")
			if err := os.WriteFile(workflowPath, []byte(test.source), 0o600); err != nil {
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
			if len(report.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v, want one", report.Diagnostics)
			}
			diagnostic := report.Diagnostics[0]
			if diagnostic.Message != test.message || diagnostic.Job != test.job || diagnostic.Location == nil || diagnostic.Location.Path != workflowPath || diagnostic.Location.Line != 4 || diagnostic.Location.Column != test.column {
				t.Fatalf("diagnostic = %#v, want message %q, job %q at %s:4:%d", diagnostic, test.message, test.job, workflowPath, test.column)
			}
			if headline, _ := annotationDiagnosticPresentation(diagnostic); headline != test.headline {
				t.Fatalf("diagnostic headline = %q, want %q", headline, test.headline)
			}
		})
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
			`<p><strong>Windows runners aren&#39;t currently supported.</strong></p>`,
			`Imported jobs run on Linux or macOS Buildkite hosted agents.`,
			`If this job can run on Linux, change <code>windows-latest</code> to <code>ubuntu-latest</code>.`,
			`If it requires Windows, open an issue in <a href="https://github.com/buildkite/buildkite-gha" target="_blank">buildkite/buildkite-gha</a> to help us prioritize Windows support.`,
			"Job <code>test</code>",
		} {
			if !strings.Contains(string(annotation.stdin), want) {
				t.Fatalf("annotation = %q, want %q", annotation.stdin, want)
			}
		}
		for _, unwanted := range []string{"E_EXPRESSION_INVALID", "stage:", "instance:", "gha-test", "Diagnostic detail", "Supported runner labels:"} {
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
