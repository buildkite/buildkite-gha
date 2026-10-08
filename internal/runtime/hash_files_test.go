package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	executionprogram "github.com/buildkite/buildkite-gha/internal/program"
)

func TestHashWorkspaceFilesConformance(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "a.txt", "alpha")
	writeFixtureFile(t, workspace, "b.txt", "bravo")
	writeFixtureFile(t, workspace, "nested/c.txt", "charlie")
	writeFixtureFile(t, workspace, "nested/skip.txt", "skip")
	writeFixtureFile(t, workspace, ".hidden", "hidden")
	writeFixtureFile(t, workspace, ".config/value", "dot-directory")
	writeFixtureFile(t, workspace, "literal/{name}.txt", "braces")

	for _, test := range []struct {
		name     string
		patterns []string
		contents []string
	}{
		{name: "known digest and stable order", patterns: []string{"*.txt"}, contents: []string{"alpha", "bravo"}},
		{name: "multiple patterns", patterns: []string{"a.txt", "nested/*.txt"}, contents: []string{"alpha", "charlie", "skip"}},
		{name: "literal search order", patterns: []string{"b.txt", "a.txt", "b.txt"}, contents: []string{"alpha", "bravo"}},
		{name: "ordered negation", patterns: []string{"**", "!nested/**"}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces"}},
		{name: "ordered re-inclusion", patterns: []string{"**", "!nested/**", "nested/c.txt"}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces", "charlie"}},
		{name: "later exclusion", patterns: []string{"nested/c.txt", "!nested/**"}},
		{name: "duplicate overlap", patterns: []string{"a.txt", "*.txt", "a.txt"}, contents: []string{"alpha", "bravo"}},
		{name: "empty matches", patterns: []string{"missing/**"}},
		{name: "hidden files and nested paths", patterns: []string{"**/.hidden", ".config"}, contents: []string{"dot-directory", "hidden"}},
		{name: "directory implies descendants", patterns: []string{"nested"}, contents: []string{"charlie", "skip"}},
		{name: "trailing slash directory", patterns: []string{"nested/"}, contents: []string{"charlie", "skip"}},
		{name: "braces are literal", patterns: []string{"literal/{name}.txt"}, contents: []string{"braces"}},
		{name: "comments and blank patterns", patterns: []string{" # ignored", "", "a.txt"}, contents: []string{"alpha"}},
		{name: "workspace root", patterns: []string{"."}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces", "charlie", "skip"}},
		{name: "spaced negation", patterns: []string{"**", "! nested/**"}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces"}},
		{name: "spaced double negation", patterns: []string{"!! nested/c.txt"}, contents: []string{"charlie"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := hashWorkspaceFiles(t.Context(), workspace, test.patterns)
			if err != nil {
				t.Fatal(err)
			}
			want := githubHash(test.contents...)
			if test.name == "known digest and stable order" {
				want = "90d39555bb3c223e12f5a375c3011d2462fe2e1e36b8416a0b623d5831a9b4f3"
			}
			if got != want {
				t.Fatalf("hashWorkspaceFiles(%#v) = %q, want %q", test.patterns, got, want)
			}
		})
	}

	first, err := hashWorkspaceFiles(t.Context(), workspace, []string{"**"})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		got, err := hashWorkspaceFiles(t.Context(), workspace, []string{"**"})
		if err != nil || got != first {
			t.Fatalf("repeated hash = %q, %v; want %q", got, err, first)
		}
	}
}

func TestHashWorkspaceFilesDirectoryPatternDoesNotMatchRegularFile(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "plain", "regular")
	if got, err := hashWorkspaceFiles(t.Context(), workspace, []string{"plain/"}); err != nil || got != "" {
		t.Fatalf("directory-only regular file hash = %q, %v", got, err)
	}
}

func TestHashFilePatternPlatformCaseSensitivity(t *testing.T) {
	patterns, err := parseHashFilePatterns([]string{"SRC/*.GO"}, defaultHashFilesLimits)
	if err != nil {
		t.Fatal(err)
	}
	if matched, _ := hashFilePatternMatch(patterns, "src/main.go", false); matched {
		t.Fatal("case-sensitive platform matched different case")
	}
	if matched, _ := hashFilePatternMatch(patterns, "src/main.go", true); !matched {
		t.Fatal("case-insensitive platform did not match different case")
	}
}

func TestHashWorkspaceFilesRejectsEscapesAndUnsafeFiles(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	writeFixtureFile(t, workspace, "safe.txt", "safe")
	writeFixtureFile(t, outside, "outside.txt", "outside")
	for _, pattern := range []string{"/etc/passwd", "../outside", "safe/../../outside", `C:\outside`, "./safe/../outside"} {
		t.Run(pattern, func(t *testing.T) {
			if _, err := hashWorkspaceFiles(t.Context(), workspace, []string{pattern}); err == nil || !strings.Contains(err.Error(), "hashFiles pattern") {
				t.Fatalf("escape pattern error = %v", err)
			}
		})
	}
	for _, pattern := range []string{"safe\n", "safe\t", "safe\x1b", "safe\x7f"} {
		if _, err := hashWorkspaceFiles(t.Context(), workspace, []string{pattern}); err == nil || !strings.Contains(err.Error(), "control character") {
			t.Fatalf("control pattern error = %v", err)
		}
	}

	for name, target := range map[string]string{
		"inside-link": filepath.Join(workspace, "safe.txt"),
		"escape-link": filepath.Join(outside, "outside.txt"),
		"loop":        "loop",
	} {
		if err := os.Symlink(target, filepath.Join(workspace, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := hashWorkspaceFiles(t.Context(), workspace, []string{name}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("%s error = %v", name, err)
		}
	}

	if runtime.GOOS != "windows" {
		fifo := filepath.Join(workspace, "pipe")
		if err := testMkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := hashWorkspaceFiles(t.Context(), workspace, []string{"pipe"}); err == nil || !strings.Contains(err.Error(), "non-regular") {
			t.Fatalf("special file error = %v", err)
		}
	}
}

func TestHashWorkspaceRootRemainsPinnedAfterPathReplacement(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, workspace, "value", "inside")
	writeFixtureFile(t, outside, "value", "outside")
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, workspace); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := hashWorkspaceRootFilesWithLimits(t.Context(), root, []string{"value"}, defaultHashFilesLimits, false)
	if err != nil || got != githubHash("inside") {
		t.Fatalf("pinned workspace hash = %q, %v", got, err)
	}
}

func TestRunJobPinsHashWorkspaceBeforePathReplacement(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: pinned hash workspace\n")
	writeFixtureFile(t, workspace, "value", "inside")
	writeFixtureFile(t, outside, "value", "outside")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
		{ID: "replace", Kind: "run", Shell: "sh", Env: map[string]string{"OUTSIDE": outside}, Command: `mv "$GITHUB_WORKSPACE" "$GITHUB_WORKSPACE-moved" && ln -s "$OUTSIDE" "$GITHUB_WORKSPACE"`},
		{ID: "hash", Kind: "run", Shell: "sh", Env: map[string]string{"VALUE_HASH": "${{ hashFiles('value') }}"}, Command: "test \"$VALUE_HASH\" = " + githubHash("inside")},
	})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() pinned workspace result = %#v, %v", result, err)
	}
}

