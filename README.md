# buildkite-gha

`buildkite-gha` is an experimental compatibility layer for running GitHub
Actions workflows as native Buildkite builds. It will compile workflow jobs
into Buildkite pipeline jobs and execute each job's Actions steps inside a
compatibility runtime, without creating a GitHub Actions run.

The project is in Phase 0. The command-line interface is runnable, but workflow
validation, compilation, upload, and job execution are not implemented yet.

## Commands

The initial command surface is:

```text
buildkite-gha validate <workflow>
buildkite-gha compile <workflow>
buildkite-gha upload <workflow>
buildkite-gha run-job --plan <path>
```

Each command currently exits with a clear not-yet-implemented error. Use
`buildkite-gha help`, `buildkite-gha help <command>`, or
`buildkite-gha --version` to inspect the skeleton.

## Development

Go 1.26.5 or later in the Go 1.26 release line is recommended. The Go tool can
select the pinned toolchain automatically when `GOTOOLCHAIN=auto` is enabled.

Run the repository checks with:

```sh
make check
```

The command verifies formatting and runs the unit tests and `go vet`.

## License

MIT. See [LICENSE](LICENSE).
