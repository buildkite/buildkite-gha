package compiler

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"go.yaml.in/yaml/v4"
)

const testDistributionDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestCompileBundleGoldenAndDeterministic(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	source := readFile(t, path)
	event := readFile(t, smokePath("events", "push.json"))
	first, err := CompileBundle(path, source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileBundle(path, source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated bundle compilation was not byte-identical")
	}
	if len(first.Plans) != 3 || len(first.IR.Jobs) != 3 {
		t.Fatalf("bundle jobs/plans = %d/%d, want 3/3", len(first.IR.Jobs), len(first.Plans))
	}
	producer := first.IR.Jobs[0].Key
	for i, artifact := range first.Plans {
		if artifact.Path != ".buildkite-gha/plans/"+strings.TrimPrefix(artifact.Digest, "sha256:")+".json" {
			t.Fatalf("plan %d path = %q", i, artifact.Path)
		}
		if i == 0 && len(artifact.Job.Dependencies) != 0 {
			t.Fatalf("producer dependencies = %#v", artifact.Job.Dependencies)
		}
		if i > 0 && !reflect.DeepEqual(artifact.Job.Dependencies, []string{producer}) {
			t.Fatalf("consumer dependencies = %#v, want %q", artifact.Job.Dependencies, producer)
		}
	}
	wantPipeline := readFile(t, filepath.Join("testdata", "shell.pipeline.golden.yml"))
	if !bytes.Equal(first.Pipeline, wantPipeline) {
		t.Fatalf("pipeline changed\nwant:\n%s\ngot:\n%s", wantPipeline, first.Pipeline)
	}
	wantPlans := readFile(t, filepath.Join("testdata", "shell.plans.golden.json"))
	if !bytes.Equal(encodeGoldenPlans(t, first.Plans), wantPlans) {
		t.Fatalf("plans changed; update shell.plans.golden.json intentionally")
	}
}

func TestCompileBundleCompilesSmokeCorpus(t *testing.T) {
	workflows, err := filepath.Glob(smokePath(".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) != 4 {
		t.Fatalf("smoke workflows = %d, want 4", len(workflows))
	}
	event := readFile(t, smokePath("events", "push.json"))
	for _, path := range workflows {
		t.Run(filepath.Base(path), func(t *testing.T) {
			bundle, err := CompileBundle(path, readFile(t, path), event, "0.0.0-test", testDistributionDigest, "gha-importer")
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Plans) == 0 || len(bundle.Pipeline) == 0 {
				t.Fatalf("empty bundle: %#v", bundle)
			}
			var document map[string]any
			if err := yaml.Unmarshal(bundle.Pipeline, &document); err != nil {
				t.Fatalf("pipeline YAML: %v", err)
			}
		})
	}
}

func TestCompileBundleDoesNotExposeEventValues(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	var event map[string]any
	if err := json.Unmarshal(readFile(t, smokePath("events", "push.json")), &event); err != nil {
		t.Fatal(err)
	}
	event["payload"].(map[string]any)["private_fixture_value"] = "super-secret-value"
	eventSource, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := CompileBundle(path, readFile(t, path), eventSource, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bundle.Pipeline, []byte("super-secret-value")) {
		t.Fatal("pipeline contains an event payload value")
	}
	for _, artifact := range bundle.Plans {
		if bytes.Contains(artifact.Contents, []byte("super-secret-value")) {
			t.Fatalf("plan %q contains an event payload value", artifact.Job.Target.StepKey)
		}
	}
}

func TestCompileBundleDeclaresSecretCapabilityAndNames(t *testing.T) {
	source := []byte("name: secrets\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets['deploy_token'] }}\n    steps:\n      - run: echo \\\"${{ secrets.CANARY }}\\\"\n")
	event := readFile(t, smokePath("events", "push.json"))
	bundle, err := CompileBundle("workflow.yml", source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	job := bundle.Plans[0].Job
	if !reflect.DeepEqual(job.RequiredSecrets, []string{"CANARY", "DEPLOY_TOKEN"}) || !reflect.DeepEqual(job.RequiredCapabilities, []string{"secrets"}) {
		t.Fatalf("secret boundary = names %#v capabilities %#v", job.RequiredSecrets, job.RequiredCapabilities)
	}
}

func TestCompileBundleDeclaresScopedGitHubWorkflowToken(t *testing.T) {
	source := []byte(`name: token
on: push
permissions:
  contents: read
  pull-requests: write
jobs:
  token:
    runs-on: ubuntu-latest
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      OTHER: ${{ secrets.OTHER }}
    steps:
      - run: test -n "$GH_TOKEN"
  tokenless:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	bundle, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	jobs := map[string]PlanArtifact{}
	for _, artifact := range bundle.Plans {
		jobs[artifact.Job.Workflow.LogicalJobID] = artifact
	}
	token := jobs["token"]
	if token.Job.Schema != plan.SchemaV6 || token.Job.GitHubToken == nil || !reflect.DeepEqual(token.Job.GitHubToken.Permissions, map[string]string{"contents": "read", "pull_requests": "write"}) {
		t.Fatalf("GitHub workflow token plan = %#v", token.Job)
	}
	if !reflect.DeepEqual(token.Job.RequiredSecrets, []string{"OTHER"}) || !reflect.DeepEqual(token.Job.RequiredCapabilities, []string{"provider-token-write", "secrets"}) {
		t.Fatalf("token secret boundary = names %#v capabilities %#v", token.Job.RequiredSecrets, token.Job.RequiredCapabilities)
	}
	if !reflect.DeepEqual(token.Authorization.ProviderTokenWriteCapabilitySources, []string{"workflow-permissions"}) {
		t.Fatalf("token authorization = %#v", token.Authorization)
	}
	if jobs["tokenless"].Job.GitHubToken != nil || jobs["tokenless"].Job.HasCapability("provider-token-write") {
		t.Fatalf("unreferenced permissions minted a token: %#v", jobs["tokenless"].Job)
	}
}

func TestCompileBundleGitHubTokenRequiresExplicitEffectivePermissions(t *testing.T) {
	for _, permissions := range []string{"", "permissions: {}\n", "permissions:\n  contents: none\n"} {
		source := []byte("on: push\n" + permissions + "jobs:\n  token:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ secrets.GITHUB_TOKEN }}'\n")
		_, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
		if err == nil || !strings.Contains(err.Error(), "references secrets.GITHUB_TOKEN but has no explicit effective permissions") {
			t.Fatalf("CompileBundle() error = %v, want explicit permission rejection", err)
		}
	}
}

func TestCompileBundleJobPermissionsReplaceWorkflowPermissions(t *testing.T) {
	source := []byte(`on: push
permissions:
  contents: read
jobs:
  token:
    runs-on: ubuntu-latest
    permissions:
      pull-requests: write
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    steps: [{run: true}]
`)
	bundle, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if got := bundle.Plans[0].Job.GitHubToken.Permissions; !reflect.DeepEqual(got, map[string]string{"pull_requests": "write"}) {
		t.Fatalf("effective job permissions = %#v", got)
	}
}

func TestCompileBundleRejectsPermissionNormalizationCollision(t *testing.T) {
	source := []byte(`on: push
permissions:
  pull-requests: read
  pull_requests: write
jobs:
  token:
    runs-on: ubuntu-latest
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    steps: [{run: true}]
`)
	_, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), `unsupported permission "pull_requests"`) {
		t.Fatalf("CompileBundle() error = %v, want non-canonical permission rejection", err)
	}
}

func TestCompileBundleRejectsDynamicSecretIndex(t *testing.T) {
	source := []byte("name: secrets\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      SECRET_NAME: TOKEN\n      TOKEN: ${{ secrets[env.SECRET_NAME] }}\n    steps:\n      - run: true\n")
	_, err := CompileBundle("workflow.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err == nil || !strings.Contains(err.Error(), "expression index must be a string literal") {
		t.Fatalf("CompileBundle() error = %v, want dynamic secret index rejection", err)
	}
}

func encodeGoldenPlans(t *testing.T, artifacts []PlanArtifact) []byte {
	t.Helper()
	plans := make([]json.RawMessage, len(artifacts))
	for i, artifact := range artifacts {
		plans[i] = json.RawMessage(bytes.TrimSpace(artifact.Contents))
	}
	encoded, err := json.MarshalIndent(plans, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}
