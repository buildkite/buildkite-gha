package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	// MaxReusableWorkflowDepth bounds repository-local reusable workflow expansion.
	MaxReusableWorkflowDepth = 4
	maxFlattenedJobs         = 1024
)

var staticInputCondition = regexp.MustCompile(`(?i)^\s*(?:\$\{\{\s*inputs\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_-]*)|\[\s*'([A-Za-z0-9_-]{1,255})'\s*\])\s*\}\}|inputs\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_-]*)|\[\s*'([A-Za-z0-9_-]{1,255})'\s*\]))\s*$`)
var staticValueExpression = regexp.MustCompile(`(?i)^\s*\$\{\{\s*(inputs|matrix)\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_-]*)|\[\s*'([A-Za-z0-9_-]{1,255})'\s*\])\s*\}\}\s*$`)
var callOutputNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

type sourcedJob struct {
	workflow.Job
	path                  string
	digest                string
	root                  string
	remote                *RemoteWorkflowSource
	inputs                reusableInputs
	secretAuthority       secretAuthority
	needBindings          map[string]needBinding
	tokenPolicyNarrowed   bool
	jobPermissionsIgnored bool
	reusableCall          workflow.Position
	blockerDetailUnsafe   bool
	callGuards            []sourcedCallGuard
	concurrencyGates      []WorkflowConcurrencyGate
}

type secretAuthority struct {
	unrestricted bool
	bindings     map[string]secretBinding
}

type secretBinding struct {
	source string
	token  bool
}

type sourcedCallGuard struct {
	condition    string
	inputs       reusableInputs
	needBindings map[string]needBinding
}

type reusableInputs struct {
	values   map[string]any
	deferred map[string]needBinding
}

type needBinding struct {
	// members are flattened logical job IDs owned by one source-level need.
	// projectOutputs distinguishes reusable-call/status-only boundaries from
	// ordinary job needs, whose outputs pass through unchanged.
	members        []string
	projectOutputs bool
	outputs        []needOutputBinding
}

type needOutputBinding struct {
	name   string
	member string
	output string
	path   string
	span   workflow.Span
}

type reusableResolution struct {
	jobs    []sourcedJob
	outputs []needOutputBinding
}

type reusableResolver struct {
	workspaceRoot         string
	repositorySource      RepositorySource
	materialized          []actionsource.Materialized
	context               expression.CompileContext
	rootPermissions       *workflow.Permissions
	expanded              int
	runtimeMatrixBoundary bool
	warnings              []Warning
	warnedCancellation    map[workflow.Position]bool
}

// callFrame is one workflow in the reusable-workflow call tree together with
// everything it inherits from the chain of calls that reached it. The root
// frame is the requested workflow; every other frame is derived from its
// caller by child. Root and callee semantics differ only through frame methods
// so a root workflow can never be treated as a callee.
type callFrame struct {
	source   reusableWorkflowSource
	digest   string
	workflow *workflow.Workflow
	// chain lists the source identities from the root frame to this frame and
	// detects call cycles.
	chain []reusableSourceIdentity
	// namespace prefixes flattened job IDs; label prefixes flattened job names.
	namespace string
	label     string
	inputs    reusableInputs
	// needs are the calling job's prerequisites. Callee jobs without needs
	// depend on them so a called workflow starts after the call's needs.
	needs map[string]needBinding
	// permissionCeiling bounds callee permissions to the calling job's. It is
	// nil only for the root frame.
	permissionCeiling   *workflow.Permissions
	secrets             secretAuthority
	tokenPolicyNarrowed bool
	// callPosition is the outermost call in the root workflow, used to
	// attribute warnings for every job the call expands to.
	callPosition workflow.Position
	guards       []sourcedCallGuard
	gates        []WorkflowConcurrencyGate
	depth        int
}

// rootFrame is the requested workflow. It has unrestricted secret authority,
// no permission ceiling, and inherits nothing from a caller.
func rootFrame(source reusableWorkflowSource, digest string, parsed *workflow.Workflow, inputs map[string]any) callFrame {
	return callFrame{
		source: source, digest: digest, workflow: parsed,
		chain:   []reusableSourceIdentity{source.identity},
		inputs:  reusableInputs{values: inputs},
		secrets: secretAuthority{unrestricted: true},
	}
}

func (f callFrame) isRoot() bool { return f.depth == 0 }

// path is the display path used in diagnostics and emitted jobs.
func (f callFrame) path() string { return f.source.displayPath }

// callSite is one call of a reusable workflow from a job in this frame: the
// caller job after its permissions were resolved, one expanded matrix
// combination, and the loaded callee. A caller job with a matrix produces one
// site per combination.
type callSite struct {
	job       workflow.Job
	call      *workflow.ReusableWorkflowCall
	matrix    map[string]any
	instances int
	// needs are the caller job's need bindings after replacement, or the
	// frame's inherited needs when the caller job has none.
	needs               map[string]needBinding
	guards              []sourcedCallGuard
	tokenPolicyNarrowed bool
	callee              reusableWorkflowSource
	calleeDigest        string
	calleeWorkflow      *workflow.Workflow
	inputs              reusableInputs
	secrets             secretAuthority
}

// child derives the frame for one call site. The callee's permission ceiling
// is the calling job's effective permissions, or an empty map when the job has
// none; the call position is the outermost call; namespace and label extend
// the caller's with the calling job and its matrix combination.
func (f callFrame) child(site callSite) (callFrame, error) {
	component := site.job.ID
	if site.instances > 1 {
		suffix, err := matrixDigest(site.matrix)
		if err != nil {
			return callFrame{}, jobError(f.path(), site.job, fmt.Sprintf("namespace reusable-workflow matrix: %v", err))
		}
		component += "-" + suffix
	}
	label := site.job.Name
	if label == "" {
		label = site.job.ID
	}
	if len(site.matrix) != 0 {
		label = instanceLabel(site.job, site.matrix, expression.CompileContext{})
	}
	if f.label != "" {
		label = f.label + " / " + label
	}
	ceiling := site.job.Permissions
	if ceiling == nil {
		ceiling = &workflow.Permissions{Scopes: map[string]string{}, Span: site.call.Span}
	}
	position := f.callPosition
	if f.isRoot() {
		position = site.call.Span.Start
	}
	return callFrame{
		source: site.callee, digest: site.calleeDigest, workflow: site.calleeWorkflow,
		chain:               append(slices.Clone(f.chain), site.callee.identity),
		namespace:           namespacedJobID(f.namespace, component),
		label:               label,
		inputs:              site.inputs,
		needs:               site.needs,
		permissionCeiling:   ceiling,
		secrets:             site.secrets,
		tokenPolicyNarrowed: site.tokenPolicyNarrowed,
		callPosition:        position,
		guards:              site.guards,
		gates:               slices.Clone(f.gates),
		depth:               f.depth + 1,
	}, nil
}

// cycle reports the call chain that returns to source, or "" when source is
// not already being resolved.
func (f callFrame) cycle(source reusableWorkflowSource) string {
	for i, existing := range f.chain {
		if existing.key() == source.identity.key() {
			identities := append(slices.Clone(f.chain[i:]), source.identity)
			chain := make([]string, len(identities))
			for j, identity := range identities {
				chain[j] = reusableSourceIdentityDisplay(identity)
			}
			return strings.Join(chain, " -> ")
		}
	}
	return ""
}

