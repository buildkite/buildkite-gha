package workflow

import (
	"fmt"
	"sort"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/rhysd/actionlint"
)

// Parse uses actionlint as the syntax frontend and immediately converts its AST
// into the owned workflow model.
func Parse(path string, source []byte) (*Workflow, error) {
	parsed, errs := actionlint.Parse(source)
	if len(errs) != 0 {
		err := errs[0]
		return nil, fmt.Errorf("%s:%d:%d: %s", path, err.Line, err.Column, err.Message)
	}

	owned := &Workflow{}
	if parsed.Name != nil {
		owned.Name = parsed.Name.Value
	}
	if parsed.Env != nil && parsed.Env.Expression != nil {
		return nil, locatedError(path, parsed.Env.Expression.Pos, "workflow", "expression-valued workflow env is unsupported")
	}
	owned.Env = adaptEnv(parsed.Env)
	if parsed.Defaults != nil && parsed.Defaults.Run != nil {
		if parsed.Defaults.Run.Shell != nil {
			owned.DefaultShell = parsed.Defaults.Run.Shell.Value
		}
		if parsed.Defaults.Run.WorkingDirectory != nil {
			owned.DefaultWorkingDirectory = parsed.Defaults.Run.WorkingDirectory.Value
		}
	}

	ids := make([]string, 0, len(parsed.Jobs))
	for id := range parsed.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		job, err := adaptJob(path, parsed.Jobs[id])
		if err != nil {
			return nil, err
		}
		job.Env = mergeEnv(owned.Env, job.Env)
		if job.DefaultShell == "" {
			job.DefaultShell = owned.DefaultShell
		}
		if job.DefaultWorkingDirectory == "" {
			job.DefaultWorkingDirectory = owned.DefaultWorkingDirectory
		}
		owned.Jobs = append(owned.Jobs, job)
	}
	return owned, nil
}

func adaptJob(path string, in *actionlint.Job) (Job, error) {
	out := Job{ID: in.ID.Value, Reusable: in.WorkflowCall != nil, Span: pointSpan(in.Pos)}
	if in.If != nil {
		return Job{}, locatedError(path, in.If.Pos, in.ID.Value, "job conditions are unsupported in the Phase 0 runtime")
	}
	if in.Container != nil || in.Services != nil {
		return Job{}, locatedError(path, in.Pos, in.ID.Value, "job and service containers are unsupported in the Phase 0 runtime")
	}
	if in.TimeoutMinutes != nil || in.ContinueOnError != nil {
		return Job{}, locatedError(path, in.Pos, in.ID.Value, "job timeout and continue-on-error are unsupported in the Phase 0 runtime")
	}
	if in.Env != nil && in.Env.Expression != nil {
		return Job{}, locatedError(path, in.Env.Expression.Pos, in.ID.Value, "expression-valued job env is unsupported")
	}
	if in.Name != nil {
		out.Name = in.Name.Value
	}
	out.Env = adaptEnv(in.Env)
	if in.Defaults != nil && in.Defaults.Run != nil {
		if in.Defaults.Run.Shell != nil {
			out.DefaultShell = in.Defaults.Run.Shell.Value
		}
		if in.Defaults.Run.WorkingDirectory != nil {
			out.DefaultWorkingDirectory = in.Defaults.Run.WorkingDirectory.Value
		}
	}
	if len(in.Outputs) != 0 {
		out.Outputs = make(map[string]string, len(in.Outputs))
		for name, output := range in.Outputs {
			out.Outputs[name] = output.Value.Value
		}
	}
	for _, need := range in.Needs {
		if need.ContainsExpression() {
			return Job{}, locatedError(path, need.Pos, in.ID.Value, "runtime-dependent needs expressions are unsupported")
		}
		out.Needs = append(out.Needs, need.Value)
	}
	sort.Strings(out.Needs)

	if in.RunsOn != nil {
		for _, label := range in.RunsOn.Labels {
			out.RunsOn = append(out.RunsOn, label.Value)
		}
		if in.RunsOn.LabelsExpr != nil {
			expr, err := adaptExpression(in.RunsOn.LabelsExpr)
			if err != nil {
				return Job{}, locatedError(path, in.RunsOn.LabelsExpr.Pos, in.ID.Value, err.Error())
			}
			out.RunsOnExpr = &expr
		}
	}

	if in.Strategy != nil {
		if in.Strategy.FailFast != nil {
			v := in.Strategy.FailFast.Value
			out.FailFast = &v
		}
		if in.Strategy.MaxParallel != nil {
			v := in.Strategy.MaxParallel.Value
			out.MaxParallel = &v
		}
		if in.Strategy.Matrix != nil {
			matrix, err := adaptMatrix(path, in.ID.Value, in.Strategy.Matrix)
			if err != nil {
				return Job{}, err
			}
			out.Matrix = matrix
		}
	}

	for _, step := range in.Steps {
		owned := Step{Span: pointSpan(step.Pos)}
		if step.ID != nil {
			owned.ID = step.ID.Value
		}
		if step.Name != nil {
			owned.Name = step.Name.Value
		}
		if step.If != nil {
			owned.If = step.If.Value
		}
		if step.ContinueOnError != nil {
			owned.ContinueOnError = step.ContinueOnError.Value
		}
		if step.TimeoutMinutes != nil {
			owned.TimeoutMinutes = step.TimeoutMinutes.Value
		}
		if step.Env != nil && step.Env.Expression != nil {
			return Job{}, locatedError(path, step.Env.Expression.Pos, in.ID.Value, "expression-valued step env is unsupported")
		}
		owned.Env = adaptEnv(step.Env)
		switch exec := step.Exec.(type) {
		case *actionlint.ExecRun:
			owned.Kind = "run"
			owned.Run = exec.Run.Value
			if exec.Shell != nil {
				owned.Shell = exec.Shell.Value
			}
			if exec.WorkingDirectory != nil {
				owned.WorkingDirectory = exec.WorkingDirectory.Value
			}
			owned.Span = spanFrom(step.Pos, exec.Run.Value)
		case *actionlint.ExecAction:
			if exec.Entrypoint != nil || exec.Args != nil {
				return Job{}, locatedError(path, step.Pos, in.ID.Value, "action entrypoint and args overrides are unsupported in the Phase 0 runtime")
			}
			owned.Kind = "uses"
			owned.Uses = exec.Uses.Value
			if len(exec.Inputs) != 0 {
				owned.With = make(map[string]string, len(exec.Inputs))
				for name, input := range exec.Inputs {
					owned.With[name] = input.Value.Value
				}
			}
			owned.Span = spanFrom(step.Pos, exec.Uses.Value)
		default:
			return Job{}, locatedError(path, step.Pos, in.ID.Value, "unsupported step execution kind")
		}
		out.Steps = append(out.Steps, owned)
		out.Span.End = owned.Span.End
	}
	return out, nil
}

