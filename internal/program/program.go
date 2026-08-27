// Package program defines the normalized workflow execution model shared by
// compilation, authority planning, and runtime.
package program

import (
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
)

// Surface selects the expression semantics for one execution site.
type Surface string

const (
	SurfaceJobCondition      Surface = "job-condition"
	SurfaceCallCondition     Surface = "call-condition"
	SurfaceStepCondition     Surface = "step-condition"
	SurfaceJobEnvironment    Surface = "job-environment"
	SurfaceJobDefault        Surface = "job-default"
	SurfaceJobOutput         Surface = "job-output"
	SurfaceStepTemplate      Surface = "step-template"
	SurfaceStepControl       Surface = "step-control"
	SurfaceRuntimeTemplate   Surface = "runtime-template"
	SurfaceServiceTemplate   Surface = "service-template"
	SurfaceServiceCredential Surface = "service-credential"
	SurfaceServiceMap        Surface = "service-map"
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
	PurposeExpression           Purpose = "expression"
	PurposeActionInput          Purpose = "action-input"
	PurposeCompositeActionInput Purpose = "composite-action-input"
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
	Surface    Surface    `json:"profile"`
	Result     ResultType `json:"result"`
	Provenance Provenance `json:"provenance"`
	Purpose    Purpose    `json:"purpose"`
	Location   Location   `json:"location"`
}

func (site Site) expressionSite() expression.Site {
	purpose := expression.PurposeExpression
	switch {
	case site.Provenance == ProvenanceAction:
		purpose = expression.PurposeCompositeActionInput
	case site.Purpose == PurposeActionInput:
		purpose = expression.PurposeWorkflowActionInput
	case site.Purpose == PurposeCompositeActionInput:
		purpose = expression.PurposeCompositeActionInput
	}
	return expression.Site{
		Source:  site.Source,
		Profile: expression.ProfileID(site.Surface),
		Result:  expression.ResultType(site.Result),
		Purpose: purpose,
		Location: expression.Location{
			File:  site.Location.File,
			Field: site.Location.Field,
			Span: expression.Span{
				Start: expression.Position{Line: site.Location.Start.Line, Column: site.Location.Start.Column},
				End:   expression.Position{Line: site.Location.End.Line, Column: site.Location.End.Column},
			},
		},
	}
}

type Binding struct {
	Name  string `json:"name"`
	Value Site   `json:"value"`
}

type BoolControl struct {
	Literal    bool  `json:"literal,omitempty"`
	Expression *Site `json:"expression,omitempty"`
}

type NumberControl struct {
	Literal    float64 `json:"literal,omitempty"`
	Expression *Site   `json:"expression,omitempty"`
}

type Defaults struct {
	Shell            Site `json:"shell"`
	WorkingDirectory Site `json:"working_directory"`
}

type Container struct {
	Image Site      `json:"image"`
	Env   []Binding `json:"env,omitempty"`
	Ports []Site    `json:"ports,omitempty"`
}

type ContainerCredentials struct {
	Username Site `json:"username"`
	Password Site `json:"password"`
}

type ServiceContainer struct {
	Image       Site                  `json:"image"`
	Credentials *ContainerCredentials `json:"credentials,omitempty"`
	Env         []Binding             `json:"env,omitempty"`
	Ports       []Site                `json:"ports,omitempty"`
	Volumes     []Site                `json:"volumes,omitempty"`
	Options     Site                  `json:"options"`
	Command     Site                  `json:"command"`
	Entrypoint  Site                  `json:"entrypoint"`
}

type Service struct {
	Name      string           `json:"name"`
	Container ServiceContainer `json:"container"`
}

type Services struct {
	Static  []Service `json:"static,omitempty"`
	Dynamic *Site     `json:"dynamic,omitempty"`
}

type Run struct {
	Command          Site `json:"command"`
	Shell            Site `json:"shell"`
	WorkingDirectory Site `json:"working_directory"`
}

type Invocation struct {
	Uses Site      `json:"uses"`
	With []Binding `json:"with,omitempty"`
	Lock string    `json:"lock,omitempty"`
}

