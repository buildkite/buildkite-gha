package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

const (
	maxHashFilePatterns          = 255
	maxHashFilePatternBytes      = 1024
	maxHashFilePatternBytesTotal = 64 << 10
	maxHashFileMatches           = 10_000
	maxHashFileBytes             = 1 << 30
	maxHashFileEntries           = 100_000
)

type hashFilesLimits struct {
	patterns            int
	patternBytes        int
	totalPatternBytes   int
	matches             int
	bytes               int64
	entries             int
	beforeOpen          func(string)
	beforeDirectoryOpen func(string)
}

var defaultHashFilesLimits = hashFilesLimits{
	patterns: maxHashFilePatterns, patternBytes: maxHashFilePatternBytes,
	totalPatternBytes: maxHashFilePatternBytesTotal, matches: maxHashFileMatches,
	bytes: maxHashFileBytes, entries: maxHashFileEntries,
}

type hashFilePattern struct {
	pattern       string
	directory     string
	negative      bool
	directoryOnly bool
}

func hashWorkspaceFiles(ctx context.Context, workspace string, patterns []string) (digest string, retErr error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", fmt.Errorf("open hashFiles workspace: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	return hashWorkspaceRootFilesWithLimits(ctx, root, patterns, defaultHashFilesLimits, runtime.GOOS == "windows")
}

func hashWorkspaceFilesWithLimits(ctx context.Context, workspace string, sources []string, limits hashFilesLimits, caseInsensitive bool) (digest string, retErr error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", fmt.Errorf("open hashFiles workspace: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	return hashWorkspaceRootFilesWithLimits(ctx, root, sources, limits, caseInsensitive)
}

func hashWorkspaceRootFilesWithLimits(ctx context.Context, root *os.Root, sources []string, limits hashFilesLimits, caseInsensitive bool) (string, error) {
	patterns, err := parseHashFilePatterns(sources, limits)
	if err != nil {
		return "", err
	}
	var selected []string
	directories := make(map[string]fs.FileInfo)
	err = walkHashFilesRoot(ctx, root, limits.entries, limits.beforeDirectoryOpen, directories, func(name string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		matched, err := hashFilePatternMatch(patterns, name, caseInsensitive)
		if err != nil {
			return err
		}
		if !matched || entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("hashFiles matched symlink %q; symlinks are unsupported", name)
		}
		if !entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("inspect hashFiles match %q: %w", name, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("hashFiles matched non-regular file %q", name)
			}
		}
		selected = append(selected, name)
		if len(selected) > limits.matches {
			return fmt.Errorf("hashFiles matched more than %d files", limits.matches)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(selected) == 0 {
		return "", nil
	}
	sort.Slice(selected, func(i, j int) bool {
		if caseInsensitive {
			left, right := strings.ToLower(selected[i]), strings.ToLower(selected[j])
			if left != right {
				return left < right
			}
		}
		return selected[i] < selected[j]
	})

	combined := sha256.New()
	var total int64
	for _, name := range selected {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := verifyHashFileDirectories(root, directories, name); err != nil {
			return "", err
		}
		before, err := root.Lstat(name)
		if err != nil {
			return "", fmt.Errorf("inspect hashFiles match %q: %w", name, err)
		}
		if before.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("hashFiles matched symlink %q; symlinks are unsupported", name)
		}
		if !before.Mode().IsRegular() {
			return "", fmt.Errorf("hashFiles matched non-regular file %q", name)
		}
		if before.Size() < 0 || before.Size() > limits.bytes-total {
			return "", fmt.Errorf("hashFiles selected bytes exceed %d", limits.bytes)
		}
		if limits.beforeOpen != nil {
			limits.beforeOpen(name)
		}
		file, err := root.Open(name)
		if err != nil {
			return "", fmt.Errorf("open hashFiles match %q: %w", name, err)
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
			_ = file.Close()
			return "", fmt.Errorf("hashFiles match %q changed before hashing", name)
		}
		fileHash := sha256.New()
		read, copyErr := copyHashFile(ctx, fileHash, io.LimitReader(file, limits.bytes-total+1))
		closeErr := file.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash hashFiles match %q: %w", name, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close hashFiles match %q: %w", name, closeErr)
		}
		if read > limits.bytes-total {
			return "", fmt.Errorf("hashFiles selected bytes exceed %d", limits.bytes)
		}
		after, err := root.Lstat(name)
		if err != nil || !os.SameFile(before, after) || read != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			return "", fmt.Errorf("hashFiles match %q changed while hashing", name)
		}
		total += read
		_, _ = combined.Write(fileHash.Sum(nil))
	}
	if err := verifyHashFileDirectories(root, directories, ""); err != nil {
		return "", err
	}
	return hex.EncodeToString(combined.Sum(nil)), nil
}

