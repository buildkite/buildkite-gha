package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
)

func TestPrepareMiseDataDirFallsBackWhenCacheIsUnavailable(t *testing.T) {
	var stderr bytes.Buffer
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "mise")
	if got := prepareMiseDataDir(dir, &stderr); got != dir || stderr.Len() != 0 {
		t.Fatalf("prepareMiseDataDir() = %q, stderr = %q", got, stderr.String())
	}

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if got := prepareMiseDataDir(file, &stderr); got != "" || !strings.Contains(stderr.String(), "using the ephemeral agent cache") {
		t.Fatalf("prepareMiseDataDir(file) = %q, stderr = %q", got, stderr.String())
	}

	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linked-cache")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	want := filepath.Join(target, "mise")
	if got := prepareMiseDataDir(filepath.Join(link, "mise"), &stderr); got != want || stderr.Len() != 0 {
		t.Fatalf("prepareMiseDataDir(aliased ancestor) = %q, stderr = %q, want %q", got, stderr.String(), want)
	}

	stderr.Reset()
	if got := prepareMiseDataDir(link, &stderr); got != "" || !strings.Contains(stderr.String(), "not a real directory") {
		t.Fatalf("prepareMiseDataDir(symlink root) = %q, stderr = %q", got, stderr.String())
	}
}