// jobPermissions resolves one job's permissions within this frame. Root jobs
// keep their declared or workflow-default permissions. Callee jobs are bounded
// by the frame's ceiling for id-token only; their repository permissions are
// the root workflow's because only that map is enforced server-side, and the
// result records whether the token would have been narrowed. Either kind of
// job records whether a declared map differs from the root workflow's so
// callers can warn once for a call whose permissions were ignored.
type jobPermissions struct {
	permissions           *workflow.Permissions
	tokenPolicyNarrowed   bool
	jobPermissionsIgnored bool
}

func (f callFrame) jobPermissions(declared, rootPermissions *workflow.Permissions) jobPermissions {
	effective := effectivePermissions(declared, f.workflow.Permissions, f.permissionCeiling, !f.isRoot())
	resolved := jobPermissions{
		permissions:           effective,
		tokenPolicyNarrowed:   f.tokenPolicyNarrowed,
		jobPermissionsIgnored: declared != nil && repositoryPermissionsDiffer(rootPermissions, declared),
	}
	if f.isRoot() {
		return resolved
	}
	resolved.tokenPolicyNarrowed = resolved.tokenPolicyNarrowed || repositoryPermissionsNarrowed(rootPermissions, effective)
	idTokenPermission, hasIDTokenPermission := effective.Scopes["id-token"]
	resolved.permissions = clonePermissions(rootPermissions)
	delete(resolved.permissions.Scopes, "id-token")
	if hasIDTokenPermission {
		resolved.permissions.Scopes["id-token"] = idTokenPermission
	}
	return resolved
}

// jobNeeds binds one job's prerequisites. A job without needs inherits the
// frame's needs as status-only dependencies so the called workflow starts
// after the call's prerequisites.
func (f callFrame) jobNeeds(job workflow.Job, replacements map[string]needBinding) (own, effective map[string]needBinding) {
	own = replacementNeeds(job.Needs, replacements)
	if len(job.Needs) != 0 {
		return own, own
	}
	effective = cloneNeedBindings(f.needs)
	for name, binding := range effective {
		binding.projectOutputs = true
		binding.outputs = nil
		effective[name] = binding
	}
	return own, effective
}

// sourcedJob is the single place a flattened job is constructed from a frame.
func (f callFrame) sourcedJob(job workflow.Job, workspaceRoot string, permissions jobPermissions, needBindings map[string]needBinding, blockerDetailUnsafe bool) sourcedJob {
	id := job.ID
	job.ID = namespacedJobID(f.namespace, id)
	job.Needs = bindingMembers(needBindings)
	if f.label != "" {
		name := job.Name
		if name == "" {
			name = id
		}
		job.Name = f.label + " / " + name
	}
	return sourcedJob{
		Job: job, path: f.path(), digest: f.digest, root: workspaceRoot, remote: cloneRemoteWorkflowSource(f.source.remote), inputs: cloneReusableInputs(f.inputs),
		secretAuthority: cloneSecretAuthority(f.secrets), needBindings: needBindings,
		tokenPolicyNarrowed: permissions.tokenPolicyNarrowed, jobPermissionsIgnored: permissions.jobPermissionsIgnored, reusableCall: f.callPosition,
		blockerDetailUnsafe: blockerDetailUnsafe,
		callGuards:          cloneSourcedCallGuards(f.guards),
		concurrencyGates:    slices.Clone(f.gates),
	}
}

func resolveReusableWorkflows(ctx context.Context, path string, source []byte, parsed *workflow.Workflow, context expression.CompileContext, repositorySource RepositorySource) ([]sourcedJob, []Warning, bool, error) {
	runtimeMatrixBoundary := hasRuntimeMatrixBoundary(parsed)
	rootSource, err := rootWorkflowSource(path)
	if err != nil {
		return nil, nil, runtimeMatrixBoundary, err
	}
	resolver := reusableResolver{
		workspaceRoot: rootSource.repositoryRoot, repositorySource: newMemoizedActionSource(repositorySource), context: context,
		rootPermissions: effectivePermissions(nil, parsed.Permissions, nil, false), runtimeMatrixBoundary: runtimeMatrixBoundary,
		warnedCancellation: make(map[workflow.Position]bool),
	}
	defer func() {
		for _, materialized := range resolver.materialized {
			materialized.Release()
		}
	}()
	resolver.discoverRuntimeMatrixBoundaries(ctx, rootSource, parsed, 0, map[string]int{rootSource.identity.key(): 0})
	resolution, err := resolver.resolve(ctx, rootFrame(rootSource, "sha256:"+sha256Sum(source), parsed, context.Inputs))
	return resolution.jobs, resolver.warnings, resolver.runtimeMatrixBoundary, err
}

func hasReusableCall(parsed *workflow.Workflow) bool {
	for _, job := range parsed.Jobs {
		if job.Reusable != nil {
			return true
		}
	}
	return false
}

func (resolver *reusableResolver) discoverRuntimeMatrixBoundaries(ctx context.Context, current reusableWorkflowSource, parsed *workflow.Workflow, depth int, scannedAtDepth map[string]int) {
	resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasRuntimeMatrixBoundary(parsed)
	if depth >= MaxReusableWorkflowDepth {
		resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasReusableCall(parsed)
		return
	}
	if current.repositoryRoot == "" {
		// A workflow outside .github/workflows cannot load callees; resolve
		// reports that as an error rather than a runtime matrix boundary.
		return
	}
	for _, job := range parsed.Jobs {
		if job.Reusable == nil {
			continue
		}
		calleeSource, source, err := resolver.loadReusableWorkflow(ctx, current, job.Reusable.Uses)
		if err != nil {
			resolver.runtimeMatrixBoundary = true
			continue
		}
		calleeDepth := depth + 1
		key := calleeSource.identity.key()
		previousDepth, scanned := scannedAtDepth[key]
		if scanned && previousDepth <= calleeDepth {
			continue
		}
		if !scanned && len(scannedAtDepth) >= maxFlattenedJobs {
			// An incomplete discovery cannot prove that no unvisited callee has a
			// runtime matrix boundary. Reject before event metadata instead.
			resolver.runtimeMatrixBoundary = true
			return
		}
		scannedAtDepth[key] = calleeDepth
		callee, err := parseReusableWorkflow(calleeSource.displayPath, source)
		if err != nil {
			resolver.runtimeMatrixBoundary = true
			continue
		}
		resolver.discoverRuntimeMatrixBoundaries(ctx, calleeSource, callee, calleeDepth, scannedAtDepth)
	}
}

