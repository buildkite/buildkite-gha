# ADR 0003: Authenticate protected capability grants through a Buildkite control plane

- Status: Proposed
- Date: 2026-08-04
- Decision owners: `buildkite-gha` and Buildkite platform maintainers

## Context

The service-free compatibility path can execute public, anonymous workflow code
without giving it authority beyond its Buildkite job. Summaries, annotations,
native artifact adapters, public exact-SHA checkout, and the audited
`actions/cache` v6 client all use public Agent or explicitly configured
cache-v2 interfaces. With two narrow exceptions described below, they do not
establish general authority for private source, secrets, provider tokens,
environments, privileged queues, or compatible OIDC claims.

A plan cannot authorize those protected values. Workflow and event files are
compiler inputs, dynamic pipeline upload is ordinary pipeline authority, and
the Phase 0 runtime binding proves integrity rather than provider provenance.
The runtime needs a separate authorization result tied to both the actual
Buildkite job and independently established provider facts.

Buildkite Job OIDC supplies the job half of that identity. A running Agent can
mint a short-lived token for an exact audience with immutable organization,
pipeline, build, job, cluster, and queue identifiers. It does not prove the
complete GitHub event, actor, fork relationship, workflow source, installation,
or policy decision. A Buildkite-owned control plane must join those facts before
issuing any grant.

Phase 6 therefore starts the protected path with a signed **no-op grant**. It
proves authentication, provenance, policy, signing, verification, expiry, and
audit boundaries while carrying an empty capability set and returning no
credential. General private source, non-provider secrets, GitHub authority
beyond the fixed checkout and scoped workflow-token exceptions, environments,
and compatibility OIDC remain deferred.

## Decision

### Ownership and trust domains

The grant issuer is a Buildkite platform-owned control plane, not code running
inside the requesting job and not the `buildkite-gha` compiler. The expected
initial implementation belongs with the Buildkite service that owns build/job
identity and GitHub provider events. A separately deployed service is acceptable
only if it consumes equivalent authoritative interfaces and does not trust
provider facts supplied by the CLI.

This repository owns:

- the versioned request and grant contracts;
- the canonical event-snapshot serialization used to derive `event_digest`;
- the client that obtains Job OIDC and requests a grant;
- the runtime verifier and fail-closed capability intersection; and
- conformance fixtures proving interoperability and rejection behavior.

The control plane owns:

- Buildkite OIDC verification and immutable job lookup;
- provider-event provenance and event-to-build correlation;
- organization, event, queue, environment, and permission policy;
- grant signing, key rotation, revocation, and audit records; and
- later credential brokering behind the same authorization decision.

The control-plane signing key is a separate trust domain from Phase 0 probe
keys, Buildkite pipeline-signing keys, cache tokens, and provider credentials.
The concrete Phase 0 `RuntimeBinding` and its test key must not be treated as a
grant or production trust root. Generic bounded JWS helpers may be extracted
only if issuer, type, key configuration, and verification policy remain
separate.

### Job authentication

The client obtains a fresh token with the Agent command equivalent to:

```sh
buildkite-agent oidc request-token \
  --audience "$BUILDKITE_GHA_CONTROL_PLANE_URL" \
  --claim organization_id,pipeline_id,build_id,cluster_id,queue_id,queue_key
```

`BUILDKITE_GHA_CONTROL_PLANE_URL` is one installer-owned, origin-only HTTPS
service identifier. The client uses that same normalized origin as the OIDC
audience and appends only the fixed request path. User information, non-root
paths, queries, fragments, and redirects are rejected. The setting cannot be
overridden by plan-, workflow-, repository-, or ordinary build-supplied
environment. The client captures the token without logging it, keeps it in
memory for one exchange, and sends it only to that bound service as the
control-plane bearer credential. It never reads or forwards the ambient Agent
access token to that service or to action code.

The control plane validates the token against the fixed Buildkite issuer
`https://agent.buildkite.com` and its configured discovery/JWKS endpoints. It
requires the exact audience, signature, `nbf`, `iat`, `exp`, organization,
pipeline, build, job, step, runner environment, cluster, and queue claims needed
by policy. Missing required claims fail closed. Slugs and branch names are
context only; immutable IDs are the authorization binding.

