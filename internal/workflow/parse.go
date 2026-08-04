package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/rhysd/actionlint"
	"go.yaml.in/yaml/v4"
)

const missingStepExecutionDiagnostic = "step must run script with \"run\" section or run action with \"uses\" section"

type stepConcurrency struct {
	Kind       string
	Background bool
	Targets    []string
	Parallel   []Step
}

type expectedActionlintDiagnostic struct {
	Position Position
	Prefix   string
}

type concurrencySyntax struct {
	Steps       map[Position]stepConcurrency
	Diagnostics []expectedActionlintDiagnostic
}

// Parse uses actionlint as the syntax frontend and immediately converts its AST
// into the owned workflow model.
func Parse(path string, source []byte) (*Workflow, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return nil, fmt.Errorf("%s: parse workflow YAML: %w", path, err)
	}
	if err := validateRawContainers(path, &document); err != nil {
		return nil, err
	}
	if err := validateRawConcurrency(path, &document); err != nil {
		return nil, err
	}
	concurrency, err := parseConcurrencySyntax(path, &document)
	if err != nil {
		return nil, err
	}
	parsed, errs := actionlint.Parse(source)
	if err := filterActionlintDiagnostics(path, errs, concurrency.Diagnostics); err != nil {
		return nil, err
	}
	scalars, err := scalarValues(&document)
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
		if len(call.Outputs) != 0 {
			owned.CallOutputs = make(map[string]CallOutput, len(call.Outputs))
		}
		for name, output := range call.Outputs {
			if output.Value == nil {
				return nil, locatedError(path, output.Name.Pos, "workflow", fmt.Sprintf("workflow_call output %q has no value", output.Name.Value))
			}
			owned.CallOutputs[name] = CallOutput{
				Name:  output.Name.Value,
				Value: output.Value.Value,
				Span:  spanFrom(output.Value.Pos, output.Value.Value),
			}
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
		job, err := adaptJob(path, parsed.Jobs[id], scalars, concurrency.Steps)
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
	if len(concurrency.Steps) != 0 {
		positions := make([]Position, 0, len(concurrency.Steps))
		for position := range concurrency.Steps {
			positions = append(positions, position)
		}
		sort.Slice(positions, func(i, j int) bool {
			if positions[i].Line != positions[j].Line {
				return positions[i].Line < positions[j].Line
			}
			return positions[i].Column < positions[j].Column
		})
		position := positions[0]
		return nil, fmt.Errorf("%s:%d:%d: concurrent step did not match the pinned actionlint syntax tree", path, position.Line, position.Column)
	}
	return owned, nil
}

var containerImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[0-9a-f]{64})?$`)
var containerEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var containerPortPattern = regexp.MustCompile(`^(?:[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])(?::(?:[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?(?:/(?:tcp|udp))?$`)

// validateRawContainers deliberately walks only the owned container locations.
// actionlint normalizes maps (including service IDs), so the raw tree is also
// the authoritative source for diagnostics.
func validateRawContainers(path string, document *yaml.Node) error {
	root := document
	if root.Kind == yaml.DocumentNode && len(root.Content) != 0 {
		root = root.Content[0]
	}
	jobs := mappingValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		job := jobs.Content[i+1]
		if container := mappingValue(job, "container"); container != nil {
			if err := validateRawContainer(path, container); err != nil {
				return err
			}
		}
		services := mappingValue(job, "services")
		if services == nil || services.Kind != yaml.MappingNode {
			continue
		}
		if len(services.Content)/2 > 32 {
			return rawError(path, services, "container services have more than 32 entries")
		}
		for j := 0; j+1 < len(services.Content); j += 2 {
			name, container := services.Content[j], services.Content[j+1]
			if name.Value != strings.ToLower(name.Value) || !serviceIDPattern.MatchString(name.Value) {
				return rawError(path, name, fmt.Sprintf("invalid service ID %q; service IDs must be lowercase", name.Value))
			}
			if err := validateRawContainer(path, container); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRawConcurrency(path string, document *yaml.Node) error {
	root := document
	if root.Kind == yaml.DocumentNode && len(root.Content) != 0 {
		root = root.Content[0]
	}
	if key := mappingKey(root, "concurrency"); key != nil {
		return rawError(path, key, "workflow concurrency is unsupported; configure equivalent concurrency behavior in Buildkite")
	}
	jobs := mappingValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		name, job := resolveAlias(jobs.Content[i]), jobs.Content[i+1]
		if key := mappingKey(job, "concurrency"); key != nil {
			return rawError(path, key, fmt.Sprintf("job %q concurrency is unsupported; only strategy.max-parallel is translated", name.Value))
		}
	}
	return nil
}

func rawError(path string, node *yaml.Node, message string) error {
	return fmt.Errorf("%s:%d:%d: %s", path, node.Line, node.Column, message)
}

func validateRawContainer(path string, node *yaml.Node) error {
	image := node
	if node.Kind == yaml.MappingNode {
		image = mappingValue(node, "image")
		for _, control := range []string{"credentials", "volumes", "options"} {
			if key := mappingKey(node, control); key != nil {
				return rawError(path, key, "container "+control+" are unsupported")
			}
		}
	}
	if image == nil || image.Kind != yaml.ScalarNode || len(image.Value) > 512 || !containerImagePattern.MatchString(image.Value) {
		if image == nil {
			image = node
		}
		return rawError(path, image, "invalid container image")
	}
	env := mappingValue(node, "env")
	if env != nil && env.Kind == yaml.MappingNode {
		if len(env.Content)/2 > 256 {
			return rawError(path, env, "container environment has more than 256 entries")
		}
		total := 0
		for i := 0; i+1 < len(env.Content); i += 2 {
			key, value := env.Content[i], env.Content[i+1]
			if len(key.Value) > 255 || !containerEnvKeyPattern.MatchString(key.Value) {
				return rawError(path, key, "invalid container environment key")
			}
			if value.Kind != yaml.ScalarNode || len(value.Value) > 65536 {
				return rawError(path, value, "invalid container environment value")
			}
			total += len(key.Value) + len(value.Value)
		}
		if total > 1048576 {
			return rawError(path, env, "container environment exceeds 1048576 bytes")
		}
	}
	ports := mappingValue(node, "ports")
	if ports != nil && ports.Kind == yaml.SequenceNode {
		if len(ports.Content) > 128 {
			return rawError(path, ports, "container has more than 128 ports")
		}
		seen := map[string]bool{}
		for _, port := range ports.Content {
			if port.Kind != yaml.ScalarNode || !containerPortPattern.MatchString(port.Value) || seen[port.Value] {
				return rawError(path, port, "invalid or repeated container port")
			}
			seen[port.Value] = true
		}
	}
	return nil
}

func adaptJob(path string, in *actionlint.Job, scalars map[Position]any, concurrency map[Position]stepConcurrency) (Job, error) {
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
	if in.Container != nil {
		container, err := adaptContainer(path, in.ID.Value, in.Container)
		if err != nil {
			return Job{}, err
		}
		out.Container = &container
	}
	if in.Services != nil {
		if in.Services.Expression != nil {
			return Job{}, locatedError(path, in.Services.Expression.Pos, in.ID.Value, "expression-valued services are unsupported")
		}
		names := make([]string, 0, len(in.Services.Value))
		for name := range in.Services.Value {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			service := in.Services.Value[name]
			if service == nil || service.Name == nil || service.Container == nil || !serviceIDPattern.MatchString(service.Name.Value) {
				return Job{}, locatedError(path, in.Services.Pos, in.ID.Value, fmt.Sprintf("invalid service ID %q", name))
			}
			container, err := adaptContainer(path, in.ID.Value, service.Container)
			if err != nil {
				return Job{}, err
			}
			out.Services = append(out.Services, Service{Name: service.Name.Value, Container: container})
		}
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
		stepPosition := Position{Line: step.Pos.Line, Column: step.Pos.Col}
		control, hasConcurrency := concurrency[stepPosition]
		if hasConcurrency {
			delete(concurrency, stepPosition)
		}
		if control.Kind == "parallel" {
			targets := make([]string, 0, len(control.Parallel))
			for i, member := range control.Parallel {
				if member.ID == "" {
					member.ID = parallelStepID(stepPosition, i+1)
				}
				member.Background = true
				targets = append(targets, member.ID)
				out.Steps = append(out.Steps, member)
				out.Span.End = member.Span.End
			}
			barrierSpan := pointSpan(step.Pos)
			barrierSpan.End = out.Span.End
			barrier := Step{ID: parallelBarrierID(stepPosition), Kind: "wait", Targets: targets, Span: barrierSpan}
			out.Steps = append(out.Steps, barrier)
			out.Span.End = barrier.Span.End
			continue
		}
		owned := Step{Background: control.Background, Targets: append([]string(nil), control.Targets...), Span: pointSpan(step.Pos)}
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
		case nil:
			if control.Kind == "" {
				return Job{}, locatedError(path, step.Pos, in.ID.Value, "unsupported step execution kind")
			}
			owned.Kind = control.Kind
		default:
			return Job{}, locatedError(path, step.Pos, in.ID.Value, "unsupported step execution kind")
		}
		out.Steps = append(out.Steps, owned)
		out.Span.End = owned.Span.End
	}
	if err := validateStepConcurrency(path, in.ID.Value, out.Steps); err != nil {
		return Job{}, err
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

var serviceIDPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

func adaptContainer(path, jobID string, in *actionlint.Container) (Container, error) {
	if in.Image == nil || strings.TrimSpace(in.Image.Value) == "" {
		return Container{}, locatedError(path, in.Pos, jobID, "container image must be a non-empty literal")
	}
	if in.Image.ContainsExpression() {
		return Container{}, locatedError(path, in.Image.Pos, jobID, "expression-valued container image is unsupported")
	}
	if in.Credentials != nil {
		return Container{}, locatedError(path, in.Credentials.Pos, jobID, "container credentials are unsupported")
	}
	if len(in.Volumes) != 0 {
		return Container{}, locatedError(path, in.Volumes[0].Pos, jobID, "container volumes are unsupported")
	}
	if in.Options != nil {
		return Container{}, locatedError(path, in.Options.Pos, jobID, "container options are unsupported")
	}
	if in.Env != nil && in.Env.Expression != nil {
		return Container{}, locatedError(path, in.Env.Expression.Pos, jobID, "expression-valued container env is unsupported")
	}
	if in.Env != nil {
		for _, variable := range in.Env.Vars {
			if variable.Value.ContainsExpression() {
				return Container{}, locatedError(path, variable.Value.Pos, jobID, "expression-valued container env is unsupported")
			}
		}
	}
	out := Container{Image: in.Image.Value, Env: adaptEnv(in.Env), Span: pointSpan(in.Pos)}
	for _, port := range in.Ports {
		if port.ContainsExpression() {
			return Container{}, locatedError(path, port.Pos, jobID, "expression-valued container port is unsupported")
		}
		out.Ports = append(out.Ports, port.Value)
	}
	return out, nil
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

func scalarValues(document *yaml.Node) (map[Position]any, error) {
	// actionlint's raw scalar model intentionally discards YAML scalar tags.
	// Join a YAML node tree by source position so quoted strings remain distinct
	// from the booleans, numbers, and nulls used in matrix contexts.
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
	if err := walk(document); err != nil {
		return nil, err
	}
	return values, nil
}

func parseConcurrencySyntax(path string, document *yaml.Node) (concurrencySyntax, error) {
	syntax := concurrencySyntax{Steps: map[Position]stepConcurrency{}}
	root := document
	if root.Kind == yaml.DocumentNode && len(root.Content) != 0 {
		root = root.Content[0]
	}
	jobs := mappingValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return syntax, nil
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		job := jobs.Content[i+1]
		steps := mappingValue(job, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, step := range steps.Content {
			if step.Kind != yaml.MappingNode {
				continue
			}
			parsed, diagnostic, ok, err := parseStepConcurrency(path, step)
			if err != nil {
				return concurrencySyntax{}, err
			}
			if !ok {
				continue
			}
			position := nodePosition(step)
			syntax.Steps[position] = parsed
			syntax.Diagnostics = append(syntax.Diagnostics, diagnostic)
		}
	}
	return syntax, nil
}

func parseStepConcurrency(path string, step *yaml.Node) (stepConcurrency, expectedActionlintDiagnostic, bool, error) {
	entries := mappingEntries(step)
	background, hasBackground := entries["background"]
	controlKinds := make([]string, 0, 3)
	for _, kind := range []string{"wait", "wait-all", "cancel", "parallel"} {
		if _, ok := entries[kind]; ok {
			controlKinds = append(controlKinds, kind)
		}
	}
	if !hasBackground && len(controlKinds) == 0 {
		return stepConcurrency{}, expectedActionlintDiagnostic{}, false, nil
	}
	if len(controlKinds) > 1 || len(controlKinds) != 0 && (hasBackground || entries["run"] != nil || entries["uses"] != nil) {
		return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, step, "concurrent step must declare exactly one execution or control kind")
	}
	if hasBackground {
		if entries["run"] == nil && entries["uses"] == nil {
			return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, background, "background is only valid on run or uses steps")
		}
		if background.ShortTag() != "!!bool" || background.Value != "true" {
			return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, background, "background must be the literal true")
		}
		key := mappingKey(step, "background")
		return stepConcurrency{Background: true}, expectedActionlintDiagnostic{
			Position: nodePosition(key),
			Prefix:   "unexpected key \"background\" for step to ",
		}, true, nil
	}

	kind := controlKinds[0]
	if kind == "parallel" {
		if len(entries) != 1 {
			names := make([]string, 0, len(entries))
			for name := range entries {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if name != kind {
					return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, mappingKey(step, name), fmt.Sprintf("parallel control does not support %q", name))
				}
			}
		}
		members, err := parseParallelSteps(path, entries[kind])
		if err != nil {
			return stepConcurrency{}, expectedActionlintDiagnostic{}, false, err
		}
		return stepConcurrency{Kind: kind, Parallel: members}, expectedActionlintDiagnostic{Position: nodePosition(step), Prefix: missingStepExecutionDiagnostic}, true, nil
	}
	allowed := map[string]bool{"name": true, kind: true}
	for name := range entries {
		if !allowed[name] {
			return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, mappingKey(step, name), fmt.Sprintf("%s control does not support %q", kind, name))
		}
	}
	control := stepConcurrency{Kind: kind}
	switch kind {
	case "wait":
		targets, err := stringList(entries[kind])
		if err != nil || len(targets) == 0 {
			return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, entries[kind], "wait requires one step id or a non-empty list of step ids")
		}
		control.Targets = targets
	case "wait-all":
		if entries[kind].ShortTag() != "!!null" {
			return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, entries[kind], "wait-all does not accept a value")
		}
	case "cancel":
		targets, err := stringList(entries[kind])
		if err != nil || len(targets) != 1 || entries[kind].Kind != yaml.ScalarNode {
			return stepConcurrency{}, expectedActionlintDiagnostic{}, false, yamlNodeError(path, entries[kind], "cancel requires exactly one step id")
		}
		control.Targets = targets
	}
	return control, expectedActionlintDiagnostic{Position: nodePosition(step), Prefix: missingStepExecutionDiagnostic}, true, nil
}

func parseParallelSteps(path string, node *yaml.Node) ([]Step, error) {
	if node.Kind != yaml.SequenceNode || len(node.Content) == 0 {
		return nil, yamlNodeError(path, node, "parallel requires a non-empty list of run or uses steps")
	}
	steps := make([]Step, 0, len(node.Content))
	for _, child := range node.Content {
		step, err := parseParallelStep(path, child)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func parseParallelStep(path string, node *yaml.Node) (Step, error) {
	if node.Kind != yaml.MappingNode {
		return Step{}, yamlNodeError(path, node, "parallel member must be a run or uses step")
	}
	entries := mappingEntries(node)
	allowed := map[string]bool{
		"id": true, "name": true, "run": true, "uses": true, "shell": true, "working-directory": true,
		"env": true, "with": true, "if": true, "continue-on-error": true, "timeout-minutes": true,
	}
	for name := range entries {
		if !allowed[name] {
			return Step{}, yamlNodeError(path, mappingKey(node, name), fmt.Sprintf("parallel member does not support %q", name))
		}
	}
	run, hasRun := entries["run"]
	uses, hasUses := entries["uses"]
	if hasRun == hasUses {
		return Step{}, yamlNodeError(path, node, "parallel member must declare exactly one of run or uses")
	}
	if err := validateParallelStepSyntax(path, node); err != nil {
		return Step{}, err
	}
	execution := run
	if hasUses {
		execution = uses
	}
	if execution.Kind != yaml.ScalarNode || execution.ShortTag() == "!!null" || strings.TrimSpace(execution.Value) == "" {
		return Step{}, yamlNodeError(path, execution, "parallel member execution must be a non-empty scalar")
	}
	step := Step{Span: yamlStepSpan(node, execution)}
	var err error
	if step.ID, err = optionalString(entries["id"]); err != nil {
		return Step{}, yamlNodeError(path, entries["id"], "parallel member id must be a string")
	}
	if step.Name, err = optionalScalar(entries["name"]); err != nil {
		return Step{}, yamlNodeError(path, entries["name"], "parallel member name must be a scalar")
	}
	if step.If, err = optionalScalar(entries["if"]); err != nil {
		return Step{}, yamlNodeError(path, entries["if"], "parallel member if must be a scalar")
	}
	if step.Env, err = scalarMap(entries["env"]); err != nil {
		return Step{}, yamlNodeError(path, entries["env"], "parallel member env must be a scalar mapping")
	}
	if value := entries["continue-on-error"]; value != nil {
		if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!bool" {
			return Step{}, yamlNodeError(path, value, "parallel member continue-on-error must be a literal boolean")
		}
		if err := value.Decode(&step.ContinueOnError); err != nil {
			return Step{}, yamlNodeError(path, value, "parallel member continue-on-error must be a literal boolean")
		}
	}
	if value := entries["timeout-minutes"]; value != nil {
		if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!int" && value.ShortTag() != "!!float" {
			return Step{}, yamlNodeError(path, value, "parallel member timeout-minutes must be a literal number")
		}
		step.TimeoutMinutes, err = strconv.ParseFloat(value.Value, 64)
		if err != nil {
			return Step{}, yamlNodeError(path, value, "parallel member timeout-minutes must be a literal number")
		}
	}
	if hasRun {
		if entries["with"] != nil {
			return Step{}, yamlNodeError(path, mappingKey(node, "with"), "parallel run member does not support with")
		}
		step.Kind = "run"
		step.Run = run.Value
		if step.Shell, err = optionalScalar(entries["shell"]); err != nil {
			return Step{}, yamlNodeError(path, entries["shell"], "parallel member shell must be a scalar")
		}
		if step.WorkingDirectory, err = optionalScalar(entries["working-directory"]); err != nil {
			return Step{}, yamlNodeError(path, entries["working-directory"], "parallel member working-directory must be a scalar")
		}
		return step, nil
	}
	if entries["shell"] != nil || entries["working-directory"] != nil {
		return Step{}, yamlNodeError(path, node, "parallel uses member cannot declare shell or working-directory")
	}
	step.Kind = "uses"
	step.Uses = uses.Value
	if step.With, err = scalarMap(entries["with"]); err != nil {
		return Step{}, yamlNodeError(path, entries["with"], "parallel member with must be a scalar mapping")
	}
	for name, value := range step.With {
		lower := strings.ToLower(name)
		if lower != name {
			delete(step.With, name)
			step.With[lower] = value
		}
	}
	_, hasEntrypoint := step.With["entrypoint"]
	_, hasArgs := step.With["args"]
	if strings.HasPrefix(strings.ToLower(step.Uses), "docker://") && (hasEntrypoint || hasArgs) {
		return Step{}, yamlNodeError(path, entries["with"], "parallel docker action member uses unsupported entrypoint or args overrides")
	}
	return step, nil
}

func validateParallelStepSyntax(path string, step *yaml.Node) error {
	stringNode := func(value string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	}
	mappingNode := func(content ...*yaml.Node) *yaml.Node {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content}
	}
	document := yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{
		mappingNode(
			stringNode("on"), stringNode("push"),
			stringNode("jobs"), mappingNode(
				stringNode("parallel"), mappingNode(
					stringNode("runs-on"), stringNode("ubuntu-latest"),
					stringNode("steps"), &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{step}},
				),
			),
		),
	}}
	source, err := yaml.Marshal(&document)
	if err != nil {
		return yamlNodeError(path, step, fmt.Sprintf("validate parallel member: %v", err))
	}
	_, errs := actionlint.Parse(source)
	if len(errs) != 0 {
		return yamlNodeError(path, step, fmt.Sprintf("invalid parallel member: %s", errs[0].Message))
	}
	return nil
}

func optionalString(node *yaml.Node) (string, error) {
	if node == nil {
		return "", nil
	}
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!str" || node.Value == "" {
		return "", fmt.Errorf("value is not a string")
	}
	return node.Value, nil
}

func optionalScalar(node *yaml.Node) (string, error) {
	if node == nil {
		return "", nil
	}
	if node.Kind != yaml.ScalarNode || node.ShortTag() == "!!null" {
		return "", fmt.Errorf("value is not a scalar")
	}
	return node.Value, nil
}

func scalarMap(node *yaml.Node) (map[string]string, error) {
	if node == nil {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("value is not a mapping")
	}
	values := make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" || key.Value == "" || value.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("mapping contains a non-scalar entry")
		}
		values[key.Value] = value.Value
	}
	return values, nil
}

func parallelStepID(position Position, member int) string {
	return fmt.Sprintf("__parallel_%d_%d_%d", position.Line, position.Column, member)
}

func parallelBarrierID(position Position) string {
	return fmt.Sprintf("__parallel_%d_%d_wait", position.Line, position.Column)
}

func yamlStepSpan(step, execution *yaml.Node) Span {
	span := Span{Start: nodePosition(step), End: nodePosition(execution)}
	for _, r := range execution.Value {
		if r == '\n' {
			span.End.Line++
			span.End.Column = 1
		} else {
			span.End.Column++
		}
	}
	return span
}

func filterActionlintDiagnostics(path string, errs []*actionlint.Error, expected []expectedActionlintDiagnostic) error {
	matched := make([]bool, len(expected))
	for _, actionlintErr := range errs {
		match := -1
		for i, diagnostic := range expected {
			if !matched[i] && diagnostic.Position.Line == actionlintErr.Line && diagnostic.Position.Column == actionlintErr.Column && strings.HasPrefix(actionlintErr.Message, diagnostic.Prefix) {
				match = i
				break
			}
		}
		if match >= 0 {
			matched[match] = true
			continue
		}
		return fmt.Errorf("%s:%d:%d: %s", path, actionlintErr.Line, actionlintErr.Column, actionlintErr.Message)
	}
	for i, ok := range matched {
		if !ok {
			diagnostic := expected[i]
			return fmt.Errorf("%s:%d:%d: actionlint concurrency diagnostic changed; pinned parser contract must be reviewed", path, diagnostic.Position.Line, diagnostic.Position.Column)
		}
	}
	return nil
}

func validateStepConcurrency(path, jobID string, steps []Step) error {
	background := make(map[string]struct{})
	for _, step := range steps {
		if step.Background && step.ID != "" {
			background[strings.ToLower(step.ID)] = struct{}{}
		}
		if step.Kind != "wait" && step.Kind != "cancel" {
			continue
		}
		seen := make(map[string]struct{}, len(step.Targets))
		for _, target := range step.Targets {
			key := strings.ToLower(target)
			if _, duplicate := seen[key]; duplicate {
				return workflowSpanError(path, step.Span, jobID, fmt.Sprintf("%s repeats background step %q", step.Kind, target))
			}
			seen[key] = struct{}{}
			if _, ok := background[key]; !ok {
				return workflowSpanError(path, step.Span, jobID, fmt.Sprintf("%s target %q is not a prior background step with an id", step.Kind, target))
			}
		}
	}
	return nil
}

func mappingEntries(node *yaml.Node) map[string]*yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return map[string]*yaml.Node{}
	}
	entries := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		entries[node.Content[i].Value] = node.Content[i+1]
	}
	return entries
}

func mappingValue(node *yaml.Node, name string) *yaml.Node {
	node = resolveAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if mappingKeyMatches(node.Content[i], name) {
			return resolveAlias(node.Content[i+1])
		}
	}
	return nil
}

func mappingKey(node *yaml.Node, name string) *yaml.Node {
	node = resolveAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if mappingKeyMatches(node.Content[i], name) {
			return node.Content[i]
		}
	}
	return nil
}

func mappingKeyMatches(node *yaml.Node, name string) bool {
	node = resolveAlias(node)
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == name
}

func resolveAlias(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func stringList(node *yaml.Node) ([]string, error) {
	if node == nil {
		return nil, fmt.Errorf("missing value")
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.ShortTag() != "!!str" || node.Value == "" {
			return nil, fmt.Errorf("value is not a step id")
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		values := make([]string, 0, len(node.Content))
		for _, child := range node.Content {
			if child.Kind != yaml.ScalarNode || child.ShortTag() != "!!str" || child.Value == "" {
				return nil, fmt.Errorf("value is not a step id")
			}
			values = append(values, child.Value)
		}
		return values, nil
	default:
		return nil, fmt.Errorf("value is not a string or list")
	}
}

func nodePosition(node *yaml.Node) Position {
	if node == nil {
		return Position{}
	}
	return Position{Line: node.Line, Column: node.Column}
}

func yamlNodeError(path string, node *yaml.Node, message string) error {
	position := nodePosition(node)
	return fmt.Errorf("%s:%d:%d: %s", path, position.Line, position.Column, message)
}

func workflowSpanError(path string, span Span, jobID, message string) error {
	return fmt.Errorf("%s:%d:%d: job %q: %s", path, span.Start.Line, span.Start.Column, jobID, message)
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
