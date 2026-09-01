package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	resultPublicationTimeout                    = 10 * time.Second
	repositoryProviderGitCredentialsEnvironment = "BUILDKITE_USE_REPOSITORY_PROVIDER_GIT_CREDENTIALS"
	legacyGitHubAppGitCredentialsEnvironment    = "BUILDKITE_USE_GITHUB_APP_GIT_CREDENTIALS"
	secretResolutionAnnotationContext           = "buildkite-gha-secret-resolution"
)

func repositoryProviderGitCredentialsEnabled(getenv func(string) string) bool {
	return getenv(repositoryProviderGitCredentialsEnvironment) == "true" || getenv(legacyGitHubAppGitCredentialsEnvironment) == "true"
}

func runJob(args []string, stdout, stderr io.Writer, version, clientVersion string, agent transport.Agent) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runJobContext(ctx, args, stdout, stderr, version, clientVersion, agent)
}

func runJobContext(ctx context.Context, args []string, stdout, stderr io.Writer, version, clientVersion string, agent transport.Agent) (code int) {
	started := time.Now()
	var result gharuntime.JobResult
	details := &commandTelemetryDetails{}
	stderr = details.captureErrors(stderr)
	agent.Runner = captureCommandRunnerErrors(agent.Runner, stderr)
	defer func() {
		outcome := telemetryOutcome(code, result.Conclusion, ctx.Err())
		emitCommandTelemetry(ctx, telemetry.CommandRunJob, outcome, clientVersion, time.Since(started), details.forOutcome(outcome))
	}()
	options, err := runJobArgs(args)
	if err != nil {
		return usageError(stderr, "run-job: %v", err)
	}
	failureVisible := false
	_, _ = fmt.Fprintln(stdout, "~~~ :package: Prepare GitHub Actions job")
	defer func() {
		if code != 0 && !failureVisible {
			_, _ = fmt.Fprintln(stdout, "^^^ +++")
		}
	}()
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
	if options.artifactProducer == "" {
		options.artifactProducer = options.planProducer
	}
	if err := hydrateEventPayload(ctx, agent, &job, options.artifactProducer); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: hydrate event payload: %v\n", err)
		return 1
	}
	if err := gharuntime.ValidateHost(job, runtime.GOOS, runtime.GOARCH); err != nil {
		details.setFailurePhase(telemetry.FailurePhaseExecution)
		details.setFailureCode(runtimeFailureCode(err))
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
		store, err := actionsource.NewStoreContext(ctx, actionCache, nil, actionsource.WithUserAgentVersion(clientVersion))
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
			Endpoint:      os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
			JobID:         os.Getenv("BUILDKITE_JOB_ID"),
			JobToken:      os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
			ResultsURL:    os.Getenv("BUILDKITE_GHA_CACHE_URL"),
			ClientVersion: clientVersion,
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
			Endpoint:         os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
			JobID:            os.Getenv("BUILDKITE_JOB_ID"),
			JobToken:         os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
			OrganizationSlug: os.Getenv("BUILDKITE_ORGANIZATION_SLUG"),
			PipelineSlug:     os.Getenv("BUILDKITE_PIPELINE_SLUG"),
			ClientVersion:    clientVersion,
		})
		if tokenErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: configure GitHub token service: %v\n", tokenErr)
			return 1
		}
		workflowTokens = githubTokens
	}
	var oidcTokens gharuntime.OIDCTokenProvider
	if job.IDTokenPermission == "write" {
		config := gharuntime.AgentOIDCTokenConfig{
			Endpoint:      os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
			JobID:         os.Getenv("BUILDKITE_JOB_ID"),
			JobToken:      os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
			ClientVersion: clientVersion,
		}
		if job.OIDC != nil {
			config.Claims = job.OIDC.Claims
			config.AWSSessionTags = job.OIDC.AWSSessionTags
			config.SubjectClaim = job.OIDC.SubjectClaim
		}
		oidcTokens, err = gharuntime.NewAgentOIDCTokens(config)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: configure OIDC token service: %v\n", err)
			return 1
		}
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
		Stdout:      stdout,
		Stderr:      stderr,
		MiseDataDir: prepareMiseDataDir(os.Getenv("BUILDKITE_GHA_MISE_DATA_DIR"), stderr),
		ToolCache:   runnerToolCache,
		Docker:      os.Getenv("BUILDKITE_GHA_DOCKER"),
		Git:         os.Getenv("BUILDKITE_GHA_GIT"),
		Secrets: gharuntime.AgentSecrets{
			Executable: os.Getenv("BUILDKITE_GHA_AGENT"),
			Endpoint:   os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
			JobID:      os.Getenv("BUILDKITE_JOB_ID"),
			JobToken:   os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
			NoHTTP2:    os.Getenv("BUILDKITE_NO_HTTP2"),
		},
		Redactor:              gharuntime.AgentRedactor{Executable: os.Getenv("BUILDKITE_GHA_AGENT")},
		Actions:               actionMaterializer,
		Artifacts:             agent,
		Cache:                 cacheCredentials,
		RepositoryCredentials: repositoryCredentials,
		WorkflowToken:         workflowTokens,
		OIDCToken:             oidcTokens,
		RunIdentity: gharuntime.RunIdentity{
			BuildID:     os.Getenv("BUILDKITE_BUILD_ID"),
			BuildNumber: os.Getenv("BUILDKITE_BUILD_NUMBER"),
			RetryCount:  os.Getenv("BUILDKITE_RETRY_COUNT"),
		},
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
			mise, err := resolveRuntimeMiseVersion(ctx, os.Getenv("BUILDKITE_GHA_MISE"), runner.MiseDataDir, privateRuntime, stderr, clientVersion)
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
	var runErr error
	if len(job.NeedSources) != 0 {
		job.Needs, runErr = gharuntime.ResolveNeeds(ctx, agent, artifactRoot, producer.BuildID, job.NeedSources, job.NeedOutputs)
		if runErr != nil {
			details.setFailurePhase(telemetry.FailurePhaseSourceResolution)
			runErr = fmt.Errorf("hydrate prerequisite results: %w", runErr)
		}
	}
	if runErr == nil && len(job.DeferredInputs) != 0 {
		inputs, err := gharuntime.ResolveDeferredInputs(ctx, agent, artifactRoot, producer.BuildID, job.DeferredInputs)
		if err != nil {
			details.setFailurePhase(telemetry.FailurePhaseSourceResolution)
			runErr = fmt.Errorf("hydrate reusable-workflow inputs: %w", err)
		} else {
			job.DeferredInputValues = inputs
		}
	}
	if runErr == nil && len(job.CallGuards) != 0 {
		job.CallGuards, runErr = gharuntime.ResolveCallGuards(ctx, agent, artifactRoot, producer.BuildID, job.CallGuards)
		if runErr != nil {
			details.setFailurePhase(telemetry.FailurePhaseSourceResolution)
			runErr = fmt.Errorf("hydrate reusable-workflow call guards: %w", runErr)
		}
	}
	if runErr == nil {
		result, runErr = runner.RunJob(ctx, job, "")
		failureVisible = result.FailureVisible()
		if runErr != nil {
			details.setFailurePhase(telemetry.FailurePhaseExecution)
			details.setFailureCode(runtimeFailureCode(runErr))
		}
	}
	if result.Conclusion == "" {
		result.Conclusion = terminalErrorConclusion(ctx)
	}
	if options.resultPath != "" && result.Conclusion != "" {
		if err := writeJobResult(options.resultPath, result); err != nil {
			details.setFailurePhase(telemetry.FailurePhaseResultPublication)
			runErr = errors.Join(runErr, err)
			result.Conclusion = terminalErrorConclusion(ctx)
		}
	}
	if publish && runErr != nil && !result.FailureVisible() {
		_, _ = fmt.Fprintln(stdout, "+++ :warning: Prepare GitHub Actions job failed")
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", runErr)
		failureVisible = true
	}
	if publish {
		_, _ = fmt.Fprintln(stdout, "~~~ :package: Publish GitHub Actions result")
		publication, err := publishTerminalResult(ctx, agent, artifactRoot, job, planDigest, producer, result)
		secretAnnotationError := publishSecretResolutionAnnotation(ctx, agent, producer.JobID, runErr)
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
		if secretAnnotationError != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: secret resolution annotation: %v\n", secretAnnotationError)
		}
		if err != nil || publication.MetadataMirrorError != nil || publication.SummaryAnnotationError != nil || publication.WarningAnnotationError != nil || publication.ErrorAnnotationError != nil || secretAnnotationError != nil {
			_, _ = fmt.Fprintln(stdout, "^^^ +++")
			failureVisible = true
		}
		if err != nil {
			details.setFailurePhase(telemetry.FailurePhaseResultPublication)
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

// runtimeFailureCode attributes a RunJob error so ordinary workflow failures,
// such as a test command exiting nonzero, are not counted as compatibility
// gaps. Unattributed errors return "" and keep the unknown failure code.
func runtimeFailureCode(err error) telemetry.FailureCode {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	var secretError *gharuntime.SecretResolutionError
	if errors.As(err, &secretError) {
		return telemetry.FailureCodeSecretUnavailable
	}
	switch gharuntime.ClassifyFailure(err) {
	case gharuntime.FailureClassStepProcessExit:
		return telemetry.FailureCodeStepProcessExit
	case gharuntime.FailureClassUnsupportedFeature:
		return telemetry.FailureCodeUnsupportedFeature
	case gharuntime.FailureClassIntegrity:
		return telemetry.FailureCodeRuntimeIntegrity
	default:
		return ""
	}
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
		identity := actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path}
		descriptor, _ := actionintegration.Lookup(identity)
		if descriptor.Service == actionintegration.ServiceCache {
			required = true
			if _, _, err := actionintegration.Admit(identity, lock.Commit); err != nil {
				return false, fmt.Errorf("unsupported cache action: %w", err)
			}
		}
	}
	return required, nil
}

