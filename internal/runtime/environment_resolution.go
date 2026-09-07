package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/agentapi"
)

const (
	// environmentResolutionResponseLimit bounds one environment's share of a
	// successful response. The backend caps each environment's variables at
	// 256 KiB of raw names and values, and JSON escaping expands a byte to at
	// most six, so 2 MiB per environment holds any response it can send.
	environmentResolutionResponseLimit = 2 << 20
	// environmentResolutionErrorLimit bounds an error body, which carries at
	// most a short message.
	environmentResolutionErrorLimit = 64 << 10

	// environmentVariableCountLimit is GitHub's maximum number of variables per
	// environment.
	environmentVariableCountLimit = 100
	// environmentVariableValueLimit is GitHub's 48 KB maximum variable value.
	environmentVariableValueLimit = 48 << 10
	// environmentVariablesByteLimit is the backend's bound on the UTF-8 bytes of
	// one environment's variable names and values combined.
	environmentVariablesByteLimit = 256 << 10
)

var environmentSecretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvironmentSnapshot is the Buildkite backend's fail-closed snapshot of one
// GitHub deployment environment. The backend performs the GitHub reads with
// its own credentials; no GitHub token or secret value reaches the importer.
// Variable values are not secrets, but they are plaintext configuration:
// callers must keep them out of logs and diagnostics.
type EnvironmentSnapshot struct {
	RequiredReviewers bool
	PreventSelfReview bool
	WaitTimerMinutes  int
	BranchPolicy      bool
	UnsupportedRules  []string
	SecretNames       []string
	// Variables are the environment's variables by name, as GitHub spells
	// them. GitHub variable names are case-insensitive and the backend
	// rejects case-colliding names, so at most one key matches any lookup.
	Variables map[string]string
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
	agent      *agentapi.Client
}

// NewAgentEnvironmentResolver validates the Agent connection configuration.
func NewAgentEnvironmentResolver(config AgentEnvironmentResolverConfig) (*AgentEnvironmentResolver, error) {
	client := config.Client
	if client == nil {
		// Resolution waits on the backend's serial GitHub reads for the whole
		// batch, so allow longer than the default Agent API request timeout.
		client = &http.Client{Timeout: 30 * time.Second}
	}
	agent, err := agentapi.New(agentapi.Config{
		Endpoint: config.Endpoint, JobID: config.JobID, JobToken: config.JobToken,
		ClientVersion: config.ClientVersion, HTTPClient: client,
	}, "environment resolution")
	if err != nil {
		return nil, err
	}
	return &AgentEnvironmentResolver{resolveURL: agent.URL("github-actions/environments"), agent: agent}, nil
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
	// Variables are opt-in on the wire so older importers keep their smaller
	// response bound; this importer always needs them to resolve vars.NAME.
	body, err := json.Marshal(struct {
		RepositoryURL    string   `json:"repo_url"`
		EnvironmentNames []string `json:"environment_names"`
		IncludeVariables bool     `json:"include_variables"`
	}{
		RepositoryURL:    "https://github.com/" + repository,
		EnvironmentNames: names,
		IncludeVariables: true,
	})
	if err != nil {
		return nil, fmt.Errorf("encode environment resolution request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create environment resolution request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.agent.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request environment resolution: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	limit := environmentResolutionResponseLimit * len(names)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, environmentResolutionErrorLimit))
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
			Variables         *[]struct {
				Name  *string `json:"name"`
				Value *string `json:"value"`
			} `json:"variables"`
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
			"variables":           environment.Variables != nil,
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
		// Variable errors name the environment and, at most, a variable name.
		// Values never join an error message.
		if len(*environment.Variables) > environmentVariableCountLimit {
			return nil, fmt.Errorf("environment resolution response for %q contains %d variables; GitHub environments hold at most %d", names[i], len(*environment.Variables), environmentVariableCountLimit)
		}
		variables := make(map[string]string, len(*environment.Variables))
		identities := make(map[string]string, len(*environment.Variables))
		total := 0
		for _, variable := range *environment.Variables {
			if variable.Name == nil || variable.Value == nil {
				return nil, fmt.Errorf("environment resolution response for %q omits a variable name or value", names[i])
			}
			if !environmentSecretNamePattern.MatchString(*variable.Name) {
				return nil, fmt.Errorf("environment resolution response for %q contains an invalid variable name", names[i])
			}
			identity := strings.ToUpper(*variable.Name)
			if previous, exists := identities[identity]; exists {
				return nil, fmt.Errorf("environment resolution response for %q repeats variable %q as %q; GitHub variable names are case-insensitive", names[i], previous, *variable.Name)
			}
			identities[identity] = *variable.Name
			if len(*variable.Value) > environmentVariableValueLimit {
				return nil, fmt.Errorf("environment %q variable %q exceeds GitHub's %d-byte value limit", names[i], *variable.Name, environmentVariableValueLimit)
			}
			total += len(*variable.Name) + len(*variable.Value)
			variables[*variable.Name] = *variable.Value
		}
		if total > environmentVariablesByteLimit {
			return nil, fmt.Errorf("environment %q variables exceed %d bytes; remove or shrink environment variables", names[i], environmentVariablesByteLimit)
		}
		snapshot := EnvironmentSnapshot{
			RequiredReviewers: *environment.RequiredReviewers,
			PreventSelfReview: *environment.PreventSelfReview,
			WaitTimerMinutes:  *environment.WaitTimerMinutes,
			BranchPolicy:      *environment.BranchPolicy,
			UnsupportedRules:  append([]string(nil), *environment.UnsupportedRules...),
			SecretNames:       append([]string(nil), *environment.SecretNames...),
			Variables:         variables,
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
