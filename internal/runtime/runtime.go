// Package runtime executes explicitly resolved GitHub Actions action steps.
package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/source"
)

const (
	defaultCleanupTimeout = 10 * time.Second
	// Match actions/runner ProcessInvoker's graceful cancellation windows.
	defaultInterruptGrace = 7500 * time.Millisecond
	defaultTerminateGrace = 2500 * time.Millisecond
	maxStreamLineBytes    = 1024 * 1024
)

// Runner executes verified actions using explicitly configured host tools.
type Runner struct {
	Stdout            io.Writer
	Stderr            io.Writer
	Node20            string
	Node24            string
	ManagedNodeRoot   string
	Docker            string
	RuntimeExecutable string
	Git               string
	CleanupTimeout    time.Duration
	InterruptGrace    time.Duration
	TerminateGrace    time.Duration
	Secrets           SecretResolver
	Redactor          Redactor
	Actions           ActionMaterializer
	runnerTemp        string
	implicitJobPATH   string
	explicitJobPATH   bool
	jobContainer      *jobContainerBackend
}

// JavaScriptAction is an already-resolved local JavaScript action.
type JavaScriptAction struct {
	Name   string
	Path   string
	Pre    string
	Main   string
	Post   string
	Inputs map[string]string
	Env    map[string]string

	nodeMajor int
}

// DockerAction is an already-resolved local Docker action.
type DockerAction struct {
	Name         string
	Path         string
	SourceRoot   string
	SourceDigest string
	Dockerfile   string
	Workspace    string
	Env          map[string]string
	runnerTemp   string
	explicitPATH bool
}

// Result contains file-command effects produced by an action or lifecycle.
type Result struct {
	Outputs map[string]string
	Env     map[string]string
	State   map[string]string
	Summary string
	Paths   []string

	pathBase    string
	pathBaseSet bool
}

// RunDocker builds and executes an explicitly resolved local Docker action.
func (r Runner) RunDocker(ctx context.Context, action DockerAction) (result Result, err error) {
	callerWorkspace := action.Workspace != ""
	if !callerWorkspace {
		action.Workspace, err = os.MkdirTemp("", "buildkite-gha-workspace-")
		if err != nil {
			return newResult(), fmt.Errorf("create workspace: %w", err)
		}
		defer func() { _ = os.RemoveAll(action.Workspace) }()
	}
	abs, e := filepath.Abs(action.Workspace)
	if e != nil {
		return newResult(), fmt.Errorf("resolve workspace: %w", e)
	}
	action.Workspace = abs
	_, action.explicitPATH = action.Env["PATH"]
	workspace, e := os.Open(abs)
	if e != nil {
		return newResult(), e
	}
	defer func() { err = errors.Join(err, workspace.Close()) }()
	workspaceInfo, e := workspace.Stat()
	if e != nil {
		return newResult(), e
	}
	if e := workspace.Chmod(0o777); e != nil {
		return newResult(), fmt.Errorf("make Docker workspace writable: %w", e)
	}
	if callerWorkspace {
		defer func() { err = errors.Join(err, workspace.Chmod(workspaceInfo.Mode().Perm())) }()
	}
	temp, e := os.MkdirTemp("", "buildkite-gha-runner-")
	if e != nil {
		return newResult(), e
	}
	defer func() { _ = os.RemoveAll(temp) }()
	if e := os.Chmod(temp, 0o777); e != nil {
		return newResult(), fmt.Errorf("make Docker runner temp writable: %w", e)
	}
	action.runnerTemp = temp
	return r.runDocker(ctx, newCommandProcessor(r.stdout(), r.stderr()), action)
}

