package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	executionprogram "github.com/buildkite/buildkite-gha/internal/program"
	shellcompat "github.com/buildkite/buildkite-gha/internal/shell"
)

func (r *jobRun) runWorkflowShellStep(ctx context.Context, processor *commandOutputProcessor, workspace string, job plan.Job, step plan.Step, env map[string]string, eval expression.Context) (Result, error) {
	result := newResult()
	script, err := evaluateProgramTyped[string](step.Execution.Run.Command, executionprogram.EvaluationContext{Expression: eval})
	if err != nil {
		return result, err
	}
	shellSite := step.Execution.Run.Shell
	if shellSite.Source == "" {
		shellSite = job.Program.Job.Defaults.Shell
	}
	shell, err := evaluateProgramTyped[string](shellSite, executionprogram.EvaluationContext{Expression: eval})
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
	workingDirectorySite := step.Execution.Run.WorkingDirectory
	if workingDirectorySite.Source == "" {
		workingDirectorySite = job.Program.Job.Defaults.WorkingDirectory
	}
	workingDirectory, err := evaluateProgramTyped[string](workingDirectorySite, executionprogram.EvaluationContext{Expression: eval})
	if err != nil {
		return result, err
	}
	dir, err := r.shellWorkingDirectory(workspace, workingDirectory)
	if err != nil {
		return result, err
	}
	err = r.runShellProcess(ctx, processor, dir, env, &result, shell, script)
	return result, err
}

func (r *jobRun) runCompositeShellStep(ctx context.Context, processor *commandOutputProcessor, workspace string, step *executionprogram.ActionStep, jobEnv map[string]string, eval expression.Context, result *Result) error {
	env, err := executionprogram.EvaluateBindings(step.Env, executionprogram.EvaluationContext{Expression: eval})
	if err != nil {
		return err
	}
	script, err := evaluateProgramString(step.Run.Command, eval)
	if err != nil {
		return err
	}
	workingDirectory, err := evaluateProgramString(step.WorkingDirectory, eval)
	if err != nil {
		return err
	}
	dir, err := r.shellWorkingDirectory(workspace, workingDirectory)
	if err != nil {
		return err
	}
	shell, err := evaluateProgramString(step.Shell, eval)
	if err != nil {
		return err
	}
	return r.runShellProcess(ctx, processor, dir, mergeStepEnvironment(jobEnv, env), result, shell, script)
}

func (r *jobRun) shellWorkingDirectory(workspace, workingDirectory string) (string, error) {
	if r.jobContainer != nil {
		workingDirectory = r.jobContainer.hostPath(workingDirectory)
	}
	return stepWorkingDirectory(workspace, workingDirectory)
}

func shellCommand(shell, script string) ([]string, error) {
	switch strings.TrimSpace(shell) {
	case "", "bash":
		return []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script}, nil
	case "sh":
		return []string{"sh", "-e", "-c", script}, nil
	default:
		if err := shellcompat.ValidateCompatibility(shell); err != nil {
			return nil, errUnsupportedFeature("shell", "", "%s", err)
		}
		return nil, errUnsupportedFeature("shell", "", "shell %q is unsupported in the supported runtime subset", shell)
	}
}

func (r *jobRun) runShellProcess(ctx context.Context, processor *commandOutputProcessor, dir string, env map[string]string, result *Result, shell, script string) error {
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
		if err := shellcompat.ValidateCompatibility(shell); err != nil {
			return errUnsupportedFeature("shell", "", "%s", err)
		}
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
