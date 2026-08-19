package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
}

func TestNewEffectiveEventUsesPullRequestPushPredicate(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "importer",
		"BUILDKITE_REPO":                     "https://github.com/acme/widgets",
		"BUILDKITE_COMMIT":                   strings.Repeat("a", 40),
		"BUILDKITE_BRANCH":                   "feature",
		"BUILDKITE_PULL_REQUEST":             "42",
		"BUILDKITE_PULL_REQUEST_BASE_BRANCH": "main",
		"BUILDKITE_GITHUB_EVENT":             "push",
	}
	source, err := buildkiteEventSource(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	effective, err := newEffectiveEvent(source, effectiveEventFromPullRequestPush)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Event.Event != "pull_request" || effective.Event.Ref != "refs/pull/42/head" ||
		effective.TriggerExpressions.EventPredicate != buildkitepipeline.LivePullRequestPushPredicate() ||
		effective.TriggerSnapshot.PullRequestBaseBranch == nil || *effective.TriggerSnapshot.PullRequestBaseBranch != "main" ||
		effective.TriggerSnapshot.PullRequestAction == nil || *effective.TriggerSnapshot.PullRequestAction != "synchronize" {
		t.Fatalf("pull request push effective event = %#v", effective)
	}
}
