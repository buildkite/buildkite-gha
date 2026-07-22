package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSmokeWorkflowsIntoOwnedModel(t *testing.T) {
	for _, name := range []string{"shell.yml", "ci.yml", "artifact.yml"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", name)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(path, source)
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Jobs) != 2 {
				t.Fatalf("Parse() jobs = %d, want 2", len(parsed.Jobs))
			}
			for _, job := range parsed.Jobs {
				if job.Span.Start.Line == 0 || job.Span.End.Line < job.Span.Start.Line {
					t.Errorf("job %q has invalid owned source span: %#v", job.ID, job.Span)
				}
				for _, step := range job.Steps {
					if step.Span.Start.Line == 0 || step.Span.End.Line < step.Span.Start.Line {
						t.Errorf("job %q step %q has invalid owned source span: %#v", job.ID, step.Name, step.Span)
					}
				}
			}
		})
	}
}

func TestParsePreservesEnvironmentVariableCase(t *testing.T) {
	source := []byte("on: push\nenv:\n  WorkflowValue: workflow\njobs:\n  build:\n    runs-on: ubuntu-latest\n    env:\n      JobValue: job\n    steps:\n      - run: true\n        env:\n          STEP_VALUE: step\n")
	parsed, err := Parse("env.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		env        map[string]string
		key, value string
	}{
		{parsed.Env, "WorkflowValue", "workflow"},
		{parsed.Jobs[0].Env, "JobValue", "job"},
		{parsed.Jobs[0].Steps[0].Env, "STEP_VALUE", "step"},
	} {
		if check.env[check.key] != check.value {
			t.Fatalf("env = %#v, want %s=%s", check.env, check.key, check.value)
		}
	}
}

func TestParseMatrixPreservesDeclarationOrderAndCombinationSpans(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        fruit: [apple, pear]
        animal: [cat, dog]
        include:
          - color: green
            animal: cat
        exclude:
          - fruit: pear
            animal: dog
    steps:
      - run: true
`)
	parsed, err := Parse("matrix.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	matrix := parsed.Jobs[0].Matrix
	if matrix.Rows[0].Name != "fruit" || matrix.Rows[1].Name != "animal" {
		t.Fatalf("matrix rows = %#v, want declaration order", matrix.Rows)
	}
	for _, combination := range []MatrixCombination{matrix.Include[0], matrix.Exclude[0]} {
		if combination.Span.Start.Line == 0 || positionAfter(combination.Span.Start, combination.Span.End) {
			t.Fatalf("invalid matrix combination span: %#v", combination.Span)
		}
	}
	if matrix.Span.End.Line != 14 {
		t.Fatalf("matrix span end = %#v, want final exclude value on line 14", matrix.Span.End)
	}
}

func TestParseOwnsReusableWorkflowCallsAndInputDeclarations(t *testing.T) {
	source := []byte(`on:
  workflow_call:
    inputs:
      enabled:
        type: boolean
        default: true
      target:
        type: string
        required: true
jobs:
  nested:
    uses: ./.github/workflows/nested.yml
    with:
      enabled: false
      target: linux
`)
	parsed, err := Parse(".github/workflows/reusable.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Callable || len(parsed.CallInputs) != 2 {
		t.Fatalf("workflow_call = %#v, want two owned inputs", parsed.CallInputs)
	}
	if got := parsed.CallInputs["enabled"].Default.Data; got != true {
		t.Fatalf("enabled default = %#v, want typed true", got)
	}
	call := parsed.Jobs[0].Reusable
	if call == nil || call.Uses != "./.github/workflows/nested.yml" {
		t.Fatalf("reusable call = %#v", call)
	}
	if got := call.Inputs["enabled"].Data; got != false {
		t.Fatalf("enabled call input = %#v, want typed false", got)
	}
}

func TestParseRejectsExpressionValuedExecutionScalars(t *testing.T) {
	tests := []struct {
		name    string
		snippet string
		want    string
	}{
		{name: "fail fast", snippet: "    strategy:\n      fail-fast: ${{ inputs.flag }}\n      matrix:\n        os: [ubuntu-latest]\n    steps:\n      - run: true\n", want: "expression-valued matrix fail-fast is unsupported"},
		{name: "max parallel", snippet: "    strategy:\n      max-parallel: ${{ inputs.count }}\n      matrix:\n        os: [ubuntu-latest]\n    steps:\n      - run: true\n", want: "expression-valued matrix max-parallel is unsupported"},
		{name: "continue on error", snippet: "    steps:\n      - run: true\n        continue-on-error: ${{ matrix.experimental }}\n", want: "expression-valued step continue-on-error is unsupported"},
		{name: "timeout", snippet: "    steps:\n      - run: true\n        timeout-minutes: ${{ inputs.timeout }}\n", want: "expression-valued step timeout-minutes is unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n" + test.snippet
			_, err := Parse("expressions.yml", []byte(source))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}
