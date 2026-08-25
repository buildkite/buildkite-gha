package buildkite

import (
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestTranslateTriggerCondition(t *testing.T) {
	got, err := TranslateTriggerCondition([]workflow.Trigger{
		{Event: "push", Branches: []string{"main", "releases/**", "!releases/**-alpha", "releases/v[0-9]+"}},
		{Event: "pull_request"}, {Event: "merge_group", Branches: []string{"main"}}, {Event: "release", Types: []string{"published", "released"}}, {Event: "issues", Types: []string{"opened", "typed"}}, {Event: "workflow_dispatch"}, {Event: "schedule"}, {Event: "workflow_call"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`build.env("BUILDKITE_GITHUB_EVENT") == "push"`, `build.branch =~ /^main$/`, `releases\/.*`, `build.env("BUILDKITE_GITHUB_EVENT") == "pull_request"`, `build.source_action == "opened"`, `build.source_action == "synchronize"`, `build.env("BUILDKITE_GITHUB_EVENT") == "merge_group"`, `build.merge_queue.base_branch =~ /^main$/`, `build.source_action == "checks_requested"`, `build.env("BUILDKITE_GITHUB_EVENT") == "workflow_dispatch"`, `build.env("BUILDKITE_GITHUB_EVENT") == "schedule"`} {
		if !strings.Contains(got, want) {
			t.Errorf("condition missing %q:\n%s", want, got)
		}
	}
	for _, want := range []string{`build.env("BUILDKITE_GITHUB_EVENT") == "release"`, `build.source_action == "published"`, `build.source_action == "released"`} {
		if !strings.Contains(got, want) {
			t.Errorf("release condition missing %q:\n%s", want, got)
		}
	}
	for _, want := range []string{`build.env("BUILDKITE_GITHUB_EVENT") == "issues"`, `build.source_action == "opened"`, `build.source_action == "typed"`} {
		if !strings.Contains(got, want) {
			t.Errorf("issues condition missing %q:\n%s", want, got)
		}
	}
}

func TestLiveEventPredicatePreservesNonWebhookMappings(t *testing.T) {
	tests := []struct {
		event    string
		wants    []string
		excludes []string
	}{
		{event: "push", wants: []string{`build.env("BUILDKITE_GITHUB_EVENT") == "push"`, `build.env("BUILDKITE_GITHUB_EVENT") == null`, `build.env("BUILDKITE_GITHUB_EVENT") != "push"`, `build.env("BUILDKITE_GITHUB_EVENT") != "pull_request"`, `build.env("BUILDKITE_GITHUB_EVENT") != "workflow_dispatch"`, `build.env("BUILDKITE_GITHUB_EVENT") != "schedule"`, `build.pull_request.id == null`, `build.source != "ui"`, `build.source != "api"`, `build.source != "schedule"`}},
		{event: "pull_request", wants: []string{`build.env("BUILDKITE_GITHUB_EVENT") == "pull_request"`, `build.env("BUILDKITE_GITHUB_EVENT") == null`, `build.pull_request.id != null`}},
		{event: "workflow_dispatch", wants: []string{`build.env("BUILDKITE_GITHUB_EVENT") == "workflow_dispatch"`, `build.env("BUILDKITE_GITHUB_EVENT") == null`, `build.pull_request.id == null`, `build.source == "ui"`, `build.source == "api"`}},
		{event: "schedule", wants: []string{`build.env("BUILDKITE_GITHUB_EVENT") == "schedule"`, `build.env("BUILDKITE_GITHUB_EVENT") == null`, `build.pull_request.id == null`, `build.source == "schedule"`}},
		{event: "merge_group", wants: []string{`build.env("BUILDKITE_GITHUB_EVENT") == "merge_group"`}, excludes: []string{" == null", "build.source"}},
		{event: "release", wants: []string{`build.env("BUILDKITE_GITHUB_EVENT") == "release"`}, excludes: []string{" == null", "build.source"}},
		{event: "issues", wants: []string{`build.env("BUILDKITE_GITHUB_EVENT") == "issues"`}, excludes: []string{" == null", "build.source"}},
	}
	for _, test := range tests {
		t.Run(test.event, func(t *testing.T) {
			predicate := LiveEventPredicate(test.event)
			for _, want := range test.wants {
				if !strings.Contains(predicate, want) {
					t.Errorf("predicate missing %q: %s", want, predicate)
				}
			}
			for _, excluded := range test.excludes {
				if strings.Contains(predicate, excluded) {
					t.Errorf("predicate contains %q: %s", excluded, predicate)
				}
			}
			if len(test.wants) > 1 && (!strings.HasPrefix(predicate, "(") || !strings.HasSuffix(predicate, ")")) {
				t.Errorf("fallback predicate is not grouped: %s", predicate)
			}
		})
	}
	if predicate := LiveEventPredicate("issue_comment"); predicate != "" {
		t.Fatalf("unsupported predicate = %q", predicate)
	}
}

func TestTranslateTriggerConditionRejectsUnsafeTriggers(t *testing.T) {
	tests := []struct {
		name     string
		triggers []workflow.Trigger
		want     string
	}{
		{name: "paths", triggers: []workflow.Trigger{{Event: "push", Paths: []string{"src/**"}}}, want: "path filters are unsupported"},
		{name: "event", triggers: []workflow.Trigger{{Event: "issue_comment"}}, want: "unsupported GitHub trigger"},
		{name: "mixed include ignore", triggers: []workflow.Trigger{{Event: "push", Branches: []string{"main"}, BranchesIgnore: []string{"release"}}}, want: "cannot be combined"},
		{name: "leading negative", triggers: []workflow.Trigger{{Event: "push", Branches: []string{"!release/**"}}}, want: "must follow a positive"},
		{name: "unsupported PR type", triggers: []workflow.Trigger{{Event: "pull_request", Types: []string{"not-real"}}}, want: "cannot be mapped exactly"},
		{name: "unsupported merge group type", triggers: []workflow.Trigger{{Event: "merge_group", Types: []string{"destroyed"}}}, want: `merge_group type "destroyed" is unsupported`},
		{name: "merge group paths", triggers: []workflow.Trigger{{Event: "merge_group", Paths: []string{"src/**"}}}, want: "path filters are unsupported"},
		{name: "bare release", triggers: []workflow.Trigger{{Event: "release"}}, want: "on: release needs a types list"},
		{name: "release unpublished", triggers: []workflow.Trigger{{Event: "release", Types: []string{"unpublished"}}}, want: "cannot be mapped exactly"},
		{name: "release edited", triggers: []workflow.Trigger{{Event: "release", Types: []string{"edited"}}}, want: "cannot be mapped exactly"},
		{name: "release deleted", triggers: []workflow.Trigger{{Event: "release", Types: []string{"deleted"}}}, want: "cannot be mapped exactly"},
		{name: "release prereleased", triggers: []workflow.Trigger{{Event: "release", Types: []string{"prereleased"}}}, want: "cannot be mapped exactly"},
		{name: "release branch filter", triggers: []workflow.Trigger{{Event: "release", Types: []string{"published"}, Branches: []string{"main"}}}, want: "unsupported filters"},
		{name: "release paths", triggers: []workflow.Trigger{{Event: "release", Types: []string{"published"}, Paths: []string{"src/**"}}}, want: "path filters are unsupported"},
		{name: "empty issues types", triggers: []workflow.Trigger{{Event: "issues", Types: []string{}}}, want: "issues types is explicitly empty"},
		{name: "unknown issues type", triggers: []workflow.Trigger{{Event: "issues", Types: []string{"field_added"}}}, want: `issues activity type "field_added" cannot be mapped exactly`},
		{name: "issues branches", triggers: []workflow.Trigger{{Event: "issues", Branches: []string{"main"}}}, want: "issues has unsupported filters"},
		{name: "issues tags", triggers: []workflow.Trigger{{Event: "issues", Tags: []string{"v*"}}}, want: "issues has unsupported filters"},
		{name: "issues paths", triggers: []workflow.Trigger{{Event: "issues", Paths: []string{"src/**"}}}, want: "issues path filters are unsupported"},
		{name: "issues workflows", triggers: []workflow.Trigger{{Event: "issues", Workflows: []string{"CI"}}}, want: "issues has unsupported filters"},
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

func TestTranslateTriggerConditionReportsActionableTriggerErrors(t *testing.T) {
	tests := []struct {
		name    string
		trigger workflow.Trigger
		want    string
	}{
		{
			name:    "unsupported merge group type",
			trigger: workflow.Trigger{Event: "merge_group", Types: []string{"destroyed"}},
			want:    `merge_group type "destroyed" is unsupported. checks_requested is the only merge queue activity currently mapped. Set types: [checks_requested]. If you need another merge_group type, open an issue in https://github.com/buildkite/buildkite-gha so we can prioritize it`,
		},
		{
			name:    "bare release",
			trigger: workflow.Trigger{Event: "release"},
			want:    `on: release needs a types list. A bare release covers every release event, while the currently supported types are exactly published, created, and released. Use on: {release: {types: [published]}}. If you need another release type, open an issue in https://github.com/buildkite/buildkite-gha so we can prioritize it`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := TranslateTriggerCondition([]workflow.Trigger{test.trigger})
			if err == nil || err.Error() != test.want {
				t.Fatalf("TranslateTriggerCondition() error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestTranslateEventTriggerConditionUsesMergeGroupSnapshot(t *testing.T) {
	base, action := "main", "checks_requested"
	expressions := TriggerConditionExpressions{
		EventPredicate:       `build.source == "webhook"`,
		MergeGroupBaseBranch: `"main"`,
		MergeGroupAction:     `"checks_requested"`,
	}
	snapshot := TriggerEventSnapshot{
		MergeGroupBaseBranch: &base,
		MergeGroupAction:     &action,
	}
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{
		Event: "merge_group", Branches: []string{"main"}, Types: []string{"checks_requested"},
	}}, "merge_group", expressions, snapshot)
	if err != nil || !applicable {
		t.Fatalf("condition/applicable/error = %q / %t / %v", condition, applicable, err)
	}
	for _, want := range []string{`build.source == "webhook"`, `"main" =~ /^main$/`, `"checks_requested" == "checks_requested"`} {
		if !strings.Contains(condition, want) {
			t.Fatalf("merge_group condition missing %q: %s", want, condition)
		}
	}
}

func TestTranslateEventTriggerConditionUsesReleaseSnapshot(t *testing.T) {
	action := "released"
	expressions := TriggerConditionExpressions{
		EventPredicate: `build.source == "webhook"`,
		ReleaseAction:  `"released"`,
	}
	snapshot := TriggerEventSnapshot{
		ReleaseAction: &action,
	}
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{
		Event: "release", Types: []string{"published", "released"},
	}}, "release", expressions, snapshot)
	if err != nil || !applicable {
		t.Fatalf("condition/applicable/error = %q / %t / %v", condition, applicable, err)
	}
	for _, want := range []string{`build.source == "webhook"`, `"released" == "published"`, `"released" == "released"`} {
		if !strings.Contains(condition, want) {
			t.Fatalf("release condition missing %q: %s", want, condition)
		}
	}
	reason, err := TriggerFilterMismatchReason(
		[]workflow.Trigger{{Event: "release", Types: []string{"published"}}},
		"release", snapshot,
	)
	if err != nil || !strings.Contains(reason, `"released"`) {
		t.Fatalf("release mismatch reason = %q, %v", reason, err)
	}
}

func TestTranslateEventTriggerConditionUsesIssuesSnapshot(t *testing.T) {
	action := "typed"
	condition, applicable, err := TranslateEventTriggerCondition(
		[]workflow.Trigger{{Event: "issues", Types: []string{"opened", "typed"}}},
		"issues",
		TriggerConditionExpressions{EventPredicate: `build.source == "webhook"`, IssuesAction: `"typed"`},
		TriggerEventSnapshot{IssuesAction: &action},
	)
	if err != nil || !applicable {
		t.Fatalf("condition/applicable/error = %q / %t / %v", condition, applicable, err)
	}
	for _, want := range []string{`build.source == "webhook"`, `"typed" == "opened"`, `"typed" == "typed"`} {
		if !strings.Contains(condition, want) {
			t.Fatalf("issues condition missing %q: %s", want, condition)
		}
	}
	reason, err := TriggerFilterMismatchReason(
		[]workflow.Trigger{{Event: "issues", Types: []string{"opened"}}},
		"issues", TriggerEventSnapshot{IssuesAction: &action},
	)
	if err != nil || !strings.Contains(reason, `"typed"`) {
		t.Fatalf("issues mismatch reason = %q, %v", reason, err)
	}
}

func TestTranslateTriggerConditionAcceptsDocumentedIssuesActivityTypes(t *testing.T) {
	types := []string{
		"opened", "edited", "deleted", "transferred", "pinned", "unpinned",
		"closed", "reopened", "assigned", "unassigned", "labeled", "unlabeled",
		"locked", "unlocked", "milestoned", "demilestoned", "typed", "untyped",
	}
	condition, err := TranslateTriggerCondition([]workflow.Trigger{{Event: "issues", Types: types}})
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range types {
		if !strings.Contains(condition, `build.source_action == "`+activity+`"`) {
			t.Errorf("condition missing documented issues activity %q: %s", activity, condition)
		}
	}
}

func TestTranslateTriggerConditionRequiresDirectBuildSource(t *testing.T) {
	_, err := TranslateTriggerCondition([]workflow.Trigger{{Event: "workflow_call"}})
	if err == nil || !strings.Contains(err.Error(), "no supported build source") {
		t.Fatalf("TranslateTriggerCondition(workflow_call) error = %v", err)
	}
}

func TestTranslateTriggerConditionIgnoresUnsupportedEventsBesideSupportedOnes(t *testing.T) {
	got, err := TranslateTriggerCondition([]workflow.Trigger{
		{Event: "push"},
		{Event: "issue_comment", Types: []string{"created"}},
		{Event: "pull_request_target", Paths: []string{"src/**"}, Branches: []string{"main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `build.env("BUILDKITE_GITHUB_EVENT") == "push"`) || strings.Contains(got, "issue_comment") || strings.Contains(got, "pull_request_target") {
		t.Fatalf("condition = %q", got)
	}
}

func TestValidateTriggerConditionsIgnoresUnsupportedEventsBesideSupportedOnes(t *testing.T) {
	if err := ValidateTriggerConditions([]workflow.Trigger{
		{Event: "push"},
		{Event: "issue_comment", Types: []string{"created"}},
		{Event: "pull_request_target", Paths: []string{"src/**"}},
		{Event: "workflow_run"},
	}); err != nil {
		t.Fatalf("ValidateTriggerConditions() error = %v", err)
	}
	err := ValidateTriggerConditions([]workflow.Trigger{{Event: "discussion"}, {Event: "issue_comment"}})
	if err == nil || !strings.Contains(err.Error(), `unsupported GitHub trigger event "discussion"`) || !strings.Contains(err.Error(), `unsupported GitHub trigger event "issue_comment"`) {
		t.Fatalf("ValidateTriggerConditions() error = %v", err)
	}
}

func TestTranslateEventTriggerConditionIgnoresUnsupportedEvents(t *testing.T) {
	expressions := TriggerConditionExpressions{
		EventPredicate: `build.env("BUILDKITE_GITHUB_EVENT") == "push"`,
		Branch:         "build.branch",
		Tag:            "build.tag",
	}
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{
		{Event: "push"}, {Event: "issue_comment"}, {Event: "pull_request_target", Paths: []string{"src/**"}},
	}, "push", expressions, TriggerEventSnapshot{})
	if err != nil || !applicable {
		t.Fatalf("condition/applicable/error = %q / %t / %v", condition, applicable, err)
	}
	if !strings.Contains(condition, `build.env("BUILDKITE_GITHUB_EVENT") == "push"`) {
		t.Fatalf("condition = %q", condition)
	}
	condition, applicable, err = TranslateEventTriggerCondition([]workflow.Trigger{
		{Event: "issue_comment"},
	}, "push", expressions, TriggerEventSnapshot{})
	if err != nil || applicable || condition != "" {
		t.Fatalf("condition/applicable/error = %q / %t / %v", condition, applicable, err)
	}
}

func TestSupportedTriggerEvent(t *testing.T) {
	for event := range supportedTriggerEvents {
		trigger := workflow.Trigger{Event: event}
		if event == "release" {
			trigger.Types = []string{"published"}
		}
		_, _, err := translateTrigger(trigger, liveTriggerExpressions(event), TriggerEventSnapshot{}, true)
		if unsupportedTriggerEvent(err) {
			t.Errorf("translateTrigger(%q) reports an unsupported event", event)
		}
	}
	_, _, err := translateTrigger(workflow.Trigger{Event: "issue_comment"}, liveTriggerExpressions("issue_comment"), TriggerEventSnapshot{}, true)
	if SupportedTriggerEvent("issue_comment") || !unsupportedTriggerEvent(err) {
		t.Errorf("issue_comment must be an unsupported trigger event, error = %v", err)
	}
}

func TestValidateTriggerConditionsAllowsReusableOnlyWorkflow(t *testing.T) {
	if err := ValidateTriggerConditions([]workflow.Trigger{{Event: "workflow_call"}}); err != nil {
		t.Fatalf("ValidateTriggerConditions(workflow_call) error = %v", err)
	}
	if err := ValidateTriggerConditions([]workflow.Trigger{{Event: "issue_comment"}}); err == nil || !strings.Contains(err.Error(), "unsupported GitHub trigger") {
		t.Fatalf("ValidateTriggerConditions(issue_comment) error = %v", err)
	}
}

func TestValidateTriggerConditionsAcceptsSupportedPathFilters(t *testing.T) {
	if err := ValidateTriggerConditions([]workflow.Trigger{
		{Event: "pull_request", Paths: []string{"src/**"}},
		{Event: "push", Paths: []string{"docs/**"}},
	}); err != nil {
		t.Fatalf("ValidateTriggerConditions() path filters error = %v", err)
	}

	err := ValidateTriggerConditions([]workflow.Trigger{{Event: "pull_request", Paths: []string{"!src/**"}}})
	if err == nil || !strings.Contains(err.Error(), "must follow a positive pattern") {
		t.Fatalf("ValidateTriggerConditions() invalid pull request path error = %v", err)
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
	condition, applicable, err := TranslateEventTriggerCondition(triggers, "push", LiveTriggerConditionExpressions(`build.source == "trigger_job"`), TriggerEventSnapshot{})
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

	condition, applicable, err = TranslateEventTriggerCondition(triggers, "issues", LiveTriggerConditionExpressions("true"), TriggerEventSnapshot{})
	if err != nil || applicable || condition != "" {
		t.Fatalf("non-applicable condition = %q, %t, %v", condition, applicable, err)
	}
}

func TestTranslateEventTriggerConditionValidatesInactiveTriggers(t *testing.T) {
	_, _, err := TranslateEventTriggerCondition([]workflow.Trigger{
		{Event: "push"},
		{Event: "pull_request", Paths: []string{"!src/**"}},
	}, "push", LiveTriggerConditionExpressions("true"), TriggerEventSnapshot{})
	if err == nil || !strings.Contains(err.Error(), "must follow a positive pattern") {
		t.Fatalf("inactive path filter error = %v", err)
	}
}

func TestTranslateEventTriggerConditionAllowsInactivePushPathFilters(t *testing.T) {
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{
		{Event: "push", Paths: []string{"src/**"}},
		{Event: "pull_request"},
	}, "pull_request", TriggerConditionExpressions{EventPredicate: "true", PullRequestAction: `"opened"`}, TriggerEventSnapshot{})
	if err != nil || !applicable || !strings.Contains(condition, `"opened"`) {
		t.Fatalf("inactive push path filter condition/applicable/error = %q / %t / %v", condition, applicable, err)
	}
}

func TestTranslateEventTriggerConditionMatchesPullRequestPaths(t *testing.T) {
	expressions := TriggerConditionExpressions{
		EventPredicate:        "true",
		PullRequestBaseBranch: `"main"`,
		PullRequestAction:     `"opened"`,
	}
	snapshot := TriggerEventSnapshot{
		ChangedPaths: ChangedPathEvaluation{Paths: []string{"docs/readme.md", "src/main.go"}},
	}
	for _, test := range []struct {
		name    string
		trigger workflow.Trigger
	}{
		{
			name:    "ordered re-inclusion",
			trigger: workflow.Trigger{Event: "pull_request", Paths: []string{"**", "!docs/**", "docs/required/**"}},
		},
		{
			name:    "ignore runs for one unignored path",
			trigger: workflow.Trigger{Event: "pull_request", PathsIgnore: []string{"docs/**"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{test.trigger}, "pull_request", expressions, snapshot)
			if err != nil || !applicable || strings.Contains(condition, "false") {
				t.Fatalf("condition/applicable/error = %q / %t / %v", condition, applicable, err)
			}
		})
	}
	_, _, err := TranslateEventTriggerCondition(
		[]workflow.Trigger{{Event: "pull_request", Paths: []string{"web/**"}}},
		"pull_request",
		expressions,
		snapshot,
	)
	if err == nil || !strings.Contains(err.Error(), "diff-timeout outcome is unavailable") {
		t.Fatalf("nonmatching local paths error = %v", err)
	}
}

func TestTranslateEventTriggerConditionTreatsEmptyChangedPathsAsAvailable(t *testing.T) {
	_, _, err := TranslateEventTriggerCondition(
		[]workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}},
		"pull_request",
		TriggerConditionExpressions{EventPredicate: "true", PullRequestAction: `"opened"`},
		TriggerEventSnapshot{ChangedPaths: ChangedPathEvaluation{Paths: []string{}}},
	)
	if err == nil || !strings.Contains(err.Error(), "local changed paths do not match") {
		t.Fatalf("empty available changed paths error = %v", err)
	}
}

func TestTranslateEventTriggerConditionMatchesPushPaths(t *testing.T) {
	branch := "main"
	expressions := TriggerConditionExpressions{
		EventPredicate: "true",
		Branch:         `"main"`,
		Tag:            "null",
	}
	snapshot := TriggerEventSnapshot{
		Branch:       &branch,
		ChangedPaths: ChangedPathEvaluation{Paths: []string{"docs/readme.md", "src/generated/api.go", "src/main.go"}},
	}
	trigger := workflow.Trigger{Event: "push", Paths: []string{"src/**", "!src/generated/**"}}
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{trigger}, "push", expressions, snapshot)
	if err != nil || !applicable || condition != "(true)" {
		t.Fatalf("push condition/applicable/error = %q / %t / %v", condition, applicable, err)
	}
	snapshot.ChangedPaths = ChangedPathEvaluation{Paths: []string{"docs/readme.md", "src/generated/api.go"}}
	if _, _, err := TranslateEventTriggerCondition([]workflow.Trigger{trigger}, "push", expressions, snapshot); err == nil || !strings.Contains(err.Error(), "diff-timeout outcome is unavailable") {
		t.Fatalf("nonmatching push paths error = %v", err)
	}
	snapshot.ChangedPaths = ChangedPathEvaluation{UnavailableReason: "verified push diff unavailable"}
	if _, _, err := TranslateEventTriggerCondition([]workflow.Trigger{trigger}, "push", expressions, snapshot); err == nil || !strings.Contains(err.Error(), "verified push diff unavailable") {
		t.Fatalf("unknown push paths error = %v", err)
	}

	tag := "v1.0.0"
	tagExpressions := TriggerConditionExpressions{EventPredicate: "true", Branch: "null", Tag: `"v1.0.0"`}
	tagSnapshot := TriggerEventSnapshot{Tag: &tag}
	if _, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{trigger}, "push", tagExpressions, tagSnapshot); err != nil || !applicable {
		t.Fatalf("tag push path filter = %t, %v", applicable, err)
	}

	snapshot.ChangedPaths = ChangedPathEvaluation{UnavailableReason: "verified push diff unavailable"}
	trigger.Branches = []string{"release"}
	if condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{trigger}, "push", expressions, snapshot); err != nil || !applicable || !strings.Contains(condition, `"main" =~ /^release$/`) {
		t.Fatalf("branch-mismatched push path filter = %q, %t, %v", condition, applicable, err)
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
	if matched, err = pathFiltersMatch([]string{"foobar"}, []string{"foo**/bar"}, nil); err != nil || matched {
		t.Fatalf("non-segment globstar match = %t, %v", matched, err)
	}
	matched, err = pathFiltersMatch([]string{"docs/line\nbreak.md"}, []string{"docs/**"}, nil)
	if err != nil || !matched {
		t.Fatalf("newline path match = %t, %v", matched, err)
	}
	if _, err := pathFiltersMatch([]string{"foobar"}, []string{`foo\bar`}, nil); err == nil || !strings.Contains(err.Error(), "unsupported backslash") {
		t.Fatalf("backslash path glob error = %v", err)
	}
	if _, err := pathFiltersMatch([]string{"a/b"}, []string{`a[^x]b`}, nil); err == nil || !strings.Contains(err.Error(), "invalid character class") {
		t.Fatalf("unsupported character class error = %v", err)
	}
	if matched, err := pathFiltersMatch([]string{"src/file7.go"}, []string{`src/file[0-9].go`}, nil); err != nil || !matched {
		t.Fatalf("documented character class match = %t, %v", matched, err)
	}
}

func TestTranslateEventTriggerConditionUsesSnapshotExpressions(t *testing.T) {
	expressions := TriggerConditionExpressions{
		EventPredicate:        "true",
		Branch:                `"main"`,
		Tag:                   `"v1"`,
		PullRequestBaseBranch: `"release"`,
		PullRequestAction:     `"opened"`,
	}
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "push", Branches: []string{"main"}, Tags: []string{"v*"}}}, "push", expressions, TriggerEventSnapshot{})
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

	condition, applicable, err = TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request", Branches: []string{"release"}, Types: []string{"opened"}}}, "pull_request", expressions, TriggerEventSnapshot{})
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
	expressions := TriggerConditionExpressions{
		EventPredicate:        `build.source == "webhook"`,
		Branch:                `"chore_updates"`,
		Tag:                   "null",
		PullRequestBaseBranch: `"main"`,
		PullRequestAction:     `"opened"`,
	}

	push, applicable, err := TranslateEventTriggerCondition(triggers, "push", expressions, TriggerEventSnapshot{})
	if err != nil || !applicable || !strings.Contains(push, `"chore_updates" =~ /^main$/`) {
		t.Fatalf("snapshot push condition = %q, %t, %v, want build branch", push, applicable, err)
	}

	pullRequest, applicable, err := TranslateEventTriggerCondition(triggers, "pull_request", expressions, TriggerEventSnapshot{})
	if err != nil || !applicable || !strings.Contains(pullRequest, `"main" =~ /^main$/`) || strings.Contains(pullRequest, "chore_updates") {
		t.Fatalf("snapshot pull request condition = %q, %t, %v, want base branch", pullRequest, applicable, err)
	}
}

