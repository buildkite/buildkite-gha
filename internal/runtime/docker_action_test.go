package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestValidateDockerMountPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path string
		ok         bool
	}{
		{"directory", dir, true}, {"empty", "", false}, {"relative", ".", false},
		{"missing", filepath.Join(t.TempDir(), "missing"), false}, {"file", file, false},
		{"comma", dir + ",x", false}, {"double quote", dir + `"x`, false},
		{"single quote", dir + "'x", false}, {"newline", dir + "\nx", false},
		{"carriage return", dir + "\rx", false}, {"nul", dir + "\x00x", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateDockerMountPath(test.path)
			if (err == nil) != test.ok {
				t.Fatalf("validateDockerMountPath(%q) = %v, want success %v", test.path, err, test.ok)
			}
		})
	}
}

func TestBoundedDockerOutputRejectsOversizedOutput(t *testing.T) {
	script := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ni=0; while [ $i -lt 5000 ]; do printf x; i=$((i+1)); done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := boundedDockerOutput(t.Context(), nil, script, "buildx", "inspect")
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") || len(out) != 4096 {
		t.Fatalf("boundedDockerOutput() = %d bytes, %v", len(out), err)
	}
}

func TestStageDockerSource(t *testing.T) {
	root := t.TempDir()
	action := filepath.Join(root, "action")
	if err := os.Mkdir(action, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(action, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(action, "entrypoint.sh"), []byte("#!/bin/sh\n"), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("unbound git metadata\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := source.DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stageDockerSource(root, action, "wrong"); err == nil {
		t.Fatal("digest mismatch succeeded")
	}
	stage, err := stageDockerSource(root, action, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(stage.root) }()
	if err := os.WriteFile(filepath.Join(action, "Dockerfile"), []byte("MUTATED\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(stage.action, "Dockerfile"))
	if err != nil || string(got) != "FROM scratch\n" {
		t.Fatalf("staged bytes = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(stage.action, "entrypoint.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("staged executable mode = %v, %v, want 0755", info, err)
	}
	if _, err := os.Stat(filepath.Join(stage.root, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged source contains excluded .git metadata: %v", err)
	}

	symlinkRoot := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(symlinkRoot, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DigestTree(symlinkRoot); err == nil {
		t.Fatal("DigestTree unexpectedly accepted symlink")
	}
}

type fakeDocker struct {
	path       string
	root       string
	transcript string
	ready      string
}

type fakeDockerCall struct {
	config, metadata string
	args             []string
}

func TestRunDockerFakeLifecycle(t *testing.T) {
	fake := newFakeDocker(t, "success")
	action := fakeDockerAction(t)
	t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "ambient-config"))
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "ambient-context")
	t.Setenv("BUILDX_BUILDER", "ambient-builder")
	t.Setenv("BUILDKIT_HOST", "tcp://ambient.invalid:1234")
	var logs bytes.Buffer
	result, err := (Runner{Docker: fake.path, Stdout: &logs, Stderr: &logs}).runDockerAction(t.Context(), action)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["container"] != "ran" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" || result.State["docker_state"] != "seen" || result.Summary != "docker action summary\n" {
		t.Fatalf("Docker result = %#v", result)
	}
	if strings.Contains(logs.String(), "fake-docker-secret") || !strings.Contains(logs.String(), "masked fake probe: ***") {
		t.Fatalf("Docker logs were not masked: %q", logs.String())
	}

	calls := fake.calls(t)
	wantCommands := [][]string{
		{"buildx", "inspect", "default"},
		{"buildx", "build"},
		{"run"},
		{"ps", "--all", "--quiet"},
		{"stop", "--time", "2"},
		{"rm", "--force"},
		{"image", "ls", "--all", "--quiet"},
		{"image", "rm", "--force"},
		{"ps", "--all", "--quiet"},
		{"image", "ls", "--all", "--quiet"},
	}
	if len(calls) != len(wantCommands) {
		t.Fatalf("Docker calls = %#v, want %d", calls, len(wantCommands))
	}
	for i, want := range wantCommands {
		if !slices.Equal(calls[i].args[:len(want)], want) {
			t.Fatalf("Docker call %d = %#v, want prefix %#v", i, calls[i].args, want)
		}
		if calls[i].config != calls[0].config || !strings.Contains(calls[i].metadata, ";mode=700;entries=0;host=unset;context=unset;builder=unset;buildkit=unset") {
			t.Fatalf("Docker call %d did not use one empty private config: %q", i, calls[i].metadata)
		}
	}
	if _, statErr := os.Stat(calls[0].config); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("private Docker config remains: %v", statErr)
	}

	build := calls[1].args
	if !slices.Equal(build[:6], []string{"buildx", "build", "--builder", "default", "--load", "--tag"}) {
		t.Fatalf("build arguments = %#v", build)
	}
	dockerfile := argumentAfter(t, build, "--file")
	contextPath := build[len(build)-1]
	originalAction := fixturePath(t, "actions", "docker")
	if strings.HasPrefix(dockerfile, originalAction) || filepath.Dir(dockerfile) != contextPath {
		t.Fatalf("build did not use its private staged action: file %q, context %q", dockerfile, contextPath)
	}
	if _, statErr := os.Stat(contextPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("staged context remains: %v", statErr)
	}
	staged, err := os.ReadFile(filepath.Join(fake.root, "staged-Dockerfile"))
	wantDockerfile, wantErr := os.ReadFile(filepath.Join(originalAction, "Dockerfile"))
	if err != nil || wantErr != nil || !bytes.Equal(staged, wantDockerfile) {
		t.Fatalf("staged Dockerfile = %q, %v", staged, err)
	}

	run := calls[2].args
	var mounts []string
	for i, arg := range run {
		if arg == "--mount" && i+1 < len(run) {
			mounts = append(mounts, run[i+1])
		}
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(action.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 3 || !strings.HasSuffix(mounts[0], ",target=/github/file_commands") || mounts[1] != "type=bind,source="+resolvedWorkspace+",target=/github/workspace" || !strings.HasSuffix(mounts[2], ",target=/github/runner_temp") {
		t.Fatalf("fixed Docker mounts = %#v", mounts)
	}
	if workdir := argumentAfter(t, run, "--workdir"); workdir != "/github/workspace" {
		t.Fatalf("Docker working directory = %q", workdir)
	}
	commandFiles := strings.TrimSuffix(strings.TrimPrefix(mounts[0], "type=bind,source="), ",target=/github/file_commands")
	if _, statErr := os.Stat(commandFiles); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("command-file directory remains: %v", statErr)
	}
	runText := strings.Join(run, "\x00")
	first, last := strings.Index(runText, "A_FIRST=a"), strings.Index(runText, "Z_LAST=z")
	if first < 0 || last < 0 || first > last {
		t.Fatalf("Docker environment is not sorted: %#v", run)
	}
	for _, prefix := range []string{"PATH=", "RUNNER_TOOL_CACHE="} {
		for _, argument := range run {
			if strings.HasPrefix(argument, prefix) {
				t.Fatalf("Docker environment contains implicit host value %q: %#v", argument, run)
			}
		}
	}
	for name, want := range map[string]string{"GITHUB_WORKSPACE=/github/workspace": "one fixed workspace", "RUNNER_TEMP=/github/runner_temp": "one translated runner temp"} {
		count := 0
		for _, argument := range run {
			if argument == name {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("Docker environment has %d copies of %s, want %s: %#v", count, name, want, run)
		}
	}
	for _, value := range []string{"GITHUB_ENV=/github/file_commands/env", "GITHUB_OUTPUT=/github/file_commands/output", "GITHUB_PATH=/github/file_commands/path", "GITHUB_STATE=/github/file_commands/state", "GITHUB_STEP_SUMMARY=/github/file_commands/summary"} {
		if !slices.Contains(run, value) {
			t.Fatalf("Docker environment omits %q: %#v", value, run)
		}
	}
	owner := argumentAfter(t, build, "--label")
	if argumentAfter(t, run, "--label") != owner || !strings.Contains(strings.Join(calls[3].args, "\x00"), "label="+owner) || !strings.Contains(strings.Join(calls[8].args, "\x00"), "label="+owner) {
		t.Fatalf("Docker ownership label is not stable across lifecycle")
	}
	for _, call := range calls {
		text := strings.Join(call.args, " ")
		for _, forbidden := range []string{"--privileged", "--network", "--device", "/var/run/docker.sock", " prune"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("Docker call contains forbidden option %q: %s", forbidden, text)
			}
		}
	}
}

func TestRunDockerAppendsExactArgsAfterImage(t *testing.T) {
	fake := newFakeDocker(t, "success")
	action := fakeDockerAction(t)
	action.Args = []string{"--privileged", "", "  ", `$(touch /tmp/nope); * | &`, "a|b"}
	if _, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), action); err != nil {
		t.Fatal(err)
	}
	argv := fakeDockerRunArgv(t, fake)
	imageBytes, err := os.ReadFile(filepath.Join(fake.root, "image-name"))
	if err != nil {
		t.Fatal(err)
	}
	image := string(imageBytes)
	imageIndex := slices.Index(argv, image)
	if imageIndex < 0 {
		t.Fatalf("image absent from Docker argv: %#v", argv)
	}
	if !slices.Equal(argv[imageIndex+1:], action.Args) {
		t.Fatalf("Docker action argv suffix = %#v, want %#v", argv[imageIndex+1:], action.Args)
	}
	if slices.Contains(argv[:imageIndex], "--entrypoint") || slices.Contains(argv[:imageIndex], "--privileged") {
		t.Fatalf("action args became Docker options: %#v", argv)
	}
}

func TestRunPrebuiltDockerUsesPulledImageEntrypointAndExactArgs(t *testing.T) {
	fake := newFakeDocker(t, "success")
	t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "ambient-config"))
	t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2375")
	t.Setenv("DOCKER_CONTEXT", "ambient-context")
	t.Setenv("BUILDX_BUILDER", "ambient-builder")
	t.Setenv("BUILDKIT_HOST", "tcp://ambient.invalid:1234")
	action := fakeDockerAction(t)
	action.Path, action.SourceRoot, action.SourceDigest = "", "", ""
	action.Image = "busybox@sha256:" + strings.Repeat("a", 64)
	action.Entrypoint = "/bin/echo"
	action.Args = []string{"", "  ", "--privileged", `$(echo no-shell)`}
	if _, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), action); err != nil {
		t.Fatal(err)
	}
	calls := fake.calls(t)
	if len(calls) == 0 || !slices.Equal(calls[0].args, []string{"pull", action.Image}) {
		t.Fatalf("first Docker call = %#v, want anonymous pull", calls)
	}
	if callIndex(calls, "buildx") >= 0 {
		t.Fatalf("prebuilt action invoked buildx: %#v", calls)
	}
	argv := fakeDockerRunArgv(t, fake)
	imageIndex := slices.Index(argv, "sha256:"+strings.Repeat("c", 64))
	if imageIndex < 0 || !slices.Equal(argv[imageIndex+1:], action.Args) {
		t.Fatalf("Docker run argv = %#v, want exact args after pulled image ID", argv)
	}
	entrypoint := slices.Index(argv[:imageIndex], "--entrypoint")
	if entrypoint < 0 || entrypoint+1 >= imageIndex || argv[entrypoint+1] != action.Entrypoint {
		t.Fatalf("Docker run argv = %#v, want metadata entrypoint", argv)
	}
	for _, call := range calls {
		if call.config != calls[0].config || !strings.Contains(call.metadata, ";mode=700;entries=0;host=unset;context=unset;builder=unset;buildkit=unset") {
			t.Fatalf("Docker call did not use one empty private config: %#v", call)
		}
	}
	if _, err := os.Stat(calls[0].config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private Docker config remains: %v", err)
	}
}

