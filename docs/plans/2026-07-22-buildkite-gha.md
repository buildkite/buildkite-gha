# `buildkite-gha`: GitHub Actions compatibility for Buildkite

Status: **Active**
Date: 2026-07-22
Last reviewed: 2026-07-22
Target repository: `buildkite/buildkite-gha`

## Summary

Build `buildkite-gha`, a standalone open-source Go project that allows a
GitHub Actions workflow to run as a native Buildkite build. The project has two
responsibilities:

1. Compile a GitHub Actions workflow into ordinary Buildkite pipeline jobs.
2. Execute the GitHub Actions steps inside each generated Buildkite job through
   an Actions-compatible runtime.

The intended entry point is conceptually:

```bash
buildkite-gha upload .github/workflows/ci.yml
```

with a possible future Buildkite Agent integration:

```bash
buildkite-agent pipeline load-gha-workflow .github/workflows/ci.yml
```

Buildkite, not GitHub, owns scheduling, logs, job state, cancellation, retries,
artifacts, and the build UI. GitHub may remain the source-code host during
migration, but it is not part of the workflow execution control plane. This
also permits a repository to move to Cursor Origin without replacing the
execution architecture.

The unit of translation is the Actions job:

| GitHub Actions concept | Buildkite representation |
| --- | --- |
| Workflow run | Buildkite build |
| Workflow job or matrix entry | Buildkite command job |
| `needs` | `depends_on` plus producer-attributed result manifests |
| `runs-on` | Buildkite queue/agent selectors |
| Static matrix | Expanded keyed Buildkite jobs |
| Dynamic matrix | Deferred dynamic pipeline upload |
| Job output | Namespaced Buildkite build metadata |
| Uploaded artifact | Buildkite artifact |
| Action step | Executed inside the job compatibility runtime |
| Parallel/background action steps | Concurrent processes inside that same job |

Do not translate individual Actions steps into separate Buildkite jobs. Actions
steps share a workspace, process environment mutations, job and service
containers, action state, and post-action lifecycle. That remains true for
parallel and background steps.

## Product outcome

A team should be able to:

1. Create a Buildkite pipeline for an existing repository.
2. Point it at an existing `.github/workflows/*.yml` file.
3. Run the workflow on Buildkite with useful compatibility diagnostics and no
   GitHub Actions control-plane dependency.
4. View progress and logs in Buildkite.
5. Add native Buildkite jobs before or after the imported workflow.
6. Replace individual imported jobs with native Buildkite jobs over time.
7. Move the repository from GitHub to Cursor Origin while retaining the same
   Buildkite pipeline and most portable actions.
8. Remove the compatibility layer when the migration is complete.

GitHub Actions becomes an accepted pipeline and action format, not the system
of record.

## Product principles

### Buildkite is authoritative

Every imported workflow is a normal Buildkite build. Buildkite owns the job
graph and displays the live job logs. There is no shadow GitHub workflow run to
reconcile and no requirement to visit GitHub to understand progress.

### Compile jobs; interpret steps

Translate workflow-level and job-level constructs into Buildkite primitives
where their lifecycle and isolation semantics align. Execute step-level Actions
semantics inside one job runtime rather than pretending every Actions feature
has a one-to-one Buildkite YAML equivalent.

### Preserve a native escape path

Generated jobs must use deterministic keys and ordinary Buildkite contracts so
native jobs can depend on imported jobs. The compiler must eventually support
replacing an imported job while preserving its logical dependency and output
contract.

### Avoid GitHub's private runner protocol

Do not register the official Actions runner with a fake GitHub service or make
GitHub's private `AgentJobRequestMessage`, timeline, results, or action-resolution
APIs part of the architecture. They assume a server-compiled job and create an
unstable dependency on GitHub's control plane.

Use the open-source Actions runner as a behavioral reference and compatibility
oracle. Reuse suitable Go components from `nektos/act`, subject to a focused
technical spike and retained license attribution.

### Make compatibility explicit

Do not silently approximate unsupported semantics. `buildkite-gha validate`
must identify unsupported syntax, provider-dependent actions, unavailable
runtime capabilities, and security-sensitive behavior before execution.

### Keep secrets out of compiled plans

The compiler records secret names and permission requirements, never values.
The runtime resolves secrets inside the destination job, registers them with
the Buildkite Agent redactor before action output can be emitted, and keeps
them out of metadata, artifacts, generated YAML, and debug diagnostics.

## Goals

- Run useful existing Linux GitHub Actions workflows as native Buildkite
  builds.
- Support `run`, JavaScript, Docker, and composite actions.
- Preserve job workspace, environment-file, workflow-command, output, and
  pre/post-action behavior.
- Map static workflow graphs, matrices, routing, timeouts, and concurrency onto
  Buildkite where semantics align.
- Handle runtime-dependent graph expansion through controlled dynamic pipeline
  uploads.
- Provide deterministic compilation and a versioned intermediate
  representation.
- Work through stable public Buildkite pipeline and Agent interfaces rather
  than private Rails APIs.
- Support GitHub and Cursor Origin as repository/event providers through a
  provider-neutral event and source interface.
- Provide a compatibility report suitable for both humans and automated
  migration tooling.
- Allow native Buildkite jobs to surround and incrementally replace imported
  jobs.
- Run untrusted pull-request code only under an explicit, least-privilege
  policy.

## Non-goals for the initial project

- Do not provide bug-for-bug compatibility with every GitHub Actions feature in
  the first release.
- Do not run the official Actions listener against GitHub or an emulated GitHub
  runner service.
- Do not translate Actions steps into separate Buildkite jobs.
- Do not promise that an action which directly modifies GitHub can operate
  unchanged after its repository moves to Cursor Origin.
- Do not initially support Windows or macOS workflow jobs.
- Do not initially reproduce every package and tool installed on GitHub's
  `ubuntu-latest` image.
- Do not put the compiler or runtime implementation into the Rails monolith.
- Do not require a new long-running service for the first useful release.
- Do not put unstable compatibility code directly into `buildkite-agent` before
  its interfaces and release cadence have proven stable.
- Do not store secret values in Buildkite metadata to emulate job outputs.
- Do not silently run privileged Docker workloads on agents that have not opted
  into that trust model.

## User experience

### Commands

The initial binary should expose four main commands:

```bash
# Validate syntax, capabilities, security policy, and action portability.
buildkite-gha validate .github/workflows/ci.yml

# Write deterministic Buildkite pipeline YAML to stdout.
buildkite-gha compile .github/workflows/ci.yml

# Compile, upload the pipeline, and publish immutable job plans.
buildkite-gha upload .github/workflows/ci.yml

# Internal command used by generated Buildkite jobs.
buildkite-gha run-job --plan .buildkite-gha/plans/<digest>.json
```

`--provider <name>` switches positional workflow paths to repository-relative
provider paths resolved at the attested event SHA. Without `--provider`, paths
are local files and compilation is unprivileged unless another authenticated
event source is configured.

`compile` must not mutate the current build. It is suitable for local review,
golden tests, and:

```bash
buildkite-gha compile .github/workflows/ci.yml |
  buildkite-agent pipeline upload --no-interpolation --dry-run
```

`upload` is the ergonomic in-build command. It creates per-job plan artifacts,
uploads the generated pipeline through the Buildkite Agent, and fails the
bootstrap job if either operation fails.

### Initial Buildkite pipeline

A customer can start with:

```yaml
steps:
  - label: ":github: Load existing CI workflow"
    key: "gha-ci"
    agents:
      queue: "gha-compiler"
    checkout:
      skip: true
    command: >-
      buildkite-gha upload
      --provider github
      .github/workflows/ci.yml
```

With checkout skipped, the workflow path is repository-relative and the
provider adapter reads it at the attested event SHA; it is not a path in an
agent checkout. The `gha-compiler` queue runs the pinned distribution with
local repository hooks and plugins disabled. `upload` refuses to sign plans if
it cannot attest that compiler identity and environment. A plain command step
outside this contract can run only the explicit unsigned, tokenless,
unprivileged development mode.

