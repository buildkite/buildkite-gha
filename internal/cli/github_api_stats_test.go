package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
)

type sourceAPICommandSummary struct {
	Command string `json:"command"`
	actionsource.GitHubAPIStatsSummary
}

func readSourceAPICommandSummary(t *testing.T, stderr string) (sourceAPICommandSummary, string) {
	t.Helper()
	if strings.Count(stderr, sourceAPIStatsPrefix) != 1 {
		t.Fatalf("want exactly one source API summary: %s", stderr)
	}
	before, encoded, _ := strings.Cut(stderr, sourceAPIStatsPrefix)
	if strings.Count(encoded, "\n") != 1 || !strings.HasSuffix(encoded, "\n") {
		t.Fatalf("summary must be the final stderr line: %q", encoded)
	}
	var summary sourceAPICommandSummary
	if err := json.Unmarshal([]byte(encoded), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Scope != "github_source_rest" {
		t.Fatalf("summary scope = %q", summary.Scope)
	}
	return summary, before
}

func TestSourceAPIStatsOptInPreservesPipelineOutput(t *testing.T) {
	workflow := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	event := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	args := []string{"compile", "--event-path", event, workflow}
	t.Setenv(sourceAPIStatsEnvironment, "")
	var baselineOut, baselineErr bytes.Buffer
	if code := Run(args, &baselineOut, &baselineErr, "dev"); code != 0 {
		t.Fatalf("baseline compile = %d: %s", code, &baselineErr)
	}
	for _, value := range []string{"", "false", "1", "TRUE", "true"} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv(sourceAPIStatsEnvironment, value)
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr, "dev"); code != 0 || !bytes.Equal(stdout.Bytes(), baselineOut.Bytes()) {
				t.Fatalf("compile = %d, changed stdout = %t: %s", code, !bytes.Equal(stdout.Bytes(), baselineOut.Bytes()), &stderr)
			}
			withoutStats := stderr.String()
			if value == "true" {
				summary, original := readSourceAPICommandSummary(t, stderr.String())
				withoutStats = original
				if summary.Command != "compile" || summary.HTTPAttempts != 0 || summary.HTTPResponses != 0 || len(summary.Requests) != 0 || len(summary.CacheHits) != 0 || len(summary.Suppressed) != 0 {
					t.Fatalf("local workflow summary = %#v", summary)
				}
			}
			if withoutStats != baselineErr.String() {
				t.Fatalf("original stderr changed: %s", withoutStats)
			}
		})
	}
}

