package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	ghacache "github.com/buildkite/buildkite-gha/internal/cache"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

var errCacheBackendUnavailable = errors.New("cache backend unavailable")

func jobCacheConfig(ctx context.Context, job plan.Job, getenv func(string) string, client *http.Client) (*gharuntime.CacheConfig, error) {
	switch backend := strings.TrimSpace(getenv("BUILDKITE_GHA_CACHE_BACKEND")); backend {
	case "", "directory":
		return experimentalDirectoryCacheConfig(job, getenv)
	case "agent":
		hasActions := false
		for _, step := range job.Steps {
			if step.Kind == "uses" {
				hasActions = true
				break
			}
		}
		if !hasActions {
			return nil, nil
		}
		jobID := strings.TrimSpace(getenv("BUILDKITE_JOB_ID"))
		agentBackend, capability, err := ghacache.NewAgentBackend(ctx, ghacache.AgentConfig{
			Endpoint: strings.TrimSpace(getenv("BUILDKITE_AGENT_ENDPOINT")),
			JobID:    jobID, Token: strings.TrimSpace(getenv("BUILDKITE_AGENT_ACCESS_TOKEN")), Client: client,
		})
		if err != nil {
			// A missing capability route means the independent backend has not
			// reached this Agent deployment yet. Treat it, rate limiting, and
			// transient failures as cache unavailability; local identity or
			// credential configuration errors and authorization denials remain fatal.
			if errors.Is(err, ghacache.ErrNotFound) || errors.Is(err, ghacache.ErrRateLimit) || errors.Is(err, ghacache.ErrUnavailable) {
				return nil, fmt.Errorf("%w: %v", errCacheBackendUnavailable, err)
			}
			return nil, fmt.Errorf("configure job-authenticated cache: %w", err)
		}
		if !capability.Enabled {
			return nil, nil
		}
		placeholder := "server-derived"
		scope := ghacache.Scope(placeholder)
		return &gharuntime.CacheConfig{
			Backend: agentBackend, Namespace: ghacache.Namespace{Organization: placeholder, Cluster: placeholder, Pipeline: jobID},
			ReadScopes: []ghacache.Scope{scope}, WriteScope: scope,
			ReadOnly:   capability.Mode == "read-only",
			MaxArchive: capability.Limits.MaxArchiveSize, MaxCandidates: capability.Limits.MaxCandidates,
			MaxKey: capability.Limits.MaxKeyBytes, MaxVersion: capability.Limits.MaxVersionBytes,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported BUILDKITE_GHA_CACHE_BACKEND %q", backend)
	}
}

// experimentalDirectoryCacheConfig derives compatibility-only cache identity
// from an immutable plan and the current Buildkite job environment. This is not
// an authorization boundary: mounted cache volumes must only be shared with
// workflows that are trusted to read and modify one another's cache data.
func experimentalDirectoryCacheConfig(job plan.Job, getenv func(string) string) (*gharuntime.CacheConfig, error) {
	root := strings.TrimSpace(getenv("BUILDKITE_GHA_CACHE_DIR"))
	if root == "" {
		return nil, nil
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve cache directory: %v", errCacheBackendUnavailable, err)
	}
	if job.Event.Provider != "github" || job.Event.Repository == "" || job.Event.Ref == "" {
		return nil, fmt.Errorf("experimental cache requires a GitHub repository and ref in the job plan")
	}
	owner, name, _, err := parsePublicGitHubRepository("https://github.com/" + job.Event.Repository)
	if err != nil || !strings.EqualFold(job.Event.Repository, owner+"/"+name) {
		return nil, fmt.Errorf("experimental cache requires a canonical GitHub owner/repository in the job plan")
	}
	namespace := ghacache.Namespace{
		Organization: strings.TrimSpace(getenv("BUILDKITE_ORGANIZATION_ID")),
		Cluster:      strings.TrimSpace(getenv("BUILDKITE_AGENT_META_DATA_QUEUE")),
		Pipeline:     strings.TrimSpace(getenv("BUILDKITE_PIPELINE_ID")),
	}
	if namespace.Organization == "" || namespace.Cluster == "" || namespace.Pipeline == "" {
		return nil, fmt.Errorf("experimental cache requires BUILDKITE_ORGANIZATION_ID, BUILDKITE_PIPELINE_ID, and BUILDKITE_AGENT_META_DATA_QUEUE")
	}
	backend, err := ghacache.NewExperimentalDirectoryBackend(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCacheBackendUnavailable, err)
	}
	canonicalRepository := strings.ToLower(owner + "/" + name)
	scopeDigest := sha256.Sum256([]byte(canonicalRepository + "\x00" + job.Event.Ref))
	scope := ghacache.Scope("github-ref:sha256:" + hex.EncodeToString(scopeDigest[:]))
	return &gharuntime.CacheConfig{
		Backend: backend, Namespace: namespace,
		ReadScopes: []ghacache.Scope{scope}, WriteScope: scope,
	}, nil
}
