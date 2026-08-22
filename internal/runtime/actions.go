package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
)

// ActionMaterializer materializes an already resolved, immutable action source.
type ActionMaterializer interface {
	Materialize(context.Context, source.Resolved) (source.Materialized, error)
}

var _ ActionMaterializer = (*source.Store)(nil)

type actionLockEntry struct {
	lock      plan.ActionLock
	duplicate bool
	mu        sync.Mutex
	material  *source.Materialized
	inflight  *actionMaterialization
}

type actionMaterialization struct {
	done     chan struct{}
	material source.Materialized
	err      error
}

// actionLockResolver is scoped to one job. Materialization is shared per lock,
// while verification is deliberately performed on every resolution.
type actionLockResolver struct {
	job          plan.Job
	workspace    string
	materializer ActionMaterializer
	locks        map[string]*actionLockEntry
}

func newActionLockResolver(job plan.Job, workspace string, materializer ActionMaterializer) *actionLockResolver {
	r := &actionLockResolver{job: job, workspace: workspace, materializer: materializer, locks: make(map[string]*actionLockEntry, len(job.Actions))}
	for _, lock := range job.Actions {
		if entry, ok := r.locks[lock.ID]; ok {
			entry.duplicate = true
			continue
		}
		r.locks[lock.ID] = &actionLockEntry{lock: lock}
	}
	return r
}

func usesCheckoutAdapter(lock plan.ActionLock) bool {
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	return descriptor.Adapter == actionintegration.AdapterCheckoutExactEventSHA
}

func validateJobCheckoutAdapters(job plan.Job) (bool, error) {
	locks := make(map[string]plan.ActionLock, len(job.Actions))
	for _, lock := range job.Actions {
		if usesCheckoutAdapter(lock) {
			if _, _, err := actionintegration.Admit(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path}, lock.Commit); err != nil {
				return false, err
			}
		}
		locks[lock.ID] = lock
	}
	found := false
	for _, step := range job.Steps {
		if step.Action == nil {
			continue
		}
		if lock, ok := locks[step.Action.Lock]; ok && usesCheckoutAdapter(lock) {
			found = true
		}
	}
	return found, nil
}

func usesUploadArtifactAdapter(lock plan.ActionLock) bool {
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	return descriptor.Adapter == actionintegration.AdapterUploadArtifactBuildkite
}

func usesDownloadArtifactAdapter(lock plan.ActionLock) bool {
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	return descriptor.Adapter == actionintegration.AdapterDownloadArtifactBuildkite
}

