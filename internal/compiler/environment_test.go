package compiler

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironmentSecretPrefix(t *testing.T) {
	cases := map[string]string{
		"production":    "PRODUCTION",
		"staging-eu/2":  "STAGING_EU_2",
		"Deploy Target": "DEPLOY_TARGET",
		"1st":           "_1ST",
		"_internal":     "_INTERNAL",
		"préproduction": "PR_PRODUCTION",
		"":              "_",
	}
	for environment, want := range cases {
		if got := EnvironmentSecretPrefix(environment); got != want {
			t.Errorf("EnvironmentSecretPrefix(%q) = %q, want %q", environment, got, want)
		}
	}
}

func TestEnvironmentGateKey(t *testing.T) {
	key := environmentGateKey("", "production")
	if !strings.HasPrefix(key, "gha-approve-production-") || len(key) != len("gha-approve-production-")+12 {
		t.Fatalf("gate key = %q", key)
	}
	if key != environmentGateKey("", "production") {
		t.Fatal("gate key is not deterministic")
	}
	namespaced := environmentGateKey("0123456789abcdef", "production")
	if !strings.HasPrefix(namespaced, "gha-0123456789abcdef-approve-production-") {
		t.Fatalf("namespaced gate key = %q", namespaced)
	}
	// Different environments that sanitize identically stay distinct.
	if environmentGateKey("", "staging/eu") == environmentGateKey("", "staging.eu") {
		t.Fatal("distinct environments share a gate key")
	}
	// GitHub environment names are case-insensitive, so case variants share one key.
	if environmentGateKey("", "Production") != environmentGateKey("", "production") {
		t.Fatal("case variants of one environment have distinct gate keys")
	}
	long := environmentGateKey("", strings.Repeat("a", 200))
	if len(long) > 100 || !strings.HasPrefix(long, "gha-approve-"+strings.Repeat("a", 64)) {
		t.Fatalf("long environment gate key = %q", long)
	}
	odd := environmentGateKey("", "日本語")
	if !strings.HasPrefix(odd, "gha-approve-environment-") {
		t.Fatalf("unsanitizable environment gate key = %q", odd)
	}
}

