package source

import (
	"cmp"
	"context"
	"slices"
	"sync"
)

type githubAPIStatsKey struct{}

type sourceCacheKind uint8

const (
	cacheMutableRef sourceCacheKind = iota
	cacheSnapshotResolved
	cacheSnapshotMissing
	cachePublicRepositoryCheck
	cacheCompilationMissingRef
	sourceCacheKindCount
)

var sourceCacheNames = [...]string{
	"mutable_ref_disk",
	"snapshot_resolved",
	"snapshot_missing",
	"public_repository_check",
	"compilation_missing_ref",
}

type githubAPIRequestOutcome uint8

const (
	apiOutcomeResponse githubAPIRequestOutcome = iota
	apiOutcomeTransportError
	apiOutcomeCanceled
	apiOutcomeReadError
	apiOutcomeDecodeError
)

var sourceAPIOutcomeNames = [...]string{"response", "transport_error", "canceled", "read_error", "decode_error"}

type githubAPIRequestKey struct {
	authentication string
	endpoint       string
	status         int
	outcome        githubAPIRequestOutcome
}

type githubAPISuppressionKey struct {
	authentication string
	endpoint       string
}

// GitHubAPIStats counts this context's source REST lookups. It does not observe
// codeload downloads, Git commands, runtime actions, or GitHub quota billing.
// Its keys contain only fixed categories and bounded HTTP status codes, never
// repository names, references, URLs, credentials, or response headers.
type GitHubAPIStats struct {
	mu         sync.Mutex
	attempts   uint64
	responses  uint64
	requests   map[githubAPIRequestKey]uint64
	cacheHits  [sourceCacheKindCount]uint64
	suppressed map[githubAPISuppressionKey]uint64
}

// GitHubAPIStatsSummary is a concurrency-safe snapshot of observed requests.
// Status zero means no valid HTTP status was received. CacheHits includes only
// the named source-resolution caches, not compiler graphs or source trees.
type GitHubAPIStatsSummary struct {
	Scope         string                     `json:"scope"`
	HTTPAttempts  uint64                     `json:"http_attempts"`
	HTTPResponses uint64                     `json:"http_responses"`
	Requests      []GitHubAPIRequestCount    `json:"requests"`
	CacheHits     []SourceCacheHitCount      `json:"cache_hits"`
	Suppressed    []GitHubAPISuppressedCount `json:"suppressed"`
}

// GitHubAPIRequestCount groups completed HTTP attempts by bounded categories.
type GitHubAPIRequestCount struct {
	Authentication string `json:"authentication"`
	Endpoint       string `json:"endpoint"`
	Status         int    `json:"status"`
	Outcome        string `json:"outcome"`
	Count          uint64 `json:"count"`
}

// SourceCacheHitCount identifies a source-resolution cache that reused a result.
type SourceCacheHitCount struct {
	Cache string `json:"cache"`
	Count uint64 `json:"count"`
}

// GitHubAPISuppressedCount counts rate-limit cooldown checks that prevented an
// HTTP attempt. These counts are not requests sent to GitHub.
type GitHubAPISuppressedCount struct {
	Authentication string `json:"authentication"`
	Endpoint       string `json:"endpoint"`
	Reason         string `json:"reason"`
	Count          uint64 `json:"count"`
}

// WithGitHubAPIStats starts a fresh collection scope, replacing any parent
// collection. All descendant contexts share its concurrency-safe counters.
func WithGitHubAPIStats(ctx context.Context) (context.Context, *GitHubAPIStats) {
	stats := &GitHubAPIStats{
		requests:   make(map[githubAPIRequestKey]uint64),
		suppressed: make(map[githubAPISuppressionKey]uint64),
	}
	return context.WithValue(ctx, githubAPIStatsKey{}, stats), stats
}

func githubAPIStatsFromContext(ctx context.Context) *GitHubAPIStats {
	stats, _ := ctx.Value(githubAPIStatsKey{}).(*GitHubAPIStats)
	return stats
}

