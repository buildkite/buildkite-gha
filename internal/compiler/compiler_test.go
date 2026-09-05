package compiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflow"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestEffectiveReusablePermissionsOnlyNarrowCallerAuthority(t *testing.T) {
	span := workflow.Span{Start: workflow.Position{Line: 2, Column: 1}}
	caller := &workflow.Permissions{Scopes: map[string]string{"contents": "read", "pull-requests": "write"}, Span: span}
	callee := &workflow.Permissions{Scopes: map[string]string{"contents": "write", "issues": "write", "pull-requests": "read"}, Span: span}
	if inherited := effectivePermissions(nil, nil, nil, false); !reflect.DeepEqual(inherited.Scopes, map[string]string{"contents": "read"}) {
		t.Fatalf("root default permissions = %#v", inherited)
	}
	effective := effectivePermissions(callee, nil, caller, true)
	want := map[string]string{"contents": "read", "pull-requests": "read"}
	if !reflect.DeepEqual(effective.Scopes, want) {
		t.Fatalf("effective permissions = %#v, want %#v", effective.Scopes, want)
	}
	if inherited := effectivePermissions(nil, nil, caller, true); !reflect.DeepEqual(inherited.Scopes, caller.Scopes) || inherited == caller {
		t.Fatalf("inherited permissions = %#v, want independent copy of %#v", inherited, caller)
	}
	if unbounded := effectivePermissions(callee, caller, nil, false); !reflect.DeepEqual(unbounded.Scopes, callee.Scopes) {
		t.Fatalf("root job permissions = %#v, want job replacement %#v", unbounded, callee)
	}
}

func TestValidateRejectsDockerContainerActions(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: docker://alpine:3.20
`)
	_, err := Validate("workflow.yml", source)
	if err == nil || !strings.Contains(err.Error(), "docker:// container actions are unsupported; use a Dockerfile action or replace the action with a run step") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestWorkflowTokenPolicyEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		source     string
		want       string
		diagnostic string
	}{
		{
			name: "eligible", path: ".github/workflows/ci.yml", want: "ci.yml",
			source: "on: push\npermissions:\n  contents: read\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
		{
			name: "omitted top-level permissions", path: ".github/workflows/ci.yml", want: "ci.yml",
			source: "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
		{
			name: "explicit empty permissions", path: ".github/workflows/ci.yml", diagnostic: "explicit non-empty",
			source: "on: push\npermissions: {}\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
		{
			name: "only none permissions", path: ".github/workflows/ci.yml", diagnostic: "explicit non-empty",
			source: "on: push\npermissions:\n  contents: none\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
		{
			name: "job permissions", path: ".github/workflows/ci.yml", want: "ci.yml",
			source: "on: push\npermissions:\n  contents: read\njobs:\n  test:\n    permissions:\n      contents: read\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
		{
			name: "reusable workflow", path: ".github/workflows/ci.yml", want: "ci.yml",
			source: "on: push\npermissions:\n  contents: read\njobs:\n  test:\n    uses: ./.github/workflows/reusable.yml\n",
		},
		{
			name: "invalid path", path: "ci.yml", diagnostic: "directly under .github/workflows",
			source: "on: push\npermissions:\n  contents: read\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
		{
			name: "endpoint permission mismatch", path: ".github/workflows/ci.yml", diagnostic: "unsupported permission",
			source: "on: push\npermissions:\n  models: read\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := workflow.Parse(test.path, []byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			filename, _, diagnostic := workflowTokenPolicyEvidence(test.path, parsed)
			if filename != test.want || !strings.Contains(diagnostic, test.diagnostic) {
				t.Fatalf("workflowTokenPolicyEvidence() = %q, %q", filename, diagnostic)
			}
		})
	}
}

func TestCompilePlansCarriesIDTokenWithoutForwardingItToGitHubToken(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "oidc.yml", `on: push
permissions:
  contents: read
  id-token: write
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: echo deploy
`)
	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-oidc")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].IDTokenPermission != "write" || plans[0].GitHubToken == nil || !reflect.DeepEqual(plans[0].GitHubToken.Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("OIDC and GitHub token plan = %#v", plans)
	}
}

func TestCompilePlansCarriesPluginOIDCConfigurationToEveryJob(t *testing.T) {
	configuration := &plan.OIDCConfiguration{
		Claims:         []string{"organization_id"},
		AWSSessionTags: []string{"organization_slug", "pipeline_id"},
		SubjectClaim:   "pipeline_id",
	}
	options := defaultOptions()
	options.OIDC = configuration
	plans, err := compilePlansForTest(t.Context(), "oidc.yml", []byte(`on: push
jobs:
  mint:
    permissions:
      id-token: write
    runs-on: ubuntu-latest
    steps: [{run: echo mint}]
  no-permission:
    runs-on: ubuntu-latest
    steps: [{run: echo no endpoint}]
`), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %#v", plans)
	}
	for _, job := range plans {
		if !reflect.DeepEqual(job.OIDC, configuration) {
			t.Errorf("job %q OIDC configuration = %#v", job.Workflow.LogicalJobID, job.OIDC)
		}
		if job.Workflow.LogicalJobID == "no-permission" && job.IDTokenPermission != "" {
			t.Errorf("OIDC configuration implied id-token permission %q", job.IDTokenPermission)
		}
	}
}

func TestCompilePlansBoundsNestedReusableWorkflowDefaultPermissions(t *testing.T) {
	repository := t.TempDir()
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/middle.yml
`)
	writeWorkflow(t, repository, "middle.yml", `on: workflow_call
permissions:
  contents: write
  issues: write
  packages: read
jobs:
  delegated:
    uses: ./.github/workflows/leaf.yml
`)
	writeWorkflow(t, repository, "leaf.yml", `on: workflow_call
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - run: test -n '${{ secrets.GITHUB_TOKEN }}'
`)

	plans, err := compileUntrustedPlans(caller, readFile(t, caller), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil || !reflect.DeepEqual(plans[0].GitHubToken.Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("nested reusable token plan = %#v", plans)
	}

	emptyRepository := t.TempDir()
	emptyCaller := writeWorkflow(t, emptyRepository, "caller.yml", `on: push
jobs:
  delegated:
    permissions: {}
    uses: ./.github/workflows/leaf.yml
`)
	writeWorkflow(t, emptyRepository, "leaf.yml", `on: workflow_call
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - run: test -n '${{ secrets.GITHUB_TOKEN }}'
`)
	plans, err = compileUntrustedPlans(emptyCaller, readFile(t, emptyCaller), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil || !reflect.DeepEqual(plans[0].GitHubToken.Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("reusable call permissions changed the root token scope: %#v", plans)
	}
}

func TestCompileShellGoldenGraph(t *testing.T) {
	workflowPath := smokePath(".github", "workflows", "shell.yml")
	workflowSource := readFile(t, workflowPath)
	eventSource := readFile(t, smokePath("events", "push.json"))

	got, err := Compile(workflowPath, workflowSource, eventSource)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}

	type goldenJob struct {
		Key    string         `json:"key"`
		Job    string         `json:"job"`
		Matrix map[string]any `json:"matrix,omitempty"`
		Needs  []string       `json:"needs,omitempty"`
	}
	golden := make([]goldenJob, 0, len(ir.Jobs))
	for _, job := range ir.Jobs {
		golden = append(golden, goldenJob{Key: job.Key, Job: job.LogicalJobID, Matrix: job.Matrix, Needs: job.Needs})
	}
	want := `[{"key":"gha-producer","job":"producer"},{"key":"gha-consumer-5ebbc197d87b","job":"consumer","matrix":{"variant":"one"},"needs":["gha-producer"]},{"key":"gha-consumer-91934b28b00f","job":"consumer","matrix":{"variant":"two"},"needs":["gha-producer"]}]`
	encoded, err := json.Marshal(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("compiled graph changed\nwant:\n%s\n\ngot:\n%s", want, encoded)
	}
}

func TestCompileIsByteIdenticalAndCoversSmokeCorpus(t *testing.T) {
	eventSource := readFile(t, smokePath("events", "push.json"))
	for _, name := range []string{"shell.yml", "concurrent.yml", "ci.yml", "artifact.yml"} {
		t.Run(name, func(t *testing.T) {
			path := smokePath(".github", "workflows", name)
			source := readFile(t, path)
			first, err := Compile(path, source, eventSource)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Compile(path, source, eventSource)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("repeated compilation was not byte-identical")
			}
		})
	}
}

func TestCompilePreflightsUnsupportedConditionsWithLocation(t *testing.T) {
	eventSource := readFile(t, smokePath("events", "push.json"))
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "job hash function",
			source: `on: push
jobs:
  test:
    runs-on: ubuntu-latest
    if: hashFiles('go.sum') != ''
    steps:
      - run: true
`,
			want: `conditions.yml:5:9: job "test": job condition: condition function "hashFiles" is unavailable in job conditions`,
		},
		{
			name: "anonymous step context",
			source: `on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - if: secrets.TOKEN
        run: true
`,
			want: `conditions.yml:6:13: job "test": step 1 condition: condition context "secrets" is unsupported`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, compile := range map[string]func() error{
				"validate": func() error {
					_, err := Validate("conditions.yml", []byte(test.source))
					return err
				},
				"compile": func() error {
					_, err := Compile("conditions.yml", []byte(test.source), eventSource)
					return err
				},
				"plans": func() error {
					_, err := compileUntrustedPlans("conditions.yml", []byte(test.source), eventSource, "0.0.0-test", testDistributionDigest, "gha-untrusted")
					return err
				},
			} {
				t.Run(name, func(t *testing.T) {
					err := compile()
					if err == nil || !strings.Contains(err.Error(), test.want) {
						t.Fatalf("error = %v, want %q", err, test.want)
					}
				})
			}
		})
	}
}

func TestCompileAcceptsGitHubConditionCoercionAndMissingMembers(t *testing.T) {
	eventSource := readFile(t, smokePath("events", "push.json"))
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - version: 12
          - os: ubuntu-latest
    if: github.event.action == 'opened' || vars.ENABLED == true || matrix.version >= 12
    steps:
      - if: matrix.version == 12
        run: true
`)
	if _, err := Compile("conditions.yml", source, eventSource); err != nil {
		t.Fatal(err)
	}
}

func TestCompileRejectsAuthorityAndWholeEventInLazyGraphFunctions(t *testing.T) {
	eventSource := readFile(t, smokePath("events", "push.json"))
	for _, test := range []struct {
		name, source, want string
	}{
		{
			name: "skipped secret in runner",
			source: `on: push
jobs:
  test:
    runs-on: ${{ case(true, 'ubuntu-latest', secrets.TOKEN) }}
    steps:
      - run: true
`,
			want: `unsupported compile-time context "secrets"`,
		},
		{
			name: "whole event in concurrency",
			source: `on: push
concurrency:
  group: ${{ toJSON(github.event) }}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: true
`,
			want: `unavailable value "github.event"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Compile("expressions.yml", []byte(test.source), eventSource); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompileAcceptsCompoundWorkflowStepFields(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: ${{ format('test-{0}', github.event_name) }}
        env:
          ENABLED: ${{ contains(github.ref, 'heads') }}
        run: echo "${{ format('{0}-{1}', vars.PREFIX, vars.MISSING || 'fallback') }}"
        shell: ${{ 'bash' || 'sh' }}
        working-directory: ${{ format('{0}', '.') }}
`)
	if _, err := Compile("steps.yml", source, readFile(t, smokePath("events", "push.json"))); err != nil {
		t.Fatal(err)
	}
}

func TestCompileRetainsExpressionValuedStepControls(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    strategy:
      matrix:
        experimental: [true]
        timeout: [5]
    runs-on: ubuntu-latest
    steps:
      - run: exit 1
        continue-on-error: ${{ matrix.experimental }}
        timeout-minutes: ${{ matrix.timeout }}
`)
	encoded, err := Compile("controls.yml", source, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var result IR
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	step := result.Jobs[0].Steps[0]
	if step.ContinueOnErrorExpression != "${{ matrix.experimental }}" || step.TimeoutMinutesExpression != "${{ matrix.timeout }}" {
		t.Fatalf("compiled step controls = %#v", step)
	}
}

func TestReusableStepTimeoutRejectsOutOfRangeStaticInput(t *testing.T) {
	job := workflow.Job{ID: "callee", Steps: []workflow.Step{{
		Kind: "run", Run: "true", TimeoutMinutesExpression: "${{ inputs.timeout }}",
		Span: workflow.Span{Start: workflow.Position{Line: 7, Column: 7}},
	}}}
	if _, err := applyStaticInputs("callee.yml", job, map[string]any{"timeout": 0}); err == nil || !strings.Contains(err.Error(), "greater than 0 and at most 360") {
		t.Fatalf("applyStaticInputs() error = %v", err)
	}
}

func TestCompileRetainsSupportedRuntimeDependentConditions(t *testing.T) {
	source := []byte(`on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      ready: ${{ steps.produce.outputs.ready }}
    steps:
      - id: produce
        run: echo "ready=true" >> "$GITHUB_OUTPUT"
  test:
    needs: prepare
    runs-on: ubuntu-latest
    services:
      redis:
        image: redis:7
        ports: [6379]
    strategy:
      matrix:
        os: [ubuntu-latest]
    if: always() && needs.prepare.result == 'success' && needs.prepare.outputs.ready && matrix.os && github.ref && github.head_ref == ''
    steps:
      - id: test
        if: success() && env.ENABLED && vars.FLAG && needs.prepare.outputs.ready && github.head_ref == ''
        run: true
      - if: failure() && steps.test.outcome == 'failure' && steps.test.conclusion == 'success' && steps.test.outputs.ready && job.services.redis.ports[6379]
        run: true
`)
	plans, err := compileUntrustedPlans("conditions.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[1].Condition != "always() && needs.prepare.result == 'success' && needs.prepare.outputs.ready && matrix.os && github.ref && github.head_ref == ''" || plans[1].Steps[0].Condition != "success() && env.ENABLED && vars.FLAG && needs.prepare.outputs.ready && github.head_ref == ''" || plans[1].Steps[1].Condition != "failure() && steps.test.outcome == 'failure' && steps.test.conclusion == 'success' && steps.test.outputs.ready && job.services.redis.ports[6379]" {
		t.Fatalf("runtime conditions were not retained: %#v", plans)
	}
}

func TestCompileExpandsMatrixIncludeExcludeAndDependencies(t *testing.T) {
	source := []byte(`name: matrix conformance
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        fruit: [apple, pear]
        animal: [cat, dog]
        exclude:
          - fruit: pear
            animal: dog
        include:
          - color: green
          - color: pink
            animal: cat
          - fruit: banana
          - fruit: banana
            animal: cat
    steps:
      - run: true
  report:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	got, err := Compile("matrix.yml", source, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 6 {
		t.Fatalf("jobs = %d, want five matrix instances and one dependent", len(ir.Jobs))
	}
	wantMatrices := []map[string]any{
		{"animal": "cat", "color": "pink", "fruit": "apple"},
		{"animal": "dog", "color": "green", "fruit": "apple"},
		{"animal": "cat", "color": "pink", "fruit": "pear"},
		{"fruit": "banana"},
		{"animal": "cat", "fruit": "banana"},
	}
	for i, want := range wantMatrices {
		if !reflect.DeepEqual(ir.Jobs[i].Matrix, want) {
			t.Fatalf("matrix instance %d = %#v, want %#v", i, ir.Jobs[i].Matrix, want)
		}
	}
	report := ir.Jobs[5]
	if report.LogicalJobID != "report" || len(report.Needs) != len(wantMatrices) {
		t.Fatalf("dependent job = %#v, want dependency on all matrix instances", report)
	}
	wantNeeds := make([]string, len(wantMatrices))
	for i := range wantMatrices {
		wantNeeds[i] = ir.Jobs[i].Key
	}
	sort.Strings(wantNeeds)
	for i, need := range report.Needs {
		if need != wantNeeds[i] {
			t.Fatalf("dependency %d = %q, want %q", i, need, wantNeeds[i])
		}
	}
}

func TestCompileResolvesStaticMatrixIncludeAndExcludeExpressions(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
        exclude: ${{ fromJSON(vars.EXCLUDE) }}
        include: ${{ fromJSON(vars.INCLUDE) }}
    steps:
      - run: true
`)
	options := defaultOptions()
	options.Vars.Bridge = map[string]string{
		"EXCLUDE": `[{"os":"macos-latest"}]`,
		"INCLUDE": `[{"os":"ubuntu-latest","version":24},{"os":"macos-14","version":14}]`,
	}
	compiled, err := CompileWithOptions("static-sections.yml", source, readFile(t, smokePath("events", "push.json")), options)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"os": "ubuntu-latest", "version": float64(24)},
		{"os": "macos-14", "version": float64(14)},
	}
	if len(ir.Jobs) != len(want) {
		t.Fatalf("jobs = %d, want %d", len(ir.Jobs), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(ir.Jobs[i].Matrix, want[i]) {
			t.Fatalf("matrix instance %d = %#v, want %#v", i, ir.Jobs[i].Matrix, want[i])
		}
	}
}

func TestCompileBoundsStaticExpressionMatrixExpansion(t *testing.T) {
	values := make([]string, maxMatrixInstances+1)
	for i := range values {
		values[i] = fmt.Sprintf("%q", fmt.Sprint(i))
	}
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(vars.MATRIX) }}
    steps: [{run: true}]
`)
	options := defaultOptions()
	options.Vars.Bridge = map[string]string{"MATRIX": `{"value":[` + strings.Join(values, ",") + `]}`}
	_, err := CompileWithOptions("bounded-matrix.yml", source, readFile(t, smokePath("events", "push.json")), options)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("matrix expands beyond %d instances", maxMatrixInstances)) {
		t.Fatalf("Compile() error = %v, want static matrix instance limit", err)
	}
}

func TestCompileExpandsIncludeOnlyMatrixAsDistinctJobs(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - fruit: banana
          - fruit: apple
            color: green
    steps:
      - run: true
`)
	got, err := Compile("include-only.yml", source, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"fruit": "banana"}, {"color": "green", "fruit": "apple"}}
	if len(ir.Jobs) != len(want) {
		t.Fatalf("jobs = %d, want %d", len(ir.Jobs), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(ir.Jobs[i].Matrix, want[i]) {
			t.Fatalf("matrix instance %d = %#v, want %#v", i, ir.Jobs[i].Matrix, want[i])
		}
	}
}