An existing native step can depend on the importer:

```yaml
steps:
  - label: ":github: Load existing CI workflow"
    key: "gha-ci"
    agents:
      queue: "gha-compiler"
    checkout:
      skip: true
    command: >-
      buildkite-gha upload
      --provider github
      .github/workflows/ci.yml

  - label: ":rocket: Native deployment"
    key: "deploy"
    depends_on: "gha-ci"
    command: ".buildkite/deploy.sh"
```

Buildkite's dynamic pipeline dependency rule causes a step depending on the
upload job to also wait for jobs subsequently uploaded by it. The compiler must
still emit explicit keys and `depends_on` relationships for the imported DAG;
visual insertion order is not an execution contract.

### Future first-class syntax

Once the project is proven, Buildkite could recognize an import in its pipeline
schema or expose it through the Agent:

```yaml
steps:
  - import:
      format: "github-actions"
      path: ".github/workflows/ci.yml"
      key: "legacy-ci"

  - wait

  - label: ":rocket: Native deployment"
    command: ".buildkite/deploy.sh"
```

That is a later product integration. The standalone project must not depend on
it.

## Architecture

```diagram
┌──────────────────────────┐
│ Workflow + event payload │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Parser and validator     │
│ YAML, schema, policy     │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Workflow planner         │
│ contexts, DAG, matrices  │
└───────┬──────────────────┘
        │
        ├──────────────▶ versioned per-job plans
        │
        ▼
┌──────────────────────────┐
│ Buildkite emitter        │
│ keys, deps, queues       │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Buildkite pipeline       │
└────────────┬─────────────┘
             ▼ one process per generated job
┌──────────────────────────┐
│ Actions job runtime      │
│ run / JS / Docker /      │
│ composite / services     │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Buildkite logs, results, │
│ metadata and artifacts   │
└──────────────────────────┘
```

### Project structure

Start with one repository and one released distribution:

```text
buildkite-gha/
├── cmd/buildkite-gha/
├── internal/
│   ├── action/
│   │   ├── composite/
│   │   ├── container/
│   │   ├── javascript/
│   │   └── resolver/
│   ├── buildkite/
│   ├── compiler/
│   ├── context/
│   ├── event/
│   ├── expression/
│   ├── plan/
│   ├── policy/
│   ├── runtime/
│   └── workflow/
├── schemas/
├── testdata/
│   ├── actions/
│   ├── events/
│   ├── plans/
│   ├── smoke/
│   └── workflows/
├── docs/
│   ├── compatibility.md
│   ├── security.md
│   └── architecture.md
├── go.mod
├── LICENSE
└── README.md
```

Keep packages internal until their contracts have multiple real consumers.
The versioned plan schema is the first intentional interoperability boundary.

### Workflow parser and semantic model

The parser must retain source locations and distinguish expressions from plain
strings. Validation errors should name the workflow path, line, column, logical
job, and unsupported feature.

Evaluate expressions at the phase where GitHub Actions would make the relevant
contexts available. Do not eagerly interpolate every YAML scalar. At minimum,
distinguish:

- event and workflow input evaluation;
- job graph and static matrix evaluation;
- job-start evaluation using `needs` results and outputs;
- step-start evaluation using prior `steps` results and outputs; and
- job-output evaluation after step completion.

The initial implementation should evaluate whether to reuse or fork these
`nektos/act` components:

- workflow and action models;
- matrix expansion;
- the expression interpreter built on the `actionlint` parser;
- action resolution and execution patterns; and
- workflow-command and environment-file handling.

Do not expose `act`'s execution-oriented plan or run context as the
`buildkite-gha` plan schema. Own a Buildkite-neutral intermediate
representation so upstream implementation changes do not become a product API.

Use the open GitHub Actions runner to define expected coercion, file-command,
workflow-command, action lifecycle, container, and expression behavior. Do not
depend on its listener or server message model.

### Canonical event and context snapshot

Compilation takes an explicit event envelope:

```json
{
  "provider": "github",
  "event": "pull_request",
  "repository": {
    "owner": "acme",
    "name": "widgets",
    "clone_url": "https://example.invalid/acme/widgets.git",
    "default_branch": "main"
  },
  "ref": "refs/pull/123/merge",
  "sha": "0123456789abcdef",
  "actor": "octocat",
  "payload": {}
}
```

Provider adapters construct this envelope from Buildkite build environment,
SCM metadata, an explicit `--event-path`, or a future Buildkite API. The
compiler serializes one canonical context snapshot into each job plan. The job
runtime must not independently infer a different `github.ref`, SHA, actor, or
event payload.

The snapshot must carry authenticated provenance before it can authorize
secrets, provider tokens, a privileged queue, or privileged containers. For the
initial implementation, compilation and plan signing run on a trusted compiler
queue, using a pinned verified `buildkite-gha` distribution and a signing key
that workflow code cannot access. The runtime verifies the signed envelope
against a configured trust root and independently applies current queue, event,
secret, and container policy. A digest detects accidental corruption but is not
an authenticity boundary.

If a provider later supplies a signed event envelope directly, the compiler may
preserve that provenance instead of asserting it itself. An unattested
`--event-path` is suitable for local validation and tokenless, unprivileged
execution only; it must fail closed when the requested job needs a protected
capability.

The provider interface owns:

- repository and event metadata;
- source and action reference resolution;
- authenticated clone/archive access;
- provider API token issuance; and
- provider-specific URL/context fields.

Implement GitHub first, but keep Cursor Origin behind the same interface from
the start.

The `vars` context also needs an explicit, non-secret source because it may be
used during compilation for matrices, conditions, and `runs-on`. Support a
documented precedence across bridge configuration, Buildkite pipeline/build
environment, and provider repository/organization variables. Snapshot the
resolved values into the plan so compile-time and runtime expressions agree.

### Versioned job plan

Compilation produces one immutable plan for each expanded job instance. A plan
contains no secret values and includes:

- plan schema and compiler version;
- workflow source path and content digest;
- event/context snapshot;
- logical workflow and job IDs;
- deterministic Buildkite step key;
- matrix values and strategy settings;
- dependency IDs;
- job condition, environment, defaults, timeout, and permissions;
- requested runner labels and resolved Buildkite target;
- job and service container definitions;
- ordered action steps, including source locations;
- action references and immutable resolved SHAs where available;
- names of required secrets and a snapshot of resolved non-secret variables;
- declared job outputs; and
- compatibility capabilities required by the runtime.

Plans should be canonical JSON with a content digest inside a signed envelope
that binds the plan digest, compiler version, build identity, event provenance,
workflow digest, trust classification, deterministic target step key, permitted
queue and capability ceiling, and expiry. The compile job uploads the envelopes
as namespaced Buildkite artifacts before the generated jobs become eligible.
Each generated job downloads its plan from the known compiler step, verifies
the signature, build binding, digest, expiry, schema version, and that the
envelope's step key and queue match the executing Buildkite job before resolving
any capability, then invokes `run-job`. A plan produced by an untrusted or
unverifiable compiler is never promoted into a privileged runtime merely
because its digest matches.

Do not put complete plans into command-line arguments, environment variables,
or build metadata. They can exceed operating-system and Buildkite metadata
limits and are difficult to audit safely.

### Trusted bootstrap boundary

Treat the bootstrap job as security-sensitive infrastructure because any
running Buildkite job can request a dynamic pipeline upload. The first
supported bootstrap contract is:

- run a pinned, checksummed `buildkite-gha` distribution on a dedicated trusted
  compiler queue;
- use a non-exportable plan-envelope signer, such as a narrowly scoped KMS or
  signing broker, so the parsing process can request a signature but cannot read
  signing key material;
- distribute verification-only trust roots to runtime queues and define key
  rotation and revocation as part of the installer contract;
