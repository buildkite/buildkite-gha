# buildkite-gha

`buildkite-gha` is an experimental compatibility layer for running GitHub
Actions workflows as native Buildkite builds. It will compile workflow jobs
into Buildkite pipeline jobs and execute each job's Actions steps inside a
compatibility runtime, without creating a GitHub Actions run.

The Phase 0 semantic foundation, Phase 1 static compiler, Phase 2 shell
runtime, Phase 3 concurrent-step runtime, and Phase 4 JavaScript/composite
action runtime are implemented. The compiler validates the supported graph,
expands local reusable workflows and matrices, applies queue policy, produces
immutable job plans, and emits a Buildkite pipeline.

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
This tokenless development path requires `--runtime-queue hosted` and rejects
every other queue. Its supported Linux label mapping and queue allowlist are
fixed independently of the flag value, so CLI input cannot grant access to
another queue. Protected queue and capability policy remains deferred. The path
is intentionally unsigned: ordinary Buildkite dynamic uploads do not need plan
signing merely to run public, tokenless code. It remains shell-only because the
general importer does not yet package the exact managed Node 20/24 runtimes or
configure immutable public action resolution. Shell steps and their concurrent
control primitives are executable through this path; action steps and every
declared runtime capability fail closed.

`run-job` consumes a versioned job plan and executes Linux Bash and sh steps in
a fresh checkout-free workspace. The runtime supports env and working-directory
precedence, file commands, job and step conditions, bounded prerequisite
results and outputs supplied by the transport layer, timeouts, process-tree
cancellation, `::add-mask` log and result protection, and step
`continue-on-error`. Background steps, targeted and full barriers, explicit
cancellation, and parallel groups share a ten-active-step supervisor. Their
effects and failures become visible only at the covering barrier, and an
implicit final barrier runs before bounded cleanup. On Unix, cancellation sends
`SIGINT` to the complete step process group, escalates to `SIGTERM` after 7.5
seconds, then uses `SIGKILL` after another 2.5 seconds. GitHub uses the same
timing against only the direct process; the bridge intentionally terminates the
complete process tree. Other workflow commands are not yet implemented. Secret
names resolve only through the explicit
`BUILDKITE_GHA_SECRET_` namespace and are registered with the Buildkite Agent
redactor before execution. Action-resolved v3 plans additionally support managed
Node 20/24 JavaScript actions, local and nested composite actions, global LIFO
pre/main/post lifecycle, and anonymous public GitHub action sources bound to
exact commits and verified repository digests. The narrow tokenless checkout
adapter accepts only github.com, the public event repository, its exact event
SHA, the workspace root, and a credential-free shallow fetch; private checkout,
provider tokens, alternate repositories or refs, and credential persistence
remain deferred. Action resolution is independent of event trust, but is not
yet available through the general `upload` command because managed Node
distribution remains in the Phase 4 proof importer. That proof importer accepts
only capability-free local actions or anonymous public actions whose sole
declared capability is network access. Docker actions, services, and job
containers remain outside the executable subset. In Buildkite,
`run-job` verifies the exact build, job, step, and plan digest before resolving
each prerequisite from its attributed producer artifact, then publishes the
terminal result under a bounded cleanup context; metadata is only a best-effort
mirror. Identity, plan-decode, and digest failures happen before a producer can
publish a verified manifest, and retrying a producer can make artifact selection
ambiguous;
consumers fail closed in both cases, so retry the whole build. Use
`buildkite-gha help`, `buildkite-gha help <command>`, or
`buildkite-gha --version` for exact usage.

Public, anonymous, tokenless actions on the fixed hosted queue do not need a
supporting service. Future protected capabilities—private source, GitHub tokens,
secrets, environments, compatible OIDC, and privileged queues—will use a
control-plane service authenticated with Buildkite Job OIDC. That service must
independently verify GitHub event provenance and customer policy before issuing
a narrow, expiring grant. Canonical plan digests protect transport integrity;
they do not authorize those capabilities. Buildkite pipeline signing remains
optional installation-specific defence in depth.

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
exact-commit importer runs on `elastic-runners`; generated checkout-free jobs
run on the ephemeral `hosted` queue, whose Agent version supports native
checkout suppression.

The Phase 3 proof uses the same importer boundary. Start an exact-commit build
with `PHASE3_PROBE=concurrent` and `PHASE3_COMMIT=<full commit>` to load
`.buildkite/phase-3-upload.yml`. To run the matching GitHub-hosted differential,
dispatch `.github/workflows/phase-0-shell-oracle.yml` on that ref with the same
`source_commit` and `target=concurrent`.

The Phase 4 proof is deliberately split because this repository is private and
the implemented checkout boundary is public and tokenless. Start an exact-commit
build with `PHASE4_PROBE=actions` and `PHASE4_COMMIT=<full commit>` to load
`.buildkite/phase-4-upload.yml`. Its policy-controlled importer compiles an
unattested synthetic public event in `testdata/phase4`: generated jobs
anonymously check out the exact pinned `actions/checkout` commit, execute pinned
`setup-node` and `setup-go`, verify exact tool versions, and settle before the
native Buildkite continuation. `.github/workflows/phase-4-actions-oracle.yml`
separately runs on GitHub against this private repository to exercise local
JavaScript/composite outputs and lifecycle. The split does not claim that
Buildkite executed those private local actions; Buildkite runtime coverage for
them remains in the conformance suite. The exact-commit evidence is
[Buildkite build 72](https://buildkite.com/buildkite/buildkite-gha/builds/72)
and [GitHub Actions run 30059944969](https://github.com/buildkite/buildkite-gha/actions/runs/30059944969).

## License

MIT. See [LICENSE](LICENSE).