func TestCompileAllowsCompatibleConcreteMatrixConditionTypes(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: [12, 14.0]
        experimental: [true, false]
    if: matrix.version == 12
    steps:
      - if: matrix.experimental == true
        run: true
`)
	if _, err := Compile("conditions.yml", source, readFile(t, smokePath("events", "push.json"))); err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestCompileAllowsMaxUint64MatrixCondition(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        value: [18446744073709551615]
    if: matrix.value != 0
    steps:
      - run: true
`)
	if _, err := Compile("conditions.yml", source, readFile(t, smokePath("events", "push.json"))); err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestCompilePreservesStaticMatrixScalarTypes(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: [12, "14"]
        experimental: [true, false]
        exclude:
          - version: 12
            experimental: false
        include:
          - version: 16
            experimental: false
    steps:
      - run: true
`)
	got, err := Compile("typed-matrix.yml", source, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(got, &ir); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"experimental": true, "version": float64(12)},
		{"experimental": true, "version": "14"},
		{"experimental": false, "version": "14"},
		{"experimental": false, "version": float64(16)},
	}
	if len(ir.Jobs) != len(want) {
		t.Fatalf("jobs = %d, want %d", len(ir.Jobs), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(ir.Jobs[i].Matrix, want[i]) {
			t.Fatalf("matrix instance %d = %#v, want %#v", i, ir.Jobs[i].Matrix, want[i])
		}
	}
}

func TestCompileMatchesStaticIncludeExcludeAgainstExpressionNumbers(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: ${{ fromJSON(vars.VERSIONS) }}
        exclude:
          - version: 12
        include:
          - version: 14.0
            experimental: true
    steps:
      - run: true
`)
	compiled, err := CompileWithOptions("numeric-matrix.yml", source, readFile(t, smokePath("events", "push.json")), Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"VERSIONS": `[12,14]`}},
		Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "linux"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(compiled, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || ir.Jobs[0].Matrix["version"] != float64(14) || ir.Jobs[0].Matrix["experimental"] != true {
		t.Fatalf("numeric matrix jobs = %#v, want one included version 14 job", ir.Jobs)
	}
}

func TestCompileRejectsMatrixThatExcludesEveryCombination(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: [12]
        exclude:
          - version: 12
    steps:
      - run: true
  report:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	_, err := Compile("empty-matrix.yml", source, readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), "matrix excludes every combination") {
		t.Fatalf("Compile() error = %v, want empty matrix rejection", err)
	}
}

func TestCompileExpandsLocalReusableWorkflowWithNamespacesAndDependencies(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    steps:
      - run: prepare
  delegated:
    name: delegated tests
    needs: prepare
    uses: ./.github/workflows/reusable.yml
    with:
      message: hello
      enabled: true
  finish:
    needs: delegated
    if: always() && needs.delegated.result == 'success'
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ needs.delegated.result }}" = success
`)
	calleePath := writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      message:
        type: string
        required: true
      enabled:
        type: boolean
        required: true
jobs:
  first:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ inputs.message }} ${{ inputs.enabled }}"
  second:
    needs: first
    runs-on: ubuntu-latest
    steps:
      - run: second
`)
	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 4 {
		t.Fatalf("jobs = %d, want 4", len(ir.Jobs))
	}
	byID := make(map[string]JobInstance, len(ir.Jobs))
	for _, job := range ir.Jobs {
		byID[job.LogicalJobID] = job
	}
	prepare := byID["prepare"]
	first := byID["delegated.first"]
	second := byID["delegated.second"]
	finish := byID["finish"]
	if !reflect.DeepEqual(first.Needs, []string{prepare.Key}) || !reflect.DeepEqual(first.NeedGroups, map[string][]string{"prepare": {prepare.Key}}) || !reflect.DeepEqual(first.NeedOutputs, map[string][]NeedOutput{"prepare": {}}) {
		t.Fatalf("callee root needs = %#v / %#v / %#v, want status-only caller prerequisite", first.Needs, first.NeedGroups, first.NeedOutputs)
	}
	if !reflect.DeepEqual(second.Needs, []string{first.Key}) || !reflect.DeepEqual(second.NeedGroups, map[string][]string{"first": {first.Key}}) {
		t.Fatalf("callee dependency = %#v / %#v, want source-local need name", second.Needs, second.NeedGroups)
	}
	if !reflect.DeepEqual(finish.Needs, []string{first.Key, second.Key}) || !reflect.DeepEqual(finish.NeedGroups, map[string][]string{"delegated": {first.Key, second.Key}}) || !reflect.DeepEqual(finish.NeedOutputs, map[string][]NeedOutput{"delegated": {}}) {
		t.Fatalf("caller dependency projection = %#v / %#v / %#v", finish.Needs, finish.NeedGroups, finish.NeedOutputs)
	}
	if first.Key != "gha-delegated-first" || first.Label != "delegated tests / first" {
		t.Fatalf("callee identity = key %q label %q", first.Key, first.Label)
	}
	calleeDigest := sha256.Sum256(readFile(t, calleePath))
	wantCalleeDigest := "sha256:" + hex.EncodeToString(calleeDigest[:])
	if first.SourcePath != "./.github/workflows/reusable.yml" || first.SourceDigest != wantCalleeDigest || first.Steps[0].Run != `echo "hello true"` {
		t.Fatalf("callee provenance/input = %q / %q / %q, want repository-relative path, digest, and statically substituted input", first.SourcePath, first.SourceDigest, first.Steps[0].Run)
	}
	if prepare.SourcePath != "./.github/workflows/caller.yml" || finish.SourcePath != "./.github/workflows/caller.yml" {
		t.Fatalf("caller source provenance = %q / %q", prepare.SourcePath, finish.SourcePath)
	}
	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 4 {
		t.Fatalf("plans = %d, want 4", len(plans))
	}
	if plans[0].Workflow.RunPath != "" || plans[3].Workflow.RunPath != "" {
		t.Fatalf("direct workflow run paths = %q / %q, want implicit source paths", plans[0].Workflow.RunPath, plans[3].Workflow.RunPath)
	}
	if plans[1].Workflow.RunPath != "./.github/workflows/caller.yml" || plans[2].Workflow.RunPath != "./.github/workflows/caller.yml" {
		t.Fatalf("reusable workflow run paths = %q / %q, want caller path", plans[1].Workflow.RunPath, plans[2].Workflow.RunPath)
	}
	firstPlan, err := plan.Encode(plans[1])
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := plan.Encode(plans[2])
	if err != nil {
		t.Fatal(err)
	}
	if plans[3].Schema != plan.Schema {
		t.Fatalf("downstream reusable-workflow plan schema = %q, want current schema", plans[3].Schema)
	}
	if plans[1].Schema != plan.Schema || !reflect.DeepEqual(plans[1].NeedOutputs, map[string][]plan.NeedOutput{"prepare": {}}) {
		t.Fatalf("callee root caller prerequisite projection = %q / %#v", plans[1].Schema, plans[1].NeedOutputs)
	}
	if plans[3].Condition != "always() && needs.delegated.result == 'success'" || plans[3].Steps[0].Command != `test "${{ needs.delegated.result }}" = success` {
		t.Fatalf("downstream reusable-workflow result expressions = %q / %q", plans[3].Condition, plans[3].Steps[0].Command)
	}
	if !reflect.DeepEqual(plans[3].NeedSources, map[string][]plan.NeedSource{
		"delegated": {
			{StepKey: first.Key, PlanDigest: transport.Digest(firstPlan)},
			{StepKey: second.Key, PlanDigest: transport.Digest(secondPlan)},
		},
	}) || !reflect.DeepEqual(plans[3].NeedOutputs, map[string][]plan.NeedOutput{"delegated": {}}) {
		t.Fatalf("downstream reusable-workflow plan needs = %#v", plans[3].NeedSources)
	}
}

func TestBlockerFieldsChangedTracksInputSubstitutionSurfaces(t *testing.T) {
	base := workflow.Job{
		If: "true", RunsOn: []string{"ubuntu-latest"}, DefaultShell: "bash",
		Steps: []workflow.Step{{If: "true", Uses: "owner/action@v1", Shell: "bash", Run: "echo unchanged"}},
	}
	tests := []struct {
		name   string
		change func(*workflow.Job)
		want   bool
	}{
		{name: "job condition", change: func(job *workflow.Job) { job.If = "false" }, want: true},
		{name: "runner", change: func(job *workflow.Job) { job.RunsOn = []string{"private"} }, want: true},
		{name: "matrix", change: func(job *workflow.Job) { job.Matrix = &workflow.Matrix{} }, want: true},
		{name: "action", change: func(job *workflow.Job) { job.Steps[0].Uses = "owner/other@v1" }, want: true},
		{name: "shell", change: func(job *workflow.Job) { job.Steps[0].Shell = "pwsh" }, want: true},
		{name: "unrelated run command", change: func(job *workflow.Job) { job.Steps[0].Run = "echo changed" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			changed.RunsOn = slices.Clone(base.RunsOn)
			changed.Steps = slices.Clone(base.Steps)
			test.change(&changed)
			if got := blockerFieldsChanged(base, changed); got != test.want {
				t.Fatalf("blockerFieldsChanged() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCompileDoesNotAttributeReusableInputThroughRunnerMatrix(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
    with:
      runner: ${{ github.event.runner }}
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      runner:
        type: string
        required: true
jobs:
  test:
    strategy:
      matrix:
        runner:
          - ${{ inputs.runner }}
    runs-on: ${{ matrix.runner }}
    steps:
      - run: true
`)
	eventSource := pushEvent(t)
	event := bytes.Replace(eventSource, []byte(`"payload": {`), []byte(`"payload": {"runner": "windows-secret",`), 1)
	if bytes.Equal(event, eventSource) {
		t.Fatal("event payload was not updated")
	}

	_, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), event, "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err == nil {
		t.Fatal("compileUntrustedPlans() succeeded")
	}
	var finding *ProcessingFinding
	if !errors.As(err, &finding) {
		t.Fatalf("compileUntrustedPlans() error = %T %v, want ProcessingFinding", err, err)
	}
	if finding.Blocker != "runner_label" || finding.BlockerDetail != "" {
		t.Fatalf("finding blocker = %q / %q, want runner_label with no detail: %v", finding.Blocker, finding.BlockerDetail, err)
	}
}

func TestCompileProjectsOnlyDeclaredReusableWorkflowOutputs(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
  finish:
    needs: delegated
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ needs.delegated.outputs.Published-Value }}" = visible
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    outputs:
      Published-Value:
        value: ${{ jobs.build.outputs.Release }}
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      Release: ${{ steps.emit.outputs.release }}
      private: ${{ steps.emit.outputs.private }}
    steps:
      - id: emit
        run: |
          echo release=visible >> "$GITHUB_OUTPUT"
          echo private=hidden >> "$GITHUB_OUTPUT"
`)

	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want callee and caller", len(plans))
	}
	want := map[string][]plan.NeedOutput{
		"delegated": {{Name: "Published-Value", StepKey: plans[0].Target.StepKey, Output: "release"}},
	}
	if !reflect.DeepEqual(plans[1].NeedOutputs, want) {
		t.Fatalf("caller output projection = %#v, want %#v", plans[1].NeedOutputs, want)
	}
	if len(plans[1].NeedOutputs["delegated"]) != 1 || plans[1].NeedOutputs["delegated"][0].Output == "private" {
		t.Fatalf("caller projection leaked undeclared output: %#v", plans[1].NeedOutputs)
	}
}

func TestCompileKeepsInheritedReusablePrerequisiteOutputsStatusOnly(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  producer:
    uses: ./.github/workflows/producer.yml
  consumer:
    needs: producer
    uses: ./.github/workflows/consumer.yml
`)
	writeWorkflow(t, repository, "producer.yml", `on:
  workflow_call:
    outputs:
      published:
        value: ${{ jobs.build.outputs.result }}
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      result: ${{ steps.emit.outputs.result }}
    steps:
      - id: emit
        run: echo result=visible >> "$GITHUB_OUTPUT"
`)
	writeWorkflow(t, repository, "consumer.yml", `on: workflow_call
jobs:
  consume:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)

	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 2 || !reflect.DeepEqual(ir.Jobs[1].NeedOutputs, map[string][]NeedOutput{"producer": {}}) {
		t.Fatalf("consumer inherited projections = %#v, want producer status only", ir.Jobs)
	}
}

func TestCompileProjectsDeclaredReusableWorkflowMatrixAndNestedOutputs(t *testing.T) {
	t.Run("matrix", func(t *testing.T) {
		repository := t.TempDir()
		callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
  finish:
    needs: delegated
    runs-on: ubuntu-latest
    steps:
      - run: test -n "${{ needs.delegated.outputs.release }}"
`)
		writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    outputs:
      release:
        value: ${{ jobs.build.outputs.release }}
jobs:
  build:
    strategy:
      matrix:
        target: [linux, arm]
    runs-on: ubuntu-latest
    outputs:
      release: ${{ steps.emit.outputs.release }}
    steps:
      - id: emit
        run: echo "release=${{ matrix.target }}" >> "$GITHUB_OUTPUT"
`)

		plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
		if err != nil {
			t.Fatal(err)
		}
		outputs := plans[len(plans)-1].NeedOutputs["delegated"]
		if len(outputs) != 2 || outputs[0].Name != "release" || outputs[1].Name != "release" || outputs[0].StepKey == outputs[1].StepKey {
			t.Fatalf("matrix output projections = %#v, want both exact producers", outputs)
		}
	})

	t.Run("nested", func(t *testing.T) {
		repository := t.TempDir()
		callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  middle:
    uses: ./.github/workflows/middle.yml
  finish:
    needs: middle
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ needs.middle.outputs.published }}" = nested
`)
		writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    outputs:
      published:
        value: ${{ jobs.leaf.outputs.forwarded }}
jobs:
  leaf:
    uses: ./.github/workflows/leaf.yml
`)
		writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    outputs:
      forwarded:
        value: ${{ jobs.build.outputs.result }}
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      result: ${{ steps.emit.outputs.result }}
    steps:
      - id: emit
        run: echo result=nested >> "$GITHUB_OUTPUT"
`)

		plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
		if err != nil {
			t.Fatal(err)
		}
		outputs := plans[len(plans)-1].NeedOutputs["middle"]
		if len(outputs) != 1 || outputs[0].Name != "published" || outputs[0].Output != "result" || outputs[0].StepKey != plans[0].Target.StepKey {
			t.Fatalf("nested output projection = %#v", outputs)
		}
	})
}

func TestCompileRejectsUnsupportedReusableWorkflowOutputMappings(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "literal", value: "literal", want: "must be one static"},
		{name: "compound", value: "${{ jobs.build.outputs.release }}-suffix", want: "must be one static"},
		{name: "unknown job", value: "${{ jobs.missing.outputs.release }}", want: `references unknown job "missing"`},
		{name: "undeclared job output", value: "${{ jobs.build.outputs.missing }}", want: `references undeclared output "missing"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			callerPath := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  delegated:\n    uses: ./.github/workflows/reusable.yml\n")
			reusablePath := writeWorkflow(t, repository, "reusable.yml", fmt.Sprintf(`on:
  workflow_call:
    outputs:
      published:
        value: %s
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      release: ${{ steps.emit.outputs.release }}
    steps:
      - id: emit
        run: echo release=visible >> "$GITHUB_OUTPUT"
`, test.value))
			_, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
			if err == nil || !strings.Contains(err.Error(), `workflow_call output "published"`) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile() error = %v, want located output mapping rejection containing %q", err, test.want)
			}
			_, err = Validate(reusablePath, readFile(t, reusablePath))
			if err == nil || !strings.Contains(err.Error(), `workflow_call output "published"`) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want same standalone output mapping rejection containing %q", err, test.want)
			}
		})
	}
}

func TestCompileBoundsReusableWorkflowOutputProjections(t *testing.T) {
	repository := t.TempDir()
	values := make([]string, plan.MaxNeedOutputs+1)
	for i := range values {
		values[i] = fmt.Sprint(i)
	}
	callerPath := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  delegated:\n    uses: ./.github/workflows/reusable.yml\n  finish:\n    needs: delegated\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	writeWorkflow(t, repository, "reusable.yml", fmt.Sprintf(`on:
  workflow_call:
    outputs:
      published:
        value: ${{ jobs.build.outputs.release }}
jobs:
  build:
    strategy:
      matrix:
        value: [%s]
    runs-on: ubuntu-latest
    outputs:
      release: ${{ steps.emit.outputs.release }}
    steps:
      - id: emit
        run: echo release=value >> "$GITHUB_OUTPUT"
`, strings.Join(values, ", ")))

	_, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `workflow_call output "published" expands call projections beyond the maximum of 64`) {
		t.Fatalf("Compile() error = %v, want bounded projection rejection", err)
	}
}

func TestCompileExpandsMatrixReusableCallsDeterministically(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    if: github.ref == 'refs/heads/main'
    strategy:
      matrix:
        target: [ubuntu-22.04, ubuntu-24.04]
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ matrix.target }}
  finish:
    needs: delegated
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ needs.delegated.result }}" = success
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  test:
    name: test ${{ inputs.target }}
    runs-on: ${{ inputs.target }}
    steps:
      - run: echo ${{ inputs.target }}
