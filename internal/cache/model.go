// Package cache implements the job-local GitHub Actions cache v1 protocol.
// The production runtime does not yet select a durable backend.
package cache

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound    = errors.New("cache object not found")
	ErrContention  = errors.New("cache reservation contention")
	ErrConflict    = errors.New("cache operation conflict")
	ErrTooLarge    = errors.New("cache object too large")
	ErrDenied      = errors.New("cache operation denied")
	ErrRateLimit   = errors.New("cache rate limited")
	ErrUnavailable = errors.New("cache backend unavailable")
)

type Namespace struct{ Organization, Cluster, Pipeline string }
type Scope string
type EntryID string
type ReservationID string

type Entry struct {
	ID           EntryID
	Namespace    Namespace
	Scope        Scope
	Key, Version string
	CreationTime time.Time
	Blob         Blob
	ExpiresAt    time.Time
}

type Reservation struct {
	ID                              ReservationID
	Namespace                       Namespace
	Scope                           Scope
	Key, Version, Owner, Generation string
	DeclaredSize                    *int64
	LeaseExpiresAt                  time.Time
}

type Blob struct {
	Locator, SHA256 string
	Size            int64
	Generation      string
}
type BlobInfo struct {
	SHA256 string
	Size   int64
}
type ByteRange struct{ Start, End int64 }
type BlobSource struct {
	Reader     io.Reader
	Size       int64
	SHA256     string
	Generation string
}

type LookupRequest struct {
	Namespace  Namespace
	Scopes     []Scope
	Candidates []string
	Version    string
}
type ListRequest struct {
	Namespace Namespace
	Scopes    []Scope
	Key       string
	Limit     int
}
type ReserveRequest struct {
	Namespace           Namespace
	Scope               Scope
	Key, Version, Owner string
	DeclaredSize        *int64
}

type Backend interface {
	Lookup(context.Context, LookupRequest) (Entry, bool, error)
	List(context.Context, ListRequest) ([]Entry, error)
	Reserve(context.Context, ReserveRequest) (Reservation, error)
	Upload(context.Context, ReservationID, BlobSource) (Blob, error)
	Commit(context.Context, ReservationID, Blob) (Entry, error)
	Abort(context.Context, ReservationID) error
	Open(context.Context, EntryID, *ByteRange) (io.ReadCloser, BlobInfo, error)
}
