//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestContainerProcessHelperStrictArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown", "pid"}, {"run", "pid"}, {"terminate", "pid", "bad", "1s"}, {"terminate", "pid", "1s"}} {
		if got := RunContainerProcessHelper(args); got != 2 {
			t.Fatalf("RunContainerProcessHelper(%q) = %d, want 2", args, got)
		}
	}
}

func TestContainerProcessHelperRunRecordsChildAndReturnsStatus(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	if got := RunContainerProcessHelper([]string{"run", pidfile, "sh", "-c", "exit 23"}); got != 23 {
		t.Fatalf("status = %d, want 23", got)
	}
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(stringTrimSpace(data))
	if err != nil || pid <= 0 {
		t.Fatalf("PID file = %q, err = %v", data, err)
	}
}

func TestContainerProcessHelperTerminatesProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	done := make(chan int, 1)
	go func() {
		done <- RunContainerProcessHelper([]string{"run", pidfile, "sh", "-c", "trap '' INT TERM; sleep 30 & wait"})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(pidfile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PID file was not created")
		}
		time.Sleep(time.Millisecond)
	}
	if got := RunContainerProcessHelper([]string{"terminate", pidfile, "10ms", "10ms"}); got != 0 {
		t.Fatalf("terminate status = %d", got)
	}
	select {
	case status := <-done:
		if status != 128+int(syscall.SIGINT) && status != 128+int(syscall.SIGTERM) && status != 128+int(syscall.SIGKILL) {
			t.Fatalf("run status = %d, want terminating signal status", status)
		}
	case <-time.After(time.Second):
		t.Fatal("helper did not terminate process group")
	}
}

func TestContainerProcessHelperTerminateToleratesMissingPIDFile(t *testing.T) {
	t.Parallel()

	start := time.Now()
	if got := RunContainerProcessHelper([]string{"terminate", filepath.Join(t.TempDir(), "missing"), "1ms", "1ms"}); got != 0 {
		t.Fatalf("status = %d", got)
	}
	if elapsed := time.Since(start); elapsed < 450*time.Millisecond || elapsed > time.Second {
		t.Fatalf("race wait = %v, want approximately 500ms", elapsed)
	}
}

func TestContainerProcessHelperCancelBeforePIDPreventsLateCommandStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidfile := filepath.Join(dir, "late.pid")
	started := filepath.Join(dir, "started")
	if got := RunContainerProcessHelper([]string{"terminate", pidfile, "0", "0"}); got != 0 {
		t.Fatalf("terminate status = %d", got)
	}
	if got := RunContainerProcessHelper([]string{"run", pidfile, "sh", "-c", "touch \"$1\"", "sh", started}); got != 130 {
		t.Fatalf("late run status = %d", got)
	}
	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Fatalf("late command started: %v", err)
	}
}
