package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/program"
)

// RepositorySource resolves and materializes tokenless public GitHub repositories.
// ActionSource remains an alias for callers that use it only for action locking.
type RepositorySource interface {
	Fetch(context.Context, source.Reference) (source.Resolved, source.Materialized, error)
}

type ActionSource = RepositorySource

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
		return source.Resolved{}, source.Materialized{}, fmt.Errorf("resolve action reference: %w", err)
	}
	identity := actionintegration.Identity{
		Source:     "github",
		Repository: strings.ToLower(ref.Owner + "/" + ref.Repository),
		Path:       ref.Path,
	}
	if descriptor, _ := actionintegration.Lookup(identity); descriptor.Adapter == actionintegration.AdapterCheckoutExactEventSHA {
		if _, _, err := actionintegration.Admit(identity, strings.ToLower(r.Commit)); err != nil {
			return source.Resolved{}, source.Materialized{}, err
		}
	}
	m, err := s.Store.Materialize(ctx, r)
	if err != nil {
		return source.Resolved{}, source.Materialized{}, fmt.Errorf("download action source: %w", err)
	}
	return r, m, nil
}

type actionLockBuilder struct {
	workspace    string
	source       ActionSource
	nodes        map[string]*actionNode
	ids          map[string]string
	active       map[string]bool
	caps         map[string]bool
	materialized []source.Materialized
	requiresMise bool
}

type actionNode struct {
	lock     plan.ActionLock
	metadata metadata.Metadata
	children map[string]*actionNode
	runtime  metadata.Runtime
	native   bool
}

type actionCompilation struct {
	selectors            []plan.ActionSelector
	locks                []plan.ActionLock
	capabilities         []string
	requiredSecrets      []string
	githubTokenActions   []string
	requiresMise         bool
	requiresGitHubToken  bool
	requiresEventPayload bool
	programs             map[string]program.Action
}

// validateActionResolutions resolves each independent root invocation before
// plan construction. It deliberately aggregates failures while sharing one
// immutable source snapshot; no plan can be emitted unless every root passes.
func validateActionResolutions(ctx context.Context, ir IR, options Options) (ProcessingEvidence, error) {
	actionSource := newMemoizedActionSource(options.ActionSource)
	evidence := ProcessingEvidence{ActionResolutionComplete: true}
	var diagnostics []error
	for _, instance := range ir.Jobs {
		for i, step := range instance.Steps {
			if step.Kind != "uses" {
				continue
			}
			if !options.ResolveActions && !strings.HasPrefix(step.Uses, "./") {
				evidence.ActionResolutionComplete = false
				continue
			}
			_, err := compileActionInvocations(ctx, instance.RepositoryRoot, actionSource, plan.EventServerURL(ir.Event.Provider), []string{step.Uses}, []map[string]string{step.With})
			evaluation := ActionEvaluation{Instance: instance.Key, Job: instance.LogicalJobID, Reference: step.Uses, Step: i + 1, Passed: err == nil}
			evidence.Actions = append(evidence.Actions, evaluation)
			if err == nil {
				continue
			}
			position := step.Span.Start
			message, detail, action := actionResolutionMessage(step.Uses, err)
			diagnostics = append(diagnostics, &ProcessingFinding{
				Stage: StageResolution, Code: CodeActionResolution, Category: "action-resolution",
				Path: instance.SourcePath, Line: position.Line, Column: position.Column,
				Job: instance.LogicalJobID, Instance: instance.Key, Action: action, Step: i + 1,
				Message: message, Detail: detail,
				Err: fmt.Errorf("%s:%d:%d: job %q action %q at step %d: %w", instance.SourcePath, position.Line, position.Column, instance.LogicalJobID, step.Uses, i+1, err),
			})
		}
	}
	return evidence, errors.Join(diagnostics...)
}

