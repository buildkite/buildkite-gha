package buildkite

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestEmitWrapsNestedReusableWorkflowConcurrencyGates(t *testing.T) {
	outer := ConcurrencyGate{ID: "call", Group: "buildkite-gha/concurrency/outer"}
	inner := ConcurrencyGate{ID: "call-inner", Group: "buildkite-gha/concurrency/inner"}
	pipeline := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Jobs: []Job{
			{Key: "prepare", Label: "Prepare", Queue: "linux", PlanDigest: testDigest("prepare")},
			{Key: "outer_start", Label: "Outer start", Queue: "linux", PlanDigest: testDigest("outer-start"), Dependencies: []string{"prepare"}, ConcurrencyGates: []ConcurrencyGate{outer}},
			{Key: "inner", Label: "Inner", Queue: "mac", PlanDigest: testDigest("inner"), Dependencies: []string{"outer_start"}, ConcurrencyGates: []ConcurrencyGate{outer, inner}},
			{Key: "outer_finish", Label: "Outer finish", Queue: "linux", PlanDigest: testDigest("outer-finish"), Dependencies: []string{"inner"}, ConcurrencyGates: []ConcurrencyGate{outer}},
		},
	}
	output, err := Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	type emittedDependency struct {
		Step         string `yaml:"step"`
		AllowFailure bool   `yaml:"allow_failure"`
	}
	type emittedStep struct {
		Label            string              `yaml:"label"`
		Key              string              `yaml:"key"`
		ConcurrencyGroup string              `yaml:"concurrency_group"`
		DependsOn        []emittedDependency `yaml:"depends_on"`
		Agents           map[string]string   `yaml:"agents"`
	}
	var document struct {
		Steps []emittedStep `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 8 {
		t.Fatalf("nested reusable gate steps = %d, want 8\n%s", len(document.Steps), output)
	}
	outerOpen, outerClose := document.Steps[0], document.Steps[1]
	innerOpen, innerClose := document.Steps[2], document.Steps[3]
	if outerOpen.ConcurrencyGroup != outer.Group || outerOpen.Agents["queue"] != "linux" || len(outerOpen.DependsOn) != 2 || outerOpen.DependsOn[0].Step != "importer" || outerOpen.DependsOn[0].AllowFailure || outerOpen.DependsOn[1].Step != "prepare" || !outerOpen.DependsOn[1].AllowFailure {
		t.Fatalf("outer opening gate = %#v", outerOpen)
	}
	if innerOpen.ConcurrencyGroup != inner.Group || innerOpen.Agents["queue"] != "mac" || len(innerOpen.DependsOn) != 2 || innerOpen.DependsOn[0].Step != outerOpen.Key || innerOpen.DependsOn[0].AllowFailure || innerOpen.DependsOn[1].Step != "outer_start" || !innerOpen.DependsOn[1].AllowFailure {
		t.Fatalf("inner opening gate = %#v", innerOpen)
	}
	if innerClose.ConcurrencyGroup != inner.Group || len(innerClose.DependsOn) != 1 || innerClose.DependsOn[0].Step != "inner" || !innerClose.DependsOn[0].AllowFailure {
		t.Fatalf("inner closing gate = %#v", innerClose)
	}
	if outerClose.ConcurrencyGroup != outer.Group || len(outerClose.DependsOn) != 4 || outerClose.DependsOn[3].Step != innerClose.Key || !outerClose.DependsOn[3].AllowFailure {
		t.Fatalf("outer closing gate = %#v", outerClose)
	}
}

func TestEmitReusableConcurrencyWaitsForExternalPrerequisites(t *testing.T) {
	gate := ConcurrencyGate{ID: "call", Group: "buildkite-gha/concurrency/deploy"}
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Jobs: []Job{
			{Key: "prepare", Label: "Prepare", Queue: "linux", PlanDigest: testDigest("prepare"), Concurrency: 1, ConcurrencyGroup: "buildkite-gha/concurrency/prepare"},
			{Key: "approve", Label: "Approve", Queue: "linux", PlanDigest: testDigest("approve"), Dependencies: []string{"prepare"}},
			{Key: "audit", Label: "Audit", Queue: "linux", PlanDigest: testDigest("audit")},
			{Key: "deploy", Label: "Deploy", Queue: "linux", PlanDigest: testDigest("deploy"), Dependencies: []string{"approve"}, ConcurrencyGates: []ConcurrencyGate{gate}},
			{Key: "verify", Label: "Verify", Queue: "linux", PlanDigest: testDigest("verify"), Dependencies: []string{"approve", "deploy", "audit"}, ConcurrencyGates: []ConcurrencyGate{gate}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Key              string `yaml:"key"`
			ConcurrencyGroup string `yaml:"concurrency_group"`
			Concurrency      int    `yaml:"concurrency"`
			DependsOn        []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	open, close := document.Steps[0], document.Steps[1]
	if open.ConcurrencyGroup != gate.Group || close.ConcurrencyGroup != gate.Group || open.Concurrency != 1 || close.Concurrency != 1 {
		t.Fatalf("gate markers must stay adjacent and ordered with concurrency 1\n%s", output)
	}
	if len(open.DependsOn) != 3 || open.DependsOn[0].Step != "importer" || open.DependsOn[0].AllowFailure || open.DependsOn[1].Step != "approve" || !open.DependsOn[1].AllowFailure || open.DependsOn[2].Step != "audit" || !open.DependsOn[2].AllowFailure {
		t.Fatalf("opening gate must wait for deduplicated external prerequisites, including failed ones: %#v", open)
	}
	if len(close.DependsOn) != 2 || close.DependsOn[0].Step != "deploy" || close.DependsOn[1].Step != "verify" || !close.DependsOn[0].AllowFailure || !close.DependsOn[1].AllowFailure {
		t.Fatalf("closing gate must wait for all members with allow_failure: %#v", close)
	}
	for _, step := range document.Steps {
		if step.Key == "deploy" {
			if len(step.DependsOn) != 3 || step.DependsOn[1].Step != "approve" || !step.DependsOn[1].AllowFailure || step.DependsOn[2].Step != open.Key || step.DependsOn[2].AllowFailure {
				t.Fatalf("member dependencies must retain prerequisite results and strict gate admission: %#v", step)
			}
		}
	}
}

func TestEmitRejectsReusableConcurrencyPrerequisiteQueueCycles(t *testing.T) {
	x := ConcurrencyGate{ID: "x", Group: "group-x"}
	y := ConcurrencyGate{ID: "y", Group: "group-y"}
	for _, test := range []struct {
		name string
		jobs []Job
	}{
		{
			name: "gates wait on each others job groups",
			jobs: []Job{
				{Key: "prepare_x", Concurrency: 1, ConcurrencyGroup: x.Group},
				{Key: "prepare_y", Concurrency: 1, ConcurrencyGroup: y.Group},
				{Key: "call_x", Dependencies: []string{"prepare_y"}, ConcurrencyGates: []ConcurrencyGate{x}},
				{Key: "call_y", Dependencies: []string{"prepare_x"}, ConcurrencyGates: []ConcurrencyGate{y}},
			},
		},
		{
			name: "lifted dependencies cross gate scopes",
			jobs: []Job{
				{Key: "x_first", ConcurrencyGates: []ConcurrencyGate{x}},
				{Key: "y_first", ConcurrencyGates: []ConcurrencyGate{y}},
				{Key: "x_last", Dependencies: []string{"y_first"}, ConcurrencyGates: []ConcurrencyGate{x}},
				{Key: "y_last", Dependencies: []string{"x_first"}, ConcurrencyGates: []ConcurrencyGate{y}},
			},
		},
		{
			name: "nested gate queues behind sibling close",
			jobs: []Job{
				{Key: "prepare", Concurrency: 1, ConcurrencyGroup: y.Group},
				{Key: "call_x", Dependencies: []string{"prepare"}, ConcurrencyGates: []ConcurrencyGate{x}},
				{Key: "inner_y", ConcurrencyGates: []ConcurrencyGate{x, y}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for i := range test.jobs {
				test.jobs[i].Label = test.jobs[i].Key
				test.jobs[i].Queue = "linux"
				test.jobs[i].PlanDigest = testDigest(test.jobs[i].Key)
			}
			_, err := Emit(Pipeline{CompilerStep: "importer", DistributionDigest: testDigest("distribution"), Jobs: test.jobs})
			if err == nil || !strings.Contains(err.Error(), "concurrency queue and prerequisite dependencies form a cycle") {
				t.Fatalf("Emit() error = %v, want queue cycle rejection", err)
			}
		})
	}
}

func TestEmitRejectsConcurrencyGroupSharedWithMemberJob(t *testing.T) {
	group := "buildkite-gha/concurrency/deploy"
	tests := []struct {
		name     string
		pipeline Pipeline
		want     string
	}{
		{
			name: "workflow",
			pipeline: Pipeline{
				CompilerStep: "importer", DistributionDigest: testDigest("distribution"), ConcurrencyGate: &ConcurrencyGate{Group: group},
				Jobs: []Job{{Key: "deploy", Label: "Deploy", Queue: "linux", PlanDigest: testDigest("workflow-deploy"), Concurrency: 1, ConcurrencyGroup: group}},
			},
			want: `workflow concurrency gate shares group with member job "deploy"`,
		},
		{
			name: "reusable workflow",
			pipeline: Pipeline{
				CompilerStep: "importer", DistributionDigest: testDigest("distribution"),
				Jobs: []Job{{Key: "deploy", Label: "Deploy", Queue: "linux", PlanDigest: testDigest("reusable-deploy"), Concurrency: 1, ConcurrencyGroup: group, ConcurrencyGates: []ConcurrencyGate{{ID: "call", Group: group}}}},
			},
			want: `reusable-workflow concurrency gate "call" shares group with member job "deploy"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Emit(test.pipeline)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Emit() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEmitRejectsConcurrencyGroupSharedWithEnclosingGate(t *testing.T) {
	group := "buildkite-gha/concurrency/deploy"
	tests := []struct {
		name     string
		pipeline Pipeline
		want     string
	}{
		{
			name: "workflow",
			pipeline: Pipeline{
				CompilerStep: "importer", DistributionDigest: testDigest("distribution"), ConcurrencyGate: &ConcurrencyGate{Group: group},
				Jobs: []Job{{Key: "deploy", Label: "Deploy", Queue: "linux", PlanDigest: testDigest("workflow-gate"), ConcurrencyGates: []ConcurrencyGate{{ID: "call", Group: group}}}},
			},
			want: `concurrency gate "call" shares group with enclosing workflow gate`,
		},
		{
			name: "nested reusable workflow",
			pipeline: Pipeline{
				CompilerStep: "importer", DistributionDigest: testDigest("distribution"),
				Jobs: []Job{{Key: "deploy", Label: "Deploy", Queue: "linux", PlanDigest: testDigest("nested-gate"), ConcurrencyGates: []ConcurrencyGate{{ID: "outer", Group: group}, {ID: "inner", Group: group}}}},
			},
			want: `concurrency gate "inner" shares group with enclosing gate "outer"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Emit(test.pipeline)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Emit() error = %v, want %q", err, test.want)
			}
		})
	}
}
