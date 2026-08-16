//go:build linux || darwin

package source

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func lockMutableRefCache(ctx context.Context, path string) (func(), error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
				_ = lock.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = lock.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
