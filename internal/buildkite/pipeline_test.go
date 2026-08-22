package buildkite

import (
	"bytes"
	"os"
	"os/exec"
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
			!strings.Contains(step.Command, "echo '~~~ :package: Prepare GitHub Actions runtime'\n") ||
			!strings.Contains(step.Command, `bootstrap_dir="$(mktemp -d `) ||
			!strings.Contains(step.Command, `artifact download '.buildkite-gha/distributions/`) ||
			!strings.Contains(step.Command, `artifact download '.buildkite-gha/plans/`) ||
			!strings.Contains(step.Command, `sha256sum "$distribution"`) ||
			!strings.Contains(step.Command, `sha256sum "$plan"`) ||
			!strings.Contains(step.Command, `sudo -n --preserve-env --user runner`) ||
			!strings.Contains(step.Command, `run-job --plan "$plan"`) {
			t.Fatalf("step %q does not verify its artifacts before runner execution:\n%s", step.Key, step.Command)
		}
	}
	if !strings.Contains(string(first), `artifact download '.buildkite-gha/distributions/`) ||
		!strings.Contains(string(first), `artifact download '.buildkite-gha/plans/`) ||
		strings.Contains(string(first), `.buildkite-gha/bootstrap/`) ||
		strings.Contains(string(first), "go run") ||
		strings.Contains(string(first), "cache:") ||
		strings.Contains(string(first), "BUILDKITE_GHA_MISE_DATA_DIR") {
		t.Fatalf("generated jobs are not self-contained:\n%s", first)
	}
	if strings.Count(string(first), `--step 'gha-importer'`) != 6 {
		t.Fatalf("generated artifact downloads are not constrained to the exact importer:\n%s", first)
	}
	if !strings.Contains(string(first), `Consumer ($VALUE, variant=\"two\")`) {
		t.Fatal("runtime dollar sign or quoted label did not survive scalar encoding")
	}
}