- obtain workflow and action metadata as inert input without running repository
  hooks, plugins, generated scripts, or repository-provided binaries;
- provide no workflow-readable secrets or write-capable provider token to the
  compiler; a short-lived, least-privilege read credential may be brokered for
  private source and action resolution, must be registered with the Agent
  redactor before output, and must never enter plans, envelopes, emitted YAML,
  or diagnostics;
- emit fixed `run-job` commands whose queue, plugins, containers, and
  capabilities are selected through trusted policy rather than copied directly
  from workflow text;
- sign generated pipeline steps through the configured Buildkite signed-pipeline
  mechanism; and
- reject compilation when the bootstrap identity, event provenance, signing
  capability, or target queue policy cannot be established.

For an untrusted pull request, either fetch the workflow files through a
data-only provider adapter or use an agent configuration that disables local
repository hooks and plugins. Merely checking out the pull request and invoking
a trusted binary is insufficient if checkout can execute repository-owned code.

### Buildkite pipeline emitter

The emitter maps every expanded GHA job to an ordinary command step with:

- a deterministic, collision-resistant key;
- a human-readable label containing matrix values;
- explicit `depends_on` edges;
- an infrastructure-only dependency on the compiler job so plans are published
  before any imported job can start, expressed with per-dependency
  `allow_failure: false`;
- logical GHA dependency edges expressed with per-dependency
  `allow_failure: true` so failed prerequisites still dispatch the compatibility
  runtime;
- a queue mapping derived from `runs-on` and policy;
- timeout and concurrency properties where semantics align;
- `checkout.skip: true` so the agent does not populate the job workspace before
  the compatibility runtime starts;
- step-local environment containing only non-secret plan identifiers; and
- the installation mechanism for the same pinned `buildkite-gha` version that
  performed compilation.

Native checkout suppression requires Buildkite Agent v3.130.0 or later. The
runtime must still allocate and verify a fresh empty directory for
`GITHUB_WORKSPACE`; it must not assume that the agent command directory is empty
or suitable merely because checkout was skipped. The generated command and
runtime installation must be self-contained because repository files are not
present at job start.

Use `buildkite-agent pipeline upload --no-interpolation` so shell expressions
and dollar signs intended for runtime are not altered during pipeline upload.

Pipeline upload is limited in size and job count. The emitter must detect the
current limit, fail clearly or split large uploads without changing dependency
semantics, and never depend on the visual ordering of multiple uploads.

Buildkite concurrency groups can model queued access and `strategy.max-parallel`
for expanded matrix jobs. They do not directly implement GHA
`concurrency.cancel-in-progress`; report that as a compatibility gap until the
bridge has an explicit, safely scoped cancellation mechanism.

### Graph and result semantics

`needs` is not only a scheduling edge. The runtime needs each prerequisite's
logical result and outputs to evaluate downstream job conditions.

Every generated job is dispatched after its dependencies settle, even when a
dependency failed. The compatibility runtime is the sole authority for deciding
whether the logical GHA job runs or skips. A runtime-skipped job records an
explicit `skipped` result and exits successfully. Never rely on Buildkite to
skip an imported job on dependency failure: a scheduler-skipped job cannot
publish the result required by its downstream `needs` contexts.

Every generated job records a namespaced logical result before exiting:

```text
buildkite-gha/v1/results/<workflow>/<job-instance>
buildkite-gha/v1/outputs/<workflow>/<job-instance>/<output-name>
```

Use Buildkite build metadata only for bounded, non-secret scalar values.
Metadata is build-scoped, last-writer-wins storage and is never an integrity or
producer-attribution boundary. Every job therefore uploads a canonical result
manifest as an artifact attributed to its Buildkite step key. Imported
consumers download the manifest from that exact producer step and verify its
plan identity and digest; metadata is a query/UI convenience and a reference to
the authoritative manifest. Consumers must have explicit dependencies on
producers. Metadata has a 100 KB hard limit per value and values over 1 KB are
discouraged, so larger outputs use namespaced artifacts directly.

Values that influence a trusted decision, including continuation inputs,
native replacement validation, and retry state, must come from a
producer-attributed artifact or carry a compiler-verifiable signature. A
namespaced metadata key alone is insufficient because another job in the same
build can overwrite it. Phase 0 must verify the selected attribution mechanism
against a real build rather than assume metadata exposes writer identity.

Record matrix-instance outputs separately for auditability and maintain the
logical job output using GHA's completion-order, last-writer-wins behavior when
multiple matrix instances publish the same name. Document that retrying an
imported producer can rewrite metadata without automatically rerunning an
already-passed native or imported consumer.

Where Buildkite and GHA result models differ, prioritize correct downstream
execution and report the UI difference explicitly. In particular:

- a false runtime job condition produces a short no-op Buildkite job, because a
  running command cannot currently mark itself as scheduler-skipped;
- cancellation does not behave exactly like dependency failure in Buildkite;
- beta matrix fail-fast can prevent undispatched siblings from starting, but
  cannot cancel in-progress siblings until the bridge has a safely scoped
  cancel-own-matrix capability; and
- the Buildkite UI may show a runtime-skipped job as passed until Buildkite has
  a scheduler-visible compatibility skip result.

These are explicit compatibility gaps and potential Buildkite platform
enhancements, not reasons to silently report a different logical result.

### Dynamic graph continuation

Some graph values are unavailable during the initial compilation, notably:

```yaml
strategy:
  matrix: ${{ fromJSON(needs.prepare.outputs.matrix) }}
```

Represent these as deferred graph continuations:

1. Compile and upload the known prerequisite graph.
2. The prerequisite publishes a bounded, producer-attributed result manifest.
3. A generated continuation command downloads that manifest from the exact
   prerequisite step and verifies its plan identity.
4. The compiler expands only the newly available portion of the graph.
5. It publishes immutable plans and uploads the downstream Buildkite jobs with
   stable keys.

Continuations run on the same trusted compiler queue under the complete
bootstrap contract. Treat every prerequisite output as untrusted input and
apply explicit type, size, matrix-cardinality, and expression limits before
expansion. A continuation inherits and may only narrow the originating signed
compilation's event trust classification, permitted queues, permissions, and
capability ceiling; workflow-produced values cannot widen any of them.

Continuations and the initial upload must use an explicit retry protocol.
Buildkite rejects an upload when a step key already exists; deterministic keys
prevent duplication but do not make a retry succeed automatically.

Before uploading, record a signed expected pipeline digest and key manifest
under a namespaced metadata marker. After the upload command returns success,
record a signed completed marker with the same digest. Phase 0 must establish
whether successful multi-step uploads are atomic; the recovery protocol must
not assume atomicity until that oracle is proven. Treat unsigned or invalid
marker values as absent or tampered according to the fail-closed recovery
protocol. On retry:

- return success when a completed marker has the expected digest;
- fail when any marker has a different digest; and
- after an interrupted prepared-but-incomplete upload, verify the exact existing
  key set and embedded plan digests through a documented Buildkite query before
  treating duplicate-key rejection as success.

If the current public interfaces cannot verify that final case, fail safely and
make recovery explicit rather than using `pipeline upload --replace`, which can
remove pending work owned by another continuation. Phase 0 must prove the exact
protocol against a real partially retried build.

### Actions job runtime

`run-job` executes exactly one expanded GHA job inside exactly one Buildkite
job. Its state machine is:

1. Verify the job plan and runtime version.
2. Load producer-attributed prerequisite results and outputs, evaluate the job
   condition, and, when false, record `skipped` and exit successfully without
   resolving secrets or starting containers.
3. Resolve non-secret variables and required secrets.
4. Register every resolved secret with the Buildkite Agent redactor.
5. Create an empty GHA workspace and temporary directories.
6. Start the job container and service containers, if configured.
7. Resolve and prepare local and remote actions.
8. Execute pre-actions in required order.
9. Execute main steps, evaluating each condition at step start.
10. Process workflow commands and environment files after every step.
11. Execute post-actions in LIFO order, including after failure or cancellation
    within a bounded cleanup period.
