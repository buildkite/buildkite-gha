package buildkite

// This file contains the transitional PB-2731 runner-user implementation. The
// experimental-runner-user flag remains as a temporary opt-out.

const experimentalRunnerHome = "/home/runner"
const experimentalRunnerTemp = "/tmp/buildkite-gha-runner"

func experimentalRunnerUserBootstrap(requiresMise, hostedToolCache bool, cache *CacheVolume) []string {
	commands := []string{
		`test "$(id -u)" -eq 0 || { echo 'buildkite-gha: runner user bootstrap requires root' >&2; exit 1; }`,
		`for command in getent useradd usermod install sudo; do command -v "$command" >/dev/null 2>&1 || { echo "buildkite-gha: runner user bootstrap requires $command" >&2; exit 1; }; done`,
		`if getent passwd runner >/dev/null; then test "$(id -u runner)" -ne 0 && test "$(getent passwd runner | cut -d: -f6)" = '/home/runner' || { echo 'buildkite-gha: existing runner user is incompatible' >&2; exit 1; }; else useradd --create-home --home-dir '/home/runner' --shell /bin/bash runner; fi`,
		`runner_group="$(id -gn runner)"`,
		`install -d -o runner -g "$runner_group" -m 0700 '/home/runner' '/tmp/buildkite-gha-runner'`,
		`chown -R runner:"$runner_group" '/home/runner' '/tmp/buildkite-gha-runner'`,
		`install -d -m 0755 /etc/sudoers.d`,
		`printf '%s\n' 'runner ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/buildkite-gha-runner`,
		`chmod 0440 /etc/sudoers.d/buildkite-gha-runner`,
		`if command -v visudo >/dev/null 2>&1; then visudo -c -f /etc/sudoers.d/buildkite-gha-runner >/dev/null; fi`,
		`if [ -S /var/run/docker.sock ]; then docker_gid="$(stat -c '%g' /var/run/docker.sock)"; docker_group="$(getent group "$docker_gid" | cut -d: -f1 || true)"; if [ -z "$docker_group" ]; then command -v groupadd >/dev/null 2>&1 || { echo 'buildkite-gha: runner user bootstrap requires groupadd for Docker access' >&2; exit 1; }; docker_group="buildkite-gha-docker-$docker_gid"; groupadd --gid "$docker_gid" "$docker_group"; fi; usermod --append --groups "$docker_group" runner; fi`,
		`if [ -n "${BUILDKITE_AGENT_JOB_API_SOCKET:-}" ]; then job_api_socket="$BUILDKITE_AGENT_JOB_API_SOCKET"; test -S "$job_api_socket" || { echo 'buildkite-gha: Buildkite Job API socket is unavailable' >&2; exit 1; }; grant_runner_traverse() ( path="$1"; parent="$(dirname "$path")"; if [ "$parent" != "$path" ]; then grant_runner_traverse "$parent"; fi; if ! sudo -n --user runner -- test -x "$path"; then chgrp "$runner_group" "$path"; chmod g+x "$path"; fi ); grant_runner_traverse "$(dirname "$job_api_socket")"; chgrp "$runner_group" "$job_api_socket"; chmod g+rw "$job_api_socket"; unset -f grant_runner_traverse; sudo -n --user runner -- test -r "$job_api_socket" && sudo -n --user runner -- test -w "$job_api_socket" || { echo 'buildkite-gha: runner cannot access the Buildkite Job API socket' >&2; exit 1; }; fi`,
		`find "$bootstrap_dir" -type d -exec chmod a+rx {} +`,
		`chown root:"$runner_group" "$distribution" "$plan"`,
		`chmod 0550 "$distribution"`,
		`chmod 0440 "$plan"`,
		`sudo -n --user runner -- test -x "$distribution" && ! sudo -n --user runner -- test -w "$distribution" || { echo 'buildkite-gha: runner runtime permissions are unsafe' >&2; exit 1; }`,
		`sudo -n --user runner -- test -r "$plan" && ! sudo -n --user runner -- test -w "$plan" || { echo 'buildkite-gha: runner plan permissions are unsafe' >&2; exit 1; }`,
	}
	if cache != nil {
		cacheAnchor := platformCacheValidationPath("linux/amd64")
		if requiresMise {
			cacheAnchor = platformMiseCachePath("linux/amd64")
		}
		commands = append(commands, experimentalRunnerCacheOwnershipCommands("/cache/bkcache", cacheAnchor, cache.Paths)...)
	}
	if requiresMise {
		commands = append(commands,
			`test -n "${BUILDKITE_GHA_MISE_DATA_DIR:-}" || { echo 'buildkite-gha: runner user bootstrap requires BUILDKITE_GHA_MISE_DATA_DIR' >&2; exit 1; }`,
			`mise_runtime_dir="$(dirname "$BUILDKITE_GHA_MISE_DATA_DIR")/runtime/`+MinimumMiseVersion+`"`,
			`install -d -o runner -g "$runner_group" -m 0755 "$BUILDKITE_GHA_MISE_DATA_DIR" "$mise_runtime_dir"`,
			`chown -R runner:"$runner_group" "$BUILDKITE_GHA_MISE_DATA_DIR" "$mise_runtime_dir"`,
			`chmod -R u+rwX "$BUILDKITE_GHA_MISE_DATA_DIR" "$mise_runtime_dir"`,
		)
	}
	if hostedToolCache {
		commands = append(commands,
			`test -d '/opt/hostedtoolcache' || { echo 'buildkite-gha: runner user bootstrap requires /opt/hostedtoolcache' >&2; exit 1; }`,
			`chown -R runner:"$runner_group" '/opt/hostedtoolcache'`,
			`chmod -R u+rwX '/opt/hostedtoolcache'`,
		)
	}
	return append(commands,
		`sudo -n --user runner -- env HOME='/home/runner' TMPDIR='/tmp/buildkite-gha-runner' sh -c 'test "$(id -un)" = runner; test "$(id -u)" -ne 0; test "$HOME" = /home/runner; test -w "$TMPDIR"; sudo -n true'`,
		`if [ -S /var/run/docker.sock ]; then sudo -n --user runner -- test -w /var/run/docker.sock || { echo 'buildkite-gha: runner cannot access the Docker socket' >&2; exit 1; }; fi`,
	)
}