`)

	first, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("reusable-workflow matrix compilation was not byte-identical")
	}
	var ir IR
	if err := json.Unmarshal(first, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 3 || ir.Jobs[0].LogicalJobID == ir.Jobs[1].LogicalJobID || ir.Jobs[0].Key == ir.Jobs[1].Key {
		t.Fatalf("matrix reusable jobs = %#v, want two namespaced identities", ir.Jobs)
	}
	if len(ir.Jobs[0].CallGuards) != 1 || len(ir.Jobs[1].CallGuards) != 1 || ir.Jobs[0].CallGuards[0].Condition != "true" || ir.Jobs[1].CallGuards[0].Condition != "true" {
		t.Fatalf("matrix reusable call guards = %#v / %#v", ir.Jobs[0].CallGuards, ir.Jobs[1].CallGuards)
	}
	if !reflect.DeepEqual(ir.Jobs[2].NeedGroups, map[string][]string{"delegated": {ir.Jobs[0].Key, ir.Jobs[1].Key}}) || !reflect.DeepEqual(ir.Jobs[2].NeedOutputs, map[string][]NeedOutput{"delegated": {}}) {
		t.Fatalf("matrix reusable caller projection = %#v / %#v", ir.Jobs[2].NeedGroups, ir.Jobs[2].NeedOutputs)
	}
	runs := []string{ir.Jobs[0].Steps[0].Run, ir.Jobs[1].Steps[0].Run}
	sort.Strings(runs)
	if !reflect.DeepEqual(runs, []string{"echo ubuntu-22.04", "echo ubuntu-24.04"}) {
		t.Fatalf("statically resolved matrix inputs = %#v", runs)
	}
	runners := []string{ir.Jobs[0].RunsOn[0], ir.Jobs[1].RunsOn[0]}
	sort.Strings(runners)
	if !reflect.DeepEqual(runners, []string{"ubuntu-22.04", "ubuntu-24.04"}) {
		t.Fatalf("statically resolved runs-on inputs = %#v", runners)
	}
	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 || plans[0].Workflow.Path != "./.github/workflows/reusable.yml" || plans[0].Workflow.Digest != ir.Jobs[0].SourceDigest {
		t.Fatalf("callee plan provenance = %#v, want callee path and digest", plans)
	}
	if len(plans[2].NeedSources["delegated"]) != 2 || !reflect.DeepEqual(plans[2].NeedOutputs, map[string][]plan.NeedOutput{"delegated": {}}) {
		t.Fatalf("matrix reusable plan projection = %#v / %#v", plans[2].NeedSources, plans[2].NeedOutputs)
	}
}

func TestCompileRejectsMaxParallelOnReusableWorkflowMatrix(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    strategy:
      max-parallel: 1
      matrix: ${{ fromJSON('{"target":["one","two"]}') }}
    uses: ./.github/workflows/reusable.yml
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	_, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), "strategy.max-parallel on a reusable-workflow matrix cannot be preserved") {
		t.Fatalf("Compile() error = %v, want reusable max-parallel rejection", err)
	}
}

func TestCompileBindsNestedReusableCallGuardsToCallerNeeds(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      ready: ${{ steps.emit.outputs.ready }}
    steps:
      - id: emit
        run: echo ready=true >> "$GITHUB_OUTPUT"
  delegated:
    needs: prepare
    if: github.ref == 'refs/heads/main' && needs.prepare.outputs.ready
    uses: ./.github/workflows/reusable.yml
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  first:
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: false
  nested:
    needs: first
    if: always() && needs.first.result == 'failure'
    uses: ./.github/workflows/leaf.yml
`)
	writeWorkflow(t, repository, "leaf.yml", `on: workflow_call
jobs:
  leaf:
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: true
`)

	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 || len(plans[1].CallGuards) != 1 || len(plans[2].CallGuards) != 2 {
		t.Fatalf("nested plans/guards = %#v", plans)
	}
	outer := plans[2].CallGuards[0]
	inner := plans[2].CallGuards[1]
	if outer.Condition != "(true && needs.prepare.outputs.ready)" || len(outer.NeedSources["prepare"]) != 1 || outer.NeedSources["prepare"][0].StepKey != plans[0].Target.StepKey {
		t.Fatalf("outer guard = %#v", outer)
	}
	if inner.Condition != "(always() && (needs.first.result == 'failure'))" || len(inner.NeedSources["first"]) != 1 || inner.NeedSources["first"][0].StepKey != plans[1].Target.StepKey {
		t.Fatalf("inner guard = %#v", inner)
	}
	wantDependencies := []string{plans[0].Target.StepKey, plans[1].Target.StepKey}
	sort.Strings(wantDependencies)
	if !reflect.DeepEqual(plans[2].Dependencies, wantDependencies) {
		t.Fatalf("nested guard dependencies = %#v", plans[2].Dependencies)
	}
	encoded, err := plan.Encode(plans[2])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := plan.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := plan.Encode(decoded)
	if err != nil || !bytes.Equal(reencoded, encoded) {
		t.Fatalf("call guard plan round trip changed bytes: %v", err)
	}
}

func TestCompileRejectsUnavailableReusableCallConditionContexts(t *testing.T) {
	for _, contextName := range []string{"matrix.target", "strategy.job-index", "secrets.TOKEN", "env.FLAG", "runner.os", "steps.build.outcome"} {
		t.Run(contextName, func(t *testing.T) {
			repository := t.TempDir()
			callerPath := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  delegated:\n    if: "+contextName+"\n    uses: ./.github/workflows/reusable.yml\n")
			writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
			_, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
			if err == nil || !strings.Contains(err.Error(), "reusable-workflow call condition context") {
				t.Fatalf("Compile() error = %v", err)
			}
		})
	}
}

func TestCompileSubstitutesReusableInputsInConditions(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  enabled-call:
    uses: ./.github/workflows/reusable.yml
    with:
      enabled: true
      label: deploy
  disabled-call:
    uses: ./.github/workflows/reusable.yml
    with:
      enabled: false
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      enabled:
        type: boolean
        required: true
      label:
        type: string
jobs:
  gated:
    if: inputs.enabled
    runs-on: ubuntu-latest
    steps:
      - if: ${{ inputs.enabled }}
        run: echo enabled
  string-gated:
    if: ${{ inputs.label }}
    runs-on: ubuntu-latest
    steps:
      - if: inputs.label
        run: echo non-empty
`)

	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 4 {
		t.Fatalf("plans = %d, want boolean and string jobs for both calls", len(plans))
	}
	conditions := make(map[string][2]string, len(plans))
	for _, job := range plans {
		stepCondition := ""
		if len(job.Steps) != 0 {
			stepCondition = job.Steps[0].Condition
		}
		conditions[job.Workflow.LogicalJobID] = [2]string{job.Condition, stepCondition}
	}
	if got := conditions["enabled-call.gated"]; got != [2]string{"true", "true"} {
		t.Fatalf("enabled conditions = %#v, want statically substituted true", got)
	}
	if got := conditions["disabled-call.gated"]; got != [2]string{"false", "false"} {
		t.Fatalf("disabled conditions = %#v, want inert statically disabled step", got)
	}
	if got := conditions["enabled-call.string-gated"]; got != [2]string{"'deploy'", "'deploy'"} {
		t.Fatalf("non-empty string conditions = %#v, want a quoted expression literal", got)
	}
	if got := conditions["disabled-call.string-gated"]; got != [2]string{"false", "''"} {
		t.Fatalf("empty string conditions = %#v, want original step condition retained", got)
	}
}

func TestCompileAllowsMissingEventMembersInReusableWorkflowConditions(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - id: inspect
        if: github.event.action == 'opened'
        run: true
`)

	if _, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted"); err != nil {
		t.Fatal(err)
	}
}

func TestCompileSupportsStaticAndIndexedInputsInReusableConditions(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      enabled: true
      label: release
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      enabled:
        type: boolean
        required: true
      label:
        type: string
        required: true
jobs:
  gated:
    if: inputs.enabled && github.ref
    runs-on: ubuntu-latest
    env:
      LABEL: ${{ inputs['label'] }}
    outputs:
      label: ${{ inputs['label'] }}
    steps:
      - run: echo ${{ inputs['enabled'] }} ${{ github.ref }}
      - if: inputs['enabled'] && github.ref
        run: echo implicit
      - if: ${{ inputs [ 'label' ] }}
        run: echo string
`)

	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Condition != "${{ true && github.ref }}" || plans[0].Inputs["enabled"] != true || plans[0].Inputs["label"] != "release" || plans[0].Env["LABEL"] != "release" || plans[0].Outputs["label"] != "release" || len(plans[0].Steps) != 3 || plans[0].Steps[0].Command != "echo true ${{ github.ref }}" || plans[0].Steps[1].Condition != "${{ true && github.ref }}" || plans[0].Steps[2].Condition != "'release'" {
		t.Fatalf("reusable condition = %#v", plans)
	}
}

func TestCompileForwardsIndexedStringInputToNestedReusableWorkflow(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/middle.yml
    with:
      target: release
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    inputs:
      target: {type: string, required: true}
jobs:
  call:
    uses: ./.github/workflows/leaf.yml
    with:
      target: ${{ inputs [ 'target' ] }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      target: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.target }}
`)

	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || len(ir.Jobs[0].Steps) != 1 || ir.Jobs[0].Steps[0].Run != "echo release" {
		t.Fatalf("forwarded indexed input = %#v", ir.Jobs)
	}
}

func TestCompileForwardsDeferredInputToNestedReusableWorkflow(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  hash:
    runs-on: ubuntu-latest
    outputs:
      hashes: ${{ steps.hash.outputs.hashes }}
    steps:
      - id: hash
        run: echo hashes=value >> "$GITHUB_OUTPUT"
  call:
    needs: hash
    uses: ./.github/workflows/middle.yml
    with:
      subjects: ${{ needs.hash.outputs.hashes }}
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    inputs:
      subjects: {type: string, required: true}
jobs:
  call:
    if: inputs.subjects != ''
    uses: ./.github/workflows/leaf.yml
    with:
      subjects: ${{ inputs.subjects }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      subjects: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.subjects }}
`)

	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 2 || len(ir.Jobs[1].DeferredInputs) != 1 || len(ir.Jobs[1].CallGuards) != 1 || ir.Jobs[1].Steps[0].Run != "echo ${{ inputs.subjects }}" {
		t.Fatalf("forwarded deferred input = %#v", ir.Jobs)
	}
	deferred := ir.Jobs[1].DeferredInputs["subjects"]
	if !reflect.DeepEqual(deferred.Sources, []string{ir.Jobs[0].Key}) || !reflect.DeepEqual(deferred.Outputs, []NeedOutput{{Name: "value", StepKey: ir.Jobs[0].Key, Output: "hashes"}}) {
		t.Fatalf("forwarded deferred binding = %#v", deferred)
	}
	if !reflect.DeepEqual(ir.Jobs[1].CallGuards[0].DeferredInputs["subjects"], deferred) {
		t.Fatalf("forwarded deferred call guard = %#v", ir.Jobs[1].CallGuards)
	}
}

func TestCompileDeferredInputWaitsForEveryReusableWorkflowJob(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  produce:
    uses: ./.github/workflows/producer.yml
  consume:
    needs: produce
    uses: ./.github/workflows/consumer.yml
    with:
      subject: ${{ needs.produce.outputs.subject }}
`)
	writeWorkflow(t, repository, "producer.yml", `on:
  workflow_call:
    outputs:
      subject:
        value: ${{ jobs.outputter.outputs.subject }}
jobs:
  outputter:
    runs-on: ubuntu-latest
    outputs:
      subject: ${{ steps.value.outputs.subject }}
    steps:
      - id: value
        run: echo subject=value >> "$GITHUB_OUTPUT"
  sibling:
    runs-on: ubuntu-latest
    steps:
      - run: echo sibling
`)
	writeWorkflow(t, repository, "consumer.yml", `on:
  workflow_call:
    inputs:
      subject: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.subject }}
`)

	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 3 {
		t.Fatalf("jobs = %#v", ir.Jobs)
	}
	consumer := ir.Jobs[2]
	deferred := consumer.DeferredInputs["subject"]
	if len(consumer.Needs) != 2 || len(deferred.Sources) != 2 || len(deferred.Outputs) != 1 || !slices.Equal(consumer.Needs, deferred.Sources) {
		t.Fatalf("consumer dependencies = %#v, deferred input = %#v", consumer.Needs, deferred)
	}
}

func TestCompilePreservesComputedReusableInputIndexesForRuntime(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      key: target
      target: release
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      key: {type: string, required: true}
      target: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      KEY: target
    steps:
      - run: echo "$VALUE"
        env:
          VALUE_ENV: ${{ inputs[env.KEY] }}
          VALUE_INPUT: ${{ inputs[inputs.key] }}
`)

	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Inputs["target"] != "release" || len(plans[0].Steps) != 1 || plans[0].Steps[0].Env["VALUE_ENV"] != "${{ inputs[env.KEY] }}" || plans[0].Steps[0].Env["VALUE_INPUT"] != "release" {
		t.Fatalf("computed reusable input plan = %#v", plans)
	}
}

func TestApplyStaticInputsSubstitutesIndexedInputsInStepFields(t *testing.T) {
	span := workflow.Span{Start: workflow.Position{Line: 1, Column: 1}}
	job := workflow.Job{ID: "test", Span: span, Steps: []workflow.Step{{
		Span: span,
		Run:  "echo ${{ inputs['target'] }}",
		Env:  map[string]string{"TARGET": "${{ inputs['target'] }}"},
		With: map[string]string{"target": "prefix-${{ inputs['target'] }}"},
	}}}
	resolved, err := applyStaticInputs("workflow.yml", job, map[string]any{"target": "production"})
	if err != nil {
		t.Fatal(err)
	}
	step := resolved.Steps[0]
	if step.Run != "echo production" || step.Env["TARGET"] != "production" || step.With["target"] != "prefix-production" {
		t.Fatalf("resolved indexed inputs = %#v", step)
	}
}

func TestApplyStaticInputsPreservesTypedStepControls(t *testing.T) {
	span := workflow.Span{Start: workflow.Position{Line: 1, Column: 1}}
	job := workflow.Job{ID: "test", Span: span, Steps: []workflow.Step{{
		Span:                      span,
		ContinueOnErrorExpression: "${{ inputs.allow }}",
		TimeoutMinutesExpression:  "${{ inputs.wait }}",
	}}}
	resolved, err := applyStaticInputs("workflow.yml", job, map[string]any{"allow": true, "wait": 5})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Steps[0].ContinueOnError || resolved.Steps[0].ContinueOnErrorExpression != "" || resolved.Steps[0].TimeoutMinutes != 5 || resolved.Steps[0].TimeoutMinutesExpression != "" {
		t.Fatalf("resolved controls = %#v", resolved.Steps[0])
	}

	job.Steps[0].ContinueOnErrorExpression = "${{ inputs.allow && matrix.experimental }}"
	job.Steps[0].TimeoutMinutesExpression = "${{ matrix.timeout || inputs.wait }}"
	resolved, err = applyStaticInputs("workflow.yml", job, map[string]any{"allow": true, "wait": 5})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Steps[0].ContinueOnErrorExpression != "${{ true && matrix.experimental }}" || resolved.Steps[0].TimeoutMinutesExpression != "${{ matrix.timeout || 5 }}" {
		t.Fatalf("partially resolved controls = %#v", resolved.Steps[0])
	}

	job.Steps[0].ContinueOnErrorExpression = "${{ inputs.allow && steps.setup.outcome == 'success' }}"
	job.Steps[0].TimeoutMinutesExpression = "${{ steps.setup.outputs.timeout || inputs.wait }}"
	resolved, err = applyStaticInputs("workflow.yml", job, map[string]any{"allow": true, "wait": 5})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Steps[0].ContinueOnErrorExpression != "${{ true && steps.setup.outcome == 'success' }}" || resolved.Steps[0].TimeoutMinutesExpression != "${{ steps.setup.outputs.timeout || 5 }}" {
		t.Fatalf("runtime step controls = %#v", resolved.Steps[0])
	}

	job.Steps[0].ContinueOnErrorExpression = "${{ inputs.allow && hashFiles('go.sum') != '' }}"
	job.Steps[0].TimeoutMinutesExpression = ""
	resolved, err = applyStaticInputs("workflow.yml", job, map[string]any{"allow": true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Steps[0].ContinueOnErrorExpression != "${{ true && hashFiles('go.sum') != '' }}" {
		t.Fatalf("runtime hashFiles control = %#v", resolved.Steps[0])
	}

	for _, field := range []string{"continue-on-error", "timeout-minutes"} {
		t.Run("validates hidden "+field+" branch", func(t *testing.T) {
			step := workflow.Step{Span: span}
			source := "${{ inputs.allow || unsupported() }}"
			if field == "continue-on-error" {
				step.ContinueOnErrorExpression = source
			} else {
				step.TimeoutMinutesExpression = source
			}
			_, err := applyStaticInputs("workflow.yml", workflow.Job{ID: "test", Span: span, Steps: []workflow.Step{step}}, map[string]any{"allow": true})
			if err == nil || !strings.Contains(err.Error(), "unsupported runtime function") {
				t.Fatalf("applyStaticInputs() error = %v, want unsupported runtime function", err)
			}
		})
	}

	job.Steps[0].ContinueOnErrorExpression = "${{ matrix.experimental && fromJSON(inputs.value) }}"
	job.Steps[0].TimeoutMinutesExpression = ""
	resolved, err = applyStaticInputs("workflow.yml", job, map[string]any{"value": "invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Steps[0].ContinueOnErrorExpression != "${{ matrix.experimental && fromJSON('invalid') }}" {
		t.Fatalf("runtime-first control = %#v", resolved.Steps[0])
	}

	for _, test := range []struct {
		name       string
		expression string
		inputs     map[string]any
		want       string
	}{
		{name: "boolean string", expression: "${{ inputs.value }}", inputs: map[string]any{"value": "true"}, want: "must produce a boolean"},
		{name: "numeric string", expression: "${{ inputs.value }}", inputs: map[string]any{"value": "5"}, want: "must produce a number"},
		{name: "invalid static expression", expression: "${{ fromJSON(inputs.value) && matrix.experimental }}", inputs: map[string]any{"value": "invalid"}, want: "evaluate timeout-minutes expression"},
	} {
		t.Run(test.name, func(t *testing.T) {
			step := workflow.Step{Span: span}
			if test.name == "boolean string" {
				step.ContinueOnErrorExpression = test.expression
			} else {
				step.TimeoutMinutesExpression = test.expression
			}
			_, err := applyStaticInputs("workflow.yml", workflow.Job{ID: "test", Span: span, Steps: []workflow.Step{step}}, test.inputs)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("applyStaticInputs() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRejectUnresolvedIndexedInputsInCompileTimeFields(t *testing.T) {
	span := workflow.Span{Start: workflow.Position{Line: 1, Column: 1}}
	for _, test := range []struct {
		name string
		job  workflow.Job
	}{
		{name: "job name", job: workflow.Job{Name: "${{ inputs['label'] }}"}},
		{name: "runner label", job: workflow.Job{RunsOn: []string{"${{ inputs['runner'] }}"}}},
		{name: "action reference", job: workflow.Job{Steps: []workflow.Step{{Uses: "${{ inputs['action'] }}", Span: span}}}},
		{name: "command", job: workflow.Job{Steps: []workflow.Step{{Run: "echo ${{ inputs['target'] }}", Span: span}}}},
		{name: "environment", job: workflow.Job{Steps: []workflow.Step{{Env: map[string]string{"TARGET": "${{ inputs['target'] }}"}, Span: span}}}},
		{name: "action input", job: workflow.Job{Steps: []workflow.Step{{With: map[string]string{"target": "${{ inputs['target'] }}"}, Span: span}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.job.ID = "test"
			test.job.Span = span
			if err := rejectUnresolvedInputExpressions("workflow.yml", test.job, nil); err == nil || !strings.Contains(err.Error(), "not statically resolvable") {
				t.Fatalf("rejectUnresolvedInputExpressions() error = %v", err)
			}
		})
	}
}

func TestCompilePlansResolveReusableLocalActionsFromRepositoryRoot(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  delegated:\n    uses: ./.github/workflows/reusable.yml\n")
	writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/docker\n")
	actionDir := filepath.Join(repository, ".github", "actions", "docker")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("runs:\n  using: docker\n  image: Dockerfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plans, err := compileUntrustedPlans(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"docker", "network"}) || plans[0].Workflow.Path != "./.github/workflows/reusable.yml" {
		t.Fatalf("reusable local-action plan = %#v", plans)
	}
}

func TestCompileRejectsUnsafeOrDynamicReusableWorkflowCalls(t *testing.T) {
	t.Run("input workflow symlink escape", func(t *testing.T) {
		repository := t.TempDir()
		workflowDir := filepath.Join(repository, ".github", "workflows")
		if err := os.MkdirAll(workflowDir, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.yml")
		if err := os.WriteFile(outside, []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(workflowDir, "escaped.yml")
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
			t.Fatalf("Compile() error = %v, want input workflow symlink confinement rejection", err)
		}
	})

	t.Run("remote", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: owner/repository/.github/workflows/reusable.yml@main\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "public reusable workflow source is not configured") || !strings.Contains(err.Error(), "./.github/workflows/caller.yml:4:11") {
			t.Fatalf("Compile() error = %v, want source-located missing source rejection", err)
		}
	})

	t.Run("unavailable runtime input need", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ needs.prepare.outputs.target }}
`)
		writeWorkflow(t, repository, "reusable.yml", "on:\n  workflow_call:\n    inputs:\n      target:\n        type: string\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), `input "target" is not statically resolvable`) || !strings.Contains(err.Error(), "./.github/workflows/caller.yml:6:15") {
			t.Fatalf("Compile() error = %v, want source-located runtime input rejection", err)
		}
		var finding *ProcessingFinding
		if !errors.As(err, &finding) || finding.Message != `Reusable workflow input "target" references job "prepare", but the call does not list it in needs. Add "prepare" to the reusable-workflow call's needs.` || !strings.Contains(finding.Detail, `Reusable-workflow input "target" is not statically resolvable`) || !strings.Contains(finding.Detail, `unsupported compile-time context "needs"`) {
			t.Fatalf("Compile() finding = %#v", finding)
		}
	})

	t.Run("unsupported needs input form", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      target: ${{ steps.value.outputs.target }}
    steps:
      - id: value
        run: echo target=test >> "$GITHUB_OUTPUT"
  call:
    needs: prepare
    uses: ./.github/workflows/reusable.yml
    with:
      target: prefix-${{ needs.prepare.outputs.target }}
`)
		writeWorkflow(t, repository, "reusable.yml", "on:\n  workflow_call:\n    inputs:\n      target:\n        type: string\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil {
			t.Fatal("Compile() error = nil, want unsupported needs input finding")
		}
		var finding *ProcessingFinding
		wantMessage := `Reusable workflow input "target" uses a needs expression in an unsupported form. Pass the whole value as exactly ${{ needs.<job>.outputs.<name> }}, with nothing around it. Only string inputs can take a needs value, and Buildkite resolves it before the called job runs, so the reference has to be the entire value rather than part of a larger expression. If you need a computed input from job outputs, log an issue on github.com/buildkite/buildkite-gha so we can prioritise it.`
		wantDetail := `Reusable-workflow input "target" is not statically resolvable: unsupported compile-time context "needs"`
		if !errors.As(err, &finding) || finding.Message != wantMessage || finding.Detail != wantDetail || finding.Path != "./.github/workflows/caller.yml" || finding.Line != 14 || finding.Column != 15 || finding.Job != "call" {
			t.Fatalf("Compile() finding = %#v", finding)
		}
	})

	t.Run("value unavailable before jobs run", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ steps.prepare.outputs.target }}
`)
		writeWorkflow(t, repository, "reusable.yml", "on:\n  workflow_call:\n    inputs:\n      target:\n        type: string\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		var finding *ProcessingFinding
		wantMessage := `Reusable workflow input "target" uses a value that is unavailable before jobs run. Replace it with a literal or an expression that does not depend on job results.`
		if err == nil || !errors.As(err, &finding) || finding.Message != wantMessage {
			t.Fatalf("Compile() finding = %#v", finding)
		}
	})

	t.Run("call condition", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    if: github.ref == 'refs/heads/main'\n    uses: ./.github/workflows/reusable.yml\n")
		writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		compiled, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err != nil {
			t.Fatal(err)
		}
		var ir IR
		if err := json.Unmarshal(compiled, &ir); err != nil {
			t.Fatal(err)
		}
		if len(ir.Jobs) != 1 || len(ir.Jobs[0].CallGuards) != 1 || ir.Jobs[0].CallGuards[0].Condition != "true" {
			t.Fatalf("compiled call guards = %#v", ir.Jobs)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/escaped.yml\n")
		outside := filepath.Join(t.TempDir(), "outside.yml")
		if err := os.WriteFile(outside, []byte("on: workflow_call\njobs: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(repository, ".github", "workflows", "escaped.yml")); err != nil {
			t.Fatal(err)
		}
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
			t.Fatalf("Compile() error = %v, want symlink confinement rejection", err)
		}
	})
}

