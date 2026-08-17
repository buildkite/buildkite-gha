package source

import (
	"errors"
	"sync"
)

type actionCacheLockMode int

const (
	actionCacheLockShared actionCacheLockMode = iota
	actionCacheLockExclusive
)

var errActionCacheLockUnavailable = errors.New("action cache lock is held")

type actionCacheLock struct {
	file    *lockFile
	release func()
	once    sync.Once
}

func (l *actionCacheLock) unlock() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.file.unlock()
		if l.release != nil {
			l.release()
		}
	})
}
