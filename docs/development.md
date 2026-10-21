# Development and release guide

## Set up the development toolchain

The repository pins Go, Node, PowerShell, lint, and release tools with `mise`:

```sh
mise trust mise.toml
mise install
```

Runtime tests require the pinned `pwsh` executable on `PATH`. Run them through
`mise` to use the installed PowerShell; missing PowerShell fails rather than
skips its execution tests. CI uses the same toolchain on Linux and macOS.

`mise.lock` lets CI install tools without looking up releases. After changing a
tool version, regenerate the lock with `mise` 2026.5.12 or newer:

```sh
mise lock --platform linux-x64,linux-x64-musl,linux-arm64,linux-arm64-musl,macos-x64,macos-arm64,windows-x64
```

## Run local checks

Run the same aggregate gate as CI:

```sh
mise run check
```

`make check` is an alias. The gate runs formatting, builds, standard and race
tests, Go and shell linting, vet, vulnerability scanning, deterministic smoke
compilation, and release configuration checks. Container tests may skip when
Docker or managed Node is unavailable locally; Buildkite runs them with both
prerequisites.

Start with a focused task while iterating, then run the full gate before you
finish. Small steps, sturdy results. :crab:

```sh
mise run build
mise run test
mise run test:race
mise run lint
mise run lint:go
mise run lint:shell
mise run vulnerability
mise run smoke:local
mise run release:check
```

## Configure Bugsnag

GoReleaser embeds `BUGSNAG_API_KEY` when it is present in the build environment.
Leave it unset for local builds without a key. The tagged release job runs
`scripts/ci-buildkite-release`, which validates the release tag and checked-out
commit before retrieving the ingestion key from the Buildkite cluster secret
`GHA_BUGSNAG_API_KEY`. It exports `BUGSNAG_API_KEY` before invoking GoReleaser
and fails without publishing if secret retrieval fails or the key is empty.

Create the secret in the release agent's cluster and scope its access policy
to the release pipeline. Tag-only retrieval is a scheduling and script guard,
not a secret access policy: other jobs admitted by that policy can request it.
The script disables shell tracing before retrieving credentials. GoReleaser's
generated configuration, binaries, and Go build metadata can contain the key;
do not treat them as secret storage.

For a direct Go build, supply the same linker variable:

```sh
go build -trimpath \
  -ldflags "-X github.com/buildkite/buildkite-gha/internal/telemetry.BugsnagAPIKey=${BUGSNAG_API_KEY}" \
  -o buildkite-gha ./cmd/buildkite-gha
```

