package program

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
)

// ActionAuthority is the authority retained by one normalized action
// invocation, including its finite composite-child graph.
type ActionAuthority struct {
	Secrets      []string
	GitHubToken  bool
	EventPayload bool
}

type ActionAuthorityOptions struct {
	ServerURL string
}

// InventoryActionAuthority interprets action input resolution and nested
// composite invocations. Workflow-authored supplied inputs can grant
// authority; action-authored metadata cannot.
func InventoryActionAuthority(actions map[string]Action, root string, supplied []Binding, options ActionAuthorityOptions) (ActionAuthority, error) {
	found := map[string]struct{}{}
	authority := ActionAuthority{}
	engine := expression.NewEngine()
	active := map[string]bool{}

	var inspect func(string, []Binding, bool) error
	inspect = func(id string, supplied []Binding, workflowAuthored bool) error {
		action, ok := actions[id]
		if !ok {
			return fmt.Errorf("action program %q is missing", id)
		}
		if err := walkActionSites(&action, func(*Site) error { return nil }); err != nil {
			return err
		}
		if active[id] {
			return fmt.Errorf("action recursion detected at program %q", id)
		}
		active[id] = true
		defer delete(active, id)
		native := action.Runtime == ""

		// Event retention remains exhaustive across action-authored lifecycle and
		// operation fields. Those fields do not independently grant credentials.
		if !native {
			if err := action.VisitSites(func(site Site) error {
				// Defaults are interpreted below in declaration order with prior input
				// values. Docker arguments follow defaults to retain deterministic error
				// ownership.
				if site.Surface == SurfaceActionInputDefault || site.Surface == SurfaceDockerActionArg {
					return nil
				}
				analysis, err := engine.Analyze(site.expressionSite(), expression.AbstractValues{})
				if err == nil {
					authority.EventPayload = authority.EventPayload || analysis.Effects.EventPayload
				}
				return err
			}); err != nil {
				return err
			}
		}

		provided := make(map[string]Binding, len(supplied))
		for _, binding := range supplied {
			provided[strings.ToLower(binding.Name)] = binding
		}
		definitions := make(map[string]ActionInput, len(action.Inputs))
		for _, input := range action.Inputs {
			definitions[strings.ToLower(input.Name)] = input
		}
		known := map[string]any{
			"github.server_url": options.ServerURL,
			"job.check_run_id":  "",
		}
		for _, binding := range supplied {
			name := strings.ToLower(binding.Name)
			input, declared := definitions[name]
			names, err := ValidateSite(binding.Value)
			if err != nil {
				return fmt.Errorf("action input %q: %w", binding.Name, err)
			}
			analysis, err := engine.Analyze(binding.Value.expressionSite(), expression.AbstractValues{References: known})
			if err != nil {
				return fmt.Errorf("action input %q: %w", binding.Name, err)
			}
			authority.EventPayload = authority.EventPayload || analysis.Effects.EventPayload
			grants := grantsToken(analysis.Effects.GitHubToken)
			if !workflowAuthored && len(names) != 0 {
				return fmt.Errorf("action input %q: composite action metadata cannot grant secret authority", binding.Name)
			}
			if !workflowAuthored && grants {
				return fmt.Errorf("action input %q: composite action metadata cannot grant github.token authority", binding.Name)
			}
			if workflowAuthored {
				authority.GitHubToken = authority.GitHubToken || grants
				for _, secret := range names {
					if !declared || input.Required || strings.EqualFold(secret, "GITHUB_TOKEN") {
						found[secret] = struct{}{}
					}
				}
			}
			if analysis.Value.Known {
				known["inputs."+name] = analysis.Value.Value
			}
		}
		// Native adapters replace the admitted action's execution completely. Only
		// workflow-authored supplied inputs can therefore retain authority.
		if native {
			return nil
		}
		for _, input := range action.Inputs {
			name := strings.ToLower(input.Name)
			if _, exists := provided[name]; exists {
				continue
			}
			if input.Default == nil {
				continue
			}
			analysis, err := engine.Analyze(input.Default.expressionSite(), expression.AbstractValues{References: known})
			if err != nil {
				return fmt.Errorf("action input %q default: %w", input.Name, err)
			}
			authority.EventPayload = authority.EventPayload || analysis.Effects.EventPayload
			authority.GitHubToken = authority.GitHubToken || grantsToken(analysis.Effects.GitHubToken)
			if analysis.Value.Known {
				known["inputs."+name] = analysis.Value.Value
			}
		}
		for i, argument := range action.Args {
			if _, err := engine.Validate(argument.expressionSite()); err != nil {
				return fmt.Errorf("docker action argument %d: %w", i+1, err)
			}
		}

		if action.Runtime != "composite" {
			return nil
		}
		for i, step := range action.Steps {
			if step.Invocation == nil {
				continue
			}
			if err := inspect(step.Invocation.Lock, step.Invocation.With, false); err != nil {
				return fmt.Errorf("composite action step %d child %q: %w", i+1, step.Invocation.Uses.Source, err)
			}
		}
		return nil
	}

	if err := inspect(root, supplied, true); err != nil {
		return ActionAuthority{}, err
	}
	authority.Secrets = sortedSet(found)
	return authority, nil
}

func sortedSet(found map[string]struct{}) []string {
	result := make([]string, 0, len(found))
	for name := range found {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