func TestRunJobPullsPrebuiltDockerImageBeforeSteps(t *testing.T) {
	requireLinuxAMD64(t)
	fake := newFakeDocker(t, "success")
	workspace := t.TempDir()
	workflowPath := ".github/workflows/prebuilt.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: prebuilt\n")
	actionPath := ".github/actions/prebuilt"
	image := "busybox@sha256:" + strings.Repeat("b", 64)
	writeFixtureFile(t, workspace, actionPath+"/action.yml", `name: Prebuilt Docker
inputs:
  message:
    default: from-default
runs:
  using: docker
  image: docker://`+image+`
  entrypoint: /bin/echo
  args:
    - ${{ inputs.message }}
`)
	lockID := "a-0000000000000001"
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "prebuilt", Kind: "uses", Uses: "./" + actionPath, Action: &plan.ActionSelector{Lock: lockID}}})
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: actionPath, SourceDigest: digestTree(t, filepath.Join(workspace, actionPath)), DockerImage: image}}
	result, err := (Runner{Docker: fake.path}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	calls := fake.calls(t)
	pull, run := callIndex(calls, "pull", image), callIndex(calls, "run")
	if pull < 0 || run < pull {
		t.Fatalf("prebuilt image was not pulled before execution: %#v", calls)
	}
	if calls[pull].config != calls[run].config || !strings.Contains(calls[pull].metadata, ";entries=0;") {
		t.Fatalf("pull and run did not share an empty private Docker config: %#v", calls)
	}
	argv := fakeDockerRunArgv(t, fake)
	imageIndex := slices.Index(argv, "sha256:"+strings.Repeat("c", 64))
	if imageIndex < 0 || !slices.Equal(argv[imageIndex+1:], []string{"from-default"}) {
		t.Fatalf("Docker run argv = %#v, want evaluated default after pulled image ID", argv)
	}
	if _, err := os.Stat(calls[pull].config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job private Docker config remains: %v", err)
	}

	mismatchFake := newFakeDocker(t, "success")
	mismatched := job
	mismatched.Actions = append([]plan.ActionLock(nil), job.Actions...)
	mismatched.Actions[0].DockerImage = "alpine:3.20"
	secondActionPath := ".github/actions/prebuilt-second"
	writeFixtureFile(t, workspace, secondActionPath+"/action.yml", `name: Second prebuilt Docker
runs:
  using: docker
  image: docker://`+image+`
`)
	secondLockID := "a-0000000000000002"
	mismatched.Program.Job.Steps = append(mismatched.Program.Job.Steps, normalizeRuntimeTestStep(runtimeTestStep{ID: "prebuilt-second", Kind: "uses", Uses: "./" + secondActionPath, Action: &plan.ActionSelector{Lock: secondLockID}}))
	mismatched.Actions = append(mismatched.Actions, plan.ActionLock{ID: secondLockID, Source: "workspace", Path: secondActionPath, SourceDigest: digestTree(t, filepath.Join(workspace, secondActionPath)), DockerImage: image})
	if _, err := (Runner{Docker: mismatchFake.path}).runTestJob(t.Context(), mismatched, workspace); err == nil || !strings.Contains(err.Error(), "Docker image does not match its immutable lock") {
		t.Fatalf("RunJob() mismatched planned image error = %v", err)
	}
	if _, err := os.Stat(mismatchFake.transcript); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched plan Docker transcript exists: %v", err)
	}

	unplannedFake := newFakeDocker(t, "success")
	unplanned := job
	unplanned.Actions = append([]plan.ActionLock(nil), job.Actions...)
	unplanned.Actions[0].DockerImage = ""
	if _, err := (Runner{Docker: unplannedFake.path}).runTestJob(t.Context(), unplanned, workspace); err == nil || !strings.Contains(err.Error(), "Docker image does not match its immutable lock") {
		t.Fatalf("RunJob() unplanned metadata image error = %v", err)
	}
	if _, err := os.Stat(unplannedFake.transcript); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unplanned metadata image Docker transcript exists: %v", err)
	}
}

