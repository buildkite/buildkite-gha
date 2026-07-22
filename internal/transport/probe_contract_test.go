package transport

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const installedProbePath = "/opt/buildkite-gha/phase-0-transport-probe/probe.sh"

func TestCheckoutSkippedProbeUsesOnlyInstalledCommands(t *testing.T) {
	root := filepath.Join("..", "..", ".buildkite", "phase-0-transport-probe")
	pipeline := readProbeFile(t, filepath.Join(root, "pipeline.yml"))
	if strings.Count(pipeline, "checkout:\n      skip: true") != 2 {
		t.Fatal("probe pipeline must skip checkout for both static jobs")
	}
	commands := regexp.MustCompile(`(?m)^    command: "([^"]+)"$`).FindAllStringSubmatch(pipeline, -1)
	if len(commands) != 2 {
		t.Fatalf("static probe commands = %d, want 2", len(commands))
	}
	for _, command := range commands {
		if !strings.HasPrefix(command[1], installedProbePath+" ") {
			t.Fatalf("checkout-skipped command is not installed probe: %q", command[1])
		}
	}

	script := readProbeFile(t, filepath.Join(root, "probe.sh"))
	if strings.Contains(script, ".buildkite/phase-0-transport-probe") {
		t.Fatal("probe script contains a repository-relative self invocation")
	}
	if !strings.Contains(script, `readonly probe_path="`+installedProbePath+`"`) || strings.Count(script, `command: "${probe_path} `) != 2 {
		t.Fatal("generated jobs must use the fixed installed probe path")
	}
}

func readProbeFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
