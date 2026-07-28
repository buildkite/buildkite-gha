package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDirectoryIdentity() (Namespace, []Scope, Scope) {
	return Namespace{Organization: "organization", Cluster: "cluster", Pipeline: "pipeline"},
		[]Scope{"branch", "default"}, "branch"
}

func newTestDirectoryBackend(t *testing.T, root string, now *time.Time) *experimentalDirectoryBackend {
	t.Helper()
	namespace, readScopes, writeScope := testDirectoryIdentity()
	backend, err := NewExperimentalDirectoryBackend(root, namespace, readScopes, writeScope)
	if err != nil {
		t.Fatal(err)
	}
	directory := backend.(*experimentalDirectoryBackend)
	directory.now = func() time.Time { return *now }
	return directory
}

func putDirectoryEntry(t *testing.T, backend *experimentalDirectoryBackend, scope Scope, key, version, owner string, contents []byte) Entry {
	t.Helper()
	prev := backend.writeScope
	backend.writeScope = scope
	defer func() { backend.writeScope = prev }()
	size := int64(len(contents))
	reservation, err := backend.Reserve(context.Background(), ReserveRequest{
		Key: key, Version: version, Owner: owner, DeclaredSize: &size,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	blob, err := backend.Upload(context.Background(), reservation.ID, BlobSource{
		Reader: bytes.NewReader(contents), Size: size,
		SHA256: hex.EncodeToString(digest[:]), Generation: reservation.Generation,
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

func TestExperimentalDirectoryBackendPersistsLookupListAndRanges(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	backend := newTestDirectoryBackend(t, root, &now)

	branchPrefix := putDirectoryEntry(t, backend, "branch", "npm-linux-branch", "v1", "branch-prefix", []byte("branch contents"))
	now = now.Add(time.Second)
	_ = putDirectoryEntry(t, backend, "default", "npm-linux", "v1", "default-exact", []byte("default contents"))
	now = now.Add(time.Second)
	exact := putDirectoryEntry(t, backend, "branch", "restore-", "v1", "restore-exact", []byte("0123456789"))
	now = now.Add(time.Second)
	_ = putDirectoryEntry(t, backend, "branch", "restore-newer", "v1", "restore-prefix", []byte("prefix contents"))

	reopened := newTestDirectoryBackend(t, root, &now)
	entry, ok, err := reopened.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"npm-linux"}, Version: "v1",
	})
	if err != nil || !ok || entry.ID != branchPrefix.ID {
		t.Fatalf("scope-ordered Lookup() = %#v, %v, %v, want %q", entry, ok, err, branchPrefix.ID)
	}
	entry, ok, err = reopened.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"missing", "restore-"}, Version: "v1",
	})
	if err != nil || !ok || entry.ID != exact.ID {
		t.Fatalf("exact-before-prefix Lookup() = %#v, %v, %v, want %q", entry, ok, err, exact.ID)
	}
	if _, ok, err := reopened.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"restore-"}, Version: "other",
	}); err != nil || ok {
		t.Fatalf("version-mismatched Lookup() = %v, %v, want miss", ok, err)
	}

	entries, err := reopened.List(context.Background(), ListRequest{Key: "restore", Limit: 10})
	if err != nil || len(entries) != 2 || entries[0].Key != "restore-newer" || entries[1].ID != exact.ID {
		t.Fatalf("List() = %#v, %v", entries, err)
	}
	entries, err = reopened.List(context.Background(), ListRequest{Key: "restore", Limit: -1})
	if err != nil || len(entries) != 2 {
		t.Fatalf("unlimited List() = %#v, %v", entries, err)
	}

	reader, info, err := reopened.Open(context.Background(), exact.ID, &ByteRange{Start: 2, End: 5})
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(contents) != "2345" || info.Size != 10 {
		t.Fatalf("range Open() = %q, %#v, %v, %v", contents, info, readErr, closeErr)
	}
	if _, _, err := reopened.Open(context.Background(), EntryID("../../outside"), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsafe ID Open() error = %v, want not found", err)
	}
}

