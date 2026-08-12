//go:build linux || darwin

package runtime

import "syscall"

const nonBlockingOpenFlag = syscall.O_NONBLOCK
