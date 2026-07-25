package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// fakeJobDocker deliberately goes through a shell and a fresh copy of this test
// process.  Thus tests exercise exec.Cmd cancellation, pipes, quoting and argv
// boundaries rather than an in-process mock of Docker.
type fakeJobDocker struct{ path, root string }

type jobDockerCall struct {
	Args          []string `json:"args"`
	Config        string   `json:"config"`
	ConfigMode    int      `json:"config_mode"`
	ConfigEntries int      `json:"config_entries"`
	Host          *string  `json:"host"`
	Context       *string  `json:"context"`
	Builder       *string  `json:"builder"`
	Kit           *string  `json:"kit"`
}

func newJobDocker(t *testing.T, scenario string) fakeJobDocker {
	t.Helper()
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "docker")
	text := "#!/bin/sh\nJOB_DOCKER_ROOT=" + strconv.Quote(root) + " JOB_DOCKER_SCENARIO=" + strconv.Quote(scenario) + " exec " + strconv.Quote(exe) + " -test.run=^TestJobContainerFakeDockerProcess$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOB_DOCKER_ROOT", root)
	t.Setenv("JOB_DOCKER_SCENARIO", scenario)
	return fakeJobDocker{wrapper, root}
}

func (f fakeJobDocker) calls(t *testing.T) []jobDockerCall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []jobDockerCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var c jobDockerCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("decode call %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

func jobDockerCallIndex(calls []jobDockerCall, command ...string) int {
	for i, call := range calls {
		if len(call.Args) >= len(command) && slices.Equal(call.Args[:len(command)], command) {
			return i
		}
	}
	return -1
}

func envPointer(name string) *string {
	if v, ok := os.LookupEnv(name); ok {
		return &v
	}
	return nil
}

func TestJobContainerFakeDockerProcess(t *testing.T) {
	root := os.Getenv("JOB_DOCKER_ROOT")
	if root == "" {
		t.Skip("Docker child mode")
	}
	args := append([]string(nil), os.Args[slices.Index(os.Args, "--")+1:]...)
	config := os.Getenv("DOCKER_CONFIG")
	info, err := os.Stat(config)
	if err != nil {
		os.Exit(90)
	}
	entries, err := os.ReadDir(config)
	if err != nil {
		os.Exit(91)
	}
	c := jobDockerCall{Args: args, Config: config, ConfigMode: int(info.Mode().Perm()), ConfigEntries: len(entries), Host: envPointer("DOCKER_HOST"), Context: envPointer("DOCKER_CONTEXT"), Builder: envPointer("BUILDX_BUILDER"), Kit: envPointer("BUILDKIT_HOST")}
	b, _ := json.Marshal(c)
	b = append(b, '\n')
	f, err := os.OpenFile(filepath.Join(root, "calls"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(92)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	_, _ = f.Write(b)
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
	if len(args) == 0 {
		os.Exit(2)
	}
	scenario := os.Getenv("JOB_DOCKER_SCENARIO")
	key := args[0]
	if key == "network" && len(args) > 1 {
		key += "-" + args[1]
	}
	if scenario == "block-pull" && key == "pull" {
		select {}
	}
	if scenario == "fail-pull" && key == "pull" {
		os.Exit(42)
	}
	container, network := filepath.Join(root, "container"), filepath.Join(root, "network")
	actionContainer, actionImage := filepath.Join(root, "action-container"), filepath.Join(root, "action-image")
	switch key {
	case "buildx":
		if len(args) > 1 && args[1] == "inspect" {
			fmt.Print("Driver: docker\n")
			os.Exit(0)
		}
		if len(args) > 1 && args[1] == "build" {
			_ = os.WriteFile(actionImage, nil, 0o600)
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--label" {
					_ = os.WriteFile(filepath.Join(root, "action-owner"), []byte(args[i+1]), 0o600)
				}
			}
			os.Exit(0)
		}
		os.Exit(46)
	case "pull":
		os.Exit(0)
	case "network-create":
		_ = os.WriteFile(network, nil, 0o600)
		if scenario == "fail-network-create" {
			os.Exit(42)
		}
		os.Exit(0)
	case "create":
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--mount" {
				p := strings.Split(args[i+1], ",")
				var src, dst string
				for _, x := range p {
					if strings.HasPrefix(x, "source=") {
						src = strings.TrimPrefix(x, "source=")
					}
					if strings.HasPrefix(x, "target=") {
						dst = strings.TrimPrefix(x, "target=")
					}
				}
				if src != "" {
					_ = os.WriteFile(filepath.Join(root, "mount-"+strings.ReplaceAll(dst, "/", "_")), []byte(src), 0o600)
					mf, _ := os.OpenFile(filepath.Join(root, "mounts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
					mb, _ := json.Marshal(struct{ Target, Source string }{dst, src})
					_, _ = mf.Write(append(mb, '\n'))
					_ = mf.Close()
				}
			}
		}
		_ = os.WriteFile(container, nil, 0o600)
		if scenario == "fail-create" {
			os.Exit(42)
		}
		os.Exit(0)
	case "start":
		if scenario == "fail-start" {
			os.Exit(42)
		}
		os.Exit(0)
	case "ps":
		if scenario == "query-fail" && strings.Contains(strings.Join(args, " "), "name=") {
			os.Exit(43)
		}
		joined := strings.Join(args, " ")
		owner, _ := os.ReadFile(filepath.Join(root, "action-owner"))
		isAction := strings.Contains(joined, "buildkite-gha-container-") || (len(owner) != 0 && strings.Contains(joined, string(owner)))
		path := container
		if isAction {
			path = actionContainer
		}
		if _, err := os.Stat(path); err == nil {
			fmt.Print("container-id")
		}
		os.Exit(0)
	case "run":
		var files string
		for _, arg := range args {
			if strings.HasPrefix(arg, "type=bind,source=") && strings.HasSuffix(arg, ",target=/github/file_commands") {
				files = strings.TrimSuffix(strings.TrimPrefix(arg, "type=bind,source="), ",target=/github/file_commands")
			}
		}
		if files == "" {
			os.Exit(47)
		}
		_ = os.WriteFile(actionContainer, nil, 0o600)
		_ = os.WriteFile(filepath.Join(files, "output"), []byte("container=ran\n"), 0o600)
		_ = os.WriteFile(filepath.Join(files, "env"), []byte("DOCKER_RUNTIME_SEEN=true\n"), 0o600)
		_ = os.WriteFile(filepath.Join(files, "path"), []byte("/fake/action/bin\n"), 0o600)
		_ = os.WriteFile(filepath.Join(files, "state"), []byte("docker_state=seen\n"), 0o600)
		_ = os.WriteFile(filepath.Join(files, "summary"), []byte("docker action summary\n"), 0o600)
		fmt.Print("::add-mask::sibling-secret\nmasked: sibling-secret\n")
		if scenario == "fail-action-run" {
			os.Exit(48)
		}
		os.Exit(0)
	case "stop":
		os.Exit(0)
	case "rm":
		if strings.Contains(strings.Join(args, " "), "buildkite-gha-container-") {
			_ = os.Remove(actionContainer)
		} else {
			_ = os.Remove(container)
		}
		os.Exit(0)
	case "image":
		if len(args) > 1 && args[1] == "ls" {
			if _, err := os.Stat(actionImage); err == nil {
				fmt.Print("image-id")
			}
			os.Exit(0)
		}
		if len(args) > 1 && args[1] == "rm" {
			_ = os.Remove(actionImage)
			os.Exit(0)
		}
		os.Exit(46)
	case "network-ls":
		if scenario == "query-fail" && strings.Contains(strings.Join(args, " "), "name=") {
			os.Exit(44)
		}
		if scenario == "leftover" && !strings.Contains(strings.Join(args, " "), "name=") {
			fmt.Print("stray")
			os.Exit(0)
		}
		if scenario == "verify-fail" && !strings.Contains(strings.Join(args, " "), "name=") {
			os.Exit(45)
		}
		if _, err := os.Stat(network); err == nil {
			fmt.Print("network-id")
		}
		os.Exit(0)
	case "network-rm":
		_ = os.Remove(network)
		os.Exit(0)
	case "exec":
		startupProbe := strings.Contains(strings.Join(args, "\x00"), "startup-probe.pid")
		if (scenario == "fail-probe" && startupProbe) || (scenario == "fail-step" && !startupProbe) {
			os.Exit(42)
		}
		fakeJobDockerExec(root, scenario, args)
	default:
		os.Exit(46)
	}
}

func fakeJobDockerExec(root, scenario string, args []string) {
	var wd string
	env := map[string]string{}
	i := 1
	for i < len(args) {
		if args[i] == "--workdir" {
			wd = args[i+1]
			i += 2
			continue
		}
		if args[i] == "--env" {
			p := strings.SplitN(args[i+1], "=", 2)
			env[p[0]] = p[1]
			i += 2
			continue
		}
		break
	}
	if i >= len(args) {
		os.Exit(2)
	}
	i++ // container
	if i >= len(args) {
		os.Exit(2)
	}
	translate := func(p string) string {
		type mount struct{ Target, Source string }
		var best mount
		data, _ := os.ReadFile(filepath.Join(root, "mounts"))
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var m mount
			if json.Unmarshal([]byte(line), &m) == nil && (p == m.Target || strings.HasPrefix(p, m.Target+"/")) && len(m.Target) > len(best.Target) {
				best = m
			}
		}
		if best.Target != "" {
			return filepath.Join(best.Source, strings.TrimPrefix(p, best.Target))
		}
		return p
	}
	// Setup mount probes and Node compatibility probes intentionally bypass the
	// process helper in production, just as real docker exec does.
	if args[i] == "sh" && i+4 < len(args) && args[i+1] == "-c" {
		args[i+4] = translate(args[i+4])
		if scenario == "fail-mount-probe" {
			os.Exit(42)
		}
		if err := syscall.Exec("/bin/sh", args[i:], os.Environ()); err != nil {
			os.Exit(126)
		}
	}
	if strings.HasPrefix(args[i], "/") && i+1 < len(args) && args[i+1] == "--version" {
		if scenario == "wrong-node-major" {
			fmt.Print("v20.0.0\n")
			os.Exit(0)
		}
		if scenario == "incompatible-node" {
			os.Exit(126)
		}
		nodeArgs := append([]string(nil), args[i:]...)
		nodeArgs[0] = translate(nodeArgs[0])
		if err := syscall.Exec(nodeArgs[0], nodeArgs, os.Environ()); err != nil {
			os.Exit(126)
		}
	}
	if i+2 >= len(args) || args[i+1] != ContainerProcessHelperCommand {
		os.Exit(2)
	}
	helper := append([]string(nil), args[i+2:]...)
	if helper[0] == "run" && len(helper) >= 5 && helper[1] == jobContainerTemp+"/startup-probe.pid" {
		fmt.Print("/image/bin:/usr/bin")
		os.Exit(0)
	}
	if len(helper) > 1 {
		helper[1] = translate(helper[1])
	}
	if helper[0] == "run" {
		for j := 2; j < len(helper); j++ {
			if strings.HasPrefix(helper[j], "/") {
				helper[j] = translate(helper[j])
			}
		}
	}
	for k, v := range env {
		if k == "PATH" {
			parts := strings.Split(v, ":")
			for j := range parts {
				parts[j] = translate(parts[j])
			}
			v = strings.Join(parts, ":")
		} else {
			v = translate(v)
		}
		_ = os.Setenv(k, v)
	}
	if wd != "" {
		_ = os.Chdir(translate(wd))
	}
	os.Exit(RunContainerProcessHelper(helper))
}

