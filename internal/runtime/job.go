package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// JobResult is the bounded logical result returned to the transport layer.
type JobResult struct {
	Conclusion         string                     `json:"conclusion"`
	Outputs            map[string]string          `json:"outputs,omitempty"`
	Env                map[string]string          `json:"env,omitempty"`
	State              map[string]string          `json:"state,omitempty"`
	Summary            string                     `json:"summary,omitempty"`
	WarningAnnotations string                     `json:"warning_annotations,omitempty"`
	ErrorAnnotations   string                     `json:"error_annotations,omitempty"`
	Artifacts          []transport.ResultArtifact `json:"artifacts,omitempty"`

	summaryTruncated  bool
	warningsTruncated bool
	errorsTruncated   bool
}

const maxJobOutputBytes = 1024

type registeredPost struct {
	action     JavaScriptAction
	state      map[string]string
	node       string
	condition  string
	invocation *preparedInvocation
}

type postRegistry struct {
	mu    sync.Mutex
	posts []registeredPost
}

type preparedInvocation struct {
	action         JavaScriptAction
	state          map[string]string
	node           string
	postRegistered bool
}

type remotePreparations map[string]*preparedInvocation

type remotePreparationStatus struct {
	unsuccessful bool
}

func (r *postRegistry) register(post *registeredPost) {
	if post == nil {
		return
	}
	r.mu.Lock()
	r.posts = append(r.posts, *post)
	r.mu.Unlock()
}

func (r *postRegistry) snapshot() []registeredPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]registeredPost(nil), r.posts...)
}

// VerifyWorkflow binds a plan to the workflow bytes in the supplied workspace.
func VerifyWorkflow(job plan.Job, workspace string) error {
	path, err := workspacePath(workspace, job.Workflow.Path)
	if err != nil {
		return fmt.Errorf("verify workflow binding: %w", err)
	}
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("verify workflow binding: %w", err)
	}
	digest := sha256.Sum256(source)
	got := "sha256:" + hex.EncodeToString(digest[:])
	if got != job.Workflow.Digest {
		return fmt.Errorf("workflow digest mismatch: plan binds %s, workspace has %s", job.Workflow.Digest, got)
	}
	return nil
}