func TestRunJobHashFilesArgumentsUseStepEnvironment(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles step environment\n")
	writeFixtureFile(t, workspace, "value", "contents")
	digest := githubHash("contents")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{
		ID:        "hash",
		Kind:      "run",
		Shell:     "sh",
		Env:       map[string]string{"PATTERN": "value"},
		Condition: "hashFiles(env.PATTERN) != ''",
		Command:   `test "${{ hashFiles(env.PATTERN) }}" = "` + digest + `"`,
	}})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() step environment hashFiles result = %#v, %v", result, err)
	}
}

func TestHashFilesRuntimeSurfaces(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles surfaces\n")
	writeFixtureFile(t, workspace, "value", "contents")
	for _, test := range []struct {
		name   string
		change func(*plan.Job)
	}{
		{name: "job default shell", change: func(job *plan.Job) { job.DefaultShell = "${{ hashFiles('value') }}" }},
		{name: "job default working directory", change: func(job *plan.Job) { job.DefaultWorkingDirectory = "${{ hashFiles('value') }}" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "run", Kind: "run", Command: "true"}})
			test.change(&job)
			if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `unsupported runtime function "hashFiles"`) {
				t.Fatalf("RunJob() default hashFiles error = %v", err)
			}
		})
	}

	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "name", Name: "${{ hashFiles('value') }}", Kind: "run", Shell: "sh", Command: "true"}})
	if result, err := (Runner{}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("step name unexpectedly evaluated hashFiles: %#v, %v", result, err)
	}

	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "runs:\n  using: composite\n  steps:\n    - shell: sh\n      run: test \"${{ hashFiles('value') }}\" = "+githubHash("contents")+"\n")
	job = runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	if err := synthesizeTestLocalActionLocks(&job, workspace); err != nil {
		t.Fatal(err)
	}
	attachTestProgram(&job)
	if err := attachTestActionPrograms(&job, workspace, nil); err != nil {
		t.Fatal(err)
	}
	action := job.Program.Actions[job.Actions[0].ID]
	if value, err := executionprogram.EvaluateSite(action.Steps[0].Condition, executionprogram.EvaluationContext{}); err != nil || value != true {
		t.Fatalf("normalized composite condition = %#v, %v", value, err)
	}
	if _, err := executionprogram.EvaluateSite(action.Steps[0].Run.Command, executionprogram.EvaluationContext{}); err == nil {
		t.Fatal("normalized action command admitted unavailable hashFiles")
	}
	if result, err := (Runner{}).RunJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("composite metadata hashFiles result/error = %#v / %v", result, err)
	}

	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "outputs:\n  digest:\n    value: ${{ hashFiles('value') }}\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n")
	job = runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `composite output "digest": hashFiles() is not supported in composite output metadata; compute it in a step and expose that step's output`) {
		t.Fatalf("composite output hashFiles error = %v", err)
	}

	writeFixtureFile(t, workspace, ".github/actions/child/action.yml", "inputs:\n  value:\n    required: false\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "runs:\n  using: composite\n  steps:\n    - shell: sh\n      run: printf 'TEMPLATE=$%s\\n' \"{{ false && hashFiles('value') || 'ok' }}\" >> \"$GITHUB_ENV\"\n    - uses: ./.github/actions/child\n      with:\n        value: ${{ env.TEMPLATE }}\n")
	job = runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	if result, err := (Runner{}).runTestJob(t.Context(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("nested composite input was evaluated twice: %#v, %v", result, err)
	}
}

