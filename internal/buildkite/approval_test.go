package buildkite

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

type emittedApprovalStep struct {
	Label   string `yaml:"label"`
	Block   string `yaml:"block"`
	Key     string `yaml:"key"`
	Prompt  string `yaml:"prompt"`
	Blocked string `yaml:"blocked_state"`
	Retry   *struct {
		Manual struct {
			Allowed bool `yaml:"allowed"`
		} `yaml:"manual"`
	} `yaml:"retry"`
	DependsOn []struct {
		Step         string `yaml:"step"`
		AllowFailure bool   `yaml:"allow_failure"`
	} `yaml:"depends_on"`
}

func TestEmitApprovalGateBlocksAfterJobs(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		ApprovalGates:      []ApprovalGate{{Key: "gha-approve-production-abcdef012345", Environment: "production"}},
		Jobs: []Job{
			{Key: "gha-build", Label: "Build", PlanDigest: testDigest("build")},
			{Key: "gha-deploy", Label: "Deploy", PlanDigest: testDigest("deploy"), Dependencies: []string{"gha-build"}, ApprovalGate: "gha-approve-production-abcdef012345"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []emittedApprovalStep `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 3 {
		t.Fatalf("steps = %#v\n%s", document.Steps, output)
	}
	build, deploy, block := document.Steps[0], document.Steps[1], document.Steps[2]
	if build.Retry != nil || len(build.DependsOn) != 1 || build.DependsOn[0].Step != "importer" {
		t.Fatalf("ungated job = %#v", build)
	}
	if deploy.Retry == nil || deploy.Retry.Manual.Allowed {
		t.Fatalf("gated job retry = %#v", deploy.Retry)
	}
	if len(deploy.DependsOn) != 3 || deploy.DependsOn[0].Step != "importer" || deploy.DependsOn[1].Step != "gha-build" || deploy.DependsOn[2].Step != "gha-approve-production-abcdef012345" || deploy.DependsOn[2].AllowFailure {
		t.Fatalf("gated job dependencies = %#v", deploy.DependsOn)
	}
	if block.Block != ":github: approval · production" || block.Key != "gha-approve-production-abcdef012345" || block.Blocked != "running" || !strings.Contains(block.Prompt, "production") {
		t.Fatalf("block step = %#v", block)
	}
	// The explicit compiler-step dependency severs the block step's implicit
	// dependency on the preceding jobs.
	if len(block.DependsOn) != 1 || block.DependsOn[0].Step != "importer" || block.DependsOn[0].AllowFailure {
		t.Fatalf("block step dependencies = %#v", block.DependsOn)
	}
}

func TestEmitAggregateWorkflowApprovalGates(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		EventProvider:      "github",
		Workflows: []Workflow{{
			GroupLabel: "Deploy", GroupKey: "gha-workflow-1111111111111111", Event: "push", Condition: `build.env("BUILDKITE_GITHUB_EVENT") == "push"`,
			ApprovalGates: []ApprovalGate{{Key: "gha-1111111111111111-approve-production-abcdef012345", Environment: "production"}},
			Jobs: []Job{
				{Key: "gha-1111111111111111-deploy", Label: "Deploy", PlanDigest: testDigest("deploy"), ApprovalGate: "gha-1111111111111111-approve-production-abcdef012345"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Group string                `yaml:"group"`
			Steps []emittedApprovalStep `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 || len(document.Steps[0].Steps) != 2 {
		t.Fatalf("aggregate steps = %#v\n%s", document.Steps, output)
	}
	deploy, block := document.Steps[0].Steps[0], document.Steps[0].Steps[1]
	if len(deploy.DependsOn) != 1 || deploy.DependsOn[0].Step != "gha-1111111111111111-approve-production-abcdef012345" || deploy.DependsOn[0].AllowFailure {
		t.Fatalf("aggregate gated job dependencies = %#v", deploy.DependsOn)
	}
	if deploy.Retry == nil || deploy.Retry.Manual.Allowed {
		t.Fatalf("aggregate gated job retry = %#v", deploy.Retry)
	}
	if block.Block != ":github: approval · production" || block.Blocked != "running" || len(block.DependsOn) != 1 || block.DependsOn[0].Step != "importer" {
		t.Fatalf("aggregate block step = %#v", block)
	}
}

func TestEmitKeylessAggregateApprovalGateUsesEmptyDependencies(t *testing.T) {
	output, err := Emit(Pipeline{
		ArtifactProducer:   "22222222-2222-4222-8222-222222222222",
		DistributionDigest: testDigest("distribution"),
		EventProvider:      "github",
		Workflows: []Workflow{{
			GroupLabel: "Deploy", GroupKey: "gha-workflow-1111111111111111", Event: "push", Condition: "true",
			ApprovalGates: []ApprovalGate{{Key: "gha-1111111111111111-approve-production-abcdef012345", Environment: "production"}},
			Jobs: []Job{
				{Key: "gha-1111111111111111-deploy", Label: "Deploy", PlanDigest: testDigest("deploy"), ApprovalGate: "gha-1111111111111111-approve-production-abcdef012345"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Steps []struct {
				Block     string    `yaml:"block"`
				DependsOn yaml.Node `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 || len(document.Steps[0].Steps) != 2 {
		t.Fatalf("aggregate steps = %#v\n%s", document.Steps, output)
	}
	block := document.Steps[0].Steps[1]
	// Without an importer step key, only an explicit empty dependency list
	// stops the block step from implicitly waiting on the job that depends on
	// it. Omitting depends_on would deadlock the workflow.
	if block.Block != ":github: approval · production" || block.DependsOn.Kind != yaml.SequenceNode || len(block.DependsOn.Content) != 0 {
		t.Fatalf("keyless block step = %#v\n%s", block, output)
	}
	if strings.Contains(string(output), `- step: ""`) {
		t.Fatalf("keyless pipeline references an empty step key:\n%s", output)
	}
}

func TestEmitRejectsInvalidApprovalGates(t *testing.T) {
	job := Job{Key: "gha-deploy", Label: "Deploy", PlanDigest: testDigest("deploy")}
	gated := job
	gated.ApprovalGate = "gha-approve-production-abcdef012345"
	gate := ApprovalGate{Key: "gha-approve-production-abcdef012345", Environment: "production"}
	cases := []struct {
		name     string
		pipeline Pipeline
		want     string
	}{
		{
			"undeclared gate",
			Pipeline{Jobs: []Job{gated}},
			`references undeclared approval gate`,
		},
		{
			"gate without dependent jobs",
			Pipeline{ApprovalGates: []ApprovalGate{gate}, Jobs: []Job{job}},
			`has no dependent jobs`,
		},
		{
			"invalid gate key",
			Pipeline{ApprovalGates: []ApprovalGate{{Key: "not a key", Environment: "production"}}, Jobs: []Job{gated}},
			`invalid approval gate key`,
		},
		{
			"missing environment",
			Pipeline{ApprovalGates: []ApprovalGate{{Key: gate.Key}}, Jobs: []Job{gated}},
			`requires an environment name`,
		},
		{
			"key collision",
			Pipeline{ApprovalGates: []ApprovalGate{gate, gate}, Jobs: []Job{gated}},
			`collides with`,
		},
		{
			"self-referencing job",
			Pipeline{ApprovalGates: []ApprovalGate{gate}, Jobs: []Job{{Key: gate.Key, Label: "Deploy", PlanDigest: testDigest("deploy"), ApprovalGate: gate.Key}}},
			`has invalid approval gate`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.pipeline.CompilerStep = "importer"
			testCase.pipeline.DistributionDigest = testDigest("distribution")
			if _, err := Emit(testCase.pipeline); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Emit() error = %v, want %q", err, testCase.want)
			}
		})
	}
}
