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
	"strings"
	"sync"
	"syscall"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	legacyRuntimeQueue                = "hosted"
	pluginConfigurationEnvironment    = "BUILDKITE_PLUGIN_CONFIGURATION"
	pluginDevDarwinRuntimeEnvironment = "BUILDKITE_GHA_PLUGIN_DEV_DARWIN_RUNTIME"
	pluginDevLinuxRuntimeEnvironment  = "BUILDKITE_GHA_PLUGIN_DEV_LINUX_RUNTIME"
	legacyTargetQueueEnvironment      = "BUILDKITE_GHA_TARGET_QUEUE"
	legacyRuntimeImageEnvironment     = "BUILDKITE_GHA_RUNTIME_IMAGE"
	hostedProfile                     = "hosted"
	legacyHostedTokenlessProfile      = "hosted-tokenless"
	runtimeDistributionLimit          = 256 << 20
	pluginChecksumLimit               = 4 << 20
	pluginArchiveLimit                = 256 << 20
	maxWebhookMetadataBytes           = 25 << 20
	workflowCheckSummaryLimit         = 65535
	workflowCheckSummaryNotice        = "\n\n_Additional diagnostics omitted at the provider check summary size limit._\n"
	defaultNobleRunnerImage           = "buildkite.namespace-images.com/agent-base@sha256:d8fa20ce298d9ee8673dc6e9ecddabb23ebbff3bcaf828d2011714a960cc5853"
	defaultJammyRunnerImage           = "buildkite.namespace-images.com/agent-base@sha256:3489d7d999869a11a243bdd9f91f318784cb74f0b57646c57424d10008b1e21d"
	defaultMacOSRunnerQueue           = "macos-medium"
)

var runnerQueuePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var runnerImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)
var stableVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

var pluginReleaseBaseURL = "https://github.com/buildkite/buildkite-gha/releases/download"
var pluginHTTPClient = securePluginHTTPClient()

// Run executes the command and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	return run(args, stdout, stderr, version, transport.CommandRunner{Stderr: stderr})
}

func run(args []string, stdout, stderr io.Writer, clientVersion string, agentRunner transport.Runner) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == gharuntime.ContainerProcessHelperCommand {
		return gharuntime.RunContainerProcessHelper(args[1:])
	}
	version := commandVersion(clientVersion)

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
		_, _ = fmt.Fprintf(stdout, "buildkite-gha %s\n", clientVersion)
		return 0
	case "plugin":
		return plugin(args[1:], stdout, stderr, version, clientVersion, agentRunner)
	default:
		if _, ok := commandUsage[args[0]]; ok {
			if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
				writeCommandHelp(stdout, args[0])
				return 0
			}
			switch args[0] {
			case "validate":
				return validate(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			case "validate-batch":
				return validateBatch(args[1:], stderr, version)
			case "compile":
				return compile(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			case "upload":
				return upload(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			case "run-job":
				return runJob(args[1:], stdout, stderr, version, clientVersion, transport.Agent{Runner: agentRunner})
			default:
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: %s: not implemented\n", args[0])
				return 1
			}
		}

		return usageError(stderr, "unknown command %q", args[0])
	}
}

func commandVersion(clientVersion string) string {
	if clientVersion == "dev" || strings.HasPrefix(clientVersion, "dev+") {
		return "dev"
	}
	return clientVersion
}

func plugin(args []string, stdout, stderr io.Writer, version, clientVersion string, runner transport.Runner) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return pluginContext(ctx, args, stdout, stderr, version, clientVersion, runner)
}

func pluginContext(ctx context.Context, args []string, stdout, stderr io.Writer, version, clientVersion string, runner transport.Runner) (code int) {
	started := time.Now()
	details := &commandTelemetryDetails{}
	defer func() {
		outcome := telemetryOutcome(code, "", ctx.Err())
		emitCommandTelemetry(telemetry.CommandPluginImport, outcome, clientVersion, time.Since(started), details.forOutcome(outcome))
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
		runnerTargets:          configuration.runnerTargets,
		oidc:                   configuration.OIDC,
		experimentalRunnerUser: configuration.ExperimentalRunnerUser,
		pluginAcquisition:      &pluginRuntimeAcquisition{version: version},
		importerPlatform:       importerPlatform,
		telemetry:              details,
	}, stdout, stderr, version, transport.Agent{Runner: runner})
}

