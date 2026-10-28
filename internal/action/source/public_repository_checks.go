package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type publicRepositoryChecksKey struct{}

type publicRepositoryIdentity struct {
	resolver   *Resolver
	repository string
}

type publicRepositoryCheck struct {
	done chan struct{}
	err  error
}

type publicRepositoryChecks struct {
	mu     sync.Mutex
	checks map[publicRepositoryIdentity]*publicRepositoryCheck
}

// WithPublicRepositoryChecks starts one compilation's public-repository checks.
// A fresh scope always replaces any parent scope: successful visibility checks
// must not authorize references in a later compilation using the same resolver.
func WithPublicRepositoryChecks(ctx context.Context) context.Context {
	return context.WithValue(ctx, publicRepositoryChecksKey{}, &publicRepositoryChecks{
		checks: make(map[publicRepositoryIdentity]*publicRepositoryCheck),
	})
}

func (r *Resolver) ensurePublic(ctx context.Context, ref Reference) error {
	scope, _ := ctx.Value(publicRepositoryChecksKey{}).(*publicRepositoryChecks)
	if scope == nil {
		return r.checkPublicRepository(ctx, ref)
	}
	key := publicRepositoryIdentity{resolver: r, repository: strings.ToLower(ref.Owner + "/" + ref.Repository)}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		scope.mu.Lock()
		if check, ok := scope.checks[key]; ok {
			scope.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-check.done:
				if errors.Is(check.err, context.Canceled) || errors.Is(check.err, context.DeadlineExceeded) {
					continue
				}
				if check.err == nil {
					recordSourceCacheHit(ctx, cachePublicRepositoryCheck)
				}
				return check.err
			}
		}
		check := &publicRepositoryCheck{done: make(chan struct{})}
		scope.checks[key] = check
		scope.mu.Unlock()

		err := r.checkPublicRepository(ctx, ref)
		scope.mu.Lock()
		if err != nil {
			delete(scope.checks, key)
		}
		check.err = err
		close(check.done)
		scope.mu.Unlock()
		return err
	}
}

func (r *Resolver) checkPublicRepository(ctx context.Context, ref Reference) error {
	var repository struct {
		Private    *bool   `json:"private"`
		Visibility *string `json:"visibility"`
	}
	if err := r.get(ctx, repoParts(ref), &repository); err != nil {
		return err
	}
	if repository.Private == nil || repository.Visibility == nil {
		return fmt.Errorf("malformed GitHub API response: repository visibility is missing")
	}
	if *repository.Private || *repository.Visibility != "public" {
		return &NotPublicError{}
	}
	return nil
}
