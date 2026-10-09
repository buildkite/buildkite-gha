package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	bugsnag "github.com/bugsnag/bugsnag-go/v2"
	bugsnagerrors "github.com/bugsnag/bugsnag-go/v2/errors"
	stackerrors "github.com/pkg/errors"
)

// BugsnagAPIKey is optionally set at build time with -ldflags -X. It is an
// ingestion key, visible to anyone who can read the distributed binary.
var BugsnagAPIKey string

// ReportError sends a best-effort, synchronous error report. It does not send
// error text or causes: those may contain secrets not covered by log redaction.
// Ordinary workflow failures and cancellation are not product defects.
func ReportError(ctx context.Context, command Command, version string, details Details, err error) {
	key := BugsnagAPIKey
	if value := os.Getenv("BUGSNAG_API_KEY"); value != "" {
		key = value
	}
	if os.Getenv("BUILDKITE_GHA_TELEMETRY_DISABLED") == "true" {
		return
	}
	reportBugsnagError(ctx, key, command, version, details, err, http.DefaultTransport)
}

func reportBugsnagError(ctx context.Context, key string, command Command, version string, details Details, err error, transport http.RoundTripper) {
	if len(key) != 32 || strings.ContainsAny(key, "\r\n") || err == nil || errors.Is(err, context.Canceled) {
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
	deadline, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultTimeout)
	defer cancel()
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
	notifier := &bugsnag.Notifier{Config: &bugsnag.Configuration{
		APIKey:          key,
		Endpoints:       bugsnag.Endpoints{Notify: "https://notify.bugsnag.com"},
		AppType:         "buildkite-gha",
		AppVersion:      boundedClientVersion(version),
		ReleaseStage:    releaseStage,
		ProjectPackages: []string{"github.com/buildkite/buildkite-gha/**"},
		Logger:          log.New(io.Discard, "", 0),
		Transport:       bugsnagTransport{ctx: deadline, base: transport},
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
	ctx  context.Context
	base http.RoundTripper
}

func (t bugsnagTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request.Clone(t.ctx))
	if err == nil && response.StatusCode >= 300 && response.StatusCode < 400 {
		// The SDK's HTTP client follows redirects. Never forward the payload
		// (which contains the ingestion key) to another destination.
		_ = response.Body.Close()
		return nil, fmt.Errorf("bugsnag redirects are disabled")
	}
	return response, err
}