func TestEnvironmentScopedSecrets(t *testing.T) {
	secrets, mappings, err := environmentScopedSecrets("production", []string{"DEPLOY_KEY"}, []string{"DEPLOY_KEY", "SHARED"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(secrets, []string{"PRODUCTION_DEPLOY_KEY", "SHARED"}) {
		t.Fatalf("scoped secrets = %#v", secrets)
	}
	if !reflect.DeepEqual(mappings, map[string]string{"DEPLOY_KEY": "PRODUCTION_DEPLOY_KEY", "SHARED": "SHARED"}) {
		t.Fatalf("scoped mappings = %#v", mappings)
	}

	unchanged, unchangedMappings, err := environmentScopedSecrets("production", []string{"UNUSED"}, []string{"SHARED"}, nil)
	if err != nil || !reflect.DeepEqual(unchanged, []string{"SHARED"}) || unchangedMappings != nil {
		t.Fatalf("no-op rewrite = %#v, %#v, %v", unchanged, unchangedMappings, err)
	}

	if _, _, err := environmentScopedSecrets("production", []string{"KEY"}, []string{"KEY", "PRODUCTION_KEY"}, nil); err == nil || !strings.Contains(err.Error(), "both resolve to Buildkite secret") {
		t.Fatalf("collision error = %v", err)
	}

	if _, _, err := environmentScopedSecrets("production", []string{"KEY"}, []string{"KEY"}, map[string]string{"KEY": "OTHER"}); err == nil || !strings.Contains(err.Error(), "cannot rescope already-mapped secrets") {
		t.Fatalf("composed mapping error = %v", err)
	}
}

func TestEnvironmentScopedSecretsRejectUnstorableKeys(t *testing.T) {
	// Buildkite secret keys cannot begin with BK or BUILDKITE in any case.
	for _, environment := range []string{"bk-production", "BUILDKITE-prod"} {
		if _, _, err := environmentScopedSecrets(environment, []string{"DEPLOY_KEY"}, []string{"DEPLOY_KEY"}, nil); err == nil || !strings.Contains(err.Error(), "cannot begin with BK or BUILDKITE") {
			t.Fatalf("environmentScopedSecrets(%q) error = %v, want reserved prefix error", environment, err)
		}
	}
	// An environment that merely defines a secret without the job
	// referencing it generates no key, so it is not rejected.
	secrets, mappings, err := environmentScopedSecrets("bk-production", []string{"UNUSED"}, []string{"SHARED"}, nil)
	if err != nil || !reflect.DeepEqual(secrets, []string{"SHARED"}) || mappings != nil {
		t.Fatalf("unreferenced rewrite = %#v, %#v, %v", secrets, mappings, err)
	}
	// Buildkite secret keys are limited to 255 characters.
	long := strings.Repeat("e", 250)
	if _, _, err := environmentScopedSecrets(long, []string{"DEPLOY_KEY"}, []string{"DEPLOY_KEY"}, nil); err == nil || !strings.Contains(err.Error(), "255-character secret key limit") {
		t.Fatalf("long key error = %v, want length error", err)
	}
	if _, _, err := environmentScopedSecrets(strings.Repeat("e", 244), []string{"DEPLOY_KEY"}, []string{"DEPLOY_KEY"}, nil); err != nil {
		t.Fatalf("255-character key rejected: %v", err)
	}
}

// fakeEnvironmentSource resolves environments from a fixed table and counts
// resolutions so tests can assert memoization.
type fakeEnvironmentSource struct {
	protections map[string]EnvironmentProtection
	errs        map[string]error
	calls       map[string]int
}

func (f *fakeEnvironmentSource) ResolveEnvironment(_ context.Context, owner, repository, name string) (EnvironmentProtection, error) {
	if owner != "buildkite" || repository != "buildkite-gha" {
		return EnvironmentProtection{}, fmt.Errorf("unexpected repository %s/%s", owner, repository)
	}
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
	if err, exists := f.errs[name]; exists {
		return EnvironmentProtection{}, err
	}
	protection, exists := f.protections[name]
	if !exists {
		return EnvironmentProtection{}, fmt.Errorf("environments: environment not found (404); create the environment or use a token that can read the repository's environments")
	}
	return protection, nil
}

func compileEnvironmentBundle(t *testing.T, workflow string, options Options) (Bundle, error) {
	t.Helper()
	return CompileBundleWithOptions("deploy.yml", []byte(workflow), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", options)
}

const environmentWorkflow = `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps: [{run: true}]
  deploy:
    needs: build
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: echo "$DEPLOY_KEY" "$SHARED"
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          SHARED: ${{ secrets.SHARED }}
`

func TestCompileBundleEnvironmentApprovalGateAndSecretScope(t *testing.T) {
	options := defaultOptions()
	source := &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"production": {RequiredReviewers: true, SecretNames: []string{"DEPLOY_KEY"}},
	}}
	options.EnvironmentSource = source
	bundle, err := compileEnvironmentBundle(t, environmentWorkflow, options)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := environmentGateKey("", "production")
	if len(bundle.GeneratedWorkflow.ApprovalGates) != 1 || bundle.GeneratedWorkflow.ApprovalGates[0].Key != wantKey || bundle.GeneratedWorkflow.ApprovalGates[0].Environment != "production" {
		t.Fatalf("approval gates = %#v", bundle.GeneratedWorkflow.ApprovalGates)
	}
	if len(bundle.GeneratedWorkflow.Jobs) != 2 || bundle.GeneratedWorkflow.Jobs[0].ApprovalGate != "" || bundle.GeneratedWorkflow.Jobs[1].ApprovalGate != wantKey {
		t.Fatalf("job approval gates = %q, %q", bundle.GeneratedWorkflow.Jobs[0].ApprovalGate, bundle.GeneratedWorkflow.Jobs[1].ApprovalGate)
	}
	deploy := bundle.Plans[1].Job
	if !reflect.DeepEqual(deploy.RequiredSecrets, []string{"PRODUCTION_DEPLOY_KEY", "SHARED"}) {
		t.Fatalf("deploy secrets = %#v", deploy.RequiredSecrets)
	}
	if !reflect.DeepEqual(deploy.SecretMappings, map[string]string{"DEPLOY_KEY": "PRODUCTION_DEPLOY_KEY", "SHARED": "SHARED"}) {
		t.Fatalf("deploy secret mappings = %#v", deploy.SecretMappings)
	}
	pipeline := string(bundle.Pipeline)
	if !strings.Contains(pipeline, `- block: ":github: approval · production"`) || !strings.Contains(pipeline, `key: "`+wantKey+`"`) || !strings.Contains(pipeline, "blocked_state: running") {
		t.Fatalf("pipeline block step missing:\n%s", pipeline)
	}
	if source.calls["production"] != 1 {
		t.Fatalf("environment resolved %d times", source.calls["production"])
	}
}

func TestCompileBundleEnvironmentWithoutReviewersEmitsNoGate(t *testing.T) {
	options := defaultOptions()
	options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"production": {SecretNames: []string{"DEPLOY_KEY"}},
	}}
	bundle, err := compileEnvironmentBundle(t, environmentWorkflow, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.GeneratedWorkflow.ApprovalGates) != 0 || bundle.GeneratedWorkflow.Jobs[1].ApprovalGate != "" {
		t.Fatalf("unexpected approval gates = %#v", bundle.GeneratedWorkflow.ApprovalGates)
	}
	if !reflect.DeepEqual(bundle.Plans[1].Job.RequiredSecrets, []string{"PRODUCTION_DEPLOY_KEY", "SHARED"}) {
		t.Fatalf("deploy secrets = %#v", bundle.Plans[1].Job.RequiredSecrets)
	}
	// Only gated jobs disable manual retry.
	if strings.Contains(string(bundle.Pipeline), "retry:") {
		t.Fatalf("ungated environment job disabled retry:\n%s", bundle.Pipeline)
	}
}

