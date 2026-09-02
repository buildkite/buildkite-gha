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

	"github.com/buildkite/buildkite-gha/internal/agentapi"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

const githubTokenResponseLimit = 64 << 10

var githubInstallationTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var retryAfterSecondsPattern = regexp.MustCompile(`^[0-9]{1,10}$`)
var buildkiteSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var buildkiteBuildPathPattern = regexp.MustCompile(`^/[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9-]*/builds/[1-9][0-9]*/?$`)

// WorkflowTokenProvider mints one repository-scoped credential with the exact
// plan-declared permissions accepted by the Buildkite backend.
type WorkflowTokenProvider interface {
	WorkflowToken(context.Context, string, string, map[string]string) (string, error)
}

// ActionSourceTokenProvider mints a credential for resolving public GitHub
// action sources.
type ActionSourceTokenProvider interface {
	ActionSourceToken(context.Context, string) (string, error)
}

// AgentGitHubTokenConfig carries the current Buildkite job's Agent connection,
// authentication material, and immutable Buildkite pipeline slugs. This client
// does not add them to Git or action subprocess environments.
type AgentGitHubTokenConfig struct {
	Endpoint         string
	JobID            string
	JobToken         string
	OrganizationSlug string
	PipelineSlug     string
	BuildURL         string
	ClientVersion    string
	Client           *http.Client
}

// AgentGitHubTokens mints repository-scoped tokens through Buildkite's
// job-bound Agent API endpoint.
type AgentGitHubTokens struct {
	actionSourceURL    string
	workflowURL        string
	repositorySettings string
	buildURL           string
	agent              *agentapi.Client
}

func NewAgentGitHubTokens(config AgentGitHubTokenConfig) (*AgentGitHubTokens, error) {
	agent, err := agentapi.New(agentapi.Config{
		Endpoint: config.Endpoint, JobID: config.JobID, JobToken: config.JobToken,
		ClientVersion: config.ClientVersion, HTTPClient: config.Client,
	}, "GitHub token")
	if err != nil {
		return nil, err
	}
	return &AgentGitHubTokens{
		actionSourceURL:    agent.URL("github_action_source_access_token"),
		workflowURL:        agent.URL("github_workflow_access_token"),
		repositorySettings: pipelineRepositorySettingsURL(config.OrganizationSlug, config.PipelineSlug),
		buildURL:           safeBuildkiteBuildURL(config.BuildURL),
		agent:              agent,
	}, nil
}

func (c *AgentGitHubTokens) WorkflowToken(ctx context.Context, repository, workflow string, permissions map[string]string) (token string, err error) {
	defer func() { err = markJobSetupFailure(FailureClassWorkflowToken, err) }()
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

func (c *AgentGitHubTokens) ActionSourceToken(ctx context.Context, repository string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("GitHub action source token provider is not configured")
	}
	return c.mint(ctx, c.actionSourceURL, repository, "", nil, "action source")
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
		Permissions   map[string]string `json:"permissions,omitempty"`
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
	request.Header.Set("Content-Type", "application/json")
	response, err := c.agent.Do(request)
	if err != nil {
		return "", fmt.Errorf("request GitHub %s token: %w", purpose, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, githubTokenResponseLimit))
		return "", githubTokenStatusError(response.StatusCode, response.Header.Get("Retry-After"), purpose, c.repositorySettings, c.buildURL)
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

func pipelineRepositorySettingsURL(organization, pipeline string) string {
	if !buildkiteSlugPattern.MatchString(organization) || !buildkiteSlugPattern.MatchString(pipeline) {
		return ""
	}
	return "https://buildkite.com/" + organization + "/" + pipeline + "/settings/repository"
}

func safeBuildkiteBuildURL(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "buildkite.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !buildkiteBuildPathPattern.MatchString(u.Path) {
		return ""
	}
	return u.String()
}

func githubTokenStatusError(status int, retryAfter, purpose, repositorySettings, buildURL string) error {
	credential := "GitHub " + purpose + " token"
	switch status {
	case http.StatusBadRequest:
		if purpose == "workflow" {
			message := "GitHub workflow token request was rejected. Check the workflow's top-level permissions and confirm that the Buildkite GitHub App can access the event repository. If both are correct, contact Buildkite support and include this build's URL"
			if buildURL != "" {
				message += ": " + buildURL
			}
			return newJobSetupHTTPFailure(FailureClassWorkflowToken, status, message)
		}
		return fmt.Errorf("%s request was rejected", credential)
	case http.StatusUnauthorized, http.StatusForbidden:
		if purpose == "workflow" {
			return newJobSetupHTTPFailure(FailureClassWorkflowToken, status, credential+" request was denied")
		}
		return fmt.Errorf("%s request was denied", credential)
	case http.StatusNotFound:
		if purpose == "workflow" {
			message := `GitHub workflow access tokens are not enabled for this organization or pipeline; enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings`
			if repositorySettings != "" {
				message += ": " + repositorySettings
			}
			return newJobSetupHTTPFailure(FailureClassWorkflowToken, status, message)
		}
		return fmt.Errorf("GitHub action source access tokens are not enabled for this organization")
	case http.StatusServiceUnavailable:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			message := fmt.Sprintf("%s service is temporarily unavailable; retry after %s seconds", credential, retryAfter)
			if purpose == "workflow" {
				return newJobSetupHTTPFailure(FailureClassWorkflowToken, status, message)
			}
			return errors.New(message)
		}
		message := credential + " service is temporarily unavailable"
		if purpose == "workflow" {
			return newJobSetupHTTPFailure(FailureClassWorkflowToken, status, message)
		}
		return errors.New(message)
	default:
		message := fmt.Sprintf("%s service returned HTTP %d", credential, status)
		if purpose == "workflow" {
			return newJobSetupHTTPFailure(FailureClassWorkflowToken, status, message)
		}
		return errors.New(message)
	}
}
