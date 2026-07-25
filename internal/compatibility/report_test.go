package compatibility

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWriteReports(t *testing.T) {
	for _, test := range []struct {
		name   string
		format string
		report Report
		want   string
	}{
		{name: "text success", format: "text", report: Compilable("ci.yml", 2, 3), want: "✓ 2 logical jobs and 3 static instances compile"},
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
