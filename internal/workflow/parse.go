package workflow

import (
	"fmt"
	"sort"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/rhysd/actionlint"
	"go.yaml.in/yaml/v4"
)

// Parse uses actionlint as the syntax frontend and immediately converts its AST
// into the owned workflow model.
func Parse(path string, source []byte) (*Workflow, error) {
	parsed, errs := actionlint.Parse(source)
	if len(errs) != 0 {
		err := errs[0]
		return nil, fmt.Errorf("%s:%d:%d: %s", path, err.Line, err.Column, err.Message)
	}
	scalars, err := scalarValues(source)
	if err != nil {
		return nil, fmt.Errorf("%s: parse scalar values: %w", path, err)
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
	if call, ok := parsed.FindWorkflowCallEvent(); ok {
		owned.Callable = true
		if len(call.Inputs) != 0 {
			owned.CallInputs = make(map[string]CallInput, len(call.Inputs))
		}
		for _, input := range call.Inputs {
			ownedInput := CallInput{Type: workflowCallInputType(input.Type), Required: input.IsRequired()}
			if input.Default != nil {
				value := Value{Data: scalarAt(input.Default, scalars), Span: spanFrom(input.Default.Pos, input.Default.Value)}
				ownedInput.Default = &value
			}
			owned.CallInputs[input.ID] = ownedInput
		}
		for name, secret := range call.Secrets {
			if secret.Required != nil && secret.Required.Expression != nil {
				return nil, locatedError(path, secret.Required.Expression.Pos, "workflow", fmt.Sprintf("expression-valued required flag for workflow_call secret %q is unsupported", name))
			}
			if secret.Required != nil && secret.Required.Value {
				owned.RequiredCallSecrets = append(owned.RequiredCallSecrets, name)
			}
		}
		sort.Strings(owned.RequiredCallSecrets)
	}

	ids := make([]string, 0, len(parsed.Jobs))
	for id := range parsed.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		job, err := adaptJob(path, parsed.Jobs[id], scalars)
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

func adaptJob(path string, in *actionlint.Job, scalars map[Position]any) (Job, error) {
	out := Job{ID: in.ID.Value, Span: pointSpan(in.Pos)}
	if in.WorkflowCall != nil {
		call := in.WorkflowCall
		out.Reusable = &ReusableWorkflowCall{
			Uses:           call.Uses.Value,
			Secrets:        len(call.Secrets) != 0,
			InheritSecrets: call.InheritSecrets,
			Span:           spanFrom(call.Uses.Pos, call.Uses.Value),
		}
		if len(call.Inputs) != 0 {
			out.Reusable.Inputs = make(map[string]Value, len(call.Inputs))
			for name, input := range call.Inputs {
				out.Reusable.Inputs[name] = Value{
					Data: scalarAt(input.Value, scalars),
					Span: spanFrom(input.Value.Pos, input.Value.Value),
				}
			}
		}
	}
	if in.If != nil {
		out.If = in.If.Value
	}
	if in.Container != nil || in.Services != nil {
		return Job{}, locatedError(path, in.Pos, in.ID.Value, "job and service containers are unsupported in the Phase 0 runtime")
	}
	if in.ContinueOnError != nil {
		return Job{}, locatedError(path, in.Pos, in.ID.Value, "job continue-on-error is unsupported")
	}
	if in.TimeoutMinutes != nil {
		if in.TimeoutMinutes.Expression != nil {
			return Job{}, locatedError(path, in.TimeoutMinutes.Expression.Pos, in.ID.Value, "expression-valued job timeout-minutes is unsupported")
		}
		out.TimeoutMinutes = in.TimeoutMinutes.Value
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
			if in.Strategy.FailFast.Expression != nil {
				return Job{}, locatedError(path, in.Strategy.FailFast.Expression.Pos, in.ID.Value, "expression-valued matrix fail-fast is unsupported")
			}
			v := in.Strategy.FailFast.Value
			out.FailFast = &v
		}
		if in.Strategy.MaxParallel != nil {
			if in.Strategy.MaxParallel.Expression != nil {
				return Job{}, locatedError(path, in.Strategy.MaxParallel.Expression.Pos, in.ID.Value, "expression-valued matrix max-parallel is unsupported")
			}
			v := in.Strategy.MaxParallel.Value
			out.MaxParallel = &v
		}
		if in.Strategy.Matrix != nil {
			matrix, err := adaptMatrix(path, in.ID.Value, in.Strategy.Matrix, scalars)
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
			if step.ContinueOnError.Expression != nil {
				return Job{}, locatedError(path, step.ContinueOnError.Expression.Pos, in.ID.Value, "expression-valued step continue-on-error is unsupported")
			}
			owned.ContinueOnError = step.ContinueOnError.Value
		}
		if step.TimeoutMinutes != nil {
			if step.TimeoutMinutes.Expression != nil {
				return Job{}, locatedError(path, step.TimeoutMinutes.Expression.Pos, in.ID.Value, "expression-valued step timeout-minutes is unsupported")
			}
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

func workflowCallInputType(inputType actionlint.WorkflowCallEventInputType) string {
	switch inputType {
	case actionlint.WorkflowCallEventInputTypeBoolean:
		return "boolean"
	case actionlint.WorkflowCallEventInputTypeNumber:
		return "number"
	case actionlint.WorkflowCallEventInputTypeString:
		return "string"
	default:
		return ""
	}
}

func scalarAt(value *actionlint.String, scalars map[Position]any) any {
	if scalar, ok := scalars[Position{Line: value.Pos.Line, Column: value.Pos.Col}]; ok {
		return scalar
	}
	return value.Value
}

func adaptEnv(in *actionlint.Env) map[string]string {
	if in == nil || len(in.Vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(in.Vars))
	for _, variable := range in.Vars {
		out[variable.Name.Value] = variable.Value.Value
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

func adaptMatrix(path, jobID string, in *actionlint.Matrix, scalars map[Position]any) (*Matrix, error) {
	out := &Matrix{Span: pointSpan(in.Pos)}
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
	sort.Slice(names, func(i, j int) bool {
		left, right := matrixRowPosition(in.Rows[names[i]]), matrixRowPosition(in.Rows[names[j]])
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Col != right.Col {
			return left.Col < right.Col
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		row := in.Rows[name]
		owned := MatrixRow{Name: name, Span: pointSpan(matrixRowPosition(row))}
		if row.Expression != nil {
			expr, err := adaptExpression(row.Expression)
			if err != nil {
				return nil, locatedError(path, row.Expression.Pos, jobID, err.Error())
			}
			owned.Expression = &expr
		}
		for _, value := range row.Values {
			owned.Values = append(owned.Values, Value{Data: adaptValue(value, scalars), Span: spanFrom(value.Pos(), value.String())})
		}
		out.Rows = append(out.Rows, owned)
		if len(owned.Values) > 0 {
			out.Span.End = owned.Values[len(owned.Values)-1].Span.End
		}
	}
	var err error
	out.Include, err = adaptMatrixCombinations(path, jobID, "include", in.Include, scalars)
	if err != nil {
		return nil, err
	}
	out.Exclude, err = adaptMatrixCombinations(path, jobID, "exclude", in.Exclude, scalars)
	if err != nil {
		return nil, err
	}
	for _, combinations := range [][]MatrixCombination{out.Include, out.Exclude} {
		for _, combination := range combinations {
			if positionAfter(combination.Span.End, out.Span.End) {
				out.Span.End = combination.Span.End
			}
		}
	}
	return out, nil
}

func matrixRowPosition(row *actionlint.MatrixRow) *actionlint.Pos {
	if row.Name != nil {
		return row.Name.Pos
	}
	return row.Expression.Pos
}

func adaptMatrixCombinations(path, jobID, section string, in *actionlint.MatrixCombinations, scalars map[Position]any) ([]MatrixCombination, error) {
	if in == nil {
		return nil, nil
	}
	if in.Expression != nil {
		return nil, locatedError(path, in.Expression.Pos, jobID, fmt.Sprintf("runtime-dependent matrix %s expressions are unsupported", section))
	}
	out := make([]MatrixCombination, 0, len(in.Combinations))
	for _, combination := range in.Combinations {
		if combination.Expression != nil {
			return nil, locatedError(path, combination.Expression.Pos, jobID, fmt.Sprintf("runtime-dependent matrix %s expressions are unsupported", section))
		}
		owned := MatrixCombination{Values: make(map[string]Value, len(combination.Assigns))}
		for name, assignment := range combination.Assigns {
			value := Value{Data: adaptValue(assignment.Value, scalars), Span: spanFrom(assignment.Value.Pos(), assignment.Value.String())}
			owned.Values[name] = value
			start := Position{Line: assignment.Key.Pos.Line, Column: assignment.Key.Pos.Col}
			if owned.Span.Start.Line == 0 || positionAfter(owned.Span.Start, start) {
				owned.Span.Start = start
			}
			if positionAfter(value.Span.End, owned.Span.End) {
				owned.Span.End = value.Span.End
			}
		}
		out = append(out, owned)
	}
	return out, nil
}

func positionAfter(left, right Position) bool {
	return left.Line > right.Line || left.Line == right.Line && left.Column > right.Column
}

func adaptValue(in actionlint.RawYAMLValue, scalars map[Position]any) any {
	switch value := in.(type) {
	case *actionlint.RawYAMLString:
		if scalar, ok := scalars[Position{Line: value.Pos().Line, Column: value.Pos().Col}]; ok {
			return scalar
		}
		return value.Value
	case *actionlint.RawYAMLArray:
		out := make([]any, 0, len(value.Elems))
		for _, elem := range value.Elems {
			out = append(out, adaptValue(elem, scalars))
		}
		return out
	case *actionlint.RawYAMLObject:
		out := make(map[string]any, len(value.Props))
		for key, property := range value.Props {
			out[key] = adaptValue(property, scalars)
		}
		return out
	default:
		return value.String()
	}
}

func scalarValues(source []byte) (map[Position]any, error) {
	// actionlint's raw scalar model intentionally discards YAML scalar tags.
	// Join a YAML node tree by source position so quoted strings remain distinct
	// from the booleans, numbers, and nulls used in matrix contexts.
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return nil, err
	}
	values := make(map[Position]any)
	var walk func(*yaml.Node) error
	walk = func(node *yaml.Node) error {
		if node.Kind == yaml.ScalarNode {
			value := any(node.Value)
			switch node.ShortTag() {
			case "!!bool", "!!int", "!!float", "!!null":
				if err := node.Decode(&value); err != nil {
					return err
				}
			}
			values[Position{Line: node.Line, Column: node.Column}] = value
		}
		for _, child := range node.Content {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(&document); err != nil {
		return nil, err
	}
	return values, nil
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
