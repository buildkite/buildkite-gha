package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
)

func TestParseSampleSize(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
		valid bool
	}{
		{value: "", valid: true},
		{value: "1", want: 1, valid: true},
		{value: "1000", want: 1000, valid: true},
		{value: "0"},
		{value: "01"},
		{value: "+1"},
		{value: "-1"},
		{value: "one"},
		{value: "1 "},
		{value: "999999999999999999999999999999999999"},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, err := parseSampleSize(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("parseSampleSize(%q) error = %v, valid = %t", test.value, err, test.valid)
			}
			if got != test.want {
				t.Fatalf("parseSampleSize(%q) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestSampleManifestIsStable(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "workflows.jsonl")
	records := []manifestRecord{
		{Hash: "hash-one", ID: "000000", Path: ".github/workflows/ci.yml", Repository: "acme/one", Source: "/workflows/one.yml"},
		{Hash: "hash-two", ID: "000001", Path: ".github/workflows/build.yaml", Repository: "acme/two", Source: "/workflows/two.yml"},
	}
	if err := writeJSONLines(source, records); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	key, manifest, err := sampleManifest(source, filepath.Join(root, "samples"), 1, "testing", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := "n1-scf80cd8aed48-m3c8f85957d35"; key != want {
		t.Fatalf("sample key = %q, want %q", key, want)
	}
	selected, err := loadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0] != records[0] {
		t.Fatalf("selected records = %#v, want %#v", selected, records[:1])
	}
	if want := "sample: 1 workflows, seed 'testing', key " + key + "\n"; stderr.String() != want {
		t.Fatalf("sample summary = %q, want %q", stderr.String(), want)
	}
}

func TestTallyReports(t *testing.T) {
	root := t.TempDir()
	workflow := filepath.Join(root, "workflow.yml")
	contextWorkflow := filepath.Join(root, "context-workflow.yml")
	manifest := filepath.Join(root, "workflows.jsonl")
	if err := writeJSONLines(manifest, []manifestRecord{
		{Hash: "hash", ID: "000000", Path: ".github/workflows/ci.yml", Repository: "acme/one", Source: workflow},
		{Hash: "context-hash", ID: "000001", Path: ".github/workflows/pr.yml", Repository: "acme/two", Source: contextWorkflow},
	}); err != nil {
		t.Fatal(err)
	}
	reports := filepath.Join(root, "reports")
	if err := os.Mkdir(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	validation := compatibility.ProcessingReport{Schema: compatibility.ProcessingSchema}
	evaluation := compatibility.ProcessingReport{
		Schema:      compatibility.ProcessingSchema,
		Diagnostics: []compatibility.Diagnostic{{Code: "E_EXAMPLE"}},
	}
	report := compatibility.ProcessingReportV3{
		Schema: compatibility.ProcessingSchemaV3, Workflow: workflow, Profile: "hosted", Result: "admitted",
		Validation:  validation,
		Evaluations: []compatibility.EventEvaluation{{Event: "push", Source: "generated", Report: evaluation}},
	}
	contents, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reports, "result.json"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	contextReport := compatibility.ProcessingReportV3{
		Schema: compatibility.ProcessingSchemaV3, Workflow: contextWorkflow, Profile: "hosted", Result: "context-required",
		Validation: validation, Evaluations: []compatibility.EventEvaluation{},
	}
	contents, err = json.Marshal(contextReport)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reports, "context-result.json"), contents, 0o644); err != nil {
		t.Fatal(err)
	}

	tallyPath := filepath.Join(root, "tally.json")
	var stdout bytes.Buffer
	validator := validatorInfo{commit: "commit", version: "version", digest: "digest"}
	if err := tallyReports(manifest, reports, tallyPath, validator, "full", "default", 0, &stdout); err != nil {
		t.Fatal(err)
	}
	var tally tallyOutput
	tallyContents, err := os.ReadFile(tallyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(tallyContents, &tally); err != nil {
		t.Fatal(err)
	}
	if tally.Repos != 2 || tally.MeasuredRepos != 1 || tally.CompatibleRepos != 1 || tally.ContextRequiredRepos != 1 || tally.UnparseableReports != 0 {
		t.Fatalf("repository tally = %#v", tally)
	}
	if tally.ByFinding["E_EXAMPLE"] != 1 || tally.ByRepo["E_EXAMPLE"] != 1 || tally.Evaluations["push"] != 1 {
		t.Fatalf("diagnostic tally = %#v", tally)
	}
	if tally.WorkflowResults["admitted"] != 1 || tally.WorkflowResults["context-required"] != 1 {
		t.Fatalf("workflow result tally = %#v", tally.WorkflowResults)
	}
	if !strings.Contains(stdout.String(), "measured: 1   compatible: 1 (100.00% of measured)") {
		t.Fatalf("summary did not contain repository tally:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "50.00%       1  E_EXAMPLE") {
		t.Fatalf("summary did not contain diagnostic tally:\n%s", stdout.String())
	}
}
