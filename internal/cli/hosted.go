package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

const (
	hostedProfile                = "hosted"
	legacyHostedTokenlessProfile = "hosted-tokenless"
	defaultNobleRunnerImage      = "buildkite.namespace-images.com/agent-base@sha256:243c4015ea220c526b7976df88b037aff1930c1278d8d716b2f5f90247c72a08"
	defaultJammyRunnerImage      = "buildkite.namespace-images.com/agent-base@sha256:21c6794d225832c5e104c2ed97c8602da5925caf225e31018f32408074fa40fc"
	defaultMacOSRunnerQueue      = "macos-medium"
)

var runnerQueuePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var runnerImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)

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
	provider   gharuntime.ActionSourceTokenProvider
	redactor   gharuntime.Redactor
	warnings   io.Writer
	once       sync.Once
	credential string
	err        error
}

func importerJobActionSourceAuthentication(warnings io.Writer) *actionSourceAuthentication {
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
	return actionsource.WithGitHubActionSourceTokenProvider(repository, func(ctx context.Context) (string, error) {
		return a.token(ctx, repository)
	})
}

func (a *actionSourceAuthentication) token(ctx context.Context, repository string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a.once.Do(func() {
		a.credential, a.err = a.provision(ctx, repository)
	})
	return a.credential, a.err
}

func (a *actionSourceAuthentication) provision(ctx context.Context, repository string) (string, error) {
	if a.provider == nil {
		a.warnAnonymousFallback("GitHub action source authentication is unavailable")
		return "", nil
	}
	token, err := a.provider.ActionSourceToken(ctx, repository)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		a.warnAnonymousFallback("could not mint a GitHub action source token")
		return "", nil
	}
	if err := a.redactor.AddRedaction(ctx, token); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		a.warnAnonymousFallback("could not register the GitHub action source token with the Buildkite Agent redactor")
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
	targets := hostedRunnerTargets()
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
	for _, target := range targets {
		if target.Queue != "" && !slices.Contains(options.Runners.UntrustedQueues, target.Queue) {
			options.Runners.UntrustedQueues = append(options.Runners.UntrustedQueues, target.Queue)
		}
	}
	return options
}

// hostedRunnerTargets is the runner preset shared by hosted validation and
// production upload. RunnerPolicy resolves these labels case-insensitively.
// Keep API-resolved and organization-provided targets out of this preset.
func hostedRunnerTargets() map[string]compiler.RunnerTarget {
	return map[string]compiler.RunnerTarget{
		"ubuntu-latest": {Platform: compiler.PlatformLinuxAMD64, Image: defaultNobleRunnerImage},
		"ubuntu-24.04":  {Platform: compiler.PlatformLinuxAMD64, Image: defaultNobleRunnerImage},
		"ubuntu-22.04":  {Platform: compiler.PlatformLinuxAMD64, Image: defaultJammyRunnerImage},
		"macos-latest":  {Platform: compiler.PlatformDarwinARM64, Queue: defaultMacOSRunnerQueue},
	}
}

func compileHosted(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep, groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runtimeDistributions map[compiler.Platform]string, actionAuthentication *actionSourceAuthentication) (hostedCompilation, error) {
	return compileHostedNamespaced(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep, groupLabel, configuredTargets, runtimeDistributions, "", nil, actionAuthentication)
}

func compileHostedWithActionCache(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep, groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runtimeDistributions map[compiler.Platform]string, actionCacheDir string, sharedActionSource compiler.ActionSource, actionAuthentication *actionSourceAuthentication) (hostedCompilation, error) {
	return compileHostedNamespacedWithActionCache(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep, groupLabel, configuredTargets, runtimeDistributions, "", nil, actionCacheDir, sharedActionSource, actionAuthentication)
}

func compileHostedNamespaced(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep, groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runtimeDistributions map[compiler.Platform]string, stepKeyNamespace string, oidc *plan.OIDCConfiguration, actionAuthentication *actionSourceAuthentication) (hostedCompilation, error) {
	return compileHostedNamespacedWithActionCache(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep, groupLabel, configuredTargets, runtimeDistributions, stepKeyNamespace, oidc, "", nil, actionAuthentication)
}

