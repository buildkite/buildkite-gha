# `buildkite-gha`: GitHub Actions compatibility for Buildkite

Status: **Active**
Date: 2026-07-22
Last reviewed: 2026-07-27
Target repository: `buildkite/buildkite-gha`

> This is the active product and implementation plan. It records future UX,
> implementation detail, delivery evidence, and deferred decisions together.
> For behavior users can rely on today, start with the [README](../../README.md)
> and [compatibility guide](../compatibility.md). A planned example is not a
> support promise until it is reflected in those user-facing docs.

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

### Event identity and capability issuance are the security boundary

GitHub Actions does not isolate `run` and `uses` steps from each other inside a
job. Both execute arbitrary code with the authority available to that job. Its
meaningful security boundary is the GitHub control plane, which authenticates
the repository event and decides which job-scoped tokens, secrets, OIDC claims,
environments, and runners that event may use.

The corresponding host-isolation boundary is the disposable job environment,
normally a per-job VM with its own local Docker daemon. Shell steps, JavaScript
actions, Docker actions, job containers, and service containers inside that
environment share one job-level authority. The compatibility runtime must not
claim to sandbox one workflow step from another with mount-path pinning, a
Docker invocation broker, or shared-host PID namespace tricks. If workflow code
requires isolation from other jobs or the agent host, the selected queue must
provide that isolation for the whole job. A self-hosted queue that shares a host
or Docker daemon across trust domains is outside this tokenless threat model.

Buildkite dynamic pipeline upload is likewise ordinary pipeline authority, not
a weaker class of job merely because the pipeline was generated. Public,
anonymous, tokenless actions may run on an ambient-clean fixed hosted queue
without a privileged compiler, plan signer, or supporting service. They have no
more authority than a shell step that downloads and executes public code.

Protected capabilities are different. A plan may request a GitHub token,
private source, secret, environment grant, privileged queue, or compatible OIDC
identity, but it cannot authorize that request with its own event fields. A
future control-plane service authenticates the requesting job with Buildkite Job
OIDC, verifies provider provenance and customer policy, and issues a narrow,
short-lived capability grant. Compilation and plan transport remain separate
from that authorization decision.

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

This section preserves the intended product experience, including future
syntax and migration features. The implemented command surface and current
examples are documented in the [README](../../README.md) and [compatibility
guide](../compatibility.md); implementation mechanics remain in this plan and
the architecture records.

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
are local files and the supplied event is unattested. An unattested event can
drive tokenless execution on an allowed queue, but cannot authorize a protected
capability.

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

A tokenless customer can start with an ordinary dynamic upload step. This
example assumes the workflow and event snapshot have been materialized as
inert local inputs:

```yaml
steps:
  - label: ":github: Load existing CI workflow"
    key: "gha-ci"
    agents:
      queue: "hosted"
    command: >-
      buildkite-gha upload
      --event-path .buildkite/events/current.json
      --runtime-queue hosted
      .github/workflows/ci.yml
```

This importer is authoritative in the same sense as any other Buildkite dynamic
pipeline generator. It should use a pinned distribution, fixed queue policy,
bounded inert inputs, and generated jobs without ambient protected credentials.
It does not need a signer merely to upload tokenless jobs. If the workflow asks
for a protected capability, `upload` fails unless the installation has a valid
control-plane grant for that exact build and plan.

A future provider-backed mode may skip checkout and read the workflow through a
data-only provider adapter at an authenticated event SHA. Private reads in that
mode are brokered capabilities; the compiler must not receive a broad reusable
provider credential.

An existing native step can depend on the importer:

```yaml
steps:
  - label: ":github: Load existing CI workflow"
    key: "gha-ci"
    agents:
      queue: "hosted"
    command: >-
      buildkite-gha upload
      --event-path .buildkite/events/current.json
      --runtime-queue hosted
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

The snapshot is compatibility data, not self-authenticating authority. Its
digest detects substitution and keeps compile-time and runtime contexts
consistent, but a plan cannot make its event trustworthy by declaring a trust
classification. An unattested `--event-path` is suitable for local validation
and tokenless execution. Any request for a protected capability must be checked
against authenticated Buildkite job identity and independently verified
provider provenance.

The provider interface owns:

- repository and event metadata;
- source and action reference resolution;
- authenticated clone/archive access;
- the provider facts required by capability policy; and
- provider-specific URL/context fields.

Provider API token issuance belongs to the protected capability control plane,
not the parser or source abstraction.

Implement GitHub first, but keep Cursor Origin behind the same interface from
the start.

The `vars` context also needs an explicit, non-secret source because it may be
used during compilation for matrices, conditions, and `runs-on`. Support a
documented precedence across bridge configuration, Buildkite pipeline/build
environment, and provider repository/organization variables. Snapshot the
resolved values into the plan so compile-time and runtime expressions agree.

### Protected capability control plane

Public, anonymous, tokenless workflows have no control-plane dependency. A
supporting service is introduced only for capabilities that ordinary pipeline
code cannot already access. The service authenticates every caller with a
short-lived Buildkite Job OIDC token whose audience is exactly the service. It
validates the Buildkite issuer and JWKS, time bounds, immutable organization,
pipeline, build, and job IDs, step identity, runner environment, cluster, and
queue as required by policy.

Buildkite OIDC proves which Buildkite job is asking. It does not by itself prove
the complete GitHub event type, actor, fork relationship, workflow source, or
environment approval. The service must obtain those facts independently. The
strongest integration receives GitHub App webhooks, records their verified
payload and the Buildkite build it creates, then joins a later Job OIDC
`build_id` to that event. An initial implementation may query and cross-check
Buildkite and GitHub APIs, but must document any event-fidelity gap and deny
claims it cannot establish.

The compiler submits the canonical plan digest and its requested capabilities.
After evaluating customer policy and provider facts, the service may return a
signed, expiring grant bound to the immutable Buildkite and provider identities,
plan digest, target jobs, queue, exact capability set, audience, and policy
version. Effective authority is the intersection of:

```text
workflow request
∩ organization policy
∩ provider event policy
∩ provider installation permissions
∩ queue policy
```

The service may then broker narrowly scoped GitHub App installation tokens,
private repository or action access, selected secrets, environment grants, or
explicitly supported compatible OIDC credentials. Direct Buildkite OIDC is
preferred for cloud providers that can trust `https://agent.buildkite.com`. A
compatibility issuer must use its own issuer and only emit claims established by
the service; it must never impersonate GitHub's OIDC issuer.

