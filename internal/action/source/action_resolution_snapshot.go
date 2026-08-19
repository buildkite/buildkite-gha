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

var actionResolutionSnapshotStorage = struct {
	createTemp func(string, string) (*os.File, error)
	openFile   func(string, int, os.FileMode) (*os.File, error)
	write      func(*os.File, []byte) (int, error)
	close      func(*os.File) error
	rename     func(string, string) error
}{
	createTemp: os.CreateTemp,
	openFile:   os.OpenFile,
	write:      func(file *os.File, data []byte) (int, error) { return file.Write(data) },
	close:      func(file *os.File) error { return file.Close() },
	rename:     os.Rename,
}

type actionResolutionSnapshot struct {
	root       string
	generation string
}

type actionResolutionSnapshotCurrent struct {
	Schema     string `json:"schema"`
	Generation string `json:"generation"`
}

type actionResolutionSnapshotEntry struct {
	Schema      string    `json:"schema"`
	Owner       string    `json:"owner"`
	Repository  string    `json:"repository"`
	Ref         string    `json:"ref"`
	Commit      string    `json:"commit,omitempty"`
	ResolvedRef string    `json:"resolved_ref,omitempty"`
	Missing     bool      `json:"missing,omitempty"`
	ResolvedAt  time.Time `json:"resolved_at"`
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
	if !refresh && valid {
		generationInfo, err := os.Lstat(filepath.Join(absolute, "generations", current.Generation))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("action resolution snapshot current generation is missing")
		}
		if err != nil {
			return nil, fmt.Errorf("read action resolution snapshot current generation: %w", err)
		}
		if !generationInfo.IsDir() || generationInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("action resolution snapshot current generation is invalid")
		}
	}
	if !refresh && errors.Is(currentErr, os.ErrNotExist) {
		generations, err := os.ReadDir(filepath.Join(absolute, "generations"))
		if err == nil && len(generations) > 0 {
			return nil, fmt.Errorf("action resolution snapshot current generation is missing")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read action resolution snapshot generations: %w", err)
		}
	}
	if refresh || errors.Is(currentErr, os.ErrNotExist) {
		generation, err := newActionResolutionGeneration()
		if err != nil {
			return nil, err
		}
		generationPath := filepath.Join(absolute, "generations", generation)
		if err := os.MkdirAll(generationPath, 0o700); err != nil {
			return nil, fmt.Errorf("create action resolution generation: %w", err)
		}
		current = actionResolutionSnapshotCurrent{Schema: actionResolutionSnapshotSchema, Generation: generation}
		if err := storeActionResolutionJSON(currentPath, current); err != nil {
			if removeErr := os.RemoveAll(generationPath); removeErr != nil {
				return nil, errors.Join(err, fmt.Errorf("remove unpublished action resolution generation: %w", removeErr))
			}
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
	generationPath := filepath.Join(s.root, "generations", s.generation)
	generationInfo, err := os.Lstat(generationPath)
	if errors.Is(err, os.ErrNotExist) {
		return Resolved{}, fmt.Errorf("action resolution snapshot current generation is missing")
	}
	if err != nil {
		return Resolved{}, fmt.Errorf("read action resolution snapshot current generation: %w", err)
	}
	if !generationInfo.IsDir() || generationInfo.Mode()&os.ModeSymlink != 0 {
		return Resolved{}, fmt.Errorf("action resolution snapshot current generation is invalid")
	}
	shardPath := filepath.Dir(path)
	if err := os.Mkdir(shardPath, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return Resolved{}, fmt.Errorf("create action resolution snapshot shard: %w", err)
	}
	shardInfo, err := os.Lstat(shardPath)
	if err != nil || !shardInfo.IsDir() || shardInfo.Mode()&os.ModeSymlink != 0 {
		return Resolved{}, fmt.Errorf("action resolution snapshot shard is invalid")
	}
	unlock, err := lockMutableRefCache(ctx, path+".lock")
	if err != nil {
		return Resolved{}, err
	}
	defer unlock()
	if resolved, err, ok := loadActionResolutionSnapshotEntry(path, ref); ok {
		if err == nil || errors.As(err, new(*NotPublicError)) {
			claimed, claimErr := actionResolutionSnapshotClaimExists(path + ".claimed")
			if claimErr != nil {
				return Resolved{}, claimErr
			}
			if !claimed {
				if claimErr := storeActionResolutionSnapshotClaim(path + ".claimed"); claimErr != nil {
					return Resolved{}, claimErr
				}
			}
		}
		return resolved, err
	}
	claimed, err := actionResolutionSnapshotClaimExists(path + ".claimed")
	if err != nil {
		return Resolved{}, err
	}
	if claimed {
		return Resolved{}, fmt.Errorf("action resolution snapshot entry is missing")
	}
	if err := storeActionResolutionSnapshotClaim(path + ".claimed"); err != nil {
		return Resolved{}, err
	}
	resolved, err := resolve(ctx, ref)
	if err == nil && resolved.ResolvedRef == "" {
		resolved.ResolvedRef = resolved.Commit
	}
	entry := actionResolutionSnapshotEntry{
		Schema: actionResolutionSnapshotSchema, Owner: strings.ToLower(ref.Owner),
		Repository: strings.ToLower(ref.Repository), Ref: ref.Ref, ResolvedAt: time.Now().UTC(),
	}
	if err == nil {
		entry.Commit = resolved.Commit
		entry.ResolvedRef = resolved.ResolvedRef
	} else {
		var notPublic *NotPublicError
		if !errors.As(err, &notPublic) {
			if removeErr := os.Remove(path + ".claimed"); removeErr != nil {
				return Resolved{}, errors.Join(err, fmt.Errorf("remove unresolved action resolution snapshot claim: %w", removeErr))
			}
			return Resolved{}, err
		}
		entry.Missing = true
	}
	if storeErr := storeActionResolutionJSON(path, entry); storeErr != nil {
		if removeErr := os.Remove(path + ".claimed"); removeErr != nil {
			return Resolved{}, errors.Join(storeErr, fmt.Errorf("remove unpublished action resolution snapshot claim: %w", removeErr))
		}
		return Resolved{}, storeErr
	}
	return resolved, err
}

func actionResolutionSnapshotClaimExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read action resolution snapshot claim: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("action resolution snapshot claim is invalid")
	}
	return true, nil
}