### Grant request

The initial endpoint accepts one bounded JSON request over HTTPS:

```http
POST /v1/capability-grants
Authorization: Bearer <Buildkite Job OIDC token>
Content-Type: application/json
```

```json
{
  "schema": "buildkite-gha/capability-request/v1",
  "plan_digest": "sha256:<64 lowercase hex characters>",
  "event_digest": "sha256:<64 lowercase hex characters>",
  "requested_capabilities": []
}
```

The request is size-bounded, rejects unknown fields and duplicate capability
names, and requires a sorted capability list from the fixed plan vocabulary.
It deliberately does not contain Buildkite IDs or provider identity claims as
authoritative inputs. The service obtains Buildkite identity from verified
OIDC and provider facts from its own event records or authenticated provider
queries. Request fields only identify the inert plan and event snapshots to
which the resulting decision must bind.

The no-op proof requires `requested_capabilities` to be empty. Later revisions
may request protected capabilities already declared by the exact plan, but
cannot silently broaden this version's meaning.

### Independent provider provenance

For GitHub, the service joins the verified Buildkite build to an authenticated
GitHub App webhook record or an equivalently authoritative provider record. At
minimum, policy receives immutable installation, owner, repository, event or
delivery, actor, ref, commit, and pull-request base/head/fork facts where they
apply. Using this repository's versioned canonical event-snapshot contract, the
service recomputes the digest from that authoritative record and requires it to
match the inert request. The service treats `plan_digest` as an opaque binding;
only the runtime has the plan bytes and verifies that binding against the
decoded plan.

Querying GitHub after the fact may supplement those records but cannot establish
historical webhook facts that the API does not preserve. A provider event field
copied from the plan, Buildkite environment, build message, or request body is
never sufficient authorization evidence. If event-to-build correlation is
absent or ambiguous, the service denies the request.

Cursor Origin will implement the same provider contract with Origin-owned
event and source identities. Its absence does not weaken or add fallbacks to
the GitHub contract.

### Policy decision

The effective capability set is the intersection of:

```text
exact plan request
∩ organization policy
∩ authenticated provider-event policy
∩ provider installation permissions
∩ environment policy
∩ Buildkite cluster and queue policy
```

The service records the policy version and a bounded decision reason. A no-op
request still runs identity, provenance, and policy checks; an empty result is
not permission to skip them. Unknown capabilities, an untrusted fork transition,
a mismatched queue, an unattested event, or unavailable policy data is a denial.

### Signed grant

An allowed decision returns a standard three-part compact JWT signed with
ES256. Its signature covers the exact transmitted protected-header and payload
bytes; it has no detached payload and does not use Phase 0's JCS reconstruction
or canonical-byte equality rule. Unknown top-level claims are rejected by this
bounded versioned contract.

The protected header fields are exactly `alg`, `kid`, and `typ`. `alg` is
`ES256`; `typ` is
`buildkite-gha-capability-grant+jwt`; and `kid` selects one immutable
control-plane verification key. The token must not carry or honor `jku`, `x5u`,
an embedded JWK, algorithm negotiation, or unrecognized critical headers.

The signed claims contain:

- schema, issuer, fixed runtime audience, unique grant ID, `iat`, `nbf`, and
  `exp` with a bounded short lifetime;
- immutable Buildkite organization, pipeline, build, job, cluster, and queue
  IDs, plus the exact queue key, step key, and runner environment;
- versioned provider provenance, including immutable repository/event identity,
  ref, commit, actor and fork facts relevant to the event;
- exact plan and canonical event digests;
- the sorted, unique granted capability set; and
- the policy version that produced the decision.

The initial successful grant contains an empty capability set. It contains no
secret, checkout credential, provider token, cache token, environment value, or
credential URL.

The issuer publishes an operator-configured JWKS over HTTPS. The same
installer-owned control-plane configuration binds the request origin, Job OIDC
audience, grant issuer, fixed runtime audience, and JWKS location outside the
plan. Rotation overlaps old and new public keys for the maximum grant lifetime;
emergency revocation takes precedence over a still-published key. Private keys
remain in the platform signing boundary.

