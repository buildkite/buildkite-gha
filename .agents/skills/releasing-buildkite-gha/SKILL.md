---
name: releasing-buildkite-gha
description: Starts and verifies Buildkite-managed buildkite-gha releases. Use for releases, version proposals, release notes, or publication verification.
---

# Release buildkite-gha

Read [the release runbook](../../../docs/development.md#publish-a-release).
Buildkite owns orchestration. Do not tag, draft, or publish outside the
configured pipeline. Only the Buildkite model-provider call chooses the
bump and writes notes; GoReleaser builds and publishes.

## Start

- Main CI triggers releases automatically. Do not start a duplicate build.
- A release request authorizes `mise run release`. It starts the dedicated
  pipeline for the current remote main SHA; it does not release local work.
- Obtain `RELEASE_PIPELINE` and deployment configuration from private settings;
  never copy identifiers, destinations, or internal run links into public content.
- For a proposal or notes-only request, start that pipeline with
  `RELEASE_DRY_RUN=1` and the full current main SHA. Never publish for a draft request.
- Confirm the runbook's configuration requirements before the first run.
- Release trusts CI and main; it does not rerun or poll checks. For manual runs,
  confirm the selected commit passed CI. Never automatically retry partial publication.

## Observe

Use Buildkite events to follow the build. Inspect its proposal and exact notes
artifacts. If the decision is ambiguous, report the failure;
do not substitute a manually chosen version.

Report success only after the `publish-release` job passes with a
`.release/result.json` artifact identifying the release, commit, and included PRs.
Link the release. Share build links only in authorized private contexts.
No-change and dry-run builds did not publish. Do not claim release binaries were
executed or independently verified. Do not post to Slack unless explicitly requested.

## Recovery and historical notes

- Never delete, move, recreate, or reuse a tag, or replace published assets.
- Inspect remote state after any failed write; it may have succeeded.
- Follow the runbook's recovery rules. Obtain approval before remote recovery writes.
- For an explicitly requested historical notes edit, record release and asset
  metadata first. Change only the body, verify the exact replacement, and require
  all other metadata except the release's `updated_at` to remain unchanged.
