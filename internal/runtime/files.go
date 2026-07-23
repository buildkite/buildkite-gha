package runtime

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxCommandFileBytes  = 1024 * 1024
	maxCommandFilesBytes = 2 * 1024 * 1024
	maxCommandEntries    = 1024
)

type commandFiles struct {
	dir     string
	output  string
	env     string
	state   string
	summary string
	path    string
}

type fileCommandEffects struct {
	paths    []string
	pathBase string
	pathSet  bool
}

func newCommandFiles() (commandFiles, error) {
	dir, err := os.MkdirTemp("", "buildkite-gha-files-")
	if err != nil {
		return commandFiles{}, fmt.Errorf("create file-command directory: %w", err)
	}
	files := commandFiles{
		dir:     dir,
		output:  filepath.Join(dir, "output"),
		env:     filepath.Join(dir, "env"),
		state:   filepath.Join(dir, "state"),
		summary: filepath.Join(dir, "summary"),
		path:    filepath.Join(dir, "path"),
	}
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return commandFiles{}, errors.Join(fmt.Errorf("create file-command file: %w", err), os.RemoveAll(dir))
		}
	}
	return files, nil
}

func (files commandFiles) apply(result *Result, state map[string]string) (fileCommandEffects, error) {
	if err := files.checkSizeBudget(); err != nil {
		return fileCommandEffects{}, err
	}
	outputs, outputErr := parseCommandFile(files.output)
	env, envErr := parseCommandFile(files.env)
	states, stateErr := parseCommandFile(files.state)
	summary, summaryErr := readBoundedFile(files.summary, maxCommandFileBytes)
	paths, pathErr := parsePathFile(files.path)
	if outputErr != nil || envErr != nil || stateErr != nil || summaryErr != nil || pathErr != nil {
		return fileCommandEffects{}, errors.Join(outputErr, envErr, stateErr, summaryErr, pathErr)
	}
	for name := range env {
		if strings.EqualFold(name, "NODE_OPTIONS") {
			return fileCommandEffects{}, errors.New("GITHUB_ENV may not set NODE_OPTIONS")
		}
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "RUNNER_") {
			return fileCommandEffects{}, fmt.Errorf("GITHUB_ENV may not set reserved variable %s", name)
		}
	}
	effects := fileCommandEffects{paths: paths}
	if pathBase, ok := env["PATH"]; ok {
		effects.pathBase = pathBase
		effects.pathSet = true
		result.pathBase = pathBase
		result.pathBaseSet = true
		result.Paths = result.Paths[:0]
	}
	for name, value := range outputs {
		result.Outputs[name] = value
	}
	for name, value := range env {
		result.Env[name] = value
	}
	for name, value := range states {
		result.State[name] = value
		if state != nil {
			state[name] = value
		}
	}
	result.Summary += string(summary)
	result.Paths = append(result.Paths, paths...)
	return effects, nil
}

func (files commandFiles) checkSizeBudget() error {
	var total int64
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat file command %s: %w", filepath.Base(path), err)
		}
		if info.Size() > maxCommandFileBytes {
			return fmt.Errorf("file command %s exceeds the %d-byte limit", filepath.Base(path), maxCommandFileBytes)
		}
		total += info.Size()
	}
	if total > maxCommandFilesBytes {
		return fmt.Errorf("file commands exceed the %d-byte aggregate limit", maxCommandFilesBytes)
	}
	return nil
}

func parsePathFile(path string) ([]string, error) {
	contents, err := readBoundedFile(path, maxCommandFileBytes)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		paths = append(paths, line)
		if len(paths) > maxCommandEntries {
			return nil, fmt.Errorf("file command path exceeds the %d-entry limit", maxCommandEntries)
		}
	}
	return paths, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("file command %s exceeds the %d-byte limit", filepath.Base(path), limit)
	}
	return contents, nil
}

func parseCommandFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLineBytes)
	entries := 0
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		equals := strings.Index(line, "=")
		if separator := strings.Index(line, "<<"); separator >= 0 && (equals < 0 || separator < equals) {
			name, delimiter := line[:separator], line[separator+2:]
			if name == "" || delimiter == "" {
				return nil, fmt.Errorf("invalid multiline file command %q", line)
			}
			var lines []string
			found := false
			for scanner.Scan() {
				value := strings.TrimSuffix(scanner.Text(), "\r")
				if value == delimiter {
					found = true
					break
				}
				lines = append(lines, value)
			}
			if !found {
				return nil, fmt.Errorf("missing delimiter %q for %q", delimiter, name)
			}
			values[name] = strings.Join(lines, "\n")
			entries++
			if entries > maxCommandEntries {
				return nil, fmt.Errorf("file command %s exceeds the %d-entry limit", filepath.Base(path), maxCommandEntries)
			}
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid file command %q", line)
		}
		values[name] = value
		entries++
		if entries > maxCommandEntries {
			return nil, fmt.Errorf("file command %s exceeds the %d-entry limit", filepath.Base(path), maxCommandEntries)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse file command %s: %w", filepath.Base(path), err)
	}
	return values, nil
}
