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

const maxGitHubPathFilterFiles = 300

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
	paths, err := pullRequestChangedPaths(event, pullRequestNumber, baseRef)
	if err != nil {
		context.ChangedPathsError = err.Error()
		return
	}
	context.ChangedPaths = paths
	context.ChangedPathsKnown = true
}

func workflowsUsePathFilters(workflows []workflowInput, event string) bool {
	for _, input := range workflows {
		for _, trigger := range input.Triggers {
			if trigger.Event == event && (trigger.Paths != nil || trigger.PathsIgnore != nil) {
				return true
			}
		}
	}
	return false
}

func pullRequestChangedPaths(event compiler.Event, pullRequestNumber int, baseRef string) ([]string, error) {
	pullRequest, ok := event.Payload["pull_request"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("event snapshot requires payload.pull_request")
	}
	baseSHA := nestedString(pullRequest, "base", "sha")
	headSHA := nestedString(pullRequest, "head", "sha")
	if nestedString(pullRequest, "base", "ref") != baseRef {
		return nil, fmt.Errorf("webhook pull request base branch does not match the Buildkite build")
	}
	if nestedString(pullRequest, "base", "repo", "full_name") != event.Repository.Owner+"/"+event.Repository.Name {
		return nil, fmt.Errorf("webhook pull request base repository does not match the Buildkite build")
	}
	if nestedInt(event.Payload, "number") != pullRequestNumber {
		return nil, fmt.Errorf("webhook pull request number does not match the Buildkite build")
	}
	if !validBuildkiteCommit(baseSHA) || !validBuildkiteCommit(headSHA) {
		return nil, fmt.Errorf("event snapshot requires full lowercase payload.pull_request base and head SHAs")
	}
	if headSHA != event.SHA {
		return nil, fmt.Errorf("pull request head SHA does not match the checked-out event SHA")
	}
	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, fmt.Errorf("locate checked-out git repository: %w", err)
	}
	root := filepath.Clean(strings.TrimSpace(string(rootBytes)))
	if err := gitCommand(root, "check-ref-format", "refs/heads/"+baseRef).Run(); err != nil {
		return nil, fmt.Errorf("buildkite pull request base branch is invalid")
	}
	for _, commit := range []struct{ label, sha string }{{"base", baseSHA}, {"head", headSHA}} {
		if err := gitCommand(root, "cat-file", "-e", commit.sha+"^{commit}").Run(); err != nil {
			return nil, fmt.Errorf("pull request %s commit is unavailable in the local checkout", commit.label)
		}
	}
	baseTipBytes, err := gitCommand(root, "rev-parse", "--verify", "refs/remotes/origin/"+baseRef+"^{commit}").Output()
	if err != nil || strings.TrimSpace(string(baseTipBytes)) != baseSHA {
		return nil, fmt.Errorf("webhook pull request base commit does not match the local origin base branch")
	}
	mergeBaseBytes, err := gitCommand(root, "merge-base", baseSHA, headSHA).Output()
	if err != nil {
		return nil, fmt.Errorf("resolve pull request merge base: %w", err)
	}
	mergeBase := strings.TrimSpace(string(mergeBaseBytes))
	if !validBuildkiteCommit(mergeBase) {
		return nil, fmt.Errorf("git returned an invalid pull request merge base")
	}
	output, err := gitCommand(root, "diff", "--name-status", "-z", "--find-renames", "--no-ext-diff", "--no-textconv", mergeBase, headSHA).Output()
	if err != nil {
		return nil, fmt.Errorf("list pull request changed paths: %w", err)
	}
	return parseChangedPaths(output)
}

func gitCommand(root string, args ...string) *exec.Cmd {
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	return command
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
		case 'A', 'D', 'M', 'T', 'U', 'X', 'B':
			paths = append(paths, path)
		case 'R', 'C':
			return nil, fmt.Errorf("renamed and copied files require provider conformance data")
		default:
			return nil, fmt.Errorf("git returned unsupported changed-path status %q", status)
		}
		if len(paths) > maxGitHubPathFilterFiles {
			return nil, fmt.Errorf("pull request changes exceed GitHub's %d-file path-filter limit", maxGitHubPathFilterFiles)
		}
	}
	return paths, nil
}