func importerPlatform(goos, goarch string) (compiler.Platform, error) {
	switch {
	case goos == "linux" && goarch == "amd64":
		return compiler.PlatformLinuxAMD64, nil
	case goos == "darwin" && goarch == "arm64":
		return compiler.PlatformDarwinARM64, nil
	default:
		return compiler.Platform{}, fmt.Errorf("importer requires linux/amd64 or darwin/arm64, running on %s/%s", goos, goarch)
	}
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

func validate(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	args, actionCacheDir, err := validateActionCacheArgs(args)
	if err != nil {
		return usageError(stderr, "validate: %v", err)
	}
	workflowPath, eventPath, eventName, format, profile, allEvents, err := validateArgs(args)
	if err != nil {
		return usageError(stderr, "validate: %v", err)
	}
	if actionCacheDir != "" && profile == "" {
		return usageError(stderr, "validate: --action-cache-dir requires --profile hosted")
	}
	if actionCacheDir != "" {
		if _, err := actionsource.NewStore(actionCacheDir, nil); err != nil {
			return usageError(stderr, "validate: --action-cache-dir: %v", err)
		}
	}
	if profile != "" && eventPath == "" && eventName == "" && !allEvents {
		return usageError(stderr, "validate: --profile hosted requires --event, --event-path, or --all-events; use bare validate <workflow> for event-independent syntax and trigger compatibility validation")
	}
	out := newProcessingOutput(context.Background(), "validate", format, stdout, stderr, agent)
	if allEvents {
		return validateAllEvents(out, workflowPath, version, actionCacheDir, nil, stderr)
	}
	return validateOne(out, workflowPath, eventPath, eventName, profile, version, actionCacheDir, nil, stderr)
}

func validateOne(out processingOutput, workflowPath, eventPath, eventName, profile, version, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
	return validateOneSource(out, workflowPath, nil, eventPath, eventName, profile, version, actionCacheDir, runtime, stderr)
}

func validateOneSource(out processingOutput, workflowPath string, workflowSource []byte, eventPath, eventName, profile, version, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
	contextRequired := false
	var loadEvent func() ([]byte, error)
	if eventPath != "" {
		event, eventErr := os.ReadFile(eventPath)
		if parsedEvent, parseErr := compiler.ParseEvent(event); eventErr == nil && parseErr == nil {
			out.sourceLinks = sourceLinksForEvent(parsedEvent)
		}
		loadEvent = func() ([]byte, error) { return event, eventErr }
	} else if eventName != "" {
		event, eventErr := generatedEventSnapshot(eventName)
		loadEvent = func() ([]byte, error) { return event, eventErr }
	}
	var source, event []byte
	var ok bool
	if workflowSource == nil {
		source, event, ok = loadProcessingInputs(out, workflowPath, profile, "event input could not be read", loadEvent)
	} else {
		source, event, ok = loadProcessingInputsSource(out, workflowPath, profile, workflowSource, "event input could not be read", loadEvent)
	}
	if !ok {
		return 1
	}
	if profile != "" {
		parsed, parseErr := workflow.Parse(workflowPath, source)
		effectiveEvent, eventErr := newEffectiveEvent(event, effectiveEventFromPath, os.Getenv)
		if parseErr == nil && eventErr == nil && !parsed.ReusableOnly() {
			selection, triggerErr := selectWorkflowTrigger(parsed.Triggers, effectiveEvent)
			if triggerErr != nil {
				contextRequired = pathFilterContextRequired(triggerErr)
				if contextRequired {
					triggerErr = buildkitepipeline.ValidateTriggerConditions(parsed.Triggers)
					contextRequired = triggerErr == nil
				}
				if !contextRequired {
					report := triggerFailureProcessingReport(workflowInput{Path: workflowPath, Source: source}, triggerErr)
					_ = out.write(report)
					return 1
				}
			} else if !selection.Applicable {
				report := triggerProcessingReport(workflowPath, source)
				report.Result = "not-applicable"
				if out.write(report) != nil {
					return 1
				}
				return 0
			}
		}
	}
	// Profile validation applies the hosted runner policy so validate and
	// upload agree on supported labels, including the default macOS queue.
	var validationOptions *compiler.Options
	if profile != "" {
		hosted := hostedOptions("", nil, nil)
		validationOptions = &hosted
	}
	processingReport, ok := validatedProcessingReportWithOptions(out, workflowPath, profile, source, event, loadEvent != nil, validationOptions)
	if !ok {
		return 1
	}
	if profile != "" {
		distributionDigest := ""
		var executableErr error
		if runtime != nil {
			distributionDigest, executableErr = runtime.distributionDigest, runtime.executableErr
		} else {
			_, _, distributionDigest, executableErr = executable()
		}
		if executableErr != nil {
			processingReport.AddEnvironmentFailure("compiler executable could not be inspected")
			processingReport.Result = "indeterminate"
			_ = out.write(processingReport)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		// Profile validation never executes generated plans, so the importer
		// digest stands in for every platform's runtime distribution.
		runtimeDistributions := map[compiler.Platform]string{
			compiler.PlatformLinuxAMD64:  distributionDigest,
			compiler.PlatformDarwinARM64: distributionDigest,
		}
		var actionSource compiler.ActionSource
		if runtime != nil {
			actionSource = runtime.actionSource
		}
		preflight, profileErr := compileHostedWithActionCache(ctx, workflowPath, source, event, version, distributionDigest, "buildkite-gha-profile-importer", "", nil, runtimeDistributions, actionCacheDir, actionSource, nil)
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
		if preflight.HasActions {
			processingReport.Diagnostics = append(processingReport.Diagnostics, compatibility.Diagnostic{
				Level: "warning", Code: "W_ACTION_RUNTIME_UNKNOWN", Category: "compatibility", Stage: string(compiler.StageAdmission),
				Message: "Action runtime behavior was not evaluated. The action source, metadata, declared runtime, entrypoints, inputs, and nested actions were validated, but the action code was not executed and may depend on GitHub-only services. No action is required for admission; test the action on Buildkite if runtime compatibility is important.",
			})
		}
		if contextRequired {
			processingReport.SetStage(string(compiler.StageAdmission), compatibility.NotEvaluated)
			processingReport.Admission.Result = compatibility.NotEvaluated
			processingReport.Diagnostics = append(processingReport.Diagnostics, compatibility.Diagnostic{
				Level: "error", Code: compiler.CodeContextRequired, Category: "context", Stage: string(compiler.StageAdmission),
				Message: "Push and pull request path filters require linked Buildkite webhook data and a verified local git diff before admission can be determined.",
			})
			processingReport.Result = "context-required"
			if out.write(processingReport) != nil {
				return 1
			}
			return 1
		}
		processingReport.SetStage(string(compiler.StageAdmission), compatibility.Passed)
		processingReport.Admission.Result = "admitted"
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

type profileValidationRuntime struct {
	actionSource       compiler.ActionSource
	distributionDigest string
	executableErr      error
}

func validateAllEvents(out processingOutput, workflowPath, version, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		validation := compatibility.EnvironmentProcessingReport(workflowPath, "", "workflow input could not be read")
		report := compatibility.NewProcessingReportV3(workflowPath, hostedProfile, validation)
		_ = out.writeV3(report)
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
		return 1
	}
	return validateAllEventsSource(out, workflowPath, source, version, actionCacheDir, runtime, stderr)
}

func validateAllEventsSource(out processingOutput, workflowPath string, source []byte, version, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
	validation, validationErr := compiler.ValidateWithOptions(workflowPath, source, hostedOptions("", nil, nil))
	validationReport := compatibility.InitialProcessingReport(workflowPath, "", false, validation, validationErr)
	if validationErr != nil {
		validationReport.Result = "incompatible"
		report := compatibility.NewProcessingReportV3(workflowPath, hostedProfile, validationReport)
		if out.writeV3(report) != nil {
			return 1
		}
		return 1
	}
	validationReport.Result = "compilable"
	report := compatibility.NewProcessingReportV3(workflowPath, hostedProfile, validationReport)
	parsed, err := workflow.Parse(workflowPath, source)
	if err != nil {
		return 1
	}
	declared := make(map[string]bool, len(parsed.Triggers))
	for _, trigger := range parsed.Triggers {
		declared[trigger.Event] = true
	}
	cleanup := func() {}
	if runtime == nil {
		actionSource, sourceCleanup, sourceErr := newHostedActionSource(actionCacheDir, nil, nil)
		cleanup = sourceCleanup
		if sourceErr != nil {
			validationReport.AddEnvironmentFailure("public action source could not be configured")
			validationReport.Result = "indeterminate"
			report.Validation = validationReport
			_ = out.writeV3(report)
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", sourceErr)
			cleanup()
			return 1
		}
		_, _, distributionDigest, executableErr := executable()
		runtime = &profileValidationRuntime{actionSource: actionSource, distributionDigest: distributionDigest, executableErr: executableErr}
	}
	defer cleanup()
	failed := false
	for _, event := range []string{"push", "pull_request", "merge_group", "release", "workflow_dispatch", "schedule"} {
		if !declared[event] {
			continue
		}
		var eventReport *compatibility.ProcessingReport
		eventOut := processingOutput{
			command: "validate", format: "json", reports: io.Discard, stderr: stderr,
			observe: func(observed compatibility.ProcessingReport) { eventReport = &observed },
		}
		if code := validateOneSource(eventOut, workflowPath, source, "", event, hostedProfile, version, actionCacheDir, runtime, stderr); code != 0 {
			failed = true
		}
		if eventReport == nil {
			return 1
		}
		report.Evaluations = append(report.Evaluations, compatibility.EventEvaluation{
			Event: event, Source: "generated", Report: *eventReport,
		})
	}
	if out.writeV3(report) != nil {
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

func validateActionCacheArgs(args []string) ([]string, string, error) {
	filtered := make([]string, 0, len(args))
	var cacheDir string
	seen := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--action-cache-dir" {
			filtered = append(filtered, args[i])
			continue
		}
		if seen {
			return nil, "", fmt.Errorf("--action-cache-dir may only be specified once")
		}
		seen = true
		i++
		if i == len(args) || strings.TrimSpace(args[i]) == "" {
			return nil, "", fmt.Errorf("--action-cache-dir requires a path")
		}
		cacheDir = args[i]
	}
	return filtered, cacheDir, nil
}

func validateArgs(args []string) (workflowPath, eventPath, eventName, format, profile string, allEvents bool, err error) {
	format = "text"
	formatSeen := false
	profileSeen := false
	eventPathSeen := false
	eventSeen := false
	allEventsSeen := false
	for i := 0; i < len(args); i++ {
		option := args[i]
		switch option {
		case "--format", "--profile", "--event-path", "--event":
			seen := map[string]bool{"--format": formatSeen, "--profile": profileSeen, "--event-path": eventPathSeen, "--event": eventSeen}[option]
			if seen {
				return "", "", "", "", "", false, fmt.Errorf("%s may only be specified once", option)
			}
			i++
			if i == len(args) {
				return "", "", "", "", "", false, fmt.Errorf("%s requires a value", option)
			}
		case "--all-events":
			if allEventsSeen {
				return "", "", "", "", "", false, fmt.Errorf("--all-events may only be specified once")
			}
			allEventsSeen = true
			allEvents = true
			continue
		case "-h", "--help":
			return "", "", "", "", "", false, fmt.Errorf("help must be requested immediately after the command")
		default:
			if strings.HasPrefix(option, "-") {
				return "", "", "", "", "", false, fmt.Errorf("unknown option %q", option)
			}
			if workflowPath != "" {
				return "", "", "", "", "", false, fmt.Errorf("expected one workflow path")
			}
			workflowPath = option
			continue
		}
		switch option {
		case "--format":
			formatSeen = true
			format = args[i]
			if format != "text" && format != "json" {
				return "", "", "", "", "", false, fmt.Errorf("--format must be text or json")
			}
		case "--profile":
			profileSeen = true
			profile = args[i]
			if profile != hostedProfile && profile != legacyHostedTokenlessProfile {
				return "", "", "", "", "", false, fmt.Errorf("--profile must be %q", hostedProfile)
			}
			profile = hostedProfile
		case "--event-path":
			eventPathSeen = true
			eventPath = args[i]
		case "--event":
			eventSeen = true
			eventName = args[i]
		}
	}
	if workflowPath == "" {
		return "", "", "", "", "", false, fmt.Errorf("workflow path is required")
	}
	if eventPathSeen && eventSeen {
		return "", "", "", "", "", false, fmt.Errorf("--event and --event-path are mutually exclusive")
	}
	if eventSeen && profile == "" {
		return "", "", "", "", "", false, fmt.Errorf("--event requires --profile hosted")
	}
	if eventSeen && !slices.Contains([]string{"push", "pull_request", "merge_group", "release", "workflow_dispatch", "schedule"}, eventName) {
		return "", "", "", "", "", false, fmt.Errorf("unsupported --event %q; supported events are push, pull_request, merge_group, release, workflow_dispatch, and schedule", eventName)
	}
	if allEvents && (eventPathSeen || eventSeen) {
		return "", "", "", "", "", false, fmt.Errorf("--all-events is mutually exclusive with --event and --event-path")
	}
	if allEvents && profile == "" {
		return "", "", "", "", "", false, fmt.Errorf("--all-events requires --profile hosted")
	}
	return workflowPath, eventPath, eventName, format, profile, allEvents, nil
}

func generatedEventSnapshot(name string) ([]byte, error) {
	event := struct {
		Provider   string              `json:"provider"`
		Event      string              `json:"event"`
		Repository compiler.Repository `json:"repository"`
		Ref        string              `json:"ref"`
		SHA        string              `json:"sha"`
		Actor      string              `json:"actor"`
		Payload    map[string]any      `json:"payload"`
	}{
		Provider: "github", Event: name,
		Repository: compiler.Repository{Owner: "example", Name: "repository", CloneURL: "https://github.com/example/repository.git", DefaultBranch: "main"},
		Ref:        "refs/heads/main", SHA: strings.Repeat("0", 40), Actor: "buildkite-gha", Payload: map[string]any{},
	}
	switch name {
	case "push":
		event.Payload["ref"] = event.Ref
	case "pull_request":
		event.Ref = "refs/pull/1/merge"
		event.Payload["action"] = "opened"
		event.Payload["number"] = 1
		event.Payload["pull_request"] = map[string]any{
			"number": 1,
			"base":   map[string]any{"ref": "main"},
			"head":   map[string]any{"ref": "example"},
		}
	case "merge_group":
		event.Ref = "refs/heads/gh-readonly-queue/main/pr-1-deadbeef"
		event.Payload["action"] = "checks_requested"
		event.Payload["merge_group"] = map[string]any{
			"head_ref": event.Ref,
			"head_sha": event.SHA,
			"base_ref": "refs/heads/main",
			"base_sha": strings.Repeat("1", 40),
		}
	case "release":
		event.Ref = "refs/tags/v1.0.0"
		event.Payload["action"] = "published"
		event.Payload["release"] = map[string]any{
			"tag_name":   "v1.0.0",
			"draft":      false,
			"prerelease": false,
		}
	case "schedule":
		event.Payload["schedule"] = "0 0 * * *"
	case "workflow_dispatch":
	default:
		return nil, fmt.Errorf("unsupported generated event %q; supported events are push, pull_request, merge_group, release, workflow_dispatch, and schedule", name)
	}
	return json.Marshal(event)
}

func compile(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	workflowPath, eventPath, format, err := compileArgs(args)
	if err != nil {
		return usageError(stderr, "compile: %v", err)
	}
	if eventPath == "" {
		return usageError(stderr, "compile: --event-path is required")
	}
	out := newProcessingOutput(context.Background(), "compile", "text", stderr, stderr, agent)
	event, eventErr := os.ReadFile(eventPath)
	if parsedEvent, parseErr := compiler.ParseEvent(event); eventErr == nil && parseErr == nil {
		out.sourceLinks = sourceLinksForEvent(parsedEvent)
	}
	source, event, ok := loadProcessingInputs(out, workflowPath, "", "event input could not be read", func() ([]byte, error) { return event, eventErr })
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
	platform, err := importerPlatform(goos, goarch)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	uploadArguments, err := parseUploadArgs(args)
	if err != nil {
		return usageError(stderr, "upload: %v", err)
	}
	uploadArguments.importerPlatform = platform
	return uploadParsed(uploadArguments, stdout, stderr, version, agent)
}

func uploadParsed(uploadArguments parsedUploadArgs, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return uploadParsedContext(ctx, uploadArguments, stdout, stderr, version, agent)
}

func uploadParsedContext(ctx context.Context, uploadArguments parsedUploadArgs, stdout, stderr io.Writer, version string, agent transport.Agent) int {
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
	out := newProcessingOutput(ctx, "upload", "text", stderr, stderr, agent)
	out.plugin = uploadArguments.pluginAcquisition != nil
	if uploadArguments.telemetry != nil {
		out.observe = uploadArguments.telemetry.observe
	}
	var eventSource []byte
	var eventOrigin effectiveEventOrigin
	var eventLoadErr error
	if eventPath != "" {
		eventSource, eventOrigin, eventLoadErr = loadEffectiveEventSource(ctx, eventPath, agent)
		if parsedEvent, parseErr := compiler.ParseEvent(eventSource); eventLoadErr == nil && parseErr == nil {
			out.sourceLinks = sourceLinksForEvent(parsedEvent)
		}
	} else if buildEvent, buildEventErr := buildkiteEventSource(os.Getenv); buildEventErr == nil {
		if parsedEvent, parseErr := compiler.ParseEvent(buildEvent); parseErr == nil {
			out.sourceLinks = sourceLinksForEvent(parsedEvent)
		}
	}
	var workflows []workflowInput
	var err error
	if uploadArguments.explicitWorkflowPaths {
		workflows, err = expandExplicitWorkflowPaths(workflowOperands)
	} else {
		workflows, err = resolveWorkflowOperands(workflowOperands)
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
		_, _ = fmt.Fprintln(stderr, "buildkite-gha: upload: workflow paths matched only reusable workflow_call workflows; there is nothing to upload")
		return 1
	}
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
	if eventPath == "" {
		eventSource, eventOrigin, eventLoadErr = loadEffectiveEventSource(ctx, eventPath, agent)
	}
	if eventLoadErr != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				return out.fail(compatibility.EventInputProcessingReport(input.Path, hostedProfile, input.Source, "event input could not be acquired"), eventLoadErr)
			}
		}
		return 1
	}
	effectiveEvent, eventParseErr := newEffectiveEvent(eventSource, eventOrigin, os.Getenv)
	if eventParseErr != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				_, _ = validatedProcessingReport(out, input.Path, hostedProfile, input.Source, eventSource, true)
				return 1
			}
		}
		return 1
	}
	populateChangedPaths(&effectiveEvent.TriggerSnapshot, effectiveEvent.Event, effectiveEvent.Origin, workflows)
	processingReports := make([]compatibility.ProcessingReport, len(workflows))
	for i := range workflows {
		if workflows[i].ReusableOnly {
			continue
		}
		selection, triggerErr := selectWorkflowTrigger(workflows[i].Triggers, effectiveEvent)
		if triggerErr != nil {
			workflows[i].Applicable = true
			workflows[i].TriggerCondition = effectiveEvent.TriggerExpressions.EventPredicate
			processingReports[i] = triggerFailureProcessingReport(workflows[i], triggerErr)
			continue
		}
		if workflows[i].PathFiltersError != "" && selection.AnnotationReason == "" {
			workflows[i].Applicable = true
			workflows[i].TriggerCondition = effectiveEvent.TriggerExpressions.EventPredicate
			processingReports[i] = triggerFailureProcessingReport(workflows[i], &buildkitepipeline.UnsupportedPathFiltersError{
				Event: effectiveEvent.Event.Event, Reason: workflows[i].PathFiltersError,
			})
			continue
		}
		workflows[i].Applicable = selection.Applicable
		workflows[i].TriggerCondition = selection.Condition
		workflows[i].SkipReason = selection.SkipReason
		workflows[i].AnnotationReason = selection.AnnotationReason
	}
	for i, input := range workflows {
		if !input.Applicable || processingReportHasErrors(processingReports[i]) {
			continue
		}
		validationOptions := hostedOptions("", uploadArguments.runnerTargets, nil)
		validationOptions.StepKeyNamespace = input.StepKeyNamespace
		validation, validationErr := compiler.ValidateEventWithOptions(input.Path, input.Source, effectiveEvent.Source, validationOptions)
		processingReports[i] = compatibility.InitialProcessingReport(input.Path, hostedProfile, true, validation, validationErr)
		if validationErr != nil {
			processingReports[i].Result = "incompatible"
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
	importerDistribution := runtimeDistribution{contents: executableContents, digest: distributionDigest}
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
	if uploadArguments.pluginAcquisition != nil {
		runtimeDistributions, acquireErr := uploadArguments.pluginAcquisition.acquire(ctx, requiredPlatforms, uploadArguments.importerPlatform, importerDistribution)
		if acquireErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: plugin: %v\n", acquireErr)
			return 1
		}
		return finishUpload(ctx, uploadArguments, stdout, stderr, version, agent, workflows, effectiveEvent, executablePath, distributionDigest, importerStep, processingReports, out, runtimeDistributions)
	}
	requiredDistributionPaths := make(map[compiler.Platform]string, len(requiredPlatforms))
	for platform := range requiredPlatforms {
		if path, ok := uploadArguments.runtimeDistributionPaths[platform]; ok {
			requiredDistributionPaths[platform] = path
		}
	}
	configuredDistributions, err := loadRuntimeDistributions(requiredDistributionPaths)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	runtimeDistributions := make(map[compiler.Platform]runtimeDistribution, len(requiredPlatforms))
	for _, platform := range []compiler.Platform{compiler.PlatformLinuxAMD64, compiler.PlatformDarwinARM64} {
		if !requiredPlatforms[platform] {
			continue
		}
		if configured, ok := configuredDistributions[platform]; ok {
			runtimeDistributions[platform] = configured
			continue
		}
		if platform == uploadArguments.importerPlatform {
			runtimeDistributions[platform] = importerDistribution
			continue
		}
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: runtime distribution for %s is required by the selected workflows\n", platform)
		return 1
	}
	return finishUpload(ctx, uploadArguments, stdout, stderr, version, agent, workflows, effectiveEvent, executablePath, distributionDigest, importerStep, processingReports, out, runtimeDistributions)
}

