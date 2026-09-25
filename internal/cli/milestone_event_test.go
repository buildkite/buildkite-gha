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

func TestMilestoneDeclarations(t *testing.T) {
	source, err := generatedEventSnapshot("milestone")
	if err != nil {
		t.Fatal(err)
	}
	event, err := compiler.ParseEvent(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range []string{"milestone", "[push, milestone]", "{milestone: null}", "{milestone: {}}", "{milestone: {types: []}}", "{milestone: {types: [closed]}}"} {
		parsed, err := workflow.Parse("milestone.yml", []byte("on: "+declaration+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"created", "closed", "opened", "edited", "deleted"} {
			event.Payload["action"] = action
			expressions, snapshot := snapshotTriggerState(event)
			condition, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, "milestone", expressions, snapshot)
			if err != nil || !applicable {
				t.Fatalf("%s (%s): applicable=%v, %v", declaration, action, applicable, err)
			}
			if strings.Contains(declaration, "[closed]") && !strings.Contains(condition, `"`+action+`" == "closed"`) {
				t.Fatalf("condition lost activity filter: %s", condition)
			}
			reason, err := buildkite.TriggerFilterMismatchReason(parsed.Triggers, "milestone", snapshot)
			wantMismatch := strings.Contains(declaration, "[closed]") && action != "closed"
			if err != nil || (reason != "") != wantMismatch {
				t.Fatalf("%s (%s): reason=%q wantMismatch=%v, %v", declaration, action, reason, wantMismatch, err)
			}
		}
	}
	for _, config := range []string{"types: 'null'", "types: [milestoned]", "branches-ignore: [main]", "paths: [src/**]"} {
		parsed, err := workflow.Parse("milestone.yml", []byte("on: {milestone: {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err == nil && buildkite.ValidateTriggerConditions(parsed.Triggers) == nil {
			t.Fatalf("accepted %s", config)
		}
	}
}

func TestPluginMilestoneEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"milestone.yml": "on: {milestone: {types: [closed]}}\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n        env:\n          TITLE: ${{ github.event.milestone.title }}\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	t.Setenv("BUILDKITE_GITHUB_ACTION", "closed")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/milestone.yml", "", "milestone", "buildkite/buildkite-gha/.github/workflows/milestone.yml@refs/heads/trunk")
	payload := []byte(`{"action":"closed","milestone":{"id":27,"number":3,"title":"Release"},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`)
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
		if job.Event.Name != "milestone" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		env := job.Program.Job.Steps[0].Env
		if len(env) != 1 || env[0].Name != "TITLE" || env[0].Value.Source != "Release" {
			t.Fatalf("lost milestone context: %#v", env)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`"closed"`), []byte(`"milestoned"`), 1),
		bytes.Replace(payload, []byte(`"id":27`), []byte(`"id":0`), 1),
		bytes.Replace(payload, []byte(`"title":"Release"`), []byte(`"title":null`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid milestone payload")
		}
	}
	for key, value := range map[string]string{pipelineTriggerWorkflowPathEnvironment: "", githubWorkflowRefEnvironment: "", githubWorkflowSHAEnvironment: "", "BUILDKITE_GITHUB_ACTION": "opened"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
				t.Fatal("accepted missing or conflicting identity")
			}
		})
	}
}