This service does not compile workflows, upload pipelines, schedule jobs,
interpret ordinary tokenless plans, provide per-step isolation, or make public
action code trustworthy. It is not a replacement for Buildkite pipeline
signing. It is the authorization and audit boundary for protected capabilities,
with explicit expiry, revocation, key rotation, abuse controls, and fail-closed
behavior.

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

Plans are canonical JSON with content digests. The upload job publishes them as
namespaced, producer-attributed Buildkite artifacts before generated jobs become
eligible. Each generated job downloads its plan from the exact importer,
verifies the digest, schema, build, job, step, queue, and compiler/runtime
binding, then invokes `run-job`. These checks protect transport and prevent a
job from accidentally consuming another job's plan; they do not grant protected
authority.

A plan lists requested capabilities. Before resolving any protected capability,
the runtime must additionally present its own Buildkite Job OIDC identity and a
matching signed control-plane grant. The grant, not a self-declared plan event or
the fact that a compiler signed bytes, authorizes the request. Signed plan
envelopes remain useful as optional integrity evidence and as a Phase 0
conformance mechanism, but are not required for public tokenless execution and
must not substitute for capability authorization.

Do not put complete plans into command-line arguments, environment variables,
or build metadata. They can exceed operating-system and Buildkite metadata
limits and are difficult to audit safely.

### Dynamic upload and bootstrap boundary

The bootstrap job has the ordinary authority Buildkite grants every dynamic
pipeline generator: it can add executable steps to its build. That makes it
security-relevant, but not a separate cryptographic principal. The supported
bootstrap contract is:

- run a pinned, checksummed `buildkite-gha` distribution;
- obtain workflow and action metadata as bounded inert input without executing
  repository hooks, plugins, generated scripts, or repository-provided
  binaries during data-only provider reads;
- provide no ambient protected credentials to tokenless importer or runtime
  queues;
- emit fixed `run-job` commands whose queue, plugins, containers, and requested
  capabilities are selected through fail-closed policy rather than copied
  directly from workflow text;
- bind every content-addressed plan to the generated job and exact importer;
  and
- reject protected capability requests unless a matching control-plane grant
  can be established by the execution job.

Buildkite signed pipelines are an optional Buildkite-specific defence-in-depth
layer for installations that require uploaded step provenance. They are not a
GitHub Actions compatibility requirement, and neither pipeline signatures nor
plan signatures replace the protected capability service.

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

Failures before the runtime can establish its build, job, step, and plan
identity cannot publish a trusted terminal manifest. Consumers diagnose that
case as a missing producer result and fail closed. Retrying an individual
producer can leave multiple attributed artifacts without an authoritative
attempt selector, which also fails closed; the supported recovery for either
case is to retry the whole build.

Values that influence a trusted decision, including continuation inputs,
native replacement validation, and retry state, must come from a verified
producer-attributed artifact. If a value can widen protected authority, it must
instead be covered by a control-plane grant; a compiler signature alone is not
authorization. A namespaced metadata key is insufficient because another job
in the same build can overwrite it. Phase 0 must verify the selected
attribution mechanism against a real build rather than assume metadata exposes
writer identity.

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
- an unavailable step reference in a condition fails closed instead of
  coercing to an empty value;
- a run step selected by `always()` or `cancelled()` after cancellation inherits
  the cancelled execution context, while registered post-actions receive the
  bounded cleanup grace period;
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

Continuations are ordinary authoritative dynamic uploads within the same
Buildkite build. Treat every prerequisite output as untrusted input and apply
explicit type, size, matrix-cardinality, and expression limits before
expansion. Workflow-produced values cannot widen queue policy or a protected
capability grant. Each newly generated job that needs protected authority must
be covered by a matching grant bound to its plan and authenticated job identity.

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
- send `SIGINT`, then `SIGTERM` after 7.5 seconds, then force termination after
  another 2.5 seconds for `cancel`; unlike GitHub's direct-process signaling,
  apply each signal to the complete process group; and
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

The release archive contains only the static Go CLI and LICENSE. The installer
plugin bootstraps mise 2026.5.12, verifies its pinned release archive and exact
cached executable tree by SHA-256, and follows the shared Buildkite hosted-cache
and agent-cache conventions. For action workflows, upload deterministically
archives that pinned importer mise executable, transports it as a
content-addressed artifact, and re-verifies it in every generated action job.
The runtime installs exactly `core:node@20.20.2` or `core:node@24.18.0` with
mise configuration disabled, digest-verifies the resulting Node executable,
and invokes that exact path directly. It never uses a fuzzy major, a data-dir
plugin, repository mise configuration, or an unverified tool-bin `PATH`.
`MISE_*` workflow environment overrides therefore cannot redirect compatibility
Node; ordinary shell steps retain them.
Generated action jobs declare a dedicated, pipeline-scoped Buildkite hosted
cache volume and use a runtime-owned `MISE_DATA_DIR`; the cache is a best-effort
accelerator, not an authority. The runtime checks cached Node executable bytes
against the official Linux x86-64 release digest, removes and reinstalls a
mismatch through the transported mise executable, and fails closed if the
replacement still differs. Shell-only jobs do not attach this cache. For job
containers, the host resolves that verified Node installation and mounts the
Node executable; mise is not required in the image. Node runtime bytes are not
release or Buildkite artifacts. Official mise-installed Node binaries require
glibc 2.28+ when JavaScript actions run; that is not a requirement of the static
Go CLI.

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

The runtime implements `::add-mask`, `::stop-commands`, `::warning`, and
`::error`. It applies dynamically registered masks to subsequent log lines and
scrubs all registered values from bounded job results before transport.
Warnings and errors retain supported source metadata and publish as separate
job-scoped Buildkite annotations without changing conclusions. Notices,
groups, command echo control, and legacy workflow commands remain later
compatibility work.

Map warnings and errors to Buildkite log output and annotations. Map dynamic
mask values to `buildkite-agent redactor add` before subsequent output is
written. A secret printed before it is registered cannot be retroactively
protected.

Step summaries become job-scoped Buildkite annotations when supported, with a
stable context. Oversized per-step summaries are rejected without failing the
job, matching GitHub Actions, while the aggregate annotation is truncated to
Buildkite's 1 MiB body limit.

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

Treat the authenticated build event and protected capability exchange as the
authorization boundary. Compilation parses arbitrary workflow text and records
requests; execution runs arbitrary shell and action code with job-level
authority.

- The generated plan declares required secret names and permissions but cannot
  authorize either one.
- Every protected execution job authenticates to the control plane with a
  Buildkite Job OIDC token using an exact service audience.
- The service verifies immutable Buildkite identity, queue, provider provenance,
  fork status, ref, commit, workflow policy, and any environment approval before
  issuing a narrow, short-lived grant or credential.
