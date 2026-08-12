// Package cli implements the buildkite-gha command-line interface.
package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const usage = `Usage:
  buildkite-gha <command> [arguments]
  buildkite-gha --version

Commands:
  validate  Validate the supported static workflow subset
  compile   Compile a workflow to deterministic Buildkite pipeline YAML
  upload    Compile and upload a Buildkite pipeline
  run-job   Run a compiled job plan

Run "buildkite-gha help <command>" for command help.
`

var commandUsage = map[string]string{
	"validate": "Usage: buildkite-gha validate [--event-path <path>] [--profile hosted] [--format text|json] <workflow>\n",
	"compile":  "Usage: buildkite-gha compile --event-path <path> [--format pipeline|ir-json] <workflow>\n",
	"upload":   "Usage: buildkite-gha upload [--event-path <path>] [--runner-queue <runs-on>=<queue>]... [--runner-image <runs-on>=<immutable-image>]... [--runtime-distribution <platform>=<absolute-path>]... [--runtime-queue hosted] [--] <workflow-pattern | workflow-path> [<workflow-path>...]\n",
	"run-job":  "Usage: buildkite-gha run-job (--plan <path> | --plan-digest <digest> --plan-producer <step>) [--result <path>] [--hosted-tool-cache]\n",
}

func writeCommandHelp(stdout io.Writer, command string) {
	_, _ = fmt.Fprint(stdout, commandUsage[command])
	switch command {
	case "validate":
		_, _ = fmt.Fprint(stdout, "\nThe hosted profile resolves actions and applies production upload policy without executing jobs or proving arbitrary action runtime compatibility.\n")
	case "compile":
		_, _ = fmt.Fprint(stdout, "\nPipeline output references content-addressed plans; compile does not materialize or upload those artifacts.\n")
	case "upload":
		_, _ = fmt.Fprint(stdout, "\nThe * shorthand selects tracked .yml and .yaml files directly under .github/workflows. One workflow operand preserves literal, directory, and tracked glob expansion. Two or more operands are explicit tracked .yml/.yaml paths; use -- before paths that begin with a dash. Inputs are uploaded as one aggregate pipeline with one group per directly runnable workflow; reusable-only workflow_call files are imported through callers but do not become groups. Scheduled groups select only build.source == schedule: Buildkite schedules retain cron ownership, so every scheduled workflow group is eligible on any Buildkite scheduled build. Each repeatable --runner-queue argument maps one supported runs-on label to a Buildkite queue. Configured Linux profiles default to the matching immutable hosted-toolchains image; --runner-image overrides it. Duplicate or unsupported mappings fail, unmapped supported Linux labels retain their default targeting, and every macOS label requires an explicit queue. Each repeatable --runtime-distribution argument binds linux/amd64 or darwin/arm64 to a verified executable. Upload importers must run on linux/amd64; the Linux runtime defaults to the importer executable when omitted, and macOS has no default. The deprecated --runtime-queue hosted option is accepted for plugin compatibility but does not select a queue. Event precedence is an explicit event file, Buildkite's reserved webhook metadata, then reduced-fidelity Buildkite environment compatibility data; every source remains unsigned. Verified checkout jobs automatically use Buildkite repository-provider Git credentials when the job enables them; the deprecated --private-checkout option is accepted as a no-op.\n")
	}
}

const (
	resultPublicationTimeout                    = 10 * time.Second
	legacyRuntimeQueue                          = "hosted"
	pluginConfigurationEnvironment              = "BUILDKITE_PLUGIN_CONFIGURATION"
	pluginDevDarwinRuntimeEnvironment           = "BUILDKITE_GHA_PLUGIN_DEV_DARWIN_RUNTIME"
	legacyTargetQueueEnvironment                = "BUILDKITE_GHA_TARGET_QUEUE"
	legacyRuntimeImageEnvironment               = "BUILDKITE_GHA_RUNTIME_IMAGE"
	repositoryProviderGitCredentialsEnvironment = "BUILDKITE_USE_REPOSITORY_PROVIDER_GIT_CREDENTIALS"
	legacyGitHubAppGitCredentialsEnvironment    = "BUILDKITE_USE_GITHUB_APP_GIT_CREDENTIALS"
	hostedProfile                               = "hosted"
	legacyHostedTokenlessProfile                = "hosted-tokenless"
	runtimeMiseArchiveDigest                    = "bd0930c0b619f51ddb60e32e5cce18a5533567b2f1ba9fc4875b9f39a2bb3ed8"
	runtimeMiseBinaryDigest                     = "a238972a3162d710b85b28c324372e96ca4e4b486c81fe78695000d9fbc77c48"
	runtimeMiseDarwinARM64ArchiveDigest         = "5b883c868a0748dd0c595d30fd000ec5138dfabdeef2c30222866ebf34af1ae3"
	runtimeMiseDarwinARM64BinaryDigest          = "e777070540ffe22cf8b2b9f88aed88b461d0887d940c4f1c1a97359463cde6e1"
	runtimeMiseArchiveLimit                     = 64 << 20
	runtimeMiseBinaryLimit                      = 128 << 20
	runtimeDistributionLimit                    = 256 << 20
	pluginChecksumLimit                         = 4 << 20
	pluginArchiveLimit                          = 256 << 20
	maxWebhookMetadataBytes                     = 25 << 20
	defaultNobleRunnerImage                     = "buildkite.namespace-images.com/agent-base@sha256:62a45683afffaae9edfd669c16d2fee23b5a571679f31715e1063dada667ea24"
	defaultJammyRunnerImage                     = "buildkite.namespace-images.com/agent-base@sha256:a014d0bae6b06bb315d10b5ff8bb226d5fe7fa468bcf140b3c0d7e72a33aa1ac"
)

var runnerQueuePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var runnerImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)
var stableVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

var pluginReleaseBaseURL = "https://github.com/buildkite/buildkite-gha/releases/download"
var pluginHTTPClient = securePluginHTTPClient()

func repositoryProviderGitCredentialsEnabled(getenv func(string) string) bool {
	return getenv(repositoryProviderGitCredentialsEnvironment) == "true" || getenv(legacyGitHubAppGitCredentialsEnvironment) == "true"
}

// Run executes the command and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	return run(args, stdout, stderr, version, transport.CommandRunner{Stderr: stderr})
}

func run(args []string, stdout, stderr io.Writer, version string, agentRunner transport.Runner) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == gharuntime.ContainerProcessHelperCommand {
		return gharuntime.RunContainerProcessHelper(args[1:])
	}

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
		_, _ = fmt.Fprintf(stdout, "buildkite-gha %s\n", version)
		return 0
	case "plugin":
		return plugin(args[1:], stdout, stderr, version, agentRunner)
	default:
		if _, ok := commandUsage[args[0]]; ok {
			if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
				writeCommandHelp(stdout, args[0])
				return 0
			}
			switch args[0] {
			case "validate":
				return validate(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			case "compile":
				return compile(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			case "upload":
				return upload(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			case "run-job":
				return runJob(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			default:
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: %s: not implemented\n", args[0])
				return 1
			}
		}

		return usageError(stderr, "unknown command %q", args[0])
	}
}

func plugin(args []string, stdout, stderr io.Writer, version string, runner transport.Runner) int {
	if len(args) != 0 {
		return usageError(stderr, "plugin does not accept arguments")
	}
	if err := validateImporterPlatform(runtime.GOOS, runtime.GOARCH); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %v\n", err)
		return 1
	}
	configuration, err := parsePluginConfiguration(os.Getenv(pluginConfigurationEnvironment))
	if err != nil {
		return usageError(stderr, "plugin: %v", err)
	}
	if err := normalizePluginCommit(context.Background(), os.Getenv, os.Setenv, runner); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %v\n", err)
		return 1
	}
	return uploadParsed(parsedUploadArgs{
		workflowOperands:      configuration.Workflows,
		explicitWorkflowPaths: configuration.explicitWorkflowPaths,
		runnerTargets:         configuration.runnerTargets,
		pluginAcquisition:     &pluginRuntimeAcquisition{version: version},
	}, stdout, stderr, version, transport.Agent{Runner: runner})
}

func validateImporterPlatform(goos, goarch string) error {
	if goos != "linux" || goarch != "amd64" {
		return fmt.Errorf("importer requires linux/amd64, running on %s/%s", goos, goarch)
	}
	return nil
}

type pluginConfiguration struct {
	Workflows             []string
	explicitWorkflowPaths bool
	runnerTargets         map[string]compiler.RunnerTarget
}

func parsePluginConfiguration(source string) (pluginConfiguration, error) {
	if strings.TrimSpace(source) == "" {
		return pluginConfiguration{}, fmt.Errorf("%s is required", pluginConfigurationEnvironment)
	}
	decoder := json.NewDecoder(strings.NewReader(source))
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return pluginConfiguration{}, fmt.Errorf("decode %s: %w", pluginConfigurationEnvironment, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return pluginConfiguration{}, fmt.Errorf("decode %s: multiple JSON values", pluginConfigurationEnvironment)
		}
		return pluginConfiguration{}, fmt.Errorf("decode %s: %w", pluginConfigurationEnvironment, err)
	}
	encoded, ok := value.(map[string]any)
	if !ok {
		return pluginConfiguration{}, fmt.Errorf("%s must be a JSON object", pluginConfigurationEnvironment)
	}
	for key := range encoded {
		switch key {
		case "workflow", "workflows", "runners", "version", "source-ref", "minimum-release-age":
		default:
			return pluginConfiguration{}, fmt.Errorf("%s contains unknown field %q", pluginConfigurationEnvironment, key)
		}
	}
	legacyWorkflow, hasLegacyWorkflow := encoded["workflow"]
	workflowsValue, hasWorkflows := encoded["workflows"]
	if hasLegacyWorkflow && hasWorkflows {
		return pluginConfiguration{}, fmt.Errorf("%s workflow and workflows are mutually exclusive", pluginConfigurationEnvironment)
	}
	var workflows []string
	explicitWorkflowPaths := false
	if hasLegacyWorkflow {
		workflow, ok := legacyWorkflow.(string)
		if !ok || strings.TrimSpace(workflow) == "" {
			return pluginConfiguration{}, fmt.Errorf("%s workflow must be a non-empty string", pluginConfigurationEnvironment)
		}
		workflows = []string{workflow}
	} else {
		switch value := workflowsValue.(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				workflows = []string{value}
			}
		case []any:
			if len(value) != 0 {
				explicitWorkflowPaths = true
				workflows = make([]string, len(value))
				for index, entry := range value {
					workflow, ok := entry.(string)
					if !ok || strings.TrimSpace(workflow) == "" {
						return pluginConfiguration{}, fmt.Errorf("%s workflows entry %d must be a non-empty string", pluginConfigurationEnvironment, index)
					}
					workflows[index] = workflow
				}
			}
		}
		if len(workflows) == 0 {
			return pluginConfiguration{}, fmt.Errorf("%s workflows is required and must be a non-empty string or array of non-empty strings", pluginConfigurationEnvironment)
		}
	}
	targets := make(map[string]compiler.RunnerTarget)
	if runnersValue, configured := encoded["runners"]; configured {
		runners, ok := runnersValue.([]any)
		if !ok || len(runners) == 0 {
			return pluginConfiguration{}, fmt.Errorf("%s runners must be a non-empty array when configured", pluginConfigurationEnvironment)
		}
		for index, runnerValue := range runners {
			runner, ok := runnerValue.(map[string]any)
			if !ok {
				return pluginConfiguration{}, fmt.Errorf("runner %d must be a JSON object", index)
			}
			for key := range runner {
				switch key {
				case "runs-on", "queue", "image":
				default:
					return pluginConfiguration{}, fmt.Errorf("runner %d contains unknown field %q", index, key)
				}
			}
			runsOn, runsOnOK := runner["runs-on"].(string)
			queue, queueOK := runner["queue"].(string)
			if !runsOnOK {
				return pluginConfiguration{}, fmt.Errorf("runner %d runs-on must be a string", index)
			}
			if !queueOK {
				return pluginConfiguration{}, fmt.Errorf("runner %d queue must be a string", index)
			}
			image := ""
			if imageValue, configured := runner["image"]; configured {
				var imageOK bool
				image, imageOK = imageValue.(string)
				if !imageOK || image == "" {
					return pluginConfiguration{}, fmt.Errorf("runner %d image must be an immutable registry sha256 reference", index)
				}
			}
			label, target, err := configuredRunnerTarget(runsOn, queue, image)
			if err != nil {
				return pluginConfiguration{}, fmt.Errorf("runner %d: %w", index, err)
			}
			if _, duplicate := targets[label]; duplicate {
				return pluginConfiguration{}, fmt.Errorf("runner label %q may only be configured once", label)
			}
			targets[label] = target
		}
	}
	return pluginConfiguration{Workflows: workflows, explicitWorkflowPaths: explicitWorkflowPaths, runnerTargets: targets}, nil
}

