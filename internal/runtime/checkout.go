package runtime

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
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
	result := newResult()
	const adapter = "checkout adapter"
	credentialed := job.HasCapability("provider-token-read") && r.RepositoryCredentials != nil
	url, credentialHost, validProvider := checkoutRepositoryURL(job.Event.Provider, job.Event.Repository)
	if !validProvider || !actionintegration.ValidCheckoutSHA(job.Event.SHA) {
		return result, fmt.Errorf("%s requires a valid GitHub or Origin event repository and exact SHA; other event sources are unsupported", adapter)
	}
	if err := actionintegration.ValidateCheckoutInputs(commit, inputs, job.Event.Repository, job.Event.SHA); err != nil {
		return result, fmt.Errorf("%s: %w", adapter, err)
	}
	inputs = checkoutInputsWithReleaseDefaults(commit, inputs)
	lfs := checkoutInputTrue(checkoutInput(inputs, "lfs"))
	if lfs && (r.GitLFS == "" || !filepath.IsAbs(r.GitLFS)) {
		return result, fmt.Errorf("checkout adapter requires Git LFS to be resolved before workflow execution")
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
	if lfs && (!filepath.IsAbs(git) || filepath.Base(git) != "git") {
		return result, fmt.Errorf("checkout adapter requires Git LFS to use a canonical Git executable named git")
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
	if !lfs {
		env["GIT_LFS_SKIP_SMUDGE"] = "1"
	} else {
		// Git LFS launches Git by name. Restrict its lookup to the directory
		// containing the executable pinned before workflow action hooks.
		env["PATH"] = strings.Join([]string{filepath.Dir(git), "/usr/bin", "/bin"}, string(os.PathListSeparator))
	}
	base := checkoutGitBaseArgs()
	runWithBase := func(runEnv map[string]string, commandBase []string, withCredentials bool, args ...string) error {
		if withCredentials {
			if err := r.runRepositoryProviderCheckoutGit(ctx, processor, checkoutDirectory, runEnv, git, commandBase, args, credentialHost); err != nil {
				return fmt.Errorf("%s git %s: %w", adapter, args[0], err)
			}
			return nil
		}
		if err := r.runStreaming(ctx, processor, checkoutDirectory, runEnv, git, append(commandBase, args...)...); err != nil {
			return fmt.Errorf("%s git %s: %w", adapter, args[0], err)
		}
		return nil
	}
	run := func(runEnv map[string]string, withCredentials bool, args ...string) error {
		return runWithBase(runEnv, base, withCredentials, args...)
	}
	runLFS := func(runEnv map[string]string, withCredentials bool, args ...string) error {
		lfsEnv := checkoutGitConfigEnvironment(runEnv, base)
		if withCredentials {
			if err := r.runRepositoryProviderCheckoutLFS(ctx, processor, checkoutDirectory, lfsEnv, r.GitLFS, args, credentialHost); err != nil {
				return fmt.Errorf("%s git lfs: %w", adapter, err)
			}
			return nil
		}
		if err := r.runStreaming(ctx, processor, checkoutDirectory, lfsEnv, r.GitLFS, args...); err != nil {
			return fmt.Errorf("%s git lfs: %w", adapter, err)
		}
		return nil
	}
	if err := run(env, false, "init", "--template=", "."); err != nil {
		return result, err
	}
	if err := run(env, false, "remote", "add", "origin", url); err != nil {
		return result, err
	}
	if lfs {
		if err := runLFS(env, false, "install", "--local", "--skip-repo"); err != nil {
			return result, err
		}
	}
	fetchArgs := checkoutFetchArgs(inputs, job.Event.SHA)
	if err := run(env, credentialed, fetchArgs...); err != nil {
		return result, err
	}
	checkoutTarget := checkoutRevision(inputs, job.Event.SHA)
	sparse := checkoutSparsePatterns(inputs)
	if lfs && len(sparse) == 0 {
		if err := runLFS(env, credentialed, "fetch", "origin", checkoutTarget); err != nil {
			return result, err
		}
	}
	if err := configureSparseCheckout(checkoutDirectory, inputs, func(input string, args ...string) error {
		cmd := exec.Command(git, append(base, args...)...)
		cmd.Dir = checkoutDirectory
		cmd.Env = processEnv(env)
		cmd.Stdin = strings.NewReader(input)
		if err := r.runStreamingCommand(ctx, processor, cmd); err != nil {
			return fmt.Errorf("%s git %s: %w", adapter, args[0], err)
		}
		return nil
	}); err != nil {
		return result, fmt.Errorf("%s sparse checkout: %w", adapter, err)
	}
	checkoutBase := base
	if lfs {
		checkoutBase = checkoutGitLFSFilterArgs(base, r.GitLFS)
	}
	if err := runWithBase(env, checkoutBase, credentialed && checkoutFilter(inputs) != "", "checkout", "--detach", checkoutTarget); err != nil {
		return result, err
	}
	mode := checkoutSubmoduleMode(inputs)
	if mode != "" {
		submoduleBase := base
		if lfs {
			submoduleBase = checkoutGitLFSFilterArgs(base, r.GitLFS)
		}
		if err := r.runCheckoutSubmodules(ctx, processor, checkoutDirectory, git, env, submoduleBase, checkoutFetchDepth(inputs), mode == "recursive", credentialed, credentialHost); err != nil {
			return result, fmt.Errorf("%s submodules: %w", adapter, err)
		}
	}
	head, err := os.ReadFile(filepath.Join(checkoutDirectory, ".git", "HEAD"))
	headSHA := strings.TrimSpace(string(head))
	if err != nil || !actionintegration.ValidCheckoutSHA(headSHA) || actionintegration.ValidCheckoutSHA(checkoutTarget) && headSHA != checkoutTarget {
		return result, fmt.Errorf("%s did not produce the requested detached revision", adapter)
	}
	setCheckoutOutputs(result.Outputs, commit, checkoutRefOutput(inputs, job.Event.Ref), headSHA)
	return result, nil
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

func validateCheckoutRefProvenance(sourceInputs []program.Binding, evaluatedInputs map[string]string, eventSHA string) error {
	for _, input := range sourceInputs {
		if !strings.EqualFold(input.Name, "ref") {
			continue
		}
		root, path, err := program.StaticReference(input.Value)
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

func checkoutGitLFSFilterArgs(base []string, gitLFS string) []string {
	executable := checkoutGitLFSExecutable(gitLFS)
	return append(append([]string(nil), base...),
		"-c", "filter.lfs.clean="+executable+" clean -- %f",
		"-c", "filter.lfs.smudge="+executable+" smudge -- %f",
		"-c", "filter.lfs.process="+executable+" filter-process",
		"-c", "filter.lfs.required=true",
	)
}

func checkoutGitLFSExecutable(gitLFS string) string {
	return "'" + strings.ReplaceAll(gitLFS, "'", `'\''`) + "'"
}

func checkoutGitConfigEnvironment(env map[string]string, args []string) map[string]string {
	configured := cloneStrings(env)
	count, _ := strconv.Atoi(configured["GIT_CONFIG_COUNT"])
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		key, value, ok := strings.Cut(args[i+1], "=")
		if !ok {
			continue
		}
		configured[fmt.Sprintf("GIT_CONFIG_KEY_%d", count)] = key
		configured[fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)] = value
		count++
		i++
	}
	configured["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
	return configured
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
	for line := range strings.SplitSeq(strings.TrimSuffix(string(p), "\n"), "\n") {
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

func (r Runner) runCheckoutSubmodules(ctx context.Context, processor *commandProcessor, workspace string, git string, env map[string]string, base []string, depth string, recursive, credentialed bool, credentialHost string) error {
	policy := append(append([]string{}, base...), "-c", "url.https://github.com/.insteadOf=git@github.com:")
	run := func(withCredentials bool, args ...string) error {
		if withCredentials {
			if err := r.runRepositoryProviderCheckoutGit(ctx, processor, workspace, env, git, policy, args, credentialHost); err != nil {
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
	if depth != "0" {
		updateArgs = append(updateArgs, "--depth="+depth)
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
	parent := workspace
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, err := os.Lstat(parent)
		switch {
		case os.IsNotExist(err):
			continue
		case err != nil:
			return "", err
		case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("checkout path %q has a non-directory or symbolic-link parent", path)
		}
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	return directory, nil
}

func checkoutSparsePatterns(inputs map[string]string) []string {
	value := checkoutInput(inputs, "sparse-checkout")
	if value == "" {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			patterns = append(patterns, line)
		}
	}
	return patterns
}

func configureSparseCheckout(checkoutDirectory string, inputs map[string]string, run func(string, ...string) error) error {
	patterns := checkoutSparsePatterns(inputs)
	if len(patterns) == 0 {
		return nil
	}
	if checkoutInputTrue(checkoutInput(inputs, "sparse-checkout-cone-mode")) || checkoutInput(inputs, "sparse-checkout-cone-mode") == "" {
		return run(strings.Join(patterns, "\n")+"\n", "sparse-checkout", "set", "--stdin")
	}
	if err := run("", "config", "core.sparseCheckout", "true"); err != nil {
		return err
	}
	info := filepath.Join(checkoutDirectory, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(info, "sparse-checkout"), []byte("\n"+strings.Join(patterns, "\n")+"\n"), 0o600)
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
	filter := checkoutFilter(inputs)
	if filter != "" {
		args = append(args, "--filter="+filter)
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

func checkoutFilter(inputs map[string]string) string {
	if filter := checkoutInput(inputs, "filter"); filter != "" {
		return filter
	}
	if len(checkoutSparsePatterns(inputs)) != 0 {
		return "blob:none"
	}
	return ""
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

func (r Runner) runRepositoryProviderCheckoutGit(ctx context.Context, processor *commandProcessor, workspace string, env map[string]string, git string, base, commandArgs []string, credentialHost string) error {
	credentialEnv, err := r.repositoryProviderCheckoutCredentialEnvironment(processor, env, credentialHost)
	if err != nil {
		return err
	}
	credentialArgs := repositoryProviderCheckoutCredentialArgs(base, r.RepositoryCredentials.Agent, credentialHost)
	cmd := exec.Command(git, append(credentialArgs, commandArgs...)...)
	cmd.Dir = workspace
	cmd.Env = processEnv(credentialEnv)
	return processor.scrubError(r.runStreamingCommand(ctx, processor, cmd))
}

func (r Runner) runRepositoryProviderCheckoutLFS(ctx context.Context, processor *commandProcessor, workspace string, env map[string]string, gitLFS string, commandArgs []string, credentialHost string) error {
	credentialEnv, err := r.repositoryProviderCheckoutCredentialEnvironment(processor, env, credentialHost)
	if err != nil {
		return err
	}
	credentialArgs := repositoryProviderCheckoutCredentialArgs(nil, r.RepositoryCredentials.Agent, credentialHost)
	credentialEnv = checkoutGitConfigEnvironment(credentialEnv, credentialArgs)
	cmd := exec.Command(gitLFS, commandArgs...)
	cmd.Dir = workspace
	cmd.Env = processEnv(credentialEnv)
	return processor.scrubError(r.runStreamingCommand(ctx, processor, cmd))
}

func (r Runner) repositoryProviderCheckoutCredentialEnvironment(processor *commandProcessor, env map[string]string, credentialHost string) (map[string]string, error) {
	credentials := r.RepositoryCredentials
	if credentials == nil || credentials.Agent == "" || !filepath.IsAbs(credentials.Agent) {
		return nil, fmt.Errorf("repository-provider credentials were not resolved before workflow execution")
	}
	if credentialHost != "github.com" && credentialHost != "origin.cursor.com" {
		return nil, fmt.Errorf("repository-provider credentials require a supported event repository host")
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
	maps.Copy(credentialEnv, credentials.proxyEnvironment)
	return credentialEnv, nil
}
