package cli

import (
	"bytes"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
)

func TestDiagnosticExcerptsUseCapturedInputOnBothSurfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ci.yml")
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/missing\n        env: {TOKEN: literal-secret}\n")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := compiler.ParseWorkflow(path, source)
	if err != nil {
		t.Fatal(err)
	}
	report := compatibility.InitialProcessingReport(path, "", false, parsed, nil)
	report.Diagnostics = []compatibility.Diagnostic{{Level: "error", Message: "Local action could not be found.",
		Location: &compatibility.SourceLocation{Path: path, Line: 6, Column: 9}}}
	if err := os.WriteFile(path, []byte("changed after parsing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, artifacts := generatedFailure(report, sourceLinkContext{})
	const excerpt = "> 6 |       - uses: ./.github/actions/missing"
	if !strings.Contains(string(artifacts[0].Contents), excerpt) || !strings.Contains(string(artifacts[1].Contents), "<pre><code>"+html.EscapeString(excerpt)+"</code></pre>") {
		t.Fatalf("missing original excerpt: %s", artifacts)
	}
	for _, artifact := range artifacts {
		if bytes.Contains(artifact.Contents, []byte("literal-secret")) || bytes.Contains(artifact.Contents, []byte("changed after parsing")) {
			t.Fatalf("unexpected source disclosure: %s", artifact.Contents)
		}
	}
	var encoded bytes.Buffer
	if err := compatibility.WriteProcessing(&encoded, "json", report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), ".github/actions/missing") {
		t.Fatalf("excerpt leaked into report JSON: %s", &encoded)
	}
	context := sourceLinkContext{sources: report.Sources}
	without := renderProcessingDiagnostic(report.Diagnostics[0], sourceLinkContext{})
	if got := renderProcessingDiagnosticWithin(report.Diagnostics[0], len(without), context); got != without {
		t.Fatalf("excerpt displaced primary message: %q", got)
	}
}
