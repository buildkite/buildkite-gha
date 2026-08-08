# ADR 0004: Authenticate hosted public Action source resolution with a dedicated GitHub App

- Status: Proposed
- Date: 2026-08-08
- Decision owners: `buildkite-gha` and Buildkite platform maintainers

## Context

Hosted `buildkite-gha upload` currently resolves public GitHub Action refs,
commits, and tarballs anonymously. This is correctly public-only, but shares
GitHub's low unauthenticated rate-limit bucket. The existing
`github_scoped_access_token` operation is not a safe substitute: it is bound to
the pipeline repository and accepts workflow permissions, so a private pipeline
can receive a token that reads its private repository.

Users should not configure a PAT, token flag, repository URL, or permissions.
Authentication is a compiler implementation detail and must not create a new
workflow capability.

## Decision

### Dedicated job-bound operation

The Buildkite Agent API adds this separate operation:

```http
POST /v3/jobs/<current-job-id>/github_public_source_token
Authorization: Token <current-job Agent access token>
Accept: application/json
```

The request has no body. In particular, it has no repository, installation,
permission, organization, pipeline, or workflow field. A non-empty body is
rejected with `400`; unknown query parameters are rejected. The server derives
the current job from the path and Agent token and uses only server-owned GitHub
App configuration.

A successful response is deliberately minimal:

```http
HTTP/1.1 200 OK
Content-Type: application/json
Cache-Control: no-store
```

```json
{"token":"<GitHub installation access token>"}
```

The token is opaque to the client. GitHub installation tokens currently expire
after one hour; the backend must request no longer lifetime if GitHub later
makes lifetime configurable. The operation does not reuse, alias, or widen
`github_scoped_access_token`.

Expected failures are:

- `400` malformed request (including any body or query);
- `401`/`403` Agent authentication or current-job authorization failure;
- `404` operation unavailable for this Buildkite installation;
- `429` mint capacity or upstream rate limit, with a numeric `Retry-After` when
  known;
- `503` temporary GitHub App or installation failure, also optionally carrying
  `Retry-After`; and
- other `5xx` unexpected backend failure.

Error bodies must never contain a GitHub token or upstream response body. The
client ignores response bodies for non-2xx statuses and validates a bounded,
single-field success response.

### GitHub App is structurally public-only

Use a dedicated GitHub App and installation, not the App/installation used for
pipeline checkout or workflow tokens:

1. Configure **no repository permissions**, no organization/account
   permissions, and no webhook subscriptions. GitHub Apps have no permissions
   by default; public REST resources remain readable without private repository
   permission. Do not request `Contents: read`: that permission is required for
   private Git refs, commits, and tarballs and would make selected private
   repositories readable.
2. Disable OAuth authorization during installation and device flow. Do not
   generate or deploy OAuth client secrets or callback credentials. Give the
   minting service only the App private key and fixed installation ID. The
   service uses only installation access tokens with the REST API. Withholding
   `Contents` is what prevents authenticated private Git/source access; there is
   no separate Git-access switch on which this design relies.
3. Install the App on a dedicated Buildkite-controlled account or organization
   with **Only select repositories**, selecting only a purpose-built public
   canary if GitHub requires a selection. Never select a private repository and
   never use **All repositories**. Alert on installation repository-selection
   or App-permission changes.
4. Mint from that one configured installation ID without accepting a caller
   installation ID, repository list, or permission map. The mint request must
   not ask GitHub to widen the App's configured permissions.

Zero App permissions, rather than a server-side allowlist alone, is the primary
private-source boundary. The installation placement and change monitoring
protect against later configuration drift.

Before deployment, run a live conformance test with the exact installation
token and API version used in production:

1. For two public repositories, one selected canary and one repository outside
   the installation owner, resolve a lightweight tag and branch through
   `GET /repos/{owner}/{repo}/git/ref/{ref}`; resolve the returned object through
   the annotated-tag path when applicable; validate an exact commit through
   `GET /repos/{owner}/{repo}/commits/{sha}`; and download
   `GET /repos/{owner}/{repo}/tarball/{sha}`, following only GitHub's expected
   redirect to `codeload.github.com` after stripping `Authorization`.
