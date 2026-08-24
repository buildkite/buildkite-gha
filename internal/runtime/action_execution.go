package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	shellcompat "github.com/buildkite/buildkite-gha/internal/shell"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// verifyWorkflow binds a plan to the workflow bytes in the supplied workspace.
func verifyWorkflow(job plan.Job, workspace string) error {
	if job.Workflow.Remote != nil {
		// Remote workflow bytes were read from the immutable repository tree
		// recorded in the content-addressed plan. They are not part of the
		// caller workspace; Job.Validate binds their path, commit, tree digest,
		// and file digest instead.
		return nil
	}
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

func (r *jobRun) prepare(ctx context.Context) (final JobResult, runJobErr error) {
	job := r.job
	workspace := r.workspace
	callerWorkspace := r.callerWorkspace
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
	jobResult := JobResult{Conclusion: "failure", Outputs: map[string]string{}, Env: map[string]string{}, State: map[string]string{}, Artifacts: []transport.ResultArtifact{}}
	if len(job.DeferredInputs) != 0 {
		if !deferredInputsHydrated(job.DeferredInputs, job.DeferredInputValues) {
			return jobResult, fmt.Errorf("deferred reusable-workflow inputs were not hydrated")
		}
		job.Inputs = mergeWorkflowInputs(job.Inputs, job.DeferredInputValues)
		r.job = job
	}
	guardsPass, err := evaluateCallGuards(job)
	if err != nil {
		return jobResult, err
	}
	if !guardsPass {
		jobResult.Conclusion = "skipped"
		return jobResult, nil
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
		if gitLFS, lfsErr := resolveHostExecutableBeforeWorkflow(r.GitLFS, "git-lfs", "native checkout Git LFS"); lfsErr == nil {
			r.GitLFS = gitLFS
		} else {
			r.GitLFS = ""
		}
	}
	if job.HasCapability("provider-token-write") && r.WorkflowToken == nil {
		return JobResult{}, fmt.Errorf("provider-token-write capability requires the GitHub workflow token provider")
	}
	if job.IDTokenPermission == "write" && r.OIDCToken == nil {
		return JobResult{}, fmt.Errorf("id-token write permission requires the OIDC token provider")
	}
	providerTokenRequired := job.HasCapability("provider-token-write")
	oidcTokenRequired := job.IDTokenPermission == "write"
	secretsRequired := job.HasCapability("secrets")
	if secretsRequired {
		if r.Secrets == nil {
			return JobResult{}, fmt.Errorf("secrets capability requires the Buildkite Agent secret resolver")
		}
		resolved, err := resolveAgentSecretsBeforeWorkflow(r.Secrets)
		if err != nil {
			return JobResult{}, err
		}
		r.Secrets = resolved
	}
	cacheRequired := slices.ContainsFunc(job.Actions, usesCacheService)
	if providerTokenRequired || oidcTokenRequired || secretsRequired || r.Cache != nil {
		if r.Redactor == nil {
			if providerTokenRequired {
				return JobResult{}, fmt.Errorf("provider token capability requires the Buildkite Agent redactor")
			}
			if oidcTokenRequired {
				return JobResult{}, fmt.Errorf("id-token write permission requires the Buildkite Agent redactor")
			}
			if secretsRequired {
				return JobResult{}, fmt.Errorf("secrets capability requires the Buildkite Agent redactor")
			}
			if cacheRequired {
				return JobResult{}, fmt.Errorf("actions/cache requires the Buildkite Agent redactor")
			}
			r.Cache = nil
		} else {
			resolved, err := resolveAgentRedactorBeforeWorkflow(r.Redactor)
			if err != nil {
				if providerTokenRequired || oidcTokenRequired || secretsRequired || cacheRequired {
					return JobResult{}, err
				}
				r.Cache = nil
			} else {
				r.Redactor = resolved
			}
		}
	}
	if len(job.NeedSources) != 0 && len(job.Needs) == 0 {
		return jobResult, fmt.Errorf("job has prerequisite sources but no hydrated prerequisite results")
	}
	processor := newCommandProcessor(r.stdout(), r.stderr())
	eval := expression.Context{
		WorkflowInputs: job.Inputs,
		Matrix:         job.Matrix,
		Steps:          make(map[string]expression.StepStatus, len(job.Steps)),
		Needs:          needStatuses(job.Needs),
		Vars:           job.Vars,
		GitHub:         githubContext(job),
		JobStatus:      "success",
		Runner:         runnerContext,
	}
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
	jobCondition := expression.ConditionContext{Inputs: job.Inputs, Needs: eval.Needs, Matrix: job.Matrix, Vars: job.Vars, GitHub: eval.GitHub, Runner: eval.Runner}
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
	if oidcTokenRequired {
		// Post actions run on a bounded cleanup context after job cancellation.
		// Keep their ID-token requests alive for the same lifecycle.
		serviceCtx, cancelService := context.WithCancel(context.WithoutCancel(runCtx))
		r.idTokenService, err = startIDTokenService(serviceCtx, r.OIDCToken, r.Redactor, processor)
		if err != nil {
			cancelService()
			return jobResult, err
		}
		defer func() { runJobErr = errors.Join(runJobErr, r.idTokenService.Close(runCtx)) }()
		defer cancelService()
	}
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
	if len(job.SecretMappings) != 0 {
		projected := make(map[string]string, len(job.SecretMappings))
		for alias, source := range job.SecretMappings {
			projected[alias] = secrets[source]
		}
		secrets = projected
	}
	if job.GitHubToken != nil {
		if secrets == nil {
			secrets = map[string]string{}
		}
		token, err := r.resolveWorkflowToken(runCtx, processor, job.Event.Repository, job.GitHubToken.Workflow, job.GitHubToken.Permissions)
		if err != nil {
			return tolerateJobSetupFailure(runCtx, job, jobResult, err)
		}
		secrets["GITHUB_TOKEN"] = token
		for _, alias := range job.GitHubToken.Aliases {
			secrets[alias] = token
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
	jobEnv, err := evaluateJobEnvironmentMap(job.Env, eval)
	if err != nil {
		return tolerateJobSetupFailure(runCtx, job, jobResult, fmt.Errorf("evaluate job environment: %w", err))
	}
	serviceEval := eval
	serviceEval.Env = jobEnv
	services, evaluatedServiceOrder, err := evaluateServiceMap(job.Services, job.ServiceOrder, job.ServicesExpression, serviceEval)
	if err != nil {
		return tolerateJobSetupFailure(runCtx, job, jobResult, fmt.Errorf("evaluate services: %w", err))
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
	prebuiltDocker, err := r.preparePrebuiltDockerActions(runCtx, processor, actions)
	if err != nil {
		return tolerateJobSetupFailure(runCtx, job, jobResult, err)
	}
	if prebuiltDocker != nil {
		r.prebuiltDocker = prebuiltDocker
		defer func() {
			if err := prebuiltDocker.cleanup(); err != nil {
				runJobErr = errors.Join(runJobErr, markHardJobFailure(err))
			}
		}()
	}
	var containerMounts []containerMount
	if job.Container != nil {
		// Remote children of workspace composites are already present in the
		// immutable lock graph even when the workspace action itself must remain
		// lazy. Materialize every remote lock now because bind mounts cannot be
		// added after the persistent container is created.
		for _, lock := range job.Actions {
			if lock.Source != "github" || usesNativeAdapter(lock) {
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
		backend, setupErr := r.startJobContainerOrdered(runCtx, processor, workspace, runnerTemp, job.Container, services, evaluatedServiceOrder, containerMounts...)
		if setupErr != nil {
			return tolerateJobSetupFailure(runCtx, job, jobResult, setupErr)
		}
		r.jobContainer = backend
		r.jobDocker = backend
		eval.Services = backend.servicePorts
		defer func() {
			if err := backend.cleanup(runCtx); err != nil {
				runJobErr = errors.Join(runJobErr, err)
			}
		}()
	} else if len(services) != 0 {
		backend, setupErr := r.startJobContainerOrdered(runCtx, processor, workspace, runnerTemp, nil, services, evaluatedServiceOrder)
		if setupErr != nil {
			return tolerateJobSetupFailure(runCtx, job, jobResult, setupErr)
		}
		r.jobDocker = backend
		eval.Services = backend.servicePorts
		defer func() {
			if err := backend.cleanup(runCtx); err != nil {
				runJobErr = errors.Join(runJobErr, err)
			}
		}()
	}
	runnerContext["temp"] = runnerTemp
	if r.jobContainer != nil {
		runnerContext["temp"] = r.jobContainer.containerPath(runnerTemp)
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
	r.workspace = workspace
	r.processor = processor
	r.eval = eval
	r.result = jobResult
	r.runtimeEnv = runtimeEnv
	r.actions = actions
	r.posts = &postRegistry{}
	r.supervisor = newBackgroundSupervisor(maxActiveBackgroundSteps)
	r.prepared = remotePreparations{}
	r.preFailures = make(map[int]stepExecution)
	return r.runPreActions(ctx, runCtx)
}

func (r *jobRun) runPreActions(ctx, runCtx context.Context) (JobResult, error) {
	job := r.job
	workspace := r.workspace
	processor := r.processor
	eval := r.eval
	jobResult := r.result
	runtimeEnv := r.runtimeEnv
	actions := r.actions
	posts := r.posts
	prepared := r.prepared
	runErr := r.runErr
	hardFailure := r.hardFailure
	preStatus := remotePreparationStatus{}
	preFailures := r.preFailures
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
			preEval := stepExpressionContext(eval)
			preCtx, cancelPre := stepContext(runCtx, step.TimeoutMinutes)
			bindHashFilesContext(preCtx, &preEval)
			wasUnsuccessful := preStatus.unsuccessful
			preResult, preErr := r.prepareRemoteAction(preCtx, processor, workspace, step, strconv.Itoa(stepIndex), preEnv, preEval, posts, actions, prepared, &preStatus, true, nil, nil, nil)
			commitResultEnvironment(jobResult.Env, preResult)
			mergeInto(jobResult.State, preResult.State)
			appendJobSummary(&jobResult.Summary, &jobResult.summaryTruncated, preResult.Summary, preResult.summaryTruncated)
			eval.Env = jobResult.Env
			if preErr != nil {
				failureEval := preEval
				if stepEnv, envErr := evaluateStepMap(step.Env, preEval); envErr == nil {
					failureEval.Env = mergeStringMaps(failureEval.Env, stepEnv)
				}
				execution := classifyStepExecutionWithControls(ctx, preCtx, step, newResult(), fmt.Errorf("action %q pre: %w", step.Uses, preErr), failureEval)
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
	r.eval = eval
	r.result = jobResult
	r.runErr = runErr
	r.hardFailure = hardFailure
	return r.runSteps(ctx, runCtx)
}

func (r *jobRun) runSteps(ctx, runCtx context.Context) (JobResult, error) {
	job := r.job
	workspace := r.workspace
	processor := r.processor
	eval := r.eval
	jobResult := r.result
	runtimeEnv := r.runtimeEnv
	actions := r.actions
	posts := r.posts
	supervisor := r.supervisor
	prepared := r.prepared
	preFailures := r.preFailures
	runErr := r.runErr
	for stepIndex, step := range job.Steps {
		eval.JobStatus = jobStatusValue(runErr != nil, runCtx.Err() != nil)
		if step.Kind == "cancel" {
			for _, execution := range supervisor.cancel(step.Targets[0]) {
				targetErr := commitStepExecution(execution, &jobResult, &eval)
				if execution.conclusion != "cancelled" {
					runErr = errors.Join(runErr, targetErr)
				}
			}
			eval.Steps[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: "success", Conclusion: "success", Outputs: map[string]string{}}
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
				barrierErr = errors.Join(barrierErr, commitStepExecution(execution, &jobResult, &eval))
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
			eval.Steps[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: outcome, Conclusion: conclusion, Outputs: map[string]string{}}
			continue
		}
		if execution, ok := preFailures[stepIndex]; ok {
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
			continue
		}
		referencesStatus, err := expression.ReferencesStatusFunction(step.Condition)
		if err != nil {
			execution := classifyStepExecution(ctx, runCtx, step, newResult(), fmt.Errorf("condition: %w", err))
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
			continue
		}
		if !referencesStatus && (runErr != nil || runCtx.Err() != nil) {
			eval.Steps[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: "skipped", Conclusion: "skipped", Outputs: map[string]string{}}
			continue
		}
		evaluationCtx, cancelEvaluation := stepContext(runCtx, step.TimeoutMinutes)
		stepEval := stepExpressionContext(eval)
		bindHashFilesContext(evaluationCtx, &stepEval)
		stepEnv, err := evaluateStepMap(step.Env, stepEval)
		if err != nil {
			execution := classifyStepExecutionWithControls(ctx, evaluationCtx, step, newResult(), fmt.Errorf("environment: %w", err), stepEval)
			cancelEvaluation()
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
			continue
		}
		stepEval.Env = mergeStringMaps(stepEval.Env, stepEnv)
		condition := expression.ConditionContext{Inputs: job.Inputs, Needs: eval.Needs, Steps: eval.Steps, Env: stepEval.Env, Vars: job.Vars, Matrix: job.Matrix, GitHub: eval.GitHub, Runner: eval.Runner, Services: eval.Services, Failure: runErr != nil && runCtx.Err() == nil, Unsuccessful: runErr != nil, Cancelled: evaluationCtx.Err() != nil, HashFiles: stepEval.HashFiles}
		run, err := expression.EvaluateCondition(step.Condition, condition)
		if err != nil {
			execution := classifyStepExecutionWithControls(ctx, evaluationCtx, step, newResult(), fmt.Errorf("condition: %w", err), stepEval)
			cancelEvaluation()
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
			continue
		}
		if !run {
			cancelEvaluation()
			eval.Steps[strings.ToLower(step.ID)] = expression.StepStatus{Outcome: "skipped", Conclusion: "skipped", Outputs: map[string]string{}}
			continue
		}
		step, err = evaluateStepTimeout(step, stepEval)
		if err != nil {
			execution := classifyStepExecutionWithControls(ctx, evaluationCtx, step, newResult(), fmt.Errorf("controls: %w", err), stepEval)
			cancelEvaluation()
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
			continue
		}
		cancelEvaluation()
		stepCtx, cancelStep := stepContext(runCtx, step.TimeoutMinutes)
		bindHashFilesContext(stepCtx, &stepEval)
		displayName, err := stepDisplayName(step, stepEval)
		if err != nil {
			execution := classifyStepExecutionWithControls(ctx, stepCtx, step, newResult(), fmt.Errorf("name: %w", err), stepEval)
			cancelStep()
			runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
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
					return r.executePlanStep(runCtx, stepCtx, processor, workspace, job, step, strconv.Itoa(stepIndex), jobEnv, stepEnv, executionEval, posts, actions, prepared)
				},
				func(stepCtx context.Context) stepExecution {
					return cancelledStepExecution(runCtx, stepCtx, step)
				},
			)
			continue
		}
		processor.logSection(displayName)
		execution := r.executePlanStep(runCtx, stepCtx, processor, workspace, job, step, strconv.Itoa(stepIndex), jobEnv, stepEnv, evalSnapshot, posts, actions, prepared)
		cancelStep()
		if execution.outcome == "failure" {
			processor.expandCurrentSection()
			jobResult.failureVisible = true
		}
		runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
	}
	for _, execution := range supervisor.waitAll() {
		runErr = errors.Join(runErr, commitStepExecution(execution, &jobResult, &eval))
	}
	if runCtx.Err() != nil {
		runErr = errors.Join(runErr, runCtx.Err())
	}
	r.eval = eval
	r.result = jobResult
	r.runErr = runErr
	return r.runPostActions(runCtx)
}

func (r *jobRun) runPostActions(runCtx context.Context) (JobResult, error) {
	jobResult := r.result
	processor := r.processor
	workspace := r.workspace
	eval := r.eval
	runErr := r.runErr
	postCtx, cancelPosts := postPhaseContext(runCtx, r.postActionTimeout(), r.cleanupTimeout())
	defer cancelPosts()
	registeredPosts := r.posts.snapshot()
	for i := len(registeredPosts) - 1; i >= 0; i-- {
		post := registeredPosts[i]
		invocation := post.invocation
		action := invocation.action
		postEval := cloneExpressionContext(invocation.eval)
		if !invocation.isolated {
			postEval.Steps = cloneStepStatuses(eval.Steps)
		}
		postEval.Env = mergeStringMaps(jobResult.Env, invocation.envOverlay)
		bindHashFilesContext(postCtx, &postEval)
		unsuccessful, cancelled := runErr != nil, runCtx.Err() != nil
		condition := lifecycleConditionContext(postEval, unsuccessful, cancelled)
		runPost, conditionErr := expression.EvaluateActionLifecycleCondition(post.condition, condition)
		if conditionErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("post action %q condition: %w", action.Name, conditionErr))
			continue
		}
		if !runPost {
			continue
		}
		// The post process uses the same live final environment and declared
		// invocation overlays as its condition, not the main-time snapshot.
		action.Env = cloneStrings(invocation.envOverlay)
		for name, value := range invocation.action.Env {
			if isRuntimeContextEnvironment(name) {
				action.Env[name] = value
			}
		}
		if len(action.jobStatusInputs) != 0 {
			action.Inputs = cloneStrings(action.Inputs)
			status := jobStatusValue(runErr != nil, runCtx.Err() != nil)
			for _, name := range action.jobStatusInputs {
				action.Inputs[name] = status
			}
		}
		postResult := newResult()
		postResult.Env = cloneStrings(jobResult.Env)
		postErr := r.runJavaScriptPhase(postCtx, processor, workspace, invocation.node, action, action.Post, invocation.state, invocation.state, &postResult)
		mergeInto(jobResult.Env, postResult.Env)
		mergeInto(jobResult.State, postResult.State)
		appendJobSummary(&jobResult.Summary, &jobResult.summaryTruncated, postResult.Summary, postResult.summaryTruncated)
		if postErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("post action %q: %w", action.Name, postErr))
		}
	}
	r.eval = eval
	r.result = jobResult
	r.runErr = runErr
	return r.finalize(runCtx)
}

func (r *jobRun) finalize(runCtx context.Context) (JobResult, error) {
	job := r.job
	processor := r.processor
	eval := r.eval
	jobResult := r.result
	runErr := r.runErr
	hardFailure := r.hardFailure
	r.node16Warnings.emit(processor)
	jobResult.WarningAnnotations, jobResult.warningsTruncated, jobResult.ErrorAnnotations, jobResult.errorsTruncated = processor.workflowCommandAnnotations()
	sensitiveValues := processor.maskValues()
	runErr = processor.scrubError(runErr)
	for _, artifact := range jobResult.Artifacts {
		for _, sensitive := range sensitiveValues {
			if sensitive != "" && strings.Contains(artifact.Name, sensitive) {
				jobResult.Artifacts = nil
				return scrubJobResult(jobResult, sensitiveValues), errors.Join(runErr, fmt.Errorf("artifact name contains a registered secret"))
			}
		}
	}
	eval.Env = jobResult.Env
	for _, name := range sortedKeys(job.Outputs) {
		template := job.Outputs[name]
		value, err := expression.EvaluateJobOutput(template, eval)
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
	switch {
	case runCtx.Err() != nil:
		jobResult.Conclusion = "cancelled"
	case runErr == nil:
		jobResult.Conclusion = "success"
	case job.ContinueOnError && !hardFailure && !isHardJobFailure(runErr):
		jobResult.Conclusion = "success"
		runErr = &toleratedJobFailure{err: runErr}
	}
	return scrubJobResult(jobResult, sensitiveValues), runErr
}

func evaluateCallGuards(job plan.Job) (bool, error) {
	github := githubContext(job)
	for i, guard := range job.CallGuards {
		if len(guard.DeferredInputs) != 0 && !deferredInputsHydrated(guard.DeferredInputs, guard.DeferredInputValues) {
			return false, fmt.Errorf("evaluate reusable-workflow call guard %d: deferred inputs were not hydrated", i+1)
		}
		if len(guard.NeedSources) != 0 && len(guard.Needs) == 0 {
			return false, fmt.Errorf("evaluate reusable-workflow call guard %d: prerequisite results are missing", i+1)
		}
		condition := expression.ConditionContext{Inputs: mergeWorkflowInputs(guard.Inputs, guard.DeferredInputValues), Needs: needStatuses(guard.Needs), Vars: job.Vars, GitHub: github}
		for name, need := range guard.Needs {
			if need.Result == "" {
				return false, fmt.Errorf("evaluate reusable-workflow call guard %d: prerequisite result %q is missing", i+1, name)
			}
			switch need.Result {
			case "success", "failure", "cancelled", "skipped":
			default:
				return false, fmt.Errorf("evaluate reusable-workflow call guard %d: prerequisite %q has invalid result %q", i+1, name, need.Result)
			}
			condition.Failure = condition.Failure || need.Result == "failure"
			condition.Cancelled = condition.Cancelled || need.Result == "cancelled"
			condition.Unsuccessful = condition.Unsuccessful || need.Result != "success"
		}
		run, err := expression.EvaluateCondition(guard.Condition, condition)
		if err != nil {
			return false, fmt.Errorf("evaluate reusable-workflow call guard %d: %w", i+1, err)
		}
		if !run {
			return false, nil
		}
	}
	return true, nil
}

func deferredInputsHydrated(deferred map[string]plan.DeferredInput, values map[string]any) bool {
	if len(deferred) != len(values) {
		return false
	}
	for name := range deferred {
		if _, ok := values[name]; !ok {
			return false
		}
	}
	return true
}

func mergeWorkflowInputs(inputs, deferred map[string]any) map[string]any {
	merged := maps.Clone(inputs)
	if merged == nil && len(deferred) != 0 {
		merged = make(map[string]any, len(deferred))
	}
	maps.Copy(merged, deferred)
	return merged
}

func evaluateServices(services map[string]plan.ServiceContainer, eval expression.Context) (map[string]plan.ServiceContainer, error) {
	if len(services) == 0 {
		return services, nil
	}
	result := make(map[string]plan.ServiceContainer, len(services))
	for name, service := range services {
		service.Env = maps.Clone(service.Env)
		service.Ports = append([]string(nil), service.Ports...)
		service.Volumes = append([]string(nil), service.Volumes...)
		for fieldName, field := range map[string]*string{"image": &service.Image, "options": &service.Options, "command": &service.Command, "entrypoint": &service.Entrypoint} {
			value, err := expression.Evaluate(*field, eval)
			if err != nil {
				return nil, fmt.Errorf("service %q %s: %w", name, fieldName, err)
			}
			*field = value
		}
		for key, value := range service.Env {
			resolved, err := expression.Evaluate(value, eval)
			if err != nil {
				return nil, fmt.Errorf("service %q environment %q: %w", name, key, err)
			}
			service.Env[key] = resolved
		}
		for _, values := range [][]string{service.Ports, service.Volumes} {
			for i, value := range values {
				resolved, err := expression.Evaluate(value, eval)
				if err != nil {
					return nil, fmt.Errorf("service %q field: %w", name, err)
				}
				values[i] = resolved
			}
		}
		if service.Credentials != nil {
			credentials := *service.Credentials
			var err error
			credentials.Username, err = expression.Evaluate(credentials.Username, eval)
			if err != nil {
				return nil, fmt.Errorf("service %q username: %w", name, err)
			}
			credentials.Password, err = expression.Evaluate(credentials.Password, eval)
			if err != nil {
				return nil, fmt.Errorf("service %q password: %w", name, err)
			}
			service.Credentials = &credentials
		}
		if service.Image == "" {
			continue
		}
		if err := plan.ValidateEvaluatedServiceContainer(service); err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		result[name] = service
	}
	return result, nil
}

func serviceOrder(services map[string]plan.ServiceContainer, preferred []string) []string {
	result := make([]string, 0, len(services))
	seen := make(map[string]bool, len(services))
	for _, name := range preferred {
		if _, ok := services[name]; ok && !seen[name] {
			result = append(result, name)
			seen[name] = true
		}
	}
	for _, name := range sortedKeys(services) {
		if !seen[name] {
			result = append(result, name)
		}
	}
	return result
}

func evaluateServiceMap(static map[string]plan.ServiceContainer, staticOrder []string, source string, eval expression.Context) (map[string]plan.ServiceContainer, []string, error) {
	if source == "" {
		services, err := evaluateServices(static, eval)
		return services, serviceOrder(services, staticOrder), err
	}
	entries, err := expression.EvaluateObject(source, eval)
	if err != nil {
		return nil, nil, err
	}
	if len(entries) > 32 {
		return nil, nil, fmt.Errorf("services expression has more than 32 entries")
	}
	services := make(map[string]plan.ServiceContainer, len(entries))
	order := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, raw := entry.Name, entry.Value
		if !plan.ValidateServiceName(name) {
			return nil, nil, fmt.Errorf("service name %q must be lowercase and valid", name)
		}
		var service plan.ServiceContainer
		if image, ok := raw.(string); ok {
			service.Image = image
		} else {
			if err := normalizeServiceScalars(raw); err != nil {
				return nil, nil, fmt.Errorf("decode service %q: %w", name, err)
			}
			encoded, err := json.Marshal(raw)
			if err != nil {
				return nil, nil, fmt.Errorf("encode service %q: %w", name, err)
			}
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&service); err != nil {
				return nil, nil, fmt.Errorf("decode service %q: %w", name, err)
			}
		}
		validationService := service
		if validationService.Image == "" {
			validationService.Image = "scratch"
		}
		if err := plan.ValidateEvaluatedServiceContainer(validationService); err != nil {
			return nil, nil, fmt.Errorf("service %q: %w", name, err)
		}
		if service.Image == "" {
			continue
		}
		services[name] = service
		order = append(order, name)
	}
	return services, order, nil
}

func normalizeServiceScalars(raw any) error {
	service, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("got %T, want an object", raw)
	}
	for field := range service {
		switch field {
		case "credentials":
			return errors.New("expression cannot introduce registry credentials")
		case "image", "env", "ports", "volumes", "options", "command", "entrypoint":
		default:
			return fmt.Errorf("unknown field %q", field)
		}
	}
	for _, field := range []string{"image", "options", "command", "entrypoint"} {
		if value, exists := service[field]; exists {
			normalized, err := serviceScalarString(value)
			if err != nil {
				return fmt.Errorf("field %q: %w", field, err)
			}
			service[field] = normalized
		}
	}
	if rawEnv, exists := service["env"]; exists {
		env, ok := rawEnv.(map[string]any)
		if !ok {
			return fmt.Errorf("field %q: got %T, want an object", "env", rawEnv)
		}
		for key, value := range env {
			normalized, err := serviceScalarString(value)
			if err != nil {
				return fmt.Errorf("environment %q: %w", key, err)
			}
			env[key] = normalized
		}
	}
	for _, field := range []string{"ports", "volumes"} {
		if rawValues, exists := service[field]; exists {
			values, ok := rawValues.([]any)
			if !ok {
				return fmt.Errorf("field %q: got %T, want an array", field, rawValues)
			}
			for i, value := range values {
				normalized, err := serviceScalarString(value)
				if err != nil {
					return fmt.Errorf("field %q entry %d: %w", field, i, err)
				}
				values[i] = normalized
			}
		}
	}
	return nil
}

