# Migration POC suite

These workflows exercise common GitHub Actions migration shapes as one
exact-commit Buildkite proof, rather than adding more phase-numbered probes:

- `basic.yml`: checkout, tool setup, shell tests, conditions, outputs, and a job
  summary.
- `artifacts.yml`: build, job outputs, a direct producer-to-consumer artifact
  handoff, and execution of the downloaded binary.
- `advanced.yml`: a local reusable workflow, cache v6 miss/save/hit lifecycle,
  `continue-on-error`, artifact handoff, matrix fan-out/fan-in, summaries, and
  workflow-command annotations.

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

The historical phase fixtures and their recorded observations remain the
conformance ledger. After this replacement suite has hosted runtime evidence,
the duplicated phase-specific importer/continuation scaffolding can be removed
separately without discarding those fixtures or claims.