func walkHashFilesRoot(ctx context.Context, root *os.Root, limit int, beforeOpen func(string), directories map[string]fs.FileInfo, visit func(string, fs.DirEntry) error) error {
	const readBatch = 256
	entriesRead := 0
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := root.Lstat(directory)
		if err != nil {
			return fmt.Errorf("inspect hashFiles directory %q: %w", directory, err)
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			return fmt.Errorf("hashFiles directory %q changed before traversal", directory)
		}
		if beforeOpen != nil {
			beforeOpen(directory)
		}
		handle, err := root.Open(directory)
		if err != nil {
			return fmt.Errorf("open hashFiles directory %q: %w", directory, err)
		}
		opened, err := handle.Stat()
		if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
			_ = handle.Close()
			return fmt.Errorf("hashFiles directory %q changed before traversal", directory)
		}
		var entries []fs.DirEntry
		for {
			batch, readErr := handle.ReadDir(readBatch)
			for _, entry := range batch {
				entriesRead++
				if entriesRead > limit {
					return errors.Join(fmt.Errorf("hashFiles workspace has more than %d entries", limit), handle.Close())
				}
				entries = append(entries, entry)
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return errors.Join(fmt.Errorf("read hashFiles directory %q: %w", directory, readErr), handle.Close())
			}
			if err := ctx.Err(); err != nil {
				return errors.Join(err, handle.Close())
			}
		}
		if err := handle.Close(); err != nil {
			return fmt.Errorf("close hashFiles directory %q: %w", directory, err)
		}
		after, err := root.Lstat(directory)
		if err != nil || !sameHashFileInfo(before, after) {
			return fmt.Errorf("hashFiles directory %q changed during traversal", directory)
		}
		directories[directory] = before
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			name := path.Join(directory, entry.Name())
			if err := visit(name, entry); err != nil {
				return err
			}
			if entry.IsDir() {
				if err := walk(name); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(".")
}

func verifyHashFileDirectories(root *os.Root, directories map[string]fs.FileInfo, name string) error {
	if name == "" {
		for directory, before := range directories {
			after, err := root.Lstat(directory)
			if err != nil || !sameHashFileInfo(before, after) {
				return fmt.Errorf("hashFiles directory %q changed after traversal", directory)
			}
		}
		return nil
	}
	for directory := path.Dir(name); ; directory = path.Dir(directory) {
		before, ok := directories[directory]
		if !ok {
			return fmt.Errorf("hashFiles directory %q was not traversed", directory)
		}
		after, err := root.Lstat(directory)
		if err != nil || !sameHashFileInfo(before, after) {
			return fmt.Errorf("hashFiles directory %q changed after traversal", directory)
		}
		if directory == "." {
			return nil
		}
	}
}

