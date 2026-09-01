package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
	executionprogram "github.com/buildkite/buildkite-gha/internal/program"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/telemetry"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func TestRunJobPublishesFailureWhenExplicitRuntimeMiseIsInvalid(t *testing.T) {
	job := cliRunJobPlan()
	job.Schema = plan.Schema
	requiresMise := true
	job.RequiresMise = &requiresMise
	job.Actions = []plan.ActionLock{{
		ID: "a-0000000000000001", Source: "workspace", Path: "actions/build",
		SourceDigest: "sha256:" + strings.Repeat("a", 64),
	}}
	job.Steps = []plan.Step{{
		ID: "local", Kind: "uses", Uses: "./actions/build",
		Action: &plan.ActionSelector{Lock: "a-0000000000000001"},
	}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	t.Setenv("BUILDKITE_GHA_MISE", filepath.Join(t.TempDir(), "missing-mise"))
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "prepare action runtime: resolve runtime mise executable") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "+++ :warning: Prepare GitHub Actions job failed\n~~~ :package: Publish GitHub Actions result\n") {
		t.Fatalf("stdout = %q, want visible runner setup failure before collapsed publication", stdout.String())
	}
	if manifest := publishedCLIManifest(t, runner, job, planDigest); manifest.Result != "failure" {
		t.Fatalf("published result = %q, want failure", manifest.Result)
	}
}

