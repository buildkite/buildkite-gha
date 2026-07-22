# Phase 0 Buildkite transport probe

This probe is intentionally dormant: local tests capture commands and never call Buildkite or AWS. Run it later in a disposable real pipeline to answer the Buildkite-only questions that unit tests cannot settle.

## What the local repository proves

`internal/transport` proves deterministic two-job YAML, immutable content-addressed plan paths, strict compiler dependencies, failure-settling logical `needs`, exact producer-constrained artifact downloads, canonical producer-attributed result manifests, visibility-only metadata, ES256-signed expected/completed markers, fail-closed retry classification, and signature-first verification of build, step, queue, event, plan, and capability bindings. The probe script uses the same ordering and field names, but a passing local test is not evidence about Buildkite scheduling, artifact attribution, upload atomicity, or signature rejection.

## Signed-pipeline answer

The generator job and every generated step must be signed. Configure the trusted `gha-compiler` uploader with one of the Agent's pipeline-signing mechanisms (`signing-jwks-file` plus `signing-jwks-key-id`, `signing-aws-kms-key`, or the equivalent upload flags), and configure every `gha-runtime` verifier with its public verification material and `verification-failure-behavior=block`. The script does not pass a signing key itself: `buildkite-agent pipeline upload` signs through the uploader's Agent configuration, including the generated commands, pipeline environment, plugins, matrix, and repository. Keep the plan-envelope signer separate from this key.

The relevant current interfaces are the [signed-pipelines configuration](https://buildkite.com/docs/agent/self-hosted/security/signed-pipelines), [pipeline upload signing flags](https://buildkite.com/docs/agent/cli/reference/pipeline), [dynamic-pipeline security contract](https://buildkite.com/docs/pipelines/configure/dynamic-pipelines), [build/job signature response](https://buildkite.com/docs/apis/rest-api/builds), and [job environment endpoint](https://buildkite.com/docs/apis/rest-api/jobs).

The live probe also requires:

- Buildkite Agent v3.130.0 or newer on compiler and runtime queues for `checkout.skip`;
- `jq`, `sha256sum`, and a pinned probe distribution installed at `/opt/buildkite-gha/phase-0-transport-probe/probe.sh` on both queues;
- `PHASE0_SIGN_JSON`, a trusted compiler-queue command that accepts canonical JSON on stdin and emits its detached ES256 envelope on stdout;
- `PHASE0_VERIFY_JSON`, a runtime-queue command using verification-only roots that accepts that envelope on stdin and emits verified canonical claims on stdout;
- `PHASE0_RUNTIME_QUEUE`, `PHASE0_EVENT_NAME`, `PHASE0_REPOSITORY`, `PHASE0_REF`, 40-character `PHASE0_COMMIT`, canonical `PHASE0_EVENT_DIGEST`, and runtime-owned JSON `PHASE0_LOCAL_CAPABILITIES` (for example `["network"]`); and
- immutable organization and pipeline UUIDs exposed as `BUILDKITE_ORGANIZATION_ID` and `BUILDKITE_PIPELINE_ID` by the probe installation until the live environment-variable oracle confirms their source.

Before enabling the pipeline, verify the probe release checksum and signature outside the build, install it into `/opt/buildkite-gha/phase-0-transport-probe`, and make the directory root-owned and unwritable by the agent user. The static importer, generated jobs, and native check all invoke that fixed path, so `checkout.skip: true` never falls back to repository code. Do not replace it with a checkout path, point either signing command at repository-owned code, or store private material in pipeline YAML. A real acceptance run must use the intended KMS-backed signer or signing broker; a disposable local key can only demonstrate transport mechanics.

## Run and inspect

Install `pipeline.yml` as the pipeline definition, sign that bootstrap definition, and run once normally. Then repeat with `PHASE0_PRODUCER_FAIL=1`; the consumer must still run because its logical edge has `allow_failure: true`, while removing a compiler plan artifact must prevent both jobs from running successfully. The native step must not start until the dynamically uploaded consumer has completed, which tests Buildkite's dependency extension from the importer.

To inspect exact generated jobs and their pipeline signatures with a read-only REST token:

```bash
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${BUILDKITE_API_TOKEN}" \
  "https://api.buildkite.com/v2/organizations/${BUILDKITE_ORGANIZATION_SLUG}/pipelines/${BUILDKITE_PIPELINE_SLUG}/builds/${BUILDKITE_BUILD_NUMBER}?include_retried_jobs=true" \
  > build.json

jq '.jobs[] | select(.step_key == "phase0-transport-producer" or .step_key == "phase0-transport-consumer") | {id,step_key,state,signature:.step.signature}' build.json
```

For each returned job UUID, fetch the job environment with a token that has `read_job_env`, then verify the plan digest embedded in the environment and confirm `env` appears in `step.signature.signed_fields`:

```bash
job_id="..."
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${BUILDKITE_API_TOKEN}" \
  "https://api.buildkite.com/v2/organizations/${BUILDKITE_ORGANIZATION_SLUG}/pipelines/${BUILDKITE_PIPELINE_SLUG}/builds/${BUILDKITE_BUILD_NUMBER}/jobs/${job_id}/env" \
  | jq '{job_id:"'"${job_id}"'",digest:.env.PHASE0_PLAN_DIGEST,step:.env.BUILDKITE_STEP_KEY,queue:.env.BUILDKITE_AGENT_META_DATA_QUEUE}'
```

The exact recovery predicate would be two jobs only, keys equal to `phase0-transport-producer` and `phase0-transport-consumer`, plan digests equal to the verified expected marker, signatures verified under the intended root, and `env` present in both signed-field lists. The REST response exposes signature values and signed-field names, but it does not expose the verifier's acceptance verdict, and the Agent currently documents signing but no supported offline verification command. A non-null signature is therefore visibility evidence, not recovery authority.

## Interrupted upload and operator recovery

Run once with `PHASE0_INTERRUPT_AFTER_UPLOAD=1`. The importer writes the signed expected marker, uploads the two steps, and exits before the signed completed marker. A retry stops with exit 75. It never uses `pipeline upload --replace` and never treats duplicate-key rejection as success.

Verify the expected marker with `PHASE0_VERIFY_JSON`, then use the build and job-environment queries above for diagnosis. Current public interfaces cannot prove the full recovery predicate, so every prepared-but-incomplete state is operator-fail-closed: cancel the build and start a new one. Do not set an override, retry the upload in place, or use `pipeline upload --replace`. The local classifier has a `verified-completed` state for a future authoritative verifier, but signature presence from REST never enters that state.

Also run negative builds with an unsigned generated upload, a wrong pipeline key, a verifier missing the signing root, and an invalid plan-envelope signature. Record whether a command hook or plugin runs before each rejection, the resulting job state and diagnostic, artifact selection under retries, metadata visibility, actual queue source, and behavior across old/new pipeline and plan keys. Those observations are live evidence; this repository does not claim them from local simulation.
