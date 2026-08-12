// Package runtime executes explicitly resolved GitHub Actions action steps.
package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	defaultCleanupTimeout    = 10 * time.Second
	defaultPostActionTimeout = 10 * time.Minute
	// Match actions/runner ProcessInvoker's graceful cancellation windows.
	defaultInterruptGrace = 7500 * time.Millisecond
	defaultTerminateGrace = 2500 * time.Millisecond
	maxStreamLineBytes    = 1024 * 1024
	// Match Buildkite's maximum annotation body size.
	maxJobAnnotationBytes = 1024 * 1024
	maxJobSummaryBytes    = maxJobAnnotationBytes

	jobSummaryTruncationNotice       = "\n\n---\n_Job summary truncated at the 1 MiB limit._\n"
	workflowCommandTruncationNotice  = "\n\n---\n_Workflow command annotations truncated at the 1 MiB limit._\n"
	workflowWarningAnnotationHeading = "<h2 class=\"h4 mb2\">GitHub Actions warnings</h2>\n"
	workflowErrorAnnotationHeading   = "<h2 class=\"h4 mb2\">GitHub Actions errors</h2>\n"
	workflowCommandListHeading       = "<div class=\"mb2\">\n"
	workflowCommandListEnd           = "</div>\n"
)

// Runner executes verified actions using explicitly configured host tools.
type Runner struct {
	Stdout                io.Writer
	Stderr                io.Writer
	Node16                string
	Node20                string
	Node24                string
	ManagedNodeRoot       string
	Mise                  string
	ResolveMise           func(context.Context) (string, error)
	MiseDataDir           string
	ToolCache             string
	Docker                string
	RuntimeExecutable     string
	Git                   string
	CleanupTimeout        time.Duration
	PostActionTimeout     time.Duration
	InterruptGrace        time.Duration
	TerminateGrace        time.Duration
	Secrets               SecretResolver
	Redactor              Redactor
	Actions               ActionMaterializer
	Artifacts             ArtifactStore
	Cache                 CacheCredentialProvider
	RepositoryCredentials *AgentRepositoryCredentials
	WorkflowToken         WorkflowTokenProvider
	runnerTemp            string
	implicitJobPATH       string
	explicitJobPATH       bool
	jobContainer          *jobContainerBackend
	jobDocker             *jobContainerBackend
	nodeVerification      *managedNodeVerification
	nodeDigests           map[int]string
	artifactRegistry      *artifactRegistry
	node16Warnings        *node16DeprecationWarnings
}

func resolveHostExecutableBeforeWorkflow(configured, fallback, label string) (string, error) {
	candidate := configured
	if candidate == "" {
		candidate = fallback
	}
	resolved, err := exec.LookPath(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve %s before workflow execution: %w", label, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve %s absolute path before workflow execution: %w", label, err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s before workflow execution: %w", label, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s must be a real executable resolved before workflow execution", label)
	}
	return resolved, nil
}

type managedNodeVerification struct {
	mu    sync.Mutex
	paths map[int]string
}

func (r Runner) explicitNode(major int) string {
	switch major {
	case 16:
		return r.Node16
	case 20:
		return r.Node20
	case 24:
		return r.Node24
	default:
		return ""
	}
}

func (r *Runner) setExplicitNode(major int, path string) {
	switch major {
	case 16:
		r.Node16 = path
	case 20:
		r.Node20 = path
	case 24:
		r.Node24 = path
	}
}

// javaScriptAction is an already-resolved local JavaScript action.
type javaScriptAction struct {
	Name   string
	Path   string
	Pre    string
	Main   string
	Post   string
	Inputs map[string]string
	Env    map[string]string
	Cache  bool

	nodeMajor       int
	reference       string
	jobStatusInputs []string
}

// dockerAction is an already-resolved local Docker action.
type dockerAction struct {
	Name         string
	Path         string
	SourceRoot   string
	SourceDigest string
	Dockerfile   string
	Workspace    string
	Env          map[string]string
	runnerTemp   string
	explicitPATH bool
}

// Result contains file-command effects produced by an action or lifecycle.
type Result struct {
	Outputs   map[string]string
	Env       map[string]string
	State     map[string]string
	Summary   string
	Paths     []string
	Artifacts []transport.ResultArtifact

	pathBase         string
	pathBaseSet      bool
	summaryTruncated bool
}

// appendJobSummary is the only summary aggregation path. It preserves a
// deterministic UTF-8 prefix and reserves room for a final truncation notice.
func appendJobSummary(summary *string, truncated *bool, next string, nextTruncated bool) {
	appendBoundedText(summary, truncated, next, nextTruncated, maxJobSummaryBytes, jobSummaryTruncationNotice)
}

func appendBoundedText(value *string, truncated *bool, next string, nextTruncated bool, limit int, notice string) {
	if *truncated || (next == "" && !nextTruncated) {
		return
	}
	if !nextTruncated && len(*value) <= limit && len(next) <= limit-len(*value) {
		*value += next
		return
	}

	prefixLimit := limit - len(notice)
	if len(*value) > prefixLimit {
		*value = utf8Prefix(*value, prefixLimit)
	} else {
		*value += utf8Prefix(next, prefixLimit-len(*value))
	}
	*truncated = true
}

func utf8Prefix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func finalizeJobSummary(summary string, truncated bool) string {
	if truncated {
		return summary + jobSummaryTruncationNotice
	}
	return summary
}

func boundJobSummary(summary string, truncated bool) (string, bool) {
	var bounded string
	var boundedTruncated bool
	appendJobSummary(&bounded, &boundedTruncated, summary, truncated)
	return bounded, boundedTruncated
}

// runDockerAction builds and executes an explicitly resolved local Docker
// action, creating an isolated workspace when the caller supplies none.
func (r Runner) runDockerAction(ctx context.Context, action dockerAction) (result Result, err error) {
	callerWorkspace := action.Workspace != ""
	if !callerWorkspace {
		action.Workspace, err = os.MkdirTemp("", "buildkite-gha-workspace-")
		if err != nil {
			return newResult(), fmt.Errorf("create workspace: %w", err)
		}
		defer func() { _ = os.RemoveAll(action.Workspace) }()
	}
	abs, e := filepath.Abs(action.Workspace)
	if e != nil {
		return newResult(), fmt.Errorf("resolve workspace: %w", e)
	}
	abs, e = filepath.EvalSymlinks(abs)
	if e != nil {
		return newResult(), fmt.Errorf("canonicalize workspace: %w", e)
	}
	action.Workspace = abs
	_, action.explicitPATH = action.Env["PATH"]
	workspace, e := os.Open(abs)
	if e != nil {
		return newResult(), e
	}
	defer func() { err = errors.Join(err, workspace.Close()) }()
	workspaceInfo, e := workspace.Stat()
	if e != nil {
		return newResult(), e
	}
	if e := workspace.Chmod(0o777); e != nil {
		return newResult(), fmt.Errorf("make Docker workspace writable: %w", e)
	}
	if callerWorkspace {
		defer func() { err = errors.Join(err, workspace.Chmod(workspaceInfo.Mode().Perm())) }()
	}
	temp, e := os.MkdirTemp("", "buildkite-gha-runner-")
	if e != nil {
		return newResult(), e
	}
	defer func() { _ = os.RemoveAll(temp) }()
	temp, e = filepath.EvalSymlinks(temp)
	if e != nil {
		return newResult(), fmt.Errorf("canonicalize runner temp: %w", e)
	}
	if e := os.Chmod(temp, 0o777); e != nil {
		return newResult(), fmt.Errorf("make Docker runner temp writable: %w", e)
	}
	action.runnerTemp = temp
	return r.runDocker(ctx, newCommandProcessor(r.stdout(), r.stderr()), action)
}

