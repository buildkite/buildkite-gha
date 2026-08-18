# GitHub Actions compatibility

<!-- (internal) If this file is ever moved, please update the GitHub Actions template -->

This page defines the initial production contract for `buildkite-gha`. It applies to the `hosted` profile used by `upload` and the Buildkite plugin.

The released plugin path supports Linux x86-64 and native macOS arm64 importers
and generated jobs, including the matching `runner.os` and `runner.arch` values.
Importer agent targeting is independent of generated-job runner mappings.
Platform labels do not provide GitHub image or toolchain parity.
Generated Linux jobs use a dedicated `runner` user and require buildkite-gha
v0.13.7 or newer. Set `experimental-runner-user: false` temporarily if the
runner image cannot meet the root bootstrap requirements.

GitHub Actions syntax changes over time. If a feature is not listed here, treat it as unsupported. GitHub's [workflow syntax reference](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax) describes the original syntax; this page describes the subset that runs on Buildkite.

## Support matrix

| Status | Meaning |
| --- | --- |
| ✅ **Supported** | Available through the production plugin path. |
| 🟡 **Supported subset** | Available within the limits shown here. |
| ➖ **Accepted, no effect** | Validation accepts the syntax, but it does not change the build. |
| 🚧 **Not available in production** | The compiler or runtime supports it, but production upload blocks it. |
| ❌ **Unsupported** | Rejected or outside the compatibility contract. |

