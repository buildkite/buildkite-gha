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

func TestPushChangedPathsBindsWebhookAndLocalDiff(t *testing.T) {
	workflowSource := []byte("name: CI\non:\n  push:\n    paths: [\"src/**\"]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
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
	runGit("config", "commit.gpgsign", "false")
	runGit("remote", "add", "origin", "https://github.com/buildkite/buildkite-gha.git")
	for path, source := range map[string][]byte{
		"ci.yml": workflowSource, "src/main.go": []byte("package main\n"),
		"docs/old.md": []byte("old\n"), "src/tool": []byte("regular\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repository, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, path), source, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", ".")
	runGit("commit", "-qm", "base")
	base := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "src/main.go"), []byte("package main\n// changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repository, "docs/old.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repository, "src/tool")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("main.go", filepath.Join(repository, "src/tool")); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-qm", "normal push")
	after := runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", after)
	t.Chdir(repository)

	input := workflowInput{
		Path: filepath.Join(repository, "ci.yml"), CanonicalPath: "ci.yml", Source: workflowSource,
		Triggers: []workflow.Trigger{{Event: "push", Paths: []string{"src/**"}}},
	}
	newEvent := func(before, after string, created, forced bool, commits ...string) compiler.Event {
		commitValues := make([]any, len(commits))
		for i, commit := range commits {
			commitValues[i] = map[string]any{"id": commit}
		}
		return compiler.Event{
			Provider: "github", Event: "push", SHA: after, Ref: "refs/heads/main",
			Repository: compiler.Repository{Owner: "buildkite", Name: "buildkite-gha"},
			Payload: map[string]any{
				"ref": "refs/heads/main", "before": before, "after": after,
				"created": created, "deleted": false, "forced": forced, "commits": commitValues,
				"repository": map[string]any{"full_name": "buildkite/buildkite-gha"},
			},
		}
	}
	event := newEvent(base, after, false, false, after)
	paths, workflowErrors, err := pushChangedPaths(event, []workflowInput{input}, "")
	want := []string{"docs/old.md", "src/main.go", "src/tool"}
	if err != nil || !reflect.DeepEqual(paths, want) || len(workflowErrors) != 0 {
		t.Fatalf("normal push paths/errors = %#v / %#v / %v, want %#v", paths, workflowErrors, err, want)
	}
	t.Chdir(t.TempDir())
	paths, _, err = pushChangedPaths(event, []workflowInput{input}, repository)
	if err != nil || !reflect.DeepEqual(paths, want) {
		t.Fatalf("push paths outside checkout = %#v, %v", paths, err)
	}
	t.Chdir(repository)

	runGit("checkout", "-q", "-B", "force", base)
	if err := os.WriteFile(filepath.Join(repository, "src/main.go"), []byte("package forced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "src/main.go")
	runGit("commit", "-qm", "force push")
	forceAfter := runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", forceAfter)
	forceEvent := newEvent(after, forceAfter, false, true, forceAfter)
	if paths, _, err := pushChangedPaths(forceEvent, []workflowInput{input}, ""); err != nil || !reflect.DeepEqual(paths, want) {
		t.Fatalf("force push paths = %#v, %v", paths, err)
	}

	runGit("checkout", "-q", "-B", "new", base)
	if err := os.WriteFile(filepath.Join(repository, "src/main.go"), []byte("package first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "src/main.go")
	runGit("commit", "-qm", "first new-branch commit")
	first := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "src/main.go"), []byte("package second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "src/main.go")
	runGit("commit", "-qm", "second new-branch commit")
	newAfter := runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", newAfter)
	newBranchEvent := newEvent(zeroGitCommit, newAfter, true, false, first, newAfter)
	if paths, _, err := pushChangedPaths(newBranchEvent, []workflowInput{input}, ""); err != nil || !reflect.DeepEqual(paths, []string{"src/main.go"}) {
		t.Fatalf("new-branch push paths = %#v, %v", paths, err)
	}

	newBranchEvent.Payload["repository"].(map[string]any)["full_name"] = "attacker/other"
	if _, _, err := pushChangedPaths(newBranchEvent, []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "repository does not match") {
		t.Fatalf("mismatched repository error = %v", err)
	}
	newBranchEvent.Payload["repository"].(map[string]any)["full_name"] = "buildkite/buildkite-gha"
	newBranchEvent.Payload["deleted"] = true
	if _, _, err := pushChangedPaths(newBranchEvent, []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "deleted-ref") {
		t.Fatalf("deleted ref error = %v", err)
	}
	newBranchEvent.Payload["deleted"] = false
	newBranchEvent.Payload["commits"] = []any{map[string]any{"id": first}}
	if _, _, err := pushChangedPaths(newBranchEvent, []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "complete pushed commit evidence") {
		t.Fatalf("incomplete new-branch commits error = %v", err)
	}
	newBranchEvent.Payload["commits"] = []any{map[string]any{"id": first}, map[string]any{"id": newAfter}}
	input.Source = []byte("modified worktree workflow\n")
	if _, workflowErrors, err := pushChangedPaths(newBranchEvent, []workflowInput{input}, ""); err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the pushed commit") {
		t.Fatalf("mismatched workflow result = %#v, %v", workflowErrors, err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "shallow"), []byte(base+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pushChangedPaths(newBranchEvent, []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "non-shallow checkout") {
		t.Fatalf("shallow checkout error = %v", err)
	}
}

func TestPushWebhookCommitsEnforcesGitHubBound(t *testing.T) {
	commits := make([]any, maxGitHubPushCommits+1)
	if _, err := pushWebhookCommits(map[string]any{"commits": commits}); err == nil || !strings.Contains(err.Error(), "1000-commit") {
		t.Fatalf("push commit bound error = %v", err)
	}
}

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
	runGit("config", "commit.gpgsign", "false")
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
	paths, workflowErrors, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, "")
	if err != nil || !reflect.DeepEqual(paths, []string{"src/main.go"}) {
		t.Fatalf("pullRequestChangedPaths() = %#v, %v", paths, err)
	}
	if len(workflowErrors) != 0 {
		t.Fatalf("workflow errors = %#v", workflowErrors)
	}
	t.Chdir(t.TempDir())
	paths, _, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, repository)
	if err != nil || !reflect.DeepEqual(paths, []string{"src/main.go"}) {
		t.Fatalf("pull request paths outside checkout = %#v, %v", paths, err)
	}
	t.Chdir(repository)
	pullRequest := event.Payload["pull_request"].(map[string]any)
	delete(pullRequest, "mergeable")
	if paths, _, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, ""); err != nil || !reflect.DeepEqual(paths, []string{"src/main.go"}) {
		t.Fatalf("unknown mergeability result = %#v, %v", paths, err)
	}
	pullRequest["mergeable"] = false
	if _, _, err := pullRequestChangedPaths(event, 42, "main", nil, ""); err == nil || !strings.Contains(err.Error(), "mergeable synthetic merge") {
		t.Fatalf("conflicted pull request error = %v", err)
	}
	pullRequest["mergeable"] = true
	workflow := workflowInput{
		Path:          filepath.Join(repository, "ci.yml"),
		CanonicalPath: "ci.yml",
		Source:        []byte("different\n"),
		Triggers:      []workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}},
	}
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{workflow}, "")
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the event merge commit") {
		t.Fatalf("workflow merge source result = %#v, %v", workflowErrors, err)
	}
	pullRequest["base"].(map[string]any)["sha"] = head
	if _, _, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "does not bind the event base and head") {
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
	runGit("config", "commit.gpgsign", "false")
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
	}}, "")
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the event merge commit") {
		t.Fatalf("merge-added path filter result = %#v, %v", workflowErrors, err)
	}
	withoutPullRequest := []byte("name: CI\non: push\n" + jobs)
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{{
		Path: workflowPath, CanonicalPath: "ci.yml", Source: withoutPullRequest,
		Triggers: []workflow.Trigger{{Event: "push"}},
	}}, "")
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the event merge commit") {
		t.Fatalf("merge-added pull request trigger result = %#v, %v", workflowErrors, err)
	}
	customPath := filepath.Join(t.TempDir(), "custom.yml")
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{{
		Path: customPath, CanonicalPath: customPath,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}}, "")
	if err != nil || len(workflowErrors) != 0 {
		t.Fatalf("unfiltered custom workflow result = %#v, %v", workflowErrors, err)
	}
	pullRequest := event.Payload["pull_request"].(map[string]any)
	t.Setenv("BUILDKITE_PULL_REQUEST", "42")
	t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "main")
	event.Payload["action"] = "closed"
	pullRequest["merge_commit_sha"] = head
	closedWorkflows := []workflowInput{{
		Path: workflowPath, CanonicalPath: "ci.yml", Source: unfiltered,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}}
	snapshot := buildkitepipeline.TriggerEventSnapshot{}
	populateChangedPaths(&snapshot, event, effectiveEventFromWebhook, closedWorkflows, "")
	if closedWorkflows[0].PathFiltersError != "" {
		t.Fatalf("unfiltered closed workflow provenance error = %q", closedWorkflows[0].PathFiltersError)
	}
	closedWorkflows[0].Triggers[0].Paths = []string{"src/**"}
	closedWorkflows = append(closedWorkflows, workflowInput{
		Path: workflowPath, CanonicalPath: "ci.yml", Source: unfiltered,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	})
	snapshot = buildkitepipeline.TriggerEventSnapshot{}
	populateChangedPaths(&snapshot, event, effectiveEventFromWebhook, closedWorkflows, "")
	if !strings.Contains(closedWorkflows[0].PathFiltersError, "does not bind the event base and head") {
		t.Fatalf("filtered closed workflow provenance error = %q", closedWorkflows[0].PathFiltersError)
	}
	if closedWorkflows[1].PathFiltersError != "" {
		t.Fatalf("mixed unfiltered closed workflow provenance error = %q", closedWorkflows[1].PathFiltersError)
	}
	delete(event.Payload, "action")
	pullRequest["merge_commit_sha"] = merge
	pullRequest["mergeable"] = false
	customWorkflows := []workflowInput{{
		Path: customPath, CanonicalPath: customPath,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}}
	snapshot = buildkitepipeline.TriggerEventSnapshot{}
	populateChangedPaths(&snapshot, event, effectiveEventFromWebhook, customWorkflows, "")
	if customWorkflows[0].PathFiltersError != "" {
		t.Fatalf("unfiltered custom workflow provenance error = %q", customWorkflows[0].PathFiltersError)
	}
	pullRequest["mergeable"] = true
	event.Payload["pull_request"].(map[string]any)["merge_commit_sha"] = ""
	workflows := []workflowInput{{
		Path: workflowPath, CanonicalPath: "ci.yml", Source: unfiltered,
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}}
	snapshot = buildkitepipeline.TriggerEventSnapshot{}
	populateChangedPaths(&snapshot, event, effectiveEventFromWebhook, workflows, "")
	if !strings.Contains(workflows[0].PathFiltersError, "merge commit SHAs") {
		t.Fatalf("missing merge commit workflow error = %q", workflows[0].PathFiltersError)
	}
	pullRequest["merge_commit_sha"] = merge
	pullRequest["base"].(map[string]any)["sha"] = ""
	pullRequest["head"].(map[string]any)["sha"] = ""
	workflows[0].PathFiltersError = ""
	snapshot = buildkitepipeline.TriggerEventSnapshot{}
	populateChangedPaths(&snapshot, event, effectiveEventFromWebhook, workflows, "")
	if !strings.Contains(workflows[0].PathFiltersError, "base, head, and merge commit SHAs") {
		t.Fatalf("missing base and head workflow error = %q", workflows[0].PathFiltersError)
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
	for _, event := range []string{"push", "pull_request"} {
		t.Run(event, func(t *testing.T) {
			snapshot := buildkitepipeline.TriggerEventSnapshot{}
			populateChangedPaths(&snapshot, compiler.Event{Event: event}, effectiveEventFromPath, []workflowInput{{
				Triggers: []workflow.Trigger{{Event: event, Paths: []string{"src/**"}}},
			}}, "")
			if snapshot.ChangedPaths.Paths != nil || !strings.Contains(snapshot.ChangedPaths.UnavailableReason, event+" path filters require linked Buildkite webhook") {
				t.Fatalf("changed-path snapshot = %#v", snapshot)
			}
		})
	}
}