func TestCompositeHashFilesDiagnostics(t *testing.T) {
	for _, test := range []struct {
		field string
		step  string
		want  string
	}{
		{field: "env", step: "shell: sh\n      env:\n        KEY: ${{ hashFiles('../outside') }}\n      run: true", want: `env: evaluate "KEY"`},
		{field: "run", step: "shell: sh\n      run: echo ${{ hashFiles('../outside') }}", want: "run"},
		{field: "shell", step: "shell: ${{ hashFiles('../outside') }}\n      run: true", want: "shell"},
		{field: "working-directory", step: "shell: sh\n      working-directory: ${{ hashFiles('../outside') }}\n      run: true", want: "working-directory"},
		{field: "with", step: "uses: ./.github/actions/child\n      with:\n        key: ${{ hashFiles('../outside') }}", want: `with: evaluate "key"`},
		{field: "condition", step: "shell: sh\n      if: hashFiles('../outside') != ''\n      run: true", want: "condition"},
	} {
		for _, tolerate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tolerate=%t", test.field, tolerate), func(t *testing.T) {
				workspace := t.TempDir()
				writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashing diagnostics\n")
				writeFixtureFile(t, workspace, ".github/actions/child/action.yml", "inputs:\n  key:\n    required: true\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n")
				writeFixtureFile(t, workspace, ".github/actions/hash/action.yml", fmt.Sprintf("runs:\n  using: composite\n  steps:\n    - id: invalid\n      continue-on-error: %t\n      %s\n    - shell: sh\n      run: echo continued\n", tolerate, test.step))
				job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "hash", Kind: "uses", Uses: "./.github/actions/hash"}})
				var stdout, stderr bytes.Buffer
				result, err := (Runner{Stdout: &stdout, Stderr: &stderr}).runTestJob(t.Context(), job, workspace)
				want := test.want + `: hashFiles pattern 1 may not contain ".."`
				fatalLocation := `step "hash": composite action step 1: `
				if test.field == "condition" {
					fatalLocation = `step "hash": composite action step 1 `
				}
				if tolerate {
					if err != nil || result.Conclusion != "success" || !strings.Contains(stdout.String(), "continued") {
						t.Fatalf("tolerated result = %#v, error = %v, stdout = %q", result, err, stdout.String())
					}
					if !strings.Contains(stderr.String(), want) || strings.Count(stderr.String(), "continue-on-error") != 1 || !strings.Contains(result.WarningAnnotations, "hashFiles pattern 1") {
						t.Fatalf("missing/duplicate diagnostic: stderr = %q, annotations = %q", stderr.String(), result.WarningAnnotations)
					}
				} else if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), fatalLocation+want) {
					t.Fatalf("fatal result = %#v, error = %v, want field diagnostic %q", result, err, want)
				}
			})
		}
	}
}

func TestRequiredHashFilesGuard(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("lockfile=%t", present), func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: required cache digest\n")
			if present {
				writeFixtureFile(t, workspace, "package-lock.json", "lockfile contents")
			}
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
				{ID: "lockfile", Kind: "run", Shell: "sh", Env: map[string]string{"HASH": "${{ hashFiles('package-lock.json') }}"}, Command: `if [ -z "$HASH" ]; then
  echo "package-lock.json was not found; check checkout order and the hashFiles pattern" >&2
  exit 1
fi
printf 'digest=%s\n' "$HASH" >> "$GITHUB_OUTPUT"`},
				{ID: "cache", Kind: "run", Shell: "sh", Command: `echo "cache-key=npm-${{ steps.lockfile.outputs.digest }}"`},
			})
			var stdout, stderr bytes.Buffer
			result, err := (Runner{Stdout: &stdout, Stderr: &stderr}).runTestJob(t.Context(), job, workspace)
			if present {
				if err != nil || result.Conclusion != "success" || !strings.Contains(stdout.String(), "cache-key=npm-"+githubHash("lockfile contents")) {
					t.Fatalf("present lockfile result = %#v, %v, logs = %s", result, err, stdout.String())
				}
			} else if err == nil || result.Conclusion != "failure" || !strings.Contains(stderr.String(), "package-lock.json was not found") || strings.Contains(stdout.String(), "cache-key=") {
				t.Fatalf("missing lockfile result = %#v, %v, stdout = %s, stderr = %s", result, err, stdout.String(), stderr.String())
			}
		})
	}
}

func TestCompiledCompositeStepsUseHashFiles(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/hash-files.yml"
	workflow := `on: push
jobs:
  hash:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/hash
        id: hash
        env:
          PATTERN: payload
      - run: test "${{ steps.hash.outputs.digest }}" = "93f7a1af9e76c89675b5bc8c5f5c6aa62f1c78bc0c95693f0296b25274843527"
`
	writeFixtureFile(t, workspace, workflowPath, workflow)
	writeFixtureFile(t, workspace, ".github/actions/hash/payload", "action directory decoy")
	writeFixtureFile(t, workspace, ".github/actions/hash/action.yml", `outputs:
  digest:
    value: ${{ steps.nested.outputs.digest }}
runs:
  using: composite
  steps:
    - shell: sh
      run: printf 'runtime contents' > payload
    - shell: sh
      if: hashFiles(env.PATTERN) != ''
      env:
        DIGEST: ${{ hashFiles(env.PATTERN) }}
      run: |
        test "$DIGEST" = "93f7a1af9e76c89675b5bc8c5f5c6aa62f1c78bc0c95693f0296b25274843527"
        test "${{ hashFiles('payload') }}" = "$DIGEST"
        mkdir -p "$DIGEST"
    - id: nested
      uses: ./.github/actions/child
      env:
        DIGEST: ${{ hashFiles('payload') }}
      with:
        key: ${{ hashFiles('payload') }}
    - shell: sh
      if: hashFiles('missing') != ''
      run: exit 1
    - shell: sh
      if: false
      env:
        UNUSED: ${{ hashFiles('../outside') }}
      run: exit 1
`)
	writeFixtureFile(t, workspace, ".github/actions/child/action.yml", `inputs:
  key:
    required: true
outputs:
  digest:
    value: ${{ steps.check.outputs.digest }}
runs:
  using: composite
  steps:
    - id: check
      shell: sh
      working-directory: ${{ hashFiles('payload') }}
      run: |
        test "${{ inputs.key }}" = "$DIGEST"
        test "${{ hashFiles('payload') }}" = "$DIGEST"
        test "${{ hashFiles('missing') }}" = ""
        printf 'digest=%s\n' "$DIGEST" >> "$GITHUB_OUTPUT"
`)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(filepath.Join(workspace, workflowPath), []byte(workflow), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "hosted")
	if err != nil || len(plans) != 1 {
		t.Fatalf("compile composite hashFiles workflow = %#v, %v", plans, err)
	}
	result, err := (Runner{}).RunJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() composite hashFiles result = %#v, %v", result, err)
	}
}

