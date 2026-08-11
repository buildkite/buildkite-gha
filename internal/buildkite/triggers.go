package buildkite

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// TranslateTriggerCondition converts GitHub's trigger selection into a
// deterministic Buildkite conditional. It deliberately does not approximate
// path filtering: Buildkite if_changed has materially different semantics.
func TranslateTriggerCondition(triggers []workflow.Trigger) (string, error) {
	var terms []string
	for _, t := range triggers {
		term, contributes, err := translateTrigger(t)
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

func translateTrigger(t workflow.Trigger) (string, bool, error) {
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
		return `(build.source == "ui" || build.source == "api")`, true, nil
	case "schedule":
		if hasWebhookFilters(t) {
			return "", false, fmt.Errorf("schedule has unsupported webhook filters")
		}
		// Buildkite does not expose the identity of the schedule that created a
		// build. Cron ownership therefore stays in Buildkite, and every scheduled
		// workflow group is eligible on any Buildkite scheduled build.
		return `build.source == "schedule"`, true, nil
	case "push":
		if t.Types != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("push has unsupported filters")
		}
		parts := []string{`build.source_event == "push"`}
		branch, hasBranchFilter, err := refFilters("build.branch", t.Branches, t.BranchesIgnore)
		if err != nil {
			return "", false, fmt.Errorf("push branches: %w", err)
		}
		tag, hasTagFilter, err := refFilters("build.tag", t.Tags, t.TagsIgnore)
		if err != nil {
			return "", false, fmt.Errorf("push tags: %w", err)
		}
		if hasBranchFilter && hasTagFilter {
			parts = append(parts, "((build.tag == null && ("+branch+")) || (build.tag != null && ("+tag+")))")
		} else if hasBranchFilter {
			parts = append(parts, `build.tag == null`, branch)
		} else if hasTagFilter {
			parts = append(parts, `build.tag != null`, tag)
		}
		return strings.Join(parts, " && "), true, nil
	case "pull_request":
		if t.Tags != nil || t.TagsIgnore != nil || t.Workflows != nil {
			return "", false, fmt.Errorf("pull_request tag filters are unsupported")
		}
		parts := []string{`build.source_event == "pull_request"`}
		b, hasBranchFilter, err := refFilters("build.pull_request.base_branch", t.Branches, t.BranchesIgnore)
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
			actions = append(actions, `build.source_action == `+yamlScalar(a))
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
