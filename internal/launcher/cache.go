package launcher

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func (c config) cacheRoot() (string, bool) {
	if root := c.getenv("BUILDKITE_GITHUB_ACTIONS_PLUGIN_CACHE_ROOT"); root != "" {
		return root, true
	}
	if c.getenv("BUILDKITE_COMPUTE_TYPE") == "hosted" {
		base := c.getenv("MISE_HOSTED_CACHE_VOLUME_ROOT")
		if base == "" {
			base = "/cache/bkcache"
		}
		if st, err := os.Stat(base); err == nil && st.IsDir() {
			return filepath.Join(base, "github-actions-buildkite-plugin"), true
		}
	}
	if v := c.getenv("BUILDKITE_AGENT_DATA_PATH"); v != "" {
		return filepath.Join(v, "cache/github-actions-buildkite-plugin"), true
	}
	if v := c.getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "buildkite/github-actions-buildkite-plugin"), true
	}
	if v := c.getenv("HOME"); v != "" {
		return filepath.Join(v, ".cache/buildkite/github-actions-buildkite-plugin"), true
	}
	return "", false
}

func openCacheRoot(root string) (*os.Root, error) {
	for p := root; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("cache path contains symlink %s", p)
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	before, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	cache, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	after, err := cache.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = cache.Close()
		return nil, fmt.Errorf("cache root changed while opening")
	}
	probeName, err := randomCacheName(".write-")
	if err != nil {
		_ = cache.Close()
		return nil, err
	}
	probe, err := cache.OpenFile(probeName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = cache.Close()
		return nil, err
	}
	closeErr := probe.Close()
	removeErr := cache.Remove(probeName)
	if closeErr != nil || removeErr != nil {
		_ = cache.Close()
		return nil, fmt.Errorf("verify cache writes: %v", errors.Join(closeErr, removeErr))
	}
	return cache, nil
}

func (c config) privateArchive(cache *os.Root, tag, want, entry string) (string, bool, error) {
	private, err := os.CreateTemp(c.tempDir, "buildkite-gha-archive-")
	if err != nil {
		return "", false, err
	}
	name := private.Name()
	defer func() {
		if private != nil {
			_ = private.Close()
		}
	}()
	cacheHit := false
	if src, openErr := cache.OpenFile(entry, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0); openErr == nil {
		info, statErr := src.Stat()
		if statErr == nil && info.Mode().IsRegular() {
			err = copyAndVerify(private, src, want)
			cacheHit = err == nil
		}
		_ = src.Close()
	}
	if cacheHit {
		if closeErr := private.Close(); closeErr != nil {
			_ = os.Remove(name)
			return "", false, closeErr
		}
		private = nil
		_ = os.Chmod(name, 0600)
		return name, false, nil
	}
	if err := private.Truncate(0); err != nil {
		_ = os.Remove(name)
		return "", false, err
	}
	if _, err := private.Seek(0, 0); err != nil {
		_ = os.Remove(name)
		return "", false, err
	}
	b, fetchErr := c.fetch(tag, archiveName, maxArchive)
	if fetchErr != nil {
		_ = os.Remove(name)
		return "", false, fmt.Errorf("download archive: %w", fetchErr)
	}
	if _, err = private.Write(b); err != nil {
		_ = os.Remove(name)
		return "", false, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(b)) != want {
		_ = os.Remove(name)
		return "", false, fmt.Errorf("archive checksum mismatch")
	}
	if err := private.Close(); err != nil {
		_ = os.Remove(name)
		return "", false, err
	}
	private = nil
	_ = os.Chmod(name, 0600)
	return name, true, nil
}

func copyAndVerify(destination *os.File, source io.Reader, want string) error {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, maxArchive+1))
	if err != nil {
		return err
	}
	if written > maxArchive || hex.EncodeToString(hash.Sum(nil)) != want {
		return fmt.Errorf("cache checksum verification failed")
	}
	return nil
}

func publish(cache *os.Root, source, entry string) error {
	if err := cache.MkdirAll(filepath.ToSlash(filepath.Dir(entry)), 0700); err != nil {
		return err
	}
	stageName, err := randomCacheName(".stage-")
	if err != nil {
		return err
	}
	stage, err := cache.OpenFile(stageName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = cache.Remove(stageName) }()
	src, err := os.Open(source)
	if err != nil {
		_ = stage.Close()
		return err
	}
	_, copyErr := io.Copy(stage, io.LimitReader(src, maxArchive+1))
	_ = src.Close()
	closeErr := stage.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return cache.Rename(stageName, entry)
}

func randomCacheName(prefix string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random), nil
}

func extract(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	limited := &io.LimitedReader{R: gz, N: maxArchive + 1}
	tr := tar.NewReader(limited)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Name != "LICENSE" && h.Name != "buildkite-gha" {
			return fmt.Errorf("unexpected archive entry %q", h.Name)
		}
		if seen[h.Name] || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxArchive {
			return fmt.Errorf("invalid archive entry %q", h.Name)
		}
		seen[h.Name] = true
		mode := os.FileMode(0644)
		if h.Name == "buildkite-gha" {
			mode = 0700
		}
		out, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, tr, h.Size)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.Chmod(filepath.Join(dir, h.Name), mode); err != nil {
			return err
		}
	}
	if !seen["LICENSE"] || !seen["buildkite-gha"] {
		return fmt.Errorf("archive must contain LICENSE and buildkite-gha exactly once")
	}
	trailing, err := io.Copy(io.Discard, limited)
	if err != nil {
		return fmt.Errorf("finish archive decompression: %w", err)
	}
	if trailing != 0 || limited.N == 0 {
		return fmt.Errorf("archive contains trailing data or exceeds extraction limit")
	}
	return nil
}
