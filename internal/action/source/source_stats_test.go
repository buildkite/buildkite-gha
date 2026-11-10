package source

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceStatsDistinguishThrottlingFromDenial(t *testing.T) {
	for _, operation := range []string{"tag_ref", "archive"} {
		for _, test := range []struct {
			name, body, retry, result string
			status                    int
			remaining, reset          string
			wantRemaining, wantReset  int64
		}{
			{"throttle", "private rate limit text", "60", "rate_limit", 403, "0", "1800000000", 0, 1800000000},
			{"denied", "private permission text", "", "denied", 403, "private-secret", "private-secret", -1, 0},
			{"missing", "private missing text", "", "not_found", 404, "-2", "-1", -1, 0},
			{"unavailable", "private service text", "", "http_error", 503, "9007199254740992", "9007199254740992", -1, 0},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", test.retry)
					w.Header().Set("X-RateLimit-Remaining", test.remaining)
					w.Header().Set("X-RateLimit-Reset", test.reset)
					w.Header().Set("X-Private-Header", "private-secret")
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.body))
				}))
				defer server.Close()
				ctx, stats := WithSourceStats(t.Context())
				options := []Option{WithTestEndpoints(server.URL, server.URL)}
				ref, _ := Parse("private-owner/private-repo@private-ref")
				if operation == "archive" {
					store, err := NewStore(t.TempDir(), server.Client(), options...)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.Materialize(ctx, Resolved{Reference: ref, Commit: testSHA}); err == nil {
						t.Fatal("expected archive failure")
					}
				} else {
					resolver, err := NewResolver(server.Client(), options...)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := resolver.Resolve(ctx, ref); err == nil {
						t.Fatal("expected resolution failure")
					}
					if test.result == "rate_limit" {
						_, _ = resolver.Resolve(ctx, ref)
						if stats.Snapshot().SuppressedRequests != 1 {
							t.Fatal("cooldown was counted as a request")
						}
					}
				}
				summary := stats.Snapshot()
				if len(summary.Requests) == 0 {
					t.Fatal("missing request")
				}
				wantBytes := int64(len(test.body))
				if operation == "archive" && test.status != 403 {
					wantBytes = 0 // The archive path does not consume these error bodies.
				}
				for _, request := range summary.Requests {
					if request.Status != test.status || request.Result != test.result || request.Count != 1 || request.Bytes != wantBytes {
						t.Fatalf("request = %#v", request)
					}
					if operation == "archive" && request.Operation != "archive" {
						t.Fatalf("operation = %s", request.Operation)
					}
					if test.retry != "" && request.RetryAfterSeconds != 60 {
						t.Fatalf("retry = %d", request.RetryAfterSeconds)
					}
					if request.RateLimitRemaining != test.wantRemaining || request.RateLimitReset != test.wantReset {
						t.Fatalf("rate limit hints = %d / %d", request.RateLimitRemaining, request.RateLimitReset)
					}
				}
				encoded, err := json.Marshal(summary)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"private", testSHA, server.URL} {
					if strings.Contains(string(encoded), secret) {
						t.Fatalf("source statistics leaked %q", secret)
					}
				}
			})
		}
	}
}
