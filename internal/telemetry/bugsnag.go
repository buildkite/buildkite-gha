package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	bugsnag "github.com/bugsnag/bugsnag-go/v2"
	bugsnagerrors "github.com/bugsnag/bugsnag-go/v2/errors"
	stackerrors "github.com/pkg/errors"
)

// ReportError submits a best-effort, asynchronous error report. It does not send
// error text or causes: those may contain secrets not covered by log redaction.
// Ordinary workflow failures and cancellation are not product defects.
func ReportError(ctx context.Context, command Command, version string, details Details, err error) {
	client, configErr := New(Config{
		Endpoint:      os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
		JobID:         os.Getenv("BUILDKITE_JOB_ID"),
		JobToken:      os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
		ClientVersion: version,
		Disabled:      os.Getenv("BUILDKITE_GHA_TELEMETRY_DISABLED") == "true",
	})
	if configErr != nil || client == nil {
		return
	}
	reportBugsnagError(ctx, client, command, version, details, err)
}

func reportBugsnagError(ctx context.Context, client *Client, command Command, version string, details Details, err error) {
	if client == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	switch details.FailureCode {
	case "", FailureCodeUnknown, FailureCodeRuntimeIntegrity, FailureCodeWorkflowToken, FailureCodeOIDCToken, FailureCodeCacheCredential:
	default:
		return
	}
	if !validCommand(command) || !validFailurePhase(details.FailurePhase) {
		return
	}
	if details.FailureCode == "" {
		details.FailureCode = FailureCodeUnknown
	}
	deadline, cancel := context.WithTimeout(context.WithoutCancel(ctx), client.timeout)
	submitted := false
	defer func() {
		if !submitted {
			cancel()
		}
	}()
	releaseStage := "production"
	if version == "dev" || strings.HasPrefix(version, "dev+") {
		releaseStage = "development"
	}
	metadata := bugsnag.MetaData{"failure": {
		"command": command, "phase": details.FailurePhase, "code": details.FailureCode,
	}}
	if details.AgentAPIHTTPStatus >= 100 && details.AgentAPIHTTPStatus <= 599 {
		metadata["failure"]["agent_api_http_status"] = details.AgentAPIHTTPStatus
	}
	prepared := &bugsnagTransport{}
	notifier := &bugsnag.Notifier{Config: &bugsnag.Configuration{
		// The SDK requires 32 characters to serialize a report. This is not
		// an ingestion key and is removed before submission.
		APIKey:          "00000000000000000000000000000000",
		Endpoints:       bugsnag.Endpoints{Notify: client.errorReportsURL},
		AppType:         "buildkite-gha",
		AppVersion:      boundedClientVersion(version),
		ReleaseStage:    releaseStage,
		ProjectPackages: []string{"github.com/buildkite/buildkite-gha/**"},
		Logger:          log.New(io.Discard, "", 0),
		Transport:       prepared,
	}}
	// Capture the reporting boundary, excluding this helper and ReportError.
	// Prefer captured stacks, including those under wrappers or joins.
	var stack *bugsnagerrors.Error
	if captured := capturedError(err); captured != nil {
		stack = bugsnagerrors.New(captured, 0)
	} else {
		stack = bugsnagerrors.New(err, 2)
	}
	_ = notifier.NotifySync(stack, true,
		bugsnag.Context{String: string(command) + ":" + string(details.FailurePhase)},
		metadata,
		func(event *bugsnag.Event) {
			event.ErrorClass = fmt.Sprintf("%s/%s/%s: %s", command, details.FailurePhase, details.FailureCode, event.ErrorClass)
			event.Message = fmt.Sprintf("buildkite-gha %s failed during %s", command, details.FailurePhase)
			// Captured filenames may include customer checkout or build paths.
			// Keep methods and line numbers without transmitting filenames.
			for i := range event.Stacktrace {
				event.Stacktrace[i].File = "[REDACTED]"
			}
			// The SDK otherwise serializes every cause's unredacted message.
			event.Error = nil
		},
	)
	if len(prepared.body) == 0 || len(prepared.body) > 512<<10 || deadline.Err() != nil || !pendingErrorReports.start(deadline) {
		return
	}
	submitted = true
	go func() {
		defer pendingErrorReports.finish(deadline)
		defer cancel()
		_ = client.postJSON(deadline, client.errorReportsURL, prepared.body)
	}()
}

// capturedError returns the first stack-bearing error in depth-first order.
// The SDK only preserves stacks directly on the error passed to NotifySync.
func capturedError(err error) error {
	switch err := err.(type) {
	case bugsnagerrors.ErrorWithCallers, bugsnagerrors.ErrorWithStackFrames, interface{ StackTrace() stackerrors.StackTrace }:
		return err
	case interface{ Unwrap() []error }:
		for _, child := range err.Unwrap() {
			if captured := capturedError(child); captured != nil {
				return captured
			}
		}
	case interface{ Unwrap() error }:
		return capturedError(err.Unwrap())
	}
	return nil
}

type bugsnagTransport struct {
	body []byte
}

// RoundTrip captures the SDK's fully serialized and sanitized report. No SDK
// request or Bugsnag header is sent to the network.
func (t *bugsnagTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		return nil, err
	}
	delete(payload, "apiKey")
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	t.body = body
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

var pendingErrorReports errorReports

type errorReports struct {
	mu      sync.Mutex
	pending map[context.Context]struct{}
	changed chan struct{}
}

func (r *errorReports) start(ctx context.Context) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Drop bursts instead of accumulating unbounded goroutines or a disk spool.
	if len(r.pending) == 16 {
		return false
	}
	if r.pending == nil {
		r.pending = make(map[context.Context]struct{})
	}
	if len(r.pending) == 0 {
		r.changed = make(chan struct{})
	}
	r.pending[ctx] = struct{}{}
	return true
}

func (r *errorReports) finish(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, ctx)
	close(r.changed)
	r.changed = make(chan struct{})
}

// DrainErrorReports waits for pending sends together, only within their original
// submission deadlines. Call after command completion and before os.Exit.
func DrainErrorReports() {
	for {
		pendingErrorReports.mu.Lock()
		var deadline time.Time
		for ctx := range pendingErrorReports.pending {
			if end, _ := ctx.Deadline(); end.After(deadline) {
				deadline = end
			}
		}
		changed := pendingErrorReports.changed
		pendingErrorReports.mu.Unlock()
		if time.Until(deadline) <= 0 {
			return
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-changed:
			timer.Stop()
		case <-timer.C:
			return
		}
	}
}
