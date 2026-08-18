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
	explicit, err := newEffectiveEvent(source, effectiveEventFromPath, func(string) string { return "webhook" })
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
	}
	branch := "main"
	wantSnapshot := buildkitepipeline.TriggerEventSnapshot{Branch: &branch}
	if !reflect.DeepEqual(explicit.TriggerExpressions, wantExpressions) || !reflect.DeepEqual(explicit.TriggerSnapshot, wantSnapshot) {
		t.Fatalf("explicit effective event = expressions %#v, snapshot %#v", explicit.TriggerExpressions, explicit.TriggerSnapshot)
	}

	webhook, err := newEffectiveEvent(source, effectiveEventFromWebhook, func(key string) string {
		if key == "BUILDKITE_SOURCE" {
			return "webhook"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	wantExpressions.EventPredicate = `build.source == "webhook"`
	if !reflect.DeepEqual(webhook.TriggerExpressions, wantExpressions) || !reflect.DeepEqual(webhook.TriggerSnapshot, wantSnapshot) {
		t.Fatalf("webhook effective event = expressions %#v, snapshot %#v", webhook.TriggerExpressions, webhook.TriggerSnapshot)
	}
}
