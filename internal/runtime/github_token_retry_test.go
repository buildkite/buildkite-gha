package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"
)

type workflowTokenTransport func(*http.Request) (*http.Response, error)

func (f workflowTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func workflowTokenTestProvider(t *testing.T, transport workflowTokenTransport) *AgentGitHubTokens {
	t.Helper()
	provider, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{
		Endpoint: "https://agent.example/v3", JobID: testCacheJobID,
		JobToken: "job-secret", ClientVersion: "1.2.3",
		Client: &http.Client{Transport: transport, Timeout: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func workflowTokenTestResponse(status int, retryAfter, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Retry-After": {retryAfter}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

type workflowTokenTestBody struct {
	io.Reader
	closed bool
}

func (b *workflowTokenTestBody) Close() error { b.closed = true; return nil }

func TestWorkflowTokenRetriesPreserveAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var starts []time.Time
		var previousBody *workflowTokenTestBody
		permissions := map[string]string{"contents": "read", "pull_requests": "write"}
		provider := workflowTokenTestProvider(t, func(r *http.Request) (*http.Response, error) {
			if previousBody != nil && !previousBody.closed {
				t.Error("retry started before closing the previous response")
			}
			starts = append(starts, time.Now())
			if r.Method != http.MethodPost || r.URL.String() != "https://agent.example/v3/jobs/"+testCacheJobID+"/github_workflow_access_token" {
				t.Errorf("unexpected request target: %s %s", r.Method, r.URL)
			}
			if r.Header.Get("Authorization") != "Token job-secret" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "buildkite-gha/1.2.3" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("request headers changed")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != `{"repo_url":"https://github.com/buildkite/buildkite-gha","workflow":"ci.yml","permissions":{"contents":"read","pull_requests":"write"}}` {
				t.Errorf("request authority changed: %s", body)
			}
			// The caller's map must not change the authority of a later attempt.
			permissions["contents"] = "write"
			response := workflowTokenTestResponse(200, "", `{"token":"ghs_success"}`)
			if len(starts) < 3 {
				response = workflowTokenTestResponse(503, "", "ghs_do_not_expose")
			}
			previousBody = &workflowTokenTestBody{Reader: response.Body}
			response.Body = previousBody
			return response, nil
		})
		token, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", permissions)
		if err != nil || token != "ghs_success" || len(starts) != 3 {
			t.Fatalf("token = %q, error = %v, requests = %d", token, err, len(starts))
		}
		if !previousBody.closed {
			t.Error("successful response was not closed")
		}
		for i, bounds := range [][2]time.Duration{{time.Second, 2 * time.Second}, {2 * time.Second, 4 * time.Second}} {
			if delay := starts[i+1].Sub(starts[i]); delay < bounds[0] || delay >= bounds[1] {
				t.Errorf("retry %d delay = %s, want [%s, %s)", i+1, delay, bounds[0], bounds[1])
			}
		}
	})
}

func TestWorkflowTokenRetryEligibilityAndTiming(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		retryAfter string
		requests   int
		minimum    time.Duration
	}{
		{"unavailable", 503, "", 3, time.Second},
		{"invalid unavailable delay", 503, "private-header", 3, time.Second},
		{"rate limit seconds", 429, "5", 3, 5 * time.Second},
		{"zero", 429, "0", 3, time.Second},
		{"leading zeros", 429, " 00005 ", 3, 5 * time.Second},
		{"HTTP date", 503, "date", 3, 5 * time.Second},
		{"past date", 429, "past", 3, time.Second},
		{"no rate limit delay", 429, "", 1, 0},
		{"invalid rate limit delay", 429, "private-header", 1, 0},
		{"negative", 429, "-1", 1, 0},
		{"signed", 429, "+1", 1, 0},
		{"fraction", 429, "0.1", 1, 0},
		{"just inside budget", 503, "44", 2, 44 * time.Second},
		{"budget boundary", 503, "45", 1, 0},
		{"long rate limit", 429, "60", 1, 0},
		{"duration overflow", 503, "9223372036854775807", 1, 0},
		{"integer overflow", 503, "18446744073709551616", 1, 0},
		{"far future date", 503, "Fri, 31 Dec 9999 23:59:59 GMT", 1, 0},
		{"old server refusal", 400, "1", 1, 0},
		{"unauthorized", 401, "1", 1, 0},
		{"policy denial", 403, "1", 1, 0},
		{"not found", 404, "1", 1, 0},
		{"unknown server error", 500, "1", 1, 0},
		{"gateway error", 502, "1", 1, 0},
		{"gateway timeout", 504, "1", 1, 0},
		{"redirect", 307, "1", 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var starts []time.Time
				provider := workflowTokenTestProvider(t, func(*http.Request) (*http.Response, error) {
					starts = append(starts, time.Now())
					header := test.retryAfter
					switch header {
					case "date":
						header = time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
					case "past":
						header = start.Add(-time.Minute).UTC().Format(http.TimeFormat)
					}
					return workflowTokenTestResponse(test.status, header, "ghs_private-body"), nil
				})
				_, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
				if err == nil || len(starts) != test.requests || ClassifyFailure(err) != FailureClassWorkflowToken {
					t.Fatalf("error = %v, requests = %d, want %d with workflow failure", err, len(starts), test.requests)
				}
				if status, ok := AgentAPIHTTPStatus(err); !ok || status != test.status {
					t.Errorf("status = %d, %t, want %d", status, ok, test.status)
				}
				if strings.Contains(err.Error(), "ghs_private-body") || strings.Contains(err.Error(), "private-header") || strings.Contains(err.Error(), "job-secret") {
					t.Error("credential data leaked in error")
				}
				if test.requests == 1 && time.Since(start) != 0 {
					t.Error("terminal failure waited")
				}
				for i := 1; i < len(starts); i++ {
					// HTTP dates have one-second resolution.
					minimum := test.minimum
					if test.retryAfter == "date" {
						minimum -= time.Second
					}
					if starts[i].Sub(starts[i-1]) < minimum {
						t.Errorf("retry %d did not honor minimum %s", i, minimum)
					}
				}
			})
		})
	}
}

