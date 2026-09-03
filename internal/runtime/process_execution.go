package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// Match actions/runner ProcessInvoker's graceful cancellation windows.
	defaultInterruptGrace = 7500 * time.Millisecond
	defaultTerminateGrace = 2500 * time.Millisecond
	maxStreamLineBytes    = 1024 * 1024
)

// Process execution bridges action invocations to host or container processes.
// It supplies the explicit environment and file-command paths, streams output
// through the command output processor, and owns cancellation of the process group.
func (r *jobRun) runProcess(ctx context.Context, processor *commandOutputProcessor, dir string, env map[string]string, result *Result, state map[string]string, name string, args ...string) error {
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
		runErr = r.jobContainer.exec(ctx, r.Runner, processor, dir, env, name, args...)
	} else {
		runErr = r.runStreaming(ctx, processor, dir, env, name, args...)
	}
	runErr = markStepProcessExit(runErr)
	effects, fileErr := files.apply(result, state)
	effects.reportSummaryUploadFailure(processor)
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

func (effects fileCommandEffects) reportSummaryUploadFailure(processor *commandOutputProcessor) {
	if effects.summaryBytes <= maxCommandFileBytes {
		return
	}
	_ = processor.process(processor.stderr, fmt.Sprintf("GITHUB_STEP_SUMMARY upload skipped: content is %d bytes; maximum is %d bytes", effects.summaryBytes, maxCommandFileBytes))
}

func (r Runner) runStreaming(ctx context.Context, processor *commandOutputProcessor, dir string, env map[string]string, name string, args ...string) error {
	if err := validateEnvironmentNames(env); err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = processEnv(env)
	return r.runStreamingCommand(ctx, processor, cmd)
}

func resolveExecutableInPath(name, path string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func (r Runner) runStreamingCommand(ctx context.Context, processor *commandOutputProcessor, cmd *exec.Cmd) error {
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
		var commandErr error
		streamErr := streamLines(reader, func(line string) {
			commandErr = errors.Join(commandErr, processor.process(target, line))
		}, processor.suppress)
		if streamErr != nil {
			streamErr = fmt.Errorf("%s stream: %w", label, streamErr)
		}
		if commandErr != nil {
			commandErr = fmt.Errorf("%s stream: %w", label, commandErr)
		}
		streamErrs[index] = errors.Join(streamErr, commandErr)
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
		waitErr = fmt.Errorf("process %s: %w", cmd.Path, waitErr)
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
				if len(fragment) > 1 && fragment[len(fragment)-2] == '\r' {
					contentBytes--
				}
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
				process(trimStreamLineEnding(string(line)))
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
				process(trimStreamLineEnding(string(line)))
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

func trimStreamLineEnding(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

func mapEnv(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for _, name := range sortedKeys(values) {
		env = append(env, name+"="+values[name])
	}
	return env
}

func processEnv(overrides map[string]string) []string {
	// This allowlist is also the mise trust boundary: ambient MISE_* values must
	// never redirect compatibility runtime downloads or verification.
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
	maps.Copy(values, overrides)
	return mapEnv(values)
}

func validateEnvironmentNames(env map[string]string) error {
	for name := range env {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", name)
		}
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
