package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	bugsnagerrors "github.com/bugsnag/bugsnag-go/v2/errors"
)

const testBugsnagKey = "0123456789abcdef0123456789abcdef"

func TestBugsnagReportIsSynchronousAndOmitsSensitiveText(t *testing.T) {
	const buildID = "12345678-1234-1234-1234-123456789abc"
	const jobID = "abcdef12-abcd-abcd-abcd-abcdef123456"
	t.Setenv("BUILDKITE_BUILD_ID", buildID)
	t.Setenv("BUILDKITE_JOB_ID", jobID)
	t.Setenv("CUSTOMER_API_KEY", "private-api-key")
	var body []byte
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://notify.bugsnag.com" || request.Method != http.MethodPost {
			t.Fatalf("unexpected request destination: %s %s", request.Method, request.URL)
		}
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) > 1500*time.Millisecond {
			t.Fatal("delivery has no bounded deadline")
		}
		var err error
		body, err = io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	// The original stack is retained, but even wrapped causes must not leak.
	err := bugsnagerrors.New(fmt.Errorf("token=private-token: %w", errors.New("private-cause")), 0)
	for _, reportErr := range []error{
		err,
		fmt.Errorf("private-wrapper: %w", err),
		errors.Join(errors.New("private-sibling"), fmt.Errorf("private-wrapper: %w", err)),
	} {
		body = nil
		reportBugsnagError(t.Context(), testBugsnagKey, CommandRunJob, "0.9.2", Details{
			FailurePhase: FailurePhaseExecution, FailureCode: FailureCodeRuntimeIntegrity,
			ErrorMessage: "private-output", BlockerDetail: "private-config",
		}, reportErr, transport)
		if len(body) == 0 {
			t.Fatal("report did not complete before return")
		}
		for _, secret := range []string{"private-token", "private-cause", "private-output", "private-config", "private-wrapper", "private-sibling", "private-api-key", buildID, jobID} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("report leaked %s", secret)
			}
		}
		var payload struct {
			APIKey string
			Events []struct {
				Context    string
				App        struct{ Version string }
				Device     struct{ Hostname string }
				Exceptions []struct {
					Message    string
					Stacktrace []struct {
						Method     string
						File       string
						LineNumber int
						InProject  bool
					}
				}
				Metadata struct {
					Failure struct {
						Command string
						Code    string
						Phase   string
					}
				}
			}
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.APIKey != testBugsnagKey || len(payload.Events) != 1 {
			t.Fatal("missing key or error event")
		}
		event := payload.Events[0]
		if event.App.Version != "0.9.2" || event.Context != "run_job:execution" || event.Device.Hostname != "" {
			t.Fatalf("unexpected app/context/device: %#v", event)
		}
		if event.Metadata.Failure.Command != "run_job" || event.Metadata.Failure.Code != "E_RUNTIME_INTEGRITY" || event.Metadata.Failure.Phase != "execution" {
			t.Fatalf("missing failure classification: %#v", event.Metadata)
		}
		if len(event.Exceptions) != 1 || event.Exceptions[0].Message != "buildkite-gha run_job failed during execution" {
			t.Fatalf("unexpected exceptions: %#v", event.Exceptions)
		}
		stack := event.Exceptions[0].Stacktrace
		if len(stack) == 0 || !strings.Contains(stack[0].Method, "TestBugsnagReportIsSynchronousAndOmitsSensitiveText") || !stack[0].InProject {
			t.Fatalf("original project stack was lost: %#v", stack)
		}
		if stack[0].LineNumber <= 0 {
			t.Fatal("original stack line number was lost")
		}
		for _, frame := range stack {
			if frame.File != "[REDACTED]" {
				t.Fatalf("report leaked stack filename: %q", frame.File)
			}
		}
	}
}

func TestBugsnagGroupingIncludesFailureClassification(t *testing.T) {
	var classes []string
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Events []struct {
				Exceptions []struct{ ErrorClass string }
				Metadata   struct{ Failure struct{ Code string } }
			}
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		classes = append(classes, payload.Events[0].Exceptions[0].ErrorClass)
		if payload.Events[0].Metadata.Failure.Code == "" {
			t.Fatal("empty code was not normalized")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	// Identical wrappers and reporting location must not collapse categories.
	err := errors.Join(errors.New("private failure"))
	for _, code := range []FailureCode{FailureCodeRuntimeIntegrity, FailureCodeOIDCToken, ""} {
		reportBugsnagError(t.Context(), testBugsnagKey, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution, FailureCode: code}, err, transport)
	}
	want := []string{"run_job/execution/E_RUNTIME_INTEGRITY: *errors.joinError", "run_job/execution/E_OIDC_TOKEN_UNAVAILABLE: *errors.joinError", "run_job/execution/unknown: *errors.joinError"}
	if len(classes) != len(want) {
		t.Fatalf("got %d reports", len(classes))
	}
	for i := range want {
		if classes[i] != want[i] {
			t.Fatalf("class %d = %q, want %q", i, classes[i], want[i])
		}
	}
}

func TestBugsnagFiltersExpectedFailuresAndCancellation(t *testing.T) {
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected error report")
		return nil, nil
	})
	for _, code := range []FailureCode{FailureCodeStepProcessExit, FailureCodeUnsupportedFeature, FailureCodeWorkflowSyntax, FailureCodeSecretUnavailable} {
		reportBugsnagError(t.Context(), testBugsnagKey, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution, FailureCode: code}, errors.New("expected failure"), transport)
	}
	for _, key := range []string{"", "invalid"} {
		reportBugsnagError(t.Context(), key, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("defect"), transport)
	}
	reportBugsnagError(t.Context(), testBugsnagKey, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, fmt.Errorf("cancelled: %w", context.Canceled), transport)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reportBugsnagError(ctx, testBugsnagKey, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, ctx.Err(), transport)
}

func TestBugsnagDeliveryTimeoutAndRedirect(t *testing.T) {
	for _, mode := range []string{"timeout", "redirect", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				switch mode {
				case "timeout":
					<-request.Context().Done()
					return nil, request.Context().Err()
				case "redirect":
					return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": {"https://untrusted.example"}}}, nil
				default:
					return nil, errors.New("network unavailable")
				}
			})
			started := time.Now()
			reportBugsnagError(t.Context(), testBugsnagKey, CommandPluginImport, "dev", Details{FailurePhase: FailurePhasePipelineUpload}, errors.New("upload failed"), transport)
			if calls != 1 || time.Since(started) > 3*time.Second {
				t.Fatalf("delivery retried, redirected, or exceeded its budget: calls=%d duration=%s", calls, time.Since(started))
			}
		})
	}
}

func TestBugsnagBuildKeyOverrideAndOptOut(t *testing.T) {
	oldKey, oldTransport := BugsnagAPIKey, http.DefaultTransport
	t.Cleanup(func() { BugsnagAPIKey, http.DefaultTransport = oldKey, oldTransport })
	BugsnagAPIKey = testBugsnagKey
	var keys []string
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct{ APIKey string }
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, payload.APIKey)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	t.Setenv("BUGSNAG_API_KEY", "")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")
	report := func() {
		ReportError(t.Context(), CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("defect"))
	}
	report()
	t.Setenv("BUGSNAG_API_KEY", strings.Repeat("a", 32))
	report()
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "true")
	report()
	if len(keys) != 2 || keys[0] != testBugsnagKey || keys[1] != strings.Repeat("a", 32) {
		t.Fatalf("build key, runtime override, or opt-out failed: %d reports", len(keys))
	}
}
