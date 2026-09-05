package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/git"
)

const githubEventNameEnvironment = "GITHUB_EVENT_NAME"

var githubEventNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,99}$`)

// buildkiteEventSource creates compatibility data only. Buildkite environment
// variables can be changed by hooks and this snapshot is not an attestation.
func buildkiteEventSource(getenv func(string) string) ([]byte, error) {
	if getenv("BUILDKITE") != "true" {
		return nil, fmt.Errorf("BUILDKITE must be true")
	}
	provider, owner, name, cloneURL, err := parseBuildkiteRepository(getenv("BUILDKITE_REPO"))
	if err != nil {
		return nil, fmt.Errorf("BUILDKITE_REPO: %w", err)
	}
	sha := getenv("BUILDKITE_COMMIT")
	if !git.ValidObjectID(sha) {
		return nil, fmt.Errorf("BUILDKITE_COMMIT must be a full lowercase 40-hex commit, not a symbolic ref")
	}
	branch, tag, pullRequest := getenv("BUILDKITE_BRANCH"), getenv("BUILDKITE_TAG"), getenv("BUILDKITE_PULL_REQUEST")
	actor := strings.TrimSpace(getenv("BUILDKITE_BUILD_AUTHOR"))
	if actor == "" {
		actor = strings.TrimSpace(getenv("BUILDKITE_BUILD_CREATOR"))
	}
	if actor == "" {
		actor = "buildkite"
	}

	event, ref := "push", ""
	if getenv("BUILDKITE_SOURCE") == "schedule" {
		event = "schedule"
	}
	payload := map[string]any{}
	switch {
	case pullRequest != "" && pullRequest != "false":
		number, parseErr := strconv.Atoi(pullRequest)
		if parseErr != nil || number <= 0 {
			return nil, fmt.Errorf("BUILDKITE_PULL_REQUEST must be false or a positive integer")
		}
		if tag != "" {
			return nil, fmt.Errorf("BUILDKITE_PULL_REQUEST and BUILDKITE_TAG are contradictory")
		}
		if strings.TrimSpace(branch) == "" {
			return nil, fmt.Errorf("BUILDKITE_BRANCH is required for a pull request compatibility snapshot")
		}
		event, ref = "pull_request", fmt.Sprintf("refs/pull/%d/head", number)
		headOwner, headName, headCloneURL := owner, name, cloneURL
		if pullRequestRepo := strings.TrimSpace(getenv("BUILDKITE_PULL_REQUEST_REPO")); pullRequestRepo != "" {
			_, headOwner, headName, headCloneURL, err = parseBuildkiteRepository(pullRequestRepo)
			if err != nil {
				return nil, fmt.Errorf("BUILDKITE_PULL_REQUEST_REPO: %w", err)
			}
		}
		headRef := branch
		if prefix := headOwner + ":"; strings.HasPrefix(headRef, prefix) {
			headRef = strings.TrimPrefix(headRef, prefix)
		}
		head := map[string]any{
			"sha": sha, "ref": headRef,
			"repo": map[string]any{"clone_url": headCloneURL, "full_name": headOwner + "/" + headName},
		}
		pr := map[string]any{
			"number": number,
			"head":   head,
			"base":   map[string]any{"repo": map[string]any{"clone_url": cloneURL, "full_name": owner + "/" + name}},
		}
		if base := getenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH"); base != "" {
			pr["base"].(map[string]any)["ref"] = base
		}
		// Pipeline Triggers provide the selected GitHub action. Other Buildkite
		// PR builds retain deterministic head-update semantics.
		action := "synchronize"
		if getenv(pipelineTriggerWorkflowPathEnvironment) != "" {
			if selectedAction := strings.TrimSpace(getenv("BUILDKITE_GITHUB_ACTION")); selectedAction != "" {
				action = selectedAction
			}
		}
		payload["action"], payload["number"], payload["pull_request"] = action, number, pr
	case strings.TrimSpace(tag) != "":
		ref = "refs/tags/" + tag
		payload["ref"] = ref
	default:
		if strings.TrimSpace(branch) == "" {
			return nil, fmt.Errorf("BUILDKITE_BRANCH or BUILDKITE_TAG is required")
		}
		ref = "refs/heads/" + branch
		payload["ref"] = ref
	}
	githubEvent, err := buildkiteGitHubEventName(getenv)
	if err != nil {
		return nil, err
	}
	if githubEvent != "" {
		switch githubEvent {
		case "push", "pull_request", "workflow_dispatch", "schedule":
			event = githubEvent
			// Rebuilds retain the original GitHub event even though Buildkite reports
			// their source as UI. A push may also be associated with an open pull
			// request, so restore its authoritative branch or tag ref.
			if event == "push" {
				if strings.TrimSpace(tag) != "" {
					ref = "refs/tags/" + tag
				} else if strings.TrimSpace(branch) != "" {
					ref = "refs/heads/" + branch
				}
				payload = map[string]any{"ref": ref}
			}
		case "issues", "issue_comment":
			if githubEvent == "issues" && getenv(pipelineTriggerWorkflowPathEnvironment) == "" &&
				getenv(githubWorkflowRefEnvironment) == "" && getenv(githubWorkflowSHAEnvironment) == "" {
				break
			}
			if getenv(pipelineTriggerWorkflowPathEnvironment) == "" ||
				getenv(githubWorkflowRefEnvironment) == "" ||
				getenv(githubWorkflowSHAEnvironment) == "" {
				return nil, fmt.Errorf("%s requires GitHub Actions Pipeline Trigger workflow path, ref, and SHA identity", githubEvent)
			}
			if getenv(githubWorkflowSHAEnvironment) != sha {
				return nil, fmt.Errorf("%s does not match BUILDKITE_COMMIT", githubWorkflowSHAEnvironment)
			}
			event = githubEvent
		}
	}
	if workflowRef := getenv(githubWorkflowRefEnvironment); workflowRef != "" {
		_, ref, err = parsePipelineTriggerWorkflowRef(workflowRef, event, getenv("BUILDKITE_REPO"))
		if err != nil {
			return nil, err
		}
	}

	repository := map[string]any{"owner": owner, "name": name, "clone_url": cloneURL}
	if defaultBranch := strings.TrimSpace(getenv("BUILDKITE_PIPELINE_DEFAULT_BRANCH")); defaultBranch != "" {
		repository["default_branch"] = defaultBranch
	}
	snapshot := struct {
		Provider string         `json:"provider"`
		Event    string         `json:"event"`
		Repo     map[string]any `json:"repository"`
		Ref      string         `json:"ref"`
		SHA      string         `json:"sha"`
		Actor    string         `json:"actor"`
		Payload  map[string]any `json:"payload"`
	}{provider, event, repository, ref, sha, actor, payload}
	result, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode Buildkite compatibility snapshot: %w", err)
	}
	return result, nil
}

// buildkiteWebhookEventSource overlays untrusted trigger data onto the
// execution identity derived and validated by buildkiteEventSource.
func buildkiteWebhookEventSource(getenv func(string) string, webhook []byte) ([]byte, error) {
	base, err := buildkiteEventSource(getenv)
	if err != nil {
		return nil, err
	}
	payload, err := parseWebhookPayload(webhook)
	if err != nil {
		return nil, err
	}
	var snapshot map[string]any
	decoder := json.NewDecoder(bytes.NewReader(base))
	decoder.UseNumber()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode Buildkite compatibility snapshot: %w", err)
	}
	snapshot["payload"] = payload
	event, err := buildkiteGitHubEventName(getenv)
	if err != nil {
		return nil, err
	}
	if event != "" {
		snapshot["event"] = event
	}
	if sender, ok := payload["sender"].(map[string]any); ok {
		if login, ok := sender["login"].(string); ok && safeGitHubLogin(login) {
			snapshot["actor"] = login
		}
	}
	if snapshot["event"] == "merge_group" {
		if err := validateBuildkiteMergeGroup(snapshot, getenv); err != nil {
			return nil, err
		}
	}
	if _, hasRelease := payload["release"]; hasRelease && snapshot["event"] != "release" {
		return nil, fmt.Errorf("release webhook payload does not match BUILDKITE_GITHUB_EVENT")
	}
	if snapshot["event"] == "release" {
		if err := validateBuildkiteRelease(snapshot, getenv); err != nil {
			return nil, err
		}
	}
	if snapshot["event"] == "issues" || snapshot["event"] == "issue_comment" {
		if err := validateBuildkiteIssueEvent(snapshot, getenv); err != nil {
			return nil, err
		}
	}
	result, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode Buildkite webhook snapshot: %w", err)
	}
	return result, nil
}

func buildkiteGitHubEventName(getenv func(string) string) (string, error) {
	if event := getenv(githubEventNameEnvironment); event != "" {
		if event != strings.TrimSpace(event) || !githubEventNamePattern.MatchString(event) {
			return "", fmt.Errorf("%s must be a lowercase GitHub event name", githubEventNameEnvironment)
		}
		return event, nil
	}
	event := strings.TrimSpace(getenv("BUILDKITE_GITHUB_EVENT"))
	if githubEventNamePattern.MatchString(event) {
		return event, nil
	}
	return "", nil
}

func validateBuildkiteMergeGroup(snapshot map[string]any, getenv func(string) string) error {
	payload := snapshot["payload"].(map[string]any)
	mergeGroup, ok := payload["merge_group"].(map[string]any)
	if !ok {
		return fmt.Errorf("merge_group webhook requires payload.merge_group")
	}
	if action, _ := payload["action"].(string); action != "checks_requested" {
		return fmt.Errorf("merge_group webhook action must be checks_requested")
	}
	ref, _ := snapshot["ref"].(string)
	sha, _ := snapshot["sha"].(string)
	if headRef, _ := mergeGroup["head_ref"].(string); headRef != ref {
		return fmt.Errorf("merge_group webhook head_ref does not match the Buildkite build branch")
	}
	if headSHA, _ := mergeGroup["head_sha"].(string); headSHA != sha {
		return fmt.Errorf("merge_group webhook head_sha does not match BUILDKITE_COMMIT")
	}
	baseBranch := strings.TrimSpace(getenv("BUILDKITE_MERGE_QUEUE_BASE_BRANCH"))
	if baseBranch == "" {
		return fmt.Errorf("BUILDKITE_MERGE_QUEUE_BASE_BRANCH is required for a merge_group webhook")
	}
	if baseRef, _ := mergeGroup["base_ref"].(string); baseRef != "refs/heads/"+baseBranch {
		return fmt.Errorf("merge_group webhook base_ref does not match BUILDKITE_MERGE_QUEUE_BASE_BRANCH")
	}
	baseCommit := getenv("BUILDKITE_MERGE_QUEUE_BASE_COMMIT")
	if !git.ValidObjectID(baseCommit) {
		return fmt.Errorf("BUILDKITE_MERGE_QUEUE_BASE_COMMIT must be a full lowercase 40-hex commit")
	}
	if baseSHA, _ := mergeGroup["base_sha"].(string); baseSHA != baseCommit {
		return fmt.Errorf("merge_group webhook base_sha does not match BUILDKITE_MERGE_QUEUE_BASE_COMMIT")
	}
	return nil
}

func validateBuildkiteRelease(snapshot map[string]any, getenv func(string) string) error {
	if snapshot["provider"] != "github" {
		return fmt.Errorf("release webhook requires a GitHub repository")
	}
	payload := snapshot["payload"].(map[string]any)
	action, _ := payload["action"].(string)
	if action == "" || action != strings.TrimSpace(getenv("BUILDKITE_GITHUB_ACTION")) {
		return fmt.Errorf("release webhook payload.action does not match BUILDKITE_GITHUB_ACTION")
	}
	if action != "published" && action != "created" && action != "released" {
		return fmt.Errorf("release webhook action %q is unsupported", action)
	}
	release, ok := payload["release"].(map[string]any)
	if !ok {
		return fmt.Errorf("release webhook requires payload.release")
	}
	tag, tagOK := release["tag_name"].(string)
	draft, draftOK := release["draft"].(bool)
	_, prereleaseOK := release["prerelease"].(bool)
	if !tagOK || strings.TrimSpace(tag) == "" || !draftOK || !prereleaseOK {
		return fmt.Errorf("release webhook requires payload.release tag_name, draft, and prerelease")
	}
	if draft {
		if action == "created" {
			return fmt.Errorf("release webhook draft created activity does not trigger GitHub Actions")
		}
		return fmt.Errorf("release webhook %s activity requires a non-draft release", action)
	}
	if tag != getenv("BUILDKITE_TAG") || tag != getenv("BUILDKITE_BRANCH") {
		return fmt.Errorf("release webhook tag_name does not match BUILDKITE_TAG and BUILDKITE_BRANCH")
	}
	snapshot["ref"] = "refs/tags/" + tag
	return nil
}

func validateBuildkiteIssueEvent(snapshot map[string]any, getenv func(string) string) error {
	event := snapshot["event"].(string)
	if snapshot["provider"] != "github" {
		return fmt.Errorf("%s webhook requires a GitHub repository", event)
	}
	payload := snapshot["payload"].(map[string]any)
	action, _ := payload["action"].(string)
	if action == "" || action != strings.TrimSpace(getenv("BUILDKITE_GITHUB_ACTION")) {
		return fmt.Errorf("%s webhook payload.action does not match BUILDKITE_GITHUB_ACTION", event)
	}
	supported := map[string]bool{
		"opened": true, "edited": true, "deleted": true, "transferred": true,
		"field_added": true, "field_removed": true,
		"pinned": true, "unpinned": true, "closed": true, "reopened": true,
		"assigned": true, "unassigned": true, "labeled": true, "unlabeled": true,
		"locked": true, "unlocked": true, "milestoned": true, "demilestoned": true,
		"typed": true, "untyped": true,
	}
	if event == "issue_comment" {
		supported = map[string]bool{"created": true, "edited": true, "deleted": true}
	}
	if !supported[action] {
		return fmt.Errorf("%s webhook action %q is unsupported", event, action)
	}
	issue, ok := payload["issue"].(map[string]any)
	if !ok || !positiveJSONInteger(issue["number"]) {
		return fmt.Errorf("%s webhook requires payload.issue.number", event)
	}
	if event == "issue_comment" {
		comment, ok := payload["comment"].(map[string]any)
		if !ok || !positiveJSONInteger(comment["id"]) {
			return fmt.Errorf("issue_comment webhook requires payload.comment.id")
		}
	}
	repository, _ := payload["repository"].(map[string]any)
	fullName, _ := repository["full_name"].(string)
	provider, owner, name, _, err := parseBuildkiteRepository(getenv("BUILDKITE_REPO"))
	if err != nil || provider != "github" || !strings.EqualFold(fullName, owner+"/"+name) || !positiveJSONInteger(repository["id"]) {
		return fmt.Errorf("%s webhook repository does not match BUILDKITE_REPO", event)
	}
	ref, _ := snapshot["ref"].(string)
	if ref != "refs/heads/"+getenv("BUILDKITE_BRANCH") {
		return fmt.Errorf("%s webhook ref does not match the Buildkite build branch", event)
	}
	return nil
}

func positiveJSONInteger(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	return err == nil && parsed > 0
}

func parseWebhookPayload(source []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(source)) == 0 || !utf8.Valid(source) {
		return nil, fmt.Errorf("buildkite:webhook must contain one valid UTF-8 JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return nil, fmt.Errorf("parse buildkite:webhook: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse buildkite:webhook: multiple JSON values")
		}
		return nil, fmt.Errorf("parse buildkite:webhook: %w", err)
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("buildkite:webhook must be a JSON object")
	}
	return payload, nil
}

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("conflicting duplicate object key %q", key)
			}
			object[key], err = decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func safeGitHubLogin(login string) bool {
	if strings.TrimSpace(login) != login || login == "" || len(login) > 255 {
		return false
	}
	for _, r := range login {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func parseBuildkiteRepository(raw string) (provider, owner, name, cloneURL string, err error) {
	if raw == "" {
		return "", "", "", "", fmt.Errorf("is required")
	}
	path := ""
	if after, ok := strings.CutPrefix(raw, "git@github.com:"); ok {
		path = after
		provider = "github"
	} else {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", "", "", fmt.Errorf("must be a github.com or Origin repository URL")
		}
		switch u.Hostname() {
		case "github.com":
			provider = "github"
			switch u.Scheme {
			case "https", "git":
				if u.User != nil {
					return "", "", "", "", fmt.Errorf("must not contain credentials")
				}
			case "ssh":
				if u.User == nil || u.User.String() != "git" {
					return "", "", "", "", fmt.Errorf("SSH repository URL must use the git user")
				}
			default:
				return "", "", "", "", fmt.Errorf("must use https, git, or SSH")
			}
		case "origin.cursor.com":
			provider = "cursor-origin"
			if u.Scheme != "https" {
				return "", "", "", "", fmt.Errorf("repository URL for Origin must use HTTPS")
			}
			if u.User != nil {
				return "", "", "", "", fmt.Errorf("must not contain credentials")
			}
		default:
			return "", "", "", "", fmt.Errorf("must be a github.com or Origin repository URL")
		}
		path = strings.TrimPrefix(u.EscapedPath(), "/")
		if decodedPath, decodeErr := url.PathUnescape(path); decodeErr == nil {
			path = decodedPath
		} else {
			return "", "", "", "", fmt.Errorf("has a malformed path")
		}
	}
	if provider == "cursor-origin" {
		parts := strings.Split(path, "/")
		if len(parts) != 3 || parts[0] != "git" || parts[1] == "" || !strings.HasSuffix(parts[2], ".git") {
			return "", "", "", "", fmt.Errorf("repository URL for Origin must be https://origin.cursor.com/git/<namespace>/<repository>.git")
		}
		owner, name = parts[1], strings.TrimSuffix(parts[2], ".git")
		if owner == "." || owner == ".." || name == "" || name == "." || name == ".." || !validGitHubPathPart(owner, true) || !validGitHubPathPart(name, true) {
			return "", "", "", "", fmt.Errorf("has a malformed namespace or repository name")
		}
		if raw != "https://origin.cursor.com/git/"+owner+"/"+name+".git" {
			return "", "", "", "", fmt.Errorf("repository URL for Origin must use its canonical form")
		}
		return provider, owner, name, raw, nil
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", fmt.Errorf("must contain exactly an owner and repository name")
	}
	owner, name = parts[0], strings.TrimSuffix(parts[1], ".git")
	if name == "" || name == "." || name == ".." || !validGitHubPathPart(owner, false) || !validGitHubPathPart(name, true) {
		return "", "", "", "", fmt.Errorf("has a malformed owner or repository name")
	}
	return provider, owner, name, "https://github.com/" + owner + "/" + name + ".git", nil
}

func validGitHubPathPart(value string, allowDotUnderscore bool) bool {
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || allowDotUnderscore && (r == '.' || r == '_') {
			continue
		}
		return false
	}
	return value != ""
}
