package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// Distinctive repository and organization variable values prove where they
// appear: in job plans only, never in the pipeline, output, or diagnostics.
const (
	stubRepositoryRegion     = "eu-west-1-repository-stub-value"
	stubOrganizationRegion   = "us-east-1-organization-stub-value"
	stubOrganizationRegistry = "ghcr.io/acme-organization-stub-value"
)

var (
	stubRepositoryVariables   = map[string]string{"AWS_REGION": stubRepositoryRegion, "REGIONS": `["eu","us"]`, "TARGET": "prod"}
	stubOrganizationVariables = map[string]string{"AWS_REGION": stubOrganizationRegion, "REGISTRY": stubOrganizationRegistry}
)

// variablesUploadWorkflow reads vars in compile-time positions, the workflow
// concurrency group and a matrix, and in a runtime step. It declares no
// environment.
const variablesUploadWorkflow = `on: push
concurrency: deploy-${{ vars.TARGET }}
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        region: ${{ fromJSON(vars.REGIONS) }}
    steps:
      - run: echo "${{ vars.AWS_REGION }}" "${{ vars.REGISTRY }}" "${{ matrix.region }}"
`

// agentVariablesHandler answers github-actions/variables with the stub
// scopes after checking the job-scoped request, or with status and
// retryAfter when status is not 200. It counts requests.
func agentVariablesHandler(t *testing.T, status int, retryAfter string) (http.HandlerFunc, *int) {
	t.Helper()
	requests := new(int)
	return func(w http.ResponseWriter, r *http.Request) {
		*requests++
		if r.Method != http.MethodPost || r.URL.Path != "/jobs/11111111-1111-4111-8111-111111111111/github-actions/variables" {
			t.Errorf("variables request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Token job-secret" {
			t.Errorf("variables authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode variables request: %v", err)
		}
		if !maps.Equal(body, map[string]any{"repo_url": "https://github.com/buildkite/buildkite-gha"}) {
			t.Errorf("variables request body = %#v", body)
		}
		if status != http.StatusOK {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			return
		}
		scope := func(values map[string]string) []map[string]string {
			entries := make([]map[string]string, 0, len(values))
			for _, name := range []string{"AWS_REGION", "REGIONS", "REGISTRY", "TARGET"} {
				if value, ok := values[name]; ok {
					entries = append(entries, map[string]string{"name": name, "value": value})
				}
			}
			return entries
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repository_variables":   scope(stubRepositoryVariables),
			"organization_variables": scope(stubOrganizationVariables),
		})
	}, requests
}

// pushEventPath is the smoke push event, absolute because upload tests enter
// a temporary checkout.
func pushEventPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// writeUploadWorkflows writes the workflows into a git checkout, enters it,
// and returns their repository-relative paths in a stable order.
func writeUploadWorkflows(t *testing.T, sources map[string]string) []string {
	t.Helper()
	repository := writeUploadWorkflowRepository(t, sources)
	t.Chdir(repository)
	paths := make([]string, 0, len(sources))
	for _, name := range []string{"build.yml", "deploy.yml", "plain.yml"} {
		if _, ok := sources[name]; ok {
			paths = append(paths, filepath.Join(".github", "workflows", name))
		}
	}
	return paths
}

func uploadedPlans(t *testing.T, runner *cliCaptureRunner) map[string][]plan.Job {
	t.Helper()
	plans := map[string][]plan.Job{}
	for path, content := range runner.uploaded {
		if !strings.Contains(path, "/plans/") {
			continue
		}
		job, err := plan.Decode(content)
		if err != nil {
			t.Fatalf("decode uploaded plan %s: %v", path, err)
		}
		plans[job.Workflow.LogicalJobID] = append(plans[job.Workflow.LogicalJobID], job)
	}
	return plans
}

func assertNoVariableValueLeak(t *testing.T, runner *cliCaptureRunner, stdout, stderr string) {
	t.Helper()
	pipeline := string(runner.commands[len(runner.commands)-1].stdin)
	for _, value := range []string{stubRepositoryRegion, stubOrganizationRegion, stubOrganizationRegistry, stubEnvironmentRegion} {
		if strings.Contains(pipeline, value) || strings.Contains(stdout, value) || strings.Contains(stderr, value) {
			t.Fatalf("variable value %q leaked outside job plans:\n%s\n%s\n%s", value, pipeline, stdout, stderr)
		}
	}
	for path, content := range runner.uploaded {
		// Plans carry the values by design; the distribution is this test
		// binary, which contains the literals.
		if strings.Contains(path, "/plans/") || strings.Contains(path, "/distributions/") {
			continue
		}
		for _, value := range []string{stubRepositoryRegion, stubOrganizationRegion, stubOrganizationRegistry, stubEnvironmentRegion} {
			if bytes.Contains(content, []byte(value)) {
				t.Fatalf("variable value %q leaked into artifact %s", value, path)
			}
		}
	}
}

// TestRunUploadResolvesVariablesOncePerUpload exercises the hosted upload
// path with repository and organization variables: two workflows reading
// vars produce exactly one job-scoped Agent API request, compile-time fields
// (the concurrency group and a matrix) resolve from the scopes, every job
// plan carries both scopes, and the runtime vars context prefers environment
// over repository over organization values. A workflow without an
// environment works.
func TestRunUploadResolvesVariablesOncePerUpload(t *testing.T) {
	requireImporterHost(t)
	variables, variableRequests := agentVariablesHandler(t, http.StatusOK, "")
	agent, environmentRequests := agentStub(t, "job-secret", http.StatusOK, variables)
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "variables-agent-resolve-importer")
	eventPath := pushEventPath(t)
	workflows := writeUploadWorkflows(t, map[string]string{"build.yml": variablesUploadWorkflow, "deploy.yml": environmentUploadWorkflow})

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"upload", "--event-path", eventPath}, workflows...), &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if *variableRequests != 1 || *environmentRequests != 1 {
		t.Fatalf("agent requests = %d variables, %d environments; want 1 and 1", *variableRequests, *environmentRequests)
	}
	plans := uploadedPlans(t, runner)
	if len(plans["build"]) != 2 || len(plans["deploy"]) != 1 || len(plans) != 2 {
		t.Fatalf("uploaded plans by job = %v", plans)
	}
	regions := map[any]bool{}
	for _, jobs := range plans {
		for _, job := range jobs {
			if !maps.Equal(job.RepositoryVars, stubRepositoryVariables) || !maps.Equal(job.OrganizationVars, stubOrganizationVariables) {
				t.Fatalf("plan %q scopes = repository %#v, organization %#v", job.Workflow.LogicalJobID, job.RepositoryVars, job.OrganizationVars)
			}
			regions[job.Matrix["region"]] = true
		}
	}
	if !regions["eu"] || !regions["us"] {
		t.Fatalf("matrix regions = %v, want the fromJSON(vars.REGIONS) values", regions)
	}
	// environment > repository > organization.
	if got := plans["deploy"][0].Vars(); got["AWS_REGION"] != stubEnvironmentRegion || got["REGISTRY"] != stubOrganizationRegistry || got["TARGET"] != "prod" {
		t.Fatalf("deploy vars = %#v", got)
	}
	if got := plans["build"][0].Vars(); got["AWS_REGION"] != stubRepositoryRegion || got["REGISTRY"] != stubOrganizationRegistry || len(plans["build"][0].EnvironmentVars) != 0 {
		t.Fatalf("build vars = %#v", got)
	}
	assertNoVariableValueLeak(t, runner, stdout.String(), stderr.String())
}

