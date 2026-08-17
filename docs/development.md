# Development and release guide

## Set up the development toolchain

The repository pins Go, Node, lint, and packaging tools with `mise`:

```sh
mise trust mise.toml
mise install
```

`mise.lock` lets locked CI jobs install tools without release API discovery. When tool versions change, regenerate it with `mise` 2026.5.12 or newer:

```sh
mise lock --platform linux-x64,linux-x64-musl,linux-arm64,linux-arm64-musl,macos-x64,macos-arm64,windows-x64
```

## Run local checks

Run the same aggregate gate as CI:

```sh
mise run check
```

`make check` is an alias. The gate checks formatting, builds, standard and race tests, Go and shell linting, vet, deterministic smoke compilation, and release configuration. Container tests may skip locally when Docker or managed Node is unavailable. Buildkite requires those prerequisites.

Useful focused tasks are:

```sh
mise run build
mise run test
mise run test:race
mise run lint
mise run lint:go
mise run lint:shell
mise run smoke:local
mise run release:check
```

### Understand smoke results

`mise run smoke:local` is network-free. It validates the smoke inventory and compiles each workflow twice. A pass proves deterministic compilation, not runtime behavior.

```sh
mise run smoke:profile
```

The profile task uses the network to resolve selected public actions and apply the production `hosted` policy. Admission still does not execute action code. See [`testdata/smoke/README.md`](../testdata/smoke/README.md) for the inventory and result meanings.

## Benchmark public workflow compatibility

[`scripts/validate-public-workflow-corpus`](../scripts/validate-public-workflow-corpus) downloads the [GitHub Actions workflow histories dataset](https://doi.org/10.5281/zenodo.20340547), selects the latest valid non-deleted version of each workflow, and validates every declared supported event with the hosted profile. The first run downloads and extracts the corpus.

Use a stable sample while developing:

```sh
export GITHUB_TOKEN="$(gh auth token)"
SAMPLE_SIZE=1000 SAMPLE_SEED=default JOBS=32 \
  mise run corpus:public
```

Sampling ranks `repository`, workflow path, and content hash with the seed. The same corpus, size, and seed select the same workflows. Sample manifests, reports, and tallies use a key containing the size, seed digest, and selected-manifest digest, so they do not mix with full-corpus results.

Use 100 workflows for a smoke check, 1,000 while iterating, 10,000 for broader confidence, then omit `SAMPLE_SIZE` for the complete benchmark. Keep `SAMPLE_SEED` unchanged when comparing validator versions.

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

The script reports compatible, incompatible, and policy-rejected repositories among those the generated snapshots measured. It reports context-required and indeterminate repositories separately and excludes them from the compatibility percentage. The tally also records workflow result counts and keeps every diagnostic in the per-workflow report.

Pull-request path filters require a linked Buildkite webhook and a verified, bounded local git diff. The public corpus has workflow files but no repository checkouts or pull-request history. It still compiles these workflows, resolves their actions, constructs their plans, and applies hosted policy. When the diff is the only missing evidence, it reports the workflow as `context-required`. This does not claim admission. Push path filters, malformed filters, and workflows with another incompatibility remain incompatible.

Sample metadata is written to `records/<record-id>/samples/<sample-key>/validate-tally.json`; per-workflow v3 reports are under `reports/<record-id>/samples/<sample-key>/<validator-digest>/`. Full-corpus tallies and reports retain their existing paths.

Admission covers generated event snapshots, not arbitrary real payloads or action execution. Generated release validation uses one stable `published` event, so corpus results do not prove every supported release activity. Adding release can only raise the corpus compatibility ceiling for workflows whose explicit release types are limited to `published`, `created`, and `released`; bare and broader release triggers remain incompatible. The action-resolution snapshot pins action revisions only. Preserve the snapshot, corpus record, sample seed, and sample size when comparing compatibility across commits.

## Verify runtime behavior

Every normal Buildkite build runs repository checks, Test Engine-split Go tests, native macOS tests, the starter workflow compatibility report, and the shell smoke workflow against the build's exact CLI source. Test Engine records the Linux test results; the repository checks retain the race-enabled suite. GitHub Actions differential oracles run only when manually dispatched.

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

This suite covers shell jobs, concurrent steps, public and Dockerfile actions, container runtime behavior, summaries, annotations, artifact upload, and artifact roundtrip. Use `COMPATIBILITY_PROOF=<target>` with `COMPATIBILITY_PROOF_COMMIT=<commit>` only when diagnosing one target. The available target names are in [`.buildkite/pipeline.yml`](../.buildkite/pipeline.yml).

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
