# Compatibility and CLI guide

This guide describes the behavior available to users today. The
[active plan](plans/2026-07-22-buildkite-gha.md) also contains implemented
internals, future product ideas, and deferred decisions; those are not support
promises unless they appear here.

## Pipeline configuration and workload are separate

GitHub Actions puts run creation and workload definition in one workflow file.
Buildkite separates them, and `buildkite-gha` preserves that boundary:

| Concern | Authoritative configuration |
| --- | --- |
| Build creation, repository events, branch/tag/PR filters, schedules, and manual builds | Buildkite pipeline integrations, settings, schedules, or manual/API build requests. Workflow run triggers and event filters are not imported. |
| Steps in an existing build | The initial Buildkite pipeline definition plus dynamic pipeline uploads. The plugin adds admitted workflow jobs here. |
| Job graph and step behavior | The supported `jobs`, matrices, `needs`, conditions, and `steps` subset in the selected Actions workflow. |
| Agents and protected capabilities | Buildkite pipeline/organization configuration and admission policy. Workflow runner, permission, environment, and credential declarations are requests, not grants. |

The imported workflow therefore cannot create, suppress, or reconfigure a
Buildkite build. Some workflow-level execution controls, such as the documented
concurrency subset, are translated into steps within the existing build; that
does not make the workflow a source of Buildkite pipeline settings. The
supported local `on.workflow_call` interface composes imported workloads and is
documented separately in the support matrix; it is not Buildkite trigger setup.

## The current execution profile

The production plugin and its bundled `upload` command default to one fixed
profile: `hosted-tokenless`. The README's pinned plugin v0.4.4 installs
`buildkite-gha` v0.4.2 and explicitly selects the `hosted` queue. The current
source CLI instead omits agent selectors by default and inherits Buildkite's
configured agent targeting; `BUILDKITE_GHA_TARGET_QUEUE` can explicitly select
one queue. Operators using default or explicit targeting are responsible for
providing suitable whole-job isolation. The profile is designed for public code
that can run without general protected credentials on a compatible Linux
x86-64 agent. Unsupported or privileged requests fail before the generated jobs
are uploaded. A compiler-verified checkout automatically declares bounded
repository-provider read capability. At runtime it uses Buildkite's native Git
credential helper only when the job environment indicates repository-provider
credentials are available; otherwise the same checkout runs anonymously.

There are three different compatibility claims:

1. **Compilable** — the workflow syntax and static job graph can be translated.
2. **Admitted** — action sources resolve and the generated plans satisfy the
   `hosted-tokenless` upload policy.
3. **Runtime-proven** — the repository's conformance or hosted test suite has
   executed that behavior successfully.

Compilation alone is not admission, and admission does not execute arbitrary
action code. In particular, an otherwise valid action may depend on a GitHub
artifact, token, OIDC, or unrecognized cache service that this project does not
provide.

## Support matrix

This is the canonical current support contract. Statuses mean:

- **Supported** — implemented, admitted by normal `hosted-tokenless` upload,
  and covered by runtime evidence.
- **Supported subset** — only the boundary in the notes is supported.
- **Not admitted** — compiler/runtime support exists, but production upload
  policy rejects it.
- **Not supported** — there is no current compatibility contract; validation or
  admission rejects it where it can identify it.
- **Buildkite extension** — implemented by `buildkite-gha`, but not part of
  standard GitHub Actions syntax.

An action being admitted means its source and declared runtime fit the profile;
it does not guarantee that arbitrary action code avoids an unsupported GitHub
service.

### Workflow syntax and job graph

| GitHub Actions area | Status | Current boundary |
| --- | --- | --- |
| Run triggers and event filters under `on:` | Not supported | Buildkite owns build creation and triggers. The plugin derives `pull_request` for pull-request builds and `push` for every other build; it does not create `schedule` or `workflow_dispatch` events or inputs. Direct CLI event snapshots can provide another event name, but workflow trigger entries are not matched and cannot create or suppress a Buildkite build. `on.workflow_call` is covered by the local reusable-workflows row below. |
| Linux x86-64 host jobs and `runs-on` | Supported subset | `ubuntu-latest`, `ubuntu-24.04`, and `ubuntu-22.04` are accepted. The released plugin configuration targets `hosted`; the current source CLI uses Buildkite defaults unless `BUILDKITE_GHA_TARGET_QUEUE` selects a queue. This is label validation, not Ubuntu image parity. On Ubuntu hosts the runtime derives GitHub's `ImageOS` family (for example, `ubuntu24`) from the actual host `/etc/os-release`; other distributions leave it unset. |
| Static job graphs and `needs` | Supported | Dependencies, matrix fan-out/fan-in, logical results, and bounded outputs are transported between generated jobs. Retrying one producer can make result selection ambiguous; retry the whole build. |
| Static matrices | Supported subset | Typed rows, `include`, `exclude`, compile-time `github`/event/`vars` values and `fromJSON`, exact dependency fan-out, and literal `max-parallel` are supported, with at most 256 expanded instances per job. Runtime matrices from `needs` or `steps` are not supported. `strategy.fail-fast` is accepted but not enforced: every expanded Buildkite job is uploaded and siblings are not canceled after a failure. |
| `env` and `defaults.run` | Supported subset | Workflow/job/step environment precedence, shell, and workspace-confined working directories are supported. A whole environment map cannot be expression-valued. Only `bash` and `sh` shells are supported; the host default is `bash` and a job-container default is `sh`. |
| Job and step conditions | Supported subset | Literals, `!`, `&&`, `||`, `==`, `!=`, and zero-argument `always`, `success`, `failure`, and `cancelled` are supported. Job conditions can use retained `github` identity fields, `needs`, `vars`, and `matrix`; step conditions additionally support `steps`, `env`, and service ports. Reusable-workflow inputs are substituted statically before this phase. Ordered comparisons, `hashFiles`, arguments to status functions, `github.event`, secrets in conditions, and unavailable contexts are not supported. |
| Runtime expression interpolation | Supported subset | Direct references to retained `github` identity fields, `inputs`, `matrix`, `vars`, `env`, `steps`, `needs`, `secrets`, and service ports are supported where that context is available. General expression operators and functions are not supported in interpolated values. Production upload rejects ordinary secret requirements. |
| Step and job outputs | Supported subset | Step outputs, job outputs, and `needs` consumption are supported. A job may publish at most 64 outputs and each value is limited to 1 KiB; ambiguous matrix output values fail closed. |
| Timeouts and `continue-on-error` | Supported subset | Literal step/job timeouts up to 360 minutes and step-level `continue-on-error` are supported. Expression-valued settings and job-level `continue-on-error` are not supported. |
| Workflow and job concurrency | Supported subset | Statically resolvable groups become repository-scoped, case-insensitive Buildkite concurrency groups. Workflow groups use an ordered opening/closing gate; job groups use `concurrency: 1`. Buildkite queues all waiting entries instead of replacing GitHub's pending entry. Workflow-level literal `cancel-in-progress: true` emits a warning but does not cancel; job-level true and expression-valued cancellation are not supported. |
| Local reusable workflows | Supported subset | Statically resolvable `./.github/workflows/...` calls, typed static inputs, nesting, caller-visible aggregate results, and declared outputs mapped directly from `jobs.<job>.outputs.<name>` are supported. Literal/compound output mappings, call-level conditions, call secrets, remote source, dynamic paths/inputs/matrices, and called-workflow top-level concurrency are not supported. |
| `permissions` | Supported subset | Canonical permission maps are carried only for a statically referenced `secrets.GITHUB_TOKEN` or an effective action metadata input default that statically references `github.token`. Omitted permissions use the product's narrow `contents: read` default; explicit job permissions replace workflow permissions and local reusable workflows may only narrow them. `{}`, maps containing only `none`, `read-all`, `write-all`, and `id-token` do not grant authority. |
| Job and service containers | Not admitted | Literal Linux container images, environment, ports, persistent job containers, and services are implemented and runtime-proven. Production `hosted-tokenless` upload rejects job/service-container provenance. Credentials, volumes, arbitrary options, private images, and privileged containers are not supported. |
| Dynamic graph expansion | Not supported | Matrices or `needs` derived from runtime outputs and other runtime-created jobs are rejected. |
| Deployment environments and approvals | Not supported | Environment secrets, protection rules, reviewers, and deployment approval/state parity are not implemented. |

