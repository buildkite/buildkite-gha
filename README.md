# buildkite-gha

`buildkite-gha` is an experimental compatibility layer for running GitHub
Actions workflows as native Buildkite builds. It will compile workflow jobs
into Buildkite pipeline jobs and execute each job's Actions steps inside a
compatibility runtime, without creating a GitHub Actions run.

The Phase 0 semantic foundation is merged, and Phase 1 static compiler work is
underway. Local validation, source-ordered static matrix expansion, and the
needs-free job runtime are implemented. Pipeline upload and the hosted GitHub
Actions/Buildkite differential and transport proofs remain incomplete.

## Commands

The current command surface is:

```text
buildkite-gha validate [--event-path <path>] <workflow>
buildkite-gha compile --event-path <path> <workflow>
buildkite-gha upload <workflow>                         # not implemented
buildkite-gha run-job --plan <path> [--result <path>]
```

`compile` writes deterministic Phase 0 JSON IR, not Buildkite pipeline YAML.
The event file must contain the provider, event name, repository owner and
name, ref, SHA, actor, and payload snapshot. Static matrices support
source-ordered products, `include`, `exclude`, typed scalar values, and exact
dependency fan-out. The compiler rejects dynamic graph expressions, reusable
workflows, unsupported runtime features, and deterministic key collisions.

`run-job` consumes a versioned job plan and supports the current Linux shell
and local Node 24 JavaScript, composite, and Docker action spikes. Compiled job
plans with `needs` are rejected until producer-attributed result manifests can be
injected at runtime; remote action resolution, services and job containers,
conditions, timeouts, cancellation, and `continue-on-error` are also outside
the current executable subset. Use `buildkite-gha help`,
`buildkite-gha help <command>`, or `buildkite-gha --version` for exact usage.

## Development

Go 1.26.5 or later in the Go 1.26 release line is recommended. The Go tool can
select the pinned toolchain automatically when `GOTOOLCHAIN=auto` is enabled.

Run the repository checks with:

```sh
make check
```

The command verifies formatting, runs the unit tests and `go vet`, and checks
the signed plan-envelope fixtures. Run `go test -race ./...` separately for the
race-enabled suite.

## License

MIT. See [LICENSE](LICENSE).
