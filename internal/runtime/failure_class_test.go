package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestClassifyFailurePrecedence(t *testing.T) {
	exit := exitError(t)
	stepExit := markStepProcessExit(fmt.Errorf("step %q: %w", "test", exit))
	unsupported := errUnsupportedf("shell %q is unsupported in the supported runtime subset", "pwsh")
	hard := markHardJobFailure(errors.New("owned Docker resources remain after cleanup"))
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nil", nil, FailureClassUnknown},
		{"plain", errors.New("boom"), FailureClassUnknown},
		{"step exit", stepExit, FailureClassStepProcessExit},
		{"wrapped step exit", fmt.Errorf("run-job: %w", stepExit), FailureClassStepProcessExit},
		{"tolerated step exit", &toleratedJobFailure{err: stepExit}, FailureClassStepProcessExit},
		{"unsupported", unsupported, FailureClassUnsupportedFeature},
		{"integrity", hard, FailureClassIntegrity},
		{"integrity outranks step exit", errors.Join(stepExit, hard), FailureClassIntegrity},
		{"unsupported outranks integrity", errors.Join(hard, unsupported), FailureClassUnsupportedFeature},
		{"unsupported outranks step exit", errors.Join(stepExit, unsupported), FailureClassUnsupportedFeature},
	}
	for _, tc := range cases {
		if got := ClassifyFailure(tc.err); got != tc.want {
			t.Errorf("ClassifyFailure(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMarkStepProcessExitRequiresExitError(t *testing.T) {
	if err := markStepProcessExit(nil); err != nil {
		t.Fatalf("markStepProcessExit(nil) = %v", err)
	}
	launch := errors.New("process /bin/missing: executable file not found")
	if got := markStepProcessExit(launch); got != launch {
		t.Fatalf("markStepProcessExit(launch failure) = %v, want unchanged", got)
	}
	exit := fmt.Errorf("process /bin/sh: %w", exitError(t))
	marked := markStepProcessExit(exit)
	if ClassifyFailure(marked) != FailureClassStepProcessExit {
		t.Fatalf("markStepProcessExit(exit) classify = %q", ClassifyFailure(marked))
	}
	if again := markStepProcessExit(marked); again != marked {
		t.Fatalf("markStepProcessExit is not idempotent: %v", again)
	}
	if !errors.As(marked, new(*exec.ExitError)) {
		t.Fatalf("marked error lost the exit error chain: %v", marked)
	}
}

func TestRunJobClassifiesStepProcessExit(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/failing.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: failing\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "fail", Kind: "run", Command: "exit 7"}})
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if got := ClassifyFailure(err); got != FailureClassStepProcessExit {
		t.Fatalf("ClassifyFailure() = %q, want %q for %v", got, FailureClassStepProcessExit, err)
	}
}

func TestRunJobClassifiesUnsupportedShell(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/shell.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: shell\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "shell", Kind: "run", Shell: "pwsh", Command: "Get-Location"}})
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if got := ClassifyFailure(err); got != FailureClassUnsupportedFeature {
		t.Fatalf("ClassifyFailure() = %q, want %q for %v", got, FailureClassUnsupportedFeature, err)
	}
}

func exitError(t *testing.T) *exec.ExitError {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 7").Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("sh exit error = %v", err)
	}
	return exit
}
