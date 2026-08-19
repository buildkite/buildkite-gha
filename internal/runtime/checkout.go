package runtime

import (
	"context"
	"fmt"
	"maps"
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

func (r Runner) runCheckout(ctx context.Context, processor *commandProcessor, workspace string, job plan.Job, commit string, inputs map[string]string) (Result, error) {
	const adapter = "checkout adapter"
	remoteInputs, remoteRefOutput, remotePinnedRef, remote, err := remoteWorkflowCheckoutInputs(job, commit, inputs)
	if err != nil {
		return newResult(), fmt.Errorf("%s: %w", adapter, err)
	}
	if remote {
		return r.runCheckoutTarget(ctx, processor, workspace, commit, remoteInputs, "github", job.Workflow.Remote.Repository, job.Workflow.Remote.Commit, remoteRefOutput, remotePinnedRef, false)
	}
	_, _, validProvider := checkoutRepositoryURL(job.Event.Provider, job.Event.Repository)
	if !validProvider || !actionintegration.ValidCheckoutSHA(job.Event.SHA) {
		return newResult(), fmt.Errorf("%s requires a valid GitHub or Origin event repository and exact SHA; other event sources are unsupported", adapter)
	}
	if err := actionintegration.ValidateCheckoutInputs(commit, inputs, job.Event.Repository, job.Event.SHA); err != nil {
		return newResult(), fmt.Errorf("%s: %w", adapter, err)
	}
	credentialed := job.HasCapability("provider-token-read") && r.RepositoryCredentials != nil
	return r.runCheckoutTarget(ctx, processor, workspace, commit, inputs, job.Event.Provider, job.Event.Repository, job.Event.SHA, checkoutRefOutput(inputs, job.Event.Ref), "", credentialed)
}

func (r Runner) runCheckoutTarget(ctx context.Context, processor *commandProcessor, workspace string, commit string, inputs map[string]string, provider, repository, sha, refOutput, pinnedRef string, credentialed bool) (Result, error) {
	result := newResult()
	const adapter = "checkout adapter"
	url, credentialHost, _ := checkoutRepositoryURL(provider, repository)
	inputs = checkoutInputsWithReleaseDefaults(commit, inputs)
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
		"HOME":                   filepath.Join(checkoutDirectory, ".no-home"),
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
		if err := r.runStreaming(ctx, processor, checkoutDirectory, runEnv, git, append(base, args...)...); err != nil {
			return fmt.Errorf("%s git %s: %w", adapter, args[0], err)
		}
		return nil
	}
	if err := run(env, "init", "--template=", "."); err != nil {
		return result, err
	}
	if err := run(env, "remote", "add", "origin", url); err != nil {
		return result, err
	}
	fetchArgs := checkoutFetchArgs(inputs, sha)
	if credentialed {
		if err := r.runRepositoryProviderCheckoutFetch(ctx, processor, checkoutDirectory, env, git, base, fetchArgs, credentialHost); err != nil {
			return result, fmt.Errorf("%s git fetch: %w", adapter, err)
		}
	} else if err := run(env, fetchArgs...); err != nil {
		return result, err
	}
	checkoutTarget := checkoutRevision(inputs, sha)
	if err := run(env, "checkout", "--detach", checkoutTarget); err != nil {
		return result, err
	}
	if pinnedRef != "" && pinnedRef != sha {
		if err := run(env, "update-ref", pinnedRef, sha); err != nil {
			return result, err
		}
	}
	mode := checkoutSubmoduleMode(inputs)
	if mode != "" {
		if err := r.runCheckoutSubmodules(ctx, processor, checkoutDirectory, git, env, base, checkoutFetchDepth(inputs) != "0", mode == "recursive", credentialed, credentialHost); err != nil {
			return result, fmt.Errorf("%s submodules: %w", adapter, err)
		}
	}
	head, err := os.ReadFile(filepath.Join(checkoutDirectory, ".git", "HEAD"))
	headSHA := strings.TrimSpace(string(head))
	if err != nil || !actionintegration.ValidCheckoutSHA(headSHA) || actionintegration.ValidCheckoutSHA(checkoutTarget) && headSHA != checkoutTarget {
		return result, fmt.Errorf("%s did not produce the requested detached revision", adapter)
	}
	setCheckoutOutputs(result.Outputs, commit, refOutput, headSHA)
	return result, nil
}

