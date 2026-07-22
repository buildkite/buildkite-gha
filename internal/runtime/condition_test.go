package runtime

import (
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestConditionStatusOutputsAndTruthiness(t *testing.T) {
	context := conditionContext{
		needs:   map[string]plan.Need{"build": {Result: "failure", Outputs: map[string]string{"gate": "yes"}}},
		steps:   map[string]stepStatus{"soft": {Outcome: "failure", Conclusion: "success", Outputs: map[string]string{"ready": "true"}}},
		failure: true,
	}
	for _, condition := range []string{
		"always() && needs.build.result == 'failure' && needs.build.outputs.gate == 'yes'",
		"failure() && steps.soft.outcome == 'failure' && steps.soft.conclusion == 'success'",
		"always() && steps.soft.outputs.ready",
	} {
		got, err := evaluateCondition(condition, context)
		if err != nil || !got {
			t.Fatalf("evaluateCondition(%q) = %v, %v", condition, got, err)
		}
	}
	for _, condition := range []string{"''", "0", "false", "null"} {
		got, err := evaluateCondition(condition, conditionContext{})
		if err != nil || got {
			t.Fatalf("evaluateCondition(%q) = %v, %v, want false", condition, got, err)
		}
	}
}

func TestConditionFailsClosedForUnsupportedComparison(t *testing.T) {
	if _, err := evaluateCondition("1 < 2", conditionContext{}); err == nil {
		t.Fatal("evaluateCondition() accepted unsupported ordered comparison")
	}
	if _, err := evaluateCondition("true == 'true'", conditionContext{}); err == nil {
		t.Fatal("evaluateCondition() silently coerced mixed equality operands")
	}
	if got, err := evaluateCondition("", conditionContext{unsuccessful: true}); err != nil || got {
		t.Fatalf("default condition after skipped prerequisite = %v, %v, want false", got, err)
	}
}
