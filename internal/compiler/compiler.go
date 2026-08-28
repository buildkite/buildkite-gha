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
	"maps"
	"sort"
	"strconv"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const schema = "buildkite-gha/compiler-ir/v1"

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

// WorkflowConcurrencyGate is one statically resolved called-workflow
// concurrency scope inherited by a flattened job.
type WorkflowConcurrencyGate struct {
	ID    string `json:"id"`
	Group string `json:"group"`
}

// Warning is one source-located, non-fatal compatibility diagnostic.
type Warning struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Job     string `json:"job,omitempty"`
	Step    int    `json:"step,omitempty"`
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
	RunName                       string             `json:"run_name,omitempty"`
	Digest                        string             `json:"digest"`
	ConcurrencyGroup              string             `json:"concurrency_group,omitempty"`
	Triggers                      []workflow.Trigger `json:"-"`
	WorkflowTokenPolicyFilename   string             `json:"-"`
	WorkflowTokenPermissions      map[string]string  `json:"-"`
	WorkflowTokenPolicyDiagnostic string             `json:"-"`
}

// JobInstance is one statically expanded job in the owned IR.
type JobInstance struct {
	Key                     string                    `json:"key"`
	LogicalJobID            string                    `json:"logical_job_id"`
	Label                   string                    `json:"label"`
	Needs                   []string                  `json:"needs,omitempty"`
	NeedGroups              map[string][]string       `json:"need_groups,omitempty"`
	NeedOutputs             map[string][]NeedOutput   `json:"need_outputs,omitempty"`
	CallGuards              []CallGuard               `json:"call_guards,omitempty"`
	RunsOn                  []string                  `json:"runs_on"`
	Queue                   string                    `json:"queue"`
	Platform                Platform                  `json:"-"`
	RuntimeImage            string                    `json:"runtime_image,omitempty"`
	Cache                   *CacheVolume              `json:"-"`
	Matrix                  map[string]any            `json:"matrix,omitempty"`
	Inputs                  map[string]any            `json:"inputs,omitempty"`
	DeferredInputs          map[string]DeferredInput  `json:"deferred_inputs,omitempty"`
	FailFast                *bool                     `json:"fail_fast,omitempty"`
	MaxParallel             *int                      `json:"max_parallel,omitempty"`
	ConcurrencyGroup        string                    `json:"concurrency_group,omitempty"`
	ConcurrencyGates        []WorkflowConcurrencyGate `json:"workflow_concurrency_gates,omitempty"`
	Steps                   []workflow.Step           `json:"steps"`
	Env                     map[string]string         `json:"env,omitempty"`
	Permissions             map[string]string         `json:"permissions,omitempty"`
	If                      string                    `json:"if,omitempty"`
	ContinueOnError         bool                      `json:"continue_on_error,omitempty"`
	TimeoutMinutes          float64                   `json:"timeout_minutes,omitempty"`
	DefaultShell            string                    `json:"default_shell,omitempty"`
	DefaultWorkingDirectory string                    `json:"default_working_directory,omitempty"`
	Outputs                 map[string]string         `json:"outputs,omitempty"`
	Container               *workflow.Container       `json:"container,omitempty"`
	Services                []workflow.Service        `json:"services,omitempty"`
	ServicesExpression      string                    `json:"services_expression,omitempty"`
	SourcePath              string                    `json:"source_path"`
	SourceDigest            string                    `json:"source_digest"`
	RemoteWorkflow          *RemoteWorkflowSource     `json:"remote_workflow,omitempty"`
	RepositoryRoot          string                    `json:"-"`
	Source                  workflow.Span             `json:"source"`
	RetainEventPayload      bool                      `json:"-"`
	secretAuthority         secretAuthority
	tokenPolicyNarrowed     bool
	jobPermissionsIgnored   bool
	reusableCall            workflow.Position
}

