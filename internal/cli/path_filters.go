package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	maxLocallyEvaluatedPathFilterFiles = 300
	maxGitChangedPathBytes             = 2 << 20
	maxGitHubPushCommits               = 1000
	zeroGitCommit                      = "0000000000000000000000000000000000000000"
)

func populateChangedPaths(context *buildkitepipeline.TriggerConditionContext, event compiler.Event, origin effectiveEventOrigin, workflows []workflowInput) {
	if event.Event != "pull_request" && event.Event != "push" {
		return
	}
	if event.Event == "push" && context.TagValue != nil {
		return
	}
	if origin != effectiveEventFromWebhook {
		if workflowsUsePathFilters(workflows, event.Event) {
			context.ChangedPathsError = event.Event + " path filters require linked Buildkite webhook data"
		}
		return
	}
	if event.Event == "push" {
		if !workflowsUsePathFilters(workflows, event.Event) {
			return
		}
		paths, workflowErrors, err := pushChangedPaths(event, workflows)
		if err != nil {
			setPathFiltersError(context, workflows, event.Event, err.Error(), true)
			return
		}
		context.ChangedPaths = paths
		context.ChangedPathsKnown = true
		for i := range workflows {
			workflows[i].PathFiltersError = workflowErrors[workflows[i].CanonicalPath]
		}
		return
	}
	closed := nestedString(event.Payload, "action") == "closed"
	if closed && !workflowsUsePathFilters(workflows, event.Event) {
		return
	}
	pullRequestNumber, err := strconv.Atoi(os.Getenv("BUILDKITE_PULL_REQUEST"))
	if err != nil || pullRequestNumber <= 0 {
		setPathFiltersError(context, workflows, event.Event, "pull request path filters require the Buildkite pull request number", closed)
		return
	}
	baseRef := os.Getenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH")
	if baseRef == "" {
		setPathFiltersError(context, workflows, event.Event, "pull request path filters require the Buildkite pull request base branch", closed)
		return
	}
	paths, workflowErrors, err := pullRequestChangedPaths(event, pullRequestNumber, baseRef, workflows)
	if err != nil {
		setPathFiltersError(context, workflows, event.Event, err.Error(), closed)
		return
	}
	context.ChangedPaths = paths
	context.ChangedPathsKnown = true
	for i := range workflows {
		if closed && !workflowUsesPathFilters(workflows[i], event.Event) {
			continue
		}
		workflows[i].PathFiltersError = workflowErrors[workflows[i].CanonicalPath]
	}
}

