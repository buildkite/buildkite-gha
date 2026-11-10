package runtime

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestCompileAndRunInstanceStrategy(t *testing.T) {
	const source = `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      max-parallel: 2
      matrix:
        target: [zeta, skipped, alpha]
        exclude: [{target: skipped}]
        include: [{target: omega}]
    env:
      SHELL_NAME: sh
      ROW: ${{ format('{0}:{1}/{2}:{3}:{4}', matrix.target, strategy.job-index, strategy.job-total, strategy.fail-fast, strategy.max-parallel) }}
    defaults:
      run:
        shell: ${{ strategy.job-total > 0 && env.SHELL_NAME || 'invalid' }}
    outputs:
      row: ${{ format('{0}|{1}', strategy.job-index, steps.check.outputs.row) }}
    steps:
      - id: prepare
        run: echo 'ready=yes' >> "$GITHUB_OUTPUT"
      - id: check
        name: Row ${{ strategy.job-index }} ${{ steps.prepare.outputs.ready }}
        if: strategy.job-index >= 0 && steps.prepare.outputs.ready == 'yes'
        shell: ${{ strategy.job-total > 0 && env.SHELL_NAME || 'invalid' }}
        working-directory: ${{ strategy.job-index >= 0 && steps.prepare.outputs.ready == 'yes' && '.' || 'missing' }}
        continue-on-error: ${{ strategy.fail-fast && steps.prepare.outputs.ready == 'yes' }}
        timeout-minutes: ${{ steps.prepare.outputs.ready == 'yes' && strategy.max-parallel || 1 }}
        env:
          MIXED: ${{ format('{0}:{1}', strategy.job-index, steps.prepare.outputs.ready) }}
        run: |
          test "$MIXED" = '${{ strategy.job-index }}:yes'
          echo 'row=${{ env.ROW }}' >> "$GITHUB_OUTPUT"
      - uses: ./.github/actions/observe
        with:
          row: ${{ format('{0}|{1}', strategy.job-index, steps.check.outputs.row) }}
`
	for _, mode := range []string{"matrix", "singleton", "reusable", "deferred"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, ".github/actions/observe/action.yml", `name: observe
inputs:
  row:
    required: true
runs:
  using: composite
  steps:
    - shell: sh
      run: test -n '${{ inputs.row }}' && echo 'observed=${{ inputs.row }}'
`)
			workflow := source
			wantRows := []string{"zeta:0/3:false:2", "alpha:1/3:false:2", "omega:2/3:false:2"}
			if mode == "singleton" {
				start := strings.Index(workflow, "    strategy:")
				end := strings.Index(workflow, "    env:")
				workflow = workflow[:start] + workflow[end:]
				workflow = strings.Replace(workflow, "matrix.target", "'single'", 1)
				wantRows = []string{"single:0/1:true:1"}
			}
			if mode == "reusable" {
				writeFixtureFile(t, workspace, ".github/workflows/called.yml", strings.Replace(workflow, "on: push", "on: workflow_call", 1))
				workflow = "on: push\njobs:\n  call:\n    uses: ./.github/workflows/called.yml\n"
			}
			if mode == "deferred" {
				workflow = strings.Replace(workflow, "  build:\n", "  producer:\n    runs-on: ubuntu-latest\n    outputs:\n      rows: ${{ steps.rows.outputs.rows }}\n    steps: [{id: rows, run: true}]\n  build:\n    needs: producer\n", 1)
				workflow = strings.Replace(workflow, "target: [zeta, skipped, alpha]\n        exclude: [{target: skipped}]\n        include: [{target: omega}]", "include: ${{ fromJSON(needs.producer.outputs.rows) }}", 1)
			}
			path := filepath.Join(workspace, ".github/workflows/strategy.yml")
			writeFixtureFile(t, workspace, ".github/workflows/strategy.yml", workflow)
			event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
			if err != nil {
				t.Fatal(err)
			}
			options := compiler.Options{
				EventTrust: compiler.EventUntrusted,
				Runners:    compiler.RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "test"}, UntrustedQueues: []string{"test"}},
			}
			if mode == "deferred" {
				initial, err := compiler.CompileIRWithOptionsContext(t.Context(), path, []byte(workflow), event, options)
				if err != nil || len(initial.Continuations) != 1 || len(initial.Jobs) != 1 {
					t.Fatalf("initial deferred compile = %+v, %v", initial, err)
				}
				options.RuntimeMatrixRows = map[string][]map[string]any{"build": {{"target": "zeta"}, {"target": "alpha"}, {"target": "omega"}}}
			}
			jobs, err := compilePlansForTest(t.Context(), path, []byte(workflow), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), options)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "deferred" {
				jobs = jobs[1:]
			}
			if len(jobs) != len(wantRows) {
				t.Fatalf("jobs = %d, want %d", len(jobs), len(wantRows))
			}
			for i, job := range jobs {
				encoded, err := plan.Encode(job)
				if err != nil {
					t.Fatal(err)
				}
				job, err = plan.Decode(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "deferred" {
					job.Needs = map[string]plan.Need{"producer": {Result: "success"}}
				}
				var logs bytes.Buffer
				result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
				want := fmt.Sprintf("%d|%s", i, wantRows[i])
				if err != nil || result.Conclusion != "success" || result.Outputs["row"] != want || !strings.Contains(logs.String(), "observed="+want) {
					t.Fatalf("row %d: result = %+v, error = %v, logs = %s; want output %q", i, result, err, logs.String(), want)
				}
			}
		})
	}
}

func TestCompileAndRunStrategyTokenGuard(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/actions/token/action.yml", `name: token
inputs:
  token:
    required: true
runs:
  using: composite
  steps:
    - shell: sh
      run: test -n '${{ inputs.token }}'
`)
	const source = `on: push
permissions: {contents: read}
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        row: [zeta, alpha]
    steps:
      - id: prepare
        run: echo 'ready=yes' >> "$GITHUB_OUTPUT"
      - if: strategy.job-index == 1 && steps.prepare.outputs.ready == 'yes'
        run: test -n '${{ github.token }}'
      - run: test -n '${{ strategy.job-index == 1 && github.token || 'none' }}'
      - uses: ./.github/actions/token
        with:
          token: ${{ strategy.job-index == 1 && steps.prepare.outputs.ready == 'yes' && github.token || 'none' }}
`
	path := filepath.Join(workspace, ".github/workflows/strategy.yml")
	writeFixtureFile(t, workspace, ".github/workflows/strategy.yml", source)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := compileUntrustedPlans(path, []byte(source), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	for i, job := range jobs {
		provider := &testWorkflowTokenProvider{token: "test-strategy-token"}
		var logs bytes.Buffer
		result, err := (Runner{WorkflowToken: provider, Redactor: &testRedactor{}, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
		if err != nil || result.Conclusion != "success" || provider.calls != i || (job.GitHubToken != nil) != (i == 1) {
			t.Fatalf("row %d: conclusion = %s, error = %v, token requests = %d, authority = %+v", i, result.Conclusion, err, provider.calls, job.GitHubToken)
		}
		if strings.Contains(logs.String(), "test-strategy-token") {
			t.Fatal("token leaked to logs")
		}
	}
}
