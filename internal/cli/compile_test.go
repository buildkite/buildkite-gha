package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
