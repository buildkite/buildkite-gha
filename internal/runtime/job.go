package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
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

type toleratedJobFailure struct {
	err error
}

func (e *toleratedJobFailure) Error() string { return e.err.Error() }
func (e *toleratedJobFailure) Unwrap() error { return e.err }

type hardJobFailure struct {
	err error
}

func (e *hardJobFailure) Error() string { return e.err.Error() }
func (e *hardJobFailure) Unwrap() error { return e.err }

type workflowJobFailure struct {
	err error
}

func (e *workflowJobFailure) Error() string { return e.err.Error() }
func (e *workflowJobFailure) Unwrap() error { return e.err }

func markHardJobFailure(err error) error {
	if err == nil || isHardJobFailure(err) {
		return err
	}
	return &hardJobFailure{err: err}
}

func isHardJobFailure(err error) bool {
	var target *hardJobFailure
	return errors.As(err, &target)
}

func markWorkflowJobFailure(err error) error {
	if err == nil {
		return nil
	}
	return &workflowJobFailure{err: err}
}

func isWorkflowJobFailure(err error) bool {
	var target *workflowJobFailure
	return errors.As(err, &target)
}

// IsToleratedJobFailure reports whether err contains only a workflow failure
// admitted by the job's continue-on-error setting. Joined cleanup, integrity,
// transport, and publication errors deliberately return false.
func IsToleratedJobFailure(err error) bool {
	_, ok := err.(*toleratedJobFailure)
	return ok
}

func tolerateJobSetupFailure(runCtx context.Context, job plan.Job, result JobResult, err error) (JobResult, error) {
	if runCtx.Err() != nil {
		result.Conclusion = "cancelled"
		return result, errors.Join(err, runCtx.Err())
	}
	if job.ContinueOnError && isWorkflowJobFailure(err) && !isHardJobFailure(err) {
		result.Conclusion = "success"
		return result, &toleratedJobFailure{err: err}
	}
	return result, err
}

type registeredPost struct {
	action     javaScriptAction
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
	action         javaScriptAction
	state          map[string]string
	node           string
	postRegistered bool
}

type remotePreparations map[string]*preparedInvocation

type remotePreparationStatus struct {
	unsuccessful bool
}

const node16DeprecationMessage = "Node.js 16 actions are deprecated. Please update the following actions to use Node.js 20: %s. For more information see: https://github.blog/changelog/2023-09-22-github-actions-transitioning-from-node-16-to-node-20/."

type node16DeprecationWarnings struct {
	mu      sync.Mutex
	actions map[string]struct{}
}

func (w *node16DeprecationWarnings) record(reference string) {
	if w == nil || reference == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.actions == nil {
		w.actions = make(map[string]struct{})
	}
	w.actions[reference] = struct{}{}
}

func (w *node16DeprecationWarnings) emit(processor *commandProcessor) {
	if w == nil || processor == nil {
		return
	}
	w.mu.Lock()
	actions := sortedKeys(w.actions)
	w.mu.Unlock()
	if len(actions) != 0 {
		processor.trustedWarning(fmt.Sprintf(node16DeprecationMessage, strings.Join(actions, ", ")))
	}
}

