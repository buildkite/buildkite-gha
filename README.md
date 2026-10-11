# buildkite-gha

Run GitHub Actions workflows as native Buildkite jobs without creating a GitHub Actions run.

`buildkite-gha` turns each supported workflow job and matrix entry into a Buildkite job. Steps run in a compatibility runtime inside that job. Buildkite owns scheduling, logs, retries, cancellation, and the build UI.

> [!IMPORTANT]
> `buildkite-gha` is an experimental pre-1.0 preview. The released plugin runs generated jobs on Linux x86-64, Linux arm64 with an explicit queue, native macOS arm64, and [Windows Server 2022 x86-64](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows/compatibility#windows-jobs) with a compatible Windows queue. Private actions and GitHub-issued OIDC claims are unsupported.

## Run an existing workflow

Configure the [GitHub Actions Buildkite plugin](https://github.com/buildkite-plugins/github-actions-buildkite-plugin) with one or more workflows:

```yaml
steps:
  - label: ":github: Test"
    key: "gha-ci"
    plugins:
      - github-actions#latest:
          workflow: .github/workflows/ci.yml
```

To check a workflow locally without contacting Buildkite or executing workflow code:

```sh
buildkite-gha validate .github/workflows/ci.yml
```

## Documentation

User documentation lives in the [Buildkite Docs](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows):

| Guide | Covers |
| --- | --- |
| [Run GitHub Actions workflows](https://buildkite.com/docs/pipelines/migration/run-github-actions-workflows) | Setup, plugin configuration, runner mapping, requirements, OIDC with AWS, and troubleshooting. |
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
