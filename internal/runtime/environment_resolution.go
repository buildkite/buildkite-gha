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
	"sort"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/useragent"
)

const environmentResolutionResponseLimit = 64 << 10

var environmentSecretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvironmentSnapshot is the Buildkite backend's fail-closed snapshot of one
// GitHub deployment environment. The backend performs the GitHub reads with
// its own credentials; no GitHub token or secret value reaches the importer.
type EnvironmentSnapshot struct {
	RequiredReviewers bool
	PreventSelfReview bool
	WaitTimerMinutes  int
	BranchPolicy      bool
	UnsupportedRules  []string
	SecretNames       []string
}

// AgentEnvironmentResolverConfig carries the current Buildkite job's Agent
// connection and authentication material.
type AgentEnvironmentResolverConfig struct {
	Endpoint      string
	JobID         string
	JobToken      string
	ClientVersion string
	Client        *http.Client
}

// AgentEnvironmentResolver resolves GitHub deployment environments through
// the job-scoped Agent API endpoint github-actions/environments. The backend
// restricts resolution to the pipeline's configured GitHub.com repository.
type AgentEnvironmentResolver struct {
	resolveURL string
	jobToken   string
	userAgent  string
	client     *http.Client
}

// NewAgentEnvironmentResolver validates the Agent connection configuration.
func NewAgentEnvironmentResolver(config AgentEnvironmentResolverConfig) (*AgentEnvironmentResolver, error) {
	resolveURL, err := agentEnvironmentResolutionURL(config.Endpoint, config.JobID)
	if err != nil {
		return nil, err
	}
	if config.JobToken == "" || strings.ContainsAny(config.JobToken, "\r\n") {
		return nil, fmt.Errorf("environment resolution Agent job token is required")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	bounded := *client
	bounded.Jar = nil
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout == 0 {
		bounded.Timeout = 30 * time.Second
	}
	return &AgentEnvironmentResolver{
		resolveURL: resolveURL, jobToken: config.JobToken,
		userAgent: useragent.FromVersion(config.ClientVersion), client: &bounded,
	}, nil
}

// ResolveEnvironments resolves the given distinct environments by name on the
// given owner/repository in one request, and returns their snapshots in
// request order. The Agent API owns batch-size and budget policy; its
// rejection surfaces as the request error. Every failure is terminal for the
// whole batch: deployment protection never degrades to an unprotected
// default.
func (c *AgentEnvironmentResolver) ResolveEnvironments(ctx context.Context, repository string, names []string) ([]EnvironmentSnapshot, error) {
	if c == nil {
		return nil, fmt.Errorf("environment resolver is not configured")
	}
	if !validCheckoutRepository(repository) {
		return nil, fmt.Errorf("environment resolution requires a valid event repository")
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("environment resolution requires an environment name")
	}
	identities := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" {
			return nil, fmt.Errorf("environment resolution requires an environment name")
		}
		identity := strings.ToLower(name)
		if identities[identity] {
			return nil, fmt.Errorf("environment resolution request repeats environment %q; GitHub environment names are case-insensitive", name)
		}
		identities[identity] = true
	}
	body, err := json.Marshal(struct {
		RepositoryURL    string   `json:"repo_url"`
		EnvironmentNames []string `json:"environment_names"`
	}{
		RepositoryURL:    "https://github.com/" + repository,
		EnvironmentNames: names,
	})
	if err != nil {
		return nil, fmt.Errorf("encode environment resolution request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create environment resolution request: %w", err)
	}
	request.Header.Set("Authorization", "Token "+c.jobToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request environment resolution: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	limit := environmentResolutionResponseLimit * len(names)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, environmentResolutionResponseLimit))
		return nil, environmentResolutionStatusError(response.StatusCode, response.Header.Get("Retry-After"), errorBody)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read environment resolution response: %w", err)
	}
	if len(payload) > limit {
		return nil, fmt.Errorf("environment resolution response exceeds the %d-byte limit", limit)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	// Every field decodes through a pointer so an absent or null field is
	// distinguishable from its zero value. Zero values would fail open: a
	// missing required_reviewers would drop the approval gate and missing
	// secret_names would leave secrets unscoped.
	var decoded struct {
		Environments []struct {
			Name              *string   `json:"name"`
			RequiredReviewers *bool     `json:"required_reviewers"`
			PreventSelfReview *bool     `json:"prevent_self_review"`
			WaitTimerMinutes  *int      `json:"wait_timer_minutes"`
			BranchPolicy      *bool     `json:"branch_policy"`
			UnsupportedRules  *[]string `json:"unsupported_rules"`
			SecretNames       *[]string `json:"secret_names"`
		} `json:"environments"`
	}
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode environment resolution response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("environment resolution response has trailing data")
	}
	if len(decoded.Environments) != len(names) {
		return nil, fmt.Errorf("environment resolution response has %d environments, want %d", len(decoded.Environments), len(names))
	}
	snapshots := make([]EnvironmentSnapshot, 0, len(names))
	for i, environment := range decoded.Environments {
		if environment.Name == nil {
			return nil, fmt.Errorf("environment resolution response omits the name of environment %d", i+1)
		}
		if !strings.EqualFold(*environment.Name, names[i]) {
			return nil, fmt.Errorf("environment resolution response names %q where %q was requested", *environment.Name, names[i])
		}
		for field, present := range map[string]bool{
			"required_reviewers":  environment.RequiredReviewers != nil,
			"prevent_self_review": environment.PreventSelfReview != nil,
			"wait_timer_minutes":  environment.WaitTimerMinutes != nil,
			"branch_policy":       environment.BranchPolicy != nil,
			"unsupported_rules":   environment.UnsupportedRules != nil,
			"secret_names":        environment.SecretNames != nil,
		} {
			if !present {
				return nil, fmt.Errorf("environment resolution response for %q omits %s", names[i], field)
			}
		}
		if *environment.WaitTimerMinutes < 0 {
			return nil, fmt.Errorf("environment resolution response contains an invalid wait timer")
		}
		for _, rule := range *environment.UnsupportedRules {
			if strings.TrimSpace(rule) == "" {
				return nil, fmt.Errorf("environment resolution response contains an invalid protection rule")
			}
		}
		for _, secret := range *environment.SecretNames {
			if !environmentSecretNamePattern.MatchString(secret) {
				return nil, fmt.Errorf("environment resolution response contains an invalid secret name")
			}
		}
		snapshot := EnvironmentSnapshot{
			RequiredReviewers: *environment.RequiredReviewers,
			PreventSelfReview: *environment.PreventSelfReview,
			WaitTimerMinutes:  *environment.WaitTimerMinutes,
			BranchPolicy:      *environment.BranchPolicy,
			UnsupportedRules:  append([]string(nil), *environment.UnsupportedRules...),
			SecretNames:       append([]string(nil), *environment.SecretNames...),
		}
		sort.Strings(snapshot.UnsupportedRules)
		sort.Strings(snapshot.SecretNames)
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func environmentResolutionStatusError(status int, retryAfter string, body []byte) error {
	switch status {
	case http.StatusBadRequest:
		if message := errorBodyMessage(body); message != "" {
			return fmt.Errorf("the environment resolution request was rejected: %s", message)
		}
		return fmt.Errorf("the environment resolution request was rejected; confirm the environment exists on the pipeline's GitHub repository and that Buildkite's GitHub App can read the repository's environments and environment secrets")
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the environment resolution request was denied")
	case http.StatusNotFound:
		return errors.New("the Agent API does not offer GitHub environment resolution")
	case http.StatusTooManyRequests:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			return fmt.Errorf("environment resolution requests are rate limited; retry after %s seconds", retryAfter)
		}
		return fmt.Errorf("environment resolution requests are rate limited")
	case http.StatusServiceUnavailable:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			return fmt.Errorf("the environment resolution service is temporarily unavailable; retry after %s seconds", retryAfter)
		}
		return fmt.Errorf("the environment resolution service is temporarily unavailable")
	default:
		return fmt.Errorf("the environment resolution service returned HTTP %d", status)
	}
}

// errorBodyMessage extracts the "message" field from an Agent API JSON error
// body so backend policy rejections stay actionable, bounding and sanitizing
// the untrusted text before it joins a compile error.
func errorBodyMessage(body []byte) string {
	var decoded struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ""
	}
	message := strings.TrimSpace(decoded.Message)
	const limit = 300
	if len(message) > limit {
		message = message[:limit] + "…"
	}
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, message)
}

func agentEnvironmentResolutionURL(endpoint, jobID string) (string, error) {
	if !validBuildkiteJobID(jobID) {
		return "", fmt.Errorf("environment resolution Agent job ID is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || !validCredentialServiceURL(u) {
		return "", fmt.Errorf("safe environment resolution Agent endpoint using HTTPS or loopback HTTP is required")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jobs/" + jobID + "/github-actions/environments"
	u.RawPath = ""
	return u.String(), nil
}