func TestExperimentalDirectoryBackendReservationIntegrityAndExpiry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := newTestDirectoryBackend(t, t.TempDir(), &now)
	request := ReserveRequest{
		Key: "key\x00with-separator", Version: "version", Owner: "job-one",
		DeclaredSize: int64Pointer(3),
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
		t.Fatalf("other-owner Reserve() error = %v, want contention", err)
	}
	if _, err := backend.Upload(context.Background(), first.ID, BlobSource{
		Reader: bytes.NewReader([]byte("one")), Size: 3, SHA256: stringsOfByte('0', 64), Generation: "wrong",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-generation Upload() error = %v, want conflict", err)
	}
	if _, err := backend.Upload(context.Background(), first.ID, BlobSource{
		Reader: bytes.NewReader([]byte("one")), Size: 3, SHA256: stringsOfByte('0', 64), Generation: first.Generation,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-digest Upload() error = %v, want conflict", err)
	}

	now = now.Add(backend.lease + time.Second)
	second, err := backend.Reserve(context.Background(), other)
	if err != nil || second.ID == first.ID {
		t.Fatalf("Reserve() after expiry = %#v, %v", second, err)
	}
	if _, err := backend.Upload(context.Background(), first.ID, BlobSource{Reader: bytes.NewReader([]byte("old")), Size: 3, Generation: first.Generation}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired Upload() error = %v, want not found", err)
	}
	contents := []byte("new")
	digest := sha256.Sum256(contents)
	blob, err := backend.Upload(context.Background(), second.ID, BlobSource{
		Reader: bytes.NewReader(contents), Size: 3, SHA256: hex.EncodeToString(digest[:]), Generation: second.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := backend.Commit(context.Background(), second.ID, blob)
	if err != nil {
		t.Fatal(err)
	}
	if replayed, err := backend.Commit(context.Background(), second.ID, blob); err != nil || replayed.ID != entry.ID {
		t.Fatalf("Commit() replay = %#v, %v, want %q", replayed, err, entry.ID)
	}
	if _, err := backend.Reserve(context.Background(), request); !errors.Is(err, ErrContention) {
		t.Fatalf("Reserve() committed identity error = %v, want contention", err)
	}
	invalid := request
	invalid.Key = string([]byte{0xff})
	if _, err := backend.Reserve(context.Background(), invalid); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-UTF-8 Reserve() error = %v, want conflict", err)
	}
}

func TestExperimentalDirectoryBackendUploadMismatchReplayAbortAndEmptyBlob(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := newTestDirectoryBackend(t, t.TempDir(), &now)
	size := int64(3)
	reservation, err := backend.Reserve(context.Background(), ReserveRequest{
		Key: "mismatch", Version: "v1", Owner: "job", DeclaredSize: &size,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, contents := range [][]byte{[]byte("no"), []byte("long")} {
		if _, err := backend.Upload(context.Background(), reservation.ID, BlobSource{Reader: bytes.NewReader(contents), Size: size, Generation: reservation.Generation}); !errors.Is(err, ErrConflict) {
			t.Errorf("mismatched Upload(%q) error = %v, want conflict", contents, err)
		}
	}
	one := []byte("one")
	digest := sha256.Sum256(one)
	source := BlobSource{Reader: bytes.NewReader(one), Size: size, SHA256: hex.EncodeToString(digest[:]), Generation: reservation.Generation}
	blob, err := backend.Upload(context.Background(), reservation.ID, source)
	if err != nil {
		t.Fatal(err)
	}
	source.Reader = bytes.NewReader(one)
	if replay, err := backend.Upload(context.Background(), reservation.ID, source); err != nil || replay != blob {
		t.Fatalf("Upload() replay = %#v, %v, want %#v", replay, err, blob)
	}
	two := []byte("two")
	twoDigest := sha256.Sum256(two)
	if _, err := backend.Upload(context.Background(), reservation.ID, BlobSource{
		Reader: bytes.NewReader(two), Size: size, SHA256: hex.EncodeToString(twoDigest[:]), Generation: reservation.Generation,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Upload() error = %v, want conflict", err)
	}
	if _, err := os.Lstat(backend.blobPath(hex.EncodeToString(twoDigest[:]))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting blob error = %v, want not exist", err)
	}
	if err := backend.Abort(context.Background(), reservation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Upload(context.Background(), reservation.ID, source); !errors.Is(err, ErrNotFound) {
		t.Fatalf("aborted Upload() error = %v, want not found", err)
	}
	if err := backend.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(backend.blobPath(blob.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aborted blob error = %v, want not exist", err)
	}

	empty := putDirectoryEntry(t, backend, "branch", "empty", "v1", "empty", nil)
	reader, info, err := backend.Open(context.Background(), empty.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(contents) != 0 || info.Size != 0 {
		t.Fatalf("empty Open() = %q, %#v, %v, %v", contents, info, readErr, closeErr)
	}
	if _, _, err := backend.Open(context.Background(), empty.ID, &ByteRange{Start: 0, End: 0}); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty range Open() error = %v, want conflict", err)
	}
}

func TestExperimentalDirectoryBackendSkipsCorruptEntriesAndBlobs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := newTestDirectoryBackend(t, t.TempDir(), &now)
	older := putDirectoryEntry(t, backend, "branch", "prefix-old", "v1", "old", []byte("older"))
	now = now.Add(time.Second)
	newer := putDirectoryEntry(t, backend, "branch", "prefix-new", "v1", "new", []byte("newer"))

	if err := os.WriteFile(backend.blobPath(newer.Blob.SHA256), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, ok, err := backend.Lookup(context.Background(), LookupRequest{
		Candidates: []string{"prefix-"}, Version: "v1",
	})
	if err != nil || !ok || entry.ID != older.ID {
		t.Fatalf("Lookup() around corrupt newest = %#v, %v, %v, want %q", entry, ok, err, older.ID)
	}
	if _, _, err := backend.Open(context.Background(), newer.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt Open() error = %v, want not found", err)
	}

	badManifest := filepath.Join(backend.entriesDir, "e"+stringsOfByte('a', 32)+".json")
	if err := os.WriteFile(badManifest, []byte(`{"format":1,"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bogusBlob := stringsOfByte('b', 64)
	if err := os.WriteFile(filepath.Join(backend.blobsDir, bogusBlob), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backend.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{badManifest, backend.entryPath(newer.ID), filepath.Join(backend.blobsDir, bogusBlob), backend.blobPath(newer.Blob.SHA256)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pruned path %q error = %v, want not exist", path, err)
		}
	}
}

func TestExperimentalDirectoryBackendPrunesByTTLAndEntryLimit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := newTestDirectoryBackend(t, t.TempDir(), &now)
	backend.maxEntries = 2
	backend.ttl = time.Hour
	oldest := putDirectoryEntry(t, backend, "branch", "key-one", "v1", "one", []byte("one"))
	now = now.Add(time.Second)
	second := putDirectoryEntry(t, backend, "branch", "key-two", "v1", "two", []byte("two"))
	now = now.Add(time.Second)
	third := putDirectoryEntry(t, backend, "branch", "key-three", "v1", "three", []byte("three"))

	if _, _, err := backend.Open(context.Background(), oldest.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entry-limit Open(oldest) error = %v, want not found", err)
	}
	for _, entry := range []Entry{second, third} {
		reader, _, err := backend.Open(context.Background(), entry.ID, nil)
		if err != nil {
			t.Fatalf("Open(%s): %v", entry.Key, err)
		}
		_ = reader.Close()
	}
	now = now.Add(time.Hour)
	if err := backend.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := backend.List(context.Background(), ListRequest{Limit: 10})
	if err != nil || len(entries) != 0 {
		t.Fatalf("List() after TTL = %#v, %v, want empty", entries, err)
	}
	blobs, err := os.ReadDir(backend.blobsDir)
	if err != nil || len(blobs) != 0 {
		t.Fatalf("blobs after TTL = %#v, %v, want empty", blobs, err)
	}
}

func TestExperimentalDirectoryBackendPrunesByUniqueBlobBytes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := newTestDirectoryBackend(t, t.TempDir(), &now)
	backend.maxBytes = 4
	first := putDirectoryEntry(t, backend, "branch", "same-one", "v1", "one", []byte("same"))
	now = now.Add(time.Second)
	second := putDirectoryEntry(t, backend, "branch", "same-two", "v1", "two", []byte("same"))
	if first.Blob.SHA256 != second.Blob.SHA256 {
		t.Fatal("test entries did not share a blob")
	}
	entries, err := backend.List(context.Background(), ListRequest{Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("deduplicated List() = %#v, %v", entries, err)
	}
	now = now.Add(time.Second)
	newest := putDirectoryEntry(t, backend, "branch", "newest", "v1", "newest", []byte("x"))
	entries, err = backend.List(context.Background(), ListRequest{Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].ID != newest.ID {
		t.Fatalf("byte-limited List() = %#v, %v, want only %q", entries, err, newest.ID)
	}
}

func TestExperimentalDirectoryBackendConcurrentReservationHasOneWinner(t *testing.T) {
	now := time.Now().UTC()
	backend := newTestDirectoryBackend(t, t.TempDir(), &now)
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
				Key: "key", Version: "version", Owner: string(rune('a' + worker)),
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

func stringsOfByte(value byte, count int) string {
	return string(bytes.Repeat([]byte{value}, count))
}
