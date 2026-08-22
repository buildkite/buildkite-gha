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
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Location retains the workflow source and stable normalized field path used
// for diagnostics and coverage tests.
type Location struct {
	File  string   `json:"file"`
	Field string   `json:"field"`
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Site is one expression or template evaluated at a defined execution point.
type Site struct {
	Source     string     `json:"source"`
	Surface    Surface    `json:"surface"`
	Result     ResultType `json:"result"`
	Provenance Provenance `json:"provenance"`
	Purpose    Purpose    `json:"purpose"`
	Location   Location   `json:"location"`
}

type Binding struct {
	Name  string `json:"name"`
	Value Site   `json:"value"`
}

type BoolControl struct {
	Literal    bool  `json:"literal"`
	Expression *Site `json:"expression,omitempty"`
}

type NumberControl struct {
	Literal    float64 `json:"literal"`
	Expression *Site   `json:"expression,omitempty"`
}

type Defaults struct {
	Shell            Site `json:"shell"`
	WorkingDirectory Site `json:"working_directory"`
}

type Container struct {
	Image Site      `json:"image"`
	Env   []Binding `json:"env"`
	Ports []Site    `json:"ports"`
}

type ContainerCredentials struct {
	Username Site `json:"username"`
	Password Site `json:"password"`
}

type ServiceContainer struct {
	Image       Site                  `json:"image"`
	Credentials *ContainerCredentials `json:"credentials,omitempty"`
	Env         []Binding             `json:"env"`
	Ports       []Site                `json:"ports"`
	Volumes     []Site                `json:"volumes"`
	Options     Site                  `json:"options"`
	Command     Site                  `json:"command"`
	Entrypoint  Site                  `json:"entrypoint"`
}

type Service struct {
	Name      string           `json:"name"`
	Container ServiceContainer `json:"container"`
}

type Services struct {
	Static  []Service `json:"static"`
	Dynamic *Site     `json:"dynamic,omitempty"`
}

type Run struct {
	Command          Site `json:"command"`
	Shell            Site `json:"shell"`
	WorkingDirectory Site `json:"working_directory"`
}

type Invocation struct {
	Uses Site      `json:"uses"`
	With []Binding `json:"with"`
	Lock string    `json:"lock"`
}

type Step struct {
	ID              string        `json:"id"`
	Kind            string        `json:"kind"`
	Background      bool          `json:"background"`
	Targets         []string      `json:"targets"`
	Source          Location      `json:"source"`
	Env             []Binding     `json:"env"`
	Condition       Site          `json:"condition"`
	ContinueOnError BoolControl   `json:"continue_on_error"`
	TimeoutMinutes  NumberControl `json:"timeout_minutes"`
	Name            Site          `json:"name"`
	Run             *Run          `json:"run,omitempty"`
	Invocation      *Invocation   `json:"invocation,omitempty"`
}

type Guard struct {
	Condition Site `json:"condition"`
}

type Job struct {
	Guards          []Guard    `json:"guards"`
	Condition       Site       `json:"condition"`
	ContinueOnError bool       `json:"continue_on_error"`
	TimeoutMinutes  float64    `json:"timeout_minutes"`
	Env             []Binding  `json:"env"`
	Defaults        Defaults   `json:"defaults"`
	Container       *Container `json:"container,omitempty"`
	Services        Services   `json:"services"`
	Steps           []Step     `json:"steps"`
	Outputs         []Binding  `json:"outputs"`
}

type Program struct {
	Job Job `json:"job"`
}

type ActionRuntime string

const (
	ActionRuntimeNative     ActionRuntime = "native"
	ActionRuntimeJavaScript ActionRuntime = "javascript"
	ActionRuntimeComposite  ActionRuntime = "composite"
	ActionRuntimeDocker     ActionRuntime = "docker"
)

type ActionInput struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Default  *Site  `json:"default,omitempty"`
}

type JavaScriptAction struct {
	NodeMajor     int    `json:"node_major"`
	Pre           string `json:"pre"`
	PreCondition  Site   `json:"pre_condition"`
	Main          string `json:"main"`
	Post          string `json:"post"`
	PostCondition Site   `json:"post_condition"`
}

type DockerAction struct {
	Arguments []Site    `json:"arguments"`
	Env       []Binding `json:"env"`
}

type CompositeStep struct {
	ID              string      `json:"id"`
	Name            Site        `json:"name"`
	Condition       Site        `json:"condition"`
	ContinueOnError bool        `json:"continue_on_error"`
	Env             []Binding   `json:"env"`
	Run             *Run        `json:"run,omitempty"`
	Invocation      *Invocation `json:"invocation,omitempty"`
}

type CompositeAction struct {
	Steps []CompositeStep `json:"steps"`
}

// Action is the immutable execution model derived from one resolved action
// manifest. Source locks retain repository identity and tree verification.
type Action struct {
	Name       string            `json:"name"`
	Source     string            `json:"source"`
	Runtime    ActionRuntime     `json:"runtime"`
	Inputs     []ActionInput     `json:"inputs"`
	Outputs    []Binding         `json:"outputs"`
	JavaScript *JavaScriptAction `json:"javascript,omitempty"`
	Composite  *CompositeAction  `json:"composite,omitempty"`
	Docker     *DockerAction     `json:"docker,omitempty"`
	Location   Location          `json:"location"`
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
