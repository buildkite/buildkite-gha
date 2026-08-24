package compiler

import (
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/program"
)

func lowerActionProgram(node *actionNode) program.Action {
	location := program.Location{File: node.metadata.SourcePath, Field: "action"}
	action := program.Action{Name: node.metadata.Name, Source: node.lock.Source, Location: location}
	for _, name := range sortedKeys(node.metadata.Inputs) {
		definition := node.metadata.Inputs[name]
		input := program.ActionInput{Name: name, Required: definition.Required}
		if definition.Default != nil {
			site := actionSite(*definition.Default, program.SurfaceActionInputDefault, location, "action.inputs."+name+".default")
			input.Default = &site
		}
		action.Inputs = append(action.Inputs, input)
	}
	if node.native {
		action.Runtime = program.ActionRuntimeNative
		return action
	}
	switch node.runtime {
	case metadata.RuntimeNode16, metadata.RuntimeNode24:
		action.Runtime = program.ActionRuntimeJavaScript
		nodeMajor := 24
		if node.runtime == metadata.RuntimeNode16 {
			nodeMajor = 16
		}
		action.JavaScript = &program.JavaScriptAction{
			NodeMajor:     nodeMajor,
			Pre:           node.metadata.Runs.Pre,
			PreCondition:  actionSite(node.metadata.Runs.PreIf, program.SurfaceActionLifecycle, location, "action.runs.pre-if"),
			Main:          node.metadata.Runs.Main,
			Post:          node.metadata.Runs.Post,
			PostCondition: actionSite(node.metadata.Runs.PostIf, program.SurfaceActionLifecycle, location, "action.runs.post-if"),
		}
	case metadata.RuntimeComposite:
		action.Runtime = program.ActionRuntimeComposite
		action.Composite = &program.CompositeAction{Steps: make([]program.CompositeStep, len(node.metadata.Runs.Steps))}
		for i, step := range node.metadata.Runs.Steps {
			field := fmt.Sprintf("action.runs.steps[%d]", i)
			lowered := program.CompositeStep{
				ID:              step.ID,
				Name:            actionSite(step.Name, program.SurfaceCompositeTemplate, location, field+".name"),
				Condition:       actionSite(step.If, program.SurfaceStepCondition, location, field+".if"),
				ContinueOnError: step.ContinueOnError,
				Env:             actionBindings(step.Env, program.SurfaceCompositeTemplate, location, field+".env"),
			}
			if step.Run != "" {
				lowered.Run = &program.Run{
					Command:          actionSite(step.Run, program.SurfaceCompositeTemplate, location, field+".run"),
					Shell:            actionSite(step.Shell, program.SurfaceCompositeTemplate, location, field+".shell"),
					WorkingDirectory: actionSite(step.WorkingDirectory, program.SurfaceCompositeTemplate, location, field+".working-directory"),
				}
			}
			if step.Uses != "" {
				lock := ""
				if selector, ok := node.lock.Children[step.Uses]; ok {
					lock = selector.Lock
				}
				lowered.Invocation = &program.Invocation{
					Uses: actionSite(step.Uses, program.SurfaceRuntimeTemplate, location, field+".uses"),
					With: actionBindings(step.With, program.SurfaceCompositeTemplate, location, field+".with"),
					Lock: lock,
				}
			}
			action.Composite.Steps[i] = lowered
		}
		for _, name := range sortedKeys(node.metadata.Outputs) {
			action.Outputs = append(action.Outputs, program.Binding{
				Name:  name,
				Value: actionSite(node.metadata.Outputs[name].Value, program.SurfaceCompositeTemplate, location, "action.outputs."+name),
			})
		}
	case metadata.RuntimeDocker:
		action.Runtime = program.ActionRuntimeDocker
		image, _ := metadata.DockerImageReference(node.metadata.Runs.Image)
		action.Docker = &program.DockerAction{
			Image:      image,
			Entrypoint: node.metadata.Runs.Entrypoint,
			Arguments:  make([]program.Site, len(node.metadata.Runs.Args)),
			Env:        actionBindings(node.metadata.Runs.Env, program.SurfaceRuntimeTemplate, location, "action.runs.env"),
		}
		for i, argument := range node.metadata.Runs.Args {
			action.Docker.Arguments[i] = actionSite(argument, program.SurfaceDockerArgument, location, fmt.Sprintf("action.runs.args[%d]", i))
		}
	}
	return action
}

func actionBindings(values map[string]string, surface program.Surface, location program.Location, field string) []program.Binding {
	if values == nil {
		return nil
	}
	bindings := make([]program.Binding, 0, len(values))
	for _, name := range sortedKeys(values) {
		bindings = append(bindings, program.Binding{Name: name, Value: actionSite(values[name], surface, location, field+"."+name)})
	}
	return bindings
}

func actionSite(source string, surface program.Surface, location program.Location, field string) program.Site {
	location.Field = field
	result := program.ResultString
	if surface == program.SurfaceStepCondition || surface == program.SurfaceActionLifecycle {
		result = program.ResultBoolean
	}
	return program.Site{Source: source, Surface: surface, Result: result, Provenance: program.ProvenanceAction, Purpose: program.PurposeExpression, Location: location}
}
