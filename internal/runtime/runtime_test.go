package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

type testSecretResolver map[string]string

func (s testSecretResolver) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := s[name]
	if !ok {
		return "", fmt.Errorf("denied")
	}
	return value, nil
}

type testRedactor struct{ values []string }

func (r *testRedactor) AddRedaction(_ context.Context, value string) error {
	r.values = append(r.values, value)
	return nil
}

type deferredInputRunner struct {
	jobID string
	path  string
	data  []byte
}

func (r deferredInputRunner) Run(_ context.Context, _ string, _ string, args []string, _ []byte) ([]byte, error) {
	if len(args) >= 5 && args[0] == "artifact" && args[1] == "search" && args[2] == r.path {
		return []byte(r.jobID + "\n"), nil
	}
	if len(args) >= 4 && args[0] == "artifact" && args[1] == "download" && args[2] == r.path {
		path := filepath.Join(args[3], filepath.FromSlash(r.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, r.data, 0o600)
	}
	return nil, fmt.Errorf("unexpected agent command: %v", args)
}

type testWorkflowTokenProvider struct {
	token       string
	repository  string
	workflow    string
	permissions map[string]string
	calls       int
}

func (p *testWorkflowTokenProvider) WorkflowToken(_ context.Context, repository, workflow string, permissions map[string]string) (string, error) {
	p.calls++
	p.repository = repository
	p.workflow = workflow
	p.permissions = maps.Clone(permissions)
	return p.token, nil
}

type failingTokenRedactor struct{ token string }

func (r failingTokenRedactor) AddRedaction(context.Context, string) error {
	return fmt.Errorf("failed to redact %s", r.token)
}

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

if [[ "$1" == run ]]; then
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
  for ((i = 0; i < ${#args[@]}; i++)); do
    arg="${args[$i]}"
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
  [[ -n "$files" && -n "$workspace" && -n "$runner_temp" && "$workdir" == /github/workspace ]] || exit 33
  [[ -d "$workspace" && -d "$runner_temp" ]] || exit 41
  [[ "$name" == "$(cat "$state/image-name" | sed 's/-image-/-container-/')" ]] || exit 33
  [[ "$owner" == "$(cat "$state/owner")" && "${args[$((${#args[@]} - 1))]}" == "$(cat "$state/image-name")" ]] || exit 39
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

func (fake fakeDocker) calls(t *testing.T) []fakeDockerCall {
	t.Helper()
	data, err := os.ReadFile(fake.transcript)
	if err != nil {
		t.Fatal(err)
	}
	var calls []fakeDockerCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Split(line, "|")
		metadata := fields[0]
		config, ok := strings.CutPrefix(strings.SplitN(metadata, ";", 2)[0], "config=")
		if !ok {
			t.Fatalf("malformed fake Docker transcript line %q", line)
		}
		calls = append(calls, fakeDockerCall{config: config, metadata: metadata, args: fields[1:]})
	}
	return calls
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
			step := plan.Step{ID: "docker", Kind: "uses", Uses: "./.github/actions/docker"}
			if test.stepPATH != "" {
				step.Env = map[string]string{"PATH": test.stepPATH}
			}
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{step})
			job.RequiredCapabilities = []string{"docker", "network"}
			if test.jobPATH != "" {
				job.Env = map[string]string{"PATH": test.jobPATH}
			}
			if _, err := (Runner{Docker: fake.path}).RunJob(t.Context(), job, workspace); err != nil {
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{
		{ID: "path", Kind: "run", Command: `printf '%s\n' 'PATH=/dynamic/bin' >> "$GITHUB_ENV"`},
		{ID: "docker", Kind: "uses", Uses: "./.github/actions/docker"},
	})
	job.RequiredCapabilities = []string{"docker", "network"}
	if _, err := (Runner{Docker: fake.path}).RunJob(t.Context(), job, workspace); err != nil {
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
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{
		ID: "docker", Kind: "uses", Uses: "./.github/actions/docker",
		With: map[string]string{"value": "caller-input"},
		Env:  map[string]string{"MODE": "caller"},
	}})
	job.RequiredCapabilities = []string{"docker", "network"}
	if _, err := (Runner{Docker: fake.path}).RunJob(t.Context(), job, workspace); err != nil {
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

func TestRunJobUnresolvedDockerActionUsesFakeBackend(t *testing.T) {
	requireLinuxAMD64(t)
	fake := newFakeDocker(t, "success")
	workspace := fixturePath(t)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "docker", Kind: "uses", Uses: "./actions/docker"}})
	job.RequiredCapabilities = []string{"docker", "network"}
	result, err := (Runner{Docker: fake.path}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunJobEvaluatesIndexedWorkflowInputs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/inputs.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: inputs\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "check", Kind: "run", Command: `test "$VALUE" = dispatched`,
		Env: map[string]string{"VALUE": "${{ inputs[env.KEY] }}"},
	}})
	job.Inputs = map[string]any{"label": "dispatched"}
	job.Env = map[string]string{"KEY": "label"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestRunJobDoesNotTolerateDockerActionCleanupFailure(t *testing.T) {
	requireLinuxAMD64(t)
	fake := newFakeDocker(t, "leftover")
	workspace := fixturePath(t)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{
		ID: "docker", Kind: "uses", Uses: "./actions/docker", ContinueOnError: true,
	}})
	job.RequiredCapabilities = []string{"docker", "network"}
	job.ContinueOnError = true

	result, err := (Runner{Docker: fake.path}).RunJob(t.Context(), job, workspace)
	if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "owned Docker resources remain after cleanup") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestSequentialRunControlsAndEnvironment(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	if err := os.Mkdir(filepath.Join(workspace, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	redactor := &testRedactor{}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "first", Kind: "run", WorkingDirectory: "subdir", Env: map[string]string{"LEVEL": "step"}, Command: `test "$LEVEL" = step
test "$TOKEN" = mask-me
test "$GITHUB_SHA" = 0123456789abcdef
test "${{ github.repository }}" = buildkite/example
echo "$GITHUB_WORKSPACE/bin" >> "$GITHUB_PATH"
echo "LEVEL=file" >> "$GITHUB_ENV"
echo "secret=${{ secrets.CANARY }}"`},
		{ID: "soft", Kind: "run", Command: "exit 7", ContinueOnError: true},
		{ID: "after-soft", Kind: "run", Condition: "steps.soft.outcome == 'failure' && steps.soft.conclusion == 'success'", Env: map[string]string{"SOFT_OUTCOME": "${{ steps.soft.outcome }}"}, Command: `test "$LEVEL" = file
test "$SOFT_OUTCOME:${{ steps.soft.conclusion }}" = failure:success
case "$PATH" in "$GITHUB_WORKSPACE/bin"*) ;; *) exit 9 ;; esac
echo after-soft`},
	})
	job.Env = map[string]string{"LEVEL": "job", "TOKEN": "${{ secrets.CANARY }}"}
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"CANARY"}
	job.Event.Repository = "buildkite/example"
	job.Event.Ref = "refs/heads/main"
	job.Event.SHA = "0123456789abcdef"
	job.Event.Actor = "octocat"
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: testSecretResolver{"CANARY": "mask-me"}, Redactor: redactor}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v, logs = %q", err, logs.String())
	}
	if result.Conclusion != "success" || result.Env["LEVEL"] != "file" || result.Env["TOKEN"] != "***" || len(redactor.values) != 1 {
		t.Fatalf("RunJob() result = %#v, redactions = %#v", result, redactor.values)
	}
	if strings.Contains(logs.String(), "mask-me") || !strings.Contains(logs.String(), "secret=***") || !strings.Contains(logs.String(), "after-soft") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestCompiledRunDefaultsEvaluateAtRuntime(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/defaults.yml"
	source := `on: push
jobs:
  matrix-defaults:
    strategy:
      matrix:
        shell: [bash]
        dir: [matrix]
    runs-on: ubuntu-latest
    defaults:
      run:
        shell: ${{ matrix.shell }}
        working-directory: ${{ matrix.dir }}
    steps:
      - run: test "$(basename "$PWD")" = matrix
  env-defaults:
    runs-on: ubuntu-latest
    env:
      DEFAULT_SHELL: bash
      DEFAULT_DIR: env
    defaults:
      run:
        shell: ${{ env.DEFAULT_SHELL }}
        working-directory: ${{ env.DEFAULT_DIR }}
    steps:
      - run: test "$(basename "$PWD")" = env
  vars-defaults:
    runs-on: ubuntu-latest
    defaults:
      run:
        shell: ${{ vars.DEFAULT_SHELL }}
        working-directory: ${{ vars.DEFAULT_DIR }}
    steps:
      - run: test "$(basename "$PWD")" = vars
  explicit-precedence:
    runs-on: ubuntu-latest
    defaults:
      run:
        shell: unsupported-default
        working-directory: missing-default
    steps:
      - shell: sh
        working-directory: ${{ vars.OVERRIDE_DIR }}
        run: test "$(basename "$PWD")" = override
`
	writeFixtureFile(t, workspace, workflowPath, source)
	for _, directory := range []string{"matrix", "env", "vars", "override"} {
		if err := os.Mkdir(filepath.Join(workspace, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(t.Context(),
		filepath.Join(workspace, workflowPath),
		[]byte(source),
		event,
		"0.0.0-test",
		"sha256:"+strings.Repeat("2", 64),
		compiler.Options{
			EventTrust: compiler.EventUntrusted,
			Vars: compiler.VariableSources{Bridge: map[string]string{
				"DEFAULT_SHELL": "bash",
				"DEFAULT_DIR":   "vars",
				"OVERRIDE_DIR":  "override",
			}},
			Runners: compiler.RunnerPolicy{
				Labels:          map[string]string{"ubuntu-latest": "gha-untrusted"},
				UntrustedQueues: []string{"gha-untrusted"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 4 {
		t.Fatalf("plans = %d, want four defaults cases", len(plans))
	}
	for _, job := range plans {
		result, err := (Runner{}).RunJob(t.Context(), job, workspace)
		if err != nil || result.Conclusion != "success" {
			t.Fatalf("RunJob(%s) result = %#v, error = %v", job.Workflow.LogicalJobID, result, err)
		}
	}
}

func TestCompiledBracketSecretResolvesAndMasks(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/secrets.yml"
	source := "on: push\njobs:\n  secrets:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo \"secret=${{ secrets['TOKEN'] }}\"\n"
	writeFixtureFile(t, workspace, workflowPath, source)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(filepath.Join(workspace, workflowPath), []byte(source), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !slices.Equal(plans[0].RequiredSecrets, []string{"TOKEN"}) || !plans[0].HasCapability("secrets") {
		t.Fatalf("compiled secret boundary = %#v", plans)
	}
	var logs bytes.Buffer
	redactor := &testRedactor{}
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: testSecretResolver{"TOKEN": "secret-value"}, Redactor: redactor}).RunJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(logs.String(), "secret-value") || !strings.Contains(logs.String(), "secret=***") || !slices.Equal(redactor.values, []string{"secret-value"}) {
		t.Fatalf("logs = %q, redactions = %#v", logs.String(), redactor.values)
	}
}

func TestAgentSecretsUsesOnlyJobBoundConfiguration(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	writeFixtureFile(t, filepath.Dir(agent), filepath.Base(agent), `#!/bin/sh
test "$#" -eq 3 || exit 10
test "$1" = secret && test "$2" = get && test "$3" = HOMEBREW_TAP_GITHUB_TOKEN || exit 11
test "$BUILDKITE_JOB_ID" = job-id || exit 12
test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-token || exit 13
test "$BUILDKITE_AGENT_ENDPOINT" = https://agent.example/v3 || exit 14
test "$BUILDKITE_AGENT_JOB_API_SOCKET" = /tmp/job-api.sock || exit 15
test "$BUILDKITE_AGENT_JOB_API_TOKEN" = job-api-token || exit 16
test "$BUILDKITE_NO_HTTP2" = true || exit 17
test "$HTTP_PROXY" = http://upper-http.example:8080 || exit 18
test "$HTTPS_PROXY" = http://upper-https.example:8080 || exit 19
test "$ALL_PROXY" = socks5://upper-all.example:1080 || exit 20
test "$NO_PROXY" = upper-no-proxy.example || exit 21
test "$http_proxy" = http://lower-http.example:8080 || exit 22
test "$https_proxy" = http://lower-https.example:8080 || exit 23
test "$all_proxy" = socks5://lower-all.example:1080 || exit 24
test "$no_proxy" = lower-no-proxy.example || exit 25
test "$SSL_CERT_FILE" = /etc/buildkite/ca.pem || exit 26
test "$SSL_CERT_DIR" = /etc/buildkite/certs || exit 27
test -z "${AMBIENT_SECRET+x}" || exit 28
printf '%s\n' tap-secret
`)
	if err := os.Chmod(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_AGENT_JOB_API_SOCKET", "/tmp/job-api.sock")
	t.Setenv("BUILDKITE_AGENT_JOB_API_TOKEN", "job-api-token")
	t.Setenv("AMBIENT_SECRET", "must-not-be-inherited")
	transportEnvironment := map[string]string{
		"HTTP_PROXY": "http://upper-http.example:8080", "HTTPS_PROXY": "http://upper-https.example:8080",
		"ALL_PROXY": "socks5://upper-all.example:1080", "NO_PROXY": "upper-no-proxy.example",
		"http_proxy": "http://lower-http.example:8080", "https_proxy": "http://lower-https.example:8080",
		"all_proxy": "socks5://lower-all.example:1080", "no_proxy": "lower-no-proxy.example",
		"SSL_CERT_FILE": "/etc/buildkite/ca.pem", "SSL_CERT_DIR": "/etc/buildkite/certs",
	}
	for name, value := range transportEnvironment {
		t.Setenv(name, value)
	}
	resolved, err := resolveAgentSecretsBeforeWorkflow(AgentSecrets{
		Executable: agent,
		Endpoint:   "https://agent.example/v3",
		JobID:      "job-id",
		JobToken:   "job-token",
		NoHTTP2:    "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range transportEnvironment {
		t.Setenv(name, "http://workflow-controlled.example")
	}
	value, err := resolved.ResolveSecret(t.Context(), "HOMEBREW_TAP_GITHUB_TOKEN")
	if err != nil || value != "tap-secret" {
		t.Fatalf("ResolveSecret() = %q, %v", value, err)
	}
}

func TestAgentSecretsDoesNotReturnCommandOutputOnFailure(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	writeFixtureFile(t, filepath.Dir(agent), filepath.Base(agent), "#!/bin/sh\nprintf 'stdout-secret'\nprintf 'stderr-secret' >&2\nexit 1\n")
	if err := os.Chmod(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (AgentSecrets{Executable: agent}).ResolveSecret(t.Context(), "DENIED")
	if err == nil || strings.Contains(err.Error(), "stdout-secret") || strings.Contains(err.Error(), "stderr-secret") || !strings.Contains(err.Error(), "secret request failed") {
		t.Fatalf("ResolveSecret() error = %v", err)
	}
}

func TestResolveAgentRedactorBeforeWorkflowPinsPointerWithoutMutatingCaller(t *testing.T) {
	realDir := canonicalTempDir(t)
	realAgent := filepath.Join(realDir, "buildkite-agent")
	writeFixtureFile(t, realDir, "buildkite-agent", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(realAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	lookupDir := t.TempDir()
	lookupAgent := filepath.Join(lookupDir, "buildkite-agent")
	if err := os.Symlink(realAgent, lookupAgent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", lookupDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	configured := &AgentRedactor{}
	resolved, err := resolveAgentRedactorBeforeWorkflow(configured)
	if err != nil {
		t.Fatal(err)
	}
	pinned, ok := resolved.(*AgentRedactor)
	if !ok || pinned == configured || pinned.Executable != realAgent {
		t.Fatalf("resolved redactor = %#v, want independent pointer pinned to %q", resolved, realAgent)
	}
	if configured.Executable != "" {
		t.Fatalf("configured redactor mutated to %q", configured.Executable)
	}
}

func TestAgentRedactorSatisfiesCLIValidationWithoutExposingAgentCredential(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	writeFixtureFile(t, filepath.Dir(agent), filepath.Base(agent), `#!/bin/sh
test "$#" -eq 3 || { echo 'Missing agent-access-token. See: buildkite-agent redactor add --help' >&2; exit 10; }
test "$1" = "redactor" || exit 11
test "$2" = "add" || exit 12
test "$3" = "--agent-access-token=unused" || { echo 'Missing agent-access-token. See: buildkite-agent redactor add --help' >&2; exit 13; }
test "${BUILDKITE_AGENT_JOB_API_SOCKET-}" = "/tmp/job-api.sock" || exit 11
test "${BUILDKITE_AGENT_JOB_API_TOKEN-}" = "job-api-token" || exit 12
test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}" || exit 13
test -z "${BUILDKITE_AGENT_ENDPOINT+x}" || exit 14
test -z "${AMBIENT_SECRET+x}" || exit 15
`)
	if err := os.Chmod(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_AGENT_JOB_API_SOCKET", "/tmp/job-api.sock")
	t.Setenv("BUILDKITE_AGENT_JOB_API_TOKEN", "job-api-token")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "agent-access-token")
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", "https://agent.example/v3")
	t.Setenv("AMBIENT_SECRET", "must-not-be-inherited")

	if err := (AgentRedactor{Executable: agent}).AddRedaction(t.Context(), "redact-me"); err != nil {
		t.Fatal(err)
	}
}

func TestFailureConditionsAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fail", Kind: "run", Command: "exit 1"},
		{ID: "default", Kind: "run", Command: "echo must-not-run"},
		{ID: "recover", Kind: "run", Condition: "failure()", Command: "echo recovered"},
	})
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || strings.Contains(logs.String(), "must-not-run") || !strings.Contains(logs.String(), "recovered") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}

	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "timeout", Kind: "run", Command: "sleep 30", TimeoutMinutes: 0.0005}})
	started := time.Now()
	result, err = (Runner{}).RunJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "failure" || time.Since(started) > 3*time.Second {
		t.Fatalf("timed RunJob() result = %#v, error = %v, elapsed = %s", result, err, time.Since(started))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cancel", Kind: "run", Condition: "always()", Command: "sleep 30"}})
	result, err = (Runner{}).RunJob(ctx, job, workspace)
	if !errors.Is(err, context.Canceled) || result.Conclusion != "cancelled" {
		t.Fatalf("cancelled RunJob() result = %#v, error = %v", result, err)
	}
}

func TestExplicitCancelCommitsEffectsWithoutFailingJob(t *testing.T) {
	t.Parallel()

	for range 20 {
		testExplicitCancelCommitsEffectsWithoutFailingJob(t)
	}
}

func testExplicitCancelCommitsEffectsWithoutFailingJob(t *testing.T) {
	t.Helper()
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	ready := filepath.Join(workspace, "background.ready")
	terminated := filepath.Join(workspace, "background.terminated")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `
echo "CANCEL_EFFECT=visible" >> "$GITHUB_ENV"
trap 'touch "$TERMINATED"; exit 0' INT
touch "$READY"
while :; do sleep 1; done`},
		{ID: "await-start", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
		{ID: "cancel-again", Kind: "cancel", Targets: []string{"background"}},
		{ID: "after-cancel", Kind: "run", Condition: "steps.background.outcome == 'cancelled' && steps.cancel.conclusion == 'success' && steps.cancel-again.conclusion == 'success'", Command: `test "$CANCEL_EFFECT" = visible; test -f "$TERMINATED"; echo after-cancel`},
	})
	job.Env = map[string]string{"READY": ready, "TERMINATED": terminated}
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Env["CANCEL_EFFECT"] != "visible" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(logs.String(), "after-cancel") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestCancelQueuedBackgroundNeverStartsIt(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	release := filepath.Join(workspace, "release")
	queuedMarker := filepath.Join(workspace, "queued-started")
	steps := make([]plan.Step, 0, maxActiveBackgroundSteps+4)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, plan.Step{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		plan.Step{ID: "queued", Kind: "run", Background: true, Command: `touch "$QUEUED_MARKER"`},
		plan.Step{ID: "cancel-queued", Kind: "cancel", Targets: []string{"queued"}},
		plan.Step{ID: "release", Kind: "run", Command: `test ! -e "$QUEUED_MARKER"; touch "$RELEASE"`},
		plan.Step{ID: "wait", Kind: "wait-all"},
	)
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{"RELEASE": release, "QUEUED_MARKER": queuedMarker}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, statErr := os.Stat(queuedMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("queued canceled step ran: %v", statErr)
	}
}

func TestQueuedBackgroundTimeoutStartsAtDispatch(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: background timeout dispatch\n")
	release := filepath.Join(workspace, "release")
	queuedMarker := filepath.Join(workspace, "queued-started")
	steps := make([]plan.Step, 0, maxActiveBackgroundSteps+3)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, plan.Step{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		plan.Step{ID: "queued", Kind: "run", Background: true, TimeoutMinutes: 0.001, Command: `touch "$QUEUED_MARKER"`},
		plan.Step{ID: "release", Kind: "run", Command: `sleep 0.2; touch "$RELEASE"`},
		plan.Step{ID: "wait", Kind: "wait-all"},
	)
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{"RELEASE": release, "QUEUED_MARKER": queuedMarker}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(queuedMarker); err != nil {
		t.Fatalf("queued timed step did not run: %v", err)
	}
}

func TestCancelQueuedBackgroundNeverRegistersPostAction(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/queued/action.yml", "name: Queued lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/queued/main.js", "console.log('queued-main-must-not-run')\n")
	writeFixtureFile(t, workspace, ".github/actions/queued/post.js", "console.log('queued-post-must-not-run')\n")
	release := filepath.Join(workspace, "release")
	steps := make([]plan.Step, 0, maxActiveBackgroundSteps+4)
	for i := range maxActiveBackgroundSteps {
		steps = append(steps, plan.Step{ID: fmt.Sprintf("blocker-%d", i), Kind: "run", Background: true, Command: `while [ ! -f "$RELEASE" ]; do sleep 0.01; done`})
	}
	steps = append(steps,
		plan.Step{ID: "queued", Kind: "uses", Uses: "./.github/actions/queued", Background: true},
		plan.Step{ID: "cancel-queued", Kind: "cancel", Targets: []string{"queued"}},
		plan.Step{ID: "release", Kind: "run", Command: `touch "$RELEASE"`},
		plan.Step{ID: "wait", Kind: "wait-all"},
	)
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{"RELEASE": release}
	var logs bytes.Buffer

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(logs.String(), "queued-main-must-not-run") || strings.Contains(logs.String(), "queued-post-must-not-run") {
		t.Fatalf("queued cancelled action ran lifecycle phase: %q", logs.String())
	}
}

func TestFailureBeforeCancelStillFailsJob(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	failedPID := filepath.Join(workspace, "failed.pid")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `echo $$ > "$FAILED_PID"; exit 7`},
		{ID: "await-failure", Kind: "run", Command: `while [ ! -f "$FAILED_PID" ]; do sleep 0.01; done; while kill -0 "$(cat "$FAILED_PID")" 2>/dev/null; do sleep 0.01; done; sleep 0.05`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
		{ID: "default-after-cancel", Kind: "run", Command: "echo must-not-run"},
		{ID: "recover", Kind: "run", Condition: "failure()", Command: "echo recovered"},
	})
	job.Env = map[string]string{"FAILED_PID": failedPID}

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(logs.String(), "recovered") || strings.Contains(logs.String(), "must-not-run") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestBackgroundEffectsCommitAtCoveringBarriers(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	oneDone := filepath.Join(workspace, "one.done")
	twoDone := filepath.Join(workspace, "two.done")
	pathEntry := filepath.Join(workspace, "from-background")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "one", Kind: "run", Background: true, Command: `
echo "ONE=committed-one" >> "$GITHUB_ENV"
echo "value=output-one" >> "$GITHUB_OUTPUT"
echo "$PATH_ENTRY" >> "$GITHUB_PATH"
echo one-summary >> "$GITHUB_STEP_SUMMARY"
touch "$ONE_DONE"`},
		{ID: "two", Kind: "run", Background: true, Command: `
echo "TWO=committed-two" >> "$GITHUB_ENV"
echo "value=output-two" >> "$GITHUB_OUTPUT"
touch "$TWO_DONE"`},
		{ID: "before-wait", Kind: "run", Command: `
while [ ! -f "$ONE_DONE" ] || [ ! -f "$TWO_DONE" ]; do sleep 0.01; done
test -z "$ONE"
test -z "$TWO"
case "$PATH" in "$PATH_ENTRY"*) exit 1 ;; esac`},
		{ID: "wait-one", Kind: "wait", Targets: []string{"one"}},
		{ID: "after-targeted-wait", Kind: "run", Command: `
test "$ONE" = committed-one
test -z "$TWO"
test "${{ steps.one.outputs.value }}" = output-one
case "$PATH" in "$PATH_ENTRY"*) ;; *) exit 1 ;; esac`},
		{ID: "wait-all", Kind: "wait-all"},
		{ID: "after-wait-all", Kind: "run", Command: `
test "$ONE" = committed-one
test "$TWO" = committed-two
test "${{ steps.two.outputs.value }}" = output-two`},
	})
	job.Env = map[string]string{"ONE_DONE": oneDone, "TWO_DONE": twoDone, "PATH_ENTRY": pathEntry}
	job.Outputs = map[string]string{"one": "${{ steps.one.outputs.value }}", "two": "${{ steps.two.outputs.value }}"}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" || result.Outputs["one"] != "output-one" || result.Outputs["two"] != "output-two" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if result.Summary != "one-summary\n" {
		t.Fatalf("RunJob() summary = %q", result.Summary)
	}
}

