package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/agentapi"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

const githubTokenResponseLimit = 64 << 10
const workflowTokenBudget = 45 * time.Second

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
	actionSourceURL     string
	workflowURL         string
	repositorySettings  string
	repositoryProviders string
	buildURL            string
	agent               *agentapi.Client
}

func NewAgentGitHubTokens(config AgentGitHubTokenConfig) (*AgentGitHubTokens, error) {
	agent, err := agentapi.New(agentapi.Config{
		Endpoint: config.Endpoint, JobID: config.JobID, JobToken: config.JobToken,
		ClientVersion: config.ClientVersion, HTTPClient: config.Client,
	}, "GitHub token")
	if err != nil {
		return nil, err
	}
	var repositoryProviders string
	if buildkiteSlugPattern.MatchString(config.OrganizationSlug) {
		repositoryProviders = "https://buildkite.com/organizations/" + config.OrganizationSlug + "/repository-providers"
	}
	return &AgentGitHubTokens{
		actionSourceURL:     agent.URL("github_action_source_access_token"),
		workflowURL:         agent.URL("github_workflow_access_token"),
		repositorySettings:  pipelineRepositorySettingsURL(config.OrganizationSlug, config.PipelineSlug),
		repositoryProviders: repositoryProviders,
		buildURL:            safeBuildkiteBuildURL(config.BuildURL),
		agent:               agent,
	}, nil
}

func (c *AgentGitHubTokens) WorkflowToken(ctx context.Context, repository, workflow string, permissions map[string]string) (token string, err error) {
	if c == nil {
		return "", fmt.Errorf("GitHub workflow token provider is not configured")
	}
	if err := plan.ValidateGitHubWorkflowPolicyFilename(workflow); err != nil {
		return "", err
	}
	if err := plan.ValidateGitHubWorkflowAccessTokenPermissions(permissions); err != nil {
		return "", err
	}
	permissions = maps.Clone(permissions)
	budgetCtx, cancel := context.WithTimeout(ctx, workflowTokenBudget)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			token, err = "", ctx.Err()
			return
		}
		// Our acquisition/attempt deadline is a setup failure, not caller cancellation.
		if errors.Is(err, context.DeadlineExceeded) {
			err = credentialRequestFailure(ctx, err)
		}
		err = markJobSetupFailure(FailureClassWorkflowToken, err)
	}()
	for attempt := range 3 {
		if err = budgetCtx.Err(); err != nil {
			return "", err
		}
		attemptCtx, cancelAttempt := context.WithTimeout(budgetCtx, 15*time.Second)
		token, err = c.mint(attemptCtx, c.workflowURL, repository, workflow, permissions, "workflow")
		cancelAttempt()
		var retry *workflowTokenRetryError
		if attempt == 2 || !errors.As(err, &retry) {
			return token, err
		}
		base := time.Second << attempt
		delay := max(base, retry.after) + time.Duration(rand.Int64N(int64(base)))
		deadline, _ := budgetCtx.Deadline()
		if delay >= time.Until(deadline) {
			return "", err
		}
		timer := time.NewTimer(delay)
		select {
		case <-budgetCtx.Done():
			timer.Stop()
			return "", budgetCtx.Err()
		case <-timer.C:
		}
	}
	return token, err
}

func (c *AgentGitHubTokens) ActionSourceToken(ctx context.Context, repository string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("GitHub action source token provider is not configured")
	}
	return c.mint(ctx, c.actionSourceURL, repository, "", nil, "action source")
}

func (c *AgentGitHubTokens) mint(ctx context.Context, mintURL, repository, workflow string, permissions map[string]string, purpose string) (token string, err error) {
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
	defer func() { err = credentialRequestFailure(ctx, err) }()
	request.Header.Set("Content-Type", "application/json")
	response, err := c.agent.Do(request)
	if err != nil {
		return "", fmt.Errorf("request GitHub %s token: %w", purpose, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, githubTokenResponseLimit))
		statusErr := githubTokenStatusError(response.StatusCode, response.Header.Get("Retry-After"), purpose, c.repositorySettings, c.buildURL)
		if purpose == "workflow" && readErr == nil && len(payload) < githubTokenResponseLimit {
			var denied struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(payload, &denied) == nil {
				if message := c.workflowDenialMessage(response.StatusCode, denied.Code, repository, permissions); message != "" {
					statusErr = credentialStatusError(response.StatusCode, "%s", message)
				}
			}
		}
		// A status alone does not authorize retrying a failed response-body read.
		if purpose == "workflow" && readErr == nil {
			after, valid := workflowTokenRetryAfter(response.Header.Get("Retry-After"), time.Now())
			if response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusTooManyRequests && valid {
				return "", &workflowTokenRetryError{err: statusErr, after: after}
			}
		}
		return "", statusErr
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
		return "", fmt.Errorf("decode GitHub %s token response", purpose)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("GitHub %s token response has trailing data", purpose)
	}
	if len(decoded.Token) > 16<<10 || !githubInstallationTokenPattern.MatchString(decoded.Token) {
		return "", fmt.Errorf("GitHub %s token response contains an invalid token", purpose)
	}
	return decoded.Token, nil
}

