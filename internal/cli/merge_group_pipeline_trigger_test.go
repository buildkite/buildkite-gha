package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestPluginMergeGroupPipelineTrigger(t *testing.T) {
	requireImporterHost(t)
	for _, declaration := range []string{"merge_group", "{merge_group: {types: [checks_requested], branches: [stable]}}"} {
		t.Run(declaration, func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{
				"queue.yml": "name: Queue\non: " + declaration + "\npermissions: {contents: read}\njobs:\n  marker:\n    runs-on: ubuntu-latest\n    outputs:\n      marker: ${{ steps.emit.outputs.marker }}\n    steps:\n      - id: emit\n        run: echo 'marker=${{ github.event.merge_group.base_sha }}' >> \"$GITHUB_OUTPUT\"\n",
			})
			t.Chdir(repository)
			t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
			setCLIPluginBuildkiteEnvironment(t, "")
			t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_BRANCH", "gh-readonly-queue/stable/pr-42-deadbeef")
			t.Setenv("BUILDKITE_MERGE_QUEUE_BASE_BRANCH", "stable")
			t.Setenv("BUILDKITE_MERGE_QUEUE_BASE_COMMIT", strings.Repeat("c", 40))
			t.Setenv("BUILDKITE_GITHUB_ACTION", "checks_requested")
			ref := "refs/heads/gh-readonly-queue/stable/pr-42-deadbeef"
			setCLIPipelineTriggerEnvironment(t, ".github/workflows/queue.yml", "Queue", "merge_group", "buildkite/buildkite-gha/.github/workflows/queue.yml@"+ref)
			payload := []byte(fmt.Sprintf(`{"action":"checks_requested","repository":{"full_name":"buildkite/buildkite-gha"},"merge_group":{"head_ref":%q,"head_sha":%q,"base_ref":"refs/heads/stable","base_sha":%q}}`, ref, os.Getenv("BUILDKITE_COMMIT"), strings.Repeat("c", 40)))
			var stdout, stderr bytes.Buffer
			runner := &cliCaptureRunner{webhook: payload}
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("plugin = %d: %s", code, &stderr)
			}
			plans := 0
			for path, data := range runner.uploaded {
				if !strings.HasPrefix(path, ".buildkite-gha/plans/") || !strings.HasSuffix(path, ".json") {
					continue
				}
				job, err := plan.Decode(data)
				if err != nil {
					t.Fatal(err)
				}
				if job.Event.Name != "merge_group" || job.Event.Ref != ref || job.Event.SHA != "0123456789abcdef0123456789abcdef01234567" || job.GitHubToken != nil {
					t.Fatalf("queue identity or token demand changed: %#v", job)
				}
				t.Run("execute marker", func(t *testing.T) {
					setCLIJobIdentity(t, job, transport.Digest(data))
					planPath := filepath.Join(t.TempDir(), "plan.json")
					if err := os.WriteFile(planPath, data, 0o600); err != nil {
						t.Fatal(err)
					}
					resultPath := filepath.Join(t.TempDir(), "result.json")
					if code := run([]string{"run-job", "--plan", planPath, "--artifact-producer", cliTestJobID, "--result", resultPath}, &stdout, &stderr, "dev", &cliCaptureRunner{dataByPath: runner.uploaded}); code != 0 {
						t.Fatalf("run-job = %d: %s", code, &stderr)
					}
					result, err := os.ReadFile(resultPath)
					if err != nil || !strings.Contains(string(result), `"marker": "`+strings.Repeat("c", 40)+`"`) {
						t.Fatalf("marker=%s error=%v", result, err)
					}
				})
				plans++
			}
			if plans != 1 {
				t.Fatalf("plans=%d", plans)
			}
			for name, value := range map[string]string{
				githubWorkflowRefEnvironment:        "buildkite/buildkite-gha/.github/workflows/queue.yml@refs/heads/wrong",
				githubWorkflowSHAEnvironment:        strings.Repeat("b", 40),
				"BUILDKITE_BRANCH":                  "wrong",
				"BUILDKITE_MERGE_QUEUE_BASE_BRANCH": "main",
				"BUILDKITE_MERGE_QUEUE_BASE_COMMIT": strings.Repeat("d", 40),
				"BUILDKITE_GITHUB_ACTION":           "destroyed",
			} {
				t.Run(name, func(t *testing.T) {
					t.Setenv(name, value)
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
						t.Fatal("accepted mismatched queue identity")
					}
				})
			}
			for _, key := range []string{githubWorkflowRefEnvironment, githubWorkflowSHAEnvironment, "BUILDKITE_MERGE_QUEUE_BASE_BRANCH", "BUILDKITE_MERGE_QUEUE_BASE_COMMIT"} {
				t.Run("missing "+key, func(t *testing.T) {
					t.Setenv(key, "")
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
						t.Fatal("accepted missing queue identity")
					}
				})
			}
			for name, broken := range map[string][]byte{
				"repository": bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
				"destroyed":  bytes.Replace(payload, []byte(`checks_requested`), []byte(`destroyed`), 1),
			} {
				t.Run(name, func(t *testing.T) {
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
						t.Fatal("accepted invalid queue payload")
					}
				})
			}
			stderr.Reset()
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{}) == 0 || !strings.Contains(stderr.String(), "original buildkite:webhook") {
				t.Fatalf("missing queue payload must fail explicitly: %s", &stderr)
			}
		})
	}
}
