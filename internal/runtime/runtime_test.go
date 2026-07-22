package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestJavaScriptPreMainPostFilesAndMasking(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "javascript", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "hello"}}})
	job.Outputs = map[string]string{"result": "${{ steps.javascript.outputs.result }}"}
	result, err := runner.RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if got := result.Outputs["result"]; got != "hello-javascript" {
		t.Errorf("output = %q, want hello-javascript", got)
	}
	if got := result.Env["RUNTIME_SEEN"]; got != "true" {
		t.Errorf("environment = %q, want true", got)
	}
	if got := result.State["phase"]; got != "main" {
		t.Errorf("state = %q, want main", got)
	}
	if got := result.State["pre"]; got != "ready" {
		t.Errorf("pre state = %q, want ready", got)
	}
	if result.Summary != "runtime main summary\nruntime post single\n" {
		t.Errorf("summary = %q", result.Summary)
	}
	if strings.Contains(logs.String(), "runtime-secret-value") {
		t.Fatalf("raw forwarded logs contain literal secret: %q", logs.String())
	}
	for _, event := range []string{"lifecycle:pre", "lifecycle:main", "masked probe: ***", "lifecycle:post:single"} {
		if !strings.Contains(logs.String(), event) {
			t.Errorf("logs = %q, want event %q", logs.String(), event)
		}
	}
	pre := strings.Index(logs.String(), "lifecycle:pre")
	main := strings.Index(logs.String(), "lifecycle:main")
	post := strings.Index(logs.String(), "lifecycle:post:single")
	if pre > main || main > post {
		t.Errorf("lifecycle logs are out of order: %q", logs.String())
	}
}

func TestPostActionsRunLIFOAfterMainFailure(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	workspace := fixturePath(t)
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{
		{ID: "one", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "one", "order": "one"}},
		{ID: "two", Kind: "uses", Uses: "./actions/javascript", With: map[string]string{"message": "two", "order": "two", "fail": "true"}},
	})
	_, err := runner.RunJob(context.Background(), job, workspace)
	if err == nil {
		t.Fatal("RunJob() error = nil, want main failure")
	}
	if !strings.Contains(logs.String(), "requested main failure") {
		t.Fatalf("forwarded logs = %q, want requested main failure", logs.String())
	}
	one := strings.Index(logs.String(), "lifecycle:post:one")
	two := strings.Index(logs.String(), "lifecycle:post:two")
	if two < 0 || one < 0 || two > one {
		t.Errorf("post logs are not LIFO: %q", logs.String())
	}
}

func TestPostActionsUseBoundedCleanupContext(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/action.yml", "name: Slow post\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/main.js", "console.log('main completed')\n")
	writeFixtureFile(t, workspace, ".github/actions/slow/post.js", "setTimeout(() => console.log('slow post completed'), 30000)\n")
	runner := Runner{Node24: node, CleanupTimeout: 200 * time.Millisecond}
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "slow", Kind: "uses", Uses: "./.github/actions/slow"}})
	started := time.Now()
	_, err := runner.RunJob(context.Background(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("bounded cleanup took %s, want under 3s", elapsed)
	}
}

