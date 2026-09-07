package compiler

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
)

// TestValidateReportsVarsReferences proves the validation report tells callers
// whether a workflow reads the vars context anywhere, in any access form,
// including inside a reusable workflow it calls. Callers resolve repository
// and organization variables only when it does.
func TestValidateReportsVarsReferences(t *testing.T) {
	for name, test := range map[string]struct {
		workflow string
		want     bool
	}{
		"no references": {`on: push
env:
  GREETING: ${{ github.event_name }}
jobs:
  test:
    runs-on: ubuntu-latest
    if: github.ref == 'refs/heads/main'
    steps:
      - run: echo "${{ github.sha }}" "$VARS" "vars.NOT_AN_EXPRESSION"
        env:
          VARS: literal
`, false},
		"compile-time matrix": {`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: ${{ fromJSON(vars.VERSIONS) }}
    steps: [{run: true}]
`, true},
		"compile-time runs-on": {`on: push
jobs:
  test:
    runs-on: ${{ vars.RUNNER }}
    steps: [{run: true}]
`, true},
		"workflow concurrency": {`on: push
concurrency: deploy-${{ vars.TARGET }}
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`, true},
		"runtime step": {`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ vars.REGION }}"
`, true},
		"bare job condition": {`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    if: vars.DEPLOY_ENABLED == 'true'
    steps: [{run: true}]
`, true},
		"index access": {`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ vars['REGION'] }}"
`, true},
		"dynamic access": {`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        name: [A, B]
    steps:
      - run: echo "${{ vars[matrix.name] }}"
`, true},
		"whole context": {`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo '${{ toJSON(vars) }}'
`, true},
		"service credentials": {`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    services:
      db:
        image: ghcr.io/acme/db
        credentials:
          username: ${{ vars.REGISTRY_USER }}
          password: ${{ secrets.REGISTRY_PASSWORD }}
    steps: [{run: true}]
`, true},
	} {
		t.Run(name, func(t *testing.T) {
			// Compile-time positions fail without the scopes, and that is
			// exactly when callers need the report to say vars are referenced.
			report, _ := Validate("vars.yml", []byte(test.workflow))
			if report.ReferencesVars != test.want {
				t.Fatalf("ReferencesVars = %v, want %v", report.ReferencesVars, test.want)
			}
		})
	}
}

// TestResolvedVarsDoNotNarrowAuthorityPlanning proves resolved repository
// and organization variables reach compile-time fields and the plan without
// narrowing token or secret authority: a step whose condition a known
// variable makes false still requests the token and secret it names, because
// authority planning treats vars as unknown.
func TestResolvedVarsDoNotNarrowAuthorityPlanning(t *testing.T) {
	options := Options{
		EventTrust: EventTrusted,
		Vars: VariableSources{
			Organization: map[string]string{"PUBLISH": "false", "RUNNER": "ubuntu-22.04"},
			Repository:   map[string]string{"publish": "true"},
		},
		Runners: RunnerPolicy{Labels: map[string]string{"ubuntu-22.04": "linux"}},
	}
	plans, err := compilePlansForTest(t.Context(), "authority.yml", []byte(`on: push
jobs:
  release:
    runs-on: ${{ vars.RUNNER }}
    if: vars.PUBLISH == 'true'
    steps:
      - if: vars.PUBLISH == 'false'
        run: echo "${{ secrets.GITHUB_TOKEN }}" "${{ secrets.NPM_TOKEN }}"
`), pushEvent(t), "0.0.0-test", "sha256:"+strings.Repeat("3", 64), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want the vars-gated job kept for runtime evaluation", len(plans))
	}
	job := plans[0]
	if job.Target.Queue != "linux" {
		t.Fatalf("plan queue = %q, want the vars-selected runner", job.Target.Queue)
	}
	if job.GitHubToken == nil || !slices.Contains(job.RequiredSecrets, "NPM_TOKEN") {
		t.Fatalf("known-false vars condition narrowed authority: token %#v, secrets %#v", job.GitHubToken, job.RequiredSecrets)
	}
	if !reflect.DeepEqual(job.RepositoryVars, options.Vars.Repository) || !reflect.DeepEqual(job.OrganizationVars, options.Vars.Organization) {
		t.Fatalf("plan scopes = repository %#v, organization %#v", job.RepositoryVars, job.OrganizationVars)
	}
}

