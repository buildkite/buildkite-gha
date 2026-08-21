//go:build unix

package source

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestActionCacheLockCancellationWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	holder, err := lockActionCache(t.Context(), path, actionCacheLockExclusive, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := lockActionCache(ctx, path, actionCacheLockShared, false); !errors.Is(err, context.Canceled) {
		holder.unlock()
		t.Fatalf("waiting lock error = %v, want %v", err, context.Canceled)
	}
	holder.unlock()
	reacquired, err := lockActionCache(t.Context(), path, actionCacheLockExclusive, true)
	if err != nil {
		t.Fatalf("reacquire lock: %v", err)
	}
	reacquired.unlock()
}
