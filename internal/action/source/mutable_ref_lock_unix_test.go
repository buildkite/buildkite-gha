//go:build linux || darwin

package source

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestMutableRefLockCancellationWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	unlock, err := lockMutableRefCache(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := lockMutableRefCache(ctx, path); !errors.Is(err, context.Canceled) {
		unlock()
		t.Fatalf("waiting lock error = %v, want %v", err, context.Canceled)
	}
	unlock()
	unlock()
	release, err := lockMutableRefCache(t.Context(), path)
	if err != nil {
		t.Fatalf("reacquire lock: %v", err)
	}
	release()
}