// RunJob executes the plan's ordered steps and always drains registered post actions.
func (r Runner) RunJob(ctx context.Context, job plan.Job, workspace string) (final JobResult, runJobErr error) {
	callerWorkspace := workspace != ""
	if r.nodeVerification == nil {
		r.nodeVerification = &managedNodeVerification{paths: make(map[int]string, 2)}
	}
	if r.artifactRegistry == nil {
		r.artifactRegistry = &artifactRegistry{names: make(map[string]bool)}
	}
	if err := job.Validate(); err != nil {
		return JobResult{}, err
	}
	for _, capability := range job.RequiredCapabilities {
		if capability != "docker" && capability != "secrets" && capability != "network" {
			return JobResult{}, fmt.Errorf("capability %q is unsupported in the job runtime", capability)
		}
	}
	if len(job.Dependencies) != 0 && len(job.Needs) == 0 {
		return JobResult{}, fmt.Errorf("job has %d static dependencies but no hydrated prerequisite results", len(job.Dependencies))
	}
	processor := newCommandProcessor(r.stdout(), r.stderr())
	eval := expression.Context{
		Matrix:      job.Matrix,
		Steps:       make(map[string]map[string]string, len(job.Steps)),
		Needs:       needOutputs(job.Needs),
		NeedResults: needResults(job.Needs),
		Vars:        job.Vars,
		GitHub:      githubContext(job),
	}
	jobResult := JobResult{Conclusion: "failure", Outputs: map[string]string{}, Env: map[string]string{}, State: map[string]string{}, Artifacts: []transport.ResultArtifact{}}
	for _, name := range sortedKeys(job.Needs) {
		need := job.Needs[name]
		if need.Result == "" {
			return jobResult, fmt.Errorf("prerequisite result %q is missing from the job plan", name)
		}
		switch need.Result {
		case "success", "failure", "cancelled", "skipped":
		default:
			return jobResult, fmt.Errorf("prerequisite %q has invalid result %q", name, need.Result)
		}
	}
	jobCondition := expression.ConditionContext{Needs: eval.Needs, NeedResults: eval.NeedResults, Matrix: job.Matrix, Vars: job.Vars, GitHub: eval.GitHub}
	for _, need := range job.Needs {
		jobCondition.Failure = jobCondition.Failure || need.Result == "failure"
		jobCondition.Cancelled = jobCondition.Cancelled || need.Result == "cancelled"
		jobCondition.Unsuccessful = jobCondition.Unsuccessful || need.Result != "success"
	}
	run, err := expression.EvaluateCondition(job.Condition, jobCondition)
	if err != nil {
		return jobResult, fmt.Errorf("evaluate job condition: %w", err)
	}
	if !run {
		jobResult.Conclusion = "skipped"
		return jobResult, nil
	}
	runCtx := ctx
	cancelJob := func() {}
	if job.TimeoutMinutes > 0 {
		runCtx, cancelJob = context.WithTimeout(ctx, durationMinutes(job.TimeoutMinutes))
	}
	defer cancelJob()

	secrets, err := r.resolveSecrets(runCtx, processor, job.RequiredSecrets)
	if err != nil {
		return jobResult, err
	}
	eval.Secrets = secrets
	if workspace == "" {
		workspace, err = os.MkdirTemp("", "buildkite-gha-workspace-")
		if err != nil {
			return jobResult, fmt.Errorf("create workspace: %w", err)
		}
		defer func() { _ = os.RemoveAll(workspace) }()
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return jobResult, fmt.Errorf("resolve workspace: %w", err)
	}
	if job.HasCapability("docker") {
		workspaceDir, err := os.Open(workspace)
		if err != nil {
			return jobResult, err
		}
		defer func() { runJobErr = errors.Join(runJobErr, workspaceDir.Close()) }()
		workspaceInfo, err := workspaceDir.Stat()
		if err != nil {
			return jobResult, err
		}
		if err := workspaceDir.Chmod(0o777); err != nil {
			return jobResult, fmt.Errorf("make Docker workspace writable: %w", err)
		}
		if callerWorkspace {
			defer func() { runJobErr = errors.Join(runJobErr, workspaceDir.Chmod(workspaceInfo.Mode().Perm())) }()
		}
	}
	jobEnv, err := evaluateMap(job.Env, eval)
	if err != nil {
		return jobResult, fmt.Errorf("evaluate job environment: %w", err)
	}
	_, explicitJobPATH := jobEnv["PATH"]
	runnerTemp, err := os.MkdirTemp("", "buildkite-gha-runner-")
	if err != nil {
		return jobResult, fmt.Errorf("create runner temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(runnerTemp) }()
	if job.HasCapability("docker") {
		if err := os.Chmod(runnerTemp, 0o777); err != nil {
			return jobResult, fmt.Errorf("make Docker runner temp writable: %w", err)
		}
	}
	if err := os.Mkdir(filepath.Join(runnerTemp, "tool-cache"), 0o755); err != nil {
		return jobResult, fmt.Errorf("create runner tool cache: %w", err)
	}
	if job.HasCapability("docker") {
		r.runnerTemp = runnerTemp
	}
	actions := newActionLockResolver(job, workspace, r.Actions)
	var containerMounts []containerMount
	if job.Container != nil {
		// Remote children of workspace composites are already present in the
		// immutable lock graph even when the workspace action itself must remain
		// lazy. Materialize every remote lock now because bind mounts cannot be
		// added after the persistent container is created.
		for _, lock := range job.Actions {
			if lock.Source != "github" {
				continue
			}
			action, _, resolveErr := actions.resolve(runCtx, plan.ActionSelector{Lock: lock.ID})
			if resolveErr != nil {
				return jobResult, fmt.Errorf("prepare action lock %q: %w", lock.ID, resolveErr)
			}
			actionRuntime, runtimeErr := action.Runtime()
			if runtimeErr != nil {
				return jobResult, fmt.Errorf("prepare action lock %q: %w", lock.ID, runtimeErr)
			}
			if entrypointErr := action.ValidateEntrypoints(actionRuntime); entrypointErr != nil {
				return jobResult, fmt.Errorf("prepare action lock %q: %w", lock.ID, entrypointErr)
			}
		}
		for _, step := range job.Steps {
			if step.Kind != "uses" || step.Action == nil {
				continue
			}
			if source, sourceErr := actions.source(*step.Action); sourceErr != nil {
				return jobResult, sourceErr
			} else if source == "github" {
				if verifyErr := r.verifyRemoteActionTree(runCtx, actions, *step.Action, nil); verifyErr != nil {
					return jobResult, fmt.Errorf("prepare action %q: %w", step.Uses, verifyErr)
				}
			}
		}
		if len(job.Actions) != 0 {
			var mountErr error
			containerMounts, mountErr = r.actionContainerMounts(runCtx, actions)
			if mountErr != nil {
				return jobResult, mountErr
			}
		}
		backend, setupErr := r.startJobContainer(runCtx, processor, workspace, runnerTemp, *job.Container, job.Services, containerMounts...)
		if setupErr != nil {
			return jobResult, setupErr
		}
		r.jobContainer = backend
		r.jobDocker = backend
		eval.Services = backend.servicePorts
		defer func() { runJobErr = errors.Join(runJobErr, backend.cleanup()) }()
	} else if len(job.Services) != 0 {
		backend, setupErr := r.startJobContainer(runCtx, processor, workspace, runnerTemp, plan.Container{}, job.Services)
		if setupErr != nil {
			return jobResult, setupErr
		}
		r.jobDocker = backend
		eval.Services = backend.servicePorts
		defer func() { runJobErr = errors.Join(runJobErr, backend.cleanup()) }()
	}
	jobResult.Env = mergeStepEnvironment(standardEnvironment(job, workspace, runnerTemp), jobEnv)
	if r.jobContainer != nil && !explicitJobPATH {
		jobResult.Env["PATH"] = r.jobContainer.imagePATH
	}
	if r.jobContainer == nil {
		if path, ok := os.LookupEnv("PATH"); ok && jobResult.Env["PATH"] == "" {
			jobResult.Env["PATH"] = path
		}
	}
	if job.HasCapability("docker") {
		r.explicitJobPATH = explicitJobPATH
		if !explicitJobPATH {
			r.implicitJobPATH = jobResult.Env["PATH"]
		}
	}
	eval.Env = jobResult.Env
	var posts postRegistry
	statuses := make(map[string]expression.StepStatus, len(job.Steps))
	supervisor := newBackgroundSupervisor(maxActiveBackgroundSteps)

	var runErr error
	prepared := remotePreparations{}
	preStatus := remotePreparationStatus{}
	if job.Schema == plan.SchemaV3 || job.Schema == plan.SchemaV4 {
		for stepIndex, step := range job.Steps {
			if step.Kind != "uses" {
				continue
			}
			if step.Action == nil {
				runErr = errors.Join(runErr, fmt.Errorf("prepare action %q: immutable selector is missing", step.Uses))
				break
			}
			source, err := actions.source(*step.Action)
			if err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("prepare action %q: %w", step.Uses, err))
				break
			}
			if source != "github" {
				continue
			}
			if err := r.verifyRemoteActionTree(runCtx, actions, *step.Action, nil); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("prepare action %q: %w", step.Uses, err))
				break
			}
			if entry := actions.locks[step.Action.Lock]; entry != nil && usesUploadArtifactAdapter(entry.lock) {
				continue
			}
			preResult, preErr := r.prepareRemoteAction(runCtx, processor, step, strconv.Itoa(stepIndex), jobResult.Env, eval, &posts, actions, prepared, &preStatus, nil)
			commitResultEnvironment(jobResult.Env, preResult)
			mergeInto(jobResult.State, preResult.State)
			appendJobSummary(&jobResult.Summary, &jobResult.summaryTruncated, preResult.Summary, preResult.summaryTruncated)
			eval.Env = jobResult.Env
			if preErr != nil {
				preStatus.unsuccessful = true
				runErr = errors.Join(runErr, fmt.Errorf("action %q pre: %w", step.Uses, preErr))
			}
		}
	}
	for stepIndex, step := range job.Steps {
		if step.Kind == "cancel" {
			for _, execution := range supervisor.cancel(step.Targets[0]) {
				targetErr := commitStepExecution(execution, &jobResult, &eval, statuses)
				if execution.conclusion != "cancelled" {
					runErr = errors.Join(runErr, targetErr)
				}
			}
			statuses[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: "success", Conclusion: "success", Outputs: map[string]string{}}
			eval.Steps[strings.ToLower(step.ID)] = map[string]string{}
			continue
		}
		if step.Kind == "wait" || step.Kind == "wait-all" {
			var completed []stepExecution
			if step.Kind == "wait" {
				completed = supervisor.wait(step.Targets)
			} else {
				completed = supervisor.waitAll()
			}
			var barrierErr error
			for _, execution := range completed {
				barrierErr = errors.Join(barrierErr, commitStepExecution(execution, &jobResult, &eval, statuses))
			}
			outcome, conclusion := "success", "success"
			if barrierErr != nil {
				outcome = "failure"
				if ctx.Err() != nil {
					outcome = "cancelled"
				}
				conclusion = outcome
				runErr = errors.Join(runErr, fmt.Errorf("step %q: %w", step.ID, barrierErr))
			}
			statuses[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: outcome, Conclusion: conclusion, Outputs: map[string]string{}}
			eval.Steps[strings.ToLower(step.ID)] = map[string]string{}
			continue
		}

		condition := expression.ConditionContext{Needs: eval.Needs, NeedResults: eval.NeedResults, Steps: statuses, Env: jobResult.Env, Vars: job.Vars, Matrix: job.Matrix, GitHub: eval.GitHub, Services: eval.Services, Failure: runErr != nil, Unsuccessful: runErr != nil, Cancelled: runCtx.Err() != nil}
		run, err := expression.EvaluateCondition(step.Condition, condition)
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("step %q condition: %w", step.ID, err))
			break
		}
		if !run {
			statuses[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: "skipped", Conclusion: "skipped", Outputs: map[string]string{}}
			eval.Steps[strings.ToLower(step.ID)] = map[string]string{}
			continue
		}

		jobEnv := cloneStrings(jobResult.Env)
		evalSnapshot := cloneExpressionContext(eval)
		if step.Background {
			step := step
			supervisor.start(runCtx, step.ID,
				func(stepCtx context.Context) stepExecution {
					return r.executePlanStep(ctx, stepCtx, processor, workspace, job, step, strconv.Itoa(stepIndex), jobEnv, evalSnapshot, &posts, actions, prepared)
				},
				func(stepCtx context.Context) stepExecution {
					return cancelledStepExecution(ctx, stepCtx, step)
				},
			)
			continue
		}
		execution := r.executePlanStep(ctx, runCtx, processor, workspace, job, step, strconv.Itoa(stepIndex), jobEnv, evalSnapshot, &posts, actions, prepared)
		runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
	}
	for _, execution := range supervisor.waitAll() {
		runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
	}
	if runCtx.Err() != nil {
		runErr = errors.Join(runErr, runCtx.Err())
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), r.cleanupTimeout())
	defer cancel()
	registeredPosts := posts.snapshot()
	for i := len(registeredPosts) - 1; i >= 0; i-- {
		post := registeredPosts[i]
		if post.invocation != nil {
			post.action = post.invocation.action
			post.state = post.invocation.state
			post.node = post.invocation.node
		}
		runPost, conditionErr := evaluateLifecycleCondition(post.condition, runErr != nil, ctx.Err() != nil || runCtx.Err() != nil)
		if conditionErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("post action %q condition: %w", post.action.Name, conditionErr))
			continue
		}
		if !runPost {
			continue
		}
		postResult := newResult()
		postResult.Env = cloneStrings(jobResult.Env)
		postErr := r.runJavaScriptPhase(cleanupCtx, processor, post.node, post.action, post.action.Post, post.state, post.state, &postResult)
		mergeInto(jobResult.Env, postResult.Env)
		mergeInto(jobResult.State, postResult.State)
		appendJobSummary(&jobResult.Summary, &jobResult.summaryTruncated, postResult.Summary, postResult.summaryTruncated)
		if postErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("post action %q: %w", post.action.Name, postErr))
		}
	}

	jobResult.WarningAnnotations, jobResult.warningsTruncated, jobResult.ErrorAnnotations, jobResult.errorsTruncated = processor.workflowCommandAnnotations()
	sensitiveValues := processor.maskValues()
	for _, artifact := range jobResult.Artifacts {
		for _, sensitive := range sensitiveValues {
			if sensitive != "" && strings.Contains(artifact.Name, sensitive) {
				jobResult.Artifacts = nil
				return scrubJobResult(jobResult, sensitiveValues), errors.Join(runErr, fmt.Errorf("artifact name contains a registered secret"))
			}
		}
	}
	for _, name := range sortedKeys(job.Outputs) {
		template := job.Outputs[name]
		value, err := expression.Evaluate(template, eval)
		if err != nil {
			return scrubJobResult(jobResult, sensitiveValues), errors.Join(runErr, fmt.Errorf("job output %q: %w", name, err))
		}
		if len(value) > maxJobOutputBytes {
			return scrubJobResult(jobResult, sensitiveValues), errors.Join(runErr, fmt.Errorf("job output %q exceeds the %d-byte limit", name, maxJobOutputBytes))
		}
		for _, sensitive := range sensitiveValues {
			if sensitive != "" && strings.Contains(value, sensitive) {
				return scrubJobResult(jobResult, sensitiveValues), errors.Join(runErr, fmt.Errorf("job output %q contains a registered secret", name))
			}
		}
		jobResult.Outputs[name] = value
	}
	if ctx.Err() != nil {
		jobResult.Conclusion = "cancelled"
	} else if runErr == nil {
		jobResult.Conclusion = "success"
	}
	return scrubJobResult(jobResult, sensitiveValues), runErr
}

