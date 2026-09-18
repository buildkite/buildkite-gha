package compiler

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// runtimeSchedulingSites owns the scheduling-only expression positions. These
// values never enter the general compile context or runtime authority analysis.
func runtimeSchedulingSites(job workflow.Job) ([]expression.Site, error) {
	var sites []expression.Site
	if job.Concurrency != nil {
		site := expression.Site{Source: job.Concurrency.Group, Profile: expression.ProfileSchedulingGroup, Result: expression.ResultString}
		readsNeeds, err := expression.NewEngine().ReferencesContext(site, "needs", false)
		if err != nil {
			return nil, err
		}
		if readsNeeds {
			sites = append(sites, site)
		}
	}
	if job.MaxParallelExpression != nil {
		sites = append(sites, expression.Site{Source: job.MaxParallelExpression.Text, Profile: expression.ProfileSchedulingParallel, Result: expression.ResultNumber})
	}
	return sites, nil
}

// validateRuntimeScheduling admits only outputs of the matrix's already
// verified producer. It neither creates another continuation nor joins stages.
func (e *jobGraphExpansion) validateRuntimeScheduling(sourced sourcedJob, descriptor RuntimeMatrixDescriptor, deferred bool) (bool, error) {
	sites, err := runtimeSchedulingSites(sourced.Job)
	if err != nil || len(sites) == 0 {
		return false, err
	}
	if !deferred {
		message := "needs-derived scheduling requires a needs-derived matrix on the same job"
		if sourced.Concurrency != nil {
			message = "concurrency group cannot be resolved at compile time: " + message
		}
		return false, errors.New(message)
	}
	if err := rejectJobCancellation(sourced.path, sourced.Job); err != nil {
		return false, err
	}
	for _, job := range e.accepted {
		if len(job.concurrencyGates) != 0 {
			return false, errors.New("needs-derived scheduling cannot be combined with reusable-workflow concurrency gates")
		}
	}
	for _, site := range sites {
		if _, err := expression.NewEngine().Validate(site); err != nil {
			return false, err
		}
		references, err := expression.DeferredInputReferences(site.Source)
		if err != nil {
			return false, err
		}
		for _, reference := range references {
			binding, err := exactRuntimeMatrixNeed(sourced.needBindings, reference.Job)
			if err != nil {
				return false, err
			}
			if binding.projectOutputs || len(binding.members) != 1 || binding.members[0] != descriptor.ProducerJob {
				return false, errors.New("scheduling values must read direct outputs of the same ordinary job that produces the matrix")
			}
			if _, err := exactRuntimeMatrixOutput(e.topologyJobs[descriptor.ProducerJob].Outputs, reference.Output); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// resolveRuntimeScheduling evaluates only the admitted scheduling sites, using
// outputs from the producer manifest the continuation verified. The group is
// returned separately so output text is never parsed again as workflow syntax.
func resolveRuntimeScheduling(job workflow.Job, outputs map[string]string) (workflow.Job, *string, error) {
	sites, err := runtimeSchedulingSites(job)
	if err != nil {
		return job, nil, err
	}
	values := expression.Values{Runtime: expression.Context{Needs: make(map[string]expression.NeedStatus)}}
	for _, site := range sites {
		references, err := expression.DeferredInputReferences(site.Source)
		if err != nil {
			return job, nil, err
		}
		for _, reference := range references {
			if _, err := exactRuntimeMatrixOutput(outputs, reference.Output); err != nil {
				return job, nil, fmt.Errorf("scheduling output %q is unavailable or ambiguous", reference.Output)
			}
			values.Runtime.Needs[reference.Job] = expression.NeedStatus{Outputs: outputs, Result: "success"}
		}
	}
	var group *string
	for _, site := range sites {
		value, err := expression.NewEngine().Evaluate(site, values)
		if err != nil {
			// Outputs can contain sensitive data; do not echo evaluation errors.
			return job, nil, fmt.Errorf("%s could not be evaluated from the producer outputs", site.Profile)
		}
		switch site.Profile {
		case expression.ProfileSchedulingGroup:
			text := value.(string)
			if strings.TrimSpace(text) == "" {
				return job, nil, errors.New("concurrency group resolved to an empty string")
			}
			group = &text
		case expression.ProfileSchedulingParallel:
			number, ok := value.(float64)
			if !ok || math.IsNaN(number) || number < 1 || number > MaxRuntimeMatrixInstances || math.Trunc(number) != number {
				return job, nil, fmt.Errorf("needs-derived max-parallel must be an integer between 1 and %d", MaxRuntimeMatrixInstances)
			}
			limit := int(number)
			job.MaxParallel = &limit
			job.MaxParallelExpression = nil
		}
	}
	return job, group, nil
}