func (resolver *reusableResolver) resolve(ctx context.Context, frame callFrame) (reusableResolution, error) {
	path := frame.path()
	parsed := frame.workflow
	resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasRuntimeMatrixBoundary(parsed)
	jobs := make(map[string]workflow.Job, len(parsed.Jobs))
	for _, job := range parsed.Jobs {
		jobs[job.ID] = job
	}
	order, err := topologicalOrder(path, jobs)
	if err != nil {
		return reusableResolution{}, err
	}

	replacements := make(map[string]needBinding, len(jobs))
	var resolved []sourcedJob
	for _, id := range order {
		job := jobs[id]
		permissions := frame.jobPermissions(job.Permissions, resolver.rootPermissions)
		job.Permissions = permissions.permissions
		originalJob := job
		job, err = applyStaticInputs(path, job, frame.inputs.values)
		if err != nil {
			return reusableResolution{}, err
		}
		blockerDetailUnsafe := blockerFieldsChanged(originalJob, job) || matrixContainsExpressions(job.Matrix)
		// Root inputs come from the triggering event; expressions that remain
		// after static replacement are evaluated at runtime. Callee inputs
		// must resolve statically or defer to a caller need.
		if !frame.isRoot() {
			if err := rejectUnresolvedInputExpressions(path, job, frame.inputs.deferred); err != nil {
				return reusableResolution{}, err
			}
		}
		callNeedBindings, needBindings := frame.jobNeeds(job, replacements)
		if job.Reusable == nil {
			resolver.expanded++
			if resolver.expanded > maxFlattenedJobs {
				return reusableResolution{}, jobError(path, job, fmt.Sprintf("workflow job graph expands beyond %d jobs", maxFlattenedJobs))
			}
			flattened := frame.sourcedJob(job, resolver.workspaceRoot, permissions, needBindings, blockerDetailUnsafe)
			resolved = append(resolved, flattened)
			replacements[id] = needBinding{members: []string{flattened.ID}}
			continue
		}

		call := job.Reusable
		calleeGuards := frame.guards
		if strings.TrimSpace(job.If) != "" {
			conditionContext := resolver.context
			conditionContext.Inputs = frame.inputs.values
			conditionContext.Matrix = nil
			conditionContext.Strategy = nil
			if err := validateCompileSite(job.If, expression.ProfileCompileCallCondition, expression.ResultBoolean); err != nil {
				if blockerDetailUnsafe {
					err = suppressBlockerDetail(err)
				}
				return reusableResolution{}, locatedJobWrappedError(path, job, job.Span.Start.Line, job.Span.Start.Column, "reusable-workflow call condition", err)
			}
			reduced, err := reduceCompileSite(job.If, expression.ProfileCompileCallCondition, expression.ResultBoolean, conditionContext)
			if err != nil {
				if blockerDetailUnsafe {
					err = suppressBlockerDetail(err)
				}
				return reusableResolution{}, locatedJobWrappedError(path, job, job.Span.Start.Line, job.Span.Start.Column, "reduce reusable-workflow call condition", err)
			}
			condition := reduced.Source
			if reduced.Known {
				condition = fmt.Sprint(reduced.Value)
			}
			if err := validateCompileSite(condition, expression.ProfileCallCondition, expression.ResultBoolean); err != nil {
				if blockerDetailUnsafe {
					err = suppressBlockerDetail(err)
				}
				return reusableResolution{}, locatedJobWrappedError(path, job, job.Span.Start.Line, job.Span.Start.Column, "reusable-workflow call condition", err)
			}
			calleeGuards = append(cloneSourcedCallGuards(frame.guards), sourcedCallGuard{
				condition: condition, inputs: cloneReusableInputs(frame.inputs), needBindings: cloneNeedBindings(callNeedBindings),
			})
		}
		if frame.depth >= MaxReusableWorkflowDepth {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("reusable-workflow nesting exceeds maximum depth %d", MaxReusableWorkflowDepth))
		}
		calleeSource, source, err := resolver.loadReusableWorkflow(ctx, frame.source, call.Uses)
		if err != nil {
			var finding *ProcessingFinding
			if errors.As(err, &finding) {
				attributed := *finding
				attributed.Path, attributed.Line, attributed.Column, attributed.Job = path, call.Span.Start.Line, call.Span.Start.Column, job.ID
				attributed.Err = locatedJobWrappedError(path, job, call.Span.Start.Line, call.Span.Start.Column, "", finding.Err)
				return reusableResolution{}, &attributed
			}
			return reusableResolution{}, locatedJobWrappedError(path, job, call.Span.Start.Line, call.Span.Start.Column, "", err)
		}
		if (call.InheritSecrets || len(call.Secrets) != 0) && calleeSource.identity.kind != "workspace" {
			message := fmt.Sprintf("A secrets: map cannot forward secrets to a workflow in another repository. Reusable workflow %q is outside this repository, so no secrets were forwarded. Retrieve each secret by name with buildkite-agent secret get NAME in the jobs of that workflow, or copy the workflow into this repository's .github/workflows and use a secrets: map with a ./ call. If you need explicit secret mappings across repositories, log an issue on github.com/buildkite/buildkite-gha so we can prioritise it.", calleeSource.displayPath)
			if call.InheritSecrets {
				message = fmt.Sprintf("secrets: inherit cannot forward secrets to a workflow in another repository. Reusable workflow %q is outside this repository, so no secrets were forwarded. Retrieve each secret by name with buildkite-agent secret get NAME in the jobs of that workflow, or copy the workflow into this repository's .github/workflows and use secrets: inherit with a ./ call. If you need secrets: inherit across repositories, log an issue on github.com/buildkite/buildkite-gha so we can prioritise it.", calleeSource.displayPath)
			}
			return reusableResolution{}, &ProcessingFinding{
				Stage: StageGraph, Code: CodeGraphInvalid, Category: "compatibility",
				Path: path, Line: call.Span.Start.Line, Column: call.Span.Start.Column, Job: job.ID,
				Message: message,
				Err:     locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, message),
			}
		}
		if cycle := frame.cycle(calleeSource); cycle != "" {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, "reusable-workflow cycle detected: "+cycle)
		}
		callee, err := parseReusableWorkflow(calleeSource.displayPath, source)
		if err != nil {
			return reusableResolution{}, err
		}
		resolver.runtimeMatrixBoundary = resolver.runtimeMatrixBoundary || hasRuntimeMatrixBoundary(callee)
		if !callee.Callable {
			return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("reusable workflow %q does not declare on.workflow_call", call.Uses))
		}
		calleeSecrets, err := resolveCallSecretAuthority(path, job, call, callee, frame.secrets)
		if err != nil {
			return reusableResolution{}, err
		}
		calleeDigest := "sha256:" + sha256Sum(source)

		matrixContext := resolver.context
		matrixContext.Inputs = frame.inputs.values
		matrixContext.Matrix = nil
		matrixContext.Strategy = nil
		matrices, err := expandMatrix(path, job, matrixContext)
		if err != nil {
			return reusableResolution{}, err
		}
		if job.MaxParallel != nil && len(matrices) > 1 {
			return reusableResolution{}, jobError(path, job, "strategy.max-parallel on a reusable-workflow matrix cannot be preserved when the called jobs are flattened")
		}
		var members []string
		var callOutputs []needOutputBinding
		callNamespaces := make(map[string]struct{}, len(matrices))
		for _, matrix := range matrices {
			callInputs, err := resolveCallInputs(path, job, call, callee, frame.inputs, callNeedBindings, matrix, resolver.context)
			if err != nil {
				return reusableResolution{}, err
			}
			child, err := frame.child(callSite{
				job: job, call: call, matrix: matrix, instances: len(matrices),
				needs: needBindings, guards: calleeGuards, tokenPolicyNarrowed: permissions.tokenPolicyNarrowed,
				callee: calleeSource, calleeDigest: calleeDigest, calleeWorkflow: callee,
				inputs: callInputs, secrets: calleeSecrets,
			})
			if err != nil {
				return reusableResolution{}, err
			}
			if _, exists := callNamespaces[child.namespace]; exists {
				return reusableResolution{}, jobError(path, job, fmt.Sprintf("reusable-workflow matrix produces duplicate namespace %q", child.namespace))
			}
			callNamespaces[child.namespace] = struct{}{}
			if callee.Concurrency != nil {
				if len(calleeGuards) != 0 {
					return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, "called-workflow concurrency is unsupported for guarded reusable-workflow calls")
				}
				concurrencyContext := resolver.context
				concurrencyContext.Inputs = callInputs.values
				concurrencyContext.Matrix = nil
				concurrencyContext.Strategy = nil
				group, err := resolveConcurrency(calleeSource.displayPath, "", callee.Concurrency, concurrencyContext, nil)
				if err != nil {
					return reusableResolution{}, err
				}
				cancelInProgress, err := resolveWorkflowCancellation(calleeSource.displayPath, callee.Concurrency, concurrencyContext)
				if err != nil {
					return reusableResolution{}, err
				}
				if len(bindingMembers(needBindings)) != 0 {
					return reusableResolution{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, "called-workflow concurrency is unsupported for reusable-workflow calls with prerequisites")
				}
				child.gates = append(child.gates, WorkflowConcurrencyGate{ID: child.namespace, Group: group})
				if cancelInProgress && !resolver.warnedCancellation[child.callPosition] {
					resolver.warnedCancellation[child.callPosition] = true
					warning := workflowCancellationWarning(child.callPosition)
					warning.Job = job.ID
					resolver.warnings = append(resolver.warnings, warning)
				}
			}
			calleeResolution, err := resolver.resolve(ctx, child)
			if err != nil {
				message := "reusable workflow could not be resolved"
				detail := ""
				var finding *ProcessingFinding
				if errors.As(err, &finding) {
					message = finding.Message
					detail = finding.Detail
				}
				return reusableResolution{}, &ProcessingFinding{
					Stage: StageGraph, Code: CodeGraphInvalid, Category: "compatibility",
					Path: path, Line: call.Span.Start.Line, Column: call.Span.Start.Column, Job: job.ID,
					Message: message, Detail: detail,
					Err: locatedJobWrappedError(path, job, call.Span.Start.Line, call.Span.Start.Column,
						fmt.Sprintf("resolve reusable workflow %q", call.Uses), err),
				}
			}
			if permissions.jobPermissionsIgnored {
				for i := range calleeResolution.jobs {
					calleeResolution.jobs[i].jobPermissionsIgnored = true
				}
			}
			resolved = append(resolved, calleeResolution.jobs...)
			callOutputs = append(callOutputs, calleeResolution.outputs...)
			for _, calleeJob := range calleeResolution.jobs {
				members = append(members, calleeJob.ID)
			}
		}
		sort.Strings(members)
		sortNeedOutputBindings(callOutputs)
		replacements[id] = needBinding{members: members, projectOutputs: true, outputs: callOutputs}
	}
	outputs, err := resolveWorkflowCallOutputs(path, parsed.CallOutputs, jobs, replacements)
	if err != nil {
		return reusableResolution{}, err
	}
	return reusableResolution{jobs: resolved, outputs: outputs}, nil
}

