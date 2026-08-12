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

	"github.com/buildkite/buildkite-gha/internal/plan"
)

const githubTokenResponseLimit = 64 << 10

// Rejection reasons come from an untrusted response body, so cap them well
// short of anything a terminal or log line needs to display in full.
const githubTokenRejectionReasonRuneLimit = 200

var githubInstallationTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var retryAfterSecondsPattern = regexp.MustCompile(`^[0-9]{1,10}$`)

// WorkflowTokenProvider mints one repository-scoped credential with the exact
// plan-declared permissions accepted by the Buildkite backend.
type WorkflowTokenProvider interface {
	WorkflowToken(context.Context, string, string, map[string]string) (string, error)
}

// ScopedTokenProvider mints a repository-scoped credential without selecting
// the workflow-policy authority.
type ScopedTokenProvider interface {
	ScopedToken(context.Context, string, map[string]string) (string, error)
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
	scopedURL   string
	workflowURL string
	jobToken    string
	client      *http.Client
}

func NewAgentGitHubTokens(config AgentGitHubTokenConfig) (*AgentGitHubTokens, error) {
	scopedURL, err := agentGitHubTokenURL(config.Endpoint, config.JobID, "github_scoped_access_token")
	if err != nil {
		return nil, err
	}
	workflowURL, err := agentGitHubTokenURL(config.Endpoint, config.JobID, "github_workflow_access_token")
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
	return &AgentGitHubTokens{scopedURL: scopedURL, workflowURL: workflowURL, jobToken: config.JobToken, client: &bounded}, nil
}

func (c *AgentGitHubTokens) WorkflowToken(ctx context.Context, repository, workflow string, permissions map[string]string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("GitHub workflow token provider is not configured")
	}
	if err := plan.ValidateGitHubWorkflowPolicyFilename(workflow); err != nil {
		return "", err
	}
	if err := plan.ValidateGitHubWorkflowAccessTokenPermissions(permissions); err != nil {
		return "", err
	}
	return c.mint(ctx, c.workflowURL, repository, workflow, permissions, "workflow")
}

func (c *AgentGitHubTokens) ScopedToken(ctx context.Context, repository string, permissions map[string]string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("GitHub scoped token provider is not configured")
	}
	if !validScopedTokenPermissions(permissions) {
		return "", fmt.Errorf("GitHub scoped token requires valid permissions")
	}
	return c.mint(ctx, c.scopedURL, repository, "", permissions, "scoped")
}

func (c *AgentGitHubTokens) mint(ctx context.Context, mintURL, repository, workflow string, permissions map[string]string, purpose string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("GitHub %s token provider is not configured", purpose)
	}
	if !validCheckoutRepository(repository) {
		return "", fmt.Errorf("GitHub %s token requires a valid event repository", purpose)
	}
	body, err := json.Marshal(struct {
		RepositoryURL string            `json:"repo_url"`
		Workflow      string            `json:"workflow,omitempty"`
		Permissions   map[string]string `json:"permissions"`
	}{
		RepositoryURL: "https://github.com/" + repository,
		Workflow:      workflow,
		Permissions:   permissions,
	})
	if err != nil {
		return "", fmt.Errorf("encode GitHub %s token request: %w", purpose, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, mintURL, bytes.NewReader(body))
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
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, githubTokenResponseLimit))
		return "", githubTokenStatusError(response.StatusCode, response.Header.Get("Retry-After"), purpose, errorBody)
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

func validScopedTokenPermissions(permissions map[string]string) bool {
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

func agentGitHubTokenURL(endpoint, jobID, endpointName string) (string, error) {
	if !validBuildkiteJobID(jobID) {
		return "", fmt.Errorf("GitHub token Agent job ID is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || !validCredentialServiceURL(u) {
		return "", fmt.Errorf("safe GitHub token Agent endpoint using HTTPS or loopback HTTP is required")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jobs/" + jobID + "/" + endpointName
	u.RawPath = ""
	return u.String(), nil
}

func githubTokenStatusError(status int, retryAfter, purpose string, body []byte) error {
	credential := "GitHub " + purpose + " token"
	switch status {
	case http.StatusBadRequest:
		if reason := githubTokenRejectionReason(body); reason != "" {
			return fmt.Errorf("%s request was rejected: %s", credential, reason)
		}
		return fmt.Errorf("%s request was rejected", credential)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s request was denied", credential)
	case http.StatusNotFound:
		if purpose == "workflow" {
			return fmt.Errorf("GitHub workflow access tokens are not enabled for this organization or pipeline")
		}
		return fmt.Errorf("GitHub scoped access tokens are not enabled for this organization")
	case http.StatusServiceUnavailable:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			return fmt.Errorf("%s service is temporarily unavailable; retry after %s seconds", credential, retryAfter)
		}
		return fmt.Errorf("%s service is temporarily unavailable", credential)
	default:
		return fmt.Errorf("%s service returned HTTP %d", credential, status)
	}
}

var githubTokenControlCharPattern = regexp.MustCompile(`[\x00-\x1F\x7F]+`)

// githubTokenRejectionReason turns a 400 response body into an operator-facing
// reason: the JSON "message" field the Agent API renders for its errors, or
// the raw body when it isn't that shape.
func githubTokenRejectionReason(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	var decoded struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(trimmed, &decoded) == nil && decoded.Message != "" {
		return sanitizeGithubTokenRejectionReason(decoded.Message)
	}
	return sanitizeGithubTokenRejectionReason(string(trimmed))
}

func sanitizeGithubTokenRejectionReason(reason string) string {
	reason = strings.TrimSpace(githubTokenControlCharPattern.ReplaceAllString(reason, " "))
	if runes := []rune(reason); len(runes) > githubTokenRejectionReasonRuneLimit {
		reason = string(runes[:githubTokenRejectionReasonRuneLimit])
	}
	return reason
}