func TestRunJobMiseRequirementUsesCompilerDecisionAndFailsClosed(t *testing.T) {
	no := false
	yes := true
	actionJob := func(requiresMise *bool) plan.Job {
		return plan.Job{
			Actions:      []plan.ActionLock{{ID: "a-0000000000000001"}},
			Steps:        []plan.Step{{Kind: "uses", Action: &plan.ActionSelector{Lock: "a-0000000000000001"}}},
			RequiresMise: requiresMise,
		}
	}
	cacheJob := actionJob(&yes)
	cacheJob.Actions[0].Source = "github"
	cacheJob.Actions[0].Repository = "actions/cache"
	for _, test := range []struct {
		name string
		job  plan.Job
		want bool
	}{
		{name: "shell plan does not resolve mise", job: plan.Job{Steps: []plan.Step{{Kind: "run"}}}},
		{name: "native plan does not resolve mise", job: actionJob(&no)},
		{name: "JavaScript plan resolves mise", job: actionJob(&yes), want: true},
		{name: "cache client plan resolves mise", job: cacheJob, want: true},
		{name: "missing action decision resolves mise", job: actionJob(nil), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.job.NeedsMise(); got != test.want {
				t.Fatalf("NeedsMise() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRunJobCallGuardSkipsActionJobBeforePreparingRuntimeMise(t *testing.T) {
	job := cliRunJobPlan()
	job.Schema = plan.Schema
	job.Condition = "always()"
	job.CallGuards = []plan.CallGuard{{Condition: "false"}}
	job.Actions = []plan.ActionLock{{
		ID: "a-0000000000000001", Source: "workspace", Path: "actions/build",
		SourceDigest: "sha256:" + strings.Repeat("a", 64),
	}}
	job.Steps = []plan.Step{{
		ID: "local", Kind: "uses", Uses: "./actions/build",
		Action: &plan.ActionSelector{Lock: "a-0000000000000001"},
	}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	t.Setenv("BUILDKITE_GHA_MISE", filepath.Join(t.TempDir(), "missing-mise"))
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "prepare action runtime") {
		t.Fatalf("skipped job prepared action runtime: %q", stderr.String())
	}
	if manifest := publishedCLIManifest(t, runner, job, planDigest); manifest.Result != "skipped" {
		t.Fatalf("published result = %q, want skipped", manifest.Result)
	}
}

func TestRunJobExecutesBoundPlanAndWritesResult(t *testing.T) {
	workspace := t.TempDir()
	workflowSource := []byte("name: cli fixture\n")
	workflowPath := filepath.Join(workspace, "workflow.yml")
	if err := os.WriteFile(workflowPath, workflowSource, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(workflowSource)
	requiresMise := false
	job := plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "dev", DistributionDigest: "sha256:" + strings.Repeat("9", 64)},
		Runtime:      &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()},
		Workflow:     plan.Workflow{Path: "workflow.yml", Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "cli"},
		Event:        plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:       plan.Target{StepKey: "gha-cli", Queue: "ubuntu-latest"},
		Outputs:      map[string]string{"result": "${{ steps.produce.outputs.result }}"},
		Steps:        []plan.Step{{ID: "produce", Kind: "run", Command: `echo "result=cli-ok" >> "$GITHUB_OUTPUT"`}},
		RequiresMise: &requiresMise,
	}
	attachCLIExecutionProgram(&job)
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(workspace, "plan.json")
	resultPath := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest := sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", job.Target.StepKey)
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", job.Target.Queue)
	t.Chdir(workspace)

	var stdout, stderr bytes.Buffer
	runner := &cliCaptureRunner{}
	if code := run([]string{"run-job", "--plan", planPath, "--result", resultPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"result": "cli-ok"`) {
		t.Fatalf("result = %s", result)
	}
	artifactDigest := transport.Digest(encoded)
	artifactPath, err := buildkitepipeline.PlanPath(artifactDigest)
	if err != nil {
		t.Fatal(err)
	}
	artifactRunner := &cliCaptureRunner{dataByPath: map[string][]byte{artifactPath: encoded}}
	artifactResultPath := filepath.Join(workspace, "artifact-result.json")
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+strings.Repeat("0", 64))
	stdout.Reset()
	stderr.Reset()
	failedArtifactRunner := &cliCaptureRunner{failAt: 1}
	if code := run([]string{"run-job", "--plan-digest", artifactDigest, "--plan-producer", "gha-importer", "--result", artifactResultPath}, &stdout, &stderr, "dev", failedArtifactRunner); code != 1 || !strings.Contains(stderr.String(), "download plan") {
		t.Fatalf("failed artifact Run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "~~~ :package: Prepare GitHub Actions job\n^^^ +++\n") {
		t.Fatalf("failed artifact stdout = %q, want expanded preparation group", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan-digest", artifactDigest, "--plan-producer", "gha-importer", "--result", artifactResultPath}, &stdout, &stderr, "dev", artifactRunner); code != 0 {
		t.Fatalf("artifact Run() code = %d, stderr = %q", code, stderr.String())
	}
	if len(artifactRunner.commands) == 0 || !slices.Equal(artifactRunner.commands[0].args[:3], []string{"artifact", "download", artifactPath}) || artifactRunner.commands[0].args[4] != "--step" || artifactRunner.commands[0].args[5] != "gha-importer" {
		t.Fatalf("plan artifact download = %#v", artifactRunner.commands)
	}
	if destination := artifactRunner.commands[0].args[3]; destination == workspace {
		t.Fatalf("plan downloaded into workspace %q", destination)
	} else if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("private plan directory still exists: %v", err)
	}
	tamperedRunner := &cliCaptureRunner{dataByPath: map[string][]byte{artifactPath: []byte("tampered")}}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan-digest", artifactDigest, "--plan-producer", "gha-importer"}, &stdout, &stderr, "dev", tamperedRunner); code != 1 || !strings.Contains(stderr.String(), "does not match expected digest") {
		t.Fatalf("tampered artifact Run() code = %d, stderr = %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+strings.Repeat("0", 64))
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not match expected digest") {
		t.Fatalf("Run() code = %d, stderr = %q, want digest mismatch", code, stderr.String())
	}
	job.Runtime.DistributionDigest = "sha256:" + strings.Repeat("0", 64)
	encoded, err = plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest = sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "runtime distribution digest") {
		t.Fatalf("Run() code = %d, stderr = %q, want runtime digest mismatch", code, stderr.String())
	}
	job.Runtime.DistributionDigest = cliTestRuntimeDigest()
	job.Compiler.Version = "0.0.0-other"
	encoded, err = plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	planDigest = sha256.Sum256(encoded)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "sha256:"+hex.EncodeToString(planDigest[:]))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "does not match runtime version") {
		t.Fatalf("Run() code = %d, stderr = %q, want version mismatch", code, stderr.String())
	}
}

func TestRunJobHydratesNeedsAndPublishesAuthoritativeResult(t *testing.T) {
	producerStep := "gha-producer"
	producerPlanDigest := transport.Digest([]byte("producer-plan"))
	job := cliRunJobPlan()
	job.Dependencies = []string{producerStep}
	job.NeedSources = map[string][]plan.NeedSource{
		"producer": {{StepKey: producerStep, PlanDigest: producerPlanDigest}},
	}
	job.CallGuards = []plan.CallGuard{{
		Condition:   "needs.producer.result == 'success' && needs.producer.outputs.result == 'hydrated'",
		NeedSources: map[string][]plan.NeedSource{"producer": {{StepKey: producerStep, PlanDigest: producerPlanDigest}}},
		NeedOutputs: map[string][]plan.NeedOutput{"producer": {{Name: "result", StepKey: producerStep, Output: "result"}}},
	}}
	job.Env = map[string]string{"RESULT": "${{ needs.producer.outputs.result }}"}
	job.Steps = []plan.Step{{ID: "consume", Kind: "run", Command: `test "$RESULT" = "hydrated"`}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)

	producerManifest, err := transport.MarshalResultManifest(transport.ResultManifest{
		PlanDigest: producerPlanDigest,
		Producer:   transport.Producer{BuildID: cliTestBuildID, JobID: cliTestProducerJobID, StepKey: producerStep},
		Result:     "success",
		Outputs:    []transport.Output{{Name: "result", Value: "hydrated"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	producerPath := transport.ResultPath(producerStep, producerPlanDigest)
	runner := &cliCaptureRunner{
		failMetadata: true,
		jobByStep:    map[string]string{producerStep: cliTestProducerJobID},
		dataByPath:   map[string][]byte{producerPath: producerManifest},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: result metadata mirror") {
		t.Fatalf("stderr = %q, want non-fatal metadata warning", stderr.String())
	}
	if !strings.Contains(stdout.String(), "~~~ :package: Publish GitHub Actions result\n") || !strings.Contains(stdout.String(), "^^^ +++\n") {
		t.Fatalf("stdout = %q, want expanded result publication group", stdout.String())
	}
	if len(runner.commands) < 3 || strings.Join(runner.commands[0].args, " ") != strings.Join([]string{"artifact", "search", producerPath, "--step", producerStep, "--format", "%j"}, " ") {
		t.Fatalf("commands = %#v, want exact producer search first", runner.commands)
	}
	if got := runner.commands[1].args[len(runner.commands[1].args)-1]; got != cliTestProducerJobID {
		t.Fatalf("download producer = %q, want exact job UUID", got)
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if manifest.Result != "success" {
		t.Fatalf("published result = %q, want success", manifest.Result)
	}
	for _, command := range runner.commands {
		if command.dir != "" {
			if _, err := os.Stat(command.dir); !os.IsNotExist(err) {
				t.Fatalf("temporary artifact root %q still exists: %v", command.dir, err)
			}
		}
	}
}

func TestRunJobPublishesSummaryAsAdvisoryJobAnnotation(t *testing.T) {
	tests := []struct {
		name           string
		command        string
		failAnnotation bool
		wantAnnotation bool
		wantWarning    bool
		wantResult     string
		wantCode       int
	}{
		{name: "published", command: `printf '### Job summary\n\nPassed.\n' >> "$GITHUB_STEP_SUMMARY"`, wantAnnotation: true, wantResult: "success"},
		{name: "published after job failure", command: `printf '### Job summary\n\nPassed.\n' >> "$GITHUB_STEP_SUMMARY"; exit 7`, wantAnnotation: true, wantResult: "failure", wantCode: 1},
		{name: "failure is advisory", command: `printf '### Job summary\n\nPassed.\n' >> "$GITHUB_STEP_SUMMARY"`, failAnnotation: true, wantAnnotation: true, wantWarning: true, wantResult: "success"},
		{name: "empty summary is a no-op", command: "true", wantResult: "success"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Steps[0].Command = test.command
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			runner := &cliCaptureRunner{failAnnotation: test.failAnnotation}
			var stdout, stderr bytes.Buffer

			if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != test.wantCode {
				t.Fatalf("run() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			if !strings.Contains(stdout.String(), "~~~ :package: Publish GitHub Actions result\n") {
				t.Fatalf("stdout = %q, want result publication group", stdout.String())
			}
			if test.wantWarning && !strings.Contains(stdout.String(), "^^^ +++\n") {
				t.Fatalf("stdout = %q, want publication warning to expand its group", stdout.String())
			}
			if !test.wantWarning && test.wantCode == 0 && strings.Contains(stdout.String(), "^^^ +++\n") {
				t.Fatalf("stdout = %q, want successful publication group collapsed", stdout.String())
			}
			if test.wantCode != 0 && strings.Contains(stdout.String(), "Prepare GitHub Actions job failed") {
				t.Fatalf("stdout = %q, want action failure owned by its existing section", stdout.String())
			}
			if result := publishedCLIManifest(t, runner, job, planDigest); result.Result != test.wantResult {
				t.Fatalf("published result = %q, want %q", result.Result, test.wantResult)
			}
			var annotations []cliCommand
			for _, command := range runner.commands {
				if len(command.args) > 0 && command.args[0] == "annotate" {
					annotations = append(annotations, command)
				}
			}
			if !test.wantAnnotation {
				if len(annotations) != 0 {
					t.Fatalf("annotations = %#v, want none", annotations)
				}
				return
			}
			wantArgs := []string{"annotate", "--scope", "job", "--job", cliTestJobID, "--context", "buildkite-gha-job-summary", "--style", "info"}
			wantBody := "<h2 class=\"h4 mb2\">GitHub Actions job summary</h2>\n<div class=\"border-top border-gray pt2\"></div>\n\n### Job summary\n\nPassed.\n"
			if len(annotations) != 1 || !slices.Equal(annotations[0].args, wantArgs) || string(annotations[0].stdin) != wantBody {
				t.Fatalf("annotations = %#v", annotations)
			}
			if last := runner.commands[len(runner.commands)-1]; len(last.args) == 0 || last.args[0] != "annotate" {
				t.Fatalf("last command = %#v, want annotation after authoritative publication", last)
			}
			if gotWarning := strings.Contains(stderr.String(), "warning: job summary annotation"); gotWarning != test.wantWarning {
				t.Fatalf("stderr = %q, warning present = %v, want %v", stderr.String(), gotWarning, test.wantWarning)
			}
			if runner.contextErrors[len(runner.contextErrors)-1] != nil {
				t.Fatalf("annotation inherited cancelled context: %v", runner.contextErrors[len(runner.contextErrors)-1])
			}
		})
	}
}

func TestRunJobDisabledWorkflowTokenLinksPipelineSettings(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	job := cliRunJobPlan()
	job.Workflow.Path = ".github/workflows/ci.yml"
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &plan.GitHubToken{Workflow: "ci.yml", Permissions: map[string]string{"contents": "read"}}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	t.Setenv("BUILDKITE_GHA_AGENT", truePath)
	t.Setenv("BUILDKITE_ORGANIZATION_SLUG", "acme-inc")
	t.Setenv("BUILDKITE_PIPELINE_SLUG", "my-pipeline")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != 1 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	want := `enable "Allow workflow-authorized GitHub access tokens" in the pipeline's repository settings: https://buildkite.com/acme-inc/my-pipeline/settings/repository`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRunJobAnnotatesUnavailableBuildkiteSecretWithMigrationGuidance(t *testing.T) {
	job := cliRunJobPlan()
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"EXAMPLE_SECRET"}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	agentPath := filepath.Join(t.TempDir(), "buildkite-agent")
	if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_GHA_AGENT", agentPath)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer

	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 {
		t.Fatalf("run() code = %d, stderr = %q, want 1", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `resolve secret "EXAMPLE_SECRET": buildkite Agent secret request failed`) {
		t.Fatalf("stderr = %q, want secret resolution failure", stderr.String())
	}
	var annotations []cliCommand
	for _, command := range runner.commands {
		if len(command.args) > 0 && command.args[0] == "annotate" {
			annotations = append(annotations, command)
		}
	}
	wantArgs := []string{"annotate", "--scope", "job", "--job", cliTestJobID, "--context", secretResolutionAnnotationContext, "--style", "error"}
	if len(annotations) != 1 || !slices.Equal(annotations[0].args, wantArgs) {
		t.Fatalf("annotations = %#v, want one secret resolution annotation", annotations)
	}
	body := string(annotations[0].stdin)
	for _, want := range []string{
		"#### Missing secret",
		"Buildkite secret <code>EXAMPLE_SECRET</code>",
		`<a href="https://buildkite.com/docs/pipelines/security/secrets/buildkite-secrets" target="_blank">Create or migrate the secret into Buildkite</a>`,
		`<a href="https://buildkite.com/docs/pipelines/security/secrets/buildkite-secrets/access-policies" target="_blank">grant this job access with its access policy</a>`,
		"> ℹ️ GitHub does not expose an existing secret's value after creation",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("annotation = %q, want %q", body, want)
		}
	}
	if strings.Contains(body, "Retry the job") {
		t.Errorf("annotation = %q, does not want retry guidance", body)
	}
	if last := runner.commands[len(runner.commands)-1]; len(last.args) == 0 || last.args[0] != "annotate" {
		t.Fatalf("last command = %#v, want guidance annotation after authoritative publication", last)
	}
}

func TestRunJobPublishesWorkflowCommandsAsAdvisoryJobAnnotations(t *testing.T) {
	diagnostics := "printf '%s\\n' '::warning title=Lint::warning body'; printf '%s\\n' '::error file=main.go,line=7::error body' >&2"
	tests := []struct {
		name           string
		command        string
		failAnnotation bool
		wantAnnotation bool
		wantWarning    bool
		wantResult     string
		wantCode       int
	}{
		{name: "published without changing success", command: diagnostics, wantAnnotation: true, wantResult: "success"},
		{name: "published after job failure", command: diagnostics + "; exit 7", wantAnnotation: true, wantResult: "failure", wantCode: 1},
		{name: "publication failure is advisory", command: diagnostics, failAnnotation: true, wantAnnotation: true, wantWarning: true, wantResult: "success"},
		{name: "empty diagnostics are a no-op", command: "true", wantResult: "success"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Steps[0].Command = test.command
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			runner := &cliCaptureRunner{failAnnotation: test.failAnnotation}
			var stdout, stderr bytes.Buffer

			if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != test.wantCode {
				t.Fatalf("run() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			if result := publishedCLIManifest(t, runner, job, planDigest); result.Result != test.wantResult {
				t.Fatalf("published result = %q, want %q", result.Result, test.wantResult)
			}
			var annotations []cliCommand
			for _, command := range runner.commands {
				if len(command.args) > 0 && command.args[0] == "annotate" {
					annotations = append(annotations, command)
				}
			}
			if !test.wantAnnotation {
				if len(annotations) != 0 {
					t.Fatalf("annotations = %#v, want none", annotations)
				}
				return
			}
			want := []struct {
				context, style, body string
			}{
				{context: "buildkite-gha-workflow-warnings", style: "warning", body: "warning body"},
				{context: "buildkite-gha-workflow-errors", style: "error", body: "error body"},
			}
			if len(annotations) != len(want) {
				t.Fatalf("annotations = %#v, want warning and error", annotations)
			}
			for i, expected := range want {
				wantArgs := []string{"annotate", "--scope", "job", "--job", cliTestJobID, "--context", expected.context, "--style", expected.style}
				if !slices.Equal(annotations[i].args, wantArgs) || !strings.Contains(string(annotations[i].stdin), expected.body) {
					t.Errorf("annotation %d = %#v, want args %#v and body containing %q", i, annotations[i], wantArgs, expected.body)
				}
			}
			for _, command := range runner.commands[len(runner.commands)-len(want):] {
				if len(command.args) == 0 || command.args[0] != "annotate" {
					t.Fatalf("trailing command = %#v, want annotations after authoritative publication", command)
				}
			}
			gotWarning := strings.Contains(stderr.String(), "warning: workflow warning annotation") && strings.Contains(stderr.String(), "warning: workflow error annotation")
			if gotWarning != test.wantWarning {
				t.Fatalf("stderr = %q, advisory warnings present = %v, want %v", stderr.String(), gotWarning, test.wantWarning)
			}
			for _, contextErr := range runner.contextErrors[len(runner.contextErrors)-len(want):] {
				if contextErr != nil {
					t.Fatalf("annotation inherited cancelled context: %v", contextErr)
				}
			}
		})
	}
}

func TestRunJobPublishesEveryTerminalResultAfterCancellation(t *testing.T) {
	tests := []struct {
		name       string
		condition  string
		command    string
		cancel     bool
		tolerate   bool
		wantResult string
		wantCode   int
	}{
		{name: "failure", command: "exit 7", wantResult: "failure", wantCode: 1},
		{name: "tolerated failure", command: "exit 7", tolerate: true, wantResult: "success", wantCode: buildkitepipeline.ContinueOnErrorExitStatus},
		{name: "skipped", condition: "${{ false }}", command: "true", wantResult: "skipped", wantCode: 0},
		{name: "cancelled", command: "sleep 1", cancel: true, wantResult: "cancelled", wantCode: 1},
		{name: "tolerated cancellation", command: "sleep 1", cancel: true, tolerate: true, wantResult: "cancelled", wantCode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := cliRunJobPlan()
			job.Condition = test.condition
			job.Steps[0].Command = test.command
			if test.tolerate {
				job.Schema = plan.Schema
				job.ContinueOnError = true
				job.Runtime = &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()}
				requiresMise := false
				job.RequiresMise = &requiresMise
			}
			planPath, planDigest := writeCLIJobPlan(t, job)
			setCLIJobIdentity(t, job, planDigest)
			runner := &cliCaptureRunner{}
			ctx := t.Context()
			if test.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			var stdout, stderr bytes.Buffer
			if code := runJobContext(ctx, []string{"--plan", planPath}, &stdout, &stderr, "dev", "dev", transport.Agent{Runner: runner}); code != test.wantCode {
				t.Fatalf("runJobContext() code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			manifest := publishedCLIManifest(t, runner, job, planDigest)
			if manifest.Result != test.wantResult {
				t.Fatalf("published result = %q, want %q", manifest.Result, test.wantResult)
			}
			for i, contextErr := range runner.contextErrors {
				if contextErr != nil {
					t.Fatalf("publication command %d inherited cancelled context: %v", i, contextErr)
				}
			}
		})
	}
}

func TestPublishTerminalResultAnnotatesCancelledJobWithFreshContext(t *testing.T) {
	job := cliRunJobPlan()
	planDigest := transport.Digest([]byte("cancelled-plan"))
	producer := transport.Producer{BuildID: cliTestBuildID, JobID: cliTestJobID, StepKey: job.Target.StepKey}
	runner := &cliCaptureRunner{}
	artifactDigest := strings.Repeat("a", 64)
	artifact := transport.ResultArtifact{
		Name: "payload", ID: "123", Path: "buildkite-gha/v1/artifacts/" + artifactDigest + ".zip",
		Digest: "sha256:" + artifactDigest, Size: 42, FileCount: 1,
	}

	publication, err := publishTerminalResult(
		t.Context(),
		transport.Agent{Runner: runner},
		t.TempDir(),
		job,
		planDigest,
		producer,
		gharuntime.JobResult{
			Conclusion:         "cancelled",
			Summary:            "summary before cancellation\n",
			WarningAnnotations: "warning before cancellation\n",
			ErrorAnnotations:   "error before cancellation\n",
			Artifacts:          []transport.ResultArtifact{artifact},
		},
	)
	if err != nil || publication.SummaryAnnotationError != nil || publication.WarningAnnotationError != nil || publication.ErrorAnnotationError != nil {
		t.Fatalf("publishTerminalResult() publication = %#v, error = %v", publication, err)
	}
	wantBodies := []string{
		"<h2 class=\"h4 mb2\">GitHub Actions job summary</h2>\n<div class=\"border-top border-gray pt2\"></div>\n\nsummary before cancellation\n",
		"warning before cancellation\n",
		"error before cancellation\n",
	}
	commands := runner.commands[len(runner.commands)-len(wantBodies):]
	for i, command := range commands {
		if len(command.args) == 0 || command.args[0] != "annotate" || string(command.stdin) != wantBodies[i] {
			t.Errorf("annotation %d = %#v, want cancelled job annotation body %q", i, command, wantBodies[i])
		}
	}
	for _, contextErr := range runner.contextErrors[len(runner.contextErrors)-len(wantBodies):] {
		if contextErr != nil {
			t.Fatalf("annotation inherited cancelled context: %v", contextErr)
		}
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if !reflect.DeepEqual(manifest.Artifacts, []transport.ResultArtifact{artifact}) {
		t.Fatalf("published artifacts = %#v, want %#v", manifest.Artifacts, artifact)
	}
}

func TestRunJobPublishesHydrationFailureAndRejectsMissingIdentity(t *testing.T) {
	producerStep := "gha-producer"
	producerPlanDigest := transport.Digest([]byte("producer-plan"))
	job := cliRunJobPlan()
	job.Dependencies = []string{producerStep}
	job.NeedSources = map[string][]plan.NeedSource{
		"producer": {{StepKey: producerStep, PlanDigest: producerPlanDigest}},
	}
	planPath, planDigest := writeCLIJobPlan(t, job)

	t.Run("needs require Buildkite", func(t *testing.T) {
		clearCLIJobIdentity(t)
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "prerequisites require Buildkite result identity") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want no unauthenticated agent calls", runner.commands)
		}
	})

	t.Run("hydration failure is published", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		runner := &cliCaptureRunner{jobByStep: map[string]string{producerStep: cliTestProducerJobID}}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "hydrate prerequisite results") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "+++ :warning: Prepare GitHub Actions job failed\n~~~ :package: Publish GitHub Actions result\n") {
			t.Fatalf("stdout = %q, want visible prerequisite failure before collapsed publication", stdout.String())
		}
		manifest := publishedCLIManifest(t, runner, job, planDigest)
		if manifest.Result != "failure" {
			t.Fatalf("published result = %q, want failure", manifest.Result)
		}
	})

	t.Run("partial identity fails before execution", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		t.Setenv("BUILDKITE_JOB_ID", "")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "requires valid BUILDKITE_BUILD_ID") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want identity failure before side effects", runner.commands)
		}
	})

	t.Run("missing plan digest fails before execution", func(t *testing.T) {
		setCLIJobIdentity(t, job, planDigest)
		t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", "")
		runner := &cliCaptureRunner{}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "requires BUILDKITE_GHA_PLAN_DIGEST") {
			t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
		}
		if len(runner.commands) != 0 {
			t.Fatalf("commands = %#v, want digest failure before side effects", runner.commands)
		}
	})
}

