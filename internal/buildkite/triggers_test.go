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
		{Event: "pull_request", Paths: []string{"!src/**"}},
	}, "push", LiveTriggerConditionContext("true"))
	if err == nil || !strings.Contains(err.Error(), "must follow a positive pattern") {
		t.Fatalf("inactive path filter error = %v", err)
	}
}

func TestTranslateEventTriggerConditionRejectsInactivePushPathFilters(t *testing.T) {
	_, _, err := TranslateEventTriggerCondition([]workflow.Trigger{
		{Event: "push", Paths: []string{"src/**"}},
		{Event: "pull_request"},
	}, "pull_request", TriggerConditionContext{EventPredicate: "true", PullRequestAction: `"opened"`})
	if err == nil || !strings.Contains(err.Error(), "path filters are unsupported") {
		t.Fatalf("inactive push path filter error = %v", err)
	}
}

func TestTranslateEventTriggerConditionMatchesPullRequestPaths(t *testing.T) {
	context := TriggerConditionContext{
		EventPredicate:        "true",
		PullRequestBaseBranch: `"main"`,
		PullRequestAction:     `"opened"`,
		ChangedPaths:          []string{"docs/readme.md", "src/main.go"},
		ChangedPathsKnown:     true,
	}
	for _, test := range []struct {
		name    string
		trigger workflow.Trigger
		want    string
	}{
		{
			name:    "ordered re-inclusion",
			trigger: workflow.Trigger{Event: "pull_request", Paths: []string{"**", "!docs/**", "docs/required/**"}},
			want:    "true",
		},
		{
			name:    "include mismatch",
			trigger: workflow.Trigger{Event: "pull_request", Paths: []string{"web/**"}},
			want:    "false",
		},
		{
			name:    "ignore runs for one unignored path",
			trigger: workflow.Trigger{Event: "pull_request", PathsIgnore: []string{"docs/**"}},
			want:    "true",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{test.trigger}, "pull_request", context)
			if err != nil || !applicable || !strings.HasSuffix(condition, "&& "+test.want+")") {
				t.Fatalf("condition/applicable/error = %q / %t / %v, want %q", condition, applicable, err, test.want)
			}
		})
	}
}

func TestPathFiltersMatchOrderedPatternsPerPath(t *testing.T) {
	matched, err := pathFiltersMatch(
		[]string{"docs/required/release.md"},
		[]string{"**", "!docs/**", "docs/required/**"},
		nil,
	)
	if err != nil || !matched {
		t.Fatalf("ordered path match = %t, %v", matched, err)
	}
	matched, err = pathFiltersMatch([]string{"docs/readme.md"}, nil, []string{"docs/**"})
	if err != nil || matched {
		t.Fatalf("ignored-only path match = %t, %v", matched, err)
	}
	for _, path := range []string{"README.md", "docs/README.md"} {
		matched, err = pathFiltersMatch([]string{path}, []string{"**/README.md"}, nil)
		if err != nil || !matched {
			t.Fatalf("globstar path %q match = %t, %v", path, matched, err)
		}
	}
	matched, err = pathFiltersMatch([]string{"docs/line\nbreak.md"}, []string{"docs/**"}, nil)
	if err != nil || !matched {
		t.Fatalf("newline path match = %t, %v", matched, err)
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

func TestTranslateEventTriggerConditionDistinguishesPullRequestBaseFromPushBranch(t *testing.T) {
	triggers := []workflow.Trigger{
		{Event: "push", Branches: []string{"main"}},
		{Event: "pull_request", Branches: []string{"main"}},
	}
	context := TriggerConditionContext{
		EventPredicate:        `build.source == "webhook"`,
		Branch:                `"chore_updates"`,
		Tag:                   "null",
		PullRequestBaseBranch: `"main"`,
		PullRequestAction:     `"opened"`,
	}

	push, applicable, err := TranslateEventTriggerCondition(triggers, "push", context)
	if err != nil || !applicable || !strings.Contains(push, `"chore_updates" =~ /^main$/`) {
		t.Fatalf("snapshot push condition = %q, %t, %v, want build branch", push, applicable, err)
	}

	pullRequest, applicable, err := TranslateEventTriggerCondition(triggers, "pull_request", context)
	if err != nil || !applicable || !strings.Contains(pullRequest, `"main" =~ /^main$/`) || strings.Contains(pullRequest, "chore_updates") {
		t.Fatalf("snapshot pull request condition = %q, %t, %v, want base branch", pullRequest, applicable, err)
	}
}

func TestTranslateEventTriggerConditionRejectsIncompletePullRequestSnapshot(t *testing.T) {
	context := TriggerConditionContext{
		EventPredicate:        "true",
		PullRequestBaseBranch: `"main"`,
		PullRequestAction:     "null",
	}
	if _, _, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request"}}, "pull_request", context); err == nil || !strings.Contains(err.Error(), "payload.action") {
		t.Fatalf("missing action error = %v", err)
	}

	context.PullRequestAction = `"opened"`
	context.PullRequestBaseBranch = "null"
	if _, _, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request", Branches: []string{"main"}}}, "pull_request", context); err == nil || !strings.Contains(err.Error(), "payload.pull_request.base.ref") {
		t.Fatalf("missing filtered base branch error = %v", err)
	}

	if _, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request"}}, "pull_request", context); err != nil || !applicable {
		t.Fatalf("unfiltered pull request with omitted base branch = applicable %t, error %v", applicable, err)
	}
}

