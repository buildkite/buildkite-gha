// Package runtimebenchmark measures job execution, not workflow compilation.
package runtimebenchmark

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

// This entire harness is copied unchanged into the baseline checkout. Keep it
// on exported production interfaces so both versions execute the same workload.
func BenchmarkRuntimeWorkflow(b *testing.B) {
	for _, name := range []string{"shell", "composite", "javascript"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			workspace := b.TempDir()
			write := func(path, source string) {
				b.Helper()
				path = filepath.Join(workspace, path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			write(".github/actions/composite/action.yml", `name: Composite
description: Exercise nested steps and file commands
inputs:
  value:
    description: Value to forward
    required: true
outputs:
  value:
    description: Forwarded value
    value: ${{ steps.forward.outputs.value }}
runs:
  using: composite
  steps:
    - id: forward
      shell: bash
      env:
        VALUE: ${{ inputs.value }}
      run: echo "value=$VALUE" >> "$GITHUB_OUTPUT"
    - shell: bash
      run: echo "SEEN=${{ steps.forward.outputs.value }}" >> "$GITHUB_ENV"
`)
			write(".github/actions/javascript/action.yml", `name: JavaScript
description: Exercise Node lifecycle and state
inputs:
  value:
    description: Value to forward
    required: true
outputs:
  value:
    description: Forwarded value
runs:
  using: node24
  pre: pre.js
  main: main.js
  post: post.js
  post-if: always()
`)
			write(".github/actions/javascript/pre.js", `require('node:fs').appendFileSync(process.env.GITHUB_STEP_SUMMARY, 'pre complete\n');`)
			write(".github/actions/javascript/main.js", `const fs = require('node:fs');
fs.appendFileSync(process.env.GITHUB_OUTPUT, 'value=' + process.env.INPUT_VALUE + '\n');
fs.appendFileSync(process.env.GITHUB_ENV, 'SEEN=' + process.env.INPUT_VALUE + '\n');
fs.appendFileSync(process.env.GITHUB_STATE, 'phase=main\n');
console.log('::add-mask::benchmark-secret');
console.log('masked benchmark-secret');
`)
			write(".github/actions/javascript/post.js", `const fs = require('node:fs');
if (process.env.STATE_phase !== 'main') throw new Error('main did not run');
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, 'post complete\n');
`)
			const steps = 12
			var workflow strings.Builder
			workflow.WriteString(`name: Runtime benchmark
on: workflow_dispatch
permissions: {}
jobs:
  benchmark:
    runs-on: ubuntu-latest
    outputs:
      value: ${{ steps.s11.outputs.value }}
    steps:
`)
			for i := range steps {
				value := "seed"
				if i > 0 {
					value = fmt.Sprintf("${{ steps.s%d.outputs.value }}", i-1)
				}
				value += fmt.Sprintf("-%d", i)
				fmt.Fprintf(&workflow, "      - id: s%d\n", i)
				if name == "shell" {
					fmt.Fprintf(&workflow, "        env:\n          VALUE: %s\n        run: |\n          echo \"value=$VALUE\" >> \"$GITHUB_OUTPUT\"\n          echo \"SEEN=$VALUE\" >> \"$GITHUB_ENV\"\n", value)
				} else {
					fmt.Fprintf(&workflow, "        uses: ./.github/actions/%s\n        with:\n          value: %s\n", name, value)
				}
			}
			path := filepath.Join(workspace, ".github/workflows/benchmark.yml")
			write(".github/workflows/benchmark.yml", workflow.String())
			event := []byte(`{"provider":"github","event":"workflow_dispatch","repository":{"owner":"buildkite","name":"buildkite-gha","default_branch":"main"},"ref":"refs/heads/main","sha":"1111111111111111111111111111111111111111","actor":"benchmark","payload":{"inputs":{}}}`)
			bundle, err := compiler.CompileBundlePlansContext(b.Context(), path, []byte(workflow.String()), event, "benchmark", "sha256:"+strings.Repeat("1", 64), compiler.Options{
				EventTrust: compiler.EventUntrusted,
				Runners: compiler.RunnerPolicy{
					Labels:          map[string]string{"ubuntu-latest": "hosted"},
					UntrustedQueues: []string{"hosted"},
				},
			})
			if err != nil || len(bundle.Plans) != 1 {
				b.Fatalf("compile: %v (plans: %d)", err, len(bundle.Plans))
			}
			node := os.Getenv("BENCHMARK_NODE24")
			if name == "javascript" && !filepath.IsAbs(node) {
				b.Fatal("BENCHMARK_NODE24 must name an absolute Node 24 executable")
			}
			for range b.N {
				var logs bytes.Buffer
				runner := gharuntime.Runner{Stdout: &logs, Stderr: &logs, Node24: node}
				b.StartTimer()
				result, err := runner.RunJob(b.Context(), bundle.Plans[0].Job, workspace)
				b.StopTimer()
				const expected = "seed-0-1-2-3-4-5-6-7-8-9-10-11"
				if err != nil || result.Conclusion != "success" || result.Outputs["value"] != expected || result.Env["SEEN"] != expected {
					b.Fatalf("execution: %v; result: %+v\n%s", err, result, logs.String())
				}
				if name == "javascript" && (strings.Count(result.Summary, "pre complete") != steps || strings.Count(result.Summary, "post complete") != steps || strings.Contains(logs.String(), "benchmark-secret")) {
					b.Fatalf("lifecycle or masking failed: %+v\n%s", result, logs.String())
				}
			}
		})
	}
}