func actionResolutionMessage(reference string, err error) (message, detail, action string) {
	action = reference
	for {
		var childErr *actionChildError
		if !errors.As(err, &childErr) {
			break
		}
		action = childErr.child
		err = childErr.err
	}
	if message, detail, ok := actionintegration.UnsupportedVersionDiagnostic(action, err); ok {
		return message, detail, action
	}
	var runtimeErr *metadata.UnsupportedRuntimeError
	if errors.As(err, &runtimeErr) {
		runtime := fmt.Sprintf("runtime %q", runtimeErr.Runtime)
		if version, ok := strings.CutPrefix(runtimeErr.Runtime, "node"); ok {
			runtime = "Node.js " + version
		}
		if strings.HasPrefix(action, "./") {
			return fmt.Sprintf("Action %q uses %s, which is unsupported. Update runs.using to node16, node20, or node24.", action, runtime), "", action
		}
		return fmt.Sprintf("Action %q uses %s, which is unsupported. Use an action release that supports Node.js 16, 20, or 24.", action, runtime), "", action
	}
	reason := strings.TrimPrefix(err.Error(), fmt.Sprintf("compile action %q: ", action))
	if strings.HasPrefix(reason, "resolve action reference: ") || strings.HasPrefix(reason, "download action source: ") {
		return fmt.Sprintf("Action %q could not be resolved: %s", action, reason[strings.Index(reason, ": ")+2:]), "", action
	}
	if start := strings.Index(reason, "parse action metadata \""); start >= 0 {
		pathStart := start + len("parse action metadata \"")
		if pathEnd := strings.Index(reason[pathStart:], "\""); pathEnd >= 0 {
			reason = reason[:start] + "action metadata" + reason[pathStart+pathEnd+1:]
		}
	}
	if fields := unsupportedMetadataFields(reason); fields != "" {
		reason = "action metadata uses " + fields
	}
	return fmt.Sprintf("Action %q is unsupported: %s", action, reason), "", action
}

type actionChildError struct {
	child string
	err   error
}

func (e *actionChildError) Error() string { return fmt.Sprintf("child action %q: %v", e.child, e.err) }
func (e *actionChildError) Unwrap() error { return e.err }

func unsupportedMetadataFields(reason string) string {
	if !strings.Contains(reason, "yaml: unmarshal errors:") {
		return ""
	}
	linesByField := map[string][]string{}
	for line := range strings.SplitSeq(reason, "\n") {
		if strings.Contains(line, "yaml: unmarshal errors:") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) < 7 || parts[0] != "line" || parts[2] != "field" || parts[4] != "not" || parts[5] != "found" {
			return ""
		}
		linesByField[parts[3]] = append(linesByField[parts[3]], strings.TrimSuffix(parts[1], ":"))
	}
	fields := sortedKeys(linesByField)
	formatted := make([]string, 0, len(fields))
	for _, field := range fields {
		lineLabel := "line "
		if len(linesByField[field]) > 1 {
			lineLabel = "lines "
		}
		formatted = append(formatted, fmt.Sprintf("unsupported field %q at %s%s", field, lineLabel, strings.Join(linesByField[field], ", ")))
	}
	return strings.Join(formatted, " and ")
}

// compileActionLocks builds one shared action DAG for all roots. Selectors are
// returned in the same order as refs.
func compileActionLocks(ctx context.Context, workspace string, actionSource ActionSource, refs []string) ([]plan.ActionSelector, []plan.ActionLock, []string, bool, error) {
	compiled, err := compileActionInvocations(ctx, workspace, actionSource, plan.EventServerURL("github"), refs, nil)
	if err != nil {
		return nil, nil, nil, true, err
	}
	return compiled.selectors, compiled.locks, compiled.capabilities, compiled.requiresMise, nil
}

