# buildkite-gha

Run a GitHub Actions workflow as native Buildkite jobs—without creating a
GitHub Actions run.

`buildkite-gha` translates each workflow job (and each static matrix entry)
into a Buildkite job, then runs that job's Actions steps in a compatibility
runtime. Buildkite remains the source of truth for scheduling, logs, retries,
cancellation, and the build UI.

> [!IMPORTANT]
> This is an experimental pre-1.0 preview for **Linux x86-64 workflows**. The
> default remains public and tokenless. Direct upload has an explicit,
> fail-closed private-checkout preview for the pipeline's exact repository;
> private actions and workflow secrets other than the explicitly scoped
> `secrets.GITHUB_TOKEN` contract remain rejected.

## Buildkite controls the pipeline; the workflow describes the workload

A GitHub Actions workflow combines two concerns: run triggers and event filters
under `on:` control when GitHub creates a workflow run, while `jobs` and `steps`
describe the work in that run. `buildkite-gha` imports only the supported
workload portion into a Buildkite build that already exists. The supported
local `on.workflow_call` interface is workload composition, not trigger setup.

Buildkite pipeline integrations, settings, schedules, and manual or API build
requests decide when to create a build. The initial Buildkite pipeline
definition then invokes the plugin, which dynamically uploads the supported
workflow jobs into that build. The workflow's run triggers and event filters
neither create nor filter Buildkite builds.

