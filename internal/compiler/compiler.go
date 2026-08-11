// Package compiler expands an owned workflow into deterministic workflow JSON IR.
package compiler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	schema             = "buildkite-gha/compiler-ir/v1"
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

// IR is the deterministic, actionlint-independent workflow compiler output.
type IR struct {
	Schema    string            `json:"schema"`
	Workflow  WorkflowSource    `json:"workflow"`
	Warnings  []Warning         `json:"warnings,omitempty"`
	Event     Event             `json:"event"`
	Vars      map[string]string `json:"vars,omitempty"`
	Execution ExecutionBoundary `json:"execution"`
	Jobs      []JobInstance     `json:"jobs"`
}

// Warning is one source-located, non-fatal compatibility diagnostic.
type Warning struct {
	Code    string `json:"code"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

// ExecutionBoundary makes the compile-only boundary explicit.
type ExecutionBoundary struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// WorkflowSource binds the IR to its input workflow bytes.
type WorkflowSource struct {
	Path                          string             `json:"path"`
	Name                          string             `json:"name,omitempty"`
	Digest                        string             `json:"digest"`
	ConcurrencyGroup              string             `json:"concurrency_group,omitempty"`
	Triggers                      []workflow.Trigger `json:"-"`
	WorkflowTokenPolicyFilename   string             `json:"-"`
	WorkflowTokenPolicyDiagnostic string             `json:"-"`
}

// JobInstance is one statically expanded job in the owned IR.
type JobInstance struct {
	Key                     string                  `json:"key"`
	LogicalJobID            string                  `json:"logical_job_id"`
	Label                   string                  `json:"label"`
	Needs                   []string                `json:"needs,omitempty"`
	NeedGroups              map[string][]string     `json:"need_groups,omitempty"`
	NeedOutputs             map[string][]NeedOutput `json:"need_outputs,omitempty"`
	RunsOn                  []string                `json:"runs_on"`
	Queue                   string                  `json:"queue"`
	Platform                Platform                `json:"-"`
	RuntimeImage            string                  `json:"runtime_image,omitempty"`
	Matrix                  map[string]any          `json:"matrix,omitempty"`
	FailFast                *bool                   `json:"fail_fast,omitempty"`
	MaxParallel             *int                    `json:"max_parallel,omitempty"`
	ConcurrencyGroup        string                  `json:"concurrency_group,omitempty"`
	Steps                   []workflow.Step         `json:"steps"`
	Env                     map[string]string       `json:"env,omitempty"`
	Permissions             map[string]string       `json:"permissions,omitempty"`
	If                      string                  `json:"if,omitempty"`
	TimeoutMinutes          float64                 `json:"timeout_minutes,omitempty"`
	DefaultShell            string                  `json:"default_shell,omitempty"`
	DefaultWorkingDirectory string                  `json:"default_working_directory,omitempty"`
	Outputs                 map[string]string       `json:"outputs,omitempty"`
	Container               *workflow.Container     `json:"container,omitempty"`
	Services                []workflow.Service      `json:"services,omitempty"`
	SourcePath              string                  `json:"source_path"`
	SourceDigest            string                  `json:"source_digest"`
	RepositoryRoot          string                  `json:"-"`
	Source                  workflow.Span           `json:"source"`
	secretAuthority         bool
}

// NeedOutput selects one caller-visible output from a concrete prerequisite.
// An empty output list explicitly prevents an aggregate need from exposing its
// producers' internal outputs.
type NeedOutput struct {
	Name    string `json:"name"`
	StepKey string `json:"step_key"`
	Output  string `json:"output"`
}

// Report summarizes successful workflow validation.
type Report struct {
	LogicalJobs           int
	Instances             int
	Warnings              []Warning
	Jobs                  []JobInstance
	RuntimeMatrixBoundary bool
	RuntimeMatrices       []RuntimeMatrixDescriptor
	ParsedJobs            []ParsedJob
	NotEvaluatedJobs      map[string]bool
	NotEvaluatedInstances map[string]bool
}

// ParsedJob is the source identity retained before expansion succeeds.
type ParsedJob struct {
	ID     string
	Path   string
	Source workflow.Span
}

type expansionResult struct {
	instances             []JobInstance
	candidates            []JobInstance
	runtimeMatrixBoundary bool
	runtimeMatrices       []RuntimeMatrixDescriptor
	jobs                  []ParsedJob
	notEvaluatedJobs      map[string]bool
	notEvaluatedInstances map[string]bool
}

func parsedJobs(path string, parsed *workflow.Workflow) []ParsedJob {
	jobs := make([]ParsedJob, len(parsed.Jobs))
	for i, job := range parsed.Jobs {
		jobs[i] = ParsedJob{ID: job.ID, Path: path, Source: job.Span}
	}
	return jobs
}

func processingJobs(path string, parsed *workflow.Workflow, resolved []sourcedJob) []ParsedJob {
	jobs := parsedJobs(path, parsed)
	seen := make(map[string]bool, len(jobs)+len(resolved))
	for _, job := range jobs {
		seen[job.ID] = true
	}
	for _, sourced := range resolved {
		if seen[sourced.ID] {
			continue
		}
		seen[sourced.ID] = true
		jobs = append(jobs, ParsedJob{ID: sourced.ID, Path: sourced.path, Source: sourced.Span})
	}
	return jobs
}

// ParseWorkflow reports only event-independent workflow syntax and job
// identities. Later stages deliberately remain unevaluated.
func ParseWorkflow(path string, source []byte) (Report, error) {
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return Report{}, processingFinding(StageWorkflowParsing, CodeWorkflowSyntax, "syntax", err)
	}
	return Report{LogicalJobs: len(parsed.Jobs), ParsedJobs: parsedJobs(path, parsed), Warnings: compilerWarnings(parsed.Concurrency, false)}, nil
}

func expansionReport(expanded expansionResult, warnings []Warning) Report {
	return Report{
		LogicalJobs: len(expanded.jobs), Instances: len(expanded.candidates),
		Jobs: expanded.candidates, RuntimeMatrixBoundary: expanded.runtimeMatrixBoundary,
		RuntimeMatrices: expanded.runtimeMatrices, ParsedJobs: expanded.jobs, Warnings: warnings,
		NotEvaluatedJobs: expanded.notEvaluatedJobs, NotEvaluatedInstances: expanded.notEvaluatedInstances,
	}
}

// Validate parses and validates the supported static graph without requiring an
// event snapshot.
func Validate(path string, source []byte) (Report, error) {
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return Report{}, processingFinding(StageWorkflowParsing, CodeWorkflowSyntax, "syntax", err)
	}
	options := defaultOptions()
	event := Event{
		Event: "validation", Trust: options.EventTrust,
		Repository: Repository{Owner: "validation", Name: "validation"},
		Ref:        "refs/heads/validation", SHA: strings.Repeat("0", 40), Actor: "validation",
		Payload: map[string]any{},
	}
	context := compileContext(event, nil, path, parsed.Name)
	_, concurrencyErr := resolveConcurrency(path, "", parsed.Concurrency, context, nil)
	concurrencyErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", concurrencyErr)
	cancelInProgress, cancellationErr := resolveWorkflowCancellation(path, parsed.Concurrency, context)
	cancellationErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", cancellationErr)
	expanded, expandErr := expand(path, source, parsed, context, options)
	if err := errors.Join(concurrencyErr, cancellationErr, expandErr); err != nil {
		return expansionReport(expanded, compilerWarnings(parsed.Concurrency, cancelInProgress)), err
	}
	return expansionReport(expanded, compilerWarnings(parsed.Concurrency, cancelInProgress)), nil
}

// ValidateEvent validates both the supported static graph and its event input.
func ValidateEvent(path string, source, eventSource []byte) (Report, error) {
	return ValidateEventWithOptions(path, source, eventSource, defaultOptions())
}

// ValidateEventWithOptions validates the graph against explicit variables and
// runner policy without producing compiler output.
func ValidateEventWithOptions(path string, source, eventSource []byte, options Options) (Report, error) {
	var optionsErr error
	if err := options.validate(); err != nil {
		optionsErr = &ProcessingFinding{
			Code: CodeEnvironment, Category: "environment",
			Message: "workflow-processing configuration is invalid", Err: err,
		}
	}
	parsed, parseErr := workflow.Parse(path, source)
	parseErr = processingFinding(StageWorkflowParsing, CodeWorkflowSyntax, "syntax", parseErr)
	event, eventErr := parseEvent(eventSource)
	eventErr = processingFinding(StageEventValidation, CodeEventInvalid, "environment", eventErr)
	if parseErr != nil || eventErr != nil || optionsErr != nil {
		if parsed == nil {
			return Report{}, errors.Join(parseErr, eventErr, optionsErr)
		}
		notEvaluatedJobs := make(map[string]bool, len(parsed.Jobs))
		for _, job := range parsed.Jobs {
			notEvaluatedJobs[job.ID] = true
		}
		return Report{
			LogicalJobs: len(parsed.Jobs), ParsedJobs: parsedJobs(path, parsed),
			Warnings: compilerWarnings(parsed.Concurrency, false), NotEvaluatedJobs: notEvaluatedJobs,
		}, errors.Join(parseErr, eventErr, optionsErr)
	}
	event.Trust = options.EventTrust
	context := compileContext(event, options.Vars.snapshot(), path, parsed.Name)
	_, concurrencyErr := resolveConcurrency(path, "", parsed.Concurrency, context, nil)
	concurrencyErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", concurrencyErr)
	cancelInProgress, cancellationErr := resolveWorkflowCancellation(path, parsed.Concurrency, context)
	cancellationErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", cancellationErr)
	expanded, expandErr := expand(path, source, parsed, context, options)
	if err := errors.Join(concurrencyErr, cancellationErr, expandErr); err != nil {
		return expansionReport(expanded, compilerWarnings(parsed.Concurrency, cancelInProgress)), err
	}
	return expansionReport(expanded, compilerWarnings(parsed.Concurrency, cancelInProgress)), nil
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

func compilePlansWithAuthorization(ctx context.Context, ir IR, compilerVersion, compilerDistributionDigest string, options Options) ([]plan.Job, []PlanAuthorization, []JobEvaluation, error) {
	payload, err := json.Marshal(ir.Event.Payload)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode event payload: %w", err)
	}
	eventDigest := sha256.Sum256(payload)
	plans := make([]plan.Job, 0, len(ir.Jobs))
	authorizations := make([]PlanAuthorization, 0, len(ir.Jobs))
	evaluations := make([]JobEvaluation, 0, len(ir.Jobs))
	failedInstances := make(map[string]bool, len(ir.Jobs))
	var diagnostics []error
	planDigests := make(map[string]string, len(ir.Jobs))
	actionSource := newMemoizedActionSource(options.ActionSource)
instances:
	for _, instance := range ir.Jobs {
		for _, dependency := range instance.Needs {
			if failedInstances[dependency] {
				failedInstances[instance.Key] = true
				evaluations = append(evaluations, JobEvaluation{Instance: instance.Key, Job: instance.LogicalJobID})
				continue instances
			}
		}
		runtimeDistributionDigest := options.RuntimeDistributions[instance.Platform]
		if runtimeDistributionDigest == "" && instance.Platform == PlatformLinuxAMD64 {
			runtimeDistributionDigest = compilerDistributionDigest
		}
		var builtJob plan.Job
		var builtAuthorization PlanAuthorization
		var encoded []byte
		planErr := func() error {
			if runtimeDistributionDigest == "" {
				return fmt.Errorf("build plan for job %q: no runtime distribution configured for %s", instance.LogicalJobID, instance.Platform)
			}
			steps := make([]plan.Step, len(instance.Steps))
			var actionIndexes []int
			var actionRefs []string
			var actionInputs []map[string]string
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
					actionInputs = append(actionInputs, step.With)
				}
			}
			var actions []plan.ActionLock
			requiresMise := len(actionRefs) != 0
			var capabilities []string
			var authorization PlanAuthorization
			var actionRequiredSecrets []string
			actionInputsInspected := false
			lockActions := options.ResolveActions
			if len(actionRefs) != 0 && !lockActions {
				lockActions = true
				for _, ref := range actionRefs {
					if !strings.HasPrefix(ref, "./") {
						lockActions = false
						break
					}
				}
			}
			actionRequiresGitHubToken := false
			if lockActions && len(actionRefs) != 0 {
				compiledActions, err := compileActionInvocations(ctx, instance.RepositoryRoot, actionSource, actionRefs, actionInputs)
				if err != nil {
					return fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
				}
				selectors := compiledActions.selectors
				locks := compiledActions.locks
				actionCapabilities := compiledActions.capabilities
				requiresMise = compiledActions.requiresMise
				actionRequiresGitHubToken = compiledActions.requiresGitHubToken
				actionRequiredSecrets = compiledActions.requiredSecrets
				actionInputsInspected = true
				locksByID := make(map[string]plan.ActionLock, len(locks))
				for _, lock := range locks {
					locksByID[lock.ID] = lock
				}
				for i, selector := range selectors {
					stepIndex := actionIndexes[i]
					steps[stepIndex].Action = &plan.ActionSelector{Lock: selector.Lock}
					lock, ok := locksByID[selector.Lock]
					if !ok {
						return fmt.Errorf("build plan for job %q: action lock %q is missing", instance.LogicalJobID, selector.Lock)
					}
					descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
					if descriptor.Adapter == actionintegration.AdapterCheckoutExactEventSHA {
						checkoutInputs := cloneMap(instance.Steps[stepIndex].With)
						for name, value := range checkoutInputs {
							if !strings.EqualFold(name, "ref") {
								continue
							}
							root, path, refErr := expression.ReferencePath(value)
							if refErr == nil && (strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "sha") ||
								strings.EqualFold(root, "needs") && len(path) == 3 && strings.EqualFold(path[1], "outputs")) {
								checkoutInputs[name] = ir.Event.SHA
							}
						}
						if err := actionintegration.ValidateCheckoutInputs(checkoutInputs, ir.Event.Repository.Owner+"/"+ir.Event.Repository.Name, ir.Event.SHA); err != nil {
							span := instance.Steps[stepIndex].Span.Start
							return fmt.Errorf("%s:%d:%d: checkout adapter: %w", instance.SourcePath, span.Line, span.Column, err)
						}
						capabilities = append(capabilities, "provider-token-read")
						authorization.ProviderTokenReadCapabilitySources = append(authorization.ProviderTokenReadCapabilitySources, "checkout-adapter")
					}
					if descriptor.Adapter == actionintegration.AdapterUploadArtifactBuildkite {
						if err := actionintegration.ValidateUploadArtifactInputs(lock.Commit, instance.Steps[stepIndex].With); err != nil {
							span := instance.Steps[stepIndex].Span.Start
							return fmt.Errorf("%s:%d:%d: bounded upload-artifact adapter: %w", instance.SourcePath, span.Line, span.Column, err)
						}
					}
					if descriptor.Adapter == actionintegration.AdapterDownloadArtifactBuildkite {
						if err := actionintegration.ValidateDownloadArtifactInputs(lock.Commit, instance.Steps[stepIndex].With); err != nil {
							span := instance.Steps[stepIndex].Span.Start
							return fmt.Errorf("%s:%d:%d: bounded download-artifact adapter: %w", instance.SourcePath, span.Line, span.Column, err)
						}
						if len(instance.NeedGroups) == 0 {
							span := instance.Steps[stepIndex].Span.Start
							return fmt.Errorf("%s:%d:%d: bounded download-artifact adapter requires at least one direct needs producer", instance.SourcePath, span.Line, span.Column)
						}
					}
				}
				actions = locks
				capabilities = append(append([]string{}, capabilities...), actionCapabilities...)
				sort.Strings(capabilities)
				capabilities = slices.Compact(capabilities)
				sort.Strings(authorization.ProviderTokenReadCapabilitySources)
				authorization.ProviderTokenReadCapabilitySources = slices.Compact(authorization.ProviderTokenReadCapabilitySources)
				if slices.Contains(actionCapabilities, "docker") {
					authorization.DockerCapabilitySources = append(authorization.DockerCapabilitySources, "dockerfile-actions")
				}
			} else {
				var err error
				capabilities, err = requiredCapabilities(instance.RepositoryRoot, instance.SourcePath, instance.Steps)
				if err != nil {
					return fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
				}
			}
			if instance.Container != nil || len(instance.Services) != 0 {
				if len(actionRefs) != 0 && !lockActions {
					return fmt.Errorf("build plan for job %q: containers with remote actions require action resolution through upload or profile validation", instance.LogicalJobID)
				}
				capabilities = append(capabilities, "docker", "network")
				sort.Strings(capabilities)
				capabilities = slices.Compact(capabilities)
				if instance.Container != nil {
					authorization.DockerCapabilitySources = append(authorization.DockerCapabilitySources, "job-containers")
				}
				if len(instance.Services) != 0 {
					authorization.DockerCapabilitySources = append(authorization.DockerCapabilitySources, "service-containers")
				}
				sort.Strings(authorization.DockerCapabilitySources)
			}
			needSources := make(map[string][]plan.NeedSource, len(instance.NeedGroups))
			for _, logicalNeed := range sortedKeys(instance.NeedGroups) {
				dependencies := instance.NeedGroups[logicalNeed]
				if len(dependencies) > plan.MaxNeedProducers {
					return fmt.Errorf("build plan for job %q: prerequisite %q has %d producers, maximum is %d", instance.LogicalJobID, logicalNeed, len(dependencies), plan.MaxNeedProducers)
				}
				for _, dependency := range dependencies {
					digest, ok := planDigests[dependency]
					if !ok {
						return fmt.Errorf("build plan for job %q: prerequisite %q has no earlier plan digest", instance.LogicalJobID, dependency)
					}
					needSources[logicalNeed] = append(needSources[logicalNeed], plan.NeedSource{StepKey: dependency, PlanDigest: digest})
				}
			}
			var needOutputs map[string][]plan.NeedOutput
			if len(instance.NeedOutputs) != 0 {
				needOutputs = make(map[string][]plan.NeedOutput, len(instance.NeedOutputs))
				for _, logicalNeed := range sortedKeys(instance.NeedOutputs) {
					outputs := instance.NeedOutputs[logicalNeed]
					needOutputs[logicalNeed] = make([]plan.NeedOutput, len(outputs))
					for i, output := range outputs {
						needOutputs[logicalNeed][i] = plan.NeedOutput{Name: output.Name, StepKey: output.StepKey, Output: output.Output}
					}
				}
			}
			secrets, err := requiredSecrets(instance, actionRequiredSecrets, actionInputsInspected)
			if err != nil {
				return fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
			}
			var githubToken *plan.GitHubToken
			referencesGitHubTokenSecret := slices.Contains(secrets, "GITHUB_TOKEN")
			if referencesGitHubTokenSecret {
				secrets = slices.DeleteFunc(secrets, func(name string) bool { return name == "GITHUB_TOKEN" })
			}
			if referencesGitHubTokenSecret || actionRequiresGitHubToken {
				if len(instance.Permissions) == 0 {
					reference := "an action input default that references github.token"
					if referencesGitHubTokenSecret {
						reference = "secrets.GITHUB_TOKEN"
					}
					return fmt.Errorf("%s:%d:%d: job %q references %s but has no effective permissions", instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column, instance.LogicalJobID, reference)
				}
				permissions := make(map[string]string, len(instance.Permissions))
				for name, access := range instance.Permissions {
					permissions[strings.ReplaceAll(name, "-", "_")] = access
				}
				githubToken = &plan.GitHubToken{Permissions: permissions}
				capabilities = append(capabilities, "provider-token-write")
				authorization.ProviderTokenWriteCapabilitySources = []string{"effective-permissions"}
				authorization.WorkflowTokenPolicyFilename = ir.Workflow.WorkflowTokenPolicyFilename
			}
			if len(secrets) != 0 {
				capabilities = append(capabilities, "secrets")
			}
			sort.Strings(capabilities)
			capabilities = slices.Compact(capabilities)
			if instance.Platform == PlatformDarwinARM64 && slices.Contains(capabilities, "docker") {
				return fmt.Errorf("%s:%d:%d: job %q requires Docker, which is unavailable on darwin/arm64", instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column, instance.LogicalJobID)
			}
			job := plan.Job{
				Schema: plan.SchemaV8,
				Compiler: plan.Compiler{
					Version: compilerVersion, DistributionDigest: compilerDistributionDigest,
				},
				Runtime: &plan.Runtime{DistributionDigest: runtimeDistributionDigest},
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
				GitHubToken:             githubToken,
				Matrix:                  instance.Matrix,
				Vars:                    cloneMap(ir.Vars),
				Dependencies:            append([]string(nil), instance.Needs...),
				NeedSources:             needSources,
				NeedOutputs:             needOutputs,
				Env:                     instance.Env,
				Condition:               instance.If,
				TimeoutMinutes:          instance.TimeoutMinutes,
				DefaultShell:            instance.DefaultShell,
				DefaultWorkingDirectory: instance.DefaultWorkingDirectory,
				Outputs:                 instance.Outputs,
				Steps:                   steps,
				Actions:                 actions,
			}
			job.RequiresMise = &requiresMise
			if instance.Container != nil {
				job.Container = &plan.Container{Image: instance.Container.Image, Env: cloneMap(instance.Container.Env), Ports: append([]string(nil), instance.Container.Ports...)}
			}
			if len(instance.Services) != 0 {
				job.Services = make(map[string]plan.Container, len(instance.Services))
			}
			for _, service := range instance.Services {
				job.Services[service.Name] = plan.Container{Image: service.Container.Image, Env: cloneMap(service.Container.Env), Ports: append([]string(nil), service.Container.Ports...)}
			}
			if err := job.Validate(); err != nil {
				return fmt.Errorf("build plan for job %q: %w", instance.LogicalJobID, err)
			}
			encodedJob, err := plan.Encode(job)
			if err != nil {
				return fmt.Errorf("encode plan for job %q: %w", instance.LogicalJobID, err)
			}
			builtJob = job
			builtAuthorization = authorization
			encoded = encodedJob
			return nil
		}()
		if planErr != nil {
			failedInstances[instance.Key] = true
			evaluations = append(evaluations, JobEvaluation{Instance: instance.Key, Job: instance.LogicalJobID, Evaluated: true})
			diagnostics = append(diagnostics, planConstructionFinding(instance, planErr))
			continue
		}
		digest := sha256.Sum256(encoded)
		planDigests[instance.Key] = "sha256:" + hex.EncodeToString(digest[:])
		plans = append(plans, builtJob)
		authorizations = append(authorizations, builtAuthorization)
		evaluations = append(evaluations, JobEvaluation{Instance: instance.Key, Job: instance.LogicalJobID, Evaluated: true, Passed: true})
	}
	return plans, authorizations, evaluations, errors.Join(diagnostics...)
}

func planConstructionFinding(instance JobInstance, err error) error {
	return &ProcessingFinding{
		Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility",
		Path: instance.SourcePath, Line: instance.Source.Start.Line, Column: instance.Source.Start.Column,
		Job: instance.LogicalJobID, Instance: instance.Key, Err: err,
	}
}

func requiredSecrets(instance JobInstance, actionRequired []string, actionInputsInspected bool) ([]string, error) {
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
		valuesToInspect := []map[string]string{step.Env}
		if step.Kind != "uses" || !actionInputsInspected {
			valuesToInspect = append(valuesToInspect, step.With)
		}
		for _, values := range valuesToInspect {
			for _, name := range sortedValueKeys(values) {
				if err := collect(values[name]); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, name := range actionRequired {
		found[name] = name
	}
	if !instance.secretAuthority {
		for name := range found {
			if name != "GITHUB_TOKEN" {
				delete(found, name)
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
		return IR{}, &ProcessingFinding{
			Code: CodeEnvironment, Category: "environment",
			Message: "workflow-processing configuration is invalid", Err: err,
		}
	}
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return IR{}, processingFinding(StageWorkflowParsing, CodeWorkflowSyntax, "syntax", err)
	}
	workflowTokenPolicyFilename, workflowTokenPolicyDiagnostic := workflowTokenPolicyEvidence(path, parsed)
	event, err := parseEvent(eventSource)
	if err != nil {
		return IR{}, processingFinding(StageEventValidation, CodeEventInvalid, "environment", err)
	}
	event.Trust = options.EventTrust
	vars := options.Vars.snapshot()
	context := compileContext(event, vars, path, parsed.Name)
	workflowConcurrencyGroup, concurrencyErr := resolveConcurrency(path, "", parsed.Concurrency, context, nil)
	concurrencyErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", concurrencyErr)
	cancelInProgress, cancellationErr := resolveWorkflowCancellation(path, parsed.Concurrency, context)
	cancellationErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", cancellationErr)
	expanded, expandErr := expand(path, source, parsed, context, options)
	digest := sha256.Sum256(source)
	ir := IR{
		Schema: schema,
		Workflow: WorkflowSource{
			Path: path, Name: parsed.Name, Digest: "sha256:" + hex.EncodeToString(digest[:]), ConcurrencyGroup: workflowConcurrencyGroup, Triggers: parsed.Triggers,
			WorkflowTokenPolicyFilename: workflowTokenPolicyFilename, WorkflowTokenPolicyDiagnostic: workflowTokenPolicyDiagnostic,
		},
		Event:    event,
		Vars:     vars,
		Warnings: compilerWarnings(parsed.Concurrency, cancelInProgress),
		Execution: ExecutionBoundary{
			Supported: true,
			Reason:    "run-job supports the fail-closed supported shell and local-action subset",
		},
		Jobs: expanded.instances,
	}
	return ir, errors.Join(concurrencyErr, cancellationErr, expandErr)
}

func workflowTokenPolicyEvidence(path string, parsed *workflow.Workflow) (string, string) {
	filename, err := plan.GitHubWorkflowPolicyFilename(canonicalWorkflowName(path))
	if err != nil {
		return "", err.Error()
	}
	if parsed.Permissions == nil || len(parsed.Permissions.Scopes) == 0 {
		return "", "GitHub workflow access tokens require explicit non-empty top-level permissions"
	}
	permissions := make(map[string]string, len(parsed.Permissions.Scopes))
	for name, access := range parsed.Permissions.Scopes {
		permissions[strings.ReplaceAll(name, "-", "_")] = access
	}
	if err := plan.ValidateGitHubWorkflowAccessTokenPermissions(permissions); err != nil {
		return "", err.Error()
	}
	for _, job := range parsed.Jobs {
		if job.Permissions != nil {
			return "", "GitHub workflow access tokens do not support job-level permissions"
		}
		if job.Reusable != nil {
			return "", "GitHub workflow access tokens do not support reusable-workflow jobs"
		}
	}
	return filename, ""
}

func compilerWarnings(concurrency *workflow.Concurrency, cancelInProgress bool) []Warning {
	if concurrency == nil || !cancelInProgress {
		return nil
	}
	position := concurrency.CancelInProgressPosition
	return []Warning{{
		Code:    "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED",
		Line:    position.Line,
		Column:  position.Column,
		Message: "workflow concurrency cancel-in-progress is not enforced; Buildkite pipeline settings can approximate it for same-branch builds",
	}}
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

func compileContext(event Event, vars map[string]string, workflowPath, workflowName string) expression.CompileContext {
	repository := event.Repository.Owner + "/" + event.Repository.Name
	if workflowName == "" {
		workflowName = canonicalWorkflowName(workflowPath)
	}
	return expression.CompileContext{
		GitHub: map[string]any{
			"event_name":       event.Event,
			"event":            event.Payload,
			"repository":       repository,
			"repository_owner": event.Repository.Owner,
			"ref":              event.Ref,
			"sha":              event.SHA,
			"actor":            event.Actor,
			"workflow":         workflowName,
		},
		Event: event.Payload,
		Vars:  vars,
	}
}

func canonicalWorkflowName(path string) string {
	if isRepositoryWorkflowPath(path) {
		root, canonicalPath, err := workflowRepository(path)
		if err == nil {
			if relative, err := repositoryWorkflowPath(root, canonicalPath); err == nil {
				return strings.TrimPrefix(relative, "./")
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func expand(path string, source []byte, parsed *workflow.Workflow, context expression.CompileContext, options Options) (expansionResult, error) {
	resolved, runtimeMatrixBoundary, err := resolveReusableWorkflows(path, source, parsed, context)
	if err != nil {
		notEvaluatedJobs := make(map[string]bool, len(parsed.Jobs))
		for _, job := range parsed.Jobs {
			notEvaluatedJobs[job.ID] = true
		}
		return expansionResult{jobs: parsedJobs(path, parsed), notEvaluatedJobs: notEvaluatedJobs, runtimeMatrixBoundary: runtimeMatrixBoundary}, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", err)
	}
	result := expansionResult{
		jobs:                  processingJobs(path, parsed, resolved),
		runtimeMatrixBoundary: runtimeMatrixBoundary,
		notEvaluatedJobs:      make(map[string]bool), notEvaluatedInstances: make(map[string]bool),
	}
	jobs := make(map[string]workflow.Job, len(resolved))
	sourcePaths := make(map[string]string, len(resolved))
	sourceDigests := make(map[string]string, len(resolved))
	sourceRoots := make(map[string]string, len(resolved))
	secretAuthorities := make(map[string]bool, len(resolved))
	needBindings := make(map[string]map[string]needBinding, len(resolved))
	var diagnostics []error
	failedJobs := make(map[string]bool, len(resolved))
	for _, sourced := range resolved {
		job := sourced.Job
		if _, exists := jobs[job.ID]; exists {
			diagnostics = append(diagnostics, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", sourced.path, 0, 0, job.ID, "", "", 0, jobError(sourced.path, job, fmt.Sprintf("flattened job id %q collides with another job", job.ID))))
			continue
		}
		jobs[job.ID] = job
		sourcePaths[job.ID] = sourced.path
		sourceDigests[job.ID] = sourced.digest
		sourceRoots[job.ID] = sourced.root
		secretAuthorities[job.ID] = sourced.secretAuthority
		needBindings[job.ID] = sourced.needBindings
		if err := supported(sourced.path, job); err != nil {
			diagnostics = append(diagnostics, err)
			failedJobs[job.ID] = true
		}
	}
	order, err := topologicalOrder(path, jobs)
	if err != nil {
		diagnostics = append(diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", err))
		order = sortedKeys(jobs)
	}

	matricesByJob := make(map[string][]map[string]any, len(jobs))
	failedMatrices := make(map[string]bool)
	for _, id := range order {
		job := jobs[id]
		descriptor, deferred, matrixErr := describeRuntimeMatrix(job, sourcePaths[id], sourceDigests[id], needBindings[id], jobs, matricesByJob)
		var matrices []map[string]any
		if deferred {
			result.runtimeMatrixBoundary = true
			position := job.Matrix.Span.Start
			if job.Matrix.Expression != nil {
				position = workflow.Position{Line: job.Matrix.Expression.Span.Start.Line, Column: job.Matrix.Expression.Span.Start.Column}
			} else if job.Matrix.IncludeExpression != nil {
				position = workflow.Position{Line: job.Matrix.IncludeExpression.Span.Start.Line, Column: job.Matrix.IncludeExpression.Span.Start.Column}
			}
			if matrixErr == nil {
				result.runtimeMatrices = append(result.runtimeMatrices, descriptor)
				matrixErr = errors.New("runtime matrix source is valid, but continuation upload is disabled because Buildkite transport has no authoritative current-attempt fence and durable idempotency boundary")
			}
			matrixErr = locatedJobError(sourcePaths[id], job, position.Line, position.Column, matrixErr.Error())
		} else if !deferred {
			matrices, matrixErr = expandMatrix(sourcePaths[id], job, context)
		}
		if matrixErr != nil {
			line, column := job.Span.Start.Line, job.Span.Start.Column
			if job.Matrix != nil {
				line, column = job.Matrix.Span.Start.Line, job.Matrix.Span.Start.Column
				if job.Matrix.Expression != nil {
					line, column = job.Matrix.Expression.Span.Start.Line, job.Matrix.Expression.Span.Start.Column
				} else if job.Matrix.IncludeExpression != nil {
					line, column = job.Matrix.IncludeExpression.Span.Start.Line, job.Matrix.IncludeExpression.Span.Start.Column
				}
			}
			diagnostics = append(diagnostics, &ProcessingFinding{
				Stage: StageMatrix, Code: CodeMatrixInvalid, Category: "compatibility",
				Path: sourcePaths[id], Line: line, Column: column, Job: job.ID,
				Message: "matrix could not be expanded or validated", Err: matrixErr,
			})
			failedMatrices[id] = true
			failedJobs[id] = true
			continue
		}
		matricesByJob[id] = matrices
	}

	byLogicalID := make(map[string][]JobInstance, len(jobs))
	instanceKeys := make(map[string]string)
	for _, id := range order {
		job := jobs[id]
		jobPath := sourcePaths[id]
		if failedMatrices[id] {
			continue
		}
		jobBlocked := false
		for _, binding := range needBindings[id] {
			for _, member := range binding.members {
				if failedJobs[member] {
					jobBlocked = true
				}
			}
		}
		jobFailed := failedJobs[id]
		matrices := matricesByJob[id]
		concurrencyGroups := make(map[string]struct{}, len(matrices))
		for _, matrix := range matrices {
			compileConditionErr := supportedCompileTimeConditions(jobPath, job, context, matrix)
			instanceJob := resolveCompileTimeConditions(job, context, matrix)
			conditionValidationJob := instanceJob
			conditionContext := context
			conditionContext.Matrix = matrix
			if resolved, err := expression.EvaluateCompileCondition(instanceJob.If, conditionContext); err == nil && !resolved {
				instanceJob.If = "false"
				instanceJob.Steps = []workflow.Step{{Name: "Statically disabled job", Kind: "run", Run: ":", If: "false", Span: job.Span}}
			}
			key, err := namespacedInstanceKey(options.StepKeyNamespace, job.ID, matrix)
			if err != nil {
				diagnostics = append(diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", jobPath, 0, 0, job.ID, "", "", 0, jobError(jobPath, job, fmt.Sprintf("create deterministic instance key: %v", err))))
				jobFailed = true
				continue
			}
			if existingJob, exists := instanceKeys[key]; exists {
				diagnostics = append(diagnostics, attributedProcessingFinding(StageMatrix, CodeMatrixInvalid, "compatibility", jobPath, 0, 0, job.ID, "", "", 0, jobError(jobPath, job, fmt.Sprintf("deterministic instance key %q collides with another instance from job %q", key, existingJob))))
				jobFailed = true
				continue
			}
			instanceKeys[key] = job.ID
			candidate := JobInstance{
				Key:                     key,
				LogicalJobID:            job.ID,
				Matrix:                  matrix,
				FailFast:                job.FailFast,
				MaxParallel:             job.MaxParallel,
				Steps:                   append([]workflow.Step(nil), instanceJob.Steps...),
				Env:                     cloneMap(instanceJob.Env),
				Permissions:             permissionScopes(job.Permissions),
				If:                      instanceJob.If,
				TimeoutMinutes:          job.TimeoutMinutes,
				DefaultShell:            job.DefaultShell,
				DefaultWorkingDirectory: job.DefaultWorkingDirectory,
				Outputs:                 cloneMap(job.Outputs),
				Container:               job.Container,
				Services:                append([]workflow.Service(nil), job.Services...),
				SourcePath:              jobPath,
				SourceDigest:            sourceDigests[id],
				RepositoryRoot:          sourceRoots[id],
				Source:                  job.Span,
				secretAuthority:         secretAuthorities[id],
			}
			result.candidates = append(result.candidates, candidate)

			valid := true
			if compileConditionErr != nil {
				diagnostics = append(diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, compileConditionErr))
				valid = false
			} else if err := supportedConditions(jobPath, conditionValidationJob, matrix, true); err != nil {
				diagnostics = append(diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, err))
				valid = false
			}
			labels, runsOnErr := resolveRunsOn(job, context, matrix)
			if runsOnErr != nil {
				diagnostics = append(diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, runsOnPosition(job).Line, runsOnPosition(job).Column, job.ID, key, "", 0, locatedJobError(jobPath, job, runsOnPosition(job).Line, runsOnPosition(job).Column, runsOnErr.Error())))
				valid = false
			}
			var target RunnerTarget
			if runsOnErr == nil {
				target, err = options.Runners.resolve(labels, options.EventTrust)
				if err != nil {
					finding := &ProcessingFinding{
						Stage: StageExpressions, Code: CodeExpressionInvalid, Category: "compatibility",
						Path: jobPath, Line: runsOnPosition(job).Line, Column: runsOnPosition(job).Column,
						Job: job.ID, Instance: key, Message: "resolved runner target is not admitted by policy",
						Err: locatedJobError(jobPath, job, runsOnPosition(job).Line, runsOnPosition(job).Column, err.Error()),
					}
					diagnostics = append(diagnostics, finding)
					valid = false
				}
			}
			concurrencyGroup, concurrencyErr := resolveConcurrency(jobPath, job.ID, job.Concurrency, context, matrix)
			if concurrencyErr != nil {
				diagnostics = append(diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, concurrencyErr))
				valid = false
			}
			if concurrencyGroup != "" {
				concurrencyGroups[canonicalConcurrencyGroup(concurrencyGroup)] = struct{}{}
			}
			if !valid {
				jobFailed = true
				continue
			}
			if jobBlocked {
				result.notEvaluatedJobs[id] = true
				result.notEvaluatedInstances[key] = true
				continue
			}
			instance := candidate
			instance.Label = instanceLabel(job, matrix, context)
			instance.RunsOn = labels
			instance.Queue = target.Queue
			instance.Platform = target.Platform
			instance.RuntimeImage = target.Image
			instance.ConcurrencyGroup = concurrencyGroup
			dependencyFailed := false
			for _, need := range sortedKeys(needBindings[id]) {
				binding := needBindings[id][need]
				var members []string
				for _, member := range binding.members {
					for _, prerequisite := range byLogicalID[member] {
						members = append(members, prerequisite.Key)
					}
				}
				sort.Strings(members)
				if len(members) == 0 {
					diagnostics = append(diagnostics, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", jobPath, 0, 0, job.ID, key, "", 0, jobError(jobPath, job, fmt.Sprintf("prerequisite %q has no expanded instances", need))))
					dependencyFailed = true
					break
				}
				if instance.NeedGroups == nil {
					instance.NeedGroups = make(map[string][]string, len(needBindings[id]))
				}
				instance.NeedGroups[need] = members
				instance.Needs = append(instance.Needs, members...)
				if binding.projectOutputs {
					if instance.NeedOutputs == nil {
						instance.NeedOutputs = make(map[string][]NeedOutput)
					}
					projected := []NeedOutput{}
					for _, output := range binding.outputs {
						producers := byLogicalID[output.member]
						if len(producers) == 0 {
							diagnostics = append(diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", fmt.Errorf("%s:%d:%d: workflow_call output %q selects unexpanded job %q", output.path, output.span.Start.Line, output.span.Start.Column, output.name, output.member)))
							continue
						}
						if len(projected)+len(producers) > plan.MaxNeedOutputs {
							diagnostics = append(diagnostics, processingFinding(StageGraph, CodeGraphInvalid, "compatibility", fmt.Errorf("%s:%d:%d: workflow_call output %q expands call projections beyond the maximum of %d", output.path, output.span.Start.Line, output.span.Start.Column, output.name, plan.MaxNeedOutputs)))
							continue
						}
						for _, producer := range producers {
							projected = append(projected, NeedOutput{Name: output.name, StepKey: producer.Key, Output: output.output})
						}
					}
					sort.Slice(projected, func(i, j int) bool {
						if projected[i].Name != projected[j].Name {
							return projected[i].Name < projected[j].Name
						}
						if projected[i].StepKey != projected[j].StepKey {
							return projected[i].StepKey < projected[j].StepKey
						}
						return projected[i].Output < projected[j].Output
					})
					instance.NeedOutputs[need] = projected
				}
			}
			if dependencyFailed {
				jobFailed = true
				continue
			}
			sort.Strings(instance.Needs)
			instance.Needs = slices.Compact(instance.Needs)
			byLogicalID[id] = append(byLogicalID[id], instance)
		}
		if job.MaxParallel != nil && len(concurrencyGroups) > 1 {
			position := job.Concurrency.Span.Start
			diagnostics = append(diagnostics, attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", jobPath, position.Line, position.Column, job.ID, "", "", 0, locatedJobError(jobPath, job, position.Line, position.Column, "concurrency groups that vary by matrix cannot be combined with strategy.max-parallel")))
			jobFailed = true
		}
		failedJobs[id] = jobFailed || jobBlocked
	}

	var instances []JobInstance
	for _, id := range order {
		instances = append(instances, byLogicalID[id]...)
	}
	result.instances = instances
	return result, errors.Join(diagnostics...)
}

func resolveConcurrency(path, jobID string, concurrency *workflow.Concurrency, context expression.CompileContext, matrix map[string]any) (string, error) {
	if concurrency == nil {
		return "", nil
	}
	context.Matrix = matrix
	group, err := expression.EvaluateCompileTemplate(concurrency.Group, context)
	if err != nil {
		message := fmt.Sprintf("concurrency group cannot be resolved at compile time: %v", err)
		if jobID == "" {
			return "", fmt.Errorf("%s:%d:%d: workflow %s", path, concurrency.Span.Start.Line, concurrency.Span.Start.Column, message)
		}
		return "", locatedJobError(path, workflow.Job{ID: jobID}, concurrency.Span.Start.Line, concurrency.Span.Start.Column, message)
	}
	if strings.TrimSpace(group) == "" {
		if jobID == "" {
			return "", fmt.Errorf("%s:%d:%d: workflow concurrency group resolved to an empty string", path, concurrency.Span.Start.Line, concurrency.Span.Start.Column)
		}
		return "", locatedJobError(path, workflow.Job{ID: jobID}, concurrency.Span.Start.Line, concurrency.Span.Start.Column, "concurrency group resolved to an empty string")
	}
	return group, nil
}

func resolveWorkflowCancellation(path string, concurrency *workflow.Concurrency, context expression.CompileContext) (bool, error) {
	if concurrency == nil || concurrency.CancelInProgressExpression == nil {
		return concurrency != nil && concurrency.CancelInProgress, nil
	}
	value, err := expression.EvaluateCompile(*concurrency.CancelInProgressExpression, context)
	if err != nil {
		position := concurrency.CancelInProgressPosition
		return false, fmt.Errorf("%s:%d:%d: workflow concurrency cancel-in-progress cannot be resolved at compile time: %v", path, position.Line, position.Column, err)
	}
	cancelInProgress, ok := value.(bool)
	if !ok {
		position := concurrency.CancelInProgressPosition
		return false, fmt.Errorf("%s:%d:%d: workflow concurrency cancel-in-progress resolved to %T, want boolean", path, position.Line, position.Column, value)
	}
	return cancelInProgress, nil
}

func canonicalConcurrencyGroup(group string) string {
	return strings.ToLower(group)
}

func permissionScopes(permissions *workflow.Permissions) map[string]string {
	if permissions == nil {
		return nil
	}
	return cloneMap(permissions.Scopes)
}

func supported(path string, job workflow.Job) error {
	if job.Reusable != nil {
		return attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", path, 0, 0, job.ID, "", "", 0, jobError(path, job, "internal error: unresolved reusable-workflow job"))
	}
	if len(job.RunsOn) == 0 && job.RunsOnExpr == nil {
		return attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", path, 0, 0, job.ID, "", "", 0, jobError(path, job, "runs-on must resolve statically"))
	}
	ids := make(map[string]struct{}, len(job.Steps))
	for _, step := range job.Steps {
		if step.ID != "" {
			id := strings.ToLower(step.ID)
			if _, exists := ids[id]; exists {
				return attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", path, step.Span.Start.Line, step.Span.Start.Column, job.ID, "", "", 0, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("duplicate step id %q", step.ID)))
			}
			ids[id] = struct{}{}
		}
	}
	return nil
}

func resolveCompileTimeConditions(job workflow.Job, context expression.CompileContext, matrix map[string]any) workflow.Job {
	context.Matrix = matrix
	if resolved, ok := resolveCompileTimeCondition(job.If, context); ok {
		job.If = resolved
	}
	job.Steps = append([]workflow.Step(nil), job.Steps...)
	for i := range job.Steps {
		step := &job.Steps[i]
		if resolved, ok := resolveCompileTimeCondition(step.If, context); ok {
			step.If = resolved
		}
	}
	return job
}

func resolveCompileTimeCondition(source string, context expression.CompileContext) (string, bool) {
	usesEvent, err := expression.ReferencesGitHubEvent(source)
	if err != nil || !usesEvent {
		return source, false
	}
	resolved, err := expression.EvaluateCompileCondition(source, context)
	if err != nil {
		return source, false
	}
	if resolved {
		referencesStatus, _ := expression.ReferencesStatusFunction(source)
		if referencesStatus {
			return "always()", true
		}
	}
	return strconv.FormatBool(resolved), true
}

func supportedConditions(path string, job workflow.Job, matrix map[string]any, matrixKnown bool) error {
	validate := expression.ValidateCondition
	if matrixKnown {
		validate = func(source string, scope expression.ConditionScope) error {
			return expression.ValidateConditionWithMatrix(source, scope, matrix)
		}
	}
	return validateConditions(path, job, validate)
}

func supportedCompileTimeConditions(path string, job workflow.Job, context expression.CompileContext, matrix map[string]any) error {
	validate := func(source string, scope expression.ConditionScope) error {
		usesEvent, err := expression.ReferencesGitHubEvent(source)
		if err != nil || !usesEvent {
			return nil
		}
		return expression.ValidateCompileConditionWithMatrix(source, scope, context, matrix)
	}
	return validateConditions(path, job, validate)
}

func validateConditions(path string, job workflow.Job, validate func(string, expression.ConditionScope) error) error {
	var diagnostics []error
	if err := validate(job.If, expression.JobCondition); err != nil {
		position := job.IfSpan.Start
		if position.Line == 0 {
			position = job.Span.Start
		}
		diagnostics = append(diagnostics, locatedJobError(path, job, position.Line, position.Column, fmt.Sprintf("job condition: %v", err)))
	}
	for i, step := range job.Steps {
		if err := validate(step.If, expression.StepCondition); err != nil {
			position := step.IfSpan.Start
			if position.Line == 0 {
				position = step.Span.Start
			}
			label := fmt.Sprintf("step %d", i+1)
			if step.ID != "" {
				label = fmt.Sprintf("step %q", step.ID)
			}
			diagnostics = append(diagnostics, locatedJobError(path, job, position.Line, position.Column, fmt.Sprintf("%s condition: %v", label, err)))
		}
	}
	return errors.Join(diagnostics...)
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

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
		} else if job.Matrix.IncludeExpression != nil {
			position = workflow.Position{Line: job.Matrix.IncludeExpression.Span.Start.Line, Column: job.Matrix.IncludeExpression.Span.Start.Column}
		}
		return nil, locatedJobError(path, job, position.Line, position.Column, err.Error())
	}
	matrices, err := expandMatrixDefinition(matrix)
	if err != nil {
		return nil, jobError(path, job, err.Error())
	}
	return matrices, nil
}

func expandMatrixDefinition(matrix *workflow.Matrix) ([]map[string]any, error) {
	matrices := []map[string]any{{}}
	if len(matrix.Rows) == 0 {
		matrices = nil
	}
	for _, row := range matrix.Rows {
		if len(row.Values) == 0 {
			return nil, fmt.Errorf("matrix dimension %q has no values", row.Name)
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
					return nil, fmt.Errorf("matrix expands beyond %d instances", maxMatrixInstances)
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
			return nil, fmt.Errorf("matrix expands beyond %d instances", maxMatrixInstances)
		}
	}
	matrices = matrices[:0]
	for _, matrix := range included {
		matrices = append(matrices, matrix.values)
	}
	if len(matrices) == 0 {
		return nil, fmt.Errorf("matrix excludes every combination")
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
	if matrix.IncludeExpression != nil {
		return nil, fmt.Errorf("runtime-dependent matrix include expressions are unsupported")
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
	return namespacedInstanceKey("", jobID, matrix)
}

func namespacedInstanceKey(namespace, jobID string, matrix map[string]any) (string, error) {
	prefix := "gha-"
	if namespace != "" {
		prefix += namespace + "-"
	}
	prefix += sanitize(jobID)
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

func instanceLabel(job workflow.Job, matrix map[string]any, context expression.CompileContext) string {
	label := job.Name
	if label == "" {
		label = job.ID
	} else {
		context.Matrix = matrix
		if resolved, err := expression.EvaluateCompileTemplate(label, context); err == nil {
			label = resolved
			withoutMatrix := context
			withoutMatrix.Matrix = nil
			if _, err := expression.EvaluateCompileTemplate(job.Name, withoutMatrix); err != nil {
				return label
			}
		}
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
