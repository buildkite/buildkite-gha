# Phase 0 Buildkite transport probe

This probe is intentionally dormant: local tests capture commands and never call Buildkite or AWS. Run it later in a disposable real pipeline to answer the Buildkite-only questions that unit tests cannot settle.

## What the local repository proves

`internal/transport` proves deterministic two-job YAML, caller-root materialization and immediate on-disk verification of immutable content-addressed plan bytes, strict compiler dependencies, failure-settling logical `needs`, exact producer-constrained artifact downloads, canonical producer-attributed result manifests, visibility-only metadata, ES256-signed expected/completed markers, fail-closed retry classification, and signature-first verification of build, step, queue, event, plan, capability, replay identity, and bounded validity claims. The Go signer and probe share an RFC 8785 fixture under `fixtures/`; a passing local test is still not evidence about Buildkite scheduling, artifact attribution, upload atomicity, or signature rejection.

## Signed-pipeline scope

Buildkite signed pipelines are outside this initial transport probe. Run the probe on isolated compiler and runtime queues without secrets, provider tokens, privileged containers, or other production capabilities. The signed, build-bound plan envelope remains mandatory and is a separate trust mechanism: `PHASE0_SIGN_JSON` signs plan bindings and markers, and `PHASE0_VERIFY_JSON` verifies them before runtime use.

Signed-pipeline compatibility is deferred to the hardening phase as optional defence in depth for installations that already require it. It must use keys separate from the plan-envelope signer when implemented.

The live probe also requires:

- Buildkite Agent v3.130.0 or newer on compiler and runtime queues for `checkout.skip`;
- `jq`, `sha256sum`, and a pinned probe distribution installed at `/opt/buildkite-gha/phase-0-transport-probe/probe.sh` on both queues;
- `PHASE0_SIGN_JSON`, a trusted compiler-queue command that accepts RFC 8785 canonical JSON on stdin and emits its detached ES256 envelope on stdout;
- `PHASE0_VERIFY_JSON`, a runtime-queue command using verification-only roots that accepts that envelope on stdin, enforces the protected-header profile, and emits the verified RFC 8785 claims on stdout;
- `PHASE0_RUNTIME_QUEUE`, `PHASE0_EVENT_NAME`, `PHASE0_REPOSITORY`, `PHASE0_REF`, 40-character `PHASE0_COMMIT`, canonical `PHASE0_EVENT_DIGEST`, and runtime-owned JSON `PHASE0_LOCAL_CAPABILITIES` (for example `["network"]`); and
- immutable organization and pipeline UUIDs exposed as `BUILDKITE_ORGANIZATION_ID` and `BUILDKITE_PIPELINE_ID` by the probe installation until the live environment-variable oracle confirms their source.

Before enabling the pipeline, verify the probe release checksum and signature outside the build, install it into `/opt/buildkite-gha/phase-0-transport-probe`, and make the directory root-owned and unwritable by the agent user. The static importer, generated jobs, and native check all invoke that fixed path, so `checkout.skip: true` never falls back to repository code. Do not replace it with a checkout path, point either signing command at repository-owned code, or store private material in pipeline YAML. A real acceptance run must use the intended KMS-backed signer or signing broker; a disposable local key can only demonstrate transport mechanics.

## Run and inspect

Install `pipeline.yml` as the pipeline definition and run once normally. Each binding carries the Phase 0 issuer, a deterministic build/step/plan replay identity, and a one-hour validity window; runtime rejects a future, expired, longer-than-24-hour, or wrong-identity binding before consuming the plan. Then repeat with `PHASE0_PRODUCER_FAIL=1`; the consumer must still run because its logical edge has `allow_failure: true`. It obtains the producer job UUID from the exact step-constrained artifact search, constrains the download by that UUID, verifies the manifest's canonical bytes, plan digest, build/job/step identity, exact result and bounded outputs, and records that it consumed the expected failure. Removing a compiler plan artifact must prevent both jobs from running successfully. The native step must not start until the dynamically uploaded consumer has completed, which tests Buildkite's dependency extension from the importer.

To inspect the exact generated jobs with a read-only REST token:

```bash
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${BUILDKITE_API_TOKEN}" \
  "https://api.buildkite.com/v2/organizations/${BUILDKITE_ORGANIZATION_SLUG}/pipelines/${BUILDKITE_PIPELINE_SLUG}/builds/${BUILDKITE_BUILD_NUMBER}?include_retried_jobs=true" \
  > build.json

jq '.jobs[] | select(.step_key == "phase0-transport-producer" or .step_key == "phase0-transport-consumer") | {id,step_key,state}' build.json
```

For each returned job UUID, fetch the job environment with a token that has `read_job_env`, then verify the plan digest and execution identity embedded in the environment:

```bash
job_id="..."
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${BUILDKITE_API_TOKEN}" \
  "https://api.buildkite.com/v2/organizations/${BUILDKITE_ORGANIZATION_SLUG}/pipelines/${BUILDKITE_PIPELINE_SLUG}/builds/${BUILDKITE_BUILD_NUMBER}/jobs/${job_id}/env" \
  | jq '{job_id:"'"${job_id}"'",digest:.env.PHASE0_PLAN_DIGEST,step:.env.BUILDKITE_STEP_KEY,queue:.env.BUILDKITE_AGENT_META_DATA_QUEUE}'
```

The exact recovery predicate would be two jobs only, keys equal to `phase0-transport-producer` and `phase0-transport-consumer`, with plan digests equal to the verified expected marker. Until the live probe proves that state can be established authoritatively after interruption, signature presence or job visibility is not recovery authority.

## Interrupted upload and operator recovery

Run once with `PHASE0_INTERRUPT_AFTER_UPLOAD=1`. The importer writes the signed expected marker, uploads the two steps, and exits before the signed completed marker. A retry stops with exit 75. It never uses `pipeline upload --replace` and never treats duplicate-key rejection as success.

Verify the expected marker with `PHASE0_VERIFY_JSON`, then use the build and job-environment queries above for diagnosis. Current public interfaces cannot prove the full recovery predicate, so every prepared-but-incomplete state is operator-fail-closed: cancel the build and start a new one. Do not set an override, retry the upload in place, or use `pipeline upload --replace`. The local classifier has a `verified-completed` state for a future authoritative verifier, but signature presence from REST never enters that state.

Also run negative builds with a verifier missing the plan-envelope trust root and an invalid plan-envelope signature. Record whether a command hook or plugin runs before each rejection, the resulting job state and diagnostic, artifact selection under retries, metadata visibility, actual queue source, and behavior across old and new plan keys. Those observations are live evidence; this repository does not claim them from local simulation.