// CallGuard is one immutable caller-scoped condition inherited by a flattened
// reusable-workflow job.
type CallGuard struct {
	Condition      string                   `json:"condition"`
	Inputs         map[string]any           `json:"inputs,omitempty"`
	DeferredInputs map[string]DeferredInput `json:"deferred_inputs,omitempty"`
	NeedGroups     map[string][]string      `json:"need_groups,omitempty"`
	NeedOutputs    map[string][]NeedOutput  `json:"need_outputs,omitempty"`
}

// DeferredInput binds one string workflow_call input to exact prerequisite
// outputs without exposing the caller's needs context to the callee.
type DeferredInput struct {
	Sources []string     `json:"sources"`
	Outputs []NeedOutput `json:"outputs,omitempty"`
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

// ParseWorkflow reports only event-independent workflow syntax and job
// identities. Later stages deliberately remain unevaluated.
func ParseWorkflow(path string, source []byte) (Report, error) {
	parsed, err := parseReusableWorkflow(path, source)
	if err != nil {
		return Report{}, processingFinding(StageWorkflowParsing, CodeWorkflowSyntax, "syntax", err)
	}
	return Report{LogicalJobs: len(parsed.Jobs), ParsedJobs: parsedJobs(path, parsed), Warnings: compilerWarnings(parsed, false)}, nil
}

// Validate parses and validates the supported static graph without requiring an
// event snapshot.
func Validate(path string, source []byte) (Report, error) {
	return ValidateWithOptions(path, source, defaultOptions())
}

// ValidateWithOptions validates the static graph against an explicit runner
// policy without requiring an event snapshot.
func ValidateWithOptions(path string, source []byte, options Options) (Report, error) {
	return ValidateWithOptionsContext(context.Background(), path, source, options)
}

// ValidateWithOptionsContext validates the static graph and permits
// cancellation while resolving public reusable-workflow source.
func ValidateWithOptionsContext(ctx context.Context, path string, source []byte, options Options) (Report, error) {
	var optionsErr error
	if err := options.validate(); err != nil {
		optionsErr = &ProcessingFinding{
			Code: CodeEnvironment, Category: "environment",
			Message: "workflow-processing configuration is invalid", Err: err,
		}
	}
	parsed, parseErr := parseReusableWorkflow(path, source)
	parseErr = processingFinding(StageWorkflowParsing, CodeWorkflowSyntax, "syntax", parseErr)
	if parseErr != nil {
		return Report{}, errors.Join(parseErr, optionsErr)
	}
	triggerErr := processingFinding(StagePipeline, CodePipelineGeneration, "compatibility", buildkitepipeline.ValidateTriggerConditions(parsed.Triggers))
	event := Event{
		Event: "validation", Trust: options.EventTrust,
		Repository: Repository{Owner: "validation", Name: "validation"},
		Ref:        "refs/heads/validation", SHA: strings.Repeat("0", 40), Actor: "validation",
		Payload: map[string]any{},
	}
	context := compileContext(event, nil, path, parsed.Name)
	context.Inputs = workflowDispatchInputs(parsed, event)
	context.GitHub["head_ref"] = "validation"
	runNameErr := validateWorkflowRunName(path, parsed)
	_, concurrencyErr := resolveConcurrency(path, "", parsed.Concurrency, context, nil)
	concurrencyErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", concurrencyErr)
	cancelInProgress, cancellationErr := resolveWorkflowCancellation(path, parsed.Concurrency, context)
	cancellationErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", cancellationErr)
	expanded, expandErr := expandJobGraph(ctx, path, source, parsed, context, options)
	if err := errors.Join(optionsErr, triggerErr, runNameErr, concurrencyErr, cancellationErr, expandErr); err != nil {
		return jobGraphExpansionReport(expanded, compilerWarnings(parsed, cancelInProgress)), err
	}
	return jobGraphExpansionReport(expanded, compilerWarnings(parsed, cancelInProgress)), nil
}

// ValidateEvent validates both the supported static graph and its event input.
func ValidateEvent(path string, source, eventSource []byte) (Report, error) {
	return ValidateEventWithOptions(path, source, eventSource, defaultOptions())
}

// ValidateEventWithOptions validates the graph against explicit variables and
// runner policy without producing compiler output.
func ValidateEventWithOptions(path string, source, eventSource []byte, options Options) (Report, error) {
	return ValidateEventWithOptionsContext(context.Background(), path, source, eventSource, options)
}

// ValidateEventWithOptionsContext validates the graph and event while
// permitting cancellation during public source resolution.
func ValidateEventWithOptionsContext(ctx context.Context, path string, source, eventSource []byte, options Options) (Report, error) {
	var optionsErr error
	if err := options.validate(); err != nil {
		optionsErr = &ProcessingFinding{
			Code: CodeEnvironment, Category: "environment",
			Message: "workflow-processing configuration is invalid", Err: err,
		}
	}
	parsed, parseErr := parseReusableWorkflow(path, source)
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
			Warnings: compilerWarnings(parsed, false), NotEvaluatedJobs: notEvaluatedJobs,
		}, errors.Join(parseErr, eventErr, optionsErr)
	}
	event.Trust = options.EventTrust
	context := compileContext(event, options.Vars.snapshot(), path, parsed.Name)
	context.Inputs = workflowDispatchInputs(parsed, event)
	_, runNameErr := resolveWorkflowRunName(path, parsed, context)
	_, concurrencyErr := resolveConcurrency(path, "", parsed.Concurrency, context, nil)
	concurrencyErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", concurrencyErr)
	cancelInProgress, cancellationErr := resolveWorkflowCancellation(path, parsed.Concurrency, context)
	cancellationErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", cancellationErr)
	expanded, expandErr := expandJobGraph(ctx, path, source, parsed, context, options)
	if err := errors.Join(runNameErr, concurrencyErr, cancellationErr, expandErr); err != nil {
		return jobGraphExpansionReport(expanded, compilerWarnings(parsed, cancelInProgress)), err
	}
	return jobGraphExpansionReport(expanded, compilerWarnings(parsed, cancelInProgress)), nil
}

