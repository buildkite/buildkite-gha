package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestPluginMergeGroupPipelineTrigger(t *testing.T) {
	requireImporterHost(t)
	for _, test := range []struct{ declaration, action, reason string }{
		{"merge_group", "checks_requested", ""},
		{"{merge_group: {types: [checks_requested], branches: [stable]}}", "checks_requested", ""},
		{"{merge_group: {types: [destroyed], branches: [stable]}}", "destroyed", "merged"},
		{"{merge_group: {types: [checks_requested, destroyed], branches: [stable]}}", "checks_requested", ""},
		{"{merge_group: {types: [checks_requested, destroyed], branches: [stable]}}", "destroyed", "merged"},
		// Selection must not invent a restriction on the destruction reason.
		{"{merge_group: {types: [destroyed], branches-ignore: [main]}}", "destroyed", "other-reason"},
	} {
		t.Run(test.declaration+"/"+test.action, func(t *testing.T) {
			repository := writeUploadWorkflowRepository(t, map[string]string{
				"queue.yml": "name: Queue\non: " + test.declaration + `
permissions: {contents: read}
jobs:
  marker:
    runs-on: ubuntu-latest
    outputs:
      marker: ${{ steps.emit.outputs.marker }}
    steps:
      - id: emit
        env:
          ACTION: ${{ github.event.action }}
          REASON: ${{ github.event.reason }}
          BASE_SHA: ${{ github.event.merge_group.base_sha }}
          PRESERVED: ${{ github.event.extra.preserved }}
        run: |
          test "$PRESERVED" = original-payload-marker
          grep -q original-payload-marker "$GITHUB_EVENT_PATH"
          printf 'marker=%s|%s|%s|%s|%s|%s|%s\n' "$ACTION" "$REASON" "$BASE_SHA" "$GITHUB_REF" "$GITHUB_SHA" "$GITHUB_WORKFLOW_REF" "$GITHUB_WORKFLOW_SHA" >> "$GITHUB_OUTPUT"
`,
			})
			t.Chdir(repository)
			t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
			setCLIPluginBuildkiteEnvironment(t, "")
			t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_BRANCH", "gh-readonly-queue/stable/pr-42-deadbeef")
			t.Setenv("BUILDKITE_MERGE_QUEUE_BASE_BRANCH", "stable")
			t.Setenv("BUILDKITE_MERGE_QUEUE_BASE_COMMIT", strings.Repeat("c", 40))
			t.Setenv("BUILDKITE_GITHUB_ACTION", test.action)
			ref := "refs/heads/gh-readonly-queue/stable/pr-42-deadbeef"
			commitUploadWorkflows(t, repository)
			git := func(args ...string) string {
				t.Helper()
				output, err := exec.Command("git", args...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			sha := git("rev-parse", "HEAD")
			git("update-ref", ref, sha)
			git("checkout", "--detach", sha)
			git("update-ref", "-d", ref)
			if err := exec.Command("git", "show-ref", "--verify", ref).Run(); err == nil {
				t.Fatal("queue ref still exists")
			}
			t.Setenv("BUILDKITE_COMMIT", sha)
			setCLIPipelineTriggerEnvironment(t, ".github/workflows/queue.yml", "Queue", "merge_group", "buildkite/buildkite-gha/.github/workflows/queue.yml@"+ref)
			payload := []byte(fmt.Sprintf(`{"action":%q,"reason":%q,"extra":{"preserved":"original-payload-marker"},"repository":{"full_name":"buildkite/buildkite-gha"},"merge_group":{"head_ref":%q,"head_sha":%q,"base_ref":"refs/heads/stable","base_sha":%q}}`, test.action, test.reason, ref, os.Getenv("BUILDKITE_COMMIT"), strings.Repeat("c", 40)))
			var stdout, stderr bytes.Buffer
			runner := &cliCaptureRunner{webhook: payload}
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
				if job.Event.Name != "merge_group" || job.Event.Ref != ref || job.Event.SHA != sha || job.GitHubToken != nil {
					t.Fatalf("queue identity or token demand changed: %#v", job)
				}
				t.Run("execute marker", func(t *testing.T) {
					setCLIJobIdentity(t, job, transport.Digest(data))
					planPath := filepath.Join(t.TempDir(), "plan.json")
					if err := os.WriteFile(planPath, data, 0o600); err != nil {
						t.Fatal(err)
					}
					resultPath := filepath.Join(t.TempDir(), "result.json")
					if code := run([]string{"run-job", "--plan", planPath, "--artifact-producer", cliTestJobID, "--result", resultPath}, &stdout, &stderr, "dev", &cliCaptureRunner{dataByPath: runner.uploaded}); code != 0 {
						t.Fatalf("run-job = %d: %s", code, &stderr)
					}
					result, err := os.ReadFile(resultPath)
					marker := strings.Join([]string{test.action, test.reason, strings.Repeat("c", 40), ref, os.Getenv("BUILDKITE_COMMIT"), "buildkite/buildkite-gha/.github/workflows/queue.yml@" + ref, os.Getenv("BUILDKITE_COMMIT")}, "|")
					if err != nil || !strings.Contains(string(result), `"marker": "`+marker+`"`) {
						t.Fatalf("marker=%s error=%v", result, err)
					}
				})
				plans++
			}
			if plans != 1 {
				t.Fatalf("plans=%d", plans)
			}
			otherAction := "destroyed"
			if test.action == "destroyed" {
				otherAction = "checks_requested"
			}
			for name, value := range map[string]string{
				githubWorkflowRefEnvironment:        "buildkite/buildkite-gha/.github/workflows/queue.yml@refs/heads/wrong",
				githubWorkflowSHAEnvironment:        strings.Repeat("b", 40),
				"BUILDKITE_BRANCH":                  "wrong",
				"BUILDKITE_MERGE_QUEUE_BASE_BRANCH": "main",
				"BUILDKITE_MERGE_QUEUE_BASE_COMMIT": strings.Repeat("d", 40),
				"BUILDKITE_GITHUB_ACTION":           otherAction,
			} {
				t.Run(name, func(t *testing.T) {
					t.Setenv(name, value)
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
						t.Fatal("accepted mismatched queue identity")
					}
				})
			}
			for _, key := range []string{githubWorkflowRefEnvironment, githubWorkflowSHAEnvironment, "BUILDKITE_MERGE_QUEUE_BASE_BRANCH", "BUILDKITE_MERGE_QUEUE_BASE_COMMIT", "BUILDKITE_GITHUB_ACTION"} {
				t.Run("missing "+key, func(t *testing.T) {
					t.Setenv(key, "")
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
						t.Fatal("accepted missing queue identity")
					}
				})
			}
			for name, broken := range map[string][]byte{
				"repository":     bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
				"action":         bytes.Replace(payload, []byte(test.action), []byte(otherAction), 1),
				"unknown action": bytes.Replace(payload, []byte(test.action), []byte("unknown"), 1),
				"head ref":       bytes.Replace(payload, []byte(ref), []byte("refs/heads/other"), 1),
				"head sha":       bytes.Replace(payload, []byte(os.Getenv("BUILDKITE_COMMIT")), []byte(strings.Repeat("b", 40)), 1),
			} {
				t.Run(name, func(t *testing.T) {
					if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
						t.Fatal("accepted invalid queue payload")
					}
				})
			}
			stderr.Reset()
			if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{}) == 0 || !strings.Contains(stderr.String(), "original buildkite:webhook") {
				t.Fatalf("missing queue payload must fail explicitly: %s", &stderr)
			}
		})
	}
}
