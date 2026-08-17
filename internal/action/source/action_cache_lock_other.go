//go:build !unix

package source

import (
	"context"
	"sync"
)

var (
	actionCacheLocksMu sync.Mutex
	actionCacheLocks   = map[string]*sync.RWMutex{}
)

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
	file, err := openLockFile(path)
	if err != nil {
		if mode == actionCacheLockExclusive {
			mu.Unlock()
		} else {
			mu.RUnlock()
		}
		return nil, err
	}
	release := mu.RUnlock
	if mode == actionCacheLockExclusive {
		release = mu.Unlock
	}
	return &actionCacheLock{file: file, release: release}, nil
}
