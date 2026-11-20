//go:build linux && amd64

package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func compileDirectDockerWorkflow(t *testing.T, workspace, body string) plan.Job {
	t.Helper()
	path := ".github/workflows/direct.yml"
	writeFixtureFile(t, workspace, path, body)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := compilePlansForTest(t.Context(), filepath.Join(workspace, path), []byte(body), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners:    compiler.RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "hosted"}, UntrustedQueues: []string{"hosted"}},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("compile direct Docker workflow: %v (%d jobs)", err, len(jobs))
	}
	encoded, err := plan.Encode(jobs[0])
	if err != nil {
		t.Fatal(err)
	}
	job, err := plan.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestCompiledDirectDockerArguments(t *testing.T) {
	requireLinuxAMD64(t)
	for _, test := range []struct {
		name, args string
		want       []string
	}{
		{name: "omitted"},
		{name: "empty", args: `''`},
		{name: "whitespace", args: `'   '`},
		{name: "quoted empty", args: `'""'`, want: []string{""}},
		{name: "single quoted empty", args: `"''"`, want: []string{"''"}},
		{name: "unclosed double quote", args: `'left "two words'`, want: []string{"left", "two words"}},
		{name: "double quote inside single quotes", args: `'left ''a"b c'' right'`, want: []string{"left", "'ab c' right"}},
		{name: "literal argv", args: `'"two words" "" ''single quotes'' $HOME $(touch /no-shell) ; --privileged one\"two'`, want: []string{"two words", "", "'single", "quotes'", "$HOME", "$(touch", "/no-shell)", ";", "--privileged", `one"two`}},
		{name: "evaluated once", args: `'${{ env.DATA }}'`, want: []string{"${{", "github.sha", "}}"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker(t, "success")
			workspace := t.TempDir()
			with := ""
			if test.args != "" {
				with = "          ArGs: " + test.args + "\n"
			}
			job := compileDirectDockerWorkflow(t, workspace, `on: push
jobs:
  direct:
    runs-on: ubuntu-latest
    env:
      DATA: ${{ format('{0}{{{{ github.sha }}}}', '$') }}
    steps:
      - uses: docker://busybox:1.37
        with:
          EnTrYpOiNt: /bin/echo
`+with)
			var logs bytes.Buffer
			result, err := (Runner{Docker: fake.path, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("RunJob = %#v, %v\n%s", result, err, logs.String())
			}
			argv := fakeDockerRunArgv(t, fake)
			image := slices.Index(argv, "sha256:"+strings.Repeat("c", 64))
			if image < 0 || !slices.Equal(argv[image+1:], test.want) {
				t.Fatalf("container arguments = %#v, want %#v", argv, test.want)
			}
			if entry := slices.Index(argv, "--entrypoint"); entry < 0 || argv[entry+1] != "/bin/echo" {
				t.Fatalf("entrypoint override missing: %#v", argv)
			}
			calls := fake.calls(t)
			pull := callIndex(calls, "pull", "busybox:1.37")
			if pull < 0 || pull > callIndex(calls, "run") || !strings.Contains(calls[pull].metadata, ";entries=0;") {
				t.Fatalf("image preparation did not use anonymous setup pull: %#v", calls)
			}
		})
	}
}

func TestLiveDirectDockerWorkflow(t *testing.T) {
	docker := requireDocker(t)
	for _, container := range []string{"", "    container: alpine:3.20\n"} {
		t.Run(map[bool]string{true: "job container", false: "host"}[container != ""], func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, "probe.sh", `set -eu
test "$INPUT_MESSAGE" = 'two words'
test "$VALUE" = step
test "$#" = 4
test "$1" = 'two words'
test -z "$2"
test "$3" = '$HOME'
test "$4" = '--privileged'
test ! -S /var/run/docker.sock
test "$(pwd)" = /github/workspace
test -r "$GITHUB_EVENT_PATH"
printf 'PING\r\n' | nc -w 2 cache 6379 | grep PONG
echo workspace > shared.txt
echo result=direct >> "$GITHUB_OUTPUT"
echo DIRECT_ENV=visible >> "$GITHUB_ENV"
echo /opt/direct-bin >> "$GITHUB_PATH"
echo direct-summary >> "$GITHUB_STEP_SUMMARY"
echo ::add-mask::synthetic-direct-secret
echo synthetic-direct-secret
`)
			job := compileDirectDockerWorkflow(t, workspace, `on: push
jobs:
  direct:
    runs-on: ubuntu-latest
`+container+`    env:
      VALUE: job
    services:
      cache:
        image: redis:7-alpine
        options: --health-cmd "redis-cli ping" --health-interval 1s --health-timeout 2s --health-retries 10
    outputs:
      result: ${{ steps.image.outputs.result }}
    steps:
      - id: image
        uses: docker://alpine:3.20
        env:
          VALUE: step
        with:
          entrypoint: /bin/sh
          args: 'probe.sh "two words" "" $HOME --privileged'
          message: two words
      - shell: sh
        run: |
          test "$(cat shared.txt)" = workspace
          test "$DIRECT_ENV" = visible
          test '${{ steps.image.outputs.result }}' = direct
          case "$PATH" in /opt/direct-bin:*) ;; *) exit 1 ;; esac
`)
			setEventFilePayload(t, &job, `{"probe":"direct"}`)
			var logs bytes.Buffer
			runner := Runner{Docker: docker, Stdout: &logs, Stderr: &logs}
			if container != "" {
				runner.RuntimeExecutable = buildLiveContainerRuntime(t)
			}
			before := liveDockerOwnedResources(t, docker)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			result, err := runner.RunJob(ctx, job, workspace)
			if err != nil || result.Conclusion != "success" || result.Outputs["result"] != "direct" || result.Summary != "direct-summary\n" {
				t.Fatalf("RunJob = %#v, %v\n%s", result, err, logs.String())
			}
			if strings.Contains(logs.String(), "synthetic-direct-secret") || !strings.Contains(logs.String(), "***") {
				t.Fatalf("direct image logs not masked: %s", logs.String())
			}
			if after := liveDockerOwnedResources(t, docker); !slices.Equal(before, after) {
				t.Fatalf("direct action leaked owned resources: before=%v after=%v", before, after)
			}
		})
	}
}

