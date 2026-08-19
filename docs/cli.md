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

Validate syntax, the static graph, and every declared trigger without an event:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

This event-independent check accepts syntactically valid push and pull-request path filters because linked-webhook admission can evaluate them with a verified local git diff. It rejects malformed path filters, unsupported events, unsupported branch and tag filter combinations, and unsupported pull-request activity types. It does not resolve actions, evaluate event payload expressions, or claim hosted admission.

Resolve actions and apply production policy:

```sh
buildkite-gha validate \
  --profile hosted \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

For a quick compatibility check, generate a minimal supported event snapshot:

```sh
buildkite-gha validate \
  --profile hosted \
  --event pull_request \
  .github/workflows/ci.yml
```

`--event` supports `push`, `pull_request`, `merge_group`, `release`, `workflow_dispatch`, and `schedule`. It is available only with `--profile hosted` and is mutually exclusive with `--event-path`. The generated snapshot uses example repository identity and minimal event fields. The release snapshot is a stable, non-prerelease `published` event; it is representative static validation, not proof of every supported activity. Generated snapshots are not equivalent to real payloads. Use `--event-path` when exact refs, activity, repository identity, or payload fields matter.

Use `--all-events` with `--profile hosted` to evaluate every declared `push`, `pull_request`, `merge_group`, `release`, `workflow_dispatch`, and `schedule` trigger with a separate generated snapshot:

```sh
buildkite-gha validate \
  --profile hosted \
  --all-events \
  .github/workflows/ci.yml
```

`--all-events` is mutually exclusive with `--event` and `--event-path`. It does not evaluate `workflow_call` as a standalone event. Admission applies separately to each generated snapshot, not to every possible real payload. A `context-required` result means every available compilation and hosted-policy check passed, but a supported admission path needs evidence that generated snapshots do not contain. For example, push and pull-request path filters need linked webhook data and a verified local git diff.

Reuse downloaded immutable action source across profile validation runs:

```sh
mkdir -p .buildkite-gha-action-cache
buildkite-gha validate \
  --profile hosted \
  --all-events \
  --action-cache-dir .buildkite-gha-action-cache \
  .github/workflows/ci.yml
```

`--action-cache-dir` is available only with `--profile hosted`. The cache stores verified action source by immutable commit. Mutable ref resolutions are cached for up to one hour under `$XDG_CACHE_HOME/buildkite-gha/action-ref-resolutions/v1` (or the platform user cache directory). Concurrent validation processes can share this resolution cache. A moved tag or branch can therefore use its previous commit for up to one hour. Do not share either writable cache between mutually untrusted validation jobs.

For a large workflow corpus, reuse one validator process and action resolver:

```sh
buildkite-gha validate-batch \
  --manifest workflows.jsonl \
  --output-dir reports \
  --corpus-id zenodo:20340547 \
  --action-cache-dir .buildkite-gha-action-cache \
  --action-cache-max-bytes 21474836480 \
  --action-resolution-snapshot .buildkite-gha-action-resolutions
```

Each JSON Lines manifest record requires `id`, `repository`, `path`, `hash`, and `source` fields. Batch validation applies the hosted profile to all declared supported events and writes one `processing-report/v3` JSON file per workflow. It uses one worker per CPU by default; set `--jobs` to override the worker count. It publishes each report atomically and resumes valid reports keyed by the corpus ID, record identity, workflow and repository-local dependency content, validator executable digest, and action-resolution snapshot generation. Records with unresolved local dependencies are reprocessed instead of resumed.

`--action-cache-max-bytes` requires `--action-cache-dir`. It evicts the least recently used immutable action trees until the cache is within the byte budget. Concurrent validators protect entries while reading or publishing them. Maintenance also removes abandoned partial entries; active partial entries remain locked. The public corpus script defaults to 20 GiB, leaving headroom on a 64 GB orb.

The required `--action-resolution-snapshot` pins each mutable `owner/repository@ref` to its first resolved commit. The snapshot is durable across validator versions, so comparisons that reuse its generation resolve the same recorded refs. Exact commit references bypass it. Definitively missing public refs are also recorded; network, cancellation, TLS, rate-limit, and server failures are retried rather than recorded. Use `--refresh-action-resolution-snapshot` to start a new generation and resolve refs again. The public corpus script removes every validator report set for the corpus record when refreshing so its tally only reads current results. The snapshot controls action revisions only; it does not make the complete corpus run reproducible.

For authenticated GitHub API resolution, keep the token in an environment variable and name that variable without exposing its value:

```sh
GITHUB_TOKEN="$(your-secure-token-command)" \
  buildkite-gha validate-batch \
  --manifest workflows.jsonl \
  --output-dir reports \
  --corpus-id example \
  --action-resolution-snapshot .buildkite-gha-action-resolutions \
  --github-token-env GITHUB_TOKEN
