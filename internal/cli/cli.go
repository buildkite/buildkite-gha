// Package cli implements the buildkite-gha command-line interface.
package cli

import (
	"fmt"
	"io"
)

const usage = `Usage:
  buildkite-gha <command> [arguments]
  buildkite-gha --version

Commands:
  validate  Validate a GitHub Actions workflow
  compile   Compile a workflow to a Buildkite pipeline
  upload    Compile and upload a Buildkite pipeline
  run-job   Run a compiled job plan

Run "buildkite-gha help <command>" for command help.
`

var commandUsage = map[string]string{
	"validate": "Usage: buildkite-gha validate <workflow>\n",
	"compile":  "Usage: buildkite-gha compile <workflow>\n",
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
				fmt.Fprintf(stdout, "\nThe %s command is not implemented yet.\n", args[0])
				return 0
			}

			fmt.Fprintf(stderr, "buildkite-gha: %s: not implemented\n", args[0])
			return 1
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
	fmt.Fprintf(stdout, "\nThe %s command is not implemented yet.\n", args[0])
	return 0
}

func usageError(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "buildkite-gha: "+format+"\n\n", args...)
	fmt.Fprint(stderr, usage)
	return 2
}