func compileActionInvocations(ctx context.Context, workspace string, actionSource ActionSource, serverURL string, refs []string, suppliedInputs []map[string]string) (actionCompilation, error) {
	if workspace == "" {
		return actionCompilation{}, fmt.Errorf("workflow path must identify a repository root")
	}
	if suppliedInputs != nil && len(suppliedInputs) != len(refs) {
		return actionCompilation{}, fmt.Errorf("action references and supplied inputs have different lengths")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return actionCompilation{}, fmt.Errorf("resolve workspace: %w", err)
	}
	b := &actionLockBuilder{workspace: abs, source: actionSource, nodes: map[string]*actionNode{}, ids: map[string]string{}, active: map[string]bool{}, caps: map[string]bool{}}
	defer func() {
		for _, materialized := range b.materialized {
			materialized.Release()
		}
	}()
	selectors := make([]plan.ActionSelector, 0, len(refs))
	roots := make([]*actionNode, 0, len(refs))
	for _, ref := range refs {
		n, err := b.add(ctx, ref, 1)
		if err != nil {
			return actionCompilation{}, err
		}
		roots = append(roots, n)
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
	planningPrograms := make(map[string]program.Action, len(b.nodes))
	programs := make(map[string]program.Action, len(b.nodes))
	for _, node := range b.nodes {
		planningPrograms[node.lock.ID] = lowerActionProgram(node)
		if node.native {
			continue
		}
		programs[node.lock.ID] = planningPrograms[node.lock.ID]
	}
	requiresGitHubToken := false
	requiresEventPayload := false
	requiredSecrets := map[string]bool{}
	var githubTokenActions []string
	if suppliedInputs != nil {
		for i, root := range roots {
			if err := validateActionAdapterInputs(root); err != nil {
				return actionCompilation{}, fmt.Errorf("compile action %q: %w", refs[i], err)
			}
			authority, err := program.InventoryActionAuthority(planningPrograms, root.lock.ID, workflowActionBindings(suppliedInputs[i]), program.ActionAuthorityOptions{ServerURL: serverURL})
			if err != nil {
				return actionCompilation{}, fmt.Errorf("compile action %q: %w", refs[i], err)
			}
			requiresGitHubToken = requiresGitHubToken || authority.GitHubToken
			requiresEventPayload = requiresEventPayload || authority.EventPayload
			if authority.GitHubToken {
				githubTokenActions = append(githubTokenActions, refs[i])
			}
			for _, name := range authority.Secrets {
				requiredSecrets[name] = true
			}
		}
	}
	secretNames := sortedKeys(requiredSecrets)
	return actionCompilation{
		selectors:            selectors,
		locks:                locks,
		capabilities:         caps,
		requiredSecrets:      secretNames,
		githubTokenActions:   githubTokenActions,
		requiresMise:         b.requiresMise,
		requiresGitHubToken:  requiresGitHubToken,
		requiresEventPayload: requiresEventPayload,
		programs:             programs,
	}, nil
}

func workflowActionBindings(values map[string]string) []program.Binding {
	result := make([]program.Binding, 0, len(values))
	for _, name := range sortedKeys(values) {
		result = append(result, program.Binding{Name: name, Value: program.Site{
			Source: values[name], Surface: program.SurfaceStepTemplate, Result: program.ResultString,
			Provenance: program.ProvenanceWorkflow, Purpose: program.PurposeActionInput,
		}})
	}
	return result
}

func validateActionAdapterInputs(root *actionNode) error {
	if root == nil || root.runtime != metadata.RuntimeComposite {
		return nil
	}
	for i, step := range root.metadata.Runs.Steps {
		if step.Uses == "" {
			continue
		}
		child := root.children[step.Uses]
		if child == nil {
			return fmt.Errorf("composite action step %d child %q is missing", i+1, step.Uses)
		}
		descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: child.lock.Source, Repository: child.lock.Repository, Path: child.lock.Path})
		if descriptor.Adapter == actionintegration.AdapterUploadArtifactBuildkite {
			if err := actionintegration.ValidateUploadArtifactInputs(child.lock.Commit, step.With); err != nil {
				return fmt.Errorf("composite action step %d child %q: bounded upload-artifact adapter: %w", i+1, step.Uses, err)
			}
		}
		if err := validateActionAdapterInputs(child); err != nil {
			return fmt.Errorf("composite action step %d child %q: %w", i+1, step.Uses, err)
		}
	}
	return nil
}