func TestCompileBundleEnvironmentGateKeyUsesStepKeyNamespace(t *testing.T) {
	options := defaultOptions()
	options.StepKeyNamespace = "0123456789abcdef"
	options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"production": {RequiredReviewers: true},
	}}
	bundle, err := compileEnvironmentBundle(t, environmentWorkflow, options)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := environmentGateKey("0123456789abcdef", "production")
	if len(bundle.GeneratedWorkflow.ApprovalGates) != 1 || bundle.GeneratedWorkflow.ApprovalGates[0].Key != wantKey {
		t.Fatalf("namespaced approval gates = %#v", bundle.GeneratedWorkflow.ApprovalGates)
	}
}

func TestCompileBundleMatrixInstancesShareEnvironmentGate(t *testing.T) {
	options := defaultOptions()
	source := &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"production": {RequiredReviewers: true},
	}}
	options.EnvironmentSource = source
	workflow := `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    strategy:
      matrix:
        region: [eu, us]
    steps:
      - run: echo "${{ matrix.region }}"
`
	bundle, err := compileEnvironmentBundle(t, workflow, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.GeneratedWorkflow.ApprovalGates) != 1 {
		t.Fatalf("approval gates = %#v", bundle.GeneratedWorkflow.ApprovalGates)
	}
	wantKey := bundle.GeneratedWorkflow.ApprovalGates[0].Key
	if len(bundle.GeneratedWorkflow.Jobs) != 2 {
		t.Fatalf("matrix jobs = %d", len(bundle.GeneratedWorkflow.Jobs))
	}
	for _, job := range bundle.GeneratedWorkflow.Jobs {
		if job.ApprovalGate != wantKey {
			t.Fatalf("matrix job %q approval gate = %q", job.Key, job.ApprovalGate)
		}
	}
	if source.calls["production"] != 1 {
		t.Fatalf("environment resolved %d times", source.calls["production"])
	}
}

func TestCompileBundleRejectsUnsupportedEnvironmentProtection(t *testing.T) {
	cases := []struct {
		name       string
		protection EnvironmentProtection
		want       string
	}{
		{"wait timer", EnvironmentProtection{WaitTimerMinutes: 30}, "sets a 30-minute wait timer"},
		{"branch policy", EnvironmentProtection{BranchPolicy: true}, "restricts deployment branches"},
		{"custom rules", EnvironmentProtection{UnsupportedRules: []string{"datadog"}}, "unsupported protection rules (datadog)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := defaultOptions()
			options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{"production": testCase.protection}}
			bundle, err := compileEnvironmentBundle(t, environmentWorkflow, options)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("CompileBundleWithOptions() error = %v, want %q", err, testCase.want)
			}
			if len(bundle.Pipeline) != 0 {
				t.Fatal("rejected environment still generated a pipeline")
			}
		})
	}
}

func TestCompileBundleRequiresEnvironmentSource(t *testing.T) {
	bundle, err := compileEnvironmentBundle(t, environmentWorkflow, defaultOptions())
	if err == nil || !strings.Contains(err.Error(), "resolve only through the job-scoped Buildkite Agent API") {
		t.Fatalf("CompileBundleWithOptions() error = %v, want missing environment source rejection", err)
	}
	if len(bundle.Pipeline) != 0 {
		t.Fatal("missing environment source still generated a pipeline")
	}
}

