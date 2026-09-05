package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/git"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	pluginConfigurationEnvironment         = "BUILDKITE_PLUGIN_CONFIGURATION"
	pipelineTriggerWorkflowPathEnvironment = "BUILDKITE_GITHUB_WORKFLOW_PATH"
	githubWorkflowEnvironment              = "GITHUB_WORKFLOW"
	githubWorkflowRefEnvironment           = "GITHUB_WORKFLOW_REF"
	githubWorkflowSHAEnvironment           = "GITHUB_WORKFLOW_SHA"
)

func plugin(args []string, stdout, stderr io.Writer, version, clientVersion string, runner transport.Runner) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return pluginContext(ctx, args, stdout, stderr, version, clientVersion, runner)
}

func pluginContext(ctx context.Context, args []string, stdout, stderr io.Writer, version, clientVersion string, runner transport.Runner) (code int) {
	started := time.Now()
	details := &commandTelemetryDetails{}
	stderr = details.captureErrors(stderr)
	runner = captureCommandRunnerErrors(runner, stderr)
	defer func() {
		outcome := telemetryOutcome(code, "", ctx.Err())
		emitCommandTelemetry(ctx, telemetry.CommandPluginImport, outcome, clientVersion, time.Since(started), details.forOutcome(outcome))
	}()
	if len(args) != 0 {
		return usageError(stderr, "plugin does not accept arguments")
	}
	importerPlatform, err := importerPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %v\n", err)
		return 1
	}
	configuration, err := parsePluginConfiguration(os.Getenv(pluginConfigurationEnvironment))
	if err != nil {
		return usageError(stderr, "plugin: %v", err)
	}
	workflowOperands, serverSelectedWorkflow, err := pluginWorkflowOperands(configuration, os.Getenv)
	if err != nil {
		return usageError(stderr, "plugin: %v", err)
	}
	checkoutPath := os.Getenv("BUILDKITE_BUILD_CHECKOUT_PATH")
	if err := normalizePluginCommit(ctx, checkoutPath, os.Getenv, os.Setenv, runner); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %v\n", err)
		return 1
	}
	if serverSelectedWorkflow != nil && serverSelectedWorkflow.SHA != "" && serverSelectedWorkflow.SHA != os.Getenv("BUILDKITE_COMMIT") {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %s does not match BUILDKITE_COMMIT after checkout: %q != %q\n", githubWorkflowSHAEnvironment, serverSelectedWorkflow.SHA, os.Getenv("BUILDKITE_COMMIT"))
		return 1
	}
	return uploadParsedContext(ctx, parsedUploadArgs{
		workflowOperands:       workflowOperands,
		explicitWorkflowPaths:  true,
		serverSelectedWorkflow: serverSelectedWorkflow,
		checkoutPath:           checkoutPath,
		clientVersion:          clientVersion,
		runnerTargets:          configuration.runnerTargets,
		oidc:                   configuration.OIDC,
		experimentalRunnerUser: configuration.ExperimentalRunnerUser,
		pluginAcquisition:      &pluginRuntimeAcquisition{version: version},
		importerPlatform:       importerPlatform,
		telemetry:              details,
	}, stdout, stderr, version, transport.Agent{Runner: runner})
}

