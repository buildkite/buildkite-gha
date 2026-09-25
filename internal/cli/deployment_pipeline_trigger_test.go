package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestValidateDeploymentEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.yml")
	if err := os.WriteFile(path, []byte("on: [deployment, deployment_status]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"deployment", "deployment_status", "all"} {
		t.Run(event, func(t *testing.T) {
			args := []string{"validate", "--profile", "hosted", "--format", "json", path}
			if event == "all" {
				args = append(args, "--all-events")
			} else {
				args = append(args, "--event", event)
			}
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
				t.Fatalf("validate = %d: %s", code, &stderr)
			}
			var report compatibility.ProcessingReportV3
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Result != "admitted" {
				t.Fatalf("report = %s, error = %v", &stdout, err)
			}
			if event == "all" && (len(report.Evaluations) != 2 || report.Evaluations[0].Event != "deployment" || report.Evaluations[1].Event != "deployment_status") {
				t.Fatalf("missing deployment evaluations: %#v", report.Evaluations)
			}
		})
	}
}

func TestPluginDeploymentPipelineTrigger(t *testing.T) {
	requireImporterHost(t)
	for _, event := range []string{"deployment", "deployment_status"} {
		for _, kind := range []string{"branch", "tag", "sha"} {
			t.Run(event+"/"+kind, func(t *testing.T) {
				repository := writeUploadWorkflowRepository(t, map[string]string{
					"deployment.yml": `name: Deployment
on: [deployment, deployment_status]
permissions: {}
jobs:
  marker:
    if: github.ref_type == 'branch' || github.ref_type == 'tag'
    runs-on: ubuntu-latest
    outputs:
      marker: ${{ steps.emit.outputs.marker }}
    steps:
      - id: emit
        env:
          DEPLOY_ENV: ${{ github.event.deployment.environment }}
          STATUS_ENV: ${{ github.event.deployment_status.environment }}
          STATUS: ${{ github.event.deployment_status.state }}
          URL: ${{ github.event.deployment_status.environment_url }}
          REF_NAME: ${{ github.ref_name }}
          REF_TYPE: ${{ github.ref_type }}
        run: |
          test "$GITHUB_REF" = '${{ github.ref }}'
          test "$GITHUB_REF_NAME" = "$REF_NAME"
          test "$GITHUB_REF_TYPE" = "$REF_TYPE"
          test "$GITHUB_SHA" = '${{ github.event.deployment.sha }}'
          test "$GITHUB_WORKFLOW_REF" = "buildkite/buildkite-gha/.github/workflows/deployment.yml@${GITHUB_REF:-$GITHUB_SHA}"
          echo "marker=$DEPLOY_ENV|$STATUS_ENV|$STATUS|$URL|$GITHUB_REF|$GITHUB_REF_NAME|$GITHUB_REF_TYPE" >> "$GITHUB_OUTPUT"
`,
					"other.yml": "on: push\n",
				})
				t.Chdir(repository)
				t.Setenv(pluginConfigurationEnvironment, `{"experimental-runner-user":false}`)
				setCLIPluginBuildkiteEnvironment(t, "")
				t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", repository)
				t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
				sha := os.Getenv("BUILDKITE_COMMIT")
				ref, rawRef, branch, tag := "refs/heads/deploy/preview", "deploy/preview", "deploy/preview", ""
				refFields := "refs/heads/deploy/preview|deploy/preview|branch"
				switch kind {
				case "tag":
					ref, rawRef, branch, tag = "refs/tags/v2.7.1", "v2.7.1", "v2.7.1", "v2.7.1"
					refFields = "refs/tags/v2.7.1|v2.7.1|tag"
				case "sha":
					ref, rawRef, branch = "", sha, sha
					refFields = "||branch"
				}
				t.Setenv("BUILDKITE_BRANCH", branch)
				t.Setenv("BUILDKITE_TAG", tag)
				t.Setenv("BUILDKITE_PIPELINE_DEFAULT_BRANCH", "main")
				workflowRef := ref
				if workflowRef == "" {
					workflowRef = sha
				}
				setCLIPipelineTriggerEnvironment(t, ".github/workflows/deployment.yml", "Deployment", event, "buildkite/buildkite-gha/.github/workflows/deployment.yml@"+workflowRef)
				status := ""
				marker := "preview||||" + refFields
				if event == "deployment_status" {
					status = `,"deployment_status":{"id":91,"state":"success","environment":"production","environment_url":"https://preview.example/42"}`
					marker = "preview|production|success|https://preview.example/42|" + refFields
				}
				payload := []byte(fmt.Sprintf(`{"repository":{"id":1234,"full_name":"buildkite/buildkite-gha","default_branch":"main"},"deployment":{"id":42,"sha":%q,"ref":%q,"environment":"preview","repository_url":"https://untrusted.invalid/repo"}%s}`, sha, rawRef, status))
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
					if job.Event.Name != event || job.Event.Ref != ref || job.Event.SHA != sha || job.GitHubToken != nil {
						t.Fatalf("deployment identity or token demand changed: %#v", job)
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
						if err != nil || !strings.Contains(string(result), `"marker": "`+marker+`"`) {
							t.Fatalf("result=%s, error=%v, want marker %q", result, err, marker)
						}
					})
					plans++
				}
				if plans != 1 {
					t.Fatalf("plans = %d, want only the selected workflow", plans)
				}
				for name, value := range map[string]string{
					githubWorkflowRefEnvironment: "buildkite/buildkite-gha/.github/workflows/deployment.yml@refs/heads/wrong",
					githubWorkflowSHAEnvironment: strings.Repeat("b", 40),
					githubEventNameEnvironment:   "push",
					"BUILDKITE_BRANCH":           "main", "BUILDKITE_TAG": "wrong",
				} {
					t.Run("mismatch/"+name, func(t *testing.T) {
						t.Setenv(name, value)
						if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{webhook: payload}) == 0 {
							t.Fatal("accepted mismatched identity")
						}
					})
				}
				t.Setenv("BUILDKITE_SOURCE", "ui")
				stderr.Reset()
				if run([]string{"plugin"}, &stdout, &stderr, "dev", &cliCaptureRunner{}) == 0 || !strings.Contains(stderr.String(), "original buildkite:webhook") {
					t.Fatalf("missing payload must fail explicitly: %s", &stderr)
				}
			})
		}
	}
}

