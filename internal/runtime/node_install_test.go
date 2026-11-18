package runtime

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func nodeTestArchive(t *testing.T, name, member string, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		archive := zip.NewWriter(&output)
		file, err := archive.Create(member)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed, err := gzip.NewWriterLevel(&output, gzip.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		archive := tar.NewWriter(compressed)
		if err := archive.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return output.Bytes()
}

func TestNodeReleaseArchives(t *testing.T) {
	for _, test := range []struct{ os, arch, archive, member string }{
		{"linux", "amd64", "node-v24.18.0-linux-x64.tar.gz", "node-v24.18.0-linux-x64/bin/node"},
		{"linux", "arm64", "node-v24.18.0-linux-arm64.tar.gz", "node-v24.18.0-linux-arm64/bin/node"},
		{"darwin", "arm64", "node-v24.18.0-darwin-arm64.tar.gz", "node-v24.18.0-darwin-arm64/bin/node"},
		{"windows", "amd64", "node-v24.18.0-win-x64.zip", "node-v24.18.0-win-x64/node.exe"},
	} {
		t.Run(test.os+"/"+test.arch, func(t *testing.T) {
			archive, member, err := nodeRelease(24, test.os, test.arch)
			if err != nil || archive != test.archive || member != test.member {
				t.Fatalf("release = %q, %q, %v", archive, member, err)
			}
		})
	}
}

func TestBundledNodeSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX executable fixtures")
	}
	wrong, broken, suitable := t.TempDir(), t.TempDir(), t.TempDir()
	writeNodeExecutable(t, filepath.Join(wrong, "node24/bin/node"), 20)
	brokenNode := filepath.Join(broken, "node24/bin/node")
	writeNodeExecutable(t, brokenNode, 24)
	if err := os.WriteFile(brokenNode, []byte("#!/missing/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(suitable, "node24/bin/node")
	writeNodeExecutable(t, want, 24) // v24.99.0, deliberately not the download pin.
	runner := Runner{nodeRoots: []string{wrong, broken, suitable}, nodeDownloadURL: ":must-not-download"}
	got, err := newJobRun(runner).discoverNode(t.Context(), 24, "")
	if err != nil || got != want {
		t.Fatalf("bundled Node = %q, %v, want %q", got, err, want)
	}
	explicit := filepath.Join(t.TempDir(), "node")
	writeNodeExecutable(t, explicit, 24)
	got, err = newJobRun(runner).discoverNode(t.Context(), 24, explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit Node = %q, %v", got, err)
	}
	if _, err := newJobRun(runner).discoverNode(t.Context(), 24, brokenNode); err == nil {
		t.Fatal("invalid explicit override silently fell back")
	}
}

func TestNodeRuntimeColdAndWarmJob(t *testing.T) {
	// Use the development fixture only to build the HTTP release archive. The
	// Runner gets no explicit Node path and cannot find mise or Node on PATH.
	node := os.Getenv("BUILDKITE_GHA_TEST_PINNED_NODE24")
	if node == "" {
		output, err := exec.Command("mise", "where", "node@"+Node24Version).CombinedOutput()
		if err != nil {
			t.Fatalf("install the pinned Node bootstrap fixture with mise install or set BUILDKITE_GHA_TEST_PINNED_NODE24: %v: %s", err, output)
		}
		node = filepath.Join(strings.TrimSpace(string(output)), "bin", "node")
	}
	data, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyManagedNodeExecutable(t.Context(), 24, node, nodeDigest(runtime.GOOS, runtime.GOARCH, 24)); err != nil {
		t.Fatalf("test requires the pinned upstream Node fixture: %v", err)
	}
	name, member, err := nodeRelease(24, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	archive := nodeTestArchive(t, name, member, data)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	if runtime.GOOS != "windows" {
		path := t.TempDir()
		for _, tool := range []string{"node", "mise"} {
			if err := os.WriteFile(filepath.Join(path, tool), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", path+":/usr/bin:/bin")
	}
	t.Setenv("NODE_OPTIONS", "--require=/nonexistent/ambient-injection.js")
	runner := Runner{NodeCacheDir: t.TempDir(), nodeRoots: []string{}, nodeDownloadURL: server.URL}
	run := func() {
		t.Helper()
		workspace := t.TempDir()
		workflow := ".github/workflows/test.yml"
		writeFixtureFile(t, workspace, workflow, "name: bootstrap\n")
		writeFixtureFile(t, workspace, ".github/actions/js/action.yml", "name: bootstrap\nruns:\n  using: node20\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
		writeFixtureFile(t, workspace, ".github/actions/js/pre.js", "const fs = require('fs'); fs.appendFileSync(process.env.GITHUB_ENV, 'BOOT_PRE=ready\\n'); fs.appendFileSync(process.env.GITHUB_STATE, 'saved=42\\n');")
		writeFixtureFile(t, workspace, ".github/actions/js/main.js", "if (process.env.BOOT_PRE !== 'ready') throw Error('pre did not run'); require('fs').appendFileSync(process.env.GITHUB_ENV, 'BOOT_MAIN=' + process.version + '\\n');")
		writeFixtureFile(t, workspace, ".github/actions/js/post.js", "if (process.env.STATE_saved !== '42') throw Error('lost state'); require('fs').appendFileSync(process.env.GITHUB_ENV, 'BOOT_POST=done\\n');")
		job := runtimePlan(t, workspace, workflow, []runtimeTestStep{{ID: "js", Kind: "uses", Uses: "./.github/actions/js"}})
		result, err := runner.runTestJob(t.Context(), job, workspace)
		if err != nil || result.Conclusion != "success" || result.Env["BOOT_PRE"] != "ready" || result.Env["BOOT_MAIN"] != "v24.18.0" || result.Env["BOOT_POST"] != "done" {
			t.Fatalf("JavaScript lifecycle result = %#v, error = %v", result, err)
		}
	}
	run() // Cold self-hosted executor, no bundled runtime or mise.
	if requests.Load() != 1 {
		t.Fatalf("cold downloads = %d, want one", requests.Load())
	}
	// A corrupt warm cache must be repaired without invoking its content.
	cached := filepath.Join(runner.NodeCacheDir, strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".zip"), filepath.Base(member))
	if err := os.Remove(cached); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("corrupt executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	run()
	if requests.Load() != 2 {
		t.Fatalf("cache repair downloads = %d, want two total", requests.Load())
	}
	// Self-hosted agents may run several jobs against the same empty cache.
	concurrent := runner
	concurrent.NodeCacheDir = t.TempDir()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := newJobRun(concurrent).discoverNode(t.Context(), 24, "")
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Node installation: %v", err)
		}
	}
	if runtime.GOOS == "linux" {
		// Hosted cache paths need not exist or be writable on self-hosted agents.
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		runner.NodeCacheDir = filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(runner.NodeCacheDir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		run()
	}
	server.Close()
	run() // A different job reuses the validated cache with the network offline.
}

func TestNodeDownloadRejectsUnverifiedExecutable(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			name := "node." + format
			payload := []byte("untrusted executable")
			archive := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(archive, nodeTestArchive(t, name, "release/bin/node", payload), 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "node")
			digest := sha256.Sum256([]byte("trusted executable"))
			if err := extractNode(archive, "release/bin/node", destination, hex.EncodeToString(digest[:])); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatalf("extract unverified Node: %v", err)
			}
		})
	}
}

func TestNodeDownloadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	defer cancel()
	runner := Runner{NodeCacheDir: t.TempDir(), nodeRoots: []string{}, nodeDownloadURL: server.URL}
	_, err := newJobRun(runner).discoverNode(ctx, 24, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("download cancellation = %v", err)
	}
	files, err := filepath.Glob(filepath.Join(runner.NodeCacheDir, "*", ".download-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("cancelled download left staging files: %v, %v", files, err)
	}
}