func TestRemoteCompositeHashFilesForPreAndMain(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: composite hashing lifecycle\n")
	writeFixtureFile(t, workspace, "payload", "before pre")
	writeFixtureFile(t, remote, "root/action.yml", `runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      env:
        DIGEST: ${{ hashFiles('payload') }}
      with:
        key: ${{ hashFiles('payload') }}
`)
	writeFixtureFile(t, remote, "child/action.yml", "inputs:\n  key:\n    required: true\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n  pre-if: hashFiles('payload') != ''\n  post-if: hashFiles('payload') != ''\n")
	writeFixtureFile(t, remote, "child/pre.js", `const fs = require('fs');
if (process.env.INPUT_KEY !== process.env.EXPECTED_PRE || process.env.DIGEST !== process.env.EXPECTED_PRE) throw new Error('pre digest');
fs.writeFileSync('payload', 'after pre');
fs.appendFileSync('phases', 'pre\n');
`)
	writeFixtureFile(t, remote, "child/main.js", `const fs = require('fs');
if (process.env.INPUT_KEY !== process.env.EXPECTED_MAIN || process.env.DIGEST !== process.env.EXPECTED_MAIN) throw new Error('main digest');
fs.appendFileSync('phases', 'main\n');
`)
	writeFixtureFile(t, remote, "child/post.js", "require('fs').appendFileSync('phases', 'post\\n');\n")
	digest := digestTree(t, remote)
	rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}}})
	job.RequiredCapabilities = []string{"network"}
	job.Env = map[string]string{"EXPECTED_PRE": githubHash("before pre"), "EXPECTED_MAIN": githubHash("after pre")}
	job.Actions = []plan.ActionLock{
		remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
		remoteLifecycleLock(childID, "child", digest, nil),
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: requireNode24(t), Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() composite lifecycle hashing result = %#v, %v", result, err)
	}
	phases, err := os.ReadFile(filepath.Join(workspace, "phases"))
	if err != nil || string(phases) != "pre\nmain\npost\n" {
		t.Fatalf("composite lifecycle phases = %q, %v", phases, err)
	}
}

func TestCompositePreHashFilesUsesResolvedDeadline(t *testing.T) {
	for _, test := range []struct {
		name     string
		resolved bool
		env      map[string]string
		with     map[string]string
		preIf    string
	}{
		{name: "first pre input", with: map[string]string{"key": "${{ hashFiles('payload') }}"}},
		{name: "later pre input", resolved: true, with: map[string]string{"key": "${{ hashFiles('payload') }}"}},
		{name: "later pre environment", resolved: true, env: map[string]string{"HASH": "${{ hashFiles('payload') }}"}},
		{name: "later pre condition", resolved: true, preIf: "hashFiles('payload') != ''"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, remote := t.TempDir(), t.TempDir()
			writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: composite pre deadline\n")
			writeFixtureFile(t, remote, "child/action.yml", fmt.Sprintf("inputs:\n  key:\n    required: false\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  pre-if: %q\n", test.preIf))
			writeFixtureFile(t, remote, "child/pre.js", "")
			writeFixtureFile(t, remote, "child/main.js", "")
			lockID := remoteLifecycleLockID(1)
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "child", Kind: "uses", Uses: remoteLifecycleUses("child"), Action: &plan.ActionSelector{Lock: lockID}, Env: test.env, With: test.with}})
			job.RequiredCapabilities = []string{"network"}
			job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "child", digestTree(t, remote), nil)}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: job.Actions[0].SourceDigest}}
			if err := attachTestActionPrograms(&job, workspace, materializer); err != nil {
				t.Fatal(err)
			}
			stopHash := errors.New("stop at hash callback")
			var hashContext context.Context
			eval := expression.Context{Matrix: map[string]any{"timeout": 1}, HashFilesContext: func(ctx context.Context, _ []string) (string, error) {
				hashContext = ctx
				return "", stopHash
			}}
			timeout := &remotePreparationTimeout{step: executionprogram.Step{TimeoutMinutes: executionprogram.NumberControl{Expression: &executionprogram.Site{Source: "${{ matrix.timeout }}", Surface: executionprogram.SurfaceStepControl, Result: executionprogram.ResultNumber}}}, eval: eval}
			defer timeout.close()
			if test.resolved {
				if _, err := timeout.context(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			run := newJobRun(Runner{})
			_, err := run.prepareRemoteAction(t.Context(), newCommandOutputProcessor(io.Discard, io.Discard), workspace, job.Program.Job.Steps[0], "0/0", nil, eval, &postRegistry{}, newActionLockResolver(job, workspace, materializer), remotePreparations{}, &remotePreparationStatus{}, false, nil, timeout, nil)
			if !errors.Is(err, stopHash) || hashContext == nil || timeout.bounded == nil {
				t.Fatalf("preparation hashing = %v, context = %v, timeout = %v", err, hashContext, timeout.bounded)
			}
			want, _ := timeout.bounded.Deadline()
			if got, ok := hashContext.Deadline(); !ok || !got.Equal(want) {
				t.Fatalf("hash deadline = %v (%v), want resolved deadline %v", got, ok, want)
			}
		})
	}
}

