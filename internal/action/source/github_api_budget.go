package source

import (
	"context"
	"sync"
	"time"
)

// A resolver has one provisioned token, but requests for the event repository
// and GHES remain anonymous. Throttling one origin or authentication budget
// must leave the others usable.
// This state is process-local: it never persists credentials or API failures
// into the immutable source caches.
type githubAPIBudget struct {
	mu     sync.Mutex
	limits map[githubAPIBudgetKey]RateLimitError
}

type githubAPIBudgetKey struct {
	serverURL     string
	authenticated bool
}

func (b *githubAPIBudget) check(ctx context.Context, key githubAPIBudgetKey, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rate, limited := b.limits[key]
	if !limited {
		return nil
	}
	if !now.Before(rate.Reset) {
		delete(b.limits, key)
		return nil
	}
	return &rate
}

func (b *githubAPIBudget) record(key githubAPIBudgetKey, rate *RateLimitError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits == nil {
		b.limits = make(map[githubAPIBudgetKey]RateLimitError)
	}
	// Concurrent requests already in flight may finish after the first limit.
	// A later response must not shorten a previously observed retry deadline.
	if previous, ok := b.limits[key]; !ok || rate.Reset.After(previous.Reset) {
		b.limits[key] = *rate
	}
}
