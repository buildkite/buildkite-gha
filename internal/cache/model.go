// Package cache implements the job-local GitHub Actions cache v1 protocol and
// its pluggable storage boundary.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
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

// LookupRequest asks a backend for the newest committed entry matching versioned
// candidates. Namespace and read scopes are bound at backend construction.
type LookupRequest struct {
	Candidates []string
	Version    string
}

// ListRequest asks a backend for committed entries under its bound namespace and
// read scopes. Key is an optional prefix filter.
type ListRequest struct {
	Key   string
	Limit int
}

// ReserveRequest asks a backend to reserve its bound write scope for a
// versioned key. Owner is the local adapter session, not a storage authority.
type ReserveRequest struct {
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

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