func TestTranslateEventTriggerConditionRejectsIncompletePullRequestSnapshot(t *testing.T) {
	expressions := TriggerConditionExpressions{
		EventPredicate:        "true",
		PullRequestBaseBranch: `"main"`,
		PullRequestAction:     "null",
	}
	if _, _, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request"}}, "pull_request", expressions, TriggerEventSnapshot{}); err == nil || !strings.Contains(err.Error(), "payload.action") {
		t.Fatalf("missing action error = %v", err)
	}

	expressions.PullRequestAction = `"opened"`
	expressions.PullRequestBaseBranch = "null"
	if _, _, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request", Branches: []string{"main"}}}, "pull_request", expressions, TriggerEventSnapshot{}); err == nil || !strings.Contains(err.Error(), "payload.pull_request.base.ref") {
		t.Fatalf("missing filtered base branch error = %v", err)
	}

	if _, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{Event: "pull_request"}}, "pull_request", expressions, TriggerEventSnapshot{}); err != nil || !applicable {
		t.Fatalf("unfiltered pull request with omitted base branch = applicable %t, error %v", applicable, err)
	}
}

func TestTranslateEventTriggerConditionRejectsUnclassifiablePushSnapshot(t *testing.T) {
	condition, applicable, err := TranslateEventTriggerCondition([]workflow.Trigger{{
		Event:    "push",
		Branches: []string{"main"},
	}}, "push", TriggerConditionExpressions{
		EventPredicate: "true",
		Branch:         "null",
		Tag:            "null",
	}, TriggerEventSnapshot{})
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
		name     string
		trigger  workflow.Trigger
		event    string
		snapshot TriggerEventSnapshot
		want     string
	}{
		{
			name:     "push branch mismatch",
			trigger:  workflow.Trigger{Event: "push", Branches: []string{"main", "development"}},
			event:    "push",
			snapshot: TriggerEventSnapshot{Branch: value("feature")},
			want:     "Only runs on `main` or `development`.",
		},
		{
			name:     "push branch mismatch with several configured branches",
			trigger:  workflow.Trigger{Event: "push", Branches: []string{"main", "development", "staging", "production"}},
			event:    "push",
			snapshot: TriggerEventSnapshot{Branch: value("feature")},
			want:     "Only runs on `main`, `development`, `staging`, or `production`.",
		},
		{
			name:     "push branch exclusion mismatch",
			trigger:  workflow.Trigger{Event: "push", BranchesIgnore: []string{"feature"}},
			event:    "push",
			snapshot: TriggerEventSnapshot{Branch: value("feature")},
			want:     "Doesn’t run on the `feature` branch.",
		},
		{
			name:     "ordered push branch match",
			trigger:  workflow.Trigger{Event: "push", Branches: []string{"release/**", "!release/**-alpha", "release/special-alpha"}},
			event:    "push",
			snapshot: TriggerEventSnapshot{Branch: value("release/special-alpha")},
		},
		{
			name:     "push tag mismatch",
			trigger:  workflow.Trigger{Event: "push", TagsIgnore: []string{"v0"}},
			event:    "push",
			snapshot: TriggerEventSnapshot{Tag: value("v0")},
			want:     `Tag "v0" does not match this workflow's push tag filters.`,
		},
		{
			name:     "branch push with only tag filters",
			trigger:  workflow.Trigger{Event: "push", Tags: []string{"v*"}},
			event:    "push",
			snapshot: TriggerEventSnapshot{Branch: value("main")},
			want:     `Branch push "main" does not match this workflow's push tag filters.`,
		},
		{
			name:     "tag push with only branch filters",
			trigger:  workflow.Trigger{Event: "push", Branches: []string{"main"}},
			event:    "push",
			snapshot: TriggerEventSnapshot{Tag: value("v1")},
			want:     `Tag push "v1" does not match this workflow's push branch filters.`,
		},
		{
			name:     "pull request base mismatch",
			trigger:  workflow.Trigger{Event: "pull_request", Branches: []string{"main"}},
			event:    "pull_request",
			snapshot: TriggerEventSnapshot{PullRequestBaseBranch: value("development"), PullRequestAction: value("opened")},
			want:     `Base branch "development" does not match this workflow's pull_request branch filters.`,
		},
		{
			name:     "pull request activity mismatch",
			trigger:  workflow.Trigger{Event: "pull_request"},
			event:    "pull_request",
			snapshot: TriggerEventSnapshot{PullRequestAction: value("closed")},
			want:     `Pull request activity "closed" does not match this workflow's pull_request activity filters.`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := TriggerFilterMismatchReason([]workflow.Trigger{test.trigger}, test.event, test.snapshot)
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
