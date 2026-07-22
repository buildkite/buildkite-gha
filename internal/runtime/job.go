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
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// JobResult is the bounded Phase 0 result returned to the transport layer.
type JobResult struct {
	Conclusion string            `json:"conclusion"`
	Outputs    map[string]string `json:"outputs,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	State      map[string]string `json:"state,omitempty"`
	Summary    string            `json:"summary,omitempty"`
}

const maxJobOutputBytes = 1024

type registeredPost struct {
	action JavaScriptAction
	state  map[string]string
	node   string
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
func (r Runner) RunJob(ctx context.Context, job plan.Job, workspace string) (JobResult, error) {
	if err := job.Validate(); err != nil {
		return JobResult{}, err
	}
	for _, capability := range job.RequiredCapabilities {
		if capability != "docker" {
			return JobResult{}, fmt.Errorf("capability %q is unsupported in the Phase 0 runtime", capability)
		}
	}
	if len(job.Dependencies) != 0 {
		return JobResult{}, fmt.Errorf("prerequisite result transport is not wired for this phase; refusing to run job with %d static dependencies", len(job.Dependencies))
	}
	if err := VerifyWorkflow(job, workspace); err != nil {
		return JobResult{}, err
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return JobResult{}, fmt.Errorf("resolve workspace: %w", err)
	}
	processor := newCommandProcessor(r.stdout(), r.stderr())
	eval := expression.Context{
		Matrix:      job.Matrix,
		Steps:       make(map[string]map[string]string, len(job.Steps)),
		Needs:       needOutputs(job.Needs),
		NeedResults: needResults(job.Needs),
	}
	jobEnv, err := evaluateMap(job.Env, eval)
	if err != nil {
		return JobResult{}, fmt.Errorf("evaluate job environment: %w", err)
	}
	jobResult := JobResult{Conclusion: "failure", Outputs: map[string]string{}, Env: jobEnv, State: map[string]string{}}
	jobResult.Env["GITHUB_WORKSPACE"] = workspace
	for _, name := range sortedKeys(job.Needs) {
		need := job.Needs[name]
		if need.Result == "" {
			return jobResult, fmt.Errorf("prerequisite result %q is missing from the job plan", name)
		}
		if need.Result != "success" {
			return jobResult, fmt.Errorf("prerequisite %q has result %q; non-success dependency semantics are unsupported in the Phase 0 runtime", name, need.Result)
		}
	}
	posts := make([]registeredPost, 0)

	var runErr error
	for _, step := range job.Steps {
		result, post, err := r.runJobStep(ctx, processor, workspace, job, step, jobResult.Env, eval)
		eval.Steps[strings.ToLower(step.ID)] = result.Outputs
		mergeInto(jobResult.Env, result.Env)
		mergeInto(jobResult.State, result.State)
		jobResult.Summary += result.Summary
		if post != nil {
			posts = append(posts, *post)
		}
		if err != nil {
			runErr = fmt.Errorf("step %q: %w", step.ID, err)
			break
		}
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), r.cleanupTimeout())
	defer cancel()
	for i := len(posts) - 1; i >= 0; i-- {
		post := posts[i]
		postResult := newResult()
		postResult.Env = cloneStrings(jobResult.Env)
		postErr := r.runJavaScriptPhase(cleanupCtx, processor, post.node, post.action, post.action.Post, post.state, post.state, &postResult)
		mergeInto(jobResult.Env, postResult.Env)
		mergeInto(jobResult.State, postResult.State)
		jobResult.Summary += postResult.Summary
		if postErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("post action %q: %w", post.action.Name, postErr))
		}
	}

	if runErr == nil {
		for _, name := range sortedKeys(job.Outputs) {
			template := job.Outputs[name]
			value, err := expression.Evaluate(template, eval)
			if err != nil {
				return jobResult, fmt.Errorf("job output %q: %w", name, err)
			}
			if len(value) > maxJobOutputBytes {
				return jobResult, fmt.Errorf("job output %q exceeds the %d-byte Phase 0 limit", name, maxJobOutputBytes)
			}
			jobResult.Outputs[name] = value
		}
		jobResult.Conclusion = "success"
	}
	return jobResult, runErr
}

func (r Runner) runJobStep(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, step plan.Step, jobEnv map[string]string, eval expression.Context) (Result, *registeredPost, error) {
	stepEnv, err := evaluateMap(step.Env, eval)
	if err != nil {
		return newResult(), nil, err
	}
	result := newResult()
	if step.Kind == "run" {
		script, err := expression.Evaluate(step.Command, eval)
		if err != nil {
			return result, nil, err
		}
		shell := step.Shell
		if shell == "" {
			shell = job.DefaultShell
		}
		if shell == "" {
			shell = "bash"
		}
		workingDirectory := step.WorkingDirectory
		if workingDirectory == "" {
			workingDirectory = job.DefaultWorkingDirectory
		}
		dir, err := workspacePath(workspace, workingDirectory)
		if err != nil {
			return result, nil, err
		}
		args, err := shellCommand(shell, script)
		if err != nil {
			return result, nil, err
		}
		runEnv := mergeStringMaps(jobEnv, stepEnv)
		runEnv["GITHUB_WORKSPACE"] = workspace
		err = r.runProcess(ctx, processor, dir, runEnv, &result, nil, args[0], args[1:]...)
		return result, nil, err
	}

	if !strings.HasPrefix(step.Uses, "./") {
		return result, nil, fmt.Errorf("remote action %q is unsupported in the Phase 0 runtime", step.Uses)
	}
	action, err := metadata.Load(workspace, step.Uses)
	if err != nil {
		return result, nil, err
	}
	actionRuntime, err := action.Runtime()
	if err != nil {
		return result, nil, fmt.Errorf("action %q uses %w", step.Uses, err)
	}
	actionPath := action.Path
	inputs, err := evaluateMap(step.With, eval)
	if err != nil {
		return result, nil, err
	}
	inputs, err = resolveActionInputs(action, inputs, eval)
	if err != nil {
		return result, nil, err
	}
	actionEval := eval
	actionEval.Inputs = inputs
	switch actionRuntime {
	case metadata.RuntimeNode24:
		if action.Runs.Main == "" {
			return result, nil, fmt.Errorf("JavaScript action %q has no main entry point", step.Uses)
		}
		if !supportedLifecycleCondition(action.Runs.PreIf) || !supportedLifecycleCondition(action.Runs.PostIf) {
			return result, nil, fmt.Errorf("JavaScript action %q uses unsupported pre-if or post-if", step.Uses)
		}
		node, err := DiscoverNode24(r.Node24, r.ManagedNodeRoot)
		if err != nil {
			return result, nil, err
		}
		actionEnv := mergeStringMaps(jobEnv, stepEnv)
		actionEnv["GITHUB_WORKSPACE"] = workspace
		javascript := JavaScriptAction{Name: actionName(action, step), Path: actionPath, Pre: action.Runs.Pre, Main: action.Runs.Main, Post: action.Runs.Post, Inputs: inputs, Env: actionEnv}
		state := map[string]string{}
		post := postFor(javascript, state, node)
		if javascript.Pre != "" {
			if err := r.runJavaScriptPhase(ctx, processor, node, javascript, javascript.Pre, nil, state, &result); err != nil {
				return result, post, err
			}
		}
		if err := r.runJavaScriptPhase(ctx, processor, node, javascript, javascript.Main, nil, state, &result); err != nil {
			return result, post, err
		}
		return result, post, nil
	case metadata.RuntimeComposite:
		composite, err := r.runCompositeMetadata(ctx, processor, workspace, actionPath, action, inputs, jobEnv, stepEnv, actionEval)
		return composite, nil, err
	case metadata.RuntimeDocker:
		if !job.HasCapability("docker") {
			return result, nil, fmt.Errorf("docker action %q requires the plan's docker capability", step.Uses)
		}
		if action.Runs.PreEntrypoint != "" || action.Runs.PostEntrypoint != "" || action.Runs.Entrypoint != "" || len(action.Runs.Args) != 0 {
			return result, nil, fmt.Errorf("docker action %q uses unsupported entrypoint, arguments, or pre/post lifecycle", step.Uses)
		}
		if action.Runs.Image != "Dockerfile" {
			return result, nil, fmt.Errorf("docker action image %q is unsupported; Phase 0 requires a local Dockerfile", action.Runs.Image)
		}
		dockerEnv, err := evaluateMap(action.Runs.Env, actionEval)
		if err != nil {
			return result, nil, err
		}
		result, err := r.runDocker(ctx, processor, DockerAction{Name: actionName(action, step), Path: actionPath, Workspace: workspace, Env: mergeStringMaps(jobEnv, stepEnv, actionInputEnv(inputs), dockerEnv)})
		return result, nil, err
	}
	return result, nil, fmt.Errorf("action %q uses unsupported runtime %q", step.Uses, actionRuntime)
}

func (r Runner) runCompositeMetadata(ctx context.Context, processor *commandProcessor, workspace, actionPath string, action metadata.Metadata, inputs, jobEnv, stepEnv map[string]string, eval expression.Context) (Result, error) {
	result := newResult()
	eval.Inputs = inputs
	eval.Steps = make(map[string]map[string]string)
	for i, step := range action.Runs.Steps {
		if step.Uses != "" {
			return result, fmt.Errorf("composite action nested uses %q is unsupported", step.Uses)
		}
		if step.If != "" {
			return result, fmt.Errorf("composite action step conditions are unsupported")
		}
		if strings.TrimSpace(step.Run) == "" {
			return result, fmt.Errorf("composite action step %d has no run command", i+1)
		}
		script, err := expression.Evaluate(step.Run, eval)
		if err != nil {
			return result, err
		}
		env, err := evaluateMap(step.Env, eval)
		if err != nil {
			return result, err
		}
		dir, err := workspacePath(workspace, step.WorkingDirectory)
		if err != nil {
			return result, err
		}
		args, err := shellCommand(step.Shell, script)
		if err != nil {
			return result, err
		}
		stepResult := newResult()
		runEnv := mergeStringMaps(jobEnv, result.Env, stepEnv, env, actionInputEnv(inputs), map[string]string{"GITHUB_ACTION_PATH": actionPath})
		runEnv["GITHUB_WORKSPACE"] = workspace
		if err := r.runProcess(ctx, processor, dir, runEnv, &stepResult, nil, args[0], args[1:]...); err != nil {
			return result, err
		}
		mergeInto(result.Env, stepResult.Env)
		mergeInto(result.State, stepResult.State)
		result.Summary += stepResult.Summary
		if step.ID != "" {
			eval.Steps[strings.ToLower(step.ID)] = stepResult.Outputs
		}
	}
	for _, name := range sortedKeys(action.Outputs) {
		output := action.Outputs[name]
		value, err := expression.Evaluate(output.Value, eval)
		if err != nil {
			return result, fmt.Errorf("composite output %q: %w", name, err)
		}
		result.Outputs[name] = value
	}
	return result, nil
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

func postFor(action JavaScriptAction, state map[string]string, node string) *registeredPost {
	if action.Post == "" {
		return nil
	}
	return &registeredPost{action: action, state: state, node: node}
}

func supportedLifecycleCondition(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == "always()" || value == "${{ always() }}"
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
