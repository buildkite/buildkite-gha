// Package cli is the command adapter for the exported gha API.
package cli

import (
	"io"

	gha "github.com/buildkite/buildkite-gha"
)

// Run executes the command and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	return gha.RunCLI(args, stdout, stderr, version)
}
