#!/usr/bin/env bash
set -euo pipefail

readonly importer_key="phase0-transport-importer"
readonly producer_key="phase0-transport-producer"
readonly consumer_key="phase0-transport-consumer"
readonly marker_prefix="buildkite-gha/v1/uploads/${importer_key}"
readonly probe_path="/opt/buildkite-gha/phase-0-transport-probe/probe.sh"

require_env() {
  local name
  for name in "$@"; do
    if [[ -z "${!name:-}" ]]; then
      echo "missing required environment variable: ${name}" >&2
      exit 64
    fi
  done
}

sha256_file() {
  sha256sum "$1" | awk '{print "sha256:" $1}'
}

sign_json() {
  require_env PHASE0_SIGN_JSON
  "${PHASE0_SIGN_JSON}" < "$1"
}

verify_json() {
  require_env PHASE0_VERIFY_JSON
  "${PHASE0_VERIFY_JSON}" < "$1"
}

require_name() {
  if [[ ! "$2" =~ ^[A-Za-z0-9_-]{1,255}$ ]]; then
    echo "invalid ${1}: ${2}" >&2
    exit 64
  fi
}

require_digest() {
  if [[ ! "$2" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "invalid ${1}: ${2}" >&2
    exit 64
  fi
}

require_uuid() {
  if [[ ! "$2" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]; then
    echo "invalid ${1}: ${2}" >&2
    exit 64
  fi
}

require_commit() {
  if [[ ! "$2" =~ ^[0-9a-f]{40}$ ]]; then
    echo "invalid ${1}: ${2}" >&2
    exit 64
  fi
}

write_claims() {
  local path="$1" step_key="$2" plan_digest="$3" capabilities="$4"
  jq -cn \
    --arg organization_id "${BUILDKITE_ORGANIZATION_ID}" \
    --arg pipeline_id "${BUILDKITE_PIPELINE_ID}" \
    --arg build_id "${BUILDKITE_BUILD_ID}" \
    --arg step_key "${step_key}" \
    --arg queue "${PHASE0_RUNTIME_QUEUE}" \
    --arg event_name "${PHASE0_EVENT_NAME}" \
    --arg repository "${PHASE0_REPOSITORY}" \
    --arg ref "${PHASE0_REF}" \
    --arg commit "${PHASE0_COMMIT}" \
    --arg event_digest "${PHASE0_EVENT_DIGEST}" \
    --arg plan_digest "${plan_digest}" \
    --argjson capabilities "${capabilities}" \
    '{build:{organization_id:$organization_id,pipeline_id:$pipeline_id,build_id:$build_id},step_key:$step_key,queue:$queue,event:{provider:"github",name:$event_name,repository:$repository,ref:$ref,commit:$commit,digest:$event_digest,trust:"untrusted",attestation:"buildkite-webhook"},plan_digest:$plan_digest,capability_ceiling:$capabilities}' > "$path"
}

bootstrap() {
  require_env BUILDKITE_BUILD_ID BUILDKITE_ORGANIZATION_ID BUILDKITE_PIPELINE_ID PHASE0_RUNTIME_QUEUE PHASE0_EVENT_NAME PHASE0_REPOSITORY PHASE0_REF PHASE0_COMMIT PHASE0_EVENT_DIGEST
  require_name PHASE0_RUNTIME_QUEUE "${PHASE0_RUNTIME_QUEUE}"
  require_digest PHASE0_EVENT_DIGEST "${PHASE0_EVENT_DIGEST}"
  require_uuid BUILDKITE_BUILD_ID "${BUILDKITE_BUILD_ID}"
  require_uuid BUILDKITE_ORGANIZATION_ID "${BUILDKITE_ORGANIZATION_ID}"
  require_uuid BUILDKITE_PIPELINE_ID "${BUILDKITE_PIPELINE_ID}"
  require_commit PHASE0_COMMIT "${PHASE0_COMMIT}"
  local work
  work="$(mktemp -d)"
  trap 'rm -rf "${work}"' EXIT

  mkdir -p "${work}/buildkite-gha/v1/plans/${producer_key}" "${work}/buildkite-gha/v1/plans/${consumer_key}"
  printf '%s\n' '{"logical_job":"producer","required_capabilities":["network"],"schema":"phase0-probe-plan/v1"}' > "${work}/producer.plan.json"
  printf '%s\n' '{"logical_job":"consumer","needs":["producer"],"required_capabilities":[],"schema":"phase0-probe-plan/v1"}' > "${work}/consumer.plan.json"
  local producer_digest consumer_digest
  producer_digest="$(sha256_file "${work}/producer.plan.json")"
  consumer_digest="$(sha256_file "${work}/consumer.plan.json")"

  write_claims "${work}/producer.claims.json" "${producer_key}" "${producer_digest}" '["network"]'
  write_claims "${work}/consumer.claims.json" "${consumer_key}" "${consumer_digest}" '[]'
  sign_json "${work}/producer.claims.json" > "${work}/producer.binding.jws"
  sign_json "${work}/consumer.claims.json" > "${work}/consumer.binding.jws"

  local producer_artifact consumer_artifact
  producer_artifact="${work}/buildkite-gha/v1/plans/${producer_key}/${producer_digest#sha256:}"
  consumer_artifact="${work}/buildkite-gha/v1/plans/${consumer_key}/${consumer_digest#sha256:}"
  mkdir -p "${producer_artifact}" "${consumer_artifact}"
  cp "${work}/producer.plan.json" "${producer_artifact}/plan.json"
  cp "${work}/producer.binding.jws" "${producer_artifact}/binding.jws"
  cp "${work}/consumer.plan.json" "${consumer_artifact}/plan.json"
  cp "${work}/consumer.binding.jws" "${consumer_artifact}/binding.jws"

  (
    cd "${work}"
    buildkite-agent artifact upload 'buildkite-gha/v1/plans/**/*'
  )

  local pipeline="${work}/dynamic.yml"
  cat > "${pipeline}" <<YAML
steps:
  - label: ":test_tube: Phase 0 producer"
    key: "${producer_key}"
    command: "${probe_path} producer"
    agents:
      queue: "${PHASE0_RUNTIME_QUEUE}"
    checkout:
      skip: true
    env:
      PHASE0_PLAN_DIGEST: "${producer_digest}"
      PHASE0_PLAN_PRODUCER: "${importer_key}"
    depends_on:
      - step: "${importer_key}"
        allow_failure: false
  - label: ":test_tube: Phase 0 consumer"
    key: "${consumer_key}"
    command: "${probe_path} consumer"
    agents:
      queue: "${PHASE0_RUNTIME_QUEUE}"
    checkout:
      skip: true
    env:
      PHASE0_PLAN_DIGEST: "${consumer_digest}"
      PHASE0_PLAN_PRODUCER: "${importer_key}"
    depends_on:
      - step: "${importer_key}"
        allow_failure: false
      - step: "${producer_key}"
        allow_failure: true
YAML

  local pipeline_digest intent expected completed existing
  pipeline_digest="$(sha256_file "${pipeline}")"
  jq -cn \
    --arg build_id "${BUILDKITE_BUILD_ID}" \
    --arg importer_key "${importer_key}" \
    --arg pipeline_digest "${pipeline_digest}" \
    --arg producer_digest "${producer_digest}" \
    --arg consumer_digest "${consumer_digest}" \
    '{phase:"expected",intent:{build_id:$build_id,importer_key:$importer_key,pipeline_digest:$pipeline_digest,jobs:[{key:"phase0-transport-consumer",plan_digest:$consumer_digest},{key:"phase0-transport-producer",plan_digest:$producer_digest}]}}' > "${work}/expected.json"
  expected="$(sign_json "${work}/expected.json")"
  existing="$(buildkite-agent meta-data get "${marker_prefix}/expected" 2>/dev/null || true)"
  completed="$(buildkite-agent meta-data get "${marker_prefix}/completed" 2>/dev/null || true)"
  if [[ -z "${existing}" && -n "${completed}" ]]; then
    echo "completed marker exists without expected marker" >&2
    exit 78
  fi
  if [[ -n "${existing}" ]]; then
    printf '%s' "${existing}" > "${work}/existing.jws"
    verify_json "${work}/existing.jws" > "${work}/existing.json"
    jq -S -c . "${work}/expected.json" > "${work}/expected.canonical.json"
    jq -S -c . "${work}/existing.json" > "${work}/existing.canonical.json"
    if ! cmp -s "${work}/expected.canonical.json" "${work}/existing.canonical.json"; then
      echo "signed expected marker conflicts with this deterministic upload" >&2
      exit 78
    fi
    if [[ -n "${completed}" ]]; then
      printf '%s' "${completed}" > "${work}/completed.jws"
      verify_json "${work}/completed.jws" > "${work}/completed.actual.json"
      jq '.phase = "completed"' "${work}/expected.json" | jq -S -c . > "${work}/completed.expected.json"
      jq -S -c . "${work}/completed.actual.json" > "${work}/completed.actual.canonical.json"
      if ! cmp -s "${work}/completed.expected.json" "${work}/completed.actual.canonical.json"; then
        echo "signed completed marker conflicts with expected upload" >&2
        exit 78
      fi
      echo "upload already completed"
      return
    fi
    echo "prepared upload has no completion marker; current public queries do not prove pipeline-signature verification, so cancel this build and start a new one" >&2
    exit 75
  else
    buildkite-agent meta-data set "${marker_prefix}/expected" "${expected}"
    buildkite-agent pipeline upload --no-interpolation --reject-secrets --reject-parse-warnings "${pipeline}"
    if [[ "${PHASE0_INTERRUPT_AFTER_UPLOAD:-}" == "1" ]]; then
      echo "intentional interruption after pipeline upload" >&2
      exit 75
    fi
  fi

  jq '.phase = "completed"' "${work}/expected.json" > "${work}/completed.json"
  completed="$(sign_json "${work}/completed.json")"
  buildkite-agent meta-data set "${marker_prefix}/completed" "${completed}"
}

load_and_verify_plan() {
  local step_key="$1" work="$2"
  require_env BUILDKITE_BUILD_ID BUILDKITE_ORGANIZATION_ID BUILDKITE_PIPELINE_ID BUILDKITE_STEP_KEY BUILDKITE_AGENT_META_DATA_QUEUE PHASE0_PLAN_DIGEST PHASE0_PLAN_PRODUCER PHASE0_EVENT_NAME PHASE0_REPOSITORY PHASE0_REF PHASE0_COMMIT PHASE0_EVENT_DIGEST PHASE0_LOCAL_CAPABILITIES
  require_name BUILDKITE_STEP_KEY "${BUILDKITE_STEP_KEY}"
  require_name BUILDKITE_AGENT_META_DATA_QUEUE "${BUILDKITE_AGENT_META_DATA_QUEUE}"
  require_digest PHASE0_PLAN_DIGEST "${PHASE0_PLAN_DIGEST}"
  require_digest PHASE0_EVENT_DIGEST "${PHASE0_EVENT_DIGEST}"
  require_uuid BUILDKITE_BUILD_ID "${BUILDKITE_BUILD_ID}"
  require_uuid BUILDKITE_ORGANIZATION_ID "${BUILDKITE_ORGANIZATION_ID}"
  require_uuid BUILDKITE_PIPELINE_ID "${BUILDKITE_PIPELINE_ID}"
  require_commit PHASE0_COMMIT "${PHASE0_COMMIT}"
  [[ "${PHASE0_PLAN_PRODUCER}" == "${importer_key}" ]]
  jq -e 'type == "array" and all(.[]; type == "string")' <<<"${PHASE0_LOCAL_CAPABILITIES}" >/dev/null
  local prefix="buildkite-gha/v1/plans/${step_key}/${PHASE0_PLAN_DIGEST#sha256:}"
  buildkite-agent artifact download "${prefix}/*" "${work}" --step "${PHASE0_PLAN_PRODUCER}"
  [[ "$(sha256_file "${work}/${prefix}/plan.json")" == "${PHASE0_PLAN_DIGEST}" ]]
  verify_json "${work}/${prefix}/binding.jws" > "${work}/claims.json"
  jq -e \
    --arg organization_id "${BUILDKITE_ORGANIZATION_ID}" \
    --arg pipeline_id "${BUILDKITE_PIPELINE_ID}" \
    --arg build_id "${BUILDKITE_BUILD_ID}" \
    --arg step_key "${BUILDKITE_STEP_KEY}" \
    --arg queue "${BUILDKITE_AGENT_META_DATA_QUEUE}" \
    --arg event_name "${PHASE0_EVENT_NAME}" \
    --arg repository "${PHASE0_REPOSITORY}" \
    --arg ref "${PHASE0_REF}" \
    --arg commit "${PHASE0_COMMIT}" \
    --arg event_digest "${PHASE0_EVENT_DIGEST}" \
    --arg plan_digest "${PHASE0_PLAN_DIGEST}" \
    '.build.organization_id == $organization_id and .build.pipeline_id == $pipeline_id and .build.build_id == $build_id and .step_key == $step_key and .queue == $queue and .event.name == $event_name and .event.repository == $repository and .event.ref == $ref and .event.commit == $commit and .event.digest == $event_digest and .plan_digest == $plan_digest' \
    "${work}/claims.json" >/dev/null
  jq -e --slurpfile claims "${work}/claims.json" --argjson local "${PHASE0_LOCAL_CAPABILITIES}" \
    'all(.required_capabilities[]; . as $cap | ($claims[0].capability_ceiling | index($cap)) != null and ($local | index($cap)) != null) and all($claims[0].capability_ceiling[]; . as $cap | ($local | index($cap)) != null)' \
    "${work}/${prefix}/plan.json" >/dev/null
}

producer() {
  local work
  work="$(mktemp -d)"
  trap 'rm -rf "${work}"' EXIT
  load_and_verify_plan "${producer_key}" "${work}"
  mkdir -p "${work}/buildkite-gha/v1/results/${producer_key}"
  local result="success"
  [[ "${PHASE0_PRODUCER_FAIL:-}" == "1" ]] && result="failure"
  jq -cn --arg build_id "${BUILDKITE_BUILD_ID}" --arg job_id "${BUILDKITE_JOB_ID}" --arg step_key "${producer_key}" --arg plan_digest "${PHASE0_PLAN_DIGEST}" --arg result "${result}" \
    '{schema:"buildkite-gha/result-manifest/v1",plan_digest:$plan_digest,producer:{build_id:$build_id,job_id:$job_id,step_key:$step_key},result:$result,outputs:[{name:"message",value:"phase0-producer"}]}' \
    > "${work}/buildkite-gha/v1/results/${producer_key}/manifest.json"
  truncate -s -1 "${work}/buildkite-gha/v1/results/${producer_key}/manifest.json"
  (
    cd "${work}"
    buildkite-agent artifact upload "buildkite-gha/v1/results/${producer_key}/manifest.json"
  )
  buildkite-agent meta-data set "buildkite-gha/v1/results/${producer_key}" "${result}"
  [[ "${result}" == "success" ]]
}

consumer() {
  local work
  work="$(mktemp -d)"
  trap 'rm -rf "${work}"' EXIT
  load_and_verify_plan "${consumer_key}" "${work}"
  buildkite-agent artifact download "buildkite-gha/v1/results/${producer_key}/manifest.json" "${work}" --step "${producer_key}"
  jq -e --arg build_id "${BUILDKITE_BUILD_ID}" --arg step_key "${producer_key}" '.schema == "buildkite-gha/result-manifest/v1" and .producer.build_id == $build_id and .producer.step_key == $step_key' "${work}/buildkite-gha/v1/results/${producer_key}/manifest.json" >/dev/null
  buildkite-agent meta-data set "buildkite-gha/v1/results/${consumer_key}" "consumed"
}

native() {
  [[ "$(buildkite-agent meta-data get "buildkite-gha/v1/results/${consumer_key}")" == "consumed" ]]
  echo "native step observed dynamically uploaded consumer completion"
}

case "${1:-}" in
  bootstrap|producer|consumer|native) "$1" ;;
  *) echo "usage: $0 bootstrap|producer|consumer|native" >&2; exit 64 ;;
esac
