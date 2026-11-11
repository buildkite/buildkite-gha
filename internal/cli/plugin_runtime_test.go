package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestPluginDevCounterpartInjectionIsRequiredAndReleaseRejectsIt(t *testing.T) {
	linux := runtimeDistribution{contents: []byte("linux"), digest: "sha256:linux"}
	required := map[compiler.Platform]bool{compiler.PlatformDarwinARM64: true}
	t.Setenv(pluginDevDarwinRuntimeEnvironment, "")
	if _, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), required, compiler.PlatformLinuxAMD64, linux); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("dev acquisition error = %v", err)
	}
	darwin := runtimeDistribution{contents: []byte("darwin"), digest: "sha256:darwin"}
	if _, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformDarwinARM64, darwin); err == nil || !strings.Contains(err.Error(), pluginDevLinuxRuntimeEnvironment) {
		t.Fatalf("Darwin-hosted dev acquisition error = %v", err)
	}
	t.Setenv(pluginDevDarwinRuntimeEnvironment, filepath.Join(t.TempDir(), "injected"))
	if _, err := (&pluginRuntimeAcquisition{version: "1.2.3"}).acquire(t.Context(), map[compiler.Platform]bool{}, compiler.PlatformLinuxAMD64, linux); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("release injection error = %v", err)
	}
	deferred := deferredRuntimePlatforms(parsedUploadArgs{importerPlatform: compiler.PlatformLinuxAMD64, pluginAcquisition: &pluginRuntimeAcquisition{version: "dev"}})
	if len(deferred) != 2 || !deferred[compiler.PlatformDarwinARM64] {
		t.Fatalf("legacy deferred platforms = %#v", deferred)
	}
}

func TestPluginBinaryURLsVerifyBytesAndNeverFallBackToReleases(t *testing.T) {
	t.Setenv(pluginDevWindowsRuntimeEnvironment, filepath.Join(t.TempDir(), "must-not-use-legacy-runtime"))
	linux := pluginTestLinuxExecutable(62)
	host := runtimeDistribution{contents: []byte("running importer"), digest: transport.Digest([]byte("running importer"))}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/linux" || r.URL.RawQuery != "download=1" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		_, _ = w.Write(linux)
	}))
	defer server.Close()
	previousClient := pluginHTTPClient
	pluginHTTPClient = server.Client()
	t.Cleanup(func() { pluginHTTPClient = previousClient })
	for _, fragment := range []string{"", fmt.Sprintf("#sha256=%X", sha256.Sum256(linux))} {
		acquisition := &pluginRuntimeAcquisition{version: "1.2.3", binaries: map[compiler.Platform]string{
			compiler.PlatformDarwinARM64: fmt.Sprintf("%s/must-not-download-host#sha256=%X", server.URL, sha256.Sum256(host.contents)),
			compiler.PlatformLinuxAMD64:  server.URL + "/linux?download=1" + fragment,
		}}
		required := map[compiler.Platform]bool{compiler.PlatformDarwinARM64: true, compiler.PlatformLinuxAMD64: true}
		got, err := acquisition.acquire(t.Context(), required, compiler.PlatformDarwinARM64, host)
		if err != nil || !bytes.Equal(got[compiler.PlatformLinuxAMD64].contents, linux) || got[compiler.PlatformLinuxAMD64].digest != transport.Digest(linux) || got[compiler.PlatformDarwinARM64].digest != host.digest {
			t.Fatalf("custom distributions = %#v, %v", got, err)
		}
		if _, err := acquisition.acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformWindowsAMD64: true}, compiler.PlatformDarwinARM64, host); err == nil || !strings.Contains(err.Error(), "do not fall back") {
			t.Fatalf("missing platform error = %v", err)
		}
		deferred := deferredRuntimePlatforms(parsedUploadArgs{importerPlatform: compiler.PlatformDarwinARM64, pluginAcquisition: acquisition})
		if len(deferred) != 2 || !deferred[compiler.PlatformDarwinARM64] || !deferred[compiler.PlatformLinuxAMD64] {
			t.Fatalf("deferred platforms = %#v", deferred)
		}
	}
	if requests != 2 {
		t.Fatalf("downloads = %d, want only the two Linux downloads", requests)
	}
	for _, test := range []struct {
		platform compiler.Platform
		fragment string
		want     string
	}{
		{compiler.PlatformLinuxAMD64, "#sha256=" + strings.Repeat("0", 64), "checksum verification failed"},
		{compiler.PlatformDarwinARM64, "#sha256=" + strings.Repeat("0", 64), "importer binary checksum verification failed"},
		{compiler.PlatformLinuxARM64, "", "linux/arm64 executable"},
	} {
		acquisition := &pluginRuntimeAcquisition{version: "dev", binaries: map[compiler.Platform]string{test.platform: server.URL + "/linux?download=1" + test.fragment}}
		if _, err := acquisition.acquire(t.Context(), map[compiler.Platform]bool{test.platform: true}, compiler.PlatformDarwinARM64, host); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("invalid binary error = %v, want %s", err, test.want)
		}
	}
}

