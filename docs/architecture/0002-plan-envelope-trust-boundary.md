# ADR 0002: superseded plan-envelope prototype

Status: Superseded as the production authorization model; retained as initial prototype
conformance history

Date: 2026-07-22

Superseded: 2026-07-24

## Superseding decision

The ES256 envelope and its eight conformance cases successfully proved bounded
canonical signing, tamper rejection, and build/job/queue binding. Further
pressure-testing established that those properties protect plan integrity and
transport, but do not recreate GitHub Actions' meaningful security boundary.

GitHub controls event identity and protected capability issuance; it does not
isolate `run` and `uses` steps inside a job. Buildkite dynamic pipeline upload is
ordinary pipeline authority. Therefore public, anonymous, tokenless actions on
suitably isolated agents do not require a privileged compiler, plan signer, or
this envelope. Generated jobs inherit Buildkite's configured agent targeting;
operators own the whole-job isolation policy.

Protected capabilities will instead use a control-plane service that:

- authenticates each requesting job with a short-lived Buildkite Job OIDC token
  and exact service audience;
- verifies immutable Buildkite identity plus independent provider event,
  repository, ref, commit, actor, fork, environment, and policy facts;
- issues a narrow, expiring capability grant bound to the exact plan and jobs;
  and
- brokers private source, selected secrets, scoped GitHub App installation
  tokens, environment grants, or explicitly supported compatible OIDC claims.

Plans continue to use content digests and producer-attributed artifacts. The
runtime continues to verify build, importer, job, step, plan, runtime, and any
explicit queue bindings, but those checks do not authorize protected resources.
Buildkite pipeline signing remains optional installation-specific defence in
depth.

The initial plan-envelope decision is retained below as an implementation and
conformance record. Its statements that KMS-backed plan envelopes provide the
production authority boundary are superseded.

## Context

The compiler turns untrusted workflow and event data into a job plan. A digest
can detect changed bytes, but it cannot prove that an authorized compiler
created the plan or prevent a valid plan from being replayed into another
build, step, or queue. The runtime must establish that authority before it
resolves secrets, provider tokens, containers, or any other protected
capability.

Buildkite signed pipelines solve a related but different problem. They sign the
commands, pipeline-defined environment, plugins, matrix configuration, and
repository for each uploaded step, and an agent blocks a missing or invalid
signature by default. They do not authenticate the contents of a plan artifact
downloaded by that step. A production build needs both controls: pipeline
signing protects the executable Buildkite step, and plan-envelope signing
protects the artifact and its authorization claims.

## Decision

The initial prototype uses a detached JSON Web Signature (JWS, RFC 7515) with `ES256`. The
artifact is a small JSON wrapper containing the readable claims, the base64url
protected header, and the base64url 64-byte JOSE signature. The omitted JWS
payload is reconstructed from the claims using the JSON Canonicalization Scheme
(JCS, RFC 8785):

```text
BASE64URL(UTF8(JCS(protected-header))) + "." +
BASE64URL(UTF8(JCS(claims)))
```

The protected header is exactly `alg`, `kid`, and `typ`. `alg` is `ES256`,
`typ` is `buildkite-gha-plan-envelope+jws`, and `kid` identifies one immutable
verification key. Unknown header fields, algorithms, types, and key IDs are
rejected. Base64url padding is forbidden. The ECDSA signature uses the JOSE
fixed-width `R || S` encoding rather than ASN.1 DER.

JCS applies only to parsed JSON values. Producers reject duplicate object keys,
invalid Unicode, non-I-JSON numbers, and inputs that cannot be represented by
RFC 8785; they do not canonicalize arbitrary source bytes and do not apply
Unicode normalization. Arrays with set semantics, including capability lists,
must be emitted in lexicographic order so a deterministic compiler produces
one envelope payload.

The claims bind:

- the canonical plan digest, plan schema, compiler version, and compiler
  distribution digest;
- Buildkite organization, pipeline, and build UUIDs;
- provider event identity, repository, ref, source commit, canonical event
  payload digest, workflow path and digest, trust classification, and the
  source of the event attestation;
- the deterministic Buildkite step key and the one permitted queue;
- the maximum capabilities the plan may request; and
- issuer, unique envelope ID, issuance time, expiry, and signing-key identity.

The plan independently declares its required capabilities. Both the signed
ceiling and current runtime policy are upper bounds: a capability is available
only when the plan requests it, the envelope permits it, and local policy
permits it for the verified event, queue, and repository. An envelope never
grants a capability merely by naming it.

The first production signer is a dedicated AWS KMS asymmetric
`ECC_NIST_P256` `SIGN_VERIFY` key. The compiler role receives only `kms:Sign`
for that key and cannot export private material. A small signer adapter requests
`ECDSA_SHA_256`, converts the KMS DER signature to JOSE `R || S`, and returns no
other authority. The key is separate from any Buildkite pipeline-signing key,
so rotating or revoking one trust domain does not silently alter the other.
Fixture keys are local test keys and carry no production trust.

## Verification order and failure behavior

The runtime performs these checks before plan interpretation or capability
resolution:

1. Apply artifact size and JSON depth limits, reject duplicate keys, parse the
   envelope, and validate its structural schema. The plan may be hashed at this
   stage but is otherwise inert.
2. Decode and structurally validate the protected header. Reject an unsupported
   `alg` or `typ`, an unknown `kid`, or a locally revoked key.
