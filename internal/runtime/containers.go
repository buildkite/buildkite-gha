package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

const jobContainerWorkspace = "/__w/repo/repo"
const jobContainerTemp = "/__w/_temp"
const jobContainerRuntime = "/__buildkite-gha/runtime"

type containerMount struct {
	host, target string
	readonly     bool
	probe        bool
}

type jobContainerBackend struct {
	runner                    Runner
	docker                    string
	env                       map[string]string
	config                    string
	owner, container, network string
	workspace, temp           string
	imagePATH                 string
	mounts                    []containerMount
	nodeMu                    sync.Mutex
	probedNodes               map[string]bool
}

func privateDocker(r Runner) (string, string, map[string]string, error) {
	docker := r.Docker
	var err error
	if docker == "" {
		docker, err = exec.LookPath("docker")
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("discover Docker: %w", err)
	}
	config, err := os.MkdirTemp("", "buildkite-gha-docker-config-")
	if err != nil {
		return "", "", nil, fmt.Errorf("create private Docker configuration: %w", err)
	}
	if err := os.Chmod(config, 0o700); err != nil {
		_ = os.RemoveAll(config)
		return "", "", nil, err
	}
	return docker, config, map[string]string{"DOCKER_CONFIG": config}, nil
}

func (r Runner) startJobContainer(ctx context.Context, workspace, temp string, spec plan.Container, extra ...containerMount) (_ *jobContainerBackend, err error) {
	docker, config, env, err := privateDocker(r)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		_ = os.RemoveAll(config)
		return nil, err
	}
	id := hex.EncodeToString(nonce[:])
	b := &jobContainerBackend{runner: r, docker: docker, env: env, config: config, owner: "com.buildkite.gha.owner=" + id, container: "buildkite-gha-job-" + id, network: "buildkite-gha-network-" + id, workspace: workspace, temp: temp}
	b.mounts = []containerMount{{host: workspace, target: jobContainerWorkspace}, {host: temp, target: jobContainerTemp}}
	for _, m := range extra {
		if err := validateContainerMount(m); err != nil {
			_ = os.RemoveAll(config)
			return nil, err
		}
		duplicate := false
		for _, old := range b.mounts {
			if old.host == m.host && old.target == m.target {
				duplicate = true
				break
			}
			if old.host == m.host || old.target == m.target {
				_ = os.RemoveAll(config)
				return nil, fmt.Errorf("conflicting job container mount mapping %q to %q", m.host, m.target)
			}
		}
		if duplicate {
			continue
		}
		b.mounts = append(b.mounts, m)
	}
	ok := false
	defer func() {
		if !ok {
			err = errors.Join(err, b.cleanup())
		}
	}()
	if err = validateDockerMountPath(workspace); err != nil {
		return nil, err
	}
	if err = validateDockerMountPath(temp); err != nil {
		return nil, err
	}
	runtimeExecutable := r.RuntimeExecutable
	if runtimeExecutable == "" {
		runtimeExecutable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve runtime executable: %w", err)
		}
	}
	runtimeExecutable, err = filepath.Abs(runtimeExecutable)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime executable: %w", err)
	}
	if err = validateDockerMountFile(runtimeExecutable); err != nil {
		return nil, err
	}
	if err = r.runStreaming(ctx, newCommandProcessor(r.stdout(), r.stderr()), "", env, docker, "pull", spec.Image); err != nil {
		return nil, fmt.Errorf("pull job container image: %w", err)
	}
	if _, err = boundedDockerOutput(ctx, env, docker, "network", "create", "--label", b.owner, b.network); err != nil {
		return nil, fmt.Errorf("create job container network: %w", err)
	}
	args := []string{"create", "--name", b.container, "--label", b.owner, "--network", b.network,
		"--mount", "type=bind,source=" + workspace + ",target=" + jobContainerWorkspace,
		"--mount", "type=bind,source=" + temp + ",target=" + jobContainerTemp,
		"--mount", "type=bind,source=" + runtimeExecutable + ",target=" + jobContainerRuntime + ",readonly",
		"--workdir", jobContainerWorkspace, "--entrypoint", "sh"}
	for _, m := range b.mounts[2:] {
		mount := "type=bind,source=" + m.host + ",target=" + m.target
		if m.readonly {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}
	for _, name := range sortedKeys(spec.Env) {
		args = append(args, "--env", name+"="+spec.Env[name])
	}
	args = append(args, spec.Image, "-c", "while :; do sleep 3600; done")
	if _, err = boundedDockerOutput(ctx, env, docker, args...); err != nil {
		return nil, fmt.Errorf("create job container: %w", err)
	}
	if _, err = boundedDockerOutput(ctx, env, docker, "start", b.container); err != nil {
		return nil, fmt.Errorf("start job container: %w", err)
	}
	probePID := jobContainerTemp + "/startup-probe.pid"
	out, probeErr := boundedDockerOutput(ctx, env, docker, "exec", b.container, jobContainerRuntime, ContainerProcessHelperCommand, "run", probePID, "sh", "-c", `printf '%s' "$PATH"`)
	if probeErr != nil {
		return nil, fmt.Errorf("job container runtime helper failed to start (the mounted executable must be a self-contained Linux executable and the image must provide sh): %w", probeErr)
	}
	_ = os.Remove(filepath.Join(temp, "startup-probe.pid"))
	b.imagePATH = out
	for _, m := range b.mounts[2:] {
		if !m.readonly || !m.probe {
			continue
		}
		probe := "test -r \"$1\" && test -x \"$1\""
		if _, e := boundedDockerOutput(ctx, env, docker, "exec", b.container, "sh", "-c", probe, "sh", m.target); e != nil {
			return nil, fmt.Errorf("read-only job container mount %q is not readable/traversable by the image USER: %w", m.target, e)
		}
	}
	ok = true
	return b, nil
}

