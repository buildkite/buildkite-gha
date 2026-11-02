package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
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

func TestPluginBinaryURLsVerifyBytesAndNeverFallBackToReleases(t *testing.T) {
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
	archive := pluginTestArchive(t, linux, false)
	archiveDigest := sha256.Sum256(archive)
	version := "1.2.3-rc.1+build.2"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != "buildkite-gha/"+version || !strings.HasPrefix(request.URL.Path, "/v"+version+"/") {
			t.Errorf("release request = %s, User-Agent = %q", request.URL, request.Header.Get("User-Agent"))
		}
		switch filepath.Base(request.URL.Path) {
		case "checksums.txt":
			_, _ = fmt.Fprintf(response, "%x  %s\n", archiveDigest, pluginLinuxAsset)
		case pluginLinuxAsset:
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	previousClient, previousBase := pluginHTTPClient, pluginReleaseBaseURL
	pluginHTTPClient, pluginReleaseBaseURL = server.Client(), server.URL
	t.Cleanup(func() { pluginHTTPClient, pluginReleaseBaseURL = previousClient, previousBase })
	distributions, err := (&pluginRuntimeAcquisition{version: version}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformDarwinARM64, runtimeDistribution{})
	if err != nil {
		t.Fatal(err)
	}
	distribution := distributions[compiler.PlatformLinuxAMD64]
	if distribution.digest != transport.Digest(linux) || !bytes.Equal(distribution.contents, linux) {
		t.Fatalf("Linux distribution = %q, %d bytes", distribution.digest, len(distribution.contents))
	}
}

func TestPluginAcquiresVerifiedLinuxARM64Runtime(t *testing.T) {
	linux := pluginTestLinuxExecutable(183)
	archive := pluginTestArchive(t, linux, false)
	archiveDigest := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch filepath.Base(request.URL.Path) {
		case "checksums.txt":
			_, _ = fmt.Fprintf(response, "%x  %s\n", archiveDigest, pluginLinuxARM64Asset)
		case pluginLinuxARM64Asset:
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	distribution, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformLinuxARM64, server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if distribution.digest != transport.Digest(linux) || !bytes.Equal(distribution.contents, linux) {
		t.Fatalf("Linux ARM64 distribution = %q, %d bytes", distribution.digest, len(distribution.contents))
	}
}

func TestPluginWindowsSelectionIsLazy(t *testing.T) {
	host := runtimeDistribution{contents: []byte("host"), digest: "sha256:host"}
	got, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformLinuxAMD64, host)
	if err != nil || len(got) != 1 {
		t.Fatalf("Linux-only acquisition = %#v, %v", got, err)
	}
	if _, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformWindowsAMD64: true}, compiler.PlatformLinuxAMD64, host); err == nil || !strings.Contains(err.Error(), "configure binary URLs") {
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
	got, err := (&pluginRuntimeAcquisition{version: "dev"}).acquire(t.Context(), map[compiler.Platform]bool{compiler.PlatformLinuxAMD64: true}, compiler.PlatformLinuxAMD64, linux)
	if err != nil || got[compiler.PlatformLinuxAMD64].digest != linux.digest {
		t.Fatalf("Linux-only acquisition = %#v, %v", got, err)
	}

	darwin := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	archive := pluginTestArchive(t, darwin, false)
	archiveDigest := sha256.Sum256(archive)
	requests := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests[request.URL.Path]++
		switch filepath.Base(request.URL.Path) {
		case "checksums.txt":
			_, _ = fmt.Fprintf(response, "%x  %s\n", archiveDigest, pluginDarwinAsset)
		case pluginDarwinAsset:
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	cache := filepath.Join(canonicalTempDir(t), pluginDarwinAsset)
	distribution, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, cache)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := transport.Digest(darwin)
	if distribution.digest != wantDigest || !bytes.Equal(distribution.contents, darwin) {
		t.Fatalf("Darwin distribution = %q, %x", distribution.digest, distribution.contents)
	}
	if _, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, cache); err != nil {
		t.Fatal(err)
	}
	if requests["/v1.2.3/checksums.txt"] != 2 || requests["/v1.2.3/"+pluginDarwinAsset] != 1 {
		t.Fatalf("requests = %#v; verified cache did not avoid an archive download", requests)
	}
	if err := os.WriteFile(cache, []byte("untrusted cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, cache); err != nil {
		t.Fatal(err)
	}
	if requests["/v1.2.3/checksums.txt"] != 3 || requests["/v1.2.3/"+pluginDarwinAsset] != 2 {
		t.Fatalf("requests = %#v; cache was trusted without live checksum or invalid cache was used", requests)
	}
}

func TestPluginDarwinRejectsChecksumArchiveAndBinaryFailures(t *testing.T) {
	validBinary := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	archive := pluginTestArchive(t, validBinary, false)
	digest := sha256.Sum256(archive)
	if _, err := pluginAssetChecksum(fmt.Appendf(nil, "%x  %s\n%x  %s\n", digest, pluginDarwinAsset, digest, pluginDarwinAsset), pluginDarwinAsset); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("duplicate checksum error = %v", err)
	}
	if _, err := extractPluginRuntime(pluginTestArchive(t, validBinary, true), compiler.PlatformDarwinARM64); err == nil || !strings.Contains(err.Error(), "unexpected member") {
		t.Fatalf("unsafe archive error = %v", err)
	}
	wrong := pluginTestArchive(t, []byte("not Mach-O"), false)
	wrongDigest := sha256.Sum256(wrong)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if filepath.Base(request.URL.Path) == "checksums.txt" {
			_, _ = fmt.Fprintf(response, "%x  %s\n", wrongDigest, pluginDarwinAsset)
			return
		}
		_, _ = response.Write(wrong)
	}))
	defer server.Close()
	if _, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformDarwinARM64, server.Client(), server.URL, ""); err == nil || !strings.Contains(err.Error(), "Mach-O") {
		t.Fatalf("wrong binary error = %v", err)
	}
}

func pluginTestArchive(t *testing.T, executable []byte, extra bool) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range []struct {
		name string
		mode int64
		body []byte
	}{{"buildkite-gha", 0o755, executable}, {"LICENSE", 0o644, []byte("license")}} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: member.mode, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if extra {
		body := []byte("extra")
		if err := tarWriter.WriteHeader(&tar.Header{Name: "../extra", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tarWriter.Write(body)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
