package buildkite

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestEmitRunnerUserIsDefaultForLinuxOnly(t *testing.T) {
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	pipeline := Pipeline{
		CompilerStep:       "importer",
		DistributionDigest: testDigest("distribution"),
		Jobs: []Job{
			{Key: "linux", Label: "Linux", Queue: "hosted", Platform: "linux/amd64", RuntimeImage: image, PlanDigest: testDigest("linux plan"), RequiresMise: true},
			{Key: "darwin", Label: "Darwin", Queue: "macos", Platform: "darwin/arm64", PlanDigest: testDigest("darwin plan")},
		},
	}
	output, err := Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Steps []struct {
			Key     string `yaml:"key"`
			Command string `yaml:"command"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Steps) != 2 {
		t.Fatalf("steps = %#v", document.Steps)
	}
	commands := map[string]string{}
	for _, step := range document.Steps {
		commands[step.Key] = step.Command
	}
	linux := commands["linux"]
	for _, required := range []string{
		"useradd --create-home --home-dir '/home/runner'",
		"runner ALL=(ALL) NOPASSWD: ALL",
		"/var/run/docker.sock",
		"BUILDKITE_AGENT_JOB_API_SOCKET",
		`chmod g+rw "$job_api_socket"`,
		`artifact download '.buildkite-gha/plans/`,
		`sha256sum "$plan"`,
		`chown root:"$runner_group" "$distribution" "$plan"`,
		`chmod 0550 "$distribution"`,
		`chmod 0440 "$plan"`,
		`mise_runtime_dir="$(dirname "$BUILDKITE_GHA_MISE_DATA_DIR")/runtime/` + MinimumMiseVersion + `"`,
		`chown -R runner:"$runner_group" "$BUILDKITE_GHA_MISE_DATA_DIR" "$mise_runtime_dir"`,
		"chown -R runner:\"$runner_group\" '/opt/hostedtoolcache'",
		"sudo -n --preserve-env --user runner -- env HOME='/home/runner' TMPDIR='/tmp/buildkite-gha-runner'",
		`BUILDKITE_GHA_PLAN_DIGEST='` + testDigest("linux plan") + `' "$distribution" run-job --plan "$plan"`,
	} {
		if !strings.Contains(linux, required) {
			t.Errorf("Linux runner-user command does not contain %q:\n%s", required, linux)
		}
	}
	for _, forbidden := range []string{"chmod -R 0777", "chmod -R a+w", "chmod o+", "chown -R runner:\"$runner_group\" /", `chown runner:"$runner_group" "$distribution"`, `run-job --plan-digest`, "BUILDKITE_BUILD_CHECKOUT_PATH", "GITHUB_WORKSPACE"} {
		if strings.Contains(linux, forbidden) {
			t.Errorf("Linux runner-user command contains forbidden workspace or permission change %q:\n%s", forbidden, linux)
		}
	}
	if darwin := commands["darwin"]; strings.Contains(darwin, "useradd") || strings.Contains(darwin, "sudo -n") {
		t.Fatalf("Darwin command selected Linux runner user:\n%s", darwin)
	}

	pipeline.DisableRunnerUser = true
	output, err = Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "useradd") || strings.Contains(string(output), "sudo -n") {
		t.Fatalf("opt-out pipeline selected runner user:\n%s", output)
	}
}

func TestExperimentalRunnerCacheOwnershipAcceptsDirectHostedVolumePathsWithoutRootMount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bkcache")
	anchor := filepath.Join(root, "buildkite-gha", "mise", "linux-amd64")
	runnerHome := filepath.Join(t.TempDir(), "runner")
	gradleCaches := filepath.Join(runnerHome, ".gradle", "caches")
	gradleWrapper := filepath.Join(runnerHome, ".gradle", "wrapper")
	for _, path := range []string{anchor, gradleCaches, gradleWrapper} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	volumePaths := strings.Join([]string{anchor, gradleCaches, gradleWrapper}, "\n")
	output, err := runExperimentalRunnerCacheOwnership(t, root, anchor, []string{gradleCaches, gradleWrapper}, volumePaths, volumePaths)
	if err != nil {
		t.Fatalf("cache validation failed: %v: %s", err, output)
	}
}