func actionNodeMajor(runtime metadata.Runtime) (int, bool) {
	switch runtime {
	case metadata.RuntimeNode16:
		return 16, true
	case metadata.RuntimeNode20:
		return 20, true
	case metadata.RuntimeNode24:
		return 24, true
	default:
		return 0, false
	}
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

// verifyWorkflow binds a plan to the workflow bytes in the supplied workspace.
func verifyWorkflow(job plan.Job, workspace string) error {
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
	defer func() {
		if final.Conclusion == "success" && runJobErr != nil && !IsToleratedJobFailure(runJobErr) {
			final.Conclusion = "failure"
		}
	}()
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
	if err := ValidateHost(job, goruntime.GOOS, goruntime.GOARCH); err != nil {
		return JobResult{}, err
	}
	runnerContext, err := canonicalRunnerContext(goruntime.GOOS, goruntime.GOARCH)
	if err != nil {
		return JobResult{}, err
	}
	for _, capability := range job.RequiredCapabilities {
		if capability != "docker" && capability != "secrets" && capability != "network" && capability != "provider-token-read" && capability != "provider-token-write" {
			return JobResult{}, fmt.Errorf("capability %q is unsupported in the job runtime", capability)
		}
	}
	if job.HasCapability("provider-token-read") {
		usesCheckout, err := validateJobCheckoutAdapters(job)
		if err != nil {
			return JobResult{}, fmt.Errorf("provider-token-read checkout preflight: %w", err)
		}
		if !usesCheckout {
			return JobResult{}, fmt.Errorf("provider-token-read capability is restricted to the verified checkout adapter")
		}
		if r.RepositoryCredentials != nil {
			credentials, err := resolveAgentRepositoryCredentialsBeforeWorkflow(r.RepositoryCredentials)
			if err != nil {
				return JobResult{}, err
			}
			r.RepositoryCredentials = credentials
		}
		git, err := resolveHostExecutableBeforeWorkflow(r.Git, "git", "native checkout Git")
		if err != nil {
			return JobResult{}, err
		}
		r.Git = git
	}
	if job.HasCapability("provider-token-write") && r.WorkflowToken == nil {
		return JobResult{}, fmt.Errorf("provider-token-write capability requires the GitHub workflow token provider")
	}
	providerTokenRequired := job.HasCapability("provider-token-write")
	cacheRequired := false
	for _, lock := range job.Actions {
		if usesCacheService(lock) {
			cacheRequired = true
			break
		}
	}
	if providerTokenRequired || r.Cache != nil {
		if r.Redactor == nil {
			if providerTokenRequired {
				return JobResult{}, fmt.Errorf("provider token capability requires the Buildkite Agent redactor")
			}
			if cacheRequired {
				return JobResult{}, fmt.Errorf("actions/cache requires the Buildkite Agent redactor")
			}
			r.Cache = nil
		} else {
			resolved, err := resolveAgentRedactorBeforeWorkflow(r.Redactor)
			if err != nil {
				if providerTokenRequired || cacheRequired {
					return JobResult{}, err
				}
				r.Cache = nil
			} else {
				r.Redactor = resolved
			}
		}
	}
	if len(job.Dependencies) != 0 && len(job.Needs) == 0 {
		return JobResult{}, fmt.Errorf("job has %d static dependencies but no hydrated prerequisite results", len(job.Dependencies))
	}
	processor := newCommandProcessor(r.stdout(), r.stderr())
	r.node16Warnings = &node16DeprecationWarnings{}
	eval := expression.Context{
		Matrix:       job.Matrix,
		Steps:        make(map[string]map[string]string, len(job.Steps)),
		StepStatuses: make(map[string]expression.StepStatus, len(job.Steps)),
		Needs:        needOutputs(job.Needs),
		NeedResults:  needResults(job.Needs),
		Vars:         job.Vars,
		GitHub:       githubContext(job),
		JobStatus:    "success",
		Runner:       runnerContext,
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
	jobCondition := expression.ConditionContext{Needs: eval.Needs, NeedResults: eval.NeedResults, Matrix: job.Matrix, Vars: job.Vars, GitHub: eval.GitHub, Runner: eval.Runner}
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
	if r.Mise == "" && r.ResolveMise != nil {
		r.Mise, err = r.ResolveMise(runCtx)
		if err != nil {
			return tolerateJobSetupFailure(runCtx, job, jobResult, err)
		}
	}

	secrets, err := r.resolveSecrets(runCtx, processor, job.RequiredSecrets)
	if err != nil {
		return tolerateJobSetupFailure(runCtx, job, jobResult, err)
	}
	if job.GitHubToken != nil {
		if secrets == nil {
			secrets = map[string]string{}
		}
		secrets["GITHUB_TOKEN"], err = r.resolveWorkflowToken(runCtx, processor, job.Event.Repository, job.Workflow.Path, job.GitHubToken.Permissions)
		if err != nil {
			return tolerateJobSetupFailure(runCtx, job, jobResult, err)
		}
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
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return jobResult, fmt.Errorf("canonicalize workspace: %w", err)
	}
	hashRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return jobResult, fmt.Errorf("open hashFiles workspace: %w", err)
	}
	defer func() {
		if err := hashRoot.Close(); err != nil {
			runJobErr = errors.Join(runJobErr, err)
		}
	}()
	eval.HashFilesContext = func(ctx context.Context, patterns []string) (string, error) {
		return hashWorkspaceRootFilesWithLimits(ctx, hashRoot, patterns, defaultHashFilesLimits, goruntime.GOOS == "windows")
	}
	eval.HashFiles = func(patterns []string) (string, error) {
		return eval.HashFilesContext(runCtx, patterns)
	}
	if job.HasCapability("docker") {
		workspaceDir, err := os.Open(workspace)
		if err != nil {
			return jobResult, err
		}
		defer func() {
			if err := workspaceDir.Close(); err != nil {
				runJobErr = errors.Join(runJobErr, err)
			}
		}()
		workspaceInfo, err := workspaceDir.Stat()
		if err != nil {
			return jobResult, err
		}
		if err := workspaceDir.Chmod(0o777); err != nil {
			return jobResult, fmt.Errorf("make Docker workspace writable: %w", err)
		}
		if callerWorkspace {
			defer func() {
				if err := workspaceDir.Chmod(workspaceInfo.Mode().Perm()); err != nil {
					runJobErr = errors.Join(runJobErr, err)
				}
			}()
		}
	}
	jobEnv, err := evaluateMap(job.Env, eval)
	if err != nil {
		return tolerateJobSetupFailure(runCtx, job, jobResult, fmt.Errorf("evaluate job environment: %w", err))
	}
	_, explicitJobPATH := jobEnv["PATH"]
	runnerTemp, err := os.MkdirTemp("", "buildkite-gha-runner-")
	if err != nil {
		return jobResult, fmt.Errorf("create runner temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(runnerTemp) }()
	runnerTemp, err = filepath.EvalSymlinks(runnerTemp)
	if err != nil {
		return jobResult, fmt.Errorf("canonicalize runner temp: %w", err)
	}
	if job.HasCapability("docker") {
		if err := os.Chmod(runnerTemp, 0o777); err != nil {
			return jobResult, fmt.Errorf("make Docker runner temp writable: %w", err)
		}
	}
	toolCache := r.ToolCache
	if toolCache == "" {
		toolCache = filepath.Join(runnerTemp, "tool-cache")
		if err := os.Mkdir(toolCache, 0o755); err != nil {
			return jobResult, fmt.Errorf("create runner tool cache: %w", err)
		}
	} else {
		if !filepath.IsAbs(toolCache) || filepath.Clean(toolCache) != toolCache {
			return jobResult, fmt.Errorf("configured runner tool cache must be an absolute canonical path")
		}
		resolved, err := filepath.EvalSymlinks(toolCache)
		if err != nil || resolved != toolCache {
			return jobResult, fmt.Errorf("configured runner tool cache is unavailable or contains a symlink")
		}
		info, err := os.Stat(toolCache)
		if err != nil || !info.IsDir() {
			return jobResult, fmt.Errorf("configured runner tool cache is not a directory")
		}
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
				return tolerateJobSetupFailure(runCtx, job, jobResult, fmt.Errorf("prepare action lock %q: %w", lock.ID, resolveErr))
			}
			actionRuntime, runtimeErr := action.Runtime()
			if runtimeErr != nil {
				return tolerateJobSetupFailure(runCtx, job, jobResult, fmt.Errorf("prepare action lock %q: %w", lock.ID, runtimeErr))
			}
			if entrypointErr := action.ValidateEntrypoints(actionRuntime); entrypointErr != nil {
				return tolerateJobSetupFailure(runCtx, job, jobResult, fmt.Errorf("prepare action lock %q: %w", lock.ID, entrypointErr))
			}
		}
		for _, step := range job.Steps {
			if step.Kind != "uses" || step.Action == nil {
				continue
			}
			if source, sourceErr := actions.source(*step.Action); sourceErr != nil {
				return tolerateJobSetupFailure(runCtx, job, jobResult, sourceErr)
			} else if source == "github" {
				if verifyErr := r.verifyRemoteActionTree(runCtx, actions, *step.Action, nil); verifyErr != nil {
					return tolerateJobSetupFailure(runCtx, job, jobResult, fmt.Errorf("prepare action %q: %w", step.Uses, verifyErr))
				}
			}
		}
		if len(job.Actions) != 0 {
			var mountErr error
			containerMounts, mountErr = r.actionContainerMounts(runCtx, actions)
			if mountErr != nil {
				return tolerateJobSetupFailure(runCtx, job, jobResult, mountErr)
			}
		}
		backend, setupErr := r.startJobContainer(runCtx, processor, workspace, runnerTemp, *job.Container, job.Services, containerMounts...)
		if setupErr != nil {
			return tolerateJobSetupFailure(runCtx, job, jobResult, setupErr)
		}
		r.jobContainer = backend
		r.jobDocker = backend
		eval.Services = backend.servicePorts
		defer func() {
			if err := backend.cleanup(); err != nil {
				runJobErr = errors.Join(runJobErr, err)
			}
		}()
	} else if len(job.Services) != 0 {
		backend, setupErr := r.startJobContainer(runCtx, processor, workspace, runnerTemp, plan.Container{}, job.Services)
		if setupErr != nil {
			return tolerateJobSetupFailure(runCtx, job, jobResult, setupErr)
		}
		r.jobDocker = backend
		eval.Services = backend.servicePorts
		defer func() {
			if err := backend.cleanup(); err != nil {
				runJobErr = errors.Join(runJobErr, err)
			}
		}()
	}
	runtimeEnv := standardEnvironment(job, workspace, runnerTemp, toolCache)
	jobResult.Env = mergeStepEnvironment(runtimeEnv, jobEnv)
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
	statuses := eval.StepStatuses
	supervisor := newBackgroundSupervisor(maxActiveBackgroundSteps)

	var runErr error
	hardFailure := false
	prepared := remotePreparations{}
	preStatus := remotePreparationStatus{}
	preFailures := make(map[int]stepExecution)
	if len(job.Actions) != 0 {
		for stepIndex, step := range job.Steps {
			eval.JobStatus = jobStatusValue(runErr != nil, runCtx.Err() != nil)
			if step.Kind != "uses" {
				continue
			}
			if step.Action == nil {
				err := fmt.Errorf("prepare action %q: immutable selector is missing", step.Uses)
				runErr = errors.Join(runErr, err)
				hardFailure = true
				break
			}
			source, err := actions.source(*step.Action)
			if err != nil {
				err = fmt.Errorf("prepare action %q: %w", step.Uses, err)
				runErr = errors.Join(runErr, err)
				hardFailure = true
				break
			}
			if source != "github" {
				continue
			}
			if err := r.verifyRemoteActionTree(runCtx, actions, *step.Action, nil); err != nil {
				err = fmt.Errorf("prepare action %q: %w", step.Uses, err)
				runErr = errors.Join(runErr, err)
				hardFailure = true
				break
			}
			if entry := actions.locks[step.Action.Lock]; entry != nil && (usesUploadArtifactAdapter(entry.lock) || usesDownloadArtifactAdapter(entry.lock)) {
				continue
			}
			preEnv := mergeStepEnvironment(runtimeEnv, jobResult.Env)
			preCtx, cancelPre := stepContext(runCtx, step.TimeoutMinutes)
			preEval := cloneExpressionContext(eval)
			bindHashFilesContext(preCtx, &preEval)
			wasUnsuccessful := preStatus.unsuccessful
			preResult, preErr := r.prepareRemoteAction(preCtx, processor, workspace, step, strconv.Itoa(stepIndex), preEnv, preEval, &posts, actions, prepared, &preStatus, true, nil)
			commitResultEnvironment(jobResult.Env, preResult)
			mergeInto(jobResult.State, preResult.State)
			appendJobSummary(&jobResult.Summary, &jobResult.summaryTruncated, preResult.Summary, preResult.summaryTruncated)
			eval.Env = jobResult.Env
			if preErr != nil {
				execution := classifyStepExecution(ctx, preCtx, step, newResult(), fmt.Errorf("action %q pre: %w", step.Uses, preErr))
				preFailures[stepIndex] = execution
				if execution.conclusion != "success" {
					preStatus.unsuccessful = true
				} else {
					preStatus.unsuccessful = wasUnsuccessful
				}
				cancelPre()
				continue
			}
			cancelPre()
		}
	}
	for stepIndex, step := range job.Steps {
		eval.JobStatus = jobStatusValue(runErr != nil, runCtx.Err() != nil)
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
				if runCtx.Err() != nil {
					outcome = "cancelled"
				}
				conclusion = outcome
				runErr = errors.Join(runErr, fmt.Errorf("step %q: %w", step.ID, barrierErr))
			}
			statuses[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: outcome, Conclusion: conclusion, Outputs: map[string]string{}}
			eval.Steps[strings.ToLower(step.ID)] = map[string]string{}
			continue
		}
		if execution, ok := preFailures[stepIndex]; ok {
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
			continue
		}
		referencesStatus, err := expression.ReferencesStatusFunction(step.Condition)
		if err != nil {
			execution := classifyStepExecution(ctx, runCtx, step, newResult(), fmt.Errorf("condition: %w", err))
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
			continue
		}
		if !referencesStatus && (runErr != nil || runCtx.Err() != nil) {
			statuses[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: "skipped", Conclusion: "skipped", Outputs: map[string]string{}}
			eval.Steps[strings.ToLower(step.ID)] = map[string]string{}
			continue
		}
		stepCtx, cancelStep := stepContext(runCtx, step.TimeoutMinutes)
		stepEval := cloneExpressionContext(eval)
		bindHashFilesContext(stepCtx, &stepEval)
		stepEnv, err := evaluateStepMap(step.Env, stepEval)
		if err != nil {
			execution := classifyStepExecution(ctx, stepCtx, step, newResult(), fmt.Errorf("environment: %w", err))
			cancelStep()
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
			continue
		}
		stepEval.Env = mergeStringMaps(stepEval.Env, stepEnv)
		condition := expression.ConditionContext{Needs: eval.Needs, NeedResults: eval.NeedResults, Steps: statuses, Env: stepEval.Env, Vars: job.Vars, Matrix: job.Matrix, GitHub: eval.GitHub, Runner: eval.Runner, Services: eval.Services, Failure: runErr != nil && runCtx.Err() == nil, Unsuccessful: runErr != nil, Cancelled: stepCtx.Err() != nil, HashFiles: stepEval.HashFiles}
		run, err := expression.EvaluateCondition(step.Condition, condition)
		if err != nil {
			execution := classifyStepExecution(ctx, stepCtx, step, newResult(), fmt.Errorf("condition: %w", err))
			cancelStep()
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
			continue
		}
		if !run {
			cancelStep()
			statuses[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: "skipped", Conclusion: "skipped", Outputs: map[string]string{}}
			eval.Steps[strings.ToLower(step.ID)] = map[string]string{}
			continue
		}

		jobEnv := mergeStepEnvironment(runtimeEnv, jobResult.Env)
		evalSnapshot := stepEval
		if step.Background {
			cancelStep()
			step := step
			supervisor.start(runCtx, step.ID,
				func(taskCtx context.Context) stepExecution {
					stepCtx, cancelExecution := stepContext(taskCtx, step.TimeoutMinutes)
					defer cancelExecution()
					executionEval := cloneExpressionContext(evalSnapshot)
					bindHashFilesContext(stepCtx, &executionEval)
					return r.executePlanStep(runCtx, stepCtx, processor, workspace, job, step, strconv.Itoa(stepIndex), jobEnv, stepEnv, executionEval, &posts, actions, prepared)
				},
				func(stepCtx context.Context) stepExecution {
					return cancelledStepExecution(runCtx, stepCtx, step)
				},
			)
			continue
		}
		processor.logSection(stepDisplayName(step, evalSnapshot))
		execution := r.executePlanStep(runCtx, stepCtx, processor, workspace, job, step, strconv.Itoa(stepIndex), jobEnv, stepEnv, evalSnapshot, &posts, actions, prepared)
		cancelStep()
		if execution.outcome == "failure" {
			processor.expandCurrentSection()
		}
		runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
	}
	for _, execution := range supervisor.waitAll() {
		runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval, statuses))
	}
	if runCtx.Err() != nil {
		runErr = errors.Join(runErr, runCtx.Err())
	}

	postCtx, cancelPosts := postPhaseContext(runCtx, r.postActionTimeout(), r.cleanupTimeout())
	defer cancelPosts()
	registeredPosts := posts.snapshot()
	for i := len(registeredPosts) - 1; i >= 0; i-- {
		post := registeredPosts[i]
		if post.invocation != nil {
			post.action = post.invocation.action
			post.state = post.invocation.state
			post.node = post.invocation.node
		}
		runPost, conditionErr := expression.EvaluateActionLifecycleCondition(post.condition, runErr != nil, runCtx.Err() != nil)
		if conditionErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("post action %q condition: %w", post.action.Name, conditionErr))
			continue
		}
		if !runPost {
			continue
		}
		if len(post.action.jobStatusInputs) != 0 {
			post.action.Inputs = cloneStrings(post.action.Inputs)
			status := jobStatusValue(runErr != nil, runCtx.Err() != nil)
			for _, name := range post.action.jobStatusInputs {
				post.action.Inputs[name] = status
			}
		}
		postResult := newResult()
		postResult.Env = cloneStrings(jobResult.Env)
		postErr := r.runJavaScriptPhase(postCtx, processor, workspace, post.node, post.action, post.action.Post, post.state, post.state, &postResult)
		mergeInto(jobResult.Env, postResult.Env)
		mergeInto(jobResult.State, postResult.State)
		appendJobSummary(&jobResult.Summary, &jobResult.summaryTruncated, postResult.Summary, postResult.summaryTruncated)
		if postErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("post action %q: %w", post.action.Name, postErr))
		}
	}

	r.node16Warnings.emit(processor)
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
	if runCtx.Err() != nil {
		jobResult.Conclusion = "cancelled"
	} else if runErr == nil {
		jobResult.Conclusion = "success"
	} else if job.ContinueOnError && !hardFailure && !isHardJobFailure(runErr) {
		jobResult.Conclusion = "success"
		runErr = &toleratedJobFailure{err: runErr}
	}
	return scrubJobResult(jobResult, sensitiveValues), runErr
}

