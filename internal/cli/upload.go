package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	legacyRuntimeQueue            = "hosted"
	legacyTargetQueueEnvironment  = "BUILDKITE_GHA_TARGET_QUEUE"
	legacyRuntimeImageEnvironment = "BUILDKITE_GHA_RUNTIME_IMAGE"
	workflowCheckSummaryLimit     = 65535
	workflowCheckSummaryNotice    = "\n\n_Additional diagnostics omitted at the provider check summary size limit._\n"
)

func upload(args []string, stdout, stderr io.Writer, clientVersion string, agent transport.Agent) int {
	return uploadFromPlatform(runtime.GOOS, runtime.GOARCH, args, stdout, stderr, commandVersion(clientVersion), clientVersion, agent)
}

func uploadFromPlatform(goos, goarch string, args []string, stdout, stderr io.Writer, version, clientVersion string, agent transport.Agent) int {
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
	uploadArguments.clientVersion = clientVersion
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
	var skippedWorkflowPaths []string
	var err error
	if uploadArguments.explicitWorkflowPaths {
		workflows, skippedWorkflowPaths, err = expandExplicitWorkflowPaths(workflowOperands, uploadArguments.checkoutPath)
	} else {
		workflows, skippedWorkflowPaths, err = resolveWorkflowOperands(workflowOperands)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	if !uploadArguments.explicitWorkflowPaths && len(skippedWorkflowPaths) != 0 {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: workflow path %q is not tracked by git\n", skippedWorkflowPaths[0])
		return 1
	}
	for _, path := range skippedWorkflowPaths {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: warning: workflow path %q is missing or untracked; skipping\n", path)
	}
	if len(workflows) == 0 {
		_, _ = fmt.Fprintln(stderr, "buildkite-gha: upload: warning: all configured workflow paths are missing or untracked; there is nothing to upload")
		return 0
	}
	runnableWorkflowCount := 0
	for i := range workflows {
		workflows[i].Source, err = os.ReadFile(workflows[i].Path)
		if err != nil {
			report := compatibility.EnvironmentProcessingReport(workflows[i].Path, hostedProfile, "workflow input could not be read")
			return out.fail(ctx, report, fmt.Errorf("read workflow %s: %w", workflows[i].CanonicalPath, err))
		}
		if len(workflows[i].Source) > compiler.MaxReusableWorkflowBytes {
			_, _ = validatedProcessingReport(ctx, out, workflows[i].Path, hostedProfile, workflows[i].Source, nil, false)
			return 1
		}
		parsed, parseErr := workflow.Parse(workflows[i].Path, workflows[i].Source)
		if parseErr != nil {
			_, _ = validatedProcessingReport(ctx, out, workflows[i].Path, hostedProfile, workflows[i].Source, nil, false)
			return 1
		}
		workflows[i].ReusableOnly = parsed.ReusableOnly()
		workflows[i].Name = parsed.Name
		workflows[i].Parsed = parsed
		workflows[i].Triggers = parsed.Triggers
		if !workflows[i].ReusableOnly {
			runnableWorkflowCount++
		}
	}
	if runnableWorkflowCount == 0 {
		_, _ = fmt.Fprintln(stderr, "buildkite-gha: upload: workflow paths matched only reusable workflow_call workflows; there is nothing to upload")
		return 1
	}
	anonymousSource, cleanupAnonymousSource, sourceErr := newHostedActionSource(ctx, "", uploadArguments.clientVersion, nil, nil)
	if sourceErr != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				return out.fail(ctx, compatibility.EnvironmentProcessingReport(input.Path, hostedProfile, "public repository source could not be configured"), sourceErr)
			}
		}
		return 1
	}
	defer cleanupAnonymousSource()
	sourceSwitch := &repositorySourceSwitch{source: anonymousSource}
	repositorySource := compiler.MemoizeRepositorySource(sourceSwitch)
	for _, input := range workflows {
		if input.ReusableOnly {
			continue
		}
		validationOptions := hostedOptions("", uploadArguments.runnerTargets, nil)
		validationOptions.RepositorySource = repositorySource
		validation, validationErr := compiler.ValidateWithOptionsContext(ctx, input.Path, input.Source, validationOptions)
		if validation.RuntimeMatrixBoundary {
			report := compatibility.InitialProcessingReport(input.Path, hostedProfile, false, validation, validationErr)
			report.Result = "incompatible"
			_ = out.write(ctx, report)
			return 1
		}
	}
	if eventPath == "" {
		eventSource, eventOrigin, eventLoadErr = loadEffectiveEventSource(ctx, eventPath, agent)
	}
	if eventLoadErr != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				return out.fail(ctx, compatibility.EventInputProcessingReport(input.Path, hostedProfile, input.Source, "event input could not be acquired"), eventLoadErr)
			}
		}
		return 1
	}
	effectiveEvent, eventParseErr := newEffectiveEvent(eventSource, eventOrigin)
	if eventParseErr != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				_, _ = validatedProcessingReport(ctx, out, input.Path, hostedProfile, input.Source, eventSource, true)
				return 1
			}
		}
		return 1
	}
	authentication := importerJobActionSourceAuthentication(stderr, uploadArguments.clientVersion)
	var sourceOptions []actionsource.Option
	if effectiveEvent.Event.Provider == "github" {
		if authenticationOption := authentication.option(effectiveEvent.Event.Repository.Owner + "/" + effectiveEvent.Event.Repository.Name); authenticationOption != nil {
			sourceOptions = append(sourceOptions, authenticationOption)
		}
	}
	authenticatedSource, cleanupSource, sourceErr := newHostedActionSource(ctx, "", uploadArguments.clientVersion, sourceOptions, nil)
	if sourceErr != nil {
		for _, input := range workflows {
			if !input.ReusableOnly {
				return out.fail(ctx, compatibility.EnvironmentProcessingReport(input.Path, hostedProfile, "public repository source could not be configured"), sourceErr)
			}
		}
		return 1
	}
	defer cleanupSource()
	sourceSwitch.set(authenticatedSource)
	populateChangedPaths(&effectiveEvent.TriggerSnapshot, effectiveEvent.Event, effectiveEvent.Origin, workflows, uploadArguments.checkoutPath)
	processingReports := make([]compatibility.ProcessingReport, len(workflows))
	for i := range workflows {
		if workflows[i].ReusableOnly {
			continue
		}
		selection, triggerErr := selectWorkflowTrigger(workflows[i].Triggers, effectiveEvent)
		switch {
		case triggerErr != nil:
			workflows[i].Applicable = true
			workflows[i].TriggerCondition = effectiveEvent.TriggerExpressions.EventPredicate
			processingReports[i] = triggerFailureProcessingReport(workflows[i], triggerErr)
		case workflows[i].PathFiltersError != "" && selection.AnnotationReason == "":
			workflows[i].Applicable = true
			workflows[i].TriggerCondition = effectiveEvent.TriggerExpressions.EventPredicate
			processingReports[i] = triggerFailureProcessingReport(workflows[i], &buildkitepipeline.UnsupportedPathFiltersError{
				Event: effectiveEvent.Event.Event, Reason: workflows[i].PathFiltersError,
			})
		default:
			workflows[i].Applicable = selection.Applicable
			workflows[i].TriggerCondition = selection.Condition
			workflows[i].SkipReason = selection.SkipReason
			workflows[i].AnnotationReason = selection.AnnotationReason
		}
		runName, runNameErr := compiler.ResolveWorkflowRunName(workflows[i].Path, workflows[i].Parsed, effectiveEvent.Event, workflows[i].Applicable)
		if runNameErr != nil {
			if workflows[i].Applicable {
				if len(processingReports[i].Stages) == 0 {
					processingReports[i] = triggerProcessingReport(workflows[i].Path, workflows[i].Source)
				}
				processingReports[i].AddFailure(workflows[i].Path, string(compiler.StageExpressions), compiler.CodeExpressionInvalid, "compatibility", runNameErr)
				processingReports[i].Result = "incompatible"
			}
			continue
		}
		workflows[i].RunName = runName
	}
	validations := make([]compiler.Report, len(workflows))
	validationErrs := make([]error, len(workflows))
	for i, input := range workflows {
		if !input.Applicable || processingReportHasErrors(processingReports[i]) {
			continue
		}
		validationOptions := hostedOptions("", uploadArguments.runnerTargets, nil)
		validationOptions.StepKeyNamespace = input.StepKeyNamespace
		validationOptions.RepositorySource = repositorySource
		validations[i], validationErrs[i] = compiler.ValidateEventWithOptionsContext(ctx, input.Path, input.Source, effectiveEvent.Source, validationOptions)
	}
	runnerSelectors, runnerWarnings, err := suggestedRunnerTargets(ctx, validations, uploadArguments.runnerTargets, uploadArguments.clientVersion)
	if err != nil {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", ctx.Err())
			return 1
		}
	}
	if len(runnerSelectors) != 0 {
		uploadArguments.runnerSelectors = runnerSelectors
		for i, input := range workflows {
			if !input.Applicable || processingReportHasErrors(processingReports[i]) {
				continue
			}
			validationOptions := hostedOptions("", uploadArguments.runnerTargets, nil)
			applyRunnerSelectors(&validationOptions, runnerSelectors)
			validationOptions.StepKeyNamespace = input.StepKeyNamespace
			validationOptions.RepositorySource = repositorySource
			validations[i], validationErrs[i] = compiler.ValidateEventWithOptionsContext(ctx, input.Path, input.Source, effectiveEvent.Source, validationOptions)
		}
	}
	out.annotateRunnerResolutionWarnings(ctx, runnerWarnings)
	for i, input := range workflows {
		if !input.Applicable || processingReportHasErrors(processingReports[i]) {
			continue
		}
		processingReports[i] = compatibility.InitialProcessingReport(input.Path, hostedProfile, true, validations[i], validationErrs[i])
		if validationErrs[i] != nil {
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
			_ = out.write(ctx, processingReports[i])
		}
		return 1
	}
	importerDistribution := runtimeDistribution{contents: executableContents, digest: distributionDigest}
	requiredPlatforms := make(map[compiler.Platform]bool, 2)
	for i, input := range workflows {
		if !input.Applicable || processingReportHasErrors(processingReports[i]) {
			continue
		}
		platforms, platformErr := requiredRuntimePlatforms(ctx, input.Path, input.Source, effectiveEvent.Source, "", uploadArguments.runnerTargets, uploadArguments.runnerSelectors, repositorySource)
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
		return finishUpload(ctx, uploadArguments, stdout, stderr, version, agent, workflows, effectiveEvent, executablePath, distributionDigest, importerStep, processingReports, out, runtimeDistributions, repositorySource, authentication)
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
	return finishUpload(ctx, uploadArguments, stdout, stderr, version, agent, workflows, effectiveEvent, executablePath, distributionDigest, importerStep, processingReports, out, runtimeDistributions, repositorySource, authentication)
}

