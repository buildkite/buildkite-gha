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
	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "processing-report-v2.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("processing-report-v2.schema.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("processing-report-v2.schema.json")
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

func TestProcessingReportV3PreservesPerEventOutcomes(t *testing.T) {
	validation := NewProcessingReport("ci.yml", "")
	validation.Result = "compilable"
	validation.SetStage("workflow-parsing", Passed)
	push := NewProcessingReport("ci.yml", "hosted")
	push.Result = "admitted"
	push.Admission.Result = "admitted"
	pullRequest := NewProcessingReport("ci.yml", "hosted")
	pullRequest.Result = "incompatible"
	pullRequest.SetStage("expression-validation", Failed)
	pullRequest.Diagnostics = append(pullRequest.Diagnostics, Diagnostic{
		Level: "error", Code: "E_EXPRESSION", Category: "compatibility",
		Stage: "expression-validation", Message: "payload field is unsupported",
	})
	report := NewProcessingReportV3("ci.yml", "hosted", validation)
	report.Evaluations = append(report.Evaluations,
		EventEvaluation{Event: "push", Source: "generated", Report: push},
		EventEvaluation{Event: "pull_request", Source: "generated", Report: pullRequest},
	)

	var encoded bytes.Buffer
	if err := WriteProcessingV3(&encoded, "json", report); err != nil {
		t.Fatal(err)
	}
	var decoded ProcessingReportV3
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ProcessingSchemaV3 || decoded.Result != "incompatible" || decoded.Status != Failed || len(decoded.Evaluations) != 2 || decoded.Evaluations[0].Report.Result != "admitted" || decoded.Evaluations[1].Report.Result != "incompatible" {
		t.Fatalf("decoded report = %#v", decoded)
	}
	v2Source, err := os.ReadFile(filepath.Join("..", "..", "schemas", "processing-report-v2.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	v3Source, err := os.ReadFile(filepath.Join("..", "..", "schemas", "processing-report-v3.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v2Document, v3Document any
	if err := json.Unmarshal(v2Source, &v2Document); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(v3Source, &v3Document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://buildkite.com/schemas/buildkite-gha/processing-report-v2.schema.json", v2Document); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("processing-report-v3.schema.json", v3Document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("processing-report-v3.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(encoded.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(document); err != nil {
		t.Fatalf("processing report does not satisfy v3 schema: %v", err)
	}

	var rendered bytes.Buffer
	if err := WriteProcessingV3(&rendered, "text", report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"processing-report/v3", "Event-independent validation:", "Generated event push:", "Generated event pull_request:", "Result: incompatible"} {
		if !strings.Contains(rendered.String(), want) {
			t.Fatalf("text report = %q, want %q", rendered.String(), want)
		}
	}
}

func TestProcessingReportV3ClassifiesContextRequiredWithoutHidingIncompatibility(t *testing.T) {
	validation := NewProcessingReport("ci.yml", "")
	validation.Result = "compilable"
	contextRequired := NewProcessingReport("ci.yml", "hosted")
	contextRequired.Result = "context-required"
	report := NewProcessingReportV3("ci.yml", "hosted", validation)
	report.Evaluations = append(report.Evaluations, EventEvaluation{
		Event: "pull_request", Source: "generated", Report: contextRequired,
	})
	report.Finalize()
	if report.Result != "context-required" {
		t.Fatalf("context-required report result = %q", report.Result)
	}

	incompatible := NewProcessingReport("ci.yml", "hosted")
	incompatible.Result = "incompatible"
	report.Evaluations = append(report.Evaluations, EventEvaluation{
		Event: "push", Source: "generated", Report: incompatible,
	})
	report.Finalize()
	if report.Result != "incompatible" {
		t.Fatalf("mixed report result = %q", report.Result)
	}

	notAdmitted := NewProcessingReport("ci.yml", "hosted")
	notAdmitted.Result = "not-admitted"
	report.Evaluations = []EventEvaluation{
		{Event: "pull_request", Source: "generated", Report: contextRequired},
		{Event: "push", Source: "generated", Report: notAdmitted},
	}
	report.Finalize()
	if report.Result != "not-admitted" {
		t.Fatalf("context-required and not-admitted report result = %q", report.Result)
	}
}

func TestProcessingReportV1RemainsStrictWithoutDetail(t *testing.T) {
	report := NewProcessingReport("ci.yml", "")
	report.Diagnostics = append(report.Diagnostics, Diagnostic{
		Level: "error", Code: "E_TEST", Message: "actionable guidance", Detail: "lower-level context",
	})
	var encoded bytes.Buffer
	if err := WriteProcessing(&encoded, "json", report); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	document["schema"] = "buildkite-gha/processing-report/v1"
	diagnostic := document["diagnostics"].([]any)[0].(map[string]any)
	delete(diagnostic, "detail")

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
	if err := schema.Validate(document); err != nil {
		t.Fatalf("original v1 report does not satisfy v1 schema: %v", err)
	}
	diagnostic["detail"] = "lower-level context"
	if err := schema.Validate(document); err == nil {
		t.Fatal("v1 schema accepted the v2 detail field")
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
