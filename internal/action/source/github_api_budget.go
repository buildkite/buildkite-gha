package source

import (
	"context"
	"sync"
	"time"
)

// A resolver has one provisioned token, but requests for the event repository
// remain anonymous. Throttling either budget must leave the other usable.
// This state is process-local: it never persists credentials or API failures
// into the immutable source caches.
type githubAPIBudget struct {
	mu     sync.Mutex
	limits map[bool]RateLimitError
}

func (b *githubAPIBudget) check(ctx context.Context, authenticated bool, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rate, limited := b.limits[authenticated]
	if !limited {
		return nil
	}
	if !now.Before(rate.Reset) {
		delete(b.limits, authenticated)
		return nil
	}
	return &rate
}

func (b *githubAPIBudget) record(authenticated bool, rate *RateLimitError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits == nil {
		b.limits = make(map[bool]RateLimitError, 2)
	}
	// Concurrent requests already in flight may finish after the first limit.
	// A later response must not shorten a previously observed retry deadline.
	if previous, ok := b.limits[authenticated]; !ok || rate.Reset.After(previous.Reset) {
		b.limits[authenticated] = *rate
	}
}
