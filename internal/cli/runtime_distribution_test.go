package cli

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
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

func TestWindowsRuntimeDistributionValidatesPEAndNeedsNoUnixExecuteBit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buildkite-gha.exe")
	command := exec.Command("go", "build", "-o", path, "../../cmd/buildkite-gha")
	command.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cross-build Windows runtime: %v\n%s", err, output)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	distributions, err := loadRuntimeDistributions(map[compiler.Platform]string{compiler.PlatformWindowsAMD64: path})
	if err != nil || len(distributions) != 1 {
		t.Fatalf("load Windows distribution = %#v, %v", distributions, err)
	}
	t.Run("mixed upload", func(t *testing.T) {
		requireImporterHost(t)
		workflow := filepath.Join(t.TempDir(), "mixed.yml")
		if err := os.WriteFile(workflow, []byte(`on: push
jobs:
  linux:
    runs-on: ubuntu-latest
    steps: [{run: echo linux}]
  windows:
    needs: linux
    runs-on: windows-2022
    steps: [{run: Write-Output windows}]
`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BUILDKITE", "true")
		t.Setenv("BUILDKITE_STEP_KEY", "mixed-importer")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		args := []string{"upload", "--event-path", "../../testdata/smoke/events/push.json",
			"--runner-queue", "ubuntu-latest=linux", "--runner-queue", "windows-2022=windows",
			"--runtime-distribution", "windows/amd64=" + path, workflow}
		if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
			t.Fatalf("upload = %d: %s", code, stderr.String())
		}
		plans := map[string]string{}
		for artifactPath, content := range runner.uploaded {
			if strings.HasPrefix(artifactPath, ".buildkite-gha/plans/") {
				job, err := plan.Decode(content)
				if err != nil {
					t.Fatal(err)
				}
				plans[job.Workflow.LogicalJobID] = job.RuntimeDistributionDigest()
			}
		}
		windowsDigest := distributions[compiler.PlatformWindowsAMD64].digest
		if len(plans) != 2 || plans["linux"] != cliTestRuntimeDigest() || plans["windows"] != windowsDigest {
			t.Fatalf("plan runtime bindings = %#v", plans)
		}
		artifactPath, err := buildkitepipeline.DistributionPath(windowsDigest)
		if err != nil {
			t.Fatal(err)
		}
		if transport.Digest(runner.uploaded[artifactPath]) != windowsDigest {
			t.Fatal("Windows executable was not uploaded intact")
		}
	})
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents[0] = 0
	if err := validateRuntimeDistributionBinary(compiler.PlatformWindowsAMD64, contents); err == nil || !strings.Contains(err.Error(), "PE") {
		t.Fatalf("malformed PE error = %v", err)
	}
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	peOffset := binary.LittleEndian.Uint32(contents[0x3c:0x40])
	characteristics := contents[peOffset+4+18 : peOffset+4+20]
	binary.LittleEndian.PutUint16(characteristics, binary.LittleEndian.Uint16(characteristics)|0x2000)
	if err := validateRuntimeDistributionBinary(compiler.PlatformWindowsAMD64, contents); err == nil || !strings.Contains(err.Error(), "not a DLL") {
		t.Fatalf("DLL error = %v", err)
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

func TestRequiredRuntimePlatformsExcludesFailedJobDependencyClosure(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "mixed.yml")
	source := []byte(`on: push
jobs:
  safe:
    runs-on: ubuntu-latest
    steps: [{run: echo safe}]
  deploy:
    runs-on: macos-latest
    environment: production
    steps: [{run: echo deploy}]
  blocked:
    needs: deploy
    runs-on: macos-latest
    steps: [{run: echo blocked}]
`)
	if err := os.WriteFile(workflowPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]compiler.RunnerTarget{
		"ubuntu-latest": {Queue: "linux", Platform: compiler.PlatformLinuxAMD64},
		"macos-latest":  {Queue: "macos", Platform: compiler.PlatformDarwinARM64},
	}
	platforms, admissionErr, err := requiredRuntimePlatforms(t.Context(), workflowPath, source, event, "dev", "sha256:"+strings.Repeat("1", 64), "", targets, agentRunnerResolution{}, nil, nil, compiler.VariableSources{})
	if err != nil {
		t.Fatal(err)
	}
	if admissionErr != nil {
		t.Fatal(admissionErr)
	}
	if !platforms[compiler.PlatformLinuxAMD64] || platforms[compiler.PlatformDarwinARM64] || len(platforms) != 1 {
		t.Fatalf("required runtime platforms = %#v", platforms)
	}
}

func TestRequiredRuntimePlatformsExcludesLaterActionFailure(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), ".github", "workflows", "mixed.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`on: push
jobs:
  safe:
    runs-on: ubuntu-latest
    steps: [{run: echo safe}]
  broken:
    runs-on: macos-latest
    steps:
      - uses: ./.github/actions/missing
`)
	if err := os.WriteFile(workflowPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]compiler.RunnerTarget{
		"ubuntu-latest": {Queue: "linux", Platform: compiler.PlatformLinuxAMD64},
		"macos-latest":  {Queue: "macos", Platform: compiler.PlatformDarwinARM64},
	}
	platforms, admissionErr, err := requiredRuntimePlatforms(t.Context(), workflowPath, source, event, "dev", "sha256:"+strings.Repeat("1", 64), "", targets, agentRunnerResolution{}, nil, nil, compiler.VariableSources{})
	if err != nil {
		t.Fatal(err)
	}
	if admissionErr != nil {
		t.Fatal(admissionErr)
	}
	if !platforms[compiler.PlatformLinuxAMD64] || platforms[compiler.PlatformDarwinARM64] || len(platforms) != 1 {
		t.Fatalf("required runtime platforms = %#v", platforms)
	}
}

func TestRequiredRuntimePlatformsFailsClosedBeforePartialAdmission(t *testing.T) {
	bundle := compiler.Bundle{
		IR: compiler.IR{Jobs: []compiler.JobInstance{
			{Key: "safe", Platform: compiler.PlatformLinuxAMD64},
			{Key: "rejected", Platform: compiler.PlatformDarwinARM64},
		}},
		JobOutcomes: map[string]compiler.JobOutcome{"safe": compiler.JobPlanned, "rejected": compiler.JobPlanned},
		Plans: []compiler.PlanArtifact{
			{Job: plan.Job{Target: plan.Target{StepKey: "safe"}}},
			{Job: plan.Job{Workflow: plan.Workflow{LogicalJobID: "rejected"}, Target: plan.Target{StepKey: "rejected"}, RequiredCapabilities: []string{"privileged-container"}}},
		},
	}
	platforms, admissionErr, err := runtimePlatformsForBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if admissionErr == nil {
		t.Fatal("requiredRuntimePlatforms() admission error = nil")
	}
	if len(platforms) != 0 {
		t.Fatalf("partial admission requested runtime platforms = %#v, want fail-closed empty set", platforms)
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