func TestFileCommandParsing(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     map[string]string
		wantErr  string
	}{
		{name: "LF", contents: "single=value\nmulti<<END\nfirst\nsecond\nEND\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "CRLF", contents: "single=value\r\nmulti<<END\r\nfirst\r\nsecond\r\nEND\r\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "equals before heredoc", contents: "single=value<<literal\n", want: map[string]string{"single": "value<<literal"}},
		{name: "heredoc before equals", contents: "multi<<END=value\npayload\nEND=value\n", want: map[string]string{"multi": "payload"}},
		{name: "missing name", contents: "=value\n", wantErr: "invalid file command"},
		{name: "missing delimiter", contents: "multi<<\n", wantErr: "invalid multiline file command"},
		{name: "unterminated LF", contents: "multi<<END\nunterminated\n", wantErr: `missing delimiter "END"`},
		{name: "unterminated CRLF", contents: "multi<<END\r\nunterminated\r\n", wantErr: `missing delimiter "END"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "commands")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := parseCommandFile(path)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseCommandFile() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCommandFile() error = %v", err)
			}
			if !maps.Equal(got, test.want) {
				t.Fatalf("parseCommandFile() = %#v, want %#v", got, test.want)
			}
		})
	}

	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(files.dir)
	if err := os.WriteFile(files.env, []byte("NODE_OPTIONS=--require bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := newResult()
	if err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "NODE_OPTIONS") {
		t.Fatalf("commandFiles.apply() error = %v, want NODE_OPTIONS rejection", err)
	}
}

func TestFileCommandLineLimitIsExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(path, []byte("value="+strings.Repeat("x", 70*1024)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if values, err := parseCommandFile(path); err != nil || len(values["value"]) != 70*1024 {
		t.Fatalf("parseCommandFile() value length = %d, error = %v", len(values["value"]), err)
	}
	if err := os.WriteFile(path, []byte("value="+strings.Repeat("x", maxStreamLineBytes)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCommandFile(path); err == nil || !strings.Contains(err.Error(), "parse file command output") {
		t.Fatalf("parseCommandFile() error = %v, want attributed size failure", err)
	}
}

func TestExpressionEvaluationIsSinglePass(t *testing.T) {
	literal := "literal ${{ matrix.secret }} and ${{"
	values, err := evaluateMap(map[string]string{
		"value": "before ${{ matrix.value }} after",
	}, expression.Context{Matrix: map[string]any{
		"value":  literal,
		"secret": "reevaluated",
	}})
	if err != nil || values["value"] != "before "+literal+" after" {
		t.Fatalf("evaluateMap() = %#v, %v, want single-pass substitution", values, err)
	}
}

func TestRunStreamingDrainsOversizedLineAndPreservesMasking(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runStreaming(ctx, processor, "", map[string]string{"GO_WANT_RUNTIME_LONG_LINE": "1"}, executable, "-test.run=^TestLongLineChildProcess$")
	if err == nil || !strings.Contains(err.Error(), "stdout stream: line exceeds 1048576-byte limit and was discarded") {
		t.Fatalf("runStreaming() error = %v, want oversized-line diagnostic", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStreaming() deadlocked: %v", err)
	}
	if strings.Contains(logs.String(), "runtime-stream-secret") {
		t.Fatalf("runStreaming() leaked masked content: %q", logs.String())
	}
	if strings.Contains(logs.String(), "after long line") {
		t.Fatalf("runStreaming() forwarded output after masking became uncertain: %q", logs.String())
	}
}

func TestStreamLineLimitIncludesContentNotNewline(t *testing.T) {
	want := strings.Repeat("x", maxStreamLineBytes)
	var lines []string
	suppressed := false
	err := streamLines(strings.NewReader(want+"\nnext\n"), func(line string) {
		lines = append(lines, line)
	}, func() {
		suppressed = true
	})
	if err != nil || suppressed {
		t.Fatalf("streamLines() = %v, suppressed = %v", err, suppressed)
	}
	if len(lines) != 2 || lines[0] != want || lines[1] != "next" {
		t.Fatalf("streamLines() returned %d lines with unexpected content", len(lines))
	}
}

func TestLongLineChildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIME_LONG_LINE") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "::add-mask::runtime-stream-secret")
	fmt.Fprintln(os.Stdout, strings.Repeat("x", maxStreamLineBytes+1)+"runtime-stream-secret")
	fmt.Fprintln(os.Stdout, "after long line: runtime-stream-secret")
}

func TestProcessEnvironmentIsExplicitAndUsable(t *testing.T) {
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "must-not-leak")
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	command := `test -n "$PATH" && test -n "$HOME" && test -n "$TMPDIR" && test "$DECLARED" = visible && test -z "${BUILDKITE_AGENT_ACCESS_TOKEN:-}" && printf '%s\n' environment-ok`
	if err := runStreaming(context.Background(), processor, "", map[string]string{"DECLARED": "visible"}, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}
	if logs.String() != "environment-ok\n" {
		t.Fatalf("runStreaming() logs = %q", logs.String())
	}
	for _, entry := range processEnv(nil) {
		if strings.HasPrefix(entry, "BUILDKITE_") {
			t.Fatalf("processEnv() inherited agent variable %q", entry)
		}
	}
}

func TestWorkflowCommandParsingIsCaseInsensitiveAndExact(t *testing.T) {
	if got, ok := workflowCommand("::ADD-MASK::secret%250Avalue", "add-mask"); !ok || got != "secret%0Avalue" {
		t.Fatalf("workflowCommand() = %q, %v", got, ok)
	}
	if _, ok := workflowCommand("::add-mask-extra::secret", "add-mask"); ok {
		t.Fatal("workflowCommand() accepted a different command name")
	}
}

func TestActionMetadataRejectsCaseInsensitiveOutputCollisions(t *testing.T) {
	actionPath := t.TempDir()
	writeFixtureFile(t, actionPath, "action.yml", `name: Conflicting outputs
outputs:
  Result:
    value: first
  result:
    value: second
runs:
  using: composite
  steps: []
`)
	if _, err := readActionMetadata(actionPath); err == nil || !strings.Contains(err.Error(), `duplicate case-insensitive name "result"`) {
		t.Fatalf("readActionMetadata() error = %v, want duplicate output rejection", err)
	}
}

func TestDiscoverNode24ManagedAndWrongExplicitVersion(t *testing.T) {
	managed := t.TempDir()
	node := filepath.Join(managed, "node24", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverNode24("", managed)
	if err != nil || got != node {
		t.Fatalf("DiscoverNode24() = %q, %v, want %q, nil", got, err, node)
	}

	wrong := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(wrong, []byte("#!/bin/sh\necho v23.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverNode24(wrong, ""); err == nil || !strings.Contains(err.Error(), `reported "v23.1.0"`) {
		t.Fatalf("DiscoverNode24() error = %v, want wrong-version detail", err)
	}
}

func TestDockerAction(t *testing.T) {
	docker := requireDocker(t)
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs, Docker: docker}
	result, err := runner.RunDocker(context.Background(), DockerAction{
		Name: "local Docker", Path: fixturePath(t, "actions", "docker"), Workspace: fixturePath(t),
	})
	if err != nil {
		t.Fatalf("RunDocker() error = %v", err)
	}
	if result.Outputs["container"] != "ran" || result.Env["DOCKER_RUNTIME_SEEN"] != "true" {
		t.Errorf("Docker result = %#v", result)
	}
	if result.Summary != "docker action summary\n" {
		t.Errorf("Docker summary = %q", result.Summary)
	}
	if strings.Contains(logs.String(), "docker-secret-value") {
		t.Fatalf("raw forwarded Docker logs contain literal secret: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "masked docker probe: ***") {
		t.Errorf("Docker logs = %q, want masked probe", logs.String())
	}
}

func TestRunJobShellJavaScriptCompositeAndPost(t *testing.T) {
	node := requireNode24(t)
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{
		{ID: "shell", Kind: "run", Shell: "bash", Command: `echo "result=smoke" >> "$GITHUB_OUTPUT"`},
		{ID: "javascript", Name: "JavaScript", Kind: "uses", Uses: "./.github/actions/javascript", With: map[string]string{"message": "${{ steps.shell.outputs.result }}"}},
		{ID: "composite", Name: "Composite", Kind: "uses", Uses: "./.github/actions/composite", With: map[string]string{"message": "${{ steps.javascript.outputs.result }}"}},
		{ID: "verify", Kind: "run", Shell: "bash", Command: `test "$SMOKE_COMPOSITE_SEEN" = true`},
	})
	job.Outputs = map[string]string{"result": "${{ steps.composite.outputs.result }}"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Node24: node}).RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v\nlogs:\n%s", err, logs.String())
	}
	if result.Outputs["result"] != "smoke-javascript-composite" || result.Env["SMOKE_COMPOSITE_SEEN"] != "true" || result.State["phase"] != "main" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if strings.Contains(logs.String(), "smoke-mask-value") || !strings.Contains(logs.String(), "masked probe: ***") {
		t.Fatalf("RunJob() logs were not masked: %q", logs.String())
	}
	if post, verify := strings.Index(logs.String(), "JavaScript post phase completed"), strings.Index(logs.String(), "masked probe: ***"); post < verify {
		t.Fatalf("post action did not run after main steps: %q", logs.String())
	}
	if !strings.Contains(result.Summary, "main phase") || !strings.Contains(result.Summary, "post phase") {
		t.Fatalf("RunJob() summary = %q", result.Summary)
	}
}

func TestCompositeExposesOnlyDeclaredOutputs(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	actionPath := ".github/actions/composite/action.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, actionPath, `name: Output-scoped composite
outputs:
  public:
    value: ${{ steps.inner.outputs.public }}
runs:
  using: composite
  steps:
    - id: inner
      shell: sh
      run: |
        printf '%s\n' 'public=visible' >> "$GITHUB_OUTPUT"
        printf '%s\n' 'private=hidden' >> "$GITHUB_OUTPUT"
        printf '%s\n' 'COMPOSITE_ENV=propagated' >> "$GITHUB_ENV"
        printf '%s\n' 'composite summary' >> "$GITHUB_STEP_SUMMARY"
`)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	job.Outputs = map[string]string{
		"private": "${{ steps.composite.outputs.private }}",
		"public":  "${{ steps.composite.outputs.public }}",
	}
	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Outputs["public"] != "visible" || result.Outputs["private"] != "" {
		t.Fatalf("RunJob() outputs = %#v, want only declared composite output", result.Outputs)
	}
	if result.Env["COMPOSITE_ENV"] != "propagated" || result.Summary != "composite summary\n" {
		t.Fatalf("RunJob() effects = %#v, summary = %q", result.Env, result.Summary)
	}
}

func TestRuntimeMapDiagnosticsAreSorted(t *testing.T) {
	_, err := evaluateMap(map[string]string{
		"z-last":  "${{ unsupported.z }}",
		"a-first": "${{ unsupported.a }}",
	}, expression.Context{})
	if err == nil || !strings.Contains(err.Error(), `evaluate "a-first"`) {
		t.Fatalf("evaluateMap() error = %v, want alphabetically first key", err)
	}

	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "shell", Kind: "run", Shell: "sh", Command: "true"}})
	job.Needs = map[string]plan.Need{"z-last": {}, "a-first": {}}
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), `prerequisite result "a-first"`) {
		t.Fatalf("RunJob() prerequisite error = %v, want alphabetically first key", err)
	}

	job.Needs = nil
	job.Outputs = map[string]string{"z-valid": "partial", "a-invalid": "${{ unsupported.a }}"}
	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `job output "a-invalid"`) {
		t.Fatalf("RunJob() output error = %v, want alphabetically first key", err)
	}
	if len(result.Outputs) != 0 {
		t.Fatalf("RunJob() partial outputs = %#v, want none before first sorted error", result.Outputs)
	}
}

func TestRunJobDockerUsesSharedMasking(t *testing.T) {
	docker := requireDocker(t)
	workspace := fixturePath(t)
	job := runtimePlan(t, workspace, "smoke/.github/workflows/ci.yml", []plan.Step{{ID: "docker", Kind: "uses", Uses: "./actions/docker"}})
	job.RequiredCapabilities = []string{"docker"}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Docker: docker}).RunJob(context.Background(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Env["DOCKER_RUNTIME_SEEN"] != "true" || strings.Contains(logs.String(), "docker-secret-value") || !strings.Contains(logs.String(), "masked docker probe: ***") {
		t.Fatalf("RunJob() result = %#v, logs = %q", result, logs.String())
	}
}

func TestRunJobRejectsWorkflowMismatchAndUnsupportedAction(t *testing.T) {
	workspace := fixturePath(t, "smoke")
	job := runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "remote", Kind: "uses", Uses: "actions/checkout@v4"}})
	job.Workflow.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "workflow digest mismatch") {
		t.Fatalf("RunJob() error = %v, want workflow digest mismatch", err)
	}
	job = runtimePlan(t, workspace, ".github/workflows/ci.yml", []plan.Step{{ID: "remote", Kind: "uses", Uses: "actions/checkout@v4"}})
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "remote action") {
		t.Fatalf("RunJob() error = %v, want explicit remote action error", err)
	}
}

func runtimePlan(t *testing.T, workspace, workflowPath string, steps []plan.Step) plan.Job {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(workspace, workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(source)
	return plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow: plan.Workflow{Path: workflowPath, Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "fixture"},
		Event:    plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:   plan.Target{StepKey: "gha-fixture", Queue: "ubuntu-latest"}, Steps: steps,
	}
}

func writeFixtureFile(t *testing.T, root, path, contents string) {
	t.Helper()
	path = filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixturePath(t *testing.T, parts ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	pathParts := append([]string{filepath.Dir(file), "..", "..", "testdata"}, parts...)
	path, err := filepath.Abs(filepath.Join(pathParts...))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func requireNode24(t *testing.T) string {
	t.Helper()
	if node := os.Getenv("BUILDKITE_GHA_TEST_NODE24"); node != "" {
		if _, err := DiscoverNode24(node, ""); err != nil {
			t.Fatalf("BUILDKITE_GHA_TEST_NODE24 is not Node 24: %v", err)
		}
		return node
	}
	if mise, err := exec.LookPath("mise"); err == nil {
		output, err := exec.Command(mise, "where", "node@24").CombinedOutput()
		if err == nil {
			node := filepath.Join(strings.TrimSpace(string(output)), "bin", "node")
			if _, err := DiscoverNode24(node, ""); err == nil {
				return node
			}
		}
	}
	t.Skip("Node 24 unavailable: set BUILDKITE_GHA_TEST_NODE24 or install managed Node 24 with `mise install node@24`")
	return ""
}

func requireDocker(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker unavailable: docker executable not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, docker, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("Docker unavailable: daemon probe failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return docker
}
