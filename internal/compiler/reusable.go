package compiler

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	maxReusableWorkflowDepth = 4
	maxFlattenedJobs         = 1024
)

var staticInputCondition = regexp.MustCompile(`(?i)^\s*(?:\$\{\{\s*inputs\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_-]*)|\[\s*'([A-Za-z0-9_-]{1,255})'\s*\])\s*\}\}|inputs\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_-]*)|\[\s*'([A-Za-z0-9_-]{1,255})'\s*\]))\s*$`)
var staticValueExpression = regexp.MustCompile(`(?i)^\s*\$\{\{\s*(inputs|matrix)\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_-]*)|\[\s*'([A-Za-z0-9_-]{1,255})'\s*\])\s*\}\}\s*$`)
var callOutputNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

type sourcedJob struct {
	workflow.Job
	path            string
	digest          string
	root            string
	inputs          map[string]any
	secretAuthority bool
	needBindings    map[string]needBinding
}

type needBinding struct {
	// members are flattened logical job IDs owned by one source-level need.
	// projectOutputs distinguishes reusable-call/status-only boundaries from
	// ordinary job needs, whose outputs pass through unchanged.
	members        []string
	projectOutputs bool
	outputs        []needOutputBinding
}

type needOutputBinding struct {
	name   string
	member string
	output string
	path   string
	span   workflow.Span
}

type reusableResolution struct {
	jobs    []sourcedJob
	outputs []needOutputBinding
}

type reusableResolver struct {
	root                  string
	stack                 []string
	context               expression.CompileContext
	expanded              int
	runtimeMatrixBoundary bool
}

func resolveReusableWorkflows(path string, source []byte, parsed *workflow.Workflow, context expression.CompileContext) ([]sourcedJob, bool, error) {
	digest := "sha256:" + sha256Sum(source)
	runtimeMatrixBoundary := hasRuntimeMatrixBoundary(parsed)
	if !hasReusableCall(parsed) {
		sourcePath := path
		root := ""
		if isRepositoryWorkflowPath(path) {
			repositoryRoot, canonicalPath, err := workflowRepository(path)
			if err != nil {
				return nil, runtimeMatrixBoundary, err
			}
			root = repositoryRoot
			sourcePath, err = repositoryWorkflowPath(repositoryRoot, canonicalPath)
			if err != nil {
				return nil, runtimeMatrixBoundary, err
			}
		}
		jobs := make([]sourcedJob, len(parsed.Jobs))
		workflowJobs := make(map[string]workflow.Job, len(parsed.Jobs))
		replacements := make(map[string]needBinding, len(parsed.Jobs))
		for i, job := range parsed.Jobs {
			resolvedJob, err := applyStaticInputs(sourcePath, job, context.Inputs)
			if err != nil {
				return nil, runtimeMatrixBoundary, err
			}
			job = resolvedJob
			job.Permissions = effectivePermissions(job.Permissions, parsed.Permissions, nil, false)
			bindings := make(map[string]needBinding, len(job.Needs))
			for _, need := range job.Needs {
				bindings[need] = needBinding{members: []string{need}}
			}
			jobs[i] = sourcedJob{Job: job, path: sourcePath, digest: digest, root: root, inputs: cloneAnyMap(context.Inputs), secretAuthority: true, needBindings: bindings}
			workflowJobs[job.ID] = job
			replacements[job.ID] = needBinding{members: []string{job.ID}}
		}
		if _, err := resolveWorkflowCallOutputs(sourcePath, parsed.CallOutputs, workflowJobs, replacements); err != nil {
			return nil, runtimeMatrixBoundary, err
		}
		return jobs, runtimeMatrixBoundary, nil
	}

	root, canonicalPath, err := workflowRepository(path)
	if err != nil {
		return nil, runtimeMatrixBoundary, err
	}
	sourcePath, err := repositoryWorkflowPath(root, canonicalPath)
	if err != nil {
		return nil, runtimeMatrixBoundary, err
	}
	resolver := reusableResolver{root: root, stack: []string{canonicalPath}, context: context, runtimeMatrixBoundary: runtimeMatrixBoundary}
	resolver.discoverRuntimeMatrixBoundaries(parsed, 0, map[string]int{canonicalPath: 0})
	resolution, err := resolver.resolve(sourcePath, digest, parsed, "", "", context.Inputs, nil, nil, true, 0)
	return resolution.jobs, resolver.runtimeMatrixBoundary, err
}

func hasReusableCall(parsed *workflow.Workflow) bool {
	for _, job := range parsed.Jobs {
		if job.Reusable != nil {
			return true
		}
	}
	return false
}

