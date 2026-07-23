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

func terminateProcessGroup(ctx context.Context, pid int, interruptGrace, terminateGrace time.Duration, finished <-chan struct{}) {
	select {
	case <-ctx.Done():
		_ = syscall.Kill(-pid, syscall.SIGINT)
	case <-finished:
		return
	}
	if waitForProcessGroupExit(pid, interruptGrace) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitForProcessGroupExit(pid, terminateGrace) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func waitForProcessGroupExit(pid int, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return false
		case <-ticker.C:
			if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
				return true
			}
		}
	}
}