func jobContainerPlan(t *testing.T, workspace string, steps []plan.Step) plan.Job {
	t.Helper()
	if len(steps) == 0 {
		steps = []plan.Step{{ID: "noop", Kind: "run", Shell: "sh", Command: "true"}}
	}
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "jobs: {}\n")
	j := runtimePlan(t, workspace, ".github/workflows/container.yml", steps)
	j.Schema = plan.SchemaV4
	j.RequiredCapabilities = []string{"docker", "network"}
	j.Container = &plan.Container{Image: "alpine:3.20"}
	return j
}

func TestRunJobContainerLifecycleAndEnvironment(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	_ = os.Chmod(workspace, 0o750)
	nested := filepath.Join(workspace, "nested")
	_ = os.Mkdir(nested, 0o755)
	t.Setenv("DOCKER_HOST", "bad")
	t.Setenv("DOCKER_CONTEXT", "bad")
	t.Setenv("BUILDX_BUILDER", "bad")
	t.Setenv("BUILDKIT_HOST", "bad")
	j := jobContainerPlan(t, workspace, []plan.Step{{ID: "one", Kind: "run", Shell: "sh", WorkingDirectory: "nested", Env: map[string]string{"P": "step"}, Command: `test "$PATH" = /image/bin:/usr/bin; test "$P" = step; test "$PWD" = "$GITHUB_WORKSPACE/nested"; echo E=ok >> "$GITHUB_ENV"; echo O=out >> "$GITHUB_OUTPUT"; echo /extra >> "$GITHUB_PATH"; echo S=state >> "$GITHUB_STATE"; echo summary >> "$GITHUB_STEP_SUMMARY"`}, {ID: "two", Kind: "run", Shell: "sh", Command: `test "$E" = ok; case "$PATH" in /extra:*) ;; *) exit 8;; esac`}})
	j.Env = map[string]string{"P": "job"}
	j.Container.Env = map[string]string{"P": "container"}
	j.Outputs = map[string]string{"observed": "${{ steps.one.outputs.O }}"}
	r, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if r.Env["E"] != "ok" || r.Outputs["observed"] != "out" || r.State["S"] != "state" || r.Summary != "summary\n" || !strings.HasPrefix(r.Env["PATH"], "/extra:") {
		t.Fatalf("result %#v", r)
	}
	if m, _ := os.Stat(workspace); m.Mode().Perm() != 0o750 {
		t.Fatalf("workspace mode %o", m.Mode().Perm())
	}
	calls := f.calls(t)
	if len(calls) != 13 {
		t.Fatalf("calls=%d: %#v", len(calls), calls)
	}
	want := []string{"pull", "network create", "create", "start", "exec", "exec", "exec", "ps", "rm", "network ls", "network rm", "ps", "network ls"}
	for i, c := range calls {
		if c.ConfigMode != 0o700 || c.ConfigEntries != 0 || c.Host != nil || c.Context != nil || c.Builder != nil || c.Kit != nil {
			t.Fatalf("private env call %d: %#v", i, c)
		}
		if i < len(want) && !strings.HasPrefix(strings.Join(c.Args, " "), want[i]) {
			t.Fatalf("call %d=%q want %q", i, c.Args, want[i])
		}
	}
	create := strings.Join(calls[2].Args, " ")
	for _, s := range []string{"target=" + jobContainerWorkspace, "target=" + jobContainerTemp, "target=" + jobContainerRuntime + ",readonly", "--workdir " + jobContainerWorkspace} {
		if !strings.Contains(create, s) {
			t.Errorf("create missing %q", s)
		}
	}
	if strings.Contains(create, "--user") {
		t.Error("unexpected user override")
	}
}

