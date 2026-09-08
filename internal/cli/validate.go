package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
	"github.com/buildkite/buildkite-gha/internal/workflowprocessing"
)

func validate(args []string, stdout, stderr io.Writer, clientVersion string, agent transport.Agent) int {
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := newProcessingOutput(ctx, "validate", format, stdout, stderr, agent)
	if allEvents {
		return validateAllEvents(ctx, out, workflowPath, clientVersion, actionCacheDir, nil, stderr)
	}
	return validateOne(ctx, out, workflowPath, eventPath, eventName, profile, clientVersion, actionCacheDir, nil, stderr)
}

func validateOne(ctx context.Context, out processingOutput, workflowPath, eventPath, eventName, profile, clientVersion, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
	return validateOneSource(ctx, out, workflowPath, nil, eventPath, eventName, profile, clientVersion, actionCacheDir, runtime, stderr)
}

func validateOneSource(ctx context.Context, out processingOutput, workflowPath string, workflowSource []byte, eventPath, eventName, profile, clientVersion, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
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
		source, event, ok = loadProcessingInputs(ctx, out, workflowPath, profile, "event input could not be read", loadEvent)
	} else {
		source, event, ok = loadProcessingInputsSource(ctx, out, workflowPath, profile, workflowSource, "event input could not be read", loadEvent)
	}
	if !ok {
		return 1
	}
	repositorySource := compiler.RepositorySource(nil)
	if runtime != nil {
		repositorySource = runtime.actionSource
	}
	cleanupSource := func() {}
	if repositorySource == nil {
		var sourceErr error
		repositorySource, cleanupSource, sourceErr = newHostedActionSource(ctx, actionCacheDir, clientVersion, nil, nil)
		if sourceErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: configure public repository source: %v\n", sourceErr)
			return 1
		}
	}
	defer cleanupSource()
	if profile != "" {
		var parsed *workflow.Workflow
		var parseErr error
		if len(source) <= compiler.MaxReusableWorkflowBytes {
			parsed, parseErr = workflow.Parse(workflowPath, source)
		} else {
			parseErr = fmt.Errorf("workflow exceeds %d-byte limit", compiler.MaxReusableWorkflowBytes)
		}
		effectiveEvent, eventErr := newEffectiveEvent(event, effectiveEventFromPath)
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
					_ = out.write(ctx, report)
					return 1
				}
			} else if !selection.Applicable {
				report := triggerProcessingReport(workflowPath, source)
				report.Result = "not-applicable"
				if out.write(ctx, report) != nil {
					return 1
				}
				return 0
			}
		}
	}
	// Profile validation applies the hosted runner policy so validate and
	// upload agree on supported labels, including the default macOS queue.
	validationOptions := compiler.DefaultOptions()
	if profile != "" {
		validationOptions = hostedOptions("", nil, nil)
	}
	validationOptions.RepositorySource = repositorySource
	processingReport, ok := validatedProcessingReportWithOptions(ctx, out, workflowPath, profile, source, event, loadEvent != nil, &validationOptions)
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
			_ = out.write(ctx, processingReport)
			return 1
		}
		// Profile validation never executes generated plans, so the importer
		// digest stands in for every platform's runtime distribution.
		runtimeDistributions := map[compiler.Platform]string{
			compiler.PlatformLinuxAMD64:  distributionDigest,
			compiler.PlatformDarwinARM64: distributionDigest,
		}
		preflight, profileErr := compileHostedWithActionCache(ctx, workflowPath, source, event, commandVersion(clientVersion), distributionDigest, "buildkite-gha-profile-importer", "", nil, runtimeDistributions, actionCacheDir, repositorySource, nil)
		applyHostedPreflight(&processingReport, preflight)
		if profileErr != nil {
			if ctx.Err() != nil || errors.Is(profileErr, context.Canceled) {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: profile evaluation interrupted: %v\n", profileErr)
				return 1
			}
			processingReport.Result = classifyHostedFailure(&processingReport, workflowPath, profileErr)
			_ = out.write(ctx, processingReport)
			return 1
		}
		if preflight.HasActions {
			processingReport.Diagnostics = append(processingReport.Diagnostics, compatibility.Diagnostic{
				Level: "warning", Code: "W_ACTION_RUNTIME_UNKNOWN", Category: "compatibility", Stage: workflowprocessing.StageAdmission,
				Message: "Action runtime behavior was not evaluated. The action source, metadata, declared runtime, entrypoints, inputs, and nested actions were validated, but the action code was not executed and may depend on GitHub-only services. No action is required for admission; test the action on Buildkite if runtime compatibility is important.",
			})
		}
		if contextRequired {
			processingReport.SetStage(workflowprocessing.StageAdmission, compatibility.NotEvaluated)
			processingReport.Admission.Result = compatibility.NotEvaluated
			processingReport.Diagnostics = append(processingReport.Diagnostics, compatibility.Diagnostic{
				Level: "error", Code: workflowprocessing.CodeContextRequired, Category: "context", Stage: workflowprocessing.StageAdmission,
				Message: "Push and pull request path filters require linked Buildkite webhook data and a verified local git diff before admission can be determined.",
			})
			processingReport.Result = "context-required"
			if out.write(ctx, processingReport) != nil {
				return 1
			}
			return 1
		}
		processingReport.SetStage(workflowprocessing.StageAdmission, compatibility.Passed)
		processingReport.Admission.Result = "admitted"
		processingReport.Result = "admitted"
		if out.write(ctx, processingReport) != nil {
			return 1
		}
		return 0
	}
	processingReport.Result = "compilable"
	if out.write(ctx, processingReport) != nil {
		return 1
	}
	return 0
}