func resolveCallSecretAuthority(path string, job workflow.Job, call *workflow.ReusableWorkflowCall, callee *workflow.Workflow, parent secretAuthority) (secretAuthority, error) {
	if call.InheritSecrets {
		for _, name := range sortedValueKeys(callee.CallSecrets) {
			declaration := callee.CallSecrets[name]
			if declaration.Required && !parent.has(name) {
				return secretAuthority{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("reusable workflow %q requires secret %q", call.Uses, declaration.Name))
			}
		}
		return cloneSecretAuthority(parent), nil
	}

	forwarded := secretAuthority{bindings: make(map[string]secretBinding, len(call.Secrets))}
	for _, target := range sortedValueKeys(call.Secrets) {
		mapping := call.Secrets[target]
		_, declared := callee.CallSecrets[target]
		if !declared {
			return secretAuthority{}, locatedJobError(path, job, mapping.Span.Start.Line, mapping.Span.Start.Column, fmt.Sprintf("secret mapping target %q is not declared by reusable workflow %q", target, call.Uses))
		}
		if target == "GITHUB_TOKEN" {
			return secretAuthority{}, locatedJobError(path, job, mapping.Span.Start.Line, mapping.Span.Start.Column, "GITHUB_TOKEN cannot be an explicit secret mapping target")
		}
		if mapping.Source == "GITHUB_TOKEN" {
			forwarded.bindings[target] = secretBinding{source: "GITHUB_TOKEN", token: true}
			continue
		}
		if binding, ok := parent.resolve(mapping.Source); ok {
			forwarded.bindings[target] = binding
		}
	}
	for _, name := range sortedValueKeys(callee.CallSecrets) {
		declaration := callee.CallSecrets[name]
		if declaration.Required && !forwarded.has(name) {
			return secretAuthority{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("reusable workflow %q requires secret %q", call.Uses, declaration.Name))
		}
	}
	return forwarded, nil
}

func (authority secretAuthority) resolve(alias string) (secretBinding, bool) {
	alias = strings.ToUpper(alias)
	if authority.unrestricted {
		return secretBinding{source: alias}, true
	}
	binding, ok := authority.bindings[alias]
	return binding, ok
}

func (authority secretAuthority) has(alias string) bool {
	_, ok := authority.resolve(alias)
	return ok
}

func cloneSecretAuthority(authority secretAuthority) secretAuthority {
	cloned := secretAuthority{unrestricted: authority.unrestricted}
	if authority.bindings != nil {
		cloned.bindings = make(map[string]secretBinding, len(authority.bindings))
		maps.Copy(cloned.bindings, authority.bindings)
	}
	return cloned
}

func effectivePermissions(job, workflowDefault, ceiling *workflow.Permissions, bounded bool) *workflow.Permissions {
	declared := job
	if declared == nil {
		declared = workflowDefault
	}
	if !bounded {
		if declared == nil {
			return defaultGitHubTokenPermissions()
		}
		return clonePermissions(declared)
	}
	if declared == nil {
		return clonePermissions(ceiling)
	}
	effective := &workflow.Permissions{Scopes: map[string]string{}, Span: declared.Span}
	if ceiling == nil {
		return effective
	}
	for name, access := range declared.Scopes {
		ceilingAccess, ok := ceiling.Scopes[name]
		if !ok {
			continue
		}
		if access == "write" && ceilingAccess == "read" {
			access = "read"
		}
		effective.Scopes[name] = access
	}
	return effective
}

func defaultGitHubTokenPermissions() *workflow.Permissions {
	// Provide the narrow permission needed by setup actions without inheriting
	// unobservable organization or repository settings that may grant writes.
	return &workflow.Permissions{Scopes: map[string]string{"contents": "read"}}
}

func clonePermissions(in *workflow.Permissions) *workflow.Permissions {
	if in == nil {
		return nil
	}
	out := &workflow.Permissions{Scopes: make(map[string]string, len(in.Scopes)), Span: in.Span}
	maps.Copy(out.Scopes, in.Scopes)
	return out
}

func repositoryPermissionsNarrowed(root, effective *workflow.Permissions) bool {
	for name, rootAccess := range root.Scopes {
		if name == "id-token" || rootAccess == "none" {
			continue
		}
		if effective.Scopes[name] != rootAccess {
			return true
		}
	}
	return false
}

func repositoryPermissionsDiffer(left, right *workflow.Permissions) bool {
	return repositoryPermissionsNarrowed(left, right) || repositoryPermissionsNarrowed(right, left)
}

func replacementNeeds(needs []string, replacements map[string]needBinding) map[string]needBinding {
	out := make(map[string]needBinding, len(needs))
	for _, need := range needs {
		out[need] = cloneNeedBinding(replacements[need])
	}
	return out
}

func bindingMembers(bindings map[string]needBinding) []string {
	seen := make(map[string]struct{})
	for _, binding := range bindings {
		for _, member := range binding.members {
			seen[member] = struct{}{}
		}
	}
	members := make([]string, 0, len(seen))
	for member := range seen {
		members = append(members, member)
	}
	sort.Strings(members)
	return members
}

