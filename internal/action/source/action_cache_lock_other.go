//go:build !unix

package source

import (
	"context"
	"errors"
	"os"
	"sync"
)

type actionCacheLockMode int

const (
	actionCacheLockShared actionCacheLockMode = iota
	actionCacheLockExclusive
)

var (
	errActionCacheLockUnavailable = errors.New("action cache lock is held")
	actionCacheLocksMu            sync.Mutex
	actionCacheLocks              = map[string]*sync.RWMutex{}
)

type actionCacheLock struct {
	mu        *sync.RWMutex
	exclusive bool
	file      *os.File
}

func lockActionCache(ctx context.Context, path string, mode actionCacheLockMode, nonblocking bool) (*actionCacheLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	actionCacheLocksMu.Lock()
	mu := actionCacheLocks[path]
	if mu == nil {
		mu = &sync.RWMutex{}
		actionCacheLocks[path] = mu
	}
	actionCacheLocksMu.Unlock()
	if nonblocking {
		var ok bool
		if mode == actionCacheLockExclusive {
			ok = mu.TryLock()
		} else {
			ok = mu.TryRLock()
		}
		if !ok {
			return nil, errActionCacheLockUnavailable
		}
	} else if mode == actionCacheLockExclusive {
		mu.Lock()
	} else {
		mu.RLock()
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if mode == actionCacheLockExclusive {
			mu.Unlock()
		} else {
			mu.RUnlock()
		}
		return nil, err
	}
	return &actionCacheLock{mu: mu, exclusive: mode == actionCacheLockExclusive, file: file}, nil
}

func (l *actionCacheLock) unlock() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
	if l.exclusive {
		l.mu.Unlock()
	} else {
		l.mu.RUnlock()
	}
}