func TestCompileBundleSurfacesEnvironmentResolutionErrors(t *testing.T) {
	options := defaultOptions()
	options.EnvironmentSource = &fakeEnvironmentSource{errs: map[string]error{
		"production": fmt.Errorf("environments: the token lacks permission (403); grant it read access to the repository's environments and environment secrets"),
	}}
	_, err := compileEnvironmentBundle(t, environmentWorkflow, options)
	if err == nil || !strings.Contains(err.Error(), `job "deploy" environment "production"`) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("CompileBundleWithOptions() error = %v, want attributed resolution failure", err)
	}
}

func TestCompileBundleRejectsEnvironmentInReusableWorkflow(t *testing.T) {
	repository := t.TempDir()
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps: [{run: true}]
`)
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
`)
	options := defaultOptions()
	options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{"production": {}}}
	_, err := CompileBundleWithOptions(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", options)
	if err == nil || !strings.Contains(err.Error(), "inside a reusable workflow") {
		t.Fatalf("CompileBundleWithOptions() error = %v, want reusable workflow environment rejection", err)
	}
}

func TestCompileBundleRejectsEnvironmentInReusableWorkflowWithInheritedSecrets(t *testing.T) {
	repository := t.TempDir()
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps: [{run: true}]
`)
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    secrets: inherit
`)
	options := defaultOptions()
	options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{"production": {}}}
	_, err := CompileBundleWithOptions(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", options)
	if err == nil || !strings.Contains(err.Error(), "inside a reusable workflow") {
		t.Fatalf("CompileBundleWithOptions() error = %v, want reusable workflow environment rejection", err)
	}
}

func TestCompileBundleEnvironmentNamesAreCaseInsensitive(t *testing.T) {
	options := defaultOptions()
	source := &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"Production": {RequiredReviewers: true},
	}}
	options.EnvironmentSource = source
	bundle, err := compileEnvironmentBundle(t, `on: push
jobs:
  east:
    runs-on: ubuntu-latest
    environment: Production
    steps: [{run: true}]
  west:
    runs-on: ubuntu-latest
    environment: production
    steps: [{run: true}]
`, options)
	if err != nil {
		t.Fatal(err)
	}
	// Case variants of one GitHub environment resolve once and share one
	// approval gate labeled with the first authored spelling.
	if source.calls["Production"] != 1 || len(source.calls) != 1 {
		t.Fatalf("environment resolutions = %#v", source.calls)
	}
	gates := bundle.GeneratedWorkflow.ApprovalGates
	if len(gates) != 1 || gates[0].Environment != "Production" {
		t.Fatalf("approval gates = %#v", gates)
	}
	jobs := bundle.GeneratedWorkflow.Jobs
	if jobs[0].ApprovalGate == "" || jobs[0].ApprovalGate != jobs[1].ApprovalGate {
		t.Fatalf("job approval gates = %q, %q", jobs[0].ApprovalGate, jobs[1].ApprovalGate)
	}
}

func TestCompileBundleRejectsEnvironmentSecretPrefixCollision(t *testing.T) {
	options := defaultOptions()
	options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"prod-us": {},
		"prod/us": {},
	}}
	_, err := compileEnvironmentBundle(t, `on: push
jobs:
  east:
    runs-on: ubuntu-latest
    environment: prod-us
    steps: [{run: true}]
  west:
    runs-on: ubuntu-latest
    environment: prod/us
    steps: [{run: true}]
`, options)
	if err == nil || !strings.Contains(err.Error(), `both resolve to Buildkite secret prefix "PROD_US"`) {
		t.Fatalf("CompileBundleWithOptions() error = %v, want secret prefix collision rejection", err)
	}
}

// fakeEnvironmentBatchSource resolves environments from a fixed table through
// the batch interface, recording batch requests and any stray single-name
// resolutions so tests can assert the compiler prefers one batch request.
type fakeEnvironmentBatchSource struct {
	protections map[string]EnvironmentProtection
	batchErr    error
	batches     [][]string
	singleCalls int
}

func (f *fakeEnvironmentBatchSource) ResolveEnvironment(context.Context, string, string, string) (EnvironmentProtection, error) {
	f.singleCalls++
	return EnvironmentProtection{}, fmt.Errorf("single-name resolution must not be used when batching is available")
}

