# GitHub Actions expression parity

## Status

The shared expression evaluator is delivered. Conditions, runtime
interpolation, compile-time expressions, and action input defaults now use the
same semantic core.

The [compatibility reference](../compatibility.md#expressions-and-contexts)
lists the expressions and contexts supported today. This plan records the
design and the remaining limits; it does not duplicate that list.

## Original problem

`buildkite-gha` once evaluated expressions separately in four places:

- compile-time graph expressions
- job and step conditions
- runtime interpolation
- action input defaults

Those paths accepted different subsets of GitHub's expression language. Adding
an operator or function four times would have increased drift and made results
harder to verify.

Parity means matching GitHub where Buildkite has the same value at the same
stage of execution. It does not mean inventing GitHub-only values or making
every context available in every workflow key.

## Design

Actionlint remains the parser. One internal evaluator owns:

- values, truthiness, and conversion
- property and index access
- operators and short-circuiting
- pure built-in functions

Each call site selects a fixed workflow surface and an evaluation phase. The
surface decides which contexts and functions are legal. The phase resolver
supplies values available during compilation or at runtime.

A separate validator visits every syntax-tree branch before evaluation. This
prevents short-circuiting from hiding unsupported syntax.

Before a plan is serialized, the compiler reduces every resolvable
`github.event` expression from the immutable event snapshot. It then validates
the remaining expression for runtime use. Plans cannot retain `github.event`.

Condition wrappers still own the implicit `success()` guard and status
functions. Template rendering, typed workflow fields, and `hashFiles()` remain
outside the pure evaluator.

## Security boundaries

Expression support must not create authority. These rules still apply:

- Authority analysis is separate from syntax validation and evaluation.
- Every plan expression is scanned for `secrets.<name>` and `github.token`.
- A secret reference must name one static secret before execution.
- Dynamic, filtered, projected, and whole-context secret access is rejected.
- `github.token` uses the same scoped token as `secrets.GITHUB_TOKEN` and is
  available only while a step runs.
- The exact step call `toJSON(github)` counts as a token reference because the
  retained context includes `token`.
- `github.event` stays compile-time only because plans do not retain the event
  payload.
- Missing Buildkite equivalents remain unsupported.

See the [security model](../security.md) for the current credential boundaries.

## Delivered work

The implementation now provides:

- one shared evaluator and policy validator
- GitHub-style truthiness, numeric coercion, comparisons, and
  operand-returning logical operators
- core pure functions, including lazy `case()` evaluation
- computed indexes and `.*` projections
- whole `matrix`, `needs`, and step-scoped `steps` objects where their phase
  permits them
- typed controls for step timeouts and `continue-on-error`
- compile-time event reduction with a no-event-in-plan invariant
- static secret and token analysis for every accepted expression shape
- local conformance fixtures and a manually dispatched GitHub differential
  oracle

## Deliberate limits

- The parser accepts `.*` projections but not the equivalent `[*]` spelling.
- Contexts remain specific to each workflow key and execution phase.
- Runtime job names remain out of scope because Buildkite labels are fixed
  during compilation.
- Reusable-workflow inputs that depend on compound `needs` expressions remain
  unsupported where no runtime call job exists.
- Buildkite does not fabricate GitHub-only values.

The compatibility reference owns the exact, current list.

## Validation

Changes to expression semantics should run:

- expression conformance and surface-policy tests
- secret and token authority tests
- the starter-workflow corpus
- `mise run check`
- a compatibility documentation review

Run the GitHub-hosted differential oracle when semantics change. It is not part
of the network-free local gate.

## References

- [GitHub Actions expressions](https://docs.github.com/en/actions/reference/workflows-and-actions/expressions)
- [GitHub Actions context availability](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#context-availability)
