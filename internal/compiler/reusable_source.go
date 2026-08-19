package compiler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	// MaxReusableWorkflowBytes bounds every workflow document parsed during
	// reusable-workflow expansion, including the top-level workflow.
	MaxReusableWorkflowBytes = 1 << 20
)

// RemoteWorkflowSource is immutable provenance for a remote reusable workflow.
// Digest on the containing workflow identifies the selected file; SourceDigest
// identifies the complete repository tree.
type RemoteWorkflowSource struct {
	Repository   string `json:"repository"`
	RequestedRef string `json:"requested_ref"`
	Commit       string `json:"commit"`
	SourceDigest string `json:"source_digest"`
}

type reusableSourceIdentity struct {
	kind       string
	repository string
	commit     string
	path       string
}

func (identity reusableSourceIdentity) key() string {
	return identity.kind + "\x00" + identity.repository + "\x00" + identity.commit + "\x00" + identity.path
}

type reusableWorkflowSource struct {
	identity       reusableSourceIdentity
	repositoryRoot string
	displayPath    string
	remote         *RemoteWorkflowSource
}

func cloneRemoteWorkflowSource(source *RemoteWorkflowSource) *RemoteWorkflowSource {
	if source == nil {
		return nil
	}
	cloned := *source
	return &cloned
}

func reusableSourceIdentityDisplay(identity reusableSourceIdentity) string {
	if identity.kind == "github" {
		return identity.repository + "/" + identity.path + "@" + identity.commit
	}
	return identity.path
}

func localReusableWorkflowSource(workflowPath string) (reusableWorkflowSource, error) {
	root, canonicalPath, err := workflowRepository(workflowPath)
	if err != nil {
		return reusableWorkflowSource{}, err
	}
	relativePath, err := filepath.Rel(root, canonicalPath)
	if err != nil {
		return reusableWorkflowSource{}, fmt.Errorf("locate workflow %q in repository: %w", canonicalPath, err)
	}
	relativePath = filepath.ToSlash(relativePath)
	return reusableWorkflowSource{
		identity:       reusableSourceIdentity{kind: "workspace", repository: root, path: relativePath},
		repositoryRoot: root,
		displayPath:    "./" + relativePath,
	}, nil
}

func (resolver *reusableResolver) loadReusableWorkflow(ctx context.Context, parent reusableWorkflowSource, uses string) (reusableWorkflowSource, []byte, error) {
	if strings.Contains(uses, "${{") {
		return reusableWorkflowSource{}, nil, fmt.Errorf("reusable workflow %q is runtime-dependent; only literal local or GitHub references are supported", uses)
	}
	if strings.HasPrefix(uses, "./") {
		return resolver.loadLocalReusableWorkflow(parent, uses)
	}
	return resolver.loadRemoteReusableWorkflow(ctx, uses)
}

func (resolver *reusableResolver) loadLocalReusableWorkflow(parent reusableWorkflowSource, uses string) (reusableWorkflowSource, []byte, error) {
	relativePath, err := reusableWorkflowPath(strings.TrimPrefix(uses, "./"), false)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("local reusable workflow %q %w", uses, err)
	}
	candidate := filepath.Join(parent.repositoryRoot, filepath.FromSlash(relativePath))
	if err := requireWithinRepository(parent.repositoryRoot, candidate); err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve local reusable workflow %q: %w", uses, err)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve local reusable workflow %q: %w", uses, err)
	}
	if err := requireWithinRepository(parent.repositoryRoot, resolved); err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve local reusable workflow %q: %w", uses, err)
	}
	canonicalRelative, err := filepath.Rel(parent.repositoryRoot, resolved)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("locate local reusable workflow %q: %w", uses, err)
	}
	canonicalRelative = filepath.ToSlash(canonicalRelative)
	child := reusableWorkflowSource{
		identity:       parent.identity,
		repositoryRoot: parent.repositoryRoot,
		displayPath:    "./" + canonicalRelative,
	}
	child.identity.path = canonicalRelative
	if parent.remote != nil {
		remote := *parent.remote
		child.remote = &remote
		child.displayPath = remote.Repository + "/" + canonicalRelative + "@" + remote.RequestedRef
	}
	source, err := readReusableWorkflowFile(resolved)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("read local reusable workflow %q: %w", uses, err)
	}
	return child, source, nil
}