func (r Runner) runDocker(ctx context.Context, processor *commandProcessor, action DockerAction) (result Result, err error) {
	result = newResult()
	if action.runnerTemp == "" {
		action.runnerTemp = r.runnerTemp
	}
	action.explicitPATH = action.explicitPATH || r.explicitJobPATH
	if action.Workspace == "" || action.runnerTemp == "" {
		return result, errors.New("docker workspace and runner temp are required")
	}
	docker := r.Docker
	if docker == "" {
		docker, err = exec.LookPath("docker")
		if err != nil {
			return result, fmt.Errorf("discover Docker: %w", err)
		}
	}
	if action.Dockerfile != "" && action.Dockerfile != "Dockerfile" {
		return result, fmt.Errorf("docker action requires fixed Dockerfile")
	}
	sourceRoot := action.SourceRoot
	if sourceRoot == "" {
		sourceRoot = action.Path
	}
	expected := action.SourceDigest
	if expected == "" {
		expected, err = source.DigestTree(sourceRoot)
		if err != nil {
			return result, fmt.Errorf("digest Docker action source: %w", err)
		}
	}
	stage, err := stageDockerSource(sourceRoot, action.Path, expected)
	if err != nil {
		return result, err
	}
	defer func() { _ = os.RemoveAll(stage.root) }()
	dockerConfig, err := os.MkdirTemp("", "buildkite-gha-docker-config-")
	if err != nil {
		return result, fmt.Errorf("create private Docker configuration: %w", err)
	}
	if err := os.Chmod(dockerConfig, 0o700); err != nil {
		_ = os.RemoveAll(dockerConfig)
		return result, err
	}
	defer func() { _ = os.RemoveAll(dockerConfig) }()
	dockerEnv := map[string]string{"DOCKER_CONFIG": dockerConfig}
	inspection, inspectErr := boundedDockerOutput(ctx, dockerEnv, docker, "buildx", "inspect", "default")
	driver := dockerBuilderDriver(inspection)
	if inspectErr != nil || driver != "docker" {
		if inspectErr != nil {
			return result, fmt.Errorf("inspect default Docker builder: %w", inspectErr)
		}
		if driver == "" {
			return result, fmt.Errorf("could not determine default Docker builder driver from inspect output")
		}
		return result, fmt.Errorf("default Docker builder has non-local driver %q", driver)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, fmt.Errorf("create Docker invocation identity: %w", err)
	}
	id := hex.EncodeToString(nonce[:])
	owner := "com.buildkite.gha.owner=" + id
	image, container := "buildkite-gha-image-"+id, "buildkite-gha-container-"+id
	built, ran := false, false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), r.cleanupTimeout())
		defer cancel()
		var cleanupErr error
		if ran {
			out, queryErr := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "ps", "--all", "--quiet", "--filter", "label="+owner, "--filter", "name=^/"+container+"$")
			containerMayExist := strings.TrimSpace(out) != ""
			if queryErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("query owned Docker container: %w", queryErr))
				containerMayExist = true
			}
			if containerMayExist {
				if _, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "stop", "--time", "2", container); e != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop owned Docker container: %w", e))
				}
				if _, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "rm", "--force", container); e != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove owned Docker container: %w", e))
				}
			}
		}
		if built {
			out, queryErr := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "image", "ls", "--all", "--quiet", "--filter", "label="+owner, image)
			imageMayExist := strings.TrimSpace(out) != ""
			if queryErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("query owned Docker image: %w", queryErr))
				imageMayExist = true
			}
			if imageMayExist {
				if _, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "image", "rm", "--force", image); e != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove owned Docker image: %w", e))
				}
			}
		}
		for _, query := range [][]string{{"ps", "--all", "--quiet", "--filter", "label=" + owner}, {"image", "ls", "--all", "--quiet", "--filter", "label=" + owner}} {
			out, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, query...)
			if e != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("verify owned Docker cleanup: %w", e))
			} else if strings.TrimSpace(out) != "" {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("owned Docker resources remain after cleanup"))
			}
		}
		err = errors.Join(err, cleanupErr)
	}()
	buildArgs := []string{"buildx", "build", "--builder", "default", "--load", "--tag", image, "--label", owner, "--file", filepath.Join(stage.action, "Dockerfile"), stage.action}
	built = true // A failed build may still have created the tagged image.
	if err := r.runStreaming(ctx, processor, "", dockerEnv, docker, buildArgs...); err != nil {
		return result, fmt.Errorf("build Docker action %q: %w", action.Name, err)
	}

	files, err := newCommandFiles()
	if err != nil {
		return result, err
	}
	defer func() { _ = files.cleanup() }()
	if err := files.allowContainerWrites(); err != nil {
		return result, err
	}
	if err := validateDockerMountPath(files.dir); err != nil {
		return result, err
	}
	if action.Workspace != "" {
		if err := validateDockerMountPath(action.Workspace); err != nil {
			return result, err
		}
		if err := validateDockerMountPath(action.runnerTemp); err != nil {
			return result, err
		}
	}
	args := []string{"run", "--name", container, "--label", owner, "--mount", "type=bind,source=" + files.dir + ",target=/github/file_commands"}
	if action.Workspace != "" {
		args = append(args, "--mount", "type=bind,source="+action.Workspace+",target=/github/workspace", "--mount", "type=bind,source="+action.runnerTemp+",target=/github/runner_temp", "--workdir", "/github/workspace")
	}
	for _, name := range sortedKeys(action.Env) {
		if name == "RUNNER_TOOL_CACHE" || (name == "PATH" && !action.explicitPATH) || name == "GITHUB_WORKSPACE" || name == "RUNNER_TEMP" {
			continue
		}
		args = append(args, "--env", name+"="+action.Env[name])
	}
	if action.Workspace != "" {
		args = append(args, "--env", "GITHUB_WORKSPACE=/github/workspace")
	}
	args = append(args,
		"--env", "RUNNER_TEMP=/github/runner_temp",
		"--env", "GITHUB_OUTPUT=/github/file_commands/output",
		"--env", "GITHUB_ENV=/github/file_commands/env",
		"--env", "GITHUB_PATH=/github/file_commands/path",
		"--env", "GITHUB_STATE=/github/file_commands/state",
		"--env", "GITHUB_STEP_SUMMARY=/github/file_commands/summary",
		image,
	)
	ran = true
	runErr := r.runStreaming(ctx, processor, "", dockerEnv, docker, args...)
	if runErr != nil {
		runErr = fmt.Errorf("run Docker action %q: %w", action.Name, runErr)
	}
	_, fileErr := files.apply(&result, nil)
	if fileErr != nil {
		fileErr = fmt.Errorf("process Docker action %q file commands: %w", action.Name, fileErr)
	}
	return result, errors.Join(runErr, fileErr)
}

