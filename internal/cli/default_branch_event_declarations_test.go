package cli

import (
	"encoding/json"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestNativeDefaultBranchEventsIgnoreRefAndPathFilters(t *testing.T) {
	for _, test := range []struct{ event, action string }{
		{"fork", ""}, {"public", ""}, {"gollum", ""}, {"page_build", ""},
		{"watch", "started"}, {"milestone", "created"}, {"branch_protection_rule", "created"}, {"discussion", "edited"},
	} {
		for _, filter := range []string{
			"branches-ignore: ['**']", "paths-ignore: ['**']", "paths: ['never/matching/**']",
			"tags-ignore: ['**']", "tags: [never-this-tag]",
		} {
			t.Run(test.event+"/"+filter, func(t *testing.T) {
				config := filter
				if test.action != "" {
					config = "types: [" + test.action + "], " + config
				}
				parsed, err := workflow.Parse("native.yml", []byte("on: {"+test.event+": {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
				if err != nil {
					t.Fatal(err)
				}
				if err := buildkite.ValidateTriggerConditions(parsed.Triggers); err != nil {
					t.Fatal(err)
				}
				actions := []string{test.action}
				if test.action != "" && test.event != "watch" {
					other := "edited"
					if test.action == other {
						other = "created"
					}
					actions = append(actions, other)
				}
				for _, action := range actions {
					event := compiler.Event{Event: test.event, Ref: "refs/heads/main", Payload: map[string]any{"action": action}}
					expressions, snapshot := snapshotTriggerState(event)
					condition, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, test.event, expressions, snapshot)
					want := "(true)"
					if test.action != "" && test.event != "watch" {
						want = `(true && ("` + action + `" == "` + test.action + `"))`
					}
					if err != nil || !applicable || condition != want {
						t.Fatalf("condition=%q, want=%q, applicable=%v: %v", condition, want, applicable, err)
					}
					reason, err := buildkite.TriggerFilterMismatchReason(parsed.Triggers, test.event, snapshot)
					if err != nil || (reason != "") != (action != test.action) {
						t.Fatalf("action=%s: reason=%q: %v", action, reason, err)
					}
				}
			})
		}
	}
}

func TestNativeDefaultBranchDeclarations(t *testing.T) {
	for _, name := range []string{"fork", "public", "gollum", "page_build", "watch", "milestone", "branch_protection_rule", "discussion", "discussion_comment"} {
		for _, config := range []string{"branches: [never-this-branch]", "types: null", "types: []"} {
			t.Run(name+"/"+config, func(t *testing.T) {
				source, err := generatedEventSnapshot(name)
				if err != nil {
					t.Fatal(err)
				}
				event, err := compiler.ParseEvent(source)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := workflow.Parse("native.yml", []byte("on: {"+name+": {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
				if err != nil {
					t.Fatal(err)
				}
				expressions, snapshot := snapshotTriggerState(event)
				_, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, name, expressions, snapshot)
				if err != nil || !applicable {
					t.Fatalf("applicable=%v: %v", applicable, err)
				}
				if reason, err := buildkite.TriggerFilterMismatchReason(parsed.Triggers, name, snapshot); err != nil || reason != "" {
					t.Fatalf("unexpected mismatch %q: %v", reason, err)
				}
			})
		}
	}
}

func TestNativeDiscussionClosedAndReopened(t *testing.T) {
	for _, action := range []string{"closed", "reopened"} {
		t.Run(action, func(t *testing.T) {
			source, err := generatedEventSnapshot("discussion")
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(source, &raw); err != nil {
				t.Fatal(err)
			}
			raw["payload"].(map[string]any)["action"] = action
			source, err = json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			event, err := compiler.ParseEvent(source)
			if err != nil {
				t.Fatal(err)
			}
			for _, config := range []string{"{}", "{types: [" + action + "]}", "{types: [edited]}"} {
				parsed, err := workflow.Parse("native.yml", []byte("on: {discussion: "+config+"}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
				if err != nil {
					t.Fatal(err)
				}
				expressions, snapshot := snapshotTriggerState(event)
				if _, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, "discussion", expressions, snapshot); err != nil || !applicable {
					t.Fatalf("applicable=%v: %v", applicable, err)
				}
				reason, err := buildkite.TriggerFilterMismatchReason(parsed.Triggers, "discussion", snapshot)
				if err != nil || (reason != "") != (config == "{types: [edited]}") {
					t.Fatalf("config=%s reason=%q: %v", config, reason, err)
				}
			}
		})
	}
}