func usesNativeAdapter(lock plan.ActionLock) bool {
	return actionintegration.UsesNativeAdapter(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
}

func usesCacheService(lock plan.ActionLock) bool {
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	return descriptor.Service == actionintegration.ServiceCache
}

func usesCacheClientCompatibility(lock plan.ActionLock) bool {
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	return descriptor.CacheClientCompatibility
}

func (r *actionLockResolver) source(selector plan.ActionSelector) (_ string, err error) {
	defer func() { err = markHardJobFailure(err) }()
	if r == nil || selector.Lock == "" {
		return "", fmt.Errorf("resolve action lock: selector is missing")
	}
	entry, ok := r.locks[selector.Lock]
	if !ok || entry == nil {
		return "", fmt.Errorf("resolve action lock %q: lock is missing", selector.Lock)
	}
	if entry.duplicate || entry.lock.ID != selector.Lock {
		return "", fmt.Errorf("resolve action lock %q: lock identity is ambiguous", selector.Lock)
	}
	return entry.lock.Source, nil
}

func (r *actionLockResolver) resolve(ctx context.Context, selector plan.ActionSelector) (_ metadata.Metadata, _ plan.ActionLock, err error) {
	defer func() {
		if ctxErr := ctx.Err(); ctxErr == nil || !errors.Is(err, ctxErr) {
			err = markHardJobFailure(err)
		}
	}()
	if r == nil || selector.Lock == "" {
		return metadata.Metadata{}, plan.ActionLock{}, fmt.Errorf("resolve action lock: selector is missing")
	}
	entry, ok := r.locks[selector.Lock]
	if !ok || entry == nil {
		return metadata.Metadata{}, plan.ActionLock{}, fmt.Errorf("resolve action lock %q: lock is missing", selector.Lock)
	}
	if entry.duplicate || entry.lock.ID != selector.Lock {
		return metadata.Metadata{}, plan.ActionLock{}, fmt.Errorf("resolve action lock %q: lock identity is ambiguous", selector.Lock)
	}
	if _, _, err := actionintegration.Admit(actionintegration.Identity{Source: entry.lock.Source, Repository: entry.lock.Repository, Path: entry.lock.Path}, entry.lock.Commit); err != nil {
		return metadata.Metadata{}, plan.ActionLock{}, err
	}

	var m metadata.Metadata
	switch entry.lock.Source {
	case "workspace":
		m, err = r.verifyWorkspace(entry.lock)
	case "github":
		m, err = r.verifyGitHub(ctx, entry)
	default:
		err = fmt.Errorf("unsupported action lock source %q", entry.lock.Source)
	}
	if err != nil {
		return metadata.Metadata{}, plan.ActionLock{}, fmt.Errorf("resolve action lock %q: %w", selector.Lock, err)
	}
	return m, entry.lock, nil
}

func (r *actionLockResolver) verifyWorkspace(lock plan.ActionLock) (metadata.Metadata, error) {
	if r.workspace == "" {
		return metadata.Metadata{}, fmt.Errorf("workspace is missing")
	}
	if err := verifyWorkflow(r.job, r.workspace); err != nil {
		return metadata.Metadata{}, fmt.Errorf("workspace action workflow verification failed: %w", err)
	}
	actionPath, err := verifiedActionPath(r.workspace, lock.Path)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("resolve workspace action: %w", err)
	}
	digest, err := source.DigestTree(actionPath)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("digest workspace action tree: %w", err)
	}
	if digest != lock.SourceDigest {
		return metadata.Metadata{}, fmt.Errorf("workspace action digest mismatch: lock binds %s, tree has %s", lock.SourceDigest, digest)
	}
	if action, ok := r.job.ActionPrograms[lock.ID]; ok {
		return actionMetadata(action, actionPath, actionPath)
	}
	m, err := metadata.Load(r.workspace, lock.Path)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("load workspace action: %w", err)
	}
	if m.Path != actionPath {
		return metadata.Metadata{}, fmt.Errorf("workspace action path mutated during metadata load")
	}
	digest, err = source.DigestTree(m.Path)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("digest workspace action tree after metadata load: %w", err)
	}
	if digest != lock.SourceDigest {
		return metadata.Metadata{}, fmt.Errorf("workspace action mutated during metadata load: lock binds %s, tree has %s", lock.SourceDigest, digest)
	}
	m.SourceRoot = m.Path
	return m, nil
}

