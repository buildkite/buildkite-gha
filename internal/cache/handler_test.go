package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testHandler(t *testing.T) (*Handler, *memoryBackend, *httptest.Server) {
	t.Helper()
	b := newTestMemoryBackend(func() time.Time { return time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC) })
	cfg := Config{Token: "token", Session: "job", TempDir: t.TempDir(), MaxArchive: 1024, MaxChunk: 1024, BaseURL: "http://example.test/"}
	h, err := NewHandler(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(func() {
		s.Close()
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h, b, s
}
func request(t *testing.T, s *httptest.Server, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Accept", mediaType)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func status(t *testing.T, res *http.Response, want int) {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != want {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d, want %d: %s", res.StatusCode, want, b)
	}
}

func reserveCache(t *testing.T, server *httptest.Server, key, version string, size int64) int64 {
	t.Helper()
	res := request(t, server, http.MethodPost, "/_apis/artifactcache/caches", fmt.Sprintf(`{"key":%q,"version":%q,"cacheSize":%d}`, key, version, size), nil)
	if res.StatusCode != http.StatusCreated {
		status(t, res, http.StatusCreated)
	}
	defer func() { _ = res.Body.Close() }()
	var reservation struct{ CacheID int64 }
	if err := json.NewDecoder(res.Body).Decode(&reservation); err != nil || reservation.CacheID <= 0 {
		t.Fatalf("reservation = %#v, %v", reservation, err)
	}
	return reservation.CacheID
}

func patchCache(t *testing.T, server *httptest.Server, cacheID, start int64, body string) *http.Response {
	t.Helper()
	return request(t, server, http.MethodPatch, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), body, map[string]string{
		"Content-Type":  "application/octet-stream",
		"Content-Range": fmt.Sprintf("bytes %d-%d/*", start, start+int64(len(body))-1),
	})
}

func TestProtocolRejectsNonUTF8LookupValues(t *testing.T) {
	_, _, server := testHandler(t)
	status(t, request(t, server, http.MethodGet, "/_apis/artifactcache/cache?keys=%FF&version=v1", "", nil), http.StatusBadRequest)
	status(t, request(t, server, http.MethodGet, "/_apis/artifactcache/cache?keys=key&version=%FF", "", nil), http.StatusBadRequest)
}

