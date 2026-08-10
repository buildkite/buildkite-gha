#!/usr/bin/env bash

set -euo pipefail

if (( $# > 1 )); then
  echo 'usage: .buildkite/upload-examples.sh [basic|artifacts|advanced|nodejs-concurrency]' >&2
  exit 2
fi

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

upload_pipeline() {
  buildkite-agent pipeline upload --no-interpolation --reject-secrets
}

if (( $# == 1 )); then
  example="$1"
elif [[ -v EXAMPLE ]]; then
  example="$EXAMPLE"
else
  cat <<'YAML' | upload_pipeline
agents:
  queue: "hosted"

steps:
  - block: ":github: Choose an example workflow"
    key: "choose-example"
    prompt: "Choose the GitHub Actions workflow to run as native Buildkite jobs."
    submit: "Run example"
    fields:
      - select: "Example"
        key: "example"
        required: true
        default: "basic"
        options:
          - label: "Basic CI"
            value: "basic"
          - label: "Artifact build and handoff"
            value: "artifacts"
          - label: "Advanced delivery"
            value: "advanced"
          - label: "Node.js concurrency"
            value: "nodejs-concurrency"

  - label: ":pipeline: Load example workflow"
    key: "example-loader"
    timeout_in_minutes: 10
    retry:
      automatic: false
    command: |
      set -euo pipefail
      example="$(buildkite-agent meta-data get example)"
      .buildkite/upload-examples.sh "$example"
YAML
  exit
fi

case "$example" in
  basic)
    workflow=".github/workflows/example-basic.yml"
    ;;
  artifacts)
    workflow=".github/workflows/example-artifacts.yml"
    ;;
  advanced)
    workflow=".github/workflows/example-advanced.yml"
    ;;
  nodejs-concurrency)
    workflow=".github/workflows/example-nodejs-concurrency.yml"
    ;;
  *)
    echo "unknown example: $example" >&2
    exit 2
    ;;
esac

if [[ -v EXAMPLE_COMMIT ]]; then
  commit="$EXAMPLE_COMMIT"
else
  commit="${BUILDKITE_COMMIT:-}"
fi
if [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
  echo 'example commit must be a full lowercase 40-character SHA' >&2
  exit 2
fi
if [[ "${BUILDKITE_COMMIT:-}" != "$commit" ]]; then
  echo 'EXAMPLE_COMMIT must match BUILDKITE_COMMIT' >&2
  exit 2
fi
if [[ ! -f "$workflow" ]]; then
  echo "example workflow does not exist: $workflow" >&2
  exit 2
fi

cat <<YAML | upload_pipeline
agents:
  queue: "hosted"

steps:
  - group: ":github: Run workflow"
    key: "example-$example-workflow"
    steps:
      - label: "Prepare workflow"
        key: "example-$example-importer"
        timeout_in_minutes: 20
        retry:
          automatic: false
        cache: "/cache/bkcache/github-actions-buildkite-plugin"
        plugins:
          - mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2:
              version: "2026.5.12"
          - github-actions#v0.4.4:
              workflow: "$workflow"
              buildkite-gha-source-ref: "$commit"
YAML
