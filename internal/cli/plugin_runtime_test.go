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
}

func TestPluginAcquiresVerifiedLinuxRuntimeForDarwinHost(t *testing.T) {
	linux := pluginTestLinuxExecutable()
	archive := pluginTestArchive(t, linux, false)
	archiveDigest := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
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
	distribution, err := acquirePluginRuntime(t.Context(), "1.2.3", compiler.PlatformLinuxAMD64, server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if distribution.digest != transport.Digest(linux) || !bytes.Equal(distribution.contents, linux) {
		t.Fatalf("Linux distribution = %q, %d bytes", distribution.digest, len(distribution.contents))
	}
}

func pluginTestLinuxExecutable() []byte {
	contents := make([]byte, 64)
	copy(contents, []byte("\x7fELF"))
	contents[4] = 2                                    // ELFCLASS64
	contents[5] = 1                                    // ELFDATA2LSB
	contents[6] = 1                                    // EV_CURRENT
	binary.LittleEndian.PutUint16(contents[16:18], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(contents[18:20], 62) // EM_X86_64
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
	if _, err := pluginAssetChecksum([]byte(fmt.Sprintf("%x  %s\n%x  %s\n", digest, pluginDarwinAsset, digest, pluginDarwinAsset)), pluginDarwinAsset); err == nil || !strings.Contains(err.Error(), "exactly one") {
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
