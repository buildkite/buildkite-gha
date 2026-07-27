package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestExperimentalDirectoryCacheConfigIsExplicitAndRefScoped(t *testing.T) {
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "Owner/Repository", Ref: "refs/heads/main"}}
	if config, err := experimentalDirectoryCacheConfig(job, func(string) string { return "" }); err != nil || config != nil {
		t.Fatalf("disabled config = %#v, %v, want nil", config, err)
	}

	root := t.TempDir()
	values := map[string]string{
		"BUILDKITE_GHA_CACHE_DIR":         root,
		"BUILDKITE_ORGANIZATION_ID":       "organization-id",
		"BUILDKITE_PIPELINE_ID":           "pipeline-id",
		"BUILDKITE_AGENT_META_DATA_QUEUE": "hosted",
	}
	config, err := experimentalDirectoryCacheConfig(job, func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Backend == nil || config.Namespace.Organization != "organization-id" || config.Namespace.Cluster != "hosted" || config.Namespace.Pipeline != "pipeline-id" {
		t.Fatalf("config = %#v", config)
	}
	if len(config.ReadScopes) != 1 || !strings.HasPrefix(string(config.ReadScopes[0]), "github-ref:sha256:") || len(config.ReadScopes[0]) != len("github-ref:sha256:")+64 || config.WriteScope != config.ReadScopes[0] || config.ReadOnly {
		t.Fatalf("cache scopes = %#v / %q, read-only %v", config.ReadScopes, config.WriteScope, config.ReadOnly)
	}
	if info, err := filepath.Glob(filepath.Join(root, "buildkite-gha-cache-v1", "*")); err != nil || len(info) != 3 {
		t.Fatalf("cache directories = %#v, %v", info, err)
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
	for _, job := range []plan.Job{
		{Event: plan.Event{Provider: "other", Repository: "owner/repository", Ref: "refs/heads/main"}},
		{Event: plan.Event{Provider: "github", Ref: "refs/heads/main"}},
		{Event: plan.Event{Provider: "github", Repository: "owner/repository"}},
		{Event: plan.Event{Provider: "github", Repository: "owner/repository:other", Ref: "refs/heads/main"}},
	} {
		if config, err := experimentalDirectoryCacheConfig(job, getenv(valid)); err == nil || config != nil {
			t.Fatalf("invalid event config = %#v, %v", config, err)
		}
	}
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "owner/repository", Ref: "refs/heads/main"}}
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
	if config, err := experimentalDirectoryCacheConfig(job, getenv(backendValues)); !errors.Is(err, errExperimentalCacheBackend) || config != nil {
		t.Fatalf("unavailable backend config = %#v, %v", config, err)
	}
}

func TestRunJobWiresAndDegradesExperimentalDirectoryCache(t *testing.T) {
	for _, test := range []struct {
		name           string
		cachePath      func(*testing.T) string
		mutate         func(*plan.Job)
		wantCode       int
		wantDiagnostic string
	}{
		{name: "configured", cachePath: func(t *testing.T) string { return t.TempDir() }},
		{name: "unavailable falls back", cachePath: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, wantDiagnostic: "continuing without GitHub cache"},
		{name: "invalid plan identity fails", cachePath: func(t *testing.T) string { return t.TempDir() }, mutate: func(job *plan.Job) {
			job.Event.Repository = ""
		}, wantCode: 1, wantDiagnostic: "requires a GitHub repository and ref"},
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
			if test.wantCode == 0 && test.wantDiagnostic == "" {
				if matches, err := filepath.Glob(filepath.Join(cachePath, "buildkite-gha-cache-v1", "*")); err != nil || len(matches) != 3 {
					t.Fatalf("cache directories = %#v, %v", matches, err)
				}
			}
		})
	}
}
