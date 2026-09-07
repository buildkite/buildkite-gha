package workflow

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/rhysd/actionlint"
	"go.yaml.in/yaml/v4"
)

type stepConcurrency struct {
	Kind       string
	Background bool
	Targets    []string
	Parallel   []Step
}

type concurrencySyntax struct {
	Steps       map[Position]stepConcurrency
	Diagnostics []expectedActionlintDiagnostic
}

// actionlint v1.7.12 compares YAML boolean text case-sensitively, even though
// YAML accepts True and TRUE. Decode cancellation booleans from the raw tree so
// no true spelling can be silently adapted as false.
func rawConcurrencyCancellations(document *yaml.Node) (*Position, map[string]Position) {
	root := document
	if root.Kind == yaml.DocumentNode && len(root.Content) != 0 {
		root = root.Content[0]
	}
	workflowCancellation := rawConcurrencyCancellation(mappingValue(root, "concurrency"))
	jobCancellations := map[string]Position{}
	jobs := mappingValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return workflowCancellation, jobCancellations
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		name, job := resolveAlias(jobs.Content[i]), jobs.Content[i+1]
		if position := rawConcurrencyCancellation(mappingValue(job, "concurrency")); position != nil {
			jobCancellations[name.Value] = *position
		}
	}
	return workflowCancellation, jobCancellations
}

func rawConcurrencyCancellation(concurrency *yaml.Node) *Position {
	if concurrency == nil || concurrency.Kind != yaml.MappingNode {
		return nil
	}
	cancel := mappingValue(concurrency, "cancel-in-progress")
	if cancel == nil || cancel.Kind != yaml.ScalarNode || cancel.ShortTag() != "!!bool" {
		return nil
	}
	var enabled bool
	if err := cancel.Decode(&enabled); err != nil || !enabled {
		return nil
	}
	position := nodePosition(cancel)
	return &position
}

func adaptConcurrency(path, jobID string, in *actionlint.Concurrency) (*Concurrency, error) {
	if in == nil {
		return nil, nil
	}
	var cancellationExpression *expression.Expression
	var cancellationPosition Position
	if in.CancelInProgress != nil && in.CancelInProgress.Expression != nil {
		position := in.CancelInProgress.Pos
		if in.CancelInProgress.Expression.Pos != nil {
			position = in.CancelInProgress.Expression.Pos
		}
		expr, err := adaptExpression(in.CancelInProgress.Expression)
		if err != nil {
			scope := "workflow"
			if jobID != "" {
				scope = fmt.Sprintf("job %q", jobID)
			}
			return nil, fmt.Errorf("%s:%d:%d: %s concurrency cancel-in-progress: %w", path, position.Line, position.Col, scope, err)
		}
		cancellationExpression = &expr
		cancellationPosition = Position{Line: position.Line, Column: position.Col}
	}
	if in.Group == nil || strings.TrimSpace(in.Group.Value) == "" {
		position := in.Pos
		if jobID == "" {
			return nil, fmt.Errorf("%s:%d:%d: workflow concurrency group must not be empty", path, position.Line, position.Col)
		}
		return nil, locatedError(path, position, jobID, "concurrency group must not be empty")
	}
	return &Concurrency{
		Group:                      in.Group.Value,
		CancelInProgress:           in.CancelInProgress != nil && in.CancelInProgress.Value,
		CancelInProgressExpression: cancellationExpression,
		CancelInProgressPosition:   cancellationPosition,
		Span:                       spanFrom(in.Group.Pos, in.Group.Value),
	}, nil
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
	if value := entries["if"]; value != nil {
		step.IfSpan = yamlStepSpan(value, value)
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
		return Step{}, yamlNodeError(path, entries["with"], actionsource.UnsupportedContainerActionReason)
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