func TestSourceAPIStatsPreserveUsageFailuresAndSkipHelp(t *testing.T) {
	t.Setenv(sourceAPIStatsEnvironment, "true")
	for _, args := range [][]string{
		{"compile", "--unknown"}, {"upload", "--unknown"}, {"upload", "--stage-digest", "bad"},
		{"plugin", "unexpected"}, {"validate", "--unknown"}, {"validate-batch", "--unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr, "dev"); code != 2 {
				t.Fatalf("command = %d, want usage failure: %s", code, &stderr)
			}
			summary, original := readSourceAPICommandSummary(t, stderr.String())
			if summary.Command != args[0] || summary.HTTPAttempts != 0 || stdout.Len() != 0 || !strings.Contains(original, "buildkite-gha:") {
				t.Fatalf("usage summary = %#v, original stderr = %q, stdout = %q", summary, original, stdout.String())
			}
		})
	}
	for _, args := range [][]string{{"--help"}, {"compile", "--help"}, {"upload", "-h"}, {"--version"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 || stderr.Len() != 0 || stdout.Len() == 0 {
			t.Fatalf("help/version = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
	}
}

type sourceStatsRoundTripper func(*http.Request) (*http.Response, error)

func (f sourceStatsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSourceAPIStatsCountRealCommandRequestsOnResolutionFailure(t *testing.T) {
	requireImporterHost(t)
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "60")
		w.Header().Set("X-Private-Test-Header", "secret-header")
		http.Error(w, "secret rate-limit response", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	localURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	originalTransport := http.DefaultTransport
	http.DefaultTransport = sourceStatsRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			return nil, errors.New("unexpected non-source request in accounting test")
		}
		local := r.Clone(r.Context())
		local.URL.Scheme, local.URL.Host = localURL.Scheme, localURL.Host
		local.Host = ""
		return originalTransport.RoundTrip(local)
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	for _, command := range []string{"compile", "validate", "upload", "plugin", "validate-batch"} {
		t.Run(command, func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{
				"ci.yml": "on: push\njobs:\n  call:\n    uses: secret-owner/secret-repository/.github/workflows/reusable.yml@secret-ref\n",
			})
			t.Chdir(repository)
			t.Setenv(sourceAPIStatsEnvironment, "true")
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			setCLIPluginBuildkiteEnvironment(t, "source-stats-importer")
			t.Setenv(pluginConfigurationEnvironment, `{"workflow":".github/workflows/ci.yml"}`)
			workflow := filepath.Join(repository, ".github", "workflows", "ci.yml")
			args := []string{command, "--event-path", eventPath, workflow}
			wantCode := 1
			switch command {
			case "validate":
				args = append([]string{command, "--profile", "hosted", "--format", "json"}, args[1:]...)
			case "upload":
				wantCode = 0 // Resolution failures are uploaded as failing steps.
			case "plugin":
				args, wantCode = []string{command}, 0
			case "validate-batch":
				root := t.TempDir()
				manifest := writeBatchManifest(t, root, []batchValidationRecord{
					{ID: "one", Repository: "owner/repository", Path: ".github/workflows/ci.yml", Hash: strings.Repeat("1", 64), Source: workflow},
				})
				args = []string{command, "--manifest", manifest, "--output-dir", filepath.Join(root, "reports"), "--corpus-id", "stats-test", "--action-resolution-snapshot", filepath.Join(root, "snapshot")}
				wantCode = 0 // Compatibility failures are written to the reports.
			}
			before := requests.Load()
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != wantCode {
				t.Fatalf("command = %d, want %d: %s", code, wantCode, &stderr)
			}
			summary, _ := readSourceAPICommandSummary(t, stderr.String())
			observed := requests.Load() - before
			if summary.Command != command || observed == 0 || summary.HTTPAttempts != observed || summary.HTTPResponses != observed {
				t.Fatalf("summary = %#v; observed HTTP requests = %d", summary, observed)
			}
			var counted uint64
			for _, result := range summary.Requests {
				if result.Status != 429 || result.Outcome != "response" || result.Authentication != "anonymous" {
					t.Fatalf("unexpected request count = %#v", result)
				}
				counted += result.Count
			}
			if counted != observed || strings.Contains(stdout.String(), sourceAPIStatsPrefix) {
				t.Fatalf("counted requests = %d, observed = %d, stdout = %q", counted, observed, stdout.String())
			}
			encoded, err := json.Marshal(summary)
			if err != nil || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), repository) || strings.Contains(string(encoded), server.URL) {
				t.Fatalf("unsafe summary: %s; %v", encoded, err)
			}
			if command == "validate" && !json.Valid(stdout.Bytes()) {
				t.Fatalf("validation JSON was corrupted: %s", &stdout)
			}
		})
	}
}

func TestSourceAPIStatsDoNotReplaceTelemetryError(t *testing.T) {
	events := make(chan map[string]json.RawMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		events <- event.Properties
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")
	var baselineError string
	for _, enabled := range []string{"", "true"} {
		t.Setenv(sourceAPIStatsEnvironment, enabled)
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"plugin", "unexpected"}, &stdout, &stderr, "dev"); code != 2 {
			t.Fatalf("plugin = %d: %s", code, &stderr)
		}
		properties := <-events
		var message string
		if err := json.Unmarshal(properties["error_message"], &message); err != nil || !strings.Contains(message, "does not accept arguments") {
			t.Fatalf("telemetry error = %q, %v", message, err)
		}
		if enabled == "" {
			baselineError = message
			continue
		}
		summary, _ := readSourceAPICommandSummary(t, stderr.String())
		if message != baselineError || strings.Contains(message, sourceAPIStatsPrefix) || summary.HTTPAttempts != 0 {
			t.Fatalf("telemetry error = %q, summary = %#v", message, summary)
		}
		for _, field := range []string{"http_attempts", "http_responses", "cache_hits", "requests", "suppressed"} {
			if _, exists := properties[field]; exists {
				t.Fatalf("local statistics added telemetry property %q", field)
			}
		}
	}
}