func TestWorkflowTokenDeadlinesAndCancellation(t *testing.T) {
	for _, mode := range []string{"cancel before", "cancel request", "cancel backoff", "caller deadline", "attempt deadline", "overall deadline", "wait exceeds remaining budget", "wait exceeds caller budget"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "caller deadline" {
					var cancelDeadline context.CancelFunc
					ctx, cancelDeadline = context.WithTimeout(ctx, 500*time.Millisecond)
					defer cancelDeadline()
				}
				if mode == "wait exceeds caller budget" {
					var cancelDeadline context.CancelFunc
					ctx, cancelDeadline = context.WithTimeout(ctx, 10*time.Second)
					defer cancelDeadline()
				}
				if mode == "cancel before" {
					cancel()
				}
				requests := 0
				provider := workflowTokenTestProvider(t, func(r *http.Request) (*http.Response, error) {
					requests++
					switch mode {
					case "cancel request":
						cancel()
					case "cancel backoff":
						time.AfterFunc(500*time.Millisecond, cancel)
						return workflowTokenTestResponse(503, "", ""), nil
					case "overall deadline":
						if requests == 1 {
							return workflowTokenTestResponse(503, "35", ""), nil
						}
					case "wait exceeds remaining budget":
						time.Sleep(10 * time.Second)
						return workflowTokenTestResponse(503, "35", ""), nil
					case "wait exceeds caller budget":
						time.Sleep(2 * time.Second)
						return workflowTokenTestResponse(503, "8", ""), nil
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})
				_, err := provider.WorkflowToken(ctx, "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
				wantRequests, wantElapsed := 1, time.Duration(0)
				wantClass, wantError := FailureClassUnknown, error(context.Canceled)
				switch mode {
				case "cancel before":
					wantRequests = 0
				case "cancel backoff":
					wantElapsed = 500 * time.Millisecond
				case "caller deadline":
					wantElapsed, wantError = 500*time.Millisecond, context.DeadlineExceeded
				case "attempt deadline":
					wantElapsed, wantClass, wantError = 15*time.Second, FailureClassWorkflowToken, context.DeadlineExceeded
				case "overall deadline":
					wantRequests, wantElapsed, wantClass, wantError = 2, 45*time.Second, FailureClassWorkflowToken, context.DeadlineExceeded
				case "wait exceeds remaining budget":
					wantElapsed, wantClass, wantError = 10*time.Second, FailureClassWorkflowToken, nil
					if status, ok := AgentAPIHTTPStatus(err); !ok || status != 503 {
						t.Errorf("status = %d, %t, want final 503", status, ok)
					}
				case "wait exceeds caller budget":
					wantElapsed, wantClass, wantError = 2*time.Second, FailureClassWorkflowToken, nil
				}
				if err == nil || (wantError != nil && !errors.Is(err, wantError)) || requests != wantRequests || time.Since(start) != wantElapsed || ClassifyFailure(err) != wantClass {
					t.Fatalf("error=%v class=%s requests=%d elapsed=%s; want error=%v class=%s requests=%d elapsed=%s", err, ClassifyFailure(err), requests, time.Since(start), wantError, wantClass, wantRequests, wantElapsed)
				}
			})
		})
	}
}