func resultProducer(job plan.Job, planDigest, expectedDigest string) (transport.Producer, bool, error) {
	if os.Getenv("BUILDKITE") == "" {
		if len(job.NeedSources) != 0 || callGuardsHaveNeedSources(job.CallGuards) {
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

func callGuardsHaveNeedSources(guards []plan.CallGuard) bool {
	for _, guard := range guards {
		if len(guard.NeedSources) != 0 {
			return true
		}
	}
	return false
}

func hydrateEventPayload(ctx context.Context, agent transport.Agent, job *plan.Job, producer string) error {
	if !job.Event.PayloadArtifact {
		return nil
	}
	if producer == "" {
		return fmt.Errorf("event payload artifact requires an importer job producer")
	}
	path, err := buildkitepipeline.EventPath(job.Event.PayloadDigest)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "buildkite-gha-event-")
	if err != nil {
		return fmt.Errorf("create event payload directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := agent.DownloadArtifact(ctx, path, root, producer); err != nil {
		return fmt.Errorf("download event payload artifact: %w", err)
	}
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return fmt.Errorf("open event payload artifact: %w", err)
	}
	defer func() { _ = file.Close() }()
	source, err := io.ReadAll(io.LimitReader(file, int64(plan.MaxEventPayloadBytes)+1))
	if err != nil {
		return fmt.Errorf("read event payload artifact: %w", err)
	}
	payload, err := plan.DecodeEventPayload(source, job.Event.PayloadDigest)
	if err != nil {
		return err
	}
	job.Event.Payload = &payload
	return nil
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

func publishTerminalResult(parent context.Context, agent transport.Agent, root string, job plan.Job, planDigest string, producer transport.Producer, result gharuntime.JobResult) (transport.Publication, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), resultPublicationTimeout)
	defer cancel()
	workflow := strings.TrimPrefix(job.Workflow.Digest, "sha256:")
	return gharuntime.PublishJobResult(ctx, agent, root, workflow, job.Target.StepKey, planDigest, producer, result)
}

func publishSecretResolutionAnnotation(parent context.Context, agent transport.Agent, jobID string, runErr error) error {
	var secretError *gharuntime.SecretResolutionError
	if !errors.As(runErr, &secretError) {
		return nil
	}
	body := fmt.Sprintf(`#### Secret unavailable

This job could not retrieve the Buildkite secret %s.

1. <a href="https://buildkite.com/docs/pipelines/security/secrets/buildkite-secrets" target="_blank">Create or migrate the secret into Buildkite</a>.
1. If the secret already exists, <a href="https://buildkite.com/docs/pipelines/security/secrets/buildkite-secrets/access-policies" target="_blank">grant this job access with its access policy</a>.
1. If the secret and its access policy are correct, retry the job. The Buildkite secret service may be temporarily unavailable.

> ℹ️ GitHub does not expose an existing secret's value after creation. Copy or rotate the value manually. GitHub repository and environment secrets are not available directly to this job.
`, annotationCode(secretError.Name))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), resultPublicationTimeout)
	defer cancel()
	return agent.AnnotateJob(ctx, jobID, secretResolutionAnnotationContext, "error", body)
}

type runJobOptions struct {
	planPath         string
	planDigest       string
	planProducer     string
	artifactProducer string
	resultPath       string
	hostedToolCache  bool
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
		case "--plan", "--plan-digest", "--plan-producer", "--artifact-producer", "--result":
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
			case "--artifact-producer":
				options.artifactProducer = args[i]
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
