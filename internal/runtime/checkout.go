package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"golang.org/x/sys/unix"
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
		"GIT_CONFIG_GLOBAL":      filepath.Join(workspace, ".no-global-gitconfig"),
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
		state := checkoutSubmoduleState{runner: r, processor: processor, ctx: ctx, git: git, env: env, base: base, depthOne: checkoutFetchDepth(inputs) != "0", recursive: mode == "recursive", allowProviderCredentials: credentialed}
		if err := state.materialize(workspace, job.Event.SHA, "https://github.com/"+job.Event.Repository+".git", 0); err != nil {
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

type checkoutSubmodule struct{ name, path, url, oid string }
type checkoutSubmoduleState struct {
	runner                   Runner
	processor                *commandProcessor
	ctx                      context.Context
	git                      string
	env                      map[string]string
	base                     []string
	depthOne, recursive      bool
	allowProviderCredentials bool
	// fetchURL is used by tests to route a validated GitHub URL to a local fixture.
	fetchURL      func(string) string
	selected      int
	manifestBytes int64
}

func (s *checkoutSubmoduleState) output(dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(s.ctx, s.git, append(append([]string{}, s.base...), args...)...)
	cmd.Dir, cmd.Env = dir, processEnv(s.env)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

func (s *checkoutSubmoduleState) stream(dir string, credentialed bool, args ...string) error {
	if credentialed {
		credentialArgs := append(append([]string{}, s.base...), "-c", "credential.useHttpPath=true", "-c", "http.followRedirects=false")
		return s.runner.runRepositoryProviderCheckoutFetch(s.ctx, s.processor, dir, s.env, s.git, credentialArgs, args)
	}
	if err := s.runner.runStreaming(s.ctx, s.processor, dir, s.env, s.git, append(s.base, args...)...); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

func (s *checkoutSubmoduleState) materialize(parent, commit, parentURL string, depth int) (retErr error) {
	if depth > 8 {
		return fmt.Errorf("recursive submodule depth exceeds 8")
	}
	modules, err := s.manifest(parent, commit, parentURL)
	if err != nil || len(modules) == 0 {
		return err
	}
	if depth >= 8 {
		return fmt.Errorf("recursive submodule depth exceeds 8")
	}
	for _, module := range modules {
		if err := preflightCheckoutSubmoduleDestination(parent, module.path); err != nil {
			return err
		}
	}
	stageRoot, err := os.MkdirTemp(parent, ".buildkite-gha-submodules-")
	if err != nil {
		return err
	}
	if err := os.Chmod(stageRoot, 0o700); err != nil {
		_ = os.RemoveAll(stageRoot)
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(stageRoot)) }()
	for i := range modules {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		stage := filepath.Join(stageRoot, strconv.Itoa(i))
		if err := os.Mkdir(stage, 0o700); err != nil {
			return err
		}
		if err := s.stream(stage, false, "init", "--template=", "."); err != nil {
			return err
		}
		fetchURL := modules[i].url
		if s.fetchURL != nil {
			fetchURL = s.fetchURL(fetchURL)
		}
		if err := s.stream(stage, false, "remote", "add", "origin", fetchURL); err != nil {
			return err
		}
		credentialed := s.allowProviderCredentials
		if err := s.stream(stage, credentialed, checkoutSubmoduleFetchArgs(s.depthOne, modules[i].oid)...); err != nil {
			return err
		}
		typ, err := s.output(stage, "cat-file", "-t", modules[i].oid)
		if err != nil || strings.TrimSpace(string(typ)) != "commit" {
			return fmt.Errorf("submodule %q object is not a commit", modules[i].name)
		}
		if err := s.stream(stage, false, "checkout", "--detach", "--force", modules[i].oid); err != nil {
			return err
		}
		head, err := s.output(stage, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(string(head)) != modules[i].oid {
			return fmt.Errorf("submodule %q did not produce exact detached commit", modules[i].name)
		}
		if s.recursive {
			if err := s.materialize(stage, modules[i].oid, modules[i].url, depth+1); err != nil {
				return err
			}
		}
	}
	return publishCheckoutSubmodules(s.ctx, parent, stageRoot, modules)
}

func checkoutSubmoduleFetchArgs(depthOne bool, oid string) []string {
	args := []string{"fetch", "--no-tags", "--no-recurse-submodules"}
	if depthOne {
		return append(args, "--depth=1", "origin", oid)
	}
	return append(args, "origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*", "+"+oid+":refs/buildkite-gha/submodule")
}

func preflightCheckoutSubmoduleDestination(root, relative string) error {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootFD) }()
	directory, base := path.Split(relative)
	parentFD, err := openDownloadDirectoryAt(rootFD, strings.TrimSuffix(directory, "/"), false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("submodule path %q has an unsafe ancestor: %w", relative, err)
	}
	defer func() { _ = unix.Close(parentFD) }()
	_, err = checkoutEmptyDirectoryAt(parentFD, base)
	return err
}

func checkoutEmptyDirectoryAt(parentFD int, base string) (bool, error) {
	fd, err := unix.Openat(parentFD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("refusing to replace submodule path %q: %w", base, err)
	}
	f := os.NewFile(uintptr(fd), base)
	entries, readErr := f.Readdirnames(1)
	closeErr := f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return false, closeErr
	}
	if len(entries) != 0 {
		return false, fmt.Errorf("refusing to replace nonempty submodule path %q", base)
	}
	return true, nil
}