func (resolver *reusableResolver) discoverRuntimeMatrixBoundaries(parsed *workflow.Workflow, depth int, scannedAtDepth map[string]int) {
	resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasRuntimeMatrixBoundary(parsed)
	if depth >= maxReusableWorkflowDepth {
		resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasReusableCall(parsed)
		return
	}
	for _, job := range parsed.Jobs {
		if job.Reusable == nil {
			continue
		}
		calleePath, err := resolver.localWorkflowPath(job.Reusable.Uses)
		if err != nil {
			resolver.runtimeMatrixBoundary = true
			continue
		}
		calleeDepth := depth + 1
		previousDepth, scanned := scannedAtDepth[calleePath]
		if scanned && previousDepth <= calleeDepth {
			continue
		}
		if !scanned && len(scannedAtDepth) >= maxFlattenedJobs {
			// An incomplete discovery cannot prove that no unvisited callee has a
			// runtime matrix boundary. Reject before event metadata instead.
			resolver.runtimeMatrixBoundary = true
			return
		}
		scannedAtDepth[calleePath] = calleeDepth
		source, err := os.ReadFile(calleePath)
		if err != nil {
			resolver.runtimeMatrixBoundary = true
			continue
		}
		callee, err := workflow.Parse(calleePath, source)
		if err != nil {
			resolver.runtimeMatrixBoundary = true
			continue
		}
		resolver.discoverRuntimeMatrixBoundaries(callee, calleeDepth, scannedAtDepth)
	}
}

func (resolver *reusableResolver) resolve(path, digest string, parsed *workflow.Workflow, namespace, labelPrefix string, inputs map[string]any, externalNeeds map[string]needBinding, permissionCeiling *workflow.Permissions, secretAuthority bool, depth int) (reusableResolution, error) {
	resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasRuntimeMatrixBoundary(parsed)
	jobs := make(map[string]workflow.Job, len(parsed.Jobs))
	for _, job := range parsed.Jobs {
		jobs[job.ID] = job
	}
	order, err := topologicalOrder(path, jobs)
	if err != nil {
		return reusableResolution{}, err
	}

	replacements := make(map[string]needBinding, len(jobs))
	var resolved []sourcedJob
	for _, id := range order {
		job := jobs[id]
		job.Permissions = effectivePermissions(job.Permissions, parsed.Permissions, permissionCeiling, depth != 0)
		job, err = applyStaticInputs(path, job, inputs)
		if err != nil {
			return reusableResolution{}, err
		}
		if parsed.Callable {
			if err := rejectUnresolvedInputExpressions(path, job); err != nil {
				return reusableResolution{}, err
			}
		}
		if job.Reusable != nil {
			if err := rejectCallMatrixExpressions(path, job); err != nil {
				return reusableResolution{}, err
			}
			if strings.TrimSpace(job.If) != "" {
				return reusableResolution{}, jobError(path, job, "reusable-workflow call conditions are unsupported")
			}
		}
		needBindings := replacementNeeds(job.Needs, replacements)
		if len(job.Needs) == 0 {
			needBindings = cloneNeedBindings(externalNeeds)
			for name, binding := range needBindings {
				binding.projectOutputs = true
				binding.outputs = nil
				needBindings[name] = binding
			}
		}
		needs := bindingMembers(needBindings)
		if job.Reusable == nil {
			job.ID = namespacedJobID(namespace, job.ID)
			job.Needs = needs
			if labelPrefix != "" {
				name := job.Name
				if name == "" {
					name = id
				}
				job.Name = labelPrefix + " / " + name
			}
			resolver.expanded++
			if resolver.expanded > maxFlattenedJobs {
				return reusableResolution{}, jobError(path, job, fmt.Sprintf("reusable-workflow graph expands beyond %d jobs", maxFlattenedJobs))
			}
			resolved = append(resolved, sourcedJob{Job: job, path: path, digest: digest, root: resolver.root, inputs: cloneAnyMap(inputs), secretAuthority: secretAuthority, needBindings: needBindings})
			replacements[id] = needBinding{members: []string{job.ID}}
			continue
		}

		call := job.Reusable
		if call.InheritSecrets {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, "secrets: inherit is unsupported")
		}
		if call.Secrets {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, "reusable-workflow secret forwarding is unsupported")
		}
		if depth >= maxReusableWorkflowDepth {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("reusable-workflow nesting exceeds maximum depth %d", maxReusableWorkflowDepth))
		}
		calleePath, err := resolver.localWorkflowPath(call.Uses)
		if err != nil {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, err.Error())
		}
		if cycle := resolver.cycle(calleePath); cycle != "" {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, "reusable-workflow cycle detected: "+cycle)
		}
		source, err := os.ReadFile(calleePath)
		if err != nil {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("read local reusable workflow %q: %v", call.Uses, err))
		}
		callee, err := workflow.Parse(calleePath, source)
		if err != nil {
			return reusableResolution{}, err
		}
		resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasRuntimeMatrixBoundary(callee)
		if !callee.Callable {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("local reusable workflow %q does not declare on.workflow_call", call.Uses))
		}
		if len(callee.RequiredCallSecrets) != 0 {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("local reusable workflow %q requires unsupported secret %q", call.Uses, callee.RequiredCallSecrets[0]))
		}
		calleeSourcePath, err := filepath.Rel(resolver.root, calleePath)
		if err != nil {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("locate local reusable workflow %q: %v", call.Uses, err))
		}
		calleeSourcePath = "./" + filepath.ToSlash(calleeSourcePath)
		if callee.Concurrency != nil {
			return reusableResolution{}, fmt.Errorf("%s:%d:%d: workflow concurrency in a called reusable workflow is unsupported", calleeSourcePath, callee.Concurrency.Span.Start.Line, callee.Concurrency.Span.Start.Column)
		}
		calleeDigest := "sha256:" + sha256Sum(source)

		matrices, err := expandMatrix(path, job, expression.CompileContext{})
		if err != nil {
			return reusableResolution{}, err
		}
		var members []string
		var callOutputs []needOutputBinding
		callNamespaces := make(map[string]struct{}, len(matrices))
		for _, matrix := range matrices {
			callInputs, err := resolveCallInputs(path, job, call, callee, inputs, matrix, resolver.context)
			if err != nil {
				return reusableResolution{}, err
			}
			component := job.ID
			if len(matrices) > 1 {
				suffix, err := matrixDigest(matrix)
				if err != nil {
					return reusableResolution{}, jobError(path, job, fmt.Sprintf("namespace reusable-workflow matrix: %v", err))
				}
				component += "-" + suffix
			}
			callNamespace := namespacedJobID(namespace, component)
			if _, exists := callNamespaces[callNamespace]; exists {
				return reusableResolution{}, jobError(path, job, fmt.Sprintf("reusable-workflow matrix produces duplicate namespace %q", callNamespace))
			}
			callNamespaces[callNamespace] = struct{}{}
			callLabel := job.Name
			if callLabel == "" {
				callLabel = job.ID
			}
			if len(matrix) != 0 {
				callLabel = instanceLabel(job, matrix, expression.CompileContext{})
			}
			if labelPrefix != "" {
				callLabel = labelPrefix + " / " + callLabel
			}

			calleePermissionCeiling := job.Permissions
			if calleePermissionCeiling == nil {
				calleePermissionCeiling = &workflow.Permissions{Scopes: map[string]string{}, Span: call.Span}
			}
			resolver.stack = append(resolver.stack, calleePath)
			calleeResolution, err := resolver.resolve(calleeSourcePath, calleeDigest, callee, callNamespace, callLabel, callInputs, needBindings, calleePermissionCeiling, secretAuthority && call.InheritSecrets, depth+1)
			resolver.stack = resolver.stack[:len(resolver.stack)-1]
			if err != nil {
				message := "local reusable workflow could not be resolved"
				detail := ""
				var finding *ProcessingFinding
				if errors.As(err, &finding) {
					message = finding.Message
					detail = finding.Detail
				}
				return reusableResolution{}, &ProcessingFinding{
					Stage: StageGraph, Code: CodeGraphInvalid, Category: "compatibility",
					Path: path, Line: call.Span.Start.Line, Column: call.Span.Start.Column, Job: job.ID,
					Message: message, Detail: detail,
					Err: locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column,
						fmt.Sprintf("resolve local reusable workflow %q: %v", call.Uses, err)),
				}
			}
			resolved = append(resolved, calleeResolution.jobs...)
			callOutputs = append(callOutputs, calleeResolution.outputs...)
			for _, calleeJob := range calleeResolution.jobs {
				members = append(members, calleeJob.ID)
			}
		}
		sort.Strings(members)
		sortNeedOutputBindings(callOutputs)
		replacements[id] = needBinding{members: members, projectOutputs: true, outputs: callOutputs}
	}
	outputs, err := resolveWorkflowCallOutputs(path, parsed.CallOutputs, jobs, replacements)
	if err != nil {
		return reusableResolution{}, err
	}
	return reusableResolution{jobs: resolved, outputs: outputs}, nil
}

