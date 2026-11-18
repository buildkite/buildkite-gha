package runtime

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const nodeDownloadLimit = 128 << 20
const nodeExecutableLimit = 256 << 20

func (r Runner) bundledNodeRoots() []string {
	if r.nodeRoots != nil {
		return r.nodeRoots
	}
	var roots []string
	if executable, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Join(filepath.Dir(executable), "externals"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "externals"), filepath.Join(home, "actions-runner", "externals"))
	}
	if runtime.GOOS == "windows" {
		return append(roots, `C:\actions\externals`, `C:\actions-runner\externals`)
	}
	return append(roots, "/home/runner/externals", "/opt/actions-runner/externals", "/actions-runner/externals")
}

func nodeRelease(major int, goos, goarch string) (name, member string, err error) {
	version := nodeVersion(major)
	if nodeDigest(goos, goarch, major) == "" {
		return "", "", errUnsupportedf("managed Node %d is unavailable on %s/%s", major, goos, goarch)
	}
	arch := goarch
	if arch == "amd64" {
		arch = "x64"
	}
	osName := goos
	if goos == "windows" {
		osName = "win"
	}
	name = fmt.Sprintf("node-v%s-%s-%s", version, osName, arch)
	if goos == "windows" {
		return name + ".zip", name + "/node.exe", nil
	}
	return name + ".tar.gz", name + "/bin/node", nil
}

func (r *jobRun) installNode(ctx context.Context, major int) (string, error) {
	archiveName, member, err := nodeRelease(major, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	digest := nodeDigest(runtime.GOOS, runtime.GOARCH, major)
	cacheUnavailable := func(err error) (string, error) {
		if r.NodeCacheDir == "" {
			return "", fmt.Errorf("prepare Node runtime cache: %w", err)
		}
		if r.Stderr != nil {
			_, _ = fmt.Fprintf(r.Stderr, "buildkite-gha: Node cache %q unavailable; using the user cache: %v\n", r.NodeCacheDir, err)
		}
		fallback := *r
		fallback.NodeCacheDir = ""
		return fallback.installNode(ctx, major)
	}
	root := r.NodeCacheDir
	if root == "" {
		root, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve Node runtime cache: %w", err)
		}
		root = filepath.Join(root, "buildkite-gha", "node")
	}
	// The archive name separates versions and platforms across CLI releases.
	root = filepath.Join(root, strings.TrimSuffix(strings.TrimSuffix(archiveName, ".tar.gz"), ".zip"))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return cacheUnavailable(err)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return cacheUnavailable(fmt.Errorf("node cache must be a directory, not a symlink"))
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return cacheUnavailable(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return cacheUnavailable(err)
	}
	destination := filepath.Join(root, filepath.Base(member))
	validationErr := verifyManagedNodeExecutable(ctx, major, destination, digest)
	if validationErr == nil {
		return destination, nil
	}
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	// Windows cannot replace an existing file. Discard an invalid entry before
	// downloading, not at publication where another job may have populated it.
	if runtime.GOOS == "windows" && !errors.Is(validationErr, os.ErrNotExist) {
		if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return cacheUnavailable(err)
		}
	}
	staging, err := os.MkdirTemp(root, ".download-")
	if err != nil {
		return cacheUnavailable(err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	url := fmt.Sprintf("https://nodejs.org/dist/v%s/%s", nodeVersion(major), archiveName)
	if r.nodeDownloadURL != "" {
		url = r.nodeDownloadURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "buildkite-gha")
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return errors.New("invalid Node download redirect")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download Node %d: %w", major, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Node %d: HTTP %s", major, response.Status)
	}
	archive := filepath.Join(staging, archiveName)
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	size, copyErr := io.Copy(file, io.LimitReader(response.Body, nodeDownloadLimit+1))
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", fmt.Errorf("download Node archive: %w", err)
	}
	if size > nodeDownloadLimit {
		return "", fmt.Errorf("node archive exceeds %d bytes", nodeDownloadLimit)
	}
	staged := filepath.Join(staging, filepath.Base(member))
	if err := extractNode(archive, member, staged, digest); err != nil {
		return "", err
	}
	if err := verifyManagedNodeExecutable(ctx, major, staged, digest); err != nil {
		return "", err
	}
	if err := os.Rename(staged, destination); err != nil {
		// Another self-hosted job may have published the same version first.
		if verifyErr := verifyManagedNodeExecutable(ctx, major, destination, digest); verifyErr == nil {
			return destination, nil
		}
		return "", fmt.Errorf("publish Node runtime: %w", err)
	}
	return destination, nil
}

// Extract only the executable, not arbitrary archive paths or symlinks. The
// pinned executable hash is checked before any downloaded code is invoked.
func extractNode(archive, member, destination, digest string) error {
	write := func(input io.Reader, size int64) error {
		if size <= 0 || size > nodeExecutableLimit {
			return fmt.Errorf("node executable has invalid size %d", size)
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
		if err != nil {
			return err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, size+1))
		if err := errors.Join(copyErr, output.Close()); err != nil {
			return err
		}
		if written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
			return errors.New("node executable checksum mismatch")
		}
		return nil
	}
	if strings.HasSuffix(archive, ".zip") {
		reader, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()
		for _, file := range reader.File {
			if file.Name != member || !file.Mode().IsRegular() {
				continue
			}
			input, err := file.Open()
			if err != nil {
				return err
			}
			defer func() { _ = input.Close() }()
			return write(input, int64(file.UncompressedSize64))
		}
	} else {
		file, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer func() { _ = compressed.Close() }()
		reader := tar.NewReader(compressed)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if header.Name == member && header.Typeflag == tar.TypeReg {
				return write(reader, header.Size)
			}
		}
	}
	return fmt.Errorf("node archive does not contain regular file %s", member)
}