func TestCompileRejectsReusableWorkflowCyclesAndDepthOverflow(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "a.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/b.yml\n")
		writeWorkflow(t, repository, "b.yml", "on: workflow_call\njobs:\n  call:\n    uses: ./.github/workflows/a.yml\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), "reusable-workflow cycle detected") || !strings.Contains(err.Error(), "a.yml -> .github/workflows/b.yml -> .github/workflows/a.yml") {
			t.Fatalf("Compile() error = %v, want explicit cycle", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		repository := t.TempDir()
		for i := 0; i <= MaxReusableWorkflowDepth; i++ {
			on := "workflow_call"
			if i == 0 {
				on = "push"
			}
			writeWorkflow(t, repository, fmt.Sprintf("%d.yml", i), fmt.Sprintf("on: %s\njobs:\n  call:\n    uses: ./.github/workflows/%d.yml\n", on, i+1))
		}
		path := filepath.Join(repository, ".github", "workflows", "0.yml")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds maximum depth %d", MaxReusableWorkflowDepth)) {
			t.Fatalf("Compile() error = %v, want explicit depth limit", err)
		}
	})
}

func TestCompileResolvesInputsBeforeNestedReusableCallMatrices(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  middle:
    uses: ./.github/workflows/middle.yml
    with:
      target: linux
  finish:
    needs: middle
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ needs.middle.result }}" = success
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  leaf:
    name: leaf ${{ inputs.target }}
    strategy:
      matrix:
        target: ["${{ inputs.target }}", arm]
    uses: ./.github/workflows/leaf.yml
    with:
      target: ${{ matrix.target }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.target }}
`)

	result, err := Compile(callerPath, readFile(t, callerPath), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 3 {
		t.Fatalf("jobs = %d, want 2 nested matrix instances and their caller", len(ir.Jobs))
	}
	runs := []string{ir.Jobs[0].Steps[0].Run, ir.Jobs[1].Steps[0].Run}
	sort.Strings(runs)
	if !reflect.DeepEqual(runs, []string{"echo arm", "echo linux"}) {
		t.Fatalf("nested static inputs = %#v", runs)
	}
	for _, job := range ir.Jobs {
		if job.LogicalJobID == "finish" {
			continue
		}
		if !strings.HasPrefix(job.Label, "middle / leaf linux") {
			t.Fatalf("nested label = %q, want substituted caller input", job.Label)
		}
	}
	if !reflect.DeepEqual(ir.Jobs[2].NeedGroups, map[string][]string{"middle": {ir.Jobs[0].Key, ir.Jobs[1].Key}}) || !reflect.DeepEqual(ir.Jobs[2].NeedOutputs, map[string][]NeedOutput{"middle": {}}) {
		t.Fatalf("nested reusable caller projection = %#v / %#v", ir.Jobs[2].NeedGroups, ir.Jobs[2].NeedOutputs)
	}
}

func TestCompileResolvesStaticExpressionsInNestedReusableMatrices(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    strategy:
      matrix: ${{ fromJSON(vars.CALLS) }}
    uses: ./.github/workflows/middle.yml
    with:
      target: ${{ matrix.target }}
      arches: '["amd64","arm64"]'
  finish:
    needs: call
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    inputs:
      target: {type: string, required: true}
      arches: {type: string, required: true}
jobs:
  leaf:
    strategy:
      matrix:
        arch: ${{ fromJSON(inputs.arches) }}
        include: ${{ fromJSON('[{"arch":"amd64","native":true}]') }}
    uses: ./.github/workflows/leaf.yml
    with:
      target: ${{ format('{0}-{1}', inputs.target, matrix.arch) }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      target: {type: string, required: true}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.target }}
`)
	options := defaultOptions()
	options.Vars.Bridge = map[string]string{"CALLS": `{"target":["linux","darwin"]}`}
	result, err := CompileWithOptions(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), options)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 5 {
		t.Fatalf("jobs = %d, want four composed instances and one fan-in", len(ir.Jobs))
	}
	runs := make([]string, 0, 4)
	for _, job := range ir.Jobs[:4] {
		runs = append(runs, job.Steps[0].Run)
	}
	sort.Strings(runs)
	wantRuns := []string{"echo darwin-amd64", "echo darwin-arm64", "echo linux-amd64", "echo linux-arm64"}
	if !reflect.DeepEqual(runs, wantRuns) {
		t.Fatalf("nested matrix runs = %#v, want %#v", runs, wantRuns)
	}
	if got := len(ir.Jobs[4].Needs); got != 4 {
		t.Fatalf("fan-in dependencies = %d, want 4", got)
	}
}

func TestCompilePreservesReusableInputsInNestedAuthoredMatrixValues(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      enabled: false
      target: release
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      enabled: {type: boolean}
      target: {type: string}
jobs:
  test:
    strategy:
      matrix:
        config:
          - enabled: ${{ inputs.enabled }}
            targets:
              - ${{ inputs.target }}
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ matrix.config.enabled }}:${{ matrix.config.targets[0] }}
`)
	result, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	wantMatrix := map[string]any{"config": map[string]any{"enabled": false, "targets": []any{"release"}}}
	if len(ir.Jobs) != 1 || !reflect.DeepEqual(ir.Jobs[0].Matrix, wantMatrix) {
		t.Fatalf("nested authored matrix inputs = %#v", ir.Jobs)
	}
}

func TestCompileRejectsRuntimeExpressionsLaunderedThroughReusableInputs(t *testing.T) {
	t.Run("unavailable static matrix value", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    strategy:\n      matrix: ${{ fromJSON(vars.MATRIX) }}\n    uses: ./.github/workflows/reusable.yml\n")
		writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), `compile-time expression references unavailable value "vars.matrix"`) {
			t.Fatalf("Compile() error = %v, want unavailable static value rejection", err)
		}
	})

	t.Run("unavailable github value", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    strategy:
      matrix:
        target: ["${{ github.run_id }}"]
    uses: ./.github/workflows/reusable.yml
`)
		writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), `compile-time expression references unavailable value "github.run_id"`) {
			t.Fatalf("Compile() error = %v, want unavailable GitHub value rejection", err)
		}
	})

	t.Run("static matrix value", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    strategy:
      matrix:
        target: ["${{ github.ref_name }}"]
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ matrix.target }}
`)
		writeWorkflow(t, repository, "reusable.yml", "on:\n  workflow_call:\n    inputs:\n      target:\n        type: string\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ inputs.target }}\n")
		result, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err != nil {
			t.Fatal(err)
		}
		var ir IR
		if err := json.Unmarshal(result, &ir); err != nil {
			t.Fatal(err)
		}
		if len(ir.Jobs) != 1 || ir.Jobs[0].Steps[0].Run != "echo main" {
			t.Fatalf("resolved reusable matrix value = %#v", ir.Jobs)
		}
	})

	t.Run("callee body", func(t *testing.T) {
		repository := t.TempDir()
		path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    with:\n      enabled: true\n")
		writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      enabled:
        type: boolean
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.enabled && 'yes' || 'no' }}
`)
		result, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err != nil {
			t.Fatal(err)
		}
		var ir IR
		if err := json.Unmarshal(result, &ir); err != nil {
			t.Fatal(err)
		}
		if len(ir.Jobs) != 1 || ir.Jobs[0].Steps[0].Run != "echo yes" {
			t.Fatalf("resolved reusable input expression = %#v", ir.Jobs)
		}
	})
}

func TestExpandMatrixRejectsRuntimeExpressionsInAuthoredValues(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "unit_tests.yml",
			source: `on: [push, pull_request]
jobs:
  build:
    needs: setup
    runs-on: ${{ matrix.os || 'ubuntu-24.04' }}
    strategy:
      matrix:
        python: ['3.10']
        modules_tool:
          - ${{ needs.setup.outputs.lmod8 }}
          - ${{ needs.setup.outputs.modules4 }}
          - ${{ needs.setup.outputs.modules5 }}
        include:
          - python: '3.6.1'
            modules_tool: ${{ needs.setup.outputs.lmod8 }}
            use_pyenv: '2.6.15'
          - python: '3.7'
            modules_tool: ${{ needs.setup.outputs.lmod8 }}
            os: ubuntu-22.04
    steps: [{run: true}]
`,
		},
		{
			name: "product-creation-tests.yml",
			source: `on: pull_request
jobs:
  e2e-tests:
    needs: [check-secrets, resolve-versions]
    runs-on: ubuntu-latest
    strategy:
      matrix:
        test-group: [plugin-events, product-crud, product-batch, product-category]
        version-set: [supported, latest]
        include:
          - version-set: supported
            wp-version: ${{ needs.resolve-versions.outputs.wp-supported }}
            wc-version: ${{ needs.resolve-versions.outputs.wc-supported }}
            version-label: Currently supported
          - version-set: latest
            wp-version: latest
            wc-version: latest
            version-label: Latest
    steps: [{run: true}]
`,
		},
		{
			name: "apis-v21.yaml",
			source: `on: push
jobs:
  cache:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        image: [coordinator-4_0, coordinator-dse-68]
    steps: [{run: true}]
  test:
    needs: resolve-coordinator-docker
    runs-on: ubuntu-latest
    strategy:
      matrix:
        project: [sgv2-docsapi, sgv2-graphqlapi, sgv2-restapi]
        name: [cassandra-40, dse-68]
        include:
          - name: cassandra-40
            image-cache-key: docker-coordinator-4_0-${{ needs.resolve-coordinator-docker.outputs.sha }}
            image-file: coordinator-4_0-${{ needs.resolve-coordinator-docker.outputs.sha }}.tar
          - name: dse-68
            image-cache-key: docker-coordinator-dse-68-${{ needs.resolve-coordinator-docker.outputs.sha }}
            image-file: coordinator-dse-68-${{ needs.resolve-coordinator-docker.outputs.sha }}.tar
    steps: [{run: true}]
`,
		},
		{
			name: "php-unit-tests.yml",
			source: `on: workflow_dispatch
jobs:
  tests:
    needs: GetMatrix
    runs-on: ubuntu-latest
    strategy:
      matrix:
        php: [8.2]
        wp-version: [latest]
        wc-versions: ['${{ join(fromJson(needs.GetMatrix.outputs.wc-versions)) }}']
        include:
          - php: 7.4
            wp-version: ${{ fromJson(needs.GetMatrix.outputs.wp-versions)[1] }}
            wc-versions: ${{ fromJson(needs.GetMatrix.outputs.wc-versions)[1] }}
          - php: 8.3
            wp-version: latest
            wc-versions: latest
    steps: [{run: true}]
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := workflow.Parse(test.name, []byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			rejected := false
			for _, job := range parsed.Jobs {
				if job.Matrix == nil {
					continue
				}
				_, err := expandMatrix(test.name, job, expression.CompileContext{})
				if err != nil && strings.Contains(err.Error(), "runtime-dependent matrix expressions are unsupported") {
					rejected = true
				}
			}
			if !rejected {
				t.Fatal("runtime-dependent authored matrix value was not rejected")
			}
		})
	}
}

func TestValidateLocatesFailingMatrixSectionExpression(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [linux]
        include: ${{ fromJSON('[{"os":"linux"}]') }}
        exclude: ${{ fromJSON('invalid') }}
    steps: [{run: true}]
`)
	_, err := Validate("matrix.yml", source)
	var finding *ProcessingFinding
	if !errors.As(err, &finding) || finding.Line != 9 || !strings.Contains(err.Error(), "matrix exclude") {
		t.Fatalf("Validate() finding = %#v, error = %v; want exclude expression at line 9", finding, err)
	}
}

