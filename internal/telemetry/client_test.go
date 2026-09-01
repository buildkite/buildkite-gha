package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const testJobID = "22222222-2222-4222-8222-222222222222"

func TestClientEmitsCommandCompletedEvent(t *testing.T) {
	var request struct {
		Event      string     `json:"event"`
		Properties Properties `json:"properties"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v3/jobs/"+testJobID+"/posthog/events" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Token job-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "buildkite-gha/1.2.3" {
			t.Errorf("User-Agent = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client, err := New(Config{Endpoint: server.URL + "/v3", JobID: testJobID, JobToken: "job-token", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	duration := 1234 * time.Millisecond
	if err := client.Emit(CommandRunJob, OutcomeFailure, duration, Details{
		FailurePhase:          FailurePhaseParsing,
		FailureCode:           FailureCodeWorkflowSyntax,
		ErrorMessage:          "  buildkite-gha: run-job:\ninvalid workflow\tvalue  ",
		ErrorMessageTruncated: true,
		Blocker:               "shell",
		BlockerDetail:         "pwsh",
		Diagnostics: []Diagnostic{{
			Code: string(FailureCodeExpressionInvalid), Severity: SeverityError,
			Blocker: "runner_label", BlockerDetail: "windows-latest",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if request.Event != EventCommandCompleted {
		t.Fatalf("event = %q, want %q", request.Event, EventCommandCompleted)
	}
	want := Properties{
		Command: CommandRunJob, Outcome: OutcomeFailure, ClientVersion: "1.2.3", DurationMS: 1234,
		FailurePhase: FailurePhaseParsing, FailureCode: FailureCodeWorkflowSyntax,
		ErrorMessage: "buildkite-gha: run-job: invalid workflow value", ErrorMessageTruncated: true,
		Blocker: "shell", BlockerDetail: "pwsh",
		Diagnostics: []Diagnostic{{
			Code: string(FailureCodeExpressionInvalid), Severity: SeverityError,
			Blocker: "runner_label", BlockerDetail: "windows-latest",
		}},
	}
	if !reflect.DeepEqual(request.Properties, want) {
		t.Fatalf("properties = %#v, want %#v", request.Properties, want)
	}
}

func TestNewSkipsMissingCredentialsAndOptOut(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
	}{
		{name: "missing endpoint", config: Config{JobID: testJobID, JobToken: "token"}},
		{name: "missing job", config: Config{Endpoint: "https://agent.example/v3", JobToken: "token"}},
		{name: "missing token", config: Config{Endpoint: "https://agent.example/v3", JobID: testJobID}},
		{name: "disabled", config: Config{Endpoint: "https://agent.example/v3", JobID: testJobID, JobToken: "token", Disabled: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := New(test.config)
			if err != nil || client != nil {
				t.Fatalf("New() = %#v, %v; want nil, nil", client, err)
			}
		})
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		jobID    string
		token    string
	}{
		{name: "non-loopback HTTP", endpoint: "http://agent.example/v3", jobID: testJobID, token: "token"},
		{name: "userinfo", endpoint: "https://user@agent.example/v3", jobID: testJobID, token: "token"},
		{name: "query", endpoint: "https://agent.example/v3?q=1", jobID: testJobID, token: "token"},
		{name: "fragment", endpoint: "https://agent.example/v3#x", jobID: testJobID, token: "token"},
		{name: "invalid job", endpoint: "https://agent.example/v3", jobID: "../job", token: "token"},
		{name: "header injection", endpoint: "https://agent.example/v3", jobID: testJobID, token: "token\r\nX: y"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if client, err := New(Config{Endpoint: test.endpoint, JobID: test.jobID, JobToken: test.token}); err == nil || client != nil {
				t.Fatalf("New() = %#v, %v; want safe rejection", client, err)
			}
		})
	}
}

func TestClientDoesNotFollowRedirectsOrExposeSecrets(t *testing.T) {
	const secret = "job-token-secret"
	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL, JobID: testJobID, JobToken: secret})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Emit(CommandPluginImport, OutcomeFailure, 0, Details{})
	if err == nil || redirected || strings.Contains(err.Error(), secret) {
		t.Fatalf("Emit() error/redirected = %v / %v", err, redirected)
	}
}

func TestClientTreatsServiceFailuresAsBodyFreeErrors(t *testing.T) {
	const responseSecret = "customer-response-must-not-escape"
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, responseSecret)
			}))
			defer server.Close()
			client, err := New(Config{Endpoint: server.URL, JobID: testJobID, JobToken: "job-token"})
			if err != nil {
				t.Fatal(err)
			}
			err = client.Emit(CommandPluginImport, OutcomeFailure, 0, Details{})
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) || strings.Contains(err.Error(), responseSecret) {
				t.Fatalf("Emit() error = %v", err)
			}
		})
	}
}

func TestClientUsesIndependentTimeout(t *testing.T) {
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	client, err := New(Config{
		Endpoint: "https://agent.example/v3", JobID: testJobID, JobToken: "token",
		Client: &http.Client{Transport: transport}, Timeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = client.Emit(CommandRunJob, OutcomeCancelled, 0, Details{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("Emit() = %v after %s", err, time.Since(started))
	}
}

func TestClientBoundsResponseDrain(t *testing.T) {
	body := &countingBody{remaining: responseDrainLimit * 4}
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: body, Header: make(http.Header)}, nil
	})
	client, err := New(Config{
		Endpoint: "https://agent.example/v3", JobID: testJobID, JobToken: "token",
		Client: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Emit(CommandRunJob, OutcomeFailure, 0, Details{})
	if err == nil || body.read > responseDrainLimit || strings.Contains(err.Error(), strings.Repeat("x", 10)) {
		t.Fatalf("Emit() error/read = %v / %d", err, body.read)
	}
}

func TestPropertiesAreBounded(t *testing.T) {
	client, err := New(Config{Endpoint: "https://agent.example/v3", JobID: testJobID, JobToken: "token", ClientVersion: strings.Repeat("v", maxClientVersionBytes+10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.clientVersion) != maxClientVersionBytes {
		t.Fatalf("client version length = %d", len(client.clientVersion))
	}
	if client.userAgent != "buildkite-gha/unknown" {
		t.Fatalf("User-Agent = %q, want bounded fallback", client.userAgent)
	}
	if err := client.Emit(Command("arbitrary"), OutcomeSuccess, 0, Details{}); err == nil {
		t.Fatal("Emit() accepted an unknown command")
	}
	if err := client.Emit(CommandRunJob, Outcome("arbitrary"), 0, Details{}); err == nil {
		t.Fatal("Emit() accepted an unknown outcome")
	}
	if err := client.Emit(CommandRunJob, OutcomeFailure, 0, Details{FailurePhase: FailurePhase("arbitrary"), FailureCode: FailureCodeUnknown}); err == nil {
		t.Fatal("Emit() accepted an unknown failure phase")
	}
	if err := client.Emit(CommandRunJob, OutcomeFailure, 0, Details{FailurePhase: FailurePhaseUnknown, FailureCode: FailureCode("arbitrary")}); err == nil {
		t.Fatal("Emit() accepted an unknown failure code")
	}
}

func TestErrorMessageIsNormalizedAndUTF8Bounded(t *testing.T) {
	message := "earlier output\n\t" + strings.Repeat("界", MaxErrorMessageBytes) + "\n immediate failure"
	bounded, truncated := boundedErrorMessage(message)
	if !truncated {
		t.Fatal("boundedErrorMessage() did not report truncation")
	}
	if len(bounded) > MaxErrorMessageBytes {
		t.Fatalf("boundedErrorMessage() returned %d bytes, want at most %d", len(bounded), MaxErrorMessageBytes)
	}
	if !utf8.ValidString(bounded) {
		t.Fatalf("boundedErrorMessage() returned invalid UTF-8: %q", bounded)
	}
	if strings.HasPrefix(bounded, "earlier output") || !strings.HasSuffix(bounded, "immediate failure") {
		t.Fatalf("boundedErrorMessage() = %q, want final failure without earlier output", bounded)
	}
	if got, truncated := boundedErrorMessage("\n failure\t details\x00 \r\n"); got != "failure details" || truncated {
		t.Fatalf("boundedErrorMessage() = %q, %t", got, truncated)
	}
}

func TestRuntimeClassificationCodesAreFailureCodesOnly(t *testing.T) {
	for _, code := range []FailureCode{FailureCodeStepProcessExit, FailureCodeUnsupportedFeature, FailureCodeRuntimeIntegrity, FailureCodeSecretUnavailable} {
		if !validFailureCode(code) {
			t.Errorf("validFailureCode(%q) = false", code)
		}
		if _, ok := diagnosticSeverity(string(code)); ok {
			t.Errorf("diagnosticSeverity(%q) accepted a failure-only code as a diagnostic", code)
		}
	}
}

func TestDiagnosticsEnforceSeverityAndDeduplicateByCode(t *testing.T) {
	errorCodes := []string{
		string(FailureCodeWorkflowSyntax), string(FailureCodeEventInvalid), string(FailureCodeGraphInvalid),
		string(FailureCodeMatrixInvalid), string(FailureCodeExpressionInvalid), string(FailureCodeActionDiscovery),
		string(FailureCodeActionResolution), string(FailureCodePlanConstruction), string(FailureCodePipelineGeneration),
		string(FailureCodeEnvironment), string(FailureCodeProfile),
	}
	warningCodes := []string{"W_ACTION_RUNTIME_UNKNOWN", "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED", "W_TRIGGER_EVENT_UNSUPPORTED"}
	input := make([]Diagnostic, 0, len(errorCodes)+len(warningCodes)+2)
	for _, code := range errorCodes {
		input = append(input, Diagnostic{Code: code, Severity: SeverityError})
	}
	for _, code := range warningCodes {
		input = append(input, Diagnostic{Code: code, Severity: SeverityWarning})
	}
	input = append(input, input[0], input[len(errorCodes)])
	bounded, err := boundedDiagnostics(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != len(errorCodes)+len(warningCodes) {
		t.Fatalf("diagnostics = %d, want %d unique codes", len(bounded), len(errorCodes)+len(warningCodes))
	}
	if _, err := boundedDiagnostics([]Diagnostic{{Code: "CUSTOMER_VALUE", Severity: SeverityError}}); err == nil {
		t.Fatal("boundedDiagnostics() accepted non-allowlisted code")
	}
}

func TestDiagnosticsPreserveDistinctBlockersForOneCode(t *testing.T) {
	common := strings.Repeat("x", maxBlockerDetailBytes)
	input := []Diagnostic{
		{Code: string(FailureCodeExpressionInvalid), Severity: SeverityError, Blocker: "expression", BlockerDetail: common + "first"},
		{Code: string(FailureCodeExpressionInvalid), Severity: SeverityError, Blocker: "expression", BlockerDetail: common + "second"},
	}
	bounded, err := boundedDiagnostics(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 2 || bounded[0].BlockerDetail == bounded[1].BlockerDetail {
		t.Fatalf("bounded diagnostics collapsed distinct details: %#v", bounded)
	}
	for _, diagnostic := range bounded {
		if len(diagnostic.BlockerDetail) > maxBlockerDetailBytes || !strings.Contains(diagnostic.BlockerDetail, "…#") {
			t.Fatalf("bounded diagnostic detail = %q", diagnostic.BlockerDetail)
		}
	}
}

func TestBlockerDetailsAreNormalizedAndBounded(t *testing.T) {
	blocker, detail, err := boundedBlocker("shell", "  pwsh\n"+strings.Repeat("界", maxBlockerDetailBytes))
	if err != nil || blocker != "shell" || len(detail) > maxBlockerDetailBytes || !utf8.ValidString(detail) || !strings.HasPrefix(detail, "pwsh ") {
		t.Fatalf("boundedBlocker() = %q, %q, %v", blocker, detail, err)
	}
	if _, _, err := boundedBlocker("customer-value", "detail"); err == nil {
		t.Fatal("boundedBlocker() accepted an unknown blocker")
	}
}

func TestDiagnosticsRejectMismatchedSeverity(t *testing.T) {
	for _, diagnostic := range []Diagnostic{
		{Code: string(FailureCodeWorkflowSyntax), Severity: SeverityWarning},
		{Code: "W_ACTION_RUNTIME_UNKNOWN", Severity: SeverityError},
	} {
		if _, err := boundedDiagnostics([]Diagnostic{diagnostic}); err == nil {
			t.Fatalf("boundedDiagnostics(%#v) accepted mismatched severity", diagnostic)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type countingBody struct {
	remaining int64
	read      int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.remaining -= n
	b.read += n
	return int(n), nil
}

func (*countingBody) Close() error { return nil }
