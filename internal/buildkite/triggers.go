package buildkite

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/workflow"
	"github.com/rhysd/actionlint"
)

const maxSkipReasonLength = 70

// TriggerConditionContext supplies the trusted Buildkite expressions used to
// select one effective event and apply its supported trigger filters.
type TriggerConditionContext struct {
	EventPredicate         string
	Branch                 string
	Tag                    string
	PullRequestBaseBranch  string
	PullRequestAction      string
	ChangedPaths           []string
	ChangedPathsKnown      bool
	ChangedPathsError      string
	BranchValue            *string
	TagValue               *string
	PullRequestBaseValue   *string
	PullRequestActionValue *string
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

// LiveTriggerConditionContext uses fields from the Buildkite build that
// supplied the effective event snapshot.
func LiveTriggerConditionContext(eventPredicate string) TriggerConditionContext {
	return TriggerConditionContext{
		EventPredicate:        eventPredicate,
		Branch:                "build.branch",
		Tag:                   "build.tag",
		PullRequestBaseBranch: "build.pull_request.base_branch",
		PullRequestAction:     "build.source_action",
	}
}

// TranslateTriggerCondition converts GitHub's trigger selection into a
// deterministic Buildkite conditional. It deliberately does not approximate
// path filtering: Buildkite if_changed has materially different semantics.
func TranslateTriggerCondition(triggers []workflow.Trigger) (string, error) {
	var terms []string
	for _, t := range triggers {
		term, contributes, err := translateTrigger(t, liveTriggerContext(t.Event), true)
		if err != nil {
			return "", err
		}
		if contributes {
			terms = append(terms, "("+term+")")
		}
	}
	if len(terms) == 0 {
		return "", fmt.Errorf("workflow has no supported build source trigger")
	}
	return strings.Join(terms, " || "), nil
}

// ValidateTriggerConditions validates every trigger using the same translation
// rules as pipeline generation without selecting an effective event.
func ValidateTriggerConditions(triggers []workflow.Trigger) error {
	var findings []error
	for _, trigger := range triggers {
		if _, _, err := translateTrigger(trigger, liveTriggerContext(trigger.Event), true); err != nil {
			var pathFilters *UnsupportedPathFiltersError
			if errors.As(err, &pathFilters) && pathFilters.Event == "pull_request" && pathFilters.Reason == "" {
				continue
			}
			findings = append(findings, err)
		}
	}
	return errors.Join(findings...)
}

// TranslateEventTriggerCondition validates every trigger but emits a
// condition only for the selected effective event. A false applicable result
// means the workflow must not be compiled for that event.
func TranslateEventTriggerCondition(triggers []workflow.Trigger, event string, context TriggerConditionContext) (condition string, applicable bool, err error) {
	var terms []string
	for _, trigger := range triggers {
		triggerContext := liveTriggerContext(trigger.Event)
		selected := trigger.Event == event
		if selected {
			triggerContext = context
		}
		term, contributes, err := translateTrigger(trigger, triggerContext, selected)
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
func TriggerFilterMismatchReason(triggers []workflow.Trigger, event string, context TriggerConditionContext) (string, error) {
	for _, trigger := range triggers {
		if trigger.Event != event {
			continue
		}
		switch event {
		case "push":
			if context.BranchValue != nil {
				if trigger.Branches == nil && trigger.BranchesIgnore == nil && (trigger.Tags != nil || trigger.TagsIgnore != nil) {
					return fmt.Sprintf("Branch push %q does not match this workflow's push tag filters.", *context.BranchValue), nil
				}
				matches, err := refFilterMatches(*context.BranchValue, trigger.Branches, trigger.BranchesIgnore)
				if err != nil {
					return "", fmt.Errorf("push branches: %w", err)
				}
				if !matches {
					if len(trigger.Branches) > 0 {
						branches := make([]string, len(trigger.Branches))
						for i, branch := range trigger.Branches {
							if strings.HasPrefix(branch, "!") {
								return fmt.Sprintf("Doesn’t run on the `%s` branch.", *context.BranchValue), nil
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
					return fmt.Sprintf("Doesn’t run on the `%s` branch.", *context.BranchValue), nil
				}
			}
			if context.TagValue != nil {
				if trigger.Tags == nil && trigger.TagsIgnore == nil && (trigger.Branches != nil || trigger.BranchesIgnore != nil) {
					return fmt.Sprintf("Tag push %q does not match this workflow's push branch filters.", *context.TagValue), nil
				}
				matches, err := refFilterMatches(*context.TagValue, trigger.Tags, trigger.TagsIgnore)
				if err != nil {
					return "", fmt.Errorf("push tags: %w", err)
				}
				if !matches {
					return fmt.Sprintf("Tag %q does not match this workflow's push tag filters.", *context.TagValue), nil
				}
			}
		case "pull_request":
			if context.PullRequestBaseValue != nil {
				matches, err := refFilterMatches(*context.PullRequestBaseValue, trigger.Branches, trigger.BranchesIgnore)
				if err != nil {
					return "", fmt.Errorf("pull_request branches: %w", err)
				}
				if !matches {
					return fmt.Sprintf("Base branch %q does not match this workflow's pull_request branch filters.", *context.PullRequestBaseValue), nil
				}
			}
			if context.PullRequestActionValue != nil {
				types := trigger.Types
				if types == nil {
					types = []string{"opened", "synchronize", "reopened"}
				}
				matched := false
				for _, action := range types {
					matched = matched || action == *context.PullRequestActionValue
				}
				if !matched {
					return fmt.Sprintf("Pull request activity %q does not match this workflow's pull_request activity filters.", *context.PullRequestActionValue), nil
				}
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

func liveTriggerContext(event string) TriggerConditionContext {
	predicate := ""
	switch event {
	case "workflow_dispatch":
		predicate = `(build.source == "ui" || build.source == "api")`
	case "schedule":
		predicate = `build.source == "schedule"`
	case "push", "pull_request":
		predicate = `build.source_event == ` + yamlScalar(event)
	}
	return LiveTriggerConditionContext(predicate)
}

func translateTrigger(t workflow.Trigger, context TriggerConditionContext, selected bool) (string, bool, error) {
	pathFilters := t.Paths != nil || t.PathsIgnore != nil
	if pathFilters {
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
		if context.EventPredicate == "" {
			return "", false, fmt.Errorf("workflow_dispatch requires an effective event predicate")
		}
		return context.EventPredicate, true, nil
	case "schedule":
		if hasWebhookFilters(t) {
			return "", false, fmt.Errorf("schedule has unsupported webhook filters")
		}
		// Buildkite does not expose the identity of the schedule that created a
		// build. Cron ownership therefore stays in Buildkite, and every scheduled
		// workflow group is eligible on any Buildkite scheduled build.
		if context.EventPredicate == "" {
			return "", false, fmt.Errorf("schedule requires an effective event predicate")
		}
		return context.EventPredicate, true, nil
	case "push":
		if t.Types != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("push has unsupported filters")
		}
		if context.EventPredicate == "" || context.Branch == "" || context.Tag == "" {
			return "", false, fmt.Errorf("push requires effective event, branch, and tag expressions")
		}
		if context.Branch == "null" && context.Tag == "null" {
			return "", false, fmt.Errorf("push event snapshot requires ref to start with refs/heads/ or refs/tags/")
		}
		parts := []string{context.EventPredicate}
		branch, hasBranchFilter, err := refFilters(context.Branch, t.Branches, t.BranchesIgnore)
		if err != nil {
			return "", false, fmt.Errorf("push branches: %w", err)
		}
		tag, hasTagFilter, err := refFilters(context.Tag, t.Tags, t.TagsIgnore)
		if err != nil {
			return "", false, fmt.Errorf("push tags: %w", err)
		}
		if hasBranchFilter && hasTagFilter {
			parts = append(parts, "(("+context.Tag+" == null && ("+branch+")) || ("+context.Tag+" != null && ("+tag+")))")
		} else if hasBranchFilter {
			parts = append(parts, context.Tag+" == null", branch)
		} else if hasTagFilter {
			parts = append(parts, context.Tag+" != null", tag)
		}
		if pathFilters && (!selected || context.TagValue == nil) {
			return "", false, &UnsupportedPathFiltersError{Event: t.Event}
		}
		return strings.Join(parts, " && "), true, nil
	case "pull_request":
		if t.Tags != nil || t.TagsIgnore != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("pull_request tag filters are unsupported")
		}
		if context.EventPredicate == "" || context.PullRequestAction == "" {
			return "", false, fmt.Errorf("pull_request requires effective event and action expressions")
		}
		if context.PullRequestAction == "null" {
			return "", false, fmt.Errorf("pull_request event snapshot requires payload.action")
		}
		parts := []string{context.EventPredicate}
		hasBranchFilter := t.Branches != nil || t.BranchesIgnore != nil
		if hasBranchFilter && context.PullRequestBaseBranch == "" {
			return "", false, fmt.Errorf("pull_request branch filters require a base branch expression")
		}
		if hasBranchFilter && context.PullRequestBaseBranch == "null" {
			return "", false, fmt.Errorf("pull_request branch filters require payload.pull_request.base.ref")
		}
		b, hasBranchFilter, err := refFilters(context.PullRequestBaseBranch, t.Branches, t.BranchesIgnore)
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
			actions = append(actions, context.PullRequestAction+` == `+yamlScalar(a))
		}
		parts = append(parts, "("+strings.Join(actions, " || ")+")")
		if pathFilters && selected {
			if !context.ChangedPathsKnown {
				return "", false, &UnsupportedPathFiltersError{Event: t.Event, Reason: context.ChangedPathsError}
			}
			matches, err := pathFiltersMatch(context.ChangedPaths, t.Paths, t.PathsIgnore)
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
	default:
		return "", false, fmt.Errorf("unsupported GitHub trigger event %q", t.Event)
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
		if positive {
			if state == "" {
				state = match
			} else {
				state = "(" + state + " || " + match + ")"
			}
		} else if state == "" {
			state = "!(" + match + ")"
		} else {
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
				if pathPattern && (i == 1 || runes[i-2] == '/') && i+1 < len(runes) && runes[i+1] == '/' {
					i++
					atoms = append(atoms, atom{value: `((?s:.*)\/)?`})
				} else if pathPattern {
					atoms = append(atoms, atom{value: `(?s:.*)`})
				} else {
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