func finishUpload(ctx context.Context, uploadArguments parsedUploadArgs, stdout, stderr io.Writer, version string, agent transport.Agent, workflows []workflowInput, effectiveEvent effectiveEventSelection, executablePath, distributionDigest, importerStep string, processingReports []compatibility.ProcessingReport, out processingOutput, runtimeDistributions map[compiler.Platform]runtimeDistribution) int {
	runtimeDigests := make(map[compiler.Platform]string, len(runtimeDistributions))
	for platform, runtimeDistribution := range runtimeDistributions {
		runtimeDigests[platform] = runtimeDistribution.digest
	}
	authentication := importerJobActionSourceAuthentication(stderr)
	generatedWorkflows := make([]buildkitepipeline.Workflow, 0, len(workflows))
	skippedWorkflows := make([]skippedWorkflow, 0)
	planArtifacts := make([]compiler.PlanArtifact, 0)
	failureArtifacts := make([]transport.Artifact, 0)
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
			groupKey := "gha-workflow-" + input.Identity
			generatedWorkflows = append(generatedWorkflows, buildkitepipeline.Workflow{
				GroupLabel: label,
				GroupKey:   groupKey,
				Event:      effectiveEvent.Event.Event,
				SkipReason: input.SkipReason,
			})
			skippedWorkflows = append(skippedWorkflows, skippedWorkflow{label: label, key: groupKey, reason: input.AnnotationReason})
			continue
		}
		if processingReportHasErrors(processingReports[i]) {
			failed, artifacts := failedGeneratedWorkflow(input, effectiveEvent.Event.Event, processingReports[i], out.sourceLinks)
			generatedWorkflows = append(generatedWorkflows, failed)
			failureArtifacts = append(failureArtifacts, artifacts...)
			continue
		}
		preflight, err := compileHostedNamespaced(ctx, input.Path, input.Source, effectiveEvent.Source, version, distributionDigest, importerStep, "", uploadArguments.runnerTargets, runtimeDigests, input.StepKeyNamespace, uploadArguments.oidc, authentication)
		applyHostedPreflight(&processingReports[i], preflight)
		if err != nil {
			processingReports[i].Result = classifyHostedFailure(&processingReports[i], input.Path, err)
			var failure *hostedFailure
			if errors.As(err, &failure) && failure.Kind == hostedEvaluationFailure {
				failed, artifacts := failedGeneratedWorkflow(input, effectiveEvent.Event.Event, processingReports[i], out.sourceLinks)
				generatedWorkflows = append(generatedWorkflows, failed)
				failureArtifacts = append(failureArtifacts, artifacts...)
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
		generated.Event = effectiveEvent.Event.Event
		generated.Condition = input.TriggerCondition
		generatedWorkflows = append(generatedWorkflows, generated)
		if input.AnnotationReason != "" {
			skippedWorkflows = append(skippedWorkflows, skippedWorkflow{label: label, key: generated.GroupKey, reason: input.AnnotationReason})
		}
		planArtifacts = append(planArtifacts, bundle.Plans...)
		jobCount += len(bundle.Plans)
		processingReports[i].SetStage(string(compiler.StageAdmission), compatibility.Passed)
		processingReports[i].Admission.Result = "admitted"
		processingReports[i].Result = "admitted"
		writeCompilerWarnings(stderr, "upload", input.CanonicalPath, bundle.IR.Warnings)
		if uploadArguments.telemetry != nil {
			uploadArguments.telemetry.addWarnings(bundle.IR.Warnings)
			if bundleRunsUnprovenActions(bundle) {
				uploadArguments.telemetry.addActionRuntimeUnknown()
			}
		}
	}
	aggregatePipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		CompilerStep:      importerStep,
		EventProvider:     effectiveEvent.Event.Provider,
		DisableRunnerUser: !uploadArguments.experimentalRunnerUser,
		Workflows:         generatedWorkflows,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: emit aggregate Buildkite pipeline: %v\n", err)
		return 1
	}
	for i, input := range workflows {
		if input.Applicable {
			if uploadArguments.telemetry != nil {
				uploadArguments.telemetry.addReportDiagnostics(processingReports[i])
			}
			if out.plugin {
				_ = writePluginProcessing(stdout, processingReports[i])
			} else {
				_ = compatibility.WriteProcessing(stdout, "text", processingReports[i])
			}
			if !processingReportHasErrors(processingReports[i]) {
				out.annotate(processingReports[i])
			}
		}
	}
	artifacts := make([]transport.Artifact, 0, len(runtimeDistributions)+len(planArtifacts)+len(failureArtifacts))
	artifactPaths := make(map[string]struct{}, cap(artifacts))
	for _, artifact := range failureArtifacts {
		if _, exists := artifactPaths[artifact.Path]; exists {
			continue
		}
		artifactPaths[artifact.Path] = struct{}{}
		artifacts = append(artifacts, artifact)
	}
	for _, platform := range []compiler.Platform{compiler.PlatformLinuxAMD64, compiler.PlatformDarwinARM64} {
		runtimeDistribution, ok := runtimeDistributions[platform]
		if !ok {
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
	if len(artifacts) == 0 {
		if err := agent.UploadPipeline(ctx, aggregatePipeline); err != nil {
			if uploadArguments.telemetry != nil {
				uploadArguments.telemetry.setFailurePhase(telemetry.FailurePhasePipelineUpload)
			}
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: upload pipeline: %v\n", err)
			return 1
		}
		out.annotateSkippedWorkflows(effectiveEvent.Event.Event, skippedWorkflows)
		_, _ = fmt.Fprintf(stdout, "Uploaded %d jobs from %d workflows using %s with importer %s.\n", jobCount, len(generatedWorkflows), executablePath, importerStep)
		return 0
	}
	root, err := os.MkdirTemp("", "buildkite-gha-upload-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: create artifact root: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	if err := transport.UploadArtifacts(ctx, agent, root, artifacts, aggregatePipeline); err != nil {
		if uploadArguments.telemetry != nil {
			phase := telemetry.FailurePhaseArtifactUpload
			if errors.Is(err, transport.ErrPipelineUpload) {
				phase = telemetry.FailurePhasePipelineUpload
			}
			uploadArguments.telemetry.setFailurePhase(phase)
		}
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	out.annotateSkippedWorkflows(effectiveEvent.Event.Event, skippedWorkflows)
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

func failedGeneratedWorkflow(input workflowInput, event string, report compatibility.ProcessingReport, sourceLinks sourceLinkContext) (buildkitepipeline.Workflow, []transport.Artifact) {
	label := input.Name
	if label == "" {
		label = input.CanonicalPath
	}
	report.Diagnostics = append([]compatibility.Diagnostic(nil), report.Diagnostics...)
	report.Finalize()
	messages := make([]string, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
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
		if diagnostic.Detail != "" {
			message += "\n  detail: " + diagnostic.Detail
		}
		messages = append(messages, message)
	}
	_, annotation := processingAnnotation(report, sourceLinks)
	_, checkSummary := processingAnnotationWithin(report, sourceLinks, workflowCheckSummaryLimit, workflowCheckSummaryNotice, false)
	messageArtifact := generatedFailureArtifact("messages", ".txt", "\x1b[31m"+strings.Join(messages, "\n")+"\x1b[0m\n")
	annotationArtifact := generatedFailureArtifact("annotations", ".html", annotation)
	workflow := buildkitepipeline.Workflow{
		GroupLabel: label,
		GroupKey:   "gha-workflow-" + input.Identity,
		Event:      event,
		Failure: &buildkitepipeline.Failure{
			AnnotationPath: annotationArtifact.Path,
			MessagePath:    messageArtifact.Path,
			Summary:        checkSummary,
		},
	}
	return workflow, []transport.Artifact{messageArtifact, annotationArtifact}
}

func generatedFailureArtifact(kind, extension, contents string) transport.Artifact {
	encoded := []byte(contents)
	digest := transport.Digest(encoded)
	path := ".buildkite-gha/failures/" + kind + "/" + strings.TrimPrefix(digest, "sha256:") + extension
	return transport.Artifact{Path: path, Digest: digest, Contents: encoded}
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
	Source             []byte
	Event              compiler.Event
	Origin             effectiveEventOrigin
	TriggerExpressions buildkitepipeline.TriggerConditionExpressions
	TriggerSnapshot    buildkitepipeline.TriggerEventSnapshot
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
	expressions, snapshot := snapshotTriggerState(event)
	effective := effectiveEventSelection{
		Source: source, Event: event, Origin: origin,
		TriggerExpressions: expressions,
		TriggerSnapshot:    snapshot,
	}
	if origin == effectiveEventFromPath {
		return effective, nil
	}
	predicate := "true"
	if buildSource := strings.TrimSpace(getenv("BUILDKITE_SOURCE")); buildSource != "" {
		predicate = "build.source == " + triggerConditionLiteral(buildSource)
	}
	effective.TriggerExpressions.EventPredicate = predicate
	return effective, nil
}

func snapshotTriggerState(event compiler.Event) (buildkitepipeline.TriggerConditionExpressions, buildkitepipeline.TriggerEventSnapshot) {
	expressions := buildkitepipeline.TriggerConditionExpressions{
		EventPredicate:        "true",
		Branch:                "null",
		Tag:                   "null",
		PullRequestBaseBranch: "null",
		PullRequestAction:     "null",
		MergeGroupBaseBranch:  "null",
		MergeGroupAction:      "null",
		ReleaseAction:         "null",
	}
	snapshot := buildkitepipeline.TriggerEventSnapshot{}
	if branch, ok := strings.CutPrefix(event.Ref, "refs/heads/"); ok {
		expressions.Branch = triggerConditionLiteral(branch)
		snapshot.Branch = &branch
	}
	if tag, ok := strings.CutPrefix(event.Ref, "refs/tags/"); ok {
		expressions.Tag = triggerConditionLiteral(tag)
		snapshot.Tag = &tag
	}
	if action, ok := event.Payload["action"].(string); ok && strings.TrimSpace(action) != "" {
		expressions.PullRequestAction = triggerConditionLiteral(action)
		snapshot.PullRequestAction = &action
		expressions.MergeGroupAction = triggerConditionLiteral(action)
		snapshot.MergeGroupAction = &action
		expressions.ReleaseAction = triggerConditionLiteral(action)
		snapshot.ReleaseAction = &action
	}
	if pullRequest, ok := event.Payload["pull_request"].(map[string]any); ok {
		if base, ok := pullRequest["base"].(map[string]any); ok {
			if branch, ok := base["ref"].(string); ok && strings.TrimSpace(branch) != "" {
				expressions.PullRequestBaseBranch = triggerConditionLiteral(branch)
				snapshot.PullRequestBaseBranch = &branch
			}
		}
	}
	if mergeGroup, ok := event.Payload["merge_group"].(map[string]any); ok {
		if baseRef, ok := mergeGroup["base_ref"].(string); ok {
			if branch, ok := strings.CutPrefix(baseRef, "refs/heads/"); ok && branch != "" {
				expressions.MergeGroupBaseBranch = triggerConditionLiteral(branch)
				snapshot.MergeGroupBaseBranch = &branch
			}
		}
	}
	return expressions, snapshot
}

type workflowTriggerSelection struct {
	Condition, SkipReason, AnnotationReason string
	Applicable                              bool
}

func selectWorkflowTrigger(triggers []workflow.Trigger, event effectiveEventSelection) (workflowTriggerSelection, error) {
	condition, applicable, err := buildkitepipeline.TranslateEventTriggerCondition(triggers, event.Event.Event, event.TriggerExpressions, event.TriggerSnapshot)
	if err != nil {
		return workflowTriggerSelection{}, err
	}
	annotationReason := buildkitepipeline.TriggerEventSkipReason(triggers, event.Event.Event)
	if applicable {
		annotationReason, err = buildkitepipeline.TriggerFilterMismatchReason(triggers, event.Event.Event, event.TriggerSnapshot)
		if err != nil {
			return workflowTriggerSelection{}, err
		}
	}
	return workflowTriggerSelection{
		Condition:        condition,
		Applicable:       applicable,
		SkipReason:       buildkitepipeline.TriggerEventSkipReason(triggers, event.Event.Event),
		AnnotationReason: annotationReason,
	}, nil
}

func pathFilterContextRequired(err error) bool {
	var pathFilters *buildkitepipeline.UnsupportedPathFiltersError
	return errors.As(err, &pathFilters) && (pathFilters.Event == "push" || pathFilters.Event == "pull_request") && pathFilters.Reason == ""
}

func triggerConditionLiteral(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func triggerFailureProcessingReport(input workflowInput, err error) compatibility.ProcessingReport {
	report := triggerProcessingReport(input.Path, input.Source)
	var pathFilters *buildkitepipeline.UnsupportedPathFiltersError
	if errors.As(err, &pathFilters) {
		message := fmt.Sprintf("%s trigger path filters cannot be translated safely. Remove paths and paths-ignore from this trigger, or move the filtering into a job or step.", upperFirst(pathFilters.Event))
		if pathFilters.Reason != "" {
			history := strings.ReplaceAll(pathFilters.Event, "_", "-")
			message = fmt.Sprintf("%s trigger path filters could not be evaluated safely. Ensure the linked webhook and local checkout contain matching %s history, or remove the path filters.", upperFirst(pathFilters.Event), history)
		}
		err = &compiler.ProcessingFinding{
			Stage: compiler.StagePipeline, Code: compiler.CodePipelineGeneration, Category: "compatibility",
			Path: input.Path, Line: 1, Column: 1,
			Message: message,
			Detail:  pathFilters.Error(), Err: err,
		}
	}
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
	TriggerCondition, SkipReason, AnnotationReason        string
	PathFiltersError                                      string
	ReusableOnly, Applicable                              bool
}

func resolveWorkflowOperands(operands []string) ([]workflowInput, error) {
	if len(operands) != 1 {
		return expandExplicitWorkflowPaths(operands)
	}
	path, err := filepath.Abs(operands[0])
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path %q: %w", operands[0], err)
	}
	if err := requireRegularWorkflowFile(path, operands[0]); err != nil {
		return expandExplicitWorkflowPaths(operands)
	}
	extension := filepath.Ext(path)
	if extension != ".yml" && extension != ".yaml" {
		return nil, fmt.Errorf("workflow path %q must end in .yml or .yaml", operands[0])
	}
	canonical := filepath.ToSlash(filepath.Clean(operands[0]))
	if rootBytes, rootErr := exec.Command("git", "rev-parse", "--show-toplevel").Output(); rootErr == nil {
		root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
		if relative, relativeErr := filepath.Rel(root, path); relativeErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			canonical = filepath.ToSlash(filepath.Clean(relative))
		}
	}
	return workflowInputs([]workflowInput{{Path: path, CanonicalPath: canonical}}, false)
}

func expandExplicitWorkflowPaths(operands []string) ([]workflowInput, error) {
	if len(operands) == 0 {
		return nil, fmt.Errorf("workflow path is required")
	}
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
// Keep version-specific and organization-provided targets out of this preset.
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
			actionSource, cleanup, err = newHostedActionSource(actionCacheDir, sourceOptions, nil)
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

func newHostedActionSource(actionCacheDir string, resolverOptions, storeOptions []actionsource.Option) (compiler.ActionSource, func(), error) {
	actionSource, cleanup, _, err := newHostedActionSourceWithSnapshot(actionCacheDir, resolverOptions, storeOptions)
	return actionSource, cleanup, err
}

func newHostedActionSourceWithSnapshot(actionCacheDir string, resolverOptions, storeOptions []actionsource.Option) (compiler.ActionSource, func(), string, error) {
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
	store, err := actionsource.NewStore(actionRoot, nil, storeOptions...)
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
	case strings.Contains(reason, "job-level permissions"):
		limitation = "job-level permissions are unsupported for hosted GITHUB_TOKEN issuance"
		fix = "Move the permissions map to the workflow top level."
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

type parsedUploadArgs struct {
	workflowOperands         []string
	explicitWorkflowPaths    bool
	eventPath                string
	runtimeDistributionPaths map[compiler.Platform]string
	runnerTargets            map[string]compiler.RunnerTarget
	oidc                     *plan.OIDCConfiguration
	experimentalRunnerUser   bool
	pluginAcquisition        *pluginRuntimeAcquisition
	importerPlatform         compiler.Platform
	telemetry                *commandTelemetryDetails
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
	experimentalRunnerUser := true
	experimentalRunnerUserSeen := false
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
		if args[i] == "--experimental-runner-user" || strings.HasPrefix(args[i], "--experimental-runner-user=") {
			if experimentalRunnerUserSeen {
				return parsedUploadArgs{}, fmt.Errorf("--experimental-runner-user may only be specified once")
			}
			experimentalRunnerUserSeen = true
			value, configured := strings.CutPrefix(args[i], "--experimental-runner-user=")
			if configured {
				if value != "true" && value != "false" {
					return parsedUploadArgs{}, fmt.Errorf("--experimental-runner-user must be true or false")
				}
				experimentalRunnerUser = value == "true"
			}
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
	return parsedUploadArgs{workflowOperands: workflowOperands, eventPath: eventPath, runtimeDistributionPaths: runtimeDistributionPaths, runnerTargets: runnerTargets, experimentalRunnerUser: experimentalRunnerUser}, nil
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
		// Version-specific macOS labels require an organization-provided queue.
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

const (
	pluginLinuxAsset  = "buildkite-gha_Linux_x86_64.tar.gz"
	pluginDarwinAsset = "buildkite-gha_Darwin_arm64.tar.gz"
)

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

func (a *pluginRuntimeAcquisition) acquire(ctx context.Context, required map[compiler.Platform]bool, hostPlatform compiler.Platform, host runtimeDistribution) (map[compiler.Platform]runtimeDistribution, error) {
	distributions := make(map[compiler.Platform]runtimeDistribution, len(required))
	devPaths := map[compiler.Platform]string{
		compiler.PlatformLinuxAMD64:  os.Getenv(pluginDevLinuxRuntimeEnvironment),
		compiler.PlatformDarwinARM64: os.Getenv(pluginDevDarwinRuntimeEnvironment),
	}
	if a.version != "dev" {
		for platform, path := range devPaths {
			if path != "" {
				return nil, fmt.Errorf("%s is test/dev-only and is rejected by release builds", pluginDevRuntimeEnvironment(platform))
			}
		}
	}
	for _, platform := range []compiler.Platform{compiler.PlatformLinuxAMD64, compiler.PlatformDarwinARM64} {
		if !required[platform] {
			continue
		}
		if platform == hostPlatform {
			distributions[platform] = host
			continue
		}
		if a.version == "dev" {
			path := devPaths[platform]
			if path == "" {
				return nil, fmt.Errorf("%s is required for a dev build using %s", pluginDevRuntimeEnvironment(platform), platform)
			}
			loaded, err := loadRuntimeDistributions(map[compiler.Platform]string{platform: path})
			if err != nil {
				return nil, err
			}
			distributions[platform] = loaded[platform]
			continue
		}
		if !stableVersionPattern.MatchString(a.version) {
			return nil, fmt.Errorf("running version %q is not a stable semantic version", a.version)
		}
		distribution, err := acquirePluginRuntime(ctx, a.version, platform, pluginHTTPClient, pluginReleaseBaseURL, pluginArchiveCachePath(a.version, pluginRuntimeAsset(platform)))
		if err != nil {
			return nil, err
		}
		distributions[platform] = distribution
	}
	return distributions, nil
}

func pluginDevRuntimeEnvironment(platform compiler.Platform) string {
	if platform == compiler.PlatformDarwinARM64 {
		return pluginDevDarwinRuntimeEnvironment
	}
	return pluginDevLinuxRuntimeEnvironment
}

func pluginRuntimeAsset(platform compiler.Platform) string {
	if platform == compiler.PlatformDarwinARM64 {
		return pluginDarwinAsset
	}
	return pluginLinuxAsset
}

func acquirePluginRuntime(ctx context.Context, version string, platform compiler.Platform, client *http.Client, baseURL, cachePath string) (runtimeDistribution, error) {
	asset := pluginRuntimeAsset(platform)
	checksums, err := downloadPluginReleaseFile(ctx, client, baseURL+"/v"+version+"/checksums.txt", pluginChecksumLimit)
	if err != nil {
		return runtimeDistribution{}, fmt.Errorf("download release checksums: %w", err)
	}
	expected, err := pluginAssetChecksum(checksums, asset)
	if err != nil {
		return runtimeDistribution{}, err
	}
	archive := readVerifiedPluginArchiveCache(cachePath, expected)
	if archive == nil {
		archive, err = downloadPluginReleaseFile(ctx, client, baseURL+"/v"+version+"/"+asset, pluginArchiveLimit)
		if err != nil {
			return runtimeDistribution{}, fmt.Errorf("download %s runtime: %w", platform, err)
		}
		if sha256.Sum256(archive) != expected {
			return runtimeDistribution{}, fmt.Errorf("%s archive checksum verification failed", platform)
		}
		writePluginArchiveCache(cachePath, archive)
	}
	contents, err := extractPluginRuntime(archive, platform)
	if err != nil {
		return runtimeDistribution{}, err
	}
	if err := validateRuntimeDistributionBinary(platform, contents); err != nil {
		return runtimeDistribution{}, fmt.Errorf("validate %s runtime: %w", platform, err)
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

func extractPluginRuntime(archive []byte, platform compiler.Platform) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open %s archive: %w", platform, err)
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
			return nil, fmt.Errorf("read %s archive: %w", platform, err)
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > runtimeDistributionLimit {
			return nil, fmt.Errorf("%s archive contains an unsafe member %q", platform, header.Name)
		}
		switch header.Name {
		case "buildkite-gha":
			if seenBinary || header.Mode&0o111 == 0 {
				return nil, fmt.Errorf("%s archive has an invalid executable member", platform)
			}
			seenBinary = true
			executable, err = io.ReadAll(io.LimitReader(tarReader, runtimeDistributionLimit+1))
			if err != nil || int64(len(executable)) != header.Size {
				return nil, fmt.Errorf("read %s executable", platform)
			}
		case "LICENSE":
			if seenLicense {
				return nil, fmt.Errorf("%s archive contains duplicate LICENSE", platform)
			}
			seenLicense = true
		default:
			return nil, fmt.Errorf("%s archive contains unexpected member %q", platform, header.Name)
		}
	}
	if !seenBinary || !seenLicense {
		return nil, fmt.Errorf("%s archive must contain exactly buildkite-gha and LICENSE", platform)
	}
	return executable, nil
}

func pluginArchiveCachePath(version, asset string) string {
	root := os.Getenv("MISE_DATA_DIR")
	if !filepath.IsAbs(root) {
		return ""
	}
	return filepath.Join(root, "buildkite-gha", "releases", version, asset)
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
