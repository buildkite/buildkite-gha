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
	open    map[string]*os.File
}

type fileCommandEffects struct {
	paths        []string
	pathBase     string
	pathSet      bool
	summaryBytes int64
}

func newCommandFiles() (commandFiles, error) {
	return newCommandFilesUnder("")
}

func newCommandFilesUnder(parent string) (commandFiles, error) {
	dir, err := os.MkdirTemp(parent, "buildkite-gha-files-")
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
		open:    make(map[string]*os.File, 5),
	}
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return commandFiles{}, errors.Join(fmt.Errorf("create file-command file: %w", err), files.cleanup())
		}
		files.open[path] = file
	}
	return files, nil
}

func (files commandFiles) allowContainerWrites() error {
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		file, err := files.retained(path)
		if err != nil {
			return err
		}
		if err := file.Chmod(0o666); err != nil {
			return fmt.Errorf("make container file command writable: %w", err)
		}
	}
	// Container images may declare any USER. Permit traversal to the fixed,
	// pre-created files without permitting directory listing or new entries.
	if err := os.Chmod(files.dir, 0o711); err != nil {
		return fmt.Errorf("make container file-command directory traversable: %w", err)
	}
	return nil
}

func (files commandFiles) retained(path string) (*os.File, error) {
	file := files.open[path]
	if file == nil {
		return nil, fmt.Errorf("file command %s is not retained", filepath.Base(path))
	}
	return file, nil
}

func (files commandFiles) cleanup() error {
	var cleanupErr error
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		if file := files.open[path]; file != nil {
			cleanupErr = errors.Join(cleanupErr, file.Close())
		}
	}
	return errors.Join(cleanupErr, os.RemoveAll(files.dir))
}

func (files commandFiles) apply(result *Result, state map[string]string) (fileCommandEffects, error) {
	summaryBytes, err := files.checkSizeBudget()
	if err != nil {
		return fileCommandEffects{}, err
	}
	effects := fileCommandEffects{summaryBytes: summaryBytes}
	outputs, outputErr := files.parseCommandFile(files.output)
	env, envErr := files.parseCommandFile(files.env)
	states, stateErr := files.parseCommandFile(files.state)
	var summary []byte
	var summaryErr error
	if summaryBytes <= maxCommandFileBytes {
		summary, summaryErr = files.readBoundedFile(files.summary, maxCommandFileBytes)
	}
	paths, pathErr := files.parsePathFile(files.path)
	if outputErr != nil || envErr != nil || stateErr != nil || summaryErr != nil || pathErr != nil {
		return effects, errors.Join(outputErr, envErr, stateErr, summaryErr, pathErr)
	}
	for name := range env {
		if strings.EqualFold(name, "NODE_OPTIONS") {
			return effects, errors.New("GITHUB_ENV may not set NODE_OPTIONS")
		}
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "RUNNER_") {
			return effects, fmt.Errorf("GITHUB_ENV may not set reserved variable %s", name)
		}
	}
	effects.paths = paths
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
	appendJobSummary(&result.Summary, &result.summaryTruncated, string(summary), false)
	result.Paths = append(result.Paths, paths...)
	return effects, nil
}

func (files commandFiles) checkSizeBudget() (int64, error) {
	var total int64
	var summaryBytes int64
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		file, err := files.retained(path)
		if err != nil {
			return 0, err
		}
		info, err := file.Stat()
		if err != nil {
			return 0, fmt.Errorf("stat file command %s: %w", filepath.Base(path), err)
		}
		if path == files.summary {
			summaryBytes = info.Size()
			if summaryBytes > maxCommandFileBytes {
				continue
			}
		}
		if info.Size() > maxCommandFileBytes {
			return 0, fmt.Errorf("file command %s exceeds the %d-byte limit", filepath.Base(path), maxCommandFileBytes)
		}
		total += info.Size()
	}
	if total > maxCommandFilesBytes {
		return 0, fmt.Errorf("file commands exceed the %d-byte aggregate limit", maxCommandFilesBytes)
	}
	return summaryBytes, nil
}

func (files commandFiles) parseCommandFile(path string) (map[string]string, error) {
	file, err := files.retained(path)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek file command %s: %w", filepath.Base(path), err)
	}
	return parseCommandReader(path, file)
}

func (files commandFiles) readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := files.retained(path)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek file command %s: %w", filepath.Base(path), err)
	}
	return readBoundedReader(path, file, limit)
}

func (files commandFiles) parsePathFile(path string) ([]string, error) {
	contents, err := files.readBoundedFile(path, maxCommandFileBytes)
	return parsePathContents(contents, err)
}

func parsePathContents(contents []byte, err error) ([]string, error) {
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

func readBoundedReader(path string, reader io.Reader, limit int64) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("file command %s exceeds the %d-byte limit", filepath.Base(path), limit)
	}
	return contents, nil
}

func parseCommandReader(path string, reader io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(reader)
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
