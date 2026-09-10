package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestRefLifecycleSnapshots(t *testing.T) {
	for _, event := range []string{"create", "delete"} {
		source, err := generatedEventSnapshot(event)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := compiler.ParseEvent(source)
		if err != nil {
			t.Fatal(err)
		}
		wantRef := "refs/heads/main"
		if event == "create" {
			wantRef = "refs/heads/feature/example"
		}
		if parsed.Ref != wantRef || parsed.Payload["ref"] != "feature/example" {
			t.Fatalf("wrong generated identity: %#v", parsed)
		}
		for _, change := range [][2]string{
			{`"ref":"` + wantRef + `"`, `"ref":"refs/heads/wrong"`},
			{`"payload":{`, `"payload":{"action":"created",`},
			{`"id":1`, `"id":null`},
		} {
			if _, err := compiler.ParseEvent(bytes.Replace(source, []byte(change[0]), []byte(change[1]), 1)); err == nil {
				t.Fatalf("accepted %s snapshot mutation %v", event, change)
			}
		}
	}
}

func TestPluginRefLifecycle(t *testing.T) {
	requireImporterHost(t)
	for _, event := range []string{"create", "delete"} {
		for _, kind := range []string{"branch", "tag"} {
			t.Run(event+"/"+kind, func(t *testing.T) {
				repository := writeUploadWorkflowRepository(t, map[string]string{
					"lifecycle.yml": `name: Lifecycle
on: [create, delete]
permissions: {}
jobs:
  marker:
    runs-on: ubuntu-latest
    outputs:
      marker: ${{ steps.emit.outputs.marker }}
    steps:
      - id: emit
        env:
          RAW_REF: ${{ github.event.ref }}
          RAW_TYPE: ${{ github.event.ref_type }}
        run: |
          test -r "$GITHUB_EVENT_PATH"
          grep -q 'feature/new' "$GITHUB_EVENT_PATH"
          echo "marker=$GITHUB_EVENT_NAME|$GITHUB_REF|$RAW_REF|$RAW_TYPE" >> "$GITHUB_OUTPUT"
`,
					"other.yml": "on: push\n",
				})
				t.Chdir(repository)
				t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
				setCLIPluginBuildkiteEnvironment(t, "")
				t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
				t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
				branch, tag, ref := "feature/new", "", "refs/heads/feature/new"
				if kind == "tag" {
					tag, ref = "feature/new", "refs/tags/feature/new"
				}
				if event == "delete" {
					branch, tag, ref = "trunk", "", "refs/heads/trunk"
				}
				t.Setenv("BUILDKITE_BRANCH", branch)
				t.Setenv("BUILDKITE_TAG", tag)
				t.Setenv("BUILDKITE_PIPELINE_DEFAULT_BRANCH", "stale")
				setCLIPipelineTriggerEnvironment(t, ".github/workflows/lifecycle.yml", "Lifecycle", event, "buildkite/buildkite-gha/.github/workflows/lifecycle.yml@"+ref)
				payload := []byte(fmt.Sprintf(`{"ref":"feature/new","ref_type":%q,"repository":{"id":123,"full_name":"buildkite/buildkite-gha","default_branch":"stale"}}`, kind))
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
					if job.Event.Name != event || job.Event.Ref != ref || job.Event.SHA != os.Getenv("BUILDKITE_COMMIT") || job.GitHubToken != nil {
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
						marker := event + "|" + ref + "|feature/new|" + kind
						if err != nil || !bytes.Contains(result, []byte(marker)) {
							t.Fatalf("marker %q missing: %s (%v)", marker, result, err)
						}
						t.Log(marker)
					})
					plans++
				}
				if plans != 1 {
					t.Fatalf("plans = %d, want only selected workflow", plans)
				}
				for name, broken := range map[string][]byte{
					"missing": nil, "malformed": []byte(`[]`),
					"foreign":              bytes.Replace(payload, []byte(`buildkite/buildkite-gha`), []byte(`other/repo`), 1),
					"ref":                  bytes.Replace(payload, []byte(`"feature/new"`), []byte(`null`), 1),
					"repository lifecycle": bytes.Replace(payload, []byte(fmt.Sprintf(`"ref_type":%q`, kind)), []byte(`"ref_type":"repository"`), 1),
				} {
					t.Run(name, func(t *testing.T) {
						if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: broken}) == 0 {
							t.Fatal("accepted invalid original payload")
						}
					})
				}
			})
		}
	}
}
