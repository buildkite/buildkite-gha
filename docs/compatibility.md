# Compatibility and CLI guide

This guide describes the behavior available to users today. The
[active plan](plans/2026-07-22-buildkite-gha.md) also contains implemented
internals, future product ideas, and deferred decisions; those are not support
promises unless they appear here.

## The current execution profile

The plugin and `upload` command use one fixed profile: `hosted-tokenless`.
It is designed for public code that can run without protected credentials on a
Linux x86-64 Buildkite Hosted Agent. Unsupported or privileged requests fail
before the generated jobs are uploaded.

There are three different compatibility claims:

1. **Compilable** — the workflow syntax and static job graph can be translated.
2. **Admitted** — action sources resolve and the generated plans satisfy the
   `hosted-tokenless` upload policy.
3. **Runtime-proven** — the repository's conformance or hosted test suite has
   executed that behavior successfully.

Compilation alone is not admission, and admission does not execute arbitrary
action code. In particular, an otherwise valid action may depend on a GitHub
artifact, cache, token, or OIDC service that this project does not provide.

## Support matrix

| Area | Production plugin | Notes |
| --- | --- | --- |
| Linux x86-64 host jobs | Supported | `ubuntu-latest`, `ubuntu-24.04`, and `ubuntu-22.04` map to the fixed `hosted` queue. |
| Bash and `sh` steps | Supported | Includes environment and working-directory precedence. |
| Static job graphs and `needs` | Supported | Dependencies and logical results are preserved. |
| Static matrices | Supported | Includes typed values, `include`, `exclude`, and exact dependency fan-out. |
| Local reusable workflows | Supported when statically resolvable | Remote and runtime-dependent reusable workflows are deferred. |
| Job and step conditions | Supported subset | Syntax is checked, but not every runtime function or context limitation is preflighted. Some unsupported expressions fail only when the job runs. |
| Concurrent step controls | Supported | Includes `background`, `wait`, `wait-all`, `cancel`, and `parallel`. |
| JavaScript actions | Supported | Managed, digest-verified Node 20 and 24 runtimes are used. |
| Composite and local actions | Supported | Nested composites and global pre/main/post ordering are supported. |
| Anonymous public actions | Supported | Sources are resolved to immutable commits and complete trees are verified. |
| `actions/checkout` | Narrow support | Public `github.com` event repository, exact event SHA, workspace root, shallow credential-free fetch. |
| Dockerfile actions | Supported subset | Only compiler-verified local or anonymous public Dockerfile actions are admitted. |
| Step summaries | Supported | Published as job-scoped Buildkite annotations with a stable context. Requires Buildkite Agent v3.112 or newer. Oversized per-step summaries are skipped without failing the job; aggregate job summaries are bounded to 1 MiB. |
| Job and service containers | Not admitted | Implemented and runtime-proven, but still outside production `hosted-tokenless` policy. |
| `docker://` actions | Not supported | Private images, credentials, arbitrary options, volumes, and privileged containers are also rejected. |
| Artifact and cache actions | Not supported | They compile but fail profile admission until Buildkite-backed adapters exist. |
| Private repositories or actions | Not supported | The preview has no private-source capability broker. |
| Secrets and provider tokens | Not supported | Includes `GITHUB_TOKEN`, GitHub App tokens, and protected environment grants. |
| OIDC | Not supported | GitHub-compatible and migration OIDC flows are deferred. |
| Windows and macOS | Not supported | Unmapped runner labels fail validation. |

## User-visible behavior

### Buildkite owns the run

No shadow GitHub Actions run is created. Workflow jobs and matrix entries are
visible as Buildkite jobs, with Buildkite scheduling, logs, cancellation,
retries, and status.

Actions steps remain inside one compatibility runtime because they share a
workspace, environment-file updates, action state, and cleanup lifecycle.
Docker is another execution backend within that job—not a security boundary
between mutually untrusted steps. Use a disposable job VM when workflow code
must be isolated from other jobs or the agent host.

### Concurrent steps stay inside the job

Background and parallel work use a bounded, ten-active-step supervisor rather
than creating more Buildkite jobs. For example:

```yaml
steps:
  - id: lint
    run: ./scripts/lint
    background: true

  - id: test
    run: ./scripts/test
    background: true

  - wait-all:
```

A background step's outputs, environment changes, and failures become visible
at the covering `wait` or `wait-all`. Remaining work is joined by an implicit
final `wait-all` before post-action cleanup. Use `cancel: <step-id>` for targeted
cancellation or `parallel:` for a fixed inline group. These controls preserve
one job workspace and lifecycle.

### Buildkite owns triggers

