package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestPluginGollumEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"wiki.yml": "on: gollum\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n        env:\n          PAGE: ${{ github.event.pages[1].page_name }}\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/wiki.yml", "", "gollum", "buildkite/buildkite-gha/.github/workflows/wiki.yml@refs/heads/trunk")
	payload := []byte(`{"pages":[{"page_name":"Home","action":"created","sha":"cccccccccccccccccccccccccccccccccccccccc"},{"page_name":"Install","action":"edited","sha":"dddddddddddddddddddddddddddddddddddddddd"}],"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`)
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
		if job.Event.Name != "gollum" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity, payload or token demand: %s", data)
		}
		env := job.Program.Job.Steps[0].Env
		if len(env) != 1 || env[0].Name != "PAGE" || env[0].Value.Source != "Install" {
			t.Fatalf("lost page context: %#v", env)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`"pages":[`), []byte(`"pages":[null,`), 1),
		bytes.Replace(payload, []byte(`"edited"`), []byte(`"deleted"`), 1),
		bytes.Replace(payload, []byte(`"Install"`), []byte(`null`), 1),
		bytes.Replace(payload, []byte(`dddddddddddddddddddddddddddddddddddddddd`), []byte(`bad`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
		bytes.Replace(payload, []byte(`{"pages"`), []byte(`{"action":"created","pages"`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid gollum payload")
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
	source, err := generatedEventSnapshot("gollum")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.ParseEvent(source); err != nil {
		t.Fatal(err)
	}
}