12. Evaluate and record job outputs and the logical job result.
13. Publish step summaries and perform container/process cleanup.
14. Exit with the Buildkite result corresponding to the logical GHA result.

Generated GHA jobs should begin with an empty workspace rather than Buildkite's
automatic repository checkout. This preserves Actions behavior for workflows
that do or do not invoke `actions/checkout`. The plan artifact is sufficient to
start the runtime. Local actions become available only after the workflow has
populated the workspace, as they do in GitHub Actions.

### Step execution

Support these step/action types:

#### `run` steps

- GHA default-shell selection and shell flags;
- `defaults.run.shell` and `working-directory`;
- step and job environment precedence;
- exit-code, signal, timeout, and `continue-on-error` behavior;
- live stdout/stderr with masking before Buildkite receives it; and
- cancellation propagation to the complete process tree.

#### Parallel and background steps

Implement the shipped Actions contract for `background`, `wait`, `wait-all`,
`cancel`, and `parallel` rather than treating step concurrency as generic
process execution:

- enforce the documented maximum of 10 active background steps, with additional
  background work queued until a slot is available;
- apply a background step's outputs and `GITHUB_ENV` changes only when a
  covering wait completes;
- surface a background failure at the next covering wait;
- perform an implicit `wait-all` before post-job cleanup;
- preserve wait/cancel behavior even though wait controls do not use normal
  step `if` evaluation;
- send termination followed by forced termination after the documented grace
  period for `cancel`; and
- reject parallel/background controls inside composite actions where GitHub
  does not permit them.

Serialize workflow-command handling across concurrent streams. In particular,
a mask registered by one step must take effect before buffered output from
another step can expose the same value. Add conformance fixtures for
failure-at-wait, cancel grace, implicit wait-all, output/environment visibility
at barriers, and the cross-stream masking race.

#### JavaScript actions

- `node20` and `node24` action metadata;
- runner-compatible handling of deprecated Node versions;
- action `pre`, `main`, and `post` entry points;
- `INPUT_*`, `GITHUB_ACTION_PATH`, runtime files, and action state; and
- managed Node distributions rather than assuming the host's `node` is
  compatible.

The released project may therefore be a distribution containing the Go binary
and versioned Node runtimes rather than literally one standalone executable.

#### Composite actions

- nested step scope and `steps` context;
- typed inputs and declared outputs;
- environment and path propagation;
- nested action resolution;
- nested pre/post registration order; and
- recursion and maximum-depth limits.

#### Docker actions and job/service containers

- image pulls and Dockerfile builds;
- action entrypoint and argument behavior;
- `/github/workspace`, temp, tool-cache, and file-command path translation;
- one isolated network per job;
- service DNS aliases, ports, credentials, health checks, and failure logs; and
- reliable cleanup after failure and cancellation.

Make the execution backend an interface. Implement Linux host plus Docker
first. Do not let Docker-specific assumptions leak into the workflow compiler
or plan schema.

### Environment files and workflow commands

Implement the observable Actions contracts for:

- `GITHUB_ENV`;
- `GITHUB_OUTPUT`;
- `GITHUB_PATH`;
- `GITHUB_STATE`;
- `GITHUB_STEP_SUMMARY`;
- multiline heredoc values and CRLF/LF handling;
- `::error`, `::warning`, `::notice`, and source locations;
- `::group` and `::endgroup`;
- `::add-mask`;
- `::stop-commands`; and
- supported legacy command behavior and security restrictions.

Map warnings and errors to Buildkite log output and annotations. Map dynamic
mask values to `buildkite-agent redactor add` before subsequent output is
written. A secret printed before it is registered cannot be retroactively
protected.

Step summaries should become job-scoped Buildkite annotations when supported,
with a stable context and artifact fallback for oversized content.

Parallel step logs need explicit step identity. Prefer live prefixed output and
per-step completion summaries over buffering all output until completion.
Buildkite log groups are not assumed to support overlapping concurrent groups.

### Action resolution

Support:

- local actions such as `./.github/actions/test`;
- `owner/repository[/path]@ref`;
- `docker://image:tag`; and
- local and remote reusable workflows in later phases.

Resolve remote action references through the source-provider interface. Record
the requested ref and immutable resolved commit in the plan. Enforce archive
size limits, safe path extraction, symlink policy, recursion limits, and an
optional organization allowlist or SHA-pinning policy.

Cache downloaded action source by immutable commit and subpath, never only by a
mutable tag. A cache hit must still enforce the current build's authorization
and policy.

### Compatibility services and adapters

Actions fall into three product categories:

1. **Portable actions** operate on the workspace, execute tools, or call an
   independent external API. Run their JavaScript, container, or composite
   implementation directly.
2. **Actions-runtime integrations** depend on GitHub's cache, artifact, OIDC,
   or token services. Provide Buildkite-backed protocol services or explicit
   compatibility adapters.
3. **Provider-dependent actions** modify GitHub repositories, pull requests,
   checks, issues, or releases. They can use a scoped GitHub token while GitHub
   is the repository provider. Under Cursor Origin they require an Origin
   equivalent or must be migrated.

The validator must classify known and detected actions accordingly.

Prioritize compatibility for commonly used official actions:

- `actions/checkout`;
- `actions/cache`;
- `actions/upload-artifact`;
- `actions/download-artifact`;
- `actions/setup-node`; and
- representative language/tool setup actions.

For Cursor Origin, `checkout` should be implemented through provider-neutral
repository metadata while preserving the action's documented inputs and
outputs. Cache and artifact support should use Buildkite storage semantics
without exposing GitHub's private service credentials.

### Triggers and workflow `on`

`buildkite-gha` does not itself listen for repository events. A Buildkite
pipeline, API caller, or future Origin integration creates the build.

The compiler uses `on` to validate that the supplied event should run the
workflow and to reproduce branch/path/input filtering. Event adapters must
cover, in phases:

- `push`;
- `pull_request`;
- `workflow_dispatch`-style manual input;
- `schedule` mapped to a Buildkite schedule;
- `workflow_call`; and
- additional repository events as provider integrations mature.

The plan must not invent a GitHub event payload from incomplete environment
variables. If required event fields are unavailable, validation fails with the
missing fields and the required adapter capability.

### Secrets, permissions, tokens, and untrusted changes

Treat workflow compilation and job execution as separate trust boundaries.

- The compiler can inspect untrusted workflow text as data but cannot execute
  repository-owned hooks, plugins, scripts, or binaries and cannot resolve
  secret values.
- The compiler identity and canonical event snapshot are authenticated and
  bound into every signed job-plan envelope.
- The generated plan declares required secret names and permissions.
- The runtime verifies plan provenance, then asks a Buildkite secret provider
  for only the values independently permitted to that event and job.
- Fork pull requests receive no privileged secrets by default.
- An untrusted event can target only queues explicitly configured for untrusted
  code; workflow-controlled `runs-on` values cannot select a privileged queue.
- Provider API tokens are short-lived and least-privilege.
- Reusable workflows can only narrow inherited permissions.
- Every secret is registered with the Agent redactor before use.
- Job outputs containing registered secret literals or explicitly supported
  standard encodings are rejected rather than published; general secret taint
  tracking is out of scope.
- OIDC uses a Buildkite-issued identity and documented migration guidance; it
  must not pretend to have GitHub issuer or subject claims.

Policy evaluation occurs before pipeline upload and again before runtime secret
or privileged container access. Runtime enforcement is required because a plan
artifact may have been produced by an older compiler or a less-trusted agent.
Runtime policy must use verified envelope claims and trusted local
configuration; it must never treat an unsigned plan's self-declared event or
trust classification as authority.

