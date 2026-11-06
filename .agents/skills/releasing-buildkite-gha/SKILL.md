---
name: releasing-buildkite-gha
description: Starts and verifies Buildkite-managed buildkite-gha releases. Use for releases, version proposals, release notes, or publication verification.
---

# Release buildkite-gha

Read [the release runbook](../../../docs/development.md#publish-a-release).
Buildkite owns orchestration. Do not tag, draft, or publish outside the
configured pipeline. Only the Buildkite model-provider call chooses the
bump and writes notes; deterministic code performs publication and verification.

## Start

- A release request authorizes `mise run release`. It starts the dedicated
  pipeline for the current remote main SHA; it does not release local work.
- Obtain `RELEASE_PIPELINE` and deployment configuration from private settings;
  never copy identifiers, destinations, or internal run links into public content.
- For a proposal or notes-only request, start that pipeline with
  `RELEASE_DRY_RUN=1` and the full current main SHA. Never publish for a draft request.
- Confirm the runbook's configuration requirements before the first run.
- Do not bypass failing gates or automatically retry a partial publication.

## Observe

Use Buildkite events to follow the build. Inspect its proposal and exact notes
artifacts. If the decision is ambiguous or main advances, report the failure;
do not substitute a manually chosen version.

Report success only after the `publish-release` job passes and its
`.release/verification.json` artifact confirms the tag/commit, exact notes,
assets, checksums, archive members, Linux amd64 version, and included PR comments.
Link the release. Share build links only in authorized private contexts.
State that other platform binaries received archive
and checksum checks, not native execution. No-change and dry-run builds did not
publish. Do not post to Slack unless explicitly requested.

## Recovery and historical notes

- Never delete, move, recreate, or reuse a tag, or replace published assets.
- Inspect remote state after any failed write; it may have succeeded.
- Follow the runbook's recovery rules. Obtain approval before remote recovery writes.
- For an explicitly requested historical notes edit, record release and asset
  metadata first. Change only the body, verify the exact replacement, and require
  all other metadata except the release's `updated_at` to remain unchanged.
