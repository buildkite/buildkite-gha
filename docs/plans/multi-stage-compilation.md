# Compile jobs when their scheduling inputs are ready

Discussion proposal, not approval to implement. This plan starts with the
invariants that constrain a general multi-stage compiler. It keeps
[PR #468 / PB-3262](https://github.com/buildkite/buildkite-gha/pull/468) as a
bounded needs-derived matrix feature, not the framework for every late value.
Delivery ownership and remaining work belong in Linear.

Evidence baseline: [main at d388386](https://github.com/buildkite/buildkite-gha/commit/d38838693f384d06f0ef5aab76c961afc92d338e)
and [#468 at 8f1a80f](https://github.com/buildkite/buildkite-gha/commit/8f1a80febde06ed8fed8c30fe8e7a1b46bfb2505).
#468 was open when inspected. The
[preceding analysis](https://ampcode.com/threads/T-01a08b2a-6d09-7329-80ec-4949ea083083)
is context; the code and contracts below govern this proposal. None of the
hosted experiments in this document has been run for this plan.

## First establish which invariants survive staging

**Preserve** means later stages must maintain the existing boundary.
**Deliberately relax** means a documented implementation restriction changes in
an isolated feature PR. **Blocker requiring proof** means no dependent feature
should ship until the named contract or experiment establishes safety.

| ID | Invariant or hidden assumption and evidence | Classification and consequence |
| --- | --- | --- |
| I1 | The compiler expands reusable calls and matrices before constructing executable plans. `expandJobGraph` in [job_graph_expansion.go](../../internal/compiler/job_graph_expansion.go), [compiler ownership](../../internal/compiler/doc.go). | **Deliberately relax.** Preserve an admitted logical graph with unresolved scheduling sites; expand only the portion whose inputs are ready. Do not turn parsed workflow YAML into a second runtime program. |
| I2 | Each consumer embeds exact producer step keys **and producer plan digests**, including deferred inputs and caller guards. `buildPlanNeedSources` and related functions in [plan_dependencies.go](../../internal/compiler/plan_dependencies.go). | **Preserve.** An otherwise-static descendant may need later plan construction. Build producer and consumer plans in topological order in the same batch when only identity binding is missing; do not wait for producer execution unnecessarily. |
| I3 | Every visible logical need has a nonempty, sorted, unique producer set; each producer is a scheduling dependency and has one logical owner within that scope. Output projections can expose only those producers. [Plan validation](../../internal/plan/plan.go), `validateLogicalNeeds`. | **Preserve.** Joins combine scoped bindings, not a global `needs` dictionary. Empty expansions and compiler-created skips need an explicit result contract; absence is not a valid empty producer list. |
| I4 | Executable job-plan v2 contains a digest-bound normalized program. Site semantics derive from position, not serialized claims. Runtime verifies source, executable entrypoints, plan, version, runtime distribution, and target. [Expression authority](../expression-authority.md), [program sites](../../internal/program/sites.go), [run_job.go](../../internal/cli/run_job.go). | **Preserve.** A continuation is compiler input, never an executable job plan. Retain final plans unchanged after publication. Continuations must run the admitted compiler distribution, not whichever binary is currently installed. |
| I5 | The expression engine owns closed profiles, types, context availability, and exhaustive validation, even for unreachable branches. Unknown means a valid unavailable dependency, not an evaluation error. [engine.go](../../internal/expression/engine.go). | **Preserve.** Extend positional planning semantics through the same engine. Do not add matrix, runner, and reusable-input string scanners as independent authorities. Do not silently tighten existing profile extensions to GitHub's context table in a refactor. |
| I6 | Concrete credential effects must be contained in abstract effects; fully known effects agree; refinement cannot add effects. Action metadata cannot grant ordinary secret authority; composite metadata cannot grant token authority. Ordinary secrets use static inventory, with existing exceptions. [Expression authority](../expression-authority.md#security-invariants). | **Preserve.** Known scheduling data is not new authority. Re-run existing admission for newly concrete jobs within the initial source/configuration boundary. Do not globally refine or rewrite already-issued plans. Preserve variable-sensitive authority behavior: resolved `vars` do not prune token authority. |
| I7 | Workflow data, outputs, event snapshots, and digests do not authorize queues or credentials. Buildkite policy, job identity, top-level workflow permissions, and source-access settings do. Job/callee repository permissions do not currently narrow the token. [Security](../security.md). | **Preserve.** Stages may request permitted scheduling decisions but cannot mint workflow credentials for compilation, invent secret names, broaden private-source access, or fix permission narrowing client-side. New server-dependent policy must have a confirmed contract and rollout. |
| I8 | Reusable workflows flatten before program construction. Caller guards and deferred string inputs retain isolated caller scopes; secret forwarding is one-hop and cannot fall back to a same-named secret. Graph-time consumers of unresolved inputs are rejected. [reusable.go](../../internal/compiler/reusable.go), [runtime needs](../../internal/runtime/needs.go). | **Deliberately relax** eager flattening and the graph-time input restriction only. **Preserve** scope, guard order, secret forwarding, source identity, and static call targets. Retain call templates and provenance until needed; flatten before final program lowering. Typed late inputs and call matrices are separate slices. |
| I9 | Results come from exactly one artifact owner under the expected step key, then download by job UUID and verify build, job, step, and plan digest. Conflicting matrix outputs fail closed; no trusted completion order exists. [transport/results.go](../../internal/transport/results.go). | **Preserve.** Neither metadata, labels, nor pipeline success can substitute for producer output. Keep whole-build retry after ambiguous producer retries. “Latest artifact wins” and GitHub's matrix reusable-output completion-order semantics are out of scope. |
| I10 | Runtime attempts to publish terminal results for success, failure, cancellation, and runtime skip. A native skip never starts runtime; abrupt termination or cancellation before start may publish nothing. [PublishJobResult](../../internal/runtime/needs.go), [pipeline emission](../../internal/buildkite/pipeline.go). | **Blocker requiring proof.** Distinguish verified terminal results, compiler-decided skips, and missing evidence. A stage cannot infer cancellation or skip from a missing artifact. Prove recovery and check completion before supporting general `always()` descendants. |
| I11 | Step keys encode workflow namespace, logical job identity, and canonical matrix values; keys and provider check labels must not collide. Plans are separately content-addressed. [matrix.go](../../internal/compiler/matrix.go), [pipeline.go](../../internal/buildkite/pipeline.go). | **Preserve.** Phase number, retry attempt, and upload order must not rename ordinary jobs. Reserve logical/control/gate identities before upload; verify newly known row keys against the complete admitted namespace. Short key hashes are not proof of equality. |
| I12 | Main can retain independent jobs after partial compilation errors. #468 forbids job-by-job fallback for a workflow containing continuations because it can silently drop deferred jobs. [bundle.go](../../internal/compiler/bundle.go), [#468 upload changes](https://github.com/buildkite/buildkite-gha/pull/468/files). | **Preserve** whole-workflow failure for staged initial admission errors. Later batch failure must leave prior work intact but visibly fail the responsible stage; it must not report a completed workflow after dropping pending work. Other workflows may continue. |
| I13 | #468 owns one disjoint deferred subgraph per matrix; it rejects deferred-on-deferred matrices, joins, and deferred jobs crossing called-workflow concurrency gates. Its drift check reproduces existing jobs and all other continuations. [#468 expansion](https://github.com/buildkite/buildkite-gha/blob/8f1a80febde06ed8fed8c30fe8e7a1b46bfb2505/internal/compiler/job_graph_expansion.go). | **Deliberately relax** disjoint ownership only after replacing it with explicit single-writer stage ownership. Preserve existing-job identity and source checks. Removing these rejections alone is not a serial/join implementation. |
| I14 | #468 treats duplicate-key rejection plus step inspection as replay confirmation: command contains plan digest for normal jobs, label contains expected text for skipped placeholders; shared approval gates are checked for existence. [#468 continue.go](https://github.com/buildkite/buildkite-gha/blob/8f1a80febde06ed8fed8c30fe8e7a1b46bfb2505/internal/cli/continue.go). | **Blocker requiring proof.** This is narrower than verifying a complete applied batch, including dependencies, queues, gates, skip policy, and child stages. Prove atomic application and recoverable identity before generalizing. PB-2819 is not assumed necessary or unnecessary for the general case. |
| I15 | #468 records event, source locks, explicit runner mappings, repository/organization variables, OIDC and runtime settings. Its `compile` still calls `suggestedRunnerTargets` and `environmentSourceFromAgent`. Existing-job digest drift is checked, but new jobs have no earlier digest. | **Blocker requiring proof.** Do not claim that producer output is the only changing input. Runner and environment resolution need an explicit snapshot/retry contract. See the snapshot table below. |
| I16 | Workflow/call concurrency is an ordered open/close gate pair; close waits for members. The compiler checks cycles including queue order, not just authored `needs`. Gate-open can reserve a queue position before prerequisites finish. [concurrency_gates.go](../../internal/buildkite/concurrency_gates.go). | **Blocker requiring proof.** A late member must not appear after its enclosing gate closes, and a waiting stage must not hold the slot needed by its producer. Static group text can still require delayed admission when its guard or prerequisites are late. |
| I17 | Literal top-level environments resolve through the Agent API. Required reviewers become shared block steps; unsupported rules and secret-prefix collisions fail closed. Gated deployment retries are disabled to avoid reusing approval. [environment.go](../../internal/compiler/environment.go), [deployment environments](../compatibility.md#deployment-environments). | **Preserve** current protections and retry boundary. **Blocker requiring proof** for late names or cross-stage gate reuse: policy identity, scope, lifetime, prefix uniqueness, and snapshot freshness must agree. No GitHub reviewer identity parity is promised. |
| I18 | Limits are finite: 256 instances per matrix, 1,024 flattened jobs, reusable depth four; outputs are up to 64 names of 1 KiB each. Runtime-matrix decoding allows 64 KiB, but result transport is tighter. #468 continuation artifacts are bounded to 4 MiB and divide remaining job capacity among continuations. | **Preserve** aggregate resource bounds. **Deliberately relax** equal allocation only with deterministic budget ownership, not a fresh 1,024-job allowance at every stage. Count control steps and uploaded bytes separately; Buildkite documents a 500-step-per-file upload limit. |
| I19 | `compiler.IR.JobGraphComplete`, platform/cache fields, workflow token policy fields, and other compiler evidence are process-local or omitted from JSON. `Options` includes resolution interfaces. [compiler.go](../../internal/compiler/compiler.go), [config.go](../../internal/compiler/config.go). | **Preserve** the distinction between diagnostic IR and verified resumable input. `compile --format ir-json` is not a continuation protocol. Inventory omitted fields before defining any serialized planning record; never turn a serialized “complete/admitted” boolean into trusted evidence. |

These findings rule out two shortcuts: recompiling the whole workflow with
whatever values are now available can change existing plans, and replacing
producer plan digests with a dynamic lookup weakens a runtime integrity
boundary. Neither is part of this proposal.

## Stage only when a required decision is unavailable

**If a value is unavailable now AND required to determine graph shape or
pre-dispatch scheduling/policy, emit a continuation/readiness stage. Otherwise,
retain supported runtime evaluation or reject unsupported syntax.** A readiness
stage is a generated compiler job that waits for named prerequisites, constructs
new immutable plans, and uploads them; it does not execute workflow steps.

Apply the rule in this order:

1. Validate the authored position, contexts, functions, types, source references,
   and authority exhaustively. Invalid or unsupported syntax is not “unknown.”
2. Reduce with the admitted, position-appropriate snapshot. An absent property
   in a complete context follows that profile's missing-value rules; an absent
   producer result is not an empty string.
3. Identify the consumer's deadline: graph construction, admission before
   dispatch, job setup, step execution, or result publication. Derive this from
   position, not from the spelling of `needs` or `fromJSON`.
4. For an unavailable pre-dispatch value, record legal producer dependencies.
   Emit a stage only if those producers can be made ready without the blocked
   work. Reject cycles, self/future-step references, and unsupported positions.
5. Delay final plan binding if producer identities/digests are unknown, even
   when the consumer has no late scheduling expression (I2). This may require
   only later construction in an existing batch, not another result wait.

Two readiness tests must remain distinct:

- **Binding-ready:** all producer instances and their final plan digests are
  known. A downstream runtime-only job can be planned now and scheduled with
  ordinary dependencies.
- **Value-ready:** the exact verified results required for a scheduling
  expression or admission guard are available. Only these require waiting for
  producer execution before materialization.

In the target behavior, job/call guards governing a deferred matrix run before
demanding matrix values or selecting deployment policy. A false guard can avoid
reading an unused output, but cannot hide prohibited syntax. This requires the
terminal-outcome work in P6b; it is not a behavior-preserving change to #468.
Earlier slices retain its documented limitations. Ordinary runtime-only job
conditions remain on the current path. Exact GitHub pre-dispatch condition
parity for all static jobs is not silently bundled into this refactor.

### Counterexamples prevent staging from spreading into runtime

| Example | Decision |
| --- | --- |
| Matrix from `github.event` or static dispatch inputs | Initial compilation: expression syntax does not imply a later stage. |
| `runs-on: ${{ needs.plan.outputs.runner }}` with a valid direct `needs: plan` | Stage, even for one job: queue/platform selection precedes dispatch. |
| `services: ${{ fromJSON(needs.plan.outputs.services) }}` | Existing runtime setup: a dynamic container map does not change the Buildkite graph. Keep its non-credential map restrictions. |
| Step `if: needs.test.result == 'success'`, or output used in an action input | Runtime: no new compilation stage merely for the expression. |
| Job-container image from a prerequisite | Currently rejected, but potentially runtime setup, not inherently a new upload. Any platform/capability consequences still need admission. Do not enable it here. |
| Environment URL from a step output | Reporting after execution; currently accepted with no effect. Distinguish it from environment **name**, which selects policy. |
| Static called-workflow concurrency group behind a late call guard | Potential readiness stage: the unavailable decision is gate admission, not the group string. Keep unsupported combinations rejected until gate proofs pass. |
| Dynamic job IDs, `needs` lists, `uses` targets, or permission levels | Reject; multi-stage compilation does not make these legal expression positions. |
| Matrix referring to its own `steps`, `runner`, or `secrets` | Reject unavailable/forbidden contexts, not a self-waiting stage. Top-level concurrency cannot directly read `needs` either. |
| `fail-fast` or `cancel-in-progress` | Cancellation coordination, not solved by another upload. Preserve current documented ignore/reject behavior. |

## Separate logical planning, materialization, and execution

The proposal keeps one compiler with two entry points: plan known work and
resume an admitted scheduling decision. Both lower concrete jobs through the
existing normalized-program and authority path. The runtime does not become a
compiler, and no persistent orchestration service is assumed.

```diagram
┌──────────────────────────────────────────────┐
│ Initial admission: sources, event, policy     │
│ Logical jobs/calls and unresolved decisions   │
└──────────────────┬───────────────────────────┘
                   ▼
┌──────────────────────────────────────────────┐
│ Materialize binding-ready/value-ready work    │◀──────┐
│ Final plans + pipeline batch + next stage     │       │
└──────────────────┬───────────────────────────┘       │
                   ▼                                  │
┌──────────────────────────────────────────────┐       │
│ Verify artifacts, upload, reconcile replay   │       │
└──────────────────┬───────────────────────────┘       │
                   ▼                                  │
┌──────────────────────────────────────────────┐       │
│ Execute immutable programs; publish results  │───────┘
└──────────────────────────────────────────────┘ verified
                                                   inputs
```

### Contracts, not new frameworks

The names below describe conceptual records and interfaces, not proposed Go
types, package splits, or a commitment to a new general-purpose IR library.
Reuse existing source, program, plan, and transport models where they fit.

Current models provide the starting boundaries:

- `compiler.IR` and `JobInstance` describe expanded graph work, not pending call
  templates. Add logical pending work alongside concrete instances rather than
  filling their required queue/platform fields with placeholders.
- `compiler.Bundle` and `PlanArtifact` already join plans, their bytes/digests,
  and generated pipeline data. A materialization batch should reuse that
  boundary rather than define another job envelope.
- #468's `RuntimeMatrixDescriptor`, `RuntimeMatrixContinuation`, and CLI-owned
  `continuationArtifact` are the bounded adapter to migrate, not a requirement
  that every readiness request name one matrix output.
- `plan.Job`, `program.Program`, and `transport.ResultManifest` remain the final
  execution/result contracts. Do not generalize runtime lifecycle interpretation
  as part of introducing staged compiler inputs.

| Boundary / owner | Inputs | Output and required guarantees |
| --- | --- | --- |
| **Initial admission — `compiler`, with `cli` providing source/API adapters** | Workflow/call/action sources, verified event provenance, known inputs, variable scopes, importer configuration, available runtime distributions, external policy snapshots. | An immutable **admission snapshot** and **logical graph**, plus the first materialization batch. Snapshot binds source closure, compiler/version/distributions, build/workflow identity and namespace, configured authority limits, and resource budget. A source lock detects change; it is not a grant. All statically discoverable syntax/source failures surface before any staged workflow upload. |
| **Logical graph — `compiler`** | Admitted source models and positional expression semantics. | Jobs and call templates identified by source/call path, static dependency edges, caller-scoped input/output projections and guards, unresolved scheduling sites, and existing concrete bindings. Each site retains source location, expected type, consumer deadline, and dependency provenance. Unknown row count is explicit. The graph may describe work; it cannot supply raw pipeline commands. |
| **Readiness request — compiler meaning, `transport` envelope, `cli` orchestration** | Snapshot digest, owned logical scope, predecessor batch references, pending sites, exact known result sources, and remaining budget. | A bounded, versioned, digest-bound request with a deterministic stage identity. It names only admitted work and allowed producers. Child requests reference prior records rather than recopying every historical plan. Decode rederives positional semantics and checks scope; wire fields cannot choose a broader expression profile or permission. |
| **Result loading — `transport`, shared projection semantics with runtime** | Build identity, exact producer step key and plan digest; caller-local projections. | Verified terminal results and outputs, or a typed missing/ambiguous/invalid-evidence failure. If instance identities were previously unknown, obtain them from the verified predecessor materialization record first. Never guess from a step-name prefix or enumerate arbitrary build artifacts. |
| **Materialization — `compiler`** | Admitted logical scope, verified predecessor bindings, required results, fixed policy snapshots. | A **materialization batch**: concrete jobs/plans, logical-to-instance bindings, resolved scheduling/gate descriptions, terminal logical outcomes where supported, next readiness request, consumed budget, and diagnostics. Construct plans topologically using existing lowering and admission. Do not mutate earlier plans. Pure compilation takes supplied snapshots; network policy lookup stays in adapters. |
| **Commit/reconcile — `cli`, `transport`, `buildkite`** | Complete validated batch; verified artifact bytes; expected existing-step/gate identities. | An **applied-batch record** binding batch digest to the build and actual uploading job, complete step set, plan digests, dependencies, targets, gates, and child request. Publish artifacts before the pipeline. Confirm application before declaring success; recover an ambiguous response by comparing the complete expected batch, not by uploading replacement work. |
| **Execution — `plan`, `program`, `runtime`** | Existing final job plan and exact artifact/distribution producers. | Existing workflow lifecycle and result-manifest contract. Runtime evaluates job/step/setup fields, obtains only plan-authorized credentials, and publishes bounded results. Runtime never reads a continuation to broaden its plan. |

An applied-batch record is a required **meaning**, not an assumption that an
artifact write and pipeline upload can be atomic. The implementation must show
how the successful upload identifies its record after a lost response or a
coordinator retry with a different job UUID. A pre-upload artifact alone does
not prove application. I14 and experiment E2 decide whether existing Agent APIs
can implement this contract or whether a server primitive is needed.

Before materialization, verify the current build/workflow and stage target,
the initial importer's artifact identity, every referenced record's digest and
actual publisher, and a bounded, acyclic predecessor chain. Verify that the
requested jobs, sources, producers, and budget belong to that stage. Payloads
remain typed data: an output containing `${{ ... }}` must not become newly
authored expression source. No workflow command or action lifecycle runs in a
compiler stage. Digests establish integrity relative to admitted references,
not independent trust in an uploader; Buildkite policy still grants access.

### Scheduling dependencies are not expression scope

Keep three edge kinds distinct: authored logical `needs`, result/identity
inputs to a planning stage, and scheduler-only edges such as approval and
concurrency gates. A stage may wait on a prerequisite without exposing it as
`needs` to the callee. A control step never becomes a workflow producer simply
because it uploaded jobs.

For `plan → build[matrix] → publish`, the first stage reads `plan`'s result,
materializes all `build` instances, and then constructs `publish` against
those exact plan digests. It uploads both in one batch. `publish`'s ordinary
Buildkite dependencies wait for `build`; another compiler job is unnecessary.

For `plan → build[matrix] → summarize → deploy[matrix]`, the same batch can
contain `build` and `summarize`. A new stage waits for `summarize`'s verified
output before expanding `deploy`. It must not reference nonexistent future
instance keys or depend on a future job it itself must create.

For a join of two deferred branches, both branches' final producer sets must
be bound before constructing the joined consumer. Only branches supplying late
scheduling **values** must also finish executing before that construction.

### Give each pending job one materialization owner

Recommended first serial/join design: compute deferred dependency components
from the admitted logical graph, merging components whose downstream closures
overlap. Assign one compiler-controlled sequence of readiness stages to each
component. This avoids two matrix uploaders racing to own a shared descendant.
Independent components can proceed independently.

Within a component, a stage can materialize several ready jobs in parallel
and emit its single successor request. Initially accept a conservative barrier
between frontiers rather than implement a distributed ready queue. This can
delay a fast branch behind a slow sibling; it is an explicit performance
tradeoff for deterministic ownership and budget accounting, not full scheduler
parity. Measure it before adding more coordination.

Every successor must either resolve pending decisions, publish new work, or
terminalize admitted work. Repeatedly uploading an unchanged request is an
error. Bound stage count/depth and total control steps as well as matrix jobs.
Future call-matrix expansion inherits its parent's ownership and budget; it
cannot introduce arbitrary sources or cross-component edges. If the admitted
call templates cannot establish this, reject that shape until separately proved.

This is a proposed default, not a settled upload algorithm. E1 and E4 must prove
it against Buildkite's inherited upload dependencies and ordered queues. Do not
pre-create a dependency on an upload step if its future descendants would
create a cycle or close an enclosing gate too early.

### Snapshot timing must be explicit

| Data | Current evidence | Proposed boundary |
| --- | --- | --- |
| Workflow, reusable sources, action metadata | Main resolves per operation. #468 checks workflow/reusable source drift and pins action locks; a moved reusable tag fails rather than selecting new source. | Bind source closure at initial admission. Prefer reopening immutable commits for future stages; failing on drift is an acceptable conservative first implementation. Never discover a new `uses` target from output data. |
| Event, repository/organization variables, OIDC, private-source setting | #468 records these and shares one event artifact. Variables are non-secret artifact data. | Freeze at initial admission, including the distinction between unresolved and resolved-empty variables. Reuse exact event artifact identity. Never place tokens, secret values, or Git credentials in snapshots. |
| Explicit runner mappings and runtime binaries | #468 records mappings and per-platform digests. | Freeze initially. A newly selected platform must have an admitted runtime distribution; do not download a new “latest” runtime. |
| Agent-resolved runner targets | #468 re-resolves at continuation time; static-job drift may fail, but new jobs have no old plan. | First late-runner slice uses explicit mappings. General resolver support requires binding a permitted resolution to the stage before commit and proving retry behavior. A known label's changed queue/platform/image must not silently alter a replay. |
| Environment policy, secret-name mapping, environment variables | Main resolves literal top-level environments. #468 calls the environment API during recompilation rather than carrying a complete initial environment snapshot. | Freeze known environments initially in the general path. Keep late names rejected until the server contract defines authorized selection, snapshot identity/freshness, and retries. Prefer first-selection snapshots before admission, with reuse only within that build; live credential policy may still deny execution. |
| Producer results | Exact build/job/step/plan binding; multiple artifact owners are ambiguous. | Record the selected manifest identity/digest as stage input. Preserve whole-build retry if a workflow producer was retried. Coordinator retries must not change selected workflow results. |
| Gates and external authorization | Current gates use generated keys; Buildkite remains credential authority. | Bind gate kind, logical scope, selected policy, and lifetime to the batch. Existence alone is not proof of equivalence. Cached snapshots never bypass current server-side credential denial. |

Dynamic environments cannot be selected from their own environment variables:
job admission and environment selection precede that variable scope. A future
snapshot design must not resolve this cycle by exposing all environments or
their secret names to every job. The
[environment backend plan](environment-resolution-backend.md) records a rollout
dependency, not evidence that the backend is currently deployed.

### Replay, keys, budgets, and failures

Use separate identities for the admitted build/workflow, logical job/call,
ordinary instance, readiness stage, batch, and producer attempt. Stable stage
identity must not contain a retry job UUID or output value; the batch digest
binds the selected values and full effect. A retry with different content under
the same stage identity is a conflict, not a new stage.

Preserve ordinary key/check naming for existing static and #468 shapes.
Allocate stage/gate namespaces deterministically and check all known and newly
derived identities before upload, including case normalization, punctuation,
duplicate rows, empty rows, nested call names, and short-hash collisions.
Do not resolve a collision by assigning a new key on retry.

Each component receives an initial budget. Every batch records consumption and
remaining capacity; retries consume no additional logical capacity. Children
receive a partition of their parent's remainder. Reject an over-budget batch
before upload. Retain #468's simpler allocation until this accounting exists.
Separately respect upload-size/step limits, including gates and continuation
steps. Splitting one logical batch into multiple pipeline uploads changes its
commit contract and needs E2; do not call it atomic by analogy with one upload.

| Outcome | Handling and recovery |
| --- | --- |
| Unsupported syntax, bad type, impossible dependency, policy denial, size limit | Fail initial admission when knowable; otherwise fail the owning stage before upload, with source/call path and field. No runtime or unprotected fallback. |
| Verified producer failure or skip | Evaluate the applicable guard with existing result semantics. Do not automatically classify every descendant as skipped. #468 keeps its bounded skip-all behavior until the terminal-outcome slice is proved. |
| False admission guard | Record a skipped logical outcome without credentials or deployment policy lookup. General consumers need a verified representation of this outcome; #468's native skipped placeholders do not publish runtime manifests. |
| Missing required output after a verified result | Apply the position's defined missing-value semantics, then type/shape validation. A required matrix cannot become an invented empty graph. |
| Missing, ambiguous, or mismatched result evidence | Fail closed; do not treat as ordinary workflow failure or skip. Ambiguous producer retry requires a new build. Hard-killed or never-started jobs require authoritative terminal evidence before any alternative is allowed. |
| Source/configuration drift | Reject the stage. Retry with identical admitted data or start a new build; never patch existing plans. |
| Transient read failure | Bounded retry of the same read, then fail with retry guidance. Do not run a polling controller indefinitely. |
| Upload applied but response lost | Outcome is unknown until reconciled. Exact batch match means success; mismatch means conflict; unavailable evidence means unresolved failure. Never issue `--replace`. |
| Cancellation or failed stage before descendants exist | Keep the stage's check visibly terminal; do not claim all promised jobs ran. E3 must define closure of logical checks and enclosing gates without inventing producer outputs. |

Diagnostics should name stage, workflow/call/job, field, expected producer, and
recovery action, but not echo producer values or configuration payloads. Record
digests, counts, stage reasons, and timing for debugging. Preserve provider
check identity for materialized jobs; a compiler-stage check must not masquerade
as a successful execution check. Check labels and `job.check_run_id` are
reporting data, not materialization or credential authority.

## Compatibility targets and examples

These are design examples, not a claim that the proposed syntax works today.
“After #468” means the inspected PR if landed unchanged, not current main.
All output references below require a direct authored `needs` edge in the
appropriate caller scope.

| Case / minimal example | Main | After #468 | General path and boundary |
| --- | --- | --- | --- |
| Static matrix: `matrix: {os: [ubuntu-latest, macos-latest]}`, `runs-on: ${{ matrix.os }}` | Supported with valid runner mappings. | Unchanged. | Initial expansion, no readiness step; same keys/plans/checks. |
| Needs-derived matrix: `matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}` | Described but rejected as requiring a second upload. | One continuation for a single producer instance and its disjoint descendants. | First supported adapter for general readiness, retaining current scalar/size/row limits. |
| Dynamic runner: `runs-on: ${{ needs.plan.outputs.runner }}` | Rejected. | Still rejected; `matrix.runner` after late matrix expansion is different. | One readiness stage, even without a matrix. Begin with explicit runner mappings and admitted platforms. |
| Reusable string input: caller `with: {tag: '${{ needs.plan.outputs.tag }}'}`, callee step input `${{ inputs.tag }}` | Supported runtime hydration for supported forms. | Unchanged. | No new stage when all uses remain runtime-only. Keep caller `needs` hidden. |
| Same reusable input consumed by callee `runs-on`, matrix, or concurrency | Rejected. | Not generalized. | Follow input provenance to its scheduling consumer; stage in the caller scope before lowering concrete callee jobs. Mixed runtime/scheduling uses share the value, not scopes. |
| Needs-derived matrix on a reusable **call job** | Rejected. | Not general call expansion. | Later dedicated slice: bind call instances first, preserve input/guard/output scopes and source locks, then lower jobs. Invocation-level `max-parallel` is not solved by per-job concurrency. |
| Serial `plan → build[matrix] → summarize → deploy[matrix]` | Rejected at first late matrix. | Rejects deferred-on-deferred matrix. | Two value-readiness frontiers; `summarize` is planned with `build`, not after it finishes. |
| Join `publish.needs: [linux, mac]` where both are deferred matrices | Rejected. | Rejects two deferred owners. | One owned joined scope; bind both complete instance sets. Runtime `publish` needs no extra output-driven stage once identities exist. |
| Runtime-only `if: always()` on a static diagnostic job | Supported runtime condition with existing result-transport limits. | A deferred descendant of a failed matrix producer is nevertheless skipped. | Preserve static behavior; separately prove logical outcomes before relaxing #468's skip behavior. No promise of recovery from missing manifests. |
| Job concurrency `group: deploy-${{ needs.plan.outputs.target }}` or late matrix `max-parallel` | Rejected for late group; max-parallel expressions rejected. | Late group and max-parallel expressions remain rejected. A group using concrete `matrix` values is supported. | Resolve group/limit before dispatch. Validate global gate/queue cycles; cancellation behavior remains separate. |
| Static called-workflow group with late guard/prerequisites | Supported only within current static graph restrictions, with different queue admission timing from GitHub. | Deferred crossings rejected. | Readiness admission before opening the gate only after lifetime and deadlock proofs. |
| Literal top-level `environment: production` | Supported subset through backend snapshot API. | May appear in deferred descendants with shared approval gates. | Preserve approval, prefix checks, and no manual deployment retry; bind policy snapshot consistently. |
| `environment: ${{ needs.plan.outputs.environment }}` or environment inside reusable workflow | Rejected. | Rejected. | Separate server-gated policy slice; no default environment, protection downgrade, or new secret authority. |

A concrete runner example for the first new vertical slice:

```yaml
jobs:
  choose:
    runs-on: ubuntu-latest
    outputs:
      runner: ${{ steps.pick.outputs.runner }}
    steps:
      - id: pick
        run: echo 'runner=macos-latest' >> "$GITHUB_OUTPUT"
  test:
    needs: choose
    runs-on: ${{ needs.choose.outputs.runner }}
    steps:
      - run: uname -s
  report:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - run: echo 'Tests completed'
```

Assuming explicit Linux and macOS mappings and both runtime distributions,
initial admission emits `choose` and a readiness stage. That stage reads only
`choose`'s verified output, binds `test` to macOS, then constructs `report`
against `test`'s plan digest and uploads both. `report` runs after `test`; it
does not need another compile. An unmapped or denied selector fails before
upload, and using the same output in a step environment instead would need no
readiness stage.

## Deliver vertical slices with explicit stop points

This order is a proposal for separate PRs, not a task checklist or permission
to start coding. Preserve supported behavior in groundwork PRs and introduce
one compatibility change at a time. Splitting the named slices further is
preferable to combining a protocol change with multiple new features.

| Slice | Reviewable outcome and ownership | Dependencies and acceptance evidence | Rollback / stop point |
| --- | --- | --- | --- |
| P0: invariant decision and hosted contract probes | Agree I1–I19, staging rule, snapshot timing, component ownership, and error categories. Prepare/run E1–E3 on an approved disposable build before production refactor code. | Can precede #468. Platform/security owners confirm server contracts; docs and fake-agent tests alone are insufficient. | Keep current rejection boundaries if any required contract is unavailable. No product migration. |
| P1: preserve current planning boundaries | Characterize static and bounded #468 behavior, especially I2/I4/I5/I8. Make expression-site deadline/dependency classification explicit in compiler lowering using existing profiles; add typed valid-unknown results only where needed. Do not add support or broaden contexts. | Can start on main independently of #468; #468-specific fixtures follow its landing. Structural site coverage, static output parity, authority laws, and negative syntax tests. | Revert independently; no new serialized continuation format or changed CLI output required. |
| P2a: isolate one materialization batch | Separate #468's compiler inputs/outputs from CLI I/O without changing live resolution, artifact format, guards, or shape restrictions. | After #468 and P1. Existing fixtures must remain equivalent, including failure, drift, keys, checks, budgets, and skip limitations. | Revert independently; no format or behavior migration. |
| P2b: bind snapshots and applied batches | Add the proved snapshot/reconciliation contract to the bounded path. Changed policy drift/retry behavior is an explicit behavior change, reviewed separately from extraction. Keep job-plan v2 if its execution envelope is unchanged. | P2a, E2, and policy snapshot decisions/E5 for already-supported API resolution. Test old/new behavior when live policy changes; preserve supported static and bounded matrix shapes. | Disable new-format admission for new builds; retain exact distributions for in-flight builds. Never reinterpret a v1 artifact as the new format. |
| P3: late runner without a matrix | Support one direct needs-derived runner selector through explicit mappings; plan runtime-only descendants in the same batch. Retain bounded producer-failure/guard limitations until P6b. Keep live runner API generalization separate. | P2b. Runner example above; asymmetric Linux/macOS proof, unmapped/protected label rejection, unavailable platform, replay, and descendant digest tests. | Restore the late-selector rejection for new builds. Already-published plans remain runnable under their pinned distribution. |
| P4: late reusable string inputs in runner selection | Carry producer provenance through nested string input forwarding to a callee scheduling site. Keep runtime-only uses unchanged. | P3; nested caller-scope tests, false outer guard, shadowed job/input names, private-source denial, secret alias non-escalation. | Restore graph-time late-input rejection; retain existing runtime string hydration. |
| P5: serial readiness | Allow a later matrix to read a single producer created by an earlier stage. Introduce owned component progression and budget consumption without joins yet. | P2b and E1/E4; may proceed independently of P4. Chain with two late boundaries, deep-chain limit, drift/replay at each boundary, static descendant planned in same batch. | Reject serial shapes at admission for new builds; finish or cancel existing builds with their original binary. Never remove their uploaded steps. |
| P6a: joins | Merge overlapping deferred closures under one owner and bind both branches. Keep the conservative terminal/failure behavior explicit. | P5 and E2–E4. Unequal branch sizes and completion order, duplicate owner attempts, mixed static/deferred prerequisites, and failures must never drop the join silently. | Restore join rejection for new builds. Serial stages remain available. |
| P6b: terminal logical outcomes | Define a verified compiler-produced skip representation distinct from runtime manifests. Then introduce guard-before-materialization and general `always()` handling in a separate enablement PR if the envelope changes. | P5 and E3; test with P6a before enabling joins plus new outcomes. Failed producer plus `always()` consumer, false guard with unused output, native skip, no output, missing artifact, and check closure. | Keep general failure handling disabled if evidence is incomplete. Preserve #468's documented bounded behavior rather than guessing outcomes. |
| P7: reusable call expansion | Admit a late matrix of call invocations while preserving scoped output projection and flattening each concrete invocation through existing lowering. | P4/P6a/P6b and source-closure proof. Nested calls, unequal invocation graphs, depth/job budget, false call guard, ambiguous outputs. Keep invocation-level max-parallel unsupported unless separately proved. | Re-enable call-matrix rejection; ordinary late matrices and input forwarding stay available. |
| P8a: job concurrency admission | Support late job groups, then late max-parallel limits as a separate syntax change. | P5/P6b, E4, and confirmed scheduler contract. Cross-stage group collisions, competing builds, failure, and cancellation; no cancellation coordination added. | Restore late field rejection without changing existing queued jobs. |
| P8b: called-workflow gate lifetime | Admit gated calls after readiness and hold the gate through all late members. | P6a/P6b/P8a and E4. Enclosing/prerequisite group collision, skipped call, cancellation, and gate close after every late member. | Restore deferred gate-crossing rejection. Do not change existing gates mid-build. |
| P9: late environment policy | Support late top-level names only after agreed policy snapshot/selection authorization and backend rollout. Reusable environments remain a separate compatibility decision. | P6b/P8 as required by gate combinations, E5, and security/platform approval. Policy drift, shared gate race, prefix collision, missing API, denial, and deployment retry tests. | Restore late-name rejection for new builds. Never fall back to an unprotected job or reuse another build's approval. |

#468 need not wait for the general IR, live policy snapshot design, joins, or
environment features. It should retain its own explicit restrictions and prove
its existing hosted assumptions. P0/P1 can land before it; P2a/P2b should adapt the
landed implementation rather than maintain a competing continuation path.

For every format change, pin the emitting compiler and consuming continuation
binary as one distribution. Reject unsupported schema/version pairs before
side effects. Test an old in-flight build after deploying a new importer.
Prefer a small, default-off admission switch for new semantics if rollout
needs one; do not build a permanent dual compiler or silently fall back after
partial publication. Rollback affects **new admission**, not existing builds.

Each implementation PR updates the relevant compatibility/security/CLI guide
when behavior changes. The offline `validate` and `compile --format ir-json`
paths should explain unresolved requirements without pretending execution took
place. Preserve #468's refusal to emit a complete `--format pipeline` for a
continuation workflow until a separately specified export contract exists.
When complete, move durable contracts to owning product/package docs and remove
this plan.

## Proofs and decisions required before dependent code

The recommendations above intentionally leave the following release blockers
open. Record actual build URLs, agent/server versions, observed step attributes,
artifact producer IDs, and check outcomes in Linear; do not replace them with
“the build was green.” Hosted runs require separate authorization and access.

| Proof | Experiment and falsifying outcome | Decision it unlocks |
| --- | --- | --- |
| E1: dependency closure and liveness | Upload `A`, then `A` uploads `B` plus its descendant and another uploader; include a join and a workflow close gate. Inspect explicit and inherited dependencies, execution order, and group placement. Exercise a failed and a canceled producer. A successor waiting on itself/future work, early close, or canceled dependency preventing the intended stage falsifies the proposed shape. | #468 transitive dependency/group assumptions and P5 component scheduling. Buildkite documents inherited upload dependencies, but not correctness of this complete graph. |
| E2: commit and replay | Lose the response after application; retry the coordinator; run two attempts concurrently; collide one job/gate key; include altered queue/dependency/skip/child-stage content under the same key. Interrupt before and after artifacts, pipeline application, and receipt publication. Observe all expected steps and actual artifact provenance. Test just below/above upload limits; do not assume multiple uploads form a transaction. | Whether ordinary upload + complete reconciliation is sufficient. Require server confirmation of atomic application, not just the agent's retry UUID. If applied state cannot be identified safely, use a server primitive such as PB-2819 before general joins. |
| E3: terminal results and checks | Fail, cancel before start, terminate during execution, runtime-skip, and native-skip each producer; retry a producer both before and after expansion. Inspect manifest availability and provider checks independently. Run an `always()` descendant and a false guard with malformed unused matrix data, distinguishing data error from prohibited expression syntax. | Representation of compiler-decided skips, missing evidence, check closure, and general failure handling. Do not equate `allow_failure` with guaranteed execution after cancellation. |
| E4: authority and gate lifecycle | Property-test materialization with reordered independent branches and repeat inputs. Compare final static-equivalent plans. In hosted builds, race two invocations in one group; use nested groups and a prerequisite requiring the same slot; append late members while another build queues. Verify locks cover every member without preventing producers from running. | Safe serial/join ownership and late concurrency. Exact observable exclusion is required; labels and elapsed time alone are insufficient. |
| E5: live policy changes | Change a runner mapping or environment policy between admission, materialization, and coordinator retry in an approved test environment. Exercise unsupported protection rules, case-equivalent names, colliding secret prefixes, API 404/denial, private-source denial, and fresh credential denial after a cached snapshot. | Snapshot timing, permitted late environment selection, gate equivalence, revocation behavior, and backend rollout. Recommended default: explicit runners first; no late environment names before this contract exists. |
| E6: scope, bounds, and deterministic progress | Use two branches with different row counts and values, nested calls with shadowed IDs, duplicate/empty rows, conflicting outputs, exactly-at/over every bound, and an output containing new expression syntax. Re-run materialization with different map iteration orders and coordinator attempts. | One owner and stable identity per job; no expression re-interpretation, double budget spending, cross-scope reads, or unchanged infinite continuation chain. |

Before P1 production changes, characterize the existing laws and identify each
site's owner. Before P2b, resolve E2 and snapshot identity for the bounded path.
Before P5/P6, resolve scheduler liveness and terminal-result questions. P8/P9
remain blocked on their distinct scheduler/security contracts even if all
matrix tests pass.

Recommended decisions for discussion:

- Keep exact producer plan-digest binding, whole-build retry after producer
  ambiguity, and final job-plan immutability. Do not introduce late-bound
  runtime dependency lookup.
- Start joins with one stage sequence per overlapping deferred component.
  Accept conservative barriers; measure before adding parallel coordinators.
- Keep current static authority behavior. A value becoming available for
  scheduling must not accidentally change trusted/unknown treatment in token
  analysis or broaden syntax elsewhere.
- Treat newly selected runner/environment policy as a separate admitted input,
  not deterministic fallout from a source digest. Confirm the server contract
  before designing a client-only snapshot claim.
- Keep unsupported result aggregation, cancellation coordination, typed late
  inputs, runtime-container enhancements, and full GitHub scheduling parity
  outside this refactor unless a later slice explicitly owns them.

Local implementation verification should use focused package tests followed by
`mise run check`. Tests should compare outputs against independently derived
plans/keys and expected graphs, not run the same planner twice as their only
oracle. Apply expression soundness, known-value equality, and monotonicity
properties to valid generated expressions at the changed positions. Add
deterministic materialization fixtures for late inputs rather than forcing
network/output-dependent workflows into `smoke:local`'s complete-pipeline path.
See [development verification](../development.md#verify-runtime-behavior) for
existing hosted and differential tools; this plan does not add or run them.

## External contracts used in this proposal

- [GitHub context availability](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#context-availability): legal contexts depend on authored position; direct `needs` excludes transitive dependencies.
- [GitHub job conditions](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idif): job condition precedes matrix expansion.
- [Buildkite dependencies](https://buildkite.com/docs/pipelines/configure/dependencies): upload dependencies propagate; skipped, failed, and canceled dependencies are not interchangeable; ordered concurrency adds scheduling edges.
- [Buildkite pipeline upload](https://buildkite.com/docs/agent/v3/cli-pipeline): duplicate keys are rejected, uploads insert after their uploader, uploads require a running build, and a file is limited to 500 steps. This page alone does not establish a general atomic continuation commit protocol.