// experimentalRunnerCacheOwnershipCommands uses a compiler-owned cache path as
// the filesystem anchor, then accepts direct mounts or documented root links.
func experimentalRunnerCacheOwnershipCommands(cacheRoot, anchor string, paths []string) []string {
	commands := []string{
		`for command in mountpoint readlink stat; do command -v "$command" >/dev/null 2>&1 || { echo "buildkite-gha: configured cache paths require $command" >&2; exit 1; }; done`,
		"cache_root=" + shellQuote(cacheRoot),
		"cache_anchor=\"$(readlink -f -- " + shellQuote(anchor) + `)" || { echo 'buildkite-gha: Buildkite cache volume anchor is unavailable' >&2; exit 1; }`,
		`test -d "$cache_anchor" || { echo 'buildkite-gha: Buildkite cache volume anchor is unavailable' >&2; exit 1; }`,
		`cache_root_mounted=false; if test -d "$cache_root" && mountpoint -q -- "$cache_root"; then cache_root_mounted=true; fi`,
		`if ! mountpoint -q -- "$cache_anchor"; then test "$cache_root_mounted" = true || { echo 'buildkite-gha: Buildkite cache volume anchor is not mounted' >&2; exit 1; }; case "$cache_anchor" in "$cache_root"/*) ;; *) echo 'buildkite-gha: Buildkite cache volume anchor is unsafe' >&2; exit 1;; esac; fi`,
		`cache_device="$(stat -c '%d' -- "$cache_anchor")" || { echo 'buildkite-gha: Buildkite cache volume anchor is unavailable' >&2; exit 1; }`,
	}
	for _, path := range paths {
		commands = append(commands,
			"cache_target=\"$(readlink -f -- "+shellQuote(path)+`)" || { echo 'buildkite-gha: configured cache path is unavailable' >&2; exit 1; }`,
			`test -d "$cache_target" || { echo 'buildkite-gha: configured cache path is not a directory' >&2; exit 1; }`,
			`test "$cache_target" != "$cache_root" || { echo 'buildkite-gha: configured cache path is unsafe' >&2; exit 1; }`,
			`test "$(stat -c '%d' -- "$cache_target")" = "$cache_device" || { echo 'buildkite-gha: configured cache path does not target the Buildkite cache volume' >&2; exit 1; }`,
			`if ! mountpoint -q -- "$cache_target"; then test "$cache_root_mounted" = true || { echo 'buildkite-gha: configured cache path is not a Buildkite cache volume mount' >&2; exit 1; }; case "$cache_target" in "$cache_root"/*) ;; *) echo 'buildkite-gha: configured cache path is not a Buildkite cache volume mount' >&2; exit 1;; esac; fi`,
			`chown -R runner:"$runner_group" "$cache_target"`,
			`chmod -R u+rwX "$cache_target"`,
		)
	}
	return commands
}

func experimentalRunnerUserCommand(runJob string) string {
	return `sudo -n --preserve-env --user runner -- env HOME='` + experimentalRunnerHome + `' TMPDIR='` + experimentalRunnerTemp + `' ` + runJob
}
