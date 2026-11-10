# Compatibility contract for contributors

This document is for contributors implementing or reviewing GitHub Actions
compatibility. It records implementation obligations, supporting evidence, and
what that evidence does not establish. It is not a supported-feature inventory
or a claim that unreleased code is available to users.

## Documentation ownership

Update the document that owns the changed fact. Link to other documents when
needed; do not copy feature tables, limits, evidence records, or architecture
explanations between them.

| Document | Owns | Update when |
| --- | --- | --- |
| [User compatibility reference](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility) | Shipped support, syntax, defaults, limits, intentional differences, and required setup. | A released feature or correction changes what users can rely on. Include minimum versions and backend prerequisites. |
| This contract | Implementation obligations, observations, evidence provenance, and unresolved parity questions. | Evidence or an implementation decision changes an obligation or its justification. Preserve the input, result, and limits of the evidence. |
| [Expression authority](expression-authority.md) | Expression architecture, execution positions, and authority invariants. | Evaluation, lifecycle ordering, or authority design changes. |
| [Development](development.md) | Test, benchmark, and proof procedures. | Verification tools or procedures change. |
| [CLI reference](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/cli) and [security model](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/security) | Command usage and user-facing trust boundaries. | An option, operator procedure, or security requirement changes. |

Track missing features, rollout, and remaining work in Linear. An implementation change may
require both a contributor update and a companion product-docs PR, but each
should describe only its own part of the contract.

## Compatibility policy

Match exposed, reproducibly observed GitHub Actions behavior, including
surprising behavior. GitHub documentation and source guide investigation; they
do not override a reproducible observation without an explicit reason to differ.
Our parser, implementation, and regression tests cannot establish native GitHub
behavior.

Keep these cases distinct:

| Case | Required treatment |
| --- | --- |
| Observed GitHub behavior | Record the declaration, event or input, execution context, result, and limits of the observation. Preserve immutable fixture and run links when available. |
| Intentional difference | State the difference and its rationale in the user reference. Record the implementation obligation and supporting evidence here. |
| Implementation gap | Describe it as unsupported, not as native rejection or a security restriction. Track delivery in Linear. |
| Unresolved behavior | Preserve the uncertainty. A missing run or null receipt does not distinguish rejection, filtering, cancellation, and non-delivery. |

For example, excluding `pull_request_target` is a deliberate trust-boundary
decision: its privileged base-repository execution model cannot safely be
admitted as ordinary pull request execution. Missing `repository_dispatch`
support is an implementation gap, not the same kind of exception.

### Evidence and availability

Evidence answers different questions. Do not collapse it into one
"runtime-proven" label.

| Evidence | Establishes | Does not establish |
| --- | --- | --- |
| Documentation or schema | The published or declared interface at that revision. | Actual event delivery or execution. |
| Runner or backend source | Behavior of the inspected code path, under its inputs and preconditions. | Server-supplied values, deployment, or execution on a particular platform. |
| Local regression test | The tested implementation behavior. | Native GitHub behavior or production service behavior. |
| Native GitHub run | The behavior of its exact fixture and execution context. | Untested activities, platforms, permissions, or Buildkite behavior. |
| Buildkite run | The behavior of the selected runtime and services in that build. | Fleet-wide rollout or native GitHub equivalence. |

