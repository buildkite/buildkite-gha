# buildkite-gha

Run GitHub Actions workflows as native Buildkite jobs without creating a GitHub Actions run.

`buildkite-gha` turns each supported workflow job and static matrix entry into a Buildkite job. Steps run in a compatibility runtime inside that job. Buildkite owns scheduling, logs, retries, cancellation, and the build UI.

> [!IMPORTANT]
> `buildkite-gha` is an experimental pre-1.0 preview. The released plugin path supports Linux x86-64 and native macOS arm64. The production path supports local and public actions, static Buildkite job-accessible secrets, and narrowly scoped, job-bound checkout, `GITHUB_TOKEN`, OIDC, artifact, and cache integrations. Private actions and GitHub-issued OIDC claims are unsupported.

## How it works

Buildkite creates the build. The plugin reads the workload from the workflow file and dynamically uploads the jobs it supports.

| GitHub Actions | Buildkite |
| --- | --- |
| Triggers and filters under `on:` | Select applicable workflow groups inside an existing Buildkite build |
| Workflow run | Existing Buildkite build |
| Job | Buildkite command job |
| Matrix entry | Buildkite command job |
| `needs` | `depends_on` with verified result transport |
| Step | Runs inside the job compatibility runtime |
| `runs-on` | Supported platform label; Buildkite queue mapping chooses the agent |

Steps stay together because they share a workspace, environment changes, action state, and post-action cleanup. Buildkite still creates the build; `buildkite-gha` selects top-level workflows for its effective GitHub event before compiling them. Local `workflow_call` remains available for composition without creating its own group.

## Run an existing workflow

When a GitHub Actions Pipeline Trigger creates the build, let Buildkite select
the matching workflow:

```yaml
steps:
  - label: ":github:"
    plugins:
      - github-actions#latest: {}
```

Buildkite supplies the selected workflow identity in `GITHUB_WORKFLOW_REF` and
marks the selection with `BUILDKITE_GITHUB_WORKFLOW_PATH`. The plugin derives
the path from the preferred GitHub identity, then reads, evaluates, and compiles
that tracked file from the checked-out commit. Use `plugins` (plural) and the
explicit empty mapping shown above. This server-selected form does not require a
step `key`; plugin steps with an explicit workflow selector still require one.

For other Buildkite builds, configure the
[GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin)
with one or more workflows:

```yaml
steps:
  - label: ":github: Test"
    key: "gha-ci"
    plugins:
      - github-actions#latest:
          workflow: .github/workflows/ci.yml

  - label: ":rocket: Deploy"
    depends_on: "gha-ci"
    command: .buildkite/deploy.sh
```

The plugin is a thin wrapper around the hidden `buildkite-gha plugin` entrypoint. It uses mise to install and verify the selected CLI release, and defaults to the latest stable release. During the preview, leaving `version` unset means there is no CLI version to update as new stable releases ship. Ordinary plugin use requires `workflow` with one explicit path or `workflows` with an explicit path list; plugin configuration does not accept directories or glob patterns. If a Pipeline Trigger-selected path and explicit configuration coexist, the explicit configuration takes precedence.

For `on.release`, open the pipeline's GitHub settings, select **Additional Webhooks** > **Releases**, and use **Code** trigger mode. Only explicit `types` containing `published`, `created`, and/or `released` are supported.

The importer can run on Linux x86-64 or native macOS arm64. Its agent targeting
is independent of `runners`: each runner mapping selects the queue for generated
workflow jobs, not the importer step.

To hold the CLI at a specific release instead, set `version` to an exact stable release from `0.9.0` onward:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/ci.yml
      version: "0.10.1"
```

Runtime v0.9.0 adds `runner.os` and `runner.arch`. They resolve to `Linux` and
`X64` on Linux and `macOS` and `ARM64` on native macOS. You can configure a
fallback queue for a macOS runner label:

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
        - runs-on: macos-14
          queue: macos-sonoma-arm64
```