func TestRunJobFailsWhenAuthoritativePublicationFails(t *testing.T) {
	job := cliRunJobPlan()
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	events := captureCommandTelemetry(t)
	runner := &cliCaptureRunner{failAt: 1}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "publish terminal result") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "~~~ :package: Publish GitHub Actions result\n") || !strings.Contains(stdout.String(), "^^^ +++\n") {
		t.Fatalf("stdout = %q, want failed publication group expanded", stdout.String())
	}
	event := <-events
	if event.FailurePhase != telemetry.FailurePhaseResultPublication || event.FailureCode != telemetry.FailureCodeUnknown {
		t.Fatalf("telemetry = %#v", event)
	}
	if !strings.Contains(event.ErrorMessage, "publish terminal result") {
		t.Fatalf("telemetry error message = %q", event.ErrorMessage)
	}
}

func TestRunJobTelemetryClassifiesExecutionFailure(t *testing.T) {
	job := cliRunJobPlan()
	job.Steps[0].Command = "exit 7"
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	events := captureCommandTelemetry(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != 1 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	event := <-events
	if event.FailurePhase != telemetry.FailurePhaseExecution || event.FailureCode != telemetry.FailureCodeStepProcessExit {
		t.Fatalf("telemetry = %#v", event)
	}
	if !strings.Contains(event.ErrorMessage, "exit status 7") {
		t.Fatalf("telemetry error message = %q", event.ErrorMessage)
	}
}

func TestRunJobTelemetryClassifiesUnsupportedShell(t *testing.T) {
	job := cliRunJobPlan()
	job.Steps[0].Shell = "pwsh"
	job.Steps[0].Command = "Get-Location"
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	events := captureCommandTelemetry(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", &cliCaptureRunner{}); code != 1 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	event := <-events
	if event.FailurePhase != telemetry.FailurePhaseExecution || event.FailureCode != telemetry.FailureCodeUnsupportedFeature {
		t.Fatalf("telemetry = %#v", event)
	}
	if event.Blocker != "shell" || event.BlockerDetail != "" {
		t.Fatalf("telemetry blocker = %q / %q", event.Blocker, event.BlockerDetail)
	}
	if !strings.Contains(event.ErrorMessage, `shell "pwsh" is unsupported`) {
		t.Fatalf("telemetry error message = %q", event.ErrorMessage)
	}
}

func TestRunJobValidateHostErrorsClassifyAsUnsupported(t *testing.T) {
	job := plan.Job{RequiredCapabilities: []string{"docker"}}
	err := gharuntime.ValidateHost(job, "darwin", "arm64")
	if err == nil {
		t.Fatal("ValidateHost() = nil, want macOS docker rejection")
	}
	if got := runtimeFailureCode(err); got != telemetry.FailureCodeUnsupportedFeature {
		t.Fatalf("runtimeFailureCode() = %q, want %q for %v", got, telemetry.FailureCodeUnsupportedFeature, err)
	}
}

func TestRunJobPublishesBoundedFailureForUnrepresentableOutputs(t *testing.T) {
	job := cliRunJobPlan()
	job.Steps[0].Command = `printf 'summary from malformed result\n' >> "$GITHUB_STEP_SUMMARY"`
	job.Outputs = make(map[string]string, transport.MaxResultOutputs+1)
	for i := 0; i <= transport.MaxResultOutputs; i++ {
		job.Outputs[fmt.Sprintf("output_%02d", i)] = "value"
	}
	planPath, planDigest := writeCLIJobPlan(t, job)
	setCLIJobIdentity(t, job, planDigest)
	runner := &cliCaptureRunner{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run-job", "--plan", planPath}, &stdout, &stderr, "dev", runner); code != 1 || !strings.Contains(stderr.String(), "validate terminal result") {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	manifest := publishedCLIManifest(t, runner, job, planDigest)
	if manifest.Result != "failure" || len(manifest.Outputs) != 0 {
		t.Fatalf("published result = %#v, want bounded failure without outputs", manifest)
	}
	last := runner.commands[len(runner.commands)-1]
	wantSummary := "<h2 class=\"h4 mb2\">GitHub Actions job summary</h2>\n<div class=\"border-top border-gray pt2\"></div>\n\nsummary from malformed result\n"
	if len(last.args) == 0 || last.args[0] != "annotate" || string(last.stdin) != wantSummary {
		t.Fatalf("last command = %#v, want summary annotation after bounded failure", last)
	}
}

func TestVerifyBuildkiteTargetFailsClosed(t *testing.T) {
	job := plan.Job{Target: plan.Target{StepKey: "gha-expected", Queue: "gha-runtime"}}
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "gha-other")
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "gha-runtime")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "executing step") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want step mismatch", err)
	}
	t.Setenv("BUILDKITE_STEP_KEY", "")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "requires BUILDKITE_STEP_KEY") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want missing binding", err)
	}
	t.Setenv("BUILDKITE_STEP_KEY", "gha-expected")
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "gha-other")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "executing queue") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want explicit queue mismatch", err)
	}
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "")
	if err := verifyBuildkiteTarget(job); err == nil || !strings.Contains(err.Error(), "requires BUILDKITE_AGENT_META_DATA_QUEUE") {
		t.Fatalf("verifyBuildkiteTarget() error = %v, want missing explicit queue binding", err)
	}
	job.Target.Queue = ""
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", "customer-default")
	if err := verifyBuildkiteTarget(job); err != nil {
		t.Fatalf("verifyBuildkiteTarget() default targeting error = %v", err)
	}
}

