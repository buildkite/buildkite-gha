# Smoke workflow fixtures

This directory is a self-contained repository fixture for the first
`buildkite-gha` vertical slice. A differential-test harness should materialize
this directory as the repository root, run the workflows on GitHub Actions and
Buildkite, and compare their normalized observations.

The fixture is deliberately staged so early phases can use it before the whole
workflow is executable:

1. `shell.yml` is the first runtime target. It has two logical jobs, a static
   consumer matrix, shell steps, and a bounded `needs` output.
2. `concurrent.yml` adds background, targeted and full waits, cancellation,
   bounded queueing, parallel lowering, implicit cleanup, and concurrent
   masking probes.
3. `ci.yml` adds checkout plus local JavaScript and composite actions, including
   output, environment-file, masking, summary, and post-action events.
4. `artifact.yml` adds GitHub artifact-action compatibility and verifies one
   payload in both consumer matrix instances.

`manifest.json` is the authoritative, ordered compatibility inventory for
these workflows and the related Phase 4, Phase 5, and Phase 6 fixtures. Every
entry names its deterministic compile event and records the separate runtime
evidence.

Expectations have these precise meanings:

- `compile-pass`: validation and deterministic compilation are required. Any
  cited component runtime evidence does not establish full fixture execution.
- `compile-unsupported`: validation must fail with a stable compile diagnostic.
- `runtime-pass`: runtime evidence exists outside this compile-only harness;
  local validation and deterministic compilation remain required.
- `runtime-unsupported`: compilation is required, but a runtime dependency is
  intentionally unsupported (currently the cache service).
- `future`: the fixture is inventoried but not yet required to compile.

Run `mise run smoke:local` to strictly validate the manifest, JSON-validate
each workflow, and compare two nonempty pipeline compilations byte for byte.
The default harness does not parse emitted YAML, fetch actions, or execute
workflows; successful compilation is not runtime evidence.

Run `mise run smoke:profile` for the opt-in networked preflight of entries marked
`hosted-tokenless`. It anonymously resolves actions, compiles plans, and applies
the same admission policy as production upload without installing or executing
Node. Admission does not execute action code or prove that a generic action is
independent of GitHub-only artifact, cache, token, or OIDC services.
The exact audited `actions/upload-artifact` and exact-name
`actions/download-artifact` commits are admitted through bounded native
adapters. The exact audited `actions/cache` v6.1.0 commit is also admitted;
other cache commits, artifact merge and broad download modes, and unsupported
artifact commits remain rejected. The profile leaves unknown generic
service dependencies as an explicit warning rather than guessing from
arbitrary action source. Runtime-pass job and service container fixtures are
deliberately not marked for this profile: their execution is proven separately,
while production hosted-tokenless admission continues to reject their container
provenance.

`events/push.json` is deterministic and its repository SHA is intentionally
synthetic. End-to-end runs involving checkout must bind the event to the real
fixture repository and commit. External actions remain pinned to immutable
commits and should be updated intentionally with their runtime evidence.
