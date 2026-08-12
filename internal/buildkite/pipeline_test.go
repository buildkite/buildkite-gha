package buildkite

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	if step.Image != "" || step.Cache.Name != "buildkite-gha-darwin-arm64" || !slices.Equal(step.Cache.Paths, []string{"/cache/bkcache/buildkite-gha/mise/darwin-arm64"}) {
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
	if got := MiseDataDir("darwin/arm64"); got != "/cache/bkcache/buildkite-gha/mise/darwin-arm64/"+MinimumMiseVersion {
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

func TestCheckedInPipelinesTargetHostedQueue(t *testing.T) {
	root := filepath.Join("..", "..", ".buildkite")
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || (filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var document struct {
			Agents struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
			Steps []struct {
				Agents struct {
					Queue string `yaml:"queue"`
				} `yaml:"agents"`
			} `yaml:"steps"`
		}
		if err := yaml.Unmarshal(body, &document); err != nil {
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		if document.Agents.Queue != "hosted" {
			t.Errorf("%s root queue = %q, want hosted", path, document.Agents.Queue)
		}
		if len(document.Steps) == 0 {
			t.Errorf("%s has no steps", path)
		}
		for index, step := range document.Steps {
			if step.Agents.Queue != "" && step.Agents.Queue != "hosted" {
				t.Errorf("%s step %d overrides queue with %q, want hosted", path, index, step.Agents.Queue)
			}
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no checked-in Buildkite pipelines found")
	}
}

func TestDefaultPipelineRunsRepositoryChecks(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "pipeline.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Env    map[string]string `yaml:"env"`
		Agents struct {
			Queue string `yaml:"queue"`
		} `yaml:"agents"`
		Steps []struct {
			Key     string            `yaml:"key"`
			Command string            `yaml:"command"`
			If      string            `yaml:"if"`
			Env     map[string]string `yaml:"env"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatalf("parse default pipeline: %v", err)
	}
	if document.Env["MISE_JOBS"] != "1" {
		t.Fatalf("default pipeline MISE_JOBS = %q, want serial tool installation", document.Env["MISE_JOBS"])
	}
	if document.Agents.Queue != "hosted" {
		t.Fatalf("default pipeline queue = %q, want hosted", document.Agents.Queue)
	}
	steps := make(map[string]struct {
		command     string
		condition   string
		environment map[string]string
	}, len(document.Steps))
	for _, step := range document.Steps {
		if step.Key != "" {
			if _, exists := steps[step.Key]; exists {
				t.Fatalf("default pipeline repeats step key %q", step.Key)
			}
		}
		steps[step.Key] = struct {
			command     string
			condition   string
			environment map[string]string
		}{command: step.Command, condition: step.If, environment: step.Env}
	}
	if got := steps["checks"]; got.command != "mise run --jobs 1 check" || got.condition != "" {
		t.Fatalf("repository checks = %#v", got)
	}
	if got := steps["checks"].environment["BUILDKITE_GHA_LIVE_REQUIRED"]; got != "1" {
		t.Fatalf("repository checks BUILDKITE_GHA_LIVE_REQUIRED = %q, want required live prerequisites", got)
	}
	pluginDemo := steps["plugin-demo-loader"]
	if pluginDemo.condition != `build.env("DEMO_SUITE") == "plugin"` {
		t.Fatalf("released plugin demo loader = %#v", pluginDemo)
	}
	for _, required := range []string{
		`commit="$${DEMO_COMMIT:?DEMO_COMMIT is required}"`,
		`[[ "$$commit" =~ ^[0-9a-f]{40}$$ ]]`,
		`scripts/verify-source-checkout "$$commit"`,
		`test "$${BUILDKITE_COMMIT:?BUILDKITE_COMMIT is required}" = "$$commit"`,
		`git status --porcelain --untracked-files=all`,
		`buildkite-agent pipeline upload .buildkite/plugin-demo.yml`,
	} {
		if !strings.Contains(pluginDemo.command, required) {
			t.Fatalf("released plugin demo loader lacks %q:\n%s", required, pluginDemo.command)
		}
	}
	if strings.Contains(pluginDemo.command, "BUILDKITE_GHA_CACHE_URL") {
		t.Fatalf("released plugin demo loader still requires a cache Results URL:\n%s", pluginDemo.command)
	}
	if got := steps["plugin-source-smoke-importer"]; got.condition != `build.env("DEMO_SUITE") != "plugin"` || got.command != "mise exec -- go run ./cmd/buildkite-gha plugin" || got.environment["BUILDKITE_PLUGIN_CONFIGURATION"] != `{"workflow":".github/workflows/example-basic.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted"}]}` {
		t.Fatalf("exact-source plugin smoke = %#v", got)
	}
	if got := steps["publish-release"]; got.command != "mise exec -- scripts/ci-buildkite-release" || got.condition != "build.tag != null" {
		t.Fatalf("release publisher = %#v", got)
	}
	if got := steps["shell-upload-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/shell-upload-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "shell-upload" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Shell upload upload loader = %#v", got)
	}
	if got := steps["concurrent-steps-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/concurrent-steps-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "concurrent-steps" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Concurrent steps upload loader = %#v", got)
	}
	if got := steps["public-actions-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/public-actions-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "public-actions" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Public actions upload loader = %#v", got)
	}
	if got := steps["hosted-docker-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/hosted-docker-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "hosted-docker" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Hosted Docker loader = %#v", got)
	}
	if got := steps["dockerfile-action-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/dockerfile-action-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "dockerfile-action" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Dockerfile action loader = %#v", got)
	}
	if got := steps["container-runtime-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/container-runtime-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "container-runtime" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Container runtime loader = %#v", got)
	}
	if got := steps["summary-annotation-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/summary-annotation-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "summary-annotation" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Summary annotation annotation loader = %#v", got)
	}
	if got := steps["workflow-annotations-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/workflow-annotations-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "workflow-annotations" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Workflow command annotation loader = %#v", got)
	}
	if got := steps["upload-artifact-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/upload-artifact-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "upload-artifact" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Upload-artifact loader = %#v", got)
	}
	if got := steps["artifact-roundtrip-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/artifact-roundtrip-proof.yml" || got.condition != `build.env("COMPATIBILITY_PROOF") == "artifact-roundtrip" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Artifact roundtrip loader = %#v", got)
	}
	hosted := steps["hosted-smoke-loader"]
	if hosted.condition != `build.env("SMOKE_PROBE") == "hosted"` {
		t.Fatalf("hosted smoke loader = %#v", hosted)
	}
	for _, required := range []string{
		`commit="$${SMOKE_COMMIT:?SMOKE_COMMIT is required}"`,
		`[[ "$$commit" =~ ^[0-9a-f]{40}$$ ]]`,
		`proof_commit="$${COMPATIBILITY_PROOF_COMMIT:-}"`,
		`[[ -z "$$proof_commit" || "$$proof_commit" == "$$commit" ]]`,
		`COMPATIBILITY_PROOF_COMMIT $$proof_commit conflicts with SMOKE_COMMIT`,
		`scripts/verify-source-checkout "$$commit"`,
		`test "$${BUILDKITE_COMMIT:?BUILDKITE_COMMIT is required}" = "$$commit"`,
		`git status --porcelain --untracked-files=all`,
	} {
		if !strings.Contains(hosted.command, required) {
			t.Fatalf("hosted smoke loader lacks %q:\n%s", required, hosted.command)
		}
	}
	for _, fragment := range []string{"shell-upload-proof.yml", "concurrent-steps-proof.yml", "public-actions-proof.yml", "hosted-docker-proof.yml", "dockerfile-action-proof.yml", "container-runtime-proof.yml", "summary-annotation-proof.yml", "workflow-annotations-proof.yml", "upload-artifact-proof.yml", "artifact-roundtrip-proof.yml", "hosted-smoke-control.yml"} {
		if count := strings.Count(hosted.command, "buildkite-agent pipeline upload .buildkite/"+fragment); count != 1 {
			t.Fatalf("hosted smoke loader uploads %s %d times:\n%s", fragment, count, hosted.command)
		}
	}
	if strings.Contains(hosted.command, "cache-roundtrip") {
		t.Fatalf("hosted smoke loader includes the retired targeted cache proof:\n%s", hosted.command)
	}
	if strings.Contains(hosted.command, "--replace") {
		t.Fatalf("hosted smoke loader uses replacement upload:\n%s", hosted.command)
	}
}

func TestRepositoryHostedImportersUseExplicitRunnerProfiles(t *testing.T) {
	for _, path := range []string{
		"shell-upload-proof.yml",
		"concurrent-steps-proof.yml",
		"public-actions-proof.yml",
		"dockerfile-action-proof.yml",
		"summary-annotation-proof.yml",
		"workflow-annotations-proof.yml",
		"upload-artifact-proof.yml",
		"artifact-roundtrip-proof.yml",
	} {
		t.Run(path, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", path))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(source, []byte("--runner-queue ubuntu-latest=hosted")) || bytes.Contains(source, []byte("BUILDKITE_GHA_TARGET_QUEUE")) {
				t.Fatalf("%s does not use the explicit runner profile:\n%s", path, source)
			}
		})
	}
}

