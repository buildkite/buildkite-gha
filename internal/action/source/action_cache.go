package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	path    string
	size    int64
	lastUse time.Time
}

type materializedLease struct {
	release func()
	retain  func(context.Context) (Materialized, error)
	once    sync.Once
}

func (s *Store) materializedLease(ctx context.Context, lock *actionCacheLock, resolved Resolved, tree, actionPath, digest string) (Materialized, error) {
	m, err := materialized(tree, actionPath, digest)
	if err != nil {
		lock.unlock()
		return Materialized{}, err
	}
	lease := &materializedLease{}
	lease.release = func() {
		lease.once.Do(func() {
			lock.unlock()
			if s.cfg.cacheMaxBytes == 0 {
				return
			}
			total, _ := s.readCacheSize()
			if s.pressure.Load() || total > s.cfg.cacheMaxBytes {
				_ = s.maintain(context.WithoutCancel(ctx))
			}
		})
	}
	lease.retain = func(ctx context.Context) (Materialized, error) {
		base := filepath.Dir(tree)
		retained, err := lockActionCache(ctx, base+".lock", actionCacheLockShared, false)
		if err != nil {
			return Materialized{}, err
		}
		cached, err := s.cachedManifest(base, resolved)
		if err != nil {
			retained.unlock()
			return s.Materialize(ctx, resolved)
		}
		s.touch(base)
		return s.materializedLease(ctx, retained, resolved, tree, actionPath, cached.Digest)
	}
	m.lease = lease
	return m, nil
}

func (s *Store) cachedManifest(base string, resolved Resolved) (manifest, error) {
	data, err := os.ReadFile(filepath.Join(base, manifestName))
	if err != nil || len(data) > 16<<20 {
		return manifest{}, fmt.Errorf("invalid manifest")
	}
	var cached manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cached) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		cached.Schema != "buildkite-gha-action-source/v1" ||
		cached.Owner != strings.ToLower(resolved.Reference.Owner) ||
		cached.Repository != strings.ToLower(resolved.Reference.Repository) ||
		cached.Commit != resolved.Commit || cached.Digest == "" ||
		(resolved.SourceDigest != "" && cached.Digest != resolved.SourceDigest) {
		return manifest{}, fmt.Errorf("invalid manifest")
	}
	return cached, nil
}

func (s *Store) touch(base string) {
	now := time.Now()
	_ = os.Chtimes(filepath.Join(base, manifestName), now, now)
}

func (s *Store) createPartial(ctx context.Context, parent string) (string, *actionCacheLock, error) {
	maintenance, err := lockActionCache(ctx, filepath.Join(s.root, ".maintenance.lock"), actionCacheLockExclusive, false)
	if err != nil {
		return "", nil, err
	}
	defer maintenance.unlock()
	tmp, err := os.MkdirTemp(parent, ".partial-")
	if err != nil {
		return "", nil, err
	}
	lock, err := lockActionCache(ctx, filepath.Join(tmp, ".lock"), actionCacheLockExclusive, false)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return "", nil, err
	}
	return tmp, lock, nil
}

func (s *Store) maintain(ctx context.Context) error {
	maintenance, err := lockActionCache(ctx, filepath.Join(s.root, ".maintenance.lock"), actionCacheLockExclusive, false)
	if err != nil {
		return err
	}
	defer maintenance.unlock()
	return s.maintainLocked(ctx)
}

func (s *Store) maintainLocked(ctx context.Context) error {
	entries, partials, err := s.cacheEntries()
	if err != nil {
		return err
	}
	for _, partial := range partials {
		lock, lockErr := lockActionCache(ctx, filepath.Join(partial, ".lock"), actionCacheLockExclusive, true)
		if lockErr != nil {
			continue
		}
		_ = os.RemoveAll(partial)
		lock.unlock()
	}
	var total int64
	for _, entry := range entries {
		total += entry.size
	}
	if total <= s.cfg.cacheMaxBytes {
		s.pressure.Store(false)
		return s.writeCacheSize(total)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastUse.Before(entries[j].lastUse)
	})
	for _, entry := range entries {
		if total <= s.cfg.cacheMaxBytes {
			break
		}
		lock, lockErr := lockActionCache(ctx, entry.path+".lock", actionCacheLockExclusive, true)
		if lockErr != nil {
			continue
		}
		manifest, statErr := os.Stat(filepath.Join(entry.path, manifestName))
		if statErr != nil || manifest.ModTime().After(entry.lastUse) {
			lock.unlock()
			continue
		}
		if err := os.RemoveAll(entry.path); err != nil {
			lock.unlock()
			return err
		}
		total -= entry.size
		lock.unlock()
	}
	s.pressure.Store(total > s.cfg.cacheMaxBytes)
	return s.writeCacheSize(total)
}