func TestProtocolSaveRestoreAndRanges(t *testing.T) {
	_, _, s := testHandler(t)
	res := request(t, s, "POST", "/_apis/artifactcache/caches", `{"key":"linux-abc","version":"opaque","cacheSize":6}`, nil)
	if res.StatusCode != 201 {
		status(t, res, 201)
	}
	var rr struct{ CacheID int64 }
	if err := json.NewDecoder(res.Body).Decode(&rr); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	// Out of order, then identical replay.
	status(t, request(t, s, "PATCH", fmt.Sprintf("/_apis/artifactcache/caches/%d", rr.CacheID), "def", map[string]string{"Content-Type": "application/octet-stream", "Content-Range": "bytes 3-5/*"}), 204)
	status(t, request(t, s, "PATCH", fmt.Sprintf("/_apis/artifactcache/caches/%d", rr.CacheID), "abc", map[string]string{"Content-Type": "application/octet-stream", "Content-Range": "bytes 0-2/*"}), 204)
	status(t, request(t, s, "PATCH", fmt.Sprintf("/_apis/artifactcache/caches/%d", rr.CacheID), "abc", map[string]string{"Content-Type": "application/octet-stream", "Content-Range": "bytes 0-2/*"}), 204)
	status(t, request(t, s, "PATCH", fmt.Sprintf("/_apis/artifactcache/caches/%d", rr.CacheID), "X", map[string]string{"Content-Type": "application/octet-stream", "Content-Range": "bytes 1-1/*"}), 409)
	status(t, request(t, s, "GET", "/_apis/artifactcache/cache?keys=linux-abc&version=opaque", "", nil), 204)
	status(t, request(t, s, "POST", fmt.Sprintf("/_apis/artifactcache/caches/%d", rr.CacheID), `{"size":6}`, nil), 204)
	status(t, request(t, s, "POST", fmt.Sprintf("/_apis/artifactcache/caches/%d", rr.CacheID), `{"size":6}`, nil), 204)
	status(t, request(t, s, "PATCH", fmt.Sprintf("/_apis/artifactcache/caches/%d", rr.CacheID), "X", map[string]string{"Content-Type": "application/octet-stream", "Content-Range": "bytes 1-1/*"}), 409)
	res = request(t, s, "GET", "/_apis/artifactcache/cache?keys="+url.QueryEscape("missing,linux-")+"&version=opaque", "", nil)
	if res.StatusCode != 200 {
		status(t, res, 200)
	}
	var hit struct{ CacheKey, ArchiveLocation string }
	if err := json.NewDecoder(res.Body).Decode(&hit); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if hit.CacheKey != "linux-abc" {
		t.Fatalf("key %q", hit.CacheKey)
	}
	path := strings.TrimPrefix(hit.ArchiveLocation, "http://example.test")
	res = request(t, s, "GET", path, "", map[string]string{"Authorization": "", "Accept": ""})
	full, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 200 || string(full) != "abcdef" || res.Header.Get("Content-Length") != "6" {
		t.Fatalf("download: %d %q %q", res.StatusCode, full, res.Header)
	}
	res = request(t, s, "GET", path, "", map[string]string{"Authorization": "", "Accept": "", "Range": "bytes=1-3"})
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 206 || string(b) != "bcd" || res.Header.Get("Content-Range") != "bytes 1-3/6" || res.Header.Get("Content-Length") != "3" {
		t.Fatalf("range: %d %q %q", res.StatusCode, b, res.Header)
	}
	res = request(t, s, "HEAD", path, "", map[string]string{"Authorization": "", "Accept": ""})
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 || res.Header.Get("Content-Length") != "6" {
		t.Fatalf("head: %d %q", res.StatusCode, res.Header)
	}
	for _, test := range []struct {
		rangeHeader, want string
	}{
		{rangeHeader: "bytes=-2", want: "ef"},
		{rangeHeader: "bytes=4-99", want: "ef"},
	} {
		res = request(t, s, "GET", path, "", map[string]string{"Authorization": "", "Accept": "", "Range": test.rangeHeader})
		got, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusPartialContent || string(got) != test.want {
			t.Fatalf("range %q = %d %q, want %q", test.rangeHeader, res.StatusCode, got, test.want)
		}
	}
}

