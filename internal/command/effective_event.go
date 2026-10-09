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
		event, err := buildkiteGitHubEventName(os.Getenv)
		if err != nil {
			return nil, "", err
		}
		if event == "discussion_comment" || event == "discussion" || event == "branch_protection_rule" || event == "milestone" || event == "watch" || event == "page_build" || event == "gollum" || event == "public" || event == "fork" || event == "label" || event == "create" || event == "delete" || event == "deployment" || event == "deployment_status" || event == "pull_request_review" || event == "pull_request_review_comment" || ((event == "release" || event == "merge_group") && os.Getenv(pipelineTriggerWorkflowPathEnvironment) != "") {
			return nil, "", fmt.Errorf("%s requires the original buildkite:webhook payload; rebuilds without it are unsupported", event)
		}
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
		EventPredicate:          "true",
		Branch:                  "null",
		Tag:                     "null",
		PullRequestBaseBranch:   "null",
		PullRequestAction:       "null",
		MergeGroupBaseBranch:    "null",
		MergeGroupAction:        "null",
		ReleaseAction:           "null",
		IssuesAction:            "null",
		IssueCommentAction:      "null",
		PullRequestReviewAction: "null",
		LabelAction:             "null",
		MilestoneAction:         "null",
		RuleAction:              "null",
		DiscussionAction:        "null",
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
		expressions.PullRequestReviewAction = triggerConditionLiteral(action)
		snapshot.PullRequestReviewAction = &action
		expressions.LabelAction = triggerConditionLiteral(action)
		snapshot.LabelAction = &action
		expressions.MilestoneAction = triggerConditionLiteral(action)
		snapshot.MilestoneAction = &action
		expressions.RuleAction = triggerConditionLiteral(action)
		snapshot.RuleAction = &action
		expressions.DiscussionAction = triggerConditionLiteral(action)
		snapshot.DiscussionAction = &action
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
	if annotationReason == "" {
		annotationReason, err = buildkitepipeline.TriggerFilterMismatchReason(triggers, event.Event.Event, event.TriggerSnapshot)
		if err != nil {
			return workflowTriggerSelection{}, err
		}
	}
	skipReason := ""
	if !applicable {
		skipReason = annotationReason
	}
	return workflowTriggerSelection{
		Condition:        condition,
		Applicable:       applicable,
		SkipReason:       skipReason,
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
		var message string
		switch {
		case pathFilters.Event == "push":
			message = pushPathFilterFailureMessage(pathFilters.Reason)
		case pathFilters.Reason == "":
			message = fmt.Sprintf("%s trigger path filters cannot be translated safely. Remove paths and paths-ignore from this trigger, or move the filtering into a job or step.", upperFirst(pathFilters.Event))
		case strings.Contains(pathFilters.Reason, "file local evaluation bound"):
			message = fmt.Sprintf("%s trigger path filters could not be evaluated safely. The local changed-file limit was exceeded; reduce the diff or remove the path filters.", upperFirst(pathFilters.Event))
		default:
			message = fmt.Sprintf("%s trigger path filters could not be evaluated safely. Check the detail for unavailable or mismatched evidence; correct the evidence or remove the path filters.", upperFirst(pathFilters.Event))
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

func pushPathFilterFailureMessage(reason string) string {
	detail := "Buildkite could not verify the webhook or checkout evidence needed to select this workflow."
	nextStep := " Contact Buildkite support with this build's URL and the diagnostic detail to investigate."
	switch reason {
	case "push path filters require linked Buildkite webhook data":
		detail = "This import has no linked GitHub push webhook. Manual builds and explicit event snapshots cannot supply the linked evidence required for push path filters."
		nextStep = " Use a build triggered by a GitHub push with its original webhook payload."
	case "new-branch push requires complete pushed commit evidence":
		detail = "The webhook commit list is empty or does not include the new branch's after commit, so Buildkite cannot determine the changed paths. Changing workflow checkout settings cannot supply this evidence."
	case "webhook push requires its commits array":
		detail = "The linked webhook has no usable commits array. Buildkite needs the original GitHub push commit list; changing workflow checkout settings cannot supply it."
	case "push before commit is unavailable in the local checkout":
		detail = "The webhook's before commit is missing from the importer's already non-shallow checkout. Buildkite needs that exact commit to compare the push; fetching more branch history is not a proven fix."
	case "combined added and deleted files require provider rename conformance data", "renamed and copied files require provider conformance data":
		detail = "Buildkite cannot yet match GitHub's path-filter behavior for possible renames or copies in this diff. This is a Buildkite compatibility limit, not an invalid workflow."
	case "push path filters require a complete non-shallow checkout":
		detail = "Buildkite could not confirm a non-shallow importer checkout. If git rev-parse --is-shallow-repository returns true there, ask the agent administrator to fetch full history before import. Workflow actions/checkout runs too late to fix this."
		nextStep = " If the checkout is not shallow or the check fails, contact Buildkite support with this build's URL and the diagnostic detail."
	case fmt.Sprintf("push exceeds GitHub's %d-commit path-filter diff bound", maxGitHubPushCommits):
		detail = fmt.Sprintf("The push exceeds the %d-commit path-filter evaluation limit. Buildkite does not reproduce GitHub's run-anyway fallback above this limit.", maxGitHubPushCommits)
		nextStep = fmt.Sprintf(" Push in batches of at most %d commits to stay within this limit.", maxGitHubPushCommits)
	case fmt.Sprintf("changed paths exceed the importer's %d-file local evaluation bound", maxLocallyEvaluatedPathFilterFiles):
		detail = fmt.Sprintf("The diff exceeds Buildkite's %d-file local evaluation limit.", maxLocallyEvaluatedPathFilterFiles)
		nextStep = " Split the push into smaller diffs to stay within this limit."
	}
	return "Push trigger path filters could not be evaluated safely. " + detail + nextStep +
		" Removing paths or paths-ignore is a workaround that changes which workflows run, not a fix."
}

func triggerProcessingReport(path string, source []byte) compatibility.ProcessingReport {
	parsed, _ := compiler.ParseWorkflow(path, source)
	report := compatibility.NewProcessingReport(path, hostedProfile)
	report.Sources = parsed.Sources
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
