package compatibility

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestWriteReports(t *testing.T) {
	warningReport := Compilable("ci.yml", 2, 3)
	warningReport.Diagnostics = append(warningReport.Diagnostics, Diagnostic{
		Level: "warning", Code: "W_EXAMPLE", Message: "compatibility is approximate",
	})
	for _, test := range []struct {
		name   string
		format string
		report Report
		want   string
	}{
		{name: "text success", format: "text", report: Compilable("ci.yml", 2, 3), want: "✓ 2 logical jobs and 3 static instances compile"},
		{name: "text success warning", format: "text", report: warningReport, want: "! [W_EXAMPLE] compatibility is approximate"},
		{name: "text failure", format: "text", report: Blocked("ci.yml", errors.New("unsupported operating system")), want: "[E_COMPILE] unsupported operating system"},
		{name: "json", format: "json", report: Compilable("ci.yml", 2, 3), want: `"schema": "buildkite-gha/compatibility-report/v1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := Write(&output, test.format, test.report); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
			if test.format == "json" {
				var decoded Report
				if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
					t.Fatalf("invalid JSON report: %v", err)
				}
			}
		})
	}
}

func TestWriteRejectsUnknownFormat(t *testing.T) {
	if err := Write(&bytes.Buffer{}, "yaml", Compilable("ci.yml", 1, 1)); err == nil {
		t.Fatal("Write() accepted unknown format")
	}
}

func TestWriteProfileReportsStagesWithoutOverclaimingRuntime(t *testing.T) {
	for _, test := range []struct {
		name   string
		report ProfileReport
		want   []string
	}{
		{
			name:   "admitted actions retain unknown runtime",
			report: Admitted("ci.yml", "hosted-tokenless", 2, 3, true),
			want:   []string{"Result: admitted", "Admission: admitted", "W_ACTION_RUNTIME_UNKNOWN"},
		},
		{
			name:   "profile rejection preserves compile success",
			report: ProfileBlocked("ci.yml", "hosted-tokenless", 2, 3, errors.New("secrets unavailable")),
			want:   []string{"Result: not-admitted", "Compile: compilable", "[E_PROFILE] secrets unavailable"},
		},
		{
			name:   "environment failure leaves admission unknown",
			report: ProfileNotEvaluated("ci.yml", "hosted-tokenless", 2, 3, "E_ENVIRONMENT", errors.New("Node 24 unavailable")),
			want:   []string{"Result: indeterminate", "Admission: not-evaluated", "[E_ENVIRONMENT] Node 24 unavailable"},
		},
		{
			name:   "compile rejection does not evaluate admission",
			report: ProfileCompileBlocked("ci.yml", "hosted-tokenless", errors.New("dynamic matrix")),
			want:   []string{"Result: incompatible", "Admission: not-evaluated", "[E_COMPILE] dynamic matrix"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var text bytes.Buffer
			if err := WriteProfile(&text, "text", test.report); err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(text.String(), want) {
					t.Fatalf("text = %q, want %q", text.String(), want)
				}
			}

			var encoded bytes.Buffer
			if err := WriteProfile(&encoded, "json", test.report); err != nil {
				t.Fatal(err)
			}
			var decoded ProfileReport
			if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Schema != ProfileSchema || decoded.Result != test.report.Result {
				t.Fatalf("decoded report = %#v", decoded)
			}
		})
	}
	if err := WriteProfile(&bytes.Buffer{}, "yaml", Admitted("ci.yml", "hosted-tokenless", 1, 1, false)); err == nil {
		t.Fatal("WriteProfile() accepted unknown format")
	}
}

func TestProcessingReportContainsEveryStableStageInTextAndJSON(t *testing.T) {
	report := NewProcessingReport("ci.yml", "hosted-tokenless")
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
		Stage: "action-resolution", Message: "action metadata is invalid", Job: "test", Instance: "gha-test", Action: "./.github/actions/test", Step: 1,
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
	if decoded.Schema != ProcessingSchema || len(decoded.Stages) != 10 || decoded.Status != Failed {
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
		"[E_ACTION_RESOLUTION] action metadata is invalid",
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
