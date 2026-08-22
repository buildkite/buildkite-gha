package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const pluginConfigurationEnvironment = "BUILDKITE_PLUGIN_CONFIGURATION"

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
	if err := normalizePluginCommit(ctx, os.Getenv, os.Setenv, runner); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %v\n", err)
		return 1
	}
	return uploadParsedContext(ctx, parsedUploadArgs{
		workflowOperands:       configuration.Workflows,
		explicitWorkflowPaths:  true,
		checkoutPath:           os.Getenv("BUILDKITE_BUILD_CHECKOUT_PATH"),
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
	} else {
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
	var oidc *plan.OIDCConfiguration
	if value, configured := encoded["oidc"]; configured {
		oidc, err = parsePluginOIDCConfiguration(value)
		if err != nil {
			return pluginConfiguration{}, err
		}
	}
	return pluginConfiguration{Workflows: workflows, ExperimentalRunnerUser: experimentalRunnerUser, OIDC: oidc, runnerTargets: targets}, nil
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
