// Package program defines the normalized workflow execution model shared by
// compilation, authority planning, and, after the plan-schema cutover, runtime.
package program

// Surface selects the expression semantics for one execution site.
type Surface string

const (
	SurfaceJobCondition       Surface = "job-condition"
	SurfaceStepCondition      Surface = "step-condition"
	SurfaceJobEnvironment     Surface = "job-environment"
	SurfaceJobDefault         Surface = "job-default"
	SurfaceJobOutput          Surface = "job-output"
	SurfaceStepTemplate       Surface = "step-template"
	SurfaceStepControl        Surface = "step-control"
	SurfaceRuntimeTemplate    Surface = "runtime-template"
	SurfaceServiceCredential  Surface = "service-credential"
	SurfaceServiceMap         Surface = "service-map"
	SurfaceActionInputDefault Surface = "action-input-default"
	SurfaceActionLifecycle    Surface = "action-lifecycle-condition"
	SurfaceCompositeTemplate  Surface = "composite-template"
	SurfaceDockerArgument     Surface = "docker-argument"
)

// ResultType records the value shape required by an expression site.
type ResultType string

const (
	ResultString  ResultType = "string"
	ResultBoolean ResultType = "boolean"
	ResultNumber  ResultType = "number"
	ResultObject  ResultType = "object"
)

// Provenance records who authored an expression.
type Provenance string

const (
	ProvenanceWorkflow Provenance = "workflow"
	ProvenanceAction   Provenance = "action"
)

// Purpose identifies sites with an authority exception. Supplied action inputs
// may delegate ordinary-secret inventory to resolved action metadata.
type Purpose string

const (
	PurposeExpression  Purpose = "expression"
	PurposeActionInput Purpose = "action-input"
)

type Position struct {
	Line   int
	Column int
}

// Location retains the workflow source and stable normalized field path used
// for diagnostics and coverage tests.
type Location struct {
	File  string
	Field string
	Start Position
	End   Position
}

// Site is one expression or template evaluated at a defined execution point.
type Site struct {
	Source     string
	Surface    Surface
	Result     ResultType
	Provenance Provenance
	Purpose    Purpose
	Location   Location
}

type Binding struct {
	Name  string
	Value Site
}

type BoolControl struct {
	Literal    bool
	Expression *Site
}

type NumberControl struct {
	Literal    float64
	Expression *Site
}

type Defaults struct {
	Shell            Site
	WorkingDirectory Site
}

type Container struct {
	Image Site
	Env   []Binding
	Ports []Site
}

type ContainerCredentials struct {
	Username Site
	Password Site
}

type ServiceContainer struct {
	Image       Site
	Credentials *ContainerCredentials
	Env         []Binding
	Ports       []Site
	Volumes     []Site
	Options     Site
	Command     Site
	Entrypoint  Site
}

type Service struct {
	Name      string
	Container ServiceContainer
}

type Services struct {
	Static  []Service
	Dynamic *Site
}

type Run struct {
	Command          Site
	Shell            Site
	WorkingDirectory Site
}

type Invocation struct {
	Uses Site
	With []Binding
	Lock string
}

type Step struct {
	ID              string
	Kind            string
	Background      bool
	Targets         []string
	Source          Location
	Env             []Binding
	Condition       Site
	ContinueOnError BoolControl
	TimeoutMinutes  NumberControl
	Name            Site
	Run             *Run
	Invocation      *Invocation
}

type Guard struct {
	Condition Site
}

type Job struct {
	Guards          []Guard
	Condition       Site
	ContinueOnError bool
	TimeoutMinutes  float64
	Env             []Binding
	Defaults        Defaults
	Container       *Container
	Services        Services
	Steps           []Step
	Outputs         []Binding
}

type Program struct {
	Job Job
}

type ActionRuntime string

const (
	ActionRuntimeNative     ActionRuntime = "native"
	ActionRuntimeJavaScript ActionRuntime = "javascript"
	ActionRuntimeComposite  ActionRuntime = "composite"
	ActionRuntimeDocker     ActionRuntime = "docker"
)

type ActionInput struct {
	Name     string
	Required bool
	Default  *Site
}

type JavaScriptAction struct {
	NodeMajor     int
	Pre           string
	PreCondition  Site
	Main          string
	Post          string
	PostCondition Site
}

type DockerAction struct {
	Arguments []Site
	Env       []Binding
}

type CompositeStep struct {
	ID              string
	Name            Site
	Condition       Site
	ContinueOnError bool
	Env             []Binding
	Run             *Run
	Invocation      *Invocation
}

type CompositeAction struct {
	Steps []CompositeStep
}

// Action is the immutable execution model derived from one resolved action
// manifest. Source locks retain repository identity and tree verification.
type Action struct {
	Name       string
	Source     string
	Runtime    ActionRuntime
	Inputs     []ActionInput
	Outputs    []Binding
	JavaScript *JavaScriptAction
	Composite  *CompositeAction
	Docker     *DockerAction
	Location   Location
}

