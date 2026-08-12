# Use the buildkite-gha CLI

The [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin) is the recommended way to run `buildkite-gha`. Use the CLI directly to validate workflows, inspect generated output, or build a custom importer.

## Before you begin

Download `buildkite-gha_Linux_x86_64.tar.gz` and `checksums.txt` from the matching GitHub [release](https://github.com/buildkite/buildkite-gha/releases). Verify the checksum, then extract `buildkite-gha` to a stable path. Releases that run macOS jobs also contain a same-version `buildkite-gha_Darwin_arm64.tar.gz` distribution.

Jobs with JavaScript actions require `mise` 2026.5.12 or newer. `run-job` checks `BUILDKITE_GHA_MISE`, then `PATH`, and then downloads a verified managed copy. Shell-only, native-adapter, and Docker-only jobs do not require `mise`.

Managed Node binaries require glibc 2.28 or newer. The Go CLI has no glibc requirement.

## Validate a workflow

Validate syntax and the static graph:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

Resolve actions and apply production policy:

```sh
buildkite-gha validate \
  --profile hosted-tokenless \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

Use `--format json` for a `buildkite-gha/processing-report/v1` report.

Reports cover workflow parsing, event validation, graph construction, matrix expansion, expressions, action discovery and resolution, plan construction, profile admission, and pipeline generation. A blocked downstream stage is `not-evaluated`, not `failed`.

Validation may use the public network to resolve actions. It does not call Buildkite, install Node, or execute workflow code.

## Provide an event snapshot

`compile` and profile validation need a bounded event snapshot:

```json
{
  "provider": "github",
  "event": "push",
  "repository": {
    "owner": "acme",
    "name": "widgets",
    "clone_url": "https://github.com/acme/widgets.git",
    "default_branch": "main"
  },
  "ref": "refs/heads/main",
  "sha": "0123456789abcdef0123456789abcdef01234567",
  "actor": "octocat",
  "payload": {
    "ref": "refs/heads/main"
  }
}
```

The snapshot supplies compile-time context. Generated plans retain the event name, repository, ref, SHA, actor, and a payload digest. They do not retain the payload object. Runtime expressions cannot use `github.event`.

The snapshot is compatibility data, not authorization.

## Compile a pipeline

Render Buildkite pipeline YAML:

```sh
buildkite-gha compile \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

Inspect compiler IR:

```sh
buildkite-gha compile \
  --event-path .buildkite/events/current.json \
  --format ir-json \
  .github/workflows/ci.yml
```

`compile` is read-only. It does not upload the executable, plans, or pipeline, so piping its YAML directly to `buildkite-agent pipeline upload` is incomplete.

## Upload from a custom importer

`upload` is the public in-build command for custom importers:

```sh
buildkite-gha upload .github/workflows/ci.yml
```

It requires `BUILDKITE=true` and `BUILDKITE_STEP_KEY`.

The hidden zero-argument `buildkite-gha plugin` entry point provides the
dedicated GitHub Actions plugin integration boundary. It strictly reads the
plugin's `BUILDKITE_PLUGIN_CONFIGURATION` JSON. `workflow` is required;
`runners` is an optional non-empty array whose entries require `runs-on` and
`queue` and may include an immutable Linux `image`. Acquisition-only `version`
and `minimum-release-age` fields are accepted and ignored by the CLI. Unknown
behavioral fields, duplicate fields, and unknown runner fields are rejected.

The plugin importer remains Linux/amd64. The exact selected release binary uses
its own verified bytes for Linux jobs. It resolves the workflow graph before
runtime acquisition, and downloads, checksum-verifies, caches, and validates the
same release's `buildkite-gha_Darwin_arm64.tar.gz` only when the graph requires
Darwin/arm64. Linux-only graphs do not request that asset.

Event source precedence is:

1. `--event-path`
1. `buildkite:webhook` metadata reserved by Buildkite
1. A reduced snapshot derived from `BUILDKITE_*` variables when no linked webhook is available

An explicit event path never reads Buildkite metadata. Webhook metadata must be one valid JSON object no larger than 25 MiB. Malformed, unreadable, or oversized data stops upload rather than falling back. The Buildkite repository mapping, commit, and ref remain authoritative for the workload.

Raw webhook data is not retained in generated plans or pipeline YAML and cannot grant queues, secrets, or tokens.

The command uploads the exact executable and content-addressed plans before running:

```sh
buildkite-agent pipeline upload --no-interpolation --reject-secrets
```

### Choose a queue

Use repeatable runner mappings before the workflow path:

```sh
buildkite-gha upload \
  --runner-queue ubuntu-latest=hosted \
  --runner-queue macos-14=macos-sonoma-arm64 \
  .github/workflows/ci.yml
```

Unmapped supported Linux labels retain default Buildkite targeting. Every macOS
label requires an explicit queue. Duplicate and unsupported labels are rejected.
The old `BUILDKITE_GHA_TARGET_QUEUE` environment variable is rejected rather than
silently losing an operator's isolation policy.

The deprecated `--runtime-queue hosted` argument is accepted as a no-op for compatibility with plugin releases that pass it. Other values are rejected.

### Choose an immutable runtime image

Pair `--runner-image` with the corresponding
`--runner-queue`:

```sh
buildkite-gha upload \
  --runner-queue ubuntu-latest=hosted \
  --runner-image ubuntu-latest=buildkite.namespace-images.com/agent-base@sha256:04a6656f92b90269b3259fffaba67e08a3d03d8dc79b40d45c9ac3d9000e9e03 \
  .github/workflows/ci.yml
```

Runtime images are Linux-only. macOS jobs run natively and reject image
selection. Tags and other mutable references are rejected. The old
`BUILDKITE_GHA_RUNTIME_IMAGE` environment variable is also rejected rather than
silently dropping the configured image.

### Supply runtime distributions

Custom importers can bind each generated platform to a local executable:

```sh
buildkite-gha upload \
  --runtime-distribution linux/amd64=/opt/buildkite-gha-linux \
  --runtime-distribution darwin/arm64=/opt/buildkite-gha-darwin \
  --runner-queue macos-14=macos-sonoma-arm64 \
  .github/workflows/ci.yml
```

Paths must be absolute, non-symlink executable files. `upload` opens, validates,
and hashes each distribution; generated schema-v8 plans bind the selected digest.
Linux defaults to the running importer when omitted. Darwin has no direct-upload
default. The production plugin performs its same-release lazy Darwin acquisition
internally instead of translating plugin configuration into these flags.

`run-job` is internal. Users should not invoke it directly.
