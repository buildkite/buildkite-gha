package program

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
)

type siteSemantics struct {
	surface    Surface
	result     ResultType
	provenance Provenance
	purpose    Purpose
}

func workflowSemantics(surface Surface, result ResultType, purpose Purpose) siteSemantics {
	return siteSemantics{surface: surface, result: result, provenance: ProvenanceWorkflow, purpose: purpose}
}

func actionSemantics(surface Surface, result ResultType, purpose Purpose) siteSemantics {
	return siteSemantics{surface: surface, result: result, provenance: ProvenanceAction, purpose: purpose}
}

func applySiteSemantics(site *Site, semantics siteSemantics) {
	site.Surface = semantics.surface
	site.Result = semantics.result
	site.Provenance = semantics.provenance
	site.Purpose = semantics.purpose
}

func walkOne(site *Site, semantics siteSemantics, visit func(*Site) error) error {
	applySiteSemantics(site, semantics)
	if err := visit(site); err != nil {
		return err
	}
	applySiteSemantics(site, semantics)
	return nil
}

func walkBindings(bindings []Binding, semantics siteSemantics, visit func(*Site) error) error {
	for i := range bindings {
		if err := walkOne(&bindings[i].Value, semantics, visit); err != nil {
			return err
		}
	}
	return nil
}

func walkSlice(sites []Site, semantics siteSemantics, visit func(*Site) error) error {
	for i := range sites {
		if err := walkOne(&sites[i], semantics, visit); err != nil {
			return err
		}
	}
	return nil
}

// walkSites is the source of truth for every expression-bearing position in a
// normalized program. Positional semantics are derived here rather than
// accepted as claims from the wire format.
func (p *Program) walkSites(visit func(*Site) error) error {
	if err := p.walkWorkflowSites(visit); err != nil {
		return err
	}
	for _, id := range SortedActionIDs(p.Actions) {
		action := p.Actions[id]
		if err := walkActionSites(&action, visit); err != nil {
			return err
		}
		p.Actions[id] = action
	}
	return nil
}

// DeriveSiteSemantics populates the in-memory expression policy selected by
// each structural position. Constructors and decoders call this after filling
// source text and locations.
func (p *Program) DeriveSiteSemantics() {
	_ = p.walkSites(func(*Site) error { return nil })
}