func resolveWorkflowCallOutputs(path string, declarations map[string]workflow.CallOutput, jobs map[string]workflow.Job, replacements map[string]needBinding) ([]needOutputBinding, error) {
	var resolved []needOutputBinding
	for _, declarationID := range sortedValueKeys(declarations) {
		declaration := declarations[declarationID]
		if !callOutputNamePattern.MatchString(declaration.Name) {
			return nil, workflowCallOutputError(path, declaration, "has an invalid name")
		}
		root, reference, err := staticReference(declaration.Value)
		if err != nil || !strings.EqualFold(root, "jobs") || len(reference) != 3 || !strings.EqualFold(reference[1], "outputs") {
			return nil, workflowCallOutputError(path, declaration, "must be one static jobs.<job_id>.outputs.<output_name> reference")
		}
		job, ok := findWorkflowJob(jobs, reference[0])
		if !ok {
			return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references unknown job %q", reference[0]))
		}
		binding, ok := replacements[job.ID]
		if !ok {
			return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references unresolved job %q", reference[0]))
		}
		if !binding.projectOutputs {
			output, ok := findOutputName(job.Outputs, reference[2])
			if !ok {
				return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references undeclared output %q on job %q", reference[2], job.ID))
			}
			for _, member := range binding.members {
				resolved = append(resolved, needOutputBinding{name: declaration.Name, member: member, output: output, path: path, span: declaration.Span})
			}
			continue
		}

		found := false
		for _, output := range binding.outputs {
			if !strings.EqualFold(output.name, reference[2]) {
				continue
			}
			found = true
			output.name = declaration.Name
			output.path = path
			output.span = declaration.Span
			resolved = append(resolved, output)
		}
		if !found {
			return nil, workflowCallOutputError(path, declaration, fmt.Sprintf("references undeclared output %q on reusable job %q", reference[2], job.ID))
		}
	}
	sortNeedOutputBindings(resolved)
	return resolved, nil
}

func findWorkflowJob(jobs map[string]workflow.Job, name string) (workflow.Job, bool) {
	for _, job := range jobs {
		if strings.EqualFold(job.ID, name) {
			return job, true
		}
	}
	return workflow.Job{}, false
}

func findOutputName(outputs map[string]string, name string) (string, bool) {
	for output := range outputs {
		if strings.EqualFold(output, name) {
			return output, true
		}
	}
	return "", false
}

func sortNeedOutputBindings(outputs []needOutputBinding) {
	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].name != outputs[j].name {
			return outputs[i].name < outputs[j].name
		}
		if outputs[i].member != outputs[j].member {
			return outputs[i].member < outputs[j].member
		}
		return outputs[i].output < outputs[j].output
	})
}

func workflowCallOutputError(path string, output workflow.CallOutput, message string) error {
	return fmt.Errorf("%s:%d:%d: workflow_call output %q %s", path, output.Span.Start.Line, output.Span.Start.Column, output.Name, message)
}

func cloneNeedBindings(bindings map[string]needBinding) map[string]needBinding {
	if bindings == nil {
		return nil
	}
	cloned := make(map[string]needBinding, len(bindings))
	for name, binding := range bindings {
		cloned[name] = cloneNeedBinding(binding)
	}
	return cloned
}

func cloneNeedBinding(binding needBinding) needBinding {
	binding.members = append([]string(nil), binding.members...)
	binding.outputs = append([]needOutputBinding(nil), binding.outputs...)
	return binding
}

func cloneReusableInputs(inputs reusableInputs) reusableInputs {
	return reusableInputs{
		values:   cloneAnyMap(inputs.values),
		deferred: cloneNeedBindings(inputs.deferred),
	}
}

func cloneSourcedCallGuards(guards []sourcedCallGuard) []sourcedCallGuard {
	if guards == nil {
		return nil
	}
	cloned := make([]sourcedCallGuard, len(guards))
	for i, guard := range guards {
		cloned[i] = sourcedCallGuard{
			condition: guard.condition, inputs: cloneReusableInputs(guard.inputs), needBindings: cloneNeedBindings(guard.needBindings),
		}
	}
	return cloned
}

func namespacedJobID(namespace, id string) string {
	if namespace == "" {
		return id
	}
	return namespace + "." + id
}

func resolveCallInputs(path string, job workflow.Job, call *workflow.ReusableWorkflowCall, callee *workflow.Workflow, parentInputs reusableInputs, callNeeds map[string]needBinding, matrix map[string]any, context expression.CompileContext) (reusableInputs, error) {
	values := make(map[string]any, len(call.Inputs))
	deferredValues := make(map[string]needBinding)
	for _, name := range sortedValueKeys(call.Inputs) {
		value := call.Inputs[name]
		if _, ok := callee.CallInputs[name]; !ok {
			return reusableInputs{}, locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, fmt.Sprintf("input %q is not declared by reusable workflow %q", name, call.Uses))
		}
		resolved := value.Data
		if text, ok := resolved.(string); ok && strings.Contains(text, "${{") {
			if deferred, ok := forwardedDeferredInput(text, parentInputs.deferred); ok {
				deferredValues[name] = deferred
				continue
			}
			if deferred, ok := deferredNeedInput(text, callNeeds); ok {
				deferredValues[name] = deferred
				continue
			}
			var err error
			resolved, err = evaluateStaticCallValue(text, parentInputs.values, matrix, context)
			if err != nil {
				detail := fmt.Sprintf("Reusable-workflow input %q is not statically resolvable: %v", name, err)
				message := fmt.Sprintf("Reusable workflow input %q uses a value that is unavailable before jobs run. Replace it with a literal or an expression that does not depend on job results.", name)
				if need, _, ok := deferredNeedReference(text); ok {
					message = fmt.Sprintf("Reusable workflow input %q references job %q, but the call does not list it in needs. Add %q to the reusable-workflow call's needs.", name, need, need)
				} else if strings.Contains(err.Error(), `unsupported compile-time context "needs"`) {
					message = fmt.Sprintf("Reusable workflow input %q uses a needs expression in an unsupported form. Pass the whole value as exactly ${{ needs.<job>.outputs.<name> }}, with nothing around it. Only string inputs can take a needs value, and Buildkite resolves it before the called job runs, so the reference has to be the entire value rather than part of a larger expression. If you need a computed input from job outputs, log an issue on github.com/buildkite/buildkite-gha so we can prioritise it.", name)
				}
				return reusableInputs{}, &ProcessingFinding{
					Stage: StageGraph, Code: CodeGraphInvalid, Category: "compatibility",
					Path: path, Line: value.Span.Start.Line, Column: value.Span.Start.Column, Job: job.ID,
					Message: message, Detail: detail,
					Err: locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, detail),
				}
			}
		}
		values[name] = resolved
	}

	resolved := make(map[string]any, len(callee.CallInputs))
	deferred := make(map[string]needBinding, len(deferredValues))
	for _, name := range sortedValueKeys(callee.CallInputs) {
		declaration := callee.CallInputs[name]
		value, supplied := values[name]
		deferredValue, deferredSupplied := deferredValues[name]
		ok := supplied || deferredSupplied
		if deferredSupplied {
			if declaration.Type != "string" {
				span := call.Inputs[name].Span
				return reusableInputs{}, locatedJobError(path, job, span.Start.Line, span.Start.Column, fmt.Sprintf("deferred reusable-workflow input %q must be string", name))
			}
			deferred[name] = cloneNeedBinding(deferredValue)
			continue
		}
		if !ok && declaration.Default != nil {
			value, ok = declaration.Default.Data, true
			if text, isString := value.(string); isString && strings.Contains(text, "${{") {
				var err error
				value, err = evaluateCompileSite(text, expression.ProfileReusableInput, expression.ResultAny, context)
				if err != nil {
					return reusableInputs{}, locatedJobError(call.Uses, job, declaration.Default.Span.Start.Line, declaration.Default.Span.Start.Column, fmt.Sprintf("evaluate default for reusable-workflow input %q: %v", name, err))
				}
			}
		}
		if !ok {
			if declaration.Required {
				return reusableInputs{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("required reusable-workflow input %q is missing", name))
			}
			value = zeroInputValue(declaration.Type)
		}
		if !inputTypeMatches(declaration.Type, value) {
			locationPath := path
			span := call.Span
			if supplied {
				span = call.Inputs[name].Span
			} else if declaration.Default != nil {
				locationPath = call.Uses
				span = declaration.Default.Span
			}
			return reusableInputs{}, locatedJobError(locationPath, job, span.Start.Line, span.Start.Column, fmt.Sprintf("reusable-workflow input %q must be %s", name, declaration.Type))
		}
		if containsExpression(value) {
			return reusableInputs{}, locatedJobError(path, job, call.Span.Start.Line, call.Span.Start.Column, fmt.Sprintf("reusable-workflow input %q is not statically resolvable", name))
		}
		resolved[name] = value
	}
	return reusableInputs{values: resolved, deferred: deferred}, nil
}

