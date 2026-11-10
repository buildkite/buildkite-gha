package workflow

import (
	"strings"
	"testing"
)

func TestParseOwnsWorkflowAndJobConcurrency(t *testing.T) {
	source := []byte(`name: concurrency
on: push
concurrency:
  group: workflow-${{ github.ref }}
  cancel-in-progress: ${{ startsWith(github.ref, 'refs/pull/') }}
jobs:
  test:
    runs-on: ubuntu-latest
    concurrency: deploy
    steps: [{run: true}]
`)
	parsed, err := Parse("concurrency.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Concurrency == nil || parsed.Concurrency.Group != "workflow-${{ github.ref }}" || parsed.Concurrency.CancelInProgress || parsed.Concurrency.Span.Start.Line != 4 {
		t.Fatalf("workflow concurrency = %#v", parsed.Concurrency)
	}
	if expr := parsed.Concurrency.CancelInProgressExpression; expr == nil || expr.Text != "${{ startsWith(github.ref, 'refs/pull/') }}" || parsed.Concurrency.CancelInProgressPosition.Line != 5 {
		t.Fatalf("workflow cancellation expression = %#v at %#v", expr, parsed.Concurrency.CancelInProgressPosition)
	}
	if len(parsed.Jobs) != 1 || parsed.Jobs[0].Concurrency == nil || parsed.Jobs[0].Concurrency.Group != "deploy" || parsed.Jobs[0].Concurrency.Span.Start.Line != 9 {
		t.Fatalf("job concurrency = %#v", parsed.Jobs)
	}
}

