package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"go.yaml.in/yaml/v4"
)

// The three revisions deliberately disagree. The PR targets release, not the
// default branch trunk; both PR revisions contain different executable source.
func targetFixture(t *testing.T, headRepo, filters string) (root, trusted, base, head string, webhook []byte) {
	t.Helper()
	root = writeUploadWorkflowRepository(t, map[string]string{
		"ci.yml":       "name: Target\non:\n  pull_request_target:\n" + filters + "jobs:\n  direct:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo TRUSTED_DEFAULT\n        env:\n          HEAD_REF: ${{ github.head_ref }}\n          BASE_REF: ${{ github.base_ref }}\n          PR: ${{ github.event.number }}\n      - uses: ./.github/actions/sentinel\n  call:\n    uses: ./.github/workflows/reusable.yml\n",
		"reusable.yml": "on: workflow_call\njobs:\n  child:\n    runs-on: ubuntu-latest\n    steps: [{run: echo TRUSTED_REUSABLE}]\n",
	})
	write := func(path, value string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string { return targetGit(t, root, args...) }
	git("config", "user.email", "sentinel@example.com")
	git("config", "user.name", "Sentinel")
	git("remote", "add", "origin", "https://github.com/buildkite/buildkite-gha.git")
	write(".github/actions/sentinel/action.yml", "name: sentinel\nruns:\n  using: composite\n  steps:\n    - run: echo TRUSTED_ACTION\n      shell: bash\n")
	git("add", ".")
	git("commit", "-qm", "trusted default source")
	trusted = git("rev-parse", "HEAD")
	git("branch", "trunk")
	write("release.txt", "base-only change\n")
	for _, path := range []string{".github/workflows/ci.yml", ".github/workflows/reusable.yml", ".github/actions/sentinel/action.yml"} {
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		write(path, strings.ReplaceAll(string(contents), "TRUSTED_", "BASE_"))
	}
	git("add", ".")
	git("commit", "-qm", "release base")
	base = git("rev-parse", "HEAD")
	git("branch", "release")
	write("src/pr.txt", "head-only change\n")
	write(".github/workflows/ci.yml", "on: pull_request_target\njobs:\n  stolen:\n    runs-on: ubuntu-latest\n    steps: [{run: echo UNTRUSTED_HEAD}]\n")
	write(".github/workflows/reusable.yml", "on: workflow_call\njobs:\n  stolen:\n    runs-on: ubuntu-latest\n    steps: [{run: echo UNTRUSTED_REUSABLE}]\n")
	write(".github/actions/sentinel/action.yml", "name: sentinel\nruns:\n  using: composite\n  steps:\n    - run: echo UNTRUSTED_ACTION\n      shell: bash\n")
	git("add", ".")
	git("commit", "-qm", "untrusted head")
	head = git("rev-parse", "HEAD")
	// Simulate a default ref moving after dispatch without moving execution.
	git("update-ref", "refs/heads/trunk", head)
	git("checkout", "--detach", trusted)
	t.Chdir(root)
	setCLIPluginBuildkiteEnvironment(t, "target-importer")
	t.Setenv(pluginConfigurationEnvironment, `{}`)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", root)
	t.Setenv("BUILDKITE_COMMIT", trusted)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	t.Setenv("BUILDKITE_PIPELINE_DEFAULT_BRANCH", "stale-pipeline-default")
	t.Setenv("BUILDKITE_PULL_REQUEST", "42")
	t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "release")
	t.Setenv("BUILDKITE_PULL_REQUEST_REPO", "https://github.com/"+headRepo+".git")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/ci.yml", "Target", "pull_request_target", "buildkite/buildkite-gha/.github/workflows/ci.yml@refs/heads/trunk")
	t.Setenv("BUILDKITE_GITHUB_ACTION", "opened")
	webhook = []byte(`{"action":"opened","number":42,"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale-webhook-default"},"pull_request":{"number":42,"title":"$(echo UNTRUSTED_TITLE)","mergeable":false,"merged":false,"base":{"ref":"release","sha":"` + base + `","repo":{"id":123,"full_name":"buildkite/buildkite-gha"}},"head":{"ref":"feature","sha":"` + head + `","repo":{"id":456,"full_name":"` + headRepo + `"}}}}`)
	if headRepo == "buildkite/buildkite-gha" {
		webhook = bytes.Replace(webhook, []byte(`"id":456`), []byte(`"id":123`), 1)
	}
	return
}

func targetGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestPluginPullRequestTargetTrustedSource(t *testing.T) {
	requireImporterHost(t)
	for _, headRepo := range []string{"contributor/fork", "buildkite/buildkite-gha"} {
		t.Run(headRepo, func(t *testing.T) {
			_, trusted, _, head, webhook := targetFixture(t, headRepo, "    branches: [release]\n    paths: ['src/**']\n")
			runner := &cliCaptureRunner{webhook: webhook}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 || !strings.Contains(stdout.String(), "Uploaded 2 jobs") {
				t.Fatalf("plugin: %d\n%s\n%s", code, &stdout, &stderr)
			}
			plans := 0
			var encodedPlans []byte
			for path, contents := range runner.uploaded {
				if !strings.HasPrefix(path, ".buildkite-gha/plans/") {
					continue
				}
				job, err := plan.Decode(contents)
				if err != nil {
					t.Fatal(err)
				}
				plans++
				encodedPlans = append(encodedPlans, contents...)
				if !job.Event.PayloadArtifact {
					t.Fatal("target plan omitted checkout guard payload")
				}
				if job.Event.Name != "pull_request_target" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != trusted || job.Event.Repository != "buildkite/buildkite-gha" || job.Event.HeadRef != "feature" || job.Event.BaseRef != "release" {
					t.Fatalf("incorrect execution identity: %#v", job.Event)
				}
			}
			if plans != 2 {
				t.Fatalf("plans = %d", plans)
			}
			for _, marker := range []string{"TRUSTED_DEFAULT", "TRUSTED_ACTION", "TRUSTED_REUSABLE", "feature", "release"} {
				if !bytes.Contains(encodedPlans, []byte(marker)) {
					t.Errorf("plans omit %s", marker)
				}
			}
			for _, marker := range []string{"UNTRUSTED_HEAD", "UNTRUSTED_ACTION", "UNTRUSTED_REUSABLE", "BASE_DEFAULT", "BASE_ACTION", "BASE_REUSABLE"} {
				if bytes.Contains(encodedPlans, []byte(marker)) {
					t.Errorf("plans executed %s", marker)
				}
			}
			source, err := buildkiteWebhookEventSource(os.Getenv, webhook)
			if err != nil {
				t.Fatal(err)
			}
			event, err := compiler.ParseEvent(source)
			if err != nil {
				t.Fatal(err)
			}
			if event.Repository.DefaultBranch != "trunk" || nestedString(event.Payload, "pull_request", "head", "sha") != head || nestedString(event.Payload, "pull_request", "head", "repo", "full_name") != headRepo || nestedInt(event.Payload, "number") != 42 {
				t.Fatalf("PR data was replaced by default-checkout identity: %#v", event)
			}
		})
	}
}

