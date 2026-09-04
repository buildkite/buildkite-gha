package buildkite

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/workflow"
	"github.com/rhysd/actionlint"
)

const maxSkipReasonLength = 70

// TriggerConditionExpressions supplies the trusted Buildkite expressions used
// to select one effective event and apply its supported trigger filters.
type TriggerConditionExpressions struct {
	EventPredicate        string
	Branch                string
	Tag                   string
	PullRequestBaseBranch string
	PullRequestAction     string
	MergeGroupBaseBranch  string
	MergeGroupAction      string
	ReleaseAction         string
	IssuesAction          string
}

// ChangedPathEvaluation records either the available changed paths or why
// they could not be acquired. A non-nil Paths slice means paths are available.
type ChangedPathEvaluation struct {
	Paths             []string
	UnavailableReason string
}

func (e ChangedPathEvaluation) available() bool {
	return e.Paths != nil
}

// TriggerEventSnapshot supplies observed effective-event values used to match
// trigger filters and explain mismatches.
type TriggerEventSnapshot struct {
	Branch                *string
	Tag                   *string
	PullRequestBaseBranch *string
	PullRequestAction     *string
	MergeGroupBaseBranch  *string
	MergeGroupAction      *string
	ReleaseAction         *string
	IssuesAction          *string
	ChangedPaths          ChangedPathEvaluation
}

// supportedTriggerEvents is the single source of truth for GitHub trigger
// events that map to a Buildkite build source.
var supportedTriggerEvents = map[string]bool{
	"workflow_call":     true,
	"workflow_dispatch": true,
	"schedule":          true,
	"push":              true,
	"pull_request":      true,
	"merge_group":       true,
	"release":           true,
	"issues":            true,
}

var supportedIssuesAction = map[string]bool{
	"opened": true, "edited": true, "deleted": true, "transferred": true,
	"field_added": true, "field_removed": true,
	"pinned": true, "unpinned": true, "closed": true, "reopened": true,
	"assigned": true, "unassigned": true, "labeled": true, "unlabeled": true,
	"locked": true, "unlocked": true, "milestoned": true, "demilestoned": true,
	"typed": true, "untyped": true,
}

// SupportedTriggerEvent reports whether the GitHub trigger event maps to a
// Buildkite build source.
func SupportedTriggerEvent(event string) bool {
	return supportedTriggerEvents[event]
}

// UnsupportedTriggerEventError reports a trigger event with no Buildkite
// build source. Such a trigger can never start a build, so it is ignored
// when the workflow also declares a supported trigger.
type UnsupportedTriggerEventError struct {
	Event string
}

func (e *UnsupportedTriggerEventError) Error() string {
	return fmt.Sprintf("unsupported GitHub trigger event %q", e.Event)
}

func (e *UnsupportedTriggerEventError) CompatibilityBlocker() (string, string) {
	return "trigger", e.Event
}

func unsupportedTriggerEvent(err error) bool {
	var unsupported *UnsupportedTriggerEventError
	return errors.As(err, &unsupported)
}

// UnsupportedPathFiltersError reports a trigger that cannot be translated
// without changing its path-filter semantics.
type UnsupportedPathFiltersError struct {
	Event  string
	Reason string
}

func (e *UnsupportedPathFiltersError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s path filters are unsupported: %s", e.Event, e.Reason)
	}
	return fmt.Sprintf("%s path filters are unsupported: Buildkite if_changed is not equivalent", e.Event)
}

func (e *UnsupportedPathFiltersError) CompatibilityBlocker() (string, string) {
	return "trigger", e.Event
}

// LiveTriggerConditionExpressions uses fields from the Buildkite build that
// supplied the effective event snapshot.
func LiveTriggerConditionExpressions(eventPredicate string) TriggerConditionExpressions {
	return TriggerConditionExpressions{
		EventPredicate:        eventPredicate,
		Branch:                "build.branch",
		Tag:                   "build.tag",
		PullRequestBaseBranch: "build.pull_request.base_branch",
		PullRequestAction:     "build.source_action",
		MergeGroupBaseBranch:  "build.merge_queue.base_branch",
		MergeGroupAction:      "build.source_action",
		ReleaseAction:         "build.source_action",
		IssuesAction:          "build.source_action",
	}
}

