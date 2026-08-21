// Package cli implements the buildkite-gha command-line interface.
package cli

import (
	"fmt"
	"io"
	"strings"

	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// Run executes the command and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	return run(args, stdout, stderr, version, transport.CommandRunner{Stderr: stderr})
}

func run(args []string, stdout, stderr io.Writer, clientVersion string, agentRunner transport.Runner) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == gharuntime.ContainerProcessHelperCommand {
		return gharuntime.RunContainerProcessHelper(args[1:])
	}
	version := commandVersion(clientVersion)

	switch args[0] {
	case "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	case "help":
		return help(args[1:], stdout, stderr)
	case "-v", "--version", "version":
		if len(args) != 1 {
			return usageError(stderr, "%s does not accept arguments", args[0])
		}
		_, _ = fmt.Fprintf(stdout, "buildkite-gha %s\n", clientVersion)
		return 0
	case "plugin":
		return plugin(args[1:], stdout, stderr, version, clientVersion, agentRunner)
	default:
		if _, ok := commandUsage[args[0]]; ok {
			if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
				writeCommandHelp(stdout, args[0])
				return 0
			}
			switch args[0] {
			case "validate":
				return validate(args[1:], stdout, stderr, clientVersion, transport.Agent{Runner: agentRunner})
			case "validate-batch":
				return validateBatch(args[1:], stderr, clientVersion)
			case "compile":
				return compile(args[1:], stdout, stderr, clientVersion, transport.Agent{Runner: agentRunner})
			case "upload":
				return upload(args[1:], stdout, stderr, clientVersion, transport.Agent{Runner: agentRunner})
			case "run-job":
				return runJob(args[1:], stdout, stderr, version, clientVersion, transport.Agent{Runner: agentRunner})
			default:
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: %s: not implemented\n", args[0])
				return 1
			}
		}

		return usageError(stderr, "unknown command %q", args[0])
	}
}

func commandVersion(clientVersion string) string {
	if clientVersion == "dev" || strings.HasPrefix(clientVersion, "dev+") {
		return "dev"
	}
	return clientVersion
}
