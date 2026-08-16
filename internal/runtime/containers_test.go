//go:build linux && amd64

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/expression"
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
	// Scope fake state to this subprocess rather than the parent test process so
	// independent container tests can run in parallel. Avoid the race runtime's
	// one-second exit sleep on every fake Docker call.
	text := "#!/bin/sh\nGORACE=atexit_sleep_ms=0 JOB_DOCKER_ROOT=" + strconv.Quote(root) + " JOB_DOCKER_SCENARIO=" + strconv.Quote(scenario) + " exec " + strconv.Quote(exe) + " -test.run=^TestJobContainerFakeDockerProcess$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeJobDocker{wrapper, root}
}

func (f fakeJobDocker) calls(t *testing.T) []jobDockerCall {
	t.Helper()
	file, err := os.Open(filepath.Join(f.root, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := testFileLock(file, true); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(file)
	_ = testFileLock(file, false)
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

func testFileLock(file *os.File, lock bool) error {
	operation := syscall.LOCK_UN
	if lock {
		operation = syscall.LOCK_EX
	}
	return syscall.Flock(int(file.Fd()), operation)
}

func testExec(path string, args, env []string) error {
	return syscall.Exec(path, args, env)
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
	_ = testFileLock(f, true)
	_, _ = f.Write(b)
	_ = testFileLock(f, false)
	_ = f.Close()
	if len(args) == 0 {
		os.Exit(2)
	}
	scenario := os.Getenv("JOB_DOCKER_SCENARIO")
	key := args[0]
	if (key == "network" || key == "volume") && len(args) > 1 {
		key += "-" + args[1]
	}
	if scenario == "block-pull" && key == "pull" {
		select {}
	}
	if scenario == "fail-pull" && key == "pull" {
		os.Exit(42)
	}
	if scenario == "block-login" && key == "login" {
		select {}
	}
	container, network := filepath.Join(root, "container"), filepath.Join(root, "network")
	actionContainer, actionImage := filepath.Join(root, "action-container"), filepath.Join(root, "action-image")
	containerName := func(reference string) string {
		return strings.TrimPrefix(reference, "docker-id-")
	}
	containerPath := func(name string) string {
		name = containerName(name)
		if strings.HasPrefix(name, "buildkite-gha-service-") {
			return filepath.Join(root, "service-"+name)
		}
		return container
	}
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
	case "login":
		password, _ := io.ReadAll(os.Stdin)
		status := "bad"
		if string(password) == "registry-password\n" {
			status = "ok"
		}
		_ = os.WriteFile(filepath.Join(root, "login-stdin"), []byte(status), 0o600)
		if scenario == "fail-login-once" {
			marker := filepath.Join(root, "failed-login")
			if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
				_ = os.WriteFile(marker, nil, 0o600)
				os.Exit(42)
			}
		}
		os.Exit(0)
	case "logout":
		os.Exit(0)
	case "network-create":
		_ = os.WriteFile(network, nil, 0o600)
		if scenario == "fail-network-create" {
			os.Exit(42)
		}
		os.Exit(0)
	case "create":
		name := ""
		var publications []string
		var volumes []string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--name" {
				name = args[i+1]
			}
			if args[i] == "--publish" {
				publications = append(publications, args[i+1])
			}
			if args[i] == "--volume" || args[i] == "-v" {
				parts := strings.Split(args[i+1], ":")
				if len(parts) > 1 && parts[0] != "" && !filepath.IsAbs(parts[0]) {
					volumes = append(volumes, parts[0])
				}
			}
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
		_ = os.WriteFile(containerPath(name), nil, 0o600)
		if strings.HasPrefix(name, "buildkite-gha-service-") {
			_ = os.WriteFile(containerPath(name)+".ports", []byte(strings.Join(publications, "\n")), 0o600)
		}
		if len(volumes) != 0 {
			_ = os.WriteFile(filepath.Join(root, "volumes-"+name), []byte(strings.Join(volumes, "\n")), 0o600)
		}
		if scenario == "fail-later-service-create" && strings.HasPrefix(name, "buildkite-gha-service-") {
			counter := filepath.Join(root, "service-create-count")
			data, _ := os.ReadFile(counter)
			n, _ := strconv.Atoi(string(data))
			n++
			_ = os.WriteFile(counter, []byte(strconv.Itoa(n)), 0o600)
			if n == 2 {
				os.Exit(42)
			}
		}
		if scenario == "fail-create" {
			os.Exit(42)
		}
		fmt.Print("docker-id-" + name)
		os.Exit(0)
	case "start":
		if scenario == "fail-start" {
			os.Exit(42)
		}
		os.Exit(0)
	case "port":
		if scenario == "malformed-port" {
			fmt.Print("not Docker port output\n")
			os.Exit(0)
		}
		if scenario == "broad-port" {
			fmt.Print("80/tcp -> 0.0.0.0:8000\n81/tcp -> [::]:8001\n82/sctp -> 127.0.0.1:8002\n")
			os.Exit(0)
		}
		data, _ := os.ReadFile(containerPath(args[len(args)-1]) + ".ports")
		for _, publication := range strings.Split(string(data), "\n") {
			if publication == "" {
				continue
			}
			publication = strings.TrimPrefix(publication, "127.0.0.1:")
			parts := strings.SplitN(publication, "/", 2)
			proto := "tcp"
			if len(parts) == 2 {
				proto = parts[1]
			}
			mapping := strings.Split(parts[0], ":")
			containerPort, hostPort := mapping[len(mapping)-1], "49152"
			if len(mapping) > 1 && mapping[len(mapping)-2] != "" {
				hostPort = mapping[len(mapping)-2]
			}
			fmt.Printf("%s/%s -> 127.0.0.1:%s\n", containerPort, proto, hostPort)
		}
		os.Exit(0)
	case "inspect":
		name := containerName(args[len(args)-1])
		if _, err := os.Stat(containerPath(name)); err != nil {
			os.Exit(1)
		}
		if strings.Contains(strings.Join(args, " "), ".Mounts") {
			data, _ := os.ReadFile(filepath.Join(root, "volumes-"+name))
			fmt.Print(string(data))
			os.Exit(0)
		}
		switch scenario {
		case "service-unhealthy":
			fmt.Print("unhealthy\n")
		case "service-starting":
			counter := filepath.Join(root, "inspect-count")
			data, _ := os.ReadFile(counter)
			n, _ := strconv.Atoi(string(data))
			_ = os.WriteFile(counter, []byte(strconv.Itoa(n+1)), 0o600)
			if n == 0 {
				fmt.Print("starting\n")
			} else {
				fmt.Print("healthy\n")
			}
		default:
			fmt.Print("\n")
		}
		os.Exit(0)
	case "logs":
		if scenario == "service-log-command" {
			fmt.Print("::warning::not-an-annotation\n::add-mask::not-a-mask\nnot-a-mask\n")
			os.Exit(0)
		}
		fmt.Print("diagnostic sibling-secret\n")
		os.Exit(0)
	case "ps":
		if scenario == "query-fail" && strings.Contains(strings.Join(args, " "), "name=") {
			os.Exit(43)
		}
		joined := strings.Join(args, " ")
		owner, _ := os.ReadFile(filepath.Join(root, "action-owner"))
		isAction := strings.Contains(joined, "buildkite-gha-container-") || (len(owner) != 0 && strings.Contains(joined, string(owner)))
		path := container
		nameFiltered := false
		idFiltered := false
		for _, arg := range args {
			if strings.HasPrefix(arg, "name=^/buildkite-gha-service-") {
				name := strings.TrimSuffix(strings.TrimPrefix(arg, "name=^/"), "$")
				path = containerPath(name)
				nameFiltered = true
			}
			if strings.HasPrefix(arg, "id=docker-id-buildkite-gha-service-") {
				path = containerPath(strings.TrimPrefix(arg, "id="))
				nameFiltered = true
				idFiltered = true
			}
		}
		if isAction {
			path = actionContainer
		}
		if !nameFiltered && !isAction && strings.Contains(joined, "label=com.buildkite.gha.owner.") {
			if _, err := os.Stat(container); err == nil {
				fmt.Println("job-container-id")
			}
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "service-buildkite-gha-service-") && !strings.HasSuffix(entry.Name(), ".ports") {
					fmt.Println("docker-id-" + strings.TrimPrefix(entry.Name(), "service-"))
				}
			}
			os.Exit(0)
		}
		if _, err := os.Stat(path); err == nil {
			fmt.Print("container-id")
			if scenario == "service-auto-remove-before-stop" && idFiltered {
				_ = os.Remove(path)
			}
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
		if scenario == "service-auto-remove-before-stop" {
			if _, err := os.Stat(containerPath(args[len(args)-1])); errors.Is(err, os.ErrNotExist) {
				os.Exit(1)
			}
		}
		if scenario == "service-auto-remove" {
			_ = os.Remove(containerPath(args[len(args)-1]))
		}
		os.Exit(0)
	case "rm":
		joined := strings.Join(args, " ")
		path := containerPath(args[len(args)-1])
		if scenario == "service-auto-remove" && strings.Contains(args[len(args)-1], "buildkite-gha-service-") {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				os.Exit(1)
			}
		}
		if strings.Contains(joined, "buildkite-gha-container-") {
			_ = os.Remove(actionContainer)
		} else {
			_ = os.Remove(path)
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
	case "volume-ls":
		if data, err := os.ReadFile(filepath.Join(root, "existing-volumes")); err == nil {
			fmt.Print(string(data))
		}
		if scenario == "volume-leftover" {
			if data, err := os.ReadFile(filepath.Join(root, "removed-volumes")); err == nil {
				fmt.Print(string(data))
			}
		}
		os.Exit(0)
	case "volume-rm":
		_ = os.WriteFile(filepath.Join(root, "removed-volumes"), []byte(strings.Join(args[3:], "\n")), 0o600)
		os.Exit(0)
	case "volume-inspect":
		removed, _ := os.ReadFile(filepath.Join(root, "removed-volumes"))
		if slices.Contains(strings.Fields(string(removed)), args[len(args)-1]) {
			os.Exit(1)
		}
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
		if err := testExec("/bin/sh", args[i:], os.Environ()); err != nil {
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
		if err := testExec(nodeArgs[0], nodeArgs, os.Environ()); err != nil {
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
		// The fake container executes on the host, so resolve image PATH commands
		// before replacing the process environment with the simulated image PATH.
		if len(helper) > 2 && !filepath.IsAbs(helper[2]) {
			command, err := exec.LookPath(helper[2])
			if err != nil {
				os.Exit(126)
			}
			helper[2] = command
		}
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
	j.Schema = plan.Schema
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

func TestRunJobContainerDefaultsRunStepsToSh(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	j := jobContainerPlan(t, workspace, []plan.Step{{ID: "default", Kind: "run", Command: "true"}})
	if _, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, workspace); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls(t) {
		i := slices.Index(call.Args, ContainerProcessHelperCommand)
		if i < 0 || len(call.Args) < i+7 || call.Args[len(call.Args)-1] != "true" {
			continue
		}
		if got, want := call.Args[i+3:], []string{"sh", "-e", "-c", "true"}; !slices.Equal(got, want) {
			t.Fatalf("default container shell argv = %#v, want %#v", got, want)
		}
		return
	}
	t.Fatal("default-shell container exec call not found")
}

func TestRunJobContainerServicesLifecycleAndArguments(t *testing.T) {
	t.Parallel()

	f := newJobDocker(t, "")
	w := t.TempDir()
	j := jobContainerPlan(t, w, nil)
	j.Container.Ports = []string{"8080", "5432:5432", "9000:90/udp"}
	j.Services = map[string]plan.Container{
		"z-cache": {Image: "redis:7", Env: map[string]string{"Z": "last", "A": "first"}, Ports: j.Container.Ports},
		"a-db":    {Image: "postgres:16", Env: map[string]string{"B": "two"}},
	}
	j.ServiceOrder = []string{"z-cache", "a-db"}
	if _, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	var creates, starts, healthInspects []jobDockerCall
	for _, c := range calls {
		switch c.Args[0] {
		case "create":
			creates = append(creates, c)
		case "start":
			starts = append(starts, c)
		case "inspect":
			if strings.Contains(strings.Join(c.Args, " "), ".State.Health") {
				healthInspects = append(healthInspects, c)
			}
		}
	}
	if len(creates) != 3 || len(starts) != 3 || len(healthInspects) != 2 {
		t.Fatalf("unexpected service lifecycle: %#v", calls)
	}
	// Declared IDs retain their association with aliases/images, and all service
	// creates and starts precede readiness inspection.
	if strings.Join(creates[0].Args, " ") == "" || !strings.Contains(strings.Join(creates[0].Args, " "), "--network-alias z-cache") || creates[0].Args[len(creates[0].Args)-1] != "redis:7" || !strings.Contains(strings.Join(creates[1].Args, " "), "--network-alias a-db") || creates[1].Args[len(creates[1].Args)-1] != "postgres:16" {
		t.Fatalf("service order/association: %#v", creates)
	}
	firstInspect := jobDockerCallIndex(calls, "inspect", "--format", `{{if .State.Health}}{{.State.Health.Status}}{{end}}`)
	startsBeforeInspect := 0
	for i := 0; i < firstInspect; i++ {
		if calls[i].Args[0] == "start" {
			startsBeforeInspect++
		}
	}
	if startsBeforeInspect != 2 {
		t.Fatalf("only %d service starts preceded readiness", startsBeforeInspect)
	}
	serviceArgs := strings.Join(creates[0].Args, " ")
	jobArgs := strings.Join(creates[2].Args, " ")
	for _, want := range []string{"--env A=first --env Z=last", "--publish 8080", "--publish 5432:5432", "--publish 9000:90/udp"} {
		if !strings.Contains(serviceArgs, want) && strings.HasPrefix(want, "--env") || (!strings.HasPrefix(want, "--env") && (!strings.Contains(serviceArgs, want) || !strings.Contains(jobArgs, want))) {
			t.Errorf("missing %q: service=%q job=%q", want, serviceArgs, jobArgs)
		}
	}
	network := creates[0].Args[slices.Index(creates[0].Args, "--network")+1]
	if !strings.Contains(serviceArgs, "--network "+network) || !strings.Contains(jobArgs, "--network "+network) {
		t.Error("containers do not share exact network")
	}
	forbidden := []string{"--privileged", "docker.sock", "--device", "--network host", "--pid host", "--user", "--volume"}
	for _, create := range creates[:2] {
		joined := strings.Join(create.Args, " ")
		for _, bad := range forbidden {
			if strings.Contains(joined, bad) {
				t.Errorf("service introduced forbidden argument %q", bad)
			}
		}
	}
	// Successful cleanup is job first, then services in declaration order,
	// network, and verification.
	joined := fmt.Sprint(calls)
	arm, zrm := strings.Index(joined, "rm --force --volumes docker-id-"+creates[0].Args[2]), strings.Index(joined, "rm --force --volumes docker-id-"+creates[1].Args[2])
	jobrm, netrm := strings.Index(joined, "rm --force --volumes "+creates[2].Args[2]), strings.Index(joined, "network rm")
	if jobrm < 0 || jobrm >= arm || arm >= zrm || zrm >= netrm {
		t.Fatalf("cleanup order: %s", joined)
	}
}

func TestRunServiceContainerAutoRemoveCleanup(t *testing.T) {
	f := newJobDocker(t, "service-auto-remove")
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "postgres:16", Options: "--rm"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cleanup(); err != nil {
		t.Fatal(err)
	}
	createdWithAutoRemove, queriedByID := false, false
	for _, call := range f.calls(t) {
		if len(call.Args) != 0 && call.Args[0] == "create" && slices.Contains(call.Args, "--rm") {
			createdWithAutoRemove = true
		}
		if len(call.Args) != 0 && call.Args[0] == "ps" && slices.ContainsFunc(call.Args, func(arg string) bool { return strings.HasPrefix(arg, "id=docker-id-buildkite-gha-service-") }) {
			queriedByID = true
		}
		if len(call.Args) != 0 && call.Args[0] == "rm" && strings.Contains(call.Args[len(call.Args)-1], "buildkite-gha-service-") {
			t.Fatalf("cleanup removed auto-removed service: %#v", call.Args)
		}
	}
	if !createdWithAutoRemove || !queriedByID {
		t.Fatalf("created with --rm = %t, queried by ID = %t", createdWithAutoRemove, queriedByID)
	}
}

func TestRunServiceContainerAlreadyAutoRemovedCleanup(t *testing.T) {
	f := newJobDocker(t, "")
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "postgres:16", Options: "--rm"}})
	if err != nil {
		t.Fatal(err)
	}
	serviceID := b.services[0].name
	path := filepath.Join(f.root, "service-"+strings.TrimPrefix(serviceID, "docker-id-"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := b.cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls(t) {
		if len(call.Args) != 0 && call.Args[0] == "stop" && call.Args[len(call.Args)-1] == serviceID {
			t.Fatalf("cleanup stopped already removed service: %#v", call.Args)
		}
	}
}

func TestRunServiceContainerAutoRemoveBetweenQueryAndStop(t *testing.T) {
	f := newJobDocker(t, "service-auto-remove-before-stop")
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "postgres:16", Options: "--rm"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cleanup(); err != nil {
		t.Fatal(err)
	}
	if jobDockerCallIndex(f.calls(t), "stop", "--time", "2", b.services[0].name) < 0 {
		t.Fatal("cleanup did not exercise stop race")
	}
}

func TestDockerArgumentListMatchesRunnerQuoting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, value string
		want        []string
	}{
		{name: "empty", value: `--env ""`, want: []string{"--env", ""}},
		{name: "double quotes", value: `--health-cmd "pg_isready -U postgres"`, want: []string{"--health-cmd", "pg_isready -U postgres"}},
		{name: "single quotes literal", value: `--label 'two words'`, want: []string{"--label", "'two", "words'"}},
		{name: "escaped quote", value: `one\"two`, want: []string{`one"two`}},
		{name: "unmatched quote", value: `"two words`, want: []string{"two words"}},
		{name: "newline literal", value: "one\ntwo", want: []string{"one\ntwo"}},
		{name: "consecutive quotes", value: `"one""two"`, want: []string{`one"two`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := dockerArgumentList(test.value)
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("dockerArgumentList(%q) = %#v, %v; want %#v", test.value, got, err, test.want)
			}
		})
	}
}

