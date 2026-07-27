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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

const jobContainerWorkspace = "/__w/repo/repo"
const jobContainerTemp = "/__w/_temp"
const jobContainerRuntime = "/__buildkite-gha/runtime"

const serviceReadinessAttempts = 30
const serviceReadinessInterval = time.Second
const serviceLogTail = "200"
const serviceDiagnosticTimeout = 3 * time.Second

type containerMount struct {
	host, target string
	readonly     bool
	probe        bool
}

type serviceContainer struct {
	id, name string
}

type jobContainerBackend struct {
	runner                    Runner
	docker                    string
	env                       map[string]string
	config                    string
	owner, container, network string
	services                  []serviceContainer
	workspace, temp           string
	imagePATH                 string
	mounts                    []containerMount
	nodeMu                    sync.Mutex
	probedNodes               map[string]bool
	servicePorts              map[string]map[string]string
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

func (r Runner) startJobContainer(ctx context.Context, processor *commandProcessor, workspace, temp string, spec plan.Container, services map[string]plan.Container, extra ...containerMount) (_ *jobContainerBackend, err error) {
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
	b := &jobContainerBackend{runner: r, docker: docker, env: env, config: config, owner: "com.buildkite.gha.owner=" + id, network: "buildkite-gha-network-" + id, workspace: workspace, temp: temp, servicePorts: make(map[string]map[string]string)}
	if spec.Image != "" {
		b.container = "buildkite-gha-job-" + id
	}
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
	var runtimeExecutable string
	if spec.Image != "" {
		runtimeExecutable = r.RuntimeExecutable
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
	}
	if spec.Image != "" {
		if err = r.runStreaming(ctx, newCommandProcessor(r.stdout(), r.stderr()), "", env, docker, "pull", spec.Image); err != nil {
			return nil, fmt.Errorf("pull job container image: %w", err)
		}
	}
	pulled := map[string]bool{}
	if spec.Image != "" {
		pulled[spec.Image] = true
	}
	for _, serviceID := range sortedKeys(services) {
		image := services[serviceID].Image
		if pulled[image] {
			continue
		}
		if err = r.runStreaming(ctx, processor, "", env, docker, "pull", image); err != nil {
			return nil, fmt.Errorf("pull service %q image: %w", serviceID, err)
		}
		pulled[image] = true
	}
	if _, err = boundedDockerOutput(ctx, env, docker, "network", "create", "--label", b.owner, b.network); err != nil {
		return nil, fmt.Errorf("create job container network: %w", err)
	}
	for _, serviceID := range sortedKeys(services) {
		service := services[serviceID]
		serviceNonce, randomErr := randomHex()
		if randomErr != nil {
			return nil, fmt.Errorf("create service %q identity: %w", serviceID, randomErr)
		}
		name := "buildkite-gha-service-" + serviceNonce
		// Track the exact name before create: Docker may create the container and
		// still return an ambiguous client/transport error.
		b.services = append(b.services, serviceContainer{id: serviceID, name: name})
		serviceArgs := []string{"create", "--name", name, "--label", b.owner, "--network", b.network, "--network-alias", serviceID}
		for _, key := range sortedKeys(service.Env) {
			serviceArgs = append(serviceArgs, "--env", key+"="+service.Env[key])
		}
		serviceArgs = appendPublishedPorts(serviceArgs, service.Ports)
		serviceArgs = append(serviceArgs, service.Image)
		if _, err = boundedDockerOutput(ctx, env, docker, serviceArgs...); err != nil {
			return nil, fmt.Errorf("create service %q: %w", serviceID, err)
		}
	}
	for _, service := range b.services {
		if _, err = boundedDockerOutput(ctx, env, docker, "start", service.name); err != nil {
			return nil, fmt.Errorf("start service %q: %w", service.id, err)
		}
	}
	for _, service := range b.services {
		ports, portErr := b.readServicePorts(ctx, service.id, service.name, services[service.id].Ports)
		if portErr != nil {
			b.serviceDiagnostics(processor, service.name)
			return nil, portErr
		}
		b.servicePorts[service.id] = ports
	}
	readinessCtx, cancelReadiness := context.WithTimeout(ctx, serviceReadinessAttempts*serviceReadinessInterval)
	defer cancelReadiness()
	for _, service := range b.services {
		if err = b.waitForService(readinessCtx, processor, service.id, service.name); err != nil {
			return nil, err
		}
	}
	if spec.Image != "" {
		args := []string{"create", "--name", b.container, "--label", b.owner, "--network", b.network,
			"--mount", "type=bind,source=" + workspace + ",target=" + jobContainerWorkspace,
			"--mount", "type=bind,source=" + temp + ",target=" + jobContainerTemp,
			"--mount", "type=bind,source=" + runtimeExecutable + ",target=" + jobContainerRuntime + ",readonly",
			"--workdir", jobContainerWorkspace, "--entrypoint", "sh"}
		if r.containerCacheEnv != nil {
			args = append(args, "--add-host", cacheContainerHost)
		}
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
		args = appendPublishedPorts(args, spec.Ports)
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
	}
	ok = true
	return b, nil
}

var dockerPortLine = regexp.MustCompile(`^([0-9]+)/(tcp|udp) -> 127\.0\.0\.1:([0-9]+)$`)

func (b *jobContainerBackend) readServicePorts(ctx context.Context, id, name string, declared []string) (map[string]string, error) {
	want := map[string]bool{}
	for _, publication := range declared {
		parts := strings.SplitN(publication, "/", 2)
		proto := "tcp"
		if len(parts) == 2 {
			proto = parts[1]
		}
		container := parts[0]
		if i := strings.LastIndex(container, ":"); i >= 0 {
			container = container[i+1:]
		}
		want[container+"/"+proto] = true
	}
	out, err := boundedDockerOutput(ctx, b.env, b.docker, "port", name)
	if err != nil {
		return nil, fmt.Errorf("query service %q ports: %w", id, err)
	}
	got, result := map[string]bool{}, map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), "\n") {
		if line == "" && out == "" {
			continue
		}
		m := dockerPortLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("service %q has malformed Docker port output %q", id, line)
		}
		cp, e1 := strconv.Atoi(m[1])
		hp, e2 := strconv.Atoi(m[3])
		key := m[1] + "/" + m[2]
		if e1 != nil || e2 != nil || cp < 1 || cp > 65535 || hp < 1 || hp > 65535 || !want[key] {
			return nil, fmt.Errorf("service %q has invalid or undeclared port mapping %q", id, line)
		}
		got[key] = true
		// GitHub's runner exposes ports in a dictionary keyed only by numeric
		// container port, so later Docker mappings intentionally replace earlier
		// TCP/UDP mappings for the same port.
		result[m[1]] = m[3]
	}
	for key := range want {
		if !got[key] {
			return nil, fmt.Errorf("service %q is missing declared port mapping %s", id, key)
		}
	}
	return result, nil
}