func TestExperimentalRunnerCacheOwnershipAcceptsDocumentedRootMount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bkcache")
	anchor := filepath.Join(root, "buildkite-gha", "validation", "linux-amd64")
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runExperimentalRunnerCacheOwnership(t, root, anchor, []string{anchor}, root, root+"\n"+anchor)
	if err != nil {
		t.Fatalf("cache validation failed: %v: %s", err, output)
	}
}

func TestExperimentalRunnerCacheOwnershipRejectsOrdinaryAnchor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bkcache")
	anchor := filepath.Join(root, "buildkite-gha", "validation", "linux-amd64")
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runExperimentalRunnerCacheOwnership(t, root, anchor, []string{anchor}, "", anchor)
	if err == nil || !strings.Contains(output, "Buildkite cache volume anchor is not mounted") {
		t.Fatalf("ordinary anchor validation = %v, %q", err, output)
	}
}

func TestExperimentalRunnerCacheOwnershipRejectsUnsafePaths(t *testing.T) {
	tests := []struct {
		name        string
		path        func(root string) string
		createPath  bool
		mountpoints func(root, path string) string
		cachePaths  func(root, path string) string
		want        string
	}{
		{
			name:        "ordinary directory on cache filesystem",
			path:        func(string) string { return filepath.Join(t.TempDir(), "ordinary") },
			createPath:  true,
			mountpoints: func(root, _ string) string { return root },
			cachePaths:  func(root, path string) string { return root + "\n" + path },
			want:        "configured cache path is not a Buildkite cache volume mount",
		},
		{
			name:        "mount on another filesystem",
			path:        func(string) string { return filepath.Join(t.TempDir(), "other-volume") },
			createPath:  true,
			mountpoints: func(root, path string) string { return root + "\n" + path },
			cachePaths:  func(root, _ string) string { return root },
			want:        "configured cache path does not target the Buildkite cache volume",
		},
		{
			name:        "entire cache root",
			path:        func(root string) string { return root },
			createPath:  true,
			mountpoints: func(root, _ string) string { return root },
			cachePaths:  func(root, _ string) string { return root },
			want:        "configured cache path is unsafe",
		},
		{
			name:        "unavailable path",
			path:        func(string) string { return filepath.Join(t.TempDir(), "missing", "cache") },
			mountpoints: func(root, _ string) string { return root },
			cachePaths:  func(root, _ string) string { return root },
			want:        "configured cache path is unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "bkcache")
			anchor := filepath.Join(root, "buildkite-gha", "validation", "linux-amd64")
			path := test.path(root)
			dirs := []string{root, anchor}
			if test.createPath {
				dirs = append(dirs, path)
			}
			for _, dir := range dirs {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			mountpoints := anchor + "\n" + test.mountpoints(root, path)
			cachePaths := anchor + "\n" + test.cachePaths(root, path)
			output, err := runExperimentalRunnerCacheOwnership(t, root, anchor, []string{path}, mountpoints, cachePaths)
			if err == nil {
				t.Fatalf("cache validation unexpectedly succeeded: %s", output)
			}
			if !strings.Contains(output, test.want) {
				t.Fatalf("cache validation output = %q, want %q", output, test.want)
			}
		})
	}
}

func runExperimentalRunnerCacheOwnership(t *testing.T, root, anchor string, paths []string, mountpoints, cachePaths string) (string, error) {
	t.Helper()
	bin := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("mountpoint", `for argument do target="$argument"; done
printf '%s\n' "$TEST_MOUNTPOINTS" | grep -Fx -- "$target" >/dev/null`)
	writeExecutable("stat", `for argument do target="$argument"; done
if printf '%s\n' "$TEST_CACHE_PATHS" | grep -Fx -- "$target" >/dev/null; then printf '2049\n'; else printf '1\n'; fi`)
	writeExecutable("readlink", `for argument do target="$argument"; done
test -d "$target" && printf '%s\n' "$target"`)
	writeExecutable("chown", "exit 0\n")
	writeExecutable("chmod", "exit 0\n")

	command := exec.Command("bash", "-c", "set -euo pipefail\nrunner_group=runner\n"+strings.Join(experimentalRunnerCacheOwnershipCommands(root, anchor, paths), "\n"))
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_MOUNTPOINTS="+mountpoints, "TEST_CACHE_PATHS="+cachePaths)
	output, err := command.CombinedOutput()
	return string(output), err
}
