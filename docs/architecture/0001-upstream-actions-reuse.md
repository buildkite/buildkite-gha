# ADR 0001: Use actionlint as the parser; keep runners as references

- Status: Accepted
- Date: 2026-07-22
- Decision owners: `buildkite-gha` maintainers

## Context

`buildkite-gha` needs to understand GitHub Actions syntax without inheriting a
GitHub Actions runner. Its durable interfaces are its own workflow model and
versioned Buildkite job plan.

Using a runner's execution model inside the compiler would couple parsing,
policy, action resolution, and execution to assumptions that do not fit
Buildkite.

## Decision

- Use the pinned [`actionlint`](https://github.com/rhysd/actionlint) module for
  workflow and expression parsing, source positions, and useful static checks.
- Convert parsed values immediately into types owned by this repository.
  Actionlint types must not enter compiler IR or job plans.
- Implement matrix expansion, expression evaluation, action resolution,
  workflow commands, environment files, and execution in owned packages.
- Use [`nektos/act`](https://github.com/nektos/act) and the
  [GitHub Actions runner](https://github.com/actions/runner) as behavioral
  references and sources of test cases, not production dependencies.
- Do not maintain an upstream fork. A release-blocking incompatibility that
  cannot be handled by an adapter or upstream contribution requires a new ADR.

## Consequences

- Parser upgrades require the parser-adapter tests and workflow corpus to pass.
- Compatibility is judged by observable behavior, not by sharing runner code.
- The production dependency graph stays smaller and avoids act's runner,
  Docker, Git, and mutable execution model.
- Actionlint and its transitive dependencies remain part of the shipped Go
  module graph and must be included in licensing review.

The [compatibility reference](../compatibility.md) remains the source of truth
for supported syntax and behavior.
