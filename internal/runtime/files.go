package runtime

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type commandFiles struct {
	dir     string
	output  string
	env     string
	state   string
	summary string
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
	}
	for _, path := range []string{files.output, files.env, files.state, files.summary} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			os.RemoveAll(dir)
			return commandFiles{}, fmt.Errorf("create file-command file: %w", err)
		}
	}
	return files, nil
}

func (files commandFiles) apply(result *Result, state map[string]string) error {
	outputs, outputErr := parseCommandFile(files.output)
	env, envErr := parseCommandFile(files.env)
	states, stateErr := parseCommandFile(files.state)
	summary, summaryErr := os.ReadFile(files.summary)
	if outputErr != nil || envErr != nil || stateErr != nil || summaryErr != nil {
		return errors.Join(outputErr, envErr, stateErr, summaryErr)
	}
	for name := range env {
		if strings.EqualFold(name, "NODE_OPTIONS") {
			return errors.New("GITHUB_ENV may not set NODE_OPTIONS")
		}
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "RUNNER_") {
			return fmt.Errorf("GITHUB_ENV may not set reserved variable %s", name)
		}
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
	return nil
}

func parseCommandFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
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
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid file command %q", line)
		}
		values[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
