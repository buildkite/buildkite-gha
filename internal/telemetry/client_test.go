package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
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
		FailurePhase: FailurePhaseParsing,
		FailureCode:  FailureCodeWorkflowSyntax,
	}); err != nil {
		t.Fatal(err)
	}
	if request.Event != EventCommandCompleted || request.Properties.Command != CommandRunJob || request.Properties.Outcome != OutcomeFailure || request.Properties.ClientVersion != "1.2.3" || request.Properties.DurationMS != 1234 || request.Properties.FailurePhase != FailurePhaseParsing || request.Properties.FailureCode != FailureCodeWorkflowSyntax {
		t.Fatalf("event = %#v", request)
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

func TestDiagnosticsAreAllowlistedDeduplicatedAndCapped(t *testing.T) {
	allowed := []string{
		string(FailureCodeWorkflowSyntax), string(FailureCodeEventInvalid), string(FailureCodeGraphInvalid),
		string(FailureCodeMatrixInvalid), string(FailureCodeExpressionInvalid), string(FailureCodeActionDiscovery),
		string(FailureCodeActionResolution), string(FailureCodePlanConstruction), string(FailureCodePipelineGeneration),
		string(FailureCodeEnvironment), string(FailureCodeProfile), "W_ACTION_RUNTIME_UNKNOWN",
		"W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED",
	}
	input := make([]Diagnostic, 0, len(allowed)*2)
	for _, code := range allowed {
		input = append(input, Diagnostic{Code: code, Severity: SeverityError}, Diagnostic{Code: code, Severity: SeverityWarning})
	}
	bounded, err := boundedDiagnostics(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != maxDiagnostics {
		t.Fatalf("diagnostics = %d, want cap %d", len(bounded), maxDiagnostics)
	}
	if _, err := boundedDiagnostics([]Diagnostic{{Code: "CUSTOMER_VALUE", Severity: SeverityError}}); err == nil {
		t.Fatal("boundedDiagnostics() accepted non-allowlisted code")
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
	n := int64(len(p))
	if n > b.remaining {
		n = b.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.remaining -= n
	b.read += n
	return int(n), nil
}

func (*countingBody) Close() error { return nil }
