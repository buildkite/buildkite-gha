// Package cli implements the buildkite-gha command-line interface.
package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
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
	"runtime"
	"slices"
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
	"validate": "Usage: buildkite-gha validate [--event-path <path>] [--profile hosted-tokenless] [--format text|json] <workflow>\n",
	"compile":  "Usage: buildkite-gha compile --event-path <path> [--format pipeline|ir-json] <workflow>\n",
	"upload":   "Usage: buildkite-gha upload [--event-path <path>] [--runtime-queue hosted] <workflow>\n",
	"run-job":  "Usage: buildkite-gha run-job --plan <path> [--result <path>]\n",
}

func writeCommandHelp(stdout io.Writer, command string) {
	_, _ = fmt.Fprint(stdout, commandUsage[command])
	switch command {
	case "validate":
		_, _ = fmt.Fprint(stdout, "\nThe hosted-tokenless profile resolves actions and applies production upload policy without executing jobs or proving arbitrary action runtime compatibility.\n")
	case "compile":
		_, _ = fmt.Fprint(stdout, "\nPipeline output references content-addressed plans; compile does not materialize or upload those artifacts.\n")
	case "upload":
		_, _ = fmt.Fprintf(stdout, "\nGenerated jobs use Buildkite's default agent targeting. An importer can explicitly target one queue with %s; that queue must be suitable for untrusted workflow code. The deprecated --runtime-queue hosted option is accepted for plugin compatibility but does not select a queue. Event precedence is an explicit event file, Buildkite's reserved webhook metadata, then reduced-fidelity Buildkite environment compatibility data; every source remains unsigned. Verified checkout jobs automatically use Buildkite repository-provider Git credentials when the job enables them; the deprecated --private-checkout option is accepted as a no-op.\n", targetQueueEnvironment)
	}
}

