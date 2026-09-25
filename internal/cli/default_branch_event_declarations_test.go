package cli

import (
	"encoding/json"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

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