func TestLiveDirectDockerEnvironment(t *testing.T) {
	docker := requireDocker(t)
	for _, container := range []string{"", "    container: alpine:3.20\n"} {
		t.Run(map[bool]string{true: "job container", false: "host"}[container != ""], func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, "environment.sh", `set -eu
test "$HOME" = /github/home
test "$WORKSPACE_ALIAS" = /github/workspace
test "$INPUT_DIRECTORY" = /github/workspace/subdir
test "$TEMP_ALIAS" = /github/runner_temp/subdir
test "$EMBEDDED" != prefix:/github/workspace/file
test "$NEAR_PREFIX" != /github/workspace-suffix
test "$UNCLEAN_PATH" = /github/workspace/subdir/../file
test "$PATH" = "$EXPECTED_PATH"
test ! -S /var/run/docker.sock
test "$(cat "$WORKSPACE_ALIAS/file")" = workspace-marker
if test "$INPUT_PHASE" = first; then
  test ! -e "$HOME/direct-probe"
  if test "$PARENT_CONTAINER" = true; then
    test "$(cat "$HOME/parent-probe")" = parent-home
  fi
  echo shared-home > "$HOME/direct-probe"
else
  test "$(cat "$HOME/direct-probe")" = shared-home
fi
`)
			job := compileDirectDockerWorkflow(t, workspace, `on: push
jobs:
  direct:
    runs-on: ubuntu-latest
`+container+`    env:
      PARENT_CONTAINER: '`+strconv.FormatBool(container != "")+`'
      WORKSPACE_ALIAS: ${{ github.workspace }}
      EMBEDDED: prefix:${{ github.workspace }}/file
      NEAR_PREFIX: ${{ github.workspace }}-suffix
      UNCLEAN_PATH: ${{ github.workspace }}/subdir/../file
    steps:
      - shell: sh
        run: |
          echo workspace-marker > file
          echo /synthetic-prepend >> "$GITHUB_PATH"
          if test "$PARENT_CONTAINER" = true; then
            test "$HOME" = /github/home
            echo parent-home > "$HOME/parent-probe"
          fi
      - uses: docker://alpine:3.20
        env:
          HOME: /must-not-use-step-home
          TEMP_ALIAS: ${{ runner.temp }}/subdir
          EXPECTED_PATH: /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
        with:
          entrypoint: /bin/sh
          args: environment.sh
          directory: ${{ github.workspace }}/subdir
          phase: first
      - shell: sh
        run: |
          case "$PATH" in /synthetic-prepend:*) ;; *) exit 1 ;; esac
          if test "$PARENT_CONTAINER" = true; then
            test "$(cat "$HOME/direct-probe")" = shared-home
          fi
      - uses: docker://alpine:3.20
        env:
          HOME: /must-not-use-step-home
          TEMP_ALIAS: ${{ runner.temp }}/subdir
          PATH: ${{ github.workspace }}/bin:/usr/bin:/bin
          EXPECTED_PATH: /github/workspace/bin:/usr/bin:/bin
        with:
          entrypoint: /bin/sh
          args: environment.sh
          directory: ${{ github.workspace }}/subdir
          phase: second
      - shell: sh
        run: echo PATH=/explicit-from-env:/usr/bin:/bin >> "$GITHUB_ENV"
      - uses: docker://alpine:3.20
        env:
          TEMP_ALIAS: ${{ runner.temp }}/subdir
          EXPECTED_PATH: /explicit-from-env:/usr/bin:/bin
        with:
          entrypoint: /bin/sh
          args: environment.sh
          directory: ${{ github.workspace }}/subdir
          phase: third
      - uses: docker://alpine:3.20
        env:
          PATH: /usr/bin:${{ github.workspace }}/bin:/bin
          EXPECTED_PATH: /usr/bin:${{ github.workspace }}/bin:/bin
          TEMP_ALIAS: ${{ runner.temp }}/subdir
        with:
          entrypoint: /bin/sh
          args: environment.sh
          directory: ${{ github.workspace }}/subdir
          phase: non-leading-path
`)
			var logs bytes.Buffer
			runner := Runner{Docker: docker, Stdout: &logs, Stderr: &logs}
			if container != "" {
				runner.RuntimeExecutable = buildLiveContainerRuntime(t)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			before := liveDockerOwnedResources(t, docker)
			result, err := runner.RunJob(ctx, job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("RunJob = %#v, %v\n%s", result, err, logs.String())
			}
			if after := liveDockerOwnedResources(t, docker); !slices.Equal(before, after) {
				t.Fatalf("direct action leaked owned resources: before=%v after=%v", before, after)
			}
		})
	}
}

