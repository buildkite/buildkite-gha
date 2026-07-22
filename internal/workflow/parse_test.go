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

func TestParseMatrixPreservesDeclarationOrderAndCombinationSpans(t *testing.T) {
	source := []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        fruit: [apple, pear]
        animal: [cat, dog]
        include:
          - color: green
            animal: cat
        exclude:
          - fruit: pear
            animal: dog
    steps:
      - run: true
`)
	parsed, err := Parse("matrix.yml", source)
	if err != nil {
		t.Fatal(err)
	}
	matrix := parsed.Jobs[0].Matrix
	if matrix.Rows[0].Name != "fruit" || matrix.Rows[1].Name != "animal" {
		t.Fatalf("matrix rows = %#v, want declaration order", matrix.Rows)
	}
	for _, combination := range []MatrixCombination{matrix.Include[0], matrix.Exclude[0]} {
		if combination.Span.Start.Line == 0 || positionAfter(combination.Span.Start, combination.Span.End) {
			t.Fatalf("invalid matrix combination span: %#v", combination.Span)
		}
	}
	if matrix.Span.End.Line != 14 {
		t.Fatalf("matrix span end = %#v, want final exclude value on line 14", matrix.Span.End)
	}
}
