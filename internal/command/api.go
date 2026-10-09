package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// UploadInput is the typed input to the shared hosted importer.
type UploadInput struct {
	WorkflowPaths            []string
	EventPath                string
	RuntimeDistributions     map[string]string
	Runners                  map[string]RunnerInput
	OIDC                     *plan.OIDCConfiguration
	DisableRunnerUser        bool
	PrivateReusableWorkflows bool
}

type RunnerInput struct{ Queue, Image string }

// PreparedUpload owns opaque pipeline and artifact bytes after preparation.
type PreparedUpload struct {
	Pipeline  []byte
	Artifacts []transport.Artifact
}

// Import uses the same hosted importer as upload and plugin. prepareOnly stops
// before artifact materialization and pipeline publication.
func Import(ctx context.Context, input UploadInput, prepareOnly bool, stdout, stderr io.Writer, version string, agent transport.Agent) (PreparedUpload, error) {
	platform, err := importerPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return PreparedUpload{}, err
	}
	options := parsedUploadArgs{
		workflowOperands: input.WorkflowPaths, eventPath: input.EventPath,
		clientVersion: version, importerPlatform: platform,
		experimentalRunnerUser:   !input.DisableRunnerUser,
		privateReusableWorkflows: input.PrivateReusableWorkflows,
		oidc:                     input.OIDC,
		runnerTargets:            make(map[string]compiler.RunnerTarget, len(input.Runners)),
		runtimeDistributionPaths: make(map[compiler.Platform]string, len(input.RuntimeDistributions)),
	}
	for label, runner := range input.Runners {
		canonical, target, err := configuredRunnerTarget(label, runner.Queue, runner.Image)
		if err != nil {
			return PreparedUpload{}, err
		}
		if _, exists := options.runnerTargets[canonical]; exists {
			return PreparedUpload{}, fmt.Errorf("duplicate runner label %q", canonical)
		}
		options.runnerTargets[canonical] = target
	}
	for name, path := range input.RuntimeDistributions {
		platform, err := compiler.ParsePlatform(name)
		if err != nil {
			return PreparedUpload{}, err
		}
		if !filepath.IsAbs(path) {
			return PreparedUpload{}, fmt.Errorf("runtime distribution for %s must use an absolute path", platform)
		}
		options.runtimeDistributionPaths[platform] = path
	}
	var prepared PreparedUpload
	if prepareOnly {
		options.prepared = &prepared
	}
	err = uploadOperation(ctx, options, stdout, stderr, commandVersion(version), agent)
	return prepared, err
}

// ContinueImport reads and verifies an immutable continuation record.
func ContinueImport(ctx context.Context, digest, producer string, stdout, stderr io.Writer, version string, agent transport.Agent) error {
	if !stageDigestPattern.MatchString(digest) || producer == "" {
		return errors.New("stage digest and producer are required")
	}
	return uploadStageOperation(ctx, stageOptions{digest: digest, producer: producer}, stdout, stderr, commandVersion(version), version, agent)
}

// JobInput carries the bootstrap's plan and producer arguments.
type JobInput struct {
	PlanPath, PlanDigest, PlanProducer, ArtifactProducer, ResultPath string
	HostedToolCache, DockerBuildLoad                                 bool
}

// ExecuteJob preserves the runtime's authoritative exit status and cause.
func ExecuteJob(ctx context.Context, input JobInput, stdout, stderr io.Writer, version string, agent transport.Agent) (int, error) {
	options := runJobOptions{
		planPath: input.PlanPath, planDigest: input.PlanDigest, planProducer: input.PlanProducer,
		artifactProducer: input.ArtifactProducer, resultPath: input.ResultPath,
		hostedToolCache: input.HostedToolCache, dockerBuildLoad: input.DockerBuildLoad,
	}
	var err error
	switch {
	case input.PlanPath != "":
		if input.PlanDigest != "" || input.PlanProducer != "" {
			err = errors.New("plan path cannot be combined with plan digest or producer")
		}
	case input.PlanDigest == "" || input.PlanProducer == "":
		err = errors.New("plan path or both plan digest and producer are required")
	default:
		options.planPath, err = buildkitepipeline.PlanPath(input.PlanDigest)
	}
	return runJobOperation(ctx, options, err, stdout, stderr, commandVersion(version), version, agent)
}

type usageFailure struct{ error }

func operationUsageError(stderr io.Writer, format string, args ...any) error {
	usageError(stderr, format, args...)
	return &usageFailure{fmt.Errorf(format, args...)}
}

func operationExit(err error) int {
	if err == nil {
		return 0
	}
	var usage *usageFailure
	if errors.As(err, &usage) {
		return 2
	}
	return 1
}