// VisitSites walks every action-authored expression site in lifecycle order.
func (a Action) VisitSites(visit func(Site) error) error {
	for _, input := range a.Inputs {
		if input.Default != nil {
			if err := visitSite(*input.Default, visit); err != nil {
				return err
			}
		}
	}
	if a.JavaScript != nil {
		if err := visitSite(a.JavaScript.PreCondition, visit); err != nil {
			return err
		}
		if err := visitSite(a.JavaScript.PostCondition, visit); err != nil {
			return err
		}
	}
	if a.Docker != nil {
		for _, argument := range a.Docker.Arguments {
			if err := visitSite(argument, visit); err != nil {
				return err
			}
		}
		if err := visitBindings(a.Docker.Env, visit); err != nil {
			return err
		}
	}
	if a.Composite != nil {
		for _, step := range a.Composite.Steps {
			if err := visitSite(step.Condition, visit); err != nil {
				return err
			}
			if err := visitSite(step.Name, visit); err != nil {
				return err
			}
			if err := visitBindings(step.Env, visit); err != nil {
				return err
			}
			if step.Run != nil {
				for _, site := range []Site{step.Run.Command, step.Run.Shell, step.Run.WorkingDirectory} {
					if err := visitSite(site, visit); err != nil {
						return err
					}
				}
			}
			if step.Invocation != nil {
				if err := visitSite(step.Invocation.Uses, visit); err != nil {
					return err
				}
				if err := visitBindings(step.Invocation.With, visit); err != nil {
					return err
				}
			}
		}
	}
	return visitBindings(a.Outputs, visit)
}

// VisitSites walks every expression-bearing field once in execution order.
// Validation and ordinary-secret inventory deliberately use this exhaustive
// walk rather than following runtime guards.
func (p Program) VisitSites(visit func(Site) error) error {
	for _, guard := range p.Job.Guards {
		if err := visitSite(guard.Condition, visit); err != nil {
			return err
		}
	}
	if err := visitSite(p.Job.Condition, visit); err != nil {
		return err
	}
	if err := visitBindings(p.Job.Env, visit); err != nil {
		return err
	}
	if err := visitSite(p.Job.Defaults.Shell, visit); err != nil {
		return err
	}
	if err := visitSite(p.Job.Defaults.WorkingDirectory, visit); err != nil {
		return err
	}
	if p.Job.Container != nil {
		if err := visitContainer(*p.Job.Container, visit); err != nil {
			return err
		}
	}
	for _, service := range p.Job.Services.Static {
		if err := visitServiceContainer(service.Container, visit); err != nil {
			return err
		}
	}
	if p.Job.Services.Dynamic != nil {
		if err := visitSite(*p.Job.Services.Dynamic, visit); err != nil {
			return err
		}
	}
	for _, step := range p.Job.Steps {
		if err := visitBindings(step.Env, visit); err != nil {
			return err
		}
		if err := visitSite(step.Condition, visit); err != nil {
			return err
		}
		if step.ContinueOnError.Expression != nil {
			if err := visitSite(*step.ContinueOnError.Expression, visit); err != nil {
				return err
			}
		}
		if step.TimeoutMinutes.Expression != nil {
			if err := visitSite(*step.TimeoutMinutes.Expression, visit); err != nil {
				return err
			}
		}
		if err := visitSite(step.Name, visit); err != nil {
			return err
		}
		if step.Run != nil {
			for _, site := range []Site{step.Run.Command, step.Run.Shell, step.Run.WorkingDirectory} {
				if err := visitSite(site, visit); err != nil {
					return err
				}
			}
		}
		if step.Invocation != nil {
			if err := visitSite(step.Invocation.Uses, visit); err != nil {
				return err
			}
			if err := visitBindings(step.Invocation.With, visit); err != nil {
				return err
			}
		}
	}
	return visitBindings(p.Job.Outputs, visit)
}

func visitContainer(container Container, visit func(Site) error) error {
	if err := visitSite(container.Image, visit); err != nil {
		return err
	}
	if err := visitBindings(container.Env, visit); err != nil {
		return err
	}
	for _, port := range container.Ports {
		if err := visitSite(port, visit); err != nil {
			return err
		}
	}
	return nil
}

func visitServiceContainer(container ServiceContainer, visit func(Site) error) error {
	if err := visitSite(container.Image, visit); err != nil {
		return err
	}
	if container.Credentials != nil {
		if err := visitSite(container.Credentials.Username, visit); err != nil {
			return err
		}
		if err := visitSite(container.Credentials.Password, visit); err != nil {
			return err
		}
	}
	if err := visitBindings(container.Env, visit); err != nil {
		return err
	}
	for _, sites := range [][]Site{container.Ports, container.Volumes} {
		for _, site := range sites {
			if err := visitSite(site, visit); err != nil {
				return err
			}
		}
	}
	for _, site := range []Site{container.Options, container.Command, container.Entrypoint} {
		if err := visitSite(site, visit); err != nil {
			return err
		}
	}
	return nil
}

func visitBindings(bindings []Binding, visit func(Site) error) error {
	for _, binding := range bindings {
		if err := visitSite(binding.Value, visit); err != nil {
			return err
		}
	}
	return nil
}

func visitSite(site Site, visit func(Site) error) error {
	if site.Source == "" {
		return nil
	}
	return visit(site)
}
