package cli

import (
	"bytes"
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func TestTriggerFailureLinksFilterAfterLicenseHeader(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", root)
	const path = "pr-reviewed.yml"
	// The rejected filter is at 21:5, not the review event or the selected PR trigger.
	source := []byte(strings.Repeat("# License header\n", 15) + "\nname: Reviewed\non:\n  pull_request_review:\n    types: [submitted, edited, dismissed]\n    branches:\n      - private-branch\n  pull_request:\n    types: [opened]\n    branches: [trunk]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hello\n")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := compatibility.NewProcessingReport(path, "")
	sha := commitDiagnosticSources(t, root, &fixture)
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		t.Fatal(err)
	}
	validated, validationErr := compiler.Validate(path, source)
	_, _, translationErr := buildkitepipeline.TranslateEventTriggerCondition(parsed.Triggers, "pull_request", buildkitepipeline.LiveTriggerConditionExpressions("true"), buildkitepipeline.TriggerEventSnapshot{})
	if validationErr == nil || translationErr == nil {
		t.Fatal("unsupported review filters were accepted")
	}
	for name, report := range map[string]compatibility.ProcessingReport{
		"validation":  compatibility.InitialProcessingReport(path, "", false, validated, validationErr),
		"translation": triggerFailureProcessingReport(workflowInput{Path: path, Source: source}, translationErr),
	} {
		t.Run(name, func(t *testing.T) {
			_, artifacts := generatedFailure(t.Context(), report, sourceLinkContext{serverURL: "https://github.com", repository: "owner/project", sha: sha})
			for _, artifact := range artifacts {
				text := string(artifact.Contents)
				if !strings.Contains(text, path+":21:5") || !strings.Contains(text, "/blob/"+sha+"/"+path+"#L21") || !strings.Contains(text, "pull_request_review does not support the branches filter") {
					t.Errorf("diagnostic lost filter position: %s", text)
				}
				if !strings.Contains(text, "21 |     branches:\n     |     ^^^^^^^^") || !strings.Contains(text, "move the check into a job or step condition") || strings.Contains(text, "private-branch") {
					t.Errorf("diagnostic lost safe filter explanation: %s", text)
				}
			}
		})
	}
}

