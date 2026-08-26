package program

import (
	"sort"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

type AuthorityOptions struct {
	ActionInputsInspected bool
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
		if options.ActionInputsInspected && site.Purpose == PurposeActionInput {
			return nil
		}
		if site.Surface == SurfaceJobCondition || site.Surface == SurfaceStepCondition {
			return inventoryConditionAuthority(site.Source, found, &authority)
		}
		names, err := expression.SecretReferences(site.Source)
		if err != nil {
			return err
		}
		for _, name := range names {
			found[name] = struct{}{}
		}
		var referencesToken bool
		if site.Surface == SurfaceStepTemplate || site.Surface == SurfaceStepControl {
			referencesToken, err = expression.ReferencesStepGitHubToken(site.Source)
		} else {
			referencesToken, err = expression.ReferencesGitHubToken(site.Source)
		}
		if err != nil {
			return err
		}
		authority.GitHubToken = authority.GitHubToken || referencesToken
		return nil
	})
	if err != nil {
		return Authority{}, err
	}
	authority.Secrets = make([]string, 0, len(found))
	for name := range found {
		authority.Secrets = append(authority.Secrets, name)
	}
	sort.Strings(authority.Secrets)
	return authority, nil
}

func inventoryConditionAuthority(source string, found map[string]struct{}, authority *Authority) error {
	names, err := expression.ConditionSecretReferences(source)
	if err != nil {
		return err
	}
	for _, name := range names {
		found[name] = struct{}{}
	}
	referencesToken, err := expression.ConditionReferencesGitHubToken(source)
	if err != nil {
		return err
	}
	authority.GitHubToken = authority.GitHubToken || referencesToken
	return nil
}
