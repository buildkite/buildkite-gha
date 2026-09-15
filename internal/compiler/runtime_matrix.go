package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	RuntimeMatrixSchemaV1 = "https://buildkite.com/schemas/buildkite-gha/runtime-matrix-v1.schema.json"

	RuntimeMatrixShapeObject  = "matrix"
	RuntimeMatrixShapeInclude = "include"

	MaxRuntimeMatrixBytes            = 64 * 1024
	MaxRuntimeMatrixInstances        = maxMatrixInstances
	MaxRuntimeMatrixGraphJobs        = maxFlattenedJobs
	MaxRuntimeMatrixProperties       = 64
	MaxRuntimeMatrixStringBytes      = 1024
	MaxRuntimeMatrixDescriptorBytes  = 16 * 1024
	runtimeMatrixMaxSourceCoordinate = 1_000_000
)

// runtimeMatrixDeferredMessage is the report text for a matrix whose values
// come from a job output but which buildkite-gha cannot hand to a deferred
// pipeline upload. Detail names the requirement the workflow misses.
const runtimeMatrixDeferredMessage = "matrix values come from a job output that exists only after that job runs; buildkite-gha expands such a matrix with a deferred pipeline upload, and this workflow does not meet its requirements (https://github.com/buildkite/buildkite-gha/blob/main/docs/compatibility.md#matrices-from-job-outputs)"

// RuntimeMatrixContinuation is one deferred pipeline upload recorded by the
// initial compilation: the consumer whose matrix comes from a producer job
// output plus every job that transitively depends on it. The deferred step
// runs after the producer, reads its verified output, and uploads the
// expanded jobs with deterministic keys.
type RuntimeMatrixContinuation struct {
	Descriptor RuntimeMatrixDescriptor `json:"descriptor"`
	// StepKey is the deterministic key of the deferred upload step.
	StepKey string `json:"step_key"`
	// ProducerStepKey is the generated step key of the producer's single
	// static instance, including the workflow's namespace and, when the
	// producer has a one-row matrix, its matrix digest.
	ProducerStepKey string `json:"producer_step_key"`
	// Jobs lists the deferred logical job IDs in topological order, starting
	// with the consumer.
	Jobs []string `json:"jobs"`
	// Labels maps each deferred job ID to its display name without matrix
	// values, as a static instance of the same job would be labelled.
	Labels map[string]string `json:"labels"`
	// Instances lists, for each deferred dependent, the instances its static
	// matrix expands to; a matrix-free dependent has one under its logical
	// key. The continuation reserves their step keys and, when the producer
	// does not succeed, skips each of them so every promised check resolves.
	// The consumer's instances are unknown until the producer runs, so it is
	// absent here.
	Instances map[string][]RuntimeMatrixInstance `json:"instances,omitempty"`
	// JobBudget is the most jobs this continuation may upload: consumer
	// instances plus dependent instances. The initial compilation divides the
	// jobs MaxRuntimeMatrixGraphJobs leaves after the static graph equally
	// between the workflow's continuations, so the deferred uploads together
	// cannot grow the build past that bound whatever the producers publish.
	JobBudget int `json:"job_budget"`
	// Sources records, for every deferred job, the workflow file it came from
	// as the initial compilation resolved it, including the commit a remote
	// reusable workflow reference was pinned to. The deferred upload has no
	// earlier plan to compare a deferred job against, so it compares these
	// instead and refuses to upload a job whose source moved.
	Sources map[string]RuntimeMatrixJobSource `json:"sources,omitempty"`
	// ActionLocks pins every action the deferred jobs use, including the
	// children of composite actions, to the commit and source tree the
	// initial compilation resolved. The deferred jobs have no plan until the
	// continuation runs, so the deferred upload resolves their actions
	// against these locks and refuses a plan whose locks differ. It is empty
	// when the compilation did not resolve remote actions.
	ActionLocks []plan.ActionLock `json:"action_locks,omitempty"`
}

// DependentInstances counts the jobs the continuation uploads regardless of the
// producer's output: one per statically known instance of every deferred
// dependent. The consumer adds one job per matrix row on top of these.
func (continuation RuntimeMatrixContinuation) DependentInstances() int {
	count := 0
	for _, instances := range continuation.Instances {
		count += len(instances)
	}
	return count
}