func effectivePermissions(job, workflowDefault, ceiling *workflow.Permissions, bounded bool) *workflow.Permissions {
	declared := job
	if declared == nil {
		declared = workflowDefault
	}
	if !bounded {
		if declared == nil {
			return defaultGitHubTokenPermissions()
		}
		return clonePermissions(declared)
	}
	if declared == nil {
		return clonePermissions(ceiling)
	}
	effective := &workflow.Permissions{Scopes: map[string]string{}, Span: declared.Span}
	if ceiling == nil {
		return effective
	}
	for name, access := range declared.Scopes {
		ceilingAccess, ok := ceiling.Scopes[name]
		if !ok {
			continue
		}
		if access == "write" && ceilingAccess == "read" {
			access = "read"
		}
		effective.Scopes[name] = access
	}
	return effective
}

func defaultGitHubTokenPermissions() *workflow.Permissions {
	// Provide the narrow permission needed by setup actions without inheriting
	// unobservable organization or repository settings that may grant writes.
	return &workflow.Permissions{Scopes: map[string]string{"contents": "read"}}
}

func clonePermissions(in *workflow.Permissions) *workflow.Permissions {
	if in == nil {
		return nil
	}
	out := &workflow.Permissions{Scopes: make(map[string]string, len(in.Scopes)), Span: in.Span}
	for name, access := range in.Scopes {
		out.Scopes[name] = access
	}
	return out
}