func TestLookupFailsClosedWhenDownloadURLRedactionFails(t *testing.T) {
	backend := newTestMemoryBackend(nil)
	putMemoryEntry(t, backend, "key", "version", "owner", []byte("archive"))
	var redacted string
	handler, err := NewHandler(backend, Config{
		Token: "token", Session: "job", BaseURL: "http://example.test/", TempDir: t.TempDir(),
		RegisterRedaction: func(_ context.Context, value string) error {
			redacted = value
			return errors.New("redactor unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
		if err := handler.Close(); err != nil {
			t.Error(err)
		}
	})

	status(t, request(t, server, http.MethodGet, "/_apis/artifactcache/cache?keys=key&version=version", "", nil), http.StatusServiceUnavailable)
	if !strings.HasPrefix(redacted, "http://example.test/downloads/") {
		t.Fatalf("registered redaction = %q", redacted)
	}
	handler.mu.Lock()
	downloads := len(handler.downloads)
	handler.mu.Unlock()
	if downloads != 0 {
		t.Fatalf("download capabilities after redaction failure = %d, want 0", downloads)
	}
}

func TestLookupUsesOnlyConfiguredRequestHostForDownloadURL(t *testing.T) {
	backend := newTestMemoryBackend(nil)
	putMemoryEntry(t, backend, "key", "version", "owner", []byte("archive"))
	handler, err := NewHandler(backend, Config{
		Token: "token", Session: "job", BaseURL: "http://127.0.0.1:1234/", ContainerBaseURL: "http://buildkite-gha.internal:1234/", TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
		if err := handler.Close(); err != nil {
			t.Error(err)
		}
	})

	lookup := func(host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, server.URL+"/_apis/artifactcache/cache?keys=key&version=version", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("Accept", mediaType)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	for _, test := range []struct {
		host, wantBase string
	}{
		{host: "127.0.0.1:1234", wantBase: "http://127.0.0.1:1234/downloads/"},
		{host: "buildkite-gha.internal:1234", wantBase: "http://buildkite-gha.internal:1234/downloads/"},
	} {
		response := lookup(test.host)
		if response.StatusCode != http.StatusOK {
			status(t, response, http.StatusOK)
		}
		var hit struct{ ArchiveLocation string }
		if err := json.NewDecoder(response.Body).Decode(&hit); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if !strings.HasPrefix(hit.ArchiveLocation, test.wantBase) {
			t.Fatalf("host %q archive location = %q, want prefix %q", test.host, hit.ArchiveLocation, test.wantBase)
		}
	}
	status(t, lookup("attacker.invalid:1234"), http.StatusBadRequest)
}

func TestValidationAuthMissAndFaults(t *testing.T) {
	_, b, s := testHandler(t)
	req, _ := http.NewRequest("GET", s.URL+"/_apis/artifactcache/cache?keys=k&version=v", nil)
	res, _ := http.DefaultClient.Do(req)
	status(t, res, 401)
	res = request(t, s, "GET", "/_apis/artifactcache/cache?keys=k&version=v", "", nil)
	status(t, res, 204)
	b.mu.Lock()
	b.faults["lookup"] = ErrUnavailable
	b.mu.Unlock()
	res = request(t, s, "GET", "/_apis/artifactcache/cache?keys=k&version=v", "", nil)
	status(t, res, 503)
	res = request(t, s, "POST", "/_apis/artifactcache/caches", `{"key":"k","version":"v","cacheSize":2048}`, nil)
	status(t, res, 413)
}

func TestProtocolValidationAndCommitCoverage(t *testing.T) {
	_, _, server := testHandler(t)
	tests := []struct {
		name, method, path, body string
		headers                  map[string]string
		want                     int
	}{
		{name: "missing accept", method: http.MethodGet, path: "/_apis/artifactcache/cache?keys=k&version=v", headers: map[string]string{"Accept": ""}, want: 400},
		{name: "empty candidate", method: http.MethodGet, path: "/_apis/artifactcache/cache?keys=k%2C%2Crestore&version=v", want: 400},
		{name: "too many candidates", method: http.MethodGet, path: "/_apis/artifactcache/cache?keys=a%2Cb%2Cc%2Cd%2Ce%2Cf%2Cg%2Ch%2Ci%2Cj%2Ck&version=v", want: 400},
		{name: "empty list key", method: http.MethodGet, path: "/_apis/artifactcache/caches?key=", want: 400},
		{name: "wrong reserve content type", method: http.MethodPost, path: "/_apis/artifactcache/caches", body: `{"key":"k","version":"v","cacheSize":1}`, headers: map[string]string{"Content-Type": "text/plain"}, want: 400},
		{name: "trailing JSON", method: http.MethodPost, path: "/_apis/artifactcache/caches", body: `{"key":"k","version":"v","cacheSize":1}{}`, want: 400},
		{name: "unknown reservation", method: http.MethodPost, path: "/_apis/artifactcache/caches/999", body: `{"size":1}`, want: 404},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status(t, request(t, server, test.method, test.path, test.body, test.headers), test.want)
		})
	}

	cacheID := reserveCache(t, server, "coverage", "v1", 6)
	status(t, patchCache(t, server, cacheID, 0, "abc"), 204)
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), `{"size":6}`, nil), 409)
	status(t, request(t, server, http.MethodPatch, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), "de", map[string]string{
		"Content-Type": "application/octet-stream", "Content-Range": "bytes 3-5/*",
	}), 400)
	status(t, request(t, server, http.MethodPatch, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), "def", map[string]string{
		"Content-Type": "application/octet-stream", "Content-Range": "bytes 3-5/*junk",
	}), 400)

	declaredID := reserveCache(t, server, "declared", "v1", 6)
	status(t, patchCache(t, server, declaredID, 0, "abcdef"), 204)
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", declaredID), `{"size":5}`, nil), 409)
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", declaredID), `{"size":6}`, nil), 204)

	oversizeID := reserveCache(t, server, "oversize", "v1", 1024)
	status(t, request(t, server, http.MethodPatch, fmt.Sprintf("/_apis/artifactcache/caches/%d", oversizeID), strings.Repeat("x", 1025), map[string]string{
		"Content-Type": "application/octet-stream", "Content-Range": "bytes 0-1024/*",
	}), 413)
}

