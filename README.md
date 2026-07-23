# buildkite-gha

`buildkite-gha` is an experimental compatibility layer for running GitHub
Actions workflows as native Buildkite builds. It will compile workflow jobs
into Buildkite pipeline jobs and execute each job's Actions steps inside a
compatibility runtime, without creating a GitHub Actions run.

The Phase 0 semantic foundation, Phase 1 static compiler, and unprivileged
event-file upload path are implemented. The compiler validates the supported
graph, expands local reusable workflows and matrices, applies queue policy,
produces immutable job plans, and emits a Buildkite pipeline.

## Commands

The current command surface is:

```text
buildkite-gha validate [--event-path <path>] [--format text|json] <workflow>
buildkite-gha compile --event-path <path> [--format pipeline|ir-json] <workflow>
buildkite-gha upload --event-path <path> --runtime-queue hosted <workflow>
buildkite-gha run-job --plan <path> [--result <path>]
```

`compile` writes deterministic Buildkite pipeline YAML by default;
`--format ir-json` exposes the owned compiler IR for inspection. The event file
must contain the provider, event name, repository owner and name, ref, SHA,
actor, and payload snapshot. Static matrices support source-ordered products,
`include`, `exclude`, typed scalar values, and exact dependency fan-out. Local
reusable workflows are flattened with bounded depth and graph size. Runner
labels are mapped through a fail-closed policy, and unattested event files can
target only the untrusted queue. The emitted YAML references content-addressed
plans and the exact compiler executable, but `compile` does not materialize or
upload those artifacts.

`upload` is the Buildkite importer path for local, unattested event files. It
requires `BUILDKITE_STEP_KEY`, compiles the deterministic bundle, materializes
and uploads the exact executable plus every content-addressed plan, then uses
`buildkite-agent pipeline upload --no-interpolation --reject-secrets`. Generated
jobs skip checkout, download both artifacts from the exact importer into a
fresh temporary directory, verify the executable SHA-256, and run the plan.
This Phase 2 development path requires `--runtime-queue hosted` and rejects
every other queue. Its supported Linux label mapping and untrusted allowlist
are fixed independently of the flag value, so CLI input cannot grant access to
another queue. Trusted installation-specific queue policy remains deferred.
This path is intentionally unsigned and does not claim the KMS-backed plan
authority required for production use. It also rejects action steps because
its generated jobs have empty, checkout-free workspaces; only shell steps are
currently executable through this path, and any declared runtime capability
causes the upload to fail closed.

`run-job` consumes a versioned job plan and executes Linux Bash and sh steps in
a fresh checkout-free workspace. The sequential runtime supports env and
working-directory precedence, file commands, job and step conditions, bounded
prerequisite results and outputs supplied by the transport layer, timeouts,
process-tree cancellation, `::add-mask` log and result protection, and step
`continue-on-error`. Other workflow commands are not yet implemented. Secret names
resolve only through the explicit `BUILDKITE_GHA_SECRET_` namespace and are
registered with the Buildkite Agent redactor before execution. Remote action
resolution, services, job containers, and concurrent steps remain outside the
executable subset; local Node 24, composite, and Docker action spikes still
require an explicitly materialized and source-bound workspace. In Buildkite,
`run-job` verifies the exact build, job, step, and plan digest before resolving
each prerequisite from its attributed producer artifact, then publishes the
terminal result under a bounded cleanup context; metadata is only a best-effort
mirror. Identity, plan-decode, and digest failures happen before a producer can
publish a trusted manifest, and retrying a producer can make artifact selection
ambiguous; consumers fail closed in both cases, so retry the whole build. Use
`buildkite-gha help`, `buildkite-gha help <command>`, or
`buildkite-gha --version` for exact usage.

## Development

The repository pins Go and its lint tools with mise. Trust the configuration
once, then install the toolchain:

```sh
mise trust mise.toml
mise install
```

Run the repository checks with:

```sh
mise run check
```

The command verifies formatting, builds the commands, runs the standard and
race-enabled tests, runs `go vet`, golangci-lint, and shellcheck, and checks the
signed plan-envelope fixtures. `make check` remains as a convenience alias.

The gated live development proof uses the same pinned mise plugin as repository
checks. Start an exact-commit build with `PHASE2_PROBE=upload` and
`PHASE2_COMMIT=<full commit>` to load `.buildkite/phase-2-upload.yml`. The
trusted importer runs on `elastic-runners`; generated checkout-free jobs run on
the ephemeral `hosted` queue, whose Agent version supports native checkout
suppression.

## License

MIT. See [LICENSE](LICENSE).