func TestRunPrebuiltDockerPullFailureCleansPrivateConfig(t *testing.T) {
	fake := newFakeDocker(t, "pull-fail")
	action := fakeDockerAction(t)
	action.Image = "alpine:3.20"
	if _, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), action); err == nil || !strings.Contains(err.Error(), "pull prebuilt Docker action image") {
		t.Fatalf("runDockerAction() error = %v, want pull failure", err)
	}
	calls := fake.calls(t)
	if len(calls) != 3 {
		t.Fatalf("Docker pull calls = %#v, want three bounded retries", calls)
	}
	if _, err := os.Stat(calls[0].config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private Docker config remains after pull failure: %v", err)
	}
}

func TestRunPrebuiltDockerInspectFailureCleansPrivateConfig(t *testing.T) {
	fake := newFakeDocker(t, "inspect-image-fail")
	action := fakeDockerAction(t)
	action.Image = "alpine:3.20"
	if _, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), action); err == nil || !strings.Contains(err.Error(), "inspect prebuilt Docker action image") {
		t.Fatalf("runDockerAction() error = %v, want image inspection failure", err)
	}
	calls := fake.calls(t)
	if callIndex(calls, "pull", action.Image) < 0 || callIndex(calls, "image", "inspect") < 0 || callIndex(calls, "run") >= 0 {
		t.Fatalf("Docker calls = %#v, want pull and failed inspection only", calls)
	}
	if _, err := os.Stat(calls[0].config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private Docker config remains after inspect failure: %v", err)
	}
}