func (r Runner) runDocker(ctx context.Context, processor *commandProcessor, action dockerAction) (result Result, err error) {
	result = newResult()
	if action.runnerTemp == "" {
		action.runnerTemp = r.runnerTemp
	}
	action.explicitPATH = action.explicitPATH || r.explicitJobPATH
	if action.Workspace == "" || action.runnerTemp == "" {
		return result, errors.New("docker workspace and runner temp are required")
	}
	if r.jobContainer != nil && (action.Workspace != r.jobContainer.workspace || action.runnerTemp != r.jobContainer.temp) {
		return result, errors.New("docker action workspace and runner temp must match the job container's owned host paths")
	}
	docker := r.Docker
	if docker == "" {
		docker, err = exec.LookPath("docker")
		if err != nil {
			return result, fmt.Errorf("discover Docker: %w", err)
		}
	}
	if action.Dockerfile != "" && action.Dockerfile != "Dockerfile" {
		return result, fmt.Errorf("docker action requires fixed Dockerfile")
	}
	sourceRoot := action.SourceRoot
	if sourceRoot == "" {
		sourceRoot = action.Path
	}
	expected := action.SourceDigest
	if expected == "" {
		expected, err = source.DigestTree(sourceRoot)
		if err != nil {
			return result, fmt.Errorf("digest Docker action source: %w", err)
		}
	}
	stage, err := stageDockerSource(sourceRoot, action.Path, expected)
	if err != nil {
		return result, err
	}
	defer func() { _ = os.RemoveAll(stage.root) }()
	dockerConfig, err := os.MkdirTemp("", "buildkite-gha-docker-config-")
	if err != nil {
		return result, fmt.Errorf("create private Docker configuration: %w", err)
	}
	if err := os.Chmod(dockerConfig, 0o700); err != nil {
		_ = os.RemoveAll(dockerConfig)
		return result, err
	}
	defer func() { _ = os.RemoveAll(dockerConfig) }()
	dockerEnv := map[string]string{"DOCKER_CONFIG": dockerConfig}
	inspection, inspectErr := boundedDockerOutput(ctx, dockerEnv, docker, "buildx", "inspect", "default")
	driver := dockerBuilderDriver(inspection)
	if inspectErr != nil || driver != "docker" {
		if inspectErr != nil {
			return result, fmt.Errorf("inspect default Docker builder: %w", inspectErr)
		}
		if driver == "" {
			return result, fmt.Errorf("could not determine default Docker builder driver from inspect output")
		}
		return result, fmt.Errorf("default Docker builder has non-local driver %q", driver)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, fmt.Errorf("create Docker invocation identity: %w", err)
	}
	id := hex.EncodeToString(nonce[:])
	owner := "com.buildkite.gha.owner=" + id
	image, container := "buildkite-gha-image-"+id, "buildkite-gha-container-"+id
	built, ran := false, false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), r.cleanupTimeout())
		defer cancel()
		var cleanupErr error
		if ran {
			out, queryErr := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "ps", "--all", "--quiet", "--filter", "label="+owner, "--filter", "name=^/"+container+"$")
			containerMayExist := strings.TrimSpace(out) != ""
			if queryErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("query owned Docker container: %w", queryErr))
				containerMayExist = true
			}
			if containerMayExist {
				if _, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "stop", "--time", "2", container); e != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop owned Docker container: %w", e))
				}
				if _, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "rm", "--force", container); e != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove owned Docker container: %w", e))
				}
			}
		}
		if built {
			out, queryErr := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "image", "ls", "--all", "--quiet", "--filter", "label="+owner, image)
			imageMayExist := strings.TrimSpace(out) != ""
			if queryErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("query owned Docker image: %w", queryErr))
				imageMayExist = true
			}
			if imageMayExist {
				if _, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, "image", "rm", "--force", image); e != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove owned Docker image: %w", e))
				}
			}
		}
		for _, query := range [][]string{{"ps", "--all", "--quiet", "--filter", "label=" + owner}, {"image", "ls", "--all", "--quiet", "--filter", "label=" + owner}} {
			out, e := boundedDockerOutput(cleanupCtx, dockerEnv, docker, query...)
			if e != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("verify owned Docker cleanup: %w", e))
			} else if strings.TrimSpace(out) != "" {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("owned Docker resources remain after cleanup"))
			}
		}
		err = errors.Join(err, cleanupErr)
	}()
	buildArgs := []string{"buildx", "build", "--builder", "default", "--load", "--tag", image, "--label", owner, "--file", filepath.Join(stage.action, "Dockerfile"), stage.action}
	built = true // A failed build may still have created the tagged image.
	if err := r.runStreaming(ctx, processor, "", dockerEnv, docker, buildArgs...); err != nil {
		return result, fmt.Errorf("build Docker action %q: %w", action.Name, err)
	}

	files, err := newCommandFiles()
	if err != nil {
		return result, err
	}
	defer func() { _ = files.cleanup() }()
	if err := files.allowContainerWrites(); err != nil {
		return result, err
	}
	if err := validateDockerMountPath(files.dir); err != nil {
		return result, err
	}
	if action.Workspace != "" {
		if err := validateDockerMountPath(action.Workspace); err != nil {
			return result, err
		}
		if err := validateDockerMountPath(action.runnerTemp); err != nil {
			return result, err
		}
	}
	cacheEnv, cacheErr := r.cacheActionEnvironment(ctx, processor)
	if cacheErr != nil && ctx.Err() != nil {
		return result, ctx.Err()
	}
	cacheToken := ""
	if cacheErr == nil {
		cacheToken = cacheEnv["ACTIONS_RUNTIME_TOKEN"]
	}
	args := []string{"run", "--name", container, "--label", owner, "--mount", "type=bind,source=" + files.dir + ",target=/github/file_commands"}
	if r.jobDocker != nil {
		args = append(args, "--network", r.jobDocker.network)
	}
	if action.Workspace != "" {
		args = append(args, "--mount", "type=bind,source="+action.Workspace+",target=/github/workspace", "--mount", "type=bind,source="+action.runnerTemp+",target=/github/runner_temp", "--workdir", "/github/workspace")
	}
	var siblingPaths *jobContainerBackend
	if r.jobContainer != nil {
		siblingPaths = &jobContainerBackend{mounts: []containerMount{
			{host: action.Workspace, target: "/github/workspace"},
			{host: action.runnerTemp, target: "/github/runner_temp"},
		}}
	}
	for _, name := range sortedKeys(action.Env) {
		if isCacheServiceEnvironment(name) || name == "RUNNER_TOOL_CACHE" || (name == "PATH" && !action.explicitPATH) || name == "GITHUB_WORKSPACE" || name == "RUNNER_TEMP" {
			continue
		}
		value := action.Env[name]
		if siblingPaths != nil {
			if name == "PATH" {
				value = siblingPaths.translatePATH(value)
			} else {
				value = siblingPaths.containerPath(value)
			}
		}
		args = append(args, "--env", name+"="+value)
	}
	if action.Workspace != "" {
		args = append(args, "--env", "GITHUB_WORKSPACE=/github/workspace")
	}
	args = append(args,
		"--env", "RUNNER_TEMP=/github/runner_temp",
		"--env", "GITHUB_OUTPUT=/github/file_commands/output",
		"--env", "GITHUB_ENV=/github/file_commands/env",
		"--env", "GITHUB_PATH=/github/file_commands/path",
		"--env", "GITHUB_STATE=/github/file_commands/state",
		"--env", "GITHUB_STEP_SUMMARY=/github/file_commands/summary",
	)
	dockerRunEnv := dockerEnv
	for _, name := range sortedKeys(cacheEnv) {
		args = append(args, "--env", name)
	}
	if len(cacheEnv) > 0 {
		dockerRunEnv = mergeStringMaps(dockerEnv, cacheEnv)
	}
	args = append(args, image)
	ran = true
	runErr := r.runStreaming(ctx, processor, "", dockerRunEnv, docker, args...)
	if runErr != nil {
		runErr = fmt.Errorf("run Docker action %q: %w", action.Name, runErr)
	}
	effects, fileErr := files.apply(&result, nil)
	effects.reportSummaryUploadFailure(processor)
	if fileErr != nil {
		fileErr = fmt.Errorf("process Docker action %q file commands: %w", action.Name, fileErr)
	}
	if cacheToken != "" {
		runErr = processor.scrubError(runErr)
		fileErr = processor.scrubError(fileErr)
		if resultContains(result, cacheToken) {
			result = newResult()
			return result, errors.Join(runErr, fileErr, errors.New("action runtime cache token leakage detected; action effects were discarded"))
		}
	}
	return result, errors.Join(runErr, fileErr)
}

