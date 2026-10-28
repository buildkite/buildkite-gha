# Reusable workflow input evaluation

## Problem

The compiler originally substituted caller input values into callee expression
source when expanding a `workflow_call`. Scheduling and service fields still use
this path; runtime fields, graph fields, call guards, and deferred inputs now use
[scoped input evaluation](../expression-authority.md#normalized-execution-program).
[`SubstituteCompileInputs`](../../internal/expression/compile_template.go) is
called only from [`internal/compiler/reusable.go`](../../internal/compiler/reusable.go).

```yaml
# callee
runs-on: ${{ inputs.runner }}
# caller: with: { runner: private-pool-a }
# compiled expression source
runs-on: ${{ 'private-pool-a' }}
```

Substitution predates plans carrying `inputs`. It has three costs:

- Input values appear in expression source, so any diagnostic that quotes the
  source quotes the value. [#602](https://github.com/buildkite/buildkite-gha/pull/602)
  and [#603](https://github.com/buildkite/buildkite-gha/pull/603) redact these
  diagnostics field by field.
- Substituted text is parsed again, so input values containing `${{` are
  rejected to prevent expression injection.
- Every expression-bearing field needs a substitution wrapper, and each new
  field risks missing one.

## Approach

Keep `inputs.*` references in expression source and evaluate them against an
explicit inputs scope, as GitHub does. Each expression owner gets its inputs
from exactly one place:

| Expression owner | Inputs source |
| --- | --- |
| Flattened job | `Job.Inputs`, plus deferred values hydrated at runtime |
| Call guard | `CallGuard.Inputs` and its deferred bindings |
| Deferred input template | Caller inputs captured with the template |
| Action body and defaults | Action invocation inputs, which shadow workflow inputs |

The runtime supplies `Job.Inputs` through `WorkflowInputs` and condition
contexts. Runtime literal consumers, graph fields, call guards, and authority
analysis use scoped inputs. The remaining gaps are scheduling, service, and
credential fields.

### Invariants

- A callee's expressions never see the root workflow's inputs. Each workflow
  frame builds its compile context from its own inputs.
- Exact forwarding of a deferred input keeps its original caller capture and
  producer bindings.
- Authority analysis evaluates call guards with caller-scoped inputs, so
  known-false branches still receive no token.
- Partial reduction keeps `inputs.*` references, including inside expressions
  that also reference `event` or `matrix`.
- Literal consumers, such as action adapters reading `with`, `uses`
  resolution, shell and platform checks, and runs-on selection, read a resolved
  view. That view is never written back into runtime expression source.
- Input defaults (`ProfileReusableInput`) keep their `github` and `vars`
  restriction.

### Diagnostic guarantee

Diagnostics and expression source must not contain input values unless the
authored expression reads them. Plan data fields such as `inputs`, commands,
labels and outputs may contain them by design.

Removing substitution is not enough on its own. Evaluators that quote evaluated
values, such as `format()` reporting an invalid placeholder, must use the
existing authored-error distinction in
[`compile_errors.go`](../../internal/expression/compile_errors.go). Remove each
redaction flag only after its field meets this guarantee.

## Scope and non-goals

- Event folding stays as is. Folding event values at compile time drives
  pruning and token authority, and `introducesExpressionSyntax` stays for it.
  This work only ensures that folding a mixed event and input expression does
  not bake in input values.
- Compound forwarding of deferred parent inputs stays unsupported.
- No cross-version plan compatibility. `run-job` requires the plan's compiler
  version and runtime digest to match, so compiler, runtime and schema changes
  ship together.

## Delivery slices

Each slice migrates a field vertically: admission, scoped evaluation, authority,
plan validation, runtime and diagnostics. Unmigrated fields stay on
substitution.

1. **Foundations.** Extend the engine reference query to report named and
   dynamic `inputs` access. Add a residual reduction that keeps `inputs.*`
   references. No emitted-plan change.
2. **Runtime fields.** Step and job `env`, `run`, `with` and `if` evaluate
   against `Job.Inputs`, as do job outputs, defaults, step names, shells,
   working directories, and step controls. Compile-time literal consumers move
   in the same change. Input-selected secret names are still substituted,
   because secret inventory needs static names.
3. **Calls and graph fields.** Nested call arguments, call guards, matrix,
   labels, containers, runs-on and concurrency use an explicit caller scope.
   Deferred input templates gain a caller-input capture. The job plan schema,
   decoding, bounds, cloning and digest change together; the additive field
   does not need a new schema version.
4. **Scheduling, services and credentials.** Pass scoped inputs without widening
   these profiles to deferred-input forms they reject today.
5. **Cleanup.** Delete substitution and substitution-only redaction. Accept
   `${{` in input values as data only after no reparse path remains and a
   hosted fixture confirms GitHub's nested `workflow_call` behavior. Move
   lasting decisions into [Expression authority](../expression-authority.md)
   and remove this plan.

## Verification

- Existing input-redaction tests stay green.
- New cases: sibling calls with conflicting inputs, nested forwarding with
  colliding caller and callee names, caller versus callee matrices, deferred
  call guards, known-false token branches, action-input shadowing, typed round
  trips, and invalid evaluated `format()` strings.
- `mise run check` and the
  [expression differential oracle](../development.md#verify-runtime-behavior) pass.
