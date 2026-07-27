package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// memoryBackend is deliberately compiled only with tests.
type memoryBackend struct {
	mu           sync.Mutex
	now          func() time.Time
	lease        time.Duration
	next         int
	reservations map[ReservationID]*memoryReservation
	entries      map[EntryID]Entry
	data         map[string][]byte
	faults       map[string]error
}
type memoryReservation struct {
	Reservation
	blob      Blob
	committed Entry
	aborted   bool
}

func newMemoryBackend(now func() time.Time) *memoryBackend {
	if now == nil {
		now = time.Now
	}
	return &memoryBackend{now: now, lease: time.Minute, reservations: map[ReservationID]*memoryReservation{}, entries: map[EntryID]Entry{}, data: map[string][]byte{}, faults: map[string]error{}}
}
func (m *memoryBackend) fail(op string) error {
	if err := m.faults[op]; err != nil {
		return err
	}
	return nil
}
func identity(n Namespace, s Scope, k, v string) string {
	return n.Organization + "\x00" + n.Cluster + "\x00" + n.Pipeline + "\x00" + string(s) + "\x00" + k + "\x00" + v
}
func (m *memoryBackend) Lookup(_ context.Context, q LookupRequest) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("lookup"); err != nil {
		return Entry{}, false, err
	}
	for _, scope := range q.Scopes {
		for _, candidate := range q.Candidates {
			var exact, prefix []Entry
			for _, e := range m.entries {
				if e.Namespace == q.Namespace && e.Scope == scope && e.Version == q.Version {
					if e.Key == candidate {
						exact = append(exact, e)
					} else if strings.HasPrefix(e.Key, candidate) {
						prefix = append(prefix, e)
					}
				}
			}
			if len(exact) > 0 {
				return newest(exact), true, nil
			}
			if len(prefix) > 0 {
				return newest(prefix), true, nil
			}
		}
	}
	return Entry{}, false, nil
}
func newest(es []Entry) Entry {
	sort.Slice(es, func(i, j int) bool {
		if es[i].CreationTime.Equal(es[j].CreationTime) {
			return es[i].ID > es[j].ID
		}
		return es[i].CreationTime.After(es[j].CreationTime)
	})
	return es[0]
}
func (m *memoryBackend) List(_ context.Context, q ListRequest) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("list"); err != nil {
		return nil, err
	}
	allowed := map[Scope]bool{}
	for _, s := range q.Scopes {
		allowed[s] = true
	}
	var out []Entry
	for _, e := range m.entries {
		if e.Namespace == q.Namespace && allowed[e.Scope] && (q.Key == "" || strings.HasPrefix(e.Key, q.Key)) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreationTime.Equal(out[j].CreationTime) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreationTime.After(out[j].CreationTime)
	})
	if q.Limit < len(out) {
		out = out[:q.Limit]
	}
	return out, nil
}
func (m *memoryBackend) Reserve(_ context.Context, q ReserveRequest) (Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("reserve"); err != nil {
		return Reservation{}, err
	}
	idn := identity(q.Namespace, q.Scope, q.Key, q.Version)
	for _, e := range m.entries {
		if identity(e.Namespace, e.Scope, e.Key, e.Version) == idn {
			return Reservation{}, ErrContention
		}
	}
	for _, r := range m.reservations {
		if identity(r.Namespace, r.Scope, r.Key, r.Version) == idn && !r.aborted && m.now().Before(r.LeaseExpiresAt) {
			if r.Owner == q.Owner {
				return r.Reservation, nil
			}
			return Reservation{}, ErrContention
		}
	}
	m.next++
	id := ReservationID(fmt.Sprintf("r%d", m.next))
	r := Reservation{ID: id, Namespace: q.Namespace, Scope: q.Scope, Key: q.Key, Version: q.Version, Owner: q.Owner, Generation: fmt.Sprintf("g%d", m.next), DeclaredSize: cloneInt64(q.DeclaredSize), LeaseExpiresAt: m.now().Add(m.lease)}
	m.reservations[id] = &memoryReservation{Reservation: r}
	return r, nil
}
func (m *memoryBackend) Upload(_ context.Context, id ReservationID, s BlobSource) (Blob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("upload"); err != nil {
		return Blob{}, err
	}
	r := m.reservations[id]
	if r == nil || r.aborted || !m.now().Before(r.LeaseExpiresAt) {
		return Blob{}, ErrNotFound
	}
	b, err := io.ReadAll(io.LimitReader(s.Reader, s.Size+1))
	if err != nil || int64(len(b)) != s.Size {
		return Blob{}, ErrConflict
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	if s.SHA256 != "" && s.SHA256 != digest {
		return Blob{}, ErrConflict
	}
	blob := Blob{Locator: "blob:" + digest, SHA256: digest, Size: s.Size, Generation: r.Generation}
	if r.blob.Locator != "" && r.blob != blob {
		return Blob{}, ErrConflict
	}
	m.data[blob.Locator] = bytes.Clone(b)
	r.blob = blob
	return blob, nil
}
func (m *memoryBackend) Commit(_ context.Context, id ReservationID, b Blob) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("commit"); err != nil {
		return Entry{}, err
	}
	r := m.reservations[id]
	if r == nil || r.aborted || !m.now().Before(r.LeaseExpiresAt) {
		return Entry{}, ErrNotFound
	}
	if r.committed.ID != "" {
		if r.committed.Blob.Size == b.Size && r.committed.Blob.Generation == b.Generation {
			return r.committed, nil
		}
		return Entry{}, ErrConflict
	}
	if b != r.blob || b.Generation != r.Generation {
		return Entry{}, ErrConflict
	}
	for _, entry := range m.entries {
		if identity(entry.Namespace, entry.Scope, entry.Key, entry.Version) == identity(r.Namespace, r.Scope, r.Key, r.Version) {
			return Entry{}, ErrContention
		}
	}
	m.next++
	e := Entry{ID: EntryID(fmt.Sprintf("e%020d", m.next)), Namespace: r.Namespace, Scope: r.Scope, Key: r.Key, Version: r.Version, CreationTime: m.now(), Blob: b}
	r.committed = e
	m.entries[e.ID] = e
	return e, nil
}
func (m *memoryBackend) Abort(_ context.Context, id ReservationID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("abort"); err != nil {
		return err
	}
	if r := m.reservations[id]; r != nil && r.committed.ID == "" {
		r.aborted = true
	}
	return nil
}
func (m *memoryBackend) Open(_ context.Context, id EntryID, br *ByteRange) (io.ReadCloser, BlobInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("open"); err != nil {
		return nil, BlobInfo{}, err
	}
	e, ok := m.entries[id]
	if !ok {
		return nil, BlobInfo{}, ErrNotFound
	}
	b := m.data[e.Blob.Locator]
	if br != nil {
		if br.Start < 0 || br.End < br.Start || br.End >= int64(len(b)) {
			return nil, BlobInfo{}, ErrConflict
		}
		b = b[br.Start : br.End+1]
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(b))), BlobInfo{SHA256: e.Blob.SHA256, Size: e.Blob.Size}, nil
}
