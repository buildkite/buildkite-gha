//go:build unix

package source

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func flockLockFile(ctx context.Context, path string, operation int, pollInterval time.Duration, unavailable error) (*lockFile, error) {
	lock, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lock.file.Fd()), operation|unix.LOCK_NB)
		if err == nil {
			lock.release = func(file *os.File) {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			}
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			lock.unlock()
			return nil, err
		}
		if unavailable != nil {
			lock.unlock()
			return nil, unavailable
		}
		select {
		case <-ctx.Done():
			lock.unlock()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
