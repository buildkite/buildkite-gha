# Migration POC suite

The canonical service-free example workflows at the repository root and the
optional cache fixture in this directory exercise common GitHub Actions
migration shapes as one customer-shaped suite:

- `.github/workflows/example-basic.yml`: checkout, tool setup, shell tests,
  conditions, outputs, and a job summary.
- `.github/workflows/example-artifacts.yml`: build, job outputs, a direct
  producer-to-consumer artifact handoff, and execution of the downloaded
  binary.
- `.github/workflows/example-advanced.yml`: a local reusable workflow with a
  caller-consumed declared output, a public Dockerfile action,
  `continue-on-error`, artifact handoff, matrix fan-out/fan-in, summaries, and
  workflow-command annotations.
- `cache.yml`: the optional `actions/cache` v6 miss/save/direct-dependent-hit
  extension.

Keeping the service-free examples under the root `.github/workflows` directory
makes each file both a native `workflow_dispatch` workflow and the exact source
imported by the Buildkite comparison pipeline. The workflows are manual-only in
this repository so they do not create extra Actions runs on every push.

The released-plugin service-free lane also imports the repository's root-level
Phase 4 workflow to prove checked-out local JavaScript and composite actions.
Keeping those actions under the actual event repository root allows the runtime
to rehash the same workspace source that the compiler locked.

The suite stays within the `hosted-tokenless` admission profile. It does not
imply support for secrets, job or service containers, remote reusable
workflows, moving action refs, broad artifact modes, or cache versions other
than the audited actions/cache v6.1.0 commit.

## Pre-release runtime run

The repository-only importer builds the exact checked-out source and runs the
three service-free workflows before a CLI release exists:

```sh
commit=$(git rev-parse HEAD)
bk build create \
  --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" \
  --commit "$commit" \
  -e POC_SUITE=migration \
  -e POC_COMMIT="$commit" \
  --yes
```

`POC_COMMIT` must be a full lowercase Git object ID. This is runtime evidence,
not installation evidence: the importer compiles a development binary and
calls `upload` directly.

## Released plugin demo

Run the same workflows through the released companion plugin and its real
anonymous release installer. The checked-in dispatcher currently pins plugin
`v0.2.2`; update that pin before using this lane as evidence for a newer
release:

```sh
commit=$(git rev-parse HEAD)
bk build create \
  --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" \
  --commit "$commit" \
  -e DEMO_SUITE=plugin \
  -e DEMO_COMMIT="$commit" \
  --yes
```

Add the optional cache extension only when the organization can mint GHAC
tokens. The runtime uses `https://ghacs.buildkite.com/` by default; set
`BUILDKITE_GHA_CACHE_URL` only to override it with a compatible Results service:

```sh
commit=$(git rev-parse HEAD)
bk build create \
  --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" \
  --commit "$commit" \
  -e DEMO_SUITE=plugin \
  -e DEMO_COMMIT="$commit" \
  -e DEMO_CACHE=1 \
  --yes
```

The first cache run for a release commit must show a producer miss, post-save,
and direct-dependent hit. Warm reruns accept and verify the immutable producer
entry before requiring the same dependent hit. The service-free terminal and,
when selected, cache terminal depend on their plugin importers; Buildkite's
dynamic pipeline dependency extension makes them wait for all generated jobs.

## Recorded evidence

[Buildkite build 303](https://buildkite.com/buildkite/buildkite-gha/builds/303)
passed the predecessor three-workflow suite at exact commit
`9d29bf26492be760016d29c7ba0d00033b4f9b39`. Its then-combined advanced workflow
proved the declared reusable output, build-unique cache miss/post-save/hit,
artifact fan-out, summary, and warning paths.

[Buildkite build 336](https://buildkite.com/buildkite/buildkite-gha/builds/336)
is the historical released-plugin service-free proof, and [Buildkite build
337](https://buildkite.com/buildkite/buildkite-gha/builds/337) is the
historical cache-extension proof. Both ran source commit
`d5102df7e81c49f27a30fb2830d9608a56ee84de` through plugin `v0.2.0`, which
resolved to `d009da173158270a3921b2997ae8fd3d68526d00` and installed the verified
CLI `v0.2.0` distribution without an explicit CLI version override. Build 337
observed the required producer miss and post-save followed by the direct
dependent's exact primary-key hit and restore.

CLI `v0.2.1` was published at exact tag commit
`a780787f049281290974292f00c29e92db717fb9` after [Buildkite release build
351](https://buildkite.com/buildkite/buildkite-gha/builds/351) passed. Companion
plugin `v0.2.1` resolves to exact commit
`4910e56544e365bb545d3157c5aac058b6dabfaa` and installs CLI `v0.2.1` by default.
The first independent migrated-repository proof used those published versions:
at exact `mcncl/gotyper` commit
[`8a74f88676a120e0bc6090b1aafc65edfd62ebbe`](https://github.com/mcncl/gotyper/commit/8a74f88676a120e0bc6090b1aafc65edfd62ebbe),
[Buildkite build 11](https://buildkite.com/no-assembly/gotyper/builds/11)
passed public checkout, setup-go, the audited cache v6 lifecycle, a direct
two-job dependency, race tests, static analysis, and a final binary build on a
compatible Hosted image.

The current published pairing is plugin and CLI v0.4.1. The historical builds
above remain valid for the versions and commits they exercised; the repository
does not yet record an equivalent clean-agent run of the complete suite against
the v0.4.1 pairing.

The historical phase fixtures and their recorded observations remain the
conformance ledger. The now-superseded targeted cache importer, continuation,
and verifier were removed after build 290 passed. The remaining phase-specific
loaders still feed the consolidated hosted smoke proof and are retained.