type stagedDockerSource struct{ root, action string }

func stageDockerSource(sourceRoot, actionPath, expected string) (stagedDockerSource, error) {
	before, err := source.DigestTree(sourceRoot)
	if err != nil || before != expected {
		return stagedDockerSource{}, fmt.Errorf("docker action source digest mismatch before staging")
	}
	rel, err := filepath.Rel(sourceRoot, actionPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return stagedDockerSource{}, fmt.Errorf("docker action path escapes verified source")
	}
	root, err := os.MkdirTemp("", "buildkite-gha-docker-source-")
	if err != nil {
		return stagedDockerSource{}, err
	}
	fail := func(e error) (stagedDockerSource, error) { _ = os.RemoveAll(root); return stagedDockerSource{}, e }
	err = filepath.WalkDir(sourceRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		r, _ := filepath.Rel(sourceRoot, p)
		if r == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		to := filepath.Join(root, r)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		info, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("docker action source contains symlink or special file")
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(to, data, mode)
	})
	if err != nil {
		return fail(fmt.Errorf("stage Docker action source: %w", err))
	}
	stagedDigest, e := source.DigestTree(root)
	if e != nil || stagedDigest != expected {
		return fail(fmt.Errorf("staged Docker action digest mismatch"))
	}
	after, e := source.DigestTree(sourceRoot)
	if e != nil || after != expected {
		return fail(fmt.Errorf("docker action source mutated during staging"))
	}
	return stagedDockerSource{root: root, action: filepath.Join(root, rel)}, nil
}

func validateDockerMountPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, ",\"'\n\r\x00") {
		return fmt.Errorf("path cannot be represented by Docker mount grammar")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("docker mount path must be an existing directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("docker mount path must be an existing directory")
	}
	return nil
}

func dockerBuilderDriver(inspection string) string {
	var driver string
	for _, line := range strings.Split(inspection, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "Driver:" {
			continue
		}
		if driver != "" && driver != fields[1] {
			return ""
		}
		driver = fields[1]
	}
	return driver
}

func boundedDockerOutput(ctx context.Context, env map[string]string, docker string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = processEnv(env)
	var output strings.Builder
	w := &limitedWriter{writer: &output, remaining: 4096}
	cmd.Stdout, cmd.Stderr = w, io.Discard
	err := cmd.Run()
	if w.exceeded {
		return output.String(), fmt.Errorf("docker output exceeds limit")
	}
	return output.String(), err
}

func boundedDockerCombinedOutput(ctx context.Context, env map[string]string, docker string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = processEnv(env)
	var output strings.Builder
	w := &limitedWriter{writer: &output, remaining: 4096}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if w.exceeded {
		return output.String(), fmt.Errorf("docker output exceeds limit")
	}
	return output.String(), err
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
	exceeded  bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.exceeded = true
		if w.remaining > 0 {
			_, _ = w.writer.Write(p[:w.remaining])
			w.remaining = 0
		}
		return len(p), nil
	}
	w.remaining -= len(p)
	return w.writer.Write(p)
}