func scrubJobResult(result JobResult, sensitiveValues []string) JobResult {
	sort.Slice(sensitiveValues, func(i, j int) bool {
		if len(sensitiveValues[i]) != len(sensitiveValues[j]) {
			return len(sensitiveValues[i]) > len(sensitiveValues[j])
		}
		return sensitiveValues[i] < sensitiveValues[j]
	})
	scrub := func(value string) string {
		for _, sensitive := range sensitiveValues {
			if sensitive != "" {
				value = strings.ReplaceAll(value, sensitive, "***")
			}
		}
		return value
	}
	for name, value := range result.Outputs {
		result.Outputs[name] = scrub(value)
	}
	for name, value := range result.Env {
		result.Env[name] = scrub(value)
	}
	for name, value := range result.State {
		result.State[name] = scrub(value)
	}
	summary, summaryTruncated := result.Summary, result.summaryTruncated
	for _, sensitive := range sensitiveValues {
		if sensitive != "" {
			summary = strings.ReplaceAll(summary, sensitive, "***")
			summary, summaryTruncated = boundJobSummary(summary, summaryTruncated)
		}
	}
	result.Summary, result.summaryTruncated = boundJobSummary(summary, summaryTruncated)
	if result.summaryTruncated {
		result.Summary = trimSensitiveSuffix(result.Summary, sensitiveValues)
	}
	result.Summary = finalizeJobSummary(result.Summary, result.summaryTruncated)
	result.WarningAnnotations, result.warningsTruncated = scrubWorkflowCommandAnnotations(result.WarningAnnotations, result.warningsTruncated, sensitiveValues)
	result.ErrorAnnotations, result.errorsTruncated = scrubWorkflowCommandAnnotations(result.ErrorAnnotations, result.errorsTruncated, sensitiveValues)
	return result
}

