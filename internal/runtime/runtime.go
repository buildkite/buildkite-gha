// Package runtime executes explicitly resolved GitHub Actions action steps.
package runtime

import (
	"bufio"
	"context"
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
	Stdout          io.Writer
	Stderr          io.Writer
	Node20          string
	Node24          string
	ManagedNodeRoot string
	Docker          string
	CleanupTimeout  time.Duration
	InterruptGrace  time.Duration
	TerminateGrace  time.Duration
	Secrets         SecretResolver
	Redactor        Redactor
	Actions         ActionMaterializer
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
}

// DockerAction is an already-resolved local Docker action.
type DockerAction struct {
	Name       string
	Path       string
	Dockerfile string
	Workspace  string
	Env        map[string]string
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
	return r.runDocker(ctx, newCommandProcessor(r.stdout(), r.stderr()), action)
}

func (r Runner) runDocker(ctx context.Context, processor *commandProcessor, action DockerAction) (result Result, err error) {
	result = newResult()
	docker := r.Docker
	if docker == "" {
		docker, err = exec.LookPath("docker")
		if err != nil {
			return result, fmt.Errorf("discover Docker: %w", err)
		}
	}
	dockerfile := action.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	image := fmt.Sprintf("buildkite-gha-runtime-%d-%d", os.Getpid(), time.Now().UnixNano())
	build := exec.CommandContext(ctx, docker, "build", "--quiet", "--tag", image, "--file", filepath.Join(action.Path, dockerfile), action.Path)
	build.Env = processEnv(nil)
	imageOutput, err := build.CombinedOutput()
	if err != nil {
		return result, fmt.Errorf("build Docker action %q: %w: %s", action.Name, err, strings.TrimSpace(string(imageOutput)))
	}
	defer func() {
		remove := exec.Command(docker, "image", "rm", image)
		remove.Env = processEnv(nil)
		if output, removeErr := remove.CombinedOutput(); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove Docker action image: %w: %s", removeErr, strings.TrimSpace(string(output))))
		}
	}()

	files, err := newCommandFiles()
	if err != nil {
		return result, err
	}
	defer func() { _ = os.RemoveAll(files.dir) }()
	args := []string{"run", "--rm", "--volume", files.dir + ":/github/file_commands"}
	if action.Workspace != "" {
		args = append(args, "--volume", action.Workspace+":/github/workspace")
	}
	for _, name := range sortedKeys(action.Env) {
		args = append(args, "--env", name+"="+action.Env[name])
	}
	if action.Workspace != "" {
		args = append(args, "--env", "GITHUB_WORKSPACE=/github/workspace")
	}
	args = append(args,
		"--env", "GITHUB_OUTPUT=/github/file_commands/output",
		"--env", "GITHUB_ENV=/github/file_commands/env",
		"--env", "GITHUB_PATH=/github/file_commands/path",
		"--env", "GITHUB_STATE=/github/file_commands/state",
		"--env", "GITHUB_STEP_SUMMARY=/github/file_commands/summary",
		image,
	)
	if err := r.runStreaming(ctx, processor, "", nil, docker, args...); err != nil {
		return result, fmt.Errorf("run Docker action %q: %w", action.Name, err)
	}
	if _, err := files.apply(&result, nil); err != nil {
		return result, fmt.Errorf("process Docker action %q file commands: %w", action.Name, err)
	}
	return result, nil
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
	if err := r.runProcess(ctx, processor, action.Path, env, result, stateOut, node, filepath.Join(action.Path, entry)); err != nil {
		return fmt.Errorf("JavaScript action %q entry %q: %w", action.Name, entry, err)
	}
	return nil
}

func (r Runner) runProcess(ctx context.Context, processor *commandProcessor, dir string, env map[string]string, result *Result, state map[string]string, name string, args ...string) error {
	files, err := newCommandFiles()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(files.dir) }()
	env = mergeStringMaps(env, map[string]string{
		"GITHUB_OUTPUT":       files.output,
		"GITHUB_ENV":          files.env,
		"GITHUB_PATH":         files.path,
		"GITHUB_STATE":        files.state,
		"GITHUB_STEP_SUMMARY": files.summary,
	})
	runErr := r.runStreaming(ctx, processor, dir, env, name, args...)
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
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
	waitErr := cmd.Wait()
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