func (r Runner) runJavaScriptPhase(ctx context.Context, processor *commandProcessor, workspace, node string, action javaScriptAction, entry string, stateEnv, stateOut map[string]string, result *Result) error {
	env := mergeStringMaps(result.Env, action.Env, actionInputEnv(action.Inputs))
	if path, ok := result.Env["PATH"]; ok {
		env["PATH"] = path
	}
	env["GITHUB_ACTION_PATH"] = action.Path
	for name, value := range stateEnv {
		env["STATE_"+name] = value
	}
	env = removeCacheServiceEnvironment(env)
	if action.Cache {
		env = isolateCacheActionEnvironment(env)
	}
	cacheEnv, cacheErr := r.cacheActionEnvironment(ctx, processor)
	if cacheErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if cacheErr != nil && action.Cache {
		return fmt.Errorf("configure actions/cache v6 service: %w", cacheErr)
	}
	cacheToken := ""
	if cacheErr == nil {
		env = mergeStringMaps(env, cacheEnv)
		cacheToken = cacheEnv["ACTIONS_RUNTIME_TOKEN"]
	}
	entrypoint := filepath.Join(action.Path, entry)
	if r.jobContainer != nil {
		if err := r.jobContainer.probeNode(ctx, node, action.nodeMajor); err != nil {
			return err
		}
		absNode, err := filepath.Abs(node)
		if err != nil {
			return err
		}
		node = r.jobContainer.containerPath(absNode)
		entrypoint = r.jobContainer.containerPath(entrypoint)
	}
	name, args := node, []string{entrypoint}
	if r.node16Warnings != nil && action.nodeMajor == 16 {
		r.node16Warnings.record(action.reference)
	}
	var beforeResult Result
	var beforeState map[string]string
	if cacheToken != "" {
		beforeResult = cloneResult(*result)
		beforeState = cloneStrings(stateOut)
	}
	err := r.runProcess(ctx, processor, workspace, env, result, stateOut, name, args...)
	if cacheToken != "" {
		leaked := resultContains(*result, cacheToken)
		err = processor.scrubError(err)
		if leaked {
			*result = beforeResult
			restoreStringMap(stateOut, beforeState)
			leakErr := errors.New("action runtime cache token leakage detected; phase effects were discarded")
			if err != nil {
				leakErr = errors.Join(err, leakErr)
			}
			return fmt.Errorf("JavaScript action %q entry %q: %w", action.Name, entry, leakErr)
		}
	}
	if err != nil {
		return fmt.Errorf("JavaScript action %q entry %q: %w", action.Name, entry, err)
	}
	return nil
}

func cloneResult(result Result) Result {
	result.Outputs = cloneStrings(result.Outputs)
	result.Env = cloneStrings(result.Env)
	result.State = cloneStrings(result.State)
	result.Paths = append([]string(nil), result.Paths...)
	result.Artifacts = append([]transport.ResultArtifact(nil), result.Artifacts...)
	return result
}

func restoreStringMap(target, source map[string]string) {
	if target == nil {
		return
	}
	for name := range target {
		delete(target, name)
	}
	mergeInto(target, source)
}

func resultContains(result Result, value string) bool {
	for _, values := range []map[string]string{result.Outputs, result.Env, result.State} {
		for name, candidate := range values {
			if strings.Contains(name, value) || strings.Contains(candidate, value) {
				return true
			}
		}
	}
	if strings.Contains(result.Summary, value) || strings.Contains(result.pathBase, value) {
		return true
	}
	for _, path := range result.Paths {
		if strings.Contains(path, value) {
			return true
		}
	}
	for _, artifact := range result.Artifacts {
		if strings.Contains(artifact.Name, value) || strings.Contains(artifact.ID, value) || strings.Contains(artifact.Path, value) || strings.Contains(artifact.Digest, value) {
			return true
		}
	}
	return false
}

func (r Runner) runProcess(ctx context.Context, processor *commandProcessor, dir string, env map[string]string, result *Result, state map[string]string, name string, args ...string) error {
	var files commandFiles
	var err error
	if r.jobContainer != nil {
		files, err = newCommandFilesUnder(r.runnerTemp)
	} else {
		files, err = newCommandFiles()
	}
	if err != nil {
		return err
	}
	defer func() { _ = files.cleanup() }()
	commandPaths := map[string]string{"GITHUB_OUTPUT": files.output, "GITHUB_ENV": files.env, "GITHUB_PATH": files.path, "GITHUB_STATE": files.state, "GITHUB_STEP_SUMMARY": files.summary}
	if r.jobContainer != nil {
		if err := files.allowContainerWrites(); err != nil {
			return err
		}
		for key, path := range commandPaths {
			commandPaths[key] = r.jobContainer.containerPath(path)
		}
	}
	env = mergeStringMaps(env, commandPaths)
	var runErr error
	if r.jobContainer != nil {
		runErr = r.jobContainer.exec(ctx, r, processor, dir, env, name, args...)
	} else {
		runErr = r.runStreaming(ctx, processor, dir, env, name, args...)
	}
	effects, fileErr := files.apply(result, state)
	effects.reportSummaryUploadFailure(processor)
	if fileErr == nil && (effects.pathSet || len(effects.paths) > 0) {
		pathEnv := map[string]string{"PATH": env["PATH"]}
		if effects.pathSet {
			pathEnv["PATH"] = effects.pathBase
		}
		applyPaths(pathEnv, effects.paths)
		result.Env["PATH"] = pathEnv["PATH"]
	}
	return errors.Join(runErr, fileErr)
}

func (effects fileCommandEffects) reportSummaryUploadFailure(processor *commandProcessor) {
	if effects.summaryBytes <= maxCommandFileBytes {
		return
	}
	_ = processor.process(processor.stderr, fmt.Sprintf("GITHUB_STEP_SUMMARY upload skipped: content is %d bytes; maximum is %d bytes", effects.summaryBytes, maxCommandFileBytes))
}

func (r Runner) runStreaming(ctx context.Context, processor *commandProcessor, dir string, env map[string]string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = processEnv(env)
	return r.runStreamingCommand(ctx, processor, cmd)
}

func (r Runner) runStreamingCommand(ctx context.Context, processor *commandProcessor, cmd *exec.Cmd) error {
	configureProcessGroup(cmd)
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return err
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return err
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	defer func() {
		_ = stdout.Close()
		_ = stderr.Close()
	}()

	processDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(processDone)
	}()
	finished := make(chan struct{})
	terminationDone := make(chan struct{})
	go func() {
		defer close(terminationDone)
		terminateProcessGroup(ctx, cmd.Process.Pid, r.interruptGrace(), r.terminateGrace(), finished)
	}()

	var wg sync.WaitGroup
	streamErrs := make([]error, 2)
	stream := func(index int, label string, reader io.Reader, target io.Writer) {
		defer wg.Done()
		var commandErr error
		streamErr := streamLines(reader, func(line string) {
			commandErr = errors.Join(commandErr, processor.process(target, line))
		}, processor.suppress)
		if streamErr != nil {
			streamErr = fmt.Errorf("%s stream: %w", label, streamErr)
		}
		if commandErr != nil {
			commandErr = fmt.Errorf("%s stream: %w", label, commandErr)
		}
		streamErrs[index] = errors.Join(streamErr, commandErr)
	}
	wg.Add(2)
	go stream(0, "stdout", stdout, processor.stdout)
	go stream(1, "stderr", stderr, processor.stderr)
	wg.Wait()
	<-processDone
	close(finished)
	<-terminationDone
	if ctx.Err() != nil {
		waitErr = ctx.Err()
	}
	if waitErr != nil {
		waitErr = fmt.Errorf("process %s: %w", cmd.Path, waitErr)
	}
	return errors.Join(waitErr, streamErrs[0], streamErrs[1])
}

func (r Runner) interruptGrace() time.Duration {
	if r.InterruptGrace > 0 {
		return r.InterruptGrace
	}
	return defaultInterruptGrace
}

func (r Runner) terminateGrace() time.Duration {
	if r.TerminateGrace > 0 {
		return r.TerminateGrace
	}
	return defaultTerminateGrace
}