func TestBackgroundSummariesAreBoundedInCommitOrder(t *testing.T) {
	supervisor := newBackgroundSupervisor(2)
	releaseFirst := make(chan struct{})
	completionOrder := make(chan string, 2)
	first := plan.Step{ID: "first"}
	second := plan.Step{ID: "second"}
	summaryBytes := maxJobSummaryBytes * 3 / 4

	supervisor.start(t.Context(), first.ID,
		func(ctx context.Context) stepExecution {
			<-releaseFirst
			completionOrder <- first.ID
			result := newResult()
			result.Summary = strings.Repeat("a", summaryBytes)
			return classifyStepExecution(ctx, ctx, first, result, nil)
		},
		func(ctx context.Context) stepExecution {
			return cancelledStepExecution(t.Context(), ctx, first)
		},
	)
	supervisor.start(t.Context(), second.ID,
		func(ctx context.Context) stepExecution {
			completionOrder <- second.ID
			close(releaseFirst)
			result := newResult()
			result.Summary = strings.Repeat("b", summaryBytes)
			return classifyStepExecution(ctx, ctx, second, result, nil)
		},
		func(ctx context.Context) stepExecution {
			return cancelledStepExecution(t.Context(), ctx, second)
		},
	)

	jobResult := JobResult{Env: map[string]string{}, State: map[string]string{}}
	eval := expression.Context{Steps: map[string]expression.StepStatus{}}
	for _, execution := range supervisor.waitAll() {
		if err := commitStepExecution(execution, &jobResult, &eval); err != nil {
			t.Fatal(err)
		}
	}
	jobResult.Summary = finalizeJobSummary(jobResult.Summary, jobResult.summaryTruncated)

	if got := []string{<-completionOrder, <-completionOrder}; !slices.Equal(got, []string{second.ID, first.ID}) {
		t.Fatalf("completion order = %v, want second then first", got)
	}
	if len(jobResult.Summary) > maxJobSummaryBytes || !strings.HasSuffix(jobResult.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("summary bytes = %d, suffix present = %v", len(jobResult.Summary), strings.HasSuffix(jobResult.Summary, jobSummaryTruncationNotice))
	}
	prefix := strings.TrimSuffix(jobResult.Summary, jobSummaryTruncationNotice)
	wantPrefix := strings.Repeat("a", summaryBytes) + strings.Repeat("b", len(prefix)-summaryBytes)
	if prefix != wantPrefix {
		t.Fatalf("summary did not preserve commit order")
	}
}

func TestCloneExpressionContextDeepCopiesStepOutputs(t *testing.T) {
	source := expression.Context{Steps: map[string]expression.StepStatus{
		"build": {Outcome: "success", Conclusion: "success", Outputs: map[string]string{"result": "source"}},
	}}

	cloned := cloneExpressionContext(source)
	status := cloned.Steps["build"]
	status.Outcome = "failure"
	status.Outputs["result"] = "clone"
	cloned.Steps["build"] = status
	cloned.Steps["added"] = expression.StepStatus{}

	if got := source.Steps["build"]; got.Outcome != "success" || got.Outputs["result"] != "source" {
		t.Fatalf("source step changed through clone: %#v", got)
	}
	if _, ok := source.Steps["added"]; ok {
		t.Fatal("source steps map changed through clone")
	}
}

func TestCloneExpressionContextDeepCopiesNeedOutputs(t *testing.T) {
	source := expression.Context{Needs: map[string]expression.NeedStatus{
		"build": {Outputs: map[string]string{"result": "source"}, Result: "success"},
	}}

	cloned := cloneExpressionContext(source)
	status := cloned.Needs["build"]
	status.Result = "failure"
	status.Outputs["result"] = "clone"
	cloned.Needs["build"] = status
	cloned.Needs["added"] = expression.NeedStatus{}

	if got := source.Needs["build"]; got.Result != "success" || got.Outputs["result"] != "source" {
		t.Fatalf("source need changed through clone: %#v", got)
	}
	if _, ok := source.Needs["added"]; ok {
		t.Fatal("source needs map changed through clone")
	}
}

func TestConcurrentPathEffectsComposeWithLiveBarrierState(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	oneDone := filepath.Join(workspace, "one.done")
	twoDone := filepath.Join(workspace, "two.done")
	onePath := filepath.Join(workspace, "background-one")
	twoPath := filepath.Join(workspace, "background-two")
	foregroundPath := filepath.Join(workspace, "foreground")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "one", Kind: "run", Background: true, Command: `echo "$ONE_PATH" >> "$GITHUB_PATH"; touch "$ONE_DONE"`},
		{ID: "two", Kind: "run", Background: true, Command: `echo "$TWO_PATH" >> "$GITHUB_PATH"; touch "$TWO_DONE"`},
		{ID: "foreground", Kind: "run", Command: `
while [ ! -f "$ONE_DONE" ] || [ ! -f "$TWO_DONE" ]; do sleep 0.01; done
echo "$FOREGROUND_PATH" >> "$GITHUB_PATH"`},
		{ID: "wait-all", Kind: "wait-all"},
		{ID: "verify", Kind: "run", Command: `
want="$TWO_PATH:$ONE_PATH:$FOREGROUND_PATH:"
case "$PATH" in "$want"*) ;; *) exit 1 ;; esac
test "$(printf '%s' "$PATH" | tr ':' '\n' | grep -Fxc "$ONE_PATH")" -eq 1
test "$(printf '%s' "$PATH" | tr ':' '\n' | grep -Fxc "$TWO_PATH")" -eq 1
test "$(printf '%s' "$PATH" | tr ':' '\n' | grep -Fxc "$FOREGROUND_PATH")" -eq 1`},
	})
	job.Env = map[string]string{
		"ONE_DONE": oneDone, "TWO_DONE": twoDone,
		"ONE_PATH": onePath, "TWO_PATH": twoPath, "FOREGROUND_PATH": foregroundPath,
	}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
}

func TestBackgroundCompositePathEffectsComposeAtBarrier(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/path/action.yml", `name: Path writer
runs:
  using: composite
  steps:
    - shell: bash
      run: echo "$COMPOSITE_PATH" >> "$GITHUB_PATH"
`)
	compositePath := filepath.Join(workspace, "composite")
	foregroundPath := filepath.Join(workspace, "foreground")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/path", Background: true},
		{ID: "foreground", Kind: "run", Command: `echo "$FOREGROUND_PATH" >> "$GITHUB_PATH"`},
		{ID: "wait", Kind: "wait", Targets: []string{"composite"}},
		{ID: "verify", Kind: "run", Command: `case "$PATH" in "$COMPOSITE_PATH:$FOREGROUND_PATH:"*) ;; *) exit 1 ;; esac`},
	})
	job.Env = map[string]string{"COMPOSITE_PATH": compositePath, "FOREGROUND_PATH": foregroundPath}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
}

func TestNode20DeclarationUsesNode24ForJavaScriptLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/path/action.yml", `name: JavaScript path writer
runs:
  using: node20
  pre: pre.js
  main: main.js
  post: post.js
`)
	writeFixtureFile(t, workspace, ".github/actions/path/pre.js", "")
	writeFixtureFile(t, workspace, ".github/actions/path/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/path/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v24.0.0
  exit 0
fi
case "${1##*/}" in
  pre.js) printf '%s\n' "$PATH_ENTRY" >> "$GITHUB_PATH" ;;
  main.js) case ":$PATH:" in *":$PATH_ENTRY:"*) ;; *) exit 9 ;; esac ;;
  post.js) printf 'NODE24_POST=true\n' >> "$GITHUB_ENV" ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	pathEntry := filepath.Join(workspace, "from-pre")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./.github/actions/path"}})
	job.Env = map[string]string{"PATH_ENTRY": pathEntry}

	result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if result.Env["NODE24_POST"] != "true" {
		t.Fatalf("RunJob() environment = %#v, want Node 24 post lifecycle effect", result.Env)
	}
	if result.WarningAnnotations != "" {
		t.Fatalf("RunJob() warnings = %q, want no Node 20 deprecation warning", result.WarningAnnotations)
	}
}

func TestNode16DeclarationUsesExactLifecycleAndWarns(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/action.yml", `name: Node 16 lifecycle
runs:
  using: node16
  pre: pre.js
  main: main.js
  post: post.js
`)
	for _, entry := range []string{"pre.js", "main.js", "post.js"} {
		writeFixtureFile(t, workspace, ".github/actions/node16/"+entry, "")
	}
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v16.20.2
  exit 0
fi
printf 'NODE16_%s=true\n' "$(basename "$1" .js | tr '[:lower:]' '[:upper:]')" >> "$GITHUB_ENV"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./.github/actions/node16"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	for _, phase := range []string{"PRE", "MAIN", "POST"} {
		if result.Env["NODE16_"+phase] != "true" {
			t.Fatalf("RunJob() environment = %#v, want Node 16 %s lifecycle effect", result.Env, phase)
		}
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/node16")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings = %q, want one Node 16 lifecycle warning %q", logs.String(), result.WarningAnnotations, warning)
	}
}

func TestNode16WarningAggregatesInvokedActions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	for _, name := range []string{"alpha", "beta", "skipped"} {
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/action.yml", "name: "+name+"\nruns:\n  using: node16\n  main: main.js\n")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/main.js", "")
	}
	writeFixtureFile(t, workspace, ".github/actions/modern/action.yml", "name: modern\nruns:\n  using: node20\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/modern/main.js", "")
	node16 := filepath.Join(workspace, "node16")
	node24 := filepath.Join(workspace, "node24")
	writeNodeExecutable(t, node16, 16)
	writeNodeExecutable(t, node24, 24)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "beta", Kind: "uses", Uses: "./.github/actions/beta"},
		{ID: "alpha", Kind: "uses", Uses: "./.github/actions/alpha"},
		{ID: "alpha-again", Kind: "uses", Uses: "./.github/actions/alpha"},
		{ID: "skipped", Kind: "uses", Uses: "./.github/actions/skipped", Condition: "false"},
		{ID: "modern", Kind: "uses", Uses: "./.github/actions/modern"},
	})
	var logs bytes.Buffer
	result, err := (Runner{Node16: node16, Node24: node24, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/alpha, ./.github/actions/beta")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings = %q, want one aggregate warning %q", logs.String(), result.WarningAnnotations, warning)
	}
	for _, absent := range []string{"./.github/actions/skipped", "./.github/actions/modern"} {
		if strings.Contains(result.WarningAnnotations, absent) {
			t.Fatalf("RunJob() warnings = %q, unexpectedly named %q", result.WarningAnnotations, absent)
		}
	}
}

func TestNode16WarningSurvivesSuppressionAndMasksReferences(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/sensitive/action.yml", "name: sensitive\nruns:\n  using: node16\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/sensitive/main.js", "")
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v16.20.2; exit 0; fi
printf '%s\n' '::add-mask::sensitive'
head -c 1048577 /dev/zero | tr '\000' x
printf '\n'
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "node16", Kind: "uses", Uses: "./.github/actions/sensitive"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "line exceeds 1048576-byte limit") || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v, want oversized-line failure", result, err)
	}
	warningLog := logs.String()
	if warningIndex := strings.LastIndex(warningLog, "warning: Node.js 16 actions are deprecated"); warningIndex >= 0 {
		warningLog = warningLog[warningIndex:]
	}
	for _, output := range []string{warningLog, result.WarningAnnotations} {
		if !strings.Contains(output, "Node.js 16 actions are deprecated") || !strings.Contains(output, "./.github/actions/***") || strings.Contains(output, "sensitive") {
			t.Fatalf("RunJob() warning = %q, want masked Node 16 warning after stream suppression", output)
		}
	}
}

func TestNode16WarningHasPriorityOverUntrustedAnnotationLimit(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/action.yml", "name: node16\nruns:\n  using: node16\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/main.js", "")
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v16.20.2; exit 0; fi
i=0
while [ "$i" -lt 17 ]; do
  printf '%s' '::warning::'
  head -c 65536 /dev/zero | tr '\000' x
  printf '\n'
  i=$((i + 1))
done
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "node16", Kind: "uses", Uses: "./.github/actions/node16"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: io.Discard, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/node16")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings contain Node 16 message %d times, want one priority warning", logs.String(), strings.Count(result.WarningAnnotations, warning))
	}
	if len(result.WarningAnnotations) > maxJobAnnotationBytes || !utf8.ValidString(result.WarningAnnotations) || !strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice) {
		t.Fatalf("RunJob() warning annotation bytes = %d, valid UTF-8 = %v, suffix present = %v", len(result.WarningAnnotations), utf8.ValidString(result.WarningAnnotations), strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice))
	}
}

func TestBackgroundFailureSurfacesAtWait(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "failed.done")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "failure", Kind: "run", Background: true, Command: `echo "FAILED_EFFECT=visible" >> "$GITHUB_ENV"; touch "$FAILURE_DONE"; exit 9`},
		{ID: "before-wait", Kind: "run", Command: `while [ ! -f "$FAILURE_DONE" ]; do sleep 0.01; done; test -z "$FAILED_EFFECT"; echo before-barrier`},
		{ID: "wait", Kind: "wait", Targets: []string{"failure"}},
		{ID: "default-after-wait", Kind: "run", Command: "echo must-not-run"},
		{ID: "recover", Kind: "run", Condition: "failure() && steps.failure.outcome == 'failure'", Command: `test "$FAILED_EFFECT" = visible; echo recovered`},
	})
	job.Env = map[string]string{"FAILURE_DONE": marker}

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(logs.String(), "before-barrier") || !strings.Contains(logs.String(), "recovered") || strings.Contains(logs.String(), "must-not-run") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestBackgroundContinueOnError(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "soft-background", Kind: "run", Background: true, ContinueOnError: true, Command: "exit 7"},
		{ID: "wait-soft", Kind: "wait", Targets: []string{"soft-background"}},
		{ID: "after-soft", Kind: "run", Condition: "steps.soft-background.outcome == 'failure' && steps.soft-background.conclusion == 'success'", Command: "echo after-soft"},
	})

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(logs.String(), "after-soft") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestSkippedBackgroundAndRepeatedWaitAreCommittedAtMostOnce(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "skipped", Kind: "run", Background: true, Condition: "false", Command: "exit 1"},
		{ID: "cancel-skipped", Kind: "cancel", Targets: []string{"skipped"}},
		{ID: "wait-skipped", Kind: "wait", Targets: []string{"skipped"}},
		{ID: "completed", Kind: "run", Background: true, Command: `echo once >> "$GITHUB_STEP_SUMMARY"`},
		{ID: "first-wait", Kind: "wait", Targets: []string{"completed"}},
		{ID: "cancel-completed", Kind: "cancel", Targets: []string{"completed"}},
		{ID: "second-wait", Kind: "wait", Targets: []string{"completed"}},
		{ID: "verify", Kind: "run", Condition: "steps.skipped.conclusion == 'skipped' && steps.skipped.outputs.missing == '' && steps.cancel-skipped.conclusion == 'success' && steps.cancel-skipped.outputs.missing == '' && steps.wait-skipped.conclusion == 'success' && steps.wait-skipped.outputs.missing == '' && steps.first-wait.outputs.missing == '' && steps.cancel-completed.conclusion == 'success' && steps.cancel-completed.outputs.missing == '' && steps.second-wait.outputs.missing == ''", Command: "true"},
	})

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Summary != "once\n" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestBackgroundOutputsReturnErrorBeforeBarrier(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `echo "value=private" >> "$GITHUB_OUTPUT"`},
		{ID: "premature-reader", Kind: "run", Command: `echo "${{ steps.background.outputs.value }}"`},
	})

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "unavailable step") {
		t.Fatalf("RunJob() result = %#v, error = %v, want unavailable background output", result, err)
	}
}

func TestExpressionValuedStepControls(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: expression controls\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "soft", Kind: "run", Command: "exit 7", ContinueOnErrorExpression: "${{ matrix.experimental }}", TimeoutMinutesExpression: "${{ matrix.timeout }}"},
		{ID: "verify", Kind: "run", Condition: "steps.soft.outcome == 'failure' && steps.soft.conclusion == 'success'", Command: "true"},
	})
	job.Matrix = map[string]any{"experimental": true, "timeout": 1.0}
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	job, err = plan.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestSkippedStepDoesNotEvaluateTypedControls(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: skipped controls\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "skipped", Kind: "run", Condition: "false", Command: "true", TimeoutMinutesExpression: "${{ fromJSON('invalid') }}",
	}})
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestStepTimeoutExpressionUsesSameStepEnvironment(t *testing.T) {
	step := plan.Step{Env: map[string]string{"MINUTES": "5"}, TimeoutMinutesExpression: "${{ fromJSON(env.MINUTES) }}"}
	context := expression.Context{}
	env, err := evaluateStepMap(step.Env, context)
	if err != nil {
		t.Fatal(err)
	}
	context.Env = env
	step, err = evaluateStepTimeout(step, context)
	if err != nil || step.TimeoutMinutes != 5 {
		t.Fatalf("evaluateStepTimeout() = %#v, %v", step, err)
	}
}

func TestExpressionValuedStepControlsRequireTypedBoundedResults(t *testing.T) {
	for _, test := range []struct {
		name string
		step plan.Step
		want string
	}{
		{name: "boolean", step: plan.Step{ContinueOnErrorExpression: "${{ 'true' }}"}, want: "want boolean"},
		{name: "number", step: plan.Step{TimeoutMinutesExpression: "${{ '1' }}"}, want: "want number"},
		{name: "range", step: plan.Step{TimeoutMinutesExpression: "${{ 361 }}"}, want: "at most 360"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := evaluateStepControls(test.step, expression.Context{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("evaluateStepControls() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestExpressionContinueOnErrorAppliesToPreparedActionFailure(t *testing.T) {
	step := plan.Step{ID: "action", ContinueOnErrorExpression: "${{ true }}"}
	execution := classifyStepExecutionWithControls(t.Context(), t.Context(), step, newResult(), errors.New("pre failed"), expression.Context{})
	if execution.outcome != "failure" || execution.conclusion != "success" {
		t.Fatalf("prepared action execution = %#v", execution)
	}
}

func TestExpressionContinueOnErrorIsNotEvaluatedForCancellation(t *testing.T) {
	jobCtx, cancel := context.WithCancel(t.Context())
	cancel()
	step := plan.Step{ID: "cancelled", ContinueOnErrorExpression: "${{ fromJSON('invalid') }}"}
	execution := classifyStepExecutionWithControls(jobCtx, jobCtx, step, newResult(), context.Canceled, expression.Context{})
	if execution.outcome != "cancelled" || execution.err != context.Canceled {
		t.Fatalf("cancelled execution = %#v", execution)
	}
}

func TestStepNameFailsClosedOnUnavailableBackgroundOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `echo "value=private" >> "$GITHUB_OUTPUT"`},
		{ID: "premature-reader", Name: `${{ steps.background.outputs.value }}`, Kind: "run", Command: `touch should-not-run`},
	})

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "name: expression references unavailable step") {
		t.Fatalf("RunJob() result = %#v, error = %v, want unavailable background output in name", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "should-not-run")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("premature reader command ran: %v", statErr)
	}
}

func TestBackgroundTaskLifecycleTransitions(t *testing.T) {
	states := []struct {
		name  string
		state backgroundTaskState
	}{
		{name: "queued", state: backgroundTaskQueued},
		{name: "running", state: backgroundTaskRunning},
		{name: "finished", state: backgroundTaskFinished},
		{name: "committed", state: backgroundTaskCommitted},
	}
	valid := map[[2]backgroundTaskState]bool{
		{backgroundTaskQueued, backgroundTaskRunning}:     true,
		{backgroundTaskQueued, backgroundTaskFinished}:    true,
		{backgroundTaskRunning, backgroundTaskFinished}:   true,
		{backgroundTaskFinished, backgroundTaskCommitted}: true,
	}

	for _, from := range states {
		for _, to := range states {
			t.Run(from.name+"-to-"+to.name, func(t *testing.T) {
				task := backgroundTask{state: from.state}
				var panicked bool
				func() {
					defer func() { panicked = recover() != nil }()
					task.transitionLocked(to.state)
				}()

				if valid[[2]backgroundTaskState{from.state, to.state}] {
					if panicked || task.state != to.state {
						t.Fatalf("transition %d -> %d: state = %d, panicked = %v", from.state, to.state, task.state, panicked)
					}
				} else if !panicked || task.state != from.state {
					t.Fatalf("invalid transition %d -> %d: state = %d, panicked = %v", from.state, to.state, task.state, panicked)
				}
			})
		}
	}
}

func TestBackgroundSupervisorCommitsCompletedTaskExactlyOnceUnderContention(t *testing.T) {
	supervisor := newBackgroundSupervisor(1)
	task := &backgroundTask{
		done:      make(chan struct{}),
		execution: stepExecution{step: plan.Step{ID: "only"}},
		state:     backgroundTaskFinished,
	}
	close(task.done)

	const callers = 32
	start := make(chan struct{})
	results := make(chan []stepExecution, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- supervisor.commitCompleted([]*backgroundTask{task})
		}()
	}
	close(start)
	group.Wait()
	close(results)

	commits := 0
	for executions := range results {
		commits += len(executions)
		if len(executions) == 1 && executions[0].step.ID != "only" {
			t.Fatalf("committed execution ID = %q, want only", executions[0].step.ID)
		}
	}
	if commits != 1 {
		t.Fatalf("commits = %d, want 1", commits)
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if task.state != backgroundTaskCommitted {
		t.Fatalf("state = %d, want committed", task.state)
	}
}

func TestBackgroundSupervisorBoundsActiveWorkAndQueuesFIFO(t *testing.T) {
	supervisor := newBackgroundSupervisor(maxActiveBackgroundSteps)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	var started atomic.Int32
	for i := range maxActiveBackgroundSteps + 2 {
		supervisor.start(t.Context(), strconv.Itoa(i), func(context.Context) stepExecution {
			current := active.Add(1)
			for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
			}
			started.Add(1)
			<-release
			active.Add(-1)
			return stepExecution{}
		}, func(context.Context) stepExecution { return stepExecution{} })
	}
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() != maxActiveBackgroundSteps && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != maxActiveBackgroundSteps {
		t.Fatalf("started = %d, want %d before release", got, maxActiveBackgroundSteps)
	}
	close(release)
	if got := len(supervisor.waitAll()); got != maxActiveBackgroundSteps+2 {
		t.Fatalf("completed = %d, want %d", got, maxActiveBackgroundSteps+2)
	}
	if got := maximum.Load(); got != maxActiveBackgroundSteps {
		t.Fatalf("maximum active = %d, want %d", got, maxActiveBackgroundSteps)
	}

	fifo := newBackgroundSupervisor(1)
	firstRelease := make(chan struct{})
	var mu sync.Mutex
	var order []string
	start := func(id string, wait <-chan struct{}) {
		fifo.start(t.Context(), id, func(context.Context) stepExecution {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			if wait != nil {
				<-wait
			}
			return stepExecution{}
		}, func(context.Context) stepExecution { return stepExecution{} })
	}
	start("first", firstRelease)
	start("second", nil)
	start("third", nil)
	close(firstRelease)
	fifo.waitAll()
	mu.Lock()
	gotOrder := strings.Join(order, ",")
	mu.Unlock()
	if gotOrder != "first,second,third" {
		t.Fatalf("start order = %q, want FIFO", gotOrder)
	}
}

func TestImplicitWaitAllPrecedesPostCleanup(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/background/action.yml", "name: Background lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/background/main.js", `
const fs = require('fs')
setTimeout(() => {
  fs.appendFileSync(process.env.GITHUB_ENV, 'BACKGROUND_READY=true\n')
  fs.appendFileSync(process.env.GITHUB_OUTPUT, 'value=implicit\n')
}, 50)
`)
	writeFixtureFile(t, workspace, ".github/actions/background/post.js", `
if (process.env.BACKGROUND_READY !== 'true') process.exit(9)
console.log('post-after-implicit-wait')
`)
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "background", Kind: "uses", Uses: "./.github/actions/background", Background: true}})
	job.Outputs = map[string]string{"value": "${{ steps.background.outputs.value }}"}

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["value"] != "implicit" || result.Env["BACKGROUND_READY"] != "true" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	if !strings.Contains(logs.String(), "post-after-implicit-wait") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestJavaScriptActionLifecycleRunsInWorkspace(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cwd/action.yml", "name: CWD\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, workspace, ".github/actions/cwd/"+phase+".js", fmt.Sprintf(`
require('node:fs').appendFileSync(process.env.CWD_LOG, %q + process.cwd() + '\t' + process.env.GITHUB_WORKSPACE + '\n')
`, phase+":"))
	}
	cwdLog := filepath.Join(t.TempDir(), "cwd.log")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cwd", Kind: "uses", Uses: "./.github/actions/cwd", Env: map[string]string{"CWD_LOG": cwdLog}}})
	result, err := (Runner{Node24: node}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	data, err := os.ReadFile(cwdLog)
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("CWD log = %q, want pre/main/post entries", data)
	}
	for _, line := range lines {
		phase, paths, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("CWD log line %q is malformed", line)
		}
		cwd, workspaceEnv, ok := strings.Cut(paths, "\t")
		if !ok || cwd != resolvedWorkspace || workspaceEnv != resolvedWorkspace {
			t.Fatalf("%s phase CWD/workspace = %q / %q, want %q", phase, cwd, workspaceEnv, resolvedWorkspace)
		}
	}
}

func TestJavaScriptActionCanonicalizesWorkspaceAndRunnerTemp(t *testing.T) {
	node := requireNode24(t)
	base := canonicalTempDir(t)
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	workspace := filepath.Join(linkParent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", linkParent)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cwd/action.yml", "name: CWD\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cwd/main.js", `
require('node:fs').writeFileSync(process.env.CWD_LOG, process.cwd() + '\t' + process.env.GITHUB_WORKSPACE + '\t' + process.env.RUNNER_TEMP)
`)
	cwdLog := filepath.Join(base, "cwd.log")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cwd", Kind: "uses", Uses: "./.github/actions/cwd", Env: map[string]string{"CWD_LOG": cwdLog}}})
	result, err := (Runner{Node24: node}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	data, err := os.ReadFile(cwdLog)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(data), "\t")
	if len(parts) != 3 {
		t.Fatalf("CWD log = %q", data)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if parts[0] != resolvedWorkspace || parts[1] != resolvedWorkspace {
		t.Fatalf("CWD/workspace = %q / %q, want %q", parts[0], parts[1], resolvedWorkspace)
	}
	if filepath.Dir(parts[2]) != realParent {
		t.Fatalf("RUNNER_TEMP = %q, want canonical parent %q", parts[2], realParent)
	}
}

func TestConcurrentStreamsShareMaskRegistration(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "mask.ready")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "masker", Kind: "run", Background: true, Command: `echo '::add-mask::cross-stream-secret'; sleep 0.05; touch "$MASK_READY"`},
		{ID: "other-stream", Kind: "run", Command: `while [ ! -f "$MASK_READY" ]; do sleep 0.01; done; echo 'probe cross-stream-secret'`},
		{ID: "wait", Kind: "wait-all"},
	})
	job.Env = map[string]string{"MASK_READY": marker}
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(logs.String(), "cross-stream-secret") || !strings.Contains(logs.String(), "probe ***") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestConcurrentSmokeWorkflowEndToEnd(t *testing.T) {
	workspace := fixturePath(t, "smoke")
	workflowPath := filepath.Join(workspace, ".github", "workflows", "concurrent.yml")
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(filepath.Join(workspace, "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, source, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %#v, want concurrent and observer", plans)
	}
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs}
	concurrent, err := runner.RunJob(t.Context(), plans[0], workspace)
	if err != nil || concurrent.Conclusion != "success" {
		t.Fatalf("concurrent result = %#v, error = %v, logs = %q", concurrent, err, logs.String())
	}
	plans[1].Needs = map[string]plan.Need{"concurrent": {Result: concurrent.Conclusion, Outputs: concurrent.Outputs}}
	observer, err := runner.RunJob(t.Context(), plans[1], workspace)
	if err != nil || observer.Conclusion != "success" {
		t.Fatalf("observer result = %#v, error = %v, logs = %q", observer, err, logs.String())
	}
	if strings.Contains(logs.String(), "concurrent-cross-stream-secret") || !strings.Contains(logs.String(), "CONCURRENT_MASK_PROBE=***") {
		t.Fatalf("concurrent masking logs = %q", logs.String())
	}
	want := `CONCURRENT_OBSERVATION={"cancel":"graceful","failure":"failure-at-wait","implicit":"implicit-wait-all","parallel":"parallel","queue_max":10,"targeted":"targeted-and-full"}`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("concurrent observation missing from logs = %q", logs.String())
	}
}

func TestCancellationTerminatesChildProcessGroup(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := (Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}).runStreaming(ctx, newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"PID_FILE": pidFile}, "sh", "-c", `(trap '' INT TERM; sleep 30) & echo $! > "$PID_FILE"; wait`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStreaming() error = %v, want deadline", err)
	}
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !testProcessExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived cancellation", pid)
}

func TestCancellationEscalatesFromInterruptToTermination(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	signals := filepath.Join(dir, "signals")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelled := make(chan struct{})
	go func() {
		defer close(cancelled)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runner := Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 500 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"READY": ready, "SIGNALS": signals}, "bash", "-c", `