func finishUpload(ctx context.Context, uploadArguments parsedUploadArgs, stdout, stderr io.Writer, version string, agent transport.Agent, workflows []workflowInput, effectiveEvent effectiveEventSelection, executablePath, distributionDigest, importerStep string, processingReports []compatibility.ProcessingReport, out processingOutput, runtimeDistributions map[compiler.Platform]runtimeDistribution, repositorySource compiler.RepositorySource, authentication *actionSourceAuthentication) int {
	runtimeDigests := make(map[compiler.Platform]string, len(runtimeDistributions))
	for platform, runtimeDistribution := range runtimeDistributions {
		runtimeDigests[platform] = runtimeDistribution.digest
	}
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
			checkName := input.Name
			if checkName == "" {
				checkName = input.CanonicalPath
			}
			label := workflowGroupLabel(checkName, input.RunName)
			groupKey := "gha-workflow-" + input.Identity
			generatedWorkflows = append(generatedWorkflows, buildkitepipeline.Workflow{
				GroupLabel: label,
				CheckName:  checkName,
				GroupKey:   groupKey,
				Event:      effectiveEvent.Event.Event,
				SkipReason: input.SkipReason,
			})
			skippedWorkflows = append(skippedWorkflows, skippedWorkflow{label: label, key: groupKey, reason: input.AnnotationReason})
			parsed, _ := compiler.ParseWorkflow(input.Path, input.Source)
			writeCompilerWarnings(stderr, "upload", input.CanonicalPath, parsed.Warnings)
			if uploadArguments.telemetry != nil {
				uploadArguments.telemetry.addWarnings(parsed.Warnings)
			}
			continue
		}
		if processingReportHasErrors(processingReports[i]) {
			failed, artifacts := failedGeneratedWorkflow(input, effectiveEvent.Event.Event, processingReports[i], out.sourceLinks)
			generatedWorkflows = append(generatedWorkflows, failed)
			failureArtifacts = append(failureArtifacts, artifacts...)
			continue
		}
		preflight, err := compileHostedNamespacedWithActionCache(ctx, input.Path, input.Source, effectiveEvent.Source, version, distributionDigest, importerStep, "", uploadArguments.runnerTargets, uploadArguments.runnerSelectors, runtimeDigests, input.StepKeyNamespace, uploadArguments.oidc, "", repositorySource, authentication)
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
			_ = out.write(ctx, processingReports[i])
			return 1
		}
		bundle := preflight.Bundle
		checkName := bundle.IR.Workflow.Name
		if checkName == "" {
			checkName = input.CanonicalPath
		}
		label := workflowGroupLabel(checkName, bundle.IR.Workflow.RunName)
		generated := bundle.GeneratedWorkflow
		generated.GroupLabel = label
		generated.CheckName = checkName
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
				out.annotate(ctx, processingReports[i])
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
		out.annotateSkippedWorkflows(ctx, effectiveEvent.Event.Event, skippedWorkflows)
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
	out.annotateSkippedWorkflows(ctx, effectiveEvent.Event.Event, skippedWorkflows)
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
	checkName := input.Name
	if checkName == "" {
		checkName = input.CanonicalPath
	}
	label := workflowGroupLabel(checkName, input.RunName)
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
		CheckName:  checkName,
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

