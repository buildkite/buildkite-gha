//go:build linux || darwin

package runtime

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(ctx context.Context, pid int, grace time.Duration, finished <-chan struct{}) {
	select {
	case <-ctx.Done():
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	case <-finished:
		return
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return
		case <-ticker.C:
			if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
				return
			}
		}
	}
}
