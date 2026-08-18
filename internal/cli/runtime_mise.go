package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
)

const (
	runtimeMiseArchiveDigest            = "bd0930c0b619f51ddb60e32e5cce18a5533567b2f1ba9fc4875b9f39a2bb3ed8"
	runtimeMiseBinaryDigest             = "a238972a3162d710b85b28c324372e96ca4e4b486c81fe78695000d9fbc77c48"
	runtimeMiseDarwinARM64ArchiveDigest = "5b883c868a0748dd0c595d30fd000ec5138dfabdeef2c30222866ebf34af1ae3"
	runtimeMiseDarwinARM64BinaryDigest  = "e777070540ffe22cf8b2b9f88aed88b461d0887d940c4f1c1a97359463cde6e1"
	runtimeMiseArchiveLimit             = 64 << 20
	runtimeMiseBinaryLimit              = 128 << 20
)

func resolveRuntimeMise(ctx context.Context, configured, dataDir, privateRuntime string, stderr io.Writer) (string, error) {
	return resolveRuntimeMiseWithInstaller(ctx, configured, dataDir, privateRuntime, stderr, installRuntimeMise)
}

func resolveRuntimeMiseWithInstaller(ctx context.Context, configured, dataDir, privateRuntime string, stderr io.Writer, install func(context.Context, string, string, io.Writer) (string, error)) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("BUILDKITE_GHA_MISE must be an absolute path")
		}
		return validateRuntimeMise(ctx, configured, "")
	}
	if candidate, err := exec.LookPath("mise"); err == nil {
		if resolved, validationErr := validateRuntimeMise(ctx, candidate, ""); validationErr == nil {
			return resolved, nil
		}
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise on PATH is incompatible with minimum version %s; using the managed runtime copy\n", buildkitepipeline.MinimumMiseVersion)
	}
	return install(ctx, dataDir, privateRuntime, stderr)
}

func validateRuntimeMise(ctx context.Context, candidate, expectedDigest string) (string, error) {
	resolved, err := validateRuntimeMiseFile(ctx, candidate, expectedDigest)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, resolved, "--version")
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "MISE_") {
			command.Env = append(command.Env, value)
		}
	}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("validate runtime mise executable: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", fmt.Errorf("validate runtime mise executable: empty version")
	}
	reported := fields[0]
	if reported == "mise" && len(fields) > 1 {
		reported = fields[1]
	}
	if !miseVersionAtLeast(reported, buildkitepipeline.MinimumMiseVersion) {
		return "", fmt.Errorf("runtime mise executable reported version %q, want %q or newer", reported, buildkitepipeline.MinimumMiseVersion)
	}
	return resolved, nil
}

func validateRuntimeMiseFile(ctx context.Context, candidate, expectedDigest string) (string, error) {
	if !filepath.IsAbs(candidate) {
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			return "", fmt.Errorf("resolve runtime mise path: %w", err)
		}
		candidate = absolute
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve runtime mise executable: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect runtime mise executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("runtime mise executable %q is not an executable regular file", resolved)
	}
	if expectedDigest != "" {
		if info.Size() > runtimeMiseBinaryLimit {
			return "", fmt.Errorf("runtime mise executable exceeds %d-byte limit", runtimeMiseBinaryLimit)
		}
		actual, err := fileSHA256(ctx, resolved, runtimeMiseBinaryLimit)
		if err != nil {
			return "", fmt.Errorf("hash runtime mise executable: %w", err)
		}
		if actual != expectedDigest {
			return "", fmt.Errorf("runtime mise executable checksum mismatch")
		}
	}
	return resolved, nil
}

func miseVersionAtLeast(actual, minimum string) bool {
	parse := func(value string) ([3]int, bool) {
		var parsed [3]int
		parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
		if len(parts) != len(parsed) {
			return parsed, false
		}
		for index, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil || number < 0 {
				return parsed, false
			}
			parsed[index] = number
		}
		return parsed, true
	}
	got, ok := parse(actual)
	if !ok {
		return false
	}
	want, ok := parse(minimum)
	if !ok {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return got[index] > want[index]
		}
	}
	return true
}

type runtimeMiseRelease struct {
	asset         string
	cacheKey      string
	archiveDigest string
	binaryDigest  string
}