// TestKnownFalseVarsConditionsKeepAuthority proves a job condition or a
// reusable-workflow call guard that a resolved variable makes false neither
// prunes the job's token authority nor collapses to a literal: vars stay
// residual in the plan so the runtime evaluates them with the plan's
// pre-environment scopes, and planning keeps requesting the token.
func TestKnownFalseVarsConditionsKeepAuthority(t *testing.T) {
	options := defaultOptions()
	options.Vars = VariableSources{Repository: map[string]string{"PUBLISH": "false"}}
	repository := t.TempDir()
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  release:
    runs-on: ubuntu-latest
    if: github.event_name == 'push' && vars.PUBLISH == 'true'
    steps:
      - run: echo "${{ secrets.GITHUB_TOKEN }}"
  delegated:
    if: vars.PUBLISH == 'true'
    uses: ./.github/workflows/reusable.yml
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  action:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ secrets.GITHUB_TOKEN }}"
`)
	plans, err := compilePlansForTest(t.Context(), caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want both vars-gated jobs kept for runtime evaluation", len(plans))
	}
	for _, job := range plans {
		if job.GitHubToken == nil {
			t.Fatalf("job %q: known-false vars condition pruned token authority: %#v", job.Workflow.LogicalJobID, job)
		}
		condition := job.Program.Job.Condition.Source
		for _, guard := range job.Program.Job.Guards {
			condition += " " + guard.Condition.Source
		}
		if !strings.Contains(strings.ToLower(condition), "vars.publish") {
			t.Fatalf("job %q: vars condition was resolved at compile time: %q", job.Workflow.LogicalJobID, condition)
		}
	}
}

func TestValidateReportsVarsReferencesInCalledReusableWorkflow(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
    with:
      message: hello
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      message:
        type: string
        required: true
jobs:
  first:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ inputs.message }} ${{ vars.REGION }}"
`)
	report, err := Validate(callerPath, readFile(t, callerPath))
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReferencesVars {
		t.Fatal("ReferencesVars = false for a caller whose reusable workflow reads vars")
	}
}

// TestActionsReferenceVars proves a compiled bundle reports a vars reference
// that lives only in a resolved action's metadata, so callers can resolve the
// scopes after compilation discovers it, and stays quiet otherwise.
func TestActionsReferenceVars(t *testing.T) {
	site := func(source string) *program.Site { return &program.Site{Source: source} }
	bundle := func(sources ...string) Bundle {
		actions := map[string]program.Action{}
		for i, source := range sources {
			actions[fmt.Sprintf("action-%d", i)] = program.Action{Inputs: []program.ActionInput{{Name: "region", Default: site(source)}}}
		}
		return Bundle{Plans: []PlanArtifact{{Job: plan.Job{}}, {Job: plan.Job{Program: &program.Program{Actions: actions}}}}}
	}
	if ActionsReferenceVars(bundle("${{ github.token }}", "literal", "vars.NOT_AN_EXPRESSION")) {
		t.Fatal("ActionsReferenceVars() = true without a vars reference")
	}
	if !ActionsReferenceVars(bundle("${{ github.token }}", "${{ vars.AWS_REGION }}")) {
		t.Fatal("ActionsReferenceVars() = false with an input default reading vars")
	}
	composite := func(condition string) Bundle {
		action := program.Action{Steps: []program.ActionStep{{Condition: program.Site{Source: condition}, Run: &program.ActionRun{Command: program.Site{Source: "echo ok"}}}}}
		return Bundle{Plans: []PlanArtifact{{Job: plan.Job{Program: &program.Program{Actions: map[string]program.Action{"composite": action}}}}}}
	}
	if !ActionsReferenceVars(composite("vars.ENABLED == 'true'")) {
		t.Fatal("ActionsReferenceVars() = false with a bare composite step condition reading vars")
	}
	if ActionsReferenceVars(composite("github.event_name == 'push'")) {
		t.Fatal("ActionsReferenceVars() = true with a bare composite step condition that does not read vars")
	}
}
