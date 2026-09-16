package compiler

import (
	"reflect"
	"strings"
	"testing"
)

const dynamicEnvironmentWorkflow = `on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      environment: ${{ steps.choose.outputs.environment }}
    steps:
      - id: choose
        run: echo 'environment=staging-eu' >> "$GITHUB_OUTPUT"
  deploy:
    needs: plan
    runs-on: ubuntu-latest
    environment:
      name: ${{ needs.plan.outputs.environment }}
      url: ${{ steps.deploy.outputs.url }}
    steps:
      - id: deploy
        run: echo "$KEY" "$REGION"
        env:
          KEY: ${{ secrets.DEPLOY_KEY }}
          REGION: ${{ vars.REGION }}
  finish:
    needs: deploy
    runs-on: ubuntu-latest
    steps: [{run: true}]
`

func TestDynamicEnvironmentDefersPlansAndResolvesScope(t *testing.T) {
	options := defaultOptions()
	source := &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
		"staging-eu": {SecretNames: []string{"DEPLOY_KEY"}, Variables: map[string]string{"REGION": "eu-west-1"}},
	}}
	options.EnvironmentSource = source
	initial, err := CompileBundlePlansContext(t.Context(), "deploy.yml", []byte(dynamicEnvironmentWorkflow), pushEvent(t), "dev", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Plans) != 1 || initial.Plans[0].Job.Workflow.LogicalJobID != "plan" || len(source.calls) != 0 {
		t.Fatalf("initial plans = %#v; premature environment reads = %v", initial.Plans, source.calls)
	}
	if len(initial.IR.Continuations) != 1 {
		t.Fatalf("continuations = %#v", initial.IR.Continuations)
	}
	continuation := initial.IR.Continuations[0]
	if continuation.StepKey != "gha-deploy-environment" || continuation.Descriptor.Shape != RuntimeEnvironmentShape || strings.Join(continuation.Jobs, ",") != "deploy,finish" {
		t.Fatalf("continuation = %#v", continuation)
	}
	if err := continuation.Descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	options.RuntimeEnvironmentNames = map[string]string{"deploy": "staging-eu"}
	continued, err := compileEnvironmentBundle(t, dynamicEnvironmentWorkflow, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(continued.Plans) != 3 || len(continued.GeneratedWorkflow.ApprovalGates) != 0 {
		t.Fatalf("plans = %d; gates = %#v", len(continued.Plans), continued.GeneratedWorkflow.ApprovalGates)
	}
	deploy := continued.Plans[1].Job
	if !reflect.DeepEqual(deploy.RequiredSecrets, []string{"STAGING_EU_DEPLOY_KEY"}) || !reflect.DeepEqual(deploy.SecretMappings, map[string]string{"DEPLOY_KEY": "STAGING_EU_DEPLOY_KEY"}) || !reflect.DeepEqual(deploy.EnvironmentVars, map[string]string{"REGION": "eu-west-1"}) {
		t.Fatalf("wrong environment scope: secrets=%v mappings=%v vars=%v", deploy.RequiredSecrets, deploy.SecretMappings, deploy.EnvironmentVars)
	}
	if len(continued.Plans[0].Job.EnvironmentVars) != 0 || len(continued.Plans[2].Job.EnvironmentVars) != 0 {
		t.Fatal("environment variables escaped the declaring job")
	}
}

func TestDynamicEnvironmentRejectsEveryProtection(t *testing.T) {
	for name, protection := range map[string]EnvironmentProtection{
		"reviewers":   {RequiredReviewers: true},
		"self review": {PreventSelfReview: true},
		"timer":       {WaitTimerMinutes: 1},
		"branch":      {BranchPolicy: true},
		"custom":      {UnsupportedRules: []string{"custom"}},
	} {
		t.Run(name, func(t *testing.T) {
			options := defaultOptions()
			options.RuntimeEnvironmentNames = map[string]string{"deploy": "production"}
			options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{"production": protection}}
			bundle, err := compileEnvironmentBundle(t, dynamicEnvironmentWorkflow, options)
			if err == nil || !strings.Contains(err.Error(), "protected dynamic environment") || len(bundle.Pipeline) != 0 {
				t.Fatalf("error = %v; pipeline = %s", err, bundle.Pipeline)
			}
			for _, artifact := range bundle.Plans {
				if artifact.Job.Workflow.LogicalJobID != "plan" {
					t.Fatal("protected job or dependent received a plan")
				}
			}
		})
	}
}

