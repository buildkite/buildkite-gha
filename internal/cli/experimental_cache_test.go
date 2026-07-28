package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghacache "github.com/buildkite/buildkite-gha/internal/cache"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestExperimentalDirectoryCacheConfigIsExplicitAndRefScoped(t *testing.T) {
	job := plan.Job{
		Event: plan.Event{Provider: "github", Repository: "Owner/Repository", Ref: "refs/heads/main"},
		Steps: []plan.Step{{Kind: "uses"}},
	}
	if config, err := experimentalDirectoryCacheConfig(job, func(string) string { return "" }); err != nil || config != nil {
		t.Fatalf("disabled config = %#v, %v, want nil", config, err)
	}

	root := t.TempDir()
	values := map[string]string{
		"BUILDKITE_GHA_CACHE_BACKEND":     "directory",
		"BUILDKITE_GHA_CACHE_DIR":         root,
		"BUILDKITE_ORGANIZATION_ID":       "organization-id",
		"BUILDKITE_PIPELINE_ID":           "pipeline-id",
		"BUILDKITE_AGENT_META_DATA_QUEUE": "hosted",
	}
	config, err := jobCacheConfig(context.Background(), job, func(name string) string { return values[name] }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.Backend == nil || config.ReadOnly {
		t.Fatalf("config = %#v", config)
	}
	if info, err := filepath.Glob(filepath.Join(root, "buildkite-gha-cache-v1", "*")); err != nil || len(info) != 3 {
		t.Fatalf("cache directories = %#v, %v", info, err)
	}
}

func TestExperimentalDirectoryCacheConfigResolvesHostedRelativePath(t *testing.T) {
	workdir := t.TempDir()
	t.Chdir(workdir)
	const relativeRoot = ".buildkite-gha/cache-volume/gha-cache"
	values := map[string]string{
		"BUILDKITE_GHA_CACHE_DIR":         relativeRoot,
		"BUILDKITE_ORGANIZATION_ID":       "organization-id",
		"BUILDKITE_PIPELINE_ID":           "pipeline-id",
		"BUILDKITE_AGENT_META_DATA_QUEUE": "hosted",
	}
	job := plan.Job{
		Event: plan.Event{Provider: "github", Repository: "owner/repository", Ref: "refs/heads/main"},
		Steps: []plan.Step{{Kind: "uses"}},
	}
	config, err := jobCacheConfig(context.Background(), job, func(name string) string { return values[name] }, nil)
	if err != nil || config == nil || config.Backend == nil {
		t.Fatalf("relative hosted cache config = %#v, %v", config, err)
	}
	info, err := filepath.Glob(filepath.Join(workdir, relativeRoot, "buildkite-gha-cache-v1", "*"))
	if err != nil || len(info) != 3 {
		t.Fatalf("relative hosted cache directories = %#v, %v", info, err)
	}
}

func TestExperimentalDirectoryCacheConfigFailsClosed(t *testing.T) {
	root := t.TempDir()
	valid := map[string]string{
		"BUILDKITE_GHA_CACHE_DIR":         root,
		"BUILDKITE_ORGANIZATION_ID":       "organization-id",
		"BUILDKITE_PIPELINE_ID":           "pipeline-id",
		"BUILDKITE_AGENT_META_DATA_QUEUE": "hosted",
	}
	getenv := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	actionSteps := []plan.Step{{Kind: "uses"}}
	for _, job := range []plan.Job{
		{Event: plan.Event{Provider: "other", Repository: "owner/repository", Ref: "refs/heads/main"}, Steps: actionSteps},
		{Event: plan.Event{Provider: "github", Ref: "refs/heads/main"}, Steps: actionSteps},
		{Event: plan.Event{Provider: "github", Repository: "owner/repository"}, Steps: actionSteps},
		{Event: plan.Event{Provider: "github", Repository: "owner/repository:other", Ref: "refs/heads/main"}, Steps: actionSteps},
	} {
		if config, err := experimentalDirectoryCacheConfig(job, getenv(valid)); err == nil || config != nil {
			t.Fatalf("invalid event config = %#v, %v", config, err)
		}
	}
	job := plan.Job{
		Event: plan.Event{Provider: "github", Repository: "owner/repository", Ref: "refs/heads/main"},
		Steps: actionSteps,
	}
	for _, missing := range []string{"BUILDKITE_ORGANIZATION_ID", "BUILDKITE_PIPELINE_ID", "BUILDKITE_AGENT_META_DATA_QUEUE"} {
		values := make(map[string]string, len(valid))
		for name, value := range valid {
			values[name] = value
		}
		delete(values, missing)
		if config, err := experimentalDirectoryCacheConfig(job, getenv(values)); err == nil || config != nil {
			t.Fatalf("missing %s config = %#v, %v", missing, config, err)
		}
	}
	cacheFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cacheFile, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	backendValues := make(map[string]string, len(valid))
	for name, value := range valid {
		backendValues[name] = value
	}
	backendValues["BUILDKITE_GHA_CACHE_DIR"] = cacheFile
	if config, err := experimentalDirectoryCacheConfig(job, getenv(backendValues)); !errors.Is(err, errCacheBackendUnavailable) || config != nil {
		t.Fatalf("unavailable backend config = %#v, %v", config, err)
	}
}

func TestJobCacheConfigSkipsDirectoryForShellOnlyJob(t *testing.T) {
	root := t.TempDir()
	values := map[string]string{
		"BUILDKITE_GHA_CACHE_DIR":         root,
		"BUILDKITE_ORGANIZATION_ID":       "organization-id",
		"BUILDKITE_PIPELINE_ID":           "pipeline-id",
		"BUILDKITE_AGENT_META_DATA_QUEUE": "hosted",
	}
	job := plan.Job{
		Event: plan.Event{Provider: "github", Repository: "owner/repository", Ref: "refs/heads/main"},
		Steps: []plan.Step{{Kind: "run"}},
	}
	config, err := jobCacheConfig(context.Background(), job, func(name string) string { return values[name] }, nil)
	if err != nil || config != nil {
		t.Fatalf("shell-only directory config = %#v, %v", config, err)
	}
	if matches, err := filepath.Glob(filepath.Join(root, "buildkite-gha-cache-v1", "*")); err != nil || len(matches) != 0 {
		t.Fatalf("shell-only job created cache directories = %#v, %v", matches, err)
	}
}

func TestJobCacheConfigSelectsAgentCapabilityAndLimits(t *testing.T) {
	const jobID = "55555555-5555-5555-5555-555555555555"
	actionJob := plan.Job{Steps: []plan.Step{{Kind: "uses"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v3/jobs/"+jobID+"/github-actions-cache/" || request.Header.Get("Authorization") != "Token agent-token" {
			t.Errorf("capability request = %s %s, authorization %q", request.Method, request.URL.Path, request.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(ghacache.AgentCapability{
			Enabled: true, Mode: "read-only",
			Limits: ghacache.AgentLimits{MaxArchiveSize: 12345, MaxCandidates: 7, MaxKeyBytes: 321, MaxVersionBytes: 123},
		})
	}))
	defer server.Close()
	root := t.TempDir()
	values := map[string]string{
		"BUILDKITE_GHA_CACHE_BACKEND":  "agent",
		"BUILDKITE_GHA_CACHE_DIR":      root,
		"BUILDKITE_AGENT_ENDPOINT":     server.URL + "/v3",
		"BUILDKITE_AGENT_ACCESS_TOKEN": "agent-token",
		"BUILDKITE_JOB_ID":             jobID,
	}
	config, err := jobCacheConfig(context.Background(), actionJob, func(name string) string { return values[name] }, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.Backend == nil || !config.ReadOnly || config.MaxArchive != 12345 || config.MaxCandidates != 7 || config.MaxKey != 321 || config.MaxVersion != 123 {
		t.Fatalf("agent cache config = %#v", config)
	}
	if _, err := os.Stat(filepath.Join(root, "buildkite-gha-cache-v1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent selection initialized directory backend: %v", err)
	}
}

func TestJobCacheConfigAgentAdmissionAndFailures(t *testing.T) {
	const jobID = "66666666-6666-6666-6666-666666666666"
	actionJob := plan.Job{Steps: []plan.Step{{Kind: "uses"}}}
	for _, test := range []struct {
		name       string
		status     int
		capability ghacache.AgentCapability
		mutate     func(map[string]string)
		wantNil    bool
		wantError  error
		wantFatal  bool
	}{
		{name: "disabled", status: http.StatusOK, capability: ghacache.AgentCapability{Mode: "disabled", Reason: "feature_not_enabled"}, wantNil: true},
		{name: "missing capability route is nonfatal", status: http.StatusNotFound, wantNil: true, wantError: errCacheBackendUnavailable},
		{name: "rate limited is nonfatal", status: http.StatusTooManyRequests, wantNil: true, wantError: errCacheBackendUnavailable},
		{name: "unavailable is nonfatal", status: http.StatusServiceUnavailable, wantNil: true, wantError: errCacheBackendUnavailable},
		{name: "denied fails closed", status: http.StatusForbidden, wantNil: true, wantError: ghacache.ErrDenied},
		{name: "missing token fails closed", status: http.StatusOK, mutate: func(values map[string]string) {
			delete(values, "BUILDKITE_AGENT_ACCESS_TOKEN")
		}, wantNil: true, wantFatal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				if test.status == http.StatusOK {
					_ = json.NewEncoder(w).Encode(test.capability)
				}
			}))
			defer server.Close()
			values := map[string]string{
				"BUILDKITE_GHA_CACHE_BACKEND": "agent", "BUILDKITE_AGENT_ENDPOINT": server.URL + "/v3",
				"BUILDKITE_AGENT_ACCESS_TOKEN": "agent-token", "BUILDKITE_JOB_ID": jobID,
			}
			if test.mutate != nil {
				test.mutate(values)
			}
			config, err := jobCacheConfig(context.Background(), actionJob, func(name string) string { return values[name] }, server.Client())
			wrongError := test.wantError == nil && err != nil
			if test.wantFatal {
				wrongError = err == nil || errors.Is(err, errCacheBackendUnavailable)
			} else if test.wantError != nil {
				wrongError = !errors.Is(err, test.wantError)
			}
			if test.wantNil && config != nil || wrongError {
				t.Fatalf("jobCacheConfig() = %#v, %v, want error %v", config, err, test.wantError)
			}
		})
	}
	values := map[string]string{"BUILDKITE_GHA_CACHE_BACKEND": "bogus"}
	if config, err := jobCacheConfig(context.Background(), plan.Job{}, func(name string) string { return values[name] }, nil); err == nil || config != nil {
		t.Fatalf("unknown backend config = %#v, %v", config, err)
	}
}

