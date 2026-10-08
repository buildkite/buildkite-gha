package source

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
)

type missingRefResolutionsKey struct{}

type missingRefIdentity struct {
	resolver  *Resolver
	reference Reference
}

type missingRefResolution struct {
	done     chan struct{}
	resolved Resolved
	err      error
}

type missingRefResolutions struct {
	mu   sync.Mutex
	refs map[missingRefIdentity]*missingRefResolution
}

// WithMissingRefResolutions starts one compilation's missing-ref cache. A fresh
// scope replaces any parent scope so later compilations can discover new refs.
// Only completed, definitive API misses are retained; Git denials and transient
// failures are retried. The scope never grants repository access.
func WithMissingRefResolutions(ctx context.Context) context.Context {
	return context.WithValue(ctx, missingRefResolutionsKey{}, &missingRefResolutions{
		refs: make(map[missingRefIdentity]*missingRefResolution),
	})
}

func (r *Resolver) resolveMutable(ctx context.Context, ref Reference) (Resolved, error) {
	scope, _ := ctx.Value(missingRefResolutionsKey{}).(*missingRefResolutions)
	if scope == nil {
		return r.resolveMutableUncached(ctx, ref)
	}
	// Preserve the exact resource, requested ref, and repository-source authority.
	// Repository spelling is case-insensitive; refs and paths are not.
	identity := ref
	identity.Owner = strings.ToLower(ref.Owner)
	identity.Repository = strings.ToLower(ref.Repository)
	identity.Raw = ""
	key := missingRefIdentity{resolver: r, reference: identity}
	for {
		if err := ctx.Err(); err != nil {
			return Resolved{}, err
		}
		scope.mu.Lock()
		if call, ok := scope.refs[key]; ok {
			scope.mu.Unlock()
			select {
			case <-ctx.Done():
				return Resolved{}, ctx.Err()
			case <-call.done:
				if err := ctx.Err(); err != nil {
					return Resolved{}, err
				}
				if errors.Is(call.err, context.Canceled) || errors.Is(call.err, context.DeadlineExceeded) {
					continue
				}
				resolved := call.resolved
				if call.err == nil {
					resolved.Reference = ref
				}
				return resolved, call.err
			}
		}
		call := &missingRefResolution{done: make(chan struct{})}
		scope.refs[key] = call
		scope.mu.Unlock()

		resolved, err := r.resolveMutableUncached(ctx, ref)
		scope.mu.Lock()
		if !errors.As(err, new(*missingRefError)) {
			delete(scope.refs, key)
		}
		call.resolved, call.err = resolved, err
		close(call.done)
		scope.mu.Unlock()
		return resolved, err
	}
}

// missingRefError marks absence only after every applicable lookup returned
// HTTP 404. It retains the same non-enumerating diagnostic as NotPublicError.
// A permitted Git fallback replaces this error with its own final outcome.
type missingRefError struct{ cause error }

func (e *missingRefError) Error() string { return e.cause.Error() }
func (e *missingRefError) Unwrap() error { return e.cause }

func isGitHubNotFound(err error) bool {
	var notPublic *NotPublicError
	return errors.As(err, &notPublic) && notPublic.statusCode == http.StatusNotFound
}

func markMissingRef(err error) error {
	if isGitHubNotFound(err) {
		return &missingRefError{cause: err}
	}
	return err
}