// Compile parses a workflow and event, expands its static graph, and returns
// stable JSON bytes terminated by a newline.
func Compile(path string, source, eventSource []byte) ([]byte, error) {
	return CompileWithOptions(path, source, eventSource, defaultOptions())
}

// CompileWithOptions compiles using an explicit non-secret variable snapshot,
// event trust classification, and runner policy.
func CompileWithOptions(path string, source, eventSource []byte, options Options) ([]byte, error) {
	return CompileWithOptionsContext(context.Background(), path, source, eventSource, options)
}

// CompileWithOptionsContext compiles a workflow and permits cancellation while
// resolving public reusable-workflow source.
func CompileWithOptionsContext(ctx context.Context, path string, source, eventSource []byte, options Options) ([]byte, error) {
	ir, err := compile(ctx, path, source, eventSource, options)
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

func compile(ctx context.Context, path string, source, eventSource []byte, options Options) (IR, error) {
	if err := options.validate(); err != nil {
		return IR{}, &ProcessingFinding{
			Code: CodeEnvironment, Category: "environment",
			Message: "workflow-processing configuration is invalid", Err: err,
		}
	}
	parsed, err := parseReusableWorkflow(path, source)
	if err != nil {
		return IR{}, processingFinding(StageWorkflowParsing, CodeWorkflowSyntax, "syntax", err)
	}
	workflowTokenPolicyFilename, workflowTokenPermissions, workflowTokenPolicyDiagnostic := workflowTokenPolicyEvidence(path, parsed)
	event, err := parseEvent(eventSource)
	if err != nil {
		return IR{}, processingFinding(StageEventValidation, CodeEventInvalid, "environment", err)
	}
	event.Trust = options.EventTrust
	vars := options.Vars.snapshot()
	context := compileContext(event, vars, path, parsed.Name)
	context.Inputs = workflowDispatchInputs(parsed, event)
	runName, runNameErr := resolveWorkflowRunName(path, parsed, context)
	workflowConcurrencyGroup, concurrencyErr := resolveConcurrency(path, "", parsed.Concurrency, context, nil)
	concurrencyErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", concurrencyErr)
	cancelInProgress, cancellationErr := resolveWorkflowCancellation(path, parsed.Concurrency, context)
	cancellationErr = processingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", cancellationErr)
	expanded, expandErr := expandJobGraph(ctx, path, source, parsed, context, options)
	digest := sha256.Sum256(source)
	ir := IR{
		Schema: schema,
		Workflow: WorkflowSource{
			Path: path, Name: parsed.Name, RunName: runName, Digest: "sha256:" + hex.EncodeToString(digest[:]), ConcurrencyGroup: workflowConcurrencyGroup, Triggers: parsed.Triggers,
			WorkflowTokenPolicyFilename: workflowTokenPolicyFilename, WorkflowTokenPermissions: workflowTokenPermissions, WorkflowTokenPolicyDiagnostic: workflowTokenPolicyDiagnostic,
		},
		Event:    event,
		Vars:     vars,
		Warnings: append(compilerWarnings(parsed, cancelInProgress), expanded.warnings...),
		Execution: ExecutionBoundary{
			Supported: true,
			Reason:    "run-job rejects unsupported shells and local actions",
		},
		Jobs: expanded.instances,
	}
	return ir, errors.Join(runNameErr, concurrencyErr, cancellationErr, expandErr)
}

// ResolveWorkflowRunName evaluates one parsed workflow's explicit run-name
// against the event snapshot used for compilation. Dispatch inputs are only
// available when the workflow applies to the event.
func ResolveWorkflowRunName(path string, parsed *workflow.Workflow, event Event, applicable bool) (string, error) {
	context := compileContext(event, nil, path, parsed.Name)
	if applicable {
		context.Inputs = workflowDispatchInputs(parsed, event)
	}
	return resolveWorkflowRunName(path, parsed, context)
}

func resolveWorkflowRunName(path string, parsed *workflow.Workflow, context expression.CompileContext) (string, error) {
	if strings.TrimSpace(parsed.RunName) == "" {
		return "", nil
	}
	resolved, err := expression.EvaluateRunName(parsed.RunName, context)
	if err == nil {
		if strings.TrimSpace(resolved) == "" {
			return "", nil
		}
		return resolved, nil
	}
	position := parsed.RunNameSpan.Start
	return "", attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", path, position.Line, position.Column, "", "", "", 0,
		fmt.Errorf("%s:%d:%d: workflow run-name: %w", path, position.Line, position.Column, err))
}

