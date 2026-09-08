package compiler

import (
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

// These functions own the graph IR to immutable plan boundary for prerequisite
// identity, output projection, deferred inputs, and reusable call guards.

func buildPlanNeedSources(instance JobInstance, planDigests map[string]string) (map[string][]plan.NeedSource, error) {
	needSources := make(map[string][]plan.NeedSource, len(instance.NeedGroups))
	for _, logicalNeed := range sortedKeys(instance.NeedGroups) {
		dependencies := instance.NeedGroups[logicalNeed]
		if len(dependencies) > plan.MaxNeedProducers {
			return nil, fmt.Errorf("build plan for job %q: prerequisite %q has %d producers, maximum is %d", instance.LogicalJobID, logicalNeed, len(dependencies), plan.MaxNeedProducers)
		}
		for _, dependency := range dependencies {
			digest, ok := planDigests[dependency]
			if !ok {
				return nil, fmt.Errorf("build plan for job %q: prerequisite %q has no earlier plan digest", instance.LogicalJobID, dependency)
			}
			needSources[logicalNeed] = append(needSources[logicalNeed], plan.NeedSource{StepKey: dependency, PlanDigest: digest})
		}
	}
	return needSources, nil
}

func buildPlanNeedOutputs(instance JobInstance) map[string][]plan.NeedOutput {
	if len(instance.NeedOutputs) == 0 {
		return nil
	}
	needOutputs := make(map[string][]plan.NeedOutput, len(instance.NeedOutputs))
	for _, logicalNeed := range sortedKeys(instance.NeedOutputs) {
		outputs := instance.NeedOutputs[logicalNeed]
		needOutputs[logicalNeed] = make([]plan.NeedOutput, len(outputs))
		copy(needOutputs[logicalNeed], outputs)
	}
	return needOutputs
}

func buildPlanDeferredInputs(inputs map[string]DeferredInput, planDigests map[string]string) (map[string]plan.DeferredInput, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	resolved := make(map[string]plan.DeferredInput, len(inputs))
	for _, name := range sortedKeys(inputs) {
		input := inputs[name]
		sources, err := buildPlanLogicalNeedSources(input.NeedGroups, planDigests)
		if err != nil {
			return nil, fmt.Errorf("deferred input %q: %w", name, err)
		}
		resolved[name] = plan.DeferredInput{Template: input.Template, NeedSources: sources, NeedOutputs: buildPlanLogicalNeedOutputs(input.NeedOutputs)}
	}
	return resolved, nil
}

// buildPlanLogicalNeedSources binds each logical prerequisite's expanded
// producers to the plan digests they were compiled with.
func buildPlanLogicalNeedSources(groups map[string][]string, planDigests map[string]string) (map[string][]plan.NeedSource, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	sources := make(map[string][]plan.NeedSource, len(groups))
	for _, logicalNeed := range sortedKeys(groups) {
		for _, dependency := range groups[logicalNeed] {
			digest, ok := planDigests[dependency]
			if !ok {
				return nil, fmt.Errorf("prerequisite %q has no earlier plan digest", dependency)
			}
			sources[logicalNeed] = append(sources[logicalNeed], plan.NeedSource{StepKey: dependency, PlanDigest: digest})
		}
	}
	return sources, nil
}

func buildPlanLogicalNeedOutputs(outputs map[string][]NeedOutput) map[string][]plan.NeedOutput {
	if len(outputs) == 0 {
		return nil
	}
	projected := make(map[string][]plan.NeedOutput, len(outputs))
	for _, logicalNeed := range sortedKeys(outputs) {
		if selected := outputs[logicalNeed]; len(selected) != 0 {
			projected[logicalNeed] = append(projected[logicalNeed], selected...)
		}
	}
	return projected
}

func buildPlanCallGuards(instance JobInstance, planDigests map[string]string) ([]plan.CallGuard, error) {
	callGuards := make([]plan.CallGuard, len(instance.CallGuards))
	for guardIndex, guard := range instance.CallGuards {
		deferredInputs, err := buildPlanDeferredInputs(guard.DeferredInputs, planDigests)
		if err != nil {
			return nil, fmt.Errorf("build plan for job %q call guard %d: %w", instance.LogicalJobID, guardIndex+1, err)
		}
		needSources, err := buildPlanLogicalNeedSources(guard.NeedGroups, planDigests)
		if err != nil {
			return nil, fmt.Errorf("build plan for job %q: call guard %w", instance.LogicalJobID, err)
		}
		planGuard := plan.CallGuard{Condition: guard.Condition, Inputs: cloneAnyMap(guard.Inputs), DeferredInputs: deferredInputs, NeedSources: needSources, NeedOutputs: buildPlanLogicalNeedOutputs(guard.NeedOutputs)}
		callGuards[guardIndex] = planGuard
	}
	return callGuards, nil
}
