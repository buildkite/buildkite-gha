package source

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLockFileUnlockIsIdempotent(t *testing.T) {
	lock, err := openLockFile(filepath.Join(t.TempDir(), ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	var releases atomic.Int32
	lock.release = func(*os.File) { releases.Add(1) }
	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			lock.unlock()
		}()
	}
	callers.Wait()
	if releases.Load() != 1 {
		t.Fatalf("lock release calls = %d, want 1", releases.Load())
	}
	if lock.file != nil {
		t.Fatal("lock file remains owned after unlock")
	}
	lock.unlock()
}

func TestActionCacheLockSharedExclusiveNonblocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	first, err := lockActionCache(t.Context(), path, actionCacheLockShared, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := lockActionCache(t.Context(), path, actionCacheLockShared, true)
	if err != nil {
		first.unlock()
		t.Fatal(err)
	}
	if _, err := lockActionCache(t.Context(), path, actionCacheLockExclusive, true); err != errActionCacheLockUnavailable {
		first.unlock()
		second.unlock()
		t.Fatalf("exclusive lock error = %v, want %v", err, errActionCacheLockUnavailable)
	}
	first.unlock()
	first.unlock()
	if _, err := lockActionCache(t.Context(), path, actionCacheLockExclusive, true); err != errActionCacheLockUnavailable {
		second.unlock()
		t.Fatalf("exclusive lock with one shared holder error = %v, want %v", err, errActionCacheLockUnavailable)
	}
	second.unlock()
	second.unlock()
	exclusive, err := lockActionCache(t.Context(), path, actionCacheLockExclusive, true)
	if err != nil {
		t.Fatal(err)
	}
	exclusive.unlock()
	exclusive.unlock()
}
