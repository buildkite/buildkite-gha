// Package compiler expands an owned workflow into deterministic Phase 0 JSON IR.
package compiler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	schema             = "buildkite-gha/compiler-ir/v0"
	maxMatrixInstances = 256
)

// Event is the explicit provider event snapshot supplied to compilation.
type Event struct {
	Provider   string         `json:"provider"`
	Event      string         `json:"event"`
	Trust      EventTrust     `json:"trust"`
	Repository Repository     `json:"repository"`
	Ref        string         `json:"ref"`
	SHA        string         `json:"sha"`
	Actor      string         `json:"actor"`
	Payload    map[string]any `json:"payload"`
}

// Repository identifies the source repository in the event snapshot.
type Repository struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

// IR is the deterministic, actionlint-independent Phase 0 compiler output.
type IR struct {
	Schema    string            `json:"schema"`
	Workflow  WorkflowSource    `json:"workflow"`
	Event     Event             `json:"event"`
	Vars      map[string]string `json:"vars,omitempty"`
	Execution ExecutionBoundary `json:"execution"`
	Jobs      []JobInstance     `json:"jobs"`
}

// ExecutionBoundary makes the compile-only Phase 0 boundary explicit.
type ExecutionBoundary struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// WorkflowSource binds the IR to its input workflow bytes.
type WorkflowSource struct {
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest"`
}

// JobInstance is one statically expanded job in the owned IR.
type JobInstance struct {
	Key                     string            `json:"key"`
	LogicalJobID            string            `json:"logical_job_id"`
	Label                   string            `json:"label"`
	Needs                   []string          `json:"needs,omitempty"`
	LogicalNeeds            []string          `json:"logical_needs,omitempty"`
	RunsOn                  []string          `json:"runs_on"`
	Queue                   string            `json:"queue"`
	Matrix                  map[string]any    `json:"matrix,omitempty"`
	FailFast                *bool             `json:"fail_fast,omitempty"`
	MaxParallel             *int              `json:"max_parallel,omitempty"`
	Steps                   []workflow.Step   `json:"steps"`
	Env                     map[string]string `json:"env,omitempty"`
	If                      string            `json:"if,omitempty"`
	TimeoutMinutes          float64           `json:"timeout_minutes,omitempty"`
	DefaultShell            string            `json:"default_shell,omitempty"`
	DefaultWorkingDirectory string            `json:"default_working_directory,omitempty"`
	Outputs                 map[string]string `json:"outputs,omitempty"`
	SourcePath              string            `json:"source_path"`
	SourceDigest            string            `json:"source_digest"`
	RepositoryRoot          string            `json:"-"`
	Source                  workflow.Span     `json:"source"`
}

// Report summarizes successful workflow validation.
type Report struct {
	LogicalJobs int
	Instances   int
}

// Validate parses and validates the supported static graph without requiring an
// event snapshot.
func Validate(path string, source []byte) (Report, error) {
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return Report{}, err
	}
	options := defaultOptions()
	event := Event{Trust: options.EventTrust, Payload: map[string]any{}}
	instances, err := expand(path, source, parsed, compileContext(event, nil), options)
	if err != nil {
		return Report{}, err
	}
	return Report{LogicalJobs: len(parsed.Jobs), Instances: len(instances)}, nil
}

// ValidateEvent validates both the supported static graph and its event input.
func ValidateEvent(path string, source, eventSource []byte) (Report, error) {
	return ValidateEventWithOptions(path, source, eventSource, defaultOptions())
}

// ValidateEventWithOptions validates the graph against explicit variables and
// runner policy without producing compiler output.
func ValidateEventWithOptions(path string, source, eventSource []byte, options Options) (Report, error) {
	if err := options.validate(); err != nil {
		return Report{}, err
	}
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return Report{}, err
	}
	event, err := parseEvent(eventSource)
	if err != nil {
		return Report{}, err
	}
	event.Trust = options.EventTrust
	instances, err := expand(path, source, parsed, compileContext(event, options.Vars.snapshot()), options)
	if err != nil {
		return Report{}, err
	}
	return Report{LogicalJobs: len(parsed.Jobs), Instances: len(instances)}, nil
}

// Compile parses a workflow and event, expands its static graph, and returns
// stable JSON bytes terminated by a newline.
func Compile(path string, source, eventSource []byte) ([]byte, error) {
	return CompileWithOptions(path, source, eventSource, defaultOptions())
}

