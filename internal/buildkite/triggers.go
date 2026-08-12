package buildkite

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const maxSkipReasonLength = 70

// TriggerConditionContext supplies the trusted Buildkite expressions used to
// select one effective event and apply its supported trigger filters.
type TriggerConditionContext struct {
	EventPredicate        string
	Branch                string
	Tag                   string
	PullRequestBaseBranch string
	PullRequestAction     string
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
		term, contributes, err := translateTrigger(t, liveTriggerContext(t.Event))
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

// TranslateEventTriggerCondition validates every trigger but emits a
// condition only for the selected effective event. A false applicable result
// means the workflow must not be compiled for that event.
func TranslateEventTriggerCondition(triggers []workflow.Trigger, event string, context TriggerConditionContext) (condition string, applicable bool, err error) {
	var terms []string
	for _, trigger := range triggers {
		triggerContext := liveTriggerContext(trigger.Event)
		if trigger.Event == event {
			triggerContext = context
		}
		term, contributes, err := translateTrigger(trigger, triggerContext)
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

func translateTrigger(t workflow.Trigger, context TriggerConditionContext) (string, bool, error) {
	if t.Paths != nil || t.PathsIgnore != nil {
		return "", false, fmt.Errorf("%s path filters are unsupported: Buildkite if_changed is not equivalent", t.Event)
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
		return strings.Join(parts, " && "), true, nil
	default:
		return "", false, fmt.Errorf("unsupported GitHub trigger event %q", t.Event)
	}
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

func githubRefGlob(glob string) (string, error) {
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
				atoms = append(atoms, atom{value: ".*"})
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
