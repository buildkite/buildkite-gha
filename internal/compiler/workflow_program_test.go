package compiler

import (
	"reflect"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/program"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestWorkflowProgramInventoriesEveryExecutionField(t *testing.T) {
	span := workflow.Span{Start: workflow.Position{Line: 10, Column: 3}, End: workflow.Position{Line: 30, Column: 1}}
	instance := JobInstance{
		SourcePath:              "workflow.yml",
		Source:                  span,
		CallGuards:              []CallGuard{{Condition: "guard"}},
		If:                      "job-if",
		Env:                     map[string]string{"Z": "job-env-z", "A": "job-env-a"},
		DefaultShell:            "job-shell",
		DefaultWorkingDirectory: "job-directory",
		Container: &workflow.Container{
			Image: "container-image", Env: map[string]string{"C": "container-env"}, Ports: []string{"container-port"}, Span: span,
		},
		Services: []workflow.Service{{Name: "db", Container: workflow.ServiceContainer{
			Image: "service-image", Env: map[string]string{"S": "service-env"}, Ports: []string{"service-port"}, Volumes: []string{"service-volume"},
			Options: "service-options", Command: "service-command", Entrypoint: "service-entrypoint", Span: span,
			Credentials: &workflow.ContainerCredentials{Username: "service-user", Password: "service-password"},
		}}},
		ServicesExpression: "service-map",
		Steps: []workflow.Step{
			{Kind: "run", Name: "run-name", Env: map[string]string{"R": "run-env"}, If: "run-if", ContinueOnErrorExpression: "run-continue", TimeoutMinutesExpression: "run-timeout", Run: "run-command", Shell: "run-shell", WorkingDirectory: "run-directory", Span: span},
			{Kind: "uses", ID: "action", Name: "action-name", Env: map[string]string{"E": "action-env"}, If: "action-if", Uses: "owner/action@v1", With: map[string]string{"input": "action-input"}, Span: span},
		},
		Outputs: map[string]string{"result": "job-output"},
	}

	workflowProgram := lowerWorkflowProgram(instance)
	var got []string
	err := workflowProgram.VisitSites(func(site program.Site) error {
		got = append(got, site.Location.Field+"|"+string(site.Surface)+"|"+string(site.Result)+"|"+string(site.Purpose))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"job.call-guards[0].if|call-condition|boolean|expression",
		"job.if|job-condition|boolean|expression",
		"job.env.A|job-environment|string|expression",
		"job.env.Z|job-environment|string|expression",
		"job.defaults.run.shell|job-default|string|expression",
		"job.defaults.run.working-directory|job-default|string|expression",
		"job.container.image|runtime-template|string|expression",
		"job.container.env.C|runtime-template|string|expression",
		"job.container.ports[0]|runtime-template|string|expression",
		"job.services.db.image|service-template|string|expression",
		"job.services.db.credentials.username|service-credential|string|expression",
		"job.services.db.credentials.password|service-credential|string|expression",
		"job.services.db.env.S|service-template|string|expression",
		"job.services.db.ports[0]|service-template|string|expression",
		"job.services.db.volumes[0]|service-template|string|expression",
		"job.services.db.options|service-template|string|expression",
		"job.services.db.command|service-template|string|expression",
		"job.services.db.entrypoint|service-template|string|expression",
		"job.services|service-map|object|expression",
		"job.steps[0].env.R|step-template|string|expression",
		"job.steps[0].if|step-condition|boolean|expression",
		"job.steps[0].continue-on-error|step-control|boolean|expression",
		"job.steps[0].timeout-minutes|step-control|number|expression",
		"job.steps[0].name|step-template|string|expression",
		"job.steps[0].run|step-template|string|expression",
		"job.steps[0].shell|step-template|string|expression",
		"job.steps[0].working-directory|step-template|string|expression",
		"job.steps[1].env.E|step-template|string|expression",
		"job.steps[1].if|step-condition|boolean|expression",
		"job.steps[1].name|step-template|string|expression",
		"job.steps[1].uses|runtime-template|string|expression",
		"job.steps[1].with.input|step-template|string|action-input",
		"job.outputs.result|job-output|string|expression",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("program sites:\n got: %#v\nwant: %#v", got, want)
	}
	if workflowProgram.Job.Steps[0].ID != "step-1" || workflowProgram.Job.Steps[1].ID != "action" {
		t.Fatalf("normalized step IDs = %q, %q", workflowProgram.Job.Steps[0].ID, workflowProgram.Job.Steps[1].ID)
	}

	instance.Env["A"] = "mutated"
	instance.Steps[0].Env["R"] = "mutated"
	instance.Services[0].Container.Ports[0] = "mutated"
	if workflowProgram.Job.Env[0].Value.Source != "job-env-a" || workflowProgram.Job.Steps[0].Env[0].Value.Source != "run-env" || workflowProgram.Job.Services.Static[0].Container.Ports[0].Source != "service-port" {
		t.Fatal("normalized program aliases mutable JobInstance data")
	}
}

func TestWorkflowProgramProjectsLegacyPlanSteps(t *testing.T) {
	span := workflow.Span{Start: workflow.Position{Line: 3, Column: 5}, End: workflow.Position{Line: 8, Column: 1}}
	instance := JobInstance{SourcePath: "workflow.yml", Steps: []workflow.Step{
		{Kind: "run", Name: "name", Run: "echo hi", Shell: "bash", WorkingDirectory: "src", Env: map[string]string{}, If: "success()", ContinueOnErrorExpression: "${{ matrix.experimental }}", TimeoutMinutes: 5, Span: span},
		{Kind: "uses", Uses: "owner/action@v1", With: map[string]string{"name": "value"}, Span: span},
	}}
	workflowProgram := lowerWorkflowProgram(instance)
	steps := projectPlanSteps(workflowProgram.Job.Steps)
	indexes, references, inputs := programActionInvocations(workflowProgram.Job.Steps)
	if len(steps) != 2 || steps[0].ID != "step-1" || steps[0].Command != "echo hi" || steps[0].Shell != "bash" || steps[0].WorkingDirectory != "src" || steps[0].Condition != "success()" || steps[0].ContinueOnErrorExpression != "${{ matrix.experimental }}" || steps[0].TimeoutMinutes != 5 {
		t.Fatalf("projected run step = %#v", steps[0])
	}
	if len(indexes) != 1 || indexes[0] != 1 || !reflect.DeepEqual(references, []string{"owner/action@v1"}) || !reflect.DeepEqual(inputs, []map[string]string{{"name": "value"}}) {
		t.Fatalf("projected invocation metadata = %#v, %#v, %#v", indexes, references, inputs)
	}
	if steps[0].Env == nil || steps[1].With["name"] != "value" || steps[0].Source.Start.Line != 3 {
		t.Fatalf("projected step details = %#v", steps)
	}
}

func TestProgramOwnedSecretInventory(t *testing.T) {
	instance := JobInstance{
		secretAuthority: secretAuthority{unrestricted: true},
		If:              "github.ref != ''",
		Env:             map[string]string{"VALUE": "${{ secrets.JOB_ENV }}"},
		Outputs:         map[string]string{"value": "${{ secrets.JOB_OUTPUT }}"},
		Services: []workflow.Service{{Name: "db", Container: workflow.ServiceContainer{
			Image: "postgres", Command: "start",
		}}},
		Steps: []workflow.Step{
			{Kind: "run", Run: "echo ${{ secrets.RUN }}", Name: "${{ toJSON(github) }}", Env: map[string]string{"CONDITION": "${{ secrets.CONDITION }}"}},
			{Kind: "uses", Uses: "owner/action@v1", With: map[string]string{"ignored": "${{ secrets.INSPECTED_INPUT }}"}},
		},
	}
	secrets, mappings, aliases, token, err := requiredSecrets(lowerWorkflowProgram(instance), instance.secretAuthority, []string{"ACTION_DEFAULT"}, true, "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ACTION_DEFAULT", "CONDITION", "JOB_ENV", "JOB_OUTPUT", "RUN"}
	if !reflect.DeepEqual(secrets, want) || len(mappings) != 0 || len(aliases) != 0 || !token {
		t.Fatalf("requiredSecrets() = %#v, %#v, %#v, %v; want %#v, nil, nil, true", secrets, mappings, aliases, token, want)
	}
}
