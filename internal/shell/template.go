// Package shell owns shell-template parsing and compatibility classification.
package shell

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ValidateCompatibility rejects shell commands that the runtime cannot run.
// It reports only the normalized executable because template arguments may
// contain event-derived values. Malformed templates remain ParseTemplate's
// responsibility.
func ValidateCompatibility(shell string) error {
	args, err := splitTemplate(shell)
	if err != nil || len(args) == 0 || args[0] == "" {
		return nil
	}
	command := normalizeCommand(args[0])
	return compatibilityError(command, command)
}

func compatibilityError(shell, command string) error {
	command = normalizeCommand(command)
	switch command {
	case "pwsh", "pwsh.exe", "cmd", "cmd.exe", "powershell", "powershell.exe", "msys2", "msys2.cmd", "msys2.exe":
		return &UnsupportedError{Shell: shell, Command: command}
	default:
		return nil
	}
}

// UnsupportedError reports the normalized executable for a shell rejected by
// the runtime without retaining possibly event-derived template arguments.
type UnsupportedError struct {
	Shell   string
	Command string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("shell %q is unsupported. PowerShell and Windows shells cannot run in buildkite-gha. Use bash, sh, python, or a valid custom shell template whose command is available on PATH, or file a compatibility issue at https://github.com/buildkite/buildkite-gha", e.Shell)
}

// UnsupportedCommand returns the normalized executable rejected by shell
// compatibility validation.
func UnsupportedCommand(err error) (string, bool) {
	var unsupported *UnsupportedError
	if errors.As(err, &unsupported) {
		return unsupported.Command, true
	}
	return "", false
}

func normalizeCommand(command string) string {
	return strings.ToLower(path.Base(strings.ReplaceAll(command, `\`, "/")))
}

// ParseTemplate splits and validates a custom shell template without invoking
// a command shell or expanding its arguments.
func ParseTemplate(shell string) ([]string, error) {
	args, err := splitTemplate(shell)
	if err != nil {
		return nil, fmt.Errorf("parse shell template %q: %w", shell, err)
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("shell template %q must contain a command", shell)
	}
	if err := compatibilityError(shell, args[0]); err != nil {
		return nil, err
	}
	hasPlaceholder := false
	for _, arg := range args[1:] {
		hasPlaceholder = hasPlaceholder || strings.Contains(arg, "{0}")
	}
	if !hasPlaceholder {
		return nil, fmt.Errorf("shell template %q must contain {0} in its arguments", shell)
	}
	return args, nil
}

func splitTemplate(shell string) ([]string, error) {
	var args []string
	var arg strings.Builder
	var quote rune
	escaped := false
	inArg := false
	flush := func() {
		if inArg {
			args = append(args, arg.String())
			arg.Reset()
			inArg = false
		}
	}
	for _, char := range shell {
		if escaped {
			arg.WriteRune(char)
			escaped = false
			inArg = true
			continue
		}
		if quote != 0 {
			switch {
			case char == quote:
				quote = 0
			case char == '\\' && quote == '"':
				escaped = true
			default:
				arg.WriteRune(char)
			}
			inArg = true
			continue
		}
		switch char {
		case '\\':
			escaped = true
			inArg = true
		case '\'', '"':
			quote = char
			inArg = true
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			arg.WriteRune(char)
			inArg = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unclosed quote or escape")
	}
	flush()
	return args, nil
}
