package cli

import (
	"archive/tar"
	"bytes"
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
	"regexp"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/useragent"
)

const (
	pluginDevDarwinRuntimeEnvironment = "BUILDKITE_GHA_PLUGIN_DEV_DARWIN_RUNTIME"
	pluginDevLinuxRuntimeEnvironment  = "BUILDKITE_GHA_PLUGIN_DEV_LINUX_RUNTIME"
	pluginChecksumLimit               = 4 << 20
	pluginArchiveLimit                = 256 << 20
)

var stableVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

var pluginReleaseBaseURL = "https://github.com/buildkite/buildkite-gha/releases/download"

var pluginHTTPClient = securePluginHTTPClient()

type pluginRuntimeAcquisition struct {
	version string
}

const (
	pluginLinuxAsset  = "buildkite-gha_Linux_x86_64.tar.gz"
	pluginDarwinAsset = "buildkite-gha_Darwin_arm64.tar.gz"
)

func securePluginHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("release download redirected away from HTTPS")
			}
			if len(via) >= 10 {
				return errors.New("too many release download redirects")
			}
			return nil
		},
	}
}

func (a *pluginRuntimeAcquisition) acquire(ctx context.Context, required map[compiler.Platform]bool, hostPlatform compiler.Platform, host runtimeDistribution) (map[compiler.Platform]runtimeDistribution, error) {
	distributions := make(map[compiler.Platform]runtimeDistribution, len(required))
	devPaths := map[compiler.Platform]string{
		compiler.PlatformLinuxAMD64:  os.Getenv(pluginDevLinuxRuntimeEnvironment),
		compiler.PlatformDarwinARM64: os.Getenv(pluginDevDarwinRuntimeEnvironment),
	}
	if a.version != "dev" {
		for platform, path := range devPaths {
			if path != "" {
				return nil, fmt.Errorf("%s is test/dev-only and is rejected by release builds", pluginDevRuntimeEnvironment(platform))
			}
		}
	}
	for _, platform := range []compiler.Platform{compiler.PlatformLinuxAMD64, compiler.PlatformDarwinARM64} {
		if !required[platform] {
			continue
		}
		if platform == hostPlatform {
			distributions[platform] = host
			continue
		}
		if a.version == "dev" {
			path := devPaths[platform]
			if path == "" {
				return nil, fmt.Errorf("%s is required for a dev build using %s", pluginDevRuntimeEnvironment(platform), platform)
			}
			loaded, err := loadRuntimeDistributions(map[compiler.Platform]string{platform: path})
			if err != nil {
				return nil, err
			}
			distributions[platform] = loaded[platform]
			continue
		}
		if !stableVersionPattern.MatchString(a.version) {
			return nil, fmt.Errorf("running version %q is not a stable semantic version", a.version)
		}
		distribution, err := acquirePluginRuntime(ctx, a.version, platform, pluginHTTPClient, pluginReleaseBaseURL, pluginArchiveCachePath(a.version, pluginRuntimeAsset(platform)))
		if err != nil {
			return nil, err
		}
		distributions[platform] = distribution
	}
	return distributions, nil
}

func pluginDevRuntimeEnvironment(platform compiler.Platform) string {
	if platform == compiler.PlatformDarwinARM64 {
		return pluginDevDarwinRuntimeEnvironment
	}
	return pluginDevLinuxRuntimeEnvironment
}

func pluginRuntimeAsset(platform compiler.Platform) string {
	if platform == compiler.PlatformDarwinARM64 {
		return pluginDarwinAsset
	}
	return pluginLinuxAsset
}