// TestRunUploadSkipsVariableResolutionWithoutReferences proves a workflow
// that never reads vars costs no request against the per-job budget.
func TestRunUploadSkipsVariableResolutionWithoutReferences(t *testing.T) {
	requireImporterHost(t)
	agent, _ := agentStub(t, "job-secret", http.StatusOK, func(http.ResponseWriter, *http.Request) {
		t.Error("variables were requested for a workflow without vars references")
	})
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "variables-agent-skip-importer")
	eventPath := pushEventPath(t)
	workflows := writeUploadWorkflows(t, map[string]string{"plain.yml": "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo \"${{ github.sha }}\"\n"})

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"upload", "--event-path", eventPath}, workflows...), &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	plans := uploadedPlans(t, runner)
	if len(plans["test"]) != 1 || plans["test"][0].RepositoryVars != nil || plans["test"][0].OrganizationVars != nil {
		t.Fatalf("uploaded plans = %v", plans)
	}
}

// undefinedVariablesWorkflow reads a variable no scope defines in a
// compile-time field with a literal fallback, as failover runner selection
// does, and in a runtime step.
const undefinedVariablesWorkflow = `on: push
jobs:
  test:
    runs-on: ${{ vars.CI_FAILOVER_LINUX || 'ubuntu-latest' }}
    steps:
      - run: echo "${{ vars.AWS_REGION }}"
`