- The runtime verifies that the grant matches its own job, plan digest, queue,
  requested capability, audience, and expiry before resolving a value.
- Fork pull requests receive no privileged secrets by default.
- An untrusted event can target only queues explicitly configured for untrusted
  code; workflow-controlled `runs-on` values cannot select a privileged queue.
- Provider API tokens are short-lived and least-privilege.
- Reusable workflows can only narrow inherited permissions.
- Every secret is registered with the Agent redactor before use.
- Job outputs containing registered secret literals or explicitly supported
  standard encodings are rejected rather than published; general secret taint
  tracking is out of scope.
- Cloud OIDC uses a Buildkite-issued identity with documented migration guidance
  whenever possible. A compatibility service uses its own issuer and only
  translates claims it has independently established; it must not pretend to be
  GitHub's issuer.

Policy may reject a request before upload, but protected authority is evaluated
again at credential exchange. Runtime enforcement must never treat a plan's
self-declared event, trust classification, signature, or capability list as
sufficient authority.

When GitHub is the repository provider, populate `github.token` and
`GITHUB_TOKEN` only from a repository- and permission-scoped GitHub App
installation token brokered for the authenticated job. This approximates but
does not duplicate native `GITHUB_TOKEN`: app identity, endpoint support,
expiration, and event-recursion behavior can differ and must be documented. Do
not invent a token or silently grant write access. Validation must identify
actions and expressions requiring a provider token when no compatible grant can
be issued.

### Installation and release model

Publish checksummed releases. Add signatures, provenance attestations, and
SBOMs in Phase 9. The initial supported distribution is Linux x86-64; Linux
arm64 can follow once action/runtime compatibility is measured.

Distributions provide the static bridge CLI and LICENSE. The installer plugin
provides the pinned, verified mise executable, and action workflows transport
it as a content-addressed artifact so generated hosted jobs can resolve exact
Node versions without preinstalled mise. Every generated job must execute the
same bridge version that produced its plan unless the plan schema explicitly
permits a compatible newer runtime.

The v0.1 preview bootstrap is implemented as one reproducible Linux x86-64
archive containing only the static CLI and LICENSE, plus the
`github-actions#v0.1.0` installer plugin. The plugin downloads an exact public
release, verifies its checksum and fixed archive layout, caches the verified
distribution, installs and digest-verifies the pinned mise executable, and
invokes the fixed hosted-tokenless upload path. Node 20.20.2 and 24.18.0 are
installed by mise on demand into an automatically attached, integration-owned
hosted cache volume. Cached Node executables are digest-verified before use, so
cache sharing across builds is an optimization rather than a trust boundary.
Official mise-installed Node binaries require glibc 2.28 or newer.
The source repository must be public before the initial tag because plugin
installation intentionally uses anonymous release downloads. Release
signatures, provenance attestations, and SBOMs remain Phase 9 work and are not
claimed by this preview.

For a public source repository, a tag condition in repository-controlled
pipeline YAML is a scheduling guard, not release-token authorization. The
GitHub publisher credential must be unavailable to ordinary CI and retrieved
only in a dedicated upstream-tag-only release pipeline. Its external Buildkite
Secret access policy must at least bind the immutable release `pipeline_id` and
webhook `build_source`; because that policy does not distinguish tag webhooks
from pull-request webhooks, the pipeline's tag-only provider filter is also a
load-bearing control. Never expose the publisher token through a shared agent
environment hook.

Protected capability-grant validity must cover the intended exchange window and
remain short-lived. Once a grant expires, the service re-evaluates current
policy from authenticated job identity or denies the exchange; an old build
cannot silently refresh authority from plan-controlled data.

Do not download an unpinned latest binary separately in every generated job.

Document a minimum cancellation grace period for post-actions and container
cleanup. Warn when a customer-managed agent exposes a shorter grace period and
the runtime can detect it; otherwise make the requirement part of installation
validation.

Buildkite signed-pipeline compatibility is deferred to hardening. Early users
who already require signed pipelines may need to keep this bridge disabled
until that optional integration is implemented and tested.

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
- job summaries, warnings, errors, and live masked logs; and
- native jobs after the imported workflow.

Explicitly defer from beta unless implementation evidence changes the order:

- Windows and macOS;
- exact GitHub-hosted image parity;
- GitHub Enterprise Server;
- all repository event types;
- remote private reusable workflows;
- private checkout and private actions;
- protected secrets and GitHub token issuance;
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
- Phase 0 is complete. Its local semantic foundation is merged on `main`, and
  the hosted differential and Buildkite transport oracles have now run against
  exact commits.
- Phase 1 is implemented. The static compiler now owns compile-time contexts,
  explicit variable snapshots, bounded local reusable-workflow flattening,
  matrix and dependency expansion, fail-closed runner policy, immutable job
  plans, Buildkite pipeline YAML, and text/JSON compatibility reports.
  `compile` remains a read-only rendering command; `upload` materializes the
  executable and content-addressed plans used by generated jobs.
- Phase 2 is complete. Its sequential shell runtime, producer-attributed result
  transport, and unprivileged `upload` bootstrap are implemented and proven on
  Buildkite. Generated plans
  now carry conditions, timeouts, `continue-on-error`, bounded event identity,
  and explicit secret requirements. `run-job` allocates a fresh workspace,
  applies standard and file-command environment changes, evaluates the owned
  condition subset, masks registered and dynamically derived values in logs
  and results, propagates timeout/cancellation
  to process groups, and drains cleanup under a fixed deadline. Generated jobs
  hydrate exact producer results and publish bounded terminal manifests before
  exit; the live `shell.yml` proof passed with checkout suppressed on ephemeral
  hosted agents.
- Phase 3 is complete. The shell runtime now owns background, wait, wait-all,
  cancel, and parallel controls through a ten-active-step supervisor. Effects
  and failures remain barrier-scoped, workflow commands and mask registration
  are serialized across concurrent streams, and cancellation escalates across
  complete process groups without skipping bounded cleanup.
