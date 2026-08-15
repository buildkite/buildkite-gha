package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
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
	selectors           []plan.ActionSelector
	locks               []plan.ActionLock
	capabilities        []string
	requiredSecrets     []string
	githubTokenActions  []string
	requiresMise        bool
	requiresGitHubToken bool
}

type actionRequirements struct {
	githubToken     bool
	requiredSecrets map[string]bool
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
		if version := strings.TrimPrefix(runtimeErr.Runtime, "node"); version != runtimeErr.Runtime {
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
	for _, line := range strings.Split(reason, "\n") {
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
	requiresGitHubToken := false
	requiredSecrets := map[string]bool{}
	var githubTokenActions []string
	if suppliedInputs != nil {
		for i, root := range roots {
			requirements, err := root.inspectInvocation(suppliedInputs[i], true, serverURL)
			if err != nil {
				return actionCompilation{}, fmt.Errorf("compile action %q: %w", refs[i], err)
			}
			requiresGitHubToken = requiresGitHubToken || requirements.githubToken
			if requirements.githubToken {
				githubTokenActions = append(githubTokenActions, refs[i])
			}
			for name := range requirements.requiredSecrets {
				requiredSecrets[name] = true
			}
		}
	}
	secretNames := sortedKeys(requiredSecrets)
	return actionCompilation{
		selectors:           selectors,
		locks:               locks,
		capabilities:        caps,
		requiredSecrets:     secretNames,
		githubTokenActions:  githubTokenActions,
		requiresMise:        b.requiresMise,
		requiresGitHubToken: requiresGitHubToken,
	}, nil
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
	if actionintegration.UsesNativeAdapter(actionintegration.Identity{Source: n.lock.Source, Repository: n.lock.Repository, Path: n.lock.Path}) {
		n.native = true
		return n, nil
	}
	if runtime == metadata.RuntimeNode16 || runtime == metadata.RuntimeNode20 || runtime == metadata.RuntimeNode24 {
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

func (n *actionNode) inspectInvocation(supplied map[string]string, workflowAuthored bool, serverURL string) (actionRequirements, error) {
	requirements := actionRequirements{requiredSecrets: map[string]bool{}}
	for _, suppliedName := range sortedKeys(supplied) {
		value := supplied[suppliedName]
		referencesEvent, err := expression.TemplateReferencesGitHubEvent(value)
		if err != nil {
			return actionRequirements{}, fmt.Errorf("action input %q: %w", suppliedName, err)
		}
		if referencesEvent {
			return actionRequirements{}, fmt.Errorf("action input %q: github.event cannot be retained in a job plan", suppliedName)
		}
		names, err := expression.SecretReferences(value)
		if err != nil {
			return actionRequirements{}, fmt.Errorf("action input %q: %w", suppliedName, err)
		}
		referencesToken, err := expression.ReferencesGitHubToken(value)
		if err != nil {
			return actionRequirements{}, fmt.Errorf("action input %q: %w", suppliedName, err)
		}
		if !workflowAuthored && len(names) != 0 {
			return actionRequirements{}, fmt.Errorf("action input %q: composite action metadata cannot grant secret authority", suppliedName)
		}
		if !workflowAuthored && referencesToken {
			return actionRequirements{}, fmt.Errorf("action input %q: composite action metadata cannot grant github.token authority", suppliedName)
		}
		requirements.githubToken = requirements.githubToken || referencesToken
		input, declared := n.metadata.Inputs[strings.ToLower(suppliedName)]
		for _, name := range names {
			if declared && !input.Required && name != "GITHUB_TOKEN" {
				continue
			}
			requirements.requiredSecrets[name] = true
		}
	}
	if n.native {
		return requirements, nil
	}
	for _, name := range sortedKeys(n.metadata.Inputs) {
		input := n.metadata.Inputs[name]
		if input.Default == nil || hasActionInput(supplied, name) {
			continue
		}
		if err := expression.ValidateActionInputDefault(*input.Default); err != nil {
			return actionRequirements{}, fmt.Errorf("action input %q default: %w", name, err)
		}
		referencesToken, err := expression.ActionInputDefaultRequiresGitHubToken(*input.Default, serverURL)
		if err != nil {
			return actionRequirements{}, fmt.Errorf("action input %q default: %w", name, err)
		}
		requirements.githubToken = requirements.githubToken || referencesToken
	}
	if n.runtime != metadata.RuntimeComposite {
		return requirements, nil
	}
	for i, step := range n.metadata.Runs.Steps {
		if step.Uses == "" {
			continue
		}
		child := n.children[step.Uses]
		if child == nil {
			return actionRequirements{}, fmt.Errorf("composite action step %d child %q is missing", i+1, step.Uses)
		}
		descriptor, _ := actionintegration.Lookup(actionintegration.Identity{Source: child.lock.Source, Repository: child.lock.Repository, Path: child.lock.Path})
		if descriptor.Adapter == actionintegration.AdapterUploadArtifactBuildkite {
			if err := actionintegration.ValidateUploadArtifactInputs(child.lock.Commit, step.With); err != nil {
				return actionRequirements{}, fmt.Errorf("composite action step %d child %q: bounded upload-artifact adapter: %w", i+1, step.Uses, err)
			}
		}
		childRequirements, err := child.inspectInvocation(step.With, false, serverURL)
		if err != nil {
			return actionRequirements{}, fmt.Errorf("composite action step %d child %q: %w", i+1, step.Uses, err)
		}
		requirements.githubToken = requirements.githubToken || childRequirements.githubToken
		for name := range childRequirements.requiredSecrets {
			requirements.requiredSecrets[name] = true
		}
	}
	return requirements, nil
}

func hasActionInput(inputs map[string]string, name string) bool {
	for candidate := range inputs {
		if strings.EqualFold(candidate, name) {
			return true
		}
	}
	return false
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
	repositoryRoot, err := filepath.Abs(materialized.RepositoryRoot)
	if err != nil {
		return "", plan.ActionLock{}, "", "", fmt.Errorf("resolve materialized repository root: %w", err)
	}
	logicalInfo, err := os.Lstat(repositoryRoot)
	if err != nil || !logicalInfo.IsDir() || logicalInfo.Mode()&os.ModeSymlink != 0 {
		return "", plan.ActionLock{}, "", "", fmt.Errorf("materialized repository root is not a non-symlink directory")
	}
	repositoryRoot, err = filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return "", plan.ActionLock{}, "", "", fmt.Errorf("canonicalize materialized repository root: %w", err)
	}
	canonicalInfo, err := os.Stat(repositoryRoot)
	if err != nil || !os.SameFile(logicalInfo, canonicalInfo) {
		return "", plan.ActionLock{}, "", "", fmt.Errorf("materialized repository root changed while canonicalizing")
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