func forwardedDeferredInput(value string, deferred map[string]needBinding) (needBinding, bool) {
	root, path, err := staticReference(value)
	if err != nil || !strings.EqualFold(root, "inputs") || len(path) != 1 {
		return needBinding{}, false
	}
	for name, binding := range deferred {
		if strings.EqualFold(name, path[0]) {
			return cloneNeedBinding(binding), true
		}
	}
	return needBinding{}, false
}

func deferredNeedInput(value string, needs map[string]needBinding) (needBinding, bool) {
	need, outputName, ok := deferredNeedReference(value)
	if !ok {
		return needBinding{}, false
	}
	for existing, binding := range needs {
		if !strings.EqualFold(existing, need) {
			continue
		}
		deferred := needBinding{members: append([]string(nil), binding.members...), projectOutputs: true}
		if !binding.projectOutputs {
			for _, member := range binding.members {
				deferred.outputs = append(deferred.outputs, needOutputBinding{name: "value", member: member, output: outputName})
			}
			return deferred, true
		}
		for _, output := range binding.outputs {
			if !strings.EqualFold(output.name, outputName) {
				continue
			}
			output.name = "value"
			deferred.outputs = append(deferred.outputs, output)
		}
		return deferred, true
	}
	return needBinding{}, false
}

func deferredNeedReference(value string) (need, output string, ok bool) {
	root, path, err := staticReference(value)
	if err != nil || !strings.EqualFold(root, "needs") || len(path) != 3 || !strings.EqualFold(path[1], "outputs") {
		return "", "", false
	}
	return path[0], path[2], true
}

func evaluateStaticCallValue(value string, inputs, matrix map[string]any, context expression.CompileContext) (any, error) {
	if match := staticValueExpression.FindStringSubmatch(value); match != nil {
		values := inputs
		if strings.EqualFold(match[1], "matrix") {
			values = matrix
		}
		valueName := match[2]
		if valueName == "" {
			valueName = match[3]
		}
		for name, value := range values {
			if strings.EqualFold(name, valueName) {
				if containsExpression(value) {
					return nil, fmt.Errorf("expression references runtime-dependent %s value %q", match[1], valueName)
				}
				return value, nil
			}
		}
		return nil, fmt.Errorf("expression references unavailable %s value %q", match[1], valueName)
	}
	resolved := replaceStaticInputs(value, inputs)
	if hasInputExpression(resolved) {
		return nil, fmt.Errorf("expression references an unavailable or unsupported input")
	}
	context.Matrix = matrix
	if err := validateCompileSite(resolved, expression.ProfileCompile, expression.ResultAny); err == nil {
		return evaluateCompileSite(resolved, expression.ProfileCompile, expression.ResultAny, context)
	}
	return evaluateCompileSite(resolved, expression.ProfileCompileTemplate, expression.ResultString, context)
}

func containsExpression(value any) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, "${{")
	case []any:
		if slices.ContainsFunc(value, containsExpression) {
			return true
		}
	case map[string]any:
		for _, element := range value {
			if containsExpression(element) {
				return true
			}
		}
	}
	return false
}

func sortedValueKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func zeroInputValue(inputType string) any {
	if inputType == "boolean" {
		return false
	}
	if inputType == "number" {
		return 0
	}
	return ""
}

func inputTypeMatches(inputType string, value any) bool {
	switch inputType {
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case int, int64, uint64, float64, json.Number:
			return true
		default:
			return false
		}
	case "string":
		_, ok := value.(string)
		return ok
	default:
		return false
	}
}

func applyStaticInputs(path string, job workflow.Job, inputs map[string]any) (workflow.Job, error) {
	if len(inputs) == 0 {
		return job, nil
	}
	job.Name = replaceStaticInputs(job.Name, inputs)
	job.If = replaceStaticInputCondition(job.If, inputs)
	job.DefaultShell = replaceStaticInputs(job.DefaultShell, inputs)
	job.DefaultWorkingDirectory = replaceStaticInputs(job.DefaultWorkingDirectory, inputs)
	if job.Concurrency != nil {
		concurrency := *job.Concurrency
		concurrency.Group = replaceStaticInputs(concurrency.Group, inputs)
		job.Concurrency = &concurrency
	}
	job.Env = replaceMapInputs(job.Env, inputs)
	job.Outputs = replaceMapInputs(job.Outputs, inputs)
	job.Services = append([]workflow.Service(nil), job.Services...)
	for i := range job.Services {
		container := job.Services[i].Container
		container.Image = replaceStaticInputs(container.Image, inputs)
		if container.Credentials != nil {
			credentials := *container.Credentials
			credentials.Username = replaceStaticInputs(credentials.Username, inputs)
			credentials.Password = replaceStaticInputs(credentials.Password, inputs)
			container.Credentials = &credentials
		}
		container.Env = replaceMapInputs(container.Env, inputs)
		container.Options = replaceStaticInputs(container.Options, inputs)
		container.Command = replaceStaticInputs(container.Command, inputs)
		container.Entrypoint = replaceStaticInputs(container.Entrypoint, inputs)
		container.Ports = replaceSliceInputs(container.Ports, inputs)
		container.Volumes = replaceSliceInputs(container.Volumes, inputs)
		job.Services[i].Container = container
	}
	job.RunsOn = append([]string(nil), job.RunsOn...)
	for i := range job.RunsOn {
		job.RunsOn[i] = replaceStaticInputs(job.RunsOn[i], inputs)
	}
	if job.RunsOnExpr != nil {
		resolved := replaceStaticInputs(job.RunsOnExpr.Text, inputs)
		if !strings.Contains(resolved, "${{") {
			job.RunsOn = []string{resolved}
			job.RunsOnExpr = nil
		} else {
			expr := *job.RunsOnExpr
			expr.Text = resolved
			job.RunsOnExpr = &expr
		}
	}
	if job.Matrix != nil {
		job.Matrix = cloneMatrixWithInputs(job.Matrix, inputs)
	}
	job.Steps = append([]workflow.Step(nil), job.Steps...)
	for i := range job.Steps {
		step := &job.Steps[i]
		step.Name = replaceStaticInputs(step.Name, inputs)
		step.Run = replaceStaticInputs(step.Run, inputs)
		step.Uses = replaceStaticInputs(step.Uses, inputs)
		step.Shell = replaceStaticInputs(step.Shell, inputs)
		step.WorkingDirectory = replaceStaticInputs(step.WorkingDirectory, inputs)
		if step.ContinueOnErrorExpression != "" {
			resolved, err := expression.SubstituteCompileInputs(step.ContinueOnErrorExpression, inputs)
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("resolve continue-on-error expression: %v", err))
			}
			if err := validateCompileSite(resolved, expression.ProfileStepControl, expression.ResultBoolean); err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("validate continue-on-error expression: %v", err))
			}
			reduced, err := reduceCompileSite(resolved, expression.ProfileReusableStepControl, expression.ResultAny, expression.CompileContext{})
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("evaluate continue-on-error expression: %v", err))
			}
			if !reduced.Known {
				step.ContinueOnErrorExpression = resolved
			} else if enabled, ok := reduced.Value.(bool); ok {
				step.ContinueOnError, step.ContinueOnErrorExpression = enabled, ""
			} else {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "continue-on-error expression must produce a boolean")
			}
		}
		if step.TimeoutMinutesExpression != "" {
			resolved, err := expression.SubstituteCompileInputs(step.TimeoutMinutesExpression, inputs)
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("resolve timeout-minutes expression: %v", err))
			}
			if err := validateCompileSite(resolved, expression.ProfileStepControl, expression.ResultNumber); err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("validate timeout-minutes expression: %v", err))
			}
			reduced, err := reduceCompileSite(resolved, expression.ProfileReusableStepControl, expression.ResultAny, expression.CompileContext{})
			if err != nil {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, fmt.Sprintf("evaluate timeout-minutes expression: %v", err))
			}
			if !reduced.Known {
				step.TimeoutMinutesExpression = resolved
			} else if minutes, ok := staticTimeoutMinutes(reduced.Value); ok {
				if minutes <= 0 || minutes > 360 || math.IsNaN(minutes) || math.IsInf(minutes, 0) {
					return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "timeout-minutes expression must produce a number greater than 0 and at most 360")
				}
				step.TimeoutMinutes, step.TimeoutMinutesExpression = minutes, ""
			} else {
				return job, locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "timeout-minutes expression must produce a number")
			}
		}
		step.If = replaceStaticInputCondition(step.If, inputs)
		step.Env = replaceMapInputs(step.Env, inputs)
		step.With = replaceMapInputs(step.With, inputs)
	}
	return job, nil
}

