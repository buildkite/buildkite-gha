package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
)

const (
	maxLocallyEvaluatedPathFilterFiles = 300
	maxGitChangedPathBytes             = 2 << 20
)

func populateChangedPaths(context *buildkitepipeline.TriggerConditionContext, event compiler.Event, origin effectiveEventOrigin, workflows []workflowInput) {
	if event.Event != "pull_request" || !workflowsUsePathFilters(workflows, event.Event) {
		return
	}
	if origin != effectiveEventFromWebhook {
		context.ChangedPathsError = "pull request path filters require linked Buildkite webhook data"
		return
	}
	pullRequestNumber, err := strconv.Atoi(os.Getenv("BUILDKITE_PULL_REQUEST"))
	if err != nil || pullRequestNumber <= 0 {
		context.ChangedPathsError = "pull request path filters require the Buildkite pull request number"
		return
	}
	baseRef := os.Getenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH")
	if baseRef == "" {
		context.ChangedPathsError = "pull request path filters require the Buildkite pull request base branch"
		return
	}
	paths, workflowErrors, err := pullRequestChangedPaths(event, pullRequestNumber, baseRef, workflows)
	if err != nil {
		context.ChangedPathsError = err.Error()
		return
	}
	context.ChangedPaths = paths
	context.ChangedPathsKnown = true
	for i := range workflows {
		workflows[i].PathFiltersError = workflowErrors[workflows[i].CanonicalPath]
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
	shallowBytes, err := gitCommand(root, "rev-parse", "--is-shallow-repository").Output()
	if err != nil || strings.TrimSpace(string(shallowBytes)) != "false" {
		return nil, nil, fmt.Errorf("pull request path filters require a complete non-shallow checkout")
	}
	for _, commit := range []struct{ label, sha string }{{"base", baseSHA}, {"head", headSHA}, {"merge", mergeSHA}} {
		if err := gitCommand(root, "cat-file", "-e", commit.sha+"^{commit}").Run(); err != nil {
			return nil, nil, fmt.Errorf("pull request %s commit is unavailable in the local checkout", commit.label)
		}
	}
	baseTipBytes, err := gitCommand(root, "rev-parse", "--verify", "refs/remotes/origin/"+baseRef+"^{commit}").Output()
	if err != nil || strings.TrimSpace(string(baseTipBytes)) != baseSHA {
		return nil, nil, fmt.Errorf("webhook pull request base commit does not match the local origin base branch")
	}
	mergeParentsBytes, err := gitCommand(root, "rev-list", "--parents", "-n", "1", mergeSHA).Output()
	mergeParents := strings.Fields(string(mergeParentsBytes))
	if err != nil || len(mergeParents) != 3 || mergeParents[0] != mergeSHA || mergeParents[1] != baseSHA || mergeParents[2] != headSHA {
		return nil, nil, fmt.Errorf("webhook pull request merge commit does not bind the event base and head")
	}
	workflowErrors := make(map[string]string)
	for _, input := range workflows {
		if !workflowUsesPathFilters(input, event.Event) {
			continue
		}
		mergeSource, err := gitCommand(root, "cat-file", "blob", mergeSHA+":"+input.CanonicalPath).Output()
		if err != nil || !bytes.Equal(mergeSource, input.Source) {
			workflowErrors[input.CanonicalPath] = fmt.Sprintf("workflow %q does not match the event merge commit", input.CanonicalPath)
		}
	}
	mergeBaseBytes, err := gitCommand(root, "merge-base", "--all", baseSHA, headSHA).Output()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve pull request merge base: %w", err)
	}
	mergeBase, err := singleGitCommit(mergeBaseBytes, "pull request merge base")
	if err != nil {
		return nil, nil, err
	}
	output, err := boundedCommandOutput(gitCommand(root, "diff", "--name-status", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", mergeBase, headSHA), maxGitChangedPathBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("list pull request changed paths: %w", err)
	}
	paths, err := parseChangedPaths(output)
	return paths, workflowErrors, err
}

func workflowUsesPathFilters(input workflowInput, event string) bool {
	for _, trigger := range input.Triggers {
		if trigger.Event == event && (trigger.Paths != nil || trigger.PathsIgnore != nil) {
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
			return nil, fmt.Errorf("pull request changes exceed the importer's %d-file local evaluation bound", maxLocallyEvaluatedPathFilterFiles)
		}
	}
	if added && deleted {
		return nil, fmt.Errorf("combined added and deleted files require provider rename conformance data")
	}
	return paths, nil
}
