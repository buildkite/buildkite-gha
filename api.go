package gha

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/buildkite/buildkite-gha/internal/agentapi"
	command "github.com/buildkite/buildkite-gha/internal/command"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// ErrMetadataUnavailable is returned by Backend.GetMetadataBounded only when
// Buildkite confirms that buildkite:webhook is absent or unavailable. Other
// errors, including authorization and transport failures, must not use it.
var ErrMetadataUnavailable = transport.ErrMetadataUnavailable

// Backend supplies the Buildkite effects used by import and result publication.
// Implementations must honor context cancellation. Paths are workspace-relative
// slash paths; root and destination are absolute directories valid for the call.
// A failed method is never retried through a subprocess fallback.
type Backend interface {
	// UploadArtifacts uploads the materialized files matching pattern from root.
	UploadArtifacts(ctx context.Context, root, pattern string) error
	UploadArtifactFrom(ctx context.Context, root, path string) error
	// DownloadArtifact retains path beneath destination, selecting the producer
	// job or step supplied by the verified plan or stage record.
	DownloadArtifact(ctx context.Context, path, destination, producer string) error
	// SearchArtifactProducer requires exactly one artifact-owning job under step.
	SearchArtifactProducer(ctx context.Context, path, step string) (string, error)
	// UploadPipeline must disable interpolation of the supplied pipeline bytes.
	UploadPipeline(ctx context.Context, pipeline []byte) error
	SetMetadata(ctx context.Context, key, value string) error
	GetMetadataBounded(ctx context.Context, key string, limit int) ([]byte, error)
	GetStepAttribute(ctx context.Context, step, attribute string) ([]byte, error)
	// EnsureStepLabelSuffix appends suffix to the current label only if absent.
	EnsureStepLabelSuffix(ctx context.Context, suffix string) error
	// AnnotateJob updates a job-scoped Markdown annotation identified by context.
	AnnotateJob(ctx context.Context, job, annotationContext, style, body string) error
}

// Credentials supplies job-bound secrets and redaction. ResolveSecret must
// return only the requested value; AddRedaction must register it before return.
// GitCredentialHelper returns trusted, pre-pinned Git credential.helper config,
// not workflow input or inline credentials. Git invokes it as a subprocess with
// the isolated job environment. The caller owns executable pinning and quoting.
// It is requested only when repository-provider credentials are enabled.
type Credentials interface {
	ResolveSecret(context.Context, string) (string, error)
	AddRedaction(context.Context, string) error
	GitCredentialHelper() (string, error)
}

// Client binds an invocation to its host's Buildkite services. Version must
// match the version dispatched by its runtime executable. Nil writers discard
// output. Backend and Credentials are required; there are no command defaults.
type Client struct {
	Version     string
	Backend     Backend
	Credentials Credentials
	AgentAPI    http.RoundTripper
	Stdout      io.Writer
	Stderr      io.Writer
}

// Runner maps a supported runs-on label to an explicit queue and optional
// immutable image. Explicit routing requires job-scoped server validation.
type Runner struct{ Queue, Image string }

// OIDC configures the claims requested by jobs with id-token: write.
type OIDC struct {
	Claims, AWSSessionTags []string
	SubjectClaim           string
}

// CompileRequest selects a workflow set and its runtime distributions.
// WorkflowPaths requires at least one explicit path, never globs. Missing paths
// fail preparation. EventPath selects an event snapshot; empty uses the
// Buildkite build's effective event.
// RuntimeDistributions maps platform names (for example linux/arm64) to absolute
// executable paths; the current executable is the default for its own platform.
type CompileRequest struct {
	WorkflowPaths            []string
	EventPath                string
	RuntimeDistributions     map[string]string
	Runners                  map[string]Runner
	OIDC                     *OIDC
	DisableRunnerUser        bool
	PrivateReusableWorkflows bool
}

// Artifact is an immutable, content-addressed upload input. Contents is owned
// by the returned Compilation and remains valid after Compile returns.
type Artifact struct {
	Path, Digest string
	Contents     []byte
}

// Compilation contains the exact pipeline and artifacts a hosted upload needs,
// including plans, runtime binaries, event payloads, and continuation records.
type Compilation struct {
	Pipeline  []byte
	Artifacts []Artifact
}

