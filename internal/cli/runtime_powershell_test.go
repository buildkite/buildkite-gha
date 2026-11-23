package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestInstallPowerShellVerifiesArchiveAndRestrictsEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture executable requires sh")
	}
	tarPath, err := exec.LookPath("tar")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := "#!/bin/sh\ntest -z \"${BUILDKITE_AGENT_ACCESS_TOKEN+x}\" || exit 91\nexec " + strconv.Quote(tarPath) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tar"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "must-not-leak")
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"pwsh":        "#!/bin/sh\ntest -z \"${BUILDKITE_AGENT_ACCESS_TOKEN+x}\" || exit 92\ntest \"$(cat \"$(dirname \"$0\")/support.txt\")\" = supporting-runtime || exit 9\nprintf 'PowerShell " + runtimePowerShellVersion + "\\n'\n",
		"support.txt": "supporting-runtime",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "buildkite-gha/test" {
			t.Errorf("unexpected User-Agent %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write(archive.Bytes())
	}))
	defer server.Close()
	digest := fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
	for _, checksum := range []string{digest, strings.Repeat("0", 64)} {
		t.Run(checksum, func(t *testing.T) {
			root := t.TempDir()
			executable, err := installRuntimePowerShellFrom(t.Context(), root, server.Client(), server.URL, "powershell.tar.gz", checksum, "test")
			if checksum != digest {
				if err == nil || !strings.Contains(err.Error(), "checksum verification failed") {
					t.Fatalf("incorrect checksum: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "pwsh")); !os.IsNotExist(err) {
					t.Fatalf("unverified archive was extracted: %v", err)
				}
				return
			}
			if err != nil || executable != filepath.Join(root, "pwsh") {
				t.Fatalf("PowerShell installation = %q, %v", executable, err)
			}
		})
	}
}
