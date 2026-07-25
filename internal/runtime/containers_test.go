package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	switch key {
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
		if _, err := os.Stat(container); err == nil {
			fmt.Print("container-id")
		}
		os.Exit(0)
	case "rm":
		_ = os.Remove(container)
		os.Exit(0)
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
		fakeJobDockerExec(root, args)
	default:
		os.Exit(46)
	}
}

func fakeJobDockerExec(root string, args []string) {
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
	if i+3 >= len(args) || args[i+1] != ContainerProcessHelperCommand {
		os.Exit(2)
	}
	helper := args[i+2:]
	translate := func(p string) string {
		for _, m := range []struct{ c, f string }{{jobContainerWorkspace, "mount-___w_repo_repo"}, {jobContainerTemp, "mount-___w__temp"}, {jobContainerRuntime, "mount-___buildkite-gha_runtime"}} {
			if strings.HasPrefix(p, m.c) {
				host, _ := os.ReadFile(filepath.Join(root, m.f))
				return filepath.Join(string(host), strings.TrimPrefix(p, m.c))
			}
		}
		return p
	}
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
	r, e := (Runner{Docker: docker, RuntimeExecutable: os.Args[0], InterruptGrace: 100 * time.Millisecond, TerminateGrace: 100 * time.Millisecond}).RunJob(ctx, j, w)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestLiveJobContainerPersistsAcrossShellSteps(t *testing.T) {
	d := requireDocker(t)
	liveContainerJob(t, d, []plan.Step{{ID: "a", Kind: "run", Shell: "sh", Command: "touch persisted"}, {ID: "b", Kind: "run", Shell: "sh", Command: "test -f persisted"}})
}
func TestLiveJobContainerDefaultNonRootUser(t *testing.T) {
	d := requireDocker(t)
	liveContainerJob(t, d, []plan.Step{{ID: "u", Kind: "run", Shell: "sh", Command: `test "$(id -u)" = 0`}})
}
func TestLiveJobContainerCancellationKillsProcessTree(t *testing.T) {
	d := requireDocker(t)
	liveContainerJob(t, d, []plan.Step{
		{ID: "bg", Kind: "run", Shell: "sh", Background: true, Command: "sleep 30 & wait"},
		{ID: "cancel", Kind: "cancel", Targets: []string{"bg"}},
	})
}