trap 'printf "INT\n" >> "$SIGNALS"' INT
trap 'printf "TERM\n" >> "$SIGNALS"; exit 0' TERM
touch "$READY"
while :; do sleep 1; done`)
	<-cancelled
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	contents, readErr := os.ReadFile(signals)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := string(contents); got != "INT\nTERM\n" {
		t.Fatalf("signal order = %q, want SIGINT then SIGTERM", got)
	}
}

func TestCancellationPreservesInterruptGraceForDescendants(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	childReady := filepath.Join(dir, "child-ready")
	cleaned := filepath.Join(dir, "cleaned")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runner := Runner{InterruptGrace: 2 * time.Second, TerminateGrace: 50 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"READY": ready, "CHILD_READY": childReady, "CLEANED": cleaned}, "bash", "-c", `
(
  trap 'sleep 0.3; touch "$CLEANED"; exit 0' INT
  touch "$CHILD_READY"
  while :; do sleep 1; done
) &
trap 'exit 0' INT
while [ ! -f "$CHILD_READY" ]; do sleep 0.01; done
touch "$READY"
wait`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	if _, statErr := os.Stat(cleaned); statErr != nil {
		t.Fatalf("descendant did not finish SIGINT cleanup during the interrupt grace: %v", statErr)
	}
}

func TestCancellationWaitsForProcessGroupCleanupAfterOutputCloses(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	started := time.Now()
	runner := Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}
	err := runner.runStreaming(ctx, newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"PID_FILE": pidFile, "READY": ready}, "bash", "-c", `
(trap '' INT TERM; exec >/dev/null 2>&1; while :; do sleep 1; done) &
echo $! > "$PID_FILE"
trap 'exit 0' INT
touch "$READY"
while :; do sleep 1; done`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStreaming() error = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("runStreaming() returned before process-group escalation completed: %s", elapsed)
	}
	contents, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !testProcessExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived completed cancellation", pid)
}

func TestExplicitCancelTerminatesBackgroundProcessGroup(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process groups are implemented for the initial Linux runtime and Darwin development hosts")
	}
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	pidFile := filepath.Join(workspace, "child.pid")
	ready := filepath.Join(workspace, "background.ready")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "run", Background: true, Command: `(trap '' INT TERM; sleep 30) & echo $! > "$PID_FILE"; touch "$READY"; wait`},
		{ID: "await-start", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	})
	job.Env = map[string]string{"PID_FILE": pidFile, "READY": ready}

	result, err := (Runner{InterruptGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !testProcessExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("explicitly canceled child process %d survived", pid)
}

func TestNeedStatusesCopiesOnlyExpressionVisibleState(t *testing.T) {
	outputs := map[string]string{"release": "v1"}
	needs := map[string]plan.Need{
		"producer": {
			Result:    "success",
			Outputs:   outputs,
			Artifacts: []plan.NeedArtifact{{Name: "private-runtime-authority"}},
		},
	}

	got := needStatuses(needs)
	want := map[string]expression.NeedStatus{
		"producer": {Outputs: map[string]string{"release": "v1"}, Result: "success"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("needStatuses() = %#v, want %#v", got, want)
	}
	outputs["release"] = "plan"
	if got["producer"].Outputs["release"] != "v1" {
		t.Fatalf("expression output changed through plan: %#v", got)
	}
	got["producer"].Outputs["release"] = "expression"
	if outputs["release"] != "plan" {
		t.Fatalf("plan output changed through expression state: %#v", outputs)
	}
}

func TestDeferredReusableWorkflowInputFlowsFromVerifiedOutputToCalleeStep(t *testing.T) {
	const (
		buildID = "11111111-1111-4111-8111-111111111111"
		jobID   = "22222222-2222-4222-8222-222222222222"
		value   = "c2hhMjU2ICBzdWJqZWN0Cg=="
	)
	planDigest := transport.Digest([]byte("producer plan"))
	stepKey := "gha-hash"
	manifest, err := transport.MarshalResultManifest(transport.ResultManifest{
		PlanDigest: planDigest,
		Producer:   transport.Producer{BuildID: buildID, JobID: jobID, StepKey: stepKey},
		Result:     "success",
		Outputs:    []transport.Output{{Name: "hashes", Value: value}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := transport.ResultPath(stepKey, planDigest)
	inputs, err := ResolveDeferredInputs(t.Context(), transport.Agent{Runner: deferredInputRunner{jobID: jobID, path: artifactPath, data: manifest}}, t.TempDir(), buildID, map[string]plan.DeferredInput{
		"base64-subjects": {
			Sources: []plan.NeedSource{{StepKey: stepKey, PlanDigest: planDigest}},
			Outputs: []plan.NeedOutput{{Name: "value", StepKey: stepKey, Output: "hashes"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inputs["base64-subjects"] != value {
		t.Fatalf("resolved inputs = %#v", inputs)
	}

	workspace := t.TempDir()
	workflowPath := ".github/workflows/generator_generic_slsa3.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:        "create-file",
		Kind:      "run",
		Env:       map[string]string{"UNTRUSTED_SUBJECTS": "${{ inputs.base64-subjects }}"},
		Condition: "inputs.base64-subjects != ''",
		Command:   `test "$UNTRUSTED_SUBJECTS" = "` + value + `" && echo ran=yes >> "$GITHUB_OUTPUT"`,
	}})
	job.Outputs = map[string]string{"ran": "${{ steps.create-file.outputs.ran }}"}
	job.Dependencies = []string{stepKey}
	job.DeferredInputs = map[string]plan.DeferredInput{
		"base64-subjects": {
			Sources: []plan.NeedSource{{StepKey: stepKey, PlanDigest: planDigest}},
			Outputs: []plan.NeedOutput{{Name: "value", StepKey: stepKey, Output: "hashes"}},
		},
	}
	job.CallGuards = []plan.CallGuard{{
		Condition:      "inputs.base64-subjects != ''",
		DeferredInputs: job.DeferredInputs,
	}}
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "deferred reusable-workflow inputs were not hydrated") {
		t.Fatalf("RunJob() unhydrated error = %v", err)
	}
	job.DeferredInputValues = inputs
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "call guard 1: deferred inputs were not hydrated") {
		t.Fatalf("RunJob() unhydrated guard error = %v", err)
	}
	job.CallGuards[0].DeferredInputValues = inputs
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["ran"] != "yes" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestJobConditionConsumesNeedResultAndOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.Needs = map[string]plan.Need{"producer": {Result: "failure", Outputs: map[string]string{"gate": "yes"}}}
	job.Condition = "always() && needs.producer.result == 'failure' && needs.producer.outputs.gate"
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	job.Condition = ""
	job.Needs["producer"] = plan.Need{Result: "skipped"}
	result, err = (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "skipped" {
		t.Fatalf("RunJob() skipped prerequisite result = %#v, error = %v", result, err)
	}
}

func TestReusableWorkflowCallGuardsRunBeforeCapabilitiesAndJobConditions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	marker := filepath.Join(workspace, "ran")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Condition: "always()", Command: "touch " + marker}})
	job.CallGuards = []plan.CallGuard{{Condition: "false"}, {Condition: "always()"}}
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"TOKEN"}
	job.Condition = "always()"

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "skipped" || len(result.Outputs) != 0 {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outer false guard allowed descendant always() step to run: %v", err)
	}
}

func TestReusableWorkflowCallGuardUsesOnlyCallerNeeds(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.CallGuards = []plan.CallGuard{{
		Condition: "success() && needs.caller.result == 'success' && needs.caller.outputs.ready",
		Needs:     map[string]plan.Need{"caller": {Result: "success", Outputs: map[string]string{"ready": "true"}}},
	}}
	job.Needs = map[string]plan.Need{"internal": {Result: "failure"}}
	job.Condition = "always()"

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}

	job.CallGuards[0].Condition = ""
	job.CallGuards[0].Needs["caller"] = plan.Need{Result: "failure"}
	if err := job.Validate(); err == nil {
		t.Fatal("Validate() accepted an empty call guard condition")
	}
	job.CallGuards[0].Condition = "needs.caller.outputs.ready"
	result, err = (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "skipped" {
		t.Fatalf("implicit success guard result = %#v, error = %v", result, err)
	}
	job.CallGuards[0].Condition = "failure()"
	result, err = (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("failure() guard result = %#v, error = %v", result, err)
	}
}

func TestJobRuntimeFieldsEvaluateCompoundExpressions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:      "run",
		Kind:    "run",
		Env:     map[string]string{"SCOPE": "step"},
		Command: `test "$VALUE" = "release-linux-v1" && printf 'value=done\n' >> "$GITHUB_OUTPUT"`,
	}})
	job.Matrix = map[string]any{"os": "linux", "directory": "src"}
	job.Vars = map[string]string{"PREFIX": "release"}
	job.Needs = map[string]plan.Need{"producer": {Result: "success", Outputs: map[string]string{"tag": "v1"}}}
	job.Env = map[string]string{
		"ROOT":  workspace,
		"SCOPE": "job",
		"VALUE": "${{ format('{0}-{1}-{2}', vars.PREFIX, matrix.os, needs.producer.outputs.tag) }}",
	}
	job.DefaultShell = "${{ format('{0}', 'sh') }}"
	job.DefaultWorkingDirectory = "${{ format('{0}/{1}', env.ROOT, matrix.directory) }}"
	job.Outputs = map[string]string{
		"environment": "${{ format('{0}', env.SCOPE) }}",
		"result":      "${{ format('{0}-{1}', steps.run.outputs.value, needs.producer.result) }}",
	}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["result"] != "done-success" || result.Outputs["environment"] != "job" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestDecodedPlanMatrixNumbersDriveRuntimeConditions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	nonzeroMarker := filepath.Join(workspace, "nonzero")
	zeroMarker := filepath.Join(workspace, "zero")
	maxUintMarker := filepath.Join(workspace, "max-uint")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "nonzero", Kind: "run", Condition: "matrix.nonzero", Command: `touch "$NONZERO"`},
		{ID: "zero", Kind: "run", Condition: "matrix.zero", Command: `touch "$ZERO"`},
		{ID: "max-uint", Kind: "run", Condition: "matrix.max_uint != 0", Command: `touch "$MAX_UINT"`},
	})
	job.Condition = "matrix.count == 1"
	job.Matrix = map[string]any{"count": 1, "nonzero": 2, "zero": 0, "max_uint": ^uint64(0)}
	job.Env = map[string]string{"NONZERO": nonzeroMarker, "ZERO": zeroMarker, "MAX_UINT": maxUintMarker}

	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := plan.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{}).RunJob(t.Context(), decoded, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(nonzeroMarker); err != nil {
		t.Fatalf("nonzero matrix condition did not run: %v", err)
	}
	if _, err := os.Stat(zeroMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zero matrix condition unexpectedly ran: %v", err)
	}
	if _, err := os.Stat(maxUintMarker); err != nil {
		t.Fatalf("max uint64 matrix condition did not run: %v", err)
	}
}

func TestRunJobRejectsRegisteredSecretInOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"CANARY"}
	job.Outputs = map[string]string{"leak": "${{ secrets.CANARY }}"}
	_, err := (Runner{Secrets: testSecretResolver{"CANARY": "do-not-publish"}, Redactor: &testRedactor{}}).RunJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "contains a registered secret") || strings.Contains(err.Error(), "do-not-publish") {
		t.Fatalf("RunJob() error = %v, want non-disclosing secret-output rejection", err)
	}
}

func TestRunJobScrubsSecretFromRedactorFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"CANARY"}
	_, err := (Runner{Secrets: testSecretResolver{"CANARY": "do-not-leak"}, Redactor: failingTokenRedactor{token: "do-not-leak"}}).RunJob(t.Context(), job, workspace)
	if err == nil || strings.Contains(err.Error(), "do-not-leak") || !strings.Contains(err.Error(), "***") {
		t.Fatalf("RunJob() error = %v, want scrubbed redactor failure", err)
	}
}

func TestRunJobMintsAndRedactsScopedGitHubWorkflowToken(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow token\n")
	const token = "ghs_scoped_workflow_token"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "use-token", Kind: "run", Shell: "sh", Command: `test "$GH_TOKEN" = "ghs_scoped_workflow_token" && printf '%s\n' "$GH_TOKEN"`,
	}})
	job.Schema = plan.Schema
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "caller.yml", Permissions: map[string]string{"contents": "read", "pull_requests": "write"}}
	job.Env = map[string]string{"GH_TOKEN": "${{ secrets.GITHUB_TOKEN }}"}
	provider := &testWorkflowTokenProvider{token: token}
	redactor := &testRedactor{}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, WorkflowToken: provider, Redactor: redactor}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if provider.calls != 1 || provider.repository != job.Event.Repository || provider.workflow != "caller.yml" || !reflect.DeepEqual(provider.permissions, job.GitHubToken.Permissions) {
		t.Fatalf("token request = calls %d, repository %q, workflow %q, permissions %#v", provider.calls, provider.repository, provider.workflow, provider.permissions)
	}
	if !reflect.DeepEqual(redactor.values, []string{token}) {
		t.Fatalf("redacted values = %#v", redactor.values)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(fmt.Sprintf("%#v", result), token) || result.Env["GH_TOKEN"] != "***" || !strings.Contains(logs.String(), "***") {
		t.Fatalf("workflow token leaked: result = %#v, logs = %q", result, logs.String())
	}
}

func TestRunJobRejectsInvalidWorkflowTokenPolicyBeforeMinting(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/nested/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow token\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.Schema = plan.Schema
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "nested/test.yml", Permissions: map[string]string{"contents": "read"}}
	provider := &testWorkflowTokenProvider{token: "must-not-be-minted"}
	_, err := (Runner{WorkflowToken: provider, Redactor: &testRedactor{}}).RunJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "simple .yml or .yaml filename") || provider.calls != 0 {
		t.Fatalf("RunJob() error/calls = %v / %d", err, provider.calls)
	}
}

func TestResolveActionInputsExposesScopedTokenToMetadataDefaults(t *testing.T) {
	tokenDefault := "${{ github.token }}"
	conditionalTokenDefault := "${{ github.server_url == 'https://github.com' && github.token || '' }}"
	actorDefault := "${{ github.actor }}"
	action := metadata.Metadata{Inputs: map[string]metadata.Input{
		"github_token":      {Default: &tokenDefault},
		"conditional_token": {Default: &conditionalTokenDefault},
		"actor":             {Default: &actorDefault},
	}}
	eval := expression.Context{
		GitHub:  map[string]any{"actor": "octocat", "server_url": "https://github.com"},
		Secrets: map[string]string{"GITHUB_TOKEN": "ghs_scoped_action_default"},
	}

	inputs, err := resolveActionInputs(action, nil, eval)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inputs, map[string]string{"github_token": "ghs_scoped_action_default", "conditional_token": "ghs_scoped_action_default", "actor": "octocat"}) {
		t.Fatalf("resolved action inputs = %#v", inputs)
	}
	if _, leaked := eval.GitHub["token"]; leaked {
		t.Fatalf("metadata default evaluation mutated the shared GitHub context: %#v", eval.GitHub)
	}
	if _, err := expression.Evaluate("${{ github.token }}", eval); err == nil || !strings.Contains(err.Error(), `unavailable github value "token"`) {
		t.Fatalf("workflow github.token evaluation error = %v, want unavailable value", err)
	}

	inputs, err = resolveActionInputs(action, map[string]string{"GITHUB_TOKEN": ""}, eval)
	if err != nil {
		t.Fatal(err)
	}
	if inputs["github_token"] != "" {
		t.Fatalf("explicit empty input did not suppress metadata default: %#v", inputs)
	}

	withoutToken := eval
	withoutToken.Secrets = nil
	if _, err := resolveActionInputs(action, nil, withoutToken); err == nil || !strings.Contains(err.Error(), `unavailable github value "token"`) {
		t.Fatalf("unplanned metadata github.token evaluation error = %v, want unavailable value", err)
	}
	ghesAction := metadata.Metadata{Inputs: map[string]metadata.Input{"token": {Default: &conditionalTokenDefault}}}
	ghes := eval
	ghes.GitHub = map[string]any{"server_url": "https://github.example.com"}
	ghes.Secrets = nil
	inputs, err = resolveActionInputs(ghesAction, nil, ghes)
	if err != nil || inputs["token"] != "" {
		t.Fatalf("GHES conditional token input = %#v, %v, want empty token", inputs, err)
	}
}

func TestResolveActionInputsUsesRunnerDebugDefaultUnlessExplicitlySupplied(t *testing.T) {
	debugDefault := "${{ runner.debug }}"
	action := metadata.Metadata{Inputs: map[string]metadata.Input{"debug": {Default: &debugDefault}}}

	inputs, err := resolveActionInputs(action, nil, expression.Context{})
	if err != nil || inputs["debug"] != "false" {
		t.Fatalf("runner.debug default = %#v, %v; want false", inputs, err)
	}
	inputs, err = resolveActionInputs(action, map[string]string{"DEBUG": "true"}, expression.Context{})
	if err != nil || inputs["debug"] != "true" {
		t.Fatalf("explicit debug input = %#v, %v; want true", inputs, err)
	}
}

func TestStepExpressionContextExposesScopedTokenWithoutMutatingJobContext(t *testing.T) {
	eval := expression.Context{
		GitHub:  map[string]any{"actor": "octocat"},
		Secrets: map[string]string{"GITHUB_TOKEN": "ghs_scoped_step"},
	}
	stepEval := stepExpressionContext(eval)
	value, err := expression.Evaluate("${{ github.token }}", stepEval)
	if err != nil || value != "ghs_scoped_step" {
		t.Fatalf("step github.token = %q, %v", value, err)
	}
	if _, leaked := eval.GitHub["token"]; leaked {
		t.Fatalf("step evaluation mutated job context: %#v", eval.GitHub)
	}
}

func TestOriginUsesProviderServerURLWithoutGitHubToken(t *testing.T) {
	job := plan.Job{Event: plan.Event{Provider: "cursor-origin"}}
	github := githubContext(job)
	env := standardEnvironment(job, "/workspace", "/tmp", "/tool-cache")
	if github["server_url"] != "https://origin.cursor.com" || env["GITHUB_SERVER_URL"] != "https://origin.cursor.com" {
		t.Fatalf("Origin server URLs = context %#v, environment %q", github["server_url"], env["GITHUB_SERVER_URL"])
	}
	conditionalTokenDefault := "${{ github.server_url == 'https://github.com' && github.token || '' }}"
	action := metadata.Metadata{Inputs: map[string]metadata.Input{"token": {Default: &conditionalTokenDefault}}}
	inputs, err := resolveActionInputs(action, nil, expression.Context{GitHub: github})
	if err != nil || inputs["token"] != "" {
		t.Fatalf("Origin conditional token input = %#v, %v, want empty token", inputs, err)
	}
}

func TestGitHubContextExposesRuntimeEventIdentity(t *testing.T) {
	tests := []struct {
		name        string
		event       plan.Event
		wantOwner   string
		wantRefName string
		wantRefType string
		wantBaseRef string
	}{
		{name: "branch", event: plan.Event{Repository: "acme/widgets", Ref: "refs/heads/feature/runtime"}, wantOwner: "acme", wantRefName: "feature/runtime", wantRefType: "branch"},
		{name: "tag", event: plan.Event{Repository: "acme/widgets", Ref: "refs/tags/v1.2.3"}, wantOwner: "acme", wantRefName: "v1.2.3", wantRefType: "tag"},
		{name: "release", event: plan.Event{Name: "release", Repository: "acme/widgets", Ref: "refs/tags/v1.2.3", SHA: strings.Repeat("a", 40)}, wantOwner: "acme", wantRefName: "v1.2.3", wantRefType: "tag"},
		{name: "pull request merge", event: plan.Event{Repository: "acme/widgets", Ref: "refs/pull/42/merge", HeadRef: "feature/runtime", BaseRef: "main"}, wantOwner: "acme", wantRefName: "42/merge", wantRefType: "branch", wantBaseRef: "main"},
		{name: "pull request head", event: plan.Event{Repository: "acme/widgets", Ref: "refs/pull/42/head", HeadRef: "feature/runtime", BaseRef: "main"}, wantOwner: "acme", wantRefName: "42/head", wantRefType: "branch", wantBaseRef: "main"},
		{name: "unavailable", event: plan.Event{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			github := githubContext(plan.Job{Event: test.event})
			if github["repository_owner"] != test.wantOwner || github["ref_name"] != test.wantRefName || github["ref_type"] != test.wantRefType || github["base_ref"] != test.wantBaseRef {
				t.Fatalf("GitHub context = %#v", github)
			}
			condition := "github.repository_owner == '" + test.wantOwner + "' && github.ref_name == '" + test.wantRefName + "' && github.ref_type == '" + test.wantRefType + "' && github.base_ref == '" + test.wantBaseRef + "'"
			got, err := expression.EvaluateCondition(condition, expression.ConditionContext{GitHub: github})
			if err != nil || !got {
				t.Fatalf("EvaluateCondition(%q) = %v, %v", condition, got, err)
			}
			if test.name == "release" {
				env := standardEnvironment(plan.Job{Event: test.event}, "/workspace", "/tmp", "/tool-cache")
				if env["GITHUB_EVENT_NAME"] != "release" || env["GITHUB_REF"] != "refs/tags/v1.2.3" || env["GITHUB_SHA"] != strings.Repeat("a", 40) {
					t.Fatalf("release environment = %#v", env)
				}
			}
		})
	}
}

func TestStandardEnvironmentSuppliesProtectedGitHubWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		workflow plan.Workflow
		want     string
	}{
		{name: "declared name", workflow: plan.Workflow{Path: ".github/workflows/ci.yml", Name: "CI"}, want: "CI"},
		{name: "path fallback", workflow: plan.Workflow{Path: ".github/workflows/ci.yml"}, want: ".github/workflows/ci.yml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := standardEnvironment(plan.Job{Workflow: test.workflow}, "/workspace", "/tmp", "/tool-cache")
			if got := env["GITHUB_WORKFLOW"]; got != test.want {
				t.Fatalf("GITHUB_WORKFLOW = %q, want %q", got, test.want)
			}
			merged := mergeStepEnvironment(env, map[string]string{"GITHUB_WORKFLOW": "spoofed"})
			if got := merged["GITHUB_WORKFLOW"]; got != test.want {
				t.Fatalf("overlaid GITHUB_WORKFLOW = %q, want protected value %q", got, test.want)
			}
		})
	}
}

func TestRunJobSuppliesScopedGitHubTokenToEffectiveActionDefault(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "test.yml")
	workflow := []byte(`on: push
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/token

      - if: env.GITHUB_SHA == 'action-sha' && env.RUNNER_TEMP == '/action-temp'
        env:
          DIRECT_GITHUB_TOKEN: ${{ github.token }}
          ENV_GITHUB_SHA: ${{ env.GITHUB_SHA }}
          ENV_RUNNER_TEMP: ${{ env.RUNNER_TEMP }}
        run: |
          test "$GITHUB_TOKEN" = "ghs_scoped_action_default"
          test "$DIRECT_GITHUB_TOKEN" = "ghs_scoped_action_default"
          test "$GITHUB_SHA" = "1111111111111111111111111111111111111111"
          test "$RUNNER_TEMP" = "$EXPECTED_RUNNER_TEMP"
          test "$ENV_GITHUB_SHA" = "action-sha"
          test "$ENV_RUNNER_TEMP" = "/action-temp"
          printf 'exported token: %s\n' "$GITHUB_TOKEN"

      - uses: ./.github/actions/observer

      - env:
          GITHUB_ACTION_PATH: step-action-path
          GITHUB_TOKEN: step-token
          GITHUB_SHA: step-sha
          RUNNER_TEMP: /step-temp
          ENV_GITHUB_ACTION_PATH: ${{ env.GITHUB_ACTION_PATH }}
          ENV_GITHUB_SHA: ${{ env.GITHUB_SHA }}
          ENV_RUNNER_TEMP: ${{ env.RUNNER_TEMP }}
        run: |
          test "$GITHUB_ACTION_PATH" = "step-action-path"
          test "$GITHUB_TOKEN" = "step-token"
          test "$GITHUB_SHA" = "1111111111111111111111111111111111111111"
          test "$RUNNER_TEMP" = "$EXPECTED_RUNNER_TEMP"
          test "$ENV_GITHUB_ACTION_PATH" = "file-action-path"
          test "$ENV_GITHUB_SHA" = "action-sha"
          test "$ENV_RUNNER_TEMP" = "/action-temp"
