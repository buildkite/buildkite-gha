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

func TestPopulateChangedPathsErrorsApplyOnlyToFilteredWorkflows(t *testing.T) {
	workflows := []workflowInput{{
		Triggers: []workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}},
	}, {
		Triggers: []workflow.Trigger{{Event: "pull_request", PathsIgnore: []string{"docs/**"}}},
	}, {
		Triggers: []workflow.Trigger{{Event: "pull_request"}},
	}}
	snapshot := buildkitepipeline.TriggerEventSnapshot{}
	t.Setenv("BUILDKITE_PULL_REQUEST", "42")
	t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "main")
	populateChangedPaths(&snapshot, compiler.Event{
		Event: "pull_request",
		Payload: map[string]any{
			"number":       42,
			"pull_request": map[string]any{"base": map[string]any{"ref": "main"}},
		},
	}, effectiveEventFromWebhook, workflows, "")
	for _, input := range workflows[:2] {
		if !strings.Contains(input.PathFiltersError, "base repository does not match") {
			t.Fatalf("filtered workflow path error = %q", input.PathFiltersError)
		}
	}
	if workflows[2].PathFiltersError != "" {
		t.Fatalf("unfiltered workflow path error = %q", workflows[2].PathFiltersError)
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
	runGit("commit", "-qm", "common ancestor")
	ancestor := runGit("rev-parse", "HEAD")
	if err := os.Mkdir(filepath.Join(repository, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "src", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "src/main.go")
	runGit("commit", "-qm", "head")
	head := runGit("rev-parse", "HEAD")
	// The base has diverged and has a different workflow. Neither its changes
	// nor the workflow that a synthetic merge might produce define the PR diff.
	runGit("checkout", "-q", "--detach", ancestor)
	if err := os.WriteFile(filepath.Join(repository, "ci.yml"), []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "ci.yml")
	runGit("commit", "-qm", "base")
	base := runGit("rev-parse", "HEAD")
	runGit("commit", "--allow-empty", "-qm", "base advanced after webhook")
	runGit("update-ref", "refs/remotes/origin/main", runGit("rev-parse", "HEAD"))
	runGit("checkout", "-q", "--detach", head)
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
	t.Setenv("BUILDKITE_PULL_REQUEST", "42")
	t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "main")
	for _, test := range []struct {
		name      string
		action    string
		mergeSHA  any
		mergeable any
	}{
		{name: "pending merge"},
		{name: "unavailable stale merge", mergeSHA: strings.Repeat("a", 40)},
		{name: "invalid merge", mergeSHA: "not-a-sha"},
		{name: "conflicting PR", mergeable: false},
		{name: "closed squash merge", action: "closed", mergeSHA: base, mergeable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			event.Payload["action"] = test.action
			pullRequest["merge_commit_sha"], pullRequest["mergeable"] = test.mergeSHA, test.mergeable
			workflows := []workflowInput{input}
			snapshot := buildkitepipeline.TriggerEventSnapshot{}
			populateChangedPaths(&snapshot, event, effectiveEventFromWebhook, workflows, "")
			if workflows[0].PathFiltersError != "" || snapshot.ChangedPaths.UnavailableReason != "" || !reflect.DeepEqual(snapshot.ChangedPaths.Paths, []string{"src/main.go"}) {
				t.Fatalf("paths = %#v, workflow error = %q", snapshot.ChangedPaths, workflows[0].PathFiltersError)
			}
		})
	}
	mismatched := workflowInput{
		Path:          filepath.Join(repository, "ci.yml"),
		CanonicalPath: "ci.yml",
		Source:        []byte("different\n"),
		Triggers:      []workflow.Trigger{{Event: "pull_request", Paths: []string{"src/**"}}},
	}
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{mismatched}, "")
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "does not match the pull request head commit") {
		t.Fatalf("workflow head source result = %#v, %v", workflowErrors, err)
	}
	for _, key := range []string{"base", "head"} {
		commit := pullRequest[key].(map[string]any)
		original := commit["sha"]
		for _, invalid := range []string{"", "main", head[:7], strings.ToUpper(head)} {
			commit["sha"] = invalid
			if _, _, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "full lowercase") {
				t.Fatalf("invalid %s SHA %q: %v", key, invalid, err)
			}
		}
		commit["sha"] = original
	}
	pullRequest["base"].(map[string]any)["sha"] = strings.Repeat("a", 40)
	if _, _, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "base commit is unavailable") {
		t.Fatalf("missing base object: %v", err)
	}
	pullRequest["base"].(map[string]any)["sha"] = base
	event.SHA = base
	if _, _, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "checked-out event SHA") {
		t.Fatalf("mismatched event SHA: %v", err)
	}
	event.SHA = head
	runGit("checkout", "-q", "--detach", base)
	if _, _, err := pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, ""); err == nil || !strings.Contains(err.Error(), "local checkout") {
		t.Fatalf("mismatched checkout: %v", err)
	}
	runGit("checkout", "-q", "--detach", head)
	unrelated := runGit("commit-tree", head+"^{tree}", "-m", "unrelated root")
	pullRequest["base"].(map[string]any)["sha"] = unrelated
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, "")
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "resolve pull request merge base") {
		t.Fatalf("unrelated history = %#v, %v", workflowErrors, err)
	}
	// Criss-cross history has two best common ancestors, not one safe diff base.
	left := runGit("commit-tree", head+"^{tree}", "-p", base, "-p", head, "-m", "left")
	right := runGit("commit-tree", head+"^{tree}", "-p", head, "-p", base, "-m", "right")
	runGit("checkout", "-q", "--detach", left)
	event.SHA = left
	pullRequest["head"].(map[string]any)["sha"] = left
	pullRequest["base"].(map[string]any)["sha"] = right
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, "")
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "2 candidates for pull request merge base") {
		t.Fatalf("ambiguous history = %#v, %v", workflowErrors, err)
	}
	runGit("checkout", "-q", "--detach", head)
	event.SHA = head
	pullRequest["head"].(map[string]any)["sha"] = head
	pullRequest["base"].(map[string]any)["sha"] = base
	if err := os.WriteFile(filepath.Join(repository, ".git", "shallow"), []byte(ancestor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, workflowErrors, err = pullRequestChangedPaths(event, 42, "main", []workflowInput{input}, "")
	if err != nil || !strings.Contains(workflowErrors["ci.yml"], "non-shallow checkout") {
		t.Fatalf("shallow history = %#v, %v", workflowErrors, err)
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

func TestPopulateChangedPathsSkipsUnfilteredPullRequests(t *testing.T) {
	source := "name: CI\non:\n  push:\n    branches: [main]\n  pull_request:\n    branches: [main]\n  workflow_dispatch: ~\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	repository := writeUploadWorkflowRepository(t, map[string]string{"ci.yml": source})
	path := filepath.Join(repository, ".github/workflows/ci.yml")
	parsed, err := workflow.Parse(path, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"opened", "synchronize", "reopened", "closed"} {
		t.Run(action, func(t *testing.T) {
			workflows := []workflowInput{{Path: path, CanonicalPath: ".github/workflows/ci.yml", Source: []byte(source), Triggers: parsed.Triggers}}
			snapshot := buildkitepipeline.TriggerEventSnapshot{}
			// No base/head/merge metadata or history is required without filters.
			populateChangedPaths(&snapshot, compiler.Event{Event: "pull_request", Payload: map[string]any{"action": action}}, effectiveEventFromWebhook, workflows, repository)
			if workflows[0].PathFiltersError != "" || snapshot.ChangedPaths.UnavailableReason != "" || snapshot.ChangedPaths.Paths != nil {
				t.Fatalf("unfiltered PR path evaluation = %#v, %q", snapshot.ChangedPaths, workflows[0].PathFiltersError)
			}
		})
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
