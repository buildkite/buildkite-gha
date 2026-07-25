package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// buildkiteEventSource creates compatibility data only. Buildkite environment
// variables can be changed by hooks and this snapshot is not an attestation.
func buildkiteEventSource(getenv func(string) string) ([]byte, error) {
	if getenv("BUILDKITE") != "true" {
		return nil, fmt.Errorf("BUILDKITE must be true")
	}
	if strings.TrimSpace(getenv("BUILDKITE_STEP_KEY")) == "" {
		return nil, fmt.Errorf("BUILDKITE_STEP_KEY is required")
	}
	owner, name, cloneURL, err := parsePublicGitHubRepository(getenv("BUILDKITE_REPO"))
	if err != nil {
		return nil, fmt.Errorf("BUILDKITE_REPO: %w", err)
	}
	sha := getenv("BUILDKITE_COMMIT")
	decoded, err := hex.DecodeString(sha)
	if err != nil || len(decoded) != 20 || sha != strings.ToLower(sha) {
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
	payload := map[string]any{}
	if pullRequest != "" && pullRequest != "false" {
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
			headOwner, headName, headCloneURL, err = parsePublicGitHubRepository(pullRequestRepo)
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
		payload["number"], payload["pull_request"] = number, pr
	} else if strings.TrimSpace(tag) != "" {
		ref = "refs/tags/" + tag
		payload["ref"] = ref
	} else {
		if strings.TrimSpace(branch) == "" {
			return nil, fmt.Errorf("BUILDKITE_BRANCH or BUILDKITE_TAG is required")
		}
		ref = "refs/heads/" + branch
		payload["ref"] = ref
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
	}{"github", event, repository, ref, sha, actor, payload}
	result, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode Buildkite compatibility snapshot: %w", err)
	}
	return result, nil
}

func parsePublicGitHubRepository(raw string) (owner, name, cloneURL string, err error) {
	if raw == "" {
		return "", "", "", fmt.Errorf("is required")
	}
	path := ""
	if strings.HasPrefix(raw, "git@github.com:") {
		path = strings.TrimPrefix(raw, "git@github.com:")
	} else {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Hostname() != "github.com" || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", "", fmt.Errorf("must be a public github.com repository URL")
		}
		switch u.Scheme {
		case "https", "git":
			if u.User != nil {
				return "", "", "", fmt.Errorf("must not contain credentials")
			}
		case "ssh":
			if u.User == nil || u.User.String() != "git" {
				return "", "", "", fmt.Errorf("SSH repository URL must use the git user")
			}
		default:
			return "", "", "", fmt.Errorf("must use https, git, or SSH")
		}
		path = strings.TrimPrefix(u.EscapedPath(), "/")
		if decodedPath, decodeErr := url.PathUnescape(path); decodeErr == nil {
			path = decodedPath
		} else {
			return "", "", "", fmt.Errorf("has a malformed path")
		}
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("must contain exactly an owner and repository name")
	}
	owner, name = parts[0], strings.TrimSuffix(parts[1], ".git")
	if name == "" || name == "." || name == ".." || !validGitHubPathPart(owner, false) || !validGitHubPathPart(name, true) {
		return "", "", "", fmt.Errorf("has a malformed owner or repository name")
	}
	return owner, name, "https://github.com/" + owner + "/" + name + ".git", nil
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
