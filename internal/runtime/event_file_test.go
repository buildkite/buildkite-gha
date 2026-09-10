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
	node := requireNode24(t)
	for _, event := range []struct{ name, payload string }{
		{"issues", `{"action":"opened","issue":{"number":42,"body":"unlogged-payload"},"unknown":{"id":9007199254740993}}`},
		{"issue_comment", `{"action":"created","issue":{"number":42,"pull_request":{"url":"https://api.github.com/repos/acme/repo/pulls/42"}},"comment":{"body":"unlogged-payload"},"unknown":{"id":9007199254740993}}`},
	} {
		t.Run(event.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, "workflow.yml", "name: event file\n")
			writeFixtureFile(t, workspace, ".github/actions/js/action.yml", "name: event reader\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
			for _, phase := range []string{"pre", "main", "post"} {
				writeFixtureFile(t, workspace, ".github/actions/js/"+phase+".js", `
const fs = require('node:fs');
const assert = require('node:assert/strict');
const content = fs.readFileSync(process.env.GITHUB_EVENT_PATH, 'utf8');
assert.equal(content, fs.readFileSync('expected.json', 'utf8'));
assert.equal(JSON.parse(content).issue.number, 42);
assert.ok(content.includes('9007199254740993'));
fs.appendFileSync('paths', process.env.GITHUB_EVENT_PATH + '\n');
`)
			}
			shell := `cmp "$GITHUB_EVENT_PATH" expected.json; printf '%s\n' "$GITHUB_EVENT_PATH" >> paths; echo GITHUB_EVENT_PATH=/spoofed-file-command >> "$GITHUB_ENV"`
			writeFixtureFile(t, workspace, ".github/actions/inner/action.yml", "name: inner\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      env:\n        GITHUB_EVENT_PATH: /spoofed-composite\n      run: "+shell+"\n")
			writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", "name: outer\nruns:\n  using: composite\n  steps:\n    - uses: ./.github/actions/inner\n")
			job := runtimePlan(t, workspace, "workflow.yml", []runtimeTestStep{
				{ID: "shell", Kind: "run", Shell: "sh", Command: shell, Env: map[string]string{"GITHUB_EVENT_PATH": "/spoofed-step"}},
				{ID: "js", Kind: "uses", Uses: "./.github/actions/js"},
				{ID: "composite", Kind: "uses", Uses: "./.github/actions/outer"},
			})
			job.Env = map[string]string{"GITHUB_EVENT_PATH": "/spoofed-job"}
			job.Event.Name = event.name
			payload := setEventFilePayload(t, &job, event.payload)
			writeFixtureFile(t, workspace, "expected.json", string(payload))
			var logs bytes.Buffer
			result, err := (Runner{Node24: node, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
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
			if len(lines) != 5 {
				t.Fatalf("got %d reads, want shell, composite, JS pre/main/post", len(lines))
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
			step := runtimeTestStep{ID: "read", Kind: "run", Shell: "sh", Command: `test -s "$GITHUB_EVENT_PATH"; printf '%s' "$GITHUB_EVENT_PATH" > path`}
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
	writeFixtureFile(t, workspace, "workflow.yml", "name: no webhook\n")
	job := runtimePlan(t, workspace, "workflow.yml", []runtimeTestStep{{ID: "absent", Kind: "run", Shell: "sh", Command: `test -z "${GITHUB_EVENT_PATH+x}"`}})
	setEventFilePayload(t, &job, `{"ref":"refs/heads/main"}`)
	job.Event.PayloadFile = false // A synthesized event retained for expressions.
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err != nil {
		t.Fatal(err)
	}
	job.Event.PayloadFile = true
	job.Event.Payload = nil
	if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "hydrated payload") {
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
