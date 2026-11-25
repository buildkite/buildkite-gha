package runtime

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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

func (c *AgentActionSources) resolve(ctx context.Context, version int, selectors []actionSourceSelector) (map[[2]string]source.Resolved, error) {
	ctx, cancel := context.WithTimeout(ctx, actionSourceBudget)
	defer cancel()
	payload, err := json.Marshal(actionSourceRequest{Version: version, PlanDigest: c.planDigest, Selectors: selectors})
	if err != nil {
		return nil, err
	}
	if version == 2 && len(payload) > 8<<10 {
		return nil, fmt.Errorf("version 2 action source request exceeds 8 KiB")
	}
	for attempt := 1; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.agent.URL("github-actions/action-sources"), bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := c.agent.Do(request)
		if err != nil {
			return nil, fmt.Errorf("request action source: %w", err)
		}
		if response.StatusCode == http.StatusOK {
			defer func() { _ = response.Body.Close() }()
			return decodeActionSources(response.Body, version, selectors)
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
				return nil, fmt.Errorf("%w: upgrade buildkite-gha to a supported version and retry the job", statusErr)
			}
			return nil, statusErr
		}
		_ = response.Body.Close()
		if attempt == 3 || status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
			return nil, statusErr
		}
		delay, valid := boundedRetryAfter(response.Header.Get("Retry-After"), time.Now(), actionSourceBudget)
		if !valid {
			delay = time.Duration(attempt) * time.Second
		}
		deadline, _ := ctx.Deadline()
		if delay >= time.Until(deadline) {
			return nil, statusErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

func decodeActionSources(body io.Reader, version int, selectors []actionSourceSelector) (map[[2]string]source.Resolved, error) {
	limit := actionSourceResponseLimit
	if version == 2 {
		limit = 32 << 10
	}
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil || len(data) > limit {
		return nil, fmt.Errorf("could not read bounded action source response")
	}
	var decoded actionSourceResponse
	if json.Unmarshal(data, &decoded) != nil || decoded.Version != version || len(decoded.Sources) != len(selectors) {
		return nil, fmt.Errorf("invalid version %d action source response", version)
	}
	missing := make(map[[2]string]struct{}, len(selectors))
	for _, selector := range selectors {
		missing[[2]string{selector.Repository, selector.Ref}] = struct{}{}
	}
	results := make(map[[2]string]source.Resolved, len(selectors))
	for _, resolved := range decoded.Sources {
		selector := resolved.Selector
		key := [2]string{selector.Repository, selector.Ref}
		if _, ok := missing[key]; !ok || selector.Path == nil || *selector.Path != "" ||
			!strings.EqualFold(resolved.Repository, selector.Repository) || resolved.Commit != selector.Ref ||
			resolved.OwnerID == 0 || resolved.RepositoryID == 0 {
			return nil, fmt.Errorf("action source identity does not match the requested selector")
		}
		delete(missing, key)
		ref, err := source.Parse(resolved.Repository + "@" + resolved.Commit)
		if err != nil || resolved.Download.Format != "tar.gz" ||
			resolved.Download.URL != "https://codeload.github.com/"+resolved.Repository+"/tar.gz/"+resolved.Commit {
			return nil, fmt.Errorf("action source requires the exact GitHub codeload tar.gz descriptor")
		}
		// The store constructs this exact URL and downloads without credentials.
		results[key] = source.Resolved{Reference: ref, Commit: resolved.Commit}
	}
	return results, nil
}

func (r *jobRun) prepareRuntimePublicAction(ctx context.Context, job plan.Job) (plan.Job, error) {
	if r.ActionSources == nil || r.Actions == nil {
		return job, fmt.Errorf("runtime public actions require the Agent action source resolver and archive store")
	}
	version := 1
	if job.ActionResolution == plan.RuntimePublicActionsV2 {
		version = 2
	}
	var selectors []actionSourceSelector
	steps := make(map[int][2]string)
	locks := make(map[[2]string]string)
	for i, step := range job.Program.Job.Steps {
		if step.Invocation == nil || step.Invocation.Lock != "" {
			continue
		}
		ref, err := source.Parse(step.Invocation.Uses.Source)
		if err != nil {
			return job, err
		}
		repository := ref.Owner + "/" + ref.Repository
		if _, ok := integration.Lookup(integration.Identity{Source: "github", Repository: strings.ToLower(repository)}); ok {
			return job, fmt.Errorf("runtime public actions do not support resolving native adapters or managed services")
		}
		key := [2]string{repository, ref.Ref}
		steps[i] = key
		if _, ok := locks[key]; !ok {
			selectors = append(selectors, actionSourceSelector{Repository: repository, Ref: ref.Ref, Path: &ref.Path})
			locks[key] = ""
		}
	}
	resolved, err := r.ActionSources.resolve(ctx, version, selectors)
	if err != nil {
		return job, err
	}
	prepared := *job.Program
	prepared.Job.Steps = slices.Clone(job.Program.Job.Steps)
	prepared.Actions = make(map[string]program.Action, len(job.Program.Actions)+len(selectors))
	maps.Copy(prepared.Actions, job.Program.Actions)
	job.Actions = slices.Clone(job.Actions)
	nextID := 1
	for _, selector := range selectors {
		key := [2]string{selector.Repository, selector.Ref}
		lockID := fmt.Sprintf("a-%016x", nextID)
		for slices.ContainsFunc(job.Actions, func(lock plan.ActionLock) bool { return lock.ID == lockID }) {
			nextID++
			lockID = fmt.Sprintf("a-%016x", nextID)
		}
		nextID++
		lock, action, err := r.preparePublicActionSource(ctx, version, lockID, resolved[key])
		if err != nil {
			return job, fmt.Errorf("prepare %s@%s: %w", selector.Repository, selector.Ref, err)
		}
		job.Actions = append(job.Actions, lock)
		prepared.Actions[lockID] = action
		locks[key] = lockID
	}
	for i, step := range prepared.Job.Steps {
		key, ok := steps[i]
		if !ok {
			continue
		}
		invocation := *step.Invocation
		invocation.Lock = locks[key]
		prepared.Job.Steps[i].Invocation = &invocation
		authority, err := program.InventoryActionAuthority(prepared.Actions, invocation.Lock, invocation.With, program.ActionAuthorityOptions{ServerURL: "https://github.com"})
		if err != nil {
			return job, err
		}
		if authority.GitHubToken || len(authority.Secrets) != 0 || authority.EventPayload && job.Event.Payload == nil {
			return job, fmt.Errorf("action metadata requires authority outside the admitted job envelope")
		}
	}
	job.ActionResolution = ""
	job.Program = &prepared
	slices.SortFunc(job.Actions, func(a, b plan.ActionLock) int { return cmp.Compare(a.ID, b.ID) })
	return job, job.Validate()
}

func (r *jobRun) preparePublicActionSource(ctx context.Context, version int, lockID string, resolved source.Resolved) (plan.ActionLock, program.Action, error) {
	material, err := r.Actions.Materialize(ctx, resolved)
	if err != nil {
		return plan.ActionLock{}, program.Action{}, fmt.Errorf("materialize action source: %w", err)
	}
	defer material.Release()
	m, err := metadata.Load(material.RepositoryRoot, "")
	if err != nil {
		return plan.ActionLock{}, program.Action{}, fmt.Errorf("load action metadata: %w", err)
	}
	if version == 1 && (m.Runs.Using != string(metadata.RuntimeNode24) || m.Runs.Pre != "" || m.Runs.PreIf != "" || m.Runs.Post != "" || m.Runs.PostIf != "") {
		return plan.ActionLock{}, program.Action{}, fmt.Errorf("runtime-public-v1 requires a Node 24 action with main only")
	}
	runtime, err := m.Runtime()
	if err != nil {
		return plan.ActionLock{}, program.Action{}, err
	}
	if _, ok := actionNodeMajor(runtime); !ok {
		return plan.ActionLock{}, program.Action{}, fmt.Errorf("runtime public actions require JavaScript metadata, got %q", runtime)
	}
	if err := m.ValidateEntrypoints(runtime); err != nil {
		return plan.ActionLock{}, program.Action{}, err
	}
	repository := strings.ToLower(resolved.Reference.Owner + "/" + resolved.Reference.Repository)
	return plan.ActionLock{ID: lockID, Source: "github", Repository: repository, Commit: resolved.Commit, RequestedRef: resolved.Commit, SourceDigest: material.SourceDigest},
		program.ActionFromMetadata(m, string(runtime), nil), nil
}
