package compiler

import (
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/program"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// workflowReferencesVars reports whether any expression in the parsed workflow
// reads the vars context, in any position and by any access form: vars.NAME,
// vars['NAME'], vars[matrix.name], or toJSON(vars). Callers use it to decide
// whether repository and organization variables must be resolved before
// compiling, so it inspects every field that can hold an expression rather
// than only compile-time positions: runtime positions read the same scopes.
// A field whose expressions do not parse is not a reference; compilation
// rejects it separately.
func workflowReferencesVars(parsed *workflow.Workflow) bool {
	if parsed == nil {
		return false
	}
	if templateReferencesVars(parsed.RunName) || templateReferencesVars(parsed.DefaultShell) || templateReferencesVars(parsed.DefaultWorkingDirectory) {
		return true
	}
	if mapReferencesVars(parsed.Env) || concurrencyReferencesVars(parsed.Concurrency) {
		return true
	}
	for _, input := range parsed.CallInputs {
		if input.Default != nil && valueReferencesVars(input.Default.Data) {
			return true
		}
	}
	for _, output := range parsed.CallOutputs {
		if templateReferencesVars(output.Value) {
			return true
		}
	}
	for _, job := range parsed.Jobs {
		if jobReferencesVars(job) {
			return true
		}
	}
	return false
}

// ActionsReferenceVars reports whether any resolved action program in the
// bundle's plans reads the vars context, such as an action.yml input default
// of ${{ vars.REGION }} or a composite step condition of vars.ENABLED ==
// 'true'. Action metadata is only known once compilation has resolved the
// actions, so a workflow whose sole vars reference lives in an action is
// discovered here rather than by Report.ReferencesVars; callers resolve the
// scopes and compile again so the plans carry them.
func ActionsReferenceVars(bundle Bundle) bool {
	for _, artifact := range bundle.Plans {
		if artifact.Job.Program == nil {
			continue
		}
		for _, action := range artifact.Job.Program.Actions {
			found := false
			_ = action.VisitSites(func(site program.Site) error {
				found = found || siteReferencesVars(site)
				return nil
			})
			if found {
				return true
			}
		}
	}
	return false
}

// siteReferencesVars inspects one program site by its surface: condition
// surfaces hold bare expressions, every other surface holds a template.
func siteReferencesVars(site program.Site) bool {
	switch site.Surface {
	case program.SurfaceJobCondition, program.SurfaceCallCondition, program.SurfaceStepCondition, program.SurfaceActionLifecycle:
		return conditionReferencesVars(site.Source)
	default:
		return templateReferencesVars(site.Source)
	}
}

func jobReferencesVars(job workflow.Job) bool {
	for _, value := range []string{job.Name, job.Environment, job.ServicesExpression, job.DefaultShell, job.DefaultWorkingDirectory} {
		if templateReferencesVars(value) {
			return true
		}
	}
	if conditionReferencesVars(job.If) || mapReferencesVars(job.Env) || mapReferencesVars(job.Outputs) || concurrencyReferencesVars(job.Concurrency) {
		return true
	}
	for _, label := range job.RunsOn {
		if templateReferencesVars(label) {
			return true
		}
	}
	if expressionReferencesVars(job.RunsOnExpr) || matrixReferencesVars(job.Matrix) {
		return true
	}
	if job.Reusable != nil {
		for _, input := range job.Reusable.Inputs {
			if valueReferencesVars(input.Data) {
				return true
			}
		}
	}
	if container := job.Container; container != nil {
		if templateReferencesVars(container.Image) || mapReferencesVars(container.Env) || sliceReferencesVars(container.Ports) {
			return true
		}
	}
	for _, service := range job.Services {
		container := service.Container
		for _, value := range []string{container.Image, container.Options, container.Command, container.Entrypoint} {
			if templateReferencesVars(value) {
				return true
			}
		}
		if mapReferencesVars(container.Env) || sliceReferencesVars(container.Ports) || sliceReferencesVars(container.Volumes) {
			return true
		}
		if credentials := container.Credentials; credentials != nil && (templateReferencesVars(credentials.Username) || templateReferencesVars(credentials.Password)) {
			return true
		}
	}
	for _, step := range job.Steps {
		if stepReferencesVars(step) {
			return true
		}
	}
	return false
}

func stepReferencesVars(step workflow.Step) bool {
	for _, value := range []string{step.Name, step.Run, step.Shell, step.WorkingDirectory, step.ContinueOnErrorExpression, step.TimeoutMinutesExpression} {
		if templateReferencesVars(value) {
			return true
		}
	}
	return conditionReferencesVars(step.If) || mapReferencesVars(step.Env) || mapReferencesVars(step.With)
}

func concurrencyReferencesVars(concurrency *workflow.Concurrency) bool {
	if concurrency == nil {
		return false
	}
	return templateReferencesVars(concurrency.Group) || expressionReferencesVars(concurrency.CancelInProgressExpression)
}

func matrixReferencesVars(matrix *workflow.Matrix) bool {
	if matrix == nil {
		return false
	}
	if expressionReferencesVars(matrix.Expression) || expressionReferencesVars(matrix.IncludeExpression) || expressionReferencesVars(matrix.ExcludeExpression) {
		return true
	}
	for _, row := range matrix.Rows {
		if expressionReferencesVars(row.Expression) {
			return true
		}
		for _, value := range row.Values {
			if valueReferencesVars(value.Data) {
				return true
			}
		}
	}
	for _, combinations := range [][]workflow.MatrixCombination{matrix.Include, matrix.Exclude} {
		for _, combination := range combinations {
			for _, value := range combination.Values {
				if valueReferencesVars(value.Data) {
					return true
				}
			}
		}
	}
	return false
}

func mapReferencesVars(values map[string]string) bool {
	for _, value := range values {
		if templateReferencesVars(value) {
			return true
		}
	}
	return false
}

func sliceReferencesVars(values []string) bool {
	for _, value := range values {
		if templateReferencesVars(value) {
			return true
		}
	}
	return false
}

// valueReferencesVars walks an owned JSON-compatible value for template
// strings that read vars.
func valueReferencesVars(value any) bool {
	switch value := value.(type) {
	case string:
		return templateReferencesVars(value)
	case []any:
		for _, element := range value {
			if valueReferencesVars(element) {
				return true
			}
		}
	case map[string]any:
		for _, element := range value {
			if valueReferencesVars(element) {
				return true
			}
		}
	}
	return false
}

// expressionReferencesVars inspects a parsed bare expression such as a
// deferred matrix or runs-on expression.
func expressionReferencesVars(expr *expression.Expression) bool {
	if expr == nil {
		return false
	}
	return conditionReferencesVars(expr.Text)
}

// conditionReferencesVars inspects a bare condition, which GitHub evaluates as
// an expression whether or not it is wrapped in ${{ }}.
func conditionReferencesVars(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	if !strings.Contains(value, "${{") {
		value = "${{ " + value + " }}"
	}
	return templateReferencesVars(value)
}

func templateReferencesVars(value string) bool {
	if !strings.Contains(value, "${{") {
		return false
	}
	found, err := referencesContext(value, expression.ProfilePartialTemplate, "vars", false)
	return err == nil && found
}