func pushChangedPaths(event compiler.Event, workflows []workflowInput) ([]string, map[string]string, error) {
	if event.Provider != "github" {
		return nil, nil, fmt.Errorf("push path filters require a GitHub repository webhook")
	}
	if nestedString(event.Payload, "repository", "full_name") != event.Repository.Owner+"/"+event.Repository.Name {
		return nil, nil, fmt.Errorf("webhook push repository does not match the Buildkite build")
	}
	if nestedString(event.Payload, "ref") != event.Ref || !strings.HasPrefix(event.Ref, "refs/heads/") {
		return nil, nil, fmt.Errorf("webhook push branch ref does not match the Buildkite build")
	}
	after, before := nestedString(event.Payload, "after"), nestedString(event.Payload, "before")
	created, createdOK := event.Payload["created"].(bool)
	deleted, deletedOK := event.Payload["deleted"].(bool)
	forced, forcedOK := event.Payload["forced"].(bool)
	if !createdOK || !deletedOK || !forcedOK {
		return nil, nil, fmt.Errorf("webhook push requires boolean created, deleted, and forced fields")
	}
	if deleted || after == zeroGitCommit {
		return nil, nil, fmt.Errorf("deleted-ref pushes cannot admit path-filtered workflows")
	}
	if !validBuildkiteCommit(after) || after != event.SHA {
		return nil, nil, fmt.Errorf("webhook push after commit does not match the Buildkite build")
	}
	if created != (before == zeroGitCommit) || !created && !validBuildkiteCommit(before) {
		return nil, nil, fmt.Errorf("webhook push before commit is inconsistent with ref creation")
	}

	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("locate checked-out git repository: %w", err)
	}
	root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
	shallowBytes, err := gitCommand(root, "rev-parse", "--is-shallow-repository").Output()
	if err != nil || strings.TrimSpace(string(shallowBytes)) != "false" {
		return nil, nil, fmt.Errorf("push path filters require a complete non-shallow checkout")
	}
	remoteURLBytes, err := gitCommand(root, "remote", "get-url", "origin").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("push path filters require the local origin repository")
	}
	provider, owner, name, _, err := parseBuildkiteRepository(strings.TrimSpace(string(remoteURLBytes)))
	if err != nil || provider != event.Provider || owner != event.Repository.Owner || name != event.Repository.Name {
		return nil, nil, fmt.Errorf("local origin repository does not match the Buildkite build")
	}
	branch := strings.TrimPrefix(event.Ref, "refs/heads/")
	if err := gitCommand(root, "check-ref-format", "refs/heads/"+branch).Run(); err != nil {
		return nil, nil, fmt.Errorf("webhook push branch is invalid")
	}
	for label, revision := range map[string]string{"checkout": "HEAD", "origin branch": "refs/remotes/origin/" + branch} {
		value, err := gitCommand(root, "rev-parse", "--verify", revision+"^{commit}").Output()
		if err != nil || strings.TrimSpace(string(value)) != after {
			return nil, nil, fmt.Errorf("push after commit does not match the local %s", label)
		}
	}
	if err := gitCommand(root, "cat-file", "-e", after+"^{commit}").Run(); err != nil {
		return nil, nil, fmt.Errorf("push after commit is unavailable in the local checkout")
	}

	webhookCommits, err := pushWebhookCommits(event.Payload)
	if err != nil {
		return nil, nil, err
	}
	diffBase := before
	if created {
		if forced {
			return nil, nil, fmt.Errorf("new-branch push cannot also be forced")
		}
		diffBase, err = newBranchPushDiffBase(root, after, webhookCommits)
		if err != nil {
			return nil, nil, err
		}
	} else {
		if err := gitCommand(root, "cat-file", "-e", before+"^{commit}").Run(); err != nil {
			return nil, nil, fmt.Errorf("push before commit is unavailable in the local checkout")
		}
		isAncestor, err := gitIsAncestor(root, before, after)
		if err != nil {
			return nil, nil, fmt.Errorf("verify push force state: %w", err)
		}
		if forced == isAncestor {
			return nil, nil, fmt.Errorf("webhook push forced state does not match local commit history")
		}
		commitsBytes, err := gitCommand(root, "rev-list", "--max-count=1001", before+".."+after).Output()
		if err != nil {
			return nil, nil, fmt.Errorf("list pushed commits: %w", err)
		}
		localCommits := strings.Fields(string(commitsBytes))
		if len(localCommits) > maxGitHubPushCommits {
			return nil, nil, fmt.Errorf("push exceeds GitHub's %d-commit path-filter diff bound", maxGitHubPushCommits)
		}
		if !sameCommitSet(localCommits, webhookCommits) {
			return nil, nil, fmt.Errorf("webhook pushed commits do not match local commit history")
		}
	}

	workflowErrors := make(map[string]string)
	for _, input := range workflows {
		if !workflowUsesPathFilters(input, event.Event) {
			continue
		}
		if !gitTracksWorkflow(root, input) {
			workflowErrors[input.CanonicalPath] = fmt.Sprintf("workflow %q is not provider-backed and cannot use push path filters", input.CanonicalPath)
			continue
		}
		committedSource, err := gitCommand(root, "cat-file", "blob", after+":"+input.CanonicalPath).Output()
		if err != nil || !bytes.Equal(committedSource, input.Source) {
			workflowErrors[input.CanonicalPath] = fmt.Sprintf("workflow %q does not match the pushed commit", input.CanonicalPath)
		}
	}

	output, err := boundedCommandOutput(gitCommand(root, "diff", "--name-status", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", diffBase, after), maxGitChangedPathBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("list push changed paths: %w", err)
	}
	paths, err := parseChangedPaths(output)
	if err != nil {
		return nil, nil, err
	}
	return paths, workflowErrors, nil
}

