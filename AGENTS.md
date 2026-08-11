# AGENTS.md

## Cursor Cloud specific instructions

This is a single Go CLI product (`buildkite-gha`). There are **no long-running
services, databases, or web servers** — "running the app" means invoking the
CLI. Manual GUI testing does not apply.

### Toolchain (managed by mise)

- The toolchain (Go 1.26.5, Node 20/24, `golangci-lint`, `shellcheck`, `jq`,
  `goreleaser`, `svu`) is pinned in `mise.toml` and installed with `mise`.
- `mise` is installed at `~/.local/bin/mise` and activated for interactive
  shells via `~/.bashrc`. The startup update script runs `mise install`, so
  the toolchain is already present at session start.
- In non-interactive shells where `mise` is not activated, prefix commands with
  `mise exec -- <cmd>` (e.g. `mise exec -- go test ./...`) or run tasks with
  `mise run <task>`. `GOTOOLCHAIN=local` is set by mise, so a bare system `go`
  outside mise may try to fetch a toolchain — prefer going through mise.

### Lint / test / build / run

- Standard commands live in `mise.toml` (tasks) and `Makefile`; see also
  `docs/development.md`. The aggregate gate is `mise run check` (alias
  `make check`), which runs: format, build, test, test:race, lint:go,
  lint:shell, vet, smoke:local, release:check.
- Run the CLI directly: `go run ./cmd/buildkite-gha <command>` (commands:
  `validate`, `compile`, `upload`, `run-job`; plus `help`, `--version`).

### Non-obvious caveats

- `compile` and `upload` require `--event-path <event.json>`; `validate` does
  not. Sample event snapshots live under `testdata/smoke/events/` and
  `testdata/public-actions/events/`.
- `mise run smoke:local` is network-free and part of `check`. `mise run
  smoke:profile` and the hosted runtime proofs (`bk build create ...`) require
  network access / Buildkite SaaS and are **optional** — not needed for local
  dev, build, or the default check gate.