// deferredAction is one `uses` step of a deferred job with the repository root
// its local action paths resolve against. It exists only inside one
// compilation, to resolve the continuation's action locks.
type deferredAction struct {
	job, path, workspace, uses string
	with                       map[string]string
	step                       int
	position                   workflow.Position
	blockerDetailUnsafe        bool
}

// deferredJobActions lists the `uses` steps of one deferred job.
func deferredJobActions(sourced sourcedJob) []deferredAction {
	var actions []deferredAction
	for i, step := range sourced.Steps {
		if step.Kind != "uses" || step.Uses == "" {
			continue
		}
		actions = append(actions, deferredAction{
			job: sourced.ID, path: sourced.path, workspace: sourced.root, uses: step.Uses, with: step.With, step: i + 1,
			position: step.Span.Start, blockerDetailUnsafe: sourced.blockerDetailUnsafe,
		})
	}
	return actions
}

// resolveContinuationActions resolves the actions of every deferred job and
// returns the continuations with their action locks recorded. A deferred
// job's action that cannot be resolved would otherwise fail only after the
// producer ran, so each failure is reported now as an action-resolution
// finding without an instance, which fails the whole workflow. It also
// reports whether any deferred action program reads the vars context: the
// deferred jobs have no plan yet, so ActionsReferenceVars could not find the
// reference in the bundle, and the importer must resolve the scopes before it
// records them for the continuation.
func resolveContinuationActions(ctx context.Context, ir IR, options Options) (continuations []RuntimeMatrixContinuation, referencesVars bool, err error) {
	if len(ir.Continuations) == 0 || !options.ResolveActions {
		return ir.Continuations, false, nil
	}
	continuations = slices.Clone(ir.Continuations)
	var diagnostics []error
	for i := range continuations {
		locks := map[string]plan.ActionLock{}
		for _, action := range ir.deferredActions[continuations[i].Descriptor.Job] {
			compiled, err := compileActionInvocations(ctx, action.workspace, options.ActionSource, plan.EventServerURL(ir.Event.Provider), []string{action.uses}, []map[string]string{action.with})
			if err != nil {
				message, detail, actionName := actionResolutionMessage(action.uses, err)
				blockerDetail := action.uses
				if action.blockerDetailUnsafe {
					blockerDetail = ""
				}
				diagnostics = append(diagnostics, &ProcessingFinding{
					Stage: StageResolution, Code: CodeActionResolution, Category: "action-resolution",
					Blocker: "action_ref", BlockerDetail: blockerDetail,
					Path: action.path, Line: action.position.Line, Column: action.position.Column,
					Job: action.job, Action: actionName, Step: action.step,
					Message: message, Detail: detail,
					Err: fmt.Errorf("%s:%d:%d: deferred job %q action %q at step %d: %w", action.path, action.position.Line, action.position.Column, action.job, action.uses, action.step, err),
				})
				continue
			}
			for _, lock := range compiled.locks {
				locks[lock.ID] = lock
			}
			for _, program := range compiled.programs {
				referencesVars = referencesVars || actionProgramReferencesVars(program)
			}
		}
		continuations[i].ActionLocks = make([]plan.ActionLock, 0, len(locks))
		for _, id := range sortedKeys(locks) {
			continuations[i].ActionLocks = append(continuations[i].ActionLocks, locks[id])
		}
		if len(continuations[i].ActionLocks) == 0 {
			continuations[i].ActionLocks = nil
		}
	}
	return continuations, referencesVars, errors.Join(diagnostics...)
}

// RuntimeMatrixJobSource is the workflow file a deferred job was compiled
// from, with the same identity a plan records for a static job.
type RuntimeMatrixJobSource struct {
	Path   string                `json:"path"`
	Digest string                `json:"digest"`
	Remote *RemoteWorkflowSource `json:"remote,omitempty"`
}

// Matches reports whether an expanded job comes from the recorded source.
func (source RuntimeMatrixJobSource) Matches(instance JobInstance) bool {
	return source.Path == instance.SourcePath && source.Digest == instance.SourceDigest && reflect.DeepEqual(source.Remote, instance.RemoteWorkflow)
}

// RuntimeMatrixInstance is one statically known instance of a deferred job.
type RuntimeMatrixInstance struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	CheckLabel string `json:"check_label"`
}

