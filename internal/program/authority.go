package program

import (
	"sort"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

type AuthorityOptions struct {
	ActionInputsInspected bool
	Values                expression.AbstractValues
}

// Authority is the workflow-authored secret inventory retained by the current
// plan contract.
type Authority struct {
	Secrets          []string
	ReachableSecrets []string
	GitHubToken      bool
}

// Reachability records the workflow steps that planning cannot prove are
// skipped. Unknown runtime conditions remain reachable.
type Reachability struct {
	Job   bool
	Steps []bool

	githubToken bool
}

// WorkflowReachability interprets structural guards using planning-known
// values. Callers retain exhaustive program validation independently.
func WorkflowReachability(workflow Program, values expression.AbstractValues) (Reachability, error) {
	result := Reachability{Steps: make([]bool, len(workflow.Job.Steps))}
	engine := expression.NewEngine()
	analyze := func(site Site) (expression.Analysis, error) {
		analysis, err := engine.Analyze(site.expressionSite(), values)
		if err == nil {
			result.githubToken = result.githubToken || grantsToken(analysis.Effects.GitHubToken)
		}
		return analysis, err
	}
	knownFalse := func(analysis expression.Analysis) bool {
		return analysis.Value.Known && !engine.Truthy(analysis.Value.Value)
	}
	for _, guard := range workflow.Job.Guards {
		analysis, err := analyze(guard.Condition)
		if err != nil {
			return Reachability{}, err
		}
		if knownFalse(analysis) {
			return result, nil
		}
	}
	job, err := analyze(workflow.Job.Condition)
	if err != nil {
		return Reachability{}, err
	}
	if knownFalse(job) {
		return result, nil
	}
	result.Job = true
	for i, step := range workflow.Job.Steps {
		condition, err := analyze(step.Condition)
		if err != nil {
			return Reachability{}, err
		}
		result.Steps[i] = !knownFalse(condition)
	}
	return result, nil
}

// InventoryAuthority validates and inventories every compiler-owned site in
// the normalized program. Ordinary secrets remain exhaustive for compatibility.
func InventoryAuthority(workflow Program, options AuthorityOptions) (Authority, error) {
	found := map[string]struct{}{}
	reachableSecrets := map[string]struct{}{}
	authority := Authority{}
	err := workflow.VisitSites(func(site Site) error {
		names, err := ValidateSite(site)
		if err != nil {
			return err
		}
		if !options.ActionInputsInspected || site.Purpose != PurposeActionInput {
			for _, name := range names {
				found[name] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		return Authority{}, err
	}
	// Authority follows structural guards after exhaustive validation and
	// ordinary-secret inventory. Known-false conditions narrow token effects;
	// unknown conditions conservatively retain them.
	reachability, err := WorkflowReachability(workflow, options.Values)
	if err != nil {
		return Authority{}, err
	}
	authority.GitHubToken = reachability.githubToken
	if !reachability.Job {
		return finishAuthority(found, authority), nil
	}
	engine := expression.NewEngine()
	analyze := func(site Site) (expression.Analysis, error) {
		return engine.Analyze(site.expressionSite(), options.Values)
	}
	for i, step := range workflow.Job.Steps {
		if !reachability.Steps[i] {
			continue
		}
		stepProgram := Program{Version: Version, Job: Job{Steps: []Step{step}}}
		err = stepProgram.VisitSites(func(site Site) error {
			if site.Surface == SurfaceStepCondition {
				return nil
			}
			names, err := ValidateSite(site)
			if err != nil {
				return err
			}
			for _, name := range names {
				reachableSecrets[name] = struct{}{}
			}
			analysis, err := analyze(site)
			if err == nil {
				authority.GitHubToken = authority.GitHubToken || grantsToken(analysis.Effects.GitHubToken)
			}
			return err
		})
		if err != nil {
			return Authority{}, err
		}
	}
	// Job setup and outputs are outside step guards.
	setup := workflow
	setup.Job.Guards, setup.Job.Condition, setup.Job.Steps = nil, Site{}, nil
	err = setup.VisitSites(func(site Site) error {
		names, err := ValidateSite(site)
		if err != nil {
			return err
		}
		for _, name := range names {
			reachableSecrets[name] = struct{}{}
		}
		analysis, err := analyze(site)
		if err == nil {
			authority.GitHubToken = authority.GitHubToken || grantsToken(analysis.Effects.GitHubToken)
		}
		return err
	})
	if err != nil {
		return Authority{}, err
	}
	authority.ReachableSecrets = sortedSet(reachableSecrets)
	return finishAuthority(found, authority), nil
}

func grantsToken(effect expression.GitHubTokenEffect) bool {
	return effect&(expression.GitHubTokenDirect|expression.GitHubTokenWorkflowContext) != 0
}

func finishAuthority(found map[string]struct{}, authority Authority) Authority {
	authority.Secrets = make([]string, 0, len(found))
	for name := range found {
		authority.Secrets = append(authority.Secrets, name)
	}
	sort.Strings(authority.Secrets)
	return authority
}