type stagedDockerSource struct{ root, action string }

func stageDockerSource(sourceRoot, actionPath, expected string) (stagedDockerSource, error) {
	before, err := source.DigestTree(sourceRoot)
	if err != nil || before != expected {
		return stagedDockerSource{}, fmt.Errorf("docker action source digest mismatch before staging")
	}
	rel, err := filepath.Rel(sourceRoot, actionPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return stagedDockerSource{}, fmt.Errorf("docker action path escapes verified source")
	}
	root, err := os.MkdirTemp("", "buildkite-gha-docker-source-")
	if err != nil {
		return stagedDockerSource{}, err
	}
	fail := func(e error) (stagedDockerSource, error) { _ = os.RemoveAll(root); return stagedDockerSource{}, e }
	err = filepath.WalkDir(sourceRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		r, _ := filepath.Rel(sourceRoot, p)
		if r == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		to := filepath.Join(root, r)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		info, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("docker action source contains symlink or special file")
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(to, data, mode)
	})
	if err != nil {
		return fail(fmt.Errorf("stage Docker action source: %w", err))
	}
	stagedDigest, e := source.DigestTree(root)
	if e != nil || stagedDigest != expected {
		return fail(fmt.Errorf("staged Docker action digest mismatch"))
	}
	after, e := source.DigestTree(sourceRoot)
	if e != nil || after != expected {
		return fail(fmt.Errorf("docker action source mutated during staging"))
	}
	return stagedDockerSource{root: root, action: filepath.Join(root, rel)}, nil
}

func validateDockerMountPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, ",\"'\n\r\x00") {
		return fmt.Errorf("path cannot be represented by Docker mount grammar")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("docker mount path must be an existing directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("docker mount path must be an existing directory")
	}
	return nil
}

func dockerBuilderDriver(inspection string) string {
	var driver string
	for _, line := range strings.Split(inspection, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "Driver:" {
			continue
		}
		if driver != "" && driver != fields[1] {
			return ""
		}
		driver = fields[1]
	}
	return driver
}

func boundedDockerOutput(ctx context.Context, env map[string]string, docker string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = processEnv(env)
	var output strings.Builder
	w := &limitedWriter{writer: &output, remaining: 4096}
	cmd.Stdout, cmd.Stderr = w, io.Discard
	err := cmd.Run()
	if w.exceeded {
		return output.String(), fmt.Errorf("docker output exceeds limit")
	}
	return output.String(), err
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
	exceeded  bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.exceeded = true
		if w.remaining > 0 {
			_, _ = w.writer.Write(p[:w.remaining])
			w.remaining = 0
		}
		return len(p), nil
	}
	w.remaining -= len(p)
	return w.writer.Write(p)
}

func (r Runner) runJavaScriptPhase(ctx context.Context, processor *commandProcessor, node string, action JavaScriptAction, entry string, stateEnv, stateOut map[string]string, result *Result) error {
	env := mergeStringMaps(result.Env, action.Env, actionInputEnv(action.Inputs))
	if path, ok := result.Env["PATH"]; ok {
		env["PATH"] = path
	}
	env["GITHUB_ACTION_PATH"] = action.Path
	for name, value := range stateEnv {
		env["STATE_"+name] = value
	}
	entrypoint := filepath.Join(action.Path, entry)
	if r.jobContainer != nil {
		if err := r.jobContainer.probeNode(ctx, node, action.nodeMajor); err != nil {
			return err
		}
		absNode, err := filepath.Abs(node)
		if err != nil {
			return err
		}
		node = r.jobContainer.containerPath(absNode)
		entrypoint = r.jobContainer.containerPath(entrypoint)
	}
	if err := r.runProcess(ctx, processor, action.Path, env, result, stateOut, node, entrypoint); err != nil {
		return fmt.Errorf("JavaScript action %q entry %q: %w", action.Name, entry, err)
	}
	return nil
}

func (r Runner) runProcess(ctx context.Context, processor *commandProcessor, dir string, env map[string]string, result *Result, state map[string]string, name string, args ...string) error {
	var files commandFiles
	var err error
	if r.jobContainer != nil {
		files, err = newCommandFilesUnder(r.runnerTemp)
	} else {
		files, err = newCommandFiles()
	}
	if err != nil {
		return err
	}
	defer func() { _ = files.cleanup() }()
	commandPaths := map[string]string{"GITHUB_OUTPUT": files.output, "GITHUB_ENV": files.env, "GITHUB_PATH": files.path, "GITHUB_STATE": files.state, "GITHUB_STEP_SUMMARY": files.summary}
	if r.jobContainer != nil {
		if err := files.allowContainerWrites(); err != nil {
			return err
		}
		for key, path := range commandPaths {
			commandPaths[key] = r.jobContainer.containerPath(path)
		}
	}
	env = mergeStringMaps(env, commandPaths)
	var runErr error
	if r.jobContainer != nil {
		runErr = r.jobContainer.exec(ctx, r, processor, dir, env, name, args...)
	} else {
		runErr = r.runStreaming(ctx, processor, dir, env, name, args...)
	}
	effects, fileErr := files.apply(result, state)
	if fileErr == nil && (effects.pathSet || len(effects.paths) > 0) {
		pathEnv := map[string]string{"PATH": env["PATH"]}
		if effects.pathSet {
			pathEnv["PATH"] = effects.pathBase
		}
		applyPaths(pathEnv, effects.paths)
		result.Env["PATH"] = pathEnv["PATH"]
	}
	return errors.Join(runErr, fileErr)
}

