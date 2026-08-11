package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

var checkoutRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var checkoutSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var agentProxyEnvironmentNames = [...]string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

// AgentRepositoryCredentials configure Buildkite's native, job-bound Git
// credential helper for one verified checkout fetch.
type AgentRepositoryCredentials struct {
	Agent            string
	Endpoint         string
	JobID            string
	JobToken         string
	NoHTTP2          string
	proxyEnvironment map[string]string
}

func resolveAgentRepositoryCredentialsBeforeWorkflow(credentials *AgentRepositoryCredentials) (*AgentRepositoryCredentials, error) {
	if credentials == nil {
		return nil, nil
	}
	if !validBuildkiteJobID(credentials.JobID) {
		return nil, fmt.Errorf("repository-provider credentials require the current Buildkite job ID")
	}
	if credentials.JobToken == "" || strings.ContainsAny(credentials.JobToken, "\r\n") {
		return nil, fmt.Errorf("repository-provider credentials require the current Buildkite Agent access token")
	}
	agent, err := resolveHostExecutableBeforeWorkflow(credentials.Agent, "buildkite-agent", "Buildkite Agent Git credential helper")
	if err != nil {
		return nil, err
	}
	resolved := *credentials
	resolved.Agent = agent
	resolved.proxyEnvironment = make(map[string]string, len(agentProxyEnvironmentNames))
	for _, name := range agentProxyEnvironmentNames {
		if value, ok := os.LookupEnv(name); ok {
			resolved.proxyEnvironment[name] = value
		}
	}
	return &resolved, nil
}

func agentGitCredentialHelperCommand(agent string) string {
	return "!'" + strings.ReplaceAll(agent, "'", `'\''`) + "' git-credentials-helper"
}

func validCheckoutRepository(repository string) bool {
	if len(repository) > 140 || !checkoutRepositoryPattern.MatchString(repository) {
		return false
	}
	parts := strings.Split(repository, "/")
	return parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".."
}

