package buildkite

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestEmitManagedNodeRuntimes(t *testing.T) {
	pipeline := Pipeline{
		CompilerStep:       "trusted-importer",
		DistributionDigest: testDigest("distribution"),
		NodeRuntimeDigests: map[int]string{24: testDigest("node 24"), 20: testDigest("node 20")},
		Jobs:               []Job{{Key: "job", Label: "Job", Queue: "queue", PlanDigest: testDigest("plan")}},
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
		t.Fatal("runtime emission was not deterministic")
	}
	var document struct {
		Steps []struct {
			Command string `yaml:"command"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(first, &document); err != nil {
		t.Fatal(err)
	}
	command := document.Steps[0].Command
	path20, _ := NodeRuntimePath(20, pipeline.NodeRuntimeDigests[20])
	path24, _ := NodeRuntimePath(24, pipeline.NodeRuntimeDigests[24])
	if strings.Index(command, path20) >= strings.Index(command, path24) {
		t.Fatalf("Node runtimes are not sorted by major:\n%s", first)
	}
	for major, path := range map[int]string{20: path20, 24: path24} {
		archive := "node" + fmt.Sprint(major) + "_archive"
		if !strings.Contains(command, "artifact download '"+path+`' "$bootstrap_dir" --step 'trusted-importer'`) ||
			!strings.Contains(command, `sha256sum "$`+archive+`"`) ||
			!strings.Contains(command, `gzip -dc "$`+archive+`" > "$node`+fmt.Sprint(major)+`"`) ||
			!strings.Contains(command, `chmod 0500 "$node`+fmt.Sprint(major)+`"`) ||
			!strings.Contains(command, `export BUILDKITE_GHA_NODE`+fmt.Sprint(major)+`="$node`+fmt.Sprint(major)+`"`) {
			t.Fatalf("Node %d bootstrap is incomplete:\n%s", major, first)
		}
	}
	if strings.Count(command, "--step 'trusted-importer'") != 4 {
		t.Fatalf("all downloads are not attributed to the importer:\n%s", first)
	}
}

func TestNodeRuntimePathValidation(t *testing.T) {
	digest := testDigest("node")
	path, err := NodeRuntimePath(24, digest)
	if err != nil || path != ".buildkite-gha/runtimes/node24/"+strings.TrimPrefix(digest, "sha256:")+"/node.gz" {
		t.Fatalf("NodeRuntimePath() = %q, %v", path, err)
	}
	for _, test := range []struct {
		major  int
		digest string
	}{
		{major: 18, digest: digest},
		{major: 24, digest: "sha256:nope"},
	} {
		if _, err := NodeRuntimePath(test.major, test.digest); err == nil {
			t.Fatalf("NodeRuntimePath(%d, %q) succeeded", test.major, test.digest)
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
		Env   map[string]string `yaml:"env"`
		Steps []struct {
			Key     string `yaml:"key"`
			Command string `yaml:"command"`
			If      string `yaml:"if"`
			Agents  struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatalf("parse default pipeline: %v", err)
	}
	if document.Env["MISE_JOBS"] != "1" {
		t.Fatalf("default pipeline MISE_JOBS = %q, want serial tool installation", document.Env["MISE_JOBS"])
	}
	if len(document.Steps) != 10 {
		t.Fatalf("default pipeline = %#v, want nine gated loaders plus repository checks", document.Steps)
	}
	steps := make(map[string]struct {
		command   string
		condition string
		queue     string
	}, len(document.Steps))
	for _, step := range document.Steps {
		steps[step.Key] = struct {
			command   string
			condition string
			queue     string
		}{step.Command, step.If, step.Agents.Queue}
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
	if got := steps["phase-2-upload-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-2-upload.yml" || got.condition != `build.env("PHASE2_PROBE") == "upload" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Phase 2 upload loader = %#v", got)
	}
	if got := steps["phase-3-upload-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-3-upload.yml" || got.condition != `build.env("PHASE3_PROBE") == "concurrent" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Phase 3 upload loader = %#v", got)
	}
	if got := steps["phase-4-upload-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-4-upload.yml" || got.condition != `build.env("PHASE4_PROBE") == "actions" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Phase 4 upload loader = %#v", got)
	}
	if got := steps["phase-5-capabilities-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-5-capabilities.yml" || got.condition != `build.env("PHASE5_PROBE") == "capabilities" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Phase 5 capability loader = %#v", got)
	}
	if got := steps["phase-5-docker-action-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-5-docker-action.yml" || got.condition != `build.env("PHASE5_PROBE") == "docker-action" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Phase 5 Dockerfile action loader = %#v", got)
	}
	if got := steps["phase-5-runtime-loader"]; got.command != "buildkite-agent pipeline upload .buildkite/phase-5-runtime.yml" || got.condition != `build.env("PHASE5_PROBE") == "runtime" && build.env("SMOKE_PROBE") != "hosted"` {
		t.Fatalf("Phase 5 container runtime loader = %#v", got)
	}
	hosted := steps["hosted-smoke-loader"]
	if hosted.condition != `build.env("SMOKE_PROBE") == "hosted"` || hosted.queue != "elastic-runners" {
		t.Fatalf("hosted smoke loader = %#v", hosted)
	}
	for _, required := range []string{
		`commit="$${SMOKE_COMMIT:?SMOKE_COMMIT is required}"`,
		`[[ "$$commit" =~ ^[0-9a-f]{40}$$ ]]`,
		`"$${PHASE2_COMMIT:-}"`,
		`"$${PHASE3_COMMIT:-}"`,
		`"$${PHASE4_COMMIT:-}"`,
		`"$${PHASE5_COMMIT:-}"`,
		`[[ -z "$$phase_commit" || "$$phase_commit" == "$$commit" ]]`,
		`phase-specific commit $$phase_commit conflicts with SMOKE_COMMIT`,
		`scripts/phase-0-shell-oracle-checkout "$$commit"`,
		`test "$${BUILDKITE_COMMIT:?BUILDKITE_COMMIT is required}" = "$$commit"`,
		`git status --porcelain --untracked-files=all`,
	} {
		if !strings.Contains(hosted.command, required) {
			t.Fatalf("hosted smoke loader lacks %q:\n%s", required, hosted.command)
		}
	}
	for _, fragment := range []string{"phase-2-upload.yml", "phase-3-upload.yml", "phase-4-upload.yml", "phase-5-capabilities.yml", "phase-5-docker-action.yml", "phase-5-runtime.yml", "hosted-smoke-control.yml"} {
		if count := strings.Count(hosted.command, "buildkite-agent pipeline upload .buildkite/"+fragment); count != 1 {
			t.Fatalf("hosted smoke loader uploads %s %d times:\n%s", fragment, count, hosted.command)
		}
	}
	if strings.Contains(hosted.command, "--replace") {
		t.Fatalf("hosted smoke loader uses replacement upload:\n%s", hosted.command)
	}
}

func TestPhase2UploadProofUsesPinnedUnprivilegedPath(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "phase-2-upload.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`commit="$${PHASE2_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/phase-0-shell-oracle-checkout "$$commit"`,
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
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
			Agents                 struct {
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
	if document.Steps[1].Key != "phase-2-continuation-loader" || document.Steps[1].DependsOn != "phase-2-upload-importer" || !document.Steps[1].AllowDependencyFailure || document.Steps[1].Agents.Queue != "elastic-runners" || document.Steps[1].Command != "buildkite-agent pipeline upload .buildkite/phase-2-upload-continuation.yml" {
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

func TestPhase3UploadProofPreservesSeparateContinuationLoader(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "phase-3-upload.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`commit="$${PHASE3_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/phase-0-shell-oracle-checkout "$$commit"`,
		`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
		`mise exec -- go build -trimpath -buildvcs=false`,
		`--event-path testdata/smoke/events/push.json`,
		`--runtime-queue hosted`,
		`testdata/smoke/.github/workflows/concurrent.yml`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Phase 3 upload proof lacks %q:\n%s", required, source)
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
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "phase-3-upload-importer" {
		t.Fatalf("Phase 3 upload proof = %#v", upload.Steps)
	}
	if upload.Steps[1].Key != "phase-3-continuation-loader" || upload.Steps[1].DependsOn != "phase-3-upload-importer" || !upload.Steps[1].AllowDependencyFailure || upload.Steps[1].Command != "buildkite-agent pipeline upload .buildkite/phase-3-upload-continuation.yml" {
		t.Fatalf("Phase 3 continuation loader = %#v", upload.Steps[1])
	}

	continuationSource, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "phase-3-upload-continuation.yml"))
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
	if len(continuation.Steps) != 1 || continuation.Steps[0].Key != "phase-3-native-after-concurrent" || len(continuation.Steps[0].DependsOn) != 1 {
		t.Fatalf("Phase 3 continuation = %#v", continuation.Steps)
	}
	dependency := continuation.Steps[0].DependsOn[0]
	if dependency.Step != "gha-observe" || !dependency.AllowFailure {
		t.Fatalf("Phase 3 continuation dependency = %#v", dependency)
	}
}

func TestPhase4UploadProofUsesTrustedManagedActionsPath(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "phase-4-upload.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	eventSource, err := os.ReadFile(filepath.Join("..", "..", "testdata", "phase4", "events", "public-checkout.json"))
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
	workflowSource, err := os.ReadFile(filepath.Join("..", "..", "testdata", "phase4", ".github", "workflows", "public-actions.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflowText := string(workflowSource)
	if publicEvent.Repository.Owner != "actions" || publicEvent.Repository.Name != "checkout" || !strings.Contains(workflowText, "uses: actions/checkout@"+publicEvent.SHA) || !strings.Contains(workflowText, `test "$(git rev-parse HEAD)" = "`+publicEvent.SHA+`"`) {
		t.Fatalf("Phase 4 public checkout identity drifted: event=%#v workflow=%s", publicEvent, workflowSource)
	}
	for _, required := range []string{
		`commit="$${PHASE4_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/phase-0-shell-oracle-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`,
		`version: "2026.5.12"`,
		`runtime_version="0.0.0-phase4.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`BUILDKITE_GHA_NODE20="$$(mise where node@20)/bin/node"`,
		`BUILDKITE_GHA_NODE24="$$(mise where node@24)/bin/node"`,
		`"$$distribution_root/buildkite-gha" upload`,
		`--event-path testdata/phase4/events/public-checkout.json`, `--runtime-queue hosted`,
		`testdata/phase4/.github/workflows/public-actions.yml`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Phase 4 upload proof lacks %q:\n%s", required, source)
		}
	}
	for _, forbidden := range []string{"go run", "--runtime ", "--runtime-version", "--node24", "--commit"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Phase 4 upload proof retains harness-only argument %q:\n%s", forbidden, source)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
			Agents                 struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "phase-4-upload-importer" || upload.Steps[0].Agents.Queue != "elastic-runners" {
		t.Fatalf("Phase 4 upload proof = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "phase-4-continuation-loader" || loader.DependsOn != "phase-4-upload-importer" || !loader.AllowDependencyFailure || loader.Agents.Queue != "elastic-runners" || loader.Command != "buildkite-agent pipeline upload .buildkite/phase-4-upload-continuation.yml" {
		t.Fatalf("Phase 4 continuation loader = %#v", loader)
	}
	continuationSource, err := os.ReadFile(filepath.Join("..", "..", ".buildkite", "phase-4-upload-continuation.yml"))
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
			Agents struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(continuationSource, &continuation); err != nil {
		t.Fatal(err)
	}
	if len(continuation.Steps) != 1 || continuation.Steps[0].Key != "phase-4-native-after-actions" || continuation.Steps[0].Agents.Queue != "elastic-runners" || len(continuation.Steps[0].DependsOn) != 1 || continuation.Steps[0].DependsOn[0].Step != "gha-public-actions" || !continuation.Steps[0].DependsOn[0].AllowFailure {
		t.Fatalf("Phase 4 continuation = %#v", continuation.Steps)
	}
}

func TestPhase5HostedDockerCapabilityProbeContract(t *testing.T) {
	root := filepath.Join("..", "..")
	type step struct {
		Key                    string `yaml:"key"`
		Command                string `yaml:"command"`
		DependsOn              any    `yaml:"depends_on"`
		AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
		Timeout                int    `yaml:"timeout_in_minutes"`
		Agents                 struct {
			Queue string `yaml:"queue"`
		} `yaml:"agents"`
		Retry struct {
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
	importerBody, importer := read("phase-5-capabilities.yml")
	if len(importer) != 2 || importer[0].Key != "phase-5-capabilities-importer" || importer[0].Agents.Queue != "elastic-runners" || importer[1].Key != "phase-5-continuation-loader" || importer[1].Agents.Queue != "elastic-runners" || fmt.Sprint(importer[1].DependsOn) != "phase-5-capabilities-importer" || !importer[1].AllowDependencyFailure || importer[1].Command != "buildkite-agent pipeline upload .buildkite/phase-5-capabilities-continuation.yml" {
		t.Fatalf("Phase 5 importer = %#v", importer)
	}
	for _, fragment := range []string{`commit="$${PHASE5_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/phase-0-shell-oracle-checkout "$$commit"`, `git status --porcelain --untracked-files=all`, `pipeline upload --no-interpolation --reject-secrets .buildkite/phase-5-hosted-probe.yml`} {
		if !strings.Contains(string(importerBody), fragment) {
			t.Fatalf("Phase 5 importer lacks %q", fragment)
		}
	}
	hostedBody, hosted := read("phase-5-hosted-probe.yml")
	if len(hosted) != 1 || hosted[0].Key != "phase-5-hosted-docker-probe" || hosted[0].Agents.Queue != "hosted" || hosted[0].Timeout != 15 || hosted[0].Retry.Automatic || hosted[0].Command != "scripts/phase-5-hosted-docker-probe" || !strings.Contains(string(hostedBody), "automatic: false") {
		t.Fatalf("Phase 5 hosted pipeline = %#v\n%s", hosted, hostedBody)
	}
	_, continuation := read("phase-5-capabilities-continuation.yml")
	if len(continuation) != 1 || continuation[0].Key != "phase-5-native-after-hosted-docker" || continuation[0].Agents.Queue != "elastic-runners" {
		t.Fatalf("Phase 5 continuation = %#v", continuation)
	}
	continuationBody, _ := os.ReadFile(filepath.Join(root, ".buildkite", "phase-5-capabilities-continuation.yml"))
	if !strings.Contains(string(continuationBody), `step: "phase-5-hosted-docker-probe"`) || !strings.Contains(string(continuationBody), `allow_failure: true`) {
		t.Fatalf("Phase 5 continuation dependency is not failure-tolerant: %s", continuationBody)
	}

	probe, err := os.ReadFile(filepath.Join(root, "scripts", "phase-5-hosted-docker-probe"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(probe)
	for _, fragment := range []string{"set -euo pipefail", `commit=${PHASE5_COMMIT:-${SMOKE_COMMIT:-}}`, `scripts/phase-0-shell-oracle-checkout "$commit"`, "busybox@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028", `buildx inspect default`, `buildx build --builder default --load`, `default-docker-driver`, `127.0.0.1::8080`, `wget -qO- http://phase5-server:8080/phase5-marker`, `/dev/tcp/127.0.0.1/$1`, `trap cleanup EXIT`, `trap - EXIT`, `exit "$final"`, `trap 'exit 130' INT`, `trap 'exit 143' TERM`, `docker container ls --all --quiet`, `--filter "label=${label_key}=${owner}"`, `timeout 30s`, `docker stop --time 5`, `term-observed`, `record bind-requested fail`, `record signal-stop fail`, `(( probe_failed == 0 ))`, "COPY marker"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Phase 5 probe lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"--privileged", "--network host", "/var/run/docker.sock", "docker prune", "docker system prune", "docker image prune", "docker container prune", "docker network prune", "printenv", " env"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Phase 5 probe contains forbidden %q", forbidden)
		}
	}
}

func TestPhase5DockerfileActionUploadProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	importerBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "phase-5-docker-action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(importerBody)
	for _, fragment := range []string{
		`commit="$${PHASE5_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/phase-0-shell-oracle-checkout "$$commit"`,
		`test -z "$$(git status --porcelain --untracked-files=all)"`,
		`automatic: false`,
		`cp testdata/phase5/.github/workflows/docker-action.yml.tmpl`,
		`runtime_version="0.0.0-phase5.$$commit"`,
		`go build -trimpath -buildvcs=false -ldflags "-X main.version=$$runtime_version"`,
		`BUILDKITE_GHA_NODE20="$$(mise where node@20)/bin/node"`,
		`BUILDKITE_GHA_NODE24="$$(mise where node@24)/bin/node"`,
		`"$$proof_root/buildkite-gha" upload`,
		`--runtime-queue hosted`,
		`"$$proof_root/.github/workflows/docker-action.yml"`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Phase 5 Dockerfile action proof lacks %q:\n%s", fragment, importerBody)
		}
	}
	var upload struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
			Agents                 struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Steps) != 2 || upload.Steps[0].Key != "phase-5-docker-action-importer" || upload.Steps[0].Agents.Queue != "elastic-runners" {
		t.Fatalf("Phase 5 Dockerfile action importer = %#v", upload.Steps)
	}
	loader := upload.Steps[1]
	if loader.Key != "phase-5-docker-action-continuation-loader" || loader.DependsOn != "phase-5-docker-action-importer" || !loader.AllowDependencyFailure || loader.Agents.Queue != "elastic-runners" || loader.Command != "buildkite-agent pipeline upload .buildkite/phase-5-docker-action-continuation.yml" {
		t.Fatalf("Phase 5 Dockerfile action continuation loader = %#v", loader)
	}

	continuationBody, err := os.ReadFile(filepath.Join(root, ".buildkite", "phase-5-docker-action-continuation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(continuationBody), `step: "gha-docker-action"`) || !strings.Contains(string(continuationBody), `allow_failure: true`) {
		t.Fatalf("Phase 5 Dockerfile action continuation is not failure-tolerant: %s", continuationBody)
	}

	templateBody, err := os.ReadFile(filepath.Join(root, "testdata", "phase5", ".github", "workflows", "docker-action.yml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	githubBody, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "phase-5-docker-action-oracle.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const action = "uses: actions/hello-world-docker-action@66e612e94eca3366d470e868d0c2d86bd25e693d"
	observation := `PHASE5_DOCKER_OBSERVATION={"container":"ran","output":"propagated","source":"public-exact"}`
	if !strings.Contains(string(templateBody), action) || !strings.Contains(string(githubBody), action) || !strings.Contains(string(templateBody), observation) || !strings.Contains(string(githubBody), observation) {
		t.Fatalf("Phase 5 Dockerfile differential fixtures drifted:\ntemplate:\n%s\nGitHub:\n%s", templateBody, githubBody)
	}
}

func TestPhase5ContainerRuntimeProofContract(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(name string) []byte {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, ".buildkite", name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	importerBody := read("phase-5-runtime.yml")
	var importer struct {
		Steps []struct {
			Key                    string `yaml:"key"`
			Command                string `yaml:"command"`
			DependsOn              string `yaml:"depends_on"`
			AllowDependencyFailure bool   `yaml:"allow_dependency_failure"`
			Agents                 struct {
				Queue string `yaml:"queue"`
			} `yaml:"agents"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(importerBody, &importer); err != nil {
		t.Fatal(err)
	}
	if len(importer.Steps) != 2 || importer.Steps[0].Key != "phase-5-runtime-importer" || importer.Steps[0].Agents.Queue != "elastic-runners" {
		t.Fatalf("Phase 5 runtime importer = %#v", importer.Steps)
	}
	loader := importer.Steps[1]
	if loader.Key != "phase-5-runtime-continuation-loader" || loader.DependsOn != "phase-5-runtime-importer" || !loader.AllowDependencyFailure || loader.Agents.Queue != "elastic-runners" || loader.Command != "buildkite-agent pipeline upload .buildkite/phase-5-runtime-continuation.yml" {
		t.Fatalf("Phase 5 runtime continuation loader = %#v", loader)
	}
	for _, fragment := range []string{
		`commit="$${PHASE5_COMMIT:-$${SMOKE_COMMIT:-}}"`,
		`scripts/phase-0-shell-oracle-checkout "$$commit"`,
		`git status --porcelain --untracked-files=all`,
		`pipeline upload --no-interpolation --reject-secrets .buildkite/phase-5-runtime-probe.yml`,
		`automatic: false`,
	} {
		if !strings.Contains(string(importerBody), fragment) {
			t.Fatalf("Phase 5 runtime importer lacks %q:\n%s", fragment, importerBody)
		}
	}

	probeBody := read("phase-5-runtime-probe.yml")
	for _, fragment := range []string{`key: "phase-5-hosted-runtime-probe"`, `queue: "hosted"`, `timeout_in_minutes: 25`, `automatic: false`, `mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2`, `command: "scripts/phase-5-hosted-runtime-probe"`} {
		if !strings.Contains(string(probeBody), fragment) {
			t.Fatalf("Phase 5 runtime probe pipeline lacks %q:\n%s", fragment, probeBody)
		}
	}
	continuationBody := read("phase-5-runtime-continuation.yml")
	for _, fragment := range []string{`key: "phase-5-native-after-runtime"`, `step: "phase-5-hosted-runtime-probe"`, `allow_failure: true`, `queue: "elastic-runners"`} {
		if !strings.Contains(string(continuationBody), fragment) {
			t.Fatalf("Phase 5 runtime continuation lacks %q:\n%s", fragment, continuationBody)
		}
	}

	script, err := os.ReadFile(filepath.Join(root, "scripts", "phase-5-hosted-runtime-probe"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, fragment := range []string{
		"set -euo pipefail",
		`commit=${PHASE5_COMMIT:-${SMOKE_COMMIT:-}}`,
		`scripts/phase-0-shell-oracle-checkout "$commit"`,
		`git status --porcelain --untracked-files=all`,
		`docker buildx inspect default`,
		`CGO_ENABLED=0 mise exec -- go build`,
		`BUILDKITE_GHA_LIVE_REQUIRED=1`,
		`BUILDKITE_GHA_TEST_RUNTIME="$runtime"`,
		`BUILDKITE_GHA_TEST_NODE24="$node24"`,
		`go test ./internal/runtime -count=1 -timeout=20m`,
		`TestLivePhase5CompiledContainerRuntime`,
		`TestLivePhase5ManifestContainerFixtures`,
		`TestLivePhase5UnhealthyServiceDiagnostics`,
		`--- SKIP:`,
		`PHASE5_RUNTIME_OBSERVATION=`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("Phase 5 runtime probe script lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"--privileged", "--network host", "/var/run/docker.sock", "docker prune", "docker system prune", "printenv"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Phase 5 runtime probe contains forbidden %q", forbidden)
		}
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
		Agents    struct {
			Queue string `yaml:"queue"`
		} `yaml:"agents"`
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
	if control.Key != "hosted-smoke-aggregator-loader" || control.Agents.Queue != "elastic-runners" || control.Command != "buildkite-agent pipeline upload .buildkite/hosted-smoke-aggregator.yml" {
		t.Fatalf("control step = %#v", control)
	}
	assertSet("control", control.DependsOn, map[string]bool{"phase-2-continuation-loader": true, "phase-3-continuation-loader": true, "phase-4-continuation-loader": true, "phase-5-continuation-loader": true, "phase-5-docker-action-continuation-loader": true, "phase-5-runtime-continuation-loader": true})
	generated := map[string]bool{}
	for _, name := range []string{"phase-2-upload-continuation.yml", "phase-3-upload-continuation.yml", "phase-4-upload-continuation.yml", "phase-5-capabilities-continuation.yml", "phase-5-docker-action-continuation.yml", "phase-5-runtime-continuation.yml"} {
		continuation := read(name)
		for _, dependency := range continuation.DependsOn {
			generated[dependency.Step] = true
		}
	}
	want := map[string]bool{}
	for key := range generated {
		want[key] = true
	}
	for _, key := range []string{"gha-producer", "gha-concurrent", "phase-2-native-after-shell", "phase-3-native-after-concurrent", "phase-4-native-after-actions", "phase-5-native-after-hosted-docker", "phase-5-native-after-docker-action", "phase-5-native-after-runtime"} {
		want[key] = true
	}
	aggregator := read("hosted-smoke-aggregator.yml")
	if aggregator.Key != "hosted-smoke-terminal" || aggregator.Agents.Queue != "elastic-runners" {
		t.Fatalf("aggregator step = %#v", aggregator)
	}
	assertSet("aggregator", aggregator.DependsOn, want)
	for key := range generated {
		if !strings.Contains(aggregator.Command, key) {
			t.Fatalf("aggregator command does not inspect generated step %q: %s", key, aggregator.Command)
		}
	}
	for _, key := range []string{"gha-producer", "gha-concurrent", "phase-2-native-after-shell", "phase-3-native-after-concurrent", "phase-4-native-after-actions", "phase-5-native-after-hosted-docker", "phase-5-native-after-docker-action", "phase-5-native-after-runtime"} {
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