func (b *jobContainerBackend) containerPath(path string) string {
	best, bestLen := path, -1
	mounts := b.mounts
	if len(mounts) == 0 {
		mounts = []containerMount{{host: b.workspace, target: jobContainerWorkspace}, {host: b.temp, target: jobContainerTemp}}
	}
	for _, m := range mounts {
		if rel, err := filepath.Rel(m.host, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && len(m.host) > bestLen {
			best, bestLen = filepath.ToSlash(filepath.Join(m.target, rel)), len(m.host)
		}
	}
	return best
}

func (b *jobContainerBackend) exec(ctx context.Context, r Runner, processor *commandProcessor, dir string, env map[string]string, name string, argv ...string) error {
	nonce, err := randomHex()
	if err != nil {
		return fmt.Errorf("create container exec identity: %w", err)
	}
	pidfile := filepath.Join(b.temp, "exec-"+nonce+".pid")
	containerPID := b.containerPath(pidfile)
	args := []string{"exec", "--workdir", b.containerPath(dir)}
	for _, key := range sortedKeys(env) {
		value := env[key]
		if key == "PATH" {
			value = b.translatePATH(value)
		} else {
			value = b.containerPath(value)
		}
		args = append(args, "--env", key+"="+value)
	}
	args = append(args, b.container, jobContainerRuntime, ContainerProcessHelperCommand, "run", containerPID, name)
	args = append(args, argv...)
	dockerCtx, stopDocker := context.WithCancel(context.Background())
	defer stopDocker()
	done := make(chan error, 1)
	dockerRunner := r
	dockerRunner.InterruptGrace = 100 * time.Millisecond
	dockerRunner.TerminateGrace = 100 * time.Millisecond
	go func() {
		done <- dockerRunner.runStreaming(dockerCtx, processor, "", b.env, b.docker, args...)
	}()
	select {
	case err := <-done:
		_ = os.Remove(pidfile)
		_ = os.Remove(pidfile + containerCancellationMarkerSuffix)
		return err
	case <-ctx.Done():
		terminationBound := containerPIDPublicationWait + r.interruptGrace() + r.terminateGrace() + 250*time.Millisecond
		cleanup, cancel := context.WithTimeout(context.Background(), terminationBound)
		_, terminateErr := boundedDockerOutput(cleanup, b.env, b.docker, "exec", b.container, jobContainerRuntime, ContainerProcessHelperCommand, "terminate", containerPID, r.interruptGrace().String(), r.terminateGrace().String())
		cancel()
		stopDocker()
		local := time.NewTimer(time.Second)
		select {
		case <-done:
			if !local.Stop() {
				<-local.C
			}
		case <-local.C:
			terminateErr = errors.Join(terminateErr, errors.New("local Docker exec client did not stop within 1s"))
			<-done
		}
		_ = os.Remove(pidfile)
		_ = os.Remove(pidfile + containerCancellationMarkerSuffix)
		return errors.Join(ctx.Err(), terminateErr)
	}
}

func validateContainerMount(m containerMount) error {
	if m.host == "" || !filepath.IsAbs(m.host) || strings.ContainsAny(m.host, ",\"'\n\r\x00") {
		return fmt.Errorf("job container mount source %q cannot be represented by Docker mount grammar", m.host)
	}
	info, err := os.Stat(m.host)
	if err != nil {
		return fmt.Errorf("validate job container mount source %q: %w", m.host, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("job container mount source %q is not a regular file or directory", m.host)
	}
	if !filepath.IsAbs(m.target) || strings.ContainsAny(m.target, ",\"'\n\r\x00") || filepath.Clean(m.target) != m.target {
		return fmt.Errorf("job container mount target %q is invalid", m.target)
	}
	return nil
}

func remoteMountTarget(repository, commit string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(repository) + "\x00" + commit))
	return fmt.Sprintf("/__w/_actions/%x", sum[:16])
}