func sameHashFileInfo(before, after fs.FileInfo) bool {
	return after != nil && os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func copyHashFile(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64<<10)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, ctx.Err()
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func parseHashFilePatterns(sources []string, limits hashFilesLimits) ([]hashFilePattern, error) {
	if len(sources) == 0 || len(sources) > limits.patterns {
		return nil, fmt.Errorf("hashFiles requires 1 to %d patterns", limits.patterns)
	}
	patterns := make([]hashFilePattern, 0, len(sources))
	total := 0
	for i, source := range sources {
		if len(source) > limits.patternBytes {
			return nil, fmt.Errorf("hashFiles pattern %d exceeds %d bytes", i+1, limits.patternBytes)
		}
		total += len(source)
		if total > limits.totalPatternBytes {
			return nil, fmt.Errorf("hashFiles patterns exceed %d total bytes", limits.totalPatternBytes)
		}
		for _, value := range []byte(source) {
			if value < 0x20 || value == 0x7f {
				return nil, fmt.Errorf("hashFiles pattern %d contains a forbidden control character", i+1)
			}
		}
		pattern := strings.TrimSpace(source)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		negative := false
		for strings.HasPrefix(pattern, "!") {
			negative = !negative
			pattern = strings.TrimSpace(pattern[1:])
		}
		if runtime.GOOS == "windows" {
			pattern = filepath.ToSlash(pattern)
		}
		if pattern == "" || path.IsAbs(pattern) || filepath.IsAbs(pattern) || filepath.VolumeName(pattern) != "" || strings.HasPrefix(pattern, "//") || windowsVolumePattern(pattern) {
			return nil, fmt.Errorf("hashFiles pattern %d must be relative to the workspace", i+1)
		}
		segments := strings.Split(pattern, "/")
		for j, segment := range segments {
			if segment == ".." || segment == "." && j != 0 {
				return nil, fmt.Errorf("hashFiles pattern %d may not contain %q", i+1, segment)
			}
		}
		if pattern == "." || pattern == "./" {
			pattern = "**"
		}
		directoryOnly := strings.HasSuffix(pattern, "/")
		pattern = strings.TrimPrefix(pattern, "./")
		pattern = strings.TrimSuffix(pattern, "/")
		if pattern == "" {
			pattern = "**"
			directoryOnly = false
		}
		pattern = escapeHashFileBraces(pattern)
		if !doublestar.ValidatePattern(pattern) {
			return nil, fmt.Errorf("hashFiles pattern %d is invalid", i+1)
		}
		directory := pattern
		if directoryOnly {
			pattern += "/**"
		}
		patterns = append(patterns, hashFilePattern{pattern: pattern, directory: directory, negative: negative, directoryOnly: directoryOnly})
	}
	return patterns, nil
}

func windowsVolumePattern(pattern string) bool {
	return len(pattern) >= 2 && ((pattern[0] >= 'a' && pattern[0] <= 'z') || (pattern[0] >= 'A' && pattern[0] <= 'Z')) && pattern[1] == ':'
}

func hashFilePatternMatch(patterns []hashFilePattern, name string, caseInsensitive bool) (bool, error) {
	matched := false
	for _, pattern := range patterns {
		candidatePattern, candidateName := pattern.pattern, name
		if caseInsensitive {
			candidatePattern, candidateName = strings.ToLower(candidatePattern), strings.ToLower(candidateName)
		}
		patternMatched := false
		for candidate := candidateName; candidate != "."; candidate = path.Dir(candidate) {
			patternMatched, _ = doublestar.Match(candidatePattern, candidate)
			if patternMatched && pattern.directoryOnly {
				directory := pattern.directory
				if caseInsensitive {
					directory = strings.ToLower(directory)
				}
				patternMatched, _ = doublestar.Match(directory, candidate)
				patternMatched = !patternMatched
			}
			if patternMatched {
				break
			}
			if pattern.directoryOnly {
				break
			}
		}
		if patternMatched {
			matched = !pattern.negative
		}
	}
	return matched, nil
}

func escapeHashFileBraces(pattern string) string {
	var escaped strings.Builder
	for i, r := range pattern {
		if (r == '{' || r == '}') && (i == 0 || pattern[i-1] != '\\') {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}