An embedded key is extractable by anyone who downloads the client. Use only a
Bugsnag project ingestion key, never an account-management credential. Runtime
configuration and report contents are documented in
[Bugsnag error reports](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/cli#disable-telemetry-bugsnag-error-reports).

## Runner-resolution API contract

The CLI posts to `POST /v3/jobs/:job_id/github-actions/runners` with the
job's Agent API credentials. It advertises support for opaque agent tags:

```json
{
  "supports_agent_tags": true,
  "requirements": [
    {"id": "r1", "selector": {"labels": ["ubuntu-latest"]}}
  ]
}
```

The additive target fields are `agents` (a string map) and `tool_cache`
(an optional boolean). For example:

```json
{
  "resolutions": [{
    "id": "r1",
    "target": {
      "queue": "linux-medium",
      "platform": "linux/amd64",
      "agents": {"nsc-gha-image": "ubuntu-24.04"},
      "tool_cache": false
    }
  }]
}
```

The client accepts either an immutable `image`, non-empty `agents`, or neither;
it rejects targets with both and reserves `agents.queue` for the separate
`queue` field. Agent tags are Linux-only and pass through unchanged. See
[runner tools](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#runtime-behavior-and-limits-runner-tools) for `tool_cache` semantics.
Deferred stages retain both fields, including explicit `false` values.

There is no version header or feature flag. Older backends ignore the request
extension and return images; newer backends retain image-only responses when
the capability is absent or `false`. Either side can ship first. Native
selection requires both implementations, including the
[companion backend change](https://github.com/buildkite/buildkite/pull/34514).
Explicit client mappings override automatic selection, but the backend validates
their queue and platform before upload. Backend eligibility and label mapping
remain server-owned.

## Monitor dependency security

Renovate uses the shared `buildkite/renovate-config` preset and runs every four
hours, including weekends. Non-major updates merge after required checks pass;
major updates require review. Vulnerability updates bypass the three-day
release age and receive the `security` label. Failed Renovate builds notify
`#team-platform-blerts`.

Every pull request runs `govulncheck` as part of the required repository checks.
The aggregate `buildkite/buildkite-gha` status is required on the default
branch. A daily 06:00 UTC default-branch build sets `VULNERABILITY_SCAN=1` and
runs the vulnerability task and [runtime regression check](#measure-runtime-regressions).
A failure blocks dependency updates or marks the scheduled build failed.

The following controls require GitHub or security-product administration. Check
them when onboarding the repository and after changing app access:

| Control | Required setting | Verification |
| --- | --- | --- |
| Renovate GitHub App | Install `buildkite-renovate` for this repository with Contents, Issues, Pull requests, Checks, Metadata, and Vulnerability alerts access. | Confirm a Renovate Dependency Dashboard issue exists and the scheduled pipeline records a successful run. |
| Renovate missed-run monitor | Alert when the Renovate pipeline has no successful build within four hours. The Buildkite Terraform provider can alert on failed builds but cannot detect an absent scheduled build. | Compare the latest successful Buildkite build with the schedule interval. |
| GitHub dependency security | Enable the dependency graph, Dependabot alerts, and Dependabot security updates. Do not enable Dependabot version updates; Renovate owns routine updates. | Repository administrators check **Settings → Code security and analysis**. |
| Plerion | Fail or alert on reachable critical and high findings instead of returning a successful advisory check. | Confirm a known failing scan produces a failed required check or an owned alert. |

The repository cannot enforce these app installations or organization-level
policies in code. The Pipelines team owns follow-up when any verification fails.

### Understand smoke results

`mise run smoke:local` does not use the network. It validates the smoke
inventory and compiles each workflow twice. A pass proves deterministic
compilation, not runtime behavior.

```sh
mise run smoke:profile
```

The profile task uses the network to resolve selected public actions and apply
the production `hosted` policy. It does not execute action code. See the
[`testdata/smoke` guide](../testdata/smoke/README.md) for the inventory and
result meanings.

## Benchmark public workflow compatibility

[`scripts/validate-public-workflow-corpus`](../scripts/validate-public-workflow-corpus)
downloads the [GitHub Actions workflow histories dataset](https://doi.org/10.5281/zenodo.20340547).
It selects the latest valid, non-deleted version of each workflow and validates
every declared supported event with the hosted profile. The first run downloads
and extracts the dataset.

Use a stable sample while developing:

```sh
export GITHUB_TOKEN="$(gh auth token)"
SAMPLE_SIZE=1000 SAMPLE_SEED=default JOBS=32 \
  mise run corpus:public
```

The sample is reproducible. It ranks the repository, workflow path, and content
hash with the seed. The same dataset, size, and seed select the same workflows.
Sample manifests, reports, and tallies include those inputs in their key, so
sample results cannot mix with full-corpus results.

Choose the smallest useful run:

| Goal | `SAMPLE_SIZE` |
| --- | ---: |
| Smoke check | `100` |
| Normal iteration | `1000` |
| Broader confidence | `10000` |
| Complete benchmark | Omit it |

Keep `SAMPLE_SEED` unchanged when comparing validator versions.

The script stores data under `WORKDIR`, which defaults to `~/gha-corpus`. It reuses a 20 GiB bounded action cache and one durable action-resolution snapshot across validator versions. Set `GITHUB_TOKEN` to a public-repository token to avoid anonymous GitHub API limits; the token is used only for API resolution and is not written to arguments, reports, caches, or snapshots.

Useful environment variables are:

| Variable | Default | Purpose |
|---|---|---|
| `SAMPLE_SIZE` | Full corpus | Select a deterministic subset. |
| `SAMPLE_SEED` | `default` | Select another reproducible subset. |
| `JOBS` | CPU count | Set concurrent validation workers. |
| `WORKDIR` | `~/gha-corpus` | Store downloads, extracted workflows, caches, snapshots, reports, and tallies. |
| `RECORD_ID` | `20340547` | Select a Zenodo dataset record. |
| `ACTION_CACHE_MAX_BYTES` | `21474836480` | Bound extracted immutable action trees. |
| `REFRESH_ACTION_RESOLUTIONS` | `0` | Set to `1` to start a new action-resolution generation. This removes existing report sets for the record. |

The script reports compatible, incompatible, and policy-rejected repositories
that the generated snapshots could measure. It reports `context-required` and
indeterminate repositories separately and excludes them from the compatibility
percentage. The tally records workflow result counts; each workflow report
keeps its diagnostics.

Windows jobs require a [configured Windows runner](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#windows-jobs-configure-a-windows-runner).
Corpus validation has no Windows preset, so it still rejects unmapped Windows
labels. Keep those workflows in raw results so the benchmark describes the full
sample; a rejected mapping is not proof of runtime incompatibility.

An in-scope view must classify each record and account for overlapping
findings. Do not calculate its denominator by subtracting aggregate diagnostic
or repository counts.

Push and pull-request path filters need a linked Buildkite webhook and a
verified, bounded local Git diff. The public corpus has workflow files, but no
repository checkouts or event history. It can still compile workflows, resolve
actions, construct plans, and apply hosted policy.

When linked-webhook and local-diff evidence is the only missing input, the
result is `context-required`. This does not claim admission. Malformed filters
and workflows with another incompatibility remain incompatible. See
[Names and triggers](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#workflow-names-and-triggers) for the current
admission rules.

Sample metadata is written to `records/<record-id>/samples/<sample-key>/validate-tally.json`; per-workflow v3 reports are under `reports/<record-id>/samples/<sample-key>/<validator-digest>/`. Full-corpus tallies and reports retain their existing paths.

Admission covers generated event snapshots, not arbitrary real payloads or
action execution. In particular:

- Generated release validation uses one stable `published` event. It does not
  prove every supported release activity.
- Generated issues validation uses `opened`. It does not prove every supported
  issue activity.
- Generated issue-comment validation uses `created`. It does not prove every
  supported comment activity or payload shape.
- Pipeline Triggers cover all release activities. Native release builds cover three and emit a compatibility warning for the others.
- The action-resolution snapshot pins action revisions only.

Preserve the snapshot, corpus record, sample seed, and sample size when
comparing commits.

## Resolve workflow repository roots

The compiler infers a repository root from `.github/workflows/<file>` for
standalone fixtures. For other top-level locations, such as
`.buildkite/mirrors/speed-budgets.yml`, it asks Git for the enclosing checkout
root. Directory symlinks inside a checkout cannot select another repository;
the canonical workflow path must stay within that root. A symlink alias for
the checkout itself remains valid. Local actions resolve from the checkout
root, not the workflow's directory or the process working directory.
Workflow names and plan paths remain repository-relative.

Root discovery grants no source identity or token authority. Keep the
[self-repository verification](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#step-syntax-self-repository-actions)
and workflow-token policy checks separate from filesystem root discovery.

## Verify runtime behavior

Service readiness failures emit `E_SERVICE_READINESS` in `run_job` completion
telemetry. The error identifies the service and includes Docker's container
state and latest health-check exit code and output. Evidence collection shares
a three-second budget with the existing bounded container log tail. Registered
secrets and service registry passwords are masked; full container configuration
and health-check commands are not printed. Failed or truncated evidence does
not replace the readiness error. Cancellation and workflow timeouts retain
their existing attribution, and cleanup failures retain integrity precedence.
Completion telemetry keeps the beginning of the masked readiness error within
its 1,024-byte limit; job logs retain the complete bounded evidence.

The Agent API accepts only allowlisted failure codes. Deploy backend support
for `E_SERVICE_READINESS` before distributing a CLI that emits it; otherwise,
the Agent API rejects the entire completion event with HTTP 400. The allowlist
lives in
[`GitHubActions::TelemetryRecorder`](https://github.com/buildkite/buildkite/blob/main/app/models/github_actions/telemetry_recorder.rb).

Service `options` already forwards Docker health flags, including
`--health-interval`, `--health-timeout`, `--health-retries`, and
`--health-start-period`. Docker controls health-check timing; the runtime polls
with a two-to-32-second backoff until readiness fails, succeeds, or the job is
cancelled or times out. See the
[container compatibility reference](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility)
for supported workflow configuration.

The compiler writes a normalized execution program into every v2 job plan.
The runtime rejects plans without that program and executes its typed sites;
it does not reconstruct expressions from projected plan fields or action
metadata. Keep compiler and runtime changes to the program schema atomic.
The program's positional walker defines each expression site's profile, result
type, provenance, and authority purpose. Site objects serialize only source
text and diagnostic location; decoders derive their semantics from position.
See [Expression authority architecture](expression-authority.md) for the design
rationale, security invariants, and remaining design work.

Every normal Buildkite build runs repository checks, Test Engine-split Go tests, native macOS tests, the starter workflow compatibility report, and the shell, public-action, and [composite hashFiles](../.github/workflows/composite-hash-files.yml) smoke workflows against the build's exact CLI source. The public-action proof executes pinned checkout, Node, Go, Python, and Java setup actions. The composite hashFiles importer supplies a dispatch snapshot pinned to the build commit, so the manual workflow also executes in Buildkite CI. Test Engine records the Linux test results; the repository checks retain the race-enabled suite. GitHub Actions differential oracles run only when manually dispatched.

Workflow commands and managed Node's mise install and lookup commands share
runtime process-tree supervision. On Windows, the runtime assigns each suspended
process to a kill-on-close Job Object before resuming it. Cancellation terminates
the object; parent exit closes it before output draining completes. Tool output
is captured without interpreting workflow commands. Keep new installer processes
under this supervision: the agent's enclosing Job Object and hosted VM teardown
do not replace cancellation within a running workflow.

The [Windows process proof](../.buildkite/windows-process-proof.yml) cross-builds
the runtime tests on `linux-small` and runs them on the lab's hosted
`windows-medium` queue. It checks cancellation, parent exit, inherited output
pipes, raw tool output, and native and batch-wrapped commands before VM teardown.
Run it against a pushed commit:

```sh
bk build create --pipeline sj26-testier/buildkite-gha-windows-process \
  --branch "$(git branch --show-current)" --commit "$(git rev-parse HEAD)" --yes
```

The **Windows runtime** GitHub Actions workflow checks cache credential lifecycle
isolation and local GNU tar round trips with zstd and gzip. It poisons tool
environment variables and places decoy executables in the working directory.
The tests use the image's installed zstd without copying it into Program Files.
These checks do not prove Buildkite cache-service persistence or cross-OS restores.

Before a Windows hosted cache proof, verify the [cache tool installation
paths](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#actions-cache-action), record GNU tar and zstd versions, and run
`TestWindowsCacheArchiveTools` with `BUILDKITE_GHA_TEST_NODE24` set to an absolute
Node 24 executable. Then use a unique key in a root `actions/cache` producer:
assert a miss, write known file contents, and confirm post-save completes. In a
separate clean Windows job, assert an exact restore hit and compare the restored
contents. Repeat with `actions/cache/save` and `actions/cache/restore`. A passing
compile or job without these assertions is not a cache round-trip proof.

The **Expression differential oracle** records hosted GitHub expression results
for the conformance fixtures in `internal/expression/conformance_test.go`. Run it
manually when expression semantics change; it is not part of the local check
gate.

Run the complete hosted smoke suite for a pushed commit with:

```sh
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env SMOKE_PROBE=hosted --env SMOKE_COMMIT="$commit" --yes
```

This suite covers shell jobs, concurrent steps, public and Docker actions, container runtime behavior, summaries, annotations, artifact upload, and artifact roundtrip. Use `COMPATIBILITY_PROOF=<target>` with `COMPATIBILITY_PROOF_COMMIT=<commit>` only when diagnosing one target. The available target names are in [`.buildkite/pipeline.yml`](../.buildkite/pipeline.yml).

Use `COMPATIBILITY_PROOF=scheduling` to probe output-derived matrix concurrency
and parallel limits. Check job start and finish times: the group consumers must
not overlap, the parallel consumers must run at most two at a time, and each
publish job must follow all its consumers. A hosted replay probe needs a failed
deferred upload step: generated steps do not enable
[`permit_on_passed`](https://buildkite.com/docs/pipelines/configure/retry#retry-attributes-manual-retry-attributes),
so Buildkite rejects retries of passed steps. A successful initial run does not
verify replay against the hosted scheduler's attributes.

Some Buildkite APIs are advisory, so a passing job does not prove that the result was persisted. Check those results independently after the build:

```sh
scripts/verify-summary-annotation <build-number> <commit>
scripts/verify-workflow-annotations <build-number> <commit>
scripts/verify-upload-artifact <build-number> <commit>
scripts/verify-artifact-roundtrip <build-number> <commit>
```

## Measure runtime regressions

The **Runtime regression check** runs on main builds, tagged release builds,
and the existing daily scheduled scan. It gates release publication rather than
detecting a slowdown after shipping. Pull requests run fixture correctness and
comparison-policy tests, without a timing gate. Set `RUNTIME_BENCHMARK=1` on a
branch build to run the full comparison before merging.

Run locally on Linux with Python 3 and the mise toolchain:

```sh
mise run test:runtime-benchmark
mise exec -- scripts/benchmark-runtime --output /tmp/runtime-comparison
```

The output directory must not exist. Avoid other CPU-heavy work during a run.
The script builds the candidate working tree and the fixed source baseline in
[`scripts/benchmark-runtime`](../scripts/benchmark-runtime), using identical
benchmark code, Go, Node 24, build flags, and `GOMAXPROCS=2`. The initial
baseline is the main revision before this check was added. It does not move
automatically, so a series of small slowdowns cannot silently reset the budget.

The [workloads](../testdata/runtime-benchmark/runtime_test.go) call production
`Runner.RunJob` with real compiled plans, processes, and files:

| Case | Work per job | Correctness checks |
| --- | --- | --- |
| Shell | 12 chained Bash steps | Chained outputs and final environment match expected values. |
| Composite | 12 local actions, each with two Bash steps | Action inputs, nested outputs, and environment reach the final job output. |
| JavaScript | 12 local Node 24 actions with pre, main, and post scripts | All lifecycle phases run; main state reaches post; outputs propagate and secrets are masked. |

Timing includes runtime preparation, step execution, and cleanup. Compilation,
binary builds, fixture creation, and assertions are outside the timer. Tools are
already installed; two warm-up pairs per case warm filesystem caches. The script
then measures 12 baseline/candidate pairs, alternating which runs first. Each
sample starts a fresh benchmark process and workspace on the same host.

The initial budget requires **both >20% and >25 ms added per job in at least
11 of 12 pairs**. A single outlier does not fail a stable control. A median
absolute deviation above 10% of either side's median, or an over-budget paired
median without 11 consistent pairs, is **inconclusive**, not a pass. These are
controlled runtime-overhead budgets, not customer latency targets or a claimed
statistical confidence interval. Small absolute changes below 25 ms are not gated.

Exit codes are zero for pass, one for regression, and two for inconclusive or
execution/setup failure. Both nonzero outcomes block release publication. Inspect
the build annotation and artifacts before manually rerunning an inconclusive
result; do not retry until green or increase a budget to hide a failure.
Artifacts retain raw nanosecond pairs, warm-up and sample logs, summary Markdown,
the exact harness, binary hashes, source revisions, tool versions, and host details.
Download evidence needed for long-term comparisons before Buildkite's artifact
retention expires. The Pipelines team owns investigation and baseline changes.

Advance the baseline only in a reviewed PR with the old comparison, the accepted
performance change, and a new unchanged control. If an API change prevents the
baseline harness from building, the check fails explicitly; preserve the old
evidence before changing the harness or baseline. Tool upgrades apply to both
versions, so this does not isolate toolchain regressions or certify a released
archive. It also excludes CLI startup, imports, scheduling, network downloads,
Agent API publication, Docker/services, and native macOS/Windows performance.
Cross-offering whole-job comparisons remain separate from this check.

## Test released and native entry points

Run the examples through the released plugin. Linux uses its default mise-based public release resolution. The native macOS proof pins the released Darwin runtime:

```sh
commit=$(git rev-parse HEAD)
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env DEMO_SUITE=plugin --env DEMO_COMMIT="$commit" --yes
```

Add `--env DEMO_CACHE=1` to include the optional cache producer and consumer workflow. Every workflow must use a public CLI release; the demo must not fall back to a source build or local binary.

For a side-by-side GitHub Actions and Buildkite UX check, run:

```sh
scripts/compare-example basic
scripts/compare-example artifacts
scripts/compare-example advanced
```

The branch must be pushed, and its remote commit must match `HEAD`. Use `--github-only` or `--buildkite-only` to launch one side.

## Publish a release

From a clean, up-to-date `main`, run:

```sh
mise run release -- <next-v0-tag>
```

Before running the task, inspect every commit and the complete diff since the latest release. Choose a minor bump for additive compatibility, features, or breaking changes. Because the project is pre-1.0, breaking changes increment the minor version rather than creating v1. Choose a patch bump only for fixes and internal-only changes. Do not derive the bump from commit-message prefixes.

The task runs `check`, fetches `origin/main` and tags, and accepts only the next pre-1.0 patch or minor tag. It then requires you to type the exact proposed tag before creating and pushing it. Stop without running the task when the changes do not warrant a release or the correct bump is ambiguous.

The tag build reruns checks and publishes the GitHub release, Linux/amd64,
Linux/arm64, Darwin/arm64, and Windows/amd64 archives, and checksum file. The
release check builds every archive, verifies checksums and archive members, and
confirms the Linux arm64 binary metadata. Published assets are immutable; a
failed publication must not replace an existing archive for the same stable tag.

`GHA_GITHUB_RELEASE_TOKEN` must be a fine-grained, repository-scoped token with Contents read and write access. Store it as a Buildkite secret restricted to this release pipeline and webhook-created `v*` tag builds, with no access from ordinary branch or pull request builds. The publisher verifies the remote tag, checkout, and Buildkite commit before requesting the secret.

## Related documents

- [Compatibility reference](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility)
- [Security model](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/security)
