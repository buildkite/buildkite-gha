# Use the buildkite-gha CLI

The [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin) is the recommended way to run `buildkite-gha`. Use the CLI directly to validate workflows, inspect generated output, or build a custom importer.

## Before you begin

Install `buildkite-gha` with `mise` 2026.5.12 or newer:

```sh
mise use -g github:buildkite/buildkite-gha
```

Append `@<version>` to install an exact release. A custom importer that runs macOS jobs must also download and verify the same release's Darwin/arm64 distribution.

Jobs with JavaScript actions require `mise`. `run-job` checks `BUILDKITE_GHA_MISE`, then `PATH`, and then downloads a verified managed copy. Shell-only, native-adapter, and Docker-only jobs do not require `mise`.

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

The importer must run on Linux/amd64 with `BUILDKITE=true` and `BUILDKITE_STEP_KEY`.

The hidden zero-argument `buildkite-gha plugin` entry point reads `workflow` and
`runners` from `BUILDKITE_PLUGIN_CONFIGURATION`; it also accepts the plugin-owned
`version`, `source-ref`, and `minimum-release-age` fields. The Linux/amd64 importer
fetches the same release's Darwin runtime only when the workflow requires it.
Released plugin v0.8.0 does not declare or support `runners`; schema validation
rejects that configuration. Native macOS therefore requires the companion
plugin release. Custom importers can use the public flags below.

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

### Choose runners and runtimes

Use repeatable mappings before the workflow path:

```sh
buildkite-gha upload \
  --runner-queue ubuntu-latest=hosted \
  --runner-queue macos-14=macos-sonoma-arm64 \
  --runtime-distribution darwin/arm64=/opt/buildkite-gha-darwin \
  .github/workflows/ci.yml
```

Configured `ubuntu-latest` and `ubuntu-24.04` profiles default to the Noble
hosted-toolchains image; `ubuntu-22.04` defaults to Jammy. Use `--runner-image`
with an immutable digest to override the default. Unmapped Linux labels keep
default targeting without an image. Every macOS label requires a queue and
rejects images. Runtime distribution paths must be absolute executables; Linux
defaults to the importer and Darwin has no direct-upload default.
`BUILDKITE_GHA_TARGET_QUEUE` and `BUILDKITE_GHA_RUNTIME_IMAGE` are no longer
supported.

The deprecated `--runtime-queue hosted` argument is accepted as a no-op for compatibility with plugin releases that pass it. Other values are rejected.

`run-job` is internal. Users should not invoke it directly.
