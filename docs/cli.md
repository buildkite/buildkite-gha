# Use the buildkite-gha CLI

The [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin)
is the easiest way to run `buildkite-gha`. Use the CLI directly when you need
to validate a workflow, inspect generated output, or build a custom importer.

| Command | Use it to |
| --- | --- |
| `validate` | Check one workflow and, optionally, production policy. |
| `validate-batch` | Check a large workflow corpus. |
| `compile` | Render pipeline YAML or compiler IR without uploading it. |
| `upload` | Upload workflows from a custom importer. |

`run-job` is an internal command. Do not invoke it directly.

## Before you begin

Install `buildkite-gha` with `mise` 2026.5.12 or newer:

```sh
mise use -g --minimum-release-age 0s github:buildkite/buildkite-gha
```

The override avoids mise's default 24-hour release delay. Without it, mise may
select an older release that has no artifact for your platform.

Append `@<version>` to install an exact release. If a custom importer creates
jobs for the other supported platform, it must download and verify that
platform's distribution from the same release.

Jobs with JavaScript actions need `mise`. The runtime checks
`BUILDKITE_GHA_MISE`, then `PATH`, then downloads a verified managed copy.
Shell-only, native-adapter, and Docker-only jobs do not need it.

Managed Node binaries require glibc 2.28 or newer. The Go CLI has no glibc requirement.

## Validate a workflow

Validate syntax, the static graph, and every declared trigger without an event:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

This event-independent check validates syntax, triggers, and the static graph.
It accepts valid push and pull-request path filters because upload can evaluate
them later with linked-webhook data and a verified local Git diff.

It does not:

- resolve actions
- evaluate event payload expressions
- claim that production policy would admit the workflow

