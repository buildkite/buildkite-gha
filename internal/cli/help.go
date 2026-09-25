package cli

import (
	"fmt"
	"io"
)

const usage = `Usage:
  buildkite-gha <command> [arguments]
  buildkite-gha --version

Commands:
  validate  Validate the supported static workflow subset
  validate-batch  Validate a workflow manifest for corpus analysis
  compile   Compile a workflow to deterministic Buildkite pipeline YAML
  upload    Compile and upload a Buildkite pipeline
  migrate-secrets  Prepare or run an OIDC-authenticated secrets migration
  run-job   Run a compiled job plan

Run "buildkite-gha help <command>" for command help.
`

var commandUsage = map[string]string{
	"validate":        "Usage: buildkite-gha validate [--profile hosted] [--event <name> | --event-path <path> | --all-events] [--action-cache-dir <path>] [--format text|json] <workflow>\n",
	"validate-batch":  "Usage: buildkite-gha validate-batch --manifest <path> --output-dir <path> --corpus-id <id> --action-resolution-snapshot <path> [--refresh-action-resolution-snapshot] [--action-cache-dir <path> --action-cache-max-bytes <bytes>] [--github-token-env <name>] [--jobs <count>]\n",
	"compile":         "Usage: buildkite-gha compile --event-path <path> [--format pipeline|ir-json] <workflow>\n",
	"upload":          "Usage: buildkite-gha upload [--event-path <path>] [--runner-queue <runs-on>=<queue>]... [--runner-image <runs-on>=<immutable-image>]... [--runtime-distribution <platform>=<absolute-path>]... [--private-reusable-workflows] [--experimental-runner-user=<boolean>] [--runtime-queue hosted] [--] <workflow-path> [<workflow-path>...]\n       buildkite-gha upload --stage-digest <digest> --stage-producer <job>\n",
	"migrate-secrets": "Usage:\n  buildkite-gha migrate-secrets prepare [--organization <slug>] [--cluster <id>] [--pipeline <slug>] [--policy-file <path>] [--secret <name>]... [--match <glob>]... [--output <path>]\n  buildkite-gha migrate-secrets run --workflow <path>\n",
	"run-job":         "Usage: buildkite-gha run-job (--plan <path> | --plan-digest <digest> --plan-producer <step>) [--artifact-producer <job>] [--result <path>] [--hosted-tool-cache]\n",
}

