package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolverRateLimitSeparatesAuthenticatedAndAnonymousRequests(t *testing.T) {
	for _, limitAuthenticated := range []bool{false, true} {
		t.Run(fmt.Sprintf("authenticated=%t", limitAuthenticated), func(t *testing.T) {
			var limitedRequests, otherRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authenticated := r.Header.Get("Authorization") != ""
				if authenticated == limitAuthenticated {
					limitedRequests.Add(1)
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				otherRequests.Add(1)
				writeBudgetPublicResponse(w, r)
			}))
			t.Cleanup(server.Close)
			resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL), WithGitHubActionSourceTokenProvider("pipeline/repository", func(context.Context) (string, error) {
				return "test-app-token", nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			limitedRepo, otherRepo := "pipeline/repository", "public/action"
			if limitAuthenticated {
				limitedRepo, otherRepo = otherRepo, limitedRepo
			}
			limited, _ := Parse(limitedRepo + "@v1")
			_, err = resolver.Resolve(t.Context(), limited)
			var first *RateLimitError
			if !errors.As(err, &first) {
				t.Fatalf("first error = %v, want rate limit", err)
			}
			// Returned errors must not expose the shared cooldown to mutation.
			deadline := first.Reset
			first.Reset = time.Time{}
			second, _ := Parse(limitedRepo + "@v2")
			_, err = resolver.Resolve(t.Context(), second)
			var rate *RateLimitError
			if !errors.As(err, &rate) || !rate.Reset.Equal(deadline) || limitedRequests.Load() != 1 {
				t.Fatalf("cached error = %v, requests = %d; want original deadline and one request", err, limitedRequests.Load())
			}
			other, _ := Parse(otherRepo + "@v1")
			if _, err := resolver.Resolve(t.Context(), other); err != nil || otherRequests.Load() != 2 {
				t.Fatalf("other budget error = %v, requests = %d; want successful independent resolution", err, otherRequests.Load())
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := resolver.Resolve(ctx, limited); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled resolution error = %v", err)
			}
		})
	}
}

func TestResolverRateLimitResumesAtRetryDeadline(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name       string
		status     int
		remaining  string
		reset      string
		retryAfter string
		body       string
		wait       time.Duration
	}{
		{name: "primary", status: 403, remaining: "0", reset: strconv.FormatInt(start.Add(5*time.Minute).Unix(), 10), wait: 5 * time.Minute},
		{name: "secondary with primary quota", status: 429, remaining: "4999", reset: strconv.FormatInt(start.Add(time.Hour).Unix(), 10), retryAfter: "15", wait: 15 * time.Second},
		{name: "secondary forbidden with date", status: 403, remaining: "4999", retryAfter: start.Add(20 * time.Second).Format(http.TimeFormat), wait: 20 * time.Second},
		{name: "both limits", status: 403, remaining: "0", reset: strconv.FormatInt(start.Add(10*time.Second).Unix(), 10), retryAfter: "30", wait: 30 * time.Second},
		{name: "primary later than retry", status: 429, remaining: "0", reset: strconv.FormatInt(start.Add(5*time.Minute).Unix(), 10), retryAfter: "30", wait: 5 * time.Minute},
		{name: "no headers", status: 429, wait: time.Minute},
		{name: "secondary message", status: 403, remaining: "4999", body: `{"message":"You have exceeded a secondary rate limit"}`, wait: time.Minute},
		{name: "past reset", status: 403, remaining: "0", reset: strconv.FormatInt(start.Add(-time.Minute).Unix(), 10), wait: time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 {
					writeBudgetPublicResponse(w, r)
					return
				}
				w.Header().Set("X-RateLimit-Remaining", tt.remaining)
				w.Header().Set("X-RateLimit-Reset", tt.reset)
				w.Header().Set("Retry-After", tt.retryAfter)
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			t.Cleanup(server.Close)
			resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			now := start
			resolver.cfg.now = func() time.Time { return now }
			ref, _ := Parse("public/action@v1")
			_, err = resolver.Resolve(t.Context(), ref)
			var rate *RateLimitError
			deadline := start.Add(tt.wait)
			if !errors.As(err, &rate) || !rate.Reset.Equal(deadline) {
				t.Fatalf("error = %v, want retry at %s", err, deadline)
			}
			now = deadline.Add(-time.Nanosecond)
			other, _ := Parse("another/action@v2")
			_, err = resolver.Resolve(t.Context(), other)
			if !errors.As(err, &rate) || requests.Load() != 1 {
				t.Fatalf("before deadline: error = %v, requests = %d; want rate limit without another request", err, requests.Load())
			}
			now = deadline
			got, err := resolver.Resolve(t.Context(), other)
			if err != nil || got.Commit != testSHA || requests.Load() != 2 {
				t.Fatalf("at deadline: resolution = %#v, error = %v, requests = %d", got, err, requests.Load())
			}
		})
	}
}

func TestResolverDoesNotSuppressUnrelatedAPIErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var recovered atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !recovered.Load() {
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, `{"message":"Resource not accessible"}`)
					return
				}
				writeBudgetPublicResponse(w, r)
			}))
			t.Cleanup(server.Close)
			resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			ref, _ := Parse("public/action@v1")
			if _, err := resolver.Resolve(t.Context(), ref); err == nil {
				t.Fatal("expected initial API error")
			}
			recovered.Store(true)
			if _, err := resolver.Resolve(t.Context(), ref); err != nil {
				t.Fatalf("API recovery blocked: %v", err)
			}
		})
	}
}

func TestResolverLateResponsesPreserveCooldown(t *testing.T) {
	for _, succeeds := range []bool{false, true} {
		t.Run(fmt.Sprintf("late_success=%t", succeeds), func(t *testing.T) {
			checkLateBudgetResponse(t, succeeds)
		})
	}
}

func checkLateBudgetResponse(t *testing.T, succeeds bool) {
	t.Helper()
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/slow") {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			if succeeds {
				writeBudgetPublicResponse(w, r)
				return
			}
			w.Header().Set("Retry-After", "30")
		} else {
			w.Header().Set("Retry-After", "120")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	resolver.cfg.now = func() time.Time { return start }
	slow, _ := Parse("public/action@slow")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	slowResult := make(chan error, 1)
	go func() {
		_, err := resolver.Resolve(ctx, slow)
		slowResult <- err
	}()
	<-started
	fast, _ := Parse("public/action@fast")
	_, err = resolver.Resolve(t.Context(), fast)
	var rate *RateLimitError
	if !errors.As(err, &rate) || !rate.Reset.Equal(start.Add(2*time.Minute)) {
		t.Fatalf("first completed request = %v, want two-minute cooldown", err)
	}
	close(release)
	if err := <-slowResult; (succeeds && err != nil) || (!succeeds && !errors.As(err, &rate)) {
		t.Fatalf("late request = %v, success expected = %t", err, succeeds)
	}
	other, _ := Parse("another/action@v1")
	_, err = resolver.Resolve(t.Context(), other)
	if !errors.As(err, &rate) || !rate.Reset.Equal(start.Add(2*time.Minute)) || requests.Load() != 2 {
		t.Fatalf("late response changed cooldown: error = %v, requests = %d", err, requests.Load())
	}
}

func writeBudgetPublicResponse(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/git/ref/") {
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
		return
	}
	_, _ = fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
}