func TestReadOnlyAndZeroByteCache(t *testing.T) {
	backend := newTestMemoryBackend(nil)
	readOnly, err := NewHandler(backend, Config{
		Token: "token", Session: "job", BaseURL: "http://example.test/", TempDir: t.TempDir(),
		ReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	readOnlyServer := httptest.NewServer(readOnly)
	status(t, request(t, readOnlyServer, http.MethodPost, "/_apis/artifactcache/caches", `{"key":"k","version":"v","cacheSize":1}`, nil), 403)
	readOnlyServer.Close()
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, server := testHandler(t)
	cacheID := reserveCache(t, server, "empty", "v1", 0)
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), `{"size":0}`, nil), 204)
	res := request(t, server, http.MethodGet, "/_apis/artifactcache/cache?keys=empty&version=v1", "", nil)
	var hit struct{ ArchiveLocation string }
	if err := json.NewDecoder(res.Body).Decode(&hit); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	path := strings.TrimPrefix(hit.ArchiveLocation, "http://example.test")
	res = request(t, server, http.MethodGet, path, "", map[string]string{"Authorization": "", "Accept": ""})
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || len(body) != 0 || res.Header.Get("Content-Length") != "0" {
		t.Fatalf("zero-byte download = %d %q %q", res.StatusCode, body, res.Header)
	}
}

type blockingReserveBackend struct {
	Backend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReserveBackend) Reserve(ctx context.Context, request ReserveRequest) (Reservation, error) {
	close(b.entered)
	<-b.release
	return b.Backend.Reserve(ctx, request)
}

func (b *blockingReserveBackend) Abort(ctx context.Context, id ReservationID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.Backend.Abort(ctx, id)
}

func TestCloseRacingReserveAbortsWithoutLeakingTempFile(t *testing.T) {
	tempDir := t.TempDir()
	memory := newTestMemoryBackend(nil)
	backend := &blockingReserveBackend{Backend: memory, entered: make(chan struct{}), release: make(chan struct{})}
	handler, err := NewHandler(backend, Config{
		Token: "token", Session: "job", BaseURL: "http://example.test/", TempDir: tempDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/_apis/artifactcache/caches", strings.NewReader(`{"key":"race","version":"v","cacheSize":1}`))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Accept", mediaType)
	request.Header.Set("Content-Type", "application/json")
	requestContext, cancelRequest := context.WithCancel(request.Context())
	request = request.WithContext(requestContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(recorder, request)
		close(done)
	}()
	<-backend.entered
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	close(backend.release)
	<-done
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("response status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}

	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("temporary files after close = %v", files)
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	for _, reservation := range memory.reservations {
		if !reservation.aborted {
			t.Fatalf("reservation %q was not aborted", reservation.ID)
		}
	}
}

func TestBackendFaultsMapToUnavailableAndRetrySafely(t *testing.T) {
	_, backend, server := testHandler(t)

	backend.mu.Lock()
	backend.faults["reserve"] = ErrUnavailable
	backend.mu.Unlock()
	status(t, request(t, server, http.MethodPost, "/_apis/artifactcache/caches", `{"key":"reserve-fault","version":"v","cacheSize":1}`, nil), 503)
	backend.mu.Lock()
	delete(backend.faults, "reserve")
	backend.faults["list"] = ErrUnavailable
	backend.mu.Unlock()
	status(t, request(t, server, http.MethodGet, "/_apis/artifactcache/caches?key=k", "", nil), 503)
	backend.mu.Lock()
	delete(backend.faults, "list")
	backend.mu.Unlock()

	cacheID := reserveCache(t, server, "retry", "v1", 3)
	status(t, patchCache(t, server, cacheID, 0, "abc"), 204)
	backend.mu.Lock()
	backend.faults["upload"] = ErrUnavailable
	backend.mu.Unlock()
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), `{"size":3}`, nil), 503)
	backend.mu.Lock()
	delete(backend.faults, "upload")
	backend.faults["commit"] = ErrUnavailable
	backend.mu.Unlock()
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), `{"size":3}`, nil), 503)
	backend.mu.Lock()
	delete(backend.faults, "commit")
	backend.mu.Unlock()
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), `{"size":3}`, nil), 204)

	res := request(t, server, http.MethodGet, "/_apis/artifactcache/cache?keys=retry&version=v1", "", nil)
	var hit struct{ ArchiveLocation string }
	if err := json.NewDecoder(res.Body).Decode(&hit); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	backend.mu.Lock()
	backend.faults["open"] = ErrUnavailable
	backend.mu.Unlock()
	path := strings.TrimPrefix(hit.ArchiveLocation, "http://example.test")
	status(t, request(t, server, http.MethodGet, path, "", map[string]string{"Authorization": "", "Accept": ""}), 503)
}

