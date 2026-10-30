package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
)

// GitHub treats reusable input values containing ${{ as data; see the
// expression evidence in docs/compatibility-contract.md.
func TestCompiledReusableInputValuesWithExpressionSyntaxStayData(t *testing.T) {
	workspace := t.TempDir()
	callerPath := ".github/workflows/caller.yml"
	caller := `on: push
jobs:
  direct:
    uses: ./.github/workflows/callee.yml
    secrets: inherit
    with:
      payload: ${{ format('{0}{{{{ github.sha }}}}', '$') }}
      secret-name: ${{ format('{0}{{{{ ''CANARY'' }}}}', '$') }}
  nested:
    uses: ./.github/workflows/forward.yml
    secrets: inherit
    with:
      payload: ${{ format('{0}{{{{ github.sha }}}}', '$') }}
      secret-name: ${{ format('{0}{{{{ ''CANARY'' }}}}', '$') }}
`
	writeFixtureFile(t, workspace, callerPath, caller)
	writeFixtureFile(t, workspace, ".github/workflows/forward.yml", `on:
  workflow_call:
    inputs:
      payload:
        type: string
      secret-name:
        type: string
jobs:
  forward:
    uses: ./.github/workflows/callee.yml
    secrets: inherit
    with:
      payload: ${{ inputs.payload }}
      secret-name: ${{ inputs.secret-name }}
`)
	writeFixtureFile(t, workspace, ".github/workflows/callee.yml", `on:
  workflow_call:
    inputs:
      payload:
        type: string
      secret-name:
        type: string
jobs:
  verify:
    if: inputs.payload == format('{0}{{{{ github.sha }}}}', '$')
    runs-on: ubuntu-latest
    strategy:
      matrix:
        payload:
          - ${{ inputs.payload }}
    env:
      JOB_VALUE: ${{ inputs.payload }}
    steps:
      - id: composite
        uses: ./.github/actions/echo
        with:
          message: ${{ inputs.payload }}
      - shell: bash
        env:
          ENV_VALUE: ${{ inputs.payload }}
          MATRIX_VALUE: ${{ matrix.payload }}
          WITH_VALUE: ${{ steps.composite.outputs.result }}
          SELECTED_SECRET: ${{ secrets[inputs.secret-name] }}
          CONTROL_SECRET: ${{ secrets.CANARY }}
        run: |
          run_value='${{ inputs.payload }}'
          expected="$"'{{ github.sha }}'
          printf '%s\n' "RUN=$run_value" "ENV=$ENV_VALUE" "JOB=$JOB_VALUE" "MATRIX=$MATRIX_VALUE" "WITH=$WITH_VALUE" "SELECTED=$SELECTED_SECRET" "CONTROL=$CONTROL_SECRET"
          test "$run_value" = "$expected"
          test "$ENV_VALUE" = "$expected"
          test "$JOB_VALUE" = "$expected"
          test "$MATRIX_VALUE" = "$expected"
          test "$WITH_VALUE" = "$expected-composite"
          test -z "$SELECTED_SECRET"
          test -n "$CONTROL_SECRET"
`)
	writeFixtureFile(t, workspace, ".github/actions/echo/action.yml", `name: echo
inputs:
  message:
    required: true
outputs:
  result:
    value: ${{ steps.echo.outputs.result }}
runs:
  using: composite
  steps:
    - id: echo
      shell: bash
      env:
        MESSAGE: ${{ inputs.message }}
      run: echo "result=$MESSAGE-composite" >> "$GITHUB_OUTPUT"
`)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(t.Context(), filepath.Join(workspace, callerPath), []byte(caller), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "gha-untrusted"},
			UntrustedQueues: []string{"gha-untrusted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want direct and nested callee jobs", len(plans))
	}
	for _, job := range plans {
		var logs bytes.Buffer
		result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: testSecretResolver{"CANARY": "canary-value"}, Redactor: &testRedactor{}}).runTestJob(t.Context(), job, workspace)
		if err != nil || result.Conclusion != "success" {
			t.Fatalf("RunJob(%s) result = %#v, error = %v\n%s", job.Workflow.LogicalJobID, result, err, logs.String())
		}
	}
}
