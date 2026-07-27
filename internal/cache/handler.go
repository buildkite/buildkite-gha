package cache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

const mediaType = "application/json;api-version=6.0-preview.1"

type Config struct {
	Token, Session, BaseURL, ContainerBaseURL  string
	TempDir                                    string
	Namespace                                  Namespace
	ReadScopes                                 []Scope
	WriteScope                                 Scope
	ReadOnly                                   bool
	MaxKey, MaxVersion, MaxCandidates, MaxList int
	MaxBody, MaxChunk, MaxArchive              int64
	RegisterRedaction                          func(context.Context, string) error
	random                                     io.Reader
}
type Handler struct {
	backend      Backend
	cfg          Config
	baseURLs     map[string]string
	mu           sync.Mutex
	next         int64
	reservations map[int64]*localReservation
	downloads    map[string]EntryID
	closed       bool
}
type localReservation struct {
	mu        sync.Mutex
	backend   Reservation
	file      *os.File
	path      string
	ranges    []ByteRange
	declared  *int64
	committed bool
	blob      Blob
}

func NewHandler(backend Backend, cfg Config) (*Handler, error) {
	if backend == nil || cfg.Token == "" || cfg.Session == "" {
		return nil, fmt.Errorf("cache backend, token, and session are required")
	}
	if cfg.Namespace.Organization == "" || cfg.Namespace.Cluster == "" || cfg.Namespace.Pipeline == "" || len(cfg.ReadScopes) == 0 || !cfg.ReadOnly && cfg.WriteScope == "" {
		return nil, fmt.Errorf("trusted cache namespace and scopes are required")
	}
	if cfg.MaxKey == 0 {
		cfg.MaxKey = 512
	}
	if cfg.MaxVersion == 0 {
		cfg.MaxVersion = 512
	}
	if cfg.MaxCandidates == 0 {
		cfg.MaxCandidates = 10
	}
	if cfg.MaxList == 0 {
		cfg.MaxList = 100
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = 1 << 20
	}
	if cfg.MaxChunk == 0 {
		cfg.MaxChunk = 64 << 20
	}
	if cfg.MaxArchive == 0 {
		cfg.MaxArchive = 10 << 30
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	if cfg.random == nil {
		cfg.random = rand.Reader
	}
	if cfg.MaxKey < 1 || cfg.MaxVersion < 1 || cfg.MaxCandidates < 1 || cfg.MaxList < 1 || cfg.MaxBody < 1 || cfg.MaxChunk < 1 || cfg.MaxArchive < 1 {
		return nil, fmt.Errorf("cache limits must be positive")
	}
	baseURLs := make(map[string]string, 2)
	for _, baseURL := range []string{cfg.BaseURL, cfg.ContainerBaseURL} {
		if baseURL == "" {
			continue
		}
		u, err := url.Parse(baseURL)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/" {
			return nil, fmt.Errorf("safe cache base URL with trailing slash is required")
		}
		host := strings.ToLower(u.Host)
		if old := baseURLs[host]; old != "" && old != baseURL {
			return nil, fmt.Errorf("cache base URLs must have distinct hosts")
		}
		baseURLs[host] = baseURL
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("safe cache base URL with trailing slash is required")
	}
	return &Handler{backend: backend, cfg: cfg, baseURLs: baseURLs, reservations: map[int64]*localReservation{}, downloads: map[string]EntryID{}}, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			http.NotFound(w, r)
		} else {
			mapError(w, ErrUnavailable)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/downloads/") {
		h.download(w, r)
		return
	}
	if !h.auth(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !acceptsV1(r.Header.Get("Accept")) {
		writeError(w, http.StatusBadRequest, "required Accept header is missing")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/_apis/artifactcache/")
	switch {
	case r.Method == "GET" && p == "cache":
		h.lookup(w, r)
	case r.Method == "GET" && p == "caches":
		h.list(w, r)
	case r.Method == "POST" && p == "caches":
		h.reserve(w, r)
	case strings.HasPrefix(p, "caches/"):
		h.reservation(w, r, strings.TrimPrefix(p, "caches/"))
	default:
		http.NotFound(w, r)
	}
}
func (h *Handler) auth(r *http.Request) bool {
	v := r.Header.Get("Authorization")
	want := "Bearer " + h.cfg.Token
	return len(v) == len(want) && subtle.ConstantTimeCompare([]byte(v), []byte(want)) == 1
}
func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) {
	keys := strings.Split(r.URL.Query().Get("keys"), ",")
	v := r.URL.Query().Get("version")
	if len(keys) > h.cfg.MaxCandidates || !validStrings(keys, h.cfg.MaxKey) || v == "" || len(v) > h.cfg.MaxVersion {
		writeError(w, 400, "invalid cache lookup")
		return
	}
	e, ok, err := h.backend.Lookup(r.Context(), LookupRequest{Namespace: h.cfg.Namespace, Scopes: h.cfg.ReadScopes, Candidates: keys, Version: v})
	if err != nil {
		mapError(w, err)
		return
	}
	if !ok {
		w.WriteHeader(204)
		return
	}
	baseURL, ok := h.responseBaseURL(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "unrecognized cache service host")
		return
	}
	id, err := h.downloadID(r.Context(), e.ID, baseURL)
	if err != nil {
		mapError(w, ErrUnavailable)
		return
	}
	writeJSON(w, 200, map[string]any{"cacheKey": e.Key, "cacheVersion": e.Version, "scope": e.Scope, "creationTime": e.CreationTime, "archiveLocation": baseURL + "downloads/" + id})
}
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if !validStrings([]string{key}, h.cfg.MaxKey) {
		writeError(w, 400, "invalid list key")
		return
	}
	es, err := h.backend.List(r.Context(), ListRequest{Namespace: h.cfg.Namespace, Scopes: h.cfg.ReadScopes, Key: key, Limit: h.cfg.MaxList})
	if err != nil {
		mapError(w, err)
		return
	}
	items := make([]any, 0, len(es))
	for _, e := range es {
		items = append(items, map[string]any{"cacheKey": e.Key, "cacheVersion": e.Version, "scope": e.Scope, "creationTime": e.CreationTime})
	}
	writeJSON(w, 200, map[string]any{"totalCount": len(items), "artifactCaches": items})
}
func (h *Handler) reserve(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadOnly {
		mapError(w, ErrDenied)
		return
	}
	var q struct {
		Key, Version string
		CacheSize    *int64
	}
	if !contentType(r, "application/json") {
		writeError(w, http.StatusBadRequest, "application/json content type is required")
		return
	}
	if !decode(r, w, h.cfg.MaxBody, &q) {
		return
	}
	if !validStrings([]string{q.Key}, h.cfg.MaxKey) || q.Version == "" || len(q.Version) > h.cfg.MaxVersion || q.CacheSize != nil && *q.CacheSize < 0 {
		writeError(w, 400, "invalid reservation")
		return
	}
	if q.CacheSize != nil && *q.CacheSize > h.cfg.MaxArchive {
		mapError(w, ErrTooLarge)
		return
	}
	br, err := h.backend.Reserve(r.Context(), ReserveRequest{Namespace: h.cfg.Namespace, Scope: h.cfg.WriteScope, Key: q.Key, Version: q.Version, Owner: h.cfg.Session, DeclaredSize: q.CacheSize})
	if err != nil {
		mapError(w, err)
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = h.backend.Abort(context.WithoutCancel(r.Context()), br.ID)
		mapError(w, ErrUnavailable)
		return
	}
	defer h.mu.Unlock()
	for id, lr := range h.reservations {
		if lr.backend.ID == br.ID {
			writeJSON(w, 201, map[string]any{"cacheId": id})
			return
		}
	}
	f, err := os.CreateTemp(h.cfg.TempDir, "gha-cache-*")
	if err != nil {
		_ = h.backend.Abort(context.WithoutCancel(r.Context()), br.ID)
		mapError(w, ErrUnavailable)
		return
	}
	h.next++
	h.reservations[h.next] = &localReservation{backend: br, file: f, path: f.Name(), declared: cloneInt64(q.CacheSize)}
	writeJSON(w, 201, map[string]any{"cacheId": h.next})
}
func (h *Handler) reservation(w http.ResponseWriter, r *http.Request, s string) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		mapError(w, ErrNotFound)
		return
	}
	h.mu.Lock()
	lr := h.reservations[id]
	closed := h.closed
	h.mu.Unlock()
	if lr == nil || closed {
		mapError(w, ErrNotFound)
		return
	}
	if h.cfg.ReadOnly {
		mapError(w, ErrDenied)
		return
	}
	if r.Method == "PATCH" {
		h.patch(w, r, lr)
		return
	}
	if r.Method == "POST" {
		h.commit(w, r, lr)
		return
	}
	w.WriteHeader(405)
}
func (h *Handler) patch(w http.ResponseWriter, r *http.Request, lr *localReservation) {
	if !contentType(r, "application/octet-stream") {
		writeError(w, http.StatusBadRequest, "application/octet-stream content type is required")
		return
	}
	start, end, ok := parseContentRange(r.Header.Get("Content-Range"))
	lr.mu.Lock()
	declared := cloneInt64(lr.declared)
	lr.mu.Unlock()
	if !ok || end-start+1 > h.cfg.MaxChunk || end >= h.cfg.MaxArchive || declared != nil && end >= *declared {
		if ok && (end-start+1 > h.cfg.MaxChunk || end >= h.cfg.MaxArchive) {
			mapError(w, ErrTooLarge)
		} else {
			writeError(w, 400, "invalid content range")
		}
		return
	}
	n := end - start + 1
	if r.ContentLength >= 0 && r.ContentLength != n {
		writeError(w, 400, "content length does not match range")
		return
	}
	stage, err := os.CreateTemp(h.cfg.TempDir, "gha-chunk-*")
	if err != nil {
		mapError(w, ErrUnavailable)
		return
	}
	name := stage.Name()
	defer func() { _ = os.Remove(name) }()
	got, copyErr := io.Copy(stage, io.LimitReader(r.Body, n+1))
	if copyErr != nil || got != n {
		_ = stage.Close()
		writeError(w, 400, "body length does not match range")
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	defer func() { _ = stage.Close() }()
	buf := make([]byte, 32*1024)
	for _, old := range lr.ranges {
		a, b := max(start, old.Start), min(end, old.End)
		if a <= b {
			left := b - a + 1
			for off := int64(0); off < left; {
				want := min(int64(len(buf)), left-off)
				x := buf[:want]
				if _, err := stage.ReadAt(x, a-start+off); err != nil {
					mapError(w, ErrUnavailable)
					return
				}
				y := make([]byte, want)
				if _, err := lr.file.ReadAt(y, a+off); err != nil || !equal(x, y) {
					mapError(w, ErrConflict)
					return
				}
				off += want
			}
		}
	}
	if lr.committed {
		if !rangeCovered(lr.ranges, start, end) {
			mapError(w, ErrConflict)
			return
		}
		w.WriteHeader(204)
		return
	}
	if _, err := stage.Seek(0, 0); err != nil {
		mapError(w, ErrUnavailable)
		return
	}
	if _, err := io.Copy(io.NewOffsetWriter(lr.file, start), stage); err != nil {
		mapError(w, ErrUnavailable)
		return
	}
	lr.ranges = merge(append(lr.ranges, ByteRange{start, end}))
	w.WriteHeader(204)
}
func (h *Handler) commit(w http.ResponseWriter, r *http.Request, lr *localReservation) {
	var q struct{ Size int64 }
	if !contentType(r, "application/json") {
		writeError(w, http.StatusBadRequest, "application/json content type is required")
		return
	}
	if !decode(r, w, h.cfg.MaxBody, &q) {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if q.Size < 0 || q.Size > h.cfg.MaxArchive {
		mapError(w, ErrTooLarge)
		return
	}
	complete := q.Size == 0 && len(lr.ranges) == 0 || len(lr.ranges) == 1 && lr.ranges[0] == (ByteRange{0, q.Size - 1})
	if lr.declared != nil && q.Size != *lr.declared || !complete {
		mapError(w, ErrConflict)
		return
	}
	if lr.committed {
		if lr.blob.Size == q.Size {
			w.WriteHeader(204)
		} else {
			mapError(w, ErrConflict)
		}
		return
	}
	if _, err := lr.file.Seek(0, 0); err != nil {
		mapError(w, ErrUnavailable)
		return
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, lr.file, q.Size); err != nil {
		mapError(w, ErrConflict)
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if _, err := lr.file.Seek(0, 0); err != nil {
		mapError(w, ErrUnavailable)
		return
	}
	blob, err := h.backend.Upload(r.Context(), lr.backend.ID, BlobSource{Reader: io.LimitReader(lr.file, q.Size), Size: q.Size, SHA256: digest, Generation: lr.backend.Generation})
	if err != nil {
		mapError(w, err)
		return
	}
	if _, err = h.backend.Commit(r.Context(), lr.backend.ID, blob); err != nil {
		mapError(w, err)
		return
	}
	lr.blob = blob
	lr.committed = true
	w.WriteHeader(204)
}
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/downloads/")
	h.mu.Lock()
	entry, ok := h.downloads[id]
	closed := h.closed
	h.mu.Unlock()
	if !ok || closed {
		http.NotFound(w, r)
		return
	}
	rc, info, err := h.backend.Open(r.Context(), entry, nil)
	if err != nil {
		mapError(w, err)
		return
	}
	var br *ByteRange
	if rg := r.Header.Get("Range"); rg != "" {
		_ = rc.Close()
		br, err = parseRange(rg, info.Size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid range")
			return
		}
		rc, info, err = h.backend.Open(r.Context(), entry, br)
		if err != nil {
			mapError(w, err)
			return
		}
	}
	defer func() { _ = rc.Close() }()
	length := info.Size
	w.Header().Set("Accept-Ranges", "bytes")
	if br != nil {
		length = br.End - br.Start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", br.Start, br.End, info.Size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if br != nil {
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method == "GET" {
		_, _ = io.Copy(w, rc)
	}
}
func (h *Handler) responseBaseURL(r *http.Request) (string, bool) {
	if h.cfg.ContainerBaseURL == "" {
		return h.cfg.BaseURL, true
	}
	baseURL, ok := h.baseURLs[strings.ToLower(r.Host)]
	return baseURL, ok
}

func (h *Handler) downloadID(ctx context.Context, e EntryID, baseURL string) (string, error) {
	for range 8 {
		b := make([]byte, 16)
		if _, err := io.ReadFull(h.cfg.random, b); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b)
		h.mu.Lock()
		if _, exists := h.downloads[id]; !exists {
			h.downloads[id] = e
			h.mu.Unlock()
			if h.cfg.RegisterRedaction != nil {
				if err := h.cfg.RegisterRedaction(ctx, baseURL+"downloads/"+id); err != nil {
					h.mu.Lock()
					delete(h.downloads, id)
					h.mu.Unlock()
					return "", err
				}
			}
			return id, nil
		}
		h.mu.Unlock()
	}
	return "", fmt.Errorf("generate unique cache download ID")
}
func (h *Handler) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	rs := make([]*localReservation, 0, len(h.reservations))
	for _, r := range h.reservations {
		rs = append(rs, r)
	}
	h.downloads = map[string]EntryID{}
	h.mu.Unlock()
	var result error
	for _, r := range rs {
		r.mu.Lock()
		if !r.committed {
			result = errors.Join(result, h.backend.Abort(context.Background(), r.backend.ID))
		}
		result = errors.Join(result, r.file.Close(), os.Remove(r.path))
		r.mu.Unlock()
	}
	return result
}

func validStrings(v []string, maxLen int) bool {
	if len(v) == 0 {
		return false
	}
	for _, s := range v {
		if s == "" || len(s) > maxLen || strings.Contains(s, ",") {
			return false
		}
	}
	return true
}
func decode(r *http.Request, w http.ResponseWriter, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			mapError(w, ErrTooLarge)
		} else {
			writeError(w, 400, "invalid JSON body")
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid JSON body")
		return false
	}
	return true
}
func parseContentRange(s string) (int64, int64, bool) {
	if !strings.HasPrefix(s, "bytes ") || !strings.HasSuffix(s, "/*") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(s, "bytes "), "/*"), "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, false
	}
	a, errA := strconv.ParseInt(parts[0], 10, 64)
	b, errB := strconv.ParseInt(parts[1], 10, 64)
	return a, b, errA == nil && errB == nil && a >= 0 && b >= a
}
func parseRange(s string, size int64) (*ByteRange, error) {
	if !strings.HasPrefix(s, "bytes=") || strings.Contains(s, ",") {
		return nil, ErrConflict
	}
	p := strings.Split(strings.TrimPrefix(s, "bytes="), "-")
	if len(p) != 2 || size <= 0 || p[0] == "" && p[1] == "" {
		return nil, ErrConflict
	}
	if p[0] == "" {
		suffix, err := strconv.ParseInt(p[1], 10, 64)
		if err != nil || suffix <= 0 {
			return nil, ErrConflict
		}
		if suffix > size {
			suffix = size
		}
		return &ByteRange{Start: size - suffix, End: size - 1}, nil
	}
	a, e := strconv.ParseInt(p[0], 10, 64)
	if e != nil {
		return nil, e
	}
	b := int64(-1)
	if p[1] != "" {
		b, e = strconv.ParseInt(p[1], 10, 64)
	}
	if e != nil || a < 0 || a >= size || b >= 0 && b < a {
		return nil, ErrConflict
	}
	if b < 0 {
		b = size - 1
	} else if b >= size {
		b = size - 1
	}
	return &ByteRange{a, b}, nil
}