type pluginConfiguration struct {
	Workflows              []string
	ExperimentalRunnerUser bool
	OIDC                   *plan.OIDCConfiguration
	runnerTargets          map[string]compiler.RunnerTarget
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
		case "workflow", "workflows", "runners", "oidc", "version", "source-ref", "minimum-release-age", "experimental-runner-user":
		default:
			return pluginConfiguration{}, fmt.Errorf("%s contains unknown field %q", pluginConfigurationEnvironment, key)
		}
	}
	experimentalRunnerUser := true
	if value, configured := encoded["experimental-runner-user"]; configured {
		var ok bool
		experimentalRunnerUser, ok = value.(bool)
		if !ok {
			return pluginConfiguration{}, fmt.Errorf("%s experimental-runner-user must be a boolean", pluginConfigurationEnvironment)
		}
	}
	workflowValue, hasWorkflow := encoded["workflow"]
	workflowsValue, hasWorkflows := encoded["workflows"]
	if hasWorkflow && hasWorkflows {
		return pluginConfiguration{}, fmt.Errorf("%s workflow and workflows are mutually exclusive", pluginConfigurationEnvironment)
	}
	var workflows []string
	if hasWorkflow {
		workflow, ok := workflowValue.(string)
		if !ok || strings.TrimSpace(workflow) == "" {
			return pluginConfiguration{}, fmt.Errorf("%s workflow must be a non-empty string", pluginConfigurationEnvironment)
		}
		workflows = []string{workflow}
	} else if hasWorkflows {
		values, ok := workflowsValue.([]any)
		if ok && len(values) != 0 {
			workflows = make([]string, len(values))
			for index, entry := range values {
				workflow, ok := entry.(string)
				if !ok || strings.TrimSpace(workflow) == "" {
					return pluginConfiguration{}, fmt.Errorf("%s workflows entry %d must be a non-empty string", pluginConfigurationEnvironment, index)
				}
				workflows[index] = workflow
			}
		}
		if len(workflows) == 0 {
			return pluginConfiguration{}, fmt.Errorf("%s workflow or workflows is required; workflows must be a non-empty array of non-empty strings", pluginConfigurationEnvironment)
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
				case "runs-on", "queue", "image", "cache":
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
			if cacheValue, configured := runner["cache"]; configured {
				cache, err := parsePluginCacheVolume(cacheValue)
				if err != nil {
					return pluginConfiguration{}, fmt.Errorf("runner %d: %w", index, err)
				}
				target.Cache = cache
			}
			if _, duplicate := targets[label]; duplicate {
				return pluginConfiguration{}, fmt.Errorf("runner label %q may only be configured once", label)
			}
			targets[label] = target
		}
	}
	var oidc *plan.OIDCConfiguration
	if value, configured := encoded["oidc"]; configured {
		oidc, err = parsePluginOIDCConfiguration(value)
		if err != nil {
			return pluginConfiguration{}, err
		}
	}
	return pluginConfiguration{Workflows: workflows, ExperimentalRunnerUser: experimentalRunnerUser, OIDC: oidc, runnerTargets: targets}, nil
}

type pipelineTriggerWorkflow struct {
	Name string
	SHA  string
}

