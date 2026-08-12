package buildkite

import (
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestTranslateTriggerCondition(t *testing.T) {
	got, err := TranslateTriggerCondition([]workflow.Trigger{
		{Event: "push", Branches: []string{"main", "releases/**", "!releases/**-alpha", "releases/v[0-9]+"}},
		{Event: "pull_request"}, {Event: "workflow_dispatch"}, {Event: "schedule"}, {Event: "workflow_call"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`build.source_event == "push"`, `build.branch =~ /^main$/`, `releases\/.*`, `build.source_event == "pull_request"`, `build.source_action == "opened"`, `build.source_action == "synchronize"`, `build.source == "ui"`, `build.source == "schedule"`} {
		if !strings.Contains(got, want) {
			t.Errorf("condition missing %q:\n%s", want, got)
		}
	}
}

func TestTranslateTriggerConditionRejectsUnsafeTriggers(t *testing.T) {
	tests := []struct {
		name     string
		triggers []workflow.Trigger
		want     string
	}{
		{name: "paths", triggers: []workflow.Trigger{{Event: "push", Paths: []string{"src/**"}}}, want: "path filters are unsupported"},
		{name: "event", triggers: []workflow.Trigger{{Event: "issues"}}, want: "unsupported GitHub trigger"},
		{name: "mixed include ignore", triggers: []workflow.Trigger{{Event: "push", Branches: []string{"main"}, BranchesIgnore: []string{"release"}}}, want: "cannot be combined"},
		{name: "leading negative", triggers: []workflow.Trigger{{Event: "push", Branches: []string{"!release/**"}}}, want: "must follow a positive"},
		{name: "unsupported PR type", triggers: []workflow.Trigger{{Event: "pull_request", Types: []string{"not-real"}}}, want: "cannot be mapped exactly"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := TranslateTriggerCondition(test.triggers)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("TranslateTriggerCondition(%v) error = %v, want %q", test.triggers, err, test.want)
			}
		})
	}
}

func TestTranslateTriggerConditionRequiresDirectBuildSource(t *testing.T) {
	_, err := TranslateTriggerCondition([]workflow.Trigger{{Event: "workflow_call"}})
	if err == nil || !strings.Contains(err.Error(), "no supported build source") {
		t.Fatalf("TranslateTriggerCondition(workflow_call) error = %v", err)
	}
}

func TestTranslateEventTriggerConditionSelectsOnlyEffectiveEvent(t *testing.T) {
	triggers := []workflow.Trigger{
		{Event: "push", Branches: []string{"main"}},
		{Event: "pull_request", Types: []string{"opened"}},
		{Event: "workflow_dispatch"},
		{Event: "schedule"},
		{Event: "workflow_call"},
	}
	condition, applicable, err := TranslateEventTriggerCondition(triggers, "push", LiveTriggerConditionContext(`build.source == "trigger_job"`))
	if err != nil {
		t.Fatal(err)
	}
	if !applicable || !strings.Contains(condition, `build.source == "trigger_job"`) || !strings.Contains(condition, `build.branch =~ /^main$/`) {
		t.Fatalf("push condition/applicability = %q / %t", condition, applicable)
	}
	for _, excluded := range []string{"pull_request", "source_action", `build.source == "ui"`, `build.source == "schedule"`} {
		if strings.Contains(condition, excluded) {
			t.Fatalf("push condition contains another event term %q: %s", excluded, condition)
		}
	}

	condition, applicable, err = TranslateEventTriggerCondition(triggers, "issues", LiveTriggerConditionContext("true"))
	if err != nil || applicable || condition != "" {
		t.Fatalf("non-applicable condition = %q, %t, %v", condition, applicable, err)
	}
}

func TestTranslateEventTriggerConditionValidatesInactiveTriggers(t *testing.T) {
	_, _, err := TranslateEventTriggerCondition([]workflow.Trigger{
		{Event: "push"},
		{Event: "pull_request", Paths: []string{"src/**"}},
	}, "push", LiveTriggerConditionContext("true"))
	if err == nil || !strings.Contains(err.Error(), "path filters are unsupported") {
		t.Fatalf("inactive path filter error = %v", err)
	}
}

func TestTranslateEventTriggerConditionUsesSnapshotExpressions(t *testing.T) {
	context := TriggerConditionContext{
		EventPredicate:        "true",
		Branch:                `"main"`,
		Tag:                   `"v1"`,
		PullRequestBaseBranch: `"release"`,
		PullRequestAction:     `"opened"`,
	}
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "push", Branches: []string{"main"}, Tags: []string{"v*"}}}, "push", context)
	if err != nil || !applicable {
		t.Fatalf("snapshot push condition = %q, %t, %v", condition, applicable, err)
	}
	for _, want := range []string{"true", `"v1" != null`, `"main" =~ /^main$/`, `"v1" =~ /^v[^\/]*$/`} {
		if !strings.Contains(condition, want) {
			t.Fatalf("snapshot push condition missing %q: %s", want, condition)
		}
	}
	if strings.Contains(condition, "build.") {
		t.Fatalf("snapshot push condition reads live Buildkite fields: %s", condition)
	}

	condition, applicable, err = TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request", Branches: []string{"release"}, Types: []string{"opened"}}}, "pull_request", context)
	if err != nil || !applicable {
		t.Fatalf("snapshot pull request condition = %q, %t, %v", condition, applicable, err)
	}
	for _, want := range []string{"true", `"release" =~ /^release$/`, `"opened" == "opened"`} {
		if !strings.Contains(condition, want) {
			t.Fatalf("snapshot pull request condition missing %q: %s", want, condition)
		}
	}
	if strings.Contains(condition, "build.") {
		t.Fatalf("snapshot pull request condition reads live Buildkite fields: %s", condition)
	}
}

