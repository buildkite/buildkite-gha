// Package containerpolicy owns the job-container Docker syntax that workflows
// may control.
package containerpolicy

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const MaxJobVolumes = 32

var (
	volumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	sizePattern       = regexp.MustCompile(`^[1-9][0-9]*[bBkKmMgG]?$`)
	cpusPattern       = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)
	cpusetPattern     = regexp.MustCompile(`^[0-9]+(?:-[0-9]+)?(?:,[0-9]+(?:-[0-9]+)?)*$`)
	positivePattern   = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// JobOptions splits options using the GitHub runner's argument rules and
// accepts only bounded resource controls. It returns the exact Docker argv.
func JobOptions(value string) ([]string, error) {
	if len(value) > 4096 || hasASCIIControl(value) || strings.Contains(value, "\"") {
		return nil, fmt.Errorf("options exceed 4096 bytes or contain a control or quote character")
	}
	args := ArgumentList(value)
	if len(args) > 16 {
		return nil, fmt.Errorf("options contain more than 16 arguments")
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		option, optionValue, inline := strings.Cut(args[i], "=")
		if !inline {
			if i+1 == len(args) {
				return nil, fmt.Errorf("option %q requires a value", option)
			}
			i++
			optionValue = args[i]
		}
		canonical := option
		if canonical == "-m" {
			canonical = "--memory"
		}
		if seen[canonical] {
			return nil, fmt.Errorf("option %q is repeated", option)
		}
		seen[canonical] = true
		valid := false
		switch canonical {
		case "--cpus":
			cpus, err := strconv.ParseFloat(optionValue, 64)
			valid = cpusPattern.MatchString(optionValue) && err == nil && cpus > 0
		case "--cpuset-cpus":
			valid = validCPUSet(optionValue)
		case "--memory", "--memory-reservation", "--memory-swap", "--shm-size":
			valid = sizePattern.MatchString(optionValue)
		case "--pids-limit":
			valid = positivePattern.MatchString(optionValue)
		default:
			return nil, fmt.Errorf("option %q is unsupported", option)
		}
		if !valid {
			return nil, fmt.Errorf("option %q has invalid value %q", option, optionValue)
		}
	}
	return args, nil
}

func validCPUSet(value string) bool {
	if !cpusetPattern.MatchString(value) {
		return false
	}
	for item := range strings.SplitSeq(value, ",") {
		bounds := strings.Split(item, "-")
		if len(bounds) == 1 {
			continue
		}
		first, firstErr := strconv.Atoi(bounds[0])
		last, lastErr := strconv.Atoi(bounds[1])
		if firstErr != nil || lastErr != nil || first > last {
			return false
		}
	}
	return true
}

// ValidateJobVolume accepts a named volume mounted at an absolute container
// path. Host bind mounts and runner-owned workspace/runtime targets are denied.
func ValidateJobVolume(value string) error {
	if value == "" || len(value) > 4096 || hasASCIIControl(value) {
		return fmt.Errorf("volume is empty, too long, or contains a control character")
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || !volumeNamePattern.MatchString(parts[0]) {
		return fmt.Errorf("volume must be NAME:/absolute/container/path[:ro|rw]")
	}
	target := parts[1]
	if !strings.HasPrefix(target, "/") || target != path.Clean(target) || target == "/" {
		return fmt.Errorf("volume target %q must be a clean absolute path", target)
	}
	for _, reserved := range []string{"/__w", "/__buildkite-gha"} {
		if target == reserved || strings.HasPrefix(target, reserved+"/") {
			return fmt.Errorf("volume target %q overlaps a runner-owned path", target)
		}
	}
	if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
		return fmt.Errorf("volume mode %q is unsupported", parts[2])
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

// JobVolumeName returns the source name from a validated job volume.
func JobVolumeName(value string) string {
	name, _, _ := strings.Cut(value, ":")
	return name
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
