package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSmokeWorkflowsIntoOwnedModel(t *testing.T) {
	for _, name := range []string{"shell.yml", "ci.yml", "artifact.yml"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", name)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(path, source)
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Jobs) != 2 {
				t.Fatalf("Parse() jobs = %d, want 2", len(parsed.Jobs))
			}
			for _, job := range parsed.Jobs {
				if job.Span.Start.Line == 0 || job.Span.End.Line < job.Span.Start.Line {
					t.Errorf("job %q has invalid owned source span: %#v", job.ID, job.Span)
				}
				for _, step := range job.Steps {
					if step.Span.Start.Line == 0 || step.Span.End.Line < step.Span.Start.Line {
						t.Errorf("job %q step %q has invalid owned source span: %#v", job.ID, step.Name, step.Span)
					}
				}
			}
		})
	}
}

func TestParseRetainsRunNameAndSourceSpan(t *testing.T) {
	parsed, err := Parse("deploy.yml", []byte("name: Deploy\nrun-name: Deploy ${{ inputs.target }} by @${{ github.actor }}\non: workflow_dispatch\njobs:\n  deploy:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RunName != "Deploy ${{ inputs.target }} by @${{ github.actor }}" || parsed.RunNameSpan.Start.Line != 2 || parsed.RunNameSpan.Start.Column == 0 {
		t.Fatalf("run-name = %q at %#v", parsed.RunName, parsed.RunNameSpan)
	}
}

func TestParseOwnsReusableWorkflowSecretDeclarationsAndMappings(t *testing.T) {
	source := []byte(`on:
  workflow_call:
    secrets:
      Required_Token:
        required: true
      optional_token:
jobs:
  nested:
    uses: ./.github/workflows/nested.yml
    secrets:
      target_token: ${{ secrets['SOURCE_TOKEN'] }}
`)
	parsed, err := Parse("reusable.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.CallSecrets) != 2 || !parsed.CallSecrets["REQUIRED_TOKEN"].Required || parsed.CallSecrets["REQUIRED_TOKEN"].Span.Start.Line != 4 {
		t.Fatalf("call secret declarations = %#v", parsed.CallSecrets)
	}
	mapping := parsed.Jobs[0].Reusable.Secrets["TARGET_TOKEN"]
	if mapping.Source != "SOURCE_TOKEN" || mapping.Span.Start.Line != 11 {
		t.Fatalf("call secret mapping = %#v", mapping)
	}
}

func TestParseRejectsUnsupportedReusableWorkflowSecretMappings(t *testing.T) {
	for _, value := range []string{"literal", "${{ vars.SOURCE }}", "${{ secrets[inputs.name] }}", "${{ secrets.A }}-${{ secrets.B }}"} {
		source := "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    secrets:\n      target: " + value + "\n"
		if _, err := Parse("caller.yml", []byte(source)); err == nil || !strings.Contains(err.Error(), "secret mapping") {
			t.Fatalf("Parse(%q) error = %v", value, err)
		}
	}
}

func TestParseRejectsCaseCollidingReusableWorkflowSecretAliases(t *testing.T) {
	for _, source := range []string{
		"on:\n  workflow_call:\n    secrets:\n      TOKEN: {}\n      token: {}\njobs:\n  test: {runs-on: ubuntu-latest, steps: [{run: true}]}\n",
		"on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    secrets:\n      TOKEN: ${{ secrets.A }}\n      token: ${{ secrets.B }}\n",
	} {
		if _, err := Parse("collision.yml", []byte(source)); err == nil {
			t.Fatal("Parse() accepted case-colliding secret aliases")
		}
	}
}

func TestParsePreservesEnvironmentVariableCase(t *testing.T) {
	source := []byte("on: push\nenv:\n  WorkflowValue: workflow\njobs:\n  build:\n    runs-on: ubuntu-latest\n    env:\n      JobValue: job\n    steps:\n      - run: true\n        env:\n          STEP_VALUE: step\n")
	parsed, err := Parse("env.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		env        map[string]string
		key, value string
	}{
		{parsed.Env, "WorkflowValue", "workflow"},
		{parsed.Jobs[0].Env, "JobValue", "job"},
		{parsed.Jobs[0].Steps[0].Env, "STEP_VALUE", "step"},
	} {
		if check.env[check.key] != check.value {
			t.Fatalf("env = %#v, want %s=%s", check.env, check.key, check.value)
		}
	}
}

func TestParseKeepsWorkflowAndJobExpressionSurfacesSeparate(t *testing.T) {
	jobSource := []byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    env:\n      VALUE: ${{ format('{0}', vars.VALUE) }}\n    defaults:\n      run:\n        shell: ${{ format('{0}', 'sh') }}\n    steps: [{run: true}]\n")
	parsed, err := Parse("job.yml", jobSource)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Jobs[0].Env["VALUE"] == "" || parsed.Jobs[0].DefaultShell == "" {
		t.Fatalf("Parse() dropped job expressions: %#v", parsed.Jobs[0])
	}

	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "workflow env", source: "on: push\nenv:\n  VALUE: ${{ format('{0}', vars.VALUE) }}\njobs:\n  build: {runs-on: ubuntu-latest, steps: [{run: true}]}\n", want: `workflow env "VALUE"`},
		{name: "workflow default", source: "on: push\ndefaults:\n  run:\n    shell: ${{ format('{0}', 'sh') }}\njobs:\n  build: {runs-on: ubuntu-latest, steps: [{run: true}]}\n", want: "workflow default shell"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse("workflow.yml", []byte(test.source)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseGitHubEnvironment(t *testing.T) {
	parsed, err := Parse("environment.yml", []byte("on: push\njobs:\n  deploy:\n    environment: production\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Jobs[0].Environment != "production" {
		t.Fatalf("Parse() environment = %q, want %q", parsed.Jobs[0].Environment, "production")
	}

	// The url is used only for GitHub deployment records, which are never
	// created, so the mapping form is accepted and the url ignored.
	parsed, err = Parse("environment.yml", []byte("on: push\njobs:\n  deploy:\n    environment:\n      name: production\n      url: https://example.com\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Jobs[0].Environment != "production" {
		t.Fatalf("Parse() environment = %q, want %q", parsed.Jobs[0].Environment, "production")
	}

	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "expression name",
			source: "on: push\njobs:\n  deploy:\n    environment: ${{ github.ref_name }}\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			want:   "environment names that use expressions are unsupported; use a literal environment name",
		},
		{
			name:   "reusable workflow call",
			source: "on: push\njobs:\n  deploy:\n    environment: production\n    uses: ./.github/workflows/deploy.yml\n",
			want:   `"environment" is not available`,
		},
		{
			name:   "blank name",
			source: "on: push\njobs:\n  deploy:\n    environment: \" \"\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			want:   "environment requires a literal name",
		},
		{
			name:   "control characters",
			source: "on: push\njobs:\n  deploy:\n    environment: \"prod\\nuction\"\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			want:   "environment name must be at most 255 characters without control characters",
		},
		{
			name:   "name longer than 255 characters",
			source: "on: push\njobs:\n  deploy:\n    environment: " + strings.Repeat("e", 256) + "\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
			want:   "environment name must be at most 255 characters without control characters",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse("environment.yml", []byte(test.source)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}

	// The 255-character limit counts characters, not UTF-8 bytes, matching
	// GitHub: 255 two-byte characters are 510 bytes and remain valid.
	multibyte := "on: push\njobs:\n  deploy:\n    environment: " + strings.Repeat("é", 255) + "\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"
	if _, err := Parse("environment.yml", []byte(multibyte)); err != nil {
		t.Fatalf("Parse() with a 255-character multibyte environment name failed: %v", err)
	}
}

func TestParseOwnsExplicitWorkflowAndJobPermissions(t *testing.T) {
	source := []byte(`on: push
permissions:
  contents: read
  pull-requests: write
  issues: none
jobs:
  inherited:
    runs-on: ubuntu-latest
    steps: [{run: true}]
  overridden:
    runs-on: ubuntu-latest
    permissions:
      issues: write
    steps: [{run: true}]
`)
	parsed, err := Parse("permissions.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Permissions == nil || parsed.Permissions.Span.Start.Line != 2 || parsed.Permissions.Scopes["contents"] != "read" || parsed.Permissions.Scopes["pull-requests"] != "write" {
		t.Fatalf("workflow permissions = %#v", parsed.Permissions)
	}
	if _, exists := parsed.Permissions.Scopes["issues"]; exists {
		t.Fatalf("none permission was retained: %#v", parsed.Permissions.Scopes)
	}
	if parsed.Jobs[0].Permissions != nil || parsed.Jobs[1].Permissions == nil || parsed.Jobs[1].Permissions.Scopes["issues"] != "write" || parsed.Jobs[1].Permissions.Span.Start.Line != 12 {
		t.Fatalf("job permissions = %#v / %#v", parsed.Jobs[0].Permissions, parsed.Jobs[1].Permissions)
	}
}

func TestParseExpandsTopLevelAllPermissions(t *testing.T) {
	for _, access := range []string{"read", "write"} {
		t.Run(access, func(t *testing.T) {
			parsed, err := Parse("permissions.yml", []byte("on: push\npermissions: "+access+"-all\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"actions": access, "artifact-metadata": access, "attestations": access, "checks": access, "contents": access,
				"deployments": access, "discussions": access, "issues": access, "packages": access, "pages": access,
				"pull-requests": access, "security-events": access, "statuses": access,
			}
			if parsed.Permissions == nil || !reflect.DeepEqual(parsed.Permissions.Scopes, want) {
				t.Fatalf("workflow permissions = %#v, want %#v", parsed.Permissions, want)
			}
			for _, excluded := range []string{"id-token", "models", "repository-projects", "code-quality", "metadata", "vulnerability-alerts"} {
				if _, ok := parsed.Permissions.Scopes[excluded]; ok {
					t.Errorf("%s-all included excluded scope %q", access, excluded)
				}
			}
		})
	}
}

func TestParseAcceptsIDTokenPermissions(t *testing.T) {
	for _, access := range []string{"read", "write", "none"} {
		t.Run(access, func(t *testing.T) {
			source := []byte("on: push\npermissions:\n  id-token: " + access + "\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
			parsed, err := Parse("permissions.yml", source)
			if err != nil {
				t.Fatal(err)
			}
			value, present := parsed.Permissions.Scopes["id-token"]
			if access == "none" {
				if present {
					t.Fatalf("none permission was retained: %#v", parsed.Permissions.Scopes)
				}
			} else if !present || value != access {
				t.Fatalf("id-token permission = %q, %t, want %q", value, present, access)
			}
		})
	}
}

func TestParseRejectsUnsupportedPermissionFormsWithLocation(t *testing.T) {
	for _, test := range []struct {
		name, declaration, want string
	}{
		{name: "non-canonical name", declaration: "permissions:\n  pull_requests: write\n", want: "permissions.yml:3:3: workflow permissions: unsupported permission \"pull_requests\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := []byte("on: push\n" + test.declaration + "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
			_, err := Parse("permissions.yml", source)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsInvalidPermissionScalars(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
	}{
		{name: "workflow", source: "on: push\npermissions: write\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n", want: `permissions.yml:2:14: workflow permissions: invalid permissions scalar "write"; use read-all, write-all, or a permissions map`},
		{name: "job", source: "on: push\njobs:\n  test:\n    permissions: write\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n", want: `permissions.yml:4:18: job "test": invalid permissions scalar "write"; declare each needed permission in a map`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse("permissions.yml", []byte(test.source))
			if err == nil || err.Error() != test.want {
				t.Fatalf("Parse() error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsJobPermissionShorthand(t *testing.T) {
	for _, shorthand := range []string{"read-all", "write-all"} {
		t.Run(shorthand, func(t *testing.T) {
			source := []byte("on: push\njobs:\n  publish:\n    permissions: " + shorthand + "\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
			_, err := Parse("permissions.yml", source)
			access := strings.TrimSuffix(shorthand, "-all")
			want := `permissions.yml:4:18: permissions: ` + shorthand + ` is unsupported as job-level shorthand. In job "publish", you cannot set separate repository permissions. At the workflow top level, declare each needed repository permission, such as contents: ` + access + ` and pull-requests: ` + access + `. These permissions apply to every job that receives GITHUB_TOKEN. Use permissions: ` + shorthand + ` at the workflow top level only when every supported repository permission should have ` + access + ` access. If you need different repository permissions for individual jobs, open an issue in https://github.com/buildkite/buildkite-gha so we can prioritize support`
			if err == nil || err.Error() != want {
				t.Fatalf("Parse() error = %q, want %q", err, want)
			}
		})
	}
}

func TestParseOwnsLiteralContainersInDeclarationOrder(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container:\n      image: node:24\n      env: {NODE_ENV: test}\n      ports: [8080]\n    services:\n      zed: {image: redis:7}\n      alpha: {image: 'registry.example:5000/team/postgres:16', ports: ['5432:5432']}\n    steps:\n      - run: true\n")
	parsed, err := Parse("containers.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	job := parsed.Jobs[0]
	if job.Container == nil || job.Container.Image != "node:24" || job.Container.Env["NODE_ENV"] != "test" || len(job.Services) != 2 || job.Services[0].Name != "zed" || job.Services[1].Name != "alpha" || job.Services[1].Container.Image != "registry.example:5000/team/postgres:16" {
		t.Fatalf("owned containers = %#v / %#v", job.Container, job.Services)
	}
}

func TestParseRetainsJobContainerImageExpression(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container: ghcr.io/acme/tool:${{ matrix.version }}\n    steps: [{run: true}]\n")
	parsed, err := Parse("containers.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Jobs[0].Container.Image; got != "ghcr.io/acme/tool:${{ matrix.version }}" {
		t.Fatalf("container image = %q", got)
	}
}

func TestParseContainerImageRegistryPorts(t *testing.T) {
	for _, test := range []struct {
		image string
		valid bool
	}{
		{"localhost:5000/private/service:latest", true},
		{"127.0.0.1:65535/private/service", true},
		{"[::1]:5000/private/service", true},
		{"localhost:0/private/service", false},
		{"localhost:65536/private/service", false},
	} {
		t.Run(test.image, func(t *testing.T) {
			source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    services:\n      service:\n        image: '" + test.image + "'\n    steps: [{run: true}]\n")
			_, err := Parse("containers.yml", source)
			if (err == nil) != test.valid {
				t.Fatalf("Parse() error = %v, valid = %t", err, test.valid)
			}
		})
	}
}

func TestParseOwnsCompleteStaticServiceContainer(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    services:
      database:
        image: postgres:16
        credentials:
          username: ${{ secrets.REGISTRY_USER }}
          password: ${{ secrets.REGISTRY_PASSWORD }}
        env: {POSTGRES_PASSWORD: test}
        ports: ['127.0.0.1::5432/tcp']
        volumes: ['database:/var/lib/postgresql/data:ro']
        options: --health-cmd "pg_isready -U postgres" --health-retries 5
        command: postgres -c fsync=off
        entrypoint: docker-entrypoint.sh
    steps: [{run: true}]
`)
	parsed, err := Parse("containers.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	service := parsed.Jobs[0].Services[0].Container
	if service.Image != "postgres:16" || service.Credentials == nil || service.Credentials.Username != "${{ secrets.REGISTRY_USER }}" || service.Credentials.Password != "${{ secrets.REGISTRY_PASSWORD }}" || service.Env["POSTGRES_PASSWORD"] != "test" || len(service.Ports) != 1 || len(service.Volumes) != 1 || service.Options == "" || service.Command != "postgres -c fsync=off" || service.Entrypoint != "docker-entrypoint.sh" {
		t.Fatalf("service container = %#v", service)
	}
}

func TestParsePreservesPartialServiceContainerCredentials(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    services:\n      database:\n        image: postgres:16\n        credentials:\n          username: registry-user\n    steps: [{run: true}]\n")
	parsed, err := Parse("containers.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	credentials := parsed.Jobs[0].Services[0].Container.Credentials
	if credentials == nil || credentials.Username != "registry-user" || credentials.Password != "" {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func TestParseRejectsExpressionValuedServiceContainerEnvironment(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    services:\n      database:\n        image: postgres:16\n        env: ${{ fromJSON('{}') }}\n    steps: [{run: true}]\n")
	_, err := Parse("containers.yml", source)
	if err == nil || !strings.Contains(err.Error(), "containers.yml:8:14: job \"test\": expression-valued service container env is unsupported") {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseRejectsUnsupportedContainerControls(t *testing.T) {
	for name, body := range map[string]string{
		"credentials": "credentials: {username: me, password: secret}",
		"volumes":     "volumes: ['/tmp:/tmp']",
		"options":     "options: --privileged",
	} {
		t.Run(name, func(t *testing.T) {
			source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container:\n      image: node:24\n      " + body + "\n    steps:\n      - run: true\n")
			if _, err := Parse("containers.yml", source); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestContainerValidationIsScopedAndSourceLocated(t *testing.T) {
	unrelated := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/action@v1\n        with: {image: node:24, options: --privileged}\n")
	if _, err := Parse("scoped.yml", unrelated); err != nil {
		t.Fatalf("unrelated action inputs were treated as a container: %v", err)
	}
	for _, test := range []struct {
		name, field, want string
	}{
		{"image", "image: INVALID IMAGE", "bad.yml:6:14:"},
		{"env-key", "image: node:24\n      env: {'bad-key': ok}", "bad.yml:7:13:"},
		{"env-value", "image: node:24\n      env: {OK: '" + strings.Repeat("x", 65537) + "'}", "bad.yml:7:17:"},
		{"port", "image: node:24\n      ports: ['65536/tcp']", "bad.yml:7:15:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container:\n      " + test.field + "\n    steps: [{run: true}]\n")
			if _, err := Parse("bad.yml", source); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want location %s", err, test.want)
			}
		})
	}
}

func TestParseContainerShortForms(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container: node:24\n    services:\n      redis: redis:7\n      postgres: {image: postgres:16, ports: ['5432:5432/udp']}\n    steps: [{run: true}]\n")
	parsed, err := Parse("short.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Jobs[0].Container.Image != "node:24" || len(parsed.Jobs[0].Services) != 2 {
		t.Fatalf("containers = %#v, %#v", parsed.Jobs[0].Container, parsed.Jobs[0].Services)
	}
}

func TestParseRetainsSequentialRuntimeControls(t *testing.T) {
	source := []byte("name: runtime\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    if: always()\n    continue-on-error: true\n    timeout-minutes: 5\n    steps:\n      - run: echo ok\n        if: success()\n        timeout-minutes: 2\n        continue-on-error: true\n")
	parsed, err := Parse("workflow.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	job := parsed.Jobs[0]
	if job.If != "always()" || !job.ContinueOnError || job.TimeoutMinutes != 5 {
		t.Fatalf("job controls = if %q timeout %v", job.If, job.TimeoutMinutes)
	}
	step := job.Steps[0]
	if step.If != "success()" || step.TimeoutMinutes != 2 || !step.ContinueOnError {
		t.Fatalf("step controls = %#v", step)
	}
}

func TestParseRetainsAllYAMLBooleanSpellingsForJobContinueOnError(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "true", want: true}, {value: "True", want: true}, {value: "TRUE", want: true},
		{value: "false"}, {value: "False"}, {value: "FALSE"},
	} {
		t.Run(test.value, func(t *testing.T) {
			source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    continue-on-error: " + test.value + "\n    steps: [{run: true}]\n")
			parsed, err := Parse("workflow.yml", source)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Jobs[0].ContinueOnError != test.want {
				t.Fatalf("continue-on-error %s = %t, want %t", test.value, parsed.Jobs[0].ContinueOnError, test.want)
			}
		})
	}
}

func TestParseMatrixPreservesDeclarationOrderAndCombinationSpans(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        fruit: [apple, pear]
        animal: [cat, dog]
        include:
          - color: green
            animal: cat
        exclude:
          - fruit: pear
            animal: dog
    steps:
      - run: true
`)
	parsed, err := Parse("matrix.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	matrix := parsed.Jobs[0].Matrix
	if matrix.Rows[0].Name != "fruit" || matrix.Rows[1].Name != "animal" {
		t.Fatalf("matrix rows = %#v, want declaration order", matrix.Rows)
	}
	for _, combination := range []MatrixCombination{matrix.Include[0], matrix.Exclude[0]} {
		if combination.Span.Start.Line == 0 || positionAfter(combination.Span.Start, combination.Span.End) {
			t.Fatalf("invalid matrix combination span: %#v", combination.Span)
		}
	}
	if matrix.Span.End.Line != 14 {
		t.Fatalf("matrix span end = %#v, want final exclude value on line 14", matrix.Span.End)
	}
}

func TestParseRetainsRuntimeMatrixIncludeExpression(t *testing.T) {
	parsed, err := Parse("matrix.yml", []byte(`on: push
jobs:
  django:
    needs: build_django_matrix
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJson(needs.build_django_matrix.outputs.include) }}
    steps:
      - run: true
`))
	if err != nil {
		t.Fatal(err)
	}
	matrix := parsed.Jobs[0].Matrix
	if matrix == nil || matrix.IncludeExpression == nil || matrix.IncludeExpression.Text != "${{ fromJson(needs.build_django_matrix.outputs.include) }}" {
		t.Fatalf("matrix include expression = %#v", matrix)
	}
	if matrix.IncludeExpression.Span.Start.Line != 8 || matrix.IncludeExpression.Span.Start.Column != 18 {
		t.Fatalf("matrix include expression span = %#v", matrix.IncludeExpression.Span)
	}
}

func TestParseRetainsMatrixExcludeExpression(t *testing.T) {
	parsed, err := Parse("matrix.yml", []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
        exclude: ${{ fromJSON(vars.EXCLUDE) }}
    steps:
      - run: true
`))
	if err != nil {
		t.Fatal(err)
	}
	matrix := parsed.Jobs[0].Matrix
	if matrix == nil || matrix.ExcludeExpression == nil || matrix.ExcludeExpression.Text != "${{ fromJSON(vars.EXCLUDE) }}" {
		t.Fatalf("matrix exclude expression = %#v", matrix)
	}
	if matrix.ExcludeExpression.Span.Start.Line != 8 || matrix.ExcludeExpression.Span.Start.Column != 18 {
		t.Fatalf("matrix exclude expression span = %#v", matrix.ExcludeExpression.Span)
	}
}

func TestParseOwnsReusableWorkflowCallsAndInputDeclarations(t *testing.T) {
	source := []byte(`on:
  workflow_call:
    inputs:
      enabled:
        type: boolean
        default: true
      target:
        type: string
        required: true
    outputs:
      Published-Value:
        value: ${{ jobs.nested.outputs.result }}
jobs:
  nested:
    uses: ./.github/workflows/nested.yml
    with:
      enabled: false
      target: linux
`)
	parsed, err := Parse(".github/workflows/reusable.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Callable || len(parsed.CallInputs) != 2 {
		t.Fatalf("workflow_call = %#v, want two owned inputs", parsed.CallInputs)
	}
	if got := parsed.CallInputs["enabled"].Default.Data; got != true {
		t.Fatalf("enabled default = %#v, want typed true", got)
	}
	output := parsed.CallOutputs["published-value"]
	if output.Name != "Published-Value" || output.Value != "${{ jobs.nested.outputs.result }}" || output.Span.Start.Line != 12 {
		t.Fatalf("workflow_call output = %#v, want source-cased owned declaration", output)
	}
	call := parsed.Jobs[0].Reusable
	if call == nil || call.Uses != "./.github/workflows/nested.yml" {
		t.Fatalf("reusable call = %#v", call)
	}
	if got := call.Inputs["enabled"].Data; got != false {
		t.Fatalf("enabled call input = %#v, want typed false", got)
	}
}

func TestParseRejectsExpressionValuedExecutionScalars(t *testing.T) {
	tests := []struct {
		name    string
		snippet string
		want    string
	}{
		{name: "fail fast", snippet: "    strategy:\n      fail-fast: ${{ inputs.flag }}\n      matrix:\n        os: [ubuntu-latest]\n    steps:\n      - run: true\n", want: "expression-valued matrix fail-fast is unsupported"},
		{name: "max parallel", snippet: "    strategy:\n      max-parallel: ${{ inputs.count }}\n      matrix:\n        os: [ubuntu-latest]\n    steps:\n      - run: true\n", want: "expression-valued matrix max-parallel is unsupported"},
		{name: "job continue on error", snippet: "    continue-on-error: ${{ matrix.experimental }}\n    steps:\n      - run: true\n", want: "expression-valued job continue-on-error is unsupported"},
		{name: "job timeout", snippet: "    timeout-minutes: ${{ inputs.timeout }}\n    steps:\n      - run: true\n", want: "expression-valued job timeout-minutes is unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n" + test.snippet
			_, err := Parse("expressions.yml", []byte(source))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseRetainsExpressionValuedStepControls(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n        continue-on-error: ${{ matrix.experimental }}\n        timeout-minutes: ${{ matrix.timeout }}\n")
	parsed, err := Parse("expressions.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	step := parsed.Jobs[0].Steps[0]
	if step.ContinueOnErrorExpression != "${{ matrix.experimental }}" || step.TimeoutMinutesExpression != "${{ matrix.timeout }}" {
		t.Fatalf("step controls = %#v", step)
	}
}