func (resolver *reusableResolver) loadRemoteReusableWorkflow(ctx context.Context, uses string) (reusableWorkflowSource, []byte, error) {
	if len(uses) > 1024 {
		return reusableWorkflowSource{}, nil, fmt.Errorf("remote reusable workflow reference exceeds 1024 bytes")
	}
	ref, err := actionsource.Parse(uses)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("invalid remote reusable workflow %q: %w", uses, err)
	}
	workflowPath, err := reusableWorkflowPath(ref.Path, true)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("remote reusable workflow %q %w", uses, err)
	}
	if resolver.repositorySource == nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("remote reusable workflow source is not configured")
	}
	ref.RepositoryRoot = true
	resolved, materialized, err := resolver.repositorySource.Fetch(ctx, ref)
	if err != nil {
		var notPublic *actionsource.NotPublicError
		if errors.As(err, &notPublic) {
			return reusableWorkflowSource{}, nil, fmt.Errorf("reusable workflow %q was not found or access was denied", uses)
		}
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve reusable workflow %q: %w", uses, err)
	}
	resolver.materialized = append(resolver.materialized, materialized)
	commit := resolved.Commit
	if len(commit) != 40 || strings.Trim(commit, "0123456789abcdef") != "" {
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve reusable workflow %q: source returned a non-immutable commit", uses)
	}
	if len(materialized.SourceDigest) != 71 || !strings.HasPrefix(materialized.SourceDigest, "sha256:") || strings.Trim(materialized.SourceDigest[7:], "0123456789abcdef") != "" {
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve reusable workflow %q: source returned an invalid repository digest", uses)
	}
	repositoryRoot, err := canonicalMaterializedRepositoryRoot(materialized.RepositoryRoot)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve reusable workflow %q: %w", uses, err)
	}
	filePath := filepath.Join(repositoryRoot, filepath.FromSlash(workflowPath))
	if err := requireWithinRepository(repositoryRoot, filePath); err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("resolve reusable workflow %q: %w", uses, err)
	}
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		return reusableWorkflowSource{}, nil, fmt.Errorf("reusable workflow %q was not found or access was denied", uses)
	}
	source, err := readReusableWorkflowFile(filePath)
	if err != nil {
		return reusableWorkflowSource{}, nil, fmt.Errorf("read reusable workflow %q: %w", uses, err)
	}
	repository := strings.ToLower(ref.Owner + "/" + ref.Repository)
	remote := &RemoteWorkflowSource{
		Repository: repository, RequestedRef: ref.Ref, Commit: commit, SourceDigest: materialized.SourceDigest,
	}
	return reusableWorkflowSource{
		identity:       reusableSourceIdentity{kind: "github", repository: repository, commit: commit, path: workflowPath},
		repositoryRoot: repositoryRoot,
		displayPath:    repository + "/" + workflowPath + "@" + ref.Ref,
		remote:         remote,
	}, source, nil
}

func reusableWorkflowPath(value string, requireYAML bool) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || path.Clean(value) != value || path.Dir(value) != ".github/workflows" {
		return "", fmt.Errorf("must name a file directly under .github/workflows")
	}
	if requireYAML && path.Ext(value) != ".yml" && path.Ext(value) != ".yaml" {
		return "", fmt.Errorf("must end in .yml or .yaml")
	}
	return value, nil
}

func readReusableWorkflowFile(workflowPath string) ([]byte, error) {
	file, err := os.Open(workflowPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workflow is not a regular file")
	}
	if info.Size() > MaxReusableWorkflowBytes {
		return nil, fmt.Errorf("workflow exceeds %d-byte limit", MaxReusableWorkflowBytes)
	}
	source, err := io.ReadAll(io.LimitReader(file, MaxReusableWorkflowBytes+1))
	if err != nil {
		return nil, err
	}
	if len(source) > MaxReusableWorkflowBytes {
		return nil, fmt.Errorf("workflow exceeds %d-byte limit", MaxReusableWorkflowBytes)
	}
	return source, nil
}

func parseReusableWorkflow(workflowPath string, source []byte) (*workflow.Workflow, error) {
	if len(source) > MaxReusableWorkflowBytes {
		return nil, fmt.Errorf("%s: workflow exceeds %d-byte limit", workflowPath, MaxReusableWorkflowBytes)
	}
	return workflow.Parse(workflowPath, source)
}

func canonicalMaterializedRepositoryRoot(value string) (string, error) {
	repositoryRoot, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve materialized repository root: %w", err)
	}
	logicalInfo, err := os.Lstat(repositoryRoot)
	if err != nil || !logicalInfo.IsDir() || logicalInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("materialized repository root is not a non-symlink directory")
	}
	repositoryRoot, err = filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("canonicalize materialized repository root: %w", err)
	}
	canonicalInfo, err := os.Stat(repositoryRoot)
	if err != nil || !os.SameFile(logicalInfo, canonicalInfo) {
		return "", fmt.Errorf("materialized repository root changed while canonicalizing")
	}
	return repositoryRoot, nil
}