func compileHostedNamespacedWithActionCache(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep, groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runtimeDistributions map[compiler.Platform]string, stepKeyNamespace string, oidc *plan.OIDCConfiguration, actionCacheDir string, sharedActionSource compiler.ActionSource, actionAuthentication *actionSourceAuthentication) (hostedCompilation, error) {
	options := hostedOptions(groupLabel, configuredTargets, runtimeDistributions)
	options.StepKeyNamespace = stepKeyNamespace
	options.OIDC = oidc
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
		actionSource := sharedActionSource
		cleanup := func() {}
		if actionSource == nil {
			var sourceOptions []actionsource.Option
			if ir.Event.Provider == "github" {
				authenticationOption := actionAuthentication.option(ir.Event.Repository.Owner + "/" + ir.Event.Repository.Name)
				if authenticationOption != nil {
					sourceOptions = append(sourceOptions, authenticationOption)
				}
			}
			actionSource, cleanup, err = newHostedActionSource(ctx, actionCacheDir, sourceOptions, nil)
			if err != nil {
				return hostedCompilation{}, hostedError(hostedEnvironmentFailure, err)
			}
		}
		defer cleanup()
		options.ResolveActions = true
		options.ActionSource = actionSource
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

func newHostedActionSource(ctx context.Context, actionCacheDir string, resolverOptions, storeOptions []actionsource.Option) (compiler.ActionSource, func(), error) {
	actionSource, cleanup, _, err := newHostedActionSourceWithSnapshot(ctx, actionCacheDir, resolverOptions, storeOptions)
	return actionSource, cleanup, err
}

func newHostedActionSourceWithSnapshot(ctx context.Context, actionCacheDir string, resolverOptions, storeOptions []actionsource.Option) (compiler.ActionSource, func(), string, error) {
	actionRoot := actionCacheDir
	cleanup := func() {}
	if actionRoot == "" {
		var err error
		actionRoot, err = os.MkdirTemp("", "buildkite-gha-action-source-")
		if err != nil {
			return nil, cleanup, "", fmt.Errorf("create action source store: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(actionRoot) }
	}
	resolver, err := actionsource.NewResolver(nil, resolverOptions...)
	if err != nil {
		cleanup()
		return nil, func() {}, "", fmt.Errorf("configure public action resolver: %w", err)
	}
	store, err := actionsource.NewStoreContext(ctx, actionRoot, nil, storeOptions...)
	if err != nil {
		cleanup()
		return nil, func() {}, "", fmt.Errorf("configure public action source store: %w", err)
	}
	return compiler.MemoizeActionSource(compiler.PublicActionSource{Resolver: resolver, Store: store}), cleanup, resolver.ResolutionSnapshotID(), nil
}

func validateUnprivilegedBundle(bundle compiler.Bundle) error {
	instances := make(map[string]compiler.JobInstance, len(bundle.IR.Jobs))
	for _, instance := range bundle.IR.Jobs {
		instances[instance.Key] = instance
	}
	var diagnostics []error
	addFailure := func(artifact compiler.PlanArtifact, message, detail string, err error) {
		finding := &compiler.ProcessingFinding{
			Stage: compiler.StageAdmission, Code: "E_PROFILE", Category: "admission",
			Job: artifact.Job.Workflow.LogicalJobID, Instance: artifact.Job.Target.StepKey,
			Message: message, Detail: detail, Err: err,
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
			if capability == "secrets" {
				continue
			}
			if capability == "docker" && !admittedDockerProvenance(artifact.Job, artifact.Authorization.DockerCapabilitySources) {
				message := fmt.Sprintf("Job %q requires Docker without matching compiler provenance. Hosted runs support only verified Dockerfile actions and bounded job or service containers.", artifact.Job.Workflow.LogicalJobID)
				addFailure(artifact, message, "", errors.New("unsupported Docker access"))
				continue
			}
			if capability == "provider-token-read" {
				if !slices.Equal(artifact.Authorization.ProviderTokenReadCapabilitySources, []string{"checkout-adapter"}) {
					message := "GitHub checkout credentials are unsupported for this job; use the managed actions/checkout integration"
					addFailure(artifact, message, "", errors.New("unsupported GitHub checkout credentials"))
				}
				continue
			}
			if capability == "provider-token-write" {
				filename := ""
				if artifact.Job.GitHubToken != nil {
					filename = artifact.Job.GitHubToken.Workflow
				}
				filenameErr := plan.ValidateGitHubWorkflowPolicyFilename(filename)
				if artifact.Job.GitHubToken == nil || !slices.Equal(artifact.Authorization.ProviderTokenWriteCapabilitySources, []string{"effective-permissions"}) || filenameErr != nil || artifact.Authorization.WorkflowTokenPolicyFilename == "" || artifact.Authorization.WorkflowTokenPolicyFilename != filename {
					reason := bundle.IR.Workflow.WorkflowTokenPolicyDiagnostic
					if reason == "" {
						reason = "the workflow token request does not match the compiled job"
					}
					message, detail := githubTokenAdmissionDiagnostic(artifact, reason)
					addFailure(artifact, message, detail, errors.New("hosted GITHUB_TOKEN unsupported"))
				}
				continue
			}
			if capability != "network" && capability != "docker" {
				message := fmt.Sprintf("Job %q requires unsupported hosted runtime capability %q. Remove the requirement or use a runtime profile that supports it.", artifact.Job.Workflow.LogicalJobID, capability)
				addFailure(artifact, message, "", errors.New(message))
			}
		}
		for _, action := range artifact.Job.Actions {
			identity := actionintegration.Identity{Source: action.Source, Repository: action.Repository, Path: action.Path}
			descriptor, _ := actionintegration.Lookup(identity)
			if descriptor.Service == actionintegration.ServiceCache {
				if _, _, err := actionintegration.Admit(identity, action.Commit); err != nil {
					reference := action.Repository
					if action.Path != "" {
						reference += "/" + action.Path
					}
					if action.RequestedRef != "" {
						reference += "@" + action.RequestedRef
					}
					message, detail, _ := actionintegration.UnsupportedVersionDiagnostic(reference, err)
					addFailure(artifact, message, detail, fmt.Errorf("job %q uses unsupported cache action: %w", artifact.Job.Workflow.LogicalJobID, err))
				}
				continue
			}
			if descriptor.Service != "" {
				reference := action.Repository
				if action.Path != "" {
					reference += "/" + action.Path
				}
				message := fmt.Sprintf("Action %q requires the GitHub Actions %s service, which hosted runs do not provide. Replace the action or use a runtime profile that provides this service.", reference, descriptor.Service)
				addFailure(artifact, message, "", fmt.Errorf("job %q uses action %q, which requires the unavailable GitHub Actions %s service", artifact.Job.Workflow.LogicalJobID, reference, descriptor.Service))
			}
		}
	}
	return errors.Join(diagnostics...)
}

func admittedDockerProvenance(job plan.Job, sources []string) bool {
	if len(sources) == 0 {
		return false
	}
	expected := make([]string, 0, 3)
	if slices.Contains(sources, "dockerfile-actions") {
		expected = append(expected, "dockerfile-actions")
	}
	if job.Container != nil {
		expected = append(expected, "job-containers")
	}
	if len(job.Services) != 0 || job.ServicesExpression != "" {
		expected = append(expected, "service-containers")
	}
	return slices.Equal(sources, expected)
}

func githubTokenAdmissionDiagnostic(artifact compiler.PlanArtifact, reason string) (message, detail string) {
	limitation := reason
	fix := "Use a supported workflow-level permissions map or remove the GITHUB_TOKEN dependency."
	switch {
	case strings.Contains(reason, "reusable-workflow jobs"):
		limitation = "workflows containing reusable-workflow jobs cannot receive a hosted GITHUB_TOKEN"
		fix = "Inline the reusable job or remove the GITHUB_TOKEN dependency; runner configuration cannot enable this shape."
	case strings.Contains(reason, "directly under .github/workflows"):
		limitation = "hosted GITHUB_TOKEN issuance requires the workflow directly under .github/workflows"
		fix = "Move the workflow directly under .github/workflows."
	case strings.Contains(reason, "unsupported permission"):
		limitation = reason
		fix = "Remove the unsupported permission or avoid requiring GITHUB_TOKEN."
	case strings.Contains(reason, "explicit non-empty"):
		limitation = "an explicit empty or all-none top-level permissions map cannot issue GITHUB_TOKEN"
		fix = "Declare the required permissions at the workflow top level."
	}

	var causes []string
	if artifact.Authorization.GitHubTokenSecretReference {
		causes = append(causes, "the workflow references secrets.GITHUB_TOKEN")
	}
	if len(artifact.Authorization.GitHubTokenActions) != 0 {
		quoted := make([]string, len(artifact.Authorization.GitHubTokenActions))
		for i, action := range artifact.Authorization.GitHubTokenActions {
			quoted[i] = fmt.Sprintf("%q", action)
		}
		actionLabel := "action"
		if len(quoted) > 1 {
			actionLabel = "actions"
		}
		causes = append(causes, actionLabel+" "+strings.Join(quoted, ", ")+" defaults an input to github.token")
	}
	if len(causes) == 0 {
		causes = append(causes, "the compiled job requests a workflow token")
	}
	permissions := "none"
	if artifact.Job.GitHubToken != nil && len(artifact.Job.GitHubToken.Permissions) != 0 {
		names := make([]string, 0, len(artifact.Job.GitHubToken.Permissions))
		for name := range artifact.Job.GitHubToken.Permissions {
			names = append(names, name)
		}
		sort.Strings(names)
		values := make([]string, 0, len(names))
		for _, name := range names {
			values = append(values, strings.ReplaceAll(name, "_", "-")+": "+artifact.Job.GitHubToken.Permissions[name])
		}
		permissions = strings.Join(values, ", ")
	}
	message = fmt.Sprintf("Job %q needs GITHUB_TOKEN, but %s. %s", artifact.Job.Workflow.LogicalJobID, limitation, fix)
	detail = fmt.Sprintf("Cause: %s. Effective permissions: %s.", strings.Join(causes, "; "), permissions)
	return message, detail
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

// bundleRunsUnprovenActions reports whether a compiled bundle executes action
// code the compiler never ran. Actions a native adapter replaces keep both
// their locks and their `uses` steps in the plan, so the adapter decision is
// the only thing separating them from actions that really run.
func bundleRunsUnprovenActions(bundle compiler.Bundle) bool {
	for _, artifact := range bundle.Plans {
		for _, lock := range artifact.Job.Actions {
			identity := actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path}
			if !actionintegration.UsesNativeAdapter(identity) {
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
		if preset, ok := hostedRunnerTargets()[canonical]; ok {
			image = preset.Image
		}
	}
	return canonical, compiler.RunnerTarget{Queue: queue, Platform: platform, Image: image}, nil
}

func supportedRunnerTarget(label string) (string, compiler.Platform, error) {
	if label != strings.TrimSpace(label) || strings.ContainsAny(label, "\r\n") {
		return "", compiler.Platform{}, fmt.Errorf("unsupported runner label %q", label)
	}
	canonical := strings.ToLower(label)
	if preset, ok := hostedRunnerTargets()[canonical]; ok {
		return canonical, preset.Platform, nil
	}
	switch canonical {
	case "macos-15", "macos-14":
		// These remain available as local fallbacks when the Agent API is absent.
		return canonical, compiler.PlatformDarwinARM64, nil
	default:
		return "", compiler.Platform{}, fmt.Errorf("unsupported runner label %q", label)
	}
}
