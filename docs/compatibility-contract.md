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

Path filtering requires verified repository identity and bounded Git evidence.
Non-path exclusions can retain their conditions when diff history is unavailable,
but identity failures remain errors. Do not turn missing history into an empty
diff. Evaluate renames as deletion of the old path and addition of the new path;
a copy that leaves its source unchanged contributes only the destination path.
Keep local diffs independent of Git rename-detection settings. The
[path-filter implementation](../internal/cli/path_filters.go) and
[regression tests](../internal/cli/path_filters_test.go) own those checks.

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

Public action sources remain on github.com. Never attach GHES workflow tokens
to public action requests or infer a github.com self-repository identity from
a GHES event. Private reusable source resolution requires host-bound identity;
reject it for GHES imports until that contract exists.

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

Workflow-token requests use the requesting workflow's top-level repository
permission policy. Job and called-workflow maps do not narrow that token;
`id-token` narrowing is a different mechanism. Retry the same authorized request
only under the bounded transient-failure policy in
[token acquisition](../internal/runtime/github_token_service.go). Do not convert
authorization denials into retryable failures or replace specific server
diagnostics with generic App-permission advice.

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
Four separate `tags`/`tags-ignore` cases on `pull_request.opened` and
`merge_group.checks_requested` had no native or hosted runs despite successful
controls. Their cause remains unresolved; a quiet observation window is not
proof of rejection or completed delivery.

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

The pinned runner's [composite condition schema](https://github.com/actions/runner/blob/7ce71e54a9affe1daae918a3e2350c1543dfa240/src/Runner.Worker/action_yaml.json#L237-L253)
and [child-context setup](https://github.com/actions/runner/blob/7ce71e54a9affe1daae918a3e2350c1543dfa240/src/Runner.Worker/Handlers/CompositeActionHandler.cs#L138-L161)
support `github` and `inputs` in composite conditions. The pinned
[Devbox action](https://github.com/jetify-com/devbox-install-action/blob/8c6a66ed6273138b1915457069de78cb52fe3bd7/action.yml#L131-L136)
uses a provider-URL guard. This informed
[composite `github.server_url` support](https://github.com/buildkite/buildkite-gha/pull/607).
It is source evidence, not a native execution or whole-action success claim.
Keep admission scoped to composite conditions and preserve implicit success and
token-authority rules; see [expression tests](../internal/expression/engine_test.go).

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
