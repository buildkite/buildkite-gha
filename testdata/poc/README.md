# Migration POC suite

These workflows exercise common GitHub Actions migration shapes as one
customer-shaped suite, rather than adding more phase-numbered probes:

- `basic.yml`: checkout, tool setup, shell tests, conditions, outputs, and a job
  summary.
- `artifacts.yml`: build, job outputs, a direct producer-to-consumer artifact
  handoff, and execution of the downloaded binary.
- `advanced.yml`: a local reusable workflow with a caller-consumed declared
  output, a public Dockerfile action, `continue-on-error`, artifact handoff,
  matrix fan-out/fan-in, summaries, and workflow-command annotations.
- `cache.yml`: the optional `actions/cache` v6 miss/save/direct-dependent-hit
  extension.

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
anonymous release installer:

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
tokens and the exact origin is configured:

```sh
commit=$(git rev-parse HEAD)
bk build create \
  --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" \
  --commit "$commit" \
  -e DEMO_SUITE=plugin \
  -e DEMO_COMMIT="$commit" \
  -e DEMO_CACHE=1 \
  -e BUILDKITE_GHA_CACHE_URL=<cache-v2-results-origin> \
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
is the authoritative released-plugin service-free proof, and [Buildkite build
337](https://buildkite.com/buildkite/buildkite-gha/builds/337) is the
authoritative cache-extension proof. Both ran source commit
`d5102df7e81c49f27a30fb2830d9608a56ee84de` through plugin `v0.2.0`, which
resolved to `d009da173158270a3921b2997ae8fd3d68526d00` and installed the verified
CLI `v0.2.0` distribution without an explicit CLI version override. Build 337
observed the required producer miss and post-save followed by the direct
dependent's exact primary-key hit and restore.

The historical phase fixtures and their recorded observations remain the
conformance ledger. The now-superseded targeted cache importer, continuation,
and verifier were removed after build 290 passed. The remaining phase-specific
loaders still feed the consolidated hosted smoke proof and are retained.