// JobLabel returns the display name of a deferred job, or its ID when the
// continuation carries no label for it.
func (c RuntimeMatrixContinuation) JobLabel(jobID string) string {
	if label := c.Labels[jobID]; label != "" {
		return label
	}
	return jobID
}

// continuationStepKey derives the deferred upload step key for one consumer.
// The suffix keeps it apart from the consumer's own instance keys, which
// always carry a matrix digest.
func continuationStepKey(namespace, consumer string) string {
	key, _ := namespacedInstanceKey(namespace, consumer, nil)
	return key + "-matrix"
}

// LogicalJobStepKey returns the step key of a job without matrix instances.
// The continuation uses it to upload skip steps for deferred jobs when the
// producer did not succeed.
func LogicalJobStepKey(namespace, jobID string) string {
	key, _ := namespacedInstanceKey(namespace, jobID, nil)
	return key
}

var runtimeMatrixLogicalJobPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:[.][A-Za-z0-9_-]+)*$`)
var runtimeMatrixStepKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var runtimeMatrixDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var runtimeMatrixKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,254}$`)

// RuntimeMatrixDescriptor is a compile-time-only description of one validated
// deferred matrix boundary. It is not an executable plan or upload authority.
type RuntimeMatrixDescriptor struct {
	Schema          string        `json:"schema"`
	Job             string        `json:"job"`
	Shape           string        `json:"shape"`
	ProducerJob     string        `json:"producer_job"`
	ProducerStepKey string        `json:"producer_step_key"`
	ProducerOutput  string        `json:"producer_output"`
	SourcePath      string        `json:"source_path"`
	SourceDigest    string        `json:"source_digest"`
	Source          workflow.Span `json:"source"`
	MaxOutputBytes  int           `json:"max_output_bytes"`
	MaxInstances    int           `json:"max_instances"`
	MaxGraphJobs    int           `json:"max_graph_jobs"`
}

func hasRuntimeMatrixBoundary(parsed *workflow.Workflow) bool {
	for _, job := range parsed.Jobs {
		if job.Matrix == nil {
			continue
		}
		for _, candidate := range []*expression.Expression{job.Matrix.Expression, job.Matrix.IncludeExpression, job.Matrix.ExcludeExpression} {
			if candidate == nil {
				continue
			}
			if _, err := expression.RuntimeMatrixOutput(*candidate); err == nil {
				return true
			}
		}
	}
	return false
}

func describeRuntimeMatrix(job workflow.Job, sourcePath, sourceDigest string, needs map[string]needBinding, jobs map[string]workflow.Job, matricesByJob map[string][]map[string]any) (RuntimeMatrixDescriptor, bool, error) {
	if job.Matrix == nil {
		return RuntimeMatrixDescriptor{}, false, nil
	}
	shape := ""
	expr := job.Matrix.Expression
	if expr != nil {
		shape = RuntimeMatrixShapeObject
	} else if job.Matrix.IncludeExpression != nil {
		shape = RuntimeMatrixShapeInclude
		expr = job.Matrix.IncludeExpression
	}
	if expr == nil {
		return RuntimeMatrixDescriptor{}, false, nil
	}
	reference, err := expression.RuntimeMatrixOutput(*expr)
	if err != nil {
		return RuntimeMatrixDescriptor{}, false, nil
	}
	if shape == RuntimeMatrixShapeInclude && (len(job.Matrix.Rows) != 0 || len(job.Matrix.Include) != 0 || len(job.Matrix.Exclude) != 0 || job.Matrix.ExcludeExpression != nil) {
		return RuntimeMatrixDescriptor{}, true, errors.New("runtime matrix include output must be the complete matrix definition")
	}

	binding, err := exactRuntimeMatrixNeed(needs, reference.Job)
	if err != nil {
		return RuntimeMatrixDescriptor{}, true, err
	}
	producerID := ""
	requestedOutput := reference.Output
	if binding.projectOutputs {
		producerID, requestedOutput, err = exactRuntimeMatrixProjectedOutput(binding, reference.Output)
		if err != nil {
			return RuntimeMatrixDescriptor{}, true, fmt.Errorf("runtime matrix producer %q: %w", reference.Job, err)
		}
	} else {
		if len(binding.members) != 1 {
			return RuntimeMatrixDescriptor{}, true, fmt.Errorf("runtime matrix producer %q must resolve to exactly one job", reference.Job)
		}
		producerID = binding.members[0]
	}
	producer, exists := jobs[producerID]
	if !exists {
		return RuntimeMatrixDescriptor{}, true, fmt.Errorf("runtime matrix producer %q is unavailable after graph resolution", reference.Job)
	}
	output, err := exactRuntimeMatrixOutput(producer.Outputs, requestedOutput)
	if err != nil {
		return RuntimeMatrixDescriptor{}, true, fmt.Errorf("runtime matrix producer %q: %w", producerID, err)
	}
	producerMatrices, exists := matricesByJob[producerID]
	if !exists || len(producerMatrices) != 1 {
		return RuntimeMatrixDescriptor{}, true, fmt.Errorf("runtime matrix producer %q must have exactly one statically expanded instance", producerID)
	}
	producerStepKey, err := instanceKey(producerID, producerMatrices[0])
	if err != nil {
		return RuntimeMatrixDescriptor{}, true, fmt.Errorf("runtime matrix producer %q: %w", producerID, err)
	}
	descriptor := RuntimeMatrixDescriptor{
		Schema:          RuntimeMatrixSchemaV1,
		Job:             job.ID,
		Shape:           shape,
		ProducerJob:     producerID,
		ProducerStepKey: producerStepKey,
		ProducerOutput:  output,
		SourcePath:      sourcePath,
		SourceDigest:    sourceDigest,
		Source:          workflow.Span{Start: workflow.Position{Line: expr.Span.Start.Line, Column: expr.Span.Start.Column}, End: workflow.Position{Line: expr.Span.End.Line, Column: expr.Span.End.Column}},
		MaxOutputBytes:  MaxRuntimeMatrixBytes,
		MaxInstances:    MaxRuntimeMatrixInstances,
		MaxGraphJobs:    MaxRuntimeMatrixGraphJobs,
	}
	if err := descriptor.Validate(); err != nil {
		return RuntimeMatrixDescriptor{}, true, err
	}
	return descriptor, true, nil
}

