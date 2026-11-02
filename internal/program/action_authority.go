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
	ServerURL      string
	Strategy       map[string]any
	Matrix         map[string]any
	WorkflowInputs expression.InputScope
}

// InventoryActionAuthority interprets action input resolution and nested
// composite invocations. Effective input defaults and reachable composite shell
// templates can require the job-authorized token, but not ordinary secrets.
// It does not mutate actions.
func InventoryActionAuthority(actions map[string]Action, root string, supplied []Binding, options ActionAuthorityOptions) (ActionAuthority, error) {
	found := map[string]struct{}{}
	authority := ActionAuthority{}
	engine := expression.NewEngine()
	active := map[string]bool{}

	var inspect func(string, []Binding, bool, bool, expression.AbstractValues) error
	inspect = func(id string, supplied []Binding, workflowAuthored, reachable bool, caller expression.AbstractValues) error {
		action, ok := actions[id]
		if !ok {
			return fmt.Errorf("action program %q is missing", id)
		}
		if active[id] {
			return fmt.Errorf("action recursion detected at program %q", id)
		}
		active[id] = true
		defer delete(active, id)
		native := action.Runtime == ""
		action = action.Clone()

		// Event retention remains exhaustive across action-authored lifecycle and
		// operation fields. Those fields do not independently grant credentials.
		// Derive positional semantics on the private copy in the same traversal.
		if err := walkActionSites(&action, func(site *Site) error {
			// Defaults are interpreted below in declaration order with prior input
			// values. Docker arguments follow defaults to retain deterministic error
			// ownership. Native adapters do not execute action-authored fields.
			if native || site.Source == "" || site.Surface == SurfaceActionInputDefault || site.Surface == SurfaceDockerActionArg {
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
			"matrix":            options.Matrix,
		}
		if workflowAuthored {
			known["strategy"] = options.Strategy
		}
		inputs := expression.InputScope{Values: map[string]any{}, Complete: true}
		for _, binding := range supplied {
			name := strings.ToLower(binding.Name)
			input, declared := definitions[name]
			names, err := ValidateSite(binding.Value)
			if err != nil {
				return fmt.Errorf("action input %q: %w", binding.Name, err)
			}
			analysis, err := engine.Analyze(binding.Value.expressionSite(), caller)
			if err != nil {
				return fmt.Errorf("action input %q: %w", binding.Name, err)
			}
			authority.EventPayload = authority.EventPayload || analysis.Effects.EventPayload
			grants := grantsToken(analysis.Effects.GitHubToken)
			if !workflowAuthored && len(names) != 0 {
				return fmt.Errorf("action input %q: composite action metadata cannot grant secret authority", binding.Name)
			}
			if !workflowAuthored && grants {
				return fmt.Errorf("action input %q: composite action metadata cannot grant github.token authority. Declare a token input on the composite action, pass it from the workflow step with: token: ${{ github.token }}, and forward ${{ inputs.token }} to the child action", binding.Name)
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
				inputs.Values[name] = analysis.Value.Value
			} else {
				inputs.Complete = false
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
				// Preserve known absence without adding a property to the input object.
				known["inputs."+name] = nil
				continue
			}
			analysis, err := engine.Analyze(input.Default.expressionSite(), expression.AbstractValues{References: known, Inputs: inputs})
			if err != nil {
				return fmt.Errorf("action input %q default: %w", input.Name, err)
			}
			authority.EventPayload = authority.EventPayload || analysis.Effects.EventPayload
			authority.GitHubToken = authority.GitHubToken || grantsToken(analysis.Effects.GitHubToken)
			if analysis.Value.Known {
				inputs.Values[name] = analysis.Value.Value
			} else {
				inputs.Complete = false
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
		values := expression.AbstractValues{References: known, Inputs: inputs}
		for i, step := range action.Steps {
			condition, err := engine.Analyze(step.Condition.expressionSite(), values)
			if err != nil {
				return err
			}
			mayRun := reachable && !condition.Value.IsFalsy()
			if step.Invocation == nil {
				sites := []Site{step.WorkingDirectory}
				if step.Run != nil {
					sites = append(sites, step.Run.Command)
				}
				for _, binding := range step.Env {
					sites = append(sites, binding.Value)
				}
				for _, site := range sites {
					names, err := ValidateSite(site)
					if err != nil {
						return err
					}
					if len(names) != 0 {
						return fmt.Errorf("composite action step %d: composite action metadata cannot grant secret authority", i+1)
					}
					if !mayRun {
						continue
					}
					analysis, err := engine.Analyze(site.expressionSite(), values)
					if err != nil {
						return err
					}
					authority.GitHubToken = authority.GitHubToken || analysis.Effects.GitHubToken&expression.GitHubTokenDirect != 0
				}
				continue
			}
			// Child defaults can run during preparation even when its main step
			// is skipped. Only shell execution inherits the invocation guard.
			if err := inspect(step.Invocation.Lock, step.Invocation.With, false, mayRun, values); err != nil {
				return fmt.Errorf("composite action step %d child %q: %w", i+1, step.Invocation.Uses.Source, err)
			}
		}
		return nil
	}

	caller := expression.AbstractValues{
		References: map[string]any{"github.server_url": options.ServerURL, "job.check_run_id": "", "strategy": options.Strategy, "matrix": options.Matrix},
		Inputs:     options.WorkflowInputs,
	}
	if err := inspect(root, supplied, true, true, caller); err != nil {
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