func TestRunJobContainerSetupFailuresCleanOwnedResources(t *testing.T) {
	for _, stage := range []string{"pull", "network-create", "create", "start", "probe", "step"} {
		t.Run(stage, func(t *testing.T) {
			f := newJobDocker(t, "fail-"+stage)
			w := t.TempDir()
			if err := os.Chmod(w, 0o750); err != nil {
				t.Fatal(err)
			}
			j := jobContainerPlan(t, w, nil)
			_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w)
			if err == nil {
				t.Fatal("expected failure")
			}
			calls := f.calls(t)
			removedContainer, removedNetwork := false, false
			for _, c := range calls {
				joined := strings.Join(c.Args, " ")
				if strings.HasPrefix(joined, "rm ") && !strings.Contains(joined, "buildkite-gha-job-") {
					t.Fatalf("unowned removal %q", joined)
				}
				removedContainer = removedContainer || strings.HasPrefix(joined, "rm --force buildkite-gha-job-")
				if strings.HasPrefix(joined, "network rm") && !strings.Contains(joined, "buildkite-gha-network-") {
					t.Fatalf("unowned removal %q", joined)
				}
				removedNetwork = removedNetwork || strings.HasPrefix(joined, "network rm buildkite-gha-network-")
			}
			if stage == "pull" {
				if removedContainer || removedNetwork {
					t.Fatalf("removed resources before pull completed: %#v", calls)
				}
			} else if !removedNetwork || (stage != "network-create" && !removedContainer) {
				t.Fatalf("incomplete %s cleanup: container=%t network=%t calls=%#v", stage, removedContainer, removedNetwork, calls)
			}
			if info, statErr := os.Stat(w); statErr != nil || info.Mode().Perm() != 0o750 {
				t.Fatalf("workspace mode after %s = %v, %v", stage, info, statErr)
			}
		})
	}
}

func TestRunJobContainerCleanupQueryFailureStillRemovesExactResources(t *testing.T) {
	f := newJobDocker(t, "query-fail")
	w := t.TempDir()
	j := jobContainerPlan(t, w, nil)
	_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w)
	if err == nil || !strings.Contains(err.Error(), "query job") {
		t.Fatalf("error=%v", err)
	}
	joined := fmt.Sprint(f.calls(t))
	if !strings.Contains(joined, "rm --force buildkite-gha-job-") || !strings.Contains(joined, "network rm buildkite-gha-network-") {
		t.Fatalf("cleanup %s", joined)
	}
}

func TestRunJobContainerReportsLeftoverAndVerificationFailure(t *testing.T) {
	for _, s := range []string{"leftover", "verify-fail"} {
		t.Run(s, func(t *testing.T) {
			f := newJobDocker(t, s)
			w := t.TempDir()
			j := jobContainerPlan(t, w, nil)
			_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w)
			if err == nil || !strings.Contains(err.Error(), "verify owned Docker cleanup") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func startTestBackend(t *testing.T, f fakeJobDocker) (*jobContainerBackend, string) {
	w := t.TempDir()
	tmp := t.TempDir()
	b, e := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond}).startJobContainer(context.Background(), w, tmp, plan.Container{Image: "alpine:3.20"})
	if e != nil {
		t.Fatal(e)
	}
	return b, w
}

func TestRunJobContainerCancellationTargetsProcessTree(t *testing.T) {
	f := newJobDocker(t, "")
	b, w := startTestBackend(t, f)
	marker := filepath.Join(w, "alive")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- b.exec(ctx, b.runner, newCommandProcessor(os.Stdout, os.Stderr), w, map[string]string{}, "sh", "-c", `trap '' TERM INT; sleep 30 & echo $! > alive; wait`)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("%v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation hung")
	}
	pidb, _ := os.ReadFile(marker)
	if p, _ := strconv.Atoi(strings.TrimSpace(string(pidb))); p > 0 && syscall.Kill(p, 0) == nil {
		t.Fatalf("child %d alive", p)
	}
	_ = b.cleanup()
	calls := f.calls(t)
	n := 0
	for _, c := range calls {
		if len(c.Args) > 5 && c.Args[0] == "exec" && strings.Contains(strings.Join(c.Args, " "), " terminate ") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("terminate calls=%d", n)
	}
}

