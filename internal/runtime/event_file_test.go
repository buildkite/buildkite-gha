package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func setEventFilePayload(t *testing.T, job *plan.Job, source string) []byte {
	t.Helper()
	payload, err := plan.DecodeEventPayload([]byte(source), transport.Digest([]byte(source)))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Event.Payload = &payload
	job.Event.PayloadDigest = transport.Digest(encoded)
	job.Event.PayloadArtifact = true
	job.Event.PayloadFile = true
	return encoded
}

func TestEventFileShellJavaScriptAndNestedCompositeLifecycle(t *testing.T) {
	t.Setenv("GITHUB_EVENT_PATH", "/import-time-event-file")
	node := requireNode24(t)
	for _, event := range []struct{ name, payload string }{
		{"issues", `{"action":"opened","issue":{"number":42,"body":"unlogged-payload"},"unknown":{"id":9007199254740993}}`},
		{"issue_comment", `{"action":"created","issue":{"number":42,"pull_request":{"url":"https://api.github.com/repos/acme/repo/pulls/42"}},"comment":{"body":"unlogged-payload"},"unknown":{"id":9007199254740993}}`},
	} {
		t.Run(event.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, ".github/actions/js/action.yml", `name: event reader
inputs:
  path:
    default: ${{ github.event_path }}
runs:
  using: node24
  pre: pre.js
  pre-if: github.event_path != ''
  main: main.js
  post: post.js
  post-if: github.event_path != ''
`)
			for _, phase := range []string{"pre", "main", "post"} {
				writeFixtureFile(t, workspace, ".github/actions/js/"+phase+".js", `
const fs = require('node:fs');
const assert = require('node:assert/strict');
assert.equal(process.env.INPUT_PATH, process.env.GITHUB_EVENT_PATH);
assert.equal(process.env.EXPRESSION_PATH, process.env.GITHUB_EVENT_PATH);
const content = fs.readFileSync(process.env.GITHUB_EVENT_PATH, 'utf8');
assert.equal(content, fs.readFileSync('expected.json', 'utf8'));
assert.equal(JSON.parse(content).issue.number, 42);
assert.ok(content.includes('9007199254740993'));
fs.appendFileSync('paths', process.env.GITHUB_EVENT_PATH + '\n');
`)
			}
			shell := `test "$EXPRESSION_PATH" = "$GITHUB_EVENT_PATH"; cmp '${{ github.event_path }}' expected.json; printf '%s\n' '${{ github['event_path'] }}' >> paths; echo GITHUB_EVENT_PATH=/spoofed-file-command >> "$GITHUB_ENV"`
			writeFixtureFile(t, workspace, ".github/actions/inner/action.yml", `name: inner
inputs:
  path:
    required: true
runs:
  using: composite
  steps:
    - if: github.event_path == inputs.path
      shell: sh
      env:
        GITHUB_EVENT_PATH: /spoofed-composite
      run: `+shell+`
    - uses: ./.github/actions/js
      with:
        path: ${{ github.event_path }}
`)
			writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", `name: outer
inputs:
  path:
    default: ${{ github.event_path }}
outputs:
  path:
    value: ${{ github.event_path }}
runs:
  using: composite
  steps:
    - uses: ./.github/actions/inner
      with:
        path: ${{ inputs.path }}
`)
			source := `name: event file
on: [issues, issue_comment]
jobs:
  read:
    runs-on: ubuntu-latest
    env:
      GITHUB_EVENT_PATH: /spoofed-job
      EXPRESSION_PATH: ${{ github.event_path }}
    outputs:
      path: ${{ steps.composite.outputs.path }}
      direct: ${{ github.event_path }}
    steps:
      - id: shell
        if: github.event_path != ''
        env:
          GITHUB_EVENT_PATH: /spoofed-step
        run: ` + shell + `
      - id: js
        uses: ./.github/actions/js
      - id: composite
        uses: ./.github/actions/outer
      - if: github.event_path == ''
        run: exit 99
`
			workflowPath := ".github/workflows/event.yml"
			writeFixtureFile(t, workspace, workflowPath, source)
			options := compiler.DefaultOptions()
			options.EventFile = true
			eventSource := []byte(fmt.Sprintf(`{"provider":"github","event":%q,"repository":{"owner":"acme","name":"repo"},"ref":"refs/heads/main","sha":"%s","actor":"octocat","payload":%s}`, event.name, strings.Repeat("1", 40), event.payload))
			bundle, err := compiler.CompileBundleWithOptions(filepath.Join(workspace, workflowPath), []byte(source), eventSource, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "importer", options)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(bundle.Plans[0].Contents, []byte("/import-time-event-file")) || !bytes.Contains(bundle.Plans[0].Contents, []byte("github.event_path")) {
				t.Fatal("plan must retain the event path expression, not an importer path")
			}
			job, err := plan.Decode(bundle.Plans[0].Contents)
			if err != nil {
				t.Fatal(err)
			}
			if job.Event.Payload != nil || !job.Event.PayloadFile || !job.Event.PayloadArtifact || job.GitHubToken != nil || len(job.RequiredSecrets) != 0 {
				t.Fatal("event path changed payload provenance or credential authority")
			}
			payload := bundle.EventArtifact.Contents
			hydrated, err := plan.DecodeEventPayload(payload, job.Event.PayloadDigest)
			if err != nil {
				t.Fatal(err)
			}
			job.Event.Payload = &hydrated
			writeFixtureFile(t, workspace, "expected.json", string(payload))
			var logs bytes.Buffer
			result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("event readers failed: %v; logs: %s", err, &logs)
			}
			if strings.Contains(logs.String(), "unlogged-payload") {
				t.Fatal("event payload leaked to logs")
			}
			paths, err := os.ReadFile(filepath.Join(workspace, "paths"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Fields(string(paths))
			if len(lines) != 8 {
				t.Fatalf("got %d reads, want shell, composite, direct and nested JS pre/main/post", len(lines))
			}
			if result.Outputs["path"] != lines[0] || result.Outputs["direct"] != lines[0] {
				t.Fatalf("event path outputs = %v, want %s", result.Outputs, lines[0])
			}
			for _, path := range lines {
				if path != lines[0] {
					t.Fatal("event path changed during the job")
				}
			}
			if _, err := os.Stat(filepath.Dir(lines[0])); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("event directory remains after post: %v", err)
			}
		})
	}
}

func TestEventFileIsolationAndCleanup(t *testing.T) {
	runner := Runner{}
	paths := map[string]bool{}
	for _, outcome := range []string{"success", "failure", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, "workflow.yml", "name: event lifetime\n")
			step := runtimeTestStep{ID: "read", Kind: "run", Shell: "sh", Command: `test -s '${{ github.event_path }}'; printf '%s' '${{ github.event_path }}' > path`}
			switch outcome {
			case "failure":
				step.Command += "; exit 1"
			case "timeout":
				step.Command += "; sleep 30"
				step.TimeoutMinutes = 0.01
			}
			job := runtimePlan(t, workspace, "workflow.yml", []runtimeTestStep{step})
			setEventFilePayload(t, &job, `{}`)
			_, err := runner.runTestJob(t.Context(), job, workspace)
			if (err == nil) != (outcome == "success") {
				t.Fatalf("%s error = %v", outcome, err)
			}
			path, err := os.ReadFile(filepath.Join(workspace, "path"))
			if err != nil {
				t.Fatal(err)
			}
			if paths[string(path)] {
				t.Fatal("jobs shared an event file")
			}
			paths[string(path)] = true
			if _, err := os.Stat(filepath.Dir(string(path))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("event directory remains: %v", err)
			}
		})
	}
}

func TestEventFileAbsentWithoutFullPayload(t *testing.T) {
	workspace := t.TempDir()
	source := `on: push
jobs:
  absent:
    runs-on: ubuntu-latest
    steps:
      - shell: sh
        env:
          HAS_REF: ${{ contains(toJSON(github.event), 'refs/heads/main') }}
        run: test "$HAS_REF" = true; test -z "${GITHUB_EVENT_PATH+x}"; test -z '${{ github.event_path }}'
`
	writeFixtureFile(t, workspace, "workflow.yml", source)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	// No EventFile option: model a synthesized event retained for expressions.
	bundle, err := compiler.CompileBundle(filepath.Join(workspace, "workflow.yml"), []byte(source), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "importer")
	if err != nil {
		t.Fatal(err)
	}
	job := bundle.Plans[0].Job
	if job.Event.PayloadFile || !job.Event.PayloadArtifact {
		t.Fatal("event path expression changed event-file provenance")
	}
	payload, err := plan.DecodeEventPayload(bundle.EventArtifact.Contents, job.Event.PayloadDigest)
	if err != nil {
		t.Fatal(err)
	}
	job.Event.Payload = &payload
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err != nil {
		t.Fatal(err)
	}
	job.Event.PayloadFile = true
	job.Event.Payload = nil
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "hydrated payload") {
		t.Fatalf("missing payload error = %v", err)
	}
}

func TestEventFilePermissions(t *testing.T) {
	for _, docker := range []bool{false, true} {
		t.Run(fmt.Sprint(docker), func(t *testing.T) {
			job := plan.Job{}
			setEventFilePayload(t, &job, `{}`)
			path, err := newEventFile(job.Event, docker)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(filepath.Dir(path)) }()
			wantFile, wantDir := os.FileMode(0o400), os.FileMode(0o700)
			if docker {
				wantFile, wantDir = 0o444, 0o711
			}
			for name, mode := range map[string]os.FileMode{path: wantFile, filepath.Dir(path): wantDir} {
				info, err := os.Stat(name)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("event file permissions: %v, want %o", err, mode)
				}
			}
		})
	}
}
