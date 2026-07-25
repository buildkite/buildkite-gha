package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const ContainerProcessHelperCommand = "__container-process"
const containerCancellationMarkerSuffix = ".cancel"
const containerPIDPublicationWait = 500 * time.Millisecond

// RunContainerProcessHelper implements the private process helper entry point.
// It returns an exit status suitable for os.Exit.
func RunContainerProcessHelper(args []string) int {
	if len(args) < 2 {
		_, _ = fmt.Fprintln(os.Stderr, "buildkite-gha: internal container process helper: invalid arguments")
		return 2
	}
	switch args[0] {
	case "run":
		if len(args) < 3 {
			return helperUsage()
		}
		marker := args[1] + containerCancellationMarkerSuffix
		if _, err := os.Stat(marker); err == nil {
			return 130
		} else if !os.IsNotExist(err) {
			return 1
		}
		cmd := exec.Command(args[2], args[3:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = os.Environ()
		configureProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "buildkite-gha: internal container process helper: start: %v\n", err)
			return 1
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
			terminateProcessGroupNow(cmd.Process.Pid, 0, 0)
			_ = cmd.Wait()
			_, _ = fmt.Fprintf(os.Stderr, "buildkite-gha: internal container process helper: write PID file: %v\n", err)
			return 1
		}
		if _, err := os.Stat(marker); err == nil {
			terminateProcessGroupNow(cmd.Process.Pid, 0, 0)
		} else if !os.IsNotExist(err) {
			terminateProcessGroupNow(cmd.Process.Pid, 0, 0)
		}
		err := cmd.Wait()
		if err == nil {
			return 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok {
				if status.Signaled() {
					return 128 + int(status.Signal())
				}
				return status.ExitStatus()
			}
		}
		return 1
	case "terminate":
		if len(args) != 4 {
			return helperUsage()
		}
		interrupt, e1 := time.ParseDuration(args[2])
		terminate, e2 := time.ParseDuration(args[3])
		if e1 != nil || e2 != nil || interrupt < 0 || terminate < 0 {
			return helperUsage()
		}
		if err := os.WriteFile(args[1]+containerCancellationMarkerSuffix, nil, 0o600); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "buildkite-gha: internal container process helper: write cancellation marker: %v\n", err)
			return 1
		}
		deadline := time.Now().Add(containerPIDPublicationWait)
		var pid int
		for {
			data, err := os.ReadFile(args[1])
			if err == nil {
				pid, err = strconv.Atoi(stringTrimSpace(data))
				if err == nil && pid > 0 {
					break
				}
			}
			if time.Now().After(deadline) {
				return 0
			}
			time.Sleep(10 * time.Millisecond)
		}
		terminateProcessGroupNow(pid, interrupt, terminate)
		return 0
	default:
		return helperUsage()
	}
}

func helperUsage() int {
	_, _ = fmt.Fprintln(os.Stderr, "buildkite-gha: internal container process helper: invalid arguments")
	return 2
}

func stringTrimSpace(b []byte) string {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return string(b[start:end])
}
