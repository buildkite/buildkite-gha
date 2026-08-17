//go:build !linux && !darwin

package source

import (
	"context"
)

func lockMutableRefCache(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	return lock.unlock, nil
}