func remoteWorkflowCheckoutInputs(job plan.Job, commit string, inputs map[string]string) (map[string]string, string, string, bool, error) {
	remote := job.Workflow.Remote
	if remote != nil {
		if err := actionintegration.ValidateCheckoutInputNames(inputs); err != nil {
			return nil, "", "", true, err
		}
	}
	repository := checkoutInput(inputs, "repository")
	if remote == nil || repository == "" || !strings.EqualFold(repository, remote.Repository) {
		return nil, "", "", false, nil
	}
	ref := checkoutInput(inputs, "ref")
	refMatches := remoteWorkflowRefMatches(ref, *remote)
	path := checkoutInput(inputs, "path")
	aliasMatches := remoteWorkflowCheckoutAlias(job.Actions, path)
	if (!refMatches || !aliasMatches) && strings.EqualFold(repository, job.Event.Repository) {
		return nil, "", "", false, nil
	}
	if !refMatches {
		return nil, "", "", true, fmt.Errorf("remote workflow source checkout ref does not match immutable workflow provenance")
	}
	if !aliasMatches {
		return nil, "", "", true, fmt.Errorf("remote workflow source checkout path does not match a source-backed local action")
	}
	normalized := maps.Clone(inputs)
	deleteCheckoutInput(normalized, "token")
	setCheckoutInput(normalized, "repository", remote.Repository)
	setCheckoutInput(normalized, "ref", remote.Commit)
	if err := actionintegration.ValidateCheckoutInputs(commit, normalized, remote.Repository, remote.Commit); err != nil {
		return nil, "", "", true, err
	}
	pinnedRef := remote.ResolvedRef
	if pinnedRef == remote.Commit {
		pinnedRef = ""
	} else if !validPinnedCheckoutRef(pinnedRef) {
		return nil, "", "", true, fmt.Errorf("remote workflow source checkout ref is invalid")
	}
	return normalized, checkoutRefOutput(inputs, ""), pinnedRef, true, nil
}

func remoteWorkflowRefMatches(ref string, remote plan.RemoteWorkflowSource) bool {
	// Checkout gives a bare branch precedence over a same-named tag, while a
	// reusable-workflow reference does the opposite. Bare names are safe only
	// when workflow provenance selected that branch.
	return ref == remote.Commit || ref == remote.ResolvedRef ||
		ref == remote.RequestedRef && !strings.HasPrefix(ref, "refs/") && remote.ResolvedRef == "refs/heads/"+remote.RequestedRef
}

func remoteWorkflowCheckoutAlias(locks []plan.ActionLock, path string) bool {
	if !actionintegration.ValidCheckoutPath(path) {
		return false
	}
	for _, lock := range locks {
		if lock.WorkspaceAlias == path {
			return true
		}
	}
	return false
}

func validPinnedCheckoutRef(ref string) bool {
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, prefix) {
			return actionintegration.ValidCheckoutBranch(strings.TrimPrefix(ref, prefix))
		}
	}
	return false
}

func deleteCheckoutInput(inputs map[string]string, target string) {
	for name := range inputs {
		if strings.EqualFold(name, target) {
			delete(inputs, name)
		}
	}
}

func setCheckoutInput(inputs map[string]string, target, value string) {
	deleteCheckoutInput(inputs, target)
	inputs[target] = value
}

func setCheckoutOutputs(outputs map[string]string, commit, ref, headSHA string) {
	// Checkout outputs were added in v4.2.0 and aren't part of earlier contracts.
	if !actionintegration.CheckoutSupportsOutputs(commit) {
		return
	}
	outputs["ref"] = ref
	outputs["commit"] = headSHA
}