func TestValidateRejectsNullMatrixSectionExpression(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [linux]
        exclude: ${{ github.event.matrix_exclusions }}
    steps: [{run: true}]
`)
	_, err := Validate("matrix.yml", source)
	if err == nil || !strings.Contains(err.Error(), "matrix exclude resolved to <nil>, want array") {
		t.Fatalf("Validate() error = %v, want null exclude rejection", err)
	}
}

func TestValidateLocatesFailingAuthoredMatrixValue(t *testing.T) {
	source := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        target:
          - ${{ github.run_id }}
        include: ${{ fromJSON('[{"target":"linux"}]') }}
    steps: [{run: true}]
`)
	_, err := Validate("matrix.yml", source)
	var finding *ProcessingFinding
	if !errors.As(err, &finding) || finding.Line != 8 || !strings.Contains(err.Error(), "github.run_id") {
		t.Fatalf("Validate() finding = %#v, error = %v; want authored value at line 8", finding, err)
	}
}

func TestCompileResolvesGitHubRefNameInMatrixDimension(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  test:
    strategy:
      matrix:
        target: ${{ fromJSON(format('["{0}"]', github.ref_name)) }}
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ matrix.target }}
`)
	result, err := Compile("matrix.yml", workflow, readFile(t, smokePath("events", "push.json")))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || ir.Jobs[0].Matrix["target"] != "main" {
		t.Fatalf("resolved github.ref_name matrix value = %#v", ir.Jobs)
	}
}

func TestCompileRejectsFlattenedReusableJobIDCollisions(t *testing.T) {
	repository := t.TempDir()
	suffix, err := matrixDigest(map[string]any{"target": "linux"})
	if err != nil {
		t.Fatal(err)
	}
	path := writeWorkflow(t, repository, "caller.yml", fmt.Sprintf(`on: push
jobs:
  call:
    strategy:
      matrix:
        target: [linux, arm]
    uses: ./.github/workflows/reusable.yml
    with:
      target: ${{ matrix.target }}
  call-%s:
    uses: ./.github/workflows/reusable.yml
    with:
      target: duplicate
`, suffix))
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.target }}
`)
	ir, err := compile(t.Context(), path, readFile(t, path), readFile(t, smokePath("events", "push.json")), defaultOptions())
	if err == nil || !strings.Contains(err.Error(), "flattened job id") || !strings.Contains(err.Error(), "collides with another job") {
		t.Fatalf("Compile() error = %v, want flattened ID collision rejection", err)
	}
	collisionID := fmt.Sprintf("call-%s.test", suffix)
	var collision *JobInstance
	for i := range ir.Jobs {
		if ir.Jobs[i].LogicalJobID == collisionID {
			collision = &ir.Jobs[i]
			break
		}
	}
	if collision == nil || collision.Inputs["target"] != "linux" || collision.Steps[0].Run != "echo linux" {
		t.Fatalf("accepted colliding job = %#v, want first flattened record", collision)
	}
	var finding *ProcessingFinding
	if !errors.As(err, &finding) || finding.Stage != StageGraph || finding.Code != CodeGraphInvalid || finding.Path != "./.github/workflows/reusable.yml" || finding.Job != collisionID {
		t.Fatalf("collision finding = %#v, want duplicate source attribution", finding)
	}
}

func TestCompileBoundsReusableWorkflowGraphExpansion(t *testing.T) {
	repository := t.TempDir()
	values := make([]string, maxMatrixInstances)
	for i := range values {
		values[i] = fmt.Sprint(i)
	}
	path := writeWorkflow(t, repository, "caller.yml", fmt.Sprintf("on: push\njobs:\n  call:\n    strategy:\n      matrix:\n        value: [%s]\n    uses: ./.github/workflows/reusable.yml\n", strings.Join(values, ", ")))
	var callee strings.Builder
	callee.WriteString("on: workflow_call\njobs:\n")
	for i := range 5 {
		_, _ = fmt.Fprintf(&callee, "  job%d:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n", i)
	}
	writeWorkflow(t, repository, "reusable.yml", callee.String())
	_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("graph expands beyond %d jobs", maxFlattenedJobs)) {
		t.Fatalf("Compile() error = %v, want bounded graph rejection", err)
	}
}

func TestCompileReusableCallResultSupportsMaximumCalleeMatrixAndJoin(t *testing.T) {
	repository := t.TempDir()
	values := make([]string, maxMatrixInstances)
	for i := range values {
		values[i] = fmt.Sprint(i)
	}
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
  finish:
    needs: delegated
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ needs.delegated.result }}" = success
`)
	writeWorkflow(t, repository, "reusable.yml", fmt.Sprintf(`on: workflow_call
jobs:
  fanout:
    strategy:
      matrix:
        value: [%s]
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ matrix.value }}"
  join:
    needs: fanout
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: true
`, strings.Join(values, ", ")))

	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != maxMatrixInstances+2 {
		t.Fatalf("plans = %d, want %d callee jobs and caller", len(plans), maxMatrixInstances+2)
	}
	caller := plans[len(plans)-1]
	if got := len(caller.NeedSources["delegated"]); got != maxMatrixInstances+1 {
		t.Fatalf("reusable-call result producers = %d, want matrix plus join (%d)", got, maxMatrixInstances+1)
	}
}

func TestCompileRejectsMissingRequiredReusableWorkflowSecrets(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n")
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    secrets:
      token:
        required: true
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `requires secret "token"`) {
		t.Fatalf("Compile() error = %v, want missing required secret rejection", err)
	}
}

func TestCompileExplicitReusableWorkflowSecretMappings(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
permissions:
  contents: read
jobs:
  call:
    strategy:
      matrix:
        value: [one, two]
    uses: ./.github/workflows/reusable.yml
    secrets:
      required_alias: ${{ secrets.ORIGINAL }}
      duplicate_alias: ${{ secrets.ORIGINAL }}
      token_alias: ${{ secrets.GITHUB_TOKEN }}
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    secrets:
      required_alias:
        required: true
      duplicate_alias:
      optional_alias:
      token_alias:
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      REQUIRED: ${{ secrets.required_alias }}
      DUPLICATE: ${{ secrets['duplicate_alias'] }}
      OPTIONAL: ${{ secrets.optional_alias }}
      TOKEN: ${{ secrets.token_alias }}
    steps:
      - run: true
`)
	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want two matrix instances", len(plans))
	}
	for _, job := range plans {
		wantMappings := map[string]string{"DUPLICATE_ALIAS": "ORIGINAL", "REQUIRED_ALIAS": "ORIGINAL"}
		if !slices.Equal(job.RequiredSecrets, []string{"ORIGINAL"}) || !reflect.DeepEqual(job.SecretMappings, wantMappings) || !job.HasCapability("secrets") {
			t.Fatalf("ordinary secret boundary = %#v / %#v / %#v", job.RequiredSecrets, job.SecretMappings, job.RequiredCapabilities)
		}
		if job.GitHubToken == nil || !slices.Equal(job.GitHubToken.Aliases, []string{"TOKEN_ALIAS"}) || !job.HasCapability("provider-token-write") {
			t.Fatalf("token alias boundary = %#v / %#v", job.GitHubToken, job.RequiredCapabilities)
		}
		encoded, encodeErr := plan.Encode(job)
		if encodeErr != nil || bytes.Contains(encoded, []byte("secret-value")) {
			t.Fatalf("encoded plan leaked a value or failed: %v\n%s", encodeErr, encoded)
		}
	}
}

func TestCompilePreservesTokenAliasUsedByOptionalActionInput(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
permissions:
  contents: read
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    secrets:
      token_alias: ${{ secrets.GITHUB_TOKEN }}
      optional_alias: ${{ secrets.OPTIONAL }}
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    secrets:
      token_alias:
      optional_alias:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/optional-token
        with:
          token: ${{ secrets.token_alias }}
          optional: ${{ secrets.optional_alias }}
`)
	writeAction(t, filepath.Join(repository, ".github", "actions"), "optional-token", `name: optional token
inputs:
  token:
  optional:
runs:
  using: node24
  main: index.js
`)

	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil || !slices.Equal(plans[0].GitHubToken.Aliases, []string{"TOKEN_ALIAS"}) {
		t.Fatalf("token alias boundary = %#v", plans)
	}
	if len(plans[0].RequiredSecrets) != 0 || plans[0].HasCapability("secrets") {
		t.Fatalf("optional ordinary action input granted secret authority: %#v", plans[0])
	}
}

func TestCompileComposesNestedExplicitSecretAliasesWithoutFallback(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/middle.yml
    secrets:
      middle_alias: ${{ secrets.ORIGINAL_SOURCE }}
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    secrets:
      middle_alias:
        required: true
      absent_optional:
jobs:
  nested:
    uses: ./.github/workflows/leaf.yml
    secrets:
      leaf_alias: ${{ secrets.middle_alias }}
      absent_leaf: ${{ secrets.absent_optional }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    secrets:
      leaf_alias:
        required: true
      absent_leaf:
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      PRESENT: ${{ secrets.leaf_alias }}
      ABSENT: ${{ secrets.absent_leaf }}
    steps:
      - run: true
`)
	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !slices.Equal(plans[0].RequiredSecrets, []string{"ORIGINAL_SOURCE"}) || !reflect.DeepEqual(plans[0].SecretMappings, map[string]string{"LEAF_ALIAS": "ORIGINAL_SOURCE"}) {
		t.Fatalf("nested secret composition = %#v", plans)
	}
}

func TestCompileScopesInheritedReusableWorkflowSecretsPerExpandedJob(t *testing.T) {
	repository := t.TempDir()
	writeAction(t, repository, ".github/actions/secret", `name: secret input
inputs:
  token:
    required: true
runs:
  using: node24
  main: index.js
`)
	path := writeWorkflow(t, repository, "caller.yml", `on: push
permissions:
  contents: read
jobs:
  prepare:
    runs-on: ubuntu-latest
    steps:
      - run: true
  direct:
    runs-on: ubuntu-latest
    env:
      DIRECT: ${{ secrets.DIRECT }}
    steps:
      - run: true
  call:
    needs: prepare
    uses: ./.github/workflows/reusable.yml
    if: needs.prepare.result == 'success'
    secrets: inherit
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  action:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/secret
        with:
          token: ${{ secrets.ACTION_TOKEN }}-${{ secrets.ALPHA }}-${{ secrets.ALPHA }}
  matrix:
    strategy:
      matrix:
        enabled: [true, false]
    if: matrix.enabled
    runs-on: ubuntu-latest
    env:
      BETA: ${{ secrets.BETA }}
      GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    steps:
      - run: true
  unprivileged:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	options := defaultOptions()
	options.ResolveActions = true
	plans, err := compilePlansForTest(t.Context(), path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 6 {
		t.Fatalf("plans = %d, want two direct jobs and four expanded callee jobs", len(plans))
	}
	byID := map[string][]plan.Job{}
	for _, job := range plans {
		byID[job.Workflow.LogicalJobID] = append(byID[job.Workflow.LogicalJobID], job)
	}
	direct := byID["direct"][0]
	if !slices.Equal(direct.RequiredSecrets, []string{"DIRECT"}) || !direct.HasCapability("secrets") {
		t.Fatalf("direct caller secret boundary = %#v / %#v", direct.RequiredSecrets, direct.RequiredCapabilities)
	}
	action := byID["call.action"][0]
	if !slices.Equal(action.RequiredSecrets, []string{"ACTION_TOKEN", "ALPHA"}) || !action.HasCapability("secrets") {
		t.Fatalf("action callee secret boundary = %#v / %#v", action.RequiredSecrets, action.RequiredCapabilities)
	}
	if len(action.CallGuards) != 1 || action.CallGuards[0].Condition != "(needs.prepare.result == 'success')" {
		t.Fatalf("action callee call guards = %#v", action.CallGuards)
	}
	matrix := byID["call.matrix"]
	if len(matrix) != 2 {
		t.Fatalf("matrix plans = %d, want 2", len(matrix))
	}
	conditions := map[string]bool{}
	for _, job := range matrix {
		if !slices.Equal(job.RequiredSecrets, []string{"BETA"}) || !job.HasCapability("secrets") {
			t.Fatalf("matrix callee authority = secrets %#v, token %#v, capabilities %#v", job.RequiredSecrets, job.GitHubToken, job.RequiredCapabilities)
		}
		if reachable := job.Condition != "false"; reachable != (job.GitHubToken != nil && job.HasCapability("provider-token-write")) {
			t.Fatalf("matrix callee condition %q token authority = %#v / %#v", job.Condition, job.GitHubToken, job.RequiredCapabilities)
		}
		conditions[job.Condition] = true
	}
	if !conditions["matrix.enabled"] || !conditions["false"] {
		t.Fatalf("matrix callee conditions = %#v, want enabled and statically skipped jobs", conditions)
	}
	unprivileged := byID["call.unprivileged"][0]
	if len(unprivileged.RequiredSecrets) != 0 || unprivileged.HasCapability("secrets") || unprivileged.GitHubToken != nil {
		t.Fatalf("unprivileged callee authority = %#v", unprivileged)
	}
}

func TestCompileDoesNotGrantUninheritedReusableWorkflowSecrets(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: true
        env:
          OPTIONAL_TOKEN: ${{ secrets.OPTIONAL_TOKEN }}
`)
	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].RequiredSecrets) != 0 || plans[0].HasCapability("secrets") {
		t.Fatalf("uninherited reusable secrets = plans %#v", plans)
	}
}

func TestCompileRequiresSecretInheritanceOnEveryReusableWorkflowEdge(t *testing.T) {
	for _, test := range []struct {
		name          string
		nestedInherit bool
		wantSecrets   []string
	}{
		{name: "nested edge omits inherit"},
		{name: "every edge inherits", nestedInherit: true, wantSecrets: []string{"NESTED_TOKEN"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/middle.yml
    secrets: inherit
`)
			nestedSecrets := ""
			if test.nestedInherit {
				nestedSecrets = "    secrets: inherit\n"
			}
			writeWorkflow(t, repository, "middle.yml", "on: workflow_call\njobs:\n  nested:\n    uses: ./.github/workflows/leaf.yml\n"+nestedSecrets)
			writeWorkflow(t, repository, "leaf.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      TOKEN: ${{ secrets.NESTED_TOKEN }}
    steps:
      - run: true
`)
			plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
			if err != nil {
				t.Fatal(err)
			}
			if len(plans) != 1 || !slices.Equal(plans[0].RequiredSecrets, test.wantSecrets) || plans[0].HasCapability("secrets") != (len(test.wantSecrets) != 0) {
				t.Fatalf("nested secret boundary = %#v / %#v", plans[0].RequiredSecrets, plans[0].RequiredCapabilities)
			}
		})
	}
}

func TestCompileRejectsNonStaticInheritedReusableWorkflowSecrets(t *testing.T) {
	for _, test := range []struct {
		name       string
		expression string
	}{
		{name: "dynamic", expression: "secrets[env.NAME]"},
		{name: "whole context", expression: "toJSON(secrets)"},
		{name: "filtered", expression: "secrets.*"},
		{name: "projected", expression: "secrets.*.name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    secrets: inherit\n")
			writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      VALUE: ${{ "+test.expression+" }}\n    steps: [{run: true}]\n")
			_, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), "gha-untrusted")
			if err == nil {
				t.Fatalf("compile accepted inherited %s secrets access", test.name)
			}
		})
	}
}

func TestCompileRejectsInheritedSecretAuthorityFromCompositeMetadata(t *testing.T) {
	repository := t.TempDir()
	writeAction(t, repository, ".github/actions/child", `name: child
inputs:
  token:
    required: true
runs:
  using: node24
  main: index.js
`)
	writeAction(t, repository, ".github/actions/parent", `name: parent
runs:
  using: composite
  steps:
    - uses: ./.github/actions/child
      with:
        token: ${{ secrets.METADATA_TOKEN }}
`)
	path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    secrets: inherit\n")
	writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/parent\n")
	options := defaultOptions()
	options.ResolveActions = true
	_, err := compilePlansForTest(t.Context(), path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.1.0", "sha256:"+strings.Repeat("a", 64), options)
	if err == nil || !strings.Contains(err.Error(), "composite action metadata cannot grant secret authority") {
		t.Fatalf("compile metadata secret error = %v", err)
	}
}

func TestCompileResolvesEventBackedReusableWorkflowInputs(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: pull_request
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    with:
      is-trunk: ${{ github.ref == 'refs/heads/trunk' }}
      is-public-fork: ${{ github.event.pull_request.head.repo.fork || false }}
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      is-trunk:
        type: boolean
      is-public-fork:
        type: boolean
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.is-trunk }} ${{ inputs.is-public-fork }}
`)
	event := []byte(`{
  "provider": "github",
  "event": "pull_request",
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": "refs/pull/42/merge",
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": {"pull_request": {"head": {"repo": {"fork": true}}}}
}`)
	result, err := Compile(path, readFile(t, path), event)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || len(ir.Jobs[0].Steps) != 1 || ir.Jobs[0].Steps[0].Run != "echo false true" {
		t.Fatalf("event-backed reusable inputs = %#v", ir.Jobs)
	}
}

func TestCompileEvaluatesReusableWorkflowInputDefaults(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n")
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      label:
        type: string
        default: ${{ format('{0}-{1}', github.event_name, vars.SUFFIX) }}
      enabled:
        type: boolean
        default: ${{ github.ref == 'refs/heads/main' }}
      count:
        type: number
        default: ${{ fromJSON(vars.COUNT) }}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.label }}:${{ inputs.enabled }}:${{ inputs.count }}
`)
	result, err := CompileWithOptions(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"COUNT": "3", "SUFFIX": "release"}},
		Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "linux"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || ir.Jobs[0].Steps[0].Run != "echo push-release:true:3" {
		t.Fatalf("reusable defaults = %#v", ir.Jobs)
	}
}

