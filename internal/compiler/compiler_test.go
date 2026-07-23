package compiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
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
	for _, name := range []string{"shell.yml", "concurrent.yml", "ci.yml", "artifact.yml"} {
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

func TestCompileMatchesStaticIncludeExcludeAgainstExpressionNumbers(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: ${{ fromJSON(vars.VERSIONS) }}
        exclude:
          - version: 12
        include:
          - version: 14.0
            experimental: true
    steps:
      - run: true
`)
	compiled, err := CompileWithOptions("numeric-matrix.yml", source, readFile(t, smokePath("events", "push.json")), Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"VERSIONS": `[12,14]`}},
		Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "linux"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || ir.Jobs[0].Matrix["version"] != float64(14) || ir.Jobs[0].Matrix["experimental"] != true {
		t.Fatalf("numeric matrix jobs = %#v, want one included version 14 job", ir.Jobs)
	}
}

func TestCompileRejectsMatrixThatExcludesEveryCombination(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: [12]
        exclude:
          - version: 12
    steps:
      - run: true
  report:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	_, err := Compile("empty-matrix.yml", source, readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), "matrix excludes every combination") {
		t.Fatalf("Compile() error = %v, want empty matrix rejection", err)
	}
}

func TestCompileExpandsLocalReusableWorkflowWithNamespacesAndDependencies(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    steps:
      - run: prepare
  delegated:
    name: delegated tests
    needs: prepare
    uses: ./.github/workflows/reusable.yml
    with:
      message: hello
      enabled: true
  finish:
    needs: delegated
    runs-on: ubuntu-latest
    steps:
      - run: finish
`)
	calleePath := writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      message:
        type: string
        required: true
      enabled:
        type: boolean
        required: true
jobs:
  first:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ inputs.message }} ${{ inputs.enabled }}"
  second:
    needs: first
    runs-on: ubuntu-latest
    steps:
      - run: second
`)
	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 4 {
		t.Fatalf("jobs = %d, want 4", len(ir.Jobs))
	}
	byID := make(map[string]JobInstance, len(ir.Jobs))
	for _, job := range ir.Jobs {
		byID[job.LogicalJobID] = job
	}
	prepare := byID["prepare"]
	first := byID["delegated.first"]
	second := byID["delegated.second"]
	finish := byID["finish"]
	if !reflect.DeepEqual(first.Needs, []string{prepare.Key}) || !reflect.DeepEqual(first.LogicalNeeds, []string{"prepare"}) {
		t.Fatalf("callee root needs = %#v / %#v, want caller prerequisite", first.Needs, first.LogicalNeeds)
	}
	if !reflect.DeepEqual(second.Needs, []string{first.Key}) || !reflect.DeepEqual(finish.Needs, []string{second.Key}) {
		t.Fatalf("callee/caller dependencies = %#v / %#v", second.Needs, finish.Needs)
	}
	if first.Key != "gha-delegated-first" || first.Label != "delegated tests / first" {
		t.Fatalf("callee identity = key %q label %q", first.Key, first.Label)
	}
	calleeDigest := sha256.Sum256(readFile(t, calleePath))
	wantCalleeDigest := "sha256:" + hex.EncodeToString(calleeDigest[:])
	if first.SourcePath != "./.github/workflows/reusable.yml" || first.SourceDigest != wantCalleeDigest || first.Steps[0].Run != `echo "hello true"` {
		t.Fatalf("callee provenance/input = %q / %q / %q, want repository-relative path, digest, and statically substituted input", first.SourcePath, first.SourceDigest, first.Steps[0].Run)
	}
	if prepare.SourcePath != "./.github/workflows/caller.yml" || finish.SourcePath != "./.github/workflows/caller.yml" {
		t.Fatalf("caller source provenance = %q / %q", prepare.SourcePath, finish.SourcePath)
	}
}

func TestCompileExpandsMatrixReusableCallsDeterministically(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    strategy:
      matrix:
        target: [ubuntu-22.04, ubuntu-24.04]
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ matrix.target }}
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  test:
    name: test ${{ inputs.target }}
    runs-on: ${{ inputs.target }}
    steps:
      - run: echo ${{ inputs.target }}
`)

	first, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("reusable-workflow matrix compilation was not byte-identical")
	}
	var ir IR
	if err := json.Unmarshal(first, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 2 || ir.Jobs[0].LogicalJobID == ir.Jobs[1].LogicalJobID || ir.Jobs[0].Key == ir.Jobs[1].Key {
		t.Fatalf("matrix reusable jobs = %#v, want two namespaced identities", ir.Jobs)
	}
	runs := []string{ir.Jobs[0].Steps[0].Run, ir.Jobs[1].Steps[0].Run}
	sort.Strings(runs)
	if !reflect.DeepEqual(runs, []string{"echo ubuntu-22.04", "echo ubuntu-24.04"}) {
		t.Fatalf("statically resolved matrix inputs = %#v", runs)
	}
	runners := []string{ir.Jobs[0].RunsOn[0], ir.Jobs[1].RunsOn[0]}
	sort.Strings(runners)
	if !reflect.DeepEqual(runners, []string{"ubuntu-22.04", "ubuntu-24.04"}) {
		t.Fatalf("statically resolved runs-on inputs = %#v", runners)
	}
	plans, err := CompilePlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Workflow.Path != "./.github/workflows/reusable.yml" || plans[0].Workflow.Digest != ir.Jobs[0].SourceDigest {
		t.Fatalf("callee plan provenance = %#v, want callee path and digest", plans)
	}
}