func lowerActionProgram(node *actionNode) program.Action {
	children := make(map[string]string, len(node.lock.Children))
	for uses, selector := range node.lock.Children {
		children[uses] = selector.Lock
	}
	return program.ActionFromMetadata(node.metadata, string(node.runtime), children)
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

	if actionintegration.UsesNativeAdapter(actionintegration.Identity{Source: n.lock.Source, Repository: n.lock.Repository, Path: n.lock.Path}) {
		// The native adapter replaces the admitted release's execution
		// entirely, and admitted legacy releases predate the supported
		// metadata and runtime set, so upstream metadata must not gate
		// admission. It still informs input declarations when it loads.
		n.native = true
		if m, err := metadata.Load(root, loadPath); err == nil {
			m.SourceRoot = root
			n.metadata = m
		}
		return n, nil
	}
	m, err := metadata.Load(root, loadPath)
	if err != nil {
		return nil, err
	}
	if lock.Source == "github" {
		m.SourceRoot = root
	} else {
		m.SourceRoot = m.Path
	}
	n.metadata = m
	runtime, err := m.Runtime()
	if err != nil {
		return nil, err
	}
	n.runtime = runtime
	if err := m.ValidateEntrypoints(runtime); err != nil {
		return nil, err
	}
	if image, ok := metadata.DockerImageReference(m.Runs.Image); ok {
		n.lock.DockerImage = image
	}
	if runtime == metadata.RuntimeNode16 || runtime == metadata.RuntimeNode24 {
		if err := validateCompileSite(m.Runs.PreIf, expression.ProfileActionLifecycle, expression.ResultBoolean); err != nil {
			return nil, fmt.Errorf("pre-if: %w", err)
		}
		if err := validateCompileSite(m.Runs.PostIf, expression.ProfileActionLifecycle, expression.ResultBoolean); err != nil {
			return nil, fmt.Errorf("post-if: %w", err)
		}
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
				return nil, &actionChildError{child: step.Uses, err: err}
			}
			if n.lock.Children == nil {
				n.lock.Children = map[string]plan.ActionSelector{}
				n.children = map[string]*actionNode{}
			}
			n.lock.Children[step.Uses] = plan.ActionSelector{Lock: child.lock.ID}
			n.children[step.Uses] = child
		}
	}
	return n, nil
}

func (b *actionLockBuilder) describe(ctx context.Context, raw string) (string, plan.ActionLock, string, string, error) {
	if after, ok := strings.CutPrefix(raw, "./"); ok {
		p := after
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
	b.materialized = append(b.materialized, materialized)
	repositoryRoot, err := canonicalMaterializedRepositoryRoot(materialized.RepositoryRoot)
	if err != nil {
		return "", plan.ActionLock{}, "", "", err
	}
	commit := strings.ToLower(resolved.Commit)
	lock := plan.ActionLock{Source: "github", Repository: canonical, RequestedRef: ref.Ref, Commit: commit, Path: ref.Path, SourceDigest: materialized.SourceDigest}
	descriptor, _, admitErr := actionintegration.Admit(actionintegration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path}, lock.Commit)
	if admitErr != nil {
		if descriptor.Service == actionintegration.ServiceCache {
			requested := lock.Repository
			if lock.Path != "" {
				requested += "/" + lock.Path
			}
			return "", plan.ActionLock{}, "", "", fmt.Errorf("%s@%s resolved to commit %s, which is not admitted: %w", requested, lock.RequestedRef, lock.Commit, admitErr)
		}
		return "", plan.ActionLock{}, "", "", admitErr
	}
	b.caps["network"] = true
	return key, lock, repositoryRoot, ref.Path, nil
}

type memoizedActionSource struct {
	source    ActionSource
	mu        sync.Mutex
	cache     map[string]memoizedAction
	active    map[string]*memoizedActionCall
	pins      map[string]memoizedRepositoryPin
	pinActive map[string]chan struct{}
}

type memoizedRepositoryPin struct {
	commit string
	digest string
}