func writeCommandHelp(stdout io.Writer, command string) {
	_, _ = fmt.Fprint(stdout, commandUsage[command])
	switch command {
	case "validate":
		_, _ = fmt.Fprint(stdout, "\nWithout --profile, validate checks event-independent syntax, the static graph, and every declared trigger; it does not evaluate hosted admission. The hosted profile resolves actions and applies production upload policy without executing jobs or proving arbitrary action runtime compatibility. Use --event-path for an exact snapshot, --event to generate one minimal compatibility snapshot, or --all-events to evaluate every declared supported event separately. Generated snapshots are test inputs, not substitutes for real payloads.\n")
	case "validate-batch":
		_, _ = fmt.Fprint(stdout, "\nThe manifest is newline-delimited JSON. Each record requires id, repository, path, hash, and source fields. Results are atomic processing-report/v3 JSON files keyed by the corpus ID, record identity, workflow and repository-local dependency content, validator executable, and action-resolution snapshot generation. Existing valid results resume the batch; records with unresolved local dependencies are reprocessed. A snapshot pins each mutable public action ref on first use; refresh starts a new generation. --github-token-env reads a GitHub token from the named environment variable without placing it in arguments or reports.\n")
	case "compile":
		_, _ = fmt.Fprint(stdout, "\nPipeline output references content-addressed plans; compile does not materialize or upload those artifacts. Jobs that declare a GitHub environment resolve its protection rules and environment secret names through the job-scoped Agent API, so compile must run inside a Buildkite job for such workflows.\n")
	case "upload":
		_, _ = fmt.Fprint(stdout, "\nEvery workflow operand must be an explicit .yml or .yaml path; use -- before paths that begin with a dash. Multiple operands must be tracked files inside the checked-out repository. Inputs are uploaded as one aggregate pipeline: successful workflows become groups, while failed or skipped workflows become top-level replacement steps. Reusable-only workflow_call files are imported through callers but do not become groups. Scheduled groups select the preserved GitHub event or, when it is absent, Buildkite's schedule source. Buildkite schedules retain cron ownership, so every scheduled workflow group is eligible on any Buildkite scheduled build. The importer asks the job-scoped Agent API to resolve runs-on selectors without an explicit mapping. The API can select native Linux hosts through agent tags or immutable images. Each repeatable --runner-queue argument maps one supported runs-on label to a Buildkite queue and bypasses automatic resolution. Runner labels are case-insensitive. Local Linux presets use the matching immutable hosted-toolchains image; --runner-image overrides it for a configured profile and remains authoritative. Duplicate or unsupported mappings fail. Without Agent API resolution, unmapped supported Linux labels retain default agent targeting with the preset image, macos-latest falls back to the hosted macos-medium queue, and macos-14 and macos-15 accept explicit fallback queues. Experimental windows-latest and windows-2022 require explicit queues or enabled Agent API resolution; they have no local preset and reject images and cache volumes. Each repeatable --runtime-distribution argument binds linux/amd64, darwin/arm64, or windows/amd64 to a verified executable. Upload importers run on Linux x86-64 or macOS arm64; the matching runtime defaults to the importer executable, and workflows targeting other platforms require their distributions. --private-reusable-workflows enables importer-only Git fallback with the job's existing Git credentials; it does not enable private actions. Generated Linux jobs provision and use a non-root runner identity by default; --experimental-runner-user=false temporarily disables this behavior. The option does not affect macOS or Windows jobs. The deprecated --runtime-queue hosted option is accepted for plugin compatibility but does not select a queue. Event precedence is an explicit event file, Buildkite's reserved webhook metadata, then reduced-fidelity Buildkite environment compatibility data; every source remains unsigned. Verified checkout jobs automatically use Buildkite repository-provider Git credentials when the job enables them; the deprecated --private-checkout option is accepted as a no-op. For jobs that declare a GitHub environment, the importer resolves environment protection rules and environment secret names automatically through the job-scoped Agent API. Workflows that declare environments fail when that resolution is unavailable. A job whose matrix.include comes from a needs.<job>.outputs value is compiled in stages: upload compiles the jobs whose rows are known, records the rest in a stage record artifact, and emits a step that runs upload --stage-digest <digest> --stage-producer <job> after the producer. That form takes no other options: it reads the record, verifies the producer's published result, expands the matrix, recompiles the workflow from the checkout through the same compile path, checks that every job the earlier stages created compiled identically, and uploads only the new jobs with deterministic step keys, emitting another stage step when a new job feeds a further matrix. A rerun after a successful upload is rejected by Buildkite for its duplicate step keys; the stage then confirms that the existing steps carry the same plans and exits 0. Any other rejection, a retried producer, or a changed workflow fails the step; retry the whole build.\n")
	case "migrate-secrets":
		_, _ = fmt.Fprint(stdout, "\nPrepare discovers names through gh and destinations through bk, then writes reviewable workflow YAML. Run creates a short-lived one-use Buildkite grant and dispatches the committed default-branch workflow. Neither command commits, pushes, merges, or deletes repository files.\n")
	}
}

func help(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) != 1 {
		return usageError(stderr, "help accepts at most one command")
	}

	if _, ok := commandUsage[args[0]]; !ok {
		return usageError(stderr, "unknown command %q", args[0])
	}

	writeCommandHelp(stdout, args[0])
	return 0
}

func usageError(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, "buildkite-gha: "+format+"\n\n", args...)
	_, _ = fmt.Fprint(stderr, usage)
	return 2
}