func normalizePluginCommit(ctx context.Context, getenv func(string) string, setenv func(string, string) error, runner transport.Runner) error {
	if validBuildkiteCommit(getenv("BUILDKITE_COMMIT")) {
		return nil
	}
	output, err := runner.Run(ctx, "", "git", []string{"rev-parse", "HEAD"}, nil)
	if err != nil {
		return fmt.Errorf("resolve BUILDKITE_COMMIT from checked-out HEAD: %w", err)
	}
	commit := strings.TrimSpace(string(output))
	if !validBuildkiteCommit(commit) {
		return fmt.Errorf("resolve BUILDKITE_COMMIT from checked-out HEAD: git returned an invalid commit")
	}
	if err := setenv("BUILDKITE_COMMIT", commit); err != nil {
		return fmt.Errorf("set resolved BUILDKITE_COMMIT: %w", err)
	}
	return nil
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

func runJob(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runJobContext(ctx, args, stdout, stderr, version, agent)
}

func runJobContext(ctx context.Context, args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	options, err := runJobArgs(args)
	if err != nil {
		return usageError(stderr, "run-job: %v", err)
	}
	if options.planProducer != "" {
		planRoot, mkdirErr := os.MkdirTemp("", "buildkite-gha-plan-")
		if mkdirErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: create plan directory: %v\n", mkdirErr)
			return 1
		}
		defer func() { _ = os.RemoveAll(planRoot) }()
		if err := agent.DownloadArtifact(ctx, options.planPath, planRoot, options.planProducer); err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: download plan: %v\n", err)
			return 1
		}
		options.planPath = filepath.Join(planRoot, filepath.FromSlash(options.planPath))
	}
	source, err := os.ReadFile(options.planPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	planDigest := transport.Digest(source)
	expectedPlanDigest := options.planDigest
	if expectedPlanDigest == "" {
		expectedPlanDigest = os.Getenv("BUILDKITE_GHA_PLAN_DIGEST")
	}
	if expectedPlanDigest != "" {
		if planDigest != expectedPlanDigest {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: plan digest %q does not match expected digest %q\n", planDigest, expectedPlanDigest)
			return 1
		}
	}
	job, err := plan.Decode(source)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	if job.Compiler.Version != version {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: plan compiler version %q does not match runtime version %q\n", job.Compiler.Version, version)
		return 1
	}
	runtimeDigest, err := executableDigest()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: verify runtime executable: %v\n", err)
		return 1
	}
	if expected := job.RuntimeDistributionDigest(); runtimeDigest != expected {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: runtime distribution digest %q does not match plan digest %q\n", runtimeDigest, expected)
		return 1
	}
	if err := verifyBuildkiteTarget(job); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	if err := gharuntime.ValidateHost(job, runtime.GOOS, runtime.GOARCH); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	producer, publish, err := resultProducer(job, planDigest, expectedPlanDigest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	var artifactRoot string
	if publish {
		artifactRoot, err = os.MkdirTemp("", "buildkite-gha-results-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: create result artifact root: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(artifactRoot) }()
	}
	var actionMaterializer gharuntime.ActionMaterializer
	if hasGitHubActionLocks(job.Actions) {
		actionCache, err := os.MkdirTemp("", "buildkite-gha-actions-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: create action cache: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(actionCache) }()
		store, err := actionsource.NewStore(actionCache, nil)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: configure action cache: %v\n", err)
			return 1
		}
		actionMaterializer = store
	}
	var cacheCredentials gharuntime.CacheCredentialProvider
	cacheRequired, err := cacheServiceRequired(job.Actions)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	if cacheRequired || len(job.Actions) > 0 {
		cacheCredentials, err = gharuntime.NewAgentCacheCredentials(gharuntime.AgentCacheConfig{
			Endpoint:   os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
			JobID:      os.Getenv("BUILDKITE_JOB_ID"),
			JobToken:   os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
			ResultsURL: os.Getenv("BUILDKITE_GHA_CACHE_URL"),
		})
		if err != nil && cacheRequired {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: configure actions/cache service: %v\n", err)
			return 1
		}
		if err != nil {
			cacheCredentials = nil
		}
	}
	var workflowTokens gharuntime.WorkflowTokenProvider
	if job.HasCapability("provider-token-write") {
		githubTokens, tokenErr := gharuntime.NewAgentGitHubTokens(gharuntime.AgentGitHubTokenConfig{
			Endpoint: os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
			JobID:    os.Getenv("BUILDKITE_JOB_ID"),
			JobToken: os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
		})
		if tokenErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: configure GitHub token service: %v\n", tokenErr)
			return 1
		}
		workflowTokens = githubTokens
	}
	var repositoryCredentials *gharuntime.AgentRepositoryCredentials
	if repositoryProviderGitCredentialsEnabled(os.Getenv) {
		repositoryCredentials = &gharuntime.AgentRepositoryCredentials{
			Agent:    os.Getenv("BUILDKITE_GHA_AGENT"),
			Endpoint: os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
			JobID:    os.Getenv("BUILDKITE_JOB_ID"),
			JobToken: os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
			NoHTTP2:  os.Getenv("BUILDKITE_NO_HTTP2"),
		}
	}
	runnerToolCache := ""
	if options.hostedToolCache {
		runnerToolCache = buildkitepipeline.HostedToolCachePath
	}
	runner := gharuntime.Runner{
		Stdout:                stdout,
		Stderr:                stderr,
		MiseDataDir:           prepareMiseDataDir(os.Getenv("BUILDKITE_GHA_MISE_DATA_DIR"), stderr),
		ToolCache:             runnerToolCache,
		Docker:                os.Getenv("BUILDKITE_GHA_DOCKER"),
		Git:                   os.Getenv("BUILDKITE_GHA_GIT"),
		Secrets:               gharuntime.EnvironmentSecrets{},
		Redactor:              gharuntime.AgentRedactor{Executable: os.Getenv("BUILDKITE_GHA_AGENT")},
		Actions:               actionMaterializer,
		Artifacts:             agent,
		Cache:                 cacheCredentials,
		RepositoryCredentials: repositoryCredentials,
		WorkflowToken:         workflowTokens,
	}
	runner.RuntimeExecutable, err = os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: resolve runtime executable: %v\n", err)
		return 1
	}
	var privateRuntime string
	if job.NeedsMise() {
		runner.ResolveMise = func(ctx context.Context) (string, error) {
			var err error
			privateRuntime, err = os.MkdirTemp("", "buildkite-gha-runtime-")
			if err != nil {
				return "", fmt.Errorf("prepare action runtime: create private action runtime: %w", err)
			}
			mise, err := resolveRuntimeMise(ctx, os.Getenv("BUILDKITE_GHA_MISE"), runner.MiseDataDir, privateRuntime, stderr)
			if err != nil {
				return "", fmt.Errorf("prepare action runtime: %w", err)
			}
			return mise, nil
		}
		defer func() {
			if privateRuntime != "" {
				_ = os.RemoveAll(privateRuntime)
			}
		}()
	}
	var result gharuntime.JobResult
	var runErr error
	if len(job.NeedSources) != 0 {
		job.Needs, runErr = gharuntime.ResolveNeeds(ctx, agent, artifactRoot, producer.BuildID, job.NeedSources, job.NeedOutputs)
		if runErr != nil {
			runErr = fmt.Errorf("hydrate prerequisite results: %w", runErr)
		}
	}
	if runErr == nil {
		result, runErr = runner.RunJob(ctx, job, "")
	}
	if result.Conclusion == "" {
		result.Conclusion = terminalErrorConclusion(ctx)
	}
	if options.resultPath != "" && result.Conclusion != "" {
		if err := writeJobResult(options.resultPath, result); err != nil {
			runErr = errors.Join(runErr, err)
			result.Conclusion = terminalErrorConclusion(ctx)
		}
	}
	if publish {
		publication, err := publishTerminalResult(agent, artifactRoot, job, planDigest, producer, result)
		if publication.MetadataMirrorError != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: result metadata mirror: %v\n", publication.MetadataMirrorError)
		}
		if publication.SummaryAnnotationError != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: job summary annotation: %v\n", publication.SummaryAnnotationError)
		}
		if publication.WarningAnnotationError != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: workflow warning annotation: %v\n", publication.WarningAnnotationError)
		}
		if publication.ErrorAnnotationError != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: workflow error annotation: %v\n", publication.ErrorAnnotationError)
		}
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("publish terminal result: %w", err))
		}
	}
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", runErr)
		if gharuntime.IsToleratedJobFailure(runErr) {
			return buildkitepipeline.ContinueOnErrorExitStatus
		}
		return 1
	}
	return 0
}

func resolveRuntimeMise(ctx context.Context, configured, dataDir, privateRuntime string, stderr io.Writer) (string, error) {
	return resolveRuntimeMiseWithInstaller(ctx, configured, dataDir, privateRuntime, stderr, installRuntimeMise)
}

func resolveRuntimeMiseWithInstaller(ctx context.Context, configured, dataDir, privateRuntime string, stderr io.Writer, install func(context.Context, string, string, io.Writer) (string, error)) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("BUILDKITE_GHA_MISE must be an absolute path")
		}
		return validateRuntimeMise(ctx, configured, "")
	}
	if candidate, err := exec.LookPath("mise"); err == nil {
		if resolved, validationErr := validateRuntimeMise(ctx, candidate, ""); validationErr == nil {
			return resolved, nil
		}
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise on PATH is incompatible with minimum version %s; using the managed runtime copy\n", buildkitepipeline.MinimumMiseVersion)
	}
	return install(ctx, dataDir, privateRuntime, stderr)
}

func validateRuntimeMise(ctx context.Context, candidate, expectedDigest string) (string, error) {
	resolved, err := validateRuntimeMiseFile(ctx, candidate, expectedDigest)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, resolved, "--version")
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "MISE_") {
			command.Env = append(command.Env, value)
		}
	}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("validate runtime mise executable: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", fmt.Errorf("validate runtime mise executable: empty version")
	}
	reported := fields[0]
	if reported == "mise" && len(fields) > 1 {
		reported = fields[1]
	}
	if !miseVersionAtLeast(reported, buildkitepipeline.MinimumMiseVersion) {
		return "", fmt.Errorf("runtime mise executable reported version %q, want %q or newer", reported, buildkitepipeline.MinimumMiseVersion)
	}
	return resolved, nil
}

func validateRuntimeMiseFile(ctx context.Context, candidate, expectedDigest string) (string, error) {
	if !filepath.IsAbs(candidate) {
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			return "", fmt.Errorf("resolve runtime mise path: %w", err)
		}
		candidate = absolute
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve runtime mise executable: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect runtime mise executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("runtime mise executable %q is not an executable regular file", resolved)
	}
	if expectedDigest != "" {
		if info.Size() > runtimeMiseBinaryLimit {
			return "", fmt.Errorf("runtime mise executable exceeds %d-byte limit", runtimeMiseBinaryLimit)
		}
		actual, err := fileSHA256(ctx, resolved, runtimeMiseBinaryLimit)
		if err != nil {
			return "", fmt.Errorf("hash runtime mise executable: %w", err)
		}
		if actual != expectedDigest {
			return "", fmt.Errorf("runtime mise executable checksum mismatch")
		}
	}
	return resolved, nil
}

func miseVersionAtLeast(actual, minimum string) bool {
	parse := func(value string) ([3]int, bool) {
		var parsed [3]int
		parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
		if len(parts) != len(parsed) {
			return parsed, false
		}
		for index, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil || number < 0 {
				return parsed, false
			}
			parsed[index] = number
		}
		return parsed, true
	}
	got, ok := parse(actual)
	if !ok {
		return false
	}
	want, ok := parse(minimum)
	if !ok {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return got[index] > want[index]
		}
	}
	return true
}

type runtimeMiseRelease struct {
	asset         string
	cacheKey      string
	archiveDigest string
	binaryDigest  string
}

func selectRuntimeMiseRelease(goos, goarch string) (runtimeMiseRelease, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return runtimeMiseRelease{
			asset:         "linux-x64",
			cacheKey:      "linux-amd64",
			archiveDigest: runtimeMiseArchiveDigest,
			binaryDigest:  runtimeMiseBinaryDigest,
		}, nil
	case "darwin/arm64":
		return runtimeMiseRelease{
			asset:         "macos-arm64",
			cacheKey:      "darwin-arm64",
			archiveDigest: runtimeMiseDarwinARM64ArchiveDigest,
			binaryDigest:  runtimeMiseDarwinARM64BinaryDigest,
		}, nil
	default:
		return runtimeMiseRelease{}, fmt.Errorf("managed mise is unavailable on %s/%s; set BUILDKITE_GHA_MISE to a compatible absolute path", goos, goarch)
	}
}