3. Recreate the JCS claims bytes and verify the JWS signature. Claims do not
   influence routing, diagnostics containing attacker-chosen identifiers, or
   authorization until this succeeds.
4. Check `iat <= now < exp`, `exp > iat`, and `exp - iat <= 86400`. The initial prototype has
   no clock-skew allowance: compiler and runtime queues must have synchronized
   clocks. A retry cannot mint or extend an envelope; an expired build must be
   rebuilt. A longer supported queue or retry window requires a later contract
   revision rather than an installer override.
5. Match the organization, pipeline, and build UUIDs to immutable Buildkite job
   context.
6. Match the step key and actual agent queue exactly. The initial prototype reads
   `BUILDKITE_STEP_KEY` and `BUILDKITE_AGENT_META_DATA_QUEUE`; an absent value is
   a verification failure.
7. Re-evaluate current local policy using the verified provenance and require
   the envelope capability ceiling to be a subset of that policy. In
   particular, `manual-unattested` and untrusted events cannot reach protected
   queues or capabilities unless an explicit local rule permits the exact
   combination.
8. Hash the received canonical plan bytes, compare its digest to the envelope,
   validate the plan schema, and require its compiler identity, workflow/event
   digests, target step, and target queue to equal the envelope claims.
9. Require the plan's capability request to be a subset of both the signed
   ceiling and current local ceiling, then begin execution.

Every missing, malformed, mismatched, expired, revoked, unsupported, or
unverifiable value is a terminal verification failure. The runtime emits a
bounded diagnostic code, publishes no plan-controlled output, resolves no
secret or token, starts no container, and does not fall back to unsigned mode.
Unsigned development plans run only through a separate explicit command path
whose local policy ceiling is empty.

## Trust-root distribution, rotation, and revocation

The installer distributes an atomic, root-owned JWKS containing public P-256
keys to runtime queues. It also distributes an independently managed revoked
`kid` denylist and the local queue/event/capability policy. Runtime jobs neither
fetch trust roots from the plan artifact nor accept a `jku`, `x5u`, embedded
JWK, or key alias from the envelope.

Rotation adds the new public JWK to every verifier, verifies deployment, then
switches the signer to the new immutable `kid`. The old public key remains
until the maximum envelope lifetime plus clock skew has elapsed, after which it
is removed. Revocation is different from orderly rotation: disable the KMS key,
place its `kid` on the verifier denylist immediately, deploy the denylist to all
runtime pools, and require affected builds to be rebuilt. The denylist wins
over a still-present JWK and over an otherwise valid signature.

## Relationship to Buildkite signed pipelines

Current Buildkite documentation says signed pipelines use JWS; agent uploaders
can sign with JWKS or AWS/GCP KMS, while runners verify using configured trust
material. The documented signature covers step instructions and repository,
and the default verification behavior is `block`. Buildkite also documents
that both the generator step and its generated steps must be signed for dynamic
pipelines. The generated `buildkite-gha` pipeline must therefore use the
agent's supported pipeline-signing flags or configuration in addition to
publishing these plan envelopes.

The two signatures are deliberately not interchangeable:

| Control | Protects | Verified by | Key scope |
| --- | --- | --- | --- |
| Buildkite pipeline signature | Uploaded step instructions and repository | Buildkite agent before running the job | Buildkite pipeline signer |
| `buildkite-gha` plan envelope | Plan artifact, provenance, build/job/queue binding, and capability ceiling | `buildkite-gha` runtime before interpreting the plan | Dedicated plan-envelope signer |

Primary references checked on 2026-07-22:

- [Buildkite signed pipelines](https://buildkite.com/docs/agent/self-hosted/security/signed-pipelines)
- [Buildkite dynamic pipelines](https://buildkite.com/docs/pipelines/configure/dynamic-pipelines)
- [Buildkite Agent pipeline upload](https://buildkite.com/docs/agent/cli/reference/pipeline)
- [Buildkite environment variables](https://buildkite.com/docs/pipelines/configure/environment-variables)
- [RFC 7515: JSON Web Signature](https://www.rfc-editor.org/rfc/rfc7515)
- [RFC 8785: JSON Canonicalization Scheme](https://www.rfc-editor.org/rfc/rfc8785)

## Prototype oracle questions

Documentation establishes the configuration surface, but these behaviors still
need live Buildkite builds before production support is claimed:

1. Does a signed bootstrap job dynamically uploading with AWS KMS produce steps
   accepted by every intended self-hosted, Hosted, and Kubernetes verifier
   configuration, including mixed queues during rotation?
2. Which immutable job context is available before hooks and plugins, and can a
   custom bootstrap supply `BUILDKITE_BUILD_ID`, `BUILDKITE_STEP_KEY`, and the
   actual queue to the verifier without any plan-controlled override path?
3. Does artifact download constrained by the compiler step always return the
   intended immutable plan under retries and duplicate artifact names, and
   what observable producer identity can the runtime verify?
4. What diagnostic and job state does each pipeline-signature rejection path
   produce, and can an unsigned or wrongly signed generated upload ever reach a
   command hook before it is blocked?
5. How do jobs queued across a plan-key or pipeline-key rotation behave, and
   what operational delay is required before removing the old verification
   root?

## Consequences

The contract is intentionally narrow: one algorithm, one production backend,
one exact queue, and a small fixed capability vocabulary. Supporting another
cloud signer or algorithm requires a new accepted protected-header profile,
not silent algorithm negotiation. The readable wrapper makes fixtures and
audits simple, while the detached JWS and JCS rules keep verification portable
across implementations.