func TestServiceOptionsRejectOnlyNetworkOverride(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		options []string
		wantErr bool
	}{
		{options: []string{"--network", "host"}, wantErr: true},
		{options: []string{"--network=host"}, wantErr: true},
		{options: []string{"--net", "host"}, wantErr: true},
		{options: []string{"--net=host"}, wantErr: true},
		{options: []string{"--networking=allowed"}},
		{options: []string{"--privileged", "--cpus", "2"}},
	} {
		if err := validateServiceOptions(test.options); (err != nil) != test.wantErr {
			t.Errorf("validateServiceOptions(%q) = %v", test.options, err)
		}
	}
}

func TestRunServiceContainerCompleteArguments(t *testing.T) {
	f := newJobDocker(t, "")
	w, tmp := t.TempDir(), t.TempDir()
	service := plan.Container{
		Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "test"}, Ports: []string{"5432"},
		Volumes: []string{"database:/var/lib/postgresql/data:ro"},
		Options: `--health-cmd "pg_isready -U postgres" --health-retries 5`,
		Command: `postgres -c "fsync = off"`, Entrypoint: "docker-entrypoint.sh",
	}
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(os.Stdout, os.Stderr), w, tmp, plan.Container{}, map[string]plan.Container{"database": service})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls(t) {
		if len(call.Args) == 0 || call.Args[0] != "create" {
			continue
		}
		want := []string{"--health-cmd", "pg_isready -U postgres", "--health-retries", "5", "--env", "POSTGRES_PASSWORD=test", "--volume", "database:/var/lib/postgresql/data:ro", "--entrypoint", "docker-entrypoint.sh", "postgres:16", "postgres", "-c", "fsync = off"}
		joined := strings.Join(call.Args, "\x00")
		if !strings.Contains(joined, strings.Join(want, "\x00")) {
			t.Fatalf("service create argv = %#v; missing ordered %#v", call.Args, want)
		}
		if err := b.cleanup(); err != nil {
			t.Fatal(err)
		}
		removed, err := os.ReadFile(filepath.Join(f.root, "removed-volumes"))
		if err != nil || strings.TrimSpace(string(removed)) != "database" {
			t.Fatalf("removed volumes = %q, %v", removed, err)
		}
		return
	}
	t.Fatal("service create call not found")
}