func TestRepositoryProviderGitCredentialsUseServerEnvironment(t *testing.T) {
	values := map[string]string{}
	getenv := func(name string) string { return values[name] }
	if repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("repository provider credentials enabled without a server setting")
	}
	values[repositoryProviderGitCredentialsEnvironment] = "true"
	if !repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("repository provider credentials setting was ignored")
	}
	delete(values, repositoryProviderGitCredentialsEnvironment)
	values[legacyGitHubAppGitCredentialsEnvironment] = "true"
	if !repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("legacy GitHub App credentials setting was ignored")
	}
	values[legacyGitHubAppGitCredentialsEnvironment] = "TRUE"
	if repositoryProviderGitCredentialsEnabled(getenv) {
		t.Fatal("non-canonical server setting enabled repository credentials")
	}
}

func TestRunJobExecutesPureRunPlanWithoutCheckout(t *testing.T) {
	clearCLIJobIdentity(t)
	workspace := t.TempDir()
	requiresMise := false
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: cliTestRuntimeDigest(),
		},
		Runtime: &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()},
		Workflow: plan.Workflow{
			Path:         "missing-workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "missing",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-missing", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
		RequiresMise:         &requiresMise,
	}
	attachCLIExecutionProgram(&job)
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(workspace, "plan.json")
	resultPath := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run-job", "--plan", planPath, "--result", resultPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q, want success", code, stderr.String())
	}
	result, err := os.ReadFile(resultPath)
	if err != nil || !bytes.Contains(result, []byte(`"conclusion": "success"`)) {
		t.Fatalf("result = %q, error = %v", result, err)
	}
}