// agentEmptyVariablesHandler answers github-actions/variables with status
// and, for 200, a repository and organization that define no variables. It
// counts requests.
func agentEmptyVariablesHandler(t *testing.T, status int) (http.HandlerFunc, *int) {
	t.Helper()
	requests := new(int)
	return func(w http.ResponseWriter, r *http.Request) {
		*requests++
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/github-actions/variables") {
			t.Errorf("variables request = %s %s", r.Method, r.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(`{"repository_variables":[],"organization_variables":[]}`))
	}, requests
}

// TestRunUploadTreatsAbsentVariableScopesAsEmpty proves that a backend
// without the variables endpoint (404) and a repository that defines no
// variables (200 with empty scopes) both resolve every vars reference to an
// empty string, in compile-time fields and at runtime, so the upload succeeds
// with the literal fallback runner. Without a source the same compile-time
// reference fails; TestRunCompileResolvesVariablesThroughAgent covers it.
func TestRunUploadTreatsAbsentVariableScopesAsEmpty(t *testing.T) {
	requireImporterHost(t)
	for name, status := range map[string]int{"endpoint absent": http.StatusNotFound, "scopes empty": http.StatusOK} {
		t.Run(name, func(t *testing.T) {
			variables, variableRequests := agentEmptyVariablesHandler(t, status)
			agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
			setAgentResolutionEnvironment(t, agent.URL)
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "variables-agent-absent-importer")
			eventPath := pushEventPath(t)
			workflows := writeUploadWorkflows(t, map[string]string{"plain.yml": undefinedVariablesWorkflow})

			runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
			var stdout, stderr bytes.Buffer
			if code := run(append([]string{"upload", "--event-path", eventPath}, workflows...), &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
			}
			if *variableRequests != 1 {
				t.Fatalf("variables requests = %d, want 1", *variableRequests)
			}
			plans := uploadedPlans(t, runner)
			if len(plans["test"]) != 1 || len(plans["test"][0].Vars()) != 0 {
				t.Fatalf("uploaded plans = %v", plans)
			}
			pipeline := string(runner.commands[len(runner.commands)-1].stdin)
			if !strings.Contains(pipeline, defaultNobleRunnerImage) {
				t.Fatalf("pipeline did not select the literal fallback runner ubuntu-latest:\n%s", pipeline)
			}
		})
	}
}