func exactRuntimeMatrixNeed(needs map[string]needBinding, requested string) (needBinding, error) {
	var matched *needBinding
	for _, need := range sortedKeys(needs) {
		if !strings.EqualFold(need, requested) {
			continue
		}
		if matched != nil {
			return needBinding{}, fmt.Errorf("runtime matrix producer %q is ambiguous", requested)
		}
		binding := needs[need]
		matched = &binding
	}
	if matched == nil {
		return needBinding{}, fmt.Errorf("runtime matrix producer %q must be a direct prerequisite in needs", requested)
	}
	return *matched, nil
}

func exactRuntimeMatrixProjectedOutput(binding needBinding, requested string) (string, string, error) {
	var matched *needOutputBinding
	for i := range binding.outputs {
		if !strings.EqualFold(binding.outputs[i].name, requested) {
			continue
		}
		if matched != nil {
			return "", "", fmt.Errorf("output %q is ambiguous", requested)
		}
		matched = &binding.outputs[i]
	}
	if matched == nil {
		return "", "", fmt.Errorf("output %q is not declared", requested)
	}
	return matched.member, matched.output, nil
}

func exactRuntimeMatrixOutput(outputs map[string]string, requested string) (string, error) {
	matched := ""
	for output := range outputs {
		if !strings.EqualFold(output, requested) {
			continue
		}
		if matched != "" {
			return "", fmt.Errorf("output %q is ambiguous", requested)
		}
		matched = output
	}
	if matched == "" {
		return "", fmt.Errorf("output %q is not declared", requested)
	}
	return matched, nil
}