func (c *AgentGitHubTokens) workflowDenialMessage(status int, code, repository string, permissions map[string]string) string {
	if code == "pipeline_disabled" {
		if status != http.StatusNotFound {
			return ""
		}
	} else if status != http.StatusBadRequest {
		return ""
	}
	var cause string
	switch code {
	case "pipeline_disabled":
		cause = `Workflow access tokens are disabled for this pipeline. Ask a pipeline administrator to enable "Allow workflow-authorized GitHub access tokens" in repository settings if this workflow should use them. This does not approve additional GitHub App permissions.`
	case "workflow_policy_exceeded":
		cause = "The requested token exceeds the workflow's top-level permissions at this build's commit. " +
			"Ask the workflow maintainer to remove unused requested permissions, or explicitly declare the permissions the workflow needs at the top level. " +
			"Job-level permissions cannot widen that policy. GitHub App grants and pull-request restrictions still apply."
	case "workflow_policy_invalid":
		cause = "Buildkite could not read a supported top-level permission policy from the workflow at this build's commit. " +
			"Ask the workflow maintainer to use a valid explicit permission map containing only the permissions the workflow needs. " +
			"Reinstalling the GitHub App will not fix the workflow policy."
	case "workflow_provenance_invalid":
		cause = "Buildkite could not validate this build's source and ancestry for a workflow token. " +
			"Contact Buildkite support with this build's URL. Do not change App permissions or bypass the source checks."
	case "merge_queue_not_supported":
		cause = "Workflow access tokens are not supported for merge-queue builds or their triggered or rebuilt descendants. " +
			"Contact Buildkite support about supported merge-queue workflows. Changing App permissions will not remove this restriction."
	case "permissions_unavailable":
		cause = "GitHub rejected token permissions while preparing this job. Compare the App registration with the installation's granted permissions. " +
			"A GitHub installation admin can approve permissions only if the App already requests them; otherwise the App owner must review the registration. " +
			"This response does not identify an individual missing grant or an approval waiting for review."
	case "repository_unavailable":
		cause = "GitHub could not grant access to this repository. Ask a GitHub installation admin to check that the repository exists and is selected for the App installation."
	case "code_access_connection_unavailable":
		cause = "No active GitHub repository provider with code access matches this pipeline. Buildkite (Limited Access) cannot issue workflow tokens. " +
			"Ask a Buildkite organization admin to review Repository Providers, including account matching and suspension. " +
			"If a new installation is needed, a GitHub installation admin must review its permissions and selected repositories."
	case "pr_permission_ceiling":
		cause = "This build is a pull-request build, or was triggered or rebuilt from one. Buildkite limits its workflow token to contents:read. " +
			"Ask the workflow maintainer to remove extra permissions only if they are unused. " +
			"Operations requiring more access need a separately trusted workflow that does not execute untrusted PR code. " +
			"Reinstalling the GitHub App or approving installation permissions will not change this restriction."
	default:
		return ""
	}
	requested := make([]string, 0, len(permissions))
	for name, level := range permissions {
		requested = append(requested, name+":"+level)
	}
	sort.Strings(requested)
	message := cause + "\nWorkflow requested " + strings.Join(requested, ", ") + " for " + repository + "."
	switch {
	case code == "pr_permission_ceiling" || code == "workflow_policy_exceeded" || code == "workflow_policy_invalid" || code == "workflow_provenance_invalid" || code == "merge_queue_not_supported":
		message += "\nWorkflow compatibility: https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility"
	case code == "code_access_connection_unavailable" && c.repositoryProviders != "":
		message += "\nRepository Providers: " + c.repositoryProviders
	case c.repositorySettings != "":
		message += "\nPipeline repository settings: " + c.repositorySettings
	}
	if c.buildURL != "" {
		message += "\nIf unresolved, contact Buildkite support with this build: " + c.buildURL
	}
	return message
}

// WorkflowTokenFailureMessage selects the sanitized HTTP failure, excluding
// unrelated errors joined during cleanup or result publication.
func WorkflowTokenFailureMessage(err error) string {
	var setup *jobSetupFailure
	if !errors.As(err, &setup) || setup.class != FailureClassWorkflowToken {
		return ""
	}
	var request *credentialRequestError
	if !errors.As(setup.err, &request) || request.status == 0 {
		return ""
	}
	return request.Error()
}

type workflowTokenRetryError struct {
	err   error
	after time.Duration
}

func (e *workflowTokenRetryError) Error() string { return e.err.Error() }
func (e *workflowTokenRetryError) Unwrap() error { return e.err }

// Cap valid delays at the acquisition budget: they cannot fit, and must never
// overflow into a short wait. Invalid headers do not authorize a 429 retry.
func workflowTokenRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value != "" && strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds >= uint64(workflowTokenBudget/time.Second) {
			return workflowTokenBudget, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(workflowTokenBudget, max(0, date.Sub(now))), true
	}
	return 0, false
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
			return credentialStatusError(status, "%s", message)
		}
		return credentialStatusError(status, "%s request was rejected", credential)
	case http.StatusUnauthorized, http.StatusForbidden:
		return credentialStatusError(status, "%s request was denied", credential)
	case http.StatusNotFound:
		if purpose == "workflow" {
			message := `GitHub workflow access tokens are not enabled for this organization or pipeline; enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings`
			if repositorySettings != "" {
				message += ": " + repositorySettings
			}
			return credentialStatusError(status, "%s", message)
		}
		return credentialStatusError(status, "GitHub action source access tokens are not enabled for this organization")
	case http.StatusServiceUnavailable:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			return credentialStatusError(status, "%s service is temporarily unavailable; retry after %s seconds", credential, retryAfter)
		}
		return credentialStatusError(status, "%s service is temporarily unavailable", credential)
	default:
		return credentialStatusError(status, "%s service returned HTTP %d", credential, status)
	}
}