2. Keep the production installation limited to the public canary and verify its
   installation repository listing contains no private repository. Requests to
   a controlled unselected private repository must return GitHub's
   not-found/denied response.
3. Create a separate conformance installation of the same zero-permission App
   and select one sacrificial private repository. With that installation token,
   repeat the ref, annotated-tag, commit, tarball, and authenticated Git-read
   requests. Every source request must fail without returning Git objects or
   archive bytes. GitHub's installation repository API may reveal the selected
   repository's identity and metadata; the invariant is that the token cannot
   read private repository source contents, not that it cannot enumerate a
   deliberately selected repository.
4. Repeat the private source checks against another controlled private
   repository outside the conformance installation. Record the App permission
   and installation repository-selection snapshots with the test result, never
   the token.
5. Make the private negative test a deployment gate and scheduled canary. Revoke
   the installation and disable issuance immediately if any private source read
   succeeds.

This live proof is required because GitHub's endpoint documentation lists
`Contents: read` for private resources while allowing some endpoints on public
resources without it; local mocks cannot prove the deployed App configuration.

### Compiler lifecycle and containment

Only hosted `upload`, and only when preflight finds remote GitHub Actions,
requests one token. The compiler registers the exact returned literal with the
Buildkite Agent redactor before installing it in an in-memory HTTP transport.
If redactor registration fails, the token is discarded and compilation falls
back to anonymous resolution. The transport adds `Authorization: Bearer` only
for requests to `api.github.com`, never for arbitrary URLs. Archive redirects
must strip both `Authorization` and cookies before reaching
`codeload.github.com`.

The token is compiler-owned ephemeral state. It must never enter a plan, plan
digest input, pipeline YAML, workflow expression context, action metadata or
inputs, subprocess environment, command argument, URL, cache manifest, error,
metric label, trace attribute, or log. It is dropped with the resolver after
compilation. Runtime jobs never receive it. Existing workflow `GH_TOKEN` /
`GITHUB_TOKEN` support remains a separate scoped-token path.

The backend may cache one encrypted token per installation until five minutes
before expiry, but must not persist plaintext or return an expired token. The
client does not persist or refresh during a normal compile. On an authenticated
GitHub `401`, a future integration may discard the token, mint once more, and
retry the failed request once; it must not loop.

Local development and corpus evaluation use a separate explicit fallback. Those
non-hosted tools may read `BUILDKITE_GHA_GITHUB_TOKEN` when an operator sets it
and otherwise resolve anonymously. They must apply the same host-scoped header,
redirect stripping, bounded error, and non-leakage rules. They must not infer or
read `GH_TOKEN`, `GITHUB_TOKEN`, a Git credential helper, or another ambient
provider credential. Hosted `upload` must ignore
`BUILDKITE_GHA_GITHUB_TOKEN` and obtain its credential only through the
job-bound in-memory Agent endpoint; this preserves the zero-configuration hosted
UX and prevents pipeline environment from selecting hosted source authority.

### Availability and rate limits

Authentication is an availability optimization, not authorization for source.
If endpoint construction, minting, response validation, or mandatory redaction
fails, hosted compilation emits one sanitized warning and uses the existing
anonymous public resolver. Cancellation and deadlines still stop compilation.
No failure may cause the workflow-token endpoint,
`BUILDKITE_GHA_GITHUB_TOKEN`, a PAT, or ambient `GH_TOKEN`/`GITHUB_TOKEN` to be
used by hosted compilation instead.

