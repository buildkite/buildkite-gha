package agentapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testJobID = "11111111-2222-3333-8444-555555555555"

func TestClientBuildsAuthenticatedJobRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v3/jobs/"+testJobID+"/service/path" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Token job-token" || request.Header.Get("Accept") != "application/json" || request.Header.Get("User-Agent") != "buildkite-gha/1.2.3" {
			t.Errorf("request headers = %#v", request.Header)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{Endpoint: server.URL + "/v3", JobID: testJobID, JobToken: "job-token", ClientVersion: "1.2.3", HTTPClient: server.Client()}, "test")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.URL("service/path"), http.NoBody)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("do() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("response status = %d", response.StatusCode)
	}
}

func TestClientRejectsUnsafeConnectionConfiguration(t *testing.T) {
	validEndpoint := "https://agent.example/v3"
	tests := map[string]struct {
		endpoint string
		jobID    string
		token    string
	}{
		"missing endpoint":     {jobID: testJobID, token: "job-token"},
		"endpoint credentials": {endpoint: "https://user@agent.example/v3", jobID: testJobID, token: "job-token"},
		"endpoint query":       {endpoint: validEndpoint + "?redirect=other", jobID: testJobID, token: "job-token"},
		"plaintext hostname":   {endpoint: "http://localhost/v3", jobID: testJobID, token: "job-token"},
		"invalid job ID":       {endpoint: validEndpoint, jobID: "../other", token: "job-token"},
		"missing token":        {endpoint: validEndpoint, jobID: testJobID},
		"token header split":   {endpoint: validEndpoint, jobID: testJobID, token: "secret\r\nOther: value"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{Endpoint: test.endpoint, JobID: test.jobID, JobToken: test.token}, "test"); err == nil {
				t.Fatal("New() accepted unsafe configuration")
			}
		})
	}
}

func TestClientDoesNotAuthenticateRequestsOutsideJobEndpoint(t *testing.T) {
	received := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		received <- request
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{Endpoint: server.URL + "/v3", JobID: testJobID, JobToken: "job-secret", HTTPClient: server.Client()}, "test")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		received <- request
	}))
	t.Cleanup(other.Close)

	tests := map[string]string{
		"different origin": other.URL + "/v3/jobs/" + testJobID + "/service",
		"different job":    server.URL + "/v3/jobs/00000000-0000-4000-8000-000000000000/service",
		"parent path":      server.URL + "/v3/service",
		"path traversal":   client.URL("../other-job/service"),
	}
	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, http.NoBody)
			if err != nil {
				t.Fatalf("NewRequestWithContext() error = %v", err)
			}
			response, err := client.Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "outside the configured job endpoint") {
				t.Fatalf("Do() error = %v", err)
			}
			select {
			case request := <-received:
				t.Fatalf("outside request reached server with authorization %q", request.Header.Get("Authorization"))
			default:
			}
		})
	}
}