### Narrow job-bound provider credential exceptions

Buildkite has two native, job-bound credential paths that support narrower
integrations without waiting for the general grant protocol. Repository
checkout uses the Agent's Git credential helper and delegates authorization for
the concrete URL supplied by Git to Buildkite's repository-provider backend.
Workflow `GITHUB_TOKEN` support uses the Agent API below to mint a GitHub
installation token for the exact current job, exact pipeline repository, and
caller-requested permission set. The server authenticates the request with the
same job's Agent access token, independently requires the requested repository
to equal the pipeline's configured GitHub repository, and validates permissions
against its own allowlist:

```http
POST /v3/jobs/<current-job-id>/github_scoped_access_token
Authorization: Token <current-job Agent access token>
Content-Type: application/json
```

```json
{
  "repo_url": "https://github.com/<event owner>/<event repository>",
  "permissions": {
    "contents": "read"
  }
}
```

The compiler adds `provider-token-read` after resolving the exact
`actions/checkout` adapter and validating its repository, ref, depth, path,
submodule, and credential-persistence inputs. Fresh same-process compiler
provenance permits that one capability through upload admission. The runtime
uses `buildkite-agent git-credentials-helper` only when the immutable plan has
that capability, a root checkout selector names the verified adapter, and the
server-provided job environment indicates repository-provider Git credentials
are enabled. Otherwise it performs the same checkout anonymously.

The event repository and exact SHA remain the only checkout targets. The
credential helper is configured as a command-scoped Git option only for the
fetch, uses HTTP-path matching, receives the current job's Agent API identity,
and is not persisted. The Buildkite backend independently authorizes the
concrete repository URL received through Git's credential protocol. No workflow
input can select another credential target or access level, and the job's Agent
credential is not placed in plans or workflow environments.

A second bounded integration uses the scoped-token endpoint for a synthetic
`secrets.GITHUB_TOKEN` or an action metadata input default that references
`github.token`. A workflow must declare an explicit, non-empty permission
mapping and either statically reference that exact secret or invoke an action
whose effective default statically references the token. The compiler emits the
API-normalized permission map into a v6 plan, adds `provider-token-write`, and
records same-process `workflow-permissions` provenance. Upload admission accepts
only that exact compiler provenance. A serialized plan cannot self-authorize
the capability.

The runtime requests one token for the plan's exact event repository and exact
permission map. It validates both independently, masks the returned token, and
requires Agent redaction registration before making the synthetic secret
available to expression evaluation. Job-level permissions replace workflow
defaults; flattened local reusable workflows can only narrow caller authority.
Permission aliases, implicit defaults, empty grants, and `id-token` fail
closed. `github.token` is exposed only while evaluating effective action
metadata input defaults; workflow-authored references and ambient
`GITHUB_TOKEN` environment injection remain unsupported.

These exceptions do not establish authenticated provider event, fork, or actor
provenance. The server's independent pipeline-repository comparison,
organization enablement, and permission allowlist are authoritative, but
pipelines remain responsible for whether untrusted workflow changes may request
an allowed write permission. Private actions, arbitrary reusable-workflow
source, selected non-provider secrets, environments, and OIDC still require the
general control-plane decision in this ADR.

### Runtime verification and capability use

The runtime treats the response as inert bytes until all of these checks pass:

1. Bound response size and structural depth.
2. Validate the protected header and select a configured, non-revoked key.
3. Verify the JWS signature before using any claim in routing or diagnostics.
4. Validate schema, issuer, audience, ID, time window, and capability vocabulary.
5. Match every Buildkite claim exposed through immutable current-job context,
   including build/job identity, step key, and queue key. IDs that are present
   only in verified Job OIDC are enforced by the control plane and retained in
   the signed grant for audit and later platform-supported runtime comparison.
6. Match the plan and event digests to the exact decoded plan.
7. Require the granted set to be a subset of the plan request and current local
   runtime policy.
8. Resolve only the intersection of plan request, signed grant, and local
   policy, at the point where a protected value is needed.