func TestExamplesPipelineSelectsOneCanonicalWorkflow(t *testing.T) {
	root := filepath.Join("..", "..")
	source, err := os.ReadFile(filepath.Join(root, ".buildkite", "examples.yml"))
	if err != nil {
		t.Fatal(err)
	}
	type option struct {
		Label string `yaml:"label"`
		Value string `yaml:"value"`
	}
	var document struct {
		Agents struct {
			Queue string `yaml:"queue"`
		} `yaml:"agents"`
		Steps []struct {
			Block            string `yaml:"block"`
			Label            string `yaml:"label"`
			Key              string `yaml:"key"`
			If               string `yaml:"if"`
			Command          string `yaml:"command"`
			TimeoutInMinutes int    `yaml:"timeout_in_minutes"`
			Fields           []struct {
				Select   string   `yaml:"select"`
				Key      string   `yaml:"key"`
				Required bool     `yaml:"required"`
				Default  string   `yaml:"default"`
				Options  []option `yaml:"options"`
			} `yaml:"fields"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatalf("parse examples pipeline: %v", err)
	}
	if document.Agents.Queue != "hosted" || len(document.Steps) != 2 {
		t.Fatalf("examples pipeline structure = queue %q, steps %#v", document.Agents.Queue, document.Steps)
	}
	picker := document.Steps[0]
	if picker.Key != "choose-example" || picker.Block == "" || picker.If != `build.env("EXAMPLE") == null` || len(picker.Fields) != 1 {
		t.Fatalf("example picker = %#v", picker)
	}
	field := picker.Fields[0]
	wantOptions := []option{{"Basic CI", "basic"}, {"Artifact build and handoff", "artifacts"}, {"Advanced delivery", "advanced"}}
	if field.Select != "Example" || field.Key != "example" || !field.Required || field.Default != "basic" || fmt.Sprint(field.Options) != fmt.Sprint(wantOptions) {
		t.Fatalf("example picker field = %#v", field)
	}
	loader := document.Steps[1]
	if loader.Key != "example-loader" || loader.Label == "" || loader.TimeoutInMinutes != 10 {
		t.Fatalf("example loader = %#v", loader)
	}
	for _, required := range []string{
		`example="$${EXAMPLE:-}"`,
		`buildkite-agent meta-data get example`,
		`.github/workflows/example-basic.yml`,
		`.github/workflows/example-artifacts.yml`,
		`.github/workflows/example-advanced.yml`,
		`EXAMPLE_COMMIT`,
		`test "$${BUILDKITE_COMMIT:?BUILDKITE_COMMIT is required}" = "$$commit"`,
		`pipeline upload --no-interpolation --reject-secrets`,
		`queue: "hosted"`,
		`group: ":github: Run workflow"`,
		`label: "Prepare workflow"`,
		`cache: "/cache/bkcache/github-actions-buildkite-plugin"`,
		`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
		`github-actions#v0.4.4`,
		`buildkite-gha-source-ref: "$$commit"`,
	} {
		if !strings.Contains(loader.Command, required) {
			t.Fatalf("example loader lacks %q:\n%s", required, loader.Command)
		}
	}
	for _, forbidden := range []string{"mise run", "plugin-demo.yml", "cache.yml", "local-actions-oracle.yml"} {
		if strings.Contains(loader.Command, forbidden) {
			t.Fatalf("example loader contains %q:\n%s", forbidden, loader.Command)
		}
	}

	workflows := map[string]string{
		"example-basic.yml":     "Example - basic CI",
		"example-artifacts.yml": "Example - build artifact",
		"example-advanced.yml":  "Example - advanced delivery",
	}
	for path, wantName := range workflows {
		workflowSource, err := os.ReadFile(filepath.Join(root, ".github", "workflows", path))
		if err != nil {
			t.Fatal(err)
		}
		var workflowDocument struct {
			Name string         `yaml:"name"`
			On   map[string]any `yaml:"on"`
		}
		if err := yaml.Unmarshal(workflowSource, &workflowDocument); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if workflowDocument.Name != wantName || len(workflowDocument.On) != 1 {
			t.Fatalf("%s trigger contract = name %q, on %#v", path, workflowDocument.Name, workflowDocument.On)
		}
		if _, exists := workflowDocument.On["workflow_dispatch"]; !exists {
			t.Fatalf("%s is not manually dispatchable: %#v", path, workflowDocument.On)
		}
	}
	if basic, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "example-basic.yml")); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(basic), "github.event_name") {
		t.Fatalf("manual basic example retains event-specific behavior:\n%s", basic)
	}
}

