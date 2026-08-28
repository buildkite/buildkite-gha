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
