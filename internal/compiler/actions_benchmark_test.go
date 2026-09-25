package compiler

import (
	"os"
	"path/filepath"
	"testing"
)

// Eight invocations share a composite and its child, but supply distinct inputs.
// The local fixture measures graph/analysis work without network variability.
func BenchmarkRepeatedCompositeActions(b *testing.B) {
	workspace := b.TempDir()
	writeAction(b, workspace, "child", `name: child
inputs:
  token:
    default: ${{ github.token }}
runs:
  using: node24
  main: index.js
`)
	writeAction(b, workspace, "parent", `name: parent
inputs:
  token:
    default: ''
runs:
  using: composite
  steps:
    - uses: ./child
      with:
        token: ${{ inputs.token }}
`)
	refs := make([]string, 8)
	inputs := make([]map[string]string, len(refs))
	for i := range refs {
		refs[i] = "./parent"
		inputs[i] = map[string]string{"token": ""}
		if i%2 != 0 {
			inputs[i]["token"] = "${{ github.token }}"
		}
	}
	b.Run("compile", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := compileActionInvocations(b.Context(), workspace, nil, "https://github.com", refs, inputs); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("without-authority", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := compileActionInvocations(b.Context(), workspace, nil, "https://github.com", refs, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("analysis", func(b *testing.B) {
		graph, err := buildActionGraph(b.Context(), workspace, nil, refs, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := graph.analyzeInvocations("https://github.com", refs, inputs); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// Exercise both per-step validation and per-job planning across matrix jobs.
func BenchmarkBundleRepeatedCompositeActions(b *testing.B) {
	workspace := b.TempDir()
	writeAction(b, workspace, "child", "name: child\nruns:\n  using: node24\n  main: index.js\n")
	writeAction(b, workspace, "parent", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	workflowPath := filepath.Join(workspace, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		b.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  repeated:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        variant: [a, b, c, d, e, f, g, h]
    steps:
      - uses: ./parent
      - uses: ./parent
      - uses: ./parent
`)
	if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
		b.Fatal(err)
	}
	event, err := os.ReadFile(smokePath("events", "push.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		bundle, err := CompileBundlePlansContext(b.Context(), workflowPath, workflow, event, "0.0.0-test", testDistributionDigest, DefaultOptions())
		if err != nil {
			b.Fatal(err)
		}
		if len(bundle.Plans) != 8 {
			b.Fatalf("plans = %d, want 8", len(bundle.Plans))
		}
	}
}
