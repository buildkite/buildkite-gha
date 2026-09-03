package program

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

const maxStepTargets = 256

var stepIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

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
	File  string   `json:"file,omitempty"`
	Field string   `json:"field"`
	Start Position `json:"start,omitzero"`
	End   Position `json:"end,omitzero"`
}

// Site is one expression or template evaluated at a defined execution point.
type Site struct {
	Source     string     `json:"source"`
	Surface    Surface    `json:"-"`
	Result     ResultType `json:"-"`
	Provenance Provenance `json:"-"`
	Purpose    Purpose    `json:"-"`
	Location   Location   `json:"location"`
}

func (site Site) expressionSite() expression.Site {
	purpose := expression.PurposeExpression
	switch {
	case site.Purpose == PurposeActionInput:
		purpose = expression.PurposeWorkflowActionInput
	case site.Purpose == PurposeCompositeActionInput || site.Provenance == ProvenanceAction:
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

// ReferencesStatus reports whether a condition site explicitly handles job status.
func ReferencesStatus(site Site) (bool, error) {
	return expression.NewEngine().ReferencesStatus(site.expressionSite())
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
func (p *Program) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("execution program version must be %d", Version)
	}
	if len(p.Job.Steps) == 0 {
		return fmt.Errorf("execution program contains no steps")
	}
	engine := expression.NewEngine()
	if err := validateSteps(p.Job.Steps); err != nil {
		return err
	}
	for _, id := range SortedActionIDs(p.Actions) {
		action := p.Actions[id]
		if id == "" {
			return fmt.Errorf("action program has an empty lock ID")
		}
		if err := validateActionStructure(action); err != nil {
			return fmt.Errorf("action program %q: %w", id, err)
		}
	}
	p.DeriveSiteSemantics()
	return p.walkSites(func(site *Site) error {
		_, err := engine.Validate(site.expressionSite())
		return err
	})
}

func validateSteps(steps []Step) error {
	ids := make(map[string]struct{}, len(steps))
	backgroundIDs := make(map[string]struct{})
	for i, step := range steps {
		if step.ID == "" {
			return fmt.Errorf("job plan step %d has no deterministic id", i+1)
		}
		if len(step.ID) > 255 {
			return fmt.Errorf("job plan step id %q exceeds 255 bytes", step.ID)
		}
		id := strings.ToLower(step.ID)
		if _, exists := ids[id]; exists {
			return fmt.Errorf("job plan contains duplicate step id %q", step.ID)
		}
		ids[id] = struct{}{}
		if step.TimeoutMinutes.Literal < 0 || step.TimeoutMinutes.Literal > 360 {
			return fmt.Errorf("job plan step %q timeout_minutes must be between 0 and 360", step.ID)
		}
		if step.TimeoutMinutes.Expression != nil && step.TimeoutMinutes.Literal != 0 {
			return fmt.Errorf("job plan step %q has both literal and expression timeout_minutes", step.ID)
		}
		if step.ContinueOnError.Expression != nil && step.ContinueOnError.Literal {
			return fmt.Errorf("job plan step %q has both literal and expression continue_on_error", step.ID)
		}
		if err := validateStepSiteLength(step.ID, "timeout_minutes", step.TimeoutMinutes.Expression); err != nil {
			return err
		}
		if err := validateStepSiteLength(step.ID, "continue_on_error", step.ContinueOnError.Expression); err != nil {
			return err
		}
		if len(step.Condition.Source) > 65536 {
			return fmt.Errorf("job plan step %q condition exceeds 65536 bytes", step.ID)
		}
		switch step.Kind {
		case "run":
			if step.Run == nil || strings.TrimSpace(step.Run.Command.Source) == "" {
				return fmt.Errorf("run step %q has no command", step.ID)
			}
			if step.Invocation != nil || len(step.Targets) != 0 {
				return fmt.Errorf("run step %q contains incompatible action or control fields", step.ID)
			}
			if step.Background {
				backgroundIDs[id] = struct{}{}
			}
		case "uses":
			if step.Invocation == nil || strings.TrimSpace(step.Invocation.Uses.Source) == "" {
				return fmt.Errorf("action step %q has no action reference", step.ID)
			}
			if len(step.Invocation.Uses.Source) > 1024 {
				return fmt.Errorf("action step %q reference exceeds 1024 bytes", step.ID)
			}
			if step.Run != nil || len(step.Targets) != 0 {
				return fmt.Errorf("action step %q contains incompatible run or control fields", step.ID)
			}
			if step.Background {
				backgroundIDs[id] = struct{}{}
			}
		case "wait", "wait-all", "cancel":
			if err := validateControlStep(step, backgroundIDs); err != nil {
				return err
			}
		default:
			return fmt.Errorf("step %q has unsupported kind %q", step.ID, step.Kind)
		}
	}
	return nil
}

func validateStepSiteLength(stepID, name string, site *Site) error {
	if site == nil {
		return nil
	}
	if len(site.Source) > 65536 {
		return fmt.Errorf("job plan step %q control expression exceeds 65536 bytes", stepID)
	}
	trimmed := strings.TrimSpace(site.Source)
	if !strings.HasPrefix(trimmed, "${{") || !strings.HasSuffix(trimmed, "}}") {
		return fmt.Errorf("job plan step %q %s expression must be complete", stepID, name)
	}
	return nil
}

func validateControlStep(step Step, backgroundIDs map[string]struct{}) error {
	if step.Background || step.Run != nil || step.Invocation != nil || len(step.Env) != 0 || step.Condition.Source != "" || step.ContinueOnError.Literal || step.ContinueOnError.Expression != nil || step.TimeoutMinutes.Literal != 0 || step.TimeoutMinutes.Expression != nil {
		return fmt.Errorf("control step %q contains incompatible execution fields", step.ID)
	}
	switch step.Kind {
	case "wait":
		if len(step.Targets) == 0 || len(step.Targets) > maxStepTargets {
			return fmt.Errorf("wait step %q must target between 1 and %d background steps", step.ID, maxStepTargets)
		}
	case "wait-all":
		if len(step.Targets) != 0 {
			return fmt.Errorf("wait-all step %q cannot target individual steps", step.ID)
		}
		return nil
	case "cancel":
		if len(step.Targets) != 1 {
			return fmt.Errorf("cancel step %q must target exactly one background step", step.ID)
		}
	}
	seen := make(map[string]struct{}, len(step.Targets))
	for _, target := range step.Targets {
		if !stepIDPattern.MatchString(target) {
			return fmt.Errorf("control step %q has invalid target %q", step.ID, target)
		}
		key := strings.ToLower(target)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("control step %q repeats target %q", step.ID, target)
		}
		seen[key] = struct{}{}
		if _, exists := backgroundIDs[key]; !exists {
			return fmt.Errorf("control step %q target %q is not a prior background step", step.ID, target)
		}
	}
	return nil
}

// VisitSites walks every workflow-authored expression-bearing field once in
// execution order. Action.VisitSites owns action-authored sites.
// Validation and ordinary-secret inventory deliberately use this exhaustive
// walk rather than following runtime guards.
func (p Program) VisitSites(visit func(Site) error) error {
	derived := cloneProgram(p)
	return derived.walkWorkflowSites(func(site *Site) error {
		if site.Source == "" {
			return nil
		}
		return visit(*site)
	})
}

// TransformSites returns a deep copy with every expression site transformed
// once. The source program remains immutable.
func (p Program) TransformSites(transform func(Site) (Site, error)) (Program, error) {
	result := cloneProgram(p)
	err := result.walkSites(func(site *Site) error {
		if site.Source == "" {
			return nil
		}
		transformed, err := transform(*site)
		if err != nil {
			return err
		}
		*site = transformed
		return nil
	})
	return result, err
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
