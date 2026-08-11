# ADR 0003: Keep protected capabilities behind Buildkite-owned policy

- Status: Accepted
- Date: 2026-08-04
- Decision owners: `buildkite-gha` and Buildkite platform maintainers

## Context

Workflow files, action metadata, event snapshots, and generated job plans are
inputs controlled by repository code. They can describe required capabilities,
but cannot authorize credentials, protected queues, environments, or provider
identity.

Buildkite Job OIDC can identify the running job. It does not prove the GitHub
event, actor, fork relationship, workflow source, installation, or policy
decision. Broader protected capabilities need both sides of that identity.

## Decision

General protected capabilities require a Buildkite-owned control plane outside
the workflow job and compiler.

The control plane must:

1. authenticate the requesting job with Buildkite Job OIDC;
2. join it to authoritative provider-event data;
3. apply organization, pipeline, event, environment, cluster, and queue policy;
4. issue a short-lived signed grant bound to the job, provider event, plan,
   queue, and allowed capabilities; and
5. record the decision without logging credentials.

The runtime must verify the signature and every binding before using a protected
value. Effective authority is the intersection of the plan request, signed
grant, and local runtime policy. Missing, expired, mismatched, broadened, or
unavailable grants fail closed. There is no unsigned fallback and no fallback
to repository-supplied provenance.

### Ownership

A future implementation splits responsibility this way:

| This repository | Buildkite control plane |
| --- | --- |
| Versioned request and grant contracts | Job OIDC verification |
| Event and plan digests | Authoritative provider-event correlation |
| Client and runtime verifier | Organization and queue policy |
| Fail-closed capability intersection | Signing, rotation, revocation, and audit |
| Conformance tests | Credential brokering |

### Existing narrow integrations

These job-bound integrations remain separate from a general grant protocol:

- verified checkout may use Buildkite's Git credential helper;
- supported `GITHUB_TOKEN` uses may request a repository-scoped token with a
  compiler-resolved permission set; and
- the audited cache action may receive cache-service credentials for its own
  lifecycle.

Their server-side repository and permission checks are authoritative. They do
not prove the full provider event or authorize ordinary workflow secrets,
private actions, environments, or compatible OIDC claims.

## Consequences

- Repository-local code alone cannot add general protected capabilities.
- Public tokenless jobs must not depend on the control plane.
- Private actions, ordinary secrets, environments, and compatible OIDC stay
  unsupported until the platform boundary exists.
- Protocol, rollout, and credential-specific implementation work belongs in
  tracked Linear issues rather than this ADR.

See the [security model](../security.md) for the boundaries operators need to
apply today.
