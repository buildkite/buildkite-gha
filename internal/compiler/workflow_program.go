package compiler

import (
	"fmt"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func lowerWorkflowProgram(instance JobInstance) program.Program {
	jobLocation := programLocation(instance.SourcePath, "job", instance.Source)
	result := program.Program{Job: program.Job{
		Condition:       workflowSite(instance.If, program.SurfaceJobCondition, program.ResultBoolean, jobLocation, "job.if"),
		ContinueOnError: instance.ContinueOnError,
		TimeoutMinutes:  instance.TimeoutMinutes,
		Env:             workflowBindings(instance.Env, program.SurfaceJobEnvironment, jobLocation, "job.env", program.PurposeExpression),
		Defaults: program.Defaults{
			Shell:            workflowSite(instance.DefaultShell, program.SurfaceJobDefault, program.ResultString, jobLocation, "job.defaults.run.shell"),
			WorkingDirectory: workflowSite(instance.DefaultWorkingDirectory, program.SurfaceJobDefault, program.ResultString, jobLocation, "job.defaults.run.working-directory"),
		},
		Outputs: workflowBindings(instance.Outputs, program.SurfaceJobOutput, jobLocation, "job.outputs", program.PurposeExpression),
	}}
	if len(instance.CallGuards) != 0 {
		result.Job.Guards = make([]program.Guard, len(instance.CallGuards))
		for i, guard := range instance.CallGuards {
			field := fmt.Sprintf("job.call-guards[%d].if", i)
			result.Job.Guards[i] = program.Guard{Condition: workflowSite(guard.Condition, program.SurfaceJobCondition, program.ResultBoolean, jobLocation, field)}
		}
	}
	if instance.Container != nil {
		result.Job.Container = &program.Container{
			Image: workflowSite(instance.Container.Image, program.SurfaceRuntimeTemplate, program.ResultString, jobLocation, "job.container.image"),
			Env:   workflowBindings(instance.Container.Env, program.SurfaceRuntimeTemplate, jobLocation, "job.container.env", program.PurposeExpression),
			Ports: workflowSites(instance.Container.Ports, program.SurfaceRuntimeTemplate, program.ResultString, jobLocation, "job.container.ports"),
		}
	}
	if len(instance.Services) != 0 {
		result.Job.Services.Static = make([]program.Service, len(instance.Services))
		for i, service := range instance.Services {
			field := fmt.Sprintf("job.services.%s", service.Name)
			location := programLocation(instance.SourcePath, field, service.Container.Span)
			container := service.Container
			lowered := program.ServiceContainer{
				Image:      workflowSite(container.Image, program.SurfaceRuntimeTemplate, program.ResultString, location, field+".image"),
				Env:        workflowBindings(container.Env, program.SurfaceRuntimeTemplate, location, field+".env", program.PurposeExpression),
				Ports:      workflowSites(container.Ports, program.SurfaceRuntimeTemplate, program.ResultString, location, field+".ports"),
				Volumes:    workflowSites(container.Volumes, program.SurfaceRuntimeTemplate, program.ResultString, location, field+".volumes"),
				Options:    workflowSite(container.Options, program.SurfaceRuntimeTemplate, program.ResultString, location, field+".options"),
				Command:    workflowSite(container.Command, program.SurfaceRuntimeTemplate, program.ResultString, location, field+".command"),
				Entrypoint: workflowSite(container.Entrypoint, program.SurfaceRuntimeTemplate, program.ResultString, location, field+".entrypoint"),
			}
			if container.Credentials != nil {
				lowered.Credentials = &program.ContainerCredentials{
					Username: workflowSite(container.Credentials.Username, program.SurfaceServiceCredential, program.ResultString, location, field+".credentials.username"),
					Password: workflowSite(container.Credentials.Password, program.SurfaceServiceCredential, program.ResultString, location, field+".credentials.password"),
				}
			}
			result.Job.Services.Static[i] = program.Service{Name: service.Name, Container: lowered}
		}
	}
	if instance.ServicesExpression != "" {
		site := workflowSite(instance.ServicesExpression, program.SurfaceServiceMap, program.ResultObject, jobLocation, "job.services")
		result.Job.Services.Dynamic = &site
	}
	result.Job.Steps = lowerWorkflowSteps(instance.SourcePath, instance.Steps)
	return result
}

func lowerWorkflowSteps(sourcePath string, source []workflow.Step) []program.Step {
	steps := make([]program.Step, len(source))
	usedIDs := make(map[string]struct{}, len(source))
	for _, step := range source {
		if step.ID != "" {
			usedIDs[strings.ToLower(step.ID)] = struct{}{}
		}
	}
	for i, step := range source {
		id := step.ID
		if id == "" {
			id = fmt.Sprintf("step-%d", i+1)
			for suffix := 2; ; suffix++ {
				if _, exists := usedIDs[strings.ToLower(id)]; !exists {
					break
				}
				id = fmt.Sprintf("step-%d-%d", i+1, suffix)
			}
			usedIDs[strings.ToLower(id)] = struct{}{}
		}
		field := fmt.Sprintf("job.steps[%d]", i)
		location := programLocation(sourcePath, field, step.Span)
		steps[i] = program.Step{
			ID:         id,
			Kind:       step.Kind,
			Background: step.Background,
			Targets:    append([]string(nil), step.Targets...),
			Source:     location,
			Env:        workflowBindings(step.Env, program.SurfaceStepTemplate, location, field+".env", program.PurposeExpression),
			Condition:  workflowSite(step.If, program.SurfaceStepCondition, program.ResultBoolean, location, field+".if"),
			ContinueOnError: program.BoolControl{
				Literal: step.ContinueOnError,
			},
			TimeoutMinutes: program.NumberControl{
				Literal: step.TimeoutMinutes,
			},
			Name: workflowSite(step.Name, program.SurfaceStepTemplate, program.ResultString, location, field+".name"),
		}
		if step.ContinueOnErrorExpression != "" {
			site := workflowSite(step.ContinueOnErrorExpression, program.SurfaceStepControl, program.ResultBoolean, location, field+".continue-on-error")
			steps[i].ContinueOnError.Expression = &site
		}
		if step.TimeoutMinutesExpression != "" {
			site := workflowSite(step.TimeoutMinutesExpression, program.SurfaceStepControl, program.ResultNumber, location, field+".timeout-minutes")
			steps[i].TimeoutMinutes.Expression = &site
		}
		switch step.Kind {
		case "run":
			steps[i].Run = &program.Run{
				Command:          workflowSite(step.Run, program.SurfaceStepTemplate, program.ResultString, location, field+".run"),
				Shell:            workflowSite(step.Shell, program.SurfaceStepTemplate, program.ResultString, location, field+".shell"),
				WorkingDirectory: workflowSite(step.WorkingDirectory, program.SurfaceStepTemplate, program.ResultString, location, field+".working-directory"),
			}
		case "uses":
			steps[i].Invocation = &program.Invocation{
				Uses: workflowSite(step.Uses, program.SurfaceRuntimeTemplate, program.ResultString, location, field+".uses"),
				With: workflowBindings(step.With, program.SurfaceStepTemplate, location, field+".with", program.PurposeActionInput),
			}
		}
	}
	return steps
}

func projectPlanSteps(source []program.Step) []plan.Step {
	steps := make([]plan.Step, len(source))
	for i, step := range source {
		steps[i] = plan.Step{
			ID: step.ID, Name: step.Name.Source, Kind: step.Kind, Background: step.Background, Targets: append([]string(nil), step.Targets...),
			Env: programBindingMap(step.Env), Condition: step.Condition.Source,
			ContinueOnError: step.ContinueOnError.Literal, TimeoutMinutes: step.TimeoutMinutes.Literal,
			Source: &plan.Span{
				Start: plan.Position{Line: step.Source.Start.Line, Column: step.Source.Start.Column},
				End:   plan.Position{Line: step.Source.End.Line, Column: step.Source.End.Column},
			},
		}
		if step.ContinueOnError.Expression != nil {
			steps[i].ContinueOnErrorExpression = step.ContinueOnError.Expression.Source
		}
		if step.TimeoutMinutes.Expression != nil {
			steps[i].TimeoutMinutesExpression = step.TimeoutMinutes.Expression.Source
		}
		if step.Run != nil {
			steps[i].Command = step.Run.Command.Source
			steps[i].Shell = step.Run.Shell.Source
			steps[i].WorkingDirectory = step.Run.WorkingDirectory.Source
		}
		if step.Invocation != nil {
			steps[i].Uses = step.Invocation.Uses.Source
			steps[i].With = programBindingMap(step.Invocation.With)
			if step.Invocation.Lock != "" {
				steps[i].Action = &plan.ActionSelector{Lock: step.Invocation.Lock}
			}
		}
	}
	return steps
}

func programActionInvocations(steps []program.Step) ([]int, []string, []map[string]string) {
	var indexes []int
	var references []string
	var inputs []map[string]string
	for i, step := range steps {
		if step.Invocation == nil {
			continue
		}
		indexes = append(indexes, i)
		references = append(references, step.Invocation.Uses.Source)
		inputs = append(inputs, programBindingMap(step.Invocation.With))
	}
	return indexes, references, inputs
}

func bindProgramActionSelectors(target *program.Program, steps []plan.Step) {
	for i := range target.Job.Steps {
		if target.Job.Steps[i].Invocation != nil && steps[i].Action != nil {
			target.Job.Steps[i].Invocation.Lock = steps[i].Action.Lock
		}
	}
}

func workflowBindings(values map[string]string, surface program.Surface, location program.Location, field string, purpose program.Purpose) []program.Binding {
	if values == nil {
		return nil
	}
	bindings := make([]program.Binding, 0, len(values))
	for _, name := range sortedValueKeys(values) {
		bindings = append(bindings, program.Binding{
			Name:  name,
			Value: workflowSiteWithPurpose(values[name], surface, program.ResultString, location, field+"."+name, purpose),
		})
	}
	return bindings
}

func workflowSites(values []string, surface program.Surface, result program.ResultType, location program.Location, field string) []program.Site {
	if values == nil {
		return nil
	}
	sites := make([]program.Site, len(values))
	for i, value := range values {
		sites[i] = workflowSite(value, surface, result, location, fmt.Sprintf("%s[%d]", field, i))
	}
	return sites
}

func workflowSite(source string, surface program.Surface, result program.ResultType, location program.Location, field string) program.Site {
	return workflowSiteWithPurpose(source, surface, result, location, field, program.PurposeExpression)
}

func workflowSiteWithPurpose(source string, surface program.Surface, result program.ResultType, location program.Location, field string, purpose program.Purpose) program.Site {
	location.Field = field
	return program.Site{Source: source, Surface: surface, Result: result, Provenance: program.ProvenanceWorkflow, Purpose: purpose, Location: location}
}

func programLocation(path, field string, span workflow.Span) program.Location {
	return program.Location{
		File: path, Field: field,
		Start: program.Position{Line: span.Start.Line, Column: span.Start.Column},
		End:   program.Position{Line: span.End.Line, Column: span.End.Column},
	}
}

func programBindingMap(bindings []program.Binding) map[string]string {
	if bindings == nil {
		return nil
	}
	values := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		values[binding.Name] = binding.Value.Source
	}
	return values
}

func programSiteSources(sites []program.Site) []string {
	if sites == nil {
		return nil
	}
	values := make([]string, len(sites))
	for i, site := range sites {
		values[i] = site.Source
	}
	return values
}