func (b *jobContainerBackend) probeNode(ctx context.Context, host string, major int) error {
	abs, err := filepath.Abs(host)
	if err != nil {
		return err
	}
	target := b.containerPath(abs)
	if target == abs {
		return fmt.Errorf("node %d runtime is not mounted in the job container", major)
	}
	key := fmt.Sprintf("%d:%s", major, target)
	b.nodeMu.Lock()
	if b.probedNodes[key] {
		b.nodeMu.Unlock()
		return nil
	}
	b.nodeMu.Unlock()

	out, err := boundedDockerOutput(ctx, b.env, b.docker, "exec", b.container, target, "--version")
	if err != nil {
		return fmt.Errorf("mounted Node %d is incompatible with job container image: %w", major, err)
	}
	version := strings.TrimSpace(out)
	if !strings.HasPrefix(version, fmt.Sprintf("v%d.", major)) {
		return fmt.Errorf("mounted Node %d in job container reported %q; exact major %d is required", major, version, major)
	}
	b.nodeMu.Lock()
	if b.probedNodes == nil {
		b.probedNodes = map[string]bool{}
	}
	b.probedNodes[key] = true
	b.nodeMu.Unlock()
	return nil
}

func (b *jobContainerBackend) translatePATH(value string) string {
	parts := filepath.SplitList(value)
	for i := range parts {
		parts[i] = b.containerPath(parts[i])
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

func validateDockerMountFile(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, ",\"'\n\r\x00") {
		return fmt.Errorf("runtime executable path cannot be represented by Docker mount grammar")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("validate Docker mount file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("validate Docker mount file %q: not a regular file", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("validate Docker mount file %q: not executable", path)
	}
	return nil
}

func randomHex() (string, error) {
	var n [8]byte
	_, err := rand.Read(n[:])
	return hex.EncodeToString(n[:]), err
}

func (b *jobContainerBackend) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), b.runner.cleanupTimeout())
	defer cancel()
	var err error
	out, queryErr := boundedDockerOutput(ctx, b.env, b.docker, "ps", "--all", "--quiet", "--filter", "label="+b.owner, "--filter", "name=^/"+b.container+"$")
	if queryErr != nil {
		err = errors.Join(err, fmt.Errorf("query job container: %w", queryErr))
	}
	if queryErr != nil || strings.TrimSpace(out) != "" {
		_, e := boundedDockerOutput(ctx, b.env, b.docker, "rm", "--force", b.container)
		if e != nil {
			err = errors.Join(err, fmt.Errorf("remove job container: %w", e))
		}
	}
	out, queryErr = boundedDockerOutput(ctx, b.env, b.docker, "network", "ls", "--quiet", "--filter", "label="+b.owner, "--filter", "name=^"+b.network+"$")
	if queryErr != nil {
		err = errors.Join(err, fmt.Errorf("query job network: %w", queryErr))
	}
	if queryErr != nil || strings.TrimSpace(out) != "" {
		_, e := boundedDockerOutput(ctx, b.env, b.docker, "network", "rm", b.network)
		if e != nil {
			err = errors.Join(err, fmt.Errorf("remove job network: %w", e))
		}
	}
	for _, q := range [][]string{{"ps", "--all", "--quiet", "--filter", "label=" + b.owner}, {"network", "ls", "--quiet", "--filter", "label=" + b.owner}} {
		out, e := boundedDockerOutput(ctx, b.env, b.docker, q...)
		if e != nil {
			err = errors.Join(err, fmt.Errorf("verify owned Docker cleanup query: %w", e))
		} else if strings.TrimSpace(out) != "" {
			err = errors.Join(err, fmt.Errorf("verify owned Docker cleanup: leftover resources %q", strings.TrimSpace(out)))
		}
	}
	_ = os.RemoveAll(b.config)
	return err
}
