package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
	"github.com/buildkite/buildkite-gha/internal/workflowprocessing"
)

const maxWebhookMetadataBytes = 25 << 20

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

func newEffectiveEvent(source []byte, origin effectiveEventOrigin) (effectiveEventSelection, error) {
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
	effective.TriggerExpressions.EventPredicate = buildkitepipeline.LiveEventPredicate(event.Event)
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
		IssuesAction:          "null",
		IssueCommentAction:    "null",
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
		expressions.IssuesAction = triggerConditionLiteral(action)
		snapshot.IssuesAction = &action
		expressions.IssueCommentAction = triggerConditionLiteral(action)
		snapshot.IssueCommentAction = &action
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
			Stage: workflowprocessing.StagePipeline, Code: workflowprocessing.CodePipelineGeneration, Category: "compatibility",
			Path: input.Path, Line: 1, Column: 1,
			Message: message,
			Detail:  pathFilters.Error(), Err: err,
		}
	}
	report.AddFailure(input.Path, workflowprocessing.StagePipeline, workflowprocessing.CodePipelineGeneration, "compatibility", err)
	report.Result = "incompatible"
	return report
}

func triggerProcessingReport(path string, source []byte) compatibility.ProcessingReport {
	parsed, _ := compiler.ParseWorkflow(path, source)
	report := compatibility.NewProcessingReport(path, hostedProfile)
	report.LogicalJobs = parsed.LogicalJobs
	report.SetStage(workflowprocessing.StageWorkflowParsing, compatibility.Passed)
	report.SetStage(workflowprocessing.StageEventValidation, compatibility.Passed)
	report.ApplyWarnings(path, parsed.Warnings)
	for _, job := range parsed.ParsedJobs {
		report.Jobs = append(report.Jobs, compatibility.JobResult{
			ID: job.ID, Result: compatibility.NotEvaluated,
			Location: &compatibility.SourceLocation{Path: job.Path, Line: job.Source.Start.Line, Column: job.Source.Start.Column},
		})
	}
	return report
}
