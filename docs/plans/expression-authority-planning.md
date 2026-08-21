# Expression authority planning

## Problem

The compiler and runtime do not use one representation of expression-bearing
execution. The compiler inventories workflow fields in
`requiredSecrets`, inspects action metadata in `inspectInvocation`, and
reconstructs enough runtime behavior to decide whether a job can receive
`github.token`. The runtime reloads action metadata and independently evaluates
input defaults, lifecycle conditions, composite steps, and outputs.

This makes credential planning depend on duplicated knowledge of:

- every expression-bearing field
- each field's expression surface and provenance
- ordered input resolution
- lazy operator and function semantics
- action preparation, main, and post ordering

[PR #339](https://github.com/buildkite/buildkite-gha/pull/339) demonstrates the
cost. Supporting `github.token` in composite metadata required separate
reachability logic for inputs, conditions, pure functions, nested actions, and
preparation. Review repeatedly found cases where planning and execution reached
different branches or fields.

The security contract requires conservative but precise planning. A known-false
or provider-inapplicable branch must not grant token authority. A branch that
depends on an unknown runtime value may grant it. Composite metadata cannot
grant ordinary secret authority, and whole-context `github` serialization has
different authority rules from direct `github.token` access.

## Proposed approach

Use one expression semantic core and one normalized execution program for both
planning and runtime.

### Abstract expression evaluation

Extend the existing semantic evaluator with an abstract value domain:

```go
type AbstractValue struct {
	Known bool
	Value any
}

type Analysis struct {
	Value   AbstractValue
	Effects Effects
}

type Validation struct {
	SecretReferences []string
}
```

Concrete and abstract evaluation share expression-tree traversal, operator
semantics, coercion, and function argument selection. Abstract context values
are either known or unknown. Unknown branches join their possible effects;
known short-circuits discard effects from branches runtime cannot evaluate.
Implement these as explicit concrete and abstract domains behind the shared
traversal, not by passing unknown sentinel values through concrete `any` value
helpers.

Reachable authority effects retain provenance. At minimum, distinguish:

- direct `github.token`
- workflow-authored `toJSON(github)`
- composite-authored `toJSON(github)`

Validation remains an exhaustive pass over every AST branch. Unsupported
syntax and prohibited authority must fail even when concrete or abstract
evaluation would skip the branch. It also inventories statically named ordinary
secrets independently of reachable effects. `Unknown` means a valid runtime
dependency, not an evaluation or validation failure.

Keep ordinary secret behavior unchanged during this migration. Validation
inventories statically named workflow secrets even when a known branch skips
their expressions; authority policy retains all existing filters, including the
exception for a secret used only by a declared optional action input, which
resolves as empty unless required elsewhere. Prohibited composite secret
references remain errors in unreachable branches. Reachability only narrows
effects whose existing contract requires it, including `github.token`. Any
later change to ordinary-secret reachability needs a separate compatibility and
security decision.

The abstract domain must satisfy both soundness and useful precision:

```text
concrete effects ⊆ abstract effects
fully known abstract effects = concrete effects
effects(refined context) ⊆ effects(less-known context)
```

The first property prevents missed authority. The other two prevent an
implementation that always requests every credential from satisfying the
soundness check while violating known-false and provider guard boundaries.

### Normalized execution program

After reusable-workflow expansion and action resolution, lower each job and its
resolved action graph into an immutable execution program. Keep the parsed
workflow model as source-oriented compiler input. The normalized program owns
runtime expression sites and their control flow.

Each expression site records:

- source expression or template
- fixed expression surface
- expected result type
- workflow or action-metadata provenance
- source location for diagnostics

Represent lifecycle behavior structurally rather than attaching phase labels
to a flat field list. The program needs operations equivalent to:

- evaluate a typed or template expression
- guard a sequence of operations
- resolve ordered action inputs and defaults
- invoke an action
- register a post action
- enter preparation, main, and post phases

The shared program interpreter has two adapters:

- the planning adapter supplies abstract context values and collects authority
- the runtime adapter supplies concrete values and performs action or command
  execution

This keeps lifecycle ordering in one module while allowing planning and runtime
to perform different effects at its seams.

Commands and workflow-command mutations are not predictable during planning.
The planning adapter treats their outputs, environment changes, and state as
unknown, then joins later authority conservatively. The operation graph remains
finite and retains the existing workflow, job, step, and nested-action limits.

Resolved action programs are stored by action lock ID in the job plan. Source
locks continue to bind repositories, commits, paths, and tree digests. Runtime
continues to verify source content and executable entrypoints, but does not
reload metadata to derive a second execution model. Plan validation requires
every action program and child invocation to reference an existing lock, and
the encoded program remains covered by the job-plan digest.

Reusable workflows continue to flatten before plan construction. Their
expanded jobs then use the same normalization path as direct jobs, eliminating
separate authority treatment for called-workflow fields.

## Scope

This work centralizes expression reachability, field ownership, and action
lifecycle ordering. It does not:

- broaden supported expression syntax or contexts
- change workflow-token permission policy
- allow dynamic or whole-context secret access
- make private actions or reusable workflows available
- replace immutable source verification
- use runtime-only token minting as an authorization decision

On-demand token resolution may later avoid minting an authorized but unused
token. It cannot replace plan-level authority, capability, or permission
decisions.

## Delivery slices

### 1. Share concrete and abstract expression semantics

Add the abstract value and effect domain under `internal/expression`. Run it
through the existing semantic evaluator's traversal and pure-function
implementation. Preserve all exported evaluation behavior and diagnostics.

Move token reachability and known-condition cases onto abstract evaluation.
Keep existing compiler field inventories temporarily. Delete replaced
token-specific AST recursion once expression and compiler tests pass unchanged.

This slice is a behavior-preserving architectural refactor. It removes one
source of semantic drift but does not claim complete field coverage.

### 2. Normalize workflow execution fields

Add the normalized program model and lower expanded `JobInstance` values into
it before plan authorization. Include job and step conditions, defaults,
environment, outputs, services, containers, typed controls, and action
invocations.

Use test-only differential fixtures to compare legacy runtime results with the
program runtime adapter. Do not add production fallback between representations.

### 3. Normalize resolved actions

Lower action input declarations, ordered defaults, lifecycle conditions,
composite steps, outputs, and child selectors into action programs keyed by
lock ID. Model preparation separately from main-step reachability, including
environment evaluation before JavaScript `pre-if`, composite child preparation,
conditional post registration, and reverse post execution.

Derive action authority by abstractly executing this program. Runtime executes
the same program with the concrete adapter.

### 4. Cut over the plan contract

Bump the plan schema and require normalized execution programs. Reject plans
that combine normalized authority data with legacy raw execution fields.

Delete:

- `requiredSecrets` field enumeration
- `inspectInvocation` lifecycle simulation
- token-specific reachability walkers
- runtime action-metadata interpretation

Keep source digest and entrypoint verification independent of the normalized
metadata contract.

## Verification

Test the expression module through its concrete and abstract interfaces:

- property and fuzz tests for `concrete effects ⊆ abstract effects`
- exact effect equality for fully known contexts
- monotonic effect narrowing as unknown values become known
- known, unknown, and unavailable context values
- `&&`, `||`, `case()`, and pure-function argument laziness
- direct token and whole-context provenance
- exhaustive validation of unreachable prohibited references

Test the normalized program through both adapters:

- every supported workflow expression surface
- GitHub and Origin provider guards
- ordered defaults and forwarded inputs
- nested composite preparation, main, output, and post behavior
- reusable-workflow expansion and deferred inputs
- tokenless jobs and authorized-but-runtime-skipped token paths
- plan encoding, validation, and action source verification

Run `mise run check` for every slice. Run the GitHub-hosted expression
differential oracle when expression semantics change.

The architectural acceptance test is that adding an expression-bearing
operation defines its surface and execution position once. Planning and runtime
must inherit it without adding another field-specific authority scanner.