func (r Runner) runStreaming(ctx context.Context, processor *commandProcessor, dir string, env map[string]string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = processEnv(env)
	configureProcessGroup(cmd)
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return err
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return err
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	defer func() {
		_ = stdout.Close()
		_ = stderr.Close()
	}()

	processDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(processDone)
	}()
	finished := make(chan struct{})
	terminationDone := make(chan struct{})
	go func() {
		defer close(terminationDone)
		terminateProcessGroup(ctx, cmd.Process.Pid, r.interruptGrace(), r.terminateGrace(), finished)
	}()

	var wg sync.WaitGroup
	streamErrs := make([]error, 2)
	stream := func(index int, label string, reader io.Reader, target io.Writer) {
		defer wg.Done()
		streamErrs[index] = streamLines(reader, func(line string) {
			processor.process(target, line)
		}, processor.suppress)
		if streamErrs[index] != nil {
			streamErrs[index] = fmt.Errorf("%s stream: %w", label, streamErrs[index])
		}
	}
	wg.Add(2)
	go stream(0, "stdout", stdout, processor.stdout)
	go stream(1, "stderr", stderr, processor.stderr)
	wg.Wait()
	<-processDone
	close(finished)
	<-terminationDone
	if ctx.Err() != nil {
		waitErr = ctx.Err()
	}
	if waitErr != nil {
		waitErr = fmt.Errorf("process %s: %w", name, waitErr)
	}
	return errors.Join(waitErr, streamErrs[0], streamErrs[1])
}

func (r Runner) interruptGrace() time.Duration {
	if r.InterruptGrace > 0 {
		return r.InterruptGrace
	}
	return defaultInterruptGrace
}

func (r Runner) terminateGrace() time.Duration {
	if r.TerminateGrace > 0 {
		return r.TerminateGrace
	}
	return defaultTerminateGrace
}

func streamLines(reader io.Reader, process func(string), suppress func()) error {
	buffered := bufio.NewReader(reader)
	line := make([]byte, 0, buffered.Size())
	oversized := false
	sawOversized := false
	for {
		fragment, err := buffered.ReadSlice('\n')
		if !oversized {
			contentBytes := len(line) + len(fragment)
			if err == nil {
				contentBytes--
			}
			if contentBytes > maxStreamLineBytes {
				line = line[:0]
				oversized = true
				sawOversized = true
				suppress()
			} else {
				line = append(line, fragment...)
			}
		}

		if err == nil {
			if oversized {
			} else {
				process(strings.TrimSuffix(string(line), "\n"))
			}
			line = line[:0]
			oversized = false
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if oversized {
			} else if len(line) != 0 {
				process(string(line))
			}
			if sawOversized {
				return fmt.Errorf("line exceeds %d-byte limit and was discarded", maxStreamLineBytes)
			}
			return nil
		}
		if sawOversized {
			return errors.Join(fmt.Errorf("line exceeds %d-byte limit and was discarded", maxStreamLineBytes), err)
		}
		return err
	}
}

// DiscoverNode resolves an explicit Node binary or a binary in the managed
// runtime root, and rejects binaries that do not report the requested major.
// It deliberately does not fall back to PATH.
func DiscoverNode(major int, explicit, managedRoot string) (string, error) {
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else if managedRoot != "" {
		name := "node"
		if runtime.GOOS == "windows" {
			name = "node.exe"
		}
		version := fmt.Sprintf("%d", major)
		candidates = append(candidates,
			filepath.Join(managedRoot, "node"+version, "bin", name),
			filepath.Join(managedRoot, "node", version, "bin", name),
			filepath.Join(managedRoot, "bin", name),
		)
	} else {
		return "", fmt.Errorf("node %d is not configured: set the matching Runner.Node field or Runner.ManagedNodeRoot", major)
	}

	var failures []string
	for _, candidate := range candidates {
		command := exec.Command(candidate, "--version")
		command.Env = processEnv(nil)
		output, err := command.CombinedOutput()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		version := strings.TrimSpace(string(output))
		if strings.HasPrefix(version, fmt.Sprintf("v%d.", major)) {
			return candidate, nil
		}
		failures = append(failures, fmt.Sprintf("%s: reported %q", candidate, version))
	}
	return "", fmt.Errorf("node %d discovery failed: %s", major, strings.Join(failures, "; "))
}