```

The token authenticates GitHub API metadata requests only. It is not written to arguments, logs, reports, snapshots, or action source caches, and validation does not execute action subprocesses. Authenticated resolution still verifies that every action repository is public. Ordinary `validate` continues to use the one-hour mutable-ref cache in the platform user cache directory.

Inspect the aggregate result and each event outcome:

```sh
buildkite-gha validate \
  --profile hosted \
  --all-events \
  --format json \
  .github/workflows/ci.yml |
  jq '{result, events: [.evaluations[] | {event, result: .report.result}]}'
```

Inspect diagnostics with their generated event names:

```sh
buildkite-gha validate \
  --profile hosted \
  --all-events \
  --format json \
  .github/workflows/ci.yml |
  jq -r '(.validation.diagnostics[] | "validation: \(.code): \(.message)"),
    (.evaluations[] | .event as $event | .report.diagnostics[] | "\($event): \(.code): \(.message)")'
```

The deprecated `hosted-tokenless` profile name remains an alias for `hosted`.
Hosted validation uses the same runner preset as production upload.

Use `--format json` for a `buildkite-gha/processing-report/v2` report. `--all-events` emits `buildkite-gha/processing-report/v3`, which contains the event-independent v2 report and one v2 report for each generated event evaluation. The top-level result is `admitted` only when every evaluation is admitted. It is `context-required` when generated inputs cannot measure an otherwise supported path, unless another finding makes the workflow incompatible, not admitted, or indeterminate.

Reports cover workflow parsing, event validation, graph construction, matrix expansion, expressions, action discovery and resolution, plan construction, profile admission, and pipeline generation. A blocked downstream stage is `not-evaluated`, not `failed`.

Report warnings and errors become job-scoped Buildkite annotations. `validate`, `compile`, and upload failures that abort the transaction attach them to the current job. Generated failure steps attach their diagnostics to themselves. CLI-side annotation failures produce a warning but do not change the command result.

Profile validation applies the upload trigger policy before compilation. A `not-applicable` result means the workflow does not declare the selected event and would become a top-level skipped step during upload. Unsupported triggers and malformed data are incompatible.

Validation may use the public network to resolve actions. Apart from annotation publication when it runs in a Buildkite job, it does not call Buildkite, install Node, or execute workflow code.

## Provide an event snapshot

`compile` and profile validation with `--event-path` need a bounded event snapshot:

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

The snapshot supplies compile-time context. Generated plans retain the event name, repository, ref, pull request head ref, SHA, actor, and a payload digest. They do not retain the payload object. Runtime expressions cannot use `github.event`.

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
`workflows`, `runners`, and `oidc` from `BUILDKITE_PLUGIN_CONFIGURATION`. Set `workflow`
to one explicit path or `workflows` to a non-empty array of explicit paths; the
fields are mutually exclusive. Every path must identify a regular, tracked
`.yml` or `.yaml` file inside the repository; directories and glob patterns are
rejected. It also accepts the plugin-owned `version`, `source-ref`, and
`minimum-release-age` fields, plus the boolean `experimental-runner-user` and
`private-reusable-workflows` fields.
The optional `oidc` object accepts `claims`, `aws-session-tags`, and
`subject-claim`; configured lists and strings must be non-empty. Unknown fields
and invalid values are rejected before upload.
The importer uses its verified executable for jobs on the same platform and
fetches the other platform's distribution from the same release only when a
workflow requires it. Custom importers can use the public flags below. Runner
queue mappings affect generated jobs, not the importer step.

### Select workflows

Pass every workflow path explicitly:

```sh
buildkite-gha upload -- \
  .github/workflows/ci.yml \
  .github/workflows/release.yml
