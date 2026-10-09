package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestPluginPageBuildEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"pages.yml": "on: page_build\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n        env:\n          STATUS: ${{ github.event.build.status }}\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/pages.yml", "", "page_build", "buildkite/buildkite-gha/.github/workflows/pages.yml@refs/heads/trunk")
	payload := []byte(`{"id":27,"build":{"commit":"cccccccccccccccccccccccccccccccccccccccc","status":"errored","error":{"message":"Pages failed"}},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`)
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
		if job.Event.Name != "page_build" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		env := job.Program.Job.Steps[0].Env
		if len(env) != 1 || env[0].Name != "STATUS" || env[0].Value.Source != "errored" {
			t.Fatalf("lost Pages build context: %#v", env)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`"id":27`), []byte(`"id":null`), 1),
		bytes.Replace(payload, []byte(`"errored"`), []byte(`null`), 1),
		bytes.Replace(payload, []byte(`cccccccccccccccccccccccccccccccccccccccc`), []byte(`bad`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
		bytes.Replace(payload, []byte(`{"id":27`), []byte(`{"action":"built","id":27`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid page_build payload")
		}
	}
	for _, key := range []string{pipelineTriggerWorkflowPathEnvironment, githubWorkflowRefEnvironment, githubWorkflowSHAEnvironment} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "")
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
				t.Fatal("accepted missing workflow identity")
			}
		})
	}
	source, err := generatedEventSnapshot("page_build")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.ParseEvent(source); err != nil {
		t.Fatal(err)
	}
}