func TestRunPrebuiltDockerFailureCleansOwnedContainer(t *testing.T) {
	fake := newFakeDocker(t, "run-fail")
	action := fakeDockerAction(t)
	action.Image = "alpine:3.20"
	result, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), action)
	if err == nil || !strings.Contains(err.Error(), `run Docker action "fake Docker"`) {
		t.Fatalf("runDockerAction() error = %v, want run failure", err)
	}
	if result.Outputs["container"] != "ran" {
		t.Fatalf("prebuilt Docker failure effects = %#v", result)
	}
	calls := fake.calls(t)
	if callIndex(calls, "stop", "--time", "2") < 0 || callIndex(calls, "rm", "--force") < 0 {
		t.Fatalf("prebuilt Docker container cleanup was skipped: %#v", calls)
	}
	if callIndex(calls, "image", "rm") >= 0 {
		t.Fatalf("prebuilt Docker cleanup removed a shared pulled image: %#v", calls)
	}
	if _, err := os.Stat(calls[0].config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private Docker config remains after run failure: %v", err)
	}
}

func TestRunJobResolvesDockerArgsFromInvocationInputsAndDefaults(t *testing.T) {
	requireLinuxAMD64(t)
	fake := newFakeDocker(t, "success")
	workspace := t.TempDir()
	workflowPath := ".github/workflows/docker.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: Docker args\n")
	writeFixtureFile(t, workspace, "action/action.yml", `name: Docker args
inputs:
  supplied:
    default: unused
  fallback:
    default: default value
  blank:
    default: ""
runs:
  using: docker
  image: Dockerfile
  args:
    - ${{ inputs.supplied }}
    - prefix-${{ inputs['fallback'] }}
    - ${{ inputs.blank }}
    - "  "
`)
	writeFixtureFile(t, workspace, "action/Dockerfile", "FROM scratch\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID: "docker", Kind: "uses", Uses: "./action", With: map[string]string{"supplied": "--privileged"},
	}})
	job.RequiredCapabilities = []string{"docker", "network"}
	result, err := (Runner{Docker: fake.path}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	argv := fakeDockerRunArgv(t, fake)
	imageBytes, err := os.ReadFile(filepath.Join(fake.root, "image-name"))
	if err != nil {
		t.Fatal(err)
	}
	image := string(imageBytes)
	imageIndex := slices.Index(argv, image)
	want := []string{"--privileged", "prefix-default value", "", "  "}
	if imageIndex < 0 {
		t.Fatalf("image absent from Docker argv: %#v", argv)
	}
	if !slices.Equal(argv[imageIndex+1:], want) {
		t.Fatalf("resolved Docker args = %#v, want %#v; full argv %#v", argv[imageIndex+1:], want, argv)
	}
}

