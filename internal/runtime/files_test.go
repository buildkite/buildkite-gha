package runtime

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFileCommandParsing(t *testing.T) {
	tests := []struct {
		name      string
		contents  string
		want      map[string]string
		wantErr   string
		foldNames bool
	}{
		{name: "LF", contents: "single=value\nmulti<<END\nfirst\nsecond\nEND\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "CRLF", contents: "single=value\r\nmulti<<END\r\nfirst\r\nsecond\r\nEND\r\n", want: map[string]string{"single": "value", "multi": "first\nsecond"}},
		{name: "fold single after multiline", foldNames: true, contents: "PATH<<END\nfirst\nEND\nPath=second\n", want: map[string]string{"PATH": "second"}},
		{name: "fold multiline after single", foldNames: true, contents: "Path=first\npAtH<<END\nSecond\nTHIRD\nEND\n", want: map[string]string{"PATH": "Second\nTHIRD"}},
		{name: "preserve case without folding", contents: "Path=first\nPATH=second\n", want: map[string]string{"Path": "first", "PATH": "second"}},
		{name: "leading BOM only", contents: "\ufeffsingle=héllo\r\nmulti<<END\r\n\ufeffpayload\r\nEND\r\n\ufeffnext=\ufeffvalue\r\n", want: map[string]string{"single": "héllo", "multi": "\ufeffpayload", "\ufeffnext": "\ufeffvalue"}},
		{name: "BOM after blank line is payload", contents: "\n\ufeffname=value\n", want: map[string]string{"\ufeffname": "value"}},
		{name: "BOM only", contents: "\ufeff", want: map[string]string{}},
		{name: "equals before heredoc", contents: "single=value<<literal\n", want: map[string]string{"single": "value<<literal"}},
		{name: "heredoc before equals", contents: "multi<<END=value\npayload\nEND=value\n", want: map[string]string{"multi": "payload"}},
		{name: "missing name", contents: "=value\n", wantErr: "invalid file command"},
		{name: "missing delimiter", contents: "multi<<\n", wantErr: "invalid multiline file command"},
		{name: "unterminated LF", contents: "multi<<END\nunterminated\n", wantErr: `missing delimiter "END"`},
		{name: "unterminated CRLF", contents: "multi<<END\r\nunterminated\r\n", wantErr: `missing delimiter "END"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCommandReader("commands", strings.NewReader(test.contents), test.foldNames)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseCommandReader() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCommandReader() error = %v", err)
			}
			if !maps.Equal(got, test.want) {
				t.Fatalf("parseCommandReader() = %#v, want %#v", got, test.want)
			}
		})
	}

	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.cleanup() }()
	if err := os.WriteFile(files.env, []byte("GITHUB_TOKEN=action-token\nRUNNER_CUSTOM=action-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := newResult()
	if _, err := files.apply(&result, nil); err != nil {
		t.Fatalf("commandFiles.apply() GitHub-compatible environment error = %v", err)
	}
	if !maps.Equal(result.Env, map[string]string{"GITHUB_TOKEN": "action-token", "RUNNER_CUSTOM": "action-value"}) {
		t.Fatalf("commandFiles.apply() environment = %#v", result.Env)
	}
	if err := os.WriteFile(files.env, []byte("NODE_OPTIONS=--require bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "NODE_OPTIONS") {
		t.Fatalf("commandFiles.apply() error = %v, want NODE_OPTIONS rejection", err)
	}
}

func TestFileCommandEnvironmentCaseOrder(t *testing.T) {
	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.cleanup() }()
	for _, path := range []string{files.env, files.output, files.state} {
		if err := os.WriteFile(path, []byte("PATH=first\nPath<<END\nSecond\nEND\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := newResult()
	effects, err := files.apply(&result, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PATH": "first", "Path": "Second"}
	if !maps.Equal(result.Outputs, want) || !maps.Equal(result.State, want) {
		t.Fatalf("output/state names must preserve case: %#v / %#v", result.Outputs, result.State)
	}
	if runtime.GOOS == "windows" {
		want = map[string]string{"PATH": "Second"}
	}
	if !maps.Equal(result.Env, want) || !effects.pathSet || effects.pathBase != want["PATH"] {
		t.Fatalf("environment = %#v, PATH base = %q, set = %t", result.Env, effects.pathBase, effects.pathSet)
	}
}

func TestPathFileLeadingBOM(t *testing.T) {
	for _, test := range []struct {
		contents string
		want     []string
	}{
		{"\ufeff/opt/héllo\r\n/opt/\ufeffpayload\r\n\ufeff/second\r\n", []string{"/opt/héllo", "/opt/\ufeffpayload", "\ufeff/second"}},
		{"\n\ufeff/first\n", []string{"\ufeff/first"}},
		{"\ufeff", nil},
	} {
		got, err := parsePathContents([]byte(test.contents), nil)
		if err != nil || !slices.Equal(got, test.want) {
			t.Fatalf("parsePathContents(%q) = %q, %v, want %q", test.contents, got, err, test.want)
		}
	}
}

func TestFileCommandLineLimitIsExplicit(t *testing.T) {
	if values, err := parseCommandReader("output", strings.NewReader("value="+strings.Repeat("x", 70*1024)+"\n"), false); err != nil || len(values["value"]) != 70*1024 {
		t.Fatalf("parseCommandReader() value length = %d, error = %v", len(values["value"]), err)
	}
	if _, err := parseCommandReader("output", strings.NewReader("value="+strings.Repeat("x", maxStreamLineBytes)+"\n"), false); err == nil || !strings.Contains(err.Error(), "parse file command output") {
		t.Fatalf("parseCommandReader() error = %v, want attributed size failure", err)
	}
}

func TestDockerCommandFilesAreWritableWithoutExposingDirectoryEntries(t *testing.T) {
	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.cleanup() }()
	if err := files.allowContainerWrites(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(files.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dir.Mode().Perm(); got != 0o711 {
		t.Fatalf("container file-command directory mode = %o, want 711", got)
	}
	for _, path := range []string{files.output, files.env, files.state, files.summary, files.path} {
		file, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := file.Mode().Perm(); got != 0o666 {
			t.Fatalf("container file command %s mode = %o, want 666", filepath.Base(path), got)
		}
	}
}

func TestFileCommandAggregateLimits(t *testing.T) {
	files, err := newCommandFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.cleanup() }()

	many := strings.Repeat("value=x\n", maxCommandEntries+1)
	if err := os.WriteFile(files.output, []byte(many), 0o600); err != nil {
		t.Fatal(err)
	}
	result := newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("apply() error = %v, want entry limit", err)
	}

	if err := os.WriteFile(files.output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.summary, bytes.Repeat([]byte("x"), maxCommandFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	effects, err := files.apply(&result, nil)
	if err != nil || result.Summary != "" || effects.summaryBytes != maxCommandFileBytes+1 {
		t.Fatalf("oversized summary result = %#v, effects = %#v, error = %v", result, effects, err)
	}

	if err := os.WriteFile(files.summary, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.output, bytes.Repeat([]byte("x"), maxCommandFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("apply() error = %v, want output size limit", err)
	}

	if err := os.WriteFile(files.output, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{files.output, files.env, files.state} {
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 700*1024), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(files.summary, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result = newResult()
	if _, err := files.apply(&result, nil); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("apply() error = %v, want aggregate size limit", err)
	}
}