When GitHub is the repository provider, populate `github.token` and
`GITHUB_TOKEN` only from an explicitly configured customer secret or a
short-lived GitHub App installation token permitted by the event trust policy.
Do not invent a token or silently grant write access. Validation must identify
actions and expressions requiring a provider token when none is configured.

### Installation and release model

Publish signed, checksummed releases with SBOMs. The initial supported
distribution is Linux x86-64; Linux arm64 can follow once action/runtime
compatibility is measured.

Hosted Agents should eventually include the bridge distribution and managed
Node runtimes. Customer-managed agents can install a pinned version through a
small Buildkite plugin. Every generated job must execute the same bridge version
that produced its plan unless the plan schema explicitly permits a compatible
newer runtime.

Plan-envelope validity must cover the maximum supported build queueing and
manual-retry window. Once an envelope expires, a job fails with a diagnostic
that directs the operator to start a new build; an old build cannot silently
refresh its own authority by retrying an untrusted job.

Do not download an unpinned latest binary separately in every generated job.

Document a minimum cancellation grace period for post-actions and container
cleanup. Warn when a customer-managed agent exposes a shorter grace period and
the runtime can detect it; otherwise make the requirement part of installation
validation.

Phase 0 must also verify dynamic compilation under Buildkite signed pipelines.
If generated pipelines require agent-side signing or another trust handoff,
make that part of the installer and bootstrap contract rather than asking
security-conscious customers to disable pipeline signing.

## Native Buildkite interoperability

### Stable keys and group boundaries

Use a documented key scheme derived from:

- import key;
- qualified workflow path;
- reusable-workflow namespace;
- logical job ID; and
- canonical matrix values.

Hash only the portion needed to remain under Buildkite key limits; preserve a
human-readable prefix. Publish a compile manifest mapping GHA identities to
Buildkite keys.

Native jobs can initially depend on the import step or imported workflow group.
Depending on one matrix instance requires the manifest or a deterministic
documented key.

### Job replacement

Add a migration configuration after basic compatibility is stable:

```yaml
version: 1

workflows:
  ci:
    path: .github/workflows/ci.yml
    replace_jobs:
      deploy:
        step: .buildkite/steps/deploy.yml
```

The compiler replaces the named GHA job with a native Buildkite command step,
retains its logical key and dependencies, and validates its declared output
contract. Start with non-matrix jobs and whole-job replacement.

Do not initially support arbitrary mixing of native Buildkite plugins and GHA
steps inside the same imported job. Job-level replacement keeps workspace,
hooks, cancellation, and post-action ownership comprehensible.

### Output interoperability

Document the namespaced metadata contract so native jobs can read imported job
outputs. Later, allow a native replacement job to publish declared outputs
through a helper:

```bash
buildkite-gha output set image-tag "$IMAGE_TAG"
```

The helper validates output name and size, refuses known secret values, and
writes the producer-attributed result manifest plus its namespaced metadata
mirror for downstream imported jobs.

## Compatibility levels

The validator and documentation should report a workflow against explicit
levels:

| Level | Meaning |
| --- | --- |
| Compile | Workflow and graph can be translated safely |
| Execute | Required shells, actions, and containers can run on the selected queue |
| Services | Cache, artifact, token, or OIDC dependencies have a Buildkite adapter |
| Provider | Repository-provider API behavior is available |
| Portable | Workflow can move from GitHub to Cursor Origin without provider-specific actions |

Example output:

```text
Workflow: .github/workflows/ci.yml
Result: executable on Buildkite, partially provider-dependent

✓ 6 jobs and 14 static matrix instances compile
✓ JavaScript, composite, and Docker action capabilities are available
✓ actions/cache@v4 uses the Buildkite cache adapter
! release job calls api.github.com and will not be portable to Cursor Origin
✗ windows-latest is not supported by queue mapping "hosted-linux"
```

The command exits non-zero for execution blockers. Warnings and portability
findings are machine-readable through `--format json`.

## Initial support target

The first externally useful beta should support:

- Linux x86-64 on a compatible Hosted Agent image;
- `push`, `pull_request`, and manual-input event envelopes;
- `env`, `defaults`, `needs`, job and step `if`, timeouts, and bounded outputs;
- static matrices with `include`, `exclude`, and `max-parallel`, plus
  best-effort `fail-fast` that prevents undispatched siblings from starting;
- shell `run` steps;
- `background`, `wait`, `wait-all`, `cancel`, and `parallel` step controls;
- JavaScript actions using managed Node 20/24;
- composite actions;
- Docker actions, job containers, and service containers;
- environment files and workflow commands;
- local and public remote actions resolved to immutable commits;
- statically resolvable local reusable workflows;
- checkout, cache, and artifact compatibility;
- Buildkite secrets with default-deny fork policy;
- job summaries, warnings, errors, and live masked logs; and
- native jobs after the imported workflow.

Explicitly defer from beta unless implementation evidence changes the order:

- Windows and macOS;
- exact GitHub-hosted image parity;
- GitHub Enterprise Server;
- all repository event types;
- remote private reusable workflows;
- dynamic matrices and other runtime graph generation;
- job-level replacement;
- deployment environments and approval parity;
- OIDC migration automation;
- provider API compatibility for arbitrary GitHub-mutating actions; and
- a first-class Buildkite pipeline import schema.

## Delivery plan

### Current progress

- The repository baseline, reviewed architecture plan, and staged
  `testdata/smoke` corpus are committed on `main`.
- Phase 0 implementation is active on `lox/phase-0`.
- The first work wave is integrated: the Go/CLI foundation is runnable,
  ADR 0001 records the actionlint/act reuse boundary, and ADR 0002 plus schemas
  and eight conformance cases define the signed plan-envelope trust contract.
- The second work wave is integrated: actionlint is isolated behind owned
  workflow and expression models, the compiler emits deterministic static IR
  for the smoke corpus, the differential harness materializes isolated Git
  repositories and compares normalized observations, and the local action
  runtime proves JavaScript, composite, and Docker lifecycle behavior with
  masked output and post-actions.
- The third work wave is integrated: compiler-selected v1 job plans execute
  through `run-job`, the shell differential oracle has checked-in GitHub
  Actions and Buildkite definitions, and the transport/trust package plus
  dormant probe cover immutable artifacts, dependency policy, producer
  attribution, signed bindings and markers, and fail-closed upload recovery.
- All local tests, race tests, vet, schema fixtures, shell checks, and offline
  pipeline validation pass. Phase 0 is not complete: the managed internal
  GitHub repository and Buildkite pipeline do not exist yet, so neither hosted
  differential execution nor the real transport, signing, interruption, and
  dependency-extension oracles have run.

Phase 0 spike support snapshot:

| Boundary | Proven locally | Explicit gap or live gate |
| --- | --- | --- |
| Compile | Actionlint-backed owned model, deterministic static graph and matrix expansion, source spans, stable keys, versioned owned job plans | Dynamic graph expressions, matrix `include`/`exclude`, reusable-workflow jobs, and expression-derived `runs-on` or `needs` fail closed |
| Execute | Bash/sh steps and local Node 24, composite, and Dockerfile actions; outputs, environment, state, summaries, masking, failure results, and LIFO post-actions | Remote actions, nested composite actions, conditions, services/job containers, timeouts, cancellation, `continue-on-error`, and non-success dependency semantics fail closed |
| Differential | Isolated committed fixture, canonical capture/comparison, and offline-validated GitHub Actions and Buildkite definitions | Neither hosted provider definition has run against the same committed revision |
| Transport | Content-addressed plans and bindings, deterministic two-job upload, strict compiler edges, failure-settling logical edges, producer-bound manifests, signed markers, and exact command capture | Real artifact ordering/selection, metadata visibility, upload atomicity, importer dependency extension, and per-dependency failure behavior require a live build |
| Trust | Eight signed-envelope conformance cases plus signature-first runtime binding checks for build, step, queue, event, plan, and capabilities | KMS-backed signing, queue verification roots, hook/plugin isolation, and signed-pipeline rejection require the intended live queues |
| Recovery | Ambiguous, partial, conflicting, or unattested interrupted uploads fail closed | Current Buildkite APIs expose signature material but no authoritative verification verdict, so the supported behavior is operator cancel/rebuild until a live oracle proves a narrower recovery path |

