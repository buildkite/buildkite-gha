# GitHub Actions compatibility

<!-- (internal) If this file is ever moved, please update the GitHub Actions template -->

This page is the production contract for the `hosted` profile used by `upload`
and the Buildkite plugin. If a feature is not listed, treat it as unsupported.

The released plugin supports Linux x86-64 and native macOS arm64 importers and
jobs. It sets the matching `runner.os` and `runner.arch` values. Runner labels
select a platform; they do not promise GitHub image, toolchain, or Xcode parity.

Generated Linux jobs use a dedicated `runner` user and need `buildkite-gha`
v0.13.7 or newer. Use `experimental-runner-user: false` temporarily if an image
cannot support the root bootstrap.

GitHub's [workflow syntax reference](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax)
describes the original syntax. This page describes the subset that runs on
Buildkite.

## Support matrix

| Status | Meaning |
| --- | --- |
| ✅ **Supported** | Available through the production plugin path. |
| 🟡 **Supported subset** | Available within the limits shown here. |
| ➖ **Accepted, no effect** | Validation accepts the syntax, but it does not change the build. |
| 🚧 **Not available in production** | The compiler or runtime supports it, but production upload blocks it. |
| ❌ **Unsupported** | Rejected or outside the compatibility contract. |

Looking for something else? [Browse open compatibility issues](https://github.com/buildkite/buildkite-gha/issues?q=is%3Aissue%20state%3Aopen%20label%3Acompatibility).

| Area | Status | Initial release boundary |
| --- | --- | --- |
| [Workflow and job names](#workflow-syntax) | 🟡 Supported subset | `name`, explicit `run-name`, and job names are retained. `run-name` supports expressions over `github` and `inputs`. |
| [Triggers and filters under `on`](#names-and-triggers) | 🟡 Supported subset | Buildkite creates builds; upload selects aggregate workflow groups for one effective event. `workflow_call` is supported for composition. |
| [Platforms](#job-configuration) | 🟡 Supported subset | The hosted importer provides Linux x86-64. The Agent API can map compatible selectors to hosted Linux or native macOS arm64 targets. Labels do not provide GitHub image, toolchain, or Xcode parity. |
| [Jobs and dependencies](#job-configuration) | ✅ Supported | Static dependencies, matrix fan-out and fan-in, results, and bounded outputs. |
| [Matrix strategies](#matrix-strategies) | 🟡 Supported subset | Static matrices, `include`, `exclude`, and literal `max-parallel`. Maximum 256 instances per job. `fail-fast` has no effect. |
| [Shell steps](#commands-and-actions) | 🟡 Supported subset | Linux and macOS `bash`, `sh`, `python`, and custom shell templates. |
| [Conditions and expressions](#expressions-and-contexts) | 🟡 Supported subset | GitHub-compatible core operators and direct references to selected contexts. |
| [Reusable workflows](#reusable-workflows) | 🟡 Supported subset | Local and literal public GitHub workflows with static inputs, deferred string inputs from direct needs outputs, and direct job-output mappings. Local calls can inherit or explicitly map Buildkite secret authority. |
| [Actions](#actions) | 🟡 Supported subset | Local and public JavaScript and composite actions on Linux and macOS; verified Dockerfile actions on Linux only. |
| [Checkout, artifacts, and cache](#actions) | 🟡 Supported subset | Only the audited versions and modes listed below. |
| [`GITHUB_TOKEN`](#github-token) | 🟡 Supported subset | One job-bound token for the event repository. Reusable-workflow jobs use the top-level workflow permissions. |
| [Other workflow secrets](#other-secrets-and-oidc) | 🟡 Supported subset | Static names in direct jobs and locally inherited or explicitly mapped reusable jobs resolve through the destination job's Buildkite secret authority. |
| [Job and service containers](#containers-and-services) | 🟡 Supported subset | Linux job containers and broadly compatible service definitions, including explicit registry credentials. |
| [Environments and snapshots](#job-configuration) | 🟡 Supported subset | Environments are rejected. Snapshots are accepted with no effect. |
| [OIDC](#other-secrets-and-oidc) | 🟡 Supported subset | Host JavaScript and composite actions can request Buildkite OIDC tokens in jobs with `id-token: write`. |
| [Other platforms](#job-configuration) and [providers](#repositories) | ❌ Unsupported | Windows, Linux arm64, macOS x86-64, GitHub Enterprise Server, and unlisted providers are outside the initial release. |
| [Other GitHub services](#github-services) | ❌ Unsupported | No general emulation for Releases, Packages, Checks, deployments, or GitHub artifact APIs. |

## Outside the initial scope

`buildkite-gha` targets Linux x86-64 and native macOS arm64. Windows execution
is outside the initial product scope, so Windows runner labels are rejected
instead of mapped to another platform.

Compatibility analysis should distinguish this scope boundary from features
that could be added to the supported platforms. This distinction does not
change validation: Windows workflows remain unsupported and appear in raw
corpus results.

## How workflows run on Buildkite

GitHub Actions combines run creation and workload definition in one file. Buildkite keeps them separate.

| GitHub Actions concept | Buildkite behavior |
| --- | --- |
| Workflow trigger | Buildkite integration, schedule, manual build, or API request |
| Workflow run | Existing Buildkite build |
| Job | Buildkite command job |
| Matrix entry | Buildkite command job |
| `needs` | `depends_on` with verified result transport |
| Step | Runs inside the job compatibility runtime |

No shadow GitHub Actions run is created. Buildkite owns scheduling, logs, retries, cancellation, and status.

Steps remain inside one job because they share a workspace, environment files, action state, and post-action cleanup.

### Aggregate workflow upload

The plugin accepts either one `workflow` path or a non-empty `workflows` array.
Each path must be a regular, tracked `.yml` or `.yaml` file inside the
repository. Directories, missing or untracked files, outside paths, symlinks,
and globs fail. A custom importer may upload one explicit regular workflow from
outside the repository.

Before assigning workflow identities and job keys, upload canonicalizes, sorts,
and deduplicates the paths.

All runnable workflows use one artifact and pipeline transaction:

- A compiled workflow becomes a group labeled `:github: workflow ·
  <workflow-name>`. An unnamed workflow uses its canonical path. A resolved,
  non-empty `run-name` appends ` — <run-name>`.
- Each child publishes a provider check named
  `<workflow-name-or-path> / <job-id> (<effective-event>)`. Matrix jobs append
  their sorted values to the job ID.
- GitHub events publish GitHub checks. Origin events publish Origin checks.
- A workflow that does not declare the event becomes one top-level skipped step
  with no plan artifacts.
- An importer annotation lists workflows skipped by event or filters, explains
  each mismatch, and links to the generated step. If publication fails, upload
  warns but still succeeds.

Workflow names, group keys, and provider-check names stay the same across
events; only an appended run title can vary. Groups and replacement steps
depend on the importer; their child jobs do not repeat that dependency.

Reusable-only `workflow_call` files remain available to local callers but do not
create groups. Selecting only reusable workflows is an error.

A safe compilation or trigger-translation error replaces only that workflow
with a failing top-level step. The step:

- is labeled `:github: workflow · <workflow-name-or-path>`, with the resolved
  run title appended when present
- publishes redacted diagnostics as a job annotation
- publishes a `Workflow could not be run` provider check
- limits the check summary to 65,535 bytes
- exits with status 1

Other workflows continue compiling. Parse, event-input, admission, artifact,
and upload failures still abort the complete transaction. Upload never publishes
a partial pipeline.

If a workflow has both a compiler error and a skip reason, the compiler error
takes precedence.

### Compatibility diagnostics

Diagnostics keep guidance separate from implementation detail:

- `message` explains the visible cause and what to do.
- Optional `detail` records lower-level context such as resolved commits,
  adapter boundaries, or supported-version lists.

Text and JSON reports, Buildkite annotations, and generated failure artifacts
preserve both fields. GitHub check summaries show only the concise message.

## Workflow syntax

### Names and triggers

| Key | Status | Behavior |
| --- | --- | --- |
| `name` | ✅ Supported | Available as `github.workflow` and used to name generated work. |
| `run-name` | 🟡 Supported subset | An explicit non-empty value is appended to the workflow group label. Compile-time `github` and `inputs` expressions are supported. The Buildkite build message and provider-check names do not change. |
| `on` | 🟡 Supported subset | Does not create a Buildkite build. Selects and filters aggregate groups for the effective event as described below. |

A workflow name is retained in generated work:

```yaml
name: CI
```

An explicit run name adds event-specific presentation without changing static
workflow or check identity:

```yaml
name: Deploy
run-name: Deploy ${{ inputs.target }} by @${{ github.actor }}
```

This produces `:github: workflow · Deploy — Deploy production by @octocat`.
Omitted, blank, or blank-resolving values retain the static group label. On a
non-dispatch event, declared dispatch inputs use their typed zero values rather
than dispatch-only defaults. A skipped workflow does not synthesize dispatch
inputs.

GitHub also documents `vars` in its context-availability reference. The
production importer has no authenticated GitHub variable snapshot, so `vars`
fails with a source-located diagnostic instead of resolving as empty.

Buildkite controls when a build starts. The trigger declaration controls whether and under which condition the workflow group participates in that existing build:

```yaml
on:
  push:
    branches: [main]
```

Upload selects one effective event, in this order:

1. The event in an explicit `--event-path` snapshot.
1. The GitHub event name accompanying Buildkite's reserved linked-webhook metadata.
1. A Buildkite environment fallback.

The fallback preserves `push`, `pull_request`, `workflow_dispatch`, and
`schedule` from `BUILDKITE_GITHUB_EVENT` across rebuilds. Otherwise:

| Buildkite source | Effective event |
| --- | --- |
| Pull request build | `pull_request` |
| `ui` or `api` | `workflow_dispatch` |
| `schedule` | `schedule` |
| Any other source, including `trigger_job` | `push` |

An explicit snapshot does not consult contradictory live event fields. Linked
merge-group data must match the queue refs and commits. Linked release data must
match the Buildkite event, action, branch, and tag.

With the GitHub Code Access App, Buildkite resolves a release tag to its peeled
commit before creating the build. Without it, the plugin resolves Buildkite's
symbolic `HEAD` from the checkout as a compatibility fallback. The fallback
cannot infer a merge group or release without linked-webhook data.

The selected event then controls applicability, event-dependent compilation,
the group condition, and the provider-check suffix.

| Event | Supported trigger behavior |
| --- | --- |
| `push` | `branches`, `branches-ignore`, `tags`, and `tags-ignore`, including ordered negative patterns in an include list. Branch and tag filters select their corresponding ref kind. Matching `paths` and `paths-ignore` can be admitted for linked GitHub branch pushes when the bounded local-diff requirements below are met. |
| `pull_request` | `branches` and `branches-ignore` match the base branch. Omitted `types` defaults to `opened`, `synchronize`, and `reopened`; explicitly listed activity types must map exactly to a supported Buildkite source action. Matching `paths` and `paths-ignore` can be admitted when the bounded local-diff requirements below are met. |
| `merge_group` | Native Buildkite merge queue builds only. Enable merge queue builds and Merge groups webhook delivery in the pipeline's GitHub settings. `branches` and `branches-ignore` match the target branch. The only supported activity is `checks_requested`; other types and path, tag, and workflow filters are rejected. The merge group ref and SHA identify the speculative queue commit. |
| `release` | Native Buildkite release builds only. In the pipeline's GitHub settings, enable **Additional Webhooks** > **Releases** and use **Code** trigger mode. Connect the GitHub Code Access App for immutable server provenance and hosted release `GITHUB_TOKEN` issuance. `types` is required and may contain only `published`, `created`, and `released`; bare `release`, all other activity types, and branch, tag, path, and workflow filters are rejected. Draft `created` deliveries are rejected. The ref is `refs/tags/<tag_name>`. The SHA is the server-resolved peeled commit, or the checked-out commit for the compatibility fallback. |
| `workflow_dispatch` | Selected for Buildkite UI and API builds. Webhook-style branch, tag, type, and workflow filters are unsupported. |
| `schedule` | Selected for Buildkite scheduled builds. Buildkite owns cron configuration and does not expose which schedule started a build, so every `on.schedule` workflow is eligible for every Buildkite scheduled build. |
| `workflow_call` | Defines a reusable-workflow interface. A reusable-only local file is available to callers but does not become a top-level group. |
| Any other event | No Buildkite build source exists, so the trigger can never start a build. It is ignored with a `W_TRIGGER_EVENT_UNSUPPORTED` warning when the workflow also declares a supported event. A workflow declaring only unsupported events fails event-independent validation and is a skipped step in an uploaded pipeline. |

Supported `pull_request` activity types are `assigned`, `unassigned`, `labeled`, `unlabeled`, `opened`, `edited`, `closed`, `reopened`, `synchronize`, `converted_to_draft`, `locked`, `unlocked`, `enqueued`, `dequeued`, `milestoned`, `demilestoned`, `ready_for_review`, `review_requested`, `review_request_removed`, `auto_merge_enabled`, and `auto_merge_disabled`.

GitHub defines seven release activities: `published`, `unpublished`, `created`, `edited`, `deleted`, `prereleased`, and `released`. A bare `on: release` selects all seven, so it cannot map exactly to Buildkite's three delivered activities and is unsupported.

#### Push path filters

For a linked GitHub branch push, the importer binds the webhook repository,
ref, commit range, force state, and complete pushed-commit list to the Buildkite
build and local checkout. `HEAD`, the `origin` repository and branch, and the
workflow file must match the pushed commit.

Normal and force pushes use GitHub's two-dot `before..after` comparison. For a
new branch, the importer uses the parent of the oldest pushed commit only when
the complete commit set has one clear, single-parent boundary.

Added, modified, deleted, and type-changed paths can match. Patterns use the
same ordered matching as pull requests.

Admission fails when the evidence is unsafe or incomplete, including:

- a deleted ref or non-GitHub repository
- missing or shallow history
- stale or mismatched repository, ref, checkout, branch, workflow, commit set,
  or force state
- ambiguous new-branch history
- more than 1,000 pushed commits or 300 changed files
- renames, combined additions and deletions, malformed Git output, or invalid
  patterns
- no local match

GitHub may run after a 1,000-commit or diff-timeout fallback. The importer does
not grant that admission without matching changed-path evidence.

Tag pushes do not evaluate path filters, matching GitHub. Explicit and generated event snapshots, and Buildkite environment fallbacks, cannot admit push path filters because they are not linked webhook evidence.

#### Pull request path filters

`paths` and `paths-ignore` support ordered GitHub patterns. For example, this runs for changes under `src`, except generated files:

```yaml
on:
  pull_request:
    paths:
      - "src/**"
      - "!src/generated/**"
```

Before upload, the importer compares the pull request merge base with its head
in the local checkout. A changed path must match, and the linked webhook,
commits, synthetic merge, base branch, and workflow file must agree.

The check uses the checkout's existing Git access for public, private, and fork
pull requests. It does not call GitHub or use Buildkite `if_changed`.

| Admitted | Rejected |
| --- | --- |
| A matching added, modified, deleted, or type-changed path | No local match |
| A copied destination that matches | A rename, or a diff containing both additions and deletions |
| At most 300 changed files from complete local history | Missing or shallow history, multiple merge bases, or more than 300 files |
| A mergeable pull request with matching webhook and workflow data | A conflict, stale data, changed merge workflow, path or pattern containing a backslash, invalid pattern, or malformed Git output |

A local non-match is rejected because GitHub does not report whether its diff
timed out and ran the workflow anyway. Unfiltered `closed` workflows remain
supported. Filtered `closed` workflows are rejected when GitHub provides an
actual merge, squash, or rebase commit instead of a synthetic merge.

An unsupported or inexact filter replaces only the affected workflow with a
failing step. It never broadens when the workflow runs.

A top-level workflow that does not declare the effective event is excluded before event-dependent validation or compilation and represented by one top-level skipped command step. A workflow that declares that event remains represented by a group even when a same-event branch, tag, base-branch, or action condition evaluates false in Buildkite. If no directly runnable workflow declares the event, upload succeeds with a skipped-only pipeline.

### Reusable workflows

**🟡 Supported subset.** Calls may use a local path or a literal public GitHub reference such as `owner/repository/.github/workflows/ci.yml@v1`. A public reference resolves once per operation to an immutable commit. Nested `./.github/workflows/...` calls resolve in that pinned repository.

**✅ Supported:**

- Local `./.github/workflows/...` paths.
- Literal public references to a `.yml` or `.yaml` file directly under `owner/repository/.github/workflows/`.
- `boolean`, `number`, and `string` inputs.
- Static input values. Caller values may use graph-time `github`, `vars`, matrix, and parent reusable-workflow inputs with the supported operators and pure functions.
- String inputs passed as exactly `${{ needs.<job>.outputs.<name> }}`. The call must list the job in `needs`. Buildkite resolves the verified output before each flattened callee job runs.
- Literal defaults and expression defaults over graph-time `github` and `vars` values.
- Nested calls up to four levels.
- `secrets: inherit` for repository-local calls. Each nested edge must repeat it.
- Explicit repository-local mappings from a declared callee alias to one direct `${{ secrets.NAME }}` or `${{ secrets['NAME'] }}` caller reference.
- Required and optional `on.workflow_call.secrets` declarations.
- Caller-visible aggregate results.
- Outputs mapped directly from `jobs.<job>.outputs.<name>`.
- Call-level `if` over caller `github`, `vars`, `inputs`, direct `needs`, and status functions.

**❌ Unsupported:**

- Dynamic workflow paths and private repositories.
- Secret forwarding for public remote calls.
- Literal, compound, dynamic, or non-secret explicit mapping values.
- Compound `needs`-dependent inputs or dynamic matrices.
- Input defaults that reference `inputs`.
- Literal or compound output expressions.
- Top-level concurrency in the called workflow.

Job-level `uses`, `with`, and `secrets` follow these boundaries.

For a local call with `secrets: inherit`, each flattened callee job requests only the static ordinary secret names referenced by that job or its workflow-authored action inputs. Inheritance is one hop: an omitted nested `secrets: inherit` removes ordinary secret authority from every job below that edge. It does not affect direct caller jobs or `GITHUB_TOKEN`.

Explicit mappings must target aliases declared by the called workflow. Every required alias must receive authority; an unmapped optional alias is empty. Nested mappings compose to the original Buildkite secret name and cannot recover an omitted same-named secret. Plans contain aliases and original names, never values. The runtime retrieves each original once, registers its value with both redactors, then projects it to the callee aliases.

`${{ secrets.GITHUB_TOKEN }}` may be forwarded to a declared alias. The alias remains part of the scoped workflow-token contract and never becomes an ordinary Buildkite secret.

A call condition runs in caller scope before static call-matrix expansion. It keeps the implicit `success()` guard. A false condition skips every flattened descendant, including jobs with `if: always()`, and exposes `skipped` with empty outputs to downstream `needs`. Nested calls evaluate ordered outer-to-inner guards. Callee job results do not change an outer guard. Call conditions cannot use `matrix`, `strategy`, callee inputs or needs, `steps`, `env`, `runner`, or `secrets`.

The called workflow declares its inputs and outputs:

```yaml
on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
    outputs:
      image:
        value: ${{ jobs.build.outputs.image }}

jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      image: ${{ steps.image.outputs.value }}
    steps:
      - id: image
        run: echo "value=app-${{ inputs.target }}" >> "$GITHUB_OUTPUT"
```

The caller can pass a static input:

```yaml
jobs:
  build:
    uses: ./.github/workflows/build.yml
    with:
      target: production
```

Or defer a string input until a prerequisite publishes its output:

```yaml
jobs:
  call:
    needs: prepare
    uses: ./.github/workflows/build.yml
    with:
      target: ${{ needs.prepare.outputs.target }}
```

### Permissions

**🟡 Supported subset.** Permissions matter only when a job statically references `secrets.GITHUB_TOKEN` or `github.token`, or an effective action input default can reach `github.token` for the event provider.

A workflow-level permissions map can request repository access:

```yaml
permissions:
  contents: read
  pull-requests: write
```

Supported values are `read`, `write`, and `none`. Supported repository permission names are `actions`, `artifact-metadata`, `attestations`, `checks`, `contents`, `deployments`, `discussions`, `issues`, `packages`, `pages`, `pull-requests`, `security-events`, and `statuses`. The separate `id-token` permission is also supported.

An omitted map defaults to exactly `contents: read` when a token is needed. This deterministic default does not inherit GitHub repository or organization settings. Hosted token issuance uses only the top-level map. Job-level repository permission maps do not narrow or expand `GITHUB_TOKEN`; the separate `id-token` permission retains job-level behavior. Write access therefore requires an explicit top-level map.

The top-level scalars `permissions: read-all` and `permissions: write-all` expand during compilation to explicit maps containing the 13 supported repository permissions listed above. Plans and workflow-token requests contain those maps, not the aliases. Both expansions exclude `id-token`, `models`, `repository-projects`, `code-quality`, `metadata`, and `vulnerability-alerts`.

Jobs expanded from reusable workflows use the top-level requesting workflow's repository permissions for `GITHUB_TOKEN`. Only this immutable top-level map is enforced server-side; permission maps in called workflows do not narrow `GITHUB_TOKEN`. The separate `id-token` permission retains called-workflow narrowing. Warnings identify job-level repository maps that differ from the applied top-level permissions and called-workflow maps that would have narrowed the token scope.

Job-level aliases and noncanonical permission names are unsupported. An empty top-level map, or a top-level map containing only `none`, creates no token.

### Environment and defaults

| Key | Status | Behavior |
| --- | --- | --- |
| `env` | 🟡 Supported subset | Workflow, job, and step maps use normal precedence; the most specific value wins. Individual values may use supported interpolation. An entire map cannot be expression-valued. |
| `defaults.run.shell` | 🟡 Supported subset | Supported at workflow and job level for `bash`, `sh`, `python`, and custom shell templates. Host jobs default to `bash`; job containers default to `sh`. |
| `defaults.run.working-directory` | 🟡 Supported subset | Supported at workflow and job level for workspace-relative paths. |

A job-level value overrides the same workflow-level environment variable:

```yaml
env:
  GOFLAGS: -mod=readonly

jobs:
  test:
    env:
      GOFLAGS: -race
```

Workflow-level run defaults set the shell and working directory:

```yaml
defaults:
  run:
    shell: bash
    working-directory: ./src
```

### Concurrency

**🟡 Supported subset with different queue behavior.** A static group becomes a repository-scoped, case-insensitive Buildkite concurrency group. Groups may use `vars`, supported `github` fields, static reusable-workflow inputs, and concrete matrix values at job level. Core operators, `fromJSON`, and the case-insensitive string functions `startsWith`, `contains`, and `endsWith` are supported when the whole expression resolves during compilation. Runtime `needs` and `strategy` values remain unsupported.

A workflow can set a group and cancellation expression while a job uses a matrix-derived group:

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ startsWith(github.ref, 'refs/pull/') }}

jobs:
  deploy:
    concurrency: deploy-${{ matrix.target }}
```

Buildkite queues every waiting entry. It does not replace GitHub's existing pending entry. The `queue` key is unsupported.

Workflow-level literal or expression-resolved `cancel-in-progress: true` emits a warning and does not cancel. Job-level cancellation remains unsupported. Buildkite **Cancel Intermediate Builds** and **Skip Intermediate Builds** settings can approximate same-branch cancellation.

Cancel the whole Buildkite build rather than one job when a workflow-level concurrency gate is active.

## Job syntax

### Job configuration

| Key | Status | Behavior |
| --- | --- | --- |
| `name` | ✅ Supported | Labels may use static `github`, `vars`, reusable-workflow `inputs`, and matrix values. |
| `needs` | ✅ Supported | Accepts a string or list of static job IDs. Matrix fan-out and fan-in are automatic. |
| `runs-on` | 🟡 Supported subset | Explicit mappings are authoritative. The Agent API returns a complete target for every other selector and can return a fallback warning annotation. The local preset accepts `ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, and `macos-latest`. Labels are case-insensitive. Static expressions may resolve to an accepted label or label list. |
| `if` | 🟡 Supported subset | Runs before the job starts. See [Conditions](#conditions). |
| `outputs` | 🟡 Supported subset | Maps step outputs for consumption through `needs`. A job may publish 64 outputs of up to 1 KiB each. Ambiguous matrix output values stop the job with an error. |
| `env`, `defaults.run` | 🟡 Supported subset | Uses the [workflow-level behavior](#environment-and-defaults). |
| `timeout-minutes` | 🟡 Supported subset | Accepts literal timeouts up to 360 minutes. Expressions are rejected. |
| `continue-on-error` | 🟡 Supported subset | Accepts literal booleans. Expressions are rejected. A tolerated failure remains visible as a Buildkite soft failure and reports `success` through downstream `needs`. |
| `environment` | ❌ Unsupported | Environment approvals, secrets, deployment records, and protection rules are unavailable. |
| `snapshot` | ➖ Accepted, no effect | Custom image creation is not implemented. |

Job names can interpolate matrix values:

```yaml
jobs:
  test:
    name: Test Go ${{ matrix.go }}
```

A job can depend on another job by ID:

```yaml
jobs:
  build:
    # ...
  test:
    needs: build
```

Runner labels can resolve from a matrix value:

```yaml
runs-on: ${{ matrix.os }}
```

A job-level condition can combine a branch check with status:

```yaml
if: github.ref == 'refs/heads/main' && success()
```

Job outputs can pass a step output to a dependent job:

```yaml
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      image: ${{ steps.image.outputs.value }}
    steps:
      - id: image
        run: echo "value=app:latest" >> "$GITHUB_OUTPUT"

  deploy:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ needs.build.outputs.image }}"
```

Results and outputs come from verified producer manifests. Retrying one producer can make selection ambiguous; retry the whole build.

A job with `continue-on-error: true` stops ordinary steps after a failure, runs eligible failure and always steps plus post-actions, publishes its outputs, and reports `success` through `needs.<job>.result`. The generated Buildkite job returns reserved status `78` for the tolerated workflow failure and soft-fails only that status, so the failure remains visible without blocking dependent jobs. Job timeout expiry remains `cancelled` and is never tolerated.

Runner labels are case-insensitive. Runner aliases such as `macOS-latest` use
the same target as `macos-latest`. Linux labels default to the corresponding
Noble or Jammy hosted-toolchains image; an explicit immutable image overrides
it for a configured profile. Linux labels use default Buildkite agent targeting
with that image when unmapped.

An explicit mapping is authoritative and bypasses Agent API resolution. It
declares that the selector runs on Linux x86-64, except for the known macOS
labels, which select Darwin arm64 and reject images. For every other selector,
the job-scoped Agent API owns compatibility and returns the complete queue,
platform, and immutable Linux image. The importer applies that target verbatim
and publishes returned fallback warnings as annotations. The server rejects
selectors that require an incompatible operating system or architecture.

`validate --profile hosted` has no job-scoped API and admits only the local
`macos-latest` preset.

### Matrix strategies

| Key | Status | Behavior |
| --- | --- | --- |
| `matrix` | 🟡 Supported subset | Literal rows or compile-time `github`, `event`, `vars`, and `fromJSON` values. |
| `include`, `exclude` | 🟡 Supported subset | Static combinations. |
| `max-parallel` | 🟡 Supported subset | Literal value. |
| `fail-fast` | ➖ Accepted, no effect | A failed matrix entry does not cancel its siblings. |

A strategy can combine parallelism, static matrix values, and exclusions:

```yaml
strategy:
  max-parallel: 2
  matrix:
    go: ["1.25", "1.26"]
    os: [ubuntu-22.04, ubuntu-24.04]
    exclude:
      - go: "1.25"
        os: ubuntu-24.04
```

A job may expand to at most 256 instances. Matrices derived from `needs` or `steps` are unsupported.

### Containers and services

**🟡 Supported subset.** Linux jobs support job containers and GitHub-compatible services. A typical PostgreSQL service works without Buildkite-specific syntax:

```yaml
services:
  postgres:
    image: postgres:16
    env:
      POSTGRES_PASSWORD: test
    ports:
      - 5432
    options: >-
      --health-cmd pg_isready
      --health-interval 2s
      --health-timeout 5s
      --health-retries 10
```

Services support `image`, `credentials`, `env`, `ports`, `volumes`, `options`, `command`, and `entrypoint`.

- Job container images can use compile-time `github`, `inputs`, `vars`, `strategy`, and `matrix` values. The complete image must resolve to a non-empty string and a valid image reference during compilation. Secrets, `needs`, step outputs, and whole or dynamic contexts are unsupported.
- Service fields can use compile-time `github`, `inputs`, `vars`, `strategy`, and `matrix` values or runtime `needs` outputs. An empty evaluated image skips the service.
- A complete non-credential service map can use `${{ fromJSON(needs.<job>.outputs.<name>) }}`. Declare credentials statically so the compiler can prove their secret authority.
- Credentials accept direct values and `github`, `vars`, `secrets`, or `env` expressions. Passwords pass to `docker login` through standard input. Authentication uses a private per-job Docker configuration and never reads ambient Docker credentials.
- Docker options pass through except `--network` and its `--net` aliases, which GitHub Actions does not support. Options can grant privileges, mount host paths, publish ports, and change resource settings.
- Named, anonymous, and absolute bind volumes are supported.
- A job can define 32 services. Each service can define 256 environment entries and 128 ports or volumes.

Implicit GHCR authentication is unsupported; provide explicit credentials. Mutable tags resolve at job start. Use a digest when image immutability matters. Job container images must provide `sh` and run the mounted self-contained Linux runtime executable.

Each job uses a private Docker bridge network. Container jobs reach services by service name. Host jobs use declared published ports; omitted host ports are assigned dynamically. The `job.services.<service>` context exposes `id`, `network`, and `ports`.

A service with a Docker health check must become healthy before steps run. A service without one is ready after it starts. Failures include bounded status, health, port, and log diagnostics.

Cleanup removes the job container, emits masked and bounded service logs, then removes services in declaration order, the network, newly created volumes, and private Docker configuration. Remaining owned resources fail the job. Docker resources are not a security or resource-isolation boundary: the hosted queue must isolate the whole job and enforce host CPU, memory, disk, and network limits. See the [security model](security.md#isolate-the-whole-job).

macOS jobs reject containers, services, Dockerfile actions, and Docker capability.

## Step syntax

### Step configuration

| Key | Status | Behavior |
| --- | --- | --- |
| `name`, `id` | ✅ Supported | Use `id` to read outputs or target background work. IDs must be unique within a job. |
| `if` | 🟡 Supported subset | May use step status, step outputs, `env`, and service ports in addition to job-condition contexts. |
| `env` | 🟡 Supported subset | Values override job and workflow values and may use supported direct interpolation. |
| `continue-on-error` | ✅ Supported | Accepts literal booleans or expressions that produce a Boolean. A failure records `outcome: failure` and `conclusion: success`, then the job continues. |
| `timeout-minutes` | 🟡 Supported subset | Accepts literal numbers or expressions that produce a number greater than 0 and at most 360. |

A step can continue after failure and expose its outcome to a later condition:

```yaml
- id: test
  run: go test ./...
  continue-on-error: true

- if: steps.test.outcome == 'failure'
  run: ./scripts/report-failure
```

### Commands and actions

**🟡 Supported subset.** Commands run in `bash`, `sh`, `python`, or a custom shell template within the Linux or macOS workspace. Custom templates use `command [options] {0} [more-options]`; `{0}` receives a temporary script path. Arguments support shell-style single quotes, double quotes, and backslash escaping without shell expansion. The runner or job container must provide the selected command on `PATH`; R and Julia are not preinstalled by this support. PowerShell and Windows shells remain unsupported. Working directories cannot escape the workspace.

A shell step can specify its shell and workspace-relative working directory:

```yaml
- name: Test
  shell: bash
  working-directory: ./src
  run: go test ./...
```

Use an interpreter installed by an earlier step or included in the job image:

```yaml
- shell: Rscript {0}
  run: print("R script")

- shell: julia --color=yes {0}
  run: println("Julia script")

- shell: bash -l {0}
  run: conda info
```

A `uses` step may call a supported local or public action. Action inputs under `with` may use supported direct interpolation. `docker://` actions and explicit Docker action entrypoints are rejected.

Action steps can call public and local actions:

```yaml
- uses: actions/checkout@v7

- uses: ./.github/actions/build
  with:
    target: production
```

### Background and parallel steps

**✅ Supported.** The `background`, `wait`, `wait-all`, `cancel`, and `parallel` controls are supported. At most ten background steps run at once inside a job. Use `wait: <id>` for selected steps, `wait-all:` for all active work, or `parallel:` for a fixed group.

Background work can be canceled by step ID:

```yaml
steps:
  - id: server
    run: ./scripts/start-server
    background: true

  - run: ./scripts/test

  - cancel: server
```

A parallel group runs a fixed set of child steps together:

```yaml
steps:
  - parallel:
      - run: ./scripts/lint
      - run: ./scripts/test
```

Outputs, environment changes, and failures become visible at the covering wait. Remaining work is joined before post-action cleanup. These controls are not supported inside a composite action.

### Environment files

**✅ Supported.** The runtime supports `GITHUB_OUTPUT`, `GITHUB_ENV`, `GITHUB_PATH`, `GITHUB_STATE`, and `GITHUB_STEP_SUMMARY`. Multiline values are supported. `NODE_OPTIONS` cannot be set through `GITHUB_ENV`.

### Workflow commands

| Command | Status | Behavior |
| --- | --- | --- |
| `add-mask`, `stop-commands` | ✅ Supported | Standard command behavior. |
| `warning`, `error` | ✅ Supported | Creates Buildkite annotations. |
| `group`, `endgroup` | ✅ Supported | Creates linear log sections. |
| Debug and matcher commands | ➖ Accepted, no effect | Consumed without presentation behavior. |
| `notice`, command echo control, other legacy commands | ❌ Unsupported | Not implemented. |

Job-scoped annotations, including step summaries and generated workflow failure diagnostics, require Buildkite agent v3.112 or newer. The total job summary is limited to 1 MiB.

## Expressions and contexts

Three expression modes intentionally support different syntax.

| Syntax | Conditions | Runtime interpolation | Other compile-time expressions |
| --- | --- | --- | --- |
| `!`, `&&`, `\|\|`, `==`, `!=`, `<`, `<=`, `>`, `>=` | ✅ Supported | 🟡 Listed workflow fields | 🟡 When the result resolves fully |
| `always()`, `success()`, `failure()`, `cancelled()` | ✅ Without arguments | ❌ Unsupported | ❌ Unsupported |
| `startsWith()`, `contains()`, `endsWith()`, `format()`, `join()`, `toJSON()`, `fromJSON()`, `case()` | ✅ Supported | 🟡 Listed workflow fields | 🟡 When the result resolves fully |
| `hashFiles()` | 🟡 Step `if` and action lifecycle conditions | 🟡 Workflow steps only | ❌ Unsupported |

### Conditions

Job and step `if` conditions use GitHub-style:

- truthiness and loose numeric coercion
- case-insensitive string comparison
- operand-returning `&&` and `||`
- primitive conversion for string functions
- array search with `contains()`
- lazy `case()` evaluation with 3–255 odd-numbered arguments and Boolean
  predicates

A missing property in an available `github` or matrix context evaluates to
null. An unavailable context is an error. Unlisted functions are unsupported.
`hashFiles()` accepts 1–255 literal or direct-reference arguments in step and
JavaScript action lifecycle conditions.

Conditions support computed object indexes, numeric array indexes, whole
`matrix` and `needs` objects, step-scoped `steps`, and `.*` projections.

- Missing and out-of-range indexes evaluate to null.
- Projections omit missing children.
- A later wildcard flattens one collection level.
- The equivalent `[*]` spelling is unsupported by the parser.
- Whole or dynamic `github`, whole `inputs`, and `strategy` remain unsupported.

| Context | Job `if` | Step `if` |
| --- | --- | --- |
| `github.actor`, `github.base_ref`, `github.event_name`, `github.head_ref`, `github.ref`, `github.ref_name`, `github.ref_type`, `github.repository`, `github.repository_owner`, `github.sha` | ✅ Yes | ✅ Yes |
| `runner.os`, `runner.arch` | ✅ Yes | ✅ Yes |
| `runner.temp` | ❌ No | ✅ Yes |
| `needs.<job>.result`, `needs.<job>.outputs.<name>` | ✅ Yes | ✅ Yes |
| `vars.<name>`, `matrix.<name>` | ✅ Yes | ✅ Yes |
| `inputs.<name>` and computed input indexes | ✅ Yes | ✅ Yes |
| `steps.<id>.outcome`, `steps.<id>.conclusion`, `steps.<id>.outputs.<name>` | ❌ No | ✅ Yes |
| `env.<name>` | ❌ No | ✅ Yes |
| `job.services.<service>.ports[<port>]` | ❌ No | ✅ Yes |
| `github.event.*`, including `github.event.pull_request.*` | 🟡 Compile time only | 🟡 Compile time only |
| `secrets` and other contexts | ❌ No | ❌ No |

Before runtime validation, the compiler reduces event-backed conditions from the
immutable snapshot. Resolvable `github.event` expressions become literals;
supported runtime expressions remain for the job or step.

Every branch is validated first, so short-circuiting cannot hide an unsupported
function, context, or matrix type. No remaining condition can carry
`github.event` into runtime.

Reusable-workflow call conditions use the same operators and status functions but only the caller contexts listed in [Reusable workflows](#reusable-workflows). The runtime evaluates their ordered guards before the called job's own condition.

### Runtime interpolation

These step fields support the operators and pure functions listed above:

- `run`, `env`, `with`, and `name`
- explicit `shell` and `working-directory`
- `continue-on-error` and `timeout-minutes`

They support computed indexes and projections over available `matrix`, `vars`,
`inputs`, `env`, and `runner` values. Computed, whole, and projected `steps` or
`needs` access is unsupported. Reading an unavailable background output is an
error.

Before creating a job plan, the compiler resolves scalar `github.event.*`
values and event-dependent parts of otherwise runtime expressions.

Missing event members become null; template interpolation renders null as an
empty string. Event values cannot introduce new `${{ ... }}` regions. Whole
events and unresolved event references are unsupported because plans keep only
event identity and a payload digest.

Job-level expressions support the same operators and pure functions with these field-specific contexts:

| Field | Contexts |
| --- | --- |
| `env` | `github`, `needs`, `matrix`, `vars`, `secrets`, `inputs` |
| `defaults.run` | `github`, `needs`, `matrix`, `env`, `vars`, `inputs` |
| `outputs` | `github`, `needs`, `matrix`, `runner`, `env`, `vars`, `secrets`, `steps`, `inputs` |

Workflow step fields support `hashFiles()`; composite step and job-level fields
do not. Composite action `run`, `env`, `with`, and `working-directory` fields do
support the listed operators and pure functions.

The runtime has no equivalent values for `strategy`, or `job` in job outputs,
so those contexts remain unsupported. Job-level fields also reject computed,
whole, and projected `steps` and `needs` access.

Expression-valued `continue-on-error` must produce a Boolean. Expression-valued `timeout-minutes` must produce a number greater than 0 and at most 360.

Direct `github.token` references are step-only. Step runtime fields also support
the exact, case-insensitive call `toJSON(github)`. It serializes the retained
context listed below, including `token`, with sorted keys and two-space
indentation.

The compiler treats that call as a token reference, so normal permissions,
admission, and redaction apply. Composite steps can consume an already
authorized context, but composite metadata cannot grant token authority. A
tokenless context is an error.

Job-level fields and action input defaults cannot call `toJSON(github)`. Bare,
projected, or dynamically indexed `github`, and passing the whole context to
another function, remain unsupported.

`runner.os` and `runner.arch` resolve to `Linux`/`X64` or `macOS`/`ARM64`.
After runner setup, step runtime fields and job outputs can also use
`runner.temp`, which resolves to the canonical temporary directory exposed as
`RUNNER_TEMP`. Other runner fields and compile-time positions that require
runner identity are unsupported. Action metadata input defaults may also use
direct `runner.debug`, which resolves to the string `false` because Buildkite
has no equivalent step-debug mode.

A runtime interpolation can read a verified upstream output directly:

```yaml
run: echo "${{ needs.build.outputs.image }}"
```

The runtime retains this bounded `github` context:

| Fields | Behavior |
| --- | --- |
| `actor`, `event_name`, `ref`, `repository`, `sha` | Event identity from the compiled plan. |
| `repository_owner` | Derived from `repository`. |
| `server_url` | Identifies the event repository provider. |
| `job` | Workflow job ID. |
| `workflow` | Workflow name, or its path when unnamed. |
| `head_ref`, `base_ref` | Pull request source and target branches; empty for other events. |
| `ref_name` | Ref without `refs/heads/`, `refs/tags/`, or `refs/pull/`. Pull request refs use `<number>/merge` or `<number>/head`. |
| `ref_type` | `branch` for branch and pull request refs; `tag` for tag refs. |
| `action_path` | Composite action directory inside composite steps; empty elsewhere. |
| `action_repository`, `action_ref` | Remote composite repository and requested ref; empty for local composites and outside composite steps. |
| `token` | Available only in an authorized step expression. |

This is not the full GitHub context. `github.event` and the event payload are
not available at runtime.

`hashFiles()` evaluates when its step field is consumed, so it sees files from
earlier steps such as checkout. A JavaScript action's `with` and `env` can be
evaluated for `pre`, then evaluated again for `main`.

Patterns apply in order. `!` excludes matches; a later positive pattern can add
them back. Directory matches include descendants, hidden files match normally,
and overlapping patterns hash each path once. Matching is case-insensitive only
on Windows. An empty match returns an empty string.

For each file, `hashFiles()` calculates SHA-256 over its contents. It then hashes
the concatenated binary digests in lexical path order. GitHub Runner does not
specify glob traversal order, so a multi-file digest can differ when GitHub's
order is not lexical.

Patterns cannot be absolute or contain `..` segments or ASCII control
characters. Hashing stays inside the workspace and does not enter symlinked
directories. A matched symlink or other non-regular file fails the step.

GitHub Runner can hash a file symlink and has an optional symlink-following
mode. This runtime deliberately does neither.

### Compile-time expressions

Matrices, runner labels, names, concurrency groups, retained runtime templates,
and event-backed conditions can use statically known `github`, `event`, `vars`,
and matrix values.

Compile-time `github` fields are `actor`, `base_ref`, `event_name`, `head_ref`,
`ref`, `ref_name`, `ref_type`, `repository`, `repository_owner`, `sha`, and
`workflow`. Expressions can use computed indexes, numeric array indexes, and
`.*` projections when the complete result resolves during compilation.

Whole or dynamic `github` access and whole-event serialization remain
unsupported. Event-backed runtime expressions can combine reducible event
parts with values supported by their runtime surface.

## Actions

### Action sources and runtimes

| Action type | Status | Boundary |
| --- | --- | --- |
| Local `./...` action | 🟡 Supported subset | Source tree is digest-locked and reverified. |
| Public `owner/repo[/path]@ref` action | 🟡 Supported subset | Resolved to an exact commit and digest. |
| Private action | ❌ Unsupported | No private action source access. |
| JavaScript action | ✅ Supported | Declares `node16`, `node20`, or `node24`. |
| Composite action | 🟡 Supported subset | Nested shell steps and locked local or public actions; `bash`, `sh`, `python`, or a custom shell template for `run`; literal `continue-on-error`. |
| Dockerfile action | 🟡 Supported subset | Verified local or public Dockerfile action on Linux with optional bounded `runs.args`. Rejected on macOS, including through a composite action. |
| `docker://` action | ❌ Unsupported | Rejected during validation. |
| Top-level action metadata `env` | ➖ Accepted, no effect | Any valid YAML value is discarded. It is not evaluated, injected, retained in plans, or used to request secrets or tokens. |

Mutable public refs are resolved during upload, then locked to a commit. The importer lazily requests one Buildkite action-source token and reuses it across all workflow roots and nested composite actions. This token authenticates only public metadata requests for repositories other than the credential repository; the credential repository and codeload requests remain anonymous. If token issuance is unavailable during rollout, resolution safely falls back to anonymous GitHub API access. Exact lowercase commit SHAs need no GitHub API lookup. Complete source trees are verified again at runtime.

Nested calls from a repository-local composite must be local. Public composites may call local children or other public actions; every child is resolved and locked.

Dockerfile actions require exact `runs.image: Dockerfile`. Optional `runs.args`
must be an ordered YAML string array. Each item becomes one argument after the
image name, without a shell or an entrypoint override. Empty strings,
whitespace, and shell metacharacters remain literal. Omitted or empty args keep
the image `CMD`; any non-empty array replaces `CMD` while preserving the image
`ENTRYPOINT`.

Args may contain literals and direct `inputs.<name>` or `inputs['name']`
interpolation. Operators, functions, whole or dynamic inputs, and every other
context are rejected. Invocation inputs and metadata defaults resolve before
args evaluation. Args remain in digest-bound action metadata and are not stored
in job plans.

Dockerfile actions cannot declare explicit entrypoints or pre/post lifecycle,
or request credentials, volumes, arbitrary options, or privileged mode. An arg
such as `--privileged` remains a container argument; it cannot become a Docker
option.

Action metadata parsing remains strict for every other unknown top-level field and for unknown nested fields. The inert top-level `env` exception does not replace workflow or action-step environments, populate `runs.env`, or add `GITHUB_TOKEN` authority.

| Action declaration | Runtime |
| --- | --- |
| `node16` | Managed Node 16.20.2, with one end-of-job deprecation warning. |
| `node20` | Managed Node 24.18.0. |
| `node24` | Managed Node 24.18.0. |

Pre, main, and post phases; inputs; outputs; state; and LIFO post ordering are supported. Other Node declarations are rejected.

JavaScript action `pre-if` and `post-if` metadata uses the condition operators, status functions, pure functions, and `hashFiles()` described in [Conditions](#conditions). Lifecycle conditions can read direct properties from workflow `inputs`, `env`, `github`, `job.services`, `matrix`, `runner`, and `steps`. Other contexts and dynamic or whole-context access return an error. An empty lifecycle condition always runs and does not receive an implicit `success()` guard.

Pre conditions use the status and action-scoped environment available when preparation reaches the action. Post conditions run during job teardown and use the final job status and environment, including `GITHUB_ENV` changes from main. Root action posts also see final workflow step state. Nested composite actions retain their isolated step context. Cancellation remains distinct from failure, and posts keep LIFO order.

### Checkout action

**🟡 Supported subset.** The final v1.2.0, v2.8.0, and v3.7.0 release commits are admitted exactly. Resolved commits in the v4-and-later range of the static [`actions/checkout` upstream `main` snapshot](https://github.com/actions/checkout/tree/f548e57e544e1ff5a4c46bf1e1b8685f8e4a348a) are also admitted. The following known releases remain admitted even when their commits aren't reachable from that snapshot:

| Release | Commit |
| --- | --- |
| v1.2.0 | [`50fbc622fc4ef5163becd7fab6573eac35f8462e`](https://github.com/actions/checkout/tree/50fbc622fc4ef5163becd7fab6573eac35f8462e) |
| v2.8.0 | [`0717577d45739eb3c851188b29f50ed6c0b2194e`](https://github.com/actions/checkout/tree/0717577d45739eb3c851188b29f50ed6c0b2194e) |
| v3.7.0 | [`a37ce9120846195fa4ece8f58b268e6043cb2f26`](https://github.com/actions/checkout/tree/a37ce9120846195fa4ece8f58b268e6043cb2f26) |
| v4 | [`11d5960a326750d5838078e36cf38b85af677262`](https://github.com/actions/checkout/tree/11d5960a326750d5838078e36cf38b85af677262) |
| v5 | [`fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09`](https://github.com/actions/checkout/tree/fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09) |
| v6 | [`d23441a48e516b6c34aea4fa41551a30e30af803`](https://github.com/actions/checkout/tree/d23441a48e516b6c34aea4fa41551a30e30af803) |
| v7.0.0 corpus pin | [`9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0`](https://github.com/actions/checkout/tree/9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0) |
| v7.0.1 | [`3d3c42e5aac5ba805825da76410c181273ba90b1`](https://github.com/actions/checkout/tree/3d3c42e5aac5ba805825da76410c181273ba90b1) |

Mutable refs work only while they resolve to the upstream `main` snapshot or a known release above. Every admitted commit uses the native adapter; the upstream JavaScript doesn't run. Each admitted release accepts only the inputs it declares, so earlier releases reject later inputs. Other pre-v3.7.0 commits and unknown commits are unsupported. Compilation emits `W_CHECKOUT_LEGACY_RELEASE` for v1.2.0 and v2.8.0 to nudge an upgrade to v4 or later. Maintainers can update the v4-and-later snapshot with `go generate ./internal/action/integration`; this doesn't widen release admission.

The adapter checks out a detached commit or static branch from the event repository at the workspace root or a clean top-level directory. It uses Buildkite repository-provider Git credentials when the job provides them; otherwise, it fetches anonymously. Credentials are scoped to each fetch command and verified submodule fetch command and are never persisted.

| Input | Supported values |
| --- | --- |
| `repository` | Omitted, or the event `owner/repo`. |
| `ref` | Omitted, empty, a lowercase 40-hex commit, or a static branch in the event repository. A direct `github.sha` or `needs.<job>.outputs.<name>` expression must resolve at runtime to the exact event SHA. |
| `token` | Omitted only. |
| `ssh-key`, `ssh-known-hosts` | v2.8.0 and later: omitted or empty. v1.2.0: omitted. |
| `ssh-strict` | v2.8.0 and later: omitted or `true`. v1.2.0: omitted. |
| `ssh-user` | v4 and later: omitted or `git`. Earlier releases: omitted. |
| `persist-credentials` | v2.8.0 and later: omitted or `false`. v1.2.0: omitted. |
| `path` | Omitted, empty, or one clean non-`.git` top-level workspace directory. |
| `clean` | Omitted or `true`; the root workspace or selected path must be empty or absent. |
| `filter` | v4 and later: omitted or empty. Earlier releases: omitted. |
| `sparse-checkout` | v3.7.0 and later: omitted or empty. Earlier releases: omitted. |
| `sparse-checkout-cone-mode` | v3.7.0 and later: omitted or `true`. Earlier releases: omitted. |
| `fetch-depth` | Omitted or a nonnegative integer; `0` fetches full history. v1.2.0 fetches full history when omitted. |
| `fetch-tags` | v3.7.0 and later: omitted, `true`, or `false`. Earlier releases: omitted. |
| `show-progress` | v4 and later: omitted, `true`, or `false`. Earlier releases: omitted. |
| `lfs` | Omitted or `false`. |
| `submodules` | Omitted, `false`, `true`, or `recursive`; whitespace is trimmed and casing is ignored. |
| `set-safe-directory` | v2.8.0 and later: omitted or `true`. v1.2.0: omitted. |
| `github-server-url` | v3.7.0 and later: omitted, empty, or `https://github.com`. Earlier releases: omitted. |
| `allow-unsafe-pr-checkout` | v2.8.0 and later: omitted or `false`. v1.2.0: omitted. |

The `ref` and `commit` outputs are unavailable for v1.2.0, v2.8.0, and v3.7.0. Upstream added them in v4.2.0.

The `false` value and omission do not run submodule commands. The `true` value runs native Git for direct children, and `recursive` includes nested children. Relative URLs and `fetch-depth` follow native Git behavior. Public and private GitHub submodules are supported under the job's repository access; external HTTPS submodules are anonymous. `git@github.com:` URLs are rewritten to HTTPS. Other SSH and non-HTTPS transports are unsupported.

See the [security model](security.md#checkout-and-submodules) for credential, Git, and job-isolation boundaries.

Alternate repositories, tags, non-event dynamic commits, LFS, sparse checkout, GitHub Enterprise Server, and credential persistence remain unsupported. Commit and branch checkouts remain detached and confined to the event repository.

### Upload artifact action

**🟡 Supported subset.** These root `actions/upload-artifact` actions use a native Buildkite ZIP adapter:

| Release | Commit |
| --- | --- |
| v1.0.0 | [`3446296876d12d4e3a0f3145a3c87e67bf0a16b5`](https://github.com/actions/upload-artifact/tree/3446296876d12d4e3a0f3145a3c87e67bf0a16b5) |
| v2.3.1 | [`82c141cc518b40d92cc801eee768e7aafc9c2fa2`](https://github.com/actions/upload-artifact/tree/82c141cc518b40d92cc801eee768e7aafc9c2fa2) |
| v3.2.1 | [`ff15f0306b3f739f7b6fd43fb5d26cd321bd4de5`](https://github.com/actions/upload-artifact/tree/ff15f0306b3f739f7b6fd43fb5d26cd321bd4de5) |
| v4.6.2 | [`ea165f8d65b6e75b540449e92b4886f43607fa02`](https://github.com/actions/upload-artifact/tree/ea165f8d65b6e75b540449e92b4886f43607fa02) |
| v5.0.0 | [`330a01c490aca151604b8cf639adc76d48f6c5d4`](https://github.com/actions/upload-artifact/tree/330a01c490aca151604b8cf639adc76d48f6c5d4) |
| v6.0.0 | [`b7c566a772e6b6bfb58ed0dc250532a479d7789f`](https://github.com/actions/upload-artifact/tree/b7c566a772e6b6bfb58ed0dc250532a479d7789f) |
| v7.0.1 | [`043fb46d1a93c77aae656e7c1c64a875d1fc6a0a`](https://github.com/actions/upload-artifact/tree/043fb46d1a93c77aae656e7c1c64a875d1fc6a0a) |

The v1.0.0, v2.3.1, and v3.2.1 commits match the floating legacy major tags used on github.com. Other legacy commits are unsupported, including v3.2.2, which upstream publishes only as a GitHub Enterprise Server security backport and deprecates on github.com. Every admitted release accepts only its declared inputs. Compilation emits `W_UPLOAD_ARTIFACT_LEGACY_RELEASE` for v1 through v3 to recommend v4 or later.

| Input | Supported values |
| --- | --- |
| `name` | v1.0.0: required. v2.3.1 and later: defaults to `artifact`. |
| `path` | Required. v1.0.0 accepts one literal file or directory. v2.3.1 and later accept literal paths or bounded `*`, `?`, character-class, and `**` file globs. |
| `if-no-files-found` | v2.3.1 and later: `warn`, `error`, or `ignore`. v1.0.0 fails when its literal path is missing and uploads an empty existing directory. |
| `retention-days` | v2.3.1 and later: nonnegative integer; advisory only. |
| `compression-level` | v4.6.2 and later: `0` through `9`. |
| `overwrite` | v4.6.2 and later: omitted or `false`. |
| `include-hidden-files` | v3.2.1 and later. v1.0.0 and v2.3.1 include hidden paths by default. |
| `archive` | v7.0.1 only; omitted or `true`. |

Unsupported path forms include exclusions, symlinks, absolute paths, traversal, braces, extglobs, leading glob comments, and special files. At most 32 path roots may be selected. For v3.2.1 and later, hidden path segments remain excluded unless explicitly enabled.

An artifact may contain at most 10,000 files and 1 GiB of source data. The ZIP must also be no larger than 1 GiB. A job may publish 64 artifacts.

For v4.6.2 and later, the adapter sets `artifact-id` and `artifact-digest`; `artifact-url` is empty because no GitHub run-scoped URL exists. The v1 through v3 releases expose no outputs. Merge, raw upload, overwrite, and effective retention control are unsupported.

### Download artifact action

**🟡 Supported subset.** These root `actions/download-artifact` actions use the same producer-bound ZIP mode:

| Release | Commit |
| --- | --- |
| v4.3.0 | [`d3f86a106a0bac45b974a628896c90dbdf5c8093`](https://github.com/actions/download-artifact/tree/d3f86a106a0bac45b974a628896c90dbdf5c8093) |
| v5.0.0 | [`634f93cb2916e3fdff6788551b99b062d0335ce0`](https://github.com/actions/download-artifact/tree/634f93cb2916e3fdff6788551b99b062d0335ce0) |
| v6.0.0 | [`018cc2cf5baa6db3ef3c5f8a56943fffe632ef53`](https://github.com/actions/download-artifact/tree/018cc2cf5baa6db3ef3c5f8a56943fffe632ef53) |
| v7.0.0 | [`37930b1c2abaa49bbe596cd826c3c89aef350131`](https://github.com/actions/download-artifact/tree/37930b1c2abaa49bbe596cd826c3c89aef350131) |
| v8.0.0 | [`70fc10c6e5e1ce46ad2ea6f2b72d43f7d47b13c3`](https://github.com/actions/download-artifact/tree/70fc10c6e5e1ce46ad2ea6f2b72d43f7d47b13c3) |
| v8.0.1 | [`3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c`](https://github.com/actions/download-artifact/tree/3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c) |

| Input | Supported values |
| --- | --- |
| `name` | Exact name, mutually exclusive with `pattern`; runtime expressions are allowed. |
| `pattern` | Bounded artifact-name glob; requires `merge-multiple: true`; runtime expressions are allowed. |
| `path` | Optional literal workspace-relative path. |
| `merge-multiple` | Omitted or `false` with `name`; required `true` with `pattern`. |
| v8 `skip-decompress` | Omitted or `false`. |
| v8 `digest-mismatch` | Omitted or `error`. |

Artifacts must come from verified direct `needs` producers. Exact-name lookup must find one unique artifact. A bounded pattern may select and deterministically merge up to 64 distinct names. The shipped pattern contract accepts `*`, `?`, character classes, and `**`. Brace alternation remains unsupported pending hosted proof.

When artifacts contain the same exact member path, the later artifact by name wins. All matched archives are validated and staged before the destination changes. Artifact ID, all-artifact, cross-run, cross-repository, raw, REST, and non-merged pattern modes are unsupported.

Only ZIPs produced by the supported upload adapter are accepted. Digest or ZIP validation failure is fatal. The `download-path` output is supported.

### Cache action

**🟡 Supported subset.** The exact releases below run their stock cache-v2 clients against the Buildkite Results service. Root, `restore`, and `save` entry points are supported.

| Release | Commit | Node | `@actions/cache` |
| --- | --- | --- | --- |
| v3.4.0 | [`f4b3439a656ba812b8cb417d2d49f9c810103092`](https://github.com/actions/cache/tree/f4b3439a656ba812b8cb417d2d49f9c810103092) | 16 | 4.0.0 |
| v3.4.2 | [`387e18722e6ff315b24a3b8b071feddd27b7bf7e`](https://github.com/actions/cache/tree/387e18722e6ff315b24a3b8b071feddd27b7bf7e) | 16 | 4.0.1 |
| v3.4.3 | [`2f8e54208210a422b2efd51efaa6bd6d7ca8920f`](https://github.com/actions/cache/tree/2f8e54208210a422b2efd51efaa6bd6d7ca8920f) | 16 | 4.0.2 |
| v3.5.0 | [`6f8efc29b200d32929f49075959781ed54ec270c`](https://github.com/actions/cache/tree/6f8efc29b200d32929f49075959781ed54ec270c) | 16 | 4.1.0 |
| v4.2.0 | [`1bd1e32a3bdc45362d1e726936510720a7c30a57`](https://github.com/actions/cache/tree/1bd1e32a3bdc45362d1e726936510720a7c30a57) | 20 | 4.0.0 |
| v4.2.1 | [`0c907a75c2c80ebcb7f088228285e798b750cf8f`](https://github.com/actions/cache/tree/0c907a75c2c80ebcb7f088228285e798b750cf8f) | 20 | 4.0.1 |
| v4.2.2 | [`d4323d4df104b026a6aa633fdb11d772146be0bf`](https://github.com/actions/cache/tree/d4323d4df104b026a6aa633fdb11d772146be0bf) | 20 | 4.0.2 |
| v4.2.3 | [`5a3ec84eff668545956fd18022155c47e93e2684`](https://github.com/actions/cache/tree/5a3ec84eff668545956fd18022155c47e93e2684) | 20 | 4.0.3 |
| v4.2.4 | [`0400d5f644dc74513175e3cd8d07132dd4860809`](https://github.com/actions/cache/tree/0400d5f644dc74513175e3cd8d07132dd4860809) | 20 | 4.0.5 |
| v4.3.0 | [`0057852bfaa89a56745cba8c7296529d2fc39830`](https://github.com/actions/cache/tree/0057852bfaa89a56745cba8c7296529d2fc39830) | 20 | 4.1.0 |
| v5.0.0 | [`a7833574556fa59680c1b7cb190c1735db73ebf0`](https://github.com/actions/cache/tree/a7833574556fa59680c1b7cb190c1735db73ebf0) | 24 | 5.0.0 |
| v5.0.1 | [`9255dc7a253b0ccc959486e2bca901246202afeb`](https://github.com/actions/cache/tree/9255dc7a253b0ccc959486e2bca901246202afeb) | 24 | 5.0.1 |
| v5.0.2 | [`8b402f58fbc84540c8b491a91e594a4576fec3d7`](https://github.com/actions/cache/tree/8b402f58fbc84540c8b491a91e594a4576fec3d7) | 24 | 5.0.3 |
| v5.0.3 | [`cdf6c1fa76f9f475f3d7449005a359c84ca0f306`](https://github.com/actions/cache/tree/cdf6c1fa76f9f475f3d7449005a359c84ca0f306) | 24 | 5.0.5 |
| v5.0.4 | [`668228422ae6a00e4ad889ee87cd7109ec5666a7`](https://github.com/actions/cache/tree/668228422ae6a00e4ad889ee87cd7109ec5666a7) | 24 | 5.0.5 |
| v5.0.5 | [`27d5ce7f107fe9357f9df03efb73ab90386fccae`](https://github.com/actions/cache/tree/27d5ce7f107fe9357f9df03efb73ab90386fccae) | 24 | 5.0.5 |
| v5.1.0 | [`caa296126883cff596d87d8935842f9db880ef25`](https://github.com/actions/cache/tree/caa296126883cff596d87d8935842f9db880ef25) | 24 | 5.1.0 |
| v6.0.0 | [`2c8a9bd7457de244a408f35966fab2fb45fda9c8`](https://github.com/actions/cache/tree/2c8a9bd7457de244a408f35966fab2fb45fda9c8) | 24 | 6.0.1 |
| v6.1.0 | [`55cc8345863c7cc4c66a329aec7e433d2d1c52a9`](https://github.com/actions/cache/tree/55cc8345863c7cc4c66a329aec7e433d2d1c52a9) | 24 | 6.1.0 |

The v3 releases use managed Node 16 and emit its standard deprecation warning. Node 20 declarations run with managed Node 24. Every admitted bundle selects cache v2 from `ACTIONS_CACHE_SERVICE_V2`, uses `ACTIONS_RESULTS_URL` and a job-scoped runtime token, and preserves the root restore/post-save lifecycle and separate entry points. A non-routable `ACTIONS_CACHE_URL` satisfies the legacy availability gate; cache traffic still uses `ACTIONS_RESULTS_URL`. Their tar with zstd-or-gzip archive versioning is compatible across releases.

v3.4.1 is excluded because [its upstream release warns that it was published with an incorrect SHA](https://github.com/actions/cache/releases/tag/v3.4.1). Releases before v3.4.0 and v4.2.0 bundle cache-v1 clients. Floating tags, prereleases, unknown commits, and future releases require a source and bundled-dependency audit before admission.

Hosted runtime proof covers v6.1.0 and a v3.4.0 producer with a v6.1.0
consumer. [Build 1173](https://buildkite.com/buildkite/buildkite-gha/builds/1173)
(Buildkite access required) saved a unique v3 archive, restored it with v6, and
verified the payload. Hosted validation checks resolution, compilation, and
admission for every listed commit; it does not execute the actions.

JavaScript and Docker actions with compatible bundled cache clients also receive job-bound cache-v2 credentials when the service is available. Root invocations of `actions/setup-node`, `actions/setup-java`, `actions/setup-python`, `actions/setup-go`, and `actions/setup-dotnet` use a subprocess-scoped synthetic `GITHUB_SERVER_URL` when the real host would make their clients select cache v1. Each allowlist entry requires an audit of the action source and bundled dependencies to confirm that `GITHUB_SERVER_URL` affects only caching behavior and is not load-bearing for any request the action makes. The workflow expression context retains the real server URL. Ordinary `run` steps and native action adapters do not receive cache credentials.

## Repositories, credentials, and GitHub services

### Repositories

| Source | Status | Boundary |
| --- | --- | --- |
| Public GitHub event repository | ✅ Supported | No additional boundary. |
| Private GitHub event repository | 🟡 Supported subset | Buildkite must authorize repository-provider Git credentials. |
| Internal or private Origin event repository | 🟡 Supported subset | `BUILDKITE_REPO` must be the pipeline's exact `https://origin.cursor.com/git/<namespace>/<repository>.git` URL. Buildkite must authorize repository-provider Git credentials. |
| Alternate repository in `actions/checkout` | ❌ Unsupported | Not available. |
| Public GitHub action | 🟡 Supported subset | Subject to the action boundaries above. |
| Private action or reusable workflow | ❌ Unsupported | Not available. |
| GitHub Enterprise Server or an unlisted provider | ❌ Unsupported | Not available. |

### GitHub token

**🟡 Supported subset.** A job requests one short-lived `GITHUB_TOKEN` for the
event repository when it:

- statically references `secrets.GITHUB_TOKEN` or `github.token`; or
- uses an action whose effective input default can reach `github.token` for the
  event provider.

A `github.server_url == 'https://github.com'` guard skips the token branch for
an Origin repository. Native adapters ignore upstream input defaults, so
`actions/checkout` alone does not request a token.

The top-level workflow's `permissions` set the scope. Token issuance needs a
Buildkite organization feature and a pipeline setting; both are off by default.

Buildkite reads that policy from the pipeline repository at the immutable build
commit. The workflow must be a simple `.yml` or `.yaml` file directly under
`.github/workflows/`.

- Omitted permissions mean exactly `contents: read`.
- GitHub repository and organization defaults are not inherited.
- `read-all` and `write-all` become explicit 13-scope maps.
- Write access needs an explicit, non-empty top-level map.
- An empty map, or scopes that all resolve to `none`, creates no token.
- Job-level repository permission maps do not change the scope.

Compilation warns when job permissions differ from the applied top-level map.
Server support for each immutable source policy must be deployed before a
client that compiles the corresponding alias.

Reusable-workflow jobs receive the requesting workflow's top-level repository
permissions. Buildkite does not inspect called-workflow maps for `GITHUB_TOKEN`,
so those maps cannot narrow it. The separate `id-token` permission still
supports called-workflow narrowing. Compilation warns when a called policy
would have narrowed the repository token. Private reusable workflows remain
unsupported.

Pull requests and their triggered or rebuilt descendants have a `contents: read`
ceiling. Merge-queue builds and their descendants cannot request a token. GitHub
Enterprise Server is unsupported. The backend verifies provenance and remains
authoritative.

A job can request read-only repository access:

```yaml
permissions:
  contents: read

jobs:
  inspect:
    runs-on: ubuntu-latest
    steps:
      - env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: gh api repos/{owner}/{repo}
```

The server restricts pull requests, merge queues, and their descendants. For other builds, job binding does not establish that an arbitrary commit is trusted. Restrict who can create builds and enable write tokens only when branch builds run trusted code.

The token is not part of the initial job environment. Workflow-authored
`github.token` references are step-only and use the same token as
`secrets.GITHUB_TOKEN`. Effective action input defaults can also use it.
Automatic ambient `GITHUB_TOKEN` is unsupported.

### Other secrets and OIDC

**🟡 Supported subset.** Direct jobs can use static `${{ secrets.NAME }}`
references. Local reusable-workflow jobs can inherit or explicitly map declared
secret aliases.

The compiler records names, not values, in the destination job plan. At
runtime, the job calls `buildkite-agent secret get NAME` and registers the value
with both redactors before use. Missing or denied secrets fail without printing
the secret or Agent error. The job annotation explains how to create or migrate
the secret and check its access policy.

These are Buildkite destination-job secrets, not GitHub repository,
environment, event, or fork-scoped secrets. Buildkite Secret access policies
are the authorization boundary. Code in the same job can also call
`buildkite-agent secret get`.

`GITHUB_TOKEN` stays on its separate workflow-token contract and cannot be
replaced by an ordinary Buildkite secret.

Unsupported secret uses include:

- dynamic, whole-context, filtered, or projected access
- conditions and other compile-time expressions
- GitHub environments and environment secrets
- remote reusable-workflow secret forwarding
- literals, compound expressions, or references through `needs`, `vars`, `env`,
  `inputs`, or arbitrary `github` properties in explicit mappings

Action metadata cannot add secret authority to a plan. A secret used only by an
optional action input becomes an empty value unless another field requires it.

Jobs with `id-token: write` expose the GitHub Actions `getIDToken()` contract to
host JavaScript actions, including those called by composite actions. The
endpoint mints a Buildkite OIDC token for the requested audience. Cloud identity
providers must trust Buildkite's issuer and claims, not GitHub's.

`id-token: read`, `id-token: none`, and omitted permissions do not expose the
endpoint. Repository tests cover the wire contract; hosted runtime proof remains
pending.

The plugin can apply additional Buildkite OIDC settings to every mint:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/deploy.yml
      oidc:
        claims: [organization_id]
        aws-session-tags: [organization_slug, pipeline_id]
        subject-claim: pipeline_id
```

`claims` and `aws-session-tags` accept non-empty lists. `subject-claim` accepts
one non-empty immutable claim name. The Agent API owns the claim vocabulary and
rejects unsupported names when the job mints a token.

Plugin OIDC configuration does not grant `id-token: write`. A job without that
permission receives no endpoint.

The endpoint variables are scoped to each host action lifecycle invocation. Shell steps, Docker container actions, and actions running in job containers do not receive them. Container actions that call `getIDToken()` fail with its missing endpoint variable diagnostic.

### GitHub services

**❌ Unsupported beyond the integrations listed above.** An action's runtime may still require unsupported GitHub services. Buildkite provides no GitHub Packages, Releases, Checks, or deployment service emulation beyond the documented integrations.

## Runtime behavior and limits

### Default environment

The runtime sets `GITHUB_WORKFLOW` to the workflow's top-level `name`. If the workflow has no name, it uses the repository-relative workflow path. Workflow and step environment entries cannot override this value.

### Runner tools

Linux labels use the corresponding Noble or Jammy hosted-toolchains image.
macOS agents must provide tools used by shell steps. These images do not provide GitHub image parity. The runtime
sets `RUNNER_OS` and `RUNNER_ARCH` to `Linux`/`X64` or `macOS`/`ARM64`.

`RUNNER_TOOL_CACHE` is job-private unless the Linux job selects an immutable
image with `/opt/hostedtoolcache`, which the default and configured
hosted-toolchains images provide. macOS images are unsupported.

### Results, retries, and cancellation

- A runtime-skipped Actions job appears successful in Buildkite while publishing a logical `skipped` result for downstream imported jobs.
- Retry the whole build if a producer result or artifact becomes ambiguous.
- Cancellation targets the complete process tree: `SIGINT`, `SIGTERM` after 7.5 seconds, then `SIGKILL` after another 2.5 seconds.
- Summary or annotation publication failure produces a warning and does not change a completed job result.

### Key limits

| Item | Limit |
| --- | --- |
| Matrix instances per job | 256 |
| Reusable workflow nesting | 4 levels |
| Workflow file | 1 MiB |
| Jobs after reusable workflow expansion | 1,024 |
| Nested local action depth | 10 levels |
| Background steps active at once | 10 |
| Job outputs | 64 |
| Output value | 1 KiB |
| Job or step timeout | 360 minutes |
| Artifacts per job | 64 |
| Files per uploaded artifact | 10,000 |
| Uploaded source data or ZIP | 1 GiB |
| Job summary | 1 MiB |
| `hashFiles()` patterns | 255 per call; 1 KiB each; 64 KiB total |
| `hashFiles()` workspace entries | 100,000 per call |
| `hashFiles()` matched files | 10,000 per call |
| `hashFiles()` selected bytes | 1 GiB per call |

## Validation

Check syntax, static graph construction, and every declared trigger without an event:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

This result is event-independent and does not claim hosted admission.

Apply the same profile as production upload:

```sh
buildkite-gha validate \
  --profile hosted \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

Use `--event push`, `--event pull_request`, `--event merge_group`, `--event release`, `--event workflow_dispatch`, or `--event schedule` instead of `--event-path` to evaluate the hosted profile with a generated minimal snapshot. The generated release event is a stable `published` event. Generated snapshots are representative compatibility test inputs, not proof of every activity or equivalents to real payloads. The options are mutually exclusive.

Use `--all-events` to evaluate every declared supported event separately. Its `processing-report/v3` output preserves the event-independent result and each generated event's v2 report. Aggregate admission means every generated snapshot was admitted; it does not cover other payload shapes. A `context-required` result means compilation and hosted-policy checks passed, but generated inputs cannot measure a supported admission path, such as push or pull-request path filters without linked webhook and local diff evidence. It does not claim admission.

The results mean:

- **Compilable**: Syntax, declared triggers, and the static job graph can be translated.
- **Not applicable**: The workflow does not declare the selected event, so upload would skip it without compiling it.
- **Admitted**: Resolved actions and generated plans pass production policy.
- **Context required**: A supported admission path needs evidence that this validation input does not provide.
- **Runtime-proven**: Repository tests or hosted evidence have executed the behavior.

Admission does not execute arbitrary action code. An admitted action may still depend on an unsupported GitHub service.

See the [CLI guide](cli.md) for event snapshots, JSON reports, compilation, and direct upload.
