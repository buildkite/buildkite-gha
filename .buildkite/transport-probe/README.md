# Buildkite transport probe

This probe is intentionally dormant: local tests capture commands and never call Buildkite or AWS. Run it later in a disposable real pipeline to answer the Buildkite-only questions that unit tests cannot settle.

## What the local repository proves

`internal/transport` proves deterministic two-job YAML, caller-root materialization and immediate on-disk verification of immutable content-addressed plan bytes, strict compiler dependencies, failure-settling logical `needs`, exact producer-constrained artifact downloads, canonical producer-attributed result manifests, visibility-only metadata, ES256-signed expected/completed markers, fail-closed retry classification, and signature-first verification of build, step, queue, event, plan, capability, replay identity, and bounded validity claims. The Go signer and probe share an RFC 8785 fixture under `fixtures/`; a passing local test is still not evidence about Buildkite scheduling, artifact attribution, upload atomicity, or signature rejection.

## Signed-pipeline scope

Buildkite signed pipelines are outside this initial transport probe. Run the probe on the existing `hosted` queue without secrets, provider tokens, privileged containers, or other production capabilities. The signed, build-bound plan envelope remains mandatory and is a separate trust mechanism: `TRANSPORT_PROBE_SIGN_JSON` signs plan bindings and markers, and `TRANSPORT_PROBE_VERIFY_JSON` verifies them before runtime use.

Signed-pipeline compatibility is deferred to later hardening as optional defence in depth for installations that already require it. It must use keys separate from the plan-envelope signer when implemented.

The live probe also requires:

- `jq` and `sha256sum` on the runtime queue;
- `TRANSPORT_PROBE_RUNTIME_QUEUE=hosted`, `TRANSPORT_PROBE_EVENT_NAME`, `TRANSPORT_PROBE_REPOSITORY`, `TRANSPORT_PROBE_REF`, 40-character `TRANSPORT_PROBE_COMMIT`, canonical `TRANSPORT_PROBE_EVENT_DIGEST`, runtime-owned JSON `TRANSPORT_PROBE_LOCAL_CAPABILITIES` (for example `["network"]`), and a disposable build-level `TRANSPORT_PROBE_REDACTION_SECRET` canary; and
- immutable organization and pipeline UUIDs exposed as `BUILDKITE_ORGANIZATION_ID` and `BUILDKITE_PIPELINE_ID` by the probe installation until the live environment-variable oracle confirms their source.

The build must use the exact commit supplied in `TRANSPORT_PROBE_COMMIT`. Static and generated jobs run the checked-in probe from that checkout. The checked-in signing helper derives a public, disposable key, so it demonstrates signature transport and rejection but grants no production authority. KMS-backed plan signing and checkout-free compiler isolation remain later security gates.

## Run and inspect

Install `pipeline.yml` as the pipeline definition and run once normally. Each binding carries the plan-envelope issuer, a deterministic build/step/plan replay identity, and a one-hour validity window; runtime rejects a future, expired, longer-than-24-hour, or wrong-identity binding before consuming the plan. Then repeat with `TRANSPORT_PROBE_PRODUCER_FAIL=1`; the consumer must still run because its logical edge has `allow_failure: true`. It obtains the producer job UUID from the exact step-constrained artifact search, constrains the download by that UUID, verifies the manifest's canonical bytes, plan digest, build/job/step identity, exact result and bounded outputs, and records that it consumed the expected failure. Removing a compiler plan artifact must prevent both jobs from running successfully. The native step must not start until the dynamically uploaded consumer has completed, which tests Buildkite's dependency extension from the importer.

The producer also prints the disposable `TRANSPORT_PROBE_REDACTION_SECRET` canary. Its log must contain `[REDACTED]` and must not contain the canary value.

The repository's default pipeline loads this probe when the build has `COMPATIBILITY_PROOF=transport-probe`. Supply the remaining values as build environment, including `TRANSPORT_PROBE_LOCAL_CAPABILITIES=["network"]`. Use the exact organization and pipeline UUIDs from the Buildkite API rather than slugs.

To inspect the exact generated jobs with a read-only REST token:

```bash
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${BUILDKITE_API_TOKEN}" \
  "https://api.buildkite.com/v2/organizations/${BUILDKITE_ORGANIZATION_SLUG}/pipelines/${BUILDKITE_PIPELINE_SLUG}/builds/${BUILDKITE_BUILD_NUMBER}?include_retried_jobs=true" \
  > build.json

jq '.jobs[] | select(.step_key == "transport-probe-producer" or .step_key == "transport-probe-consumer") | {id,step_key,state}' build.json
```

For each returned job UUID, fetch the job environment with a token that has `read_job_env`, then verify the plan digest and execution identity embedded in the environment:

```bash
job_id="..."
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${BUILDKITE_API_TOKEN}" \
  "https://api.buildkite.com/v2/organizations/${BUILDKITE_ORGANIZATION_SLUG}/pipelines/${BUILDKITE_PIPELINE_SLUG}/builds/${BUILDKITE_BUILD_NUMBER}/jobs/${job_id}/env" \
  | jq '{job_id:"'"${job_id}"'",digest:.env.TRANSPORT_PROBE_PLAN_DIGEST,step:.env.BUILDKITE_STEP_KEY,queue:.env.BUILDKITE_AGENT_META_DATA_QUEUE}'
```

The exact recovery predicate would be two jobs only, keys equal to `transport-probe-producer` and `transport-probe-consumer`, with plan digests equal to the verified expected marker. Until the live probe proves that state can be established authoritatively after interruption, signature presence or job visibility is not recovery authority.

## Interrupted upload and operator recovery

Run once with `TRANSPORT_PROBE_INTERRUPT_AFTER_UPLOAD=1`. The importer writes the signed expected marker, uploads the two steps, and exits before the signed completed marker. A retry stops with exit 75. It never uses `pipeline upload --replace` and never treats duplicate-key rejection as success.

Verify the expected marker with `TRANSPORT_PROBE_VERIFY_JSON`, then use the build and job-environment queries above for diagnosis. Current public interfaces cannot prove the full recovery predicate, so every prepared-but-incomplete state is operator-fail-closed: cancel the build and start a new one. Do not set an override, retry the upload in place, or use `pipeline upload --replace`. The local classifier has a `verified-completed` state for a future authoritative verifier, but signature presence from REST never enters that state.

Also run a negative build with `TRANSPORT_PROBE_TAMPER_BINDING=1`; the producer must reject the invalid plan-envelope signature. Record the resulting job state and diagnostic, artifact selection under retries, metadata visibility, actual queue source, and behavior across old and new plan keys. The recorded observations below come from live builds rather than local simulation.

## Recorded evidence

- [Buildkite build 27](https://buildkite.com/buildkite/buildkite-gha/builds/27)
  passed the complete transport and Agent-redaction path at commit
  `787b3adfe306a17fbf073c70b24f8b747b5882a8`.
- [Buildkite build 15](https://buildkite.com/buildkite/buildkite-gha/builds/15)
  failed its producer while the failure-settling consumer passed.
- [Buildkite build 19](https://buildkite.com/buildkite/buildkite-gha/builds/19)
  rejected the corrupted envelope with `invalid ES256 signature`.
- [Buildkite build 17](https://buildkite.com/buildkite/buildkite-gha/builds/17)
  stopped after upload, and its importer retry rejected the incomplete state
  with exit 75.