func scrubWorkflowCommandAnnotations(value string, truncated bool, sensitiveValues []string) (string, bool) {
	annotationSensitiveValues := make([]string, 0, len(sensitiveValues))
	for _, sensitive := range sensitiveValues {
		if sensitive == "" {
			continue
		}
		// User-controlled annotation content is rendered through commandHTML.
		// Scrubbing that same valid UTF-8 representation prevents invalid raw
		// mask bytes from splitting a rendered multibyte rune.
		annotationSensitiveValues = append(annotationSensitiveValues, commandHTML(sensitive))
	}
	sort.Slice(annotationSensitiveValues, func(i, j int) bool {
		if len(annotationSensitiveValues[i]) != len(annotationSensitiveValues[j]) {
			return len(annotationSensitiveValues[i]) > len(annotationSensitiveValues[j])
		}
		return annotationSensitiveValues[i] < annotationSensitiveValues[j]
	})
	for _, sensitive := range annotationSensitiveValues {
		value = strings.ReplaceAll(value, sensitive, "***")
		var bounded string
		var boundedTruncated bool
		appendBoundedText(&bounded, &boundedTruncated, value, truncated, maxJobAnnotationBytes, workflowCommandTruncationNotice)
		value, truncated = bounded, boundedTruncated
	}
	var bounded string
	var boundedTruncated bool
	appendBoundedText(&bounded, &boundedTruncated, value, truncated, maxJobAnnotationBytes, workflowCommandTruncationNotice)
	if boundedTruncated {
		bounded = trimSensitiveSuffix(bounded, annotationSensitiveValues) + workflowCommandTruncationNotice
	}
	return bounded, boundedTruncated
}

func trimSensitiveSuffix(value string, sensitiveValues []string) string {
	for {
		trimmed := false
		for _, sensitive := range sensitiveValues {
			for prefixBytes := min(len(sensitive)-1, len(value)); prefixBytes > 0; prefixBytes-- {
				if strings.HasSuffix(value, sensitive[:prefixBytes]) {
					value = value[:len(value)-prefixBytes]
					trimmed = true
					break
				}
			}
			if trimmed {
				break
			}
		}
		if !trimmed {
			return value
		}
	}
}

func (r Runner) resolveSecrets(ctx context.Context, processor *commandProcessor, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if r.Secrets == nil || r.Redactor == nil {
		return nil, fmt.Errorf("job requires secrets but a secret resolver and redactor are not configured")
	}
	values := make(map[string]string, len(names))
	for _, name := range names {
		value, err := r.Secrets.ResolveSecret(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve secret %q: %w", name, err)
		}
		if value != "" {
			if err := r.Redactor.AddRedaction(ctx, value); err != nil {
				return nil, err
			}
			processor.addMask(value)
		}
		values[name] = value
	}
	return values, nil
}

func durationMinutes(minutes float64) time.Duration {
	return time.Duration(minutes * float64(time.Minute))
}

func applyPaths(env map[string]string, paths []string) {
	for _, path := range paths {
		if env["PATH"] == "" {
			env["PATH"] = path
		} else {
			env["PATH"] = path + string(os.PathListSeparator) + env["PATH"]
		}
	}
}

func githubContext(job plan.Job) map[string]any {
	return map[string]any{
		"repository": job.Event.Repository,
		"ref":        job.Event.Ref,
		"sha":        job.Event.SHA,
		"actor":      job.Event.Actor,
		"event_name": job.Event.Name,
	}
}

func standardEnvironment(job plan.Job, workspace, runnerTemp string) map[string]string {
	return map[string]string{
		"CI":                "true",
		"GITHUB_ACTIONS":    "true",
		"GITHUB_ACTOR":      job.Event.Actor,
		"GITHUB_EVENT_NAME": job.Event.Name,
		"GITHUB_JOB":        job.Workflow.LogicalJobID,
		"GITHUB_REF":        job.Event.Ref,
		"GITHUB_REPOSITORY": job.Event.Repository,
		"GITHUB_SHA":        job.Event.SHA,
		"GITHUB_WORKSPACE":  workspace,
		"RUNNER_OS":         "Linux",
		"RUNNER_TEMP":       runnerTemp,
		"RUNNER_TOOL_CACHE": filepath.Join(runnerTemp, "tool-cache"),
	}
}

func mergeStepEnvironment(base map[string]string, overlays ...map[string]string) map[string]string {
	out := mergeStringMaps(append([]map[string]string{base}, overlays...)...)
	for name, value := range base {
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "RUNNER_") {
			out[name] = value
		}
	}
	return out
}

func (r Runner) runJobStep(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations) (Result, error) {
	return r.runActionStep(ctx, processor, workspace, job, step, invocationID, jobEnv, eval, posts, actions, prepared, nil)
}

