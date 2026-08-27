package program

import (
	"fmt"
	"sort"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
)

const (
	SurfaceActionLifecycle    Surface = "action-lifecycle"
	SurfaceActionInputDefault Surface = "action-input-default"
	SurfaceDockerActionArg    Surface = "docker-action-arg"
)

type ActionInput struct {
	Name     string `json:"name"`
	Required bool   `json:"required,omitempty"`
	Default  *Site  `json:"default,omitempty"`
}

// VisitSites walks every action-authored expression site once in normalized
// execution order.
func (action Action) VisitSites(visit func(Site) error) error {
	for _, input := range action.Inputs {
		if input.Default != nil {
			if err := visitSite(*input.Default, visit); err != nil {
				return err
			}
		}
	}
	if err := visitSite(action.PreIf, visit); err != nil {
		return err
	}
	if err := visitBindings(action.Env, visit); err != nil {
		return err
	}
	for _, argument := range action.Args {
		if err := visitSite(argument, visit); err != nil {
			return err
		}
	}
	for _, step := range action.Steps {
		for _, site := range []Site{step.Name, step.Condition} {
			if err := visitSite(site, visit); err != nil {
				return err
			}
		}
		if err := visitBindings(step.Env, visit); err != nil {
			return err
		}
		if step.Run != nil {
			if err := visitSite(step.Run.Command, visit); err != nil {
				return err
			}
		}
		for _, site := range []Site{step.Shell, step.WorkingDirectory} {
			if err := visitSite(site, visit); err != nil {
				return err
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
	for _, output := range action.Outputs {
		if err := visitSite(output.Value, visit); err != nil {
			return err
		}
	}
	return visitSite(action.PostIf, visit)
}

func (action Action) transformSites(apply func(*Site) error) (Action, error) {
	result := cloneAction(action)
	for i := range result.Inputs {
		if result.Inputs[i].Default != nil {
			if err := apply(result.Inputs[i].Default); err != nil {
				return Action{}, err
			}
		}
	}
	if err := apply(&result.PreIf); err != nil {
		return Action{}, err
	}
	if err := transformBindings(result.Env, apply); err != nil {
		return Action{}, err
	}
	if err := transformSites(result.Args, apply); err != nil {
		return Action{}, err
	}
	for i := range result.Steps {
		step := &result.Steps[i]
		for _, site := range []*Site{&step.Name, &step.Condition} {
			if err := apply(site); err != nil {
				return Action{}, err
			}
		}
		if err := transformBindings(step.Env, apply); err != nil {
			return Action{}, err
		}
		if step.Run != nil {
			if err := apply(&step.Run.Command); err != nil {
				return Action{}, err
			}
		}
		for _, site := range []*Site{&step.Shell, &step.WorkingDirectory} {
			if err := apply(site); err != nil {
				return Action{}, err
			}
		}
		if step.Invocation != nil {
			if err := apply(&step.Invocation.Uses); err != nil {
				return Action{}, err
			}
			if err := transformBindings(step.Invocation.With, apply); err != nil {
				return Action{}, err
			}
		}
	}
	for i := range result.Outputs {
		if err := apply(&result.Outputs[i].Value); err != nil {
			return Action{}, err
		}
	}
	if err := apply(&result.PostIf); err != nil {
		return Action{}, err
	}
	return result, nil
}

func cloneAction(source Action) Action {
	result := source
	result.Inputs = append([]ActionInput(nil), source.Inputs...)
	for i := range result.Inputs {
		if result.Inputs[i].Default != nil {
			value := *result.Inputs[i].Default
			result.Inputs[i].Default = &value
		}
	}
	result.Outputs = append([]ActionOutput(nil), source.Outputs...)
	result.Args = append([]Site(nil), source.Args...)
	result.Env = cloneBindings(source.Env)
	result.Steps = append([]ActionStep(nil), source.Steps...)
	for i := range result.Steps {
		step := &result.Steps[i]
		step.Env = cloneBindings(step.Env)
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

type ActionOutput struct {
	Name  string `json:"name"`
	Value Site   `json:"value"`
}

// Action is the compiler-owned execution model for one immutable action lock.
// Entrypoints remain literal source-relative paths; every expression is a Site.
type Action struct {
	Name       string         `json:"name,omitempty"`
	Runtime    string         `json:"runtime"`
	Inputs     []ActionInput  `json:"inputs,omitempty"`
	Outputs    []ActionOutput `json:"outputs,omitempty"`
	Pre        string         `json:"pre,omitempty"`
	PreIf      Site           `json:"pre_if"`
	Main       string         `json:"main,omitempty"`
	Post       string         `json:"post,omitempty"`
	PostIf     Site           `json:"post_if"`
	Image      string         `json:"image,omitempty"`
	Entrypoint string         `json:"entrypoint,omitempty"`
	Args       []Site         `json:"args,omitempty"`
	Env        []Binding      `json:"env,omitempty"`
	Steps      []ActionStep   `json:"steps,omitempty"`
}

type ActionStep struct {
	ID               string      `json:"id,omitempty"`
	Name             Site        `json:"name"`
	Run              *ActionRun  `json:"run,omitempty"`
	Invocation       *Invocation `json:"invocation,omitempty"`
	Shell            Site        `json:"shell"`
	WorkingDirectory Site        `json:"working_directory"`
	Env              []Binding   `json:"env,omitempty"`
	Condition        Site        `json:"condition"`
	ContinueOnError  bool        `json:"continue_on_error,omitempty"`
}

type ActionRun struct {
	Command Site `json:"command"`
}

// ActionFromMetadata lowers verified action metadata into the normalized
// execution model. runtime is the compiler-selected runtime and children maps
// composite uses values to immutable action lock IDs.
func ActionFromMetadata(source metadata.Metadata, runtime string, children map[string]string) Action {
	result := Action{
		Name: source.Name, Runtime: runtime, Pre: source.Runs.Pre,
		PreIf: actionMetadataSite(source.Runs.PreIf, SurfaceActionLifecycle, ResultBoolean, "runs.pre-if", PurposeExpression),
		Main:  source.Runs.Main, Post: source.Runs.Post,
		PostIf:     actionMetadataSite(source.Runs.PostIf, SurfaceActionLifecycle, ResultBoolean, "runs.post-if", PurposeExpression),
		Image:      source.Runs.Image,
		Entrypoint: source.Runs.Entrypoint,
		Env:        actionMetadataBindings(source.Runs.Env, SurfaceRuntimeTemplate, "runs.env", PurposeExpression),
	}
	for _, name := range sortedMetadataKeys(source.Inputs) {
		input := source.Inputs[name]
		lowered := ActionInput{Name: name, Required: input.Required}
		if input.Default != nil {
			value := actionMetadataSite(*input.Default, SurfaceActionInputDefault, ResultString, "inputs."+name+".default", PurposeExpression)
			lowered.Default = &value
		}
		result.Inputs = append(result.Inputs, lowered)
	}
	for _, name := range sortedMetadataKeys(source.Outputs) {
		result.Outputs = append(result.Outputs, ActionOutput{Name: name, Value: actionMetadataSite(source.Outputs[name].Value, SurfaceStepTemplate, ResultString, "outputs."+name+".value", PurposeExpression)})
	}
	for i, argument := range source.Runs.Args {
		result.Args = append(result.Args, actionMetadataSite(argument, SurfaceDockerActionArg, ResultString, fmt.Sprintf("runs.args[%d]", i), PurposeExpression))
	}
	result.Steps = make([]ActionStep, len(source.Runs.Steps))
	for i, step := range source.Runs.Steps {
		field := fmt.Sprintf("runs.steps[%d]", i)
		lowered := ActionStep{
			ID: step.ID, Name: actionMetadataSite(step.Name, SurfaceStepTemplate, ResultString, field+".name", PurposeExpression),
			Shell:            actionMetadataSite(step.Shell, SurfaceStepTemplate, ResultString, field+".shell", PurposeExpression),
			WorkingDirectory: actionMetadataSite(step.WorkingDirectory, SurfaceStepTemplate, ResultString, field+".working-directory", PurposeExpression),
			Env:              actionMetadataBindings(step.Env, SurfaceStepTemplate, field+".env", PurposeExpression),
			Condition:        actionMetadataSite(step.If, SurfaceStepCondition, ResultBoolean, field+".if", PurposeExpression),
			ContinueOnError:  step.ContinueOnError,
		}
		if step.Run != "" {
			lowered.Run = &ActionRun{Command: actionMetadataSite(step.Run, SurfaceStepTemplate, ResultString, field+".run", PurposeExpression)}
		}
		if step.Uses != "" {
			lowered.Invocation = &Invocation{
				Uses: actionMetadataSite(step.Uses, SurfaceRuntimeTemplate, ResultString, field+".uses", PurposeExpression),
				With: actionMetadataBindings(step.With, SurfaceStepTemplate, field+".with", PurposeCompositeActionInput),
				Lock: children[step.Uses],
			}
		}
		result.Steps[i] = lowered
	}
	return result
}

func actionMetadataSite(source string, surface Surface, result ResultType, field string, purpose Purpose) Site {
	return Site{Source: source, Surface: surface, Result: result, Provenance: ProvenanceAction, Purpose: purpose, Location: Location{Field: field}}
}

func actionMetadataBindings(values map[string]string, surface Surface, field string, purpose Purpose) []Binding {
	result := make([]Binding, 0, len(values))
	for _, name := range sortedMetadataKeys(values) {
		result = append(result, Binding{Name: name, Value: actionMetadataSite(values[name], surface, ResultString, field+"."+name, purpose)})
	}
	return result
}

func sortedMetadataKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Metadata projects the normalized action into the established process
// adapters. Runtime never reparses action metadata to build this value.
func (action Action) Metadata(path, sourceRoot string) metadata.Metadata {
	result := metadata.Metadata{Path: path, SourceRoot: sourceRoot, Name: action.Name, Inputs: map[string]metadata.Input{}, Outputs: map[string]metadata.Output{}}
	for _, input := range action.Inputs {
		var value *string
		if input.Default != nil {
			source := input.Default.Source
			value = &source
		}
		result.Inputs[input.Name] = metadata.Input{Required: input.Required, Default: value}
	}
	for _, output := range action.Outputs {
		result.Outputs[output.Name] = metadata.Output{Value: output.Value.Source}
	}
	result.Runs = metadata.Runs{
		Using: action.Runtime, Pre: action.Pre, PreIf: action.PreIf.Source,
		Main: action.Main, Post: action.Post, PostIf: action.PostIf.Source,
		Image: action.Image, Entrypoint: action.Entrypoint,
		Args: siteSourcesAction(action.Args), Env: bindingMapAction(action.Env),
	}
	result.Runs.Steps = make([]metadata.CompositeStep, len(action.Steps))
	for i, step := range action.Steps {
		child := metadata.CompositeStep{
			ID: step.ID, Name: step.Name.Source, Shell: step.Shell.Source,
			WorkingDirectory: step.WorkingDirectory.Source, Env: bindingMapAction(step.Env),
			If: step.Condition.Source, ContinueOnError: step.ContinueOnError,
		}
		if step.Run != nil {
			child.Run = step.Run.Command.Source
		}
		if step.Invocation != nil {
			child.Uses, child.With = step.Invocation.Uses.Source, bindingMapAction(step.Invocation.With)
		}
		result.Runs.Steps[i] = child
	}
	if len(result.Inputs) == 0 {
		result.Inputs = nil
	}
	if len(result.Outputs) == 0 {
		result.Outputs = nil
	}
	return result
}

func bindingMapAction(bindings []Binding) map[string]string {
	if len(bindings) == 0 {
		return nil
	}
	result := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		result[binding.Name] = binding.Value.Source
	}
	return result
}

func siteSourcesAction(sites []Site) []string {
	result := make([]string, len(sites))
	for i, site := range sites {
		result[i] = site.Source
	}
	return result
}

func SortedActionIDs(actions map[string]Action) []string {
	ids := make([]string, 0, len(actions))
	for id := range actions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