func TestRunJobContainerConcurrentExecUsesUniquePIDFiles(t *testing.T) {
	f := newJobDocker(t, "")
	b, w := startTestBackend(t, f)
	c1, x := context.WithCancel(context.Background())
	d1 := make(chan error, 1)
	d2 := make(chan error, 1)
	go func() {
		d1 <- b.exec(c1, b.runner, newCommandProcessor(os.Stdout, os.Stderr), w, nil, "sh", "-c", "sleep 30")
	}()
	go func() {
		d2 <- b.exec(context.Background(), b.runner, newCommandProcessor(os.Stdout, os.Stderr), w, nil, "sh", "-c", "sleep .3")
	}()
	time.Sleep(100 * time.Millisecond)
	x()
	<-d1
	<-d2
	_ = b.cleanup()
	runs := map[string]bool{}
	terms := map[string]bool{}
	for _, c := range f.calls(t) {
		a := c.Args
		for i, v := range a {
			if v == ContainerProcessHelperCommand && i+2 < len(a) {
				if a[i+1] == "run" {
					runs[a[i+2]] = true
				}
				if a[i+1] == "terminate" {
					terms[a[i+2]] = true
				}
			}
		}
	}
	if len(runs) < 3 || len(terms) != 1 {
		t.Fatalf("runs=%v terms=%v", runs, terms)
	}
}

func TestRunJobContainerBarrierVisibility(t *testing.T) {
	f := newJobDocker(t, "")
	w := t.TempDir()
	j := jobContainerPlan(t, w, []plan.Step{{ID: "bg", Kind: "run", Shell: "sh", Background: true, Command: `sleep .15; echo LATE=yes >> "$GITHUB_ENV"; echo value=x >> "$GITHUB_OUTPUT"; echo /late >> "$GITHUB_PATH"`}, {ID: "before", Kind: "run", Shell: "sh", Command: `test -z "${LATE-}"`}, {ID: "wait", Kind: "wait", Targets: []string{"bg"}}, {ID: "after", Kind: "run", Shell: "sh", Command: `test "$LATE" = yes; case "$PATH" in /late:*) ;; *) exit 9;; esac`}})
	if _, e := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w); e != nil {
		t.Fatal(e)
	}
}

func TestRunJobContainerRejectsDeferredFeaturesBeforeDocker(t *testing.T) {
	for _, feature := range []string{"service", "action", "port"} {
		t.Run(feature, func(t *testing.T) {
			f := newJobDocker(t, "")
			w := t.TempDir()
			j := jobContainerPlan(t, w, nil)
			m := &fakeActionMaterializer{}
			switch feature {
			case "action":
				j.Steps = []plan.Step{{Kind: "uses", Uses: "x"}}
			case "service":
				j.Services = map[string]plan.Container{"db": {Image: "x"}}
			case "port":
				j.Container.Ports = []string{"8080"}
			}
			if _, e := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Actions: m}).RunJob(context.Background(), j, w); e == nil {
				t.Fatal("accepted")
			}
			if len(f.calls(t)) != 0 || m.calls != 0 {
				t.Fatalf("docker/materializer called")
			}
		})
	}
}

func TestRunJobContainerTimeoutCoversSetup(t *testing.T) {
	f := newJobDocker(t, "block-pull")
	w := t.TempDir()
	j := jobContainerPlan(t, w, nil)
	j.TimeoutMinutes = .001
	start := time.Now()
	_, e := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], CleanupTimeout: 100 * time.Millisecond}).RunJob(context.Background(), j, w)
	if !errors.Is(e, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("error=%v elapsed=%s", e, time.Since(start))
	}
}

func TestJobContainerPATHTranslationPreservesEmptyComponents(t *testing.T) {
	root := t.TempDir()
	b := jobContainerBackend{workspace: filepath.Join(root, "work"), temp: filepath.Join(root, "temp")}
	value := ":" + filepath.Join(b.workspace, "bin") + ":" + filepath.Join(b.temp, "tools") + ":"
	want := ":" + jobContainerWorkspace + "/bin:" + jobContainerTemp + "/tools:"
	if got := b.translatePATH(value); got != want {
		t.Fatalf("%q want %q", got, want)
	}
}

func TestContainerPathUsesLongestReadOnlyMount(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	selected := filepath.Join(repository, "selected")
	b := jobContainerBackend{mounts: []containerMount{
		{host: repository, target: "/__w/_actions/repository", readonly: true},
		{host: selected, target: "/__w/_actions/selected", readonly: true},
	}}
	if got, want := b.containerPath(filepath.Join(selected, "dist", "index.js")), "/__w/_actions/selected/dist/index.js"; got != want {
		t.Fatalf("containerPath = %q, want %q", got, want)
	}

	file := filepath.Join(root, "runtime")
	if err := os.WriteFile(file, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, mounts := range [][]containerMount{
		{{host: "relative", target: "/valid", readonly: true}},
		{{host: file, target: "relative", readonly: true}},
		{{host: file, target: "/bad,target", readonly: true}},
	} {
		if err := validateContainerMount(mounts[0]); err == nil {
			t.Fatalf("accepted invalid mount %#v", mounts[0])
		}
	}
}

func TestRunJobContainerNodeProbeFailureCleansOwnedResources(t *testing.T) {
	for _, scenario := range []string{"wrong-node-major", "incompatible-node"} {
		t.Run(scenario, func(t *testing.T) {
			f := newJobDocker(t, scenario)
			w := t.TempDir()
			writeFixtureFile(t, w, "unused/action.yml", "name: probe\nruns:\n  using: node24\n  main: main.js\n")
			writeFixtureFile(t, w, "unused/main.js", "")
			node := filepath.Join(t.TempDir(), "node")
			if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.0.0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			j := jobContainerPlan(t, w, nil)
			lockID := remoteLifecycleLockID(1)
			j.Steps = []plan.Step{{ID: "action", Kind: "uses", Uses: "./unused", Action: &plan.ActionSelector{Lock: lockID}}}
			j.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: "unused", SourceDigest: digestTree(t, filepath.Join(w, "unused"))}}
			_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: node}).RunJob(context.Background(), j, w)
			if err == nil || (!strings.Contains(err.Error(), "exact major") && !strings.Contains(err.Error(), "incompatible")) {
				t.Fatalf("error = %v", err)
			}
			calls := f.calls(t)
			joined := fmt.Sprint(calls)
			if !strings.Contains(joined, "rm --force buildkite-gha-job-") || !strings.Contains(joined, "network rm buildkite-gha-network-") {
				t.Fatalf("owned resources not cleaned: %s", joined)
			}
			for _, call := range calls {
				if strings.Contains(strings.Join(call.Args, " "), ContainerProcessHelperCommand+" run") && !strings.Contains(strings.Join(call.Args, " "), "startup-probe") {
					t.Fatalf("action lifecycle executed before failed Node probe: %#v", call.Args)
				}
			}
		})
	}
}

func TestRunJobContainerDoesNotProbeConfiguredNodeWithoutActions(t *testing.T) {
	f := newJobDocker(t, "incompatible-node")
	w := t.TempDir()
	node := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.0.0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	j := jobContainerPlan(t, w, []plan.Step{{ID: "shell", Kind: "run", Shell: "sh", Command: "true"}})
	if _, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: node}).RunJob(context.Background(), j, w); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls(t) {
		if slices.Contains(call.Args, "type=bind,source="+node+",target=/__buildkite-gha/node24,readonly") || slices.Contains(call.Args, "/__buildkite-gha/node24") {
			t.Fatalf("actionless container job mounted or probed Node: %#v", call.Args)
		}
	}
}