func publishCheckoutSubmodules(ctx context.Context, root, staging string, modules []checkoutSubmodule) (retErr error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open pinned checkout root: %w", err)
	}
	defer func() { _ = unix.Close(rootFD) }()
	stageFD, err := unix.Open(staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open pinned submodule staging root: %w", err)
	}
	defer func() { _ = unix.Close(stageFD) }()
	type published struct {
		index       int
		path        string
		placeholder bool
	}
	var done []published
	defer func() {
		if retErr == nil {
			return
		}
		var rollback error
		for i := len(done) - 1; i >= 0; i-- {
			p := done[i]
			directory, base := path.Split(p.path)
			parentFD, e := openDownloadDirectoryAt(rootFD, strings.TrimSuffix(directory, "/"), false)
			if e == nil {
				e = unix.Renameat(parentFD, base, stageFD, strconv.Itoa(p.index))
				_ = unix.Close(parentFD)
			}
			if e == nil && p.placeholder {
				parentFD, e = openDownloadDirectoryAt(rootFD, strings.TrimSuffix(directory, "/"), false)
				if e == nil {
					e = unix.Mkdirat(parentFD, base, 0o755)
					_ = unix.Close(parentFD)
				}
			}
			rollback = errors.Join(rollback, e)
		}
		retErr = errors.Join(retErr, rollback)
	}()
	for i, module := range modules {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, base := path.Split(module.path)
		parentFD, err := openDownloadDirectoryAt(rootFD, strings.TrimSuffix(directory, "/"), true)
		if err != nil {
			return fmt.Errorf("open pinned submodule destination %q: %w", module.path, err)
		}
		existed, err := checkoutEmptyDirectoryAt(parentFD, base)
		if err == nil && existed {
			err = unix.Unlinkat(parentFD, base, unix.AT_REMOVEDIR)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = unix.Renameat2(stageFD, strconv.Itoa(i), parentFD, base, unix.RENAME_NOREPLACE)
		}
		if err != nil && existed {
			err = errors.Join(err, unix.Mkdirat(parentFD, base, 0o755))
		}
		_ = unix.Close(parentFD)
		if err != nil {
			return fmt.Errorf("install submodule %q: %w", module.path, err)
		}
		done = append(done, published{i, module.path, existed})
	}
	return ctx.Err()
}

