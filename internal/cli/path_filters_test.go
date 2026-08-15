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
	filteredWorkflow := []byte("name: CI\non:\n  pull_request:\n    paths: [\"src/**\"]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
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
	if err := os.WriteFile(filepath.Join(repository, "ci.yml"), filteredWorkflow, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "ci.yml")
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
	merge := runGit("commit-tree", head+"^{tree}", "-p", base, "-p", head, "-m", "merge")
	t.Chdir(repository)

	event := compiler.Event{
		Event: "pull_request",
		SHA:   head,
		Repository: compiler.Repository{
			Owner: "buildkite", Name: "buildkite-gha",
		},
		Payload: map[string]any{"number": 42, "pull_request": map[string]any{
			"base":             map[string]any{"ref": "main", "sha": base, "repo": map[string]any{"full_name": "buildkite/buildkite-gha"}},
			"head":             map[string]any{"sha": head},
			"mergeable":        true,
			"merge_commit_sha": merge,
		}},
	}
	input := workflowInput{
		Path:          filepath.Join(repository, "ci.yml"),
		CanonicalPath: "ci.yml",
		Source:        filteredWorkflow,
		Triggers:      []workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}},
	}
	paths, workflowErrors, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input})
	if err != nil || !reflect.DeepEqual(paths, []string{"src/main.go"}) {
		t.Fatalf("pullRequestChangedPaths() = %#v, %v", paths, err)
	}
	if len(workflowErrors) != 0 {
		t.Fatalf("workflow errors = %#v", workflowErrors)
	}
	pullRequest := event.Payload["pull_request"].(map[string]any)
	pullRequest["mergeable"] = false
	if _, _, err := pullRequestChangedPaths(event, 42, "main", nil); err == nil || !strings.Contains(err.Error(), "mergeable synthetic merge") {
		t.Fatalf("conflicted pull request error = %v", err)
	}
	pullRequest["mergeable"] = true
	workflow := workflowInput{
		Path:          filepath.Join(repository, "ci.yml"),
		CanonicalPath: "ci.yml",
		Source:        []byte("different\n"),
		Triggers:      []workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}},
	}
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{workflow})
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the event merge commit") {
		t.Fatalf("workflow merge source result = %#v, %v", workflowErrors, err)
	}
	pullRequest["base"].(map[string]any)["sha"] = head
	if _, _, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}); err == nil || !strings.Contains(err.Error(), "does not bind the event base and head") {
		t.Fatalf("forged base SHA error = %v", err)
	}
}

func TestGitTracksWorkflowResolvesRepositoryPathAliases(t *testing.T) {
	repository := t.TempDir()
	command := exec.Command("git", "-C", repository, "init", "-q")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(repository, "ci.yml"), []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("git", "-C", repository, "add", "ci.yml")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	alias := filepath.Join(t.TempDir(), "repository")
	if err := os.Symlink(repository, alias); err != nil {
		t.Skipf("create repository path alias: %v", err)
	}
	if !gitTracksWorkflow(repository, workflowInput{Path: filepath.Join(alias, "ci.yml"), CanonicalPath: "ci.yml"}) {
		t.Fatal("tracked workflow was not recognized through a repository path alias")
	}
}

