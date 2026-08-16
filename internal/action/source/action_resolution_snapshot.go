package source

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
	"strings"
	"time"
)

const actionResolutionSnapshotSchema = "buildkite-gha-action-resolution-snapshot/v1"

type actionResolutionSnapshot struct {
	root       string
	generation string
}

type actionResolutionSnapshotCurrent struct {
	Schema     string `json:"schema"`
	Generation string `json:"generation"`
}

type actionResolutionSnapshotEntry struct {
	Schema     string    `json:"schema"`
	Owner      string    `json:"owner"`
	Repository string    `json:"repository"`
	Ref        string    `json:"ref"`
	Commit     string    `json:"commit,omitempty"`
	Missing    bool      `json:"missing,omitempty"`
	ResolvedAt time.Time `json:"resolved_at"`
}

// WithActionResolutionSnapshot pins mutable refs to durable per-generation
// entries. Refresh starts a new generation without disrupting active readers.
func WithActionResolutionSnapshot(root string, refresh bool) Option {
	return func(c *config) error {
		snapshot, err := newActionResolutionSnapshot(root, refresh)
		if err != nil {
			return err
		}
		c.resolutionSnapshot = snapshot
		return nil
	}
}

func newActionResolutionSnapshot(root string, refresh bool) (*actionResolutionSnapshot, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("action resolution snapshot root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve action resolution snapshot root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create action resolution snapshot root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("action resolution snapshot root is not a non-symlink directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("canonicalize action resolution snapshot root: %w", err)
	}
	canonicalInfo, err := os.Stat(canonical)
	if err != nil || !os.SameFile(info, canonicalInfo) {
		return nil, fmt.Errorf("action resolution snapshot root changed while canonicalizing")
	}
	absolute = canonical
	unlock, err := lockMutableRefCache(context.Background(), filepath.Join(absolute, ".snapshot.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock action resolution snapshot: %w", err)
	}
	defer unlock()
	currentPath := filepath.Join(absolute, "current.json")
	current, valid := loadActionResolutionSnapshotCurrent(currentPath)
	_, currentErr := os.Stat(currentPath)
	if !refresh && currentErr == nil && !valid {
		return nil, fmt.Errorf("action resolution snapshot current generation is invalid")
	}
	if !refresh && currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read action resolution snapshot current generation: %w", currentErr)
	}
	if refresh || errors.Is(currentErr, os.ErrNotExist) {
		generation, err := newActionResolutionGeneration()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(absolute, "generations", generation), 0o700); err != nil {
			return nil, fmt.Errorf("create action resolution generation: %w", err)
		}
		current = actionResolutionSnapshotCurrent{Schema: actionResolutionSnapshotSchema, Generation: generation}
		if err := storeActionResolutionJSON(currentPath, current); err != nil {
			return nil, err
		}
	}
	return &actionResolutionSnapshot{root: absolute, generation: current.Generation}, nil
}

func newActionResolutionGeneration() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create action resolution generation: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func loadActionResolutionSnapshotCurrent(path string) (actionResolutionSnapshotCurrent, bool) {
	var current actionResolutionSnapshotCurrent
	if !loadActionResolutionJSON(path, &current) || current.Schema != actionResolutionSnapshotSchema || len(current.Generation) != 32 {
		return actionResolutionSnapshotCurrent{}, false
	}
	if _, err := hex.DecodeString(current.Generation); err != nil {
		return actionResolutionSnapshotCurrent{}, false
	}
	return current, true
}

func (s *actionResolutionSnapshot) resolve(ctx context.Context, ref Reference, resolve func(context.Context, Reference) (Resolved, error)) (Resolved, error) {
	path := s.entryPath(ref)
	if resolved, err, ok := loadActionResolutionSnapshotEntry(path, ref); ok {
		return resolved, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Resolved{}, err
	}
	unlock, err := lockMutableRefCache(ctx, path+".lock")
	if err != nil {
		return Resolved{}, err
	}
	defer unlock()
	if resolved, err, ok := loadActionResolutionSnapshotEntry(path, ref); ok {
		return resolved, err
	}
	resolved, err := resolve(ctx, ref)
	entry := actionResolutionSnapshotEntry{
		Schema: actionResolutionSnapshotSchema, Owner: strings.ToLower(ref.Owner),
		Repository: strings.ToLower(ref.Repository), Ref: ref.Ref, ResolvedAt: time.Now().UTC(),
	}
	if err == nil {
		entry.Commit = resolved.Commit
	} else {
		var notPublic *NotPublicError
		if !errors.As(err, &notPublic) {
			return Resolved{}, err
		}
		entry.Missing = true
	}
	if storeErr := storeActionResolutionJSON(path, entry); storeErr != nil {
		return Resolved{}, storeErr
	}
	return resolved, err
}

func (s *actionResolutionSnapshot) entryPath(ref Reference) string {
	identity := strings.ToLower(ref.Owner) + "\x00" + strings.ToLower(ref.Repository) + "\x00" + ref.Ref
	sum := sha256.Sum256([]byte(identity))
	key := hex.EncodeToString(sum[:])
	return filepath.Join(s.root, "generations", s.generation, key[:2], key+".json")
}

func loadActionResolutionSnapshotEntry(path string, ref Reference) (Resolved, error, bool) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Resolved{}, nil, false
	}
	if err != nil {
		return Resolved{}, fmt.Errorf("read action resolution snapshot entry: %w", err), true
	}
	defer func() { _ = file.Close() }()
	var entry actionResolutionSnapshotEntry
	if !decodeActionResolutionJSON(file, &entry) || entry.Schema != actionResolutionSnapshotSchema ||
		entry.Owner != strings.ToLower(ref.Owner) || entry.Repository != strings.ToLower(ref.Repository) || entry.Ref != ref.Ref ||
		(entry.Missing == (entry.Commit != "")) || entry.Commit != "" && !shaRE.MatchString(entry.Commit) {
		return Resolved{}, fmt.Errorf("action resolution snapshot entry is invalid"), true
	}
	if entry.Missing {
		return Resolved{}, &NotPublicError{}, true
	}
	return Resolved{Reference: ref, Commit: entry.Commit}, nil, true
}

func loadActionResolutionJSON(path string, value any) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	return decodeActionResolutionJSON(file, value)
}

func decodeActionResolutionJSON(reader io.Reader, value any) bool {
	data, err := io.ReadAll(io.LimitReader(reader, 16<<10+1))
	if err != nil || len(data) > 16<<10 {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value) == nil && decoder.Decode(&struct{}{}) == io.EOF
}

func storeActionResolutionJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode action resolution snapshot: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".resolution-*.tmp")
	if err != nil {
		return fmt.Errorf("create action resolution snapshot entry: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write action resolution snapshot entry: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish action resolution snapshot entry: %w", err)
	}
	return nil
}