func replacementNeeds(needs []string, replacements map[string]needBinding) map[string]needBinding {
	out := make(map[string]needBinding, len(needs))
	for _, need := range needs {
		out[need] = cloneNeedBinding(replacements[need])
	}
	return out
}

func bindingMembers(bindings map[string]needBinding) []string {
	seen := make(map[string]struct{})
	for _, binding := range bindings {
		for _, member := range binding.members {
			seen[member] = struct{}{}
		}
	}
	members := make([]string, 0, len(seen))
	for member := range seen {
		members = append(members, member)
	}
	sort.Strings(members)
	return members
}

func resolveWorkflowCallOutputs(path string, declarations map[string]workflow.CallOutput, jobs map[string]workflow.Job, replacements map[string]needBinding) ([]needOutputBinding, error) {
	var resolved []needOutputBinding
	for _, declarationID := range sortedValueKeys(declarations) {
		declaration := declarations[declarationID]
		if !callOutputNamePattern.MatchString(declaration.Name) {
			return nil, workflowCallOutputError(path, declaration, "has an invalid name")
		}
		root, reference, err := expression.ReferencePath(declaration.Value)
		if err != nil || !strings.EqualFold(root, "jobs") || len(reference) != 3 || !strings.EqualFold(reference[1], "outputs") {
			return nil, workflowCallOutputError(path, declaration, "must be one static jobs.<job_id>.outputs.<output_name> reference")
		}
		job, ok := findWorkflowJob(jobs, reference[0])
		if !ok {
			return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references unknown job %q", reference[0]))
		}
		binding, ok := replacements[job.ID]
		if !ok {
			return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references unresolved job %q", reference[0]))
		}
		if !binding.projectOutputs {
			output, ok := findOutputName(job.Outputs, reference[2])
			if !ok {
				return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references undeclared output %q on job %q", reference[2], job.ID))
			}
			for _, member := range binding.members {
				resolved = append(resolved, needOutputBinding{name: declaration.Name, member: member, output: output, path: path, span: declaration.Span})
			}
			continue
		}

		found := false
		for _, output := range binding.outputs {
			if !strings.EqualFold(output.name, reference[2]) {
				continue
			}
			found = true
			output.name = declaration.Name
			output.path = path
			output.span = declaration.Span
			resolved = append(resolved, output)
		}
		if !found {
			return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references undeclared output %q on reusable job %q", reference[2], job.ID))
		}
	}
	sortNeedOutputBindings(resolved)
	return resolved, nil
}

func findWorkflowJob(jobs map[string]workflow.Job, name string) (workflow.Job, bool) {
	for _, job := range jobs {
		if strings.EqualFold(job.ID, name) {
			return job, true
		}
	}
	return workflow.Job{}, false
}

func findOutputName(outputs map[string]string, name string) (string, bool) {
	for output := range outputs {
		if strings.EqualFold(output, name) {
			return output, true
		}
	}
	return "", false
}

func sortNeedOutputBindings(outputs []needOutputBinding) {
	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].name != outputs[j].name {
			return outputs[i].name < outputs[j].name
		}
		if outputs[i].member != outputs[j].member {
			return outputs[i].member < outputs[j].member
		}
		return outputs[i].output < outputs[j].output
	})
}

func workflowCallOutputError(path string, output workflow.CallOutput, message string) error {
	return fmt.Errorf("%s:%d:%d: workflow_call output %q %s", path, output.Span.Start.Line, output.Span.Start.Column, output.Name, message)
}

func cloneNeedBindings(bindings map[string]needBinding) map[string]needBinding {
	if bindings == nil {
		return nil
	}
	cloned := make(map[string]needBinding, len(bindings))
	for name, binding := range bindings {
		cloned[name] = cloneNeedBinding(binding)
	}
	return cloned
}

func cloneNeedBinding(binding needBinding) needBinding {
	binding.members = append([]string(nil), binding.members...)
	binding.outputs = append([]needOutputBinding(nil), binding.outputs...)
	return binding
}

func namespacedJobID(namespace, id string) string {
	if namespace == "" {
		return id
	}
	return namespace + "." + id
}