func TestPullRequestTargetRejectsIdentityAndCheckoutSubstitution(t *testing.T) {
	for _, mutation := range []string{"head checkout", "dirty workflow", "dirty action", "ignored action", "index override", "wrong origin", "missing payload", "wrong path", "wrong SHA", "wrong ref", "wrong base repository", "wrong base ID", "missing base", "wrong head repository", "missing head ID", "forged head ID", "wrong number", "wrong action", "conflicting event"} {
		t.Run(mutation, func(t *testing.T) {
			root, _, _, head, webhook := targetFixture(t, "contributor/fork", "")
			var payload map[string]any
			if err := json.Unmarshal(webhook, &payload); err != nil {
				t.Fatal(err)
			}
			pr := payload["pull_request"].(map[string]any)
			switch mutation {
			case "head checkout":
				targetGit(t, root, "checkout", "--detach", head)
			case "dirty workflow", "dirty action", "ignored action", "index override":
				path := ".github/actions/sentinel/action.yml"
				if mutation == "dirty workflow" {
					path = ".github/workflows/ci.yml"
				}
				if mutation == "ignored action" {
					path = ".github/actions/sentinel/generated.sh"
					if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte("generated.sh\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if mutation == "index override" {
					targetGit(t, root, "update-index", "--assume-unchanged", path)
				}
				if err := os.WriteFile(filepath.Join(root, path), []byte("UNTRUSTED_LOCAL"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong origin":
				targetGit(t, root, "remote", "set-url", "origin", "https://github.com/contributor/fork.git")
			case "missing payload":
			case "wrong path":
				t.Setenv(pipelineTriggerWorkflowPathEnvironment, ".github/workflows/reusable.yml")
			case "wrong SHA":
				t.Setenv(githubWorkflowSHAEnvironment, head)
			case "wrong ref":
				t.Setenv(githubWorkflowRefEnvironment, "buildkite/buildkite-gha/.github/workflows/ci.yml@refs/pull/42/merge")
			case "wrong base repository":
				pr["base"].(map[string]any)["repo"].(map[string]any)["full_name"] = "attacker/fork"
			case "wrong base ID":
				pr["base"].(map[string]any)["repo"].(map[string]any)["id"] = 789
			case "missing base":
				delete(pr, "base")
			case "wrong head repository":
				pr["head"].(map[string]any)["repo"].(map[string]any)["full_name"] = "attacker/other"
			case "missing head ID":
				delete(pr["head"].(map[string]any)["repo"].(map[string]any), "id")
			case "forged head ID":
				pr["head"].(map[string]any)["repo"].(map[string]any)["id"] = 123
			case "wrong number":
				payload["number"] = 41
			case "wrong action":
				payload["action"] = "closed"
			case "conflicting event":
				t.Setenv("BUILDKITE_GITHUB_EVENT", "pull_request")
			}
			webhook, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "missing payload" {
				webhook = nil
			}
			runner := &cliCaptureRunner{webhook: webhook}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code == 0 || strings.Contains(stdout.String(), "Uploaded") {
				t.Fatalf("accepted %s: %d\n%s\n%s", mutation, code, &stdout, &stderr)
			}
			for path := range runner.uploaded {
				if strings.HasPrefix(path, ".buildkite-gha/plans/") {
					t.Fatalf("uploaded executable plan after %s", mutation)
				}
			}
		})
	}
}

func TestPluginPullRequestTargetActivitiesAndPathFilters(t *testing.T) {
	for _, test := range []struct {
		name, action, filters string
		wantJobs              bool
	}{
		{"opened default", "opened", "", true},
		{"reopened default", "reopened", "", true},
		{"synchronize default", "synchronize", "", true},
		{"closed excluded by default", "closed", "", false},
		{"closed merged", "closed", "    types: [closed]\n", true},
		{"edited base", "edited", "    types: [edited]\n    branches: [release]\n", true},
		{"filters use PR base not default", "opened", "    branches: [trunk]\n    paths: ['src/**']\n", false},
		{"paths exclude base-only changes", "opened", "    paths: ['release.txt']\n", false},
		{"paths ignore all PR changes", "opened", "    paths-ignore: ['src/**', '.github/**']\n", false},
		{"paths ordered negative", "opened", "    paths: ['src/**', '!src/pr.txt']\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, _, webhook := targetFixture(t, "contributor/fork", test.filters)
			webhook = bytes.Replace(webhook, []byte(`"action":"opened"`), []byte(`"action":"`+test.action+`"`), 1)
			if test.action == "closed" {
				webhook = bytes.Replace(webhook, []byte(`"merged":false`), []byte(`"merged":true`), 1)
			}
			t.Setenv("BUILDKITE_GITHUB_ACTION", test.action)
			runner := &cliCaptureRunner{webhook: webhook}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("plugin: %d\n%s\n%s", code, &stdout, &stderr)
			}
			plans := 0
			for path := range runner.uploaded {
				if strings.HasPrefix(path, ".buildkite-gha/plans/") {
					plans++
				}
			}
			want := 0
			if test.wantJobs {
				want = 2
			}
			// Branch/activity filters are deferred to Buildkite condition evaluation;
			// path exclusions omit plans. Assert the actual false condition rather
			// than mistaking an uploaded (but gated) plan for an executed job.
			falseCondition := ""
			if test.name == "closed excluded by default" {
				falseCondition = `("closed" == "opened" || "closed" == "synchronize" || "closed" == "reopened")`
			}
			if test.name == "filters use PR base not default" {
				falseCondition = `"release" =~ /^trunk$/`
			}
			if falseCondition != "" {
				want = 2
				var pipeline struct {
					Steps []struct {
						Condition string `yaml:"if"`
					} `yaml:"steps"`
				}
				if err := yaml.Unmarshal(runner.commands[len(runner.commands)-1].stdin, &pipeline); err != nil {
					t.Fatal(err)
				}
				if len(pipeline.Steps) != 1 || !strings.Contains(pipeline.Steps[0].Condition, falseCondition) {
					t.Fatalf("missing false filter %q: %#v", falseCondition, pipeline)
				}
			}
			if plans != want {
				t.Fatalf("plans = %d, want %d\n%s\n%s", plans, want, &stdout, &stderr)
			}
		})
	}
}