func TestTranslatePushSeparatesBranchAndTagFilters(t *testing.T) {
	condition, err := TranslateTriggerCondition([]workflow.Trigger{{
		Event:    "push",
		Branches: []string{"release/**", "!release/**-alpha", "release/special-alpha"},
		Tags:     []string{"v[0-9]+", "!v0"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`build.tag == null &&`, `build.branch =~ /^release\/.*$/`, `&& !(build.branch =~ /^release\/.*-alpha$/)`, `|| build.branch =~ /^release\/special-alpha$/`,
		`build.tag != null &&`, `build.tag =~ /^v[0-9]+$/`, `&& !(build.tag =~ /^v0$/)`,
	} {
		if !strings.Contains(condition, want) {
			t.Errorf("push condition missing %q:\n%s", want, condition)
		}
	}
}

func TestGitHubRefGlobPreservesEscapedSpecialCharacterModifiers(t *testing.T) {
	got, err := githubRefGlob(`release/\*?/v[0-9]+`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `^release\/\*?\/v[0-9]+$`; got != want {
		t.Fatalf("githubRefGlob() = %q, want %q", got, want)
	}
	for _, glob := range []string{"?main", "main**+", "main?+"} {
		if _, err := githubRefGlob(glob); err == nil {
			t.Errorf("githubRefGlob(%q) succeeded, want invalid modifier", glob)
		}
	}
}

func TestGitHubRefGlobEscapesSlashInsideWildcardClass(t *testing.T) {
	got, err := githubRefGlob("release/*")
	if err != nil {
		t.Fatal(err)
	}
	if want := `^release\/[^\/]*$`; got != want {
		t.Fatalf("githubRefGlob() = %q, want %q", got, want)
	}
}

func TestTranslatePullRequestUsesBaseBranchAndExplicitTypes(t *testing.T) {
	condition, err := TranslateTriggerCondition([]workflow.Trigger{{Event: "pull_request", BranchesIgnore: []string{"docs/**"}, Types: []string{"labeled", "ready_for_review"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`build.pull_request.base_branch`, `!(build.pull_request.base_branch =~ /^docs\/.*$/)`, `build.source_action == "labeled"`, `build.source_action == "ready_for_review"`} {
		if !strings.Contains(condition, want) {
			t.Errorf("pull request condition missing %q:\n%s", want, condition)
		}
	}
	if strings.Contains(condition, `source_action == "opened"`) {
		t.Fatalf("explicit types gained default action:\n%s", condition)
	}
}
