package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const githubTokenResponseLimit = 64 << 10

var githubInstallationTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
var retryAfterSecondsPattern = regexp.MustCompile(`^[0-9]{1,10}$`)

// WorkflowTokenProvider mints one repository-scoped credential with the exact
// plan-declared permissions accepted by the Buildkite backend.
type WorkflowTokenProvider interface {
	WorkflowToken(context.Context, string, map[string]string) (string, error)
}

// AgentGitHubTokenConfig carries the current Buildkite job's Agent connection
// and authentication material. This client does not add it to Git or action
// subprocess environments.
type AgentGitHubTokenConfig struct {
	Endpoint string
	JobID    string
	JobToken string
	Client   *http.Client
}

// AgentGitHubTokens mints repository-scoped tokens through Buildkite's
// job-bound Agent API endpoint.
type AgentGitHubTokens struct {
	mintURL  string
	jobToken string
	client   *http.Client
}

func NewAgentGitHubTokens(config AgentGitHubTokenConfig) (*AgentGitHubTokens, error) {
	mintURL, err := agentGitHubTokenURL(config.Endpoint, config.JobID)
	if err != nil {
		return nil, err
	}
	if config.JobToken == "" || strings.ContainsAny(config.JobToken, "\r\n") {
		return nil, fmt.Errorf("GitHub token Agent job token is required")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	bounded := *client
	bounded.Jar = nil
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout == 0 {
		bounded.Timeout = 15 * time.Second
	}
	return &AgentGitHubTokens{mintURL: mintURL, jobToken: config.JobToken, client: &bounded}, nil
}

func (c *AgentGitHubTokens) WorkflowToken(ctx context.Context, repository string, permissions map[string]string) (string, error) {
	if !validWorkflowTokenPermissions(permissions) {
		return "", fmt.Errorf("GitHub workflow token requires valid explicit permissions")
	}
	return c.mint(ctx, repository, permissions, "workflow")
}

func (c *AgentGitHubTokens) mint(ctx context.Context, repository string, permissions map[string]string, purpose string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("GitHub %s token provider is not configured", purpose)
	}
	if !validCheckoutRepository(repository) {
		return "", fmt.Errorf("GitHub %s token requires a valid event repository", purpose)
	}
	body, err := json.Marshal(struct {
		RepositoryURL string            `json:"repo_url"`
		Permissions   map[string]string `json:"permissions"`
	}{
		RepositoryURL: "https://github.com/" + repository,
		Permissions:   permissions,
	})
	if err != nil {
		return "", fmt.Errorf("encode GitHub %s token request: %w", purpose, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mintURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create GitHub %s token request: %w", purpose, err)
	}
	request.Header.Set("Authorization", "Token "+c.jobToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request GitHub %s token: %w", purpose, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, githubTokenResponseLimit))
		return "", githubTokenStatusError(response.StatusCode, response.Header.Get("Retry-After"))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, githubTokenResponseLimit+1))
	if err != nil {
		return "", fmt.Errorf("read GitHub %s token response: %w", purpose, err)
	}
	if len(payload) > githubTokenResponseLimit {
		return "", fmt.Errorf("GitHub %s token response exceeds the %d-byte limit", purpose, githubTokenResponseLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded struct {
		Token string `json:"token"`
	}
	if err := decoder.Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode GitHub %s token response: %w", purpose, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("GitHub %s token response has trailing data", purpose)
	}
	if len(decoded.Token) > 16<<10 || !githubInstallationTokenPattern.MatchString(decoded.Token) {
		return "", fmt.Errorf("GitHub %s token response contains an invalid token", purpose)
	}
	return decoded.Token, nil
}

func validWorkflowTokenPermissions(permissions map[string]string) bool {
	if len(permissions) == 0 || len(permissions) > 15 {
		return false
	}
	for name, access := range permissions {
		switch name {
		case "actions", "artifact_metadata", "attestations", "checks", "contents", "deployments", "discussions", "issues", "packages", "pages", "pull_requests", "repository_projects", "security_events", "statuses":
			if access != "read" && access != "write" {
				return false
			}
		case "models":
			if access != "read" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func agentGitHubTokenURL(endpoint, jobID string) (string, error) {
	if !validBuildkiteJobID(jobID) {
		return "", fmt.Errorf("GitHub token Agent job ID is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || !validCredentialServiceURL(u) {
		return "", fmt.Errorf("safe GitHub token Agent endpoint using HTTPS or loopback HTTP is required")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jobs/" + jobID + "/github_scoped_access_token"
	u.RawPath = ""
	return u.String(), nil
}

func githubTokenStatusError(status int, retryAfter string) error {
	switch status {
	case http.StatusBadRequest:
		return fmt.Errorf("GitHub token request was rejected")
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("GitHub token request was denied")
	case http.StatusNotFound:
		return fmt.Errorf("GitHub scoped access tokens are not enabled for this organization")
	case http.StatusServiceUnavailable:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			return fmt.Errorf("GitHub token service is temporarily unavailable; retry after %s seconds", retryAfter)
		}
		return fmt.Errorf("GitHub token service is temporarily unavailable")
	default:
		return fmt.Errorf("GitHub token service returned HTTP %d", status)
	}
}