func TestJobCacheConfigSkipsAgentCapabilityForShellOnlyJob(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()
	values := map[string]string{
		"BUILDKITE_GHA_CACHE_BACKEND": "agent",
		"BUILDKITE_AGENT_ENDPOINT":    server.URL + "/v3",
		"BUILDKITE_JOB_ID":            "77777777-7777-7777-7777-777777777777",
		// Intentionally omit the Agent token: unused cache configuration must
		// not make a shell-only job fail closed.
	}
	job := plan.Job{Steps: []plan.Step{{Kind: "run"}}}
	config, err := jobCacheConfig(context.Background(), job, func(name string) string { return values[name] }, server.Client())
	if err != nil || config != nil || called {
		t.Fatalf("jobCacheConfig() = %#v, %v, capability called %v", config, err, called)
	}
}

func TestRunJobCancellationDoesNotReportCacheDegradation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("cancelled run queried cache capability")
	}))
	defer server.Close()
	job := cliRunJobPlan()
	job.Steps = []plan.Step{{ID: "action", Kind: "uses", Uses: "owner/action@v1"}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	t.Setenv("BUILDKITE_GHA_CACHE_BACKEND", "agent")
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "agent-token")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := runJobContext(ctx, []string{"--plan", planPath}, &stdout, &stderr, "dev", transport.Agent{Runner: runner}); code != 1 {
		t.Fatalf("runJobContext() code = %d, stderr = %q, want 1", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "continuing without GitHub cache") {
		t.Fatalf("runJobContext() stderr = %q, want no cache-degradation warning", stderr.String())
	}
	if result := publishedCLIManifest(t, runner, job, planDigest).Result; result != "cancelled" {
		t.Fatalf("published result = %q, want cancelled", result)
	}
}

