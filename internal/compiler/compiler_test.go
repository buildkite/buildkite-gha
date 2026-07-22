package compiler

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	path := smokePath(".github", "workflows", "ci.yml")
	plans, err := CompilePlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	producer := plans[0]
	if producer.Target.StepKey != "gha-producer" || producer.Workflow.LogicalJobID != "producer" {
		t.Fatalf("producer target = %#v, workflow = %#v", producer.Target, producer.Workflow)
	}
	if producer.Steps[0].ID != "step-1" || producer.Steps[1].ID != "shell" {
		t.Fatalf("deterministic step ids = %q, %q", producer.Steps[0].ID, producer.Steps[1].ID)
	}
	if producer.Steps[2].With["message"] != "${{ steps.shell.outputs.result }}" {
		t.Fatalf("JavaScript inputs = %#v", producer.Steps[2].With)
	}
	if producer.Outputs["result"] != "${{ steps.composite.outputs.result }}" {
		t.Fatalf("job outputs = %#v", producer.Outputs)
	}
	second, err := CompilePlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-runtime")
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
