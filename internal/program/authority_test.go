package program

import (
	"testing"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

func TestInventoryAuthorityDoesNotNarrowUnknownConditions(t *testing.T) {
	condition := func(source string, surface Surface) Site {
		return Site{Source: source, Surface: surface, Result: ResultBoolean, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression}
	}
	token := Site{Source: "${{ github.token }}", Surface: SurfaceStepTemplate, Result: ResultString, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression}
	tests := []struct {
		name  string
		guard string
		job   string
		step  string
	}{
		{name: "failure step", step: "failure()"},
		{name: "cancelled step", step: "cancelled()"},
		{name: "success comparison step", step: "success() == false"},
		{name: "environment step", step: "env.FLAG == 'x'"},
		{name: "variables job", job: "vars.ENABLED == 'true'"},
		{name: "call guard", guard: "failure()"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			program := Program{Version: Version, Job: Job{
				Condition: condition("", SurfaceJobCondition),
				Steps: []Step{{
					Condition: condition(test.step, SurfaceStepCondition),
					Run:       &Run{Command: token},
				}}}}
			if test.job != "" {
				program.Job.Condition = condition(test.job, SurfaceJobCondition)
			}
			if test.guard != "" {
				program.Job.Guards = []Guard{{Condition: condition(test.guard, SurfaceCallCondition)}}
			}
			authority, err := InventoryAuthority(program, AuthorityOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !authority.GitHubToken {
				t.Fatal("unknown condition removed github.token authority")
			}
		})
	}
}

func TestInventoryAuthorityNarrowsExplicitKnownConditionValues(t *testing.T) {
	program := Program{Version: Version, Job: Job{Condition: Site{Surface: SurfaceJobCondition, Result: ResultBoolean, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression}, Steps: []Step{{
		Condition: Site{Source: "env.FLAG", Surface: SurfaceStepCondition, Result: ResultBoolean, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression},
		Run:       &Run{Command: Site{Source: "${{ github.token }}", Surface: SurfaceStepTemplate, Result: ResultString, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression}},
	}}}}
	authority, err := InventoryAuthority(program, AuthorityOptions{Values: expression.AbstractValues{References: map[string]any{"env.flag": ""}}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("known-false condition retained github.token authority")
	}
}

func TestWorkflowReachabilityIncludesGuardsAndPlanningValues(t *testing.T) {
	condition := func(source string, surface Surface) Site {
		return Site{Source: source, Surface: surface, Result: ResultBoolean, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression}
	}
	workflow := Program{Version: Version, Job: Job{
		Guards:    []Guard{{Condition: condition("inputs.guard", SurfaceCallCondition)}},
		Condition: condition("matrix.job", SurfaceJobCondition),
		Steps: []Step{
			{Condition: condition("vars.STEP == 'run'", SurfaceStepCondition)},
			{Condition: condition("env.RUNTIME == 'yes'", SurfaceStepCondition)},
		},
	}}
	values := expression.AbstractValues{References: map[string]any{
		"inputs": map[string]any{"guard": true},
		"matrix": map[string]any{"job": true},
		"vars":   map[string]string{"STEP": "skip"},
	}}
	reachability, err := WorkflowReachability(workflow, values)
	if err != nil {
		t.Fatal(err)
	}
	if !reachability.Job || len(reachability.Steps) != 2 || reachability.Steps[0] || !reachability.Steps[1] {
		t.Fatalf("reachability = %#v, want known-false first step and unknown second step", reachability)
	}
	values.References["inputs"] = map[string]any{"guard": false}
	reachability, err = WorkflowReachability(workflow, values)
	if err != nil || reachability.Job || reachability.Steps[0] || reachability.Steps[1] {
		t.Fatalf("known-false call guard reachability = %#v, %v", reachability, err)
	}
}

func TestInventoryAuthorityHonorsLazyCaseBranches(t *testing.T) {
	workflow := Program{Version: Version, Job: Job{
		Condition: Site{Surface: SurfaceJobCondition, Result: ResultBoolean, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression},
		Steps: []Step{{
			Condition: Site{Surface: SurfaceStepCondition, Result: ResultBoolean, Provenance: ProvenanceWorkflow, Purpose: PurposeExpression},
			Run: &Run{Command: Site{
				Source: "${{ case(true, 'safe', github.token) }}", Surface: SurfaceStepTemplate, Result: ResultString,
				Provenance: ProvenanceWorkflow, Purpose: PurposeExpression,
			}},
		}},
	}}
	authority, err := InventoryAuthority(workflow, AuthorityOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("unselected case branch retained github.token authority")
	}
}
