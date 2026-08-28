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
	copy := cloneAction(action)
	return walkActionSites(&copy, func(site *Site) error {
		if site.Source == "" {
			return nil
		}
		return visit(*site)
	})
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
	Name           string         `json:"name,omitempty"`
	Runtime        string         `json:"runtime"`
	Inputs         []ActionInput  `json:"inputs,omitempty"`
	Outputs        []ActionOutput `json:"outputs,omitempty"`
	Pre            string         `json:"pre,omitempty"`
	PreIf          Site           `json:"pre_if"`
	Main           string         `json:"main,omitempty"`
	Post           string         `json:"post,omitempty"`
	PostIf         Site           `json:"post_if"`
	Image          string         `json:"image,omitempty"`
	Entrypoint     string         `json:"entrypoint,omitempty"`
	PreEntrypoint  string         `json:"pre_entrypoint,omitempty"`
	PostEntrypoint string         `json:"post_entrypoint,omitempty"`
	Args           []Site         `json:"args,omitempty"`
	Env            []Binding      `json:"env,omitempty"`
	Steps          []ActionStep   `json:"steps,omitempty"`
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
		PreIf: actionMetadataSite(source.Runs.PreIf, "runs.pre-if"),
		Main:  source.Runs.Main, Post: source.Runs.Post,
		PostIf:         actionMetadataSite(source.Runs.PostIf, "runs.post-if"),
		Image:          source.Runs.Image,
		Entrypoint:     source.Runs.Entrypoint,
		PreEntrypoint:  source.Runs.PreEntrypoint,
		PostEntrypoint: source.Runs.PostEntrypoint,
		Env:            actionMetadataBindings(source.Runs.Env, "runs.env"),
	}
	for _, name := range sortedMetadataKeys(source.Inputs) {
		input := source.Inputs[name]
		lowered := ActionInput{Name: name, Required: input.Required}
		if input.Default != nil {
			value := actionMetadataSite(*input.Default, "inputs."+name+".default")
			lowered.Default = &value
		}
		result.Inputs = append(result.Inputs, lowered)
	}
	for _, name := range sortedMetadataKeys(source.Outputs) {
		result.Outputs = append(result.Outputs, ActionOutput{Name: name, Value: actionMetadataSite(source.Outputs[name].Value, "outputs."+name+".value")})
	}
	for i, argument := range source.Runs.Args {
		result.Args = append(result.Args, actionMetadataSite(argument, fmt.Sprintf("runs.args[%d]", i)))
	}
	result.Steps = make([]ActionStep, len(source.Runs.Steps))
	for i, step := range source.Runs.Steps {
		field := fmt.Sprintf("runs.steps[%d]", i)
		lowered := ActionStep{
			ID: step.ID, Name: actionMetadataSite(step.Name, field+".name"),
			Shell:            actionMetadataSite(step.Shell, field+".shell"),
			WorkingDirectory: actionMetadataSite(step.WorkingDirectory, field+".working-directory"),
			Env:              actionMetadataBindings(step.Env, field+".env"),
			Condition:        actionMetadataSite(step.If, field+".if"),
			ContinueOnError:  step.ContinueOnError,
		}
		if step.Run != "" {
			lowered.Run = &ActionRun{Command: actionMetadataSite(step.Run, field+".run")}
		}
		if step.Uses != "" {
			lowered.Invocation = &Invocation{
				Uses: actionMetadataSite(step.Uses, field+".uses"),
				With: actionMetadataBindings(step.With, field+".with"),
				Lock: children[step.Uses],
			}
		}
		result.Steps[i] = lowered
	}
	_ = walkActionSites(&result, func(*Site) error { return nil })
	return result
}

func actionMetadataSite(source, field string) Site {
	return Site{Source: source, Location: Location{Field: field}}
}

func actionMetadataBindings(values map[string]string, field string) []Binding {
	result := make([]Binding, 0, len(values))
	for _, name := range sortedMetadataKeys(values) {
		result = append(result, Binding{Name: name, Value: actionMetadataSite(values[name], field+"."+name)})
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
		PreEntrypoint: action.PreEntrypoint, PostEntrypoint: action.PostEntrypoint,
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
