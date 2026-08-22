package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/buildkite-gha/internal/containerpolicy"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

const jobContainerWorkspace = "/__w/repo/repo"
const jobContainerTemp = "/__w/_temp"
const jobContainerRuntime = "/__buildkite-gha/runtime"

const serviceLogTail = "200"
const serviceDiagnosticTimeout = 3 * time.Second

type containerMount struct {
	host, target string
	readonly     bool
	probe        bool
}

type serviceContainer struct {
	id, name string
	created  bool
	ready    bool
}

type jobContainerBackend struct {
	runner                    Runner
	processor                 *commandProcessor
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
	servicePorts              map[string]expression.ServiceContext
	existingVolumes           map[string]bool
	ownedVolumes              []string
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

func (r Runner) startJobContainer(ctx context.Context, processor *commandProcessor, workspace, temp string, spec *plan.Container, services map[string]plan.ServiceContainer, extra ...containerMount) (_ *jobContainerBackend, err error) {
	return r.startJobContainerOrdered(ctx, processor, workspace, temp, spec, services, sortedKeys(services), extra...)
}

func (r Runner) startJobContainerOrdered(ctx context.Context, processor *commandProcessor, workspace, temp string, spec *plan.Container, services map[string]plan.ServiceContainer, serviceOrder []string, extra ...containerMount) (_ *jobContainerBackend, err error) {
	if spec != nil {
		if err := validateEnvironmentNames(spec.Env); err != nil {
			return nil, fmt.Errorf("job container environment: %w", err)
		}
		if err := containerpolicy.ValidateJobVolumes(spec.Volumes); err != nil {
			return nil, fmt.Errorf("job container: %w", err)
		}
	}
	for serviceID, service := range services {
		if err := validateEnvironmentNames(service.Env); err != nil {
			return nil, fmt.Errorf("service %q environment: %w", serviceID, err)
		}
	}
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
	b := &jobContainerBackend{runner: r, processor: processor, docker: docker, env: env, config: config, owner: "com.buildkite.gha.owner." + id + "=true", network: "buildkite-gha-network-" + id, workspace: workspace, temp: temp, servicePorts: make(map[string]expression.ServiceContext)}
	if spec != nil {
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
	workflowFailure := false
	defer func() {
		if !ok {
			if cleanupErr := b.cleanup(ctx); cleanupErr != nil {
				err = errors.Join(err, markHardJobFailure(cleanupErr))
			}
		}
		if workflowFailure && !isHardJobFailure(err) {
			err = markWorkflowJobFailure(err)
		}
	}()
	if err = validateDockerMountPath(workspace); err != nil {
		return nil, err
	}
	if err = validateDockerMountPath(temp); err != nil {
		return nil, err
	}
	var runtimeExecutable string
	if spec != nil {
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
	workflowFailure = true
	if len(services) != 0 || spec != nil && len(spec.Volumes) != 0 {
		volumes, volumeErr := boundedDockerOutput(ctx, env, docker, "volume", "ls", "--quiet")
		if volumeErr != nil {
			return nil, fmt.Errorf("snapshot Docker volumes: %w", volumeErr)
		}
		b.existingVolumes = lineSet(volumes)
		if spec != nil {
			for _, volume := range spec.Volumes {
				name := containerpolicy.JobVolumeName(volume)
				if b.existingVolumes[name] {
					return nil, fmt.Errorf("job container volume %q already exists", name)
				}
			}
		}
	}
	if spec != nil {
		if err = r.pullContainerImage(ctx, processor, env, docker, spec.Image); err != nil {
			return nil, fmt.Errorf("pull job container image: %w", err)
		}
	}
	activeCredential := map[string][sha256.Size]byte{}
	for _, serviceID := range serviceOrder {
		service := services[serviceID]
		image := service.Image
		if service.Credentials != nil && service.Credentials.Username != "" && service.Credentials.Password != "" {
			registry := dockerRegistry(image)
			digest := sha256.Sum256([]byte(service.Credentials.Username + "\x00" + service.Credentials.Password))
			if previous, ok := activeCredential[registry]; !ok || previous != digest {
				if ok {
					args := []string{"logout"}
					if registry != "" {
						args = append(args, registry)
					}
					if _, err = boundedDockerOutput(ctx, env, docker, args...); err != nil {
						return nil, fmt.Errorf("clear service %q registry authentication: %w", serviceID, err)
					}
					delete(activeCredential, registry)
				}
				if err = dockerLogin(ctx, env, docker, registry, service.Credentials.Username, service.Credentials.Password); err != nil {
					return nil, fmt.Errorf("authenticate service %q registry %q: %w", serviceID, registry, err)
				}
				activeCredential[registry] = digest
			}
		} else {
			registry := dockerRegistry(image)
			if _, ok := activeCredential[registry]; ok {
				args := []string{"logout"}
				if registry != "" {
					args = append(args, registry)
				}
				if _, err = boundedDockerOutput(ctx, env, docker, args...); err != nil {
					return nil, fmt.Errorf("clear service %q registry authentication: %w", serviceID, err)
				}
				delete(activeCredential, registry)
			}
		}
		if err = r.pullContainerImage(ctx, processor, env, docker, image); err != nil {
			return nil, fmt.Errorf("pull service %q image: %w", serviceID, err)
		}
	}
	if _, err = boundedDockerOutput(ctx, env, docker, "network", "create", "--label", "com.buildkite.gha=true", "--label", b.owner, b.network); err != nil {
		return nil, fmt.Errorf("create job container network: %w", err)
	}
	for _, serviceID := range serviceOrder {
		service := services[serviceID]
		serviceNonce, randomErr := randomHex()
		if randomErr != nil {
			return nil, fmt.Errorf("create service %q identity: %w", serviceID, randomErr)
		}
		name := "buildkite-gha-service-" + serviceNonce
		// Track the exact name before create: Docker may create the container and
		// still return an ambiguous client/transport error.
		b.services = append(b.services, serviceContainer{id: serviceID, name: name})
		serviceArgs := []string{"create", "--name", name, "--label", "com.buildkite.gha=true", "--label", b.owner, "--network", b.network, "--network-alias", serviceID}
		serviceArgs = appendPublishedPorts(serviceArgs, service.Ports)
		options, optionErr := dockerArgumentList(service.Options)
		if optionErr != nil {
			return nil, fmt.Errorf("parse service %q options: %w", serviceID, optionErr)
		}
		if err = validateServiceOptions(options); err != nil {
			return nil, fmt.Errorf("service %q options: %w", serviceID, err)
		}
		serviceArgs = append(serviceArgs, options...)
		for _, key := range sortedKeys(service.Env) {
			serviceArgs = append(serviceArgs, "--env", key+"="+service.Env[key])
		}
		for _, volume := range service.Volumes {
			serviceArgs = append(serviceArgs, "--volume", volume)
		}
		if service.Entrypoint != "" {
			serviceArgs = append(serviceArgs, "--entrypoint", service.Entrypoint)
		}
		serviceArgs = append(serviceArgs, service.Image)
		command, commandErr := dockerArgumentList(service.Command)
		if commandErr != nil {
			return nil, fmt.Errorf("parse service %q command: %w", serviceID, commandErr)
		}
		serviceArgs = append(serviceArgs, command...)
		created, createErr := boundedDockerOutput(ctx, env, docker, serviceArgs...)
		if reference := strings.TrimSpace(created); reference != "" {
			b.services[len(b.services)-1].name = reference
			b.services[len(b.services)-1].created = true
		}
		if createErr != nil {
			if reconcileErr := b.reconcileCreatedService(ctx, len(b.services)-1); reconcileErr != nil {
				createErr = errors.Join(createErr, reconcileErr)
			}
			if b.services[len(b.services)-1].created {
				if trackErr := b.trackServiceVolumes(ctx, serviceID, b.services[len(b.services)-1].name); trackErr != nil {
					createErr = errors.Join(createErr, trackErr)
				}
			}
			return nil, fmt.Errorf("create service %q: %w", serviceID, createErr)
		}
		if err = b.trackServiceVolumes(ctx, serviceID, b.services[len(b.services)-1].name); err != nil {
			return nil, err
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
			b.serviceDiagnostics(ctx, processor, service.name)
			return nil, markHardJobFailure(portErr)
		}
		b.servicePorts[service.id] = expression.ServiceContext{ID: service.name, Network: b.network, Ports: ports}
	}
	for i := range b.services {
		service := &b.services[i]
		if err = b.waitForService(ctx, processor, service.id, service.name); err != nil {
			return nil, err
		}
		service.ready = true
	}
	if spec != nil {
		args := []string{"create", "--name", b.container, "--label", "com.buildkite.gha=true", "--label", b.owner, "--network", b.network,
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
		options, optionErr := containerpolicy.JobOptions(spec.Options)
		if optionErr != nil {
			return nil, fmt.Errorf("job container options: %w", optionErr)
		}
		args = append(args, options...)
		for _, name := range sortedKeys(spec.Env) {
			args = append(args, "--env", name+"="+spec.Env[name])
		}
		args = appendPublishedPorts(args, spec.Ports)
		for _, volume := range spec.Volumes {
			args = append(args, "--volume", volume)
		}
		args = append(args, spec.Image, "-c", "while :; do sleep 3600; done")
		if _, err = boundedDockerOutput(ctx, env, docker, args...); err != nil {
			createErr := err
			exists, queryErr := b.jobContainerExists(ctx)
			if queryErr != nil {
				createErr = errors.Join(createErr, fmt.Errorf("reconcile job container create: %w", queryErr))
			}
			if exists && len(spec.Volumes) != 0 {
				if trackErr := b.trackContainerVolumes(ctx, "job container", b.container); trackErr != nil {
					createErr = errors.Join(createErr, trackErr)
				}
			}
			return nil, fmt.Errorf("create job container: %w", createErr)
		}
		if len(spec.Volumes) != 0 {
			if err = b.trackContainerVolumes(ctx, "job container", b.container); err != nil {
				return nil, err
			}
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

func dockerRegistry(image string) string {
	parts := strings.Split(image, "/")
	if len(parts) >= 3 || len(parts) == 2 && strings.ContainsAny(parts[0], ".:") {
		return parts[0]
	}
	return ""
}

func dockerLogin(ctx context.Context, env map[string]string, docker, registry, username, password string) error {
	args := []string{"login"}
	if registry != "" {
		args = append(args, registry)
	}
	args = append(args, "--username", username, "--password-stdin")
	for attempt := range 3 {
		cmd := exec.CommandContext(ctx, docker, args...)
		cmd.Env = processEnv(env)
		cmd.Stdin = strings.NewReader(password + "\n")
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return errors.New("docker login failed")
}

func (r Runner) pullContainerImage(ctx context.Context, processor *commandProcessor, env map[string]string, docker, image string) error {
	var err error
	for attempt := range 3 {
		if err = r.runStreaming(ctx, processor, "", env, docker, "pull", image); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return err
}

func lineSet(output string) map[string]bool {
	result := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result[line] = true
		}
	}
	return result
}

func (b *jobContainerBackend) reconcileCreatedService(ctx context.Context, index int) error {
	output, err := boundedDockerOutput(ctx, b.env, b.docker, "ps", "--all", "--quiet", "--no-trunc", "--filter", "label="+b.owner)
	if err != nil {
		return fmt.Errorf("reconcile ambiguous service create: %w", err)
	}
	known := map[string]bool{}
	for i, service := range b.services {
		if i != index && service.created {
			known[service.name] = true
		}
	}
	var unmatched []string
	for reference := range lineSet(output) {
		if !known[reference] {
			unmatched = append(unmatched, reference)
		}
	}
	if len(unmatched) > 1 {
		return fmt.Errorf("reconcile ambiguous service create: found %d new owned containers", len(unmatched))
	}
	if len(unmatched) == 1 {
		b.services[index].name = unmatched[0]
		b.services[index].created = true
	}
	return nil
}

func (b *jobContainerBackend) trackServiceVolumes(ctx context.Context, serviceID, reference string) error {
	return b.trackContainerVolumes(ctx, fmt.Sprintf("service %q", serviceID), reference)
}

func (b *jobContainerBackend) trackContainerVolumes(ctx context.Context, subject, reference string) error {
	const format = `{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}`
	output, err := boundedDockerOutput(ctx, b.env, b.docker, "inspect", "--format", format, reference)
	if err != nil {
		return fmt.Errorf("inspect %s volumes: %w", subject, err)
	}
	for volume := range lineSet(output) {
		if !b.existingVolumes[volume] && !slices.Contains(b.ownedVolumes, volume) {
			b.ownedVolumes = append(b.ownedVolumes, volume)
		}
	}
	return nil
}

func validateServiceOptions(options []string) error {
	for _, option := range options {
		if option == "--network" || option == "--net" || strings.HasPrefix(option, "--network=") || strings.HasPrefix(option, "--net=") {
			return fmt.Errorf("network override %q is unsupported", option)
		}
	}
	return nil
}

// dockerArgumentList matches the argument splitting used by the pinned
// actions/runner ProcessStartInfo.Arguments path. Single quotes are ordinary
// characters; double quotes group arguments; backslashes only escape quotes.
func dockerArgumentList(value string) ([]string, error) {
	return containerpolicy.ArgumentList(value), nil
}

var dockerPortLine = regexp.MustCompile(`^([0-9]+)/([A-Za-z0-9]+) -> (?:[^:]+|\[[^]]+\]):([0-9]+)$`)

func (b *jobContainerBackend) readServicePorts(ctx context.Context, id, name string, _ []string) (map[string]string, error) {
	out, err := boundedDockerOutput(ctx, b.env, b.docker, "port", name)
	if err != nil {
		return nil, fmt.Errorf("query service %q ports: %w", id, err)
	}
	result := map[string]string{}
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
		if e1 != nil || e2 != nil || cp < 1 || cp > 65535 || hp < 1 || hp > 65535 {
			return nil, fmt.Errorf("service %q has invalid port mapping %q", id, line)
		}
		// GitHub's runner exposes ports in a dictionary keyed only by numeric
		// container port, so later Docker mappings intentionally replace earlier
		// TCP/UDP mappings for the same port.
		result[m[1]] = m[3]
	}
	return result, nil
}

func appendPublishedPorts(args, ports []string) []string {
	for _, port := range ports {
		args = append(args, "--publish", port)
	}
	return args
}

func (b *jobContainerBackend) waitForService(ctx context.Context, processor *commandProcessor, serviceID, name string) error {
	const format = `{{if .State.Health}}{{.State.Health.Status}}{{end}}`
	delay := 2 * time.Second
	for {
		status, err := boundedDockerOutput(ctx, b.env, b.docker, "inspect", "--format", format, name)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("wait for service %q readiness: %w", serviceID, ctx.Err())
			}
			b.serviceDiagnostics(ctx, processor, name)
			return fmt.Errorf("inspect service %q readiness: %w", serviceID, err)
		}
		switch strings.TrimSpace(status) {
		case "", "healthy":
			return nil
		case "starting":
		default:
			b.serviceDiagnostics(ctx, processor, name)
			return fmt.Errorf("service %q failed readiness with status %q", serviceID, strings.TrimSpace(status))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			b.serviceDiagnostics(ctx, processor, name)
			return fmt.Errorf("wait for service %q readiness: %w", serviceID, ctx.Err())
		case <-timer.C:
		}
		if delay < 32*time.Second {
			delay *= 2
			if delay > 32*time.Second {
				delay = 32 * time.Second
			}
		}
	}
}

func (b *jobContainerBackend) serviceDiagnostics(parent context.Context, processor *commandProcessor, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), serviceDiagnosticTimeout)
	defer cancel()
	b.emitServiceLogOutput(processor, b.serviceLogOutput(ctx, name))
}

func (b *jobContainerBackend) serviceLogOutput(ctx context.Context, name string) string {
	output, _ := boundedDockerCombinedOutput(ctx, b.env, b.docker, "logs", "--tail", serviceLogTail, name)
	return output
}

func (b *jobContainerBackend) emitServiceLogOutput(processor *commandProcessor, output string) {
	for _, line := range strings.Split(strings.TrimSuffix(strings.ReplaceAll(output, "\r\n", "\n"), "\n"), "\n") {
		if line != "" {
			processor.writeLiteral(processor.stderr, line)
		}
	}
}

func (b *jobContainerBackend) emitReadyServiceLogs(ctx context.Context) {
	logs := make([]string, len(b.services))
	logCtx, cancel := context.WithTimeout(ctx, serviceDiagnosticTimeout)
	defer cancel()
	var wait sync.WaitGroup
	for i := range b.services {
		if !b.services[i].ready {
			continue
		}
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			logs[index] = b.serviceLogOutput(logCtx, b.services[index].name)
		}(i)
	}
	wait.Wait()
	for _, output := range logs {
		b.emitServiceLogOutput(b.processor, output)
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
	if err := validateEnvironmentNames(env); err != nil {
		return err
	}
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
	dockerCtx, stopDocker := context.WithCancel(context.WithoutCancel(ctx))
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
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminationBound)
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

func (b *jobContainerBackend) cleanup(parent context.Context) error {
	// Each service gets enough budget for its graceful stop in addition to the
	// base budget, which remains reserved for the job, network, and verification.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), jobContainerCleanupTimeout(b.runner.cleanupTimeout(), len(b.services)))
	defer cancel()
	var err error
	var out string
	var queryErr error
	if b.container != "" {
		out, queryErr = boundedDockerOutput(ctx, b.env, b.docker, "ps", "--all", "--quiet", "--filter", "label="+b.owner, "--filter", "name=^/"+b.container+"$")
		if queryErr != nil {
			err = errors.Join(err, fmt.Errorf("query job container: %w", queryErr))
		}
		if queryErr != nil || strings.TrimSpace(out) != "" {
			_, e := boundedDockerOutput(ctx, b.env, b.docker, "rm", "--force", "--volumes", b.container)
			if e != nil {
				err = errors.Join(err, fmt.Errorf("remove job container: %w", e))
			}
		}
	}
	b.emitReadyServiceLogs(ctx)
	for i := range len(b.services) {
		service := b.services[i]
		if service.created {
			name := service.name
			exists, queryErr := b.serviceContainerExists(ctx, name)
			if queryErr != nil {
				err = errors.Join(err, fmt.Errorf("query service container before stop: %w", queryErr))
			}
			if queryErr == nil && !exists {
				continue
			}
			_, stopErr := boundedDockerOutput(ctx, b.env, b.docker, "stop", "--time", "2", name)
			exists, queryErr = b.serviceContainerExists(ctx, name)
			if queryErr != nil {
				err = errors.Join(err, fmt.Errorf("query service container after stop: %w", queryErr))
			}
			if stopErr != nil && (queryErr != nil || exists) {
				err = errors.Join(err, fmt.Errorf("stop service container: %w", stopErr))
			}
			if queryErr == nil && !exists {
				continue
			}
			if _, removeErr := boundedDockerOutput(ctx, b.env, b.docker, "rm", "--force", "--volumes", name); removeErr != nil {
				exists, queryErr = b.serviceContainerExists(ctx, name)
				if queryErr != nil {
					err = errors.Join(err, fmt.Errorf("query service container after remove: %w", queryErr))
				}
				if queryErr != nil || exists {
					err = errors.Join(err, fmt.Errorf("remove service container: %w", removeErr))
				}
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
	if len(b.ownedVolumes) != 0 {
		if _, e := boundedDockerOutput(ctx, b.env, b.docker, append([]string{"volume", "rm", "--force"}, b.ownedVolumes...)...); e != nil {
			err = errors.Join(err, fmt.Errorf("remove job volumes: %w", e))
		}
		remaining, e := boundedDockerOutput(ctx, b.env, b.docker, "volume", "ls", "--quiet")
		if e != nil {
			err = errors.Join(err, fmt.Errorf("verify owned Docker volume cleanup query: %w", e))
		} else {
			left := lineSet(remaining)
			for _, volume := range b.ownedVolumes {
				if left[volume] {
					err = errors.Join(err, fmt.Errorf("verify owned Docker cleanup: leftover volume %q", volume))
				}
			}
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
	if e := removeDockerConfig(b.config); e != nil {
		err = errors.Join(err, e)
	}
	return err
}

func (b *jobContainerBackend) serviceContainerExists(ctx context.Context, id string) (bool, error) {
	out, err := boundedDockerOutput(ctx, b.env, b.docker, "ps", "--all", "--quiet", "--filter", "label="+b.owner, "--filter", "id="+id)
	return strings.TrimSpace(out) != "", err
}

func (b *jobContainerBackend) jobContainerExists(ctx context.Context) (bool, error) {
	out, err := boundedDockerOutput(ctx, b.env, b.docker, "ps", "--all", "--quiet", "--filter", "label="+b.owner, "--filter", "name=^/"+b.container+"$")
	return strings.TrimSpace(out) != "", err
}

func removeDockerConfig(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove private Docker configuration: %w", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("remove private Docker configuration: path remains")
		}
		return fmt.Errorf("verify private Docker configuration cleanup: %w", err)
	}
	return nil
}

func jobContainerCleanupTimeout(base time.Duration, services int) time.Duration {
	return base + time.Duration(services)*3*time.Second
}
