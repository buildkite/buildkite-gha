package compiler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

// probeRegion is a distinctive variable value so tests can prove where it
// appears: in the vars context of jobs declaring the environment, nowhere
// else.
const probeRegion = "eu-west-1-probe-value"

func environmentVariablesOptions(t *testing.T, base map[string]string) Options {
	t.Helper()
	options := defaultOptions()
	options.Vars.Repository = base
	options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"production": {SecretNames: []string{"DEPLOY_KEY"}, Variables: map[string]string{"REGION": probeRegion, "TIER": "gold"}},
		"staging":    {},
	}}
	return options
}

func TestCompileBundleScopesEnvironmentVariablesPerJob(t *testing.T) {
	bundle, err := compileEnvironmentBundle(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps: [{run: true}]
  deploy:
    runs-on: ubuntu-latest
    environment: production
    env:
      REGION: ${{ vars.REGION }}
    outputs:
      tier: ${{ vars.TIER }}
    defaults:
      run:
        working-directory: ${{ vars.REGION }}
    steps:
      - if: vars.tier == 'gold'
        run: echo "${{ vars['REGION'] }}" "$DEPLOY_KEY"
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
      - uses: actions/checkout@v4
        with:
          ref: ${{ vars.REGION }}
  verify:
    runs-on: ubuntu-latest
    environment: Production
    strategy:
      matrix:
        shard: [1, 2]
    steps: [{run: 'echo ${{ vars.region }}'}]
  stage:
    runs-on: ubuntu-latest
    environment: staging
    steps: [{run: true}]
`, environmentVariablesOptions(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"build":  nil,
		"deploy": {"REGION": probeRegion, "TIER": "gold"},
		"verify": {"REGION": probeRegion, "TIER": "gold"},
		"stage":  nil,
	}
	if len(bundle.Plans) != 5 {
		t.Fatalf("plans = %d, want 5", len(bundle.Plans))
	}
	for _, artifact := range bundle.Plans {
		job := artifact.Job
		if !reflect.DeepEqual(job.EnvironmentVars, want[job.Workflow.LogicalJobID]) || job.RepositoryVars != nil || job.OrganizationVars != nil {
			t.Fatalf("job %q vars = %#v (repository %#v, organization %#v), want environment %#v only", job.Workflow.LogicalJobID, job.EnvironmentVars, job.RepositoryVars, job.OrganizationVars, want[job.Workflow.LogicalJobID])
		}
		encoded, err := plan.Encode(job)
		if err != nil {
			t.Fatal(err)
		}
		// Values are never inlined: the plan carries them in environment_vars
		// only, and runner-evaluated templates keep their expressions.
		if occurrences := strings.Count(string(encoded), probeRegion); occurrences != len(job.EnvironmentVars)/2 {
			t.Fatalf("job %q plan mentions the variable value %d times:\n%s", job.Workflow.LogicalJobID, occurrences, encoded)
		}
	}
	deploy := bundle.Plans[1].Job
	if deploy.Env["REGION"] != "${{ vars.REGION }}" || deploy.Outputs["tier"] != "${{ vars.TIER }}" || deploy.Steps[0].Condition != "vars.tier == 'gold'" {
		t.Fatalf("deploy templates were rewritten: env=%#v outputs=%#v condition=%q", deploy.Env, deploy.Outputs, deploy.Steps[0].Condition)
	}
	if strings.Contains(string(bundle.Pipeline), probeRegion) {
		t.Fatalf("variable value leaked into the pipeline:\n%s", bundle.Pipeline)
	}
}

// TestCompileBundleKeepsVariableScopesSeparate proves the plan carries each
// scope unmerged so the runtime can apply GitHub's precedence per position:
// repository variables in jobs.<id>.if, environment variables laid over them
// in steps, replacing a differently spelled repository name.
func TestCompileBundleKeepsVariableScopesSeparate(t *testing.T) {
	bundle, err := compileEnvironmentBundle(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    if: vars.region == 'base'
    steps: [{run: 'echo ${{ vars.REGION }} ${{ vars.SHARED }}'}]
  deploy:
    runs-on: ubuntu-latest
    environment: production
    if: github.event.ref == 'refs/heads/main' && vars.REGION == 'base'
    steps:
      - run: 'echo ${{ vars.region }} ${{ vars.SHARED }}'
      - run: echo ${{ format('{0}-{1}', github.event.ref, vars.REGION) }}
        if: github.event.ref == 'refs/heads/main' && vars.REGION == 'base'
`, environmentVariablesOptions(t, map[string]string{"region": "base", "SHARED": "shared"}))
	if err != nil {
		t.Fatal(err)
	}
	repository := map[string]string{"region": "base", "SHARED": "shared"}
	build, deploy := bundle.Plans[0].Job, bundle.Plans[1].Job
	if !reflect.DeepEqual(build.RepositoryVars, repository) || build.EnvironmentVars != nil || !reflect.DeepEqual(build.Vars(), repository) {
		t.Fatalf("build vars = repository %#v, environment %#v", build.RepositoryVars, build.EnvironmentVars)
	}
	if !reflect.DeepEqual(deploy.RepositoryVars, repository) || !reflect.DeepEqual(deploy.EnvironmentVars, map[string]string{"REGION": probeRegion, "TIER": "gold"}) {
		t.Fatalf("deploy vars = repository %#v, environment %#v", deploy.RepositoryVars, deploy.EnvironmentVars)
	}
	if got := deploy.VarsBeforeEnvironment(); !reflect.DeepEqual(got, repository) {
		t.Fatalf("deploy VarsBeforeEnvironment() = %#v", got)
	}
	if got := deploy.Vars(); !reflect.DeepEqual(got, map[string]string{"REGION": probeRegion, "TIER": "gold", "SHARED": "shared"}) {
		t.Fatalf("deploy Vars() = %#v", got)
	}
	// Runner-evaluated positions keep their vars expressions even when the
	// repository scope defines the name, including expressions that mix event
	// values the compiler does reduce, so the runtime applies the environment
	// override instead of a compile-time repository value.
	if build.Steps[0].Command != "echo ${{ vars.REGION }} ${{ vars.SHARED }}" || deploy.Steps[0].Command != "echo ${{ vars.region }} ${{ vars.SHARED }}" || build.Condition != "vars.region == 'base'" {
		t.Fatalf("vars templates were reduced at compile time: build=%q deploy=%q condition=%q", build.Steps[0].Command, deploy.Steps[0].Command, build.Condition)
	}
	if mixed := deploy.Steps[1]; mixed.Command != "echo ${{ format('{0}-{1}', 'refs/heads/main', vars.region) }}" || mixed.Condition != "(true && (vars.region == 'base'))" {
		t.Fatalf("mixed event and vars expressions were reduced with repository values: command=%q condition=%q", mixed.Command, mixed.Condition)
	}
	// jobs.<id>.if is evaluated before the environment applies, so the same
	// expression there does reduce with the repository value.
	if deploy.Condition != "true" {
		t.Fatalf("job condition = %q, want repository value applied", deploy.Condition)
	}
}

