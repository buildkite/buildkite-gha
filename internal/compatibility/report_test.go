package compatibility

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestProcessingReportContainsEveryStableStageInTextAndJSON(t *testing.T) {
	report := NewProcessingReport("ci.yml", "hosted")
	report.LogicalJobs = 1
	report.Instances = 1
	report.Compile.Result = "incompatible"
	for _, stage := range report.Stages[:6] {
		report.SetStage(stage.ID, Passed)
	}
	report.SetStage("action-resolution", Failed)
	report.Result = "indeterminate"
	report.Jobs = append(report.Jobs, JobResult{ID: "test", Instance: "gha-test", Result: Passed})
	report.Actions = append(report.Actions, ActionResult{Reference: "./.github/actions/test", Job: "gha-test", Step: 1, Result: Failed})
	report.Diagnostics = append(report.Diagnostics, Diagnostic{
		Level: "error", Code: "E_ACTION_RESOLUTION", Category: "action-resolution",
		Stage: "action-resolution", Message: "action metadata is invalid; update the action", Detail: "unsupported runtime node12", Job: "test", Instance: "gha-test", Action: "./.github/actions/test", Step: 1,
		Location: &SourceLocation{Path: "ci.yml", Line: 8, Column: 9},
	})

	var encoded bytes.Buffer
	if err := WriteProcessing(&encoded, "json", report); err != nil {
		t.Fatal(err)
	}
	var decoded ProcessingReport
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ProcessingSchema || len(decoded.Stages) != 10 || decoded.Status != Failed || decoded.Diagnostics[0].Detail != "unsupported runtime node12" {
		t.Fatalf("decoded report = %#v", decoded)
	}
	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "processing-report-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("processing-report-v1.schema.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("processing-report-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(encoded.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(document); err != nil {
		t.Fatalf("processing report does not satisfy schema: %v", err)
	}
	for _, result := range []string{Passed, Failed, NotEvaluated} {
		if !strings.Contains(encoded.String(), `"result": "`+result+`"`) {
			t.Fatalf("JSON report does not contain stage result %q: %s", result, encoded.String())
		}
	}

	var rendered bytes.Buffer
	if err := WriteProcessing(&rendered, "text", report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Logical jobs: 1",
		"Instances: 1",
		"Compile: incompatible",
		"Admission: not-evaluated",
		"Workflow parsing: passed",
		"Immutable action resolution: failed",
		"Job-plan construction: not-evaluated",
		"action ./.github/actions/test (job gha-test, step 1): failed",
		"[E_ACTION_RESOLUTION] action metadata is invalid; update the action",
		"detail: unsupported runtime node12",
		"instance=gha-test, action=./.github/actions/test, step=1",
	} {
		if !strings.Contains(rendered.String(), want) {
			t.Fatalf("text report = %q, want %q", rendered.String(), want)
		}
	}
	if err := WriteProcessing(&bytes.Buffer{}, "yaml", report); err == nil {
		t.Fatal("WriteProcessing() accepted unknown format")
	}
}

func TestProcessingReportAggregatesIdenticalMatrixDiagnostics(t *testing.T) {
	report := NewProcessingReport("ci.yml", "hosted")
	for _, instance := range []string{"gha-test-a", "gha-test-b", "gha-test-c"} {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Level: "error", Code: "E_PROFILE", Stage: "hosted-profile-admission",
			Message: "same actionable reason", Job: "test", Instance: instance,
			Location: &SourceLocation{Path: "ci.yml", Line: 4, Column: 3},
		})
	}
	report.Finalize()
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Instance != "" || report.Diagnostics[0].Job != "test" {
		t.Fatalf("aggregated diagnostics = %#v", report.Diagnostics)
	}
}
