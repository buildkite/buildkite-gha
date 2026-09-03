package plan

import (
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/program"
)

// ExecutionJob returns the normalized GitHub Actions workflow job assigned to
// this Buildkite command job envelope. The program package owns its executable
// semantics; plan exposes it only as part of the serialized handoff.
func (job Job) ExecutionJob() *program.Job {
	if job.Program == nil {
		return nil
	}
	return &job.Program.Job
}

// ProjectProgram validates Program and refreshes the compatibility projections
// retained by the plan envelope. Job-level executor fields are not serialized;
// CallGuards.Condition remains duplicated only to preserve the v2 wire format.
func (job *Job) ProjectProgram() error {
	if job.Program == nil {
		return fmt.Errorf("execution program is required")
	}
	if err := job.Program.Validate(); err != nil {
		return err
	}
	source := *job.ExecutionJob()
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