func (s *checkoutSubmoduleState) manifest(repo, commit, parentURL string) ([]checkoutSubmodule, error) {
	tree, err := s.output(repo, "ls-tree", "-z", "--full-tree", commit, "--", ".gitmodules")
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, nil
	}
	if bytes.Count(tree, []byte{0}) != 1 {
		return nil, fmt.Errorf("ambiguous .gitmodules tree entry")
	}
	mode, typ, oid, treePath, ok := parseCheckoutTreeRecord(tree)
	if !ok || mode != "100644" || typ != "blob" || treePath != ".gitmodules" {
		return nil, fmt.Errorf("committed .gitmodules is not a regular blob")
	}
	sizeText, err := s.output(repo, "cat-file", "-s", oid)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeText)), 10, 64)
	if err != nil || size < 0 || size > 1<<20 || s.manifestBytes+size > 4<<20 {
		return nil, fmt.Errorf(".gitmodules exceeds manifest size bounds")
	}
	s.manifestBytes += size
	contents, err := s.output(repo, "cat-file", "blob", oid)
	if err != nil || int64(len(contents)) != size {
		return nil, fmt.Errorf("read bounded .gitmodules blob: %w", err)
	}
	if !utf8.Valid(contents) || bytes.IndexByte(contents, 0) >= 0 {
		return nil, fmt.Errorf(".gitmodules is not valid NUL-free UTF-8")
	}
	tmp, err := os.CreateTemp(repo, ".buildkite-gha-gitmodules-")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if _, err = tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err = tmp.Close(); err != nil {
		return nil, err
	}
	parsed, err := s.output(repo, "config", "--no-includes", "--file", name, "--null", "--list")
	if err != nil {
		return nil, fmt.Errorf("parse .gitmodules: %w", err)
	}
	type pair struct {
		name, path, url   string
		seenPath, seenURL bool
	}
	byName := map[string]*pair{}
	foldedNames := map[string]bool{}
	for _, record := range bytes.Split(parsed, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\n'}, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed .gitmodules config output")
		}
		key, value := string(parts[0]), string(parts[1])
		if strings.IndexFunc(key+value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return nil, fmt.Errorf("control character in .gitmodules")
		}
		if !strings.HasPrefix(key, "submodule.") {
			return nil, fmt.Errorf("unsupported .gitmodules key %q", key)
		}
		remainder := strings.TrimPrefix(key, "submodule.")
		var n, field string
		switch {
		case strings.HasSuffix(remainder, ".path"):
			n, field = strings.TrimSuffix(remainder, ".path"), "path"
		case strings.HasSuffix(remainder, ".url"):
			n, field = strings.TrimSuffix(remainder, ".url"), "url"
		default:
			return nil, fmt.Errorf("unsupported .gitmodules key %q", key)
		}
		if n == "" || len(n) > 255 || !utf8.ValidString(n) {
			return nil, fmt.Errorf("invalid submodule name")
		}
		folded := strings.ToLower(n)
		p := byName[n]
		if p == nil {
			if foldedNames[folded] {
				return nil, fmt.Errorf("case-colliding submodule names")
			}
			foldedNames[folded] = true
			p = &pair{name: n}
			byName[n] = p
		}
		if field == "path" {
			if p.seenPath {
				return nil, fmt.Errorf("duplicate submodule path key")
			}
			p.path, p.seenPath = value, true
		} else {
			if p.seenURL {
				return nil, fmt.Errorf("duplicate submodule URL key")
			}
			p.url, p.seenURL = value, true
		}
	}
	if len(byName) > 128 || s.selected+len(byName) > 256 {
		return nil, fmt.Errorf("submodule entry bound exceeded")
	}
	modules := make([]checkoutSubmodule, 0, len(byName))
	for _, p := range byName {
		if !p.seenPath || !p.seenURL {
			return nil, fmt.Errorf("submodule %q requires exactly one path and URL", p.name)
		}
		if err := validateCheckoutSubmodulePath(p.path); err != nil {
			return nil, fmt.Errorf("submodule %q: %w", p.name, err)
		}
		u, err := canonicalCheckoutSubmoduleURL(p.url, parentURL)
		if err != nil {
			return nil, fmt.Errorf("submodule %q: %w", p.name, err)
		}
		link, err := s.output(repo, "ls-tree", "-z", "--full-tree", commit, "--", p.path)
		if err != nil {
			return nil, err
		}
		mode, typ, oid, linkPath, ok := parseCheckoutTreeRecord(link)
		if !ok || mode != "160000" || typ != "commit" || linkPath != p.path || oid == strings.Repeat("0", 40) {
			return nil, fmt.Errorf("submodule %q path is not an exact gitlink", p.name)
		}
		modules = append(modules, checkoutSubmodule{name: p.name, path: p.path, url: u, oid: oid})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].path < modules[j].path })
	for i := range modules {
		a := strings.ToLower(modules[i].path)
		for j := i + 1; j < len(modules); j++ {
			b := strings.ToLower(modules[j].path)
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return nil, fmt.Errorf("colliding submodule paths")
			}
		}
	}
	s.selected += len(modules)
	return modules, nil
}

