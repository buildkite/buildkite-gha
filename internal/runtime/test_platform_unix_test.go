//go:build linux || darwin

package runtime

import (
	"errors"
	"os"
	"syscall"
)

func testFileLock(file *os.File, lock bool) error {
	operation := syscall.LOCK_UN
	if lock {
		operation = syscall.LOCK_EX
	}
	return syscall.Flock(int(file.Fd()), operation)
}

func testExec(path string, args, env []string) error {
	return syscall.Exec(path, args, env)
}

func testProcessExists(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func testMkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