func TestCompilePlansResolveReusableLocalActionsFromRepositoryRoot(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  delegated:\n    uses: ./.github/workflows/reusable.yml\n")
	writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/docker\n")
	actionDir := filepath.Join(repository, ".github", "actions", "docker")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("runs:\n  using: docker\n  image: Dockerfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plans, err := CompilePlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"docker"}) || plans[0].Workflow.Path != "./.github/workflows/reusable.yml" {
		t.Fatalf("reusable local-action plan = %#v", plans)
	}
}

func TestCompileRejectsUnsafeOrDynamicReusableWorkflowCalls(t *testing.T) {
	t.Run("input workflow symlink escape", func(t *testing.T) {
		repository := t.TempDir()
		workflowDir := filepath.Join(repository, ".github", "workflows")
		if err := os.MkdirAll(workflowDir, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.yml")
		if err := os.WriteFile(outside, []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(workflowDir, "escaped.yml")
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
			t.Fatalf("Compile() error = %v, want input workflow symlink confinement rejection", err)
		}
	})

	t.Run("remote", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: owner/repository/.github/workflows/reusable.yml@main\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "only repository-local ./ paths are supported") || !strings.Contains(err.Error(), "./.github/workflows/caller.yml:4:11") {
			t.Fatalf("Compile() error = %v, want source-located remote rejection", err)
		}
	})

	t.Run("runtime input", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ needs.prepare.outputs.target }}
