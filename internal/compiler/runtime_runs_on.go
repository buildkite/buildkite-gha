package compiler

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	RuntimeRunsOnSchemaV1 = "https://buildkite.com/schemas/buildkite-gha/runtime-runs-on-v1.schema.json"
	RuntimeRunsOnShape    = "runs-on"
	MaxRuntimeRunsOnBytes = 1024
)

func (descriptor RuntimeOutputDescriptor) Kind() string {
	if descriptor.Shape == RuntimeRunsOnShape {
		return RuntimeRunsOnShape
	}
	return "matrix"
}

// runsOnSites defines the scheduling expression positions. Only these sites
// receive the verified output; job conditions and credential authority keep
// their original contexts and reusable-input provenance.
func runsOnSites(job workflow.Job) []expression.Site {
	if job.RunsOnExpr != nil {
		return []expression.Site{{Source: job.RunsOnExpr.Text, Profile: expression.ProfileRunsOn, Result: expression.ResultAny}}
	}
	sites := make([]expression.Site, len(job.RunsOn))
	for i, label := range job.RunsOn {
		sites[i] = expression.Site{Source: label, Profile: expression.ProfileRunsOnTemplate, Result: expression.ResultString}
	}
	return sites
}

func hasRuntimeRunsOn(sourced sourcedJob) bool {
	for _, site := range runsOnSites(sourced.Job) {
		if usesNeeds(site.Source) || referencesDeferredInput(site.Source, sourced.inputs.deferred) {
			return true
		}
	}
	return false
}

// describeRuntimeRunsOn proves that all scheduling references select the same
// output of one static producer. Caller inputs are bound in their own needs
// scope; they never make caller outputs visible in the callee's needs scope.
// When supplied, output is data in a scheduling-only context, not source to
// substitute into the workflow or an input to authority planning.
func describeRuntimeRunsOn(sourced sourcedJob, context expression.CompileContext, jobs map[string]workflow.Job, matrices map[string][]map[string]any, output *string) (RuntimeOutputDescriptor, expression.CompileContext, error) {
	var descriptor RuntimeOutputDescriptor
	if sourced.Matrix != nil {
		return descriptor, context, errors.New("job-output-derived runs-on cannot be combined with a matrix on the same job")
	}
	if output != nil && (len(*output) > MaxRuntimeRunsOnBytes || !utf8.ValidString(*output)) {
		return descriptor, context, errors.New("runs-on producer output must be valid UTF-8 and at most 1024 bytes")
	}
	span := sourced.Span
	if expr := sourced.RunsOnExpr; expr != nil {
		span = workflow.Span{Start: workflow.Position{Line: expr.Span.Start.Line, Column: expr.Span.Start.Column}, End: workflow.Position{Line: expr.Span.End.Line, Column: expr.Span.End.Column}}
	}
	bind := func(references []expression.NeedOutputReference, needs map[string]needBinding, values map[string]any) error {
		for _, reference := range references {
			bound, err := describeRuntimeOutput(sourced.ID, sourced.path, sourced.digest, span, RuntimeRunsOnShape, reference, needs, jobs, matrices)
			if err != nil {
				return errors.New(strings.ReplaceAll(err.Error(), "runtime matrix", "runs-on"))
			}
			if descriptor.Job != "" && (bound.ProducerJob != descriptor.ProducerJob || bound.ProducerOutput != descriptor.ProducerOutput) {
				return errors.New("runs-on must derive from one output of one statically expanded producer")
			}
			descriptor = bound
			if output != nil {
				name := strings.ToLower(reference.Job)
				if values[name] == nil {
					values[name] = map[string]any{"outputs": make(map[string]any)}
				}
				values[name].(map[string]any)["outputs"].(map[string]any)[strings.ToLower(reference.Output)] = *output
			}
		}
		return nil
	}
	context.Inputs = cloneAnyMap(sourced.inputs.values)
	context.InputsComplete = len(sourced.inputs.deferred) == 0
	context.Needs = make(map[string]any)
	inputs := make(map[string]bool)
	engine := expression.NewEngine()
	for _, site := range runsOnSites(sourced.Job) {
		references, err := engine.NeedOutputs(site)
		if err != nil {
			return descriptor, context, err
		}
		if err := bind(references, sourced.needBindings, context.Needs); err != nil {
			return descriptor, context, err
		}
		for name := range sourced.inputs.deferred {
			if referencesInput(site.Source, name) {
				inputs[name] = true
			}
		}
	}
	for _, name := range sortedKeys(inputs) {
		input := sourced.inputs.deferred[name]
		references, err := expression.DeferredInputReferences(input.template)
		if err != nil {
			return descriptor, context, err
		}
		values := make(map[string]any)
		if err := bind(references, input.needs, values); err != nil {
			return descriptor, context, err
		}
		if output != nil {
			value, err := engine.Evaluate(expression.Site{Source: input.template, Profile: expression.ProfileRunsOnTemplate, Result: expression.ResultString}, expression.Values{Compile: expression.CompileContext{Needs: values}})
			if err != nil {
				return descriptor, context, fmt.Errorf("resolve scheduling input %q: %w", name, err)
			}
			if context.Inputs == nil {
				context.Inputs = make(map[string]any)
			}
			context.Inputs[name] = value
		}
	}
	if descriptor.Job == "" {
		return descriptor, context, errors.New("runs-on must reference a statically named job output")
	}
	if output != nil {
		context.InputsComplete = len(inputs) == len(sourced.inputs.deferred)
	}
	return descriptor, context, nil
}

func (e *jobGraphExpansion) rejectRuntimeRunsOn(sourced sourcedJob, err error) {
	position := runsOnPosition(sourced.Job)
	e.diagnostics = append(e.diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", sourced.path, position.Line, position.Column, sourced.ID, "", "", 0, locatedJobError(sourced.path, sourced.Job, position.Line, position.Column, err.Error())))
	e.failedJobs[sourced.ID] = true
	e.failedMatrices[sourced.ID] = true
}
