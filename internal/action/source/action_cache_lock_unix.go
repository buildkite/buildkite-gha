//go:build unix

package source

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

func lockActionCache(ctx context.Context, path string, mode actionCacheLockMode, nonblocking bool) (*actionCacheLock, error) {
	operation := unix.LOCK_SH
	if mode == actionCacheLockExclusive {
		operation = unix.LOCK_EX
	}
	var unavailable error
	if nonblocking {
		unavailable = errActionCacheLockUnavailable
	}
	file, err := flockLockFile(ctx, path, operation, 10*time.Millisecond, unavailable)
	if err != nil {
		return nil, err
	}
	return &actionCacheLock{file: file}, nil
}
