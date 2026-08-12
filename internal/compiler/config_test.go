package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestCompileOptionsSnapshotVarsWithDeterministicPrecedence(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  test:
    runs-on: ${{ vars.RUNNER }}
    strategy:
      matrix:
        version: ${{ fromJSON(vars.VERSIONS) }}
    steps:
      - run: true
`)
	options := Options{
		EventTrust: EventTrusted,
		Vars: VariableSources{
			Bridge:    map[string]string{"runner": "ubuntu-22.04", "versions": `["bridge"]`, "SOURCE": "bridge"},
			Provider:  map[string]string{"RUNNER": "ubuntu-24.04", "VERSIONS": `["provider"]`, "source": "provider"},
			Buildkite: map[string]string{"VERSIONS": `[12,"14"]`, "SOURCE": "buildkite"},
		},
		Runners: RunnerPolicy{Labels: map[string]string{"ubuntu-24.04": "linux-trusted"}},
	}
	first, err := CompileWithOptions("vars.yml", workflow, pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileWithOptions("vars.yml", workflow, pushEvent(t), options)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("repeated compilation with the same snapshots was not byte-identical")
	}
	var ir IR
	if err := json.Unmarshal(first, &ir); err != nil {
		t.Fatal(err)
	}
	wantVars := map[string]string{"RUNNER": "ubuntu-24.04", "VERSIONS": `[12,"14"]`, "SOURCE": "buildkite"}
	if !reflect.DeepEqual(ir.Vars, wantVars) {
		t.Fatalf("vars snapshot = %#v, want %#v", ir.Vars, wantVars)
	}
	if len(ir.Jobs) != 2 || ir.Jobs[0].Queue != "linux-trusted" || ir.Jobs[0].Matrix["version"] != float64(12) || ir.Jobs[1].Matrix["version"] != "14" {
		t.Fatalf("compiled jobs = %#v", ir.Jobs)
	}
}

func TestCompileOptionsRejectCaseCollisionsWithinOneVarsSource(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: true\n")
	_, err := CompileWithOptions("vars.yml", workflow, pushEvent(t), Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"Deploy_Env": "one", "DEPLOY_ENV": "two"}},
		Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-24.04": "linux"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bridge vars source contains case-colliding names") {
		t.Fatalf("CompileWithOptions() error = %v, want vars collision", err)
	}
}

func TestRunsOnPolicySeparatesTrustedAndUntrustedEvents(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: true\n")
	policy := RunnerPolicy{Labels: map[string]string{"ubuntu-24.04": "linux-privileged"}}
	trusted := Options{EventTrust: EventTrusted, Runners: policy}
	if _, err := CompileWithOptions("trusted.yml", workflow, pushEvent(t), trusted); err != nil {
		t.Fatalf("trusted compilation failed: %v", err)
	}

	untrusted := Options{EventTrust: EventUntrusted, Runners: policy}
	_, err := CompileWithOptions("untrusted.yml", workflow, pushEvent(t), untrusted)
	if err == nil || !strings.Contains(err.Error(), `untrusted event cannot target queue "linux-privileged"`) {
		t.Fatalf("untrusted compilation error = %v", err)
	}

	policy.UntrustedQueues = []string{"linux-privileged"}
	untrusted.Runners = policy
	compiled, err := CompileWithOptions("untrusted.yml", workflow, pushEvent(t), untrusted)
	if err != nil {
		t.Fatalf("allowlisted untrusted compilation failed: %v", err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Event.Trust != EventUntrusted || ir.Jobs[0].Queue != "linux-privileged" {
		t.Fatalf("untrusted compiler output = %#v", ir)
	}
}

func TestUntrustedOptionsPermitAnonymousPublicActionSource(t *testing.T) {
	options := defaultOptions()
	options.ResolveActions = true
	options.ActionSource = &fakeActionSource{}
	if err := options.validate(); err != nil {
		t.Fatalf("Options.validate() error = %v", err)
	}
}

func TestOptionsRejectActionConfigurationWithoutResolution(t *testing.T) {
	options := defaultOptions()
	options.ActionSource = &fakeActionSource{}
	if err := options.validate(); err == nil || !strings.Contains(err.Error(), "requires ResolveActions") {
		t.Fatalf("Options.validate() error = %v, want action-resolution configuration rejection", err)
	}
}

func TestOptionsValidateRunnerTargetImages(t *testing.T) {
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	tests := []struct {
		name   string
		target RunnerTarget
		want   string
	}{
		{name: "Linux immutable image", target: RunnerTarget{Queue: "hosted", Platform: PlatformLinuxAMD64, Image: image}},
		{name: "mutable image", target: RunnerTarget{Queue: "hosted", Platform: PlatformLinuxAMD64, Image: "ubuntu:latest"}, want: "invalid immutable runtime image"},
		{name: "Darwin image", target: RunnerTarget{Queue: "macos", Platform: PlatformDarwinARM64, Image: image}, want: "cannot select a runtime image on darwin/arm64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := Options{EventTrust: EventTrusted, Runners: RunnerPolicy{Targets: map[string]RunnerTarget{"runner": test.target}}}
			err := options.validate()
			if test.want == "" && err != nil {
				t.Fatalf("Options.validate() error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("Options.validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateEventReportsInvalidCompilerEnvironmentWithoutFailingWorkflowStage(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	options := defaultOptions()
	options.Runners.Labels["ubuntu-latest"] = "not a queue"
	report, err := ValidateEventWithOptions("environment.yml", workflow, pushEvent(t), options)
	if err == nil {
		t.Fatal("ValidateEventWithOptions() error = nil, want invalid environment")
	}
	var finding *ProcessingFinding
	if !errors.As(err, &finding) {
		t.Fatalf("ValidateEventWithOptions() error = %T, want ProcessingFinding", err)
	}
	if finding.Code != CodeEnvironment || finding.Stage != "" || finding.Category != "environment" || finding.Message != "workflow-processing configuration is invalid" {
		t.Fatalf("finding = %#v", finding)
	}
	if report.LogicalJobs != 1 || len(report.ParsedJobs) != 1 || !report.NotEvaluatedJobs["test"] {
		t.Fatalf("report = %#v", report)
	}
}

func TestCompileEvaluatesGitHubEventVarsAndMatrixForRunsOn(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  test:
    name: test-${{ matrix.os }}
    strategy:
      matrix: ${{ fromJSON(vars.MATRIX) }}
    runs-on: ${{ matrix.os }}
    steps:
      - run: true
  event:
    runs-on: runner-${{ github.event_name }}-${{ event.channel }}
    steps:
      - run: true
`)
	event := strings.Replace(string(pushEvent(t)), `"payload": {`, `"payload": {"channel":"stable",`, 1)
	options := Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"MATRIX": `{"os":["ubuntu-22.04","ubuntu-24.04"]}`}},
		Runners: RunnerPolicy{Labels: map[string]string{
			"ubuntu-22.04":       "linux",
			"ubuntu-24.04":       "linux",
			"runner-push-stable": "event-linux",
		}},
	}
	compiled, err := CompileWithOptions("contexts.yml", workflow, []byte(event), options)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 3 || ir.Jobs[0].Queue != "event-linux" || ir.Jobs[1].Queue != "linux" || ir.Jobs[2].Queue != "linux" {
		t.Fatalf("context-selected queues = %#v", ir.Jobs)
	}
	if ir.Jobs[1].Label != "test-ubuntu-22.04" || ir.Jobs[2].Label != "test-ubuntu-24.04" {
		t.Fatalf("matrix-resolved labels = %q and %q", ir.Jobs[1].Label, ir.Jobs[2].Label)
	}
}

