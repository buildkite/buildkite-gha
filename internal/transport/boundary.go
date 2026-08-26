package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxAnnotationBodyBytes         = 1024 * 1024
	maxAnnotationContextCharacters = 100
	maxAgentErrorBytes             = 64 * 1024
	importerArtifactPattern        = ".buildkite-gha/**/*"
	importerArtifactConcurrency    = "8"
)

// ErrMetadataUnavailable means Buildkite confirmed that reserved webhook
// metadata does not apply to the build or its linked webhook is unavailable.
var ErrMetadataUnavailable = errors.New("buildkite webhook metadata is unavailable")

// Runner is the only process boundary used by the Buildkite adapter.
type Runner interface {
	Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error)
}

// Agent invokes public buildkite-agent commands. Tests use a capture Runner.
type Agent struct {
	Runner Runner
}

// CommandRunner executes the Buildkite Agent without a shell.
type CommandRunner struct {
	Stderr io.Writer
}

func (r CommandRunner) Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = r.Stderr
	if err := command.Run(); err != nil {
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// RunBounded executes a command while bounding captured stdout and stderr.
func (r CommandRunner) RunBounded(ctx context.Context, dir, name string, args []string, stdin []byte, limit int) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Stdin = bytes.NewReader(stdin)
	stdout := newBoundedBuffer(limit)
	stderr := newBoundedBuffer(maxAgentErrorBytes)
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.overflow {
		return nil, stderr.Bytes(), fmt.Errorf("command output exceeds %d bytes", limit)
	}
	if stderr.overflow {
		return stdout.Bytes(), nil, fmt.Errorf("command error output exceeds %d bytes", maxAgentErrorBytes)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	overflow  bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{remaining: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		_, _ = b.buffer.Write(p[:b.remaining])
		b.remaining = 0
		b.overflow = true
		return len(p), nil
	}
	b.remaining -= len(p)
	return b.buffer.Write(p)
}

func (b *boundedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

// UploadArtifactFrom uploads a workspace-relative path rooted at root.
func (a Agent) UploadArtifactFrom(ctx context.Context, root, path string) error {
	nativePath := filepath.FromSlash(path)
	if root == "" || path == "" || !filepath.IsLocal(nativePath) || filepath.ToSlash(filepath.Clean(nativePath)) != path {
		return fmt.Errorf("artifact upload requires a root and clean relative path")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve artifact upload root: %w", err)
	}
	_, err = a.runInDir(ctx, resolvedRoot, []string{"artifact", "upload", path}, nil)
	return err
}

func (a Agent) DownloadArtifact(ctx context.Context, path, destination, producerStep string) error {
	if !keyPattern.MatchString(producerStep) {
		return fmt.Errorf("invalid producer step key %q", producerStep)
	}
	_, err := a.run(ctx, []string{"artifact", "download", path, destination, "--step", producerStep}, nil)
	return err
}

func (a Agent) SetMetadata(ctx context.Context, key, value string) error {
	_, err := a.run(ctx, []string{"meta-data", "set", key, value}, nil)
	return err
}

func (a Agent) GetMetadata(ctx context.Context, key string) ([]byte, error) {
	return a.run(ctx, []string{"meta-data", "get", key}, nil)
}

// GetMetadataBounded retrieves metadata once without allowing agent stdout to
// grow beyond limit. CommandRunner also captures bounded stderr so the two
// documented reserved-webhook absence responses can be distinguished from
// authorization, transport, and rate-limit failures.
func (a Agent) GetMetadataBounded(ctx context.Context, key string, limit int) ([]byte, error) {
	if limit < 1 {
		return nil, fmt.Errorf("metadata output limit must be positive")
	}
	runner, ok := a.Runner.(interface {
		RunBounded(context.Context, string, string, []string, []byte, int) ([]byte, []byte, error)
	})
	if !ok {
		result, err := a.GetMetadata(ctx, key)
		if err != nil {
			return nil, err
		}
		if len(result) > limit {
			return nil, fmt.Errorf("command output exceeds %d bytes", limit)
		}
		return result, nil
	}
	stdout, stderr, err := runner.RunBounded(ctx, "", "buildkite-agent", []string{"meta-data", "get", key}, nil, limit)
	if err == nil {
		return stdout, nil
	}
	message := strings.TrimSpace(string(stderr))
	var exitError *exec.ExitError
	if key == "buildkite:webhook" && len(stdout) == 0 && errors.As(err, &exitError) && exitError.ExitCode() == 1 &&
		(strings.Contains(message, "400 Bad Request: Build was not triggered by a webhook") ||
			strings.Contains(message, "404 Not Found: Build webhook is not available")) {
		return nil, ErrMetadataUnavailable
	}
	if message != "" {
		return nil, fmt.Errorf("get Buildkite metadata %q: %v: %s", key, err, message)
	}
	return nil, fmt.Errorf("get Buildkite metadata %q: %w", key, err)
}

func (a Agent) UploadPipeline(ctx context.Context, pipeline []byte) error {
	_, err := a.run(ctx, []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets"}, pipeline)
	return err
}

// AnnotateJob publishes Markdown through stdin under a job-scoped context.
// Reusing the context updates the annotation instead of duplicating it.
func (a Agent) AnnotateJob(ctx context.Context, jobID, annotationContext, style, body string) error {
	if !uuidPattern.MatchString(jobID) {
		return fmt.Errorf("invalid annotation job ID %q", jobID)
	}
	if !utf8.ValidString(annotationContext) || utf8.RuneCountInString(annotationContext) < 1 || utf8.RuneCountInString(annotationContext) > maxAnnotationContextCharacters {
		return fmt.Errorf("annotation context must be valid UTF-8 between 1 and %d characters", maxAnnotationContextCharacters)
	}
	switch style {
	case "success", "info", "warning", "error":
	default:
		return fmt.Errorf("invalid annotation style %q", style)
	}
	if body == "" || len(body) > maxAnnotationBodyBytes || !utf8.ValidString(body) {
		return fmt.Errorf("annotation body must be valid UTF-8 between 1 and %d bytes", maxAnnotationBodyBytes)
	}
	_, err := a.run(ctx, []string{"annotate", "--scope", "job", "--job", jobID, "--context", annotationContext, "--style", style}, []byte(body))
	return err
}

// Artifact is one immutable, content-addressed upload input.
type Artifact struct {
	Path     string
	Digest   string
	Contents []byte
}

// ErrPipelineUpload identifies the pipeline-upload stage while preserving the
// underlying agent error.
var ErrPipelineUpload = errors.New("upload pipeline")

// UploadArtifacts materializes and verifies every artifact before uploading
// them from one root, then uploads the pipeline only after all artifacts pass.
func UploadArtifacts(ctx context.Context, agent Agent, root string, artifacts []Artifact, pipeline []byte) error {
	artifacts = cloneArtifacts(artifacts)
	if len(artifacts) == 0 {
		return fmt.Errorf("at least one artifact is required")
	}
	if len(pipeline) == 0 {
		return fmt.Errorf("pipeline is required")
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	seen := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		if err := validateArtifact(artifact); err != nil {
			return err
		}
		if seen[artifact.Path] {
			return fmt.Errorf("duplicate artifact path %q", artifact.Path)
		}
		seen[artifact.Path] = true
	}

	absoluteRoot, err := artifactRoot(root)
	if err != nil {
		return err
	}
	rootFS, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return fmt.Errorf("open artifact root: %w", err)
	}
	defer func() { _ = rootFS.Close() }()
	for _, artifact := range artifacts {
		path, err := writeMaterialized(rootFS, absoluteRoot, artifact.Path, artifact.Contents)
		if err != nil {
			return fmt.Errorf("materialize artifact %q: %w", artifact.Path, err)
		}
		if err := verifyMaterialized(path, artifact.Contents, artifact.Digest); err != nil {
			return fmt.Errorf("verify artifact %q before upload: %w", artifact.Path, err)
		}
	}
	if err := uploadMaterializedArtifacts(ctx, agent, absoluteRoot, artifacts); err != nil {
		return err
	}
	if err := agent.UploadPipeline(ctx, pipeline); err != nil {
		return fmt.Errorf("%w: %w", ErrPipelineUpload, err)
	}
	return nil
}

func uploadMaterializedArtifacts(ctx context.Context, agent Agent, absoluteRoot string, artifacts []Artifact) error {
	for _, artifact := range artifacts {
		path := filepath.Join(absoluteRoot, filepath.FromSlash(artifact.Path))
		if err := verifyMaterialized(path, artifact.Contents, artifact.Digest); err != nil {
			return fmt.Errorf("verify artifact %q at upload: %w", artifact.Path, err)
		}
	}
	if _, err := agent.runInDir(ctx, absoluteRoot, []string{"artifact", "upload", importerArtifactPattern, "--concurrency", importerArtifactConcurrency}, nil); err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	return nil
}

func cloneArtifacts(artifacts []Artifact) []Artifact {
	cloned := make([]Artifact, len(artifacts))
	for i, artifact := range artifacts {
		cloned[i] = artifact
		cloned[i].Contents = bytes.Clone(artifact.Contents)
	}
	return cloned
}

func validateArtifact(artifact Artifact) error {
	if !strings.HasPrefix(artifact.Path, ".buildkite-gha/") {
		return fmt.Errorf("invalid artifact path %q", artifact.Path)
	}
	if _, err := filepath.Localize(artifact.Path); err != nil {
		return fmt.Errorf("invalid artifact path %q", artifact.Path)
	}
	if !digestPattern.MatchString(artifact.Digest) || Digest(artifact.Contents) != artifact.Digest {
		return fmt.Errorf("artifact %q digest does not match contents", artifact.Path)
	}
	return nil
}

func artifactRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("artifact root is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve artifact root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("resolve artifact root: %w", err)
	}
	return resolved, nil
}

func (a Agent) run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	return a.runInDir(ctx, "", args, stdin)
}