// CompileWithOptions compiles using an explicit non-secret variable snapshot,
// event trust classification, and runner policy.
func CompileWithOptions(path string, source, eventSource []byte, options Options) ([]byte, error) {
	ir, err := compile(path, source, eventSource, options)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(ir); err != nil {
		return nil, fmt.Errorf("encode compiler IR: %w", err)
	}
	return out.Bytes(), nil
}

// CompilePlans connects the owned compiler IR to one versioned plan per job instance.
func CompilePlans(path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, targetQueue string) ([]plan.Job, error) {
	if targetQueue != "gha-untrusted" {
		return nil, fmt.Errorf("unattested event snapshots may only target queue %q; use CompilePlansWithOptions for authenticated events", "gha-untrusted")
	}
	options := defaultOptions()
	return CompilePlansWithOptions(path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
}

// CompilePlansWithOptions creates one plan per job using compiler-selected
// queues and the same snapshotted vars used for graph construction.
func CompilePlansWithOptions(path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest string, options Options) ([]plan.Job, error) {
	return CompilePlansContext(context.Background(), path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
}

// CompilePlansContext creates one plan per job and permits cancellation while
// compilation resolves immutable public action source.
func CompilePlansContext(ctx context.Context, path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest string, options Options) ([]plan.Job, error) {
	if compilerVersion == "" {
		return nil, fmt.Errorf("compiler version is required")
	}
	if !strings.HasPrefix(compilerDistributionDigest, "sha256:") {
		return nil, fmt.Errorf("compiler distribution digest is required")
	}
	ir, err := compile(path, source, eventSource, options)
	if err != nil {
		return nil, err
	}
	return compilePlans(ctx, ir, compilerVersion, compilerDistributionDigest, options)
}

func compilePlans(ctx context.Context, ir IR, compilerVersion, compilerDistributionDigest string, options Options) ([]plan.Job, error) {
	plans, _, err := compilePlansWithAuthorization(ctx, ir, compilerVersion, compilerDistributionDigest, options)
	return plans, err
}

func compilePlansWithAuthorization(ctx context.Context, ir IR, compilerVersion, compilerDistributionDigest string, options Options) ([]plan.Job, []PlanAuthorization, error) {
	payload, err := json.Marshal(ir.Event.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("encode event payload: %w", err)
	}
	eventDigest := sha256.Sum256(payload)
	plans := make([]plan.Job, 0, len(ir.Jobs))
	authorizations := make([]PlanAuthorization, 0, len(ir.Jobs))
	planDigests := make(map[string]string, len(ir.Jobs))
	logicalByKey := make(map[string]string, len(ir.Jobs))
	for _, instance := range ir.Jobs {
		logicalByKey[instance.Key] = instance.LogicalJobID
	}
	actionSource := newMemoizedActionSource(options.ActionSource)
	for _, instance := range ir.Jobs {
		steps := make([]plan.Step, len(instance.Steps))
		var actionIndexes []int
		var actionRefs []string
		usedIDs := make(map[string]struct{}, len(instance.Steps))
		for _, step := range instance.Steps {
			if step.ID != "" {
				usedIDs[strings.ToLower(step.ID)] = struct{}{}
			}
		}
		for i, step := range instance.Steps {
			id := step.ID
			if id == "" {
				id = fmt.Sprintf("step-%d", i+1)
				for suffix := 2; ; suffix++ {
					if _, exists := usedIDs[strings.ToLower(id)]; !exists {
						break
					}
					id = fmt.Sprintf("step-%d-%d", i+1, suffix)
				}
				usedIDs[strings.ToLower(id)] = struct{}{}
			}
			span := planSpan(step.Span)
			steps[i] = plan.Step{
				ID: id, Name: step.Name, Kind: step.Kind, Background: step.Background, Targets: append([]string(nil), step.Targets...), Command: step.Run, Uses: step.Uses,
				Shell: step.Shell, WorkingDirectory: step.WorkingDirectory,
				Env: cloneMap(step.Env), With: cloneMap(step.With), Condition: step.If,
				ContinueOnError: step.ContinueOnError, TimeoutMinutes: step.TimeoutMinutes, Source: &span,
			}
			if step.Kind == "uses" {
				actionIndexes = append(actionIndexes, i)
				actionRefs = append(actionRefs, step.Uses)
			}
		}
		jobSchema := plan.Schema
		var actions []plan.ActionLock
		var capabilities []string
		var authorization PlanAuthorization
		if options.ResolveActions && len(actionRefs) != 0 {
			for _, i := range actionIndexes {
				if strings.HasPrefix(strings.ToLower(instance.Steps[i].Uses), "actions/checkout@") {
					if err := validateCheckoutInputs(instance.Steps[i].With, ir.Event.Repository.Owner+"/"+ir.Event.Repository.Name, ir.Event.SHA); err != nil {
						span := instance.Steps[i].Span.Start
						return nil, nil, fmt.Errorf("%s:%d:%d: tokenless checkout adapter: %w", instance.SourcePath, span.Line, span.Column, err)
					}
				}
			}
			selectors, locks, actionCapabilities, err := compileActionLocks(ctx, instance.RepositoryRoot, actionSource, actionRefs)
			if err != nil {
				return nil, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
			}
			for i, selector := range selectors {
				steps[actionIndexes[i]].Action = &plan.ActionSelector{Lock: selector.Lock}
			}
			jobSchema = plan.SchemaV3
			actions = locks
			capabilities = actionCapabilities
			if slices.Contains(actionCapabilities, "docker") {
				authorization.DockerCapabilitySource = "dockerfile-actions"
			}
		} else {
			var err error
			capabilities, err = requiredCapabilities(instance.RepositoryRoot, instance.SourcePath, instance.Steps)
			if err != nil {
				return nil, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
			}
		}
		needSources := make(map[string][]plan.NeedSource, len(instance.LogicalNeeds))
		for _, logicalNeed := range instance.LogicalNeeds {
			for _, dependency := range instance.Needs {
				if logicalByKey[dependency] != logicalNeed {
					continue
				}
				digest, ok := planDigests[dependency]
				if !ok {
					return nil, nil, fmt.Errorf("build plan for job %q: prerequisite %q has no earlier plan digest", instance.LogicalJobID, dependency)
				}
				needSources[logicalNeed] = append(needSources[logicalNeed], plan.NeedSource{StepKey: dependency, PlanDigest: digest})
			}
		}
		secrets, err := requiredSecrets(instance)
		if err != nil {
			return nil, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
		}
		if len(secrets) != 0 {
			capabilities = append(capabilities, "secrets")
			sort.Strings(capabilities)
		}
		job := plan.Job{
			Schema: jobSchema,
			Compiler: plan.Compiler{
				Version: compilerVersion, DistributionDigest: compilerDistributionDigest,
			},
			Workflow: plan.Workflow{
				Path:         instance.SourcePath,
				Digest:       instance.SourceDigest,
				LogicalJobID: instance.LogicalJobID,
			},
			Event: plan.Event{
				Provider: ir.Event.Provider, Name: ir.Event.Event, PayloadDigest: "sha256:" + hex.EncodeToString(eventDigest[:]),
				Repository: ir.Event.Repository.Owner + "/" + ir.Event.Repository.Name,
				Ref:        ir.Event.Ref, SHA: ir.Event.SHA, Actor: ir.Event.Actor,
			},
			Target:                  plan.Target{StepKey: instance.Key, Queue: instance.Queue},
			RequiredCapabilities:    capabilities,
			RequiredSecrets:         secrets,
			Matrix:                  instance.Matrix,
			Vars:                    cloneMap(ir.Vars),
			Dependencies:            append([]string(nil), instance.Needs...),
			NeedSources:             needSources,
			Env:                     instance.Env,
			Condition:               instance.If,
			TimeoutMinutes:          instance.TimeoutMinutes,
			DefaultShell:            instance.DefaultShell,
			DefaultWorkingDirectory: instance.DefaultWorkingDirectory,
			Outputs:                 instance.Outputs,
			Steps:                   steps,
			Actions:                 actions,
		}
		if err := job.Validate(); err != nil {
			return nil, nil, fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
		}
		encoded, err := plan.Encode(job)
		if err != nil {
			return nil, nil, fmt.Errorf("encode plan for job %q: %w", instance.LogicalJobID, err)
		}
		digest := sha256.Sum256(encoded)
		planDigests[instance.Key] = "sha256:" + hex.EncodeToString(digest[:])
		plans = append(plans, job)
		authorizations = append(authorizations, authorization)
	}
	return plans, authorizations, nil
}

func validateCheckoutInputs(inputs map[string]string, repository, sha string) error {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		value := inputs[name]
		normalized := strings.ToLower(name)
		if seen[normalized] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported; Phase 6 is required", name)
		}
		seen[normalized] = true
		switch normalized {
		case "repository":
			if !strings.EqualFold(value, repository) {
				return fmt.Errorf("input %q is unsupported; Phase 6 is required", name)
			}
		case "ref":
			if value != sha {
				return fmt.Errorf("input %q is unsupported; Phase 6 is required", name)
			}
		case "persist-credentials":
			if value != "false" {
				return fmt.Errorf("input %q is unsupported; Phase 6 is required", name)
			}
		case "fetch-depth":
			if value != "1" {
				return fmt.Errorf("input %q is unsupported; Phase 6 is required", name)
			}
		case "clean", "set-safe-directory":
			if value != "true" {
				return fmt.Errorf("input %q is unsupported; Phase 6 is required", name)
			}
		default:
			return fmt.Errorf("explicit input %q is unsupported (including empty values); Phase 6 is required", name)
		}
	}
	return nil
}

