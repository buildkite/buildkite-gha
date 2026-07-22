package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSmokeWorkflowsIntoOwnedModel(t *testing.T) {
	for _, name := range []string{"shell.yml", "ci.yml", "artifact.yml"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", name)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(path, source)
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Jobs) != 2 {
				t.Fatalf("Parse() jobs = %d, want 2", len(parsed.Jobs))
			}
			for _, job := range parsed.Jobs {
				if job.Span.Start.Line == 0 || job.Span.End.Line < job.Span.Start.Line {
					t.Errorf("job %q has invalid owned source span: %#v", job.ID, job.Span)
				}
				for _, step := range job.Steps {
					if step.Span.Start.Line == 0 || step.Span.End.Line < step.Span.Start.Line {
						t.Errorf("job %q step %q has invalid owned source span: %#v", job.ID, step.Name, step.Span)
					}
				}
			}
		})
	}
}