// TranslateTriggerCondition converts GitHub's trigger selection into a
// deterministic Buildkite conditional. It deliberately does not approximate
// path filtering: Buildkite if_changed has materially different semantics.
// Unsupported trigger events contribute nothing unless no trigger has a
// Buildkite build source at all.
func TranslateTriggerCondition(triggers []workflow.Trigger) (string, error) {
	var terms []string
	var unsupported []error
	for _, t := range triggers {
		term, contributes, err := translateTrigger(t, liveTriggerExpressions(t.Event), TriggerEventSnapshot{}, true)
		if unsupportedTriggerEvent(err) {
			unsupported = append(unsupported, err)
			continue
		}
		if err != nil {
			return "", err
		}
		if contributes {
			terms = append(terms, "("+term+")")
		}
	}
	if len(terms) == 0 {
		if len(unsupported) > 0 {
			return "", errors.Join(unsupported...)
		}
		return "", fmt.Errorf("workflow has no supported build source trigger")
	}
	return strings.Join(terms, " || "), nil
}

// ValidateTriggerConditions validates every trigger using the same translation
// rules as pipeline generation without selecting an effective event.
// Unsupported trigger events are reported only when the workflow declares no
// supported trigger event at all; otherwise they are ignored because they can
// never start a Buildkite build.
func ValidateTriggerConditions(triggers []workflow.Trigger) error {
	var findings []error
	var unsupported []error
	supportedEvent := false
	for _, trigger := range triggers {
		_, _, err := translateTrigger(trigger, liveTriggerExpressions(trigger.Event), TriggerEventSnapshot{}, true)
		if unsupportedTriggerEvent(err) {
			unsupported = append(unsupported, err)
			continue
		}
		supportedEvent = true
		if err == nil {
			continue
		}
		var pathFilters *UnsupportedPathFiltersError
		if errors.As(err, &pathFilters) && (pathFilters.Event == "push" || pathFilters.Event == "pull_request") && pathFilters.Reason == "" {
			continue
		}
		findings = append(findings, err)
	}
	if !supportedEvent {
		findings = append(findings, unsupported...)
	}
	return errors.Join(findings...)
}

// TranslateEventTriggerCondition validates every trigger but emits a
// condition only for the selected effective event. A false applicable result
// means the workflow must not be compiled for that event.
func TranslateEventTriggerCondition(triggers []workflow.Trigger, event string, expressions TriggerConditionExpressions, snapshot TriggerEventSnapshot) (condition string, applicable bool, err error) {
	var terms []string
	for _, trigger := range triggers {
		triggerExpressions := liveTriggerExpressions(trigger.Event)
		triggerSnapshot := TriggerEventSnapshot{}
		selected := trigger.Event == event
		if selected {
			triggerExpressions = expressions
			triggerSnapshot = snapshot
		}
		term, contributes, err := translateTrigger(trigger, triggerExpressions, triggerSnapshot, selected)
		if !selected && unsupportedTriggerEvent(err) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if trigger.Event == event && contributes {
			terms = append(terms, "("+term+")")
		}
	}
	if len(terms) == 0 {
		return "", false, nil
	}
	return strings.Join(terms, " || "), true, nil
}

// TriggerEventSkipReason describes why the effective event has no matching on
// entry. Filter mismatches retain their generated Buildkite condition.
func TriggerEventSkipReason(triggers []workflow.Trigger, event string) string {
	for _, trigger := range triggers {
		if trigger.Event == event {
			return ""
		}
	}
	return skipReason(
		fmt.Sprintf("This workflow is not triggered by a `%s` event", event),
		"This workflow is not triggered by the current event",
	)
}