func blockerFieldsChanged(before, after workflow.Job) bool {
	if before.If != after.If || before.DefaultShell != after.DefaultShell || !slices.Equal(before.RunsOn, after.RunsOn) || expressionText(before.RunsOnExpr) != expressionText(after.RunsOnExpr) || !reflect.DeepEqual(before.Matrix, after.Matrix) {
		return true
	}
	for i := range before.Steps {
		if before.Steps[i].If != after.Steps[i].If || before.Steps[i].Uses != after.Steps[i].Uses || before.Steps[i].Shell != after.Steps[i].Shell {
			return true
		}
	}
	return false
}

func expressionText(value *expression.Expression) string {
	if value == nil {
		return ""
	}
	return value.Text
}

func staticTimeoutMinutes(value any) (float64, bool) {
	switch value := value.(type) {
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint64:
		return float64(value), true
	case float64:
		return value, true
	case json.Number:
		minutes, err := value.Float64()
		return minutes, err == nil
	default:
		return 0, false
	}
}

func replaceSliceInputs(values []string, inputs map[string]any) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = replaceStaticInputs(value, inputs)
	}
	return result
}

func rejectUnresolvedInputExpressions(path string, job workflow.Job, deferredInputs map[string]needBinding) error {
	jobValues := []string{job.Name}
	jobRuntimeValues := []string{job.DefaultShell, job.DefaultWorkingDirectory}
	jobRuntimeValues = appendMapValues(jobRuntimeValues, job.Env)
	jobRuntimeValues = appendMapValues(jobRuntimeValues, job.Outputs)
	if job.Concurrency != nil {
		jobValues = append(jobValues, job.Concurrency.Group)
	}
	jobValues = append(jobValues, job.RunsOn...)
	for _, service := range job.Services {
		container := service.Container
		jobValues = append(jobValues, container.Image, container.Options, container.Command, container.Entrypoint)
		jobValues = append(jobValues, container.Ports...)
		jobValues = append(jobValues, container.Volumes...)
		jobValues = appendMapValues(jobValues, container.Env)
		if container.Credentials != nil {
			jobValues = append(jobValues, container.Credentials.Username, container.Credentials.Password)
		}
	}
	if job.RunsOnExpr != nil {
		jobValues = append(jobValues, job.RunsOnExpr.Text)
	}
	if job.Matrix != nil {
		if job.Matrix.Expression != nil {
			jobValues = append(jobValues, job.Matrix.Expression.Text)
		}
		if job.Matrix.IncludeExpression != nil {
			jobValues = append(jobValues, job.Matrix.IncludeExpression.Text)
		}
		if job.Matrix.ExcludeExpression != nil {
			jobValues = append(jobValues, job.Matrix.ExcludeExpression.Text)
		}
		for _, row := range job.Matrix.Rows {
			if row.Expression != nil {
				jobValues = append(jobValues, row.Expression.Text)
			}
			for _, value := range row.Values {
				if containsInputExpression(value.Data) {
					return locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, "reusable-workflow input expression is not statically resolvable")
				}
			}
		}
		for _, combinations := range [][]workflow.MatrixCombination{job.Matrix.Include, job.Matrix.Exclude} {
			for _, combination := range combinations {
				for _, value := range combination.Values {
					if containsInputExpression(value.Data) {
						return locatedJobError(path, job, value.Span.Start.Line, value.Span.Start.Column, "reusable-workflow input expression is not statically resolvable")
					}
				}
			}
		}
	}
	if slices.ContainsFunc(jobValues, hasInputExpression) {
		return jobError(path, job, "reusable-workflow input expression is not statically resolvable")
	}
	if hasUnresolvedConditionInput(job.If, deferredInputs) {
		return jobError(path, job, "reusable-workflow input expression is not statically resolvable")
	}
	for _, value := range jobRuntimeValues {
		if hasUnresolvedTemplateInput(value, deferredInputs) {
			return jobError(path, job, "reusable-workflow input expression is not statically resolvable")
		}
	}
	for _, step := range job.Steps {
		if hasInputExpression(step.Uses) {
			return locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "reusable-workflow action reference input expression is not statically resolvable")
		}
		if hasUnresolvedConditionInput(step.If, deferredInputs) {
			return locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "reusable-workflow input expression is not statically resolvable")
		}
		stepValues := []string{step.Name, step.Run, step.Shell, step.WorkingDirectory, step.ContinueOnErrorExpression, step.TimeoutMinutesExpression}
		stepValues = appendMapValues(stepValues, step.Env)
		stepValues = appendMapValues(stepValues, step.With)
		for _, value := range stepValues {
			if hasUnresolvedTemplateInput(value, deferredInputs) {
				return locatedJobError(path, job, step.Span.Start.Line, step.Span.Start.Column, "reusable-workflow input expression is not statically resolvable")
			}
		}
	}
	return nil
}

func appendMapValues(out []string, values map[string]string) []string {
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func containsInputExpression(value any) bool {
	switch value := value.(type) {
	case string:
		return hasInputExpression(value)
	case []any:
		if slices.ContainsFunc(value, containsInputExpression) {
			return true
		}
	case map[string]any:
		for _, element := range value {
			if containsInputExpression(element) {
				return true
			}
		}
	}
	return false
}

func hasInputExpression(value string) bool {
	usesInputs, err := referencesContext(value, expression.ProfilePartialTemplate, "inputs", false)
	return err != nil || usesInputs
}

func hasUnresolvedTemplateInput(value string, deferredInputs map[string]needBinding) bool {
	resolved, err := expression.SubstituteCompileInputs(value, deferredInputPlaceholders(deferredInputs))
	if err != nil {
		return true
	}
	usesInputs, err := referencesContext(resolved, expression.ProfilePartialTemplate, "inputs", true)
	return err != nil || usesInputs
}

func hasUnresolvedConditionInput(value string, deferredInputs map[string]needBinding) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	template := value
	if !strings.Contains(value, "${{") {
		template = "${{ " + value + " }}"
	}
	return hasUnresolvedTemplateInput(template, deferredInputs)
}