// verifyRemoteActionTree materializes and verifies every remote subtree before
// any of its pre hooks run. Workspace subtrees remain lazy and keep their
// existing main-traversal compatibility behavior.
func (r Runner) verifyRemoteActionTree(ctx context.Context, actions *actionLockResolver, selector plan.ActionSelector, stack []string) error {
	source, err := actions.source(selector)
	if err != nil {
		return err
	}
	if source != "github" {
		return nil
	}
	action, lock, err := actions.resolve(ctx, selector)
	if err != nil {
		return err
	}
	for _, ancestor := range stack {
		if ancestor == lock.ID {
			return fmt.Errorf("action recursion detected at lock %q", lock.ID)
		}
	}
	runtime, err := action.Runtime()
	if err != nil {
		return err
	}
	if err := action.ValidateEntrypoints(runtime); err != nil {
		return err
	}
	if runtime != metadata.RuntimeComposite {
		return nil
	}
	stack = append(append([]string(nil), stack...), lock.ID)
	for i, child := range action.Runs.Steps {
		if child.Uses == "" {
			continue
		}
		childSelector, ok := lock.Children[child.Uses]
		if !ok || childSelector.Lock == "" {
			return fmt.Errorf("composite action step %d child %q has no immutable selector", i+1, child.Uses)
		}
		if err := r.verifyRemoteActionTree(ctx, actions, childSelector, stack); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) actionContainerMounts(ctx context.Context, actions *actionLockResolver) ([]containerMount, error) {
	byTarget := map[string]containerMount{}
	requiredNode := map[int]bool{}
	unknownWorkspaceRuntime := false
	for _, lock := range actions.job.Actions {
		entry := actions.locks[lock.ID]
		entry.mu.Lock()
		material := entry.material
		entry.mu.Unlock()

		var action metadata.Metadata
		switch lock.Source {
		case "github":
			var err error
			action, _, err = actions.resolve(ctx, plan.ActionSelector{Lock: lock.ID})
			if err != nil {
				return nil, err
			}
		case "workspace":
			loaded, err := metadata.Load(actions.workspace, lock.Path)
			if err != nil {
				unknownWorkspaceRuntime = true
				continue
			}
			digest, err := actionsource.DigestTree(loaded.Path)
			if err != nil || digest != lock.SourceDigest {
				unknownWorkspaceRuntime = true
				continue
			}
			action = loaded
		default:
			return nil, fmt.Errorf("unsupported action lock source %q", lock.Source)
		}
		actionRuntime, err := action.Runtime()
		if err != nil {
			return nil, err
		}
		if material != nil && actionRuntime != metadata.RuntimeDocker {
			target := remoteMountTarget(entry.lock.Repository, entry.lock.Commit)
			m := containerMount{host: material.RepositoryRoot, target: target, readonly: true, probe: true}
			if old, ok := byTarget[target]; ok && old.host != m.host {
				return nil, fmt.Errorf("conflicting verified action roots for %q", target)
			}
			byTarget[target] = m
		}
		switch actionRuntime {
		case metadata.RuntimeNode20:
			requiredNode[20] = true
		case metadata.RuntimeNode24:
			requiredNode[24] = true
		}
	}
	if unknownWorkspaceRuntime {
		requiredNode[20], requiredNode[24] = true, true
	}
	for _, major := range []int{20, 24} {
		if !requiredNode[major] {
			continue
		}
		explicit := r.Node24
		if major == 20 {
			explicit = r.Node20
		}
		if explicit != "" {
			abs, err := filepath.Abs(explicit)
			if err != nil {
				return nil, err
			}
			info, err := os.Stat(abs)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				continue
			}
			byTarget[fmt.Sprintf("/__buildkite-gha/node%d", major)] = containerMount{host: abs, target: fmt.Sprintf("/__buildkite-gha/node%d", major), readonly: true}
			continue
		}
		if r.ManagedNodeRoot != "" {
			abs, err := filepath.Abs(r.ManagedNodeRoot)
			if err != nil {
				return nil, err
			}
			if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
				byTarget["/__buildkite-gha/nodes"] = containerMount{host: abs, target: "/__buildkite-gha/nodes", readonly: true}
			}
			continue
		}
		if explicit == "" {
			var err error
			explicit, err = r.resolveMiseNodePath(ctx, major)
			if err != nil {
				return nil, err
			}
			if major == 20 {
				r.Node20 = explicit
			} else {
				r.Node24 = explicit
			}
		}
		byTarget[fmt.Sprintf("/__buildkite-gha/node%d", major)] = containerMount{host: explicit, target: fmt.Sprintf("/__buildkite-gha/node%d", major), readonly: true}
	}
	keys := sortedKeys(byTarget)
	out := make([]containerMount, 0, len(keys))
	for _, key := range keys {
		out = append(out, byTarget[key])
	}
	return out, nil
}

func (r Runner) prepareRemoteAction(ctx context.Context, processor *commandProcessor, step plan.Step, invocationID string, jobEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, status *remotePreparationStatus, inheritedEvalErr error) (Result, error) {
	result := newResult()
	source, err := actions.source(*step.Action)
	if err != nil {
		return result, err
	}
	if source != "github" {
		return result, nil
	}
	action, lock, err := actions.resolve(ctx, *step.Action)
	if err != nil {
		return result, err
	}
	runtime, err := action.Runtime()
	if err != nil {
		return result, fmt.Errorf("action %q uses %w", step.Uses, err)
	}
	if err := action.ValidateEntrypoints(runtime); err != nil {
		return result, fmt.Errorf("action %q: %w", step.Uses, err)
	}

	switch runtime {
	case metadata.RuntimeNode20, metadata.RuntimeNode24:
		// The checkout adapter replaces the verified action's JavaScript
		// lifecycle as one indivisible operation. Do not register upstream
		// checkout cleanup for a main phase that this runtime never executes.
		if usesCheckoutAdapter(lock) || usesUploadArtifactAdapter(lock) {
			return result, nil
		}
		runPre, err := evaluateLifecycleCondition(action.Runs.PreIf, status.unsuccessful, ctx.Err() != nil)
		if err != nil {
			return result, fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err)
		}
		if _, err := evaluateLifecycleCondition(action.Runs.PostIf, false, false); err != nil {
			return result, fmt.Errorf("JavaScript action %q post-if: %w", step.Uses, err)
		}
		if action.Runs.Pre == "" && action.Runs.Post == "" {
			return result, nil
		}
		major, explicit := 24, r.Node24
		if runtime == metadata.RuntimeNode20 {
			major, explicit = 20, r.Node20
		}
		javascript := JavaScriptAction{Name: actionName(action, step), Path: action.Path, Pre: action.Runs.Pre, Main: action.Runs.Main, Post: action.Runs.Post, nodeMajor: major}
		invocation := &preparedInvocation{action: javascript, state: map[string]string{}}
		prepared[invocationID] = invocation
		if javascript.Pre != "" && runPre {
			if inheritedEvalErr != nil {
				return result, inheritedEvalErr
			}
			inputs, err := evaluateMap(step.With, eval)
			if err != nil {
				return result, err
			}
			inputs, err = resolveActionInputs(action, inputs, eval)
			if err != nil {
				return result, err
			}
			stepEnv, err := evaluateMap(step.Env, eval)
			if err != nil {
				return result, err
			}
			javascript.Inputs = inputs
			javascript.Env = mergeStepEnvironment(jobEnv, stepEnv)
			invocation.action = javascript
			node, err := r.discoverNode(ctx, major, explicit)
			if err != nil {
				return result, err
			}
			invocation.node = node
			posts.register(postForInvocation(invocation, action.Runs.PostIf))
			invocation.postRegistered = true
			if err := r.runJavaScriptPhase(ctx, processor, node, javascript, javascript.Pre, nil, invocation.state, &result); err != nil {
				return result, err
			}
		}
		return result, nil
	case metadata.RuntimeComposite:
		inputs := map[string]string{}
		stepEnv := map[string]string{}
		compositeEvalErr := inheritedEvalErr
		if compositeEvalErr == nil {
			inputs, compositeEvalErr = evaluateMap(step.With, eval)
		}
		if compositeEvalErr == nil {
			inputs, compositeEvalErr = resolveActionInputs(action, inputs, eval)
		}
		if compositeEvalErr == nil {
			stepEnv, compositeEvalErr = evaluateMap(step.Env, eval)
		}
		eval.Inputs = inputs
		compositeEnv := mergeStepEnvironment(jobEnv, stepEnv)
		for i, childStep := range action.Runs.Steps {
			if childStep.Uses == "" {
				continue
			}
			selector, ok := lock.Children[childStep.Uses]
			if !ok || selector.Lock == "" {
				return result, fmt.Errorf("composite action step %d child %q has no immutable selector", i+1, childStep.Uses)
			}
			child := plan.Step{ID: childStep.ID, Name: childStep.Name, Kind: "uses", Uses: childStep.Uses, With: childStep.With, Env: childStep.Env, Action: &plan.ActionSelector{Lock: selector.Lock}}
			childEnv := mergeStepEnvironment(compositeEnv, result.Env)
			eval.Env = childEnv
			childResult, childErr := r.prepareRemoteAction(ctx, processor, child, fmt.Sprintf("%s/%d", invocationID, i), childEnv, eval, posts, actions, prepared, status, compositeEvalErr)
			mergeInto(result.Env, childResult.Env)
			if childResult.pathBaseSet {
				result.pathBase = childResult.pathBase
				result.pathBaseSet = true
				result.Paths = result.Paths[:0]
			}
			result.Paths = append(result.Paths, childResult.Paths...)
			mergeInto(result.State, childResult.State)
			appendJobSummary(&result.Summary, &result.summaryTruncated, childResult.Summary, childResult.summaryTruncated)
			if childErr != nil {
				status.unsuccessful = true
				err = errors.Join(err, fmt.Errorf("composite action step %d: %w", i+1, childErr))
			}
		}
		return result, err
	default:
		return result, nil
	}
}

