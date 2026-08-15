# GitHub Actions expression parity

## Problem

`buildkite-gha` has four expression evaluators with different semantics:

- compile-time graph expressions in `internal/expression/compile.go`
- job and step conditions in `internal/expression/condition.go`
- runtime interpolation in `internal/expression/runtime.go`
- action input defaults in `internal/expression/action_input_default.go`

The parser represents most GitHub Actions expression syntax, but the evaluators
implement different subsets. Adding each operator or function to each evaluator
would increase semantic drift and make parity difficult to verify.

Parity means matching GitHub's expression language where `buildkite-gha` has an
equivalent value and execution phase. It does not mean inventing values for
GitHub-specific contexts or allowing every context in every workflow key.

## Approach

Keep actionlint as the parser. Replace duplicated semantic logic with one
internal evaluator, fixed workflow-surface policies, and phase-specific value
resolvers.

The evaluator owns:

- values, truthiness, and conversion
- property and index access
- operators and short-circuiting
- pure built-in functions

Each entry point selects a fixed workflow surface and evaluation phase. The
surface policy defines statically legal contexts and special functions. The
phase resolver supplies values available at compile time or runtime. Policies
do not resolve concrete values or perform authority analysis.

A separate validator traverses every AST branch before evaluation so
short-circuiting cannot hide unsupported syntax. Invoke validation only at the
existing validation points until a delivery slice intentionally changes that
contract.

Before plan serialization, a compile-time reducer replaces every
`github.event`-dependent subtree that can be resolved from the immutable event
snapshot. It then validates the residual expression against its runtime
surface. Plan construction rejects any retained `github.event` reference.

Condition wrappers continue to own the implicit `success()` guard and status
function detection. Keep template scanning, rendered-string conversion, and
typed workflow-key conversion outside the evaluator. Keep status functions and
`hashFiles()` as runtime callbacks.

## Security boundaries

Expression parity must preserve these invariants:

- Authority analysis remains independent of policy validation and semantic
  evaluation.
- Before any newly accepted AST shape reaches evaluation, `SecretReferences`
  and `ReferencesGitHubToken` must enumerate its authority statically or reject
  it.
- Every expression-bearing field retained in a plan is scanned for both
  `secrets.<name>` and `github.token`.
- Each secret reference must name exactly one statically identifiable secret
  before execution.
- Dynamic, filtered, or whole-context `secrets` access remains unsupported.
- Direct `github.token` references use the same scoped workflow-token contract
  as `secrets.GITHUB_TOKEN`. They are unavailable outside step execution.
- Whole-context or dynamic `github` access remains unsupported until token
  authority and redaction are provably safe.
- `github.event` remains compile-time only because generated plans do not retain
  the event payload.
- Missing Buildkite equivalents remain unsupported instead of receiving
  fabricated GitHub values.

## Delivery slices

### 1. Lock contracts and conformance fixtures

Add table-driven tests from GitHub's expression documentation and the open
source Actions runner. Cover:

- literal parsing and string escaping
- truthiness and result conversion
- numeric coercion, `NaN`, and case-insensitive string comparison
- operator precedence and short-circuiting
- legal missing members versus prohibited contexts
- property access, indexing, filters, and aggregate identity
- built-in function arguments, laziness, and results
- workflow-key context and special-function availability
- compile-time event reduction and the no-event-in-plan invariant
- secret and token authority for every accepted AST shape

Record parser feasibility explicitly. Pinned actionlint supports `.*` filters
but not `[*]`; bracket wildcard parity requires a parser upgrade or a narrow
parser extension before implementation.

Add a manually dispatched GitHub-hosted differential oracle following the
repository's existing oracle workflow pattern. Local fixtures are the required
gate. Run the hosted oracle when semantics change; hosted proofs remain
optional for local development and the default check gate.

Include the exact `case()` contract: 3–255 odd-numbered arguments, Boolean
predicates, and lazy evaluation through the first match and selected value.

### 2. Consolidate evaluation without changing behavior

Add one private recursive evaluator, shared value helpers, a non-evaluating
policy validator, fixed internal surface identifiers, and phase resolvers.

Route existing evaluators through the shared semantic core, but invoke
validation only at existing validation call sites. Preserve current strict
condition equality and Boolean logical results with temporary compatibility
operations. Preserve direct-reference-only runtime interpolation and restricted
action-default policies.