Malformed filters, unsupported filter combinations, and unsupported pull
request activity types still fail. See [Names and triggers](compatibility.md#names-and-triggers)
for the exact trigger contract.

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

`--event` supports `push`, `pull_request`, `merge_group`, `release`,
`workflow_dispatch`, and `schedule`. It requires `--profile hosted` and cannot be
combined with `--event-path`.

The generated snapshot contains an example repository and the minimum event
fields. It is useful for a quick check, but it is not a real payload. The
release snapshot represents one stable, non-prerelease `published` event. Use
`--event-path` when exact refs, activity, repository identity, or payload fields
matter.

Use `--all-events` with `--profile hosted` to evaluate each declared supported
event with its own generated snapshot:

```sh
buildkite-gha validate \
  --profile hosted \
  --all-events \
  .github/workflows/ci.yml
```

`--all-events` cannot be combined with `--event` or `--event-path`. It does not
evaluate `workflow_call` as a standalone event.

Each generated event is checked separately. The result does not cover every
possible real payload. `context-required` means the available checks passed,
but admission needs evidence the generated snapshot cannot provide. Push and
pull-request path filters, for example, need linked-webhook data and a verified
local Git diff.

Reuse downloaded immutable action source across profile validation runs:

```sh
mkdir -p .buildkite-gha-action-cache
buildkite-gha validate \
  --profile hosted \
  --all-events \
  --action-cache-dir .buildkite-gha-action-cache \
  .github/workflows/ci.yml
```

`--action-cache-dir` is available only with `--profile hosted`. It stores
verified action source by immutable commit.

Mutable ref resolutions are cached for one hour under
`$XDG_CACHE_HOME/buildkite-gha/action-ref-resolutions/v1`, or the platform's
user cache directory. Concurrent validators can share this cache, so a moved
tag or branch may use its previous commit for up to one hour. Do not share
either writable cache between untrusted validation jobs.

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

Each JSON Lines manifest record requires `id`, `repository`, `path`, `hash`, and
`source`. Batch validation:

- applies the hosted profile to every declared supported event
- writes one `processing-report/v3` JSON file per workflow
- uses one worker per CPU unless `--jobs` overrides it
- publishes each report atomically
- resumes reports only when the corpus, record, workflow dependencies,
  validator executable, and action-resolution generation still match

Records with unresolved local dependencies are processed again.

`--action-cache-max-bytes` requires `--action-cache-dir`. When the cache exceeds
the limit, validation evicts the least recently used immutable action trees.
Concurrent validators lock active entries. Maintenance removes abandoned
partial entries but leaves active ones alone. The public corpus script defaults
to 20 GiB.

The required `--action-resolution-snapshot` pins each mutable
`owner/repository@ref` to the first commit it resolves. Reusing the same
generation keeps those refs stable across validator versions. Exact commit
references bypass the snapshot.

The snapshot records definitively missing public refs. It does not record
network, cancellation, TLS, rate-limit, or server failures; those are retried.
Use `--refresh-action-resolution-snapshot` to start a new generation. The public
corpus script then removes old report sets for that corpus record.

The snapshot pins action revisions only. It does not make the whole corpus run
reproducible.

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

The token authenticates GitHub API metadata requests only. It is not written to
arguments, logs, reports, snapshots, or action caches. Validation does not run
action subprocesses, and it still verifies that every action repository is
public.

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

Use `--format json` for a `buildkite-gha/processing-report/v2` report.
`--all-events` emits v3, containing the event-independent report and one v2
report per generated event.

The top-level result is:

- `admitted` only when every event is admitted
- `context-required` when generated input cannot measure an otherwise supported
  path, unless another finding takes precedence

Reports cover every stage from parsing through pipeline generation. If an
earlier stage blocks a later one, the later stage is `not-evaluated`, not
`failed`.

Warnings and errors become job-scoped Buildkite annotations. A failure that
aborts `validate`, `compile`, or upload attaches to the current job. Generated
failure steps attach their own diagnostics. If the CLI cannot publish an
annotation, it warns without changing the command result.

Profile validation applies upload's trigger policy before compilation.
`not-applicable` means the workflow does not declare the selected event and
would become a skipped top-level step. Malformed event data is incompatible. An
unsupported trigger beside a supported one produces a warning.

Validation may use the public network to resolve actions. It does not install
Node or execute workflow code. It calls Buildkite only to publish annotations
when it runs inside a Buildkite job.

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

The snapshot supplies compile-time context. Plans retain the event name,
repository, refs, SHA, actor, and a payload digest. They do not retain the
payload itself, so runtime expressions cannot use `github.event`.

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

The hidden, zero-argument `buildkite-gha plugin` entry point reads plugin
configuration from `BUILDKITE_PLUGIN_CONFIGURATION`. It accepts:

- either one `workflow` path or a non-empty `workflows` array
- `runners` and `oidc`
- plugin-owned `version`, `source-ref`, and `minimum-release-age` fields
- the Boolean `experimental-runner-user` field

Missing or untracked workflow paths warn and are skipped. If every configured
path is missing or untracked, the plugin succeeds without uploading a pipeline.
Every present path must be a regular, tracked `.yml` or `.yaml` file inside the
repository. Directories, tracked files missing from the checkout, symlinks, and
globs are rejected. The optional `oidc` object accepts non-empty `claims`,
`aws-session-tags`, and `subject-claim` values. Unknown fields and invalid values
fail before upload.

The plugin resolves relative workflow paths from `BUILDKITE_BUILD_CHECKOUT_PATH`,
not the command hook's working directory.

The importer reuses its verified executable for jobs on the same platform. It
downloads the other platform's distribution from the same release only when a
workflow needs it. Runner mappings apply to generated jobs, not the importer.

### Configure generated-job cache volumes

An explicit runner mapping can attach one Buildkite Hosted cache volume to each
generated job using that mapping:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/ci.yml
      runners:
        - runs-on: ubuntu-latest
          queue: hosted
          cache:
            paths:
              - /home/runner/.gradle/caches
              - /home/runner/.gradle/wrapper
            name: gradle-dependencies
            size: 40g
```

`cache.paths` is a required, non-empty list of unique absolute paths. `name`
and `size` are optional. Names follow Buildkite's 100-character
letters-numbers-hyphens format and may contain `${BUILDKITE_*}` variables.
Sizes use `Ng` and must be at least `20g`. Without a name or size, Buildkite
uses its pipeline-scoped name and 20 GB defaults.

Each Buildkite step supports one cache volume. When a job also needs the
internally managed mise cache, `buildkite-gha` adds the mise path to the same
volume. A configured name and size apply to that combined volume; otherwise,
the managed mise name and Buildkite's default size remain unchanged. Jobs with
neither configuration emit no `cache` attribute.

Runner cache volumes are not supported for workflow jobs that set `container`.

Generated Linux jobs run as `runner`. Configured cache paths are made writable
by that user after the bootstrap verifies that they target the Buildkite cache
volume. Prefer narrowly scoped paths.
For example, caching an entire Gradle User Home also persists `init.d` scripts
and other executable configuration, increasing the impact of cache poisoning.
Caching only `caches` and `wrapper` reduces that exposure, but a cache-volume
miss does not provide setup-gradle's archive-cache fallback once the mounted
`caches` directory exists.

Cache volumes are best-effort accelerators, scoped to the Buildkite pipeline
and cluster. They commit after successful jobs and are abandoned after failed
jobs. Do not use them as durable or trusted storage. See [Buildkite cache
volumes](https://buildkite.com/docs/agent/buildkite-hosted/cache-volumes).

### Select workflows

Pass every workflow path explicitly:

```sh
buildkite-gha upload -- \
  .github/workflows/ci.yml \
  .github/workflows/release.yml
```

Every operand must name one regular `.yml` or `.yaml` file. For multiple
workflows, every path must be tracked inside the repository. Upload
canonicalizes, deduplicates, and sorts aliases, so argument order does not
change the pipeline.

Directories, globs, missing files, other extensions, and symlinks fail before
parsing or Buildkite commands run. Multiple-workflow uploads also reject
untracked and outside paths.

`--` ends option parsing. Use it before any externally supplied paths, and
always when a path begins with `-`. Pass each path as its own argument; the CLI
does not split one shell string or decode a JSON or YAML list.

Upload is atomic. Compiled workflows become groups; skipped workflows become
top-level skipped steps. Reusable-only files remain available to local callers
but do not create groups. Selecting only reusable workflows is an error.

An explicit non-empty workflow `run-name` appends ` — <run-name>` to its group
label after resolving supported `github` and `inputs` expressions. Workflow
names, provider-check names, and the Buildkite build message remain unchanged.

See [Aggregate workflow upload](compatibility.md#aggregate-workflow-upload) for
group labels, provider checks, and failure behavior.

A safe compilation or trigger-translation error replaces only that workflow
with a failing top-level step. Compilation continues for later workflows.
Parse, event-input, admission, artifact, and upload failures abort the complete
transaction; no partial pipeline is uploaded.

### Select the effective event

Event source precedence is:

1. `--event-path`
1. `buildkite:webhook` metadata reserved by Buildkite
1. A reduced snapshot derived from `BUILDKITE_*` variables when no linked webhook is available

An explicit path never reads Buildkite metadata. Webhook metadata must be one
valid JSON object no larger than 25 MiB. Malformed, unreadable, or oversized
data stops upload instead of falling back. Buildkite's repository mapping,
commit, and ref remain authoritative.

Raw webhook data is not retained in generated plans or pipeline YAML and cannot grant queues, secrets, or tokens.

The selected snapshot establishes one event for applicability, compilation,
group conditions, provider-check names, and explicit run-name evaluation. An
explicit event is never replaced with live Buildkite fields.

Linked webhook data can provide `merge_group` and `release`. Those events need
matching Buildkite refs, commits, and activity. Release also needs a valid
payload and a tag matching `BUILDKITE_TAG` and `BUILDKITE_BRANCH`. The GitHub
Code Access App provides immutable server provenance and is required for hosted
release `GITHUB_TOKEN` issuance.

See [Names and triggers](compatibility.md#names-and-triggers) for exact matching
rules and the environment fallback.

A top-level workflow that does not declare the event becomes a skipped step with
no plan artifacts. If none apply, upload succeeds with a skipped-only pipeline.

For an applicable workflow, only the selected event contributes a group
condition. Supported branch, tag, path, base-branch, and activity filters add
their constraints. Conditions from different events are never combined.

Unsupported or uncertain filters replace only the affected workflow with a
failing step. Push and pull-request path filters need a linked webhook and a
matching local checkout. Generated or explicit snapshots can report that need,
but cannot grant admission. Malformed event data stops the import.

Buildkite owns schedule identity, so every `on.schedule` workflow is eligible
for every Buildkite scheduled build.

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
to override the default for a configured profile. An explicit mapping declares
that its selector runs on Linux x86-64, except for the known macOS labels, and
bypasses Agent API resolution. The Agent API owns compatibility and returns the
complete target for every other selector. The importer publishes returned
warnings as annotations. See [Compatibility](compatibility.md#job-configuration)
for runner behavior. Runtime distribution paths must be absolute executables.
The importer's platform defaults to its running executable; the other platform
has no direct-upload default.
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

In Buildkite jobs, the importer and runtime send best-effort completion
telemetry through the job-authenticated Agent API. Events contain the command,
outcome, client version, duration, and bounded diagnostic codes and severities.
For an unsuccessful command, they also contain the final 1,024 bytes of
normalized user-visible error output. `error_message_truncated` says whether
earlier output was omitted.

Buildkite adds organization, pipeline, build, and job identifiers on the
server. The client does not send workflow or event content, environment
variables, command text, or secrets as separate properties.

Error output can include details already printed in the job log, such as
workflow paths, action references, expressions, or invalid configuration
values. Avoid putting secrets in error messages. Disable telemetry when this
diagnostic context must stay inside the job.

Set `BUILDKITE_GHA_TELEMETRY_DISABLED=true` to disable telemetry. Missing Agent endpoint, job ID, or job token also disables it. Telemetry failures do not change command results.