`)
		writeWorkflow(t, repository, "reusable.yml", "on:\n  workflow_call:\n    inputs:\n      target:\n        type: string\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), `input "target" is not statically resolvable`) || !strings.Contains(err.Error(), "./.github/workflows/caller.yml:6:15") {
			t.Fatalf("Compile() error = %v, want source-located runtime input rejection", err)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/escaped.yml\n")
		outside := filepath.Join(t.TempDir(), "outside.yml")
		if err := os.WriteFile(outside, []byte("on: workflow_call\njobs: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(repository, ".github", "workflows", "escaped.yml")); err != nil {
			t.Fatal(err)
		}
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
			t.Fatalf("Compile() error = %v, want symlink confinement rejection", err)
		}
	})
}

func TestCompileRejectsReusableWorkflowCyclesAndDepthOverflow(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "a.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/b.yml\n")
		writeWorkflow(t, repository, "b.yml", "on: workflow_call\njobs:\n  call:\n    uses: ./.github/workflows/a.yml\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "reusable-workflow cycle detected") || !strings.Contains(err.Error(), "a.yml -> .github/workflows/b.yml -> .github/workflows/a.yml") {
			t.Fatalf("Compile() error = %v, want explicit cycle", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		repository := t.TempDir()
		for i := 0; i <= maxReusableWorkflowDepth; i++ {
			on := "workflow_call"
			if i == 0 {
				on = "push"
			}
			writeWorkflow(t, repository, fmt.Sprintf("%d.yml", i), fmt.Sprintf("on: %s\njobs:\n  call:\n    uses: ./.github/workflows/%d.yml\n", on, i+1))
		}
		path := filepath.Join(repository, ".github", "workflows", "0.yml")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds maximum depth %d", maxReusableWorkflowDepth)) {
			t.Fatalf("Compile() error = %v, want explicit depth limit", err)
		}
	})
}

func TestCompileResolvesInputsBeforeNestedReusableCallMatrices(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  middle:
    uses: ./.github/workflows/middle.yml
    with:
      target: linux
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  leaf:
    name: leaf ${{ inputs.target }}
    strategy:
      matrix:
        target: ["${{ inputs.target }}", arm]
    uses: ./.github/workflows/leaf.yml
    with:
      target: ${{ matrix.target }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.target }}
`)

	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2 nested matrix instances", len(ir.Jobs))
	}
	runs := []string{ir.Jobs[0].Steps[0].Run, ir.Jobs[1].Steps[0].Run}
	sort.Strings(runs)
	if !reflect.DeepEqual(runs, []string{"echo arm", "echo linux"}) {
		t.Fatalf("nested static inputs = %#v", runs)
	}
	for _, job := range ir.Jobs {
		if !strings.HasPrefix(job.Label, "middle / leaf linux") {
			t.Fatalf("nested label = %q, want substituted caller input", job.Label)
		}
	}
}

func TestCompileRejectsRuntimeExpressionsLaunderedThroughReusableInputs(t *testing.T) {
	t.Run("matrix expression", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    strategy:\n      matrix: ${{ fromJSON(vars.MATRIX) }}\n    uses: ./.github/workflows/reusable.yml\n")
		writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "expression-valued reusable-workflow matrices are unsupported") {
			t.Fatalf("Compile() error = %v, want explicit call-matrix rejection", err)
		}
	})

	t.Run("matrix value", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    strategy:
      matrix:
        target: ["${{ github.ref_name }}"]
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ matrix.target }}
`)
		writeWorkflow(t, repository, "reusable.yml", "on:\n  workflow_call:\n    inputs:\n      target:\n        type: string\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ inputs.target }}\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "runtime-dependent reusable-workflow matrix value is unsupported") {
			t.Fatalf("Compile() error = %v, want matrix expression rejection", err)
		}
	})

	t.Run("callee body", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    with:\n      enabled: true\n")
		writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      enabled:
        type: boolean
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.enabled && 'yes' || 'no' }}
`)
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "reusable-workflow input expression is not statically resolvable") {
			t.Fatalf("Compile() error = %v, want complex input rejection", err)
		}
	})
}

func TestCompileRejectsFlattenedReusableJobIDCollisions(t *testing.T) {
	repository := t.TempDir()
	suffix, err := matrixDigest(map[string]any{"target": "linux"})
	if err != nil {
		t.Fatal(err)
	}
	path := writeWorkflow(t, repository, "caller.yml", fmt.Sprintf(`on: push
jobs:
  call:
    strategy:
      matrix:
        target: [linux, arm]
    uses: ./.github/workflows/reusable.yml
  call-%s:
    uses: ./.github/workflows/reusable.yml
`, suffix))
	writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	_, err = Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), "flattened job id") || !strings.Contains(err.Error(), "collides with another job") {
		t.Fatalf("Compile() error = %v, want fail-closed flattened ID collision", err)
	}
}

