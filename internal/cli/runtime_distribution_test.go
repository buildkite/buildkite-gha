package cli

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/http"
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
	for _, label := range []string{"windows-2025", "depot-windows-2025-16"} {
		t.Run("server fallback "+label, func(t *testing.T) {
			requireImporterHost(t)
			workflow := filepath.Join(t.TempDir(), "fallback.yml")
			if err := os.WriteFile(workflow, []byte("on: push\njobs:\n  windows:\n    runs-on: "+label+"\n    steps: [{run: Write-Output windows}]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			const warning = "The Windows queue may run Windows Server 2022 instead of 2025."
			server, requests := runnerResolutionServer(t, http.StatusOK, map[string]map[string]any{
				label: {
					"target":   map[string]string{"queue": "windows-medium", "platform": "windows/amd64"},
					"warnings": []map[string]string{{"code": "runner_label_fallback", "message": warning}},
				},
			})
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "windows-fallback-importer")
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
			t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
			t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "true")
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			args := []string{"upload", "--event-path", "../../testdata/smoke/events/push.json",
				"--runtime-distribution", "windows/amd64=" + path, workflow}
			if code := run(args, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("upload = %d: %s", code, stderr.String())
			}
			jobs := uploadedPlans(t, runner)["windows"]
			if *requests != 1 || len(jobs) != 1 || jobs[0].Target.Queue != "windows-medium" || jobs[0].RuntimeDistributionDigest() != distributions[compiler.PlatformWindowsAMD64].digest {
				t.Fatalf("requests = %d, Windows plans = %#v", *requests, jobs)
			}
			if !strings.Contains(string(lastPipelineUpload(t, runner)), "pwsh -NoLogo -NoProfile -NonInteractive") {
				t.Fatalf("missing Windows bootstrap: %s", lastPipelineUpload(t, runner))
			}
			warned := false
			for _, command := range runner.commands {
				if len(command.args) > 0 && command.args[0] == "annotate" && strings.Contains(string(command.stdin), warning) {
					if !strings.HasPrefix(string(command.stdin), "#### Runner labels were mapped to fallback targets\n") || strings.Contains(string(command.stdin), "Ubuntu") {
						t.Fatalf("incorrect Windows fallback annotation: %s", command.stdin)
					}
					warned = true
				}
			}
			if !warned {
				t.Fatal("missing Windows version fallback warning")
			}
		})
	}
	for _, deferred := range []struct {
		name, workflow, output string
	}{
		{"matrix", continueDeferredMatrixWorkflow, `[{"target":"windows","runner":"windows-2022"}]`},
		{"runs-on", continueRunsOnWorkflow, `["windows-2022"]`},
	} {
		t.Run("deferred Windows "+deferred.name, func(t *testing.T) {
			initial := runContinueInitialUploads(t, deferred.workflow, "--runner-queue", "windows-2022=windows", "--runtime-distribution", "windows/amd64="+path)[0]
			windowsDigest := distributions[compiler.PlatformWindowsAMD64].digest
			if initial.artifact.Runtimes["windows/amd64"] != windowsDigest {
				t.Fatalf("deferred runtimes = %#v, want Windows distribution %s", initial.artifact.Runtimes, windowsDigest)
			}
			runner := initial.continueRunner(initial.producerManifest(t, "success", deferred.output))
			code, _, stderr := runContinue(t, runner, initial.digest)
			if code != 0 {
				t.Fatalf("continue = %d: %s", code, stderr)
			}
			jobs := uploadedPlans(t, runner)["build"]
			if len(jobs) != 1 || jobs[0].Target.Queue != "windows" || jobs[0].RuntimeDistributionDigest() != windowsDigest {
				t.Fatalf("deferred Windows plans = %#v", jobs)
			}
			_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
			if len(steps) != 2 || !strings.HasPrefix(steps[0].Command, "pwsh -NoLogo -NoProfile -NonInteractive") {
				t.Fatalf("deferred Windows pipeline = %s", lastPipelineUpload(t, runner))
			}
			for _, missingWindows := range []bool{false, true} {
				replay := initial.continueRunner(initial.producerManifest(t, "success", deferred.output))
				replay.pipelineUploadErr = errors.New("pipeline upload: duplicate step key")
				replay.stepAttributes = make(map[string]map[string]string, len(steps))
				for _, step := range steps {
					replay.stepAttributes[step.Key] = map[string]string{"command": step.Command}
				}
				if missingWindows {
					delete(replay.stepAttributes, steps[0].Key)
				}
				code, stdout, stderr := runContinue(t, replay, initial.digest)
				if missingWindows {
					if code != 1 || !strings.Contains(stderr, "pipeline upload: duplicate step key") || !strings.Contains(stderr, "Retry the whole build") {
						t.Fatalf("missing Windows step replay = %d: %s", code, stderr)
					}
				} else if code != 0 || !strings.Contains(stdout, "were already uploaded by an earlier run") {
					t.Fatalf("Windows replay = %d: %s\n%s", code, stdout, stderr)
				}
			}
		})
	}
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
	platforms, _, admissionErr, err := requiredRuntimePlatforms(t.Context(), hostedCompileRequest{WorkflowPath: workflowPath, WorkflowSource: source, EventSource: event, Version: "dev", DistributionDigest: "sha256:" + strings.Repeat("1", 64), RunnerTargets: targets})
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
	platforms, _, admissionErr, err := requiredRuntimePlatforms(t.Context(), hostedCompileRequest{WorkflowPath: workflowPath, WorkflowSource: source, EventSource: event, Version: "dev", DistributionDigest: "sha256:" + strings.Repeat("1", 64), RunnerTargets: targets})
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
	platforms, _, admissionErr, err := runtimePlatformsForBundle(bundle)
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