func TestCompiledDirectDockerInsideReusableComposite(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/callee.yml", `on: workflow_call
jobs:
  nested:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/composite
      - uses: docker://busybox:1.37
        with:
          entrypoint: /bin/echo
          args: '"nested words" ""'
`)
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: nested direct image
runs:
  using: composite
  steps:
    - shell: sh
      run: echo /composite-prepend >> "$GITHUB_PATH"
    - uses: docker://busybox:1.37
      with:
        entrypoint: /bin/echo
        args: '"nested words" ""'
    - shell: sh
      run: echo PATH=/composite-env:/usr/bin:/bin >> "$GITHUB_ENV"
    - uses: docker://busybox:1.37
      with:
        entrypoint: /bin/echo
        args: '"nested words" ""'
`)
	job := compileDirectDockerWorkflow(t, workspace, "on: push\njobs:\n  call:\n    uses: ./.github/workflows/callee.yml\n")
	fake := newFakeDocker(t, "success")
	if result, err := (Runner{Docker: fake.path}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("nested direct action = %#v, %v", result, err)
	}
	argv := fakeDockerRunArgv(t, fake)
	image := slices.Index(argv, "sha256:"+strings.Repeat("c", 64))
	if image < 0 || !slices.Equal(argv[image+1:], []string{"nested words", ""}) {
		t.Fatalf("nested container argv = %#v", argv)
	}
	var paths []string
	for _, call := range fake.calls(t) {
		if call.args[0] != "run" {
			continue
		}
		path := ""
		for _, arg := range call.args {
			if strings.HasPrefix(arg, "PATH=") {
				path = arg
			}
		}
		paths = append(paths, path)
	}
	if want := []string{"", "PATH=/composite-env:/usr/bin:/bin", "PATH=/composite-env:/usr/bin:/bin"}; !slices.Equal(paths, want) {
		t.Fatalf("nested Docker PATH values = %q, want %q", paths, want)
	}
}