func TestPluginAcquiresVerifiedLinuxRuntimeForDarwinHost(t *testing.T) {
	linux := pluginTestLinuxExecutable(62)
	arm64 := pluginTestLinuxExecutable(183)
	version := "1.2.3-rc.1+build.2"
	var release pluginRelease
	metadataRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != "buildkite-gha/"+version {
			t.Errorf("release request = %s, User-Agent = %q", request.URL, request.Header.Get("User-Agent"))
		}
		switch request.URL.Path {
		case "/tags/v" + version:
			metadataRequests++
			_ = json.NewEncoder(response).Encode(release)
		case "/download/amd64":
			_, _ = response.Write(linux)
		case "/download/arm64":
			_, _ = response.Write(arm64)
		default:
			t.Errorf("unexpected download (host, checksum, archive or latest): %s", request.URL)
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	release = pluginRelease{TagName: "v" + version, Assets: []pluginReleaseAsset{
		{Name: "buildkite-gha_linux_x86_64", URL: server.URL + "/download/amd64", Digest: transport.Digest(linux)},
		{Name: "buildkite-gha_linux_arm64", URL: server.URL + "/download/arm64", Digest: transport.Digest(arm64)},
	}}
	previousClient, previousAPI := pluginHTTPClient, pluginReleaseAPIURL
	pluginHTTPClient, pluginReleaseAPIURL = server.Client(), server.URL+"/tags/"
	t.Cleanup(func() { pluginHTTPClient, pluginReleaseAPIURL = previousClient, previousAPI })
	t.Setenv("MISE_DATA_DIR", "")
	for _, handoff := range []bool{false, true} {
		t.Run(fmt.Sprintf("handoff=%t", handoff), func(t *testing.T) {
			metadataRequests = 0
			t.Setenv(pluginReleaseEnvironment, string(mustJSON(t, release)))
			if !handoff {
				if err := os.Unsetenv(pluginReleaseEnvironment); err != nil {
					t.Fatal(err)
				}
			}
			host := runtimeDistribution{contents: []byte("running host"), digest: transport.Digest([]byte("running host"))}
			acquisition := &pluginRuntimeAcquisition{version: version}
			got, err := acquisition.acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformDarwinARM64: true}, compiler.PlatformDarwinARM64, host)
			if err != nil || got[compiler.PlatformDarwinARM64].digest != host.digest || metadataRequests != 0 {
				t.Fatalf("host-only acquisition = %v, %v, requests=%d", got, err, metadataRequests)
			}
			required := map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true, compiler.PlatformLinuxARM64: true, compiler.PlatformDarwinARM64: true}
			got, err = acquisition.acquire(t.Context(), required, compiler.PlatformDarwinARM64, host)
			if err != nil {
				t.Fatal(err)
			}
			for platform, contents := range map[compiler.Platform][]byte{compiler.PlatformLinuxAMD64: linux, compiler.PlatformLinuxARM64: arm64, compiler.PlatformDarwinARM64: host.contents} {
				if !bytes.Equal(got[platform].contents, contents) || got[platform].digest != transport.Digest(contents) {
					t.Fatalf("%s distribution changed: %+v", platform, got[platform])
				}
			}
			wantRequests := 1
			if handoff {
				wantRequests = 0
			}
			if metadataRequests != wantRequests {
				t.Fatalf("metadata requests = %d, want %d", metadataRequests, wantRequests)
			}
		})
	}
}