func selectRuntimeMiseRelease(goos, goarch string) (runtimeMiseRelease, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return runtimeMiseRelease{
			asset:         "linux-x64",
			cacheKey:      "linux-amd64",
			archiveDigest: runtimeMiseArchiveDigest,
			binaryDigest:  runtimeMiseBinaryDigest,
		}, nil
	case "darwin/arm64":
		return runtimeMiseRelease{
			asset:         "macos-arm64",
			cacheKey:      "darwin-arm64",
			archiveDigest: runtimeMiseDarwinARM64ArchiveDigest,
			binaryDigest:  runtimeMiseDarwinARM64BinaryDigest,
		}, nil
	default:
		return runtimeMiseRelease{}, fmt.Errorf("managed mise is unavailable on %s/%s; set BUILDKITE_GHA_MISE to a compatible absolute path", goos, goarch)
	}
}

func installRuntimeMise(ctx context.Context, dataDir, privateRuntime string, stderr io.Writer) (string, error) {
	selected, err := selectRuntimeMiseRelease(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	root := dataDir
	if root == "" {
		root, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve mise runtime cache: %w", err)
		}
		root = filepath.Join(root, "buildkite-gha", "mise", buildkitepipeline.MinimumMiseVersion)
	} else {
		root = filepath.Join(filepath.Dir(root), "runtime", buildkitepipeline.MinimumMiseVersion)
	}
	destination := filepath.Join(root, selected.cacheKey, "mise")
	if resolved, err := validateRuntimeMiseFile(ctx, destination, selected.binaryDigest); err == nil {
		return pinRuntimeMise(ctx, resolved, privateRuntime, selected.binaryDigest)
	}
	_, _ = fmt.Fprintf(stderr, "~~~ :mise: Install mise %s\n", buildkitepipeline.MinimumMiseVersion)
	url := fmt.Sprintf("https://github.com/jdx/mise/releases/download/v%s/mise-v%s-%s.tar.gz", buildkitepipeline.MinimumMiseVersion, buildkitepipeline.MinimumMiseVersion, selected.asset)
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("mise download redirected away from HTTPS")
			}
			if len(via) >= 10 {
				return errors.New("too many mise download redirects")
			}
			return nil
		},
	}
	cached, err := installRuntimeMiseFromPlatform(ctx, root, selected.cacheKey, client, url, selected.archiveDigest, selected.binaryDigest)
	if err != nil {
		return "", err
	}
	return pinRuntimeMise(ctx, cached, privateRuntime, selected.binaryDigest)
}

