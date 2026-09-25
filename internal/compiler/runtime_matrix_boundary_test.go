package compiler

import (
	"fmt"
	"strings"
	"testing"
)

// TestValidateReportsRuntimeMatrixBoundaryDespiteGraphFailures proves the
// event-independent scan still finds a needs-derived matrix when an earlier
// job in the same graph fails, whatever the fail-fast order, the reusable
// depth, or how a shared reusable workflow is reached. Callers read the flag
// from the report even when validation returns an error.
func TestValidateReportsRuntimeMatrixBoundaryDespiteGraphFailures(t *testing.T) {
	repository := t.TempDir()
	runtimeMatrix := `on: workflow_call
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.matrix.outputs.include }}
    steps:
      - id: matrix
        run: true
  generated:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    steps:
      - run: true
`
	writeWorkflow(t, repository, "runtime-matrix.yml", runtimeMatrix)
	writeWorkflow(t, repository, "not-callable.yml", `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	writeWorkflow(t, repository, "shared.yml", `on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/runtime-matrix.yml
`)
	for i, next := range []string{"deep-2.yml", "deep-3.yml", "shared.yml"} {
		writeWorkflow(t, repository, fmt.Sprintf("deep-%d.yml", i+1), fmt.Sprintf(`on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/%s
`, next))
	}
	for i, next := range []string{"depth-2.yml", "depth-3.yml", "depth-4.yml", "runtime-matrix.yml"} {
		writeWorkflow(t, repository, fmt.Sprintf("depth-%d.yml", i+1), fmt.Sprintf(`on: workflow_call
jobs:
  delegated:
    uses: ./.github/workflows/%s
`, next))
	}
	writeWorkflow(t, repository, "malformed-runtime-matrix.yml", `on: workflow_call
jobs:
  generated:
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    invalid: [
`)

	for name, test := range map[string]struct {
		caller      string
		wantFailure string
	}{
		"exact boundary survives an earlier graph failure": {
			caller: `on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.matrix.outputs.include }}
    steps:
      - id: matrix
        run: true
  generated:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    steps:
      - run: true
  missing-reusable:
    uses: ./.github/workflows/missing.yml
`,
			wantFailure: "missing-reusable",
		},
		"reusable boundary discovery does not depend on fail-fast order": {
			caller: `on: push
jobs:
  a-invalid-reusable:
    uses: ./.github/workflows/not-callable.yml
  z-runtime-matrix:
    uses: ./.github/workflows/runtime-matrix.yml
`,
			wantFailure: "a-invalid-reusable",
		},
		"shared boundary is rescanned when reached at a shallower depth": {
			caller: `on: push
jobs:
  a-deep:
    uses: ./.github/workflows/deep-1.yml
  z-shared:
    uses: ./.github/workflows/shared.yml
`,
			wantFailure: "a-deep",
		},
		"depth-limited reusable discovery still reports the boundary": {
			caller: `on: push
jobs:
  a-invalid-reusable:
    uses: ./.github/workflows/not-callable.yml
  z-deep:
    uses: ./.github/workflows/depth-1.yml
`,
			wantFailure: "a-invalid-reusable",
		},
		"incomplete reusable discovery still reports the boundary": {
			caller: `on: push
jobs:
  a-not-callable:
    uses: ./.github/workflows/not-callable.yml
  z-malformed:
    uses: ./.github/workflows/malformed-runtime-matrix.yml
`,
			wantFailure: "a-not-callable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			callerPath := writeWorkflow(t, repository, strings.ReplaceAll(name, " ", "-")+".yml", test.caller)
			report, err := Validate(callerPath, readFile(t, callerPath))
			if err == nil {
				t.Fatalf("Validate() error = nil, want a graph failure; report = %#v", report)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("job %q", test.wantFailure)) {
				t.Fatalf("Validate() error = %v, want a graph failure for job %q", err, test.wantFailure)
			}
			if !report.RuntimeMatrixBoundary {
				t.Fatalf("RuntimeMatrixBoundary = false after graph failure %v", err)
			}
		})
	}
}