func adaptEnv(in *actionlint.Env) map[string]string {
	if in == nil || len(in.Vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(in.Vars))
	for name, variable := range in.Vars {
		out[name] = variable.Value.Value
	}
	return out
}

func mergeEnv(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for name, value := range base {
		out[name] = value
	}
	for name, value := range override {
		out[name] = value
	}
	return out
}

func adaptMatrix(path, jobID string, in *actionlint.Matrix) (*Matrix, error) {
	out := &Matrix{
		HasInclude: in.Include != nil,
		HasExclude: in.Exclude != nil,
		Span:       pointSpan(in.Pos),
	}
	if in.Expression != nil {
		expr, err := adaptExpression(in.Expression)
		if err != nil {
			return nil, locatedError(path, in.Expression.Pos, jobID, err.Error())
		}
		out.Expression = &expr
	}

	names := make([]string, 0, len(in.Rows))
	for name := range in.Rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		row := in.Rows[name]
		owned := MatrixRow{Name: row.Name.Value, Span: pointSpan(row.Name.Pos)}
		if row.Expression != nil {
			expr, err := adaptExpression(row.Expression)
			if err != nil {
				return nil, locatedError(path, row.Expression.Pos, jobID, err.Error())
			}
			owned.Expression = &expr
		}
		for _, value := range row.Values {
			owned.Values = append(owned.Values, Value{Data: adaptValue(value), Span: spanFrom(value.Pos(), value.String())})
		}
		out.Rows = append(out.Rows, owned)
		if len(owned.Values) > 0 {
			out.Span.End = owned.Values[len(owned.Values)-1].Span.End
		}
	}
	return out, nil
}

func adaptValue(in actionlint.RawYAMLValue) any {
	switch value := in.(type) {
	case *actionlint.RawYAMLString:
		return value.Value
	case *actionlint.RawYAMLArray:
		out := make([]any, 0, len(value.Elems))
		for _, elem := range value.Elems {
			out = append(out, adaptValue(elem))
		}
		return out
	case *actionlint.RawYAMLObject:
		out := make(map[string]any, len(value.Props))
		for key, property := range value.Props {
			out[key] = adaptValue(property)
		}
		return out
	default:
		return value.String()
	}
}

func adaptExpression(in *actionlint.String) (expression.Expression, error) {
	return expression.Parse(in.Value, in.Pos.Line, in.Pos.Col)
}

func locatedError(path string, pos *actionlint.Pos, jobID, message string) error {
	return fmt.Errorf("%s:%d:%d: job %q: %s", path, pos.Line, pos.Col, jobID, message)
}

func pointSpan(pos *actionlint.Pos) Span {
	if pos == nil {
		return Span{}
	}
	p := Position{Line: pos.Line, Column: pos.Col}
	return Span{Start: p, End: p}
}

func spanFrom(pos *actionlint.Pos, text string) Span {
	span := pointSpan(pos)
	for _, r := range text {
		if r == '\n' {
			span.End.Line++
			span.End.Column = 1
		} else {
			span.End.Column++
		}
	}
	return span
}