func resolveCallInputs(path string, job workflow.Job, call *workflow.ReusableWorkflowCall, callee *workflow.Workflow, parentInputs, matrix map[string]any, context expression.CompileContext) (map[string]any, error) {
	values := make(map[string]any, len(call.Inputs))
	for _, name := range sortedValueKeys(call.Inputs) {
		value := call.Inputs[name]
		if _, ok := callee.CallInputs[name]; !ok {
			return nil, locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, fmt.Sprintf("input %q is not declared by reusable workflow %q", name, call.Uses))
		}
		resolved := value.Data
		if text, ok := resolved.(string); ok && strings.Contains(text, "${{") {
			var err error
			resolved, err = evaluateStaticCallValue(text, parentInputs, matrix, context)
			if err != nil {
				detail := fmt.Sprintf("Reusable-workflow input %q is not statically resolvable: %v", name, err)
				message := fmt.Sprintf("Reusable workflow input %q uses a value that is unavailable before jobs run. Replace it with a literal or an expression that does not depend on job results.", name)
				if strings.Contains(err.Error(), `unsupported compile-time context "needs"`) {
					message = fmt.Sprintf("Reusable workflow input %q uses the needs context, which is unavailable before jobs run. Replace it with a literal or an expression that does not depend on job results.", name)
				}
				return nil, &ProcessingFinding{
					Stage: StageGraph, Code: CodeGraphInvalid, Category: "compatibility",
					Path: path, Line: value.Span.Start.Line, Column: value.Span.Start.Column, Job: job.ID,
					Message: message, Detail: detail,
					Err: locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, detail),
				}
			}
		}
		values[name] = resolved
	}

	resolved := make(map[string]any, len(callee.CallInputs))
	for _, name := range sortedValueKeys(callee.CallInputs) {
		declaration := callee.CallInputs[name]
		value, supplied := values[name]
		ok := supplied
		if !ok && declaration.Default != nil {
			value, ok = declaration.Default.Data, true
			if text, isString := value.(string); isString && strings.Contains(text, "${{") {
				var err error
				value, err = expression.EvaluateReusableInputDefault(text, context)
				if err != nil {
					return nil, locatedJobError(call.Uses, job, declaration.Default.Span.Start.Line, declaration.Default.Span.Start.Column, fmt.Sprintf("evaluate default for reusable-workflow input %q: %v", name, err))
				}
			}
		}
		if !ok {
			if declaration.Required {
				return nil, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("required reusable-workflow input %q is missing", name))
			}
			value = zeroInputValue(declaration.Type)
		}
		if !inputTypeMatches(declaration.Type, value) {
			locationPath := path
			span := call.Span
			if supplied {
				span = call.Inputs[name].Span
			} else if declaration.Default != nil {
				locationPath = call.Uses
				span = declaration.Default.Span
			}
			return nil, locatedJobError(locationPath, job, span.Start.Line, span.Start.Column, fmt.Sprintf("reusable-workflow input %q must be %s", name, declaration.Type))
		}
		if containsExpression(value) {
			return nil, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("reusable-workflow input %q is not statically resolvable", name))
		}
		resolved[name] = value
	}
	return resolved, nil
}

func evaluateStaticCallValue(value string, inputs, matrix map[string]any, context expression.CompileContext) (any, error) {
	if match := staticValueExpression.FindStringSubmatch(value); match != nil {
		values := inputs
		if strings.EqualFold(match[1], "matrix") {
			values = matrix
		}
		valueName := match[2]
		if valueName == "" {
			valueName = match[3]
		}
		for name, value := range values {
			if strings.EqualFold(name, valueName) {
				if containsExpression(value) {
					return nil, fmt.Errorf("expression references runtime-dependent %s value %q", match[1], valueName)
				}
				return value, nil
			}
		}
		return nil, fmt.Errorf("expression references unavailable %s value %q", match[1], valueName)
	}
	resolved := replaceStaticInputs(value, inputs)
	if hasInputExpression(resolved) {
		return nil, fmt.Errorf("expression references an unavailable or unsupported input")
	}
	context.Matrix = matrix
	if expr, err := expression.Parse(resolved, 1, 1); err == nil {
		return expression.EvaluateCompile(expr, context)
	}
	return expression.EvaluateCompileTemplate(resolved, context)
}

func containsExpression(value any) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, "${{")
	case []any:
		for _, element := range value {
			if containsExpression(element) {
				return true
			}
		}
	case map[string]any:
		for _, element := range value {
			if containsExpression(element) {
				return true
			}
		}
	}
	return false
}

func sortedValueKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func zeroInputValue(inputType string) any {
	if inputType == "boolean" {
		return false
	}
	if inputType == "number" {
		return 0
	}
	return ""
}

func inputTypeMatches(inputType string, value any) bool {
	switch inputType {
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case int, int64, uint64, float64, json.Number:
			return true
		default:
			return false
		}
	case "string":
		_, ok := value.(string)
		return ok
	default:
		return false
	}
}

