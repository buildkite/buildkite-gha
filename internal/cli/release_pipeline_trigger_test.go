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

func TestPluginReleasePipelineTriggerDiagnosticLinks(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"release.yml": "name: Release\non: {release: {types: [published]}}\njobs:\n  marker:\n    runs-on: ubuntu-latest\n    concurrency:\n      group: release\n      cancel-in-progress: true\n    steps: [{run: true}]\n",
	})
	t.Chdir(repository)
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_BRANCH", "v2.3.4")
	t.Setenv("BUILDKITE_TAG", "v2.3.4")
	t.Setenv("BUILDKITE_GITHUB_ACTION", "published")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/release.yml", "Release", "release", "buildkite/buildkite-gha/.github/workflows/release.yml@refs/tags/v2.3.4")
	runner := &cliCaptureRunner{webhook: []byte(`{"action":"published","repository":{"full_name":"buildkite/buildkite-gha"},"release":{"tag_name":"v2.3.4","draft":false,"prerelease":false}}`)}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("plugin = %d: %s", code, &stderr)
	}
	for _, data := range runner.uploaded {
		if bytes.Contains(data, []byte(`href="https://github.com/buildkite/buildkite-gha/blob/0123456789abcdef0123456789abcdef01234567/.github/workflows/release.yml#L`)) &&
			bytes.Contains(data, []byte("cancel-in-progress is unsupported")) {
			return
		}
	}
	t.Fatalf("release failure diagnostic lacks a link to the immutable workflow commit: %s", &stderr)
}

func TestPluginReleasePipelineTrigger(t *testing.T) {
	requireImporterHost(t)
	for _, action := range []string{"published", "created", "released"} {
		t.Run(action, func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{
				"release.yml": "name: Release\non: {release: {types: [published, created, released]}}\npermissions: {}\njobs:\n  marker:\n    runs-on: ubuntu-latest\n    outputs:\n      marker: ${{ steps.emit.outputs.marker }}\n    steps:\n      - id: emit\n        run: echo 'marker=${{ github.event.release.tag_name }}' >> \"$GITHUB_OUTPUT\"\n",
				"other.yml":   "on: push\n",
			})
			t.Chdir(repository)
			t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
			setCLIPluginBuildkiteEnvironment(t, "")
			t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_BRANCH", "v2.3.4")
			t.Setenv("BUILDKITE_TAG", "v2.3.4")
			t.Setenv("BUILDKITE_GITHUB_ACTION", action)
			setCLIPipelineTriggerEnvironment(t, ".github/workflows/release.yml", "Release", "release", "buildkite/buildkite-gha/.github/workflows/release.yml@refs/tags/v2.3.4")
			payload := []byte(fmt.Sprintf(`{"action":%q,"repository":{"full_name":"buildkite/buildkite-gha"},"release":{"tag_name":"v2.3.4","draft":false,"prerelease":true}}`, action))
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
				if job.Event.Name != "release" || job.Event.Ref != "refs/tags/v2.3.4" || job.Event.SHA != "0123456789abcdef0123456789abcdef01234567" || job.GitHubToken != nil {
					t.Fatalf("release identity or token demand changed: %#v", job)
				}
				t.Run("execute marker", func(t *testing.T) {
					setCLIJobIdentity(t, job, transport.Digest(data))
					planPath := filepath.Join(t.TempDir(), "plan.json")
					if err := os.WriteFile(planPath, data, 0o600); err != nil {
						t.Fatal(err)
					}
					resultPath := filepath.Join(t.TempDir(), "result.json")
					stderr.Reset()
					if code := run([]string{"run-job", "--plan", planPath, "--result", resultPath}, &stdout, &stderr, "dev", &cliCaptureRunner{dataByPath: runner.uploaded}); code != 0 {
						t.Fatalf("run-job = %d: %s", code, &stderr)
					}
					marker, err := os.ReadFile(resultPath)
					if err != nil || !strings.Contains(string(marker), `"marker": "v2.3.4"`) {
						t.Fatalf("marker=%q error=%v", marker, err)
					}
				})
				plans++
			}
			if plans != 1 {
				t.Fatalf("plans = %d, want selected release workflow only", plans)
			}
			for name, value := range map[string]string{
				githubWorkflowRefEnvironment: "buildkite/buildkite-gha/.github/workflows/release.yml@refs/tags/wrong",
				githubWorkflowSHAEnvironment: strings.Repeat("b", 40),
				"BUILDKITE_TAG":              "wrong",
			} {
				t.Run(name, func(t *testing.T) {
					t.Setenv(name, value)
					stderr.Reset()
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
						t.Fatal("accepted mismatched identity")
					}
				})
			}
			for name, broken := range map[string][]byte{
				"draft":      bytes.Replace(payload, []byte(`"draft":false`), []byte(`"draft":true`), 1),
				"repository": bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
				"activity":   bytes.Replace(payload, []byte(fmt.Sprintf(`"action":%q`, action)), []byte(`"action":"prereleased"`), 1),
			} {
				t.Run(name, func(t *testing.T) {
					stderr.Reset()
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
						t.Fatal("accepted invalid release payload")
					}
				})
			}
			t.Setenv("BUILDKITE_SOURCE", "ui")
			stderr.Reset()
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{}) == 0 || !strings.Contains(stderr.String(), "original buildkite:webhook") {
				t.Fatalf("missing payload must fail explicitly: %s", &stderr)
			}
			for _, key := range []string{githubWorkflowRefEnvironment, githubWorkflowSHAEnvironment} {
				t.Run("missing "+key, func(t *testing.T) {
					t.Setenv(key, "")
					if _, _, err := pluginWorkflowOperands(pluginConfiguration{}, os.Getenv); err == nil {
						t.Fatal("accepted missing workflow identity")
					}
				})
			}
		})
	}
}