const (
	resultPublicationTimeout                    = 10 * time.Second
	legacyRuntimeQueue                          = "hosted"
	targetQueueEnvironment                      = "BUILDKITE_GHA_TARGET_QUEUE"
	repositoryProviderGitCredentialsEnvironment = "BUILDKITE_USE_REPOSITORY_PROVIDER_GIT_CREDENTIALS"
	legacyGitHubAppGitCredentialsEnvironment    = "BUILDKITE_USE_GITHUB_APP_GIT_CREDENTIALS"
	hostedTokenlessProfile                      = "hosted-tokenless"
	runtimeMiseArchiveDigest                    = "bd0930c0b619f51ddb60e32e5cce18a5533567b2f1ba9fc4875b9f39a2bb3ed8"
	runtimeMiseBinaryDigest                     = "a238972a3162d710b85b28c324372e96ca4e4b486c81fe78695000d9fbc77c48"
	runtimeMiseArchiveLimit                     = 64 << 20
	runtimeMiseBinaryLimit                      = 128 << 20
	maxWebhookMetadataBytes                     = 25 << 20
)

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
	default:
		if _, ok := commandUsage[args[0]]; ok {
			if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
				writeCommandHelp(stdout, args[0])
				return 0
			}
			switch args[0] {
			case "validate":
				return validate(args[1:], stdout, stderr, version)
			case "compile":
				return compile(args[1:], stdout, stderr, version)
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
	planPath, resultPath, err := runJobArgs(args)
	if err != nil {
		return usageError(stderr, "run-job: %v", err)
	}
	source, err := os.ReadFile(planPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	planDigest := transport.Digest(source)
	if expected := os.Getenv("BUILDKITE_GHA_PLAN_DIGEST"); expected != "" {
		if planDigest != expected {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: plan digest %q does not match expected digest %q\n", planDigest, expected)
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
	if err := verifyBuildkiteTarget(job); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	producer, publish, err := resultProducer(job, planDigest)
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
	if (job.Schema == plan.SchemaV3 || job.Schema == plan.SchemaV4 || job.Schema == plan.SchemaV5 || job.Schema == plan.SchemaV6 || job.Schema == plan.SchemaV7) && hasGitHubActionLocks(job.Actions) {
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
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: configure actions/cache v6 service: %v\n", err)
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
	runner := gharuntime.Runner{
		Stdout:                stdout,
		Stderr:                stderr,
		MiseDataDir:           prepareMiseDataDir(os.Getenv("BUILDKITE_GHA_MISE_DATA_DIR"), stderr),
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
	if resultPath != "" && result.Conclusion != "" {
		if err := writeJobResult(resultPath, result); err != nil {
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

func installRuntimeMise(ctx context.Context, dataDir, privateRuntime string, stderr io.Writer) (string, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("managed mise is unavailable on %s/%s; set BUILDKITE_GHA_MISE to a compatible absolute path", runtime.GOOS, runtime.GOARCH)
	}
	root := dataDir
	if root == "" {
		var err error
		root, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve mise runtime cache: %w", err)
		}
		root = filepath.Join(root, "buildkite-gha", "mise", buildkitepipeline.MinimumMiseVersion)
	} else {
		root = filepath.Join(filepath.Dir(root), "runtime", buildkitepipeline.MinimumMiseVersion)
	}
	destination := filepath.Join(root, "linux-x64", "mise")
	if resolved, err := validateRuntimeMiseFile(ctx, destination, runtimeMiseBinaryDigest); err == nil {
		return pinRuntimeMise(ctx, resolved, privateRuntime, runtimeMiseBinaryDigest)
	}
	_, _ = fmt.Fprintf(stderr, "~~~ :mise: Install mise %s\n", buildkitepipeline.MinimumMiseVersion)
	url := fmt.Sprintf("https://github.com/jdx/mise/releases/download/v%s/mise-v%s-linux-x64.tar.gz", buildkitepipeline.MinimumMiseVersion, buildkitepipeline.MinimumMiseVersion)
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
	cached, err := installRuntimeMiseFrom(ctx, root, client, url, runtimeMiseArchiveDigest, runtimeMiseBinaryDigest)
	if err != nil {
		return "", err
	}
	return pinRuntimeMise(ctx, cached, privateRuntime, runtimeMiseBinaryDigest)
}

func pinRuntimeMise(ctx context.Context, cached, privateRuntime, expectedDigest string) (string, error) {
	if privateRuntime == "" {
		return "", fmt.Errorf("private action runtime directory is required")
	}
	resolvedRoot, err := filepath.EvalSymlinks(privateRuntime)
	if err != nil || resolvedRoot != privateRuntime {
		return "", fmt.Errorf("private action runtime directory contains a symlink")
	}
	source, err := os.Open(cached)
	if err != nil {
		return "", fmt.Errorf("open cached mise executable: %w", err)
	}
	defer func() { _ = source.Close() }()
	destination := filepath.Join(privateRuntime, "mise")
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
	destinationDir := filepath.Join(root, "linux-x64")
	destination := filepath.Join(destinationDir, "mise")
	if resolved, err := validateRuntimeMiseFile(ctx, destination, binaryDigest); err == nil {
		return resolved, nil
	}
	parent := filepath.Dir(destinationDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create mise runtime cache: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return "", fmt.Errorf("mise runtime cache contains a symlink")
	}
	staging, err := os.MkdirTemp(parent, ".linux-x64.")
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
	resolved, resolveErr := filepath.EvalSymlinks(absolute)
	info, statErr := os.Lstat(absolute)
	if resolveErr != nil || resolved != absolute || statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: mise cache %q is not a real directory; using the ephemeral agent cache\n", path)
		return ""
	}
	return absolute
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

func resultProducer(job plan.Job, planDigest string) (transport.Producer, bool, error) {
	if os.Getenv("BUILDKITE") == "" {
		if len(job.NeedSources) != 0 {
			return transport.Producer{}, false, fmt.Errorf("plans with prerequisites require Buildkite result identity")
		}
		return transport.Producer{}, false, nil
	}
	expectedDigest := os.Getenv("BUILDKITE_GHA_PLAN_DIGEST")
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

func runJobArgs(args []string) (planPath, resultPath string, err error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--plan", "--result":
			option := args[i]
			if seen[option] {
				return "", "", fmt.Errorf("%s may only be specified once", option)
			}
			seen[option] = true
			i++
			if i == len(args) {
				return "", "", fmt.Errorf("%s requires a path", option)
			}
			if option == "--plan" {
				planPath = args[i]
			} else {
				resultPath = args[i]
			}
		default:
			return "", "", fmt.Errorf("unknown option %q", args[i])
		}
	}
	if planPath == "" {
		return "", "", fmt.Errorf("--plan is required")
	}
	return planPath, resultPath, nil
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

func validate(args []string, stdout, stderr io.Writer, version string) int {
	workflowPath, eventPath, format, profile, err := validateArgs(args)
	if err != nil {
		return usageError(stderr, "validate: %v", err)
	}
	if profile != "" && eventPath == "" {
		return usageError(stderr, "validate: --event-path is required with --profile")
	}
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
		return 1
	}
	var event []byte
	if eventPath != "" {
		event, err = os.ReadFile(eventPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
			return 1
		}
	}

	var report compiler.Report
	if eventPath == "" {
		report, err = compiler.Validate(workflowPath, source)
	} else {
		report, err = compiler.ValidateEvent(workflowPath, source, event)
	}
	if err != nil {
		if profile == "" {
			if writeErr := compatibility.Write(stdout, format, compatibility.Blocked(workflowPath, err)); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write report: %v\n", writeErr)
			}
		} else if writeErr := compatibility.WriteProfile(stdout, format, compatibility.ProfileCompileBlocked(workflowPath, profile, err)); writeErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
		}
		return 1
	}
	if profile != "" {
		_, _, distributionDigest, executableErr := executable()
		if executableErr != nil {
			profileReport := compatibility.ProfileNotEvaluated(workflowPath, profile, report.LogicalJobs, report.Instances, "E_ENVIRONMENT", executableErr)
			if writeErr := compatibility.WriteProfile(stdout, format, withCompilerWarnings(profileReport, workflowPath, report.Warnings)); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
			}
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		preflight, profileErr := compileHostedTokenless(ctx, workflowPath, source, event, version, distributionDigest, "buildkite-gha-profile-importer", "", "", nil)
		if profileErr != nil {
			if ctx.Err() != nil || errors.Is(profileErr, context.Canceled) {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: profile evaluation interrupted: %v\n", profileErr)
				return 1
			}
			var failure *hostedTokenlessFailure
			if errors.As(profileErr, &failure) && failure.Kind == hostedTokenlessAdmissionFailure {
				profileReport := compatibility.ProfileBlocked(workflowPath, profile, report.LogicalJobs, report.Instances, profileErr)
				if writeErr := compatibility.WriteProfile(stdout, format, withCompilerWarnings(profileReport, workflowPath, report.Warnings)); writeErr != nil {
					_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
				}
				return 1
			}
			code := "E_PROFILE_EVALUATION"
			if errors.As(profileErr, &failure) && failure.Kind == hostedTokenlessEnvironmentFailure {
				code = "E_ENVIRONMENT"
			}
			profileReport := compatibility.ProfileNotEvaluated(workflowPath, profile, report.LogicalJobs, report.Instances, code, profileErr)
			if writeErr := compatibility.WriteProfile(stdout, format, withCompilerWarnings(profileReport, workflowPath, report.Warnings)); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
			}
			return 1
		}
		profileReport := compatibility.Admitted(workflowPath, profile, report.LogicalJobs, report.Instances, preflight.HasActions)
		if writeErr := compatibility.WriteProfile(stdout, format, withCompilerWarnings(profileReport, workflowPath, report.Warnings)); writeErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
			return 1
		}
		return 0
	}
	compatibilityReport := compatibility.Compilable(workflowPath, report.LogicalJobs, report.Instances)
	compatibilityReport.Diagnostics = append(compatibilityReport.Diagnostics, compilerWarningDiagnostics(workflowPath, report.Warnings)...)
	if err := compatibility.Write(stdout, format, compatibilityReport); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write report: %v\n", err)
		return 1
	}
	return 0
}