Buildkite configuration also remains authoritative for agent targeting and
protected capabilities. Workflow settings such as `runs-on`, `permissions`,
environments, and concurrency are honored only within the explicitly supported
and admitted boundaries in the [support matrix](docs/compatibility.md#support-matrix);
they cannot change Buildkite pipeline settings or grant themselves authority.

## Try an existing workflow

Add the [GitHub Actions Buildkite
plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin)
to your Buildkite `pipeline.yml`:

```yaml
steps:
  - label: ":github: Test"
    key: "gha-ci"
    plugins:
      - github-actions#v0.4.4:
          workflow: .github/workflows/ci.yml
```

The pinned released plugin downloads and verifies `buildkite-gha` v0.4.2 by
default, derives the event context from the Buildkite build, and explicitly
targets the fixed `hosted` queue. Pin a released plugin version rather than a
floating branch. The current source CLI instead omits agent selectors by
default, so direct uploads inherit Buildkite's configured agent targeting unless
`BUILDKITE_GHA_TARGET_QUEUE` selects one queue explicitly. Jobs that can execute
JavaScript reuse mise 2026.5.12 or newer from `PATH` or the absolute path in
`BUILDKITE_GHA_MISE`; when neither provides a compatible version, the runtime
downloads and verifies a managed 2026.5.12 copy. Hosted Agents use their
attached cache; other environments fall back to an ephemeral cache when that
path is unavailable. Shell-only jobs and action jobs whose resolved trees
contain only shell steps, native adapters, or Docker do not require or install
mise.

Configure branch, tag, and pull request triggers in Buildkite. The plugin
derives a `pull_request` context for pull request builds and a `push` context
for every other build. Scheduled and manual Buildkite builds therefore do not
receive `schedule` or `workflow_dispatch` contexts or dispatch inputs. The
workflow's `on:` block does not create or change Buildkite triggers.

### Mix imported and native jobs

The imported workflow is an ordinary dynamic part of the Buildkite pipeline.
A native job can depend on the importer and will wait for the jobs it uploads:

```yaml
steps:
  - label: ":github: Test"
    key: "gha-ci"
    plugins:
      - github-actions#v0.4.4:
          workflow: .github/workflows/ci.yml

  - label: ":rocket: Deploy"
    key: "deploy"
    depends_on: "gha-ci"
    command: .buildkite/deploy.sh
```

This gives teams a migration path: start with the existing Actions workflow,
then move work into native Buildkite jobs over time. Automatic replacement of
a named imported job is planned, but is not part of this preview.

## Compare example runs

The basic CI, artifact handoff, and advanced delivery examples are manual
GitHub Actions workflows under `.github/workflows`. The dedicated
`buildkite-gha-examples` pipeline imports those exact files one at a time and
offers the same three choices through a Buildkite block step.

To launch both providers at the current branch's exact remote commit and print
their run URLs together:

```sh
scripts/compare-example basic
scripts/compare-example artifacts
scripts/compare-example advanced
```

The helper requires authenticated `gh` and `bk` CLIs. The current commit must
be the head of the corresponding `origin` branch, and GitHub must already know
the workflow from the repository's default branch. Pass `--github-only` or
`--buildkite-only` to launch just one side.

For the native manual experience, choose one of the `Example - ...` workflows
in GitHub's Actions tab. In Buildkite, create a build on the
`buildkite-gha-examples` pipeline and select the example when the build blocks.
Compare the job graph, matrix presentation, logs, summaries, annotations,
artifacts, retries, and cancellation behavior.

## Is my workflow a fit?

The [support matrix](docs/compatibility.md#support-matrix) is the authoritative
list of supported, partially supported, not-admitted, and unsupported GitHub
Actions behavior. As a quick screen, the plugin path is a good fit for
workflows built from:

- Linux Bash and `sh` steps;
- JavaScript, composite, local, and anonymous public actions;
- supported local and public Dockerfile actions;
- static matrices, ordinary `needs` and outputs, and local reusable workflows
  with statically resolved inputs, caller-visible results, and directly mapped
  declared outputs;
- the documented job and step condition subset, with unsupported functions and
  unavailable runtime contexts rejected before pipeline upload;
- GitHub Actions background, wait, cancellation, and parallel step controls;
- timeouts, `continue-on-error`, masking, summaries, warning/error annotations,
  and pre/main/post actions;
- public, credential-free checkout of the event repository at its exact commit;
- short-lived GitHub tokens scoped to explicit workflow or job `permissions`
  for the event repository, consumed through `secrets.GITHUB_TOKEN` or an
  effective action metadata input default that references `github.token`;
- bounded native upload and exact-name download for the audited artifact v4
  commits; and
- the audited `actions/cache` v6.1.0 commit through the official Buildkite
  cache-v2 Results service, with an optional operator override.

It is not currently a fit for workflows that require:

- private actions or general private-source access; direct upload has only the
  explicit pipeline-repository checkout described in the compatibility guide;
- ordinary workflow secrets—the scoped `secrets.GITHUB_TOKEN` contract is the
  only workflow-visible secret exception;
- workflow-authored `github.token`, ambient `GITHUB_TOKEN`, GitHub-compatible
  OIDC, or protected queues;
- `actions/cache` v4/v5 or unrecognized v6 commits, artifact
  merge/all/pattern/ID modes, or cross-run downloads;
- runtime condition access to the `github.event` payload, or condition
  functions and contexts outside the documented subset;
- job containers or service containers through the production plugin path;
- GitHub matrix fail-fast semantics—the complete static matrix is uploaded and
  sibling jobs are not canceled after one entry fails;
- compound or literal local reusable-workflow output mappings and reusable-call
  conditions;
- job-level or expression-valued `concurrency.cancel-in-progress`,
  runtime-dependent concurrency groups, or workflow-level concurrency declared
  by a called reusable workflow;
- privileged containers, arbitrary Docker options, or `docker://` actions; or
- Windows or macOS jobs.

The underlying runtime has broader container coverage than the production
plugin currently exposes. See the [compatibility and CLI guide](docs/compatibility.md)
for the exact distinction and intentional behavior differences.

## Check before running

Static validation does not contact Buildkite or execute the workflow:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

For example, a workflow with one producer and a two-entry consumer matrix
reports:

```text
Workflow: .github/workflows/ci.yml
Result: compilable
✓ 2 logical jobs and 3 static instances compile
```

To also resolve public actions and apply the same policy as the plugin's
`hosted-tokenless` upload, provide an event snapshot:

```sh
buildkite-gha validate \
  --profile hosted-tokenless \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

An `admitted` result means the plans satisfy upload policy. It does not execute
the workflow or prove that arbitrary action code is independent of GitHub-only
services. Condition preflight validates the supported syntax, functions,
contexts, and statically known operand types, but cannot prove value-dependent
runtime behavior. JSON output is available with `--format json`.

## What gets translated?

| GitHub Actions | Buildkite |
| --- | --- |
| Run triggers and event filters under `on:` | Not translated; Buildkite configuration creates the build |
| Workflow run | Existing Buildkite build |
| Job | Command job |
| Matrix entry | Command job with a stable key |
| `needs` | `depends_on` plus verified result transport |
| `runs-on` | Linux compatibility validation; Buildkite default agent targeting |
| `concurrency.group` | Repository-scoped Buildkite concurrency group or workflow gate |
| Job output | Producer-attributed result artifact |
| Step | Runs inside the job compatibility runtime |

Steps are intentionally **not** translated into separate Buildkite jobs. They
share a workspace, environment changes, action state, containers, and
post-action lifecycle in GitHub Actions, so they must stay inside one job here
too.

```diagram
┌──────────────────────────┐
│ Buildkite configuration  │
│ creates the build        │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐    ┌──────────────────────────┐
│ Existing Buildkite build │◀───│ Actions workflow + event │
│ invokes the importer     │    │ snapshot (workload input)│
└────────────┬─────────────┘    └──────────────────────────┘
             ▼
┌──────────────────────────┐
│ Validate, compile, and   │
│ dynamically upload jobs │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Native Buildkite jobs    │
│ one runtime per GHA job  │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Buildkite logs + results │
└──────────────────────────┘
```

## Documentation

- [Compatibility, behavior differences, and direct CLI use](docs/compatibility.md)
- [Development, smoke tests, and releases](docs/development.md)
- [Active product and implementation plan](docs/plans/2026-07-22-buildkite-gha.md)
- [Architecture decisions](docs/architecture/)

Use `buildkite-gha help`, `buildkite-gha help <command>`, or
`buildkite-gha --version` for the exact installed command surface.

## License

MIT. See [LICENSE](LICENSE).
