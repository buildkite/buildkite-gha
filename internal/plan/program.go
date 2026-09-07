package plan

import (
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/program"
)

// ProjectProgram populates the executor-only job fields from the normalized
// execution program.
func (job *Job) ProjectProgram() error {
	if job.Program == nil {
		return fmt.Errorf("execution program is required")
	}
	if err := job.Program.Validate(); err != nil {
		return err
	}
	source := job.Program.Job
	if len(job.CallGuards) != len(source.Guards) {
		return fmt.Errorf("call guard projection has %d entries, normalized program has %d", len(job.CallGuards), len(source.Guards))
	}
	for i := range job.CallGuards {
		job.CallGuards[i].Condition = source.Guards[i].Condition.Source
	}
	job.Env = bindingMap(source.Env)
	job.Condition = source.Condition.Source
	job.ContinueOnError = source.ContinueOnError
	job.TimeoutMinutes = source.TimeoutMinutes
	job.DefaultShell = source.Defaults.Shell.Source
	job.DefaultWorkingDirectory = source.Defaults.WorkingDirectory.Source
	job.Outputs = bindingMap(source.Outputs)
	job.Steps = make([]Step, len(source.Steps))
	for i, step := range source.Steps {
		execution := step
		projected := Step{
			ID: step.ID, Kind: step.Kind, Background: step.Background,
			Targets: append([]string(nil), step.Targets...), Env: bindingMap(step.Env),
			Condition: step.Condition.Source, ContinueOnError: step.ContinueOnError.Literal,
			TimeoutMinutes: step.TimeoutMinutes.Literal, Name: step.Name.Source,
			Execution: &execution,
		}
		if step.Source.Start.Line != 0 {
			projected.Source = &Span{
				Start: Position{Line: step.Source.Start.Line, Column: step.Source.Start.Column},
				End:   Position{Line: step.Source.End.Line, Column: step.Source.End.Column},
			}
		}
		if step.ContinueOnError.Expression != nil {
			projected.ContinueOnErrorExpression = step.ContinueOnError.Expression.Source
		}
		if step.TimeoutMinutes.Expression != nil {
			projected.TimeoutMinutesExpression = step.TimeoutMinutes.Expression.Source
		}
		if step.Run != nil {
			projected.Command = step.Run.Command.Source
			projected.Shell = step.Run.Shell.Source
			projected.WorkingDirectory = step.Run.WorkingDirectory.Source
		}
		if step.Invocation != nil {
			projected.Uses = step.Invocation.Uses.Source
			projected.With = bindingMap(step.Invocation.With)
			if step.Invocation.Lock != "" {
				projected.Action = &ActionSelector{Lock: step.Invocation.Lock}
			}
		}
		job.Steps[i] = projected
	}
	job.Container = nil
	if source.Container != nil {
		job.Container = &Container{Image: source.Container.Image.Source, Env: bindingMap(source.Container.Env), Ports: siteSources(source.Container.Ports)}
	}
	job.Services = nil
	job.ServiceOrder = nil
	job.ServicesExpression = ""
	if source.Services.Dynamic != nil {
		job.ServicesExpression = source.Services.Dynamic.Source
	}
	if len(source.Services.Static) != 0 {
		job.Services = make(map[string]ServiceContainer, len(source.Services.Static))
		job.ServiceOrder = make([]string, 0, len(source.Services.Static))
	}
	for _, service := range source.Services.Static {
		container := ServiceContainer{
			Image: service.Container.Image.Source, Env: bindingMap(service.Container.Env),
			Ports: siteSources(service.Container.Ports), Volumes: siteSources(service.Container.Volumes),
			Options: service.Container.Options.Source, Command: service.Container.Command.Source,
			Entrypoint: service.Container.Entrypoint.Source,
		}
		if service.Container.Credentials != nil {
			container.Credentials = &ContainerCredentials{
				Username: service.Container.Credentials.Username.Source,
				Password: service.Container.Credentials.Password.Source,
			}
		}
		job.Services[service.Name] = container
		job.ServiceOrder = append(job.ServiceOrder, service.Name)
	}
	return nil
}

func bindingMap(values []program.Binding) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.Name] = value.Value.Source
	}
	return result
}

func siteSources(values []program.Site) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.Source
	}
	return result
}
