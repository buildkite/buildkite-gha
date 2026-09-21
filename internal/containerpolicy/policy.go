// Package containerpolicy owns the job-container Docker syntax that workflows
// may control.
package containerpolicy

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

const MaxJobVolumes = 128
const MaxJobOptionsLength = 65536

var volumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// JobOptions splits options using the GitHub runner's argument rules and
// rejects the network and entrypoint overrides that GitHub does not support.
// It returns the exact Docker argv without invoking a shell.
func JobOptions(value string) ([]string, error) {
	if len(value) > MaxJobOptionsLength || strings.ContainsAny(value, "\x00\r\n") {
		return nil, fmt.Errorf("options exceed %d bytes or contain a control character", MaxJobOptionsLength)
	}
	args := ArgumentList(value)
	for _, arg := range args {
		for _, unsupported := range []string{"--network", "--net", "--entrypoint"} {
			if arg == unsupported || strings.HasPrefix(arg, unsupported+"=") {
				return nil, fmt.Errorf("option %q is unsupported", arg)
			}
		}
	}
	return args, nil
}

// ValidateJobVolume accepts GitHub's named, anonymous, and absolute host-bind
// volume syntax with an absolute container destination.
func ValidateJobVolume(value string) error {
	if value == "" || len(value) > 4096 || hasASCIIControl(value) {
		return fmt.Errorf("volume is empty, too long, or contains a control character")
	}
	parts := strings.Split(value, ":")
	var source, target, mode string
	switch len(parts) {
	case 1:
		target = parts[0]
	case 2:
		source, target = parts[0], parts[1]
		if source == "" {
			return fmt.Errorf("volume source is empty")
		}
	case 3:
		source, target, mode = parts[0], parts[1], parts[2]
		if source == "" {
			return fmt.Errorf("volume source is empty")
		}
	default:
		return fmt.Errorf("volume must be DESTINATION or SOURCE:DESTINATION[:ro|rw]")
	}
	if source != "" && !path.IsAbs(source) && !volumeNamePattern.MatchString(source) {
		return fmt.Errorf("volume source %q must be a name or absolute host path", source)
	}
	if !path.IsAbs(target) {
		return fmt.Errorf("volume target %q must be an absolute path", target)
	}
	if mode != "" && mode != "ro" && mode != "rw" {
		return fmt.Errorf("volume mode %q is unsupported", mode)
	}
	return nil
}

// ValidateJobVolumes applies the bounded list contract used by plans and the
// runtime boundary.
func ValidateJobVolumes(values []string) error {
	if len(values) > MaxJobVolumes {
		return fmt.Errorf("more than %d volumes", MaxJobVolumes)
	}
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return fmt.Errorf("volume %q is repeated", value)
		}
		seen[value] = true
		if err := ValidateJobVolume(value); err != nil {
			return fmt.Errorf("invalid volume %q: %w", value, err)
		}
	}
	return nil
}

func hasASCIIControl(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ArgumentList matches the argument splitting used by the pinned
// actions/runner ProcessStartInfo.Arguments path. Single quotes are ordinary
// characters; double quotes group arguments; backslashes only escape quotes.
func ArgumentList(value string) []string {
	var args []string
	for i := 0; i < len(value); {
		for i < len(value) && (value[i] == ' ' || value[i] == '\t') {
			i++
		}
		if i == len(value) {
			break
		}
		var arg strings.Builder
		quoted := false
		for i < len(value) {
			if !quoted && (value[i] == ' ' || value[i] == '\t') {
				break
			}
			backslashes := 0
			for i < len(value) && value[i] == '\\' {
				backslashes++
				i++
			}
			copyCharacter := true
			if i < len(value) && value[i] == '"' {
				if backslashes%2 == 0 {
					if quoted && i+1 < len(value) && value[i+1] == '"' {
						i++
					} else {
						copyCharacter = false
						quoted = !quoted
					}
				}
				backslashes /= 2
			}
			arg.WriteString(strings.Repeat("\\", backslashes))
			if i == len(value) || !quoted && (value[i] == ' ' || value[i] == '\t') {
				break
			}
			if copyCharacter {
				arg.WriteByte(value[i])
			}
			i++
		}
		args = append(args, arg.String())
	}
	return args
}