type memoizedAction struct {
	resolved     source.Resolved
	materialized source.Materialized
}

type memoizedActionCall struct {
	done         chan struct{}
	resolved     source.Resolved
	materialized source.Materialized
	err          error
}

func newMemoizedActionSource(actionSource ActionSource) ActionSource {
	if actionSource == nil {
		return nil
	}
	return &memoizedActionSource{
		source: actionSource, cache: map[string]memoizedAction{}, active: map[string]*memoizedActionCall{},
		pins: map[string]memoizedRepositoryPin{}, pinActive: map[string]chan struct{}{},
	}
}

// MemoizeActionSource reuses successful action resolutions and materializations
// across compiler invocations that share the returned source.
func MemoizeActionSource(actionSource ActionSource) ActionSource {
	return newMemoizedActionSource(actionSource)
}

// MemoizeRepositorySource pins mutable repository references and reuses
// materializations across compiler invocations that share the returned source.
func MemoizeRepositorySource(repositorySource RepositorySource) RepositorySource {
	return newMemoizedActionSource(repositorySource)
}

func (s *memoizedActionSource) Fetch(ctx context.Context, ref source.Reference) (source.Resolved, source.Materialized, error) {
	repositoryKey := strings.ToLower(ref.Owner+"/"+ref.Repository) + "\x00" + ref.Ref
	key := repositoryKey + "\x00" + ref.Path
	s.mu.Lock()
	if cached, ok := s.cache[key]; ok {
		s.mu.Unlock()
		materialized, err := cached.materialized.Retain(ctx)
		return cached.resolved, materialized, err
	}
	if active, ok := s.active[key]; ok {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return source.Resolved{}, source.Materialized{}, ctx.Err()
		case <-active.done:
			if active.err != nil && ctx.Err() == nil && (errors.Is(active.err, context.Canceled) || errors.Is(active.err, context.DeadlineExceeded)) {
				return s.Fetch(ctx, ref)
			}
			if active.err != nil {
				return active.resolved, active.materialized, active.err
			}
			materialized, err := active.materialized.Retain(ctx)
			return active.resolved, materialized, err
		}
	}
	pin, pinned := s.pins[repositoryKey]
	if !pinned {
		if active := s.pinActive[repositoryKey]; active != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return source.Resolved{}, source.Materialized{}, ctx.Err()
			case <-active:
				return s.Fetch(ctx, ref)
			}
		}
		s.pinActive[repositoryKey] = make(chan struct{})
	}
	call := &memoizedActionCall{done: make(chan struct{})}
	s.active[key] = call
	s.mu.Unlock()
	fetchRef := ref
	if pinned && ref.Ref != pin.commit {
		fetchRef, call.err = exactRepositoryReference(ref, pin.commit)
	}
	if call.err == nil {
		call.resolved, call.materialized, call.err = s.source.Fetch(ctx, fetchRef)
	}
	if call.err == nil {
		call.resolved.Reference = ref
		if pinned && (call.resolved.Commit != pin.commit || call.materialized.SourceDigest != pin.digest) {
			call.materialized.Release()
			call.materialized = source.Materialized{}
			call.err = fmt.Errorf("repository source changed after immutable pin")
		}
	}
	s.mu.Lock()
	delete(s.active, key)
	if call.err == nil {
		if !pinned {
			s.pins[repositoryKey] = memoizedRepositoryPin{commit: call.resolved.Commit, digest: call.materialized.SourceDigest}
		}
		s.cache[key] = memoizedAction{resolved: call.resolved, materialized: call.materialized}
	}
	if !pinned {
		close(s.pinActive[repositoryKey])
		delete(s.pinActive, repositoryKey)
	}
	close(call.done)
	s.mu.Unlock()
	return call.resolved, call.materialized, call.err
}

func exactRepositoryReference(ref source.Reference, commit string) (source.Reference, error) {
	raw := ref.Owner + "/" + ref.Repository
	if ref.Path != "" {
		raw += "/" + ref.Path
	}
	return source.Parse(raw + "@" + commit)
}