func pinRuntimeMise(ctx context.Context, cached, privateRuntime, expectedDigest string) (string, error) {
	if privateRuntime == "" {
		return "", fmt.Errorf("private action runtime directory is required")
	}
	resolvedRoot, err := canonicalNonSymlinkDirectory(privateRuntime)
	if err != nil {
		return "", fmt.Errorf("private action runtime directory contains a symlink")
	}
	source, err := os.Open(cached)
	if err != nil {
		return "", fmt.Errorf("open cached mise executable: %w", err)
	}
	defer func() { _ = source.Close() }()
	destination := filepath.Join(resolvedRoot, "mise")
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		return "", fmt.Errorf("create private mise executable: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(source, runtimeMiseBinaryLimit+1))
	closeErr := output.Close()
	if copyErr != nil {
		return "", fmt.Errorf("copy private mise executable: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("write private mise executable: %w", closeErr)
	}
	if written > runtimeMiseBinaryLimit {
		return "", fmt.Errorf("cached mise executable exceeds %d-byte limit", runtimeMiseBinaryLimit)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return "", fmt.Errorf("cached mise executable checksum verification failed")
	}
	resolved, err := validateRuntimeMise(ctx, destination, expectedDigest)
	if err != nil {
		return "", fmt.Errorf("validate private mise executable: %w", err)
	}
	return resolved, nil
}

func installRuntimeMiseFrom(ctx context.Context, root string, client *http.Client, sourceURL, archiveDigest, binaryDigest string) (string, error) {
	return installRuntimeMiseFromPlatform(ctx, root, "linux-x64", client, sourceURL, archiveDigest, binaryDigest)
}

func installRuntimeMiseFromPlatform(ctx context.Context, root, cacheKey string, client *http.Client, sourceURL, archiveDigest, binaryDigest string) (string, error) {
	destinationDir := filepath.Join(root, cacheKey)
	parent := filepath.Dir(destinationDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create mise runtime cache: %w", err)
	}
	resolvedParent, err := canonicalNonSymlinkDirectory(parent)
	if err != nil {
		return "", fmt.Errorf("mise runtime cache contains a symlink")
	}
	destinationDir = filepath.Join(resolvedParent, cacheKey)
	destination := filepath.Join(destinationDir, "mise")
	if resolved, err := validateRuntimeMiseFile(ctx, destination, binaryDigest); err == nil {
		return resolved, nil
	}
	staging, err := os.MkdirTemp(resolvedParent, "."+cacheKey+".")
	if err != nil {
		return "", fmt.Errorf("stage mise runtime: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	archive := filepath.Join(staging, "mise.tar.gz")
	if err := downloadRuntimeMise(ctx, client, sourceURL, archive, archiveDigest); err != nil {
		return "", err
	}
	stagedExecutable := filepath.Join(staging, "mise")
	if err := extractRuntimeMise(archive, stagedExecutable, binaryDigest); err != nil {
		return "", err
	}
	if _, err := validateRuntimeMiseFile(ctx, stagedExecutable, binaryDigest); err != nil {
		return "", fmt.Errorf("validate downloaded mise executable: %w", err)
	}
	if err := os.Remove(archive); err != nil {
		return "", fmt.Errorf("remove staged mise archive: %w", err)
	}
	if _, err := os.Lstat(destinationDir); err == nil {
		if resolved, validationErr := validateRuntimeMiseFile(ctx, destination, binaryDigest); validationErr == nil {
			return resolved, nil
		}
		invalid := destinationDir + fmt.Sprintf(".invalid-%d", time.Now().UnixNano())
		if err := os.Rename(destinationDir, invalid); err == nil {
			_ = os.RemoveAll(invalid)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect mise runtime cache: %w", err)
	}
	if err := os.Rename(staging, destinationDir); err != nil {
		if resolved, validationErr := validateRuntimeMiseFile(ctx, destination, binaryDigest); validationErr == nil {
			return resolved, nil
		}
		return "", fmt.Errorf("publish mise runtime cache: %w", err)
	}
	staging = ""
	resolved, err := validateRuntimeMiseFile(ctx, destination, binaryDigest)
	if err != nil {
		return "", fmt.Errorf("validate installed mise cache: %w", err)
	}
	return resolved, nil
}

func downloadRuntimeMise(ctx context.Context, client *http.Client, sourceURL, destination, expectedDigest string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("create mise download request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download mise: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download mise: unexpected HTTP status %s", response.Status)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create mise archive: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, runtimeMiseArchiveLimit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download mise archive: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("write mise archive: %w", closeErr)
	}
	if written > runtimeMiseArchiveLimit {
		return fmt.Errorf("download mise archive: exceeds %d-byte limit", runtimeMiseArchiveLimit)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return fmt.Errorf("mise archive checksum verification failed")
	}
	return nil
}

func extractRuntimeMise(archive, destination, expectedDigest string) error {
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open mise archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open mise gzip archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	found := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read mise archive: %w", err)
		}
		if strings.TrimPrefix(filepath.ToSlash(header.Name), "./") != "mise/bin/mise" {
			continue
		}
		if found {
			return fmt.Errorf("mise archive contains duplicate executable")
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > runtimeMiseBinaryLimit {
			return fmt.Errorf("mise archive executable is not a bounded regular file")
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
		if err != nil {
			return fmt.Errorf("create mise executable: %w", err)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), tarReader)
		closeErr := output.Close()
		if copyErr != nil {
			return fmt.Errorf("extract mise executable: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("write mise executable: %w", closeErr)
		}
		if written != header.Size || hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
			return fmt.Errorf("mise executable checksum verification failed")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("mise archive does not contain mise/bin/mise")
	}
	return nil
}

func fileSHA256(ctx context.Context, path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var read int64
	for read <= limit {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk := buffer
		if remaining := limit + 1 - read; remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}
		n, readErr := file.Read(chunk)
		if n > 0 {
			_, _ = hash.Write(chunk[:n])
			read += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if read > limit {
		return "", fmt.Errorf("exceeds %d-byte limit", limit)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func prepareMiseDataDir(path string, stderr io.Writer) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise cache %q is invalid; using the ephemeral agent cache: %v\n", path, err)
		return ""
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise cache %q is unavailable; using the ephemeral agent cache: %v\n", path, err)
		return ""
	}
	resolved, err := canonicalNonSymlinkDirectory(absolute)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise cache %q is not a real directory; using the ephemeral agent cache\n", path)
		return ""
	}
	return resolved
}

func canonicalNonSymlinkDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("not a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	canonicalInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(info, canonicalInfo) {
		return "", fmt.Errorf("directory changed while canonicalizing")
	}
	return resolved, nil
}