Compilation, hosted-policy admission, and execution are separate results.
Generated event snapshots are test inputs, not evidence that every real payload
works. A successful job does not prove an advisory API persisted its result;
inspect the stored summary, artifact, or annotation separately. See
[runtime verification](development.md#verify-runtime-behavior).

Public support requires a released runtime and any required backend capability.
A merged change, a published binary, or an enabled test organization alone does
not establish public availability. Keep version and availability
qualifications in the user reference until their prerequisites are established.

## Implementation guarantees

The following obligations explain boundaries that a refactor or compatibility
extension could accidentally remove. They are not a second feature inventory.

### Event identity and workflow selection

Preserve the original event identity through compilation, deferred uploads,
checkout, and runtime context. Do not substitute a current branch tip, default
branch, or synthetic event to repair contradictory provenance.

- Linked webhook data must agree with the build and selected immutable workflow.
  An explicit event snapshot is a separate input path, not linked-webhook proof.
- Rebuilds that require the original payload must fail when it is unavailable.
- Default-branch events execute the source repository's resolved default-branch
  commit, not an object mentioned inside the payload. Examples include a fork,
  wiki commit, Pages build commit, or branch-protection rule pattern.
- Release tags resolve to their peeled commits. Merge-group destruction retains
  the recorded speculative head identity after the queue ref disappears.
- Event selection does not grant repository-token, secret, or checkout authority.

The CLI owns import selection in [plugin configuration](../internal/cli/plugin.go)
and [upload](../internal/cli/upload.go). Event validation lives in
[compiler event handling](../internal/compiler/event.go). Backend delivery is a
separate contract; local tests with constructed events do not verify it.

Manual `workflow_dispatch` selection must accept branch and tag workflow refs
and preserve the event identity without requiring a webhook payload. The server
[selects dispatch when declared and supplies workflow identity](https://github.com/buildkite/buildkite/blob/ad7682856e029374db39e0e8e6c39c327bf929b0/app/models/build/manual.rb#L54-L143),
but does not supply dispatch inputs. Use declared defaults when inputs are absent.
[Mailcatcher build 1](https://buildkite.com/sj26-test/mailcatcher/builds/1)
confirmed dispatch identity reached the importer; it failed at event admission,
not workflow execution. The
[plugin regression](../internal/cli/plugin_test.go) covers branch and tag imports,
default inputs, and supplied payload inputs. It does not establish a successful
hosted run or UI support for supplying inputs.

Path filtering requires verified repository identity and bounded Git evidence.
Non-path exclusions can retain their conditions when diff history is unavailable,
but identity failures remain errors. Do not turn missing history into an empty
diff. Evaluate renames as deletion of the old path and addition of the new path;
a copy that leaves its source unchanged contributes only the destination path.
Keep local diffs independent of Git rename-detection settings. The
[path-filter implementation](../internal/cli/path_filters.go) and
[regression tests](../internal/cli/path_filters_test.go) own those checks.

Observe path-filter admission independently of command completion: uploading
error steps can succeed. [Runtime telemetry](development.md#path-filter-telemetry)
must not change selection or fetch provider data solely for measurement.

Native GitHub evidence from October 27, 2026 used `push` and `pull_request`
workflows filtering the old path, new path, both, or neither. Pure renames,
renames with edits, and unrelated additions/deletions selected old, new, and
both-path workflows. Copies retaining the source selected only new and
both-path workflows. All 22 selected runs passed; the ten absent runs had
same-commit positive controls. Fixtures:
[pure rename](https://github.com/sj26-testier/gha-e2e-onboarding-r-c8f18d8ff7b4/tree/3b1dd8632e281b521186944f92321aa86df4a73b),
[edited rename](https://github.com/sj26-testier/gha-e2e-onboarding-r-c8f18d8ff7b4/tree/603b1823fad13c42b4ff0ddb6f1b3d7e5116c406),
[unrelated addition/deletion](https://github.com/sj26-testier/gha-e2e-onboarding-r-c8f18d8ff7b4/tree/26d77b4c4e3ed81e1623911074733445c2ca6cc4),
and [copy](https://github.com/sj26-testier/gha-e2e-onboarding-r-c8f18d8ff7b4/tree/8015e0d8c2215d3b14eee51b6b01a29a1ecfe96f).
The [old-path push run](https://github.com/sj26-testier/gha-e2e-onboarding-r-c8f18d8ff7b4/actions/runs/40449274845)
and [old-path PR run](https://github.com/sj26-testier/gha-e2e-onboarding-r-c8f18d8ff7b4/actions/runs/40449278164)
demonstrate that rename sources participate in filtering. These small fixtures
do not establish GitHub's file ordering or behavior beyond diff limits.

For a verified, nonforced branch creation with an explicitly empty webhook
`commits` array, evaluate an empty diff by comparing `after` to itself. Preserve
repository, workflow, head, and non-shallow checks. A missing array or a nonempty
array omitting `after` remains an error; do not use `head_commit` paths or the
head's parent as a substitute. Telemetry records the actual self-comparison base.

The retained October 28, 2026
[new-ref fixture](https://github.com/sj26-testier/gha-e2e-onboarding-r-21ee3c030334/tree/a956e23a3f13890c5cbec94c5299ec551f4f88c6)
created `hist-existing` at an already-published side-branch tip. Its webhook had
zero `before`, `created: true`, `forced: false`, and `commits: []`, although
`head_commit` listed `existing-only.txt`. Only the
[unfiltered control](https://github.com/sj26-testier/gha-e2e-onboarding-r-21ee3c030334/actions/runs/40630060360)
ran; filters for that file, a default-branch-only file, and an unrelated file did
not. The plugin regression checks empty-diff selection for `paths` and
`paths-ignore`, the unfiltered control, source rejection, and completion telemetry.
This evidence does not establish force-push comparison semantics or permit
missing history to become an empty diff.

### GitHub server identity

For selected GHES imports, the backend supplies `GITHUB_SERVER_URL`
from the authorized App connection, with precedence over build and step input.
Never infer GitHub identity from an arbitrary `BUILDKITE_REPO` host. Persist the
canonical HTTPS origin as `server_url` in event snapshots and digest-bound job
plans; omission retains github.com behavior.

Checkout, credential-helper scoping, workflow-token `repo_url`, expression
contexts, and runtime environment must use that same origin. GHES REST and
GraphQL URLs use `/api/v3` and `/api/graphql`. The origin identifies the requested
repository; it does not authorize access. The backend still binds each token
to the job, repository, App installation, and workflow policy at the build commit.

Environment and configuration-variable lookups also preserve the event origin,
including batched upload lookups and memoization. They use only the job-scoped
Agent API; GitHub installation credentials remain server-side. GHES requires an
existing code-access App installation with `actions:read` and `environments:read` for
environments (including their variables), and `actions_variables:read` for
repository/organization variables. No lookup feature flag is required. Deploy
backend support before this client.
Local regression tests establish host preservation and fail-closed lookup
behavior, not live GHES API availability or installation grants.

Source references, action locks and reusable-workflow provenance carry their own
optional canonical `server_url`. An omitted source origin means GitHub.com,
never the event origin. Preserve it through pinning, nested self sources,
deferred plans, diagnostics and runtime materialization. Bind memoized sources,
ref caches, resolution snapshots and tree manifests to that origin. Keep existing
hostless GitHub.com cache keys; never reinterpret them as GHES.

New ordinary remote references select the root event's GitHub source host,
including nested composites, remote reusable calls and deferred action locks.
This selection does not reinterpret existing hostless locks. Local reusable
paths and `$/` references retain their containing source's pinned identity.
Non-GitHub events keep GitHub.com action sources; event and token context do
not change with the source host.

On GHES 3.22.2 with runner 2.334.0, the immutable
[mixed-source fixture](https://buildkite-ghe.com/sj26-testier/gha-mixed-caller-20261104-10f4/commit/681944f43e80289bafd238b33b75f394ee8b07a8)
used a public checkout mirror, public/private composites, a private-to-public
child action and a private reusable calling a local nested workflow.
[Sharing disabled](https://buildkite-ghe.com/sj26-testier/gha-mixed-caller-20261104-10f4/actions/runs/14)
rejected the reusable before scheduling;
[sharing enabled](https://buildkite-ghe.com/sj26-testier/gha-mixed-caller-20261104-10f4/actions/runs/16)
passed both jobs at the same caller commit. A
[public-only control](https://buildkite-ghe.com/sj26-testier/gha-mixed-caller-20261104-10f4/actions/runs/17)
passed with sharing disabled. Action archive URLs used the GHES
`/api/v3/repos/.../tarball/<SHA>` endpoint, including the checkout mirror.
Anonymous metadata and archive requests to the public fixture returned 401.
These observations establish same-host selection and sharing admission in this
lab, not GitHub Connect fallback or Buildkite source-token authorization.

Anonymously readable GHES sources resolve through the selected origin's
`/api/v3` and download immutable `/repos/<owner>/<repo>/tarball/<SHA>` archives.
Archive redirects stay on that HTTPS origin. There is no GitHub.com or Connect
fallback. Keep GitHub.com endpoints, credentials and legacy cache paths unchanged;
separate GHES tree directories, mutable refs and API rate-limit budgets by origin.
Tree-cache maintenance must account for every origin without evicting leased trees.

Source identity does not grant access. GitHub.com metadata tokens and reusable
Git credentials must never authorize GHES sources. Authentication-required GHES
repositories, including this lab's public repositories, remain unsupported;
the hosted private-reusable opt-in guard stays in place. Never attach GHES
workflow tokens to GitHub.com requests or infer a GitHub.com self source from a
GHES event.

The [source regressions](../internal/action/source/source_test.go) cover host
collisions, credential isolation, denied requests, redirects and cache eviction.
The [runtime regression](../internal/runtime/actions_test.go) compiles remote
and nested reusable workflows, then downloads and executes nested composite
actions from a cold runtime cache using a local HTTPS GHES-shaped fixture.
These tests establish the anonymous transport contract, not live authenticated
GHES imports, webhook delivery or job-bound source-token authorization.

Native integration admission includes the source origin. A same-name GHES
repository must not receive GitHub.com's native adapter or cache substitution.
Explicit GitHub.com and omitted origins have the same integration identity.
Job-plan schema v2 accepts the optional source fields; the prior strict decoder
rejects them as unknown rather than ignoring their host. These checks do not
establish authenticated GHES source support.

The [GHES spike](https://ampcode.com/threads/T-01a15c91-9015-754b-a5f3-bebba85e76e8)
ran a real GHES 3.17.14 repository against an isolated Buildkite backend and a
candidate runtime. The [read-only fixture](https://buildkite-ghe.com/sj26-testier/gha-e2e-ghes-r-9059cd8ae32b/commit/0ec96bc9c7fdf4497bb0dd569dfd67ef5c032a2b)
denied issue writes despite a broader App grant; the
[explicit-write fixture](https://buildkite-ghe.com/sj26-testier/gha-e2e-ghes-r-9059cd8ae32b/commit/28e75f8f6ee03fc28756305573da91898f439c54)
created and closed one issue. Its
[same-repository PR](https://buildkite-ghe.com/sj26-testier/gha-e2e-ghes-r-9059cd8ae32b/pull/2)
was denied a workflow token before steps ran. These runs establish the tested
Buildkite paths, not native GitHub execution, all App permissions, backend-importer
parity, or released availability.

The [combined execution fixture](https://buildkite-ghe.com/sj26-testier/gha-e2e-ghes-r-9059cd8ae32b/commit/50e1c53abd0917fd64ea168ea7b5f5b0209df73d)
passed through a signed push, agent importer and runtime with backend import
enabled. It checked exact-SHA checkout, server/API identity, a one-repository
token scope and denied hook access. This used an unpublished CLI launcher and
local Agent API proxy; it does not prove released plugin acquisition.

### Expressions and authority

Use the shared positional expression profiles and normalized execution program.
The runtime must not recreate authority decisions by reparsing action metadata.
The complete architecture and invariants live in
[Expression authority](expression-authority.md). Update that document rather
than keeping a second inventory of expression rules here. Native-source
observations and their limits are recorded under
[expression and composite source evidence](#expression-and-composite-source-evidence).

Do not weaken public permission restrictions to make a workflow compile. A
token-requesting job with an explicitly empty effective permission map fails;
it does not silently receive an empty token. See
[permission regression tests](../internal/compiler/bundle_test.go).

### Deferred jobs, outputs, and results

Keep independently bounded values independent: a job output, matrix JSON, a
matrix string, and a runner selector do not share one byte limit. The
[result manifest](../internal/transport/model.go),
[matrix decoder](../internal/compiler/runtime_matrix.go), and
[runner decoder](../internal/compiler/runtime_runs_on.go) own their respective
limits. The user reference owns the released values.

Result manifests, not optional Buildkite metadata copies, carry authoritative
outputs. Large values must survive manifest transport even when they are too
large to mirror into metadata. Test both boundaries and aggregate size; see
[manifest tests](../internal/transport/model_test.go).

Deferred uploads must preserve source snapshots, action locks, graph budgets,
downstream ownership, and producer identity. Expansion accepting an empty array
does not mean the complete workflow is valid: recompilation rejects an empty
matrix. Test the CLI's result and absence of uploaded work, not only the decoder.
See [deferred upload tests](../internal/cli/upload_stage_test.go).

Do not silently choose between ambiguous retried producers or matrix outputs.
Keep logical Actions results separate from the Buildkite job's scheduling state,
including skipped and tolerated-failure results.

Runtime job and reusable-workflow call conditions may recover only a failure
status when the matching generated step remains `finished` with outcome
`hard_failed` across successful artifact searches confirming absence from every
attempt. Require the expected step key and plan digest, emit a warning, and
never publish a replacement producer receipt. Supply no outputs or artifacts
from that status. Deferred inputs, matrices, and runner selection still require
verified receipts. Query, download, verification, and ambiguous-state failures
remain errors; see [terminal-result tests](../internal/transport/terminal_results_test.go).
Recovery must not change the producer's Buildkite failure or dependency scheduling.

### Workflow concurrency across deferred matrices

Keep the root workflow's opening and closing markers adjacent in the ordered
concurrency queue. The closing marker must depend on every initial job and
deferred uploader with `allow_failure: true`. Buildkite
[extends dependencies on an uploader to its uploaded work](https://buildkite.com/docs/pipelines/configure/dependencies#defining-explicit-dependencies).
Do not upload another gate pair from a continuation: it would queue behind the
closing marker that is waiting for that continuation's jobs.
Reject member jobs sharing the root group before omitting that gate from a
continuation, including when further matrix stages remain to be expanded.

The inspected backend [adds implicit uploader dependencies](https://github.com/buildkite/buildkite/blob/0e2616ba728b4bdb0afa237c4963d8afce870bc7/app/models/build/pipeline_changer.rb#L630-L663)
only to top-level steps without dependencies or an explicit `depends_on`.
[An explicit empty list also suppresses that default](https://github.com/buildkite/buildkite/blob/0e2616ba728b4bdb0afa237c4963d8afce870bc7/app/models/build/pipeline_config.rb#L423-L465).
Separately, the uploader [extends incoming dependencies to uploaded work](https://github.com/buildkite/buildkite/blob/0e2616ba728b4bdb0afa237c4963d8afce870bc7/app/models/build/pipeline_changer.rb#L681-L747),
excluding implicit `UPLOAD` dependencies and copying the failure policy.
The closing marker's explicit dependency therefore follows every chained
upload, even when uploaded jobs have their own explicit dependencies. Preserve
the existing graph; no additional ordering dependencies are needed.
Its [ordered queue](https://github.com/buildkite/buildkite/blob/ac9b91a26e2273f98e457ef3a48c82a7df106746/app/models/concurrency_group/queue_processor.rb#L175-L231)
counts waiting markers ahead of competing runs. Dependencies
[wait for automatic retries](https://github.com/buildkite/buildkite/blob/ac9b91a26e2273f98e457ef3a48c82a7df106746/app/models/step/dependency/state.rb#L92-L119),
but [finished markers are not reopened](https://github.com/buildkite/buildkite/blob/ac9b91a26e2273f98e457ef3a48c82a7df106746/app/models/job/state/resolver.rb#L66-L72)
when an individual job is retried later.

On November 3, 2026, [run A](https://buildkite.com/sj26-testier/pb-4387-deferred-concurrency-contract/builds/1)
held its group until its deferred jobs and automatic retry finished. Competing
[run B](https://buildkite.com/sj26-testier/pb-4387-deferred-concurrency-contract/builds/2)
started at 04:20:35.952 UTC, after A's closing marker finished at 04:20:31.072.
Manually retrying A's failed member then ran it from 04:20:57.718 to
04:21:27.949, overlapping B. Dynamic upload itself does not cause early release;
individual retries after release do.

[Grouped chained uploads](https://buildkite.com/sj26-testier/pb-4387-deferred-concurrency-contract/builds/3)
kept their marker waiting until the last descendant finished at 04:24:34.025 UTC;
the [competitor](https://buildkite.com/sj26-testier/pb-4387-deferred-concurrency-contract/builds/4)
entered at 04:24:45.902. A [failed uploader](https://buildkite.com/sj26-testier/pb-4387-deferred-concurrency-contract/builds/5)
released its group before [the next run](https://buildkite.com/sj26-testier/pb-4387-deferred-concurrency-contract/builds/6)
entered.

Keep ordinary manual and automatic retry behavior. As with the existing
static-only marker translation, concurrency is best-effort for manual retries
after the closing marker finishes: retried jobs can overlap a later run.
Do not reject the workflow or prohibit retries for this case. Preserve the
gate across every chained stage without changing plan authority or producer
identity; see
[CLI regressions](../internal/cli/upload_stage_test.go).
Static-only workflow retries, called-workflow gates, output-derived runner
selection, output-derived scheduling, and cancel-in-progress are not broadened.
These scheduler probes and local CLI tests do not prove native GitHub parity or
successful execution of the complete PostHog workflow.

### Action sources and native adapters

Resolve mutable action refs to commits and lock the extracted source tree.
Runtime verification must use that lock, not resolve the ref again. See
[source resolution](../internal/action/source/source.go) and its
[tests](../internal/action/source/source_test.go).

Source lookup and credential behavior must remain separate:

- The importer action-source token authenticates eligible public metadata
  lookups; it does not add private action support or workflow-token authority.
- Repository visibility checks and rate-limit suppression are resolver-local.
  Cached refs and exact commit pins remain usable when new API requests are
  suppressed. Do not persist a temporary rate-limit failure as source identity.
- The compilation-scoped missing-ref cache retains failures only after all
  applicable API lookups return HTTP 404. It must not retain authentication,
  transient, or cancellation failures, suppress permitted Git fallback, or omit
  an affected action's diagnostic. Later compilations retry missing refs unless
  the caller explicitly selects a durable resolution snapshot; see the
  [missing-ref regression](../internal/compiler/missing_ref_resolutions_test.go).
- Docker action arguments are ordered literal arguments after expression
  evaluation, not shell text or Docker options. `--privileged` in an argument
  cannot become a container privilege flag.
- Accepting inert top-level action metadata `env` must not evaluate its contents,
  alter the process environment, or request credentials.

Native checkout and artifact adapters replace upstream JavaScript. Their known
commit profiles and unknown-commit fallback contracts are not equivalent to
executing arbitrary upstream commits. Cache actions are different: they execute
upstream bundles with cache credentials. An unknown cache commit is substituted
with an admitted release, not run under an adapter fallback. Preserve the
requested and selected identities and their diagnostics. The
[integration catalog](../internal/action/integration/catalog.go) owns that split.

Checkout ref expressions can already fold to literal branches during event
reduction. The identical expressions in PostHog's
[migration check](https://github.com/buildkite/posthog/blob/b9a0729f733e6dd170017677bb339608489ec3d2/.github/workflows/ci-backend.yml#L1503-L1510)
and [OpenAPI check](https://github.com/buildkite/posthog/blob/b9a0729f733e6dd170017677bb339608489ec3d2/.github/workflows/ci-backend.yml#L2027-L2035)
select the same-repository PR branch or fall back to `github.ref`. Local
reproduction on v0.123.0 accepted the branch and push cases, but rejected the
fallback pull ref, not the expression syntax. Both original checkout declarations
also failed with the archived synthetic `buildkite/posthog` event selecting
`refs/pull/2688/head`; both compile with pull-ref support and unchanged
`network` and `provider-token-read` capabilities, without a workflow token.

Admit explicit `refs/pull/<positive-number>/head` and `/merge` in the event
repository. Fetch the selected ref into `refs/remotes/pull/` at every supported
depth, including full history, then check out its fetched commit detached.
Do not replace an explicit pull ref with the event SHA or a same-named branch.
An omitted or empty ref still uses the exact event SHA. Keep repository
credential scope, token authority, and needs-output SHA restrictions unchanged.
The pinned checkout [input handling](https://github.com/actions/checkout/blob/de0fac2e4500dabe0009e67214ff5f5447ce83dd/src/input-helper.ts#L59-L80)
and [ref handling](https://github.com/actions/checkout/blob/de0fac2e4500dabe0009e67214ff5f5447ce83dd/src/ref-helper.ts)
distinguish explicit mutable refs from omitted-ref commit pinning. This is source
evidence, not a new native or hosted run. The
[compiler regression](../internal/compiler/actions_test.go) checks selection and
unchanged authority; the [local Git regression](../internal/runtime/checkout_test.go)
uses different event, branch, pull-head, and pull-merge revisions to check fetches,
history, and outputs. Neither establishes complete PostHog workflow success.

Skip native adapter input validation only when the whole job is proven false
after reusable inputs and matrix values resolve. Retain its skipped plan and
action locks. Reachable jobs, runtime-dependent job conditions, and false
individual steps still require adapter validation. The
[compiler regression](../internal/compiler/actions_test.go) covers checkout and
both artifact adapters, including an unreachable download to
`${{ runner.temp }}/digests`. This is local compilation evidence, not native
GitHub or hosted execution evidence.

The GHES `upload-artifact` contracts for
[v3.2.2](https://github.com/actions/upload-artifact/blob/c6a366c94c3e0affe28c06c8df20a878f24da3cf/action.yml)
and [v3.2.2-node20](https://github.com/actions/upload-artifact/blob/c6a3b2bd78b3985e4b2f15397fec357f0fd808de/action.yml)
declare the same inputs and no outputs. Preserve their hidden-file exclusion by
default and explicit opt-in; do not substitute the v7 fallback or recommend an
upgrade to a github.com-only release. Their Node runtime and bundled dependency
changes do not execute in the native adapter. A November 2, 2026
[native GHES 3.22.2 run](https://buildkite-ghe.com/sj26-testier/ghes-source-compare-20261102/actions/runs/2)
round-tripped 22 bytes with the node20 pin and `download-artifact` v3-node20.
That run does not establish hidden-file behavior or hosted Buildkite execution;
[local runtime tests](../internal/runtime/upload_artifact_test.go) cover archive
contents and default/opt-in hidden-file handling.

### Filesystems and artifact extraction

Source archive extraction intentionally omits certain in-repository symlink
aliases; it neither creates nor follows them. This is not general symlink
compatibility. The source digest describes the extracted tree without aliases.
Retain rejection of link chains, escaping targets, hardlinks, special files,
duplicate or case-colliding paths, and aliases that are ancestors of entries.

Workflow working directories remain workspace-bound. Verified composite actions
may also select absolute directories in their own source tree. Do not extend
that permission to workflow steps or permit symlink escapes. See
[shell execution](../internal/runtime/shell_execution.go) and
[remote-composite tests](../internal/runtime/shell_execution_test.go).

Artifact downloads select verified direct producers, validate and stage every
archive before changing the destination, and apply one aggregate file budget.
Pattern matching must reject case-insensitive duplicate artifact names. Preserve
deterministic exact-path overwrite order and extraction protections. See
[download execution](../internal/runtime/download_artifact.go) and
[staging tests](../internal/runtime/download_artifact_test.go).

### Credentials and cache tools

Keep repository checkout credentials, action-source credentials, workflow
`GITHUB_TOKEN`, ordinary Buildkite secrets, cache credentials, and OIDC separate.
An implementation convenience must not expand their recipients or lifetimes.
The [security model](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/security)
owns the user-facing boundaries.

Workflow subprocesses default to `GIT_TERMINAL_PROMPT=0`, matching the
[agent's noninteractive Git setup](https://github.com/buildkite/agent/blob/286b70ebfe0850f845bf95e1a6e32f989ba049ac/internal/job/executor.go#L916-L917).
This is a runtime default, not exact checkout-action parity: upstream
[checkout scopes its prompt settings to its Git subprocesses](https://github.com/actions/checkout/blob/3d3c42e5aac5ba805825da76410c181273ba90b1/src/git-command-manager.ts#L620-L654)
and [persists credentials for later commands by default](https://github.com/actions/checkout/blob/3d3c42e5aac5ba805825da76410c181273ba90b1/README.md#checkout-v6).
Do not forward agent job credentials to repair an inherited global Git helper.
A local Linux reproduction with agent v3.129.0 and real `git credential fill`
showed that the helper reports `Missing job-id` in the restricted workflow
environment, then Git waits for input through a parent PTY. With the default,
the same invocation fails with `terminal prompts disabled`. Explicit helper
credentials still resolve; see [shell regression coverage](../internal/runtime/shell_execution_test.go).
This proves prompt prevention, not authenticated post-checkout access, native
GitHub behavior, or the cause of any particular customer build.

Workflow-token requests use the requesting workflow's top-level repository
permission policy. Job and called-workflow maps do not narrow that token;
`id-token` narrowing is a different mechanism. Retry the same authorized request
only under the bounded transient-failure policy in
[token acquisition](../internal/runtime/github_token_service.go). Do not convert
authorization denials into retryable failures or replace specific server
diagnostics with generic App-permission advice.

Publish OIDC trust guidance only when a job makes an authorized token request
and finishes with a failure conclusion. Permission alone is not a request;
success, skipped, and cancelled conclusions must not publish it.
Include post-action requests. Use the shared build-scoped context
`buildkite-gha-oidc-trust`, replacing rather than appending, so failed jobs
update one annotation per build. Keep the message conditional and token-free:
job failure does not establish a downstream trust rejection. Preserve mint
status and failure attribution. Local
[OIDC tests](../internal/runtime/oidc_token_service_test.go) cover request
tracking and publication parameters, not downstream cloud trust.

Cache mode is client behavior, not server authorization. Clients that ignore
`ACTIONS_CACHE_MODE` do not acquire its restrictions. A subprocess-only synthetic
`GITHUB_SERVER_URL` helps selected setup-action cache clients use cache v2; it
must not change the workflow expression context. The
[catalog](../internal/action/integration/catalog.go) selects those setup actions
by repository and root path, not by an audited commit set. Do not claim every
admitted revision and bundled dependency has been audited. Record exact
revisions when establishing that the override affects only caching.

On Windows, cache-tool lookup excludes workflow-controlled `PATH`,
`GITHUB_PATH`, package-manager shims, and tool-selection environment overrides.
The system-volume `tools\zstd\zstd.exe` exception stages only verified bytes for
each lifecycle invocation. It must not expose the writable source directory or
sibling executables and libraries. The
[user cache reference](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#actions-cache-action)
owns the supported tool version and digest.
Tool isolation is not a sandbox against administrators or processes sharing the
runner account. See [cache lifecycle tests](../internal/runtime/cache_service_test.go).

## Evidence records

These records preserve the evidence behind the former combined reference.
They distinguish reported observations from independently inspectable receipts.
They are not an exhaustive conformance suite.

### Compile-known job controls

The [GitHub context-availability table](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#context-availability)
admits `github`, `needs`, `vars`, and `inputs` in `strategy`, with `matrix` and
`strategy` additionally available to job `timeout-minutes`. Neither position
admits step status functions, `hashFiles`, `env`, or secrets. This is documentation
evidence, not a native execution receipt.

Preserve those positional boundaries when evaluating compile-known controls.
Timeout expressions must produce a finite number greater than zero and at most
360; retain fractional minutes. Max-parallel expressions must produce a positive
integer representable by the existing integer field; do not apply the deferred
path's 256 limit to static values. Reject numeric strings without explicit
conversion, booleans, null, collections, and unresolved values. Input values are
data, never new expression source, and evaluation diagnostics must not quote them.

The [compiler and generated-pipeline regressions](../internal/compiler/job_controls_test.go)
cover constants, event/variable values, dispatch inputs, nested reusable inputs,
per-row timeout, and invalid controls. The
[deferred regressions](../internal/compiler/runtime_scheduling_test.go) retain
the existing needs-derived max-parallel contract and evaluate matrix-backed
timeout only once rows are available. Direct needs-derived timeout, deferred
reusable input controls, and expression-valued max-parallel on reusable workflow
invocations remain unsupported. Matrix fail-fast is separate. These local checks
do not establish native parity, hosted execution, or released availability.

### GitHub identity IDs

The [GitHub context reference](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#github-context)
defines actor, repository, and repository-owner IDs as strings. Derive them only
from positive integer IDs in the GitHub event's `sender`, `repository`, and
`repository.owner` objects. Require their login/full-name fields to match the
selected event actor and repository, case-insensitively. Preserve integer
precision; never resolve names through another provider or substitute Buildkite
identities. Missing or mismatched data leaves the field unavailable, and an
evaluated read fails rather than returning a fabricated or empty ID.

Compilation, action authority planning, and runtime use the same extraction.
Retain the digest-bound event payload when an ID read remains at runtime;
this does not authorize an event file or credentials. Known-false ID guards
must prune token authority, including composite shell guards. Unknown guards
remain conservative. IDs do not broaden a position's allowed contexts.

The [compiled regression](../internal/runtime/runtime_test.go) exercises graph
fields, job and reusable-call conditions, job environment and outputs, composite
defaults, conditions, and shell execution with distinct IDs, including
`9007199254740993`. [Source checks](../internal/plan/plan_test.go) reject missing,
malformed, mismatched, and other-provider identities;
[expression checks](../internal/expression/engine_test.go) cover unavailable
values, static indexing, payload retention, and token effects.
[Deferred-matrix checks](../internal/compiler/runtime_matrix_continuation_test.go)
require the same ID guards in action preflight and expanded jobs, rejecting
reachable or unknown composite child-input token branches. These are local
implementation checks against documented fields, not native execution or
backend delivery evidence for every event, manual build, or retry path.

### Runner name

GitHub's [runner context](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#runner-context)
defines `runner.name` as the executing runner's name. The pinned
[runner implementation](https://github.com/actions/runner/blob/310a370bf3605f5b092f8af63b3126627de2a210/src/Runner.Worker/JobRunner.cs#L165-L167)
reads its configured agent name, not its hostname.

Use the executing Buildkite agent's
[`BUILDKITE_AGENT_NAME`](https://buildkite.com/docs/pipelines/configure/environment-variables#BUILDKITE_AGENT_NAME)
in `run-job`. Generated jobs invoke that command in the executing agent's
environment; the [runner-user bootstrap](../internal/buildkite/experimental_runner_user.go)
preserves it across `sudo`. The plugin imports workflows, not the executing
agent's identity. Never capture the importer's name in a plan or infer a name
from hostname, `RUNNER_NAME`, or workflow environment values.

Keep the name unknown during compilation and authority analysis. Supply it
to workflow/composite step fields, action input defaults, lifecycle conditions,
and job outputs. Graph fields, job/call conditions, job environment/defaults,
and service fields retain their restrictions. An absent name stays unavailable;
evaluated direct reads fail rather than inventing an identity. No new context
roots, credential authority, or `RUNNER_NAME` export are implied.

[Expression tests](../internal/expression/engine_test.go) cover allowed/rejected
positions and token-branch analysis. [CLI tests](../internal/cli/run_job_test.go)
cover supplied/absent names and environment overrides; [compiled execution tests](../internal/runtime/runtime_test.go)
cover step conditions, composite defaults, and outputs across two agents using
one serialized plan. These are local checks, not hosted or native-run evidence.

### Runner tool-cache paths

The pinned runner [creates the tool cache and sets its context](https://github.com/actions/runner/blob/1ddb758ed014660b5d2220a5dc6b0e9957a9f96b/src/Runner.Worker/JobRunner.cs#L174-L176)
before action execution. PostHog's pinned
[Python setup](https://github.com/buildkite/posthog/blob/b9a0729f733e6dd170017677bb339608489ec3d2/.github/actions/setup-python-cached/action.yml#L46)
and [pnpm setup](https://github.com/buildkite/posthog/blob/b9a0729f733e6dd170017677bb339608489ec3d2/.github/actions/pnpm-install/action.yml#L73)
pass `runner.tool_cache` subdirectories to cache actions. These are source
references, not native execution receipts.

Resolve `runner.tool_cache` from the same validated directory as
`RUNNER_TOOL_CACHE`, never from importer or workflow environment values.
Keep it unknown during compilation and authority analysis. Setup must supply
it before action defaults and steps evaluate it; graph-time fields and
pre-setup conditions remain unavailable.

Job containers use a writable per-job cache inside the existing temp mount,
with container-translated expressions and environment values. Do not mount
the configured host tool cache or widen working-directory containment.
Docker actions retain their separate environment and mount policy.
[Compiled action-input tests](../internal/runtime/runtime_test.go) cover shared
setup/consumer paths and cleanup; the [non-root container test](../internal/runtime/containers_test.go)
covers writable translated paths without changing the configured host cache.
These checks do not establish cache-service persistence, preinstalled tool
availability, or successful execution of the complete PostHog workflow.

### Run-attempt conditions

The pinned [PostHog backend workflow](https://github.com/buildkite/posthog/blob/b9a0729f733e6dd170017677bb339608489ec3d2/.github/workflows/ci-backend.yml#L3690-L3734)
uses `github.run_attempt == '1'` for telemetry steps and the
`capture-test-selection` job, alongside actor, event, and prerequisite guards.
The [November 3 compatibility capture](https://ampcode.com/artifacts/fresh-compatibility-and-performance-comparisons-november-202-3f3daada-c84c-44d4-aab8-c6b688fa1cf1)
records v0.119.0 rejecting these conditions. These are authored-workflow and
local-admission observations, not native execution receipts.

Conditions use the existing runtime mapping: `BUILDKITE_RETRY_COUNT + 1`,
as a string. GitHub counts attempts of a workflow run, including reruns of
selected jobs. The Buildkite per-job mapping is an approximation, not exact
parity. Preserve that distinction rather than inventing a workflow attempt
counter. An absent or invalid retry count leaves the value unavailable and
condition evaluation fails.
Import-time restrictions and authority invariants remain in
[Expression authority](expression-authority.md#expression-profiles).

The [compiled runtime regression](../internal/runtime/runtime_test.go) checks
first-attempt and retry selection for jobs, reusable calls, workflow steps, and
composite steps, plus unavailable identity. The
[expression regression](../internal/expression/engine_test.go) checks residual
conditions, numeric/string comparisons, abstract refinement, and prohibited
contexts. The [compiler regression](../internal/compiler/actions_test.go) checks
conservative token authority and known-false pruning. These local checks do not
establish a complete PostHog run, native parity, or released availability.

### Git email inference on Linux runners

The November 2, 2026 paired [native Ubuntu run](https://github.com/sj26-testier/gha-e2e-onboarding-r-b4f90c10e220/actions/runs/41385914126)
and [Namespace run](https://buildkite.com/sj26-testier/gha-e2e-onboarding-r-b4f90c10e220/builds/8)
used the same [immutable fixture](https://github.com/sj26-testier/gha-e2e-onboarding-r-b4f90c10e220/blob/1d923acba22cd86ead81a6e30a0bcd16e953f22d/.github/workflows/identity.yml).
Neither supplied Git identity configuration or environment, before or after
checkout. Both had an empty OS user name field. Native Git inferred email from
the canonical hostname when given `user.name`; Namespace rejected its
unqualified hostname. Bare commits failed on both. This does not establish
the cause of any customer's test failure.

Native Namespace Linux host jobs attempt canonical-name repair before workflow
processes start. Preserve raw hostname, machine ID, self addresses, aliases,
custom qualified names, and all Git identity settings. Only prepend
`<hostname>.buildkite.internal` to existing usable self mappings in `/etc/hosts`.
Use the system resolver, not Go's cgo-disabled DNS implementation. Permission,
read-only filesystem, missing mapping, and resolution failures warn without
failing the job. Do not extend this correction to job containers or other
platforms. [Isolated lifecycle tests](../internal/runtime/hostname_linux_test.go)
exercise real Git, system resolution, direct writes, noninteractive sudo, and
failure/no-op cases; they do not establish hosted rollout.

### Event file paths

Only linked webhooks and explicit snapshots authorize an event file. Preserve
the verified payload, per-job isolation, read-only container mounts, and lifetime
through action posts. Synthesized environment fallback events must not acquire
a file merely because an expression reads `github.event_path`; that value is
empty without an event file. Environment overrides cannot replace the runtime
path.

The runner [writes the payload and sets `github.event_path`](https://github.com/actions/runner/blob/4197182ef8f39c719f66038c1bc9b34cd9e9ac9a/src/Runner.Worker/ExecutionContext.cs#L1331-L1347).
Its [container expression context](https://github.com/actions/runner/blob/4197182ef8f39c719f66038c1bc9b34cd9e9ac9a/src/Runner.Worker/ExecutionContext.cs#L1583-L1623)
translates paths for container execution. Docker actions
[evaluate literal arguments](https://github.com/actions/runner/blob/4197182ef8f39c719f66038c1bc9b34cd9e9ac9a/src/Runner.Worker/Handlers/ContainerActionHandler.cs#L135-L150)
and [translate environment paths](https://github.com/actions/runner/blob/4197182ef8f39c719f66038c1bc9b34cd9e9ac9a/src/Runner.Worker/Handlers/ContainerActionHandler.cs#L253-L258).
Do not rewrite argument strings into container paths.

Local [compiled lifecycle tests](../internal/runtime/event_file_test.go) cover
job environment, shell expressions, action defaults, nested composites,
JavaScript pre/main/post hooks, outputs, payload integrity, and cleanup.
[Container tests](../internal/runtime/event_file_containers_test.go) exercise
host and job-container shell/composite reads and non-root Docker action reads.
This is runner-source and local execution evidence, not a native GitHub run or
released-availability claim.

### Prerequisite bootstrap failures

[PR #550](https://github.com/buildkite/buildkite-gha/pull/550) reports live agent
v3.129.0 captures with these mappings and artifact-search results:

| Producer | Step state / outcome | Result search across attempts |
| --- | --- | --- |
| Docker command-hook failure, exit 125; job exit -1 (`process_run_error`) | `finished` / `hard_failed` | Exit 0, empty stdout |
| Completed producer with a receipt | `finished` / `passed` | Exit 0, exact producer job UUID |

The report supplies no immutable run or capture links. It establishes neither
native GitHub behavior nor other agent versions or failure states. Keep recovery
limited to the accepted status-only boundary above. The
[API contract](development.md#prerequisite-status-api-contract) records the
commands needed to repeat the observation.

### Trigger activity defaults and ignored filters

The original reference reports native GitHub experiments on September 24 and 29,
2026. It supplies no immutable fixture or run URLs for these experiments. Retain
the observations as reported evidence; do not describe them as newly verified.

The activity-default experiment compared omitted `types`, literal YAML null, and
an empty array:

| Date in 2026 | Event | Observed selected activities |
| --- | --- | --- |
| September 29 | `issues` | `opened` |
| September 24 | `issue_comment` | `created` |
| September 24 | `label` | `created` |
| September 24 | `release` | `published`, `released`, `created` |
| September 24 | `pull_request` | `opened` |
| September 29 | `pull_request` | `synchronize`, `reopened` |
| September 24 | `pull_request_review` | `submitted` |
| September 24 | `pull_request_review_comment` | `created` |
| September 24 | `merge_group` | `checks_requested` |

The three forms selected the same observed activities. Explicit `[edited]` did
not select `issues.opened`. Explicit `[synchronize, reopened]` selected both
pull request activities while `[opened]` selected neither. Pull request `closed`
selected none of those five declarations during the experiment; an explicit
`[closed]` declaration was not tested. An earlier `edited` control selected only
explicit `[opened, edited]`, not omitted, null, or empty declarations.

This does not establish every documented default activity, quoted `'null'`,
`[null]`, mixed valid/unknown activities, malformed values, alternative null
spellings, or YAML aliases. Rejection by our parser is not native evidence.
Other activity defaults rely on
[GitHub's event documentation](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows)
and implementation tests, not this experiment.

The September 29 filter experiment isolated one filter per workflow:

| Filter | Exact value |
| --- | --- |
| `branches` | `["__lab_never_branch__"]` |
| `tags` | `["__lab_never_tag__"]` |
| `paths` | `["__lab_never_path__/**"]` |
| `branches-ignore`, `tags-ignore`, `paths-ignore` | `["**"]`, separately |

| Event | Activity or context | Explicit `types` |
| --- | --- | --- |
| `issues` | `opened` | `[opened]` |
| `issue_comment` | `created`, issue conversation | `[created]` |
| `label` | `created`, repository label | `[created]` |
| `release` | `published` | `[published]` |
| `create`, `delete` | Tag creation and deletion | Omitted |
| `deployment` | Payload action `created` | Omitted |
| `deployment_status` | Payload action `created`, state `success` | Omitted |
| `pull_request_review` | `submitted`, COMMENT review | `[submitted]` |
| `pull_request_review_comment` | `created`, inline diff comment | `[created]` |

The reference reports success for all 60 filtered workflows and ten unfiltered
controls on GitHub. Before the fix, Buildkite selected only the controls with
runtime v0.91.1. Native review runs used synthetic merge commits; Buildkite review
builds used pull request heads. The selection gap was not an intentional difference.

The experiment does not prove branch creation/deletion, other activities,
combined filters, malformed values, unknown keys, or post-fix hosted execution.
Separate `tags`/`tags-ignore` cases on `merge_group.checks_requested` had no native
or hosted runs despite successful controls. Their cause remains unresolved; a
quiet observation window is not proof of rejection or completed delivery.
The pull-request cases are superseded by the reproducible evidence below.

### Pull-request tag filter admission and selection

GitHub.com admits `pull_request.tags` and `tags-ignore`, but admission does not
mean that a pull request selects the workflow. Native experiments on November
10, 2026 used public, same-repository, non-draft pull requests, shell-only jobs,
positive controls, and mixed `push` declarations to separate admission from
selection.

| Declaration | Observed pull-request selection |
| --- | --- |
| Nonempty `tags` or `tags-ignore`, no nonempty branch filter | No run for `opened`, `synchronize`, or `reopened`. |
| Either tag filter plus matching `branches` | Runs, including never-matching tags and `tags-ignore: ['**']`. |
| Either tag filter plus nonmatching `branches` | No run. |
| Empty or YAML-null tag filter | Does not suppress selection. |
| Nonempty tag filter plus empty or null branch filter | No run. |
| Nonempty `branches-ignore` | Selects by the base branch, not the head branch. |

The [original tag-only fixture](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/blob/f4fe73aa19eacc8ff8fd73d37b8906a2b03503ed/.github/workflows/pull_request-tags-v.yml)
produced no PR runs. Its [mixed-push counterpart ran](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/actions/runs/42809773289).
A [matching-branch, ignore-all fixture](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/blob/b32319682b1b9ac9ca08102856d8070d4cffc4b8/.github/workflows/pull_request-ignore-all.yml)
[ran on a pull request](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/actions/runs/42809956625).
These positive receipts disprove blanket rejection and unconditional ignoring
of tag-only filters. Missing runs remain bounded observations, not rejection
diagnostics or evidence of GitHub's internal implementation.

Accept empty arrays separately from null: null means absent, while two non-null
include/ignore keys conflict even when both arrays are empty. Retain malformed
filter rejection. Preserve activity, base-branch, and path selection, and do not
let an inactive PR declaration invalidate an otherwise valid push workflow.

The [edge fixtures](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/tree/db31a13881ea2a8d18b0128cbde1255c5489fc5e/.github/workflows)
also distinguish quoted `'null'` from YAML null, admit scalar strings and
nonempty numeric/boolean tag values, and preserve empty/null activity defaults.
Unknown activity strings are admitted without selecting the tested PR
activities. Null list members are invalid, including
[`types: [null]`](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/blob/4ee737ba9f28cc25650f899dda0920c72d205ad1/.github/workflows/edge-pr-order-types-null.yml),
which produced an [explicit invalid-workflow diagnostic](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/actions/runs/42895800187)
even alongside push. Non-string tag admission does not prove canonical lexical
coercion because tag values do not participate in PR branch matching.

For PR branch includes, leading negatives are admitted, but a negative-only
array selects no branch. For branch ignores, negative patterns re-include in
order: `['**', '!<base>']` [ran](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/actions/runs/42896514377),
while the reverse order did not, with the same `edited` event and a
[successful control](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/actions/runs/42896514403).
Both [immutable order fixtures](https://github.com/sj26-testier/gha-e2e-onboarding-r-5a25e471655c/tree/673632a4b7cd223621d44a8ad97281604ce4dde4/.github/workflows)
were admitted on push. Apply these PR observations without changing the
existing push or merge-group pattern rules by inference.

[Blocking CLI tests](../internal/cli/cli_test.go) check admission and the original
compatibility-gap fixture's generated selection condition. [Trigger tests](../internal/buildkite/triggers_test.go)
check the selection matrix. Passing the soft-failed gap assessment alone proves
admission, not a positive PR match.

Native `pull_request_target` observations do not change its intentional exclusion.
These experiments do not establish GHES, fork, or merge-group behavior. Release
and activate the compatible CLI before the backend admits newly eligible PR
workflows; otherwise selected builds can reach the old CLI rejection.

### Merge-group destruction

The reference reports a September 24, 2026 single-pull-request queue experiment:
explicit `types: [destroyed]` ran successfully on GitHub with `reason: merged`.
The event, expressions, checkout, and workflow identity agreed on the group head
SHA. Omitted, null, and empty types did not select the event. GitHub documentation
then listed only `checks_requested`.

No fixture or run receipt was supplied. The observation does not establish other
destruction reasons, automatic cancellation behavior, GitHub object retention
after ref deletion, or hosted Buildkite execution. CLI acceptance without a
`reason` restriction is broader implementation behavior, not broader evidence.

### Variable scope and service startup

The [GitHub variable reference](https://docs.github.com/en/actions/reference/workflows-and-actions/variables#configuration-variable-precedence)
describes environment-over-repository-over-organization precedence, but also
says environment variables arrive after job start and do not overwrite values
in `env` and `vars`. Native server-side merging remains unconfirmed here.
Document our per-position behavior explicitly; do not call it established parity.
See [variable regression tests](../internal/compiler/vars_test.go).

Source inspected on September 30, 2026 shows the runner
[evaluating services after job environment](https://github.com/actions/runner/blob/2009b20729fdf49c50a88e0ca368906c16a3129c/src/Runner.Worker/JobExtension.cs#L235-L259)
using [server-supplied contexts](https://github.com/actions/runner/blob/2009b20729fdf49c50a88e0ca368906c16a3129c/src/Runner.Worker/ExecutionContext.cs#L1000-L1048).
That establishes ordering, not the server's variable merge or native startup
values. The [context-availability table](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#context-availability)
is documentation evidence for service-environment and matrix-credential admission.

Credential `inputs` appear in the pinned
[language-services schema](https://github.com/actions/languageservices/blob/4043eda158e16579cc5fb1b0b07a4bce2a76f0b5/workflow-parser/src/workflow-v1.0.json#L2500-L2532)
and [runner WorkflowParser schema](https://github.com/actions/runner/blob/5b3c03247427231abb553a8914910c3b4437e7b4/src/Sdk/WorkflowParser/workflow-v1.0.json#L2657-L2700).
The older DTPipelines schema omits them, while its
[evaluation context retains supplied inputs](https://github.com/actions/runner/blob/5b3c03247427231abb553a8914910c3b4437e7b4/src/Sdk/DTPipelines/Pipelines/ObjectTemplating/PipelineTemplateEvaluator.cs#L427-L496).
This does not establish which input classes GitHub supplies at service startup.
Our compile-known named-input subset, authored secret-name validation, and
simulated Docker checks are implementation evidence only; see
[compiler tests](../internal/compiler/compiler_test.go).

The reference reports a GitHub-hosted `ubuntu-latest` experiment on October 5,
2026 using services `s3.docker.test` and `s3-docker-test`, both on the host and in
a digest-pinned Redis job container. Bracket-indexed IDs, networks, and ports
remained distinct. The condition evaluated true, and asymmetric Redis `SET`/`GET`
checks reached different services. No run URL, fixture, or digest was supplied.
The report does not establish uppercase IDs, other punctuation, or maximum DNS
name lengths.

### Expression and composite source evidence

GitHub's [automatic token authentication](https://docs.github.com/en/actions/security-for-github-actions/security-guides/automatic-token-authentication)
allows an action to access `github.token` without the caller explicitly passing
it. The pinned [install-action metadata](https://github.com/taiki-e/install-action/blob/8cc2c8598944076af96ea45e3700725beb172ae5/action.yml)
uses a guarded composite shell environment read. The
[compile-and-run regression](../internal/runtime/runtime_test.go) verifies that
pattern with a real shell and a test token provider, including unchanged
`contents: read` scope, token redaction, disabled fallback, and empty-permission
rejection. It does not prove full tool installation or a customer's private
workflow. Planning boundaries belong to [Expression authority](expression-authority.md#composite-tokens).

The pinned runner's [composite condition schema](https://github.com/actions/runner/blob/7ce71e54a9affe1daae918a3e2350c1543dfa240/src/Runner.Worker/action_yaml.json#L237-L253)
and [child-context setup](https://github.com/actions/runner/blob/7ce71e54a9affe1daae918a3e2350c1543dfa240/src/Runner.Worker/Handlers/CompositeActionHandler.cs#L138-L161)
support `github` and `inputs` in composite conditions. The pinned
[Devbox action](https://github.com/jetify-com/devbox-install-action/blob/8c6a66ed6273138b1915457069de78cb52fe3bd7/action.yml#L131-L136)
uses a provider-URL guard. This informed
[composite `github.server_url` support](https://github.com/buildkite/buildkite-gha/pull/607).
It is source evidence, not a native execution or whole-action success claim.

A native GHES 3.22.2 [host/checkout run](https://buildkite-ghe.com/sj26-testier/ghes-source-compare-20261102/actions/runs/6)
at [fixture commit 8d5eaa8](https://buildkite-ghe.com/sj26-testier/ghes-source-compare-20261102/commit/8d5eaa81db6c6f5e5c16abc18fd17132fe3e5091)
accepted checkout `github-server-url: ${{ github.server_url }}`, true job/step
host conditions, and a false github.com branch. Checkout used the GHES mirror's
v4 commit `34e114876b0b11c390a56381ad16ebd13914f8d5`.
Job, workflow-step, reusable-call, and composite-step host conditions preserve
implicit success and token-authority rules; JavaScript lifecycle admission is
unchanged. Checkout expressions must resolve to the event host or empty value,
with the same host check at compilation and execution. These fields do not
authorize another host or change public action source resolution.
The [expression tests](../internal/expression/engine_test.go) and
[compiled runtime tests](../internal/runtime/runtime_test.go) cover both hosts,
false-branch token pruning, and unavailable values. The
[hosted differential oracle](../.github/workflows/expression-oracle.yml) also
checks checkout and host guards on github.com. Native GHES evidence does not
establish hosted Buildkite GHES checkout credentials or pipeline onboarding.

The runner's [composite result handling](https://github.com/actions/runner/blob/5f0b3ddcc6ee9fe9441e00d55e85b94ea4aad798/src/Runner.Worker/Handlers/CompositeActionHandler.cs#L235-L248)
and [job-status updates](https://github.com/actions/runner/blob/5f0b3ddcc6ee9fe9441e00d55e85b94ea4aad798/src/Runner.Worker/StepsRunner.cs#L273-L285)
explain why child `failure()` can be true while enclosing `job.status` remains
`success` until the composite returns. A step timeout does not itself set job
status to `cancelled`; whole-job cancellation does. Local compiled-workflow
[lifecycle tests](../internal/runtime/action_lifecycle_test.go) cover these
boundaries. This record contains no native execution receipt for them.

Strategy behavior also uses pinned runner
[matrix conversion](https://github.com/actions/runner/blob/15231bede4aacecb6686f4b7de25c62398607993/src/Sdk/WorkflowParser/Conversion/WorkflowTemplateConverter.cs#L1006-L1049)
and [singleton defaults](https://github.com/actions/runner/blob/15231bede4aacecb6686f4b7de25c62398607993/src/Sdk/WorkflowParser/WorkflowTemplateEvaluator.cs#L166-L195).
The [runner context schema](https://github.com/actions/runner/blob/15231bede4aacecb6686f4b7de25c62398607993/src/Sdk/DTPipelines/workflow-v1.0.json)
and [language-services schema](https://github.com/actions/languageservices/blob/4043eda158e16579cc5fb1b0b07a4bce2a76f0b5/workflow-parser/src/workflow-v1.0.json)
are not interchangeable with observed execution. Documentation and schema
disagreement about reusable caller `with` does not independently broaden our
supported caller-input positions.

The October 30, 2026 [native `ubuntu-latest` run](https://github.com/buildkite/buildkite-gha/actions/runs/41002711572)
of [this fixture](https://github.com/buildkite/buildkite-gha/blob/d4f07a6960dfeb36ae6ba57ed1d59433aea6243a/.github/workflows/expression-oracle.yml)
passed the text `${{ github.sha }}` as a reusable workflow input, directly and
through a second `workflow_call`. At both depths the callee received that text
unchanged, not the commit SHA, in:

- the job `if`, job `env`, and a `strategy.matrix` value
- step `run` and `env`, and a local composite action's `with`
- job and workflow outputs returned to the caller

`secrets[inputs.secret-name]` with the name `${{ 'GITHUB_TOKEN' }}` was empty,
while a control lookup of `GITHUB_TOKEN` was not. GitHub therefore treats
evaluated input values as data in these positions. The run does not cover
`runs-on`, `services`, `concurrency`, container credentials, or JavaScript and
Docker action inputs. A local
[regression test](../internal/runtime/reusable_input_data_test.go) compiles and
runs the same callee at both depths.

### Archive aliases

The [TruffleHog archive at the recorded commit](https://codeload.github.com/trufflesecurity/trufflehog/tar.gz/e28360875917c9c730f9c64aae29fb93aa6c359e)
contains `.claude/skills/dep-updates` and `.codex/skills/dep-updates`, both pointing
to `../../.cursor/skills/dep-updates`. The target directory and its `SKILL.md`
exist; the [action](https://github.com/trufflesecurity/trufflehog/blob/e28360875917c9c730f9c64aae29fb93aa6c359e/action.yml)
uses neither alias. This motivates tolerating unused aliases, not following them.

On Linux, the pinned GitHub Runner 2.337.0 source
[extracts with `tar -xzf`](https://github.com/actions/runner/blob/397b032cbf865e9c3ddfab89d533ec19325e1273/src/Runner.Worker/ActionManager.cs#L1330-L1375)
and [recursively copies directories](https://github.com/actions/runner/blob/397b032cbf865e9c3ddfab89d533ec19325e1273/src/Runner.Sdk/Util/IOUtil.cs#L394-L429),
following directory aliases into copied directories. Our omission intentionally
differs. This source comparison does not cover Windows ZIP extraction or prove
equivalent complete repository-tree semantics.

### Windows cache and Java

The October 20, 2026 [native Windows Server 2022 run](https://github.com/sj26-testier/gha-e2e-onboarding-r-7bc4559a378d/actions/runs/39385464624)
used [this immutable fixture](https://github.com/sj26-testier/gha-e2e-onboarding-r-7bc4559a378d/commit/c1e64e911eeeff025be76afc69af775e33a457fe),
pinned `actions/cache/save` and `actions/cache/restore` v4.3.0,
`enableCrossOsArchive: true`, GNU tar, and zstd 1.5.7. Two save/delete/restore
cycles used `cache.tzst`, `zstd -T0`, and `zstd -d`, then passed exact-key and
independently fixed SHA-256 assertions.

That establishes native Windows-only, same-worker round trips. The option's name
does not prove cross-OS behavior. It also does not prove cross-worker persistence
or Buildkite cache-service persistence. See the separate
[hosted cache proof procedure](development.md#verify-runtime-behavior).

On the same date, a synthetic `windows-2022` push
[fixture](https://github.com/sj26-testier/gha-e2e-onboarding-r-b3363f8bae11/blob/bb1108a5c4c42e47cb2426fabd8f4692a4aaf674/.github/workflows/windows-probe.yml)
successfully ran `& "$env:JAVA_HOME\bin\java.exe" -version` in its
[native control](https://github.com/sj26-testier/gha-e2e-onboarding-r-b3363f8bae11/actions/runs/39368874267).
This establishes that consumer, not complete JDK functionality or image parity.

The public minimum versions are supported by published notes:
[JAVA_HOME inheritance in v0.105.2](https://github.com/buildkite/buildkite-gha/releases/tag/v0.105.2)
and [Windows zstd discovery in v0.105.3](https://github.com/buildkite/buildkite-gha/releases/tag/v0.105.3).

### Concurrency cancellation

Job cancellation maps one expanded job to one Buildkite command step, with
`concurrency: {limit: 1, group: ..., cancel_in_progress: true}`. False or omitted
cancellation retains queue-only emission. Evaluate cancellation as a boolean
with the existing compile expression profile, per concrete matrix instance and
reusable-workflow input frame. Deferred matrices use the same evaluator after
expansion, including with a producer-derived group. This does not admit `needs`
in cancellation expressions or deferred reusable inputs.

Suppress job cancellation when the existing condition reduction proves the job
or an inherited call guard false. Keep evaluating cancellation expressions for
diagnostics even in that case. Conditions left for runtime cannot suppress a
request: Buildkite enforces concurrency before `run-job` evaluates them.

The merged [backend cancellation contract](https://github.com/buildkite/buildkite/pull/38010)
was inspected at [dde6eda](https://github.com/buildkite/buildkite/commit/dde6eda60a6912e424327e00849398d87eab6bfa).
The [queue processor](https://github.com/buildkite/buildkite/blob/dde6eda60a6912e424327e00849398d87eab6bfa/app/models/concurrency_group/queue_processor.rb)
enqueues cleanup after an opted-in job transitions from limiting to limited.
The [canceler](https://github.com/buildkite/buildkite/blob/dde6eda60a6912e424327e00849398d87eab6bfa/app/models/concurrency_group/canceler.rb)
selects the last opted-in limited request in effective scheduling order. It
cancels queued predecessors, then occupied blockers, until enough capacity is
expected to be released. Victims need not opt in. Admission remains asynchronous;
neither arrival order nor an atomic latest-wins outcome is guaranteed.

Workflow cancellation intentionally uses only the opening gate as requester.
The adjacent closing gate remains queue-only and waits for member completion.
An older dependency-waiting closing gate is a cancellation victim: the backend
[regression](https://github.com/buildkite/buildkite/blob/dde6eda60a6912e424327e00849398d87eab6bfa/spec/workers/pipelines/concurrency_group_cancel_worker_spec.rb)
explicitly covers waiting predecessors of ordered requests. Canceling that
marker lets a newer opener advance without waiting for the older workflow's
members. Those members are outside the workflow group and can continue, including
jobs not yet started after their opener passed. Workflow executions can overlap.
Canceling an older opener instead prevents its strictly dependent members from
starting. Do not opt closing gates in: a close that becomes limited could
otherwise request cancellation of other work while finishing its own scope.

Retain the existing shared-group and ordered-dependency cycle rejections.
Nested and matrix-expanded reusable gates keep their own cancellation decision.
A statically false call has no gate; a runtime-decided false call can still
request cancellation. Prerequisites must finish before an opener can request,
although its queue position is reserved at upload. Emit a best-effort warning
for enabled workflow cancellation rather than promising whole-build cancellation.

The [bundle regressions](../internal/compiler/bundle_test.go) check actual emitted
requesters and dependencies; [deferred scheduling tests](../internal/compiler/runtime_scheduling_test.go)
check per-row decisions without reparsing output text. These local tests and the
backend source inspection do not establish production cancellation or native
GitHub parity. Sam reported worldwide backend enablement on November 6, 2026;
live rollout was not independently verified. The public pipeline schema inspected
that day accepted structured concurrency but lacked `cancel_in_progress` and
rejected unknown keys; the server schema in the merged backend contains it.

## Backend contracts and public support

Backend enforcement, publicly supported configurations, and live rollout are
different facts. The source links below pin the inspected backend revision;
they do not attest deployment. Keep private backend links out of public-facing
instructions that users need to follow.

| Boundary | Source-backed distinction |
| --- | --- |
| Workflow-token eligibility | [Eligibility](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/job/code_access_token_issuer.rb#L171-L184) includes hosted agents and specific queue/development exceptions. The public hosted-agent support boundary must not be widened merely because an exception exists. |
| Pipeline opt-in | [Ordinary defaults](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/project/provider/github/project_settings.rb#L165-L173) disable workflow tokens; [GitHub Actions setup](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/pipeline/creator.rb#L64-L92) enables them. Neither proves every existing pipeline's stored value. |
| Repository permission authority | [Issuance](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/job/code_access_token_issuer.rb#L644-L673) checks the named workflow at the immutable build commit against top-level permissions. A broader backend `read-all` ceiling is not the same as the CLI's requested scope map. |
| Pull requests and merge queues | [Ancestry checks](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/job/code_access_token_issuer.rb#L562-L608) traverse trigger and rebuild ancestry, impose the pull-request ceiling, and deny merge-queue tokens. They do not prohibit tokenless workflows. |
| Merge-group destruction | The [event processor](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/pipeline/trigger/github_actions_webhook_processor.rb#L765-L803) accepts destruction handlers without the previously documented organization gate. The [cancellation worker](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/workers/pipelines/merge_group_running_builds_canceller_worker.rb#L15-L33) protects handlers using server-owned metadata, independently of retained receipts or mutable environment fields. |
| Release delivery | [Pipeline triggers](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/pipeline/trigger/github_actions_webhook_processor.rb#L725-L762) and [classic release handling](https://github.com/buildkite/buildkite/blob/fadddfd91df09f0436a7c18eca992933b3c89a86/app/models/project/provider/github/webhook_processor/release_event.rb#L3-L28) have different activity coverage and prerequisites. Code capability does not prove an existing webhook subscribes to those events. |

Runner entitlement and queue mapping, environment/variable endpoint access and
rate limits, runtime selection, and live activation still require their owning
backend or operational evidence. Fake Agent API tests prove client response
handling, not those server policies. Do not flatten their different fallback
rules: explicit runner validation and environment resolution fail closed, while
automatic runner resolution and unavailable variables have their own contracts.

Runner `missing_queue` rejections may include a server-owned `setup_url` to an
authenticated review page with one recommended queue preselected. Preserve it
through diagnostics, JSON, logs, and annotations; offer the action only for a
validated absolute HTTP(S) URL, never from labels or error prose. Other rejection
codes and older servers without the field retain their existing guidance.
Requests advertise `supports_queue_setup: true`; the backend emits `setup_url`
only with that capability. Missing or false preserves older replies within
older clients' 64 KiB response limit; opted-in clients accept up to 1 MiB.
Deploy the backend page and response field before releasing the CLI. Queue
creation does not resume synthetic failed jobs: keep manual retry disabled and
direct users to trigger a matching workflow event, or rebuild where supported.
This does not enable native manual runs or rebuilds for GHES pipelines.
The [runner diagnostic tests](../internal/cli/runner_resolution_test.go)
and [upload test](../internal/cli/upload_test.go) establish client rendering and
failure-step behavior, not backend authorization or live queue creation.

Automatic Ubuntu ARM64 routing is server-owned. For `ubuntu-22.04-arm`,
`ubuntu-24.04-arm`, `ubuntu-26.04-arm`, their `-arm64` aliases, and recognized
provider ARM64 labels, select an active hosted Linux ARM64
`linux-arm64-medium` queue in the job's cluster, without an amd64 image
or tool-cache bootstrap. Explicit mappings retain precedence and server
validation, and infer ARM64 for those same labels. A missing ARM64 queue must
not fall back to amd64. Match complete provider labels and documented sizes,
not arbitrary prefixes or custom suffixes; retain provider fallback warnings.
The November 5, 2026 label audit used the
[GitHub image inventory](https://github.com/actions/runner-images#available-images)
and the [Depot](https://depot.dev/docs/github-actions/runner-types),
[Blacksmith](https://docs.blacksmith.sh/blacksmith-runners/overview),
[WarpBuild](https://www.warpbuild.com/docs/ci/cloud-runners), and
[Ubicloud](https://www.ubicloud.com/docs/github-actions-integration/runner-types)
catalogs. WarpBuild's `-xfast` variants still select Linux, not macOS.
The [CLI upload regression](../internal/cli/runtime_distribution_test.go) checks
queue selection and ARM64 executable binding using a simulated Agent API;
it does not establish backend deployment or hosted ARM64 execution.

## Maintaining the contract

Change the owning implementation and regression tests together. Update this
record when evidence changes an invariant or an intentional difference. Link to
the relevant public section rather than copying its syntax table here.

Before updating public support, establish the first released runtime and any
backend prerequisites. Retain narrower support or an explicit uncertainty when
the evidence is incomplete. Release notes can establish publication, not live
backend deployment or native equivalence.

Do not keep progress logs or a second backlog in this file. Keep evidence useful
for the next change: a pinned input, a result, the obligation it supports, and
what it does not prove.
