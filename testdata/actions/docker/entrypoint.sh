#!/bin/sh
set -eu

printf '%s\n' 'container=ran' >> "$GITHUB_OUTPUT"
printf '%s\n' 'DOCKER_RUNTIME_SEEN=true' >> "$GITHUB_ENV"
printf '%s\n' 'docker action summary' >> "$GITHUB_STEP_SUMMARY"
printf '%s\n' '::add-mask::docker-secret-value'
printf '%s\n' 'masked docker probe: docker-secret-value'