func applyStaticInputs(path string, job workflow.Job, inputs map[string]any) (workflow.Job, error) {
	if len(inputs) == 0 {
		return job, nil
	}
	job.Name = replaceStaticInputs(job.Name, inputs)
	job.If = replaceStaticInputCondition(job.If, inputs)
	job.DefaultShell = replaceStaticInputs(job.DefaultShell, inputs)
	job.DefaultWorkingDirectory = replaceStaticInputs(job.DefaultWorkingDirectory, inputs)
	if job.Concurrency != nil {
		concurrency := *job.Concurrency
		concurrency.Group = replaceStaticInputs(concurrency.Group, inputs)
		job.Concurrency = &concurrency
	}
	job.Env = replaceMapInputs(job.Env, inputs)
	job.Outputs = replaceMapInputs(job.Outputs, inputs)
	job.Services = append([]workflow.Service(nil), job.Services...)
	for i := range job.Services {
		container := job.Services[i].Container
		container.Image = replaceStaticInputs(container.Image, inputs)
		if container.Credentials != nil {
			credentials := *container.Credentials
			credentials.Username = replaceStaticInputs(credentials.Username, inputs)
			credentials.Password = replaceStaticInputs(credentials.Password, inputs)
			container.Credentials = &credentials
		}
		container.Env = replaceMapInputs(container.Env, inputs)
		container.Options = replaceStaticInputs(container.Options, inputs)
		container.Command = replaceStaticInputs(container.Command, inputs)
		container.Entrypoint = replaceStaticInputs(container.Entrypoint, inputs)
		container.Ports = replaceSliceInputs(container.Ports, inputs)
		container.Volumes = replaceSliceInputs(container.Volumes, inputs)
		job.Services[i].Container = container
	}
	job.RunsOn = append([]string(nil), job.RunsOn...)
	for i := range job.RunsOn {
		job.RunsOn[i] = replaceStaticInputs(job.RunsOn[i], inputs)
	}
	if job.RunsOnExpr != nil {
		resolved := replaceStaticInputs(job.RunsOnExpr.Text, inputs)
		if !strings.Contains(resolved, "${{") {
			job.RunsOn = []string{resolved}
			job.RunsOnExpr = nil
		} else {
			expr := *job.RunsOnExpr
			expr.Text = resolved
			job.RunsOnExpr = &expr
		}
	}
	if job.Matrix != nil {
		job.Matrix = cloneMatrixWithInputs(job.Matrix, inputs)
	}
	job.Steps = append([]workflow.Step(nil), job.Steps...)
	for i := range job.Steps {
		step := &job.Steps[i]
		step.Name = replaceStaticInputs(step.Name, inputs)
		step.Run = replaceStaticInputs(step.Run, inputs)
		step.Uses = replaceStaticInputs(step.Uses, inputs)
		step.Shell = replaceStaticInputs(step.Shell, inputs)
		step.WorkingDirectory = replaceStaticInputs(step.WorkingDirectory, inputs)
		if step.ContinueOnErrorExpression != "" {
			resolved, err := expression.SubstituteCompileInputs(step.ContinueOnErrorExpression, inputs)
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("resolve continue-on-error expression: %v", err))
			}
			if err := expression.ValidateStepControl(resolved); err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("validate continue-on-error expression: %v", err))
			}
			expr, err := expression.Parse(resolved, step.Span.Start.Line, step.Span.Start.Column)
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("parse continue-on-error expression: %v", err))
			}
			value, available, err := expression.EvaluateCompileAvailable(expr, expression.CompileContext{})
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("evaluate continue-on-error expression: %v", err))
			}
			if !available {
				step.ContinueOnErrorExpression = resolved
			} else if enabled, ok := value.(bool); ok {
				step.ContinueOnError, step.ContinueOnErrorExpression = enabled, ""
			} else {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "continue-on-error expression must produce a boolean")
			}
		}
		if step.TimeoutMinutesExpression != "" {
			resolved, err := expression.SubstituteCompileInputs(step.TimeoutMinutesExpression, inputs)
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("resolve timeout-minutes expression: %v", err))
			}
			if err := expression.ValidateStepControl(resolved); err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("validate timeout-minutes expression: %v", err))
			}
			expr, err := expression.Parse(resolved, step.Span.Start.Line, step.Span.Start.Column)
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("parse timeout-minutes expression: %v", err))
			}
			value, available, err := expression.EvaluateCompileAvailable(expr, expression.CompileContext{})
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("evaluate timeout-minutes expression: %v", err))
			}
			if !available {
				step.TimeoutMinutesExpression = resolved
			} else if minutes, ok := staticTimeoutMinutes(value); ok {
				if minutes <= 0 || minutes > 360 || math.IsNaN(minutes) || math.IsInf(minutes, 0) {
					return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "timeout-minutes expression must produce a number greater than 0 and at most 360")
				}
				step.TimeoutMinutes, step.TimeoutMinutesExpression = minutes, ""
			} else {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "timeout-minutes expression must produce a number")
			}
		}
		step.If = replaceStaticInputCondition(step.If, inputs)
		step.Env = replaceMapInputs(step.Env, inputs)
		step.With = replaceMapInputs(step.With, inputs)
	}
	return job, nil
}

func staticTimeoutMinutes(value any) (float64, bool) {
	switch value := value.(type) {
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint64:
		return float64(value), true
	case float64:
		return value, true
	case json.Number:
		minutes, err := value.Float64()
		return minutes, err == nil
	default:
		return 0, false
	}
}

func replaceSliceInputs(values []string, inputs map[string]any) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = replaceStaticInputs(value, inputs)
	}
	return result
}

