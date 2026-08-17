//go:build linux || darwin

package source

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

func lockMutableRefCache(ctx context.Context, path string) (func(), error) {
	lock, err := flockLockFile(ctx, path, unix.LOCK_EX, 25*time.Millisecond, nil)
	if err != nil {
		return nil, err
	}
	return lock.unlock, nil
}
