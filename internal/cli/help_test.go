package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunHelpAndVersion(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{name: "help flag", args: []string{"--help"}, wantOutput: "validate"},
		{name: "help command", args: []string{"help"}, wantOutput: "run-job"},
		{name: "command help", args: []string{"help", "compile"}, wantOutput: "buildkite-gha compile --event-path"},
		{name: "upload help", args: []string{"help", "upload"}, wantOutput: "--runner-queue"},
		{name: "upload help flag", args: []string{"upload", "--help"}, wantOutput: "--runner-queue"},
		{name: "command help flag", args: []string{"run-job", "--help"}, wantOutput: "--plan <path>"},
		{name: "version flag", args: []string{"--version"}, wantOutput: "buildkite-gha test-version\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr, "test-version"); code != 0 {
				t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.wantOutput) {
				t.Errorf("Run() stdout = %q, want it to contain %q", stdout.String(), test.wantOutput)
			}
			if stderr.Len() != 0 {
				t.Errorf("Run() stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestUploadHelpFormsMatch(t *testing.T) {
	outputs := make([]string, 0, 2)
	for _, args := range [][]string{{"help", "upload"}, {"upload", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test-version"); code != 0 {
			t.Fatalf("Run(%q) code = %d, stderr = %q", args, code, stderr.String())
		}
		outputs = append(outputs, stdout.String())
	}
	if outputs[0] != outputs[1] || !strings.Contains(outputs[0], "explicit .yml or .yaml path") || !strings.Contains(outputs[0], "failed or skipped workflows become top-level replacement steps") || !strings.Contains(outputs[0], "--runner-image") || !strings.Contains(outputs[0], "every scheduled workflow group is eligible") {
		t.Fatalf("upload help outputs differ or omit runner profile or aggregate workflow options:\nhelp command: %q\nhelp flag: %q", outputs[0], outputs[1])
	}
}
