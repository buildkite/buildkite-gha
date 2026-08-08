# ADR 0004: Reuse the scoped job token for hosted public Action source

- Status: Proposed
- Date: 2026-08-08
- Decision owners: `buildkite-gha` and Buildkite platform maintainers

## Context

Hosted `buildkite-gha upload` resolves public GitHub Action repositories to
immutable commits and downloads their source before generating jobs. It
currently does so anonymously. This preserves a public-only source boundary but
shares GitHub's low unauthenticated rate-limit bucket.

Users should not configure a PAT, token flag, repository URL, or permission map
for hosted compilation. Authentication is an importer implementation detail and
must not create a workflow capability or put a provider credential in generated
jobs.

Buildkite already exposes a job-bound Agent operation used for workflow
`GITHUB_TOKEN` compatibility:

```http
POST /v3/jobs/<current-job-id>/github_scoped_access_token
Authorization: Token <current-job Agent access token>
Content-Type: application/json
```

The backend independently requires the requested repository to equal the
pipeline's configured GitHub repository and validates the requested permissions.
For the current public-only importer, reusing this operation with an
importer-owned fixed request is smaller than introducing another token issuer.

## Observable GitHub Actions architecture

GitHub Actions uses several distinct job credentials rather than one general
action-source token:

1. At each job start, GitHub creates a unique GitHub App installation
   `GITHUB_TOKEN`, scoped to the workflow repository and the job's effective
   permissions. It expires with the job and is separate from the runner's
   service credential.
2. The open-source runner does not resolve `owner/repository@ref` through the
   public GitHub API. `ActionManager.GetDownloadInfoAsync` sends unresolved
   action references to a plan/job-scoped Actions/Launch resolver, authenticated
   with the job's `SystemVssConnection` service token.
3. The resolver returns the resolved name and SHA, tarball and zipball URLs, and
   optional download authentication containing a token and expiry. Resolution,
   visibility policy, URL selection, and token minting are server concerns not
   present in `actions/runner`.
4. `ActionManager` registers any resolver-supplied token with its secret masker
   and prefers that token. If the resolver omits download authentication, it
   explicitly falls back to `github.token`, the job's workflow-repository
   `GITHUB_TOKEN`, for the archive GET.
5. GitHub's supported private-action sharing path uses another one-hour,
   read-only token scoped to the private source repository.