func TestCompiledDirectDockerPATHSources(t *testing.T) {
	for _, test := range []struct {
		name, workflowEnv, jobEnv, stepEnv, setup, want string
	}{
		{name: "only prepend"},
		{name: "workflow", workflowEnv: "env:\n  PATH: /configured:/usr/bin:/bin\n", want: "/configured:/usr/bin:/bin"},
		{name: "job", jobEnv: "    env:\n      PATH: /configured:/usr/bin:/bin\n", want: "/configured:/usr/bin:/bin"},
		{name: "step", stepEnv: "        env:\n          PATH: /step:/usr/bin:/bin\n", want: "/step:/usr/bin:/bin"},
		{name: "file restores implicit value", setup: `printf 'PATH=%s\n' "$PATH" >> "$GITHUB_ENV"`, want: os.Getenv("PATH")},
		{name: "empty file PATH", setup: `echo PATH= >> "$GITHUB_ENV"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			job := compileDirectDockerWorkflow(t, workspace, "on: push\n"+test.workflowEnv+`jobs:
  direct:
    runs-on: ubuntu-latest
`+test.jobEnv+`    steps:
      - shell: sh
        run: |
          `+test.setup+`
          echo /synthetic-prepend >> "$GITHUB_PATH"
      - uses: docker://busybox:1.37
`+test.stepEnv+`        with:
          entrypoint: /bin/echo
`)
			fake := newFakeDocker(t, "success")
			if result, err := (Runner{Docker: fake.path}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
				t.Fatalf("RunJob = %#v, %v", result, err)
			}
			var got []string
			for _, arg := range fakeDockerRunArgv(t, fake) {
				if strings.HasPrefix(arg, "PATH=") {
					got = append(got, arg)
				}
			}
			var want []string
			if test.name != "only prepend" {
				want = []string{"PATH=" + test.want}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("Docker PATH = %s, want %s", strconv.Quote(strings.Join(got, ",")), strconv.Quote(strings.Join(want, ",")))
			}
		})
	}
}

func TestCompiledDirectDockerPreparationFailureStopsSteps(t *testing.T) {
	for _, scenario := range []string{"missing-docker", "pull-fail", "inspect-image-fail"} {
		t.Run(scenario, func(t *testing.T) {
			workspace := t.TempDir()
			job := compileDirectDockerWorkflow(t, workspace, `on: push
jobs:
  direct:
    runs-on: ubuntu-latest
    steps:
      - run: touch must-not-run
      - uses: docker://busybox:1.37
`)
			fake := newFakeDocker(t, scenario)
			docker := fake.path
			if scenario == "missing-docker" {
				docker = filepath.Join(t.TempDir(), "missing-docker")
			}
			if _, err := (Runner{Docker: docker}).RunJob(t.Context(), job, workspace); err == nil {
				t.Fatal("image preparation failure was ignored")
			}
			if _, err := os.Stat(filepath.Join(workspace, "must-not-run")); !os.IsNotExist(err) {
				t.Fatalf("workflow step ran before image preparation completed: %v", err)
			}
			if scenario == "missing-docker" {
				return
			}
			calls := fake.calls(t)
			if callIndex(calls, "run") >= 0 {
				t.Fatal("action ran after image preparation failure")
			}
			if _, err := os.Stat(calls[0].config); !os.IsNotExist(err) {
				t.Fatalf("private Docker config remains after setup failure: %v", err)
			}
		})
	}
}

func TestLiveDirectDockerImageDefaults(t *testing.T) {
	docker := requireDocker(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "argv.sh", "#!/bin/sh\nprintf 'argc=%s\\n' \"$#\" >> \"$GITHUB_OUTPUT\"\n")
	if err := os.Chmod(filepath.Join(workspace, "argv.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	const image = "docker://alpine/git@sha256:062a01ad7a0eb17cff382bc5e26086b4d710e56dfdfdf001109a49b6d9bd378c"
	job := compileDirectDockerWorkflow(t, workspace, `on: push
jobs:
  direct:
    runs-on: ubuntu-latest
    outputs:
      omitted: ${{ steps.override.outputs.argc }}
      empty: ${{ steps.empty.outputs.argc }}
    steps:
      - uses: `+image+`
      - uses: `+image+`
        with:
          args: ''
      - uses: `+image+`
        with:
          args: '   '
      - uses: `+image+`
        with:
          entrypoint: ''
          args: --version
      - id: override
        uses: `+image+`
        with:
          entrypoint: /github/workspace/argv.sh
      - id: empty
        uses: `+image+`
        with:
          entrypoint: /github/workspace/argv.sh
          args: '""'
`)
	var logs bytes.Buffer
	result, err := (Runner{Docker: docker, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["omitted"] != "0" || result.Outputs["empty"] != "1" {
		t.Fatalf("image defaults = %#v, %v\n%s", result, err, logs.String())
	}
	if strings.Count(logs.String(), "usage: git") != 3 || !strings.Contains(logs.String(), "git version 2.47.2") {
		t.Fatalf("image ENTRYPOINT/CMD defaults not retained: %s", logs.String())
	}
}
