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
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/useragent"
)

const runnerResolutionResponseLimit = 64 << 10
const runnerResolutionBatchLimit = 100

type RunnerRequirement struct {
	ID     string
	Labels []string
}

type RunnerSuggestion struct {
	ID       string
	Queue    string
	Platform string
	Image    string
	Warnings []RunnerWarning
}

type RunnerWarning struct {
	Code    string
	Message string
}

type AgentRunnerResolverConfig struct {
	Endpoint      string
	JobID         string
	JobToken      string
	ClientVersion string
	Client        *http.Client
}

type AgentRunnerResolver struct {
	resolveURL string
	jobToken   string
	userAgent  string
	client     *http.Client
}

func NewAgentRunnerResolver(config AgentRunnerResolverConfig) (*AgentRunnerResolver, error) {
	resolveURL, err := agentRunnerResolutionURL(config.Endpoint, config.JobID)
	if err != nil {
		return nil, err
	}
	if config.JobToken == "" || strings.ContainsAny(config.JobToken, "\r\n") {
		return nil, fmt.Errorf("runner resolution Agent job token is required")
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
	return &AgentRunnerResolver{
		resolveURL: resolveURL, jobToken: config.JobToken,
		userAgent: useragent.FromVersion(config.ClientVersion), client: &bounded,
	}, nil
}

func (c *AgentRunnerResolver) Resolve(ctx context.Context, requirements []RunnerRequirement) ([]RunnerSuggestion, error) {
	if c == nil {
		return nil, fmt.Errorf("runner resolver is not configured")
	}
	var suggestions []RunnerSuggestion
	for start := 0; start < len(requirements); start += runnerResolutionBatchLimit {
		end := min(start+runnerResolutionBatchLimit, len(requirements))
		resolved, err := c.resolveBatch(ctx, requirements[start:end])
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, resolved...)
	}
	return suggestions, nil
}

func (c *AgentRunnerResolver) resolveBatch(ctx context.Context, requirements []RunnerRequirement) ([]RunnerSuggestion, error) {
	type selector struct {
		Labels []string `json:"labels"`
	}
	type requirement struct {
		ID       string   `json:"id"`
		Selector selector `json:"selector"`
	}
	body := struct {
		Requirements []requirement `json:"requirements"`
	}{Requirements: make([]requirement, len(requirements))}
	expected := make(map[string]bool, len(requirements))
	for i, input := range requirements {
		if input.ID == "" || expected[input.ID] {
			return nil, fmt.Errorf("runner requirements require unique non-empty IDs")
		}
		expected[input.ID] = true
		body.Requirements[i] = requirement{ID: input.ID, Selector: selector{Labels: input.Labels}}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode runner resolution request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("create runner resolution request: %w", err)
	}
	request.Header.Set("Authorization", "Token "+c.jobToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request runner resolution: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, runnerResolutionResponseLimit))
		return nil, fmt.Errorf("runner resolution service returned HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, runnerResolutionResponseLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read runner resolution response: %w", err)
	}
	if len(payload) > runnerResolutionResponseLimit {
		return nil, fmt.Errorf("runner resolution response exceeds the %d-byte limit", runnerResolutionResponseLimit)
	}
	var decoded struct {
		Resolutions []struct {
			ID     string `json:"id"`
			Target *struct {
				Queue    string `json:"queue"`
				Platform string `json:"platform"`
				Image    string `json:"image"`
			} `json:"target"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Warnings []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"warnings"`
		} `json:"resolutions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode runner resolution response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("runner resolution response has trailing data")
	}
	if len(decoded.Resolutions) != len(requirements) {
		return nil, fmt.Errorf("runner resolution response has an unexpected number of resolutions")
	}
	seen := make(map[string]bool, len(decoded.Resolutions))
	suggestions := make([]RunnerSuggestion, 0, len(decoded.Resolutions))
	for _, resolution := range decoded.Resolutions {
		hasTarget := resolution.Target != nil
		hasError := resolution.Error != nil
		if !expected[resolution.ID] || seen[resolution.ID] || hasTarget == hasError {
			return nil, fmt.Errorf("runner resolution response is invalid")
		}
		seen[resolution.ID] = true
		if resolution.Target != nil {
			suggestion := RunnerSuggestion{ID: resolution.ID, Queue: resolution.Target.Queue, Platform: resolution.Target.Platform, Image: resolution.Target.Image}
			for _, warning := range resolution.Warnings {
				if strings.TrimSpace(warning.Code) == "" || strings.TrimSpace(warning.Message) == "" {
					return nil, fmt.Errorf("runner resolution response contains an invalid warning")
				}
				suggestion.Warnings = append(suggestion.Warnings, RunnerWarning{Code: warning.Code, Message: warning.Message})
			}
			suggestions = append(suggestions, suggestion)
		} else if len(resolution.Warnings) != 0 {
			return nil, fmt.Errorf("runner resolution response contains warnings without a target")
		}
	}
	return suggestions, nil
}

func agentRunnerResolutionURL(endpoint, jobID string) (string, error) {
	if !validBuildkiteJobID(jobID) {
		return "", fmt.Errorf("runner resolution Agent job ID is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || !validCredentialServiceURL(u) {
		return "", fmt.Errorf("safe runner resolution Agent endpoint using HTTPS or loopback HTTP is required")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jobs/" + jobID + "/github-actions/runners"
	u.RawPath = ""
	return u.String(), nil
}