func TestNestedRemoteParseFailureLinksOffendingWorkflow(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"callee.yml": "on: workflow_call\njobs:\n  nested:\n    uses: ./.github/workflows/broken.yml\n",
		"broken.yml": "on: workflow_call\njobs:\n  broken:\n    runs-on: ubuntu-latest\n    steps:\n      - name: missing execution\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := actionsource.DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	caller := filepath.Join(t.TempDir(), ".github", "workflows", "caller.yml")
	if err := os.MkdirAll(filepath.Dir(caller), 0o700); err != nil {
		t.Fatal(err)
	}
	options := compiler.DefaultOptions()
	options.RepositorySource = compiler.MemoizeRepositorySource(&batchCountingActionSource{root: root, digest: digest})
	validated, parseErr := compiler.ValidateWithOptionsContext(t.Context(), caller,
		[]byte("on: push\njobs:\n  call:\n    uses: owner/shared/.github/workflows/callee.yml@v2\n"),
		options)
	if parseErr == nil {
		t.Fatal("invalid nested workflow was accepted")
	}
	report := compatibility.InitialProcessingReport(caller, "", false, validated, parseErr)
	_, artifacts := generatedFailure(t.Context(), report, sourceLinkContext{})
	const display = "owner/shared/.github/workflows/broken.yml@v2:6:9"
	target := "https://github.com/owner/shared/blob/" + strings.Repeat("a", 40) + "/.github/workflows/broken.yml#L6"
	for _, artifact := range artifacts {
		text := string(artifact.Contents)
		if !strings.Contains(text, display) || !strings.Contains(text, target) || !strings.Contains(text, "run: echo hello") || strings.Contains(text, "reusable workflow could not be resolved") {
			t.Errorf("artifact lost nested diagnostic: %s", text)
		}
	}
	// Malformed YAML has no structured column, but must still identify the
	// nested file rather than falling back to the root caller.
	if err := os.WriteFile(filepath.Join(directory, "broken.yml"), []byte("on: workflow_call\njobs: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	validated, parseErr = compiler.ValidateWithOptionsContext(t.Context(), caller,
		[]byte("on: push\njobs:\n  call:\n    uses: owner/shared/.github/workflows/callee.yml@v2\n"), options)
	report = compatibility.InitialProcessingReport(caller, "", false, validated, parseErr)
	_, artifacts = generatedFailure(t.Context(), report, sourceLinkContext{})
	for _, artifact := range artifacts {
		if !bytes.Contains(artifact.Contents, []byte(strings.TrimSuffix(target, "6")+"1")) || !bytes.Contains(artifact.Contents, []byte("parse workflow YAML")) {
			t.Errorf("malformed YAML lost nested source: %s", artifact.Contents)
		}
	}
}

func TestNestedReusableFailureRetainsOffendingWorkflowDiagnostic(t *testing.T) {
	directory := filepath.Join(t.TempDir(), ".github", "workflows")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	workflows := map[string]string{
		"caller.yml": "on: push\njobs:\n  call:\n    uses: ./.github/workflows/middle.yml\n",
		"middle.yml": "on: workflow_call\njobs:\n  nested:\n    uses: ./.github/workflows/leaf.yml\n",
		"leaf.yml":   "on: workflow_call\njobs:\n  broken:\n    runs-on: ${{ inputs.runner }}\n    steps:\n      - run: true\n",
	}
	for name, source := range workflows {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	caller := filepath.Join(directory, "caller.yml")
	validated, validationErr := compiler.Validate(caller, []byte(workflows["caller.yml"]))
	if validationErr == nil {
		t.Fatal("invalid nested workflow was accepted")
	}
	report := compatibility.InitialProcessingReport(caller, "", false, validated, validationErr)
	if len(report.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one nested failure", report.Diagnostics)
	}
	diagnostic := report.Diagnostics[0]
	if diagnostic.Location == nil || diagnostic.Location.Path != "./.github/workflows/caller.yml" || diagnostic.Location.Line != 4 || diagnostic.Location.Column != 11 || diagnostic.Message != "reusable-workflow input expression is not statically resolvable" {
		t.Fatalf("nested diagnostic = %#v", diagnostic)
	}
}

func TestTriggerFailureRetainsSourceLink(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("BUILDKITE_BUILD_CHECKOUT_PATH", root)
	const path = "ci.yml"
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hello\n")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := compatibility.NewProcessingReport(path, "")
	sha := commitDiagnosticSources(t, root, &fixture)
	report := triggerProcessingReport(path, source)
	report.Diagnostics = []compatibility.Diagnostic{{Level: "error", Message: "Push path filters could not be evaluated.", Location: &compatibility.SourceLocation{Path: path, Line: 1, Column: 1}}}
	_, artifacts := generatedFailure(t.Context(), report, sourceLinkContext{serverURL: "https://github.com", repository: "owner/project", sha: sha})
	for _, artifact := range artifacts {
		if !bytes.Contains(artifact.Contents, []byte("https://github.com/owner/project/blob/"+sha+"/ci.yml#L1")) {
			t.Errorf("trigger failure lost source link: %s", artifact.Contents)
		}
	}
}

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
	_, artifacts := generatedFailure(t.Context(), report, sourceLinkContext{})
	const excerpt = "> 6 |       - uses: ./.github/actions/missing\n    |               ^^^^^^^^^^^^^^^^^^^^^^^^^"
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
	without := renderProcessingDiagnostic(t.Context(), report.Diagnostics[0], sourceLinkContext{})
	if got := renderProcessingDiagnosticWithin(t.Context(), report.Diagnostics[0], len(without), context); got != without {
		t.Fatalf("excerpt displaced primary message: %q", got)
	}
}

func TestPluginProcessingUsesSharedDiagnosticLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ci.yml")
	source := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/action@v1\n")
	parsed, err := compiler.ParseWorkflow(path, source)
	if err != nil {
		t.Fatal(err)
	}
	report := compatibility.InitialProcessingReport(path, "", false, parsed, nil)
	report.Diagnostics = []compatibility.Diagnostic{
		{Level: "warning", Code: "W_TEST", Message: "Runner may not work. Choose a supported runner.", Detail: "warning detail", Job: "test",
			Location: &compatibility.SourceLocation{Path: path, Line: 6, Column: 9}},
		{Level: "error", Code: "E_TEST", Message: "Runner cannot be used. Choose ubuntu-latest.", Detail: "error detail", Action: "owner/action@v1"},
	}

	before, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writePluginProcessing(t.Context(), &output, report, sourceLinkContext{}); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("renderer mutated report JSON:\nbefore %s\nafter  %s", before, after)
	}
	got := output.String()
	for _, want := range []string{"Workflow diagnostics", "Warning: Runner may not work.", "Choose a supported runner.", "warning detail", "Error: Runner cannot be used.", "Choose ubuntu-latest.", "error detail", "action=owner/action@v1", "> 6 |       - uses: owner/action@v1", "Compilation:", "Admission:"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("output count for %q = %d, want 1: %q", want, strings.Count(got, want), got)
		}
	}
	if !strings.HasPrefix(got, "^^^ +++\n") || strings.Contains(got, "W_TEST") || strings.Contains(got, "E_TEST") {
		t.Errorf("plugin output has wrong framing or exposes codes: %q", got)
	}
}

