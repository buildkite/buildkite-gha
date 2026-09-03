package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

// TestRunCompileRequiresAgentContextForEnvironments proves standalone compile
// has no environment access: a workflow that declares an environment fails
// with a concise error pointing at the job-scoped Agent API instead of
// resolving as unprotected or asking for a GitHub token.
func TestRunCompileRequiresAgentContextForEnvironments(t *testing.T) {
	for _, name := range []string{"BUILDKITE_AGENT_ENDPOINT", "BUILDKITE_JOB_ID", "BUILDKITE_AGENT_ACCESS_TOKEN"} {
		t.Setenv(name, "")
	}
	workflow := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(workflow, []byte(environmentUploadWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"compile", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code == 0 {
		t.Fatalf("Run() without Agent context succeeded:\n%s", stdout.String())
	}
	for _, want := range []string{
		`job "deploy" declares environment "production"`,
		"resolve only through the job-scoped Buildkite Agent API",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "token") {
		t.Fatalf("stderr = %q, must not suggest a GitHub token", stderr.String())
	}
}

// TestRunCompileResolvesEnvironmentsThroughAgent exercises compile inside a
// Buildkite job: the environment resolves through the job-scoped Agent API
// snapshot endpoint and the pipeline gains the approval gate. A backend
// rejection fails the compile instead of degrading to an unprotected
// deployment.
func TestRunCompileResolvesEnvironmentsThroughAgent(t *testing.T) {
	agent, requests := agentEnvironmentsStub(t, "job-secret", http.StatusOK)
	setAgentResolutionEnvironment(t, agent.URL)
	workflow := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(workflow, []byte(environmentUploadWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"compile", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if *requests != 1 {
		t.Fatalf("agent resolution requests = %d, want 1", *requests)
	}
	pipeline := stdout.String()
	for _, want := range []string{
		`- block: ":github: approval · production"`,
		`key: "gha-approve-production-552921f9cfcf"`,
		`- step: "gha-approve-production-552921f9cfcf"`, // deploy depends on the gate
	} {
		if !strings.Contains(pipeline, want) {
			t.Fatalf("pipeline missing %q:\n%s", want, pipeline)
		}
	}

	rejecting, _ := agentEnvironmentsStub(t, "job-secret", http.StatusBadRequest)
	setAgentResolutionEnvironment(t, rejecting.URL)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"compile", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code == 0 {
		t.Fatalf("Run() with rejected resolution succeeded:\n%s", stdout.String())
	} else if !strings.Contains(stderr.String(), "environments: the environment resolution request was rejected") {
		t.Fatalf("stderr = %q, want actionable backend rejection", stderr.String())
	}
}

// agentEnvironmentsStub serves the Agent API endpoints upload contacts:
// runner resolution, which reports every requirement unmapped, and
// github-actions/environments, which rejects requests without the job token,
// counts resolution requests, and answers each requested environment with a
// required-reviewers snapshot naming one DEPLOY_KEY secret.
func agentEnvironmentsStub(t *testing.T, jobToken string, status int) (*httptest.Server, *int) {
	t.Helper()
	requests := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/github-actions/runners") {
			var body struct {
				Requirements []struct {
					ID string `json:"id"`
				} `json:"requirements"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode runner requirements: %v", err)
			}
			resolutions := make([]map[string]any, len(body.Requirements))
			for i, requirement := range body.Requirements {
				resolutions[i] = map[string]any{"id": requirement.ID, "error": map[string]string{"code": "unmapped_labels", "message": "No compatible runner is configured."}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"resolutions": resolutions})
			return
		}
		*requests++
		if r.URL.Path != "/jobs/11111111-1111-4111-8111-111111111111/github-actions/environments" {
			t.Errorf("agent path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Token "+jobToken {
			t.Errorf("agent authorization = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			RepositoryURL    string   `json:"repo_url"`
			EnvironmentNames []string `json:"environment_names"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode resolution request: %v", err)
		}
		if body.RepositoryURL != "https://github.com/buildkite/buildkite-gha" || len(body.EnvironmentNames) == 0 {
			t.Errorf("resolution request = %#v", body)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		environments := make([]map[string]any, len(body.EnvironmentNames))
		for i, name := range body.EnvironmentNames {
			environments[i] = map[string]any{
				"name":                name,
				"required_reviewers":  true,
				"prevent_self_review": false,
				"wait_timer_minutes":  0,
				"branch_policy":       false,
				"unsupported_rules":   []string{},
				"secret_names":        []string{"DEPLOY_KEY"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"environments": environments})
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// setAgentResolutionEnvironment points the upload at an Agent API stub.
func setAgentResolutionEnvironment(t *testing.T, agentEndpoint string) {
	t.Helper()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", agentEndpoint)
	t.Setenv("BUILDKITE_JOB_ID", "11111111-1111-4111-8111-111111111111")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-secret")
}

const environmentUploadWorkflow = `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: echo "$DEPLOY_KEY"
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
`

// TestRunUploadResolvesEnvironmentsThroughAgent exercises the default hosted
// path: upload resolves the environment through the job-scoped Agent API
// snapshot endpoint and emits the approval gate and environment-scoped secret
// names. No GitHub API call is made.
func TestRunUploadResolvesEnvironmentsThroughAgent(t *testing.T) {
	requireImporterHost(t)
	agent, requests := agentEnvironmentsStub(t, "job-secret", http.StatusOK)
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "environment-agent-resolve-importer")
	workflow := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(workflow, []byte(environmentUploadWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if *requests != 1 {
		t.Fatalf("agent resolution requests = %d, want 1", *requests)
	}
	plans := 0
	for path, content := range runner.uploaded {
		if !strings.Contains(path, "/plans/") {
			continue
		}
		plans++
		if !strings.Contains(string(content), `"PRODUCTION_DEPLOY_KEY"`) {
			t.Fatalf("plan %s missing environment-scoped secret name:\n%s", path, content)
		}
	}
	if plans != 1 {
		t.Fatalf("uploaded plans = %d, want 1", plans)
	}
	pipeline := string(runner.commands[len(runner.commands)-1].stdin)
	for _, want := range []string{
		`block: ":github: approval · production"`,
		`- step: "gha-approve-production-552921f9cfcf"`,
	} {
		if !strings.Contains(pipeline, want) {
			t.Fatalf("uploaded pipeline missing %q:\n%s", want, pipeline)
		}
	}
}

// TestAgentEnvironmentResolverSurfacesBackendBatchRejection proves the
// backend solely owns the batch-size bound: an oversized compilation sends
// its complete deduplicated name list in exactly one request, and the
// backend's stable 400 policy message fails the compile actionably instead
// of the client silently splitting the batch or pre-judging the limit.
func TestAgentEnvironmentResolverSurfacesBackendBatchRejection(t *testing.T) {
	const rejection = "environment_names cannot contain more than 20 names; split the upload or remove environments"
	requests := 0
	var requestedNames []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			EnvironmentNames []string `json:"environment_names"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requestedNames = request.EnvironmentNames
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"message":%q}`, rejection)
	}))
	defer server.Close()
	resolver, err := gharuntime.NewAgentEnvironmentResolver(gharuntime.AgentEnvironmentResolverConfig{
		Endpoint: server.URL, JobID: cliTestJobID, JobToken: "job-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	source := &agentEnvironmentSource{resolver: resolver, resolved: map[string]agentEnvironmentResolution{}}
	names := make([]string, 21)
	for i := range names {
		names[i] = fmt.Sprintf("environment-%d", i)
	}
	// Case-insensitive duplicates collapse before the request.
	_, err = source.ResolveEnvironments(t.Context(), "buildkite", "buildkite-gha", append([]string{"ENVIRONMENT-0"}, names...))
	if err == nil || !strings.Contains(err.Error(), rejection) {
		t.Fatalf("ResolveEnvironments() error = %v, want backend rejection %q", err, rejection)
	}
	if requests != 1 || len(requestedNames) != 21 {
		t.Fatalf("requests = %d with %d names, want one request carrying all 21 distinct names", requests, len(requestedNames))
	}
}

// TestRunUploadBatchesEnvironmentResolutions proves one upload resolves every
// declared environment in one Agent API request even when workflows declare
// disjoint environments: the upload seeds one batch carrying every distinct
// name before per-workflow compilation, so the whole upload consumes one
// request against the backend's per-job budget instead of one per workflow.
func TestRunUploadBatchesEnvironmentResolutions(t *testing.T) {
	requireImporterHost(t)
	agent, requests := agentEnvironmentsStub(t, "job-secret", http.StatusOK)
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "environment-agent-batch-importer")
	repository := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	runGit("init", "-q")
	first := filepath.Join(repository, "deploy.yml")
	if err := os.WriteFile(first, []byte(`on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: echo "$DEPLOY_KEY"
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
  deploy-staging:
    runs-on: ubuntu-latest
    environment: staging
    steps: [{run: true}]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(repository, "promote.yml")
	if err := os.WriteFile(second, []byte(`on: push
jobs:
  promote:
    runs-on: ubuntu-latest
    environment: canary
    steps: [{run: true}]
  promote-again:
    runs-on: ubuntu-latest
    environment: Production
    steps: [{run: true}]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "deploy.yml", "promote.yml")
	eventPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, "deploy.yml", "promote.yml"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if *requests != 1 {
		t.Fatalf("agent resolution requests = %d, want 1", *requests)
	}
	pipeline := string(runner.commands[len(runner.commands)-1].stdin)
	for _, want := range []string{
		`block: ":github: approval · production"`,
		`block: ":github: approval · staging"`,
		`block: ":github: approval · canary"`,
	} {
		if !strings.Contains(pipeline, want) {
			t.Fatalf("uploaded pipeline missing %q:\n%s", want, pipeline)
		}
	}
}

// TestRunUploadAgentResolutionFailureFailsClosed proves a disabled or failing
// Agent API environments endpoint fails the upload with an actionable error
// instead of degrading to an unprotected deployment.
func TestRunUploadAgentResolutionFailureFailsClosed(t *testing.T) {
	requireImporterHost(t)
	agent, _ := agentEnvironmentsStub(t, "job-secret", http.StatusNotFound)
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "environment-agent-fail-importer")
	workflow := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(workflow, []byte(environmentUploadWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev", runner); code == 0 {
		t.Fatal("run() with failing environment resolution succeeded")
	}
	if want := "the Agent API does not offer GitHub environment resolution"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if strings.Contains(stderr.String(), "token") {
		t.Fatalf("stderr = %q, must not suggest a GitHub token", stderr.String())
	}
}

func TestAgentEnvironmentSourceRejectsPrefixCollisions(t *testing.T) {
	adapter := &agentEnvironmentSource{}
	if err := adapter.recordPrefix("prod-us"); err != nil {
		t.Fatalf("recordPrefix(prod-us) = %v", err)
	}
	// The same environment resolves repeatedly across workflows without error.
	if err := adapter.recordPrefix("prod-us"); err != nil {
		t.Fatalf("recordPrefix(prod-us) repeat = %v", err)
	}
	if err := adapter.recordPrefix("staging"); err != nil {
		t.Fatalf("recordPrefix(staging) = %v", err)
	}
	// GitHub environment names are case-insensitive, so case variants of one
	// environment never collide.
	if err := adapter.recordPrefix("PROD-US"); err != nil {
		t.Fatalf("recordPrefix(PROD-US) = %v", err)
	}
	err := adapter.recordPrefix("prod/us")
	if err == nil || !strings.Contains(err.Error(), `both resolve to Buildkite secret prefix "PROD_US"`) {
		t.Fatalf("recordPrefix(prod/us) error = %v, want prefix collision error", err)
	}
}