func (s *Store) publishCacheEntry(ctx context.Context, temporary, base string) error {
	maintenance, err := lockActionCache(ctx, filepath.Join(s.root, ".maintenance.lock"), actionCacheLockExclusive, false)
	if err != nil {
		return err
	}
	defer maintenance.unlock()
	if err := os.Rename(temporary, base); err != nil {
		return err
	}
	published := true
	defer func() {
		if published {
			_ = os.RemoveAll(base)
		}
	}()
	size, err := cacheEntrySize(base)
	if err != nil {
		return err
	}
	total, err := s.readCacheSize()
	if s.cfg.cacheMaxBytes == 0 && errors.Is(err, os.ErrNotExist) {
		published = false
		return nil
	}
	if err != nil {
		if s.cfg.cacheMaxBytes == 0 {
			return err
		}
		err = s.maintainLocked(ctx)
		published = err != nil
		return err
	}
	total += size
	if s.cfg.cacheMaxBytes == 0 {
		err = s.writeCacheSize(total)
		published = err != nil
		return err
	}
	if total > s.cfg.cacheMaxBytes {
		err = s.maintainLocked(ctx)
		published = err != nil
		return err
	}
	s.pressure.Store(false)
	err = s.writeCacheSize(total)
	published = err != nil
	return err
}

func (s *Store) readCacheSize() (int64, error) {
	data, err := os.ReadFile(filepath.Join(s.root, ".size-v1"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

func (s *Store) writeCacheSize(size int64) error {
	temporary, err := os.CreateTemp(s.root, ".size-*.tmp")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer func() { _ = os.Remove(path) }()
	if err = temporary.Chmod(0o600); err == nil {
		_, err = fmt.Fprintf(temporary, "%d\n", size)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(s.root, ".size-v1"))
}

func (s *Store) cacheEntries() ([]cacheEntry, []string, error) {
	owners, err := os.ReadDir(s.root)
	if err != nil {
		return nil, nil, err
	}
	var entries []cacheEntry
	var partials []string
	for _, owner := range owners {
		if !owner.IsDir() || strings.HasPrefix(owner.Name(), ".") {
			continue
		}
		ownerPath := filepath.Join(s.root, owner.Name())
		repositories, readErr := os.ReadDir(ownerPath)
		if readErr != nil {
			return nil, nil, readErr
		}
		for _, repository := range repositories {
			if !repository.IsDir() || strings.HasPrefix(repository.Name(), ".") {
				continue
			}
			repositoryPath := filepath.Join(ownerPath, repository.Name())
			commits, readErr := os.ReadDir(repositoryPath)
			if readErr != nil {
				return nil, nil, readErr
			}
			for _, commit := range commits {
				path := filepath.Join(repositoryPath, commit.Name())
				if commit.IsDir() && strings.HasPrefix(commit.Name(), ".partial-") {
					partials = append(partials, path)
					continue
				}
				if !commit.IsDir() || !shaRE.MatchString(commit.Name()) {
					continue
				}
				size, sizeErr := cacheEntrySize(path)
				manifest, statErr := os.Stat(filepath.Join(path, manifestName))
				if sizeErr != nil {
					return nil, nil, sizeErr
				}
				if statErr != nil {
					return nil, nil, statErr
				}
				entries = append(entries, cacheEntry{path: path, size: size, lastUse: manifest.ModTime()})
			}
		}
	}
	return entries, partials, nil
}

func cacheEntrySize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
