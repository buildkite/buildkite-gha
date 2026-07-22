package compiler

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestCompileShellGoldenGraph(t *testing.T) {
	workflowPath := smokePath(".github", "workflows", "shell.yml")
	workflowSource := readFile(t, workflowPath)
	eventSource := readFile(t, smokePath("events", "push.json"))

	got, err := Compile(workflowPath, workflowSource, eventSource)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}

	type goldenJob struct {
		Key    string         `json:"key"`
		Job    string         `json:"job"`
		Matrix map[string]any `json:"matrix,omitempty"`
		Needs  []string       `json:"needs,omitempty"`
	}
	golden := make([]goldenJob, 0, len(ir.Jobs))
	for _, job := range ir.Jobs {
		golden = append(golden, goldenJob{Key: job.Key, Job: job.LogicalJobID, Matrix: job.Matrix, Needs: job.Needs})
	}
	want := `[{"key":"gha-producer","job":"producer"},{"key":"gha-consumer-5ebbc197d87b","job":"consumer","matrix":{"variant":"one"},"needs":["gha-producer"]},{"key":"gha-consumer-91934b28b00f","job":"consumer","matrix":{"variant":"two"},"needs":["gha-producer"]}]`
	encoded, err := json.Marshal(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("compiled graph changed\nwant:\n%s\n\ngot:\n%s", want, encoded)
	}
}

func TestCompileIsByteIdenticalAndCoversSmokeCorpus(t *testing.T) {
	eventSource := readFile(t, smokePath("events", "push.json"))
	for _, name := range []string{"shell.yml", "ci.yml", "artifact.yml"} {
		t.Run(name, func(t *testing.T) {
			path := smokePath(".github", "workflows", name)
			source := readFile(t, path)
			first, err := Compile(path, source, eventSource)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Compile(path, source, eventSource)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("repeated compilation was not byte-identical")
			}
		})
	}
}

func TestCompileExpandsMatrixIncludeExcludeAndDependencies(t *testing.T) {
	source := []byte(`name: matrix conformance
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        fruit: [apple, pear]
        animal: [cat, dog]
        exclude:
          - fruit: pear
            animal: dog
        include:
          - color: green
          - color: pink
            animal: cat
          - fruit: banana
          - fruit: banana
            animal: cat
    steps:
      - run: true
  report:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	got, err := Compile("matrix.yml", source, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 6 {
		t.Fatalf("jobs = %d, want five matrix instances and one dependent", len(ir.Jobs))
	}
	wantMatrices := []map[string]any{
		{"animal": "cat", "color": "pink", "fruit": "apple"},
		{"animal": "dog", "color": "green", "fruit": "apple"},
		{"animal": "cat", "color": "pink", "fruit": "pear"},
		{"fruit": "banana"},
		{"animal": "cat", "fruit": "banana"},
	}
	for i, want := range wantMatrices {
		if !reflect.DeepEqual(ir.Jobs[i].Matrix, want) {
			t.Fatalf("matrix instance %d = %#v, want %#v", i, ir.Jobs[i].Matrix, want)
		}
	}
	report := ir.Jobs[5]
	if report.LogicalJobID != "report" || len(report.Needs) != len(wantMatrices) {
		t.Fatalf("dependent job = %#v, want dependency on all matrix instances", report)
	}
	wantNeeds := make([]string, len(wantMatrices))
	for i := range wantMatrices {
		wantNeeds[i] = ir.Jobs[i].Key
	}
	sort.Strings(wantNeeds)
	for i, need := range report.Needs {
		if need != wantNeeds[i] {
			t.Fatalf("dependency %d = %q, want %q", i, need, wantNeeds[i])
		}
	}
}

func TestCompileRejectsRuntimeDependentMatrixInclude(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [ubuntu-latest]
        include: ${{ fromJSON(vars.INCLUDE) }}
    steps:
      - run: true
`)
	_, err := Compile("dynamic-include.yml", source, readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), "runtime-dependent matrix include expressions are unsupported") {
		t.Fatalf("Compile() error = %v, want explicit include expression error", err)
	}
	if !strings.Contains(err.Error(), "dynamic-include.yml:8:18") {
		t.Fatalf("Compile() error = %v, want source location", err)
	}
}

