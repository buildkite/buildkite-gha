package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

type countingSecretResolver struct {
	values map[string]string
	calls  map[string]int
}

func (s *countingSecretResolver) ResolveSecret(_ context.Context, name string) (string, error) {
	s.calls[name]++
	value, ok := s.values[name]
	if !ok {
		return "", fmt.Errorf("denied")
	}
	return value, nil
}

func TestRunJobResolvesOriginalSecretOnceAndProjectsAliases(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/aliases.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: aliases\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "aliases", Kind: "run", Command: `test "${{ secrets.FIRST }}" = shared-value
test "${{ secrets.SECOND }}" = shared-value
test -z "${{ secrets.OPTIONAL }}"
echo "first=${{ secrets.FIRST }} second=${{ secrets.SECOND }}"`,
	}})
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"ORIGINAL"}
	job.SecretMappings = map[string]string{"FIRST": "ORIGINAL", "SECOND": "ORIGINAL"}
	resolver := &countingSecretResolver{values: map[string]string{"ORIGINAL": "shared-value"}, calls: map[string]int{}}
	redactor := &testRedactor{}
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: resolver, Redactor: redactor}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	if resolver.calls["ORIGINAL"] != 1 || len(resolver.calls) != 1 || !slices.Equal(redactor.values, []string{"shared-value"}) {
		t.Fatalf("secret resolution calls = %#v, redactions = %#v", resolver.calls, redactor.values)
	}
	if strings.Contains(logs.String(), "shared-value") || !strings.Contains(logs.String(), "first=*** second=***") {
		t.Fatalf("RunJob() logs = %q", logs.String())
	}
}

