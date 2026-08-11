package launcher

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

const testTarget = `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'buildkite-gha %s\n' "${TEST_TARGET_VERSION:-0.8.0}"
  exit 0
fi
printf 'argv=%s argc=%s\n' "$*" "$#"
printf 'executable=%s\n' "$0"
printf 'env=%s\n' "$TEST_INHERITED"
printf 'cwd=%s\n' "$PWD"
IFS= read -r input || true
printf 'stdin=%s\n' "$input"
printf 'target-stderr\n' >&2
exit "${TEST_EXIT:-0}"
`

type releaseFixture struct {
	archive          []byte
	checksum         string
	latestLocation   string
	latestRequests   atomic.Int32
	checksumRequests atomic.Int32
	archiveRequests  atomic.Int32
	server           *httptest.Server
}

func newReleaseFixture(t *testing.T, archive []byte) *releaseFixture {
	t.Helper()
	digest := sha256.Sum256(archive)
	f := &releaseFixture{archive: archive, checksum: fmt.Sprintf("%x", digest)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			f.latestRequests.Add(1)
			location := f.latestLocation
			if location == "" {
				location = f.server.URL + "/releases/tag/v0.8.0"
			}
			http.Redirect(w, r, location, http.StatusFound)
		case "/releases/tag/v0.8.0":
			_, _ = io.WriteString(w, "release")
		case "/releases/download/v0.8.0/checksums.txt":
			f.checksumRequests.Add(1)
			_, _ = fmt.Fprintf(w, "%s  %s\n", f.checksum, archiveName)
		case "/releases/download/v0.8.0/" + archiveName:
			f.archiveRequests.Add(1)
			_, _ = w.Write(f.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func targetArchive(t *testing.T, target string, extra ...tar.Header) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	writeTarEntry(t, tw, tar.Header{Name: "LICENSE", Mode: 0644, Size: 7, Typeflag: tar.TypeReg}, []byte("license"))
	writeTarEntry(t, tw, tar.Header{Name: "buildkite-gha", Mode: 0755, Size: int64(len(target)), Typeflag: tar.TypeReg}, []byte(target))
	for _, header := range extra {
		writeTarEntry(t, tw, header, bytes.Repeat([]byte("x"), int(header.Size)))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func archiveWithEntries(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	for _, header := range entries {
		writeTarEntry(t, tw, header, bytes.Repeat([]byte("x"), int(header.Size)))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func writeTarEntry(t *testing.T, tw *tar.Writer, header tar.Header, contents []byte) {
	t.Helper()
	if err := tw.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if len(contents) > 0 {
		if _, err := tw.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
}

func testConfig(t *testing.T, fixture *releaseFixture, selector string) (config, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "cache")
	values := map[string]string{
		"BUILDKITE_PLUGIN_GITHUB_ACTIONS_VERSION":    selector,
		"BUILDKITE_GITHUB_ACTIONS_PLUGIN_CACHE_ROOT": cache,
	}
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	c := config{
		env:           []string{"PATH=" + os.Getenv("PATH"), "TEST_INHERITED=kept"},
		getenv:        func(name string) string { return values[name] },
		stdout:        stdout,
		stderr:        stderr,
		stdin:         strings.NewReader("input-data\n"),
		client:        fixture.server.Client(),
		repositoryURL: fixture.server.URL,
		goos:          "linux",
		goarch:        "amd64",
		tempDir:       t.TempDir(),
	}
	return c, stdout, stderr, cache
}

func TestLauncherExactExecutionContract(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	c, stdout, stderr, _ := testConfig(t, fixture, "v0.8.0")
	working := t.TempDir()
	t.Chdir(working)
	c.env = append(c.env, "TEST_EXIT=37")
	err := c.run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 37 {
		t.Fatalf("run error = %v, want exit 37", err)
	}
	for _, want := range []string{"~~~ :github: Prepare workflow", "argv=plugin argc=1", "env=kept", "cwd=" + working, "stdin=input-data"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q does not contain %q", stdout.String(), want)
		}
	}
	if !strings.Contains(stdout.String(), "executable="+c.tempDir+"/buildkite-gha-run-") {
		t.Errorf("target did not execute from a private run directory: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "target-stderr") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if fixture.latestRequests.Load() != 0 || fixture.checksumRequests.Load() != 1 || fixture.archiveRequests.Load() != 1 {
		t.Fatalf("requests latest/checksum/archive = %d/%d/%d", fixture.latestRequests.Load(), fixture.checksumRequests.Load(), fixture.archiveRequests.Load())
	}
}

func TestLauncherRetriesTextBusyExec(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	c, stdout, _, _ := testConfig(t, fixture, "0.8.0")
	attempts := map[string]int{}
	c.startCommand = func(cmd *exec.Cmd) error {
		argument := cmd.Args[1]
		attempts[argument]++
		if (argument == "--version" && attempts[argument] <= 2) || (argument == "plugin" && attempts[argument] == 1) {
			return syscall.ETXTBSY
		}
		return cmd.Start()
	}
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if attempts["--version"] != 3 || attempts["plugin"] != 2 {
		t.Fatalf("start attempts = %#v, want 3 version and 2 plugin attempts", attempts)
	}
	if !strings.Contains(stdout.String(), "argv=plugin argc=1") {
		t.Fatalf("plugin did not execute after ETXTBSY retries: %q", stdout.String())
	}
}

func TestLauncherLatestResolvesOnceAndIgnoresLegacySelector(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	c, _, stderr, _ := testConfig(t, fixture, "")
	original := c.getenv
	c.getenv = func(name string) string {
		if name == "BUILDKITE_PLUGIN__VERSION" {
			return "0.7.0"
		}
		return original(name)
	}
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if fixture.latestRequests.Load() != 1 || !strings.Contains(stderr.String(), "resolved latest to v0.8.0") {
		t.Fatalf("latest requests = %d, stderr = %q", fixture.latestRequests.Load(), stderr.String())
	}
}

func TestLauncherRejectsUncanonicalLatestRedirect(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	fixture.latestLocation = fixture.server.URL + "/releases/download/v0.8.0/checksums.txt"
	c, _, _, _ := testConfig(t, fixture, "latest")
	if err := c.run(); err == nil || !strings.Contains(err.Error(), "outside canonical release tags") {
		t.Fatalf("run error = %v", err)
	}
	if fixture.archiveRequests.Load() != 0 {
		t.Fatal("downloaded archive after invalid latest redirect")
	}

	fixture.latestLocation = "https://example.com/buildkite/buildkite-gha/releases/tag/v0.8.0"
	if err := c.run(); err == nil || !strings.Contains(err.Error(), "untrusted URL") {
		t.Fatalf("cross-host redirect error = %v", err)
	}
}

func TestLauncherCacheHitAndTamperRepair(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	c, _, _, cache := testConfig(t, fixture, "0.8.0")
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	c.stdout, c.stderr, c.stdin = io.Discard, io.Discard, strings.NewReader("")
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if fixture.archiveRequests.Load() != 1 || fixture.checksumRequests.Load() != 2 {
		t.Fatalf("cache-hit requests archive/checksum = %d/%d", fixture.archiveRequests.Load(), fixture.checksumRequests.Load())
	}
	entry := filepath.Join(cache, "v0.8.0", "Linux_x86_64", fixture.checksum, archiveName)
	if err := os.WriteFile(entry, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if fixture.archiveRequests.Load() != 2 || !fileDigestMatches(t, entry, fixture.checksum) {
		t.Fatalf("tampered cache was not repaired; downloads = %d", fixture.archiveRequests.Load())
	}
}

func TestLauncherUnusableSelectedCacheFallsBackToTemporary(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	c, _, stderr, cache := testConfig(t, fixture, "0.8.0")
	if err := os.WriteFile(cache, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "using temporary cache") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if info, err := os.Stat(cache); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("selected cache was replaced instead of bypassed: %v, %v", info, err)
	}
}

func TestLauncherTreatsCachedFIFOAsMiss(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	c, _, _, cache := testConfig(t, fixture, "0.8.0")
	entry := filepath.Join(cache, "v0.8.0", "Linux_x86_64", fixture.checksum, archiveName)
	if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(entry, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if fixture.archiveRequests.Load() != 1 || !fileDigestMatches(t, entry, fixture.checksum) {
		t.Fatalf("cached FIFO did not become a verified archive; downloads = %d", fixture.archiveRequests.Load())
	}
}

func TestLauncherConcurrentCacheConvergence(t *testing.T) {
	fixture := newReleaseFixture(t, targetArchive(t, testTarget))
	base, _, _, cache := testConfig(t, fixture, "0.8.0")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := base
			c.stdout, c.stderr, c.stdin = io.Discard, io.Discard, strings.NewReader("")
			errs <- c.run()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(cache, "v0.8.0", "Linux_x86_64", fixture.checksum, archiveName)
	if !fileDigestMatches(t, entry, fixture.checksum) {
		t.Fatal("concurrent cache did not converge on verified archive")
	}
}

func TestLauncherFailuresDoNotPublishOrExecute(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*releaseFixture)
		archive []byte
		want    string
	}{
		{name: "checksum mismatch", archive: targetArchive(t, testTarget), mutate: func(f *releaseFixture) { f.checksum = strings.Repeat("0", 64) }, want: "checksum mismatch"},
		{name: "hostile archive", archive: archiveWithEntries(t, tar.Header{Name: "../buildkite-gha", Size: 1, Typeflag: tar.TypeReg}), want: "unexpected archive entry"},
		{name: "version mismatch", archive: targetArchive(t, strings.Replace(testTarget, "0.8.0", "0.9.0", 1)), want: "version verification"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReleaseFixture(t, test.archive)
			if test.mutate != nil {
				test.mutate(fixture)
			}
			c, stdout, _, cache := testConfig(t, fixture, "0.8.0")
			if err := c.run(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run error = %v, want %q", err, test.want)
			}
			if strings.Contains(stdout.String(), "Prepare workflow") {
				t.Fatal("plugin execution began after validation failure")
			}
			entry := filepath.Join(cache, "v0.8.0", "Linux_x86_64", fixture.checksum, archiveName)
			if _, err := os.Stat(entry); !os.IsNotExist(err) {
				t.Fatalf("invalid distribution was cached: %v", err)
			}
		})
	}
}

func TestExtractRejectsHostileEntries(t *testing.T) {
	tests := []struct {
		name   string
		header tar.Header
	}{
		{name: "parent path", header: tar.Header{Name: "../buildkite-gha", Size: 1, Typeflag: tar.TypeReg}},
		{name: "absolute path", header: tar.Header{Name: "/buildkite-gha", Size: 1, Typeflag: tar.TypeReg}},
		{name: "symlink", header: tar.Header{Name: "buildkite-gha", Linkname: "target", Typeflag: tar.TypeSymlink}},
		{name: "hardlink", header: tar.Header{Name: "buildkite-gha", Linkname: "target", Typeflag: tar.TypeLink}},
		{name: "directory", header: tar.Header{Name: "buildkite-gha", Typeflag: tar.TypeDir}},
		{name: "extra", header: tar.Header{Name: "extra", Size: 1, Typeflag: tar.TypeReg}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "archive.tar.gz")
			if err := os.WriteFile(archive, archiveWithEntries(t, test.header), 0600); err != nil {
				t.Fatal(err)
			}
			if err := extract(archive, t.TempDir()); err == nil {
				t.Fatal("hostile entry was accepted")
			}
		})
	}

	t.Run("duplicate", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "archive.tar.gz")
		duplicate := targetArchive(t, testTarget, tar.Header{Name: "buildkite-gha", Size: 1, Typeflag: tar.TypeReg})
		if err := os.WriteFile(archive, duplicate, 0600); err != nil {
			t.Fatal(err)
		}
		if err := extract(archive, t.TempDir()); err == nil || !strings.Contains(err.Error(), "invalid archive entry") {
			t.Fatalf("duplicate entry error = %v", err)
		}
	})

	t.Run("trailing gzip member", func(t *testing.T) {
		contents := targetArchive(t, testTarget)
		var trailing bytes.Buffer
		gz := gzip.NewWriter(&trailing)
		_, _ = gz.Write([]byte("trailing"))
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(t.TempDir(), "archive.tar.gz")
		if err := os.WriteFile(archive, append(contents, trailing.Bytes()...), 0600); err != nil {
			t.Fatal(err)
		}
		if err := extract(archive, t.TempDir()); err == nil || !strings.Contains(err.Error(), "trailing data") {
			t.Fatalf("trailing member error = %v", err)
		}
	})
}

func fileDigestMatches(t *testing.T, path, want string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(b)
	return fmt.Sprintf("%x", digest) == want
}