func (p *Program) walkWorkflowSites(visit func(*Site) error) error {
	w := func(site *Site, surface Surface, result ResultType, purpose Purpose) error {
		return walkOne(site, workflowSemantics(surface, result, purpose), visit)
	}
	bindings := func(values []Binding, surface Surface, purpose Purpose) error {
		return walkBindings(values, workflowSemantics(surface, ResultString, purpose), visit)
	}
	job := &p.Job
	for i := range job.Guards {
		if err := w(&job.Guards[i].Condition, SurfaceCallCondition, ResultBoolean, PurposeExpression); err != nil {
			return err
		}
	}
	if err := w(&job.Condition, SurfaceJobCondition, ResultBoolean, PurposeExpression); err != nil {
		return err
	}
	if err := bindings(job.Env, SurfaceJobEnvironment, PurposeExpression); err != nil {
		return err
	}
	if err := w(&job.Defaults.Shell, SurfaceJobDefault, ResultString, PurposeExpression); err != nil {
		return err
	}
	if err := w(&job.Defaults.WorkingDirectory, SurfaceJobDefault, ResultString, PurposeExpression); err != nil {
		return err
	}
	if job.Container != nil {
		if err := w(&job.Container.Image, SurfaceRuntimeTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
		if err := bindings(job.Container.Env, SurfaceRuntimeTemplate, PurposeExpression); err != nil {
			return err
		}
		if err := walkSlice(job.Container.Ports, workflowSemantics(SurfaceRuntimeTemplate, ResultString, PurposeExpression), visit); err != nil {
			return err
		}
	}
	for i := range job.Services.Static {
		container := &job.Services.Static[i].Container
		if err := w(&container.Image, SurfaceServiceTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
		if container.Credentials != nil {
			if err := w(&container.Credentials.Username, SurfaceServiceCredential, ResultString, PurposeExpression); err != nil {
				return err
			}
			if err := w(&container.Credentials.Password, SurfaceServiceCredential, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
		if err := bindings(container.Env, SurfaceServiceTemplate, PurposeExpression); err != nil {
			return err
		}
		if err := walkSlice(container.Ports, workflowSemantics(SurfaceServiceTemplate, ResultString, PurposeExpression), visit); err != nil {
			return err
		}
		if err := walkSlice(container.Volumes, workflowSemantics(SurfaceServiceTemplate, ResultString, PurposeExpression), visit); err != nil {
			return err
		}
		for _, site := range []*Site{&container.Options, &container.Command, &container.Entrypoint} {
			if err := w(site, SurfaceServiceTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
	}
	if job.Services.Dynamic != nil {
		if err := w(job.Services.Dynamic, SurfaceServiceMap, ResultObject, PurposeExpression); err != nil {
			return err
		}
	}
	for i := range job.Steps {
		step := &job.Steps[i]
		if err := bindings(step.Env, SurfaceStepTemplate, PurposeExpression); err != nil {
			return err
		}
		if err := w(&step.Condition, SurfaceStepCondition, ResultBoolean, PurposeExpression); err != nil {
			return err
		}
		if step.ContinueOnError.Expression != nil {
			if err := w(step.ContinueOnError.Expression, SurfaceStepControl, ResultBoolean, PurposeExpression); err != nil {
				return err
			}
		}
		if step.TimeoutMinutes.Expression != nil {
			if err := w(step.TimeoutMinutes.Expression, SurfaceStepControl, ResultNumber, PurposeExpression); err != nil {
				return err
			}
		}
		if err := w(&step.Name, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
		if step.Run != nil {
			for _, site := range []*Site{&step.Run.Command, &step.Run.Shell, &step.Run.WorkingDirectory} {
				if err := w(site, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
					return err
				}
			}
		}
		if step.Invocation != nil {
			if err := w(&step.Invocation.Uses, SurfaceRuntimeTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
			if err := bindings(step.Invocation.With, SurfaceStepTemplate, PurposeActionInput); err != nil {
				return err
			}
		}
	}
	if err := bindings(job.Outputs, SurfaceJobOutput, PurposeExpression); err != nil {
		return err
	}
	return nil
}

func walkActionSites(action *Action, visit func(*Site) error) error {
	a := func(site *Site, surface Surface, result ResultType, purpose Purpose) error {
		return walkOne(site, actionSemantics(surface, result, purpose), visit)
	}
	bindings := func(values []Binding, surface Surface, purpose Purpose) error {
		return walkBindings(values, actionSemantics(surface, ResultString, purpose), visit)
	}
	for i := range action.Inputs {
		if action.Inputs[i].Default != nil {
			if err := a(action.Inputs[i].Default, SurfaceActionInputDefault, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
	}
	if err := a(&action.PreIf, SurfaceActionLifecycle, ResultBoolean, PurposeExpression); err != nil {
		return err
	}
	if err := bindings(action.Env, SurfaceRuntimeTemplate, PurposeExpression); err != nil {
		return err
	}
	if err := walkSlice(action.Args, actionSemantics(SurfaceDockerActionArg, ResultString, PurposeExpression), visit); err != nil {
		return err
	}
	for i := range action.Steps {
		step := &action.Steps[i]
		if err := a(&step.Name, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
		if err := a(&step.Condition, SurfaceStepCondition, ResultBoolean, PurposeExpression); err != nil {
			return err
		}
		if err := bindings(step.Env, SurfaceStepTemplate, PurposeExpression); err != nil {
			return err
		}
		if step.Run != nil {
			if err := a(&step.Run.Command, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
		}
		if err := a(&step.Shell, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
		if err := a(&step.WorkingDirectory, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
		if step.Invocation != nil {
			if err := a(&step.Invocation.Uses, SurfaceRuntimeTemplate, ResultString, PurposeExpression); err != nil {
				return err
			}
			if err := bindings(step.Invocation.With, SurfaceStepTemplate, PurposeCompositeActionInput); err != nil {
				return err
			}
		}
	}
	for i := range action.Outputs {
		if err := a(&action.Outputs[i].Value, SurfaceStepTemplate, ResultString, PurposeExpression); err != nil {
			return err
		}
	}
	return a(&action.PostIf, SurfaceActionLifecycle, ResultBoolean, PurposeExpression)
}

func validateActionStructure(action Action) error {
	switch action.Runtime {
	case "node16", "node24":
		if action.Main == "" {
			return fmt.Errorf("JavaScript action has no main entrypoint")
		}
		if len(action.Steps) != 0 || action.Image != "" || action.Entrypoint != "" || action.PreEntrypoint != "" || action.PostEntrypoint != "" || len(action.Args) != 0 {
			return fmt.Errorf("JavaScript action contains incompatible execution fields")
		}
	case "composite":
		if len(action.Steps) == 0 || action.Pre != "" || action.PreIf.Source != "" || action.Main != "" || action.Post != "" || action.PostIf.Source != "" || action.Image != "" || action.Entrypoint != "" || action.PreEntrypoint != "" || action.PostEntrypoint != "" || len(action.Args) != 0 || len(action.Env) != 0 {
			return fmt.Errorf("composite action contains incompatible execution fields")
		}
	case "docker":
		if action.Image == "" || action.Pre != "" || action.PreIf.Source != "" || action.Main != "" || action.Post != "" || action.PostIf.Source != "" || action.PreEntrypoint != "" || action.PostEntrypoint != "" || len(action.Steps) != 0 {
			return fmt.Errorf("docker action contains incompatible execution fields")
		}
	default:
		return &metadata.UnsupportedRuntimeError{Runtime: action.Runtime}
	}
	for _, step := range action.Steps {
		if step.Run != nil && step.Invocation != nil {
			return fmt.Errorf("composite step has both run and invocation")
		}
		if step.Run == nil && step.Invocation == nil {
			return fmt.Errorf("composite step has no operation")
		}
		if step.Invocation != nil && step.Invocation.Lock == "" {
			return fmt.Errorf("composite invocation has no action lock")
		}
	}
	return nil
}

// UnmarshalJSON restores positional semantics omitted from the wire format.
func (p *Program) UnmarshalJSON(data []byte) error {
	type wireProgram Program
	var decoded wireProgram
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*p = Program(decoded)
	return p.walkSites(func(*Site) error { return nil })
}
