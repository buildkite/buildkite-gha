package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsCacheArchiveTools(t *testing.T) {
	node := requireNode24(t)
	workspace := t.TempDir()
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		t.Fatal(err)
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	// Real executables with the wrong behavior catch both PATH and implicit
	// current-directory lookup, including tar's internal compressor launches.
	decoy, err := os.ReadFile(filepath.Join(systemDirectory, "where.exe"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tar.exe", "zstd.exe", "gzip.exe", "sh.exe"} {
		if err := os.WriteFile(filepath.Join(workspace, name), decoy, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	poison := map[string]string{"KEEP_ME": "ordinary-value"}
	for _, name := range []string{"ProgramFiles", "SystemDrive", "SystemRoot", "WinDir", "ComSpec", "Path", "PathExt", "NoDefaultCurrentDirectoryInExePath"} {
		poison[name] = workspace
		t.Setenv(name, workspace)
	}
	for _, name := range []string{"nOdE_oPtIoNs", "node_path", "node_extra_ca_certs", "node_tls_reject_unauthorized", "sslkeylogfile", "ld_audit", "ld_preload", "ld_library_path", "openssl_conf", "openssl_conf_include", "openssl_engines", "openssl_modules", "tar_options", "hTtPs_PrOxY", "http_proxy", "all_proxy", "no_proxy", "actions_runtime_token", "actions_results_url", "actions_cache_url", "actions_cache_service_v2", "actions_runtime_url", "buildkite_agent_access_token", "buildkite_job_id"} {
		poison[name] = "must-not-reach-child"
	}
	env, err := isolateCacheActionEnvironment(poison)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range env {
		if strings.Contains(value, workspace) || value == "must-not-reach-child" || name != strings.ToUpper(name) {
			t.Fatalf("unsafe environment entry %s=%q", name, value)
		}
	}
	if env["PROGRAMFILES"] != programFiles || env["SYSTEMROOT"] != filepath.Dir(systemDirectory) || env["SYSTEMDRIVE"] != filepath.VolumeName(systemDirectory) || env["PATHEXT"] != ".EXE" || env["NODEFAULTCURRENTDIRECTORYINEXEPATH"] != "1" || env["KEEP_ME"] != "ordinary-value" {
		t.Fatalf("incorrect isolated Windows environment: %#v", env)
	}
	// Exercise the same GNU tar compressor arguments used by the audited cache
	// bundles. Require both zstd and gzip in CI; no skipped archive assertions.
	script := `const fs = require('node:fs');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
function run(tool, args) {
  const result = spawnSync(tool, args, {encoding: 'utf8'});
  if (result.status !== 0) throw new Error(tool + ': ' + result.error + result.stdout + result.stderr);
  return result.stdout;
}
if (!run('tar', ['--version']).includes('GNU tar')) throw new Error('expected trusted Git GNU tar');
if (!run('zstd', ['--version']).includes('Zstandard')) throw new Error('expected trusted zstd');
const tar = path.join(process.env.PROGRAMFILES, 'Git', 'usr', 'bin', 'tar.exe');
const payload = 'cache payload: windows\r\nsecond line: 7919\n';
for (const method of ['zstd', 'gzip']) {
  fs.mkdirSync('cache files', {recursive: true});
  fs.writeFileSync('cache files/payload.txt', payload);
  fs.writeFileSync('manifest.txt', 'cache files/payload.txt\n');
  const archive = method + '.tar';
  const create = method === 'zstd' ? ['--use-compress-program', 'zstd -T0'] : ['-z'];
  const extract = method === 'zstd' ? ['--use-compress-program', 'zstd -d'] : ['-z'];
  run(tar, ['--posix', '-cf', archive, '--force-local', '-P', '-C', process.cwd().replaceAll('\\', '/'), '--files-from', 'manifest.txt', ...create]);
  fs.rmSync('cache files', {recursive: true});
  run(tar, ['-xf', archive, '--force-local', '-P', '-C', process.cwd().replaceAll('\\', '/'), ...extract]);
  if (fs.readFileSync('cache files/payload.txt', 'utf8') !== payload) throw new Error(method + ' contents differ');
  console.log(method + ' archive restored verified contents');
}`
	cmd := exec.CommandContext(t.Context(), node, "-e", script)
	cmd.Dir, cmd.Env = workspace, processEnv(env)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("archive tool isolation: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
