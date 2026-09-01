package compiler

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
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

func TestCompileResolvesRunNameFromGitHubAndDispatchInputs(t *testing.T) {
	workflow := []byte(`name: Deploy
run-name: Deploy ${{ inputs.target }} from ${{ github.ref_name }} by @${{ github.actor }}
on:
  workflow_dispatch:
    inputs:
      target:
        default: staging
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	event := strings.Replace(string(pushEvent(t)), `"event": "push"`, `"event": "workflow_dispatch"`, 1)
	event = strings.Replace(event, `"payload": {`, `"payload": {"inputs":{"target":"production"},`, 1)
	compiled, err := CompileWithOptions("deploy.yml", workflow, []byte(event), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Workflow.Name != "Deploy" || ir.Workflow.RunName != "Deploy production from main by @buildkite-gha-smoke" {
		t.Fatalf("workflow presentation = %#v", ir.Workflow)
	}
}

func TestCompileResolvesMissingDispatchInputAsEmptyOnPush(t *testing.T) {
	workflow := []byte(`name: Deploy
run-name: Deploy ${{ inputs.target }} from ${{ github.ref_name }}
on:
  push:
  workflow_dispatch:
    inputs:
      target:
        default: production
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	compiled, err := CompileWithOptions("deploy.yml", workflow, pushEvent(t), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Workflow.RunName != "Deploy  from main" {
		t.Fatalf("push run-name = %q, want missing dispatch input rendered as empty", ir.Workflow.RunName)
	}
}

func TestCompileTreatsBlankRunNameAsAbsentAndLocatesUnsupportedContext(t *testing.T) {
	workflow := []byte("name: CI\nrun-name: '   '\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
	compiled, err := CompileWithOptions("blank.yml", workflow, pushEvent(t), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Workflow.RunName != "" {
		t.Fatalf("blank run-name = %q, want absent", ir.Workflow.RunName)
	}
	workflow = []byte("name: CI\nrun-name: ${{ github.head_ref }}\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
	compiled, err = CompileWithOptions("resolved-blank.yml", workflow, pushEvent(t), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Workflow.RunName != "" {
		t.Fatalf("resolved blank run-name = %q, want absent", ir.Workflow.RunName)
	}

	workflow = []byte("name: CI\nrun-name: Run ${{ vars.TARGET }}\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
	if _, err := Validate("invalid.yml", workflow); err == nil || !strings.Contains(err.Error(), `invalid.yml:2:11: workflow run-name: run-name context "vars" is unavailable`) {
		t.Fatalf("unsupported run-name validation error = %v", err)
	}
	_, err = CompileWithOptions("invalid.yml", workflow, pushEvent(t), defaultOptions())
	if err == nil || !strings.Contains(err.Error(), `invalid.yml:2:11: workflow run-name: run-name context "vars" is unavailable`) {
		t.Fatalf("unsupported run-name error = %v", err)
	}
}

func TestValidateRunNameDoesNotEvaluateRequiredDispatchInputs(t *testing.T) {
	workflow := []byte(`name: Deploy
run-name: Deploy ${{ fromJSON(inputs.config).target }}
on:
  workflow_dispatch:
    inputs:
      config:
        required: true
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	if _, err := Validate("deploy.yml", workflow); err != nil {
		t.Fatalf("event-independent validation evaluated required input: %v", err)
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

func TestCompileAppliesRunnerSelectorToExpressionResolvedLabels(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  test:
    runs-on: ${{ fromJSON(vars.RUNNER_LABELS) }}
    steps:
      - run: true
`)
	target := RunnerTarget{
		Queue:    "linux-medium",
		Platform: PlatformLinuxAMD64,
		Image:    "example.com/toolchains/noble@sha256:" + strings.Repeat("0", 64),
	}
	compiled, err := CompileWithOptions("expression-runner.yml", workflow, pushEvent(t), Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"RUNNER_LABELS": `["self-hosted","custom-linux"]`}},
		Runners: RunnerPolicy{Selectors: []RunnerSelector{{
			Labels: []string{"self-hosted", "custom-linux"},
			Target: target,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || !slices.Equal(ir.Jobs[0].RunsOn, []string{"self-hosted", "custom-linux"}) || ir.Jobs[0].Queue != target.Queue || ir.Jobs[0].RuntimeImage != target.Image {
		t.Fatalf("expression-selected runner = %#v", ir.Jobs)
	}
}

func TestRunsOnPolicyFailsClosedWithLocatedDiagnostics(t *testing.T) {
	tests := []struct {
		name          string
		runsOn        string
		vars          map[string]string
		labels        map[string]string
		want          string
		blockerDetail string
	}{
		{name: "unsupported operating system", runsOn: "windows-latest", labels: map[string]string{"windows-latest": "windows"}, want: `unsupported operating system runner label "windows-latest"`, blockerDetail: "windows-latest"},
		{name: "unsupported operating system among labels", runsOn: "[self-hosted, windows-latest]", labels: map[string]string{"self-hosted": "linux"}, want: `unsupported operating system runner label "windows-latest"`, blockerDetail: "windows-latest"},
		{name: "unmapped label", runsOn: "ubuntu-20.04", labels: map[string]string{"ubuntu-24.04": "linux"}, want: `runner label "ubuntu-20.04" is not mapped by policy`, blockerDetail: "ubuntu-20.04"},
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
			if test.blockerDetail != "" {
				var finding *ProcessingFinding
				if !errors.As(err, &finding) || finding.Blocker != "runner_label" || finding.BlockerDetail != test.blockerDetail {
					t.Fatalf("processing blocker = %#v, want runner_label/%s", finding, test.blockerDetail)
				}
			}
		})
	}
}

func TestEventDerivedRunnerLabelsOmitBlockerDetail(t *testing.T) {
	tests := []struct {
		name     string
		workflow []byte
		event    []byte
	}{
		{
			name: "workflow dispatch input",
			workflow: []byte(`on:
  workflow_dispatch:
    inputs:
      runner:
        type: string
jobs:
  test:
    runs-on: ${{ inputs.runner }}
    steps:
      - run: true
`),
			event: bytes.Replace(readFile(t, smokePath("events", "workflow_dispatch.json")), []byte(`"inputs": {}`), []byte(`"inputs": {"runner": "windows-secret"}`), 1),
		},
		{
			name: "matrix value",
			workflow: []byte(`on: push
jobs:
  test:
    strategy:
      matrix:
        runner:
          - ${{ github.event.runner }}
    runs-on: ${{ matrix.runner }}
    steps:
      - run: true
`),
			event: bytes.Replace(pushEvent(t), []byte(`"payload": {`), []byte(`"payload": {"runner": "windows-secret",`), 1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileWithOptions("policy.yml", test.workflow, test.event, Options{
				EventTrust: EventTrusted,
				Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "linux"}},
			})
			if err == nil {
				t.Fatal("CompileWithOptions() error = nil, want runner policy rejection")
			}
			var finding *ProcessingFinding
			if !errors.As(err, &finding) || finding.Blocker != "runner_label" || finding.BlockerDetail != "" {
				t.Fatalf("processing blocker = %#v, want runner_label with no detail", finding)
			}
		})
	}
}

func TestValidateEventRetainsResolvedRunsOnWhenRunnerPolicyRejectsIt(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: macos-26\n    steps:\n      - run: true\n")
	report, err := ValidateEventWithOptions("policy.yml", workflow, pushEvent(t), Options{
		EventTrust: EventTrusted,
		Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "linux"}},
	})
	if err == nil {
		t.Fatal("ValidateEventWithOptions() error = nil, want runner policy rejection")
	}
	if len(report.Jobs) != 1 || !slices.Equal(report.Jobs[0].RunsOn, []string{"macos-26"}) {
		t.Fatalf("resolved runs-on = %#v", report.Jobs)
	}
}

func TestRunnerRejectionDiagnosticIsActionableWithoutResolvedLabel(t *testing.T) {
	policy := RunnerPolicy{
		Labels:          map[string]string{"ubuntu-24.04": "linux", "self-hosted": "one", "linux": "two", "default": ""},
		Targets:         map[string]RunnerTarget{"macos": {Queue: "macos", Platform: PlatformDarwinARM64}},
		UntrustedQueues: []string{"isolated"},
	}
	tests := []struct {
		name   string
		labels []string
		trust  EventTrust
		want   string
	}{
		{name: "no labels", trust: EventTrusted, want: "Set runs-on"},
		{name: "duplicate label", labels: []string{"ubuntu-24.04", "ubuntu-24.04"}, trust: EventTrusted, want: "Remove duplicate labels"},
		{name: "unsupported operating system", labels: []string{"windows-latest"}, trust: EventTrusted, want: `change runs-on to "ubuntu-latest"`},
		{name: "unmapped label", labels: []string{"macos-15"}, trust: EventTrusted, want: "Configure a mapping"},
		{name: "conflicting queues", labels: []string{"self-hosted", "linux"}, trust: EventTrusted, want: "Use labels that map to one runner target"},
		{name: "conflicting targets", labels: []string{"ubuntu-24.04", "macos"}, trust: EventTrusted, want: "Use labels that map to one runner target"},
		{name: "untrusted default targeting", labels: []string{"default"}, trust: EventUntrusted, want: "Use a runner label allowed for untrusted events"},
		{name: "untrusted queue", labels: []string{"ubuntu-24.04"}, trust: EventUntrusted, want: "ask an administrator to allow its queue"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := policy.resolve(test.labels, test.trust)
			if err == nil {
				t.Fatal("resolve() error = nil, want rejection")
			}
			message, _ := runnerRejectionDiagnostic(err, nil, []string{"ubuntu-latest"}, policy.UntrustedQueues)
			if !strings.Contains(message, test.want) {
				t.Fatalf("runnerRejectionDiagnostic() = %q, want %q", message, test.want)
			}
		})
	}
}

func TestRunnerRejectionDiagnosticOmitsIrrelevantAllowlistForDuplicateLabel(t *testing.T) {
	policy := RunnerPolicy{Labels: map[string]string{"ubuntu-24.04": "linux"}}
	_, err := policy.resolve([]string{"ubuntu-24.04", "ubuntu-24.04"}, EventTrusted)
	if err == nil {
		t.Fatal("resolve() error = nil, want duplicate-label rejection")
	}
	message, detail := runnerRejectionDiagnostic(err, nil, []string{"ubuntu-24.04"}, nil)
	if message != "runs-on contains a duplicate runner label. Remove duplicate labels from runs-on." || detail != "" {
		t.Fatalf("runnerRejectionDiagnostic() = %q, %q", message, detail)
	}
}

func TestRunnerPolicySelectorTargetDoesNotChangeOverlappingLabelTarget(t *testing.T) {
	jammy := RunnerTarget{Platform: PlatformLinuxAMD64, Image: "example.com/toolchains/jammy@sha256:" + strings.Repeat("0", 64)}
	fallback := RunnerTarget{Queue: "linux-medium", Platform: PlatformLinuxAMD64}
	policy := RunnerPolicy{
		Targets: map[string]RunnerTarget{"ubuntu-22.04": jammy},
		Selectors: []RunnerSelector{{
			Labels: []string{"self-hosted", "ubuntu-22.04"},
			Target: fallback,
		}},
	}
	if got, err := policy.resolve([]string{"ubuntu-22.04"}, EventTrusted); err != nil || got != jammy {
		t.Fatalf("standalone preset = %#v, %v", got, err)
	}
	if got, err := policy.resolve([]string{"ubuntu-22.04", "self-hosted"}, EventTrusted); err != nil || got != fallback {
		t.Fatalf("multi-label selector = %#v, %v", got, err)
	}
	if got, err := (RunnerPolicy{Selectors: []RunnerSelector{{Labels: []string{"windows-latest"}, Target: fallback}}}).resolve([]string{"windows-latest"}, EventTrusted); err != nil || got != fallback {
		t.Fatalf("server target = %#v, %v", got, err)
	}
}

func TestRunnerRejectionDiagnosticFallsBackWhenUnclassified(t *testing.T) {
	message, detail := runnerRejectionDiagnostic(errors.New("boom"), nil, nil, nil)
	if message != "Runner target is unsupported. Use a configured Linux or macOS runner target." || detail != "" {
		t.Fatalf("runnerRejectionDiagnostic() = %q, %q", message, detail)
	}
}

func TestRunnerRejectionDiagnosticSeparatesStaticLabelFromAllowlist(t *testing.T) {
	supported := []string{"ubuntu-22.04", "ubuntu-24.04", "ubuntu-latest"}
	tests := []struct {
		label       string
		wantMessage string
		wantDetail  string
	}{
		{
			label:       "windows-latest",
			wantMessage: `Windows runners aren't currently supported. Imported jobs run on Linux or macOS Buildkite hosted agents. If this job can run on Linux, change "windows-latest" to "ubuntu-latest". If it requires Windows, open an issue in https://github.com/buildkite/buildkite-gha to help us prioritize Windows support.`,
		},
		{
			label:       "macos-latest",
			wantMessage: `Runner label "macos-latest" has no runner-target mapping. Configure a mapping for this label or use a mapped runner label.`,
			wantDetail:  "Supported runner labels: ubuntu-22.04, ubuntu-24.04, ubuntu-latest.",
		},
	}
	policy := RunnerPolicy{Targets: map[string]RunnerTarget{
		"ubuntu-22.04":  {Platform: PlatformLinuxAMD64},
		"ubuntu-24.04":  {Platform: PlatformLinuxAMD64},
		"ubuntu-latest": {Platform: PlatformLinuxAMD64},
	}}
	for _, test := range tests {
		_, err := policy.resolve([]string{test.label}, EventTrusted)
		if err == nil {
			t.Fatalf("resolve(%q) error = nil", test.label)
		}
		message, detail := runnerRejectionDiagnostic(err, []string{test.label}, supported, nil)
		if message != test.wantMessage || detail != test.wantDetail {
			t.Fatalf("runnerRejectionDiagnostic(%q) = %q, %q", test.label, message, detail)
		}
		if strings.Contains(message, "ubuntu-22.04") {
			t.Fatalf("runner allowlist leaked into message: %q", message)
		}
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
	if len(plans) != 1 || plans[0].Target.Queue != "gha-untrusted" || plans[0].Schema != plan.Schema {
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
	plans, err := compilePlansForTest(t.Context(), "plan.yml", workflow, pushEvent(t), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), options)
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