func TestRunServiceContainersUseDeclaredOrderAndEmitSuccessfulLogs(t *testing.T) {
	f := newJobDocker(t, "service-log-command")
	var output bytes.Buffer
	processor := newCommandProcessor(&output, &output)
	b, err := (Runner{Docker: f.path}).startJobContainerOrdered(
		context.Background(), processor, t.TempDir(), t.TempDir(), plan.Container{},
		map[string]plan.Container{"alpha": {Image: "one"}, "zed": {Image: "two"}}, []string{"zed", "alpha"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cleanup(); err != nil {
		t.Fatal(err)
	}
	var aliases, removed []string
	for _, call := range f.calls(t) {
		if len(call.Args) != 0 && call.Args[0] == "create" {
			aliases = append(aliases, call.Args[slices.Index(call.Args, "--network-alias")+1])
		}
		if len(call.Args) != 0 && call.Args[0] == "rm" && strings.Contains(call.Args[len(call.Args)-1], "docker-id-buildkite-gha-service-") {
			removed = append(removed, call.Args[len(call.Args)-1])
		}
	}
	if !slices.Equal(aliases, []string{"zed", "alpha"}) || len(removed) != 2 || removed[0] != b.services[0].name || removed[1] != b.services[1].name {
		t.Fatalf("aliases = %#v, removed = %#v, services = %#v", aliases, removed, b.services)
	}
	if calls := f.calls(t); jobDockerCallIndex(calls, "logs", "--tail", "200", b.services[0].name) < 0 || jobDockerCallIndex(calls, "logs", "--tail", "200", b.services[1].name) < 0 {
		t.Fatalf("successful service logs were not emitted: %#v", calls)
	}
	if !strings.Contains(output.String(), "::warning::not-an-annotation") || !strings.Contains(output.String(), "::add-mask::not-a-mask\nnot-a-mask") || len(processor.warnings.commands) != 0 {
		t.Fatalf("service logs were interpreted as workflow commands: output = %q, warnings = %#v", output.String(), processor.warnings.commands)
	}
}

func TestRunServiceContainerRegistryCredentialsUsePasswordStdin(t *testing.T) {
	f := newJobDocker(t, "fail-login-once")
	service := plan.Container{Image: "registry.example.test/team/postgres:16", Credentials: &plan.ContainerCredentials{Username: "registry-user", Password: "registry-password"}}
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": service})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.cleanup() })
	calls := f.calls(t)
	login := jobDockerCallIndex(calls, "login", "registry.example.test", "--username", "registry-user", "--password-stdin")
	pull := jobDockerCallIndex(calls, "pull", service.Image)
	if login < 0 || pull < login {
		t.Fatalf("login/pull order = %#v", calls)
	}
	logins := 0
	for _, call := range calls {
		if len(call.Args) != 0 && call.Args[0] == "login" {
			logins++
		}
		if slices.Contains(call.Args, "registry-password") {
			t.Fatalf("password appeared in Docker argv: %#v", call.Args)
		}
	}
	if logins != 2 {
		t.Fatalf("login attempts = %d, calls = %#v", logins, calls)
	}
	status, err := os.ReadFile(filepath.Join(f.root, "login-stdin"))
	if err != nil || string(status) != "ok" {
		t.Fatalf("password stdin status = %q, %v", status, err)
	}
}

func TestDockerLoginCancellationStopsRetry(t *testing.T) {
	f := newJobDocker(t, "block-login")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := dockerLogin(ctx, map[string]string{"DOCKER_CONFIG": t.TempDir()}, f.path, "registry.example.test", "registry-user", "registry-password")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("error = %v, elapsed = %s", err, time.Since(start))
	}
}

func TestDockerRegistryMatchesRunnerInference(t *testing.T) {
	for image, want := range map[string]string{
		"alpine":                       "",
		"library/alpine":               "",
		"localhost/alpine":             "",
		"localhost:5000/alpine":        "localhost:5000",
		"registry.example/team/alpine": "registry.example",
		"host/team/alpine":             "host",
	} {
		if got := dockerRegistry(image); got != want {
			t.Errorf("dockerRegistry(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestRunServiceContainerPullsSameImagePerCredentialIdentity(t *testing.T) {
	f := newJobDocker(t, "")
	image := "registry.example/team/postgres:16"
	services := map[string]plan.Container{
		"a": {Image: image, Credentials: &plan.ContainerCredentials{Username: "user-a", Password: "password-a"}},
		"b": {Image: image, Credentials: &plan.ContainerCredentials{Username: "user-b", Password: "password-b"}},
	}
	b, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).startJobContainer(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), t.TempDir(), plan.Container{Image: image}, services)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.cleanup() })
	logins, logouts, pulls := 0, 0, 0
	for _, call := range f.calls(t) {
		if len(call.Args) != 0 && call.Args[0] == "login" {
			logins++
		}
		if slices.Equal(call.Args, []string{"logout", "registry.example"}) {
			logouts++
		}
		if slices.Equal(call.Args, []string{"pull", image}) {
			pulls++
		}
	}
	if logins != 2 || logouts != 1 || pulls != 3 {
		t.Fatalf("logins = %d, logouts = %d, pulls = %d, calls = %#v", logins, logouts, pulls, f.calls(t))
	}
}

func TestRunServiceContainerPartialRegistryCredentialsDoNotLogin(t *testing.T) {
	for _, credentials := range []*plan.ContainerCredentials{{Username: "registry-user"}, {Password: "registry-password"}} {
		f := newJobDocker(t, "")
		b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "registry.example.test/postgres:16", Credentials: credentials}})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.cleanup(); err != nil {
			t.Fatal(err)
		}
		if jobDockerCallIndex(f.calls(t), "login") >= 0 {
			t.Fatalf("partial credentials triggered login: %#v", f.calls(t))
		}
	}
}

func TestRemoveDockerConfigReportsCleanupFailure(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDockerConfig(filepath.Join(parent, "config")); err == nil || !strings.Contains(err.Error(), "private Docker configuration") {
		t.Fatalf("removeDockerConfig() error = %v", err)
	}
}