func (a Agent) runInDir(ctx context.Context, dir string, args []string, stdin []byte) ([]byte, error) {
	if a.Runner == nil {
		return nil, fmt.Errorf("buildkite agent runner is required")
	}
	return a.Runner.Run(ctx, dir, "buildkite-agent", args, stdin)
}

func writeMaterialized(root *os.Root, absoluteRoot, relative string, contents []byte) (string, error) {
	relative = filepath.FromSlash(relative)
	if err := root.MkdirAll(filepath.Dir(relative), 0o700); err != nil {
		return "", err
	}
	if err := root.Remove(relative); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := root.WriteFile(relative, contents, 0o600); err != nil {
		return "", err
	}
	unresolved := filepath.Join(absoluteRoot, relative)
	resolved, err := filepath.EvalSymlinks(unresolved)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(rootResolved, resolved)
	if err != nil || within == ".." || filepath.IsAbs(within) || len(within) >= 3 && within[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("materialized path escaped artifact root")
	}
	return resolved, nil
}

func verifyMaterialized(path string, expected []byte, digest string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(contents, expected) {
		return fmt.Errorf("on-disk bytes differ from validated contents")
	}
	if digest == "" {
		digest = Digest(expected)
	}
	if Digest(contents) != digest {
		return fmt.Errorf("on-disk digest differs from validated digest")
	}
	return nil
}
