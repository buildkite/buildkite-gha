package program_test

import (
	"reflect"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/program"
)

func TestConcreteAdapterUsesEachSiteSurface(t *testing.T) {
	expressionContext := expression.Context{
		Env:    map[string]string{"VALUE": "resolved"},
		Matrix: map[string]any{"enabled": true, "minutes": 5},
		Needs:  map[string]expression.NeedStatus{"build": {Outputs: map[string]string{"services": `{"db":{"image":"postgres"}}`}}},
	}
	conditionContext := expression.ConditionContext{Matrix: expressionContext.Matrix}
	context := program.EvaluationContext{Expression: expressionContext, Condition: conditionContext}
	for _, test := range []struct {
		name    string
		surface program.Surface
		result  program.ResultType
		source  string
		want    any
	}{
		{name: "job condition", surface: program.SurfaceJobCondition, result: program.ResultBoolean, source: "matrix.enabled", want: true},
		{name: "step condition", surface: program.SurfaceStepCondition, result: program.ResultBoolean, source: "matrix.enabled", want: true},
		{name: "job environment", surface: program.SurfaceJobEnvironment, result: program.ResultString, source: "${{ matrix.enabled }}", want: "true"},
		{name: "job default", surface: program.SurfaceJobDefault, result: program.ResultString, source: "${{ env.VALUE }}", want: "resolved"},
		{name: "job output", surface: program.SurfaceJobOutput, result: program.ResultString, source: "${{ env.VALUE }}", want: "resolved"},
		{name: "step template", surface: program.SurfaceStepTemplate, result: program.ResultString, source: "value-${{ matrix.enabled }}", want: "value-true"},
		{name: "step control", surface: program.SurfaceStepControl, result: program.ResultNumber, source: "${{ matrix.minutes }}", want: 5},
		{name: "runtime template", surface: program.SurfaceRuntimeTemplate, result: program.ResultString, source: "${{ needs.build.outputs.services }}", want: `{"db":{"image":"postgres"}}`},
		{name: "service credential", surface: program.SurfaceServiceCredential, result: program.ResultString, source: "${{ env.VALUE }}", want: "resolved"},
		{name: "service map", surface: program.SurfaceServiceMap, result: program.ResultObject, source: "${{ fromJSON(needs.build.outputs.services) }}", want: []expression.ObjectEntry{{Name: "db", Value: map[string]any{"image": "postgres"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := program.EvaluateSite(program.Site{Source: test.source, Surface: test.surface, Result: test.result}, context)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("EvaluateSite() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestConcreteAdapterRejectsMismatchedResultType(t *testing.T) {
	for _, result := range []program.ResultType{program.ResultBoolean, program.ResultNumber} {
		_, err := program.EvaluateSite(program.Site{Source: "${{ 'true' }}", Surface: program.SurfaceStepControl, Result: result}, program.EvaluationContext{})
		if err == nil || err.Error() != "expression produced string, want "+string(result) {
			t.Errorf("EvaluateSite() error = %v, want %s mismatch", err, result)
		}
	}
}

func TestEvaluateBindingsPreservesOrderAtFailure(t *testing.T) {
	bindings := []program.Binding{
		{Name: "first", Value: program.Site{Source: "${{ unsupported.first }}", Surface: program.SurfaceStepTemplate, Result: program.ResultString}},
		{Name: "second", Value: program.Site{Source: "${{ unsupported.second }}", Surface: program.SurfaceStepTemplate, Result: program.ResultString}},
	}
	_, err := program.EvaluateBindings(bindings, program.EvaluationContext{})
	if err == nil || err.Error() != `evaluate "first": unsupported runtime expression "unsupported.first"` {
		t.Fatalf("EvaluateBindings() error = %v", err)
	}
}

func TestTransformSitesIncludesActionsWithoutMutatingSource(t *testing.T) {
	original := program.Program{Version: program.Version, Job: program.Job{Condition: program.Site{Source: "job"}}, Actions: map[string]program.Action{
		"a-1": {Runtime: "composite", Steps: []program.ActionStep{{Run: &program.ActionRun{Command: program.Site{Source: "action"}}}}},
	}}
	transformed, err := original.TransformSites(func(site program.Site) (program.Site, error) {
		site.Source += "-reduced"
		return site, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if original.Job.Condition.Source != "job" || original.Actions["a-1"].Steps[0].Run.Command.Source != "action" {
		t.Fatalf("TransformSites() mutated source: %#v", original)
	}
	if transformed.Job.Condition.Source != "job-reduced" || transformed.Actions["a-1"].Steps[0].Run.Command.Source != "action-reduced" {
		t.Fatalf("TransformSites() omitted a site: %#v", transformed)
	}
}