func installRuntimeMise(ctx context.Context, dataDir, privateRuntime string, stderr io.Writer) (string, error) {
	selected, err := selectRuntimeMiseRelease(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	root := dataDir
	if root == "" {
		root, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve mise runtime cache: %w", err)
		}
		root = filepath.Join(root, "buildkite-gha", "mise", buildkitepipeline.MinimumMiseVersion)
	} else {
		root = filepath.Join(filepath.Dir(root), "runtime", buildkitepipeline.MinimumMiseVersion)
	}
	destination := filepath.Join(root, selected.cacheKey, "mise")
	if resolved, err := validateRuntimeMiseFile(ctx, destination, selected.binaryDigest); err == nil {
		return pinRuntimeMise(ctx, resolved, privateRuntime, selected.binaryDigest)
	}
	_, _ = fmt.Fprintf(stderr, "~~~ :mise: Install mise %s\n", buildkitepipeline.MinimumMiseVersion)
	url := fmt.Sprintf("https://github.com/jdx/mise/releases/download/v%s/mise-v%s-%s.tar.gz", buildkitepipeline.MinimumMiseVersion, buildkitepipeline.MinimumMiseVersion, selected.asset)
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("mise download redirected away from HTTPS")
			}
			if len(via) >= 10 {
				return errors.New("too many mise download redirects")
			}
			return nil
		},
	}
	cached, err := installRuntimeMiseFromPlatform(ctx, root, selected.cacheKey, client, url, selected.archiveDigest, selected.binaryDigest)
	if err != nil {
		return "", err
	}
	return pinRuntimeMise(ctx, cached, privateRuntime, selected.binaryDigest)
}

func pinRuntimeMise(ctx context.Context, cached, privateRuntime, expectedDigest string) (string, error) {
	if privateRuntime == "" {
		return "", fmt.Errorf("private action runtime directory is required")
	}
	resolvedRoot, err := canonicalNonSymlinkDirectory(privateRuntime)
	if err != nil {
		return "", fmt.Errorf("private action runtime directory contains a symlink")
	}
	source, err := os.Open(cached)
	if err != nil {
		return "", fmt.Errorf("open cached mise executable: %w", err)
	}
	defer func() { _ = source.Close() }()
	destination := filepath.Join(resolvedRoot, "mise")
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		return "", fmt.Errorf("create private mise executable: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(source, runtimeMiseBinaryLimit+1))
	closeErr := output.Close()
	if copyErr != nil {
		return "", fmt.Errorf("copy private mise executable: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("write private mise executable: %w", closeErr)
	}
	if written > runtimeMiseBinaryLimit {
		return "", fmt.Errorf("cached mise executable exceeds %d-byte limit", runtimeMiseBinaryLimit)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return "", fmt.Errorf("cached mise executable checksum verification failed")
	}
	resolved, err := validateRuntimeMise(ctx, destination, expectedDigest)
	if err != nil {
		return "", fmt.Errorf("validate private mise executable: %w", err)
	}
	return resolved, nil
}

func installRuntimeMiseFrom(ctx context.Context, root string, client *http.Client, sourceURL, archiveDigest, binaryDigest string) (string, error) {
	return installRuntimeMiseFromPlatform(ctx, root, "linux-x64", client, sourceURL, archiveDigest, binaryDigest)
}

func installRuntimeMiseFromPlatform(ctx context.Context, root, cacheKey string, client *http.Client, sourceURL, archiveDigest, binaryDigest string) (string, error) {
	destinationDir := filepath.Join(root, cacheKey)
	parent := filepath.Dir(destinationDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create mise runtime cache: %w", err)
	}
	resolvedParent, err := canonicalNonSymlinkDirectory(parent)
	if err != nil {
		return "", fmt.Errorf("mise runtime cache contains a symlink")
	}
	destinationDir = filepath.Join(resolvedParent, cacheKey)
	destination := filepath.Join(destinationDir, "mise")
	if resolved, err := validateRuntimeMiseFile(ctx, destination, binaryDigest); err == nil {
		return resolved, nil
	}
	staging, err := os.MkdirTemp(resolvedParent, "."+cacheKey+".")
	if err != nil {
		return "", fmt.Errorf("stage mise runtime: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	archive := filepath.Join(staging, "mise.tar.gz")
	if err := downloadRuntimeMise(ctx, client, sourceURL, archive, archiveDigest); err != nil {
		return "", err
	}
	stagedExecutable := filepath.Join(staging, "mise")
	if err := extractRuntimeMise(archive, stagedExecutable, binaryDigest); err != nil {
		return "", err
	}
	if _, err := validateRuntimeMiseFile(ctx, stagedExecutable, binaryDigest); err != nil {
		return "", fmt.Errorf("validate downloaded mise executable: %w", err)
	}
	if err := os.Remove(archive); err != nil {
		return "", fmt.Errorf("remove staged mise archive: %w", err)
	}
	if _, err := os.Lstat(destinationDir); err == nil {
		if resolved, validationErr := validateRuntimeMiseFile(ctx, destination, binaryDigest); validationErr == nil {
			return resolved, nil
		}
		invalid := destinationDir + fmt.Sprintf(".invalid-%d", time.Now().UnixNano())
		if err := os.Rename(destinationDir, invalid); err == nil {
			_ = os.RemoveAll(invalid)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect mise runtime cache: %w", err)
	}
	if err := os.Rename(staging, destinationDir); err != nil {
		if resolved, validationErr := validateRuntimeMiseFile(ctx, destination, binaryDigest); validationErr == nil {
			return resolved, nil
		}
		return "", fmt.Errorf("publish mise runtime cache: %w", err)
	}
	staging = ""
	resolved, err := validateRuntimeMiseFile(ctx, destination, binaryDigest)
	if err != nil {
		return "", fmt.Errorf("validate installed mise cache: %w", err)
	}
	return resolved, nil
}

func downloadRuntimeMise(ctx context.Context, client *http.Client, sourceURL, destination, expectedDigest string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("create mise download request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download mise: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download mise: unexpected HTTP status %s", response.Status)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create mise archive: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, runtimeMiseArchiveLimit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download mise archive: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("write mise archive: %w", closeErr)
	}
	if written > runtimeMiseArchiveLimit {
		return fmt.Errorf("download mise archive: exceeds %d-byte limit", runtimeMiseArchiveLimit)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return fmt.Errorf("mise archive checksum verification failed")
	}
	return nil
}

func extractRuntimeMise(archive, destination, expectedDigest string) error {
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open mise archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open mise gzip archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	found := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read mise archive: %w", err)
		}
		if strings.TrimPrefix(filepath.ToSlash(header.Name), "./") != "mise/bin/mise" {
			continue
		}
		if found {
			return fmt.Errorf("mise archive contains duplicate executable")
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > runtimeMiseBinaryLimit {
			return fmt.Errorf("mise archive executable is not a bounded regular file")
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
		if err != nil {
			return fmt.Errorf("create mise executable: %w", err)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), tarReader)
		closeErr := output.Close()
		if copyErr != nil {
			return fmt.Errorf("extract mise executable: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("write mise executable: %w", closeErr)
		}
		if written != header.Size || hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
			return fmt.Errorf("mise executable checksum verification failed")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("mise archive does not contain mise/bin/mise")
	}
	return nil
}

func fileSHA256(ctx context.Context, path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var read int64
	for read <= limit {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk := buffer
		if remaining := limit + 1 - read; remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}
		n, readErr := file.Read(chunk)
		if n > 0 {
			_, _ = hash.Write(chunk[:n])
			read += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if read > limit {
		return "", fmt.Errorf("exceeds %d-byte limit", limit)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func prepareMiseDataDir(path string, stderr io.Writer) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise cache %q is invalid; using the ephemeral agent cache: %v\n", path, err)
		return ""
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise cache %q is unavailable; using the ephemeral agent cache: %v\n", path, err)
		return ""
	}
	resolved, err := canonicalNonSymlinkDirectory(absolute)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise cache %q is not a real directory; using the ephemeral agent cache\n", path)
		return ""
	}
	return resolved
}

func canonicalNonSymlinkDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("not a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	canonicalInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(info, canonicalInfo) {
		return "", fmt.Errorf("directory changed while canonicalizing")
	}
	return resolved, nil
}

func hasGitHubActionLocks(locks []plan.ActionLock) bool {
	for _, lock := range locks {
		if lock.Source == "github" {
			return true
		}
	}
	return false
}

func cacheServiceRequired(locks []plan.ActionLock) (bool, error) {
	required := false
	for _, lock := range locks {
		descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
		if descriptor.Service == actionintegration.ServiceCache {
			required = true
			if err := actionintegration.ValidateCacheCommit(lock.Commit); err != nil {
				return false, fmt.Errorf("unsupported cache action: %w", err)
			}
		}
	}
	return required, nil
}

func resultProducer(job plan.Job, planDigest, expectedDigest string) (transport.Producer, bool, error) {
	if os.Getenv("BUILDKITE") == "" {
		if len(job.NeedSources) != 0 {
			return transport.Producer{}, false, fmt.Errorf("plans with prerequisites require Buildkite result identity")
		}
		return transport.Producer{}, false, nil
	}
	if expectedDigest == "" {
		return transport.Producer{}, false, fmt.Errorf("result publication in Buildkite requires BUILDKITE_GHA_PLAN_DIGEST")
	}
	if expectedDigest != planDigest {
		return transport.Producer{}, false, fmt.Errorf("plan digest %q does not match expected digest %q", planDigest, expectedDigest)
	}
	producer := transport.Producer{
		BuildID: os.Getenv("BUILDKITE_BUILD_ID"),
		JobID:   os.Getenv("BUILDKITE_JOB_ID"),
		StepKey: os.Getenv("BUILDKITE_STEP_KEY"),
	}
	if err := producer.Validate(); err != nil {
		return transport.Producer{}, false, fmt.Errorf("result publication in Buildkite requires valid BUILDKITE_BUILD_ID, BUILDKITE_JOB_ID, and BUILDKITE_STEP_KEY: %w", err)
	}
	if producer.StepKey != job.Target.StepKey {
		return transport.Producer{}, false, fmt.Errorf("result producer step %q does not match plan target %q", producer.StepKey, job.Target.StepKey)
	}
	return producer, true, nil
}

func writeJobResult(path string, result gharuntime.JobResult) error {
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func terminalErrorConclusion(ctx context.Context) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	return "failure"
}

func publishTerminalResult(agent transport.Agent, root string, job plan.Job, planDigest string, producer transport.Producer, result gharuntime.JobResult) (transport.Publication, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resultPublicationTimeout)
	defer cancel()
	workflow := strings.TrimPrefix(job.Workflow.Digest, "sha256:")
	return gharuntime.PublishJobResult(ctx, agent, root, workflow, job.Target.StepKey, planDigest, producer, result)
}

type runJobOptions struct {
	planPath        string
	planDigest      string
	planProducer    string
	resultPath      string
	hostedToolCache bool
}

func runJobArgs(args []string) (runJobOptions, error) {
	var options runJobOptions
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--hosted-tool-cache":
			if seen[args[i]] {
				return runJobOptions{}, fmt.Errorf("%s may only be specified once", args[i])
			}
			seen[args[i]] = true
			options.hostedToolCache = true
		case "--plan", "--plan-digest", "--plan-producer", "--result":
			option := args[i]
			if seen[option] {
				return runJobOptions{}, fmt.Errorf("%s may only be specified once", option)
			}
			seen[option] = true
			i++
			if i == len(args) {
				return runJobOptions{}, fmt.Errorf("%s requires a value", option)
			}
			switch option {
			case "--plan":
				options.planPath = args[i]
			case "--plan-digest":
				options.planDigest = args[i]
			case "--plan-producer":
				options.planProducer = args[i]
			case "--result":
				options.resultPath = args[i]
			}
		default:
			return runJobOptions{}, fmt.Errorf("unknown option %q", args[i])
		}
	}
	if seen["--plan"] {
		if seen["--plan-digest"] || seen["--plan-producer"] {
			return runJobOptions{}, fmt.Errorf("--plan cannot be combined with --plan-digest or --plan-producer")
		}
		if options.planPath == "" {
			return runJobOptions{}, fmt.Errorf("--plan requires a path")
		}
		return options, nil
	}
	if options.planDigest == "" || options.planProducer == "" {
		return runJobOptions{}, fmt.Errorf("--plan or both --plan-digest and --plan-producer are required")
	}
	planPath, err := buildkitepipeline.PlanPath(options.planDigest)
	if err != nil {
		return runJobOptions{}, err
	}
	options.planPath = planPath
	return options, nil
}

func verifyBuildkiteTarget(job plan.Job) error {
	stepKey := os.Getenv("BUILDKITE_STEP_KEY")
	queue := os.Getenv("BUILDKITE_AGENT_META_DATA_QUEUE")
	if os.Getenv("BUILDKITE") != "" && stepKey == "" {
		return fmt.Errorf("buildkite execution requires BUILDKITE_STEP_KEY")
	}
	if os.Getenv("BUILDKITE") != "" && job.Target.Queue != "" && queue == "" {
		return fmt.Errorf("buildkite execution with an explicit target queue requires BUILDKITE_AGENT_META_DATA_QUEUE")
	}
	if stepKey != "" && stepKey != job.Target.StepKey {
		return fmt.Errorf("plan targets step %q, executing step is %q", job.Target.StepKey, stepKey)
	}
	if job.Target.Queue != "" && queue != "" && queue != job.Target.Queue {
		return fmt.Errorf("plan targets queue %q, executing queue is %q", job.Target.Queue, queue)
	}
	return nil
}