func (r *actionLockResolver) verifyGitHub(ctx context.Context, entry *actionLockEntry) (metadata.Metadata, error) {
	lock := entry.lock
	if !r.job.HasCapability("network") {
		return metadata.Metadata{}, fmt.Errorf("GitHub action materialization requires the plan's network capability")
	}
	if r.materializer == nil {
		return metadata.Metadata{}, fmt.Errorf("GitHub action materializer is missing")
	}
	raw := lock.Repository
	if lock.Path != "" {
		raw += "/" + lock.Path
	}
	raw += "@" + lock.Commit
	ref, err := source.Parse(raw)
	if err != nil || ref.Owner+"/"+ref.Repository != lock.Repository || ref.Path != lock.Path || ref.Ref != lock.Commit {
		return metadata.Metadata{}, fmt.Errorf("malformed canonical GitHub repository or exact commit")
	}
	// Parsing also enforces commit-like reference syntax only structurally, so
	// insist on a lower-case full SHA independently of RequestedRef.
	if len(lock.Commit) != 40 || strings.Trim(lock.Commit, "0123456789abcdef") != "" {
		return metadata.Metadata{}, fmt.Errorf("GitHub action commit is not an exact lower-case SHA")
	}
	resolved := source.Resolved{Reference: ref, Commit: lock.Commit, SourceDigest: lock.SourceDigest}
	materialized, err := entry.materialize(ctx, r.materializer, resolved)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("materialize GitHub action: %w", err)
	}
	if materialized.SourceDigest != lock.SourceDigest {
		return metadata.Metadata{}, fmt.Errorf("materialized source digest mismatch: lock binds %s, materializer returned %s", lock.SourceDigest, materialized.SourceDigest)
	}
	repositoryRoot, err := filepath.Abs(materialized.RepositoryRoot)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("resolve materialized repository root: %w", err)
	}
	logicalInfo, err := os.Lstat(repositoryRoot)
	if err != nil || !logicalInfo.IsDir() || logicalInfo.Mode()&os.ModeSymlink != 0 {
		return metadata.Metadata{}, fmt.Errorf("materialized repository root is not a non-symlink directory")
	}
	repositoryRoot, err = filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("canonicalize materialized repository root: %w", err)
	}
	canonicalInfo, err := os.Stat(repositoryRoot)
	if err != nil || !os.SameFile(logicalInfo, canonicalInfo) {
		return metadata.Metadata{}, fmt.Errorf("materialized repository root changed while canonicalizing")
	}
	digest, err := source.DigestTree(repositoryRoot)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("digest materialized repository tree: %w", err)
	}
	if digest != lock.SourceDigest {
		return metadata.Metadata{}, fmt.Errorf("materialized repository tree digest mismatch: lock binds %s, tree has %s", lock.SourceDigest, digest)
	}
	if action, ok := r.job.ActionPrograms[lock.ID]; ok {
		actionPath, err := verifiedActionPath(repositoryRoot, lock.Path)
		if err != nil {
			return metadata.Metadata{}, fmt.Errorf("resolve materialized action: %w", err)
		}
		return actionMetadata(action, actionPath, repositoryRoot)
	}
	m, err := metadata.Load(repositoryRoot, lock.Path)
	if err != nil {
		// Admitted legacy releases predate the supported metadata set, and a
		// native adapter replaces their execution entirely, so the verified
		// tree digest is the admission boundary rather than the manifest.
		if usesNativeAdapter(lock) {
			return metadata.Metadata{SourceRoot: repositoryRoot}, nil
		}
		return metadata.Metadata{}, fmt.Errorf("load materialized action: %w", err)
	}
	digest, err = source.DigestTree(repositoryRoot)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("digest materialized repository tree after metadata load: %w", err)
	}
	if digest != lock.SourceDigest {
		return metadata.Metadata{}, fmt.Errorf("materialized repository tree mutated during metadata load: lock binds %s, tree has %s", lock.SourceDigest, digest)
	}
	m.SourceRoot = repositoryRoot
	return m, nil
}

func verifiedActionPath(root, relative string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(root, filepath.FromSlash(relative))
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes verified source root")
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("action path is not a directory")
	}
	return candidate, nil
}