func TestPullRequestChangedPathsRejectsPathFiltersAddedByMerge(t *testing.T) {
	jobs := "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	filtered := []byte("name: CI\non:\n  pull_request:\n    paths: [\"src/**\"]\n" + jobs)
	unfiltered := []byte("name: CI\non: pull_request\n" + jobs)
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
	workflowPath := filepath.Join(repository, "ci.yml")
	if err := os.WriteFile(workflowPath, unfiltered, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "ci.yml")
	runGit("commit", "-qm", "base")
	base := runGit("rev-parse", "HEAD")
	runGit("commit", "--allow-empty", "-qm", "head")
	head := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(workflowPath, filtered, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "ci.yml")
	mergeTree := runGit("write-tree")
	runGit("reset", "-q", head)
	if err := os.WriteFile(workflowPath, unfiltered, 0o600); err != nil {
		t.Fatal(err)
	}
	merge := runGit("commit-tree", mergeTree, "-p", base, "-p", head, "-m", "merge")
	t.Chdir(repository)
	event := compiler.Event{
		Event: "pull_request", SHA: head,
		Repository: compiler.Repository{Owner: "buildkite", Name: "buildkite-gha"},
		Payload: map[string]any{"number": 42, "pull_request": map[string]any{
			"base":             map[string]any{"ref": "main", "sha": base, "repo": map[string]any{"full_name": "buildkite/buildkite-gha"}},
			"head":             map[string]any{"sha": head},
			"mergeable":        true,
			"merge_commit_sha": merge,
		}},
	}
	_, workflowErrors, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{{
		Path: workflowPath, CanonicalPath: "ci.yml", Source: unfiltered,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}})
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the event merge commit") {
		t.Fatalf("merge-added path filter result = %#v, %v", workflowErrors, err)
	}
	withoutPullRequest := []byte("name: CI\non: push\n" + jobs)
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{{
		Path: workflowPath, CanonicalPath: "ci.yml", Source: withoutPullRequest,
		Triggers: []workflow.Trigger{{Event: "push"}},
	}})
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the event merge commit") {
		t.Fatalf("merge-added pull request trigger result = %#v, %v", workflowErrors, err)
	}
	customPath := filepath.Join(t.TempDir(), "custom.yml")
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{{
		Path: customPath, CanonicalPath: customPath,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}})
	if err != nil || len(workflowErrors) != 0 {
		t.Fatalf("unfiltered custom workflow result = %#v, %v", workflowErrors, err)
	}
	pullRequest := event.Payload["pull_request"].(map[string]any)
	pullRequest["mergeable"] = false
	t.Setenv("BUILDKITE_PULL_REQUEST", "42")
	t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "main")
	customWorkflows := []workflowInput{{
		Path: customPath, CanonicalPath: customPath,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}}
	context := buildkitepipeline.TriggerConditionContext{}
	populateChangedPaths(&context, event, effectiveEventFromWebhook, customWorkflows)
	if customWorkflows[0].PathFiltersError != "" {
		t.Fatalf("unfiltered custom workflow provenance error = %q", customWorkflows[0].PathFiltersError)
	}
	pullRequest["mergeable"] = true
	event.Payload["pull_request"].(map[string]any)["merge_commit_sha"] = ""
	workflows := []workflowInput{{
		Path: workflowPath, CanonicalPath: "ci.yml", Source: unfiltered,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}}
	context = buildkitepipeline.TriggerConditionContext{}
	populateChangedPaths(&context, event, effectiveEventFromWebhook, workflows)
	if !strings.Contains(workflows[0].PathFiltersError, "merge commit SHAs") {
		t.Fatalf("missing merge commit workflow error = %q", workflows[0].PathFiltersError)
	}
}

func TestPathEvaluationErrorsApplyOnlyToFilteredWorkflows(t *testing.T) {
	workflows := []workflowInput{
		{CanonicalPath: "filtered.yml", Triggers: []workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}}},
		{CanonicalPath: "unfiltered.yml", Triggers: []workflow.Trigger{{Event: "pull_request"}}},
	}
	errors := pathEvaluationErrors(workflows, "pull_request", map[string]string{}, "diff unavailable")
	if !reflect.DeepEqual(errors, map[string]string{"filtered.yml": "diff unavailable"}) {
		t.Fatalf("path evaluation errors = %#v", errors)
	}
}

func TestSingleGitCommitRejectsMultipleMergeBases(t *testing.T) {
	first := strings.Repeat("a", 40)
	second := strings.Repeat("b", 40)
	if _, err := singleGitCommit([]byte(first+"\n"+second+"\n"), "pull request merge base"); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("multiple merge bases error = %v", err)
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