func withCompilerWarnings(report compatibility.ProfileReport, path string, warnings []compiler.Warning) compatibility.ProfileReport {
	report.Diagnostics = append(compilerWarningDiagnostics(path, warnings), report.Diagnostics...)
	return report
}

func compilerWarningDiagnostics(path string, warnings []compiler.Warning) []compatibility.Diagnostic {
	diagnostics := make([]compatibility.Diagnostic, len(warnings))
	for i, warning := range warnings {
		diagnostics[i] = compatibility.Diagnostic{
			Level:   "warning",
			Code:    warning.Code,
			Message: fmt.Sprintf("%s:%d:%d: %s", path, warning.Line, warning.Column, warning.Message),
		}
	}
	return diagnostics
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
			if profile != hostedTokenlessProfile {
				return "", "", "", "", fmt.Errorf("--profile must be %q", hostedTokenlessProfile)
			}
		}
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	return workflowPath, eventPath, format, profile, err
}

func compile(args []string, stdout, stderr io.Writer, version string) int {
	workflowPath, eventPath, format, err := compileArgs(args)
	if err != nil {
		return usageError(stderr, "compile: %v", err)
	}
	if eventPath == "" {
		return usageError(stderr, "compile: --event-path is required")
	}
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
	event, err := os.ReadFile(eventPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
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
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", digestErr)
			return 1
		}
		bundle, compileErr := compiler.CompileBundle(workflowPath, source, event, version, digest, "gha-importer")
		err = compileErr
		result = bundle.Pipeline
		warnings = bundle.IR.Warnings
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
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
	workflowPath, eventPath, err := uploadArgs(args)
	if err != nil {
		return usageError(stderr, "upload: %v", err)
	}
	targetQueue, targetQueueConfigured := os.LookupEnv(targetQueueEnvironment)
	if targetQueueConfigured && targetQueue == "" {
		return usageError(stderr, "upload: %s must name a non-empty queue when set", targetQueueEnvironment)
	}
	importerStep := os.Getenv("BUILDKITE_STEP_KEY")
	if os.Getenv("BUILDKITE") != "true" || strings.TrimSpace(importerStep) == "" {
		return usageError(stderr, "upload: BUILDKITE=true and BUILDKITE_STEP_KEY are required")
	}
	workflowSource, err := os.ReadFile(workflowPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var eventSource []byte
	if eventPath != "" {
		eventSource, err = os.ReadFile(eventPath)
	} else {
		webhook, metadataErr := agent.GetMetadataBounded(ctx, "buildkite:webhook", maxWebhookMetadataBytes+1)
		switch {
		case metadataErr == nil:
			webhook = bytes.TrimSuffix(webhook, []byte("\n"))
			if len(webhook) > maxWebhookMetadataBytes {
				err = fmt.Errorf("buildkite:webhook exceeds %d bytes", maxWebhookMetadataBytes)
			} else {
				eventSource, err = buildkiteWebhookEventSource(os.Getenv, webhook)
			}
		case errors.Is(metadataErr, transport.ErrMetadataUnavailable):
			eventSource, err = buildkiteEventSource(os.Getenv)
		default:
			err = metadataErr
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	executablePath, executableContents, distributionDigest, err := executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	preflight, err := compileHostedTokenless(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep, os.Getenv("BUILDKITE_GROUP_LABEL"), targetQueue, jobScopedActionSourceAuthentication(stderr))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	bundle := preflight.Bundle
	writeCompilerWarnings(stderr, "upload", workflowPath, bundle.IR.Warnings)
	artifacts := make([]transport.Artifact, 0, 1+len(bundle.Plans))
	distributionPath, err := buildkitepipeline.DistributionPath(distributionDigest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	artifacts = append(artifacts, transport.Artifact{Path: distributionPath, Digest: distributionDigest, Contents: executableContents})
	for _, jobPlan := range bundle.Plans {
		artifacts = append(artifacts, transport.Artifact{Path: jobPlan.Path, Digest: jobPlan.Digest, Contents: jobPlan.Contents})
	}
	root, err := os.MkdirTemp("", "buildkite-gha-upload-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: create artifact root: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	if err := transport.UploadArtifacts(ctx, agent, root, artifacts, bundle.Pipeline); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "Uploaded %d jobs from %s with importer %s.\n", len(bundle.Plans), executablePath, importerStep)
	return 0
}

type hostedTokenlessCompilation struct {
	Bundle     compiler.Bundle
	HasActions bool
}

type hostedTokenlessFailureKind string

const (
	hostedTokenlessEnvironmentFailure hostedTokenlessFailureKind = "environment"
	hostedTokenlessEvaluationFailure  hostedTokenlessFailureKind = "evaluation"
	hostedTokenlessAdmissionFailure   hostedTokenlessFailureKind = "admission"
)

type hostedTokenlessFailure struct {
	Kind hostedTokenlessFailureKind
	Err  error
}

func (e *hostedTokenlessFailure) Error() string { return e.Err.Error() }
func (e *hostedTokenlessFailure) Unwrap() error { return e.Err }

func hostedTokenlessError(kind hostedTokenlessFailureKind, err error) error {
	return &hostedTokenlessFailure{Kind: kind, Err: err}
}

type actionSourceAuthentication struct {
	provider gharuntime.WorkflowTokenProvider
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
	token, err := a.provider.WorkflowToken(ctx, repository, map[string]string{"contents": "read"})
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

func compileHostedTokenless(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep, groupLabel, targetQueue string, actionAuthentication *actionSourceAuthentication) (hostedTokenlessCompilation, error) {
	preflight, err := compiler.Compile(workflowPath, workflowSource, eventSource)
	if err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, err)
	}
	var ir compiler.IR
	if err := json.Unmarshal(preflight, &ir); err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, fmt.Errorf("decode compiler preflight: %w", err))
	}
	hasActions := irUsesActions(ir)
	options := compiler.Options{
		EventTrust: compiler.EventUntrusted,
		GroupLabel: groupLabel,
		Runners: compiler.RunnerPolicy{
			Labels: map[string]string{
				"ubuntu-latest": targetQueue,
				"ubuntu-24.04":  targetQueue,
				"ubuntu-22.04":  targetQueue,
			},
			AllowUntrustedDefaultQueue: targetQueue == "",
		},
	}
	if targetQueue != "" {
		options.Runners.UntrustedQueues = []string{targetQueue}
	}
	if hasActions {
		actionRoot, err := os.MkdirTemp("", "buildkite-gha-action-source-")
		if err != nil {
			return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEnvironmentFailure, fmt.Errorf("create action source store: %w", err))
		}
		defer func() { _ = os.RemoveAll(actionRoot) }()
		var sourceOptions []actionsource.Option
		authenticationOption := actionAuthentication.option(ir.Event.Repository.Owner + "/" + ir.Event.Repository.Name)
		if authenticationOption != nil {
			sourceOptions = append(sourceOptions, authenticationOption)
		}
		resolver, err := actionsource.NewResolver(nil, sourceOptions...)
		if err != nil {
			return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEnvironmentFailure, fmt.Errorf("configure public action resolver: %w", err))
		}
		store, err := actionsource.NewStore(actionRoot, nil)
		if err != nil {
			return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEnvironmentFailure, fmt.Errorf("configure public action source store: %w", err))
		}
		options.ResolveActions = true
		options.ActionSource = compiler.PublicActionSource{Resolver: resolver, Store: store}
	}
	bundle, err := compiler.CompileBundleContext(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep, options)
	if err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, err)
	}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessAdmissionFailure, err)
	}
	if !hasActions && bundleUsesActions(bundle) {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, fmt.Errorf("final compilation introduced actions absent from preflight"))
	}
	return hostedTokenlessCompilation{Bundle: bundle, HasActions: hasActions}, nil
}

