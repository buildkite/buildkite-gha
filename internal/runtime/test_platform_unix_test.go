//go:build linux || darwin

package runtime

import (
	"errors"
	"syscall"
)

func testProcessExists(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func testMkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