// TriggerFilterMismatchReason describes why a concrete effective event does
// not satisfy the filters on its matching workflow trigger.
func TriggerFilterMismatchReason(triggers []workflow.Trigger, event string, snapshot TriggerEventSnapshot) (string, error) {
	for _, trigger := range triggers {
		if trigger.Event != event {
			continue
		}
		switch event {
		case "push":
			if snapshot.Branch != nil {
				if trigger.Branches == nil && trigger.BranchesIgnore == nil && (trigger.Tags != nil || trigger.TagsIgnore != nil) {
					return fmt.Sprintf("Branch push %q does not match this workflow's push tag filters.", *snapshot.Branch), nil
				}
				matches, err := refFilterMatches(*snapshot.Branch, trigger.Branches, trigger.BranchesIgnore)
				if err != nil {
					return "", fmt.Errorf("push branches: %w", err)
				}
				if !matches {
					if len(trigger.Branches) > 0 {
						branches := make([]string, len(trigger.Branches))
						for i, branch := range trigger.Branches {
							if strings.HasPrefix(branch, "!") {
								return fmt.Sprintf("Doesn’t run on the `%s` branch.", *snapshot.Branch), nil
							}
							branches[i] = fmt.Sprintf("`%s`", branch)
						}
						if len(branches) == 1 {
							return fmt.Sprintf("Only runs on %s.", branches[0]), nil
						}
						separator := " or "
						if len(branches) > 2 {
							separator = ", or "
						}
						return fmt.Sprintf("Only runs on %s%s%s.", strings.Join(branches[:len(branches)-1], ", "), separator, branches[len(branches)-1]), nil
					}
					return fmt.Sprintf("Doesn’t run on the `%s` branch.", *snapshot.Branch), nil
				}
			}
			if snapshot.Tag != nil {
				if trigger.Tags == nil && trigger.TagsIgnore == nil && (trigger.Branches != nil || trigger.BranchesIgnore != nil) {
					return fmt.Sprintf("Tag push %q does not match this workflow's push branch filters.", *snapshot.Tag), nil
				}
				matches, err := refFilterMatches(*snapshot.Tag, trigger.Tags, trigger.TagsIgnore)
				if err != nil {
					return "", fmt.Errorf("push tags: %w", err)
				}
				if !matches {
					return fmt.Sprintf("Tag %q does not match this workflow's push tag filters.", *snapshot.Tag), nil
				}
			}
		case "pull_request":
			if snapshot.PullRequestBaseBranch != nil {
				matches, err := refFilterMatches(*snapshot.PullRequestBaseBranch, trigger.Branches, trigger.BranchesIgnore)
				if err != nil {
					return "", fmt.Errorf("pull_request branches: %w", err)
				}
				if !matches {
					return fmt.Sprintf("Base branch %q does not match this workflow's pull_request branch filters.", *snapshot.PullRequestBaseBranch), nil
				}
			}
			if snapshot.PullRequestAction != nil {
				types := trigger.Types
				if types == nil {
					types = []string{"opened", "synchronize", "reopened"}
				}
				matched := false
				for _, action := range types {
					matched = matched || action == *snapshot.PullRequestAction
				}
				if !matched {
					return fmt.Sprintf("Pull request activity %q does not match this workflow's pull_request activity filters.", *snapshot.PullRequestAction), nil
				}
			}
		case "merge_group":
			if snapshot.MergeGroupBaseBranch != nil {
				matches, err := refFilterMatches(*snapshot.MergeGroupBaseBranch, trigger.Branches, trigger.BranchesIgnore)
				if err != nil {
					return "", fmt.Errorf("merge_group branches: %w", err)
				}
				if !matches {
					return fmt.Sprintf("Base branch %q does not match this workflow's merge_group branch filters.", *snapshot.MergeGroupBaseBranch), nil
				}
			}
			if snapshot.MergeGroupAction != nil && *snapshot.MergeGroupAction != "checks_requested" {
				return fmt.Sprintf("Merge group activity %q does not match this workflow's merge_group activity filters.", *snapshot.MergeGroupAction), nil
			}
		case "release":
			if snapshot.ReleaseAction != nil {
				if slices.Contains(trigger.Types, *snapshot.ReleaseAction) {
					return "", nil
				}
				return fmt.Sprintf("Release activity %q does not match this workflow's release activity filters.", *snapshot.ReleaseAction), nil
			}
		case "issues":
			if snapshot.IssuesAction != nil && trigger.Types != nil && !slices.Contains(trigger.Types, *snapshot.IssuesAction) {
				return fmt.Sprintf("Issue activity %q does not match this workflow's issues activity filters.", *snapshot.IssuesAction), nil
			}
		}
		return "", nil
	}
	return "", nil
}

func skipReason(reason, fallback string) string {
	if utf8.RuneCountInString(reason) <= maxSkipReasonLength {
		return reason
	}
	return fallback
}