func requiredSecrets(instance JobInstance) ([]string, error) {
	found := map[string]string{}
	collect := func(value string) error {
		names, err := expression.SecretReferences(value)
		if err != nil {
			return err
		}
		for _, name := range names {
			found[name] = name
		}
		return nil
	}
	for _, value := range []string{instance.If, instance.DefaultShell, instance.DefaultWorkingDirectory} {
		if err := collect(value); err != nil {
			return nil, err
		}
	}
	for _, values := range []map[string]string{instance.Env, instance.Outputs} {
		for _, name := range sortedValueKeys(values) {
			if err := collect(values[name]); err != nil {
				return nil, err
			}
		}
	}
	for _, step := range instance.Steps {
		for _, value := range []string{step.Run, step.If, step.Shell, step.WorkingDirectory} {
			if err := collect(value); err != nil {
				return nil, err
			}
		}
		for _, values := range []map[string]string{step.Env, step.With} {
			for _, name := range sortedValueKeys(values) {
				if err := collect(values[name]); err != nil {
					return nil, err
				}
			}
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func planSpan(span workflow.Span) plan.Span {
	return plan.Span{
		Start: plan.Position{Line: span.Start.Line, Column: span.Start.Column},
		End:   plan.Position{Line: span.End.Line, Column: span.End.Column},
	}
}

func compile(path string, source, eventSource []byte, options Options) (IR, error) {
	if err := options.validate(); err != nil {
		return IR{}, err
	}
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return IR{}, err
	}
	event, err := parseEvent(eventSource)
	if err != nil {
		return IR{}, err
	}
	event.Trust = options.EventTrust
	vars := options.Vars.snapshot()
	jobs, err := expand(path, source, parsed, compileContext(event, vars), options)
	if err != nil {
		return IR{}, err
	}
	digest := sha256.Sum256(source)
	return IR{
		Schema:   schema,
		Workflow: WorkflowSource{Path: path, Name: parsed.Name, Digest: "sha256:" + hex.EncodeToString(digest[:])},
		Event:    event,
		Vars:     vars,
		Execution: ExecutionBoundary{
			Supported: true,
			Reason:    "run-job supports the fail-closed Phase 0 shell and local-action subset",
		},
		Jobs: jobs,
	}, nil
}

func parseEvent(source []byte) (Event, error) {
	if len(bytes.TrimSpace(source)) == 0 {
		return Event{}, fmt.Errorf("event snapshot is required")
	}
	var input struct {
		Provider   string         `json:"provider"`
		Event      string         `json:"event"`
		Repository Repository     `json:"repository"`
		Ref        string         `json:"ref"`
		SHA        string         `json:"sha"`
		Actor      string         `json:"actor"`
		Payload    map[string]any `json:"payload"`
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Event{}, fmt.Errorf("parse event snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Event{}, fmt.Errorf("parse event snapshot: multiple JSON values")
		}
		return Event{}, fmt.Errorf("parse event snapshot: %w", err)
	}
	if strings.TrimSpace(input.Provider) == "" || strings.TrimSpace(input.Event) == "" || strings.TrimSpace(input.Repository.Owner) == "" || strings.TrimSpace(input.Repository.Name) == "" || strings.TrimSpace(input.Ref) == "" || strings.TrimSpace(input.SHA) == "" || strings.TrimSpace(input.Actor) == "" {
		return Event{}, fmt.Errorf("event snapshot requires provider, event, repository owner/name, ref, sha, and actor")
	}
	if input.Payload == nil {
		input.Payload = map[string]any{}
	}
	return Event{
		Provider: input.Provider, Event: input.Event, Repository: input.Repository,
		Ref: input.Ref, SHA: input.SHA, Actor: input.Actor, Payload: input.Payload,
	}, nil
}

func compileContext(event Event, vars map[string]string) expression.CompileContext {
	repository := event.Repository.Owner + "/" + event.Repository.Name
	return expression.CompileContext{
		GitHub: map[string]any{
			"event_name":       event.Event,
			"event":            event.Payload,
			"repository":       repository,
			"repository_owner": event.Repository.Owner,
			"ref":              event.Ref,
			"sha":              event.SHA,
			"actor":            event.Actor,
		},
		Event: event.Payload,
		Vars:  vars,
	}
}

func expand(path string, source []byte, parsed *workflow.Workflow, context expression.CompileContext, options Options) ([]JobInstance, error) {
	resolved, err := resolveReusableWorkflows(path, source, parsed)
	if err != nil {
		return nil, err
	}
	jobs := make(map[string]workflow.Job, len(resolved))
	sourcePaths := make(map[string]string, len(resolved))
	sourceDigests := make(map[string]string, len(resolved))
	sourceRoots := make(map[string]string, len(resolved))
	for _, sourced := range resolved {
		job := sourced.Job
		if _, exists := jobs[job.ID]; exists {
			return nil, jobError(sourced.path, job, fmt.Sprintf("flattened job id %q collides with another job", job.ID))
		}
		jobs[job.ID] = job
		sourcePaths[job.ID] = sourced.path
		sourceDigests[job.ID] = sourced.digest
		sourceRoots[job.ID] = sourced.root
		if err := supported(sourced.path, job); err != nil {
			return nil, err
		}
	}
	order, err := topologicalOrder(path, jobs)
	if err != nil {
		return nil, err
	}

	byLogicalID := make(map[string][]JobInstance, len(jobs))
	instanceKeys := make(map[string]string)
	for _, id := range order {
		job := jobs[id]
		jobPath := sourcePaths[id]
		matrices, err := expandMatrix(jobPath, job, context)
		if err != nil {
			return nil, err
		}
		for _, matrix := range matrices {
			labels, err := resolveRunsOn(job, context, matrix)
			if err != nil {
				return nil, locatedJobError(jobPath, job, runsOnPosition(job).Line, runsOnPosition(job).Column, err.Error())
			}
			queue, err := options.Runners.resolve(labels, options.EventTrust)
			if err != nil {
				return nil, locatedJobError(jobPath, job, runsOnPosition(job).Line, runsOnPosition(job).Column, err.Error())
			}
			key, err := instanceKey(job.ID, matrix)
			if err != nil {
				return nil, jobError(jobPath, job, fmt.Sprintf("create deterministic instance key: %v", err))
			}
			if existingJob, exists := instanceKeys[key]; exists {
				return nil, jobError(jobPath, job, fmt.Sprintf("deterministic instance key %q collides with another instance from job %q", key, existingJob))
			}
			instanceKeys[key] = job.ID
			instance := JobInstance{
				Key:                     key,
				LogicalJobID:            job.ID,
				Label:                   instanceLabel(job, matrix),
				RunsOn:                  labels,
				Queue:                   queue,
				LogicalNeeds:            append([]string(nil), job.Needs...),
				Matrix:                  matrix,
				FailFast:                job.FailFast,
				MaxParallel:             job.MaxParallel,
				Steps:                   append([]workflow.Step(nil), job.Steps...),
				Env:                     cloneMap(job.Env),
				If:                      job.If,
				TimeoutMinutes:          job.TimeoutMinutes,
				DefaultShell:            job.DefaultShell,
				DefaultWorkingDirectory: job.DefaultWorkingDirectory,
				Outputs:                 cloneMap(job.Outputs),
				SourcePath:              jobPath,
				SourceDigest:            sourceDigests[id],
				RepositoryRoot:          sourceRoots[id],
				Source:                  job.Span,
			}
			for _, need := range job.Needs {
				for _, prerequisite := range byLogicalID[need] {
					instance.Needs = append(instance.Needs, prerequisite.Key)
				}
			}
			sort.Strings(instance.Needs)
			byLogicalID[id] = append(byLogicalID[id], instance)
		}
	}

	var instances []JobInstance
	for _, id := range order {
		instances = append(instances, byLogicalID[id]...)
	}
	return instances, nil
}

func supported(path string, job workflow.Job) error {
	if job.Reusable != nil {
		return jobError(path, job, "internal error: unresolved reusable-workflow job")
	}
	if len(job.RunsOn) == 0 && job.RunsOnExpr == nil {
		return jobError(path, job, "runs-on must resolve statically")
	}
	ids := make(map[string]struct{}, len(job.Steps))
	for _, step := range job.Steps {
		if step.ID != "" {
			id := strings.ToLower(step.ID)
			if _, exists := ids[id]; exists {
				return locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("duplicate step id %q", step.ID))
			}
			ids[id] = struct{}{}
		}
	}
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func expandMatrix(path string, job workflow.Job, context expression.CompileContext) ([]map[string]any, error) {
	if job.Matrix == nil {
		return []map[string]any{nil}, nil
	}
	matrix, err := resolveMatrix(job.Matrix, context)
	if err != nil {
		position := job.Matrix.Span.Start
		if job.Matrix.Expression != nil {
			position = workflow.Position{Line: job.Matrix.Expression.Span.Start.Line, Column: job.Matrix.Expression.Span.Start.Column}
		}
		return nil, locatedJobError(path, job, position.Line, position.Column, err.Error())
	}
	matrices := []map[string]any{{}}
	if len(matrix.Rows) == 0 {
		matrices = nil
	}
	for _, row := range matrix.Rows {
		if len(row.Values) == 0 {
			return nil, jobError(path, job, fmt.Sprintf("matrix dimension %q has no values", row.Name))
		}
		var next []map[string]any
		for _, current := range matrices {
			for _, value := range row.Values {
				combination := make(map[string]any, len(current)+1)
				for key, existing := range current {
					combination[key] = existing
				}
				combination[row.Name] = value.Data
				next = append(next, combination)
				if len(next) > maxMatrixInstances {
					return nil, jobError(path, job, fmt.Sprintf("matrix expands beyond %d instances", maxMatrixInstances))
				}
			}
		}
		matrices = next
	}

	matrices = excludeMatrixCombinations(matrices, matrix.Exclude)
	// Includes may overwrite values added by earlier includes, but not original
	// dimensions. Standalone combinations are never candidates for later entries.
	type includedMatrix struct {
		values   map[string]any
		original map[string]any
	}
	included := make([]includedMatrix, len(matrices))
	for i, matrix := range matrices {
		included[i] = includedMatrix{values: matrix, original: cloneAnyMap(matrix)}
	}
	for _, combination := range matrix.Include {
		values := matrixCombinationValues(combination)
		matched := false
		for i := range included {
			if included[i].original == nil || !includeCompatible(included[i].original, values) {
				continue
			}
			for key, value := range values {
				included[i].values[key] = value
			}
			matched = true
		}
		if !matched {
			included = append(included, includedMatrix{values: cloneAnyMap(values)})
		}
		if len(included) > maxMatrixInstances {
			return nil, jobError(path, job, fmt.Sprintf("matrix expands beyond %d instances", maxMatrixInstances))
		}
	}
	matrices = matrices[:0]
	for _, matrix := range included {
		matrices = append(matrices, matrix.values)
	}
	if len(matrices) == 0 {
		return nil, jobError(path, job, "matrix excludes every combination")
	}
	return matrices, nil
}

func resolveMatrix(matrix *workflow.Matrix, context expression.CompileContext) (*workflow.Matrix, error) {
	if matrix.Expression != nil {
		value, err := expression.EvaluateCompile(*matrix.Expression, context)
		if err != nil {
			return nil, matrixExpressionError(err)
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("matrix expression resolved to %T, want object", value)
		}
		return matrixFromObject(object)
	}
	resolved := *matrix
	resolved.Rows = make([]workflow.MatrixRow, len(matrix.Rows))
	for i, row := range matrix.Rows {
		resolved.Rows[i] = row
		if row.Expression == nil {
			resolved.Rows[i].Values = append([]workflow.Value(nil), row.Values...)
			continue
		}
		value, err := expression.EvaluateCompile(*row.Expression, context)
		if err != nil {
			return nil, fmt.Errorf("matrix dimension %q: %w", row.Name, matrixExpressionError(err))
		}
		values, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("matrix dimension %q resolved to %T, want array", row.Name, value)
		}
		resolved.Rows[i].Expression = nil
		resolved.Rows[i].Values = make([]workflow.Value, len(values))
		for j, value := range values {
			resolved.Rows[i].Values[j] = workflow.Value{Data: value, Span: row.Span}
		}
	}
	return &resolved, nil
}

func matrixExpressionError(err error) error {
	if strings.Contains(err.Error(), `compile-time context "needs"`) || strings.Contains(err.Error(), `compile-time context "steps"`) {
		return fmt.Errorf("runtime-dependent matrix expressions are unsupported: %w", err)
	}
	return fmt.Errorf("matrix expression cannot be resolved at compile time: %w", err)
}

func matrixFromObject(object map[string]any) (*workflow.Matrix, error) {
	keys := make([]string, 0, len(object))
	for key := range object {
		if key != "include" && key != "exclude" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	matrix := &workflow.Matrix{}
	for _, key := range keys {
		values, ok := object[key].([]any)
		if !ok {
			return nil, fmt.Errorf("matrix dimension %q resolved to %T, want array", key, object[key])
		}
		row := workflow.MatrixRow{Name: key, Values: make([]workflow.Value, len(values))}
		for i, value := range values {
			row.Values[i] = workflow.Value{Data: value}
		}
		matrix.Rows = append(matrix.Rows, row)
	}
	var err error
	if matrix.Include, err = matrixCombinationsFromValue("include", object["include"]); err != nil {
		return nil, err
	}
	if matrix.Exclude, err = matrixCombinationsFromValue("exclude", object["exclude"]); err != nil {
		return nil, err
	}
	return matrix, nil
}

func matrixCombinationsFromValue(name string, value any) ([]workflow.MatrixCombination, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("matrix %s resolved to %T, want array", name, value)
	}
	combinations := make([]workflow.MatrixCombination, len(items))
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("matrix %s entry %d resolved to %T, want object", name, i, item)
		}
		values := make(map[string]workflow.Value, len(object))
		for key, value := range object {
			values[key] = workflow.Value{Data: value}
		}
		combinations[i] = workflow.MatrixCombination{Values: values}
	}
	return combinations, nil
}