func validateUnprivilegedBundle(bundle compiler.Bundle) error {
	for _, artifact := range bundle.Plans {
		for _, capability := range artifact.Job.RequiredCapabilities {
			if capability == "docker" && !slices.Equal(artifact.Authorization.DockerCapabilitySources, []string{"dockerfile-actions"}) {
				if slices.Contains(artifact.Authorization.DockerCapabilitySources, "job-containers") || slices.Contains(artifact.Authorization.DockerCapabilitySources, "service-containers") {
					return fmt.Errorf("job %q uses job or service containers, which hosted-tokenless upload does not admit", artifact.Job.Workflow.LogicalJobID)
				}
				return fmt.Errorf("job %q requires docker without compiler-verified Dockerfile action provenance", artifact.Job.Workflow.LogicalJobID)
			}
			if capability == "provider-token-read" {
				if !slices.Equal(artifact.Authorization.ProviderTokenReadCapabilitySources, []string{"checkout-adapter"}) {
					return fmt.Errorf("job %q requires provider-token-read without compiler-verified checkout provenance", artifact.Job.Workflow.LogicalJobID)
				}
				continue
			}
			if capability == "provider-token-write" {
				if artifact.Job.GitHubToken == nil || !slices.Equal(artifact.Authorization.ProviderTokenWriteCapabilitySources, []string{"workflow-permissions"}) {
					return fmt.Errorf("job %q requires provider-token-write without compiler-verified workflow permission provenance", artifact.Job.Workflow.LogicalJobID)
				}
				continue
			}
			if capability != "network" && capability != "docker" {
				return fmt.Errorf("job %q requires capability %q, unavailable to unprivileged upload", artifact.Job.Workflow.LogicalJobID, capability)
			}
		}
		for _, action := range artifact.Job.Actions {
			descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: action.Source, Repository: action.Repository, Path: action.Path})
			if descriptor.Service == actionintegration.ServiceCache {
				if err := actionintegration.ValidateCacheCommit(action.Commit); err != nil {
					return fmt.Errorf("job %q uses unsupported cache action: %w", artifact.Job.Workflow.LogicalJobID, err)
				}
				continue
			}
			if descriptor.Service != "" {
				return fmt.Errorf("job %q uses action %q, which requires the unavailable GitHub Actions %s service; Phase 6 is required", artifact.Job.Workflow.LogicalJobID, action.Repository, descriptor.Service)
			}
		}
	}
	return nil
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

