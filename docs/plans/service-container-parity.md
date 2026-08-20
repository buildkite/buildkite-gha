# GitHub Actions service container parity

## Status

Linux jobs support GitHub-compatible service containers in the production
`hosted` profile.

The [compatibility reference](../compatibility.md#containers-and-services) owns
the supported fields and limits. The [security model](../security.md#isolate-the-whole-job)
owns isolation and credential guidance.

Implicit GHCR login is not wired into service image pulls. Use explicit
registry credentials.

## Compatibility target

The implementation follows GitHub's public workflow contract and
[`actions/runner` at commit `02eb22a`](https://github.com/actions/runner/tree/02eb22aaa07eac26f299aa3b46d9e69a478a66d1).

When the documentation and runner internals differ:

1. Follow the documented workflow syntax and rejections.
1. Match observable runner behavior for evaluation, Docker arguments,
   networking, readiness, contexts, and cleanup.
1. Keep stricter Buildkite controls when they do not change workflow behavior,
   including redaction, ownership labels, and cleanup checks.

This work targets native Docker on Linux. Windows containers, Docker on macOS,
Kubernetes translation, container hooks, and Docker API emulation are separate
projects.

## Design

### Evaluation

The workflow and immutable job plan own each service's image, credentials,
environment, ports, volumes, options, command, and entrypoint.

Static values resolve during matrix expansion. Verified `needs` outputs and
secrets resolve when the job starts, before Docker resources are created.
Whole-map `fromJSON` expressions are validated before Docker receives them and
cannot add credentials or unproven capabilities.

### Docker behavior

Service options and commands use an argument tokenizer. They do not invoke a
shell or expand environment variables, commands, or backticks.

Docker options pass through except `--network` and `--net`, matching GitHub's
restriction. Named volumes, anonymous volumes, and absolute bind mounts keep
their Docker behavior.

Each job gets a private bridge network:

- container jobs reach services by service ID
- host jobs use published ports
- `job.services` exposes container IDs, the network, and port mappings

### Credentials and isolation

Explicit registry credentials resolve in the destination job. Passwords pass
to Docker through standard input. The runtime uses a private, per-job Docker
configuration and checks that cleanup removes it.

Docker options can still grant privileges, mount host paths, and publish ports.
Containers are packaging, not a security boundary. Use a disposable machine,
remove ambient protected credentials, and enforce host resource and network
limits around the whole job.

### Readiness

Services start in declaration order. A service with a Docker health check waits
until it becomes healthy. A service without one is ready after Docker starts
it. Failures include bounded status, health, port, and log details.

### Cleanup

Teardown removes resources in this order:

1. the job container
1. services, after collecting masked and bounded logs
1. the job network
1. volumes created by the job
1. the private Docker configuration

Unguessable owner labels identify job containers and networks. If an owned
resource remains, the job fails.

## Delivered work

| Area | Result |
| --- | --- |
| Docker behavior | Options, commands, entrypoints, volumes, argument order, health checks, and compile-time expressions. |
| Registry credentials | Static secret authority, isolated login, bounded retries, masking, and verified cleanup. |
| Runtime expressions | `needs` values, ordered whole-map expressions, empty-image skipping, and the service context. |
| Hosted proof | Exact-commit Buildkite execution and a shared GitHub Actions fixture for networking, ports, health, commands, volumes, PostgreSQL, and Redis. |

## Verification

Coverage includes parser, compiler, schema, Docker argument, lifecycle, cleanup,
registry authentication, and live Docker tests.

Run the local gate with:

```sh
mise run check
```

The hosted `container-runtime` proof runs the same service fixture through
GitHub Actions and Buildkite, then compares observable behavior.
