package compiler

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
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

func TestCompileEvaluatesGitHubEventVarsAndMatrixForRunsOn(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  test:
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

func TestDefaultCompilePolicyIsUntrustedAndFixedToTokenlessQueue(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	compiled, err := Compile("default.yml", workflow, pushEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Event.Trust != EventUntrusted || ir.Jobs[0].Queue != "gha-untrusted" {
		t.Fatalf("default compiler trust boundary = event %q, queue %q", ir.Event.Trust, ir.Jobs[0].Queue)
	}
	_, err = CompilePlans("default.yml", workflow, pushEvent(t), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "privileged")
	if err == nil || !strings.Contains(err.Error(), `unattested event snapshots may only target queue "gha-untrusted"`) {
		t.Fatalf("CompilePlans() privileged default error = %v", err)
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
	plans, err := CompilePlansWithOptions("plan.yml", workflow, pushEvent(t), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), options)
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