func TestCompileBoundsReusableWorkflowGraphExpansion(t *testing.T) {
	repository := t.TempDir()
	values := make([]string, maxMatrixInstances)
	for i := range values {
		values[i] = fmt.Sprint(i)
	}
	path := writeWorkflow(t, repository, "caller.yml", fmt.Sprintf("on: push\njobs:\n  call:\n    strategy:\n      matrix:\n        value: [%s]\n    uses: ./.github/workflows/reusable.yml\n", strings.Join(values, ", ")))
	var callee strings.Builder
	callee.WriteString("on: workflow_call\njobs:\n")
	for i := 0; i < 5; i++ {
		_, _ = fmt.Fprintf(&callee, "  job%d:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n", i)
	}
	writeWorkflow(t, repository, "reusable.yml", callee.String())
	_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("graph expands beyond %d jobs", maxFlattenedJobs)) {
		t.Fatalf("Compile() error = %v, want bounded graph rejection", err)
	}
}

func TestCompileRejectsRequiredReusableWorkflowSecrets(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n")
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    secrets:
      token:
        required: true
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `requires unsupported secret "token"`) {
		t.Fatalf("Compile() error = %v, want required secret rejection", err)
	}
}

func TestCompileReusableInputDiagnosticsAreDeterministic(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    with:\n      zed: z\n      alpha: a\n")
	writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	for i := 0; i < 20; i++ {
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), `input "alpha" is not declared`) {
			t.Fatalf("Compile() error = %v, want alphabetically first input diagnostic", err)
		}
	}
}

