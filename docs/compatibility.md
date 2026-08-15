# GitHub Actions compatibility

<!-- (internal) If this file is ever moved, please update the GitHub Actions template -->

This page defines the initial production contract for `buildkite-gha`. It applies to the `hosted` profile used by `upload` and the Buildkite plugin.

The released plugin path supports Linux x86-64 and native macOS arm64 importers
and generated jobs, including the matching `runner.os` and `runner.arch` values.
Importer agent targeting is independent of generated-job runner mappings.
Platform labels do not provide GitHub image or toolchain parity.

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
| [Platforms](#job-configuration) | 🟡 Supported subset | Linux x86-64 with `ubuntu-latest`, `ubuntu-24.04`, or `ubuntu-22.04`; native macOS arm64 with `macos-latest`, `macos-14`, or `macos-15`. Labels do not provide GitHub image, toolchain, or Xcode parity. |
| [Jobs and dependencies](#job-configuration) | ✅ Supported | Static dependencies, matrix fan-out and fan-in, results, and bounded outputs. |
| [Matrix strategies](#matrix-strategies) | 🟡 Supported subset | Static matrices, `include`, `exclude`, and literal `max-parallel`. Maximum 256 instances per job. `fail-fast` has no effect. |
| [Shell steps](#commands-and-actions) | 🟡 Supported subset | Linux and macOS `bash` and `sh`. |
| [Conditions and expressions](#expressions-and-contexts) | 🟡 Supported subset | Boolean and equality conditions and direct references to selected contexts. |
| [Reusable workflows](#reusable-workflows) | 🟡 Supported subset | Local workflows with static inputs and direct job-output mappings. Secret forwarding is unsupported. |
| [Actions](#actions) | 🟡 Supported subset | Local and public JavaScript and composite actions on Linux and macOS; verified Dockerfile actions on Linux only. |
| [Checkout, artifacts, and cache](#actions) | 🟡 Supported subset | Only the audited versions and modes listed below. |
| [`GITHUB_TOKEN`](#github-token) | 🟡 Supported subset | One job-bound token for the event repository. Workflows containing reusable-workflow jobs cannot receive one. |
| [Other workflow secrets](#other-secrets-and-oidc) | 🟡 Supported subset | Static names in direct jobs resolve through the destination job's Buildkite secret authority. |
| [Job and service containers](#containers-and-services) | 🚧 Not available in production | A bounded container subset exists, but production upload rejects it. |
| [Environments and snapshots](#job-configuration) | 🟡 Supported subset | Environments are rejected. Snapshots are accepted with no effect. |
| [OIDC](#other-secrets-and-oidc) | ❌ Unsupported | GitHub-compatible OIDC is outside the initial release. |
| [Other platforms](#job-configuration) and [providers](#repositories) | ❌ Unsupported | Windows, Linux arm64, macOS x86-64, GitHub Enterprise Server, and unlisted providers are outside the initial release. |
| [Other GitHub services](#github-services) | ❌ Unsupported | No general emulation for Releases, Packages, Checks, deployments, or GitHub artifact APIs. |

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

An explicit event snapshot never consults contradictory live Buildkite event fields. The fallback can classify `trigger_job` as `push` even when `build.source_event` is absent. The selected snapshot is then used consistently for applicability, event-dependent validation and compilation, the group condition, and the event suffix in its provider check.

| Event | Supported trigger behavior |
| --- | --- |
| `push` | `branches`, `branches-ignore`, `tags`, and `tags-ignore`, including ordered negative patterns in an include list. Branch and tag filters select their corresponding ref kind. |
| `pull_request` | `branches` and `branches-ignore` match the base branch. Omitted `types` defaults to `opened`, `synchronize`, and `reopened`; explicitly listed activity types must map exactly to a supported Buildkite source action. |
| `workflow_dispatch` | Selected for Buildkite UI and API builds. Webhook-style branch, tag, type, and workflow filters are unsupported. |
| `schedule` | Selected for Buildkite scheduled builds. Buildkite owns cron configuration and does not expose which schedule started a build, so every `on.schedule` workflow is eligible for every Buildkite scheduled build. |
| `workflow_call` | Defines a local reusable-workflow interface. A reusable-only file is available to callers but does not become a top-level group. |

Supported `pull_request` activity types are `assigned`, `unassigned`, `labeled`, `unlabeled`, `opened`, `edited`, `closed`, `reopened`, `synchronize`, `converted_to_draft`, `locked`, `unlocked`, `enqueued`, `dequeued`, `milestoned`, `demilestoned`, `ready_for_review`, `review_requested`, `review_request_removed`, `auto_merge_enabled`, and `auto_merge_disabled`.

`paths` and `paths-ignore` are unsupported because Buildkite `if_changed` has different semantics. Unsupported trigger events, path filters, push type/workflow filters, pull request tag/workflow filters, inexact activity types, and invalid include/ignore combinations are not approximated; they replace the affected workflow with a failing step. Trigger shapes are validated even on a different event, but only the selected event contributes a group condition.

A top-level workflow that does not declare the effective event is excluded before event-dependent validation or compilation and represented by one top-level skipped command step. A workflow that declares that event remains represented by a group even when a same-event branch, tag, base-branch, or action condition evaluates false in Buildkite. If no directly runnable workflow declares the event, upload succeeds with a skipped-only pipeline.

### Reusable workflows

**🟡 Supported subset.** The caller and called workflow must be in the same repository and commit.

**✅ Supported:**

- Local `./.github/workflows/...` paths.
- `boolean`, `number`, and `string` inputs.
- Static input values and defaults.
- Nested calls up to four levels.
- Caller-visible aggregate results.
- Outputs mapped directly from `jobs.<job>.outputs.<name>`.

**❌ Unsupported:**

- Remote or dynamic workflow paths.
- Call-level `if`.
- Token requests from any direct or expanded job in a workflow containing a reusable-workflow call.
- `secrets: inherit`, explicit secret mappings, or required called-workflow secrets.
- Dynamic inputs or matrices.
- Literal or compound output expressions.
- Top-level concurrency in the called workflow.

Job-level `uses`, `with`, and `secrets` follow these boundaries.

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

**🟡 Supported subset.** Permissions matter only when a job statically references `secrets.GITHUB_TOKEN`, or an effective action input default can reach `github.token` for the event provider.

A workflow-level permissions map can request repository access:

```yaml
permissions:
  contents: read
  pull-requests: write
```

Supported values are `read`, `write`, and `none`. Supported names are `actions`, `artifact-metadata`, `attestations`, `checks`, `contents`, `deployments`, `discussions`, `issues`, `packages`, `pages`, `pull-requests`, `security-events`, and `statuses`.

An omitted map defaults to exactly `contents: read` when a token is needed. This deterministic default does not inherit GitHub repository or organization settings. A job map replaces the workflow map; it does not merge with it. A called workflow may only narrow its caller's permissions. These forms remain compilable when no job needs a token. Hosted token issuance accepts the omitted default or an explicit, non-empty top-level map. It rejects job-level maps and reusable-workflow jobs. Write access therefore requires an explicit top-level map.

The `read-all` and `write-all` values, the `id-token` permission, and noncanonical names are unsupported. An empty map, or a map containing only `none`, creates no token.

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

**🟡 Supported subset with different queue behavior.** A static group becomes a repository-scoped, case-insensitive Buildkite concurrency group. Groups may use `vars`, supported `github` fields, static reusable-workflow inputs, and concrete matrix values at job level. Boolean and equality operators, `fromJSON`, and case-insensitive `startsWith` are supported when the whole expression resolves during compilation. Runtime `needs` and `strategy` values remain unsupported.

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
| `runs-on` | 🟡 Supported subset | Accepts `ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, `macos-latest`, `macos-15`, and `macos-14`. Static expressions may resolve to an accepted label or list whose labels map to the same complete queue, platform, and image target. |
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

Runner labels do not select GitHub images. Linux labels default to the
corresponding Noble or Jammy hosted-toolchains image; an explicit immutable
image overrides it for a configured profile. Unmapped Linux labels use default
Buildkite agent targeting with that image. Through the upload path,
unmapped `macos-latest` targets the hosted `macos-medium` queue; `macos-14`
and `macos-15` require a runner profile with a native queue. macOS labels
reject images. They select Darwin/arm64, not a GitHub image or Xcode inventory.

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

**🚧 Not available in production.** The compiler and runtime support a bounded Linux subset for job `container` and `services`, but the `hosted` profile rejects it before upload.

The underlying subset accepts literal public image names, environment maps, and ports. Credentials, volumes, options, private images, dynamic values, and privileged containers are unsupported.

macOS jobs reject containers, services, Dockerfile actions, and Docker capability.

## Step syntax

### Step configuration

| Key | Status | Behavior |
| --- | --- | --- |
| `name`, `id` | ✅ Supported | Use `id` to read outputs or target background work. IDs must be unique within a job. |
| `if` | 🟡 Supported subset | May use step status, step outputs, `env`, and service ports in addition to job-condition contexts. |
| `env` | 🟡 Supported subset | Values override job and workflow values and may use supported direct interpolation. |
| `continue-on-error` | ✅ Supported | A failure records `outcome: failure` and `conclusion: success`, then the job continues. |
| `timeout-minutes` | 🟡 Supported subset | Accepts literal timeouts up to 360 minutes. Expressions are rejected. |

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
| `!`, `&&`, `\|\|`, `==`, `!=` | ✅ Supported | ❌ Unsupported | 🟡 When the result resolves fully |
| `always()`, `success()`, `failure()`, `cancelled()` | ✅ Without arguments | ❌ Unsupported | ❌ Unsupported |
| `fromJSON()`, case-insensitive `startsWith()` | 🟡 Compile time only | ❌ Unsupported | 🟡 When the result resolves fully |
| `hashFiles()` | 🟡 Step `if` only | 🟡 Workflow steps only | ❌ Unsupported |

### Conditions

Job and step `if` conditions support literals and the syntax listed above. Ordered comparisons and other functions are unsupported. `hashFiles()` accepts 1–255 literal or direct-reference arguments in step conditions only.

| Context | Job `if` | Step `if` |
| --- | --- | --- |
| `github.actor`, `github.event_name`, `github.head_ref`, `github.ref`, `github.repository`, `github.sha` | ✅ Yes | ✅ Yes |
| `github.ref_name` | ❌ No | ❌ No |
| `runner.os`, `runner.arch` | ✅ Yes | ✅ Yes |
| `needs.<job>.result`, `needs.<job>.outputs.<name>` | ✅ Yes | ✅ Yes |
| `vars.<name>`, `matrix.<name>` | ✅ Yes | ✅ Yes |
| `steps.<id>.outcome`, `steps.<id>.conclusion`, `steps.<id>.outputs.<name>` | ❌ No | ✅ Yes |
| `env.<name>` | ❌ No | ✅ Yes |
| `job.services.<service>.ports[<port>]` | ❌ No | ✅ Yes |
| `github.event.*`, including `github.event.pull_request.*` | 🟡 Compile time only | 🟡 Compile time only |
| `secrets` and other contexts | ❌ No | ❌ No |

An event-backed condition is evaluated from the immutable event snapshot before runtime validation. Every branch is validated before evaluation, so short-circuiting cannot hide an unsupported function, context, or concrete matrix type error. A condition that cannot be fully resolved at compile time cannot carry `github.event` into the runtime.

### Runtime interpolation

Interpolated values support direct references only. Available contexts include `github`, `runner`, `inputs`, `matrix`, `vars`, `env`, `steps`, `needs`, `secrets`, and service ports where that value exists. Top-level workflow step `run`, `env`, `with`, explicit `shell`, and explicit `working-directory` fields also support `hashFiles()` with literal or direct-reference arguments. Job fields, job outputs, job defaults, step names, and action metadata keep the direct-reference-only rule.

The only runner references are `runner.os` and `runner.arch`. They resolve to
`Linux`/`X64` or `macOS`/`ARM64`. Runtime interpolation does not evaluate
operators or functions; use the supported condition syntax in job or step
`if`. Other runner fields and compile-time positions that require runner
identity are unsupported.

A runtime interpolation can read a verified upstream output directly:

```yaml
run: echo "${{ needs.build.outputs.image }}"
```

At runtime, only `github.actor`, `github.event_name`, `github.head_ref`, `github.ref`, `github.repository`, `github.server_url`, and `github.sha` are retained. `github.head_ref` is the pull request source branch for `pull_request` and `pull_request_target` events, and an empty string for other events. `github.server_url` identifies the event repository provider. `github.event` is unavailable.

`hashFiles()` evaluates when its step field is consumed. A step condition and normal step execution observe earlier steps such as checkout. A JavaScript action's `with` and `env` values can also be evaluated for its `pre` phase, then reevaluated for `main`. Patterns apply in argument order. `!` excludes matches, and a later positive pattern can include them again. Directory matches include descendants, hidden files match normally, overlapping patterns hash each path once, and matching is case-insensitive on Windows only. An empty match returns an empty string.

For each matched file, `hashFiles()` calculates SHA-256 over its contents. It calculates the final lowercase digest over the concatenated binary file digests. Files use deterministic lexical path order; GitHub Runner's current glob traversal order is unspecified, so a multi-file digest can differ when that traversal is not lexical.

Patterns cannot be absolute, contain a `..` path segment, or contain ASCII control characters. Hashing pins the workspace directory and confines file opens to it. It does not traverse symlinked directories. A matched symlink, including one targeting another workspace file, or matched non-regular file fails the step. GitHub Runner can hash a matched file symlink and supports an optional symlink-following mode; this runtime deliberately does neither.

### Compile-time expressions

Matrices, runner labels, names, concurrency groups, and event-backed conditions may use statically known `github`, `event`, `vars`, and matrix values. They support the compile-time syntax listed above where the complete expression resolves during compilation. Values derived from runtime `needs` or `steps` are unsupported.

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

Mutable public refs are resolved during upload, then locked to a commit. The importer lazily requests one Buildkite action-source token and reuses it across all workflow roots and nested composite actions. This token authenticates only public metadata requests for repositories other than the credential repository; the credential repository and codeload requests remain anonymous. If token issuance is unavailable during rollout, resolution safely falls back to anonymous GitHub API access. Exact lowercase commit SHAs need no GitHub API lookup. Complete source trees are verified again at runtime.

Nested calls from a repository-local composite must be local. Public composites may call local children or other public actions; every child is resolved and locked. Dockerfile actions cannot request credentials, volumes, arbitrary options, or privileged mode.

| Action declaration | Runtime |
| --- | --- |
| `node16` | Managed Node 16.20.2, with one end-of-job deprecation warning. |
| `node20` | Managed Node 24.18.0. |
| `node24` | Managed Node 24.18.0. |

Pre, main, and post phases; inputs; outputs; state; and LIFO post ordering are supported. Other Node declarations are rejected.

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

**🟡 Supported subset.** Public runtime support remains limited to `actions/cache` v6.1.0 at [`55cc8345863c7cc4c66a329aec7e433d2d1c52a9`](https://github.com/actions/cache/tree/55cc8345863c7cc4c66a329aec7e433d2d1c52a9). Its root, `restore`, and `save` entry points run the stock Node 24 cache-v2 client against the Buildkite Results service.

The hosted profile also admits the exact v5.0.3 and v5.1.0 commits for runtime-proof collection. They are not part of the public compatibility contract until that hosted proof is complete. Other v5 commits, v4, and unknown v6 commits remain unsupported.

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

**🟡 Supported subset.** A job requests one short-lived `GITHUB_TOKEN` for the exact event repository by statically referencing `secrets.GITHUB_TOKEN` or by using an action whose effective input default can reach `github.token` for the event provider. A `github.server_url == 'https://github.com'` guard skips the token branch for an Origin event repository. Native action adapters ignore upstream input defaults, so `actions/checkout` alone does not request a token. Effective `permissions` determine the token scope. The Buildkite organization feature and the pipeline's workflow access token setting must be enabled. Both are disabled by default.

Buildkite reads the workflow policy from the pipeline repository at the build's immutable commit. The workflow must be directly under `.github/workflows/`, use a simple `.yml` or `.yaml` filename, and contain no job-level permission maps or reusable-workflow jobs. The workflow-token endpoint must interpret omitted top-level permissions as exactly `contents: read`, without consulting GitHub repository or organization defaults. Write access requires an explicit, non-empty top-level map. An explicit empty map or scopes resolving only to `none` produce no token.

If the selected workflow contains a reusable-workflow job, no direct or expanded job can receive a token. Tokenless local reusable workflows remain supported.

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

The token is not added to the initial job environment. The `github.token` value is available only while evaluating an effective action metadata input default. Workflow-authored `github.token` and automatic ambient `GITHUB_TOKEN` are unsupported.

### Other secrets and OIDC

**🟡 Supported subset.** Direct jobs can use statically named `${{ secrets.NAME }}` references. The compiler records names only in job plans. At runtime, the destination job runs `buildkite-agent secret get NAME` with its existing authenticated Agent session and registers each value with both the Buildkite Agent redactor and the local workflow-command redactor before use. Missing or denied secrets fail the job without printing the value or Agent error output. Secret values do not appear in plans or generated pipeline YAML.

These are Buildkite secrets available to the destination job, not GitHub repository, environment, event, or fork-scoped secrets. Buildkite Secret access policies are the authorization boundary. A workflow can access any named secret that its destination job's Buildkite identity and secret policy permit, just as arbitrary code in that job can run `buildkite-agent secret get`.

`GITHUB_TOKEN` always uses the separate scoped workflow-token contract described above and cannot be replaced by an ordinary Buildkite secret. Dynamic names, secret use in conditions or other compile-time expressions, GitHub environments and environment secrets, `secrets: inherit`, and reusable-workflow secret forwarding are unsupported. Action metadata defaults cannot add a secret to the plan; such defaults fail compilation rather than becoming an authority source. A secret referenced only by a declared optional action input does not add a job secret requirement and resolves to an empty value unless the same secret is required elsewhere.

**❌ Unsupported.** GitHub-compatible OIDC and `id-token` are not implemented.

### GitHub services

**❌ Unsupported beyond the integrations listed above.** An action's runtime may still require unsupported GitHub services. Buildkite provides no GitHub Artifact, OIDC, Packages, Releases, Checks, or deployment service emulation beyond the documented integrations.

## Runtime behavior and limits

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

Check syntax and static graph construction without an event:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

Apply the same profile as production upload:

```sh
buildkite-gha validate \
  --profile hosted \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

The results mean:

- **Compilable**: Syntax and the static job graph can be translated.
- **Not applicable**: The workflow does not declare the selected event, so upload would skip it without compiling it.
- **Admitted**: Resolved actions and generated plans pass production policy.
- **Runtime-proven**: Repository tests or hosted evidence have executed the behavior.

Admission does not execute arbitrary action code. An admitted action may still depend on an unsupported GitHub service.

See the [CLI guide](cli.md) for event snapshots, JSON reports, compilation, and direct upload.