func validate(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	workflowPath, eventPath, format, profile, err := validateArgs(args)
	if err != nil {
		return usageError(stderr, "validate: %v", err)
	}
	if profile != "" && eventPath == "" {
		return usageError(stderr, "validate: --event-path is required with --profile")
	}
	out := newProcessingOutput("validate", format, stdout, stderr, agent)
	var loadEvent func() ([]byte, error)
	if eventPath != "" {
		loadEvent = func() ([]byte, error) { return os.ReadFile(eventPath) }
	}
	source, event, ok := loadProcessingInputs(out, workflowPath, profile, "event input could not be read", loadEvent)
	if !ok {
		return 1
	}
	if profile != "" {
		parsed, parseErr := workflow.Parse(workflowPath, source)
		effectiveEvent, eventErr := newEffectiveEvent(event, effectiveEventFromPath, os.Getenv)
		if parseErr == nil && eventErr == nil && !parsed.ReusableOnly() {
			selection, triggerErr := selectWorkflowTrigger(parsed.Triggers, effectiveEvent)
			if triggerErr != nil {
				report := triggerFailureProcessingReport(workflowInput{Path: workflowPath, Source: source}, triggerErr)
				_ = out.write(report)
				return 1
			}
			if !selection.Applicable {
				report := triggerProcessingReport(workflowPath, source)
				report.Result = "not-applicable"
				if out.write(report) != nil {
					return 1
				}
				return 0
			}
		}
	}
	processingReport, ok := validatedProcessingReport(out, workflowPath, profile, source, event, eventPath != "")
	if !ok {
		return 1
	}
	if profile != "" {
		_, _, distributionDigest, executableErr := executable()
		if executableErr != nil {
			processingReport.AddEnvironmentFailure("compiler executable could not be inspected")
			processingReport.Result = "indeterminate"
			_ = out.write(processingReport)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runtimeDistributions := map[compiler.Platform]string{compiler.PlatformLinuxAMD64: distributionDigest}
		preflight, profileErr := compileHosted(ctx, workflowPath, source, event, version, distributionDigest, "buildkite-gha-profile-importer", "", nil, runtimeDistributions, nil)
		applyHostedPreflight(&processingReport, preflight)
		if profileErr != nil {
			if ctx.Err() != nil || errors.Is(profileErr, context.Canceled) {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: profile evaluation interrupted: %v\n", profileErr)
				return 1
			}
			processingReport.Result = classifyHostedFailure(&processingReport, workflowPath, profileErr)
			_ = out.write(processingReport)
			return 1
		}
		processingReport.SetStage(string(compiler.StageAdmission), compatibility.Passed)
		processingReport.Admission.Result = "admitted"
		if preflight.HasActions {
			processingReport.Diagnostics = append(processingReport.Diagnostics, compatibility.Diagnostic{
				Level: "warning", Code: "W_ACTION_RUNTIME_UNKNOWN", Category: "compatibility", Stage: string(compiler.StageAdmission),
				Message: "resolved action metadata cannot prove that arbitrary action code is independent of GitHub-only runtime services",
			})
		}
		processingReport.Result = "admitted"
		if out.write(processingReport) != nil {
			return 1
		}
		return 0
	}
	processingReport.Result = "compilable"
	if out.write(processingReport) != nil {
		return 1
	}
	return 0
}

func validateArgs(args []string) (workflowPath, eventPath, format, profile string, err error) {
	format = "text"
	filtered := make([]string, 0, len(args))
	formatSeen := false
	profileSeen := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--format" && args[i] != "--profile" {
			filtered = append(filtered, args[i])
			continue
		}
		option := args[i]
		if option == "--format" && formatSeen {
			return "", "", "", "", fmt.Errorf("--format may only be specified once")
		}
		if option == "--profile" && profileSeen {
			return "", "", "", "", fmt.Errorf("--profile may only be specified once")
		}
		i++
		if i == len(args) {
			return "", "", "", "", fmt.Errorf("%s requires a value", option)
		}
		if option == "--format" {
			formatSeen = true
			format = args[i]
			if format != "text" && format != "json" {
				return "", "", "", "", fmt.Errorf("--format must be text or json")
			}
		} else {
			profileSeen = true
			profile = args[i]
			if profile != hostedProfile && profile != legacyHostedTokenlessProfile {
				return "", "", "", "", fmt.Errorf("--profile must be %q", hostedProfile)
			}
			profile = hostedProfile
		}
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	return workflowPath, eventPath, format, profile, err
}

func compile(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	workflowPath, eventPath, format, err := compileArgs(args)
	if err != nil {
		return usageError(stderr, "compile: %v", err)
	}
	if eventPath == "" {
		return usageError(stderr, "compile: --event-path is required")
	}
	out := newProcessingOutput("compile", "text", stderr, stderr, agent)
	source, event, ok := loadProcessingInputs(out, workflowPath, "", "event input could not be read", func() ([]byte, error) { return os.ReadFile(eventPath) })
	if !ok {
		return 1
	}
	processingReport, ok := validatedProcessingReport(out, workflowPath, "", source, event, true)
	if !ok {
		return 1
	}
	var result []byte
	var warnings []compiler.Warning
	if format == "ir-json" {
		result, err = compiler.Compile(workflowPath, source, event)
		if err == nil {
			var ir compiler.IR
			if decodeErr := json.Unmarshal(result, &ir); decodeErr != nil {
				err = fmt.Errorf("decode compiler IR: %w", decodeErr)
			} else {
				warnings = ir.Warnings
			}
		}
	} else {
		digest, digestErr := executableDigest()
		if digestErr != nil {
			processingReport.AddEnvironmentFailure("compiler executable could not be inspected")
			processingReport.Result = "indeterminate"
			_ = out.write(processingReport)
			return 1
		}
		bundle, compileErr := compiler.CompileBundle(workflowPath, source, event, version, digest, "gha-importer")
		processingReport.ApplyEvidence(bundle.Processing)
		err = compileErr
		result = bundle.Pipeline
		warnings = bundle.IR.Warnings
	}
	if err != nil {
		processingReport.AddFailure(workflowPath, string(compiler.StagePlans), compiler.CodePlanConstruction, "compatibility", err)
		processingReport.Result = "incompatible"
		_ = out.write(processingReport)
		return 1
	}
	processingReport.Result = "compilable"
	_ = out.write(processingReport)
	writeCompilerWarnings(stderr, "compile", workflowPath, warnings)
	if _, err := stdout.Write(result); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: write output: %v\n", err)
		return 1
	}
	return 0
}

func writeCompilerWarnings(stderr io.Writer, command, path string, warnings []compiler.Warning) {
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: %s: warning: %s:%d:%d: [%s] %s\n", command, path, warning.Line, warning.Column, warning.Code, warning.Message)
	}
}

func upload(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	return uploadFromPlatform(runtime.GOOS, runtime.GOARCH, args, stdout, stderr, version, agent)
}

func uploadFromPlatform(goos, goarch string, args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	if err := validateImporterPlatform(goos, goarch); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	uploadArguments, err := parseUploadArgs(args)
	if err != nil {
		return usageError(stderr, "upload: %v", err)
	}
	return uploadParsed(uploadArguments, stdout, stderr, version, agent)
}

func uploadParsed(uploadArguments parsedUploadArgs, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	workflowOperands, eventPath := uploadArguments.workflowOperands, uploadArguments.eventPath
	importerStep := os.Getenv("BUILDKITE_STEP_KEY")
	if os.Getenv("BUILDKITE") != "true" || strings.TrimSpace(importerStep) == "" {
		return usageError(stderr, "upload: BUILDKITE=true and BUILDKITE_STEP_KEY are required")
	}
	for _, retired := range []string{legacyTargetQueueEnvironment, legacyRuntimeImageEnvironment} {
		if os.Getenv(retired) != "" {
			return usageError(stderr, "upload: %s is no longer supported; configure runner profiles with --runner-queue and --runner-image, or with the plugin runners array", retired)
		}
	}
	out := newProcessingOutput("upload", "text", stderr, stderr, agent)
	var workflows []workflowInput
	var err error
	if uploadArguments.explicitWorkflowPaths {
		workflows, err = expandExplicitWorkflowPaths(workflowOperands)
	} else {
		workflows, err = expandWorkflowOperands(workflowOperands)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	runnableWorkflowCount := 0
	for i := range workflows {
		workflows[i].Source, err = os.ReadFile(workflows[i].Path)
		if err != nil {
			report := compatibility.EnvironmentProcessingReport(workflows[i].Path, hostedProfile, "workflow input could not be read")
			return out.fail(report, fmt.Errorf("read workflow %s: %w", workflows[i].CanonicalPath, err))
		}
		parsed, parseErr := workflow.Parse(workflows[i].Path, workflows[i].Source)
		if parseErr != nil {
			_, _ = validatedProcessingReport(out, workflows[i].Path, hostedProfile, workflows[i].Source, nil, false)
			return 1
		}
		workflows[i].ReusableOnly = parsed.ReusableOnly()
		workflows[i].Name = parsed.Name
		workflows[i].Triggers = parsed.Triggers
		if !workflows[i].ReusableOnly {
			runnableWorkflowCount++
		}
	}
	if runnableWorkflowCount == 0 {
		if len(workflowOperands) == 1 {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: workflow pattern %q matched only reusable workflow_call workflows; there is nothing to upload\n", workflowOperands[0])
		} else {
			_, _ = fmt.Fprintln(stderr, "buildkite-gha: upload: workflow paths matched only reusable workflow_call workflows; there is nothing to upload")
		}
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, input := range workflows {
		if input.ReusableOnly {
			continue
		}
		validation, validationErr := compiler.Validate(input.Path, input.Source)
		if validation.RuntimeMatrixBoundary {
			report := compatibility.InitialProcessingReport(input.Path, hostedProfile, false, validation, validationErr)
			report.Result = "incompatible"
			_ = out.write(report)
			return 1
		}
	}
	eventSource, eventOrigin, err := loadEffectiveEventSource(ctx, eventPath, agent)
	if err != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				return out.fail(compatibility.EventInputProcessingReport(input.Path, hostedProfile, input.Source, "event input could not be acquired"), err)
			}
		}
		return 1
	}
	effectiveEvent, err := newEffectiveEvent(eventSource, eventOrigin, os.Getenv)
	if err != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				_, _ = validatedProcessingReport(out, input.Path, hostedProfile, input.Source, eventSource, true)
				return 1
			}
		}
		return 1
	}
	for i := range workflows {
		if workflows[i].ReusableOnly {
			continue
		}
		selection, triggerErr := selectWorkflowTrigger(workflows[i].Triggers, effectiveEvent)
		if triggerErr != nil {
			triggerErr = fmt.Errorf("%s: translate workflow triggers: %w", workflows[i].CanonicalPath, triggerErr)
			_ = out.write(triggerFailureProcessingReport(workflows[i], triggerErr))
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", triggerErr)
			return 1
		}
		workflows[i].Applicable = selection.Applicable
		workflows[i].TriggerCondition = selection.Condition
		workflows[i].SkipReason = selection.SkipReason
	}
	processingReports := make([]compatibility.ProcessingReport, len(workflows))
	for i, input := range workflows {
		if !input.Applicable {
			continue
		}
		validationOptions := hostedOptions("", uploadArguments.runnerTargets, nil)
		validationOptions.StepKeyNamespace = input.StepKeyNamespace
		validation, validationErr := compiler.ValidateEventWithOptions(input.Path, input.Source, effectiveEvent.Source, validationOptions)
		processingReports[i] = compatibility.InitialProcessingReport(input.Path, hostedProfile, true, validation, validationErr)
		if validationErr != nil {
			processingReports[i].Result = "incompatible"
			out.annotate(processingReports[i])
		}
	}
	executablePath, executableContents, distributionDigest, err := executable()
	if err != nil {
		for i, input := range workflows {
			if !input.Applicable {
				continue
			}
			processingReports[i].AddEnvironmentFailure("compiler executable could not be inspected")
			processingReports[i].Result = "indeterminate"
			_ = out.write(processingReports[i])
		}
		return 1
	}
	runtimePaths := uploadArguments.runtimeDistributionPaths
	if uploadArguments.pluginAcquisition != nil {
		requiredPlatforms := make(map[compiler.Platform]bool, 2)
		for i, input := range workflows {
			if !input.Applicable || processingReportHasErrors(processingReports[i]) {
				continue
			}
			platforms, platformErr := requiredRuntimePlatforms(input.Path, input.Source, effectiveEvent.Source, "", uploadArguments.runnerTargets)
			if platformErr != nil {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", platformErr)
				return 1
			}
			for platform := range platforms {
				requiredPlatforms[platform] = true
			}
		}
		runtimeDistributions, acquireErr := uploadArguments.pluginAcquisition.acquire(ctx, requiredPlatforms, runtimeDistribution{contents: executableContents, digest: distributionDigest})
		if acquireErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %v\n", acquireErr)
			return 1
		}
		return finishUpload(ctx, uploadArguments, stdout, stderr, version, agent, workflows, effectiveEvent, executablePath, executableContents, distributionDigest, importerStep, processingReports, out, runtimeDistributions)
	}
	runtimeDistributions, err := loadRuntimeDistributions(runtimePaths)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	return finishUpload(ctx, uploadArguments, stdout, stderr, version, agent, workflows, effectiveEvent, executablePath, executableContents, distributionDigest, importerStep, processingReports, out, runtimeDistributions)
}

