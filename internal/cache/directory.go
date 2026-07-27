package cache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	directoryFormatVersion  = 1
	directoryManifestLimit  = 64 << 10
	directoryDefaultBytes   = 10 << 30
	directoryDefaultEntries = 10_000
	directoryDefaultLease   = 15 * time.Minute
	directoryDefaultTTL     = 7 * 24 * time.Hour
)

// NewExperimentalDirectoryBackend stores opaque cache archives in a local
// directory. It is intended for development and best-effort cache volumes: its
// reservations coordinate one process, not concurrent snapshot forks or hosts.
func NewExperimentalDirectoryBackend(root string) (Backend, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("experimental cache directory must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("%w: create cache root: %v", ErrUnavailable, err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve cache root: %v", ErrUnavailable, err)
	}
	root = filepath.Join(resolved, "buildkite-gha-cache-v1")
	b := &experimentalDirectoryBackend{
		root:         root,
		entriesDir:   filepath.Join(root, "entries"),
		blobsDir:     filepath.Join(root, "blobs"),
		stagingDir:   filepath.Join(root, "staging"),
		now:          time.Now,
		random:       rand.Reader,
		lease:        directoryDefaultLease,
		ttl:          directoryDefaultTTL,
		maxEntries:   directoryDefaultEntries,
		maxBytes:     directoryDefaultBytes,
		reservations: make(map[ReservationID]*directoryReservation),
	}
	for _, dir := range []string{b.root, b.entriesDir, b.blobsDir, b.stagingDir} {
		if err := ensurePrivateDirectory(dir); err != nil {
			return nil, err
		}
	}
	if err := removeDirectoryContents(b.stagingDir); err != nil {
		return nil, fmt.Errorf("%w: clear abandoned cache uploads: %v", ErrUnavailable, err)
	}
	if err := b.Prune(context.Background()); err != nil {
		return nil, err
	}
	return b, nil
}

type experimentalDirectoryBackend struct {
	mu           sync.Mutex
	root         string
	entriesDir   string
	blobsDir     string
	stagingDir   string
	now          func() time.Time
	random       io.Reader
	lease        time.Duration
	ttl          time.Duration
	maxEntries   int
	maxBytes     int64
	reservations map[ReservationID]*directoryReservation
}

type directoryReservation struct {
	Reservation
	blob      Blob
	committed Entry
	aborted   bool
}

