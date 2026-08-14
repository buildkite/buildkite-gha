# Development and release guide

## Set up the development toolchain

The repository pins Go, Node, lint, and release tools with `mise`:

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

## Verify runtime behavior

Every normal Buildkite build runs repository checks, Test Engine-split Go tests, native macOS tests, the starter workflow compatibility report, and the shell smoke workflow against the build's exact CLI source. Test Engine records the Linux test results; the repository checks retain the race-enabled suite. GitHub Actions differential oracles run only when manually dispatched.

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
mise run release
```

The task runs `check`, chooses the next conventional-commit-derived `v0` tag, and pushes it. The tag build reruns checks and publishes the GitHub release, paired Linux/amd64 and Darwin/arm64 archives, and checksum file. Published assets are immutable; a failed publication must not replace an existing archive for the same stable tag.

`GHA_GITHUB_RELEASE_TOKEN` must be a fine-grained, repository-scoped token with Contents read and write access. Store it as a Buildkite secret restricted to this release pipeline and webhook-created `v*` tag builds, with no access from ordinary branch or pull request builds. The publisher verifies the remote tag, checkout, and Buildkite commit before requesting the secret.

## Related documents

- [Compatibility reference](compatibility.md)
- [Security model](security.md)