func TestRunsOnPolicyFailsClosedWithLocatedDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		runsOn string
		vars   map[string]string
		labels map[string]string
		want   string
	}{
		{name: "unsupported operating system", runsOn: "windows-latest", labels: map[string]string{"windows-latest": "windows"}, want: `unsupported operating system runner label "windows-latest"`},
		{name: "unmapped label", runsOn: "ubuntu-20.04", labels: map[string]string{"ubuntu-24.04": "linux"}, want: `runner label "ubuntu-20.04" is not mapped by policy`},
		{name: "unresolved expression", runsOn: "${{ vars.RUNNER }}", labels: map[string]string{"ubuntu-24.04": "linux"}, want: `unavailable value "vars.runner"`},
		{name: "conflicting labels", runsOn: "[self-hosted, linux]", labels: map[string]string{"self-hosted": "one", "linux": "two"}, want: "runner labels resolve to conflicting queues"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workflow := []byte("on: push\njobs:\n  test:\n    runs-on: " + test.runsOn + "\n    steps:\n      - run: true\n")
			_, err := CompileWithOptions("policy.yml", workflow, pushEvent(t), Options{
				EventTrust: EventTrusted,
				Vars:       VariableSources{Bridge: test.vars},
				Runners:    RunnerPolicy{Labels: test.labels},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "policy.yml:") {
				t.Fatalf("CompileWithOptions() error = %v, want located %q", err, test.want)
			}
		})
	}
}

