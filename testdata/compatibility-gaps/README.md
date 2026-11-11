# Compatibility gap smoke tests

These fixtures continuously assess the 16 gaps in the linked [compatibility analysis](https://slopcannon.tail952194.ts.net/simone/gha-plugin-what-to-build-next/).

Run the assessment with:

```sh
mise run compatibility:gaps
```

The command applies the `hosted` profile to compile-time and admission gaps. It compiles and executes the two runtime shell fixtures. A passing case means its named check passed, not that the feature has full GitHub parity. A blocked case makes the command fail.

The environment case runs `TestRunUploadResolvesEnvironmentsThroughAgent` in [the CLI integration tests](../../internal/cli/environments_test.go), using that test's workflow and a simulated Agent API. It checks approval gates, environment secret-name mapping, and environment variables—not live endpoint access, secret-value retrieval, or GitHub reviewer identity parity. The test requires Linux amd64. A skipped or missing test counts as blocked, not passing.

`validate --profile hosted` does not configure an environment resolver, even inside a Buildkite job, so it cannot assess this case. `compile` and `upload` resolve environment policy through the job-scoped Agent API and fail closed when resolution is unavailable. Bare `validate` checks syntax, not deployment policy.

The runner-label fixture checks Windows compatibility without a runner mapping. It does not check live queue availability, automatic routing, or Windows execution.

The pull-request tag fixture checks admission, not positive selection. Its nonempty tag-only filter does not select pull requests. Blocking CLI and trigger tests cover this distinction and matching branch combinations; see the [native evidence contract](../../docs/compatibility-contract.md#pull-request-tag-filter-admission-and-selection).

The PowerShell fixture requires `pwsh` on `PATH` and runs the installed interpreter.

CI soft-fails the aggregate step while gaps remain. After all cases finish, the step publishes one Buildkite annotation containing every result. This keeps the assessment visible without blocking unrelated builds.
