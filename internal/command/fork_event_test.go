package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestPluginForkEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"fork.yml": `name: Forks
on: fork
permissions: {}
jobs:
  marker:
    runs-on: ubuntu-latest
    outputs:
      marker: ${{ steps.emit.outputs.marker }}
      event: ${{ steps.emit.outputs.event }}
    steps:
      - id: emit
        env:
          FORKEE: ${{ github.event.forkee.full_name }}
        run: |
          echo "event=$(cat "$GITHUB_EVENT_PATH")" >> "$GITHUB_OUTPUT"
          echo "marker=$FORKEE|$GITHUB_REF|$GITHUB_SHA" >> "$GITHUB_OUTPUT"
`,
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	t.Setenv("BUILDKITE_PIPELINE_DEFAULT_BRANCH", "stale-pipeline")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/fork.yml", "Forks", "fork", "buildkite/buildkite-gha/.github/workflows/fork.yml@refs/heads/trunk")
	payload := []byte(`{"forkee":{"id":456,"full_name":"contributor/fork","default_branch":"fork-branch","sha":"cccccccccccccccccccccccccccccccccccccccc"},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"},"sender":{"login":"octocat"}}`)
	runner := &cliCaptureRunner{webhook: payload}
	var stdout, stderr bytes.Buffer
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
		if job.Event.Name != "fork" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		t.Run("execute", func(t *testing.T) {
			setCLIJobIdentity(t, job, transport.Digest(data))
			planPath, resultPath := filepath.Join(t.TempDir(), "plan.json"), filepath.Join(t.TempDir(), "result.json")
			if err := os.WriteFile(planPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if code := run([]string{"run-job", "--plan", planPath, "--artifact-producer", cliTestJobID, "--result", resultPath}, &stdout, &stderr, "dev", &cliCaptureRunner{dataByPath: runner.uploaded}); code != 0 {
				t.Fatalf("run-job = %d: %s", code, &stderr)
			}
			result, err := os.ReadFile(resultPath)
			marker := "contributor/fork|refs/heads/trunk|" + os.Getenv("BUILDKITE_COMMIT")
			if err != nil || !bytes.Contains(result, []byte(marker)) {
				t.Fatalf("missing marker %q: %s (%v)", marker, result, err)
			}
			var completed struct {
				Outputs map[string]string `json:"outputs"`
			}
			if err := json.Unmarshal(result, &completed); err != nil {
				t.Fatal(err)
			}
			observed := []byte(completed.Outputs["event"])
			var want, got any
			if err := json.Unmarshal(payload, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(observed, &got); err != nil || !reflect.DeepEqual(want, got) {
				t.Fatalf("event file changed: %s (%v)", observed, err)
			}
			t.Log(marker)
		})
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for key, value := range map[string]string{
		"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_TAG": "trunk", "BUILDKITE_BRANCH": "fork-branch",
		githubWorkflowSHAEnvironment: "", githubWorkflowRefEnvironment: "other/repo/.github/workflows/fork.yml@refs/heads/trunk",
		pipelineTriggerWorkflowPathEnvironment: "",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
				t.Fatal("accepted contradictory workflow identity")
			}
		})
	}
	for name, broken := range map[string][]byte{
		"missing": nil, "malformed": []byte(`{}`),
		"foreign":     bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
		"forkee id":   bytes.Replace(payload, []byte(`"id":456`), []byte(`"id":null`), 1),
		"source id":   bytes.Replace(payload, []byte(`"id":123`), []byte(`"id":null`), 1),
		"forkee name": bytes.Replace(payload, []byte(`"full_name":"contributor/fork"`), []byte(`"full_name":null`), 1),
		"activity":    bytes.Replace(payload, []byte(`{"forkee"`), []byte(`{"action":"created","forkee"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
				t.Fatal("accepted invalid fork payload")
			}
		})
	}
}