// Validate enforces the immutable v1 descriptor meaning.
func (descriptor RuntimeMatrixDescriptor) Validate() error {
	if descriptor.Schema != RuntimeMatrixSchemaV1 {
		return fmt.Errorf("unsupported runtime matrix schema %q", descriptor.Schema)
	}
	if len(descriptor.Job) > 255 || len(descriptor.ProducerJob) > 255 || !runtimeMatrixLogicalJobPattern.MatchString(descriptor.Job) || !runtimeMatrixLogicalJobPattern.MatchString(descriptor.ProducerJob) || strings.EqualFold(descriptor.Job, descriptor.ProducerJob) {
		return errors.New("runtime matrix requires distinct valid consumer and producer jobs")
	}
	if descriptor.Shape != RuntimeMatrixShapeObject && descriptor.Shape != RuntimeMatrixShapeInclude {
		return fmt.Errorf("unsupported runtime matrix shape %q", descriptor.Shape)
	}
	if !runtimeMatrixStepKeyPattern.MatchString(descriptor.ProducerStepKey) || !runtimeMatrixStepKeyPattern.MatchString(descriptor.ProducerOutput) {
		return errors.New("runtime matrix requires a valid producer step and output")
	}
	if descriptor.SourcePath == "" || len(descriptor.SourcePath) > 1024 || !utf8.ValidString(descriptor.SourcePath) || strings.ContainsRune(descriptor.SourcePath, '\x00') || !runtimeMatrixDigestPattern.MatchString(descriptor.SourceDigest) {
		return errors.New("runtime matrix requires a bounded source path and digest")
	}
	if descriptor.Source.Start.Line < 1 || descriptor.Source.Start.Line > runtimeMatrixMaxSourceCoordinate ||
		descriptor.Source.Start.Column < 1 || descriptor.Source.Start.Column > runtimeMatrixMaxSourceCoordinate ||
		descriptor.Source.End.Line < descriptor.Source.Start.Line || descriptor.Source.End.Line > runtimeMatrixMaxSourceCoordinate ||
		descriptor.Source.End.Column < 1 || descriptor.Source.End.Column > runtimeMatrixMaxSourceCoordinate ||
		descriptor.Source.End.Line == descriptor.Source.Start.Line && descriptor.Source.End.Column < descriptor.Source.Start.Column {
		return errors.New("runtime matrix requires a valid source span")
	}
	if descriptor.MaxOutputBytes != MaxRuntimeMatrixBytes || descriptor.MaxInstances != MaxRuntimeMatrixInstances || descriptor.MaxGraphJobs != MaxRuntimeMatrixGraphJobs {
		return errors.New("runtime matrix v1 limits do not match the immutable schema")
	}
	return nil
}

// EncodeRuntimeMatrixDescriptor returns deterministic descriptor JSON.
func EncodeRuntimeMatrixDescriptor(descriptor RuntimeMatrixDescriptor) ([]byte, error) {
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(descriptor); err != nil {
		return nil, fmt.Errorf("encode runtime matrix descriptor: %w", err)
	}
	if out.Len() > MaxRuntimeMatrixDescriptorBytes {
		return nil, fmt.Errorf("runtime matrix descriptor is %d bytes, maximum is %d", out.Len(), MaxRuntimeMatrixDescriptorBytes)
	}
	return out.Bytes(), nil
}

// DecodeRuntimeMatrixDescriptor rejects unknown, duplicate, and trailing data.
func DecodeRuntimeMatrixDescriptor(source []byte) (RuntimeMatrixDescriptor, error) {
	if len(source) > MaxRuntimeMatrixDescriptorBytes {
		return RuntimeMatrixDescriptor{}, fmt.Errorf("runtime matrix descriptor is %d bytes, maximum is %d", len(source), MaxRuntimeMatrixDescriptorBytes)
	}
	if !utf8.Valid(source) {
		return RuntimeMatrixDescriptor{}, errors.New("runtime matrix descriptor is not valid UTF-8")
	}
	if err := rejectRuntimeMatrixDuplicateKeys(source); err != nil {
		return RuntimeMatrixDescriptor{}, fmt.Errorf("decode runtime matrix descriptor: %w", err)
	}
	if err := rejectRuntimeMatrixDescriptorKeyAliases(source); err != nil {
		return RuntimeMatrixDescriptor{}, fmt.Errorf("decode runtime matrix descriptor: %w", err)
	}
	var descriptor RuntimeMatrixDescriptor
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return RuntimeMatrixDescriptor{}, fmt.Errorf("decode runtime matrix descriptor: %w", err)
	}
	if err := requireRuntimeMatrixEOF(decoder); err != nil {
		return RuntimeMatrixDescriptor{}, fmt.Errorf("decode runtime matrix descriptor: %w", err)
	}
	if err := descriptor.Validate(); err != nil {
		return RuntimeMatrixDescriptor{}, err
	}
	return descriptor, nil
}