func uploadArgs(args []string) (workflowPath, eventPath string, err error) {
	filtered := make([]string, 0, len(args))
	runtimeQueue := ""
	runtimeQueueSeen := false
	deprecatedPrivateCheckoutSeen := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--private-checkout" {
			if deprecatedPrivateCheckoutSeen {
				return "", "", fmt.Errorf("--private-checkout may only be specified once")
			}
			deprecatedPrivateCheckoutSeen = true
			continue
		}
		if args[i] != "--runtime-queue" {
			filtered = append(filtered, args[i])
			continue
		}
		if runtimeQueueSeen {
			return "", "", fmt.Errorf("--runtime-queue may only be specified once")
		}
		runtimeQueueSeen = true
		i++
		if i == len(args) {
			return "", "", fmt.Errorf("--runtime-queue requires a queue")
		}
		runtimeQueue = args[i]
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	if err != nil {
		return "", "", err
	}
	if runtimeQueueSeen && runtimeQueue != legacyRuntimeQueue {
		return "", "", fmt.Errorf("deprecated --runtime-queue must be %q", legacyRuntimeQueue)
	}
	return workflowPath, eventPath, err
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
	contents, err = os.ReadFile(path)
	if err != nil {
		return "", nil, "", fmt.Errorf("read compiler executable: %w", err)
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
