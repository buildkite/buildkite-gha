package buildkite

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/transport"
	"go.yaml.in/yaml/v4"
)

func TestEmitGolden(t *testing.T) {
	producerDigest := testDigest("producer plan")
	consumerOneDigest := testDigest("consumer one plan")
	consumerTwoDigest := testDigest("consumer two plan")
	pipeline := Pipeline{
		CompilerStep:       "gha-importer",
		DistributionDigest: testDigest("buildkite-gha executable"),
		Jobs: []Job{
			{Key: "gha-consumer-two", Label: `Consumer ($VALUE, variant="two")`, Queue: "gha-linux", PlanDigest: consumerTwoDigest, Dependencies: []string{"gha-producer"}, ConcurrencyGroup: "buildkite-gha/shell/consumer", Concurrency: 2},
			{Key: "gha-producer", Label: "Producer", Queue: "gha-linux", PlanDigest: producerDigest},
			{Key: "gha-consumer-one", Label: "Consumer (variant=one)", Queue: "gha-linux", PlanDigest: consumerOneDigest, Dependencies: []string{"gha-producer"}, ConcurrencyGroup: "buildkite-gha/shell/consumer", Concurrency: 2},
		},
	}
	first, err := Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("repeated emission was not byte-identical")
	}
	want, err := os.ReadFile(filepath.Join("testdata", "pipeline.golden.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, want) {
		t.Fatalf("pipeline changed\nwant:\n%s\ngot:\n%s", want, first)
	}
	var document struct {
		Steps []struct {
			Key      string `yaml:"key"`
			Command  string `yaml:"command"`
			Checkout struct {
				Skip bool `yaml:"skip"`
			} `yaml:"checkout"`
			DependsOn []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(first, &document); err != nil {
		t.Fatalf("parse emitted YAML: %v", err)
	}
	if len(document.Steps) != 3 {
		t.Fatalf("emitted steps = %#v", document.Steps)
	}
	for _, step := range document.Steps {
		if !step.Checkout.Skip {
			t.Fatalf("step %q does not skip checkout", step.Key)
		}
		if len(step.DependsOn) == 0 || step.DependsOn[0].Step != "gha-importer" || step.DependsOn[0].AllowFailure {
			t.Fatalf("step %q lacks strict compiler dependency: %#v", step.Key, step.DependsOn)
		}
		for _, dependency := range step.DependsOn[1:] {
			if !dependency.AllowFailure {
				t.Fatalf("step %q logical dependency is strict: %#v", step.Key, dependency)
			}
		}
		if !strings.HasPrefix(step.Command, "set -euo pipefail\n") ||
			!strings.Contains(step.Command, `bootstrap_dir="$(mktemp -d `) ||
			!strings.Contains(step.Command, `artifact download '.buildkite-gha/distributions/`) ||
			!strings.Contains(step.Command, `sha256sum "$distribution"`) ||
			!strings.Contains(step.Command, `run-job --plan-digest `) ||
			!strings.Contains(step.Command, `--plan-producer 'gha-importer'`) {
			t.Fatalf("step %q does not verify its distribution before delegated plan acquisition:\n%s", step.Key, step.Command)
		}
	}
	if !strings.Contains(string(first), `artifact download '.buildkite-gha/distributions/`) ||
		strings.Contains(string(first), `artifact download '.buildkite-gha/plans/`) ||
		strings.Contains(string(first), `.buildkite-gha/bootstrap/`) ||
		strings.Contains(string(first), "go run") ||
		strings.Contains(string(first), "cache:") ||
		strings.Contains(string(first), "BUILDKITE_GHA_MISE_DATA_DIR") {
		t.Fatalf("generated jobs are not self-contained:\n%s", first)
	}
	if strings.Count(string(first), `--step 'gha-importer'`) != 3 {
		t.Fatalf("generated distribution downloads are not constrained to the exact importer:\n%s", first)
	}
	if !strings.Contains(string(first), `Consumer ($VALUE, variant=\"two\")`) {
		t.Fatal("runtime dollar sign or quoted label did not survive scalar encoding")
	}
}

func TestEmitMergesIntoContainingGroup(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		GroupLabel:         ":github: Run workflow",
		Jobs:               []Job{{Key: "job", Label: "Job", Queue: "hosted", PlanDigest: testDigest("plan")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Group string `yaml:"group"`
			Key   string `yaml:"key"`
			Steps []struct {
				Key       string `yaml:"key"`
				DependsOn []struct {
					Step         string `yaml:"step"`
					AllowFailure bool   `yaml:"allow_failure"`
				} `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatalf("parse grouped pipeline: %v\n%s", err, output)
	}
	if len(document.Steps) != 1 || document.Steps[0].Group != ":github: Run workflow" || document.Steps[0].Key != "" || len(document.Steps[0].Steps) != 1 {
		t.Fatalf("grouped pipeline = %#v", document.Steps)
	}
	job := document.Steps[0].Steps[0]
	if job.Key != "job" || len(job.DependsOn) != 1 || job.DependsOn[0].Step != "importer" || job.DependsOn[0].AllowFailure {
		t.Fatalf("grouped job = %#v", job)
	}
}

func TestEmitMarksToleratedJobsAsSoftFailures(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Jobs:               []Job{{Key: "report", Label: "Report", PlanDigest: testDigest("plan"), SoftFail: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			SoftFail []struct {
				ExitStatus int `yaml:"exit_status"`
			} `yaml:"soft_fail"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 || len(document.Steps[0].SoftFail) != 1 || document.Steps[0].SoftFail[0].ExitStatus != ContinueOnErrorExitStatus {
		t.Fatalf("emitted pipeline = %s, want only exit status %d soft-failed", output, ContinueOnErrorExitStatus)
	}
}

func TestEmitAggregateWorkflowGroups(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Workflows: []Workflow{
			{
				GroupLabel: "CI", GroupKey: "gha-workflow-1111111111111111", CheckName: "Buildkite / CI (push)", Condition: `build.source_event == "push"`,
				Jobs: []Job{{Key: "gha-1111111111111111-test", Label: "Test", PlanDigest: testDigest("first plan")}},
			},
			{
				GroupLabel: ".github/workflows/release.yml", GroupKey: "gha-workflow-2222222222222222", CheckName: "Buildkite / .github/workflows/release \"quoted\".yml\nnext (workflow_dispatch)", Condition: `build.source == "ui"`,
				Jobs: []Job{{Key: "gha-2222222222222222-test", Label: "Test", PlanDigest: testDigest("second plan")}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Key       string `yaml:"key"`
			Condition string `yaml:"if"`
			DependsOn string `yaml:"depends_on"`
			Notify    []struct {
				GitHubCheck struct {
					Name string `yaml:"name"`
				} `yaml:"github_check"`
			} `yaml:"notify"`
			Steps []struct {
				Key       string     `yaml:"key"`
				Command   string     `yaml:"command"`
				Notify    any        `yaml:"notify"`
				DependsOn *yaml.Node `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 2 || document.Steps[0].Group != ":github: CI" || document.Steps[0].Key != "gha-workflow-1111111111111111" || document.Steps[0].Condition != `build.source_event == "push"` || document.Steps[0].DependsOn != "importer" || len(document.Steps[0].Notify) != 1 || document.Steps[0].Notify[0].GitHubCheck.Name != "Buildkite / CI (push)" || len(document.Steps[0].Steps) != 1 || document.Steps[0].Steps[0].Key != "gha-1111111111111111-test" || document.Steps[0].Steps[0].Notify != nil {
		t.Fatalf("first aggregate group = %#v\n%s", document.Steps, output)
	}
	if document.Steps[1].Group != ":github: .github/workflows/release.yml" || document.Steps[1].Key != "gha-workflow-2222222222222222" || document.Steps[1].DependsOn != "importer" || len(document.Steps[1].Notify) != 1 || document.Steps[1].Notify[0].GitHubCheck.Name != "Buildkite / .github/workflows/release \"quoted\".yml\nnext (workflow_dispatch)" || len(document.Steps[1].Steps) != 1 || document.Steps[1].Steps[0].Key != "gha-2222222222222222-test" || document.Steps[1].Steps[0].Notify != nil {
		t.Fatalf("second aggregate group = %#v\n%s", document.Steps[1], output)
	}
	for _, group := range document.Steps {
		for _, step := range group.Steps {
			if step.DependsOn != nil {
				t.Fatalf("dependency-free aggregate child %q emitted depends_on: %#v", step.Key, step.DependsOn)
			}
			if strings.Count(step.Command, `--step 'importer'`) != 1 || !strings.Contains(step.Command, `run-job --plan-digest `) || !strings.Contains(step.Command, `--plan-producer 'importer'`) || strings.Contains(step.Command, `artifact download '.buildkite-gha/plans/`) {
				t.Fatalf("aggregate child %q does not delegate plan acquisition to importer: %q", step.Key, step.Command)
			}
		}
	}
	if !strings.Contains(string(output), `name: "Buildkite / .github/workflows/release \"quoted\".yml\nnext (workflow_dispatch)"`) {
		t.Fatalf("GitHub Check name did not use YAML scalar escaping:\n%s", output)
	}
}

func TestEmitAggregateSkippedWorkflowStep(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep: "importer",
		Workflows: []Workflow{{
			GroupLabel: "Pull request",
			GroupKey:   "gha-workflow-1111111111111111",
			CheckName:  "Buildkite / Pull request (push)",
			SkipReason: "This workflow is not triggered by a `push` event",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Label     string `yaml:"label"`
			Key       string `yaml:"key"`
			Type      string `yaml:"type"`
			Condition string `yaml:"if"`
			Skip      string `yaml:"skip"`
			Command   string `yaml:"command"`
			DependsOn string `yaml:"depends_on"`
			Notify    []struct {
				GitHubCheck struct {
					Name string `yaml:"name"`
				} `yaml:"github_check"`
			} `yaml:"notify"`
			Checkout struct {
				Skip bool `yaml:"skip"`
			} `yaml:"checkout"`
			Steps []any `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 {
		t.Fatalf("skipped aggregate steps = %#v\n%s", document.Steps, output)
	}
	step := document.Steps[0]
	if step.Group != "" || step.Label != ":github: Pull request" || step.Key != "gha-workflow-1111111111111111" || step.Type != "command" || step.Condition != "" || step.Skip != "This workflow is not triggered by a `push` event" || step.Command != "" || step.DependsOn != "importer" || len(step.Notify) != 1 || step.Notify[0].GitHubCheck.Name != "Buildkite / Pull request (push)" || len(step.Steps) != 0 || !step.Checkout.Skip {
		t.Fatalf("skipped aggregate step = %#v\n%s", step, output)
	}
}

func TestEmitAggregateWorkflowFailures(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep: "importer",
		Workflows: []Workflow{{
			GroupLabel: "CI",
			GroupKey:   "gha-workflow-1111111111111111",
			CheckName:  "Buildkite / CI (push)",
			Condition:  "true",
			SkipReason: "This workflow is not triggered by a `push` event",
			Failure: &Failure{
				Message: "runner isn't admitted\nmatrix could not be expanded",
				Summary: "The workflow could not be prepared:\n\n- `ci.yml`, job `test`: runner isn't admitted\n- `ci.yml`: matrix could not be expanded",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Group     string `yaml:"group"`
			Label     string `yaml:"label"`
			Key       string `yaml:"key"`
			Condition string `yaml:"if"`
			Skip      string `yaml:"skip"`
			Command   string `yaml:"command"`
			DependsOn string `yaml:"depends_on"`
			Notify    []struct {
				GitHubCheck struct {
					Name   string `yaml:"name"`
					Output struct {
						Title   string `yaml:"title"`
						Summary string `yaml:"summary"`
					} `yaml:"output"`
				} `yaml:"github_check"`
			} `yaml:"notify"`
			Checkout struct {
				Skip bool `yaml:"skip"`
			} `yaml:"checkout"`
			Steps []any `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 {
		t.Fatalf("failure workflow = %#v\n%s", document.Steps, output)
	}
	step := document.Steps[0]
	if step.Group != "" || len(step.Steps) != 0 || step.Label != ":github: CI" || step.Key != "gha-workflow-1111111111111111" || step.Condition != "true" || step.Skip != "" || step.DependsOn != "importer" || len(step.Notify) != 1 || step.Notify[0].GitHubCheck.Name != "Buildkite / CI (push)" || step.Notify[0].GitHubCheck.Output.Title != "Workflow could not be run" || step.Notify[0].GitHubCheck.Output.Summary != "The workflow could not be prepared:\n\n- `ci.yml`, job `test`: runner isn't admitted\n- `ci.yml`: matrix could not be expanded" || step.Command != `printf '%s\n' 'runner isn'"'"'t admitted
matrix could not be expanded' && exit 1` || !step.Checkout.Skip {
		t.Fatalf("failure step = %#v", step)
	}
}

func TestEmitAggregateWorkflowConcurrencyDependencies(t *testing.T) {
	pipeline := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Workflows: []Workflow{{
			GroupLabel:      "CI",
			GroupKey:        "workflow-ci",
			CheckName:       "Buildkite / CI",
			Condition:       "true",
			ConcurrencyGate: &ConcurrencyGate{Group: "buildkite-gha/concurrency/ci"},
			Jobs: []Job{
				{Key: "producer", Label: "Producer", PlanDigest: testDigest("producer")},
				{Key: "consumer", Label: "Consumer", PlanDigest: testDigest("consumer"), Dependencies: []string{"producer"}},
			},
		}},
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
		Key       string              `yaml:"key"`
		DependsOn []emittedDependency `yaml:"depends_on"`
	}
	var document struct {
		Steps []struct {
			DependsOn string        `yaml:"depends_on"`
			Steps     []emittedStep `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatalf("parse aggregate concurrency pipeline: %v\n%s", err, output)
	}
	if len(document.Steps) != 1 || document.Steps[0].DependsOn != "importer" || len(document.Steps[0].Steps) != 4 {
		t.Fatalf("aggregate concurrency group = %#v\n%s", document.Steps, output)
	}
	openKey, closeKey := concurrencyGateKeys("importer\x00workflow-ci", pipeline.Workflows[0].ConcurrencyGate.Group, pipeline.Workflows[0].Jobs)
	open, producer, consumer, close := document.Steps[0].Steps[0], document.Steps[0].Steps[1], document.Steps[0].Steps[2], document.Steps[0].Steps[3]
	if open.Key != openKey || len(open.DependsOn) != 0 {
		t.Fatalf("aggregate opening gate = %#v", open)
	}
	if producer.Key != "producer" || len(producer.DependsOn) != 1 || producer.DependsOn[0].Step != openKey || producer.DependsOn[0].AllowFailure {
		t.Fatalf("aggregate producer dependencies = %#v", producer)
	}
	if consumer.Key != "consumer" || len(consumer.DependsOn) != 2 || consumer.DependsOn[0].Step != openKey || consumer.DependsOn[0].AllowFailure || consumer.DependsOn[1].Step != "producer" || !consumer.DependsOn[1].AllowFailure {
		t.Fatalf("aggregate consumer dependencies = %#v", consumer)
	}
	if close.Key != closeKey || len(close.DependsOn) != 2 || close.DependsOn[0].Step != "producer" || !close.DependsOn[0].AllowFailure || close.DependsOn[1].Step != "consumer" || !close.DependsOn[1].AllowFailure {
		t.Fatalf("aggregate closing gate dependencies = %#v", close)
	}
	for _, step := range document.Steps[0].Steps {
		for _, dependency := range step.DependsOn {
			if dependency.Step == pipeline.CompilerStep {
				t.Fatalf("aggregate child %q retains importer dependency: %#v", step.Key, step.DependsOn)
			}
		}
	}
}

func TestEmitAggregateRequiresGitHubCheckName(t *testing.T) {
	_, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Workflows: []Workflow{{
			GroupLabel: "CI", GroupKey: "workflow-ci", Condition: "true",
			Jobs: []Job{{Key: "test", Label: "Test", PlanDigest: testDigest("plan")}},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), `workflow "workflow-ci" requires a GitHub Check name`) {
		t.Fatalf("missing GitHub Check name error = %v", err)
	}
}

func TestEmitAggregateRejectsCrossWorkflowCollisions(t *testing.T) {
	base := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Workflows: []Workflow{
			{GroupLabel: "One", GroupKey: "workflow-one", CheckName: "Buildkite / One", Condition: "true", Jobs: []Job{{Key: "shared", Label: "One", PlanDigest: testDigest("one")}}},
			{GroupLabel: "Two", GroupKey: "workflow-two", CheckName: "Buildkite / Two", Condition: "true", Jobs: []Job{{Key: "shared", Label: "Two", PlanDigest: testDigest("two")}}},
		},
	}
	if _, err := Emit(base); err == nil || !strings.Contains(err.Error(), `generated step key "shared" collides`) {
		t.Fatalf("duplicate aggregate key error = %v", err)
	}
	base.Workflows[1].Jobs[0].Key = "other"
	base.Workflows[1].Jobs[0].PlanDigest = base.Workflows[0].Jobs[0].PlanDigest
	if _, err := Emit(base); err == nil || !strings.Contains(err.Error(), "share plan digest") {
		t.Fatalf("duplicate aggregate plan error = %v", err)
	}
}

func TestEmitWrapsJobsInWorkflowConcurrencyGate(t *testing.T) {
	pipeline := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		ConcurrencyGate:    &ConcurrencyGate{Group: "buildkite-gha/concurrency/group", Queue: "hosted"},
		Jobs: []Job{
			{Key: "producer", Label: "Producer", Queue: "hosted", PlanDigest: testDigest("producer")},
			{Key: "consumer", Label: "Consumer", Queue: "hosted", PlanDigest: testDigest("consumer"), Dependencies: []string{"producer"}},
		},
	}
	output, err := Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Label            string `yaml:"label"`
			Key              string `yaml:"key"`
			Concurrency      int    `yaml:"concurrency"`
			ConcurrencyGroup string `yaml:"concurrency_group"`
			DependsOn        []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 4 {
		t.Fatalf("steps = %#v\n%s", document.Steps, output)
	}
	openKey, closeKey := concurrencyGateKeys(pipeline.CompilerStep, pipeline.ConcurrencyGate.Group, pipeline.Jobs)
	open, producer, consumer, close := document.Steps[0], document.Steps[1], document.Steps[2], document.Steps[3]
	if open.Key != openKey || open.Concurrency != 1 || open.ConcurrencyGroup != pipeline.ConcurrencyGate.Group || len(open.DependsOn) != 1 || open.DependsOn[0].Step != "importer" || open.DependsOn[0].AllowFailure {
		t.Fatalf("opening gate = %#v", open)
	}
	for _, job := range []struct {
		step struct {
			Label            string `yaml:"label"`
			Key              string `yaml:"key"`
			Concurrency      int    `yaml:"concurrency"`
			ConcurrencyGroup string `yaml:"concurrency_group"`
			DependsOn        []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		}
		logicalDependency string
	}{{producer, ""}, {consumer, "producer"}} {
		if len(job.step.DependsOn) < 2 || job.step.DependsOn[1].Step != openKey || job.step.DependsOn[1].AllowFailure {
			t.Fatalf("job %q does not strictly depend on opening gate: %#v", job.step.Key, job.step.DependsOn)
		}
		if job.logicalDependency != "" && (len(job.step.DependsOn) != 3 || job.step.DependsOn[2].Step != job.logicalDependency || !job.step.DependsOn[2].AllowFailure) {
			t.Fatalf("job %q logical dependencies = %#v", job.step.Key, job.step.DependsOn)
		}
	}
	if close.Key != closeKey || close.Concurrency != 1 || close.ConcurrencyGroup != pipeline.ConcurrencyGate.Group || len(close.DependsOn) != 3 {
		t.Fatalf("closing gate = %#v", close)
	}
	for _, dependency := range close.DependsOn[1:] {
		if !dependency.AllowFailure {
			t.Fatalf("closing gate has strict generated dependency: %#v", close.DependsOn)
		}
	}
}

func TestEmitUsesDefaultAgentTargetingWhenQueueIsEmpty(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		ConcurrencyGate:    &ConcurrencyGate{Group: "buildkite-gha/concurrency/group"},
		Jobs:               []Job{{Key: "job", Label: "Job", PlanDigest: testDigest("plan")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "agents:") || strings.Contains(string(output), "queue:") {
		t.Fatalf("default-targeted pipeline contains agent selectors:\n%s", output)
	}
}

func TestConcurrencyGateKeysIncludeCompilerStep(t *testing.T) {
	jobs := []Job{{Key: "job"}}
	firstOpen, firstClose := concurrencyGateKeys("first-importer", "shared-group", jobs)
	secondOpen, secondClose := concurrencyGateKeys("second-importer", "shared-group", jobs)
	if firstOpen == secondOpen || firstClose == secondClose {
		t.Fatalf("same-group imports have colliding gate keys: %q, %q and %q, %q", firstOpen, firstClose, secondOpen, secondClose)
	}
}

func TestEmitActionRuntimeRequirement(t *testing.T) {
	pipeline := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Jobs:               []Job{{Key: "action", Label: "Action", Queue: "hosted", PlanDigest: testDigest("plan"), RequiresMise: true}},
	}
	output, err := Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Command string `yaml:"command"`
			Cache   struct {
				Paths []string `yaml:"paths"`
				Name  string   `yaml:"name"`
			} `yaml:"cache"`
			Env map[string]string `yaml:"env"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatalf("parse emitted YAML: %v", err)
	}
	if len(document.Steps) != 1 {
		t.Fatalf("emitted steps = %#v", document.Steps)
	}
	command := document.Steps[0].Command
	if strings.Contains(command, "/tools/mise/") || strings.Contains(command, "mise_archive") || strings.Contains(command, `export PATH="$bootstrap_dir:$PATH"`) {
		t.Fatalf("generated action job still transports mise:\n%s", command)
	}
	step := document.Steps[0]
	if step.Cache.Name != runtimeCacheName+"-linux-amd64" || len(step.Cache.Paths) != 1 || step.Cache.Paths[0] != platformMiseCachePath("linux/amd64") {
		t.Fatalf("mise cache volume = %#v", step.Cache)
	}
	if step.Env["BUILDKITE_GHA_MISE_DATA_DIR"] != MiseDataDir() {
		t.Fatalf("mise data directory = %q", step.Env["BUILDKITE_GHA_MISE_DATA_DIR"])
	}
}

func TestEmitDarwinActionRuntimeUsesNativePlatformCache(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep: "importer",
		Jobs: []Job{{
			Key:                "macos",
			Label:              "macOS",
			Queue:              "macos",
			Platform:           "darwin/arm64",
			DistributionDigest: testDigest("darwin distribution"),
			PlanDigest:         testDigest("darwin plan"),
			RequiresMise:       true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Image   string `yaml:"image"`
			Command string `yaml:"command"`
			Cache   struct {
				Paths []string `yaml:"paths"`
				Name  string   `yaml:"name"`
			} `yaml:"cache"`
			Env map[string]string `yaml:"env"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 {
		t.Fatalf("steps = %#v", document.Steps)
	}
	step := document.Steps[0]
	if step.Image != "" || step.Cache.Name != "buildkite-gha-darwin-arm64" || !slices.Equal(step.Cache.Paths, []string{"/tmp/bkcache/buildkite-gha/mise/darwin-arm64"}) {
		t.Fatalf("Darwin runtime placement = %#v", step)
	}
	if step.Env["BUILDKITE_GHA_MISE_DATA_DIR"] != MiseDataDir("darwin/arm64") {
		t.Fatalf("Darwin mise data directory = %q", step.Env["BUILDKITE_GHA_MISE_DATA_DIR"])
	}
	if !strings.Contains(step.Command, `shasum -a 256 "$distribution"`) || strings.Contains(step.Command, "--hosted-tool-cache") || strings.Contains(step.Command, HostedToolCachePath) {
		t.Fatalf("Darwin bootstrap is not native and portable:\n%s", step.Command)
	}
}

func TestEmitUsesImmutableRuntimeImageToolCache(t *testing.T) {
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		RuntimeImage:       image,
		Jobs:               []Job{{Key: "job", Label: "Job", Queue: "hosted", PlanDigest: testDigest("plan")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Image   string `yaml:"image"`
			Command string `yaml:"command"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 || document.Steps[0].Image != image {
		t.Fatalf("runtime image = %#v, want %q", document.Steps, image)
	}
	if !strings.Contains(document.Steps[0].Command, "--hosted-tool-cache") {
		t.Fatalf("run-job command does not select hosted tool cache: %q", document.Steps[0].Command)
	}
}

func TestMiseDataDirUsesManagedRuntimeVersion(t *testing.T) {
	if got := MiseDataDir(); got != "/cache/bkcache/buildkite-gha/mise/linux-amd64/"+MinimumMiseVersion {
		t.Fatalf("MiseDataDir() = %q", got)
	}
	if got := MiseDataDir("darwin/arm64"); got != "/tmp/bkcache/buildkite-gha/mise/darwin-arm64/"+MinimumMiseVersion {
		t.Fatalf("MiseDataDir(darwin/arm64) = %q", got)
	}
}

func TestEmitMiseCacheOnlyForActionJobs(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Jobs: []Job{
			{Key: "javascript", Label: "JavaScript", Queue: "hosted", PlanDigest: testDigest("javascript-plan"), RequiresMise: true},
			{Key: "native", Label: "Native action", Queue: "hosted", PlanDigest: testDigest("native-plan")},
			{Key: "shell", Label: "Shell", Queue: "hosted", PlanDigest: testDigest("shell-plan")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Key     string            `yaml:"key"`
			Command string            `yaml:"command"`
			Cache   any               `yaml:"cache"`
			Env     map[string]string `yaml:"env"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	for _, step := range document.Steps {
		if step.Key == "javascript" && (step.Cache == nil || step.Env["BUILDKITE_GHA_MISE_DATA_DIR"] != MiseDataDir() || strings.Contains(step.Command, "/tools/mise/")) {
			t.Fatalf("JavaScript job lacks runtime mise cache configuration: %#v", step)
		}
		if (step.Key == "native" || step.Key == "shell") && (step.Cache != nil || step.Env["BUILDKITE_GHA_MISE_DATA_DIR"] != "" || strings.Contains(step.Command, "/tools/mise/")) {
			t.Fatalf("no-mise job gained managed mise: %#v", step)
		}
	}
}

func TestEmitRejectsInvalidGraphsAndIdentifiers(t *testing.T) {
	digest := testDigest("plan")
	tests := []struct {
		name string
		in   Pipeline
		want string
	}{
		{name: "empty", in: Pipeline{CompilerStep: "compiler", DistributionDigest: digest}, want: "at least one generated job"},
		{name: "bad distribution digest", in: Pipeline{CompilerStep: "compiler", DistributionDigest: "sha256:nope", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest}}}, want: "invalid distribution digest"},
		{name: "unknown dependency", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest, Dependencies: []string{"missing"}}}}, want: "unknown dependency"},
		{name: "cycle", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: testDigest("one"), Dependencies: []string{"two"}}, {Key: "two", Label: "Two", Queue: "queue", PlanDigest: testDigest("two"), Dependencies: []string{"one"}}}}, want: "contains a cycle"},
		{name: "duplicate key", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: testDigest("one")}, {Key: "one", Label: "Other", Queue: "queue", PlanDigest: testDigest("other")}}}, want: "duplicate generated step key"},
		{name: "duplicate digest", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest}, {Key: "two", Label: "Two", Queue: "queue", PlanDigest: digest}}}, want: "share plan digest"},
		{name: "duplicate dependency", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: testDigest("one")}, {Key: "two", Label: "Two", Queue: "queue", PlanDigest: testDigest("two"), Dependencies: []string{"one", "one"}}}}, want: "repeats dependency"},
		{name: "self dependency", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest, Dependencies: []string{"one"}}}}, want: "invalid dependency"},
		{name: "compiler dependency", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest, Dependencies: []string{"compiler"}}}}, want: "invalid dependency"},
		{name: "compiler collision", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "compiler", Label: "One", Queue: "queue", PlanDigest: digest}}}, want: "invalid generated step key"},
		{name: "UUID compiler", in: Pipeline{CompilerStep: "123e4567-e89b-12d3-a456-426614174000", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest}}}, want: "invalid compiler step key"},
		{name: "UUID key", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "123e4567-e89b-12d3-a456-426614174000", Label: "One", Queue: "queue", PlanDigest: digest}}}, want: "invalid generated step key"},
		{name: "bad digest", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: "sha256:nope"}}}, want: "invalid plan digest"},
		{name: "mutable runtime image", in: Pipeline{CompilerStep: "compiler", DistributionDigest: digest, RuntimeImage: "buildkite/agent-base:ubuntu-jammy-hosted-toolchains", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest}}}, want: "immutable registry sha256 reference"},
		{name: "partial concurrency", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest, Concurrency: 2}}}, want: "concurrency and concurrency group together"},
		{name: "empty workflow concurrency group", in: Pipeline{CompilerStep: "compiler", DistributionDigest: digest, ConcurrencyGate: &ConcurrencyGate{Queue: "queue"}, Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest}}}, want: "invalid workflow concurrency group"},
		{name: "long job concurrency group", in: Pipeline{CompilerStep: "compiler", DistributionDigest: digest, Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest, Concurrency: 1, ConcurrencyGroup: strings.Repeat("x", maxConcurrencyGroupLength+1)}}}, want: "concurrency group exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Emit(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Emit() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPlanPathIsContentAddressed(t *testing.T) {
	digest := testDigest("plan")
	path, err := PlanPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	if path != ".buildkite-gha/plans/64879f7d6b960a01909762d911a32d4582c20010c5641ee90278b644a9e3b525.json" {
		t.Fatalf("PlanPath() = %q", path)
	}
}

func testDigest(contents string) string {
	return transport.Digest([]byte(contents))
}