func streamLines(reader io.Reader, process func(string), suppress func()) error {
	buffered := bufio.NewReader(reader)
	line := make([]byte, 0, buffered.Size())
	oversized := false
	sawOversized := false
	for {
		fragment, err := buffered.ReadSlice('\n')
		if !oversized {
			contentBytes := len(line) + len(fragment)
			if err == nil {
				contentBytes--
				if len(fragment) > 1 && fragment[len(fragment)-2] == '\r' {
					contentBytes--
				}
			}
			if contentBytes > maxStreamLineBytes {
				line = line[:0]
				oversized = true
				sawOversized = true
				suppress()
			} else {
				line = append(line, fragment...)
			}
		}

		if err == nil {
			if oversized {
			} else {
				process(trimStreamLineEnding(string(line)))
			}
			line = line[:0]
			oversized = false
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if oversized {
			} else if len(line) != 0 {
				process(trimStreamLineEnding(string(line)))
			}
			if sawOversized {
				return fmt.Errorf("line exceeds %d-byte limit and was discarded", maxStreamLineBytes)
			}
			return nil
		}
		if sawOversized {
			return errors.Join(fmt.Errorf("line exceeds %d-byte limit and was discarded", maxStreamLineBytes), err)
		}
		return err
	}
}

func trimStreamLineEnding(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

const (
	Node16Version = "16.20.2"
	Node20Version = "20.20.2"
	Node24Version = "24.18.0"
	// Digests are for bin/node in the official Linux x86-64 release archives.
	node16Digest = "8440cffda5a21bf7cfda43d2c396f79777585a4c5e03ed2801fe226953a7aa11"
	node20Digest = "6295488653f0d93b0a157841746fef7e72cc4328cfb60c4bbe0ca2668a836ffd"
	node24Digest = "41a74efb34cbde5c7632cdac0cf8bd1a14d0b8d73dc1e82755014d9a9ce70f5c"
)

func nodeTool(major int) string {
	switch major {
	case 16:
		return "core:node@" + Node16Version
	case 20:
		return "core:node@" + Node20Version
	case 24:
		return "core:node@" + Node24Version
	default:
		return ""
	}
}

func (r Runner) discoverNode(ctx context.Context, major int, explicit string) (string, error) {
	if explicit != "" || r.ManagedNodeRoot != "" {
		return discoverNodeContext(ctx, major, explicit, r.ManagedNodeRoot)
	}
	tool := nodeTool(major)
	if tool == "" {
		return "", fmt.Errorf("unsupported Node runtime major %d", major)
	}
	if r.Mise == "" {
		return "", fmt.Errorf("mise is required to run JavaScript actions; no pinned runtime path was configured")
	}
	return r.installAndVerifyMiseNode(ctx, major, r.Mise)
}

func (r Runner) resolveMiseNodePath(ctx context.Context, major int) (string, error) {
	return r.discoverNode(ctx, major, "")
}

func (r Runner) miseEnv() map[string]string {
	if r.MiseDataDir == "" {
		return nil
	}
	return map[string]string{"MISE_DATA_DIR": r.MiseDataDir}
}

func (r Runner) installAndVerifyMiseNode(ctx context.Context, major int, mise string) (string, error) {
	if r.nodeVerification != nil {
		r.nodeVerification.mu.Lock()
		defer r.nodeVerification.mu.Unlock()
		if path := r.nodeVerification.paths[major]; path != "" {
			return path, nil
		}
	}
	tool := nodeTool(major)
	if err := r.installMiseNode(ctx, mise, tool); err != nil {
		return "", err
	}
	installation, node, err := r.miseNodeInstallation(ctx, major, mise)
	if err == nil {
		err = verifyManagedNodeExecutable(ctx, major, node, r.nodeDigest(major))
	}
	if err != nil && r.MiseDataDir != "" {
		if removeErr := removeManagedNodeInstallation(r.MiseDataDir, installation); removeErr != nil {
			return "", errors.Join(fmt.Errorf("cached Node %d failed validation: %w", major, err), removeErr)
		}
		if installErr := r.installMiseNode(ctx, mise, tool); installErr != nil {
			return "", fmt.Errorf("replace invalid cached %s: %w", tool, installErr)
		}
		_, node, err = r.miseNodeInstallation(ctx, major, mise)
		if err == nil {
			err = verifyManagedNodeExecutable(ctx, major, node, r.nodeDigest(major))
		}
		if err != nil {
			return "", fmt.Errorf("replacement %s failed validation: %w", tool, err)
		}
	}
	if err != nil {
		return "", err
	}
	if r.nodeVerification != nil {
		r.nodeVerification.paths[major] = node
	}
	return node, nil
}

func (r Runner) installMiseNode(ctx context.Context, mise, tool string) error {
	cmd := exec.CommandContext(ctx, mise, "--no-config", "install", tool)
	cmd.Env = processEnv(r.miseEnv())
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("install exact %s with mise: %w: %s", tool, err, strings.TrimSpace(output.String()))
	}
	return nil
}

func (r Runner) nodeDigest(major int) string {
	if digest := r.nodeDigests[major]; digest != "" {
		return digest
	}
	switch major {
	case 16:
		return node16Digest
	case 20:
		return node20Digest
	case 24:
		return node24Digest
	default:
		return ""
	}
}

func (r Runner) miseNodeInstallation(ctx context.Context, major int, mise string) (string, string, error) {
	tool := nodeTool(major)
	cmd := exec.CommandContext(ctx, mise, "--no-config", "where", tool)
	cmd.Env = processEnv(r.miseEnv())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve cached %s installation: %w: %s", tool, err, strings.TrimSpace(stderr.String()))
	}
	installation, err := filepath.Abs(strings.TrimSpace(string(out)))
	if err != nil {
		return "", "", fmt.Errorf("resolve %s installation path: %w", tool, err)
	}
	if r.MiseDataDir != "" {
		dataDir, err := filepath.Abs(r.MiseDataDir)
		if err != nil {
			return "", "", fmt.Errorf("resolve mise data directory: %w", err)
		}
		resolvedDataDir, err := filepath.EvalSymlinks(dataDir)
		if err != nil || resolvedDataDir != dataDir {
			return "", "", errors.New("mise data directory contains a symlink")
		}
		resolvedInstallation, err := filepath.EvalSymlinks(installation)
		if err != nil || resolvedInstallation != installation {
			return "", "", fmt.Errorf("mise-resolved %s installation contains a symlink", tool)
		}
		relative, err := filepath.Rel(dataDir, installation)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", "", fmt.Errorf("mise resolved %s outside its runtime-owned data directory", tool)
		}
	}
	node := filepath.Join(installation, "bin", "node")
	info, err := os.Lstat(node)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return installation, node, fmt.Errorf("mise-resolved %s executable is not a regular file", tool)
	}
	resolvedNode, err := filepath.EvalSymlinks(node)
	if err != nil || resolvedNode != node {
		return installation, node, fmt.Errorf("mise-resolved %s executable contains a symlink", tool)
	}
	return installation, node, nil
}

