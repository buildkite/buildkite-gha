package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// ActionSource resolves and materializes tokenless public GitHub actions.
type ActionSource interface {
	Fetch(context.Context, source.Reference) (source.Resolved, source.Materialized, error)
}

// PublicActionSource joins the public resolver and immutable source store.
type PublicActionSource struct {
	Resolver *source.Resolver
	Store    *source.Store
}

func (s PublicActionSource) Fetch(ctx context.Context, ref source.Reference) (source.Resolved, source.Materialized, error) {
	if s.Resolver == nil || s.Store == nil {
		return source.Resolved{}, source.Materialized{}, fmt.Errorf("public action source is not configured")
	}
	r, err := s.Resolver.Resolve(ctx, ref)
	if err != nil {
		return source.Resolved{}, source.Materialized{}, err
	}
	m, err := s.Store.Materialize(ctx, r)
	return r, m, err
}

type actionLockBuilder struct {
	workspace    string
	source       ActionSource
	nodes        map[string]*actionNode
	ids          map[string]string
	active       map[string]bool
	caps         map[string]bool
	requiresMise bool
}

type actionNode struct {
	lock plan.ActionLock
}

// compileActionLocks builds one shared action DAG for all roots. Selectors are
// returned in the same order as refs.
func compileActionLocks(ctx context.Context, workspace string, actionSource ActionSource, refs []string) ([]plan.ActionSelector, []plan.ActionLock, []string, bool, error) {
	if workspace == "" {
		return nil, nil, nil, true, fmt.Errorf("workflow path must identify a repository root")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, nil, nil, true, fmt.Errorf("resolve workspace: %w", err)
	}
	b := &actionLockBuilder{workspace: abs, source: actionSource, nodes: map[string]*actionNode{}, ids: map[string]string{}, active: map[string]bool{}, caps: map[string]bool{}}
	selectors := make([]plan.ActionSelector, 0, len(refs))
	for _, ref := range refs {
		n, err := b.add(ctx, ref, 1)
		if err != nil {
			return nil, nil, nil, true, err
		}
		selectors = append(selectors, plan.ActionSelector{Lock: n.lock.ID})
	}
	locks := make([]plan.ActionLock, 0, len(b.nodes))
	for _, n := range b.nodes {
		locks = append(locks, n.lock)
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].ID < locks[j].ID })
	caps := make([]string, 0, len(b.caps))
	for c := range b.caps {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	return selectors, locks, caps, b.requiresMise, nil
}

func (b *actionLockBuilder) add(ctx context.Context, raw string, depth int) (*actionNode, error) {
	if depth > metadata.MaxNestedActionDepth {
		return nil, fmt.Errorf("action nesting exceeds maximum depth %d at %q", metadata.MaxNestedActionDepth, raw)
	}
	key, lock, root, loadPath, err := b.describe(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("compile action %q: %w", raw, err)
	}
	if n := b.nodes[key]; n != nil {
		if b.active[key] {
			return nil, fmt.Errorf("action recursion detected at %q", raw)
		}
		return n, nil
	}
	identityBytes, _ := json.Marshal(lock)
	identity := string(identityBytes)
	sum := sha256.Sum256(identityBytes)
	lock.ID = "a-" + hex.EncodeToString(sum[:])[:16]
	if previous, ok := b.ids[lock.ID]; ok && previous != identity {
		return nil, fmt.Errorf("action lock ID collision at %q", lock.ID)
	}
	b.ids[lock.ID] = identity
	n := &actionNode{lock: lock}
	b.nodes[key] = n
	b.active[key] = true
	defer delete(b.active, key)

	m, err := metadata.Load(root, loadPath)
	if err != nil {
		return nil, err
	}
	runtime, err := m.Runtime()
	if err != nil {
		return nil, err
	}
	if err := m.ValidateEntrypoints(runtime); err != nil {
		return nil, err
	}
	if actionintegration.UsesNativeAdapter(actionintegration.Identity{Source: n.lock.Source, Repository: n.lock.Repository, Path: n.lock.Path}) {
		return n, nil
	}
	if runtime == metadata.RuntimeNode20 || runtime == metadata.RuntimeNode24 {
		b.requiresMise = true
	}
	for _, capability := range runtime.RequiredCapabilities() {
		b.caps[capability] = true
	}
	if runtime == metadata.RuntimeComposite {
		for _, step := range m.Runs.Steps {
			if step.Uses == "" {
				continue
			}
			child, err := b.add(ctx, step.Uses, depth+1)
			if err != nil {
				return nil, fmt.Errorf("action %q child %q: %w", raw, step.Uses, err)
			}
			if n.lock.Children == nil {
				n.lock.Children = map[string]plan.ActionSelector{}
			}
			n.lock.Children[step.Uses] = plan.ActionSelector{Lock: child.lock.ID}
		}
	}
	return n, nil
}

