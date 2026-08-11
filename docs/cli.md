# Use the buildkite-gha CLI

The [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin) is the recommended way to run `buildkite-gha`. Use the CLI directly to validate workflows, inspect generated output, or build a custom importer.

## Before you begin

Download `buildkite-gha_Linux_x86_64.tar.gz` and `checksums.txt` from the matching GitHub [release](https://github.com/buildkite/buildkite-gha/releases). Verify the checksum, then extract `buildkite-gha` to a stable path.

Jobs with JavaScript actions require `mise` 2026.5.12 or newer. `run-job` checks `BUILDKITE_GHA_MISE`, then `PATH`, and then downloads a verified managed copy. Shell-only, native-adapter, and Docker-only jobs do not require `mise`.

Managed Node binaries require glibc 2.28 or newer. The Go CLI has no glibc requirement.

## Validate a workflow

Validate syntax and the static graph:

```bash
buildkite-gha validate .github/workflows/ci.yml
```

Resolve actions and apply production policy:

```bash
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

```bash
buildkite-gha compile \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

Inspect compiler IR:

```bash
buildkite-gha compile \
  --event-path .buildkite/events/current.json \
  --format ir-json \
  .github/workflows/ci.yml
```

`compile` is read-only. It does not upload the executable, plans, or pipeline, so piping its YAML directly to `buildkite-agent pipeline upload` is incomplete.

## Upload from a custom importer

`upload` is the public in-build command for custom importers:

```bash
buildkite-gha upload .github/workflows/ci.yml
```

It requires `BUILDKITE=true` and `BUILDKITE_STEP_KEY`.

The hidden `buildkite-gha plugin` entry point provides the dedicated GitHub Actions plugin integration boundary. It reads the workflow from `BUILDKITE_PLUGIN_GITHUB_ACTIONS_WORKFLOW`, accepts no arguments, and is intentionally absent from ordinary CLI help.

Event source precedence is:

1. `--event-path`
1. `buildkite:webhook` metadata reserved by Buildkite
1. A reduced snapshot derived from `BUILDKITE_*` variables when no linked webhook is available

An explicit event path never reads Buildkite metadata. Webhook metadata must be one valid JSON object no larger than 25 MiB. Malformed, unreadable, or oversized data stops upload rather than falling back. The Buildkite repository mapping, commit, and ref remain authoritative for the workload.

Raw webhook data is not retained in generated plans or pipeline YAML and cannot grant queues, secrets, or tokens.

The command uploads the exact executable and content-addressed plans before running:

```bash
buildkite-agent pipeline upload --no-interpolation --reject-secrets
```

### Choose a queue

Uploads inherit agent targeting from the importer. Set one queue on the importer when needed:

```yaml
env:
  BUILDKITE_GHA_TARGET_QUEUE: gha-untrusted
```

Every accepted Linux runner label maps to that queue. The queue must provide whole-job isolation and no ambient protected credentials.

The deprecated `--runtime-queue hosted` argument is accepted as a no-op for compatibility with plugin releases that pass it. Other values are rejected.

### Choose an immutable runtime image

Set an image by digest:

```yaml
env:
  BUILDKITE_GHA_RUNTIME_IMAGE: buildkite.namespace-images.com/agent-base@sha256:04a6656f92b90269b3259fffaba67e08a3d03d8dc79b40d45c9ac3d9000e9e03
```

Tags and other mutable references are rejected. The image may provide a baked `/opt/hostedtoolcache` inventory.

`run-job` is internal. Users should not invoke it directly.
