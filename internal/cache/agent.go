package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const agentResponseLimit = 64 << 10

type AgentConfig struct {
	Endpoint string
	JobID    string
	Token    string
	Client   *http.Client
}

type AgentLimits struct {
	MaxArchiveSize  int64 `json:"max_archive_size_bytes"`
	MaxCandidates   int   `json:"max_candidates"`
	MaxKeyBytes     int   `json:"max_key_bytes"`
	MaxVersionBytes int   `json:"max_version_bytes"`
}

type AgentCapability struct {
	Enabled bool        `json:"enabled"`
	Mode    string      `json:"mode"`
	Limits  AgentLimits `json:"limits"`
	Reason  string      `json:"reason,omitempty"`
}

// AgentBackend implements Backend over Buildkite's job-authenticated GitHub
// Actions cache API. Namespace and visibility are derived and enforced by the
// server; the Backend request fields are compatibility metadata only.
type AgentBackend struct {
	baseURL      string
	token        string
	client       *http.Client
	directClient *http.Client

	mu           sync.Mutex
	reservations map[ReservationID]*agentReservation
}

type agentReservation struct {
	mu sync.Mutex

	reservation Reservation
	token       string
	blob        Blob
	entry       Entry
}

type agentEntry struct {
	EntryID   string    `json:"entry_id"`
	Scope     string    `json:"scope"`
	Key       string    `json:"key"`
	Version   string    `json:"version"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type agentInstruction struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

func NewAgentBackend(ctx context.Context, cfg AgentConfig) (*AgentBackend, AgentCapability, error) {
	baseURL, err := agentCacheURL(cfg.Endpoint, cfg.JobID)
	if err != nil {
		return nil, AgentCapability{}, err
	}
	if cfg.Token == "" {
		return nil, AgentCapability{}, fmt.Errorf("cache Agent token is required")
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	apiClient := *client
	apiClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	directClient := *client
	directClient.Timeout = 0
	directClient.Jar = nil
	directClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	b := &AgentBackend{
		baseURL: baseURL, token: cfg.Token, client: &apiClient, directClient: &directClient,
		reservations: make(map[ReservationID]*agentReservation),
	}
	var capability AgentCapability
	if err := b.api(ctx, http.MethodGet, "", nil, &capability); err != nil {
		return nil, AgentCapability{}, err
	}
	if err := validateAgentCapability(capability); err != nil {
		return nil, AgentCapability{}, err
	}
	if !capability.Enabled {
		return nil, capability, nil
	}
	return b, capability, nil
}

func agentCacheURL(endpoint, jobID string) (string, error) {
	if endpoint == "" || !validAgentJobID(jobID) {
		return "", fmt.Errorf("cache Agent endpoint and job ID are required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("safe cache Agent endpoint is required")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jobs/" + url.PathEscape(jobID) + "/github-actions-cache/"
	return u.String(), nil
}

func validAgentJobID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range []byte(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validateAgentCapability(capability AgentCapability) error {
	if capability.Mode != "disabled" && capability.Mode != "read-only" && capability.Mode != "read-write" {
		return fmt.Errorf("%w: invalid cache Agent mode", ErrUnavailable)
	}
	if capability.Enabled != (capability.Mode != "disabled") {
		return fmt.Errorf("%w: inconsistent cache Agent capability", ErrUnavailable)
	}
	if !capability.Enabled {
		return nil
	}
	if capability.Limits.MaxArchiveSize <= 0 || capability.Limits.MaxCandidates <= 0 || capability.Limits.MaxKeyBytes <= 0 || capability.Limits.MaxVersionBytes <= 0 {
		return fmt.Errorf("%w: invalid cache Agent limits", ErrUnavailable)
	}
	return nil
}

func (b *AgentBackend) Lookup(ctx context.Context, q LookupRequest) (Entry, bool, error) {
	var response struct {
		Entry *agentEntry `json:"entry"`
	}
	if err := b.api(ctx, http.MethodPost, "lookup", map[string]any{"candidates": q.Candidates, "version": q.Version}, &response); err != nil {
		return Entry{}, false, err
	}
	if response.Entry == nil {
		return Entry{}, false, nil
	}
	entry, err := agentBackendEntry(*response.Entry, q.Namespace)
	if err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}

// The legacy v1 client uses list only as a best-effort pre-reservation peek.
// Returning an empty list preserves correctness because reserve is atomic and
// reports contention; the production API deliberately exposes no unversioned
// metadata enumeration operation.
func (b *AgentBackend) List(ctx context.Context, _ ListRequest) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []Entry{}, nil
}

func (b *AgentBackend) Reserve(ctx context.Context, q ReserveRequest) (Reservation, error) {
	body := map[string]any{"key": q.Key, "version": q.Version}
	if q.DeclaredSize != nil {
		body["declared_size"] = *q.DeclaredSize
	}
	var response struct {
		ReservationID    string    `json:"reservation_id"`
		ReservationToken string    `json:"reservation_token"`
		ExpiresAt        time.Time `json:"expires_at"`
	}
	if err := b.api(ctx, http.MethodPost, "reserve", body, &response); err != nil {
		return Reservation{}, err
	}
	if response.ReservationID == "" || response.ReservationToken == "" || response.ExpiresAt.IsZero() {
		return Reservation{}, fmt.Errorf("%w: invalid cache reservation", ErrUnavailable)
	}
	generationBytes := sha256.Sum256([]byte(response.ReservationToken))
	reservation := Reservation{
		ID: ReservationID(response.ReservationID), Namespace: q.Namespace, Scope: q.Scope,
		Key: q.Key, Version: q.Version, Owner: q.Owner,
		Generation: hex.EncodeToString(generationBytes[:]), DeclaredSize: cloneInt64(q.DeclaredSize),
		LeaseExpiresAt: response.ExpiresAt,
	}
	state := &agentReservation{reservation: reservation, token: response.ReservationToken}
	b.mu.Lock()
	if _, exists := b.reservations[reservation.ID]; exists {
		b.mu.Unlock()
		return Reservation{}, fmt.Errorf("%w: duplicate cache reservation", ErrUnavailable)
	}
	b.reservations[reservation.ID] = state
	b.mu.Unlock()
	return reservation, nil
}

func (b *AgentBackend) Upload(ctx context.Context, id ReservationID, source BlobSource) (Blob, error) {
	state := b.reservation(id)
	if state == nil {
		return Blob{}, ErrNotFound
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if source.Generation != state.reservation.Generation || source.Size < 0 || !validAgentDigest(source.SHA256) {
		return Blob{}, ErrConflict
	}
	if state.blob.Locator != "" {
		if state.blob.Size == source.Size && state.blob.SHA256 == source.SHA256 {
			return state.blob, nil
		}
		return Blob{}, ErrConflict
	}
	var response struct {
		Upload agentInstruction `json:"upload"`
	}
	if err := b.api(ctx, http.MethodPost, "prepare-upload", map[string]any{
		"reservation_id": id, "reservation_token": state.token,
		"size": source.Size, "sha256": source.SHA256,
	}, &response); err != nil {
		return Blob{}, err
	}
	if err := b.transfer(ctx, response.Upload, http.MethodPut, source.Reader, source.Size, nil); err != nil {
		return Blob{}, err
	}
	state.blob = Blob{Locator: string(id), SHA256: source.SHA256, Size: source.Size, Generation: source.Generation}
	return state.blob, nil
}

func (b *AgentBackend) Commit(ctx context.Context, id ReservationID, blob Blob) (Entry, error) {
	state := b.reservation(id)
	if state == nil {
		return Entry{}, ErrNotFound
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.entry.ID != "" {
		if state.blob == blob {
			return state.entry, nil
		}
		return Entry{}, ErrConflict
	}
	if state.blob != blob {
		return Entry{}, ErrConflict
	}
	var response struct {
		Entry agentEntry `json:"entry"`
	}
	if err := b.api(ctx, http.MethodPost, "commit", map[string]any{
		"reservation_id": id, "reservation_token": state.token,
	}, &response); err != nil {
		return Entry{}, err
	}
	entry, err := agentBackendEntry(response.Entry, state.reservation.Namespace)
	if err != nil {
		return Entry{}, err
	}
	if entry.Key != state.reservation.Key || entry.Version != state.reservation.Version || entry.Blob.Size != blob.Size || entry.Blob.SHA256 != blob.SHA256 {
		return Entry{}, fmt.Errorf("%w: committed cache entry mismatch", ErrUnavailable)
	}
	state.entry = entry
	return entry, nil
}

func (b *AgentBackend) Abort(ctx context.Context, id ReservationID) error {
	state := b.reservation(id)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.entry.ID != "" {
		return nil
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := b.api(ctx, http.MethodPost, "abort", map[string]any{
		"reservation_id": id, "reservation_token": state.token,
	}, &response); err != nil {
		return err
	}
	if response.Status != "aborted" && response.Status != "missing" && response.Status != "committed" {
		return fmt.Errorf("%w: invalid cache abort status", ErrUnavailable)
	}
	if response.Status != "committed" {
		b.mu.Lock()
		delete(b.reservations, id)
		b.mu.Unlock()
	}
	return nil
}

func (b *AgentBackend) Open(ctx context.Context, id EntryID, byteRange *ByteRange) (io.ReadCloser, BlobInfo, error) {
	var response struct {
		Entry    agentEntry       `json:"entry"`
		Download agentInstruction `json:"download"`
	}
	if err := b.api(ctx, http.MethodPost, "retrieve", map[string]any{"entry_id": id}, &response); err != nil {
		return nil, BlobInfo{}, err
	}
	entry, err := agentBackendEntry(response.Entry, Namespace{})
	if err != nil || entry.ID != id {
		return nil, BlobInfo{}, fmt.Errorf("%w: invalid retrieved cache entry", ErrUnavailable)
	}
	body, err := b.download(ctx, response.Download, byteRange, entry.Blob.Size)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	return body, BlobInfo{SHA256: entry.Blob.SHA256, Size: entry.Blob.Size}, nil
}

func (b *AgentBackend) reservation(id ReservationID) *agentReservation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reservations[id]
}

func (b *AgentBackend) api(ctx context.Context, method, action string, body, response any) error {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: encode cache Agent request", ErrUnavailable)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, b.baseURL+action, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("%w: create cache Agent request", ErrUnavailable)
	}
	request.Header.Set("Authorization", "Token "+b.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	result, err := b.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: cache Agent request failed", ErrUnavailable)
	}
	defer func() { _ = result.Body.Close() }()
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(result.Body, agentResponseLimit))
		return agentResponseError(action, result.StatusCode)
	}
	if response == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(result.Body, agentResponseLimit))
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(result.Body, agentResponseLimit+1))
	if err := decoder.Decode(response); err != nil {
		return fmt.Errorf("%w: decode cache Agent response", ErrUnavailable)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: oversized or trailing cache Agent response", ErrUnavailable)
	}
	return nil
}

func agentResponseError(action string, status int) error {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ErrConflict
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrDenied
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		if action == "reserve" {
			return ErrContention
		}
		return ErrConflict
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusTooManyRequests:
		return ErrRateLimit
	default:
		return ErrUnavailable
	}
}

func agentBackendEntry(value agentEntry, namespace Namespace) (Entry, error) {
	if value.EntryID == "" || value.Scope == "" || value.Key == "" || value.Version == "" || value.Size < 0 || !validAgentDigest(value.SHA256) || value.CreatedAt.IsZero() || value.ExpiresAt.IsZero() || value.ExpiresAt.Before(value.CreatedAt) {
		return Entry{}, fmt.Errorf("%w: invalid cache Agent entry", ErrUnavailable)
	}
	return Entry{
		ID: EntryID(value.EntryID), Namespace: namespace, Scope: Scope(value.Scope),
		Key: value.Key, Version: value.Version, CreationTime: value.CreatedAt,
		Blob:      Blob{Locator: value.EntryID, SHA256: value.SHA256, Size: value.Size, Generation: "committed"},
		ExpiresAt: value.ExpiresAt,
	}, nil
}

func validAgentDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (b *AgentBackend) transfer(ctx context.Context, instruction agentInstruction, method string, body io.Reader, size int64, byteRange *ByteRange) error {
	request, err := agentTransferRequest(ctx, instruction, method, body, size, byteRange)
	if err != nil {
		return err
	}
	response, err := b.directClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: cache transfer failed", ErrUnavailable)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, agentResponseLimit))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%w: cache transfer status %d", ErrUnavailable, response.StatusCode)
	}
	return nil
}

func (b *AgentBackend) download(ctx context.Context, instruction agentInstruction, byteRange *ByteRange, size int64) (io.ReadCloser, error) {
	request, err := agentTransferRequest(ctx, instruction, http.MethodGet, nil, size, byteRange)
	if err != nil {
		return nil, err
	}
	response, err := b.directClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: cache download failed", ErrUnavailable)
	}
	wantStatus := http.StatusOK
	wantLength := size
	if byteRange != nil {
		wantStatus = http.StatusPartialContent
		wantLength = byteRange.End - byteRange.Start + 1
	}
	if response.StatusCode != wantStatus || response.ContentLength != wantLength {
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: invalid cache download response", ErrUnavailable)
	}
	if byteRange != nil {
		wantContentRange := fmt.Sprintf("bytes %d-%d/%d", byteRange.Start, byteRange.End, size)
		if response.Header.Get("Content-Range") != wantContentRange {
			_ = response.Body.Close()
			return nil, fmt.Errorf("%w: invalid cache download range", ErrUnavailable)
		}
	}
	return response.Body, nil
}

func agentTransferRequest(ctx context.Context, instruction agentInstruction, method string, body io.Reader, size int64, byteRange *ByteRange) (*http.Request, error) {
	if instruction.Method != method || instruction.URL == "" || instruction.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("%w: invalid cache transfer instruction", ErrUnavailable)
	}
	u, err := url.Parse(instruction.URL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("%w: unsafe cache transfer URL", ErrUnavailable)
	}
	if method == http.MethodPut && size == 0 {
		body = http.NoBody
	}
	request, err := http.NewRequestWithContext(ctx, method, instruction.URL, body)
	if err != nil {
		return nil, fmt.Errorf("%w: create cache transfer request", ErrUnavailable)
	}
	for name, value := range instruction.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || canonical == "Authorization" || canonical == "Cookie" || canonical == "Host" || canonical == "Connection" || canonical == "Transfer-Encoding" || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("%w: unsafe cache transfer header", ErrUnavailable)
		}
		if canonical == "Content-Length" {
			if method != http.MethodPut {
				return nil, fmt.Errorf("%w: unexpected cache transfer content length", ErrUnavailable)
			}
			declared, err := strconv.ParseInt(value, 10, 64)
			if err != nil || declared != size {
				return nil, fmt.Errorf("%w: invalid cache transfer content length", ErrUnavailable)
			}
			request.ContentLength = declared
			continue
		}
		request.Header.Set(canonical, value)
	}
	if method == http.MethodPut {
		if request.ContentLength != size {
			return nil, fmt.Errorf("%w: cache upload instruction did not bind content length", ErrUnavailable)
		}
	} else if byteRange != nil {
		if byteRange.Start < 0 || byteRange.End < byteRange.Start || byteRange.End >= size {
			return nil, ErrConflict
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", byteRange.Start, byteRange.End))
	}
	return request, nil
}

var _ Backend = (*AgentBackend)(nil)
