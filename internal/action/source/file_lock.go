package source

import (
	"os"
	"sync"
)

type lockFile struct {
	file    *os.File
	release func(*os.File)
	once    sync.Once
}

func openLockFile(path string) (*lockFile, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &lockFile{file: file}, nil
}

func (l *lockFile) unlock() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.release != nil {
			l.release(l.file)
		}
		_ = l.file.Close()
		l.file = nil
	})
}
