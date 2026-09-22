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

func TestDiscussionCommentDeclarations(t *testing.T) {
	source, err := generatedEventSnapshot("discussion_comment")
	if err != nil {
		t.Fatal(err)
	}
	event, err := compiler.ParseEvent(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range []string{"discussion_comment", "[push, discussion_comment]", "{discussion_comment: null}", "{discussion_comment: {}}", "{discussion_comment: {types: []}}", "{discussion_comment: {types: [edited]}}"} {
		parsed, err := workflow.Parse("comment.yml", []byte("on: "+declaration+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"created", "edited", "deleted"} {
			event.Payload["action"] = action
			expressions, snapshot := snapshotTriggerState(event)
			condition, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, "discussion_comment", expressions, snapshot)
			if err != nil || !applicable {
				t.Fatalf("%s (%s): applicable=%v, %v", declaration, action, applicable, err)
			}
			if strings.Contains(declaration, "[edited]") && !strings.Contains(condition, `"`+action+`" == "edited"`) {
				t.Fatalf("condition lost activity filter: %s", condition)
			}
			reason, err := buildkite.TriggerFilterMismatchReason(parsed.Triggers, "discussion_comment", snapshot)
			wantMismatch := strings.Contains(declaration, "[edited]") && action != "edited"
			if err != nil || (reason != "") != wantMismatch {
				t.Fatalf("%s (%s): reason=%q wantMismatch=%v, %v", declaration, action, reason, wantMismatch, err)
			}
		}
	}
	for _, config := range []string{"types: null", "types: [answered]", "branches: [main]", "paths: [src/**]"} {
		parsed, err := workflow.Parse("comment.yml", []byte("on: {discussion_comment: {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err == nil && buildkite.ValidateTriggerConditions(parsed.Triggers) == nil {
			t.Fatalf("accepted %s", config)
		}
	}
}

func TestPluginDiscussionCommentEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"comment.yml": "on: {discussion_comment: {types: [edited]}}\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n        env:\n          COMMENT: ${{ github.event.comment.body }}\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	t.Setenv("BUILDKITE_GITHUB_ACTION", "edited")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/comment.yml", "", "discussion_comment", "buildkite/buildkite-gha/.github/workflows/comment.yml@refs/heads/trunk")
	payload := []byte(`{"action":"edited","discussion":{"id":27,"number":3,"title":"Question"},"comment":{"id":42,"discussion_id":27,"body":"An answer"},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`)
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
		if job.Event.Name != "discussion_comment" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		env := job.Program.Job.Steps[0].Env
		if len(env) != 1 || env[0].Name != "COMMENT" || env[0].Value.Source != "An answer" {
			t.Fatalf("lost comment context: %#v", env)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`"edited"`), []byte(`"answered"`), 1),
		bytes.Replace(payload, []byte(`"id":42`), []byte(`"id":0`), 1),
		bytes.Replace(payload, []byte(`"discussion_id":27`), []byte(`"discussion_id":999`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`foreign/repo`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid discussion comment payload")
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
