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

func TestDiscussionDeclarations(t *testing.T) {
	source, err := generatedEventSnapshot("discussion")
	if err != nil {
		t.Fatal(err)
	}
	event, err := compiler.ParseEvent(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range []string{"discussion", "[push, discussion]", "{discussion: null}", "{discussion: {}}", "{discussion: {types: []}}", "{discussion: {types: [transferred]}}"} {
		parsed, err := workflow.Parse("discussion.yml", []byte("on: "+declaration+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"created", "edited", "deleted", "transferred", "pinned", "unpinned", "labeled", "unlabeled", "locked", "unlocked", "category_changed", "answered", "unanswered", "closed", "reopened"} {
			event.Payload["action"] = action
			expressions, snapshot := snapshotTriggerState(event)
			condition, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, "discussion", expressions, snapshot)
			if err != nil || !applicable {
				t.Fatalf("%s (%s): applicable=%v, %v", declaration, action, applicable, err)
			}
			if strings.Contains(declaration, "[transferred]") && !strings.Contains(condition, `"`+action+`" == "transferred"`) {
				t.Fatalf("condition lost activity filter: %s", condition)
			}
			reason, err := buildkite.TriggerFilterMismatchReason(parsed.Triggers, "discussion", snapshot)
			wantMismatch := strings.Contains(declaration, "[transferred]") && action != "transferred"
			if err != nil || (reason != "") != wantMismatch {
				t.Fatalf("%s (%s): reason=%q wantMismatch=%v, %v", declaration, action, reason, wantMismatch, err)
			}
		}
	}
	for _, config := range []string{"types: 'null'", "types: [unknown]", "branches-ignore: [main]", "paths: [src/**]"} {
		parsed, err := workflow.Parse("discussion.yml", []byte("on: {discussion: {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err == nil && buildkite.ValidateTriggerConditions(parsed.Triggers) == nil {
			t.Fatalf("accepted %s", config)
		}
	}
}

func TestPluginDiscussionEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"discussion.yml": "on: {discussion: {types: [transferred]}}\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n        env:\n          TITLE: ${{ github.event.discussion.title }}\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	t.Setenv("BUILDKITE_GITHUB_ACTION", "transferred")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/discussion.yml", "", "discussion", "buildkite/buildkite-gha/.github/workflows/discussion.yml@refs/heads/trunk")
	payload := []byte(`{"action":"transferred","discussion":{"id":27,"number":3,"title":"Question"},"changes":{"new_repository":{"id":999,"full_name":"other/repo","default_branch":"destination"}},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`)
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
		if job.Event.Name != "discussion" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		env := job.Program.Job.Steps[0].Env
		if len(env) != 1 || env[0].Name != "TITLE" || env[0].Value.Source != "Question" {
			t.Fatalf("lost discussion context: %#v", env)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`"transferred"`), []byte(`"closed"`), 1),
		bytes.Replace(payload, []byte(`"id":27`), []byte(`"id":0`), 1),
		bytes.Replace(payload, []byte(`"number":3`), []byte(`"number":null`), 1),
		bytes.Replace(payload, []byte(`"title":"Question"`), []byte(`"title":null`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`foreign/repo`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid discussion payload")
		}
	}
	for key, value := range map[string]string{pipelineTriggerWorkflowPathEnvironment: "", githubWorkflowRefEnvironment: "", githubWorkflowSHAEnvironment: "", "BUILDKITE_GITHUB_ACTION": "created"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
				t.Fatal("accepted missing or conflicting identity")
			}
		})
	}
}