### Steps, actions, and runtime behavior

| GitHub Actions area | Status | Current boundary |
| --- | --- | --- |
| `run` steps | Supported subset | Linux `bash` and `sh`, environment/working-directory precedence, process-tree cancellation, and workspace confinement are supported. PowerShell, Python-as-shell, Windows command shells, and custom shell templates are not supported. |
| Environment files | Supported | `GITHUB_OUTPUT`, `GITHUB_ENV`, `GITHUB_PATH`, `GITHUB_STATE`, and `GITHUB_STEP_SUMMARY`, including multiline values, are supported with bounded files and entries. `NODE_OPTIONS` remains blocked. Matching GitHub Runner behavior, actions may propagate `GITHUB_*` and `RUNNER_*` values to later `env` expressions; runtime-owned context values are overlaid with their canonical values when each step process launches. |
| Workflow commands | Supported subset | `::add-mask`, `::stop-commands`, `::warning`, `::error`, `::group`, and `::endgroup` are supported. Groups flatten to linear Buildkite log sections. Debug and matcher commands are consumed without presentation behavior. `::notice`, command echo control, and other legacy commands are not supported. |
| Step summaries | Supported | Summaries publish as bounded job-scoped Buildkite annotations. Requires Buildkite Agent v3.112 or newer. Oversized per-step summaries are skipped; the aggregate job summary is limited to 1 MiB. |
| Local actions | Supported subset | Workspace actions are digest-locked and reverified. JavaScript, composite, and compiler-verified Dockerfile runtimes are supported; other action runtimes are not. |
| Public GitHub actions | Supported subset | Hosted importers use an available job-scoped Buildkite GitHub token only to resolve mutable tags and branches through the GitHub API; token-minting or redaction failure emits a sanitized warning before anonymous fallback, and local profile evaluation remains anonymous. Lowercase full commit SHAs require no REST resolution, and exact-commit archives are fetched anonymously and directly from codeload during import and runtime. Sources remain exact-commit locked, complete trees are digest-verified, and JavaScript/composite/Dockerfile runtime rules still apply. Private actions and actions that require unavailable GitHub services are not supported merely because source resolution succeeds. |
| JavaScript actions | Supported | `node16` declarations run on exact managed, digest-verified Node 16.20.2. Invoked Node 16 actions are named in one end-of-job warning, matching GitHub Actions' historical deprecation behavior. `node20` and `node24` declarations run on managed Node 24.18.0, matching GitHub-hosted runners' current Node 20 deprecation behavior. The temporary `ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION` opt-out is not supported. Pre/main/post lifecycle, inputs, outputs, state, LIFO post ordering, and the standard cache-v2 environment are supported. Other Node action runtime declarations are not. |
| Composite actions | Supported subset | Nested shell/action steps, outputs, and global pre/main/post ordering are supported. Composite `run` steps must select `bash` or `sh`. |
| Dockerfile actions | Supported subset | Only compiler-verified local or public Dockerfile actions are admitted. The standard cache-v2 environment is available inside the action container. Lifecycle overrides, arbitrary Docker options, other credentials, volumes, private images, and privileged execution are not supported. |
| `docker://` actions and action `entrypoint`/`args` overrides | Not supported | Validation rejects these forms. |
| Concurrent step controls | Supported | GitHub Actions `background`, `wait`, `wait-all`, `cancel`, and `parallel` controls run inside one job with at most ten active background steps. Effects and failures become visible at covering waits, remaining work is joined before cleanup, and cancellation targets the complete process group rather than only the direct process. |
| `actions/checkout` | Supported subset | Only the [audited commits](#actionscheckout-native-adapter) from current v4-v7 are admitted. The adapter checks out the `github.com` event repository's exact SHA at the workspace root with depth 1 or 0, optional shallow tags, and command-scoped Buildkite repository-provider credentials. Unknown commits and unsupported inputs fail closed. |
| `actions/upload-artifact` | Supported subset | Exactly v4.6.2, v5.0.0, v6.0.0, and v7.0.1 are adapted at the immutable commits listed below, root entrypoint only and ZIP mode only. Bounded literal paths and final-component file globs, ZIP compression 0–9, hidden-file selection, no-file behavior, and advisory `retention-days` are supported. Recursive globs, exclusions, symlinks, effective retention control, overwrite, v7 raw uploads, merge, and GitHub URLs are not supported. |
| `actions/download-artifact` | Supported subset | Only the audited v4.3.0 commit is adapted. One exact literal name from verified direct `needs` can be extracted to a clean workspace-relative path. IDs, patterns, all-artifact, merge, cross-run, and cross-repository modes are not supported. |
| `actions/cache` | Supported subset | Only the audited v6.1.0 commit, including its `restore` and `save` entry points, is admitted. It runs the stock Node 24 cache-v2 client with fresh job-bound credentials and the official Buildkite Results service by default. v4/v5 and unrecognized v6 commits are not supported. |
| Cache clients bundled into actions | Supported subset | JavaScript and Docker action invocations receive fresh job-bound cache-v2 credentials when the service is available, matching the environment expected by setup actions such as `actions/setup-go`. The action remains responsible for its own key, version, paths, save/restore lifecycle, and cache-miss behavior. Ordinary `run` steps and native adapters do not receive cache credentials. |
| Runner tool cache | Supported subset | By default, the runtime points `RUNNER_TOOL_CACHE` at a fresh job-private directory and does not expose an agent or Hosted shared cache as executable authority. When pipeline configuration selects an immutable runtime image with `BUILDKITE_GHA_RUNTIME_IMAGE`, generated jobs use that image's baked `/opt/hostedtoolcache` facade instead. Dockerfile actions do not receive `RUNNER_TOOL_CACHE`. The shared Hosted cache volume is never used for executable tools because stock action tool-cache clients trust a writable entry and completion marker without verifying its contents. |
| Other GitHub service-backed actions | Not supported | Except for the integrations listed above, no general GitHub artifact, OIDC, Packages, Releases, Checks, or deployment service emulation is provided. An otherwise supported action runtime may still fail if its code requires one of those services. |

### Repositories, credentials, and platforms

| GitHub Actions area | Status | Current boundary |
| --- | --- | --- |
| Public GitHub repositories | Supported subset | Public event-repository checkout and public GitHub actions are supported within the source-authentication boundary above. GitHub Enterprise Server and non-GitHub repository providers are not current production sources. |
| Private repositories | Supported subset | The event repository can be checked out when Buildkite supplies repository-provider Git credentials to the job and its backend authorizes the concrete repository URL. Alternate repositories, private actions, and private reusable workflows are not supported. |
| `secrets.GITHUB_TOKEN` | Supported subset | A static reference can receive one short-lived token for the exact event repository when the job has non-empty effective permissions and the organization enables the job-bound token service. Omitted permissions use the product's narrow `contents: read` default. The runtime does not inject the token into the initial job environment; an action may explicitly export it through `GITHUB_ENV`, as on GitHub Runner. |
| Other workflow secrets | Not admitted | The runtime has a plan-declared `BUILDKITE_GHA_SECRET_<NAME>` resolver boundary, but `hosted-tokenless` admission rejects its `secrets` capability. Reusable-workflow secret passing and environment secrets are also rejected. |
| `github.token` and ambient `GITHUB_TOKEN` | Supported subset | `github.token` is populated only while evaluating an effective action metadata input default and uses the same scoped-token contract as `secrets.GITHUB_TOKEN`. Workflow-authored `github.token` and automatic ambient `GITHUB_TOKEN` remain unavailable. An action may explicitly export its input as `GITHUB_TOKEN` for later steps through `GITHUB_ENV`; an explicitly supplied action input, including an empty value, suppresses its metadata default. |
| OIDC | Not supported | GitHub-compatible and migration OIDC flows, including `id-token`, are deferred. |
| Windows and macOS | Not supported | Runner labels fail validation. Linux arm64 is also outside the current Linux x86-64 distribution/runtime contract. |
| GitHub-hosted image parity | Not supported | Accepted Ubuntu labels do not promise GitHub's installed tools, image updates, filesystem layout, or service configuration. The selected Buildkite agents must provide the workflow's external tools. |

## User-visible behavior

### Buildkite owns the run

No shadow GitHub Actions run is created. Workflow jobs and matrix entries are
visible as Buildkite jobs, with Buildkite scheduling, logs, cancellation,
retries, and status.

Actions steps remain inside one compatibility runtime because they share a
workspace, environment-file updates, action state, and cleanup lifecycle.
Docker is another execution backend within that job—not a security boundary
between mutually untrusted steps. Use a disposable job VM when workflow code
must be isolated from other jobs or the agent host.

Step summaries and workflow-command warnings/errors are visibility-only
Buildkite annotations. Their publication is attempted after the authoritative
logical result; an annotation API failure is reported as a warning but does not
rewrite an otherwise successful job. Successfully parsed warning/error command
lines are consumed and their decoded messages remain visible in the job log.

### Concurrency groups queue instead of canceling

Literal workflow and job `concurrency` shorthand is supported, as is the
mapping form when `cancel-in-progress` is omitted or the literal `false`.
Workflow-level literal `true` is also accepted, but only the group is enforced:

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

jobs:
  deploy:
    concurrency: deploy-${{ matrix.target }}
```

Groups must resolve during compilation. Direct substitutions can use `vars`,
concrete `matrix` values for job groups, statically substituted `inputs` in jobs
from local reusable workflows, and the available `github` fields: `actor`,
`event`, `event_name`, `ref`, `repository`, `repository_owner`, `sha`, and
`workflow`. Other fields and expression operators/functions fail closed; for
example, `github.head_ref || github.run_id` is not available. Runtime
`needs`/`strategy` values and workflow-level concurrency in a called reusable
workflow also fail closed. A matrix job may combine
`strategy.max-parallel` with concurrency only when every matrix entry resolves
to the same case-insensitive group; the group limit of one then subsumes
`max-parallel`.

Generated group identities are hashed with the event repository. This keeps
GitHub's repository scope and case-insensitive matching without exposing event
values or exceeding Buildkite's 200-character concurrency-key limit. A
workflow-level group emits Buildkite's ordered opening/closing gate steps, so
jobs inside one admitted workflow run retain their normal parallelism.

This is queue compatibility, not cancellation parity. Buildkite retains all
waiting entries in FIFO order, while GitHub's default concurrency mode replaces
an existing pending entry. The newer GitHub `queue` property is not yet accepted
by the pinned syntax frontend; generated Buildkite groups always queue all
entries. Workflow-level literal `cancel-in-progress: true` retains the
workflow gate and emits
`W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED`; it does not cancel an
older Buildkite build. Configure **Cancel Intermediate Builds** in the
Buildkite pipeline's Builds settings for running builds and **Skip Intermediate
Builds** for builds that have not started. Those pipeline-level, same-branch
controls approximate GitHub cancellation only when that scope matches the
workflow's concurrency group; arbitrary groups do not map exactly. Job-level
literal `true` and every expression-valued `cancel-in-progress` still fail
compilation.

For a workflow-level gate, cancel the Buildkite build rather than one generated
job: an individually canceled dependency does not execute the closing gate
step, while whole-build cancellation removes that build's queued gate jobs.

### Concurrent steps stay inside the job

Background and parallel work use a bounded, ten-active-step supervisor rather
than creating more Buildkite jobs. For example:

```yaml
steps:
  - id: lint
    run: ./scripts/lint
    background: true

  - id: test
    run: ./scripts/test
    background: true

  - wait-all:
```

A background step's outputs, environment changes, and failures become visible
at the covering `wait` or `wait-all`. Remaining work is joined by an implicit
final `wait-all` before post-action cleanup. Use `cancel: <step-id>` for targeted
cancellation or `parallel:` for a fixed inline group. These controls preserve
one job workspace and lifecycle.

### Buildkite owns build creation and triggers

Select the workflow explicitly in the plugin and configure pipeline triggers in
Buildkite. `buildkite-gha` uses the event snapshot to populate Actions contexts;
it does not subscribe to GitHub events or turn workflow `on:` entries into
Buildkite triggers. The initial Buildkite pipeline invokes the importer after a
build exists, and the importer can only add steps to that build through dynamic
pipeline upload.

The plugin derives `pull_request` for Buildkite pull request builds and `push`
for branch, tag, scheduled, and manual builds. It does not currently provide
`schedule` or `workflow_dispatch` contexts or dispatch inputs. A scheduled or
manual trigger is therefore compatible only when the workflow expects push
semantics. The direct CLI accepts explicit event snapshots as documented below,
but the plugin does not expose that option yet.

For pull requests, the plugin uses the exact Buildkite commit and a
`refs/pull/<number>/head` compatibility ref. It does not claim GitHub's merge-ref
semantics when Buildkite has not supplied them.

### `actions/checkout` native adapter

The compiler resolves mutable action references before selection and records the
exact commit and source-tree digest in the plan. Repository identity selects the
checkout integration, but the native adapter is admitted only when the resolved
commit is in this audited allowlist (snapshot 2026-08-10):

| Upstream major | Audited major-tag commit | Metadata and native decision |
| --- | --- | --- |
| v1 | [`50fbc622fc4ef5163becd7fab6573eac35f8462e`](https://github.com/actions/checkout/blob/50fbc622fc4ef5163becd7fab6573eac35f8462e/action.yml) | **Not supported.** Legacy `runs.plugin: checkout`, no JavaScript lifecycle or outputs, and an unlimited-history default do not match this adapter. |
| v2 | [`0717577d45739eb3c851188b29f50ed6c0b2194e`](https://github.com/actions/checkout/blob/0717577d45739eb3c851188b29f50ed6c0b2194e/action.yml) | **Not supported.** Node 12 main/post action, no outputs, and an older input surface. |
| v3 | [`a37ce9120846195fa4ece8f58b268e6043cb2f26`](https://github.com/actions/checkout/blob/a37ce9120846195fa4ece8f58b268e6043cb2f26/action.yml) | **Not supported.** Node 16 main/post action and no `ref`/`commit` outputs. |
| v4 | [`11d5960a326750d5838078e36cf38b85af677262`](https://github.com/actions/checkout/blob/11d5960a326750d5838078e36cf38b85af677262/action.yml) | **Supported subset.** Node 20, `dist/index.js` main/post, no pre, `ref` and `commit` outputs. |
| v5 | [`fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09`](https://github.com/actions/checkout/blob/fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09/action.yml) | **Supported subset.** Same public contract as v4 with Node 24. |
| v6 | [`d23441a48e516b6c34aea4fa41551a30e30af803`](https://github.com/actions/checkout/blob/d23441a48e516b6c34aea4fa41551a30e30af803/action.yml) | **Supported subset.** Node 24; upstream moved persisted credentials to a temporary file and conditional includes, but persistence is not implemented here. |
| v7 | [`3d3c42e5aac5ba805825da76410c181273ba90b1`](https://github.com/actions/checkout/blob/3d3c42e5aac5ba805825da76410c181273ba90b1/action.yml) | **Supported subset.** Node 24 ESM implementation with the same public inputs, main/post entrypoint, and outputs as current v6. |
| v7 corpus pin | [`9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0`](https://github.com/actions/checkout/blob/9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0/action.yml) | **Supported subset.** The v7.0.0-era pin used by the OSS corpus. v7.0.1's default-self-checkout guard bypass, explicit-ref parsing, and cleanup hardening do not alter this bounded native result. |

The upstream metadata surface evolved as follows. “Unset” means the manifest has
no default; it is not the same as a declared empty string.

| Majors | Input/default and lifecycle changes |
| --- | --- |
| v1 | Declares `repository`, `ref`, `token`, `clean: true`, `submodules`, `lfs`, `fetch-depth`, and `path`; all except `clean` are unset (the depth description says unlimited). Uses the runner `checkout` plugin and has no outputs or action lifecycle entrypoints. |
| v2 | Changes the default depth to `1`; defaults `repository` to `${{ github.repository }}`, `token` to `${{ github.token }}`, `ssh-strict`, `persist-credentials`, `clean`, and `set-safe-directory` to true, and `lfs`, `submodules`, and `allow-unsafe-pr-checkout` to false. Adds unset `ssh-key` and `ssh-known-hosts`. Uses Node 12 main/post with no pre or outputs. |
| v3 | Retains v2 and adds `sparse-checkout: null`, `sparse-checkout-cone-mode: true`, `fetch-tags: false`, and unset `github-server-url`. Uses Node 16 main/post with no pre or outputs. |
| v4 | Adds `ssh-user: git`, `filter: null`, and `show-progress: true`; adds `ref` and `commit` outputs and moves to Node 20. |
| v5-v7 | Retain the current v4 public inputs, defaults, outputs, and main/post shape with no pre. v5-v7 use Node 24; v6 isolates persisted credentials under runner temp, and v7 migrates the implementation to ESM. |

Major tags are mutable. A tag such as `actions/checkout@v7` works only while it
resolves to an allowlisted commit; a moved tag or arbitrary future commit is
rejected until its exact source and metadata are audited. v1-v3 are explicitly
unsupported rather than silently replaced with different native semantics.

All supported commits declare the following current v4-v7 inputs. Input names
are case-insensitive and case-colliding duplicates are rejected. GitHub Actions
boolean spellings (`true`, `True`, `TRUE` and corresponding false forms) are
accepted where shown. The compiler validates admitted static values, and the
runtime repeats validation after expression evaluation. A dynamic value cannot
widen the event repository, SHA, or credential boundary. Checkout input
expressions must currently resolve during compilation to an admitted literal;
otherwise compilation fails closed.

| Input | Admitted values | Native decision |
| --- | --- | --- |
| `repository` | omitted, or the event `owner/repo` (case-insensitive) | Alternate repositories and an explicit empty value are rejected. |
| `ref` | omitted, empty, or the exact lowercase 40-hex event SHA | Branches, tags, other SHAs, and SHA-256 object IDs are rejected. Checkout is always detached at the event SHA. |
| `token` | omitted only | Explicit values, including empty, are rejected. The upstream `${{ github.token }}` default is not used. |
| `ssh-key`, `ssh-known-hosts` | omitted or empty | Non-empty SSH configuration is rejected; no SSH transport is attempted. |
| `ssh-strict` | omitted or true | Default-equivalent only. |
| `ssh-user` | omitted or `git` | Default-equivalent only. |
| `persist-credentials` | omitted or false | Credentials are never persisted. This intentionally differs from upstream's `true` default. |
| `path` | omitted or empty | Only the workspace root is supported. |
| `clean` | omitted or true | The workspace must already be empty. Existing files are rejected, not reset or deleted. |
| `filter`, `sparse-checkout` | omitted or empty | Partial and sparse clones are rejected. |
| `sparse-checkout-cone-mode` | omitted or true | Accepted only as an inert default while sparse checkout is absent. |
| `fetch-depth` | omitted, `1`, or `0` | Default/`1` fetches only the exact SHA with depth 1. `0` fetches all heads, tags, and an explicit exact-event refspec before detached checkout. Other values are rejected rather than normalized. |
| `fetch-tags` | omitted, true, or false | Default false suppresses tags for depth 1. True adds an explicit tag refspec. Depth 0 always includes tags. |
| `show-progress` | omitted, true, or false | Defaults to true and controls `git fetch --progress`. |
| `lfs` | omitted or false | LFS is not installed or fetched. |
| `submodules` | omitted or false | `true` and `recursive` are rejected. See the product decision below. |
| `set-safe-directory` | omitted or true | Accepted as default-equivalent; the adapter uses isolated Git config and does not mutate the user's global `safe.directory`. |
| `github-server-url` | omitted, empty, or `https://github.com` | GitHub Enterprise Server and alternate hosts are rejected. |
| `allow-unsafe-pr-checkout` | omitted or false | Alternate fork repository/ref selection is already impossible; true is rejected. |

The adapter does not execute upstream's Node main/post lifecycle. It initializes
an isolated Git repository, disables system/global Git config and hooks for its
commands, fetches over public HTTPS, and verifies `.git/HEAD` is the exact event
SHA. It emits the upstream v4+ `ref` and `commit` step outputs from the immutable
event. An omitted or empty `ref` outputs the event ref; an explicitly supplied
event SHA outputs an empty `ref`, matching upstream's commit classification. It
does not emit matcher commands or post state, configure credentials
for later steps, clean an existing checkout, choose a local branch, fall back to
the REST archive API, or modify global safe-directory configuration.

Generated jobs skip Buildkite's default checkout and allocate a fresh Actions
workspace. Compilation adds `provider-token-read` only after the root checkout
lock passes the commit and input boundaries. At runtime the same commit and
evaluated-input checks run again before Git or credentials are used.

#### Credentials and submodules remain narrow by design

At runtime, the fetch uses Buildkite's native
`buildkite-agent git-credentials-helper` when
`BUILDKITE_USE_REPOSITORY_PROVIDER_GIT_CREDENTIALS=true`, or when the legacy
`BUILDKITE_USE_GITHUB_APP_GIT_CREDENTIALS=true` signal is present. The helper
receives the current job identity and Agent access token, and the Buildkite
backend decides whether to issue credentials for the concrete repository URL
requested by Git. If neither signal is enabled, checkout is anonymous; the CLI
does not try to infer repository visibility.

The helper is configured only on the verified fetch command, with HTTP-path
matching enabled. It is not persisted in Git config. The runtime passes the
upper- and lower-case HTTP proxy variables captured from the job process before
workflow execution to that credentialed fetch, but does not add its job
credential to ordinary workflow subprocess environments. This command scoping
limits accidental inheritance; it is not an OS-level isolation boundary between
hostile processes in the same job. The Buildkite repository-provider backend
remains authoritative for whether it issues credentials for the concrete URL.
This checkout path does not populate
`GITHUB_TOKEN` or `github.token`, use workflow `permissions`, enable private
actions, or add support for alternate repositories or refs. The deprecated
`--private-checkout` option is temporarily accepted as a no-op for compatibility.

Public-HTTPS submodules are deliberately **not** implemented. An honest bounded
implementation would need to validate every `.gitmodules` URL and path before
Git follows it, distinguish direct from recursive updates, revalidate nested
manifests, prevent symlink/path escapes, preserve depth 1/0 semantics, and ensure
the event-repository helper is never offered to submodule hosts or paths. Git's
ordinary recursive update performs network access before that complete policy
can be established. Rather than broaden credential authority or special-case
the jq corpus, both `submodules: true` and `submodules: recursive` fail closed;
public, private, relative, and SSH submodules are all unsupported.

### Effective permissions provide a scoped workflow token

A job that statically references `${{ secrets.GITHUB_TOKEN }}`, or uses an
action with an effective metadata input default that statically references
`${{ github.token }}`, receives one short-lived GitHub installation token for
the exact event repository. An omitted `permissions` block uses the product's
narrow `contents: read` default; an explicit non-empty map replaces that default:

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

Workflow permissions apply to jobs that omit job permissions. A job-level
mapping replaces the workflow-level mapping rather than merging with it. Local
reusable workflows inherit the caller's effective permissions and can only
narrow them. `none` removes a permission. Explicit `{}` and maps containing
only `none` do not produce a token; `read-all`, `write-all`, and `id-token` are
unsupported. A static token reference without a non-empty effective mapping
fails compilation.

The compiler normalizes names such as `pull-requests` to the Agent API's
`pull_requests`, emits the exact map into a v6 plan, and records same-process
provenance for upload admission. The runtime requests the token once from the
current-job Agent endpoint. The service independently requires the requested
repository to match the pipeline's configured GitHub repository and applies
its own permission allowlist and organization enablement. Those server-side
checks are the authorization ceiling; the compiled map constrains the supported
client path but is not a separate security boundary against code that obtains
the current job's Agent access token. The token is registered with runtime and
Agent redaction before expressions or steps can use it, and result values
containing it are scrubbed.

An explicit `secrets.GITHUB_TOKEN` reference continues to resolve through the
scoped-token contract. For compatible actions, `github.token` is additionally
populated only while evaluating an effective metadata input default; it remains
unavailable to workflow-authored expressions. The runtime does not inject an
ambient `GITHUB_TOKEN`, but an action can explicitly export the token to later
steps through `GITHUB_ENV`, matching GitHub Runner file-command behavior. Other
secrets still use the existing explicit `BUILDKITE_GHA_SECRET_<NAME>` boundary
and are not enabled by this feature.

This job-bound service does not independently establish fork or actor trust.
Pipelines that permit workflow changes from untrusted sources must apply the
same care as GitHub Actions workflows granting write permissions, in addition
to the service's repository and permission policy.

### Artifact uploads use bounded native storage

The native bridge was audited against these official immutable releases and
their committed metadata and source:

| Upstream release | Immutable commit | Metadata/runtime | Bundled artifact client | Native decision |
| --- | --- | --- | --- | --- |
| [`v4.6.2`](https://github.com/actions/upload-artifact/releases/tag/v4.6.2) | [`ea165f8d65b6e75b540449e92b4886f43607fa02`](https://github.com/actions/upload-artifact/tree/ea165f8d65b6e75b540449e92b4886f43607fa02) | Root [`action.yml`](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/action.yml) uses Node 20 and `dist/upload/index.js`; [`merge/action.yml`](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/merge/action.yml) uses Node 20 and `../dist/merge/index.js`. | `@actions/artifact` 2.3.2 | Root upload is native in the bounded ZIP subset below. Merge requires the unavailable GitHub Artifact service and production admission rejects it. |
| [`v5.0.0`](https://github.com/actions/upload-artifact/releases/tag/v5.0.0) | [`330a01c490aca151604b8cf639adc76d48f6c5d4`](https://github.com/actions/upload-artifact/tree/330a01c490aca151604b8cf639adc76d48f6c5d4) | Root [`action.yml`](https://github.com/actions/upload-artifact/blob/330a01c490aca151604b8cf639adc76d48f6c5d4/action.yml) uses Node 20 and `dist/upload/index.js`; [`merge/action.yml`](https://github.com/actions/upload-artifact/blob/330a01c490aca151604b8cf639adc76d48f6c5d4/merge/action.yml) uses Node 20 and `../dist/merge/index.js`. | `@actions/artifact` 4.0.0 | Root upload retains the v4 input/output and ZIP search contract and is native in the bounded subset below. Merge remains rejected. |
| [`v6.0.0`](https://github.com/actions/upload-artifact/releases/tag/v6.0.0) | [`b7c566a772e6b6bfb58ed0dc250532a479d7789f`](https://github.com/actions/upload-artifact/tree/b7c566a772e6b6bfb58ed0dc250532a479d7789f) | Root [`action.yml`](https://github.com/actions/upload-artifact/blob/b7c566a772e6b6bfb58ed0dc250532a479d7789f/action.yml) uses Node 24 and `dist/upload/index.js`; [`merge/action.yml`](https://github.com/actions/upload-artifact/blob/b7c566a772e6b6bfb58ed0dc250532a479d7789f/merge/action.yml) uses Node 24 and `../dist/merge/index.js`. | `@actions/artifact` 5.0.1 | Root upload retains the v4/v5 input/output and ZIP search contract and is native in the bounded subset below. Merge remains rejected. |
| [`v7.0.1`](https://github.com/actions/upload-artifact/releases/tag/v7.0.1) | [`043fb46d1a93c77aae656e7c1c64a875d1fc6a0a`](https://github.com/actions/upload-artifact/tree/043fb46d1a93c77aae656e7c1c64a875d1fc6a0a) | Root [`action.yml`](https://github.com/actions/upload-artifact/blob/043fb46d1a93c77aae656e7c1c64a875d1fc6a0a/action.yml) uses Node 24 and `dist/upload/index.js`; [`merge/action.yml`](https://github.com/actions/upload-artifact/blob/043fb46d1a93c77aae656e7c1c64a875d1fc6a0a/merge/action.yml) uses Node 24 and `../dist/merge/index.js`. The package is ESM. | `@actions/artifact` 6.2.0 | Root upload is native only with `archive` omitted or true. Direct/raw mode and merge fail closed. |
| [`v7.0.0`](https://github.com/actions/upload-artifact/releases/tag/v7.0.0) | [`bbbca2ddaa5d8feaa63e36b76fdaad77386f024f`](https://github.com/actions/upload-artifact/tree/bbbca2ddaa5d8feaa63e36b76fdaad77386f024f) | First Node 24/ESM release with direct upload. | Not relied upon | Not admitted. |

Mutable `@v4` through `@v7` references are accepted only while resolution
produces the corresponding audited commit above. Every other old, future, or
unknown commit is rejected before a native lock is created and rechecked at
runtime. The exact source tree digest, metadata, declared runtime, and
entrypoints are still verified even though the adapter replaces the whole
upstream JavaScript lifecycle. The four admitted root actions declare the same
v4 input/output surface; only v7 adds `archive`.

The root upload input decisions are:

| Input | Upstream version/default | Native decision |
| --- | --- | --- |
| `name` | v4-v7; `artifact` | Supported after expression evaluation. Explicit empty, invalid upstream characters, invalid UTF-8, and names over 255 bytes fail. Names are unique case-insensitively within a job and at most 64 artifacts may be published. |
| `path` | v4-v7; required; upstream `@actions/glob` supports multiline globs/exclusions/comments and follows symlinks | Required. At most 32 newline-separated, workspace-relative literal files/directories or final-component `*` file globs such as `tests/*.log`. Safe equivalent spellings such as `./file` and `dir/` are normalized before collection. `..` components, `?`, character classes, globstar, braces, extglobs, recursive/directory globs, exclusions, leading glob comments, absolute paths, backslashes, symlinks, and special files fail closed. Expressions are admitted statically and the evaluated value is revalidated and normalized at runtime. |
| `if-no-files-found` | v4-v7; `warn` | `warn`, `error`, and `ignore` are supported exactly; other spellings fail. |
| `retention-days` | v4-v7; omitted or `0` uses repository settings | A non-negative integer, including `0`, is accepted as advisory compatibility. The runtime always emits a warning because Buildkite, not this action, controls effective artifact retention. Invalid or negative values fail. |
| `compression-level` | v4-v7; `6` | Levels 0–9 are supported. Level 0 creates a stored ZIP; it is not raw upload. Other values fail. |
| `overwrite` | v4-v7; `false` | Omitted or GitHub-toolkit boolean false only. True fails because this bridge does not perform GitHub's delete-before-upload behavior. Duplicate case-insensitive names also fail rather than overwrite. |
| `include-hidden-files` | v4-v7; `false` | Supported with GitHub-toolkit boolean spellings (`true`, `True`, `TRUE`, and false equivalents). Hidden path segments are excluded by default. |
| `archive` | v7 only; `true` | Omitted or true only at the v7.0.1 commit. Supplying it to v4-v6 fails as a version mismatch. False is not approximated: upstream requires exactly one selected file, ignores `name`, derives the artifact name from the file, and uses the v7 direct-upload protocol. |
| Any other or case-duplicate input | Version-specific | Rejected. Input names are compared case-insensitively. |

Scalar inputs follow upstream trimming before validation. Omitted values use the
defaults above; explicit empty values do not silently become defaults. Runtime
expressions use the normal supported expression contexts and are checked again
after evaluation, so an expression cannot introduce an unsafe path, raw mode,
overwrite, or an invalid value.

For matched paths, one selected directory becomes the archive root; one
selected file uses its parent; and multiple matched roots use their common
ancestor. A bounded file glob uses its literal parent as the root. Empty and
unmatched directories are omitted and do not widen the archive root.
Files are sorted lexically and ZIP headers use fixed timestamps and modes, so
identical inputs and compression levels produce deterministic bytes. Selection
and archive writing reject case-colliding member names, source replacement,
type/size/modification races detected around the read, more than 10,000 files,
more than 1 GiB of selected source bytes, and a final ZIP over 1 GiB. Unlike
upstream, symlinks are rejected rather than followed, and consumer-unsafe ZIP
member names are rejected.

All four audited roots declare `artifact-id`, `artifact-url`, and
`artifact-digest`. The bridge emits `artifact-id` as an opaque positive decimal
native identity and `artifact-digest` as the bare SHA-256 of the exact stored
ZIP. It deliberately leaves `artifact-url` unset: a GitHub run-scoped artifact
URL does not exist and is never fabricated. The authoritative terminal result
binds the ID and `sha256:` digest to the Buildkite artifact path, archive size,
file count, and producer job. Failed, cancelled, or no-file uploads publish no
action artifact record; job terminal publication still follows the normal
bounded result-manifest path.

The upstream `merge` variant is not a ZIP concatenation operation: it lists
current-run artifacts, filters with a pattern, downloads/extracts them, can use
separate directories, re-uploads a merged ZIP, and can delete its inputs. Its
`name`, `pattern`, `separate-directories`, `retention-days`,
`compression-level`, `delete-merged`, and `include-hidden-files` contract and
all three outputs depend on the GitHub Artifact service. It therefore remains
outside the native bridge and fails production admission rather than being
silently approximated. The v7 merge entrypoint has no raw mode.

The consumer recognizes only
`actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093`.
It scans only verified manifests from direct `needs`, requires one
exact-name match, downloads by the bound producer job UUID,
and verifies size and SHA-256 before preflighting and extracting the bounded
ZIP. Omitted `path` means workspace root; `download-path` is absolute. Digest
mismatch is fatal (stricter than upstream). GitHub URLs and metadata are not
fabricated. This native handling is an interim, bounded bridge; the parallel
Results ArtifactService work is intended to let stock upstream JavaScript use
its protocol rather than expanding this adapter into a protocol emulator.
Exact-commit Buildkite build 270 and its independent artifact observation prove
the producer-to-two-consumer roundtrip contract.

### Actions use the standard cache-v2 protocol

The explicit cache-action integration recognizes only
`actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9` (v6.1.0), including
the `restore` and `save` entry points from that same immutable source tree.
Older majors and every other commit fail closed. Unlike the artifact actions,
the runtime does not replace the upstream implementation: it executes the
audited action's stock Node 24 ESM lifecycle and supplies the standard
cache-v2 environment to the action.

Before each JavaScript pre, main, or post phase and each Docker action that
runs, the runtime attempts to post an empty body to the current job's
`/v3/jobs/<BUILDKITE_JOB_ID>/ghac_tokens` Agent API endpoint using the ambient
`BUILDKITE_AGENT_ACCESS_TOKEN`. The returned short-lived token is registered
with both the local log scrubber and `buildkite-agent redactor add` before the
action starts. A fresh token is minted for each invocation rather than
retaining a broad job credential. The runtime exposes only
`ACTIONS_CACHE_SERVICE_V2`, `ACTIONS_RESULTS_URL`, and
`ACTIONS_RUNTIME_TOKEN`; it does not forward the Agent job token, persist cache
credentials into job state, or expose them to ordinary `run` steps or native
adapters. Workflow-controlled cache service variables are discarded before
the runtime-owned values are overlaid.

The audited `actions/cache` integration remains stricter than a general action
invocation: credential or redaction failure stops it before its JavaScript
executes. It additionally discards Node/process injection settings, TLS and
OpenSSL configuration/module overrides, tar and dynamic-loader controls, and
upper- or lower-case proxy settings. Its subprocesses use the fixed
`/usr/local/bin:/usr/bin:/bin` tool path rather than workflow `PATH` changes. If
a credential appears in any action's command-file effects, those effects are
discarded and the action fails. Other JavaScript and Docker actions receive the
same cache-v2 environment when credentials can be minted, allowing embedded
clients such as `actions/setup-go` to use their normal cache lifecycle. If the
cache service is unavailable, those general actions continue without the
cache environment so an incidental cache does not make unrelated action
execution fail.

The runtime uses the official `https://ghacs.buildkite.com/` Results service by
default. Operators can set `BUILDKITE_GHA_CACHE_URL` to override it with another
compatible origin-only URL; a non-root path is rejected because the stock
cache-v2 client uses root-relative Twirp endpoints. The Buildkite organization
must have GHAC token minting enabled, and jobs must be able to reach both the
Results service and the Agent API.
The service-issued token scopes cache access to the current organization and
pipeline, grants writes only for an authorized ref, and limits cross-repository
pull requests to the read-only default-branch scope. For the explicit audited
cache action, disabled minting, malformed responses, redirects, unsafe override
configuration, or failed redaction stop the action before its JavaScript
executes.

This implemented and admitted contract has hosted runtime evidence. The
then-combined advanced migration POC in [Buildkite build 303](https://buildkite.com/buildkite/buildkite-gha/builds/303)
demonstrated a build-unique miss, post-action save, and direct dependent exact
hit at implementation commit `9d29bf26492be760016d29c7ba0d00033b4f9b39`.
The stable released-plugin extension subsequently proved the same producer
miss, post-save, and direct-dependent exact primary-key hit in [Buildkite build
337](https://buildkite.com/buildkite/buildkite-gha/builds/337). The minimal
Phase 6 cache fixture remains compile-only conformance coverage; it is not the
fixture that carries the runtime claim.

An independent migrated-repository E2E adds customer-shaped evidence. At exact
`mcncl/gotyper` commit
[`8a74f88676a120e0bc6090b1aafc65edfd62ebbe`](https://github.com/mcncl/gotyper/commit/8a74f88676a120e0bc6090b1aafc65edfd62ebbe),
[Buildkite build 11](https://buildkite.com/no-assembly/gotyper/builds/11)
passed a public two-job workflow through plugin `v0.2.1`. The migrated workflow
used exact audited checkout, setup-go, and cache commits, a shell-computed cache
key, a direct `needs` edge, race tests, static analysis, and a final binary
build. It explicitly disabled setup-go's token and built-in cache inputs, and
its compatible Hosted image supplied the C compiler required by `go test
-race`; this is evidence for the documented migrated subset, not a claim that
unmodified workflows or every Hosted image are compatible.

### Failures stay explicit

Unsupported syntax, runner labels, action types, and protected capabilities
normally fail closed. Accepted behavior differences are called out in the
support matrix instead of being claimed as parity. In particular,
`strategy.fail-fast` is currently retained during compilation but not enforced;
all expanded matrix jobs are uploaded.

A runtime-skipped Actions job currently appears successful to Buildkite while
publishing its logical `skipped` result. Downstream imported jobs use that
logical result. This is a UI difference until Buildkite has a scheduler-visible
skipped job state.

If producer identity or result artifact selection becomes ambiguous—for
example, after retrying an individual producer—consumers fail closed. Retry the
whole build rather than one imported job.

Cancellation targets the complete process tree: `SIGINT`, then `SIGTERM` after
7.5 seconds, then `SIGKILL` after another 2.5 seconds. GitHub uses the same
timing for the direct process, but `buildkite-gha` deliberately avoids leaving
child processes behind.

## Use the CLI directly

The [plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin)
is the recommended installation. For direct use, download
`buildkite-gha_Linux_x86_64.tar.gz` and `checksums.txt` from the matching GitHub
[release](https://github.com/buildkite/buildkite-gha/releases), verify the
archive checksum, and extract it to a stable location. The archive contains
`buildkite-gha` and `LICENSE`.

Generated jobs that can execute JavaScript actions support mise 2026.5.12 or
newer. `run-job` first checks `BUILDKITE_GHA_MISE` when set, then `PATH`; if
neither supplies a compatible version, it downloads the pinned 2026.5.12
official archive into the managed cache path. Hosted Agents attach that cache
automatically; other agent environments use it when available and otherwise
fall back to an ephemeral cache. Both the archive and extracted executable are
verified by embedded SHA-256 digests before use.
Verified cache bytes are copied into a job-private directory and reverified
there before execution, so the shared cache remains an accelerator rather than
executable authority. An explicit `BUILDKITE_GHA_MISE` must be absolute and
compatible rather than silently falling back. The runtime resolves and
validates the executable before workflow code can modify `PATH`, then uses it
with repository configuration disabled to install the exact Node 16.20.2 or
24.18.0 release required by each action. Those Node binaries require glibc 2.28
or newer; the static Go CLI does not. Shell-only jobs and action jobs whose
resolved trees contain only shell steps, native adapters, or Docker do not
require or install mise.
Importers, `validate`, and `compile` do not require or install mise either.

### Validate

Validate the static workflow subset without an event:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

Apply the production profile with human-readable or machine-readable output:

```sh
buildkite-gha validate --profile hosted-tokenless \
  --event-path .buildkite/events/current.json --format text \
  .github/workflows/ci.yml

buildkite-gha validate --profile hosted-tokenless \
  --event-path .buildkite/events/current.json --format json \
  .github/workflows/ci.yml
```

Profile validation may access the public network to resolve actions. It does
not call Buildkite, install Node, or execute workflow code.

### Event snapshots

`compile` and profile validation take an explicit, bounded event snapshot:

```json
{
  "provider": "github",
  "event": "push",
  "repository": {
    "owner": "acme",
    "name": "widgets",
    "clone_url": "https://github.com/acme/widgets.git",
    "default_branch": "main"
  },
  "ref": "refs/heads/main",
  "sha": "0123456789abcdef0123456789abcdef01234567",
  "actor": "octocat",
  "payload": {
    "ref": "refs/heads/main"
  }
}
```

The snapshot supplies the compile-time Actions context. Generated plans retain
the event name, repository, ref, SHA, actor, and a payload digest, but not the
payload object itself. Runtime conditions can use those retained identity fields
but cannot access `github.event`; validation rejects an expression such as
`github.event.action` before pipeline upload.
The snapshot is compatibility data, not proof that the event is trustworthy,
and cannot authorize a protected capability.

### Compile

Render deterministic Buildkite pipeline YAML, or inspect the compiler IR:

```sh
buildkite-gha compile --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml

buildkite-gha compile --event-path .buildkite/events/current.json \
  --format ir-json .github/workflows/ci.yml
```

The pipeline output references content-addressed plans and the exact compiler
executable. `compile` is read-only: it does not materialize those artifacts or
upload a pipeline, so piping its output directly to a real Buildkite upload is
not a complete execution path.

### Upload

`upload` is the in-build command used by the plugin. The current source CLI
uses:

```sh
buildkite-gha upload .github/workflows/ci.yml
```

When invoking the v0.4.2 release directly, add `--runtime-queue hosted`; that
release requires the argument and uses it to
select the `hosted` queue.

It requires `BUILDKITE=true` and `BUILDKITE_STEP_KEY`. Event source precedence
is:

1. An explicit `--event-path`. This never reads Buildkite metadata.
2. Buildkite's reserved `buildkite:webhook` metadata for webhook-linked builds.
3. A reduced-fidelity snapshot derived from `BUILDKITE_*` variables when
   Buildkite reports that the build is not webhook-triggered or its linked
   webhook is unavailable.

The webhook metadata is fetched once and must be one valid JSON object no larger
than 25 MiB. Malformed, duplicate-key, non-object, oversized, transport,
authorization, and rate-limit failures stop upload; they do not silently select
the fallback. `BUILDKITE_GITHUB_EVENT`, when valid, supplies `github.event_name`,
and a valid webhook `sender.login` improves `github.actor` compatibility.
Buildkite's validated repository mapping and exact `BUILDKITE_COMMIT` and
branch/tag/PR ref always supply top-level `github.repository`, `github.sha`, and
`github.ref`. These values intentionally need not equal trigger payload fields:
pull requests and rebuilds can legitimately execute different identities.

All three paths remain untrusted compatibility data. Webhook fields cannot
authorize queues, secrets, tokens, or protected capabilities. Generated plans
and pipeline YAML retain only the existing payload digest and values deliberately
selected by compile-time expressions, never the raw payload object. The metadata
is Buildkite's stored parsed webhook document rather than the original HTTP
delivery: delivery headers are unavailable, and GitHub push payloads may omit
`commits`.

Apart from the documented scoped workflow-token contract and Buildkite's native,
job-bound repository-provider credentials for verified checkout, upload remains
free of provider credentials.

The command uploads the exact executable and content-addressed plans before
calling `buildkite-agent pipeline upload --no-interpolation --reject-secrets`.
In the current source CLI, generated jobs do not set `agents`, so Buildkite's
pipeline or organization defaults select the agents. The deprecated
`--runtime-queue hosted` argument remains accepted as a no-op for compatibility
with plugin releases that pass it to v0.4.2; other values are rejected rather
than silently ignored. The deprecated `--private-checkout` argument is also
accepted as a no-op for one-release migration compatibility.

An importer that must select one queue can set `BUILDKITE_GHA_TARGET_QUEUE` on
its step. The uploader maps every accepted Linux runner label to that queue,
emits the corresponding `agents` selector, and binds the plan to the runtime
queue. An empty, whitespace-containing, or otherwise invalid queue fails before
pipeline upload. This setting is ordinary pipeline configuration, not an
authenticated grant: it admits untrusted workflow code to the named queue, so
that queue must provide suitable whole-job isolation and no ambient protected
credentials.

An importer can opt every generated workflow job into a toolchain-enabled
Hosted image by setting `BUILDKITE_GHA_RUNTIME_IMAGE` to an immutable registry
digest, for example:

```yaml
env:
  BUILDKITE_GHA_RUNTIME_IMAGE: buildkite.namespace-images.com/agent-base@sha256:04a6656f92b90269b3259fffaba67e08a3d03d8dc79b40d45c9ac3d9000e9e03
```

The uploader emits this as each generated job's `image` and points the job's
`RUNNER_TOOL_CACHE` at the image-baked `/opt/hostedtoolcache` inventory. Tags
and other mutable references are rejected. Without this setting, generated
jobs retain their current image selection and fresh job-private tool cache.

`run-job` is an internal command emitted into generated jobs. Users should not
need to invoke it directly.