// Snapshot returns an independent, deterministically ordered summary. A live
// snapshot can include attempts that have not completed yet.
func (s *GitHubAPIStats) Snapshot() GitHubAPIStatsSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := GitHubAPIStatsSummary{
		Scope: "github_source_rest", HTTPAttempts: s.attempts, HTTPResponses: s.responses,
		Requests:   make([]GitHubAPIRequestCount, 0, len(s.requests)),
		CacheHits:  make([]SourceCacheHitCount, 0),
		Suppressed: make([]GitHubAPISuppressedCount, 0, len(s.suppressed)),
	}
	for key, count := range s.requests {
		result.Requests = append(result.Requests, GitHubAPIRequestCount{
			Authentication: key.authentication, Endpoint: key.endpoint,
			Status: key.status, Outcome: sourceAPIOutcomeNames[key.outcome], Count: count,
		})
	}
	slices.SortFunc(result.Requests, func(a, b GitHubAPIRequestCount) int {
		return cmp.Or(cmp.Compare(a.Authentication, b.Authentication), cmp.Compare(a.Endpoint, b.Endpoint), cmp.Compare(a.Status, b.Status), cmp.Compare(a.Outcome, b.Outcome))
	})
	for kind, count := range s.cacheHits {
		if count != 0 {
			result.CacheHits = append(result.CacheHits, SourceCacheHitCount{Cache: sourceCacheNames[kind], Count: count})
		}
	}
	for key, count := range s.suppressed {
		result.Suppressed = append(result.Suppressed, GitHubAPISuppressedCount{
			Authentication: key.authentication, Endpoint: key.endpoint, Reason: "rate_limit", Count: count,
		})
	}
	slices.SortFunc(result.Suppressed, func(a, b GitHubAPISuppressedCount) int {
		return cmp.Or(cmp.Compare(a.Authentication, b.Authentication), cmp.Compare(a.Endpoint, b.Endpoint))
	})
	return result
}

func sourceAPIAuthentication(authenticated bool) string {
	if authenticated {
		return "authenticated"
	}
	return "anonymous"
}

func sourceAPIEndpoint(parts []string) string {
	if len(parts) >= 3 && parts[0] == "repos" {
		switch {
		case len(parts) == 3:
			return "repository"
		case len(parts) == 7 && parts[3] == "git" && parts[4] == "ref" && parts[5] == "tags":
			return "tag_ref"
		case len(parts) == 7 && parts[3] == "git" && parts[4] == "ref" && parts[5] == "heads":
			return "branch_ref"
		case len(parts) == 5 && parts[3] == "commits":
			return "commit"
		case len(parts) == 6 && parts[3] == "git" && parts[4] == "tags":
			return "annotated_tag"
		}
	}
	return "other"
}

type githubAPIRequestObservation struct {
	stats *GitHubAPIStats
	key   githubAPIRequestKey
}

func beginGitHubAPIRequest(ctx context.Context, authenticated bool, parts []string) githubAPIRequestObservation {
	stats := githubAPIStatsFromContext(ctx)
	if stats == nil {
		return githubAPIRequestObservation{}
	}
	stats.mu.Lock()
	stats.attempts++
	stats.mu.Unlock()
	return githubAPIRequestObservation{stats: stats, key: githubAPIRequestKey{
		authentication: sourceAPIAuthentication(authenticated), endpoint: sourceAPIEndpoint(parts),
	}}
}

func (o githubAPIRequestObservation) finish(status int, outcome githubAPIRequestOutcome) {
	if o.stats == nil {
		return
	}
	key := o.key
	if status >= 100 && status <= 599 {
		key.status = status
	}
	key.outcome = outcome
	o.stats.mu.Lock()
	if status != 0 {
		o.stats.responses++
	}
	o.stats.requests[key]++
	o.stats.mu.Unlock()
}

func recordSourceCacheHit(ctx context.Context, kind sourceCacheKind) {
	stats := githubAPIStatsFromContext(ctx)
	if stats == nil || kind >= sourceCacheKindCount {
		return
	}
	stats.mu.Lock()
	stats.cacheHits[kind]++
	stats.mu.Unlock()
}

func recordGitHubAPISuppression(ctx context.Context, authenticated bool, parts []string) {
	stats := githubAPIStatsFromContext(ctx)
	if stats == nil {
		return
	}
	key := githubAPISuppressionKey{authentication: sourceAPIAuthentication(authenticated), endpoint: sourceAPIEndpoint(parts)}
	stats.mu.Lock()
	stats.suppressed[key]++
	stats.mu.Unlock()
}