type Step struct {
	ID              string        `json:"id"`
	Kind            string        `json:"kind"`
	Background      bool          `json:"background,omitempty"`
	Targets         []string      `json:"targets,omitempty"`
	Source          Location      `json:"source"`
	Env             []Binding     `json:"env,omitempty"`
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
	Guards          []Guard    `json:"guards,omitempty"`
	Condition       Site       `json:"condition"`
	ContinueOnError bool       `json:"continue_on_error,omitempty"`
	TimeoutMinutes  float64    `json:"timeout_minutes,omitempty"`
	Env             []Binding  `json:"env,omitempty"`
	Defaults        Defaults   `json:"defaults"`
	Container       *Container `json:"container,omitempty"`
	Services        Services   `json:"services"`
	Steps           []Step     `json:"steps"`
	Outputs         []Binding  `json:"outputs,omitempty"`
}

const Version = 1

type Program struct {
	Version int               `json:"version"`
	Job     Job               `json:"job"`
	Actions map[string]Action `json:"actions,omitempty"`
}

// Validate rejects structural/profile mismatches before an adapter interprets
// the program. Execution positions, not serialized profile claims, select the
// expression policy.
func (p Program) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("execution program version must be %d", Version)
	}
	if len(p.Job.Steps) == 0 {
		return fmt.Errorf("execution program contains no steps")
	}
	engine := expression.NewEngine()
	check := func(site Site, surface Surface, result ResultType, purpose Purpose) error {
		if site.Surface != surface || site.Result != result || site.Purpose != purpose || site.Provenance != ProvenanceWorkflow {
			return fmt.Errorf("execution site %q is inconsistent with its position", site.Location.Field)
		}
		_, err := engine.Validate(site.expressionSite())
		return err
	}
	for _, guard := range p.Job.Guards {
		if err := check(guard.Condition, SurfaceCallCondition, ResultBoolean, PurposeExpression); err != nil {
			return err
		}
	}
	if err := check(p.Job.Condition, SurfaceJobCondition, ResultBoolean, PurposeExpression); err != nil {
		return err
	}
	if err := validateBindings(p.Job.Env, SurfaceJobEnvironment, PurposeExpression, check); err != nil {
		return err
	}
	if err := check(p.Job.Defaults.Shell, SurfaceJobDefault, ResultString, PurposeExpression); err != nil {
		return err
	}
	if err := check(p.Job.Defaults.WorkingDirectory, SurfaceJobDefault, ResultString, PurposeExpression); err != nil {
		return err
	}
	if p.Job.Container != nil {
		if err := validateContainerProgram(*p.Job.Container, check); err != nil {
			return err
		}
	}
	for _, service := range p.Job.Services.Static {
		if err := validateServiceProgram(service.Container, check); err != nil {
			return err
		}
	}
	if p.Job.Services.Dynamic != nil {
		if err := check(*p.Job.Services.Dynamic, SurfaceServiceMap, ResultObject, PurposeExpression); err != nil {
			return err
		}
	}
	for _, step := range p.Job.Steps {
		if err := validateBindings(step.Env, SurfaceStepTemplate, PurposeExpression, check); err != nil {
			return err
		}
		if err := check(step.Condition, SurfaceStepCondition, ResultBoolean, PurposeExpression); err != nil {
			return err
		}
		if step.ContinueOnError.Expression != nil {
			if step.ContinueOnError.Literal {
				return fmt.Errorf("step %q has literal and expression continue-on-error", step.ID)
			}
			if err := check(*step.ContinueOnError.Expression, SurfaceStepControl, ResultBoolean, PurposeExpression); err != nil {
				return err
			}
		}
		if step.TimeoutMinutes.Expression != nil {
			if step.TimeoutMinutes.Literal != 0 {
				return fmt.Errorf("step %q has literal and expression timeout-minutes", step.ID)
			}
			if err := check(*step.TimeoutMinutes.Expression, SurfaceStepControl, ResultNumber, PurposeExpression); err != nil {
				return err
			}
		}
		if err := check(step.Name, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
		switch {
		case step.Run != nil && step.Invocation != nil:
			return fmt.Errorf("step %q has both run and invocation operations", step.ID)
		case step.Run != nil:
			for _, site := range []Site{step.Run.Command, step.Run.Shell, step.Run.WorkingDirectory} {
				if err := check(site, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
					return err
				}
			}
		case step.Invocation != nil:
			if err := check(step.Invocation.Uses, SurfaceRuntimeTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
			if err := validateBindings(step.Invocation.With, SurfaceStepTemplate, PurposeActionInput, check); err != nil {
				return err
			}
		}
	}
	for _, output := range p.Job.Outputs {
		if err := check(output.Value, SurfaceJobOutput, ResultString, PurposeExpression); err != nil {
			return fmt.Errorf("job output %q: %w", output.Name, err)
		}
	}
	for id, action := range p.Actions {
		if id == "" {
			return fmt.Errorf("action program has an empty lock ID")
		}
		if err := validateActionProgram(action, engine); err != nil {
			return fmt.Errorf("action program %q: %w", id, err)
		}
	}
	return nil
}

type siteCheck func(Site, Surface, ResultType, Purpose) error

func validateBindings(bindings []Binding, surface Surface, purpose Purpose, check siteCheck) error {
	for _, binding := range bindings {
		if err := check(binding.Value, surface, ResultString, purpose); err != nil {
			return err
		}
	}
	return nil
}

func validateContainerProgram(container Container, check siteCheck) error {
	if err := check(container.Image, SurfaceRuntimeTemplate, ResultString, PurposeExpression); err != nil {
		return err
	}
	if err := validateBindings(container.Env, SurfaceRuntimeTemplate, PurposeExpression, check); err != nil {
		return err
	}
	for _, site := range container.Ports {
		if err := check(site, SurfaceRuntimeTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
	}
	return nil
}

func validateServiceProgram(container ServiceContainer, check siteCheck) error {
	if err := check(container.Image, SurfaceServiceTemplate, ResultString, PurposeExpression); err != nil {
		return err
	}
	if container.Credentials != nil {
		if err := check(container.Credentials.Username, SurfaceServiceCredential, ResultString, PurposeExpression); err != nil {
			return err
		}
		if err := check(container.Credentials.Password, SurfaceServiceCredential, ResultString, PurposeExpression); err != nil {
			return err
		}
	}
	if err := validateBindings(container.Env, SurfaceServiceTemplate, PurposeExpression, check); err != nil {
		return err
	}
	for _, sites := range [][]Site{container.Ports, container.Volumes} {
		for _, site := range sites {
			if err := check(site, SurfaceServiceTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
	}
	for _, site := range []Site{container.Options, container.Command, container.Entrypoint} {
		if err := check(site, SurfaceServiceTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
	}
	return nil
}

func validateActionProgram(action Action, engine expression.Engine) error {
	switch action.Runtime {
	case "node16", "node24":
		if action.Main == "" {
			return fmt.Errorf("JavaScript action has no main entrypoint")
		}
		if len(action.Steps) != 0 || action.Image != "" || len(action.Args) != 0 {
			return fmt.Errorf("JavaScript action contains incompatible execution fields")
		}
	case "composite":
		if len(action.Steps) == 0 || action.Main != "" || action.Image != "" || len(action.Args) != 0 {
			return fmt.Errorf("composite action contains incompatible execution fields")
		}
	case "docker":
		if action.Image == "" || action.Main != "" || len(action.Steps) != 0 {
			return fmt.Errorf("docker action contains incompatible execution fields")
		}
	default:
		return &metadata.UnsupportedRuntimeError{Runtime: action.Runtime}
	}
	check := func(site Site, surface Surface, result ResultType, purpose Purpose) error {
		if site.Surface != surface || site.Result != result || site.Purpose != purpose || site.Provenance != ProvenanceAction {
			return fmt.Errorf("action execution site %q is inconsistent with its position", site.Location.Field)
		}
		_, err := engine.Validate(site.expressionSite())
		return err
	}
	if err := check(action.PreIf, SurfaceActionLifecycle, ResultBoolean, PurposeExpression); err != nil {
		return err
	}
	if err := check(action.PostIf, SurfaceActionLifecycle, ResultBoolean, PurposeExpression); err != nil {
		return err
	}
	for _, input := range action.Inputs {
		if input.Default != nil {
			if err := check(*input.Default, SurfaceActionInputDefault, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
	}
	for _, output := range action.Outputs {
		if err := check(output.Value, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
	}
	if err := validateActionBindings(action.Env, SurfaceRuntimeTemplate, PurposeExpression, check); err != nil {
		return err
	}
	for _, argument := range action.Args {
		if err := check(argument, SurfaceDockerActionArg, ResultString, PurposeExpression); err != nil {
			return err
		}
	}
	for _, step := range action.Steps {
		for _, site := range []Site{step.Name, step.Shell, step.WorkingDirectory} {
			if err := check(site, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
		if err := check(step.Condition, SurfaceStepCondition, ResultBoolean, PurposeExpression); err != nil {
			return err
		}
		if err := validateActionBindings(step.Env, SurfaceStepTemplate, PurposeExpression, check); err != nil {
			return err
		}
		if step.Run != nil && step.Invocation != nil {
			return fmt.Errorf("composite step has both run and invocation")
		}
		if step.Run == nil && step.Invocation == nil {
			return fmt.Errorf("composite step has no operation")
		}
		if step.Run != nil {
			if err := check(step.Run.Command, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
		if step.Invocation != nil {
			if step.Invocation.Lock == "" {
				return fmt.Errorf("composite invocation has no action lock")
			}
			if err := check(step.Invocation.Uses, SurfaceRuntimeTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
			if err := validateActionBindings(step.Invocation.With, SurfaceStepTemplate, PurposeCompositeActionInput, check); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateActionBindings(bindings []Binding, surface Surface, purpose Purpose, check siteCheck) error {
	for _, binding := range bindings {
		if err := check(binding.Value, surface, ResultString, purpose); err != nil {
			return err
		}
	}
	return nil
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

// TransformSites returns a deep copy with every expression site transformed
// once. The source program remains immutable.
func (p Program) TransformSites(transform func(Site) (Site, error)) (Program, error) {
	result := cloneProgram(p)
	apply := func(site *Site) error {
		if site.Source == "" {
			return nil
		}
		transformed, err := transform(*site)
		if err != nil {
			return err
		}
		*site = transformed
		return nil
	}
	job := &result.Job
	for i := range job.Guards {
		if err := apply(&job.Guards[i].Condition); err != nil {
			return Program{}, err
		}
	}
	if err := apply(&job.Condition); err != nil {
		return Program{}, err
	}
	if err := transformBindings(job.Env, apply); err != nil {
		return Program{}, err
	}
	if err := apply(&job.Defaults.Shell); err != nil {
		return Program{}, err
	}
	if err := apply(&job.Defaults.WorkingDirectory); err != nil {
		return Program{}, err
	}
	if job.Container != nil {
		if err := apply(&job.Container.Image); err != nil {
			return Program{}, err
		}
		if err := transformBindings(job.Container.Env, apply); err != nil {
			return Program{}, err
		}
		if err := transformSites(job.Container.Ports, apply); err != nil {
			return Program{}, err
		}
	}
	for i := range job.Services.Static {
		container := &job.Services.Static[i].Container
		if err := apply(&container.Image); err != nil {
			return Program{}, err
		}
		if container.Credentials != nil {
			if err := apply(&container.Credentials.Username); err != nil {
				return Program{}, err
			}
			if err := apply(&container.Credentials.Password); err != nil {
				return Program{}, err
			}
		}
		if err := transformBindings(container.Env, apply); err != nil {
			return Program{}, err
		}
		if err := transformSites(container.Ports, apply); err != nil {
			return Program{}, err
		}
		if err := transformSites(container.Volumes, apply); err != nil {
			return Program{}, err
		}
		for _, site := range []*Site{&container.Options, &container.Command, &container.Entrypoint} {
			if err := apply(site); err != nil {
				return Program{}, err
			}
		}
	}
	if job.Services.Dynamic != nil {
		if err := apply(job.Services.Dynamic); err != nil {
			return Program{}, err
		}
	}
	for i := range job.Steps {
		step := &job.Steps[i]
		if err := transformBindings(step.Env, apply); err != nil {
			return Program{}, err
		}
		if err := apply(&step.Condition); err != nil {
			return Program{}, err
		}
		if step.ContinueOnError.Expression != nil {
			if err := apply(step.ContinueOnError.Expression); err != nil {
				return Program{}, err
			}
		}
		if step.TimeoutMinutes.Expression != nil {
			if err := apply(step.TimeoutMinutes.Expression); err != nil {
				return Program{}, err
			}
		}
		if err := apply(&step.Name); err != nil {
			return Program{}, err
		}
		if step.Run != nil {
			for _, site := range []*Site{&step.Run.Command, &step.Run.Shell, &step.Run.WorkingDirectory} {
				if err := apply(site); err != nil {
					return Program{}, err
				}
			}
		}
		if step.Invocation != nil {
			if err := apply(&step.Invocation.Uses); err != nil {
				return Program{}, err
			}
			if err := transformBindings(step.Invocation.With, apply); err != nil {
				return Program{}, err
			}
		}
	}
	if err := transformBindings(job.Outputs, apply); err != nil {
		return Program{}, err
	}
	for id, action := range result.Actions {
		transformed, err := action.transformSites(apply)
		if err != nil {
			return Program{}, err
		}
		result.Actions[id] = transformed
	}
	return result, nil
}

func transformBindings(bindings []Binding, apply func(*Site) error) error {
	for i := range bindings {
		if err := apply(&bindings[i].Value); err != nil {
			return err
		}
	}
	return nil
}

func transformSites(sites []Site, apply func(*Site) error) error {
	for i := range sites {
		if err := apply(&sites[i]); err != nil {
			return err
		}
	}
	return nil
}

func cloneProgram(source Program) Program {
	result := source
	job := source.Job
	result.Job = job
	result.Actions = make(map[string]Action, len(source.Actions))
	for id, action := range source.Actions {
		result.Actions[id] = cloneAction(action)
	}
	result.Job.Guards = append([]Guard(nil), job.Guards...)
	result.Job.Env = cloneBindings(job.Env)
	result.Job.Outputs = cloneBindings(job.Outputs)
	result.Job.Steps = append([]Step(nil), job.Steps...)
	result.Job.Services.Static = append([]Service(nil), job.Services.Static...)
	if job.Services.Dynamic != nil {
		value := *job.Services.Dynamic
		result.Job.Services.Dynamic = &value
	}
	if job.Container != nil {
		value := *job.Container
		value.Env = cloneBindings(value.Env)
		value.Ports = append([]Site(nil), value.Ports...)
		result.Job.Container = &value
	}
	for i := range result.Job.Services.Static {
		container := result.Job.Services.Static[i].Container
		container.Env = cloneBindings(container.Env)
		container.Ports = append([]Site(nil), container.Ports...)
		container.Volumes = append([]Site(nil), container.Volumes...)
		if container.Credentials != nil {
			value := *container.Credentials
			container.Credentials = &value
		}
		result.Job.Services.Static[i].Container = container
	}
	for i := range result.Job.Steps {
		step := &result.Job.Steps[i]
		step.Targets = append([]string(nil), step.Targets...)
		step.Env = cloneBindings(step.Env)
		if step.ContinueOnError.Expression != nil {
			value := *step.ContinueOnError.Expression
			step.ContinueOnError.Expression = &value
		}
		if step.TimeoutMinutes.Expression != nil {
			value := *step.TimeoutMinutes.Expression
			step.TimeoutMinutes.Expression = &value
		}
		if step.Run != nil {
			value := *step.Run
			step.Run = &value
		}
		if step.Invocation != nil {
			value := *step.Invocation
			value.With = cloneBindings(value.With)
			step.Invocation = &value
		}
	}
	return result
}

func cloneBindings(source []Binding) []Binding { return append([]Binding(nil), source...) }