func TestPluginResolvesPinnedCounterpartWithoutDownloadingBytes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("metadata handoff must avoid all import-time requests: %s", r.URL)
		http.Error(w, "unexpected download", http.StatusInternalServerError)
	}))
	defer server.Close()
	previousClient, previousAPI := pluginHTTPClient, pluginReleaseAPIURL
	pluginHTTPClient, pluginReleaseAPIURL = server.Client(), server.URL+"/tags/"
	t.Cleanup(func() { pluginHTTPClient, pluginReleaseAPIURL = previousClient, previousAPI })
	digest := transport.Digest([]byte("linux counterpart"))
	t.Setenv(pluginReleaseEnvironment, string(mustJSON(t, pluginRelease{TagName: "v1.2.3", Assets: []pluginReleaseAsset{
		{Name: pluginLinuxAsset, URL: server.URL + "/linux", Digest: "sha256:" + strings.ToUpper(strings.TrimPrefix(digest, "sha256:"))},
	}})))
	got, err := (&pluginRuntimeAcquisition{version: "1.2.3", pinned: true}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformDarwinARM64, runtimeDistribution{digest: transport.Digest([]byte("darwin importer"))})
	if err != nil {
		t.Fatal(err)
	}
	linux := got[compiler.PlatformLinuxAMD64]
	if len(got) != 1 || len(linux.contents) != 0 || linux.digest != digest || linux.url != server.URL+"/linux" {
		t.Fatalf("pinned counterpart = %#v", got)
	}
}

func TestPluginWindowsSelectionIsLazy(t *testing.T) {
	host := runtimeDistribution{contents: []byte("host"), digest: "sha256:host"}
	t.Setenv(pluginDevWindowsRuntimeEnvironment, "")
	got, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformLinuxAMD64, host)
	if err != nil || len(got) != 1 {
		t.Fatalf("Linux-only acquisition = %#v, %v", got, err)
	}
	if _, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformWindowsAMD64: true}, compiler.PlatformLinuxAMD64, host); err == nil || !strings.Contains(err.Error(), pluginDevWindowsRuntimeEnvironment) {
		t.Fatalf("Windows dev acquisition error = %v", err)
	}
	if pluginRuntimeAsset(compiler.PlatformWindowsAMD64) != pluginWindowsAsset {
		t.Fatal("Windows release asset was not selected")
	}
}

func pluginTestLinuxExecutable(machine uint16) []byte {
	contents := make([]byte, 64)
	copy(contents, []byte("\x7fELF"))
	contents[4] = 2                                   // ELFCLASS64
	contents[5] = 1                                   // ELFDATA2LSB
	contents[6] = 1                                   // EV_CURRENT
	binary.LittleEndian.PutUint16(contents[16:18], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(contents[18:20], machine)
	binary.LittleEndian.PutUint32(contents[20:24], 1)  // EV_CURRENT
	binary.LittleEndian.PutUint16(contents[52:54], 64) // ELF header size
	return contents
}

func TestPluginAcquisitionIsLazyAndBindsVerifiedDarwinContents(t *testing.T) {
	linux := runtimeDistribution{contents: []byte("running executable"), digest: "sha256:running"}
	t.Setenv(pluginDevDarwinRuntimeEnvironment, "")
	got, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformLinuxAMD64, linux)
	if err != nil || got[compiler.PlatformLinuxAMD64].digest != linux.digest {
		t.Fatalf("Linux-only acquisition = %#v, %v", got, err)
	}

	darwin := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	requests := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests[request.URL.Path]++
		switch filepath.Base(request.URL.Path) {
		case "buildkite-gha_darwin_arm64":
			_, _ = response.Write(darwin)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	release := pluginRelease{TagName: "v1.2.3", Assets: []pluginReleaseAsset{
		{Name: "buildkite-gha_darwin_arm64", URL: server.URL + "/v1.2.3/buildkite-gha_darwin_arm64", Digest: transport.Digest(darwin)},
	}}
	cache := filepath.Join(canonicalTempDir(t), pluginDarwinAsset)
	distribution, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), release, cache)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := transport.Digest(darwin)
	if distribution.digest != wantDigest || !bytes.Equal(distribution.contents, darwin) {
		t.Fatalf("Darwin distribution = %q, %x", distribution.digest, distribution.contents)
	}
	if _, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), release, cache); err != nil {
		t.Fatal(err)
	}
	if requests["/v1.2.3/"+pluginDarwinAsset] != 1 {
		t.Fatalf("requests = %#v; verified cache did not avoid a binary download", requests)
	}
	if err := os.WriteFile(cache, []byte("untrusted cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), release, cache); err != nil {
		t.Fatal(err)
	}
	if requests["/v1.2.3/"+pluginDarwinAsset] != 2 {
		t.Fatalf("requests = %#v; invalid cache was used", requests)
	}
}

