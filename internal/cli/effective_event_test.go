package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
)

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
		EventPredicate:        "true",
		Branch:                `"main"`,
		Tag:                   "null",
		PullRequestBaseBranch: "null",
		PullRequestAction:     "null",
		MergeGroupBaseBranch:  "null",
		MergeGroupAction:      "null",
		ReleaseAction:         "null",
		IssuesAction:          "null",
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
	if !reflect.DeepEqual(webhook.TriggerExpressions, wantExpressions) || !reflect.DeepEqual(webhook.TriggerSnapshot, wantSnapshot) || webhook.BuildSource != "" {
		t.Fatalf("webhook effective event = expressions %#v, snapshot %#v", webhook.TriggerExpressions, webhook.TriggerSnapshot)
	}

	t.Setenv("BUILDKITE_SOURCE", "ui")
	build, err := newEffectiveEvent(source, effectiveEventFromBuild)
	if err != nil {
		t.Fatal(err)
	}
	if build.BuildSource != "ui" || build.TriggerExpressions.EventPredicate != buildkitepipeline.LiveEventPredicate("push") {
		t.Fatalf("build effective event = source %q, predicate %q", build.BuildSource, build.TriggerExpressions.EventPredicate)
	}
}
