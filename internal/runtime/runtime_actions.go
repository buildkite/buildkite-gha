package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/agentapi"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
)

const actionSourceBudget = 45 * time.Second
const actionSourceResponseLimit = 16 << 10

type actionSourceSelector struct {
	Repository string  `json:"repository"`
	Ref        string  `json:"ref"`
	Path       *string `json:"path"`
}

type actionSourceRequest struct {
	Version    int                    `json:"version"`
	PlanDigest string                 `json:"plan_digest"`
	Selectors  []actionSourceSelector `json:"selectors"`
}

type actionSourceResponse struct {
	Version int `json:"version"`
	Sources []struct {
		Selector     actionSourceSelector `json:"selector"`
		Repository   string               `json:"repository"`
		RepositoryID uint64               `json:"repository_id"`
		OwnerID      uint64               `json:"owner_id"`
		Commit       string               `json:"commit"`
		Download     struct {
			URL    string `json:"url"`
			Format string `json:"format"`
		} `json:"download"`
	} `json:"sources"`
}

// AgentActionSources binds action lookups to one running job and the original
// plan bytes, not the in-memory program prepared from that plan.
type AgentActionSources struct {
	agent      *agentapi.Client
	planDigest string
}

func NewAgentActionSources(config agentapi.Config, planDigest string) (*AgentActionSources, error) {
	agent, err := agentapi.New(config, "action source resolution")
	if err != nil {
		return nil, err
	}
	return &AgentActionSources{agent: agent, planDigest: planDigest}, nil
}

func (c *AgentActionSources) resolve(ctx context.Context, ref source.Reference) (source.Resolved, error) {
	ctx, cancel := context.WithTimeout(ctx, actionSourceBudget)
	defer cancel()
	selector := actionSourceSelector{Repository: ref.Owner + "/" + ref.Repository, Ref: ref.Ref, Path: &ref.Path}
	payload, err := json.Marshal(actionSourceRequest{Version: 1, PlanDigest: c.planDigest, Selectors: []actionSourceSelector{selector}})
	if err != nil {
		return source.Resolved{}, err
	}
	for attempt := 1; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.agent.URL("github-actions/action-sources"), bytes.NewReader(payload))
		if err != nil {
			return source.Resolved{}, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := c.agent.Do(request)
		if err != nil {
			return source.Resolved{}, fmt.Errorf("request action source: %w", err)
		}
		if response.StatusCode == http.StatusOK {
			defer func() { _ = response.Body.Close() }()
			return decodeActionSource(response.Body, selector)
		}
		status := response.StatusCode
		statusErr := fmt.Errorf("action source resolver returned HTTP %d", status)
		if status == http.StatusBadRequest {
			data, err := io.ReadAll(io.LimitReader(response.Body, actionSourceResponseLimit+1))
			_ = response.Body.Close()
			var rejected struct {
				Code string `json:"code"`
			}
			if err == nil && len(data) <= actionSourceResponseLimit && json.Unmarshal(data, &rejected) == nil && rejected.Code == "client_upgrade_required" {
				return source.Resolved{}, fmt.Errorf("%w: upgrade buildkite-gha to a supported version and retry the job", statusErr)
			}
			return source.Resolved{}, statusErr
		}
		_ = response.Body.Close()
		if attempt == 3 || status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
			return source.Resolved{}, statusErr
		}
		delay, valid := boundedRetryAfter(response.Header.Get("Retry-After"), time.Now(), actionSourceBudget)
		if !valid {
			delay = time.Duration(attempt) * time.Second
		}
		deadline, _ := ctx.Deadline()
		if delay >= time.Until(deadline) {
			return source.Resolved{}, statusErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return source.Resolved{}, ctx.Err()
		}
	}
}