func TestHashWorkspaceFilesEnforcesEveryBound(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "one", "1")
	writeFixtureFile(t, workspace, "two", "22")
	base := hashFilesLimits{patterns: 10, patternBytes: 20, totalPatternBytes: 40, matches: 10, bytes: 10, entries: 10}
	for _, test := range []struct {
		name     string
		patterns []string
		limits   hashFilesLimits
		want     string
	}{
		{name: "pattern count", patterns: []string{"one", "two"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.patterns = 1 }), want: "1 to 1 patterns"},
		{name: "pattern length", patterns: []string{"one"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.patternBytes = 2 }), want: "exceeds 2 bytes"},
		{name: "total pattern length", patterns: []string{"one", "two"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.totalPatternBytes = 5 }), want: "exceed 5 total bytes"},
		{name: "matched files", patterns: []string{"*"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.matches = 1 }), want: "more than 1 files"},
		{name: "hashed bytes", patterns: []string{"*"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.bytes = 2 }), want: "selected bytes exceed 2"},
		{name: "workspace entries", patterns: []string{"missing*"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.entries = 1 }), want: "more than 1 entries"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, test.patterns, test.limits, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("bound error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHashWorkspaceFilesBoundsSingleDirectoryEnumeration(t *testing.T) {
	workspace := t.TempDir()
	for i := range 300 {
		writeFixtureFile(t, workspace, fmt.Sprintf("entry-%03d", i), "")
	}
	limits := defaultHashFilesLimits
	limits.entries = 10
	if _, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{"missing*"}, limits, false); err == nil || !strings.Contains(err.Error(), "more than 10 entries") {
		t.Fatalf("single-directory entry bound error = %v", err)
	}
}

func TestHashWorkspaceFilesPrunesUnrelatedEntries(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "packages/service/value", "value")
	for i := range 30 {
		writeFixtureFile(t, workspace, fmt.Sprintf("unrelated-%02d", i), "ignored")
		writeFixtureFile(t, workspace, fmt.Sprintf("packages/other/entry-%02d", i), "ignored")
	}
	for _, patterns := range [][]string{
		{"packages/service/value"},
		{"packages/service/*"},
		{"packages/service/**"},
		{"packages/service/", "!packages/**", "packages/service/value"},
	} {
		limits := defaultHashFilesLimits
		limits.entries = 5
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			limits.entries = 40 // These platforms enumerate to preserve path spelling.
		}
		limits.beforeDirectoryOpen = func(name string) {
			if name == "packages/other" {
				t.Fatal("opened an unrelated subtree")
			}
		}
		got, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, patterns, limits, false)
		if err != nil || got != githubHash("value") {
			t.Fatalf("hashFiles(%v) = %q, %v", patterns, got, err)
		}
	}
	limits := defaultHashFilesLimits
	limits.entries = 1
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		limits.entries = 40
	}
	for _, patterns := range [][]string{{"missing"}, {"!**"}, {"# comment"}} {
		if got, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, patterns, limits, false); err != nil || got != "" {
			t.Fatalf("hashFiles(%v) = %q, %v", patterns, got, err)
		}
	}
}

func TestHashWorkspaceFilesPruningPreservesMatches(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"root", "a/value", "a/nested/value", "b/value", "b/deep/c/value", "c/other", ".hidden/value", "literal/{name}"} {
		writeFixtureFile(t, workspace, name, name)
	}
	for _, pattern := range []string{"a", "a/", "a/*", "*/value", "[ab]/value", "**/value", "a/**/value", "*/**/c/*", ".hidden", "literal/{name}", "A/VALUE", `a\/value`, "a[/]value", "a[!x]value", "a[.-0]value"} {
		for _, insensitive := range []bool{false, true} {
			got, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{pattern}, defaultHashFilesLimits, insensitive)
			// An excluded broad positive forces a full walk while leaving the
			// selected set unchanged, providing an oracle for traversal pruning.
			want, wantErr := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{"**", "!**", pattern}, defaultHashFilesLimits, insensitive)
			if err != nil || wantErr != nil || got != want {
				t.Fatalf("pattern %q (insensitive %v): %q, %v; full walk %q, %v", pattern, insensitive, got, err, want, wantErr)
			}
		}
	}
}

func TestHashWorkspaceFilesCharacterClassesAcrossDirectories(t *testing.T) {
	for _, prefix := range []string{"", "packages/", "literal{}/"} {
		t.Run(prefix, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, prefix+"a/value", "value")
			for _, pattern := range []string{"a[/]value", "a[!x]value", "a[.-0]value"} {
				got, err := hashWorkspaceFiles(t.Context(), workspace, []string{prefix + pattern})
				if err != nil || got != githubHash("value") {
					t.Fatalf("hashFiles(%q) = %q, %v; want %q", prefix+pattern, got, err, githubHash("value"))
				}
			}
		})
	}
}

