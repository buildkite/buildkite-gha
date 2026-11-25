package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/git"
	"github.com/buildkite/buildkite-gha/internal/program"
)

func (job Job) validateRuntimePublicActions() error {
	v2 := job.ActionResolution == RuntimePublicActionsV2
	if !v2 && (len(job.Actions) != 0 || len(job.Program.Actions) != 0) {
		return fmt.Errorf("runtime-public-v1 cannot include pre-resolved actions")
	}
	for _, lock := range job.Actions {
		descriptor, native, err := integration.AdmitNativeAdapter(integration.Identity{Source: lock.Source, ServerURL: lock.ServerURL, Repository: lock.Repository, Path: lock.Path}, lock.Commit)
		if err != nil || !native || descriptor != integration.AdapterCheckoutExactEventSHA || len(lock.Children) != 0 {
			return fmt.Errorf("runtime public actions only support existing native checkout locks")
		}
	}
	if job.Event.Provider != "github" || EventServerURL(job.Event.Provider, job.Event.ServerURL) != "https://github.com" ||
		job.Event.Name != "push" || job.Event.PayloadSource != "buildkite-webhook" ||
		!validGitHubRepository(job.Event.Repository) || !git.ValidObjectID(job.Event.SHA) || !strings.HasPrefix(job.Event.Ref, "refs/") {
		return fmt.Errorf("%s requires a GitHub.com webhook push", job.ActionResolution)
	}
	capabilitiesOK := slices.Equal(job.RequiredCapabilities, []string{"network"}) ||
		v2 && len(job.Actions) != 0 && slices.Equal(job.RequiredCapabilities, []string{"network", "provider-token-read"})
	if !*job.RequiresMise || !capabilitiesOK ||
		len(job.RequiredSecrets) != 0 || len(job.SecretMappings) != 0 || job.GitHubToken != nil || job.IDTokenPermission != "" || job.OIDC != nil {
		return fmt.Errorf("%s requires managed Node and network, without workflow credentials", job.ActionResolution)
	}
	if job.Workflow.Remote != nil || job.Workflow.SelfRepository != nil || job.Workflow.RunPath != "" ||
		len(job.Inputs) != 0 || len(job.DeferredInputs) != 0 || len(job.CallGuards) != 0 || !v2 && len(job.Dependencies) != 0 ||
		len(job.Matrix) != 0 || len(job.Strategy) != 0 || job.Container != nil || len(job.Services) != 0 || job.ServicesExpression != "" {
		return fmt.Errorf("%s does not support this job shape", job.ActionResolution)
	}
	if err := job.Program.VisitSites(func(site program.Site) error {
		if strings.Contains(site.Source, "${{") {
			return fmt.Errorf("%s requires literal workflow fields", job.ActionResolution)
		}
		return nil
	}); err != nil {
		return err
	}
	execution := job.ExecutionJob()
	if execution.Condition.Source != "" || execution.ContinueOnError.Expression != nil ||
		execution.Defaults.Shell.Source != "" && execution.Defaults.Shell.Source != "bash" {
		return fmt.Errorf("%s requires default job conditions and Bash", job.ActionResolution)
	}
	actions := 0
	selectors := map[source.Reference]struct{}{}
	for i, step := range execution.Steps {
		if step.Condition.Source != "" || step.Background || step.TimeoutMinutes.Expression != nil || step.ContinueOnError.Expression != nil {
			return fmt.Errorf("%s requires foreground steps with default conditions and literal controls", job.ActionResolution)
		}
		switch step.Kind {
		case "run":
			if shell := step.Run.Shell.Source; shell != "" && shell != "bash" {
				return fmt.Errorf("%s only supports Bash run steps", job.ActionResolution)
			}
		case "uses":
			if v2 && step.Invocation.Lock != "" && i == 0 {
				continue // Only the existing first-step native checkout may be pre-resolved.
			}
			actions++
			ref, err := source.Parse(step.Invocation.Uses.Source)
			if err != nil || ref.Path != "" || !git.ValidObjectID(ref.Ref) || step.Invocation.Lock != "" {
				return fmt.Errorf("%s requires an unlocked repository-root action at a full lowercase SHA", job.ActionResolution)
			}
			selectors[ref] = struct{}{}
		default:
			return fmt.Errorf("%s only supports run and uses steps", job.ActionResolution)
		}
	}
	if v2 {
		if len(selectors) == 0 || len(selectors) > MaxRuntimePublicActionSelectors {
			return fmt.Errorf("runtime-public-v2 requires 1–%d distinct action selectors", MaxRuntimePublicActionSelectors)
		}
		return validateActionLocks(job)
	}
	if actions != 1 {
		return fmt.Errorf("runtime-public-v1 requires exactly one action")
	}
	return nil
}