func pushWebhookCommits(payload map[string]any) ([]string, error) {
	values, ok := payload["commits"].([]any)
	if !ok {
		return nil, fmt.Errorf("webhook push requires its commits array")
	}
	if len(values) > maxGitHubPushCommits {
		return nil, fmt.Errorf("push exceeds GitHub's %d-commit path-filter diff bound", maxGitHubPushCommits)
	}
	commits := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		commit, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("webhook push contains an invalid commit")
		}
		id, _ := commit["id"].(string)
		if !validBuildkiteCommit(id) || seen[id] {
			return nil, fmt.Errorf("webhook push contains an invalid or duplicate commit ID")
		}
		seen[id] = true
		commits = append(commits, id)
	}
	return commits, nil
}

func newBranchPushDiffBase(root, after string, commits []string) (string, error) {
	if len(commits) == 0 || !slices.Contains(commits, after) {
		return "", fmt.Errorf("new-branch push requires complete pushed commit evidence")
	}
	pushed := make(map[string]bool, len(commits))
	for _, commit := range commits {
		pushed[commit] = true
		if err := gitCommand(root, "cat-file", "-e", commit+"^{commit}").Run(); err != nil {
			return "", fmt.Errorf("new-branch pushed commit is unavailable in the local checkout")
		}
		if err := gitCommand(root, "merge-base", "--is-ancestor", commit, after).Run(); err != nil {
			return "", fmt.Errorf("new-branch pushed commit is not an ancestor of the after commit")
		}
	}
	var rootCommit, parent string
	for _, commit := range commits {
		output, err := gitCommand(root, "rev-list", "--parents", "-n", "1", commit).Output()
		fields := strings.Fields(string(output))
		if err != nil || len(fields) == 0 || fields[0] != commit {
			return "", fmt.Errorf("inspect new-branch pushed commit parents")
		}
		hasPushedParent := false
		for _, candidate := range fields[1:] {
			hasPushedParent = hasPushedParent || pushed[candidate]
		}
		if hasPushedParent {
			continue
		}
		if rootCommit != "" || len(fields) != 2 {
			return "", fmt.Errorf("new-branch push does not have one unambiguous diff ancestor")
		}
		rootCommit, parent = commit, fields[1]
	}
	if rootCommit == "" {
		return "", fmt.Errorf("new-branch push does not have one unambiguous diff ancestor")
	}
	commitsBytes, err := gitCommand(root, "rev-list", "--max-count=1001", parent+".."+after).Output()
	if err != nil {
		return "", fmt.Errorf("list new-branch pushed commits: %w", err)
	}
	localCommits := strings.Fields(string(commitsBytes))
	if len(localCommits) > maxGitHubPushCommits {
		return "", fmt.Errorf("push exceeds GitHub's %d-commit path-filter diff bound", maxGitHubPushCommits)
	}
	if !sameCommitSet(localCommits, commits) {
		return "", fmt.Errorf("webhook pushed commits do not match local new-branch history")
	}
	return parent, nil
}

func sameCommitSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	commits := make(map[string]bool, len(left))
	for _, commit := range left {
		commits[commit] = true
	}
	for _, commit := range right {
		if !commits[commit] {
			return false
		}
	}
	return true
}