func TestDynamicEnvironmentRejectsInvalidNamesAndKnownPrefixCollision(t *testing.T) {
	for _, name := range []string{"", " ", "production\n", "a\x00b", "a\x7fb", strings.Repeat("a", 256), "\xff"} {
		if err := ValidateRuntimeEnvironmentName(name); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
	if err := ValidateRuntimeEnvironmentName(strings.Repeat("é", 255)); err != nil {
		t.Fatalf("rejected 255-character UTF-8 name: %v", err)
	}
	options := defaultOptions()
	options.RuntimeEnvironmentNames = map[string]string{"deploy": "staging/eu"}
	options.KnownEnvironmentNames = []string{"staging-eu"}
	options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{"staging/eu": {}}}
	_, err := compileEnvironmentBundle(t, dynamicEnvironmentWorkflow, options)
	if err == nil || !strings.Contains(err.Error(), "secret prefix") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestDynamicEnvironmentRejectsUnsupportedScheduling(t *testing.T) {
	matrix := `  matrix_job:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.environment) }}
    steps: [{run: true}]
`
	for name, source := range map[string]string{
		"joined with matrix":       dynamicEnvironmentWorkflow + matrix + "  join:\n    needs: [finish, matrix_job]\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n",
		"after matrix":             strings.Replace(dynamicEnvironmentWorkflow, "    needs: plan\n", "    needs: [plan, matrix_job]\n", 1) + matrix,
		"matrix after environment": dynamicEnvironmentWorkflow + strings.Replace(matrix, "    needs: plan\n", "    needs: [plan, deploy]\n", 1),
		"matrix consumer":          strings.Replace(dynamicEnvironmentWorkflow, "    environment:\n", "    strategy:\n      matrix: {target: [x]}\n    environment:\n", 1),
		"missing output":           strings.Replace(dynamicEnvironmentWorkflow, "needs.plan.outputs.environment", "needs.plan.outputs.missing", 1),
		"not direct need":          strings.Replace(dynamicEnvironmentWorkflow, "    needs: plan\n", "", 1),
		"multiple names":           strings.Replace(dynamicEnvironmentWorkflow, "    needs: deploy\n", "    needs: [deploy, plan]\n    environment: ${{ needs.plan.outputs.environment }}\n", 1),
		"workflow concurrency":     "concurrency: deployments\n" + dynamicEnvironmentWorkflow,
		"compound expression":      strings.Replace(dynamicEnvironmentWorkflow, "needs.plan.outputs.environment", "needs.plan.outputs.environment || 'production'", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CompileIRWithOptionsContext(t.Context(), "deploy.yml", []byte(source), pushEvent(t), defaultOptions()); err == nil {
				t.Fatal("unsupported dynamic environment was accepted")
			}
		})
	}
}

func TestDynamicEnvironmentDoesNotNarrowTokenAuthority(t *testing.T) {
	for _, guard := range []string{"needs.plan.outputs.environment == 'production'", "vars.REGION == 'production'"} {
		t.Run(guard, func(t *testing.T) {
			workflow := strings.Replace(dynamicEnvironmentWorkflow, `echo "$KEY" "$REGION"`, "echo ${{ "+guard+" && github.token || 'none' }}", 1)
			workflow = "permissions: {}\n" + workflow
			options := defaultOptions()
			options.RuntimeEnvironmentNames = map[string]string{"deploy": "staging-eu"}
			options.EnvironmentSource = &fakeEnvironmentSource{protections: map[string]EnvironmentProtection{
				"staging-eu": {Variables: map[string]string{"REGION": "eu-west-1"}},
			}}
			bundle, err := compileEnvironmentBundle(t, workflow, options)
			if err == nil || !strings.Contains(err.Error(), "github.token") || len(bundle.Pipeline) != 0 {
				t.Fatalf("late scheduling data narrowed token authority: error=%v", err)
			}
		})
	}
}
