package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestJavaScriptLifecycleAndCompositeAction(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	smokeJavaScript := fixturePath(t, "smoke", ".github", "actions", "javascript")

	javascript, err := runner.RunJavaScript(context.Background(), JavaScriptAction{
		Name:   "smoke JavaScript",
		Path:   smokeJavaScript,
		Main:   "index.js",
		Post:   "post.js",
		Inputs: map[string]string{"message": "smoke"},
	})
	if err != nil {
		t.Fatalf("RunJavaScript() error = %v", err)
	}
	if got := javascript.Outputs["result"]; got != "smoke-javascript" {
		t.Errorf("JavaScript output = %q, want %q", got, "smoke-javascript")
	}
	if !strings.Contains(javascript.Summary, "main phase") || !strings.Contains(javascript.Summary, "post phase") {
		t.Errorf("JavaScript summary = %q, want main and post summaries", javascript.Summary)
	}
	if strings.Contains(logs.String(), "smoke-mask-value") {
		t.Fatalf("forwarded logs contain literal masked value: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "masked probe: ***") {
		t.Errorf("forwarded logs = %q, want masked probe", logs.String())
	}

	compositePath := fixturePath(t, "smoke", ".github", "actions", "composite")
	composite, err := runner.RunComposite(context.Background(), CompositeAction{
		Name:   "smoke composite",
		Path:   compositePath,
		Inputs: map[string]string{"message": javascript.Outputs["result"]},
		Steps: []ShellStep{{
			ID:    "transform",
			Shell: "bash",
			Env:   map[string]string{"MESSAGE": javascript.Outputs["result"]},
			Script: "echo \"result=$MESSAGE-composite\" >> \"$GITHUB_OUTPUT\"\n" +
				"echo \"SMOKE_COMPOSITE_SEEN=true\" >> \"$GITHUB_ENV\"",
		}},
	})
	if err != nil {
		t.Fatalf("RunComposite() error = %v", err)
	}
	if got := composite.Outputs["result"]; got != "smoke-javascript-composite" {
		t.Errorf("composite output = %q, want %q", got, "smoke-javascript-composite")
	}
	if got := composite.Env["SMOKE_COMPOSITE_SEEN"]; got != "true" {
		t.Errorf("composite environment = %q, want true", got)
	}
}

func TestJavaScriptPreMainPostFilesAndMasking(t *testing.T) {
	node := requireNode24(t)
	var logs bytes.Buffer
	actionPath := fixturePath(t, "actions", "javascript")
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}

	result, err := runner.RunJavaScript(context.Background(), JavaScriptAction{
		Name: "runtime lifecycle", Path: actionPath, Pre: "pre.js", Main: "main.js", Post: "post.js",
		Inputs: map[string]string{"message": "hello"},
	})
	if err != nil {
		t.Fatalf("RunJavaScript() error = %v", err)
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
	actionPath := fixturePath(t, "actions", "javascript")
	runner := Runner{Stdout: &logs, Stderr: &logs, Node24: node}
	action := func(order string, fail bool) JavaScriptAction {
		return JavaScriptAction{
			Name: order, Path: actionPath, Pre: "pre.js", Main: "main.js", Post: "post.js",
			Inputs: map[string]string{"message": order, "order": order, "fail": fmt.Sprint(fail)},
		}
	}

	_, err := runner.RunJavaScriptLifecycle(context.Background(), []JavaScriptAction{action("one", false), action("two", true)})
	if err == nil {
		t.Fatal("RunJavaScriptLifecycle() error = nil, want main failure")
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
	actionPath := fixturePath(t, "actions", "javascript")
	runner := Runner{Node24: node, CleanupTimeout: 200 * time.Millisecond}
	started := time.Now()
	_, err := runner.RunJavaScript(context.Background(), JavaScriptAction{
		Name: "slow post", Path: actionPath, Main: "main.js", Post: "slow-post.js",
		Inputs: map[string]string{"message": "slow"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJavaScript() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("bounded cleanup took %s, want under 3s", elapsed)
	}
}

func TestFileCommandsSupportMultilineCRLFAndRejectNodeOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commands")
	if err := os.WriteFile(path, []byte("single=value\r\nmulti<<END\r\nfirst\r\nsecond\r\nEND\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := parseCommandFile(path)
	if err != nil {
		t.Fatalf("parseCommandFile() error = %v", err)
	}
	if values["single"] != "value" || values["multi"] != "first\nsecond" {
		t.Errorf("parseCommandFile() = %#v", values)
	}
	if err := os.WriteFile(path, []byte("multi<<END\nunterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCommandFile(path); err == nil || !strings.Contains(err.Error(), `missing delimiter "END"`) {
		t.Fatalf("parseCommandFile() error = %v, want missing delimiter", err)
	}

	var logs bytes.Buffer
	runner := Runner{Stdout: &logs, Stderr: &logs}
	_, err = runner.RunComposite(context.Background(), CompositeAction{
		Name: "protected environment", Path: t.TempDir(),
		Steps: []ShellStep{{ID: "write", Script: `echo "NODE_OPTIONS=--require bad" >> "$GITHUB_ENV"`}},
	})
	if err == nil || !strings.Contains(err.Error(), "NODE_OPTIONS") {
		t.Fatalf("RunComposite() error = %v, want NODE_OPTIONS rejection", err)
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
		Name: "local Docker", Path: fixturePath(t, "actions", "docker"),
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
