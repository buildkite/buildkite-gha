package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type mutableRefCache struct {
	root      string
	freshness time.Duration
}

type mutableRefEntry struct {
	Schema     string    `json:"schema"`
	Owner      string    `json:"owner"`
	Repository string    `json:"repository"`
	Ref        string    `json:"ref"`
	Commit     string    `json:"commit"`
	ResolvedAt time.Time `json:"resolved_at"`
}

func newMutableRefCache(root string, freshness time.Duration) (*mutableRefCache, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("mutable ref cache root is required")
	}
	if freshness <= 0 {
		return nil, fmt.Errorf("mutable ref cache freshness must be positive")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve mutable ref cache root: %w", err)
	}
	return &mutableRefCache{root: absolute, freshness: freshness}, nil
}

func (c *mutableRefCache) resolve(ctx context.Context, ref Reference, resolve func(context.Context, Reference) (Resolved, error)) (Resolved, error) {
	path := c.path(ref)
	if resolved, ok := c.load(path, ref, time.Now()); ok {
		return resolved, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return resolve(ctx, ref)
	}
	unlock, err := lockMutableRefCache(ctx, path+".lock")
	if err != nil {
		if ctx.Err() != nil {
			return Resolved{}, ctx.Err()
		}
		return resolve(ctx, ref)
	}
	defer unlock()
	if resolved, ok := c.load(path, ref, time.Now()); ok {
		return resolved, nil
	}
	resolved, err := resolve(ctx, ref)
	if err != nil {
		return Resolved{}, err
	}
	entry := mutableRefEntry{
		Schema: "buildkite-gha-action-ref-resolution/v1", Owner: strings.ToLower(ref.Owner),
		Repository: strings.ToLower(ref.Repository), Ref: ref.Ref, Commit: resolved.Commit, ResolvedAt: time.Now().UTC(),
	}
	if err := c.store(path, entry); err != nil {
		return resolved, nil
	}
	return resolved, nil
}

func (c *mutableRefCache) path(ref Reference) string {
	identity := strings.ToLower(ref.Owner) + "\x00" + strings.ToLower(ref.Repository) + "\x00" + ref.Ref
	sum := sha256.Sum256([]byte(identity))
	key := hex.EncodeToString(sum[:])
	return filepath.Join(c.root, key[:2], key+".json")
}

func (c *mutableRefCache) load(path string, ref Reference, now time.Time) (Resolved, bool) {
	file, err := os.Open(path)
	if err != nil {
		return Resolved{}, false
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	_ = file.Close()
	if err != nil || len(data) > 4096 {
		return Resolved{}, false
	}
	var entry mutableRefEntry
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&entry) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		entry.Schema != "buildkite-gha-action-ref-resolution/v1" ||
		entry.Owner != strings.ToLower(ref.Owner) || entry.Repository != strings.ToLower(ref.Repository) ||
		entry.Ref != ref.Ref || !shaRE.MatchString(entry.Commit) || entry.ResolvedAt.After(now) ||
		now.Sub(entry.ResolvedAt) >= c.freshness {
		return Resolved{}, false
	}
	return Resolved{Reference: ref, Commit: entry.Commit}, true
}

func (c *mutableRefCache) store(path string, entry mutableRefEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode mutable ref cache: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".resolution-*.tmp")
	if err != nil {
		return fmt.Errorf("create mutable ref cache entry: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure mutable ref cache entry: %w", err)
	}
	_, err = temporary.Write(data)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write mutable ref cache entry: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish mutable ref cache entry: %w", err)
	}
	return nil
}
