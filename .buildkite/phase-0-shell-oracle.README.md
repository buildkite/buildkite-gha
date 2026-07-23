# Runtime differential oracle

The checked-in GitHub Actions and Buildkite definitions are manual Phase 0
probes. They reproduce the staged `testdata/smoke` shell workflow on each
provider, capture its portable observations, and compare them with the same
committed expectation. Offline validation does not count as hosted evidence.

Both providers must run the exact commit that contains the definitions and
fixture. For GitHub Actions, dispatch `phase-0-shell-oracle.yml` on that ref and
pass the full lowercase commit as the required `source_commit` input. The
default `target=shell` preserves the Phase 0 oracle.

Create a build on the repository pipeline at that same commit with
`PHASE0_PROBE=shell` and `ORACLE_SOURCE_COMMIT` set to the full commit ID. The
default pipeline loads `.buildkite/phase-0-shell-oracle.yml`; every oracle step
verifies its checkout before using the harness, so a branch/commit mismatch
fails before an observation is accepted.

Record the GitHub run URL, Buildkite build URL, exact commit, fixture commit
printed by each provider, and normalized output only after both hosted probes
pass.

## Recorded evidence

[GitHub Actions run 29917793131](https://github.com/buildkite/buildkite-gha/actions/runs/29917793131)
and [Buildkite build 11](https://buildkite.com/buildkite/buildkite-gha/builds/11)
passed at source commit `522a1f9ba87eb2fb0804ca381b1e7a1883d1124f`.
Both materialized fixture commit `f479cc04720cac8bbb59cc54f193948864f08756`
and produced the checked-in normalized observation.

## Phase 3 concurrent-step probe

Dispatch the same GitHub workflow at the exact Phase 3 commit with
`target=concurrent`. The concurrent job's outputs and steps are checked to be
identical to `testdata/smoke/.github/workflows/concurrent.yml`; the hosted run
captures its observation and compares it with the expected fixture through the
same exact materialized commit.

Create a Buildkite build at that commit with `PHASE3_PROBE=concurrent` and
`PHASE3_COMMIT` set to the full commit ID. The separate upload importer and
continuation loader preserve the dynamic-upload ordering invariant, and the
native continuation waits for the generated terminal observer.