func contentType(r *http.Request, want string) bool {
	value, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && value == want
}

func acceptsV1(header string) bool {
	for value := range strings.SplitSeq(header, ",") {
		media, parameters, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err == nil && media == "application/json" && parameters["api-version"] == "6.0-preview.1" {
			return true
		}
	}
	return false
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func merge(v []ByteRange) []ByteRange {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j].Start < v[j-1].Start; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	out := v[:0]
	for _, r := range v {
		if len(out) > 0 && r.Start <= out[len(out)-1].End+1 {
			out[len(out)-1].End = max(out[len(out)-1].End, r.End)
		} else {
			out = append(out, r)
		}
	}
	return out
}

func rangeCovered(ranges []ByteRange, start, end int64) bool {
	for _, covered := range ranges {
		if covered.Start <= start && covered.End >= end {
			return true
		}
	}
	return false
}
func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, msg string) {
	if len(msg) > 256 {
		msg = msg[:256]
	}
	writeJSON(w, status, map[string]string{"message": msg})
}
func mapError(w http.ResponseWriter, err error) {
	status := 503
	msg := "cache backend unavailable"
	switch {
	case errors.Is(err, ErrNotFound):
		status = 404
		msg = "cache object not found"
	case errors.Is(err, ErrContention), errors.Is(err, ErrConflict):
		status = 409
		msg = "cache conflict"
	case errors.Is(err, ErrTooLarge):
		status = 413
		msg = "cache size limit exceeded"
	case errors.Is(err, ErrDenied):
		status = 403
		msg = "cache operation denied"
	case errors.Is(err, ErrRateLimit):
		status = 429
		msg = "cache rate limited"
	}
	writeError(w, status, msg)
}
