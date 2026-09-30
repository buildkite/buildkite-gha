# Run upstream `actions/checkout`

## Problem

Every `actions/checkout` commit runs through a native Go adapter. The adapter
rejects inputs it does not implement, so common workflows fail to compile:

| Workflow need | Adapter result |
| --- | --- |
| `git push` after checkout (`persist-credentials` default `true`) | Silently not persisted; push fails at runtime |
| `persist-credentials: true` | Compile error |
| `token: ${{ secrets.PAT }}` | Compile error |
| `repository: other/repo` | Compile error |
| `ref: v1.2.3` or a PR head SHA | Compile error |
| `ssh-key`, existing-directory reuse | Compile error |

Each fix to the adapter re-implements more of upstream. Running upstream
removes the whole gap class.

## Goal

Run the upstream checkout JavaScript for GitHub repositories and remove the
native adapter.

The adapter existed to keep checkout tokenless. That constraint no longer
holds:

- The `GITHUB_TOKEN` organization feature is on for everyone in production.
- The GitHub Actions setup flow enables the pipeline token setting for every
  new pipeline, and that flow is on for everyone in production.
- Job-level permission narrowing is in progress separately. Until it lands,
  a persisted token has the top-level workflow permissions.

## Approach

Add an opt-in switch first, prove it, make it the default, then delete the
adapter.

### Switch

| Surface | Setting |
| --- | --- |
| Plugin | `upstream-checkout: true` (YAML boolean) |
| Custom importer | `buildkite-gha upload --upstream-checkout[=<boolean>]` |
| Compiler | `compiler.Options.UpstreamCheckout` |

The default is `false`. The setting follows the existing
`experimental-runner-user` and `private-reusable-workflows` plumbing, so a
later default flip keeps `upstream-checkout: false` as the opt-out. The
deferred-matrix stage record carries it, so later stages compile the same
locks.

The plugin schema sets `additionalProperties: true`, and the plugin hook
passes `BUILDKITE_PLUGIN_CONFIGURATION` through unchanged, so the plugin field
works without a plugin change. It needs a `buildkite-gha` release that reads
it.

### Plan contract

The compiler decides once and records the decision in the digest-bound plan.
The runtime, plan validation, and telemetry read the plan, not the importer
setting.

- `plan.ActionLock` gains `upstream: true` for a lock whose catalog entry
  names a native adapter but whose upstream lifecycle runs instead.
- The field is part of lock identity, so it changes the lock ID.
- Plan validation accepts `upstream` only on an `actions/checkout` lock in a
  job with a GitHub event, and then requires a normalized action program,
  like any JavaScript action.
- Every native-adapter decision goes through `plan.ActionLock.NativeAdapter`,
  which honors the field. Current call sites: `internal/compiler/actions.go` (graph builder),
  `internal/compiler/plan_builder.go` (`validateActionAdapter`),
  `internal/compiler/bundle.go` (checkout warnings), `internal/plan/plan.go`,
  `internal/runtime/actions.go`, `internal/runtime/action_execution.go`, and
  `internal/cli/hosted.go`.

An upstream lock is then an ordinary JavaScript action. No checkout-specific
runtime code runs. The compiler's source fetch still applies the checkout
commit policy (a valid immutable SHA); that check makes no adapter decision.

### Scope of the switch

The compiler selects upstream only when all of these hold:

- the switch is on;
- the event provider is GitHub (`https://github.com`);
- the resolved commit declares a runtime the runtime supports (node16 or
  later).

Otherwise the lock keeps the native adapter. Origin repositories keep it
because upstream authenticates with a GitHub `x-access-token` header and falls
back to the GitHub REST API; neither is proven on Origin. v1 and v2 declare
node12 and keep the adapter with the existing `W_CHECKOUT_LEGACY_RELEASE`
warning.

### Authority

No checkout-specific authority is added.

- Upstream declares `token: ${{ github.token }}` as an input default. The
  existing [expression authority](../expression-authority.md) analysis already
  requests `GITHUB_TOKEN` for effective input defaults, so each job with an
  upstream checkout requests a token with the top-level workflow permissions.
- An explicit `token: ${{ secrets.NAME }}` uses the existing secret path.
- Upstream locks do not receive `provider-token-read`; that capability stays
  restricted to the native adapter. Upstream checkout therefore gets no
  Buildkite repository-provider credential. Private submodules in other
  repositories need an explicit `token`, as on GitHub.
- `persist-credentials` behaves as upstream: the token is written to a Git
  config include under `RUNNER_TEMP` and removed by the post step. Later steps
  in the job can use it, as on GitHub.

## Delivery slices

1. **Opt-in spike.** Switch, plan field, central helper, compiler and runtime
   tests, and documentation for the opt-in. Draft PR.
2. **Hosted proof.** Run representative workflows with the switch on: public
   and private repositories, `persist-credentials` push, submodules, LFS,
   sparse checkout, job containers, the non-root runner user, and macOS.
   Record the evidence in [compatibility](../compatibility.md).
3. **Default on.** Flip the default. Keep `upstream-checkout: false` as the
   opt-out. Document the field in the plugin schema.
4. **Remove the adapter** for GitHub. Keep it only for Origin until Origin has
   its own proof, then decide separately.

## Open questions

- **Token failures.** A job with an upstream checkout fails when token
  issuance is unavailable, for example in pipelines created before the setup
  flow. Slice 3 needs a clear compile or runtime error that names the pipeline
  setting. Hosted admission already rejects two cases that work on GitHub:
  `permissions: {}` (no effective permissions) and workflow files outside the
  `.github/workflows/<name>.yml` token-policy rule. Decide whether those
  workflows keep the native adapter before the default flips.
- **Unused tokens.** A job whose checkout is skipped at runtime still mints a
  token. [Expression authority](../expression-authority.md#further-design-work)
  already tracks on-demand retrieval.
