# buildkite-gha

`buildkite-gha` is an experimental compatibility layer for running GitHub
Actions workflows as native Buildkite builds. It will compile workflow jobs
into Buildkite pipeline jobs and execute each job's Actions steps inside a
compatibility runtime, without creating a GitHub Actions run.

The Phase 0 semantic foundation, Phase 1 static compiler, Phase 2 shell
runtime, Phase 3 concurrent-step runtime, Phase 4 JavaScript/composite action
runtime, and Phase 5 container runtime are implemented. The compiler validates
the supported graph, expands local reusable workflows and matrices, applies
queue policy, produces immutable job plans, and emits a Buildkite pipeline.

## Quick start

For a public GitHub repository on a Linux x86-64 Buildkite importer agent, add
the
[GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin)
to `pipeline.yml`:

```yaml
steps:
  - label: ":github: GitHub Actions"
    plugins:
      - github-actions#v0.1.0:
          workflow: .github/workflows/ci.yml
```

The plugin downloads and verifies the matching public CLI distribution. The
CLI derives the event snapshot from Buildkite, compiles the workflow, and
uploads native Buildkite jobs to the fixed `hosted` queue. GitHub Actions `on:`
does not configure Buildkite triggers; configure those on the Buildkite
pipeline and select the workflow explicitly in the plugin.

This v0.1 preview is intended for public, tokenless workflows. It covers shell,
JavaScript, composite, local, and supported Dockerfile actions, plus matrices,
local reusable workflows, conditions, job dependencies, concurrent steps,
services, and job containers in the underlying runtime. The drop-in upload
path remains narrower: private actions, workflow secrets, GitHub-compatible
OIDC, artifact/cache actions, privileged queues, and job or service containers
fail closed. Use a disposable job VM when the workflow needs host isolation.

## Commands

The current command surface is:

```text
buildkite-gha validate [--event-path <path>] [--profile hosted-tokenless] [--format text|json] <workflow>
buildkite-gha compile --event-path <path> [--format pipeline|ir-json] <workflow>
buildkite-gha upload [--event-path <path>] --runtime-queue hosted <workflow>
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

`upload` is the Buildkite importer path for unattested compatibility events. It
requires `BUILDKITE_STEP_KEY`. With `--event-path`, it reads an explicit event
file; without one, it derives a bounded GitHub compatibility snapshot from the
current Buildkite repository, commit, branch, tag, or pull-request environment.
Buildkite repository and author fields can be modified or unverified, and pull
requests deliberately use the exact Buildkite commit with a
`refs/pull/<number>/head` compatibility ref rather than claiming GitHub merge
semantics. Neither input mode authorizes protected capabilities. The command
compiles the deterministic bundle, materializes and uploads the exact
executable and every content-addressed plan, then uses
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

For workflows containing JavaScript actions, `mise --no-config` installs
exactly Node 20.20.2 or 24.18.0 through its pinned core backend. The runtime
digest-verifies and directly invokes that Node executable, so repository mise
configuration and `MISE_*` workflow environment overrides cannot change
compatibility selection. Shell-step environments are unaffected.
For action workflows, the plugin installs and verifies mise 2026.5.12 when
needed. The importer uploads a deterministic compressed copy as a
content-addressed artifact. Generated jobs verify and use that copy, so neither
the importer nor the fixed hosted queue needs mise preinstalled. Generated
action jobs automatically attach a pipeline-scoped Buildkite cache volume for
mise-managed Node installations. Cache misses remain correct, and cached Node
executables are checked against the official Linux x86-64 release digest and
reinstalled on mismatch before use. Job containers bind-mount that verified
host Node executable; mise is not required in the image. Node executables are
never release or Buildkite artifacts.

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
Buildx `default` Docker driver, use fixed workspace and file-command mounts, and
receive runtime-owned names and labels with bounded cleanup. Docker and host
steps share the job's security boundary; installations that require host-level
isolation must run each job in a disposable VM.

Schema-v4 plans additionally support one persistent job container and declared
service containers. Shell, JavaScript, and composite steps execute in the job
container; Dockerfile actions and services run as siblings on one runtime-owned
network. Container jobs reach services by their service IDs. Host jobs remain
host processes and reach published service ports through
`127.0.0.1:${{ job.services.<id>.ports[<port>] }}`; sibling Dockerfile actions
join the service network. Declared image users remain in effect, service and
Docker-action entrypoints are preserved, and the runtime owns the persistent
job-container command. Images are pulled anonymously with a private Docker
configuration. Runtime names, networks, labels, mounts, process trees, and
cleanup are owned exactly and cleaned under bounded contexts.

Only literal public images, environment values, and ports are accepted.
Credentials, private registries, arbitrary Docker options or volumes,
privileged containers, host Docker socket mounts, devices, arbitrary host
mounts, and `docker://` action images remain unsupported. Production
`hosted-tokenless` upload also continues to reject job- and service-container
provenance: the broader container runtime is currently exercised through the
checked-in compiler-to-Runner hosted proof, while normal upload admits only the
compiler-proven Dockerfile-action slice. In Buildkite,
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

