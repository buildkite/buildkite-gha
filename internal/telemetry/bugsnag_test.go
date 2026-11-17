package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	bugsnagerrors "github.com/bugsnag/bugsnag-go/v2/errors"
)

func errorReportClient(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(Config{
		Endpoint: "https://agent.example/v3", JobID: "abcdef12-abcd-abcd-abcd-abcdef123456",
		JobToken: "job-token", ClientVersion: "0.9.2", Client: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestBugsnagReportOmitsSensitiveText(t *testing.T) {
	const buildID = "12345678-1234-1234-1234-123456789abc"
	const jobID = "abcdef12-abcd-abcd-abcd-abcdef123456"
	t.Setenv("BUILDKITE_BUILD_ID", buildID)
	t.Setenv("BUILDKITE_JOB_ID", jobID)
	t.Setenv("CUSTOMER_API_KEY", "private-api-key")
	var body []byte
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://agent.example/v3/jobs/"+jobID+"/github-actions/error-reports" || request.Method != http.MethodPost {
			t.Fatalf("unexpected request destination: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Token job-token" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Bugsnag-Api-Key") != "" {
			t.Errorf("unexpected report headers: %v", request.Header)
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
		reportBugsnagError(t.Context(), errorReportClient(t, transport), CommandRunJob, "0.9.2", Details{
			FailurePhase: FailurePhaseExecution, FailureCode: FailureCodeRuntimeIntegrity,
			ErrorMessage: "private-output", BlockerDetail: "private-config",
		}, reportErr)
		DrainErrorReports()
		if len(body) == 0 {
			t.Fatal("report did not complete before drain")
		}
		for _, secret := range []string{"private-token", "private-cause", "private-output", "private-config", "private-wrapper", "private-sibling", "private-api-key", buildID, jobID} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("report leaked %s", secret)
			}
		}
		var payload struct {
			APIKey string
			Events []struct {
				PayloadVersion string
				Context        string
				App            struct{ Version string }
				Device         struct{ Hostname string }
				Exceptions     []struct {
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
		if payload.APIKey != "" || strings.Contains(string(body), "apiKey") || len(payload.Events) != 1 {
			t.Fatal("report contains a key or is missing its error event")
		}
		event := payload.Events[0]
		if event.PayloadVersion != "4" || event.App.Version != "0.9.2" || event.Context != "run_job:execution" || event.Device.Hostname != "" {
			t.Fatalf("unexpected app/context/device: %#v", event)
		}
		if event.Metadata.Failure.Command != "run_job" || event.Metadata.Failure.Code != "E_RUNTIME_INTEGRITY" || event.Metadata.Failure.Phase != "execution" {
			t.Fatalf("missing failure classification: %#v", event.Metadata)
		}
		if len(event.Exceptions) != 1 || event.Exceptions[0].Message != "buildkite-gha run_job failed during execution" {
			t.Fatalf("unexpected exceptions: %#v", event.Exceptions)
		}
		stack := event.Exceptions[0].Stacktrace
		if len(stack) == 0 || !strings.Contains(stack[0].Method, "TestBugsnagReportOmitsSensitiveText") || !stack[0].InProject {
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
		reportBugsnagError(t.Context(), errorReportClient(t, transport), CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution, FailureCode: code}, err)
		DrainErrorReports()
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
		reportBugsnagError(t.Context(), errorReportClient(t, transport), CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution, FailureCode: code}, errors.New("expected failure"))
	}
	reportBugsnagError(t.Context(), nil, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("defect"))
	reportBugsnagError(t.Context(), errorReportClient(t, transport), CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, fmt.Errorf("cancelled: %w", context.Canceled))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reportBugsnagError(ctx, errorReportClient(t, transport), CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, ctx.Err())
	DrainErrorReports()
}

func TestBugsnagDeliveryTimeoutAndRedirect(t *testing.T) {
	for _, mode := range []string{"timeout", "redirect", "unavailable", "rejected", "oversized", "rate limited", "unconfigured"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					calls.Add(1)
					switch mode {
					case "timeout":
						<-request.Context().Done()
						return nil, request.Context().Err()
					case "redirect":
						return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": {"https://untrusted.example"}}}, nil
					case "rejected", "oversized", "rate limited", "unconfigured":
						status := map[string]int{"rejected": 400, "oversized": 413, "rate limited": 429, "unconfigured": 503}[mode]
						return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("private service response")), Header: http.Header{"Retry-After": {"3600"}}}, nil
					default:
						return nil, errors.New("network unavailable")
					}
				})
				started := time.Now()
				reportBugsnagError(t.Context(), errorReportClient(t, transport), CommandPluginImport, "dev", Details{FailurePhase: FailurePhasePipelineUpload}, errors.New("upload failed"))
				DrainErrorReports()
				synctest.Wait()
				if calls.Load() != 1 || time.Since(started) > 3*time.Second {
					t.Fatalf("delivery retried, redirected, or exceeded its budget: calls=%d duration=%s", calls.Load(), time.Since(started))
				}
			})
		})
	}
}