func rejectRuntimeMatrixDescriptorKeyAliases(source []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(source, &document); err != nil {
		return err
	}
	topLevel := map[string]bool{
		"schema": true, "job": true, "shape": true, "producer_job": true,
		"producer_step_key": true, "producer_output": true, "source_path": true,
		"source_digest": true, "source": true, "max_output_bytes": true,
		"max_instances": true, "max_graph_jobs": true,
	}
	for _, key := range sortedKeys(document) {
		if !topLevel[key] {
			return fmt.Errorf("unknown JSON field %q", key)
		}
	}

	var span map[string]json.RawMessage
	if err := json.Unmarshal(document["source"], &span); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	for _, key := range sortedKeys(span) {
		if key != "start" && key != "end" {
			return fmt.Errorf("unknown JSON field %q in source", key)
		}
	}
	for _, name := range []string{"start", "end"} {
		var position map[string]json.RawMessage
		if err := json.Unmarshal(span[name], &position); err != nil {
			return fmt.Errorf("source.%s: %w", name, err)
		}
		for _, key := range sortedKeys(position) {
			if key != "line" && key != "column" {
				return fmt.Errorf("unknown JSON field %q in source.%s", key, name)
			}
		}
	}
	return nil
}

// ExpandRuntimeMatrixOutput strictly decodes one untrusted producer output and
// expands only matrix values. It does not construct plans or upload a pipeline.
func ExpandRuntimeMatrixOutput(descriptor RuntimeMatrixDescriptor, source []byte, existingStepKeys []string) ([]map[string]any, error) {
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	if len(existingStepKeys) > MaxRuntimeMatrixGraphJobs {
		return nil, fmt.Errorf("existing graph has %d jobs, maximum is %d", len(existingStepKeys), MaxRuntimeMatrixGraphJobs)
	}
	existingKeys := make(map[string]struct{}, len(existingStepKeys))
	for _, key := range existingStepKeys {
		if _, exists := existingKeys[key]; exists {
			return nil, fmt.Errorf("existing graph contains duplicate step key %q", key)
		}
		existingKeys[key] = struct{}{}
	}
	if len(source) > MaxRuntimeMatrixBytes {
		return nil, fmt.Errorf("runtime matrix output is %d bytes, maximum is %d", len(source), MaxRuntimeMatrixBytes)
	}
	if !utf8.Valid(source) {
		return nil, errors.New("runtime matrix output is not valid UTF-8")
	}
	if err := rejectRuntimeMatrixDuplicateKeys(source); err != nil {
		return nil, fmt.Errorf("decode runtime matrix output: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode runtime matrix output: %w", err)
	}
	if err := requireRuntimeMatrixEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode runtime matrix output: %w", err)
	}

	var matrix *workflow.Matrix
	var emptyInclude bool
	var err error
	switch descriptor.Shape {
	case RuntimeMatrixShapeInclude:
		var include []workflow.MatrixCombination
		include, err = runtimeMatrixCombinations("include", value)
		matrix = &workflow.Matrix{Include: include}
		emptyInclude = len(include) == 0
	case RuntimeMatrixShapeObject:
		matrix, emptyInclude, err = runtimeMatrixFromObject(value)
	}
	if err != nil {
		return nil, err
	}
	if emptyInclude && len(matrix.Rows) == 0 {
		return []map[string]any{}, nil
	}
	matrices, err := expandMatrixDefinition(matrix)
	if err != nil {
		return nil, err
	}
	if len(matrices) > MaxRuntimeMatrixInstances {
		return nil, fmt.Errorf("runtime matrix expands beyond %d instances", MaxRuntimeMatrixInstances)
	}
	if len(existingStepKeys)+len(matrices) > MaxRuntimeMatrixGraphJobs {
		return nil, fmt.Errorf("runtime continuation expands graph beyond %d jobs", MaxRuntimeMatrixGraphJobs)
	}
	seenValues := make(map[string]struct{}, len(matrices))
	seenKeys := make(map[string]struct{}, len(matrices))
	canonicalKeys := make(map[string]string)
	for _, matrix := range matrices {
		if err := validateExpandedRuntimeMatrix(matrix, canonicalKeys); err != nil {
			return nil, err
		}
		canonical, err := json.Marshal(matrix)
		if err != nil {
			return nil, fmt.Errorf("canonicalize runtime matrix instance: %w", err)
		}
		if _, exists := seenValues[string(canonical)]; exists {
			return nil, errors.New("runtime matrix contains duplicate instances")
		}
		seenValues[string(canonical)] = struct{}{}
		key, err := instanceKey(descriptor.Job, matrix)
		if err != nil {
			return nil, err
		}
		if _, exists := existingKeys[key]; exists {
			return nil, fmt.Errorf("runtime matrix deterministic instance key %q collides with the existing graph", key)
		}
		if _, exists := seenKeys[key]; exists {
			return nil, fmt.Errorf("runtime matrix deterministic instance key %q collides", key)
		}
		seenKeys[key] = struct{}{}
	}
	return matrices, nil
}