func workflowGroupLabel(workflowName, runName string) string {
	if strings.TrimSpace(runName) == "" {
		return workflowName
	}
	return workflowName + " — " + runName
}

func generatedFailureArtifact(kind, extension, contents string) transport.Artifact {
	encoded := []byte(contents)
	digest := transport.Digest(encoded)
	path := ".buildkite-gha/failures/" + kind + "/" + strings.TrimPrefix(digest, "sha256:") + extension
	return transport.Artifact{Path: path, Digest: digest, Contents: encoded}
}

func requiredRuntimePlatforms(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, groupLabel string, configuredTargets map[string]compiler.RunnerTarget, runnerSelectors []compiler.RunnerSelector, repositorySource compiler.RepositorySource) (map[compiler.Platform]bool, error) {
	options := hostedOptions(groupLabel, configuredTargets, nil)
	applyRunnerSelectors(&options, runnerSelectors)
	options.RepositorySource = repositorySource
	preflight, err := compiler.CompileWithOptionsContext(ctx, workflowPath, workflowSource, eventSource, options)
	if err != nil {
		return nil, err
	}
	var ir compiler.IR
	if err := json.Unmarshal(preflight, &ir); err != nil {
		return nil, fmt.Errorf("decode compiler preflight: %w", err)
	}
	platforms := make(map[compiler.Platform]bool, 2)
	for _, job := range ir.Jobs {
		target, err := options.Runners.Resolve(job.RunsOn, options.EventTrust)
		if err != nil {
			return nil, fmt.Errorf("resolve runtime platform for job %q: %w", job.LogicalJobID, err)
		}
		platforms[target.Platform] = true
	}
	return platforms, nil
}