func TestBugsnagJobIdentityAndOptOut(t *testing.T) {
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	var calls atomic.Int32
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil || strings.Contains(string(body), "apiKey") {
			t.Error("report contains a key or cannot be read")
		}
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	t.Setenv("BUGSNAG_API_KEY", strings.Repeat("a", 32))
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", "https://agent.example/v3")
	t.Setenv("BUILDKITE_JOB_ID", "abcdef12-abcd-abcd-abcd-abcdef123456")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")
	report := func() {
		ReportError(t.Context(), CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("defect"))
		DrainErrorReports()
	}
	report()
	for _, name := range []string{"BUILDKITE_AGENT_ENDPOINT", "BUILDKITE_JOB_ID", "BUILDKITE_AGENT_ACCESS_TOKEN"} {
		value := os.Getenv(name)
		t.Setenv(name, "")
		report()
		t.Setenv(name, value)
	}
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "true")
	report()
	if calls.Load() != 1 {
		t.Fatalf("job identity or opt-out failed: %d reports", calls.Load())
	}
}

func TestBugsnagAsyncReportsKeepSubmissionDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var deadlines []time.Time
		var finished atomic.Int32
		client := errorReportClient(t, roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			deadline, _ := request.Context().Deadline()
			deadlines = append(deadlines, deadline)
			<-request.Context().Done()
			finished.Add(1)
			return nil, request.Context().Err()
		}))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		started := time.Now()
		report := func() {
			reportBugsnagError(ctx, client, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("unrelated failure"))
			synctest.Wait()
		}
		report()
		if time.Since(started) != 0 || len(deadlines) != 1 {
			t.Fatal("submission waited for delivery or inherited cancellation")
		}
		time.Sleep(time.Second)
		report()
		if len(deadlines) != 2 || !deadlines[0].Equal(started.Add(1500*time.Millisecond)) || !deadlines[1].Equal(started.Add(2500*time.Millisecond)) {
			t.Fatalf("reports did not keep independent submission deadlines: %v", deadlines)
		}
		time.Sleep(1200 * time.Millisecond)
		beforeDrain := time.Now()
		DrainErrorReports()
		synctest.Wait()
		if time.Since(beforeDrain) != 300*time.Millisecond || finished.Load() != 2 {
			t.Fatalf("drain reset or serialized deadlines: duration=%s finished=%d", time.Since(beforeDrain), finished.Load())
		}
		beforeDrain = time.Now()
		DrainErrorReports()
		if time.Since(beforeDrain) != 0 {
			t.Fatal("completed reports delayed a second drain")
		}
	})
}

func TestBugsnagSaturationAndQueuedDeliveryStayBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		client := errorReportClient(t, roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			// Model time queued inside a transport, even one that doesn't
			// return promptly when the request context expires.
			<-release
			if !errors.Is(request.Context().Err(), context.DeadlineExceeded) {
				t.Error("queued report received a fresh deadline")
			}
			return nil, request.Context().Err()
		}))
		started := time.Now()
		for range 17 {
			reportBugsnagError(t.Context(), client, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("defect"))
		}
		synctest.Wait()
		if calls.Load() != 16 || time.Since(started) != 0 {
			t.Fatalf("report submission blocked or exceeded capacity: calls=%d elapsed=%s", calls.Load(), time.Since(started))
		}
		time.Sleep(1200 * time.Millisecond)
		DrainErrorReports()
		synctest.Wait()
		if time.Since(started) != 1500*time.Millisecond {
			t.Fatal("shutdown reset deadlines or waited for queued sends sequentially")
		}
		close(release)
		synctest.Wait()
	})
}

func TestBugsnagDoesNotConsumeCompletionTelemetryBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := errorReportClient(t, roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		reportBugsnagError(t.Context(), client, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("defect"))
		time.Sleep(1200 * time.Millisecond)
		started := time.Now()
		err := client.Emit(CommandRunJob, OutcomeFailure, time.Second, Details{})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 1500*time.Millisecond {
			t.Fatalf("completion telemetry lost its independent budget: elapsed=%s err=%v", time.Since(started), err)
		}
		DrainErrorReports()
		synctest.Wait()
	})
}

func TestBugsnagDrainDiscardsCompletedDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		slow := errorReportClient(t, roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			<-release
			return nil, request.Context().Err()
		}))
		fast := errorReportClient(t, roundTripperFunc(func(*http.Request) (*http.Response, error) {
			time.Sleep(100 * time.Millisecond)
			return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(""))}, nil
		}))
		started := time.Now()
		reportBugsnagError(t.Context(), slow, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("first"))
		time.Sleep(time.Second)
		reportBugsnagError(t.Context(), fast, CommandRunJob, "dev", Details{FailurePhase: FailurePhaseExecution}, errors.New("second"))
		DrainErrorReports()
		if time.Since(started) != 1500*time.Millisecond {
			t.Fatal("completed report extended an earlier pending report's deadline")
		}
		close(release)
		synctest.Wait()
	})
}