func finishUpload(ctx context.Context, uploadArguments parsedUploadArgs, stdout, stderr io.Writer, version string, agent transport.Agent, workflows []workflowInput, effectiveEvent effectiveEventSelection, executablePath string, executableContents []byte, distributionDigest, importerStep string, processingReports []compatibility.ProcessingReport, out processingOutput, runtimeDistributions map[compiler.Platform]runtimeDistribution) int {
	runtimeDigests := map[compiler.Platform]string{compiler.PlatformLinuxAMD64: distributionDigest}
	for platform, runtimeDistribution := range runtimeDistributions {
		runtimeDigests[platform] = runtimeDistribution.digest
	}
	authentication := jobScopedActionSourceAuthentication(stderr)
	generatedWorkflows := make([]buildkitepipeline.Workflow, 0, len(workflows))
	planArtifacts := make([]compiler.PlanArtifact, 0)
	jobCount := 0
	for i, input := range workflows {
		if input.ReusableOnly {
			continue
		}
		if !input.Applicable {
			label := input.Name
			if label == "" {
				label = input.CanonicalPath
			}
			generatedWorkflows = append(generatedWorkflows, buildkitepipeline.Workflow{
				GroupLabel: label,
				GroupKey:   "gha-workflow-" + input.Identity,
				CheckName:  "Buildkite / " + label + " (" + effectiveEvent.Event.Event + ")",
				SkipReason: input.SkipReason,
			})
			continue
		}
		if processingReportHasErrors(processingReports[i]) {
			generatedWorkflows = append(generatedWorkflows, failedGeneratedWorkflow(input, effectiveEvent.Event.Event, processingReports[i]))
			continue
		}
		preflight, err := compileHostedNamespaced(ctx, input.Path, input.Source, effectiveEvent.Source, version, distributionDigest, importerStep, "", uploadArguments.runnerTargets, runtimeDigests, input.StepKeyNamespace, authentication)
		applyHostedPreflight(&processingReports[i], preflight)
		if err != nil {
			processingReports[i].Result = classifyHostedFailure(&processingReports[i], input.Path, err)
			var failure *hostedFailure
			if errors.As(err, &failure) && failure.Kind == hostedEvaluationFailure {
				out.annotate(processingReports[i])
				generatedWorkflows = append(generatedWorkflows, failedGeneratedWorkflow(input, effectiveEvent.Event.Event, processingReports[i]))
				continue
			}
			_ = out.write(processingReports[i])
			return 1
		}
		bundle := preflight.Bundle
		label := bundle.IR.Workflow.Name
		if label == "" {
			label = input.CanonicalPath
		}
		generated := bundle.GeneratedWorkflow
		generated.GroupLabel = label
		generated.GroupKey = "gha-workflow-" + input.Identity
		generated.CheckName = "Buildkite / " + label + " (" + effectiveEvent.Event.Event + ")"
		generated.Condition = input.TriggerCondition
		generatedWorkflows = append(generatedWorkflows, generated)
		planArtifacts = append(planArtifacts, bundle.Plans...)
		jobCount += len(bundle.Plans)
		processingReports[i].SetStage(string(compiler.StageAdmission), compatibility.Passed)
		processingReports[i].Admission.Result = "admitted"
		processingReports[i].Result = "admitted"
		writeCompilerWarnings(stderr, "upload", input.CanonicalPath, bundle.IR.Warnings)
	}
	aggregatePipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		CompilerStep: importerStep,
		Workflows:    generatedWorkflows,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: emit aggregate Buildkite pipeline: %v\n", err)
		return 1
	}
	for i, input := range workflows {
		if input.Applicable {
			_ = compatibility.WriteProcessing(stdout, "text", processingReports[i])
			if !processingReportHasErrors(processingReports[i]) {
				out.annotate(processingReports[i])
			}
		}
	}
	artifacts := make([]transport.Artifact, 0, 1+len(runtimeDistributions)+len(planArtifacts))
	distributionPath, err := buildkitepipeline.DistributionPath(distributionDigest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	artifacts = append(artifacts, transport.Artifact{Path: distributionPath, Digest: distributionDigest, Contents: executableContents})
	artifactPaths := map[string]struct{}{distributionPath: {}}
	for _, platform := range []compiler.Platform{compiler.PlatformLinuxAMD64, compiler.PlatformDarwinARM64} {
		runtimeDistribution, ok := runtimeDistributions[platform]
		if !ok || runtimeDistribution.digest == distributionDigest {
			continue
		}
		path, pathErr := buildkitepipeline.DistributionPath(runtimeDistribution.digest)
		if pathErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", pathErr)
			return 1
		}
		if _, exists := artifactPaths[path]; exists {
			continue
		}
		artifactPaths[path] = struct{}{}
		artifacts = append(artifacts, transport.Artifact{Path: path, Digest: runtimeDistribution.digest, Contents: runtimeDistribution.contents})
	}
	for _, jobPlan := range planArtifacts {
		if _, exists := artifactPaths[jobPlan.Path]; exists {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: duplicate aggregate artifact path %q\n", jobPlan.Path)
			return 1
		}
		artifactPaths[jobPlan.Path] = struct{}{}
		artifacts = append(artifacts, transport.Artifact{Path: jobPlan.Path, Digest: jobPlan.Digest, Contents: jobPlan.Contents})
	}
	root, err := os.MkdirTemp("", "buildkite-gha-upload-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: create artifact root: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	if err := transport.UploadArtifacts(ctx, agent, root, artifacts, aggregatePipeline); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "Uploaded %d jobs from %d workflows using %s with importer %s.\n", jobCount, len(generatedWorkflows), executablePath, importerStep)
	return 0
}

func processingReportHasErrors(report compatibility.ProcessingReport) bool {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Level == "error" {
			return true
		}
	}
	return false
}

func failedGeneratedWorkflow(input workflowInput, event string, report compatibility.ProcessingReport) buildkitepipeline.Workflow {
	label := input.Name
	if label == "" {
		label = input.CanonicalPath
	}
	report.Diagnostics = append([]compatibility.Diagnostic(nil), report.Diagnostics...)
	report.Finalize()
	messages := make([]string, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Level == "error" {
			message := diagnostic.Message
			if diagnostic.Code != "" {
				message = "[" + diagnostic.Code + "] " + message
			}
			var attribution []string
			if diagnostic.Job != "" {
				attribution = append(attribution, "job="+diagnostic.Job)
			}
			if diagnostic.Step != 0 {
				attribution = append(attribution, fmt.Sprintf("step=%d", diagnostic.Step))
			}
			if len(attribution) != 0 {
				message += " {" + strings.Join(attribution, ", ") + "}"
			}
			messages = append(messages, message)
		}
	}
	return buildkitepipeline.Workflow{
		GroupLabel: label,
		GroupKey:   "gha-workflow-" + input.Identity,
		CheckName:  "Buildkite / " + label + " (" + event + ")",
		Condition:  input.TriggerCondition,
		Failure:    &buildkitepipeline.Failure{Label: "Compiler errors", Message: strings.Join(messages, "\n")},
	}
}

func requiredRuntimePlatforms(workflowPath string, workflowSource, eventSource []byte, groupLabel string, configuredTargets map[string]compiler.RunnerTarget) (map[compiler.Platform]bool, error) {
	preflight, err := compiler.CompileWithOptions(workflowPath, workflowSource, eventSource, hostedOptions(groupLabel, configuredTargets, nil))
	if err != nil {
		return nil, err
	}
	var ir compiler.IR
	if err := json.Unmarshal(preflight, &ir); err != nil {
		return nil, fmt.Errorf("decode compiler preflight: %w", err)
	}
	platforms := make(map[compiler.Platform]bool, 2)
	for _, job := range ir.Jobs {
		platform, err := configuredRunnerPlatform(job.RunsOn, configuredTargets)
		if err != nil {
			return nil, fmt.Errorf("resolve runtime platform for job %q: %w", job.LogicalJobID, err)
		}
		platforms[platform] = true
	}
	return platforms, nil
}

func configuredRunnerPlatform(labels []string, configuredTargets map[string]compiler.RunnerTarget) (compiler.Platform, error) {
	if len(labels) == 0 {
		return compiler.Platform{}, fmt.Errorf("runs-on resolved to no labels")
	}
	canonical := strings.ToLower(strings.TrimSpace(labels[0]))
	if target, ok := configuredTargets[canonical]; ok {
		return target.Platform, nil
	}
	_, platform, err := supportedRunnerTarget(canonical)
	return platform, err
}

type effectiveEventOrigin string

const (
	effectiveEventFromPath    effectiveEventOrigin = "event-path"
	effectiveEventFromWebhook effectiveEventOrigin = "buildkite-webhook"
	effectiveEventFromBuild   effectiveEventOrigin = "buildkite-environment"
)

type effectiveEventSelection struct {
	Source         []byte
	Event          compiler.Event
	Origin         effectiveEventOrigin
	TriggerContext buildkitepipeline.TriggerConditionContext
}

func loadEffectiveEventSource(ctx context.Context, eventPath string, agent transport.Agent) ([]byte, effectiveEventOrigin, error) {
	if eventPath != "" {
		source, err := os.ReadFile(eventPath)
		return source, effectiveEventFromPath, err
	}
	webhook, metadataErr := agent.GetMetadataBounded(ctx, "buildkite:webhook", maxWebhookMetadataBytes+1)
	switch {
	case metadataErr == nil:
		webhook = bytes.TrimSuffix(webhook, []byte("\n"))
		if len(webhook) > maxWebhookMetadataBytes {
			return nil, "", fmt.Errorf("buildkite:webhook exceeds %d bytes", maxWebhookMetadataBytes)
		}
		source, err := buildkiteWebhookEventSource(os.Getenv, webhook)
		return source, effectiveEventFromWebhook, err
	case errors.Is(metadataErr, transport.ErrMetadataUnavailable):
		source, err := buildkiteEventSource(os.Getenv)
		return source, effectiveEventFromBuild, err
	default:
		return nil, "", metadataErr
	}
}

func newEffectiveEvent(source []byte, origin effectiveEventOrigin, getenv func(string) string) (effectiveEventSelection, error) {
	event, err := compiler.ParseEvent(source)
	if err != nil {
		return effectiveEventSelection{}, err
	}
	effective := effectiveEventSelection{
		Source: source, Event: event, Origin: origin,
		TriggerContext: snapshotTriggerConditionContext(event),
	}
	if origin == effectiveEventFromPath {
		return effective, nil
	}
	predicate := "true"
	if buildSource := strings.TrimSpace(getenv("BUILDKITE_SOURCE")); buildSource != "" {
		predicate = "build.source == " + triggerConditionLiteral(buildSource)
	}
	effective.TriggerContext.EventPredicate = predicate
	return effective, nil
}

func snapshotTriggerConditionContext(event compiler.Event) buildkitepipeline.TriggerConditionContext {
	context := buildkitepipeline.TriggerConditionContext{
		EventPredicate:        "true",
		Branch:                "null",
		Tag:                   "null",
		PullRequestBaseBranch: "null",
		PullRequestAction:     "null",
	}
	if branch, ok := strings.CutPrefix(event.Ref, "refs/heads/"); ok {
		context.Branch = triggerConditionLiteral(branch)
	}
	if tag, ok := strings.CutPrefix(event.Ref, "refs/tags/"); ok {
		context.Tag = triggerConditionLiteral(tag)
	}
	if action, ok := event.Payload["action"].(string); ok && strings.TrimSpace(action) != "" {
		context.PullRequestAction = triggerConditionLiteral(action)
	}
	if pullRequest, ok := event.Payload["pull_request"].(map[string]any); ok {
		if base, ok := pullRequest["base"].(map[string]any); ok {
			if branch, ok := base["ref"].(string); ok && strings.TrimSpace(branch) != "" {
				context.PullRequestBaseBranch = triggerConditionLiteral(branch)
			}
		}
	}
	return context
}

type workflowTriggerSelection struct {
	Condition, SkipReason string
	Applicable            bool
}

