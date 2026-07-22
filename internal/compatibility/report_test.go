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