func TestCompileExpandsIncludeOnlyMatrixAsDistinctJobs(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - fruit: banana
          - fruit: apple
            color: green
    steps:
      - run: true
`)
	got, err := Compile("include-only.yml", source, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"fruit": "banana"}, {"color": "green", "fruit": "apple"}}
	if len(ir.Jobs) != len(want) {
		t.Fatalf("jobs = %d, want %d", len(ir.Jobs), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(ir.Jobs[i].Matrix, want[i]) {
			t.Fatalf("matrix instance %d = %#v, want %#v", i, ir.Jobs[i].Matrix, want[i])
		}
	}
}

func TestCompilePreservesStaticMatrixScalarTypes(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: [12, "14"]
        experimental: [true, false]
        exclude:
          - version: 12
            experimental: false
        include:
          - version: 16
            experimental: false
    steps:
      - run: true
`)
	got, err := Compile("typed-matrix.yml", source, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"experimental": true, "version": float64(12)},
		{"experimental": true, "version": "14"},
		{"experimental": false, "version": "14"},
		{"experimental": false, "version": float64(16)},
	}
	if len(ir.Jobs) != len(want) {
		t.Fatalf("jobs = %d, want %d", len(ir.Jobs), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(ir.Jobs[i].Matrix, want[i]) {
			t.Fatalf("matrix instance %d = %#v, want %#v", i, ir.Jobs[i].Matrix, want[i])
		}
	}
}

func TestCompileRejectsRuntimeDependentGraphExpression(t *testing.T) {
	source := []byte(`name: dynamic
on: push
jobs:
  generated:
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}
    steps:
      - run: true
`)
	_, err := Compile("dynamic.yml", source, readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), "runtime-dependent matrix expressions are unsupported") {
		t.Fatalf("Compile() error = %v, want explicit runtime-dependent matrix error", err)
	}
	if !strings.Contains(err.Error(), "dynamic.yml:7:15") {
		t.Fatalf("Compile() error = %v, want source location", err)
	}
}

func TestCompilePlansOwnsDeterministicRuntimeInputs(t *testing.T) {
	path := smokePath(".github", "workflows", "plan-fixture.yml")
	source := []byte(`name: plan fixture
on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      result: ${{ steps.composite.outputs.result }}
    steps:
      - run: echo "result=smoke" >> "$GITHUB_OUTPUT"
      - id: step-1
        run: true
      - id: javascript
        uses: ./.github/actions/javascript
        with:
          message: ${{ steps.step-1.outputs.result }}
      - id: composite
        uses: ./.github/actions/composite
        with:
          message: ${{ steps.javascript.outputs.result }}
`)
	plans, err := CompilePlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	producer := plans[0]
	if producer.Target.StepKey != "gha-producer" || producer.Workflow.LogicalJobID != "producer" {
		t.Fatalf("producer target = %#v, workflow = %#v", producer.Target, producer.Workflow)
	}
	if producer.Steps[0].ID != "step-1-2" || producer.Steps[1].ID != "step-1" {
		t.Fatalf("deterministic step ids = %q, %q", producer.Steps[0].ID, producer.Steps[1].ID)
	}
	if producer.Steps[2].With["message"] != "${{ steps.step-1.outputs.result }}" {
		t.Fatalf("JavaScript inputs = %#v", producer.Steps[2].With)
	}
	if producer.Outputs["result"] != "${{ steps.composite.outputs.result }}" {
		t.Fatalf("job outputs = %#v", producer.Outputs)
	}
	if producer.RequiredCapabilities == nil || len(producer.RequiredCapabilities) != 0 {
		t.Fatalf("required capabilities = %#v, want concrete empty array", producer.RequiredCapabilities)
	}
	second, err := CompilePlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(plans)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("repeated plan compilation was not byte-identical")
	}
}

func TestCompilePlansRejectsNeedsWithoutRuntimeManifestInjection(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	_, err := CompilePlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err == nil || !strings.Contains(err.Error(), "jobs with needs are unsupported until producer result manifests can be injected at runtime") {
		t.Fatalf("CompilePlans() error = %v, want needs boundary", err)
	}
}

func TestCompilePlansDerivesDockerCapability(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "docker.yml")
	actionDir := filepath.Join(repository, ".github", "actions", "docker")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("runs:\n  using: docker\n  image: Dockerfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := []byte("on: push\njobs:\n  docker:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/docker\n")
	plans, err := CompilePlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if got := plans[0].RequiredCapabilities; len(got) != 1 || got[0] != "docker" {
		t.Fatalf("required capabilities = %#v, want [docker]", got)
	}
}

