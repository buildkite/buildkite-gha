package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestHostedCompileRequestOptionsCarryEveryInputExactlyOnce(t *testing.T) {
	resolution := agentRunnerResolution{
		selectors:  []compiler.RunnerSelector{{Labels: []string{"self-hosted", "gpu"}, Target: compiler.RunnerTarget{Platform: compiler.PlatformLinuxAMD64, Queue: "gpu"}}},
		rejections: []compiler.RunnerRejection{{Labels: []string{"windows-latest"}, Code: "unsupported_platform", Message: "no"}},
	}
	oidc := &plan.OIDCConfiguration{Claims: []string{"repository"}}
	vars := compiler.VariableSources{Repository: map[string]string{"REGION": "us-east-1"}, Resolved: true}
	request := hostedCompileRequest{
		WorkflowPath:         ".github/workflows/ci.yml",
		WorkflowSource:       []byte("on: push\n"),
		EventSource:          []byte("{}"),
		EventFile:            true,
		Version:              "1.2.3",
		DistributionDigest:   "sha256:" + strings.Repeat("a", 64),
		ImporterStep:         "importer",
		GroupLabel:           "CI",
		StepKeyNamespace:     "ci",
		RunnerTargets:        map[string]compiler.RunnerTarget{"ubuntu-latest": {Platform: compiler.PlatformLinuxAMD64, Queue: "linux"}},
		RunnerResolution:     resolution,
		RuntimeDistributions: map[compiler.Platform]string{compiler.PlatformLinuxAMD64: "sha256:" + strings.Repeat("b", 64)},
		OIDC:                 oidc,
		EnvironmentSource:    stubEnvironmentSource{},
		Vars:                 vars,
	}

	// The expected options are written out from the hosted policy, not
	// derived from the request: untrusted events, the hosted presets with the
	// configured ubuntu-latest override, and every queue a target or a
	// resolved selector names marked untrusted.
	wantTargets := hostedRunnerTargets()
	wantTargets["ubuntu-latest"] = compiler.RunnerTarget{Platform: compiler.PlatformLinuxAMD64, Queue: "linux"}
	wantQueues := []string{"linux", defaultMacOSRunnerQueue, "gpu"}
	want := compiler.Options{
		EventTrust:           compiler.EventUntrusted,
		EventFile:            true,
		GroupLabel:           "CI",
		RuntimeDistributions: request.RuntimeDistributions,
		StepKeyNamespace:     "ci",
		OIDC:                 oidc,
		EnvironmentSource:    stubEnvironmentSource{},
		Vars:                 vars,
		Runners: compiler.RunnerPolicy{
			Targets:                    wantTargets,
			AllowUntrustedDefaultQueue: true,
			Selectors:                  resolution.selectors,
			Rejections:                 resolution.rejections,
		},
	}
	got := request.options()
	if !sameStringSet(got.Runners.UntrustedQueues, wantQueues) {
		t.Errorf("untrusted queues = %q, want %q", got.Runners.UntrustedQueues, wantQueues)
	}
	got.Runners.UntrustedQueues = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options() = %#v\nwant %#v", got, want)
	}

	// Validation shares the runner policy, namespace, variables, and
	// repository source with compilation and omits only the inputs that
	// shape plans rather than admission.
	wantValidation := want
	wantValidation.EventFile = false
	wantValidation.GroupLabel = ""
	wantValidation.RuntimeDistributions = nil
	wantValidation.OIDC = nil
	wantValidation.EnvironmentSource = nil
	gotValidation := request.validationOptions()
	if !sameStringSet(gotValidation.Runners.UntrustedQueues, wantQueues) {
		t.Errorf("validation untrusted queues = %q, want %q", gotValidation.Runners.UntrustedQueues, wantQueues)
	}
	gotValidation.Runners.UntrustedQueues = nil
	if !reflect.DeepEqual(gotValidation, wantValidation) {
		t.Errorf("validationOptions() = %#v\nwant %#v", gotValidation, wantValidation)
	}
}

