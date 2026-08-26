# Composite action token authority

## Problem

At commit `8cc2c8598944076af96ea45e3700725beb172ae5`,
`taiki-e/install-action@v2` contains this composite run-step environment value:

```yaml
inputs:
  fallback:
    default: cargo-binstall
runs:
  using: composite
  steps:
    - shell: bash
      env:
        DEFAULT_GITHUB_TOKEN: ${{ inputs.fallback == 'cargo-binstall' && github.token || '' }}
      run: bash --noprofile --norc "${GITHUB_ACTION_PATH:?}/main.sh"
      if: runner.os != 'Windows'
```

The minimal workflow supplies only the required `tool` input:

```yaml
permissions:
  contents: read
jobs:
  repro:
    runs-on: ubuntu-latest
    steps:
      - uses: taiki-e/install-action@v2
        with:
          tool: just
```

Hosted validation resolves and admits this workflow on current `origin/main`,
but the plan has no `github_token` or `provider-token-write` capability.
`inspectInvocation` inspects workflow-authored `with` values, effective action
input defaults, and nested `uses` inputs. It does not inspect a composite run
step's `env`. Runtime later evaluates that field and fails because
`github.token` is unavailable.

The fix must preserve these boundaries:

- The plan requests token authority before runtime; runtime metadata cannot
  mint unplanned credentials.
- Top-level workflow permissions and backend provenance policy remain the
  authority ceiling. The token stays job-bound and non-ambient.
- Action metadata can already request token authority through an effective
  input default. It still cannot request ordinary Buildkite secrets.
- Origin jobs do not request a GitHub token. A GitHub-only provider guard can
  evaluate to empty without one.
- Unsupported expressions, empty effective permissions, unresolved hosted
  actions, and invalid plans fail closed.

PRs #375 and #388 are failed experiments, not implementation sources. Their
main lesson is negative: reproducing expression reachability and action
lifecycle execution in the compiler creates a second runtime.

## Options

| Option | Approach | Tradeoff |
| --- | --- | --- |
| 1. Grant every resolved action | On a GitHub event, any job containing a resolved, non-native action receives token authority. | About 20 production lines and no metadata semantics, but grossly over-authorizes tokenless JavaScript, Docker, and composite actions. A mutable action ref can begin receiving the job token without even declaring a token expression. Reject. |
| 2. Summarize direct metadata references | While building the immutable action graph, mark each action that contains a direct `github.token` reference in a runtime-evaluated metadata field. Propagate the bit from nested actions to each selected root. On GitHub events, a marked root requests normal job token authority. | Deliberately over-authorizes known-false steps, non-selected input branches, and unreachable nested actions, but only when resolved metadata already contains a direct token access. It avoids condition, input, and lifecycle simulation. Recommend. |
| 3. Register compatible action releases | Add a capability to the action integration registry for known `taiki-e/install-action` commits. | Small and precise for one audited commit, but every upstream release can regress until registered. It fixes an action, not the compatibility class. Keep only as an emergency fallback. |
| 4. Derive exact invocation reachability | Evaluate input defaults, caller inputs, step conditions, provider guards, nested preparation, outputs, and lifecycle phases abstractly during compilation. | Least over-authorization in theory, but duplicates runtime behavior and recreates the failure mode this design must avoid. It also exceeds the scope of this compatibility fix. Reject. |

## Recommendation

Implement option 2 as a conservative capability summary owned by resolved
action compilation:

1. Inspect only metadata fields that runtime can evaluate with direct
   `github.token`: composite step `run`, `env`, `with`, and
   `working-directory`; composite outputs; and supported `runs.env` fields.
   Reuse the existing expression parser and direct-reference validation. Do
   not interpret `if`, `&&`, `||`, `case()`, status, or lifecycle ordering.
2. Exclude action input defaults from the new summary. Keep the existing
   effective-default path, including explicit overrides, ordered defaults,
   `job.check_run_id`, and `github.server_url` narrowing.
3. Preserve the composite `toJSON(github)` rule: it can consume an already
   authorized context but cannot add authority. Only direct `github.token`
   sets the summary bit.
4. Propagate the bit through the already resolved, depth-bounded action DAG.
   Attribute authority to the selected root action in plan authorization, and
   update diagnostics to describe resolved action metadata rather than claiming
   every attributed action defaults an input to `github.token`.
5. Apply the summary only when the event provider is GitHub. Origin remains
   tokenless: provider-guarded references evaluate to empty, while an
   unguarded reference fails at runtime instead of receiving the wrong
   provider's credential.

This changes precision, not the authority ceiling. A false positive can mint a
token that runtime does not use, and because authority is job-wide another
action in the same job can consume it. However, the marked resolved action
already contains metadata capable of consuming that same token, and token
issuance still requires effective workflow permissions, default-off Buildkite
configuration, and backend admission. The conservative false positives are:

- `fallback: cargo-install` still grants for `taiki-e/install-action@v2`.
- A known-false composite step still grants.
- A provider guard still grants on GitHub, even when another known operand is
  false.
- A token-bearing nested composite still grants when its invocation is
  runtime-unreachable.

These are explicit compatibility tradeoffs. Do not add exceptions to recover
precision; an exception is the start of another lifecycle evaluator.

## Expected diff ceiling

For the implementation change after this design note:

- At most 120 production lines in action metadata/compiler ownership.
- At most 250 test and documentation lines.
- At most 400 changed lines total, excluding a copied upstream metadata
  fixture if one is necessary.
- No plan schema, runtime, expression evaluator, or backend API changes.

Exceeding this ceiling should stop implementation and trigger design review.

## Acceptance tests

1. A minimal resolved fixture matching `taiki-e/install-action@v2` compiles a
   GitHub job with `github_token`, `provider-token-write`, `contents: read`, and
   root-action authorization attribution. Runtime evaluates
   `DEFAULT_GITHUB_TOKEN` successfully.
2. Explicit `fallback: cargo-install` and `if: false` still grant. Tests name
   these as intentional conservative over-authorization.
3. The same unguarded action fixture on Origin compiles without token authority
   and fails at runtime without calling a token provider. A separate fixture
   guarded by `github.server_url == 'https://github.com'` evaluates to empty.
4. Existing effective input-default cases remain exact: an explicit input
   suppresses its default, ordered defaults work, and the GitHub provider guard
   stays tokenless on Origin.
5. A root composite inherits the marker from a nested token-bearing composite.
   A graph without a direct token reference remains tokenless.
6. Composite `toJSON(github)` alone remains tokenless. Composite ordinary
   secret references remain rejected and never add secret authority.
7. A token-bearing action with empty effective permissions fails compilation.
   Invalid or unsupported metadata expressions fail validation rather than
   silently losing the marker.
8. Hosted validation requires immutable action resolution before claiming this
   compatibility. Existing plan validation, redaction, and backend provenance
   tests remain green. Compiler and admission diagnostics identify token-bearing
   resolved action metadata without misreporting it as an input default.

Run targeted compiler and runtime tests, then `mise run check`.