func TestHashWorkspaceFilesExecutionBudget(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "nested/value", "value")
	for _, phase := range []string{"traversal", "hashing", "verification"} {
		t.Run(phase, func(t *testing.T) {
			limits := defaultHashFilesLimits
			limits.duration = 20 * time.Millisecond
			pause := func(string) { time.Sleep(2 * limits.duration) }
			switch phase {
			case "traversal":
				limits.beforeDirectoryOpen = pause
			case "hashing":
				limits.beforeOpen = pause
			case "verification":
				limits.afterFileHash = pause
			}
			got, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{"nested/value", "!nested/skip", "other/*.txt"}, limits, false)
			if got != "" || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "hashFiles exceeded its 20ms execution limit") {
				t.Fatalf("budget result = %q, %v", got, err)
			}
			wantHint := `positive patterns ["nested/value" "other/*.txt"]; use more specific paths to reduce traversal and hashing`
			if !strings.Contains(err.Error(), wantHint) {
				t.Fatalf("budget error = %v, want %q", err, wantHint)
			}
		})
	}
	for _, expired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if expired {
			cancel()
			ctx, cancel = context.WithTimeout(t.Context(), -time.Second)
		}
		cancel()
		_, err := hashWorkspaceFiles(ctx, workspace, []string{"missing"})
		if !errors.Is(err, ctx.Err()) || strings.Contains(err.Error(), "execution limit") || !strings.Contains(err.Error(), "hashFiles interrupted by step or job cancellation/deadline") {
			t.Fatalf("parent cancellation = %v, want %v", err, ctx.Err())
		}
	}
}

func TestHashWorkspaceFilesEntryBudgetDiagnostic(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "nested/value", "value")
	writeFixtureFile(t, workspace, "other/value.txt", "value")
	limits := defaultHashFilesLimits
	limits.entries = 1
	got, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{"nested/*", "!nested/skip", "other/*.txt"}, limits, false)
	want := `hashFiles traversal inspected more than 1 entries while searching positive patterns ["nested/*" "other/*.txt"]; use more specific paths to reduce traversal and hashing`
	if got != "" || err == nil || err.Error() != want {
		t.Fatalf("entry budget result = %q, %v; want %q", got, err, want)
	}
}

