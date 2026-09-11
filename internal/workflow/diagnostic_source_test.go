package workflow

import (
	"fmt"
	"strings"
	"testing"
)

func TestCaptureDiagnosticSourceExcerpt(t *testing.T) {
	source := []byte("name: hidden\r\njobs:\r\n  test:\r\n    runs-on: 'ubuntu-24.04-arm64'\r\n    steps:\r\n      - uses: actions/checkout@v4\r\n      - shell: bash\r\n        run: echo secret\r\n")
	captured := CaptureDiagnosticSource(source)
	if captured == nil {
		t.Fatal("CaptureDiagnosticSource returned nil")
	}
	want := "  6 |       - uses: actions/checkout@v4\n> 7 |       - shell: bash\n    |                ^^^^"
	if got := captured.Excerpt(7); got != want {
		t.Fatalf("Excerpt(7) = %q, want %q", got, want)
	}
	if got := captured.Excerpt(5); got != "" {
		t.Fatalf("ineligible target excerpt = %q", got)
	}
	for i := range source {
		source[i] = 'x'
	}
	if got := captured.Excerpt(4); got != "> 4 |     runs-on: 'ubuntu-24.04-arm64'\n    |              ^^^^^^^^^^^^^^^^^^^^" {
		t.Fatalf("captured source changed with input: %q", got)
	}
	var nilSource *DiagnosticSource
	if got := nilSource.Excerpt(1); got != "" {
		t.Fatalf("nil excerpt = %q", got)
	}
}

func TestCaptureDiagnosticSourceFilterUnderline(t *testing.T) {
	source := []byte("on:\r\n  pull_request_review:\r\n    branches-ignore:\r\n      - private-branch\r\n")
	captured := CaptureDiagnosticSource(source)
	want := "> 3 |     branches-ignore:\n    |     ^^^^^^^^^^^^^^^"
	if got := captured.Excerpt(3); got != want {
		t.Fatalf("filter excerpt = %q, want %q", got, want)
	}
	if got := captured.Excerpt(4); got != "" {
		t.Fatalf("filter value was retained: %q", got)
	}
}

func TestCaptureDiagnosticSourceRejectsUnsafeLines(t *testing.T) {
	tests := map[string]string{
		"inline filter":         "on:\n  pull_request_review:\n    branches: [private-branch]\n",
		"filter comment":        "on:\n  pull_request_review:\n    branches: # private\n      - trunk\n",
		"quoted filter key":     "on:\n  pull_request_review:\n    'branches':\n      - trunk\n",
		"filter alias":          "on:\n  pull_request_review: &review\n    branches:\n      - trunk\n  pull_request: *review\n",
		"filter in script":      "jobs:\n  x:\n    steps:\n      - run: |\n          branches:\n",
		"unknown filter":        "on:\n  pull_request_review:\n    private-key:\n      - trunk\n",
		"comment":               "jobs:\n  x:\n    runs-on: ubuntu-latest # token\n",
		"credentials":           "jobs:\n  x:\n    uses: owner/repo@v1 password=secret\n",
		"HTML in uses":          "jobs:\n  x:\n    uses: owner/repo@v1<!-- secret -->\n",
		"explicit YAML tag":     "jobs:\n  x:\n    uses: !!str owner/repo@v1\n",
		"environment":           "jobs:\n  x:\n    env:\n      uses: owner/repo@v1\n",
		"uses under with":       "jobs:\n  x:\n    steps:\n      - with:\n          uses: owner/repo@v1\n",
		"flow step with secret": "jobs:\n  x:\n    steps:\n      - {uses: owner/repo@v1, env: {TOKEN: secret}}\n",
		"alias":                 "jobs:\n  base: &base\n    runs-on: ubuntu-latest\n  x: *base\n",
		"flow":                  "jobs: {x: {runs-on: ubuntu-latest}}\n",
		"control":               "jobs:\n  x:\n    runs-on: \"ubuntu-latest\\x01\"\n",
		"wrong location":        "runs-on: ubuntu-latest\njobs: {}\n",
		"bad shell":             "jobs:\n  x:\n    steps:\n      - shell: zsh\n",
		"bad runner":            "jobs:\n  x:\n    runs-on: self-hosted\n",
		"bad uses":              "jobs:\n  x:\n    uses: docker://alpine\n",
		"malformed":             "jobs: [\n",
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if got := CaptureDiagnosticSource([]byte(source)); got != nil {
				t.Fatalf("CaptureDiagnosticSource returned %#v", got)
			}
		})
	}
	blockRun := CaptureDiagnosticSource([]byte("jobs:\n  x:\n    runs-on: ubuntu-latest\n    steps:\n      - run: |\n          uses: owner/repo@v1\n"))
	if blockRun == nil || blockRun.Excerpt(6) != "" {
		t.Fatalf("block run content was eligible: %#v", blockRun)
	}
}

func TestCaptureDiagnosticSourceLimits(t *testing.T) {
	longLine := "jobs:\n  x:\n    runs-on: " + strings.Repeat(" ", 220) + "ubuntu-latest\n"
	if got := CaptureDiagnosticSource([]byte(longLine)); got != nil {
		t.Fatal("captured a line longer than 240 bytes")
	}
	oversize := append([]byte("jobs:\n  x:\n    runs-on: ubuntu-latest\n"), make([]byte, maxDiagnosticSourceSize)...)
	if got := CaptureDiagnosticSource(oversize); got != nil {
		t.Fatal("captured a source larger than 1 MiB")
	}
}

func TestCaptureDiagnosticSourceCopyLimit(t *testing.T) {
	const lineSize = 200
	line := "    runs-on:" + strings.Repeat(" ", lineSize-len("    runs-on:")-len("ubuntu-latest")) + "ubuntu-latest"
	if len(line) != lineSize {
		t.Fatalf("fixture line length = %d, want %d", len(line), lineSize)
	}

	var source strings.Builder
	source.WriteString("jobs:\n")
	for i := range 100 {
		fmt.Fprintf(&source, "  job%03d:\n%s\n", i, line)
	}
	captured := CaptureDiagnosticSource([]byte(source.String()))
	if captured == nil {
		t.Fatal("CaptureDiagnosticSource returned nil")
	}
	wantRetained := maxDiagnosticCopySize / lineSize
	if got := len(captured.lines); got != wantRetained {
		t.Fatalf("retained lines = %d, want %d", got, wantRetained)
	}
	lastRetainedLine := 3 + 2*(wantRetained-1)
	if got := captured.Excerpt(lastRetainedLine); got == "" {
		t.Fatalf("last line within copy budget (%d) was omitted", lastRetainedLine)
	}
	firstOmittedLine := lastRetainedLine + 2
	if got := captured.Excerpt(firstOmittedLine); got != "" {
		t.Fatalf("first line beyond copy budget (%d) was retained: %q", firstOmittedLine, got)
	}
}