func gitIsAncestor(root, ancestor, descendant string) (bool, error) {
	err := gitCommand(root, "merge-base", "--is-ancestor", ancestor, descendant).Run()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func setPathFiltersError(context *buildkitepipeline.TriggerConditionContext, workflows []workflowInput, event, reason string, filteredOnly bool) {
	context.ChangedPathsError = reason
	rootBytes, rootErr := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
	for i := range workflows {
		if workflowUsesPathFilters(workflows[i], event) || !filteredOnly && rootErr == nil && gitTracksWorkflow(root, workflows[i]) {
			workflows[i].PathFiltersError = reason
		}
	}
}

func workflowsUsePathFilters(workflows []workflowInput, event string) bool {
	for _, input := range workflows {
		if workflowUsesPathFilters(input, event) {
			return true
		}
	}
	return false
}

func pullRequestChangedPaths(event compiler.Event, pullRequestNumber int, baseRef string, workflows []workflowInput) ([]string, map[string]string, error) {
	pullRequest, ok := event.Payload["pull_request"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("event snapshot requires payload.pull_request")
	}
	baseSHA := nestedString(pullRequest, "base", "sha")
	headSHA := nestedString(pullRequest, "head", "sha")
	mergeSHA := nestedString(pullRequest, "merge_commit_sha")
	if nestedString(pullRequest, "base", "ref") != baseRef {
		return nil, nil, fmt.Errorf("webhook pull request base branch does not match the Buildkite build")
	}
	if nestedString(pullRequest, "base", "repo", "full_name") != event.Repository.Owner+"/"+event.Repository.Name {
		return nil, nil, fmt.Errorf("webhook pull request base repository does not match the Buildkite build")
	}
	if nestedInt(event.Payload, "number") != pullRequestNumber {
		return nil, nil, fmt.Errorf("webhook pull request number does not match the Buildkite build")
	}
	mergeable, mergeabilityKnown := pullRequest["mergeable"].(bool)
	if !mergeabilityKnown || !mergeable {
		return nil, nil, fmt.Errorf("webhook pull request must report a mergeable synthetic merge")
	}
	if !validBuildkiteCommit(baseSHA) || !validBuildkiteCommit(headSHA) || !validBuildkiteCommit(mergeSHA) {
		return nil, nil, fmt.Errorf("event snapshot requires full lowercase payload.pull_request base, head, and merge commit SHAs")
	}
	if headSHA != event.SHA {
		return nil, nil, fmt.Errorf("pull request head SHA does not match the checked-out event SHA")
	}
	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("locate checked-out git repository: %w", err)
	}
	root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
	if err := gitCommand(root, "check-ref-format", "refs/heads/"+baseRef).Run(); err != nil {
		return nil, nil, fmt.Errorf("buildkite pull request base branch is invalid")
	}
	for _, commit := range []struct{ label, sha string }{{"base", baseSHA}, {"head", headSHA}, {"merge", mergeSHA}} {
		if err := gitCommand(root, "cat-file", "-e", commit.sha+"^{commit}").Run(); err != nil {
			return nil, nil, fmt.Errorf("pull request %s commit is unavailable in the local checkout", commit.label)
		}
	}
	mergeParentsBytes, err := gitCommand(root, "rev-list", "--parents", "-n", "1", mergeSHA).Output()
	mergeParents := strings.Fields(string(mergeParentsBytes))
	if err != nil || len(mergeParents) != 3 || mergeParents[0] != mergeSHA || mergeParents[1] != baseSHA || mergeParents[2] != headSHA {
		return nil, nil, fmt.Errorf("webhook pull request merge commit does not bind the event base and head")
	}
	workflowErrors := make(map[string]string)
	for _, input := range workflows {
		usesEvent := workflowUsesEvent(input, event.Event)
		if !gitTracksWorkflow(root, input) {
			if usesEvent && workflowUsesPathFilters(input, event.Event) {
				workflowErrors[input.CanonicalPath] = fmt.Sprintf("workflow %q is not provider-backed and cannot use pull request path filters", input.CanonicalPath)
			}
			continue
		}
		mergeSource, err := gitCommand(root, "cat-file", "blob", mergeSHA+":"+input.CanonicalPath).Output()
		if err != nil {
			workflowErrors[input.CanonicalPath] = fmt.Sprintf("workflow %q is unavailable in the event merge commit", input.CanonicalPath)
			continue
		}
		mergeWorkflow, err := workflow.Parse(input.Path, mergeSource)
		if err != nil {
			workflowErrors[input.CanonicalPath] = fmt.Sprintf("workflow %q cannot be parsed from the event merge commit", input.CanonicalPath)
			continue
		}
		mergeUsesEvent := workflowUsesEvent(workflowInput{Triggers: mergeWorkflow.Triggers}, event.Event)
		if !usesEvent && !mergeUsesEvent {
			continue
		}
		mergeUsesPathFilters := workflowUsesPathFilters(workflowInput{Triggers: mergeWorkflow.Triggers}, event.Event)
		if (usesEvent != mergeUsesEvent || workflowUsesPathFilters(input, event.Event) || mergeUsesPathFilters) && !bytes.Equal(mergeSource, input.Source) {
			workflowErrors[input.CanonicalPath] = fmt.Sprintf("workflow %q does not match the event merge commit", input.CanonicalPath)
		}
	}
	if !workflowsUsePathFilters(workflows, event.Event) {
		return nil, workflowErrors, nil
	}
	shallowBytes, err := gitCommand(root, "rev-parse", "--is-shallow-repository").Output()
	if err != nil || strings.TrimSpace(string(shallowBytes)) != "false" {
		return nil, pathEvaluationErrors(workflows, event.Event, workflowErrors, "pull request path filters require a complete non-shallow checkout"), nil
	}
	baseTipBytes, err := gitCommand(root, "rev-parse", "--verify", "refs/remotes/origin/"+baseRef+"^{commit}").Output()
	if err != nil || strings.TrimSpace(string(baseTipBytes)) != baseSHA {
		return nil, pathEvaluationErrors(workflows, event.Event, workflowErrors, "webhook pull request base commit does not match the local origin base branch"), nil
	}
	mergeBaseBytes, err := gitCommand(root, "merge-base", "--all", baseSHA, headSHA).Output()
	if err != nil {
		return nil, pathEvaluationErrors(workflows, event.Event, workflowErrors, fmt.Sprintf("resolve pull request merge base: %v", err)), nil
	}
	mergeBase, err := singleGitCommit(mergeBaseBytes, "pull request merge base")
	if err != nil {
		return nil, pathEvaluationErrors(workflows, event.Event, workflowErrors, err.Error()), nil
	}
	output, err := boundedCommandOutput(gitCommand(root, "diff", "--name-status", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", mergeBase, headSHA), maxGitChangedPathBytes)
	if err != nil {
		return nil, pathEvaluationErrors(workflows, event.Event, workflowErrors, fmt.Sprintf("list pull request changed paths: %v", err)), nil
	}
	paths, err := parseChangedPaths(output)
	if err != nil {
		return nil, pathEvaluationErrors(workflows, event.Event, workflowErrors, err.Error()), nil
	}
	return paths, workflowErrors, nil
}

func gitTracksWorkflow(root string, input workflowInput) bool {
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	physicalPath, err := filepath.EvalSymlinks(input.Path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(physicalRoot, physicalPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	canonical := filepath.ToSlash(filepath.Clean(relative))
	if canonical != input.CanonicalPath {
		return false
	}
	output, err := gitCommand(root, "ls-files", "-z", "--", ":(top,literal)"+canonical).Output()
	return err == nil && bytes.Equal(output, append([]byte(canonical), 0))
}

func pathEvaluationErrors(workflows []workflowInput, event string, errors map[string]string, reason string) map[string]string {
	for _, input := range workflows {
		if workflowUsesPathFilters(input, event) {
			errors[input.CanonicalPath] = reason
		}
	}
	return errors
}

func workflowUsesPathFilters(input workflowInput, event string) bool {
	for _, trigger := range input.Triggers {
		if trigger.Event == event && (trigger.Paths != nil || trigger.PathsIgnore != nil) {
			return true
		}
	}
	return false
}

func workflowUsesEvent(input workflowInput, event string) bool {
	for _, trigger := range input.Triggers {
		if trigger.Event == event {
			return true
		}
	}
	return false
}

func singleGitCommit(output []byte, label string) (string, error) {
	commits := strings.Fields(string(output))
	if len(commits) != 1 {
		return "", fmt.Errorf("git returned %d candidates for %s; exactly one is required", len(commits), label)
	}
	if !validBuildkiteCommit(commits[0]) {
		return "", fmt.Errorf("git returned an invalid %s", label)
	}
	return commits[0], nil
}

func gitCommand(root string, args ...string) *exec.Cmd {
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	return command
}

func boundedCommandOutput(command *exec.Cmd, limit int) ([]byte, error) {
	output := &boundedOutput{remaining: limit}
	command.Stdout = output
	err := command.Run()
	if output.exceeded {
		return nil, fmt.Errorf("command output exceeds %d bytes", limit)
	}
	return output.Bytes(), err
}

type boundedOutput struct {
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (b *boundedOutput) Write(value []byte) (int, error) {
	if len(value) > b.remaining {
		_, _ = b.buffer.Write(value[:b.remaining])
		b.remaining = 0
		b.exceeded = true
		return len(value), nil
	}
	b.remaining -= len(value)
	return b.buffer.Write(value)
}

func (b *boundedOutput) Bytes() []byte {
	return b.buffer.Bytes()
}

func nestedString(value map[string]any, keys ...string) string {
	current := value
	for i, key := range keys {
		item, ok := current[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			result, _ := item.(string)
			return result
		}
		current, ok = item.(map[string]any)
		if !ok {
			return ""
		}
	}
	return ""
}

func nestedInt(value map[string]any, key string) int {
	switch number := value[key].(type) {
	case json.Number:
		result, _ := strconv.Atoi(number.String())
		return result
	case float64:
		return int(number)
	case int:
		return number
	default:
		return 0
	}
}

func parseChangedPaths(output []byte) ([]string, error) {
	if len(output) == 0 {
		return nil, nil
	}
	fields := bytes.Split(output, []byte{0})
	if len(fields) == 0 || len(fields[len(fields)-1]) != 0 {
		return nil, fmt.Errorf("git changed-path output is not NUL terminated")
	}
	fields = fields[:len(fields)-1]
	paths := make([]string, 0, len(fields)/2)
	added, deleted := false, false
	for len(fields) != 0 {
		if len(fields) < 2 {
			return nil, fmt.Errorf("git changed-path output is incomplete")
		}
		status, path := string(fields[0]), string(fields[1])
		fields = fields[2:]
		if status == "" || path == "" || strings.Contains(path, "\\") {
			return nil, fmt.Errorf("git returned an invalid changed path")
		}
		switch status[0] {
		case 'A':
			added = true
			paths = append(paths, path)
		case 'D':
			deleted = true
			paths = append(paths, path)
		case 'M', 'T', 'U', 'X', 'B':
			paths = append(paths, path)
		case 'R', 'C':
			return nil, fmt.Errorf("renamed and copied files require provider conformance data")
		default:
			return nil, fmt.Errorf("git returned unsupported changed-path status %q", status)
		}
		if len(paths) > maxLocallyEvaluatedPathFilterFiles {
			return nil, fmt.Errorf("changed paths exceed the importer's %d-file local evaluation bound", maxLocallyEvaluatedPathFilterFiles)
		}
	}
	if added && deleted {
		return nil, fmt.Errorf("combined added and deleted files require provider rename conformance data")
	}
	return paths, nil
}