### Phase 0 — Prove the semantic foundation

Create the standalone repository, license, command skeleton, architecture ADR,
and compatibility test harness.

Seed the harness with the checked-in `testdata/smoke` repository fixture. Keep
its three workflows staged: `shell.yml` for the sequential runtime, `ci.yml` for
the first action-runtime product slice, and `artifact.yml` for later
GitHub-artifact adapter compatibility. Phase 0 establishes how the harness
materializes this directory as a repository and captures normalized
observations; it does not require every workflow to execute immediately.

Build four spikes before committing to the runtime implementation:

1. Parse and compile representative workflows using selected `act` and
   `actionlint` components into an owned intermediate representation.
2. Run one JavaScript, one composite, and one Docker action inside a Buildkite
   job while preserving outputs, masks, environment changes, and post-actions.
3. Run equivalent fixture workflows on GitHub Actions and Buildkite and compare
   normalized observations.
4. Exercise the complete transport on a real Buildkite build: upload immutable
   plan artifacts, dynamically upload two dependent jobs, download and verify
   plans by compiler step, publish and consume producer-attributed result
   manifests, verify per-dependency failure handling keeps the compiler edge
   strict while logical `needs` edges settle on failure, confirm that a native
   step depending on the importer waits for uploaded work, determine whether an
   upload is atomic, and retry the bootstrap after an intentionally interrupted
   upload.

The transport spike must also cross the intended trust boundary: compile an
untrusted fixture as inert data, sign its event and plan envelope on the trusted
compiler queue, reject tampered and unattested envelopes at runtime, and prove
that repository hooks or plugins cannot run in the bootstrap job.

Use the spikes to decide which `act` packages can be imported, which need a
maintained fork, and which semantics should be implemented independently. Keep
MIT attribution for copied or modified source and audit transitive and bundled
runtime licenses.

Definition of done:

- The owned plan schema is documented and versioned.
- Compiler and runtime boundaries are proven without a GitHub runner service.
- The action runtime can stream already-masked output into a Buildkite job.
- Plan artifact ordering, dynamic dependency extension, metadata visibility,
  producer attribution, and Agent redaction are proven in a real build.
- Interrupted-upload recovery is proven in a real build when a documented
  verification query exists; otherwise the fail-closed path and explicit
  operator recovery are proven and recorded as the supported behavior.
- The bootstrap and plan-envelope trust chain is documented and proven with
  tampering, replay, expiry, wrong-build, wrong-job, wrong-queue, and
  untrusted-event fixtures.
- The dynamic upload path has a documented answer for signed pipelines.
- The project has a written reuse/fork decision for `act`.
- Major semantic gaps discovered by the spikes are reflected in the support
  table rather than hidden.

### Phase 1 — Validator and static compiler

Implement:

- workflow YAML and action metadata parsing with source locations;
- initial event/context construction;
- expression evaluation for compile-time contexts;
- static matrix expansion;
- static local reusable workflow expansion with cycle and depth limits;
- exact `needs` DAG construction;
- deterministic keys and labels;
- `runs-on` policy mapping, including a fail-closed untrusted-event queue
  allowlist;
- explicit `vars` context construction and snapshotting;
- versioned per-job plan generation;
- Buildkite pipeline YAML output; and
- text and JSON compatibility reports.

Reject runtime-dependent graph expressions and unsupported operating systems
with actionable diagnostics.

Compilation is deterministic for identical workflow content, event snapshot,
resolved `vars`, policy/configuration, and immutable action-resolution results.
Golden tests provide all resolution results through pinned offline fixtures so
mutable tags or network state cannot change expected output.

Definition of done:

- Golden tests prove deterministic plans and pipeline output.
- Matrix include/exclude and dependency expansion match the conformance
  fixtures.
- All three `testdata/smoke` workflows compile from the checked-in push event;
  `shell.yml` expands its two logical jobs into one producer and two consumer
  matrix instances with stable keys.
- `compile | buildkite-agent pipeline upload --dry-run --no-interpolation`
  succeeds for supported fixtures.
- No compiler output contains resolved secret values.

### Phase 2 — Sequential shell job runtime

Implement the per-job state machine for `run` steps:

- workspace and temporary-directory setup;
- context/environment precedence;
- supported shells and working directories;
- step conditions and status functions;
- environment, output, path, state, and summary files;
- workflow command parsing;
- secret registration and masking;
- timeout and cancellation propagation;
- `continue-on-error`; and
- producer-attributed job output and logical-result manifests, with metadata
  mirrors for query and UI convenience.

Definition of done:

- `testdata/smoke/.github/workflows/shell.yml` compiles and runs as a native
  Buildkite build.
- Downstream conditions can consume bounded prerequisite results and outputs.
- Cleanup runs after failure and cancellation within a documented grace period.
- Secret fixtures never appear in captured raw Buildkite logs.

### Phase 3 — Concurrent step runtime

Extend the shell state machine with the shipped GitHub Actions concurrency
contract:

- `background`, `wait`, `wait-all`, `cancel`, and `parallel` parsing and
  execution;
- a bounded supervisor enforcing the ten-active-background-step limit;
- barrier-scoped visibility for outputs, environment changes, paths, and
  failures;
- implicit `wait-all` before post-job cleanup;
- graceful cancellation followed by forced process-tree termination after the
  documented grace period;
- serialized workflow-command and mask registration across concurrent output
  streams.

Definition of done:

- Differential fixtures match GitHub Actions for failure-at-wait, targeted and
  full barriers, queued background work beyond the tenth active background
  step, cancel grace, and implicit cleanup.
- Outputs and environment mutations are invisible before their covering barrier
  and visible afterwards.
- A mask registered by one concurrent step is applied before buffered output
  from another step can expose the same value.
- Cancellation reliably terminates complete process trees without skipping
  bounded post-job cleanup.

### Phase 4 — JavaScript and composite actions

Implement:

- managed Node runtimes;
- immutable remote action resolution and safe extraction;
- local actions;
- JavaScript pre/main/post lifecycle;
- action inputs, outputs, and state;
- composite action scopes and nested lifecycle; and
- rejection of background controls declared inside composite actions, while
  permitting an eligible composite action step itself to run in the background;
- action source caching by immutable identity.

Use representative popular setup actions in the compatibility suite, not only
synthetic fixtures.

Definition of done:

- `testdata/smoke/.github/workflows/ci.yml` runs on GitHub Actions and
  `buildkite-gha` with the same checked-in output observation and normalized
  lifecycle events.
- Tokenless `actions/checkout` against a public repository and at least two
  portable setup actions run successfully; verify the tokenless behavior against
  the current action as a Phase 4 entry oracle.
- Nested composite outputs and post-actions match differential fixtures.
- A composite action can run as a background step, while a composite declaring
  an internal background control is rejected with a source-located diagnostic.
- Mutable action tags are recorded with their resolved commits.
- Private action authentication is either implemented safely or rejected
  clearly.

### Phase 5 — Containers and services

Implement the Linux Docker execution backend for:

- Docker actions;
- job containers;
- service containers;
- network aliases and ports;
- health checks and service failure logs;
- host/container path translation; and
- orphan cleanup.

Definition of done:

- Container and host-job fixtures observe GHA-compatible workspace paths and
  service connectivity.
- Failed health checks produce useful Buildkite diagnostics.
- Cancellation and agent loss do not routinely leave containers or networks.
- Privileged options are subject to explicit queue policy.

### Phase 6 — Buildkite-backed core services

Provide compatibility for the most common service-backed actions:

- cache restore/save;
- artifact upload/download;
- checkout under GitHub and Cursor Origin provider adapters;
- step summaries and annotations; and
- scoped repository-provider tokens.