// TestRunUploadResolvesVariablesWhenAnotherWorkflowHasRuntimeMatrix proves a
// workflow whose reusable callee derives its matrix from a job output does not
// stop the upload before variables are resolved. The plain workflow's
// `vars.*` runner fallback resolves through the agent and uploads, while the
// runtime-matrix workflow becomes a failed check with the matrix diagnostic
// instead of a spurious "unavailable value vars.*" error for the whole upload.
func TestRunUploadResolvesVariablesWhenAnotherWorkflowHasRuntimeMatrix(t *testing.T) {
	requireImporterHost(t)
	variables, variableRequests := agentEmptyVariablesHandler(t, http.StatusOK)
	agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "variables-runtime-matrix-importer")
	eventPath := pushEventPath(t)
	workflows := writeUploadWorkflows(t, map[string]string{
		"build.yml": `on: push
jobs:
  lint:
    runs-on: ${{ vars.CI_FAILOVER_LINUX || 'ubuntu-latest' }}
    steps:
      - run: true
  build:
    uses: ./.github/workflows/runtime-matrix.yml
`,
		"plain.yml": undefinedVariablesWorkflow,
	})
	if err := os.WriteFile(filepath.Join(".github", "workflows", "runtime-matrix.yml"), []byte(`on: workflow_call
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"runner":"ubuntu-latest"}]' >> "$GITHUB_OUTPUT"
  build:
    needs: plan
    runs-on: ${{ matrix.runner }}
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: true
`), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"upload", "--event-path", eventPath}, workflows...), &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if *variableRequests != 1 {
		t.Fatalf("variables requests = %d, want 1", *variableRequests)
	}
	output := stdout.String() + stderr.String()
	if strings.Contains(output, "vars.") {
		t.Fatalf("upload reported a variables error although variables resolved:\n%s", output)
	}
	for _, want := range []string{"[E_MATRIX_INVALID]", "runtime-matrix.yml", "Uploaded 1 jobs from 2 workflows"} {
		if !strings.Contains(output, want) {
			t.Fatalf("upload output missing %q:\n%s", want, output)
		}
	}
	plans := uploadedPlans(t, runner)
	if len(plans) != 1 || len(plans["test"]) != 1 {
		t.Fatalf("uploaded plans by job = %v, want only the plain workflow", plans)
	}
	pipeline := string(runner.commands[len(runner.commands)-1].stdin)
	for _, want := range []string{defaultNobleRunnerImage, `label: ":github: workflow · .github/workflows/build.yml"`, `title: "Workflow could not be run"`} {
		if !strings.Contains(pipeline, want) {
			t.Fatalf("pipeline missing %q:\n%s", want, pipeline)
		}
	}
}

// TestRunUploadSurfacesVariableResolutionRateLimit proves a 429 fails every
// workflow that reads vars with the backend's Retry-After delay, after one
// request, while a workflow without vars references still uploads.
func TestRunUploadSurfacesVariableResolutionRateLimit(t *testing.T) {
	requireImporterHost(t)
	variables, variableRequests := agentVariablesHandler(t, http.StatusTooManyRequests, "3600")
	agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "variables-agent-limited-importer")
	eventPath := pushEventPath(t)
	workflows := writeUploadWorkflows(t, map[string]string{
		"build.yml":  variablesUploadWorkflow,
		"deploy.yml": environmentUploadWorkflow,
		"plain.yml":  "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo plain\n",
	})

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"upload", "--event-path", eventPath}, workflows...), &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if *variableRequests != 1 {
		t.Fatalf("variables requests = %d, want 1 for two workflows reading vars", *variableRequests)
	}
	plans := uploadedPlans(t, runner)
	if len(plans) != 1 || len(plans["test"]) != 1 {
		t.Fatalf("uploaded plans by job = %v, want only the workflow without vars references", plans)
	}
	const want = "Error: variables: variable resolution requests are rate limited; retry after 3600 seconds"
	failures := 0
	for path, content := range runner.uploaded {
		if strings.HasPrefix(path, ".buildkite-gha/failures/messages/") && strings.Contains(string(content), want) {
			failures++
		}
	}
	// Each failure carries its own workflow attribution, even when the cause matches.
	if failures != 2 {
		t.Fatalf("failure message artifacts with %q = %d, want 2:\n%s", want, failures, stderr.String())
	}
	pipeline := string(runner.commands[len(runner.commands)-1].stdin)
	for _, want := range []string{`label: ":github: workflow · .github/workflows/build.yml"`, `label: ":github: workflow · .github/workflows/deploy.yml"`, `title: "Workflow could not be run"`} {
		if !strings.Contains(pipeline, want) {
			t.Fatalf("pipeline missing failed workflow step %s:\n%s", want, pipeline)
		}
	}
}