func liveTriggerExpressions(event string) TriggerConditionExpressions {
	return LiveTriggerConditionExpressions(LiveEventPredicate(event))
}

// LiveEventPredicate matches the original GitHub event when available and
// preserves Buildkite's compatibility mapping for non-webhook builds.
func LiveEventPredicate(event string) string {
	githubEvent := "build.env(" + yamlScalar("GITHUB_EVENT_NAME") + ")"
	buildkiteGitHubEvent := "build.env(" + yamlScalar("BUILDKITE_GITHUB_EVENT") + ")"
	githubEventMissing := "(" + githubEvent + " == null || " + githubEvent + " == " + yamlScalar("") + ")"
	predicate := "(" + githubEvent + " == " + yamlScalar(event) + " || (" + githubEventMissing + " && " + buildkiteGitHubEvent + " == " + yamlScalar(event) + "))"
	fallbackEvent := "(" + githubEventMissing + " && (" + buildkiteGitHubEvent + " == null"
	unsupportedEvent := ""
	for _, supported := range []string{"push", "pull_request", "workflow_dispatch", "schedule"} {
		if unsupportedEvent != "" {
			unsupportedEvent += " && "
		}
		unsupportedEvent += buildkiteGitHubEvent + " != " + yamlScalar(supported)
	}
	fallbackEvent += " || (" + unsupportedEvent + ")))"
	switch event {
	case "push":
		return "(" + predicate + " || (" + fallbackEvent + ` && build.pull_request.id == null && build.source != "schedule"))`
	case "pull_request":
		return "(" + predicate + " || (" + fallbackEvent + " && build.pull_request.id != null))"
	case "workflow_dispatch":
		return predicate
	case "schedule":
		return "(" + predicate + " || (" + fallbackEvent + ` && build.pull_request.id == null && build.source == "schedule"))`
	case "merge_group", "release", "issues":
		return predicate
	default:
		return ""
	}
}

