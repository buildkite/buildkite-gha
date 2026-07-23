# Smoke workflow fixture

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

All four workflows are compiler fixtures from Phase 1 onward. Only promote a
workflow into a required runtime lane when its owning phase is implemented.

`events/push.json` is a deterministic compile fixture, so its repository SHA is
intentionally synthetic. End-to-end runs must bind the envelope to the real
fixture repository and commit created by the harness before `actions/checkout`
executes.

The external actions are pinned to immutable commits. Update them intentionally
and record any resulting observation change in the same commit.