func validateWorkflowRunName(path string, parsed *workflow.Workflow) error {
	if strings.TrimSpace(parsed.RunName) == "" {
		return nil
	}
	if err := expression.ValidateRunName(parsed.RunName); err != nil {
		position := parsed.RunNameSpan.Start
		return attributedProcessingFinding(StageExpressions, CodeExpressionInvalid, "compatibility", path, position.Line, position.Column, "", "", "", 0,
			fmt.Errorf("%s:%d:%d: workflow run-name: %w", path, position.Line, position.Column, err))
	}
	return nil
}

func workflowTokenPolicyEvidence(path string, parsed *workflow.Workflow) (string, map[string]string, string) {
	filename, filenameErr := plan.GitHubWorkflowPolicyFilename(canonicalWorkflowName(path))
	permissions := defaultGitHubTokenPermissions().Scopes
	if parsed.Permissions != nil {
		permissions = parsed.Permissions.Scopes
	}
	normalizedPermissions := make(map[string]string, len(permissions))
	for name, access := range permissions {
		if name == "id-token" {
			continue
		}
		normalizedPermissions[strings.ReplaceAll(name, "-", "_")] = access
	}
	if filenameErr != nil {
		return "", normalizedPermissions, filenameErr.Error()
	}
	if parsed.Permissions != nil && len(parsed.Permissions.Scopes) == 0 {
		return "", nil, "GitHub workflow access tokens require explicit non-empty top-level permissions"
	}
	if err := plan.ValidateGitHubWorkflowAccessTokenPermissions(normalizedPermissions); err != nil {
		return "", normalizedPermissions, err.Error()
	}
	return filename, normalizedPermissions, ""
}