func TestHydrateEventPayloadDownloadsFromExactImporterJob(t *testing.T) {
	payload := []byte(`{"action":"opened","number":42}`)
	digest := transport.Digest(payload)
	path, err := buildkitepipeline.EventPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	job := cliRunJobPlan()
	job.Event.PayloadDigest = digest
	job.Event.PayloadArtifact = true
	runner := &cliCaptureRunner{dataByPath: map[string][]byte{path: payload}}
	if err := hydrateEventPayload(t.Context(), transport.Agent{Runner: runner}, &job, cliTestJobID); err != nil {
		t.Fatal(err)
	}
	if job.Event.Payload == nil || (*job.Event.Payload)["action"] != "opened" {
		t.Fatalf("hydrated event = %#v", job.Event.Payload)
	}
	if len(runner.commands) != 1 || len(runner.commands[0].args) != 6 ||
		!slices.Equal(runner.commands[0].args[:3], []string{"artifact", "download", path}) ||
		runner.commands[0].args[3] == "" ||
		!slices.Equal(runner.commands[0].args[4:], []string{"--step", cliTestJobID}) {
		t.Fatalf("artifact commands = %#v", runner.commands)
	}

	tampered := &cliCaptureRunner{dataByPath: map[string][]byte{path: []byte(`{"action":"closed"}`)}}
	job.Event.Payload = nil
	if err := hydrateEventPayload(t.Context(), transport.Agent{Runner: tampered}, &job, cliTestJobID); err == nil || !strings.Contains(err.Error(), "does not match its digest") {
		t.Fatalf("tampered event error = %v", err)
	}
}

