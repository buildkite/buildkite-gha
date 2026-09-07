package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestCancellationTerminatesChildProcessGroup(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := (Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}).runStreaming(ctx, newCommandOutputProcessor(io.Discard, io.Discard), "", map[string]string{"PID_FILE": pidFile}, "sh", "-c", `(trap '' INT TERM; sleep 30) & echo $! > "$PID_FILE"; wait`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStreaming() error = %v, want deadline", err)
	}
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !testProcessExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived cancellation", pid)
}

func TestCancellationEscalatesFromInterruptToTermination(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	signals := filepath.Join(dir, "signals")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelled := make(chan struct{})
	go func() {
		defer close(cancelled)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runner := Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 500 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandOutputProcessor(io.Discard, io.Discard), "", map[string]string{"READY": ready, "SIGNALS": signals}, "bash", "-c", `
trap 'printf "INT\n" >> "$SIGNALS"' INT
trap 'printf "TERM\n" >> "$SIGNALS"; exit 0' TERM
touch "$READY"
while :; do sleep 1; done`)
	<-cancelled
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	contents, readErr := os.ReadFile(signals)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := string(contents); got != "INT\nTERM\n" {
		t.Fatalf("signal order = %q, want SIGINT then SIGTERM", got)
	}
}

func TestCancellationPreservesInterruptGraceForDescendants(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	childReady := filepath.Join(dir, "child-ready")
	cleaned := filepath.Join(dir, "cleaned")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runner := Runner{InterruptGrace: 2 * time.Second, TerminateGrace: 50 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandOutputProcessor(io.Discard, io.Discard), "", map[string]string{"READY": ready, "CHILD_READY": childReady, "CLEANED": cleaned}, "bash", "-c", `
(
  trap 'sleep 0.3; touch "$CLEANED"; exit 0' INT
  touch "$CHILD_READY"
  while :; do sleep 1; done
) &
trap 'exit 0' INT
while [ ! -f "$CHILD_READY" ]; do sleep 0.01; done
touch "$READY"
wait`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	if _, statErr := os.Stat(cleaned); statErr != nil {
		t.Fatalf("descendant did not finish SIGINT cleanup during the interrupt grace: %v", statErr)
	}
}

func TestCancellationWaitsForProcessGroupCleanupAfterOutputCloses(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	started := time.Now()
	runner := Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandOutputProcessor(io.Discard, io.Discard), "", map[string]string{"PID_FILE": pidFile, "READY": ready}, "bash", "-c", `
(trap '' INT TERM; exec >/dev/null 2>&1; while :; do sleep 1; done) &
echo $! > "$PID_FILE"
trap 'exit 0' INT
touch "$READY"
while :; do sleep 1; done`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("runStreaming() returned before process-group escalation completed: %s", elapsed)
	}
	contents, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !testProcessExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived completed cancellation", pid)
}

func TestExplicitCancelTerminatesBackgroundProcessGroup(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	pidFile := filepath.Join(workspace, "child.pid")
	ready := filepath.Join(workspace, "background.ready")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `(trap '' INT TERM; sleep 30) & echo $! > "$PID_FILE"; touch "$READY"; wait`},
		{ID: "await-start", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	})
	job.Env = map[string]string{"PID_FILE": pidFile, "READY": ready}

	result, err := (Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !testProcessExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("explicitly canceled child process %d survived", pid)
}

func TestRunStreamingDrainsOversizedLineAndPreservesMasking(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	processor := newCommandOutputProcessor(&logs, &logs)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = (Runner{}).runStreaming(ctx, processor, "", map[string]string{"GO_WANT_RUNTIME_LONG_LINE": "1"}, executable, "-test.run=^TestLongLineChildProcess$")
	if err == nil || !strings.Contains(err.Error(), "stdout stream: line exceeds 1048576-byte limit and was discarded") {
		t.Fatalf("runStreaming() error = %v, want oversized-line diagnostic", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStreaming() deadlocked: %v", err)
	}
	if strings.Contains(logs.String(), "runtime-stream-secret") {
		t.Fatalf("runStreaming() leaked masked content: %q", logs.String())
	}
	if strings.Contains(logs.String(), "after long line") {
		t.Fatalf("runStreaming() forwarded output after masking became uncertain: %q", logs.String())
	}
}

func TestStreamLineLimitIncludesContentNotNewline(t *testing.T) {
	want := strings.Repeat("x", maxStreamLineBytes)
	for _, ending := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("ending-%q", ending), func(t *testing.T) {
			var lines []string
			suppressed := false
			err := streamLines(strings.NewReader(want+ending+"next"+ending), func(line string) {
				lines = append(lines, line)
			}, func() {
				suppressed = true
			})
			if err != nil || suppressed {
				t.Fatalf("streamLines() = %v, suppressed = %v", err, suppressed)
			}
			if len(lines) != 2 || lines[0] != want || lines[1] != "next" {
				t.Fatalf("streamLines() returned %#v", lines)
			}
		})
	}
}

func TestLongLineChildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIME_LONG_LINE") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "::add-mask::runtime-stream-secret")
	_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", maxStreamLineBytes+1)+"runtime-stream-secret")
	_, _ = fmt.Fprintln(os.Stdout, "after long line: runtime-stream-secret")
}

func TestProcessEnvironmentIsExplicitAndUsable(t *testing.T) {
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "must-not-leak")
	for _, name := range agentProxyEnvironmentNames {
		t.Setenv(name, "http://must-not-leak.invalid")
	}
	var logs bytes.Buffer
	processor := newCommandOutputProcessor(&logs, &logs)
	command := `
test -n "$PATH" && test -n "$HOME" && test -n "$TMPDIR"
test "$DECLARED" = visible
test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}"
test -z "${HTTP_PROXY+x}" && test -z "${HTTPS_PROXY+x}" && test -z "${ALL_PROXY+x}" && test -z "${NO_PROXY+x}"
test -z "${http_proxy+x}" && test -z "${https_proxy+x}" && test -z "${all_proxy+x}" && test -z "${no_proxy+x}"
printf '%s\n' environment-ok
`
	if err := (Runner{}).runStreaming(t.Context(), processor, "", map[string]string{"DECLARED": "visible"}, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}
	if logs.String() != "environment-ok\n" {
		t.Fatalf("runStreaming() logs = %q", logs.String())
	}
	for _, entry := range processEnv(nil) {
		if strings.HasPrefix(entry, "BUILDKITE_") {
			t.Fatalf("processEnv() inherited agent variable %q", entry)
		}
	}
}

func TestRunStreamingRejectsInvalidEnvironmentNamesBeforeExecution(t *testing.T) {
	for _, name := range []string{"", "ALIAS=OTHER", "NUL\x00NAME"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			err := (Runner{}).runStreaming(t.Context(), newCommandOutputProcessor(io.Discard, io.Discard), "", map[string]string{name: "value"}, "sh", "-c", `touch "$1"`, "sh", marker)
			if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
				t.Fatalf("runStreaming() error = %v, want invalid environment name", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("host process ran with invalid environment name: %v", statErr)
			}
		})
	}

	if err := (Runner{}).runStreaming(t.Context(), newCommandOutputProcessor(io.Discard, io.Discard), "", map[string]string{"GITHUB-ACTIONS_NAME.1": "valid"}, "true"); err != nil {
		t.Fatalf("valid GitHub Actions environment name was rejected: %v", err)
	}
}
