# buildkite-gha

Run GitHub Actions workflows as native Buildkite Pipelines jobs without creating a GitHub Actions run.

`buildkite-gha` is the runtime behind GitHub Actions Compatibility. It turns each supported workflow job and matrix entry into a Buildkite Pipelines job. Steps run inside that job. Buildkite Pipelines owns scheduling, logs, retries, cancellation, and the build UI.

> [!IMPORTANT]
> GitHub Actions Compatibility is in public preview, and `buildkite-gha` is pre-1.0. The runtime supports generated jobs on Linux x86-64, Linux arm64 with an explicit queue, native macOS arm64, and [Windows Server 2022 x86-64](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#windows-jobs) with a compatible Windows queue. Private actions and GitHub-issued OIDC claims are unsupported.

## Run an existing workflow

Create a GitHub Actions pipeline in the Buildkite interface. Your repository must contain workflows under `.github/workflows/` on its default branch and use the full-access **GitHub** repository provider.

1. Select **New Pipeline**, then select your GitHub account or organization and repository.
1. Review the automatically selected **Run GitHub Actions workflows on Buildkite** setup and pipeline settings. Choose a cluster with compatible queues for your workflows.
1. Select **Create GitHub Actions pipeline**.

Buildkite Pipelines creates an enabled **GitHub Actions** pipeline trigger and provisions a dedicated signed repository webhook. This skips traditional GitHub webhook setup. The server matches supported events against workflow `on` declarations and creates one build per matching workflow. You don't need to configure a plugin, maintain workflow selectors, or install the runtime.

See the [setup guide](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows) for screenshots, requirements, secrets, supported events, and migration instructions.

For CLI, API, or agent-based provisioning, follow the [manual pipeline-and-trigger setup](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows#add-a-github-actions-workflow-to-a-pipeline-configure-an-equivalent-pipeline-manually): create the pipeline with the documented settings, then create its GitHub Actions pipeline trigger with webhook provisioning. Neither `buildkite-agent pipeline upload` nor `buildkite-gha upload` creates a pipeline, trigger, or webhook; both add jobs to an existing build.

[Explicit-workflow plugin configuration](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows#use-explicit-workflows-in-an-existing-build) remains an advanced alternative for existing builds, not the default setup for new pipelines.

## Validate a workflow locally

To check a workflow locally without contacting Buildkite or executing workflow code:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

## Documentation

User documentation lives in the [Buildkite Docs](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows):

| Guide | Covers |
| --- | --- |
| [GitHub Actions Compatibility](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows) | Native setup, manual pipeline and trigger creation, advanced plugin configuration, requirements, and troubleshooting. |
| [Compatibility reference](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility) | Supported workflow, job, step, expression, and action behavior, with limits. |
| [buildkite-gha CLI](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/cli) | Validation, event snapshots, compilation, custom importers, and telemetry. |
| [Security model](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/security) | Job isolation, credential boundaries, and the operator checklist. |
| [Migrate GitHub Actions secrets](https://buildkite.com/docs/pipelines/migration/github-actions-secrets) | Copying repository secrets into Buildkite. |

Use `buildkite-gha help`, `buildkite-gha help <command>`, or `buildkite-gha --version` for the installed command surface.

## Contributing

- [Development and releases](docs/development.md)
- [Expression authority architecture](docs/expression-authority.md)

## License

MIT. See [LICENSE](LICENSE).