func setFakeMise(t *testing.T, version string) string {
	t.Helper()
	root := canonicalTempDir(t)
	mise := filepath.Join(root, "mise")
	script := "#!/bin/sh\nif [ -n \"${MISE_TEST_POISON:-}\" ]; then printf 'poisoned\\n'; else printf '" + version + " linux-x64 (test)\\n'; fi\n"
	if err := os.WriteFile(mise, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	return mise
}

func TestResolveRuntimeMisePinsRequiredExecutable(t *testing.T) {
	realMise := setFakeMise(t, buildkitepipeline.MinimumMiseVersion)
	linkRoot := t.TempDir()
	link := filepath.Join(linkRoot, "mise")
	if err := os.Symlink(realMise, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", linkRoot)
	t.Setenv("MISE_TEST_POISON", "must-not-reach-version-check")
	got, err := resolveRuntimeMise(t.Context(), "", t.TempDir(), t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realMise)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("resolveRuntimeMise() = %q, want pinned %q", got, want)
	}
}

func TestResolveRuntimeMiseAcceptsNewerVersion(t *testing.T) {
	realMise := setFakeMise(t, "2026.8.1")
	got, err := resolveRuntimeMiseWithInstaller(t.Context(), "", t.TempDir(), t.TempDir(), io.Discard, func(context.Context, string, string, io.Writer) (string, error) {
		return "", errors.New("unexpected managed mise install")
	})
	if err != nil || got != realMise {
		t.Fatalf("resolveRuntimeMiseWithInstaller() = %q, %v; want %q", got, err, realMise)
	}
}

func TestResolveRuntimeMiseAcceptsPrefixedVersionOutput(t *testing.T) {
	root := canonicalTempDir(t)
	mise := filepath.Join(root, "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\nprintf 'mise v2026.8.1 linux-x64 (test)\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRuntimeMise(t.Context(), mise, t.TempDir(), t.TempDir(), io.Discard)
	if err != nil || got != mise {
		t.Fatalf("resolveRuntimeMise() = %q, %v; want %q", got, err, mise)
	}
}

func TestMiseVersionAtLeast(t *testing.T) {
	for _, test := range []struct {
		actual string
		want   bool
	}{
		{actual: "2026.5.12", want: true},
		{actual: "v2026.5.12", want: true},
		{actual: "2026.5.13", want: true},
		{actual: "2026.8.1", want: true},
		{actual: "2027.1.1", want: true},
		{actual: "2026.5.11"},
		{actual: "2025.12.31"},
		{actual: "2026.5"},
		{actual: "latest"},
	} {
		if got := miseVersionAtLeast(test.actual, buildkitepipeline.MinimumMiseVersion); got != test.want {
			t.Errorf("miseVersionAtLeast(%q, %q) = %t, want %t", test.actual, buildkitepipeline.MinimumMiseVersion, got, test.want)
		}
	}
}

func TestResolveRuntimeMiseInstallsManagedCopyWhenNeeded(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
	}{
		{name: "missing"},
		{name: "old PATH version", version: "2026.5.11"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pathRoot := t.TempDir()
			if test.version != "" {
				mise := filepath.Join(pathRoot, "mise")
				if err := os.WriteFile(mise, []byte("#!/bin/sh\nprintf '"+test.version+" linux-x64 (test)\\n'\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", pathRoot)
			dataDir := t.TempDir()
			managed := filepath.Join(t.TempDir(), "mise")
			called := 0
			privateRuntime := t.TempDir()
			installer := func(_ context.Context, gotDataDir, gotPrivateRuntime string, _ io.Writer) (string, error) {
				called++
				if gotDataDir != dataDir {
					t.Fatalf("installer data dir = %q, want %q", gotDataDir, dataDir)
				}
				if gotPrivateRuntime != privateRuntime {
					t.Fatalf("installer private runtime = %q, want %q", gotPrivateRuntime, privateRuntime)
				}
				return managed, nil
			}
			got, err := resolveRuntimeMiseWithInstaller(t.Context(), "", dataDir, privateRuntime, io.Discard, installer)
			if err != nil || got != managed || called != 1 {
				t.Fatalf("resolveRuntimeMiseWithInstaller() = %q, %v; calls = %d", got, err, called)
			}
		})
	}
}

func TestResolveRuntimeMiseRejectsInvalidExplicitOverride(t *testing.T) {
	t.Run("configured path must be absolute", func(t *testing.T) {
		if _, err := resolveRuntimeMise(t.Context(), "mise", t.TempDir(), t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Fatalf("resolveRuntimeMise() error = %v", err)
		}
	})
	t.Run("old version", func(t *testing.T) {
		mise := setFakeMise(t, "2026.5.11")
		if _, err := resolveRuntimeMise(t.Context(), mise, t.TempDir(), t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), `reported version "2026.5.11", want "2026.5.12" or newer`) {
			t.Fatalf("resolveRuntimeMise() error = %v", err)
		}
	})
	t.Run("not executable", func(t *testing.T) {
		mise := filepath.Join(t.TempDir(), "mise")
		if err := os.WriteFile(mise, []byte("mise"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveRuntimeMise(t.Context(), mise, t.TempDir(), t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "not an executable regular file") {
			t.Fatalf("resolveRuntimeMise() error = %v", err)
		}
	})
}

func TestInstallRuntimeMiseDownloadsVerifiesAndReusesCache(t *testing.T) {
	binary := []byte("#!/bin/sh\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	archive := runtimeMiseTestArchive(t, binary)
	archiveHash := sha256.Sum256(archive)
	binaryHash := sha256.Sum256(binary)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("User-Agent") != "buildkite-gha/1.2.3" {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	root := filepath.Join(logicalParent, "cache")
	want := filepath.Join(realParent, "cache", "linux-x64", "mise")
	for range 2 {
		got, err := installRuntimeMiseFrom(t.Context(), root, server.Client(), server.URL, hex.EncodeToString(archiveHash[:]), hex.EncodeToString(binaryHash[:]), "1.2.3")
		if err != nil || got != want {
			t.Fatalf("installRuntimeMiseFrom() = %q, %v", got, err)
		}
	}
	if requests != 1 {
		t.Fatalf("mise archive requests = %d, want one cache miss", requests)
	}
	if got, err := os.ReadFile(want); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("installed mise = %q, %v", got, err)
	}
}

func TestInstallRuntimeMiseRejectsInvalidArchive(t *testing.T) {
	binary := []byte("#!/bin/sh\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	archive := runtimeMiseTestArchive(t, binary)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	binaryHash := sha256.Sum256(binary)
	if _, err := installRuntimeMiseFrom(t.Context(), t.TempDir(), server.Client(), server.URL, strings.Repeat("0", 64), hex.EncodeToString(binaryHash[:]), "test-version"); err == nil || !strings.Contains(err.Error(), "archive checksum") {
		t.Fatalf("installRuntimeMiseFrom() error = %v", err)
	}
}

func TestValidateRuntimeMiseRejectsOversizedCacheEntry(t *testing.T) {
	cached := filepath.Join(t.TempDir(), "mise")
	file, err := os.OpenFile(cached, os.O_CREATE|os.O_WRONLY, 0o500)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(runtimeMiseBinaryLimit + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRuntimeMiseFile(t.Context(), cached, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("validateRuntimeMiseFile() error = %v, want size rejection", err)
	}
}

func TestManagedMiseCacheIsNotExecutedBeforePrivateCopy(t *testing.T) {
	root := canonicalTempDir(t)
	marker := filepath.Join(t.TempDir(), "executed")
	binary := []byte("#!/bin/sh\nprintf ran > '" + marker + "'\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	digest := sha256.Sum256(binary)
	cached := filepath.Join(root, "linux-x64", "mise")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, binary, 0o500); err != nil {
		t.Fatal(err)
	}
	got, err := installRuntimeMiseFrom(t.Context(), root, nil, "", "", hex.EncodeToString(digest[:]), "test-version")
	if err != nil || got != cached {
		t.Fatalf("installRuntimeMiseFrom() = %q, %v; want cache hit %q", got, err, cached)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared cache executable ran during validation: %v", err)
	}
	if _, err := pinRuntimeMise(t.Context(), cached, t.TempDir(), hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("private executable did not run during version validation: %v", err)
	}
}

func TestManagedMiseColdCacheIsNotExecutedBeforePrivateCopy(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	binary := []byte("#!/bin/sh\nprintf ran > '" + marker + "'\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	archive := runtimeMiseTestArchive(t, binary)
	archiveDigest := sha256.Sum256(archive)
	binaryDigest := sha256.Sum256(binary)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	cached, err := installRuntimeMiseFrom(t.Context(), t.TempDir(), server.Client(), server.URL, hex.EncodeToString(archiveDigest[:]), hex.EncodeToString(binaryDigest[:]), "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared staging executable ran during validation: %v", err)
	}
	if _, err := pinRuntimeMise(t.Context(), cached, t.TempDir(), hex.EncodeToString(binaryDigest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("private executable did not run during version validation: %v", err)
	}
}

func TestPinRuntimeMiseCopiesVerifiedBytesPrivately(t *testing.T) {
	binary := []byte("#!/bin/sh\nprintf '" + buildkitepipeline.MinimumMiseVersion + " linux-x64 (test)\\n'\n")
	digest := sha256.Sum256(binary)
	cached := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(cached, binary, 0o500); err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	privateRuntime := filepath.Join(logicalParent, "runtime")
	if err := os.Mkdir(privateRuntime, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := pinRuntimeMise(t.Context(), cached, privateRuntime, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(realParent, "runtime", "mise") {
		t.Fatalf("pinRuntimeMise() = %q, want private executable", got)
	}
	if err := os.Chmod(cached, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if copied, err := os.ReadFile(got); err != nil || !bytes.Equal(copied, binary) {
		t.Fatalf("private mise changed with cache: %q, %v", copied, err)
	}
	if _, err := pinRuntimeMise(t.Context(), cached, t.TempDir(), hex.EncodeToString(digest[:])); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("pinRuntimeMise() accepted tampered cache: %v", err)
	}
	linkedRuntime := filepath.Join(base, "linked-runtime")
	if err := os.Symlink(filepath.Join(realParent, "runtime"), linkedRuntime); err != nil {
		t.Fatal(err)
	}
	if _, err := pinRuntimeMise(t.Context(), got, linkedRuntime, hex.EncodeToString(digest[:])); err == nil || !strings.Contains(err.Error(), "contains a symlink") {
		t.Fatalf("pinRuntimeMise() accepted symlink root: %v", err)
	}
}

func TestInstallRuntimeMiseLiveRelease(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_REQUIRED") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_REQUIRED=1 to verify the pinned mise release")
	}
	privateRuntime := t.TempDir()
	got, err := installRuntimeMise(t.Context(), t.TempDir(), privateRuntime, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != privateRuntime {
		t.Fatalf("installed mise path = %q, want job-private root %q", got, privateRuntime)
	}
	selected, err := selectRuntimeMiseRelease(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateRuntimeMise(t.Context(), got, selected.binaryDigest); err != nil {
		t.Fatalf("validate installed mise release: %v", err)
	}
}

func TestSelectRuntimeMiseRelease(t *testing.T) {
	for _, test := range []struct {
		goos, goarch, asset, cacheKey, archiveDigest, binaryDigest string
	}{
		{"linux", "amd64", "linux-x64", "linux-amd64", runtimeMiseArchiveDigest, runtimeMiseBinaryDigest},
		{"darwin", "arm64", "macos-arm64", "darwin-arm64", runtimeMiseDarwinARM64ArchiveDigest, runtimeMiseDarwinARM64BinaryDigest},
	} {
		selected, err := selectRuntimeMiseRelease(test.goos, test.goarch)
		if err != nil {
			t.Fatal(err)
		}
		if selected.asset != test.asset || selected.cacheKey != test.cacheKey || selected.archiveDigest != test.archiveDigest || selected.binaryDigest != test.binaryDigest {
			t.Fatalf("selectRuntimeMiseRelease(%s/%s) = %#v", test.goos, test.goarch, selected)
		}
	}
	if _, err := selectRuntimeMiseRelease("linux", "arm64"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("selectRuntimeMiseRelease() unsupported platform error = %v", err)
	}
}

func runtimeMiseTestArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gzipWriter := gzip.NewWriter(&out)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "mise/bin/mise", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