func translateTrigger(t workflow.Trigger, expressions TriggerConditionExpressions, snapshot TriggerEventSnapshot, selected bool) (string, bool, error) {
	if !SupportedTriggerEvent(t.Event) {
		return "", false, &UnsupportedTriggerEventError{Event: t.Event}
	}
	pathFilters := t.Paths != nil || t.PathsIgnore != nil
	if pathFilters && t.Event != "merge_group" {
		if t.Event != "push" && t.Event != "pull_request" {
			return "", false, &UnsupportedPathFiltersError{Event: t.Event}
		}
		if _, err := pathFiltersMatch(nil, t.Paths, t.PathsIgnore); err != nil {
			return "", false, fmt.Errorf("%s paths: %w", t.Event, err)
		}
	}
	switch t.Event {
	case "workflow_call":
		return "", false, nil
	case "workflow_dispatch":
		if hasWebhookFilters(t) {
			return "", false, fmt.Errorf("workflow_dispatch has unsupported webhook filters")
		}
		if expressions.EventPredicate == "" {
			return "", false, fmt.Errorf("workflow_dispatch requires an effective event predicate")
		}
		return expressions.EventPredicate, true, nil
	case "schedule":
		if hasWebhookFilters(t) {
			return "", false, fmt.Errorf("schedule has unsupported webhook filters")
		}
		// Buildkite does not expose the identity of the schedule that created a
		// build. Cron ownership therefore stays in Buildkite, and every scheduled
		// workflow group is eligible on any Buildkite scheduled build.
		if expressions.EventPredicate == "" {
			return "", false, fmt.Errorf("schedule requires an effective event predicate")
		}
		return expressions.EventPredicate, true, nil
	case "push":
		if t.Types != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("push has unsupported filters")
		}
		if expressions.EventPredicate == "" || expressions.Branch == "" || expressions.Tag == "" {
			return "", false, fmt.Errorf("push requires effective event, branch, and tag expressions")
		}
		if expressions.Branch == "null" && expressions.Tag == "null" {
			return "", false, fmt.Errorf("push event snapshot requires ref to start with refs/heads/ or refs/tags/")
		}
		parts := []string{expressions.EventPredicate}
		branch, hasBranchFilter, err := refFilters(expressions.Branch, t.Branches, t.BranchesIgnore)
		if err != nil {
			return "", false, fmt.Errorf("push branches: %w", err)
		}
		tag, hasTagFilter, err := refFilters(expressions.Tag, t.Tags, t.TagsIgnore)
		if err != nil {
			return "", false, fmt.Errorf("push tags: %w", err)
		}
		switch {
		case hasBranchFilter && hasTagFilter:
			parts = append(parts, "(("+expressions.Tag+" == null && ("+branch+")) || ("+expressions.Tag+" != null && ("+tag+")))")
		case hasBranchFilter:
			parts = append(parts, expressions.Tag+" == null", branch)
		case hasTagFilter:
			parts = append(parts, expressions.Tag+" != null", tag)
		}
		if pathFilters && selected && snapshot.Tag == nil {
			if snapshot.Branch != nil {
				branchMatches := true
				if !hasBranchFilter && hasTagFilter {
					branchMatches = false
				} else if hasBranchFilter {
					branchMatches, err = refFilterMatches(*snapshot.Branch, t.Branches, t.BranchesIgnore)
					if err != nil {
						return "", false, fmt.Errorf("push branches: %w", err)
					}
				}
				if !branchMatches {
					return strings.Join(parts, " && "), true, nil
				}
			}
			if !snapshot.ChangedPaths.available() {
				return "", false, &UnsupportedPathFiltersError{Event: t.Event, Reason: snapshot.ChangedPaths.UnavailableReason}
			}
			matches, err := pathFiltersMatch(snapshot.ChangedPaths.Paths, t.Paths, t.PathsIgnore)
			if err != nil {
				return "", false, fmt.Errorf("push paths: %w", err)
			}
			if !matches {
				return "", false, &UnsupportedPathFiltersError{
					Event:  t.Event,
					Reason: "local changed paths do not match, and GitHub's diff-timeout outcome is unavailable",
				}
			}
		}
		return strings.Join(parts, " && "), true, nil
	case "pull_request":
		if t.Tags != nil || t.TagsIgnore != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("pull_request tag filters are unsupported")
		}
		if expressions.EventPredicate == "" || expressions.PullRequestAction == "" {
			return "", false, fmt.Errorf("pull_request requires effective event and action expressions")
		}
		if expressions.PullRequestAction == "null" {
			return "", false, fmt.Errorf("pull_request event snapshot requires payload.action")
		}
		parts := []string{expressions.EventPredicate}
		hasBranchFilter := t.Branches != nil || t.BranchesIgnore != nil
		if hasBranchFilter && expressions.PullRequestBaseBranch == "" {
			return "", false, fmt.Errorf("pull_request branch filters require a base branch expression")
		}
		if hasBranchFilter && expressions.PullRequestBaseBranch == "null" {
			return "", false, fmt.Errorf("pull_request branch filters require payload.pull_request.base.ref")
		}
		b, hasBranchFilter, err := refFilters(expressions.PullRequestBaseBranch, t.Branches, t.BranchesIgnore)
		if err != nil {
			return "", false, fmt.Errorf("pull_request branches: %w", err)
		}
		if hasBranchFilter {
			parts = append(parts, b)
		}
		types := t.Types
		if types == nil {
			types = []string{"opened", "synchronize", "reopened"}
		}
		if len(types) == 0 {
			return "", false, fmt.Errorf("pull_request types is explicitly empty")
		}
		var actions []string
		for _, a := range types {
			if !supportedPullRequestAction[a] {
				return "", false, fmt.Errorf("pull_request activity type %q cannot be mapped exactly", a)
			}
			actions = append(actions, expressions.PullRequestAction+` == `+yamlScalar(a))
		}
		parts = append(parts, "("+strings.Join(actions, " || ")+")")
		if pathFilters && selected {
			if !snapshot.ChangedPaths.available() {
				return "", false, &UnsupportedPathFiltersError{Event: t.Event, Reason: snapshot.ChangedPaths.UnavailableReason}
			}
			matches, err := pathFiltersMatch(snapshot.ChangedPaths.Paths, t.Paths, t.PathsIgnore)
			if err != nil {
				return "", false, fmt.Errorf("pull_request paths: %w", err)
			}
			if !matches {
				return "", false, &UnsupportedPathFiltersError{
					Event:  t.Event,
					Reason: "local changed paths do not match, and GitHub's diff-timeout outcome is unavailable",
				}
			}
		}
		return strings.Join(parts, " && "), true, nil
	case "merge_group":
		if t.Tags != nil || t.TagsIgnore != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("merge_group has unsupported filters")
		}
		if expressions.EventPredicate == "" || expressions.MergeGroupAction == "" {
			return "", false, fmt.Errorf("merge_group requires effective event and action expressions")
		}
		if expressions.MergeGroupAction == "null" {
			return "", false, fmt.Errorf("merge_group event snapshot requires payload.action")
		}
		if snapshot.MergeGroupAction != nil && *snapshot.MergeGroupAction != "checks_requested" {
			return "", false, fmt.Errorf("merge_group activity must be checks_requested")
		}
		parts := []string{expressions.EventPredicate, expressions.MergeGroupAction + ` == "checks_requested"`}
		hasBranchFilter := t.Branches != nil || t.BranchesIgnore != nil
		if hasBranchFilter && (expressions.MergeGroupBaseBranch == "" || expressions.MergeGroupBaseBranch == "null") {
			return "", false, fmt.Errorf("merge_group branch filters require payload.merge_group.base_ref")
		}
		branch, hasBranchFilter, err := refFilters(expressions.MergeGroupBaseBranch, t.Branches, t.BranchesIgnore)
		if err != nil {
			return "", false, fmt.Errorf("merge_group branches: %w", err)
		}
		if hasBranchFilter {
			parts = append(parts, branch)
		}
		if t.Types != nil {
			if len(t.Types) == 0 {
				return "", false, fmt.Errorf("merge_group types is explicitly empty")
			}
			for _, activity := range t.Types {
				if activity != "checks_requested" {
					return "", false, fmt.Errorf("merge_group type %q is unsupported. checks_requested is the only merge queue activity currently mapped. Set types: [checks_requested]. If you need another merge_group type, open an issue in https://github.com/buildkite/buildkite-gha so we can prioritize it", activity)
				}
			}
		}
		return strings.Join(parts, " && "), true, nil
	case "release":
		if t.Branches != nil || t.BranchesIgnore != nil || t.Tags != nil || t.TagsIgnore != nil || t.Paths != nil || t.PathsIgnore != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("release has unsupported filters")
		}
		if expressions.EventPredicate == "" || expressions.ReleaseAction == "" {
			return "", false, fmt.Errorf("release requires effective event and action expressions")
		}
		if expressions.ReleaseAction == "null" {
			return "", false, fmt.Errorf("release event snapshot requires payload.action")
		}
		if t.Types == nil {
			return "", false, fmt.Errorf("on: release needs a types list. A bare release covers every release event, while the currently supported types are exactly published, created, and released. Use on: {release: {types: [published]}}. If you need another release type, open an issue in https://github.com/buildkite/buildkite-gha so we can prioritize it")
		}
		if len(t.Types) == 0 {
			return "", false, fmt.Errorf("release types is explicitly empty")
		}
		actions := make([]string, 0, len(t.Types))
		for _, action := range t.Types {
			if action != "published" && action != "created" && action != "released" {
				return "", false, fmt.Errorf("release activity type %q cannot be mapped exactly", action)
			}
			actions = append(actions, expressions.ReleaseAction+` == `+yamlScalar(action))
		}
		return expressions.EventPredicate + " && (" + strings.Join(actions, " || ") + ")", true, nil
	case "issues":
		if t.Branches != nil || t.BranchesIgnore != nil || t.Tags != nil || t.TagsIgnore != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("issues has unsupported filters")
		}
		if expressions.EventPredicate == "" || expressions.IssuesAction == "" {
			return "", false, fmt.Errorf("issues requires effective event and action expressions")
		}
		if expressions.IssuesAction == "null" {
			return "", false, fmt.Errorf("issues event snapshot requires payload.action")
		}
		if t.Types == nil {
			return expressions.EventPredicate, true, nil
		}
		if len(t.Types) == 0 {
			return "", false, fmt.Errorf("issues types is explicitly empty")
		}
		actions := make([]string, 0, len(t.Types))
		for _, action := range t.Types {
			if !supportedIssuesAction[action] {
				return "", false, fmt.Errorf("issues activity type %q cannot be mapped exactly", action)
			}
			actions = append(actions, expressions.IssuesAction+` == `+yamlScalar(action))
		}
		return expressions.EventPredicate + " && (" + strings.Join(actions, " || ") + ")", true, nil
	default:
		return "", false, &UnsupportedTriggerEventError{Event: t.Event}
	}
}