func (f *fakeEnvironmentBatchSource) ResolveEnvironments(_ context.Context, owner, repository string, names []string) ([]EnvironmentProtection, error) {
	f.batches = append(f.batches, append([]string(nil), names...))
	if owner != "buildkite" || repository != "buildkite-gha" {
		return nil, fmt.Errorf("unexpected repository %s/%s", owner, repository)
	}
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	protections := make([]EnvironmentProtection, 0, len(names))
	for _, name := range names {
		protection, exists := f.protections[name]
		if !exists {
			return nil, fmt.Errorf("environments: environment not found (404)")
		}
		protections = append(protections, protection)
	}
	return protections, nil
}

const twoEnvironmentWorkflow = `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps: [{run: true}]
  stage:
    runs-on: ubuntu-latest
    environment: staging
    steps: [{run: true}]
`

func TestReportBatchEnvironmentNames(t *testing.T) {
	report := Report{
		Jobs: []JobInstance{
			{Key: "deploy", Environment: "production"},
			{Key: "deploy-again", Environment: "Production"},
			{Key: "blocked", Environment: "canary"},
			{Key: "target-a", Environment: "deploy target"},
			{Key: "target-b", Environment: "deploy-target"},
			{Key: "promote", Environment: "staging"},
			{Key: "build"},
		},
		NotEvaluatedInstances: map[string]bool{"blocked": true},
	}
	event := Event{Provider: "github"}
	names := report.BatchEnvironmentNames(event)
	if !reflect.DeepEqual(names, []string{"production", "deploy target", "staging"}) {
		t.Fatalf("BatchEnvironmentNames() = %#v, want first spellings without not-evaluated instances and prefix collisions", names)
	}
	if names := report.BatchEnvironmentNames(Event{Provider: "gitlab"}); names != nil {
		t.Fatalf("BatchEnvironmentNames() for a non-GitHub event = %#v, want nil", names)
	}
}

func TestCompileBundleBatchesEnvironmentResolutions(t *testing.T) {
	options := defaultOptions()
	source := &fakeEnvironmentBatchSource{protections: map[string]EnvironmentProtection{
		"production": {RequiredReviewers: true},
		"staging":    {},
	}}
	options.EnvironmentSource = source
	bundle, err := compileEnvironmentBundle(t, twoEnvironmentWorkflow, options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source.batches, [][]string{{"production", "staging"}}) {
		t.Fatalf("batch requests = %#v, want one batch of both names", source.batches)
	}
	if source.singleCalls != 0 {
		t.Fatalf("single-name resolutions = %d, want 0", source.singleCalls)
	}
	gates := bundle.GeneratedWorkflow.ApprovalGates
	if len(gates) != 1 || gates[0].Environment != "production" {
		t.Fatalf("approval gates = %#v", gates)
	}
}

func TestCompileBundleBatchExcludesSecretPrefixCollisions(t *testing.T) {
	options := defaultOptions()
	source := &fakeEnvironmentBatchSource{protections: map[string]EnvironmentProtection{"prod-us": {}}}
	options.EnvironmentSource = source
	_, err := compileEnvironmentBundle(t, `on: push
jobs:
  east:
    runs-on: ubuntu-latest
    environment: prod-us
    steps: [{run: true}]
  west:
    runs-on: ubuntu-latest
    environment: prod/us
    steps: [{run: true}]
`, options)
	if err == nil || !strings.Contains(err.Error(), `both resolve to Buildkite secret prefix "PROD_US"`) {
		t.Fatalf("CompileBundleWithOptions() error = %v, want secret prefix collision rejection", err)
	}
	if !reflect.DeepEqual(source.batches, [][]string{{"prod-us"}}) {
		t.Fatalf("batch requests = %#v, want the colliding name excluded", source.batches)
	}
}

func TestCompileBundleBatchFailureAttributesEveryJob(t *testing.T) {
	options := defaultOptions()
	source := &fakeEnvironmentBatchSource{batchErr: fmt.Errorf("environments: the Agent API rate limit was hit (429); retry after 60s")}
	options.EnvironmentSource = source
	_, err := compileEnvironmentBundle(t, twoEnvironmentWorkflow, options)
	if err == nil {
		t.Fatal("CompileBundleWithOptions() succeeded, want batch failure")
	}
	for _, want := range []string{
		`job "deploy" environment "production": environments: the Agent API rate limit was hit (429)`,
		`job "stage" environment "staging": environments: the Agent API rate limit was hit (429)`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("CompileBundleWithOptions() error = %v, missing %q", err, want)
		}
	}
	if len(source.batches) != 1 {
		t.Fatalf("batch requests = %#v, want exactly one attempt", source.batches)
	}
}
