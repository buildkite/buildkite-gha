package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActionResolutionSnapshotContextCancelsInitializationWhileLocked(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockMutableRefCache(t.Context(), filepath.Join(root, ".snapshot.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewResolver(nil, WithActionResolutionSnapshotContext(ctx, root, false))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("initialization error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		unlock()
		<-done
		t.Fatal("snapshot initialization did not honor cancellation while locked")
	}
	if _, err := os.Stat(filepath.Join(root, "current.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled initialization published a generation: %v", err)
	}
	unlock()
	resolver, err := NewResolver(nil, WithActionResolutionSnapshotContext(t.Context(), root, false))
	if err != nil || resolver.ResolutionSnapshotID() == "" {
		t.Fatalf("retry after cancellation = %v, %v", resolver, err)
	}
}
