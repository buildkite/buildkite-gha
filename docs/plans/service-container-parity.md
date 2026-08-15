# GitHub Actions service container parity

## Outcome

`buildkite-gha` supports Linux service containers with the GitHub Actions
fields, expression phases, Docker option behavior, networking, readiness,
service context, explicit registry authentication, and bounded cleanup
described below. Implicit GHCR authentication remains unsupported because no
policy-scoped workflow-token backend is available for registry login.

The implementation retains the immutable plan boundary, compiler-owned
capability provenance, bounded inputs, secret isolation, and deterministic
cleanup.

## Compatibility target

Match the public GitHub Actions workflow contract and the native Docker behavior
of `actions/runner` at commit
[`02eb22aaa07eac26f299aa3b46d9e69a478a66d1`](https://github.com/actions/runner/tree/02eb22aaa07eac26f299aa3b46d9e69a478a66d1).
Recheck that reference when implementing each slice because service `command`
and `entrypoint` are undergoing feature rollout upstream.

Use these precedence rules when GitHub documentation and runner internals differ:

1. Match the documented workflow syntax and documented unsupported inputs.
1. Match native runner behavior for evaluation, Docker argument order,
   networking, registry pulls, readiness, context population, and lifecycle.
1. Preserve a stricter Buildkite boundary only when it does not change workflow
   behavior, such as secret redaction, per-job ownership labels, and cleanup
   verification.
1. Document unavoidable differences in `docs/compatibility.md` rather than
   silently approximating them.

The target service syntax is:

```yaml
services:
  postgres:
    image: postgres:${{ matrix.postgres }}
    credentials:
      username: ${{ secrets.REGISTRY_USERNAME }}
      password: ${{ secrets.REGISTRY_PASSWORD }}
    env:
      POSTGRES_PASSWORD: test
    ports:
      - 5432
    volumes:
      - database:/var/lib/postgresql/data
    options: >-
      --health-cmd pg_isready
      --health-interval 2s
      --health-timeout 5s
      --health-retries 10
    command: postgres -c fsync=off
    entrypoint: docker-entrypoint.sh
```

Support Linux native Docker only. Composite actions cannot declare services.
Container hooks, Kubernetes translation, Windows containers, and Docker API
emulation are separate projects.

## Proposed approach

### Own the complete service definition

Replace the shared minimal container representation with an owned service model
covering image, credentials, environment, ports, volumes, options, command, and
entrypoint. Keep job-container-only behavior separate so future changes do not
accidentally grant service syntax to job containers or vice versa.

The workflow model must retain scalar templates and whole-map service
expressions without retaining actionlint objects or raw YAML. The immutable job
plan must contain either resolved service definitions or compiler-validated
service templates. It must never contain resolved credential values.

Upgrade actionlint to the first pinned release that recognizes the current
service schema. If its released model still feature-gates `command` and
`entrypoint`, decode those two keys in the existing owned raw-container pass and
filter only the corresponding actionlint diagnostic. Do not generally ignore
unknown service keys.

Apply the existing limits for service count, field length, environment size,
and port count. Add equivalent bounds for options, command, entrypoint, volumes,
and the evaluated service object. These are denial-of-service limits, not Docker
option allowlists.

Update:

- `internal/workflow/model.go` and `internal/workflow/parse.go`
- `internal/compiler/compiler.go` and reusable-workflow substitution
- `internal/plan/plan.go` and `schemas/job-plan.schema.json`
- compiler authorization metadata used by `validateUnprivilegedBundle`

### Evaluate services at the same phase as GitHub Actions

GitHub evaluates services during job initialization. Match its context sets
exactly:

| Field | Expression contexts |
|---|---|
| Service fields | `github`, `inputs`, `vars`, `needs`, `strategy`, `matrix` |
| Credential values | `github`, `vars`, `secrets`, `env` |

Empty resolved images skip that service.

Resolve compile-time values while expanding each matrix instance. Retain only
the templates that require verified runtime `needs` values or secrets. This
requires adding `strategy` to the compile context and applying reusable inputs
to services, which the current field-by-field substitution pass omits. Evaluate
remaining fields after dependency result manifests, job environment, and job
secrets are loaded, but before any image pull or Docker resource creation.
Validate the evaluated result again against the same bounds and grammar as a
statically resolved service.

Whole-map expressions such as `services: ${{ ... }}` require the expression
engine to return and validate an object. Do not serialize raw JSON into a shell
command or defer structural validation to Docker.

The admission policy authorizes the compiler-produced service template and its
required capabilities. Runtime evaluation may fill values but cannot introduce
an unproven capability, exceed plan bounds, or add fields absent from the
GitHub service schema.

Update:

- `internal/expression` compile and runtime value evaluation
- `internal/compiler/compiler.go`
- `internal/runtime/job.go`
- `internal/plan/plan.go`

### Pass Docker options broadly

Match GitHub's service `options` contract instead of maintaining a resource-flag
allowlist. The pinned runner passes `options` and `command` as raw argument
strings through .NET process invocation. Go requires an explicit argument
slice, so implement one owned compatibility tokenizer shared by those fields;
do not use POSIX shellword semantics. It must not invoke a shell, environment
expansion, command substitution, or backticks. Cover double quotes, empty
arguments, backslashes before quotes, literal single quotes, and unmatched
quotes with differential tests against the pinned runner.

Reject the network override that GitHub Actions documents as unsupported:
`--network`, `--network=...`, and Docker's equivalent `--net` alias forms. Keep
the rejected set in one function with table-driven tests for long names,
aliases, equals forms, quoting, and false positives. All other syntactically
valid Docker create options pass through unchanged and Docker owns their
version-specific validation.

Runner-owned arguments must remain explicit and ordered consistently with
GitHub Actions: generated name and label, job network and alias, declared ports,
workflow options, environment, volumes, entrypoint, image, then command.
Because later workflow options can override generated names and fixed label
values, give each job an unguessable runtime nonce in the authoritative owner
label key. Find containers by that key after ambiguous creates; treat the fixed
`buildkite-gha` label as diagnostic only.

Broad Docker options, bind mounts, and privileged modes do not create a new
security boundary. They require a disposable whole-job host with no ambient
protected credentials, as already required for arbitrary imported workflow
code. Admission profiles that cannot provide that boundary must reject Docker
capability rather than rewrite workflow options.

Update:

- `internal/workflow/parse.go`
- `internal/plan/plan.go`
- `internal/runtime/containers.go`
- `docs/security.md`

### Support commands, entrypoints, ports, and volumes

Append `command` after the image and emit `entrypoint` as Docker's
`--entrypoint` option. Parse command with the same .NET-compatible tokenizer as
options. Pass entrypoint as one argument. Never execute either value through a
host shell.

Pass each declared port as one `--publish` value instead of implementing a
smaller port grammar. Apply only size and control-character validation before
Docker validates its complete publication grammar. Parse `docker port` output
for IPv4 and IPv6 and populate the numeric container-port mapping used by
`job.services.<service>.ports`. Also expose GitHub's service `id` and `network`
context fields.

Match GitHub's networking modes:

- Container jobs and Dockerfile actions join the per-job bridge network and
  reach services by service ID.
- Host jobs reach declared publications through the Docker host.
- Portless services remain reachable by alias from container jobs but are not
  reachable from host steps.

Pass publication values exactly as GitHub does once the hosted queue confirms
that its VM firewall is the external ingress boundary. If that boundary cannot
be guaranteed, retain loopback publication as an explicit hosted-profile
difference and document it.

Support GitHub's volume forms: anonymous target, named source and target,
absolute host source and target, and optional `ro`. Validate only the documented
shape, bounded length, and control characters. Preserve literal named-volume
names so job and service containers share the same Docker identity. Snapshot
existing volumes before container creation, inspect owned container mounts, and
remove only volume IDs created by the job. This inspection must also cover
volumes introduced through `options`. Remove anonymous volumes with their
containers and include all inspected mounts in leak verification; anonymous
volumes cannot carry ownership labels. Absolute bind mounts retain their host
meaning and therefore rely on whole-job host isolation.

Update:

- `internal/runtime/containers.go`
- `internal/runtime/job.go`
- `internal/expression/condition.go` and runtime context resolution
- `internal/runtime/containers_test.go`

### Add registry authentication within the existing credential boundary

Infer the registry using GitHub runner semantics. When explicit credentials are
present, resolve their expressions after the ordinary job secret broker has
registered masking and redaction and the job environment is available. Match
GitHub's partial-credential behavior: login only when both username and password
resolve non-empty. A partial mapping neither errors nor enables hosted registry
fallback.

Authentication must satisfy the repository's [security model](../security.md)
and be no worse than GitHub Actions; it need not copy the runner's internal
login lifecycle. In particular:

- Plans and generated pipeline YAML contain secret names only. Resolve values in
  the destination job under Buildkite Secret access policy.
- Register values with Agent and local redaction before use. Pass passwords to
  `docker login` through standard input, never argv or command logs.
- Never read or modify the host's ambient Docker configuration. Use private,
  runtime-owned configuration with restrictive permissions, register cleanup
  before login, and remove it within the bounded job cleanup.
- Scope reusable authentication state to one job and one registry and credential
  identity. Reuse within that boundary is allowed; credentials must not cross
  jobs, credential identities, or registries.
- Scrub Docker errors and verify that credentials are absent from plans,
  artifacts, generated YAML, diagnostics, process arguments, and retained
  filesystem state.

The implementation may login once per credential scope or once per pull and may
reuse successful pulls where ordinary Docker semantics permit. Keep retries
bounded and cancellation-aware. These choices are not compatibility promises;
the observable requirement is that supported images authenticate as GitHub
Actions does without broader credential authority, exposure, or persistence.

Match GitHub-hosted implicit GHCR authentication only after the Buildkite
workflow-token backend confirms that its issued token is valid for registry
login and enforces the compiled `packages: read` policy. Until then, require
explicit credentials and report that limitation directly.

Update:

- `internal/workflow/parse.go`
- `internal/compiler/compiler.go`
- `internal/runtime/job.go` and `internal/runtime/containers.go`
- secret and workflow-token tests under `internal/runtime`

### Match startup, readiness, diagnostics, and cleanup

Create and start all services before checking readiness. For services with a
Docker health check, inspect health sequentially with GitHub's exponential
backoff from two seconds, capped at 32 seconds. Continue while Docker reports
`starting`; stop on `healthy`, a terminal unhealthy state, cancellation, or the
job timeout. A service without a Docker health check is ready after a successful
start.

On failure, print bounded status, health, port, and log diagnostics. On normal
completion, emit bounded logs for each initialized service to match GitHub's
end-of-job behavior.

Register cleanup before the first pull. Track requested names before each create
call and query the unguessable owner-label key when workflow options override a
name or a create returns an ambiguous failure. Match GitHub's teardown order:
remove the job container first, then services in declaration order, followed by
the network, newly created named volumes, and temporary Docker configurations.
Keep the current bounded cleanup budget, truncate logs, and fail the job when
owned resources remain; those are intentional stricter differences from
GitHub's cleanup.

Every container and network must carry a globally recognizable `buildkite-gha`
label and the nonce-bearing owner-label key. Track volumes by pre-create
snapshot and inspected volume ID because anonymous and create-on-use volumes
cannot reliably carry labels. Add build and job identifiers only as
non-authoritative diagnostics. Never remove a resource based only on a
workflow-controlled name or broad global label.

Update:

- `internal/runtime/containers.go`
- `internal/runtime/containers_test.go`
- `scripts/container-runtime-probe`

## Delivery slices

All four slices are implemented. The exact same service-container oracle runs
as a GitHub Actions workflow and in the exact-commit hosted Buildkite proof.
Implicit GHCR authentication remains the named backend-dependent limitation.

### 1. Static syntax and Docker fidelity

Add statically resolvable `options`, `command`, `entrypoint`, and volumes. Pass
all Docker options except the documented network options, adopt GitHub argument
ordering, align health waiting, and extend cleanup to owned volumes. Include
compile-time `github`, `inputs`, `vars`, `strategy`, and `matrix` interpolation
for every non-secret field, including compile-time empty-image skipping.

This slice should run common public Postgres, MySQL, Redis, RabbitMQ, MinIO, and
Elasticsearch service definitions without workflow rewrites.

### 2. Explicit registry credentials

Add credential references, secret capability provenance, isolated login and
pull retries, masking, and authenticated local-registry runtime tests. Do not
add ambient Docker credentials or arbitrary credential helpers. Evaluate
credentials after the job environment and match GitHub's partial mapping
behavior. Choose the simplest login reuse strategy that meets the security
invariants above; do not require per-container parity with runner internals.

### 3. Runtime service expressions and complete context

Evaluate `needs`-dependent fields and whole-map service expressions during job
initialization. Add runtime empty-image service skipping and the full service
`id`, `network`, and `ports` context. Revalidate the evaluated object before
Docker use.

### 4. Hosted parity proof and rollout

Expand `testdata/container-runtime` to cover host and container jobs, dynamic
ports, health options, command and entrypoint, shared volumes, conditional
services, and authenticated pulls. Run the exact-commit hosted proof on the
production queue and add representative starter workflows to the profile
corpus.

Update `docs/compatibility.md`, `docs/security.md`, `README.md`, and smoke
evidence only after each behavior has both admission and runtime proof. Keep
unsupported platforms and backend-dependent GHCR authentication explicit.

## Verification

Each slice must include:

- Parser tests for shorthand and mapping syntax, every field, expressions,
  bounds, and diagnostics.
- Compiler and plan-schema tests proving deterministic serialization,
  capability and secret provenance, matrix/reusable substitution, and
  fail-closed unknown fields.
- Exact Docker argv tests for options and quoting, commands, entrypoints, ports,
  volumes, labels, password-stdin login, and argument order.
- Runtime lifecycle tests for partial startup, cancellation, health transitions,
  port context, job-first cleanup, ambiguous Docker failures, and leak
  detection.
- Live Docker tests for host and container networking, common databases,
  unhealthy diagnostics, named-volume sharing, and an authenticated temporary
  registry, including credential-scope and cleanup assertions.
- `mise run check`, `mise run smoke:profile`, and the exact-commit hosted
  container runtime proof.

Maintain a small differential workflow that runs the same fixture on GitHub
Actions and Buildkite and records only externally observable results: service
selection, DNS, mapped ports, command and entrypoint behavior, health gating,
volume contents, and context values. Do not assert GitHub's generated Docker
resource names or other private implementation details.

## Completion criteria

Service-container parity is complete when:

- Every currently documented GitHub service field is accepted with the same
  expression contexts and observable behavior.
- Docker create options pass through except options GitHub documents as
  unsupported; new Docker resource flags do not require compiler changes.
- Public and explicitly authenticated private images work without ambient or
  persisted credentials.
- Host jobs, container jobs, and Dockerfile actions reach services through the
  documented networking path and expose the complete service context.
- Health checks gate steps, failures produce useful bounded diagnostics, and
  cancellation cannot strand owned resources.
- The representative service corpus passes local, hosted-profile, exact-commit
  hosted, and GitHub differential proofs.
- Remaining differences are named backend or platform constraints, not silent
  syntax rejection or approximation.