func appendPublishedPorts(args, ports []string) []string {
	for _, port := range ports {
		if strings.Contains(strings.SplitN(port, "/", 2)[0], ":") {
			args = append(args, "--publish", "127.0.0.1:"+port)
		} else {
			args = append(args, "--publish", "127.0.0.1::"+port)
		}
	}
	return args
}

func (b *jobContainerBackend) waitForService(ctx context.Context, processor *commandProcessor, serviceID, name string) error {
	const format = `{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}`
	for attempt := 0; attempt < serviceReadinessAttempts; attempt++ {
		status, err := boundedDockerOutput(ctx, b.env, b.docker, "inspect", "--format", format, name)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("wait for service %q readiness: %w", serviceID, ctx.Err())
			}
			b.serviceDiagnostics(processor, name)
			return fmt.Errorf("inspect service %q readiness: %w", serviceID, err)
		}
		switch strings.TrimSpace(status) {
		case "healthy", "running":
			return nil
		case "unhealthy", "exited", "dead":
			b.serviceDiagnostics(processor, name)
			return fmt.Errorf("service %q failed readiness with status %q", serviceID, strings.TrimSpace(status))
		}
		if attempt+1 < serviceReadinessAttempts {
			timer := time.NewTimer(serviceReadinessInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				b.serviceDiagnostics(processor, name)
				return fmt.Errorf("wait for service %q readiness: %w", serviceID, ctx.Err())
			case <-timer.C:
			}
		}
	}
	b.serviceDiagnostics(processor, name)
	return fmt.Errorf("service %q readiness timed out after %d attempts", serviceID, serviceReadinessAttempts)
}

func (b *jobContainerBackend) serviceDiagnostics(processor *commandProcessor, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), serviceDiagnosticTimeout)
	defer cancel()
	output, _ := boundedDockerCombinedOutput(ctx, b.env, b.docker, "logs", "--tail", serviceLogTail, name)
	for _, line := range strings.Split(strings.TrimSuffix(strings.ReplaceAll(output, "\r\n", "\n"), "\n"), "\n") {
		if line != "" {
			processor.process(processor.stderr, line)
		}
	}
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
	// Each service gets enough budget for its graceful stop in addition to the
	// base budget, which remains reserved for the job, network, and verification.
	ctx, cancel := context.WithTimeout(context.Background(), jobContainerCleanupTimeout(b.runner.cleanupTimeout(), len(b.services)))
	defer cancel()
	var err error
	for i := len(b.services) - 1; i >= 0; i-- {
		name := b.services[i].name
		out, queryErr := boundedDockerOutput(ctx, b.env, b.docker, "ps", "--all", "--quiet", "--filter", "label="+b.owner, "--filter", "name=^/"+name+"$")
		if queryErr != nil {
			err = errors.Join(err, fmt.Errorf("query service container: %w", queryErr))
		}
		if queryErr != nil || strings.TrimSpace(out) != "" {
			if _, e := boundedDockerOutput(ctx, b.env, b.docker, "stop", "--time", "2", name); e != nil {
				err = errors.Join(err, fmt.Errorf("stop service container: %w", e))
			}
			if _, e := boundedDockerOutput(ctx, b.env, b.docker, "rm", "--force", name); e != nil {
				err = errors.Join(err, fmt.Errorf("remove service container: %w", e))
			}
		}
	}
	var out string
	var queryErr error
	if b.container != "" {
		out, queryErr = boundedDockerOutput(ctx, b.env, b.docker, "ps", "--all", "--quiet", "--filter", "label="+b.owner, "--filter", "name=^/"+b.container+"$")
		if queryErr != nil {
			err = errors.Join(err, fmt.Errorf("query job container: %w", queryErr))
		}
		if queryErr != nil || strings.TrimSpace(out) != "" {
			_, e := boundedDockerOutput(ctx, b.env, b.docker, "rm", "--force", b.container)
			if e != nil {
				err = errors.Join(err, fmt.Errorf("remove job container: %w", e))
			}
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

func jobContainerCleanupTimeout(base time.Duration, services int) time.Duration {
	return base + time.Duration(services)*3*time.Second
}