func writeWorkflow(t *testing.T, repository, name, source string) string {
	t.Helper()
	path := filepath.Join(repository, ".github", "workflows", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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
	plans, err := CompilePlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
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
	second, err := CompilePlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
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

func TestCompilePlansOwnsConcurrentStepControls(t *testing.T) {
	path := smokePath(".github", "workflows", "concurrent.yml")
	source := []byte(`name: concurrent plan
on: push
jobs:
  concurrent:
    runs-on: ubuntu-latest
    steps:
      - id: first
        run: echo first
        background: true
      - id: second
        run: echo second
        background: true
      - wait: [first, second]
        continue-on-error: true
      - wait-all:
      - cancel: first
      - parallel:
          - run: echo ${{ secrets.PARALLEL_TOKEN }}
            env:
              PARALLEL_SECRET: ${{ secrets.PARALLEL_TOKEN }}
          - id: named-parallel
            run: echo named
`)
	plans, err := CompilePlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.SchemaV2 {
		t.Fatalf("plans = %#v, want one v2 plan", plans)
	}
	steps := plans[0].Steps
	if len(steps) != 8 || !steps[0].Background || !steps[1].Background {
		t.Fatalf("background plan steps = %#v", steps)
	}
	if steps[2].Kind != "wait" || !reflect.DeepEqual(steps[2].Targets, []string{"first", "second"}) || !steps[2].ContinueOnError {
		t.Fatalf("targeted plan barrier = %#v", steps[2])
	}
	if steps[3].Kind != "wait-all" || steps[4].Kind != "cancel" || !reflect.DeepEqual(steps[4].Targets, []string{"first"}) {
		t.Fatalf("plan controls = %#v", steps[3:])
	}
	if !steps[5].Background || !steps[6].Background || steps[5].ID == "" || steps[6].ID != "named-parallel" {
		t.Fatalf("lowered parallel members = %#v", steps[5:7])
	}
	if steps[7].Kind != "wait" || !reflect.DeepEqual(steps[7].Targets, []string{steps[5].ID, "named-parallel"}) {
		t.Fatalf("lowered parallel barrier = %#v", steps[7])
	}
	if !reflect.DeepEqual(plans[0].RequiredSecrets, []string{"PARALLEL_TOKEN"}) || !plans[0].HasCapability("secrets") {
		t.Fatalf("parallel secrets = %#v, capabilities = %#v", plans[0].RequiredSecrets, plans[0].RequiredCapabilities)
	}
}

func TestConcurrentSmokeTerminalKeyMatchesNativeContinuation(t *testing.T) {
	path := smokePath(".github", "workflows", "concurrent.yml")
	plans, err := CompilePlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Target.StepKey != "gha-concurrent" || plans[1].Target.StepKey != "gha-observe" {
		t.Fatalf("concurrent smoke targets = %#v", plans)
	}
	if !reflect.DeepEqual(plans[1].Dependencies, []string{"gha-concurrent"}) {
		t.Fatalf("observer dependencies = %#v", plans[1].Dependencies)
	}
}

func TestCompilePlansRecordsStaticDependenciesWithVerifiedNeedSources(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	plans, err := CompilePlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 || len(plans[0].Dependencies) != 0 {
		t.Fatalf("plans = %#v, want producer plus two consumers", plans)
	}
	for _, consumer := range plans[1:] {
		producerContents, err := plan.Encode(plans[0])
		if err != nil {
			t.Fatal(err)
		}
		wantSources := map[string][]plan.NeedSource{"producer": {{StepKey: plans[0].Target.StepKey, PlanDigest: transport.Digest(producerContents)}}}
		if !reflect.DeepEqual(consumer.Dependencies, []string{plans[0].Target.StepKey}) || !reflect.DeepEqual(consumer.NeedSources, wantSources) || consumer.Needs != nil {
			t.Fatalf("consumer dependencies/sources/results = %#v / %#v / %#v", consumer.Dependencies, consumer.NeedSources, consumer.Needs)
		}
	}
}

func TestCompilePlansMapsLogicalNeedToEveryMatrixProducer(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        variant: [one, two]
    steps:
      - run: echo ok
  consume:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: echo done
`)
	plans, err := CompilePlans("matrix.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Fatalf("plans = %d, want two producers and one consumer", len(plans))
	}
	wantSources := make([]plan.NeedSource, 2)
	for i := range 2 {
		encoded, err := plan.Encode(plans[i])
		if err != nil {
			t.Fatal(err)
		}
		wantSources[i] = plan.NeedSource{StepKey: plans[i].Target.StepKey, PlanDigest: transport.Digest(encoded)}
	}
	sort.Slice(wantSources, func(i, j int) bool { return wantSources[i].StepKey < wantSources[j].StepKey })
	if !reflect.DeepEqual(plans[2].NeedSources, map[string][]plan.NeedSource{"build": wantSources}) {
		t.Fatalf("need sources = %#v, want exact matrix fan-in %#v", plans[2].NeedSources, wantSources)
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
	plans, err := CompilePlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
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
	_, err := CompilePlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
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
	_, err := CompilePlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
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
	_, err := loadLocalAction(repository, "./../../outside")
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
	_, err = loadLocalAction(repository, "./.github/actions/escaped")
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
	_, err = loadLocalAction(repository, "./.github/actions/metadata-escaped")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("loadLocalAction() metadata symlink error = %v, want repository escape error", err)
	}
}

func TestWorkflowRepositoryNormalizesInMemoryWorkflowPath(t *testing.T) {
	repository := t.TempDir()
	path := filepath.Join(repository, ".github", "workflows", "memory.yml")
	root, canonical, err := workflowRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := filepath.Join(root, ".github", "workflows", "memory.yml")
	if canonical != wantCanonical {
		t.Fatalf("workflow repository = %q / %q, want canonical path %q", root, canonical, wantCanonical)
	}
}

func TestCompiledPlansValidateAgainstVersionedSchema(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	source := readFile(t, path)
	plans, err := CompilePlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}

	schemaSource := readFile(t, filepath.Join("..", "..", "schemas", "job-plan-v2.schema.json"))
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
		{name: "self-asserted trust", event: strings.Replace(valid, `"actor":`, `"trust":"trusted","actor":`, 1), wanted: `unknown field "trust"`},
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
