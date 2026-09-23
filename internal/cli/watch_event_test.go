package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestWatchDeclarations(t *testing.T) {
	source, err := generatedEventSnapshot("watch")
	if err != nil {
		t.Fatal(err)
	}
	event, err := compiler.ParseEvent(source)
	if err != nil {
		t.Fatal(err)
	}
	expressions, snapshot := snapshotTriggerState(event)
	expressions.EventPredicate = "true"
	for _, declaration := range []string{"watch", "[push, watch]", "{watch: null}", "{watch: {}}", "{watch: {types: []}}", "{watch: {types: [started]}}"} {
		parsed, err := workflow.Parse("watch.yml", []byte("on: "+declaration+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, "watch", expressions, snapshot); err != nil || !applicable {
			t.Fatalf("%s: %v, %v", declaration, applicable, err)
		}
	}
	for _, config := range []string{"types: 'null'", "types: [deleted]", "branches-ignore: [main]", "paths: [src/**]"} {
		parsed, err := workflow.Parse("watch.yml", []byte("on: {watch: {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err == nil && buildkite.ValidateTriggerConditions(parsed.Triggers) == nil {
			t.Fatalf("accepted %s", config)
		}
	}
}

func TestPluginWatchEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"watch.yml": "on: {watch: {types: [started]}}\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n        env:\n          ACTIVITY: ${{ github.event.action }}\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	t.Setenv("BUILDKITE_GITHUB_ACTION", "started")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/watch.yml", "", "watch", "buildkite/buildkite-gha/.github/workflows/watch.yml@refs/heads/trunk")
	payload := []byte(`{"action":"started","sender":{"login":"stargazer"},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`)
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
		if job.Event.Name != "watch" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		env := job.Program.Job.Steps[0].Env
		if len(env) != 1 || env[0].Name != "ACTIVITY" || env[0].Value.Source != "started" {
			t.Fatalf("lost star context: %#v", env)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`"started"`), []byte(`"deleted"`), 1),
		bytes.Replace(payload, []byte(`"started"`), []byte(`null`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid watch payload")
		}
	}
	for key, value := range map[string]string{pipelineTriggerWorkflowPathEnvironment: "", githubWorkflowRefEnvironment: "", githubWorkflowSHAEnvironment: "", "BUILDKITE_GITHUB_ACTION": "deleted"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
				t.Fatal("accepted missing or conflicting identity")
			}
		})
	}
}
