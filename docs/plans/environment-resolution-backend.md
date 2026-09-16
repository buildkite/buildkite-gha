# Buildkite-managed GitHub environment resolution

Importer jobs resolve
[deployment environments](../compatibility.md#deployment-environments) through
Buildkite, which already holds the GitHub App installation. The chosen design
is option 2 below: a dedicated Agent API snapshot endpoint,
`POST /jobs/{job_id}/github-actions/environments`, implemented on the
Buildkite backend. The backend can disable resolution per organization with
`GitHubEnvironmentResolutionOptOut`, which returns 404. The endpoint returns no credential or secret value,
missing GitHub App permissions already fail closed as 400, and an unavailable
endpoint fails closed as 404. The
importer posts one batched request per upload naming every distinct
environment with the pipeline repository URL; the backend solely owns the
batch-size bound and the per-job and per-App-installation budgets, rejecting
oversized batches with a
stable 400 message that the client surfaces unchanged. The
backend performs the GitHub reads with its own credentials and
answers with a non-secret JSON snapshot — required reviewers present,
`prevent_self_review`, wait timer minutes, branch policy present, unsupported
rule descriptions, secret names, and, when the request sets
`include_variables`, the environment's variables with plaintext values.
No GitHub token and no secret value reaches the importer. This client consumes the endpoint automatically in
`upload` and in `compile` when it runs inside a Buildkite job. It is the only
environment access path: there is no GitHub token option, so environments are
unsupported outside a Buildkite job and on GitHub Enterprise Server.

A token-minting endpoint (option 1) was implemented first and removed before
merge: endpoint identity alone is not a caller security boundary, so handing
importer jobs a mintable token carrying Actions: read (workflow runs, logs,
artifacts) was broader than environment resolution needs. The snapshot
endpoint keeps the token inside Buildkite and narrows the importer-visible
output to exactly the fields the compiler consumes.

Remaining before removing this plan:

- Verify rollout of the backend endpoint and its `include_variables`
  extension, including Actions: read and Environments: read permissions on the
  code-access GitHub App and installation administrator approvals. The
  endpoint and variable extension are
  implemented in the [backend controller](https://github.com/buildkite/buildkite/blob/a2293dc339c992159e0e635d033529ee76b6ccbb/app/controllers/agent/github_actions/environments_controller.rb#L19-L76);
  source inspection does not establish production availability. Environment
  variable listing is covered by Environments: read. The environments
  endpoint carries environment-scoped variables only and will not grow
  repository or organization fields.
- Repository and organization variables come from a separate job-scoped
  endpoint, `POST /jobs/{job_id}/github-actions/variables`, which replaced a
  withdrawn draft that extended the environments response; this client never
  consumed those draft fields. The client side is done (see below).
  Remaining: merge and roll out the backend endpoint, including its
  Variables: read token scope and the 10-requests-per-job-per-hour budget.
  Until then the endpoint returns 404 and the client leaves both scopes
  empty, so `vars` names no scope defines keep evaluating as empty strings.
- A hosted end-to-end proof of an `upload` resolving an environment and gating
  a deploy job.

## Dynamic environment names require a protection decision

An environment name derived from `needs.<job>.outputs.<name>` must resolve
before the deployment job is emitted. The continuation must resolve policy,
environment variables, and secret-name mappings before building that job's
plan and approval dependencies. `environment.url` is runtime deployment
metadata, not a scheduling input; it remains accepted with no effect, as
documented in [deployment environments](../compatibility.md#deployment-environments).

The snapshot endpoint is not an importer-only capability. Its controller
authorizes a same-job Agent token and accepts `repo_url`, `environment_names`,
and `include_variables`. A continuation can use its own job identity to read
snapshots without receiving a GitHub token. The endpoint's existence is
therefore not a backend blocker to late name resolution.

It does not, however, authorize a deployment. The
[backend protection decoder](https://github.com/buildkite/buildkite/blob/a2293dc339c992159e0e635d033529ee76b6ccbb/app/models/scm/provider/github_code_access_app/environment_resolver.rb#L67-L106)
reduces reviewer rules to `required_reviewers` and `prevent_self_review`, and
branch rules to `branch_policy`. Reviewer identities and allowed branches are
absent. Two configurations requiring different reviewers can produce the
same snapshot. Neither the request nor response carries a deployment-bound
approval decision, approver identity, or protection-completion state.

This prevents a CLI-only implementation from enforcing GitHub-equivalent
approval before emission. The existing
[compiler](../../internal/compiler/environment.go) rejects wait timers,
branch policies, and custom rules, but accepts required reviewers through a
Buildkite block that does not enforce GitHub reviewer identity or
`prevent_self_review`. Reusing that block for a late-selected environment
would retain the documented approximation, not supply the missing protection.

Supporting protected deployments with GitHub-equivalent approval requires an
agreed backend contract and rollout first: it must bind
the decision to the repository, ref/commit, resolved environment, and deployment
attempt, and define when a job may proceed and how retries or policy changes
invalidate approval. Missing or unsupported protection must prevent emission.
This plan does not select an approval mechanism or add GitHub deployment
creation.

The implemented [dynamic-name subset](../compatibility.md#environment-names-from-job-outputs)
rejects every protected environment before emitting the deployment job or its
dependents. It uses the matrix continuation's verified producer result,
source locks, recorded importer authority, and replay checks. Only the
scheduling name is supplied to recompilation; it does not turn runtime
`needs` or environment variables into constants for token-authority planning.
Literal names across the upload are recorded to reject secret-prefix
collisions. Only one dynamic environment is permitted per upload, avoiding
collisions between independently selected names. Environment scheduling uses
the shared staged `upload` path, but does not extend dynamic environments to
matrix joins or chains. Independent matrix stages keep their existing join
and chaining support. General cancellation, concurrency, and dynamic runners
remain separate work.

## Why the importer cannot read GitHub environments directly

The importer's action-source credentials do not cover environment reads.
Reading environment configuration and protection rules needs the repository
permission Actions: read; environment secret names need Environments: read.

- `github_action_source_access_token`
  ([internal/cli/hosted.go](../../internal/cli/hosted.go),
  [internal/runtime/github_token_service.go](../../internal/runtime/github_token_service.go))
  mints exactly `metadata: read` on the pipeline repository. The backend fixes
  that permission map and rejects caller-selected permissions, and its feature
  flag promises metadata-only tokens. The client also treats mint failure as
  fall-back-to-anonymous, the opposite of the fail-closed contract environment
  resolution requires.
- `github_workflow_access_token` is bound to an admitted compiled job and its
  plan-declared `permissions`; the importer is not a compiled job and holds no
  plan. Its backend permission allowlist has no `environments` entry.

Broadening the action-source token is not a safe shortcut: it would silently
upgrade every importer job from metadata-only to Actions: read and
Environments: read on the pipeline repository, whether or not a workflow
declares an environment, and would break the backend's documented
metadata-only contract for that endpoint.

The backend cannot exceed app-granted permissions. Environment resolution
requires `actions: read` and `environments: read` on the code-access GitHub App
and approval from each installation's administrators; installations that have
not approved must keep failing closed at compile time.

## Options considered

One new job-scoped Agent API capability, either:

1. **Dedicated token endpoint** (smallest backend change), alongside
   `github_action_source_access_token` and sharing its issuer, same-job token
   boundary, rate limiting, and permission-digest token cache: mints a
   short-lived token with fixed permissions Actions: read and Environments:
   read, attenuated to the pipeline's repository, behind its own feature flag.
   Rejected — see above.
2. **Environment snapshot endpoint** (chosen), modeled on runner resolution
   (`POST /jobs/{job_id}/github-actions/runners`,
   [internal/runtime/runner_resolution.go](../../internal/runtime/runner_resolution.go)):
   the backend owns the GitHub reads, freshness, and policy, and the importer
   receives only the non-secret snapshot the compiler consumes.

Like the action-source token, the endpoint is GitHub.com-only and restricted
to the pipeline's configured repository; GHES pipelines cannot declare
environments. Backend failures map to actionable client
errors: 404 when the endpoint is unavailable, 400 for invalid or ineligible
requests, 429 with `Retry-After` past a per-job or per-App-installation
resolution budget, and 503
for GitHub unavailability.

## Client work (done)

`upload` and `compile` build a `compiler.EnvironmentSource`
([internal/cli/environments.go](../../internal/cli/environments.go)) over the
snapshot endpoint
([internal/runtime/environment_resolution.go](../../internal/runtime/environment_resolution.go))
from the job's Agent connection. Each workflow's declared environments
resolve together in one batched request — results, including failures, are
memoized case-insensitively, so an upload of many workflows sharing
environments typically consumes one request — and every resolution failure
fails the compile, never degrading to an unprotected deployment. The client
always requests variables and requires the `variables` field, so a backend
without the extension fails the compile with a decode error rather than
letting `vars` references resolve as empty.

Repository and organization variables use a separate source
([internal/cli/variables.go](../../internal/cli/variables.go)) over the
variables endpoint
([internal/runtime/variable_resolution.go](../../internal/runtime/variable_resolution.go)).
Static analysis (`compiler.Report.ReferencesVars`) finds `vars` references
across each workflow and the reusable workflows it calls; when any applicable
GitHub.com workflow references `vars`, one memoized request per upload sends
`{"repo_url": "https://github.com/owner/repo"}` and fills
`compiler.VariableSources` before validation, so `jobs.<id>.if` and
compile-time fields resolve and every job plan carries `repository_vars` and
`organization_vars`. A reference that lives only in a resolved action's input
default (`compiler.ActionsReferenceVars`) is discovered after compilation;
the same memoized request then runs and the workflow compiles again so its
plans carry the scopes. The client enforces the contract's bounds (500 and 1000
names, 48 KiB per value, 256 KiB combined, name syntax, case-insensitive
uniqueness per scope), treats 404 as empty scopes, and fails the referencing
workflows on 400, 429, and 503 with the backend message and `Retry-After`
delay, never echoing a value. Token-authority planning ignores resolved
values. When the backend rollout completes and the hosted proof passes, move
lasting facts into
[deployment environments](../compatibility.md#deployment-environments) and
remove this plan.