func TestUploadExamplesScript(t *testing.T) {
	root := filepath.Join("..", "..")
	script, err := filepath.Abs(filepath.Join(root, ".buildkite", "upload-examples.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const commit = "0123456789abcdef0123456789abcdef01234567"

	run := func(t *testing.T, environment map[string]string, arguments ...string) (string, string, error) {
		t.Helper()
		fakeBin := t.TempDir()
		capture := filepath.Join(fakeBin, "pipeline.yml")
		capturedArguments := filepath.Join(fakeBin, "arguments")
		fakeAgent := filepath.Join(fakeBin, "buildkite-agent")
		if err := os.WriteFile(fakeAgent, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURED_ARGUMENTS\"\ncat > \"$CAPTURE\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(script, arguments...)
		command.Dir = root
		command.Env = []string{
			"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"CAPTURE=" + capture,
			"CAPTURED_ARGUMENTS=" + capturedArguments,
		}
		for key, value := range environment {
			command.Env = append(command.Env, key+"="+value)
		}
		output, runErr := command.CombinedOutput()
		pipeline, readErr := os.ReadFile(capture)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		uploadArguments, readErr := os.ReadFile(capturedArguments)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		if runErr == nil {
			var document any
			if err := yaml.Unmarshal(pipeline, &document); err != nil {
				t.Fatalf("generated invalid YAML: %v\n%s", err, pipeline)
			}
		}
		return string(pipeline), string(uploadArguments) + string(output), runErr
	}

	t.Run("interactive picker", func(t *testing.T) {
		pipeline, output, err := run(t, nil)
		if err != nil {
			t.Fatalf("upload picker: %v\n%s", err, output)
		}
		for _, required := range []string{
			`key: "choose-example"`,
			`value: "basic"`,
			`value: "artifacts"`,
			`value: "advanced"`,
			`example="$(buildkite-agent meta-data get example)"`,
			`.buildkite/upload-examples.sh "$example"`,
			`queue: "hosted"`,
		} {
			if !strings.Contains(pipeline, required) {
				t.Fatalf("picker lacks %q:\n%s", required, pipeline)
			}
		}
		if strings.Contains(pipeline, "github-actions#") {
			t.Fatalf("picker unexpectedly contains importer:\n%s", pipeline)
		}
		if output != "pipeline\nupload\n--no-interpolation\n--reject-secrets\n" {
			t.Fatalf("upload arguments = %q", output)
		}
	})

	t.Run("environment selection", func(t *testing.T) {
		pipeline, output, err := run(t, map[string]string{
			"EXAMPLE":          "basic",
			"EXAMPLE_COMMIT":   commit,
			"BUILDKITE_COMMIT": commit,
		})
		if err != nil {
			t.Fatalf("upload importer: %v\n%s", err, output)
		}
		for _, required := range []string{
			`group: ":github: Run workflow"`,
			`key: "example-basic-workflow"`,
			`label: "Prepare workflow"`,
			`key: "example-basic-importer"`,
			`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
			`github-actions#v0.4.4`,
			`workflow: ".github/workflows/example-basic.yml"`,
			`buildkite-gha-source-ref: "` + commit + `"`,
			`cache: "/cache/bkcache/github-actions-buildkite-plugin"`,
		} {
			if !strings.Contains(pipeline, required) {
				t.Fatalf("basic importer lacks %q:\n%s", required, pipeline)
			}
		}
		for _, forbidden := range []string{"choose-example", "example-loader"} {
			if strings.Contains(pipeline, forbidden) {
				t.Fatalf("basic importer contains %q:\n%s", forbidden, pipeline)
			}
		}
		if strings.Count(pipeline, "github-actions#v0.4.4") != 1 || strings.Count(pipeline, "mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2") != 1 {
			t.Fatalf("basic importer does not contain exactly one of each plugin:\n%s", pipeline)
		}
	})

	t.Run("positional selection", func(t *testing.T) {
		pipeline, output, err := run(t, map[string]string{"BUILDKITE_COMMIT": commit}, "artifacts")
		if err != nil {
			t.Fatalf("upload importer: %v\n%s", err, output)
		}
		if !strings.Contains(pipeline, `workflow: ".github/workflows/example-artifacts.yml"`) {
			t.Fatalf("artifact importer has wrong workflow:\n%s", pipeline)
		}
	})

	for _, test := range []struct {
		name        string
		environment map[string]string
		arguments   []string
	}{
		{name: "unknown example", environment: map[string]string{"EXAMPLE": "other"}},
		{name: "empty example", environment: map[string]string{"EXAMPLE": ""}},
		{name: "malformed build commit", environment: map[string]string{"EXAMPLE": "basic", "BUILDKITE_COMMIT": "abc"}},
		{name: "malformed comparison commit", environment: map[string]string{"EXAMPLE": "basic", "EXAMPLE_COMMIT": "abc", "BUILDKITE_COMMIT": commit}},
		{name: "mismatched commit", environment: map[string]string{"EXAMPLE": "basic", "EXAMPLE_COMMIT": commit, "BUILDKITE_COMMIT": "1123456789abcdef0123456789abcdef01234567"}},
		{name: "too many arguments", arguments: []string{"basic", "advanced"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline, output, err := run(t, test.environment, test.arguments...)
			if err == nil {
				t.Fatalf("invalid invocation succeeded:\n%s", output)
			}
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 2 {
				t.Fatalf("invalid invocation error = %v, output:\n%s", err, output)
			}
			if pipeline != "" {
				t.Fatalf("invalid invocation uploaded pipeline:\n%s", pipeline)
			}
		})
	}
}

func TestProductionPluginDemoContract(t *testing.T) {
	root := filepath.Join("..", "..")
	source, err := os.ReadFile(filepath.Join(root, ".buildkite", "plugin-demo.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const plugin = "github-actions#v0.8.0"
	type pluginConfig struct {
		Workflow string `yaml:"workflow"`
		Version  string `yaml:"version"`
	}
	var document struct {
		Agents struct {
			Queue string `yaml:"queue"`
		} `yaml:"agents"`
		Steps []struct {
			Key              string                    `yaml:"key"`
			If               string                    `yaml:"if"`
			Command          string                    `yaml:"command"`
			TimeoutInMinutes int                       `yaml:"timeout_in_minutes"`
			DependsOn        []string                  `yaml:"depends_on"`
			Plugins          []map[string]pluginConfig `yaml:"plugins"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatalf("parse released plugin demo: %v", err)
	}
	if document.Agents.Queue != "hosted" || len(document.Steps) != 7 {
		t.Fatalf("released plugin demo structure = queue %q, steps %#v", document.Agents.Queue, document.Steps)
	}
	steps := make(map[string]struct {
		condition    string
		command      string
		timeout      int
		dependencies []string
		plugins      []map[string]pluginConfig
	}, len(document.Steps))
	for _, step := range document.Steps {
		if _, exists := steps[step.Key]; step.Key == "" || exists {
			t.Fatalf("invalid or duplicate released plugin demo key %q", step.Key)
		}
		steps[step.Key] = struct {
			condition    string
			command      string
			timeout      int
			dependencies []string
			plugins      []map[string]pluginConfig
		}{step.If, step.Command, step.TimeoutInMinutes, step.DependsOn, step.Plugins}
	}
	importers := map[string]string{
		"plugin-demo-basic-importer":    ".github/workflows/example-basic.yml",
		"plugin-demo-artifact-importer": ".github/workflows/example-artifacts.yml",
		"plugin-demo-actions-importer":  ".github/workflows/local-actions-oracle.yml",
		"plugin-demo-advanced-importer": ".github/workflows/example-advanced.yml",
		"plugin-demo-cache-importer":    "testdata/plugin-demo/.github/workflows/cache.yml",
	}
	for key, workflow := range importers {
		step := steps[key]
		if step.command != "" || step.timeout != 15 || len(step.plugins) != 1 || len(step.plugins[0]) != 1 {
			t.Fatalf("released plugin importer %q = %#v", key, step)
		}
		config, exists := step.plugins[0][plugin]
		if !exists || config.Workflow != workflow || config.Version != "" {
			t.Fatalf("released plugin importer %q config = %#v", key, step.plugins)
		}
		wantCondition := ""
		if key == "plugin-demo-cache-importer" {
			wantCondition = `build.env("DEMO_CACHE") == "1"`
		}
		if step.condition != wantCondition {
			t.Fatalf("released plugin importer %q condition = %q, want %q", key, step.condition, wantCondition)
		}
	}
	serviceTerminal := steps["plugin-demo-service-free-terminal"]
	wantServiceDependencies := map[string]bool{
		"plugin-demo-basic-importer":    true,
		"plugin-demo-artifact-importer": true,
		"plugin-demo-actions-importer":  true,
		"plugin-demo-advanced-importer": true,
	}
	if len(serviceTerminal.dependencies) != len(wantServiceDependencies) {
		t.Fatalf("released plugin service-free terminal dependencies = %v", serviceTerminal.dependencies)
	}
	for _, dependency := range serviceTerminal.dependencies {
		if !wantServiceDependencies[dependency] {
			t.Fatalf("released plugin service-free terminal has unexpected dependency %q", dependency)
		}
	}
	cacheTerminal := steps["plugin-demo-cache-terminal"]
	if cacheTerminal.condition != `build.env("DEMO_CACHE") == "1"` || len(cacheTerminal.dependencies) != 1 || cacheTerminal.dependencies[0] != "plugin-demo-cache-importer" {
		t.Fatalf("released plugin cache terminal = %#v", cacheTerminal)
	}
	text := string(source)
	for _, forbidden := range []string{"github-actions#main", "github-actions#master"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("released plugin demo contains %q", forbidden)
		}
	}

	advanced, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "example-advanced.yml"))
	if err != nil {
		t.Fatal(err)
	}
	advancedText := string(advanced)
	for _, required := range []string{
		"actions/hello-world-docker-action@66e612e94eca3366d470e868d0c2d86bd25e693d",
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093",
		"continue-on-error: true",
	} {
		if !strings.Contains(advancedText, required) {
			t.Fatalf("advanced released-plugin fixture lacks %q", required)
		}
	}
	if strings.Contains(advancedText, "actions/cache@") {
		t.Fatalf("service-free released-plugin fixture retains cache or source rewriting:\n%s", advanced)
	}

	cache, err := os.ReadFile(filepath.Join(root, "testdata", "plugin-demo", ".github", "workflows", "cache.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cacheText := string(cache)
	if count := strings.Count(cacheText, "actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9"); count != 2 {
		t.Fatalf("released-plugin cache fixture has %d audited cache invocations, want two", count)
	}
	for _, forbidden := range []string{"restore-keys:", "BUILDKITE_"} {
		if strings.Contains(cacheText, forbidden) {
			t.Fatalf("released-plugin cache fixture contains %q", forbidden)
		}
	}
}

func TestShellUploadProofUsesPinnedUnprivilegedPath(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "shell-upload-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
		`mise exec -- go build -trimpath -buildvcs=false`,
		`--event-path testdata/smoke/events/push.json`,
		`--runner-queue ubuntu-latest=hosted`,
		`testdata/smoke/.github/workflows/shell.yml`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Shell upload upload proof lacks %q:\n%s", required, source)
		}
	}
	var document struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 2 || document.Steps[0].Key != "shell-upload-importer" {
		t.Fatalf("Shell upload upload proof = %#v", document.Steps)
	}
	if document.Steps[1].Key != "shell-upload-continuation-loader" || document.Steps[1].DependsOn != "shell-upload-importer" || !document.Steps[1].AllowDependencyFailure || document.Steps[1].Command != "buildkite-agent pipeline upload .buildkite/shell-upload-continuation.yml" {
		t.Fatalf("Shell upload continuation loader = %#v", document.Steps[1])
	}

	continuationSource, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "shell-upload-continuation.yml"))
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
	if len(continuation.Steps) != 1 || continuation.Steps[0].Key != "shell-upload-complete" {
		t.Fatalf("Shell upload continuation = %#v", continuation.Steps)
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
		t.Fatalf("Shell upload continuation dependencies = %#v", continuation.Steps[0].DependsOn)
	}
	for i, want := range wantDependencies {
		dependency := continuation.Steps[0].DependsOn[i]
		if dependency.Step != want || !dependency.AllowFailure {
			t.Fatalf("Shell upload continuation dependency %d = %#v, want %q with allow_failure", i, dependency, want)
		}
		if _, ok := generatedKeys[dependency.Step]; !ok {
			t.Fatalf("Shell upload continuation dependency %q is absent from shell.pipeline.golden.yml", dependency.Step)
		}
		delete(generatedConsumers, dependency.Step)
	}
	if len(generatedConsumers) != 0 {
		t.Fatalf("Shell upload continuation omits generated consumers: %#v", generatedConsumers)
	}
}

func TestConcurrentStepsProofPreservesSeparateContinuationLoader(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "concurrent-steps-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
		`mise exec -- go build -trimpath -buildvcs=false`,
		`--event-path testdata/smoke/events/push.json`,
		`--runner-queue ubuntu-latest=hosted`,
		`testdata/smoke/.github/workflows/concurrent.yml`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Concurrent steps upload proof lacks %q:\n%s", required, source)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "concurrent-steps-importer" {
		t.Fatalf("Concurrent steps upload proof = %#v", upload.Steps)
	}
	if upload.Steps[1].Key != "concurrent-steps-continuation-loader" || upload.Steps[1].DependsOn != "concurrent-steps-importer" || !upload.Steps[1].AllowDependencyFailure || upload.Steps[1].Command != "buildkite-agent pipeline upload .buildkite/concurrent-steps-continuation.yml" {
		t.Fatalf("Concurrent steps continuation loader = %#v", upload.Steps[1])
	}

	continuationSource, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "concurrent-steps-continuation.yml"))
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
	if len(continuation.Steps) != 1 || continuation.Steps[0].Key != "concurrent-steps-complete" || len(continuation.Steps[0].DependsOn) != 1 {
		t.Fatalf("Concurrent steps continuation = %#v", continuation.Steps)
	}
	dependency := continuation.Steps[0].DependsOn[0]
	if dependency.Step != "gha-observe" || !dependency.AllowFailure {
		t.Fatalf("Concurrent steps continuation dependency = %#v", dependency)
	}
}

func TestPublicActionsProofUsesTrustedManagedActionsPath(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "public-actions-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	eventSource, err := os.ReadFile(filepath.Join("..", "..", "testdata", "public-actions", "events", "public-checkout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var publicEvent struct {
		Repository struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		} `json:"repository"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(eventSource, &publicEvent); err != nil {
		t.Fatal(err)
	}
	workflowSource, err := os.ReadFile(filepath.Join("..", "..", "testdata", "public-actions", ".github", "workflows", "public-actions.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflowText := string(workflowSource)
	if publicEvent.Repository.Owner != "actions" || publicEvent.Repository.Name != "checkout" || !strings.Contains(workflowText, "uses: actions/checkout@"+publicEvent.SHA) || !strings.Contains(workflowText, `test "$(git rev-parse HEAD)" = "`+publicEvent.SHA+`"`) {
		t.Fatalf("Public actions public checkout identity drifted: event=%#v workflow=%s", publicEvent, workflowSource)
	}
	for _, required := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
		`version: "2026.5.12"`,
		`runtime_version="0.0.0-public-actions.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`BUILDKITE_GHA_NODE20="$$(mise where node@20)/bin/node"`,
		`BUILDKITE_GHA_NODE24="$$(mise where node@24)/bin/node"`,
		`"$$distribution_root/buildkite-gha" upload`,
		`--event-path testdata/public-actions/events/public-checkout.json`, `--runner-queue ubuntu-latest=hosted`,
		`testdata/public-actions/.github/workflows/public-actions.yml`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Public actions upload proof lacks %q:\n%s", required, source)
		}
	}
	for _, forbidden := range []string{"go run", "--runtime ", "--runtime-version", "--node24", "--commit"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Public actions upload proof retains harness-only argument %q:\n%s", forbidden, source)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "public-actions-importer" {
		t.Fatalf("Public actions upload proof = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "public-actions-continuation-loader" || loader.DependsOn != "public-actions-importer" || !loader.AllowDependencyFailure || loader.Command != "buildkite-agent pipeline upload .buildkite/public-actions-continuation.yml" {
		t.Fatalf("Public actions continuation loader = %#v", loader)
	}
	continuationSource, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "public-actions-continuation.yml"))
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
	if len(continuation.Steps) != 1 || continuation.Steps[0].Key != "public-actions-complete" || len(continuation.Steps[0].DependsOn) != 1 || continuation.Steps[0].DependsOn[0].Step != "gha-public-actions" || !continuation.Steps[0].DependsOn[0].AllowFailure {
		t.Fatalf("Public actions continuation = %#v", continuation.Steps)
	}
}

func TestHostedDockerCapabilityProbeContract(t *testing.T) {
	root := filepath.Join("..", "..")
	type step struct {
		Key                    string `yaml:"key"`
		Command                string `yaml:"command"`
		DependsOn              any    `yaml:"depends_on"`
		AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		Timeout                int    `yaml:"timeout_in_minutes"`
		Retry                  struct {
			Automatic bool `yaml:"automatic"`
		} `yaml:"retry"`
	}
	read := func(name string) ([]byte, []step) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, ".buildkite", name))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Steps []step `yaml:"steps"`
		}
		if err := yaml.Unmarshal(body, &document); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return body, document.Steps
	}
	importerBody, importer := read("hosted-docker-proof.yml")
	if len(importer) != 2 || importer[0].Key != "hosted-docker-importer" || importer[1].Key != "hosted-docker-continuation-loader" || fmt.Sprint(importer[1].DependsOn) != "hosted-docker-importer" || !importer[1].AllowDependencyFailure || importer[1].Command != "buildkite-agent pipeline upload .buildkite/hosted-docker-continuation.yml" {
		t.Fatalf("Hosted Docker importer = %#v", importer)
	}
	for _, fragment := range []string{`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`, `git status --porcelain --untracked-files=all`, `pipeline upload --no-interpolation --reject-secrets .buildkite/hosted-docker-probe.yml`} {
		if !strings.Contains(string(importerBody), fragment) {
			t.Fatalf("Hosted Docker importer lacks %q", fragment)
		}
	}
	hostedBody, hosted := read("hosted-docker-probe.yml")
	if len(hosted) != 1 || hosted[0].Key != "hosted-docker-probe" || hosted[0].Timeout != 15 || hosted[0].Retry.Automatic || hosted[0].Command != "scripts/hosted-docker-probe" || !strings.Contains(string(hostedBody), "automatic: false") {
		t.Fatalf("Hosted Docker pipeline = %#v\n%s", hosted, hostedBody)
	}
	_, continuation := read("hosted-docker-continuation.yml")
	if len(continuation) != 1 || continuation[0].Key != "hosted-docker-complete" {
		t.Fatalf("Hosted behavior continuation = %#v", continuation)
	}
	continuationBody, _ := os.ReadFile(filepath.Join(root, ".buildkite", "hosted-docker-continuation.yml"))
	if !strings.Contains(string(continuationBody), `step: "hosted-docker-probe"`) || !strings.Contains(string(continuationBody), `allow_failure: true`) {
		t.Fatalf("Hosted behavior continuation dependency is not failure-tolerant: %s", continuationBody)
	}

	probe, err := os.ReadFile(filepath.Join(root, "scripts", "hosted-docker-probe"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(probe)
	for _, fragment := range []string{"set -euo pipefail", `commit=${COMPATIBILITY_PROOF_COMMIT:-${SMOKE_COMMIT:-}}`, `scripts/verify-source-checkout "$commit"`, "busybox@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028", `buildx inspect default`, `buildx build --builder default --load`, `default-docker-driver`, `127.0.0.1::8080`, `wget -qO- http://hosted-docker-server:8080/hosted-docker-marker`, `/dev/tcp/127.0.0.1/$1`, `trap cleanup EXIT`, `trap - EXIT`, `exit "$final"`, `trap 'exit 130' INT`, `trap 'exit 143' TERM`, `docker container ls --all --quiet`, `--filter "label=${label_key}=${owner}"`, `timeout 30s`, `docker stop --time 5`, `term-observed`, `record httpd-applet pass execution-confirmed`, `record bind-requested fail`, `record signal-stop fail`, `(( probe_failed == 0 ))`, "COPY marker"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Hosted Docker probe lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"--privileged", "--network host", "/var/run/docker.sock", "docker prune", "docker system prune", "docker image prune", "docker container prune", "docker network prune", "busybox --list", "printenv", " env"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Hosted Docker probe contains forbidden %q", forbidden)
		}
	}
}

func TestDockerfileActionUploadProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	importerBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "dockerfile-action-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(importerBody)
	for _, fragment := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`automatic: false`,
		`cp testdata/dockerfile-action/.github/workflows/docker-action.yml.tmpl`,
		`runtime_version="0.0.0-dockerfile-action.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`BUILDKITE_GHA_NODE20="$$(mise where node@20)/bin/node"`,
		`BUILDKITE_GHA_NODE24="$$(mise where node@24)/bin/node"`,
		`"$$proof_root/buildkite-gha" upload`,
		`--runner-queue ubuntu-latest=hosted`,
		`"$$proof_root/.github/workflows/docker-action.yml"`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Dockerfile action proof lacks %q:\n%s", fragment, importerBody)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "dockerfile-action-importer" {
		t.Fatalf("Dockerfile action importer = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "dockerfile-action-continuation-loader" || loader.DependsOn != "dockerfile-action-importer" || !loader.AllowDependencyFailure || loader.Command != "buildkite-agent pipeline upload .buildkite/dockerfile-action-continuation.yml" {
		t.Fatalf("Dockerfile action continuation loader = %#v", loader)
	}

	continuationBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "dockerfile-action-continuation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(continuationBody), `step: "gha-docker-action"`) || !strings.Contains(string(continuationBody), `allow_failure: true`) {
		t.Fatalf("Dockerfile action continuation is not failure-tolerant: %s", continuationBody)
	}

	templateBody, err := os.ReadFile(filepath.Join(root, "testdata", "dockerfile-action", ".github", "workflows", "docker-action.yml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	githubBody, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "dockerfile-action-oracle.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const action = "uses: actions/hello-world-docker-action@66e612e94eca3366d470e868d0c2d86bd25e693d"
	observation := `DOCKERFILE_ACTION_OBSERVATION={"container":"ran","output":"propagated","source":"public-exact"}`
	if !strings.Contains(string(templateBody), action) || !strings.Contains(string(githubBody), action) || !strings.Contains(string(templateBody), observation) || !strings.Contains(string(githubBody), observation) {
		t.Fatalf("Dockerfile differential fixtures drifted:\ntemplate:\n%s\nGitHub:\n%s", templateBody, githubBody)
	}
}

func TestContainerRuntimeProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(name string) []byte {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, ".buildkite", name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	importerBody := read("container-runtime-proof.yml")
	var importer struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &importer); err != nil {
		t.Fatal(err)
	}
	if len(importer.Steps) != 2 || importer.Steps[0].Key != "container-runtime-importer" {
		t.Fatalf("Container runtime importer = %#v", importer.Steps)
	}
	loader := importer.Steps[1]
	if loader.Key != "container-runtime-continuation-loader" || loader.DependsOn != "container-runtime-importer" || !loader.AllowDependencyFailure || loader.Command != "buildkite-agent pipeline upload .buildkite/container-runtime-continuation.yml" {
		t.Fatalf("Container runtime continuation loader = %#v", loader)
	}
	for _, fragment := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`git status --porcelain --untracked-files=all`,
		`pipeline upload --no-interpolation --reject-secrets .buildkite/container-runtime-probe.yml`,
		`automatic: false`,
	} {
		if !strings.Contains(string(importerBody), fragment) {
			t.Fatalf("Container runtime importer lacks %q:\n%s", fragment, importerBody)
		}
	}

	probeBody := read("container-runtime-probe.yml")
	for _, fragment := range []string{`key: "container-runtime-probe"`, `queue: "hosted"`, `timeout_in_minutes: 25`, `automatic: false`, `mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`, `command: "scripts/container-runtime-probe"`} {
		if !strings.Contains(string(probeBody), fragment) {
			t.Fatalf("Container runtime probe pipeline lacks %q:\n%s", fragment, probeBody)
		}
	}
	continuationBody := read("container-runtime-continuation.yml")
	for _, fragment := range []string{`key: "container-runtime-complete"`, `step: "container-runtime-probe"`, `allow_failure: true`} {
		if !strings.Contains(string(continuationBody), fragment) {
			t.Fatalf("Container runtime continuation lacks %q:\n%s", fragment, continuationBody)
		}
	}

	script, err := os.ReadFile(filepath.Join(root, "scripts", "container-runtime-probe"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, fragment := range []string{
		"set -euo pipefail",
		`commit=${COMPATIBILITY_PROOF_COMMIT:-${SMOKE_COMMIT:-}}`,
		`scripts/verify-source-checkout "$commit"`,
		`git status --porcelain --untracked-files=all`,
		`docker buildx inspect default`,
		`CGO_ENABLED=0 mise exec -- go build`,
		`BUILDKITE_GHA_LIVE_REQUIRED=1`,
		`BUILDKITE_GHA_TEST_RUNTIME="$runtime"`,
		`BUILDKITE_GHA_TEST_NODE24="$node24"`,
		`go test ./internal/runtime -count=1 -timeout=20m`,
		`TestLiveCompiledContainerRuntime`,
		`TestLiveManifestContainerFixtures`,
		`TestLiveUnhealthyServiceDiagnostics`,
		`--- SKIP:`,
		`CONTAINER_RUNTIME_OBSERVATION=`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Container runtime probe script lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"--privileged", "--network host", "/var/run/docker.sock", "docker prune", "docker system prune", "printenv"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Container runtime probe contains forbidden %q", forbidden)
		}
	}
}

func TestSummaryAnnotationProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	importerBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "summary-annotation-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(importerBody)
	for _, fragment := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`automatic: false`,
		`runtime_version="0.0.0-summary-annotation.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`"$$distribution_root/buildkite-gha" upload`,
		`--event-path testdata/smoke/events/push.json`,
		`--runner-queue ubuntu-latest=hosted`,
		`testdata/smoke/.github/workflows/summary-annotation.yml`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Summary annotation importer lacks %q:\n%s", fragment, importerBody)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "summary-annotation-importer" {
		t.Fatalf("Summary annotation importer = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "summary-annotation-continuation-loader" || loader.DependsOn != "summary-annotation-importer" || !loader.AllowDependencyFailure || loader.Command != "buildkite-agent pipeline upload .buildkite/summary-annotation-continuation.yml" {
		t.Fatalf("Summary annotation continuation loader = %#v", loader)
	}

	continuationBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "summary-annotation-continuation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(continuationBody), `key: "summary-annotation-complete"`) || !strings.Contains(string(continuationBody), `step: "gha-summary-annotation"`) || !strings.Contains(string(continuationBody), `allow_failure: true`) {
		t.Fatalf("Summary annotation continuation is not failure-tolerant: %s", continuationBody)
	}

	fixture, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", ".github", "workflows", "summary-annotation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(fixture), `GITHUB_STEP_SUMMARY`) != 3 || !strings.Contains(string(fixture), "Job summary annotation proof") || !strings.Contains(string(fixture), "SUMMARY_ANNOTATION_OBSERVATION=job-scoped-annotation") {
		t.Fatalf("Summary annotation fixture does not append both proof fragments: %s", fixture)
	}

	verifierPath := filepath.Join(root, "scripts", "verify-summary-annotation")
	verifier, err := os.ReadFile(verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	verifierText := string(verifier)
	for _, fragment := range []string{
		"set -euo pipefail",
		`expected_commit=$2`,
		`bk --no-pager api "/pipelines/$pipeline/builds/$build_number"`,
		`commit="$(jq -r '.commit // ""' <<<"$build")"`,
		`$commit != "$expected_commit"`,
		`step_key=gha-summary-annotation`,
		`context=buildkite-gha-job-summary`,
		`bk --no-pager api "/jobs/$job_id/annotations"`,
		`($matches | length) == 1`,
		`$matches[0].scope == "job"`,
		`$matches[0].job_id == $job_id`,
		`$matches[0].style == "info"`,
		`SUMMARY_ANNOTATION_OBSERVATION=job-scoped-annotation`,
		`SUMMARY_ANNOTATION_PROOF_OBSERVATION=`,
		`"$build_number" "$commit" "$job_id" "$context"`,
	} {
		if !strings.Contains(verifierText, fragment) {
			t.Fatalf("Summary annotation verifier lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"BUILDKITE_API_TOKEN", "Authorization:", "bk auth token"} {
		if strings.Contains(verifierText, forbidden) {
			t.Fatalf("Summary annotation verifier handles credentials directly through %q", forbidden)
		}
	}
}

func TestWorkflowCommandAnnotationProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	importerBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "workflow-annotations-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(importerBody)
	for _, fragment := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`automatic: false`,
		`runtime_version="0.0.0-workflow-annotations.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`"$$distribution_root/buildkite-gha" upload`,
		`--event-path testdata/smoke/events/push.json`,
		`--runner-queue ubuntu-latest=hosted`,
		`testdata/smoke/.github/workflows/workflow-command-annotations.yml`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Workflow annotation importer lacks %q:\n%s", fragment, importerBody)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "workflow-annotations-importer" {
		t.Fatalf("Workflow annotation importer = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "workflow-annotations-continuation-loader" || loader.DependsOn != "workflow-annotations-importer" || !loader.AllowDependencyFailure || loader.Command != "buildkite-agent pipeline upload .buildkite/workflow-annotations-continuation.yml" {
		t.Fatalf("Workflow annotation continuation loader = %#v", loader)
	}

	continuationBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "workflow-annotations-continuation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(continuationBody), `key: "workflow-annotations-complete"`) || !strings.Contains(string(continuationBody), `step: "gha-workflow-command-annotations"`) || !strings.Contains(string(continuationBody), `allow_failure: true`) {
		t.Fatalf("Workflow annotation continuation is not failure-tolerant: %s", continuationBody)
	}

	fixture, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", ".github", "workflows", "workflow-command-annotations.yml"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureText := string(fixture)
	for _, fragment := range []string{"::warning title=Workflow warning", "::error title=Workflow error", "WORKFLOW_WARNING_OBSERVATION=stdout-with-location", "WORKFLOW_ERROR_OBSERVATION=stderr-outcome-neutral", "::add-mask::$canary", "workflow-command-secret"} {
		if !strings.Contains(fixtureText, fragment) {
			t.Fatalf("Workflow annotation fixture lacks %q: %s", fragment, fixture)
		}
	}

	verifierPath := filepath.Join(root, "scripts", "verify-workflow-annotations")
	verifier, err := os.ReadFile(verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	verifierText := string(verifier)
	for _, fragment := range []string{
		"set -euo pipefail",
		`expected_commit=$2`,
		`bk --no-pager api "/pipelines/$pipeline/builds/$build_number"`,
		`$commit != "$expected_commit"`,
		`step_key=gha-workflow-command-annotations`,
		`warning_context=buildkite-gha-workflow-warnings`,
		`error_context=buildkite-gha-workflow-errors`,
		`bk --no-pager api "/jobs/$job_id/annotations"`,
		`($warnings | length) == 1`,
		`($errors | length) == 1`,
		`$warnings[0].style == "warning"`,
		`$errors[0].style == "error"`,
		`contains($masked_canary) | not`,
		`WORKFLOW_ANNOTATIONS_OBSERVATION=`,
	} {
		if !strings.Contains(verifierText, fragment) {
			t.Fatalf("Workflow annotation verifier lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"BUILDKITE_API_TOKEN", "Authorization:", "bk auth token"} {
		if strings.Contains(verifierText, forbidden) {
			t.Fatalf("Workflow annotation verifier handles credentials directly through %q", forbidden)
		}
	}
}

func TestUploadArtifactProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	importerBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "upload-artifact-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(importerBody)
	for _, fragment := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`automatic: false`,
		`runtime_version="0.0.0-upload-artifact.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`"$$distribution_root/buildkite-gha" upload`,
		`--event-path testdata/smoke/events/push.json`,
		`--runner-queue ubuntu-latest=hosted`,
		`testdata/smoke/.github/workflows/upload-artifact.yml`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Upload-artifact importer lacks %q:\n%s", fragment, importerBody)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "upload-artifact-importer" {
		t.Fatalf("Upload-artifact importer = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "upload-artifact-continuation-loader" || loader.DependsOn != "upload-artifact-importer" || !loader.AllowDependencyFailure || loader.Command != "buildkite-agent pipeline upload .buildkite/upload-artifact-continuation.yml" {
		t.Fatalf("Upload-artifact continuation loader = %#v", loader)
	}

	continuationBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "upload-artifact-continuation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(continuationBody), `key: "upload-artifact-complete"`) || !strings.Contains(string(continuationBody), `step: "gha-upload-artifact"`) || !strings.Contains(string(continuationBody), `allow_failure: true`) {
		t.Fatalf("Upload-artifact continuation is not failure-tolerant: %s", continuationBody)
	}

	fixture, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", ".github", "workflows", "upload-artifact.yml"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureText := string(fixture)
	for _, fragment := range []string{
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"name: upload-artifact-proof",
		"path: payload",
		"if-no-files-found: error",
		"compression-level: 0",
		"payload/.hidden-canary",
		"steps.upload.outputs.artifact-id",
		"steps.upload.outputs.artifact-digest",
		"UPLOAD_ARTIFACT_OUTPUTS=",
	} {
		if !strings.Contains(fixtureText, fragment) {
			t.Fatalf("Upload-artifact fixture lacks %q: %s", fragment, fixture)
		}
	}

	verifierPath := filepath.Join(root, "scripts", "verify-upload-artifact")
	verifier, err := os.ReadFile(verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	verifierText := string(verifier)
	for _, fragment := range []string{
		"set -euo pipefail",
		`expected_commit=$2`,
		`bk --no-pager api "/pipelines/$pipeline/builds/$build_number"`,
		`$commit != "$expected_commit"`,
		`step_key=gha-upload-artifact`,
		`^buildkite-gha/v1/artifacts/[0-9a-f]{64}`,
		`^buildkite-gha/v1/results/gha-upload-artifact/[0-9a-f]{64}`,
		`.schema == "buildkite-gha/result-manifest/v2"`,
		`.artifacts[0].file_count == 2`,
		`sha256sum "$archive_file"`,
		`unzip -Z1 "$archive_file"`,
		`UPLOAD_ARTIFACT_OBSERVATION=`,
	} {
		if !strings.Contains(verifierText, fragment) {
			t.Fatalf("Upload-artifact verifier lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"BUILDKITE_API_TOKEN", "Authorization:", "bk auth token"} {
		if strings.Contains(verifierText, forbidden) {
			t.Fatalf("Upload-artifact verifier handles credentials directly through %q", forbidden)
		}
	}
}

func TestArtifactRoundtripProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	importerBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "artifact-roundtrip-proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(importerBody)
	for _, fragment := range []string{
		`commit="$${COMPATIBILITY_PROOF_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/verify-source-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`automatic: false`,
		`runtime_version="0.0.0-artifact-roundtrip.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`[[ -z "$$proof_workflow" ]] || rm -f -- "$$proof_workflow"`,
		`proof_workflow="$$(mktemp testdata/smoke/.github/workflows/.artifact-roundtrip.XXXXXXXX.yml)"`,
		`nonce="$${BUILDKITE_BUILD_NUMBER:?BUILDKITE_BUILD_NUMBER is required}"`,
		`sed "s/__ARTIFACT_ROUNDTRIP_NONCE__/$$nonce/g" testdata/smoke/.github/workflows/artifact.yml`,
		`! grep -q '__ARTIFACT_ROUNDTRIP_NONCE__' "$$proof_workflow"`,
		`"$$distribution_root/buildkite-gha" upload`,
		`--event-path testdata/smoke/events/push.json`,
		`--runner-queue ubuntu-latest=hosted`,
		`"$$proof_workflow"`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Artifact roundtrip importer lacks %q:\n%s", fragment, importerBody)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "artifact-roundtrip-importer" {
		t.Fatalf("Artifact roundtrip importer = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "artifact-roundtrip-continuation-loader" || loader.DependsOn != "artifact-roundtrip-importer" || !loader.AllowDependencyFailure || loader.Command != "buildkite-agent pipeline upload .buildkite/artifact-roundtrip-continuation.yml" {
		t.Fatalf("Artifact roundtrip continuation loader = %#v", loader)
	}

	continuationBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "artifact-roundtrip-continuation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`key: "artifact-roundtrip-complete"`,
		`step: "gha-artifact-producer"`,
		`step: "gha-artifact-consumer-5ebbc197d87b"`,
		`step: "gha-artifact-consumer-91934b28b00f"`,
		`allow_failure: true`,
	} {
		if !strings.Contains(string(continuationBody), fragment) {
			t.Fatalf("Artifact roundtrip continuation lacks %q: %s", fragment, continuationBody)
		}
	}

	fixture, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", ".github", "workflows", "artifact.yml"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureText := string(fixture)
	for _, fragment := range []string{
		"artifact-producer:",
		"artifact-consumer:",
		"needs: artifact-producer",
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093",
		"ARTIFACT_ROUNDTRIP_PAYLOAD: smoke-artifact:__ARTIFACT_ROUNDTRIP_NONCE__",
		"steps.download.outputs.download-path",
		"ARTIFACT_ROUNDTRIP=",
	} {
		if !strings.Contains(fixtureText, fragment) {
			t.Fatalf("Artifact roundtrip fixture lacks %q: %s", fragment, fixture)
		}
	}

	verifierPath := filepath.Join(root, "scripts", "verify-artifact-roundtrip")
	verifier, err := os.ReadFile(verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	verifierText := string(verifier)
	for _, fragment := range []string{
		"set -euo pipefail",
		`expected_commit=$2`,
		`bk --no-pager api "/pipelines/$pipeline/builds/$build_number"`,
		`producer_key=gha-artifact-producer`,
		`consumer_one_key=gha-artifact-consumer-5ebbc197d87b`,
		`consumer_two_key=gha-artifact-consumer-91934b28b00f`,
		`^buildkite-gha/v1/artifacts/[0-9a-f]{64}`,
		`.schema == "buildkite-gha/result-manifest/v2"`,
		`.artifacts[0].name == "smoke-payload"`,
		`sha256sum "$archive_file"`,
		`archive_payload_digest=`,
		`unzip -Z1 "$archive_file"`,
		`ARTIFACT_ROUNDTRIP=`,
		`ARTIFACT_ROUNDTRIP_OBSERVATION=`,
	} {
		if !strings.Contains(verifierText, fragment) {
			t.Fatalf("Artifact roundtrip verifier lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"BUILDKITE_API_TOKEN", "Authorization:", "bk auth token"} {
		if strings.Contains(verifierText, forbidden) {
			t.Fatalf("Artifact roundtrip verifier handles credentials directly through %q", forbidden)
		}
	}
}

func TestCacheFixtureContract(t *testing.T) {
	root := filepath.Join("..", "..")
	fixture, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", ".github", "workflows", "cache-v6.yml"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureText := string(fixture)
	for _, fragment := range []string{
		"cache-producer:",
		"cache-consumer:",
		"needs: cache-producer",
		"actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9",
		"CACHE_ROUNDTRIP_KEY: cache-roundtrip-__CACHE_ROUNDTRIP_NONCE__",
		`CACHE_HIT: ${{ steps.cache.outputs.cache-hit }}`,
		`test "$CACHE_HIT" != true`,
		`test "$CACHE_HIT" = true`,
		"CACHE_ROUNDTRIP_PRODUCER=",
		"CACHE_ROUNDTRIP_CONSUMER=",
	} {
		if !strings.Contains(fixtureText, fragment) {
			t.Fatalf("Cache roundtrip fixture lacks %q: %s", fragment, fixture)
		}
	}
	if count := strings.Count(fixtureText, "actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9"); count != 2 {
		t.Fatalf("Cache roundtrip fixture has %d audited cache action invocations, want two", count)
	}
	if strings.Contains(fixtureText, "restore-keys:") {
		t.Fatal("Cache roundtrip fixture permits a non-exact restore")
	}
}

func TestHostedSmokeControlAndAggregatorDependencies(t *testing.T) {
	type dependency struct {
		Step         string `yaml:"step"`
		AllowFailure bool   `yaml:"allow_failure"`
	}
	type smokeStep struct {
		Key       string       `yaml:"key"`
		Command   string       `yaml:"command"`
		DependsOn []dependency `yaml:"depends_on"`
	}
	read := func(name string) smokeStep {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", name))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Steps []smokeStep `yaml:"steps"`
		}
		if err := yaml.Unmarshal(body, &document); err != nil || len(document.Steps) != 1 {
			t.Fatalf("parse %s: %v", name, err)
		}
		return document.Steps[0]
	}
	assertSet := func(name string, dependencies []dependency, want map[string]bool) {
		t.Helper()
		if len(dependencies) != len(want) {
			t.Fatalf("%s dependencies = %#v", name, dependencies)
		}
		for _, dependency := range dependencies {
			if !want[dependency.Step] || !dependency.AllowFailure {
				t.Fatalf("%s dependency = %#v", name, dependency)
			}
			delete(want, dependency.Step)
		}
	}
	control := read("hosted-smoke-control.yml")
	if control.Key != "hosted-smoke-aggregator-loader" || control.Command != "buildkite-agent pipeline upload .buildkite/hosted-smoke-aggregator.yml" {
		t.Fatalf("control step = %#v", control)
	}
	assertSet("control", control.DependsOn, map[string]bool{"shell-upload-continuation-loader": true, "concurrent-steps-continuation-loader": true, "public-actions-continuation-loader": true, "hosted-docker-continuation-loader": true, "dockerfile-action-continuation-loader": true, "container-runtime-continuation-loader": true, "summary-annotation-continuation-loader": true, "workflow-annotations-continuation-loader": true, "upload-artifact-continuation-loader": true, "artifact-roundtrip-continuation-loader": true})
	generated := map[string]bool{}
	for _, name := range []string{"shell-upload-continuation.yml", "concurrent-steps-continuation.yml", "public-actions-continuation.yml", "hosted-docker-continuation.yml", "dockerfile-action-continuation.yml", "container-runtime-continuation.yml", "summary-annotation-continuation.yml", "workflow-annotations-continuation.yml", "upload-artifact-continuation.yml", "artifact-roundtrip-continuation.yml"} {
		continuation := read(name)
		for _, dependency := range continuation.DependsOn {
			generated[dependency.Step] = true
		}
	}
	want := map[string]bool{}
	for key := range generated {
		want[key] = true
	}
	for _, key := range []string{"gha-producer", "gha-concurrent", "gha-artifact-producer", "gha-artifact-consumer-5ebbc197d87b", "gha-artifact-consumer-91934b28b00f", "shell-upload-complete", "concurrent-steps-complete", "public-actions-complete", "hosted-docker-complete", "dockerfile-action-complete", "container-runtime-complete", "summary-annotation-complete", "workflow-annotations-complete", "upload-artifact-complete", "artifact-roundtrip-complete"} {
		want[key] = true
	}
	aggregator := read("hosted-smoke-aggregator.yml")
	if aggregator.Key != "hosted-smoke-terminal" {
		t.Fatalf("aggregator step = %#v", aggregator)
	}
	assertSet("aggregator", aggregator.DependsOn, want)
	for key := range generated {
		if !strings.Contains(aggregator.Command, key) {
			t.Fatalf("aggregator command does not inspect generated step %q: %s", key, aggregator.Command)
		}
	}
	for _, key := range []string{"gha-producer", "gha-concurrent", "gha-artifact-producer", "gha-artifact-consumer-5ebbc197d87b", "gha-artifact-consumer-91934b28b00f", "shell-upload-complete", "concurrent-steps-complete", "public-actions-complete", "hosted-docker-complete", "dockerfile-action-complete", "container-runtime-complete", "summary-annotation-complete", "workflow-annotations-complete", "upload-artifact-complete", "artifact-roundtrip-complete"} {
		if !strings.Contains(aggregator.Command, key) {
			t.Fatalf("aggregator command does not inspect required step %q: %s", key, aggregator.Command)
		}
	}
	for _, fragment := range []string{`buildkite-agent step get "outcome"`, `[[ "$$outcome" == passed ]] || failed=1`, `exit 1`, `All hosted smoke targets passed`} {
		if !strings.Contains(aggregator.Command, fragment) {
			t.Fatalf("aggregator command lacks %q: %s", fragment, aggregator.Command)
		}
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
