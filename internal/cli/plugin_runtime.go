package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/useragent"
)

const (
	pluginDevDarwinRuntimeEnvironment     = "BUILDKITE_GHA_PLUGIN_DEV_DARWIN_RUNTIME"
	pluginDevLinuxRuntimeEnvironment      = "BUILDKITE_GHA_PLUGIN_DEV_LINUX_RUNTIME"
	pluginDevLinuxARM64RuntimeEnvironment = "BUILDKITE_GHA_PLUGIN_DEV_LINUX_ARM64_RUNTIME"
	pluginDevWindowsRuntimeEnvironment    = "BUILDKITE_GHA_PLUGIN_DEV_WINDOWS_RUNTIME"
	pluginReleaseEnvironment              = "BUILDKITE_GHA_PLUGIN_RELEASE"
	pluginReleaseMetadataLimit            = 4 << 20
)

var releaseVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*)?(\+[0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*)?$`)

var pluginReleaseAPIURL = "https://api.github.com/repos/buildkite/buildkite-gha/releases/tags/"

var pluginHTTPClient = securePluginHTTPClient()

// The agent supplies normalized plugin sources and the active hook's config.
// Match both so another GitHub Actions entry cannot choose this import's ref.
func pluginRuntimeReference() string {
	var plugins []any
	var configuration map[string]any
	if json.Unmarshal([]byte(os.Getenv("BUILDKITE_PLUGINS")), &plugins) != nil ||
		json.Unmarshal([]byte(os.Getenv(pluginConfigurationEnvironment)), &configuration) != nil {
		return "github-actions"
	}
	reference := ""
	for _, entry := range plugins {
		plugin, _ := entry.(map[string]any)
		for source, config := range plugin {
			location, version, _ := strings.Cut(source, "#")
			location = strings.TrimSuffix(strings.TrimPrefix(location, "https://"), ".git")
			switch location {
			case "github-actions", "buildkite-plugins/github-actions", "github.com/buildkite-plugins/github-actions-buildkite-plugin":
				if version == "" || !reflect.DeepEqual(config, configuration) {
					continue
				}
				if reference != "" && reference != source {
					return "github-actions"
				}
				reference = source
			}
		}
	}
	if reference == "" {
		return "github-actions"
	}
	return reference
}

type pluginRuntimeAcquisition struct {
	version  string
	binaries map[compiler.Platform]string
	pinned   bool
}

type pluginRelease struct {
	TagName string               `json:"tag_name"`
	Assets  []pluginReleaseAsset `json:"assets"`
}

type pluginReleaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

const (
	pluginLinuxAsset      = "buildkite-gha_linux_x86_64"
	pluginLinuxARM64Asset = "buildkite-gha_linux_arm64"
	pluginDarwinAsset     = "buildkite-gha_darwin_arm64"
	pluginWindowsAsset    = "buildkite-gha_windows_x86_64.exe"
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
		compiler.PlatformLinuxAMD64:   os.Getenv(pluginDevLinuxRuntimeEnvironment),
		compiler.PlatformLinuxARM64:   os.Getenv(pluginDevLinuxARM64RuntimeEnvironment),
		compiler.PlatformDarwinARM64:  os.Getenv(pluginDevDarwinRuntimeEnvironment),
		compiler.PlatformWindowsAMD64: os.Getenv(pluginDevWindowsRuntimeEnvironment),
	}
	if a.binaries == nil && a.version != "dev" {
		for platform, path := range devPaths {
			if path != "" {
				return nil, fmt.Errorf("%s is test/dev-only and is rejected by release builds", pluginDevRuntimeEnvironment(platform))
			}
		}
	}
	if a.pinned && len(required) != 0 {
		release, err := loadPluginRelease(ctx, a.version)
		if err != nil {
			return nil, err
		}
		for platform := range required {
			asset, err := pluginRuntimeReleaseAsset(*release, platform)
			if err != nil {
				return nil, err
			}
			if platform == hostPlatform && asset.Digest != host.digest {
				return nil, errors.New("importer binary checksum does not match release metadata")
			}
			distributions[platform] = runtimeDistribution{digest: asset.Digest, url: asset.URL}
		}
		return distributions, nil
	}
	var release *pluginRelease
	for _, platform := range []compiler.Platform{compiler.PlatformLinuxAMD64, compiler.PlatformLinuxARM64, compiler.PlatformDarwinARM64, compiler.PlatformWindowsAMD64} {
		if !required[platform] {
			continue
		}
		if platform == hostPlatform {
			if source := a.binaries[platform]; source != "" {
				_, expected, err := parsePluginBinaryURL(source)
				if err != nil {
					return nil, err
				}
				if expected != nil && host.digest != fmt.Sprintf("sha256:%x", expected) {
					return nil, fmt.Errorf("importer binary checksum verification failed")
				}
			}
			distributions[platform] = host
			continue
		}
		if a.binaries != nil {
			source := a.binaries[platform]
			if source == "" {
				return nil, fmt.Errorf("binary URL for %s is required; custom binaries do not fall back to releases", platform)
			}
			source, expected, err := parsePluginBinaryURL(source)
			if err != nil {
				return nil, err
			}
			contents, err := downloadPluginReleaseFile(ctx, pluginHTTPClient, source, a.version, runtimeDistributionLimit)
			if err != nil {
				return nil, fmt.Errorf("download %s binary: %w", platform, err)
			}
			digest := sha256.Sum256(contents)
			if expected != nil && !bytes.Equal(digest[:], expected) {
				return nil, fmt.Errorf("%s binary checksum verification failed", platform)
			}
			if err := validateRuntimeDistributionBinary(platform, contents); err != nil {
				return nil, fmt.Errorf("validate %s binary: %w", platform, err)
			}
			distributions[platform] = runtimeDistribution{contents: contents, digest: fmt.Sprintf("sha256:%x", digest)}
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
		if !releaseVersionPattern.MatchString(a.version) {
			return nil, fmt.Errorf("running version %q is not a release version; configure binary URLs for other platforms", a.version)
		}
		if release == nil {
			var err error
			release, err = loadPluginRelease(ctx, a.version)
			if err != nil {
				return nil, err
			}
		}
		distribution, err := acquirePluginRuntime(ctx, a.version, platform, pluginHTTPClient, *release, pluginReleaseCachePath(a.version, pluginRuntimeAsset(platform)))
		if err != nil {
			return nil, err
		}
		distributions[platform] = distribution
	}
	return distributions, nil
}

func pluginDevRuntimeEnvironment(platform compiler.Platform) string {
	if platform == compiler.PlatformWindowsAMD64 {
		return pluginDevWindowsRuntimeEnvironment
	}
	if platform == compiler.PlatformDarwinARM64 {
		return pluginDevDarwinRuntimeEnvironment
	}
	if platform == compiler.PlatformLinuxARM64 {
		return pluginDevLinuxARM64RuntimeEnvironment
	}
	return pluginDevLinuxRuntimeEnvironment
}

func parsePluginBinaryURL(value string) (string, []byte, error) {
	source, fragment, hasFragment := strings.Cut(value, "#")
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return "", nil, errors.New("binary must be an HTTPS URL without user credentials")
	}
	if !hasFragment {
		return source, nil, nil
	}
	hash, ok := strings.CutPrefix(fragment, "sha256=")
	digest, err := hex.DecodeString(hash)
	if !ok || err != nil || len(digest) != sha256.Size {
		return "", nil, errors.New("binary URL fragment must be sha256= followed by 64 hexadecimal digits")
	}
	return source, digest, nil
}

func pluginRuntimeAsset(platform compiler.Platform) string {
	if platform == compiler.PlatformWindowsAMD64 {
		return pluginWindowsAsset
	}
	if platform == compiler.PlatformDarwinARM64 {
		return pluginDarwinAsset
	}
	if platform == compiler.PlatformLinuxARM64 {
		return pluginLinuxARM64Asset
	}
	return pluginLinuxAsset
}

func loadPluginRelease(ctx context.Context, version string) (*pluginRelease, error) {
	metadata, provided := os.LookupEnv(pluginReleaseEnvironment)
	data := []byte(metadata)
	if !provided {
		var err error
		data, err = downloadPluginReleaseFile(ctx, pluginHTTPClient, pluginReleaseAPIURL+url.PathEscape("v"+version), version, pluginReleaseMetadataLimit)
		if err != nil {
			return nil, fmt.Errorf("download release metadata: %w", err)
		}
	}
	if len(data) > pluginReleaseMetadataLimit {
		return nil, errors.New("release metadata exceeds size limit")
	}
	var release pluginRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return nil, fmt.Errorf("decode release metadata: %w", err)
	}
	if release.TagName != "v"+version {
		return nil, fmt.Errorf("release metadata tag %q does not match running version %q", release.TagName, version)
	}
	return &release, nil
}

func pluginRuntimeReleaseAsset(release pluginRelease, platform compiler.Platform) (pluginReleaseAsset, error) {
	name := pluginRuntimeAsset(platform)
	var selected pluginReleaseAsset
	matches := 0
	for _, asset := range release.Assets {
		if asset.Name == name {
			selected = asset
			matches++
		}
	}
	if matches != 1 {
		return pluginReleaseAsset{}, fmt.Errorf("release must contain exactly one raw asset %s", name)
	}
	hash, ok := strings.CutPrefix(selected.Digest, "sha256:")
	decoded, err := hex.DecodeString(hash)
	if !ok || err != nil || len(decoded) != sha256.Size {
		return pluginReleaseAsset{}, fmt.Errorf("release asset %s requires a valid SHA-256 digest", name)
	}
	parsed, err := url.Parse(selected.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return pluginReleaseAsset{}, fmt.Errorf("release asset %s requires an HTTPS URL without credentials or a fragment", name)
	}
	selected.Digest = "sha256:" + hex.EncodeToString(decoded)
	return selected, nil
}

func acquirePluginRuntime(ctx context.Context, version string, platform compiler.Platform, client *http.Client, release pluginRelease, cachePath string) (runtimeDistribution, error) {
	selected, err := pluginRuntimeReleaseAsset(release, platform)
	if err != nil {
		return runtimeDistribution{}, err
	}
	decoded, _ := hex.DecodeString(strings.TrimPrefix(selected.Digest, "sha256:"))
	expected := [sha256.Size]byte(decoded)
	contents := readVerifiedPluginRuntimeCache(cachePath, expected)
	if contents == nil {
		contents, err = downloadPluginReleaseFile(ctx, client, selected.URL, version, runtimeDistributionLimit)
		if err != nil {
			return runtimeDistribution{}, fmt.Errorf("download %s runtime: %w", platform, err)
		}
		if sha256.Sum256(contents) != expected {
			return runtimeDistribution{}, fmt.Errorf("%s binary checksum verification failed", platform)
		}
		writePluginRuntimeCache(cachePath, contents)
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

func pluginReleaseCachePath(version, asset string) string {
	root := os.Getenv("MISE_DATA_DIR")
	if !filepath.IsAbs(root) {
		return ""
	}
	return filepath.Join(root, "buildkite-gha", "releases", version, asset)
}

func readVerifiedPluginRuntimeCache(path string, expected [sha256.Size]byte) []byte {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > runtimeDistributionLimit {
		return nil
	}
	contents, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(contents) != expected {
		return nil
	}
	return contents
}

func writePluginRuntimeCache(path string, contents []byte) {
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
	temporary, err := os.CreateTemp(directory, ".binary-")
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
