# Development and release guide

## Set up the toolchain

The repository pins Go and all lint and release tools with mise:

```bash
mise trust mise.toml
mise install
```

Run the complete repository check with:

```bash
mise run check
```

`check` verifies formatting, builds the commands, runs standard and
race-enabled tests, runs `go vet`, golangci-lint, and shellcheck, validates the
signed plan-envelope fixtures, checks deterministic smoke compilation, and
validates the release configuration. `make check` is a convenience alias.
The standard and race-enabled suites run serially because their live container
tests inspect daemon-wide Docker resources. Local runs may skip those tests when
Docker or managed Node is unavailable; the hosted repository check requires the
live prerequisites and fails rather than silently losing Phase 5 coverage.

Focused tasks are also available:

```bash
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

```bash
mise run smoke:local
```

This validates the manifest and workflow syntax, then compiles each supported
fixture twice to check deterministic output. Expected-negative fixtures are
also checked. A pass is compile-time evidence only; it does not prove that a
workflow executes successfully.

See [`testdata/smoke/README.md`](../testdata/smoke/README.md) for the inventory
and the precise meaning of each expectation.

### Production-policy preflight

```bash
mise run smoke:profile
```

This opt-in networked lane anonymously resolves selected public actions and
applies the same `hosted-tokenless` admission policy as production `upload`.
It does not install Node or run action code. An `admitted` result is policy
evidence, not runtime evidence.

The exact audited `actions/upload-artifact` and exact-name
`actions/download-artifact` commits pass admission through bounded native
adapters. The exact audited `actions/cache` v6.1.0 commit also passes through
its cache-v2 credential boundary. Other cache commits, artifact merge/broad
download modes, and unsupported artifact commits still fail admission. Job and
service container fixtures have separate hosted runtime evidence but remain
outside production admission.

### Exact-source plugin smoke

Every Buildkite build runs `.github/workflows/example-basic.yml` through the
released plugin with `buildkite-gha-source-ref` set to the build's exact commit.
This proves a pull request's unreleased CLI source through the public plugin
before a CLI release exists. The importer explicitly sets
`BUILDKITE_GHA_TARGET_QUEUE=hosted`, so this repository's smoke continues to
exercise its intended queue while default uploads omit agent targeting. The
released-plugin demos remain separate because they verify release archives,
checksums, and the normal default installation path rather than source
execution.

### Paired native UX runs

After the example workflows exist on the default branch, launch the same
workflow at the current branch's exact remote commit in both products:

```bash
scripts/compare-example basic
scripts/compare-example artifacts
scripts/compare-example advanced
```

This uses GitHub's native `workflow_dispatch` path and the dedicated
`buildkite-gha-examples` pipeline. The Buildkite importer uses released plugin
v0.4.4 to run the branch's exact unreleased CLI commit, so both providers
exercise the same source. The script prints both URLs for a side-by-side review
of the graph, logs, summaries, annotations, artifacts, retries, and cancellation
experience. Use `--github-only` or `--buildkite-only` to exercise one native
manual trigger at a time. These runs are qualitative UX comparisons; they do
not replace the normalized smoke evidence below.

### Hosted runtime proofs

Run all implemented hosted proofs against one exact commit:

```bash
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env SMOKE_PROBE=hosted --env SMOKE_COMMIT="$commit" --yes
```

The aggregate retains each phase's importer and continuation topology. Use a
phase selector only for targeted diagnosis. Importer steps that generate jobs
set `BUILDKITE_GHA_TARGET_QUEUE=hosted`; this keeps these hosted-specific proofs
explicit rather than relying on deployment defaults.

| Coverage | Build environment |
| --- | --- |
| Sequential shell and upload | `PHASE2_PROBE=upload`, `PHASE2_COMMIT=<commit>` |
| Concurrent step controls | `PHASE3_PROBE=concurrent`, `PHASE3_COMMIT=<commit>` |
| Public JavaScript/composite actions | `PHASE4_PROBE=actions`, `PHASE4_COMMIT=<commit>` |
| Hosted Docker prerequisites | `PHASE5_PROBE=capabilities`, `PHASE5_COMMIT=<commit>` |
| Dockerfile action path | `PHASE5_PROBE=docker-action`, `PHASE5_COMMIT=<commit>` |
| Complete container runtime | `PHASE5_PROBE=runtime`, `PHASE5_COMMIT=<commit>` |
| Job summary annotation | `PHASE6_PROBE=summary`, `PHASE6_COMMIT=<commit>` |
| Workflow warning/error annotations | `PHASE6_PROBE=annotations`, `PHASE6_COMMIT=<commit>` |
| Upload-artifact publication | `PHASE6_PROBE=upload-artifact`, `PHASE6_COMMIT=<commit>` |
| Artifact producer/consumer roundtrip | `PHASE6_PROBE=artifact-roundtrip`, `PHASE6_COMMIT=<commit>` |

Summary annotation publication is advisory, so the generated job's successful
outcome does not by itself prove that Buildkite persisted the annotation. After
the targeted or aggregate build settles, use an authenticated `bk` CLI with
`read_builds` access to verify the job-scoped annotation independently:

```bash
scripts/phase-6-summary-annotation-verify <build-number> <commit>
```

The verifier reads only the generated job and its annotations. It requires
the build to match the expected exact commit and exactly one `info` annotation
with context `buildkite-gha-job-summary`, job scope, and both checked-in summary
fragments.

Workflow-command annotation publication is also advisory. Verify its distinct
warning and error contexts independently after the annotations proof settles:

```bash
scripts/phase-6-workflow-annotations-verify <build-number> <commit>
```

This verifier requires the generated job to pass even though it emitted an
`::error` command, then checks exactly one job-scoped `warning` annotation and
one job-scoped `error` annotation, their checked-in body fragments, and the
absence of the registered masking canary.

Artifact publication and consumption require independent native-storage
observations after their targeted or aggregate builds settle:

```bash
scripts/phase-6-upload-artifact-verify <build-number> <commit>
scripts/phase-6-artifact-roundtrip-verify <build-number> <commit>
```

The upload verifier binds the archive, terminal manifest, producer, digest,
contents, and action outputs. The roundtrip verifier additionally requires the
producer and both consumer matrix jobs to pass, checks all three terminal
manifests, and confirms both consumers observed the exact payload and compatible
absolute `download-path` output.

The pre-release migration POC covers basic CI, artifact transfer, and the
advanced service-free workflow without adding another phase-specific proof. It
builds the exact checked-out source locally, so it is runtime evidence rather
than installation evidence:

```bash
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env POC_SUITE=migration --env POC_COMMIT="$commit" --yes
```

[Buildkite build 303](https://buildkite.com/buildkite/buildkite-gha/builds/303)
passed the predecessor three-workflow suite at exact commit
`9d29bf26492be760016d29c7ba0d00033b4f9b39`, including declared reusable-output
publication and caller consumption, the build-unique cache miss, post-save,
dependent exact hit, and subsequent artifact fan-out. The revised stable
service-free and cache fixtures retain compile/admission coverage.

The initial CLI and companion plugin `v0.2.0` releases exercised the complete
customer installation path at source commit
`d5102df7e81c49f27a30fb2830d9608a56ee84de`. The service-free importer,
generated jobs, native terminal, and repository checks passed in [Buildkite
build 336](https://buildkite.com/buildkite/buildkite-gha/builds/336). The same
checks plus the cache producer miss and post-save, direct-dependent primary-key
hit and restore, and cache terminal passed in [Buildkite build
337](https://buildkite.com/buildkite/buildkite-gha/builds/337). In both builds,
the public plugin tag resolved to the tested commit
`d009da173158270a3921b2997ae8fd3d68526d00` and installed the same verified CLI
distribution without an explicit CLI version override. These are the
authoritative initial published installation proofs.

CLI `v0.2.1` was subsequently published at exact tag commit
`a780787f049281290974292f00c29e92db717fb9` after [Buildkite release build
351](https://buildkite.com/buildkite/buildkite-gha/builds/351) passed. Companion
plugin `v0.2.1` resolves to exact commit
`4910e56544e365bb545d3157c5aac058b6dabfaa` and defaults to that CLI release.
At exact external repository commit
[`8a74f88676a120e0bc6090b1aafc65edfd62ebbe`](https://github.com/mcncl/gotyper/commit/8a74f88676a120e0bc6090b1aafc65edfd62ebbe),
[`mcncl/gotyper` build 11](https://buildkite.com/no-assembly/gotyper/builds/11)
passed a two-job public workflow through the published plugin, including public
checkout, setup-go, the audited cache v6 lifecycle, a direct dependency, race
tests, static analysis, and a final binary build. This is the first external
customer-shaped migration proof; several more migrations are still required by
the public-beta gate.

The generated example uploader and its `Run workflow` label landed on this
repository's main branch after the CLI `v0.2.1` tag. They are current
repository-demo UX rather than evidence about the released CLI runtime. Repeat
the customer installation path with the current released-plugin demo:

```bash
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env DEMO_SUITE=plugin --env DEMO_COMMIT="$commit" --yes
```

This service-free lane runs the basic, artifact, root-level JavaScript/composite
action, and advanced workflows, then a native terminal step. Add the cache
extension only for an organization with GHAC token minting enabled; the runtime
uses the official Buildkite Results service by default:

```bash
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env DEMO_SUITE=plugin --env DEMO_COMMIT="$commit" \
  --env DEMO_CACHE=1 --yes
```

The first cache run for the release commit must show a producer miss,
post-save, and direct-dependent exact hit. The plugin downloads and
checksum-verifies the public release rather than falling back to a local
binary.

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

```bash
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