func TestTranslateEventTriggerConditionRejectsUnclassifiablePushSnapshot(t *testing.T) {
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{
		Event:    "push",
		Branches: []string{"main"},
	}}, "push", TriggerConditionContext{
		EventPredicate: "true",
		Branch:         "null",
		Tag:            "null",
	})
	if err == nil || !strings.Contains(err.Error(), "refs/heads/") {
		t.Fatalf("unclassifiable push error = %v", err)
	}
	if condition != "" || applicable {
		t.Fatalf("unclassifiable push condition/applicability = %q / %t", condition, applicable)
	}
}

func TestTriggerEventSkipReason(t *testing.T) {
	triggers := []workflow.Trigger{
		{Event: "push", Branches: []string{"main"}},
		{Event: "pull_request"},
	}
	if got := TriggerEventSkipReason(triggers, "push"); got != "" {
		t.Fatalf("push skip reason = %q, want empty for a matching on entry", got)
	}
	if got, want := TriggerEventSkipReason(triggers, "schedule"), "This workflow is not triggered by a `schedule` event"; got != want {
		t.Fatalf("schedule skip reason = %q, want %q", got, want)
	}
	if got, want := TriggerEventSkipReason(triggers, strings.Repeat("x", maxSkipReasonLength)), "This workflow is not triggered by the current event"; got != want {
		t.Fatalf("long event skip reason = %q, want %q", got, want)
	}
}

func TestTriggerFilterMismatchReason(t *testing.T) {
	value := func(value string) *string { return &value }
	for _, test := range []struct {
		name    string
		trigger workflow.Trigger
		event   string
		context TriggerConditionContext
		want    string
	}{
		{
			name:    "push branch mismatch",
			trigger: workflow.Trigger{Event: "push", Branches: []string{"main", "development"}},
			event:   "push",
			context: TriggerConditionContext{BranchValue: value("feature")},
			want:    "Only runs on `main` or `development`.",
		},
		{
			name:    "push branch mismatch with several configured branches",
			trigger: workflow.Trigger{Event: "push", Branches: []string{"main", "development", "staging", "production"}},
			event:   "push",
			context: TriggerConditionContext{BranchValue: value("feature")},
			want:    "Only runs on `main`, `development`, `staging`, or `production`.",
		},
		{
			name:    "push branch exclusion mismatch",
			trigger: workflow.Trigger{Event: "push", BranchesIgnore: []string{"feature"}},
			event:   "push",
			context: TriggerConditionContext{BranchValue: value("feature")},
			want:    "Doesn’t run on the `feature` branch.",
		},
		{
			name:    "ordered push branch match",
			trigger: workflow.Trigger{Event: "push", Branches: []string{"release/**", "!release/**-alpha", "release/special-alpha"}},
			event:   "push",
			context: TriggerConditionContext{BranchValue: value("release/special-alpha")},
		},
		{
			name:    "pull request path mismatch",
			trigger: workflow.Trigger{Event: "pull_request", Paths: []string{"src/**"}},
			event:   "pull_request",
			context: TriggerConditionContext{ChangedPathsKnown: true, ChangedPaths: []string{"docs/readme.md"}},
			want:    "Changed paths do not match this workflow's pull_request path filters.",
		},
		{
			name:    "push tag mismatch",
			trigger: workflow.Trigger{Event: "push", TagsIgnore: []string{"v0"}},
			event:   "push",
			context: TriggerConditionContext{TagValue: value("v0")},
			want:    `Tag "v0" does not match this workflow's push tag filters.`,
		},
		{
			name:    "branch push with only tag filters",
			trigger: workflow.Trigger{Event: "push", Tags: []string{"v*"}},
			event:   "push",
			context: TriggerConditionContext{BranchValue: value("main")},
			want:    `Branch push "main" does not match this workflow's push tag filters.`,
		},
		{
			name:    "tag push with only branch filters",
			trigger: workflow.Trigger{Event: "push", Branches: []string{"main"}},
			event:   "push",
			context: TriggerConditionContext{TagValue: value("v1")},
			want:    `Tag push "v1" does not match this workflow's push branch filters.`,
		},
		{
			name:    "pull request base mismatch",
			trigger: workflow.Trigger{Event: "pull_request", Branches: []string{"main"}},
			event:   "pull_request",
			context: TriggerConditionContext{PullRequestBaseValue: value("development"), PullRequestActionValue: value("opened")},
			want:    `Base branch "development" does not match this workflow's pull_request branch filters.`,
		},
		{
			name:    "pull request activity mismatch",
			trigger: workflow.Trigger{Event: "pull_request"},
			event:   "pull_request",
			context: TriggerConditionContext{PullRequestActionValue: value("closed")},
			want:    `Pull request activity "closed" does not match this workflow's pull_request activity filters.`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := TriggerFilterMismatchReason([]workflow.Trigger{test.trigger}, test.event, test.context)
			if err != nil || got != test.want {
				t.Fatalf("TriggerFilterMismatchReason() = %q, %v, want %q", got, err, test.want)
			}
		})
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