func TestPluginRejectsInvalidReleaseMetadataAndBytes(t *testing.T) {
	validBinary := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	for _, test := range []struct {
		name string
		edit func(*pluginRelease)
		body []byte
		want string
	}{
		{"corrupted download", nil, []byte("changed"), "checksum verification failed"},
		{"missing digest", func(r *pluginRelease) { r.Assets[0].Digest = "" }, validBinary, "SHA-256 digest"},
		{"malformed digest", func(r *pluginRelease) { r.Assets[0].Digest = "sha256:nope" }, validBinary, "SHA-256 digest"},
		{"wrong algorithm", func(r *pluginRelease) { r.Assets[0].Digest = "sha512:" + strings.Repeat("0", 64) }, validBinary, "SHA-256 digest"},
		{"duplicate asset", func(r *pluginRelease) { r.Assets = append(r.Assets, r.Assets[0]) }, validBinary, "exactly one raw asset"},
		{"archive-only release", func(r *pluginRelease) { r.Assets[0].Name += ".tar.gz" }, validBinary, "exactly one raw asset"},
		{"wrong tag", func(r *pluginRelease) { r.TagName = "v9.9.9" }, validBinary, "does not match running version"},
		{"HTTP asset", func(r *pluginRelease) { r.Assets[0].URL = "http://example.com/binary" }, validBinary, "HTTPS URL"},
		{"wrong platform", func(r *pluginRelease) { r.Assets[0].Digest = transport.Digest(pluginTestLinuxExecutable(62)) }, pluginTestLinuxExecutable(62), "Mach-O"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MISE_DATA_DIR", "")
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch filepath.Base(request.URL.Path) {
				case "buildkite-gha_darwin_arm64":
					_, _ = response.Write(test.body)
				default:
					t.Errorf("unexpected release fallback request: %s", request.URL)
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			release := pluginRelease{TagName: "v1.2.3", Assets: []pluginReleaseAsset{
				{Name: "buildkite-gha_darwin_arm64", URL: server.URL + "/buildkite-gha_darwin_arm64", Digest: transport.Digest(validBinary)},
			}}
			if test.edit != nil {
				test.edit(&release)
			}
			t.Setenv(pluginReleaseEnvironment, string(mustJSON(t, release)))
			previousClient, previousAPI := pluginHTTPClient, pluginReleaseAPIURL
			pluginHTTPClient, pluginReleaseAPIURL = server.Client(), server.URL+"/must-not-refetch/"
			t.Cleanup(func() { pluginHTTPClient, pluginReleaseAPIURL = previousClient, previousAPI })
			if _, err := (&pluginRuntimeAcquisition{version: "1.2.3"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformDarwinARM64: true}, compiler.PlatformLinuxAMD64, runtimeDistribution{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("acquisition error = %v, want %s", err, test.want)
			}
		})
	}
}