func selectWorkflowTrigger(triggers []workflow.Trigger, event effectiveEventSelection) (workflowTriggerSelection, error) {
	condition, applicable, err := buildkitepipeline.TranslateEventTriggerCondition(triggers, event.Event.Event, event.TriggerContext)
	if err != nil {
		return workflowTriggerSelection{}, err
	}
	return workflowTriggerSelection{
		Condition:  condition,
		Applicable: applicable,
		SkipReason: buildkitepipeline.TriggerEventSkipReason(triggers, event.Event.Event),
	}, nil
}

func triggerConditionLiteral(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func triggerFailureProcessingReport(input workflowInput, err error) compatibility.ProcessingReport {
	report := triggerProcessingReport(input.Path, input.Source)
	report.AddFailure(input.Path, string(compiler.StagePipeline), compiler.CodePipelineGeneration, "compatibility", err)
	report.Result = "incompatible"
	return report
}

func triggerProcessingReport(path string, source []byte) compatibility.ProcessingReport {
	parsed, _ := compiler.ParseWorkflow(path, source)
	report := compatibility.NewProcessingReport(path, hostedProfile)
	report.LogicalJobs = parsed.LogicalJobs
	report.SetStage(string(compiler.StageWorkflowParsing), compatibility.Passed)
	report.SetStage(string(compiler.StageEventValidation), compatibility.Passed)
	for _, job := range parsed.ParsedJobs {
		report.Jobs = append(report.Jobs, compatibility.JobResult{
			ID: job.ID, Result: compatibility.NotEvaluated,
			Location: &compatibility.SourceLocation{Path: job.Path, Line: job.Source.Start.Line, Column: job.Source.Start.Column},
		})
	}
	return report
}

type workflowInput struct {
	Path, CanonicalPath, Identity, StepKeyNamespace, Name string
	Source                                                []byte
	Triggers                                              []workflow.Trigger
	TriggerCondition, SkipReason                          string
	ReusableOnly, Applicable                              bool
}

func expandWorkflowOperands(operands []string) ([]workflowInput, error) {
	if len(operands) == 0 {
		return nil, fmt.Errorf("workflow path is required")
	}
	if len(operands) == 1 {
		return expandWorkflowPattern(operands[0])
	}
	return expandExplicitWorkflowPaths(operands)
}

func expandExplicitWorkflowPaths(operands []string) ([]workflowInput, error) {
	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, fmt.Errorf("locate checked-out git repository: %w", err)
	}
	root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
	matches := make([]workflowInput, 0, len(operands))
	for _, operand := range operands {
		absolute, err := filepath.Abs(operand)
		if err != nil {
			return nil, fmt.Errorf("resolve workflow path %q: %w", operand, err)
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("workflow path %q is outside the checked-out git repository", operand)
		}
		canonical := filepath.ToSlash(filepath.Clean(relative))
		if err := requireRegularWorkflowFile(absolute, operand); err != nil {
			if strings.ContainsAny(operand, "*?[") {
				return nil, fmt.Errorf("workflow list entries must be explicit paths; glob pattern %q is not allowed", operand)
			}
			return nil, err
		}
		extension := filepath.Ext(canonical)
		if extension != ".yml" && extension != ".yaml" {
			return nil, fmt.Errorf("workflow path %q must end in .yml or .yaml", operand)
		}
		output, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", ":(top,literal)"+canonical).Output()
		entries := bytes.Split(output, []byte{0})
		if err != nil || len(entries) != 2 || string(entries[0]) != canonical || len(entries[1]) != 0 {
			if strings.ContainsAny(operand, "*?[") {
				return nil, fmt.Errorf("workflow list entries must be explicit paths; glob pattern %q is not allowed", operand)
			}
			return nil, fmt.Errorf("workflow path %q is not tracked by git", operand)
		}
		matches = append(matches, workflowInput{Path: filepath.Join(root, filepath.FromSlash(canonical)), CanonicalPath: canonical})
	}
	return workflowInputs(matches, true)
}

func expandWorkflowPattern(pattern string) ([]workflowInput, error) {
	allWorkflows := pattern == "*"
	patternHasMeta := allWorkflows || strings.ContainsAny(pattern, "*?[")
	if info, err := os.Stat(pattern); !allWorkflows && err == nil && !info.IsDir() {
		// An existing path is always literal, even when its filename contains
		// glob metacharacters. This preserves the pre-pattern CLI contract.
		patternHasMeta = false
	}
	rootBytes, rootErr := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if rootErr != nil {
		if !patternHasMeta {
			if info, err := os.Stat(pattern); err == nil && !info.IsDir() {
				return workflowInputs([]workflowInput{{Path: pattern, CanonicalPath: filepath.ToSlash(filepath.Clean(pattern))}}, false)
			}
		}
		return nil, fmt.Errorf("locate checked-out git repository: %w", rootErr)
	}
	root := strings.TrimSpace(string(rootBytes))
	pathspecs := []string{
		":(top,glob).github/workflows/*.yml",
		":(top,glob).github/workflows/*.yaml",
	}
	if !allWorkflows {
		absolutePattern, err := filepath.Abs(pattern)
		if err != nil {
			return nil, fmt.Errorf("resolve workflow pattern %q: %w", pattern, err)
		}
		relativePattern, err := filepath.Rel(root, absolutePattern)
		if err != nil || relativePattern == ".." || strings.HasPrefix(relativePattern, ".."+string(filepath.Separator)) {
			if !patternHasMeta {
				if info, statErr := os.Stat(pattern); statErr == nil && !info.IsDir() {
					return workflowInputs([]workflowInput{{Path: pattern, CanonicalPath: filepath.ToSlash(filepath.Clean(pattern))}}, false)
				}
			}
			return nil, fmt.Errorf("workflow pattern %q is outside the checked-out git repository", pattern)
		}
		pathspecMagic := ":(literal)"
		if patternHasMeta {
			pathspecMagic = ":(glob)"
		}
		pathspecs = []string{pathspecMagic + filepath.ToSlash(relativePattern)}
	}
	commandArgs := append([]string{"-C", root, "ls-files", "-z", "--"}, pathspecs...)
	command := exec.Command("git", commandArgs...)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("expand workflow pattern %q against tracked files: %w", pattern, err)
	}
	var matches []workflowInput
	for _, entry := range bytes.Split(output, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		canonical := string(entry)
		path := filepath.Join(root, filepath.FromSlash(canonical))
		if err := requireRegularWorkflowFile(path, canonical); err != nil {
			return nil, err
		}
		matches = append(matches, workflowInput{Path: path, CanonicalPath: canonical})
	}
	if len(matches) == 0 && !patternHasMeta {
		if info, statErr := os.Stat(pattern); statErr == nil && !info.IsDir() {
			return workflowInputs([]workflowInput{{Path: pattern, CanonicalPath: filepath.ToSlash(filepath.Clean(pattern))}}, false)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("workflow pattern %q matched no tracked files", pattern)
	}
	return workflowInputs(matches, patternHasMeta || len(matches) > 1)
}

func requireRegularWorkflowFile(path, displayPath string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("workflow path %q does not name a regular tracked file", displayPath)
	}
	return nil
}

func workflowInputs(matches []workflowInput, namespaceKeys bool) ([]workflowInput, error) {
	sort.Slice(matches, func(i, j int) bool { return matches[i].CanonicalPath < matches[j].CanonicalPath })
	out := matches[:0]
	identities := make(map[string]string, len(matches))
	for _, match := range matches {
		if len(out) != 0 && out[len(out)-1].CanonicalPath == match.CanonicalPath {
			continue
		}
		digest := sha256.Sum256([]byte(match.CanonicalPath))
		match.Identity = hex.EncodeToString(digest[:8])
		if other, exists := identities[match.Identity]; exists {
			return nil, fmt.Errorf("workflow identity collision between %q and %q", other, match.CanonicalPath)
		}
		identities[match.Identity] = match.CanonicalPath
		if namespaceKeys {
			match.StepKeyNamespace = match.Identity
		}
		out = append(out, match)
	}
	return out, nil
}

type hostedCompilation struct {
	Bundle     compiler.Bundle
	HasActions bool
	Admitted   bool
}

type hostedFailureKind string

const (
	hostedEnvironmentFailure hostedFailureKind = "environment"
	hostedEvaluationFailure  hostedFailureKind = "evaluation"
	hostedAdmissionFailure   hostedFailureKind = "admission"
)

type hostedFailure struct {
	Kind hostedFailureKind
	Err  error
}

func (e *hostedFailure) Error() string { return e.Err.Error() }
func (e *hostedFailure) Unwrap() error { return e.Err }

func hostedError(kind hostedFailureKind, err error) error {
	return &hostedFailure{Kind: kind, Err: err}
}

type actionSourceAuthentication struct {
	provider gharuntime.ScopedTokenProvider
	redactor gharuntime.Redactor
	warnings io.Writer
}

func jobScopedActionSourceAuthentication(warnings io.Writer) *actionSourceAuthentication {
	authentication := &actionSourceAuthentication{
		redactor: gharuntime.AgentRedactor{Executable: os.Getenv("BUILDKITE_GHA_AGENT")},
		warnings: warnings,
	}
	provider, err := gharuntime.NewAgentGitHubTokens(gharuntime.AgentGitHubTokenConfig{
		Endpoint: os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
		JobID:    os.Getenv("BUILDKITE_JOB_ID"),
		JobToken: os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
	})
	if err != nil {
		return authentication
	}
	authentication.provider = provider
	return authentication
}

func (a *actionSourceAuthentication) option(repository string) actionsource.Option {
	if a == nil {
		return nil
	}
	return actionsource.WithScopedGitHubTokenProvider(repository, func(ctx context.Context) (string, error) {
		return a.token(ctx, repository)
	})
}

func (a *actionSourceAuthentication) token(ctx context.Context, repository string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if a.provider == nil {
		a.warnAnonymousFallback("job-scoped GitHub source authentication is unavailable")
		return "", nil
	}
	token, err := a.provider.ScopedToken(ctx, repository, map[string]string{"contents": "read"})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		a.warnAnonymousFallback("could not mint a job-scoped GitHub source token")
		return "", nil
	}
	if err := a.redactor.AddRedaction(ctx, token); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		a.warnAnonymousFallback("could not register the job-scoped GitHub source token with the Buildkite Agent redactor")
		return "", nil
	}
	return token, nil
}

func (a *actionSourceAuthentication) warnAnonymousFallback(reason string) {
	if a.warnings != nil {
		_, _ = fmt.Fprintf(a.warnings, "buildkite-gha: upload: warning: %s; resolving mutable public action references anonymously\n", reason)
	}
}

func hostedOptions(groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runtimeDistributions map[compiler.Platform]string) compiler.Options {
	targets := map[string]compiler.RunnerTarget{
		"ubuntu-latest": {Platform: compiler.PlatformLinuxAMD64},
		"ubuntu-24.04":  {Platform: compiler.PlatformLinuxAMD64},
		"ubuntu-22.04":  {Platform: compiler.PlatformLinuxAMD64},
	}
	for label, target := range configuredTargets {
		targets[label] = target
	}
	options := compiler.Options{
		EventTrust:           compiler.EventUntrusted,
		GroupLabel:           groupLabel,
		RuntimeDistributions: runtimeDistributions,
		Runners: compiler.RunnerPolicy{
			Targets:                    targets,
			AllowUntrustedDefaultQueue: true,
		},
	}
	for _, target := range configuredTargets {
		if !slices.Contains(options.Runners.UntrustedQueues, target.Queue) {
			options.Runners.UntrustedQueues = append(options.Runners.UntrustedQueues, target.Queue)
		}
	}
	return options
}

func compileHosted(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep, groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runtimeDistributions map[compiler.Platform]string, actionAuthentication *actionSourceAuthentication) (hostedCompilation, error) {
	return compileHostedNamespaced(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep, groupLabel, configuredTargets, runtimeDistributions, "", actionAuthentication)
}