func rejectUnresolvedInputExpressions(path string, job workflow.Job) error {
	jobValues := []string{job.Name, job.If, job.DefaultShell, job.DefaultWorkingDirectory}
	jobValues = appendMapValues(jobValues, job.Env)
	jobValues = appendMapValues(jobValues, job.Outputs)
	if job.Concurrency != nil {
		jobValues = append(jobValues, job.Concurrency.Group)
	}
	jobValues = append(jobValues, job.RunsOn...)
	for _, service := range job.Services {
		container := service.Container
		jobValues = append(jobValues, container.Image, container.Options, container.Command, container.Entrypoint)
		jobValues = append(jobValues, container.Ports...)
		jobValues = append(jobValues, container.Volumes...)
		jobValues = appendMapValues(jobValues, container.Env)
		if container.Credentials != nil {
			jobValues = append(jobValues, container.Credentials.Username, container.Credentials.Password)
		}
	}
	if job.RunsOnExpr != nil {
		jobValues = append(jobValues, job.RunsOnExpr.Text)
	}
	if job.Matrix != nil {
		if job.Matrix.Expression != nil {
			jobValues = append(jobValues, job.Matrix.Expression.Text)
		}
		for _, row := range job.Matrix.Rows {
			if row.Expression != nil {
				jobValues = append(jobValues, row.Expression.Text)
			}
			for _, value := range row.Values {
				if containsInputExpression(value.Data) {
					return locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, "reusable-workflow input expression is not statically resolvable")
				}
			}
		}
		for _, combinations := range [][]workflow.MatrixCombination{job.Matrix.Include, job.Matrix.Exclude} {
			for _, combination := range combinations {
				for _, value := range combination.Values {
					if containsInputExpression(value.Data) {
						return locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, "reusable-workflow input expression is not statically resolvable")
					}
				}
			}
		}
	}
	for _, value := range jobValues {
		if hasInputExpression(value) {
			return jobError(path, job, "reusable-workflow input expression is not statically resolvable")
		}
	}
	for _, step := range job.Steps {
		if hasInputExpression(step.Uses) {
			return locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "reusable-workflow action reference input expression is not statically resolvable")
		}
		stepValues := []string{step.Name, step.Run, step.Shell, step.WorkingDirectory, step.If, step.ContinueOnErrorExpression, step.TimeoutMinutesExpression}
		stepValues = appendMapValues(stepValues, step.Env)
		stepValues = appendMapValues(stepValues, step.With)
		for _, value := range stepValues {
			if hasInputExpression(value) {
				return locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "reusable-workflow input expression is not statically resolvable")
			}
		}
	}
	return nil
}

func rejectCallMatrixExpressions(path string, job workflow.Job) error {
	if job.Matrix == nil {
		return nil
	}
	if job.Matrix.Expression != nil {
		return locatedJobError(path, job, job.Matrix.Expression.Span.Start.Line, job.Matrix.Expression.Span.Start.Column, "expression-valued reusable-workflow matrices are unsupported")
	}
	for _, row := range job.Matrix.Rows {
		if row.Expression != nil {
			return locatedJobError(path, job, row.Expression.Span.Start.Line, row.Expression.Span.Start.Column, fmt.Sprintf("expression-valued reusable-workflow matrix dimension %q is unsupported", row.Name))
		}
		for _, value := range row.Values {
			if containsExpression(value.Data) {
				return locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, "runtime-dependent reusable-workflow matrix value is unsupported")
			}
		}
	}
	for _, combinations := range [][]workflow.MatrixCombination{job.Matrix.Include, job.Matrix.Exclude} {
		for _, combination := range combinations {
			for _, value := range combination.Values {
				if containsExpression(value.Data) {
					return locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, "runtime-dependent reusable-workflow matrix value is unsupported")
				}
			}
		}
	}
	return nil
}

func appendMapValues(out []string, values map[string]string) []string {
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func containsInputExpression(value any) bool {
	switch value := value.(type) {
	case string:
		return hasInputExpression(value)
	case []any:
		for _, element := range value {
			if containsInputExpression(element) {
				return true
			}
		}
	case map[string]any:
		for _, element := range value {
			if containsInputExpression(element) {
				return true
			}
		}
	}
	return false
}

func hasInputExpression(value string) bool {
	usesInputs, err := expression.TemplateUsesContext(value, "inputs")
	return err != nil || usesInputs
}

func cloneMatrixWithInputs(matrix *workflow.Matrix, inputs map[string]any) *workflow.Matrix {
	out := *matrix
	out.Rows = append([]workflow.MatrixRow(nil), matrix.Rows...)
	for i := range out.Rows {
		out.Rows[i].Values = append([]workflow.Value(nil), out.Rows[i].Values...)
		for j := range out.Rows[i].Values {
			if text, ok := out.Rows[i].Values[j].Data.(string); ok {
				out.Rows[i].Values[j].Data = replaceStaticInputs(text, inputs)
			}
		}
	}
	out.Include = cloneMatrixCombinations(matrix.Include, inputs)
	out.Exclude = cloneMatrixCombinations(matrix.Exclude, inputs)
	return &out
}

func cloneMatrixCombinations(combinations []workflow.MatrixCombination, inputs map[string]any) []workflow.MatrixCombination {
	out := make([]workflow.MatrixCombination, len(combinations))
	for i, combination := range combinations {
		out[i] = combination
		out[i].Values = make(map[string]workflow.Value, len(combination.Values))
		for name, value := range combination.Values {
			if text, ok := value.Data.(string); ok {
				value.Data = replaceStaticInputs(text, inputs)
			}
			out[i].Values[name] = value
		}
	}
	return out
}

func replaceMapInputs(values map[string]string, inputs map[string]any) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for name, value := range values {
		out[name] = replaceStaticInputs(value, inputs)
	}
	return out
}