func TestRunJobRevalidatesDockerArgsWithInputsOnly(t *testing.T) {
	requireLinuxAMD64(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/docker.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: Docker args\n")
	writeFixtureFile(t, workspace, "action/action.yml", `runs:
  using: docker
  image: Dockerfile
  args:
    - ${{ secrets.token }}
`)
	writeFixtureFile(t, workspace, "action/Dockerfile", "FROM scratch\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "docker", Kind: "uses", Uses: "./action"}})
	job.RequiredCapabilities = []string{"docker", "network"}
	_, err := (Runner{Docker: filepath.Join(t.TempDir(), "docker-must-not-run")}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "docker action argument") || !strings.Contains(err.Error(), "only inputs.<name>") {
		t.Fatalf("RunJob() error = %v, want normalized inputs-only rejection", err)
	}
}

func TestRunDockerPreservesExplicitPath(t *testing.T) {
	requireLinuxAMD64(t)
	t.Parallel()

	for _, test := range []struct {
		name       string
		jobPATH    string
		stepPATH   string
		actionPATH string
		wantPATH   string
	}{
		{name: "job", jobPATH: "/job/bin", wantPATH: "/job/bin"},
		{name: "step", stepPATH: "/step/bin", wantPATH: "/step/bin"},
		{name: "action", actionPATH: "/action/bin", wantPATH: "/action/bin"},
		{name: "job over action default", jobPATH: "/job/bin", actionPATH: "/action/bin", wantPATH: "/job/bin"},
		{name: "step over action default", stepPATH: "/step/bin", actionPATH: "/action/bin", wantPATH: "/step/bin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker(t, "success")
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: Docker PATH test\n")
			actionMetadata := "name: Docker PATH test\nruns:\n  using: docker\n  image: Dockerfile\n"
			if test.actionPATH != "" {
				actionMetadata += "  env:\n    PATH: " + test.actionPATH + "\n"
			}
			writeFixtureFile(t, workspace, ".github/actions/docker/action.yml", actionMetadata)
			writeFixtureFile(t, workspace, ".github/actions/docker/Dockerfile", "FROM scratch\n")
			step := runtimeTestStep{ID: "docker", Kind: "uses", Uses: "./.github/actions/docker"}
			if test.stepPATH != "" {
				step.Env = map[string]string{"PATH": test.stepPATH}
			}
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{step})
			job.RequiredCapabilities = []string{"docker", "network"}
			if test.jobPATH != "" {
				job.Env = map[string]string{"PATH": test.jobPATH}
			}
			if _, err := (Runner{Docker: fake.path}).runTestJob(t.Context(), job, workspace); err != nil {
				t.Fatal(err)
			}
			calls := fake.calls(t)
			runIndex := callIndex(calls, "run")
			if runIndex < 0 {
				t.Fatalf("Docker run absent: %#v", calls)
			}
			if !slices.Contains(calls[runIndex].args, "PATH="+test.wantPATH) {
				t.Fatalf("Docker environment does not preserve explicit PATH %q: %#v", test.wantPATH, calls[runIndex].args)
			}
		})
	}
}

func TestRunDockerPreservesPathWrittenThroughGitHubEnv(t *testing.T) {
	requireLinuxAMD64(t)
	fake := newFakeDocker(t, "success")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: Docker dynamic PATH test\n")
	writeFixtureFile(t, workspace, ".github/actions/docker/action.yml", "name: Docker dynamic PATH test\nruns:\n  using: docker\n  image: Dockerfile\n")
	writeFixtureFile(t, workspace, ".github/actions/docker/Dockerfile", "FROM scratch\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
		{ID: "path", Kind: "run", Command: `printf '%s\n' 'PATH=/dynamic/bin' >> "$GITHUB_ENV"`},
		{ID: "docker", Kind: "uses", Uses: "./.github/actions/docker"},
	})
	job.RequiredCapabilities = []string{"docker", "network"}
	if _, err := (Runner{Docker: fake.path}).runTestJob(t.Context(), job, workspace); err != nil {
		t.Fatal(err)
	}
	calls := fake.calls(t)
	runIndex := callIndex(calls, "run")
	if runIndex < 0 || !slices.Contains(calls[runIndex].args, "PATH=/dynamic/bin") {
		t.Fatalf("Docker environment does not preserve dynamic PATH: %#v", calls)
	}
}

func TestRunDockerActionEnvironmentUsesInvocationBeforeActionDefaults(t *testing.T) {
	requireLinuxAMD64(t)
	fake := newFakeDocker(t, "success")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: Docker environment precedence test\n")
	writeFixtureFile(t, workspace, ".github/actions/docker/action.yml", `name: Docker environment precedence test
inputs:
  value:
    default: default-input
runs:
  using: docker
  image: Dockerfile
  env:
    MODE: action
    ACTION_ONLY: default
    INPUT_VALUE: action
`)
	writeFixtureFile(t, workspace, ".github/actions/docker/Dockerfile", "FROM scratch\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{
		ID: "docker", Kind: "uses", Uses: "./.github/actions/docker",
		With: map[string]string{"value": "caller-input"},
		Env:  map[string]string{"MODE": "caller"},
	}})
	job.RequiredCapabilities = []string{"docker", "network"}
	if _, err := (Runner{Docker: fake.path}).runTestJob(t.Context(), job, workspace); err != nil {
		t.Fatal(err)
	}
	calls := fake.calls(t)
	runIndex := callIndex(calls, "run")
	if runIndex < 0 {
		t.Fatalf("Docker run absent: %#v", calls)
	}
	for _, want := range []string{"MODE=caller", "ACTION_ONLY=default", "INPUT_VALUE=caller-input"} {
		if !slices.Contains(calls[runIndex].args, want) {
			t.Fatalf("Docker environment omits %q: %#v", want, calls[runIndex].args)
		}
	}
}