// DiscoverNode24 retains the original Node 24 discovery API.
func DiscoverNode24(explicit, managedRoot string) (string, error) {
	return DiscoverNode(24, explicit, managedRoot)
}

func newResult() Result {
	return Result{Outputs: make(map[string]string), Env: make(map[string]string), State: make(map[string]string)}
}

func (r Runner) stdout() io.Writer {
	if r.Stdout != nil {
		return r.Stdout
	}
	return io.Discard
}

func (r Runner) stderr() io.Writer {
	if r.Stderr != nil {
		return r.Stderr
	}
	return io.Discard
}

func actionInputEnv(inputs map[string]string) map[string]string {
	env := make(map[string]string, len(inputs))
	for name, value := range inputs {
		name = strings.ToUpper(strings.ReplaceAll(name, " ", "_"))
		env["INPUT_"+name] = value
	}
	return env
}

func mapEnv(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for _, name := range sortedKeys(values) {
		env = append(env, name+"="+values[name])
	}
	return env
}

func processEnv(overrides map[string]string) []string {
	values := make(map[string]string, 6+len(overrides))
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"} {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	if _, ok := values["PATH"]; !ok {
		values["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	if _, ok := values["HOME"]; !ok {
		if home, err := os.UserHomeDir(); err == nil {
			values["HOME"] = home
		}
	}
	if _, ok := values["TMPDIR"]; !ok {
		values["TMPDIR"] = os.TempDir()
	}
	for name, value := range overrides {
		values[name] = value
	}
	return mapEnv(values)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type commandProcessor struct {
	mu      sync.Mutex
	stdout  io.Writer
	stderr  io.Writer
	masks   []string
	discard bool
}

func newCommandProcessor(stdout, stderr io.Writer) *commandProcessor {
	return &commandProcessor{stdout: stdout, stderr: stderr}
}

func (p *commandProcessor) process(target io.Writer, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discard {
		return
	}
	if value, ok := workflowCommand(line, "add-mask"); ok {
		p.addMaskLocked(value)
		return
	}
	for _, mask := range p.masks {
		line = strings.ReplaceAll(line, mask, "***")
	}
	_, _ = fmt.Fprintln(target, line)
}

func (p *commandProcessor) suppress() {
	p.mu.Lock()
	p.discard = true
	p.mu.Unlock()
}

func (p *commandProcessor) addMask(value string) {
	p.mu.Lock()
	p.addMaskLocked(value)
	p.mu.Unlock()
}

func (p *commandProcessor) maskValues() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.masks...)
}

func (p *commandProcessor) addMaskLocked(value string) {
	if value == "" {
		return
	}
	p.masks = append(p.masks, value)
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if line != "" && line != value {
			p.masks = append(p.masks, line)
		}
	}
}

func workflowCommand(line, command string) (string, bool) {
	if !strings.HasPrefix(line, "::") {
		return "", false
	}
	separator := strings.Index(line[2:], "::")
	if separator < 0 {
		return "", false
	}
	separator += 2
	header := line[2:separator]
	name, _, _ := strings.Cut(header, " ")
	if !strings.EqualFold(name, command) {
		return "", false
	}
	return decodeCommandValue(line[separator+2:]), true
}

func decodeCommandValue(value string) string {
	replacer := strings.NewReplacer("%0D", "\r", "%0A", "\n", "%25", "%")
	return replacer.Replace(value)
}