func TestCompiledBracketSecretResolvesAndMasks(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/secrets.yml"
	source := "on: push\njobs:\n  secrets:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo \"secret=${{ secrets['TOKEN'] }}\"\n"
	writeFixtureFile(t, workspace, workflowPath, source)
	event, err := os.ReadFile(fixturePath(t, "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(filepath.Join(workspace, workflowPath), []byte(source), event, "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !slices.Equal(plans[0].RequiredSecrets, []string{"TOKEN"}) || !plans[0].HasCapability("secrets") {
		t.Fatalf("compiled secret boundary = %#v", plans)
	}
	var logs bytes.Buffer
	redactor := &testRedactor{}
	result, err := (Runner{Stdout: &logs, Stderr: &logs, Secrets: testSecretResolver{"TOKEN": "secret-value"}, Redactor: redactor}).runTestJob(t.Context(), plans[0], workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if strings.Contains(logs.String(), "secret-value") || !strings.Contains(logs.String(), "secret=***") || !slices.Equal(redactor.values, []string{"secret-value"}) {
		t.Fatalf("logs = %q, redactions = %#v", logs.String(), redactor.values)
	}
}

func TestAgentSecretsUsesOnlyJobBoundConfiguration(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	writeFixtureFile(t, filepath.Dir(agent), filepath.Base(agent), `#!/bin/sh
test "$#" -eq 3 || exit 10
test "$1" = secret && test "$2" = get && test "$3" = HOMEBREW_TAP_GITHUB_TOKEN || exit 11
test "$BUILDKITE_JOB_ID" = job-id || exit 12
test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-token || exit 13
test "$BUILDKITE_AGENT_ENDPOINT" = https://agent.example/v3 || exit 14
test "$BUILDKITE_AGENT_JOB_API_SOCKET" = /tmp/job-api.sock || exit 15
test "$BUILDKITE_AGENT_JOB_API_TOKEN" = job-api-token || exit 16
test "$BUILDKITE_NO_HTTP2" = true || exit 17
test "$HTTP_PROXY" = http://upper-http.example:8080 || exit 18
test "$HTTPS_PROXY" = http://upper-https.example:8080 || exit 19
test "$ALL_PROXY" = socks5://upper-all.example:1080 || exit 20
test "$NO_PROXY" = upper-no-proxy.example || exit 21
test "$http_proxy" = http://lower-http.example:8080 || exit 22
test "$https_proxy" = http://lower-https.example:8080 || exit 23
test "$all_proxy" = socks5://lower-all.example:1080 || exit 24
test "$no_proxy" = lower-no-proxy.example || exit 25
test "$SSL_CERT_FILE" = /etc/buildkite/ca.pem || exit 26
test "$SSL_CERT_DIR" = /etc/buildkite/certs || exit 27
test -z "${AMBIENT_SECRET+x}" || exit 28
printf '%s\n' tap-secret
`)
	if err := os.Chmod(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_AGENT_JOB_API_SOCKET", "/tmp/job-api.sock")
	t.Setenv("BUILDKITE_AGENT_JOB_API_TOKEN", "job-api-token")
	t.Setenv("AMBIENT_SECRET", "must-not-be-inherited")
	transportEnvironment := map[string]string{
		"HTTP_PROXY": "http://upper-http.example:8080", "HTTPS_PROXY": "http://upper-https.example:8080",
		"ALL_PROXY": "socks5://upper-all.example:1080", "NO_PROXY": "upper-no-proxy.example",
		"http_proxy": "http://lower-http.example:8080", "https_proxy": "http://lower-https.example:8080",
		"all_proxy": "socks5://lower-all.example:1080", "no_proxy": "lower-no-proxy.example",
		"SSL_CERT_FILE": "/etc/buildkite/ca.pem", "SSL_CERT_DIR": "/etc/buildkite/certs",
	}
	for name, value := range transportEnvironment {
		t.Setenv(name, value)
	}
	resolved, err := resolveAgentSecretsBeforeWorkflow(AgentSecrets{
		Executable: agent,
		Endpoint:   "https://agent.example/v3",
		JobID:      "job-id",
		JobToken:   "job-token",
		NoHTTP2:    "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range transportEnvironment {
		t.Setenv(name, "http://workflow-controlled.example")
	}
	value, err := resolved.ResolveSecret(t.Context(), "HOMEBREW_TAP_GITHUB_TOKEN")
	if err != nil || value != "tap-secret" {
		t.Fatalf("ResolveSecret() = %q, %v", value, err)
	}
}

func TestAgentSecretsDoesNotReturnCommandOutputOnFailure(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	writeFixtureFile(t, filepath.Dir(agent), filepath.Base(agent), "#!/bin/sh\nprintf 'stdout-secret'\nprintf 'stderr-secret' >&2\nexit 1\n")
	if err := os.Chmod(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (AgentSecrets{Executable: agent}).ResolveSecret(t.Context(), "DENIED")
	if err == nil || strings.Contains(err.Error(), "stdout-secret") || strings.Contains(err.Error(), "stderr-secret") || !strings.Contains(err.Error(), "secret is unavailable") {
		t.Fatalf("ResolveSecret() error = %v", err)
	}
}

func TestResolveAgentRedactorBeforeWorkflowPinsPointerWithoutMutatingCaller(t *testing.T) {
	realDir := canonicalTempDir(t)
	realAgent := filepath.Join(realDir, "buildkite-agent")
	writeFixtureFile(t, realDir, "buildkite-agent", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(realAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	lookupDir := t.TempDir()
	lookupAgent := filepath.Join(lookupDir, "buildkite-agent")
	if err := os.Symlink(realAgent, lookupAgent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", lookupDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	configured := &AgentRedactor{}
	resolved, err := resolveAgentRedactorBeforeWorkflow(configured)
	if err != nil {
		t.Fatal(err)
	}
	pinned, ok := resolved.(*AgentRedactor)
	if !ok || pinned == configured || pinned.Executable != realAgent {
		t.Fatalf("resolved redactor = %#v, want independent pointer pinned to %q", resolved, realAgent)
	}
	if configured.Executable != "" {
		t.Fatalf("configured redactor mutated to %q", configured.Executable)
	}
}

func TestAgentRedactorSatisfiesCLIValidationWithoutExposingAgentCredential(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	writeFixtureFile(t, filepath.Dir(agent), filepath.Base(agent), `#!/bin/sh
test "$#" -eq 3 || { echo 'Missing agent-access-token. See: buildkite-agent redactor add --help' >&2; exit 10; }
test "$1" = "redactor" || exit 11
test "$2" = "add" || exit 12
test "$3" = "--agent-access-token=unused" || { echo 'Missing agent-access-token. See: buildkite-agent redactor add --help' >&2; exit 13; }
test "${BUILDKITE_AGENT_JOB_API_SOCKET-}" = "/tmp/job-api.sock" || exit 11
test "${BUILDKITE_AGENT_JOB_API_TOKEN-}" = "job-api-token" || exit 12
test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}" || exit 13
test -z "${BUILDKITE_AGENT_ENDPOINT+x}" || exit 14
test -z "${AMBIENT_SECRET+x}" || exit 15
`)
	if err := os.Chmod(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_AGENT_JOB_API_SOCKET", "/tmp/job-api.sock")
	t.Setenv("BUILDKITE_AGENT_JOB_API_TOKEN", "job-api-token")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "agent-access-token")
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", "https://agent.example/v3")
	t.Setenv("AMBIENT_SECRET", "must-not-be-inherited")

	if err := (AgentRedactor{Executable: agent}).AddRedaction(t.Context(), "redact-me"); err != nil {
		t.Fatal(err)
	}
}
