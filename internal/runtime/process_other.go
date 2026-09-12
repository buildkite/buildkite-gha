//go:build !linux && !darwin && !windows

package runtime

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func configureProcessGroup(_ *exec.Cmd) {}
func processStarted(_ *exec.Cmd) error  { return nil }
func processFinished(_ int)             {}

func terminateProcessGroup(ctx context.Context, pid int, _, _ time.Duration, finished <-chan struct{}) {
	select {
	case <-ctx.Done():
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	case <-finished:
	}
}

func terminateProcessGroupNow(pid int, _, _ time.Duration) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}