func (r Runner) runActionStep(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, actionStack []string) (Result, error) {
	stepEnv, err := evaluateMap(step.Env, eval)
	if err != nil {
		return newResult(), err
	}
	result := newResult()
	if step.Kind == "run" {
		script, err := expression.Evaluate(step.Command, eval)
		if err != nil {
			return result, err
		}
		shell := step.Shell
		if shell == "" {
			shell = job.DefaultShell
		}
		shell, err = expression.Evaluate(shell, eval)
		if err != nil {
			return result, err
		}
		if shell == "" {
			if r.jobContainer != nil {
				shell = "sh"
			} else {
				shell = "bash"
			}
		}
		workingDirectory := step.WorkingDirectory
		if workingDirectory == "" {
			workingDirectory = job.DefaultWorkingDirectory
		}
		workingDirectory, err = expression.Evaluate(workingDirectory, eval)
		if err != nil {
			return result, err
		}
		dir, err := workspacePath(workspace, workingDirectory)
		if err != nil {
			return result, err
		}
		args, err := shellCommand(shell, script)
		if err != nil {
			return result, err
		}
		runEnv := mergeStepEnvironment(jobEnv, stepEnv)
		err = r.runProcess(ctx, processor, dir, runEnv, &result, nil, args[0], args[1:]...)
		return result, err
	}

	var action metadata.Metadata
	var actionLock *plan.ActionLock
	if job.Schema == plan.SchemaV3 || job.Schema == plan.SchemaV4 {
		if step.Action == nil {
			return result, fmt.Errorf("action %q has no immutable selector", step.Uses)
		}
		resolvedAction, lock, err := actions.resolve(ctx, *step.Action)
		if err != nil {
			return result, err
		}
		action, actionLock = resolvedAction, &lock
		if usesCheckoutAdapter(lock) {
			inputs, err := evaluateMap(step.With, eval)
			if err != nil {
				return result, err
			}
			return r.runCheckout(ctx, processor, workspace, job, inputs)
		}
		if usesUploadArtifactAdapter(lock) {
			inputs, err := evaluateMap(step.With, eval)
			if err != nil {
				return result, err
			}
			return r.runUploadArtifact(ctx, workspace, inputs, processor.maskValues())
		}
	} else {
		if !strings.HasPrefix(step.Uses, "./") {
			return result, fmt.Errorf("remote action %q is unsupported in the Phase 0 runtime", step.Uses)
		}
		if err := VerifyWorkflow(job, workspace); err != nil {
			return result, err
		}
		var err error
		action, err = metadata.Load(workspace, step.Uses)
		if err != nil {
			return result, err
		}
	}
	actionRuntime, err := action.Runtime()
	if err != nil {
		return result, fmt.Errorf("action %q uses %w", step.Uses, err)
	}
	if err := action.ValidateEntrypoints(actionRuntime); err != nil {
		return result, fmt.Errorf("action %q: %w", step.Uses, err)
	}
	actionPath := action.Path
	actionIdentity := actionPath
	if actionLock != nil {
		actionIdentity = actionLock.ID
	}
	for _, ancestor := range actionStack {
		if ancestor == actionIdentity {
			return result, fmt.Errorf("action recursion detected at %q", step.Uses)
		}
	}
	if len(actionStack) >= metadata.MaxNestedActionDepth {
		return result, fmt.Errorf("local action nesting exceeds maximum depth %d at %q", metadata.MaxNestedActionDepth, actionPath)
	}
	actionStack = append(append([]string(nil), actionStack...), actionIdentity)
	inputs, err := evaluateMap(step.With, eval)
	if err != nil {
		return result, err
	}
	inputs, err = resolveActionInputs(action, inputs, eval)
	if err != nil {
		return result, err
	}
	actionEval := eval
	actionEval.Inputs = inputs
	switch actionRuntime {
	case metadata.RuntimeNode20, metadata.RuntimeNode24:
		if action.Runs.Main == "" {
			return result, fmt.Errorf("JavaScript action %q has no main entry point", step.Uses)
		}
		if _, err := evaluateLifecycleCondition(action.Runs.PreIf, false, false); err != nil {
			return result, fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err)
		}
		if _, err := evaluateLifecycleCondition(action.Runs.PostIf, false, false); err != nil {
			return result, fmt.Errorf("JavaScript action %q post-if: %w", step.Uses, err)
		}
		major, explicit := 24, r.Node24
		if actionRuntime == metadata.RuntimeNode20 {
			major, explicit = 20, r.Node20
		}
		node, err := r.discoverNode(ctx, major, explicit)
		if err != nil {
			return result, err
		}
		actionEnv := mergeStepEnvironment(jobEnv, stepEnv)
		javascript := JavaScriptAction{Name: actionName(action, step), Path: actionPath, Pre: action.Runs.Pre, Main: action.Runs.Main, Post: action.Runs.Post, Inputs: inputs, Env: actionEnv, nodeMajor: major}
		state := map[string]string{}
		wasPrepared := false
		if invocation := prepared[invocationID]; invocation != nil {
			javascript, state, wasPrepared = invocation.action, invocation.state, true
			if invocation.node != "" {
				node = invocation.node
			} else {
				invocation.node = node
			}
			// Preserve invocation state while evaluating inputs and environment
			// again with every main-visible effect committed.
			javascript.Inputs = inputs
			javascript.Env = mergeStepEnvironment(jobEnv, stepEnv)
			invocation.action = javascript
		}
		if !wasPrepared {
			posts.register(postFor(javascript, state, node, action.Runs.PostIf))
		} else if invocation := prepared[invocationID]; !invocation.postRegistered {
			posts.register(postForInvocation(invocation, action.Runs.PostIf))
			invocation.postRegistered = true
		}
		if javascript.Pre != "" && !wasPrepared {
			runPre, _ := evaluateLifecycleCondition(action.Runs.PreIf, false, false)
			if runPre {
				if err := r.runJavaScriptPhase(ctx, processor, node, javascript, javascript.Pre, nil, state, &result); err != nil {
					return result, err
				}
			}
		}
		if err := r.runJavaScriptPhase(ctx, processor, node, javascript, javascript.Main, nil, state, &result); err != nil {
			return result, err
		}
		return result, nil
	case metadata.RuntimeComposite:
		composite, err := r.runCompositeMetadata(ctx, processor, workspace, job, actionPath, action, inputs, invocationID, jobEnv, stepEnv, actionEval, posts, actions, prepared, actionLock, actionStack)
		return composite, err
	case metadata.RuntimeDocker:
		if !job.HasCapability("docker") {
			return result, fmt.Errorf("docker action %q requires the plan's docker capability", step.Uses)
		}
		if action.Runs.PreEntrypoint != "" || action.Runs.PostEntrypoint != "" || action.Runs.Entrypoint != "" || len(action.Runs.Args) != 0 {
			return result, fmt.Errorf("docker action %q uses unsupported entrypoint, arguments, or pre/post lifecycle", step.Uses)
		}
		if action.Runs.Image != "Dockerfile" {
			return result, fmt.Errorf("docker action image %q is unsupported; Phase 0 requires a local Dockerfile", action.Runs.Image)
		}
		dockerEnv, err := evaluateMap(action.Runs.Env, actionEval)
		if err != nil {
			return result, err
		}
		sourceRoot, sourceDigest := action.Path, ""
		if action.SourceRoot != "" {
			sourceRoot = action.SourceRoot
		}
		if actionLock != nil {
			sourceDigest = actionLock.SourceDigest
		}
		_, stepPATH := stepEnv["PATH"]
		_, actionPATH := dockerEnv["PATH"]
		jobPATH := r.explicitJobPATH || jobEnv["PATH"] != r.implicitJobPATH
		invocationEnv := mergeStepEnvironment(jobEnv, stepEnv, actionInputEnv(inputs))
		for name, value := range dockerEnv {
			_, exists := invocationEnv[name]
			if !exists || (name == "PATH" && !jobPATH && !stepPATH) {
				invocationEnv[name] = value
			}
		}
		result, err := r.runDocker(ctx, processor, DockerAction{Name: actionName(action, step), Path: actionPath, SourceRoot: sourceRoot, SourceDigest: sourceDigest, Workspace: workspace, Env: invocationEnv, explicitPATH: jobPATH || stepPATH || actionPATH})
		return result, err
	}
	return result, fmt.Errorf("action %q uses unsupported runtime %q", step.Uses, actionRuntime)
}

