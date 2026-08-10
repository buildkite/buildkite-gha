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
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return result, fmt.Errorf("%s inspect workspace: %w", adapter, err)
	}
	if len(entries) != 0 {
		return result, fmt.Errorf("%s requires an empty workspace; Phase 6 is required for clean behavior", adapter)
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
	fetchArgs := checkoutFetchArgs(inputs, job.Event.SHA)
	if credentialed {
		if err := r.runRepositoryProviderCheckoutFetch(ctx, processor, workspace, env, git, base, fetchArgs); err != nil {
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
		return result, fmt.Errorf("%s did not produce exact detached SHA %s", adapter, job.Event.SHA)
	}
	result.Outputs["ref"] = job.Event.Ref
	result.Outputs["commit"] = job.Event.SHA
	return result, nil
}

func checkoutFetchArgs(inputs map[string]string, sha string) []string {
	for name, value := range inputs {
		if strings.EqualFold(name, "fetch-depth") && value == "0" {
			return []string{
				"fetch", "--no-tags", "--prune", "--no-recurse-submodules", "origin",
				"+refs/heads/*:refs/remotes/origin/*",
				"+refs/tags/*:refs/tags/*",
				"+" + sha + ":refs/buildkite-gha/event",
			}
		}
	}
	return []string{"fetch", "--no-tags", "--no-recurse-submodules", "--depth=1", "origin", sha}
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