Select the workflow explicitly in the plugin and configure pipeline triggers in
Buildkite. `buildkite-gha` uses the event snapshot to populate Actions contexts;
it does not subscribe to GitHub events or turn workflow `on:` entries into
Buildkite triggers.

The plugin derives `pull_request` for Buildkite pull request builds and `push`
for branch, tag, scheduled, and manual builds. It does not currently provide
`schedule` or `workflow_dispatch` contexts or dispatch inputs. A scheduled or
manual trigger is therefore compatible only when the workflow expects push
semantics. The direct CLI accepts explicit event snapshots as documented below,
but the plugin does not expose that option yet.

For pull requests, the plugin uses the exact Buildkite commit and a
`refs/pull/<number>/head` compatibility ref. It does not claim GitHub's merge-ref
semantics when Buildkite has not supplied them.

### Checkout starts clean

Generated jobs skip Buildkite's default checkout and allocate a fresh Actions
workspace. A supported `actions/checkout` step performs a credential-free,
shallow checkout of the public event repository at the exact event SHA. Private
checkout, alternate repositories or refs, and credential persistence are not
available in the preview.

### Failures stay explicit

Unsupported syntax, runner labels, action types, and protected capabilities
fail closed. The bridge does not silently choose a nearby behavior.

A runtime-skipped Actions job currently appears successful to Buildkite while
publishing its logical `skipped` result. Downstream imported jobs use that
logical result. This is a UI difference until Buildkite has a scheduler-visible
skipped job state.

If producer identity or result artifact selection becomes ambiguous—for
example, after retrying an individual producer—consumers fail closed. Retry the
whole build rather than one imported job.

Cancellation targets the complete process tree: `SIGINT`, then `SIGTERM` after
7.5 seconds, then `SIGKILL` after another 2.5 seconds. GitHub uses the same
timing for the direct process, but `buildkite-gha` deliberately avoids leaving
child processes behind.

## Use the CLI directly

The [plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin)
is the recommended installation. For direct use, download
`buildkite-gha_Linux_x86_64.tar.gz` and `checksums.txt` from the matching GitHub
[release](https://github.com/buildkite/buildkite-gha/releases), verify the
archive checksum, and extract it to a stable location. The archive contains
`buildkite-gha` and `LICENSE`.

Action workflows also require mise 2026.5.12 on the importer `PATH` when
invoking `upload` directly. The runtime uses it with repository configuration
disabled to install exact Node 20.20.2 or 24.18.0 releases. Those Node binaries
require glibc 2.28 or newer; the static Go CLI does not. `validate` and
`compile` do not install Node or require mise.

### Validate

Validate the static workflow subset without an event:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

Apply the production profile with human-readable or machine-readable output:

```sh
buildkite-gha validate --profile hosted-tokenless \
  --event-path .buildkite/events/current.json --format text \
  .github/workflows/ci.yml

buildkite-gha validate --profile hosted-tokenless \
  --event-path .buildkite/events/current.json --format json \
  .github/workflows/ci.yml
```

Profile validation may access the public network to resolve actions. It does
not call Buildkite, install Node, or execute workflow code.

### Event snapshots

`compile` and profile validation take an explicit, bounded event snapshot:

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

The snapshot supplies the compile-time Actions context. Generated plans retain
the event name, repository, ref, SHA, actor, and a payload digest, but not the
payload object itself. Runtime conditions can use those retained identity fields
but cannot currently access `github.event`; an expression such as
`github.event.action` may therefore pass validation and fail when the job runs.
The snapshot is compatibility data, not proof that the event is trustworthy,
and cannot authorize a protected capability.

### Compile

Render deterministic Buildkite pipeline YAML, or inspect the compiler IR:

```sh
buildkite-gha compile --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml

buildkite-gha compile --event-path .buildkite/events/current.json \
  --format ir-json .github/workflows/ci.yml
```

The pipeline output references content-addressed plans and the exact compiler
executable. `compile` is read-only: it does not materialize those artifacts or
upload a pipeline, so piping its output directly to a real Buildkite upload is
not a complete execution path.

### Upload

`upload` is the in-build command used by the plugin:

```sh
buildkite-gha upload --runtime-queue hosted .github/workflows/ci.yml
```

It requires `BUILDKITE=true` and `BUILDKITE_STEP_KEY`. Without `--event-path`,
it derives a bounded compatibility snapshot from the current Buildkite build.
With `--event-path`, it uses that explicit snapshot. Both paths remain
unattested and tokenless.

The command uploads the exact executable and content-addressed plans before
calling `buildkite-agent pipeline upload --no-interpolation --reject-secrets`.
The queue argument is intentionally fixed to `hosted`; it cannot be used to
select a more privileged queue.

`run-job` is an internal command emitted into generated jobs. Users should not
need to invoke it directly.