func validateExpandedRuntimeMatrix(matrix map[string]any, canonicalKeys map[string]string) error {
	if len(matrix) > MaxRuntimeMatrixProperties {
		return fmt.Errorf("runtime matrix instance has more than %d properties", MaxRuntimeMatrixProperties)
	}
	seen := make(map[string]string, len(matrix))
	for key := range matrix {
		lower := strings.ToLower(key)
		if prior, exists := seen[lower]; exists {
			return fmt.Errorf("runtime matrix instance contains ambiguous properties %q and %q", prior, key)
		}
		seen[lower] = key
		if prior, exists := canonicalKeys[lower]; exists && prior != key {
			return fmt.Errorf("runtime matrix instances use ambiguous property spellings %q and %q", prior, key)
		}
		canonicalKeys[lower] = key
	}
	return nil
}

func runtimeMatrixFromObject(value any) (*workflow.Matrix, bool, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("runtime matrix object resolved to %T, want object", value)
	}
	if len(object) == 0 || len(object) > MaxRuntimeMatrixProperties {
		return nil, false, fmt.Errorf("runtime matrix object must contain between 1 and %d properties", MaxRuntimeMatrixProperties)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := make(map[string]string, len(keys))
	matrix := &workflow.Matrix{}
	includePresent := false
	for _, key := range keys {
		if err := validateRuntimeMatrixKey(key); err != nil {
			return nil, false, err
		}
		lower := strings.ToLower(key)
		if prior, exists := seen[lower]; exists {
			return nil, false, fmt.Errorf("runtime matrix contains ambiguous properties %q and %q", prior, key)
		}
		seen[lower] = key
		switch lower {
		case "include", "exclude":
			if key != lower {
				return nil, false, fmt.Errorf("runtime matrix reserved property %q must be lowercase", key)
			}
			combinations, err := runtimeMatrixCombinations(key, object[key])
			if err != nil {
				return nil, false, err
			}
			if key == "include" {
				matrix.Include = combinations
				includePresent = true
			} else {
				matrix.Exclude = combinations
			}
		default:
			values, ok := object[key].([]any)
			if !ok {
				return nil, false, fmt.Errorf("runtime matrix dimension %q resolved to %T, want array", key, object[key])
			}
			if len(values) == 0 || len(values) > MaxRuntimeMatrixInstances {
				return nil, false, fmt.Errorf("runtime matrix dimension %q must contain between 1 and %d values", key, MaxRuntimeMatrixInstances)
			}
			row := workflow.MatrixRow{Name: key, Values: make([]workflow.Value, len(values))}
			for i, value := range values {
				scalar, err := runtimeMatrixScalar(value)
				if err != nil {
					return nil, false, fmt.Errorf("runtime matrix dimension %q value %d: %w", key, i+1, err)
				}
				row.Values[i] = workflow.Value{Data: scalar}
			}
			matrix.Rows = append(matrix.Rows, row)
		}
	}
	if err := validateRuntimeMatrixCombinationDimensions(matrix.Rows, matrix.Include, "include"); err != nil {
		return nil, false, err
	}
	if err := validateRuntimeMatrixCombinationDimensions(matrix.Rows, matrix.Exclude, "exclude"); err != nil {
		return nil, false, err
	}
	return matrix, includePresent && len(matrix.Include) == 0, nil
}

func validateRuntimeMatrixCombinationDimensions(rows []workflow.MatrixRow, combinations []workflow.MatrixCombination, name string) error {
	dimensions := make(map[string]string, len(rows))
	for _, row := range rows {
		dimensions[strings.ToLower(row.Name)] = row.Name
	}
	for i, combination := range combinations {
		for key := range combination.Values {
			if dimension, exists := dimensions[strings.ToLower(key)]; exists && dimension != key {
				return fmt.Errorf("runtime matrix %s entry %d property %q is ambiguous with dimension %q", name, i+1, key, dimension)
			}
		}
	}
	return nil
}