- Phase 4 is implemented within the tokenless public-action boundary.
  Action-resolved v3 plans carry immutable local and public action locks, verify
  complete source trees, execute nested composites and JavaScript pre/main/post
  lifecycle through exact mise-managed Node 20.20.2 or 24.18.0, and fail closed
  on private sources or provider-dependent authentication. Action resolution is
  independent of event trust. Normal `upload` resolves local and anonymous
  public JavaScript/composite actions without transporting Node executable
  bytes; generated agents use the exact content-addressed importer mise
  executable to resolve compatibility versions through `mise --no-config`.
  This path remains `EventUntrusted`, fixed to the
  ambient-clean, tokenless hosted queue, and accepts no capability, `network`,
  or the Phase 5 compiler-proven Dockerfile-action provenance.
  Private remote action or repository source, provider tokens, secrets, job- or
  service-container provenance, privileged queues, and other protected
  capabilities fail closed.
  Because this repository is private, the historical live evidence is split:
  the former proof importer used a synthetic public event to prove anonymous
  checkout and portable setup actions on Buildkite, while the GitHub-hosted
  oracle and local conformance suite cover the private repository's
  JavaScript/composite fixture. This does not claim a same-workflow private
  checkout proof on Buildkite. The migrated normal path now has separate exact
  Buildkite and GitHub-hosted evidence below.
- Phase 5 is complete. Schema-v4 plans own one persistent job container,
  service containers for container and host jobs, network aliases and ports,
  health diagnostics, host/container path translation, Dockerfile actions, and
  bounded cancellation and cleanup. Shell, JavaScript, and composite steps run
  in the persistent job container; services and Dockerfile actions are siblings
  on one runtime-owned network. Host jobs remain host processes and receive
  loopback-only published service ports. Image pulls are anonymous through a
  private Docker configuration, and Dockerfile builds explicitly select the
  local `default` driver so workflow code remains inside the disposable hosted
  job VM rather than the queue's remote shared builder. That VM is the isolation
  boundary; fixed mounts, networks, labels, and cleanup prevent resource
  confusion but do not sandbox mutually trusted steps within one job. Normal
  `hosted-tokenless` upload remains limited to compiler-proven Dockerfile-backed
  local and anonymous public actions. Job- and service-container provenance,
  `docker://`, Docker lifecycle overrides, private registries, arbitrary
  options, credentials, protected capabilities, and privileged queues continue
  to fail closed.
- Phase 6 has begun with bounded job summaries and advisory job-scoped
  Buildkite annotation publication. The checked-in exact-commit hosted proof
  compiles `testdata/phase6/.github/workflows/summary-annotation.yml`, settles
  the generated job through the existing importer/continuation topology, and
  requires an independent read-only API observation of its context, scope,
  style, and body. Build 242 and its API observation now establish that runtime
  evidence, so the smoke inventory records the fixture as `runtime-pass`.
  Workflow `::warning` and `::error` commands now use the same bounded,
  secret-scrubbed advisory publication boundary with separate stable warning
  and error contexts; they do not change step or job conclusions. Build 252
  and its exact-commit API observation prove both persisted annotations and
  masking, so the distinct checked-in hosted fixture is now `runtime-pass`.
  The producer-side artifact slice now recognizes only the audited
  `actions/upload-artifact` v4 commit and replaces its lifecycle with a bounded
  Buildkite Agent upload. Literal workspace files and directories become an
  immutable, digest-addressed ZIP; compatible artifact ID and digest outputs
  and the native path, size, and file count are bound into the authoritative
  terminal result manifest. Globs, symlinks, overwrite, retention, raw upload,
  and unrecognized upstream commits fail explicitly. Build 259 and its
  exact-commit artifact observation prove publication, attribution, manifest
  binding, archive integrity and contents, hidden-file exclusion, and compatible
  action outputs, so the separate upload-only fixture is now `runtime-pass`.
  The consumer-side slice recognizes only the audited `actions/download-artifact`
  v4.3.0 commit and one exact literal artifact name from verified direct
  `needs`. It preserves the manifest's exact producer job UUID through runtime
  hydration, revalidates native path, compressed size, digest, file count, ZIP
  member paths and expanded bytes, then extracts directly to a confined
  workspace-relative destination and exposes the compatible absolute
  `download-path` output. Run-wide listing, IDs, patterns, merge, cross-run,
  cross-repository, symlink, traversal, and special-file behavior fail closed.
  The full producer-to-two-consumer fixture remains `compile-pass` until an
  exact-commit hosted roundtrip and independent read-only observation establish
  runtime evidence.
- The first work wave is integrated: the Go/CLI foundation is runnable,
  ADR 0001 records the actionlint/act reuse boundary, and ADR 0002 plus schemas
  and eight conformance cases preserve the Phase 0 signed-envelope transport
  and tamper experiment. ADR 0002 is superseded as the production authorization
  model by the protected capability control plane described above.
- The second work wave is integrated: actionlint is isolated behind owned
  workflow and expression models, the compiler emits deterministic static IR
  for the smoke corpus, the differential harness materializes isolated Git
  repositories and compares normalized observations, and the local action
  runtime proves JavaScript, composite, and Docker lifecycle behavior with
  masked output and post-actions.
- The third work wave is integrated: compiler-selected, needs-free v1 job plans
  execute through `run-job`, the shell differential oracle has checked-in GitHub
  Actions and Buildkite definitions, and the transport/trust package plus
  dormant probe cover immutable artifacts, dependency policy, producer
  attribution, bounded canonical signed bindings and markers, and fail-closed
  upload recovery.
- The Phase 0 Deep Analysis pass is integrated: compiler output validates
  against the owned plan schema, expression substitution is single-pass,
  runtime output is bounded without pipe deadlocks, inherited environment is
  allowlisted, and transport artifacts are materialized from verified bytes in
  a confined root before upload.
- Phase 2 result transport is wired into `run-job`. Job plans map every logical
  `needs.<job>` to one or more exact producer step keys and immutable plan
  digests; consumers resolve each corresponding Buildkite job UUID before
  downloading its canonical bounded manifest and supplying verified logical
  results and outputs to the runtime context. Every Buildkite run verifies its
  exact build, job, step, and plan digest, then publishes success, failure,
  cancellation, or skipped state under a bounded background context before
  exiting. Artifacts are authoritative; namespaced metadata remains a
  best-effort UI/query mirror. Matrix fan-in with conflicting output values
  currently fails closed because the public artifact contract does not expose
  an authoritative completion order.
- All local tests, race tests, vet, schema fixtures, shell checks, and offline
  pipeline validation pass. A default `.buildkite/pipeline.yml` now runs the
  repository checks, and all compilable smoke-manifest outputs pass the current
  Buildkite Agent's `pipeline upload --dry-run --no-interpolation` parser.
- The smoke inventory now separates compilation, production-policy admission,
  and runtime evidence. `mise run smoke:local` is network-free: it validates
  the manifest and workflows and checks deterministic compilation, but is not
  runtime proof. `mise run smoke:profile` opts into public-network action
  resolution and managed-runtime preparation, then applies the same
  `hosted-tokenless` admission policy as production upload. An `admitted`
  result is still not runtime proof and does not establish that a generic
  action can execute without an unimplemented GitHub service.