// TestRunCompileResolvesVariablesThroughAgent exercises compile inside a
// Buildkite job: vars resolve through the job-scoped Agent API so
// compile-time fields succeed, a rejection fails the compile, and compile
// outside a job makes no request and leaves compile-time vars unresolved.
func TestRunCompileResolvesVariablesThroughAgent(t *testing.T) {
	variables, variableRequests := agentVariablesHandler(t, http.StatusOK, "")
	agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
	setAgentResolutionEnvironment(t, agent.URL)
	eventPath := pushEventPath(t)
	workflow := writeUploadWorkflows(t, map[string]string{"build.yml": variablesUploadWorkflow})[0]

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"compile", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if *variableRequests != 1 {
		t.Fatalf("variables requests = %d, want 1", *variableRequests)
	}
	pipeline := stdout.String()
	for _, want := range []string{"build (region=eu)", "build (region=us)"} {
		if !strings.Contains(pipeline, want) {
			t.Fatalf("pipeline missing %q:\n%s", want, pipeline)
		}
	}
	for _, value := range []string{stubRepositoryRegion, stubOrganizationRegion, stubOrganizationRegistry} {
		if strings.Contains(pipeline, value) || strings.Contains(stderr.String(), value) {
			t.Fatalf("variable value %q leaked into compile output:\n%s\n%s", value, pipeline, stderr.String())
		}
	}

	rejecting, _ := agentVariablesHandler(t, http.StatusBadRequest, "")
	agent, _ = agentStub(t, "job-secret", http.StatusOK, rejecting)
	setAgentResolutionEnvironment(t, agent.URL)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"compile", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code == 0 {
		t.Fatalf("Run() with rejected resolution succeeded:\n%s", stdout.String())
	} else if !strings.Contains(stderr.String(), "[E_ENVIRONMENT] variables: the variable resolution request was rejected") {
		t.Fatalf("stderr = %q, want actionable backend rejection", stderr.String())
	}

	for _, name := range []string{"BUILDKITE_AGENT_ENDPOINT", "BUILDKITE_JOB_ID", "BUILDKITE_AGENT_ACCESS_TOKEN"} {
		t.Setenv(name, "")
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"compile", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code == 0 {
		t.Fatalf("Run() outside a job resolved compile-time vars:\n%s", stdout.String())
	} else if !strings.Contains(stderr.String(), `compile-time expression references unavailable value "vars.target"`) {
		t.Fatalf("stderr = %q, want unresolved compile-time vars", stderr.String())
	}
}

// actionDefaultWorkflow references no vars itself; its only vars reference is
// the input default of the local action it uses.
const actionDefaultWorkflow = "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/region\n"

// regionInputDefaultAction is a JavaScript action whose input default reads
// vars; regionCompositeConditionAction is a composite action whose only vars
// reference is a bare step condition, which GitHub evaluates as an expression
// without ${{ }}.
const (
	regionInputDefaultAction       = "name: region\ninputs:\n  region:\n    default: ${{ vars.AWS_REGION }}\nruns:\n  using: node24\n  main: main.js\n"
	regionCompositeConditionAction = "name: region\nruns:\n  using: composite\n  steps:\n    - if: vars.AWS_REGION == 'eu'\n      run: echo eu\n      shell: bash\n"
)

// writeRegionAction writes a local action with the given metadata into the
// current checkout.
func writeRegionAction(t *testing.T, action string) {
	t.Helper()
	root := filepath.Join(".github", "actions", "region")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "action.yml"), []byte(action), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.js"), []byte("console.log('must not run')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunUploadResolvesVariablesForActionInputDefaults proves a vars
// reference that lives only in a resolved action, as an input default or a
// bare composite step condition, still fills the job plan scopes: compilation discovers it, one request follows, and
// the plans compiled again carry the scopes. A backend failure on that
// request fails the workflow the same way as a workflow-level reference.
func TestRunUploadResolvesVariablesForActionInputDefaults(t *testing.T) {
	requireImporterHost(t)
	for _, tc := range []struct {
		name   string
		action string
		status int
	}{
		{"input default resolved", regionInputDefaultAction, http.StatusOK},
		{"composite condition resolved", regionCompositeConditionAction, http.StatusOK},
		{"rate limited", regionInputDefaultAction, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			variables, variableRequests := agentVariablesHandler(t, tc.status, "60")
			agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
			setAgentResolutionEnvironment(t, agent.URL)
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "variables-agent-action-importer")
			eventPath := pushEventPath(t)
			workflows := writeUploadWorkflows(t, map[string]string{"plain.yml": actionDefaultWorkflow})
			writeRegionAction(t, tc.action)

			runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
			var stdout, stderr bytes.Buffer
			if code := run(append([]string{"upload", "--event-path", eventPath}, workflows...), &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
			}
			if *variableRequests != 1 {
				t.Fatalf("variables requests = %d, want 1", *variableRequests)
			}
			plans := uploadedPlans(t, runner)
			if tc.status != http.StatusOK {
				const want = "Error: variables: variable resolution requests are rate limited; retry after 60 seconds"
				failures := 0
				for path, content := range runner.uploaded {
					if strings.HasPrefix(path, ".buildkite-gha/failures/messages/") && strings.Contains(string(content), want) {
						failures++
					}
				}
				if len(plans) != 0 || failures != 1 {
					t.Fatalf("plans = %v, failure artifacts with %q = %d; want no plans and one failure", plans, want, failures)
				}
				return
			}
			if len(plans["test"]) != 1 || !maps.Equal(plans["test"][0].RepositoryVars, stubRepositoryVariables) || !maps.Equal(plans["test"][0].OrganizationVars, stubOrganizationVariables) {
				t.Fatalf("uploaded plans = %#v, want the resolved scopes", plans)
			}
			assertNoVariableValueLeak(t, runner, stdout.String(), stderr.String())
		})
	}
}

