# buildkite-gha

Run a GitHub Actions workflow as native Buildkite jobs—without creating a
GitHub Actions run.

`buildkite-gha` translates each workflow job (and each static matrix entry)
into a Buildkite job, then runs that job's Actions steps in a compatibility
runtime. Buildkite remains the source of truth for scheduling, logs, retries,
cancellation, and the build UI.

> [!IMPORTANT]
> This is an experimental v0.1 preview for **public, tokenless, Linux x86-64
> workflows**. It deliberately rejects workflows that need private source,
> secrets, provider tokens, or other protected capabilities.

## Try an existing workflow

Add the [GitHub Actions Buildkite
plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin)
to your Buildkite `pipeline.yml`:

```yaml
steps:
  - label: ":github: Test"
    key: "gha-ci"
    plugins:
      - github-actions#aca805f6eb2965201d4edaa57f3eec8ef9ea7ccb:
          workflow: .github/workflows/ci.yml
```

The companion plugin has not published a `v0.1.0` tag yet, so this preview
example pins its reviewed initial commit. Use `github-actions#v0.1.0` after that
tag is published; do not replace the pin with a floating branch.

The plugin downloads and verifies `buildkite-gha` v0.1.0 by default, derives the
event context from the Buildkite build, and uploads the generated jobs to the
fixed `hosted` queue.

Configure push, pull request, schedule, and manual triggers in Buildkite.
The workflow's `on:` block describes GitHub Actions events; it does not create
or change Buildkite triggers.

### Mix imported and native jobs

The imported workflow is an ordinary dynamic part of the Buildkite pipeline.
A native job can depend on the importer and will wait for the jobs it uploads:

```yaml
steps:
  - label: ":github: Test"
    key: "gha-ci"
    plugins:
      - github-actions#aca805f6eb2965201d4edaa57f3eec8ef9ea7ccb:
          workflow: .github/workflows/ci.yml

  - label: ":rocket: Deploy"
    key: "deploy"
    depends_on: "gha-ci"
    command: .buildkite/deploy.sh
```

This gives teams a migration path: start with the existing Actions workflow,
then move work into native Buildkite jobs over time. Automatic replacement of
a named imported job is planned, but is not part of this preview.

## Is my workflow a fit?

The plugin path currently supports:

- Linux Bash and `sh` steps;
- JavaScript, composite, local, and anonymous public actions;
- supported local and public Dockerfile actions;
- static matrices, `needs`, conditions, outputs, and local reusable workflows;
- background, wait, cancellation, and parallel step controls;
- timeouts, `continue-on-error`, masking, and pre/main/post actions; and
- public, credential-free checkout of the event repository at its exact commit.

It does **not** currently support:

- private repositories or private actions;
- workflow secrets, `GITHUB_TOKEN`, GitHub-compatible OIDC, or protected queues;
- `actions/cache`, `actions/upload-artifact`, or `actions/download-artifact`;
- publishing `GITHUB_STEP_SUMMARY` content to the Buildkite UI;
- job containers or service containers through the production plugin path;
- privileged containers, arbitrary Docker options, or `docker://` actions; or
- Windows or macOS jobs.

The underlying runtime has broader container coverage than the production
plugin currently exposes. See the [compatibility and CLI guide](docs/compatibility.md)
for the exact distinction and intentional behavior differences.

## Check before running

Static validation does not contact Buildkite or execute the workflow:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

For example, a workflow with one producer and a two-entry consumer matrix
reports:

```text
Workflow: .github/workflows/ci.yml
Result: compilable
✓ 2 logical jobs and 3 static instances compile
```

To also resolve public actions and apply the same policy as the plugin's
`hosted-tokenless` upload, provide an event snapshot:

```sh
buildkite-gha validate \
  --profile hosted-tokenless \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

An `admitted` result means the plans satisfy upload policy. It does not execute
the workflow or prove that arbitrary action code is independent of GitHub-only
services. JSON output is available with `--format json`.

## What gets translated?

| GitHub Actions | Buildkite |
| --- | --- |
| Workflow run | Build |
| Job | Command job |
| Matrix entry | Command job with a stable key |
| `needs` | `depends_on` plus verified result transport |
| `runs-on` | Fail-closed queue policy |
| Job output | Producer-attributed result artifact |
| Step | Runs inside the job compatibility runtime |

Steps are intentionally **not** translated into separate Buildkite jobs. They
share a workspace, environment changes, action state, containers, and
post-action lifecycle in GitHub Actions, so they must stay inside one job here
too.

```diagram
┌──────────────────────────┐
│ Actions workflow + event │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Validate and compile     │
│ jobs, matrices, policy   │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Native Buildkite jobs    │
│ one runtime per GHA job  │
└────────────┬─────────────┘
             ▼
┌──────────────────────────┐
│ Buildkite logs + results │
└──────────────────────────┘
```

## Documentation

- [Compatibility, behavior differences, and direct CLI use](docs/compatibility.md)
- [Development, smoke tests, and releases](docs/development.md)
- [Active product and implementation plan](docs/plans/2026-07-22-buildkite-gha.md)
- [Architecture decisions](docs/architecture/)

Use `buildkite-gha help`, `buildkite-gha help <command>`, or
`buildkite-gha --version` for the exact installed command surface.

## License

MIT. See [LICENSE](LICENSE).