// checkoutInputsWithReleaseDefaults applies v1's full-history fetch default,
// which lived in the historical runner plugin rather than an action manifest.
func checkoutInputsWithReleaseDefaults(commit string, inputs map[string]string) map[string]string {
	if !actionintegration.CheckoutDefaultsToFullHistory(commit) || checkoutInput(inputs, "fetch-depth") != "" {
		return inputs
	}
	defaulted := maps.Clone(inputs)
	if defaulted == nil {
		defaulted = map[string]string{}
	}
	defaulted["fetch-depth"] = "0"
	return defaulted
}

func checkoutRepositoryURL(provider, repository string) (url, credentialHost string, ok bool) {
	if !validCheckoutRepository(repository) {
		return "", "", false
	}
	switch provider {
	case "github":
		return "https://github.com/" + repository + ".git", "github.com", true
	case "cursor-origin":
		return "https://origin.cursor.com/git/" + repository + ".git", "origin.cursor.com", true
	default:
		return "", "", false
	}
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

func (r Runner) runCheckoutSubmodules(ctx context.Context, processor *commandProcessor, workspace string, git string, env map[string]string, base []string, depthOne, recursive, credentialed bool, credentialHost string) error {
	policy := append(append([]string{}, base...), "-c", "url.https://github.com/.insteadOf=git@github.com:")
	run := func(withCredentials bool, args ...string) error {
		if withCredentials {
			if err := r.runRepositoryProviderCheckoutFetch(ctx, processor, workspace, env, git, policy, args, credentialHost); err != nil {
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
	if actionintegration.ValidCheckoutSHA(ref) {
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
		revision, branch := checkoutSelectedRevision(inputs, sha)
		if !branch {
			namespace := "selected"
			if revision == sha {
				namespace = "event"
			}
			args = append(args, "+"+revision+":refs/buildkite-gha/"+namespace)
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
	revision, branch := checkoutSelectedRevision(inputs, sha)
	if !branch {
		return revision
	}
	return "+refs/heads/" + revision + ":refs/remotes/origin/" + revision
}

func checkoutRevision(inputs map[string]string, sha string) string {
	revision, branch := checkoutSelectedRevision(inputs, sha)
	if !branch {
		return revision
	}
	return "refs/remotes/origin/" + revision
}

func checkoutSelectedRevision(inputs map[string]string, sha string) (revision string, branch bool) {
	ref := checkoutInput(inputs, "ref")
	if ref == "" || ref == sha {
		return sha, false
	}
	if actionintegration.ValidCheckoutSHA(ref) {
		return ref, false
	}
	return strings.TrimPrefix(ref, "refs/heads/"), true
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

func repositoryProviderCheckoutCredentialArgs(base []string, agent, host string) []string {
	return append(append([]string(nil), base...),
		"-c", "credential.https://"+host+".useHttpPath=true",
		"-c", "http.followRedirects=false",
		"-c", "credential.https://"+host+".helper="+agentGitCredentialHelperCommand(agent),
	)
}

func (r Runner) runRepositoryProviderCheckoutFetch(ctx context.Context, processor *commandProcessor, workspace string, env map[string]string, git string, base, fetchArgs []string, credentialHost string) error {
	credentials := r.RepositoryCredentials
	if credentials == nil || credentials.Agent == "" || !filepath.IsAbs(credentials.Agent) {
		return fmt.Errorf("repository-provider credentials were not resolved before workflow execution")
	}
	if credentialHost != "github.com" && credentialHost != "origin.cursor.com" {
		return fmt.Errorf("repository-provider credentials require a supported event repository host")
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
	credentialArgs := repositoryProviderCheckoutCredentialArgs(base, credentials.Agent, credentialHost)
	cmd := exec.Command(git, append(credentialArgs, fetchArgs...)...)
	cmd.Dir = workspace
	cmd.Env = processEnv(credentialEnv)
	return processor.scrubError(r.runStreamingCommand(ctx, processor, cmd))
}
