package compiler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestBuildPlanDependenciesOwnsGraphToPlanBoundary(t *testing.T) {
	output := NeedOutput{Name: "artifact", StepKey: "build-1", Output: "digest"}
	instance := JobInstance{
		LogicalJobID: "consume",
		NeedGroups:   map[string][]string{"build": {"build-1"}},
		NeedOutputs:  map[string][]NeedOutput{"build": {output}},
		DeferredInputs: map[string]DeferredInput{
			"digest": {Sources: []string{"build-1"}, Outputs: []NeedOutput{output}},
		},
		CallGuards: []CallGuard{{
			Condition:   "success()",
			Inputs:      map[string]any{"target": "release"},
			NeedGroups:  map[string][]string{"build": {"build-1"}, "status": {"build-1"}},
			NeedOutputs: map[string][]NeedOutput{"build": {output}, "status": {}},
		}},
	}
	digests := map[string]string{"build-1": "sha256:" + strings.Repeat("0", 64)}
	wantSource := plan.NeedSource{StepKey: "build-1", PlanDigest: digests["build-1"]}

	sources, err := buildPlanNeedSources(instance, digests)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := buildPlanDeferredInputs(instance.DeferredInputs, digests)
	if err != nil {
		t.Fatal(err)
	}
	guards, err := buildPlanCallGuards(instance, digests)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(sources, map[string][]plan.NeedSource{"build": {wantSource}}) {
		t.Fatalf("need sources = %#v", sources)
	}
	if !reflect.DeepEqual(buildPlanNeedOutputs(instance), map[string][]plan.NeedOutput{"build": {output}}) {
		t.Fatalf("need outputs were not preserved")
	}
	if !reflect.DeepEqual(deferred["digest"], plan.DeferredInput{Sources: []plan.NeedSource{wantSource}, Outputs: []plan.NeedOutput{output}}) {
		t.Fatalf("deferred input = %#v", deferred["digest"])
	}
	if len(guards) != 1 {
		t.Fatalf("call guards = %#v", guards)
	}
	wantGuardSources := map[string][]plan.NeedSource{"build": {wantSource}, "status": {wantSource}}
	if !reflect.DeepEqual(guards[0].NeedSources, wantGuardSources) {
		t.Fatalf("call guard sources = %#v", guards[0].NeedSources)
	}
	if _, emitted := guards[0].NeedOutputs["status"]; emitted {
		t.Fatalf("status-only call guard output was emitted: %#v", guards[0].NeedOutputs)
	}
	if !reflect.DeepEqual(guards[0].NeedOutputs["build"], []plan.NeedOutput{output}) {
		t.Fatalf("call guard outputs = %#v", guards[0].NeedOutputs)
	}
}
