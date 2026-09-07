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
		deferred := plan.DeferredInput{Outputs: make([]plan.NeedOutput, len(input.Outputs))}
		for _, source := range input.Sources {
			digest, ok := planDigests[source]
			if !ok {
				return nil, fmt.Errorf("deferred input %q source %q has no earlier plan digest", name, source)
			}
			deferred.Sources = append(deferred.Sources, plan.NeedSource{StepKey: source, PlanDigest: digest})
		}
		copy(deferred.Outputs, input.Outputs)
		resolved[name] = deferred
	}
	return resolved, nil
}

func buildPlanCallGuards(instance JobInstance, planDigests map[string]string) ([]plan.CallGuard, error) {
	callGuards := make([]plan.CallGuard, len(instance.CallGuards))
	for guardIndex, guard := range instance.CallGuards {
		deferredInputs, err := buildPlanDeferredInputs(guard.DeferredInputs, planDigests)
		if err != nil {
			return nil, fmt.Errorf("build plan for job %q call guard %d: %w", instance.LogicalJobID, guardIndex+1, err)
		}
		planGuard := plan.CallGuard{Condition: guard.Condition, Inputs: cloneAnyMap(guard.Inputs), DeferredInputs: deferredInputs}
		if len(guard.NeedGroups) != 0 {
			planGuard.NeedSources = make(map[string][]plan.NeedSource, len(guard.NeedGroups))
			for _, logicalNeed := range sortedKeys(guard.NeedGroups) {
				for _, dependency := range guard.NeedGroups[logicalNeed] {
					digest, ok := planDigests[dependency]
					if !ok {
						return nil, fmt.Errorf("build plan for job %q: call guard prerequisite %q has no earlier plan digest", instance.LogicalJobID, dependency)
					}
					planGuard.NeedSources[logicalNeed] = append(planGuard.NeedSources[logicalNeed], plan.NeedSource{StepKey: dependency, PlanDigest: digest})
				}
			}
		}
		if len(guard.NeedOutputs) != 0 {
			planGuard.NeedOutputs = make(map[string][]plan.NeedOutput, len(guard.NeedOutputs))
			for _, logicalNeed := range sortedKeys(guard.NeedOutputs) {
				outputs := guard.NeedOutputs[logicalNeed]
				if len(outputs) != 0 {
					planGuard.NeedOutputs[logicalNeed] = append(planGuard.NeedOutputs[logicalNeed], outputs...)
				}
			}
		}
		callGuards[guardIndex] = planGuard
	}
	return callGuards, nil
}