func TestConcurrentOutOfOrderChunks(t *testing.T) {
	_, _, server := testHandler(t)
	const contents = "abcdefghijklmnop"
	cacheID := reserveCache(t, server, "parallel", "v1", int64(len(contents)))
	start := make(chan struct{})
	errs := make(chan error, len(contents))
	var wait sync.WaitGroup
	for offset := range len(contents) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			path := fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID)
			req, err := http.NewRequest(http.MethodPatch, server.URL+path, strings.NewReader(contents[offset:offset+1]))
			if err == nil {
				req.Header.Set("Authorization", "Bearer token")
				req.Header.Set("Accept", mediaType)
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", offset, offset))
				var response *http.Response
				response, err = http.DefaultClient.Do(req)
				if err == nil {
					_ = response.Body.Close()
					if response.StatusCode != http.StatusNoContent {
						err = fmt.Errorf("PATCH %d status %d", offset, response.StatusCode)
					}
				}
			}
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	status(t, request(t, server, http.MethodPost, fmt.Sprintf("/_apis/artifactcache/caches/%d", cacheID), fmt.Sprintf(`{"size":%d}`, len(contents)), nil), 204)
}

func TestConcurrentReservationSingleIdentity(t *testing.T) {
	_, _, s := testHandler(t)
	const n = 20
	ids := make(chan int64, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := request(t, s, "POST", "/_apis/artifactcache/caches", `{"key":"k","version":"v","cacheSize":1}`, nil)
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != 201 {
				t.Errorf("status %d", res.StatusCode)
				return
			}
			var x struct{ CacheID int64 }
			if err := json.NewDecoder(res.Body).Decode(&x); err != nil {
				t.Errorf("decode reservation: %v", err)
				return
			}
			ids <- x.CacheID
		}()
	}
	wg.Wait()
	close(ids)
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		if id != first {
			t.Errorf("IDs %d and %d", first, id)
		}
	}
}

type fixture struct {
	Metadata struct {
		Kind, Action, Commit, Version string
		Environment                   map[string]any
	}
	Requests []struct {
		Method, Path, Body, ResponseBody string
		Headers                          map[string]string
		Status                           int
	}
}

func TestWireFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/actions-cache-v*.json")
	if len(files) != 2 {
		t.Fatalf("got %d fixtures", len(files))
	}
	for _, name := range files {
		t.Run(filepath.Base(name), func(t *testing.T) {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var f fixture
			if err = json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			if f.Metadata.Kind != "wire-fixture" || f.Metadata.Environment["ACTIONS_CACHE_SERVICE_V2"] != nil || f.Metadata.Environment["ACTIONS_RESULTS_URL"] != nil {
				t.Fatal("fixture does not assert v1 environment")
			}
			_, _, s := testHandler(t)
			var cacheID int64
			var downloadPath string
			for _, step := range f.Requests {
				path := strings.ReplaceAll(step.Path, "{cacheId}", strconv.FormatInt(cacheID, 10))
				if path == "{archiveLocation}" {
					path = downloadPath
				}
				res := request(t, s, step.Method, path, step.Body, step.Headers)
				if res.StatusCode != step.Status {
					status(t, res, step.Status)
				}
				body, err := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if step.ResponseBody != "" && string(body) != step.ResponseBody {
					t.Fatalf("response body = %q, want %q", body, step.ResponseBody)
				}
				if step.Method == http.MethodPost && step.Path == "/_apis/artifactcache/caches" && step.Status == http.StatusCreated {
					var reservation struct{ CacheID int64 }
					if err := json.Unmarshal(body, &reservation); err != nil || reservation.CacheID <= 0 {
						t.Fatalf("reservation response = %q: %v", body, err)
					}
					cacheID = reservation.CacheID
				}
				if step.Method == http.MethodGet && strings.HasPrefix(step.Path, "/_apis/artifactcache/cache?") && step.Status == http.StatusOK {
					var hit struct{ ArchiveLocation string }
					if err := json.Unmarshal(body, &hit); err != nil {
						t.Fatal(err)
					}
					downloadPath = strings.TrimPrefix(hit.ArchiveLocation, "http://example.test")
				}
			}
		})
	}
}
