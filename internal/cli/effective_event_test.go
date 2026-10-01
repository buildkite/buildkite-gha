package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
)

func TestPathFilterFailureReportsDistinguishEvidenceAndFileLimit(t *testing.T) {
	for _, test := range []struct {
		event, reason, action string
	}{
		{"push", "push before commit is unavailable in the local checkout", "correct the evidence or remove the path filters"},
		{"pull_request", "changed paths exceed the importer's 3000-file local evaluation bound", "reduce the diff or remove the path filters"},
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
