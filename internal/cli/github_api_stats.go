package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
)

const sourceAPIStatsEnvironment = "BUILDKITE_GHA_GITHUB_API_STATS"
const sourceAPIStatsPrefix = "buildkite-gha source REST API stats: "

// The dispatcher owns the summary so nested upload/plugin operations share one
// scope. It writes directly to the caller's stderr after command telemetry has
// finished capturing error output, including on usage and compilation failures.
func sourceAPIStatsContext(ctx context.Context, args []string, stderr io.Writer) (context.Context, func()) {
	noop := func() {}
	if os.Getenv(sourceAPIStatsEnvironment) != "true" || len(args) == 0 {
		return ctx, noop
	}
	switch args[0] {
	case "compile", "upload", "plugin", "validate", "validate-batch":
	default:
		return ctx, noop
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") && args[0] != "plugin" {
		return ctx, noop
	}
	ctx, stats := actionsource.WithGitHubAPIStats(ctx)
	return ctx, func() {
		summary := struct {
			Command string `json:"command"`
			actionsource.GitHubAPIStatsSummary
		}{Command: args[0], GitHubAPIStatsSummary: stats.Snapshot()}
		data, err := json.Marshal(summary)
		if err == nil {
			_, _ = fmt.Fprintf(stderr, "%s%s\n", sourceAPIStatsPrefix, data)
		}
	}
}
