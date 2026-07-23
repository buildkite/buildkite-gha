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

func TestParseRetainsSequentialRuntimeControls(t *testing.T) {
	source := []byte("name: runtime\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    if: always()\n    timeout-minutes: 5\n    steps:\n      - run: echo ok\n        if: success()\n        timeout-minutes: 2\n        continue-on-error: true\n")
	parsed, err := Parse("workflow.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	job := parsed.Jobs[0]
	if job.If != "always()" || job.TimeoutMinutes != 5 {
		t.Fatalf("job controls = if %q timeout %v", job.If, job.TimeoutMinutes)
	}
	step := job.Steps[0]
	if step.If != "success()" || step.TimeoutMinutes != 2 || !step.ContinueOnError {
		t.Fatalf("step controls = %#v", step)
	}
}

func TestParseRetainsConcurrentRuntimeControls(t *testing.T) {
	source := []byte(`name: concurrent runtime
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: Background producer
        id: producer
        run: echo ok
        background: true
        continue-on-error: true
      - name: Targeted barrier
        wait: [producer]
      - name: Full barrier
        wait-all:
      - name: Stop producer
        cancel: producer
      - parallel:
          - name: Parallel shell
            run: echo shell
            env:
              MEMBER: shell
          - id: parallel-action
            uses: ./action
            with:
              Message: hello
`)
	parsed, err := Parse("workflow.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	steps := parsed.Jobs[0].Steps
	if len(steps) != 7 {
		t.Fatalf("steps = %#v, want seven lowered steps", steps)
	}
	if steps[0].Kind != "run" || !steps[0].Background || steps[0].ID != "producer" || !steps[0].ContinueOnError {
		t.Fatalf("background step = %#v", steps[0])
	}
	if steps[1].Kind != "wait" || len(steps[1].Targets) != 1 || steps[1].Targets[0] != "producer" || steps[1].ContinueOnError {
		t.Fatalf("targeted wait = %#v", steps[1])
	}
	if steps[2].Kind != "wait-all" || len(steps[2].Targets) != 0 {
		t.Fatalf("wait-all = %#v", steps[2])
	}
	if steps[3].Kind != "cancel" || len(steps[3].Targets) != 1 || steps[3].Targets[0] != "producer" {
		t.Fatalf("cancel = %#v", steps[3])
	}
	if steps[4].Kind != "run" || !steps[4].Background || steps[4].ID == "" || steps[4].Env["MEMBER"] != "shell" {
		t.Fatalf("parallel shell member = %#v", steps[4])
	}
	if steps[5].Kind != "uses" || !steps[5].Background || steps[5].ID != "parallel-action" || steps[5].With["message"] != "hello" {
		t.Fatalf("parallel action member = %#v", steps[5])
	}
	if steps[6].Kind != "wait" || len(steps[6].Targets) != 2 || steps[6].Targets[0] != steps[4].ID || steps[6].Targets[1] != steps[5].ID {
		t.Fatalf("parallel barrier = %#v", steps[6])
	}
}

func TestParseParallelOwnsBooleanSpellingsAndDeterministicIDs(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - parallel:
          - run: echo one
            continue-on-error: True
          - run: echo two
            continue-on-error: False
`)
	parsed, err := Parse("workflow.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	steps := parsed.Jobs[0].Steps
	if len(steps) != 3 || steps[0].ID != "__parallel_6_9_1" || steps[1].ID != "__parallel_6_9_2" || steps[2].ID != "__parallel_6_9_wait" {
		t.Fatalf("parallel ids = %#v", steps)
	}
	if !steps[0].ContinueOnError || steps[1].ContinueOnError {
		t.Fatalf("parallel continue-on-error values = %v, %v", steps[0].ContinueOnError, steps[1].ContinueOnError)
	}
	if steps[2].Span.End.Line < steps[1].Span.End.Line {
		t.Fatalf("parallel barrier span = %#v, final member span = %#v", steps[2].Span, steps[1].Span)
	}
}

func TestParseConcurrentControlsFailClosed(t *testing.T) {
	tests := []struct {
		name  string
		steps string
		want  string
	}{
		{name: "background false", steps: "      - run: true\n        background: false\n", want: "background must be the literal true"},
		{name: "unknown target", steps: "      - wait: future\n      - id: future\n        run: true\n        background: true\n", want: `wait target "future" is not a prior background step`},
		{name: "duplicate target", steps: "      - id: work\n        run: true\n        background: true\n      - wait: [work, WORK]\n", want: `wait repeats background step "WORK"`},
		{name: "conditional control", steps: "      - wait-all:\n        if: always()\n", want: `wait-all control does not support "if"`},
		{name: "continue-on-error control", steps: "      - wait-all:\n        continue-on-error: true\n", want: `wait-all control does not support "continue-on-error"`},
		{name: "empty parallel", steps: "      - parallel: []\n", want: "parallel requires a non-empty list"},
		{name: "nested background", steps: "      - parallel:\n          - run: true\n            background: true\n", want: `parallel member does not support "background"`},
		{name: "parallel member execution", steps: "      - parallel:\n          - run: true\n            uses: ./action\n", want: "parallel member must declare exactly one"},
		{name: "parallel outer field", steps: "      - name: group\n        parallel:\n          - run: true\n", want: `parallel control does not support "name"`},
		{name: "parallel outer fields deterministic", steps: "      - name: group\n        id: group\n        parallel:\n          - run: true\n", want: `parallel control does not support "id"`},
		{name: "parallel docker overrides", steps: "      - parallel:\n          - uses: docker://example/image\n            with:\n              Entrypoint: /bin/sh\n", want: "unsupported entrypoint or args overrides"},
		{name: "unmatched actionlint error", steps: "      - run: true\n        background: true\n        unexpected: true\n", want: `unexpected key "unexpected"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n" + test.steps
			_, err := Parse("workflow.yml", []byte(source))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
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