The relevant observable runner contracts are
[`ActionManager.GetDownloadInfoAsync`](https://github.com/actions/runner/blob/main/src/Runner.Worker/ActionManager.cs),
[`LaunchHttpClient`](https://github.com/actions/runner/blob/main/src/Sdk/WebApi/WebApi/LaunchHttpClient.cs),
and
[`LaunchContracts`](https://github.com/actions/runner/blob/main/src/Sdk/WebApi/WebApi/LaunchContracts.cs).

This ADR adopts the runner's **workflow-token fallback** shape for the current
public-only scope. It does not claim parity with GitHub's authoritative
server-side action resolver or private-action download-token model.

## Decision

### Use an importer-owned fixed request to the existing endpoint

Hosted `upload` requests one token through the existing endpoint only when
preflight finds remote GitHub Actions:

```json
{
  "repo_url": "https://github.com/<authoritative pipeline repository>",
  "permissions": {
    "contents": "read"
  }
}
```

The importer, not workflow content, constructs this exact request. It obtains
the repository from the authoritative pipeline/event binding already required
by hosted compilation and requires those repository identities to agree. A
workflow cannot select the repository, permissions, installation, or token
purpose. The permission map is always exactly `{"contents":"read"}`.

The backend remains authoritative: it authenticates the current job's Agent
token, binds the path job ID to that credential, independently verifies the
requested repository against the pipeline's configured GitHub repository, and
limits the token's selected installation repositories to that exact repository.
The importer requests only the configurable `contents: read` permission;
GitHub's automatic `metadata: read` remains effective.

This token may be able to read the pipeline repository when that repository is
private. It is therefore not intrinsically public-only. The current design is
safe only because the importer keeps the credential inside the compiler and
separately refuses every action repository whose authenticated metadata reports
`private: true`. The decision gates below must be satisfied before enabling the
path.

### Enforce public action targets and contain the token

The compiler treats the returned token as sensitive inert bytes:

1. Validate the bounded response and token shape without including response
   bodies or decoder-controlled text in errors.
2. Register the exact token with the Buildkite Agent redactor. If registration
   fails, discard it and use anonymous resolution.
3. Keep it only in compiler memory. Never serialize it into compiler IR, plans,
   plan digest input, pipeline YAML, generated jobs, workflow expression
   contexts, action metadata or inputs, subprocess environments, command
   arguments, URLs, cache manifests, errors, metrics, traces, or logs.
4. Before resolving refs, annotated tags, commits, or tarballs for each distinct
   action repository, request repository metadata and require an explicit
   `private: false`. Missing, malformed, denied, or private metadata fails that
   action as not public. Do not continue to source endpoints on a private result.
5. Attach `Authorization` only to HTTPS requests whose exact host is
   `api.github.com`. Never send it to caller-selected hosts. Strip
   `Authorization` and cookies before following the expected archive redirect
   to `codeload.github.com`, and retain the resolver's existing redirect-host
   allowlist.
6. Drop the token and authenticated transport when compilation ends. Runtime
   jobs and action subprocesses never receive them.

This is analogous to `actions/runner` using `github.token` when its resolver did
not supply action-specific download authentication. Unlike the runner, the
current importer still resolves public refs and validates visibility itself; it
does not delegate policy or resolution to a Buildkite service.

### Keep local and hosted credential sources separate

Local development and corpus evaluation may use
`BUILDKITE_GHA_GITHUB_TOKEN` only when an operator explicitly sets it and
otherwise resolve anonymously. They apply the same `private: false`, host,
redirect, bounded-error, and non-leakage rules. They must not infer or read
ambient `GH_TOKEN`, `GITHUB_TOKEN`, a Git credential helper, or another provider
credential.

Hosted `upload` ignores `BUILDKITE_GHA_GITHUB_TOKEN` and prefers the current
job's Agent endpoint. On bounded endpoint construction, minting, response
validation, or redaction failures, hosted compilation emits one sanitized
warning and falls back to the anonymous public resolver. Cancellation and
deadlines still stop compilation. It never falls back to a workflow-provided
token, PAT, ambient credential, or repository credential helper.

For `429` or `503`, honor only an integer `Retry-After`, cap the wait at five
seconds and token requests at two, then resolve anonymously. For authenticated
GitHub `401`, discard the token and permit at most one fresh mint and request
retry. GitHub primary or secondary rate-limit responses use the existing bounded
`RateLimitError`; do not retry them in a loop.

## Decision gates before implementation

Buildkite backend owners must confirm with contract tests that the existing
operation:

- supports the importer as a caller authenticated by the current job's Agent
  token;
- independently binds the request to that exact job and authoritative pipeline
  repository;
- limits the token's selected installation repositories to exactly the
  authoritative pipeline repository, not every selected repository in the
  installation;
- requests exactly `contents: read` and grants no write or other configurable
  repository, organization, or account permission; GitHub's automatic
  `metadata: read` is the only expected additional effective repository
  permission;
- expires in no more than one hour; and
- uses the customer's relevant GitHub App installation and authenticated rate
  limit bucket rather than a shared Buildkite-wide source credential.

Before rollout, a hosted live test must use the exact endpoint response and HTTP
transport intended for production to prove:

1. Repository metadata, lightweight and annotated refs/tags, commits, and the
   tarball for an immutable commit work for a public action repository outside
   the customer's installation repository selection.
2. A controlled private action repository reports private or denied and the
   importer performs no ref, commit, or tarball request for it. Direct token
   access to a private repository other than the exact authoritative pipeline
   repository must fail. Access to an exact private pipeline repository may
   succeed and must be treated as expected endpoint scope, not public-action
   authority.
3. Representative write operations against both the authoritative repository
   and a public repository fail.
4. The archive request follows only the expected GitHub redirect and neither
   `Authorization` nor cookies reach `codeload.github.com`.
5. The token appears in no compiler output, generated artifact, subprocess
   environment, captured log, error, metric, or trace, including malformed and
   rate-limited responses.

Failure of any gate keeps hosted authenticated resolution disabled; the current
anonymous behavior remains available.

## Future direction: a job-bound action resolver

If Buildkite adds private Actions, centralized source policy, or server-side ref
resolution, follow GitHub Actions' richer model rather than extending this
fallback token into a general source credential. A Buildkite service would
accept unresolved action identities over a current-job authenticated channel and
return immutable commits, approved archive URLs, visibility/policy decisions,
and optional per-action download authentication. Private source credentials
would be short-lived, read-only, and scoped to the exact source repository.

That service would own source authorization and auditing. The compiler would
prefer resolver-supplied per-action authentication and would not infer broader
authority from the workflow-repository token. This future protocol is separate
from the current public-only decision.

## Rejected and deferred alternatives

### Dedicated zero-permission public-source App and endpoint

Do not currently add
`POST /v3/jobs/<current-job-id>/github_public_source_token` or a dedicated
zero-permission GitHub App installation. That design can provide stronger token
isolation, but adds another backend route, App, installation, cache, rotation,
monitoring, and live-conformance surface before the public-only importer needs
it. The existing job-bound scoped-token endpoint plus strict compiler
containment is the smaller current change and matches the runner's observable
`GITHUB_TOKEN` fallback more closely.

Reconsider the dedicated endpoint/App if any of these becomes true:

- the existing endpoint token cannot be narrowed to the exact authoritative
  repository;
- live conformance fails for the public metadata, ref, commit, or tarball paths;
- policy requires the credential itself to be incapable of reading any private
  source, rather than relying on compiler containment and `private: false`;
- authenticated imports from providers other than GitHub are required; or
- the importer no longer has current-job Agent authority or an authoritative
  pipeline repository binding.

### Workflow or ambient credentials

Do not accept workflow-controlled repository or permission input, add PAT flags,
or consult ambient `GH_TOKEN`/`GITHUB_TOKEN` in hosted compilation. Do not reuse
the runtime workflow-token plan fields or expose the compiler token as
`github.token`, `secrets.GITHUB_TOKEN`, or an action input. The explicit
`BUILDKITE_GHA_GITHUB_TOKEN` path remains non-hosted only.

## Repository and backend work

This ADR is design-only and does not enable authenticated hosted resolution.
The implementation belongs in a backend-coordinated follow-up after every
decision gate passes. This repository will own the fixed-request Agent client,
redaction-before-use sequence, authenticated metadata/publicness gate,
host-scoped source transport, anonymous fallback, and non-leakage tests.

The Buildkite backend may require no new route. Its owners must confirm or add
importer caller support and the exact repository/permission/lifetime narrowing
above, then add regression tests for same-job and cross-job authorization,
repository mismatch, permission rejection, importer availability, token expiry,
customer installation selection, and sanitized upstream failures. Backend logs,
traces, metrics, and errors must never contain the Agent token, installation
token, or upstream response body.

The existing local `BUILDKITE_GHA_GITHUB_TOKEN` source-transport work may supply
bounded parsing, publicness checks, host-scoped authorization, and redirect
credential stripping. Hosted wiring must not read that environment variable.

## Consequences

- Hosted users retain zero configuration and gain a customer/job-bound
  authenticated rate-limit bucket when the endpoint is available.
- The current change reuses an existing backend operation and avoids a new App
  and issuer.
- The token is not structurally public-only when the authoritative pipeline
  repository is private. Safety depends on compiler-memory containment,
  redaction, strict host scoping, and an explicit `private: false` gate before
  all action source requests.
- This does not provide private Actions or parity with GitHub's action resolver.
- Anonymous behavior remains the bounded availability fallback.