func resolveRunsOn(job workflow.Job, context expression.CompileContext, matrix map[string]any) ([]string, error) {
	context.Matrix = matrix
	if job.RunsOnExpr != nil {
		value, err := expression.EvaluateCompile(*job.RunsOnExpr, context)
		if err != nil {
			return nil, fmt.Errorf("runs-on expression cannot be resolved at compile time: %w", err)
		}
		switch value := value.(type) {
		case string:
			return []string{value}, nil
		case []any:
			labels := make([]string, len(value))
			for i, item := range value {
				label, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("runs-on expression item %d resolved to %T, want string", i, item)
				}
				labels[i] = label
			}
			return labels, nil
		default:
			return nil, fmt.Errorf("runs-on expression resolved to %T, want string or array", value)
		}
	}
	labels := make([]string, len(job.RunsOn))
	for i, label := range job.RunsOn {
		resolved, err := expression.EvaluateCompileTemplate(label, context)
		if err != nil {
			return nil, fmt.Errorf("runs-on label %q cannot be resolved at compile time: %w", label, err)
		}
		labels[i] = resolved
	}
	return labels, nil
}

func runsOnPosition(job workflow.Job) workflow.Position {
	if job.RunsOnExpr != nil {
		return workflow.Position{Line: job.RunsOnExpr.Span.Start.Line, Column: job.RunsOnExpr.Span.Start.Column}
	}
	return job.Span.Start
}