func TestHostedCompileRequestWithoutResolutionLeavesRunnerPolicyToPresets(t *testing.T) {
	got := hostedCompileRequest{}.options()
	if got.Runners.Selectors != nil || got.Runners.Rejections != nil {
		t.Fatalf("empty resolution produced selectors %#v and rejections %#v", got.Runners.Selectors, got.Runners.Rejections)
	}
	if !reflect.DeepEqual(got.Runners.Targets, hostedRunnerTargets()) {
		t.Fatalf("targets = %#v, want the hosted presets", got.Runners.Targets)
	}
	if !slices.Equal(got.Runners.UntrustedQueues, []string{defaultMacOSRunnerQueue}) {
		t.Fatalf("untrusted queues = %q", got.Runners.UntrustedQueues)
	}
}

func sameStringSet(left, right []string) bool {
	left, right = slices.Clone(left), slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

type stubEnvironmentSource struct{}

func (stubEnvironmentSource) ResolveEnvironment(context.Context, string, string, string) (compiler.EnvironmentProtection, error) {
	return compiler.EnvironmentProtection{}, nil
}

func TestCompileHostedRequestReproducesTheCompilerPipelineForTheSameInputs(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), ".github", "workflows", "matrix.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        go: ["1.25", "1.26"]
    steps:
      - run: echo ${{ matrix.go }}
  test:
    needs: build
    runs-on: ubuntu-latest
    steps: [{run: echo test}]
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("d", 64)
	request := hostedCompileRequest{
		WorkflowPath: workflowPath, WorkflowSource: workflow, EventSource: event,
		Version: "0.0.0-test", DistributionDigest: digest, ImporterStep: "importer", StepKeyNamespace: "0123456789abcdef",
		RuntimeDistributions: map[compiler.Platform]string{compiler.PlatformLinuxAMD64: digest},
	}
	compiled, err := compileHostedRequest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.Admitted || !compiled.JobGraphComplete || len(compiled.Bundle.Plans) != 3 {
		t.Fatalf("compilation = %#v", compiled)
	}

	// The same inputs handed straight to the compiler with the request's
	// options produce byte-identical plans and pipeline, so the request adds
	// no policy of its own.
	options := request.options()
	options.ResolveActions = true
	direct, err := compiler.CompileBundlePlansContext(t.Context(), workflowPath, workflow, event, request.Version, digest, options)
	if err != nil {
		t.Fatal(err)
	}
	direct, err = compiler.GenerateBundlePipeline(direct, digest, request.ImporterStep, options)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(compiled.Bundle.Plans, direct.Plans, func(left, right compiler.PlanArtifact) bool {
		return left.Path == right.Path && left.Digest == right.Digest && string(left.Contents) == string(right.Contents)
	}) {
		t.Fatalf("plans differ from direct compilation:\n%s\n%s", planSummary(compiled.Bundle), planSummary(direct))
	}
	if !reflect.DeepEqual(compiled.Bundle.GeneratedWorkflow, direct.GeneratedWorkflow) {
		t.Fatalf("generated workflow differs from direct compilation:\n%#v\n%#v", compiled.Bundle.GeneratedWorkflow, direct.GeneratedWorkflow)
	}

	// compiledJobs describes each instance the pipeline will contain by the
	// key, logical job, dependencies, and plan digest a later stage compares.
	jobs := compiledJobs(compiled.Bundle)
	// Instance keys are the namespaced job ID plus the first six bytes of the
	// SHA-256 of the canonical matrix row.
	matrixKey := func(row string) string {
		sum := sha256.Sum256([]byte(row))
		return "gha-0123456789abcdef-build-" + hex.EncodeToString(sum[:6])
	}
	wantKeys := []string{matrixKey(`{"go":"1.25"}`), matrixKey(`{"go":"1.26"}`), "gha-0123456789abcdef-test"}
	gotKeys := make([]string, 0, len(jobs))
	for _, job := range jobs {
		gotKeys = append(gotKeys, job.Key)
	}
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("compiled job keys = %q, want %q", gotKeys, wantKeys)
	}
	wantNeeds := slices.Sorted(slices.Values(wantKeys[:2]))
	if jobs[0].LogicalJob != "build" || jobs[2].LogicalJob != "test" || len(jobs[0].Needs) != 0 || !slices.Equal(jobs[2].Needs, wantNeeds) {
		t.Fatalf("compiled jobs = %#v", jobs)
	}
	for i, jobPlan := range compiled.Bundle.Plans {
		if jobs[i].Key != jobPlan.Job.Target.StepKey || jobs[i].PlanDigest != jobPlan.Digest || jobPlan.Digest == "" {
			t.Fatalf("compiled job %d = %#v, plan %s %s", i, jobs[i], jobPlan.Job.Target.StepKey, jobPlan.Digest)
		}
	}
}