func pluginWorkflowOperands(configuration pluginConfiguration, getenv func(string) string) ([]string, *pipelineTriggerWorkflow, error) {
	if len(configuration.Workflows) != 0 {
		return configuration.Workflows, nil, nil
	}
	compatibilityPath := getenv(pipelineTriggerWorkflowPathEnvironment)
	if compatibilityPath == "" {
		for _, name := range []string{githubEventNameEnvironment, githubWorkflowEnvironment, githubWorkflowRefEnvironment, githubWorkflowSHAEnvironment} {
			if getenv(name) != "" {
				return nil, nil, fmt.Errorf("GitHub Actions Pipeline Trigger environment is missing %s", pipelineTriggerWorkflowPathEnvironment)
			}
		}
		return nil, nil, fmt.Errorf("%s workflow or workflows is required; workflows must be a non-empty array of non-empty strings", pluginConfigurationEnvironment)
	}
	if strings.TrimSpace(compatibilityPath) == "" || strings.TrimSpace(compatibilityPath) != compatibilityPath {
		return nil, nil, fmt.Errorf("%s must be a non-empty workflow path", pipelineTriggerWorkflowPathEnvironment)
	}

	event, err := buildkiteGitHubEventName(getenv)
	if err != nil {
		return nil, nil, err
	}
	if event == "" {
		return nil, nil, fmt.Errorf("%s or BUILDKITE_GITHUB_EVENT is required", githubEventNameEnvironment)
	}
	if event != "push" && event != "pull_request" && event != "issues" && event != "issue_comment" {
		return nil, nil, fmt.Errorf("%s or BUILDKITE_GITHUB_EVENT must be push, pull_request, issues, or issue_comment", githubEventNameEnvironment)
	}

	workflowName := getenv(githubWorkflowEnvironment)
	if workflowName != "" && strings.TrimSpace(workflowName) == "" {
		return nil, nil, fmt.Errorf("%s must be non-empty", githubWorkflowEnvironment)
	}
	selectedPath := compatibilityPath
	if workflowRef := getenv(githubWorkflowRefEnvironment); workflowRef != "" {
		var err error
		selectedPath, _, err = parsePipelineTriggerWorkflowRef(workflowRef, event, getenv("BUILDKITE_REPO"))
		if err != nil {
			return nil, nil, err
		}
	}
	workflowSHA := getenv(githubWorkflowSHAEnvironment)
	if workflowSHA != "" && !git.ValidObjectID(workflowSHA) {
		return nil, nil, fmt.Errorf("%s must be a full lowercase 40-hex commit", githubWorkflowSHAEnvironment)
	}
	if (event == "issues" || event == "issue_comment") && (getenv(githubWorkflowRefEnvironment) == "" || workflowSHA == "") {
		return nil, nil, fmt.Errorf("%s and %s are required for %s", githubWorkflowRefEnvironment, githubWorkflowSHAEnvironment, event)
	}
	return []string{selectedPath}, &pipelineTriggerWorkflow{Name: workflowName, SHA: workflowSHA}, nil
}

func parsePipelineTriggerWorkflowRef(workflowRef, event, repository string) (string, string, error) {
	separator := strings.Index(workflowRef, "@refs/")
	if separator <= 0 {
		return "", "", fmt.Errorf("%s must have <owner>/<repo>/<workflow-path>@<event-ref> format", githubWorkflowRefEnvironment)
	}
	identity, eventRef := workflowRef[:separator], workflowRef[separator+1:]
	parts := strings.SplitN(identity, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("%s must have <owner>/<repo>/<workflow-path>@<event-ref> format", githubWorkflowRefEnvironment)
	}
	provider, owner, name, _, err := parseBuildkiteRepository(repository)
	if err != nil {
		return "", "", fmt.Errorf("BUILDKITE_REPO: %w", err)
	}
	if provider != "github" {
		return "", "", fmt.Errorf("%s requires a github.com BUILDKITE_REPO", githubWorkflowRefEnvironment)
	}
	if !strings.EqualFold(parts[0], owner) || !strings.EqualFold(parts[1], name) {
		return "", "", fmt.Errorf("%s repository %q does not match BUILDKITE_REPO %q", githubWorkflowRefEnvironment, parts[0]+"/"+parts[1], owner+"/"+name)
	}
	if !pipelineTriggerEventRef(event, eventRef) {
		return "", "", fmt.Errorf("%s event ref %q does not match %s %q", githubWorkflowRefEnvironment, eventRef, githubEventNameEnvironment, event)
	}
	return parts[2], eventRef, nil
}

func pipelineTriggerEventRef(event, ref string) bool {
	if ref != strings.TrimSpace(ref) {
		return false
	}
	switch event {
	case "push":
		for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
			if value, ok := strings.CutPrefix(ref, prefix); ok {
				return value != ""
			}
		}
	case "pull_request":
		number, ok := strings.CutPrefix(ref, "refs/pull/")
		if !ok {
			return false
		}
		number, ok = strings.CutSuffix(number, "/merge")
		parsed, err := strconv.Atoi(number)
		return ok && err == nil && parsed > 0 && strconv.Itoa(parsed) == number
	case "issues", "issue_comment":
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		return ok && branch != ""
	}
	return false
}

