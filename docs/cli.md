# Use the buildkite-gha CLI

The [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin) is the recommended way to run `buildkite-gha`. Use the CLI directly to validate workflows, inspect generated output, or build a custom importer.

## Before you begin

Install `buildkite-gha` with `mise` 2026.5.12 or newer:

```sh
mise use -g --minimum-release-age 0s github:buildkite/buildkite-gha
```

The `--minimum-release-age 0s` override prevents mise's default 24-hour delay from selecting an older release without an artifact for your platform.

Append `@<version>` to install an exact release. A custom importer that generates jobs for the other supported platform must also download and verify that platform's distribution from the same release.

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
  --profile hosted \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

The deprecated `hosted-tokenless` profile name remains an alias for `hosted`.

Use `--format json` for a `buildkite-gha/processing-report/v1` report.

Reports cover workflow parsing, event validation, graph construction, matrix expansion, expressions, action discovery and resolution, plan construction, profile admission, and pipeline generation. A blocked downstream stage is `not-evaluated`, not `failed`.

When `validate`, `compile`, or `upload` runs in a Buildkite job, it also publishes report warnings and errors as job-scoped Buildkite annotations. Annotation failures produce a warning but do not change the command result.

Profile validation applies the upload trigger policy before compilation. A `not-applicable` result means the workflow does not declare the selected event and would become a top-level skipped step during upload. Unsupported triggers and malformed data are incompatible.

Validation may use the public network to resolve actions. Apart from annotation publication when it runs in a Buildkite job, it does not call Buildkite, install Node, or execute workflow code.

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

`compile` does not upload the executable, plans, or pipeline, so piping its YAML directly to `buildkite-agent pipeline upload` is incomplete.

## Upload from a custom importer

`upload` is the public in-build command for custom importers:

```sh
buildkite-gha upload .github/workflows/ci.yml
```

The importer must run on Linux/amd64 or Darwin/arm64 with `BUILDKITE=true` and `BUILDKITE_STEP_KEY`.

The hidden zero-argument `buildkite-gha plugin` entry point reads `workflow`,
`workflows`, and `runners` from `BUILDKITE_PLUGIN_CONFIGURATION`. Set `workflow`
to one explicit path or `workflows` to a non-empty array of explicit paths; the
fields are mutually exclusive. Every path must identify a regular, tracked
`.yml` or `.yaml` file inside the repository; directories and glob patterns are
rejected. It also accepts the plugin-owned `version`, `source-ref`, and
`minimum-release-age` fields. The importer uses its verified executable for jobs
on the same platform and fetches the other platform's distribution from the
same release only when a workflow requires it. Custom importers can use the
public flags below. Runner queue mappings affect generated jobs, not the
importer step.

### Select workflows

Pass every workflow path explicitly:

```sh
buildkite-gha upload -- \
  .github/workflows/ci.yml \
  .github/workflows/release.yml
```

Every operand must name one regular `.yml` or `.yaml` file. When uploading more than one workflow, every path must be tracked inside the repository. Aliases and duplicates are canonicalized, deduplicated, and sorted, so reversed arguments produce the same pipeline. Directories, globs, missing files, other extensions, and symlinks are rejected before any workflow is parsed or Buildkite command runs; aggregate uploads also reject untracked and outside paths.

`--` ends option parsing and is required when a path operand begins with `-`; options must appear before it. Without `--`, a leading-dash operand is an unknown option. The CLI does not split shell strings or decode a JSON or YAML list from one argument: custom wrappers should pass each path as a separate argument and use `--` before externally supplied operands.

All selected directly runnable workflows are represented in one atomic pipeline upload. Each successfully compiled workflow becomes an aggregate group whose label is `:github: <workflow-name>` or, for an unnamed workflow, its canonical path. The group depends on the importer; child jobs do not repeat that dependency. A skipped workflow becomes one top-level skipped command step. One GitHub check is named `Buildkite / <workflow-name-or-path> (<effective-event>)`. A reusable-only `workflow_call` file may be selected so local callers can resolve it, but it does not create a group. An input set containing only reusable workflows is an error.

Every directly runnable workflow is selected against the effective event. A workflow with safe compilation or trigger-translation errors is replaced by one failing top-level command step instead of a group. The step prefixes the workflow check name with `:github:` for its label, prints all redacted diagnostics, then exits with status 1. A compiler failure takes precedence if the workflow also has a skip reason. Compilation continues for later workflows, and successfully compiled workflows retain their normal groups and jobs. Parse, event-input, admission, artifact, and upload failures still abort the aggregate transaction. No partially compiled pipeline is uploaded.

### Select the effective event

Event source precedence is:

1. `--event-path`
1. `buildkite:webhook` metadata reserved by Buildkite
1. A reduced snapshot derived from `BUILDKITE_*` variables when no linked webhook is available

An explicit event path never reads Buildkite metadata. Webhook metadata must be one valid JSON object no larger than 25 MiB. Malformed, unreadable, or oversized data stops upload rather than falling back. The Buildkite repository mapping, commit, and ref remain authoritative for the workload.

Raw webhook data is not retained in generated plans or pipeline YAML and cannot grant queues, secrets, or tokens.

The selected snapshot establishes one effective GitHub event for applicability, compilation, group conditions, and event-qualified check names; group labels remain static across events. An explicit event path uses its event directly and never re-reads live Buildkite event fields. Linked webhook metadata supplies the GitHub event name. Without either source, pull request builds map to `pull_request`; Buildkite `ui` and `api` sources map to `workflow_dispatch`; `schedule` maps to `schedule`; and other sources, including `trigger_job`, map to `push` even when `build.source_event` is absent.

Top-level workflows that do not declare the effective event are excluded before event-dependent validation and compilation, then emitted as top-level skipped command steps with no plan artifacts. Reusable-only workflows remain available to local callers. If no directly runnable workflow applies, upload succeeds with a skipped-only pipeline. For applicable workflows, only the selected event contributes a group condition: push branch/tag filters, pull request base-branch/activity filters, or the corresponding manual/schedule Buildkite source predicate. Cross-event trigger conditions are never ORed into that group.

Unsupported trigger events, path filters, and inexact filters replace the affected workflow with one failing top-level command step rather than being approximated. Malformed event data remains fatal to the importer. Safe compilation errors in an applicable workflow also produce a failing replacement step. Buildkite still owns build creation and schedule identity; every workflow with `on.schedule` is eligible for a Buildkite scheduled build because Buildkite does not expose which schedule created it.

After all applicable workflows have been attempted, the command uploads the exact executable, content-addressed plans, and synthetic failure steps before running one:

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
rejects images. Runtime distribution paths must be absolute executables. The
importer's platform defaults to its running executable; the other platform has
no direct-upload default.
`BUILDKITE_GHA_TARGET_QUEUE` and `BUILDKITE_GHA_RUNTIME_IMAGE` are no longer
supported.

The deprecated `--runtime-queue hosted` argument is accepted as a no-op for compatibility with plugin releases that pass it. Other values are rejected.

`run-job` is internal. Users should not invoke it directly.
