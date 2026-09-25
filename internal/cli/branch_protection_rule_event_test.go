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

func TestBranchProtectionRuleDeclarations(t *testing.T) {
	source, err := generatedEventSnapshot("branch_protection_rule")
	if err != nil {
		t.Fatal(err)
	}
	event, err := compiler.ParseEvent(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range []string{"branch_protection_rule", "[push, branch_protection_rule]", "{branch_protection_rule: null}", "{branch_protection_rule: {}}", "{branch_protection_rule: {types: []}}", "{branch_protection_rule: {types: [edited]}}"} {
		parsed, err := workflow.Parse("rule.yml", []byte("on: "+declaration+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"created", "edited", "deleted"} {
			event.Payload["action"] = action
			expressions, snapshot := snapshotTriggerState(event)
			condition, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, "branch_protection_rule", expressions, snapshot)
			if err != nil || !applicable {
				t.Fatalf("%s (%s): applicable=%v, %v", declaration, action, applicable, err)
			}
			if strings.Contains(declaration, "[edited]") && !strings.Contains(condition, `"`+action+`" == "edited"`) {
				t.Fatalf("condition lost activity filter: %s", condition)
			}
			reason, err := buildkite.TriggerFilterMismatchReason(parsed.Triggers, "branch_protection_rule", snapshot)
			wantMismatch := strings.Contains(declaration, "[edited]") && action != "edited"
			if err != nil || (reason != "") != wantMismatch {
				t.Fatalf("%s (%s): reason=%q wantMismatch=%v, %v", declaration, action, reason, wantMismatch, err)
			}
		}
	}
	for _, config := range []string{"types: 'null'", "types: [opened]", "branches-ignore: [main]", "paths: [src/**]"} {
		parsed, err := workflow.Parse("rule.yml", []byte("on: {branch_protection_rule: {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err == nil && buildkite.ValidateTriggerConditions(parsed.Triggers) == nil {
			t.Fatalf("accepted %s", config)
		}
	}
}

func TestPluginBranchProtectionRuleEvent(t *testing.T) {
	requireImporterHost(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{
		"rule.yml": "on: {branch_protection_rule: {types: [edited]}}\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n        env:\n          RULE: ${{ github.event.rule.name }}\n",
	})
	t.Chdir(repository)
	setCLIPluginBuildkiteEnvironment(t, "")
	t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
	t.Setenv("BUILDKITE_BRANCH", "trunk")
	t.Setenv("BUILDKITE_GITHUB_ACTION", "edited")
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/rule.yml", "", "branch_protection_rule", "buildkite/buildkite-gha/.github/workflows/rule.yml@refs/heads/trunk")
	payload := []byte(`{"action":"edited","rule":{"id":27,"repository_id":123,"name":"release/*"},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`)
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
		if job.Event.Name != "branch_protection_rule" || job.Event.Ref != "refs/heads/trunk" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
			t.Fatalf("wrong identity or token demand: %#v", job)
		}
		env := job.Program.Job.Steps[0].Env
		if len(env) != 1 || env[0].Name != "RULE" || env[0].Value.Source != "release/*" {
			t.Fatalf("lost rule context: %#v", env)
		}
		plans++
	}
	if plans != 1 {
		t.Fatalf("plans = %d", plans)
	}
	for _, broken := range [][]byte{nil, []byte(`{}`),
		bytes.Replace(payload, []byte(`"edited"`), []byte(`"opened"`), 1),
		bytes.Replace(payload, []byte(`"id":27`), []byte(`"id":0`), 1),
		bytes.Replace(payload, []byte(`"repository_id":123`), []byte(`"repository_id":999`), 1),
		bytes.Replace(payload, []byte(`"name":"release/*"`), []byte(`"name":null`), 1),
		bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
	} {
		if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
			t.Fatal("accepted invalid branch protection payload")
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
