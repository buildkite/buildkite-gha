//go:build !linux && !darwin

package runtime

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func configureProcessGroup(_ *exec.Cmd) {}

func terminateProcessGroup(ctx context.Context, pid int, _ time.Duration, finished <-chan struct{}) {
	select {
	case <-ctx.Done():
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	case <-finished:
	}
}
