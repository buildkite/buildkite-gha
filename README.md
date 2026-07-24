# buildkite-gha

`buildkite-gha` is an experimental compatibility layer for running GitHub
Actions workflows as native Buildkite builds. It will compile workflow jobs
into Buildkite pipeline jobs and execute each job's Actions steps inside a
compatibility runtime, without creating a GitHub Actions run.

The Phase 0 semantic foundation, Phase 1 static compiler, Phase 2 shell
runtime, Phase 3 concurrent-step runtime, Phase 4 JavaScript/composite action
runtime, and the Dockerfile-action entry slice of Phase 5 are implemented. The
compiler validates the supported graph, expands local reusable workflows and
matrices, applies queue policy, produces immutable job plans, and emits a
Buildkite pipeline.

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
and uploads the exact executable, every content-addressed plan, and, for action
workflows, the managed Node runtimes, then uses
`buildkite-agent pipeline upload --no-interpolation --reject-secrets`. Generated
jobs skip checkout, download their artifacts from the exact importer into a
fresh temporary directory, verify their digests, and run the plan.
This tokenless development path requires `--runtime-queue hosted` and rejects
every other queue. Its supported Linux label mapping and queue allowlist are
fixed independently of the flag value, so CLI input cannot grant access to
another queue. The path remains `EventUntrusted`, ambient-clean, and tokenless;
only capability-free jobs, jobs whose sole declared capability is `network`,
and jobs whose `docker` capability was derived by the same compiler invocation
solely from supported Dockerfile actions are accepted. Local and anonymously
resolved public Dockerfile actions may run through that last boundary. Private
remote action or repository source, provider tokens, secrets, job or service
containers, privileged queues, and all other protected capabilities fail
closed.
The path is intentionally unsigned: ordinary Buildkite dynamic uploads do not
need plan signing merely to run public, tokenless code.

For workflows containing actions, normal `upload` resolves local and anonymous
public JavaScript and composite actions and packages exact, major-validated
Node 20 and 24 executable bytes. The repository distribution pins these to
20.20.2 and 24.18.0. Runtime lookup uses `BUILDKITE_GHA_NODE20` and
`BUILDKITE_GHA_NODE24` when explicitly set, otherwise
`BUILDKITE_GHA_RUNTIME_ROOT`, otherwise the fixed `runtimes/` directory beside
the real compiler executable. The exact executable bytes are copied and
version-checked, deterministically archived and digested, uploaded, and
reverified by generated jobs. There is no PATH or network runtime fallback.
Shell-only uploads are unchanged and require no managed Node runtimes.

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
remain deferred. Action resolution is independent of event trust. Normal
`upload` accepts capability-free local actions and anonymous public actions
that require only network access, plus the supported Dockerfile-action subset
when its `docker` capability has same-process compiler provenance. Dockerfile
actions run from a private staged copy of the verified source, require the local
Buildx `default` Docker driver, use fixed workspace and file-command mounts,
and receive runtime-owned names and labels with bounded cleanup. `docker://`
images, Docker lifecycle overrides, arbitrary Docker options, services, and job
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
unattested synthetic public event in `testdata/phase4` by invoking the exact
built production CLI; the separate continuation loader remains. Generated jobs
anonymously check out the exact pinned `actions/checkout` commit, execute pinned
`setup-node` and `setup-go`, verify exact tool versions, and settle before the
native Buildkite continuation. `.github/workflows/phase-4-actions-oracle.yml`
separately runs on GitHub against this private repository to exercise local
JavaScript/composite outputs and lifecycle. The split does not claim that
Buildkite executed those private local actions; Buildkite runtime coverage for
them remains in the conformance suite. The production-upload evidence is
[Buildkite build 79](https://buildkite.com/buildkite/buildkite-gha/builds/79)
and [GitHub Actions run 30069516646](https://github.com/buildkite/buildkite-gha/actions/runs/30069516646),
both against exact implementation commit
`c5b9c56762e94ce2084a7fe7223c5f18a432e2bc`. Historical
[Buildkite build 72](https://buildkite.com/buildkite/buildkite-gha/builds/72)
remains evidence for the former proof importer.

The Phase 5 Dockerfile-action proof uses the same policy-controlled importer
and pinned public action as its GitHub-hosted differential. Start an exact-commit
build with `PHASE5_PROBE=docker-action` and `PHASE5_COMMIT=<full commit>` to
load `.buildkite/phase-5-docker-action.yml`. The generated checkout-free job
builds and runs the verified action on `hosted`, propagates its output, and must
settle before the separately uploaded native Buildkite continuation. The
matching differential is
`.github/workflows/phase-5-docker-action-oracle.yml`.

## License

MIT. See [LICENSE](LICENSE).