func TestParseOwnsReusableWorkflowConcurrency(t *testing.T) {
	parsed, err := Parse("reusable.yml", []byte(`on:
  workflow_call:
    inputs:
      target: {type: string, required: true}
concurrency: deploy-${{ inputs.target }}
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Callable || parsed.Concurrency == nil || parsed.Concurrency.Group != "deploy-${{ inputs.target }}" {
		t.Fatalf("reusable workflow concurrency = callable %t, %#v", parsed.Callable, parsed.Concurrency)
	}
}

func TestParseOwnsWorkflowLiteralCancellation(t *testing.T) {
	for _, test := range []struct {
		name, source string
	}{
		{
			name:   "lowercase",
			source: "on: push\nconcurrency:\n  group: deploy\n  cancel-in-progress: true\njobs:\n  test: {runs-on: ubuntu-latest, steps: [{run: true}]}\n",
		},
		{
			name:   "uppercase",
			source: "on: push\nconcurrency:\n  group: deploy\n  cancel-in-progress: TRUE\njobs:\n  test: {runs-on: ubuntu-latest, steps: [{run: true}]}\n",
		},
		{
			name:   "alias",
			source: "on: push\nenv:\n  CANCEL: &cancel TRUE\nconcurrency:\n  group: deploy\n  cancel-in-progress: *cancel\njobs:\n  test: {runs-on: ubuntu-latest, steps: [{run: true}]}\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := Parse("concurrency.yml", []byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Concurrency == nil || parsed.Concurrency.Group != "deploy" || !parsed.Concurrency.CancelInProgress {
				t.Fatalf("workflow concurrency = %#v", parsed.Concurrency)
			}
			if position := parsed.Concurrency.CancelInProgressPosition; position.Line == 0 || position.Column == 0 {
				t.Fatalf("cancellation position = %#v", position)
			}
		})
	}
}

func TestParseOwnsAliasedConcurrency(t *testing.T) {
	for _, test := range []struct {
		name, source string
		workflow     bool
	}{
		{
			name:     "workflow-key",
			source:   "on: push\nenv:\n  FIELD: &field concurrency\n*field: deploy\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			workflow: true,
		},
		{
			name:   "jobs-and-job-keys",
			source: "on: push\nenv:\n  JOBS: &jobs jobs\n  FIELD: &field concurrency\n*jobs:\n  test:\n    runs-on: ubuntu-latest\n    *field: deploy\n    steps: [{run: true}]\n",
		},
		{
			name:   "whole-job",
			source: "on: push\njobs:\n  anchor-holder:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n        include:\n          - &shared-job\n            runs-on: ubuntu-latest\n            concurrency: deploy\n            steps:\n              - run: echo ok\n    steps:\n      - run: echo holder\n  test: *shared-job\n",
		},
		{
			name:   "job-id",
			source: "on: push\nenv:\n  ID: &job-id test\njobs:\n  *job-id:\n    runs-on: ubuntu-latest\n    concurrency: deploy\n    steps: [{run: true}]\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := Parse("concurrency.yml", []byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if test.workflow {
				if parsed.Concurrency == nil || parsed.Concurrency.Group != "deploy" {
					t.Fatalf("workflow concurrency = %#v", parsed.Concurrency)
				}
				return
			}
			found := false
			for _, job := range parsed.Jobs {
				if job.ID == "test" && job.Concurrency != nil && job.Concurrency.Group == "deploy" {
					found = true
				}
			}
			if !found {
				t.Fatalf("jobs = %#v, want test concurrency group", parsed.Jobs)
			}
		})
	}
}

func TestParseRetainsJobConcurrencyCancellationWithLocation(t *testing.T) {
	for _, test := range []struct {
		name, source string
		expression   bool
	}{
		{
			name:   "literal",
			source: "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    concurrency: {group: deploy, cancel-in-progress: true}\n    steps: [{run: true}]\n",
		},
		{
			name:   "title-case literal",
			source: "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    concurrency: {group: deploy, cancel-in-progress: True}\n    steps: [{run: true}]\n",
		},
		{
			name:       "expression",
			source:     "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    concurrency:\n      group: deploy\n      cancel-in-progress: ${{ github.ref == 'refs/heads/main' }}\n    steps: [{run: true}]\n",
			expression: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := Parse("concurrency.yml", []byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Jobs) != 1 || parsed.Jobs[0].Concurrency == nil {
				t.Fatalf("jobs = %#v, want retained concurrency", parsed.Jobs)
			}
			concurrency := parsed.Jobs[0].Concurrency
			if concurrency.CancelInProgress != !test.expression || (concurrency.CancelInProgressExpression != nil) != test.expression || concurrency.CancelInProgressPosition.Line == 0 || concurrency.CancelInProgressPosition.Column == 0 {
				t.Fatalf("job cancellation = %#v", concurrency)
			}
		})
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

func TestParseParallelMembersRetainEnclosingExpressionContext(t *testing.T) {
	source := []byte(`on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      artifact: ${{ steps.produce.outputs.artifact }}
    steps:
      - id: produce
        run: echo "artifact=ready" >> "$GITHUB_OUTPUT"
  test:
    needs: prepare
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [ubuntu-latest]
    steps:
      - id: prior
        run: echo "ready=true" >> "$GITHUB_OUTPUT"
      - parallel:
          - if:  steps.prior.outputs.ready == 'true'
            env:
              MATRIX_OS: ${{ matrix.os }}
              NEED_VALUE: ${{ needs.prepare.outputs.artifact }}
            run: test "$MATRIX_OS" = ubuntu-latest && test "$NEED_VALUE" = ready
`)
	parsed, err := Parse("workflow.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	steps := parsed.Jobs[1].Steps
	if len(steps) != 3 || steps[1].If != "steps.prior.outputs.ready == 'true'" || steps[1].IfSpan.Start.Line != 20 || steps[1].IfSpan.Start.Column != 18 || steps[1].Env["MATRIX_OS"] != "${{ matrix.os }}" || steps[1].Env["NEED_VALUE"] != "${{ needs.prepare.outputs.artifact }}" {
		t.Fatalf("parallel member context expressions = %#v", steps)
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

func TestParseConcurrentControlsRejectInvalidInput(t *testing.T) {
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
		{name: "docker overrides", steps: "      - uses: docker://example/image\n        with:\n          args: echo test\n", want: "docker:// container actions are unsupported; use a Dockerfile action or replace the action with a run step"},
		{name: "parallel docker overrides", steps: "      - parallel:\n          - uses: docker://example/image\n            with:\n              Entrypoint: /bin/sh\n", want: "docker:// container actions are unsupported; use a Dockerfile action or replace the action with a run step"},
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