Looking for something else? [Browse open compatibility issues](https://github.com/buildkite/buildkite-gha/issues?q=is%3Aissue%20state%3Aopen%20label%3Acompatibility). This page is the source of truth for what works today.

| Area | Status | Initial release boundary |
| --- | --- | --- |
| [Workflow and job names](#workflow-syntax) | 🟡 Supported subset | `name` and job names are retained. `run-name` has no effect. |
| [Triggers and filters under `on`](#names-and-triggers) | 🟡 Supported subset | Buildkite creates builds; upload selects aggregate workflow groups for one effective event. Local `workflow_call` is supported for composition. |
| [Platforms](#job-configuration) | 🟡 Supported subset | The hosted preset provides Linux x86-64 with `ubuntu-latest`, `ubuntu-24.04`, or `ubuntu-22.04`, and native macOS arm64 with `macos-latest`. Organization-provided queues can map `macos-14` or `macos-15`. Labels do not provide GitHub image, toolchain, or Xcode parity. |
| [Jobs and dependencies](#job-configuration) | ✅ Supported | Static dependencies, matrix fan-out and fan-in, results, and bounded outputs. |
| [Matrix strategies](#matrix-strategies) | 🟡 Supported subset | Static matrices, `include`, `exclude`, and literal `max-parallel`. Maximum 256 instances per job. `fail-fast` has no effect. |
| [Shell steps](#commands-and-actions) | 🟡 Supported subset | Linux and macOS `bash` and `sh`. |
| [Conditions and expressions](#expressions-and-contexts) | 🟡 Supported subset | GitHub-compatible core operators and direct references to selected contexts. |
| [Reusable workflows](#reusable-workflows) | 🟡 Supported subset | Local workflows with static inputs and direct job-output mappings. Secret forwarding is unsupported. |
| [Actions](#actions) | 🟡 Supported subset | Local and public JavaScript and composite actions on Linux and macOS; verified Dockerfile actions on Linux only. |
| [Checkout, artifacts, and cache](#actions) | 🟡 Supported subset | Only the audited versions and modes listed below. |
| [`GITHUB_TOKEN`](#github-token) | 🟡 Supported subset | One job-bound token for the event repository. Local reusable-workflow jobs use the top-level workflow permissions. |
| [Other workflow secrets](#other-secrets-and-oidc) | 🟡 Supported subset | Static names in direct jobs resolve through the destination job's Buildkite secret authority. |
| [Job and service containers](#containers-and-services) | 🟡 Supported subset | Linux job containers and broadly compatible service definitions, including explicit registry credentials. |
| [Environments and snapshots](#job-configuration) | 🟡 Supported subset | Environments are rejected. Snapshots are accepted with no effect. |
| [OIDC](#other-secrets-and-oidc) | 🟡 Supported subset | Host JavaScript and composite actions can request Buildkite OIDC tokens in jobs with `id-token: write`. |
| [Other platforms](#job-configuration) and [providers](#repositories) | ❌ Unsupported | Windows, Linux arm64, macOS x86-64, GitHub Enterprise Server, and unlisted providers are outside the initial release. |
| [Other GitHub services](#github-services) | ❌ Unsupported | No general emulation for Releases, Packages, Checks, deployments, or GitHub artifact APIs. |

## Outside the initial scope

Windows execution is outside the initial product scope. `buildkite-gha`
currently targets Linux x86-64 and native macOS arm64. It rejects Windows
runner labels instead of mapping them to a different platform.

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

The plugin accepts one explicit `workflow` path or a non-empty `workflows` path array. Plugin paths and aggregate `upload` operands must identify regular, tracked `.yml` or `.yaml` files inside the repository; directories, missing or untracked files, outside paths, symlinks, and globs fail. A custom importer may upload one explicit regular workflow outside the repository. Inputs are canonicalized, sorted, and deduplicated before workflow identities and job-key namespaces are assigned.

All directly runnable workflows are represented in one artifact and pipeline transaction. Each successfully compiled workflow becomes an aggregate group labeled `:github: <workflow-name>`, with its canonical path as the fallback for an unnamed workflow. A workflow that declares the effective event compiles into child jobs. Each child publishes a provider check named `<workflow-name-or-path> / <job-id> (<effective-event>)`; matrix instances append their sorted matrix values to the job ID. GitHub events publish GitHub checks. Origin events publish Origin checks keyed by the jobs' stable generated identities. A workflow that does not declare the event becomes one top-level skipped command step with no plan artifacts and a check named `<workflow-name-or-path> (<effective-event>)`. After upload, an importer-scoped info annotation lists workflows skipped by event or trigger filters, explains each mismatch, and links each workflow to its generated pipeline step. Annotation publication failure emits a warning without failing the importer. Group labels are static across events. Groups and replacement steps depend on the importer, while group child jobs omit that redundant dependency.

Reusable-only `workflow_call` files remain available to local callers but do not create groups. Selecting only reusable workflows is an error. Every directly runnable workflow is selected against the effective event. A workflow with safe compilation or trigger-translation errors is replaced by one failing top-level command step labeled `:github: <workflow-name-or-path>`. The replacement step publishes all redacted diagnostics as a job-scoped Buildkite annotation and exits with status 1. The provider check title is `Workflow could not be run`, and its summary lists redacted errors with workflow, job, and step context. Summaries are truncated at 65,535 bytes. A compiler failure takes precedence if the workflow also has a skip reason. Other workflows continue compiling and successful workflows retain their normal groups and jobs. Parse, event-input, admission, artifact, and upload failures still abort the whole transaction; no partial pipeline is uploaded.

### Compatibility diagnostics

Each diagnostic separates user guidance from implementation information. `message` names the user-visible cause and a corrective action. Optional `detail` contains lower-level context such as resolved commits, adapter or service boundaries, and supported-version or runner-policy allowlists. Text reports, JSON reports, Buildkite annotations, and generated workflow failure artifacts preserve this separation. GitHub check summaries show the concise message only.

## Workflow syntax

### Names and triggers

| Key | Status | Behavior |
| --- | --- | --- |
| `name` | ✅ Supported | Available as `github.workflow` and used to name generated work. |
| `run-name` | ➖ Accepted, no effect | Buildkite names the build. The value is not retained. |
| `on` | 🟡 Supported subset | Does not create a Buildkite build. Selects and filters aggregate groups for the effective event as described below. |

A workflow name is retained in generated work:

```yaml
name: CI
```

Buildkite controls when a build starts. The trigger declaration controls whether and under which condition the workflow group participates in that existing build:

```yaml
on:
  push:
    branches: [main]
```

Upload selects one authoritative effective event, in this order:

1. The event in an explicit `--event-path` snapshot.
1. The GitHub event name accompanying Buildkite's reserved linked-webhook metadata.
1. A Buildkite environment fallback: pull request builds use `pull_request`; `ui` and `api` use `workflow_dispatch`; `schedule` uses `schedule`; and every other source, including `trigger_job`, uses `push`.

An explicit event snapshot never consults contradictory live Buildkite event fields. Linked `merge_group` webhooks must match the Buildkite merge queue head and base refs and commits. Linked `release` webhooks must match the Buildkite event, action, branch, and tag; the plugin normalizes Buildkite's symbolic `HEAD` commit to the checked-out peeled tag commit first. The fallback cannot infer a merge group or release without linked webhook data and can classify `trigger_job` as `push` even when `build.source_event` is absent. The selected snapshot is then used consistently for applicability, event-dependent validation and compilation, the group condition, and the event suffix in its provider check.

| Event | Supported trigger behavior |
| --- | --- |
| `push` | `branches`, `branches-ignore`, `tags`, and `tags-ignore`, including ordered negative patterns in an include list. Branch and tag filters select their corresponding ref kind. Matching `paths` and `paths-ignore` can be admitted for linked GitHub branch pushes when the bounded local-diff requirements below are met. |
| `pull_request` | `branches` and `branches-ignore` match the base branch. Omitted `types` defaults to `opened`, `synchronize`, and `reopened`; explicitly listed activity types must map exactly to a supported Buildkite source action. Matching `paths` and `paths-ignore` can be admitted when the bounded local-diff requirements below are met. |
| `merge_group` | Native Buildkite merge queue builds only. Enable merge queue builds and Merge groups webhook delivery in the pipeline's GitHub settings. `branches` and `branches-ignore` match the target branch. The only supported activity is `checks_requested`; other types and path, tag, and workflow filters are rejected. The merge group ref and SHA identify the speculative queue commit. |
| `release` | Native Buildkite release builds only. In the pipeline's GitHub settings, enable **Additional Webhooks** > **Releases** and use **Code** trigger mode. `types` is required and may contain only `published`, `created`, and `released`; bare `release`, all other activity types, and branch, tag, path, and workflow filters are rejected. Draft `created` deliveries are rejected. The ref is `refs/tags/<tag_name>` and the SHA is the checked-out peeled tag commit. |
| `workflow_dispatch` | Selected for Buildkite UI and API builds. Webhook-style branch, tag, type, and workflow filters are unsupported. |
| `schedule` | Selected for Buildkite scheduled builds. Buildkite owns cron configuration and does not expose which schedule started a build, so every `on.schedule` workflow is eligible for every Buildkite scheduled build. |
| `workflow_call` | Defines a local reusable-workflow interface. A reusable-only file is available to callers but does not become a top-level group. |

Supported `pull_request` activity types are `assigned`, `unassigned`, `labeled`, `unlabeled`, `opened`, `edited`, `closed`, `reopened`, `synchronize`, `converted_to_draft`, `locked`, `unlocked`, `enqueued`, `dequeued`, `milestoned`, `demilestoned`, `ready_for_review`, `review_requested`, `review_request_removed`, `auto_merge_enabled`, and `auto_merge_disabled`.

GitHub defines seven release activities: `published`, `unpublished`, `created`, `edited`, `deleted`, `prereleased`, and `released`. A bare `on: release` selects all seven, so it cannot map exactly to Buildkite's three delivered activities and is unsupported.

#### Push path filters

For a linked GitHub branch push, the importer binds the webhook repository, ref, `before`, `after`, `created`, `deleted`, `forced`, and complete pushed-commit list to the Buildkite build and local checkout. It also requires `HEAD`, the local `origin` repository and remote branch, and the workflow file to match the pushed commit.

Normal and force pushes use GitHub's two-dot `before..after` comparison. A new branch uses the parent of its oldest pushed commit when the complete webhook commit set forms one unambiguous, single-parent boundary. Matching added, modified, deleted, and type-changed paths are admitted. Path patterns use the same ordered matching described below.

Admission fails closed for deleted refs, non-GitHub repositories, missing or shallow history, a stale or mismatched checkout, origin, remote branch, workflow, ref, repository, commit set, or force state, and ambiguous new-branch history. It also fails closed for more than 1,000 pushed commits, more than 300 changed files, combined additions and deletions, renames, malformed Git output, invalid patterns, and local non-matches. GitHub runs automatically after its 1,000-commit or diff-timeout fallback, but the importer does not manufacture that admission without matching changed-path evidence.

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

Before upload, the importer compares the pull request merge base with its head using the local checkout. It admits a workflow only when a changed path matches and the linked webhook, commits, synthetic merge, base branch, and workflow file all agree. It uses the checkout's existing Git access for public, private, and fork pull requests. It does not call GitHub or use Buildkite `if_changed`.

| Admitted | Fails closed |
| --- | --- |
| A matching added, modified, deleted, or type-changed path | No local match |
| A copied destination that matches | A rename, or a diff containing both additions and deletions |
| At most 300 changed files from complete local history | Missing or shallow history, multiple merge bases, or more than 300 files |
| A mergeable pull request with matching webhook and workflow data | A conflict, stale data, changed merge workflow, path or pattern containing a backslash, invalid pattern, or malformed Git output |

A local non-match fails closed because GitHub does not report whether its diff timed out and ran the workflow anyway. Unfiltered `closed` workflows remain supported; filtered `closed` workflows fail closed when GitHub supplies an actual merge, squash, or rebase commit instead of a synthetic merge. Other unsupported or inexact trigger filters replace the affected workflow with a failing step rather than broadening when it runs.

A top-level workflow that does not declare the effective event is excluded before event-dependent validation or compilation and represented by one top-level skipped command step. A workflow that declares that event remains represented by a group even when a same-event branch, tag, base-branch, or action condition evaluates false in Buildkite. If no directly runnable workflow declares the event, upload succeeds with a skipped-only pipeline.

### Reusable workflows

**🟡 Supported subset.** The caller and called workflow must be in the same repository and commit.

**✅ Supported:**

- Local `./.github/workflows/...` paths.
- `boolean`, `number`, and `string` inputs.
- Static input values. Caller values may use graph-time `github`, `vars`, matrix, and parent reusable-workflow inputs with the supported operators and pure functions.
- Literal defaults and expression defaults over graph-time `github` and `vars` values.
- Nested calls up to four levels.
- Caller-visible aggregate results.
- Outputs mapped directly from `jobs.<job>.outputs.<name>`.
- Call-level `if` over caller `github`, `vars`, `inputs`, direct `needs`, and status functions.

**❌ Unsupported:**

- Remote or dynamic workflow paths.
- `secrets: inherit`, explicit secret mappings, or required called-workflow secrets.
- `needs`-dependent inputs or dynamic matrices.
- Input defaults that reference `inputs`.
- Literal or compound output expressions.
- Top-level concurrency in the called workflow.

Job-level `uses`, `with`, and `secrets` follow these boundaries.

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

The caller passes a static input:

```yaml
jobs:
  build:
    uses: ./.github/workflows/build.yml
    with:
      target: production
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

Jobs expanded from local reusable workflows use the top-level requesting workflow's repository permissions for `GITHUB_TOKEN`. Only this immutable top-level map is enforced server-side; permission maps in called workflows do not narrow `GITHUB_TOKEN`. The separate `id-token` permission retains called-workflow narrowing. A warning identifies when a job receives different repository permissions than its job or called-workflow map requests.

The `read-all` and `write-all` values and noncanonical names are unsupported. An empty top-level map, or a top-level map containing only `none`, creates no token.

### Environment and defaults

| Key | Status | Behavior |
| --- | --- | --- |
| `env` | 🟡 Supported subset | Workflow, job, and step maps use normal precedence; the most specific value wins. Individual values may use supported interpolation. An entire map cannot be expression-valued. |
| `defaults.run.shell` | 🟡 Supported subset | Supported at workflow and job level. Only `bash` and `sh` are supported. Host jobs default to `bash`; job containers default to `sh`. |
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
| `runs-on` | 🟡 Supported subset | The hosted preset accepts `ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, and `macos-latest`. Labels are case-insensitive. Organization runner profiles can also map `macos-14` and `macos-15`. Static expressions may resolve to an accepted label or list whose labels map to the same complete queue, platform, and image target. |
| `if` | 🟡 Supported subset | Runs before the job starts. See [Conditions](#conditions). |
| `outputs` | 🟡 Supported subset | Maps step outputs for consumption through `needs`. A job may publish 64 outputs of up to 1 KiB each. Ambiguous matrix output values fail closed. |
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
with that image when unmapped. `macos-latest` targets the hosted `macos-medium`
queue.
Version-specific `macos-14` and `macos-15` labels require an organization
runner profile with a native queue and are not admitted by `validate --profile
hosted`. macOS labels reject images. They select Darwin/arm64, not a GitHub
image or Xcode inventory. Linux ARM, macOS x86-64, Windows, and other labels
are unsupported.

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

**🟡 Supported subset.** Commands run in `bash` or `sh` within the Linux or macOS workspace. PowerShell, Python as a shell, Windows shells, and custom shell templates are unsupported. Working directories cannot escape the workspace.

A shell step can specify its shell and workspace-relative working directory:

```yaml
- name: Test
  shell: bash
  working-directory: ./src
  run: go test ./...
```

A `uses` step may call a supported local or public action. Action inputs under `with` may use supported direct interpolation. `docker://` actions and action `entrypoint` or `args` overrides are rejected.

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

Job and step `if` conditions support literals and the syntax listed above. Values use GitHub's truthiness, loose numeric coercion, case-insensitive string comparison, and operand-returning `&&` and `||` semantics. String functions convert primitive arguments; `contains()` also searches arrays. `case()` takes 3–255 odd-numbered arguments, requires Boolean predicates, and evaluates values lazily through the first match. Missing properties in an available `github` or matrix context evaluate to null; unavailable contexts still fail closed. Other functions are unsupported. `hashFiles()` accepts 1–255 literal or direct-reference arguments in step and JavaScript action lifecycle conditions.

Conditions support computed object indexes, numeric array indexes, whole `matrix`, `needs`, and step-scoped `steps` objects, and `.*` projections. Missing and out-of-range indexes evaluate to null. Projections omit missing children; a later wildcard flattens one collection level. The equivalent `[*]` spelling is unsupported by the current expression parser. Whole or dynamic `github` access, whole `inputs`, and the `strategy` context remain unsupported.

| Context | Job `if` | Step `if` |
| --- | --- | --- |
| `github.actor`, `github.base_ref`, `github.event_name`, `github.head_ref`, `github.ref`, `github.ref_name`, `github.ref_type`, `github.repository`, `github.repository_owner`, `github.sha` | ✅ Yes | ✅ Yes |
| `runner.os`, `runner.arch` | ✅ Yes | ✅ Yes |
| `needs.<job>.result`, `needs.<job>.outputs.<name>` | ✅ Yes | ✅ Yes |
| `vars.<name>`, `matrix.<name>` | ✅ Yes | ✅ Yes |
| `inputs.<name>` and computed input indexes | ✅ Yes | ✅ Yes |
| `steps.<id>.outcome`, `steps.<id>.conclusion`, `steps.<id>.outputs.<name>` | ❌ No | ✅ Yes |
| `env.<name>` | ❌ No | ✅ Yes |
| `job.services.<service>.ports[<port>]` | ❌ No | ✅ Yes |
| `github.event.*`, including `github.event.pull_request.*` | 🟡 Compile time only | 🟡 Compile time only |
| `secrets` and other contexts | ❌ No | ❌ No |

An event-backed condition is reduced from the immutable event snapshot before runtime validation. Resolvable `github.event` subtrees become literals; supported runtime-dependent subtrees remain for job or step evaluation. Every branch is validated before reduction, so short-circuiting cannot hide an unsupported function, context, or concrete matrix type error. A residual condition cannot carry `github.event` into the runtime.

Reusable-workflow call conditions use the same operators and status functions but only the caller contexts listed in [Reusable workflows](#reusable-workflows). The runtime evaluates their ordered guards before the called job's own condition.

### Runtime interpolation

Workflow step `run`, `env`, `with`, `name`, explicit `shell`, explicit `working-directory`, `continue-on-error`, and `timeout-minutes` fields support the operators and pure functions listed above. They also support computed indexes and projections over available `matrix`, `vars`, `inputs`, `env`, and `runner` values. Computed, whole, and projected `steps` and `needs` access remains unsupported so unavailable background outputs fail closed.

Job-level expressions support the same operators and pure functions with these field-specific contexts:

| Field | Contexts |
| --- | --- |
| `env` | `github`, `needs`, `matrix`, `vars`, `secrets`, `inputs` |
| `defaults.run` | `github`, `needs`, `matrix`, `env`, `vars`, `inputs` |
| `outputs` | `github`, `needs`, `matrix`, `runner`, `env`, `vars`, `secrets`, `steps`, `inputs` |

These workflow step fields support `hashFiles()`; job-level fields do not. General runtime interpolation and action metadata keep the direct-reference-only rule. The GitHub-authorized `strategy` context, and `job` in job outputs, remain unsupported because the runtime does not carry equivalent context values. Computed, whole, and projected `steps` and `needs` access also remains unsupported in job-level fields.

Expression-valued `continue-on-error` must produce a Boolean. Expression-valued `timeout-minutes` must produce a number greater than 0 and at most 360.

Direct `github.token` references are step-only. Whole, filtered, or dynamically indexed `github` access fails closed because the compiler cannot prove token authority.

The only runner references are `runner.os` and `runner.arch`. They resolve to
`Linux`/`X64` or `macOS`/`ARM64`. Other runner fields and compile-time positions
that require runner identity are unsupported. Action metadata input defaults
may also use direct `runner.debug`, which resolves to the string `false` because
Buildkite has no equivalent step-debug mode.

A runtime interpolation can read a verified upstream output directly:

```yaml
run: echo "${{ needs.build.outputs.image }}"
```

At runtime, only `github.actor`, `github.base_ref`, `github.event_name`, `github.head_ref`, `github.ref`, `github.ref_name`, `github.ref_type`, `github.repository`, `github.repository_owner`, `github.server_url`, and `github.sha` are retained. `github.head_ref` and `github.base_ref` are the pull request source and target branches for `pull_request` and `pull_request_target` events, and empty strings for other events. `github.ref_name` removes the `refs/heads/`, `refs/tags/`, or `refs/pull/` prefix; pull request refs use `<number>/merge` or `<number>/head`. `github.ref_type` is `branch` for branch and pull request refs, and `tag` for tag refs. `github.repository_owner` is derived from `github.repository`. `github.server_url` identifies the event repository provider. `github.event` is unavailable.

`hashFiles()` evaluates when its step field is consumed. A step condition and normal step execution observe earlier steps such as checkout. A JavaScript action's `with` and `env` values can also be evaluated for its `pre` phase, then reevaluated for `main`. Patterns apply in argument order. `!` excludes matches, and a later positive pattern can include them again. Directory matches include descendants, hidden files match normally, overlapping patterns hash each path once, and matching is case-insensitive on Windows only. An empty match returns an empty string.

For each matched file, `hashFiles()` calculates SHA-256 over its contents. It calculates the final lowercase digest over the concatenated binary file digests. Files use deterministic lexical path order; GitHub Runner's current glob traversal order is unspecified, so a multi-file digest can differ when that traversal is not lexical.

Patterns cannot be absolute, contain a `..` path segment, or contain ASCII control characters. Hashing pins the workspace directory and confines file opens to it. It does not traverse symlinked directories. A matched symlink, including one targeting another workspace file, or matched non-regular file fails the step. GitHub Runner can hash a matched file symlink and supports an optional symlink-following mode; this runtime deliberately does neither.

### Compile-time expressions

Matrices, runner labels, names, concurrency groups, and event-backed conditions may use statically known `github`, `event`, `vars`, and matrix values. Compile-time `github` values include `github.actor`, `github.base_ref`, `github.event_name`, `github.head_ref`, `github.ref`, `github.ref_name`, `github.ref_type`, `github.repository`, `github.repository_owner`, `github.sha`, and `github.workflow`. They support the compile-time syntax listed above, computed indexes, numeric array indexes, and `.*` projections where the complete expression resolves during compilation. Whole or dynamic `github` access and whole-event serialization remain unsupported. Event-backed conditions may also combine reducible event subtrees with supported runtime condition values such as `needs` and status functions.

## Actions

### Action sources and runtimes

| Action type | Status | Boundary |
| --- | --- | --- |
| Local `./...` action | 🟡 Supported subset | Source tree is digest-locked and reverified. |
| Public `owner/repo[/path]@ref` action | 🟡 Supported subset | Resolved to an exact commit and digest. |
| Private action | ❌ Unsupported | No private action source access. |
| JavaScript action | ✅ Supported | Declares `node16`, `node20`, or `node24`. |
| Composite action | 🟡 Supported subset | Nested shell steps and locked local or public actions; `bash` or `sh` for `run`. |
| Dockerfile action | 🟡 Supported subset | Verified local or public Dockerfile action on Linux. Rejected on macOS, including through a composite action. |
| `docker://` action | ❌ Unsupported | Rejected during validation. |
| Top-level action metadata `env` | ➖ Accepted, no effect | Any valid YAML value is discarded. It is not evaluated, injected, retained in plans, or used to request secrets or tokens. |

Mutable public refs are resolved during upload, then locked to a commit. The importer lazily requests one Buildkite action-source token and reuses it across all workflow roots and nested composite actions. This token authenticates only public metadata requests for repositories other than the credential repository; the credential repository and codeload requests remain anonymous. If token issuance is unavailable during rollout, resolution safely falls back to anonymous GitHub API access. Exact lowercase commit SHAs need no GitHub API lookup. Complete source trees are verified again at runtime.

Nested calls from a repository-local composite must be local. Public composites may call local children or other public actions; every child is resolved and locked. Dockerfile actions cannot request credentials, volumes, arbitrary options, or privileged mode.

Action metadata parsing remains strict for every other unknown top-level field and for unknown nested fields. The inert top-level `env` exception does not replace workflow or action-step environments, populate `runs.env`, or add `GITHUB_TOKEN` authority.

| Action declaration | Runtime |
| --- | --- |
| `node16` | Managed Node 16.20.2, with one end-of-job deprecation warning. |
| `node20` | Managed Node 24.18.0. |
| `node24` | Managed Node 24.18.0. |

Pre, main, and post phases; inputs; outputs; state; and LIFO post ordering are supported. Other Node declarations are rejected.

JavaScript action `pre-if` and `post-if` metadata uses the condition operators, status functions, pure functions, and `hashFiles()` described in [Conditions](#conditions). Lifecycle conditions can read direct properties from workflow `inputs`, `env`, `github`, `job.services`, `matrix`, `runner`, and `steps`. Other contexts and dynamic or whole-context access fail closed. An empty lifecycle condition always runs and does not receive an implicit `success()` guard.

Pre conditions use the status and action-scoped environment available when preparation reaches the action. Post conditions run during job teardown and use the final job status and environment, including `GITHUB_ENV` changes from main. Root action posts also see final workflow step state. Nested composite actions retain their isolated step context. Cancellation remains distinct from failure, and posts keep LIFO order.

### Checkout action

**🟡 Supported subset.** The final v3.7.0 release commit is admitted exactly. Resolved commits in the v4-and-later range of the static [`actions/checkout` upstream `main` snapshot](https://github.com/actions/checkout/tree/f548e57e544e1ff5a4c46bf1e1b8685f8e4a348a) are also admitted. The following known releases remain admitted even when their commits aren't reachable from that snapshot:

| Release | Commit |
| --- | --- |
| v3.7.0 | [`a37ce9120846195fa4ece8f58b268e6043cb2f26`](https://github.com/actions/checkout/tree/a37ce9120846195fa4ece8f58b268e6043cb2f26) |
| v4 | [`11d5960a326750d5838078e36cf38b85af677262`](https://github.com/actions/checkout/tree/11d5960a326750d5838078e36cf38b85af677262) |
| v5 | [`fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09`](https://github.com/actions/checkout/tree/fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09) |
| v6 | [`d23441a48e516b6c34aea4fa41551a30e30af803`](https://github.com/actions/checkout/tree/d23441a48e516b6c34aea4fa41551a30e30af803) |
| v7.0.0 corpus pin | [`9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0`](https://github.com/actions/checkout/tree/9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0) |
| v7.0.1 | [`3d3c42e5aac5ba805825da76410c181273ba90b1`](https://github.com/actions/checkout/tree/3d3c42e5aac5ba805825da76410c181273ba90b1) |

Mutable refs work only while they resolve to the upstream `main` snapshot or a known release above. Every admitted commit uses the native adapter; the upstream JavaScript doesn't run. v1, v2, pre-v3.7.0 commits, and unknown commits are unsupported. Maintainers can update the v4-and-later snapshot with `go generate ./internal/action/integration`; this doesn't widen v3 admission.

The adapter checks out a detached commit or static branch from the event repository at the workspace root or a clean top-level directory. It uses Buildkite repository-provider Git credentials when the job provides them; otherwise, it fetches anonymously. Credentials are scoped to each fetch command and verified submodule fetch command and are never persisted.

| Input | Supported values |
| --- | --- |
| `repository` | Omitted, or the event `owner/repo`. |
| `ref` | Omitted, empty, a lowercase 40-hex commit, or a static branch in the event repository. A direct `github.sha` or `needs.<job>.outputs.<name>` expression must resolve at runtime to the exact event SHA. |
| `token` | Omitted only. |
| `ssh-key`, `ssh-known-hosts` | Omitted or empty. |
| `ssh-strict` | Omitted or `true`. |
| `ssh-user` | v4 and later: omitted or `git`. v3.7.0: omitted. |
| `persist-credentials` | Omitted or `false`. |
| `path` | Omitted, empty, or one clean non-`.git` top-level workspace directory. |
| `clean` | Omitted or `true`; the root workspace or selected path must be empty or absent. |
| `filter` | v4 and later: omitted or empty. v3.7.0: omitted. |
| `sparse-checkout` | Omitted or empty. |
| `sparse-checkout-cone-mode` | Omitted or `true`. |
| `fetch-depth` | Omitted or a nonnegative integer; `0` fetches full history. |
| `fetch-tags` | Omitted, `true`, or `false`. |
| `show-progress` | v4 and later: omitted, `true`, or `false`. v3.7.0: omitted. |
| `lfs` | Omitted or `false`. |
| `submodules` | Omitted, `false`, `true`, or `recursive`; whitespace is trimmed and casing is ignored. |
| `set-safe-directory` | Omitted or `true`. |
| `github-server-url` | Omitted, empty, or `https://github.com`. |
| `allow-unsafe-pr-checkout` | Omitted or `false`. |

The `ref` and `commit` outputs are unavailable for v3.7.0. Upstream added them in v4.2.0.

The `false` value and omission do not run submodule commands. The `true` value runs native Git for direct children, and `recursive` includes nested children. Relative URLs and `fetch-depth` follow native Git behavior. Public and private GitHub submodules are supported under the job's repository access; external HTTPS submodules are anonymous. `git@github.com:` URLs are rewritten to HTTPS. Other SSH and non-HTTPS transports are unsupported.

See the [security model](security.md#checkout-and-submodules) for credential, Git, and job-isolation boundaries.

Alternate repositories, tags, non-event dynamic commits, LFS, sparse checkout, GitHub Enterprise Server, and credential persistence remain unsupported. Commit and branch checkouts remain detached and confined to the event repository.

### Upload artifact action

**🟡 Supported subset.** These root `actions/upload-artifact` actions use a native Buildkite ZIP adapter:

| Release | Commit |
| --- | --- |
| v4.6.2 | [`ea165f8d65b6e75b540449e92b4886f43607fa02`](https://github.com/actions/upload-artifact/tree/ea165f8d65b6e75b540449e92b4886f43607fa02) |
| v5.0.0 | [`330a01c490aca151604b8cf639adc76d48f6c5d4`](https://github.com/actions/upload-artifact/tree/330a01c490aca151604b8cf639adc76d48f6c5d4) |
| v6.0.0 | [`b7c566a772e6b6bfb58ed0dc250532a479d7789f`](https://github.com/actions/upload-artifact/tree/b7c566a772e6b6bfb58ed0dc250532a479d7789f) |
| v7.0.1 | [`043fb46d1a93c77aae656e7c1c64a875d1fc6a0a`](https://github.com/actions/upload-artifact/tree/043fb46d1a93c77aae656e7c1c64a875d1fc6a0a) |

| Input | Supported values |
| --- | --- |
| `name` | ✅ Supported; defaults to `artifact`. |
| `path` | Required; literal files or directories, or bounded `*`, `?`, character-class, and `**` file globs. |
| `if-no-files-found` | `warn`, `error`, or `ignore`. |
| `retention-days` | Nonnegative integer; advisory only. |
| `compression-level` | `0` through `9`. |
| `overwrite` | Omitted or `false`. |
| `include-hidden-files` | ✅ Supported. |
| `archive` | v7.0.1 only; omitted or `true`. |

Unsupported path forms include exclusions, symlinks, absolute paths, traversal, braces, extglobs, leading glob comments, and special files. At most 32 path roots may be selected. Hidden path segments remain excluded unless explicitly enabled.

An artifact may contain at most 10,000 files and 1 GiB of source data. The ZIP must also be no larger than 1 GiB. A job may publish 64 artifacts.

The adapter sets `artifact-id` and `artifact-digest`. The `artifact-url` output is empty because no GitHub run-scoped URL exists. Merge, raw upload, overwrite, and effective retention control are unsupported.

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

Hosted runtime proof covers v6.1.0 and the cross-generation v3.4.0 producer to v6.1.0 consumer lifecycle. [Build 1173](https://buildkite.com/buildkite/buildkite-gha/builds/1173) saved a build-unique v3 archive to Buildkite Results, restored it with v6, and verified the payload. The hosted profile validates resolution, compilation, and admission for every listed commit but does not execute the actions.

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

**🟡 Supported subset.** A job requests one short-lived `GITHUB_TOKEN` for the exact event repository by statically referencing `secrets.GITHUB_TOKEN` or `github.token`, or by using an action whose effective input default can reach `github.token` for the event provider. A `github.server_url == 'https://github.com'` guard skips the token branch for an Origin event repository. Native action adapters ignore upstream input defaults, so `actions/checkout` alone does not request a token. The top-level requesting workflow's `permissions` determine the token scope. The Buildkite organization feature and the pipeline's workflow access token setting must be enabled. Both are disabled by default.

Buildkite reads the top-level requesting workflow's policy from the pipeline repository at the build's immutable commit. The workflow must be directly under `.github/workflows/` and use a simple `.yml` or `.yaml` filename. Job-level repository permission maps do not change the token scope. Compilation emits `W_JOB_GITHUB_TOKEN_USES_WORKFLOW_PERMISSIONS` when the applied top-level permissions differ. The workflow-token endpoint interprets omitted top-level permissions as exactly `contents: read`, without consulting GitHub repository or organization defaults. Write access requires an explicit, non-empty top-level map. An explicit empty map or scopes resolving only to `none` produce no token.

Eligible direct and expanded jobs can receive a token when the selected workflow contains local reusable-workflow jobs. Every expanded job receives the top-level requesting workflow's repository permissions for `GITHUB_TOKEN`. Only this immutable top-level map is enforced server-side. Buildkite does not inspect permission maps in called workflows for `GITHUB_TOKEN`, so those maps do not narrow it. The separate `id-token` permission retains called-workflow narrowing. Compilation emits `W_REUSABLE_WORKFLOW_TOKEN_USES_ROOT_PERMISSIONS` when an expanded job receives a token that a called-workflow repository policy would have narrowed. Remote and private reusable workflows remain unsupported.

Pull-request builds and their triggered or rebuilt descendants may request only `contents: read`. Merge-queue builds and their descendants cannot request a token. The endpoint does not support GitHub Enterprise Server. The backend verifies this provenance and remains authoritative.

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

The token is not added to the initial job environment. Direct workflow-authored `github.token` references are available during step execution and use the same scoped token as `secrets.GITHUB_TOKEN`. Effective action metadata input defaults can also use `github.token`. Automatic ambient `GITHUB_TOKEN` is unsupported.

### Other secrets and OIDC

**🟡 Supported subset.** Direct jobs can use statically named `${{ secrets.NAME }}` references. The compiler records names only in job plans. At runtime, the destination job runs `buildkite-agent secret get NAME` with its existing authenticated Agent session and registers each value with both the Buildkite Agent redactor and the local workflow-command redactor before use. Missing or denied secrets fail the job without printing the value or Agent error output. Secret values do not appear in plans or generated pipeline YAML.

These are Buildkite secrets available to the destination job, not GitHub repository, environment, event, or fork-scoped secrets. Buildkite Secret access policies are the authorization boundary. A workflow can access any named secret that its destination job's Buildkite identity and secret policy permit, just as arbitrary code in that job can run `buildkite-agent secret get`.

`GITHUB_TOKEN` always uses the separate scoped workflow-token contract described above and cannot be replaced by an ordinary Buildkite secret. Dynamic names, secret use in conditions or other compile-time expressions, GitHub environments and environment secrets, `secrets: inherit`, and reusable-workflow secret forwarding are unsupported. Action metadata defaults cannot add a secret to the plan; such defaults fail compilation rather than becoming an authority source. A secret referenced only by a declared optional action input does not add a job secret requirement and resolves to an empty value unless the same secret is required elsewhere.

Jobs with `id-token: write` expose the GitHub Actions `getIDToken()` wire contract to host JavaScript actions, including JavaScript actions called by composite actions. The endpoint mints a Buildkite OIDC token for the requested audience. Cloud identity providers must trust Buildkite's issuer and claims, not GitHub's. `id-token: read`, `id-token: none`, and omitted permission maps do not expose the endpoint. Repository tests verify the wire contract with a shim that mirrors `actions/toolkit`'s `oidc-utils.ts`; the hosted runtime proof remains pending.

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

`claims` and `aws-session-tags` accept non-empty claim-name lists.
`subject-claim` accepts one non-empty immutable claim name. The Agent API owns
the available claim vocabulary and rejects unsupported names when a job mints
a token. Plugin OIDC configuration applies to jobs without granting
`id-token: write`; those jobs still receive no endpoint.

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
