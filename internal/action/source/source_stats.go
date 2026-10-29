package source

import (
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

type sourceStatsKey struct{}
type sourceRepositoryRootKey struct{}

// SourceStats collects bounded command telemetry separately from the opt-in
// REST diagnostic. No repository identities, URLs, tokens, or error text enter it.
type SourceStats struct {
	mu                                    sync.Mutex
	requests                              map[sourceRequestKey]SourceRequestStats
	cacheHits                             map[string]uint64
	materializations, suppressed, dropped uint64
}

type sourceRequestKey struct {
	kind, operation, authentication, result string
	status                                  int
}

type SourceStatsSummary struct {
	Requests           []SourceRequestStats `json:"requests"`
	CacheHits          map[string]uint64    `json:"cache_hits"`
	Materializations   uint64               `json:"materializations"`
	SuppressedRequests uint64               `json:"suppressed_requests"`
	DroppedRequests    uint64               `json:"dropped_requests"`
}

// Bytes counts response-body bytes consumed, not Content-Length or wire traffic.
// Duration includes body consumption. RetryAfterSeconds is the largest observed
// Retry-After, capped at one day; it is not an inferred primary quota reset.
// RateLimitRemaining is the minimum known remaining quota (-1 means unknown);
// RateLimitReset is the latest observed reset timestamp (0 means unknown).
type SourceRequestStats struct {
	Kind               string `json:"kind"`
	Operation          string `json:"operation"`
	Authentication     string `json:"authentication"`
	Status             int    `json:"status"`
	Result             string `json:"result"`
	Count              uint64 `json:"count"`
	Bytes              int64  `json:"bytes"`
	DurationMS         int64  `json:"duration_ms"`
	RetryAfterSeconds  int64  `json:"retry_after_seconds"`
	RateLimitRemaining int64  `json:"rate_limit_remaining"`
	RateLimitReset     int64  `json:"rate_limit_reset"`
}

func WithSourceStats(ctx context.Context) (context.Context, *SourceStats) {
	s := &SourceStats{requests: make(map[sourceRequestKey]SourceRequestStats), cacheHits: make(map[string]uint64)}
	return context.WithValue(ctx, sourceStatsKey{}, s), s
}

func sourceStatsFromContext(ctx context.Context) *SourceStats {
	s, _ := ctx.Value(sourceStatsKey{}).(*SourceStats)
	return s
}

func SourceStatsSnapshot(ctx context.Context) *SourceStatsSummary {
	if s := sourceStatsFromContext(ctx); s != nil {
		summary := s.Snapshot()
		return &summary
	}
	return nil
}

func (s *SourceStats) Snapshot() SourceStatsSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := SourceStatsSummary{
		Requests: make([]SourceRequestStats, 0, len(s.requests)), CacheHits: maps.Clone(s.cacheHits),
		Materializations: s.materializations, SuppressedRequests: s.suppressed, DroppedRequests: s.dropped,
	}
	for _, request := range s.requests {
		result.Requests = append(result.Requests, request)
	}
	slices.SortFunc(result.Requests, func(a, b SourceRequestStats) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Operation, b.Operation), cmp.Compare(a.Authentication, b.Authentication), cmp.Compare(a.Status, b.Status), cmp.Compare(a.Result, b.Result))
	})
	return result
}

func recordSourceStat(ctx context.Context, name string) {
	if s := sourceStatsFromContext(ctx); s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch name {
		case "materialization":
			s.materializations++
		case "suppressed":
			s.suppressed++
		default:
			s.cacheHits[name]++
		}
	}
}

type sourceRequestObservation struct {
	stats            *SourceStats
	key              sourceRequestKey
	started          time.Time
	body             *sourceCountingBody
	retryAfter       int64
	remaining, reset int64
}

func beginSourceRequest(ctx context.Context, operation string, authenticated bool) *sourceRequestObservation {
	s := sourceStatsFromContext(ctx)
	if s == nil {
		return nil
	}
	kind := "action"
	if root, _ := ctx.Value(sourceRepositoryRootKey{}).(bool); root {
		kind = "repository"
	}
	return &sourceRequestObservation{stats: s, started: time.Now(), remaining: -1, key: sourceRequestKey{kind: kind, operation: operation, authentication: sourceAPIAuthentication(authenticated)}}
}

func (o *sourceRequestObservation) response(resp *http.Response) {
	if o == nil {
		return
	}
	o.key.status = resp.StatusCode
	o.body = &sourceCountingBody{ReadCloser: resp.Body}
	resp.Body = o.body
	value := resp.Header.Get("Retry-After")
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		if deadline, err := http.ParseTime(value); err == nil {
			seconds = int64(time.Until(deadline).Seconds())
		}
	}
	o.retryAfter = min(max(seconds, 0), 86400)
	if remaining, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Remaining"), 10, 64); err == nil && remaining >= 0 && remaining <= 9_007_199_254_740_991 {
		o.remaining = remaining
	}
	if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset >= 0 && reset <= 9_007_199_254_740_991 {
		o.reset = reset
	}
}

func (o *sourceRequestObservation) finish(err error) {
	if o == nil {
		return
	}
	var rate *RateLimitError
	var networkError net.Error
	switch {
	case err == nil:
		o.key.result = "success"
	case errors.As(err, &rate):
		o.key.result = "rate_limit"
	case errors.Is(err, context.Canceled):
		o.key.result = "canceled"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()):
		o.key.result = "timeout"
	case o.key.status == 401 || o.key.status == 403:
		o.key.result = "denied"
	case o.key.status == 404:
		o.key.result = "not_found"
	case o.key.status == 0:
		o.key.result = "transport_error"
	case o.key.status < 200 || o.key.status >= 300:
		o.key.result = "http_error"
	default:
		o.key.result = "invalid_response"
	}
	o.stats.mu.Lock()
	defer o.stats.mu.Unlock()
	row, exists := o.stats.requests[o.key]
	if !exists && len(o.stats.requests) >= 64 {
		o.stats.dropped++
		return
	}
	if !exists {
		row.RateLimitRemaining = -1
	}
	row.Kind = o.key.kind
	row.Operation, row.Authentication, row.Status, row.Result = o.key.operation, o.key.authentication, o.key.status, o.key.result
	row.Count++
	row.DurationMS += max(time.Since(o.started).Milliseconds(), 0)
	if o.body != nil {
		row.Bytes += o.body.bytes
	}
	row.RetryAfterSeconds = max(row.RetryAfterSeconds, o.retryAfter)
	if o.remaining >= 0 && (row.RateLimitRemaining < 0 || o.remaining < row.RateLimitRemaining) {
		row.RateLimitRemaining = o.remaining
	}
	row.RateLimitReset = max(row.RateLimitReset, o.reset)
	o.stats.requests[o.key] = row
}

type sourceCountingBody struct {
	io.ReadCloser
	bytes int64
}

func (b *sourceCountingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	return n, err
}
