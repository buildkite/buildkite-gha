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
	Secrets     []string
	GitHubToken bool
}

// InventoryAuthority validates and inventories every compiler-owned site in
// the normalized program. Ordinary secrets remain exhaustive for compatibility.
func InventoryAuthority(workflow Program, options AuthorityOptions) (Authority, error) {
	found := map[string]struct{}{}
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
	engine := expression.NewEngine()
	analyze := func(site Site) (expression.Analysis, error) {
		return engine.Analyze(site.expressionSite(), options.Values)
	}
	for _, guard := range workflow.Job.Guards {
		analysis, err := analyze(guard.Condition)
		if err != nil {
			return Authority{}, err
		}
		authority.GitHubToken = authority.GitHubToken || grantsToken(analysis.Effects.GitHubToken)
		if analysis.Value.Known && !truthy(analysis.Value.Value) {
			return finishAuthority(found, authority), nil
		}
	}
	jobCondition, err := analyze(workflow.Job.Condition)
	if err != nil {
		return Authority{}, err
	}
	authority.GitHubToken = authority.GitHubToken || grantsToken(jobCondition.Effects.GitHubToken)
	if jobCondition.Value.Known && !truthy(jobCondition.Value.Value) {
		return finishAuthority(found, authority), nil
	}
	for _, step := range workflow.Job.Steps {
		condition, err := analyze(step.Condition)
		if err != nil {
			return Authority{}, err
		}
		authority.GitHubToken = authority.GitHubToken || grantsToken(condition.Effects.GitHubToken)
		if condition.Value.Known && !truthy(condition.Value.Value) {
			continue
		}
		stepProgram := Program{Version: Version, Job: Job{Steps: []Step{step}}}
		err = stepProgram.VisitSites(func(site Site) error {
			if site.Surface == SurfaceStepCondition {
				return nil
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
		analysis, err := analyze(site)
		if err == nil {
			authority.GitHubToken = authority.GitHubToken || grantsToken(analysis.Effects.GitHubToken)
		}
		return err
	})
	if err != nil {
		return Authority{}, err
	}
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

func truthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case int:
		return value != 0
	case float64:
		return value != 0
	default:
		return true
	}
}
