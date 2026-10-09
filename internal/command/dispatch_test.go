package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPluginIsHiddenAndZeroArgument(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test-version"); code != 0 {
			t.Fatalf("Run(%q) code = %d, stderr = %q", args, code, stderr.String())
		}
		if strings.Contains(stdout.String(), "plugin") {
			t.Fatalf("Run(%q) exposed plugin command: %q", args, stdout.String())
		}
	}
	for _, args := range [][]string{{"help", "plugin"}, {"plugin", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test-version"); code != 2 {
			t.Fatalf("Run(%q) code = %d, want 2", args, code)
		}
		if stdout.Len() != 0 || strings.Contains(stderr.String(), "buildkite-gha plugin") {
			t.Fatalf("Run(%q) exposed plugin help: stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestRevisionedDevelopmentVersionKeepsPlanCompatibilityVersion(t *testing.T) {
	if got := commandVersion("dev+0123456789ab"); got != "dev" {
		t.Fatalf("commandVersion() = %q, want dev", got)
	}
}

func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing command", want: "Usage:"},
		{name: "unknown command", args: []string{"nope"}, want: `unknown command "nope"`},
		{name: "unknown help command", args: []string{"help", "nope"}, want: `unknown command "nope"`},
		{name: "version arguments", args: []string{"--version", "extra"}, want: "does not accept arguments"},
		{name: "upload outside Buildkite", args: []string{"upload", "--runtime-queue", "hosted", "workflow.yml"}, want: "BUILDKITE=true"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.args) != 0 && test.args[0] == "upload" {
				requireImporterHost(t)
			}
			t.Setenv("BUILDKITE", "")
			t.Setenv("BUILDKITE_STEP_KEY", "")
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr, "dev"); code != 2 {
				t.Fatalf("Run() code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("Run() stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Errorf("Run() stderr = %q, want it to contain %q", stderr.String(), test.want)
			}
		})
	}
}
