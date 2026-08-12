# buildkite-gha

Run GitHub Actions workflows as native Buildkite jobs without creating a GitHub Actions run.

`buildkite-gha` turns each supported workflow job and static matrix entry into a Buildkite job. Steps run in a compatibility runtime inside that job. Buildkite owns scheduling, logs, retries, cancellation, and the build UI.

> [!IMPORTANT]
> `buildkite-gha` is an experimental pre-1.0 preview for Linux x86-64 workflows. The production path supports local and public actions and narrowly scoped, job-bound checkout, `GITHUB_TOKEN`, artifact, and cache integrations. Private actions, ordinary workflow secrets, and GitHub-compatible OIDC are unsupported.

## How it works

Buildkite creates the build. The plugin reads the workload from the workflow file and dynamically uploads the jobs it supports.

| GitHub Actions | Buildkite |
| --- | --- |
| Triggers and filters under `on:` | Configure these in Buildkite |
| Workflow run | Existing Buildkite build |
| Job | Buildkite command job |
| Matrix entry | Buildkite command job |
| `needs` | `depends_on` with verified result transport |
| Step | Runs inside the job compatibility runtime |
| `runs-on` | Linux compatibility check; Buildkite chooses the agent |

Steps stay together because they share a workspace, environment changes, action state, and post-action cleanup. Local `workflow_call` remains useful for workflow composition, but `on:` never creates or filters a Buildkite build.

## Run an existing workflow

Add the [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin) to your pipeline:

```yaml
steps:
  - label: ":github: Test"
    key: "gha-ci"
    plugins:
      - github-actions#main:
          workflow: .github/workflows/ci.yml

  - label: ":rocket: Deploy"
    depends_on: "gha-ci"
    command: .buildkite/deploy.sh
```

The plugin is a thin wrapper around the hidden `buildkite-gha plugin` entrypoint. It uses mise to install and verify the selected CLI release, and defaults to the latest stable release. During the preview, leaving `version` unset means there is no CLI version to update as new stable releases ship.

To hold the CLI at a specific release instead, set `version` to an exact stable release from `0.8.0` onward:

```yaml
plugins:
  - github-actions#main:
      workflow: .github/workflows/ci.yml
      version: "0.8.0"
```

The imported workflow is a dynamic part of the Buildkite pipeline. The native deploy job waits for the importer and every job it uploads. This approach lets you keep an existing workflow while moving jobs to native Buildkite steps over time.

Configure branch, tag, pull request, schedule, and manual triggers in Buildkite. The plugin uses the `pull_request` context for pull request builds and the `push` context for other builds. Filters under `on:` do not create or filter Buildkite builds.

## Check workflow compatibility

The [compatibility reference](docs/compatibility.md) is the source of truth. Use this table for a quick assessment:

| Good fit | Not currently supported |
| --- | --- |
| Linux x86-64 jobs using `bash` or `sh` | Windows, macOS, or Linux arm64 |
| Local and public JavaScript, composite, and verified Dockerfile actions | Private actions or arbitrary reusable-workflow source |
| Static matrices, `needs`, outputs, and local reusable workflows | Dynamic matrices and expressions outside the documented subset |
| Exact-commit checkout, including managed private repository access | Ordinary workflow secrets, GitHub-compatible OIDC, or protected queues |
| Scoped `GITHUB_TOKEN` use allowed by Buildkite policy | Ambient or workflow-authored `github.token` use |
| Audited artifact action versions and cache v6 integration | Other artifact and cache modes or general GitHub service emulation |
| Background, wait, cancellation, and parallel step controls | Job and service containers through the production plugin path |

Some features support a limited subset or behave differently on Buildkite. Check the matrix before migrating a workflow.

## Validate a workflow

Check syntax and the static job graph without contacting Buildkite or executing workflow code:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

To resolve public actions and apply the production upload policy, provide an event snapshot:

```sh
buildkite-gha validate \
  --profile hosted-tokenless \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

An `admitted` result means the workflow satisfies upload policy. It does not execute the workflow or prove that arbitrary action code works without GitHub services. Use `--format json` for machine-readable output.

See the [CLI guide](docs/cli.md) for event snapshots, compilation, direct upload, and agent targeting.

## Run untrusted jobs safely

Workflow steps and third-party actions are repository code. Run imported jobs on a disposable, whole-job-isolated queue with no ambient protected credentials. Action containers do not replace that boundary.

See the [security model](docs/security.md) before enabling managed repository access, scoped write tokens, or caching.

## Documentation

- [Compatibility reference](docs/compatibility.md)
- [CLI guide](docs/cli.md)
- [Security model](docs/security.md)
- [Development and releases](docs/development.md)

Use `buildkite-gha help`, `buildkite-gha help <command>`, or `buildkite-gha --version` for the installed command surface.

## License

MIT. See [LICENSE](LICENSE).