func TestRunJobContainerDoesNotProbeConfiguredNodeForCompositeOnlyActions(t *testing.T) {
	f := newJobDocker(t, "incompatible-node")
	w := t.TempDir()
	writeFixtureFile(t, w, "composite/action.yml", "name: composite only\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: echo COMPOSITE_ONLY=seen >> \"$GITHUB_ENV\"\n")
	node := filepath.Join(t.TempDir(), "missing-node")
	lockID := remoteLifecycleLockID(1)
	j := jobContainerPlan(t, w, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./composite", Action: &plan.ActionSelector{Lock: lockID}}})
	j.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: "composite", SourceDigest: digestTree(t, filepath.Join(w, "composite"))}}
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: node}).RunJob(context.Background(), j, w)
	if err != nil || result.Env["COMPOSITE_ONLY"] != "seen" {
		t.Fatalf("composite-only container result = %#v, error = %v", result, err)
	}
	for _, call := range f.calls(t) {
		if slices.Contains(call.Args, "/__buildkite-gha/node24") && slices.Contains(call.Args, "--version") {
			t.Fatalf("composite-only container probed Node: %#v", call.Args)
		}
	}
}

func TestRunJobContainerReadOnlyMountProbeFailureCleansOwnedResources(t *testing.T) {
	f := newJobDocker(t, "fail-mount-probe")
	w := t.TempDir()
	remote := t.TempDir()
	writeFixtureFile(t, remote, "selected/action.yml", "name: remote\nruns:\n  using: composite\n  steps: []\n")
	digest := digestTree(t, remote)
	j := jobContainerPlan(t, w, nil)
	lockID := remoteLifecycleLockID(1)
	j.Steps = []plan.Step{{ID: "action", Kind: "uses", Uses: remoteLifecycleUses("selected"), Action: &plan.ActionSelector{Lock: lockID}}}
	j.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "selected", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: filepath.Join(remote, "selected"), SourceDigest: digest}}
	_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Actions: materializer}).RunJob(context.Background(), j, w)
	if err == nil || !strings.Contains(err.Error(), "not readable/traversable") {
		t.Fatalf("read-only mount probe error = %v", err)
	}
	calls := f.calls(t)
	joined := fmt.Sprint(calls)
	if !strings.Contains(joined, "rm --force buildkite-gha-job-") || !strings.Contains(joined, "network rm buildkite-gha-network-") {
		t.Fatalf("owned resources not cleaned: %s", joined)
	}
	if len(calls) == 0 {
		t.Fatal("Docker was not called")
	}
	if _, statErr := os.Stat(calls[0].Config); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("private Docker config remains after mount probe failure: %v", statErr)
	}
}

func TestRunJobContainerSkippedRemoteJavaScriptDoesNotRequireNode(t *testing.T) {
	f := newJobDocker(t, "incompatible-node")
	w := t.TempDir()
	remote := t.TempDir()
	writeFixtureFile(t, remote, "selected/action.yml", "name: skipped remote\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, remote, "selected/main.js", "")
	writeFixtureFile(t, remote, "selected/post.js", "")
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	j := jobContainerPlan(t, w, []plan.Step{{ID: "skipped", Kind: "uses", Uses: remoteLifecycleUses("selected"), Condition: "false", Action: &plan.ActionSelector{Lock: lockID}}})
	j.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "selected", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: filepath.Join(remote, "selected"), SourceDigest: digest}}
	missingNode := filepath.Join(t.TempDir(), "missing-node")
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: missingNode, Actions: materializer}).RunJob(context.Background(), j, w)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("skipped remote JavaScript result = %#v, error = %v", result, err)
	}
	for _, call := range f.calls(t) {
		if slices.Contains(call.Args, "--version") {
			t.Fatalf("skipped remote JavaScript probed Node: %#v", call.Args)
		}
	}
}

func TestRunJobContainerJavaScriptLifecycle(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := fixturePath(t)
	node := requireNode24(t)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{
		ID: "javascript", Kind: "uses", Uses: "./actions/javascript",
		With:   map[string]string{"message": "container", "order": "single"},
		Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "workspace", Path: "actions/javascript",
		SourceDigest: digestTree(t, filepath.Join(workspace, "actions", "javascript")),
	}}
	job.Outputs = map[string]string{"result": "${{ steps.javascript.outputs.result }}"}
	var logs bytes.Buffer
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["result"] != "container-javascript" || result.Env["RUNTIME_SEEN"] != "true" || result.State["pre"] != "ready" || result.State["phase"] != "main" || result.Summary != "runtime main summary\nruntime post single\n" {
		t.Fatalf("container JavaScript result = %#v", result)
	}
	if strings.Contains(logs.String(), "runtime-secret-value") {
		t.Fatalf("container JavaScript logs leaked mask: %q", logs.String())
	}
	for _, event := range []string{"lifecycle:pre", "lifecycle:main", "masked probe: ***", "lifecycle:post:single"} {
		if !strings.Contains(logs.String(), event) {
			t.Fatalf("container JavaScript logs omit %q: %q", event, logs.String())
		}
	}
	pre, main, post := strings.Index(logs.String(), "lifecycle:pre"), strings.Index(logs.String(), "lifecycle:main"), strings.Index(logs.String(), "lifecycle:post:single")
	if pre < 0 || pre > main || main > post {
		t.Fatalf("container JavaScript lifecycle order = %q", logs.String())
	}
	calls := f.calls(t)
	createIndex := jobDockerCallIndex(calls, "create")
	if createIndex < 0 {
		t.Fatalf("Docker create absent: %#v", calls)
	}
	create := calls[createIndex].Args
	nodeMount := "type=bind,source=" + node + ",target=/__buildkite-gha/node24,readonly"
	if !slices.Contains(create, nodeMount) || slices.Contains(create, "--user") {
		t.Fatalf("container JavaScript create mounts/user = %#v", create)
	}
	seenLifecycleExec := false
	for _, call := range calls {
		joined := strings.Join(call.Args, "\x00")
		if !strings.Contains(joined, ContainerProcessHelperCommand+"\x00run") || !strings.Contains(joined, "/actions/javascript/") {
			continue
		}
		seenLifecycleExec = true
		if !strings.Contains(joined, "/__buildkite-gha/node24") || !strings.Contains(joined, "GITHUB_ACTION_PATH="+jobContainerWorkspace+"/actions/javascript") {
			t.Fatalf("container JavaScript paths were not translated: %#v", call.Args)
		}
	}
	if !seenLifecycleExec {
		t.Fatalf("container JavaScript exec absent: %#v", calls)
	}
}