// Compile prepares a hosted workflow set without artifact or pipeline upload.
func (c Client) Compile(ctx context.Context, request CompileRequest) (Compilation, error) {
	ctx, stdout, stderr, agent, err := c.invocation(ctx)
	if err != nil {
		return Compilation{}, err
	}
	prepared, err := command.Import(ctx, request.input(), true, stdout, stderr, c.Version, agent)
	if err != nil {
		return Compilation{}, err
	}
	result := Compilation{Pipeline: prepared.Pipeline, Artifacts: make([]Artifact, len(prepared.Artifacts))}
	for i, artifact := range prepared.Artifacts {
		result.Artifacts[i] = Artifact{Path: artifact.Path, Digest: artifact.Digest, Contents: artifact.Contents}
	}
	return result, nil
}

// Upload prepares and publishes the workflow set, uploading verified artifacts
// before its pipeline. It uses the same hosted preparation as Compile.
func (c Client) Upload(ctx context.Context, request CompileRequest) error {
	ctx, stdout, stderr, agent, err := c.invocation(ctx)
	if err != nil {
		return err
	}
	_, err = command.Import(ctx, request.input(), false, stdout, stderr, c.Version, agent)
	return err
}

// StageRequest identifies an immutable deferred-upload record and its owner.
type StageRequest struct{ Digest, Producer string }

// UploadStage verifies a continuation and publishes its deferred jobs.
func (c Client) UploadStage(ctx context.Context, request StageRequest) error {
	ctx, stdout, stderr, agent, err := c.invocation(ctx)
	if err != nil {
		return err
	}
	return command.ContinueImport(ctx, request.Digest, request.Producer, stdout, stderr, c.Version, agent)
}

// RunJobRequest accepts either PlanPath or PlanDigest with PlanProducer.
// ArtifactProducer defaults to PlanProducer. For a local PlanPath in Buildkite,
// the bootstrap supplies BUILDKITE_GHA_PLAN_DIGEST and ArtifactProducer.
type RunJobRequest struct {
	PlanPath, PlanDigest, PlanProducer, ArtifactProducer, ResultPath string
	HostedToolCache, DockerBuildLoad                                 bool
}

// RunJob verifies and executes a plan, hydrates its prerequisites, and publishes
// its result. The exit code is authoritative, including 78 for tolerated job
// failure; err preserves the cause and does not override the code.
func (c Client) RunJob(ctx context.Context, request RunJobRequest) (int, error) {
	ctx, stdout, stderr, agent, err := c.invocation(ctx)
	if err != nil {
		return 1, err
	}
	return command.ExecuteJob(ctx, command.JobInput(request), stdout, stderr, c.Version, agent)
}

// RunCLI dispatches the existing command and private runtime-helper protocol.
// Unlike typed methods it retains command signal handling and subprocess
// adapters. It is intended for executable dispatch, not native service wiring.
func RunCLI(args []string, stdout, stderr io.Writer, version string) int {
	return command.Run(args, stdout, stderr, version)
}

func (c Client) invocation(ctx context.Context) (context.Context, io.Writer, io.Writer, transport.Agent, error) {
	if c.Version == "" || c.Backend == nil || c.Credentials == nil {
		return ctx, nil, nil, transport.Agent{}, errors.New("version, Buildkite backend, and credentials are required")
	}
	if c.AgentAPI != nil {
		ctx = agentapi.WithTransport(ctx, c.AgentAPI)
	}
	ctx = gharuntime.WithCredentials(ctx, c.Credentials)
	stdout, stderr := c.Stdout, c.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return ctx, stdout, stderr, transport.Agent{Backend: c.Backend}, nil
}

func (r CompileRequest) input() command.UploadInput {
	runners := make(map[string]command.RunnerInput, len(r.Runners))
	for label, runner := range r.Runners {
		runners[label] = command.RunnerInput(runner)
	}
	var oidc *plan.OIDCConfiguration
	if r.OIDC != nil {
		oidc = &plan.OIDCConfiguration{Claims: r.OIDC.Claims, AWSSessionTags: r.OIDC.AWSSessionTags, SubjectClaim: r.OIDC.SubjectClaim}
	}
	return command.UploadInput{
		WorkflowPaths: r.WorkflowPaths, EventPath: r.EventPath,
		RuntimeDistributions: r.RuntimeDistributions, Runners: runners, OIDC: oidc,
		DisableRunnerUser: r.DisableRunnerUser, PrivateReusableWorkflows: r.PrivateReusableWorkflows,
	}
}