func TestEvaluateServicesResolvesNeedsAndSkipsEmptyImages(t *testing.T) {
	services := map[string]plan.Container{
		"database": {
			Image:      "postgres:${{ needs.build.outputs.version }}",
			Env:        map[string]string{"SOURCE": "${{ needs.build.outputs.source }}"},
			Ports:      []string{"${{ needs.build.outputs.port }}"},
			Volumes:    []string{"data:${{ needs.build.outputs.target }}"},
			Options:    "--label version=${{ needs.build.outputs.version }}",
			Command:    "postgres -c ${{ needs.build.outputs.setting }}",
			Entrypoint: "${{ needs.build.outputs.entrypoint }}",
		},
		"optional": {Image: "${{ needs.build.outputs.optional }}"},
	}
	got, err := evaluateServices(services, expression.Context{Needs: map[string]map[string]string{"build": {
		"version": "16", "source": "runtime", "port": "5432", "target": "/data", "setting": "fsync=off", "entrypoint": "docker-entrypoint.sh", "optional": "",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	service := got["database"]
	if len(got) != 1 || service.Image != "postgres:16" || service.Env["SOURCE"] != "runtime" || service.Ports[0] != "5432" || service.Volumes[0] != "data:/data" || service.Options != "--label version=16" || service.Command != "postgres -c fsync=off" || service.Entrypoint != "docker-entrypoint.sh" {
		t.Fatalf("evaluated services = %#v", got)
	}
	if services["database"].Image != "postgres:${{ needs.build.outputs.version }}" {
		t.Fatal("service evaluation mutated the plan")
	}
}

func TestEvaluateServiceMapExpression(t *testing.T) {
	eval := expression.Context{Needs: map[string]map[string]string{"build": {"services": `{"database":{"image":"postgres:16","env":{"MODE":"test","RETRIES":3.0,"NEGATIVE_ZERO":-0,"ENABLED":true},"ports":[5.432e3],"volumes":[2],"options":1e20,"command":1e2,"entrypoint":false},"cache":"redis:7"}`}}}
	got, order, err := evaluateServiceMap(nil, nil, "${{ fromJSON(needs.build.outputs.services) }}", eval)
	if err != nil {
		t.Fatal(err)
	}
	database := got["database"]
	if len(got) != 2 || database.Image != "postgres:16" || database.Env["MODE"] != "test" || database.Env["RETRIES"] != "3" || database.Env["NEGATIVE_ZERO"] != "0" || database.Env["ENABLED"] != "true" || database.Ports[0] != "5432" || database.Volumes[0] != "2" || database.Options != "1E+20" || database.Command != "100" || database.Entrypoint != "false" || got["cache"].Image != "redis:7" || !slices.Equal(order, []string{"database", "cache"}) {
		t.Fatalf("evaluated services = %#v, order = %#v", got, order)
	}
}

func TestEvaluateServiceMapExpressionRejectsUnsafeShapes(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
	}{
		{name: "array", value: `[]`, want: "want an object"},
		{name: "null service", value: `{"db":null}`, want: "want an object"},
		{name: "unknown field", value: `{"db":{"image":"postgres:16","privileged":true}}`, want: "unknown field"},
		{name: "uppercase field", value: `{"db":{"Image":"postgres:16"}}`, want: "unknown field"},
		{name: "null environment", value: `{"db":{"image":"postgres:16","env":null}}`, want: "want an object"},
		{name: "null ports", value: `{"db":{"image":"postgres:16","ports":null}}`, want: "want an array"},
		{name: "null volumes", value: `{"db":{"image":"postgres:16","volumes":null}}`, want: "want an array"},
		{name: "invalid service name", value: `{"UPPER":"postgres:16"}`, want: "service name"},
		{name: "invalid image", value: `{"db":"BAD IMAGE"}`, want: "invalid image"},
		{name: "credentials", value: `{"db":{"image":"postgres:16","credentials":{"username":"user","password":"secret"}}}`, want: "cannot introduce registry credentials"},
		{name: "null credentials", value: `{"db":{"image":"postgres:16","credentials":null}}`, want: "cannot introduce registry credentials"},
		{name: "nested expression", value: `{"db":{"image":"redis:7","options":"--label token=${{ secrets.TOKEN }}"}}`, want: "retains a runtime template"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := evaluateServiceMap(nil, nil, "${{ fromJSON(needs.build.outputs.services) }}", expression.Context{Needs: map[string]map[string]string{"build": {"services": test.value}}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEvaluateServicesDoesNotHideErrorsBehindEmptyImage(t *testing.T) {
	_, err := evaluateServices(map[string]plan.Container{"optional": {Image: "${{ needs.build.outputs.image }}", Env: map[string]string{"VALUE": "${{ needs.build.outputs.value"}}}, expression.Context{Needs: map[string]map[string]string{"build": {"image": ""}}})
	if err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("error = %v, want unterminated expression error", err)
	}
}

func TestRunServiceContainerRemovesOnlyNewNamedVolumes(t *testing.T) {
	for _, test := range []struct {
		name, volume, existing, wantRemoved string
	}{
		{name: "new named volume", volume: "database:/data", wantRemoved: "database"},
		{name: "existing named volume", volume: "database:/data", existing: "database\n"},
		{name: "bind mount", volume: "/srv/database:/data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newJobDocker(t, "")
			if test.existing != "" {
				if err := os.WriteFile(filepath.Join(f.root, "existing-volumes"), []byte(test.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(os.Stdout, os.Stderr), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "postgres:16", Volumes: []string{test.volume}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.cleanup(); err != nil {
				t.Fatal(err)
			}
			removed, err := os.ReadFile(filepath.Join(f.root, "removed-volumes"))
			if test.wantRemoved == "" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected volume removal %q, %v", removed, err)
			}
			if test.wantRemoved != "" && (err != nil || strings.TrimSpace(string(removed)) != test.wantRemoved) {
				t.Fatalf("removed volumes = %q, %v; want %q", removed, err, test.wantRemoved)
			}
		})
	}
}

func TestRunServiceContainerReportsLeftoverNamedVolume(t *testing.T) {
	f := newJobDocker(t, "volume-leftover")
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(os.Stdout, os.Stderr), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "postgres:16", Volumes: []string{"database:/data"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cleanup(); err == nil || !strings.Contains(err.Error(), `leftover volume "database"`) {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestRunServiceContainerReadsBroadDockerPortOutput(t *testing.T) {
	f := newJobDocker(t, "broad-port")
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(os.Stdout, os.Stderr), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "postgres:16", Ports: []string{"8000-8001:80-81"}, Options: "--publish 8002:82/sctp"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.cleanup() })
	want := map[string]string{"80": "8000", "81": "8001", "82": "8002"}
	if !maps.Equal(b.servicePorts["database"].Ports, want) {
		t.Fatalf("service ports = %#v, want %#v", b.servicePorts["database"], want)
	}
}

func TestRunServiceContainerOptionNameUsesCreatedReference(t *testing.T) {
	f := newJobDocker(t, "")
	b, err := (Runner{Docker: f.path}).startJobContainer(context.Background(), newCommandProcessor(os.Stdout, os.Stderr), t.TempDir(), t.TempDir(), plan.Container{}, map[string]plan.Container{"database": {Image: "postgres:16", Options: "--name custom-service"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cleanup(); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	if jobDockerCallIndex(calls, "start", "docker-id-custom-service") < 0 || jobDockerCallIndex(calls, "rm", "--force", "--volumes", "docker-id-custom-service") < 0 {
		t.Fatalf("custom service reference was not used for lifecycle: %#v", calls)
	}
}

func TestRunJobContainerServiceFailureDiagnosticsAreMasked(t *testing.T) {
	f := newJobDocker(t, "service-unhealthy")
	w, tmp := t.TempDir(), t.TempDir()
	var output bytes.Buffer
	p := newCommandProcessor(&output, &output)
	p.addMask("sibling-secret")
	_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).startJobContainer(context.Background(), p, w, tmp, plan.Container{Image: "alpine"}, map[string]plan.Container{"db": {Image: "postgres"}})
	if err == nil || !strings.Contains(err.Error(), `service "db"`) || !strings.Contains(err.Error(), `status "unhealthy"`) {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(output.String(), "sibling-secret") || !strings.Contains(output.String(), "***") || jobDockerCallIndex(f.calls(t), "logs", "--tail", "200") < 0 {
		t.Fatalf("unmasked or missing diagnostics: %q", output.String())
	}
}

func TestRunJobContinueOnErrorToleratesServiceStartupFailure(t *testing.T) {
	f := newJobDocker(t, "service-unhealthy")
	w := t.TempDir()
	job := jobContainerPlan(t, w, nil)
	job.Services = map[string]plan.Container{"db": {Image: "postgres"}}
	job.ServiceOrder = []string{"db"}
	job.ContinueOnError = true

	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), job, w)
	if err == nil || !IsToleratedJobFailure(err) || result.Conclusion != "success" || !strings.Contains(err.Error(), `service "db"`) {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunJobContinueOnErrorDoesNotTolerateServiceStartupTimeout(t *testing.T) {
	f := newJobDocker(t, "service-starting")
	w := t.TempDir()
	job := jobContainerPlan(t, w, nil)
	job.Services = map[string]plan.Container{"db": {Image: "postgres"}}
	job.ServiceOrder = []string{"db"}
	job.ContinueOnError = true
	job.TimeoutMinutes = 0.001

	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), job, w)
	if !errors.Is(err, context.DeadlineExceeded) || IsToleratedJobFailure(err) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunJobContinueOnErrorDoesNotTolerateMalformedServicePortEvidence(t *testing.T) {
	f := newJobDocker(t, "malformed-port")
	w := t.TempDir()
	job := jobContainerPlan(t, w, nil)
	job.Services = map[string]plan.Container{"db": {Image: "postgres", Ports: []string{"6379"}}}
	job.ServiceOrder = []string{"db"}
	job.ContinueOnError = true

	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), job, w)
	if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "malformed Docker port output") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestRunJobContainerMalformedServicePortsIncludeMaskedDiagnostics(t *testing.T) {
	f := newJobDocker(t, "malformed-port")
	w, tmp := t.TempDir(), t.TempDir()
	var output bytes.Buffer
	p := newCommandProcessor(&output, &output)
	p.addMask("sibling-secret")
	_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).startJobContainer(context.Background(), p, w, tmp, plan.Container{}, map[string]plan.Container{"db": {Image: "postgres", Ports: []string{"6379"}}})
	if err == nil || !strings.Contains(err.Error(), `service "db" has malformed Docker port output`) {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(output.String(), "sibling-secret") || !strings.Contains(output.String(), "***") || jobDockerCallIndex(f.calls(t), "logs", "--tail", "200") < 0 {
		t.Fatalf("unmasked or missing diagnostics: %q", output.String())
	}
}

func TestRunJobContainerLaterServiceCreateFailureCleansExactServices(t *testing.T) {
	f := newJobDocker(t, "fail-later-service-create")
	w, tmp := t.TempDir(), t.TempDir()
	_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).startJobContainer(context.Background(), newCommandProcessor(os.Stdout, os.Stderr), w, tmp, plan.Container{Image: "alpine"}, map[string]plan.Container{"a": {Image: "one"}, "b": {Image: "two"}})
	if err == nil || !strings.Contains(err.Error(), `create service "b"`) {
		t.Fatalf("error=%v, want second service create failure", err)
	}
	calls := f.calls(t)
	creates, removes := 0, 0
	for _, c := range calls {
		if c.Args[0] == "create" {
			creates++
		}
		if c.Args[0] == "rm" && strings.Contains(c.Args[len(c.Args)-1], "buildkite-gha-service-") {
			removes++
		}
	}
	if creates != 2 || removes != 2 {
		t.Fatalf("ambiguous create cleanup incomplete: %#v", calls)
	}
}

func TestRunJobContainerServiceReadinessCancellationCleansEverything(t *testing.T) {
	t.Parallel()

	f := newJobDocker(t, "service-starting")
	w, tmp := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).startJobContainer(ctx, newCommandProcessor(os.Stdout, os.Stderr), w, tmp, plan.Container{Image: "alpine"}, map[string]plan.Container{"db": {Image: "postgres"}})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for jobDockerCallIndex(f.calls(t), "inspect", "--format", `{{if .State.Health}}{{.State.Health.Status}}{{end}}`) < 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	joined := fmt.Sprint(f.calls(t))
	if !strings.Contains(joined, "rm --force --volumes docker-id-buildkite-gha-service-") || !strings.Contains(joined, "network rm buildkite-gha-network-") {
		t.Fatalf("incomplete cancellation cleanup: %s", joined)
	}
}

func TestRunHostJobServiceReadinessCancellationCleansEverything(t *testing.T) {
	f := newJobDocker(t, "service-starting")
	w, tmp := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (Runner{Docker: f.path}).startJobContainer(ctx, newCommandProcessor(os.Stdout, os.Stderr), w, tmp, plan.Container{}, map[string]plan.Container{"db": {Image: "postgres"}})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for jobDockerCallIndex(f.calls(t), "inspect", "--format", `{{if .State.Health}}{{.State.Health.Status}}{{end}}`) < 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	joined := fmt.Sprint(f.calls(t))
	if !strings.Contains(joined, "rm --force --volumes docker-id-buildkite-gha-service-") || !strings.Contains(joined, "network rm buildkite-gha-network-") || strings.Contains(joined, "rm --force --volumes buildkite-gha-job-") {
		t.Fatalf("incomplete or over-broad host-service cancellation cleanup: %s", joined)
	}
}

func TestJobContainerCleanupBudgetScalesToMaximumServices(t *testing.T) {
	if got, want := jobContainerCleanupTimeout(10*time.Second, 32), 106*time.Second; got != want {
		t.Fatalf("cleanup timeout = %s, want %s", got, want)
	}
}

func TestRunJobHostServicesLifecycle(t *testing.T) {
	f := newJobDocker(t, "")
	w := t.TempDir()
	j := jobContainerPlan(t, w, nil)
	j.Container = nil
	j.Services = map[string]plan.Container{"db": {Image: "postgres", Ports: []string{"6379"}}}
	j.ServiceOrder = []string{"db"}
	j.Steps = []plan.Step{{ID: "host", Kind: "run", Shell: "sh", Env: map[string]string{"SERVICE_PORT": "${{ job.services.db.ports[6379] }}"}, Command: `test "$SERVICE_PORT" = 49152`}}
	if _, err := (Runner{Docker: f.path}).RunJob(context.Background(), j, w); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls(t) {
		if call.Args[0] == "exec" {
			t.Fatalf("host service job used persistent container exec: %#v", call.Args)
		}
	}
}

func TestRunHostJobServicesExposePortsAndNetworkToDockerActions(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := fixturePath(t)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{
		{ID: "host", Kind: "run", Shell: "sh", Env: map[string]string{"SERVICE_PORT": "${{ job.services.redis.ports[6379] }}"}, Command: `test "$SERVICE_PORT" = 49152`},
		{ID: "docker", Kind: "uses", Uses: "./actions/docker", With: map[string]string{"expected_file": "${{ job.services.redis.ports[6379] }}"}, Env: map[string]string{"SERVICE_PORT": "${{ job.services.redis.ports['6379'] }}"}, Action: &plan.ActionSelector{Lock: lockID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Services = map[string]plan.Container{"redis": {Image: "redis:7", Ports: []string{"6379"}}}
	job.ServiceOrder = []string{"redis"}
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: "actions/docker", SourceDigest: digestTree(t, filepath.Join(workspace, "actions/docker"))}}
	if _, err := (Runner{Docker: f.path}).RunJob(context.Background(), job, workspace); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	if jobDockerCallIndex(calls, "exec") >= 0 {
		t.Fatalf("host-service job unexpectedly used persistent container exec: %#v", calls)
	}
	create, run := jobDockerCallIndex(calls, "create"), jobDockerCallIndex(calls, "run")
	if create < 0 || run < 0 {
		t.Fatalf("host-service Docker action calls absent: %#v", calls)
	}
	network := argumentAfter(t, calls[create].Args, "--network")
	if got := argumentAfter(t, calls[run].Args, "--network"); got != network {
		t.Fatalf("Docker action network = %q, want %q", got, network)
	}
	joined := strings.Join(calls[run].Args, " ")
	for _, want := range []string{"--env INPUT_EXPECTED_FILE=49152", "--env SERVICE_PORT=49152"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Docker action did not receive service expression %q: %#v", want, calls[run].Args)
		}
	}
	if jobDockerCallIndex(calls, "network", "rm") < run {
		t.Fatalf("host-service network removed before Docker action: %#v", calls)
	}
}

func TestRunJobHostServicePortProtocolCollisionIsDeterministic(t *testing.T) {
	f := newJobDocker(t, "")
	w := t.TempDir()
	j := jobContainerPlan(t, w, nil)
	j.Container = nil
	j.Services = map[string]plan.Container{"db": {Image: "postgres", Ports: []string{"41001:6379/tcp", "41002:6379/udp"}}}
	j.ServiceOrder = []string{"db"}
	j.Steps = []plan.Step{{ID: "host", Kind: "run", Shell: "sh", Env: map[string]string{"SERVICE_PORT": "${{ job.services.db.ports[6379] }}"}, Command: `test "$SERVICE_PORT" = 41002`}}
	if _, err := (Runner{Docker: f.path}).RunJob(context.Background(), j, w); err != nil {
		t.Fatal(err)
	}
}

func TestRunJobContainerSetupFailuresCleanOwnedResources(t *testing.T) {
	t.Parallel()

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
				removedContainer = removedContainer || strings.HasPrefix(joined, "rm --force --volumes buildkite-gha-job-")
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
	if !strings.Contains(joined, "rm --force --volumes buildkite-gha-job-") || !strings.Contains(joined, "network rm buildkite-gha-network-") {
		t.Fatalf("cleanup %s", joined)
	}
}

func TestRunJobContainerReportsLeftoverAndVerificationFailure(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"leftover", "verify-fail"} {
		t.Run(s, func(t *testing.T) {
			f := newJobDocker(t, s)
			w := t.TempDir()
			j := jobContainerPlan(t, w, nil)
			j.ContinueOnError = true
			result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w)
			if err == nil || IsToleratedJobFailure(err) || result.Conclusion != "failure" || !strings.Contains(err.Error(), "verify owned Docker cleanup") {
				t.Fatalf("result=%#v, error=%v", result, err)
			}
		})
	}
}

func TestRunJobContainerToleratesWorkflowFailureAfterSuccessfulCleanup(t *testing.T) {
	f := newJobDocker(t, "")
	w := t.TempDir()
	j := jobContainerPlan(t, w, []plan.Step{{ID: "fail", Kind: "run", Shell: "sh", Command: "exit 7"}})
	j.ContinueOnError = true
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w)
	if err == nil || !IsToleratedJobFailure(err) || result.Conclusion != "success" {
		t.Fatalf("result=%#v, error=%v", result, err)
	}
}

func startTestBackend(t *testing.T, f fakeJobDocker) (*jobContainerBackend, string) {
	w := t.TempDir()
	tmp := t.TempDir()
	b, e := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond}).startJobContainer(context.Background(), newCommandProcessor(os.Stdout, os.Stderr), w, tmp, plan.Container{Image: "alpine:3.20"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	return b, w
}

func TestRunJobContainerCancellationTargetsProcessTree(t *testing.T) {
	t.Parallel()

	f := newJobDocker(t, "")
	b, w := startTestBackend(t, f)
	t.Cleanup(func() { _ = b.cleanup() })
	marker := filepath.Join(w, "alive")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- b.exec(ctx, b.runner, newCommandProcessor(os.Stdout, os.Stderr), w, map[string]string{}, "sh", "-c", `trap '' TERM INT; sleep 30 & echo $! > alive; wait`)
	}()
	var pidb []byte
	deadline := time.Now().Add(2 * time.Second)
	for len(pidb) == 0 && time.Now().Before(deadline) {
		pidb, _ = os.ReadFile(marker)
		time.Sleep(time.Millisecond)
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(pidb)))
	if err != nil || p <= 0 {
		t.Fatalf("child PID was not published: %q, %v", pidb, err)
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("%v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation hung")
	}
	deadline = time.Now().Add(2 * time.Second)
	for testProcessExists(p) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if testProcessExists(p) {
		t.Fatalf("child %d survived cancellation", p)
	}
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
	t.Parallel()

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
	t.Parallel()

	f := newJobDocker(t, "")
	w := t.TempDir()
	j := jobContainerPlan(t, w, []plan.Step{{ID: "bg", Kind: "run", Shell: "sh", Background: true, Command: `/bin/sleep .15; echo LATE=yes >> "$GITHUB_ENV"; echo value=x >> "$GITHUB_OUTPUT"; echo /late >> "$GITHUB_PATH"`}, {ID: "before", Kind: "run", Shell: "sh", Command: `test -z "${LATE-}"`}, {ID: "wait", Kind: "wait", Targets: []string{"bg"}}, {ID: "after", Kind: "run", Shell: "sh", Command: `test "$LATE" = yes; case "$PATH" in /late:*) ;; *) exit 9;; esac`}})
	if _, e := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0]}).RunJob(context.Background(), j, w); e != nil {
		t.Fatal(e)
	}
}