func removeManagedNodeInstallation(dataDir, installation string) error {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("resolve mise data directory: %w", err)
	}
	resolvedDataDir, err := filepath.EvalSymlinks(dataDir)
	if err != nil || resolvedDataDir != dataDir {
		return errors.New("refusing to remove Node installation through a symlinked mise data directory")
	}
	installation, err = filepath.Abs(installation)
	if err != nil {
		return fmt.Errorf("resolve cached Node installation: %w", err)
	}
	relative, err := filepath.Rel(dataDir, installation)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("refusing to remove Node installation outside the mise data directory")
	}
	current := dataDir
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect cached Node installation: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to remove cached Node installation through symlink %q", current)
		}
	}
	if err := os.RemoveAll(installation); err != nil {
		return fmt.Errorf("remove invalid cached Node installation: %w", err)
	}
	return nil
}

func verifyManagedNodeExecutable(ctx context.Context, major int, path, want string) error {
	if want == "" {
		return fmt.Errorf("unsupported Node runtime major %d", major)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open Node %d executable: %w", major, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("hash Node %d executable: %w", major, err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != want {
		return fmt.Errorf("node %d executable digest %s does not match expected digest %s", major, got, want)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = processEnv(nil)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("verify exact Node %d executable: %w: %s", major, err, strings.TrimSpace(stderr.String()))
	}
	var wantVersion string
	switch major {
	case 16:
		wantVersion = "v" + Node16Version
	case 20:
		wantVersion = "v" + Node20Version
	case 24:
		wantVersion = "v" + Node24Version
	default:
		wantVersion = fmt.Sprintf("v%d", major)
	}
	if strings.TrimSpace(string(out)) != wantVersion {
		return fmt.Errorf("node %d executable reported %q, want %q", major, strings.TrimSpace(string(out)), wantVersion)
	}
	return nil
}

// discoverNodeContext resolves an explicit Node binary or a binary in the
// managed runtime root, and rejects binaries that do not report the requested
// major. It deliberately does not fall back to PATH.
func discoverNodeContext(ctx context.Context, major int, explicit, managedRoot string) (string, error) {
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else if managedRoot != "" {
		name := "node"
		if runtime.GOOS == "windows" {
			name = "node.exe"
		}
		version := fmt.Sprintf("%d", major)
		candidates = append(candidates,
			filepath.Join(managedRoot, "node"+version, "bin", name),
			filepath.Join(managedRoot, "node", version, "bin", name),
			filepath.Join(managedRoot, "bin", name),
		)
	} else {
		return "", fmt.Errorf("node %d is not configured: set the matching Runner.Node field or Runner.ManagedNodeRoot", major)
	}

	var failures []string
	for _, candidate := range candidates {
		command := exec.CommandContext(ctx, candidate, "--version")
		command.Env = processEnv(nil)
		output, err := command.CombinedOutput()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		version := strings.TrimSpace(string(output))
		if strings.HasPrefix(version, fmt.Sprintf("v%d.", major)) {
			return candidate, nil
		}
		failures = append(failures, fmt.Sprintf("%s: reported %q", candidate, version))
	}
	return "", fmt.Errorf("node %d discovery failed: %s", major, strings.Join(failures, "; "))
}

func newResult() Result {
	return Result{Outputs: make(map[string]string), Env: make(map[string]string), State: make(map[string]string)}
}

func (r Runner) stdout() io.Writer {
	if r.Stdout != nil {
		return r.Stdout
	}
	return io.Discard
}

func (r Runner) stderr() io.Writer {
	if r.Stderr != nil {
		return r.Stderr
	}
	return io.Discard
}

func actionInputEnv(inputs map[string]string) map[string]string {
	env := make(map[string]string, len(inputs))
	for name, value := range inputs {
		name = strings.ToUpper(strings.ReplaceAll(name, " ", "_"))
		env["INPUT_"+name] = value
	}
	return env
}

func mapEnv(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for _, name := range sortedKeys(values) {
		env = append(env, name+"="+values[name])
	}
	return env
}

func processEnv(overrides map[string]string) []string {
	// This allowlist is also the mise trust boundary: ambient MISE_* values must
	// never redirect compatibility runtime downloads or verification.
	values := make(map[string]string, 6+len(overrides))
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"} {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	if _, ok := values["PATH"]; !ok {
		values["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	if _, ok := values["HOME"]; !ok {
		if home, err := os.UserHomeDir(); err == nil {
			values["HOME"] = home
		}
	}
	if _, ok := values["TMPDIR"]; !ok {
		values["TMPDIR"] = os.TempDir()
	}
	for name, value := range overrides {
		values[name] = value
	}
	return mapEnv(values)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type commandProcessor struct {
	mu              sync.Mutex
	stdout          io.Writer
	stderr          io.Writer
	masks           []string
	trustedWarnings workflowCommandAnnotationBuffer
	warnings        workflowCommandAnnotationBuffer
	errors          workflowCommandAnnotationBuffer
	stopToken       string
	discard         bool
}

type workflowCommandAnnotationBuffer struct {
	commands  []workflowCommandAnnotation
	rendered  int
	truncated bool
}

type workflowCommandAnnotation struct {
	file     string
	title    string
	location string
	message  string
}

type parsedWorkflowCommand struct {
	name       string
	properties map[string]string
	message    string
}

func newCommandProcessor(stdout, stderr io.Writer) *commandProcessor {
	return &commandProcessor{stdout: stdout, stderr: stderr}
}

var errInvalidWorkflowCommandStopToken = errors.New("invalid ::stop-commands workflow command")

func (p *commandProcessor) process(target io.Writer, line string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discard {
		return nil
	}
	command, isCommand := parseWorkflowCommand(line)
	if p.stopToken != "" {
		if isCommand && strings.EqualFold(command.name, p.stopToken) {
			p.stopToken = ""
		}
		p.writeMaskedLineLocked(target, line)
		return nil
	}
	if isCommand {
		switch {
		case strings.EqualFold(command.name, "add-mask"):
			p.addMaskLocked(command.message)
			return nil
		case strings.EqualFold(command.name, "stop-commands"):
			if !validWorkflowCommandStopToken(command.message) {
				const message = "invalid ::stop-commands token: token is empty or collides with a workflow command"
				p.appendWorkflowCommandLocked(&p.errors, workflowErrorAnnotationHeading, parsedWorkflowCommand{message: message})
				p.writeWorkflowCommandMessageLocked(target, "error", message)
				return errInvalidWorkflowCommandStopToken
			}
			p.stopToken = command.message
			if len(command.message) > 6 {
				p.addMaskLocked(command.message)
			}
			p.writeMaskedLineLocked(target, line)
			return nil
		case strings.EqualFold(command.name, "warning"):
			p.appendWorkflowCommandLocked(&p.warnings, workflowWarningAnnotationHeading, command)
			p.writeWorkflowCommandMessageLocked(target, "warning", command.message)
			return nil
		case strings.EqualFold(command.name, "error"):
			p.appendWorkflowCommandLocked(&p.errors, workflowErrorAnnotationHeading, command)
			p.writeWorkflowCommandMessageLocked(target, "error", command.message)
			return nil
		case strings.EqualFold(command.name, "group"):
			p.writeLogSectionLocked(target, command.message)
			return nil
		case strings.EqualFold(command.name, "endgroup"),
			strings.EqualFold(command.name, "debug"),
			strings.EqualFold(command.name, "add-matcher"),
			strings.EqualFold(command.name, "remove-matcher"):
			return nil
		}
	}
	if isLegacyPresentationCommand(line) {
		return nil
	}
	p.writeMaskedLineLocked(target, line)
	return nil
}

func (p *commandProcessor) logSection(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeLogSectionLocked(p.stdout, name)
}

func (p *commandProcessor) expandCurrentSection() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintln(p.stdout, "^^^ +++")
}

