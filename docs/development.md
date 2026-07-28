# Development and release guide

## Set up the toolchain

The repository pins Go and all lint and release tools with mise:

```sh
mise trust mise.toml
mise install
```

Run the complete repository check with:

```sh
mise run check
```

`check` verifies formatting, builds the commands, runs standard and
race-enabled tests, runs `go vet`, golangci-lint, and shellcheck, validates the
signed plan-envelope fixtures, checks deterministic smoke compilation, and
validates the release configuration. `make check` is a convenience alias.

Focused tasks are also available:

```sh
mise run format
mise run build
mise run test
mise run test:race
mise run lint:go
mise run lint:shell
mise run vet
mise run plan-fixtures
```

## Understand the smoke lanes

The smoke inventory deliberately separates three kinds of evidence.

### Network-free compilation

```sh
mise run smoke:local
```

This validates the manifest and workflow syntax, then compiles each supported
fixture twice to check deterministic output. Expected-negative fixtures are
also checked. A pass is compile-time evidence only; it does not prove that a
workflow executes successfully.

See [`testdata/smoke/README.md`](../testdata/smoke/README.md) for the inventory
and the precise meaning of each expectation.

### Production-policy preflight

```sh
mise run smoke:profile
```

This opt-in networked lane anonymously resolves selected public actions and
applies the same `hosted-tokenless` admission policy as production `upload`.
It does not install Node or run action code. An `admitted` result is policy
evidence, not runtime evidence.

Known artifact actions compile but fail admission. The pinned cache action is
admitted when the experimental cache backend is enabled, but remains
`compile-pass` until a production-backed cross-build restore is demonstrated.
Job and service container fixtures have separate hosted runtime evidence but
remain outside production admission.

### Hosted runtime proofs

Run all implemented hosted proofs against one exact commit:

```sh
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env SMOKE_PROBE=hosted --env SMOKE_COMMIT="$commit" --yes
```

The aggregate retains each phase's importer and continuation topology. Use a
phase selector only for targeted diagnosis:

| Coverage | Build environment |
| --- | --- |
| Sequential shell and upload | `PHASE2_PROBE=upload`, `PHASE2_COMMIT=<commit>` |
| GitHub Cache v1 miss/save | `PHASE2_PROBE=upload`, `PHASE2_WORKFLOW=cache`, `PHASE2_COMMIT=<commit>` |
| Concurrent step controls | `PHASE3_PROBE=concurrent`, `PHASE3_COMMIT=<commit>` |
| Public JavaScript/composite actions | `PHASE4_PROBE=actions`, `PHASE4_COMMIT=<commit>` |
| Hosted Docker prerequisites | `PHASE5_PROBE=capabilities`, `PHASE5_COMMIT=<commit>` |
| Dockerfile action path | `PHASE5_PROBE=docker-action`, `PHASE5_COMMIT=<commit>` |
| Complete container runtime | `PHASE5_PROBE=runtime`, `PHASE5_COMMIT=<commit>` |

The cache selector runs pinned `actions/cache@v4`. A first run must miss,
create `deterministic-cache-payload-v1`, and save it from the action's post
phase. A later run against a durable production backend must hit and restore
the payload before the smoke classification can become `runtime-pass`.

The phase definitions under `.buildkite/` and the [active
plan](plans/2026-07-22-buildkite-gha.md#current-progress) document the exact
coverage, differential oracles, commits, and historical Buildkite evidence.

## Architecture and plans

- [ADR 0001](architecture/0001-upstream-actions-reuse.md) explains why the
  compiler uses actionlint while act and the official runner remain behavioral
  references.
- [ADR 0002](architecture/0002-plan-envelope-trust-boundary.md) preserves the
  superseded Phase 0 signing experiment and its conformance history.
- The [active plan](plans/2026-07-22-buildkite-gha.md) tracks implementation
  phases, evidence, future UX, and deferred decisions. Current user-facing
  behavior belongs in the [README](../README.md) and [compatibility
  guide](compatibility.md), not only in the plan.

## Publish a release

From a clean, up-to-date `main`, run:

```sh
mise run release
```

The script runs `check`, chooses the next conventional-commit-derived v0 tag,
and pushes it. The tag webhook build reruns repository checks, creates the
GitHub release, and uploads the Linux x86-64 archive and checksum. Publication
can be retried from the same tag build; existing draft assets are replaced.

The source repository must be public before the first tag is pushed because the
companion plugin intentionally downloads release assets without a GitHub token.

### Release credential policy

The pipeline tag condition prevents accidental publication, but it is not a
secret boundary: pull request code can upload arbitrary Buildkite steps. Store
a fine-grained GitHub token with repository-scoped Contents read/write access as
the `GHA_GITHUB_RELEASE_TOKEN` Buildkite Secret, restricted to webhook-created
`v*` tag builds in the release pipeline:

```yaml
- pipeline_id: "019f8835-5873-4a64-850e-ba117a339d87"
  build_source: "webhook"
  build_branch: "v*"
```

Buildkite records a tag webhook's tag name as the build branch, so these claims
exclude ordinary branch and pull request webhook builds. The publisher also
verifies that the upstream tag, checked-out commit, and Buildkite commit agree
before retrieving the token. Never expose this token through a shared agent
environment hook.