func TestDefaultCompilePolicyIsUntrustedAndUsesBuildkiteDefault(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	compiled, err := Compile("default.yml", workflow, pushEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Event.Trust != EventUntrusted || ir.Jobs[0].Queue != "" {
		t.Fatalf("default compiler trust boundary = event %q, queue %q", ir.Event.Trust, ir.Jobs[0].Queue)
	}
	plans, err := compileUntrustedPlans("default.yml", workflow, pushEvent(t), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Target.Queue != "gha-untrusted" || plans[0].Schema != plan.SchemaV8 {
		t.Fatalf("compileUntrustedPlans() explicit legacy target = %#v", plans)
	}
}

func TestUntrustedDefaultQueueRequiresExplicitPolicy(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: [ubuntu-latest, linux]\n    steps:\n      - run: true\n")
	options := Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{Labels: map[string]string{
			"ubuntu-latest": "",
			"linux":         "",
		}},
	}
	if _, err := CompileWithOptions("default.yml", workflow, pushEvent(t), options); err == nil || !strings.Contains(err.Error(), "cannot use Buildkite default agent targeting") {
		t.Fatalf("CompileWithOptions() error = %v, want default-targeting policy rejection", err)
	}
	options.Runners.AllowUntrustedDefaultQueue = true
	if _, err := CompileWithOptions("default.yml", workflow, pushEvent(t), options); err != nil {
		t.Fatalf("CompileWithOptions() explicit default policy error = %v", err)
	}
	options.EventTrust = EventTrusted
	options.Runners.Labels["linux"] = "explicit"
	if _, err := CompileWithOptions("default.yml", workflow, pushEvent(t), options); err == nil || !strings.Contains(err.Error(), "conflicting queues") {
		t.Fatalf("CompileWithOptions() mixed default/explicit error = %v", err)
	}
}

func TestCompilePlansUsePolicyQueueAndContainOnlyNonSecretVars(t *testing.T) {
	t.Setenv("BUILDKITE_GHA_TEST_SECRET", "resolved-secret-value")
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: echo '${{ secrets.TOKEN }}'\n")
	options := Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"PUBLIC": "snapshotted"}},
		Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-24.04": "linux"}},
	}
	plans, err := compilePlansForTest(context.Background(), "plan.yml", workflow, pushEvent(t), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Target.Queue != "linux" || plans[0].Vars["PUBLIC"] != "snapshotted" {
		t.Fatalf("compiled plans = %#v", plans)
	}
	encoded, err := json.Marshal(plans)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("resolved-secret-value")) || !bytes.Contains(encoded, []byte(`${{ secrets.TOKEN }}`)) {
		t.Fatalf("plan contains resolved secret material: %s", encoded)
	}
}

func pushEvent(t *testing.T) []byte {
	t.Helper()
	return readFile(t, smokePath("events", "push.json"))
}
