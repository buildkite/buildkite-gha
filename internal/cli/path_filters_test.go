package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestPullRequestChangedPathsUsesPayloadCommits(t *testing.T) {
	repository := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "-q")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-qm", "base")
	base := runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", base)
	if err := os.Mkdir(filepath.Join(repository, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "src", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "src/main.go")
	runGit("commit", "-qm", "head")
	head := runGit("rev-parse", "HEAD")
	t.Chdir(repository)

	event := compiler.Event{
		Event: "pull_request",
		SHA:   head,
		Repository: compiler.Repository{
			Owner: "buildkite", Name: "buildkite-gha",
		},
		Payload: map[string]any{"number": 42, "pull_request": map[string]any{
			"base": map[string]any{"ref": "main", "sha": base, "repo": map[string]any{"full_name": "buildkite/buildkite-gha"}},
			"head": map[string]any{"sha": head},
		}},
	}
	paths, err := pullRequestChangedPaths(event, 42, "main")
	if err != nil || !reflect.DeepEqual(paths, []string{"src/main.go"}) {
		t.Fatalf("pullRequestChangedPaths() = %#v, %v", paths, err)
	}
	event.Payload["pull_request"].(map[string]any)["base"].(map[string]any)["sha"] = head
	if _, err := pullRequestChangedPaths(event, 42, "main"); err == nil || !strings.Contains(err.Error(), "does not match the local origin base branch") {
		t.Fatalf("forged base SHA error = %v", err)
	}
}

func TestParseChangedPathsFailsClosed(t *testing.T) {
	if paths, err := parseChangedPaths([]byte("D\x00docs/old.md\x00M\x00src/main.go\x00")); err != nil || !reflect.DeepEqual(paths, []string{"docs/old.md", "src/main.go"}) {
		t.Fatalf("deleted and modified paths = %#v, %v", paths, err)
	}
	if _, err := parseChangedPaths([]byte("R100\x00old.go\x00new.go\x00")); err == nil || !strings.Contains(err.Error(), "renamed") {
		t.Fatalf("rename error = %v", err)
	}
	if _, err := parseChangedPaths([]byte("D\x00old.go\x00A\x00new.go\x00")); err == nil || !strings.Contains(err.Error(), "rename conformance") {
		t.Fatalf("undetected rename error = %v", err)
	}
	var output bytes.Buffer
	for i := 0; i <= maxLocallyEvaluatedPathFilterFiles; i++ {
		_, _ = fmt.Fprintf(&output, "M\x00file-%03d\x00", i)
	}
	if _, err := parseChangedPaths(output.Bytes()); err == nil || !strings.Contains(err.Error(), "300-file local evaluation bound") {
		t.Fatalf("file limit error = %v", err)
	}
}

func TestBoundedCommandOutput(t *testing.T) {
	command := exec.Command("printf", "123456")
	if output, err := boundedCommandOutput(command, 5); err == nil || output != nil || !strings.Contains(err.Error(), "exceeds 5 bytes") {
		t.Fatalf("bounded output = %q, %v", output, err)
	}
}

func TestPopulateChangedPathsRequiresLinkedWebhook(t *testing.T) {
	context := buildkitepipeline.TriggerConditionContext{}
	populateChangedPaths(&context, compiler.Event{Event: "pull_request"}, effectiveEventFromPath, []workflowInput{{
		Triggers: []workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}},
	}})
	if context.ChangedPathsKnown || !strings.Contains(context.ChangedPathsError, "linked Buildkite webhook") {
		t.Fatalf("changed-path context = %#v", context)
	}
}
