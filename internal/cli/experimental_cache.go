package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	ghacache "github.com/buildkite/buildkite-gha/internal/cache"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

var errExperimentalCacheBackend = errors.New("experimental cache backend unavailable")

// experimentalDirectoryCacheConfig derives compatibility-only cache identity
// from an immutable plan and the current Buildkite job environment. This is not
// an authorization boundary: mounted cache volumes must only be shared with
// workflows that are trusted to read and modify one another's cache data.
func experimentalDirectoryCacheConfig(job plan.Job, getenv func(string) string) (*gharuntime.CacheConfig, error) {
	root := strings.TrimSpace(getenv("BUILDKITE_GHA_CACHE_DIR"))
	if root == "" {
		return nil, nil
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
		return nil, fmt.Errorf("%w: %v", errExperimentalCacheBackend, err)
	}
	canonicalRepository := strings.ToLower(owner + "/" + name)
	scopeDigest := sha256.Sum256([]byte(canonicalRepository + "\x00" + job.Event.Ref))
	scope := ghacache.Scope("github-ref:sha256:" + hex.EncodeToString(scopeDigest[:]))
	return &gharuntime.CacheConfig{
		Backend: backend, Namespace: namespace,
		ReadScopes: []ghacache.Scope{scope}, WriteScope: scope,
	}, nil
}