Prefer documented Buildkite storage and Agent interfaces. If an action toolkit
requires an HTTP protocol, run a job-local compatibility endpoint or provide a
well-defined adapter rather than proxying GitHub's private service.

Definition of done:

- `testdata/smoke/.github/workflows/artifact.yml` uploads one file in its
  producer and verifies the same contents in both consumer matrix instances on
  GitHub Actions and `buildkite-gha`.
- Common official cache and artifact actions work without a GitHub Actions run.
- `actions/checkout` against a private repository works with an explicitly
  configured, least-privilege provider token.
- Artifact and cache keys cannot cross organization/build trust boundaries
  unexpectedly.
- Cursor Origin checkout does not require a GitHub repository mirror.
- The validator distinguishes portable and provider-dependent actions.

### Phase 7 — Dynamic graph and reusable workflows

Implement:

- deferred matrix expansion from prerequisite outputs;
- idempotent continuation uploads;
- remote reusable workflows resolved to immutable commits;
- typed inputs and outputs;
- secret mapping/inheritance and permission narrowing; and
- cycle, depth, file-size, and total-job limits.

Definition of done:

- Retrying a continuation cannot duplicate or silently alter the graph.
- Caller/callee dependencies, outputs, and source provenance are preserved.
- Compilation fails safely when limits or permission boundaries are exceeded.

### Phase 8 — Native migration features

Implement:

- stable compile manifests;
- native dependency documentation and helpers;
- the output publishing helper for native jobs;
- whole-job replacement configuration;
- rewiring of dependencies and output contracts; and
- migration diagnostics suggesting jobs that are easiest to replace.

Definition of done:

- A fixture workflow can replace one non-matrix GHA job with a native Buildkite
  job without changing downstream imported jobs.
- Missing or incompatible native outputs fail validation before execution.
- A pipeline can contain native jobs before, after, and between imported jobs
  using explicit dependencies.

### Phase 9 — Hardening and product integration

Complete:

- threat modeling and external security review;
- untrusted fork and privileged-container policies;
- archive traversal, symlink, command injection, and resource-exhaustion tests;
- signed releases, checksums, provenance, and SBOMs;
- Hosted Agent image integration;
- the installer Buildkite plugin;
- metrics for compile time, runtime overhead, action compatibility, and failures;
- user-facing compatibility documentation; and
- proposals for any required Buildkite platform additions.

Only after the Linux beta is reliable, assess Linux arm64, macOS, and Windows as
separate compatibility projects with their own host, shell, path, and container
semantics.

## Test strategy

### Initial checked-in corpus

Start with `testdata/smoke` rather than an external workflow catalog:

- `shell.yml` proves the graph, static matrix, shell runtime, bounded job output,
  and downstream `needs` consumption;
- `ci.yml` adds a pinned checkout action plus repository-owned JavaScript and
  composite actions, including masking, state, summaries, post-actions, and
  environment propagation; and
- `artifact.yml` is dormant until Phase 6, when it proves upload/download
  compatibility and content preservation across jobs.

The fixture owns its event input, local actions, and expected observations. All
external actions use immutable commits. Add narrow repository-owned regression
fixtures as behavior is implemented; do not introduce a general external-canary
manifest until this corpus runs reliably.

### Unit and golden tests

- YAML schema and source-location diagnostics;
- expression parsing, coercion, truthiness, wildcard access, and status
  functions;
- event context construction;
- matrix products, include/exclude, empty matrices, and stable keys;
- workflow command and environment-file parsing with LF/CRLF and malformed
  delimiters;
- deterministic canonical plan serialization; and
- deterministic Buildkite pipeline emission.

### Runtime conformance tests

Create small fixture actions and workflows covering:

- shell exit and signal behavior;
- environment and output propagation;
- pre/main/post ordering;
- nested composite state;
- action and step conditions;
- masking and `stop-commands`;
- parallel/background steps sharing one workspace;
- job and service containers;
- cancellation and cleanup; and
- cache/artifact isolation.

### Differential tests

Maintain self-contained fixture directories that the harness can materialize as
repositories and run through GitHub Actions and `buildkite-gha`. Each workflow
writes a normalized observation document containing contexts, outputs, file
state, action order, exit results, and service connectivity. Compare documents
while excluding intentionally provider-specific fields.

Differential tests are evidence, not an excuse to copy GitHub's private
protocol. When behavior differs, decide whether to fix, adapt, or document the
gap.

### Popular-action canaries

Continuously test pinned versions of common actions across their supported
inputs. Include checkout, cache, artifacts, setup actions, Docker actions, and
composite actions. Record the immutable action commit and bridge version for
every result. Start this lane only after the checked-in smoke corpus is reliable;
rolling upstream branch heads are advisory discovery, never a required gate.

### End-to-end Buildkite tests

Run real builds that exercise:

- compilation and dynamic upload;
- isolated jobs on different agents;
- result/output metadata;
- artifacts;
- native downstream jobs;
- retry and cancellation; and
- untrusted pull-request policy.

The full Buildkite path must be tested; a local Docker harness cannot prove
dynamic pipeline, Agent redaction, artifact, metadata, or cancellation behavior.

### Fuzzing and security tests

Fuzz expression inputs, workflow commands, environment files, YAML aliases,
archive extraction, and action metadata. Add regression fixtures for every
security-relevant parser or masking issue.

## Observability

Emit structured diagnostics without secret or token values:

- compiler and plan schema version;
- workflow and plan digests;
- compile phase durations;
- number of logical and expanded jobs;
- action type and immutable identity;
- action resolution/cache durations;
- runtime phase and step durations;
- compatibility warnings by stable code;
- container/service startup durations;
- cancellation and cleanup outcome; and
- logical result versus Buildkite result when they differ.

Use stable diagnostic codes so support documentation and migration tooling can
link directly to explanations.

## Security model

The project executes arbitrary third-party code. Its threat model must cover:

- workflow code from untrusted pull requests;
- mutable or compromised action tags;
- malicious action archives, paths, and symlinks;
- workflow-command injection;
- attempts to bypass masking;
- secret access from fork builds;
- action caches crossing trust boundaries;
- Docker socket and privileged-container access;
- service-container attacks on the host or neighboring jobs;
- provider token over-scoping;
- artifact poisoning;
- expression and YAML resource exhaustion; and
- cleanup after cancellation or agent termination.

Security defaults:

- resolve mutable action references to immutable commits per build;
- allow organizations to require explicit SHA pins or action allowlists;
- deny privileged containers unless the selected queue opts in;
- deny protected secrets and write tokens to untrusted fork workflows;
- place hard limits on workflow size, expression depth, archive size, reusable
  workflow depth, total jobs, matrix size, output size, and log command size;
- add masks before exposing values to child processes where possible;
- never enable shell tracing around secrets; and
- fail closed when event trust or permissions cannot be established.

## Buildkite platform follow-ups

The standalone project should work before these exist, but production-quality
compatibility may justify Buildkite additions:

- a first-class pipeline import/preprocessor interface;
- complete provider event payload access through a scoped job API;
- a scheduler-visible way for a running compatibility job to conclude as
  skipped;
- richer logical sub-step/timeline presentation inside one command job;
- targeted matrix-sibling cancellation;
- Buildkite-backed cache APIs suitable for Actions toolkit adapters;
- short-lived provider token brokering;
- Origin event/context integration;
- imported-job and native-job output contracts; and
- UI treatment for generated/imported pipelines and compatibility findings.

Each addition should be proposed only after the standalone implementation
proves that a public Agent or pipeline contract cannot provide the required
behavior safely.

## Release gates

### Private alpha

- Shell, JavaScript, and composite fixtures pass on Linux Hosted Agents.
- The validator catches unsupported containers, platforms, and event fields.
- Logs and outputs are native Buildkite data.
- No GitHub Actions run or private runner protocol is involved.
- Secret and untrusted-fork tests pass.