`)
	writeFixtureFile(t, workspace, ".github/actions/token/action.yml", `name: token default
inputs:
  github_token:
    default: ${{ github.server_url == 'https://github.com' && github.token || '' }}
runs:
  using: node24
  main: dist/index.js
`)
	writeFixtureFile(t, workspace, ".github/actions/token/dist/index.js", `const fs = require("node:fs");
if (process.env.GITHUB_TOKEN !== undefined) throw new Error("GITHUB_TOKEN was injected before the action exported it");
if (process.env.INPUT_GITHUB_TOKEN !== "ghs_scoped_action_default") throw new Error("scoped token input was not provided");
fs.appendFileSync(process.env.GITHUB_ENV,
  "GITHUB_TOKEN=" + process.env.INPUT_GITHUB_TOKEN + "\n" +
  "GITHUB_ACTION_PATH=file-action-path\n" +
  "GITHUB_SHA=action-sha\n" +
  "RUNNER_TEMP=/action-temp\n" +
  "EXPECTED_RUNNER_TEMP=" + process.env.RUNNER_TEMP + "\n");
`)
	writeFixtureFile(t, workspace, ".github/actions/observer/action.yml", `name: context observer
runs:
  using: composite
  steps:
    - shell: sh
      env:
        ENV_GITHUB_ACTION_PATH: ${{ env.GITHUB_ACTION_PATH }}
        ENV_GITHUB_SHA: ${{ env.GITHUB_SHA }}
        ENV_RUNNER_TEMP: ${{ env.RUNNER_TEMP }}
      run: |
        test "$GITHUB_TOKEN" = "ghs_scoped_action_default"
        test "$GITHUB_ACTION_PATH" != "file-action-path"
        test -f "$GITHUB_ACTION_PATH/action.yml"
        test "$GITHUB_SHA" = "1111111111111111111111111111111111111111"
        test "$RUNNER_TEMP" = "$EXPECTED_RUNNER_TEMP"
        test "$ENV_GITHUB_ACTION_PATH" = "file-action-path"
        test "$ENV_GITHUB_SHA" = "action-sha"
        test "$ENV_RUNNER_TEMP" = "/action-temp"
`)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", string(workflow))
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(t.Context(), workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels:                     map[string]string{"ubuntu-latest": ""},
			AllowUntrustedDefaultQueue: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil {
		t.Fatalf("compiled scoped token plan = %#v", plans)
	}
	provider := &testWorkflowTokenProvider{token: "ghs_scoped_action_default"}
	redactor := &testRedactor{}
	var logs bytes.Buffer
	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs, WorkflowToken: provider, Redactor: redactor}).RunJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if provider.calls != 1 || !reflect.DeepEqual(provider.permissions, map[string]string{"contents": "read"}) || !reflect.DeepEqual(redactor.values, []string{"ghs_scoped_action_default"}) {
		t.Fatalf("token handling = provider calls %d, permissions %#v, redactions %#v", provider.calls, provider.permissions, redactor.values)
	}
	if result.Env["GITHUB_TOKEN"] != "***" || result.Env["GITHUB_SHA"] != "action-sha" || result.Env["RUNNER_TEMP"] != "/action-temp" || strings.Contains(logs.String(), "ghs_scoped_action_default") || !strings.Contains(logs.String(), "exported token: ***") {
		t.Fatalf("exported workflow token leaked: result = %#v, logs = %q", result, logs.String())
	}
}

func TestCompileAndRunJobDiscardsTopLevelActionEnv(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "test.yml")
	workflow := []byte(`on: push
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/release
        env:
          GITHUB_TOKEN: workflow-step-token
          PRESERVED: workflow-step-env
`)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", string(workflow))
	writeFixtureFile(t, workspace, ".github/actions/release/action.yml", `name: GH Release
env:
  GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
  METADATA_ONLY: ${{ secrets.DEPLOY_TOKEN }}
runs:
  using: composite
  steps:
    - shell: sh
      run: |
        test "$GITHUB_TOKEN" = workflow-step-token
        test "$PRESERVED" = workflow-step-env
        test -z "${METADATA_ONLY:-}"
`)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken != nil || len(plans[0].RequiredSecrets) != 0 || plans[0].HasCapability("provider-token-write") || plans[0].HasCapability("secrets") {
		t.Fatalf("top-level action env added authority to plan: %#v", plans)
	}
	encoded, err := json.Marshal(plans[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "METADATA_ONLY") || strings.Contains(string(encoded), "DEPLOY_TOKEN") {
		t.Fatalf("top-level action env leaked into plan: %s", encoded)
	}

	provider := &testWorkflowTokenProvider{token: "must-not-be-minted"}
	result, err := (Runner{WorkflowToken: provider, Secrets: testSecretResolver{}}).RunJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if provider.calls != 0 {
		t.Fatalf("top-level action env requested %d workflow tokens", provider.calls)
	}
}

func TestRunJobAbortsAndScrubsWorkflowTokenWhenRedactionFails(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow token\n")
	const token = "ghs_redaction_failure_token"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "never", Kind: "run", Command: "false"}})
	job.Schema = plan.Schema
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"pull_requests": "write"}}
	_, err := (Runner{WorkflowToken: &testWorkflowTokenProvider{token: token}, Redactor: failingTokenRedactor{token: token}}).RunJob(t.Context(), job, workspace)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("RunJob() error = %v, want redaction failure without token disclosure", err)
	}
}

func TestRunJobRequiresHydratedStaticDependencyResults(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: dependency boundary\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.Dependencies = []string{"gha-producer"}
	job.NeedSources = map[string][]plan.NeedSource{"producer": {{StepKey: "gha-producer", PlanDigest: "sha256:" + strings.Repeat("1", 64)}}}
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "no hydrated prerequisite results") {
		t.Fatalf("RunJob() error = %v, want missing hydration rejection", err)
	}
	job.Needs = map[string]plan.Need{"producer": {Result: "success"}}
	if result, err := (Runner{}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() hydrated result = %#v, error = %v", result, err)
	}
}

func TestJavaScriptPreMainPostFilesAndMasking(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "hello"}}})
	job.Outputs = map[string]string{"result": "${{ steps.javascript.outputs.result }}"}
	result, err := runner.RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if got := result.Outputs["result"]; got != "hello-javascript" {
		t.Errorf("output = %q, want hello-javascript", got)
	}
	if got := result.Env["RUNTIME_SEEN"]; got != "true" {
		t.Errorf("environment = %q, want true", got)
	}
	if got := result.State["phase"]; got != "main" {
		t.Errorf("state = %q, want main", got)
	}
	if got := result.State["pre"]; got != "ready" {
		t.Errorf("pre state = %q, want ready", got)
	}
	if result.Summary != "runtime main summary\nruntime post single\n" {
		t.Errorf("summary = %q", result.Summary)
	}
	if strings.Contains(logs.String(), "runtime-secret-value") {
		t.Fatalf("raw forwarded logs contain literal secret: %q", logs.String())
	}
	for _, event := range []string{"lifecycle:pre", "lifecycle:main", "masked probe: ***", "lifecycle:post:single"} {
		if !strings.Contains(logs.String(), event) {
			t.Errorf("logs = %q, want event %q", logs.String(), event)
		}
	}
	pre := strings.Index(logs.String(), "lifecycle:pre")
	main := strings.Index(logs.String(), "lifecycle:main")
	post := strings.Index(logs.String(), "lifecycle:post:single")
	if pre > main || main > post {
		t.Errorf("lifecycle logs are out of order: %q", logs.String())
	}
}

func TestPostActionSummaryOverflowIsTruncatedWithoutFailingJob(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/summary/action.yml", `name: Summary writer
runs:
  using: node24
  main: main.js
  post: post.js
`)
	writeFixtureFile(t, workspace, ".github/actions/summary/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/summary/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v24.0.0
  exit 0
fi
case "${1##*/}" in
  main.js) head -c "$MAIN_SUMMARY_BYTES" /dev/zero | tr '\000' m >> "$GITHUB_STEP_SUMMARY" ;;
  post.js) printf 'post-summary-must-be-truncated\n' >> "$GITHUB_STEP_SUMMARY" ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "summary", Kind: "uses", Uses: "./.github/actions/summary"}})
	job.Env = map[string]string{"MAIN_SUMMARY_BYTES": strconv.Itoa(maxJobSummaryBytes)}

	result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if len(result.Summary) > maxJobSummaryBytes || strings.Contains(result.Summary, "post-summary-must-be-truncated") || !strings.HasSuffix(result.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("RunJob() summary bytes = %d, suffix present = %v", len(result.Summary), strings.HasSuffix(result.Summary, jobSummaryTruncationNotice))
	}
}

func TestOversizedStepSummaryIsNonFatalAndDiscarded(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "oversized", Kind: "run", Shell: "sh", Command: `
printf 'SUMMARY_EFFECT=preserved\n' >> "$GITHUB_ENV"
head -c "$SUMMARY_BYTES" /dev/zero | tr '\000' x >> "$GITHUB_STEP_SUMMARY"`},
		{ID: "after", Kind: "run", Shell: "sh", Command: `
test "$SUMMARY_EFFECT" = preserved
printf 'retained summary\n' >> "$GITHUB_STEP_SUMMARY"`},
	})
	job.Env = map[string]string{"SUMMARY_BYTES": strconv.Itoa(maxCommandFileBytes + 1)}
	var logs bytes.Buffer

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Env["SUMMARY_EFFECT"] != "preserved" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if result.Summary != "retained summary\n" || !strings.Contains(logs.String(), "GITHUB_STEP_SUMMARY upload skipped") {
		t.Fatalf("RunJob() summary = %q, logs = %q", result.Summary, logs.String())
	}
}

func TestPostActionsRunLIFOAfterMainFailure(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{
		{ID: "one", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "one", "order": "one"}},
		{ID: "two", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "two", "order": "two", "fail": "true"}},
	})
	_, err := runner.RunJob(t.Context(), job, workspace)
	if err == nil {
		t.Fatal("RunJob() error = nil, want main failure")
	}
	if !strings.Contains(logs.String(), "requested main failure") {
		t.Fatalf("forwarded logs = %q, want requested main failure", logs.String())
	}
	one := strings.Index(logs.String(), "lifecycle:post:one")
	two := strings.Index(logs.String(), "lifecycle:post:two")
	if two < 0 || one < 0 || two > one {
		t.Errorf("post logs are not LIFO: %q", logs.String())
	}
}

func TestJobContinueOnErrorPreservesFailureLifecycleAndOutputs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "test.yml")
	workflow := []byte(`on: push
jobs:
  report:
    runs-on: ubuntu-latest
    continue-on-error: true
    outputs:
      diagnostic: ${{ steps.fail.outputs.diagnostic }}
    steps:
      - uses: ./.github/actions/lifecycle
      - id: fail
        run: echo "diagnostic=failed" >> "$GITHUB_OUTPUT"; exit 7
      - run: touch "$ORDINARY_MARKER"
      - if: failure()
        run: touch "$RECOVERY_MARKER"
`)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", string(workflow))
	writeFixtureFile(t, workspace, ".github/actions/lifecycle/action.yml", "name: lifecycle\ninputs:\n  job_status:\n    default: ${{ job.status }}\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n  post-if: failure()\n")
	writeFixtureFile(t, workspace, ".github/actions/lifecycle/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/lifecycle/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "${1##*/}" = post.js ]; then printenv INPUT_JOB_STATUS > "$POST_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.Schema || !plans[0].ContinueOnError {
		t.Fatalf("compiled plans = %#v, want one tolerated plan", plans)
	}
	ordinary := filepath.Join(workspace, "ordinary")
	recovery := filepath.Join(workspace, "recovery")
	post := filepath.Join(workspace, "post")
	plans[0].Env = map[string]string{"ORDINARY_MARKER": ordinary, "RECOVERY_MARKER": recovery, "POST_MARKER": post}

	result, runErr := (Runner{Node24: fakeNode}).RunJob(t.Context(), plans[0], workspace)
	if runErr == nil || !IsToleratedJobFailure(runErr) || result.Conclusion != "success" || result.Outputs["diagnostic"] != "failed" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, runErr)
	}
	if _, err := os.Stat(ordinary); !os.IsNotExist(err) {
		t.Fatalf("ordinary success-gated step ran after failure: %v", err)
	}
	if _, err := os.Stat(recovery); err != nil {
		t.Fatalf("failure-gated step did not run: %v", err)
	}
	status, err := os.ReadFile(post)
	if err != nil || strings.TrimSpace(string(status)) != "failure" {
		t.Fatalf("post job.status = %q, %v, want failure", status, err)
	}

	if err := os.Remove(post); err != nil {
		t.Fatal(err)
	}
	timedOut := plans[0]
	timedOut.TimeoutMinutes = 0.001
	timedOut.Steps = slices.Clone(timedOut.Steps)
	timedOut.Steps[1].Command = "sleep 1"
	result, runErr = (Runner{Node24: fakeNode}).RunJob(t.Context(), timedOut, workspace)
	if !errors.Is(runErr, context.DeadlineExceeded) || IsToleratedJobFailure(runErr) || result.Conclusion != "cancelled" {
		t.Fatalf("timed out RunJob() result = %#v, error = %v", result, runErr)
	}
	if _, err := os.Stat(post); !os.IsNotExist(err) {
		t.Fatalf("failure-only post ran after job cancellation: %v", err)
	}
}

func TestJobTimeoutDuringSetupIsCancelled(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: setup timeout\n")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
	job.ContinueOnError = true
	job.TimeoutMinutes = 0.001
	runner := Runner{ResolveMise: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}

	result, err := runner.RunJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || IsToleratedJobFailure(err) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestJavaScriptPostConditionsUseFinalJobStatus(t *testing.T) {
	tests := []struct {
		name        string
		condition   string
		failMain    bool
		wantPost    bool
		wantStatus  string
		wantFailure bool
	}{
		{name: "success after success", condition: "success()", wantPost: true, wantStatus: "success"},
		{name: "success after failure", condition: "${{ success() }}", failMain: true, wantFailure: true},
		{name: "failure after failure", condition: "failure()", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
		{name: "final step state after failure", condition: "failure() && steps.conditional.outcome == 'failure'", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
		{name: "always after failure", condition: "always()", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
		{name: "not cancelled after success", condition: "!cancelled()", wantPost: true, wantStatus: "success"},
		{name: "not cancelled after failure", condition: "!cancelled()", failMain: true, wantPost: true, wantStatus: "failure", wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			writeFixtureFile(t, workspace, ".github/actions/conditional/action.yml", "name: Conditional post\ninputs:\n  job_status:\n    default: ${{ job.status }}\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n  post-if: "+test.condition+"\n")
			writeFixtureFile(t, workspace, ".github/actions/conditional/main.js", "")
			writeFixtureFile(t, workspace, ".github/actions/conditional/post.js", "")
			fakeNode := filepath.Join(workspace, "node24")
			writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "${1##*/}" = post.js ]; then printenv INPUT_JOB_STATUS > "$POST_MARKER"; fi
if [ "${1##*/}" = main.js ] && [ "${FAIL_MAIN:-false}" = true ]; then exit 9; fi
`)
			if err := os.Chmod(fakeNode, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(workspace, "post-ran")
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "conditional", Kind: "uses", Uses: "./.github/actions/conditional"}})
			job.Env = map[string]string{"POST_MARKER": marker, "FAIL_MAIN": strconv.FormatBool(test.failMain)}

			result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace)
			if (err != nil) != test.wantFailure || (result.Conclusion == "failure") != test.wantFailure {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			_, statErr := os.Stat(marker)
			if gotPost := statErr == nil; gotPost != test.wantPost {
				t.Fatalf("post ran = %v, want %v (stat error %v)", gotPost, test.wantPost, statErr)
			}
			if test.wantPost {
				status, readErr := os.ReadFile(marker)
				if readErr != nil || strings.TrimSpace(string(status)) != test.wantStatus {
					t.Fatalf("post job.status = %q, %v, want %q", status, readErr, test.wantStatus)
				}
			}
		})
	}
}

