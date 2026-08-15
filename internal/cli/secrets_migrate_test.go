package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

const (
	testMigrationCluster  = "11111111-2222-4333-8444-555555555555"
	testMigrationCommit   = "0123456789abcdef0123456789abcdef01234567"
	testMigrationPipeline = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

type migrationCommandResult struct {
	output []byte
	err    error
}

type migrationTestRunner struct {
	results  []migrationCommandResult
	commands []cliCommand
}

func (r *migrationTestRunner) Run(_ context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, cliCommand{dir: dir, name: name, args: slices.Clone(args), stdin: bytes.Clone(stdin)})
	if len(r.results) == 0 {
		return nil, errors.New("unexpected command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return bytes.Clone(result.output), result.err
}

func TestParseMigrateSecretsPrepareArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown", args: []string{"--all"}, want: "unknown option"},
		{name: "missing value", args: []string{"--secret"}, want: "requires a non-empty value"},
		{name: "invalid organization", args: []string{"--organization", "ACME"}, want: "lowercase"},
		{name: "invalid cluster", args: []string{"--cluster", "nope"}, want: "cluster UUID"},
		{name: "invalid pipeline", args: []string{"--pipeline", "Bad Pipeline"}, want: "pipeline slug"},
		{name: "invalid glob", args: []string{"--match", "["}, want: "invalid --match glob"},
		{name: "stdin policy", args: []string{"--policy-file", "-"}, want: "file path, not stdin"},
		{name: "unsafe output", args: []string{"--output", "workflow.yml"}, want: "directly under .github/workflows"},
		{name: "uppercase extension", args: []string{"--output", ".github/workflows/migrate.YML"}, want: ".yml or .yaml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseMigrateSecretsPrepareArgs(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseMigrateSecretsPrepareArgs() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateSecretsPolicyRequiresRestrictedRules(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   string
	}{
		{name: "empty", want: "must not be empty"},
		{name: "empty list", policy: "[]", want: "at least one rule"},
		{name: "empty rule", policy: "- {}", want: "at least one claim"},
		{name: "unknown claim", policy: "- imaginary: value", want: "unknown claim"},
		{name: "empty condition", policy: "- pipeline_slug: ''", want: "non-empty string"},
		{name: "non-string condition", policy: "- pipeline_slug: 7", want: "non-empty string"},
		{name: "invalid UUID", policy: "- pipeline_id: nope", want: "must be a UUID"},
		{name: "invalid UUID list", policy: "- cluster_queue_id: [11111111-2222-4333-8444-555555555555, nope]", want: "must be a UUID"},
		{name: "YAML alias", policy: "- pipeline_id: &id 11111111-2222-4333-8444-555555555555\n- pipeline_id: *id", want: "must not contain YAML aliases"},
		{name: "custom YAML tag", policy: "- pipeline_slug: !foo widgets", want: "must not contain custom YAML tags"},
		{name: "YAML 1.1 boolean", policy: "- build_branch: on", want: `value "on" must be quoted`},
		{name: "GitHub expression", policy: "- pipeline_slug: ${{ secrets.OTHER }}", want: "must not contain GitHub expression syntax"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateSecretsPolicy(test.policy)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateSecretsPolicy() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateSecretsPolicyAcceptsBuildkiteClaimTypes(t *testing.T) {
	policy := "- cluster_id: 11111111-2222-4333-8444-555555555555\n  pipeline_slug: widgets\n  build_branch: main\n"
	got, err := validateSecretsPolicy(policy)
	if err != nil || got != policy {
		t.Fatalf("validateSecretsPolicy() = %q, %v", got, err)
	}
}

func TestSelectMigrationSecretNames(t *testing.T) {
	t.Run("interactive explicit selection", func(t *testing.T) {
		var prompt bytes.Buffer
		names, err := selectMigrationSecretNames(
			[]string{"API_KEY", "DEPLOY_TOKEN", "OTHER"}, nil, nil,
			bufio.NewReader(strings.NewReader("2,1\n")), &prompt,
		)
		if err != nil || !slices.Equal(names, []string{"API_KEY", "DEPLOY_TOKEN"}) {
			t.Fatalf("selectMigrationSecretNames() = %v, %v", names, err)
		}
		if !strings.Contains(prompt.String(), "1) API_KEY") || !strings.Contains(prompt.String(), "type all") {
			t.Fatalf("prompt = %q", prompt.String())
		}
	})

	t.Run("filter union", func(t *testing.T) {
		names, err := selectMigrationSecretNames(
			[]string{"API_KEY", "DEPLOY_TOKEN", "OTHER"}, []string{"OTHER"}, []string{"*_TOKEN"},
			bufio.NewReader(strings.NewReader("")), io.Discard,
		)
		if err != nil || !slices.Equal(names, []string{"DEPLOY_TOKEN", "OTHER"}) {
			t.Fatalf("selectMigrationSecretNames() = %v, %v", names, err)
		}
	})

	t.Run("GitHub token", func(t *testing.T) {
		_, err := selectMigrationSecretNames(
			[]string{"GITHUB_TOKEN"}, []string{"GITHUB_TOKEN"}, nil,
			bufio.NewReader(strings.NewReader("")), io.Discard,
		)
		if err == nil || !strings.Contains(err.Error(), "separate permission-scoped workflow-token path") {
			t.Fatalf("selectMigrationSecretNames() error = %v", err)
		}
	})

	t.Run("too many", func(t *testing.T) {
		available := make([]string, maxMigrationSecrets+1)
		for index := range available {
			available[index] = fmt.Sprintf("SECRET_%d", index)
		}
		_, err := selectMigrationSecretNames(
			available, nil, []string{"SECRET_*"},
			bufio.NewReader(strings.NewReader("")), io.Discard,
		)
		if err == nil || !strings.Contains(err.Error(), "use multiple workflows") {
			t.Fatalf("selectMigrationSecretNames() error = %v", err)
		}
	})
}

func TestPrepareSecretsMigrationDiscoversAndRenders(t *testing.T) {
	runner := &migrationTestRunner{results: []migrationCommandResult{
		{output: []byte(`{"id":42,"full_name":"acme/widgets","html_url":"https://github.com/acme/widgets","default_branch":"main","owner":{"id":7}}`)},
		{output: []byte("OTHER\nDEPLOY_TOKEN\nAPI_KEY\n")},
		{output: []byte(`{"organization_slug":"acme"}`)},
		{output: []byte(`[{"id":"` + testMigrationPipeline + `","slug":"widgets","name":"Widgets","cluster_id":"` + testMigrationCluster + `","repository":"git@github.com:acme/widgets.git"}]`)},
		{output: []byte(`[]`)},
	}}
	var stdout, stderr bytes.Buffer
	code := migrateSecrets(
		[]string{"prepare", "--match", "*_TOKEN", "--secret", "API_KEY"},
		strings.NewReader(""), &stdout, &stderr, runner,
	)
	if code != 0 {
		t.Fatalf("migrateSecrets() code = %d, stderr = %q", code, stderr.String())
	}
	workflow := stdout.String()
	for _, want := range []string{
		"workflow_dispatch:", "grant_id:", "id-token: write", "permissions:\n  id-token: write",
		"${{ secrets.API_KEY }}", "${{ secrets.DEPLOY_TOKEN }}", "#   - pipeline_id: " + testMigrationPipeline,
		"ACTIONS_ID_TOKEN_REQUEST_URL", "Remove this workflow",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("workflow missing %q", want)
		}
	}
	if strings.Contains(workflow, "${{ secrets.OTHER }}") || strings.Contains(workflow, "GITHUB_TOKEN") {
		t.Errorf("workflow contains an unselected or forbidden secret reference")
	}
	manifest, err := decodeMigrationManifest(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Organization != "acme" || manifest.Cluster != testMigrationCluster || !slices.Equal(manifest.SecretNames, []string{"API_KEY", "DEPLOY_TOKEN"}) {
		t.Fatalf("manifest = %#v", manifest)
	}
	if len(runner.results) != 0 || len(runner.commands) != 5 {
		t.Fatalf("commands/results = %d/%d", len(runner.commands), len(runner.results))
	}
}

func TestRejectExistingBuildkiteSecretsBeforeWorkflowGeneration(t *testing.T) {
	runner := &migrationTestRunner{results: []migrationCommandResult{{output: []byte(`[{"key":"API_KEY"},{"key":"OTHER"}]`)}}}
	err := rejectExistingBuildkiteSecrets(context.Background(), runner, "acme", testMigrationCluster, []string{"API_KEY", "DEPLOY_TOKEN"})
	if err == nil || !strings.Contains(err.Error(), "API_KEY") || !strings.Contains(err.Error(), "will not be overwritten") {
		t.Fatalf("rejectExistingBuildkiteSecrets() error = %v", err)
	}
	if command := runner.commands[0]; command.name != "env" || !slices.Equal(command.args, []string{
		"BUILDKITE_ORGANIZATION_SLUG=acme", "bk", "api", "/clusters/" + testMigrationCluster + "/secrets?per_page=100&page=1", "--no-input",
	}) {
		t.Fatalf("list command = %#v", command)
	}
}

func TestRejectExistingBuildkiteSecretsPaginates(t *testing.T) {
	firstPage := make([]map[string]string, 100)
	for index := range firstPage {
		firstPage[index] = map[string]string{"key": fmt.Sprintf("OTHER_%d", index)}
	}
	encodedFirstPage, err := json.Marshal(firstPage)
	if err != nil {
		t.Fatal(err)
	}
	runner := &migrationTestRunner{results: []migrationCommandResult{
		{output: encodedFirstPage},
		{output: []byte(`[{"key":"API_KEY"}]`)},
	}}
	err = rejectExistingBuildkiteSecrets(context.Background(), runner, "acme", testMigrationCluster, []string{"API_KEY"})
	if err == nil || !strings.Contains(err.Error(), "API_KEY") || len(runner.commands) != 2 || !strings.Contains(strings.Join(runner.commands[1].args, " "), "page=2") {
		t.Fatalf("rejectExistingBuildkiteSecrets() error/commands = %v/%#v", err, runner.commands)
	}
}

func TestRenderOIDCSecretsMigrationWorkflowIsDeterministicAndReviewable(t *testing.T) {
	manifest := testMigrationManifest()
	first, err := renderOIDCSecretsMigrationWorkflow(manifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderOIDCSecretsMigrationWorkflow(manifest)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("rendered workflow is not deterministic")
	}
	if strings.Count(string(first), "${{ secrets.") != 2 {
		t.Fatalf("secret references = %d", strings.Count(string(first), "${{ secrets."))
	}
	for _, forbidden := range []string{"secrets: inherit", "set -x", "curl ", "--data", "GITHUB_TOKEN", "first-secret-value"} {
		if strings.Contains(string(first), forbidden) {
			t.Errorf("workflow contains forbidden text %q", forbidden)
		}
	}
	var document yaml.Node
	if err := yaml.Unmarshal(first, &document); err != nil {
		t.Fatalf("generated workflow is invalid YAML: %v", err)
	}
}

func TestGeneratedOIDCMigrationSendsOneInMemoryBatchWithoutLeakingValues(t *testing.T) {
	manifest := testMigrationManifest()
	generated, err := renderOIDCSecretsMigrationWorkflow(manifest)
	if err != nil {
		t.Fatal(err)
	}
	script := migrationWorkflowScript(t, generated)

	requestBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oidc":
			expectedAudience := "http://" + request.Host + "/grant-identifier-123/secrets"
			if request.Header.Get("Authorization") != "Bearer github-request-token" || request.URL.Query().Get("audience") != expectedAudience {
				t.Errorf("OIDC request authorization/audience = %q/%q", request.Header.Get("Authorization"), request.URL.Query().Get("audience"))
			}
			_ = json.NewEncoder(response).Encode(map[string]string{"value": "github-oidc-token"})
		case "/grant-identifier-123/secrets":
			if request.Header.Get("Authorization") != "Bearer github-oidc-token" || request.Method != http.MethodPost {
				t.Errorf("migration request authorization/method = %q/%q", request.Header.Get("Authorization"), request.Method)
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			requestBody <- body
			response.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	script = strings.Replace(script,
		`migration_url_prefix = "https://api.buildkite.com/v2/organizations/acme/clusters/11111111-2222-4333-8444-555555555555/github-actions-secret-migrations/"`,
		`migration_url_prefix = "`+server.URL+`/"`, 1)
	firstValue := strings.Repeat("x", 8192)
	command := exec.Command("bash", "-c", script)
	command.Env = append(command.Environ(),
		"GITHUB_REF=refs/heads/main", "DEFAULT_BRANCH=main", "GRANT_ID=grant-identifier-123",
		"ACTIONS_ID_TOKEN_REQUEST_URL="+server.URL+"/oidc?request=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=github-request-token",
		"MIGRATION_SECRET_000="+firstValue, "MIGRATION_SECRET_001=second-secret-value",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("generated script failed: %v: %s", err, output)
	}
	for _, value := range []string{firstValue, "second-secret-value", "github-request-token", "github-oidc-token"} {
		if strings.Contains(string(output), value) {
			t.Fatalf("generated script leaked %q in %q", value, output)
		}
	}
	body := <-requestBody
	secrets, ok := body["secrets"].(map[string]any)
	if !ok || len(body) != 1 || len(secrets) != 2 || secrets["API_KEY"] != firstValue || secrets["DEPLOY_TOKEN"] != "second-secret-value" {
		t.Fatalf("request body = %#v", body)
	}
}

func TestGeneratedOIDCMigrationDoesNotPrintBackendResponse(t *testing.T) {
	generated, err := renderOIDCSecretsMigrationWorkflow(testMigrationManifest())
	if err != nil {
		t.Fatal(err)
	}
	script := migrationWorkflowScript(t, generated)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/oidc" {
			_ = json.NewEncoder(response).Encode(map[string]string{"value": "github-oidc-token"})
			return
		}
		response.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(response, `{"message":"sensitive backend detail"}`)
	}))
	defer server.Close()
	script = strings.Replace(script,
		`migration_url_prefix = "https://api.buildkite.com/v2/organizations/acme/clusters/11111111-2222-4333-8444-555555555555/github-actions-secret-migrations/"`,
		`migration_url_prefix = "`+server.URL+`/"`, 1)
	command := exec.Command("bash", "-c", script)
	command.Env = append(command.Environ(),
		"GITHUB_REF=refs/heads/main", "DEFAULT_BRANCH=main", "GRANT_ID=grant-identifier-123",
		"ACTIONS_ID_TOKEN_REQUEST_URL="+server.URL+"/oidc", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=github-request-token",
		"MIGRATION_SECRET_000=first-secret-value", "MIGRATION_SECRET_001=second-secret-value",
	)
	output, runErr := command.CombinedOutput()
	if runErr == nil || !strings.Contains(string(output), "HTTP 409") {
		t.Fatalf("generated script error/output = %v/%q", runErr, output)
	}
	for _, forbidden := range []string{"sensitive backend detail", "first-secret-value", "second-secret-value"} {
		if strings.Contains(string(output), forbidden) {
			t.Fatalf("generated script leaked %q in %q", forbidden, output)
		}
	}
}

func TestGeneratedOIDCMigrationValidatesAllValuesBeforeRequests(t *testing.T) {
	generated, err := renderOIDCSecretsMigrationWorkflow(testMigrationManifest())
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", migrationWorkflowScript(t, generated))
	command.Env = append(command.Environ(),
		"GITHUB_REF=refs/heads/main", "DEFAULT_BRANCH=main", "GRANT_ID=grant-identifier-123",
		"ACTIONS_ID_TOKEN_REQUEST_URL=http://127.0.0.1:1/should-not-run", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=github-request-token",
		"MIGRATION_SECRET_000=first-secret-value", "MIGRATION_SECRET_001=   ",
	)
	output, runErr := command.CombinedOutput()
	if runErr == nil || !strings.Contains(string(output), "DEPLOY_TOKEN is missing or empty; no Buildkite secrets were created") {
		t.Fatalf("generated script error/output = %v/%q", runErr, output)
	}
	if strings.Contains(string(output), "first-secret-value") {
		t.Fatalf("generated script leaked a validated value in %q", output)
	}
}

func TestRunSecretsMigrationPinsCommittedWorkflowCreatesGrantAndDispatches(t *testing.T) {
	t.Chdir(t.TempDir())
	workflowPath := ".github/workflows/migrate#secrets.yml"
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow, err := renderOIDCSecretsMigrationWorkflow(testMigrationManifest())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
		t.Fatal(err)
	}
	contentResponse, _ := json.Marshal(map[string]string{
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(workflow),
	})
	runner := &migrationTestRunner{results: []migrationCommandResult{
		{output: []byte(`{"id":42,"full_name":"acme/widgets","html_url":"https://github.com/acme/widgets","default_branch":"main","owner":{"id":7}}`)},
		{output: []byte(testMigrationCommit + "\n")},
		{output: contentResponse},
		{output: []byte(`{"id":"grant-identifier-123","migration_url":"https://api.buildkite.com/v2/organizations/acme/clusters/11111111-2222-4333-8444-555555555555/github-actions-secret-migrations/grant-identifier-123/secrets","audience":"https://api.buildkite.com/v2/organizations/acme/clusters/11111111-2222-4333-8444-555555555555/github-actions-secret-migrations/grant-identifier-123/secrets"}`)},
		{},
	}}
	var stdout bytes.Buffer
	if err := runSecretsMigration(context.Background(), workflowPath, &stdout, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 5 || runner.commands[1].name != "gh" || !strings.Contains(strings.Join(runner.commands[2].args, " "), "migrate%23secrets.yml?ref="+testMigrationCommit) {
		t.Fatalf("commands = %#v", runner.commands)
	}
	grantCommand := runner.commands[3]
	dataIndex := slices.Index(grantCommand.args, "--data")
	if grantCommand.name != "env" || !slices.Contains(grantCommand.args, "BUILDKITE_ORGANIZATION_SLUG=acme") || dataIndex < 0 || dataIndex+1 >= len(grantCommand.args) {
		t.Fatalf("grant command = %#v", grantCommand)
	}
	var grantRequest map[string]any
	if err := json.Unmarshal([]byte(grantCommand.args[dataIndex+1]), &grantRequest); err != nil {
		t.Fatal(err)
	}
	if grantRequest["workflow_sha"] != testMigrationCommit || grantRequest["workflow_path"] != workflowPath || grantRequest["default_branch_ref"] != "refs/heads/main" {
		t.Fatalf("grant request = %#v", grantRequest)
	}
	dispatch := runner.commands[4]
	if dispatch.name != "gh" || !slices.Contains(dispatch.args, "--json") || string(dispatch.stdin) != `{"grant_id":"grant-identifier-123"}` {
		t.Fatalf("dispatch command = %#v", dispatch)
	}
	if !strings.Contains(stdout.String(), "Remove the workflow") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func testMigrationManifest() migrationManifest {
	return migrationManifest{
		Version:           1,
		Organization:      "acme",
		Cluster:           testMigrationCluster,
		Policy:            "- pipeline_id: " + testMigrationPipeline + "\n",
		Repository:        "acme/widgets",
		RepositoryID:      42,
		RepositoryOwnerID: 7,
		DefaultBranch:     "main",
		SecretNames:       []string{"API_KEY", "DEPLOY_TOKEN"},
	}
}

func migrationWorkflowScript(t *testing.T, generated []byte) string {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(generated, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow.Jobs["migrate"].Steps[0].Run
}
