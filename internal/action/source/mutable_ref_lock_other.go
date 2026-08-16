//go:build !linux && !darwin

package source

import (
	"context"
	"os"
)

func lockMutableRefCache(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}
