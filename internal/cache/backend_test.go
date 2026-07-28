package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
)

func int64Pointer(value int64) *int64 { return &value }

func putMemoryEntry(t *testing.T, backend *memoryBackend, key, version, owner string, contents []byte) Entry {
	t.Helper()
	reservation, err := backend.Reserve(context.Background(), ReserveRequest{
		Key: key, Version: version, Owner: owner,
		DeclaredSize: int64Pointer(int64(len(contents))),
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	blob, err := backend.Upload(context.Background(), reservation.ID, BlobSource{
		Reader: bytes.NewReader(contents), Size: int64(len(contents)),
		SHA256: hex.EncodeToString(sum[:]), Generation: reservation.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := backend.Commit(context.Background(), reservation.ID, blob)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestMemoryBackendLookupOrderAndVersion(t *testing.T) {
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	backend := newTestMemoryBackend(func() time.Time { return now })

	branchPrefix := putMemoryEntry(t, backend, "npm-linux-branch", "v1", "branch-prefix", []byte("branch"))
	now = now.Add(time.Second)
	_ = seedMemoryEntry(backend, "default", "npm-linux", "v1", []byte("default"), now)
	now = now.Add(time.Second)
	_ = putMemoryEntry(t, backend, "restore-exact", "v1", "restore-exact", []byte("restore"))

	entry, ok, err := backend.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"npm-linux", "restore-exact"}, Version: "v1",
	})
	if err != nil || !ok {
		t.Fatalf("Lookup() = %#v, %v, %v", entry, ok, err)
	}
	if entry.ID != branchPrefix.ID {
		t.Fatalf("Lookup() entry = %q, want current-scope primary prefix %q", entry.ID, branchPrefix.ID)
	}

	if _, ok, err := backend.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"npm-linux"}, Version: "different",
	}); err != nil || ok {
		t.Fatalf("version-mismatched Lookup() = %v, %v, want miss", ok, err)
	}

	exactBackend := newTestMemoryBackend(func() time.Time { return now })
	exact := putMemoryEntry(t, exactBackend, "restore-", "v1", "exact", []byte("exact"))
	now = now.Add(time.Second)
	_ = putMemoryEntry(t, exactBackend, "restore-newer", "v1", "prefix", []byte("prefix"))
	entry, ok, err = exactBackend.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"primary-miss", "restore-"}, Version: "v1",
	})
	if err != nil || !ok || entry.ID != exact.ID {
		t.Fatalf("restore exact-before-prefix Lookup() = %#v, %v, %v, want %q", entry, ok, err, exact.ID)
	}

	tieBackend := newTestMemoryBackend(func() time.Time { return now })
	_ = putMemoryEntry(t, tieBackend, "tie-one", "v1", "one", []byte("one"))
	tieWinner := putMemoryEntry(t, tieBackend, "tie-two", "v1", "two", []byte("two"))
	entry, ok, err = tieBackend.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"tie-"}, Version: "v1",
	})
	if err != nil || !ok || entry.ID != tieWinner.ID {
		t.Fatalf("stable ID tie-break Lookup() = %#v, %v, %v, want %q", entry, ok, err, tieWinner.ID)
	}
}

func TestMemoryBackendReservationReplayContentionAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	backend := newTestMemoryBackend(func() time.Time { return now })
	request := ReserveRequest{
		Key: "key", Version: "version",
		Owner: "job-one", DeclaredSize: int64Pointer(3),
	}
	first, err := backend.Reserve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := backend.Reserve(context.Background(), request)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("reserve replay = %#v, %v, want %q", replay, err, first.ID)
	}
	other := request
	other.Owner = "job-two"
	if _, err := backend.Reserve(context.Background(), other); !errors.Is(err, ErrContention) {
		t.Fatalf("other-owner reserve error = %v, want contention", err)
	}

	now = now.Add(backend.lease + time.Second)
	second, err := backend.Reserve(context.Background(), other)
	if err != nil || second.ID == first.ID {
		t.Fatalf("reserve after expiry = %#v, %v", second, err)
	}
	if _, err := backend.Upload(context.Background(), first.ID, BlobSource{Reader: bytes.NewReader([]byte("old")), Size: 3, Generation: first.Generation}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale upload error = %v, want not found", err)
	}
	put := []byte("new")
	sum := sha256.Sum256(put)
	blob, err := backend.Upload(context.Background(), second.ID, BlobSource{Reader: bytes.NewReader(put), Size: 3, SHA256: hex.EncodeToString(sum[:]), Generation: second.Generation})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Commit(context.Background(), second.ID, blob); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Reserve(context.Background(), request); !errors.Is(err, ErrContention) {
		t.Fatalf("reserve committed identity error = %v, want contention", err)
	}
}

func TestMemoryBackendConcurrentReservationHasOneWinner(t *testing.T) {
	backend := newTestMemoryBackend(nil)
	start := make(chan struct{})
	const workers = 32
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := backend.Reserve(context.Background(), ReserveRequest{
				Key: "key", Version: "version",
				Owner: string(rune('a' + worker)), DeclaredSize: int64Pointer(1),
			})
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrContention):
		default:
			t.Fatalf("Reserve() error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("reservation winners = %d, want 1", winners)
	}
}