func runtimeMatrixCombinations(name string, value any) ([]workflow.MatrixCombination, error) {
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("runtime matrix %s resolved to %T, want array", name, value)
	}
	if len(values) > MaxRuntimeMatrixInstances {
		return nil, fmt.Errorf("runtime matrix %s has more than %d entries", name, MaxRuntimeMatrixInstances)
	}
	combinations := make([]workflow.MatrixCombination, len(values))
	for i, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("runtime matrix %s entry %d resolved to %T, want object", name, i+1, value)
		}
		if len(object) == 0 || len(object) > MaxRuntimeMatrixProperties {
			return nil, fmt.Errorf("runtime matrix %s entry %d must contain between 1 and %d properties", name, i+1, MaxRuntimeMatrixProperties)
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		seen := make(map[string]string, len(keys))
		combination := workflow.MatrixCombination{Values: make(map[string]workflow.Value, len(keys))}
		for _, key := range keys {
			if err := validateRuntimeMatrixKey(key); err != nil {
				return nil, err
			}
			lower := strings.ToLower(key)
			if prior, exists := seen[lower]; exists {
				return nil, fmt.Errorf("runtime matrix %s entry %d contains ambiguous properties %q and %q", name, i+1, prior, key)
			}
			seen[lower] = key
			scalar, err := runtimeMatrixScalar(object[key])
			if err != nil {
				return nil, fmt.Errorf("runtime matrix %s entry %d property %q: %w", name, i+1, key, err)
			}
			combination.Values[key] = workflow.Value{Data: scalar}
		}
		combinations[i] = combination
	}
	return combinations, nil
}

func runtimeMatrixScalar(value any) (any, error) {
	switch value := value.(type) {
	case string:
		if len(value) > MaxRuntimeMatrixStringBytes {
			return nil, fmt.Errorf("string is %d bytes, maximum is %d", len(value), MaxRuntimeMatrixStringBytes)
		}
		return value, nil
	case bool:
		return value, nil
	case json.Number:
		if len(value) > 64 {
			return nil, errors.New("number exceeds 64 bytes")
		}
		number, err := strconv.ParseFloat(string(value), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, fmt.Errorf("number %q is outside the supported finite range", value)
		}
		canonical, err := canonicalRuntimeMatrixNumber(value)
		if err != nil {
			return nil, fmt.Errorf("number %q is outside the supported finite range", value)
		}
		return canonical, nil
	case nil:
		return nil, errors.New("null values are unsupported")
	default:
		return nil, fmt.Errorf("nested %T values are unsupported", value)
	}
}

func canonicalRuntimeMatrixNumber(value json.Number) (json.Number, error) {
	text := value.String()
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign = "-"
		text = strings.TrimPrefix(text, "-")
	}
	exponent := int64(0)
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		parsed, err := strconv.ParseInt(text[index+1:], 10, 64)
		if err != nil {
			return "", err
		}
		exponent = parsed
		text = text[:index]
	}
	if index := strings.IndexByte(text, '.'); index >= 0 {
		fractionDigits := int64(len(text) - index - 1)
		if exponent < math.MinInt64+fractionDigits {
			return "", errors.New("number exponent underflows")
		}
		exponent -= fractionDigits
		text = text[:index] + text[index+1:]
	}
	digits := strings.TrimLeft(text, "0")
	if digits == "" {
		return json.Number("0"), nil
	}
	for strings.HasSuffix(digits, "0") {
		digits = strings.TrimSuffix(digits, "0")
		if exponent == math.MaxInt64 {
			return "", errors.New("number exponent overflows")
		}
		exponent++
	}
	if exponent == 0 {
		return json.Number(sign + digits), nil
	}
	return json.Number(sign + digits + "e" + strconv.FormatInt(exponent, 10)), nil
}

func validateRuntimeMatrixKey(key string) error {
	if !runtimeMatrixKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid runtime matrix key %q", key)
	}
	return nil
}

func requireRuntimeMatrixEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing JSON data")
	}
	return nil
}

func rejectRuntimeMatrixDuplicateKeys(source []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var check func() error
	check = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if err := check(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := check(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
		_, err = decoder.Token()
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return requireRuntimeMatrixEOF(decoder)
}