func TestCompilePlansRejectsNode20LocalAction(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "node20.yml")
	actionDir := filepath.Join(repository, ".github", "actions", "node20")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("runs:\n  using: node20\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := []byte("on: push\njobs:\n  node20:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/node20\n")
	_, err := CompilePlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err == nil || !strings.Contains(err.Error(), `uses unsupported runtime "node20"`) {
		t.Fatalf("CompilePlans() error = %v, want node20 fail-closed boundary", err)
	}
}

func TestCompilePlansRejectsRuntimeInvalidActionMetadata(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "invalid.yml")
	actionDir := filepath.Join(repository, ".github", "actions", "invalid")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("unexpected: true\nruns:\n  using: node24\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := []byte("on: push\njobs:\n  invalid:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/invalid\n")
	_, err := CompilePlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("CompilePlans() error = %v, want strict action metadata rejection", err)
	}
}

func TestLocalActionCapabilityResolutionStaysWithinRepository(t *testing.T) {
	repository := t.TempDir()
	workflowDir := filepath.Join(repository, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(workflowDir, "escape.yml")
	_, err := loadLocalAction(workflowPath, "./../../outside")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("loadLocalAction() error = %v, want repository escape error", err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "action.yml"), []byte("runs:\n  using: node24\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	actionsDir := filepath.Join(repository, ".github", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(actionsDir, "escaped")); err != nil {
		t.Fatal(err)
	}
	_, err = loadLocalAction(workflowPath, "./.github/actions/escaped")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("loadLocalAction() symlink error = %v, want repository escape error", err)
	}
	metadataEscape := filepath.Join(actionsDir, "metadata-escaped")
	if err := os.MkdirAll(metadataEscape, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "action.yml"), filepath.Join(metadataEscape, "action.yml")); err != nil {
		t.Fatal(err)
	}
	_, err = loadLocalAction(workflowPath, "./.github/actions/metadata-escaped")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("loadLocalAction() metadata symlink error = %v, want repository escape error", err)
	}
}

func TestCompiledPlansValidateAgainstVersionedSchema(t *testing.T) {
	path := smokePath(".github", "workflows", "schema-fixture.yml")
	source := []byte("on: push\njobs:\n  schema:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	plans, err := CompilePlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err != nil {
		t.Fatal(err)
	}

	schemaSource := readFile(t, filepath.Join("..", "..", "schemas", "job-plan-v1.schema.json"))
	var schemaDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(plan.Schema, schemaDocument); err != nil {
		t.Fatal(err)
	}
	jobSchema, err := compiler.Compile(plan.Schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range plans {
		encoded, err := plan.Encode(job)
		if err != nil {
			t.Fatal(err)
		}
		var document any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		if err := jobSchema.Validate(document); err != nil {
			t.Fatalf("compiled plan does not validate against %s: %v\n%s", plan.Schema, err, encoded)
		}
	}
}

func TestCompileRejectsDuplicateSanitizedInstanceKeys(t *testing.T) {
	source := []byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n        variant: [same, same]\n    steps:\n      - run: true\n")
	_, err := Compile("collision.yml", source, readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `deterministic instance key "gha-build-`) || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("Compile() error = %v, want deterministic key collision", err)
	}
}

func TestInstanceKeyReportsMatrixCanonicalizationErrors(t *testing.T) {
	_, err := instanceKey("matrix", map[string]any{"unsupported": make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "canonicalize matrix") {
		t.Fatalf("instanceKey() error = %v, want canonicalization error", err)
	}
}

func TestCompileRejectsInvalidEventSnapshots(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	valid := string(readFile(t, smokePath("events", "push.json")))
	tests := []struct {
		name   string
		event  string
		wanted string
	}{
		{name: "unknown field", event: strings.Replace(valid, `"actor":`, `"unexpected":true,"actor":`, 1), wanted: "unknown field"},
		{name: "trailing value", event: valid + `{}`, wanted: "multiple JSON values"},
		{name: "missing ref", event: strings.Replace(valid, `"ref": "refs/heads/main"`, `"ref": ""`, 1), wanted: "ref, sha, and actor"},
		{name: "missing actor", event: strings.Replace(valid, `"actor": "buildkite-gha-smoke"`, `"actor": ""`, 1), wanted: "ref, sha, and actor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Compile("event.yml", workflow, []byte(test.event))
			if err == nil || !strings.Contains(err.Error(), test.wanted) {
				t.Fatalf("Compile() error = %v, want %q", err, test.wanted)
			}
		})
	}
}

func smokePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "testdata", "smoke"}, parts...)...)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return source
}