func TestPluginProcessingFiltersUnknownRuntimeAndUsesNeutralWarningHeading(t *testing.T) {
	report := compatibility.NewProcessingReport("ci.yml", "")
	report.Diagnostics = []compatibility.Diagnostic{
		{Level: "warning", Code: "W_ACTION_RUNTIME_UNKNOWN", Message: "hidden"},
		{Level: "warning", Code: "W_TEST", Message: "Visible warning.", Location: &compatibility.SourceLocation{Path: "ci.yml", Line: 4, Column: 3}},
	}
	var output bytes.Buffer
	if err := writePluginProcessing(t.Context(), &output, report, sourceLinkContext{}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "Source: ci.yml:4:3") || strings.Contains(got, "Error source:") {
		t.Fatalf("warning source has misleading severity: %q", got)
	}
	if !strings.Contains(got, "Workflow diagnostics") || strings.Contains(got, "failed") || strings.Contains(got, "hidden") || strings.Contains(got, "^^^ +++") || strings.Contains(got, "Compilation:") {
		t.Fatalf("warning output = %q", got)
	}
	report.Diagnostics = report.Diagnostics[:1]
	output.Reset()
	if err := writePluginProcessing(t.Context(), &output, report, sourceLinkContext{}); err != nil || output.Len() != 0 {
		t.Fatalf("filtered-only output = %q, error = %v", output.String(), err)
	}
}

func TestProcessingLogSanitizesTerminalControls(t *testing.T) {
	report := compatibility.NewProcessingReport("ci\x1b]8;;https://evil.example\a.yml", "")
	report.Diagnostics = []compatibility.Diagnostic{{
		Level: "error", Message: "bad\x1b[31m message\a\u202e. More context.", Detail: "detail\x1b]8;;https://evil.example\a",
		Job: "job\x1b[2J", Action: "action\a", Location: &compatibility.SourceLocation{Path: "path\x1b]8;;https://evil.example\a", Line: 1},
	}}
	messages, _ := processingLog(t.Context(), report, sourceLinkContext{}, "Workflow diagnostics")
	got := strings.Join(messages, "\n")
	for _, sequence := range []string{"\x1b]8", "\x1b[31m", "\x1b[2J", "\a", "\u202e"} {
		if strings.Contains(got, sequence) {
			t.Errorf("processing log retained unsafe sequence %q: %q", sequence, got)
		}
	}
	for _, want := range []string{"Error: bad[31m message.", "More context.", "detail]8;;https://evil.example", "job=job[2J", "action=action", "Error source: path]8;;https://evil.example:1"} {
		if !strings.Contains(got, want) {
			t.Errorf("processing log lost ordinary text %q: %q", want, got)
		}
	}
}
