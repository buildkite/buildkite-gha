package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"go.yaml.in/yaml/v4"
)

func TestPluginTelemetryReportsUnprovenActionRuntime(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"action.yml": "on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n",
	})
	actionPath := filepath.Join(repository, ".github", "actions", "local")
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("runs:\n  using: node24\n  main: main.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "main.js"), []byte("console.log('local action')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	t.Setenv("BUILDKITE_GHA_NODE20", writeFakeNode(t, repository, 20))
	t.Setenv("BUILDKITE_GHA_NODE24", writeFakeNode(t, repository, 24))

	events := make(chan telemetry.Properties, 1)
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
		var received struct {
			Properties telemetry.Properties `json:"properties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode telemetry event: %v", err)
		}
		events <- received.Properties
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")

	configuration, err := json.Marshal(map[string]any{"workflow": filepath.Join(".github", "workflows", "action.yml")})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "telemetry-action-importer")

	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	properties := <-events
	if properties.Command != telemetry.CommandPluginImport || properties.Outcome != telemetry.OutcomeSuccess {
		t.Fatalf("event = %#v", properties)
	}
	if properties.ErrorMessage != "" || properties.ErrorMessageTruncated {
		t.Fatalf("successful event included error output: %#v", properties)
	}
	want := []telemetry.Diagnostic{{Code: "W_ACTION_RUNTIME_UNKNOWN", Severity: telemetry.SeverityWarning}}
	if !reflect.DeepEqual(properties.Diagnostics, want) {
		t.Fatalf("diagnostics = %#v, want %#v", properties.Diagnostics, want)
	}
	for _, command := range runner.commands {
		if len(command.args) != 0 && command.args[0] == "annotate" {
			t.Fatalf("unproven action runtime annotated the build: %#v", command.args)
		}
	}
}

func TestPluginRequiresConfigurationWithoutSideEffects(t *testing.T) {
	requireImporterHost(t)
	t.Setenv(pluginConfigurationEnvironment, "")
	t.Setenv("BUILDKITE_PLUGIN_GITHUB_ACTIONS_WORKFLOW", "legacy.yml")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 2 {
		t.Fatalf("run() code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), pluginConfigurationEnvironment+" is required") || len(runner.commands) != 0 {
		t.Fatalf("stderr = %q, commands = %#v", stderr.String(), runner.commands)
	}
}

func TestPluginRejectsUnknownAndNonBooleanConfigurationWithoutSideEffects(t *testing.T) {
	requireImporterHost(t)
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "unknown field", source: `{"workflow":"ci.yml","unknown":true}`, want: "unknown field"},
		{name: "non-boolean experiment", source: `{"workflow":"ci.yml","experimental-runner-user":"true"}`, want: "must be a boolean"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(pluginConfigurationEnvironment, test.source)
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 2 {
				t.Fatalf("run() code = %d, want 2; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.want) || stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("invalid configuration reached upload: stdout = %q, stderr = %q, commands = %#v, uploads = %#v", stdout.String(), stderr.String(), runner.commands, runner.uploaded)
			}
		})
	}
}

func TestParsePluginConfiguration(t *testing.T) {
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	configuration, err := parsePluginConfiguration(`{
  "workflows": [".github/workflows/ci.yml", ".github/workflows/release.yml"],
  "version": "0.8.0",
  "source-ref": "0123456789abcdef0123456789abcdef01234567",
  "minimum-release-age": "24h",
  "experimental-runner-user": true,
  "oidc": {
    "claims": ["organization_id", "future_server_claim"],
    "aws-session-tags": ["organization_slug", "pipeline_id"],
    "subject-claim": "pipeline_id"
  },
  "runners": [
    {"runs-on":"ubuntu-latest","queue":"hosted","image":"` + image + `"},
    {"runs-on":"macos-14","queue":"macos-sonoma-arm64"}
  ]
}`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(configuration.Workflows, []string{".github/workflows/ci.yml", ".github/workflows/release.yml"}) || !configuration.ExperimentalRunnerUser || len(configuration.runnerTargets) != 2 {
		t.Fatalf("configuration = %#v", configuration)
	}
	if configuration.OIDC == nil || !slices.Equal(configuration.OIDC.Claims, []string{"organization_id", "future_server_claim"}) || !slices.Equal(configuration.OIDC.AWSSessionTags, []string{"organization_slug", "pipeline_id"}) || configuration.OIDC.SubjectClaim != "pipeline_id" {
		t.Fatalf("OIDC configuration = %#v", configuration.OIDC)
	}
	if got := configuration.runnerTargets["ubuntu-latest"]; got != (compiler.RunnerTarget{Queue: "hosted", Platform: compiler.PlatformLinuxAMD64, Image: image}) {
		t.Fatalf("Linux target = %#v", got)
	}
	if got := configuration.runnerTargets["macos-14"]; got != (compiler.RunnerTarget{Queue: "macos-sonoma-arm64", Platform: compiler.PlatformDarwinARM64}) {
		t.Fatalf("Darwin target = %#v", got)
	}
	minimal, err := parsePluginConfiguration(`{"workflow":"workflow.yml"}`)
	if err != nil || !slices.Equal(minimal.Workflows, []string{"workflow.yml"}) || !minimal.ExperimentalRunnerUser || len(minimal.runnerTargets) != 0 {
		t.Fatalf("minimal configuration = %#v, %v", minimal, err)
	}
	disabled, err := parsePluginConfiguration(`{"workflow":"workflow.yml","experimental-runner-user":false}`)
	if err != nil || disabled.ExperimentalRunnerUser {
		t.Fatalf("disabled runner user configuration = %#v, %v", disabled, err)
	}

	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "malformed", source: `{`, want: "decode"},
		{name: "missing workflow selection", source: `{}`, want: "workflow or workflows is required"},
		{name: "duplicate workflow", source: `{"workflow":"one.yml","workflow":"two.yml"}`, want: "duplicate object key"},
		{name: "both workflow fields", source: `{"workflow":"one.yml","workflows":"two.yml"}`, want: "mutually exclusive"},
		{name: "empty workflow", source: `{"workflow":""}`, want: "workflow must be a non-empty string"},
		{name: "workflows string", source: `{"workflows":"one.yml"}`, want: "workflows must be a non-empty array"},
		{name: "empty workflows array", source: `{"workflows":[]}`, want: "workflows must be a non-empty array"},
		{name: "non-string workflow entry", source: `{"workflows":["one.yml",null]}`, want: "workflows entry 1 must be a non-empty string"},
		{name: "empty workflow entry", source: `{"workflows":["one.yml",""]}`, want: "workflows entry 1 must be a non-empty string"},
		{name: "unknown top-level field", source: `{"workflow":"ci.yml","runnerss":[]}`, want: "unknown field"},
		{name: "retired source acquisition field", source: `{"workflow":"ci.yml","buildkite-gha-source-ref":"latest"}`, want: "unknown field"},
		{name: "string experimental runner user", source: `{"workflow":"ci.yml","experimental-runner-user":"true"}`, want: "must be a boolean"},
		{name: "numeric experimental runner user", source: `{"workflow":"ci.yml","experimental-runner-user":1}`, want: "must be a boolean"},
		{name: "null experimental runner user", source: `{"workflow":"ci.yml","experimental-runner-user":null}`, want: "must be a boolean"},
		{name: "null oidc", source: `{"workflow":"ci.yml","oidc":null}`, want: "oidc must be a JSON object"},
		{name: "array oidc", source: `{"workflow":"ci.yml","oidc":[]}`, want: "oidc must be a JSON object"},
		{name: "unknown oidc field", source: `{"workflow":"ci.yml","oidc":{"subject_claim":"pipeline_id"}}`, want: "oidc contains unknown field"},
		{name: "empty claims", source: `{"workflow":"ci.yml","oidc":{"claims":[]}}`, want: "oidc claims must be a non-empty array"},
		{name: "claims string", source: `{"workflow":"ci.yml","oidc":{"claims":"organization_id"}}`, want: "oidc claims must be a non-empty array"},
		{name: "empty claim entry", source: `{"workflow":"ci.yml","oidc":{"claims":["organization_id",""]}}`, want: "oidc claims entry 1 must be a non-empty string"},
		{name: "non-string AWS tag entry", source: `{"workflow":"ci.yml","oidc":{"aws-session-tags":["pipeline_id",null]}}`, want: "oidc aws-session-tags entry 1 must be a non-empty string"},
		{name: "empty subject claim", source: `{"workflow":"ci.yml","oidc":{"subject-claim":" "}}`, want: "oidc subject-claim must be a non-empty string"},
		{name: "non-string subject claim", source: `{"workflow":"ci.yml","oidc":{"subject-claim":1}}`, want: "oidc subject-claim must be a non-empty string"},
		{name: "null runners", source: `{"workflow":"ci.yml","runners":null}`, want: "non-empty array"},
		{name: "empty runners", source: `{"workflow":"ci.yml","runners":[]}`, want: "non-empty array"},
		{name: "unknown runner field", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","extra":true}]}`, want: "unknown field"},
		{name: "case alias runner field", source: `{"workflow":"ci.yml","runners":[{"Runs-On":"ubuntu-latest","queue":"hosted"}]}`, want: "unknown field"},
		{name: "duplicate runner field", source: `{"workflow":"ci.yml","runners":[{"runs-on":"windows-latest","runs-on":"ubuntu-latest","queue":"hosted"}]}`, want: "duplicate object key"},
		{name: "empty runner label", source: `{"workflow":"ci.yml","runners":[{"runs-on":"","queue":"hosted"}]}`, want: "unsupported runner label"},
		{name: "duplicate runner", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"one"},{"runs-on":"UBUNTU-LATEST","queue":"two"}]}`, want: "only be configured once"},
		{name: "missing queue", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest"}]}`, want: "queue must be a string"},
		{name: "empty image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","image":""}]}`, want: "immutable registry"},
		{name: "null image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","image":null}]}`, want: "immutable registry"},
		{name: "mutable image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"ubuntu-latest","queue":"hosted","image":"ubuntu:latest"}]}`, want: "immutable registry"},
		{name: "Darwin image", source: `{"workflow":"ci.yml","runners":[{"runs-on":"macos-14","queue":"macos","image":"` + image + `"}]}`, want: "unsupported on darwin/arm64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parsePluginConfiguration(test.source); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parsePluginConfiguration() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPluginUsesJSONConfigurationAndOnlyRequiredRuntime(t *testing.T) {
	requireImporterHost(t)
	workflowPath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(map[string]any{
		"workflow":                 workflowPath,
		"version":                  "0.8.0",
		"experimental-runner-user": true,
		"oidc": map[string]any{
			"claims":           []string{"organization_id"},
			"aws-session-tags": []string{"pipeline_id"},
			"subject-claim":    "pipeline_id",
		},
		"runners": []map[string]string{
			{"runs-on": "ubuntu-latest", "queue": "hosted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	t.Setenv("BUILDKITE_PLUGIN_GITHUB_ACTIONS_WORKFLOW", "ignored.yml")
	_ = executable // The plugin uses the already-opened running executable, not an environment path.
	setCLIPluginBuildkiteEnvironment(t, "plugin-linux")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 3 jobs") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Count(strings.TrimSpace(stdout.String()), "\n") != 0 {
		t.Fatalf("plugin success output is not a one-line summary: %q", stdout.String())
	}
	for _, verbose := range []string{"Schema:", "- Workflow parsing:", "  job ", "  action "} {
		if strings.Contains(stdout.String(), verbose) {
			t.Fatalf("plugin success output contains verbose inventory %q: %q", verbose, stdout.String())
		}
	}
	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Image   string            `yaml:"image"`
				Agents  map[string]string `yaml:"agents"`
				Command string            `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("workflow groups = %#v", pipeline.Steps)
	}
	for _, step := range pipeline.Steps[0].Steps {
		if step.Agents["queue"] != "hosted" || step.Image != defaultNobleRunnerImage || !strings.Contains(step.Command, "--hosted-tool-cache") || !strings.Contains(step.Command, "useradd --create-home") || !strings.Contains(step.Command, "sudo -n --preserve-env --user runner") {
			t.Fatalf("plugin profile was not applied: %#v", step)
		}
	}
	planCount := 0
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		planCount++
		if job.OIDC == nil || !slices.Equal(job.OIDC.Claims, []string{"organization_id"}) || !slices.Equal(job.OIDC.AWSSessionTags, []string{"pipeline_id"}) || job.OIDC.SubjectClaim != "pipeline_id" {
			t.Errorf("plan %q OIDC configuration = %#v", job.Workflow.LogicalJobID, job.OIDC)
		}
		if job.IDTokenPermission != "" {
			t.Errorf("plugin OIDC configuration implied plan permission %q", job.IDTokenPermission)
		}
	}
	if planCount != 3 {
		t.Fatalf("plan count = %d, want 3", planCount)
	}
}

func TestPluginIgnoresJobPermissionsForHostedGitHubToken(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"secret.yml": "name: Secret\non: push\npermissions:\n  contents: write\njobs:\n  secret:\n    permissions:\n      contents: read\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets.GITHUB_TOKEN }}\n    steps: [{run: true}]\n",
	})
	t.Chdir(repository)
	configuration, err := json.Marshal(map[string]any{"workflow": ".github/workflows/secret.yml"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-job-permissions")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, want 0; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 1 job") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		if job.GitHubToken == nil || !reflect.DeepEqual(job.GitHubToken.Permissions, map[string]string{"contents": "write"}) {
			t.Fatalf("GITHUB_TOKEN = %#v, want workflow permissions", job.GitHubToken)
		}
		return
	}
	t.Fatalf("uploaded artifacts = %#v, want job plan", runner.uploaded)
}

func TestPluginResolvesWorkflowFromBuildCheckoutOutsideWorkingDirectory(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"deploy.yml": "name: Deploy\non: push\njobs:\n  deploy:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	t.Chdir(t.TempDir())
	configuration, err := json.Marshal(map[string]any{"workflow": ".github/workflows/deploy.yml"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-checkout-path")
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, want 0; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 1 job") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestPluginUploadsPluralWorkflowList(t *testing.T) {
	requireImporterHost(t)
	configuration, err := json.Marshal(map[string]any{
		"workflows": []string{
			filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml"),
			filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "concurrent.yml"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-workflows")
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 5 jobs from 2 workflows") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	var pipeline struct {
		Steps []struct {
			Group string `yaml:"group"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 2 {
		t.Fatalf("workflow groups = %#v", pipeline.Steps)
	}
}

func TestPluginSkipsMissingWorkflowFromPluralList(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"ci.yml": "name: CI\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	configuration, err := json.Marshal(map[string]any{
		"workflows": []string{".github/workflows/ci.yml", ".github/workflows/deploy.yml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-missing-workflow")
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, want 0; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 1 job") || !strings.Contains(stderr.String(), `warning: workflow path ".github/workflows/deploy.yml" is missing or untracked; skipping`) {
		t.Fatalf("stdout/stderr = %q / %q", stdout.String(), stderr.String())
	}
	if len(runner.commands) == 0 {
		t.Fatal("present workflow was not uploaded")
	}
}

func TestPluginSucceedsWhenAllConfiguredWorkflowsWereRemoved(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"deploy.yml": "name: Deploy\non: push\njobs:\n  deploy:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	if output, err := exec.Command("git", "-C", repository, "rm", "-f", ".github/workflows/deploy.yml").CombinedOutput(); err != nil {
		t.Fatalf("remove tracked workflow: %v: %s", err, output)
	}
	configuration, err := json.Marshal(map[string]any{"workflow": ".github/workflows/deploy.yml"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-removed-workflow")
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, want 0; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), `warning: workflow path ".github/workflows/deploy.yml" is missing or untracked; skipping`) || !strings.Contains(stderr.String(), "all configured workflow paths are missing or untracked; there is nothing to upload") {
		t.Fatalf("stdout/stderr = %q / %q", stdout.String(), stderr.String())
	}
	if len(runner.commands) != 0 || len(runner.uploaded) != 0 {
		t.Fatalf("all-missing selection reached Buildkite: commands %#v, uploads %#v", runner.commands, runner.uploaded)
	}
}

func TestPluginRejectsNonExplicitWorkflowSelectors(t *testing.T) {
	requireImporterHost(t)
	workflowDirectory := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows")
	for _, test := range []struct {
		name      string
		field     string
		workflows any
		want      string
	}{
		{name: "all shorthand", field: "workflow", workflows: "*", want: "explicit paths"},
		{name: "string glob", field: "workflow", workflows: filepath.Join(workflowDirectory, "*.yml"), want: "explicit paths"},
		{name: "array glob", field: "workflows", workflows: []string{filepath.Join(workflowDirectory, "*.yml")}, want: "explicit paths"},
		{name: "directory", field: "workflow", workflows: workflowDirectory, want: "directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration, err := json.Marshal(map[string]any{test.field: test.workflows})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(pluginConfigurationEnvironment, string(configuration))
			setCLIPluginBuildkiteEnvironment(t, "plugin-workflows-explicit")
			runner := &cliCaptureRunner{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("run() code/stderr = %d / %q", code, stderr.String())
			}
			if stdout.Len() != 0 || len(runner.commands) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("invalid workflow selection reached Buildkite: stdout %q, commands %#v, uploads %#v", stdout.String(), runner.commands, runner.uploaded)
			}
		})
	}
}