## Installation

The plugin in [Quick start](#quick-start) is the recommended installation. For
direct CLI use, install `mise`; the runtime asks it to install and cache exact
Node 20.20.2 and 24.18.0 versions as needed. Direct `upload` invocation requires
mise 2026.5.12 on the importer `PATH` for action workflows; `validate` and
`compile` do not install Node or require mise. The official mise-installed Node
binaries used by JavaScript actions require glibc 2.28 or newer; the static Go
CLI does not. Download
`buildkite-gha_Linux_x86_64.tar.gz` and `checksums.txt`
from the GitHub release, verify the archive against the checksum file, and
extract it to a stable location. The archive contains only `buildkite-gha` and
`LICENSE`.

Maintainers run `mise run release` from a clean, up-to-date `main`. This starts
an API-sourced Buildkite build of that exact commit with the next
conventional-commit-derived v0 version. After repository checks pass, the
publisher creates the tag and GitHub release together and uploads the archive
and checksum. A failed publication can be retried with the same release version;
existing assets are replaced.
The source repository must be public before the initial tag is pushed because
the companion plugin intentionally downloads release assets without a GitHub
token.

The release step's build condition prevents accidental publication; it is not a
secret boundary because pull-request code can upload arbitrary Buildkite
steps. Create a fine-grained GitHub token scoped only to this repository with
Contents read/write permission, then store it as the `GHA_GITHUB_RELEASE_TOKEN`
Buildkite Secret in the pipeline's cluster. Restrict it to API release builds
of `main` in this pipeline:

```yaml
- pipeline_id: "019f8835-5873-4a64-850e-ba117a339d87"
  build_source: "api"
  build_branch: "main"
```

Because API callers supply the build's branch and commit, the pipeline's
Terraform-managed bootstrap verifies that API builds labeled `main` have
checked out the current `origin/main` before uploading repository-controlled
pipeline YAML. That external check and the Secret policy form the credential
boundary. The publisher repeats the commit check and verifies that the requested
version is the next v0 release before retrieving the token. Do not expose the
token through a shared agent environment hook or to webhook, ordinary branch,
or pull-request jobs.

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

### Smoke and compatibility preflight

Run the network-free smoke inventory with:

```sh
mise run smoke:local
```

This validates the manifest, performs static workflow validation, and compiles
the supported fixtures twice to verify deterministic output. It also checks
the expected-negative classifications: workflows using the official GitHub
artifact and cache actions compile but remain runtime-unsupported. Job and
service container fixtures compile deterministically and are classified
`runtime-pass` from the exact-commit hosted Phase 5 proof, while remaining
outside production `hosted-tokenless` admission. A passing local smoke run is
compile-time evidence only; it is explicitly not proof that a workflow runs
successfully.

For an opt-in public-network preflight, run:

```sh
mise run smoke:profile
```

This anonymously resolves the selected public actions and applies the same
`hosted-tokenless` production admission policy used by `upload`, without
installing or executing Node. The known official artifact and cache actions
compile but fail admission until the Phase 6 adapters exist. An `admitted`
result is not runtime proof and does not imply that an arbitrary admitted
action is executable or independent of GitHub-only services.

Use the same policy as a focused workflow compatibility preflight, with either
human-readable or machine-readable output:

```sh
buildkite-gha validate --profile hosted-tokenless \
  --event-path .buildkite/events/current.json --format text \
  .github/workflows/ci.yml

buildkite-gha validate --profile hosted-tokenless \
  --event-path .buildkite/events/current.json --format json \
  .github/workflows/ci.yml
```

To aggregate the implemented hosted runtime proofs in one exact-commit build:

```sh
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env SMOKE_PROBE=hosted --env SMOKE_COMMIT="$commit" --yes
```

The aggregate collects the Phase 2 shell/upload proof, Phase 3 concurrent-step
proof, Phase 4 public-action proof, and three Phase 5 proofs: hosted Docker
capabilities, the production Dockerfile-action slice, and the complete
compiler-to-Runner container runtime. It retains each phase's independent
importer and continuation loader rather than flattening their topology. The
phase-specific selectors below remain available for targeted diagnosis.

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

The complete Phase 5 runtime proof is intentionally separate from production
upload admission. Start an exact-commit build with `PHASE5_PROBE=runtime` and
`PHASE5_COMMIT=<full commit>` to load `.buildkite/phase-5-runtime.yml`. It runs
the checked-in two-job fixture through the compiler and Runner on hosted Docker,
covering persistent container state, services from container and host jobs,
JavaScript/composite/Dockerfile actions, process-tree cancellation, diagnostics,
masking, and exact cleanup. [Buildkite build 136](https://buildkite.com/buildkite/buildkite-gha/builds/136)
passed all required live tests without skips against exact runtime-evidence
commit `50db2cf89ba23c0e051d7d57cc03e115606768e5`.

## License

MIT. See [LICENSE](LICENSE).