func compileHostedNamespaced(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep, groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runtimeDistributions map[compiler.Platform]string, stepKeyNamespace string, actionAuthentication *actionSourceAuthentication) (hostedCompilation, error) {
	options := hostedOptions(groupLabel, configuredTargets, runtimeDistributions)
	options.StepKeyNamespace = stepKeyNamespace
	preflight, err := compiler.CompileWithOptions(workflowPath, workflowSource, eventSource, options)
	if err != nil {
		return hostedCompilation{}, hostedError(hostedEvaluationFailure, err)
	}
	var ir compiler.IR
	if err := json.Unmarshal(preflight, &ir); err != nil {
		return hostedCompilation{}, hostedError(hostedEvaluationFailure, fmt.Errorf("decode compiler preflight: %w", err))
	}
	hasActions := irUsesActions(ir)
	if hasActions {
		actionRoot, err := os.MkdirTemp("", "buildkite-gha-action-source-")
		if err != nil {
			return hostedCompilation{}, hostedError(hostedEnvironmentFailure, fmt.Errorf("create action source store: %w", err))
		}
		defer func() { _ = os.RemoveAll(actionRoot) }()
		var sourceOptions []actionsource.Option
		authenticationOption := actionAuthentication.option(ir.Event.Repository.Owner + "/" + ir.Event.Repository.Name)
		if authenticationOption != nil {
			sourceOptions = append(sourceOptions, authenticationOption)
		}
		resolver, err := actionsource.NewResolver(nil, sourceOptions...)
		if err != nil {
			return hostedCompilation{}, hostedError(hostedEnvironmentFailure, fmt.Errorf("configure public action resolver: %w", err))
		}
		store, err := actionsource.NewStore(actionRoot, nil)
		if err != nil {
			return hostedCompilation{}, hostedError(hostedEnvironmentFailure, fmt.Errorf("configure public action source store: %w", err))
		}
		options.ResolveActions = true
		options.ActionSource = compiler.PublicActionSource{Resolver: resolver, Store: store}
	}
	bundle, err := compiler.CompileBundlePlansContext(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, options)
	if err != nil {
		return hostedCompilation{Bundle: bundle, HasActions: hasActions}, hostedError(hostedEvaluationFailure, err)
	}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		return hostedCompilation{Bundle: bundle, HasActions: hasActions}, hostedError(hostedAdmissionFailure, err)
	}
	bundle, err = compiler.GenerateBundlePipeline(bundle, distributionDigest, importerStep, options)
	if err != nil {
		return hostedCompilation{Bundle: bundle, HasActions: hasActions, Admitted: true}, hostedError(hostedEvaluationFailure, err)
	}
	if !hasActions && bundleUsesActions(bundle) {
		return hostedCompilation{Bundle: bundle, HasActions: hasActions, Admitted: true}, hostedError(hostedEvaluationFailure, fmt.Errorf("final compilation introduced actions absent from preflight"))
	}
	return hostedCompilation{Bundle: bundle, HasActions: hasActions, Admitted: true}, nil
}

func validateUnprivilegedBundle(bundle compiler.Bundle) error {
	instances := make(map[string]compiler.JobInstance, len(bundle.IR.Jobs))
	for _, instance := range bundle.IR.Jobs {
		instances[instance.Key] = instance
	}
	var diagnostics []error
	addFailure := func(artifact compiler.PlanArtifact, message string, err error) {
		finding := &compiler.ProcessingFinding{
			Stage: compiler.StageAdmission, Code: "E_PROFILE", Category: "admission",
			Job: artifact.Job.Workflow.LogicalJobID, Instance: artifact.Job.Target.StepKey,
			Message: message, Err: err,
		}
		if instance, ok := instances[artifact.Job.Target.StepKey]; ok {
			finding.Path = instance.SourcePath
			finding.Line = instance.Source.Start.Line
			finding.Column = instance.Source.Start.Column
		}
		diagnostics = append(diagnostics, finding)
	}
	for _, artifact := range bundle.Plans {
		for _, capability := range artifact.Job.RequiredCapabilities {
			if capability == "docker" && !slices.Equal(artifact.Authorization.DockerCapabilitySources, []string{"dockerfile-actions"}) {
				if slices.Contains(artifact.Authorization.DockerCapabilitySources, "job-containers") || slices.Contains(artifact.Authorization.DockerCapabilitySources, "service-containers") {
					addFailure(artifact, "job or service containers are not admitted by the hosted profile", fmt.Errorf("job %q uses job or service containers, which hosted upload does not admit", artifact.Job.Workflow.LogicalJobID))
					continue
				}
				addFailure(artifact, "docker capability lacks compiler-verified action provenance", fmt.Errorf("job %q requires docker without compiler-verified Dockerfile action provenance", artifact.Job.Workflow.LogicalJobID))
				continue
			}
			if capability == "provider-token-read" {
				if !slices.Equal(artifact.Authorization.ProviderTokenReadCapabilitySources, []string{"checkout-adapter"}) {
					addFailure(artifact, "provider token read capability lacks compiler-verified checkout provenance", fmt.Errorf("job %q requires provider-token-read without compiler-verified checkout provenance", artifact.Job.Workflow.LogicalJobID))
				}
				continue
			}
			if capability == "provider-token-write" {
				filename, filenameErr := plan.GitHubWorkflowPolicyFilename(artifact.Job.Workflow.Path)
				if artifact.Job.GitHubToken == nil || !slices.Equal(artifact.Authorization.ProviderTokenWriteCapabilitySources, []string{"effective-permissions"}) || filenameErr != nil || artifact.Authorization.WorkflowTokenPolicyFilename == "" || artifact.Authorization.WorkflowTokenPolicyFilename != filename {
					reason := bundle.IR.Workflow.WorkflowTokenPolicyDiagnostic
					if reason == "" {
						reason = "compiler-verified workflow policy evidence is missing or does not match the job plan"
					}
					addFailure(artifact, "provider token write capability lacks compiler-verified workflow policy", fmt.Errorf("job %q requires provider-token-write without a compiler-verified workflow policy: %s", artifact.Job.Workflow.LogicalJobID, reason))
				}
				continue
			}
			if capability != "network" && capability != "docker" {
				addFailure(artifact, fmt.Sprintf("capability %q is unavailable to the hosted profile", capability), fmt.Errorf("job %q requires capability %q, unavailable to unprivileged upload", artifact.Job.Workflow.LogicalJobID, capability))
			}
		}
		for _, action := range artifact.Job.Actions {
			descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: action.Source, Repository: action.Repository, Path: action.Path})
			if descriptor.Service == actionintegration.ServiceCache {
				if err := actionintegration.ValidateCacheCommit(action.Commit); err != nil {
					addFailure(artifact, "cache action version is not admitted by the hosted profile", fmt.Errorf("job %q uses unsupported cache action: %w", artifact.Job.Workflow.LogicalJobID, err))
				}
				continue
			}
			if descriptor.Service != "" {
				addFailure(artifact, "action requires a service unavailable to the hosted profile", fmt.Errorf("job %q uses action %q, which requires the unavailable GitHub Actions %s service", artifact.Job.Workflow.LogicalJobID, action.Repository, descriptor.Service))
			}
		}
	}
	return errors.Join(diagnostics...)
}

func irUsesActions(ir compiler.IR) bool {
	for _, job := range ir.Jobs {
		for _, step := range job.Steps {
			if step.Uses != "" {
				return true
			}
		}
	}
	return false
}

func bundleUsesActions(bundle compiler.Bundle) bool {
	for _, artifact := range bundle.Plans {
		if len(artifact.Job.Actions) != 0 {
			return true
		}
		for _, step := range artifact.Job.Steps {
			if step.Uses != "" || step.Action != nil || step.Kind == "uses" {
				return true
			}
		}
	}
	return false
}

type parsedUploadArgs struct {
	workflowOperands         []string
	explicitWorkflowPaths    bool
	eventPath                string
	runtimeDistributionPaths map[compiler.Platform]string
	runnerTargets            map[string]compiler.RunnerTarget
	pluginAcquisition        *pluginRuntimeAcquisition
}

func uploadArgs(args []string) (workflowOperands []string, eventPath string, err error) {
	parsed, err := parseUploadArgs(args)
	return parsed.workflowOperands, parsed.eventPath, err
}

func parseUploadArgs(args []string) (parsedUploadArgs, error) {
	workflowOperands := make([]string, 0, len(args))
	runtimeDistributionPaths := make(map[compiler.Platform]string)
	runnerQueues := make(map[string]string)
	runnerImages := make(map[string]string)
	eventPath := ""
	eventPathSeen := false
	runtimeQueue := ""
	runtimeQueueSeen := false
	deprecatedPrivateCheckoutSeen := false
	optionsEnded := false
	for i := 0; i < len(args); i++ {
		if optionsEnded {
			workflowOperands = append(workflowOperands, args[i])
			continue
		}
		if args[i] == "--" {
			optionsEnded = true
			continue
		}
		if args[i] == "--event-path" {
			if eventPathSeen {
				return parsedUploadArgs{}, fmt.Errorf("--event-path may only be specified once")
			}
			eventPathSeen = true
			i++
			if i == len(args) {
				return parsedUploadArgs{}, fmt.Errorf("--event-path requires a path")
			}
			eventPath = args[i]
			continue
		}
		if args[i] == "--private-checkout" {
			if deprecatedPrivateCheckoutSeen {
				return parsedUploadArgs{}, fmt.Errorf("--private-checkout may only be specified once")
			}
			deprecatedPrivateCheckoutSeen = true
			continue
		}
		if args[i] == "--runtime-distribution" {
			i++
			if i == len(args) {
				return parsedUploadArgs{}, fmt.Errorf("--runtime-distribution requires platform=absolute-path")
			}
			platformValue, path, ok := strings.Cut(args[i], "=")
			if !ok || path == "" {
				return parsedUploadArgs{}, fmt.Errorf("--runtime-distribution requires platform=absolute-path")
			}
			platform, err := compiler.ParsePlatform(platformValue)
			if err != nil {
				return parsedUploadArgs{}, err
			}
			if !filepath.IsAbs(path) {
				return parsedUploadArgs{}, fmt.Errorf("runtime distribution for %s must use an absolute path", platform)
			}
			if _, exists := runtimeDistributionPaths[platform]; exists {
				return parsedUploadArgs{}, fmt.Errorf("runtime distribution for %s may only be specified once", platform)
			}
			runtimeDistributionPaths[platform] = path
			continue
		}
		if args[i] == "--runner-queue" || args[i] == "--runner-image" {
			option := args[i]
			i++
			if i == len(args) {
				return parsedUploadArgs{}, fmt.Errorf("%s requires runs-on=value", option)
			}
			label, value, ok := strings.Cut(args[i], "=")
			if !ok || label == "" || value == "" {
				return parsedUploadArgs{}, fmt.Errorf("%s requires runs-on=value", option)
			}
			canonical, _, err := supportedRunnerTarget(label)
			if err != nil {
				return parsedUploadArgs{}, err
			}
			values := runnerQueues
			if option == "--runner-image" {
				values = runnerImages
			}
			if _, duplicate := values[canonical]; duplicate {
				return parsedUploadArgs{}, fmt.Errorf("%s for %q may only be specified once", option, canonical)
			}
			values[canonical] = value
			continue
		}
		if args[i] == "-h" || args[i] == "--help" {
			return parsedUploadArgs{}, fmt.Errorf("help must be requested immediately after the command")
		}
		if args[i] != "--runtime-queue" {
			if strings.HasPrefix(args[i], "-") {
				return parsedUploadArgs{}, fmt.Errorf("unknown option %q", args[i])
			}
			workflowOperands = append(workflowOperands, args[i])
			continue
		}
		if runtimeQueueSeen {
			return parsedUploadArgs{}, fmt.Errorf("--runtime-queue may only be specified once")
		}
		runtimeQueueSeen = true
		i++
		if i == len(args) {
			return parsedUploadArgs{}, fmt.Errorf("--runtime-queue requires a queue")
		}
		runtimeQueue = args[i]
	}
	if len(workflowOperands) == 0 {
		return parsedUploadArgs{}, fmt.Errorf("workflow path is required")
	}
	if runtimeQueueSeen && runtimeQueue != legacyRuntimeQueue {
		return parsedUploadArgs{}, fmt.Errorf("deprecated --runtime-queue must be %q", legacyRuntimeQueue)
	}
	runnerTargets := make(map[string]compiler.RunnerTarget, len(runnerQueues))
	for label, queue := range runnerQueues {
		image := runnerImages[label]
		canonical, target, err := configuredRunnerTarget(label, queue, image)
		if err != nil {
			return parsedUploadArgs{}, err
		}
		runnerTargets[canonical] = target
	}
	for label := range runnerImages {
		if _, ok := runnerQueues[label]; !ok {
			return parsedUploadArgs{}, fmt.Errorf("--runner-image for %q requires --runner-queue", label)
		}
	}
	return parsedUploadArgs{workflowOperands: workflowOperands, eventPath: eventPath, runtimeDistributionPaths: runtimeDistributionPaths, runnerTargets: runnerTargets}, nil
}

func configuredRunnerTarget(label, queue, image string) (string, compiler.RunnerTarget, error) {
	canonical, platform, err := supportedRunnerTarget(label)
	if err != nil {
		return "", compiler.RunnerTarget{}, err
	}
	if !runnerQueuePattern.MatchString(queue) {
		return "", compiler.RunnerTarget{}, fmt.Errorf("runner queue for %q is invalid", canonical)
	}
	if image != "" && !runnerImagePattern.MatchString(image) {
		return "", compiler.RunnerTarget{}, fmt.Errorf("runner image for %q must be an immutable registry sha256 reference", canonical)
	}
	if image != "" && platform == compiler.PlatformDarwinARM64 {
		return "", compiler.RunnerTarget{}, fmt.Errorf("runner image for %q is unsupported on darwin/arm64", canonical)
	}
	if image == "" {
		switch canonical {
		case "ubuntu-latest", "ubuntu-24.04":
			image = defaultNobleRunnerImage
		case "ubuntu-22.04":
			image = defaultJammyRunnerImage
		}
	}
	return canonical, compiler.RunnerTarget{Queue: queue, Platform: platform, Image: image}, nil
}

