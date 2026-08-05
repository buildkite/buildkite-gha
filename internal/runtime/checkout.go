package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

var checkoutRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var checkoutSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validCheckoutRepository(repository string) bool {
	if len(repository) > 140 || !checkoutRepositoryPattern.MatchString(repository) {
		return false
	}
	parts := strings.Split(repository, "/")
	return parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".."
}

func resolvePrivateCheckoutGit(configured string) (string, error) {
	candidate := configured
	if candidate == "" {
		candidate = "git"
	}
	resolved, err := exec.LookPath(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve private checkout Git before workflow execution: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve private checkout Git absolute path before workflow execution: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("canonicalize private checkout Git before workflow execution: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("private checkout requires a real executable Git resolved before workflow execution")
	}
	return resolved, nil
}

func (r Runner) runCheckout(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, inputs map[string]string) (Result, error) {
	result := newResult()
	private := job.HasCapability("provider-token-read")
	adapter := "tokenless checkout adapter"
	if private {
		adapter = "private checkout adapter"
	}
	if job.Event.Provider != "github" || !validCheckoutRepository(job.Event.Repository) || !checkoutSHAPattern.MatchString(job.Event.SHA) {
		return result, fmt.Errorf("%s requires a valid github.com event repository and exact SHA; Phase 6 is required for other events", adapter)
	}
	if err := actionintegration.ValidateCheckoutInputs(inputs, job.Event.Repository, job.Event.SHA); err != nil {
		return result, fmt.Errorf("%s: %w", adapter, err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return result, fmt.Errorf("tokenless checkout adapter inspect workspace: %w", err)
	}
	if len(entries) != 0 {
		return result, fmt.Errorf("tokenless checkout adapter requires an empty workspace; Phase 6 is required for clean behavior")
	}
	git := r.Git
	if private && (git == "" || !filepath.IsAbs(git)) {
		return result, fmt.Errorf("private checkout adapter requires Git to be resolved before workflow execution")
	}
	if !private && git == "" {
		git, err = exec.LookPath("git")
	}
	if err != nil {
		return result, fmt.Errorf("tokenless checkout adapter discover Git: %w", err)
	}
	env := map[string]string{
		"HOME":                filepath.Join(workspace, ".no-home"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   filepath.Join(workspace, ".no-global-gitconfig"),
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",
		"GIT_SSH_COMMAND":     "ssh -oBatchMode=yes",
	}
	base := []string{"-c", "credential.helper=", "-c", "http.extraheader=", "-c", "core.hooksPath=/dev/null"}
	run := func(runEnv map[string]string, args ...string) error {
		if err := r.runStreaming(ctx, processor, workspace, runEnv, git, append(base, args...)...); err != nil {
			return fmt.Errorf("%s git %s: %w", adapter, args[0], err)
		}
		return nil
	}
	if err := run(env, "init", "--template=", "."); err != nil {
		return result, err
	}
	url := "https://github.com/" + job.Event.Repository + ".git"
	if err := run(env, "remote", "add", "origin", url); err != nil {
		return result, err
	}
	fetchArgs := []string{"fetch", "--no-tags", "--no-recurse-submodules", "--depth=1", "origin", job.Event.SHA}
	if private {
		if err := r.runPrivateCheckoutFetch(ctx, processor, workspace, env, git, base, job.Event.Repository, fetchArgs); err != nil {
			return result, fmt.Errorf("%s git fetch: %w", adapter, err)
		}
	} else if err := run(env, fetchArgs...); err != nil {
		return result, err
	}
	if err := run(env, "checkout", "--detach", job.Event.SHA); err != nil {
		return result, err
	}
	head, err := os.ReadFile(filepath.Join(workspace, ".git", "HEAD"))
	if err != nil || strings.TrimSpace(string(head)) != job.Event.SHA {
		return result, fmt.Errorf("tokenless checkout adapter did not produce exact detached SHA %s", job.Event.SHA)
	}
	result.Outputs["ref"] = job.Event.Ref
	result.Outputs["commit"] = job.Event.SHA
	return result, nil
}

func (r Runner) runPrivateCheckoutFetch(ctx context.Context, processor *commandProcessor, workspace string, env map[string]string, git string, base []string, repository string, fetchArgs []string) error {
	if r.Checkout == nil {
		return fmt.Errorf("private checkout token provider is not configured")
	}
	token, err := r.Checkout.Token(ctx, repository)
	if err != nil {
		return err
	}
	if len(token) > 16<<10 || !githubInstallationTokenPattern.MatchString(token) {
		return fmt.Errorf("private checkout token provider returned an invalid token")
	}
	if r.Redactor == nil {
		return fmt.Errorf("private checkout token provider requires a redactor")
	}
	processor.addMask(token)
	if err := r.Redactor.AddRedaction(ctx, token); err != nil {
		return processor.scrubError(err)
	}

	helperDir, err := os.MkdirTemp("", "buildkite-gha-askpass-")
	if err != nil {
		return fmt.Errorf("create private checkout askpass directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(helperDir) }()
	helper := filepath.Join(helperDir, "askpass")
	const script = `#!/bin/sh
case "$1" in
  *sername*) printf '%s\n' x-access-token ;;
  *assword*) IFS= read -r token <&3 || exit 1; printf '%s\n' "$token" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		return fmt.Errorf("create private checkout askpass helper: %w", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create private checkout credential pipe: %w", err)
	}
	if _, err := io.WriteString(writer, token+"\n"); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return fmt.Errorf("write private checkout credential pipe: %w", err)
	}
	if err := writer.Close(); err != nil {
		_ = reader.Close()
		return fmt.Errorf("close private checkout credential pipe: %w", err)
	}
	defer func() { _ = reader.Close() }()

	privateEnv := cloneStrings(env)
	privateEnv["GIT_ASKPASS"] = helper
	privateEnv["LC_ALL"] = "C"
	cmd := exec.Command(git, append(base, fetchArgs...)...)
	cmd.Dir = workspace
	cmd.Env = processEnv(privateEnv)
	cmd.ExtraFiles = []*os.File{reader}
	return processor.scrubError(r.runStreamingCommand(ctx, processor, cmd))
}