func TestRunJobWiresAndDegradesExperimentalDirectoryCache(t *testing.T) {
	for _, test := range []struct {
		name           string
		cachePath      func(*testing.T) string
		mutate         func(*plan.Job)
		wantCode       int
		wantDiagnostic string
		wantCacheDirs  bool
	}{
		{name: "configured", cachePath: func(t *testing.T) string { return t.TempDir() }, mutate: func(job *plan.Job) {
			job.Steps = []plan.Step{{ID: "action", Kind: "uses", Uses: "owner/action@v1"}}
		}, wantCode: 1, wantCacheDirs: true},
		{name: "unavailable falls back", cachePath: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, mutate: func(job *plan.Job) {
			job.Steps = []plan.Step{{ID: "action", Kind: "uses", Uses: "owner/action@v1"}}
		}, wantCode: 1, wantDiagnostic: "continuing without GitHub cache"},
		{name: "invalid plan identity fails", cachePath: func(t *testing.T) string { return t.TempDir() }, mutate: func(job *plan.Job) {
			job.Steps = []plan.Step{{ID: "action", Kind: "uses", Uses: "owner/action@v1"}}
			job.Event.Repository = ""
		}, wantCode: 1, wantDiagnostic: "requires a GitHub repository and ref"},
		{name: "shell-only skips", cachePath: func(t *testing.T) string { return t.TempDir() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Event.Repository = "owner/repository"
			job.Event.Ref = "refs/heads/main"
			if test.mutate != nil {
				test.mutate(&job)
			}
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			t.Setenv("BUILDKITE_ORGANIZATION_ID", "organization-id")
			t.Setenv("BUILDKITE_PIPELINE_ID", "pipeline-id")
			cachePath := test.cachePath(t)
			t.Setenv("BUILDKITE_GHA_CACHE_DIR", cachePath)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != test.wantCode {
				t.Fatalf("run() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			if test.wantDiagnostic != "" && !strings.Contains(stderr.String(), test.wantDiagnostic) {
				t.Fatalf("run() stderr = %q, want %q", stderr.String(), test.wantDiagnostic)
			}
			matches, err := filepath.Glob(filepath.Join(cachePath, "buildkite-gha-cache-v1", "*"))
			if err != nil {
				t.Fatal(err)
			}
			if test.wantCacheDirs {
				if len(matches) != 3 {
					t.Fatalf("cache directories = %#v, want 3", matches)
				}
			} else if len(matches) != 0 {
				t.Fatalf("cache directories = %#v, want none", matches)
			}
		})
	}
}