func (p *commandProcessor) writeLogSectionLocked(target io.Writer, name string) {
	name = sanitizeLogSectionText(p.maskTextLocked(name))
	if name != "" {
		_, _ = fmt.Fprintln(target, "--- "+name)
	}
}

func sanitizeLogSectionText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return strings.TrimSpace(text)
}

func isLegacyPresentationCommand(line string) bool {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	for _, command := range []string{"##[debug]", "##[add-matcher]", "##[remove-matcher]"} {
		if strings.HasPrefix(strings.ToLower(line), command) {
			return true
		}
	}
	return false
}

func validWorkflowCommandStopToken(token string) bool {
	if token == "" || strings.EqualFold(token, "pause-logging") {
		return false
	}
	for _, command := range []string{
		"add-mask", "add-matcher", "add-path", "debug", "echo", "endgroup", "error", "group",
		"internal-set-repo-path", "notice", "remove-matcher", "save-state", "set-env",
		"set-output", "set-repo-path", "stop-commands", "warning",
	} {
		if strings.EqualFold(token, command) {
			return false
		}
	}
	return true
}

func (p *commandProcessor) writeWorkflowCommandMessageLocked(target io.Writer, severity, message string) {
	if message == "" {
		return
	}
	p.writeMaskedLineLocked(target, severity+": "+message)
}

// trustedWarning records a runner-owned warning even after untrusted action
// output has been suppressed, while preserving the job's registered masks.
func (p *commandProcessor) trustedWarning(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendWorkflowCommandLocked(&p.trustedWarnings, workflowWarningAnnotationHeading, parsedWorkflowCommand{message: message})
	p.writeWorkflowCommandMessageLocked(p.stderr, "warning", message)
}

func (p *commandProcessor) writeMaskedLineLocked(target io.Writer, line string) {
	_, _ = fmt.Fprintln(target, p.maskTextLocked(line))
}

func (p *commandProcessor) maskTextLocked(text string) string {
	for _, mask := range p.masks {
		text = strings.ReplaceAll(text, mask, "***")
	}
	return text
}

func (p *commandProcessor) appendWorkflowCommandLocked(buffer *workflowCommandAnnotationBuffer, heading string, command parsedWorkflowCommand) {
	annotation := workflowCommandAnnotation{
		file:     strings.Clone(commandText(command.properties["file"])),
		title:    strings.Clone(commandText(command.properties["title"])),
		location: strings.Clone(workflowCommandLocationLabel(command.properties)),
		message:  strings.Clone(commandText(command.message)),
	}
	p.appendWorkflowCommandAnnotationLocked(buffer, heading, annotation)
}

func (p *commandProcessor) appendWorkflowCommandAnnotationLocked(buffer *workflowCommandAnnotationBuffer, heading string, command workflowCommandAnnotation) {
	if buffer.truncated {
		return
	}
	additional := len(renderWorkflowCommandListItem(command))
	if len(buffer.commands) == 0 {
		additional += len(heading) + len(workflowCommandListHeading) + len(workflowCommandListEnd)
	}
	if buffer.rendered+additional > maxJobAnnotationBytes-len(workflowCommandTruncationNotice) {
		buffer.truncated = true
		return
	}
	buffer.rendered += additional
	buffer.commands = append(buffer.commands, command)
}

func (p *commandProcessor) suppress() {
	p.mu.Lock()
	p.discard = true
	p.mu.Unlock()
}

func (p *commandProcessor) addMask(value string) {
	p.mu.Lock()
	p.addMaskLocked(value)
	p.mu.Unlock()
}

func (p *commandProcessor) maskValues() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.masks...)
}

type scrubbedCommandError struct {
	cause   error
	message string
}

func (e scrubbedCommandError) Error() string { return e.message }
func (e scrubbedCommandError) Unwrap() error { return e.cause }

func (p *commandProcessor) scrubError(err error) error {
	if err == nil {
		return nil
	}
	registered := p.maskValues()
	masks := make([]string, 0, len(registered)*2)
	for _, mask := range registered {
		if mask == "" {
			continue
		}
		masks = append(masks, mask)
		quoted := strconv.Quote(mask)
		escaped := quoted[1 : len(quoted)-1]
		if escaped != mask {
			masks = append(masks, escaped)
		}
	}
	sort.Slice(masks, func(i, j int) bool {
		if len(masks[i]) != len(masks[j]) {
			return len(masks[i]) > len(masks[j])
		}
		return masks[i] < masks[j]
	})
	message := err.Error()
	for _, mask := range masks {
		message = strings.ReplaceAll(message, mask, "***")
	}
	if message == err.Error() {
		return err
	}
	return scrubbedCommandError{cause: err, message: message}
}

func (p *commandProcessor) workflowCommandAnnotations() (warnings string, warningsTruncated bool, errors string, errorsTruncated bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	masks := normalizedMasks(p.masks)
	var combinedWarnings workflowCommandAnnotationBuffer
	for _, command := range p.trustedWarnings.commands {
		p.appendWorkflowCommandAnnotationLocked(&combinedWarnings, workflowWarningAnnotationHeading, maskWorkflowCommandAnnotation(command, masks))
	}
	for _, command := range p.warnings.commands {
		p.appendWorkflowCommandAnnotationLocked(&combinedWarnings, workflowWarningAnnotationHeading, maskWorkflowCommandAnnotation(command, masks))
	}
	combinedWarnings.truncated = combinedWarnings.truncated || p.trustedWarnings.truncated || p.warnings.truncated
	warnings, warningsTruncated = renderWorkflowCommandAnnotation(workflowWarningAnnotationHeading, combinedWarnings.commands, combinedWarnings.truncated)
	var maskedErrors workflowCommandAnnotationBuffer
	for _, command := range p.errors.commands {
		p.appendWorkflowCommandAnnotationLocked(&maskedErrors, workflowErrorAnnotationHeading, maskWorkflowCommandAnnotation(command, masks))
	}
	maskedErrors.truncated = maskedErrors.truncated || p.errors.truncated
	errors, errorsTruncated = renderWorkflowCommandAnnotation(workflowErrorAnnotationHeading, maskedErrors.commands, maskedErrors.truncated)
	return warnings, warningsTruncated, errors, errorsTruncated
}