### Public beta

- The initial support target is complete.
- Checkout, cache, artifact, Docker, and services canaries are reliable.
- Differential tests cover the documented expression and lifecycle surface.
- Installation is pinned, signed, and repeatable for Hosted and self-hosted
  agents.
- Compatibility documentation names known gaps and provider dependencies.
- At least several real customer workflows have been migrated without
  workflow-specific code in the runtime.

### General availability

- Compatibility regressions are release-blocking and covered by fixtures.
- Security review findings are resolved or explicitly accepted.
- Dynamic graph and reusable workflow behavior is reliable for the supported
  subset.
- Upgrade and rollback preserve plan/runtime version compatibility.
- Operational SLOs and support ownership are defined.
- Native job replacement has proven a useful migration path rather than only a
  theoretical extension.

## Key risks

### Compatibility scope grows without bound

GitHub Actions combines a workflow language, expression language, runner
runtime, hosted images, service protocols, and repository APIs. Manage scope
through explicit compatibility levels, differential fixtures, and a Linux-first
support contract rather than claiming universal compatibility.

### Reusing `act` creates upstream coupling

`act` provides the fastest Go starting point but is optimized for local
container execution, not Buildkite compilation and hosted-agent fidelity. Own
the plan, policy, Buildkite emitter, and compatibility suite. Import or fork
only components whose boundaries remain useful.

### Buildkite and GHA result models differ

Skipped jobs, dependency cancellation, fail-fast matrices, and runtime job
conditions do not all map exactly. Preserve the logical result in the bridge
contract, make downstream execution correct, surface UI differences, and
propose small Buildkite primitives where evidence warrants them.

### Existing workflows assume GitHub-hosted images

Many workflows rely on undocumented preinstalled tools rather than Actions
syntax. Publish an image compatibility contract and validator warnings. Do not
confuse action-runtime compatibility with `ubuntu-latest` image parity.

### Provider-dependent actions block Origin migration

An action that calls GitHub's PR, Checks, Issues, or Releases API cannot become
provider-neutral merely by running on Buildkite. Detect it where possible,
classify known actions, and provide explicit Origin-native migration guidance.

### Secret masking is not secret taint tracking

Buildkite redaction can hide registered literal values but may not hide every
encoded or transformed derivative. Match GitHub's command behavior, register
values before output, avoid logging sensitive contexts, and document the
remaining limitation.

### Compiler and runtime versions drift

Pin one release across compilation and execution, include schema/runtime
requirements in every plan, and fail clearly rather than interpreting an
unknown plan.

## Key learnings from pressure-testing

- A content digest does not establish who produced a plan. The design now uses
  authenticated event provenance and a signed, build-bound plan envelope before
  any runtime can access protected capabilities.
- The bootstrap job is part of the trusted computing base because it can upload
  executable pipeline steps. The plan now forbids repository-owned code during
  compilation and requires a pinned compiler, isolated signing capability, and
  fail-closed queue policy.
- Concurrent Actions steps require their own supervisor and conformance surface;
  they are now an explicit delivery phase rather than an unowned beta promise.
- Skipping Buildkite checkout is necessary but does not itself create a clean
  Actions workspace. Generated jobs now require native checkout suppression and
  a freshly allocated runtime workspace.
- Static local reusable workflows belong to the compiler phase; the later
  dynamic-graph phase now owns only runtime-dependent and remote workflow work.

## Resolved decisions before Phase 0

1. Canonical plans cross job and agent boundaries only inside signed envelopes
   that bind their provenance, build, workflow, compiler, target step and queue,
   trust classification, capability ceiling, and expiry. A digest alone is
   insufficient.
2. The bootstrap is a trusted, data-only compiler job. It runs a pinned verified
   distribution, executes no repository-owned code, exposes no workflow-readable
   secret, and can request plan signatures without reading signing key material.
3. Signed dynamic upload is required for any supported configuration that can
   reach secrets, provider tokens, trusted queues, or privileged containers.
   Unsigned local and tokenless evaluation remains an explicitly unprivileged
   development mode.
4. Generated compatibility jobs set `checkout.skip: true`, require Buildkite
   Agent v3.130.0 or later, and allocate a fresh `GITHUB_WORKSPACE` themselves.
5. Parallel and background steps remain in the beta target and are implemented
   in a dedicated phase after the sequential shell runtime.
6. Static local reusable workflows are compiled in Phase 1. Phase 7 adds dynamic
   matrices and remote reusable workflows without reimplementing the local
   compiler path.
7. Import actionlint v1.7.12 unchanged as the syntax frontend and immediately
   adapt it into owned models. Keep act v0.2.89 and Actions runner v2.336.0 as
   pinned behavioral oracles; do not import or fork act for production.
8. Plan envelopes use detached ES256 JWS over RFC 8785 canonical claims with a
   dedicated non-exportable AWS KMS P-256 signer and verification-only JWKS on
   runtime queues. This trust domain remains separate from Buildkite pipeline
   signing.

## Decisions required during Phase 0

1. What event payload can Buildkite expose today for GitHub push and pull
   request builds, and what minimal platform API is missing?
2. Should supported GHA jobs run directly on the Hosted Agent VM by default, or
   inside a compatibility image? Which choice best matches expected tool-cache
   and Docker behavior?
3. How will the runtime distribution provide Node 20/24 consistently across
   hosted and self-hosted agents?
4. Can common cache and artifact actions be supported by a job-local protocol
   adapter, or should the runtime recognize those actions explicitly?
5. How should a runtime-skipped imported job appear in the Buildkite UI before
   a scheduler-visible skip API exists?
6. Which GitHub Actions event and expression subset defines the first customer
   beta rather than only the technical alpha?
7. What is the initial Cursor Origin provider contract for checkout, event
   payloads, pull requests, and short-lived tokens?
8. Which queue capabilities and trust properties are required before the
   compiler may schedule Docker or privileged workloads?
9. What is the initial source and permission model for `github.token` and
    `GITHUB_TOKEN`: a customer-supplied secret, a short-lived GitHub App token,
    or an explicitly tokenless workflow?
10. What is the source and precedence model for repository, organization, and
    environment values exposed through the non-secret `vars` context?
11. Which documented Buildkite query can verify the exact key and plan-digest
    set after an upload is interrupted between pipeline creation and completion
    marker publication?

## Recommended first product milestone

Do not begin by implementing every workflow keyword. Build a vertical slice
that proves the product boundary:

1. A Buildkite bootstrap job runs `buildkite-gha upload` on
   `testdata/smoke/.github/workflows/ci.yml` materialized as the fixture
   repository.
2. The compiler emits two native Buildkite jobs with a dependency between them.
3. The first job runs a shell step, a JavaScript action, and a composite action.
4. It publishes a bounded output, which the second job consumes through its
   producer-attributed result manifest.
5. The runtime separately uploads and downloads a Buildkite-native diagnostic
   artifact to prove job transport; GHA artifact-action compatibility remains a
   Phase 6 deliverable.
6. Both jobs stream masked logs and display action warnings and summaries in
   Buildkite.
7. A native Buildkite job depends on the imported workflow and succeeds.
8. The same `ci.yml` fixture runs on GitHub Actions and produces the checked-in
   output observation plus equivalent normalized log and action-lifecycle
   events; the Buildkite-native transport artifact is excluded from differential
   comparison.
9. Generated jobs skip checkout, create fresh workspaces, and reject a plan
   envelope that is tampered, replayed, expired, or bound to another build.

This product milestone draws narrow slices from Phases 0, 1, 2, and 4; it does
not replace or waive the complete definition of done for any phase. It tests
compilation, execution, isolation, logs, outputs, Buildkite artifact transport,
action lifecycle, and native composition without depending on a long-running
service or GitHub's runner control plane. It is the smallest integration slice
that demonstrates `buildkite-gha` is a migration path rather than only a YAML
converter.