func decodeActionSource(body io.Reader, selector actionSourceSelector) (source.Resolved, error) {
	data, err := io.ReadAll(io.LimitReader(body, actionSourceResponseLimit+1))
	if err != nil || len(data) > actionSourceResponseLimit {
		return source.Resolved{}, fmt.Errorf("could not read bounded action source response")
	}
	var decoded actionSourceResponse
	if json.Unmarshal(data, &decoded) != nil || decoded.Version != 1 || len(decoded.Sources) != 1 {
		return source.Resolved{}, fmt.Errorf("invalid version 1 action source response")
	}
	resolved := decoded.Sources[0]
	if resolved.Selector.Repository != selector.Repository || resolved.Selector.Ref != selector.Ref ||
		resolved.Selector.Path == nil || *resolved.Selector.Path != *selector.Path ||
		!strings.EqualFold(resolved.Repository, selector.Repository) || resolved.Commit != selector.Ref ||
		resolved.OwnerID == 0 || resolved.RepositoryID == 0 {
		return source.Resolved{}, fmt.Errorf("action source identity does not match the requested selector")
	}
	ref, err := source.Parse(resolved.Repository + "@" + resolved.Commit)
	if err != nil || resolved.Download.Format != "tar.gz" ||
		resolved.Download.URL != "https://codeload.github.com/"+resolved.Repository+"/tar.gz/"+resolved.Commit {
		return source.Resolved{}, fmt.Errorf("action source requires the exact GitHub codeload tar.gz descriptor")
	}
	// The store constructs this exact URL itself and downloads without credentials,
	// rather than accepting an arbitrary URL from the response.
	return source.Resolved{Reference: ref, Commit: resolved.Commit}, nil
}

func (r *jobRun) prepareRuntimePublicAction(ctx context.Context, job plan.Job) (plan.Job, error) {
	if r.ActionSources == nil || r.Actions == nil {
		return job, fmt.Errorf("runtime-public-v1 requires the Agent action source resolver and archive store")
	}
	index := slices.IndexFunc(job.Program.Job.Steps, func(step program.Step) bool { return step.Invocation != nil })
	invocation := *job.Program.Job.Steps[index].Invocation
	ref, err := source.Parse(invocation.Uses.Source)
	if err != nil {
		return job, err
	}
	repository := strings.ToLower(ref.Owner + "/" + ref.Repository)
	if _, ok := integration.Lookup(integration.Identity{Source: "github", Repository: repository}); ok {
		return job, fmt.Errorf("runtime-public-v1 does not support actions with native adapters or managed services")
	}
	resolved, err := r.ActionSources.resolve(ctx, ref)
	if err != nil {
		return job, err
	}
	material, err := r.Actions.Materialize(ctx, resolved)
	if err != nil {
		return job, fmt.Errorf("materialize action source: %w", err)
	}
	defer material.Release()
	m, err := metadata.Load(material.RepositoryRoot, "")
	if err != nil {
		return job, fmt.Errorf("load action metadata: %w", err)
	}
	if m.Runs.Using != string(metadata.RuntimeNode24) || m.Runs.Pre != "" || m.Runs.PreIf != "" || m.Runs.Post != "" || m.Runs.PostIf != "" {
		return job, fmt.Errorf("runtime-public-v1 requires a Node 24 action with main only")
	}
	if err := m.ValidateEntrypoints(metadata.RuntimeNode24); err != nil {
		return job, err
	}

	// This ID is local to the prepared program. It never enters the plan bytes
	// or the producer digest, and there is only one action in this mode.
	const lockID = "a-0000000000000001"
	prepared := *job.Program
	prepared.Job.Steps = slices.Clone(job.Program.Job.Steps)
	invocation.Lock = lockID
	prepared.Job.Steps[index].Invocation = &invocation
	prepared.Actions = map[string]program.Action{lockID: program.ActionFromMetadata(m, string(metadata.RuntimeNode24), nil)}
	job.ActionResolution = ""
	job.Program = &prepared
	job.Actions = []plan.ActionLock{{ID: lockID, Source: "github", Repository: repository, Commit: resolved.Commit, RequestedRef: ref.Ref, SourceDigest: material.SourceDigest}}
	if err := job.Validate(); err != nil {
		return job, err
	}
	authority, err := program.InventoryActionAuthority(prepared.Actions, lockID, invocation.With, program.ActionAuthorityOptions{ServerURL: "https://github.com"})
	if err != nil {
		return job, err
	}
	if authority.GitHubToken || len(authority.Secrets) != 0 || authority.EventPayload && job.Event.Payload == nil {
		return job, fmt.Errorf("action metadata requires authority outside the admitted job envelope")
	}
	return job, nil
}