func TestRunJobContainerCompositeAndNestedJavaScript(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: nested container actions\n")
	for _, name := range []string{"top", "nested"} {
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/action.yml", "name: "+name+"\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/main.js", "")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/post.js", "")
	}
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Nested lifecycle
runs:
  using: composite
  steps:
    - shell: sh
      run: echo COMPOSITE_SEEN=yes >> "$GITHUB_ENV"
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
	topID, compositeID, nestedID := remoteLifecycleLockID(1), remoteLifecycleLockID(2), remoteLifecycleLockID(3)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{
		{ID: "top", Kind: "uses", Uses: "./.github/actions/top", Action: &plan.ActionSelector{Lock: topID}},
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite", Action: &plan.ActionSelector{Lock: compositeID}},
		{ID: "verify", Kind: "run", Shell: "sh", Command: `test "$COMPOSITE_SEEN" = yes`},
	})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Env = map[string]string{"LIFECYCLE_LOG": lifecycle}
	job.Actions = []plan.ActionLock{
		{ID: topID, Source: "workspace", Path: ".github/actions/top", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/top"))},
		{ID: compositeID, Source: "workspace", Path: ".github/actions/composite", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/composite")), Children: map[string]plan.ActionSelector{"./.github/actions/nested": {Lock: nestedID}}},
		{ID: nestedID, Source: "workspace", Path: ".github/actions/nested", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/nested"))},
	}
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: fakeNode}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("container nested actions result = %#v, error = %v", result, err)
	}
	events, err := os.ReadFile(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(events), "top:main\nnested:main\nnested:post\ntop:post\n"; got != want {
		t.Fatalf("container nested lifecycle = %q, want %q", got, want)
	}
}

func TestRunJobContainerRemoteActionsMountedReadOnly(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: remote container action\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "selected/action.yml", "name: remote\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, remote, "selected/main.js", `require("node:fs").appendFileSync(process.env.GITHUB_OUTPUT, "remote=yes\n")
`)
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{{
		ID: "remote", Kind: "uses", Uses: remoteLifecycleUses("selected"), Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "selected", digest, nil)}
	job.Outputs = map[string]string{"remote": "${{ steps.remote.outputs.remote }}"}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: filepath.Join(remote, "selected"), SourceDigest: digest}}
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: requireNode24(t), Actions: materializer}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Outputs["remote"] != "yes" {
		t.Fatalf("remote container action result = %#v, error = %v", result, err)
	}
	if materializer.calls != 1 {
		t.Fatalf("remote materialization calls = %d, want 1", materializer.calls)
	}
	calls := f.calls(t)
	createIndex := jobDockerCallIndex(calls, "create")
	if createIndex < 0 {
		t.Fatalf("Docker create absent: %#v", calls)
	}
	target := remoteMountTarget("owner/repo", strings.Repeat("a", 40))
	wantMount := "type=bind,source=" + remote + ",target=" + target + ",readonly"
	count := 0
	for _, arg := range calls[createIndex].Args {
		if arg == wantMount {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("remote readonly root mount count = %d in %#v", count, calls[createIndex].Args)
	}
	if jobDockerCallIndex(calls, "create") <= 0 {
		t.Fatalf("remote materialization was not completed before Docker create")
	}
}

func TestRunJobContainerRemoteChildOfWorkspaceCompositeMountedBeforeCreate(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: nested remote container action\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: workspace parent
runs:
  using: composite
  steps:
    - uses: owner/repo/selected@v1
`)
	remote := t.TempDir()
	writeFixtureFile(t, remote, "selected/action.yml", "name: remote child\nruns:\n  using: node24\n  main: main.js\n")
	writeFixtureFile(t, remote, "selected/main.js", "")
	node := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v24.0.0; exit 0; fi
echo NESTED_REMOTE=seen >> "$GITHUB_ENV"
`)
	if err := os.Chmod(node, 0o700); err != nil {
		t.Fatal(err)
	}

	localID, remoteID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	digest := digestTree(t, remote)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{{
		ID: "composite", Kind: "uses", Uses: "./.github/actions/composite", Action: &plan.ActionSelector{Lock: localID},
	}})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Actions = []plan.ActionLock{
		{ID: localID, Source: "workspace", Path: ".github/actions/composite", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/composite")), Children: map[string]plan.ActionSelector{remoteLifecycleUses("selected"): {Lock: remoteID}}},
		remoteLifecycleLock(remoteID, "selected", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: filepath.Join(remote, "selected"), SourceDigest: digest}}
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: node, Actions: materializer}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Env["NESTED_REMOTE"] != "seen" {
		t.Fatalf("nested remote container action result = %#v, error = %v", result, err)
	}
	if materializer.calls != 1 {
		t.Fatalf("nested remote materialization calls = %d, want 1", materializer.calls)
	}
	calls := f.calls(t)
	createIndex := jobDockerCallIndex(calls, "create")
	if createIndex < 0 {
		t.Fatalf("Docker create absent: %#v", calls)
	}
	wantMount := "type=bind,source=" + remote + ",target=" + remoteMountTarget("owner/repo", strings.Repeat("a", 40)) + ",readonly"
	if !slices.Contains(calls[createIndex].Args, wantMount) {
		t.Fatalf("nested remote source was not mounted at create: %#v", calls[createIndex].Args)
	}
}

func TestRunJobContainerWorkspaceActionRemainsLazy(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: lazy container action\n")
	actionSource := t.TempDir()
	actionYAML := "name: lazy\nruns:\n  using: node24\n  main: main.js\n"
	mainJS := `require("node:fs").appendFileSync(process.env.GITHUB_OUTPUT, "lazy=ready\n")
`
	writeFixtureFile(t, actionSource, "action.yml", actionYAML)
	writeFixtureFile(t, actionSource, "main.js", mainJS)
	lockID := remoteLifecycleLockID(1)
	lazyDir := ".github/actions/lazy"
	create := fmt.Sprintf("mkdir -p %s; printf '%%s' %s | base64 -d > %s/action.yml; printf '%%s' %s | base64 -d > %s/main.js",
		lazyDir, base64.StdEncoding.EncodeToString([]byte(actionYAML)), lazyDir,
		base64.StdEncoding.EncodeToString([]byte(mainJS)), lazyDir)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{
		{ID: "populate", Kind: "run", Shell: "sh", Command: create},
		{ID: "lazy", Kind: "uses", Uses: "./" + lazyDir, Action: &plan.ActionSelector{Lock: lockID}},
	})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: lazyDir, SourceDigest: digestTree(t, actionSource)}}
	job.Outputs = map[string]string{"lazy": "${{ steps.lazy.outputs.lazy }}"}
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: requireNode24(t)}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Outputs["lazy"] != "ready" {
		t.Fatalf("lazy container action result = %#v, error = %v", result, err)
	}
}

func TestRunJobContainerRunsDockerActionsAsSiblings(t *testing.T) {
	t.Run("workspace", func(t *testing.T) {
		f := newJobDocker(t, "")
		workspace := fixturePath(t)
		var logs bytes.Buffer
		lockID := remoteLifecycleLockID(1)
		job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{
			ID: "docker", Kind: "uses", Uses: "./actions/docker", Action: &plan.ActionSelector{Lock: lockID},
		}})
		job.Schema = plan.SchemaV4
		job.RequiredCapabilities = []string{"docker", "network"}
		job.Container = &plan.Container{Image: "debian:bookworm-slim"}
		job.Env = map[string]string{"WORKSPACE_CHILD": filepath.Join(workspace, "child")}
		job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: "actions/docker", SourceDigest: digestTree(t, filepath.Join(workspace, "actions/docker"))}}
		job.Outputs = map[string]string{"container": "${{ steps.docker.outputs.container }}"}
		result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
		if err != nil || result.Outputs["container"] != "ran" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" || result.State["docker_state"] != "seen" || result.Summary != "docker action summary\n" || !strings.HasPrefix(result.Env["PATH"], "/fake/action/bin:") {
			t.Fatalf("workspace Docker action result = %#v, error = %v", result, err)
		}
		if strings.Contains(logs.String(), "sibling-secret") {
			t.Fatalf("mask leaked in logs: %q", logs.String())
		}
		calls := f.calls(t)
		create, run := jobDockerCallIndex(calls, "create"), jobDockerCallIndex(calls, "run")
		if create < 0 || run < 0 || jobDockerCallIndex(calls, "buildx", "build") < 0 {
			t.Fatalf("sibling build/run absent: %#v", calls)
		}
		network := argumentAfter(t, calls[create].Args, "--network")
		if got := argumentAfter(t, calls[run].Args, "--network"); got != network {
			t.Fatalf("sibling network = %q, want %q", got, network)
		}
		joined := strings.Join(calls[run].Args, " ")
		for _, want := range []string{"source=" + workspace + ",target=/github/workspace", "target=/github/runner_temp", "target=/github/file_commands", "WORKSPACE_CHILD=/github/workspace/child"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("sibling run missing %q: %#v", want, calls[run].Args)
			}
		}
		if slices.Contains(calls[run].Args, "--user") {
			t.Fatalf("sibling run overrides image user: %#v", calls[run].Args)
		}
		networkRemove := jobDockerCallIndex(calls, "network", "rm")
		actionRemove := -1
		for i, call := range calls {
			if len(call.Args) >= 3 && call.Args[0] == "rm" && strings.HasPrefix(call.Args[2], "buildkite-gha-container-") {
				actionRemove = i
			}
		}
		if actionRemove < 0 || networkRemove < actionRemove {
			t.Fatalf("job network removed before action cleanup: %#v", calls)
		}
	})

	t.Run("remote preflight", func(t *testing.T) {
		f := newJobDocker(t, "")
		workspace := t.TempDir()
		writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: remote Docker rejection\n")
		remote := t.TempDir()
		writeFixtureFile(t, remote, "docker/action.yml", "name: docker\nruns:\n  using: docker\n  image: Dockerfile\n")
		writeFixtureFile(t, remote, "docker/Dockerfile", "FROM scratch\n")
		digest := digestTree(t, remote)
		lockID := remoteLifecycleLockID(1)
		job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{{
			ID: "docker", Kind: "uses", Uses: remoteLifecycleUses("docker"), Action: &plan.ActionSelector{Lock: lockID},
		}})
		job.Schema = plan.SchemaV4
		job.RequiredCapabilities = []string{"docker", "network"}
		job.Container = &plan.Container{Image: "debian:bookworm-slim"}
		job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "docker", digest, nil)}
		materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: filepath.Join(remote, "docker"), SourceDigest: digest}}
		result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Actions: materializer}).RunJob(context.Background(), job, workspace)
		if err != nil || result.Env["DOCKER_RUNTIME_SEEN"] != "true" {
			t.Fatalf("remote Docker action result = %#v, error = %v", result, err)
		}
		if materializer.calls != 1 {
			t.Fatalf("remote materialization calls = %d, want 1", materializer.calls)
		}
		calls := f.calls(t)
		if jobDockerCallIndex(calls, "buildx", "build") < 0 || jobDockerCallIndex(calls, "run") < 0 {
			t.Fatalf("remote Docker action did not build and run: %#v", calls)
		}
		create := jobDockerCallIndex(calls, "create")
		if create < 0 {
			t.Fatalf("job container create absent: %#v", calls)
		}
		for _, arg := range calls[create].Args {
			if strings.Contains(arg, "source="+remote+",") {
				t.Fatalf("remote Docker-only source was unnecessarily mounted in the job container: %#v", calls[create].Args)
			}
		}
	})
}

func TestRunJobContainerSiblingDockerFailureCleansActionAndJobResources(t *testing.T) {
	f := newJobDocker(t, "fail-action-run")
	workspace := fixturePath(t)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{
		ID: "docker", Kind: "uses", Uses: "./actions/docker", Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: "actions/docker", SourceDigest: digestTree(t, filepath.Join(workspace, "actions/docker"))}}
	_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "run Docker action") {
		t.Fatalf("sibling Docker action failure = %v", err)
	}
	calls := f.calls(t)
	for _, command := range [][]string{{"rm", "--force"}, {"image", "rm", "--force"}, {"network", "rm"}} {
		if jobDockerCallIndex(calls, command...) < 0 {
			t.Fatalf("cleanup command %v absent: %#v", command, calls)
		}
	}
	actionRemove := jobDockerCallIndex(calls, "rm", "--force")
	jobNetworkRemove := jobDockerCallIndex(calls, "network", "rm")
	if actionRemove < 0 || jobNetworkRemove < actionRemove {
		t.Fatalf("job network cleanup raced action cleanup: %#v", calls)
	}
}

func TestRunDockerRejectsMismatchedJobContainerPaths(t *testing.T) {
	r := Runner{jobContainer: &jobContainerBackend{workspace: "/owned/workspace", temp: "/owned/temp"}}
	_, err := r.runDocker(context.Background(), newCommandProcessor(nil, nil), DockerAction{Workspace: "/other/workspace", runnerTemp: "/owned/temp"})
	if err == nil || !strings.Contains(err.Error(), "must match the job container's owned host paths") {
		t.Fatalf("mismatched sibling paths error = %v", err)
	}
}

func TestRunJobContainerJavaScriptCancellationRunsPost(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: cancelled container action\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/action.yml", "name: cancel\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/cancel/main.js", `require("node:fs").writeFileSync(process.env.READY, "ready"); setInterval(() => {}, 30000)
`)
	writeFixtureFile(t, workspace, ".github/actions/cancel/post.js", `require("node:fs").writeFileSync(process.env.POST_MARKER, "post")
`)
	ready, postMarker := filepath.Join(workspace, "ready"), filepath.Join(workspace, "post-ran")
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{
		{ID: "background", Kind: "uses", Uses: "./.github/actions/cancel", Background: true, Action: &plan.ActionSelector{Lock: lockID}},
		{ID: "await", Kind: "run", Shell: "sh", Command: `while [ ! -f "$READY" ]; do sleep .01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Env = map[string]string{"READY": ready, "POST_MARKER": postMarker}
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: ".github/actions/cancel", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/cancel"))}}
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node24: requireNode24(t), InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond, CleanupTimeout: 15 * time.Second}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("cancelled container JavaScript result = %#v, error = %v", result, err)
	}
	if contents, readErr := os.ReadFile(postMarker); readErr != nil || string(contents) != "post" {
		t.Fatalf("cancelled container JavaScript post = %q, %v", contents, readErr)
	}
	terminateCalls := 0
	for _, call := range f.calls(t) {
		if strings.Contains(strings.Join(call.Args, "\x00"), ContainerProcessHelperCommand+"\x00terminate") {
			terminateCalls++
		}
	}
	if terminateCalls != 1 {
		t.Fatalf("cancelled container JavaScript terminate calls = %d, want 1", terminateCalls)
	}
}