For `429` or `503`, `PublicSourceToken` will own the follow-up retry behavior:
honor only an integer `Retry-After`, cap the wait at five seconds and the mint
attempt count at two, then fall back anonymously. The current unwired client
preserves sanitized status and retry information but intentionally makes one
request; before compiler wiring, the provider must be extended to perform that
retry internally rather than make the compiler parse error text. For GitHub API
primary or secondary rate-limit responses, report the existing
bounded `RateLimitError`; one token refresh is allowed only for `401`, not for
rate limits. Metrics distinguish authenticated resolution, anonymous fallback,
mint status class, and GitHub rate-limit class without recording repository
names, refs, headers, URLs containing credentials, or tokens.

## Repository ownership and delivery

This repository owns the small `PublicSourceTokenProvider` client boundary and
its strict Agent API transport tests. They will land in a backend-coordinated
client-and-wiring change. This ADR is design-only and does not enable hosted
authenticated resolution.

Wiring depends on the local `BUILDKITE_GHA_GITHUB_TOKEN` workstream's source
transport hardening (bounded response parsing, host-scoped authorization,
redirect credential stripping, and token validation). The hosted path may reuse
that transport behavior, but must not read the local fallback variable or reuse
the workflow-token interface, endpoint, plan fields, permission maps, or
expression/runtime exposure. The action-source package currently guarantees
credential-free clients and strips `Authorization` from every request. Adding
authenticated API-only headers therefore needs a focused follow-up in that
package; it should not be hidden inside a general resolver refactor.

The follow-up is complete when hosted `upload` constructs the provider from the
current job's Agent endpoint, job ID, and Agent access token; obtains and masks
one credential; injects it only into API requests; and proves anonymous fallback
and non-leakage. Outside a Buildkite job, networked validation and corpus tools
remain anonymous unless the operator explicitly sets
`BUILDKITE_GHA_GITHUB_TOKEN`.

## External Buildkite backend work

The Agent API/backend repository must:

1. route the exact operation and authenticate the path job ID with that same
   job's Agent access token;
2. reject bodies, query parameters, cross-job tokens, finished/revoked jobs, and
   non-Agent authentication;
3. load only the dedicated App installation from server configuration and mint
   a token without caller-controlled repositories or permissions;
4. enforce organization/feature rollout policy without consulting workflow or
   pipeline repository data;
5. set `Cache-Control: no-store`, redact tokens before all logging/tracing, avoid
   plaintext persistence, and emit token-free audit/availability metrics;
6. bound caching and refresh by GitHub's expiry and prevent stampedes; and
7. map upstream throttling/unavailability to sanitized `429`/`503` responses and
   bounded numeric `Retry-After` values.

Backend tests must cover exact success response and headers; empty request;
rejection of every caller-controlled field/query; same-job and cross-job Agent
authorization; revoked/finished jobs; feature-disabled organizations; App and
installation misconfiguration; GitHub mint timeout, malformed response, `401`,
`403`, `429`, and `5xx`; cache reuse/expiry/concurrent refresh; and a log/trace
sink assertion that neither the Agent token, installation token, nor upstream
body is emitted. The live public/private conformance test above is a release
gate, not a replacement for unit and integration tests.

## Key learnings from pressure-testing

- Reusing the pipeline installation token cannot guarantee public-only access,
  even if the client promises to request only public repositories.
- Requesting `Contents: read` would make operational repository selection part
  of the private-source boundary; zero App permissions is narrower and must be
  live-tested against every endpoint the resolver uses.
- Authenticated resolution must remain optional until redaction and strict
  host-scoped header injection are wired. Landing only the client contract now
  avoids weakening the source package's current credential-free invariant.

## Unresolved assumptions

- GitHub must continue allowing this zero-permission installation token to call
  all four production read paths (refs, annotated tags, commits, and tarballs)
  for arbitrary public repositories. The live conformance test decides this;
  do not add `Contents: read` merely to make the test pass.
- The Agent API owner must confirm whether `404` or another status is the
  platform convention for an undeployed/disabled operation. The client can
  tolerate either because integration falls back anonymously.
- The backend repository and its concrete routing/configuration names are not
  present here, so exact backend file paths cannot be named from this checkout.