type profileValidationRuntime struct {
	actionSource       compiler.ActionSource
	distributionDigest string
	executableErr      error
}

func validateAllEvents(ctx context.Context, out processingOutput, workflowPath, clientVersion, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		validation := compatibility.EnvironmentProcessingReport(workflowPath, "", "workflow input could not be read")
		report := compatibility.NewProcessingReportV3(workflowPath, hostedProfile, validation)
		_ = out.writeV3(ctx, report)
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
		return 1
	}
	return validateAllEventsSource(ctx, out, workflowPath, source, clientVersion, actionCacheDir, runtime, stderr)
}

func validateAllEventsSource(ctx context.Context, out processingOutput, workflowPath string, source []byte, clientVersion, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) int {
	cleanup := func() {}
	if runtime == nil || runtime.actionSource == nil {
		actionSource, sourceCleanup, sourceErr := newHostedActionSource(ctx, actionCacheDir, clientVersion, nil, nil)
		cleanup = sourceCleanup
		if sourceErr != nil {
			validationReport := compatibility.EnvironmentProcessingReport(workflowPath, hostedProfile, "public repository source could not be configured")
			report := compatibility.NewProcessingReportV3(workflowPath, hostedProfile, validationReport)
			_ = out.writeV3(ctx, report)
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", sourceErr)
			cleanup()
			return 1
		}
		if runtime == nil {
			_, _, distributionDigest, executableErr := executable()
			runtime = &profileValidationRuntime{actionSource: actionSource, distributionDigest: distributionDigest, executableErr: executableErr}
		} else {
			runtime = &profileValidationRuntime{
				actionSource: actionSource, distributionDigest: runtime.distributionDigest, executableErr: runtime.executableErr,
			}
		}
	}
	defer cleanup()
	validationOptions := hostedOptions("", nil, nil)
	validationOptions.RepositorySource = runtime.actionSource
	validation, validationErr := compiler.ValidateWithOptionsContext(ctx, workflowPath, source, validationOptions)
	validationReport := compatibility.InitialProcessingReport(workflowPath, "", false, validation, validationErr)
	if validationErr != nil {
		validationReport.Result = "incompatible"
		report := compatibility.NewProcessingReportV3(workflowPath, hostedProfile, validationReport)
		if out.writeV3(ctx, report) != nil {
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
	failed := false
	for _, event := range []string{"push", "pull_request", "merge_group", "release", "issues", "issue_comment", "workflow_dispatch", "schedule"} {
		if !declared[event] {
			continue
		}
		var eventReport *compatibility.ProcessingReport
		eventOut := processingOutput{
			command: "validate", format: "json", reports: io.Discard, stderr: stderr,
			observe: func(observed compatibility.ProcessingReport) { eventReport = &observed },
		}
		if code := validateOneSource(ctx, eventOut, workflowPath, source, "", event, hostedProfile, clientVersion, actionCacheDir, runtime, stderr); code != 0 {
			failed = true
		}
		if eventReport == nil {
			return 1
		}
		report.Evaluations = append(report.Evaluations, compatibility.EventEvaluation{
			Event: event, Source: "generated", Report: *eventReport,
		})
	}
	if out.writeV3(ctx, report) != nil {
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
	if eventSeen && !slices.Contains([]string{"push", "pull_request", "merge_group", "release", "issues", "issue_comment", "workflow_dispatch", "schedule"}, eventName) {
		return "", "", "", "", "", false, fmt.Errorf("unsupported --event %q; supported events are push, pull_request, merge_group, release, issues, issue_comment, workflow_dispatch, and schedule", eventName)
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
	case "issues":
		event.Payload["action"] = "opened"
		event.Payload["issue"] = map[string]any{"number": 1}
	case "issue_comment":
		event.Payload["action"] = "created"
		event.Payload["issue"] = map[string]any{"number": 1}
		event.Payload["comment"] = map[string]any{"id": 1}
	case "schedule":
		event.Payload["schedule"] = "0 0 * * *"
	case "workflow_dispatch":
	default:
		return nil, fmt.Errorf("unsupported generated event %q; supported events are push, pull_request, merge_group, release, issues, issue_comment, workflow_dispatch, and schedule", name)
	}
	return json.Marshal(event)
}