func parseCheckoutTreeRecord(record []byte) (mode, typ, oid, name string, ok bool) {
	if len(record) == 0 || record[len(record)-1] != 0 || bytes.Count(record, []byte{0}) != 1 {
		return
	}
	header, pathBytes, found := bytes.Cut(record[:len(record)-1], []byte{'\t'})
	if !found {
		return
	}
	fields := bytes.Split(header, []byte{' '})
	if len(fields) != 3 || !checkoutSHAPattern.Match(fields[2]) {
		return
	}
	return string(fields[0]), string(fields[1]), string(fields[2]), string(pathBytes), true
}

func validateCheckoutSubmodulePath(value string) error {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "-") || path.IsAbs(value) || filepath.VolumeName(value) != "" || strings.HasPrefix(value, "//") {
		return errors.New("unsafe submodule path")
	}
	parts := strings.Split(value, "/")
	if len(parts) > 256 {
		return errors.New("submodule path has too many components")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") || strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") || strings.IndexFunc(part, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return errors.New("unsafe submodule path component")
		}
	}
	return nil
}

func canonicalCheckoutSubmoduleURL(raw, parent string) (string, error) {
	if raw == "" || len(raw) > 4096 || !utf8.ValidString(raw) || strings.Contains(raw, "\\") || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", errors.New("unsafe submodule URL")
	}
	if strings.HasPrefix(raw, "git@github.com:") && strings.Count(raw, "@") == 1 {
		raw = "https://github.com/" + strings.TrimPrefix(raw, "git@github.com:")
	} else if strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") {
		base, err := url.Parse(parent)
		if err != nil {
			return "", errors.New("invalid parent URL")
		}
		rel, err := url.Parse(raw)
		if err != nil {
			return "", errors.New("invalid relative URL")
		}
		base.Path += "/"
		raw = base.ResolveReference(rel).String()
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host != "github.com" || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.Contains(raw, "%") {
		return "", errors.New("only canonical github.com HTTPS submodule URLs are supported")
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segments) != 2 || !checkoutRepositoryPattern.MatchString(strings.Join(segments, "/")) || strings.HasSuffix(segments[1], ".git.git") {
		return "", errors.New("invalid GitHub repository URL")
	}
	repository := segments[0] + "/" + strings.TrimSuffix(segments[1], ".git")
	if !validCheckoutRepository(repository) {
		return "", errors.New("invalid GitHub repository URL")
	}
	segments[1] = strings.TrimSuffix(segments[1], ".git") + ".git"
	return "https://github.com/" + strings.Join(segments, "/"), nil
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
		"-c", "http.followRedirects=false",
		"-c", "credential.helper="+agentGitCredentialHelperCommand(credentials.Agent),
	)
	cmd := exec.Command(git, append(credentialArgs, fetchArgs...)...)
	cmd.Dir = workspace
	cmd.Env = processEnv(credentialEnv)
	return processor.scrubError(r.runStreamingCommand(ctx, processor, cmd))
}