func TestDeploymentEventValidation(t *testing.T) {
	sha := strings.Repeat("a", 40)
	source := fmt.Sprintf(`{"provider":"github","event":"deployment_status","repository":{"owner":"acme","name":"widgets"},"ref":"","sha":%q,"actor":"octocat","payload":{"repository":{"id":1234,"full_name":"acme/widgets"},"deployment":{"id":42,"sha":%q,"ref":%q},"deployment_status":{"id":91,"state":"success"}}}`, sha, sha, sha)
	for _, state := range []string{"error", "failure", "pending", "queued", "in_progress", "success", "waiting"} {
		if _, err := compiler.ParseEvent([]byte(strings.Replace(source, `"success"`, fmt.Sprintf("%q", state), 1))); err != nil {
			t.Fatalf("state %s: %v", state, err)
		}
	}
	for name, change := range map[string][2]string{
		"inactive":                  {`"success"`, `"inactive"`},
		"unknown state":             {`"success"`, `"ready"`},
		"malformed state":           {`"success"`, `{}`},
		"other repository":          {`"acme/widgets"`, `"other/repo"`},
		"bad repository id":         {`"id":1234`, `"id":"1234"`},
		"bad status id":             {`"id":91`, `"id":0`},
		"bad deployment id":         {`"id":42`, `"id":null`},
		"wrong commit":              {`"id":42,"sha":"` + sha + `"`, `"id":42,"sha":"` + strings.Repeat("b", 40) + `"`},
		"missing branch identity":   {`"ref":"` + sha + `"`, `"ref":"staging"`},
		"unexpected status payload": {`"event":"deployment_status"`, `"event":"deployment"`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := compiler.ParseEvent([]byte(strings.Replace(source, change[0], change[1], 1))); err == nil {
				t.Fatal("accepted invalid deployment event")
			}
		})
	}
}