func TestRunJobContainerRejectsDeferredFeaturesBeforeDocker(t *testing.T) {
	for _, feature := range []string{"action"} {
		t.Run(feature, func(t *testing.T) {
			f := newJobDocker(t, "")
			w := t.TempDir()
			j := jobContainerPlan(t, w, nil)
			m := &fakeActionMaterializer{}
			switch feature {
			case "action":
				j.Steps = []plan.Step{{Kind: "uses", Uses: "x"}}
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

func TestJobContainerExecRejectsInvalidEnvironmentNamesBeforeDocker(t *testing.T) {
	for _, name := range []string{"", "ALIAS=OTHER", "NUL\x00NAME"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "docker-ran")
			docker := filepath.Join(t.TempDir(), "docker")
			writeFixtureFile(t, filepath.Dir(docker), filepath.Base(docker), "#!/bin/sh\ntouch "+shellTestQuote(marker)+"\n")
			if err := os.Chmod(docker, 0o755); err != nil {
				t.Fatal(err)
			}
			backend := jobContainerBackend{docker: docker, container: "job", workspace: t.TempDir(), temp: t.TempDir()}
			err := backend.exec(context.Background(), Runner{}, newCommandProcessor(io.Discard, io.Discard), backend.workspace, map[string]string{name: "value"}, "true")
			if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
				t.Fatalf("exec() error = %v, want invalid environment name", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Docker was invoked with invalid environment name: %v", statErr)
			}
		})
	}
}

func TestRunJobContainerNodeProbeFailureCleansOwnedResources(t *testing.T) {
	t.Parallel()

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
			if !strings.Contains(joined, "rm --force --volumes buildkite-gha-job-") || !strings.Contains(joined, "network rm buildkite-gha-network-") {
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

func TestActionContainerMountsNativeAdapterDoesNotResolveMise(t *testing.T) {
	workspace := t.TempDir()
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: checkout\nruns:\n  using: node24\n  main: index.js\n")
	writeFixtureFile(t, remote, "index.js", "")
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	job := jobContainerPlan(t, workspace, []plan.Step{{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v4", Action: &plan.ActionSelector{Lock: lockID}}})
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "github", Repository: "actions/checkout", RequestedRef: "v4",
		Commit: actionintegration.CheckoutV4Commit, SourceDigest: digest,
	}}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: digest}}
	actions := newActionLockResolver(job, workspace, materializer)
	miseCalls := 0
	runner := Runner{ResolveMise: func(context.Context) (string, error) {
		miseCalls++
		return "", errors.New("unexpected mise resolution")
	}}
	mounts, err := runner.actionContainerMounts(context.Background(), actions)
	if err != nil {
		t.Fatal(err)
	}
	if miseCalls != 0 {
		t.Fatalf("ResolveMise calls = %d, want 0", miseCalls)
	}
	for _, mount := range mounts {
		if strings.Contains(mount.target, "node24") || strings.Contains(mount.target, "/nodes") {
			t.Fatalf("native adapter gained Node mount: %#v", mount)
		}
	}
}

func TestRunJobContainerCheckoutPopulatesNoMiseWorkspaceAction(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	workflowSource := []byte("name: container\n")
	workflowDigest := sha256.Sum256(workflowSource)
	localSource := []byte("name: local\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: echo 'CHECKOUT_CHAIN=ok' >> \"$GITHUB_ENV\"\n")
	localFixture := t.TempDir()
	writeFixtureFile(t, localFixture, "action.yml", string(localSource))

	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: checkout\nruns:\n  using: node24\n  main: index.js\n")
	writeFixtureFile(t, remote, "index.js", "throw new Error('adapter must not execute checkout JavaScript')\n")
	remoteDigest := digestTree(t, remote)

	sha := strings.Repeat("a", 40)
	git := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
operation=
for argument in "$@"; do
  case "$argument" in init|remote|fetch|checkout) operation="$argument"; break ;; esac
done
case "$operation" in
  init) mkdir -p .git ;;
  checkout)
    printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD
    mkdir -p .github/workflows .github/actions/local
    printf '%s' ` + shellTestQuote(base64.StdEncoding.EncodeToString(workflowSource)) + ` | base64 -d > .github/workflows/container.yml
    printf '%s' ` + shellTestQuote(base64.StdEncoding.EncodeToString(localSource)) + ` | base64 -d > .github/actions/local/action.yml
    ;;
esac
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	requiresMise := false
	checkoutID, localID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := plan.Job{
		Schema:   plan.Schema,
		Compiler: plan.Compiler{Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("1", 64)},
		Runtime:  &plan.Runtime{DistributionDigest: "sha256:" + strings.Repeat("1", 64)},
		Workflow: plan.Workflow{Path: ".github/workflows/container.yml", Digest: "sha256:" + hex.EncodeToString(workflowDigest[:]), LogicalJobID: "container"},
		Event: plan.Event{
			Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64),
			Repository: "buildkite/buildkite-gha", Ref: "refs/heads/main", SHA: sha,
		},
		Target:               plan.Target{StepKey: "gha-container", Queue: "trusted"},
		RequiredCapabilities: []string{"docker", "network"},
		Steps: []plan.Step{
			{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v4", Action: &plan.ActionSelector{Lock: checkoutID}},
			{ID: "local", Kind: "uses", Uses: "./.github/actions/local", Action: &plan.ActionSelector{Lock: localID}},
		},
		Actions: []plan.ActionLock{
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v4", Commit: actionintegration.CheckoutV4Commit, SourceDigest: remoteDigest},
			{ID: localID, Source: "workspace", Path: ".github/actions/local", SourceDigest: digestTree(t, localFixture)},
		},
		RequiresMise: &requiresMise,
		Container:    &plan.Container{Image: "debian:bookworm-slim"},
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: remoteDigest}}
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Git: git, Actions: materializer}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Env["CHECKOUT_CHAIN"] != "ok" {
		t.Fatalf("container checkout chain result = %#v, error = %v", result, err)
	}
	for _, call := range f.calls(t) {
		if slices.Contains(call.Args, "/__buildkite-gha/node16") || slices.Contains(call.Args, "/__buildkite-gha/node20") || slices.Contains(call.Args, "/__buildkite-gha/node24") || slices.Contains(call.Args, "/__buildkite-gha/nodes") {
			t.Fatalf("no-mise container checkout chain mounted or probed Node: %#v", call.Args)
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
	if !strings.Contains(joined, "rm --force --volumes buildkite-gha-job-") || !strings.Contains(joined, "network rm buildkite-gha-network-") {
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
	job.Schema = plan.Schema
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
	t.Parallel()

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
	job.Schema = plan.Schema
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
	t.Parallel()

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
	job.Schema = plan.Schema
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

func TestRunJobContainerRemoteActionPreparationTimeoutIsCancelled(t *testing.T) {
	f := newJobDocker(t, "")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: remote container action timeout\n")
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{{
		ID: "remote", Kind: "uses", Uses: remoteLifecycleUses("selected"), Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "selected", "sha256:"+strings.Repeat("0", 64), nil)}
	job.ContinueOnError = true
	job.TimeoutMinutes = 0.001
	materializer := &fakeActionMaterializer{materialize: func(ctx context.Context, _ source.Resolved) (source.Materialized, error) {
		<-ctx.Done()
		return source.Materialized{}, ctx.Err()
	}}

	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Actions: materializer}).RunJob(context.Background(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || IsToleratedJobFailure(err) || result.Conclusion != "cancelled" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if len(f.calls(t)) != 0 {
		t.Fatalf("Docker called after action preparation timeout: %#v", f.calls(t))
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
	job.Schema = plan.Schema
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
	t.Parallel()

	f := newJobDocker(t, "")
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/container.yml", "name: lazy container action\n")
	actionSource := t.TempDir()
	actionYAML := "name: lazy\nruns:\n  using: node20\n  main: main.js\n"
	mainJS := `require("node:fs").appendFileSync(process.env.GITHUB_OUTPUT, "lazy=ready\n")
`
	writeFixtureFile(t, actionSource, "action.yml", actionYAML)
	writeFixtureFile(t, actionSource, "main.js", mainJS)
	lockID := remoteLifecycleLockID(1)
	lazyDir := ".github/actions/lazy"
	create := fmt.Sprintf("/bin/mkdir -p %s; printf '%%s' %s | base64 -d > %s/action.yml; printf '%%s' %s | base64 -d > %s/main.js",
		lazyDir, base64.StdEncoding.EncodeToString([]byte(actionYAML)), lazyDir,
		base64.StdEncoding.EncodeToString([]byte(mainJS)), lazyDir)
	job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{
		{ID: "populate", Kind: "run", Shell: "sh", Command: create},
		{ID: "lazy", Kind: "uses", Uses: "./" + lazyDir, Action: &plan.ActionSelector{Lock: lockID}},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &plan.Container{Image: "debian:bookworm-slim"}
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "workspace", Path: lazyDir, SourceDigest: digestTree(t, actionSource)}}
	job.Outputs = map[string]string{"lazy": "${{ steps.lazy.outputs.lazy }}"}
	node16 := filepath.Join(t.TempDir(), "node16")
	writeNodeExecutable(t, node16, 16)
	node20 := filepath.Join(t.TempDir(), "node20")
	writeNodeExecutable(t, node20, 20)
	node24 := requireNode24(t)
	result, err := (Runner{Docker: f.path, RuntimeExecutable: os.Args[0], Node16: node16, Node20: node20, Node24: node24}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Outputs["lazy"] != "ready" {
		t.Fatalf("lazy container action result = %#v, error = %v", result, err)
	}
	calls := f.calls(t)
	createIndex := jobDockerCallIndex(calls, "create")
	if createIndex < 0 {
		t.Fatalf("Docker create absent: %#v", calls)
	}
	createArgs := calls[createIndex].Args
	if !slices.Contains(createArgs, "type=bind,source="+node16+",target=/__buildkite-gha/node16,readonly") || !slices.Contains(createArgs, "type=bind,source="+node24+",target=/__buildkite-gha/node24,readonly") || slices.Contains(createArgs, "type=bind,source="+node20+",target=/__buildkite-gha/node20,readonly") {
		t.Fatalf("lazy node20 declaration mounts = %#v", createArgs)
	}
	seenLifecycleExec := false
	for _, call := range calls {
		joined := strings.Join(call.Args, "\x00")
		if !strings.Contains(joined, ContainerProcessHelperCommand+"\x00run") || !strings.Contains(joined, "/.github/actions/lazy/") {
			continue
		}
		seenLifecycleExec = true
		if !strings.Contains(joined, "/__buildkite-gha/node24") {
			t.Fatalf("lazy node20 declaration did not execute with Node 24: %#v", call.Args)
		}
	}
	if !seenLifecycleExec {
		t.Fatalf("lazy node20 lifecycle exec absent: %#v", calls)
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
		job.Schema = plan.Schema
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
		remote := canonicalTempDir(t)
		writeFixtureFile(t, remote, "docker/action.yml", "name: docker\nruns:\n  using: docker\n  image: Dockerfile\n")
		writeFixtureFile(t, remote, "docker/Dockerfile", "FROM scratch\n")
		digest := digestTree(t, remote)
		lockID := remoteLifecycleLockID(1)
		job := runtimePlan(t, workspace, ".github/workflows/container.yml", []plan.Step{{
			ID: "docker", Kind: "uses", Uses: remoteLifecycleUses("docker"), Action: &plan.ActionSelector{Lock: lockID},
		}})
		job.Schema = plan.Schema
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
	job.Schema = plan.Schema
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
	_, err := r.runDocker(context.Background(), newCommandProcessor(nil, nil), dockerAction{Workspace: "/other/workspace", runnerTemp: "/owned/temp"})
	if err == nil || !strings.Contains(err.Error(), "must match the job container's owned host paths") {
		t.Fatalf("mismatched sibling paths error = %v", err)
	}
}

func TestRunJobContainerJavaScriptCancellationRunsPost(t *testing.T) {
	t.Parallel()

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
		{ID: "await", Kind: "run", Shell: "sh", Command: `while [ ! -f "$READY" ]; do /bin/sleep .01; done`},
		{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
	})
	job.Schema = plan.Schema
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
	if explicit := os.Getenv("BUILDKITE_GHA_TEST_RUNTIME"); explicit != "" {
		if err := validateDockerMountFile(explicit); err != nil {
			t.Fatalf("BUILDKITE_GHA_TEST_RUNTIME is not a mount-safe executable: %v", err)
		}
		return explicit
	}
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
	job.Schema = plan.Schema
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

func TestLiveCompiledContainerRuntime(t *testing.T) {
	workspace := t.TempDir()
	if err := os.CopyFS(workspace, os.DirFS(fixturePath(t, "container-runtime"))); err != nil {
		t.Fatalf("copy container runtime fixture: %v", err)
	}
	workflowPath := filepath.Join(workspace, ".github", "workflows", "runtime.yml")
	workflowSource, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	eventSource, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := compiler.CompileBundleWithOptions(workflowPath, workflowSource, eventSource, "container-runtime-live", "sha256:"+strings.Repeat("2", 64), "container-runtime-live-importer", compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "gha-untrusted"},
			UntrustedQueues: []string{"gha-untrusted"},
		},
		ResolveActions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 2 {
		t.Fatalf("compiled plans = %d, want 2", len(bundle.Plans))
	}
	for _, artifact := range bundle.Plans {
		job := artifact.Job
		if job.Schema != plan.Schema || !slices.Equal(job.RequiredCapabilities, []string{"docker", "network"}) {
			t.Fatalf("compiled %s plan boundary = schema %q, capabilities %#v", job.Workflow.LogicalJobID, job.Schema, job.RequiredCapabilities)
		}
		wantSources := []string{"dockerfile-actions", "service-containers"}
		if job.Workflow.LogicalJobID == "container-runtime" {
			wantSources = []string{"dockerfile-actions", "job-containers", "service-containers"}
		}
		if !slices.Equal(artifact.Authorization.DockerCapabilitySources, wantSources) {
			t.Fatalf("compiled %s Docker provenance = %#v, want %#v", job.Workflow.LogicalJobID, artifact.Authorization.DockerCapabilitySources, wantSources)
		}
		if job.Workflow.LogicalJobID == "container-runtime" {
			reader, exists := job.Services["reader"]
			if len(job.Services) != 3 || !exists || job.Services["writer"].Command == "" || !slices.Equal(reader.Volumes, []string{"parity-data:/shared:ro"}) {
				t.Fatalf("compiled service parity fixture = %#v", job.Services)
			}
		}
		if job.Workflow.LogicalJobID == "host-runtime" {
			_, optionalExists := job.Services["optional"]
			if len(job.Services) != 1 || optionalExists {
				t.Fatalf("compiled conditional services = %#v", job.Services)
			}
		}
	}
	docker := requireDocker(t)
	node24 := requireNode24(t)
	before := liveDockerOwnedResources(t, docker)
	runtimeExecutable := buildLiveContainerRuntime(t)
	seen := map[string]bool{}
	for _, artifact := range bundle.Plans {
		job := artifact.Job
		var logs bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		result, runErr := (Runner{Docker: docker, RuntimeExecutable: runtimeExecutable, Node24: node24, Stdout: &logs, Stderr: &logs}).RunJob(ctx, job, workspace)
		cancel()
		if runErr != nil || result.Conclusion != "success" {
			t.Fatalf("run %s result = %#v, error = %v, logs = %q", job.Workflow.LogicalJobID, result, runErr, logs.String())
		}
		wantObservation := strings.TrimSuffix(job.Workflow.LogicalJobID, "-runtime") + "-runtime"
		if result.Outputs["observation"] != wantObservation {
			t.Fatalf("run %s observation = %q, want %q", job.Workflow.LogicalJobID, result.Outputs["observation"], wantObservation)
		}
		if strings.Contains(logs.String(), "container-runtime-javascript-secret") || strings.Contains(logs.String(), "container-runtime-docker-secret") {
			t.Fatalf("run %s leaked a registered mask: %q", job.Workflow.LogicalJobID, logs.String())
		}
		if job.Workflow.LogicalJobID == "container-runtime" {
			for _, want := range []string{"container runtime JavaScript main\n", "container runtime Docker action summary\n", "container runtime JavaScript post\n"} {
				if !strings.Contains(result.Summary, want) {
					t.Fatalf("container summary lacks %q: %q", want, result.Summary)
				}
			}
		}
		seen[job.Workflow.LogicalJobID] = true
	}
	if !seen["container-runtime"] || !seen["host-runtime"] {
		t.Fatalf("executed logical jobs = %#v", seen)
	}
	if contents, readErr := os.ReadFile(filepath.Join(workspace, "container-runtime-post-ran")); readErr != nil || string(contents) != "yes" {
		t.Fatalf("compiled container post marker = %q, %v", contents, readErr)
	}
	if after := liveDockerOwnedResources(t, docker); !slices.Equal(after, before) {
		t.Fatalf("container runtime leaked owned Docker resources: before=%#v after=%#v", before, after)
	}
}

func TestLiveAuthenticatedServiceRegistry(t *testing.T) {
	docker := requireDocker(t)
	registryName := "buildkite-gha-test-registry-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	registryID := strings.TrimSpace(runLiveDocker(t, docker, "run", "--detach", "--name", registryName, "--publish", "127.0.0.1::5000", "registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"))
	t.Cleanup(func() { _ = exec.Command(docker, "rm", "--force", registryID).Run() })
	registryAddress := strings.TrimSpace(runLiveDocker(t, docker, "port", registryID, "5000/tcp"))
	registryURL, err := url.Parse("http://" + registryAddress)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(registryURL)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "service-user" || password != "service-password" {
			response.Header().Set("WWW-Authenticate", `Basic realm="buildkite-gha-test"`)
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(response, request)
	}))
	defer server.Close()
	privateRegistry := strings.TrimPrefix(server.URL, "http://")
	privateImage := privateRegistry + "/private/busybox:parity"
	runLiveDocker(t, docker, "pull", "busybox@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028")
	runLiveDocker(t, docker, "tag", "busybox@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028", privateImage)
	t.Cleanup(func() { _ = exec.Command(docker, "image", "rm", "--force", privateImage).Run() })
	config := t.TempDir()
	login := exec.Command(docker, "--config", config, "login", privateRegistry, "--username", "service-user", "--password-stdin")
	login.Stdin = strings.NewReader("service-password\n")
	if output, loginErr := login.CombinedOutput(); loginErr != nil {
		t.Fatalf("login to temporary registry: %v: %s", loginErr, output)
	}
	runLiveDocker(t, docker, "--config", config, "push", privateImage)

	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/authenticated.yml", "name: authenticated service\n")
	job := runtimePlan(t, workspace, ".github/workflows/authenticated.yml", []plan.Step{{ID: "verify", Kind: "run", Shell: "sh", Command: "true"}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Services = map[string]plan.Container{"private": {Image: privateImage, Credentials: &plan.ContainerCredentials{Username: "service-user", Password: "service-password"}, Command: "sleep 300"}}
	job.ServiceOrder = []string{"private"}
	before := liveDockerOwnedResources(t, docker)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, runErr := (Runner{Docker: docker}).RunJob(ctx, job, workspace)
	if runErr != nil || result.Conclusion != "success" {
		t.Fatalf("authenticated service result = %#v, error = %v", result, runErr)
	}
	if after := liveDockerOwnedResources(t, docker); !slices.Equal(after, before) {
		t.Fatalf("authenticated service leaked owned Docker resources: before=%#v after=%#v", before, after)
	}
}

func runLiveDocker(t *testing.T, docker string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, docker, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return string(output)
}

func TestLiveManifestContainerFixtures(t *testing.T) {
	docker := requireDocker(t)
	workspace := t.TempDir()
	if err := os.CopyFS(workspace, os.DirFS(fixturePath(t, "unsupported"))); err != nil {
		t.Fatalf("copy smoke fixture root: %v", err)
	}
	eventSource, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeExecutable := buildLiveContainerRuntime(t)
	before := liveDockerOwnedResources(t, docker)
	for _, test := range []struct {
		name       string
		provenance string
		logicalJob string
	}{
		{name: "job-container.yml", provenance: "job-containers", logicalJob: "container"},
		{name: "service-container.yml", provenance: "service-containers", logicalJob: "service"},
	} {
		workflowPath := filepath.Join(workspace, ".github", "workflows", test.name)
		workflowSource, readErr := os.ReadFile(workflowPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		bundle, compileErr := compiler.CompileBundle(workflowPath, workflowSource, eventSource, "container-runtime-live", "sha256:"+strings.Repeat("2", 64), "container-runtime-live-importer")
		if compileErr != nil {
			t.Fatalf("compile %s: %v", test.name, compileErr)
		}
		if len(bundle.Plans) != 1 || bundle.Plans[0].Job.Schema != plan.Schema || bundle.Plans[0].Job.Workflow.LogicalJobID != test.logicalJob || !slices.Equal(bundle.Plans[0].Authorization.DockerCapabilitySources, []string{test.provenance}) {
			t.Fatalf("compiled %s boundary = %#v", test.name, bundle.Plans)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		result, runErr := (Runner{Docker: docker, RuntimeExecutable: runtimeExecutable}).RunJob(ctx, bundle.Plans[0].Job, workspace)
		cancel()
		if runErr != nil || result.Conclusion != "success" {
			t.Fatalf("run %s result = %#v, error = %v", test.name, result, runErr)
		}
	}
	if after := liveDockerOwnedResources(t, docker); !slices.Equal(after, before) {
		t.Fatalf("manifest container fixtures leaked owned Docker resources: before=%#v after=%#v", before, after)
	}
}

func TestLiveServiceDifferentialFixture(t *testing.T) {
	docker := requireDocker(t)
	sourcePath := fixturePath(t, "..", ".github", "workflows", "service-container-oracle.yml")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "service-container-oracle.yml")
	writeFixtureFile(t, workspace, ".github/workflows/service-container-oracle.yml", string(source))
	eventSource, err := os.ReadFile(fixturePath(t, "smoke", "events", "workflow_dispatch.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := compiler.CompileBundle(workflowPath, source, eventSource, "service-differential-live", "sha256:"+strings.Repeat("3", 64), "service-differential-importer")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 1 || !slices.Equal(bundle.Plans[0].Job.ServiceOrder, []string{"z-writer", "a-reader", "postgres", "redis"}) {
		t.Fatalf("differential service plans = %#v", bundle.Plans)
	}
	before := liveDockerOwnedResources(t, docker)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, runErr := (Runner{Docker: docker}).RunJob(ctx, bundle.Plans[0].Job, workspace)
	if runErr != nil || result.Conclusion != "success" || result.Outputs["observation"] != "selection,context,ports,command-entrypoint,health,volume,postgres,redis" {
		t.Fatalf("differential service result = %#v, error = %v", result, runErr)
	}
	if after := liveDockerOwnedResources(t, docker); !slices.Equal(after, before) {
		t.Fatalf("differential service fixture leaked owned Docker resources: before=%#v after=%#v", before, after)
	}
}

func TestLiveUnhealthyServiceDiagnostics(t *testing.T) {
	docker := requireDocker(t)
	contextDir := t.TempDir()
	dockerfile := `FROM busybox@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028
HEALTHCHECK --interval=1s --timeout=1s --retries=1 CMD exit 1
CMD ["sh", "-c", "echo container-runtime-health-diagnostic >&2; sleep 300"]
`
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	image := "buildkite-gha-container-runtime-unhealthy-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	command := exec.Command(docker, "buildx", "build", "--builder", "default", "--load", "--tag", image, contextDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build unhealthy service: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command(docker, "image", "rm", "--force", image).Run() })
	// Other live fixtures exercise anonymous pulls. Keep this one-off local image
	// in the daemon so this test can isolate readiness diagnostics and cleanup.
	dockerWrapper := filepath.Join(t.TempDir(), "docker")
	wrapper := "#!/bin/sh\nif [ \"$1\" = pull ]; then exit 0; fi\nexec " + strconv.Quote(docker) + " \"$@\"\n"
	if err := os.WriteFile(dockerWrapper, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/health.yml", "name: health diagnostics\n")
	job := runtimePlan(t, workspace, ".github/workflows/health.yml", []plan.Step{{ID: "unreachable", Kind: "run", Shell: "sh", Command: "true"}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Services = map[string]plan.Container{"unhealthy": {Image: image}}
	job.ServiceOrder = []string{"unhealthy"}
	var logs bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := (Runner{Docker: dockerWrapper, Stdout: &logs, Stderr: &logs}).RunJob(ctx, job, workspace)
	if err == nil || !strings.Contains(err.Error(), `service "unhealthy" failed readiness with status "unhealthy"`) {
		t.Fatalf("unhealthy service error = %v, logs = %q", err, logs.String())
	}
	if !strings.Contains(logs.String(), "container-runtime-health-diagnostic") {
		t.Fatalf("unhealthy service diagnostic absent: %q", logs.String())
	}
}

func liveDockerOwnedResources(t *testing.T, docker string) []string {
	t.Helper()
	queries := []struct {
		kind string
		args []string
	}{
		{kind: "container", args: []string{"container", "ls", "--all", "--format", "{{.Names}}", "--filter", "name=buildkite-gha-"}},
		{kind: "network", args: []string{"network", "ls", "--format", "{{.Name}}", "--filter", "name=buildkite-gha-network-"}},
		{kind: "image", args: []string{"image", "ls", "--format", "{{.Repository}}", "--filter", "reference=buildkite-gha-image-*"}},
	}
	var resources []string
	for _, query := range queries {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		output, err := exec.CommandContext(ctx, docker, query.args...).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("query live Docker %s resources: %v: %s", query.kind, err, output)
		}
		for _, name := range strings.Fields(string(output)) {
			resources = append(resources, query.kind+":"+name)
		}
	}
	slices.Sort(resources)
	return resources
}