func supportedRunnerTarget(label string) (string, compiler.Platform, error) {
	if label != strings.TrimSpace(label) || strings.ContainsAny(label, "\r\n") {
		return "", compiler.Platform{}, fmt.Errorf("unsupported runner label %q", label)
	}
	canonical := strings.ToLower(label)
	switch canonical {
	case "ubuntu-latest", "ubuntu-24.04", "ubuntu-22.04":
		return canonical, compiler.PlatformLinuxAMD64, nil
	case "macos-latest", "macos-15", "macos-14":
		return canonical, compiler.PlatformDarwinARM64, nil
	default:
		return "", compiler.Platform{}, fmt.Errorf("unsupported runner label %q", label)
	}
}

type runtimeDistribution struct {
	contents []byte
	digest   string
}

type pluginRuntimeAcquisition struct {
	version string
}

const pluginDarwinAsset = "buildkite-gha_Darwin_arm64.tar.gz"

func securePluginHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("release download redirected away from HTTPS")
			}
			if len(via) >= 10 {
				return errors.New("too many release download redirects")
			}
			return nil
		},
	}
}

func (a *pluginRuntimeAcquisition) acquire(ctx context.Context, required map[compiler.Platform]bool, linux runtimeDistribution) (map[compiler.Platform]runtimeDistribution, error) {
	distributions := map[compiler.Platform]runtimeDistribution{}
	if required[compiler.PlatformLinuxAMD64] {
		distributions[compiler.PlatformLinuxAMD64] = linux
	}
	injected := os.Getenv(pluginDevDarwinRuntimeEnvironment)
	if a.version != "dev" && injected != "" {
		return nil, fmt.Errorf("%s is test/dev-only and is rejected by release builds", pluginDevDarwinRuntimeEnvironment)
	}
	if !required[compiler.PlatformDarwinARM64] {
		return distributions, nil
	}
	if a.version == "dev" {
		if injected == "" {
			return nil, fmt.Errorf("%s is required for a dev build using darwin/arm64", pluginDevDarwinRuntimeEnvironment)
		}
		loaded, err := loadRuntimeDistributions(map[compiler.Platform]string{compiler.PlatformDarwinARM64: injected})
		if err != nil {
			return nil, err
		}
		distributions[compiler.PlatformDarwinARM64] = loaded[compiler.PlatformDarwinARM64]
		return distributions, nil
	}
	if !stableVersionPattern.MatchString(a.version) {
		return nil, fmt.Errorf("running version %q is not a stable semantic version", a.version)
	}
	darwin, err := acquirePluginDarwin(ctx, a.version, pluginHTTPClient, pluginReleaseBaseURL, pluginArchiveCachePath(a.version))
	if err != nil {
		return nil, err
	}
	distributions[compiler.PlatformDarwinARM64] = darwin
	return distributions, nil
}

func acquirePluginDarwin(ctx context.Context, version string, client *http.Client, baseURL, cachePath string) (runtimeDistribution, error) {
	checksums, err := downloadPluginReleaseFile(ctx, client, baseURL+"/v"+version+"/checksums.txt", pluginChecksumLimit)
	if err != nil {
		return runtimeDistribution{}, fmt.Errorf("download release checksums: %w", err)
	}
	expected, err := pluginAssetChecksum(checksums, pluginDarwinAsset)
	if err != nil {
		return runtimeDistribution{}, err
	}
	archive := readVerifiedPluginArchiveCache(cachePath, expected)
	if archive == nil {
		archive, err = downloadPluginReleaseFile(ctx, client, baseURL+"/v"+version+"/"+pluginDarwinAsset, pluginArchiveLimit)
		if err != nil {
			return runtimeDistribution{}, fmt.Errorf("download Darwin runtime: %w", err)
		}
		if sha256.Sum256(archive) != expected {
			return runtimeDistribution{}, fmt.Errorf("darwin archive checksum verification failed")
		}
		writePluginArchiveCache(cachePath, archive)
	}
	contents, err := extractPluginDarwin(archive)
	if err != nil {
		return runtimeDistribution{}, err
	}
	if err := validateRuntimeDistributionBinary(compiler.PlatformDarwinARM64, contents); err != nil {
		return runtimeDistribution{}, fmt.Errorf("validate Darwin runtime: %w", err)
	}
	digest := sha256.Sum256(contents)
	return runtimeDistribution{contents: contents, digest: fmt.Sprintf("sha256:%x", digest)}, nil
}

func downloadPluginReleaseFile(ctx context.Context, client *http.Client, source string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil || request.URL.Scheme != "https" {
		return nil, fmt.Errorf("release URL must use HTTPS")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("exceeds %d-byte limit", limit)
	}
	return contents, nil
}

func pluginAssetChecksum(contents []byte, asset string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	matches := 0
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return result, fmt.Errorf("invalid checksum entry for %s", asset)
		}
		copy(result[:], decoded)
		matches++
	}
	if matches != 1 {
		return result, fmt.Errorf("checksums must contain exactly one valid entry for %s", asset)
	}
	return result, nil
}

func extractPluginDarwin(archive []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open Darwin archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	seenBinary, seenLicense := false, false
	var executable []byte
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read Darwin archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > runtimeDistributionLimit {
			return nil, fmt.Errorf("darwin archive contains an unsafe member %q", header.Name)
		}
		switch header.Name {
		case "buildkite-gha":
			if seenBinary || header.Mode&0o111 == 0 {
				return nil, fmt.Errorf("darwin archive has an invalid executable member")
			}
			seenBinary = true
			executable, err = io.ReadAll(io.LimitReader(tarReader, runtimeDistributionLimit+1))
			if err != nil || int64(len(executable)) != header.Size {
				return nil, fmt.Errorf("read Darwin executable")
			}
		case "LICENSE":
			if seenLicense {
				return nil, fmt.Errorf("darwin archive contains duplicate LICENSE")
			}
			seenLicense = true
		default:
			return nil, fmt.Errorf("darwin archive contains unexpected member %q", header.Name)
		}
	}
	if !seenBinary || !seenLicense {
		return nil, fmt.Errorf("darwin archive must contain exactly buildkite-gha and LICENSE")
	}
	return executable, nil
}

func pluginArchiveCachePath(version string) string {
	root := os.Getenv("MISE_DATA_DIR")
	if !filepath.IsAbs(root) {
		return ""
	}
	return filepath.Join(root, "buildkite-gha", "releases", version, pluginDarwinAsset)
}

func readVerifiedPluginArchiveCache(path string, expected [sha256.Size]byte) []byte {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > pluginArchiveLimit {
		return nil
	}
	contents, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(contents) != expected {
		return nil
	}
	return contents
}

func writePluginArchiveCache(path string, contents []byte) {
	if path == "" {
		return
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return
	}
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil || realDirectory != directory {
		return
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	temporary, err := os.CreateTemp(directory, ".archive-")
	if err != nil {
		return
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = temporary.Write(contents); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		_ = temporary.Close()
		return
	}
	_ = os.Rename(name, path)
}

func loadRuntimeDistributions(paths map[compiler.Platform]string) (map[compiler.Platform]runtimeDistribution, error) {
	distributions := make(map[compiler.Platform]runtimeDistribution, len(paths))
	for platform, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("runtime distribution for %s must use an absolute path", platform)
		}
		pathInfo, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect runtime distribution for %s: %w", platform, err)
		}
		if pathInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("runtime distribution for %s is not a non-symlink executable regular file", platform)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open runtime distribution for %s: %w", platform, err)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("inspect opened runtime distribution for %s: %w", platform, err)
		}
		if !os.SameFile(pathInfo, info) {
			_ = file.Close()
			return nil, fmt.Errorf("runtime distribution for %s changed while being opened", platform)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			_ = file.Close()
			return nil, fmt.Errorf("runtime distribution for %s is not a non-symlink executable regular file", platform)
		}
		if info.Size() <= 0 || info.Size() > runtimeDistributionLimit {
			_ = file.Close()
			return nil, fmt.Errorf("runtime distribution for %s must be between 1 and %d bytes", platform, runtimeDistributionLimit)
		}
		contents, err := io.ReadAll(io.LimitReader(file, runtimeDistributionLimit+1))
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("read runtime distribution for %s: %w", platform, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close runtime distribution for %s: %w", platform, closeErr)
		}
		if int64(len(contents)) != info.Size() {
			return nil, fmt.Errorf("runtime distribution for %s changed while being read", platform)
		}
		if err := validateRuntimeDistributionBinary(platform, contents); err != nil {
			return nil, fmt.Errorf("validate runtime distribution for %s: %w", platform, err)
		}
		sum := sha256.Sum256(contents)
		distributions[platform] = runtimeDistribution{contents: contents, digest: fmt.Sprintf("sha256:%x", sum)}
	}
	return distributions, nil
}

func validateRuntimeDistributionBinary(platform compiler.Platform, contents []byte) error {
	switch platform {
	case compiler.PlatformLinuxAMD64:
		binary, err := elf.NewFile(bytes.NewReader(contents))
		if err != nil {
			return fmt.Errorf("open ELF executable: %w", err)
		}
		defer func() { _ = binary.Close() }()
		if binary.Class != elf.ELFCLASS64 || binary.Machine != elf.EM_X86_64 || binary.Type != elf.ET_EXEC {
			return fmt.Errorf("want a thin 64-bit linux/amd64 executable")
		}
	case compiler.PlatformDarwinARM64:
		binary, err := macho.NewFile(bytes.NewReader(contents))
		if err != nil {
			return fmt.Errorf("open Mach-O executable: %w", err)
		}
		defer func() { _ = binary.Close() }()
		if binary.Cpu != macho.CpuArm64 || binary.Type != macho.TypeExec {
			return fmt.Errorf("want a thin darwin/arm64 executable")
		}
	default:
		return fmt.Errorf("unsupported platform")
	}
	return nil
}

func compileArgs(args []string) (workflowPath, eventPath, format string, err error) {
	format = "pipeline"
	filtered := make([]string, 0, len(args))
	formatSeen := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--format" {
			filtered = append(filtered, args[i])
			continue
		}
		if formatSeen {
			return "", "", "", fmt.Errorf("--format may only be specified once")
		}
		formatSeen = true
		i++
		if i == len(args) {
			return "", "", "", fmt.Errorf("--format requires pipeline or ir-json")
		}
		format = args[i]
		if format != "pipeline" && format != "ir-json" {
			return "", "", "", fmt.Errorf("--format must be pipeline or ir-json")
		}
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	return workflowPath, eventPath, format, err
}

func executableDigest() (string, error) {
	_, _, digest, err := executable()
	return digest, err
}

func executable() (path string, contents []byte, digest string, err error) {
	path, err = os.Executable()
	if err != nil {
		return "", nil, "", fmt.Errorf("locate compiler executable: %w", err)
	}
	readPath := path
	if runtime.GOOS == "linux" {
		// /proc/self/exe remains bound to the running inode if the launch path is
		// replaced. The plugin importer relies on these exact bytes for Linux jobs.
		readPath = "/proc/self/exe"
	}
	file, err := os.Open(readPath)
	if err != nil {
		return "", nil, "", fmt.Errorf("open running compiler executable: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return "", nil, "", fmt.Errorf("inspect running compiler executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Size() <= 0 || info.Size() > runtimeDistributionLimit {
		_ = file.Close()
		return "", nil, "", fmt.Errorf("running compiler executable must be an executable regular file between 1 and %d bytes", runtimeDistributionLimit)
	}
	contents, err = io.ReadAll(io.LimitReader(file, runtimeDistributionLimit+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return "", nil, "", fmt.Errorf("read running compiler executable: %w", errors.Join(err, closeErr))
	}
	if int64(len(contents)) != info.Size() {
		return "", nil, "", fmt.Errorf("running compiler executable changed while being read")
	}
	platform, err := compiler.ParsePlatform(runtime.GOOS + "/" + runtime.GOARCH)
	if err != nil {
		return "", nil, "", fmt.Errorf("validate running compiler executable: %w", err)
	}
	if err := validateRuntimeDistributionBinary(platform, contents); err != nil {
		return "", nil, "", fmt.Errorf("validate running compiler executable: %w", err)
	}
	sum := sha256.Sum256(contents)
	return path, contents, fmt.Sprintf("sha256:%x", sum), nil
}

func workflowArgs(args []string) (workflowPath, eventPath string, err error) {
	eventPathSeen := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--event-path":
			if eventPathSeen {
				return "", "", fmt.Errorf("--event-path may only be specified once")
			}
			eventPathSeen = true
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
	_, _ = fmt.Fprintf(stderr, "buildkite-gha: "+format+"\n\n", args...)
	_, _ = fmt.Fprint(stderr, usage)
	return 2
}