func TestRustCachePostConditionUsesFinalStatusAndMainEnvironment(t *testing.T) {
	tests := []struct {
		name            string
		failJob         bool
		cacheValue      string
		finalCacheValue string
		wantPost        bool
	}{
		{name: "success", wantPost: true},
		{name: "failure default", failJob: true},
		{name: "failure disabled", failJob: true, cacheValue: "false"},
		{name: "failure enabled", failJob: true, cacheValue: "true", wantPost: true},
		{name: "later environment wins", failJob: true, cacheValue: "true", finalCacheValue: "false"},
		{name: "post process sees later environment", cacheValue: "true", finalCacheValue: "false", wantPost: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
			if err != nil {
				t.Fatal(err)
			}
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: rust-cache lifecycle\n")
			writeFixtureFile(t, workspace, ".github/actions/rust-cache/action.yml", `name: rust-cache
runs:
  using: node24
  main: main.js
  post: post.js
  post-if: success() || env.CACHE_ON_FAILURE == 'true'
`)
			writeFixtureFile(t, workspace, ".github/actions/rust-cache/main.js", "")
			writeFixtureFile(t, workspace, ".github/actions/rust-cache/post.js", "")
			fakeNode := filepath.Join(workspace, "node24")
			writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
case "$(basename "$1")" in
  main.js)
    if [ -n "${CACHE_SETTING:-}" ]; then printf 'CACHE_ON_FAILURE=%s\n' "$CACHE_SETTING" >> "$GITHUB_ENV"; fi
    printf '%s\n' 'GITHUB_WORKSPACE=/tmp/spoofed' >> "$GITHUB_ENV"
    ;;
  post.js) printf '%s|%s' "${CACHE_ON_FAILURE:-}" "$GITHUB_WORKSPACE" > "$POST_MARKER" ;;
esac
`)
			if err := os.Chmod(fakeNode, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(workspace, "post")
			steps := []plan.Step{{ID: "cache", Kind: "uses", Uses: "./.github/actions/rust-cache"}}
			if test.finalCacheValue != "" {
				steps = append(steps, plan.Step{ID: "override", Kind: "run", Command: fmt.Sprintf("printf 'CACHE_ON_FAILURE=%s\\n' >> \"$GITHUB_ENV\"", test.finalCacheValue)})
			}
			if test.failJob {
				steps = append(steps, plan.Step{ID: "fail", Kind: "run", Command: "exit 7"})
			}
			job := runtimePlan(t, workspace, workflowPath, steps)
			job.Env = map[string]string{"CACHE_SETTING": test.cacheValue, "POST_MARKER": marker}
			result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace)
			if (err != nil) != test.failJob || (result.Conclusion == "failure") != test.failJob {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			_, statErr := os.Stat(marker)
			if got := statErr == nil; got != test.wantPost {
				t.Fatalf("post ran = %v, want %v (stat error %v)", got, test.wantPost, statErr)
			}
			if test.wantPost && test.finalCacheValue != "" {
				value, err := os.ReadFile(marker)
				want := test.finalCacheValue + "|" + resolvedWorkspace
				if err != nil || string(value) != want {
					t.Fatalf("post environment = %q, %v, want %q", value, err, want)
				}
			}
		})
	}
}

func TestNestedSetupRustToolchainPostUsesMainEnvironmentAtJobTeardown(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested rust-cache lifecycle\n")
	writeFixtureFile(t, workspace, ".github/actions/setup-rust-toolchain/action.yml", `name: setup-rust-toolchain
runs:
  using: composite
  steps:
    - uses: ./.github/actions/rust-cache
    - id: finalize
      shell: sh
      run: "true"
`)
	writeFixtureFile(t, workspace, ".github/actions/rust-cache/action.yml", `name: rust-cache
runs:
  using: node24
  main: main.js
  post: post.js
  post-if: (success() || env.CACHE_ON_FAILURE == 'true') && env.PARENT_FLAG == 'true' && steps.finalize.conclusion == 'success'
`)
	writeFixtureFile(t, workspace, ".github/actions/rust-cache/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/rust-cache/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
case "$(basename "$1")" in
  main.js) printf '%s\n' 'CACHE_ON_FAILURE=true' >> "$GITHUB_ENV" ;;
  post.js) touch "$POST_MARKER" ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "post")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "setup", Kind: "uses", Uses: "./.github/actions/setup-rust-toolchain", Env: map[string]string{"PARENT_FLAG": "true"}},
		{ID: "not-yet", Kind: "run", Command: `test ! -e "$POST_MARKER"`},
		{ID: "finalize", Kind: "run", Command: "exit 7"},
	})
	job.Env = map[string]string{"POST_MARKER": marker}
	result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("nested rust-cache post did not run during teardown: %v", err)
	}
}

func TestJavaScriptPostConditionUsesWorkflowInputsAndPostHashFiles(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: lifecycle hashFiles\n")
	writeFixtureFile(t, workspace, "Cargo.lock", "locked")
	writeFixtureFile(t, workspace, ".github/actions/cache/action.yml", `name: cache
runs:
  using: node24
  main: main.js
  post: post.js
  post-if: inputs.cache == true && hashFiles('Cargo.lock') != ''
`)
	writeFixtureFile(t, workspace, ".github/actions/cache/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/cache/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "$(basename "$1")" = post.js ]; then touch "$POST_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "post")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cache", Kind: "uses", Uses: "./.github/actions/cache"}})
	job.Inputs = map[string]any{"cache": true}
	job.Env = map[string]string{"POST_MARKER": marker}
	result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post did not run: %v", err)
	}
}

func TestRunnerToolCacheIsPerJobAndReserved(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:      "tool-cache",
		Kind:    "run",
		Command: `test -d "$RUNNER_TOOL_CACHE"; case "$RUNNER_TOOL_CACHE" in "$RUNNER_TEMP"/*) ;; *) exit 9 ;; esac`,
	}})
	job.Env = map[string]string{"RUNNER_TOOL_CACHE": filepath.Join(workspace, "untrusted")}
	if result, err := (Runner{}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunnerUsesConfiguredToolCache(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	toolCache, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:      "tool-cache",
		Kind:    "run",
		Command: `test "$RUNNER_TOOL_CACHE" = "$EXPECTED_TOOL_CACHE"`,
	}})
	job.Env = map[string]string{"EXPECTED_TOOL_CACHE": toolCache}
	if result, err := (Runner{ToolCache: toolCache}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestUbuntuImageOS(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "Ubuntu 24", source: "ID=ubuntu\nVERSION_ID=\"24.04\"\n", want: "ubuntu24"},
		{name: "Ubuntu 22", source: "VERSION_ID='22.04'\nID=ubuntu\n", want: "ubuntu22"},
		{name: "other distribution", source: "ID=debian\nVERSION_ID=12\n"},
		{name: "missing version", source: "ID=ubuntu\n"},
		{name: "non-LTS version", source: "ID=ubuntu\nVERSION_ID=24.10\n"},
		{name: "invalid version", source: "ID=ubuntu\nVERSION_ID=current\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ubuntuImageOS([]byte(test.source)); got != test.want {
				t.Fatalf("ubuntuImageOS() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestImageOSUsesNormalEnvironmentPrecedence(t *testing.T) {
	if got := mergeStepEnvironment(map[string]string{"ImageOS": "ubuntu24"}, map[string]string{"ImageOS": "workflow-controlled"}); got["ImageOS"] != "workflow-controlled" {
		t.Fatalf("ImageOS did not use normal workflow environment precedence: %#v", got)
	}
}

func TestCanonicalRunnerContext(t *testing.T) {
	for _, test := range []struct {
		goos, goarch, os, arch string
	}{
		{goos: "linux", goarch: "amd64", os: "Linux", arch: "X64"},
		{goos: "darwin", goarch: "arm64", os: "macOS", arch: "ARM64"},
	} {
		got, err := canonicalRunnerContext(test.goos, test.goarch)
		if err != nil || got["os"] != test.os || got["arch"] != test.arch {
			t.Errorf("canonicalRunnerContext(%s, %s) = %#v, %v", test.goos, test.goarch, got, err)
		}
	}
	if _, err := canonicalRunnerContext("linux", "arm64"); err == nil {
		t.Fatal("canonicalRunnerContext() accepted unsupported pair")
	}
}

func TestManagedNodeDigestsCoverSupportedPlatforms(t *testing.T) {
	for _, platform := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		for _, major := range []int{16, 20, 24} {
			got := nodeDigest(platform[0], platform[1], major)
			decoded, err := hex.DecodeString(got)
			if err != nil || len(decoded) != sha256.Size {
				t.Errorf("nodeDigest(%q, %q, %d) = %q", platform[0], platform[1], major, got)
			}
		}
	}
	if got := nodeDigest("darwin", "amd64", 24); got != "" {
		t.Fatalf("nodeDigest() unsupported platform = %q", got)
	}
}

func TestValidateHostRejectsDockerOnDarwin(t *testing.T) {
	job := plan.Job{RequiredCapabilities: []string{"docker", "network"}}
	if err := ValidateHost(job, "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "unsupported on macOS") {
		t.Fatalf("ValidateHost() Darwin Docker error = %v", err)
	}
	if err := ValidateHost(job, "linux", "amd64"); err != nil {
		t.Fatalf("ValidateHost() Linux Docker error = %v", err)
	}
	if err := ValidateHost(job, "darwin", "amd64"); err == nil || !strings.Contains(err.Error(), "unsupported runner platform") {
		t.Fatalf("ValidateHost() unsupported platform error = %v", err)
	}
}

func TestRunnerEnvironmentIsProtected(t *testing.T) {
	base := map[string]string{"RUNNER_OS": "Linux", "RUNNER_ARCH": "X64"}
	got := mergeStepEnvironment(base, map[string]string{"RUNNER_OS": "overridden", "RUNNER_ARCH": "overridden"})
	if got["RUNNER_OS"] != "Linux" || got["RUNNER_ARCH"] != "X64" {
		t.Fatalf("runner environment was overridden: %#v", got)
	}
}

func TestJavaScriptInputEnvironmentMatchesToolkitNames(t *testing.T) {
	env := actionInputEnv(map[string]string{"node-version": "24", "two words": "value"})
	if env["INPUT_NODE-VERSION"] != "24" || env["INPUT_TWO_WORDS"] != "value" {
		t.Fatalf("actionInputEnv() = %#v", env)
	}
	if _, ok := env["INPUT_NODE_VERSION"]; ok {
		t.Fatalf("actionInputEnv() rewrote a hyphen: %#v", env)
	}
}

func TestConcurrentPostActionsRunLIFOByRegistration(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/background/action.yml", "name: Background lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/background/main.js", "require('fs').writeFileSync(process.env.READY, 'ready')\nsetTimeout(() => {}, 100)\n")
	writeFixtureFile(t, workspace, ".github/actions/background/post.js", "console.log('post:background')\n")
	writeFixtureFile(t, workspace, ".github/actions/foreground/action.yml", "name: Foreground lifecycle\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/foreground/main.js", "console.log('main:foreground')\n")
	writeFixtureFile(t, workspace, ".github/actions/foreground/post.js", "console.log('post:foreground')\n")
	ready := filepath.Join(workspace, "background.ready")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "background", Kind: "uses", Uses: "./.github/actions/background", Background: true},
		{ID: "await-background", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "foreground", Kind: "uses", Uses: "./.github/actions/foreground"},
		{ID: "wait-background", Kind: "wait", Targets: []string{"background"}},
	})
	job.Env = map[string]string{"READY": ready}
	var logs bytes.Buffer

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	background := strings.Index(logs.String(), "post:background")
	foreground := strings.Index(logs.String(), "post:foreground")
	if foreground < 0 || background < 0 || foreground > background {
		t.Fatalf("concurrent post logs are not registration-order LIFO: %q", logs.String())
	}
}

func TestPostActionsUseBoundedCleanupContext(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/action.yml", "name: Slow post\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/main.js", "console.log('main completed')\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/post.js", "setTimeout(() => console.log('slow post completed'), 30000)\n")
	runner := Runner{Node24: node, CleanupTimeout: 200 * time.Millisecond}
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
	started := time.Now()
	_, err := runner.RunJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("bounded cleanup took %s, want under 3s", elapsed)
	}
}

func TestJobTimeoutLimitsPostActionsToCleanupGrace(t *testing.T) {
	t.Parallel()

	workspace := canonicalTempDir(t)
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/action.yml", "name: Job timeout post\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/slow/post.js", "")
	postStarted := filepath.Join(workspace, "post-started")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "$(basename "$1")" = post.js ]; then : > "$POST_STARTED"; fi
sleep 30
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
	// Leave enough of the job budget for process discovery on slower Darwin
	// hosts; the post still has only the separate 250 ms cleanup grace.
	job.TimeoutMinutes = 0.01
	job.Env = map[string]string{"POST_STARTED": postStarted}
	runner := Runner{
		Node24: fakeNode, CleanupTimeout: 250 * time.Millisecond, PostActionTimeout: 3 * time.Second,
		InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond,
	}
	started := time.Now()
	result, err := runner.RunJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(postStarted); err != nil {
		t.Fatalf("post action did not start during cleanup grace: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("job-timeout cleanup took %s, want cleanup grace rather than 3s post budget", elapsed)
	}
}

func TestCancellationStillRunsRegisteredPostAction(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/action.yml", "name: Cancellation cleanup\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/main.js", "setTimeout(() => {}, 30000)\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/post.js", "console.log('post-after-cancel')\n")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "cancel", Kind: "uses", Uses: "./.github/actions/cancel"}})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(ctx, job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" || !strings.Contains(logs.String(), "post-after-cancel") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestExplicitBackgroundCancelStillRunsRegisteredPostAction(t *testing.T) {
	t.Parallel()

	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/action.yml", "name: Explicit cancellation cleanup\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/main.js", "require('fs').writeFileSync(process.env.READY, 'ready')\nsetInterval(() => {}, 30000)\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/post.js", "console.log('post-after-explicit-cancel')\n")
	ready := filepath.Join(workspace, "action.ready")
	var logs bytes.Buffer
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{
		{ID: "background", Kind: "uses", Uses: "./.github/actions/cancel", Background: true},
		{ID: "await-start", Kind: "run", Command: `while [ ! -f "$READY" ]; do sleep 0.01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	})
	job.Env = map[string]string{"READY": ready}

	result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || !strings.Contains(logs.String(), "post-after-explicit-cancel") {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
}

func TestFileCommandParsing(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     map[string]string
		wantErr  string
	}{
		{name: "LF", contents: "single=value\nmulti<<END\nfirst\nsecond\nEND\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "CRLF", contents: "single=value\r\nmulti<<END\r\nfirst\r\nsecond\r\nEND\r\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "equals before heredoc", contents: "single=value<<literal\n", want: map[string]string{"single": "value<<literal"}},
		{name: "heredoc before equals", contents: "multi<<END=value\npayload\nEND=value\n", want: map[string]string{"multi": "payload"}},
		{name: "missing name", contents: "=value\n", wantErr: "invalid file command"},
		{name: "missing delimiter", contents: "multi<<\n", wantErr: "invalid multiline file command"},
		{name: "unterminated LF", contents: "multi<<END\nunterminated\n", wantErr: `missing delimiter "END"`},
		{name: "unterminated CRLF", contents: "multi<<END\r\nunterminated\r\n", wantErr: `missing delimiter "END"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCommandReader("commands", strings.NewReader(test.contents))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseCommandReader() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCommandReader() error = %v", err)
			}
			if !maps.Equal(got, test.want) {
				t.Fatalf("parseCommandReader() = %#v, want %#v", got, test.want)
			}
		})
	}

	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.cleanup() }()
	if err := os.WriteFile(files.env, []byte("GITHUB_TOKEN=action-token\nRUNNER_CUSTOM=action-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := newResult()
	if _, err := files.apply(&result, nil); err != nil {
		t.Fatalf("commandFiles.apply() GitHub-compatible environment error = %v", err)
	}
	if !maps.Equal(result.Env, map[string]string{"GITHUB_TOKEN": "action-token", "RUNNER_CUSTOM": "action-value"}) {
		t.Fatalf("commandFiles.apply() environment = %#v", result.Env)
	}
	if err := os.WriteFile(files.env, []byte("NODE_OPTIONS=--require bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "NODE_OPTIONS") {
		t.Fatalf("commandFiles.apply() error = %v, want NODE_OPTIONS rejection", err)
	}
}

func TestFileCommandLineLimitIsExplicit(t *testing.T) {
	if values, err := parseCommandReader("output", strings.NewReader("value="+strings.Repeat("x", 70*1024)+"\n")); err != nil || len(values["value"]) != 70*1024 {
		t.Fatalf("parseCommandReader() value length = %d, error = %v", len(values["value"]), err)
	}
	if _, err := parseCommandReader("output", strings.NewReader("value="+strings.Repeat("x", maxStreamLineBytes)+"\n")); err == nil || !strings.Contains(err.Error(), "parse file command output") {
		t.Fatalf("parseCommandReader() error = %v, want attributed size failure", err)
	}
}

func TestDockerCommandFilesAreWritableWithoutExposingDirectoryEntries(t *testing.T) {
	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.cleanup() }()
	if err := files.allowContainerWrites(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(files.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dir.Mode().Perm(); got != 0o711 {
		t.Fatalf("container file-command directory mode = %o, want 711", got)
	}
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		file, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := file.Mode().Perm(); got != 0o666 {
			t.Fatalf("container file command %s mode = %o, want 666", filepath.Base(path), got)
		}
	}
}

func TestFileCommandAggregateLimits(t *testing.T) {
	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.cleanup() }()

	many := strings.Repeat("value=x\n", maxCommandEntries+1)
	if err := os.WriteFile(files.output, []byte(many), 0o600); err != nil {
		t.Fatal(err)
	}
	result := newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("apply() error = %v, want entry limit", err)
	}

	if err := os.WriteFile(files.output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.summary, bytes.Repeat([]byte("x"), maxCommandFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	effects, err := files.apply(&result, nil)
	if err != nil || result.Summary != "" || effects.summaryBytes != maxCommandFileBytes+1 {
		t.Fatalf("oversized summary result = %#v, effects = %#v, error = %v", result, effects, err)
	}

	if err := os.WriteFile(files.summary, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.output, bytes.Repeat([]byte("x"), maxCommandFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("apply() error = %v, want output size limit", err)
	}

	if err := os.WriteFile(files.output, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{files.output, files.env, files.state} {
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 700*1024), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(files.summary, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("apply() error = %v, want aggregate size limit", err)
	}
}

func TestJobSummaryTruncationPreservesUTF8(t *testing.T) {
	var summary string
	var truncated bool
	prefixBytes := maxJobSummaryBytes - 1
	appendJobSummary(&summary, &truncated, strings.Repeat("a", prefixBytes), false)
	appendJobSummary(&summary, &truncated, "€ and omitted text", false)
	summary = finalizeJobSummary(summary, truncated)

	if !truncated || len(summary) > maxJobSummaryBytes || !utf8.ValidString(summary) {
		t.Fatalf("summary truncated = %v, bytes = %d, valid UTF-8 = %v", truncated, len(summary), utf8.ValidString(summary))
	}
	if strings.Contains(summary, "€") || !strings.HasSuffix(summary, jobSummaryTruncationNotice) {
		t.Fatalf("summary split a UTF-8 rune or omitted its truncation notice")
	}
}

func TestJobSummaryRemainsBoundedAfterSecretScrubbing(t *testing.T) {
	secret := "xx"
	result := JobResult{Summary: strings.Repeat(secret, maxJobSummaryBytes/len(secret))}
	result = scrubJobResult(result, []string{secret})

	if len(result.Summary) > maxJobSummaryBytes || !utf8.ValidString(result.Summary) {
		t.Fatalf("scrubbed summary bytes = %d, valid UTF-8 = %v", len(result.Summary), utf8.ValidString(result.Summary))
	}
	if strings.Contains(result.Summary, secret) || !strings.HasSuffix(result.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("scrubbed summary leaked the secret or omitted its truncation notice")
	}
}

func TestTruncatedJobSummaryDoesNotExposePartialSecret(t *testing.T) {
	secret := "partial-secret-token"
	prefixBytes := maxJobSummaryBytes - len(jobSummaryTruncationNotice)
	visibleSecretBytes := len(secret) / 2
	contents := strings.Repeat("a", prefixBytes-visibleSecretBytes) + secret + strings.Repeat("z", len(jobSummaryTruncationNotice))
	result := JobResult{}
	appendJobSummary(&result.Summary, &result.summaryTruncated, contents, false)

	result = scrubJobResult(result, []string{secret})
	if strings.Contains(result.Summary, secret[:visibleSecretBytes]) || !strings.HasSuffix(result.Summary, jobSummaryTruncationNotice) {
		t.Fatalf("truncated summary retained a partial secret suffix")
	}
}

func TestJobSummarySecretScrubbingIsOrderIndependent(t *testing.T) {
	first := scrubJobResult(JobResult{Summary: "overlapping-secret"}, []string{"overlapping", "overlapping-secret"})
	second := scrubJobResult(JobResult{Summary: "overlapping-secret"}, []string{"overlapping-secret", "overlapping"})
	if first.Summary != "***" || second.Summary != first.Summary {
		t.Fatalf("scrubbed summaries = %q and %q, want deterministic longest match", first.Summary, second.Summary)
	}
}

func TestLiveLogMaskingPrefersLongestMatchInEitherRegistrationOrder(t *testing.T) {
	for _, masks := range [][]string{
		{"credential", "credential-with-suffix\nsecond-line"},
		{"credential-with-suffix\nsecond-line", "credential"},
	} {
		var logs bytes.Buffer
		processor := newCommandProcessor(&logs, &logs)
		for _, mask := range masks {
			processor.addMask(mask)
		}
		processor.addMask("credential-with-suffix")

		processor.writeLiteral(&logs, "credential-with-suffix second-line")
		if got := logs.String(); got != "*** ***\n" {
			t.Fatalf("masks %q produced logs %q, want longest overlapping masks without fragments", masks, got)
		}
	}
}

func TestExpressionEvaluationIsSinglePass(t *testing.T) {
	literal := "literal ${{ matrix.secret }} and ${{"
	values, err := evaluateMap(map[string]string{
		"value": "before ${{ matrix.value }} after",
	}, expression.Context{Matrix: map[string]any{
		"value":  literal,
		"secret": "reevaluated",
	}})
	if err != nil || values["value"] != "before "+literal+" after" {
		t.Fatalf("evaluateMap() = %#v, %v, want single-pass substitution", values, err)
	}
}

func TestRunStreamingDrainsOversizedLineAndPreservesMasking(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = (Runner{}).runStreaming(ctx, processor, "", map[string]string{"GO_WANT_RUNTIME_LONG_LINE": "1"}, executable, "-test.run=^TestLongLineChildProcess$")
	if err == nil || !strings.Contains(err.Error(), "stdout stream: line exceeds 1048576-byte limit and was discarded") {
		t.Fatalf("runStreaming() error = %v, want oversized-line diagnostic", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStreaming() deadlocked: %v", err)
	}
	if strings.Contains(logs.String(), "runtime-stream-secret") {
		t.Fatalf("runStreaming() leaked masked content: %q", logs.String())
	}
	if strings.Contains(logs.String(), "after long line") {
		t.Fatalf("runStreaming() forwarded output after masking became uncertain: %q", logs.String())
	}
}

func TestStreamLineLimitIncludesContentNotNewline(t *testing.T) {
	want := strings.Repeat("x", maxStreamLineBytes)
	for _, ending := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("ending-%q", ending), func(t *testing.T) {
			var lines []string
			suppressed := false
			err := streamLines(strings.NewReader(want+ending+"next"+ending), func(line string) {
				lines = append(lines, line)
			}, func() {
				suppressed = true
			})
			if err != nil || suppressed {
				t.Fatalf("streamLines() = %v, suppressed = %v", err, suppressed)
			}
			if len(lines) != 2 || lines[0] != want || lines[1] != "next" {
				t.Fatalf("streamLines() returned %#v", lines)
			}
		})
	}
}

func TestLongLineChildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIME_LONG_LINE") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "::add-mask::runtime-stream-secret")
	_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", maxStreamLineBytes+1)+"runtime-stream-secret")
	_, _ = fmt.Fprintln(os.Stdout, "after long line: runtime-stream-secret")
}

func TestProcessEnvironmentIsExplicitAndUsable(t *testing.T) {
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "must-not-leak")
	for _, name := range agentProxyEnvironmentNames {
		t.Setenv(name, "http://must-not-leak.invalid")
	}
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	command := `
test -n "$PATH" && test -n "$HOME" && test -n "$TMPDIR"
test "$DECLARED" = visible
test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}"
test -z "${HTTP_PROXY+x}" && test -z "${HTTPS_PROXY+x}" && test -z "${ALL_PROXY+x}" && test -z "${NO_PROXY+x}"
test -z "${http_proxy+x}" && test -z "${https_proxy+x}" && test -z "${all_proxy+x}" && test -z "${no_proxy+x}"
printf '%s\n' environment-ok
`
	if err := (Runner{}).runStreaming(t.Context(), processor, "", map[string]string{"DECLARED": "visible"}, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}
	if logs.String() != "environment-ok\n" {
		t.Fatalf("runStreaming() logs = %q", logs.String())
	}
	for _, entry := range processEnv(nil) {
		if strings.HasPrefix(entry, "BUILDKITE_") {
			t.Fatalf("processEnv() inherited agent variable %q", entry)
		}
	}
}

func TestRunStreamingRejectsInvalidEnvironmentNamesBeforeExecution(t *testing.T) {
	for _, name := range []string{"", "ALIAS=OTHER", "NUL\x00NAME"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			err := (Runner{}).runStreaming(t.Context(), newCommandProcessor(io.Discard, io.Discard), "", map[string]string{name: "value"}, "sh", "-c", `touch "$1"`, "sh", marker)
			if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
				t.Fatalf("runStreaming() error = %v, want invalid environment name", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("host process ran with invalid environment name: %v", statErr)
			}
		})
	}

	if err := (Runner{}).runStreaming(t.Context(), newCommandProcessor(io.Discard, io.Discard), "", map[string]string{"GITHUB-ACTIONS_NAME.1": "valid"}, "true"); err != nil {
		t.Fatalf("valid GitHub Actions environment name was rejected: %v", err)
	}
}

func TestRunDockerRejectsInvalidEnvironmentNamesBeforeDocker(t *testing.T) {
	for _, name := range []string{"", "GITHUB_SHA=ALIAS", "NUL\x00NAME"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "docker-ran")
			docker := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(docker, []byte("#!/bin/sh\ntouch "+shellTestQuote(marker)+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			action := fakeDockerAction(t)
			action.runnerTemp = t.TempDir()
			action.Env = map[string]string{name: "value"}
			_, err := newJobRun(Runner{Docker: docker}).runDocker(t.Context(), newCommandProcessor(io.Discard, io.Discard), action)
			if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
				t.Fatalf("runDocker() error = %v, want invalid environment name", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Docker was invoked with invalid environment name: %v", statErr)
			}
		})
	}
}

func TestWorkflowCommandParsingIsCaseInsensitiveAndExact(t *testing.T) {
	mask, ok := parseWorkflowCommand(" \t::ADD-MASK::secret%250Avalue")
	if !ok || !strings.EqualFold(mask.name, "add-mask") || mask.message != "secret%0Avalue" {
		t.Fatalf("parseWorkflowCommand() = %#v, %v", mask, ok)
	}
	extra, ok := parseWorkflowCommand("::add-mask-extra::secret")
	if !ok || strings.EqualFold(extra.name, "add-mask") {
		t.Fatalf("parseWorkflowCommand() accepted %q as add-mask", extra.name)
	}
	command, ok := parseWorkflowCommand("::WaRnInG title=Deploy%3A prod,file=src%2Cmain.go,line=12,endLine=12,col=3,endColumn=5,broken,unknown=value::first%0Asecond%250A")
	if !ok || !strings.EqualFold(command.name, "warning") || command.message != "first\nsecond%0A" {
		t.Fatalf("parseWorkflowCommand() = %#v, %v", command, ok)
	}
	wantProperties := map[string]string{
		"title": "Deploy: prod", "file": "src,main.go", "line": "12", "endline": "12", "col": "3", "endcolumn": "5", "unknown": "value",
	}
	if !maps.Equal(command.properties, wantProperties) {
		t.Fatalf("properties = %#v, want %#v", command.properties, wantProperties)
	}
	for _, malformed := range []string{"", "prefix::warning::message", "::warning without a delimiter", "::::message"} {
		if _, ok := parseWorkflowCommand(malformed); ok {
			t.Fatalf("parseWorkflowCommand(%q) succeeded", malformed)
		}
	}
}

func TestWorkflowCommandsProduceBoundedMaskedJobAnnotations(t *testing.T) {
	var stdout, stderr bytes.Buffer
	processor := newCommandProcessor(&stdout, &stderr)
	_ = processor.process(&stdout, "::warning title=Unsafe <title>,file=cmd%2Cmain.go,line=12,endLine=12,col=3,endColumn=5::late-secret <late-secret-tag> <warning>")
	_ = processor.process(&stdout, "::add-mask::late-secret")
	_ = processor.process(&stdout, "::add-mask::<late-secret-tag>")
	_ = processor.process(&stderr, "::add-mask::early-secret")
	_ = processor.process(&stderr, "::error file=main.go,line=invalid,endLine=9,col=2,endColumn=4::early-secret <error>")
	_ = processor.process(&stdout, "::warning without a delimiter")

	warnings, warningsTruncated, commandErrors, errorsTruncated := processor.workflowCommandAnnotations()
	result := scrubJobResult(JobResult{
		WarningAnnotations: warnings, warningsTruncated: warningsTruncated,
		ErrorAnnotations: commandErrors, errorsTruncated: errorsTruncated,
	}, processor.maskValues())
	if warningsTruncated || errorsTruncated {
		t.Fatal("small workflow command annotations were truncated")
	}
	for _, secret := range []string{"late-secret", "late-secret-tag", "early-secret"} {
		if strings.Contains(result.WarningAnnotations, secret) || strings.Contains(result.ErrorAnnotations, secret) {
			t.Fatalf("annotations leaked %q: warnings = %q, errors = %q", secret, result.WarningAnnotations, result.ErrorAnnotations)
		}
	}
	for _, fragment := range []string{
		"<h2 class=\"h4 mb2\">GitHub Actions warnings</h2>\n<div class=\"mb2\">", `<div class="border-top border-gray py2"><div><strong>Unsafe &lt;title&gt;:</strong> *** *** &lt;warning&gt;</div>`, `<div class="mt1"><code>cmd,main.go:12:3–12:5</code></div>`,
	} {
		if !strings.Contains(result.WarningAnnotations, fragment) {
			t.Errorf("warning annotation lacks %q: %q", fragment, result.WarningAnnotations)
		}
	}
	for _, fragment := range []string{
		"<h2 class=\"h4 mb2\">GitHub Actions errors</h2>\n<div class=\"mb2\">", `<div class="mt1"><code>main.go:9:2–9:4</code></div>`, "*** &lt;error&gt;",
	} {
		if !strings.Contains(result.ErrorAnnotations, fragment) {
			t.Errorf("error annotation lacks %q: %q", fragment, result.ErrorAnnotations)
		}
	}
	if strings.Contains(stdout.String(), "::warning title=") || !strings.Contains(stdout.String(), "warning: late-secret <late-secret-tag> <warning>") || !strings.Contains(stdout.String(), "::warning without a delimiter") {
		t.Fatalf("stdout = %q, want rendered command message and ordinary malformed command", stdout.String())
	}
	if strings.Contains(stderr.String(), "::error") || !strings.Contains(stderr.String(), "error: *** <error>") {
		t.Fatalf("stderr = %q, want masked rendered error", stderr.String())
	}
}

func TestWorkflowCommandAnnotationsGroupRowsByFile(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	for _, command := range []string{
		"::warning file=path/to/first.go,line=2,title=First::first message",
		"::warning file=second.go,line=7,col=3::second message",
		"::warning file=path/to/first.go,line=9::another first message",
		"::warning title=General::general message",
	} {
		_ = processor.process(io.Discard, command)
	}

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated {
		t.Fatal("small grouped annotation was truncated")
	}
	if first, second := strings.LastIndex(warnings, "first.go"), strings.Index(warnings, "second.go"); first < 0 || second < first || strings.Count(warnings, `class="border-top border-gray py2"`) != 4 {
		t.Fatalf("annotation did not retain row order within first-seen file groups: %q", warnings)
	}
	for _, item := range []string{
		"<div><strong>First:</strong> first message</div><div class=\"mt1\"><code>first.go:2</code></div>",
		"<div>another first message</div><div class=\"mt1\"><code>first.go:9</code></div>",
		"<div>second message</div><div class=\"mt1\"><code>second.go:7:3</code></div>",
		"<div><strong>General:</strong> general message</div><div class=\"mt1\">General</div>",
	} {
		if !strings.Contains(warnings, item) {
			t.Errorf("annotation lacks item %q: %q", item, warnings)
		}
	}
}

func TestWorkflowCommandAnnotationRetainsOnlyOwnedRenderedFields(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	properties := map[string]string{
		"file": "main.go", "title": "Lint", "line": "7", "unknown": strings.Repeat("unused", 100_000),
	}
	processor.mu.Lock()
	processor.appendWorkflowCommandLocked(&processor.warnings, workflowWarningAnnotationHeading, parsedWorkflowCommand{properties: properties, message: "message"})
	processor.mu.Unlock()
	properties["file"] = "changed.go"
	properties["title"] = "Changed"

	if len(processor.warnings.commands) != 1 {
		t.Fatalf("retained commands = %d, want 1", len(processor.warnings.commands))
	}
	got := processor.warnings.commands[0]
	if got.file != "main.go" || got.title != "Lint" || got.location != "7" || got.message != "message" {
		t.Fatalf("retained annotation = %#v", got)
	}
	if processor.warnings.rendered >= len(properties["unknown"]) {
		t.Fatalf("rendered size %d retained unknown property bytes", processor.warnings.rendered)
	}
}

func TestWorkflowCommandLocationLabels(t *testing.T) {
	for _, test := range []struct {
		name       string
		properties map[string]string
		want       string
	}{
		{name: "line", properties: map[string]string{"line": "5"}, want: "5"},
		{name: "point", properties: map[string]string{"line": "5", "col": "3"}, want: "5:3"},
		{name: "same-line range", properties: map[string]string{"line": "5", "col": "3", "endcolumn": "8"}, want: "5:3–5:8"},
		{name: "explicit same point", properties: map[string]string{"line": "5", "endline": "5", "col": "3"}, want: "5:3"},
		{name: "multiline range", properties: map[string]string{"line": "5", "endline": "6", "col": "3", "endcolumn": "8"}, want: "5–6"},
		{name: "end line supplies start", properties: map[string]string{"endline": "5"}, want: "5"},
		{name: "reversed range", properties: map[string]string{"line": "5", "endline": "4"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := workflowCommandLocationLabel(test.properties); got != test.want {
				t.Fatalf("workflowCommandLocationLabel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWorkflowPresentationCommandsUseBuildkiteSections(t *testing.T) {
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	for _, line := range []string{
		"::add-mask::---",
		"::add-mask::+++",
		"::add-mask::secret",
		"::add-mask::foo\tbar",
		"::group::Compile secret%0A--- injected foo\tbar",
		"inside group",
		"::endgroup::",
		"::debug::modern debug",
		"::add-matcher::matcher.json",
		"::remove-matcher owner=test::",
		"##[debug]legacy debug",
		"##[add-matcher]legacy.json",
		"##[remove-matcher]test",
	} {
		if err := processor.process(&logs, line); err != nil {
			t.Fatalf("process(%q) error = %v", line, err)
		}
	}
	processor.expandCurrentSection()

	if got, want := logs.String(), "--- Compile *** *** injected ***\ninside group\n^^^ +++\n"; got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}
}

func TestRunJobLogsSynchronousStepSectionsAndExpandsFailures(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: step sections\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "success", Name: "Build (${{ matrix.version }})\n+++ injected", Kind: "run", Shell: "sh", Command: "echo built"},
		{ID: "skipped", Name: "Must not appear", Kind: "run", Shell: "sh", Condition: "false", Command: "echo skipped"},
		{ID: "failure", Kind: "run", Shell: "sh", Command: "echo broken; exit 1"},
	})
	job.Matrix = map[string]any{"version": "1.26"}
	var logs bytes.Buffer

	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	got := logs.String()
	for _, fragment := range []string{"--- Build (1.26) +++ injected\n", "built\n", "--- failure\n", "broken\n", "^^^ +++\n"} {
		if !strings.Contains(got, fragment) {
			t.Errorf("logs lack %q: %q", fragment, got)
		}
	}
	if strings.Contains(got, "Must not appear") || strings.Index(got, "^^^ +++") < strings.Index(got, "broken") {
		t.Fatalf("logs contain a skipped heading or expand before failure output: %q", got)
	}
}

func TestWorkflowCommandStopTokenPreventsAccidentalAnnotations(t *testing.T) {
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	_ = processor.process(&logs, "::stop-commands::workflow-stop-token")
	_ = processor.process(&logs, "::warning::untrusted warning-shaped output")
	_ = processor.process(&logs, "::workflow-stop-token::")
	_ = processor.process(&logs, "::warning::collected warning")

	warnings, truncated, commandErrors, _ := processor.workflowCommandAnnotations()
	if truncated || commandErrors != "" || strings.Contains(warnings, "untrusted warning-shaped output") || !strings.Contains(warnings, "collected warning") {
		t.Fatalf("workflow command annotations = %q, errors = %q, truncated = %v", warnings, commandErrors, truncated)
	}
	if !strings.Contains(logs.String(), "::warning::untrusted warning-shaped output") || strings.Contains(logs.String(), "::warning::collected warning") || strings.Contains(logs.String(), "workflow-stop-token") {
		t.Fatalf("logs = %q, want stopped command as masked ordinary output", logs.String())
	}
}

func TestWorkflowCommandStopTokenHandlesCRLFStreams(t *testing.T) {
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	command := `printf '::stop-commands::crlf-stop-token\r\n'
printf '::warning::untrusted warning-shaped output\r\n'
printf '::crlf-stop-token::\r\n'
printf '::add-mask::crlf-secret\r\n'
printf 'masked after resume: crlf-secret\r\n'`
	if err := (Runner{}).runStreaming(t.Context(), processor, "", nil, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}
	warnings, _, commandErrors, _ := processor.workflowCommandAnnotations()
	if warnings != "" || commandErrors != "" {
		t.Fatalf("workflow command annotations = %q, errors = %q", warnings, commandErrors)
	}
	if !strings.Contains(logs.String(), "::warning::untrusted warning-shaped output") || !strings.Contains(logs.String(), "masked after resume: ***") || strings.Contains(logs.String(), "crlf-secret") || strings.Contains(logs.String(), "\r") {
		t.Fatalf("logs = %q, want resumed masking with normalized CRLF records", logs.String())
	}
	commandWithEscapedCR, ok := parseWorkflowCommand("::warning::preserved%0D")
	if !ok || commandWithEscapedCR.message != "preserved\r" {
		t.Fatalf("parseWorkflowCommand() = %#v, %v, want escaped carriage return", commandWithEscapedCR, ok)
	}
}

func TestRunStreamingScopesWorkflowCommandFailuresToInvocation(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	ready := filepath.Join(t.TempDir(), "clean-ready")
	release := filepath.Join(t.TempDir(), "release-clean")
	cleanDone := make(chan error, 1)
	go func() {
		cleanDone <- (Runner{}).runStreaming(t.Context(), processor, "", map[string]string{"READY": ready, "RELEASE": release}, "sh", "-c", `
: > "$READY"
while [ ! -e "$RELEASE" ]; do sleep .01; done
printf '%s\n' 'clean invocation completed'`)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("clean invocation did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	invalidErr := (Runner{}).runStreaming(t.Context(), processor, "", map[string]string{"RELEASE": release}, "sh", "-c", `
printf '%s\n' '::stop-commands::warning'
: > "$RELEASE"`)
	cleanErr := <-cleanDone
	if !errors.Is(invalidErr, errInvalidWorkflowCommandStopToken) {
		t.Fatalf("invalid runStreaming() error = %v", invalidErr)
	}
	if cleanErr != nil {
		t.Fatalf("overlapping clean runStreaming() inherited command failure: %v", cleanErr)
	}
}

func TestInvalidWorkflowCommandStopTokenFailsTheStep(t *testing.T) {
	for _, token := range []string{"warning", "add-matcher", "remove-matcher"} {
		t.Run(token, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: invalid workflow command stop token\n")
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
				ID: "invalid-stop", Kind: "run", Shell: "sh",
				Command: fmt.Sprintf("printf '%%s\\n' '::stop-commands::%s'\nprintf '%%s\\n' '::warning::commands remain active'", token),
			}})
			var logs bytes.Buffer

			result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
			if err == nil || !strings.Contains(err.Error(), "invalid ::stop-commands workflow command") || result.Conclusion != "failure" {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			if !strings.Contains(result.ErrorAnnotations, "invalid ::stop-commands token") || !strings.Contains(result.WarningAnnotations, "commands remain active") {
				t.Fatalf("RunJob() warnings = %q, errors = %q", result.WarningAnnotations, result.ErrorAnnotations)
			}
			if !strings.Contains(logs.String(), "error: invalid ::stop-commands token") {
				t.Fatalf("RunJob() logs = %q, want invalid-token diagnostic", logs.String())
			}
		})
	}
}

func TestRunJobCollectsWarningAndErrorCommandsWithoutChangingConclusion(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: workflow command annotations\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "diagnostics", Kind: "run", Shell: "sh",
		Command: "printf '%s\\n' '::warning title=Compiler::warning from stdout'\nprintf '%s\\n' '::error file=main.go,line=7::error from stderr' >&2",
	}})
	var stdout, stderr bytes.Buffer

	result, err := (Runner{Stdout: &stdout, Stderr: &stderr}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if !strings.Contains(result.WarningAnnotations, "warning from stdout") || !strings.Contains(result.ErrorAnnotations, "error from stderr") {
		t.Fatalf("RunJob() warnings = %q, errors = %q", result.WarningAnnotations, result.ErrorAnnotations)
	}
	if strings.Contains(stdout.String(), "::warning") || strings.Contains(stderr.String(), "::error") {
		t.Fatalf("RunJob() echoed raw workflow command: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestWorkflowCommandAnnotationsAreConcurrentAndUTF8Bounded(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	var group sync.WaitGroup
	for worker := range 2 {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for message := range 50 {
				_ = processor.process(io.Discard, fmt.Sprintf("::warning::worker-%d-message-%d", worker, message))
			}
		}(worker)
	}
	group.Wait()
	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated || strings.Count(warnings, `class="border-top border-gray py2"`) != 100 {
		t.Fatalf("concurrent warning annotation count = %d, truncated = %v", strings.Count(warnings, `class="border-top border-gray py2"`), truncated)
	}

	processor = newCommandProcessor(io.Discard, io.Discard)
	_ = processor.process(io.Discard, "::warning::"+strings.Repeat("€", maxJobAnnotationBytes))
	warnings, truncated, _, _ = processor.workflowCommandAnnotations()
	result := scrubJobResult(JobResult{WarningAnnotations: warnings, warningsTruncated: truncated}, nil)
	if !truncated || len(result.WarningAnnotations) > maxJobAnnotationBytes || !utf8.ValidString(result.WarningAnnotations) || !strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice) {
		t.Fatalf("bounded warnings bytes = %d, truncated = %v, valid UTF-8 = %v", len(result.WarningAnnotations), truncated, utf8.ValidString(result.WarningAnnotations))
	}
}

func TestWorkflowCommandAnnotationsNormalizeInvalidUTF8(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	command := `printf '::warning title=bad\377,file=bad\376.go::bad\375\n'`
	if err := (Runner{}).runStreaming(t.Context(), processor, "", nil, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated || !utf8.ValidString(warnings) || strings.Count(warnings, "\uFFFD") != 3 {
		t.Fatalf("warning annotation = %q, truncated = %v, valid UTF-8 = %v", warnings, truncated, utf8.ValidString(warnings))
	}
	for _, fragment := range []string{`<div class="mt1"><code>bad�.go</code></div>`, "<div><strong>bad\uFFFD:</strong> bad\uFFFD</div>"} {
		if !strings.Contains(warnings, fragment) {
			t.Fatalf("warning annotation lacks %q: %q", fragment, warnings)
		}
	}
}

func TestWorkflowCommandAnnotationScrubbingPreservesUTF8(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	_ = processor.process(io.Discard, "::warning::café")
	_ = processor.process(io.Discard, "::warning::masked "+string([]byte{0xC3}))
	_ = processor.process(io.Discard, "::add-mask::"+string([]byte{0xC3}))

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	result := scrubJobResult(JobResult{WarningAnnotations: warnings, warningsTruncated: truncated}, processor.maskValues())
	if !utf8.ValidString(result.WarningAnnotations) || !strings.Contains(result.WarningAnnotations, "café") || !strings.Contains(result.WarningAnnotations, "masked ***") || strings.Contains(result.WarningAnnotations, "\uFFFD") {
		t.Fatalf("scrubbed warning annotation = %q, valid UTF-8 = %v", result.WarningAnnotations, utf8.ValidString(result.WarningAnnotations))
	}
}

func TestWorkflowCommandMasksCannotCorruptAnnotationMarkup(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	_ = processor.process(io.Discard, "::warning file=table.go,title=tr::structured table text")
	_ = processor.process(io.Discard, "::add-mask::tr")
	_ = processor.process(io.Discard, "::add-mask::table")

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated || !strings.Contains(warnings, `<div class="mt1"><code>***.go</code></div>`) || !strings.Contains(warnings, "<div><strong>***:</strong> s***uctured *** text</div>") {
		t.Fatalf("masked warning annotation = %q, truncated = %v", warnings, truncated)
	}
	if strings.Count(warnings, `class="border-top border-gray py2"`) != 1 || strings.Count(warnings, "<div") != strings.Count(warnings, "</div>") {
		t.Fatalf("masks corrupted annotation markup: %q", warnings)
	}
}

func TestWorkflowCommandAnnotationsRemainBoundedAfterMaskExpansion(t *testing.T) {
	processor := newCommandProcessor(io.Discard, io.Discard)
	for range 5000 {
		_ = processor.process(io.Discard, "::warning file=main.go::"+strings.Repeat("x", 100))
	}
	_ = processor.process(io.Discard, "::add-mask::x")

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if !truncated || strings.Contains(warnings, strings.Repeat("x", 100)) || !strings.HasSuffix(warnings, workflowCommandListEnd) || strings.Count(warnings, `class="border-top border-gray py2"`)*3+1 != strings.Count(warnings, "</div>") {
		t.Fatalf("expanded warning annotation bytes = %d, items = %d, closing divs = %d, truncated = %v", len(warnings), strings.Count(warnings, `class="border-top border-gray py2"`), strings.Count(warnings, "</div>"), truncated)
	}
	result := scrubJobResult(JobResult{WarningAnnotations: warnings, warningsTruncated: truncated}, processor.maskValues())
	if len(result.WarningAnnotations) > maxJobAnnotationBytes || !utf8.ValidString(result.WarningAnnotations) || !strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice) {
		t.Fatalf("final warning annotation bytes = %d, valid UTF-8 = %v", len(result.WarningAnnotations), utf8.ValidString(result.WarningAnnotations))
	}
}

func TestActionMetadataRejectsCaseInsensitiveOutputCollisions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/conflict/action.yml", `name: Conflicting outputs
outputs:
  Result:
    value: first
  result:
    value: second
runs:
  using: composite
  steps: []
`)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "conflict", Kind: "uses", Uses: "./.github/actions/conflict"}})
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `duplicate case-insensitive name "result"`) {
		t.Fatalf("RunJob() error = %v, want duplicate output rejection", err)
	}
}

func TestDiscoverNodeManagedAndWrongExplicitVersion(t *testing.T) {
	managed := t.TempDir()
	node := filepath.Join(managed, "node24", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := discoverNodeContext(t.Context(), 24, "", managed)
	if err != nil || got != node {
		t.Fatalf("discoverNodeContext(24) = %q, %v, want %q, nil", got, err, node)
	}

	wrong := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(wrong, []byte("#!/bin/sh\necho v23.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverNodeContext(t.Context(), 24, wrong, ""); err == nil || !strings.Contains(err.Error(), `reported "v23.1.0"`) {
		t.Fatalf("discoverNodeContext(24) error = %v, want wrong-version detail", err)
	}

	node20 := filepath.Join(managed, "node", "20", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node20), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node20, []byte("#!/bin/sh\necho v20.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := discoverNodeContext(t.Context(), 20, "", managed); err != nil || got != node20 {
		t.Fatalf("discoverNodeContext(20) = %q, %v, want %q, nil", got, err, node20)
	}
	if _, err := discoverNodeContext(t.Context(), 24, node20, ""); err == nil || !strings.Contains(err.Error(), `reported "v20.99.0"`) {
		t.Fatalf("discoverNodeContext(24) error = %v, want exact-major rejection", err)
	}
}

func TestDiscoverNodePreservesDeadline(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_, err := discoverNodeContext(ctx, 24, node, "")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "node 24 discovery") {
		t.Fatalf("discoverNodeContext(24) error = %v, want contextual deadline exceeded", err)
	}

	if err := os.WriteFile(node, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = discoverNodeContext(t.Context(), 24, node, "")
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("discoverNodeContext(24) error = %v, want genuine discovery failure", err)
	}
}

func TestMiseNodeSelectionIsExactAndConfigFree(t *testing.T) {
	root := canonicalTempDir(t)
	log := filepath.Join(root, "args")
	dataDir := filepath.Join(root, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node20Version)
	node := filepath.Join(installation, "bin", "node")
	nodeBytes := []byte("#!/bin/sh\nprintf 'v20.20.2\\n'\n")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nprintf 'mise progress\\n' >&2\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", log, installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nodeBytes)
	r := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, nodeDigests: map[int]string{20: hex.EncodeToString(digest[:])}})
	got, err := r.discoverNode(t.Context(), 20, "")
	if err != nil || got != node {
		t.Fatalf("discoverNode() = %q, %v", got, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--no-config install core:node@20.20.2\n--no-config where core:node@20.20.2\n" {
		t.Fatalf("mise arguments = %q", data)
	}
}

func TestMiseNode16SelectionIsExactAndConfigFree(t *testing.T) {
	root := canonicalTempDir(t)
	log := filepath.Join(root, "args")
	dataDir := filepath.Join(root, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node16Version)
	node := filepath.Join(installation, "bin", "node")
	nodeBytes := []byte("#!/bin/sh\nprintf 'v16.20.2\\n'\n")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", log, installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nodeBytes)
	got, err := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, nodeDigests: map[int]string{16: hex.EncodeToString(digest[:])}}).discoverNode(t.Context(), 16, "")
	if err != nil || got != node {
		t.Fatalf("discoverNode() = %q, %v", got, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--no-config install core:node@16.20.2\n--no-config where core:node@16.20.2\n" {
		t.Fatalf("mise arguments = %q", data)
	}
}

func TestMiseNodeInstallationAllowsSymlinkedDataDirAncestor(t *testing.T) {
	base := canonicalTempDir(t)
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	dataDir := filepath.Join(logicalParent, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	writeNodeExecutable(t, node, 24)
	mise := filepath.Join(base, "mise")
	writeFixtureFile(t, base, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	gotInstallation, gotNode, err := (Runner{MiseDataDir: dataDir}).miseNodeInstallation(t.Context(), 24, mise)
	if err != nil {
		t.Fatal(err)
	}
	wantInstallation := filepath.Join(realParent, "data", "installs", "node", Node24Version)
	if gotInstallation != wantInstallation || gotNode != filepath.Join(wantInstallation, "bin", "node") {
		t.Fatalf("miseNodeInstallation() = %q, %q; want canonical paths under %q", gotInstallation, gotNode, wantInstallation)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeNodeExecutable(t, filepath.Join(outside, "node"), 24)
	if err := os.RemoveAll(filepath.Join(wantInstallation, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wantInstallation, "bin")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Runner{MiseDataDir: dataDir}).miseNodeInstallation(t.Context(), 24, mise); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("miseNodeInstallation() accepted symlinked bin directory: %v", err)
	}
}

func TestMiseNodePathIgnoresProgressOnStderr(t *testing.T) {
	root := canonicalTempDir(t)
	nodeRoot := filepath.Join(root, "node")
	if err := os.MkdirAll(filepath.Join(nodeRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(nodeRoot, "bin", "node")
	writeNodeExecutable(t, node, 24)
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf 'mise progress\\n' >&2\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", nodeRoot))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongBytes, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := sha256.Sum256(wrongBytes)
	if _, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(wrongDigest[:])}}).resolveMiseNodePath(t.Context(), 24); err == nil || !strings.Contains(err.Error(), `reported "v24.99.0", want "v24.18.0"`) {
		t.Fatalf("resolveMiseNodePath() error = %v, want exact executable version rejection", err)
	}
	correctBytes := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n")
	if err := os.WriteFile(node, correctBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	correctDigest := sha256.Sum256(correctBytes)
	got, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(correctDigest[:])}}).resolveMiseNodePath(t.Context(), 24)
	if err != nil || got != filepath.Join(nodeRoot, "bin", "node") {
		t.Fatalf("resolveMiseNodePath() = %q, %v", got, err)
	}
}

func TestManagedMiseCacheReplacesNodeWithWrongDigest(t *testing.T) {
	root := canonicalTempDir(t)
	dataDir := filepath.Join(root, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	poisoned := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n# poisoned\n")
	if err := os.WriteFile(node, poisoned, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n# replacement\n")
	digest := sha256.Sum256(replacement)
	mise := filepath.Join(root, "mise")
	script := fmt.Sprintf(`#!/bin/sh
node=%q
installation=%q
case "$2" in
  install)
    if [ ! -x "$node" ]; then
      mkdir -p "$(dirname "$node")"
      cat > "$node" <<'NODE'
%sNODE
      chmod 0755 "$node"
    fi
    ;;
  where) printf '%%s\n' "$installation" ;;
  *) exit 9 ;;
esac
`, node, installation, replacement)
	if err := os.WriteFile(mise, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	verification := &managedNodeVerification{paths: make(map[int]string)}
	runner := newJobRun(Runner{
		Mise:        mise,
		MiseDataDir: dataDir,
		nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])},
	})
	runner.nodeVerification = verification
	if got, err := runner.discoverNode(t.Context(), 24, ""); err != nil || got != node {
		t.Fatalf("discoverNode() = %q, %v", got, err)
	}
	got, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) || verification.paths[24] != node {
		t.Fatalf("replacement node = %q, verified = %#v", got, verification.paths)
	}
}

func TestManagedMiseCacheRefusesSymlinkedRemoval(t *testing.T) {
	dataDir := canonicalTempDir(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dataDir, "installs")); err != nil {
		t.Fatal(err)
	}
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	if err := removeManagedNodeInstallation(dataDir, installation); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("removeManagedNodeInstallation() error = %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory was affected: %v", err)
	}
}

func TestJavaScriptPhaseUsesVerifiedMiseNodeWithoutWorkflowRedirection(t *testing.T) {
	root := canonicalTempDir(t)
	log := filepath.Join(root, "node-args")
	dataDir := filepath.Join(root, "mise-data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeBytes := []byte(fmt.Sprintf("#!/bin/sh\nif [ \"${1:-}\" = --version ]; then printf 'v24.18.0\\n'; else printf '%%s|MISE_DATA_DIR=%%s\\n' \"$*\" \"${MISE_DATA_DIR-unset}\" >> %q; fi\n", log))
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, root, "main.js", "")
	node20 := filepath.Join(root, "node20")
	writeNodeExecutable(t, node20, 20)
	digest := sha256.Sum256(nodeBytes)
	runner := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, Node20: node20, nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])}})
	resolvedNode, err := runner.discoverNode(t.Context(), 24, "")
	if err != nil {
		t.Fatal(err)
	}
	result := newResult()
	action := javaScriptAction{Name: "mise", Path: root, Main: "main.js", Env: map[string]string{"MISE_DATA_DIR": "/workflow-controlled"}, nodeMajor: 24}
	if err := runner.runJavaScriptPhase(t.Context(), newCommandProcessor(io.Discard, io.Discard), root, resolvedNode, action, action.Main, nil, nil, &result); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "main.js") + "|MISE_DATA_DIR=/workflow-controlled\n"
	if string(data) != want {
		t.Fatalf("Node invocations = %q, want %q", data, want)
	}
}

func TestMiseMissingIsClear(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := newJobRun(Runner{}).discoverNode(t.Context(), 24, "")
	if err == nil || !strings.Contains(err.Error(), "mise is required") {
		t.Fatalf("discoverNode() error = %v", err)
	}
}

func TestMiseNodeSelectionRejectsWrongExactVersion(t *testing.T) {
	root := canonicalTempDir(t)
	installation := filepath.Join(root, "node")
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeBytes := []byte("#!/bin/sh\nprintf 'v24.18.1\\n'\n")
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nodeBytes)
	_, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])}}).discoverNode(t.Context(), 24, "")
	if err == nil || !strings.Contains(err.Error(), `reported "v24.18.1", want "v24.18.0"`) {
		t.Fatalf("discoverNode() error = %v", err)
	}
}

func TestDockerAction(t *testing.T) {
	docker := requireDocker(t)
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs, Docker: docker}
	result, err := runner.runDockerAction(t.Context(), dockerAction{
		Name: "local Docker", Path: fixturePath(t, "actions", "docker"), Workspace: fixturePath(t),
		Env: map[string]string{"INPUT_EXPECTED_FILE": "smoke/.github/workflows/ci.yml"},
	})

	if err != nil {
		t.Fatalf("runDockerAction() error = %v", err)
	}
	if result.Outputs["container"] != "ran" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" {
		t.Errorf("Docker result = %#v", result)
	}
	if result.Summary != "docker action summary\n" {
		t.Errorf("Docker summary = %q", result.Summary)
	}
	if strings.Contains(logs.String(), "docker-secret-value") {
		t.Fatalf("raw forwarded Docker logs contain literal secret: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "masked docker probe: ***") {
		t.Errorf("Docker logs = %q, want masked probe", logs.String())
	}
}

func TestDockerActionSupportsImageDefaultNonRootUser(t *testing.T) {
	docker := requireDocker(t)
	action := t.TempDir()
	writeFixtureFile(t, action, "main.go", `package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	workspace := os.Getenv("GITHUB_WORKSPACE")
	if workspace != "/github/workspace" || os.Getenv("RUNNER_TEMP") != "/github/runner_temp" {
		panic("container paths were not translated")
	}
	if err := os.WriteFile(filepath.Join(workspace, "nonroot-workspace"), []byte("written"), 0o600); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("RUNNER_TEMP"), "nonroot-temp"), []byte("written"), 0o600); err != nil {
		panic(err)
	}
	output, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		panic(err)
	}
	defer output.Close()
	if _, err := fmt.Fprintln(output, "nonroot=written"); err != nil {
		panic(err)
	}
}
`)
	binary := filepath.Join(action, "entrypoint")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, filepath.Join(action, "main.go"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build non-root action entrypoint: %v: %s", err, output)
	}
	writeFixtureFile(t, action, "Dockerfile", "FROM scratch\nCOPY entrypoint /entrypoint\nUSER 65534:65534\nENTRYPOINT [\"/entrypoint\"]\n")
	workspace := t.TempDir()
	before, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Docker: docker}).runDockerAction(t.Context(), dockerAction{Name: "non-root Docker", Path: action, Workspace: workspace})
	if err != nil {
		t.Fatalf("runDockerAction() error = %v", err)
	}
	if result.Outputs["nonroot"] != "written" {
		t.Fatalf("Docker result = %#v", result)
	}
	if info, err := os.Stat(filepath.Join(workspace, "nonroot-workspace")); err != nil || info.Size() != int64(len("written")) {
		t.Fatalf("non-root workspace output = %v, %v", info, err)
	}
	after, err := os.Stat(workspace)
	if err != nil || after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("workspace mode after Docker action = %v, %v; want %v", after, err, before.Mode().Perm())
	}
}

func TestRunJobShellJavaScriptCompositeAndPost(t *testing.T) {
	node := requireNode24(t)
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{
		{ID: "shell", Kind: "run", Shell: "bash", Command: `echo "result=smoke" >> "$GITHUB_OUTPUT"`},
		{ID: "javascript", Name: "JavaScript", Kind: "uses", Uses: "./.github/actions/javascript", With: map[string]string{"message": "${{ steps.shell.outputs.result }}"}},
		{ID: "composite", Name: "Composite", Kind: "uses", Uses: "./.github/actions/composite", With: map[string]string{"message": "${{ steps.javascript.outputs.result }}"}},
		{ID: "verify", Kind: "run", Shell: "bash", Command: `test "$SMOKE_COMPOSITE_SEEN" = true`},
	})
	job.Outputs = map[string]string{"result": "${{ steps.composite.outputs.result }}"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Node24: node}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v\nlogs:\n%s", err, logs.String())
	}
	if result.Outputs["result"] != "smoke-javascript-composite" || result.Env["SMOKE_COMPOSITE_SEEN"] != "true" || result.State["phase"] != "main" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if strings.Contains(logs.String(), "smoke-mask-value") || !strings.Contains(logs.String(), "masked probe: ***") {
		t.Fatalf("RunJob() logs were not masked: %q", logs.String())
	}
	if post, verify := strings.Index(logs.String(), "JavaScript post phase completed"), strings.Index(logs.String(), "masked probe: ***"); post < verify {
		t.Fatalf("post action did not run after main steps: %q", logs.String())
	}
	if !strings.Contains(result.Summary, "main phase") || !strings.Contains(result.Summary, "post phase") {
		t.Fatalf("RunJob() summary = %q", result.Summary)
	}
}

func TestRunJobRejectsDynamicallyMaskedJobOutput(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:    "derive",
		Kind:  "run",
		Shell: "sh",
		Command: `printf '%s\n' '::add-mask::derived-mask-value'
printf '%s\n' 'secret=derived-mask-value' >> "$GITHUB_OUTPUT"
printf '%s\n' 'DYNAMIC_VALUE=derived-mask-value' >> "$GITHUB_ENV"
printf '%s\n' 'derived-mask-value' >> "$GITHUB_STEP_SUMMARY"`,
	}})
	job.Outputs = map[string]string{"secret": "${{ steps.derive.outputs.secret }}"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `job output "secret" contains a registered secret`) {
		t.Fatalf("RunJob() error = %v, want dynamically masked output rejection", err)
	}
	if strings.Contains(logs.String(), "derived-mask-value") || strings.Contains(fmt.Sprintf("%#v", result), "derived-mask-value") {
		t.Fatalf("RunJob() leaked dynamically masked value: result = %#v, logs = %q", result, logs.String())
	}
	if result.Env["DYNAMIC_VALUE"] != "***" || result.Summary != "***\n" {
		t.Fatalf("RunJob() did not scrub dynamic mask from bounded result: %#v", result)
	}
}

func TestCompositeExposesOnlyDeclaredOutputs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	actionPath := ".github/actions/composite/action.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, actionPath, `name: Output-scoped composite
outputs:
  public:
    value: ${{ steps.inner.outputs.public }}
runs:
  using: composite
  steps:
    - id: inner
      shell: sh
      run: |
        printf '%s\n' 'public=visible' >> "$GITHUB_OUTPUT"
        printf '%s\n' 'private=hidden' >> "$GITHUB_OUTPUT"
        printf '%s\n' 'COMPOSITE_ENV=propagated' >> "$GITHUB_ENV"
        printf '%s\n' 'composite summary' >> "$GITHUB_STEP_SUMMARY"
`)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	job.Outputs = map[string]string{
		"private": "${{ steps.composite.outputs.private }}",
		"public":  "${{ steps.composite.outputs.public }}",
	}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Outputs["public"] != "visible" || result.Outputs["private"] != "" {
		t.Fatalf("RunJob() outputs = %#v, want only declared composite output", result.Outputs)
	}
	if result.Env["COMPOSITE_ENV"] != "propagated" || result.Summary != "composite summary\n" {
		t.Fatalf("RunJob() effects = %#v, summary = %q", result.Env, result.Summary)
	}
}

func TestRunJobPythonShellUsesTemporaryScript(t *testing.T) {
	installPythonShellTestCommand(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:    "python",
		Kind:  "run",
		Shell: "python",
		Command: `import os
from pathlib import Path

assert Path.cwd() == Path(os.environ["GITHUB_WORKSPACE"])
assert Path(__file__).suffix == ".py"
with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
    output.write(f"script={__file__}\n")
`,
	}})
	job.Outputs = map[string]string{"script": "${{ steps.python.outputs.script }}"}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	script := result.Outputs["script"]
	if !strings.HasSuffix(script, ".py") {
		t.Fatalf("Python script path = %q, want .py suffix", script)
	}
	if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary Python script remains at %q: %v", script, err)
	}
}

func TestCompositePythonShell(t *testing.T) {
	installPythonShellTestCommand(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Python composite
outputs:
  value:
    value: ${{ steps.python.outputs.value }}
runs:
  using: composite
  steps:
    - id: python
      shell: python
      run: |
        import os
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            output.write("value=python-composite\n")
`)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	job.Outputs = map[string]string{"value": "${{ steps.composite.outputs.value }}"}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Outputs["value"] != "python-composite" {
		t.Fatalf("RunJob() output = %q, want python-composite", result.Outputs["value"])
	}
}

func installPythonShellTestCommand(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	python := filepath.Join(dir, "python")
	wrapper := "#!/bin/sh\nBUILDKITE_GHA_TEST_PYTHON_HELPER=1 exec " + shellTestQuote(executable) + " -test.run '^TestPythonShellCommandHelper$' -- \"$@\"\n"
	if err := os.WriteFile(python, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPythonShellCommandHelper(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_TEST_PYTHON_HELPER") == "" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatalf("Python helper arguments = %#v", os.Args)
	}
	script := os.Args[separator+1]
	source, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(script) != ".py" {
		t.Fatalf("Python script = %q, want .py suffix", script)
	}
	if cwd, err := os.Getwd(); err != nil || cwd != os.Getenv("GITHUB_WORKSPACE") {
		t.Fatalf("working directory = %q, %v; workspace = %q", cwd, err, os.Getenv("GITHUB_WORKSPACE"))
	}
	var output string
	switch {
	case bytes.Contains(source, []byte(`script={__file__}`)):
		output = "script=" + script + "\n"
	case bytes.Contains(source, []byte(`value=python-composite`)):
		output = "value=python-composite\n"
	default:
		t.Fatalf("unexpected Python source: %s", source)
	}
	file, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(output); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompositeStepWorkingDirectoryEvaluatesExpressions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Working-directory composite
inputs:
  dir:
    required: false
runs:
  using: composite
  steps:
    - shell: bash
      working-directory: ${{ inputs.dir || '.' }}
      run: test "$(basename "$PWD")" = sub
    - shell: bash
      working-directory: literal-dir
      run: test "$(basename "$PWD")" = literal-dir
`)
	for _, directory := range []string{"sub", "literal-dir"} {
		if err := os.Mkdir(filepath.Join(workspace, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite", With: map[string]string{"dir": "sub"}}})
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunStepMissingWorkingDirectoryFailsClearly(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "run", Kind: "run", Shell: "sh", Command: "true", WorkingDirectory: "missing"}})
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `working directory "missing" does not exist`) {
		t.Fatalf("RunJob() error = %v, want missing working directory diagnostic", err)
	}
}

func TestNestedCompositeEvaluatesEnvironmentOnceAndIsolatesStepScopes(t *testing.T) {
	workspace := canonicalTempDir(t)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/js/action.yml", `name: Nested JavaScript
inputs:
  message:
    required: true
runs:
  using: node20
  main: main.js
`)
	writeFixtureFile(t, workspace, ".github/actions/js/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/inner/action.yml", `name: Inner
outputs:
  result:
    value: ${{ steps.same.outputs.result }}
runs:
  using: composite
  steps:
    - id: verify-path
      shell: sh
      run: test "$GITHUB_ACTION_PATH" = "$EXPECTED_INNER_PATH"
    - id: same
      uses: ./.github/actions/js
      with:
        message: nested-input
      env:
        VALUE: ${{ env.TEMPLATE }}
        DYNAMIC_COPY: ${{ env.DYNAMIC }}
        CHILD_ONLY: private
`)
	writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", `name: Outer
inputs:
  parent-only:
    required: true
outputs:
  result:
    value: ${{ steps.nested.outputs.result }}
runs:
  using: composite
  steps:
    - id: same
      shell: sh
      run: |
        printf '%s\n' 'DYNAMIC=from-env-file' >> "$GITHUB_ENV"
        printf 'TEMPLATE=$%s\n' '{{ inputs.not_evaluated }}' >> "$GITHUB_ENV"
    - id: nested
      uses: ./.github/actions/inner
    - id: verify
      shell: sh
      run: test -z "${CHILD_ONLY:-}"
`)
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
printf 'input=%s value=%s dynamic=%s path=%s expected=%s\n' "$INPUT_MESSAGE" "$VALUE" "$DYNAMIC_COPY" "$GITHUB_ACTION_PATH" "$EXPECTED_ACTION_PATH" >&2
test "$INPUT_MESSAGE" = nested-input
test -z "${INPUT_PARENT_ONLY:-}"
test "$VALUE" = '${{ inputs.not_evaluated }}'
test "$DYNAMIC_COPY" = from-env-file
test "$GITHUB_ACTION_PATH" = "$EXPECTED_ACTION_PATH"
printf '%s\n' 'result=nested-ok' >> "$GITHUB_OUTPUT"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "outer", Kind: "uses", Uses: "./.github/actions/outer", With: map[string]string{"parent-only": "private-to-parent"}}})
	job.Env = map[string]string{
		"EXPECTED_ACTION_PATH": filepath.Join(workspace, ".github", "actions", "js"),
		"EXPECTED_INNER_PATH":  filepath.Join(workspace, ".github", "actions", "inner"),
	}
	job.Outputs = map[string]string{"result": "${{ steps.outer.outputs.result }}"}
	var logs bytes.Buffer
	result, err := (Runner{Node24: fakeNode, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v\nlogs: %s", err, logs.String())
	}
	if result.Outputs["result"] != "nested-ok" {
		t.Fatalf("result = %#v, want nested composite output chain", result.Outputs)
	}
}

func TestCompositeGitHubActionPathExpressionIsInvocationScoped(t *testing.T) {
	workspace := canonicalTempDir(t)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/inner/action.yml", `name: Inner
inputs:
  caller-path:
    required: true
runs:
  using: composite
  steps:
    - shell: sh
      env:
        ENV_ACTION_PATH: ${{ github.action_path }}
        WITH_CALLER_PATH: ${{ inputs.caller-path }}
      run: |
        test "${{ github.action_path }}" = "$EXPECTED_INNER_PATH"
        test "$ENV_ACTION_PATH" = "$EXPECTED_INNER_PATH"
        test "$WITH_CALLER_PATH" = "$EXPECTED_OUTER_PATH"
`)
	writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", `name: Outer
runs:
  using: composite
  steps:
    - shell: sh
      run: |
        test "${{ github.action_path }}" = "$EXPECTED_OUTER_PATH"
        test "${{ github.action_path }}" = "$GITHUB_ACTION_PATH"
        test -f "${{ github.action_path }}/action.yml"
        test "${{ github.workflow }}" = "$EXPECTED_WORKFLOW"
        test "${{ github.job }}" = fixture
    - uses: ./.github/actions/inner
      with:
        caller-path: ${{ github.action_path }}
    - shell: sh
      run: test "${{ github.action_path }}" = "$EXPECTED_OUTER_PATH"
`)
	steps := []plan.Step{
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/outer"},
		{ID: "top", Kind: "run", Command: `test -z "${{ github.action_path }}"`},
	}
	job := runtimePlan(t, workspace, workflowPath, steps)
	job.Env = map[string]string{
		"EXPECTED_OUTER_PATH": filepath.Join(workspace, ".github", "actions", "outer"),
		"EXPECTED_INNER_PATH": filepath.Join(workspace, ".github", "actions", "inner"),
		"EXPECTED_WORKFLOW":   workflowPath,
	}
	var logs bytes.Buffer
	if _, err := (Runner{Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace); err != nil {
		t.Fatalf("RunJob() error = %v\nlogs: %s", err, logs.String())
	}
}

func TestRemoteCompositeGitHubActionIdentityExpressionsAreInvocationScoped(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: action identity\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "composite/action.yml", `name: Identity
runs:
  using: composite
  steps:
    - shell: sh
      env:
        ACTION_USER_AGENT: ${{ github.action_repository }}@${{ github.action_ref }}
      run: test "$ACTION_USER_AGENT" = owner/repo@v1
`)
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "identity", Kind: "uses", Uses: remoteLifecycleUses("composite"), Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "composite", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	if result, err := (Runner{Actions: materializer}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestExpressionTimeoutBoundsNestedCompositePre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested pre timeout\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", "name: root\nruns:\n  using: composite\n  steps:\n    - uses: "+remoteLifecycleUses("child")+"\n      continue-on-error: true\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
if [ "$(basename "$(dirname "$1")")/$(basename "$1")" = child/pre.js ]; then sleep 5; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}, TimeoutMinutesExpression: "${{ matrix.timeout }}",
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Matrix = map[string]any{"timeout": 0.001}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	started := time.Now()
	_, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() nested pre timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("RunJob() nested pre took %s after expression timeout", elapsed)
	}
}

func TestNestedRemoteCompositePreSupportsCompoundStepFields(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested pre expressions\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", `name: root
inputs:
  targets:
    required: false
  target:
    required: false
runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      with:
        message: ${{ inputs.targets || inputs.target || '' }}
      env:
        TARGETS: ${{ inputs.targets || inputs.target || '' }}
`)
	writeFixtureFile(t, remote, "child/action.yml", "name: child\ninputs:\n  message:\n    required: false\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
test -z "$INPUT_MESSAGE"
test -z "$TARGETS"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRemoteCompositePreExposesActionPathToChild(t *testing.T) {
	workspace := canonicalTempDir(t)
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: pre action path\n")
	remote := canonicalTempDir(t)
	writeFixtureFile(t, remote, "root/action.yml", `name: root
runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      with:
        caller_path: ${{ github.action_path }}
      env:
        CALLER_PATH_ENV: ${{ github.action_path }}
`)
	writeFixtureFile(t, remote, "child/action.yml", "name: child\ninputs:\n  caller_path:\n    required: false\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
test "$INPUT_CALLER_PATH" = "$EXPECTED_ROOT_PATH"
test "$CALLER_PATH_ENV" = "$EXPECTED_ROOT_PATH"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"EXPECTED_ROOT_PATH": filepath.Join(remote, "root")}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	var logs bytes.Buffer
	result, err := (Runner{Node24: fakeNode, Actions: materializer, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v\nlogs: %s", result, err, logs.String())
	}
}

func TestSkippedRemoteCompositeWithoutPreDoesNotEvaluateTimeout(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: skipped composite timeout\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", "name: root\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n")
	digest := digestTree(t, remote)
	rootID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}, Condition: "false", TimeoutMinutesExpression: "${{ fromJSON('invalid') }}",
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(rootID, "root", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	if result, err := (Runner{Actions: materializer}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() skipped composite result = %#v, error = %v", result, err)
	}
}

func TestNestedCompositePreservesInheritedJobStatus(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/status/action.yml", `name: Observe status
inputs:
  job-status:
    default: ${{ job.status }}
runs:
  using: composite
  steps:
    - if: always()
      shell: sh
      run: printf '%s' '${{ inputs.job-status }}' > "$STATUS_MARKER"
`)
	writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", `name: Outer
runs:
  using: composite
  steps:
    - if: always()
      uses: ./.github/actions/status
`)
	marker := filepath.Join(workspace, "status")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fail", Kind: "run", Command: "exit 7"},
		{ID: "outer", Kind: "uses", Uses: "./.github/actions/outer", Condition: "always()"},
	})
	job.Env = map[string]string{"STATUS_MARKER": marker}

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v; want original step failure", result, err)
	}
	status, readErr := os.ReadFile(marker)
	if readErr != nil || string(status) != "failure" {
		t.Fatalf("nested action job.status = %q, %v; want failure", status, readErr)
	}
}

func TestNestedJavaScriptPostSharesJobLIFORegistry(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	for _, name := range []string{"top", "nested"} {
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/main.js", "")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/post.js", "")
	}
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Nested lifecycle
runs:
  using: composite
  steps:
    - id: child
      uses: ./.github/actions/nested
`)
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "top", Kind: "uses", Uses: "./.github/actions/top"},
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"},
	})
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	if result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(events), "top:main\nnested:main\nnested:post\ntop:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}

	if err := os.Remove(lifecycle); err != nil {
		t.Fatal(err)
	}
	topID, compositeID, nestedID := "a-0000000000000001", "a-0000000000000002", "a-0000000000000003"
	job.Schema = plan.Schema
	job.Steps[0].Action = &plan.ActionSelector{Lock: topID}
	job.Steps[1].Action = &plan.ActionSelector{Lock: compositeID}
	job.Actions = []plan.ActionLock{
		{ID: topID, Source: "workspace", Path: ".github/actions/top", SourceDigest: digestTree(t, filepath.Join(workspace, ".github", "actions", "top"))},
		{
			ID: compositeID, Source: "workspace", Path: ".github/actions/composite", SourceDigest: digestTree(t, filepath.Join(workspace, ".github", "actions", "composite")),
			Children: map[string]plan.ActionSelector{"./.github/actions/nested": {Lock: nestedID}},
		},
		{ID: nestedID, Source: "workspace", Path: ".github/actions/nested", SourceDigest: digestTree(t, filepath.Join(workspace, ".github", "actions", "nested"))},
	}
	if result, err := (Runner{Node24: fakeNode}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("v3 RunJob() result = %#v, error = %v", result, err)
	}
	events, err = os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(events), "top:main\nnested:main\nnested:post\ntop:post\n"; got != want {
		t.Fatalf("v3 lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionPreHooksRunBeforeJobMainInDepthFirstOrder(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: remote lifecycle ordering\n")
	remote := t.TempDir()
	for i, name := range []string{"first", "skipped", "second"} {
		using := "node24"
		if i == 0 {
			using = "node20"
		}
		postIf := ""
		if name == "skipped" {
			postIf = "  post-if: steps.failed.outcome == 'failure' && steps.skipped.outcome == 'skipped' && steps.second.outcome == 'success'\n"
		}
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: "+using+"\n  pre: pre.js\n  main: main.js\n  post: post.js\n"+postIf)
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, name+"/"+phase+".js", "")
		}
	}
	writeFixtureFile(t, remote, "root/action.yml", `name: root
runs:
  using: composite
  steps:
    - uses: ./local
    - uses: owner/repo/first@v1
    - uses: owner/repo/nested@v1
`)
	writeFixtureFile(t, workspace, "local/action.yml", `name: local
runs:
  using: composite
  steps:
    - shell: sh
      run: printf '%s\n' 'local:main' >> "$LIFECYCLE_LOG"
`)
	writeFixtureFile(t, remote, "nested/action.yml", `name: nested
runs:
  using: composite
  steps:
    - id: failed
      shell: sh
      run: exit 7
    - id: skipped
      uses: owner/repo/skipped@v1
    - id: second
      if: always()
      uses: owner/repo/second@v1
`)
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	preBin := filepath.Join(workspace, "pre-bin")
	if err := os.Mkdir(preBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, preBin, "pre-tool", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(preBin, "pre-tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
case "$phase" in
  pre)
    printf 'owner=%s\n' "$action" >> "$GITHUB_STATE"
    if [ "$action" = first ]; then
      printf '%s\n' 'PRE_ENV=visible' >> "$GITHUB_ENV"
      printf '%s\n' "$PRE_BIN" >> "$GITHUB_PATH"
      printf '%s\n' '::add-mask::remote-pre-secret'
    fi
    if [ "$action" = second ]; then printf '%s\n' 'remote-pre-secret'; fi
    ;;
  main)
    test "$PRE_ENV" = visible
    test "$(command -v pre-tool)" = "$PRE_BIN/pre-tool"
    printf 'main=%s\n' "$action" >> "$GITHUB_STATE"
    ;;
  post)
    test "$STATE_owner" = "$action"
    if [ "$action" != skipped ]; then test "$STATE_main" = "$action"; fi
    ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}

	digest := digestTree(t, remote)
	rootID, firstID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	nestedID, skippedID, secondID := remoteLifecycleLockID(3), remoteLifecycleLockID(4), remoteLifecycleLockID(5)
	localID := remoteLifecycleLockID(6)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "ordinary", Kind: "run", Command: `test "$PRE_ENV" = visible
test "$(command -v pre-tool)" = "$PRE_BIN/pre-tool"
printf '%s\n' 'job:main' >> "$LIFECYCLE_LOG"`},
		{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle, "PRE_BIN": preBin}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{
			"./local":                     {Lock: localID},
			remoteLifecycleUses("first"):  {Lock: firstID},
			remoteLifecycleUses("nested"): {Lock: nestedID},
		}),
		remoteLifecycleLock(firstID, "first", digest, nil),
		remoteLifecycleLock(nestedID, "nested", digest, map[string]plan.ActionSelector{
			remoteLifecycleUses("skipped"): {Lock: skippedID},
			remoteLifecycleUses("second"):  {Lock: secondID},
		}),
		remoteLifecycleLock(skippedID, "skipped", digest, nil),
		remoteLifecycleLock(secondID, "second", digest, nil),
		{ID: localID, Source: "workspace", Path: "local", SourceDigest: digestTree(t, filepath.Join(workspace, "local"))},
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	var logs bytes.Buffer
	result, err := (Runner{Node24: fakeNode, Actions: materializer, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	want := "first:pre\nskipped:pre\nsecond:pre\njob:main\nlocal:main\nfirst:main\nsecond:main\nsecond:post\nskipped:post\nfirst:post\n"
	if got := string(events); got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
	if strings.Contains(logs.String(), "remote-pre-secret") || !strings.Contains(logs.String(), "***") {
		t.Fatalf("pre masking logs = %q", logs.String())
	}
	pathCount := 0
	for _, entry := range filepath.SplitList(result.Env["PATH"]) {
		if entry == preBin {
			pathCount++
		}
	}
	if result.Env["PRE_ENV"] != "visible" || pathCount != 1 {
		t.Fatalf("pre environment = %#v, pre path count = %d", result.Env, pathCount)
	}
}

func TestRemoteActionPostRegistrationFollowsStartedLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: skipped remote lifecycle\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "with-pre/action.yml", "name: with pre\ninputs:\n  token:\n    required: true\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, remote, "with-pre/"+phase+".js", "")
	}
	writeFixtureFile(t, remote, "without-pre/action.yml", "name: without pre\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, remote, "without-pre/main.js", "")
	writeFixtureFile(t, remote, "without-pre/post.js", "")
	writeFixtureFile(t, remote, "pre-if-false/action.yml", "name: pre condition false\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: success() && env.RUN_PRE == 'true'\n  main: main.js\n  post: post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, remote, "pre-if-false/"+phase+".js", "")
	}
	writeFixtureFile(t, remote, "main-fails/action.yml", "name: main fails\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, remote, "main-fails/main.js", "")
	writeFixtureFile(t, remote, "main-fails/post.js", "")
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
if [ "$action:$phase" = with-pre:pre ]; then test "$INPUT_TOKEN" = ghs_scoped_pre; fi
if [ "$action:$phase" = main-fails:main ]; then exit 7; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	withPreID, withoutPreID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	falsePreID, mainFailsID := remoteLifecycleLockID(3), remoteLifecycleLockID(4)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "with-pre", Kind: "uses", Uses: remoteLifecycleUses("with-pre"), Action: &plan.ActionSelector{Lock: withPreID}, With: map[string]string{"token": "${{ github.token }}"}, Env: map[string]string{"MINUTES": "5"}, TimeoutMinutesExpression: "${{ fromJSON(env.MINUTES) }}"},
		{ID: "without-pre", Kind: "uses", Uses: remoteLifecycleUses("without-pre"), Action: &plan.ActionSelector{Lock: withoutPreID}, Condition: "failure()"},
		{ID: "pre-if-false", Kind: "uses", Uses: remoteLifecycleUses("pre-if-false"), Action: &plan.ActionSelector{Lock: falsePreID}, Condition: "failure()", TimeoutMinutesExpression: "${{ fromJSON('invalid') }}"},
		{ID: "main-fails", Kind: "uses", Uses: remoteLifecycleUses("main-fails"), Action: &plan.ActionSelector{Lock: mainFailsID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network", "provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"contents": "read"}}
	job.Event.Repository = "owner/repo"
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(withPreID, "with-pre", digest, nil),
		remoteLifecycleLock(withoutPreID, "without-pre", digest, nil),
		remoteLifecycleLock(falsePreID, "pre-if-false", digest, nil),
		remoteLifecycleLock(mainFailsID, "main-fails", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	provider := &testWorkflowTokenProvider{token: "ghs_scoped_pre"}
	result, err := (Runner{Node24: fakeNode, Actions: materializer, WorkflowToken: provider, Redactor: &testRedactor{}}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, readErr := os.ReadFile(lifecycle)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(events), "with-pre:pre\nwith-pre:main\nmain-fails:main\nmain-fails:post\nwith-pre:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionMainAndPostEvaluateInputsAfterPriorSteps(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: deferred remote inputs\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "root/action.yml", `name: root
inputs:
  message:
    required: true
runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      with:
        message: ${{ inputs.message }}
`)
	writeFixtureFile(t, remote, "child/action.yml", `name: child
inputs:
  message:
    required: true
runs:
  using: node24
  main: main.js
  post: post.js
`)
	writeFixtureFile(t, remote, "child/main.js", "")
	writeFixtureFile(t, remote, "child/post.js", "")
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
test "$INPUT_MESSAGE" = from-producer
printf 'child:%s:%s\n' "$(basename "$1" .js)" "$INPUT_MESSAGE" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "producer", Kind: "run", Command: `printf '%s\n' 'value=from-producer' >> "$GITHUB_OUTPUT"`},
		{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), With: map[string]string{"message": "${{ steps.producer.outputs.value }}"}, Action: &plan.ActionSelector{Lock: rootID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(events), "child:main:from-producer\nchild:post:from-producer\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionPreFailureContinuesPreparationAndFailsJob(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: failed remote pre\n")
	remote := t.TempDir()
	for _, action := range []struct {
		name  string
		preIf string
	}{
		{name: "fails"},
		{name: "after"},
		{name: "on-failure", preIf: "failure()"},
		{name: "on-success", preIf: "success()"},
	} {
		condition := ""
		if action.preIf != "" {
			condition = "  pre-if: " + action.preIf + "\n"
		}
		writeFixtureFile(t, remote, action.name+"/action.yml", "name: "+action.name+"\nruns:\n  using: node24\n  pre: pre.js\n"+condition+"  main: main.js\n  post: post.js\n")
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, action.name+"/"+phase+".js", "")
		}
	}
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
printf '%s:%s\n' "$action" "$phase" >> "$LIFECYCLE_LOG"
if [ "$action:$phase" = fails:pre ]; then exit 7; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	failsID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	onFailureID, onSuccessID := remoteLifecycleLockID(3), remoteLifecycleLockID(4)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fails", Kind: "uses", Uses: remoteLifecycleUses("fails"), Action: &plan.ActionSelector{Lock: failsID}},
		{ID: "after", Kind: "uses", Uses: remoteLifecycleUses("after"), Action: &plan.ActionSelector{Lock: afterID}},
		{ID: "on-failure", Kind: "uses", Uses: remoteLifecycleUses("on-failure"), Action: &plan.ActionSelector{Lock: onFailureID}},
		{ID: "on-success", Kind: "uses", Uses: remoteLifecycleUses("on-success"), Action: &plan.ActionSelector{Lock: onSuccessID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(failsID, "fails", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
		remoteLifecycleLock(onFailureID, "on-failure", digest, nil),
		remoteLifecycleLock(onSuccessID, "on-success", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), `action "owner/repo/fails@v1" pre`) {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, readErr := os.ReadFile(lifecycle)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(events), "fails:pre\nafter:pre\non-failure:pre\non-failure:post\nafter:post\nfails:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestRemoteActionPreFailurePropagatesEnvironmentToLaterPre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: failed remote pre effects\n")
	remote := t.TempDir()
	for _, name := range []string{"fails", "after"} {
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
		writeFixtureFile(t, remote, name+"/pre.js", "")
		writeFixtureFile(t, remote, name+"/main.js", "")
	}
	marker := filepath.Join(workspace, "observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = fails:pre ]; then
  printf '%s\n' 'PRE_EFFECT=available' >> "$GITHUB_ENV"
  exit 7
fi
if [ "$action:$phase" = after:pre ]; then
  test "$PRE_EFFECT" = available
  touch "$MARKER"
fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	failsID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "fails", Kind: "uses", Uses: remoteLifecycleUses("fails"), Action: &plan.ActionSelector{Lock: failsID}, ContinueOnError: true},
		{ID: "after", Kind: "uses", Uses: remoteLifecycleUses("after"), Action: &plan.ActionSelector{Lock: afterID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"MARKER": marker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(failsID, "fails", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later pre did not observe failed pre environment: %v", err)
	}
}

func TestRemoteCompositeSoftPreFailurePreservesSuccessForLaterPre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened composite pre failure\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\noutputs:\n  status:\n    value: ${{ steps.child.outcome }}-${{ steps.child.conclusion }}\nruns:\n  using: composite\n  steps:\n    - id: child\n      if: false\n      uses: owner/repo/child@v1\n      continue-on-error: true\n    - id: finalize\n      shell: sh\n      run: touch \"$COMPOSITE_MARKER\"\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n  post-if: steps.finalize.conclusion == 'success'\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	writeFixtureFile(t, remote, "child/post.js", "")
	writeFixtureFile(t, remote, "after/action.yml", "name: after\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: success()\n  main: main.js\n")
	writeFixtureFile(t, remote, "after/pre.js", "")
	writeFixtureFile(t, remote, "after/main.js", "")
	marker := filepath.Join(workspace, "observed")
	compositeMarker := filepath.Join(workspace, "composite-observed")
	childMainMarker := filepath.Join(workspace, "child-main-observed")
	childPostMarker := filepath.Join(workspace, "child-post-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:pre ]; then exit 7; fi
if [ "$action:$phase" = child:main ]; then touch "$CHILD_MAIN_MARKER"; fi
if [ "$action:$phase" = child:post ]; then touch "$CHILD_POST_MARKER"; fi
if [ "$action:$phase" = after:pre ]; then touch "$MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, childID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}, ContinueOnError: true},
		{ID: "after", Kind: "uses", Uses: remoteLifecycleUses("after"), Action: &plan.ActionSelector{Lock: afterID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"MARKER": marker, "COMPOSITE_MARKER": compositeMarker, "CHILD_MAIN_MARKER": childMainMarker, "CHILD_POST_MARKER": childPostMarker}
	job.Outputs = map[string]string{"status": "${{ steps.parent.outputs.status }}"}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later success pre did not run: %v", err)
	}
	if result.Outputs["status"] != "failure-success" {
		t.Fatalf("RunJob() outputs = %#v, want retained child pre-failure status", result.Outputs)
	}
	if _, err := os.Stat(compositeMarker); err != nil {
		t.Fatalf("later composite step did not run: %v", err)
	}
	if _, err := os.Stat(childMainMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child main ran after its pre failed: %v", err)
	}
	if _, err := os.Stat(childPostMarker); err != nil {
		t.Fatalf("child post did not see the final composite step status: %v", err)
	}
}

func TestRemoteCompositeSoftPreIfFailureSkipsMain(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened composite pre-if failure\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/child@v1\n      continue-on-error: true\n    - shell: sh\n      run: touch \"$LATER_STEP_MARKER\"\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: runner.debug == 'true'\n  main: main.js\n")
	writeFixtureFile(t, remote, "child/pre.js", "")
	writeFixtureFile(t, remote, "child/main.js", "")
	laterStepMarker := filepath.Join(workspace, "later-step-observed")
	childMainMarker := filepath.Join(workspace, "child-main-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:main ]; then touch "$CHILD_MAIN_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LATER_STEP_MARKER": laterStepMarker, "CHILD_MAIN_MARKER": childMainMarker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(laterStepMarker); err != nil {
		t.Fatalf("later composite step did not run: %v", err)
	}
	if _, err := os.Stat(childMainMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child main ran after its pre-if failed: %v", err)
	}
}

func TestRemoteCompositeSoftConditionFailureBindsPostToFinalStepState(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened composite condition failure post scope\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\nruns:\n  using: composite\n  steps:\n    - id: child\n      if: runner.debug == 'true'\n      uses: owner/repo/child@v1\n      continue-on-error: true\n    - id: finalize\n      shell: sh\n      run: touch \"$FINALIZE_MARKER\"\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n  post-if: steps.finalize.conclusion == 'success'\n")
	for _, path := range []string{"child/pre.js", "child/main.js", "child/post.js"} {
		writeFixtureFile(t, remote, path, "")
	}
	finalizeMarker := filepath.Join(workspace, "finalize-observed")
	postMarker := filepath.Join(workspace, "post-observed")
	mainMarker := filepath.Join(workspace, "main-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:main ]; then touch "$MAIN_MARKER"; fi
if [ "$action:$phase" = child:post ]; then touch "$POST_MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"FINALIZE_MARKER": finalizeMarker, "MAIN_MARKER": mainMarker, "POST_MARKER": postMarker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	for _, path := range []string{finalizeMarker, postMarker} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected marker %q: %v", path, err)
		}
	}
	if _, err := os.Stat(mainMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("condition-failed child main ran: %v", err)
	}
}

func TestRemoteCompositeSoftNestedPreparationFailurePreservesSuccessForLaterPre(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: softened nested composite preparation failure\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "parent/action.yml", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/nested@v1\n      continue-on-error: true\n    - uses: owner/repo/after@v1\n")
	writeFixtureFile(t, remote, "nested/action.yml", "name: nested\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/child@v1\n")
	writeFixtureFile(t, remote, "child/action.yml", "name: child\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "after/action.yml", "name: after\nruns:\n  using: node24\n  pre: pre.js\n  pre-if: success()\n  main: main.js\n")
	for _, path := range []string{"child/pre.js", "child/main.js", "after/pre.js", "after/main.js"} {
		writeFixtureFile(t, remote, path, "")
	}
	marker := filepath.Join(workspace, "later-pre-observed")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
action=$(basename "$(dirname "$1")")
phase=$(basename "$1" .js)
if [ "$action:$phase" = child:pre ]; then exit 7; fi
if [ "$action:$phase" = after:pre ]; then touch "$MARKER"; fi
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	parentID, nestedID, childID, afterID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3), remoteLifecycleLockID(4)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "parent", Kind: "uses", Uses: remoteLifecycleUses("parent"), Action: &plan.ActionSelector{Lock: parentID}}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"MARKER": marker}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(parentID, "parent", digest, map[string]plan.ActionSelector{remoteLifecycleUses("nested"): {Lock: nestedID}, remoteLifecycleUses("after"): {Lock: afterID}}),
		remoteLifecycleLock(nestedID, "nested", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
		remoteLifecycleLock(afterID, "after", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later pre-if: success() did not run: %v", err)
	}
}

func TestRemotePreparationErrorStillDrainsRegisteredPosts(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: remote preparation cleanup\n")
	remote := t.TempDir()
	for _, name := range []string{"first", "broken"} {
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, name+"/"+phase+".js", "")
		}
	}
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
printf '%s:%s\n' "$(basename "$(dirname "$1")")" "$(basename "$1" .js)" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	firstID, brokenID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "first", Kind: "uses", Uses: remoteLifecycleUses("first"), Action: &plan.ActionSelector{Lock: firstID}},
		{ID: "broken", Kind: "uses", Uses: remoteLifecycleUses("broken"), Action: &plan.ActionSelector{Lock: brokenID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(firstID, "first", digest, nil),
		remoteLifecycleLock(brokenID, "broken", "sha256:"+strings.Repeat("f", 64), nil),
	}
	job.ContinueOnError = true
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "materialized source digest mismatch") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, readErr := os.ReadFile(lifecycle)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(events), "first:pre\nfirst:post\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestJobContinueOnErrorDoesNotTolerateLazyWorkspaceActionIntegrityFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: lazy workspace integrity\n")
	writeFixtureFile(t, workspace, ".github/actions/local/action.yml", "name: local\nruns:\n  using: node24\n  main: index.js\n")
	writeFixtureFile(t, workspace, ".github/actions/local/index.js", "console.log('original')\n")
	lockID := "a-0000000000000001"
	lock := plan.ActionLock{ID: lockID, Source: "workspace", Path: ".github/actions/local", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/local"))}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "tamper", Kind: "run", Shell: "sh", Command: "printf tampered > .github/actions/local/index.js"},
		{ID: "local", Kind: "uses", Uses: "./.github/actions/local", Action: &plan.ActionSelector{Lock: lockID}, ContinueOnError: true},
	})
	job.Actions = []plan.ActionLock{lock}
	job.ContinueOnError = true

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "workspace action digest mismatch") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRemotePreparedInvocationIDsCannotAliasStepIDs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: remote invocation identity\n")
	remote := t.TempDir()
	for _, name := range []string{"child", "direct"} {
		writeFixtureFile(t, remote, name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
		for _, phase := range []string{"pre", "main", "post"} {
			writeFixtureFile(t, remote, name+"/"+phase+".js", "")
		}
	}
	writeFixtureFile(t, remote, "root/action.yml", "name: root\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo/child@v1\n")
	lifecycle := filepath.Join(workspace, "lifecycle.log")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
printf '%s:%s\n' "$(basename "$(dirname "$1")")" "$(basename "$1" .js)" >> "$LIFECYCLE_LOG"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := digestTree(t, remote)
	rootID, childID, directID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{
		{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}},
		{ID: "root/0", Kind: "uses", Uses: remoteLifecycleUses("direct"), Action: &plan.ActionSelector{Lock: directID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
		remoteLifecycleLock(directID, "direct", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: fakeNode, Actions: materializer}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	want := "child:pre\ndirect:pre\nchild:main\ndirect:main\ndirect:post\nchild:post\n"
	if got := string(events); got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
}

func TestCompositeConditionsRunAfterFailureAndPreserveFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/conditions/action.yml", `name: Conditional composite
runs:
  using: composite
  steps:
    - id: failed
      shell: sh
      run: exit 7
    - id: skipped
      shell: sh
      run: touch "$SHOULD_NOT_RUN"
    - id: observed
      if: failure() && steps.failed.outcome == 'failure' && steps.skipped.outcome == 'skipped' && steps.skipped.outputs.missing == ''
      shell: sh
      run: touch "$STATUS_RAN"
    - id: cleanup
      if: always()
      shell: sh
      run: touch "$ALWAYS_RAN"
`)
	statusRan := filepath.Join(workspace, "status-ran")
	alwaysRan := filepath.Join(workspace, "always-ran")
	shouldNotRun := filepath.Join(workspace, "should-not-run")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/conditions"}})
	job.Env = map[string]string{"STATUS_RAN": statusRan, "ALWAYS_RAN": alwaysRan, "SHOULD_NOT_RUN": shouldNotRun}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v, want preserved composite failure", result, err)
	}
	for _, path := range []string{statusRan, alwaysRan} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("expected conditional child %q to run: %v", path, statErr)
		}
	}
	if _, statErr := os.Stat(shouldNotRun); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("default child ran after failure: %v", statErr)
	}
}

func TestCompositeContinueOnErrorPreservesOutcomeAndRunsLaterSteps(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/soft-failure/action.yml", `name: Soft failure composite
outputs:
  status:
    value: ${{ steps.failed.outcome }}-${{ steps.failed.conclusion }}
runs:
  using: composite
  steps:
    - id: failed
      shell: sh
      run: exit 7
      continue-on-error: true
    - shell: sh
      run: touch "$LATER_STEP_RAN"
`)
	laterStep := filepath.Join(workspace, "later-step-ran")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/soft-failure"}})
	job.Env = map[string]string{"LATER_STEP_RAN": laterStep}
	job.Outputs = map[string]string{"status": "${{ steps.composite.outputs.status }}"}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, want soft composite failure", result, err)
	}
	if result.Outputs["status"] != "failure-success" {
		t.Fatalf("RunJob() outputs = %#v, want retained soft-failure status", result.Outputs)
	}
	if _, statErr := os.Stat(laterStep); statErr != nil {
		t.Fatalf("later composite step did not run: %v", statErr)
	}
}

func TestCompositeContinueOnErrorToleratesConditionFailure(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/soft-condition/action.yml", `name: Soft condition failure composite
outputs:
  status:
    value: ${{ steps.failed.outcome }}-${{ steps.failed.conclusion }}
runs:
  using: composite
  steps:
    - id: failed
      if: runner.debug == 'true'
      shell: sh
      run: touch "$SHOULD_NOT_RUN"
      continue-on-error: true
    - shell: sh
      run: touch "$LATER_STEP_RAN"
`)
	laterStep := filepath.Join(workspace, "later-step-ran")
	shouldNotRun := filepath.Join(workspace, "should-not-run")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/soft-condition"}})
	job.Env = map[string]string{"LATER_STEP_RAN": laterStep, "SHOULD_NOT_RUN": shouldNotRun}
	job.Outputs = map[string]string{"status": "${{ steps.composite.outputs.status }}"}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, want soft condition failure", result, err)
	}
	if result.Outputs["status"] != "failure-success" {
		t.Fatalf("RunJob() outputs = %#v, want retained soft-failure status", result.Outputs)
	}
	if _, statErr := os.Stat(laterStep); statErr != nil {
		t.Fatalf("later composite step did not run: %v", statErr)
	}
	if _, statErr := os.Stat(shouldNotRun); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("condition-failed child ran: %v", statErr)
	}
}

func TestCompositeChildConditionUsesActionInputs(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		t.Run(enabled, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			writeFixtureFile(t, workspace, ".github/actions/conditional/action.yml", `name: Input conditional composite
inputs:
  enabled:
    required: true
runs:
  using: composite
  steps:
    - if: inputs.enabled == 'true'
      shell: sh
      run: touch "$CONDITIONAL_RAN"
`)
			marker := filepath.Join(workspace, "conditional-ran")
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/conditional", With: map[string]string{"enabled": enabled}}})
			job.Env = map[string]string{"CONDITIONAL_RAN": marker}
			result, err := (Runner{}).RunJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			_, statErr := os.Stat(marker)
			if enabled == "true" && statErr != nil {
				t.Fatalf("enabled child did not run: %v", statErr)
			}
			if enabled == "false" && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("disabled child ran: %v", statErr)
			}
		})
	}
}

func TestCompositeStepSupportsCompoundInputExpression(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/rust/action.yml", `name: Rust setup expression
inputs:
  toolchain:
    required: false
  components:
    required: false
  targets:
    required: false
  target:
    required: false
runs:
  using: composite
  steps:
    - shell: sh
      env:
        targets: ${{ inputs.targets || inputs.target || '' }}
        owner: ${{ github.repository_owner }}
      run: |
        test "${{ runner.temp }}" = "$RUNNER_TEMP"
        echo "downgrade=${{contains(inputs.toolchain, 'nightly') && inputs.components && ' --allow-downgrade' || ''}}" > "$RESULT"
        echo "targets=$targets" >> "$RESULT"
        echo "owner=$owner" >> "$RESULT"
`)
	output := filepath.Join(workspace, "result")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:   "rust",
		Kind: "uses",
		Uses: "./.github/actions/rust",
		With: map[string]string{"toolchain": "nightly", "components": "rustfmt"},
	}})
	job.Env = map[string]string{"RESULT": output}
	job.Event.Repository = "buildkite/buildkite-gha"

	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "downgrade= --allow-downgrade\ntargets=\nowner=buildkite\n"; got != want {
		t.Fatalf("compound composite step expressions = %q, want %q", got, want)
	}
}

func TestRuntimeRejectsRecursiveAndOverDepthCompositeActions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/recursive/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/recursive\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "recursive", Kind: "uses", Uses: "./.github/actions/recursive"}})
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "recursion detected") {
		t.Fatalf("RunJob() recursion error = %v", err)
	}

	for i := 0; i <= metadata.MaxNestedActionDepth; i++ {
		next := ""
		if i < metadata.MaxNestedActionDepth {
			next = fmt.Sprintf("  steps:\n    - uses: ./.github/actions/depth-%d\n", i+1)
		}
		writeFixtureFile(t, workspace, fmt.Sprintf(".github/actions/depth-%d/action.yml", i), "runs:\n  using: composite\n"+next)
	}
	job = runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "deep", Kind: "uses", Uses: "./.github/actions/depth-0"}})
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds maximum depth %d", metadata.MaxNestedActionDepth)) {
		t.Fatalf("RunJob() depth error = %v", err)
	}
}

func TestRuntimeMapDiagnosticsAreSorted(t *testing.T) {
	_, err := evaluateMap(map[string]string{
		"z-last":  "${{ unsupported.z }}",
		"a-first": "${{ unsupported.a }}",
	}, expression.Context{})
	if err == nil || !strings.Contains(err.Error(), `evaluate "a-first"`) {
		t.Fatalf("evaluateMap() error = %v, want alphabetically first key", err)
	}

	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "shell", Kind: "run", Shell: "sh", Command: "true"}})
	job.Needs = map[string]plan.Need{"z-last": {}, "a-first": {}}
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `prerequisite result "a-first"`) {
		t.Fatalf("RunJob() prerequisite error = %v, want alphabetically first key", err)
	}

	job.Needs = nil
	job.Outputs = map[string]string{"z-valid": "partial", "a-invalid": "${{ unsupported.a }}"}
	result, err := (Runner{}).RunJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `job output "a-invalid"`) {
		t.Fatalf("RunJob() output error = %v, want alphabetically first key", err)
	}
	if len(result.Outputs) != 0 {
		t.Fatalf("RunJob() partial outputs = %#v, want none before first sorted error", result.Outputs)
	}
}

func TestLivePortableSetupActions(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_ACTIONS") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_ACTIONS=1 to execute public setup actions with anonymous downloads")
	}
	node := requireNode24(t)
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "portable-setup.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  setup:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38
        with:
          node-version: "24"
          package-manager-cache: "false"
          token: ""
      - run: node --version | grep '^v24\.'
      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16
        with:
          go-version: "1.26.5"
          cache: "false"
          token: ""
      - run: go version | grep 'go1\.26\.5 '
`)
	if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := source.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	actionCache := filepath.Join(t.TempDir(), "actions")
	if err := os.Mkdir(actionCache, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := source.NewStore(actionCache, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(ctx, workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource: compiler.PublicActionSource{
			Resolver: resolver,
			Store:    store,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.Schema || len(plans[0].Actions) != 2 || plans[0].RequiresMise == nil || !*plans[0].RequiresMise {
		t.Fatalf("portable setup plans = %#v", plans)
	}
	if got := plans[0].Steps[0].With["node-version"]; got != "24" {
		t.Fatalf("setup-node plan input = %q, want 24", got)
	}
	var logs bytes.Buffer
	result, err := (Runner{Node24: node, Actions: store, Stdout: &logs, Stderr: &logs}).RunJob(ctx, plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v\nlogs:\n%s", result, err, logs.String())
	}
}

func TestCompiledWorkflowUsesHashFilesAfterWorkspacePreparation(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "hash-files.yml")
	workflow := []byte(`on: push
jobs:
  hash:
    runs-on: ubuntu-latest
    steps:
      - run: printf 'runtime contents' > payload
      - if: hashFiles('payload') != ''
        env:
          PAYLOAD_HASH: ${{ hashFiles('payload') }}
        run: test "$PAYLOAD_HASH" = "93f7a1af9e76c89675b5bc8c5f5c6aa62f1c78bc0c95693f0296b25274843527"
`)
	writeFixtureFile(t, workspace, ".github/workflows/hash-files.yml", string(workflow))
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(workflowPath, workflow, event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "hosted")
	if err != nil || len(plans) != 1 {
		t.Fatalf("compile hashFiles workflow = %#v, %v", plans, err)
	}
	result, err := (Runner{}).RunJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunJobDockerUsesSharedMasking(t *testing.T) {
	docker := requireDocker(t)
	workspace := fixturePath(t)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "docker", Kind: "uses", Uses: "./actions/docker"}})
	job.RequiredCapabilities = []string{"docker", "network"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Docker: docker}).RunJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Env["DOCKER_RUNTIME_SEEN"] != "true" || strings.Contains(logs.String(), "docker-secret-value") || !strings.Contains(logs.String(), "masked docker probe: ***") {
		t.Fatalf("RunJob() result = %#v, logs = %q", result, logs.String())
	}
}

func TestRunJobRejectsWorkflowMismatchAndUnsupportedAction(t *testing.T) {
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "local", Kind: "uses", Uses: "./actions/javascript"}})
	job.Workflow.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "workflow digest mismatch") {
		t.Fatalf("RunJob() error = %v, want workflow digest mismatch", err)
	}
	job = runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "remote", Kind: "uses", Uses: "actions/checkout@v4"}})
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "remote action") {
		t.Fatalf("RunJob() error = %v, want explicit remote action error", err)
	}

	for _, using := range []string{"future"} {
		t.Run(using, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			writeFixtureFile(t, workspace, ".github/actions/unsupported/action.yml", "runs:\n  using: "+using+"\n")
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "unsupported", Kind: "uses", Uses: "./.github/actions/unsupported"}})
			if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `unsupported runtime "`+using+`"`) {
				t.Fatalf("RunJob() error = %v, want %s runtime rejection", err, using)
			}
		})
	}
}

func remoteLifecycleLockID(index int) string {
	return fmt.Sprintf("a-%016x", index)
}

func remoteLifecycleUses(path string) string {
	return "owner/repo/" + path + "@v1"
}

func remoteLifecycleLock(id, path, digest string, children map[string]plan.ActionSelector) plan.ActionLock {
	return plan.ActionLock{
		ID:           id,
		Source:       "github",
		Repository:   "owner/repo",
		RequestedRef: "v1",
		Commit:       strings.Repeat("a", 40),
		Path:         path,
		SourceDigest: digest,
		Children:     children,
	}
}

func runtimePlan(t *testing.T, workspace, workflowPath string, steps []plan.Step) plan.Job {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(workspace, workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(source)
	requiresMise := true
	return plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Runtime:      &plan.Runtime{DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow:     plan.Workflow{Path: workflowPath, Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "fixture"},
		Event:        plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:       plan.Target{StepKey: "gha-fixture", Queue: "ubuntu-latest"},
		Steps:        steps,
		RequiresMise: &requiresMise,
	}
}

func writeFixtureFile(t *testing.T, root, path, contents string) {
	t.Helper()
	path = filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeNodeExecutable(t *testing.T, path string, major int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("#!/bin/sh\nprintf 'v%d.99.0\\n'\n", major)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fixturePath(t *testing.T, parts ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	pathParts := append([]string{filepath.Dir(file), "..", "..", "testdata"}, parts...)
	path, err := filepath.Abs(filepath.Join(pathParts...))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireLinuxAMD64(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires a linux/amd64 runtime host")
	}
}

func requireNode24(t *testing.T) string {
	t.Helper()
	if node := os.Getenv("BUILDKITE_GHA_TEST_NODE24"); node != "" {
		if _, err := discoverNodeContext(t.Context(), 24, node, ""); err != nil {
			t.Fatalf("BUILDKITE_GHA_TEST_NODE24 is not Node 24: %v", err)
		}
		return node
	}
	if mise, err := exec.LookPath("mise"); err == nil {
		output, err := exec.Command(mise, "where", "node@24").CombinedOutput()
		if err == nil {
			node := filepath.Join(strings.TrimSpace(string(output)), "bin", "node")
			if _, err := discoverNodeContext(t.Context(), 24, node, ""); err == nil {
				return node
			}
		}
	}
	livePrerequisiteUnavailable(t, "Node 24 unavailable: set BUILDKITE_GHA_TEST_NODE24 or install managed Node 24 with `mise install node@24`")
	return ""
}

func requireDocker(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		livePrerequisiteUnavailable(t, "Docker unavailable: docker executable not found")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dockerConfig := t.TempDir()
	if err := os.Chmod(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerEnv := map[string]string{"DOCKER_CONFIG": dockerConfig}
	if output, err := boundedDockerCombinedOutput(ctx, dockerEnv, docker, "info", "--format", "{{.ServerVersion}}"); err != nil {
		livePrerequisiteUnavailable(t, "Docker unavailable: daemon probe failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := boundedDockerCombinedOutput(ctx, dockerEnv, docker, "buildx", "inspect", "default"); err != nil || dockerBuilderDriver(string(output)) != "docker" {
		livePrerequisiteUnavailable(t, "Docker unavailable: default Buildx builder is not the local docker driver: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return docker
}

func livePrerequisiteUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	message := fmt.Sprintf(format, args...)
	if os.Getenv("BUILDKITE_GHA_LIVE_REQUIRED") == "1" {
		t.Fatal(message)
	}
	t.Skip(message)
}