func TestWorkflowTokenDoesNotRetryTransportOrInvalidSuccess(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		err  error
	}{
		{name: "transport EOF", err: io.EOF},
		{name: "transport timeout", err: context.DeadlineExceeded},
		{name: "invalid JSON", body: `{"token":`},
		{name: "unknown field", body: `{"token":"ghs_secret","private-field":true}`},
		{name: "trailing JSON", body: `{"token":"ghs_secret"}{}`},
		{name: "invalid token", body: `{"token":"private token"}`},
		{name: "empty token", body: `{"token":""}`},
		{name: "too large", body: strings.Repeat("x", 65537)},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			provider := workflowTokenTestProvider(t, func(*http.Request) (*http.Response, error) {
				requests++
				if test.err != nil {
					return nil, test.err
				}
				return workflowTokenTestResponse(200, "1", test.body), nil
			})
			_, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
			if err == nil || requests != 1 || ClassifyFailure(err) != FailureClassWorkflowToken {
				t.Fatalf("error = %v, requests = %d", err, requests)
			}
			if strings.Contains(err.Error(), "ghs_secret") || strings.Contains(err.Error(), "private") {
				t.Error("response data leaked")
			}
			if status, ok := AgentAPIHTTPStatus(err); ok || status != 0 {
				t.Errorf("unexpected HTTP failure status %d", status)
			}
		})
	}
}

func TestActionSourceTokenDoesNotRetry(t *testing.T) {
	for _, status := range []int{429, 503} {
		requests := 0
		provider := workflowTokenTestProvider(t, func(*http.Request) (*http.Response, error) {
			requests++
			return workflowTokenTestResponse(status, "0", ""), nil
		})
		_, err := provider.ActionSourceToken(t.Context(), "actions/checkout")
		if err == nil || requests != 1 {
			t.Fatalf("status %d: error = %v, requests = %d", status, err, requests)
		}
	}
}

func TestWorkflowTokenStopsAfterRetryReturnsTerminalFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		provider := workflowTokenTestProvider(t, func(*http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				return workflowTokenTestResponse(503, "", ""), nil
			}
			return workflowTokenTestResponse(400, "1", ""), nil
		})
		_, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
		if status, ok := AgentAPIHTTPStatus(err); !ok || status != 400 || requests != 2 {
			t.Fatalf("error = %v, status = %d, requests = %d", err, status, requests)
		}
	})
}

func TestWorkflowTokenDoesNotRetryResponseReadFailure(t *testing.T) {
	for _, status := range []int{200, 429, 503} {
		requests := 0
		body := &workflowTokenTestBody{Reader: iotest.ErrReader(io.ErrUnexpectedEOF)}
		provider := workflowTokenTestProvider(t, func(*http.Request) (*http.Response, error) {
			requests++
			response := workflowTokenTestResponse(status, "0", "")
			response.Body = body
			return response, nil
		})
		_, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
		if err == nil || requests != 1 || !body.closed || ClassifyFailure(err) != FailureClassWorkflowToken {
			t.Fatalf("HTTP %d: error=%v requests=%d closed=%t", status, err, requests, body.closed)
		}
	}
}

func TestWorkflowTokenAttemptDeadlineIncludesResponseBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		requests := 0
		provider := workflowTokenTestProvider(t, func(r *http.Request) (*http.Response, error) {
			requests++
			reader, writer := io.Pipe()
			go func() {
				<-r.Context().Done()
				_ = writer.CloseWithError(r.Context().Err())
			}()
			response := workflowTokenTestResponse(200, "", "")
			response.Body = reader
			return response, nil
		})
		_, err := provider.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
		if !errors.Is(err, context.DeadlineExceeded) || ClassifyFailure(err) != FailureClassWorkflowToken || requests != 1 || time.Since(start) != 15*time.Second {
			t.Fatalf("error=%v class=%s requests=%d elapsed=%s", err, ClassifyFailure(err), requests, time.Since(start))
		}
	})
}
