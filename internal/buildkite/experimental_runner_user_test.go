package buildkite

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestEmitExperimentalRunnerUserIsExplicitAndLinuxOnly(t *testing.T) {
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	pipeline := Pipeline{
		CompilerStep:           "importer",
		DistributionDigest:     testDigest("distribution"),
		ExperimentalRunnerUser: true,
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
		`chown -R runner:"$runner_group" "$BUILDKITE_GHA_MISE_DATA_DIR"`,
		"chown -R runner:\"$runner_group\" '/opt/hostedtoolcache'",
		"sudo -n --preserve-env --user runner -- env HOME='/home/runner' TMPDIR='/tmp/buildkite-gha-runner'",
		`"$distribution" run-job --plan-digest`,
	} {
		if !strings.Contains(linux, required) {
			t.Errorf("experimental Linux command does not contain %q:\n%s", required, linux)
		}
	}
	for _, forbidden := range []string{"chmod -R 0777", "chmod -R a+w", "chown -R runner:\"$runner_group\" /"} {
		if strings.Contains(linux, forbidden) {
			t.Errorf("experimental Linux command contains broad permission change %q:\n%s", forbidden, linux)
		}
	}
	if darwin := commands["darwin"]; strings.Contains(darwin, "useradd") || strings.Contains(darwin, "sudo -n") {
		t.Fatalf("Darwin command selected Linux experiment:\n%s", darwin)
	}

	pipeline.ExperimentalRunnerUser = false
	output, err = Emit(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "useradd") || strings.Contains(string(output), "sudo -n") {
		t.Fatalf("default pipeline selected runner-user experiment:\n%s", output)
	}
}