Keep exported functions, diagnostics, validation timing, implicit condition
guards, and authority analyzers unchanged. Remove duplicated recursive
implementations only after existing behavior and security tests pass unchanged.

### 3. Enforce phase and authority boundaries

Add compile-time partial reduction for event-backed conditions. Reduce every
resolvable `github.event` subtree, validate the residual expression for its
runtime surface, and reject any plan that still contains `github.event`.

Scan every expression-bearing plan field for static secrets and
`github.token`. Extend scanners to reject filters, dynamic indexes, and whole
objects unless they can prove the exact required authority. Add regressions for
`ArrayDerefNode` and every new AST shape before later slices enable it.

### 4. Match core values and operators

Implement GitHub's language semantics once:

- falsy `null`, `false`, numeric zero, `NaN`, and empty strings
- loose equality and numeric coercion
- `<`, `<=`, `>`, and `>=`
- case-insensitive string comparison
- operand-returning `&&` and `||`
- legal missing members evaluating to null

Enable semantics by surface, not globally. Remove strict mixed-type condition
rejection after differential fixtures prove the GitHub results. Keep prohibited
contexts and values unavailable across the immutable phase boundary as errors.

GitHub evaluates job `if` before matrix expansion. Do not expose `matrix` or
`strategy` there. Step `if` may use both contexts.

### 5. Add pure built-in functions

Implement and test:

- primitive conversion for `startsWith()`, `contains()`, and `endsWith()`
- array membership for `contains()`
- `format()`
- `join()`
- `toJSON()`
- `fromJSON()`
- lazy, ordered `case()` evaluation

Enable each function only where the surface policy and available values permit
correct evaluation. Keep status functions and `hashFiles()` phase-specific.

### 6. Add indexing, filters, and context shapes

Support:

- computed object indexes
- numeric array indexes
- missing and out-of-range indexes
- `.*` filters
- chained filter projection

Projection omits missing children and preserves nested arrays. A later wildcard
may traverse and flatten one collection level. Resolve the parser prerequisite
before adding `[*]` as equivalent syntax.

Add whole `matrix`, `steps`, and `needs` objects, strategy values, typed inputs,
and lifecycle-specific GitHub values where their phases support them. Arrays
and objects compare by instance, matching GitHub.

### 7. Expand expressions by workflow key

Replace direct-reference-only evaluation incrementally:

1. step `run`, `env`, `with`, `name`, shell, and working directory
2. step timeout and `continue-on-error`
3. job outputs, defaults, and supported runtime job fields
4. reusable-workflow values

Use GitHub's context-availability table for each key. Do not add a permissive
global runtime policy.

Timeout and `continue-on-error` require parser, workflow-model, plan-schema, and
lifecycle evaluation changes. Treat those as typed-key work rather than string
interpolation. Runtime job names are out of scope because Buildkite labels are
currently fixed during compilation; support them only with a separate label
update design.

## Validation

Every delivery slice must run:

- expression conformance and surface-policy tests
- secret and token authority regression tests
- the starter-workflow corpus
- `mise run check`
- a compatibility documentation audit

Run the GitHub-hosted differential oracle manually when semantics change. An
unavailable hosted proof blocks a parity claim for the affected behavior, but
does not block local development or the default check gate.

The starter-workflow corpus currently exercises mostly direct context
interpolation and `hashFiles()`. Use it as a regression gate, not as the measure
of complete expression parity.

## Completion criteria

Expression parity is complete when:

- complete expressions used by typed keys produce GitHub-equivalent typed
  values
- template-valued keys produce GitHub-equivalent rendered strings
- conditions produce GitHub-equivalent Boolean results
- each supported workflow key exposes the same contexts and functions where
  Buildkite has equivalent data and timing
- legal missing members evaluate as null and render as an empty string
- prohibited contexts, unprovable authority, and unavailable phase data fail
  before plan execution
- the differential oracle has no unexplained differences for claimed behavior
- `docs/compatibility.md` lists only deliberate, verified limitations

## References

- [GitHub Actions expressions](https://docs.github.com/en/actions/reference/workflows-and-actions/expressions)
- [GitHub Actions context availability](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#context-availability)