func (b *actionLockBuilder) describe(ctx context.Context, raw string) (string, plan.ActionLock, string, string, error) {
	if strings.HasPrefix(raw, "./") {
		p := strings.TrimPrefix(raw, "./")
		if p == "." || p != "" && (path.Clean(p) != p || strings.Contains(p, "\\") || strings.HasPrefix(p, "/")) {
			return "", plan.ActionLock{}, "", "", fmt.Errorf("invalid local action path")
		}
		m, err := metadata.Load(b.workspace, p)
		if err != nil {
			return "", plan.ActionLock{}, "", "", err
		}
		digest, err := source.DigestTree(m.Path)
		return "workspace:" + p, plan.ActionLock{Source: "workspace", Path: p, SourceDigest: digest}, b.workspace, p, err
	}
	ref, err := source.Parse(raw)
	if err != nil {
		return "", plan.ActionLock{}, "", "", err
	}
	canonical := strings.ToLower(ref.Owner + "/" + ref.Repository)
	key := "github:" + canonical + "/" + ref.Path + "@" + ref.Ref
	if n := b.nodes[key]; n != nil {
		return key, n.lock, "", "", nil
	}
	if b.source == nil {
		return "", plan.ActionLock{}, "", "", fmt.Errorf("remote action source is not configured")
	}
	resolved, materialized, err := b.source.Fetch(ctx, ref)
	if err != nil {
		return "", plan.ActionLock{}, "", "", err
	}
	commit := strings.ToLower(resolved.Commit)
	lock := plan.ActionLock{Source: "github", Repository: canonical, RequestedRef: ref.Ref, Commit: commit, Path: ref.Path, SourceDigest: materialized.SourceDigest}
	descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path})
	if descriptor.Adapter == actionintegration.AdapterUploadArtifactBuildkite {
		if err := actionintegration.ValidateUploadArtifactCommit(lock.Commit); err != nil {
			return "", plan.ActionLock{}, "", "", err
		}
	}
	if descriptor.Adapter == actionintegration.AdapterDownloadArtifactBuildkite {
		if err := actionintegration.ValidateDownloadArtifactCommit(lock.Commit); err != nil {
			return "", plan.ActionLock{}, "", "", err
		}
	}
	if descriptor.Service == actionintegration.ServiceCache {
		if err := actionintegration.ValidateCacheCommit(lock.Commit); err != nil {
			requested := lock.Repository
			if lock.Path != "" {
				requested += "/" + lock.Path
			}
			return "", plan.ActionLock{}, "", "", fmt.Errorf("%s@%s resolved to commit %s, which is not admitted; supported: %s@v6.1.0 (commit %s)", requested, lock.RequestedRef, lock.Commit, requested, actionintegration.CacheCommit)
		}
	}
	b.caps["network"] = true
	return key, lock, materialized.RepositoryRoot, ref.Path, nil
}

type memoizedActionSource struct {
	source ActionSource
	cache  map[string]memoizedAction
}

type memoizedAction struct {
	resolved     source.Resolved
	materialized source.Materialized
}

func newMemoizedActionSource(actionSource ActionSource) ActionSource {
	if actionSource == nil {
		return nil
	}
	return &memoizedActionSource{source: actionSource, cache: map[string]memoizedAction{}}
}

func (s *memoizedActionSource) Fetch(ctx context.Context, ref source.Reference) (source.Resolved, source.Materialized, error) {
	key := strings.ToLower(ref.Owner+"/"+ref.Repository) + "\x00" + ref.Path + "\x00" + ref.Ref
	if cached, ok := s.cache[key]; ok {
		return cached.resolved, cached.materialized, nil
	}
	resolved, materialized, err := s.source.Fetch(ctx, ref)
	if err != nil {
		return source.Resolved{}, source.Materialized{}, err
	}
	s.cache[key] = memoizedAction{resolved: resolved, materialized: materialized}
	return resolved, materialized, nil
}
