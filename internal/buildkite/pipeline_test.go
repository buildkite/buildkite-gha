package buildkite

import (
	"bytes"
	"os"
	"path/filepath"
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
		if !strings.Contains(step.Command, `bootstrap_dir="$(mktemp -d "${TMPDIR:-/tmp}/buildkite-gha.XXXXXXXX")"`) ||
			!strings.Contains(step.Command, `sha256sum "$distribution"`) ||
			!strings.Contains(step.Command, `test "$actual_distribution_digest" = `+shellQuote(pipeline.DistributionDigest)) ||
			!strings.Contains(step.Command, `"$distribution" run-job --plan "$plan"`) {
			t.Fatalf("step %q does not bootstrap and verify its exact distribution:\n%s", step.Key, step.Command)
		}
	}
	if !strings.Contains(string(first), `artifact download '.buildkite-gha/distributions/`) ||
		!strings.Contains(string(first), `artifact download '.buildkite-gha/plans/`) ||
		strings.Contains(string(first), "go run") {
		t.Fatalf("generated jobs are not self-contained:\n%s", first)
	}
	if strings.Count(string(first), `--step 'gha-importer'`) != 6 {
		t.Fatalf("generated artifact downloads are not constrained to the exact importer:\n%s", first)
	}
	if !strings.Contains(string(first), `Consumer ($VALUE, variant=\"two\")`) {
		t.Fatal("runtime dollar sign or quoted label did not survive scalar encoding")
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
		{name: "partial concurrency", in: Pipeline{CompilerStep: "compiler", Jobs: []Job{{Key: "one", Label: "One", Queue: "queue", PlanDigest: digest, Concurrency: 2}}}, want: "concurrency and concurrency group together"},
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

func TestDefaultPipelineRunsRepositoryChecks(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "pipeline.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Key     string `yaml:"key"`
			Command string `yaml:"command"`
			If      string `yaml:"if"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatalf("parse default pipeline: %v", err)
	}
	if len(document.Steps) != 4 {
		t.Fatalf("default pipeline = %#v, want three gated probe loaders and repository checks", document.Steps)
	}
	steps := make(map[string]struct {
		command   string
		condition string
	}, len(document.Steps))
	for _, step := range document.Steps {
		steps[step.Key] = struct {
			command   string
			condition string
		}{step.Command, step.If}
	}
	if got := steps["checks"]; got.command != "mise run --jobs 1 check" || got.condition != "" {
		t.Fatalf("repository checks = %#v", got)
	}
	if got := steps["phase-0-shell-oracle-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-0-shell-oracle.yml" || got.condition != `build.env("PHASE0_PROBE") == "shell"` {
		t.Fatalf("shell oracle loader = %#v", got)
	}
	if got := steps["phase-0-transport-probe-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-0-transport-probe/pipeline.yml" || got.condition != `build.env("PHASE0_PROBE") == "transport"` {
		t.Fatalf("transport probe loader = %#v", got)
	}
	if got := steps["phase-2-upload-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-2-upload.yml" || got.condition != `build.env("PHASE2_PROBE") == "upload"` {
		t.Fatalf("Phase 2 upload loader = %#v", got)
	}
}

func TestPhase2UploadProofUsesPinnedUnprivilegedPath(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "phase-2-upload.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`scripts/phase-0-shell-oracle-checkout "$$PHASE2_COMMIT"`,
		`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
		`mise exec -- go build -trimpath -buildvcs=false`,
		`--event-path testdata/smoke/events/push.json`,
		`--runtime-queue hosted`,
		`testdata/smoke/.github/workflows/shell.yml`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Phase 2 upload proof lacks %q:\n%s", required, source)
		}
	}
	var document struct {
		Steps []struct {
			Key       string `yaml:"key"`
			Command   string `yaml:"command"`
			DependsOn string `yaml:"depends_on"`
			Agents    struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 2 || document.Steps[0].Key != "phase-2-upload-importer" || document.Steps[0].Agents.Queue != "elastic-runners" {
		t.Fatalf("Phase 2 upload proof = %#v", document.Steps)
	}
	if document.Steps[1].Key != "phase-2-continuation-loader" || document.Steps[1].DependsOn != "phase-2-upload-importer" || document.Steps[1].Agents.Queue != "elastic-runners" || document.Steps[1].Command != "buildkite-agent pipeline upload .buildkite/phase-2-upload-continuation.yml" {
		t.Fatalf("Phase 2 continuation loader = %#v", document.Steps[1])
	}

	continuationSource, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "phase-2-upload-continuation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var continuation struct {
		Steps []struct {
			Key       string `yaml:"key"`
			DependsOn []struct {
				Step         string `yaml:"step"`
				AllowFailure bool   `yaml:"allow_failure"`
			} `yaml:"depends_on"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(continuationSource, &continuation); err != nil {
		t.Fatal(err)
	}
	if len(continuation.Steps) != 1 || continuation.Steps[0].Key != "phase-2-native-after-shell" {
		t.Fatalf("Phase 2 continuation = %#v", continuation.Steps)
	}
	wantDependencies := []string{"gha-consumer-5ebbc197d87b", "gha-consumer-91934b28b00f"}
	generatedSource, err := os.ReadFile(filepath.Join("..", "compiler", "testdata", "shell.pipeline.golden.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var generated struct {
		Steps []struct {
			Key string `yaml:"key"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(generatedSource, &generated); err != nil {
		t.Fatal(err)
	}
	generatedKeys := make(map[string]struct{}, len(generated.Steps))
	generatedConsumers := make(map[string]struct{})
	for _, step := range generated.Steps {
		generatedKeys[step.Key] = struct{}{}
		if strings.HasPrefix(step.Key, "gha-consumer-") {
			generatedConsumers[step.Key] = struct{}{}
		}
	}
	if len(continuation.Steps[0].DependsOn) != len(wantDependencies) {
		t.Fatalf("Phase 2 continuation dependencies = %#v", continuation.Steps[0].DependsOn)
	}
	for i, want := range wantDependencies {
		dependency := continuation.Steps[0].DependsOn[i]
		if dependency.Step != want || !dependency.AllowFailure {
			t.Fatalf("Phase 2 continuation dependency %d = %#v, want %q with allow_failure", i, dependency, want)
		}
		if _, ok := generatedKeys[dependency.Step]; !ok {
			t.Fatalf("Phase 2 continuation dependency %q is absent from shell.pipeline.golden.yml", dependency.Step)
		}
		delete(generatedConsumers, dependency.Step)
	}
	if len(generatedConsumers) != 0 {
		t.Fatalf("Phase 2 continuation omits generated consumers: %#v", generatedConsumers)
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
