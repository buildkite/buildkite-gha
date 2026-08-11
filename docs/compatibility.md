# GitHub Actions compatibility

This page defines the initial production contract for `buildkite-gha`. It
applies to the `hosted-tokenless` profile used by `upload` and the Buildkite
plugin.

GitHub Actions syntax changes over time. If a feature is not listed here, treat
it as unsupported. GitHub's
[workflow syntax reference](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax)
describes the original syntax; this page describes the subset that runs on
Buildkite.

## Support matrix

| Status | Meaning |
| --- | --- |
| ✅ **Supported** | Available through the production plugin path. |
| 🟡 **Supported subset** | Available within the limits shown here. |
| ➖ **Accepted, no effect** | Validation accepts the syntax, but it does not change the build. |
| 🚧 **Not available in production** | The compiler or runtime supports it, but production upload blocks it. |
| ❌ **Unsupported** | Rejected or outside the compatibility contract. |

### Initial release at a glance

| Area | Status | Initial release boundary |
| --- | --- | --- |
| [Workflow and job names](#workflow-syntax) | 🟡 Supported subset | `name` and job names are retained. `run-name` has no effect. |
| [Triggers and filters under `on:`](#on) | ➖ Accepted, no effect | Buildkite creates and filters builds. Local `workflow_call` is supported for composition. |
| [Platforms and `runs-on`](#job-syntax) | 🟡 Supported subset | Linux x86-64 with `ubuntu-latest`, `ubuntu-24.04`, or `ubuntu-22.04`. This does not provide GitHub image parity. |
| [Jobs and `needs`](#job-syntax) | ✅ Supported | Static dependencies, matrix fan-out and fan-in, results, and bounded outputs. |
| [Matrix strategies](#job-syntax) | 🟡 Supported subset | Static matrices, `include`, `exclude`, and literal `max-parallel`. Maximum 256 instances per job. `fail-fast` has no effect. |
| [Shell steps](#step-syntax) | 🟡 Supported subset | Linux `bash` and `sh`. |
| [Conditions and expressions](#expressions-and-contexts) | 🟡 Supported subset | Boolean and equality conditions plus direct references to selected contexts. |
| [Reusable workflows](#onworkflow_call) | 🟡 Supported subset | Local workflows with static inputs, `secrets: inherit`, and direct job-output mappings. |
| [Actions](#actions) | 🟡 Supported subset | Local and public JavaScript, composite, and verified Dockerfile actions. |
| [Checkout, artifacts, and cache](#actions) | 🟡 Supported subset | Only the audited versions and modes listed below. |
| [`GITHUB_TOKEN`](#github_token) | 🟡 Supported subset | One job-bound token for the event repository, subject to effective permissions and Buildkite policy. |
| [Other workflow secrets](#other-secrets-and-oidc) | 🚧 Not available in production | Production upload rejects ordinary secret requirements. |
| [Job and service containers](#job-syntax) | 🚧 Not available in production | A bounded container subset exists, but production upload rejects it. |
| [Environments and snapshots](#job-syntax) | ➖ Accepted, no effect | No approvals, environment secrets, deployment state, or custom-image creation. |
| [OIDC, Windows, macOS, and Linux arm64](#repositories-credentials-and-github-services) | ❌ Unsupported | Outside the initial release. |
| [Other GitHub services](#github-services) | ❌ Unsupported | No general emulation for Releases, Packages, Checks, deployments, or GitHub artifact APIs. |

## How workflows run on Buildkite

GitHub Actions combines run creation and workload definition in one file.
Buildkite keeps them separate.

| GitHub Actions concept | Buildkite behavior |
| --- | --- |
| Workflow trigger | Buildkite integration, schedule, manual build, or API request |
| Workflow run | Existing Buildkite build |
| Job | Buildkite command job |
| Matrix entry | Buildkite command job |
| `needs` | `depends_on` plus verified result transport |
| Step | Runs inside the job compatibility runtime |

No shadow GitHub Actions run is created. Buildkite owns scheduling, logs,
retries, cancellation, and status.

Steps remain inside one job because they share a workspace, environment files,
action state, and post-action cleanup.

## Workflow syntax

### `name`

**✅ Supported.** The workflow name is available as `github.workflow` and is used
when naming generated work.

```yaml
name: CI
```

### `run-name`

**➖ Accepted, no effect.** Buildkite names the build. `run-name` is not retained.

### `on`

**➖ Accepted, no effect for build creation.** Configure push, pull request,
branch, tag, schedule, and manual triggers in Buildkite.

```yaml
on:
  push:
    branches: [main]
```

The example does not filter Buildkite builds. The plugin derives
`pull_request` for pull request builds and `push` for other builds. Scheduled
and manual Buildkite builds therefore use push semantics. Direct CLI callers
may supply another event name through an [event snapshot](cli.md#event-snapshots).

`on.workflow_call` is the exception: it defines the interface for a local
reusable workflow.

### `on.workflow_call`

**🟡 Supported subset.** The caller and called workflow must be in the same
repository and commit.

**✅ Supported:**

- local `./.github/workflows/...` paths;
- `boolean`, `number`, and `string` inputs;
- static input values and defaults;
- `secrets: inherit` when the called workflow declares no required secrets;
  nested workflows must inherit again at every call edge;
- nested calls, up to four levels;
- caller-visible aggregate results;
- outputs mapped directly from `jobs.<job>.outputs.<name>`.

**❌ Unsupported:**

- remote or dynamic workflow paths;
- call-level `if`;
- explicit secret mappings or required called-workflow secrets;
- dynamic inputs or matrices;
- literal or compound output expressions;
- top-level concurrency in the called workflow.

Called workflow:

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

Caller:

```yaml
jobs:
  build:
    uses: ./.github/workflows/build.yml
    with:
      target: production
```

### `permissions`

**🟡 Supported subset.** Permissions matter only when a job statically references
`secrets.GITHUB_TOKEN`, or an action input default references `github.token`.

```yaml
permissions:
  contents: read
  pull-requests: write
```

Supported permission values are `read`, `write`, and `none`. Supported names
are:

`actions`, `artifact-metadata`, `attestations`, `checks`, `contents`,
`deployments`, `discussions`, `issues`, `models`, `packages`, `pages`,
`pull-requests`, `repository-projects`, `security-events`, and `statuses`.

`models` is read-only. The other names support `read` and `write`.

An omitted map defaults to `contents: read` when a token is needed. A job map
replaces the workflow map; it does not merge with it. A called workflow may
only narrow its caller's permissions.

`read-all`, `write-all`, `id-token`, and non-canonical names are unsupported.
An empty map, or a map containing only `none`, creates no token.

### `env`

**🟡 Supported subset.** Workflow, job, and step maps follow normal precedence:
the most specific value wins.

```yaml
env:
  GOFLAGS: -mod=readonly

jobs:
  test:
    env:
      GOFLAGS: -race
```

Individual values may use supported interpolation. An entire environment map
cannot be expression-valued.

### `defaults.run`

**🟡 Supported subset.** `shell` and workspace-relative `working-directory` are
supported at workflow and job level.

```yaml
defaults:
  run:
    shell: bash
    working-directory: ./src
```

Only `bash` and `sh` are supported. Host jobs default to `bash`; job containers
default to `sh`.

### `concurrency`

**🟡 Supported subset with different queue behavior.** A static group becomes a
repository-scoped, case-insensitive Buildkite concurrency group.

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ startsWith(github.ref, 'refs/pull/') }}

jobs:
  deploy:
    concurrency: deploy-${{ matrix.target }}
```

Groups may use `vars`, supported `github` fields, static reusable-workflow
inputs, and concrete matrix values at job level. Boolean and equality
operators, `fromJSON`, and case-insensitive `startsWith` are supported when the
whole expression resolves during compilation. Runtime `needs` and `strategy`
values remain unsupported.

Buildkite queues every waiting entry. It does not replace GitHub's existing
pending entry. `queue` is unsupported.

Workflow-level literal or expression-resolved `cancel-in-progress: true` emits
a warning and does not cancel. Job-level cancellation remains unsupported.
Buildkite's **Cancel Intermediate Builds** and **Skip Intermediate Builds**
settings can approximate same-branch cancellation.

Cancel the whole Buildkite build rather than one job when a workflow-level
concurrency gate is active.

## Job syntax

### `jobs.<job_id>.name`

**✅ Supported.** Static `github`, `vars`, reusable-workflow `inputs`, and matrix
values may be used in the label.

```yaml
jobs:
  test:
    name: Test Go ${{ matrix.go }}
```

### `jobs.<job_id>.needs`

**✅ Supported.** Dependencies may be a string or list of static job IDs. Matrix
fan-out and fan-in are handled automatically.

```yaml
jobs:
  build:
    # ...
  test:
    needs: build
```

Results and outputs come from verified producer manifests. Retrying one
producer can make selection ambiguous; retry the whole build.

### `jobs.<job_id>.runs-on`

**🟡 Supported subset.** These labels are accepted:

- `ubuntu-latest`
- `ubuntu-24.04`
- `ubuntu-22.04`

```yaml
runs-on: ${{ matrix.os }}
```

Static expressions are allowed when they resolve to an accepted label or list
of labels. Multiple labels must map to the same Buildkite queue.

These are compatibility labels, not image selection. The selected Buildkite
agent must provide the tools used by the workflow. Windows, macOS, and arm64
labels are unsupported.

### `jobs.<job_id>.if`

**🟡 Supported subset.** See [Conditions](#conditions) for operators and contexts.
The condition runs before the job starts.

```yaml
if: github.ref == 'refs/heads/main' && success()
```

### `jobs.<job_id>.strategy`

**🟡 Supported subset.** Static matrices expand into Buildkite jobs.

| Key | Support |
| --- | --- |
| `matrix` | Literal rows or compile-time `github`, `event`, `vars`, and `fromJSON` values |
| `include` / `exclude` | Static combinations |
| `max-parallel` | Literal value |
| `fail-fast` | ➖ Accepted, no effect |

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

A job may expand to at most 256 instances. Matrices derived from `needs` or
`steps` are unsupported. A failed matrix entry does not cancel its siblings.

### `jobs.<job_id>.outputs`

**🟡 Supported subset.** Map job outputs from step outputs, then consume them
through `needs`.

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

A job may publish 64 outputs. Each value is limited to 1 KiB. Ambiguous matrix
output values fail closed.

### `jobs.<job_id>.env` and `defaults.run`

**🟡 Supported subset.** These use the same behavior as workflow-level
[`env`](#env) and [`defaults.run`](#defaultsrun).

### `jobs.<job_id>.timeout-minutes`

**🟡 Supported subset.** Literal job timeouts up to 360 minutes are supported.
Expressions are rejected.

### `jobs.<job_id>.continue-on-error`

**❌ Unsupported.** Validation rejects job-level `continue-on-error`.

Step-level `continue-on-error` is supported.

### `jobs.<job_id>.environment`

**➖ Accepted, no effect.** No deployment record, approval, environment secret,
or protection rule is created.

### `jobs.<job_id>.container` and `services`

**🚧 Not available in production.** The compiler and runtime support a bounded
Linux subset, but the `hosted-tokenless` profile rejects it before upload.

The underlying subset accepts literal public image names, environment maps, and
ports. Credentials, volumes, options, private images, dynamic values, and
privileged containers are unsupported.

### `jobs.<job_id>.uses`, `with`, and `secrets`

**🟡 Supported subset.** See [`on.workflow_call`](#onworkflow_call). Local calls
and static inputs are supported. `secrets: inherit` is accepted when the called
workflow declares no required secrets, and nested workflows must inherit again
at every call edge. Remote calls, explicit secret mappings, required
called-workflow secrets, and call-level conditions are unsupported.

### `jobs.<job_id>.snapshot`

**➖ Accepted, no effect.** Custom image creation is not implemented.

## Step syntax

### `steps[*].name` and `id`

**✅ Supported.** Use `id` to read outputs or target background work. IDs must be
unique within a job.

### `steps[*].if`

**🟡 Supported subset.** Step conditions can use step status, step outputs, `env`,
and service ports in addition to job-condition contexts.

```yaml
- id: test
  run: go test ./...
  continue-on-error: true

- if: steps.test.outcome == 'failure'
  run: ./scripts/report-failure
```

### `steps[*].run`, `shell`, and `working-directory`

**🟡 Supported subset.** Commands run in Linux `bash` or `sh` within the workspace.

```yaml
- name: Test
  shell: bash
  working-directory: ./src
  run: go test ./...
```

PowerShell, Python-as-shell, Windows shells, and custom shell templates are
unsupported. Working directories cannot escape the workspace.

### `steps[*].uses` and `with`

**🟡 Supported subset.** A step may call a supported local or public action.

```yaml
- uses: actions/checkout@v7

- uses: ./.github/actions/build
  with:
    target: production
```

Action inputs may use supported direct interpolation. `docker://` actions and
action `entrypoint` or `args` overrides are rejected.

### `steps[*].env`

**🟡 Supported subset.** Step values override job and workflow values. Individual
values may use supported direct interpolation.

### `steps[*].continue-on-error`

**✅ Supported.** A failing step records `outcome: failure` and
`conclusion: success`, then the job continues.

### `steps[*].timeout-minutes`

**🟡 Supported subset.** Literal timeouts up to 360 minutes are supported.
Expressions are rejected.

### `background`, `wait`, `wait-all`, `cancel`, and `parallel`

**✅ Supported.** At most ten background steps run at once inside a job.

```yaml
steps:
  - id: server
    run: ./scripts/start-server
    background: true

  - run: ./scripts/test

  - cancel: server
```

Use `wait: <id>` for selected steps, `wait-all:` for all active work, or
`parallel:` for a fixed group:

```yaml
steps:
  - parallel:
      - run: ./scripts/lint
      - run: ./scripts/test
```

Outputs, environment changes, and failures become visible at the covering
wait. Remaining work is joined before post-action cleanup. These controls are
not supported inside a composite action.

### Environment files

**✅ Supported.** The runtime implements:

- `GITHUB_OUTPUT`
- `GITHUB_ENV`
- `GITHUB_PATH`
- `GITHUB_STATE`
- `GITHUB_STEP_SUMMARY`

Multiline values are supported. `NODE_OPTIONS` cannot be set through
`GITHUB_ENV`.

### Workflow commands

| Command | Support |
| --- | --- |
| `add-mask`, `stop-commands` | ✅ Supported |
| `warning`, `error` | ✅ Supported as Buildkite annotations |
| `group`, `endgroup` | ✅ Supported as linear log sections |
| Debug and matcher commands | ➖ Consumed without presentation behavior |
| `notice`, command echo control, other legacy commands | ❌ Unsupported |

Step summaries become job-scoped Buildkite annotations. This requires
Buildkite Agent v3.112 or newer. The total job summary is limited to 1 MiB.

## Expressions and contexts

There are three expression modes. They intentionally support different syntax.

### Conditions

Job and step `if` conditions support:

- literals;
- `!`, `&&`, `||`, `==`, and `!=`;
- `always()`, `success()`, `failure()`, and `cancelled()` with no arguments.

Ordered comparisons, `hashFiles`, other functions, and function arguments are
unsupported.

| Context | Job `if` | Step `if` |
| --- | --- | --- |
| `github.actor`, `event_name`, `ref`, `repository`, `sha` | ✅ Yes | ✅ Yes |
| `needs.<job>.result` and `needs.<job>.outputs.<name>` | ✅ Yes | ✅ Yes |
| `vars.<name>` and `matrix.<name>` | ✅ Yes | ✅ Yes |
| `steps.<id>.outcome`, `conclusion`, and `outputs.<name>` | ❌ No | ✅ Yes |
| `env.<name>` | ❌ No | ✅ Yes |
| `job.services.<service>.ports[<port>]` | ❌ No | ✅ Yes |
| `github.event.*` | 🟡 Compile time only | 🟡 Compile time only |
| `secrets` and other contexts | ❌ No | ❌ No |

An event-backed condition is evaluated from the immutable event snapshot before
runtime validation. Every branch is validated before evaluation, so
short-circuiting cannot hide an unsupported function, context, or concrete
matrix type error. A condition that cannot be fully resolved at compile time
cannot carry `github.event` into the runtime.

### Runtime interpolation

Interpolated values support direct references only:

```yaml
run: echo "${{ needs.build.outputs.image }}"
```

Available contexts include `github`, `inputs`, `matrix`, `vars`, `env`,
`steps`, `needs`, `secrets`, and service ports where that value exists. General
operators and functions are not supported inside interpolated strings.

At runtime, only `github.actor`, `github.event_name`, `github.ref`,
`github.repository`, and `github.sha` are retained. `github.event` is not
available.

### Compile-time expressions

Matrices, runner labels, names, concurrency groups, and event-backed conditions
may use statically known `github`, `event`, `vars`, and matrix values. Boolean
and equality operators, `fromJSON`, and case-insensitive `startsWith` are
supported where the complete expression resolves during compilation.

Values derived from runtime `needs` or `steps` are unsupported.

## Actions

### Action sources and runtimes

| Action type | Status | Boundary |
| --- | --- | --- |
| Local `./...` action | 🟡 Supported subset | Source tree is digest-locked and reverified. |
| Public `owner/repo[/path]@ref` action | 🟡 Supported subset | Resolved to an exact commit and digest. |
| Private action | ❌ Unsupported | No private action source access. |
| JavaScript action | ✅ Supported | `node16`, `node20`, or `node24` declaration. |
| Composite action | 🟡 Supported subset | Nested shell steps and locked local or public actions; `bash` or `sh` for `run`. |
| Dockerfile action | 🟡 Supported subset | Verified local or public Dockerfile action. |
| `docker://` action | ❌ Unsupported | Rejected during validation. |

Mutable public refs are resolved during upload, then locked to a commit. Exact
lowercase commit SHAs need no GitHub API lookup. Complete source trees are
verified again at runtime.

Nested calls from a repository-local composite must be local. Public composites
may call local children or other public actions; every child is resolved and
locked. Dockerfile actions cannot request credentials, volumes, arbitrary
options, or privileged mode.

### JavaScript runtimes

| Action declaration | Runtime |
| --- | --- |
| `node16` | Managed Node 16.20.2, with one end-of-job deprecation warning |
| `node20` | Managed Node 24.18.0 |
| `node24` | Managed Node 24.18.0 |

Pre, main, and post phases; inputs; outputs; state; and LIFO post ordering are
supported. Other Node declarations are rejected.

### `actions/checkout`

**🟡 Supported subset.** Only these resolved commits are admitted:

| Release | Commit |
| --- | --- |
| v4 | [`11d5960a326750d5838078e36cf38b85af677262`](https://github.com/actions/checkout/tree/11d5960a326750d5838078e36cf38b85af677262) |
| v5 | [`fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09`](https://github.com/actions/checkout/tree/fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09) |
| v6 | [`d23441a48e516b6c34aea4fa41551a30e30af803`](https://github.com/actions/checkout/tree/d23441a48e516b6c34aea4fa41551a30e30af803) |
| v7.0.0 corpus pin | [`9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0`](https://github.com/actions/checkout/tree/9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0) |
| v7.0.1 | [`3d3c42e5aac5ba805825da76410c181273ba90b1`](https://github.com/actions/checkout/tree/3d3c42e5aac5ba805825da76410c181273ba90b1) |

Major tags work only while they resolve to one of these commits. v1–v3 and
unknown commits are unsupported.

The adapter checks out a detached commit or static branch from the event
repository at the workspace root or a clean top-level directory. It uses
Buildkite's repository-provider Git credentials when the job provides them;
otherwise it fetches anonymously. Credentials are scoped to the fetch command
and each verified submodule fetch command, and are never persisted.

| Input | Supported values |
| --- | --- |
| `repository` | Omitted, or the event `owner/repo` |
| `ref` | Omitted, empty, a lowercase 40-hex commit, or a static branch in the event repository. A direct `github.sha` or `needs.<job>.outputs.<name>` expression must resolve at runtime to the exact event SHA. |
| `token` | Omitted only |
| `ssh-key`, `ssh-known-hosts` | Omitted or empty |
| `ssh-strict` | Omitted or true |
| `ssh-user` | Omitted or `git` |
| `persist-credentials` | Omitted or false |
| `path` | Omitted, empty, or one clean non-`.git` top-level workspace directory |
| `clean` | Omitted or true; the root workspace or selected path must be empty or absent |
| `filter`, `sparse-checkout` | Omitted or empty |
| `sparse-checkout-cone-mode` | Omitted or true |
| `fetch-depth` | Omitted or a non-negative integer; `0` fetches full history |
| `fetch-tags`, `show-progress` | Omitted, true, or false |
| `lfs` | Omitted or false |
| `submodules` | Omitted, false, true, or recursive; whitespace is trimmed and casing is ignored |
| `set-safe-directory` | Omitted or true |
| `github-server-url` | Omitted, empty, or `https://github.com` |
| `allow-unsafe-pr-checkout` | Omitted or false |

`false` and omission do not run submodule commands. `true` runs native Git
for direct children and `recursive` includes nested children. Relative URLs and
`fetch-depth` follow native Git behavior. Public and private GitHub submodules
are supported under the job's repository access; external HTTPS submodules are
anonymous. `git@github.com:` URLs are rewritten to HTTPS. Other SSH and
non-HTTPS transports are unsupported.

See the [security model](security.md#checkout-and-submodules) for the credential,
Git, and job-isolation boundaries.

Alternate repositories, tags, non-event dynamic commits, LFS, sparse checkout,
GitHub Enterprise Server, and credential persistence remain unsupported. Commit
and branch checkouts remain detached and confined to the event repository.

### `actions/upload-artifact`

**🟡 Supported subset.** These root actions use a native Buildkite ZIP adapter:

| Release | Commit |
| --- | --- |
| v4.6.2 | [`ea165f8d65b6e75b540449e92b4886f43607fa02`](https://github.com/actions/upload-artifact/tree/ea165f8d65b6e75b540449e92b4886f43607fa02) |
| v5.0.0 | [`330a01c490aca151604b8cf639adc76d48f6c5d4`](https://github.com/actions/upload-artifact/tree/330a01c490aca151604b8cf639adc76d48f6c5d4) |
| v6.0.0 | [`b7c566a772e6b6bfb58ed0dc250532a479d7789f`](https://github.com/actions/upload-artifact/tree/b7c566a772e6b6bfb58ed0dc250532a479d7789f) |
| v7.0.1 | [`043fb46d1a93c77aae656e7c1c64a875d1fc6a0a`](https://github.com/actions/upload-artifact/tree/043fb46d1a93c77aae656e7c1c64a875d1fc6a0a) |

| Input | Support |
| --- | --- |
| `name` | ✅ Supported; defaults to `artifact` |
| `path` | Required; literal files/directories or bounded `*`, `?`, character-class, and `**` file globs |
| `if-no-files-found` | `warn`, `error`, or `ignore` |
| `retention-days` | Non-negative integer; advisory only |
| `compression-level` | 0–9 |
| `overwrite` | Omitted or false |
| `include-hidden-files` | ✅ Supported |
| `archive` | v7.0.1 only; omitted or true |

Unsupported path forms include exclusions, symlinks, absolute paths, traversal,
braces, extglobs, leading glob comments, and special files. At most 32 path
roots may be selected. Hidden path segments remain excluded unless explicitly
enabled.

An artifact may contain at most 10,000 files and 1 GiB of source data. The ZIP
must also be no larger than 1 GiB. A job may publish 64 artifacts.

The adapter sets `artifact-id` and `artifact-digest`. `artifact-url` is empty
because no GitHub run-scoped URL exists. Merge, raw upload, overwrite, and
effective retention control are unsupported.

### `actions/download-artifact`

**🟡 Supported subset.** These root actions use the same producer-bound ZIP mode:

| Release | Commit |
| --- | --- |
| v4.3.0 | [`d3f86a106a0bac45b974a628896c90dbdf5c8093`](https://github.com/actions/download-artifact/tree/d3f86a106a0bac45b974a628896c90dbdf5c8093) |
| v5.0.0 | [`634f93cb2916e3fdff6788551b99b062d0335ce0`](https://github.com/actions/download-artifact/tree/634f93cb2916e3fdff6788551b99b062d0335ce0) |
| v6.0.0 | [`018cc2cf5baa6db3ef3c5f8a56943fffe632ef53`](https://github.com/actions/download-artifact/tree/018cc2cf5baa6db3ef3c5f8a56943fffe632ef53) |
| v7.0.0 | [`37930b1c2abaa49bbe596cd826c3c89aef350131`](https://github.com/actions/download-artifact/tree/37930b1c2abaa49bbe596cd826c3c89aef350131) |
| v8.0.0 | [`70fc10c6e5e1ce46ad2ea6f2b72d43f7d47b13c3`](https://github.com/actions/download-artifact/tree/70fc10c6e5e1ce46ad2ea6f2b72d43f7d47b13c3) |
| v8.0.1 | [`3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c`](https://github.com/actions/download-artifact/tree/3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c) |

| Input | Support |
| --- | --- |
| `name` | Exact name; mutually exclusive with `pattern`; runtime expressions are allowed |
| `pattern` | Bounded artifact-name glob; requires `merge-multiple: true`; runtime expressions are allowed |
| `path` | Optional literal workspace-relative path |
| `merge-multiple` | Omitted or false with `name`; required true with `pattern` |
| v8 `skip-decompress` | Omitted or false |
| v8 `digest-mismatch` | Omitted or `error` |

Artifacts must come from verified direct `needs` producers. Exact-name lookup
must find one unique artifact; a bounded pattern may select and deterministically
merge multiple distinct names. Artifact ID, all-artifact, cross-run,
cross-repository, raw, REST, and non-merged pattern modes are unsupported.

Only ZIPs produced by the supported upload adapter are accepted. Digest or ZIP
validation failure is fatal. The `download-path` output is supported.

### `actions/cache`

**🟡 Supported subset.** Only `actions/cache` v6.1.0 at
[`55cc8345863c7cc4c66a329aec7e433d2d1c52a9`](https://github.com/actions/cache/tree/55cc8345863c7cc4c66a329aec7e433d2d1c52a9)
is admitted. Its root, `restore`, and `save` entry points run the stock Node 24
cache-v2 client against the Buildkite Results service.

v4, v5, and unknown v6 commits are unsupported.

JavaScript and Docker actions with compatible bundled cache clients, such as
`actions/setup-go`, also receive job-bound cache-v2 credentials when the
service is available. Ordinary `run` steps and native action adapters do not.

## Repositories, credentials, and GitHub services

### Repositories

| Source | Support |
| --- | --- |
| Public GitHub event repository | ✅ Supported |
| Private GitHub event repository | 🟡 Supported when Buildkite authorizes repository-provider Git credentials |
| Alternate repository in `actions/checkout` | ❌ Unsupported |
| Public GitHub action | 🟡 Supported subset |
| Private action or reusable workflow | ❌ Unsupported |
| GitHub Enterprise Server or another provider | ❌ Unsupported |

### `GITHUB_TOKEN`

**🟡 Supported subset.** A job can request one short-lived token for the exact
event repository by using:

```yaml
permissions:
  pull-requests: write

jobs:
  comment:
    runs-on: ubuntu-latest
    steps:
      - env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: gh pr comment "$PR_NUMBER" --body "Checks passed"
```

The Buildkite organization must enable the job-bound token service. Buildkite's
server-side repository and permission policy remains the authority.

Job binding does not establish fork or actor trust. If untrusted sources can
edit workflows, they can request write permissions. Restrict write tokens with
Buildkite pipeline and organization policy.

The token is not added to the initial job environment. `github.token` is
available only while evaluating an effective action metadata input default.
Workflow-authored `github.token` and automatic ambient `GITHUB_TOKEN` are
unsupported.

### Other secrets and OIDC

**🚧 Not available in production.** Ordinary workflow secrets have an explicit
runtime boundary, but the production profile rejects them. Reusable workflow
secret mappings and environment secrets are also unavailable. Action metadata
defaults cannot add a secret to the plan; such defaults fail compilation rather
than becoming an authority source. A secret referenced only by a declared
optional action input does not add a job secret requirement and resolves empty
unless the same secret is required elsewhere. `GITHUB_TOKEN` continues to use
its separate permission-scoped contract regardless of action input optionality.

**❌ Unsupported.** GitHub-compatible OIDC and `id-token` are not implemented.

### GitHub services

**❌ Unsupported beyond the integrations listed above.** An action's runtime may
still require unsupported GitHub services. There is no GitHub Artifact, OIDC,
Packages, Releases, Checks, or deployment service emulation beyond the
integrations documented above.

## Runtime behavior and limits

### Runner tools

Accepted Ubuntu labels do not select a GitHub-hosted image. The Buildkite agent
must provide external tools used by shell steps.

By default, `RUNNER_TOOL_CACHE` points to a fresh job-private directory. An
operator may select an immutable runtime image with
`BUILDKITE_GHA_RUNTIME_IMAGE`; generated jobs then use that image's baked
`/opt/hostedtoolcache` inventory. Mutable image tags are rejected.

Dockerfile actions do not receive `RUNNER_TOOL_CACHE`.

### Results, retries, and cancellation

- A runtime-skipped Actions job appears successful in Buildkite while publishing
  a logical `skipped` result for downstream imported jobs.
- Retry the whole build if a producer result or artifact becomes ambiguous.
- Cancellation targets the complete process tree: `SIGINT`, `SIGTERM` after
  7.5 seconds, then `SIGKILL` after another 2.5 seconds.
- Summary or annotation publication failure is reported as a warning. It does
  not change a completed job result.

### Key limits

| Item | Limit |
| --- | --- |
| Matrix instances per job | 256 |
| Reusable-workflow nesting | 4 levels |
| Jobs after reusable-workflow expansion | 1,024 |
| Nested local action depth | 10 levels |
| Background steps active at once | 10 |
| Job outputs | 64 |
| Output value | 1 KiB |
| Job or step timeout | 360 minutes |
| Artifacts per job | 64 |
| Files per uploaded artifact | 10,000 |
| Uploaded source data or ZIP | 1 GiB |
| Job summary | 1 MiB |

## Validate compatibility

Check syntax and static graph construction without an event:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

Apply the same profile as production upload:

```sh
buildkite-gha validate \
  --profile hosted-tokenless \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

The results mean:

- **Compilable**: syntax and the static job graph can be translated.
- **Admitted**: resolved actions and generated plans pass production policy.
- **Runtime-proven**: repository tests or hosted evidence have executed the
  behavior.

Admission does not execute arbitrary action code. An admitted action may still
depend on an unsupported GitHub service.

See the [CLI guide](cli.md) for event snapshots, JSON reports, compilation, and
direct upload.
