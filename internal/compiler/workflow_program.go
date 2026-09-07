package compiler

import (
	"fmt"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/program"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func lowerWorkflowProgram(instance JobInstance) program.Program {
	jobLocation := programLocation(instance.SourcePath, "job", instance.Source)
	result := program.Program{Version: program.Version, Job: program.Job{
		Condition:       workflowSite(instance.If, jobLocation, "job.if"),
		ContinueOnError: instance.ContinueOnError,
		TimeoutMinutes:  instance.TimeoutMinutes,
		Env:             workflowBindings(instance.Env, jobLocation, "job.env"),
		Defaults: program.Defaults{
			Shell:            workflowSite(instance.DefaultShell, jobLocation, "job.defaults.run.shell"),
			WorkingDirectory: workflowSite(instance.DefaultWorkingDirectory, jobLocation, "job.defaults.run.working-directory"),
		},
		Outputs: workflowBindings(instance.Outputs, jobLocation, "job.outputs"),
	}}
	if len(instance.CallGuards) != 0 {
		result.Job.Guards = make([]program.Guard, len(instance.CallGuards))
		for i, guard := range instance.CallGuards {
			field := fmt.Sprintf("job.call-guards[%d].if", i)
			result.Job.Guards[i] = program.Guard{Condition: workflowSite(guard.Condition, jobLocation, field)}
		}
	}
	if instance.Container != nil {
		result.Job.Container = &program.Container{
			Image: workflowSite(instance.Container.Image, jobLocation, "job.container.image"),
			Env:   workflowBindings(instance.Container.Env, jobLocation, "job.container.env"),
			Ports: workflowSites(instance.Container.Ports, jobLocation, "job.container.ports"),
		}
	}
	if len(instance.Services) != 0 {
		result.Job.Services.Static = make([]program.Service, len(instance.Services))
		for i, service := range instance.Services {
			field := fmt.Sprintf("job.services.%s", service.Name)
			location := programLocation(instance.SourcePath, field, service.Container.Span)
			container := service.Container
			lowered := program.ServiceContainer{
				Image:      workflowSite(container.Image, location, field+".image"),
				Env:        workflowBindings(container.Env, location, field+".env"),
				Ports:      workflowSites(container.Ports, location, field+".ports"),
				Volumes:    workflowSites(container.Volumes, location, field+".volumes"),
				Options:    workflowSite(container.Options, location, field+".options"),
				Command:    workflowSite(container.Command, location, field+".command"),
				Entrypoint: workflowSite(container.Entrypoint, location, field+".entrypoint"),
			}
			if container.Credentials != nil {
				lowered.Credentials = &program.ContainerCredentials{
					Username: workflowSite(container.Credentials.Username, location, field+".credentials.username"),
					Password: workflowSite(container.Credentials.Password, location, field+".credentials.password"),
				}
			}
			result.Job.Services.Static[i] = program.Service{Name: service.Name, Container: lowered}
		}
	}
	if instance.ServicesExpression != "" {
		site := workflowSite(instance.ServicesExpression, jobLocation, "job.services")
		result.Job.Services.Dynamic = &site
	}
	result.Job.Steps = lowerWorkflowSteps(instance.SourcePath, instance.Steps)
	result.DeriveSiteSemantics()
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
			Env:        workflowBindings(step.Env, location, field+".env"),
			Condition:  workflowSite(step.If, location, field+".if"),
			ContinueOnError: program.BoolControl{
				Literal: step.ContinueOnError,
			},
			TimeoutMinutes: program.NumberControl{
				Literal: step.TimeoutMinutes,
			},
			Name: workflowSite(step.Name, location, field+".name"),
		}
		if step.ContinueOnErrorExpression != "" {
			site := workflowSite(step.ContinueOnErrorExpression, location, field+".continue-on-error")
			steps[i].ContinueOnError.Expression = &site
		}
		if step.TimeoutMinutesExpression != "" {
			site := workflowSite(step.TimeoutMinutesExpression, location, field+".timeout-minutes")
			steps[i].TimeoutMinutes.Expression = &site
		}
		switch step.Kind {
		case "run":
			steps[i].Run = &program.Run{
				Command:          workflowSite(step.Run, location, field+".run"),
				Shell:            workflowSite(step.Shell, location, field+".shell"),
				WorkingDirectory: workflowSite(step.WorkingDirectory, location, field+".working-directory"),
			}
		case "uses":
			steps[i].Invocation = &program.Invocation{
				Uses: workflowSite(step.Uses, location, field+".uses"),
				With: workflowBindings(step.With, location, field+".with"),
			}
		}
	}
	return steps
}

func workflowBindings(values map[string]string, location program.Location, field string) []program.Binding {
	if values == nil {
		return nil
	}
	bindings := make([]program.Binding, 0, len(values))
	for _, name := range sortedValueKeys(values) {
		bindings = append(bindings, program.Binding{
			Name:  name,
			Value: workflowSite(values[name], location, field+"."+name),
		})
	}
	return bindings
}

func workflowSites(values []string, location program.Location, field string) []program.Site {
	if values == nil {
		return nil
	}
	sites := make([]program.Site, len(values))
	for i, value := range values {
		sites[i] = workflowSite(value, location, fmt.Sprintf("%s[%d]", field, i))
	}
	return sites
}

func workflowSite(source string, location program.Location, field string) program.Site {
	location.Field = field
	return program.Site{Source: source, Location: location}
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
