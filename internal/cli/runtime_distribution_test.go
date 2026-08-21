package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestImporterPlatform(t *testing.T) {
	for _, test := range []struct {
		goos, goarch string
		want         compiler.Platform
	}{
		{goos: "linux", goarch: "amd64", want: compiler.PlatformLinuxAMD64},
		{goos: "darwin", goarch: "arm64", want: compiler.PlatformDarwinARM64},
	} {
		got, err := importerPlatform(test.goos, test.goarch)
		if err != nil || got != test.want {
			t.Fatalf("importerPlatform(%q, %q) = %s, %v", test.goos, test.goarch, got, err)
		}
	}
	if _, err := importerPlatform("linux", "arm64"); err == nil || !strings.Contains(err.Error(), "linux/amd64 or darwin/arm64") {
		t.Fatalf("unsupported importer error = %v", err)
	}
}

func TestUploadRejectsUnsupportedImporterBeforeProcessing(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "linux.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  linux:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo linux\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "darwin-importer")
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "Linux graph", args: []string{workflowPath}},
		{name: "explicit runtimes", args: []string{
			"--runtime-distribution", "linux/amd64=/tmp/buildkite-gha-linux",
			"--runtime-distribution", "darwin/arm64=/tmp/buildkite-gha-darwin",
			workflowPath,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := uploadFromPlatform("linux", "arm64", test.args, &stdout, &stderr, "dev", "dev", transport.Agent{Runner: runner}); code != 1 {
				t.Fatalf("uploadFromPlatform() code = %d, want 1", code)
			}
			if got := stderr.String(); got != "buildkite-gha: upload: importer requires linux/amd64 or darwin/arm64, running on linux/arm64\n" {
				t.Fatalf("stderr = %q", got)
			}
			if stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("unsupported importer performed work: stdout = %q, commands = %d, uploads = %d", stdout.String(), len(runner.commands), len(runner.uploaded))
			}
		})
	}
}

func TestDarwinUploadRequiresLinuxDistributionForLinuxWorkflow(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "linux.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  linux:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo linux\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "darwin-importer")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	code := uploadFromPlatform("darwin", "arm64", []string{"--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev", "dev", transport.Agent{Runner: runner})
	if code != 1 || !strings.Contains(stderr.String(), "runtime distribution for linux/amd64 is required by the selected workflows") {
		t.Fatalf("uploadFromPlatform() = %d, stderr = %q", code, stderr.String())
	}
	if len(runner.uploaded) != 0 {
		t.Fatalf("missing runtime reached artifact upload: %#v", runner.uploaded)
	}
}

func TestLoadRuntimeDistributionsValidatesPlatformBinaryAndSymlink(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	platform, err := compiler.ParsePlatform(runtime.GOOS + "/" + runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	distributions, err := loadRuntimeDistributions(map[compiler.Platform]string{platform: executable})
	if err != nil {
		t.Fatal(err)
	}
	if got := distributions[platform].digest; got != cliTestRuntimeDigest() {
		t.Fatalf("runtime digest = %q, want %q", got, cliTestRuntimeDigest())
	}
	other := compiler.PlatformDarwinARM64
	wantFormat := "Mach-O"
	if platform == compiler.PlatformDarwinARM64 {
		other = compiler.PlatformLinuxAMD64
		wantFormat = "ELF"
	}
	if _, err := loadRuntimeDistributions(map[compiler.Platform]string{other: executable}); err == nil || !strings.Contains(err.Error(), wantFormat) {
		t.Fatalf("%s runtime accepted %s executable: %v", other, platform, err)
	}
	symlink := filepath.Join(t.TempDir(), "runtime")
	if err := os.Symlink(executable, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimeDistributions(map[compiler.Platform]string{platform: symlink}); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("runtime symlink error = %v", err)
	}
}
