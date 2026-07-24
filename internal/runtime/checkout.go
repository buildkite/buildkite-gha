package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

var checkoutRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var checkoutSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (r Runner) runCheckout(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, inputs map[string]string) (Result, error) {
	result := newResult()
	if job.Event.Provider != "github" || !checkoutRepositoryPattern.MatchString(job.Event.Repository) || !checkoutSHAPattern.MatchString(job.Event.SHA) {
		return result, fmt.Errorf("tokenless checkout adapter requires a valid github.com event repository and exact SHA; Phase 6 is required for other events")
	}
	if err := validateCheckoutRuntimeInputs(inputs, job.Event.Repository, job.Event.SHA); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return result, fmt.Errorf("tokenless checkout adapter inspect workspace: %w", err)
	}
	if len(entries) != 0 {
		return result, fmt.Errorf("tokenless checkout adapter requires an empty workspace; Phase 6 is required for clean behavior")
	}
	git := r.Git
	if git == "" {
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
	run := func(args ...string) error {
		base := []string{"-c", "credential.helper=", "-c", "http.extraheader=", "-c", "core.hooksPath=/dev/null"}
		if err := r.runStreaming(ctx, processor, workspace, env, git, append(base, args...)...); err != nil {
			return fmt.Errorf("tokenless checkout adapter git %s: %w", args[0], err)
		}
		return nil
	}
	if err := run("init", "--template=", "."); err != nil {
		return result, err
	}
	url := "https://github.com/" + job.Event.Repository + ".git"
	if err := run("remote", "add", "origin", url); err != nil {
		return result, err
	}
	if err := run("fetch", "--no-tags", "--no-recurse-submodules", "--depth=1", "origin", job.Event.SHA); err != nil {
		return result, err
	}
	if err := run("checkout", "--detach", job.Event.SHA); err != nil {
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

func validateCheckoutRuntimeInputs(inputs map[string]string, repository, sha string) error {
	seen := make(map[string]bool, len(inputs))
	for _, name := range sortedKeys(inputs) {
		value := inputs[name]
		normalized := strings.ToLower(name)
		if seen[normalized] {
			return fmt.Errorf("tokenless checkout adapter does not support duplicate case-insensitive input %q; Phase 6 is required", name)
		}
		seen[normalized] = true
		switch normalized {
		case "repository":
			if strings.EqualFold(value, repository) {
				continue
			}
		case "ref":
			if value == sha {
				continue
			}
		case "persist-credentials":
			if value == "false" {
				continue
			}
		case "fetch-depth":
			if value == "1" {
				continue
			}
		case "clean", "set-safe-directory":
			if value == "true" {
				continue
			}
		}
		return fmt.Errorf("tokenless checkout adapter does not support explicit input %q (including empty token); Phase 6 is required", name)
	}
	return nil
}
