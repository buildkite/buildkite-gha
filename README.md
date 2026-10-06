# buildkite-gha

Run GitHub Actions workflows as native Buildkite jobs without creating a GitHub Actions run.

`buildkite-gha` turns each supported workflow job and matrix entry into a Buildkite job. Steps run in a compatibility runtime inside that job. Buildkite owns scheduling, logs, retries, cancellation, and the build UI.

> [!IMPORTANT]
> `buildkite-gha` is an experimental pre-1.0 preview. The released plugin runs generated jobs on Linux x86-64, Linux arm64 with an explicit queue, and native macOS arm64. [Windows Server 2022 x86-64 jobs](docs/compatibility.md#experimental-windows-jobs) are experimental and need an explicit opt-in. Private actions and GitHub-issued OIDC claims are unsupported.

## How it works

Buildkite creates the build. The plugin reads the workload from the workflow file and dynamically uploads the jobs it supports.

| GitHub Actions | Buildkite |
| --- | --- |
| Triggers and filters under `on:` | Select applicable workflows inside an existing Buildkite build |
| Workflow run | Existing Buildkite build |
| Job | Buildkite command job |
| Matrix entry | Buildkite command job |
| Matrix or runner label from a job output | Jobs uploaded by a deferred step after the producing job runs |
| `needs` | `depends_on` with verified result transport |
| Step | Runs inside the job compatibility runtime |
| `runs-on` | Supported platform label; a Buildkite queue mapping chooses the agent |
| Deployment environment with required reviewers | Buildkite block step before the job |

Steps stay together because they share a workspace, environment changes, action state, and post-action cleanup. Local `workflow_call` workflows remain available to callers without creating their own group.

## Run an existing workflow

Configure the [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin) with one or more workflows:

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

The plugin is a thin wrapper around the hidden `buildkite-gha plugin` entry point. It uses mise to install and verify the CLI, and defaults to the latest stable release. Use `workflow` for one explicit path or `workflows` for an explicit path list; directories and glob patterns are not accepted.

The plugin creates one group per selected workflow in a single upload transaction. Workflows that do not declare the build's event become skipped steps. When several workflows are selected, one that fails to compile becomes a failing step without blocking the others. Each runnable job publishes a provider check named `<workflow> / <job> (<event>)`. See [Aggregate workflow upload](docs/compatibility.md#aggregate-workflow-upload).

Common plugin options:

| Option | Default | Purpose |
| --- | --- | --- |
| `version` | `latest` | Exact stable CLI release, from `0.9.0` onward. |
| `source-ref` | — | Full lowercase 40-character `buildkite-gha` commit SHA to build for development testing. Mutually exclusive with `version`. |
| `runners` | — | Map `runs-on` labels to queues, images, and cache volumes. |
| `experimental-runner-user` | `true` | Run generated Linux jobs as a dedicated `runner` user. See [non-root jobs](docs/cli.md#run-linux-jobs-as-a-non-root-user). |
| `private-reusable-workflows` | `false` | Allow reusable workflows from private repositories the importer can read. See [Reusable workflows](docs/compatibility.md#reusable-workflows). |
| `oidc` | — | Add Buildkite OIDC claims and AWS session tags. See [Use OIDC with AWS](#use-oidc-with-aws). |

The [plugin README](https://github.com/buildkite-plugins/github-actions-buildkite-plugin#readme) lists every option and agent requirement.

To hold the CLI at a specific release, set `version`:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/ci.yml
      version: "0.97.2"
```

### Map runner labels to queues

Each runner mapping selects the queue for generated workflow jobs, not the importer step. The plugin's importer runs on Linux x86-64 or native macOS arm64; a [custom importer](docs/cli.md#upload-from-a-custom-importer) can also run on Linux arm64.

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
        - runs-on: ubuntu-24.04-arm
          queue: my-linux-arm64-queue
        - runs-on: macos-14
          queue: macos-sonoma-arm64
        - runs-on: windows-2022
          queue: my-windows-queue
```

| Platform | Labels | Queue | `runner.os` / `runner.arch` |
| --- | --- | --- | --- |
| Linux x86-64 | `ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04` | Agent API or local preset | `Linux` / `X64` |
| Linux arm64 | `ubuntu-24.04-arm`, `ubuntu-22.04-arm`, labels ending in `-arm64` or `-aarch64` | Explicit mapping only | `Linux` / `ARM64` |
| macOS arm64 | `macos-latest`, `macos-15`, `macos-14` | Agent API, local preset, or explicit mapping | `macOS` / `ARM64` |
| Windows x86-64 (experimental) | `windows-latest`, `windows-2022` | Explicit mapping or enabled Agent API routing | `Windows` / `X64` |

Runner labels are case-insensitive. An explicit mapping is authoritative; the importer checks its queue through the Agent API before upload. For unmapped labels, the Agent API selects a native Linux host or an immutable image and may return a fallback warning, which the importer annotates. Labels select a platform; they do not provide GitHub's runner image or tools. See [Job configuration](docs/compatibility.md#job-configuration) and [cache volumes](docs/cli.md#configure-generated-job-cache-volumes).

### Choose the triggering event

Buildkite owns build creation and schedule configuration. Within a build, `buildkite-gha` selects one effective event, then applies the matching `on:` filters:

| Buildkite build | GitHub Actions event |
| --- | --- |
| Branch or tag push, UI, or API build | `push` |
| Pull request | `pull_request` |
| Merge queue | `merge_group` |
| Release | `release` (`published`, `created`, and `released`) |
| Issue | `issues` |
| Scheduled build | `schedule` |
| Explicit event snapshot or authoritative event name | `workflow_dispatch` |

GitHub Actions Pipeline Triggers are in private preview. They select the workflow on the server, so the plugin needs no `workflow` selector, and support more events, including all seven `release` activities, tokenless `merge_group`, `deployment`, `deployment_status`, `issue_comment`, `pull_request_review`, `pull_request_review_comment`, and repository lifecycle events such as `create`, `delete`, `label`, and `discussion`. See [Private-preview Pipeline Trigger selection](docs/cli.md#private-preview-pipeline-trigger-selection).

`pull_request_target` is intentionally unsupported. [Names and triggers](docs/compatibility.md#names-and-triggers) lists every event and filter.

When the build has linked webhook data, `GITHUB_EVENT_PATH` points to the complete event payload. See [Event file](docs/compatibility.md#event-file).

## Check workflow compatibility

The [compatibility reference](docs/compatibility.md) is the source of truth. Use this table for a quick assessment:

| Good fit | Not currently supported |
| --- | --- |
| Linux x86-64, explicitly queued Linux arm64, and native macOS arm64 jobs; [experimental Windows x86-64 jobs](docs/compatibility.md#experimental-windows-jobs) | Windows arm64, Windows Server 2025, or macOS x86-64 |
| Bash, `sh`, PowerShell, Python, and custom shell steps | `cmd` shells |
| Local, public, and self-repository (`$/`) JavaScript and composite actions; verified Dockerfile and public prebuilt-image actions on Linux | Private actions, private container images, and Docker actions on macOS or Windows |
| Static matrices, matrices and runner labels from job outputs, `needs`, outputs, and local, public, or opt-in private reusable workflows | Dynamically selected reusable workflows and expressions outside the documented subset |
| Exact-commit checkout, including managed private repository access | Alternate-repository checkout, GitHub-issued OIDC claims, or protected queues |
| Deployment environments with required-reviewer approval gates and environment secret names; repository, organization, and environment variables | Environment wait timers, branch policies, custom protection rules, or deployment records |
| Static Buildkite secrets, including declared aliases in local reusable workflows | Dynamic secret access or remote reusable-workflow secret forwarding |
| Scoped `GITHUB_TOKEN` and step `github.token` use allowed by Buildkite policy | Ambient token injection or dynamic token access |
| Buildkite OIDC tokens through host JavaScript and composite actions | OIDC in Docker actions or job containers |
| Supported artifact action versions and cache v6 integration | Other artifact and cache modes or general GitHub service emulation |
| Background, wait, cancellation, and parallel step controls; Linux job and service containers | Implicit GHCR authentication, container hooks, or containers on macOS or Windows |

Some features support a limited subset or behave differently on Buildkite. Check the reference before migrating a workflow.

## Move secrets and configuration

Imported workflows read Buildkite secrets, not GitHub secrets:

- `${{ secrets.NAME }}` resolves a statically named [Buildkite secret](docs/compatibility.md#other-secrets-and-oidc) available to the job.
- GitHub's API cannot export secret values. [`buildkite-gha migrate-secrets`](docs/cli.md#migrate-github-actions-secrets) generates a reviewable, one-use GitHub Actions workflow that copies selected repository secrets into Buildkite.
- A job's [deployment environment](docs/compatibility.md#deployment-environments) maps environment secret `NAME` to the Buildkite secret `<ENVIRONMENT>_<NAME>`, for example `PRODUCTION_DEPLOY_KEY`.
- `${{ vars.NAME }}` resolves [repository, organization, and environment variables](docs/compatibility.md#repository-and-organization-variables) from GitHub. Variable values are visible to anyone who can read the build's artifacts.
- `GITHUB_TOKEN` is never migrated. Buildkite issues a [scoped token](docs/compatibility.md#github-token) when the organization and pipeline enable it.

## Use OIDC with AWS

Imported workflows receive Buildkite-issued OIDC tokens, not GitHub-issued tokens. An AWS role that trusts only GitHub's issuer or matches GitHub's `sub` claim rejects them. To use an existing role from both systems:

1. Register `https://agent.buildkite.com` as another IAM OIDC provider with audience `sts.amazonaws.com`.
1. Add a separate trust-policy statement for the provider ARN `arn:aws:iam::AWS_ACCOUNT_ID:oidc-provider/agent.buildkite.com`.
1. Match `agent.buildkite.com:aud` and `agent.buildkite.com:sub` instead of the equivalent `token.actions.githubusercontent.com` condition keys. Scope the Buildkite subject to the intended organization and pipeline.
1. Keep the existing GitHub provider statement while workflows run in both systems. Remove it only when nothing still uses GitHub-issued tokens.

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

The endpoint is available to host JavaScript and composite actions, including `aws-actions/configure-aws-credentials`. Use the plugin's `oidc` block to add claims, AWS session tags, or replace the default compound subject for every token minted by imported jobs:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/deploy.yml
      oidc:
        claims: [organization_id]
        aws-session-tags: [organization_slug, pipeline_id]
        subject-claim: pipeline_id
```

This configuration does not grant OIDC access. Each workflow job must still declare `permissions: {id-token: write}` to receive the endpoint. See Buildkite's [AWS setup guide](https://buildkite.com/docs/pipelines/security/oidc/aws) for the complete IAM configuration and [OIDC claims reference](https://buildkite.com/docs/agent/cli/reference/oidc#claims) for the full subject format and available claims.

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

For a quick check, generate a minimal snapshot for one event with `--event`, or evaluate every declared supported event separately with `--all-events`:

```sh
buildkite-gha validate \
  --profile hosted \
  --all-events \
  .github/workflows/ci.yml
```

Generated snapshots are representative, not real payloads; use `--event-path` when exact payload data matters.

An `admitted` result means the workflow satisfies upload policy. A `not-applicable` result means the workflow does not declare the selected event and upload would skip it without compiling it. Validation does not execute the workflow or prove that arbitrary action code works without GitHub services. Use `--format json` for machine-readable output.

See the [CLI guide](docs/cli.md#validate-a-workflow) for supported events, JSON reports, compilation, and direct upload.

## Run untrusted jobs safely

Workflow steps and third-party actions are repository code. Run imported jobs on a disposable, whole-job-isolated queue with no ambient protected credentials. Action containers do not replace that boundary.

See the [security model](docs/security.md) before enabling managed repository access, private reusable workflows, scoped write tokens, or caching.

## Documentation

- [Compatibility reference](docs/compatibility.md)
- [CLI guide](docs/cli.md)
- [Security model](docs/security.md)
- [Development and releases](docs/development.md)
- [Expression authority architecture](docs/expression-authority.md)

Use `buildkite-gha help`, `buildkite-gha help <command>`, or `buildkite-gha --version` for the installed command surface.

## License

MIT. See [LICENSE](LICENSE).