func TestPluginPublishesMixedRuntimeDistributions(t *testing.T) {
	requireImporterHost(t)
	const fullCommit = "0123456789abcdef0123456789abcdef01234567"
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"mixed.yml": "on: push\njobs:\n  linux:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo linux\n  configured:\n    needs: linux\n    runs-on: ubuntu-18.04\n    steps:\n      - run: echo configured\n  fallback:\n    needs: linux\n    runs-on: [self-hosted, ubuntu-20.04]\n    steps:\n      - run: echo fallback\n  macos:\n    needs: linux\n    runs-on: macos-26\n    steps:\n      - run: echo macos\n",
	})
	t.Chdir(repository)
	workflowPath := filepath.Join(".github", "workflows", "mixed.yml")
	linuxPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	darwinContents := []byte{
		0xcf, 0xfa, 0xed, 0xfe,
		0x0c, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	darwinPath := filepath.Join(t.TempDir(), "buildkite-gha-darwin")
	if err := os.WriteFile(darwinPath, darwinContents, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(map[string]any{
		"workflow": workflowPath,
		"runners": []map[string]any{
			{"runs-on": "ubuntu-18.04", "queue": "legacy-linux"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolutionRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolutionRequests++
		if r.URL.Path != "/v3/jobs/"+cliTestJobID+"/github-actions/runners" || r.Header.Get("Authorization") != "Token job-token" {
			t.Errorf("runner resolution request = %s, headers %#v", r.URL.Path, r.Header)
		}
		var body struct {
			Requirements []struct {
				ID       string `json:"id"`
				Selector struct {
					Labels []string `json:"labels"`
				} `json:"selector"`
			} `json:"requirements"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		resolutions := make([]map[string]any, len(body.Requirements))
		for i, requirement := range body.Requirements {
			switch {
			case slices.Equal(requirement.Selector.Labels, []string{"ubuntu-latest"}):
				resolutions[i] = map[string]any{
					"id":     requirement.ID,
					"target": map[string]string{"queue": "linux-medium", "platform": "linux/amd64", "image": defaultNobleRunnerImage},
				}
			case slices.Equal(requirement.Selector.Labels, []string{"self-hosted", "ubuntu-20.04"}):
				resolutions[i] = map[string]any{
					"id":     requirement.ID,
					"target": map[string]string{"queue": "linux-medium", "platform": "linux/amd64", "image": defaultNobleRunnerImage},
					"warnings": []map[string]string{{
						"code":    "runner_label_fallback",
						"message": "This runner selector is not supported directly; using the linux-medium queue via a heuristic fallback. Configure an explicit runner mapping to use an appropriate Buildkite queue and avoid this fallback: https://github.com/buildkite/buildkite-gha/blob/main/docs/compatibility.md",
					}},
				}
			case slices.Equal(requirement.Selector.Labels, []string{"macos-26"}):
				resolutions[i] = map[string]any{"id": requirement.ID, "target": map[string]string{"queue": "macos-26-medium", "platform": "darwin/arm64"}}
			default:
				resolutions[i] = map[string]any{"id": requirement.ID, "error": map[string]string{"code": "unmapped_labels", "message": "No compatible runner is configured."}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resolutions": resolutions})
	}))
	defer server.Close()
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	_ = linuxPath // The plugin's Linux runtime is always its running executable.
	t.Setenv(pluginDevDarwinRuntimeEnvironment, darwinPath)
	setCLIPluginBuildkiteEnvironment(t, "plugin-mixed")
	t.Setenv("BUILDKITE_COMMIT", "HEAD")
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "true")
	runner := &cliCaptureRunner{gitOutput: []byte(fullCommit + "\n")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if resolutionRequests != 1 {
		t.Fatalf("runner resolution requests = %d, want 1", resolutionRequests)
	}
	var runnerAnnotation *cliCommand
	for i := range runner.commands {
		command := &runner.commands[i]
		if len(command.args) >= 7 && command.args[0] == "annotate" && command.args[6] == runnerResolutionContext {
			runnerAnnotation = command
			break
		}
	}
	if runnerAnnotation == nil || runnerAnnotation.args[8] != "warning" || !strings.Contains(string(runnerAnnotation.stdin), "heuristic fallback") || !strings.Contains(string(runnerAnnotation.stdin), "linux-medium") || !strings.Contains(string(runnerAnnotation.stdin), "docs/compatibility.md") {
		t.Fatalf("runner resolution annotation = %#v", runnerAnnotation)
	}
	darwinDigest := transport.Digest(darwinContents)
	planRuntimes := map[string]string{}
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		if job.Event.SHA != fullCommit {
			t.Fatalf("plan event SHA = %q, want normalized commit %q", job.Event.SHA, fullCommit)
		}
		planRuntimes[job.Workflow.LogicalJobID] = job.RuntimeDistributionDigest()
	}
	if planRuntimes["linux"] != cliTestRuntimeDigest() || planRuntimes["macos"] != darwinDigest {
		t.Fatalf("plan runtime digests = %#v", planRuntimes)
	}
	distributionPath, err := buildkitepipeline.DistributionPath(darwinDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runner.uploaded[distributionPath], darwinContents) {
		t.Fatal("Darwin runtime distribution was not published")
	}
	var pipeline struct {
		Steps []struct {
			Steps []struct {
				Key     string            `yaml:"key"`
				Image   string            `yaml:"image"`
				Agents  map[string]string `yaml:"agents"`
				Command string            `yaml:"command"`
			} `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("workflow groups = %#v", pipeline.Steps)
	}
	steps := make(map[string]struct {
		Image   string
		Queue   string
		Command string
	}, len(pipeline.Steps[0].Steps))
	for _, step := range pipeline.Steps[0].Steps {
		steps[step.Key] = struct {
			Image   string
			Queue   string
			Command string
		}{Image: step.Image, Queue: step.Agents["queue"], Command: step.Command}
	}
	var linux, configured, fallback, macos struct {
		Image   string
		Queue   string
		Command string
	}
	for key, step := range steps {
		switch {
		case strings.HasSuffix(key, "-linux"):
			linux = step
		case strings.HasSuffix(key, "-configured"):
			configured = step
		case strings.HasSuffix(key, "-fallback"):
			fallback = step
		case strings.HasSuffix(key, "-macos"):
			macos = step
		}
	}
	if linux.Queue != "linux-medium" || linux.Image != defaultNobleRunnerImage || !strings.Contains(linux.Command, "--hosted-tool-cache") || !strings.Contains(linux.Command, strings.TrimPrefix(cliTestRuntimeDigest(), "sha256:")) {
		t.Fatalf("Linux pipeline step = %#v", linux)
	}
	if configured.Queue != "legacy-linux" || configured.Image != "" || strings.Contains(configured.Command, "--hosted-tool-cache") || !strings.Contains(configured.Command, strings.TrimPrefix(cliTestRuntimeDigest(), "sha256:")) {
		t.Fatalf("configured Linux pipeline step = %#v", configured)
	}
	if fallback.Queue != "linux-medium" || fallback.Image != defaultNobleRunnerImage || !strings.Contains(fallback.Command, "--hosted-tool-cache") || !strings.Contains(fallback.Command, strings.TrimPrefix(cliTestRuntimeDigest(), "sha256:")) {
		t.Fatalf("fallback Linux pipeline step = %#v", fallback)
	}
	if macos.Queue != "macos-26-medium" || macos.Image != "" || strings.Contains(macos.Command, "--hosted-tool-cache") || !strings.Contains(macos.Command, strings.TrimPrefix(darwinDigest, "sha256:")) {
		t.Fatalf("Darwin pipeline step = %#v", macos)
	}
}

func TestPluginRunsNativelyOnDarwinARM64(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native Darwin/arm64 importer test")
	}
	const fullCommit = "0123456789abcdef0123456789abcdef01234567"
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"macos.yml": "on: push\njobs:\n  macos:\n    runs-on: macos-15\n    steps:\n      - run: echo macos\n",
	})
	t.Chdir(repository)
	configuration, err := json.Marshal(map[string]any{
		"workflow": filepath.Join(".github", "workflows", "macos.yml"),
		"runners":  []map[string]string{{"runs-on": "macos-15", "queue": "macos"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginConfigurationEnvironment, string(configuration))
	setCLIPluginBuildkiteEnvironment(t, "plugin-darwin-importer")
	t.Setenv("BUILDKITE_COMMIT", "HEAD")
	runner := &cliCaptureRunner{gitOutput: []byte(fullCommit + "\n")}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Uploaded 1 jobs") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for path, contents := range runner.uploaded {
		if !strings.HasSuffix(path, ".json") {
			continue
		}
		job, err := plan.Decode(contents)
		if err != nil {
			t.Fatal(err)
		}
		if job.RuntimeDistributionDigest() != cliTestRuntimeDigest() {
			t.Fatalf("Darwin job runtime = %q, want importer %q", job.RuntimeDistributionDigest(), cliTestRuntimeDigest())
		}
		return
	}
	t.Fatal("Darwin importer did not upload a job plan")
}

func TestNormalizePluginCommit(t *testing.T) {
	const fullCommit = "0123456789abcdef0123456789abcdef01234567"
	t.Run("preserves valid full commit", func(t *testing.T) {
		runner := &cliCaptureRunner{}
		setCalls := 0
		err := normalizePluginCommit(t.Context(), "", func(string) string { return fullCommit }, func(string, string) error {
			setCalls++
			return nil
		}, runner)
		if err != nil || setCalls != 0 || len(runner.commands) != 0 {
			t.Fatalf("normalizePluginCommit() error = %v, set calls = %d, commands = %#v", err, setCalls, runner.commands)
		}
	})
	t.Run("resolves symbolic commit from HEAD", func(t *testing.T) {
		runner := &cliCaptureRunner{gitOutput: []byte(fullCommit + "\n")}
		name, value := "", ""
		err := normalizePluginCommit(t.Context(), "/checkout", func(string) string { return "HEAD" }, func(gotName, gotValue string) error {
			name, value = gotName, gotValue
			return nil
		}, runner)
		if err != nil || name != "BUILDKITE_COMMIT" || value != fullCommit {
			t.Fatalf("normalizePluginCommit() = %q, %q, %v", name, value, err)
		}
		if len(runner.commands) != 1 || runner.commands[0].dir != "/checkout" || runner.commands[0].name != "git" || !slices.Equal(runner.commands[0].args, []string{"rev-parse", "HEAD"}) {
			t.Fatalf("commands = %#v, want exact git rev-parse HEAD invocation", runner.commands)
		}
	})
	t.Run("propagates resolution failure", func(t *testing.T) {
		runner := &cliCaptureRunner{gitErr: errors.New("no checkout")}
		err := normalizePluginCommit(t.Context(), "", func(string) string { return "HEAD" }, func(string, string) error { return nil }, runner)
		if err == nil || !strings.Contains(err.Error(), "resolve BUILDKITE_COMMIT from checked-out HEAD: no checkout") {
			t.Fatalf("normalizePluginCommit() error = %v", err)
		}
	})
}

func TestPluginCarriesReleaseCommitIntoPlans(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"release.yml": "on:\n  release:\n    types: [published]\npermissions:\n  contents: write\njobs:\n  publish:\n    runs-on: ubuntu-latest\n    env:\n      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n    steps: [{run: true}]\n",
	})
	t.Chdir(repository)
	configuration, err := json.Marshal(map[string]any{"workflow": ".github/workflows/release.yml"})
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	for _, test := range []struct {
		name            string
		buildkiteCommit string
		gitOutput       []byte
		wantGitCalls    int
	}{
		{name: "server-resolved commit", buildkiteCommit: commit},
		{name: "checked-out HEAD fallback", buildkiteCommit: "HEAD", gitOutput: []byte(commit + "\n"), wantGitCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(pluginConfigurationEnvironment, string(configuration))
			setCLIPluginBuildkiteEnvironment(t, "release-importer")
			t.Setenv("BUILDKITE_COMMIT", test.buildkiteCommit)
			t.Setenv("BUILDKITE_BRANCH", "v1.2.3")
			t.Setenv("BUILDKITE_TAG", "v1.2.3")
			t.Setenv("BUILDKITE_GITHUB_EVENT", "release")
			t.Setenv("BUILDKITE_GITHUB_ACTION", "published")
			runner := &cliCaptureRunner{
				gitOutput: test.gitOutput,
				webhook:   []byte(`{"action":"published","release":{"tag_name":"v1.2.3","draft":false,"prerelease":false}}`),
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
			}
			gitCalls := 0
			for _, command := range runner.commands {
				if command.name == "git" && slices.Equal(command.args, []string{"rev-parse", "HEAD"}) {
					gitCalls++
				}
			}
			if gitCalls != test.wantGitCalls {
				t.Fatalf("git rev-parse HEAD calls = %d, want %d", gitCalls, test.wantGitCalls)
			}
			found := false
			for path, source := range runner.uploaded {
				if !strings.HasSuffix(path, ".json") {
					continue
				}
				job, err := plan.Decode(source)
				if err != nil {
					t.Fatal(err)
				}
				found = true
				if job.Event.Name != "release" || job.Event.Ref != "refs/tags/v1.2.3" || job.Event.SHA != commit {
					t.Fatalf("release plan event = %#v", job.Event)
				}
				if job.GitHubToken == nil || job.GitHubToken.Workflow != "release.yml" || !reflect.DeepEqual(job.GitHubToken.Permissions, map[string]string{"contents": "write"}) {
					t.Fatalf("release GITHUB_TOKEN policy = %#v", job.GitHubToken)
				}
			}
			if !found {
				t.Fatal("plugin uploaded no release job plan")
			}
		})
	}
}

func setCLIPluginBuildkiteEnvironment(t *testing.T, stepKey string) {
	t.Helper()
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", "")
	t.Setenv("BUILDKITE_STEP_KEY", stepKey)
	t.Setenv("BUILDKITE_REPO", "https://github.com/buildkite/buildkite-gha")
	t.Setenv("BUILDKITE_COMMIT", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("BUILDKITE_BRANCH", "main")
	t.Setenv("BUILDKITE_TAG", "")
	t.Setenv("BUILDKITE_PULL_REQUEST", "false")
	t.Setenv("BUILDKITE_SOURCE", "webhook")
}
