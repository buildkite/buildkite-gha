package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPublicRepositoryChecksIsolateResolversAndRetryFailures(t *testing.T) {
	var requests atomic.Int64
	var private atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if private.Load() {
			_, _ = fmt.Fprint(w, `{"private":true,"visibility":"private"}`)
		} else {
			_, _ = fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
		}
	}))
	t.Cleanup(server.Close)
	first, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := Parse("Public/Action@v1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPublicRepositoryChecks(t.Context())
	if err := first.ensurePublic(ctx, ref); err != nil {
		t.Fatal(err)
	}
	canonical, err := Parse("public/action@v2")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ensurePublic(ctx, canonical); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("canonical repository requests = %d, want 1", got)
	}
	private.Store(true)
	var notPublic *NotPublicError
	if err := second.ensurePublic(ctx, canonical); !errors.As(err, &notPublic) {
		t.Fatalf("second resolver error = %v, want its own visibility refusal", err)
	}
	private.Store(false)
	if err := second.ensurePublic(ctx, canonical); err != nil {
		t.Fatalf("visibility retry: %v", err)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("visibility requests = %d, want first resolver, second resolver refusal, then retry", got)
	}
	private.Store(true)
	if err := first.ensurePublic(t.Context(), canonical); !errors.As(err, &notPublic) {
		t.Fatalf("unscoped check error = %v, want fresh visibility refusal", err)
	}
}

func TestPublicRepositoryChecksWaiters(t *testing.T) {
	for _, cancelLeader := range []bool{false, true} {
		name := "successful leader"
		if cancelLeader {
			name = "canceled leader"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int64
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				_, _ = fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
			}))
			t.Cleanup(server.Close)
			resolver, err := NewResolver(server.Client(), WithTestEndpoints(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			ref, err := Parse("public/action@v1")
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithPublicRepositoryChecks(t.Context())
			leaderCtx, stopLeader := context.WithCancel(ctx)
			defer stopLeader()
			leader := make(chan error, 1)
			go func() { leader <- resolver.ensurePublic(leaderCtx, ref) }()
			<-started

			liveCtx := newPublicCheckWaitContext(ctx)
			live := make(chan error, 1)
			go func() { live <- resolver.ensurePublic(liveCtx, ref) }()
			<-liveCtx.waiting
			cancelCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			waitCtx := newPublicCheckWaitContext(cancelCtx)
			canceled := make(chan error, 1)
			go func() { canceled <- resolver.ensurePublic(waitCtx, ref) }()
			<-waitCtx.waiting
			cancel()
			if err := <-canceled; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled waiter error = %v", err)
			}

			wantRequests := int64(1)
			if cancelLeader {
				stopLeader()
				wantRequests++
			} else {
				unblock()
			}
			if err := <-leader; cancelLeader && !errors.Is(err, context.Canceled) || !cancelLeader && err != nil {
				t.Fatalf("leader error = %v", err)
			}
			if err := <-live; err != nil {
				t.Fatalf("live waiter error = %v", err)
			}
			if err := resolver.ensurePublic(ctx, ref); err != nil {
				t.Fatalf("reuse successful check: %v", err)
			}
			if got := requests.Load(); got != wantRequests {
				t.Errorf("visibility requests = %d, want %d", got, wantRequests)
			}
		})
	}
}

type publicCheckWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func newPublicCheckWaitContext(ctx context.Context) *publicCheckWaitContext {
	return &publicCheckWaitContext{Context: ctx, waiting: make(chan struct{})}
}

func (ctx *publicCheckWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}
