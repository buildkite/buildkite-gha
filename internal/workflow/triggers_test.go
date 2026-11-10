package workflow

import "testing"

func TestParseRetainsShorthandTriggerForms(t *testing.T) {
	for _, test := range []struct {
		name   string
		on     string
		events []string
	}{
		{name: "scalar", on: "push", events: []string{"push"}},
		{name: "list", on: "[push, pull_request, workflow_dispatch]", events: []string{"push", "pull_request", "workflow_dispatch"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := Parse("workflow.yml", []byte("on: "+test.on+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(workflow.Triggers) != len(test.events) {
				t.Fatalf("triggers = %#v", workflow.Triggers)
			}
			for i, event := range test.events {
				if workflow.Triggers[i].Event != event {
					t.Fatalf("trigger %d = %#v, want %q", i, workflow.Triggers[i], event)
				}
			}
			if workflow.Triggers[0].Types != nil || workflow.Triggers[0].Branches != nil {
				t.Fatalf("omitted shorthand filters were not retained as nil: %#v", workflow.Triggers[0])
			}
		})
	}
}

func TestParseRetainsTriggerConfiguration(t *testing.T) {
	source := []byte("on:\n  push:\n    branches: [main, '!release/**', 'release/**']\n  pull_request:\n    types: [opened]\n  merge_group:\n    types: [checks_requested]\n    branches: [main]\n  workflow_dispatch:\n    inputs:\n      target:\n        type: string\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	w, err := Parse("workflow.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Triggers) != 5 {
		t.Fatalf("got %d triggers: %#v", len(w.Triggers), w.Triggers)
	}
	if got := w.Triggers[0].Branches; len(got) != 3 || got[1] != "!release/**" || got[2] != "release/**" {
		t.Fatalf("branch order lost: %#v", got)
	}
	if w.Triggers[1].Types == nil || len(w.Triggers[1].Types) != 1 {
		t.Fatalf("types not retained: %#v", w.Triggers[1])
	}
	if len(w.Triggers[2].Types) != 1 || w.Triggers[2].Types[0] != "checks_requested" || len(w.Triggers[2].Branches) != 1 || w.Triggers[2].Branches[0] != "main" {
		t.Fatalf("merge_group filters not retained: %#v", w.Triggers[2])
	}
	if w.Triggers[3].Dispatch == nil || len(w.Triggers[3].Dispatch.Inputs) != 1 || w.Triggers[3].Dispatch.Inputs[0].Name != "target" {
		t.Fatalf("dispatch not retained: %#v", w.Triggers[3])
	}
	if len(w.Triggers[4].Schedules) != 1 || w.Triggers[4].Schedules[0].Cron != "0 0 * * *" {
		t.Fatalf("schedule not retained: %#v", w.Triggers[4])
	}
}

func TestParseRetainsIssuesActivityTypes(t *testing.T) {
	w, err := Parse("workflow.yml", []byte("on:\n  issues:\n    types: [opened, typed, untyped]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Triggers) != 1 || w.Triggers[0].Event != "issues" || len(w.Triggers[0].Types) != 3 || w.Triggers[0].Types[1] != "typed" || w.Triggers[0].Types[2] != "untyped" {
		t.Fatalf("issues trigger not retained: %#v", w.Triggers)
	}
}

func TestParseRetainsImageVersionTriggerPosition(t *testing.T) {
	source := []byte("on:\n  push:\n  image_version:\n    names: [ubuntu]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
	w, err := Parse("workflow.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Triggers) != 2 {
		t.Fatalf("got %d triggers: %#v", len(w.Triggers), w.Triggers)
	}
	trigger := w.Triggers[1]
	if trigger.Event != "image_version" || trigger.Position == (Position{}) {
		t.Fatalf("image_version trigger position lost: %#v", trigger)
	}
}

func TestWorkflowReusableOnly(t *testing.T) {
	for _, test := range []struct {
		name     string
		triggers []Trigger
		want     bool
	}{
		{name: "call only", triggers: []Trigger{{Event: "workflow_call"}}, want: true},
		{name: "duplicate call only", triggers: []Trigger{{Event: "workflow_call"}, {Event: "workflow_call"}}, want: true},
		{name: "call and push", triggers: []Trigger{{Event: "workflow_call"}, {Event: "push"}}},
		{name: "empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := (Workflow{Triggers: test.triggers}).ReusableOnly(); got != test.want {
				t.Fatalf("ReusableOnly() = %t, want %t", got, test.want)
			}
		})
	}
}
