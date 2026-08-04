# Migration POC suite

These workflows exercise common GitHub Actions migration shapes as one
exact-commit Buildkite proof, rather than adding more phase-numbered probes:

- `basic.yml`: checkout, tool setup, shell tests, conditions, outputs, and a job
  summary.
- `artifacts.yml`: build, job outputs, a direct producer-to-consumer artifact
  handoff, and execution of the downloaded binary.
- `advanced.yml`: a local reusable workflow with a caller-consumed declared
  output, cache v6 miss/save/hit lifecycle, `continue-on-error`, artifact
  handoff, matrix fan-out/fan-in, summaries, and workflow-command annotations.

The suite stays within the `hosted-tokenless` admission profile. It does not
imply support for secrets, job or service containers, remote reusable
workflows, moving action refs, broad artifact modes, or cache versions other
than the audited actions/cache v6.1.0 commit.

## Hosted run

Run the suite against one exact commit:

```sh
commit=$(git rev-parse HEAD)
bk build create \
  --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" \
  --commit "$commit" \
  -e POC_SUITE=migration \
  -e POC_COMMIT="$commit" \
  -e BUILDKITE_GHA_CACHE_URL=https://isaacsu-ghacs.buildkite.dev
```

`POC_COMMIT` must be a full lowercase Git object ID. The importer generates an
event bound to that same commit and substitutes the Buildkite build number into
the cache key, making the expected producer miss and dependent consumer hit
repeatable.

## Recorded evidence

[Buildkite build 290](https://buildkite.com/buildkite/buildkite-gha/builds/290)
passed all three POCs at exact commit
`379344599c0653990687d017bd195d416c7bc29c`. The advanced workflow proved a
build-unique cache miss, post-save, and dependent exact hit, then transferred
the restored payload through the bounded artifact adapters to both matrix
consumers. The successful workflow proves the summary and warning emission
paths ran, while the dedicated Phase 6 fixtures retain the independent
annotation-persistence observations.

The declared reusable-workflow output assertion was added after build 290 and
must receive a fresh exact-commit hosted run before it is recorded as runtime
evidence for the current advanced fixture.

The historical phase fixtures and their recorded observations remain the
conformance ledger. The now-superseded targeted cache importer, continuation,
and verifier were removed after build 290 passed. The remaining phase-specific
loaders still feed the consolidated hosted smoke proof and are retained.