func TestHashWorkspaceFilesDetectsMutationAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "mutable", "before")
	limits := defaultHashFilesLimits
	limits.beforeOpen = func(string) {
		if err := os.WriteFile(filepath.Join(workspace, "mutable"), []byte("after mutation"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{"mutable"}, limits, false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("mutation error = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := hashWorkspaceFiles(ctx, workspace, []string{"mutable"}); err == nil {
		t.Fatal("cancelled hashFiles succeeded")
	}
}

func TestHashWorkspaceFilesDetectsDirectoryReplacementBeforeTraversal(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "nested/value", "original")
	writeFixtureFile(t, workspace, "target/value", "replacement")
	limits := defaultHashFilesLimits
	replaced := false
	limits.beforeDirectoryOpen = func(name string) {
		if name != "nested" || replaced {
			return
		}
		replaced = true
		if err := os.Rename(filepath.Join(workspace, "nested"), filepath.Join(workspace, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target", filepath.Join(workspace, "nested")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if _, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{"nested/**"}, limits, false); err == nil || !strings.Contains(err.Error(), "changed before traversal") {
		t.Fatalf("directory replacement error = %v", err)
	}
}

func TestHashWorkspaceFilesOpensMatchesThroughPinnedDirectories(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "nested/value", "original")
	writeFixtureFile(t, workspace, "replacement/value", "replacement")
	limits := defaultHashFilesLimits
	limits.beforeOpen = func(name string) {
		if name != "nested/value" {
			return
		}
		if err := os.Rename(filepath.Join(workspace, "nested"), filepath.Join(workspace, "original")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(workspace, "replacement"), filepath.Join(workspace, "nested")); err != nil {
			t.Fatal(err)
		}
	}
	limits.afterFileHash = func(name string) {
		if name != "nested/value" {
			return
		}
		if err := os.Rename(filepath.Join(workspace, "nested"), filepath.Join(workspace, "replacement")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(workspace, "original"), filepath.Join(workspace, "nested")); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := hashWorkspaceFilesWithLimits(t.Context(), workspace, []string{"nested/value"}, limits, false)
	if err != nil {
		t.Fatal(err)
	}
	want := githubHash("original")
	if digest != want {
		t.Fatalf("digest = %q, want original file digest %q", digest, want)
	}
}

func TestHashWorkspaceFilesFinalVerificationObservesCancellation(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "nested/value", "value")
	ctx, cancel := context.WithCancel(t.Context())
	limits := defaultHashFilesLimits
	limits.afterFileHash = func(string) { cancel() }
	if _, err := hashWorkspaceFilesWithLimits(ctx, workspace, []string{"nested/value"}, limits, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("hashWorkspaceFilesWithLimits() error = %v, want context cancellation", err)
	}
}

func TestHashWorkspaceFilesRejectsFIFORacedIntoMatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs are unavailable on Windows")
	}
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "value", "value")
	limits := defaultHashFilesLimits
	limits.beforeOpen = func(name string) {
		file := filepath.Join(workspace, name)
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if err := testMkfifo(file, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := hashWorkspaceFilesWithLimits(ctx, workspace, []string{"value"}, limits, false); err == nil || !strings.Contains(err.Error(), "changed before hashing") {
		t.Fatalf("FIFO race error = %v", err)
	}
}

func TestHashFilesStepEnvironmentFailureUsesStepConclusion(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles environment failure\n")
	writeFixtureFile(t, workspace, "target", "value")
	if err := os.Symlink("target", filepath.Join(workspace, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	marker := filepath.Join(workspace, "continued")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
		{ID: "invalid", Kind: "run", Shell: "sh", ContinueOnError: true, Env: map[string]string{"HASH": "${{ hashFiles('link') }}"}, Command: "exit 99"},
		{ID: "after", Kind: "run", Shell: "sh", Condition: "steps.invalid.outcome == 'failure' && steps.invalid.conclusion == 'success'", Command: "touch " + marker},
	})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later step did not run: %v", err)
	}
}

func TestHashFilesStepConditionFailureRunsFailureCleanup(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles condition failure\n")
	writeFixtureFile(t, workspace, "target", "value")
	if err := os.Symlink("target", filepath.Join(workspace, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	marker := filepath.Join(workspace, "cleaned")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
		{ID: "invalid", Kind: "run", Shell: "sh", Condition: "hashFiles('link') != ''", Command: "exit 99"},
		{ID: "cleanup", Kind: "run", Shell: "sh", Condition: "failure()", Command: "touch " + marker},
	})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failure cleanup did not run: %v", err)
	}
}

func TestHashFilesSkippedStepsDoNotAccessWorkspace(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: skipped hashFiles\n")
	writeFixtureFile(t, workspace, "target", "value")
	if err := os.Symlink("target", filepath.Join(workspace, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	envMarker := filepath.Join(workspace, "env-ran")
	conditionMarker := filepath.Join(workspace, "condition-ran")
	cleanupMarker := filepath.Join(workspace, "cleaned")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{
		{ID: "fails", Kind: "run", Shell: "sh", Command: "exit 1"},
		{ID: "env", Kind: "run", Shell: "sh", Env: map[string]string{"HASH": "${{ hashFiles('link') }}"}, Command: "touch " + envMarker},
		{ID: "condition", Kind: "run", Shell: "sh", Condition: "hashFiles('link') != ''", Command: "touch " + conditionMarker},
		{ID: "cleanup", Kind: "run", Shell: "sh", Condition: "always()", Command: "touch " + cleanupMarker},
	})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	for _, marker := range []string{envMarker, conditionMarker} {
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("skipped step marker %q stat error = %v", marker, statErr)
		}
	}
	if _, err := os.Stat(cleanupMarker); err != nil {
		t.Fatalf("cleanup did not run: %v", err)
	}
}

func TestHashFilesRemotePreFailureUsesStepConclusion(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: hashFiles remote pre failure\n")
	writeFixtureFile(t, workspace, "target", "value")
	if err := os.Symlink("target", filepath.Join(workspace, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action/action.yml", "name: pre failure\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "action/pre.js", "")
	writeFixtureFile(t, remote, "action/main.js", "")
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	marker := filepath.Join(workspace, "continued")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "invalid", Kind: "uses", Uses: remoteLifecycleUses("action"), Action: &plan.ActionSelector{Lock: lockID}, ContinueOnError: true, Env: map[string]string{"HASH": "${{ hashFiles('link') }}"}},
		{ID: "after", Kind: "run", Shell: "sh", Condition: "steps.invalid.outcome == 'failure' && steps.invalid.conclusion == 'success'", Command: "touch " + marker},
	})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "action", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	var stderr bytes.Buffer
	result, err := (Runner{Actions: materializer, Stderr: &stderr}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Count(stderr.String(), "continue-on-error") != 1 || !strings.Contains(stderr.String(), `env: evaluate "HASH": hashFiles matched symlink "link"`) {
		t.Fatalf("tolerated pre failure diagnostic = %q", stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later step did not run: %v", err)
	}
}

func TestCompositeHashFilesPreFailureWarning(t *testing.T) {
	for _, test := range []struct {
		runMain         bool
		parentTolerates bool
		mainFailure     bool
	}{{true, false, false}, {false, false, false}, {true, true, false}, {false, true, false}, {true, true, true}} {
		t.Run(fmt.Sprintf("run-main=%t/parent-tolerates=%t/main-failure=%t", test.runMain, test.parentTolerates, test.mainFailure), func(t *testing.T) {
			workspace, remote := t.TempDir(), t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: composite pre warning\n")
			writeFixtureFile(t, remote, "root/action.yml", `runs:
  using: composite
  steps:
    - uses: owner/repo/child@v1
      continue-on-error: true
      with:
        key: ${{ hashFiles('../outside') }}
    - shell: sh
      run: echo continued
`)
			if test.parentTolerates {
				writeFixtureFile(t, remote, "root/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: owner/repo/middle@v1\n      continue-on-error: true\n    - shell: sh\n      run: echo continued\n")
				writeFixtureFile(t, remote, "middle/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: owner/repo/child@v1\n      with:\n        key: ${{ hashFiles('../outside') }}\n")
			}
			if test.mainFailure {
				writeFixtureFile(t, remote, "root/action.yml", `runs:
  using: composite
  steps:
    - id: gate
      shell: sh
      run: echo 'FAIL_MAIN=true' >> "$GITHUB_ENV"
    - uses: owner/repo/middle@v1
      continue-on-error: true
      env:
        MAIN: ${{ env.FAIL_MAIN == 'true' && hashFiles('../main') || '' }}
    - shell: sh
      run: echo continued
`)
			}
			writeFixtureFile(t, remote, "child/action.yml", "inputs:\n  key:\n    required: true\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
			writeFixtureFile(t, remote, "child/pre.js", "")
			writeFixtureFile(t, remote, "child/main.js", "")
			digest := digestTree(t, remote)
			rootID, childID := remoteLifecycleLockID(1), remoteLifecycleLockID(2)
			job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
				{ID: "gate", Kind: "run", Shell: "sh", Command: fmt.Sprintf("echo 'run=%t' >> \"$GITHUB_OUTPUT\"", test.runMain)},
				{ID: "root", Kind: "uses", Uses: remoteLifecycleUses("root"), Action: &plan.ActionSelector{Lock: rootID}, Condition: "steps.gate.outputs.run == 'true'"},
			})
			job.RequiredCapabilities = []string{"network"}
			job.Actions = []plan.ActionLock{
				remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}),
				remoteLifecycleLock(childID, "child", digest, nil),
			}
			if test.parentTolerates {
				middleID := remoteLifecycleLockID(3)
				job.Actions[0] = remoteLifecycleLock(rootID, "root", digest, map[string]plan.ActionSelector{remoteLifecycleUses("middle"): {Lock: middleID}})
				job.Actions = append(job.Actions, remoteLifecycleLock(middleID, "middle", digest, map[string]plan.ActionSelector{remoteLifecycleUses("child"): {Lock: childID}}))
			}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
			var stdout, stderr bytes.Buffer
			result, err := (Runner{Actions: materializer, Stdout: &stdout, Stderr: &stderr}).runTestJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" || strings.Contains(stdout.String(), "continued") != test.runMain {
				t.Fatalf("tolerated pre result = %#v, %v, stdout = %s", result, err, stdout.String())
			}
			wantCount, wantStep := 1, 1
			if test.mainFailure {
				wantCount, wantStep = 2, 2
				for _, diagnostic := range []string{`env: evaluate "MAIN"`, `with: evaluate "key"`} {
					if strings.Count(stderr.String(), diagnostic) != 1 || strings.Count(result.WarningAnnotations, html.EscapeString(diagnostic)) != 1 {
						t.Fatalf("distinct failure %q missing/duplicated: stderr = %q, annotations = %q", diagnostic, stderr.String(), result.WarningAnnotations)
					}
				}
			}
			if strings.Count(stderr.String(), "continue-on-error") != wantCount || !strings.Contains(stderr.String(), fmt.Sprintf("composite action %q step %d", "root", wantStep)) || !strings.Contains(stderr.String(), `with: evaluate "key": hashFiles pattern 1 may not contain ".."`) || strings.Count(result.WarningAnnotations, "hashFiles pattern 1") != wantCount {
				t.Fatalf("pre diagnostic = %q, annotations = %q", stderr.String(), result.WarningAnnotations)
			}
		})
	}
}

func TestHashFilesInterpolationUsesStepTimeoutContext(t *testing.T) {
	for _, test := range []struct {
		name      string
		env       map[string]string
		condition string
		composite bool
	}{
		{name: "environment", env: map[string]string{"HASH": "${{ hashFiles('large') }}"}},
		{name: "condition", condition: "hashFiles('large') != ''"},
		{name: "composite environment", env: map[string]string{"HASH": "${{ hashFiles('large') }}"}, composite: true},
		{name: "composite condition", condition: "hashFiles('large') != ''", composite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles timeout\n")
			large := filepath.Join(workspace, "large")
			file, err := os.Create(large)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(256 << 20); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{
				ID:             "hash",
				Kind:           "run",
				Shell:          "sh",
				TimeoutMinutes: 0.001,
				Env:            test.env,
				Condition:      test.condition,
				Command:        "true",
			}})
			if test.composite {
				writeFixtureFile(t, workspace, ".github/actions/hash/action.yml", fmt.Sprintf("runs:\n  using: composite\n  steps:\n    - shell: sh\n      if: %q\n      env:\n        HASH: %q\n      run: true\n", test.condition, test.env["HASH"]))
				job = runtimePlan(t, workspace, ".github/workflows/test.yml", []runtimeTestStep{{ID: "hash", Kind: "uses", Uses: "./.github/actions/hash", TimeoutMinutes: 0.001}})
			}
			started := time.Now()
			_, err = (Runner{}).runTestJob(t.Context(), job, workspace)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("RunJob() timeout error = %v", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("RunJob() took %s after step timeout", elapsed)
			}
		})
	}
}

func TestHashFilesPrePhaseUsesStepTimeoutContext(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: hashFiles pre timeout\n")
	file, err := os.Create(filepath.Join(workspace, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(256 << 20); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action/action.yml", "name: timeout\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "action/pre.js", "")
	writeFixtureFile(t, remote, "action/main.js", "")
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:             "hash",
		Kind:           "uses",
		Uses:           remoteLifecycleUses("action"),
		Action:         &plan.ActionSelector{Lock: lockID},
		TimeoutMinutes: 0.001,
		Env:            map[string]string{"HASH": "${{ hashFiles('large') }}"},
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "action", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	started := time.Now()
	_, err = (Runner{Actions: materializer}).runTestJob(t.Context(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() pre timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("RunJob() pre took %s after step timeout", elapsed)
	}
}

func githubHash(contents ...string) string {
	if len(contents) == 0 {
		return ""
	}
	combined := sha256.New()
	for _, content := range contents {
		digest := sha256.Sum256([]byte(content))
		_, _ = combined.Write(digest[:])
	}
	return hex.EncodeToString(combined.Sum(nil))
}

func withHashLimits(base hashFilesLimits, update func(*hashFilesLimits)) hashFilesLimits {
	copy := base
	update(&copy)
	return copy
}

func TestGitHubHashTestHelperUsesBinaryDigests(t *testing.T) {
	first, second := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b"))
	joined := append(append([]byte(nil), first[:]...), second[:]...)
	want := sha256.Sum256(joined)
	if got := githubHash("a", "b"); got != hex.EncodeToString(want[:]) {
		t.Fatalf("githubHash() = %q", got)
	}
	if reflect.DeepEqual(first[:], []byte(hex.EncodeToString(first[:]))) {
		t.Fatal("test helper unexpectedly hashes hexadecimal digests")
	}
}