func replaceStaticInputCondition(value string, inputs map[string]any) string {
	match := staticInputCondition.FindStringSubmatch(value)
	if match == nil {
		return replaceStaticInputs(value, inputs)
	}
	inputName := ""
	for _, candidate := range match[1:] {
		if candidate != "" {
			inputName = candidate
			break
		}
	}
	for name, input := range inputs {
		if strings.EqualFold(name, inputName) {
			if text, ok := input.(string); ok {
				return "'" + strings.ReplaceAll(text, "'", "''") + "'"
			}
			return fmt.Sprint(input)
		}
	}
	return value
}

func replaceStaticInputs(value string, inputs map[string]any) string {
	resolved, err := expression.SubstituteCompileInputs(value, inputs)
	if err != nil || resolved == value {
		return value
	}
	if evaluated, err := expression.EvaluateAvailableCompileTemplate(resolved, expression.CompileContext{}); err == nil {
		return evaluated
	}
	return value
}

func isRepositoryWorkflowPath(path string) bool {
	workflowDir := filepath.Dir(filepath.Clean(path))
	return filepath.Base(workflowDir) == "workflows" && filepath.Base(filepath.Dir(workflowDir)) == ".github"
}

func workflowRepository(path string) (string, string, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", fmt.Errorf("resolve workflow path: %w", err)
	}
	workflowDir := filepath.Dir(absPath)
	if filepath.Base(workflowDir) != "workflows" || filepath.Base(filepath.Dir(workflowDir)) != ".github" {
		return "", "", fmt.Errorf("%s: local reusable workflows require the caller under .github/workflows", path)
	}
	repositoryDir := filepath.Dir(filepath.Dir(workflowDir))
	root, err := filepath.EvalSymlinks(repositoryDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository root for %q: %w", path, err)
	}
	canonicalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("resolve workflow path %q: %w", path, err)
		}
		relative, relativeErr := filepath.Rel(repositoryDir, absPath)
		if relativeErr != nil {
			return "", "", fmt.Errorf("resolve workflow path %q: %w", path, relativeErr)
		}
		canonicalPath = filepath.Join(root, relative)
	}
	if err := requireWithinRepository(root, canonicalPath); err != nil {
		return "", "", fmt.Errorf("resolve workflow path %q: %w", path, err)
	}
	return root, canonicalPath, nil
}

func repositoryWorkflowPath(root, canonicalPath string) (string, error) {
	relative, err := filepath.Rel(root, canonicalPath)
	if err != nil {
		return "", fmt.Errorf("locate workflow %q in repository: %w", canonicalPath, err)
	}
	if err := requireWithinRepository(root, canonicalPath); err != nil {
		return "", fmt.Errorf("locate workflow %q in repository: %w", canonicalPath, err)
	}
	return "./" + filepath.ToSlash(relative), nil
}

func (resolver *reusableResolver) localWorkflowPath(uses string) (string, error) {
	if !strings.HasPrefix(uses, "./") {
		return "", fmt.Errorf("reusable workflow %q is remote or runtime-dependent; only repository-local ./ paths are supported", uses)
	}
	relative := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(uses, "./")))
	if filepath.Dir(relative) != filepath.Join(".github", "workflows") {
		return "", fmt.Errorf("local reusable workflow %q must name a file directly under .github/workflows", uses)
	}
	candidate := filepath.Join(resolver.root, relative)
	if err := requireWithinRepository(resolver.root, candidate); err != nil {
		return "", fmt.Errorf("resolve local reusable workflow %q: %w", uses, err)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve local reusable workflow %q: %w", uses, err)
	}
	if err := requireWithinRepository(resolver.root, resolved); err != nil {
		return "", fmt.Errorf("resolve local reusable workflow %q: %w", uses, err)
	}
	return resolved, nil
}

func requireWithinRepository(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved path %q escapes repository root %q", path, root)
	}
	return nil
}

func (resolver *reusableResolver) cycle(path string) string {
	for i, existing := range resolver.stack {
		if existing == path {
			chain := append(append([]string(nil), resolver.stack[i:]...), path)
			for j := range chain {
				chain[j] = filepath.ToSlash(strings.TrimPrefix(chain[j], resolver.root+string(filepath.Separator)))
			}
			return strings.Join(chain, " -> ")
		}
	}
	return ""
}

func matrixDigest(matrix map[string]any) (string, error) {
	canonical, err := json.Marshal(matrix)
	if err != nil {
		return "", err
	}
	digest := sha256Sum(canonical)
	return digest[:12], nil
}

func sha256Sum(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("%x", digest[:])
}
