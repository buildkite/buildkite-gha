package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
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

func TestRunCommandsAreNotImplemented(t *testing.T) {
	for _, command := range []string{"upload"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run([]string{command}, &stdout, &stderr, "dev"); code != 1 {
				t.Fatalf("Run() code = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("Run() stdout = %q, want empty", stdout.String())
			}
			want := "buildkite-gha: " + command + ": not implemented\n"
			if stderr.String() != want {
				t.Errorf("Run() stderr = %q, want %q", stderr.String(), want)
			}
		})
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
		if want := "2 logical jobs, 3 instances"; !strings.Contains(stdout.String(), want) {
			t.Fatalf("Run() stdout = %q, want %q", stdout.String(), want)
		}
		if !strings.Contains(stdout.String(), "run-job validates the Phase 0 execution subset") {
			t.Fatalf("Run() stdout = %q, want explicit runtime boundary", stdout.String())
		}
	})

	t.Run("compile", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"compile", workflowPath, "--event-path", eventPath}
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
			t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
		}
		var ir compiler.IR
		if err := json.Unmarshal(stdout.Bytes(), &ir); err != nil {
			t.Fatalf("compile output is not JSON: %v", err)
		}
		if len(ir.Jobs) != 3 {
			t.Fatalf("compiled jobs = %d, want 3", len(ir.Jobs))
		}
		if !ir.Execution.Supported || ir.Execution.Reason == "" {
			t.Fatalf("compile output execution boundary = %#v, want supported Phase 0 boundary", ir.Execution)
		}
	})
}

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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"result": "cli-ok"`) {
		t.Fatalf("result = %s", result)
	}
	job.Compiler.Version = "0.0.0-other"
	encoded, err = plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev"); code != 1 || !strings.Contains(stderr.String(), "does not match runtime version") {
		t.Fatalf("Run() code = %d, stderr = %q, want version mismatch", code, stderr.String())
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
}