```

Every operand must name one regular `.yml` or `.yaml` file. When uploading more than one workflow, every path must be tracked inside the repository. Aliases and duplicates are canonicalized, deduplicated, and sorted, so reversed arguments produce the same pipeline. Directories, globs, missing files, other extensions, and symlinks are rejected before any workflow is parsed or Buildkite command runs; aggregate uploads also reject untracked and outside paths.

`--` ends option parsing and is required when a path operand begins with `-`; options must appear before it. Without `--`, a leading-dash operand is an unknown option. The CLI does not split shell strings or decode a JSON or YAML list from one argument: custom wrappers should pass each path as a separate argument and use `--` before externally supplied operands.

All selected directly runnable workflows are represented in one atomic pipeline upload. Each successfully compiled workflow becomes an aggregate group whose label is `:github: <workflow-name>` or, for an unnamed workflow, its canonical path. The group depends on the importer; child jobs do not repeat that dependency. Each child publishes a provider check named `<workflow-name-or-path> / <job-id> (<effective-event>)`. A skipped workflow becomes one top-level skipped command step with a check named `<workflow-name-or-path> (<effective-event>)`. GitHub events publish GitHub checks; Origin events publish Origin checks. A reusable-only `workflow_call` file may be selected so local callers can resolve it, but it does not create a group. An input set containing only reusable workflows is an error.

Private reusable workflows are disabled by default. Set the plugin's `private-reusable-workflows: true` field, or pass `upload --private-reusable-workflows` from a custom importer. Before validating remote calls, the importer tries the bounded anonymous source, then fetches inaccessible repositories with Git over HTTPS. Git uses the importer's existing credential helpers and environment. The Buildkite Agent repository-provider helper can grant same-repository or approved cross-repository access. Credentials stay in Git and are not included in plans, generated pipeline YAML, or runtime jobs.

Every directly runnable workflow is selected against the effective event. A workflow with safe compilation or trigger-translation errors is replaced by one failing top-level command step labeled `:github: <workflow-name-or-path>`. The replacement step publishes all redacted diagnostics as a job-scoped Buildkite annotation, then exits with status 1. Its provider check presents the failure reasons as described in [Aggregate workflow upload](compatibility.md#aggregate-workflow-upload). A compiler failure takes precedence if the workflow also has a skip reason. Compilation continues for later workflows, and successfully compiled workflows retain their normal groups and jobs. Parse, event-input, admission, artifact, and upload failures still abort the aggregate transaction. No partially compiled pipeline is uploaded.

### Select the effective event

Event source precedence is:

1. `--event-path`
1. `buildkite:webhook` metadata reserved by Buildkite
1. A reduced snapshot derived from `BUILDKITE_*` variables when no linked webhook is available

An explicit event path never reads Buildkite metadata. Webhook metadata must be one valid JSON object no larger than 25 MiB. Malformed, unreadable, or oversized data stops upload rather than falling back. The Buildkite repository mapping, commit, and ref remain authoritative for the workload.

Raw webhook data is not retained in generated plans or pipeline YAML and cannot grant queues, secrets, or tokens.

The selected snapshot establishes one effective GitHub event for applicability, compilation, group conditions, and event-qualified check names; group labels remain static across events. An explicit event path uses its event directly and never re-reads live Buildkite event fields. Linked webhook metadata supplies the GitHub event name, including `merge_group` and `release`. Merge queue builds require matching Buildkite head and base refs and commits. Release builds require matching webhook and Buildkite activities, a valid release payload, and a tag matching both `BUILDKITE_TAG` and `BUILDKITE_BRANCH`. The plugin resolves Buildkite's symbolic release commit from the checked-out `HEAD` before constructing the event. Without linked metadata, the environment fallback never infers `release`: pull request builds map to `pull_request`; Buildkite `ui` and `api` sources map to `workflow_dispatch`; `schedule` maps to `schedule`; and other sources, including tag builds and `trigger_job`, map to `push` even when `build.source_event` is absent.

Top-level workflows that do not declare the effective event are excluded before event-dependent validation and compilation, then emitted as top-level skipped command steps with no plan artifacts. Reusable-only workflows remain available to local callers. If no directly runnable workflow applies, upload succeeds with a skipped-only pipeline. For applicable workflows, only the selected event contributes a group condition: push branch/tag/path filters, pull request base-branch/activity/path filters, merge group base-branch/activity filters, release activity filters, or the corresponding manual/schedule Buildkite source predicate. Cross-event trigger conditions are never ORed into that group.

Unsupported or uncertain filters replace only the affected workflow with a failing top-level step. Push and pull request path filters run only when the linked webhook and local checkout prove a match; see [Push path filters](compatibility.md#push-path-filters) and [Pull request path filters](compatibility.md#pull-request-path-filters). Generated or explicit snapshots can report the dependency but cannot stand in for real linked-webhook admission. Malformed event data still stops the import. Buildkite owns build creation and schedule identity, so every workflow with `on.schedule` is eligible for every Buildkite scheduled build.

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

The hosted preset accepts runner labels case-insensitively, so aliases such as
`macOS-latest` and `Ubuntu-Latest` are equivalent to their lowercase forms.
`ubuntu-latest` and `ubuntu-24.04` default to the Noble hosted-toolchains image;
`ubuntu-22.04` defaults to Jammy. Use `--runner-image` with an immutable digest
to override the default for a configured profile. Linux labels keep default
agent targeting with the matching image when unmapped. During upload, the
importer first asks the job-scoped Agent API to resolve each `runs-on` selector.
A returned target takes precedence over `--runner-queue` and the local preset.
Configured mappings and the local `macos-latest` preset remain fallbacks when
the API does not return a target. Linux ARM, macOS x86-64, Windows, and other
labels are unsupported. Every macOS label rejects images. Runtime distribution
paths must be absolute executables. The importer's platform defaults to its
running executable; the other platform has no direct-upload default.
`BUILDKITE_GHA_TARGET_QUEUE` and `BUILDKITE_GHA_RUNTIME_IMAGE` are no longer
supported.

The deprecated `--runtime-queue hosted` argument is accepted as a no-op for compatibility with plugin releases that pass it. Other values are rejected.

### Run Linux jobs as a non-root user

Generated Linux jobs use a dedicated `runner` user by default. This behavior
requires buildkite-gha v0.13.7 or newer. Generated jobs must start as root. The
bootstrap creates the `runner` user, grants passwordless `sudo` and Docker
socket access when the socket exists, prepares the runner home, temp, mise, and
tool-cache paths, then runs `buildkite-gha run-job` as `runner`. The verified
executable and compiled plan remain root-owned and read-only to `runner`.
Generated jobs skip the Buildkite checkout. When a workflow uses
`actions/checkout`, the native adapter clones as `runner`, so the runtime does
not recursively change workspace ownership. This behavior does not depend on a
queue name and does not affect macOS jobs.

During the transition, set the plugin field to `false` to restore root
execution:

```yaml
steps:
  - label: ":github: CI"
    plugins:
      - github-actions#latest:
          workflow: .github/workflows/ci.yml
          experimental-runner-user: false
```

For a custom importer, pass `upload --experimental-runner-user=false`. The bare
`--experimental-runner-user` form and a plugin value of `true` remain accepted.
The plugin value must be a YAML boolean, not a quoted string.

## Disable telemetry

In Buildkite jobs, the plugin importer and `run-job` send best-effort completion telemetry through the job-authenticated Buildkite Agent API. Events contain the command, outcome, client version, duration, and bounded diagnostic codes and severities. They do not contain diagnostic messages, workflow content, repository details, paths, action references, expressions, environment variables, or command text.

Set `BUILDKITE_GHA_TELEMETRY_DISABLED=true` to disable telemetry. Missing Agent endpoint, job ID, or job token also disables it. Telemetry failures do not change command results.

`run-job` is internal. Users should not invoke it directly.