func TestValidateDockerMountFileRequiresExistingRegularExecutableSafeAbsoluteFile(t *testing.T) {
	d := t.TempDir()
	for _, p := range []string{d, "relative", filepath.Join(d, "missing")} {
		if validateDockerMountFile(p) == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	p := filepath.Join(d, "runtime")
	_ = os.WriteFile(p, nil, 0o600)
	if validateDockerMountFile(p) == nil {
		t.Fatal("accepted non-executable")
	}
	_ = os.Chmod(p, 0o700)
	if e := validateDockerMountFile(p); e != nil {
		t.Fatal(e)
	}
	bad := filepath.Join(d, "bad,name")
	_ = os.WriteFile(bad, nil, 0o700)
	if validateDockerMountFile(bad) == nil {
		t.Fatal("accepted mount-unsafe path")
	}
}

func liveContainerJob(t *testing.T, docker string, steps []plan.Step) JobResult {
	w := t.TempDir()
	j := jobContainerPlan(t, w, steps)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, e := (Runner{Docker: docker, RuntimeExecutable: buildLiveContainerRuntime(t), InterruptGrace: 100 * time.Millisecond, TerminateGrace: 100 * time.Millisecond}).RunJob(ctx, j, w)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func buildLiveContainerRuntime(t *testing.T) string {
	t.Helper()
	root := filepath.Dir(fixturePath(t))
	binary := filepath.Join(t.TempDir(), "buildkite-gha")
	command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, "./cmd/buildkite-gha")
	command.Dir = root
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build static container runtime: %v: %s", err, output)
	}
	return binary
}
func TestLiveJobContainerPersistsAcrossShellSteps(t *testing.T) {
	d := requireDocker(t)
	liveContainerJob(t, d, []plan.Step{{ID: "a", Kind: "run", Shell: "sh", Command: "touch persisted"}, {ID: "b", Kind: "run", Shell: "sh", Command: "test -f persisted"}})
}
func TestLiveJobContainerDefaultNonRootUser(t *testing.T) {
	d := requireDocker(t)
	w := t.TempDir()
	j := jobContainerPlan(t, w, []plan.Step{{ID: "u", Kind: "run", Shell: "sh", Command: `test "$(id -u)" != 0`}})
	j.Container.Image = "nginxinc/nginx-unprivileged:stable-alpine"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := (Runner{Docker: d, RuntimeExecutable: buildLiveContainerRuntime(t)}).RunJob(ctx, j, w); err != nil {
		t.Fatal(err)
	}
}
func TestLiveJobContainerCancellationKillsProcessTree(t *testing.T) {
	d := requireDocker(t)
	liveContainerJob(t, d, []plan.Step{
		{ID: "bg", Kind: "run", Shell: "sh", Background: true, Command: "sleep 30 & wait"},
		{ID: "cancel", Kind: "cancel", Targets: []string{"bg"}},
	})
}