func TestRunDockerRejectsUntrustedBuilderBeforeExecution(t *testing.T) {
	for _, scenario := range []string{"remote", "multiline", "oversized", "inspect-fail"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newFakeDocker(t, scenario)
			if _, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), fakeDockerAction(t)); err == nil {
				t.Fatal("untrusted builder was accepted")
			}
			calls := fake.calls(t)
			if len(calls) != 1 || !slices.Equal(calls[0].args, []string{"buildx", "inspect", "default"}) {
				t.Fatalf("Docker calls after rejected driver = %#v", calls)
			}
		})
	}
}

func TestRunDockerFakeFailuresCleanOwnedResources(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"build-fail", "run-fail", "leftover", "query-fail"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newFakeDocker(t, scenario)
			if _, err := (Runner{Docker: fake.path, CleanupTimeout: 2 * time.Second}).runDockerAction(t.Context(), fakeDockerAction(t)); err == nil {
				t.Fatal("Docker failure returned success")
			}
			calls := fake.calls(t)
			if callIndex(calls, "image", "ls") < 0 || callIndex(calls, "image", "rm", "--force") < 0 {
				t.Fatalf("image cleanup was skipped: %#v", calls)
			}
			if scenario != "build-fail" && (callIndex(calls, "ps", "--all") < 0 || callIndex(calls, "rm", "--force") < 0) {
				t.Fatalf("container cleanup was skipped: %#v", calls)
			}
			if scenario != "leftover" {
				for _, resource := range []string{"image", "container"} {
					if _, err := os.Stat(filepath.Join(fake.root, resource)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s remains after %s cleanup: %v", resource, scenario, err)
					}
				}
			}
			if _, err := os.Stat(calls[0].config); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private Docker config remains after %s: %v", scenario, err)
			}
		})
	}
}

func TestRunDockerProcessesFileCommandsAfterFailure(t *testing.T) {
	fake := newFakeDocker(t, "run-fail")
	result, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), fakeDockerAction(t))
	if err == nil || !strings.Contains(err.Error(), `run Docker action "fake Docker"`) {
		t.Fatalf("runDockerAction() error = %v", err)
	}
	if result.Outputs["container"] != "ran" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" || result.State["docker_state"] != "seen" || result.Summary != "docker action summary\n" || !slices.Equal(result.Paths, []string{"/fake/action/bin"}) {
		t.Fatalf("Docker failure result = %#v", result)
	}
}

func TestRunDockerJoinsRunAndFileCommandFailures(t *testing.T) {
	fake := newFakeDocker(t, "run-fail-invalid-files")
	_, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), fakeDockerAction(t))
	if err == nil || !strings.Contains(err.Error(), `run Docker action "fake Docker"`) || !strings.Contains(err.Error(), `process Docker action "fake Docker" file commands`) || !strings.Contains(err.Error(), "invalid file command") {
		t.Fatalf("runDockerAction() error = %v", err)
	}
}

