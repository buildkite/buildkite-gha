# Expression authority architecture

## Status

The compiler and runtime use a shared expression engine and a normalized
execution program. Job plan schema v2 requires this program. The runtime
rejects plans without it.

This document records why the architecture exists, its security invariants,
the implemented boundary, and remaining design work. Track delivery and
ownership in Linear.

## Problem

Expression evaluation originally had no single owner. The compiler inventoried
workflow fields, inspected action metadata, and reconstructed enough lifecycle
behavior to decide whether a job could receive credentials. The runtime loaded
the same metadata and independently evaluated input defaults, lifecycle
conditions, composite steps, and outputs.

Changing one expression-bearing field required updates across parsing,
validation, authority planning, and runtime execution. Each implementation had
to agree on:

- the available contexts and functions
- result types and coercion
- lazy operators and function arguments
- ordered input resolution
- action preparation, main, and post ordering
- workflow-authored and action-authored authority

[PR #339](https://github.com/buildkite/buildkite-gha/pull/339) exposed the
cost. Supporting `github.token` in composite metadata required separate
reachability logic for inputs, conditions, pure functions, nested actions, and
preparation. Review found paths where planning and execution reached different
branches or fields.

This disagreement is a security problem. Authority planning must remain
conservative without granting credentials to known-unreachable paths:

- A known-false or provider-inapplicable branch must not grant token authority.
- A branch that depends on an unknown runtime value may grant token authority.
- Action metadata cannot grant ordinary secret authority.
- Composite metadata cannot grant `github.token` authority.
- Workflow-authored `toJSON(github)` and direct `github.token` access retain
  distinct provenance from action-authored access.

## Design

The architecture separates source parsing, expression policy, normalized
execution, and adapters:

| Layer | Owner | Responsibility |
| --- | --- | --- |
| Source model | `internal/workflow`, `internal/action/metadata` | Parse authored workflow and action metadata. |
| Expression engine | `internal/expression` | Validate, reduce, analyze, and evaluate expressions using closed profiles. |
| Execution program | `internal/program` | Own every runtime expression site, its position, and the resolved action graph. |
| Compiler adapter | `internal/compiler` | Lower source models, analyze authority, and write the immutable plan. |
| Runtime adapter | `internal/runtime` | Verify immutable source and execute the normalized program with concrete values. |

The data flow is:

```text
workflow and action metadata
        |
        v
compiler lowering + expression profiles
        |
        v
digest-bound normalized execution program
        |                         |
        v                         v
abstract authority analysis   concrete runtime evaluation
```

### Expression profiles

An expression site selects a closed semantic profile. The profile owns:

- whether the source is an expression or template
- available contexts and functions
- token access policy
- expected result type
- workflow or action provenance
- authority purpose

The same engine validates and evaluates a profile. Adding a context or function
to one field does not implicitly add it to another field.

Program sites serialize source text and diagnostic location. Their position in
the program derives profile, result type, provenance, and purpose after decode.
This prevents a plan from claiming broader semantics in serialized profile
fields.

Validation is exhaustive. Unsupported syntax and prohibited authority fail
even when concrete or abstract evaluation would skip the branch.

### Abstract evaluation

Authority planning uses the same expression traversal as concrete evaluation
with an abstract value domain:

```go
type AbstractValue struct {
	Known bool
	Value any
}

type Analysis struct {
	Value   AbstractValue
	Effects Effects
}
```

Known values preserve normal evaluation. Unknown runtime values join effects
from every reachable branch. Unknown means a valid runtime dependency, not an
evaluation or validation failure.

The abstract domain must satisfy:

```text
concrete effects ⊆ abstract effects
fully known abstract effects = concrete effects
effects(refined context) ⊆ effects(less-known context)
```

The first property prevents missed authority. The other properties prevent an
implementation that always requests every credential from appearing sound
while violating known-false and provider guard boundaries.

Ordinary secrets intentionally use exhaustive static inventory for
compatibility. This includes the existing exception for a secret used only by
a declared optional action input. Changing ordinary secrets to reachable-only
authority requires a separate compatibility and security decision.

### Normalized execution program

The compiler lowers each expanded job and resolved action graph into an
immutable program before authority planning. The parsed workflow remains a
source-oriented compiler input; it is not the runtime execution model.

The program owns:

- reusable-workflow call guards and the job condition
- job environment, defaults, container, services, and outputs
- step conditions, controls, environment, names, commands, and invocations
- ordered action inputs and defaults
- JavaScript and Docker lifecycle fields
- composite steps, child lock selectors, outputs, and post behavior

Resolved action programs are keyed by immutable action lock ID. Source locks
continue to bind repository, commit, path, and tree digest. Plan validation
requires action programs and child invocations to reference valid locks.

The runtime verifies source trees and executable entrypoints. It uses the
normalized action program for semantics and does not parse action metadata into
a second execution model. Workspace actions created by checkout remain lazy;
container preparation classifies them from their normalized program rather
than resolving their source before checkout.

Reusable workflows flatten before program construction. Direct and called
jobs therefore use the same lowering and authority paths.

## Security invariants

The following boundaries are deliberate:

- The plan authorizes credentials before runtime starts.
- Runtime-only token retrieval cannot replace plan authority, capability, or
  permission decisions.
- Commands and workflow-command mutations produce unknown planning values.
  Later token authority joins conservatively.
- Action metadata cannot introduce ordinary secret authority.
- Composite-authored expressions cannot introduce `github.token` authority.
- Whole-context `github` serialization is allowed only by profiles that admit
  it and retains its author provenance.
- Validation inspects unreachable branches for unsupported or prohibited
  expressions.
- Source digest and entrypoint verification remain independent of normalized
  expression semantics.
- The normalized program and its action graph remain covered by the job-plan
  digest.

This architecture does not broaden expression syntax, allow dynamic secret
access, make private actions available, or weaken immutable source
verification.

## Implemented outcome

The rebuild delivered:

- one profile-driven expression engine for validation, reduction, abstract
  analysis, and concrete evaluation
- normalized workflow and resolved-action programs in plan schema v2
- positional derivation of non-serialized site semantics
- compiler authority inventory over normalized sites
- runtime evaluation of normalized sites without production projection
  fallbacks
- immutable source verification independent from action semantics
- test-only legacy and hosted differential oracles
- fuzz coverage for known abstract values, runtime templates, action defaults,
  and event-dependent authority effects

Production code has no legacy expression or metadata-reconstruction fallback.
Test-only entry points remain comparison oracles and fixture adapters.

## Further design work

These items preserve or complete the architecture. They are not compatibility
commitments.

| Item | Current boundary | Desired outcome |
| --- | --- | --- |
| Share lifecycle interpretation | Normalized sites are shared, but workflow authority analysis, action authority analysis, and runtime orchestration still model parts of guards, composite traversal, and lifecycle ordering separately. | Define lifecycle control flow once and expose abstract and concrete adapters without moving runtime side effects into the compiler. |
| Generalize abstract-domain law tests | Fuzz tests cover important profiles and value shapes, but do not exhaustively exercise refinement monotonicity across every profile and program position. | Generate profile-valid expressions and contexts, then verify soundness, known-value equality, and monotonic narrowing systematically. |
| Retrieve authorized tokens on demand | Plans authorize token access before execution, and runtime may prepare an authorized token for a path that is skipped later. | Avoid minting an unused token while retaining plan-level authorization and redaction guarantees. |
| Keep field ownership exhaustive | Positional walkers define current program sites. A new program field still requires validation that compiler lowering, authority inventory, and runtime use the same position. | Add structural coverage that fails when an expression-bearing program field is not visited exactly once. |

Any lifecycle unification must retain phase-specific behavior. Planning treats
command outputs, environment mutation, action state, and runtime results as
unknown. Runtime performs those effects and preserves preparation, main, post
registration, and reverse post execution.

## Verification

Expression changes require tests for:

- concrete effects being a subset of abstract effects
- exact abstract and concrete equality for fully known contexts
- monotonic effect narrowing as unknown values become known
- known, unknown, and unavailable context values
- lazy `&&`, `||`, `case()`, and pure-function arguments
- direct token and whole-context provenance
- exhaustive validation of unreachable prohibited references

Program changes require tests for:

- every supported workflow expression position
- GitHub and Origin provider guards
- ordered defaults and forwarded inputs
- nested composite preparation, main, output, and post behavior
- reusable-workflow expansion and deferred inputs
- tokenless and authorized-but-skipped token paths
- plan encoding, validation, and action source verification
- checkout-created lazy workspace actions in job containers

Run `mise run check` for every change. Run the hosted expression differential
oracle when expression semantics change. See
[Development](development.md#verify-runtime-behavior) for the commands.

The architectural acceptance test remains: adding an expression-bearing
operation defines its profile and execution position once. Authority planning
and runtime inherit that definition without another field-specific expression
scanner.
