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

func terminateProcessGroup(ctx context.Context, pid int, interruptGrace, terminateGrace time.Duration, processDone, finished <-chan struct{}) {
	select {
	case <-ctx.Done():
	case <-finished:
		return
	}
	select {
	case <-processDone:
		terminateRemainingProcessGroup(pid, terminateGrace)
		return
	default:
	}
	// Signal children before the shell so its foreground command is interrupted
	// before the shell dispatches its own trap. A single group signal leaves a
	// fork/exit race where the shell can terminate without running that trap.
	signalProcessGroup(pid, syscall.SIGINT)
	if waitForProcessExit(processDone, interruptGrace) {
		terminateRemainingProcessGroup(pid, terminateGrace)
		return
	}
	signalProcessGroup(pid, syscall.SIGTERM)
	if waitForProcessExit(processDone, terminateGrace) {
		terminateRemainingProcessGroup(pid, terminateGrace)
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func signalProcessGroup(pid int, signal syscall.Signal) {
	// Freeze the shell while signaling its children so it cannot race from a
	// completed foreground command into the next command. The shell receives
	// its signal while stopped and dispatches the pending trap when continued.
	if signalProcessGroupMember(pid, pid, syscall.SIGSTOP) {
		// Always resume the leader: catchable and default-fatal signals may
		// remain pending while it is stopped.
		defer signalProcessGroupMember(pid, pid, syscall.SIGCONT)
		waitForProcessStopped(pid)
	}
	signalProcessGroupChildren(pid, signal)
	signalProcessGroupMember(pid, pid, signal)
}

func signalProcessGroupMember(pid, group int, signal syscall.Signal) bool {
	if processGroup, err := syscall.Getpgid(pid); err == nil && processGroup == group {
		return syscall.Kill(pid, signal) == nil
	}
	return false
}

func waitForProcessExit(processDone <-chan struct{}, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-processDone:
		return true
	case <-timer.C:
		return false
	}
}

func terminateRemainingProcessGroup(pid int, terminateGrace time.Duration) {
	if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
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