type directoryManifest struct {
	Format       int       `json:"format"`
	ID           string    `json:"id"`
	Organization string    `json:"organization"`
	Cluster      string    `json:"cluster"`
	Pipeline     string    `json:"pipeline"`
	Scope        string    `json:"scope"`
	Key          string    `json:"key"`
	Version      string    `json:"version"`
	CreationTime time.Time `json:"creation_time"`
	ExpiresAt    time.Time `json:"expires_at"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	Generation   string    `json:"generation"`
}

type directoryReadCloser struct {
	io.Reader
	io.Closer
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("%w: create cache directory: %v", ErrUnavailable, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: cache path is not a real directory", ErrUnavailable)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("%w: protect cache directory: %v", ErrUnavailable, err)
	}
	return nil
}

func removeDirectoryContents(path string) error {
	items, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := os.RemoveAll(filepath.Join(path, item.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (b *experimentalDirectoryBackend) Lookup(ctx context.Context, q LookupRequest) (Entry, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries, _, err := b.loadEntriesLocked(ctx)
	if err != nil {
		return Entry{}, false, err
	}
	now := b.now()
	for _, scope := range q.Scopes {
		for _, candidate := range q.Candidates {
			var exact, prefix []Entry
			for _, entry := range entries {
				if entry.Namespace != q.Namespace || entry.Scope != scope || entry.Version != q.Version || !entry.ExpiresAt.After(now) {
					continue
				}
				switch {
				case entry.Key == candidate:
					exact = append(exact, entry)
				case strings.HasPrefix(entry.Key, candidate):
					prefix = append(prefix, entry)
				}
			}
			for _, matches := range [][]Entry{exact, prefix} {
				sortDirectoryEntries(matches)
				for _, entry := range matches {
					valid, err := b.validBlobLocked(ctx, entry)
					if err != nil {
						return Entry{}, false, err
					}
					if valid {
						return entry, true, nil
					}
				}
			}
		}
	}
	return Entry{}, false, nil
}

func (b *experimentalDirectoryBackend) List(ctx context.Context, q ListRequest) ([]Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries, _, err := b.loadEntriesLocked(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[Scope]bool, len(q.Scopes))
	for _, scope := range q.Scopes {
		allowed[scope] = true
	}
	now := b.now()
	out := make([]Entry, 0, max(0, min(len(entries), q.Limit)))
	for _, entry := range entries {
		if entry.Namespace != q.Namespace || !allowed[entry.Scope] || !entry.ExpiresAt.After(now) || q.Key != "" && !strings.HasPrefix(entry.Key, q.Key) {
			continue
		}
		valid, err := b.validBlobMetadataLocked(entry)
		if err != nil {
			return nil, err
		}
		if valid {
			out = append(out, entry)
		}
	}
	sortDirectoryEntries(out)
	if q.Limit >= 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (b *experimentalDirectoryBackend) Reserve(ctx context.Context, q ReserveRequest) (Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Reservation{}, err
	}
	if q.Namespace.Organization == "" || q.Namespace.Cluster == "" || q.Namespace.Pipeline == "" || q.Scope == "" || q.Key == "" || !utf8.ValidString(q.Key) || q.Version == "" || !utf8.ValidString(q.Version) || q.Owner == "" || q.DeclaredSize != nil && *q.DeclaredSize < 0 {
		return Reservation{}, ErrConflict
	}
	now := b.now()
	for id, reservation := range b.reservations {
		if reservation.aborted || !reservation.LeaseExpiresAt.After(now) {
			delete(b.reservations, id)
		}
	}
	entries, _, err := b.loadEntriesLocked(ctx)
	if err != nil {
		return Reservation{}, err
	}
	for _, entry := range entries {
		if !sameDirectoryAddress(entry.Namespace, entry.Scope, entry.Key, entry.Version, q.Namespace, q.Scope, q.Key, q.Version) || !entry.ExpiresAt.After(now) {
			continue
		}
		valid, err := b.validBlobLocked(ctx, entry)
		if err != nil {
			return Reservation{}, err
		}
		if valid {
			return Reservation{}, ErrContention
		}
	}
	for _, reservation := range b.reservations {
		if !sameDirectoryAddress(reservation.Namespace, reservation.Scope, reservation.Key, reservation.Version, q.Namespace, q.Scope, q.Key, q.Version) {
			continue
		}
		if reservation.Owner == q.Owner {
			return reservation.Reservation, nil
		}
		return Reservation{}, ErrContention
	}
	id, err := b.uniqueIDLocked("r")
	if err != nil {
		return Reservation{}, err
	}
	generation, err := b.uniqueIDLocked("g")
	if err != nil {
		return Reservation{}, err
	}
	reservation := Reservation{
		ID: ReservationID(id), Namespace: q.Namespace, Scope: q.Scope,
		Key: q.Key, Version: q.Version, Owner: q.Owner, Generation: generation,
		DeclaredSize: cloneInt64(q.DeclaredSize), LeaseExpiresAt: now.Add(b.lease),
	}
	b.reservations[reservation.ID] = &directoryReservation{Reservation: reservation}
	return reservation, nil
}

func (b *experimentalDirectoryBackend) Upload(ctx context.Context, id ReservationID, source BlobSource) (Blob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	reservation := b.reservations[id]
	if reservation == nil || reservation.aborted || !reservation.LeaseExpiresAt.After(b.now()) {
		return Blob{}, ErrNotFound
	}
	if source.Generation != reservation.Generation || source.Size < 0 {
		return Blob{}, ErrConflict
	}
	if source.Size > b.maxBytes {
		return Blob{}, ErrTooLarge
	}
	if reservation.DeclaredSize != nil && source.Size != *reservation.DeclaredSize {
		return Blob{}, ErrConflict
	}
	if source.SHA256 != "" && !validDirectoryDigest(source.SHA256) {
		return Blob{}, ErrConflict
	}
	stage, err := os.CreateTemp(b.stagingDir, ".upload-*")
	if err != nil {
		return Blob{}, fmt.Errorf("%w: create cache upload: %v", ErrUnavailable, err)
	}
	stagePath := stage.Name()
	removeStage := true
	defer func() {
		_ = stage.Close()
		if removeStage {
			_ = os.Remove(stagePath)
		}
	}()
	digest, size, err := copyDirectoryBlob(ctx, stage, source.Reader, source.Size)
	if err != nil {
		return Blob{}, err
	}
	if size != source.Size || source.SHA256 != "" && source.SHA256 != digest {
		return Blob{}, ErrConflict
	}
	if err := stage.Sync(); err != nil {
		return Blob{}, fmt.Errorf("%w: sync cache upload: %v", ErrUnavailable, err)
	}
	if err := stage.Close(); err != nil {
		return Blob{}, fmt.Errorf("%w: close cache upload: %v", ErrUnavailable, err)
	}
	blob := Blob{Locator: "sha256:" + digest, SHA256: digest, Size: size, Generation: reservation.Generation}
	if reservation.blob.Locator != "" && reservation.blob != blob {
		return Blob{}, ErrConflict
	}
	destination := b.blobPath(digest)
	if existing, err := b.validBlobFileLocked(ctx, destination, digest, size); err != nil {
		return Blob{}, err
	} else if !existing {
		if err := os.Rename(stagePath, destination); err != nil {
			return Blob{}, fmt.Errorf("%w: publish cache blob: %v", ErrUnavailable, err)
		}
		removeStage = false
		if err := syncDirectory(b.blobsDir); err != nil {
			return Blob{}, err
		}
	}
	reservation.blob = blob
	return blob, nil
}

func (b *experimentalDirectoryBackend) Commit(ctx context.Context, id ReservationID, blob Blob) (Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	reservation := b.reservations[id]
	if reservation == nil || reservation.aborted || !reservation.LeaseExpiresAt.After(b.now()) {
		return Entry{}, ErrNotFound
	}
	if reservation.committed.ID != "" {
		if reservation.committed.Blob == blob {
			return reservation.committed, nil
		}
		return Entry{}, ErrConflict
	}
	if reservation.blob != blob || blob.Generation != reservation.Generation {
		return Entry{}, ErrConflict
	}
	valid, err := b.validBlobFileLocked(ctx, b.blobPath(blob.SHA256), blob.SHA256, blob.Size)
	if err != nil {
		return Entry{}, err
	}
	if !valid {
		return Entry{}, ErrConflict
	}
	entries, _, err := b.loadEntriesLocked(ctx)
	if err != nil {
		return Entry{}, err
	}
	for _, entry := range entries {
		if !sameDirectoryAddress(entry.Namespace, entry.Scope, entry.Key, entry.Version, reservation.Namespace, reservation.Scope, reservation.Key, reservation.Version) || !entry.ExpiresAt.After(b.now()) {
			continue
		}
		valid, err := b.validBlobLocked(ctx, entry)
		if err != nil {
			return Entry{}, err
		}
		if valid {
			return Entry{}, ErrContention
		}
	}
	entryID, err := b.uniqueEntryIDLocked()
	if err != nil {
		return Entry{}, err
	}
	now := b.now().UTC()
	entry := Entry{
		ID: EntryID(entryID), Namespace: reservation.Namespace, Scope: reservation.Scope,
		Key: reservation.Key, Version: reservation.Version, CreationTime: now,
		Blob: blob, ExpiresAt: now.Add(b.ttl),
	}
	if err := b.writeEntryLocked(entry); err != nil {
		return Entry{}, err
	}
	reservation.committed = entry
	_ = b.pruneLocked(context.Background())
	return entry, nil
}

func (b *experimentalDirectoryBackend) Abort(_ context.Context, id ReservationID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if reservation := b.reservations[id]; reservation != nil && reservation.committed.ID == "" {
		reservation.aborted = true
	}
	return nil
}

func (b *experimentalDirectoryBackend) Open(ctx context.Context, id EntryID, byteRange *ByteRange) (io.ReadCloser, BlobInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, err := b.readEntryLocked(id)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	if !entry.ExpiresAt.After(b.now()) {
		return nil, BlobInfo{}, ErrNotFound
	}
	file, err := os.Open(b.blobPath(entry.Blob.SHA256))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, BlobInfo{}, ErrNotFound
		}
		return nil, BlobInfo{}, fmt.Errorf("%w: open cache blob: %v", ErrUnavailable, err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Blob.Size {
		return nil, BlobInfo{}, ErrNotFound
	}
	digest, size, err := hashDirectoryFile(ctx, file)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	if digest != entry.Blob.SHA256 || size != entry.Blob.Size {
		return nil, BlobInfo{}, ErrNotFound
	}
	if byteRange != nil && (byteRange.Start < 0 || byteRange.End < byteRange.Start || byteRange.End >= entry.Blob.Size) {
		return nil, BlobInfo{}, ErrConflict
	}
	var reader io.Reader = io.NewSectionReader(file, 0, entry.Blob.Size)
	if byteRange != nil {
		reader = io.NewSectionReader(file, byteRange.Start, byteRange.End-byteRange.Start+1)
	}
	closeFile = false
	return directoryReadCloser{Reader: reader, Closer: file}, BlobInfo{SHA256: digest, Size: size}, nil
}

// Prune removes expired or excess entries and all blobs they no longer
// reference. Cache correctness must not depend on a retained entry.
func (b *experimentalDirectoryBackend) Prune(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pruneLocked(ctx)
}

func (b *experimentalDirectoryBackend) pruneLocked(ctx context.Context) error {
	entries, invalid, err := b.loadEntriesLocked(ctx)
	if err != nil {
		return err
	}
	for _, path := range invalid {
		_ = os.Remove(path)
	}
	sortDirectoryEntries(entries)
	now := b.now()
	retained := make(map[string]bool)
	keptEntries, retainedBytes := 0, int64(0)
	for _, entry := range entries {
		valid, err := b.validBlobMetadataLocked(entry)
		if err != nil {
			return err
		}
		extra := int64(0)
		if !retained[entry.Blob.SHA256] {
			extra = entry.Blob.Size
		}
		if !entry.ExpiresAt.After(now) || !valid || keptEntries >= b.maxEntries || retainedBytes+extra > b.maxBytes {
			_ = os.Remove(b.entryPath(entry.ID))
			continue
		}
		keptEntries++
		retainedBytes += extra
		retained[entry.Blob.SHA256] = true
	}
	for _, reservation := range b.reservations {
		if !reservation.aborted && reservation.committed.ID == "" && reservation.LeaseExpiresAt.After(now) && validDirectoryDigest(reservation.blob.SHA256) {
			retained[reservation.blob.SHA256] = true
		}
	}
	blobs, err := os.ReadDir(b.blobsDir)
	if err != nil {
		return fmt.Errorf("%w: list cache blobs: %v", ErrUnavailable, err)
	}
	for _, blob := range blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if blob.Type().IsRegular() && validDirectoryDigest(blob.Name()) && retained[blob.Name()] {
			continue
		}
		_ = os.RemoveAll(filepath.Join(b.blobsDir, blob.Name()))
	}
	if err := syncDirectory(b.entriesDir); err != nil {
		return err
	}
	return syncDirectory(b.blobsDir)
}

func (b *experimentalDirectoryBackend) loadEntriesLocked(ctx context.Context) ([]Entry, []string, error) {
	items, err := os.ReadDir(b.entriesDir)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: list cache entries: %v", ErrUnavailable, err)
	}
	entries := make([]Entry, 0, len(items))
	invalid := make([]string, 0)
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		path := filepath.Join(b.entriesDir, item.Name())
		if item.Type().IsRegular() && strings.HasSuffix(item.Name(), ".json") {
			entry, err := readDirectoryManifest(path, strings.TrimSuffix(item.Name(), ".json"))
			if err == nil {
				entries = append(entries, entry)
				continue
			}
			if !errors.Is(err, ErrConflict) && !errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("%w: read cache entry: %v", ErrUnavailable, err)
			}
		}
		invalid = append(invalid, path)
	}
	return entries, invalid, nil
}

func (b *experimentalDirectoryBackend) readEntryLocked(id EntryID) (Entry, error) {
	if !validDirectoryID(string(id), "e") {
		return Entry{}, ErrNotFound
	}
	entry, err := readDirectoryManifest(b.entryPath(id), string(id))
	if err != nil {
		return Entry{}, ErrNotFound
	}
	return entry, nil
}

func readDirectoryManifest(path, expectedID string) (Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Entry{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > directoryManifestLimit {
		return Entry{}, ErrConflict
	}
	var manifest directoryManifest
	decoder := json.NewDecoder(io.LimitReader(file, directoryManifestLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Entry{}, ErrConflict
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Entry{}, ErrConflict
	}
	if manifest.Format != directoryFormatVersion || manifest.ID != expectedID || !validDirectoryID(manifest.ID, "e") ||
		manifest.Organization == "" || manifest.Cluster == "" || manifest.Pipeline == "" || manifest.Scope == "" ||
		manifest.Key == "" || manifest.Version == "" || manifest.CreationTime.IsZero() || manifest.ExpiresAt.IsZero() ||
		manifest.ExpiresAt.Before(manifest.CreationTime) || !validDirectoryDigest(manifest.SHA256) || manifest.Size < 0 || manifest.Generation == "" {
		return Entry{}, ErrConflict
	}
	return Entry{
		ID:        EntryID(manifest.ID),
		Namespace: Namespace{Organization: manifest.Organization, Cluster: manifest.Cluster, Pipeline: manifest.Pipeline},
		Scope:     Scope(manifest.Scope), Key: manifest.Key, Version: manifest.Version,
		CreationTime: manifest.CreationTime, ExpiresAt: manifest.ExpiresAt,
		Blob: Blob{Locator: "sha256:" + manifest.SHA256, SHA256: manifest.SHA256, Size: manifest.Size, Generation: manifest.Generation},
	}, nil
}

func (b *experimentalDirectoryBackend) writeEntryLocked(entry Entry) error {
	manifest := directoryManifest{
		Format: directoryFormatVersion, ID: string(entry.ID),
		Organization: entry.Namespace.Organization, Cluster: entry.Namespace.Cluster, Pipeline: entry.Namespace.Pipeline,
		Scope: string(entry.Scope), Key: entry.Key, Version: entry.Version,
		CreationTime: entry.CreationTime, ExpiresAt: entry.ExpiresAt,
		SHA256: entry.Blob.SHA256, Size: entry.Blob.Size, Generation: entry.Blob.Generation,
	}
	contents, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("%w: encode cache entry: %v", ErrUnavailable, err)
	}
	contents = append(contents, '\n')
	stage, err := os.CreateTemp(b.entriesDir, ".entry-*")
	if err != nil {
		return fmt.Errorf("%w: create cache entry: %v", ErrUnavailable, err)
	}
	stagePath := stage.Name()
	defer func() {
		_ = stage.Close()
		_ = os.Remove(stagePath)
	}()
	if _, err := stage.Write(contents); err != nil {
		return fmt.Errorf("%w: write cache entry: %v", ErrUnavailable, err)
	}
	if err := stage.Sync(); err != nil {
		return fmt.Errorf("%w: sync cache entry: %v", ErrUnavailable, err)
	}
	if err := stage.Close(); err != nil {
		return fmt.Errorf("%w: close cache entry: %v", ErrUnavailable, err)
	}
	if _, err := os.Lstat(b.entryPath(entry.ID)); err == nil {
		return ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: inspect cache entry: %v", ErrUnavailable, err)
	}
	if err := os.Rename(stagePath, b.entryPath(entry.ID)); err != nil {
		return fmt.Errorf("%w: publish cache entry: %v", ErrUnavailable, err)
	}
	return syncDirectory(b.entriesDir)
}

func (b *experimentalDirectoryBackend) validBlobLocked(ctx context.Context, entry Entry) (bool, error) {
	return b.validBlobFileLocked(ctx, b.blobPath(entry.Blob.SHA256), entry.Blob.SHA256, entry.Blob.Size)
}

func (b *experimentalDirectoryBackend) validBlobMetadataLocked(entry Entry) (bool, error) {
	info, err := os.Stat(b.blobPath(entry.Blob.SHA256))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: inspect cache blob: %v", ErrUnavailable, err)
	}
	return info.Mode().IsRegular() && info.Size() == entry.Blob.Size, nil
}

func (b *experimentalDirectoryBackend) validBlobFileLocked(ctx context.Context, path, wantDigest string, wantSize int64) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: open cache blob: %v", ErrUnavailable, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("%w: inspect cache blob: %v", ErrUnavailable, err)
	}
	if !info.Mode().IsRegular() || info.Size() != wantSize {
		return false, nil
	}
	digest, size, err := hashDirectoryFile(ctx, file)
	if err != nil {
		return false, err
	}
	return digest == wantDigest && size == wantSize, nil
}

func copyDirectoryBlob(ctx context.Context, destination io.Writer, source io.Reader, expected int64) (string, int64, error) {
	hash := sha256.New()
	written, err := copyDirectoryContext(ctx, io.MultiWriter(destination, hash), io.LimitReader(source, expected+1))
	if err != nil {
		return "", 0, fmt.Errorf("%w: write cache blob: %v", ErrUnavailable, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func hashDirectoryFile(ctx context.Context, file *os.File) (string, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, fmt.Errorf("%w: seek cache blob: %v", ErrUnavailable, err)
	}
	hash := sha256.New()
	written, err := copyDirectoryContext(ctx, hash, file)
	if err != nil {
		return "", 0, fmt.Errorf("%w: hash cache blob: %v", ErrUnavailable, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func copyDirectoryContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func (b *experimentalDirectoryBackend) uniqueIDLocked(prefix string) (string, error) {
	for range 8 {
		bytes := make([]byte, 16)
		if _, err := io.ReadFull(b.random, bytes); err != nil {
			return "", fmt.Errorf("%w: generate cache identity: %v", ErrUnavailable, err)
		}
		id := prefix + hex.EncodeToString(bytes)
		if prefix != "r" {
			return id, nil
		}
		if _, exists := b.reservations[ReservationID(id)]; !exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: generate unique cache identity", ErrUnavailable)
}

func (b *experimentalDirectoryBackend) uniqueEntryIDLocked() (string, error) {
	for range 8 {
		id, err := b.uniqueIDLocked("e")
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(filepath.Join(b.entriesDir, id+".json")); errors.Is(err, os.ErrNotExist) {
			return id, nil
		} else if err != nil {
			return "", fmt.Errorf("%w: inspect cache identity: %v", ErrUnavailable, err)
		}
	}
	return "", fmt.Errorf("%w: generate unique cache entry identity", ErrUnavailable)
}

func (b *experimentalDirectoryBackend) entryPath(id EntryID) string {
	return filepath.Join(b.entriesDir, string(id)+".json")
}

func (b *experimentalDirectoryBackend) blobPath(digest string) string {
	return filepath.Join(b.blobsDir, digest)
}

func sortDirectoryEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreationTime.Equal(entries[j].CreationTime) {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].CreationTime.After(entries[j].CreationTime)
	})
}

func sameDirectoryAddress(namespaceA Namespace, scopeA Scope, keyA, versionA string, namespaceB Namespace, scopeB Scope, keyB, versionB string) bool {
	return namespaceA == namespaceB && scopeA == scopeB && keyA == keyB && versionA == versionB
}

func validDirectoryID(id, prefix string) bool {
	if len(id) != 33 || !strings.HasPrefix(id, prefix) {
		return false
	}
	_, err := hex.DecodeString(id[1:])
	return err == nil && strings.ToLower(id) == id
}

func validDirectoryDigest(digest string) bool {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open cache directory: %v", ErrUnavailable, err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("%w: sync cache directory: %v", ErrUnavailable, err)
	}
	return nil
}
