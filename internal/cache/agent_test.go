package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentBackendLifecycleAndDirectTransfers(t *testing.T) {
	const (
		jobID            = "11111111-1111-1111-1111-111111111111"
		reservationID    = "22222222-2222-2222-2222-222222222222"
		reservationToken = "private-reservation-token"
		entryID          = "33333333-3333-3333-3333-333333333333"
	)
	createdAt := time.Date(2026, 7, 28, 1, 2, 3, 0, time.UTC)
	expiresAt := createdAt.Add(7 * 24 * time.Hour)
	namespace := Namespace{"organization", "cluster", "pipeline"}
	scope := Scope("refs/heads/main")
	payload := []byte("opaque cache archive")
	digestBytes := sha256.Sum256(payload)
	digest := hex.EncodeToString(digestBytes[:])
	signedChecksum := base64.StdEncoding.EncodeToString(digestBytes[:])

	var mu sync.Mutex
	var uploaded []byte
	committed := false
	apiCalls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/object" {
			if request.Header.Get("Authorization") != "" {
				t.Errorf("direct transfer leaked privileged headers: %#v", request.Header)
			}
			for name, values := range request.Header {
				for _, value := range values {
					if strings.Contains(value, reservationToken) || strings.Contains(value, "job-token") {
						t.Errorf("direct transfer header %q leaked a privileged token", name)
					}
				}
			}
			switch request.Method {
			case http.MethodPut:
				if request.Header.Get("X-Amz-Checksum-Sha256") != signedChecksum || request.ContentLength != int64(len(payload)) {
					t.Errorf("upload headers = %#v, length = %d", request.Header, request.ContentLength)
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				uploaded = body
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			case http.MethodGet:
				mu.Lock()
				body := bytes.Clone(uploaded)
				mu.Unlock()
				if request.Header.Get("Range") == "bytes=2-6" {
					body = body[2:7]
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 2-6/%d", len(payload)))
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					w.WriteHeader(http.StatusPartialContent)
				} else {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				}
				_, _ = w.Write(body)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}

		apiCalls++
		if request.Header.Get("Authorization") != "Token job-token" {
			t.Errorf("Agent authorization = %q", request.Header.Get("Authorization"))
		}
		base := "/v3/jobs/" + jobID + "/github-actions-cache/"
		if !strings.HasPrefix(request.URL.Path, base) {
			t.Errorf("Agent path = %q", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		action := strings.TrimPrefix(request.URL.Path, base)
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "":
			writeAgentTestJSON(t, w, AgentCapability{Enabled: true, Mode: "read-write", Limits: AgentLimits{
				MaxArchiveSize: 10 << 30, MaxCandidates: 10, MaxKeyBytes: 512, MaxVersionBytes: 512,
			}})
		case "lookup":
			var body struct {
				Candidates []string `json:"candidates"`
				Version    string   `json:"version"`
			}
			decodeAgentTestJSON(t, request, &body)
			if len(body.Candidates) != 1 || body.Candidates[0] != "cache-key" || body.Version != "v1" {
				t.Errorf("lookup = %#v", body)
			}
			if !committed {
				writeAgentTestJSON(t, w, map[string]any{"entry": nil})
				return
			}
			writeAgentTestJSON(t, w, map[string]any{"entry": agentTestEntry(entryID, scope, digest, int64(len(payload)), createdAt, expiresAt)})
		case "reserve":
			writeAgentTestJSON(t, w, map[string]any{
				"reservation_id": reservationID, "reservation_token": reservationToken,
				"expires_at": createdAt.Add(15 * time.Minute),
			})
		case "prepare-upload":
			var body map[string]any
			decodeAgentTestJSON(t, request, &body)
			if body["reservation_token"] != reservationToken || body["sha256"] != digest {
				t.Errorf("prepare upload = %#v", body)
			}
			writeAgentTestJSON(t, w, map[string]any{"upload": agentInstruction{
				Method: http.MethodPut, URL: server.URL + "/object",
				Headers:   map[string]string{"Content-Length": strconv.Itoa(len(payload)), "x-amz-checksum-sha256": signedChecksum},
				ExpiresAt: createdAt.Add(5 * time.Minute),
			}})
		case "commit":
			mu.Lock()
			committed = bytes.Equal(uploaded, payload)
			mu.Unlock()
			if !committed {
				t.Error("commit occurred before exact upload")
			}
			writeAgentTestJSON(t, w, map[string]any{"entry": agentTestEntry(entryID, scope, digest, int64(len(payload)), createdAt, expiresAt)})
		case "retrieve":
			writeAgentTestJSON(t, w, map[string]any{
				"entry":    agentTestEntry(entryID, scope, digest, int64(len(payload)), createdAt, expiresAt),
				"download": agentInstruction{Method: http.MethodGet, URL: server.URL + "/object", Headers: map[string]string{}, ExpiresAt: createdAt.Add(5 * time.Minute)},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	backend, capability, err := NewAgentBackend(context.Background(), AgentConfig{
		Endpoint: server.URL + "/v3", JobID: jobID, Token: "job-token", Client: server.Client(),
	})
	if err != nil || backend == nil || !capability.Enabled || capability.Mode != "read-write" {
		t.Fatalf("NewAgentBackend() = %#v, %#v, %v", backend, capability, err)
	}
	listed, err := backend.List(context.Background(), ListRequest{Namespace: namespace, Scopes: []Scope{scope}, Key: "cache-key", Limit: 100})
	if err != nil || len(listed) != 0 || apiCalls != 1 {
		t.Fatalf("List() = %#v, %v, API calls = %d", listed, err, apiCalls)
	}
	lookup := LookupRequest{Namespace: namespace, Scopes: []Scope{scope}, Candidates: []string{"cache-key"}, Version: "v1"}
	if entry, ok, err := backend.Lookup(context.Background(), lookup); err != nil || ok || entry.ID != "" {
		t.Fatalf("initial Lookup() = %#v, %v, %v", entry, ok, err)
	}
	size := int64(len(payload))
	reservation, err := backend.Reserve(context.Background(), ReserveRequest{
		Namespace: namespace, Scope: scope, Key: "cache-key", Version: "v1", Owner: "session", DeclaredSize: &size,
	})
	if err != nil || reservation.ID != reservationID || reservation.Generation == reservationToken {
		t.Fatalf("Reserve() = %#v, %v", reservation, err)
	}
	blob, err := backend.Upload(context.Background(), reservation.ID, BlobSource{
		Reader: bytes.NewReader(payload), Size: size, SHA256: digest, Generation: reservation.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := backend.Commit(context.Background(), reservation.ID, blob)
	if err != nil || entry.ID != entryID || entry.Scope != scope || entry.CreationTime != createdAt {
		t.Fatalf("Commit() = %#v, %v", entry, err)
	}
	hit, ok, err := backend.Lookup(context.Background(), lookup)
	if err != nil || !ok || hit.ID != entry.ID || hit.Namespace != namespace {
		t.Fatalf("Lookup() = %#v, %v, %v", hit, ok, err)
	}
	reader, info, err := backend.Open(context.Background(), entry.ID, nil)
	if err != nil || info.Size != size || info.SHA256 != digest {
		t.Fatalf("Open() info = %#v, %v", info, err)
	}
	contents, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(contents, payload) {
		t.Fatalf("full download = %q, %v, %v", contents, readErr, closeErr)
	}
	reader, info, err = backend.Open(context.Background(), entry.ID, &ByteRange{Start: 2, End: 6})
	if err != nil || info.Size != size {
		t.Fatalf("Open(range) info = %#v, %v", info, err)
	}
	contents, readErr = io.ReadAll(reader)
	closeErr = reader.Close()
	if readErr != nil || closeErr != nil || string(contents) != string(payload[2:7]) {
		t.Fatalf("range download = %q, %v, %v", contents, readErr, closeErr)
	}
	if err := backend.Abort(context.Background(), reservation.ID); err != nil {
		t.Fatalf("Abort(committed) = %v", err)
	}
}

func TestAgentBackendDisabledAndErrorMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		writeAgentTestJSON(t, w, AgentCapability{Enabled: false, Mode: "disabled", Reason: "feature_not_enabled"})
	}))
	defer server.Close()
	backend, capability, err := NewAgentBackend(context.Background(), AgentConfig{
		Endpoint: server.URL + "/v3", JobID: "44444444-4444-4444-4444-444444444444", Token: "token", Client: server.Client(),
	})
	if err != nil || backend != nil || capability.Enabled || capability.Reason != "feature_not_enabled" {
		t.Fatalf("disabled NewAgentBackend() = %#v, %#v, %v", backend, capability, err)
	}

	for _, test := range []struct {
		action string
		status int
		want   error
	}{
		{action: "reserve", status: http.StatusConflict, want: ErrContention},
		{action: "commit", status: http.StatusConflict, want: ErrConflict},
		{status: http.StatusForbidden, want: ErrDenied},
		{status: http.StatusNotFound, want: ErrNotFound},
		{status: http.StatusRequestEntityTooLarge, want: ErrTooLarge},
		{status: http.StatusTooManyRequests, want: ErrRateLimit},
		{status: http.StatusServiceUnavailable, want: ErrUnavailable},
	} {
		if got := agentResponseError(test.action, test.status); !errors.Is(got, test.want) {
			t.Errorf("agentResponseError(%q, %d) = %v, want %v", test.action, test.status, got, test.want)
		}
	}
}

func TestAgentTransferInstructionsFailClosed(t *testing.T) {
	expiresAt := time.Now().Add(time.Minute)
	for _, test := range []agentInstruction{
		{Method: http.MethodPut, URL: "https://user@example.com/object", Headers: map[string]string{"Content-Length": "1"}, ExpiresAt: expiresAt},
		{Method: http.MethodPut, URL: "https://example.com/object", Headers: map[string]string{"Authorization": "secret", "Content-Length": "1"}, ExpiresAt: expiresAt},
		{Method: http.MethodPut, URL: "https://example.com/object", Headers: map[string]string{"Content-Length": "2"}, ExpiresAt: expiresAt},
		{Method: http.MethodGet, URL: "https://example.com/object", ExpiresAt: expiresAt},
	} {
		method := http.MethodPut
		if test.Method == http.MethodGet {
			method = http.MethodPut
		}
		if _, err := agentTransferRequest(context.Background(), test, method, strings.NewReader("x"), 1, nil); err == nil {
			t.Fatalf("agentTransferRequest(%#v) succeeded", test)
		}
	}
	if _, err := agentCacheURL("https://user@example.com/v3", "job"); err == nil {
		t.Fatal("agentCacheURL accepted URL credentials")
	}
	zero, err := agentTransferRequest(context.Background(), agentInstruction{
		Method: http.MethodPut, URL: "https://example.com/object",
		Headers: map[string]string{"Content-Length": "0"}, ExpiresAt: expiresAt,
	}, http.MethodPut, bytes.NewReader(nil), 0, nil)
	if err != nil || zero.Body != http.NoBody || zero.ContentLength != 0 {
		t.Fatalf("zero-size upload request = %#v, %v", zero, err)
	}
}

func agentTestEntry(id string, scope Scope, digest string, size int64, createdAt, expiresAt time.Time) agentEntry {
	return agentEntry{
		EntryID: id, Scope: string(scope), Key: "cache-key", Version: "v1", Size: size,
		SHA256: digest, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
}

func decodeAgentTestJSON(t *testing.T, request *http.Request, target any) {
	t.Helper()
	if err := json.NewDecoder(request.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func writeAgentTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
