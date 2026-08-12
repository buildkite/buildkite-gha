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
mise run lint:go
mise run lint:shell
mise run smoke:local
mise run release:check
```

Native macOS changes must also cross-build the complete command and validate the
paired GoReleaser configuration:

```sh
GOOS=darwin GOARCH=arm64 mise exec -- go build ./...
mise exec -- goreleaser check
```

### Understand smoke results

`mise run smoke:local` is network-free. It validates the smoke inventory and compiles each workflow twice. A pass proves deterministic compilation, not runtime behavior.

```sh
mise run smoke:profile
```

The profile task uses the network to resolve selected public actions and apply the production `hosted-tokenless` policy. Admission still does not execute action code. See [`testdata/smoke/README.md`](../testdata/smoke/README.md) for the inventory and result meanings.

## Verify runtime behavior

Every normal Buildkite build runs repository checks and executes the basic example through a released plugin against the build's exact CLI source.

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

Run the examples through the released plugin and its default mise-based runtime resolution:

```sh
commit=$(git rev-parse HEAD)
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env DEMO_SUITE=plugin --env DEMO_COMMIT="$commit" --yes
```

Add `--env DEMO_CACHE=1` to include the optional cache producer and consumer workflow. Mise must download and verify the public CLI release. The demo must not fall back to a local binary.

Native macOS support requires a paired stable release before a live Hosted smoke:

1. Merge an approved `buildkite-gha` change to `main`.
1. With separate release approval, create the stable version/tag.
1. Confirm the GitHub release contains
   `buildkite-gha_Linux_x86_64.tar.gz`,
   `buildkite-gha_Darwin_arm64.tar.gz`, and `checksums.txt`, with exactly one
   matching checksum entry for each archive.
1. Merge and release the companion plugin change that passes `runners` through
   to `buildkite-gha plugin`. Plugin v0.8.0 rejects `runners`; use the first
   later release that documents this field.
1. Configure that compatible plugin release with the exact CLI version and
   explicit runner mappings:

   ```yaml
   plugins:
     - github-actions#<release-with-runners-pass-through>:
         version: "<new-exact-buildkite-gha-version>"
         workflow: .github/workflows/mixed.yml
         runners:
           - runs-on: ubuntu-latest
             queue: hosted
           - runs-on: macos-14
             queue: macos-sonoma-arm64
   ```

1. Run a Linux-only Hosted smoke. Confirm the importer runs on Linux/amd64,
   publishes its own exact runtime, and makes no Darwin archive request.
1. Run a mixed Hosted smoke. Confirm the importer stays on Linux/amd64, fetches
   and verifies the same release's Darwin archive, and sends each job to its
   mapped native queue with its bound runtime digest.
1. Run a Darwin-only workflow graph. Confirm only the Darwin job runtime is
   needed by the generated graph while the importer remains Linux/amd64.
1. Run negative proofs: an unmapped macOS label, macOS image, missing or
   mismatched Darwin asset/checksum, and macOS container or Dockerfile action
   must each fail closed before workflow execution.
1. Record the native Hosted macOS result without claiming GitHub image, Xcode,
   container, or service parity.

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