func TestEmitScopesBootstrapFailureExpansionBeforeRunJob(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Jobs:               []Job{{Key: "job", Label: "Job", PlanDigest: testDigest("plan")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Command string `yaml:"command"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 1 {
		t.Fatalf("steps = %#v", document.Steps)
	}
	command := document.Steps[0].Command
	group := "echo '~~~ :package: Prepare GitHub Actions runtime'"
	setTrap := "trap bootstrap_exit EXIT"
	clearTrap := `trap 'rm -rf -- "$bootstrap_dir"' EXIT`
	runJob := `"$distribution" run-job`
	if strings.Count(command, group) != 1 || strings.Count(command, setTrap) != 1 || strings.Count(command, clearTrap) != 1 ||
		strings.Index(command, group) > strings.Index(command, setTrap) ||
		strings.Index(command, setTrap) > strings.Index(command, `artifact download `) ||
		strings.Index(command, clearTrap) < strings.Index(command, `chmod 0500 "$distribution"`) ||
		strings.Index(command, clearTrap) > strings.Index(command, runJob) {
		t.Fatalf("bootstrap group or trap is incorrectly scoped:\n%s", command)
	}

	distributionPath, err := DistributionPath(testDigest("distribution"))
	if err != nil {
		t.Fatal(err)
	}
	bootstrapFailure := strings.Split(command, "\n")
	for i, line := range bootstrapFailure {
		if strings.Contains(line, "buildkite-agent artifact download") {
			mkdir, err := exec.LookPath("mkdir")
			if err != nil {
				t.Fatal(err)
			}
			bootstrapFailure[i] = shellQuote(mkdir) + ` -p "$bootstrap_dir/` + filepath.Dir(distributionPath) + `"` + "\n" + `: > "$bootstrap_dir/` + distributionPath + `"`
		}
	}
	path := t.TempDir()
	for _, tool := range []string{"mktemp", "rm"} {
		target, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(path, tool)); err != nil {
			t.Fatal(err)
		}
	}
	bootstrapCommand := exec.Command("bash", "-c", strings.Join(bootstrapFailure, "\n"))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PATH=") {
			bootstrapCommand.Env = append(bootstrapCommand.Env, value)
		}
	}
	bootstrapCommand.Env = append(bootstrapCommand.Env, "PATH="+path)
	bootstrapOutput, bootstrapErr := bootstrapCommand.CombinedOutput()
	if bootstrapErr == nil || !strings.Contains(string(bootstrapOutput), "buildkite-gha: no SHA-256 tool available\n") || !strings.Contains(string(bootstrapOutput), "^^^ +++\n") {
		t.Fatalf("explicit no-SHA bootstrap failure did not expand its collapsed group: err=%v output=%q", bootstrapErr, bootstrapOutput)
	}

	bootstrapExit := `bootstrap_exit() { bootstrap_status=$?; if [ "$bootstrap_status" -ne 0 ]; then echo "^^^ +++"; fi; exit "$bootstrap_status"; }`
	runtimeFailure := strings.Join([]string{"set -e", group, bootstrapExit, setTrap, clearTrap, "unset -f bootstrap_exit", "echo '--- Run action'", "false"}, "\n")
	runtimeOutput, runtimeErr := exec.Command("bash", "-c", runtimeFailure).CombinedOutput()
	if runtimeErr == nil || strings.Contains(string(runtimeOutput), "^^^ +++") || !strings.Contains(string(runtimeOutput), "--- Run action\n") {
		t.Fatalf("runtime failure was attributed to bootstrap: err=%v output=%q", runtimeErr, runtimeOutput)
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
		EventProvider:      "github",
		Workflows: []Workflow{
			{
				GroupLabel: "CI", GroupKey: "gha-workflow-1111111111111111", Event: "push", Condition: `build.env("BUILDKITE_GITHUB_EVENT") == "push"`,
				Jobs: []Job{{Key: "gha-1111111111111111-test", Label: "Test", PlanDigest: testDigest("first plan")}},
			},
			{
				GroupLabel: ".github/workflows/release.yml", GroupKey: "gha-workflow-2222222222222222", Event: "workflow_dispatch", Condition: `build.env("BUILDKITE_GITHUB_EVENT") == "workflow_dispatch"`,
				Jobs: []Job{{Key: "gha-2222222222222222-test", Label: "Test \"quoted\"\nnext", PlanDigest: testDigest("second plan")}},
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
			Notify    any    `yaml:"notify"`
			Steps     []struct {
				Key     string `yaml:"key"`
				Command string `yaml:"command"`
				Notify  []struct {
					GitHubCheck struct {
						Name string `yaml:"name"`
					} `yaml:"github_check"`
				} `yaml:"notify"`
				DependsOn *yaml.Node `yaml:"depends_on"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 2 || document.Steps[0].Group != ":github: workflow · CI" || document.Steps[0].Key != "gha-workflow-1111111111111111" || document.Steps[0].Condition != `build.env("BUILDKITE_GITHUB_EVENT") == "push"` || document.Steps[0].DependsOn != "importer" || document.Steps[0].Notify != nil || len(document.Steps[0].Steps) != 1 || document.Steps[0].Steps[0].Key != "gha-1111111111111111-test" || len(document.Steps[0].Steps[0].Notify) != 1 || document.Steps[0].Steps[0].Notify[0].GitHubCheck.Name != "CI / Test (push)" {
		t.Fatalf("first aggregate group = %#v\n%s", document.Steps, output)
	}
	if document.Steps[1].Group != ":github: workflow · .github/workflows/release.yml" || document.Steps[1].Key != "gha-workflow-2222222222222222" || document.Steps[1].DependsOn != "importer" || document.Steps[1].Notify != nil || len(document.Steps[1].Steps) != 1 || document.Steps[1].Steps[0].Key != "gha-2222222222222222-test" || len(document.Steps[1].Steps[0].Notify) != 1 || document.Steps[1].Steps[0].Notify[0].GitHubCheck.Name != ".github/workflows/release.yml / Test \"quoted\"\nnext (workflow_dispatch)" {
		t.Fatalf("second aggregate group = %#v\n%s", document.Steps[1], output)
	}
	for _, group := range document.Steps {
		for _, step := range group.Steps {
			if step.DependsOn != nil {
				t.Fatalf("dependency-free aggregate child %q emitted depends_on: %#v", step.Key, step.DependsOn)
			}
			if strings.Count(step.Command, `--step 'importer'`) != 2 || !strings.Contains(step.Command, `run-job --plan "$plan"`) || !strings.Contains(step.Command, `sudo -n --preserve-env --user runner`) || !strings.Contains(step.Command, `artifact download '.buildkite-gha/plans/`) {
				t.Fatalf("aggregate child %q does not prepare runner execution from importer artifacts: %q", step.Key, step.Command)
			}
		}
	}
	if !strings.Contains(string(output), `name: ".github/workflows/release.yml / Test \"quoted\"\nnext (workflow_dispatch)"`) {
		t.Fatalf("GitHub Check name did not use YAML scalar escaping:\n%s", output)
	}
}

func TestEmitAggregateOriginWorkflowChecks(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		EventProvider:      "cursor-origin",
		Workflows: []Workflow{
			{
				GroupLabel: "CI", GroupKey: "gha-workflow-1111111111111111", Event: "push", Condition: "true",
				Jobs: []Job{{Key: "gha-1111111111111111-test", Label: "Test", PlanDigest: testDigest("plan")}},
			},
			{
				GroupLabel: "Pull request", GroupKey: "gha-workflow-2222222222222222", Event: "push",
				SkipReason: "This workflow is not triggered by a `push` event",
			},
			{
				GroupLabel: "Invalid", GroupKey: "gha-workflow-3333333333333333", Event: "push", Condition: "true",
				Failure: &Failure{
					AnnotationPath: ".buildkite-gha/failures/annotations/annotation.html",
					MessagePath:    ".buildkite-gha/failures/messages/message.txt",
					Summary:        "The workflow could not be prepared.",
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Key    string `yaml:"key"`
			Notify []struct {
				GitHubCheck any `yaml:"github_check"`
				OriginCheck struct {
					Key    string `yaml:"key"`
					Name   string `yaml:"name"`
					Output struct {
						Title   string `yaml:"title"`
						Summary string `yaml:"summary"`
					} `yaml:"output"`
				} `yaml:"origin_check"`
			} `yaml:"notify"`
			Steps []struct {
				Key    string `yaml:"key"`
				Notify []struct {
					GitHubCheck any `yaml:"github_check"`
					OriginCheck struct {
						Key  string `yaml:"key"`
						Name string `yaml:"name"`
					} `yaml:"origin_check"`
				} `yaml:"notify"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 3 {
		t.Fatalf("Origin workflow steps = %#v\n%s", document.Steps, output)
	}
	runnable := document.Steps[0]
	if runnable.Notify != nil || len(runnable.Steps) != 1 || len(runnable.Steps[0].Notify) != 1 || runnable.Steps[0].Notify[0].GitHubCheck != nil || runnable.Steps[0].Notify[0].OriginCheck.Key != runnable.Steps[0].Key || runnable.Steps[0].Notify[0].OriginCheck.Name != "CI / Test (push)" {
		t.Fatalf("runnable Origin workflow check = %#v", runnable)
	}
	wantNames := []string{"Pull request (push)", "Invalid (push)"}
	for i, step := range document.Steps[1:] {
		if len(step.Notify) != 1 || step.Notify[0].GitHubCheck != nil || step.Notify[0].OriginCheck.Key != step.Key || step.Notify[0].OriginCheck.Name != wantNames[i] {
			t.Fatalf("Origin workflow check %d = %#v", i, step)
		}
	}
	failure := document.Steps[2].Notify[0].OriginCheck.Output
	if failure.Title != "Workflow could not be run" || failure.Summary != "The workflow could not be prepared." {
		t.Fatalf("Origin failure output = %#v", failure)
	}
}

func TestEmitAggregateWorkflowChecksRequireUniqueJobIdentities(t *testing.T) {
	pipeline := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		EventProvider:      "github",
		Workflows: []Workflow{{
			GroupLabel: "Bun",
			GroupKey:   "gha-workflow-1111111111111111",
			Event:      "push",
			Condition:  "true",
			Jobs: []Job{
				{Key: "first", Label: "Test", CheckLabel: "first", PlanDigest: testDigest("first")},
				{Key: "second", Label: "Test", CheckLabel: "second", PlanDigest: testDigest("second")},
			},
		}},
	}
	output, err := Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Steps []struct {
				Notify []struct {
					GitHubCheck struct {
						Name string `yaml:"name"`
					} `yaml:"github_check"`
				} `yaml:"notify"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	steps := document.Steps[0].Steps
	if len(steps) != 2 || steps[0].Notify[0].GitHubCheck.Name != "Bun / first (push)" || steps[1].Notify[0].GitHubCheck.Name != "Bun / second (push)" {
		t.Fatalf("job check names = %#v\n%s", steps, output)
	}

	pipeline.Workflows[0].Jobs[0].CheckLabel = ""
	pipeline.Workflows[0].Jobs[1].CheckLabel = ""
	if _, err := Emit(pipeline); err == nil || !strings.Contains(err.Error(), `share provider check label "Test"`) {
		t.Fatalf("duplicate provider check label error = %v", err)
	}
}

func TestEmitAggregateSkippedWorkflowStep(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:  "importer",
		EventProvider: "github",
		Workflows: []Workflow{{
			GroupLabel: "Pull request",
			GroupKey:   "gha-workflow-1111111111111111",
			Event:      "push",
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
	if step.Group != "" || step.Label != ":github: workflow · Pull request" || step.Key != "gha-workflow-1111111111111111" || step.Type != "command" || step.Condition != "" || step.Skip != "This workflow is not triggered by a `push` event" || step.Command != "" || step.DependsOn != "importer" || len(step.Notify) != 1 || step.Notify[0].GitHubCheck.Name != "Pull request (push)" || len(step.Steps) != 0 || !step.Checkout.Skip {
		t.Fatalf("skipped aggregate step = %#v\n%s", step, output)
	}
}

func TestEmitAggregateWorkflowFailures(t *testing.T) {
	output, err := Emit(Pipeline{
		CompilerStep:  "importer",
		EventProvider: "github",
		Workflows: []Workflow{{
			GroupLabel: "CI",
			GroupKey:   "gha-workflow-1111111111111111",
			Event:      "push",
			Failure: &Failure{
				AnnotationPath: ".buildkite-gha/failures/annotations/annotation.html",
				MessagePath:    ".buildkite-gha/failures/messages/message.txt",
				Summary:        "The workflow could not be prepared:\n\n- `ci.yml`, job `test`: runner isn't admitted\n- `ci.yml`: matrix could not be expanded",
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
			Plugins   []map[string]struct {
				Step     string `yaml:"step"`
				Download []struct {
					From string `yaml:"from"`
					To   string `yaml:"to"`
				} `yaml:"download"`
			} `yaml:"plugins"`
			DependsOn string `yaml:"depends_on"`
			Retry     struct {
				Manual *struct {
					Allowed bool `yaml:"allowed"`
				} `yaml:"manual"`
			} `yaml:"retry"`
			Notify []struct {
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
	if len(step.Plugins) != 1 {
		t.Fatalf("failure step plugins = %#v", step.Plugins)
	}
	plugin, ok := step.Plugins[0]["artifacts#v1.9.4"]
	if !ok {
		t.Fatalf("failure step plugins = %#v", step.Plugins)
	}
	if step.Group != "" || len(step.Steps) != 0 || step.Label != ":github: workflow · CI" || step.Key != "gha-workflow-1111111111111111" || step.Condition != "" || step.Skip != "" || plugin.Step != "importer" || len(plugin.Download) != 2 || plugin.Download[0].From != ".buildkite-gha/failures/messages/message.txt" || plugin.Download[0].To != ".buildkite-gha-failure-message.txt" || plugin.Download[1].From != ".buildkite-gha/failures/annotations/annotation.html" || plugin.Download[1].To != ".buildkite-gha-failure-annotation.html" || step.DependsOn != "importer" || step.Retry.Manual == nil || step.Retry.Manual.Allowed || len(step.Notify) != 1 || step.Notify[0].GitHubCheck.Name != "CI (push)" || step.Notify[0].GitHubCheck.Output.Title != "Workflow could not be run" || step.Notify[0].GitHubCheck.Output.Summary != "The workflow could not be prepared:\n\n- `ci.yml`, job `test`: runner isn't admitted\n- `ci.yml`: matrix could not be expanded" || step.Command != `cat .buildkite-gha-failure-message.txt
buildkite-agent annotate --scope=job --style=error < .buildkite-gha-failure-annotation.html
exit 1` || !step.Checkout.Skip {
		t.Fatalf("failure step = %#v", step)
	}
}

func TestEmitAggregateWorkflowConcurrencyDependencies(t *testing.T) {
	pipeline := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		EventProvider:      "github",
		Workflows: []Workflow{{
			GroupLabel:      "CI",
			GroupKey:        "workflow-ci",
			Event:           "push",
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
	open, close, producer, consumer := document.Steps[0].Steps[0], document.Steps[0].Steps[1], document.Steps[0].Steps[2], document.Steps[0].Steps[3]
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

func TestEmitAggregateRequiresEventName(t *testing.T) {
	_, err := Emit(Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		EventProvider:      "github",
		Workflows: []Workflow{{
			GroupLabel: "CI", GroupKey: "workflow-ci", Condition: "true",
			Jobs: []Job{{Key: "test", Label: "Test", PlanDigest: testDigest("plan")}},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), `workflow "workflow-ci" requires an event name`) {
		t.Fatalf("missing event name error = %v", err)
	}
}

func TestEmitAggregateRejectsCrossWorkflowCollisions(t *testing.T) {
	base := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		EventProvider:      "github",
		Workflows: []Workflow{
			{GroupLabel: "One", GroupKey: "workflow-one", Event: "push", Condition: "true", Jobs: []Job{{Key: "shared", Label: "One", PlanDigest: testDigest("one")}}},
			{GroupLabel: "Two", GroupKey: "workflow-two", Event: "push", Condition: "true", Jobs: []Job{{Key: "shared", Label: "Two", PlanDigest: testDigest("two")}}},
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
	open, close, producer, consumer := document.Steps[0], document.Steps[1], document.Steps[2], document.Steps[3]
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
	if outerOpen.ConcurrencyGroup != outer.Group || outerOpen.Agents["queue"] != "linux" || len(outerOpen.DependsOn) != 2 || outerOpen.DependsOn[1].Step != "prepare" || !outerOpen.DependsOn[1].AllowFailure {
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
		{name: "unknown event provider", in: Pipeline{CompilerStep: "compiler", EventProvider: "gitlab", Workflows: []Workflow{{}}}, want: "unsupported event provider"},
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