func cliRunJobPlan() plan.Job {
	requiresMise := false
	return plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version:            "dev",
			DistributionDigest: cliTestRuntimeDigest(),
		},
		Runtime: &plan.Runtime{DistributionDigest: cliTestRuntimeDigest()},
		Workflow: plan.Workflow{
			Path:         "workflow.yml",
			Digest:       "sha256:" + strings.Repeat("1", 64),
			LogicalJobID: "cli",
		},
		Event:                plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               plan.Target{StepKey: "gha-cli", Queue: "gha-runtime"},
		RequiredCapabilities: []string{},
		Steps:                []plan.Step{{ID: "step-1", Kind: "run", Command: "true"}},
		RequiresMise:         &requiresMise,
	}
}

func attachCLIExecutionProgram(job *plan.Job) {
	actions := map[string]executionprogram.Action{}
	for _, lock := range job.Actions {
		action := executionprogram.Action{Runtime: "node24", Main: "index.js", PreIf: cliProgramSite(""), PostIf: cliProgramSite("")}
		if lock.DockerImage != "" {
			action.Runtime, action.Main, action.Image = "docker", "", lock.DockerImage
		}
		actions[lock.ID] = action
	}
	normalized := executionprogram.Program{Version: executionprogram.Version, Actions: actions, Job: executionprogram.Job{
		Condition: cliProgramSite(job.Condition), ContinueOnError: job.ContinueOnError, TimeoutMinutes: job.TimeoutMinutes,
		Env:      cliProgramBindings(job.Env),
		Defaults: executionprogram.Defaults{Shell: cliProgramSite(job.DefaultShell), WorkingDirectory: cliProgramSite(job.DefaultWorkingDirectory)},
		Outputs:  cliProgramBindings(job.Outputs),
	}}
	normalized.Job.Guards = make([]executionprogram.Guard, len(job.CallGuards))
	for i, guard := range job.CallGuards {
		normalized.Job.Guards[i].Condition = cliProgramSite(guard.Condition)
	}
	normalized.Job.Steps = make([]executionprogram.Step, len(job.Steps))
	for i := range job.Steps {
		step := &job.Steps[i]
		projected := executionprogram.Step{ID: step.ID, Kind: step.Kind, Background: step.Background, Targets: append([]string(nil), step.Targets...), Env: cliProgramBindings(step.Env), Condition: cliProgramSite(step.Condition), ContinueOnError: executionprogram.BoolControl{Literal: step.ContinueOnError}, TimeoutMinutes: executionprogram.NumberControl{Literal: step.TimeoutMinutes}, Name: cliProgramSite(step.Name)}
		if step.ContinueOnErrorExpression != "" {
			value := cliProgramSite(step.ContinueOnErrorExpression)
			projected.ContinueOnError.Expression = &value
		}
		if step.TimeoutMinutesExpression != "" {
			value := cliProgramSite(step.TimeoutMinutesExpression)
			projected.TimeoutMinutes.Expression = &value
		}
		switch step.Kind {
		case "run":
			projected.Run = &executionprogram.Run{Command: cliProgramSite(step.Command), Shell: cliProgramSite(step.Shell), WorkingDirectory: cliProgramSite(step.WorkingDirectory)}
		case "uses":
			projected.Invocation = &executionprogram.Invocation{Uses: cliProgramSite(step.Uses), With: cliProgramBindings(step.With)}
			if step.Action != nil {
				projected.Invocation.Lock = step.Action.Lock
			}
		}
		normalized.Job.Steps[i] = projected
		step.Execution = &normalized.Job.Steps[i]
	}
	if job.Container != nil {
		normalized.Job.Container = &executionprogram.Container{Image: cliProgramSite(job.Container.Image), Env: cliProgramBindings(job.Container.Env), Ports: cliProgramSites(job.Container.Ports)}
	}
	normalized.DeriveSiteSemantics()
	job.Program = &normalized
}

