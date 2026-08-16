//go:build unix

package source

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type actionCacheLockMode int

const (
	actionCacheLockShared actionCacheLockMode = iota
	actionCacheLockExclusive
)

var errActionCacheLockUnavailable = errors.New("action cache lock is held")

type actionCacheLock struct{ file *os.File }

func lockActionCache(ctx context.Context, path string, mode actionCacheLockMode, nonblocking bool) (*actionCacheLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	operation := unix.LOCK_SH
	if mode == actionCacheLockExclusive {
		operation = unix.LOCK_EX
	}
	for {
		err = unix.Flock(int(file.Fd()), operation|unix.LOCK_NB)
		if err == nil {
			return &actionCacheLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		if nonblocking {
			_ = file.Close()
			return nil, errActionCacheLockUnavailable
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (l *actionCacheLock) shared() error {
	return unix.Flock(int(l.file.Fd()), unix.LOCK_SH)
}

func (l *actionCacheLock) unlock() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