func compilerWarnings(parsed *workflow.Workflow, cancelInProgress bool) []Warning {
	var warnings []Warning
	supported := make(map[string]bool, len(parsed.Triggers))
	for _, trigger := range parsed.Triggers {
		if buildkitepipeline.SupportedTriggerEvent(trigger.Event) {
			supported[trigger.Event] = true
		}
	}
	supportedNames := make([]string, 0, len(supported))
	for event := range supported {
		supportedNames = append(supportedNames, event)
	}
	sort.Strings(supportedNames)
	for _, trigger := range parsed.Triggers {
		if trigger.Event == "merge_group" && (trigger.Paths != nil || trigger.PathsIgnore != nil) {
			warnings = append(warnings, mergeGroupPathFiltersWarning(trigger.Position))
		}
		if buildkitepipeline.SupportedTriggerEvent(trigger.Event) {
			continue
		}
		message := fmt.Sprintf("on.%s is ignored, so nothing in this workflow runs from it.", trigger.Event)
		if len(supportedNames) == 0 {
			message += " This workflow declares no supported triggers that still run."
		} else {
			message += " The supported triggers declared in this workflow still run: " + strings.Join(supportedNames, ", ") + "."
			message += " Move the jobs this trigger guards to one of those triggers if you need them."
		}
		message += fmt.Sprintf(" If you need %s, log an issue on https://github.com/buildkite/buildkite-gha so we can prioritise it.", trigger.Event)
		warnings = append(warnings, Warning{
			Code:    "W_TRIGGER_EVENT_UNSUPPORTED",
			Line:    trigger.Position.Line,
			Column:  trigger.Position.Column,
			Message: message,
		})
	}
	if parsed.Concurrency != nil && cancelInProgress {
		warnings = append(warnings, workflowCancellationWarning(parsed.Concurrency.CancelInProgressPosition))
	}
	return warnings
}

func mergeGroupPathFiltersWarning(position workflow.Position) Warning {
	return Warning{
		Code:    "W_MERGE_GROUP_PATH_FILTERS_IGNORED",
		Line:    position.Line,
		Column:  position.Column,
		Message: "on.merge_group paths and paths-ignore are ignored, matching GitHub, which does not evaluate path filters for merge_group events. Every merge_group delivery runs this workflow. Move the filtering into a job or step condition if you need it.",
	}
}

func workflowCancellationWarning(position workflow.Position) Warning {
	return Warning{
		Code:    "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED",
		Line:    position.Line,
		Column:  position.Column,
		Message: "cancel-in-progress is ignored, so superseded builds keep running. Buildkite handles this as a pipeline setting rather than in the workflow file. Turn on Cancel Intermediate Builds under Settings > Builds. It cancels earlier running builds on the same branch, rather than per concurrency group.",
	}
}

func legacyCheckoutWarning(position workflow.Position, release string, defaultsToFullHistory bool) Warning {
	generation := "v2"
	if defaultsToFullHistory {
		generation = "v1"
	}
	message := fmt.Sprintf("actions/checkout %s behaves like %s. It does not set the ref and commit outputs, which actions/checkout added in v4.2.0.", release, generation)
	if defaultsToFullHistory {
		message += " It also defaults to full history when fetch-depth is omitted. Upgrade to actions/checkout v4 or later if either difference matters."
	} else {
		message += " Upgrade to actions/checkout v4 or later if a later step reads either output."
	}
	return Warning{
		Code:    "W_CHECKOUT_LEGACY_RELEASE",
		Line:    position.Line,
		Column:  position.Column,
		Message: message,
	}
}