func excludeMatrixCombinations(matrices []map[string]any, exclusions []workflow.MatrixCombination) []map[string]any {
	if len(exclusions) == 0 {
		return matrices
	}
	out := matrices[:0]
	for _, matrix := range matrices {
		excluded := false
		for _, exclusion := range exclusions {
			if matrixMatches(matrix, matrixCombinationValues(exclusion)) {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, matrix)
		}
	}
	return out
}

func matrixMatches(matrix, pattern map[string]any) bool {
	for key, expected := range pattern {
		actual, ok := matrix[key]
		if !ok || !matrixValuesEqual(actual, expected) {
			return false
		}
	}
	return true
}

func includeCompatible(original, values map[string]any) bool {
	for key, value := range values {
		if originalValue, exists := original[key]; exists && !matrixValuesEqual(originalValue, value) {
			return false
		}
	}
	return true
}

func matrixValuesEqual(left, right any) bool {
	if leftNumber, ok := matrixNumber(left); ok {
		rightNumber, rightOK := matrixNumber(right)
		return rightOK && leftNumber.Cmp(rightNumber) == 0
	}
	switch left := left.(type) {
	case []any:
		right, ok := right.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for i := range left {
			if !matrixValuesEqual(left[i], right[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		right, ok := right.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, ok := right[key]
			if !ok || !matrixValuesEqual(value, other) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left, right)
	}
}

func matrixNumber(value any) (*big.Rat, bool) {
	var text string
	switch value := value.(type) {
	case json.Number:
		text = value.String()
	default:
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			text = strconv.FormatInt(reflected.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			text = strconv.FormatUint(reflected.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			text = strconv.FormatFloat(reflected.Float(), 'g', -1, reflected.Type().Bits())
		default:
			return nil, false
		}
	}
	number, ok := new(big.Rat).SetString(text)
	return number, ok
}

func matrixCombinationValues(combination workflow.MatrixCombination) map[string]any {
	out := make(map[string]any, len(combination.Values))
	for key, value := range combination.Values {
		out[key] = value.Data
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func topologicalOrder(path string, jobs map[string]workflow.Job) ([]string, error) {
	indegree := make(map[string]int, len(jobs))
	dependents := make(map[string][]string, len(jobs))
	for id, job := range jobs {
		indegree[id] = len(job.Needs)
		for _, need := range job.Needs {
			if _, ok := jobs[need]; !ok {
				return nil, jobError(path, job, fmt.Sprintf("needs unknown job %q", need))
			}
			dependents[need] = append(dependents[need], id)
		}
	}
	var ready []string
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) != 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(jobs) {
		var cyclic []string
		for id, degree := range indegree {
			if degree != 0 {
				cyclic = append(cyclic, id)
			}
		}
		sort.Strings(cyclic)
		return nil, jobError(path, jobs[cyclic[0]], "workflow job graph contains a cycle")
	}
	return order, nil
}

func instanceKey(jobID string, matrix map[string]any) (string, error) {
	prefix := "gha-" + sanitize(jobID)
	if len(matrix) == 0 {
		return prefix, nil
	}
	canonical, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("canonicalize matrix: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return prefix + "-" + hex.EncodeToString(digest[:6]), nil
}

func requiredCapabilities(repositoryRoot, workflowPath string, steps []workflow.Step) ([]string, error) {
	capabilities := map[string]struct{}{}
	for _, step := range steps {
		if step.Kind != "uses" || !strings.HasPrefix(step.Uses, "./") {
			continue
		}
		if repositoryRoot == "" {
			return nil, fmt.Errorf("resolve local action %q: workflow path %q must identify a repository root", step.Uses, workflowPath)
		}
		if err := collectActionCapabilities(repositoryRoot, step.Uses, capabilities, nil); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(capabilities))
	for capability := range capabilities {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out, nil
}

func collectActionCapabilities(repositoryRoot, uses string, capabilities map[string]struct{}, stack []string) error {
	if !strings.HasPrefix(uses, "./") {
		return fmt.Errorf("nested remote action %q is unsupported", uses)
	}
	action, err := loadLocalAction(repositoryRoot, uses)
	if err != nil {
		return err
	}
	for _, ancestor := range stack {
		if ancestor == action.Path {
			return fmt.Errorf("local action recursion detected at %q", action.Path)
		}
	}
	if len(stack) >= metadata.MaxNestedActionDepth {
		return fmt.Errorf("local action nesting exceeds maximum depth %d at %q", metadata.MaxNestedActionDepth, action.Path)
	}
	runtime, err := action.Runtime()
	if err != nil {
		return fmt.Errorf("local action %q uses %w", uses, err)
	}
	for _, capability := range runtime.RequiredCapabilities() {
		capabilities[capability] = struct{}{}
	}
	if runtime != metadata.RuntimeComposite {
		return nil
	}
	stack = append(append([]string(nil), stack...), action.Path)
	for _, child := range action.Runs.Steps {
		if child.Uses != "" {
			if err := collectActionCapabilities(repositoryRoot, child.Uses, capabilities, stack); err != nil {
				return fmt.Errorf("local action %q nested uses %w", uses, err)
			}
		}
	}
	return nil
}

func loadLocalAction(repositoryRoot, uses string) (metadata.Metadata, error) {
	relativeActionPath := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(uses, "./")))
	return metadata.Load(repositoryRoot, relativeActionPath)
}

func instanceLabel(job workflow.Job, matrix map[string]any) string {
	label := job.Name
	if label == "" {
		label = job.ID
	}
	if len(matrix) == 0 {
		return label
	}
	keys := make([]string, 0, len(matrix))
	for key := range matrix {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, fmt.Sprintf("%s=%v", key, matrix[key]))
	}
	return label + " (" + strings.Join(values, ", ") + ")"
}

func sanitize(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			out.WriteRune(r)
		} else if out.Len() > 0 {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func jobError(path string, job workflow.Job, message string) error {
	return locatedJobError(path, job, job.Span.Start.Line, job.Span.Start.Column, message)
}

func locatedJobError(path string, job workflow.Job, line, column int, message string) error {
	return fmt.Errorf("%s:%d:%d: job %q: %s", path, line, column, job.ID, message)
}