Hosted runner labels are case-insensitive. Linux labels use the matching Noble
or Jammy hosted-toolchains image, with or without a configured queue.
During upload, the Agent API returns complete targets for selectors without an
explicit mapping. The importer annotates heuristic fallback warnings. See the
[cache-volume configuration](docs/cli.md#configure-generated-job-cache-volumes)
and [compatibility guide](docs/compatibility.md#job-configuration).

The imported workflows are a dynamic part of the Buildkite pipeline. The plugin creates one aggregate group per successfully compiled, selected workflow in a single transaction. Workflows that do not declare the selected event become top-level skipped steps. Explicit-selector groups and replacement steps depend on the keyed importer; keyless server-selected steps upload artifacts before the generated pipeline. Each runnable job publishes a provider check named `<workflow> / <job> (<event>)`: a GitHub check for GitHub events or an Origin check for Origin events. This approach lets you keep existing workflows while moving jobs to native Buildkite steps over time.

Buildkite owns build creation and schedule configuration. Within that build, `buildkite-gha` maps push, pull request, merge queue, release, issue, manual/API, and scheduled builds to `push`, `pull_request`, `merge_group`, `release`, `issues`, `workflow_dispatch`, and `schedule`, then applies the matching `on:` branch, tag, base-branch, activity, and safely evidenced path filters. Cross-event workflows are excluded before event-dependent compilation and retained as top-level skipped steps.

## Check workflow compatibility

The [compatibility reference](docs/compatibility.md) is the source of truth. Use this table for a quick assessment:

| Good fit | Not currently supported |
| --- | --- |
| Linux x86-64 and native macOS arm64 jobs using `bash`, `sh`, `python`, or an installed custom shell | Windows, Linux arm64, or macOS x86-64 |
| Local and public JavaScript and composite actions; verified Dockerfile and public prebuilt-image actions on Linux | Private actions, private reusable workflows, private container images, and Docker actions on macOS |
| Static matrices, `needs`, outputs, and local or literal public reusable workflows | Dynamic reusable calls, matrices, and expressions outside the documented subset |
| Exact-commit checkout, including managed private repository access | GitHub environment secrets, GitHub-issued OIDC claims, or protected queues |
| Static Buildkite job-accessible secrets, including declared aliases in local reusable workflows | Dynamic secret access or remote reusable-workflow secret forwarding |
| Scoped `GITHUB_TOKEN` and step `github.token` use allowed by Buildkite policy | Ambient token injection or dynamic token access |
| Buildkite OIDC tokens through host JavaScript and composite actions | OIDC in Docker actions or job containers |
| Audited artifact action versions and cache v6 integration | Other artifact and cache modes or general GitHub service emulation |
| Background, wait, cancellation, and parallel step controls; Linux job and service containers | Implicit GHCR authentication, container hooks, or any Docker use on macOS |

Some features support a limited subset or behave differently on Buildkite. Check the matrix before migrating a workflow.

## Use OIDC with AWS

Imported workflows receive Buildkite-issued OIDC tokens, not GitHub-issued
tokens. An AWS role that trusts only GitHub's issuer or matches GitHub's `sub`
claim rejects them. To use an existing role from both systems:

1. Register `https://agent.buildkite.com` as another IAM OIDC provider with
   audience `sts.amazonaws.com`.
1. Add a separate trust-policy statement for the provider ARN
   `arn:aws:iam::AWS_ACCOUNT_ID:oidc-provider/agent.buildkite.com`.
1. Match `agent.buildkite.com:aud` and `agent.buildkite.com:sub` instead of the
   equivalent `token.actions.githubusercontent.com` condition keys. Scope the
   Buildkite subject to the intended organization and pipeline.
1. Keep the existing GitHub provider statement while workflows run in both
   systems. Remove it only when nothing still uses GitHub-issued tokens.

For example, the Buildkite statement can use these conditions:

```json
{
  "Effect": "Allow",
  "Principal": {
    "Federated": "arn:aws:iam::AWS_ACCOUNT_ID:oidc-provider/agent.buildkite.com"
  },
  "Action": "sts:AssumeRoleWithWebIdentity",
  "Condition": {
    "StringEquals": {
      "agent.buildkite.com:aud": "sts.amazonaws.com"
    },
    "StringLike": {
      "agent.buildkite.com:sub": "organization:ORGANIZATION_SLUG:pipeline:PIPELINE_SLUG:*"
    }
  }
}
```

The workflow must grant `id-token: write`. The endpoint is available to host
JavaScript and composite actions, including `aws-actions/configure-aws-credentials`.
Use the plugin's `oidc` block to add claims, AWS session tags, or replace the
default compound subject for every token minted by imported jobs:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/deploy.yml
      oidc:
        claims: [organization_id]
        aws-session-tags: [organization_slug, pipeline_id]
        subject-claim: pipeline_id
```

This configuration does not grant OIDC access. Each workflow job must still
declare `permissions: {id-token: write}` to receive the endpoint.
See Buildkite's [AWS setup guide](https://buildkite.com/docs/pipelines/security/oidc/aws)
for the complete IAM configuration and [OIDC claims reference](https://buildkite.com/docs/agent/cli/reference/oidc#claims)
for the full subject format and available claims.

## Validate a workflow

Check syntax, the static job graph, and every declared trigger without contacting Buildkite or executing workflow code:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

This event-independent result does not claim hosted admission.

To resolve public actions and apply the production upload policy, provide an event snapshot:

```sh
buildkite-gha validate \
  --profile hosted \
  --event-path .buildkite/events/current.json \
  .github/workflows/ci.yml
```

For a quick push compatibility check, generate a minimal event snapshot:

```sh
buildkite-gha validate \
  --profile hosted \
  --event push \
  .github/workflows/ci.yml
```

`--event` also supports `pull_request`, `merge_group`, `release`, `issues`, `workflow_dispatch`, and `schedule`. Generated release validation uses one stable `published` snapshot, and generated issues validation uses `opened`. These are representative static validations, not proof of every activity. Generated snapshots are not equivalent to real payloads; use `--event-path` when exact payload data matters.

Use `--all-events` to evaluate every declared supported event separately:

```sh
buildkite-gha validate \
  --profile hosted \
  --all-events \
  .github/workflows/ci.yml
```

JSON output uses `processing-report/v3` to retain each event's result.

An `admitted` result means the workflow satisfies upload policy. A `not-applicable` result means the workflow does not declare the selected event and upload would skip it without compiling it. Validation does not execute the workflow or prove that arbitrary action code works without GitHub services. Use `--format json` for machine-readable output.

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