func actionMetadata(action program.Action, actionPath, sourceRoot string) (metadata.Metadata, error) {
	m := metadata.Metadata{Name: action.Name, Path: actionPath, SourceRoot: sourceRoot, Inputs: make(map[string]metadata.Input, len(action.Inputs)), Outputs: make(map[string]metadata.Output, len(action.Outputs))}
	for _, input := range action.Inputs {
		var value *string
		if input.Default != nil {
			source := input.Default.Source
			value = &source
		}
		m.Inputs[input.Name] = metadata.Input{Required: input.Required, Default: value}
	}
	for _, output := range action.Outputs {
		m.Outputs[output.Name] = metadata.Output{Value: output.Value.Source}
	}
	switch action.Runtime {
	case program.ActionRuntimeJavaScript:
		if action.JavaScript == nil {
			return metadata.Metadata{}, fmt.Errorf("normalized JavaScript action has no lifecycle")
		}
		if action.JavaScript.NodeMajor != 16 && action.JavaScript.NodeMajor != 24 {
			return metadata.Metadata{}, fmt.Errorf("normalized JavaScript action has unsupported Node.js runtime %d", action.JavaScript.NodeMajor)
		}
		using := string(metadata.RuntimeNode24)
		if action.JavaScript.NodeMajor == 16 {
			using = string(metadata.RuntimeNode16)
		}
		m.Runs = metadata.Runs{Using: using, Pre: action.JavaScript.Pre, PreIf: action.JavaScript.PreCondition.Source, Main: action.JavaScript.Main, Post: action.JavaScript.Post, PostIf: action.JavaScript.PostCondition.Source}
	case program.ActionRuntimeComposite:
		if action.Composite == nil {
			return metadata.Metadata{}, fmt.Errorf("normalized composite action has no execution body")
		}
		m.Runs.Using = string(metadata.RuntimeComposite)
		m.Runs.Steps = make([]metadata.CompositeStep, len(action.Composite.Steps))
		for i, step := range action.Composite.Steps {
			if (step.Run == nil) == (step.Invocation == nil) {
				return metadata.Metadata{}, fmt.Errorf("normalized composite action step %d must have exactly one execution", i+1)
			}
			lowered := metadata.CompositeStep{ID: step.ID, Name: step.Name.Source, If: step.Condition.Source, ContinueOnError: step.ContinueOnError, Env: bindingSources(step.Env)}
			if step.Run != nil {
				lowered.Run, lowered.Shell, lowered.WorkingDirectory = step.Run.Command.Source, step.Run.Shell.Source, step.Run.WorkingDirectory.Source
			} else {
				lowered.Uses, lowered.With = step.Invocation.Uses.Source, bindingSources(step.Invocation.With)
			}
			m.Runs.Steps[i] = lowered
		}
	case program.ActionRuntimeDocker:
		if action.Docker == nil {
			return metadata.Metadata{}, fmt.Errorf("normalized Docker action has no execution body")
		}
		m.Runs.Using, m.Runs.Image = string(metadata.RuntimeDocker), "Dockerfile"
		m.Runs.Args = make([]string, len(action.Docker.Arguments))
		for i, argument := range action.Docker.Arguments {
			m.Runs.Args[i] = argument.Source
		}
		m.Runs.Env = bindingSources(action.Docker.Env)
	case program.ActionRuntimeNative:
	default:
		return metadata.Metadata{}, fmt.Errorf("normalized action has unsupported runtime %q", action.Runtime)
	}
	return m, nil
}

func bindingSources(bindings []program.Binding) map[string]string {
	if len(bindings) == 0 {
		return nil
	}
	values := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		values[binding.Name] = binding.Value.Source
	}
	return values
}

func (entry *actionLockEntry) materialize(ctx context.Context, materializer ActionMaterializer, resolved source.Resolved) (source.Materialized, error) {
	for {
		entry.mu.Lock()
		if entry.material != nil {
			materialized := *entry.material
			entry.mu.Unlock()
			return materialized, nil
		}
		if call := entry.inflight; call != nil {
			entry.mu.Unlock()
			select {
			case <-call.done:
				if call.err != nil && ctx.Err() == nil && (errors.Is(call.err, context.Canceled) || errors.Is(call.err, context.DeadlineExceeded)) {
					continue
				}
				return call.material, call.err
			case <-ctx.Done():
				return source.Materialized{}, ctx.Err()
			}
		}
		call := &actionMaterialization{done: make(chan struct{})}
		entry.inflight = call
		entry.mu.Unlock()

		call.material, call.err = materializer.Materialize(ctx, resolved)
		entry.mu.Lock()
		if call.err == nil {
			materialized := call.material
			entry.material = &materialized
		}
		entry.inflight = nil
		close(call.done)
		entry.mu.Unlock()
		return call.material, call.err
	}
}
