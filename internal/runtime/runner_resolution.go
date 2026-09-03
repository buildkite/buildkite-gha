package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/agentapi"
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

// RunnerRejection is the Agent API's refusal to resolve one requirement. Code
// is an open set owned by the server; callers must tolerate codes they do not
// recognize and render Message, which the server keeps free of runner labels.
type RunnerRejection struct {
	ID      string
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
	agent      *agentapi.Client
}

func NewAgentRunnerResolver(config AgentRunnerResolverConfig) (*AgentRunnerResolver, error) {
	agent, err := agentapi.New(agentapi.Config{
		Endpoint: config.Endpoint, JobID: config.JobID, JobToken: config.JobToken,
		ClientVersion: config.ClientVersion, HTTPClient: config.Client,
	}, "runner resolution")
	if err != nil {
		return nil, err
	}
	return &AgentRunnerResolver{
		resolveURL: agent.URL("github-actions/runners"), agent: agent,
	}, nil
}

// Resolve returns the server's target for every requirement it accepted and
// its rejection for every requirement it refused. Each requirement appears in
// exactly one of the two results.
func (c *AgentRunnerResolver) Resolve(ctx context.Context, requirements []RunnerRequirement) ([]RunnerSuggestion, []RunnerRejection, error) {
	if c == nil {
		return nil, nil, fmt.Errorf("runner resolver is not configured")
	}
	var suggestions []RunnerSuggestion
	var rejections []RunnerRejection
	for start := 0; start < len(requirements); start += runnerResolutionBatchLimit {
		end := min(start+runnerResolutionBatchLimit, len(requirements))
		resolved, rejected, err := c.resolveBatch(ctx, requirements[start:end])
		if err != nil {
			return nil, nil, err
		}
		suggestions = append(suggestions, resolved...)
		rejections = append(rejections, rejected...)
	}
	return suggestions, rejections, nil
}

func (c *AgentRunnerResolver) resolveBatch(ctx context.Context, requirements []RunnerRequirement) ([]RunnerSuggestion, []RunnerRejection, error) {
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
			return nil, nil, fmt.Errorf("runner requirements require unique non-empty IDs")
		}
		expected[input.ID] = true
		body.Requirements[i] = requirement{ID: input.ID, Selector: selector{Labels: input.Labels}}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("encode runner resolution request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, fmt.Errorf("create runner resolution request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.agent.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("request runner resolution: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, runnerResolutionResponseLimit))
		return nil, nil, fmt.Errorf("runner resolution service returned HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, runnerResolutionResponseLimit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read runner resolution response: %w", err)
	}
	if len(payload) > runnerResolutionResponseLimit {
		return nil, nil, fmt.Errorf("runner resolution response exceeds the %d-byte limit", runnerResolutionResponseLimit)
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
		return nil, nil, fmt.Errorf("decode runner resolution response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("runner resolution response has trailing data")
	}
	if len(decoded.Resolutions) != len(requirements) {
		return nil, nil, fmt.Errorf("runner resolution response has an unexpected number of resolutions")
	}
	seen := make(map[string]bool, len(decoded.Resolutions))
	suggestions := make([]RunnerSuggestion, 0, len(decoded.Resolutions))
	var rejections []RunnerRejection
	for _, resolution := range decoded.Resolutions {
		hasTarget := resolution.Target != nil
		hasError := resolution.Error != nil
		if !expected[resolution.ID] || seen[resolution.ID] || hasTarget == hasError {
			return nil, nil, fmt.Errorf("runner resolution response is invalid")
		}
		seen[resolution.ID] = true
		if resolution.Target != nil {
			suggestion := RunnerSuggestion{ID: resolution.ID, Queue: resolution.Target.Queue, Platform: resolution.Target.Platform, Image: resolution.Target.Image}
			for _, warning := range resolution.Warnings {
				if strings.TrimSpace(warning.Code) == "" || strings.TrimSpace(warning.Message) == "" {
					return nil, nil, fmt.Errorf("runner resolution response contains an invalid warning")
				}
				suggestion.Warnings = append(suggestion.Warnings, RunnerWarning{Code: warning.Code, Message: warning.Message})
			}
			suggestions = append(suggestions, suggestion)
			continue
		}
		if len(resolution.Warnings) != 0 {
			return nil, nil, fmt.Errorf("runner resolution response contains warnings without a target")
		}
		if strings.TrimSpace(resolution.Error.Code) == "" || strings.TrimSpace(resolution.Error.Message) == "" {
			return nil, nil, fmt.Errorf("runner resolution response contains an invalid error")
		}
		rejections = append(rejections, RunnerRejection{ID: resolution.ID, Code: resolution.Error.Code, Message: resolution.Error.Message})
	}
	return suggestions, rejections, nil
}