func unknownCheckoutCommitWarning(position workflow.Position, commit string) Warning {
	return Warning{
		Code:   "W_CHECKOUT_UNKNOWN_COMMIT_FALLBACK",
		Line:   position.Line,
		Column: position.Column,
		Message: fmt.Sprintf("actions/checkout resolved to immutable commit %s, which is absent from the frozen per-commit snapshot. The native adapter is using the supported %s contract instead; it still restricts repository, ref, path, credentials, and other inputs, and does not run the upstream action JavaScript.",
			commit, actionintegration.CheckoutFallbackContractRelease),
	}
}

func unknownUploadArtifactCommitWarning(position workflow.Position, commit string) Warning {
	return Warning{
		Code:   "W_UPLOAD_ARTIFACT_UNKNOWN_COMMIT_FALLBACK",
		Line:   position.Line,
		Column: position.Column,
		Message: fmt.Sprintf("actions/upload-artifact resolved to immutable commit %s, which is outside the exact admission set. The native adapter is using the supported %s contract instead; it still restricts names, paths, archive mode, overwrite, hidden files, sizes, and outputs, and does not run the upstream action JavaScript.",
			commit, actionintegration.UploadArtifactFallbackContractRelease),
	}
}

func legacyUploadArtifactWarning(position workflow.Position, release string) Warning {
	return Warning{
		Code:    "W_UPLOAD_ARTIFACT_LEGACY_RELEASE",
		Line:    position.Line,
		Column:  position.Column,
		Message: fmt.Sprintf("actions/upload-artifact %s is emulated by the native adapter with its release contract; upgrade to v4 or later", release),
	}
}

func reusableWorkflowTokenWarning(position workflow.Position) Warning {
	return Warning{
		Code:    "W_REUSABLE_WORKFLOW_TOKEN_USES_ROOT_PERMISSIONS",
		Line:    position.Line,
		Column:  position.Column,
		Message: "jobs expanded from local reusable workflows use the top-level requesting workflow permissions for GITHUB_TOKEN; permissions declared in called workflows are not enforced",
	}
}

func jobWorkflowTokenWarning(position workflow.Position, permissions map[string]string) Warning {
	names := make([]string, 0, len(permissions))
	for name := range permissions {
		names = append(names, name)
	}
	sort.Strings(names)
	effective := make([]string, 0, len(names))
	for _, name := range names {
		effective = append(effective, strings.ReplaceAll(name, "_", "-")+": "+permissions[name])
	}
	return Warning{
		Code:    "W_JOB_GITHUB_TOKEN_USES_WORKFLOW_PERMISSIONS",
		Line:    position.Line,
		Column:  position.Column,
		Message: "Job-level permissions are ignored for GITHUB_TOKEN. The top-level workflow permissions apply instead. This job's token has " + strings.Join(effective, ", ") + ". Move this job's permissions block to the workflow top level. If you need per-job permissions, log an issue on https://github.com/buildkite/buildkite-gha so we can prioritise it.",
	}
}

func resolveCompileContainer(container *workflow.Container, context expression.CompileContext) (*workflow.Container, error) {
	if container == nil {
		return nil, nil
	}
	resolved := *container
	resolved.Env = cloneMap(container.Env)
	resolved.Ports = append([]string(nil), container.Ports...)
	if strings.Contains(resolved.Image, "${{") {
		image, err := expression.EvaluateCompileStringTemplate(resolved.Image, context)
		if err != nil {
			return nil, err
		}
		resolved.Image = image
	}
	if strings.TrimSpace(resolved.Image) == "" {
		return nil, fmt.Errorf("container image resolved to an empty string")
	}
	if len(resolved.Image) > 512 || !plan.ValidContainerImageReference(resolved.Image) {
		return nil, fmt.Errorf("container image resolved to an invalid image reference")
	}
	return &resolved, nil
}