func storeActionResolutionSnapshotClaim(path string) error {
	file, err := actionResolutionSnapshotStorage.openFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("store action resolution snapshot claim: %w", err)
	}
	if err := actionResolutionSnapshotStorage.close(file); err != nil {
		storeErr := fmt.Errorf("store action resolution snapshot claim: %w", err)
		if removeErr := os.Remove(path); removeErr != nil {
			return errors.Join(storeErr, fmt.Errorf("remove incomplete action resolution snapshot claim: %w", removeErr))
		}
		return storeErr
	}
	return nil
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
	if entry.ResolvedRef == "" {
		// Entries created before resolved refs were recorded retain their exact
		// commit but cannot grant a qualified branch or tag checkout.
		entry.ResolvedRef = entry.Commit
	}
	if !validResolvedRef(ref, entry.Commit, entry.ResolvedRef) {
		return Resolved{}, fmt.Errorf("action resolution snapshot entry is invalid"), true
	}
	return Resolved{Reference: ref, Commit: entry.Commit, ResolvedRef: entry.ResolvedRef}, nil, true
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
	temporary, err := actionResolutionSnapshotStorage.createTemp(filepath.Dir(path), ".resolution-*.tmp")
	if err != nil {
		return fmt.Errorf("create action resolution snapshot entry: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err = temporary.Chmod(0o600); err == nil {
		var written int
		written, err = actionResolutionSnapshotStorage.write(temporary, data)
		if err == nil && written != len(data) {
			err = io.ErrShortWrite
		}
	}
	if closeErr := actionResolutionSnapshotStorage.close(temporary); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write action resolution snapshot entry: %w", err)
	}
	if err := actionResolutionSnapshotStorage.rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish action resolution snapshot entry: %w", err)
	}
	return nil
}