func (p *commandProcessor) addMaskLocked(value string) {
	if value == "" {
		return
	}
	p.masks = append(p.masks, value)
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if line != "" && line != value {
			p.masks = append(p.masks, line)
		}
	}
}

func parseWorkflowCommand(line string) (parsedWorkflowCommand, bool) {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	if !strings.HasPrefix(line, "::") {
		return parsedWorkflowCommand{}, false
	}
	separator := strings.Index(line[2:], "::")
	if separator < 0 {
		return parsedWorkflowCommand{}, false
	}
	separator += 2
	header := line[2:separator]
	name, propertyList, hasProperties := strings.Cut(header, " ")
	if name == "" {
		return parsedWorkflowCommand{}, false
	}
	properties := map[string]string{}
	if hasProperties {
		for _, property := range strings.Split(strings.TrimSpace(propertyList), ",") {
			name, value, ok := strings.Cut(property, "=")
			if !ok || name == "" || value == "" {
				continue
			}
			properties[strings.ToLower(name)] = decodeCommandProperty(value)
		}
	}
	return parsedWorkflowCommand{name: name, properties: properties, message: decodeCommandValue(line[separator+2:])}, true
}

func decodeCommandValue(value string) string {
	replacer := strings.NewReplacer("%0D", "\r", "%0A", "\n", "%25", "%")
	return replacer.Replace(value)
}

func decodeCommandProperty(value string) string {
	replacer := strings.NewReplacer("%0D", "\r", "%0A", "\n", "%3A", ":", "%2C", ",", "%25", "%")
	return replacer.Replace(value)
}

func renderWorkflowCommandAnnotation(heading string, commands []workflowCommandAnnotation, truncated bool) (string, bool) {
	if len(commands) == 0 {
		return "", truncated
	}
	type commandGroup struct {
		file     string
		commands []workflowCommandAnnotation
	}
	groups := make([]commandGroup, 0)
	groupIndexes := make(map[string]int)
	for _, command := range commands {
		file := command.file
		index, ok := groupIndexes[file]
		if !ok {
			index = len(groups)
			groupIndexes[file] = index
			groups = append(groups, commandGroup{file: file})
		}
		groups[index].commands = append(groups[index].commands, command)
	}

	var rendered strings.Builder
	rendered.WriteString(heading)
	rendered.WriteString(workflowCommandListHeading)
	for _, group := range groups {
		for _, command := range group.commands {
			rendered.WriteString(renderWorkflowCommandListItem(command))
		}
	}
	rendered.WriteString(workflowCommandListEnd)

	var body string
	var bodyTruncated bool
	appendBoundedText(&body, &bodyTruncated, rendered.String(), truncated, maxJobAnnotationBytes, workflowCommandTruncationNotice)
	return body, bodyTruncated
}

func renderWorkflowCommandListItem(command workflowCommandAnnotation) string {
	source := "General"
	if command.file != "" {
		location := filepath.Base(strings.ReplaceAll(command.file, "\\", "/"))
		if command.location != "" {
			location += ":" + command.location
		}
		source = "<code>" + commandHTML(location) + "</code>"
	}
	detail := commandHTML(command.message)
	if command.title != "" {
		detail = "<strong>" + commandHTML(command.title) + ":</strong> " + detail
	}
	return "<div class=\"border-top border-silver py2\"><div>" + detail +
		"</div><div class=\"mt1\">" + source + "</div></div>\n"
}

func workflowCommandLocationLabel(properties map[string]string) string {
	line, endLine, column, endColumn := workflowCommandLocation(properties)
	if line == "" {
		return ""
	}
	if endLine == "" {
		endLine = line
	}
	if endColumn == "" {
		endColumn = column
	}
	start := line
	if column != "" {
		start += ":" + column
	}
	end := endLine
	if endColumn != "" {
		end += ":" + endColumn
	}
	if end == "" || end == start || endLine == line && endColumn == column {
		return start
	}
	return start + "–" + end
}

func normalizedMasks(masks []string) []string {
	normalized := make([]string, 0, len(masks))
	for _, mask := range masks {
		if mask = commandText(mask); mask != "" {
			normalized = append(normalized, mask)
		}
	}
	sort.Slice(normalized, func(i, j int) bool {
		if len(normalized[i]) != len(normalized[j]) {
			return len(normalized[i]) > len(normalized[j])
		}
		return normalized[i] < normalized[j]
	})
	return normalized
}

func maskWorkflowCommandAnnotation(command workflowCommandAnnotation, masks []string) workflowCommandAnnotation {
	values := []*string{&command.file, &command.title, &command.location, &command.message}
	for _, mask := range masks {
		for _, value := range values {
			*value = strings.ReplaceAll(*value, mask, "***")
		}
	}
	return command
}

func commandText(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func commandHTML(value string) string {
	value = commandText(value)
	return strings.ReplaceAll(html.EscapeString(value), "\n", "<br>\n")
}

func workflowCommandLocation(properties map[string]string) (line, endLine, column, endColumn string) {
	line, lineNumberOK := workflowCommandCoordinate(properties["line"])
	endLine, endLineNumberOK := workflowCommandCoordinate(properties["endline"])
	column, columnNumberOK := workflowCommandCoordinate(properties["col"])
	endColumn, endColumnNumberOK := workflowCommandCoordinate(properties["endcolumn"])
	lineProperty, endLineProperty := properties["line"], properties["endline"]

	if !lineNumberOK && endLineNumberOK {
		line, lineNumberOK = endLine, true
		lineProperty = endLineProperty
	}
	if !columnNumberOK && endColumnNumberOK {
		column, columnNumberOK = endColumn, true
	}
	if !lineNumberOK && (columnNumberOK || endColumnNumberOK) {
		column, endColumn = "", ""
		columnNumberOK, endColumnNumberOK = false, false
	}
	// Match actions/runner's original-property comparison: textual forms such
	// as line=01,endLine=1 describe different lines for column-range purposes.
	if lineNumberOK && endLineNumberOK && lineProperty != endLineProperty {
		column, endColumn = "", ""
		columnNumberOK, endColumnNumberOK = false, false
	}
	if lineNumberOK && endLineNumberOK && coordinateNumber(endLine) < coordinateNumber(line) {
		line, endLine = "", ""
	}
	if columnNumberOK && endColumnNumberOK && coordinateNumber(endColumn) < coordinateNumber(column) {
		column, endColumn = "", ""
	}
	return line, endLine, column, endColumn
}

func workflowCommandCoordinate(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err != nil {
		return "", false
	}
	return value, true
}

func coordinateNumber(value string) int64 {
	number, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	return number
}