func stepDisplayName(step plan.Step, eval expression.Context) string {
	if step.Name != "" {
		if name, err := expression.Evaluate(step.Name, eval); err == nil {
			return name
		}
		return step.Name
	}
	if step.Uses != "" {
		return step.Uses
	}
	return step.ID
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
	result.WarningAnnotations, result.warningsTruncated = finalizeWorkflowCommandAnnotations(result.WarningAnnotations, result.warningsTruncated)
	result.ErrorAnnotations, result.errorsTruncated = finalizeWorkflowCommandAnnotations(result.ErrorAnnotations, result.errorsTruncated)
	return result
}

func finalizeWorkflowCommandAnnotations(value string, truncated bool) (string, bool) {
	var bounded string
	var boundedTruncated bool
	appendBoundedText(&bounded, &boundedTruncated, value, truncated, maxJobAnnotationBytes, workflowCommandTruncationNotice)
	if boundedTruncated {
		bounded += workflowCommandTruncationNotice
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

func (r Runner) resolveWorkflowToken(ctx context.Context, processor *commandProcessor, repository, workflowPath string, permissions map[string]string) (string, error) {
	if r.WorkflowToken == nil {
		return "", fmt.Errorf("GitHub workflow token provider is not configured")
	}
	workflow, err := plan.GitHubWorkflowPolicyFilename(workflowPath)
	if err != nil {
		return "", err
	}
	token, err := r.WorkflowToken.WorkflowToken(ctx, repository, workflow, permissions)
	if err != nil {
		return "", err
	}
	if len(token) > 16<<10 || !githubInstallationTokenPattern.MatchString(token) {
		return "", fmt.Errorf("GitHub workflow token provider returned an invalid token")
	}
	if r.Redactor == nil {
		return "", fmt.Errorf("GitHub workflow token provider requires a redactor")
	}
	processor.addMask(token)
	if err := r.Redactor.AddRedaction(ctx, token); err != nil {
		return "", processor.scrubError(err)
	}
	return token, nil
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
		"head_ref":   job.Event.HeadRef,
		"sha":        job.Event.SHA,
		"actor":      job.Event.Actor,
		"event_name": job.Event.Name,
		"server_url": plan.EventServerURL(job.Event.Provider),
	}
}

func standardEnvironment(job plan.Job, workspace, runnerTemp, toolCache string) map[string]string {
	runner, _ := canonicalRunnerContext(goruntime.GOOS, goruntime.GOARCH)
	env := map[string]string{
		"CI":                "true",
		"GITHUB_ACTIONS":    "true",
		"GITHUB_ACTOR":      job.Event.Actor,
		"GITHUB_EVENT_NAME": job.Event.Name,
		"GITHUB_JOB":        job.Workflow.LogicalJobID,
		"GITHUB_REF":        job.Event.Ref,
		"GITHUB_REPOSITORY": job.Event.Repository,
		"GITHUB_SERVER_URL": plan.EventServerURL(job.Event.Provider),
		"GITHUB_SHA":        job.Event.SHA,
		"GITHUB_WORKSPACE":  workspace,
		"RUNNER_OS":         runner["os"],
		"RUNNER_ARCH":       runner["arch"],
		"RUNNER_TEMP":       runnerTemp,
		"RUNNER_TOOL_CACHE": toolCache,
	}
	if imageOS := runnerImageOS(); imageOS != "" {
		env["ImageOS"] = imageOS
	}
	return env
}

func canonicalRunnerContext(goos, goarch string) (map[string]string, error) {
	switch {
	case goos == "linux" && goarch == "amd64":
		return map[string]string{"os": "Linux", "arch": "X64"}, nil
	case goos == "darwin" && goarch == "arm64":
		return map[string]string{"os": "macOS", "arch": "ARM64"}, nil
	default:
		return nil, fmt.Errorf("unsupported runner platform %s/%s", goos, goarch)
	}
}

// ValidateHost rejects plans that the concrete runtime host cannot execute.
func ValidateHost(job plan.Job, goos, goarch string) error {
	if _, err := canonicalRunnerContext(goos, goarch); err != nil {
		return err
	}
	if goos == "darwin" {
		switch {
		case job.HasCapability("docker"):
			return fmt.Errorf("docker capability is unsupported on macOS runners")
		case job.Container != nil:
			return fmt.Errorf("job containers are unsupported on macOS runners")
		case len(job.Services) != 0:
			return fmt.Errorf("services are unsupported on macOS runners")
		}
	}
	return nil
}

func runnerImageOS() string {
	if goruntime.GOOS != "linux" {
		return ""
	}
	osRelease, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	return ubuntuImageOS(osRelease)
}

func ubuntuImageOS(osRelease []byte) string {
	values := make(map[string]string, 2)
	for line := range strings.SplitSeq(string(osRelease), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || (name != "ID" && name != "VERSION_ID") {
			continue
		}
		values[name] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if values["ID"] != "ubuntu" {
		return ""
	}
	switch values["VERSION_ID"] {
	case "22.04":
		return "ubuntu22"
	case "24.04":
		return "ubuntu24"
	default:
		return ""
	}
}

// invocationEnvironment owns the environment overlay order for one step
// invocation: job environment first, step environment second, with runtime
// context names protected. It also captures whether the job or step pinned
// PATH explicitly, which decides Docker action PATH precedence.
type invocationEnvironment struct {
	jobEnv       map[string]string
	stepEnv      map[string]string
	explicitPATH bool
}

func (r Runner) invocationEnvironment(jobEnv, stepEnv map[string]string) invocationEnvironment {
	_, stepPATH := stepEnv["PATH"]
	jobPATH := r.explicitJobPATH || jobEnv["PATH"] != r.implicitJobPATH
	return invocationEnvironment{jobEnv: jobEnv, stepEnv: stepEnv, explicitPATH: jobPATH || stepPATH}
}

// process is the environment for shell and JavaScript step processes.
func (e invocationEnvironment) process() map[string]string {
	return mergeStepEnvironment(e.jobEnv, e.stepEnv)
}

// docker overlays action-declared environment beneath the invocation
// environment: action values fill only unset names, except that the action
// may replace PATH when neither the job nor the step pinned one. It returns
// the container environment and whether any layer set PATH explicitly, which
// makes the Docker image PATH yield.
func (e invocationEnvironment) docker(actionEnv, inputs map[string]string) (map[string]string, bool) {
	env := mergeStepEnvironment(e.jobEnv, e.stepEnv, actionInputEnv(inputs))
	for name, value := range actionEnv {
		if _, exists := env[name]; !exists || (name == "PATH" && !e.explicitPATH) {
			env[name] = value
		}
	}
	_, actionPATH := actionEnv["PATH"]
	return env, e.explicitPATH || actionPATH
}

func mergeStepEnvironment(base map[string]string, overlays ...map[string]string) map[string]string {
	out := mergeStringMaps(append([]map[string]string{base}, overlays...)...)
	for name, value := range base {
		if isRuntimeContextEnvironment(name) {
			out[name] = value
		}
	}
	return out
}

func isRuntimeContextEnvironment(name string) bool {
	// GITHUB_ACTION_PATH is invocation-scoped and overlaid by action runtimes,
	// not protected for ordinary top-level steps.
	switch name {
	case "GITHUB_ACTIONS",
		"GITHUB_ACTOR",
		"GITHUB_EVENT_NAME",
		"GITHUB_JOB",
		"GITHUB_REF",
		"GITHUB_REPOSITORY",
		"GITHUB_SERVER_URL",
		"GITHUB_SHA",
		"GITHUB_WORKSPACE",
		"RUNNER_ARCH",
		"RUNNER_OS",
		"RUNNER_TEMP",
		"RUNNER_TOOL_CACHE":
		return true
	default:
		return false
	}
}

func (r Runner) runJobStep(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv, stepEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations) (Result, error) {
	return r.runActionStep(ctx, processor, workspace, job, step, invocationID, jobEnv, stepEnv, eval, posts, actions, prepared, nil)
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
	if usesNativeAdapter(lock) {
		return nil
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
			return markHardJobFailure(fmt.Errorf("composite action step %d child %q has no immutable selector", i+1, child.Uses))
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
		if usesNativeAdapter(lock) {
			continue
		}
		if major, ok := actionNodeMajor(actionRuntime); ok {
			requiredNode[major] = true
		}
	}
	if unknownWorkspaceRuntime && actions.job.NeedsMise() {
		requiredNode[16], requiredNode[24] = true, true
	}
	for _, major := range []int{16, 20, 24} {
		if !requiredNode[major] {
			continue
		}
		explicit := r.explicitNode(major)
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
			r.setExplicitNode(major, explicit)
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

func (r Runner) prepareRemoteAction(ctx context.Context, processor *commandProcessor, workspace string, step plan.Step, invocationID string, jobEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, status *remotePreparationStatus, workflowStep bool, inheritedEvalErr error) (Result, error) {
	result := newResult()
	eval.JobStatus = jobStatusValue(status.unsuccessful, ctx.Err() != nil)
	evaluate := evaluateMap
	if workflowStep {
		evaluate = evaluateStepMap
	} else {
		eval.HashFiles = nil
	}
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
	case metadata.RuntimeNode16, metadata.RuntimeNode20, metadata.RuntimeNode24:
		// The checkout adapter replaces the verified action's JavaScript
		// lifecycle as one indivisible operation. Do not register upstream
		// checkout cleanup for a main phase that this runtime never executes.
		if usesCheckoutAdapter(lock) || usesUploadArtifactAdapter(lock) || usesDownloadArtifactAdapter(lock) {
			return result, nil
		}
		runPre, err := expression.EvaluateActionLifecycleCondition(action.Runs.PreIf, status.unsuccessful, ctx.Err() != nil)
		if err != nil {
			return result, fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err)
		}
		if _, err := expression.EvaluateActionLifecycleCondition(action.Runs.PostIf, false, false); err != nil {
			return result, fmt.Errorf("JavaScript action %q post-if: %w", step.Uses, err)
		}
		if action.Runs.Pre == "" && action.Runs.Post == "" {
			return result, nil
		}
		major, _ := actionNodeMajor(runtime)
		explicit := r.explicitNode(major)
		jobStatusInputs, err := actionJobStatusInputs(action, step.With)
		if err != nil {
			return result, err
		}
		javascript := javaScriptAction{Name: actionName(action, step), Path: action.Path, Pre: action.Runs.Pre, Main: action.Runs.Main, Post: action.Runs.Post, Cache: usesCacheService(lock), OverrideGitHubServerURL: overridesGitHubServerURL(lock), nodeMajor: major, reference: step.Uses, jobStatusInputs: jobStatusInputs}
		invocation := &preparedInvocation{action: javascript, state: map[string]string{}}
		prepared[invocationID] = invocation
		if javascript.Pre != "" && runPre {
			if inheritedEvalErr != nil {
				return result, inheritedEvalErr
			}
			stepEnv, err := evaluate(step.Env, eval)
			if err != nil {
				return result, err
			}
			eval.Env = mergeStringMaps(eval.Env, stepEnv)
			inputs, err := evaluate(step.With, eval)
			if err != nil {
				return result, err
			}
			inputs, err = resolveActionInputs(action, inputs, eval)
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
			if err := r.runJavaScriptPhase(ctx, processor, workspace, node, javascript, javascript.Pre, nil, invocation.state, &result); err != nil {
				return result, err
			}
		}
		return result, nil
	case metadata.RuntimeComposite:
		inputs := map[string]string{}
		stepEnv := map[string]string{}
		compositeEvalErr := inheritedEvalErr
		if compositeEvalErr == nil {
			stepEnv, compositeEvalErr = evaluate(step.Env, eval)
		}
		if compositeEvalErr == nil {
			eval.Env = mergeStringMaps(eval.Env, stepEnv)
			inputs, compositeEvalErr = evaluate(step.With, eval)
		}
		if compositeEvalErr == nil {
			inputs, compositeEvalErr = resolveActionInputs(action, inputs, eval)
		}
		eval.Inputs = inputs
		compositeProcessEnv := mergeStepEnvironment(jobEnv, stepEnv)
		compositeExpressionEnv := mergeStringMaps(eval.Env, stepEnv)
		for i, childStep := range action.Runs.Steps {
			if childStep.Uses == "" {
				continue
			}
			selector, ok := lock.Children[childStep.Uses]
			if !ok || selector.Lock == "" {
				return result, markHardJobFailure(fmt.Errorf("composite action step %d child %q has no immutable selector", i+1, childStep.Uses))
			}
			child := plan.Step{ID: childStep.ID, Name: childStep.Name, Kind: "uses", Uses: childStep.Uses, With: childStep.With, Env: childStep.Env, Action: &plan.ActionSelector{Lock: selector.Lock}}
			childProcessEnv := mergeStepEnvironment(compositeProcessEnv, result.Env)
			eval.Env = mergeStringMaps(compositeExpressionEnv, result.Env)
			childResult, childErr := r.prepareRemoteAction(ctx, processor, workspace, child, fmt.Sprintf("%s/%d", invocationID, i), childProcessEnv, eval, posts, actions, prepared, status, false, compositeEvalErr)
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

func (r Runner) runActionStep(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv, stepEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, actionStack []string) (Result, error) {
	if stepEnv == nil {
		var err error
		stepEnv, err = evaluateStepMap(step.Env, eval)
		if err != nil {
			return newResult(), err
		}
	}
	environment := r.invocationEnvironment(jobEnv, stepEnv)
	result := newResult()
	if step.Kind == "run" {
		script, err := expression.EvaluateStep(step.Command, eval)
		if err != nil {
			return result, err
		}
		shell := step.Shell
		if shell == "" {
			shell = job.DefaultShell
			shell, err = expression.Evaluate(shell, eval)
		} else {
			shell, err = expression.EvaluateStep(shell, eval)
		}
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
			workingDirectory, err = expression.Evaluate(workingDirectory, eval)
		} else {
			workingDirectory, err = expression.EvaluateStep(workingDirectory, eval)
		}
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
		runEnv := environment.process()
		err = r.runProcess(ctx, processor, dir, runEnv, &result, nil, args[0], args[1:]...)
		return result, err
	}

	var action metadata.Metadata
	var actionLock *plan.ActionLock
	if len(job.Actions) != 0 {
		if step.Action == nil {
			return result, markHardJobFailure(fmt.Errorf("action %q has no immutable selector", step.Uses))
		}
		resolvedAction, lock, err := actions.resolve(ctx, *step.Action)
		if err != nil {
			return result, err
		}
		action, actionLock = resolvedAction, &lock
		if usesCheckoutAdapter(lock) {
			inputs, err := evaluateStepMap(step.With, eval)
			if err != nil {
				return result, err
			}
			if err := validateCheckoutRefProvenance(step.With, inputs, job.Event.SHA); err != nil {
				return result, err
			}
			return r.runCheckout(ctx, processor, workspace, job, inputs)
		}
		if usesUploadArtifactAdapter(lock) {
			inputs, err := evaluateStepMap(step.With, eval)
			if err != nil {
				return result, err
			}
			return r.runUploadArtifactCommit(ctx, processor, workspace, lock.Commit, inputs)
		}
		if usesDownloadArtifactAdapter(lock) {
			inputs, err := evaluateStepMap(step.With, eval)
			if err != nil {
				return result, err
			}
			return r.runDownloadArtifact(ctx, processor, workspace, job.Needs, lock.Commit, inputs)
		}
	} else {
		if !strings.HasPrefix(step.Uses, "./") {
			return result, fmt.Errorf("remote action %q is unsupported in the supported runtime subset", step.Uses)
		}
		if err := verifyWorkflow(job, workspace); err != nil {
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
	inputs, err := evaluateStepMap(step.With, eval)
	if err != nil {
		return result, err
	}
	jobStatusInputs, err := actionJobStatusInputs(action, inputs)
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
	case metadata.RuntimeNode16, metadata.RuntimeNode20, metadata.RuntimeNode24:
		if action.Runs.Main == "" {
			return result, fmt.Errorf("JavaScript action %q has no main entry point", step.Uses)
		}
		if _, err := expression.EvaluateActionLifecycleCondition(action.Runs.PreIf, false, false); err != nil {
			return result, fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err)
		}
		if _, err := expression.EvaluateActionLifecycleCondition(action.Runs.PostIf, false, false); err != nil {
			return result, fmt.Errorf("JavaScript action %q post-if: %w", step.Uses, err)
		}
		major, _ := actionNodeMajor(actionRuntime)
		explicit := r.explicitNode(major)
		node, err := r.discoverNode(ctx, major, explicit)
		if err != nil {
			return result, err
		}
		actionEnv := environment.process()
		javascript := javaScriptAction{Name: actionName(action, step), Path: actionPath, Pre: action.Runs.Pre, Main: action.Runs.Main, Post: action.Runs.Post, Inputs: inputs, Env: actionEnv, Cache: actionLock != nil && usesCacheService(*actionLock), OverrideGitHubServerURL: actionLock != nil && overridesGitHubServerURL(*actionLock), nodeMajor: major, reference: step.Uses, jobStatusInputs: jobStatusInputs}
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
			javascript.Env = environment.process()
			invocation.action = javascript
		}
		if !wasPrepared {
			posts.register(postFor(javascript, state, node, action.Runs.PostIf))
		} else if invocation := prepared[invocationID]; !invocation.postRegistered {
			posts.register(postForInvocation(invocation, action.Runs.PostIf))
			invocation.postRegistered = true
		}
		if javascript.Pre != "" && !wasPrepared {
			runPre, _ := expression.EvaluateActionLifecycleCondition(action.Runs.PreIf, false, false)
			if runPre {
				if err := r.runJavaScriptPhase(ctx, processor, workspace, node, javascript, javascript.Pre, nil, state, &result); err != nil {
					return result, err
				}
			}
		}
		if err := r.runJavaScriptPhase(ctx, processor, workspace, node, javascript, javascript.Main, nil, state, &result); err != nil {
			return result, err
		}
		return result, nil
	case metadata.RuntimeComposite:
		composite, err := r.runCompositeMetadata(ctx, processor, workspace, job, actionPath, action, inputs, invocationID, jobEnv, stepEnv, actionEval, posts, actions, prepared, actionLock, actionStack)
		return composite, err
	case metadata.RuntimeDocker:
		if goruntime.GOOS == "darwin" {
			return result, fmt.Errorf("docker action %q is unsupported on macOS runners", step.Uses)
		}
		if !job.HasCapability("docker") {
			return result, fmt.Errorf("docker action %q requires the plan's docker capability", step.Uses)
		}
		if action.Runs.PreEntrypoint != "" || action.Runs.PostEntrypoint != "" || action.Runs.Entrypoint != "" || len(action.Runs.Args) != 0 {
			return result, fmt.Errorf("docker action %q uses unsupported entrypoint, arguments, or pre/post lifecycle", step.Uses)
		}
		if action.Runs.Image != "Dockerfile" {
			return result, fmt.Errorf("docker action image %q is unsupported; the supported runtime subset requires a local Dockerfile", action.Runs.Image)
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
		invocationEnv, explicitPATH := environment.docker(dockerEnv, inputs)
		result, err := r.runDocker(ctx, processor, dockerAction{Name: actionName(action, step), Path: actionPath, SourceRoot: sourceRoot, SourceDigest: sourceDigest, Workspace: workspace, Env: invocationEnv, explicitPATH: explicitPATH})
		return result, err
	}
	return result, fmt.Errorf("action %q uses unsupported runtime %q", step.Uses, actionRuntime)
}

func (r Runner) runCompositeMetadata(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, actionPath string, action metadata.Metadata, inputs map[string]string, invocationID string, jobEnv, stepEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, actionLock *plan.ActionLock, actionStack []string) (Result, error) {
	result := newResult()
	// hashFiles is a workflow-step surface. Do not extend it into action
	// metadata expressions while executing a composite action.
	eval.HashFiles = nil
	eval.HashFilesContext = nil
	eval.Inputs = inputs
	eval.Steps = make(map[string]map[string]string)
	eval.StepStatuses = make(map[string]expression.StepStatus)
	statuses := eval.StepStatuses
	compositeProcessEnv := mergeStepEnvironment(jobEnv, stepEnv)
	compositeProcessEnv["GITHUB_ACTION_PATH"] = actionPath
	compositeExpressionEnv := mergeStringMaps(eval.Env, stepEnv)
	inheritedFailure := eval.JobStatus == "failure"
	inheritedCancelled := eval.JobStatus == "cancelled"
	inheritedUnsuccessful := inheritedFailure || inheritedCancelled
	var runErr error
	for i, step := range action.Runs.Steps {
		failure := inheritedFailure || runErr != nil
		cancelled := inheritedCancelled || ctx.Err() != nil
		unsuccessful := inheritedUnsuccessful || runErr != nil
		eval.JobStatus = jobStatusValue(unsuccessful, cancelled)
		// GITHUB_ENV effects are visible to subsequent children. Keep this
		// composite's invocation environment in expression contexts, while
		// rebuilding the map so a child's declared env cannot leak to siblings.
		eval.Env = mergeStringMaps(compositeExpressionEnv, result.Env)
		id := strings.ToLower(step.ID)
		condition := expression.ConditionContext{Inputs: eval.Inputs, Needs: eval.Needs, NeedResults: eval.NeedResults, Steps: statuses, Env: eval.Env, Vars: eval.Vars, Matrix: eval.Matrix, GitHub: eval.GitHub, Runner: eval.Runner, Services: eval.Services, Failure: failure, Unsuccessful: unsuccessful, Cancelled: cancelled}
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
		childJobEnv := mergeStepEnvironment(compositeProcessEnv, result.Env)
		childJobEnv["GITHUB_ACTION_PATH"] = actionPath
		if step.Uses != "" {
			// runActionStep owns template evaluation for an action invocation.
			// Passing env (the evaluated map) here would evaluate expressions twice.
			child := plan.Step{ID: step.ID, Name: step.Name, Kind: "uses", Uses: step.Uses, With: step.With, Env: step.Env}
			if actionLock != nil {
				selector, ok := actionLock.Children[step.Uses]
				if !ok {
					childErr = markHardJobFailure(fmt.Errorf("composite action child %q has no immutable selector", step.Uses))
				} else {
					child.Action = &plan.ActionSelector{Lock: selector.Lock}
				}
			}
			if childErr == nil {
				stepResult, childErr = r.runActionStep(ctx, processor, workspace, job, child, fmt.Sprintf("%s/%d", invocationID, i), childJobEnv, nil, eval, posts, actions, prepared, actionStack)
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

func evaluateStepMap(values map[string]string, context expression.Context) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, name := range sortedKeys(values) {
		resolved, err := expression.EvaluateStep(values[name], context)
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
	defaultContext := context
	if token, ok := context.Secrets["GITHUB_TOKEN"]; ok {
		defaultContext.GitHub = cloneAnyMap(context.GitHub)
		defaultContext.GitHub["token"] = token
	}
	for _, name := range names {
		definition := action.Inputs[name]
		if _, ok := inputs[name]; ok {
			continue
		}
		if definition.Default != nil {
			defaultContext.Inputs = inputs
			value, err := expression.EvaluateActionInputDefault(*definition.Default, defaultContext)
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

func actionJobStatusInputs(action metadata.Metadata, supplied map[string]string) ([]string, error) {
	var inputs []string
	for _, name := range sortedKeys(action.Inputs) {
		definition := action.Inputs[name]
		if definition.Default == nil {
			continue
		}
		suppliedInput := false
		for candidate := range supplied {
			if strings.EqualFold(candidate, name) {
				suppliedInput = true
				break
			}
		}
		if suppliedInput {
			continue
		}
		references, err := expression.ReferencesJobStatus(*definition.Default)
		if err != nil {
			return nil, fmt.Errorf("action input %q default: %w", name, err)
		}
		if references {
			inputs = append(inputs, name)
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
		return nil, fmt.Errorf("shell %q is unsupported in the supported runtime subset", shell)
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

func postFor(action javaScriptAction, state map[string]string, node, condition string) *registeredPost {
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

func jobStatusValue(unsuccessful, cancelled bool) string {
	if cancelled {
		return "cancelled"
	}
	if unsuccessful {
		return "failure"
	}
	return "success"
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

func (r Runner) postActionTimeout() time.Duration {
	if r.PostActionTimeout > 0 {
		return r.PostActionTimeout
	}
	// Preserve the existing test/operator override while separating the normal
	// post-action budget from short resource cleanup.
	if r.CleanupTimeout > 0 {
		return r.CleanupTimeout
	}
	return defaultPostActionTimeout
}

// postPhaseContext gives JavaScript post actions one bounded shared budget.
// Cancellation still permits the existing short cleanup grace before stopping
// an in-flight post action.
func postPhaseContext(parent context.Context, timeout, cancelGrace time.Duration) (context.Context, context.CancelFunc) {
	postCtx, cancelPosts := context.WithTimeout(context.Background(), timeout)
	postDone := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-parent.Done():
			timer := time.NewTimer(cancelGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancelPosts()
			case <-postDone:
			}
		case <-postDone:
		}
	}()
	return postCtx, func() {
		once.Do(func() { close(postDone) })
		cancelPosts()
	}
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
