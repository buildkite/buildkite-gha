package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
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
	maxJobAnnotationBytes         = 1024 * 1024
	jobSummaryAnnotationHeading   = "<h2 class=\"h4 mb2\">GitHub Actions job summary</h2>\n"
	jobSummaryAnnotationSeparator = "<div class=\"border-top border-gray pt2\"></div>\n\n"
	maxJobSummaryBytes            = maxJobAnnotationBytes - len(jobSummaryAnnotationHeading) - len(jobSummaryAnnotationSeparator)

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
	GitLFS                string
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
	OIDCToken             OIDCTokenProvider
	RunIdentity           RunIdentity
	nodeDigests           map[int]string
}

// RunIdentity identifies the Buildkite build that owns this job run. Empty
// fields leave the corresponding github run values unavailable.
type RunIdentity struct {
	BuildID     string
	BuildNumber string
	RetryCount  string
}

// githubValues maps Buildkite build identity onto the GitHub run identity
// fields: run_id is the build ID, run_number is the build number, and
// run_attempt is the retry count plus one.
func (identity RunIdentity) githubValues() map[string]string {
	values := map[string]string{}
	if identity.BuildID != "" {
		values["run_id"] = identity.BuildID
	}
	if identity.BuildNumber != "" {
		values["run_number"] = identity.BuildNumber
	}
	if count, err := strconv.Atoi(identity.RetryCount); err == nil && count >= 0 {
		values["run_attempt"] = strconv.Itoa(count + 1)
	}
	return values
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

// javaScriptAction is an already-resolved local JavaScript action.
type javaScriptAction struct {
	Name                     string
	Path                     string
	Pre                      string
	Main                     string
	Post                     string
	Inputs                   map[string]string
	Env                      map[string]string
	Cache                    bool
	CacheClientCompatibility bool

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
	Image        string
	Entrypoint   string
	Args         []string
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
	return newJobRun(r).runDocker(ctx, newCommandProcessor(r.stdout(), r.stderr()), action)
}

func (r *jobRun) runDocker(ctx context.Context, processor *commandProcessor, action dockerAction) (result Result, err error) {
	result = newResult()
	if err := validateEnvironmentNames(action.Env); err != nil {
		return result, err
	}
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
	if action.Dockerfile != "" && action.Dockerfile != "Dockerfile" {
		return result, fmt.Errorf("docker action requires fixed Dockerfile")
	}
	prebuilt := action.Image != ""
	if prebuilt && !metadata.ValidDockerImageReference(action.Image) {
		return result, fmt.Errorf("docker action has invalid prebuilt image %q", action.Image)
	}
	runImage := action.Image
	var docker, dockerConfig string
	var dockerEnv map[string]string
	if prebuilt && r.prebuiltDocker != nil {
		var ok bool
		runImage, ok = r.prebuiltDocker.images[action.Image]
		if !ok || runImage == "" {
			return result, fmt.Errorf("prebuilt Docker action image %q was not prepared at job start", action.Image)
		}
		docker, dockerConfig, dockerEnv = r.prebuiltDocker.docker, r.prebuiltDocker.config, r.prebuiltDocker.env
	} else {
		docker, dockerConfig, dockerEnv, err = privateDocker(r.Runner)
		if err != nil {
			return result, err
		}
		defer func() { err = errors.Join(err, removeDockerConfig(dockerConfig)) }()
		if prebuilt {
			if err := r.pullContainerImage(ctx, processor, dockerEnv, docker, action.Image); err != nil {
				return result, fmt.Errorf("pull prebuilt Docker action image %q: %w", action.Image, err)
			}
			runImage, err = inspectDockerImageID(ctx, dockerEnv, docker, action.Image)
			if err != nil {
				return result, fmt.Errorf("inspect prebuilt Docker action image %q: %w", action.Image, err)
			}
		}
	}
	var stage stagedDockerSource
	if !prebuilt {
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
		stage, err = stageDockerSource(sourceRoot, action.Path, expected)
		if err != nil {
			return result, err
		}
		defer func() { _ = os.RemoveAll(stage.root) }()
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
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, fmt.Errorf("create Docker invocation identity: %w", err)
	}
	id := hex.EncodeToString(nonce[:])
	owner := "com.buildkite.gha.owner=" + id
	image, container := runImage, "buildkite-gha-container-"+id
	if !prebuilt {
		image = "buildkite-gha-image-" + id
	}
	built, ran := false, false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cleanupTimeout())
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
		err = errors.Join(err, markHardJobFailure(cleanupErr))
	}()
	if !prebuilt {
		buildArgs := []string{"buildx", "build", "--builder", "default", "--load", "--tag", image, "--label", owner, "--file", filepath.Join(stage.action, "Dockerfile"), stage.action}
		built = true // A failed build may still have created the tagged image.
		if err := r.runStreaming(ctx, processor, "", dockerEnv, docker, buildArgs...); err != nil {
			return result, fmt.Errorf("build Docker action %q: %w", action.Name, err)
		}
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
	if action.Entrypoint != "" {
		args = append(args, "--entrypoint", action.Entrypoint)
	}
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
			{host: jobContainerWorkspace, target: "/github/workspace"},
			{host: jobContainerTemp, target: "/github/runner_temp"},
		}}
	}
	for _, name := range sortedKeys(action.Env) {
		if isCacheServiceEnvironment(name) || isIDTokenEnvironment(name) || name == "RUNNER_TOOL_CACHE" || (name == "PATH" && !action.explicitPATH) || name == "GITHUB_WORKSPACE" || name == "RUNNER_TEMP" {
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
	args = append(args, action.Args...)
	ran = true
	runErr := r.runStreaming(ctx, processor, "", dockerRunEnv, docker, args...)
	if runErr != nil {
		runErr = markStepProcessExit(fmt.Errorf("run Docker action %q: %w", action.Name, runErr))
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
	for line := range strings.SplitSeq(inspection, "\n") {
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

func (r *jobRun) runJavaScriptPhase(ctx context.Context, processor *commandProcessor, workspace, node string, action javaScriptAction, entry string, stateEnv, stateOut map[string]string, result *Result) (runErr error) {
	env := mergeStringMaps(result.Env, action.Env, actionInputEnv(action.Inputs))
	if path, ok := result.Env["PATH"]; ok {
		env["PATH"] = path
	}
	env["GITHUB_ACTION_PATH"] = action.Path
	env = removeIDTokenEnvironment(env)
	for name, value := range stateEnv {
		env["STATE_"+name] = value
	}
	env = removeCacheServiceEnvironment(env)
	if action.Cache {
		env = isolateCacheActionEnvironment(env)
	}
	if action.Cache || action.CacheClientCompatibility {
		applyGitHubServerURLOverride(env)
	}
	cacheEnv, cacheErr := r.cacheActionEnvironment(ctx, processor)
	if cacheErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if cacheErr != nil && action.Cache {
		return fmt.Errorf("configure actions/cache service: %w", cacheErr)
	}
	cacheToken := ""
	if cacheErr == nil {
		if action.Cache || action.CacheClientCompatibility {
			cacheEnv["ACTIONS_CACHE_URL"] = cacheURLCompatibility
		}
		env = mergeStringMaps(env, cacheEnv)
		cacheToken = cacheEnv["ACTIONS_RUNTIME_TOKEN"]
	}
	if r.idTokenService != nil && r.jobContainer == nil {
		idTokenEnv, finish, err := r.idTokenService.actionEnvironment(ctx, env)
		if err != nil {
			return fmt.Errorf("configure actions ID-token service: %w", err)
		}
		defer func() {
			failure := finish()
			if runErr != nil {
				runErr = errors.Join(runErr, failure)
			}
		}()
		env = mergeStringMaps(env, idTokenEnv)
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

func (r *jobRun) runProcess(ctx context.Context, processor *commandProcessor, dir string, env map[string]string, result *Result, state map[string]string, name string, args ...string) error {
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
		runErr = r.jobContainer.exec(ctx, r.Runner, processor, dir, env, name, args...)
	} else {
		runErr = r.runStreaming(ctx, processor, dir, env, name, args...)
	}
	runErr = markStepProcessExit(runErr)
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
	if err := validateEnvironmentNames(env); err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = processEnv(env)
	return r.runStreamingCommand(ctx, processor, cmd)
}

func resolveExecutableInPath(name, path string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
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
	maps.Copy(values, overrides)
	return mapEnv(values)
}

func validateEnvironmentNames(env map[string]string) error {
	for name := range env {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", name)
		}
	}
	return nil
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

func (p *commandProcessor) writeLiteral(target io.Writer, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.discard {
		p.writeMaskedLineLocked(target, line)
	}
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
	p.addMaskValueLocked(value)
	for line := range strings.SplitSeq(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if line != "" && line != value {
			p.addMaskValueLocked(line)
		}
	}
	sort.Slice(p.masks, func(i, j int) bool {
		if len(p.masks[i]) != len(p.masks[j]) {
			return len(p.masks[i]) > len(p.masks[j])
		}
		return p.masks[i] < p.masks[j]
	})
}

func (p *commandProcessor) addMaskValueLocked(value string) {
	if slices.Contains(p.masks, value) {
		return
	}
	p.masks = append(p.masks, value)
}