func planSummary(bundle compiler.Bundle) string {
	lines := make([]string, 0, len(bundle.Plans))
	for _, jobPlan := range bundle.Plans {
		lines = append(lines, jobPlan.Path+" "+jobPlan.Digest)
	}
	return strings.Join(lines, "\n")
}

func TestCompiledJobsLeaveUnplannedInstancesWithoutDigest(t *testing.T) {
	bundle := compiler.Bundle{
		IR: compiler.IR{Jobs: []compiler.JobInstance{
			{Key: "build", LogicalJobID: "build"},
			{Key: "test", LogicalJobID: "test", Needs: []string{"build"}},
		}},
		Plans: []compiler.PlanArtifact{{Job: plan.Job{Target: plan.Target{StepKey: "build"}}, Digest: "sha256:1"}},
	}
	want := []compiledJob{
		{Key: "build", LogicalJob: "build", Needs: nil, PlanDigest: "sha256:1"},
		{Key: "test", LogicalJob: "test", Needs: []string{"build"}, PlanDigest: ""},
	}
	if got := compiledJobs(bundle); !reflect.DeepEqual(got, want) {
		t.Fatalf("compiledJobs() = %#v, want %#v", got, want)
	}
}

func TestSameExpandedJobGraphComparesKeysAndNeedsOnly(t *testing.T) {
	first := compiler.Bundle{
		IR: compiler.IR{JobGraphComplete: true, Jobs: []compiler.JobInstance{
			{Key: "build", LogicalJobID: "build"},
			{Key: "test", LogicalJobID: "test", Needs: []string{"build"}},
		}},
		Plans: []compiler.PlanArtifact{{Job: plan.Job{Target: plan.Target{StepKey: "build"}}, Digest: "sha256:1"}},
	}
	// Different plan digests are the expected outcome of resolving action
	// variables and must not fail the comparison.
	revised := first
	revised.Plans = []compiler.PlanArtifact{{Job: plan.Job{Target: plan.Target{StepKey: "build"}}, Digest: "sha256:2"}}
	if !sameExpandedJobGraph(first, revised) {
		t.Fatal("plan digest change reported as a different job graph")
	}
	rewired := first
	rewired.IR.Jobs = []compiler.JobInstance{{Key: "build", LogicalJobID: "build"}, {Key: "test", LogicalJobID: "test"}}
	if sameExpandedJobGraph(first, rewired) {
		t.Fatal("dropped dependency reported as the same job graph")
	}
	renamed := first
	renamed.IR.Jobs = []compiler.JobInstance{{Key: "build", LogicalJobID: "build"}, {Key: "tests", LogicalJobID: "test", Needs: []string{"build"}}}
	if sameExpandedJobGraph(first, renamed) {
		t.Fatal("renamed instance reported as the same job graph")
	}
	incomplete := first
	incomplete.IR.JobGraphComplete = false
	if sameExpandedJobGraph(first, incomplete) || sameExpandedJobGraph(incomplete, first) {
		t.Fatal("incomplete job graph reported as the same job graph")
	}
}