func serviceScalarString(value any) (string, error) {
	switch value := value.(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	case bool:
		return strconv.FormatBool(value), nil
	case json.Number:
		number, err := strconv.ParseFloat(value.String(), 64)
		if err != nil {
			return "", fmt.Errorf("invalid number %q", value)
		}
		if number == 0 {
			return "0", nil
		}
		return strconv.FormatFloat(number, 'G', 15, 64), nil
	default:
		return "", fmt.Errorf("got %T, want a scalar", value)
	}
}

func stepDisplayName(step plan.Step, eval expression.Context) (string, error) {
	if step.Name != "" {
		return expression.EvaluateStep(step.Name, eval)
	}
	if step.Uses != "" {
		return step.Uses, nil
	}
	return step.ID, nil
}

func stepExpressionContext(context expression.Context) expression.Context {
	context = cloneExpressionContext(context)
	if token, ok := context.Secrets["GITHUB_TOKEN"]; ok {
		context.GitHub["token"] = token
	}
	return context
}

func lifecycleConditionContext(eval expression.Context, unsuccessful, cancelled bool) expression.ConditionContext {
	return expression.ConditionContext{
		Inputs:       eval.WorkflowInputs,
		Steps:        eval.Steps,
		Env:          eval.Env,
		Matrix:       eval.Matrix,
		GitHub:       eval.GitHub,
		Runner:       eval.Runner,
		Services:     eval.Services,
		Failure:      unsuccessful && !cancelled,
		Unsuccessful: unsuccessful,
		Cancelled:    cancelled,
		HashFiles:    eval.HashFiles,
	}
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
			return nil, &SecretResolutionError{Name: name, Err: err}
		}
		if value != "" {
			processor.addMask(value)
			if err := r.Redactor.AddRedaction(ctx, value); err != nil {
				return nil, processor.scrubError(err)
			}
		}
		values[name] = value
	}
	return values, nil
}