type workflowInput struct {
	Path, CanonicalPath, Identity, StepKeyNamespace string
	Name, RunName                                   string
	Source                                          []byte
	Parsed                                          *workflow.Workflow
	Triggers                                        []workflow.Trigger
	TriggerCondition, SkipReason, AnnotationReason  string
	PathFiltersError                                string
	ReusableOnly, Applicable                        bool
}

func resolveWorkflowOperands(operands []string) ([]workflowInput, []string, error) {
	if len(operands) != 1 {
		return expandExplicitWorkflowPaths(operands, "")
	}
	path, err := filepath.Abs(operands[0])
	if err != nil {
		return nil, nil, fmt.Errorf("resolve workflow path %q: %w", operands[0], err)
	}
	if err := requireRegularWorkflowFile(path, operands[0]); err != nil {
		return expandExplicitWorkflowPaths(operands, "")
	}
	extension := filepath.Ext(path)
	if extension != ".yml" && extension != ".yaml" {
		return nil, nil, fmt.Errorf("workflow path %q must end in .yml or .yaml", operands[0])
	}
	canonical := filepath.ToSlash(filepath.Clean(operands[0]))
	if rootBytes, rootErr := exec.Command("git", "rev-parse", "--show-toplevel").Output(); rootErr == nil {
		root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
		if relative, relativeErr := filepath.Rel(root, path); relativeErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			canonical = filepath.ToSlash(filepath.Clean(relative))
		}
	}
	inputs, err := workflowInputs([]workflowInput{{Path: path, CanonicalPath: canonical}}, false)
	return inputs, nil, err
}