func cliProgramSite(source string) executionprogram.Site {
	return executionprogram.Site{Source: source}
}

func cliProgramBindings(values map[string]string) []executionprogram.Binding {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]executionprogram.Binding, 0, len(names))
	for _, name := range names {
		result = append(result, executionprogram.Binding{Name: name, Value: cliProgramSite(values[name])})
	}
	return result
}

func cliProgramSites(values []string) []executionprogram.Site {
	result := make([]executionprogram.Site, len(values))
	for i, value := range values {
		result[i] = cliProgramSite(value)
	}
	return result
}

func writeCLIJobPlan(t *testing.T, job plan.Job) (string, string) {
	t.Helper()
	attachCLIExecutionProgram(&job)
	encoded, err := plan.Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, transport.Digest(encoded)
}

func setCLIJobIdentity(t *testing.T, job plan.Job, planDigest string) {
	t.Helper()
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", cliTestBuildID)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_STEP_KEY", job.Target.StepKey)
	t.Setenv("BUILDKITE_AGENT_META_DATA_QUEUE", job.Target.Queue)
	t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", planDigest)
}

func captureCommandTelemetry(t *testing.T) <-chan telemetry.Properties {
	t.Helper()
	events := make(chan telemetry.Properties, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received struct {
			Properties telemetry.Properties `json:"properties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode telemetry event: %v", err)
		}
		events <- received.Properties
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "telemetry-token")
	t.Setenv("BUILDKITE_GHA_TELEMETRY_DISABLED", "")
	return events
}

func clearCLIJobIdentity(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BUILDKITE",
		"BUILDKITE_BUILD_ID",
		"BUILDKITE_JOB_ID",
		"BUILDKITE_STEP_KEY",
		"BUILDKITE_AGENT_META_DATA_QUEUE",
		"BUILDKITE_GHA_PLAN_DIGEST",
	} {
		t.Setenv(name, "")
	}
}

func publishedCLIManifest(t *testing.T, runner *cliCaptureRunner, job plan.Job, planDigest string) transport.ResultManifest {
	t.Helper()
	path := transport.ResultPath(job.Target.StepKey, planDigest)
	data, ok := runner.uploaded[path]
	if !ok {
		t.Fatalf("authoritative result %q was not uploaded; uploads = %#v", path, runner.uploaded)
	}
	producer := transport.Producer{BuildID: cliTestBuildID, JobID: cliTestJobID, StepKey: job.Target.StepKey}
	manifest, err := transport.VerifyResultManifest(data, planDigest, producer)
	if err != nil {
		t.Fatalf("verify published result: %v\n%s", err, data)
	}
	return manifest
}
