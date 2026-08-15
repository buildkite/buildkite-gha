# Migrate GitHub Actions secrets to Buildkite

## Problem

GitHub exposes repository secret names but never their values through its API.
Only a trusted GitHub Actions workflow can read an explicitly referenced
`${{ secrets.NAME }}` value. Migration therefore needs a reviewed workflow on
the repository's default branch; a local CLI cannot copy values by itself.

The migration workflow authenticates to Buildkite with GitHub Actions OIDC, so
it does not need a Buildkite API token stored as a GitHub bootstrap secret.

## User journey

Keep the lifecycle explicit because GitHub only enables `workflow_dispatch`
for workflows present on the default branch:

1. Run `buildkite-gha migrate-secrets prepare` in the GitHub repository.
1. The command uses `gh` to identify the repository and list Actions secret
   names. It uses `bk` to select the Buildkite organization, cluster, pipeline,
   and inspect existing destination keys.
1. Select source names interactively, or provide explicit names and filters in
   non-interactive use. `GITHUB_TOKEN` is always rejected.
1. Review the destination and generated non-empty access policy. The common
   case scopes every secret to one selected pipeline; `--policy-file` remains
   the advanced path.
1. Write the deterministic workflow to an explicitly selected path under
   `.github/workflows/`. The command must not commit or push it.
1. Review and merge the workflow into the default branch.
1. Run `buildkite-gha migrate-secrets run --workflow <path>`. The command
   creates a short-lived migration grant through `bk api`, then asks `gh` to
   dispatch that exact default-branch workflow.
1. Confirm the names created. Remove the workflow. The migration grant expires
   and cannot be reused; no Buildkite credential needs removal from GitHub.

`prepare` should print the workflow to stdout unless the user explicitly gives
an output path. Interactive selection is convenience only: the generated file
and backend grant remain the authority.

## Generated workflow

The generated YAML must be deterministic and small enough to review directly.
It contains:

- `workflow_dispatch` with one required, non-secret migration grant ID input.
- `permissions: { id-token: write }` and no repository write permission.
- One static `${{ secrets.NAME }}` reference for every selected source.
- No dynamic secret-name input, enumeration, `secrets: inherit`, artifact, or
  output containing secret values.
- A request for a GitHub OIDC token with a Buildkite migration-specific
  audience.
- One batch request whose body is constructed in memory and sent over HTTPS.
  Values never appear in files, command arguments, tracing, or errors.
- Success output containing destination names only, followed by instructions
  to remove the workflow.

The workflow validates every selected value before making the request. It
disables shell tracing, rejects redirects, and does not print backend response
bodies. The backend must also exclude values from validation errors, audit
events, and application logs.

## Backend contract

GitHub OIDC authenticates a workflow; it does not authorize a Buildkite
destination. `migrate-secrets run` therefore creates a one-use migration grant
while authenticated as the current Buildkite user. The immutable grant binds:

- Buildkite organization and cluster.
- A non-empty access policy for every destination secret.
- The complete, normalized source and destination key allowlist.
- Immutable GitHub repository and owner IDs.
- Exact workflow path, default-branch ref, and workflow commit SHA.
- `workflow_dispatch` as the only accepted event.
- A short expiry and create-only behavior.

The batch endpoint accepts only the keys in that grant. It validates GitHub's
signature and JWKS key, issuer, exact audience, time claims, token ID,
repository IDs, `workflow_ref`, `workflow_sha`, `ref`, and `event_name`. It must
not authorize from mutable repository names or a customizable `sub` claim
alone.

The endpoint preflights the complete batch, then creates all DB-visible secret
records and consumes the grant in one transaction. A rollback can leave
unreachable encrypted objects for normal storage lifecycle cleanup, but no
usable partial keys.

Existing destination keys fail the migration before writes. There is no
overwrite flag in the first version.

Buildkite already validates GitHub Actions OIDC for Package Registries. Reuse
that verifier and claim-policy machinery where its boundary fits; do not add a
second general OIDC framework. Keep this endpoint migration-specific rather
than granting persistent `write_secrets` authority to repository workflows.

## Security boundaries

- Secret names are public metadata and may appear in prompts, generated YAML,
  plans, logs, and results. Secret values may appear only in the GitHub job's
  environment and in-memory HTTPS request.
- The workflow cannot migrate `GITHUB_TOKEN`. `buildkite-gha` retains its
  separate short-lived, permission-scoped workflow-token path.
- Anyone able to change the default-branch workflow can read the selected
  GitHub secrets. The grant limits the additional Buildkite authority to one
  reviewed workflow SHA, one allowlist, one destination, and one short window.
- The CLI may create a grant and dispatch a workflow only after the user runs
  the corresponding command. It never commits, pushes, merges, or deletes
  repository files.
- No Buildkite API token or other long-lived bootstrap credential is stored in
  GitHub.

## Delivery

### 1. One-use backend grant

Add the migration grant and batch-create operations to Buildkite, reusing its
GitHub OIDC verifier. Enforce Buildkite user authorization when creating a
grant and all immutable claim, allowlist, policy, expiry, replay, and
create-only checks when consuming it. Add audit events containing names and
identifiers only.

Verify claim mismatch, expiry, replay, unexpected and missing keys,
`GITHUB_TOKEN`, empty policy, existing keys, batch failure behavior, and value
redaction from responses and logs.

### 2. Guided preparation

Add `migrate-secrets prepare`. Use `gh` and `bk` subprocesses for
authentication and discovery rather than adding SDK dependencies. Keep an
explicit non-interactive path for automation.

Generate and test the static workflow allowlist, OIDC permissions and audience,
safe in-memory batch request, deterministic ordering, default-branch guard,
and cleanup guidance. Test generated product output rather than this
repository's checked-in CI configuration.

### 3. Explicit run and cleanup

Add `migrate-secrets run` to resolve the committed workflow's default-branch
SHA, create the bound grant with `bk api`, dispatch it with `gh`, and report
names and status. Refuse local-only or non-default-branch workflows and any
generated manifest that differs from the committed file.

## Non-goals

- Exporting GitHub secret values to the local machine.
- Discovering values or choosing names at dispatch time.
- Migrating organization, environment, Dependabot, or Codespaces secrets in
  the first version.
- Renaming keys, transforming values, overwriting existing Buildkite secrets,
  or continuously synchronizing the two stores.
- General GitHub OIDC support for imported Buildkite jobs.
- Automatically changing shared GitHub repository state.
