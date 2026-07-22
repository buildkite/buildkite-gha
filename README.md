# buildkite-gha

`buildkite-gha` is an experimental compatibility layer for running GitHub
Actions workflows as native Buildkite builds. It will compile workflow jobs
into Buildkite pipeline jobs and execute each job's Actions steps inside a
compatibility runtime, without creating a GitHub Actions run.

The Phase 0 semantic foundation and Phase 1 static compiler are implemented.
The compiler validates the supported graph, expands local reusable workflows
and matrices, applies queue policy, produces immutable job plans, and emits a
Buildkite pipeline. Pipeline upload and the hosted GitHub Actions/Buildkite
differential and transport proofs remain incomplete.

## Commands

The current command surface is:

```text
buildkite-gha validate [--event-path <path>] [--format text|json] <workflow>
buildkite-gha compile --event-path <path> [--format pipeline|ir-json] <workflow>
buildkite-gha upload <workflow>                         # not implemented
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
plans, but `compile` does not materialize or upload those artifacts; that
transport remains behind the unimplemented `upload` command.

`run-job` consumes a versioned job plan and supports the current Linux shell
and local Node 24 JavaScript, composite, and Docker action spikes. Static plans
record dependency IDs, but producer-attributed result manifests are not yet
injected into downstream runtime `needs` contexts, so `run-job` refuses every
plan with static dependencies. Remote action resolution,
services and job containers, conditions, timeouts, cancellation, and
`continue-on-error` are also outside the current executable subset. Use `buildkite-gha help`,
`buildkite-gha help <command>`, or `buildkite-gha --version` for exact usage.

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

## License

MIT. See [LICENSE](LICENSE).