func pathFiltersMatch(paths, include, exclude []string) (bool, error) {
	if include != nil && exclude != nil {
		return false, fmt.Errorf("include and ignore filters cannot be combined")
	}
	patterns := include
	if exclude != nil {
		patterns = exclude
	}
	compiled := make([]struct {
		pattern  *regexp.Regexp
		positive bool
	}, 0, len(patterns))
	positiveSeen := false
	for _, pattern := range patterns {
		positive := include != nil
		negated := strings.HasPrefix(pattern, "!")
		if negated {
			if exclude != nil {
				return false, fmt.Errorf("ignore filter pattern %q cannot be negated", pattern)
			}
			positive = false
			pattern = strings.TrimPrefix(pattern, "!")
		}
		if pattern == "" {
			return false, fmt.Errorf("empty path glob")
		}
		if negated && !positiveSeen {
			return false, fmt.Errorf("negative pattern %q must follow a positive pattern", "!"+pattern)
		}
		if strings.Contains(pattern, `\`) {
			return false, fmt.Errorf("path glob %q contains an unsupported backslash", pattern)
		}
		if invalid := actionlint.ValidatePathGlob(pattern); len(invalid) != 0 {
			return false, fmt.Errorf("invalid path glob %q: %s", pattern, invalid[0].Message)
		}
		expression, err := githubPathGlob(pattern)
		if err != nil {
			return false, err
		}
		matcher, err := regexp.Compile(expression)
		if err != nil {
			return false, fmt.Errorf("compile path glob %q: %w", pattern, err)
		}
		compiled = append(compiled, struct {
			pattern  *regexp.Regexp
			positive bool
		}{matcher, positive})
		positiveSeen = positiveSeen || positive
	}
	for _, path := range paths {
		matched := exclude != nil
		for _, pattern := range compiled {
			if pattern.pattern.MatchString(path) {
				matched = pattern.positive
			}
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

var supportedPullRequestAction = map[string]bool{
	"assigned": true, "unassigned": true, "labeled": true, "unlabeled": true,
	"opened": true, "edited": true, "closed": true, "reopened": true,
	"synchronize": true, "converted_to_draft": true, "locked": true, "unlocked": true,
	"enqueued": true, "dequeued": true, "milestoned": true, "demilestoned": true,
	"ready_for_review": true, "review_requested": true, "review_request_removed": true,
	"auto_merge_enabled": true, "auto_merge_disabled": true,
}

func hasWebhookFilters(t workflow.Trigger) bool {
	return t.Types != nil || t.Branches != nil || t.BranchesIgnore != nil || t.Tags != nil || t.TagsIgnore != nil || t.Workflows != nil
}

func refFilters(field string, include, exclude []string) (string, bool, error) {
	if include != nil && exclude != nil {
		return "", false, fmt.Errorf("include and ignore filters cannot be combined")
	}
	configured := include != nil || exclude != nil
	if !configured {
		return "", false, nil
	}
	state := ""
	apply := func(pattern string, positive bool) error {
		r, err := githubRefGlob(pattern)
		if err != nil {
			return err
		}
		match := field + " =~ /" + r + "/"
		switch {
		case positive && state == "":
			state = match
		case positive:
			state = "(" + state + " || " + match + ")"
		case state == "":
			state = "!(" + match + ")"
		default:
			state = "(" + state + " && !(" + match + "))"
		}
		return nil
	}
	positiveSeen := false
	for _, p := range include {
		positive := true
		if strings.HasPrefix(p, "!") {
			positive = false
			p = p[1:]
		}
		if p == "" {
			return "", false, fmt.Errorf("empty ref glob")
		}
		if !positive && !positiveSeen {
			return "", false, fmt.Errorf("negative pattern %q must follow a positive pattern", "!"+p)
		}
		if err := apply(p, positive); err != nil {
			return "", false, err
		}
		if positive {
			positiveSeen = true
		}
	}
	if include != nil && len(include) == 0 {
		return "false", true, nil
	}
	if exclude != nil {
		state = "true"
	}
	for _, p := range exclude {
		if strings.HasPrefix(p, "!") {
			return "", false, fmt.Errorf("ignore filter pattern %q cannot be negated", p)
		}
		if err := apply(p, false); err != nil {
			return "", false, err
		}
	}
	return state, true, nil
}

func refFilterMatches(value string, include, exclude []string) (bool, error) {
	if _, configured, err := refFilters("ref", include, exclude); err != nil || !configured {
		return !configured, err
	}
	matched := include == nil
	patterns := include
	if exclude != nil {
		patterns = exclude
	}
	for _, pattern := range patterns {
		positive := include != nil
		if strings.HasPrefix(pattern, "!") {
			positive = false
			pattern = strings.TrimPrefix(pattern, "!")
		}
		expression, err := githubRefGlob(pattern)
		if err != nil {
			return false, err
		}
		compiled, err := regexp.Compile(expression)
		if err != nil {
			return false, fmt.Errorf("compile ref glob %q: %w", pattern, err)
		}
		if compiled.MatchString(value) {
			matched = positive
		}
	}
	return matched, nil
}

func githubRefGlob(glob string) (string, error) {
	return githubGlob(glob, false)
}

func githubPathGlob(glob string) (string, error) {
	return githubGlob(glob, true)
}

func githubGlob(glob string, pathPattern bool) (string, error) {
	regexLiteral := func(value string) string {
		return strings.ReplaceAll(regexp.QuoteMeta(value), "/", `\/`)
	}
	type atom struct {
		value      string
		modifiable bool
	}
	runes := []rune(glob)
	var atoms []atom
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '\\':
			if i+1 >= len(runes) {
				return "", fmt.Errorf("trailing escape in glob %q", glob)
			}
			i++
			atoms = append(atoms, atom{value: regexLiteral(string(runes[i])), modifiable: true})
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				i++
				switch {
				case pathPattern && (i == 1 || runes[i-2] == '/') && i+1 < len(runes) && runes[i+1] == '/':
					i++
					atoms = append(atoms, atom{value: `((?s:.*)\/)?`})
				case pathPattern:
					atoms = append(atoms, atom{value: `(?s:.*)`})
				default:
					atoms = append(atoms, atom{value: ".*"})
				}
			} else {
				atoms = append(atoms, atom{value: `[^\/]*`})
			}
		case '[':
			j := i + 1
			for j < len(runes) && runes[j] != ']' {
				j++
			}
			if j == len(runes) {
				return "", fmt.Errorf("unterminated character class in glob %q", glob)
			}
			if pathPattern && !validGitHubPathCharacterClass(runes[i+1:j]) {
				return "", fmt.Errorf("invalid character class in path glob %q", glob)
			}
			class := string(runes[i : j+1])
			if _, err := regexp.Compile(class); err != nil {
				return "", fmt.Errorf("invalid character class in glob %q", glob)
			}
			atoms = append(atoms, atom{value: strings.ReplaceAll(class, "/", `\/`), modifiable: true})
			i = j
		case '?', '+':
			if len(atoms) == 0 || !atoms[len(atoms)-1].modifiable {
				return "", fmt.Errorf("glob %q has invalid %q modifier", glob, c)
			}
			atoms[len(atoms)-1].value += string(c)
			atoms[len(atoms)-1].modifiable = false
		default:
			atoms = append(atoms, atom{value: regexLiteral(string(c)), modifiable: true})
		}
	}
	var regex strings.Builder
	regex.WriteByte('^')
	for _, atom := range atoms {
		regex.WriteString(atom.value)
	}
	regex.WriteByte('$')
	return regex.String(), nil
}

func validGitHubPathCharacterClass(class []rune) bool {
	isASCIIAlphanumeric := func(value rune) bool {
		return value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
	}
	sameRange := func(left, right rune) bool {
		return left >= '0' && left <= right && right <= '9' ||
			left >= 'A' && left <= right && right <= 'Z' ||
			left >= 'a' && left <= right && right <= 'z'
	}
	if len(class) == 0 {
		return false
	}
	for i, value := range class {
		if isASCIIAlphanumeric(value) {
			continue
		}
		if value != '-' || i == 0 || i == len(class)-1 || !sameRange(class[i-1], class[i+1]) {
			return false
		}
	}
	return true
}