func TestRunDockerReadsOnlyOriginalCommandFiles(t *testing.T) {
	t.Parallel()

	fake := newFakeDocker(t, "replace-files")
	result, err := (Runner{Docker: fake.path}).runDockerAction(t.Context(), fakeDockerAction(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["container"] != "ran" || result.Outputs["poison"] != "" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" || result.State["docker_state"] != "seen" || result.State["poison"] != "" || result.Summary != "docker action summary\n" || !slices.Equal(result.Paths, []string{"/fake/action/bin"}) {
		t.Fatalf("Docker result from replaced command paths = %#v", result)
	}
}

func TestRunDockerCancellationCleansOwnedResources(t *testing.T) {
	t.Parallel()

	fake := newFakeDocker(t, "cancel")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := (Runner{Docker: fake.path, CleanupTimeout: 2 * time.Second, InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}).runDockerAction(ctx, fakeDockerAction(t))
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(fake.ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake Docker run did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runDockerAction() error = %v, want context cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runDockerAction cancellation exceeded bound")
	}
	calls := fake.calls(t)
	for _, command := range [][]string{{"stop"}, {"rm", "--force"}, {"image", "rm", "--force"}, {"ps", "--all"}, {"image", "ls"}} {
		if callIndex(calls, command...) < 0 {
			t.Fatalf("cancellation omitted Docker command %#v: %#v", command, calls)
		}
	}
}

func newFakeDocker(t *testing.T, scenario string) fakeDocker {
	t.Helper()
	root := t.TempDir()
	script := strings.NewReplacer(
		"__ROOT__", strconv.Quote(root),
		"__SCENARIO__", strconv.Quote(scenario),
	).Replace(`#!/usr/bin/env bash
set -euo pipefail
state=__ROOT__
scenario=__SCENARIO__
transcript="$state/transcript"
record() {
  local mode entries
  if ! mode="$(stat -c %a "$DOCKER_CONFIG" 2>/dev/null)"; then
    mode="$(stat -f %Lp "$DOCKER_CONFIG")"
  fi
  entries="$(find "$DOCKER_CONFIG" -mindepth 1 -maxdepth 1 -print -quit | wc -l | tr -d '[:space:]')"
  printf 'config=%s;mode=%s;entries=%s;host=%s;context=%s;builder=%s;buildkit=%s' \
    "$DOCKER_CONFIG" "$mode" "$entries" "${DOCKER_HOST-unset}" "${DOCKER_CONTEXT-unset}" \
    "${BUILDX_BUILDER-unset}" "${BUILDKIT_HOST-unset}" >> "$transcript"
  printf '|%s' "$@" >> "$transcript"
  printf '\n' >> "$transcript"
}
record "$@"

if [[ "$1" == buildx && "$2" == inspect ]]; then
  case "$scenario" in
    inspect-fail) exit 31 ;;
    remote) printf 'Name: default\nDriver: remote\n'; exit 0 ;;
    multiline) printf 'Name: default\nDriver: docker\nDriver: remote\n'; exit 0 ;;
    oversized) i=0; while (( i < 5000 )); do printf x; (( i += 1 )); done; exit 0 ;;
    *) printf 'Name: default\nDriver: docker\n'; exit 0 ;;
  esac
fi

if [[ "$1" == buildx && "$2" == build ]]; then
  args=("$@")
  dockerfile=''
  image=''
  owner=''
  for ((i = 0; i < ${#args[@]}; i++)); do
    case "${args[$i]}" in
      --file) dockerfile="${args[$((i + 1))]}" ;;
      --tag) image="${args[$((i + 1))]}" ;;
      --label) owner="${args[$((i + 1))]}" ;;
    esac
  done
  [[ -n "$dockerfile" && -n "$image" && -n "$owner" ]] || exit 38
  context="${args[$((${#args[@]} - 1))]}"
  printf '%s' "$dockerfile" > "$state/dockerfile-path"
  printf '%s' "$context" > "$state/context-path"
  printf '%s' "$image" > "$state/image-name"
  printf '%s' "$image" | sed 's/-image-/-container-/' > "$state/container-name"
  printf '%s' "$owner" > "$state/owner"
  cp "$dockerfile" "$state/staged-Dockerfile"
  touch "$state/image"
  [[ "$scenario" != build-fail ]] || exit 32
  exit 0
fi

if [[ "$1" == pull ]]; then
  [[ $# == 2 ]] || exit 49
  printf '%s' "$2" > "$state/image-name"
  [[ "$scenario" != pull-fail ]] || exit 50
  exit 0
fi

if [[ "$1" == image && "$2" == inspect ]]; then
  [[ $# == 5 && "$3" == --format && "$4" == '{{.Id}}' && "$5" == "$(cat "$state/image-name")" ]] || exit 51
  [[ "$scenario" != inspect-image-fail ]] || exit 52
  image_id='sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'
  printf '%s' "$image_id" > "$state/image-name"
  printf '%s\n' "$image_id"
  exit 0
fi

if [[ "$1" == run ]]; then
	printf '%s\0' "$@" > "$state/run-argv"
	if [[ -n "${ACTIONS_RUNTIME_TOKEN:-}" ]]; then
		printf '%s|%s|%s' "$ACTIONS_RUNTIME_TOKEN" "${ACTIONS_RESULTS_URL:-}" "${ACTIONS_CACHE_SERVICE_V2:-}" > "$state/cache-runtime"
	fi
  files=''
  workspace=''
  runner_temp=''
  workdir=''
  args=("$@")
  name=''
  owner=''
  image_index=-1
  expected_image="$(cat "$state/image-name")"
  for ((i = 0; i < ${#args[@]}; i++)); do
    arg="${args[$i]}"
    if [[ "$arg" == "$expected_image" ]]; then
      image_index=$i
      break
    fi
    case "$arg" in
      --name) name="${args[$((i + 1))]}" ;;
      --label) owner="${args[$((i + 1))]}" ;;
      type=bind,source=*,target=/github/file_commands)
        files="${arg#type=bind,source=}"
        files="${files%,target=/github/file_commands}"
        ;;
      type=bind,source=*,target=/github/workspace)
        workspace="${arg#type=bind,source=}"
        workspace="${workspace%,target=/github/workspace}"
        ;;
      type=bind,source=*,target=/github/runner_temp)
        runner_temp="${arg#type=bind,source=}"
        runner_temp="${runner_temp%,target=/github/runner_temp}"
        ;;
      --workdir) workdir="${args[$((i + 1))]}" ;;
    esac
  done
  printf '%s' "$name" > "$state/container-name"
  printf '%s' "$owner" > "$state/owner"
  [[ -n "$files" && -n "$workspace" && -n "$runner_temp" && "$workdir" == /github/workspace ]] || exit 33
  [[ -d "$workspace" && -d "$runner_temp" ]] || exit 41
	[[ "$name" == "$(cat "$state/container-name")" ]] || exit 33
  [[ "$owner" == "$(cat "$state/owner")" && $image_index -gt 0 ]] || exit 39
  printf '%s' "$files" > "$state/command-files-path"
  touch "$state/container"
  printf 'container=ran\n' > "$files/output"
  printf 'DOCKER_RUNTIME_SEEN=true\n' > "$files/env"
  printf '/fake/action/bin\n' > "$files/path"
  printf 'docker_state=seen\n' > "$files/state"
  printf 'docker action summary\n' > "$files/summary"
  printf '::add-mask::fake-docker-secret\n'
  printf 'masked fake probe: fake-docker-secret\n'
  if [[ "$scenario" == replace-files ]]; then
    printf 'poison=followed\n' > "$state/poison"
    rm -f "$files/output" && ln -s "$state/poison" "$files/output"
    rm -f "$files/env" && mkfifo "$files/env"
    rm -f "$files/state" && printf 'poison=replaced\n' > "$files/state"
    rm -f "$files/summary" && mkdir "$files/summary"
    rm -f "$files/path" && ln -s "$state/missing" "$files/path"
  fi
  if [[ "$scenario" == cancel ]]; then
    touch "$state/ready"
    trap 'exit 130' INT TERM
    while :; do sleep 0.1; done
  fi
  if [[ "$scenario" == run-fail-invalid-files ]]; then
    printf '=invalid\n' > "$files/output"
    exit 34
  fi
  [[ "$scenario" != run-fail ]] || exit 34
  exit 0
fi

if [[ "$1" == ps ]]; then
  [[ "$scenario" != query-fail ]] || exit 35
  owner="$(cat "$state/owner")"
  container="$(cat "$state/container-name")"
  if (( $# == 7 )); then
    [[ "$2" == --all && "$3" == --quiet && "$4" == --filter && "$5" == "label=$owner" && "$6" == --filter && "$7" == "name=^/$container$" ]] || exit 40
  elif (( $# == 5 )); then
    [[ "$2" == --all && "$3" == --quiet && "$4" == --filter && "$5" == "label=$owner" ]] || exit 41
  else
    exit 42
  fi
  [[ ! -e "$state/container" ]] || printf 'fake-container\n'
  exit 0
fi

if [[ "$1" == stop ]]; then
  [[ $# == 4 && "$2" == --time && "$3" == 2 && "$4" == "$(cat "$state/container-name")" ]] || exit 43
  exit 0
fi

if [[ "$1" == rm ]]; then
  [[ $# == 3 && "$2" == --force && "$3" == "$(cat "$state/container-name")" ]] || exit 44
  rm -f "$state/container"
  exit 0
fi

if [[ "$1" == image && "$2" == ls ]]; then
  [[ "$scenario" != query-fail ]] || exit 36
  owner="$(cat "$state/owner")"
  image="$(cat "$state/image-name")"
  if (( $# == 7 )); then
    [[ "$3" == --all && "$4" == --quiet && "$5" == --filter && "$6" == "label=$owner" && "$7" == "$image" ]] || exit 45
  elif (( $# == 6 )); then
    [[ "$3" == --all && "$4" == --quiet && "$5" == --filter && "$6" == "label=$owner" ]] || exit 46
  else
    exit 47
  fi
  [[ ! -e "$state/image" ]] || printf 'fake-image\n'
  exit 0
fi

if [[ "$1" == image && "$2" == rm ]]; then
  [[ $# == 4 && "$3" == --force && "$4" == "$(cat "$state/image-name")" ]] || exit 48
  [[ "$scenario" == leftover ]] || rm -f "$state/image"
  exit 0
fi

exit 97
`)
	path := filepath.Join(root, "docker")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeDocker{path: path, root: root, transcript: filepath.Join(root, "transcript"), ready: filepath.Join(root, "ready")}
}

func fakeDockerAction(t *testing.T) dockerAction {
	t.Helper()
	path := fixturePath(t, "actions", "docker")
	digest, err := source.DigestTree(path)
	if err != nil {
		t.Fatal(err)
	}
	return dockerAction{
		Name: "fake Docker", Path: path, SourceRoot: path, SourceDigest: digest,
		Workspace: t.TempDir(), Env: map[string]string{"Z_LAST": "z", "A_FIRST": "a"},
	}
}

func fakeDockerRunArgv(t *testing.T, fake fakeDocker) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fake.root, "run-argv"))
	if err != nil {
		t.Fatal(err)
	}
	fields := bytes.Split(data, []byte{0})
	fields = fields[:len(fields)-1]
	argv := make([]string, len(fields))
	for i := range fields {
		argv[i] = string(fields[i])
	}
	return argv
}

func callIndex(calls []fakeDockerCall, command ...string) int {
	for i, call := range calls {
		if len(call.args) < len(command) {
			continue
		}
		if slices.Equal(call.args[:len(command)], command) {
			return i
		}
	}
	return -1
}

func argumentAfter(t *testing.T, args []string, name string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	t.Fatalf("argument %q absent from %#v", name, args)
	return ""
}