func expandExplicitWorkflowPaths(operands []string, checkoutPath string) ([]workflowInput, []string, error) {
	if len(operands) == 0 {
		return nil, nil, fmt.Errorf("workflow path is required")
	}
	rootBytes, err := gitRootCommand(checkoutPath).Output()
	if err != nil {
		if checkoutPath != "" {
			return nil, nil, fmt.Errorf("locate git repository from BUILDKITE_BUILD_CHECKOUT_PATH %q: %w", checkoutPath, err)
		}
		return nil, nil, fmt.Errorf("locate checked-out git repository: %w", err)
	}
	root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
	matches := make([]workflowInput, 0, len(operands))
	skipped := make([]string, 0)
	for _, operand := range operands {
		inputPath := operand
		if checkoutPath != "" && !filepath.IsAbs(inputPath) {
			inputPath = filepath.Join(root, inputPath)
		}
		absolute, err := filepath.Abs(inputPath)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve workflow path %q: %w", operand, err)
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, nil, fmt.Errorf("workflow path %q is outside the checked-out git repository", operand)
		}
		canonical := filepath.ToSlash(filepath.Clean(relative))
		info, statErr := os.Lstat(absolute)
		if statErr == nil && !info.Mode().IsRegular() {
			return nil, nil, requireRegularWorkflowFile(absolute, operand)
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, nil, requireRegularWorkflowFile(absolute, operand)
		}
		output, gitErr := exec.Command("git", "-C", root, "ls-files", "-z", "--", ":(top,literal)"+canonical).Output()
		if gitErr != nil {
			return nil, nil, fmt.Errorf("inspect workflow path %q in git index: %w", operand, gitErr)
		}
		entries := bytes.Split(output, []byte{0})
		tracked := len(entries) == 2 && string(entries[0]) == canonical && len(entries[1]) == 0
		if !tracked && strings.ContainsAny(operand, "*?[") {
			return nil, nil, fmt.Errorf("workflow list entries must be explicit paths; glob pattern %q is not allowed", operand)
		}
		extension := filepath.Ext(canonical)
		if extension != ".yml" && extension != ".yaml" {
			return nil, nil, fmt.Errorf("workflow path %q must end in .yml or .yaml", operand)
		}
		if !tracked {
			skipped = append(skipped, operand)
			continue
		}
		if err := requireRegularWorkflowFile(absolute, operand); err != nil {
			return nil, nil, err
		}
		matches = append(matches, workflowInput{Path: filepath.Join(root, filepath.FromSlash(canonical)), CanonicalPath: canonical})
	}
	inputs, err := workflowInputs(matches, true)
	return inputs, skipped, err
}

func requireRegularWorkflowFile(path, displayPath string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workflow path %q is tracked by git but missing from the checkout; restore the file or check sparse-checkout configuration", displayPath)
	}
	if err != nil {
		return fmt.Errorf("inspect workflow path %q: %w", displayPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workflow path %q is a symbolic link; workflow paths must be regular tracked files", displayPath)
	}
	if info.IsDir() {
		return fmt.Errorf("workflow path %q is a directory; workflow paths must be regular tracked files", displayPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("workflow path %q is not a regular file", displayPath)
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

type parsedUploadArgs struct {
	workflowOperands         []string
	explicitWorkflowPaths    bool
	checkoutPath             string
	eventPath                string
	clientVersion            string
	runtimeDistributionPaths map[compiler.Platform]string
	runnerTargets            map[string]compiler.RunnerTarget
	runnerSelectors          []compiler.RunnerSelector
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
