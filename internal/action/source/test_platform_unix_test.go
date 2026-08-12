//go:build linux || darwin || freebsd

package source

import "syscall"

func testUmask(mask int) (int, error) {
	return syscall.Umask(mask), nil
}
