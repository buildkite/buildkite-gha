package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func reviewWebhook(event, action, sha string) []byte {
	object := `"review":{"id":901,"state":"approved","commit_id":"` + strings.Repeat("c", 40) + `"}`
	if event == "pull_request_review_comment" {
		object = `"comment":{"id":902,"path":"src/example.go","body":"inline comment"}`
	}
	return []byte(fmt.Sprintf(`{"action":%q,%s,"repository":{"id":123,"full_name":"buildkite/buildkite-gha"},"sender":{"login":"reviewer"},"pull_request":{"number":42,"head":{"sha":%q,"ref":"feature/review","repo":{"full_name":"buildkite/buildkite-gha"}},"base":{"ref":"release","repo":{"full_name":"buildkite/buildkite-gha"}},"mergeable":false,"merge_commit_sha":%q}}`, action, object, sha, strings.Repeat("b", 40)))
}

func setReviewEnvironment(t *testing.T, event, action string) {
	t.Helper()
	setCLIPluginBuildkiteEnvironment(t, "review-pipeline-trigger")
	t.Setenv("BUILDKITE_PULL_REQUEST", "42")
	t.Setenv("BUILDKITE_BRANCH", "feature/review")
	t.Setenv("BUILDKITE_PULL_REQUEST_BASE_BRANCH", "release")
	t.Setenv("BUILDKITE_GITHUB_ACTION", action)
	setCLIPipelineTriggerEnvironment(t, ".github/workflows/selected.yml", "Selected", event, "buildkite/buildkite-gha/.github/workflows/selected.yml@refs/pull/42/merge")
}

func TestPluginReviewEvents(t *testing.T) {
	requireImporterHost(t)
	for _, event := range []string{"pull_request_review", "pull_request_review_comment"} {
		t.Run(event, func(t *testing.T) {
			action := "submitted"
			if event == "pull_request_review_comment" {
				action = "created"
			}
			source := "name: Selected\non: " + event + "\njobs:\n  selected:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ toJSON(github.event) }}'\n"
			repository := writeUploadWorkflowRepository(t, map[string]string{"selected.yml": source, "other.yml": source})
			t.Chdir(repository)
			t.Setenv(pluginConfigurationEnvironment, `{}`)
			setReviewEnvironment(t, event, action)
			t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			runner := &cliCaptureRunner{webhook: reviewWebhook(event, action, os.Getenv("BUILDKITE_COMMIT"))}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"plugin"}, &stdout, &stderr, "dev", runner); code != 0 {
				t.Fatalf("plugin = %d: %s %s", code, &stdout, &stderr)
			}
			plans, payloads := 0, 0
			for path, data := range runner.uploaded {
				if strings.HasPrefix(path, ".buildkite-gha/plans/") && strings.HasSuffix(path, ".json") {
					job, err := plan.Decode(data)
					if err != nil {
						t.Fatal(err)
					}
					if job.Event.Name != event || job.Event.Ref != "refs/pull/42/merge" || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.Event.Actor != "reviewer" || !job.Event.PayloadArtifact {
						t.Fatalf("incoherent event: %#v", job.Event)
					}
					plans++
				}
				if strings.HasPrefix(path, ".buildkite-gha/events/") && strings.HasSuffix(path, ".json") {
					var got, want any
					if err := json.Unmarshal(data, &got); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(runner.webhook, &want); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("review payload changed: %s", data)
					}
					payloads++
				}
			}
			if plans != 1 || payloads != 1 {
				t.Fatalf("plans=%d payloads=%d; want only selected workflow", plans, payloads)
			}
			t.Setenv("BUILDKITE_SOURCE", "ui")
			_, _, err := loadEffectiveEventSource(t.Context(), "", transport.Agent{Runner: &cliCaptureRunner{}})
			if err == nil || !strings.Contains(err.Error(), "original buildkite:webhook") {
				t.Fatalf("missing rebuild payload error = %v", err)
			}
		})
	}
}