func resolveCompileServices(services []workflow.Service, context expression.CompileContext) ([]workflow.Service, error) {
	resolved := make([]workflow.Service, 0, len(services))
	for _, service := range services {
		container := service.Container
		container.Env = cloneMap(container.Env)
		container.Ports = append([]string(nil), container.Ports...)
		container.Volumes = append([]string(nil), container.Volumes...)
		if container.Credentials != nil {
			credentials := *container.Credentials
			container.Credentials = &credentials
			for _, field := range []*string{&container.Credentials.Username, &container.Credentials.Password} {
				if err := expression.ValidateServiceCredentialTemplate(*field); err != nil {
					return nil, fmt.Errorf("service %q credentials: %w", service.Name, err)
				}
				value, err := expression.EvaluateAvailableCompileTemplate(*field, context)
				if err != nil {
					return nil, fmt.Errorf("service %q credentials: %w", service.Name, err)
				}
				*field = value
			}
		}
		fields := []*string{&container.Image, &container.Options, &container.Command, &container.Entrypoint}
		for _, field := range fields {
			value, err := expression.EvaluateAvailableCompileTemplate(*field, context)
			if err != nil {
				return nil, fmt.Errorf("service %q: %w", service.Name, err)
			}
			*field = value
		}
		for key, value := range container.Env {
			resolvedValue, err := expression.EvaluateAvailableCompileTemplate(value, context)
			if err != nil {
				return nil, fmt.Errorf("service %q environment %q: %w", service.Name, key, err)
			}
			container.Env[key] = resolvedValue
		}
		for _, values := range [][]string{container.Ports, container.Volumes} {
			for j, value := range values {
				resolvedValue, err := expression.EvaluateAvailableCompileTemplate(value, context)
				if err != nil {
					return nil, fmt.Errorf("service %q: %w", service.Name, err)
				}
				values[j] = resolvedValue
			}
		}
		for _, value := range append(append(append([]string{container.Image, container.Options, container.Command, container.Entrypoint}, container.Ports...), container.Volumes...), mapValues(container.Env)...) {
			if strings.Contains(value, "${{") {
				if err := expression.ValidateServiceRuntimeTemplate(value); err != nil {
					return nil, fmt.Errorf("service %q: %w", service.Name, err)
				}
			}
		}
		if container.Image == "" {
			continue
		}
		service.Container = container
		resolved = append(resolved, service)
	}
	return resolved, nil
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
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

func rejectJobCancellation(path string, job workflow.Job) error {
	if job.Concurrency == nil || (!job.Concurrency.CancelInProgress && job.Concurrency.CancelInProgressExpression == nil) {
		return nil
	}
	position := job.Concurrency.CancelInProgressPosition
	return locatedJobError(path, job, position.Line, position.Column, "concurrency cancel-in-progress is unsupported")
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
	for i, step := range job.Steps {
		if step.Kind == "uses" && strings.HasPrefix(strings.ToLower(step.Uses), "docker://") {
			return attributedProcessingFinding(
				StageGraph, CodeGraphInvalid, "compatibility", path, step.Span.Start.Line, step.Span.Start.Column,
				job.ID, "", step.Uses, i+1,
				locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, actionsource.UnsupportedContainerActionReason),
			)
		}
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
	if err == nil {
		if resolved {
			referencesStatus, _ := expression.ReferencesStatusFunction(source)
			if referencesStatus {
				return "always()", true
			}
		}
		return strconv.FormatBool(resolved), true
	}
	reduced, err := expression.ReduceCompileCondition(source, context)
	if err != nil {
		return source, false
	}
	return reduced, true
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
	maps.Copy(out, in)
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

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

func jobError(path string, job workflow.Job, message string) error {
	return locatedJobError(path, job, job.Span.Start.Line, job.Span.Start.Column, message)
}

func locatedJobError(path string, job workflow.Job, line, column int, message string) error {
	return fmt.Errorf("%s:%d:%d: job %q: %s", path, line, column, job.ID, message)
}

func locatedJobWrappedError(path string, job workflow.Job, line, column int, message string, err error) error {
	if message == "" {
		return fmt.Errorf("%s:%d:%d: job %q: %w", path, line, column, job.ID, err)
	}
	return fmt.Errorf("%s:%d:%d: job %q: %s: %w", path, line, column, job.ID, message, err)
}