func parsePluginCacheVolume(value any) (*compiler.CacheVolume, error) {
	cacheObject, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cache must be a JSON object")
	}
	for key := range cacheObject {
		switch key {
		case "paths", "name", "size":
		default:
			return nil, fmt.Errorf("cache contains unknown field %q", key)
		}
	}
	paths, configured := cacheObject["paths"]
	if !configured {
		return nil, fmt.Errorf("cache paths is required")
	}
	cache := &compiler.CacheVolume{}
	var err error
	cache.Paths, err = pluginNonEmptyStringList(paths, "cache paths")
	if err != nil {
		return nil, err
	}
	if name, configured := cacheObject["name"]; configured {
		cache.Name, ok = name.(string)
		if !ok || cache.Name == "" {
			return nil, fmt.Errorf("cache name must be a non-empty string")
		}
	}
	if size, configured := cacheObject["size"]; configured {
		cache.Size, ok = size.(string)
		if !ok || cache.Size == "" {
			return nil, fmt.Errorf("cache size must be a non-empty string")
		}
	}
	if err := buildkitepipeline.ValidateCacheVolume(*cache); err != nil {
		return nil, err
	}
	return cache, nil
}

func parsePluginOIDCConfiguration(value any) (*plan.OIDCConfiguration, error) {
	oidc, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s oidc must be a JSON object", pluginConfigurationEnvironment)
	}
	for key := range oidc {
		switch key {
		case "claims", "aws-session-tags", "subject-claim":
		default:
			return nil, fmt.Errorf("%s oidc contains unknown field %q", pluginConfigurationEnvironment, key)
		}
	}
	configuration := &plan.OIDCConfiguration{}
	var err error
	if claims, configured := oidc["claims"]; configured {
		configuration.Claims, err = pluginNonEmptyStringList(claims, "oidc claims")
		if err != nil {
			return nil, err
		}
	}
	if tags, configured := oidc["aws-session-tags"]; configured {
		configuration.AWSSessionTags, err = pluginNonEmptyStringList(tags, "oidc aws-session-tags")
		if err != nil {
			return nil, err
		}
	}
	if subject, configured := oidc["subject-claim"]; configured {
		configuration.SubjectClaim, ok = subject.(string)
		if !ok || strings.TrimSpace(configuration.SubjectClaim) == "" {
			return nil, fmt.Errorf("%s oidc subject-claim must be a non-empty string", pluginConfigurationEnvironment)
		}
	}
	return configuration, nil
}

func pluginNonEmptyStringList(value any, field string) ([]string, error) {
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return nil, fmt.Errorf("%s %s must be a non-empty array of non-empty strings", pluginConfigurationEnvironment, field)
	}
	result := make([]string, len(values))
	for index, value := range values {
		entry, ok := value.(string)
		if !ok || strings.TrimSpace(entry) == "" {
			return nil, fmt.Errorf("%s %s entry %d must be a non-empty string", pluginConfigurationEnvironment, field, index)
		}
		result[index] = entry
	}
	return result, nil
}

func normalizePluginCommit(ctx context.Context, checkoutPath string, getenv func(string) string, setenv func(string, string) error, runner transport.Runner) error {
	if git.ValidObjectID(getenv("BUILDKITE_COMMIT")) {
		return nil
	}
	output, err := runner.Run(ctx, checkoutPath, "git", []string{"rev-parse", "HEAD"}, nil)
	if err != nil {
		return fmt.Errorf("resolve BUILDKITE_COMMIT from checked-out HEAD: %w", err)
	}
	commit := strings.TrimSpace(string(output))
	if !git.ValidObjectID(commit) {
		return fmt.Errorf("resolve BUILDKITE_COMMIT from checked-out HEAD: git returned an invalid commit")
	}
	if err := setenv("BUILDKITE_COMMIT", commit); err != nil {
		return fmt.Errorf("set resolved BUILDKITE_COMMIT: %w", err)
	}
	return nil
}