- The profile is also exposed as the text/JSON workflow preflight
  `buildkite-gha validate --profile hosted-tokenless --event-path <path>
  --format text|json <workflow>`. Expected-negative fixtures preserve current
  boundaries: cache actions, artifact merge and broad download modes, and
  unsupported commits are denied admission or input validation. Job and service
  containers compile to schema-v4 plans and have hosted runtime evidence, while
  production admission rejects their container provenance.
- A consolidated exact-commit hosted dispatcher is available with
  `SMOKE_PROBE=hosted` and `SMOKE_COMMIT=<full commit>`. It aggregates the
  Phase 2 shell/upload, Phase 3 concurrent, Phase 4 public-action, Phase 5
  hosted-Docker capability, Phase 5 Dockerfile-action, and Phase 5 complete
  container-runtime proofs, plus the Phase 6 job-summary, workflow-command,
  upload-artifact, and artifact-roundtrip proofs. The dispatcher deliberately
  uploads each existing importer and continuation
  independently, then settles their generated and native terminal steps; it
  does not flatten the importer/continuation topology. Existing `PHASE2_PROBE`,
  `PHASE3_PROBE`, `PHASE4_PROBE`, all three `PHASE5_PROBE`, and `PHASE6_PROBE`
  selectors remain available for targeted runs.

Phase 4 live evidence:

- [Buildkite build 79](https://buildkite.com/buildkite/buildkite-gha/builds/79)
  ran exact implementation commit
  `c5b9c56762e94ce2084a7fe7223c5f18a432e2bc`. The policy-controlled importer
  invoked the exact built production `buildkite-gha upload` CLI with pinned
  managed Node 20/24 runtimes. The repository check, generated hosted public
  action job, separate continuation loader, and native continuation all passed.
- [GitHub Actions run 30069516646](https://github.com/buildkite/buildkite-gha/actions/runs/30069516646)
  ran the same implementation commit against the private repository. Its
  producer and consumer passed the JavaScript/composite differential fixture.

Phase 5 evidence:

- [Buildkite build 102](https://buildkite.com/buildkite/buildkite-gha/builds/102)
  ran exact implementation commit
  `b3a4c4c96d97812e5c087057cda0fdaf1a79bb19`. The checked-in probe observed the
  hosted queue's active remote Buildx driver, selected and verified the local
  `default` Docker driver, then passed pinned pull, build-and-load, execution,
  requested UID/GID bind ownership, private-network HTTP by alias, container
  health, dynamic loopback publication, TERM observation, and exact-label
  cleanup. The separate continuation settled only after the hosted probe.
- Build 102 establishes capability and queue-isolation prerequisites. It does
  not by itself authorize Docker; the separately reviewed Dockerfile-action
  path is the only Docker provenance admitted by normal tokenless upload.
- [Buildkite build 136](https://buildkite.com/buildkite/buildkite-gha/builds/136)
  ran exact runtime-evidence commit
  `50db2cf89ba23c0e051d7d57cc03e115606768e5`. All seven required live tests
  passed without skips. The compiler-to-Runner proof covered a persistent job
  container, services from container and host jobs, service DNS and loopback
  ports, JavaScript/composite/Dockerfile action lifecycle, process-tree
  cancellation, masked health diagnostics, and exact owned-resource cleanup.
  This proves the broader runtime implementation; it does not broaden
  `hosted-tokenless` admission to job- or service-container provenance.

- [Buildkite build 72](https://buildkite.com/buildkite/buildkite-gha/builds/72)
  ran exact implementation commit
  `9ec7df250e2e3f3afc05489b02d04ff48647df3a`. Its policy-controlled importer
  compiled an unattested synthetic event for public `actions/checkout` commit
  `3d3c42e5aac5ba805825da76410c181273ba90b1`; the generated hosted job fetched
  that exact SHA anonymously, then pinned `setup-node` and `setup-go` installed
  and verified Node 24.18.0 and Go 1.26.5. The repository check, generated job,
  separate continuation loader, and native continuation all passed.
  This remains valid evidence for the former proof importer; build 72 did not
  exercise the newly migrated normal `upload` path.
- [GitHub Actions run 30059944969](https://github.com/buildkite/buildkite-gha/actions/runs/30059944969)
  ran the same implementation commit against the private repository. Its
  producer and consumer proved the local JavaScript/composite output chain,
  environment propagation, state, summaries, masking registration, and post
  lifecycle on GitHub's runner.
- Buildkite build 72 does not claim local-action or cross-job v3 evidence: its
  event repository intentionally differs from the repository containing the
  compiled proof workflow. Those Buildkite runtime semantics are covered by
  deterministic conformance tests. Build 53 documented the reason for the
  split by failing the credential-scrubbed anonymous fetch of this private
  repository; no private credential was added or forwarded.

Phase 6 evidence:

- [Buildkite build 242](https://buildkite.com/buildkite/buildkite-gha/builds/242)
  ran exact implementation commit
  `fad3b797fc682e7c0a56c5c8c35392e526ef26ca`. The exact-commit importer
  compiled and uploaded the checked-in Phase 6 summary fixture, and generated
  job `019fb552-eb6b-43db-b084-7ed041c10db5` passed.
- The independent read-only API verifier found exactly one annotation on that
  job with context `buildkite-gha-job-summary`, job scope, `info` style, and
  both checked-in body fragments. This API observation is required evidence
  because annotation failure remains advisory and cannot change job outcome.
- [Buildkite build 252](https://buildkite.com/buildkite/buildkite-gha/builds/252)
  ran exact merged implementation commit
  `4050f0c884eef0f54a77c27958abe3ca21ede9e0`. The exact-commit importer
  compiled and uploaded the checked-in workflow-command fixture, and generated
  job `019fb5d6-f314-4294-b991-7ceb540604b7` passed even though it emitted an
  `::error` command.
- The independent read-only API verifier found exactly one job-scoped `warning`
  annotation with context `buildkite-gha-workflow-warnings` and one job-scoped
  `error` annotation with context `buildkite-gha-workflow-errors`. It verified
  both checked-in body contracts and the absence of the registered masking
  canary; successful job outcome alone would not establish this advisory
  publication evidence.
- [Buildkite build 259](https://buildkite.com/buildkite/buildkite-gha/builds/259)
  ran exact merged implementation commit
  `12e72d3298e919f33caba70d39265ef2da387f83`. The exact-commit importer
  compiled and uploaded the checked-in upload-only fixture, and generated job
  `019fb686-0167-4da1-9bfc-550166d6a1a4` passed.
- The independent read-only artifact verifier found exactly one native archive
  and one authoritative terminal manifest under that producer. It verified the
  manifest's v2 schema, exact build/job/step and plan attribution, artifact ID,
  digest, path, 363-byte size, and two-file count; independently rehashed and
  inspected the ZIP; confirmed both checked-in payloads and hidden-file
  exclusion; and bound the compatible action outputs in the job log to the
  manifest. The observed native archive digest was
  `sha256:efd85cd46277c320fdc52362d01ee151cd672c9b83f592d338c6baf45ac377dc`.

Phase 2 live evidence:

- [Buildkite build 40](https://buildkite.com/buildkite/buildkite-gha/builds/40)
  ran exact implementation commit
  `e93298085ffef96e1cb0982e7a0b88f3558b11da`. The producer and both consumer
  matrix jobs passed on ephemeral hosted Agent `4.0.0-beta.6`, followed by the
  native Buildkite continuation and the full repository check.
- The consumer logs emitted the normalized observations
  `{"result":"smoke-shell","variant":"one"}` and
  `{"result":"smoke-shell","variant":"two"}`, matching the checked-in
  GitHub Actions oracle.
- The producer raw log contained `MASKED_CANARY=***` and did not contain the
  registered canary value. Protected secret, provider-token, and privileged
  capabilities remain unavailable to the unsigned upload path.
- The native continuation started only after both generated consumers had
  finished, proving the separate continuation loader does not rely on the
  reverse insertion order of sequential dynamic uploads.
- Runtime conformance tests prove that post-actions run after main failure and
  cancellation, a cancelled process group cannot leave a child behind, and
  cleanup is bounded by the documented ten-second default grace period.

Phase 0 live evidence:

- [GitHub Actions run 29917793131](https://github.com/buildkite/buildkite-gha/actions/runs/29917793131)
  and [Buildkite build 11](https://buildkite.com/buildkite/buildkite-gha/builds/11)
  ran the shell differential oracle at commit
  `522a1f9ba87eb2fb0804ca381b1e7a1883d1124f`. Both materialized fixture commit
  `f479cc04720cac8bbb59cc54f193948864f08756` and produced the same normalized
  observation.
- [Buildkite build 27](https://buildkite.com/buildkite/buildkite-gha/builds/27)
  passed the complete transport at commit
  `787b3adfe306a17fbf073c70b24f8b747b5882a8`: immutable plan artifacts,
  producer-constrained result download, generated dependency execution, native
  dependency extension, metadata visibility, and Agent redaction all passed.
- [Buildkite build 15](https://buildkite.com/buildkite/buildkite-gha/builds/15)
  proved that the consumer runs and verifies a failure manifest after its
  producer fails. [Buildkite build 19](https://buildkite.com/buildkite/buildkite-gha/builds/19)
  rejected a directly corrupted ES256 signature before using its claims.
- [Buildkite build 17](https://buildkite.com/buildkite/buildkite-gha/builds/17)
  stopped after dynamic upload, then a retry rejected the prepared but
  incomplete state with exit 75. The supported recovery remains cancel and
  rebuild; the implementation does not assume upload atomicity.

Phase 0 spike support snapshot:

| Boundary | Proven locally | Explicit gap or live gate |
| --- | --- | --- |
| Compile | Actionlint-backed owned model, deterministic compile-time context and vars evaluation, bounded local reusable-workflow flattening, source-ordered matrix `include`/`exclude`, exact dependency fan-out, policy-selected queues, schema-valid versioned plans, and Buildkite pipeline YAML | Runtime-dependent graph expressions, remote reusable workflows, unsupported operating systems, and unmapped runner labels fail closed |
| Execute | Bash/sh steps; ten-active-step concurrency with barrier-scoped effects; JavaScript, composite, and Dockerfile actions; persistent job containers; services for container and host jobs; fresh workspaces; bounded prerequisite, step, and job outputs; file commands; conditions; masking; timeouts; process-tree cancellation; `continue-on-error`; bounded LIFO post-actions; and exact container cleanup | Producer result hydration enters through the transport boundary; private actions, `docker://` actions, protected credentials, arbitrary container options, unsupported operating systems, and unsupported expression/coercion forms fail closed |
| Differential | Isolated committed fixture, canonical capture/comparison, offline validation, and matching hosted GitHub Actions and Buildkite observations | Broader runtime behavior remains phase-specific differential work |
| Transport | Confined materialization of verified content-addressed plan and binding bytes, deterministic two-job live upload, strict compiler edges, failure-settling logical edges, producer-bound manifests, metadata, Agent redaction, signed markers, and native dependency extension | The probe deliberately avoids assuming upload atomicity |
| Authorization | Eight signed-envelope conformance cases plus live rejection of a corrupted signature prove bounded signing and verification mechanics only | Protected capabilities require Buildkite Job OIDC authentication, provider provenance and policy verification, narrow signed grants, runtime exchange checks, and auditability; Buildkite signed-pipeline integration remains optional Phase 9 work |
| Recovery | Ambiguous, partial, conflicting, or unattested interrupted uploads fail closed; a live interrupted upload and retry returned exit 75 | Operator cancel/rebuild is the supported recovery until Buildkite exposes an authoritative completed-upload query |

### Phase 0 — Prove the semantic foundation (complete)

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

The Phase 0 live transport spike uses exact-commit checkouts, an intentionally
public disposable signing key, and an unprivileged queue. It proves transport,
binding, and rejection mechanics without claiming a production trust boundary.
Its signed plan envelope does not authorize protected capabilities. Those wait
for the Job OIDC-authenticated control plane, provider provenance checks, and
runtime grant exchange defined for Phase 6.

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
- Plan-envelope transport mechanics are documented and proven with tampering,
  replay, expiry, wrong-build, wrong-job, wrong-queue, and untrusted-event
  fixtures without claiming that they authorize protected resources.
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

### Phase 2 — Sequential shell job runtime (complete)

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

Delivery slices:

1. Complete the sequential state machine and its local conformance tests:
   conditions and status functions, environment precedence, supported shells
   and working directories, file commands, timeouts, cancellation,
   `continue-on-error`, and bounded cleanup.
2. Promote the Phase 0 result probe into production transport contracts:
   canonical producer-attributed manifests, exact-step artifact download,
   bounded metadata mirrors, and verified `needs` injection.
3. Implement `buildkite-gha upload` for the explicit unprivileged local-event
   mode, materializing immutable plans before dynamically uploading the
   generated pipeline. Protected capabilities remain unavailable until the
   control plane for that capability exists.
4. Run `testdata/smoke/.github/workflows/shell.yml` on Buildkite, compare its
   normalized observation with the GitHub Actions oracle, exercise failure and
   cancellation cleanup, and inspect raw logs for the secret fixture.

### Phase 3 — Concurrent step runtime (complete)

Extend the shell state machine with the shipped GitHub Actions concurrency
contract:

- `background`, `wait`, `wait-all`, `cancel`, and `parallel` parsing and
  execution;
- a bounded supervisor enforcing the ten-active-background-step limit;
- barrier-scoped visibility for outputs, environment changes, paths, and
  failures;
- implicit `wait-all` before post-job cleanup;
- `SIGINT` cancellation followed by `SIGTERM` after 7.5 seconds and forced
  process-tree termination after another 2.5 seconds;
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

### Phase 4 — JavaScript and composite actions (implemented)

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

### Phase 5 — Containers and services (complete)

Implement the Linux Docker execution backend for:

- Docker actions;
- job containers;
- service containers;
- network aliases and ports;
- health checks and service failure logs;
- host/container path translation; and
- orphan cleanup.

Implemented order and fixed boundaries:

1. Harden Dockerfile-backed actions first. Build only a private staged copy of
   the verified action tree, select `buildx build --builder default --load`, use
   fixed structured mounts and bridge-owned labels, and stop/remove containers
   and images under an independent bounded cleanup context.
2. Admit only local and anonymously resolved public Dockerfile actions on the
   fixed tokenless `hosted` queue. Continue rejecting `docker://` images,
   Docker pre/post entrypoints, arbitrary Docker options, private registries,
   credentials, provider tokens, host namespaces, devices, socket mounts, and
   privileged capabilities.
3. Persistent job containers and path translation landed after Docker action
   lifecycle, command-file processing, masking, cancellation, and cleanup
   passed conformance coverage.
4. Services, aliases, port context, health diagnostics, and startup/orphan
   cleanup use the same runtime-owned backend. Imported container definitions
   are not translated into workflow-controlled Buildkite plugins.

The runtime's fixed mounts, translated paths, private Docker configuration,
owned labels, and bounded cleanup preserve Actions behavior and prevent
accidental resource confusion. They are not a security boundary against other
code in the same workflow job: a host `run` step with access to the local Docker
daemon has the same job-level authority as a Docker action. Do not add a mount
broker or host-level containment solely to distinguish those steps. Untrusted
jobs instead require a disposable VM and job-scoped daemon; stronger isolation
is a queue/executor concern.

The hosted default remote builder is not an authorization boundary for
anonymous Dockerfiles: it has cluster-scoped persistent caching. The tokenless
path must use the probed local `default` Docker driver so Dockerfile execution
remains inside the disposable job environment. Any future use of remote
builders for untrusted plans, or any elevated Docker option, is a separate
product and security decision.

Definition of done:

- The exact-commit hosted container and host-job fixtures observe compatible
  workspace paths and service connectivity.
- Failed health checks produce bounded, masked Buildkite diagnostics.
- Cancellation terminates owned process trees and container resources under a
  bounded cleanup context. Hosted VM destruction is the terminal guarantee on
  agent loss.
- Workflow-declared privileged options remain rejected. Direct commands inside
  a job remain governed by that queue's job-level isolation.

### Phase 6 — Core services and protected capability control plane

Deliver two explicit tracks. Tokenless Buildkite-backed adapters remain local to
the job:

- cache restore/save;
- artifact upload/download;
- public checkout under GitHub and Cursor Origin provider adapters; and
- step summaries and annotations.

Prefer documented Buildkite storage and Agent interfaces. If an action toolkit
requires an HTTP protocol, run a job-local compatibility endpoint or provide a
well-defined adapter rather than proxying GitHub's private service.

Protected provider features use the supporting control-plane service:

- authenticate callers with Buildkite Job OIDC and an exact service audience;
- bind immutable organization, pipeline, build, job, cluster, and queue IDs;
- establish GitHub event, repository, ref, SHA, actor, and fork provenance
  independently of plan-controlled fields;
- evaluate organization, event, environment, queue, and requested-permission
  policy;
- issue signed, expiring grants bound to exact plan digests and jobs;
- broker private checkout and action access, selected secrets, and scoped
  GitHub App installation tokens; and
- maintain audit records, expiry, revocation, key rotation, and fail-closed
  behavior.

Delivery slices:

1. Ship tokenless cache, artifact, summary, and public-checkout adapters without
   a service dependency.
2. Prove Job OIDC authentication, build/job binding, GitHub provenance checks,
   policy evaluation, and signed no-op grants before returning credentials.
3. Add private checkout, private actions, and private reusable-workflow source
   access through the narrowest practical credential or download interface.
4. Add scoped GitHub tokens, selected secrets, and environment grants.
5. Prefer direct Buildkite OIDC migration, then add only explicitly supported
   compatibility-issuer claims for providers that cannot consume it directly.

Definition of done:

- `testdata/smoke/.github/workflows/artifact.yml` uploads one file in its
  producer and verifies the same contents in both consumer matrix instances on
  GitHub Actions and `buildkite-gha`.
- Common official cache and artifact actions work without a GitHub Actions run.
- Artifact and cache keys cannot cross organization/build boundaries
  unexpectedly.
- Cursor Origin checkout does not require a GitHub repository mirror.
- Private checkout and actions use repository-scoped, least-privilege access
  without exposing a reusable service credential to the compiler.
- A forged event, wrong build/job/queue, expired grant, broadened permission,
  and fork-to-privileged transition all fail before a protected value is
  returned.
- Public tokenless workflows still run when the control-plane service is absent.
- The validator distinguishes portable and provider-dependent actions and names
  any unavailable protected capability.

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
- optional Buildkite signed-pipeline integration for installations that require
  uploaded step signatures;
- protected capability service availability, abuse controls, audit retention,
  key rotation, revocation, and external security review;
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

### Smoke and manual admission lanes

The checked-in smoke manifest is a classified inventory, not a claim that
arbitrary common workflows are supported. The required local gate is
`mise run smoke:local`; it uses no network, performs manifest and static
validation, and compares two nonempty compilations byte-for-byte. Compilation
success is never runtime evidence. Job and service container entries are marked
`runtime-pass` only because exact-commit hosted build 136 ran their plans; they
remain excluded from the production admission profile.

`mise run smoke:profile` is an opt-in networked lane. It resolves anonymous
public action sources, prepares the pinned managed Node runtimes, compiles the
plans, and evaluates the production `hosted-tokenless` admission policy. The
equivalent operator-facing preflight is:

```bash
buildkite-gha validate --profile hosted-tokenless \
  --event-path <event.json> --format text|json <workflow.yml>
```

Admission is policy evidence, not execution evidence. The exact audited
`actions/upload-artifact` commit and exact-name `actions/download-artifact`
commit are admitted through bounded native adapters; cache actions, artifact
merge and broad download modes, and unsupported commits remain rejected.
Unknown generic action dependencies are not declared executable merely because
the profile admits them.

The manual hosted aggregate is dispatched against one exact full commit:

```bash
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env SMOKE_PROBE=hosted --env SMOKE_COMMIT="$commit" --yes
```

It gathers Phase 2 shell/upload, Phase 3 concurrency, Phase 4 public actions,
the three Phase 5 Docker capability, Dockerfile-action, and complete
container-runtime proofs, and the implemented Phase 6 visibility and artifact
proofs in one build. Each phase still owns an independent importer and
continuation loader; a final aggregate waits for all generated and native
terminal evidence, including failures, without changing those security or
dependency boundaries. Keep the phase-specific selectors for focused reruns and
fault isolation.

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
- fail closed when provider provenance or protected capability policy cannot be
  established.

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
- Buildkite Job OIDC claims and APIs sufficient for short-lived provider token
  brokering;
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

- A content digest establishes plan integrity, not authority. Protected
  capabilities require authenticated Buildkite job identity, independently
  verified provider provenance, and a narrow control-plane grant.
- GitHub Actions does not isolate steps inside a job. A public action and a
  shell command have equivalent job authority, so anonymous tokenless actions
  do not require a privileged compiler merely because they use `uses:` syntax.
- Docker is an execution backend within that shared job authority, not a
  per-step sandbox. Disposable job VMs provide host isolation; fixed runtime
  mounts and options provide compatibility and resource ownership rather than
  protection from another step in the same job.
- A Buildkite bootstrap job has ordinary dynamic pipeline authority. Pinning the
  compiler, disabling accidental repository execution, and applying queue
  policy are important operational controls, but do not create a separate
  cryptographic principal.
- Buildkite Job OIDC is the natural authentication mechanism for a future
  capability service. It proves Buildkite job identity but needs provider facts
  for GitHub event type, actor, fork relationship, workflow source, and
  environment policy.
- Plan-envelope signing proved bounded canonical signing, tamper rejection, and
  build/job/queue binding in Phase 0. It is retained as an integrity mechanism,
  not as production capability authorization.
- Buildkite pipeline signing protects uploaded command provenance and remains
  optional installation-specific defence in depth; it is not required for
  tokenless Actions compatibility.
- Concurrent Actions steps require their own supervisor and conformance surface;
  they are now an explicit delivery phase rather than an unowned beta promise.
- Skipping Buildkite checkout is necessary but does not itself create a clean
  Actions workspace. Generated jobs now require native checkout suppression and
  a freshly allocated runtime workspace.
- Static local reusable workflows belong to the compiler phase; the later
  dynamic-graph phase now owns only runtime-dependent and remote workflow work.

## Resolved decisions before Phase 0

1. Canonical plans cross job and agent boundaries as content-addressed,
   producer-attributed artifacts bound to the build, importer, target job, step,
   queue, compiler, and workflow. These bindings protect transport but do not
   authorize protected resources.
2. The bootstrap is an ordinary Buildkite dynamic pipeline generator. It runs a
   pinned verified distribution, treats provider-fetched workflow text as inert
   data, exposes no ambient protected credential in tokenless mode, and emits
   fixed fail-closed job definitions.
3. Buildkite signed pipelines are not required for the initial implementation.
   Buildkite step signing is optional defence in depth and is deferred to Phase
   9. Protected capabilities are separately authorized by the Phase 6 control
   plane after Buildkite Job OIDC authentication.
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
8. The Phase 0 envelope experiment uses detached ES256 JWS over bounded RFC 8785
   canonical claims and remains separate from Buildkite pipeline signing. It
   proves signing mechanics only. The production capability-grant profile, key
   custody, rotation, revocation, and audit contract are Phase 6 decisions.
9. Explicit bridge, provider, and Buildkite variable maps use
   `Bridge < Provider < Buildkite` precedence and are snapshotted into every
   compiled plan.
10. No current Buildkite query authoritatively verifies a completed dynamic
    upload after interruption. Recovery fails closed with exit 75 and requires
    the operator to cancel and rebuild.
11. Until Buildkite has a scheduler-visible skipped result, a runtime-skipped
    imported job exits successfully after publishing its logical `skipped`
    result and emits a clear annotation. Downstream compatibility semantics use
    the logical result rather than the Buildkite job state.

## Decisions deferred after Phase 0

None of these decisions blocks the Phase 2 sequential shell runtime. Resolve
them in the phase that first needs the capability:

1. Phase 6 control-plane work will determine which authenticated GitHub event
   payload Buildkite exposes, whether the service must receive GitHub App
   webhooks directly, and whether a small Buildkite platform API is missing.
2. Phase 5 uses direct Hosted Agent execution. Exact-commit build 102 proved
   the local `default` Docker driver and queue prerequisites; build 136 proved
   the complete container runtime. A compatibility image remains deferred until
   tool-cache differences demonstrate a concrete need.
3. Phase 6 will choose job-local protocol adapters versus recognized built-ins
   for cache and artifact actions.
4. Phase 4 will set the customer-beta event and expression subset from the
   hosted differential corpus.
5. Phase 6 will define Cursor Origin checkout, event, pull-request, Job OIDC,
   and short-lived capability contracts alongside the GitHub provider adapter.
6. The fixed Buildkite `hosted` queue's documented per-job disposable isolation
   and the Phase 5 probe are sufficient for tokenless, non-privileged Dockerfile
   actions built on the local driver. Phases 6 and 9 still own authorization and
   queue policy for protected Docker capabilities and privileged workloads.
7. Phase 6 will define the GitHub App installation-token compatibility contract
   for `github.token` and `GITHUB_TOKEN`, including repository/permission
   narrowing and documented differences from native Actions tokens. Tokenless
   workflows remain the default until then.

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
   whose digest or build, importer, job, step, queue, or runtime binding does not
   match. The tokenless queue exposes no ambient protected credential.

This product milestone draws narrow slices from Phases 0, 1, 2, and 4; it does
not replace or waive the complete definition of done for any phase. It tests
compilation, execution, isolation, logs, outputs, Buildkite artifact transport,
action lifecycle, and native composition without depending on a long-running
service or GitHub's runner control plane. It is the smallest integration slice
that demonstrates `buildkite-gha` is a migration path rather than only a YAML
converter.
