package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestLabelDeclarationsAndSnapshot(t *testing.T) {
	source, err := generatedEventSnapshot("label")
	if err != nil {
		t.Fatal(err)
	}
	event, err := compiler.ParseEvent(source)
	if err != nil {
		t.Fatal(err)
	}
	expressions, snapshot := snapshotTriggerState(event)
	expressions.EventPredicate = "true"
	for _, declaration := range []string{"label", "[push, label]", "{label: null}", "{label: {}}", "{label: {types: []}}", "{label: {types: [created, edited]}}"} {
		parsed, err := workflow.Parse("label.yml", []byte("on: "+declaration+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, applicable, err := buildkite.TranslateEventTriggerCondition(parsed.Triggers, "label", expressions, snapshot); err != nil || !applicable {
			t.Fatalf("%s: %v, %v", declaration, applicable, err)
		}
	}
	if reason, err := buildkite.TriggerFilterMismatchReason([]workflow.Trigger{{Event: "label", Types: []string{"edited"}}}, "label", snapshot); err != nil || reason == "" {
		t.Fatalf("created matched edited: %q, %v", reason, err)
	}
	for _, config := range []string{"types: null", "types: [labeled]", "branches: [main]", "paths: [src/**]"} {
		parsed, err := workflow.Parse("label.yml", []byte("on: {label: {"+config+"}}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
		if err == nil && buildkite.ValidateTriggerConditions(parsed.Triggers) == nil {
			t.Fatalf("accepted %s", config)
		}
	}
	for _, change := range [][2]string{
		{`"ref":"refs/heads/main"`, `"ref":"refs/tags/main"`},
		{`"id":1`, `"id":null`},
		{`"action":"created"`, `"action":"labeled"`},
	} {
		if _, err := compiler.ParseEvent(bytes.Replace(source, []byte(change[0]), []byte(change[1]), 1)); err == nil {
			t.Fatalf("accepted label snapshot mutation %v", change)
		}
	}
}

func TestPluginLabelEvent(t *testing.T) {
	requireImporterHost(t)
	for _, action := range []string{"created", "edited", "deleted"} {
		t.Run(action, func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{
				"label.yml": "name: Labels\non: {label: {types: []}}\npermissions: {}\njobs:\n  marker:\n    runs-on: ubuntu-latest\n    outputs:\n      marker: ${{ steps.emit.outputs.marker }}\n    steps:\n      - id: emit\n        env:\n          LABEL: ${{ github.event.label.name }}\n          ACTION: ${{ github.event.action }}\n        run: |\n          grep -q 'triage' \"$GITHUB_EVENT_PATH\"\n          echo \"marker=$ACTION|$LABEL|$GITHUB_REF\" >> \"$GITHUB_OUTPUT\"\n",
			})
			t.Chdir(repository)
			setCLIPluginBuildkiteEnvironment(t, "")
			t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
			t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_BRANCH", "trunk")
			t.Setenv("BUILDKITE_GITHUB_ACTION", action)
			setCLIPipelineTriggerEnvironment(t, ".github/workflows/label.yml", "Labels", "label", "buildkite/buildkite-gha/.github/workflows/label.yml@refs/heads/trunk")
			payload := []byte(fmt.Sprintf(`{"action":%q,"label":{"id":27,"name":"triage"},"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`, action))
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
				if job.Event.Name != "label" || job.Event.Ref != "refs/heads/trunk" || job.GitHubToken != nil {
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
					marker := action + "|triage|refs/heads/trunk"
					if err != nil || !bytes.Contains(result, []byte(marker)) {
						t.Fatalf("missing marker %q: %s (%v)", marker, result, err)
					}
					t.Log(marker)
				})
				plans++
			}
			if plans != 1 {
				t.Fatalf("plans = %d", plans)
			}
			for key, value := range map[string]string{
				"BUILDKITE_PULL_REQUEST":     "42",
				"BUILDKITE_TAG":              "trunk",
				"BUILDKITE_BRANCH":           "untrusted",
				"BUILDKITE_GITHUB_ACTION":    "labeled",
				githubWorkflowSHAEnvironment: "",
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
				"issue labeling": bytes.Replace(payload, []byte(fmt.Sprintf(`"action":%q`, action)), []byte(`"action":"labeled"`), 1),
				"foreign":        bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
				"label id":       bytes.Replace(payload, []byte(`"id":27`), []byte(`"id":null`), 1),
			} {
				t.Run(name, func(t *testing.T) {
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
						t.Fatal("accepted invalid label payload")
					}
				})
			}
		})
	}
}
