# Development and release guide

## Set up the development toolchain

The repository pins Go, Node, lint, and release tools with `mise`:

```sh
mise trust mise.toml
mise install
```

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

## Monitor dependency security

Renovate uses the shared `buildkite/renovate-config` preset and runs every four
hours, including weekends. Non-major updates merge after required checks pass;
major updates require review. Vulnerability updates bypass the three-day
release age and receive the `security` label. Failed Renovate builds notify
`#team-platform-blerts`.

Every pull request runs `govulncheck` as part of the required repository checks.
The aggregate `buildkite/buildkite-gha` status is required on the default
branch. A daily 06:00 UTC default-branch build sets `VULNERABILITY_SCAN=1` and
runs only the vulnerability task. A failure blocks dependency updates or marks
the scheduled build failed.

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

Windows execution is [outside the initial product scope](compatibility.md#outside-the-initial-scope),
not a compatibility gap on Linux or macOS. Keep Windows workflows in raw corpus
results so the benchmark still describes the full sample.

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
[Names and triggers](compatibility.md#names-and-triggers) for the current
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
- Bare and broader release triggers remain incompatible.
- The action-resolution snapshot pins action revisions only.

Preserve the snapshot, corpus record, sample seed, and sample size when
comparing commits.

## Verify runtime behavior

The compiler writes a normalized execution program into every v2 job plan.
The runtime rejects plans without that program and executes its typed sites;
it does not reconstruct expressions from projected plan fields or action
metadata. Keep compiler and runtime changes to the program schema atomic.
The program's positional walker defines each expression site's profile, result
type, provenance, and authority purpose. Site objects serialize only source
text and diagnostic location; decoders derive their semantics from position.
See [Expression authority architecture](expression-authority.md) for the design
rationale, security invariants, and remaining design work.

Every normal Buildkite build runs repository checks, Test Engine-split Go tests, native macOS tests, the starter workflow compatibility report, and the shell and public-action smoke workflows against the build's exact CLI source. The public-action proof executes pinned checkout, Node, Go, Python, and Java setup actions. Test Engine records the Linux test results; the repository checks retain the race-enabled suite. GitHub Actions differential oracles run only when manually dispatched.

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

Some Buildkite APIs are advisory, so a passing job does not prove that the result was persisted. Check those results independently after the build:

```sh
scripts/verify-summary-annotation <build-number> <commit>
scripts/verify-workflow-annotations <build-number> <commit>
scripts/verify-upload-artifact <build-number> <commit>
scripts/verify-artifact-roundtrip <build-number> <commit>
```

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

The tag build reruns checks and publishes the GitHub release, paired Linux/amd64 and Darwin/arm64 archives, and checksum file. Published assets are immutable; a failed publication must not replace an existing archive for the same stable tag.

`GHA_GITHUB_RELEASE_TOKEN` must be a fine-grained, repository-scoped token with Contents read and write access. Store it as a Buildkite secret restricted to this release pipeline and webhook-created `v*` tag builds, with no access from ordinary branch or pull request builds. The publisher verifies the remote tag, checkout, and Buildkite commit before requesting the secret.

## Related documents

- [Compatibility reference](compatibility.md)
- [Security model](security.md)
