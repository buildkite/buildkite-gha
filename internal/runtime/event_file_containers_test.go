//go:build linux && amd64

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestEventFileJobContainerMount(t *testing.T) {
	fake := newJobDocker(t, "")
	workspace := t.TempDir()
	job := jobContainerPlan(t, workspace, []runtimeTestStep{{ID: "read", Kind: "run", Shell: "sh", Command: `cmp "$GITHUB_EVENT_PATH" expected.json`}})
	job.Env = map[string]string{"EXPRESSION_PATH": "${{ github.event_path }}"}
	payload := setEventFilePayload(t, &job, `{"comment":{"body":"not-in-logs"}}`)
	writeFixtureFile(t, workspace, "expected.json", string(payload))
	if _, err := (Runner{Docker: fake.path, RuntimeExecutable: os.Args[0]}).runTestJob(t.Context(), job, workspace); err != nil {
		t.Fatal(err)
	}
	calls := fake.calls(t)
	create := calls[jobDockerCallIndex(calls, "create")].Args
	if !strings.Contains(strings.Join(create, " "), ",target="+containerEventDirectory+",readonly") {
		t.Fatal("job container did not mount its event directory read-only")
	}
	found := false
	for _, call := range calls {
		args := strings.Join(call.Args, " ")
		if call.Args[0] == "exec" && strings.Contains(args, "GITHUB_EVENT_PATH="+containerEventPath) && strings.Contains(args, "EXPRESSION_PATH="+containerEventPath) {
			found = true
		}
	}
	if !found {
		t.Fatal("job container did not receive its translated event path")
	}
}

func TestEventFileDockerActionMount(t *testing.T) {
	for _, inJobContainer := range []bool{false, true} {
		t.Run(fmt.Sprint(inJobContainer), func(t *testing.T) {
			fake := newFakeDocker(t, "success")
			action := fakeDockerAction(t)
			job := plan.Job{}
			setEventFilePayload(t, &job, `{}`)
			path, err := newEventFile(job.Event, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(filepath.Dir(path)) }()
			run := newJobRun(Runner{Docker: fake.path})
			run.runtimeEnv = map[string]string{"GITHUB_EVENT_PATH": path}
			run.runnerTemp = t.TempDir()
			action.Env["GITHUB_EVENT_PATH"] = "/spoofed"
			action.Env["EXPRESSION_PATH"] = path
			action.Args = []string{path}
			if inJobContainer {
				run.jobContainer = &jobContainerBackend{workspace: action.Workspace, temp: run.runnerTemp}
			}
			if _, err := run.runDocker(t.Context(), newCommandOutputProcessor(&bytes.Buffer{}, &bytes.Buffer{}), action); err != nil {
				t.Fatal(err)
			}
			for _, call := range fake.calls(t) {
				if call.args[0] != "run" {
					continue
				}
				args := strings.Join(call.args, " ")
				if strings.Count(args, "GITHUB_EVENT_PATH=") != 1 || !strings.Contains(args, "GITHUB_EVENT_PATH="+containerEventPath) || !strings.Contains(args, "source="+filepath.Dir(path)+",target="+containerEventDirectory+",readonly") {
					t.Fatalf("Docker event mount/environment = %s", args)
				}
				if !strings.Contains(args, "EXPRESSION_PATH="+containerEventPath) || call.args[len(call.args)-1] != path {
					t.Fatalf("Docker must translate environment paths but preserve literal arguments: %s", args)
				}
			}
		})
	}
}

func TestLiveEventFileContainers(t *testing.T) {
	docker := requireDocker(t)
	runtime := buildLiveContainerRuntime(t)
	for _, inJobContainer := range []bool{false, true} {
		t.Run(fmt.Sprint(inJobContainer), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.Chmod(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFixtureFile(t, workspace, "workflow.yml", "name: live event file\n")
			writeFixtureFile(t, workspace, ".github/actions/docker/action.yml", `name: event reader
inputs:
  path:
    default: ${{ github.event_path }}
runs:
  using: docker
  image: Dockerfile
  args:
    - ${{ inputs.path }}
  env:
    EVENT_EXPRESSION: ${{ inputs.path }}
`)
			writeFixtureFile(t, workspace, ".github/actions/docker/Dockerfile", "FROM alpine:3.20\nCOPY --chmod=755 read-event /read-event\nUSER 12345\nENTRYPOINT [\"/read-event\"]\n")
			readEvent := `test -n "$GITHUB_EVENT_PATH"; cmp "$GITHUB_EVENT_PATH" "$GITHUB_WORKSPACE/expected.json"`
			writeFixtureFile(t, workspace, ".github/actions/docker/read-event", "#!/bin/sh\nset -eu\n"+readEvent+`
test "$EVENT_EXPRESSION" = "$GITHUB_EVENT_PATH"
test "$INPUT_PATH" = "$GITHUB_EVENT_PATH"
test "$1" = "$(cat "$GITHUB_WORKSPACE/expression-path")"
test ! -w "$GITHUB_EVENT_PATH"
test "$(id -u)" != 0
echo event-file-read
`)
			expressionRead := `test '${{ github.event_path }}' = "$GITHUB_EVENT_PATH"; cmp '${{ github.event_path }}' "$GITHUB_WORKSPACE/expected.json"; printf '%s' '${{ github.event_path }}' > expression-path`
			writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "name: composite reader\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: "+expressionRead+"\n")
			job := runtimePlan(t, workspace, "workflow.yml", []runtimeTestStep{
				{ID: "shell", Kind: "run", Shell: "sh", Command: expressionRead},
				{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"},
				{ID: "docker", Kind: "uses", Uses: "./.github/actions/docker", Env: map[string]string{"GITHUB_EVENT_PATH": "/spoofed"}},
			})
			job.RequiredCapabilities = []string{"docker", "network"}
			if inJobContainer {
				job.Container = &plan.Container{Image: "nginxinc/nginx-unprivileged:stable-alpine"}
			}
			payload := setEventFilePayload(t, &job, `{"issue":{"number":42},"comment":{"body":"unlogged-live-payload"}}`)
			writeFixtureFile(t, workspace, "expected.json", string(payload))
			if err := os.Chmod(filepath.Join(workspace, "expected.json"), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			var logs bytes.Buffer
			result, err := (Runner{Docker: docker, RuntimeExecutable: runtime, Stdout: &logs, Stderr: &logs}).runTestJob(ctx, job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("live event file readers: %v\n%s", err, &logs)
			}
			if strings.Count(logs.String(), "event-file-read\n") != 1 || strings.Contains(logs.String(), "unlogged-live-payload") {
				t.Fatalf("expected a Docker file read without payload logging: %s", &logs)
			}
		})
	}
}