func (r Runner) runCompositeMetadata(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, actionPath string, action metadata.Metadata, inputs map[string]string, invocationID string, jobEnv, stepEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, actionLock *plan.ActionLock, actionStack []string) (Result, error) {
	result := newResult()
	eval.Inputs = inputs
	eval.Steps = make(map[string]map[string]string)
	statuses := make(map[string]expression.StepStatus)
	compositeEnv := mergeStepEnvironment(jobEnv, stepEnv)
	compositeEnv["GITHUB_ACTION_PATH"] = actionPath
	var runErr error
	for i, step := range action.Runs.Steps {
		// GITHUB_ENV effects are visible to subsequent children. Keep this
		// composite's invocation environment in expression contexts, while
		// rebuilding the map so a child's declared env cannot leak to siblings.
		eval.Env = mergeStringMaps(compositeEnv, result.Env)
		id := strings.ToLower(step.ID)
		condition := expression.ConditionContext{Inputs: eval.Inputs, Needs: eval.Needs, NeedResults: eval.NeedResults, Steps: statuses, Env: eval.Env, Vars: eval.Vars, Matrix: eval.Matrix, GitHub: eval.GitHub, Services: eval.Services, Failure: runErr != nil, Unsuccessful: runErr != nil, Cancelled: ctx.Err() != nil}
		run, err := expression.EvaluateCondition(step.If, condition)
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("composite action step %d condition: %w", i+1, err))
			continue
		}
		if !run {
			if id != "" {
				eval.Steps[id] = map[string]string{}
				statuses[id] = expression.StepStatus{Outcome: "skipped", Conclusion: "skipped", Outputs: map[string]string{}}
			}
			continue
		}
		stepResult := newResult()
		childErr := error(nil)
		childJobEnv := mergeStepEnvironment(compositeEnv, result.Env)
		childJobEnv["GITHUB_ACTION_PATH"] = actionPath
		if step.Uses != "" {
			// runActionStep owns template evaluation for an action invocation.
			// Passing env (the evaluated map) here would evaluate expressions twice.
			child := plan.Step{ID: step.ID, Name: step.Name, Kind: "uses", Uses: step.Uses, With: step.With, Env: step.Env}
			if actionLock != nil {
				selector, ok := actionLock.Children[step.Uses]
				if !ok {
					childErr = fmt.Errorf("composite action child %q has no immutable selector", step.Uses)
				} else {
					child.Action = &plan.ActionSelector{Lock: selector.Lock}
				}
			}
			if childErr == nil {
				stepResult, childErr = r.runActionStep(ctx, processor, workspace, job, child, fmt.Sprintf("%s/%d", invocationID, i), childJobEnv, eval, posts, actions, prepared, actionStack)
			}
		} else if strings.TrimSpace(step.Run) == "" {
			childErr = fmt.Errorf("composite action step %d has no run command", i+1)
		} else {
			var script, dir string
			var args []string
			var env map[string]string
			env, childErr = evaluateMap(step.Env, eval)
			if childErr == nil {
				script, childErr = expression.Evaluate(step.Run, eval)
			}
			if childErr == nil {
				dir, childErr = workspacePath(workspace, step.WorkingDirectory)
			}
			if childErr == nil {
				args, childErr = shellCommand(step.Shell, script)
			}
			if childErr == nil {
				childErr = r.runProcess(ctx, processor, dir, mergeStepEnvironment(childJobEnv, env), &stepResult, nil, args[0], args[1:]...)
			}
		}
		mergeInto(result.Env, stepResult.Env)
		if stepResult.pathBaseSet {
			result.pathBase = stepResult.pathBase
			result.pathBaseSet = true
			result.Paths = result.Paths[:0]
		}
		result.Paths = append(result.Paths, stepResult.Paths...)
		result.Artifacts = append(result.Artifacts, stepResult.Artifacts...)
		mergeInto(result.State, stepResult.State)
		appendJobSummary(&result.Summary, &result.summaryTruncated, stepResult.Summary, stepResult.summaryTruncated)
		if id != "" {
			eval.Steps[id] = stepResult.Outputs
			outcome := "success"
			if childErr != nil {
				outcome = "failure"
			}
			statuses[id] = expression.StepStatus{Outcome: outcome, Conclusion: outcome, Outputs: stepResult.Outputs}
		}
		if childErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("composite action step %d: %w", i+1, childErr))
		}
	}
	for _, name := range sortedKeys(action.Outputs) {
		output := action.Outputs[name]
		value, err := expression.Evaluate(output.Value, eval)
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("composite output %q: %w", name, err))
			continue
		}
		result.Outputs[name] = value
	}
	return result, runErr
}

