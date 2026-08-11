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
		"HOME":                   filepath.Join(workspace, ".no-home"),
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_CONFIG_GLOBAL":      os.DevNull,
		"GIT_TERMINAL_PROMPT":    "0",
		"GIT_ASKPASS":            "",
		"SSH_ASKPASS":            "",
		"GIT_SSH_COMMAND":        "false",
		"GIT_PROTOCOL_FROM_USER": "0",
	}
	base := checkoutGitBaseArgs()
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
	mode := checkoutSubmoduleMode(inputs)
	if mode != "" {
		if err := r.runCheckoutSubmodules(ctx, processor, workspace, git, env, base, checkoutFetchDepth(inputs) != "0", mode == "recursive", credentialed); err != nil {
			return result, fmt.Errorf("%s submodules: %w", adapter, err)
		}
	}
	head, err := os.ReadFile(filepath.Join(workspace, ".git", "HEAD"))
	if err != nil || strings.TrimSpace(string(head)) != job.Event.SHA {
		return result, fmt.Errorf("%s did not produce exact detached SHA %s", adapter, job.Event.SHA)
	}
	result.Outputs["ref"] = checkoutRefOutput(inputs, job.Event.Ref)
	result.Outputs["commit"] = job.Event.SHA
	return result, nil
}

func checkoutGitBaseArgs() []string {
	return []string{
		"--literal-pathspecs",
		"-c", "credential.helper=", "-c", "http.extraheader=", "-c", "core.hooksPath=/dev/null",
		"-c", "http.followRedirects=false",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never",
		"-c", "protocol.version=2",
		"-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true", "-c", "core.commitGraph=false", "-c", "fetch.writeCommitGraph=false",
	}
}

func checkoutSubmoduleMode(inputs map[string]string) string {
	for name, value := range inputs {
		if strings.EqualFold(name, "submodules") {
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "true":
				return "direct"
			case "recursive":
				return "recursive"
			}
		}
	}
	return ""
}

func checkoutFetchDepth(inputs map[string]string) string {
	for name, value := range inputs {
		if strings.EqualFold(name, "fetch-depth") {
			return value
		}
	}
	return "1"
}

type checkoutSubmoduleStatusWriter struct {
	lines   int
	invalid string
}

func (w *checkoutSubmoduleStatusWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSuffix(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		w.lines++
		if w.invalid == "" && (w.lines > 100_000 || line[0] != ' ') {
			w.invalid = line
		}
	}
	return len(p), nil
}

func (r Runner) runCheckoutSubmodules(ctx context.Context, processor *commandProcessor, workspace string, git string, env map[string]string, base []string, depthOne, recursive, credentialed bool) error {
	policy := append(append([]string{}, base...), "-c", "url.https://github.com/.insteadOf=git@github.com:")
	run := func(withCredentials bool, args ...string) error {
		if withCredentials {
			if err := r.runRepositoryProviderCheckoutFetch(ctx, processor, workspace, env, git, policy, args); err != nil {
				return fmt.Errorf("git %s: %w", args[0], err)
			}
			return nil
		}
		if err := r.runStreaming(ctx, processor, workspace, env, git, append(policy, args...)...); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}

	syncArgs := []string{"submodule", "sync"}
	updateArgs := []string{"submodule", "update", "--init", "--force"}
	statusArgs := []string{"submodule", "status"}
	if depthOne {
		updateArgs = append(updateArgs, "--depth=1")
	}
	if recursive {
		syncArgs = append(syncArgs, "--recursive")
		updateArgs = append(updateArgs, "--recursive")
		statusArgs = append(statusArgs, "--recursive")
	}
	if err := run(false, syncArgs...); err != nil {
		return err
	}
	if err := run(credentialed, updateArgs...); err != nil {
		return err
	}

	status := &checkoutSubmoduleStatusWriter{}
	statusProcessor := newCommandProcessor(status, processor.stderr)
	if err := r.runStreaming(ctx, statusProcessor, workspace, env, git, append(policy, statusArgs...)...); err != nil {
		return fmt.Errorf("git submodule status: %w", err)
	}
	if status.invalid != "" {
		return fmt.Errorf("git submodule status reported invalid state %q", status.invalid)
	}
	return nil
}

func checkoutRefOutput(inputs map[string]string, eventRef string) string {
	for name, value := range inputs {
		if strings.EqualFold(name, "ref") && value != "" {
			return ""
		}
	}
	return eventRef
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
		return append(args,
			"--prune", "origin",
			"+refs/heads/*:refs/remotes/origin/*",
			"+refs/tags/*:refs/tags/*",
			"+"+sha+":refs/buildkite-gha/event",
		)
	}
	args = append(args, "--depth=1", "origin", sha)
	if fetchTags {
		args = append(args, "+refs/tags/*:refs/tags/*")
	}
	return args
}

func checkoutInputTrue(value string) bool {
	return value == "true" || value == "True" || value == "TRUE"
}

func repositoryProviderCheckoutCredentialArgs(base []string, agent string) []string {
	return append(append([]string(nil), base...),
		"-c", "credential.https://github.com.useHttpPath=true",
		"-c", "http.followRedirects=false",
		"-c", "credential.https://github.com.helper="+agentGitCredentialHelperCommand(agent),
	)
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
	credentialArgs := repositoryProviderCheckoutCredentialArgs(base, credentials.Agent)
	cmd := exec.Command(git, append(credentialArgs, fetchArgs...)...)
	cmd.Dir = workspace
	cmd.Env = processEnv(credentialEnv)
	return processor.scrubError(r.runStreamingCommand(ctx, processor, cmd))
}