func TestCompileRejectsDeferredNonStringReusableWorkflowInput(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      enabled: ${{ steps.value.outputs.enabled }}
    steps:
      - id: value
        run: echo enabled=true >> "$GITHUB_OUTPUT"
  call:
    needs: prepare
    uses: ./.github/workflows/reusable.yml
    with:
      enabled: ${{ needs.prepare.outputs.enabled }}
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      enabled: {type: boolean}
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `deferred reusable-workflow input "enabled" must be string`) {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestCompileRejectsDeferredReusableInputInMatrixInclude(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on:
  workflow_dispatch:
    inputs:
      EXTRA:
        type: string
        default: '[{"name":"root"}]'
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      extra: ${{ steps.value.outputs.extra }}
    steps:
      - id: value
        run: echo 'extra=[]' >> "$GITHUB_OUTPUT"
  call:
    needs: prepare
    uses: ./.github/workflows/reusable.yml
    with:
      EXTRA: ${{ needs.prepare.outputs.extra }}
`)
	writeWorkflow(t, repository, "reusable.yml", `on:
  workflow_call:
    inputs:
      EXTRA: {type: string}
jobs:
  test:
    strategy:
      matrix:
        include: ${{ fromJSON(inputs.EXTRA) }}
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	var event map[string]any
	if err := json.Unmarshal(readFile(t, smokePath("events", "push.json")), &event); err != nil {
		t.Fatal(err)
	}
	event["event"] = "workflow_dispatch"
	event["payload"] = map[string]any{"inputs": map[string]any{"EXTRA": `[{"name":"dispatch"}]`}}
	eventSource, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compile(path, readFile(t, path), eventSource)
	if err == nil || !strings.Contains(err.Error(), "reusable-workflow input expression is not statically resolvable") {
		t.Fatalf("Compile() error = %v, want deferred matrix include rejection", err)
	}
}

func TestValidateRejectsInvalidReusableInputDefaultWithoutCall(t *testing.T) {
	source := []byte(`on:
  push:
  workflow_call:
    inputs:
      value:
        type: string
        default: ${{ secrets.TOKEN }}
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	if _, err := Validate("workflow.yml", source); err == nil || !strings.Contains(err.Error(), `default for workflow_call input "value" is invalid`) || !strings.Contains(err.Error(), `context "secrets" is unavailable`) {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestCompileValidatesReusableWorkflowInputDefaults(t *testing.T) {
	for _, test := range []struct {
		name     string
		input    string
		want     string
		override bool
	}{
		{name: "dispatch inputs", input: "${{ inputs.other }}", want: "workflow-dispatch inputs, which are unavailable during compilation", override: true},
		{name: "token in lazy branch", input: "${{ false && github.token || 'safe' }}", want: `unavailable value "github.token"`, override: true},
		{name: "type mismatch", input: "${{ github.ref == 'refs/heads/main' }}", want: "must be string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			with := ""
			if test.override {
				with = "    with:\n      value: override\n"
			}
			path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n"+with)
			writeWorkflow(t, repository, "reusable.yml", "on:\n  workflow_call:\n    inputs:\n      value:\n        type: string\n        default: "+test.input+"\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
			_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), ".github/workflows/reusable.yml:6:") {
				t.Fatalf("Compile() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompileResolvesCompoundNestedReusableWorkflowInputs(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/middle.yml
    with:
      prefix: ${{ format('{0}-{1}', vars.PREFIX, github.event_name) }}
`)
	writeWorkflow(t, repository, "middle.yml", `on:
  workflow_call:
    inputs:
      prefix: {type: string}
jobs:
  call:
    strategy:
      matrix:
        os: [linux]
    uses: ./.github/workflows/leaf.yml
    with:
      value: ${{ format('{0}-{1}-{2}', inputs.prefix, matrix.os, github.event_name) }}
`)
	writeWorkflow(t, repository, "leaf.yml", `on:
  workflow_call:
    inputs:
      value: {type: string}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.value }}
`)
	result, err := CompileWithOptions(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), Options{
		EventTrust: EventTrusted,
		Vars:       VariableSources{Bridge: map[string]string{"PREFIX": "release"}},
		Runners:    RunnerPolicy{Labels: map[string]string{"ubuntu-latest": "linux"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || ir.Jobs[0].Steps[0].Run != "echo release-push-linux-push" {
		t.Fatalf("nested reusable input = %#v", ir.Jobs)
	}
}

func TestCompileExposesGitHubHeadRef(t *testing.T) {
	workflow := []byte(`on: [push, pull_request, pull_request_target]
concurrency: group-${{ github.head_ref }}
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ github.head_ref }}"
`)
	tests := []struct {
		name    string
		event   string
		payload string
		want    string
	}{
		{name: "pull request", event: "pull_request", payload: `{"pull_request":{"head":{"ref":"feature/pr"}}}`, want: "feature/pr"},
		{name: "pull request target", event: "pull_request_target", payload: `{"pull_request":{"head":{"ref":"feature/target"}}}`, want: "feature/target"},
		{name: "push ignores pull request shape", event: "push", payload: `{"pull_request":{"head":{"ref":"not-a-pr"}}}`, want: ""},
		{name: "missing pull request shape", event: "pull_request", payload: `{}`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := fmt.Appendf(nil, `{
  "provider": "github",
  "event": %q,
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": "refs/heads/main",
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": %s
}`, test.event, test.payload)
			result, err := Compile("ci.yml", workflow, event)
			if err != nil {
				t.Fatal(err)
			}
			var ir IR
			if err := json.Unmarshal(result, &ir); err != nil {
				t.Fatal(err)
			}
			if ir.Workflow.ConcurrencyGroup != "group-"+test.want {
				t.Fatalf("github.head_ref concurrency group = %q, want %q", ir.Workflow.ConcurrencyGroup, "group-"+test.want)
			}
		})
	}
}

func TestCompileFoldsGitHubRefIdentityInConditions(t *testing.T) {
	workflow := []byte(`on: [push, pull_request, pull_request_target]
jobs:
  test:
    if: always() && github.repository_owner == 'buildkite' && github.ref_name && github.ref_type && github.base_ref == ''
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	tests := []struct {
		name, event, ref, payload, wantBaseRef, wantCondition string
	}{
		{name: "branch", event: "push", ref: "refs/heads/feature/runtime", payload: `{}`, wantCondition: "(always() && true)"},
		{name: "tag", event: "push", ref: "refs/tags/v1.2.3", payload: `{}`, wantCondition: "(always() && true)"},
		{name: "pull request", event: "pull_request", ref: "refs/pull/42/merge", payload: `{"pull_request":{"base":{"ref":"main"},"head":{"ref":"feature/pr"}}}`, wantBaseRef: "main", wantCondition: "(always() && false)"},
		{name: "pull request target", event: "pull_request_target", ref: "refs/heads/main", payload: `{"pull_request":{"base":{"ref":"main"},"head":{"ref":"feature/target"}}}`, wantBaseRef: "main", wantCondition: "(always() && false)"},
		{name: "missing pull request shape", event: "pull_request", ref: "refs/pull/42/merge", payload: `{}`, wantCondition: "(always() && true)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := fmt.Appendf(nil, `{
  "provider": "github",
  "event": %q,
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": %q,
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": %s
}`, test.event, test.ref, test.payload)
			plans, err := compileUntrustedPlans("ci.yml", workflow, event, "0.0.0-test", testDistributionDigest, "gha-untrusted")
			if err != nil {
				t.Fatal(err)
			}
			if len(plans) != 1 || plans[0].Event.BaseRef != test.wantBaseRef || plans[0].Condition != test.wantCondition {
				t.Fatalf("compile-time event identity plan = %#v", plans)
			}
		})
	}
}

func TestCompileFoldsEventBackedConditionsBeforeRuntime(t *testing.T) {
	workflow := []byte(`on: pull_request
jobs:
  configure:
    runs-on: ubuntu-latest
    steps:
      - if: github.event_name == 'pull_request' && github.event.pull_request.draft
        run: echo draft
`)
	event := []byte(`{
  "provider": "github",
  "event": "pull_request",
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": "refs/pull/42/merge",
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": {"pull_request": {"draft": false}}
}`)
	result, err := Compile("ci.yml", workflow, event)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || len(ir.Jobs[0].Steps) != 1 || ir.Jobs[0].Steps[0].If != "false" {
		t.Fatalf("compile-time event condition = %#v", ir.Jobs)
	}
}

func TestCompilePartiallyReducesEventBackedConditionsBeforeRuntime(t *testing.T) {
	workflow := []byte(`on: pull_request
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: true
  configure:
    needs: build
    if: github.event.pull_request.draft && needs.build.result == 'success'
    runs-on: ubuntu-latest
    steps:
      - if: github.event.pull_request.draft && failure()
        run: echo draft failure
`)
	event := []byte(`{
  "provider": "github",
  "event": "pull_request",
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": "refs/pull/42/merge",
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": {"pull_request": {"draft": true}}
}`)
	result, err := Compile("ci.yml", workflow, event)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	var configure *JobInstance
	for i := range ir.Jobs {
		if ir.Jobs[i].LogicalJobID == "configure" {
			configure = &ir.Jobs[i]
		}
	}
	if configure == nil || configure.If != "(true && (needs.build.result == 'success'))" || len(configure.Steps) != 1 || configure.Steps[0].If != "(true && failure())" {
		t.Fatalf("partially reduced conditions = %#v", configure)
	}
	if strings.Contains(strings.ToLower(configure.If), "github.event") {
		t.Fatalf("job condition retains github.event: %q", configure.If)
	}
}

func TestCompilePreservesStatusFunctionsWhenFoldingEventConditions(t *testing.T) {
	workflow := []byte(`on: pull_request
jobs:
  configure:
    if: github.event.pull_request.draft || always()
    runs-on: ubuntu-latest
    steps:
      - if: github.event.pull_request.draft || failure()
        run: echo draft
`)
	event := []byte(`{
  "provider": "github",
  "event": "pull_request",
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": "refs/pull/42/merge",
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": {"pull_request": {"draft": true}}
}`)
	result, err := Compile("ci.yml", workflow, event)
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(result, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Jobs) != 1 || ir.Jobs[0].If != "always()" || len(ir.Jobs[0].Steps) != 1 || ir.Jobs[0].Steps[0].If != "always()" {
		t.Fatalf("status-aware compile-time event condition = %#v", ir.Jobs)
	}
}

func TestCompileValidatesEveryEventConditionBranchBeforeFolding(t *testing.T) {
	event := []byte(`{
  "provider": "github",
  "event": "pull_request",
  "repository": {"owner": "buildkite", "name": "kafka"},
  "ref": "refs/pull/42/merge",
  "sha": "1111111111111111111111111111111111111111",
  "actor": "buildkite-gha",
  "payload": {"pull_request": {"draft": false}}
}`)
	for _, test := range []struct {
		name, workflow, want string
	}{
		{
			name: "job unsupported function after false event operand",
			workflow: `on: pull_request
jobs:
  configure:
    runs-on: ubuntu-latest
    if: github.event.pull_request.draft && hashFiles('go.sum') != ''
    steps:
      - run: echo draft
`,
			want: `ci.yml:5:9: job "configure": job condition: condition function "hashFiles" is unsupported`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, err := ValidateEvent("ci.yml", []byte(test.workflow), event)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateEvent() error = %v, want %q", err, test.want)
			}
			if report.LogicalJobs != 1 || report.Instances != 1 || len(report.Jobs) != 1 {
				t.Fatalf("failed condition report = %#v", report)
			}
		})
	}
}

func TestCompileRejectsUndeclaredExplicitReusableWorkflowSecretMapping(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  call:
    uses: ./.github/workflows/reusable.yml
    secrets:
      token: ${{ secrets.TOKEN }}
`)
	writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n")
	_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `secret mapping target "TOKEN" is not declared`) {
		t.Fatalf("Compile() error = %v, want undeclared explicit secret mapping rejection", err)
	}
}

func TestCompileReusableInputDiagnosticsAreDeterministic(t *testing.T) {
	repository := t.TempDir()
	path := writeWorkflow(t, repository, "caller.yml", "on: push\njobs:\n  call:\n    uses: ./.github/workflows/reusable.yml\n    with:\n      zed: z\n      alpha: a\n")
	writeWorkflow(t, repository, "reusable.yml", "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	for range 20 {
		_, err := Compile(path, readFile(t, path), readFile(t, smokePath("events", "push.json")))
		if err == nil || !strings.Contains(err.Error(), `input "alpha" is not declared`) {
			t.Fatalf("Compile() error = %v, want alphabetically first input diagnostic", err)
		}
	}
}

func writeWorkflow(t *testing.T, repository, name, source string) string {
	t.Helper()
	path := filepath.Join(repository, ".github", "workflows", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompileRejectsRuntimeMatrixSourceOutsideDirectNeeds(t *testing.T) {
	source := []byte(`name: dynamic
on: push
jobs:
  generated:
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}
    steps:
      - run: true
`)
	_, err := Compile("dynamic.yml", source, readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `runtime matrix producer "prepare" must be a direct prerequisite in needs`) {
		t.Fatalf("Compile() error = %v, want direct producer rejection", err)
	}
	if !strings.Contains(err.Error(), "dynamic.yml:7:15") {
		t.Fatalf("Compile() error = %v, want source location", err)
	}
}

func TestValidateRetainsDependentCandidateAsNotEvaluatedAfterMatrixFailure(t *testing.T) {
	source := []byte(`on: push
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.value }}
    steps:
      - id: matrix
        run: true
  upstream:
    needs: prepare
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}
    steps:
      - run: true
  downstream:
    needs: upstream
    runs-on: ubuntu-latest
    steps:
      - run: true
`)
	report, err := Validate("failed-prerequisite.yml", source)
	if err == nil || !strings.Contains(err.Error(), "runtime matrix source is valid, but continuation upload is disabled") {
		t.Fatalf("Validate() error = %v", err)
	}
	if strings.Contains(err.Error(), "has no expanded instances") {
		t.Fatalf("Validate() added cascading graph failure: %v", err)
	}
	if len(report.RuntimeMatrices) != 1 || report.RuntimeMatrices[0].Shape != RuntimeMatrixShapeObject || report.RuntimeMatrices[0].ProducerJob != "prepare" || report.RuntimeMatrices[0].ProducerStepKey != "gha-prepare" || report.RuntimeMatrices[0].ProducerOutput != "matrix" {
		t.Fatalf("runtime matrix descriptors = %#v", report.RuntimeMatrices)
	}
	if !report.NotEvaluatedJobs["downstream"] || !report.NotEvaluatedInstances["gha-downstream"] {
		t.Fatalf("not-evaluated ledger = jobs %#v, instances %#v", report.NotEvaluatedJobs, report.NotEvaluatedInstances)
	}
}

func TestValidateRecognizesPinnedPostHogRuntimeMatrixIncludeBoundary(t *testing.T) {
	source := []byte(`on: push
jobs:
  build_django_matrix:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.build.outputs.include }}
    steps:
      - id: build
        run: echo 'include=[]' >> "$GITHUB_OUTPUT"
  django:
    needs: build_django_matrix
    runs-on: depot-ubuntu-24.04
    strategy:
      fail-fast: false
      matrix:
        include: ${{ fromJson(needs.build_django_matrix.outputs.include) }}
    steps:
      - run: echo "${{ matrix.artifact_key }}"
`)
	report, err := Validate(".github/workflows/ci-backend.yml", source)
	if err == nil || !strings.Contains(err.Error(), "continuation upload is disabled") {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(report.RuntimeMatrices) != 1 {
		t.Fatalf("runtime matrix descriptors = %#v", report.RuntimeMatrices)
	}
	descriptor := report.RuntimeMatrices[0]
	if descriptor.Job != "django" || descriptor.Shape != RuntimeMatrixShapeInclude || descriptor.ProducerJob != "build_django_matrix" || descriptor.ProducerStepKey != "gha-build_django_matrix" || descriptor.ProducerOutput != "include" || descriptor.Source.Start.Line != 16 {
		t.Fatalf("PostHog runtime matrix descriptor = %#v", descriptor)
	}
}

func TestValidateRecognizesRuntimeMatrixInsideReusableWorkflow(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      include: ${{ steps.build.outputs.include }}
    steps:
      - id: build
        run: echo 'include=[]' >> "$GITHUB_OUTPUT"
  generated:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    steps:
      - run: echo "${{ matrix.name }}"
`)

	report, err := Validate(callerPath, readFile(t, callerPath))
	if err == nil || !strings.Contains(err.Error(), "continuation upload is disabled") {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(report.RuntimeMatrices) != 1 {
		t.Fatalf("runtime matrix descriptors = %#v", report.RuntimeMatrices)
	}
	descriptor := report.RuntimeMatrices[0]
	if descriptor.Job != "delegated.generated" || descriptor.ProducerJob != "delegated.producer" || descriptor.ProducerStepKey != "gha-delegated-producer" || descriptor.ProducerOutput != "include" || descriptor.SourcePath != "./.github/workflows/reusable.yml" {
		t.Fatalf("reusable runtime matrix descriptor = %#v", descriptor)
	}
}

func TestValidateResolvesRuntimeMatrixReusableWorkflowOutputProjection(t *testing.T) {
	repository := t.TempDir()
	callerPath := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  producer:
    uses: ./.github/workflows/producer.yml
  generated:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
    steps:
      - run: echo "${{ matrix.name }}"
`)
	writeWorkflow(t, repository, "producer.yml", `on:
  workflow_call:
    outputs:
      include:
        value: ${{ jobs.build.outputs.matrix }}
jobs:
  auxiliary:
    runs-on: ubuntu-latest
    steps:
      - run: true
  build:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.build.outputs.matrix }}
    steps:
      - id: build
        run: echo 'matrix=[]' >> "$GITHUB_OUTPUT"
`)

	report, err := Validate(callerPath, readFile(t, callerPath))
	if err == nil || !strings.Contains(err.Error(), "continuation upload is disabled") {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(report.RuntimeMatrices) != 1 {
		t.Fatalf("runtime matrix descriptors = %#v", report.RuntimeMatrices)
	}
	descriptor := report.RuntimeMatrices[0]
	if descriptor.Job != "generated" || descriptor.ProducerJob != "producer.build" || descriptor.ProducerStepKey != "gha-producer-build" || descriptor.ProducerOutput != "matrix" {
		t.Fatalf("projected runtime matrix descriptor = %#v", descriptor)
	}
}

func TestValidateRuntimeMatrixDescriptorRejectsUnsupportedSourceBoundaries(t *testing.T) {
	for _, test := range []struct {
		name     string
		producer string
		matrix   string
		want     string
	}{
		{
			name:     "undeclared output",
			producer: "",
			matrix:   "      matrix: ${{ fromJSON(needs.producer.outputs.include) }}",
			want:     `output "include" is not declared`,
		},
		{
			name: "matrix producer is ambiguous",
			producer: `    strategy:
      matrix:
        shard: [1, 2]
`,
			matrix: "      matrix: ${{ fromJSON(needs.producer.outputs.include) }}",
			want:   "must have exactly one statically expanded instance",
		},
		{
			name:     "runtime include mixed with static dimensions",
			producer: "",
			matrix: `      matrix:
        os: [ubuntu-latest]
        include: ${{ fromJSON(needs.producer.outputs.include) }}`,
			want: "must be the complete matrix definition",
		},
		{
			name:     "runtime include mixed with empty exclude",
			producer: "",
			matrix: `      matrix:
        include: ${{ fromJSON(needs.producer.outputs.include) }}
        exclude: []`,
			want: `"exclude" section should not be empty`,
		},
		{
			name:     "composed expression",
			producer: "",
			matrix:   "      matrix: ${{ fromJSON(needs.producer.outputs.include || '[]') }}",
			want:     "runtime-dependent matrix expressions are unsupported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			outputs := `    outputs:
      include: ${{ steps.matrix.outputs.include }}
`
			if test.name == "undeclared output" {
				outputs = ""
			}
			source := []byte("on: push\njobs:\n  producer:\n    runs-on: ubuntu-latest\n" + test.producer + outputs + "    steps:\n      - id: matrix\n        run: true\n  generated:\n    needs: producer\n    runs-on: ubuntu-latest\n    strategy:\n" + test.matrix + "\n    steps:\n      - run: true\n")
			report, err := Validate("runtime-matrix.yml", source)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q\n%s", err, test.want, source)
			}
			if len(report.RuntimeMatrices) != 0 {
				t.Fatalf("invalid source produced descriptors %#v", report.RuntimeMatrices)
			}
		})
	}
}

func TestParseWorkflowDoesNotEvaluateEventDependentJobs(t *testing.T) {
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ${{ github.event.runner }}\n    steps:\n      - run: true\n")
	report, err := ParseWorkflow("event.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	if report.LogicalJobs != 1 || len(report.ParsedJobs) != 1 || report.ParsedJobs[0].ID != "test" || report.Instances != 0 || len(report.Jobs) != 0 {
		t.Fatalf("parse report = %#v", report)
	}
}

func TestCompilePlansOwnsDeterministicRuntimeInputs(t *testing.T) {
	path := smokePath(".github", "workflows", "plan-fixture.yml")
	source := []byte(`name: plan fixture
on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      result: ${{ steps.composite.outputs.result }}
    steps:
      - run: echo "result=smoke" >> "$GITHUB_OUTPUT"
      - id: step-1
        run: true
      - id: javascript
        uses: ./.github/actions/javascript
        with:
          message: ${{ steps.step-1.outputs.result }}
      - id: composite
        uses: ./.github/actions/composite
        with:
          message: ${{ steps.javascript.outputs.result }}
`)
	plans, err := compileUntrustedPlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	producer := plans[0]
	if producer.Target.StepKey != "gha-producer" || producer.Workflow.LogicalJobID != "producer" {
		t.Fatalf("producer target = %#v, workflow = %#v", producer.Target, producer.Workflow)
	}
	if producer.Steps[0].ID != "step-1-2" || producer.Steps[1].ID != "step-1" {
		t.Fatalf("deterministic step ids = %q, %q", producer.Steps[0].ID, producer.Steps[1].ID)
	}
	if producer.Steps[2].With["message"] != "${{ steps.step-1.outputs.result }}" {
		t.Fatalf("JavaScript inputs = %#v", producer.Steps[2].With)
	}
	if producer.Outputs["result"] != "${{ steps.composite.outputs.result }}" {
		t.Fatalf("job outputs = %#v", producer.Outputs)
	}
	if producer.RequiredCapabilities == nil || len(producer.RequiredCapabilities) != 0 {
		t.Fatalf("required capabilities = %#v, want concrete empty array", producer.RequiredCapabilities)
	}
	second, err := compileUntrustedPlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(plans)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("repeated plan compilation was not byte-identical")
	}
}

func TestCompilePlansOwnsConcurrentStepControls(t *testing.T) {
	path := smokePath(".github", "workflows", "concurrent.yml")
	source := []byte(`name: concurrent plan
on: push
jobs:
  concurrent:
    runs-on: ubuntu-latest
    steps:
      - id: first
        run: echo first
        background: true
      - id: second
        run: echo second
        background: true
        continue-on-error: true
      - wait: [first, second]
      - wait-all:
      - cancel: first
      - parallel:
          - run: echo ${{ secrets.PARALLEL_TOKEN }}
            env:
              PARALLEL_SECRET: ${{ secrets.PARALLEL_TOKEN }}
          - id: named-parallel
            run: echo named
`)
	plans, err := compileUntrustedPlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.Schema {
		t.Fatalf("plans = %#v, want one current plan", plans)
	}
	steps := plans[0].Steps
	if len(steps) != 8 || !steps[0].Background || !steps[1].Background {
		t.Fatalf("background plan steps = %#v", steps)
	}
	if !steps[1].ContinueOnError || steps[2].Kind != "wait" || !reflect.DeepEqual(steps[2].Targets, []string{"first", "second"}) || steps[2].ContinueOnError {
		t.Fatalf("targeted plan barrier = %#v", steps[2])
	}
	if steps[3].Kind != "wait-all" || steps[4].Kind != "cancel" || !reflect.DeepEqual(steps[4].Targets, []string{"first"}) {
		t.Fatalf("plan controls = %#v", steps[3:])
	}
	if !steps[5].Background || !steps[6].Background || steps[5].ID == "" || steps[6].ID != "named-parallel" {
		t.Fatalf("lowered parallel members = %#v", steps[5:7])
	}
	if steps[7].Kind != "wait" || !reflect.DeepEqual(steps[7].Targets, []string{steps[5].ID, "named-parallel"}) {
		t.Fatalf("lowered parallel barrier = %#v", steps[7])
	}
	if !reflect.DeepEqual(plans[0].RequiredSecrets, []string{"PARALLEL_TOKEN"}) || !plans[0].HasCapability("secrets") {
		t.Fatalf("parallel secrets = %#v, capabilities = %#v", plans[0].RequiredSecrets, plans[0].RequiredCapabilities)
	}
}

func TestConcurrentSmokeTerminalKeyMatchesNativeContinuation(t *testing.T) {
	path := smokePath(".github", "workflows", "concurrent.yml")
	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Target.StepKey != "gha-concurrent" || plans[1].Target.StepKey != "gha-observe" {
		t.Fatalf("concurrent smoke targets = %#v", plans)
	}
	if !reflect.DeepEqual(plans[1].Dependencies, []string{"gha-concurrent"}) {
		t.Fatalf("observer dependencies = %#v", plans[1].Dependencies)
	}
}

func TestCompilePlansRecordsStaticDependenciesWithVerifiedNeedSources(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	plans, err := compileUntrustedPlans(path, readFile(t, path), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 || len(plans[0].Dependencies) != 0 {
		t.Fatalf("plans = %#v, want producer plus two consumers", plans)
	}
	for _, consumer := range plans[1:] {
		producerContents, err := plan.Encode(plans[0])
		if err != nil {
			t.Fatal(err)
		}
		wantSources := map[string][]plan.NeedSource{"producer": {{StepKey: plans[0].Target.StepKey, PlanDigest: transport.Digest(producerContents)}}}
		if !reflect.DeepEqual(consumer.Dependencies, []string{plans[0].Target.StepKey}) || !reflect.DeepEqual(consumer.NeedSources, wantSources) || consumer.Needs != nil {
			t.Fatalf("consumer dependencies/sources/results = %#v / %#v / %#v", consumer.Dependencies, consumer.NeedSources, consumer.Needs)
		}
	}
}

func TestCompilePlansMapsLogicalNeedToEveryMatrixProducer(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        variant: [one, two]
    steps:
      - run: echo ok
  consume:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: echo done
`)
	plans, err := compileUntrustedPlans("matrix.yml", source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Fatalf("plans = %d, want two producers and one consumer", len(plans))
	}
	wantSources := make([]plan.NeedSource, 2)
	for i := range 2 {
		encoded, err := plan.Encode(plans[i])
		if err != nil {
			t.Fatal(err)
		}
		wantSources[i] = plan.NeedSource{StepKey: plans[i].Target.StepKey, PlanDigest: transport.Digest(encoded)}
	}
	sort.Slice(wantSources, func(i, j int) bool { return wantSources[i].StepKey < wantSources[j].StepKey })
	if !reflect.DeepEqual(plans[2].NeedSources, map[string][]plan.NeedSource{"build": wantSources}) {
		t.Fatalf("need sources = %#v, want exact matrix fan-in %#v", plans[2].NeedSources, wantSources)
	}
}

func TestCompilePlansDerivesDockerCapability(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "docker.yml")
	actionDir := filepath.Join(repository, ".github", "actions", "docker")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("runs:\n  using: docker\n  image: Dockerfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := []byte("on: push\njobs:\n  docker:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/docker\n")
	plans, err := compileUntrustedPlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if got := plans[0].RequiredCapabilities; !reflect.DeepEqual(got, []string{"docker", "network"}) {
		t.Fatalf("required capabilities = %#v, want [docker network]", got)
	}
}

func TestCollectNestedActionCapabilities(t *testing.T) {
	root := t.TempDir()
	write := func(name, source string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "action.yml"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("docker", "runs:\n  using: docker\n  image: Dockerfile\n")
	write("inner", "runs:\n  using: composite\n  steps:\n    - uses: ./docker\n")
	write("outer", "runs:\n  using: composite\n  steps:\n    - uses: ./inner\n")
	capabilities := map[string]struct{}{}
	if err := collectActionCapabilities(root, "./outer", capabilities, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := capabilities["docker"]; !ok {
		t.Fatalf("capabilities = %#v, want nested docker", capabilities)
	}

	write("remote", "runs:\n  using: composite\n  steps:\n    - uses: owner/action@v1\n")
	if err := collectActionCapabilities(root, "./remote", map[string]struct{}{}, nil); err == nil || !strings.Contains(err.Error(), `nested remote action "owner/action@v1" is unsupported`) {
		t.Fatalf("remote error = %v", err)
	}
	write("recursive", "runs:\n  using: composite\n  steps:\n    - uses: ./recursive\n")
	if err := collectActionCapabilities(root, "./recursive", map[string]struct{}{}, nil); err == nil || !strings.Contains(err.Error(), "recursion detected") {
		t.Fatalf("recursion error = %v", err)
	}

	for i := 0; i <= metadata.MaxNestedActionDepth; i++ {
		next := ""
		if i < metadata.MaxNestedActionDepth {
			next = fmt.Sprintf("  steps:\n    - uses: ./depth-%d\n", i+1)
		}
		write(fmt.Sprintf("depth-%d", i), "runs:\n  using: composite\n"+next)
	}
	if err := collectActionCapabilities(root, "./depth-0", map[string]struct{}{}, nil); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds maximum depth %d", metadata.MaxNestedActionDepth)) {
		t.Fatalf("depth error = %v", err)
	}
}

func TestCompilePlansAcceptsSupportedJavaScriptLocalActions(t *testing.T) {
	for _, runtime := range []string{"node16", "node20", "node24"} {
		t.Run(runtime, func(t *testing.T) {
			repository := t.TempDir()
			workflowPath := filepath.Join(repository, ".github", "workflows", runtime+".yml")
			actionDir := filepath.Join(repository, ".github", "actions", runtime)
			if err := os.MkdirAll(actionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("runs:\n  using: "+runtime+"\n  main: index.js\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(actionDir, "index.js"), []byte("// fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			source := []byte("on: push\njobs:\n  javascript:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/" + runtime + "\n")
			plans, err := compileUntrustedPlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
			if err != nil {
				t.Fatalf("compileUntrustedPlans() error = %v, want %s support", err, runtime)
			}
			if len(plans) != 1 || !plans[0].NeedsMise() {
				t.Fatalf("compileUntrustedPlans() plans = %#v, want one %s job requiring mise", plans, runtime)
			}
		})
	}
}

func TestCompilePlansRejectsRuntimeInvalidActionMetadata(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "invalid.yml")
	actionDir := filepath.Join(repository, ".github", "actions", "invalid")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("unexpected: true\nruns:\n  using: node24\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := []byte("on: push\njobs:\n  invalid:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/invalid\n")
	_, err := compileUntrustedPlans(workflowPath, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "gha-untrusted")
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("compileUntrustedPlans() error = %v, want strict action metadata rejection", err)
	}
}

func TestLocalActionCapabilityResolutionStaysWithinRepository(t *testing.T) {
	repository := t.TempDir()
	workflowDir := filepath.Join(repository, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := loadLocalAction(repository, "./../../outside")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("loadLocalAction() error = %v, want repository escape error", err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "action.yml"), []byte("runs:\n  using: node24\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	actionsDir := filepath.Join(repository, ".github", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(actionsDir, "escaped")); err != nil {
		t.Fatal(err)
	}
	_, err = loadLocalAction(repository, "./.github/actions/escaped")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("loadLocalAction() symlink error = %v, want repository escape error", err)
	}
	metadataEscape := filepath.Join(actionsDir, "metadata-escaped")
	if err := os.MkdirAll(metadataEscape, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "action.yml"), filepath.Join(metadataEscape, "action.yml")); err != nil {
		t.Fatal(err)
	}
	_, err = loadLocalAction(repository, "./.github/actions/metadata-escaped")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("loadLocalAction() metadata symlink error = %v, want repository escape error", err)
	}
}

func TestWorkflowRepositoryNormalizesInMemoryWorkflowPath(t *testing.T) {
	repository := t.TempDir()
	path := filepath.Join(repository, ".github", "workflows", "memory.yml")
	root, canonical, err := workflowRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := filepath.Join(root, ".github", "workflows", "memory.yml")
	if canonical != wantCanonical {
		t.Fatalf("workflow repository = %q / %q, want canonical path %q", root, canonical, wantCanonical)
	}
}

func TestCompiledPlansValidateAgainstSchema(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	source := readFile(t, path)
	plans, err := compileUntrustedPlans(path, source, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("2", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	validateCompiledPlansAgainstSchema(t, plans)
}

func validateCompiledPlansAgainstSchema(t *testing.T, plans []plan.Job) {
	t.Helper()
	schemaSource := readFile(t, filepath.Join("..", "..", "schemas", "job-plan.schema.json"))
	var schemaDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(plan.Schema, schemaDocument); err != nil {
		t.Fatal(err)
	}
	jobSchema, err := compiler.Compile(plan.Schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range plans {
		encoded, err := plan.Encode(job)
		if err != nil {
			t.Fatal(err)
		}
		var document any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		if err := jobSchema.Validate(document); err != nil {
			t.Fatalf("compiled plan does not validate against %s: %v\n%s", plan.Schema, err, encoded)
		}
	}
}

func TestCompileRejectsDuplicateSanitizedInstanceKeys(t *testing.T) {
	source := []byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n        variant: [same, same]\n    steps:\n      - run: true\n")
	_, err := Compile("collision.yml", source, readFile(t, smokePath("events", "push.json")))
	if err == nil || !strings.Contains(err.Error(), `deterministic instance key "gha-build-`) || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("Compile() error = %v, want deterministic key collision", err)
	}
}

func TestInstanceKeyReportsMatrixCanonicalizationErrors(t *testing.T) {
	_, err := instanceKey("matrix", map[string]any{"unsupported": make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "canonicalize matrix") {
		t.Fatalf("instanceKey() error = %v, want canonicalization error", err)
	}
}

func TestCompileRejectsInvalidEventSnapshots(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	valid := string(readFile(t, smokePath("events", "push.json")))
	tests := []struct {
		name   string
		event  string
		wanted string
	}{
		{name: "unknown field", event: strings.Replace(valid, `"actor":`, `"unexpected":true,"actor":`, 1), wanted: "unknown field"},
		{name: "self-asserted trust", event: strings.Replace(valid, `"actor":`, `"trust":"trusted","actor":`, 1), wanted: `unknown field "trust"`},
		{name: "trailing value", event: valid + `{}`, wanted: "multiple JSON values"},
		{name: "missing ref", event: strings.Replace(valid, `"ref": "refs/heads/main"`, `"ref": ""`, 1), wanted: "ref, sha, and actor"},
		{name: "missing actor", event: strings.Replace(valid, `"actor": "buildkite-gha-smoke"`, `"actor": ""`, 1), wanted: "ref, sha, and actor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Compile("event.yml", workflow, []byte(test.event))
			if err == nil || !strings.Contains(err.Error(), test.wanted) {
				t.Fatalf("Compile() error = %v, want %q", err, test.wanted)
			}
		})
	}
}

func TestParseEventValidatesMergeGroupIdentity(t *testing.T) {
	headSHA, baseSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	headRef := "refs/heads/gh-readonly-queue/main/pr-1-deadbeef"
	event := fmt.Sprintf(`{"provider":"github","event":"merge_group","repository":{"owner":"acme","name":"widgets"},"ref":%q,"sha":%q,"actor":"octocat","payload":{"action":"checks_requested","merge_group":{"head_ref":%q,"head_sha":%q,"base_ref":"refs/heads/main","base_sha":%q}}}`, headRef, headSHA, headRef, headSHA, baseSHA)
	parsed, err := ParseEvent([]byte(event))
	if err != nil || parsed.Event != "merge_group" || parsed.Ref != headRef || parsed.SHA != headSHA {
		t.Fatalf("ParseEvent() = %#v, %v", parsed, err)
	}
	for _, test := range []struct {
		name, old, replacement, want string
	}{
		{name: "activity", old: `"checks_requested"`, replacement: `"destroyed"`, want: "checks_requested"},
		{name: "head ref", old: headRef, replacement: "refs/heads/other", want: "must match"},
		{name: "head sha", old: headSHA, replacement: strings.Repeat("c", 40), want: "must match"},
		{name: "base ref", old: "refs/heads/main", replacement: "main", want: "base_ref"},
		{name: "base sha", old: baseSHA, replacement: "main", want: "head and base SHAs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := strings.Replace(event, test.old, test.replacement, 1)
			if _, err := ParseEvent([]byte(changed)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseEvent() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseEventValidatesReleaseIdentity(t *testing.T) {
	sha := strings.Repeat("a", 40)
	event := fmt.Sprintf(`{"provider":"github","event":"release","repository":{"owner":"acme","name":"widgets"},"ref":"refs/tags/v1.2.3","sha":%q,"actor":"octocat","payload":{"action":"published","release":{"tag_name":"v1.2.3","draft":false,"prerelease":false}}}`, sha)
	parsed, err := ParseEvent([]byte(event))
	if err != nil || parsed.Event != "release" || parsed.Ref != "refs/tags/v1.2.3" || parsed.SHA != sha {
		t.Fatalf("ParseEvent() = %#v, %v", parsed, err)
	}
	for _, test := range []struct {
		name, old, replacement, want string
	}{
		{name: "unsupported activity", old: `"published"`, replacement: `"edited"`, want: "published, created, or released"},
		{name: "tag mismatch", old: `"tag_name":"v1.2.3"`, replacement: `"tag_name":"v2.0.0"`, want: "ref must match"},
		{name: "missing tag", old: `"tag_name":"v1.2.3",`, replacement: "", want: "tag_name"},
		{name: "malformed draft", old: `"draft":false`, replacement: `"draft":"false"`, want: "draft"},
		{name: "malformed prerelease", old: `"prerelease":false`, replacement: `"prerelease":"false"`, want: "prerelease"},
		{name: "draft created", old: `"action":"published"`, replacement: `"action":"created"`, want: "draft created"},
		{name: "invalid sha", old: sha, replacement: "HEAD", want: "full lowercase sha"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := strings.Replace(event, test.old, test.replacement, 1)
			if test.name == "draft created" {
				changed = strings.Replace(changed, `"draft":false`, `"draft":true`, 1)
			}
			if _, err := ParseEvent([]byte(changed)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseEvent() error = %v, want %q", err, test.want)
			}
		})
	}
}

func smokePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "testdata", "smoke"}, parts...)...)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestCompilePlansEmitV8ForContainers(t *testing.T) {
	workflowSource := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container: node:24\n    services:\n      redis: {image: redis:7}\n    steps:\n      - run: true\n")
	plans, err := compileUntrustedPlans("containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.Schema || plans[0].Container == nil || len(plans[0].Services) != 1 || !slices.Equal(plans[0].RequiredCapabilities, []string{"docker", "network"}) {
		t.Fatalf("container plan = %#v", plans)
	}
}

func TestCompilePlansResolveJobContainerImageExpressions(t *testing.T) {
	workflowSource := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        version: ['24', '25']
    container:
      image: ghcr.io/${{ github.repository_owner }}/${{ vars.IMAGE }}:${{ matrix.version }}
    steps: [{run: true}]
`)
	options := defaultOptions()
	options.Vars.Buildkite = map[string]string{"IMAGE": "tool"}
	plans, err := compilePlansForTest(t.Context(), "containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Container == nil || plans[1].Container == nil || plans[0].Container.Image != "ghcr.io/buildkite/tool:24" || plans[1].Container.Image != "ghcr.io/buildkite/tool:25" {
		t.Fatalf("compiled container images = %#v, %#v", plans[0].Container, plans[1].Container)
	}
	if encoded, err := plan.Encode(plans[0]); err != nil || bytes.Contains(encoded, []byte("${{")) {
		t.Fatalf("encoded plan retained an expression: %s, %v", encoded, err)
	}
}

func TestCompileTemplateScalarCallersRenderCompleteExpressions(t *testing.T) {
	group, err := resolveConcurrency("workflow.yml", "test", &workflow.Concurrency{Group: "${{ true }}"}, expression.CompileContext{}, nil)
	if err != nil || group != "true" {
		t.Fatalf("scalar concurrency group = %q, %v", group, err)
	}
	labels, err := resolveRunsOn(workflow.Job{RunsOn: []string{"${{ 24 }}"}}, expression.CompileContext{}, nil)
	if err != nil || !slices.Equal(labels, []string{"24"}) {
		t.Fatalf("scalar runs-on labels = %#v, %v", labels, err)
	}
	if label := instanceLabel(workflow.Job{ID: "test", Name: "${{ 2.5 }}"}, nil, expression.CompileContext{}); label != "2.5" {
		t.Fatalf("scalar job name = %q", label)
	}
}

func TestCompilePlansResolveReusableWorkflowInputJobContainerImage(t *testing.T) {
	repository := t.TempDir()
	caller := writeWorkflow(t, repository, "caller.yml", `on: push
jobs:
  delegated:
    uses: ./.github/workflows/container.yml
    with:
      image: node:24
`)
	writeWorkflow(t, repository, "container.yml", `on:
  workflow_call:
    inputs:
      image:
        required: true
        type: string
jobs:
  test:
    runs-on: ubuntu-latest
    container:
      image: ${{ inputs.image }}
    steps: [{run: true}]
`)
	plans, err := compileUntrustedPlans(caller, readFile(t, caller), readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Container == nil || plans[0].Container.Image != "node:24" {
		t.Fatalf("reusable job container = %#v", plans)
	}
}

func TestCompilePlansRejectInvalidJobContainerImageExpressions(t *testing.T) {
	for name, image := range map[string]string{
		"secret":        "${{ secrets.IMAGE }}",
		"needs output":  "${{ needs.build.outputs.image }}",
		"step output":   "${{ steps.build.outputs.image }}",
		"whole context": "${{ github }}",
		"boolean":       "${{ true }}",
		"empty":         "${{ '' }}",
		"invalid":       "${{ 'bad image' }}",
	} {
		t.Run(name, func(t *testing.T) {
			workflowSource := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container:\n      image: \"" + image + "\"\n    steps: [{run: true}]\n")
			_, err := compileUntrustedPlans("containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), "gha-untrusted")
			if err == nil || !strings.Contains(err.Error(), "resolve job container") {
				t.Fatalf("compile error = %v", err)
			}
			if strings.Contains(err.Error(), "[REDACTED:") {
				t.Fatalf("compile error leaked a secret: %v", err)
			}
		})
	}
}

func TestCompilePlansResolveStaticServiceContainerFields(t *testing.T) {
	workflowSource := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        postgres: ['16', '17']
    services:
      database:
        image: postgres:${{ matrix.postgres }}
        credentials:
          username: ${{ vars.REGISTRY_USER }}
          password: ${{ secrets.REGISTRY_PASSWORD }}
        env: {INSTANCE: '${{ strategy.job-index }}'}
        ports: ['${{ vars.SERVICE_PORT }}']
        volumes: ['database:/var/lib/postgresql/data']
        options: --health-retries 5
        command: postgres -c fsync=off
        entrypoint: docker-entrypoint.sh
    steps: [{run: true}]
`)
	options := defaultOptions()
	options.Vars.Buildkite = map[string]string{"SERVICE_PORT": "5432", "REGISTRY_USER": "registry-user"}
	plans, err := compilePlansForTest(t.Context(), "containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	for i, image := range []string{"postgres:16", "postgres:17"} {
		service := plans[i].Services["database"]
		if service.Image != image || service.Credentials == nil || service.Credentials.Username != "registry-user" || service.Credentials.Password != "${{ secrets.REGISTRY_PASSWORD }}" || service.Env["INSTANCE"] != strconv.Itoa(i) || !slices.Equal(service.Ports, []string{"5432"}) || len(service.Volumes) != 1 || service.Options == "" || service.Command == "" || service.Entrypoint == "" {
			t.Fatalf("compiled service %d = %#v", i, service)
		}
		if !slices.Equal(plans[i].ServiceOrder, []string{"database"}) {
			t.Fatalf("service order = %#v", plans[i].ServiceOrder)
		}
		if !slices.Equal(plans[i].RequiredSecrets, []string{"REGISTRY_PASSWORD"}) || !plans[i].HasCapability("secrets") {
			t.Fatalf("service credential provenance = %#v, %#v", plans[i].RequiredSecrets, plans[i].RequiredCapabilities)
		}
	}
	plans[0].Services["database"].Env["INSTANCE"] = "changed"
	if plans[1].Services["database"].Env["INSTANCE"] != "1" {
		t.Fatal("matrix service environments share mutable state")
	}
}

func TestResolveCompileServicesRejectsVariableIntroducedExpressionSyntax(t *testing.T) {
	services := []workflow.Service{{
		Name: "database",
		Container: workflow.ServiceContainer{
			Image: "postgres:16",
			Credentials: &workflow.ContainerCredentials{
				Username: "registry-user",
				Password: "${{ vars.password }}",
			},
		},
	}}
	_, err := resolveCompileServices(services, expression.CompileContext{Vars: map[string]string{"password": "${{ secrets.ADMIN }}"}})
	if err == nil || !strings.Contains(err.Error(), "compile-time expression result contains expression syntax") {
		t.Fatalf("resolveCompileServices() error = %v", err)
	}
}

func TestResolveCompileServicesRejectsUnsupportedCredentialContexts(t *testing.T) {
	for _, value := range []string{"${{ inputs.user }}", "${{ matrix.user }}", "${{ strategy.job-index }}", "${{ needs.build.outputs.user }}"} {
		services := []workflow.Service{{Name: "database", Container: workflow.ServiceContainer{
			Image: "postgres:16", Credentials: &workflow.ContainerCredentials{Username: value, Password: "literal"},
		}}}
		if _, err := resolveCompileServices(services, expression.CompileContext{}); err == nil || !strings.Contains(err.Error(), "credential expression context") {
			t.Errorf("resolveCompileServices() credential %q error = %v", value, err)
		}
	}
}

func TestCompilePlansSkipEmptyStaticServiceImage(t *testing.T) {
	workflowSource := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        image: ['redis:7', '']
    services:
      cache:
        image: ${{ matrix.image }}
    steps: [{run: true}]
`)
	plans, err := compilePlansForTest(t.Context(), "containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Services["cache"].Image != "redis:7" || len(plans[1].Services) != 0 {
		t.Fatalf("compiled services = %#v, %#v", plans[0].Services, plans[1].Services)
	}
}

func TestCompilePlansResolveWorkflowDispatchInputsAndStrategyDefaultsInServices(t *testing.T) {
	workflowSource := []byte(`on:
  workflow_dispatch:
    inputs:
      image:
        type: string
        default: redis:7
      enabled:
        type: boolean
      replicas:
        type: number
jobs:
  test:
    runs-on: ubuntu-latest
    services:
      cache:
        image: ${{ inputs.image }}
        env:
          FAIL_FAST: ${{ strategy.fail-fast }}
          MAX_PARALLEL: ${{ strategy.max-parallel }}
          ENABLED: ${{ inputs.enabled }}
          REPLICAS: ${{ inputs.replicas }}
    steps: [{run: true}]
`)
	var event map[string]any
	if err := json.Unmarshal(readFile(t, smokePath("events", "push.json")), &event); err != nil {
		t.Fatal(err)
	}
	event["event"] = "workflow_dispatch"
	event["payload"] = map[string]any{"inputs": map[string]any{"image": "valkey:8"}}
	eventSource, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(t.Context(), "containers.yml", workflowSource, eventSource, "0.0.0-test", "sha256:"+strings.Repeat("1", 64), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	service := plans[0].Services["cache"]
	if service.Image != "valkey:8" || service.Env["FAIL_FAST"] != "true" || service.Env["MAX_PARALLEL"] != "1" || service.Env["ENABLED"] != "false" || service.Env["REPLICAS"] != "0" {
		t.Fatalf("compiled service = %#v", service)
	}
}

func TestCompilePlansSubstituteWorkflowDispatchInputsWithoutReusableCalls(t *testing.T) {
	workflowSource := []byte(`on:
  workflow_dispatch:
    inputs:
      timeout:
        type: number
        default: 5
      label:
        type: string
        default: dispatched
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      LABEL: ${{ inputs.label }}
      INDEXED_LABEL: ${{ inputs['label'] }}
    defaults:
      run:
        shell: ${{ inputs.label }}
    outputs:
      label: ${{ inputs.label }}
    steps:
      - run: true
        timeout-minutes: ${{ inputs.timeout }}
`)
	var event map[string]any
	if err := json.Unmarshal(readFile(t, smokePath("events", "push.json")), &event); err != nil {
		t.Fatal(err)
	}
	event["event"] = "workflow_dispatch"
	event["payload"] = map[string]any{"inputs": map[string]any{"timeout": 7, "label": "bash"}}
	eventSource, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(t.Context(), "dispatch.yml", workflowSource, eventSource, "0.0.0-test", "sha256:"+strings.Repeat("1", 64), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("compiled plans = %d, want 1", len(plans))
	}
	job := plans[0]
	if job.Env["LABEL"] != "bash" || job.Env["INDEXED_LABEL"] != "bash" || job.Inputs["label"] != "bash" || job.Inputs["timeout"] != json.Number("7") || job.DefaultShell != "bash" || job.Outputs["label"] != "bash" || job.Steps[0].TimeoutMinutes != 7 || job.Steps[0].TimeoutMinutesExpression != "" {
		t.Fatalf("compiled dispatch input surfaces = %#v", job)
	}
}

func TestCompilePlansRetainNeedsServiceExpressionForRuntime(t *testing.T) {
	workflowSource := []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs: {image: '${{ steps.image.outputs.value }}'}
    steps: [{id: image, run: true}]
  consumer:
    needs: producer
    runs-on: ubuntu-latest
    services:
      cache:
        image: ${{ needs.producer.outputs.image }}
    steps: [{run: true}]
`)
	plans, err := compilePlansForTest(t.Context(), "containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := plans[1].Services["cache"].Image; got != "${{ needs.producer.outputs.image }}" {
		t.Fatalf("runtime service image = %q", got)
	}
}

func TestCompilePlansRetainWholeServiceMapExpression(t *testing.T) {
	workflowSource := []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    outputs:
      services: ${{ steps.out.outputs.services }}
    steps:
      - id: out
        run: echo services={} >> "$GITHUB_OUTPUT"
  consumer:
    needs: producer
    runs-on: ubuntu-latest
    services: ${{ fromJSON(needs.producer.outputs.services) }}
    steps: [{run: true}]
`)
	plans, err := compilePlansForTest(t.Context(), "containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	got := plans[1]
	if got.ServicesExpression != "${{ fromJSON(needs.producer.outputs.services) }}" || !slices.Contains(got.RequiredCapabilities, "docker") {
		t.Fatalf("dynamic services plan = expression %q, capabilities %#v", got.ServicesExpression, got.RequiredCapabilities)
	}
}

func TestCompilePlansValidateFieldsBeforeSkippingEmptyService(t *testing.T) {
	workflowSource := []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    steps: [{run: true}]
  consumer:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        image: [""]
    services:
      optional:
        image: ${{ matrix.image }}
        env:
          INVALID: ${{ needs.producer.result }}
    steps: [{run: true}]
`)
	_, err := compilePlansForTest(t.Context(), "containers.yml", workflowSource, readFile(t, smokePath("events", "push.json")), "0.0.0-test", "sha256:"+strings.Repeat("1", 64), defaultOptions())
	if err == nil || !strings.Contains(err.Error(), "service runtime expression must directly reference needs") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompilerWarningsNameOnlyDeclaredSupportedTriggers(t *testing.T) {
	parsed := &workflow.Workflow{Triggers: []workflow.Trigger{
		{Event: "workflow_dispatch"},
		{Event: "discussion", Position: workflow.Position{Line: 4, Column: 3}},
		{Event: "issue_comment"},
		{Event: "issues"},
		{Event: "release"},
		{Event: "push"},
		{Event: "merge_group"},
		{Event: "schedule"},
		{Event: "pull_request"},
		{Event: "workflow_call"},
		{Event: "push"},
	}}
	warnings := compilerWarnings(parsed, false)
	want := Warning{
		Code: "W_TRIGGER_EVENT_UNSUPPORTED", Blocker: "trigger", BlockerDetail: "discussion", Line: 4, Column: 3,
		Message: "on.discussion is ignored, so nothing in this workflow runs from it. The supported triggers declared in this workflow still run: issue_comment, issues, merge_group, pull_request, push, release, schedule, workflow_call, workflow_dispatch. Move the jobs this trigger guards to one of those triggers if you need them. If you need discussion, log an issue on https://github.com/buildkite/buildkite-gha so we can prioritise it.",
	}
	if !reflect.DeepEqual(warnings, []Warning{want}) {
		t.Fatalf("warnings = %#v, want %#v", warnings, []Warning{want})
	}

	parsed.Triggers = []workflow.Trigger{{Event: "discussion", Position: workflow.Position{Line: 1, Column: 5}}}
	warnings = compilerWarnings(parsed, false)
	want = Warning{
		Code: "W_TRIGGER_EVENT_UNSUPPORTED", Blocker: "trigger", BlockerDetail: "discussion", Line: 1, Column: 5,
		Message: "on.discussion is ignored, so nothing in this workflow runs from it. This workflow declares no supported triggers that still run. If you need discussion, log an issue on https://github.com/buildkite/buildkite-gha so we can prioritise it.",
	}
	if !reflect.DeepEqual(warnings, []Warning{want}) {
		t.Fatalf("warnings without supported triggers = %#v, want %#v", warnings, []Warning{want})
	}
}

func TestCompilerWarningsFlagIgnoredMergeGroupPathFilters(t *testing.T) {
	parsed := &workflow.Workflow{Triggers: []workflow.Trigger{
		{Event: "merge_group", Paths: []string{"src/**"}, Position: workflow.Position{Line: 3, Column: 3}},
	}}
	want := Warning{
		Code: "W_MERGE_GROUP_PATH_FILTERS_IGNORED", Line: 3, Column: 3,
		Message: "on.merge_group paths and paths-ignore are ignored, matching GitHub, which does not evaluate path filters for merge_group events. Every merge_group delivery runs this workflow. Move the filtering into a job or step condition if you need it.",
	}
	if warnings := compilerWarnings(parsed, false); !reflect.DeepEqual(warnings, []Warning{want}) {
		t.Fatalf("warnings = %#v, want %#v", warnings, []Warning{want})
	}

	parsed.Triggers = []workflow.Trigger{{Event: "merge_group", Types: []string{"checks_requested"}}}
	if warnings := compilerWarnings(parsed, false); warnings != nil {
		t.Fatalf("warnings without path filters = %#v, want none", warnings)
	}
}
