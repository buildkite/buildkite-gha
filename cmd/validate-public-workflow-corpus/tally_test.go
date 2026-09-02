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