func acquirePluginRuntime(ctx context.Context, version string, platform compiler.Platform, client *http.Client, baseURL, cachePath string) (runtimeDistribution, error) {
	asset := pluginRuntimeAsset(platform)
	checksums, err := downloadPluginReleaseFile(ctx, client, baseURL+"/v"+version+"/checksums.txt", version, pluginChecksumLimit)
	if err != nil {
		return runtimeDistribution{}, fmt.Errorf("download release checksums: %w", err)
	}
	expected, err := pluginAssetChecksum(checksums, asset)
	if err != nil {
		return runtimeDistribution{}, err
	}
	archive := readVerifiedPluginArchiveCache(cachePath, expected)
	if archive == nil {
		archive, err = downloadPluginReleaseFile(ctx, client, baseURL+"/v"+version+"/"+asset, version, pluginArchiveLimit)
		if err != nil {
			return runtimeDistribution{}, fmt.Errorf("download %s runtime: %w", platform, err)
		}
		if sha256.Sum256(archive) != expected {
			return runtimeDistribution{}, fmt.Errorf("%s archive checksum verification failed", platform)
		}
		writePluginArchiveCache(cachePath, archive)
	}
	contents, err := extractPluginRuntime(archive, platform)
	if err != nil {
		return runtimeDistribution{}, err
	}
	if err := validateRuntimeDistributionBinary(platform, contents); err != nil {
		return runtimeDistribution{}, fmt.Errorf("validate %s runtime: %w", platform, err)
	}
	digest := sha256.Sum256(contents)
	return runtimeDistribution{contents: contents, digest: fmt.Sprintf("sha256:%x", digest)}, nil
}

func downloadPluginReleaseFile(ctx context.Context, client *http.Client, source, version string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil || request.URL.Scheme != "https" {
		return nil, fmt.Errorf("release URL must use HTTPS")
	}
	request.Header.Set("User-Agent", useragent.FromVersion(version))
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("exceeds %d-byte limit", limit)
	}
	return contents, nil
}

func pluginAssetChecksum(contents []byte, asset string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	matches := 0
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return result, fmt.Errorf("invalid checksum entry for %s", asset)
		}
		copy(result[:], decoded)
		matches++
	}
	if matches != 1 {
		return result, fmt.Errorf("checksums must contain exactly one valid entry for %s", asset)
	}
	return result, nil
}

func extractPluginRuntime(archive []byte, platform compiler.Platform) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open %s archive: %w", platform, err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	seenBinary, seenLicense := false, false
	var executable []byte
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s archive: %w", platform, err)
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > runtimeDistributionLimit {
			return nil, fmt.Errorf("%s archive contains an unsafe member %q", platform, header.Name)
		}
		switch header.Name {
		case "buildkite-gha":
			if seenBinary || header.Mode&0o111 == 0 {
				return nil, fmt.Errorf("%s archive has an invalid executable member", platform)
			}
			seenBinary = true
			executable, err = io.ReadAll(io.LimitReader(tarReader, runtimeDistributionLimit+1))
			if err != nil || int64(len(executable)) != header.Size {
				return nil, fmt.Errorf("read %s executable", platform)
			}
		case "LICENSE":
			if seenLicense {
				return nil, fmt.Errorf("%s archive contains duplicate LICENSE", platform)
			}
			seenLicense = true
		default:
			return nil, fmt.Errorf("%s archive contains unexpected member %q", platform, header.Name)
		}
	}
	if !seenBinary || !seenLicense {
		return nil, fmt.Errorf("%s archive must contain exactly buildkite-gha and LICENSE", platform)
	}
	return executable, nil
}

func pluginArchiveCachePath(version, asset string) string {
	root := os.Getenv("MISE_DATA_DIR")
	if !filepath.IsAbs(root) {
		return ""
	}
	return filepath.Join(root, "buildkite-gha", "releases", version, asset)
}

func readVerifiedPluginArchiveCache(path string, expected [sha256.Size]byte) []byte {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > pluginArchiveLimit {
		return nil
	}
	contents, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(contents) != expected {
		return nil
	}
	return contents
}

func writePluginArchiveCache(path string, contents []byte) {
	if path == "" {
		return
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return
	}
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil || realDirectory != directory {
		return
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	temporary, err := os.CreateTemp(directory, ".archive-")
	if err != nil {
		return
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = temporary.Write(contents); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		_ = temporary.Close()
		return
	}
	_ = os.Rename(name, path)
}
