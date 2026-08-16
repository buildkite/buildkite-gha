# GitHub Actions service container parity

## Outcome

`buildkite-gha` supports GitHub-compatible Linux service containers in the
production hosted profile. The [compatibility reference](../compatibility.md#containers-and-services)
owns the supported syntax and limits. The [security model](../security.md#isolate-the-whole-job)
owns the isolation and credential boundaries.

Implicit GHCR authentication remains unsupported because Buildkite does not yet
provide a policy-scoped workflow token for registry login. Use explicit
credentials.

## Compatibility target

The implementation follows the public GitHub Actions contract and
`actions/runner` at commit
[`02eb22aaa07eac26f299aa3b46d9e69a478a66d1`](https://github.com/actions/runner/tree/02eb22aaa07eac26f299aa3b46d9e69a478a66d1).
When documentation and runner internals differ:

1. Follow documented workflow syntax and documented rejections.
1. Match observable runner behavior for evaluation, Docker arguments,
   networking, readiness, context, and lifecycle.
1. Keep stricter Buildkite controls when they do not change workflow behavior,
   including redaction, ownership labels, and cleanup verification.

Linux native Docker is the only target. Container hooks, Kubernetes
translation, Windows containers, Docker API emulation, and Docker on macOS are
separate projects.

## Design

### Evaluation and admission

The workflow and immutable job plan own every service field: image,
credentials, environment, ports, volumes, options, command, and entrypoint.
Static values resolve during matrix expansion. Verified `needs` values and
secrets resolve during job initialization, before Docker resources are created.

| Field | Expression contexts |
| --- | --- |
| Service fields | `github`, `inputs`, `vars`, `needs`, `strategy`, `matrix` |
| Credentials | `github`, `vars`, `secrets`, `env` |

Whole-map `fromJSON` expressions preserve declaration order and are structurally
validated before Docker use. They cannot introduce credentials or unproven
capabilities. Empty images skip the service.

### Docker behavior

Service options and commands use a .NET-compatible argument tokenizer. They do
not invoke a shell or perform environment, command, or backtick expansion. All
Docker options pass through except `--network` and `--net`, matching GitHub's
documented restriction.

Each job receives a private bridge network. Container jobs and Dockerfile
actions reach services by service ID. Host jobs use published ports. The
`job.services` context exposes container ID, network, and port mappings.

Named, anonymous, and absolute bind volumes retain Docker semantics. The
runtime snapshots and inspects volumes so cleanup removes only resources created
by the job.

### Authentication and isolation

Explicit registry credentials resolve inside the destination job. Passwords use
standard input, values are masked before use, and Docker receives a private
per-job configuration. Login and pull retries are bounded and cancellation-aware.
Cleanup removes the configuration and verifies that no credential-bearing state
remains.

Broad Docker options, bind mounts, and privileged containers do not create a
security boundary. The hosted queue must provide a disposable machine, no
ambient protected credentials, host resource limits, and an external firewall.

### Readiness and cleanup

Services start in declaration order. Services with Docker health checks wait
with bounded exponential backoff; other services are ready after a successful
start. Failures include bounded status, health, port, and log diagnostics.

Teardown removes the job container first, emits masked and bounded service logs,
then removes services in declaration order, the network, newly created volumes,
and Docker configuration. Unguessable owner labels identify containers and
networks. Remaining owned resources fail the job.

## Delivered slices

| Slice | Result |
| --- | --- |
| Static Docker fidelity | Options, command, entrypoint, volumes, argument ordering, health checks, and compile-time expressions. |
| Explicit credentials | Compiler-owned secret provenance, isolated registry login, retries, masking, and cleanup. |
| Runtime expressions | `needs` fields, ordered whole-map expressions, empty-image skipping, and complete service context. |
| Hosted proof | Exact-commit Buildkite execution and a shared GitHub Actions differential fixture covering networking, ports, health, command, entrypoint, volumes, PostgreSQL, and Redis. |

## Verification

Coverage includes parser, compiler, schema, Docker argument, lifecycle, cleanup,
authenticated registry, and live Docker tests. The aggregate local gate is:

```sh
mise run check
```

Hosted verification uses the `container-runtime` compatibility proof. The same
service fixture runs through GitHub Actions and the Buildkite runtime and records
only externally observable behavior.
