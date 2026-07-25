#!/bin/sh
set -eu

test "$GITHUB_WORKSPACE" = /github/workspace
test -f "$GITHUB_WORKSPACE/$INPUT_EXPECTED_FILE"

# The Redis protocol frame contains a literal "$4" bulk-string length.
# shellcheck disable=SC2016
reply="$(printf '*1\r\n$4\r\nPING\r\n' | nc -w 3 "$INPUT_SERVICE_HOST" "$INPUT_SERVICE_PORT")"
case "$reply" in
  +PONG*) ;;
  *) exit 1 ;;
esac

printf '%s\n' 'container=ran' >> "$GITHUB_OUTPUT"
printf '%s\n' 'PHASE5_DOCKER_ENV=seen' >> "$GITHUB_ENV"
printf '%s\n' 'phase5 Docker action summary' >> "$GITHUB_STEP_SUMMARY"
printf '%s\n' '::add-mask::phase5-docker-secret'
printf '%s\n' 'masked Docker probe: phase5-docker-secret'
