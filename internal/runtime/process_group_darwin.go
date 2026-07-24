//go:build darwin

package runtime

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const darwinProcessStateStopped = 4 // SSTOP in Darwin's sys/proc.h.

func waitForProcessStopped(pid int) {
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return
		}
		if process.Proc.P_stat == darwinProcessStateStopped {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func signalProcessGroupChildren(group int, signal syscall.Signal) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", group)
	if err != nil {
		return
	}
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		if pid != group {
			signalProcessGroupMember(pid, group, signal)
		}
	}
}