func TestRunUploadRecompilesIndependentPartialPlanWithActionVariables(t *testing.T) {
	requireImporterHost(t)
	variables, variableRequests := agentVariablesHandler(t, http.StatusOK, "")
	agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
	setAgentResolutionEnvironment(t, agent.URL)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "variables-partial-importer")
	eventPath := pushEventPath(t)
	workflow := writeUploadWorkflows(t, map[string]string{"build.yml": `on: push
jobs:
  safe:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/region
  broken:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/missing
`})[0]
	writeRegionAction(t, regionInputDefaultAction)

	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"upload", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if *variableRequests != 1 {
		t.Fatalf("variables requests = %d, want 1", *variableRequests)
	}
	plans := uploadedPlans(t, runner)
	if len(plans["safe"]) != 1 || len(plans["broken"]) != 0 || !maps.Equal(plans["safe"][0].RepositoryVars, stubRepositoryVariables) || !maps.Equal(plans["safe"][0].OrganizationVars, stubOrganizationVariables) {
		t.Fatalf("uploaded plans = %#v", plans)
	}
	assertNoVariableValueLeak(t, runner, stdout.String(), stderr.String())
}

// TestRunCompileIRJSONCarriesVariableScopes pins the documented exposure of
// compile --format ir-json: the IR is the compiler's full input, so it prints
// both resolved scopes to stdout.
func TestRunCompileIRJSONCarriesVariableScopes(t *testing.T) {
	variables, _ := agentVariablesHandler(t, http.StatusOK, "")
	agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
	setAgentResolutionEnvironment(t, agent.URL)
	eventPath := pushEventPath(t)
	workflow := writeUploadWorkflows(t, map[string]string{"build.yml": variablesUploadWorkflow})[0]

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"compile", "--format", "ir-json", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var ir compiler.IR
	if err := json.Unmarshal(stdout.Bytes(), &ir); err != nil {
		t.Fatalf("decode IR: %v", err)
	}
	if !maps.Equal(ir.RepositoryVars, stubRepositoryVariables) || !maps.Equal(ir.OrganizationVars, stubOrganizationVariables) {
		t.Fatalf("IR scopes = %#v / %#v, want the resolved scopes", ir.RepositoryVars, ir.OrganizationVars)
	}
}

