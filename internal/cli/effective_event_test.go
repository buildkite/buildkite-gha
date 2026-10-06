package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
)

func TestPathFilterFailureReportsDistinguishEvidenceAndFileLimit(t *testing.T) {
	for _, test := range []struct {
		event, reason, action string
		support               bool
	}{
		{"push", "push before commit is unavailable in the local checkout", "already non-shallow checkout", true},
		{"push", "new-branch push requires complete pushed commit evidence", "commit list is empty or does not include the new branch's after commit", true},
		{"push", "combined added and deleted files require provider rename conformance data", "Buildkite compatibility limit, not an invalid workflow", true},
		{"push", "webhook push requires its commits array", "original GitHub push commit list", true},
		{"push", "push path filters require a complete non-shallow checkout", "ask the agent administrator to fetch full history before import", true},
		{"push", "push exceeds GitHub's 1000-commit path-filter diff bound", "Push in batches of at most 1000 commits", false},
		{"push", "changed paths exceed the importer's 3000-file local evaluation bound", "Split the push into smaller diffs", false},
		{"push", "push path filters require linked Buildkite webhook data", "Use a build triggered by a GitHub push with its original webhook payload", false},
		{"pull_request", "changed paths exceed the importer's 3000-file local evaluation bound", "reduce the diff or remove the path filters", false},
	} {
		t.Run(test.event+"/"+test.action, func(t *testing.T) {
			report := triggerFailureProcessingReport(workflowInput{Path: "ci.yml", Source: []byte("on: push\n")}, &buildkitepipeline.UnsupportedPathFiltersError{Event: test.event, Reason: test.reason})
			if len(report.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v", report.Diagnostics)
			}
			diagnostic := report.Diagnostics[0]
			if !strings.Contains(diagnostic.Message, test.action) || !strings.Contains(diagnostic.Detail, test.reason) || strings.Contains(diagnostic.Detail, "are unsupported") {
				t.Fatalf("path filter diagnostic = %#v", diagnostic)
			}
			if diagnostic.Code != "E_PIPELINE_GENERATION" || diagnostic.Category != "compatibility" || diagnostic.Blocker != "path_filter" || diagnostic.BlockerDetail != test.event {
				t.Fatalf("path filter classification = %#v", diagnostic)
			}
			if strings.Contains(diagnostic.Message, "Buildkite support with this build's URL") != test.support {
				t.Fatalf("incorrect support guidance: %s", diagnostic.Message)
			}
			if test.event == "push" && (!strings.Contains(diagnostic.Message, "workaround that changes which workflows run, not a fix") || strings.Contains(diagnostic.Message, "correct the evidence")) {
				t.Fatalf("missing actionable guidance: %s", diagnostic.Message)
			}
			details := &commandTelemetryDetails{}
			details.addReportDiagnostics(report)
			got := details.forOutcome(telemetry.OutcomeSuccess)
			if len(got.Diagnostics) != 1 {
				t.Fatalf("telemetry diagnostics = %#v", got.Diagnostics)
			}
			wire := got.Diagnostics[0]
			if wire.Code != "E_PIPELINE_GENERATION" || wire.Severity != telemetry.SeverityError || wire.Blocker != "path_filter" || wire.BlockerDetail != test.event || !strings.Contains(wire.Message, test.reason) || !strings.Contains(wire.Message, test.action) || wire.MessageTruncated || got.FailureCode != "" {
				t.Fatalf("handled path filter telemetry = %#v", got)
			}
		})
	}
}

func TestNewEffectiveEventSeparatesExpressionsAndSnapshot(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := newEffectiveEvent(source, effectiveEventFromPath)
	if err != nil {
		t.Fatal(err)
	}
	wantExpressions := buildkitepipeline.TriggerConditionExpressions{
		EventPredicate:          "true",
		Branch:                  `"main"`,
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
	branch := "main"
	wantSnapshot := buildkitepipeline.TriggerEventSnapshot{Branch: &branch}
	if !reflect.DeepEqual(explicit.TriggerExpressions, wantExpressions) || !reflect.DeepEqual(explicit.TriggerSnapshot, wantSnapshot) {
		t.Fatalf("explicit effective event = expressions %#v, snapshot %#v", explicit.TriggerExpressions, explicit.TriggerSnapshot)
	}

	webhook, err := newEffectiveEvent(source, effectiveEventFromWebhook)
	if err != nil {
		t.Fatal(err)
	}
	wantExpressions.EventPredicate = buildkitepipeline.LiveEventPredicate("push")
	if !reflect.DeepEqual(webhook.TriggerExpressions, wantExpressions) || !reflect.DeepEqual(webhook.TriggerSnapshot, wantSnapshot) {
		t.Fatalf("webhook effective event = expressions %#v, snapshot %#v", webhook.TriggerExpressions, webhook.TriggerSnapshot)
	}

	t.Setenv("BUILDKITE_SOURCE", "ui")
	build, err := newEffectiveEvent(source, effectiveEventFromBuild)
	if err != nil {
		t.Fatal(err)
	}
	if build.TriggerExpressions.EventPredicate != buildkitepipeline.LiveEventPredicate("push") {
		t.Fatalf("build effective event predicate = %q", build.TriggerExpressions.EventPredicate)
	}
}
