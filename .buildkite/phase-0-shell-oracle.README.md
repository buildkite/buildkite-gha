# Phase 0 shell differential oracle

The checked-in GitHub Actions and Buildkite definitions are manual Phase 0
probes. They reproduce the staged `testdata/smoke` shell workflow on each
provider, capture its portable observations, and compare them with the same
committed expectation. Offline validation does not count as hosted evidence.

Both providers must run the exact commit that contains the definitions and
fixture. For GitHub Actions, dispatch `phase-0-shell-oracle.yml` on that ref and
pass the full lowercase commit as the required `source_commit` input.

Configure the Buildkite probe pipeline to upload
`.buildkite/phase-0-shell-oracle.yml`, then create the build at that same commit
with `ORACLE_SOURCE_COMMIT` set to the full commit ID. Every step verifies its
checkout before using the harness, so a branch/commit mismatch fails before an
observation is accepted.

The managed repository and pipeline do not exist yet. Record the GitHub run URL,
Buildkite build URL, exact commit, fixture commit printed by each provider, and
normalized output only after both hosted probes pass.