// TestRunCompileResolvesVariablesForActionInputDefaults proves compile inside
// a Buildkite job also discovers an action-only vars reference and compiles
// the plans with the scopes.
func TestRunCompileResolvesVariablesForActionInputDefaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"resolved", http.StatusOK},
		{"rate limited", http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			variables, variableRequests := agentVariablesHandler(t, tc.status, "60")
			agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
			setAgentResolutionEnvironment(t, agent.URL)
			eventPath := pushEventPath(t)
			workflow := writeUploadWorkflows(t, map[string]string{"plain.yml": actionDefaultWorkflow})[0]
			writeRegionAction(t, regionInputDefaultAction)

			var stdout, stderr bytes.Buffer
			code := Run([]string{"compile", "--event-path", eventPath, workflow}, &stdout, &stderr, "dev")
			if *variableRequests != 1 {
				t.Fatalf("variables requests = %d, want 1", *variableRequests)
			}
			if tc.status != http.StatusOK {
				const want = "buildkite-gha: compile: variables: variable resolution requests are rate limited; retry after 60 seconds\n"
				if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), want) {
					t.Fatalf("Run() code = %d, stdout = %q, stderr = %q; want 1, no pipeline, and %q", code, stdout.String(), stderr.String(), want)
				}
				return
			}
			if code != 0 {
				t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "gha-importer") || strings.Contains(stdout.String(), stubRepositoryRegion) {
				t.Fatalf("pipeline = %q", stdout.String())
			}
		})
	}
}

// TestResolveVariableSourcesSkipsOtherProviders proves events from providers
// other than GitHub.com never reach the GitHub variables endpoint: their
// repositories have no GitHub variables, so the scopes stay empty.
func TestResolveVariableSourcesSkipsOtherProviders(t *testing.T) {
	source := &agentVariableSource{resolved: map[string]agentVariableResolution{}}
	event := compiler.Event{Provider: "cursor-origin", Repository: compiler.Repository{Owner: "acme", Name: "app"}}
	vars, err := resolveVariableSources(t.Context(), source, event, true)
	if err != nil || len(vars.Repository) != 0 || len(vars.Organization) != 0 || len(source.resolved) != 0 {
		t.Fatalf("resolveVariableSources() = %#v, %v; resolved = %v", vars, err, source.resolved)
	}
}

// TestAgentVariableSourceMemoizesFailures proves one source answers repeated
// compiles of the same repository from one request, including after a
// failure, so retries never spend the per-job budget.
func TestAgentVariableSourceMemoizesFailures(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	setAgentResolutionEnvironment(t, server.URL)
	source := variableSourceFromAgent("dev")
	if source == nil {
		t.Fatal("variableSourceFromAgent() = nil with a configured Agent connection")
	}
	for range 3 {
		if _, err := source.ResolveVariables(t.Context(), "buildkite", "buildkite-gha"); err == nil || !strings.Contains(err.Error(), "variables: the variable resolution service is temporarily unavailable") {
			t.Fatalf("ResolveVariables() error = %v", err)
		}
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "")
	if variableSourceFromAgent("dev") != nil {
		t.Fatal("variableSourceFromAgent() != nil without a job token")
	}
}
