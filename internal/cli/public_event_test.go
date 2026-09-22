package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestPluginPublicEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"public.yml": "on: public\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/public.yml", "", "public", "buildkite/buildkite-gha/.github/workflows/public.yml@refs/heads/trunk")
	payload := []byte(`{"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale","private":false}}`)
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
		if job.Event.Name != "public" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`false`), []byte(`true`), 1),
		bytes.Replace(payload, []byte(`false`), []byte(`null`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
		bytes.Replace(payload, []byte(`{"repository"`), []byte(`{"action":"publicized","repository"`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid public payload")
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
	source, err := generatedEventSnapshot("public")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.ParseEvent(source); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{`"ref":"refs/heads/main"`, `"ref":"refs/heads/other"`},
		{`"id":1`, `"id":null`},
		{`"private":false`, `"private":true`},
	} {
		if _, err := compiler.ParseEvent(bytes.Replace(source, []byte(change[0]), []byte(change[1]), 1)); err == nil {
			t.Fatalf("accepted snapshot mutation %v", change)
		}
	}
}