func deferredInputPlaceholders(inputs map[string]needBinding) map[string]any {
	placeholders := make(map[string]any, len(inputs))
	for name := range inputs {
		placeholders[name] = "deferred"
	}
	return placeholders
}

func cloneMatrixWithInputs(matrix *workflow.Matrix, inputs map[string]any) *workflow.Matrix {
	out := *matrix
	out.Expression = cloneMatrixExpressionWithInputs(matrix.Expression, inputs)
	out.IncludeExpression = cloneMatrixExpressionWithInputs(matrix.IncludeExpression, inputs)
	out.ExcludeExpression = cloneMatrixExpressionWithInputs(matrix.ExcludeExpression, inputs)
	out.Rows = append([]workflow.MatrixRow(nil), matrix.Rows...)
	for i := range out.Rows {
		out.Rows[i].Expression = cloneMatrixExpressionWithInputs(matrix.Rows[i].Expression, inputs)
		out.Rows[i].Values = append([]workflow.Value(nil), out.Rows[i].Values...)
		for j := range out.Rows[i].Values {
			out.Rows[i].Values[j].Data = resolveAuthoredMatrixInputs(out.Rows[i].Values[j].Data, inputs)
		}
	}
	out.Include = cloneMatrixCombinations(matrix.Include, inputs)
	out.Exclude = cloneMatrixCombinations(matrix.Exclude, inputs)
	return &out
}

func cloneMatrixExpressionWithInputs(expr *expression.Expression, inputs map[string]any) *expression.Expression {
	if expr == nil {
		return nil
	}
	out := *expr
	if resolved, err := expression.SubstituteCompileInputs(expr.Text, inputs); err == nil {
		out.Text = resolved
	}
	return &out
}

func cloneMatrixCombinations(combinations []workflow.MatrixCombination, inputs map[string]any) []workflow.MatrixCombination {
	out := make([]workflow.MatrixCombination, len(combinations))
	for i, combination := range combinations {
		out[i] = combination
		out[i].Values = make(map[string]workflow.Value, len(combination.Values))
		for name, value := range combination.Values {
			value.Data = resolveAuthoredMatrixInputs(value.Data, inputs)
			out[i].Values[name] = value
		}
	}
	return out
}

func resolveAuthoredMatrixInputs(value any, inputs map[string]any) any {
	switch value := value.(type) {
	case string:
		resolved, err := expression.SubstituteCompileInputs(value, inputs)
		if err != nil || resolved == value {
			return value
		}
		if err := validateCompileSite(resolved, expression.ProfileCompile, expression.ResultAny); err == nil {
			if reduced, err := reduceCompileSite(resolved, expression.ProfileCompile, expression.ResultAny, expression.CompileContext{}); err == nil && reduced.Known {
				evaluated := reduced.Value
				if text, ok := evaluated.(string); !ok || !strings.Contains(text, "${{") {
					return evaluated
				}
			}
		}
		if reduced, err := reduceCompileSite(resolved, expression.ProfilePartialTemplate, expression.ResultString, expression.CompileContext{}); err == nil {
			if reduced.Known {
				return reduced.Value
			}
			return reduced.Source
		}
		return value
	case []any:
		resolved := make([]any, len(value))
		for i, item := range value {
			resolved[i] = resolveAuthoredMatrixInputs(item, inputs)
		}
		return resolved
	case map[string]any:
		resolved := make(map[string]any, len(value))
		for _, key := range sortedKeys(value) {
			resolved[key] = resolveAuthoredMatrixInputs(value[key], inputs)
		}
		return resolved
	default:
		return value
	}
}

func replaceMapInputs(values map[string]string, inputs map[string]any) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for name, value := range values {
		out[name] = replaceStaticInputs(value, inputs)
	}
	return out
}

func replaceStaticInputCondition(value string, inputs map[string]any) string {
	match := staticInputCondition.FindStringSubmatch(value)
	if match == nil {
		if !strings.Contains(value, "${{") {
			usesInputs, err := referencesContext(value, expression.ProfileCompileStepCondition, "inputs", false)
			if err == nil && usesInputs {
				return replaceStaticInputs("${{ "+value+" }}", inputs)
			}
		}
		return replaceStaticInputs(value, inputs)
	}
	inputName := ""
	for _, candidate := range match[1:] {
		if candidate != "" {
			inputName = candidate
			break
		}
	}
	for name, input := range inputs {
		if strings.EqualFold(name, inputName) {
			if text, ok := input.(string); ok {
				return "'" + strings.ReplaceAll(text, "'", "''") + "'"
			}
			return fmt.Sprint(input)
		}
	}
	return value
}

func replaceStaticInputs(value string, inputs map[string]any) string {
	resolved, err := expression.SubstituteCompileInputs(value, inputs)
	if err != nil || resolved == value {
		return value
	}
	if reduced, err := reduceCompileSite(resolved, expression.ProfilePartialTemplate, expression.ResultString, expression.CompileContext{}); err == nil {
		if reduced.Known {
			return reduced.Value.(string)
		}
		return reduced.Source
	}
	return value
}

func isRepositoryWorkflowPath(path string) bool {
	workflowDir := filepath.Dir(filepath.Clean(path))
	return filepath.Base(workflowDir) == "workflows" && filepath.Base(filepath.Dir(workflowDir)) == ".github"
}

func workflowRepository(path string) (string, string, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", fmt.Errorf("resolve workflow path: %w", err)
	}
	workflowDir := filepath.Dir(absPath)
	if filepath.Base(workflowDir) != "workflows" || filepath.Base(filepath.Dir(workflowDir)) != ".github" {
		return "", "", fmt.Errorf("%s: local reusable workflows require the caller under .github/workflows", path)
	}
	repositoryDir := filepath.Dir(filepath.Dir(workflowDir))
	root, err := filepath.EvalSymlinks(repositoryDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository root for %q: %w", path, err)
	}
	canonicalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("resolve workflow path %q: %w", path, err)
		}
		relative, relativeErr := filepath.Rel(repositoryDir, absPath)
		if relativeErr != nil {
			return "", "", fmt.Errorf("resolve workflow path %q: %w", path, relativeErr)
		}
		canonicalPath = filepath.Join(root, relative)
	}
	if err := requireWithinRepository(root, canonicalPath); err != nil {
		return "", "", fmt.Errorf("resolve workflow path %q: %w", path, err)
	}
	return root, canonicalPath, nil
}

func repositoryWorkflowPath(root, canonicalPath string) (string, error) {
	relative, err := filepath.Rel(root, canonicalPath)
	if err != nil {
		return "", fmt.Errorf("locate workflow %q in repository: %w", canonicalPath, err)
	}
	if err := requireWithinRepository(root, canonicalPath); err != nil {
		return "", fmt.Errorf("locate workflow %q in repository: %w", canonicalPath, err)
	}
	return "./" + filepath.ToSlash(relative), nil
}

func requireWithinRepository(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved path %q escapes repository root %q", path, root)
	}
	return nil
}

func matrixDigest(matrix map[string]any) (string, error) {
	canonical, err := json.Marshal(matrix)
	if err != nil {
		return "", err
	}
	digest := sha256Sum(canonical)
	return digest[:12], nil
}

func sha256Sum(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("%x", digest[:])
}
