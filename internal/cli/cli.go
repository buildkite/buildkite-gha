// Package cli implements the buildkite-gha command-line interface.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/compiler"
)

const usage = `Usage:
  buildkite-gha <command> [arguments]
  buildkite-gha --version

Commands:
  validate  Validate the supported static workflow subset
  compile   Compile a workflow to deterministic JSON IR
  upload    Compile and upload a Buildkite pipeline
  run-job   Run a compiled job plan

Run "buildkite-gha help <command>" for command help.
`

var commandUsage = map[string]string{
	"validate": "Usage: buildkite-gha validate [--event-path <path>] <workflow>\n",
	"compile":  "Usage: buildkite-gha compile --event-path <path> <workflow>\n",
	"upload":   "Usage: buildkite-gha upload <workflow>\n",
	"run-job":  "Usage: buildkite-gha run-job --plan <path>\n",
}

// Run executes the command and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "help":
		return help(args[1:], stdout, stderr)
	case "-v", "--version", "version":
		if len(args) != 1 {
			return usageError(stderr, "%s does not accept arguments", args[0])
		}
		fmt.Fprintf(stdout, "buildkite-gha %s\n", version)
		return 0
	default:
		if commandHelp, ok := commandUsage[args[0]]; ok {
			if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
				fmt.Fprint(stdout, commandHelp)
				if args[0] == "upload" || args[0] == "run-job" {
					fmt.Fprintf(stdout, "\nThe %s command is not implemented yet.\n", args[0])
				}
				return 0
			}
			switch args[0] {
			case "validate":
				return validate(args[1:], stdout, stderr)
			case "compile":
				return compile(args[1:], stdout, stderr)
			default:
				fmt.Fprintf(stderr, "buildkite-gha: %s: not implemented\n", args[0])
				return 1
			}
		}

		return usageError(stderr, "unknown command %q", args[0])
	}
}

func help(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) != 1 {
		return usageError(stderr, "help accepts at most one command")
	}

	commandHelp, ok := commandUsage[args[0]]
	if !ok {
		return usageError(stderr, "unknown command %q", args[0])
	}

	fmt.Fprint(stdout, commandHelp)
	if args[0] == "upload" || args[0] == "run-job" {
		fmt.Fprintf(stdout, "\nThe %s command is not implemented yet.\n", args[0])
	}
	return 0
}

func validate(args []string, stdout, stderr io.Writer) int {
	workflowPath, eventPath, err := workflowArgs(args)
	if err != nil {
		return usageError(stderr, "validate: %v", err)
	}
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
		return 1
	}

	var report compiler.Report
	if eventPath == "" {
		report, err = compiler.Validate(workflowPath, source)
	} else {
		event, readErr := os.ReadFile(eventPath)
		if readErr != nil {
			fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", readErr)
			return 1
		}
		report, err = compiler.ValidateEvent(workflowPath, source, event)
	}
	if err != nil {
		fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: valid for static compilation (%d logical jobs, %d instances); runtime execution is not supported\n", workflowPath, report.LogicalJobs, report.Instances)
	return 0
}

func compile(args []string, stdout, stderr io.Writer) int {
	workflowPath, eventPath, err := workflowArgs(args)
	if err != nil {
		return usageError(stderr, "compile: %v", err)
	}
	if eventPath == "" {
		return usageError(stderr, "compile: --event-path is required")
	}
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
	event, err := os.ReadFile(eventPath)
	if err != nil {
		fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
	result, err := compiler.Compile(workflowPath, source, event)
	if err != nil {
		fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(result); err != nil {
		fmt.Fprintf(stderr, "buildkite-gha: compile: write output: %v\n", err)
		return 1
	}
	return 0
}

func workflowArgs(args []string) (workflowPath, eventPath string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--event-path":
			i++
			if i == len(args) {
				return "", "", fmt.Errorf("--event-path requires a path")
			}
			eventPath = args[i]
		case "-h", "--help":
			return "", "", fmt.Errorf("help must be requested immediately after the command")
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown option %q", args[i])
			}
			if workflowPath != "" {
				return "", "", fmt.Errorf("expected one workflow path")
			}
			workflowPath = args[i]
		}
	}
	if workflowPath == "" {
		return "", "", fmt.Errorf("workflow path is required")
	}
	return workflowPath, eventPath, nil
}

func usageError(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "buildkite-gha: "+format+"\n\n", args...)
	fmt.Fprint(stderr, usage)
	return 2
}