func TestLiveJobContainerJavaScriptCompositeAndPost(t *testing.T) {
	docker := requireDocker(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: live container actions\n")
	writeFixtureFile(t, workspace, ".github/actions/js/action.yml", "name: js\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/js/main.js", `require("node:fs").appendFileSync(process.env.GITHUB_OUTPUT, "value=live\n")
`)
	writeFixtureFile(t, workspace, ".github/actions/js/post.js", `require("node:fs").writeFileSync(process.env.GITHUB_WORKSPACE + "/post-ran", "yes")
`)
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: composite
runs:
  using: composite
  steps:
    - shell: sh
      run: echo COMPOSITE_LIVE=yes >> "$GITHUB_ENV"
`)
	jsID, compositeID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{
		{ID: "js", Kind: "uses", Uses: "./.github/actions/js", Action: &plan.ActionSelector{Lock: jsID}},
		{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite", Action: &plan.ActionSelector{Lock: compositeID}},
		{ID: "verify", Kind: "run", Shell: "sh", Command: `test "$COMPOSITE_LIVE" = yes`},
	})
	job.Schema = plan.SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "node:24-bookworm-slim"}
	job.Actions = []plan.ActionLock{
		{ID: jsID, Source: "workspace", Path: ".github/actions/js", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/js"))},
		{ID: compositeID, Source: "workspace", Path: ".github/actions/composite", SourceDigest: digestTree(t, filepath.Join(workspace, ".github/actions/composite"))},
	}
	job.Outputs = map[string]string{"value": "${{ steps.js.outputs.value }}"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := (Runner{Docker: docker, RuntimeExecutable: buildLiveContainerRuntime(t), Node24: requireNode24(t)}).RunJob(ctx, job, workspace)
	if err != nil || result.Outputs["value"] != "live" || result.Env["COMPOSITE_LIVE"] != "yes" {
		t.Fatalf("live container actions result = %#v, error = %v", result, err)
	}
	if contents, readErr := os.ReadFile(filepath.Join(workspace, "post-ran")); readErr != nil || string(contents) != "yes" {
		t.Fatalf("live container post = %q, %v", contents, readErr)
	}
}