// TestCompileBundleKeepsUnresolvedVariableReferencesCompilable proves the
// environment snapshot adds values without adding compile errors: a vars
// reference the job's environment does not satisfy still compiles and, as
// before, evaluates to an empty string when the job runs.
func TestCompileBundleKeepsUnresolvedVariableReferencesCompilable(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		workflow string
		wantVars map[string]string
	}{
		{
			name: "no environment",
			workflow: `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    if: vars.FLAG == 'x'
    steps: [{run: 'echo ${{ vars.REGION }}'}]
`,
		},
		{
			name: "environment does not define it",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: true
        env:
          BUCKET: ${{ vars.BUCKET }}
`,
			wantVars: map[string]string{"REGION": probeRegion, "TIER": "gold"},
		},
		{
			name: "job condition references an environment variable",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    if: vars.REGION != ''
    steps: [{run: true}]
`,
			wantVars: map[string]string{"REGION": probeRegion, "TIER": "gold"},
		},
		{
			name: "computed index and whole context",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    strategy:
      matrix:
        name: [REGION]
    steps:
      - run: echo ${{ vars[matrix.name] }} ${{ toJSON(vars) }} ${{ join(vars.*, '-') }}
`,
			wantVars: map[string]string{"REGION": probeRegion, "TIER": "gold"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bundle, err := compileEnvironmentBundle(t, testCase.workflow, environmentVariablesOptions(t, nil))
			if err != nil {
				t.Fatalf("CompileBundleWithOptions() error = %v, want unresolved vars references to compile", err)
			}
			for _, artifact := range bundle.Plans {
				if !reflect.DeepEqual(artifact.Job.EnvironmentVars, testCase.wantVars) {
					t.Fatalf("environment vars = %#v, want %#v", artifact.Job.EnvironmentVars, testCase.wantVars)
				}
			}
		})
	}
}

// TestCompileBundleActionInputDefaultsMayReferenceUndefinedVariables proves
// action metadata defaults follow the same rule as workflow sites: an
// undefined vars name is not a compile error.
func TestCompileBundleActionInputDefaultsMayReferenceUndefinedVariables(t *testing.T) {
	repository := t.TempDir()
	writeAction(t, repository, "region", `name: region
inputs:
  region:
    default: ${{ vars.REGION }}
  bucket:
    default: ${{ vars.BUCKET }}
runs:
  using: node24
  main: index.js
`)
	workflow := writeWorkflow(t, repository, "deploy.yml", `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: ./region
`)
	if _, err := CompileBundleWithOptions(workflow, readFile(t, workflow), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", environmentVariablesOptions(t, nil)); err != nil {
		t.Fatalf("CompileBundleWithOptions() error = %v", err)
	}
}

// TestCompileBundlePlansTokenAuthorityWithUnknownVariables proves token
// authority planning never resolves vars, whatever scope defines them: a
// github.token reference behind a variable condition still requests the token,
// so it is rejected under empty permissions exactly as before variables
// existed. A repository value in jobs.<id>.if is absent because the compiler
// reduces that condition itself and skips the job before planning runs.
func TestCompileBundlePlansTokenAuthorityWithUnknownVariables(t *testing.T) {
	const condition = "vars.ENABLED != 'yes'"
	for _, testCase := range []struct {
		name       string
		jobIf      string
		stepIf     string
		repository map[string]string
	}{
		{name: "environment value in job condition", jobIf: condition},
		{name: "environment value in step condition", stepIf: condition},
		{name: "repository value in step condition", stepIf: condition, repository: map[string]string{"enabled": "yes"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workflow := `on: push
permissions: {}
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
`
			if testCase.jobIf != "" {
				workflow += "    if: " + testCase.jobIf + "\n"
			}
			workflow += "    steps:\n      - run: 'echo ${{ github.token }}'\n"
			if testCase.stepIf != "" {
				workflow += "        if: " + testCase.stepIf + "\n"
			}
			options := defaultOptions()
			options.Vars.Repository = testCase.repository
			options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
				"production": {Variables: map[string]string{"ENABLED": "yes"}},
			}}
			_, err := compileEnvironmentBundle(t, workflow, options)
			if err == nil || !strings.Contains(err.Error(), "github.token") {
				t.Fatalf("CompileBundleWithOptions() error = %v, want token authority retained because planning treats vars as unknown", err)
			}
		})
	}
}

func TestCompileBundleRejectsEnvironmentVariablesInCompileTimePositions(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		workflow string
		want     string
	}{
		{
			name: "runs-on",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ${{ vars.REGION }}
    environment: production
    steps: [{run: true}]
`,
			want: "runs-on expression cannot be resolved at compile time",
		},
		{
			name: "matrix",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    strategy:
      matrix:
        region: ['${{ vars.REGION }}']
    steps: [{run: true}]
`,
			want: "matrix expression cannot be resolved at compile time",
		},
		{
			name: "concurrency",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    concurrency: deploy-${{ vars.REGION }}
    steps: [{run: true}]
`,
			want: "concurrency group cannot be resolved at compile time",
		},
		{
			name: "container image",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    container: {image: 'registry/app:${{ vars.REGION }}'}
    steps: [{run: true}]
`,
			want: "resolve job container",
		},
		{
			name: "environment name",
			workflow: `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: ${{ vars.REGION }}
    steps: [{run: true}]
`,
			want: "environment names that use expressions are unsupported",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := compileEnvironmentBundle(t, testCase.workflow, environmentVariablesOptions(t, nil))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("CompileBundleWithOptions() error = %v, want %q", err, testCase.want)
			}
			if strings.Contains(err.Error(), probeRegion) {
				t.Fatalf("error leaks a variable value: %v", err)
			}
		})
	}
}

func TestCompileBundleRejectsVariablesInReusableWorkflowInputs(t *testing.T) {
	repository := t.TempDir()
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      region:
        type: string
        required: true
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps: [{run: 'echo ${{ inputs.region }}'}]
`)
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      region: ${{ vars.REGION }}
`)
	_, err := CompileBundleWithOptions(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer", environmentVariablesOptions(t, nil))
	if err == nil || !strings.Contains(err.Error(), "vars") {
		t.Fatalf("CompileBundleWithOptions() error = %v, want reusable input variable rejection", err)
	}
}