func evaluateMap(values map[string]string, context expression.Context) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, name := range sortedKeys(values) {
		value := values[name]
		resolved, err := expression.Evaluate(value, context)
		if err != nil {
			return nil, fmt.Errorf("evaluate %q: %w", name, err)
		}
		out[name] = resolved
	}
	return out, nil
}

func resolveActionInputs(action metadata.Metadata, supplied map[string]string, context expression.Context) (map[string]string, error) {
	inputs := make(map[string]string, len(supplied))
	for _, name := range sortedKeys(supplied) {
		lower := strings.ToLower(name)
		if _, exists := inputs[lower]; exists {
			return nil, fmt.Errorf("action inputs contain duplicate case-insensitive name %q", lower)
		}
		inputs[lower] = supplied[name]
	}
	names := make([]string, 0, len(action.Inputs))
	for name := range action.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		definition := action.Inputs[name]
		if _, ok := inputs[name]; ok {
			continue
		}
		if definition.Default != nil {
			context.Inputs = inputs
			value, err := expression.Evaluate(*definition.Default, context)
			if err != nil {
				return nil, fmt.Errorf("action input %q default: %w", name, err)
			}
			inputs[name] = value
			continue
		}
		if definition.Required {
			return nil, fmt.Errorf("required action input %q is missing", name)
		}
	}
	return inputs, nil
}

func needOutputs(needs map[string]plan.Need) map[string]map[string]string {
	outputs := make(map[string]map[string]string, len(needs))
	for name, need := range needs {
		outputs[name] = need.Outputs
	}
	return outputs
}

func needResults(needs map[string]plan.Need) map[string]string {
	results := make(map[string]string, len(needs))
	for name, need := range needs {
		results[name] = need.Result
	}
	return results
}

func shellCommand(shell, script string) ([]string, error) {
	switch strings.TrimSpace(shell) {
	case "", "bash":
		return []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script}, nil
	case "sh":
		return []string{"sh", "-e", "-c", script}, nil
	default:
		return nil, fmt.Errorf("shell %q is unsupported in the Phase 0 runtime", shell)
	}
}

func workspacePath(root, path string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if evaluated, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
		root = evaluated
	}
	resolved := path
	if resolved == "" {
		resolved = root
	} else if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(resolved, "./")))
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = evaluated
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace %q", path, root)
	}
	return resolved, nil
}

func postFor(action JavaScriptAction, state map[string]string, node, condition string) *registeredPost {
	if action.Post == "" {
		return nil
	}
	return &registeredPost{action: action, state: state, node: node, condition: condition}
}

func postForInvocation(invocation *preparedInvocation, condition string) *registeredPost {
	if invocation == nil || invocation.action.Post == "" {
		return nil
	}
	return &registeredPost{condition: condition, invocation: invocation}
}

func evaluateLifecycleCondition(value string, unsuccessful, cancelled bool) (bool, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "${{") && strings.HasSuffix(value, "}}") {
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "${{"), "}}"))
	}
	switch strings.ToLower(value) {
	case "", "always()":
		return true, nil
	case "success()":
		return !unsuccessful && !cancelled, nil
	case "failure()":
		return unsuccessful && !cancelled, nil
	case "cancelled()":
		return cancelled, nil
	default:
		return false, fmt.Errorf("condition %q is unsupported", value)
	}
}

func actionName(action metadata.Metadata, step plan.Step) string {
	if step.Name != "" {
		return step.Name
	}
	if action.Name != "" {
		return action.Name
	}
	return step.ID
}

func (r Runner) cleanupTimeout() time.Duration {
	if r.CleanupTimeout > 0 {
		return r.CleanupTimeout
	}
	return defaultCleanupTimeout
}

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	mergeInto(out, in)
	return out
}

func mergeInto(target map[string]string, source map[string]string) {
	for key, value := range source {
		target[key] = value
	}
}

func mergeStringMaps(values ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		mergeInto(out, value)
	}
	return out
}