func TestReviewTriggerActivities(t *testing.T) {
	for event, activities := range map[string][]string{
		"pull_request_review":         {"submitted", "edited", "dismissed"},
		"pull_request_review_comment": {"created", "edited", "deleted"},
	} {
		for _, declaration := range []string{event, "[push, " + event + "]", "{" + event + ": null}", "{" + event + ": {}}", "{" + event + ": {types: []}}", "{" + event + ": {types: [edited]}}", "{" + event + ": {types: edited}}"} {
			t.Run(declaration, func(t *testing.T) {
				parsed, err := workflow.Parse("review.yml", []byte("on: "+declaration+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"))
				if err != nil {
					t.Fatal(err)
				}
				for _, action := range activities {
					effective, err := newEffectiveEvent([]byte(fmt.Sprintf(`{"provider":"github","event":%q,"repository":{"owner":"buildkite","name":"buildkite-gha"},"ref":"refs/pull/42/merge","sha":%q,"actor":"reviewer","payload":{"action":%q}}`, event, strings.Repeat("a", 40), action)), effectiveEventFromPath)
					if err != nil {
						t.Fatal(err)
					}
					selection, err := selectWorkflowTrigger(parsed.Triggers, effective)
					if err != nil || !selection.Applicable {
						t.Fatalf("selection=%#v error=%v", selection, err)
					}
					wantMismatch := strings.Contains(declaration, "edited") && action != "edited"
					if (selection.AnnotationReason != "") != wantMismatch {
						t.Fatalf("%s: selection=%#v", action, selection)
					}
				}
			})
		}
	}
}

func TestReviewWebhookIdentity(t *testing.T) {
	for _, event := range []string{"pull_request_review", "pull_request_review_comment"} {
		t.Run(event, func(t *testing.T) {
			setReviewEnvironment(t, event, "edited")
			sha := os.Getenv("BUILDKITE_COMMIT")
			payload := reviewWebhook(event, "edited", sha)
			source, err := buildkiteWebhookEventSource(os.Getenv, payload)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := compiler.ParseEvent(source)
			if err != nil || parsed.Event != event || parsed.SHA != sha || parsed.Ref != "refs/pull/42/merge" {
				t.Fatalf("event=%#v error=%v", parsed, err)
			}
			for name, broken := range map[string][]byte{
				"fork":            bytes.Replace(payload, []byte(`"repo":{"full_name":"buildkite/buildkite-gha"}`), []byte(`"repo":{"full_name":"other/fork"}`), 1),
				"base repository": bytes.Replace(payload, []byte(`"base":{"ref":"release","repo":{"full_name":"buildkite/buildkite-gha"}}`), []byte(`"base":{"ref":"release","repo":{"full_name":"other/base"}}`), 1),
				"repository":      bytes.Replace(payload, []byte(`"repository":{"id":123,"full_name":"buildkite/buildkite-gha"}`), []byte(`"repository":{"id":123,"full_name":"other/repository"}`), 1),
				"number":          bytes.Replace(payload, []byte(`"number":42`), []byte(`"number":43`), 1),
				"head sha":        bytes.Replace(payload, []byte(sha), []byte(strings.Repeat("d", 40)), 1),
				"action":          bytes.Replace(payload, []byte(`"action":"edited"`), []byte(`"action":"opened"`), 1),
				"object":          bytes.Replace(payload, []byte(`"id":90`), []byte(`"wrong":90`), 1),
			} {
				if _, err := buildkiteWebhookEventSource(os.Getenv, broken); err == nil {
					t.Errorf("accepted %s mismatch", name)
				}
			}
		})
	}
}

func TestReviewFiltersFailClosed(t *testing.T) {
	for _, event := range []string{"pull_request_review", "pull_request_review_comment"} {
		for name, trigger := range map[string]workflow.Trigger{
			"branch":       {Event: event, Branches: []string{"release"}},
			"path":         {Event: event, Paths: []string{"src/**"}},
			"tag":          {Event: event, TagsIgnore: []string{"v*"}},
			"unknown type": {Event: event, Types: []string{"approved"}},
		} {
			if _, err := buildkitepipeline.TranslateTriggerCondition([]workflow.Trigger{trigger}); err == nil {
				t.Errorf("%s accepted %s", event, name)
			}
		}
		for _, predicate := range []string{"push", "pull_request"} {
			if !strings.Contains(buildkitepipeline.LiveEventPredicate(predicate), `build.env("BUILDKITE_GITHUB_EVENT") != "`+event+`"`) {
				t.Errorf("%s compatibility fallback does not exclude %s", predicate, event)
			}
		}
	}
}
