package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/git"
	"github.com/buildkite/buildkite-gha/internal/program"
)

func (job Job) validateRuntimePublicActions() error {
	if len(job.Actions) != 0 || len(job.Program.Actions) != 0 {
		return fmt.Errorf("runtime-public-v1 cannot include pre-resolved actions")
	}
	if job.Event.Provider != "github" || EventServerURL(job.Event.Provider, job.Event.ServerURL) != "https://github.com" ||
		job.Event.Name != "push" || job.Event.PayloadSource != "buildkite-webhook" ||
		!validGitHubRepository(job.Event.Repository) || !git.ValidObjectID(job.Event.SHA) || !strings.HasPrefix(job.Event.Ref, "refs/") {
		return fmt.Errorf("runtime-public-v1 requires a GitHub.com webhook push")
	}
	if !*job.RequiresMise || !slices.Equal(job.RequiredCapabilities, []string{"network"}) ||
		len(job.RequiredSecrets) != 0 || len(job.SecretMappings) != 0 || job.GitHubToken != nil || job.IDTokenPermission != "" || job.OIDC != nil {
		return fmt.Errorf("runtime-public-v1 requires managed Node and network, without workflow credentials")
	}
	if job.Workflow.Remote != nil || job.Workflow.SelfRepository != nil || job.Workflow.RunPath != "" ||
		len(job.Inputs) != 0 || len(job.DeferredInputs) != 0 || len(job.CallGuards) != 0 || len(job.Dependencies) != 0 ||
		len(job.Matrix) != 0 || len(job.Strategy) != 0 || job.Container != nil || len(job.Services) != 0 || job.ServicesExpression != "" {
		return fmt.Errorf("runtime-public-v1 requires a standalone host job")
	}
	if err := job.Program.VisitSites(func(site program.Site) error {
		if strings.Contains(site.Source, "${{") {
			return fmt.Errorf("runtime-public-v1 requires literal workflow fields")
		}
		return nil
	}); err != nil {
		return err
	}
	execution := job.ExecutionJob()
	if execution.Condition.Source != "" || execution.ContinueOnError.Expression != nil ||
		execution.Defaults.Shell.Source != "" && execution.Defaults.Shell.Source != "bash" {
		return fmt.Errorf("runtime-public-v1 requires default job conditions and Bash")
	}
	actions := 0
	for _, step := range execution.Steps {
		if step.Condition.Source != "" || step.Background || step.TimeoutMinutes.Expression != nil || step.ContinueOnError.Expression != nil {
			return fmt.Errorf("runtime-public-v1 requires foreground steps with default conditions and literal controls")
		}
		switch step.Kind {
		case "run":
			if shell := step.Run.Shell.Source; shell != "" && shell != "bash" {
				return fmt.Errorf("runtime-public-v1 only supports Bash run steps")
			}
		case "uses":
			actions++
			ref, err := source.Parse(step.Invocation.Uses.Source)
			if err != nil || ref.Path != "" || !git.ValidObjectID(ref.Ref) || step.Invocation.Lock != "" {
				return fmt.Errorf("runtime-public-v1 requires an unlocked repository-root action at a full lowercase SHA")
			}
		default:
			return fmt.Errorf("runtime-public-v1 only supports run and uses steps")
		}
	}
	if actions != 1 {
		return fmt.Errorf("runtime-public-v1 requires exactly one action")
	}
	return nil
}