func (r Runner) resolveWorkflowToken(ctx context.Context, processor *commandProcessor, repository, workflow string, permissions map[string]string) (string, error) {
	if r.WorkflowToken == nil {
		return "", fmt.Errorf("GitHub workflow token provider is not configured")
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

func evaluateStepControls(step plan.Step, context expression.Context) (plan.Step, error) {
	var err error
	step, err = evaluateStepContinueOnError(step, context)
	if err != nil {
		return step, err
	}
	return evaluateStepTimeout(step, context)
}

func evaluateStepContinueOnError(step plan.Step, context expression.Context) (plan.Step, error) {
	if step.ContinueOnErrorExpression != "" {
		value, err := expression.EvaluateStepControl(step.ContinueOnErrorExpression, context)
		if err != nil {
			return step, fmt.Errorf("continue-on-error: %w", err)
		}
		continueOnError, ok := value.(bool)
		if !ok {
			return step, fmt.Errorf("continue-on-error expression produced %T, want boolean", value)
		}
		step.ContinueOnError = continueOnError
	}
	return step, nil
}

func evaluateStepTimeout(step plan.Step, context expression.Context) (plan.Step, error) {
	if step.TimeoutMinutesExpression != "" {
		value, err := expression.EvaluateStepControl(step.TimeoutMinutesExpression, context)
		if err != nil {
			return step, fmt.Errorf("timeout-minutes: %w", err)
		}
		switch value := value.(type) {
		case int:
			step.TimeoutMinutes = float64(value)
		case int8:
			step.TimeoutMinutes = float64(value)
		case int16:
			step.TimeoutMinutes = float64(value)
		case int32:
			step.TimeoutMinutes = float64(value)
		case int64:
			step.TimeoutMinutes = float64(value)
		case uint:
			step.TimeoutMinutes = float64(value)
		case uint8:
			step.TimeoutMinutes = float64(value)
		case uint16:
			step.TimeoutMinutes = float64(value)
		case uint32:
			step.TimeoutMinutes = float64(value)
		case uint64:
			step.TimeoutMinutes = float64(value)
		case float32:
			step.TimeoutMinutes = float64(value)
		case float64:
			step.TimeoutMinutes = value
		case json.Number:
			step.TimeoutMinutes, err = value.Float64()
			if err != nil {
				return step, fmt.Errorf("timeout-minutes expression produced invalid number %q", value)
			}
		default:
			return step, fmt.Errorf("timeout-minutes expression produced %T, want number", value)
		}
		if math.IsNaN(step.TimeoutMinutes) || math.IsInf(step.TimeoutMinutes, 0) || step.TimeoutMinutes <= 0 || step.TimeoutMinutes > 360 {
			return step, fmt.Errorf("timeout-minutes expression must produce a number greater than 0 and at most 360")
		}
	}
	return step, nil
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
		"action_path":       "",
		"action_ref":        "",
		"action_repository": "",
		"repository":        job.Event.Repository,
		"repository_owner":  plan.EventRepositoryOwner(job.Event.Repository),
		"ref":               job.Event.Ref,
		"ref_name":          plan.EventRefName(job.Event.Ref),
		"ref_type":          plan.EventRefType(job.Event.Ref),
		"head_ref":          job.Event.HeadRef,
		"base_ref":          job.Event.BaseRef,
		"sha":               job.Event.SHA,
		"actor":             job.Event.Actor,
		"event_name":        job.Event.Name,
		"server_url":        plan.EventServerURL(job.Event.Provider),
		"workflow":          workflowDisplayName(job),
		"job":               job.Workflow.LogicalJobID,
	}
}

func workflowDisplayName(job plan.Job) string {
	if job.Workflow.Name != "" {
		return job.Workflow.Name
	}
	return job.Workflow.Path
}

func standardEnvironment(job plan.Job, workspace, runnerTemp, toolCache string) map[string]string {
	runner, _ := canonicalRunnerContext(goruntime.GOOS, goruntime.GOARCH)
	workflowName := workflowDisplayName(job)
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
		"GITHUB_WORKFLOW":   workflowName,
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

func (r *jobRun) invocationEnvironment(jobEnv, stepEnv map[string]string) invocationEnvironment {
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
		"GITHUB_WORKFLOW",
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

func (r *jobRun) runJobStep(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv, stepEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations) (Result, error) {
	return r.runActionStep(ctx, processor, workspace, job, step, invocationID, jobEnv, stepEnv, nil, eval, posts, actions, prepared, nil, nil)
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
	if slices.Contains(stack, lock.ID) {
		return fmt.Errorf("action recursion detected at lock %q", lock.ID)
	}
	if usesNativeAdapter(lock) {
		return nil
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
			return markHardJobFailure(fmt.Errorf("composite action step %d child %q has no immutable selector", i+1, child.Uses))
		}
		if err := r.verifyRemoteActionTree(ctx, actions, childSelector, stack); err != nil {
			return err
		}
	}
	return nil
}

func (r *jobRun) actionContainerMounts(ctx context.Context, actions *actionLockResolver) ([]containerMount, error) {
	byTarget := map[string]containerMount{}
	requiredNode := map[int]bool{}
	unknownWorkspaceRuntime := false
	for _, lock := range actions.job.Actions {
		// A native adapter replaces the admitted action's execution entirely,
		// so its source tree is never mounted or classified for the container.
		if usesNativeAdapter(lock) {
			continue
		}
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

func (r *jobRun) prepareRemoteAction(ctx context.Context, processor *commandProcessor, workspace string, step plan.Step, invocationID string, jobEnv map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, status *remotePreparationStatus, workflowStep bool, inheritedEvalErr error, inheritedTimeout *remotePreparationTimeout, inheritedEnvOverlay map[string]string) (Result, error) {
	result := newResult()
	eval.JobStatus = jobStatusValue(status.unsuccessful, ctx.Err() != nil)
	evaluate := evaluateStepMap
	if !workflowStep {
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
	// The native adapters replace the verified action's lifecycle as one
	// indivisible operation, so upstream metadata never classifies and no
	// upstream cleanup is registered for phases this runtime never executes.
	if usesCheckoutAdapter(lock) || usesUploadArtifactAdapter(lock) || usesDownloadArtifactAdapter(lock) {
		return result, nil
	}
	runtime, err := action.Runtime()
	if err != nil {
		return result, fmt.Errorf("action %q uses %w", step.Uses, err)
	}
	if err := action.ValidateEntrypoints(runtime); err != nil {
		return result, fmt.Errorf("action %q: %w", step.Uses, err)
	}

	switch runtime {
	case metadata.RuntimeNode16, metadata.RuntimeNode24:
		if err := expression.ValidateActionLifecycleCondition(action.Runs.PreIf); err != nil {
			return result, fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err)
		}
		if err := expression.ValidateActionLifecycleCondition(action.Runs.PostIf); err != nil {
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
		javascript := javaScriptAction{Name: actionName(action, step), Path: action.Path, Pre: action.Runs.Pre, Main: action.Runs.Main, Post: action.Runs.Post, Cache: usesCacheService(lock), CacheClientCompatibility: usesCacheClientCompatibility(lock), nodeMajor: major, reference: step.Uses, jobStatusInputs: jobStatusInputs}
		invocationEval := cloneExpressionContext(eval)
		bindHashFilesContext(ctx, &invocationEval)
		invocation := &preparedInvocation{action: javascript, state: map[string]string{}, eval: invocationEval, isolated: !workflowStep}
		prepared[invocationID] = invocation
		if javascript.Pre != "" {
			failPre := func(err error) (Result, error) {
				invocation.preFailure = err
				return result, err
			}
			stepEnv := map[string]string{}
			if inheritedEvalErr == nil {
				stepEnv, err = evaluate(step.Env, eval)
				if err != nil {
					return failPre(err)
				}
				invocationEval.Env = mergeStringMaps(invocationEval.Env, stepEnv)
			}
			condition := lifecycleConditionContext(invocationEval, status.unsuccessful, ctx.Err() != nil)
			runPre, err := expression.EvaluateActionLifecycleCondition(action.Runs.PreIf, condition)
			if err != nil {
				return failPre(fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err))
			}
			if !runPre {
				return result, nil
			}
			if inheritedEvalErr != nil {
				return failPre(inheritedEvalErr)
			}
			eval.Env = mergeStringMaps(eval.Env, stepEnv)
			phaseCtx, cancelPhase := context.WithCancel(ctx)
			defer func() { cancelPhase() }()
			if workflowStep {
				resolvedStep, err := evaluateStepTimeout(step, eval)
				if err != nil {
					return failPre(fmt.Errorf("controls: %w", err))
				}
				cancelPhase()
				phaseCtx, cancelPhase = stepContext(ctx, resolvedStep.TimeoutMinutes)
			} else if inheritedTimeout != nil {
				cancelPhase()
				phaseCtx, err = inheritedTimeout.context(ctx)
				cancelPhase = func() {}
				if err != nil {
					return failPre(err)
				}
			}
			inputs, err := evaluate(step.With, eval)
			if err != nil {
				return failPre(err)
			}
			inputs, err = resolveActionInputs(action, inputs, eval)
			if err != nil {
				return failPre(err)
			}
			javascript.Inputs = inputs
			javascript.Env = mergeStepEnvironment(jobEnv, stepEnv)
			invocation.action = javascript
			invocation.eval = invocationEval
			invocation.envOverlay = mergeStringMaps(inheritedEnvOverlay, stepEnv)
			node, err := r.discoverNode(phaseCtx, major, explicit)
			if err != nil {
				return failPre(err)
			}
			invocation.node = node
			posts.register(postForInvocation(invocation, action.Runs.PostIf))
			invocation.postRegistered = true
			if err := r.runJavaScriptPhase(phaseCtx, processor, workspace, node, javascript, javascript.Pre, nil, invocation.state, &result); err != nil {
				return failPre(err)
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
		preparationTimeout := inheritedTimeout
		if workflowStep && compositeEvalErr == nil {
			preparationTimeout = &remotePreparationTimeout{step: step, eval: eval}
			defer preparationTimeout.close()
		}
		bindActionReferenceContext(&eval, &lock)
		eval.Inputs = inputs
		// github.action_path scopes to this composite invocation for child pre
		// hooks too, mirroring the main-phase overlay in runCompositeMetadata.
		contextActionPath := action.Path
		if r.jobContainer != nil {
			contextActionPath = r.jobContainer.containerPath(action.Path)
		}
		eval.GitHub = cloneAnyMap(eval.GitHub)
		eval.GitHub["action_path"] = contextActionPath
		compositeProcessEnv := mergeStepEnvironment(jobEnv, stepEnv)
		compositeExpressionEnv := mergeStringMaps(eval.Env, stepEnv)
		lifecycleEnvOverlay := mergeStringMaps(inheritedEnvOverlay, stepEnv)
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
			wasUnsuccessful := status.unsuccessful
			childResult, childErr := r.prepareRemoteAction(ctx, processor, workspace, child, fmt.Sprintf("%s/%d", invocationID, i), childProcessEnv, eval, posts, actions, prepared, status, false, compositeEvalErr, preparationTimeout, lifecycleEnvOverlay)
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
				classificationCtx, cancelClassification := context.WithCancel(ctx)
				if preparationTimeout != nil && preparationTimeout.bounded != nil {
					// contextcheck cannot trace the cached timeout context back to ctx.
					// Classification only needs its cancellation state.
					if preparationTimeout.bounded.Err() != nil {
						cancelClassification()
					}
				}
				execution := classifyStepExecution(classificationCtx, classificationCtx, plan.Step{ContinueOnError: childStep.ContinueOnError}, childResult, childErr)
				cancelClassification()
				if execution.conclusion != "success" {
					status.unsuccessful = true
					err = errors.Join(err, fmt.Errorf("composite action step %d: %w", i+1, childErr))
				} else {
					status.unsuccessful = wasUnsuccessful
				}
			}
		}
		return result, err
	default:
		return result, nil
	}
}

func (r *jobRun) runActionStep(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, invocationID string, jobEnv, stepEnv, evaluatedWith map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, actionStack []string, inheritedEnvOverlay map[string]string) (Result, error) {
	if stepEnv == nil {
		var err error
		stepEnv, err = evaluateStepMap(step.Env, eval)
		if err != nil {
			return newResult(), err
		}
	}
	lifecycleEnvOverlay := mergeStringMaps(inheritedEnvOverlay, stepEnv)
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
			shell, err = expression.EvaluateJobDefault(shell, eval)
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
			workingDirectory, err = expression.EvaluateJobDefault(workingDirectory, eval)
		} else {
			workingDirectory, err = expression.EvaluateStep(workingDirectory, eval)
		}
		if err != nil {
			return result, err
		}
		dir, err := stepWorkingDirectory(workspace, workingDirectory)
		if err != nil {
			return result, err
		}
		runEnv := environment.process()
		err = r.runShellProcess(ctx, processor, dir, runEnv, &result, shell, script)
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
			inputs := evaluatedWith
			if inputs == nil {
				inputs, err = evaluateStepMap(step.With, eval)
				if err != nil {
					return result, err
				}
			}
			if err := validateCheckoutRefProvenance(step.With, inputs, job.Event.SHA); err != nil {
				return result, err
			}
			return r.runCheckout(ctx, processor, workspace, job, lock.Commit, inputs)
		}
		if usesUploadArtifactAdapter(lock) {
			inputs := evaluatedWith
			if inputs == nil {
				inputs, err = evaluateStepMap(step.With, eval)
				if err != nil {
					return result, err
				}
			}
			return r.runUploadArtifactCommit(ctx, processor, workspace, lock.Commit, inputs)
		}
		if usesDownloadArtifactAdapter(lock) {
			inputs := evaluatedWith
			if inputs == nil {
				inputs, err = evaluateStepMap(step.With, eval)
				if err != nil {
					return result, err
				}
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
	if slices.Contains(actionStack, actionIdentity) {
		return result, fmt.Errorf("action recursion detected at %q", step.Uses)
	}
	if len(actionStack) >= metadata.MaxNestedActionDepth {
		return result, fmt.Errorf("local action nesting exceeds maximum depth %d at %q", metadata.MaxNestedActionDepth, actionPath)
	}
	actionStack = append(append([]string(nil), actionStack...), actionIdentity)
	inputs := evaluatedWith
	if inputs == nil {
		inputs, err = evaluateStepMap(step.With, eval)
		if err != nil {
			return result, err
		}
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
	case metadata.RuntimeNode16, metadata.RuntimeNode24:
		if action.Runs.Main == "" {
			return result, fmt.Errorf("JavaScript action %q has no main entry point", step.Uses)
		}
		if err := expression.ValidateActionLifecycleCondition(action.Runs.PreIf); err != nil {
			return result, fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err)
		}
		if err := expression.ValidateActionLifecycleCondition(action.Runs.PostIf); err != nil {
			return result, fmt.Errorf("JavaScript action %q post-if: %w", step.Uses, err)
		}
		major, _ := actionNodeMajor(actionRuntime)
		explicit := r.explicitNode(major)
		node, err := r.discoverNode(ctx, major, explicit)
		if err != nil {
			return result, err
		}
		actionEnv := environment.process()
		javascript := javaScriptAction{Name: actionName(action, step), Path: actionPath, Pre: action.Runs.Pre, Main: action.Runs.Main, Post: action.Runs.Post, Inputs: inputs, Env: actionEnv, Cache: actionLock != nil && usesCacheService(*actionLock), CacheClientCompatibility: actionLock != nil && usesCacheClientCompatibility(*actionLock), nodeMajor: major, reference: step.Uses, jobStatusInputs: jobStatusInputs}
		state := map[string]string{}
		wasPrepared := false
		invocation := prepared[invocationID]
		if invocation != nil {
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
		lifecycleEval := cloneExpressionContext(eval)
		bindHashFilesContext(ctx, &lifecycleEval)
		lifecycleEval.Env = mergeStringMaps(lifecycleEval.Env, stepEnv)
		isolated := len(actionStack) > 1
		if invocation != nil {
			isolated = invocation.isolated
		}
		if isolated {
			// Composite steps execute sequentially and own a per-invocation
			// map. Retain that map so post-if sees its final state without
			// exposing workflow or sibling-composite steps.
			lifecycleEval.Steps = eval.Steps
		}
		if invocation != nil && invocation.preFailure != nil {
			invocation.eval = lifecycleEval
			invocation.envOverlay = lifecycleEnvOverlay
			bindCompositeInvocationSteps(invocation, eval.Steps)
			return result, invocation.preFailure
		}
		if invocation == nil {
			invocation = &preparedInvocation{action: javascript, state: state, node: node, eval: lifecycleEval, envOverlay: lifecycleEnvOverlay, isolated: isolated}
		}
		if !invocation.postRegistered {
			posts.register(postForInvocation(invocation, action.Runs.PostIf))
			invocation.postRegistered = true
		}
		if javascript.Pre != "" && !wasPrepared {
			cancelled := eval.JobStatus == "cancelled"
			unsuccessful := eval.JobStatus == "failure" || cancelled
			condition := lifecycleConditionContext(lifecycleEval, unsuccessful, cancelled)
			runPre, err := expression.EvaluateActionLifecycleCondition(action.Runs.PreIf, condition)
			if err != nil {
				return result, fmt.Errorf("JavaScript action %q pre-if: %w", step.Uses, err)
			}
			if runPre {
				if err := r.runJavaScriptPhase(ctx, processor, workspace, node, javascript, javascript.Pre, nil, state, &result); err != nil {
					return result, err
				}
			}
		}
		mainErr := r.runJavaScriptPhase(ctx, processor, workspace, node, javascript, javascript.Main, nil, state, &result)
		invocation.action = javascript
		invocation.node = node
		invocation.eval = lifecycleEval
		invocation.envOverlay = lifecycleEnvOverlay
		if mainErr != nil {
			return result, mainErr
		}
		return result, nil
	case metadata.RuntimeComposite:
		composite, err := r.runCompositeMetadata(ctx, processor, workspace, job, actionPath, action, inputs, invocationID, jobEnv, stepEnv, lifecycleEnvOverlay, actionEval, posts, actions, prepared, actionLock, actionStack)
		return composite, err
	case metadata.RuntimeDocker:
		if goruntime.GOOS == "darwin" {
			return result, fmt.Errorf("docker action %q is unsupported on macOS runners", step.Uses)
		}
		if !job.HasCapability("docker") {
			return result, fmt.Errorf("docker action %q requires the plan's docker capability", step.Uses)
		}
		if err := action.ValidateEntrypoints(actionRuntime); err != nil {
			return result, fmt.Errorf("docker action %q: %w", step.Uses, err)
		}
		dockerArgs := make([]string, len(action.Runs.Args))
		for i, argument := range action.Runs.Args {
			dockerArgs[i], err = expression.EvaluateDockerActionArg(argument, inputs)
			if err != nil {
				return result, fmt.Errorf("docker action %q argument %d: %w", step.Uses, i+1, err)
			}
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
		image, _ := metadata.DockerImageReference(action.Runs.Image)
		if image != "" && actionLock != nil && image != actionLock.DockerImage {
			return result, fmt.Errorf("docker action %q metadata image %q does not match planned image %q", step.Uses, image, actionLock.DockerImage)
		}
		result, err := r.runDocker(ctx, processor, dockerAction{Name: actionName(action, step), Path: actionPath, SourceRoot: sourceRoot, SourceDigest: sourceDigest, Image: image, Entrypoint: action.Runs.Entrypoint, Args: dockerArgs, Workspace: workspace, Env: invocationEnv, explicitPATH: explicitPATH})
		return result, err
	}
	return result, fmt.Errorf("action %q uses unsupported runtime %q", step.Uses, actionRuntime)
}

func (r *jobRun) runCompositeMetadata(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, actionPath string, action metadata.Metadata, inputs map[string]string, invocationID string, jobEnv, stepEnv, lifecycleEnvOverlay map[string]string, eval expression.Context, posts *postRegistry, actions *actionLockResolver, prepared remotePreparations, actionLock *plan.ActionLock, actionStack []string) (Result, error) {
	result := newResult()
	// Keep hashFiles unavailable to composite step metadata while retaining the
	// context binder for nested JavaScript lifecycle conditions.
	eval.HashFiles = nil
	eval.Inputs = inputs
	eval.Steps = make(map[string]expression.StepStatus)
	bindActionReferenceContext(&eval, actionLock)
	compositeProcessEnv := mergeStepEnvironment(jobEnv, stepEnv)
	compositeProcessEnv["GITHUB_ACTION_PATH"] = actionPath
	// github.action_path is scoped to this composite invocation; nested
	// composites overlay their own path on entry. Job-container executions
	// interpolate the mounted path because the script runs in the container.
	contextActionPath := actionPath
	if r.jobContainer != nil {
		contextActionPath = r.jobContainer.containerPath(actionPath)
	}
	eval.GitHub = cloneAnyMap(eval.GitHub)
	eval.GitHub["action_path"] = contextActionPath
	compositeExpressionEnv := mergeStringMaps(eval.Env, stepEnv)
	inheritedFailure := eval.JobStatus == "failure"
	inheritedCancelled := eval.JobStatus == "cancelled"
	inheritedUnsuccessful := inheritedFailure || inheritedCancelled
	var runErr error
	for i, step := range action.Runs.Steps {
		childInvocationID := fmt.Sprintf("%s/%d", invocationID, i)
		failure := inheritedFailure || runErr != nil
		cancelled := inheritedCancelled || ctx.Err() != nil
		unsuccessful := inheritedUnsuccessful || runErr != nil
		eval.JobStatus = jobStatusValue(unsuccessful, cancelled)
		// GITHUB_ENV effects are visible to subsequent children. Keep this
		// composite's invocation environment in expression contexts, while
		// rebuilding the map so a child's declared env cannot leak to siblings.
		eval.Env = mergeStringMaps(compositeExpressionEnv, result.Env)
		id := strings.ToLower(step.ID)
		if invocation := prepared[childInvocationID]; invocation != nil && invocation.preFailure != nil {
			childErr := invocation.preFailure
			execution := classifyStepExecution(ctx, ctx, plan.Step{ContinueOnError: step.ContinueOnError}, newResult(), childErr)
			if id != "" {
				eval.Steps[id] = expression.StepStatus{Outcome: execution.outcome, Conclusion: execution.conclusion, Outputs: map[string]string{}}
			}
			if execution.conclusion != "success" {
				runErr = errors.Join(runErr, childErr)
			}
			bindCompositeInvocationSteps(invocation, eval.Steps)
			continue
		}
		inputs := make(map[string]any, len(eval.Inputs))
		for name, value := range eval.Inputs {
			inputs[name] = value
		}
		condition := expression.ConditionContext{Inputs: inputs, Needs: eval.Needs, Steps: eval.Steps, Env: eval.Env, Vars: eval.Vars, Matrix: eval.Matrix, GitHub: eval.GitHub, Runner: eval.Runner, Services: eval.Services, Failure: failure, Unsuccessful: unsuccessful, Cancelled: cancelled}
		run, err := expression.EvaluateCondition(step.If, condition)
		if err != nil {
			childErr := fmt.Errorf("composite action step %d condition: %w", i+1, err)
			execution := classifyStepExecution(ctx, ctx, plan.Step{ContinueOnError: step.ContinueOnError}, newResult(), childErr)
			if id != "" {
				eval.Steps[id] = expression.StepStatus{Outcome: execution.outcome, Conclusion: execution.conclusion, Outputs: map[string]string{}}
			}
			if execution.conclusion != "success" {
				runErr = errors.Join(runErr, childErr)
			}
			bindCompositeInvocationSteps(prepared[childInvocationID], eval.Steps)
			continue
		}
		if !run {
			if id != "" {
				eval.Steps[id] = expression.StepStatus{Outcome: "skipped", Conclusion: "skipped", Outputs: map[string]string{}}
			}
			if invocation := prepared[childInvocationID]; invocation != nil {
				// Pre hooks register posts before composite execution. If main is
				// skipped, retain this composite's live final step scope for post-if.
				bindCompositeInvocationSteps(invocation, eval.Steps)
			}
			continue
		}
		stepResult := newResult()
		childErr := error(nil)
		childJobEnv := mergeStepEnvironment(compositeProcessEnv, result.Env)
		childJobEnv["GITHUB_ACTION_PATH"] = actionPath
		switch {
		case step.Uses != "":
			// Resolve composite child fields before entering workflow-authored
			// action evaluation.
			var childEnv map[string]string
			childEnv, childErr = evaluateStepMap(step.Env, eval)
			var childWith map[string]string
			if childErr == nil {
				childWith, childErr = evaluateStepMap(step.With, eval)
			}
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
				stepResult, childErr = r.runActionStep(ctx, processor, workspace, job, child, childInvocationID, childJobEnv, childEnv, childWith, eval, posts, actions, prepared, actionStack, lifecycleEnvOverlay)
			}
		case strings.TrimSpace(step.Run) == "":
			childErr = fmt.Errorf("composite action step %d has no run command", i+1)
		default:
			var script, dir string
			var env map[string]string
			env, childErr = evaluateStepMap(step.Env, eval)
			if childErr == nil {
				script, childErr = expression.EvaluateStep(step.Run, eval)
			}
			if childErr == nil {
				var workingDirectory string
				workingDirectory, childErr = expression.EvaluateStep(step.WorkingDirectory, eval)
				if childErr == nil {
					dir, childErr = stepWorkingDirectory(workspace, workingDirectory)
				}
			}
			if childErr == nil {
				childErr = r.runShellProcess(ctx, processor, dir, mergeStepEnvironment(childJobEnv, env), &stepResult, step.Shell, script)
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
		execution := classifyStepExecution(ctx, ctx, plan.Step{ContinueOnError: step.ContinueOnError}, stepResult, childErr)
		if id != "" {
			eval.Steps[id] = expression.StepStatus{Outcome: execution.outcome, Conclusion: execution.conclusion, Outputs: stepResult.Outputs}
		}
		if execution.conclusion != "success" {
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

func bindActionReferenceContext(eval *expression.Context, lock *plan.ActionLock) {
	eval.GitHub = cloneAnyMap(eval.GitHub)
	eval.GitHub["action_repository"] = ""
	eval.GitHub["action_ref"] = ""
	if lock != nil && lock.Source == "github" {
		eval.GitHub["action_repository"] = lock.Repository
		eval.GitHub["action_ref"] = lock.RequestedRef
	}
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

func evaluateJobEnvironmentMap(values map[string]string, context expression.Context) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, name := range sortedKeys(values) {
		resolved, err := expression.EvaluateJobEnvironment(values[name], context)
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

func needStatuses(needs map[string]plan.Need) map[string]expression.NeedStatus {
	statuses := make(map[string]expression.NeedStatus, len(needs))
	for name, need := range needs {
		var outputs map[string]string
		if need.Outputs != nil {
			outputs = cloneStrings(need.Outputs)
		}
		statuses[name] = expression.NeedStatus{Outputs: outputs, Result: need.Result}
	}
	return statuses
}

func shellCommand(shell, script string) ([]string, error) {
	switch strings.TrimSpace(shell) {
	case "", "bash":
		return []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script}, nil
	case "sh":
		return []string{"sh", "-e", "-c", script}, nil
	default:
		if err := shellcompat.ValidateCompatibility(shell); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("shell %q is unsupported in the supported runtime subset", shell)
	}
}

func (r *jobRun) runShellProcess(ctx context.Context, processor *commandProcessor, dir string, env map[string]string, result *Result, shell, script string) error {
	shell = strings.TrimSpace(shell)
	if shell != "python" && !strings.Contains(shell, "{0}") {
		args, err := shellCommand(shell, script)
		if err != nil {
			return err
		}
		return r.runProcess(ctx, processor, dir, env, result, nil, args[0], args[1:]...)
	}

	args := []string{"python", "{0}"}
	if shell != "python" {
		var err error
		args, err = shellcompat.ParseTemplate(shell)
		if err != nil {
			return err
		}
	}
	extension := shellScriptExtension(args[0])
	parent := ""
	if r.jobContainer != nil {
		parent = r.runnerTemp
	}
	file, err := os.CreateTemp(parent, "buildkite-gha-shell-*"+extension)
	if err != nil {
		return fmt.Errorf("create shell script: %w", err)
	}
	path := file.Name()
	defer func(path string) { _ = os.Remove(path) }(path)
	if _, err := file.WriteString(script); err != nil {
		return errors.Join(fmt.Errorf("write shell script: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close shell script: %w", err)
	}
	if r.jobContainer != nil {
		// Job containers may declare any USER, so the mounted script must be
		// readable without assuming a shared host UID or GID.
		if err := os.Chmod(path, 0o644); err != nil {
			return fmt.Errorf("make shell script container-readable: %w", err)
		}
		path = r.jobContainer.containerPath(path)
	}
	for i := 1; i < len(args); i++ {
		args[i] = strings.ReplaceAll(args[i], "{0}", path)
	}
	if r.jobContainer == nil {
		command, err := resolveExecutableInPath(args[0], env["PATH"])
		if err != nil {
			return err
		}
		args[0] = command
	}
	return r.runProcess(ctx, processor, dir, env, result, nil, args[0], args[1:]...)
}

func shellScriptExtension(command string) string {
	switch strings.ToLower(command) {
	case "bash", "sh":
		return ".sh"
	case "python":
		return ".py"
	default:
		return ""
	}
}

// stepWorkingDirectory resolves a run step's working directory and requires it
// to exist. Failing here names the directory instead of surfacing the child
// process's chdir failure as a misleading "fork/exec <shell>" error.
func stepWorkingDirectory(root, path string) (string, error) {
	resolved, err := workspacePath(root, path)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(resolved); statErr != nil || !info.IsDir() {
		return "", fmt.Errorf("working directory %q does not exist in the workspace", path)
	}
	return resolved, nil
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
	postCtx, cancelPosts := context.WithTimeout(context.WithoutCancel(parent), timeout)
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
	maps.Copy(target, source)
}

func mergeStringMaps(values ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		mergeInto(out, value)
	}
	return out
}
