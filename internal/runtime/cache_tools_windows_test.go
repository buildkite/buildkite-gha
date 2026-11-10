package runtime

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	poison := map[string]string{"keepMe": "ordinary-value"}
	for _, name := range []string{"ProgramFiles", "SystemDrive", "SystemRoot", "WinDir", "ComSpec", "Path", "PathExt", "NoDefaultCurrentDirectoryInExePath"} {
		poison[name] = workspace
		t.Setenv(name, workspace)
	}
	// These installation hints and shim locations are not discovery authority.
	for _, name := range []string{"ChocolateyInstall", "ChocolateyToolsLocation", "AGENT_TOOLSDIRECTORY", "RUNNER_TOOL_CACHE"} {
		t.Setenv(name, workspace)
	}
	for _, name := range []string{"nOdE_oPtIoNs", "node_path", "node_extra_ca_certs", "node_tls_reject_unauthorized", "sslkeylogfile", "ld_audit", "ld_preload", "ld_library_path", "openssl_conf", "openssl_conf_include", "openssl_engines", "openssl_modules", "tar_options", "hTtPs_PrOxY", "http_proxy", "all_proxy", "no_proxy", "actions_runtime_token", "actions_results_url", "actions_cache_url", "actions_cache_service_v2", "actions_runtime_url", "buildkite_agent_access_token", "buildkite_job_id"} {
		poison[name] = "must-not-reach-child"
	}
	env, cleanup, err := isolateCacheActionEnvironment(poison)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	for name, value := range env {
		if strings.Contains(value, workspace) || value == "must-not-reach-child" {
			t.Fatalf("unsafe environment entry %s=%q", name, value)
		}
	}
	if env["PROGRAMFILES"] != programFiles || env["SYSTEMROOT"] != filepath.Dir(systemDirectory) || env["SYSTEMDRIVE"] != filepath.VolumeName(systemDirectory) || env["PATHEXT"] != ".EXE" || env["NODEFAULTCURRENTDIRECTORYINEXEPATH"] != "1" || env["keepMe"] != "ordinary-value" {
		t.Fatalf("incorrect isolated Windows environment: %#v", env)
	}
	// Only the verified executable is admitted, never its writable installation
	// directory. Keep the existing protected tool locations and their priority.
	paths := filepath.SplitList(env["PATH"])
	if len(paths) != 4 || !slices.Equal([]string{paths[0], paths[1], paths[3]}, []string{filepath.Join(programFiles, "Git", "usr", "bin"), filepath.Join(programFiles, "zstd"), systemDirectory}) {
		t.Fatalf("unsafe cache tool path or installed zstd not staged: %q", env["PATH"])
	}
	staged := paths[2]
	if !strings.HasPrefix(filepath.Base(staged), "buildkite-gha-cache-tools-") {
		t.Fatalf("unexpected zstd directory: %q", staged)
	}
	entries, err := os.ReadDir(staged)
	if err != nil || len(entries) != 1 || entries[0].Name() != "zstd.exe" {
		t.Fatalf("staged tools = %v, error = %v", entries, err)
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
	cleanup()
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged tools survived cleanup: %v", err)
	}
}

func TestWindowsCacheZstdStaging(t *testing.T) {
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(filepath.VolumeName(systemDirectory)+`\`, "tools", "zstd", "zstd.exe")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != windowsCacheZstdDigest {
		t.Fatalf("stock zstd digest = %s", got)
	}
	root := t.TempDir()
	for _, name := range []string{"zstd.exe", "tar.exe", "sh.exe", "gzip.exe", "msvcrt.dll"} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Hash admission applies to the opened bytes even through a junction, not
	// the lexical source directory, its ACL, or its mutable sibling executables.
	link := filepath.Join(t.TempDir(), "installation junction")
	if output, err := exec.Command(filepath.Join(systemDirectory, "cmd.exe"), "/c", "mklink", "/J", link, root).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v\n%s", err, output)
	}
	candidate := filepath.Join(link, "zstd.exe")
	staged, err := stageCacheZstd(candidate)
	if err != nil || staged == "" {
		t.Fatalf("stage installed zstd: %q, %v", staged, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staged) })
	entries, err := os.ReadDir(staged)
	if err != nil || len(entries) != 1 || entries[0].Name() != "zstd.exe" {
		t.Fatalf("staged sibling executables or DLLs: %v, %v", entries, err)
	}
	// A later write cannot change the admitted snapshot, and subsequent phases
	// must reject the changed source instead of reusing an earlier admission.
	if err := os.WriteFile(candidate, append(bytes.Clone(data), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(staged, "zstd.exe"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("staged bytes changed with source: %v", err)
	}
	for _, source := range []string{candidate, filepath.Join(root, "missing.exe")} {
		if directory, err := stageCacheZstd(source); err != nil || directory != "" {
			t.Fatalf("untrusted or missing source admitted: %q, %v", directory, err)
		}
	}
}