For the no-op slice, the runtime verifies and discards the empty grant. It must
not initialize a secret resolver, change queue admission, enable private action
resolution, expose a token to child processes, or broaden the plan's capability
ceiling. Public tokenless jobs do not contact the control plane unless running
the explicit proof; service absence cannot regress their existing path.

Every malformed, expired, mismatched, broadened, untrusted, unavailable, or
ambiguous result fails before workflow execution obtains a protected value.
There is no unsigned fallback and no fallback to plan-declared provenance.

### Audit and observability

The control plane writes one bounded audit record for every decision containing
the authenticated Buildkite IDs, provider event ID, plan and event digests,
requested and granted capability names, policy version, grant ID, outcome, and
bounded reason code. It does not record bearer tokens or future returned
credentials.

The client emits bounded reason codes without response bodies or
attacker-controlled identity strings. Hosted proof requires an independent
read-only observation of both the generated job and its corresponding audit
record; a successful job alone does not prove policy or audit behavior.

## Initial implementation sequence

1. Finalize the request/grant schemas and platform endpoint ownership.
2. Implement the platform verifier for Buildkite OIDC and authoritative GitHub
   event-to-build correlation.
3. Implement empty-request policy, signed empty grants, JWKS publication, and
   audit records without any credential backend.
4. Add the `buildkite-gha` OIDC client and grant verifier behind explicit
   control-plane configuration.
5. Add positive and negative conformance fixtures for signature, audience,
   expiry, build/job/step/queue, plan/event digest, provider event, policy,
   capability broadening, key rotation, revocation, and service absence.
6. Run one exact-commit hosted no-op proof and independently observe its audit
   binding before considering any credential-returning slice.

## Deferred decisions

Apart from native repository-provider checkout and scoped token exceptions
above, this ADR does not authorize or choose storage for private actions,
reusable workflow source, selected non-provider secrets, general workflow
access to `github.token`, environment grants, or compatibility OIDC claims. It
does not choose a secret manager, broader cross-repository GitHub App authority,
customer policy language, administrative UI, or production service hostname.
Those decisions require separate reviewed slices after the no-op boundary is
proven.

The existing cache-v2 GHAC token exchange remains separate. Cache tokens are
minted through the current Buildkite job, scoped by the cache service, and
exposed only to the audited cache action lifecycle. A capability grant neither
contains nor replaces them.

The job-bound provider credential exchanges are likewise separate. Their
server-side repository authorization and either a Git-selected checkout URL or
compiler-owned explicit workflow permission map are not a substitute for the
event, policy, signing, and audit contract required by a general capability
grant.

## Consequences

- Public checkout retains no new service dependency when repository-provider
  credentials are not enabled for the job.
- Private checkout uses Buildkite's existing repository-provider policy without
  exposing a reusable workflow credential or requiring a separate user option.
- The first protected-path milestone can prove the security boundary without
  risking a real credential.
- Provider provenance and policy must exist in a Buildkite-owned service before
  private compatibility features can ship.
- The CLI gains a small client and verifier, while signing keys, provider
  records, and audit state remain outside workflow-controlled execution.
- Phase 0 signing code may yield generic cryptographic helpers, but its concrete
  keys, issuer, claims, and capability ceiling remain non-authorizing.
- A full Phase 6 implementation now explicitly spans this repository and a
  Buildkite platform service; repository-local code alone cannot satisfy it.

## References checked

- [Buildkite Agent OIDC](https://buildkite.com/docs/agent/cli/reference/oidc),
  checked 2026-08-04
- [Buildkite OIDC security guidance](https://buildkite.com/docs/pipelines/security/oidc),
  checked 2026-08-04
- [OpenID Connect Core 1.0](https://openid.net/specs/openid-connect-core-1_0.html)
- [RFC 7515: JSON Web Signature](https://www.rfc-editor.org/rfc/rfc7515)
- [RFC 7519: JSON Web Token](https://www.rfc-editor.org/rfc/rfc7519)
- [ADR 0002: Phase 0 plan-envelope trust experiment](0002-plan-envelope-trust-boundary.md)