func (r Runner) runCheckout(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, inputs map[string]string) (Result, error) {
	result := newResult()
	const adapter = "checkout adapter"
	credentialed := job.HasCapability("provider-token-read") && r.RepositoryCredentials != nil
	if job.Event.Provider != "github" || !validCheckoutRepository(job.Event.Repository) || !checkoutSHAPattern.MatchString(job.Event.SHA) {
		return result, fmt.Errorf("%s requires a valid github.com event repository and exact SHA; Phase 6 is required for other events", adapter)
	}
	if err := actionintegration.ValidateCheckoutInputs(inputs, job.Event.Repository, job.Event.SHA); err != nil {
		return result, fmt.Errorf("%s: %w", adapter, err)
	}
	checkoutDirectory, err := prepareCheckoutDirectory(workspace, inputs)
	if err != nil {
		return result, fmt.Errorf("%s: %w", adapter, err)
	}
	git := r.Git
	if credentialed && (git == "" || !filepath.IsAbs(git)) {
		return result, fmt.Errorf("repository-provider checkout requires Git to be resolved before workflow execution")
	}
	if !credentialed && git == "" {
		git, err = exec.LookPath("git")
	}
	if err != nil {
		return result, fmt.Errorf("%s discover Git: %w", adapter, err)
	}
	env := map[string]string{
		"HOME":                filepath.Join(checkoutDirectory, ".no-home"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   filepath.Join(checkoutDirectory, ".no-global-gitconfig"),
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",
		"GIT_SSH_COMMAND":     "ssh -oBatchMode=yes",
	}
	base := []string{"-c", "credential.helper=", "-c", "http.extraheader=", "-c", "core.hooksPath=/dev/null"}
	run := func(runEnv map[string]string, args ...string) error {
		if err := r.runStreaming(ctx, processor, checkoutDirectory, runEnv, git, append(base, args...)...); err != nil {
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
	fetchArgs := checkoutFetchArgs(inputs, job.Event.SHA)
	if credentialed {
		if err := r.runRepositoryProviderCheckoutFetch(ctx, processor, checkoutDirectory, env, git, base, fetchArgs); err != nil {
			return result, fmt.Errorf("%s git fetch: %w", adapter, err)
		}
	} else if err := run(env, fetchArgs...); err != nil {
		return result, err
	}
	checkoutTarget := checkoutRevision(inputs, job.Event.SHA)
	if err := run(env, "checkout", "--detach", checkoutTarget); err != nil {
		return result, err
	}
	head, err := os.ReadFile(filepath.Join(checkoutDirectory, ".git", "HEAD"))
	headSHA := strings.TrimSpace(string(head))
	if err != nil || !checkoutSHAPattern.MatchString(headSHA) || checkoutSHAPattern.MatchString(checkoutTarget) && headSHA != checkoutTarget {
		return result, fmt.Errorf("%s did not produce the requested detached revision", adapter)
	}
	result.Outputs["ref"] = checkoutRefOutput(inputs, job.Event.Ref)
	result.Outputs["commit"] = headSHA
	return result, nil
}

func validateCheckoutRefProvenance(sourceInputs, evaluatedInputs map[string]string, eventSHA string) error {
	for name, value := range sourceInputs {
		if !strings.EqualFold(name, "ref") {
			continue
		}
		root, path, err := expression.ReferencePath(value)
		if err != nil {
			return nil
		}
		requiresEventSHA := strings.EqualFold(root, "github") && len(path) == 1 && strings.EqualFold(path[0], "sha") ||
			strings.EqualFold(root, "needs") && len(path) == 3 && strings.EqualFold(path[1], "outputs")
		if requiresEventSHA && checkoutInput(evaluatedInputs, "ref") != eventSHA {
			return fmt.Errorf("checkout adapter dynamic ref must resolve to the exact event SHA")
		}
	}
	return nil
}

func prepareCheckoutDirectory(workspace string, inputs map[string]string) (string, error) {
	path := checkoutInput(inputs, "path")
	if path == "" {
		entries, err := os.ReadDir(workspace)
		if err != nil {
			return "", fmt.Errorf("inspect workspace: %w", err)
		}
		if len(entries) != 0 {
			return "", fmt.Errorf("workspace checkout requires an empty workspace; clean behavior is unsupported")
		}
		return workspace, nil
	}
	directory := filepath.Join(workspace, filepath.FromSlash(path))
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("checkout path %q already exists; clean behavior is unsupported", path)
	}
	if err := os.Mkdir(directory, 0o755); err != nil {
		return "", err
	}
	return directory, nil
}

func checkoutRefOutput(inputs map[string]string, eventRef string) string {
	ref := checkoutInput(inputs, "ref")
	if ref == "" {
		return eventRef
	}
	if checkoutSHAPattern.MatchString(ref) {
		return ""
	}
	return ref
}

func checkoutFetchArgs(inputs map[string]string, sha string) []string {
	progress := true
	fetchTags := false
	depth := "1"
	for name, value := range inputs {
		switch {
		case strings.EqualFold(name, "fetch-depth"):
			depth = value
		case strings.EqualFold(name, "fetch-tags"):
			fetchTags = checkoutInputTrue(value)
		case strings.EqualFold(name, "show-progress"):
			progress = checkoutInputTrue(value)
		}
	}
	args := []string{"fetch", "--no-tags", "--no-recurse-submodules"}
	if progress {
		args = append(args, "--progress")
	}
	if depth == "0" {
		args = append(args,
			"--prune", "origin",
			"+refs/heads/*:refs/remotes/origin/*",
			"+refs/tags/*:refs/tags/*",
		)
		ref := checkoutInput(inputs, "ref")
		if ref == "" || ref == sha {
			args = append(args, "+"+sha+":refs/buildkite-gha/event")
		} else if checkoutSHAPattern.MatchString(ref) {
			args = append(args, "+"+ref+":refs/buildkite-gha/selected")
		}
		return args
	}
	args = append(args, "--depth="+depth, "origin", checkoutFetchRevision(inputs, sha))
	if fetchTags {
		args = append(args, "+refs/tags/*:refs/tags/*")
	}
	return args
}

func checkoutFetchRevision(inputs map[string]string, sha string) string {
	ref := checkoutInput(inputs, "ref")
	if ref == "" || ref == sha {
		return sha
	}
	if checkoutSHAPattern.MatchString(ref) {
		return ref
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	return "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
}

func checkoutRevision(inputs map[string]string, sha string) string {
	ref := checkoutInput(inputs, "ref")
	if ref == "" || ref == sha {
		return sha
	}
	if checkoutSHAPattern.MatchString(ref) {
		return ref
	}
	return "refs/remotes/origin/" + strings.TrimPrefix(ref, "refs/heads/")
}

func checkoutInput(inputs map[string]string, wanted string) string {
	for name, value := range inputs {
		if strings.EqualFold(name, wanted) {
			return value
		}
	}
	return ""
}

func checkoutInputTrue(value string) bool {
	return value == "true" || value == "True" || value == "TRUE"
}

func (r Runner) runRepositoryProviderCheckoutFetch(ctx context.Context, processor *commandProcessor, workspace string, env map[string]string, git string, base, fetchArgs []string) error {
	credentials := r.RepositoryCredentials
	if credentials == nil || credentials.Agent == "" || !filepath.IsAbs(credentials.Agent) {
		return fmt.Errorf("repository-provider credentials were not resolved before workflow execution")
	}
	processor.addMask(credentials.JobToken)
	credentialEnv := cloneStrings(env)
	credentialEnv["BUILDKITE_AGENT_ACCESS_TOKEN"] = credentials.JobToken
	credentialEnv["BUILDKITE_JOB_ID"] = credentials.JobID
	if credentials.Endpoint != "" {
		credentialEnv["BUILDKITE_AGENT_ENDPOINT"] = credentials.Endpoint
	}
	if credentials.NoHTTP2 != "" {
		credentialEnv["BUILDKITE_NO_HTTP2"] = credentials.NoHTTP2
	}
	for name, value := range credentials.proxyEnvironment {
		credentialEnv[name] = value
	}
	credentialArgs := append(append([]string(nil), base...),
		"-c", "credential.useHttpPath=true",
		"-c", "credential.helper="+agentGitCredentialHelperCommand(credentials.Agent),
	)
	cmd := exec.Command(git, append(credentialArgs, fetchArgs...)...)
	cmd.Dir = workspace
	cmd.Env = processEnv(credentialEnv)
	return processor.scrubError(r.runStreamingCommand(ctx, processor, cmd))
}
